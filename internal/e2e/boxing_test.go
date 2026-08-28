// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// boxingProgram converts a value of every shape the convention boxes into an
// interface, and reads each one back.
//
// An interface value is two words and the second is always a pointer, so a
// value that is not already one pointer is copied into the heap and the word
// is the address of the copy. The runtime has one helper per shape it can copy
// by value, and everything else goes through runtime.convT or convTnoptr,
// which take the source type's descriptor and a pointer to the value.
//
// nanogo refused that last group by name, which was the largest single refusal
// of Go's own test corpus and the blocker on three of nanogo's own packages.
// The shapes here are what it refused: a struct, an array, a scalar no helper
// takes by value, and the same three inside a named type and behind a
// non-empty interface.
const boxingProgram = `package main

import "fmt"

type pair struct{ a, b int }

type held struct {
	tag string
	p   *cell
}

type cell struct{ n int }

type quad [4]int

type stringer interface{ String() string }

func (p pair) String() string  { return fmt.Sprintf("pair(%d,%d)", p.a, p.b) }
func (q quad) String() string  { return fmt.Sprintf("quad(%d)", q[3]) }
func (h held) String() string  { return fmt.Sprintf("held(%s,%d)", h.tag, h.p.n) }
func (c cell) String() string  { return fmt.Sprintf("cell(%d)", c.n) }

//go:noinline
func boxAny(i int) []any {
	return []any{
		pair{i, i + 1},
		quad{i, i + 1, i + 2, i + 3},
		uint8(i),
		int8(-i),
		held{"t", &cell{i * 2}},
		cell{i},
		[2]string{"x", "y"},
		struct{ a int }{i},
	}
}

//go:noinline
func boxStringer(i int) []stringer {
	return []stringer{
		pair{i, i + 1},
		quad{i, i + 1, i + 2, i + 3},
		held{"u", &cell{i * 3}},
		cell{i},
	}
}

//go:noinline
func roundTrip(i int) (int, int) {
	var v any = pair{i, i * 2}
	p := v.(pair)
	var w any = quad{i, i, i, i * 4}
	q := w.(quad)
	return p.a + p.b, q[3]
}

func main() {
	for i := 0; i < 3; i++ {
		for _, v := range boxAny(i) {
			fmt.Printf("%T %v\n", v, v)
		}
		for _, s := range boxStringer(i) {
			fmt.Println(s.String())
		}
		a, b := roundTrip(i)
		fmt.Println("roundTrip", a, b)
	}
}
`

// TestToolexecBoxesAValueTheRegistersCannotCarry builds the program above and
// compares every line against gc.
func TestToolexecBoxesAValueTheRegistersCannotCarry(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/boxing\n\ngo 1.27\n",
		"main.go": boxingProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "boxing", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "boxing"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// boxedPointerProgram keeps a pointer alive through the copy an interface
// value points at.
//
// The copy an interface value points at is a heap object, and the collector
// finds the pointers in it through the descriptor the boxing helper was given.
// The helper itself is checked by the runtime: a pointer-holding type through
// runtime.convTnoptr throws "objects with pointers must be zeroed" out of
// mallocgc, so that direction cannot ship. The descriptor is not checked
// anywhere. A copy made with the wrong one is an object whose pointers the
// collector does not know about, the pointee is freed while the interface
// still reaches it, and nothing says so.
//
// So this is not a comparison of output. It is a finaliser on the pointee and
// a collection while the interface holds it. Under GOGC=1 the collector runs
// as often as it can, under clobberfree=1 a freed object holds a recognisable
// pattern rather than its old contents, and under gccheckmark=1 the collector
// marks a second time with the world stopped and compares. The three together
// are what specs/027-liveness-and-stackmaps.md uses for the same class of
// mistake.
const boxedPointerProgram = `package main

import (
	"fmt"
	"runtime"
)

type cell struct{ n int }

// held is wider than a register and its second word is a pointer, so it is
// copied by runtime.convT and the copy is scanned.
type held struct {
	tag string
	p   *cell
}

// heldArray is the same fact through an array rather than a struct.
type heldArray [2]*cell

var finalized int

//go:noinline
func makeHeld(tag string, n int) any {
	c := &cell{n}
	runtime.SetFinalizer(c, func(*cell) { finalized++ })
	return held{tag, c}
}

//go:noinline
func makeArray(n int) any {
	a := heldArray{{n}, {n + 1}}
	runtime.SetFinalizer(a[0], func(*cell) { finalized++ })
	runtime.SetFinalizer(a[1], func(*cell) { finalized++ })
	return a
}

//go:noinline
func churn() {
	for i := 0; i < 2000; i++ {
		_ = make([]byte, 128)
	}
}

func main() {
	v := makeHeld("a", 41)
	w := makeArray(7)

	for i := 0; i < 3; i++ {
		churn()
		runtime.GC()
		runtime.GC()
	}

	h := v.(held)
	a := w.(heldArray)
	fmt.Println("tag", h.tag, "n", h.p.n, "a0", a[0].n, "a1", a[1].n, "finalized", finalized)
	runtime.KeepAlive(v)
	runtime.KeepAlive(w)
}
`

// TestToolexecScansTheCopyAnInterfacePointsAt is the evidence for the choice
// between runtime.convT and runtime.convTnoptr.
//
// The expected line is written out rather than compared against gc, because
// what is under test is that the object survives: a comparison would pass if
// both compilers lost it.
func TestToolexecScansTheCopyAnInterfacePointsAt(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/boxedptr\n\ngo 1.27\n",
		"main.go": boxedPointerProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "boxedptr", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	const want = "tag a n 41 a0 7 a1 8 finalized 0\n"
	got := runUnderCollector(t, filepath.Join(h.mod, "boxedptr"))
	if string(got) != want {
		t.Errorf("the program printed\n%s\nand the object the interface holds must survive:\n%s", got, want)
	}

	// The same program built by gc, so that a wrong expectation fails here
	// rather than passing as a nanogo bug.
	gc := filepath.Join(h.dir, "boxedptr-gc")
	cmd := exec.Command(h.goCmd, "build", "-o", gc, ".")
	cmd.Dir = h.mod
	cmd.Env = env([]string{"GOCACHE=" + h.cache})
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build with the installed compiler: %v\n%s", err, b)
	}
	defer os.Remove(gc)
	if b := runUnderCollector(t, gc); string(b) != want {
		t.Errorf("gc's build of the same program printed\n%s\nwant\n%s", b, want)
	}
}

// runUnderCollector runs a program with the collector checking its own work
// and collecting as often as it can.
func runUnderCollector(t *testing.T, path string) []byte {
	t.Helper()
	cmd := exec.Command(path)
	cmd.Env = env([]string{"GODEBUG=gccheckmark=1,clobberfree=1", "GOGC=1"})
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s did not run under the collector: %v\n%s", filepath.Base(path), err, b)
	}
	return b
}

// longPointerMaskProgram holds more pointer words than a bitmask describes.
//
// Past internal/abi.MaxPtrmaskBytes*8 pointer words, which is 128, gc stops
// writing the mask into the descriptor and points GCData at one word in BSS
// with TFlagGCMaskOnDemand set. The runtime fills that word in the first time
// it needs the mask, so the descriptor alone does not say which words of the
// object hold pointers and nothing is scanned until it does.
//
// The program reaches the mask from both directions. A package-level variable
// of such a type is a root the linker describes from the variable's own type,
// and a heap allocation of one is where runtime.newobject reads the descriptor
// and builds the mask. Both hold the only reference to objects with finalisers
// on them, so a mask that describes nothing is a finaliser that runs.
const longPointerMaskProgram = `package main

import (
	"fmt"
	"runtime"
)

type cell struct{ n int }

// wide is 200 pointer words, which is past the bound. narrow is 128, which is
// the bound itself and still a bitmask, so the two are the pair.
type wide [200]*cell
type narrow [128]*cell

var root wide
var finalized int

//go:noinline
func fill(n int) *wide {
	w := new(wide)
	for i := range w {
		c := &cell{n + i}
		runtime.SetFinalizer(c, func(*cell) { finalized++ })
		w[i] = c
	}
	return w
}

//go:noinline
func fillNarrow() *narrow {
	w := new(narrow)
	for i := range w {
		c := &cell{i}
		runtime.SetFinalizer(c, func(*cell) { finalized++ })
		w[i] = c
	}
	return w
}

//go:noinline
func churn() {
	for i := 0; i < 3000; i++ {
		_ = make([]byte, 128)
	}
}

// hold collects while its caller's frame holds the only reference.
//
//go:noinline
func hold(w *wide) int {
	churn()
	runtime.GC()
	runtime.GC()
	return sum(w)
}

// onStack puts one in a frame rather than in the heap or in a global. The
// address is taken and escapes into a call, so the value is a stack object and
// the collector reads its pointer map through the record in the function's
// stack objects table. That record names the mask by an offset resolved
// against moduledata.rodata, which is the one place the word the runtime fills
// in cannot be used.
//
//go:noinline
func onStack(n int) int {
	var w wide
	for i := range w {
		c := &cell{n + i}
		runtime.SetFinalizer(c, func(*cell) { finalized++ })
		w[i] = c
	}
	return hold(&w)
}

func sum(w *wide) int {
	s := 0
	for _, c := range w {
		s = s + c.n
	}
	return s
}

func main() {
	for i := range root {
		c := &cell{i * 2}
		runtime.SetFinalizer(c, func(*cell) { finalized++ })
		root[i] = c
	}
	heap := fill(1000)
	small := fillNarrow()
	frame := onStack(500)

	for i := 0; i < 3; i++ {
		churn()
		runtime.GC()
		runtime.GC()
	}

	rs := 0
	for _, c := range root {
		rs = rs + c.n
	}
	ns := 0
	for _, c := range small {
		ns = ns + c.n
	}
	fmt.Println("root", rs, "heap", sum(heap), "narrow", ns, "frame", frame, "finalized", finalized)
	runtime.KeepAlive(heap)
	runtime.KeepAlive(small)
}
`

// TestToolexecScansATypeWhoseMaskTheRuntimeBuilds is the evidence for the
// on-demand pointer mask.
//
// The expected line is written out rather than compared against gc, because
// what is under test is that the objects survive: a comparison would pass if
// both compilers lost them.
func TestToolexecScansATypeWhoseMaskTheRuntimeBuilds(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/longmask\n\ngo 1.27\n",
		"main.go": longPointerMaskProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "longmask", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	// 200 cells at 0, 2, 4 ... is 200*199, the heap's 200 from 1000 up is
	// 200*1000 + 199*200/2, and the narrow one's 128 from 0 is 127*128/2.
	// The frame's 200 cells run from 500 up, which is 200*500 + 199*200/2.
	const want = "root 39800 heap 219900 narrow 8128 frame 119900 finalized 0\n"
	got := runUnderCollector(t, filepath.Join(h.mod, "longmask"))
	if string(got) != want {
		t.Errorf("the program printed\n%s\nand every object a live root reaches must survive:\n%s", got, want)
	}
}
