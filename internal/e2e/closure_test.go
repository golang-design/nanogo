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

// The capture half of specs/033-closures-defer-panic.md, run as a program.
//
// A capture is by reference: the variable moves into a heap cell and both the
// function that declares it and the literal reach it through a pointer. Three
// claims are separable and the program makes all three:
//
//   - a literal reads a variable the function around it declared;
//   - a literal assigns one, and the function sees the assignment;
//   - a literal outlives the frame that made it, which is the case a frame
//     slot would turn into memory corruption rather than a wrong value.
//
// The exit status is the assertion, as in helloProgram. A wrong answer divides
// by zero and the process dies.
const captureProgram = `package main

func adder(base int) func(int) int {
	return func(d int) int { return base + d }
}

func counter() (func(), func() int) {
	n := 0
	return func() { n = n + 1 }, func() int { return n }
}

func main() {
	// The literal outlives adder's frame.
	add := adder(40)
	d := add(2) - 42
	if d != 0 {
		d = d / (d - d)
	}

	// Two literals share one variable, and the function that made them is
	// gone by the time either runs.
	bump, read := counter()
	bump()
	bump()
	bump()
	d = read() - 3
	if d != 0 {
		d = d / (d - d)
	}

	// The function that declares the variable sees what the literal wrote.
	total := 0
	each := func(v int) { total = total + v }
	each(3)
	each(4)
	d = total - 7
	if d != 0 {
		d = d / (d - d)
	}
}
`

// The program that keeps a capture alive across a collection.
//
// The closure object holds the cell of every capture, so the collector reaches
// the cell through the object and the object through the frame slot that holds
// the func value. Every link in that chain is compiler-generated: the locals
// bitmap describes the slot, the closure object's type descriptor says which
// of its words hold pointers, and the cell's descriptor covers what the cell
// points at. One wrong bit anywhere along it frees a live object.
//
// The code pointer is the word that must NOT be traced. It holds a text
// address, and a collector asked to follow it reads outside the heap. That is
// why the object's first field is a uintptr and every capture word is an
// unsafe.Pointer, and gccheckmark below is what says the distinction held.
//
// churn allocates enough for the collection to have something to sweep, and
// the values it makes are unreachable at once, so an object that survives
// survives because something the compiler described kept it.
const captureGCProgram = `package main

import "runtime"

type node struct {
	a, b, c, d, e, f, g, h int
	next                   *node
}

func hold(n int) func() int {
	v := &node{a: n}
	v.next = &node{a: n + 1}
	return func() int { return v.a + v.next.a }
}

//go:noinline
func churn() {
	for i := 0; i < 4096; i++ {
		_ = &node{a: i}
	}
}

func main() {
	first := hold(1)
	second := hold(10)
	churn()
	runtime.GC()
	churn()
	runtime.GC()
	d := first() + second() - 24
	if d != 0 {
		d = d / (d - d)
	}
}
`

// TestToolexecCapturesAVariable runs the three shapes of a capture.
func TestToolexecCapturesAVariable(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/capture\n\ngo 1.27\n",
		"main.go": captureProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "capture", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	if b, err := exec.Command(filepath.Join(h.mod, "capture")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled did not run: %v\n%s", err, b)
	}
}

// TestToolexecKeepsCapturesThroughACollection is the evidence that the closure
// object's pointer map is right.
//
// It runs under GODEBUG=gccheckmark=1, which marks a second time with the world
// stopped and compares, so a pointer the map misses is a crash where the
// mistake is rather than a leak, and under clobberfree=1, so a freed object
// read through a stale pointer holds a recognisable pattern rather than its old
// contents. GOGC=1 collects as often as it can.
//
// gccheckmark is also what catches the opposite mistake. A closure object whose
// first word were described as a pointer would have the collector follow a text
// address, and the runtime reports that rather than ignoring it.
func TestToolexecKeepsCapturesThroughACollection(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/capturegc\n\ngo 1.27\n",
		"main.go": captureGCProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "capturegc", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	cmd := exec.Command(filepath.Join(h.mod, "capturegc"))
	cmd.Env = append(os.Environ(), "GODEBUG=gccheckmark=1,clobberfree=1", "GOGC=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the collector or the program rejected what the closure held: %v\n%s", err, b)
	}
}

// The program that grows a closure's stack.
//
// The stack-growth tail of specs/035-goroutines-and-stack-growth.md picks one
// of two runtime symbols, and the choice is a correctness one.
// runtime.morestack_noctxt writes zero into the context register before it
// grows the stack, so a closure that grew its stack resumes with a nil closure
// object and faults on its first capture. runtime.morestack saves the register
// into g.sched.ctxt, which gogo restores.
//
// A unit test can read the relocation the tail carries and cannot run it. This
// program runs it: the literal is recursive, so it re-enters its own prologue
// twenty thousand frames deep with a frame wide enough that the check trips,
// and every one of those frames reads a capture afterwards. Built with the
// wrong symbol the program dies with a nil dereference.
//
// The recursion goes through a variable the literal captures and the enclosing
// function assigns after making it, which is the by-reference capture: the
// literal sees the assignment.
const captureGrowProgram = `package main

import "os"

type frame struct {
	a, b, c, d, e, f, g, h int
	i, j, k, l, m, n, o, p int
}

type recfn func(int) int

func main() {
	base := 1
	var rec recfn
	rec = func(n int) int {
		var f frame
		f.a = base
		if n == 0 {
			return f.a
		}
		return rec(n-1) + f.a
	}
	if rec(20000) == 20001 {
		os.Exit(7)
	}
	os.Exit(1)
}
`

// TestToolexecGrowsAClosureStack runs the stack-growth tail of a closure.
func TestToolexecGrowsAClosureStack(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/capturegrow\n\ngo 1.27\n",
		"main.go": captureGrowProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "capturegrow", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	if got := exitCode(t, filepath.Join(h.mod, "capturegrow")); got != 7 {
		t.Fatalf("the program exited %d, want 7: the closure lost its captures across the stack growth", got)
	}
}

// The program that defers a call with an operand and recovers inside it.
//
// runtime.deferproc takes one word and calls it with nothing, so an operand
// travels inside a func value: ir.Build puts the call in a literal that
// captures the operands. That literal is a frame the program did not write,
// and runtime.gorecover counts frames. It recovers only when exactly one
// non-wrapper frame stands between it and runtime.gopanic, and it decides
// which frames are wrappers by the funcID in each function's FuncInfo.
//
// So the literal is marked FuncIDWrapper, and this program is what says the
// mark reached the object. Without it the recover returns nil, the panic is
// not caught, and the process dies: a silent change of meaning that no
// refusal and no unit test would report.
//
// The operand is what the deferred call writes, so the exit status also says
// the capture carried the value the statement saw.
const deferRecoverProgram = `package main

import "os"

var code = 1

func handle(n int) {
	recover()
	code = n
}

func boom(xs []int) {
	defer handle(7)
	_ = xs[3]
}

func main() {
	boom(nil)
	os.Exit(code)
}
`

// The panic is raised by the runtime, out of a bounds check, and not written
// as a panic statement. What this program proves is that a deferred call
// carrying an operand still lets the callee recover, and the wrapper the
// operand needs is what could break it. The value the panic carries is
// incidental to that. A panic statement would make the program depend on a
// conversion to an interface as well, which is refused, so the test would
// stop proving anything about defer the day that refusal moved.

// TestToolexecRecoversFromAWrappedDefer runs a defer whose call has an
// operand and whose callee recovers.
func TestToolexecRecoversFromAWrappedDefer(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/deferrecover\n\ngo 1.27\n",
		"main.go": deferRecoverProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "deferrecover", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	// The exit status is the operand the deferred call was given, and the
	// program reaches os.Exit only if the recover caught the panic.
	if got := exitCode(t, filepath.Join(h.mod, "deferrecover")); got != 7 {
		t.Fatalf("the program exited %d, want 7: the recover did not catch the panic", got)
	}
}

const capturedResultProgram = `package main

import "os"

var saved func() int

// boom is the panic path. The result is written before the panic, the
// deferred literal recovers and reads it, and the exit loads it after
// runtime.recovery has restored the stack pointer and no register.
//
//go:noinline
func boom(xs []int) (n int) {
	defer func() {
		if recover() != nil {
			n = n + 40
		}
	}()
	n = 2
	_ = xs[3]
	n = 99
	return n
}

// scaled is the ordinary path: "return 4" assigns the named result and the
// deferred literal sees what it assigned.
//
//go:noinline
func scaled(xs []int) (n int) {
	defer func() { n = n * 3 }()
	n = 1
	if len(xs) == 0 {
		return 4
	}
	return 5
}

// escape captures a result and defers nothing. The literal outlives the frame
// and reads what the return assigned.
//
//go:noinline
func escape() (n int) {
	saved = func() int { return n }
	n = 3
	return 9
}

func main() {
	if boom(nil) != 42 {
		os.Exit(1)
	}
	if scaled(nil) != 12 {
		os.Exit(2)
	}
	if escape() != 9 {
		os.Exit(3)
	}
	if saved() != 9 {
		os.Exit(4)
	}
	os.Exit(7)
}
`

// TestToolexecCapturesANamedResult is the evidence for the join between a
// captured result's cell and the storage the ABI returns.
//
// A named result a literal captures lives in a heap cell, because the literal
// and the function share one variable. The result object is still what the
// ABI returns, so every return writes the cell and the single exit copies the
// cell into the result object after the deferred functions have run. Three
// programs check three halves of that: the panic path, where runtime.recovery
// restores the stack pointer and no register, so the cell has to be in the
// frame; the ordinary path, where "return 4" has to be visible to the deferred
// literal; and a function that captures a result and defers nothing, where the
// literal outlives the frame.
//
// The exit status is the assertion, and gc's build of the same source exits 7.
func TestToolexecCapturesANamedResult(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/capturedresult\n\ngo 1.27\n",
		"main.go": capturedResultProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "capturedresult", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	if got := exitCode(t, filepath.Join(h.mod, "capturedresult")); got != 7 {
		t.Fatalf("the program exited %d, want 7: a captured result and the value the function returned disagreed", got)
	}
}

const escapingLocalProgram = `package main

import (
	"os"
	"runtime"
)

// Each of these hands out the address of one of its own variables. Two calls
// at the same stack level must return different pointers, which is Go's own
// test/escape.go stated in three lines.
//
//go:noinline
func fromLocal(x int) *int {
	n := x
	return &n
}

//go:noinline
func fromParam(x int) *int { return &x }

//go:noinline
func fromResult(x int) (n int) {
	n = x
	return
}

//go:noinline
func addrOfResult(x int) *int {
	n := fromResult(x)
	return &n
}

//go:noinline
func churn() {
	for i := 0; i < 4096; i++ {
		_ = &[16]int{i}
	}
}

func main() {
	p := fromLocal(1)
	q := fromLocal(2)
	if p == q {
		os.Exit(1)
	}
	a := fromParam(3)
	b := fromParam(4)
	if a == b {
		os.Exit(2)
	}
	c := addrOfResult(5)

	// The frames those pointers came from are gone and the heap is walked
	// twice with the world stopped. A pointer into a frame reads whatever is
	// there now.
	churn()
	runtime.GC()
	churn()
	runtime.GC()

	if *p != 1 || *q != 2 {
		os.Exit(3)
	}
	if *a != 3 || *b != 4 {
		os.Exit(4)
	}
	if *c != 5 {
		os.Exit(5)
	}
	os.Exit(7)
}
`

// TestToolexecKeepsAnEscapingLocalAlive is the interim rule
// specs/023-escape-analysis.md states for the one site with no safe default: a
// variable whose address the source takes lives in a heap cell.
//
// It used to stay in the frame. Two calls at one stack level then returned one
// pointer, and the value read correctly for as long as nothing overwrote that
// memory, which is what made it worse than a crash. The collection between the
// calls and the reads is what makes this program say so rather than agree by
// luck.
func TestToolexecKeepsAnEscapingLocalAlive(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/escapinglocal\n\ngo 1.27\n",
		"main.go": escapingLocalProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "escapinglocal", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	if got := exitCode(t, filepath.Join(h.mod, "escapinglocal")); got != 7 {
		t.Fatalf("the program exited %d, want 7: a pointer to a local outlived the frame it named", got)
	}
}
