// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

// The collector reads the tables this package writes, and it is the only
// witness that they are right. specs/027-liveness-and-stackmaps.md says why a
// unit test cannot be: a wrong bitmap produces correct output until a
// collection happens while the wrong bit is set.
//
// The shape is spikes/stackmap's, which is the demonstration the spec rests
// on: a pointer that lives in a stack slot across a call that collects, with a
// finaliser on the object. It survives when the slot is live and it is freed
// when the slot is dead, and the same function is used for both so that the
// difference is the bitmap and nothing else.
//
// The pointer is in a frame object rather than in an argument, and that is the
// point. An incoming argument is described by the arguments bitmap as well,
// which is exact and is the same at every safepoint, so an object reached
// through one survives whatever the locals bitmap says. Only a pointer that no
// argument names isolates the locals bitmap.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
)

// typePtrInt is *int, with the pointer bit ir.Layout computes. Nothing else
// may decide which words hold pointers (specs/020-ir.md).
var typePtrInt = func() *ir.Type {
	t := &ir.Type{Kind: ir.Ptr, Elem: typeInt, Name: "*int"}
	if err := ir.Layout(t); err != nil {
		panic(err)
	}
	return t
}()

// gcFunc builds the function under test.
//
// It calls three functions gc compiled: mk allocates the object and gives it a
// finaliser, gcNow collects and records how many finalisers had run, and use
// reads the object through the pointer the frame held. The pointer lives in a
// frame object between them, which is where the locals bitmap describes it.
//
// kill puts an OpVarKill before the collection. The object's slot then holds
// the same pointer and the forward analysis of specs/027 reports it dead, so
// the bitmap has no bit for it and the collector must free the object. That is
// the pair: one function, one slot, two bitmaps.
func gcFunc(t *testing.T, name string, kill bool) *compiled {
	t.Helper()
	local := &ir.Object{Name: "obj", Type: typePtrInt, Class: ir.ClassLocal, Addrtaken: true}
	return hand(t, name, func(f *ssa.Func) {
		f.Frame = []*ir.Object{local}
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		addr := e.NewValue(0, ssa.OpLocalAddr, typePtrInt, mem)
		addr.Aux = local

		mk := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
		mk.Aux = &ir.Object{Name: "main.mk"}
		p := e.NewValue(0, ssa.OpSelectN, typePtrInt, mk)
		st := e.NewValue(0, ssa.OpStore, ssa.MemType, addr, p, mk)
		st.AuxInt = typePtrInt.Size

		last := st
		if kill {
			k := e.NewValue(0, ssa.OpVarKill, ssa.MemType, st)
			k.Aux = local
			last = k
		}
		gc := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, last)
		gc.Aux = &ir.Object{Name: "main.gcNow"}

		ld := e.NewValue(0, ssa.OpLoad, typePtrInt, addr, gc)
		use := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, ld, gc)
		use.Aux = &ir.Object{Name: "main.use"}
		r := e.NewValue(0, ssa.OpSelectN, typeInt, use)
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, r, use)
	})
}

// gcHelpers is the half of the program gc compiles.
//
// The object is reachable from nowhere but the nanogo frame once mk returns:
// mk's own frame is gone, the value travelled in a register, and no global
// holds it. That is what makes the collection decide the test.
const gcHelpers = `package main

import (
	"runtime"
	"sync/atomic"
)

var finalized int32
var duringGC int32
var seen int

//go:noinline
func mk() *int {
	p := new(int)
	*p = 0x5eed
	runtime.SetFinalizer(p, func(*int) { atomic.AddInt32(&finalized, 1) })
	return p
}

//go:noinline
func gcNow() {
	// Several collections, because a finaliser is queued by one and run by
	// the goroutine that follows it. The wait is bounded: a case that must
	// report zero would otherwise wait for as long as it is given.
	for i := 0; i < 8 && atomic.LoadInt32(&finalized) == 0; i++ {
		runtime.GC()
		runtime.Gosched()
	}
	duringGC = atomic.LoadInt32(&finalized)
}

//go:noinline
func use(p *int) int {
	seen = *p
	return *p
}

func report(r int) {
	println("result", r, "finalized", duringGC, "seen", seen)
}
`

// gcCallerSrc is the caller of a function that takes nothing.
const gcCallerSrc = gcHelpers + `
func gcrun() int

func main() { report(gcrun()) }
`

// gcArgCallerSrc is the caller of a function that takes the pointer.
//
// The object is passed and never mentioned again, so gc keeps no reference to
// it: the argument is dead at the call in the caller's own liveness, and gc
// does not write the argument area for a value that travels in a register.
// What holds the object is the word the callee wrote there.
const gcArgCallerSrc = gcHelpers + `
func gcrun(p *int, n int) int

func main() { report(gcrun(mk(), 7)) }
`

// gcArgFunc builds a function that takes a pointer, never reads it, and
// collects.
//
// The arguments bitmap of specs/027-liveness-and-stackmaps.md is exact and is
// the same at every safepoint, so it says the incoming word holds a pointer
// whether or not the function reads it. The word is in the caller's frame and
// the argument arrived in a register, so the callee has to write it: that is
// what this case tests, and without the write the collector reads whatever the
// last frame at that address left behind.
func gcArgFunc(t *testing.T, name string) *compiled {
	t.Helper()
	return hand(t, name, func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		e.NewValue(0, ssa.OpArg, typePtrInt)
		n := e.NewValue(0, ssa.OpArg, typeInt)
		gc := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
		gc.Aux = &ir.Object{Name: "main.gcNow"}
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, n, gc)
	})
}

// TestStackMapKeepsALiveSlotAndFreesADeadOne is the evidence
// specs/027-liveness-and-stackmaps.md asks for.
//
// It runs under GODEBUG=gccheckmark=1, which marks a second time with the
// world stopped and compares, so a pointer the map misses is a crash at the
// point of the mistake rather than a leak, and under clobberfree=1, so a
// freed object read through a stale pointer holds a recognisable pattern
// rather than its old contents. GOGC=1 collects as often as it can.
func TestStackMapKeepsALiveSlotAndFreesADeadOne(t *testing.T) {
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)
	env := []string{"GODEBUG=gccheckmark=1,clobberfree=1", "GOGC=1"}

	tests := []struct {
		name   string
		build  func(*testing.T, string) *compiled
		caller string
		want   string
	}{
		// The slot is live at the collection, so the object survives it and
		// use reads the value mk wrote.
		{"live", func(t *testing.T, n string) *compiled { return gcFunc(t, n, false) },
			gcCallerSrc, "result 24301 finalized 0 seen 24301"},
		// The slot holds the same pointer and the lifetime marker says it is
		// dead, so the collector frees the object. The load and use then read
		// freed memory, which is why the assertion is the finaliser and not
		// what use returned.
		{"dead", func(t *testing.T, n string) *compiled { return gcFunc(t, n, true) },
			gcCallerSrc, "finalized 1"},
		// The pointer is an argument the function never reads. The arguments
		// bitmap says the incoming word holds a pointer at every safepoint,
		// so the object survives, and it can only do so if the entry wrote
		// the register into that word.
		{"argument", gcArgFunc, gcArgCallerSrc, "result 7 finalized 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.build(t, "gcrun")
			p := newMainPackage()
			r := emit(t, c, p)
			if tt.name != "argument" {
				assertPointerSlot(t, c, r, tt.name == "dead")
			}
			addFull(t, r, p)
			caller := compileCaller(t, goCmd, c.f.Sym, tt.caller)
			out, err := runLinkedEnv(t, goCmd, tc, cfg, p, caller, env)
			if err != nil {
				t.Fatalf("the program failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Fatalf("the program printed %q, want %q in it", strings.TrimSpace(out), tt.want)
			}
			t.Logf("%s", strings.TrimSpace(out))
		})
	}
}

// assertPointerSlot checks the bitmap describes the word the emitter stored
// the pointer in.
//
// The end-to-end result is the evidence that the collector agrees. This is the
// evidence that it agreed for the right reason: the bit the collector read
// covers the frame object, at the offset the layout gave it, and it is set
// exactly when the object is live.
func assertPointerSlot(t *testing.T, c *compiled, r *Result, kill bool) {
	t.Helper()
	items, err := ssa.FrameItems(c.a)
	if err != nil {
		t.Fatal(err)
	}
	lv, err := ssa.ComputeLiveness(c.a, items)
	if err != nil {
		t.Fatal(err)
	}
	if lv.NumSafepoints() < 3 {
		t.Fatalf("%d safepoints, and the function makes three calls", lv.NumSafepoints())
	}
	// The collection is the second call.
	live := lv.LiveAt(1, frameObjectItem(t, items))
	if live == kill {
		t.Errorf("the frame object is live=%v at the collection and the lifetime marker says %v", live, !kill)
	}
	// The bitmap symbol holds n, nbit, and one byte per bitmap: one bit, so
	// the byte is one or zero.
	data := r.Funcdata[ssa.FUNCDATA_LocalsPointerMaps].Data
	if len(data) < 8 {
		t.Fatalf("the locals bitmap is %d bytes", len(data))
	}
	if nbit := int32(data[4]); nbit != 1 {
		t.Fatalf("the locals bitmap has %d bits, and one word of the frame holds a pointer", nbit)
	}
	// Which bitmap the collection selects is the pc-value stream's answer and
	// not the reader's, so it is read from the maps rather than assumed.
	idx := r.maps.Index[1]
	want := byte(1)
	if kill {
		want = 0
	}
	if got := data[8+idx]; got != want {
		t.Errorf("the bitmap %d, which the collection selects, is %#x, want %#x", idx, got, want)
	}
}

// frameObjectItem returns the position of the frame object among the items.
func frameObjectItem(t *testing.T, items []ssa.FrameItem) int {
	t.Helper()
	for i := range items {
		if items[i].Kind == ssa.ItemObject {
			return i
		}
	}
	t.Fatal("the function has no frame object")
	return -1
}

// compileCaller compiles the half of the main package gc owns.
//
// The declaration of the nanogo function has no body, which gc reads as a
// symbol defined elsewhere. The symbol ABI file is how the toolchain is told
// which convention that definition uses; without it gc assumes ABI0 and the
// call reaches nothing.
func compileCaller(t *testing.T, goCmd, sym, body string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	abis := filepath.Join(dir, "symabis")
	if err := os.WriteFile(abis, []byte("def "+sym+" ABIInternal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "main.o")
	cmd := exec.Command(goCmd, "tool", "compile", "-p", "main", "-symabis", abis,
		"-importcfg", compileConfig(t), "-o", out, src)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compiling the caller failed: %v\n%s", err, b)
	}
	return out
}

// runLinkedEnv links a nanogo object with a gc caller and runs the program
// with the given environment added.
func runLinkedEnv(t *testing.T, goCmd string, tc *obj.Toolchain, cfg string, p *obj.Package, caller string, env []string) (string, error) {
	t.Helper()
	objPath := writeObject(t, p, tc)
	dir := t.TempDir()
	archive := filepath.Join(dir, "main.a")
	if b, err := exec.Command(goCmd, "tool", "pack", "c", archive, caller, objPath).CombinedOutput(); err != nil {
		t.Fatalf("go tool pack: %v\n%s", err, b)
	}
	out := filepath.Join(dir, "a.out")
	if b, err := exec.Command(goCmd, "tool", "link", "-importcfg", cfg, "-o", out, archive).CombinedOutput(); err != nil {
		t.Fatalf("the linker rejected the object: %v\n%s", err, b)
	}
	cmd := exec.Command(out)
	cmd.Env = append(os.Environ(), env...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}
