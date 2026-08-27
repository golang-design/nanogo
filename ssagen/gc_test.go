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
	// Unnamed, because *int is a type literal and not a defined type. A name
	// here would make rtype ask for the method set of a defined type, and the
	// stack objects table asks rtype for this type's pointer mask.
	t := &ir.Type{Kind: ir.Ptr, Elem: typeInt}
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
// late puts the collection before the read rather than after it. That is the
// whole difference between the two cases: the object's address is taken again
// below the collection when late is true, so the bitmap describes it there,
// and the last address is above the collection when late is false, so the
// bitmap does not and the collector must free the object. One function, one
// slot, two bitmaps.
//
// Each OpLocalAddr takes the memory of its own point in the program. That is
// what construction produces and it is what keeps the two apart: an address
// computed before a store must not float past it, and a call writes all of
// memory, so an address above a call and one below it are different values.
func gcFunc(t *testing.T, name string, late bool) *compiled {
	t.Helper()
	local := &ir.Object{Name: "obj", Type: typePtrInt, Class: ir.ClassLocal, Addrtaken: true}
	return hand(t, name, func(f *ssa.Func) {
		f.Frame = []*ir.Object{local}
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)

		addr := func(m *ssa.Value) *ssa.Value {
			a := e.NewValue(0, ssa.OpLocalAddr, typePtrInt, m)
			a.Aux = local
			return a
		}
		call := func(m *ssa.Value, sym string, args ...*ssa.Value) *ssa.Value {
			c := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, append(args, m)...)
			c.Aux = &ir.Object{Name: sym}
			return c
		}

		mk := call(mem, "main.mk")
		p := e.NewValue(0, ssa.OpSelectN, typePtrInt, mk)
		st := e.NewValue(0, ssa.OpStore, ssa.MemType, addr(mk), p, mk)
		st.AuxInt = typePtrInt.Size

		var r, last *ssa.Value
		if late {
			// The collection, then the read. The address below it is a use of
			// the object, so the object is live at the collection.
			gc := call(st, "main.gcNow")
			ld := e.NewValue(0, ssa.OpLoad, typePtrInt, addr(gc), gc)
			use := call(gc, "main.use", ld)
			r, last = e.NewValue(0, ssa.OpSelectN, typeInt, use), use
		} else {
			// The read, then the collection. Nothing names the object below
			// the collection, so the bitmap does not describe it there.
			ld := e.NewValue(0, ssa.OpLoad, typePtrInt, addr(st), st)
			use := call(st, "main.use", ld)
			r = e.NewValue(0, ssa.OpSelectN, typeInt, use)
			last = call(use, "main.gcNow")
		}
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, r, last)
	})
}

// gcReachFunc builds a function whose object is reached only through the
// stack objects table.
//
// The address is passed to a callee that collects while holding it. The
// caller's last use of the object is the address itself, so the locals bitmap
// does not describe the object at the call, and the only thing that can keep
// it alive is FUNCDATA_StackObjects: the collector finds the pointer in the
// callee's frame, sees that it points into this one, and looks the object up.
//
// This is stackobj.go's shape, which is the corpus file that found the gap.
func gcReachFunc(t *testing.T, name string) *compiled {
	t.Helper()
	local := &ir.Object{Name: "obj", Type: typePtrInt, Class: ir.ClassLocal, Addrtaken: true}
	return hand(t, name, func(f *ssa.Func) {
		f.Frame = []*ir.Object{local}
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)

		addr := func(m *ssa.Value) *ssa.Value {
			a := e.NewValue(0, ssa.OpLocalAddr, typePtrInt, m)
			a.Aux = local
			return a
		}
		mk := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
		mk.Aux = &ir.Object{Name: "main.mk"}
		p := e.NewValue(0, ssa.OpSelectN, typePtrInt, mk)
		st := e.NewValue(0, ssa.OpStore, ssa.MemType, addr(mk), p, mk)
		st.AuxInt = typePtrInt.Size

		hold := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, addr(st), st)
		hold.Aux = &ir.Object{Name: "main.hold"}
		r := e.NewValue(0, ssa.OpSelectN, typeInt, hold)
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, r, hold)
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
	"time"
	"unsafe"
)

// tinyBlock is the runtime's maxTinySize.
const tinyBlock = 16

// payload is the object the collector has to decide about, and it is bigger
// than a tiny block on purpose.
//
// The tiny allocator serves an allocation that is smaller than tinyBlock and
// holds no pointer, and it packs several of them into one block. A finaliser
// set on such an allocation belongs to the block and not to the allocation:
// runtime/mfinal.go accepts a finaliser on a pointer that is not the start of
// the block for exactly this case. The finaliser then runs only once every
// allocation in the block is unreachable, and which allocations share the
// block is decided by everything else the program has allocated. new(int) is
// such an allocation, and with it the two cases that assert the object is
// freed reported no finaliser at all on some hosts.
//
// A block of its own makes the object's liveness the object's own, which is
// what the stack map decides and what this test measures.
type payload [4]int

// A compile-time check that the object keeps a block to itself. The constant
// is negative and the program does not build if payload ever fits in a tiny
// block again.
var _ [unsafe.Sizeof(payload{}) - tinyBlock]struct{}

var finalized int32
var duringGC int32
var seen int

//go:noinline
func mk() *int {
	p := new(payload)
	p[0] = 0x5eed
	runtime.SetFinalizer(p, func(*payload) { atomic.AddInt32(&finalized, 1) })
	return &p[0]
}

//go:noinline
func gcNow() {
	// Several collections, because a finaliser is queued by one collection
	// and run by the finaliser goroutine afterwards.
	//
	// The wait sleeps. runtime.Gosched only yields: it returns as soon as this
	// goroutine is runnable again, which on an idle machine is usually after
	// the finaliser goroutine has run and on a loaded one is often before it.
	// Eight rounds of Gosched passed here and failed on CI; two thousand
	// rounds passed here and still failed on CI, which is the evidence that
	// yielding more is not the same as waiting.
	//
	// A sleep gives the finaliser goroutine a scheduling opportunity that does
	// not depend on this one becoming unrunnable. The loop is bounded, so a
	// case that must report zero finalisers spends at most its budget, and it
	// stops as soon as the count moves.
	for i := 0; i < 200 && atomic.LoadInt32(&finalized) == 0; i++ {
		runtime.GC()
		time.Sleep(2 * time.Millisecond)
	}
	duringGC = atomic.LoadInt32(&finalized)
}

//go:noinline
func use(p *int) int {
	seen = *p
	return *p
}

//go:noinline
func hold(p **int) int {
	// The only reference to the object during this collection is the pointer
	// into the caller's frame that this frame holds. Nothing in the caller's
	// bitmap describes the object, so it survives only if the caller's stack
	// objects table says where it is and what is in it.
	gcNow()
	seen = **p
	return **p
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
const gcArgCallerSrc = gcHelpers + `
func gcrun(p *int, n int) int

func main() { report(gcrun(mk(), 7)) }
`

// gcArgFunc builds a function that takes a pointer, never reads it, and
// collects.
//
// Nothing holds the object. The argument travels in a register, the caller let
// it die at the call, and this function never stores it, so the collector must
// free it. gc answers the same for the same program: `go build -gcflags=-live`
// reports the pointer live only at the SetFinalizer that made it.
//
// The reserved word of the argument area is where a register argument would
// be, and it is not an answer: only the stack-growth tail writes it, and only
// the bitmap in effect inside the tail describes it. Before that rule this
// case reported the object as surviving, which was the collector following a
// word no instruction of this function had written.
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
	hostRunsNanogoOutput(t)
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
		// The object's address is taken below the collection, so the bitmap
		// describes it there, the object survives and use reads the value mk
		// wrote.
		{"live", func(t *testing.T, n string) *compiled { return gcFunc(t, n, true) },
			gcCallerSrc, "result 24301 finalized 0 seen 24301"},
		// The slot holds the same pointer and nothing names the object below
		// the collection, so the collector frees it. use has already run, so
		// the assertion is the finaliser and not what use returned.
		{"dead", func(t *testing.T, n string) *compiled { return gcFunc(t, n, false) },
			gcCallerSrc, "finalized 1"},
		// The object is dead in the bitmap at the collection and a pointer to
		// it is live in the callee. Only the stack objects table can keep it,
		// so this case is the table and nothing else.
		{"reachable", gcReachFunc, gcCallerSrc, "result 24301 finalized 0 seen 24301"},
		// The pointer is an argument the function never reads, and it arrived
		// in a register. Nothing on the stack holds the object, so it is
		// freed, which is gc's answer for the same program.
		{"argument", gcArgFunc, gcArgCallerSrc, "result 7 finalized 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.build(t, "gcrun")
			p := newMainPackage()
			r := emit(t, c, p)
			if tt.name == "live" || tt.name == "dead" {
				assertPointerSlot(t, c, r, tt.name == "dead")
			}
			addFull(t, r, p)
			caller := compileCaller(t, goCmd, c.f.Sym, tt.caller)
			bin := linkProgram(t, goCmd, tc, cfg, p, caller)
			// One P and then several, from one link.
			//
			// A retention of an object this test says is dead can be invisible
			// at one number of Ps and plain at another, and the host's core
			// count picks the number when nothing else does. Both values are
			// asked for so that the result does not depend on the machine CI
			// runs on. What keeps the object's own liveness out of the answer
			// is the size of payload and not this loop.
			for _, procs := range []string{"1", "4"} {
				runEnv := append(append([]string{}, env...), "GOMAXPROCS="+procs)
				out, err := runProgram(bin, runEnv)
				if err != nil {
					t.Fatalf("at GOMAXPROCS=%s the program failed: %v\n%s", procs, err, out)
				}
				if !strings.Contains(out, tt.want) {
					t.Fatalf("at GOMAXPROCS=%s the program printed %q, want %q in it",
						procs, strings.TrimSpace(out), tt.want)
				}
				t.Logf("GOMAXPROCS=%s: %s", procs, strings.TrimSpace(out))
			}
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
	if lv.NumSafepoints() != 3 {
		t.Fatalf("%d safepoints, and the function makes three calls", lv.NumSafepoints())
	}
	// The collection is the second call when the read is below it and the
	// third when the read is above it.
	gcAt := 1
	if kill {
		gcAt = 2
	}
	live := lv.LiveAt(gcAt, frameObjectItem(t, items))
	if live == kill {
		t.Errorf("the frame object is live=%v at the collection, and the address below it says %v", live, !kill)
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
	idx := r.maps.Index[gcAt]
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

// linkProgram links a nanogo object with a gc caller and returns the binary.
//
// It is separate from running so that one program can be run more than once,
// which is how a test asks the same binary the same question under a different
// environment without paying for a second link.
func linkProgram(t *testing.T, goCmd string, tc *obj.Toolchain, cfg string, p *obj.Package, caller string) string {
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
	return out
}

// runProgram runs a linked program with the given environment added.
func runProgram(bin string, env []string) (string, error) {
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// runLinkedEnv links a nanogo object with a gc caller and runs the program
// with the given environment added.
func runLinkedEnv(t *testing.T, goCmd string, tc *obj.Toolchain, cfg string, p *obj.Package, caller string, env []string) (string, error) {
	t.Helper()
	return runProgram(linkProgram(t, goCmd, tc, cfg, p, caller), env)
}
