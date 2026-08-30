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

// nosplitFits is a //go:nosplit function whose chain fits below the guard.
//
// The array is large enough that the emitter cannot skip the check on the
// frame size alone, and the call to add makes the function non-leaf, so the
// only thing that removes the check is the directive. The exit status is the
// assertion, as in every other program here: a wrong answer divides by zero
// and the process dies.
const nosplitFits = `package main

//go:noinline
func add(a, b int) int { return a + b }

//go:nosplit
func hot(n int) int {
	var a [24]int
	a[n&23] = n
	return add(a[n&23], 0)
}

func main() {
	d := hot(3) - 3
	if d != 0 {
		d = d / (d - d)
	}
}
`

// nosplitOverflows is the same program with a frame the budget does not hold.
//
// 200 words is 1600 bytes and the limit is internal/abi.StackNosplitBase times
// the guard multiplier, less the size of a call. cmd/link is what adds the
// chain up (cmd/link/internal/ld/stackcheck.go), and it only looks at symbols
// that carry SymFlagNoSplit.
const nosplitOverflows = `package main

//go:noinline
func add(a, b int) int { return a + b }

//go:nosplit
func hot(n int) int {
	var a [200]int
	a[n&199] = n
	return add(a[n&199], 0)
}

func main() {
	d := hot(3) - 3
	if d != 0 {
		d = d / (d - d)
	}
}
`

// TestNoSplitLinksAndRunsWithNoGrowthCheck compiles a //go:nosplit function
// with nanogo, links it with the real linker and runs it.
//
// Three things are asserted and each one fails differently. The binary runs,
// so the function is not merely emitted. Its instructions hold no call to
// runtime.morestack, which is what the directive asks for and what a function
// running where the growth is not allowed requires. And the same program
// built entirely by gc gives the same exit status, which is the comparison
// that says the answer is Go's answer and not nanogo's.
func TestNoSplitLinksAndRunsWithNoGrowthCheck(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/nosplit\n\ngo 1.27\n",
		"main.go": nosplitFits,
	}, []string{"main"})

	out, err := h.build(t, "-o", "prog", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if !compiled(h.decisions(t), "main") {
		t.Fatalf("nanogo did not compile the main package, so this test measures gc:\n%s",
			strings.Join(h.decisions(t), "\n"))
	}
	prog := filepath.Join(h.mod, "prog")
	if b, err := exec.Command(prog).CombinedOutput(); err != nil {
		t.Fatalf("the program did not run: %v\n%s", err, b)
	}

	// The same source through gc alone, as the reference answer.
	gc := exec.Command(h.goCmd, "build", "-o", "gcprog", ".")
	gc.Dir = h.mod
	gc.Env = env([]string{"GOCACHE=" + h.cache})
	if b, err := gc.CombinedOutput(); err != nil {
		t.Fatalf("the all-gc build failed: %v\n%s", err, b)
	}
	if b, err := exec.Command(filepath.Join(h.mod, "gcprog")).CombinedOutput(); err != nil {
		t.Fatalf("the all-gc program did not run, so the nanogo one proves nothing: %v\n%s", err, b)
	}

	// And the linked function has no check in it. objdump reads the final
	// binary, so this is the instruction stream that runs and not the one the
	// compiler believes it wrote.
	dump := exec.Command(h.goCmd, "tool", "objdump", "-s", `main\.hot`, prog)
	dump.Env = env(nil)
	b, err := dump.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool objdump: %v\n%s", err, b)
	}
	text := string(b)
	if !strings.Contains(text, "TEXT main.hot(SB)") {
		t.Fatalf("objdump did not find main.hot, so nothing was read:\n%s", text)
	}
	if strings.Contains(text, "morestack") {
		t.Errorf("//go:nosplit and the linked function still calls morestack:\n%s", text)
	}
	// The guard load is the first instruction of the check, and it is the
	// half that has no relocation, so a symbol name is not enough to find it.
	if strings.Contains(text, "MOVD 16(g)") || strings.Contains(text, "MOVD 16(R28)") {
		t.Errorf("//go:nosplit and the linked function still reads the stack guard:\n%s", text)
	}
}

// nosplitPointerProgram keeps a pointer alive in a //go:nosplit frame across
// two collections.
//
// The directive removes every ordinary safe point, which is what gc's
// liveness.IsUnsafe does, and it must not remove the stack map at a call.
//
// hold allocates the node itself rather than taking it as an argument, so no
// other frame ever held the pointer and hold's own frame is the only thing
// that keeps the object while runtime.GC runs. A frame the collector cannot
// read there is a freed object that hold then dereferences. churn fills the
// heap so a collection has something to sweep, and the chain through next is a
// second pointer that only the first one reaches.
const nosplitPointerProgram = `package main

import "runtime"

type node struct {
	a, b, c, d int
	next       *node
}

//go:noinline
func crash() {
	d := 0
	d = d / d
}

//go:noinline
func churn() {
	for i := 0; i < 4096; i++ {
		_ = &node{a: i}
	}
}

//go:noinline
func fresh(i int) *node {
	return &node{a: i, next: &node{a: i - 1}}
}

//go:nosplit
func hold(i int) int {
	n := fresh(i)
	churn()
	runtime.GC()
	churn()
	runtime.GC()
	if n.a != 7 {
		crash()
	}
	if n.next == nil || n.next.a != 6 {
		crash()
	}
	return n.a
}

func main() {
	if hold(7) != 7 {
		crash()
	}
}
`

// TestNoSplitKeepsTheStackMapAtACall is the collector's half of the directive.
//
// //go:nosplit makes the whole body one unsafe point, and an implementation
// that read that as "no tables" would take the stack maps away with the safe
// points. gc does not: liveness.hasStackMap does not read allUnsafe, so a call
// still carries a map. The difference between the two readings is not a
// compile error, it is a collector fault, so it is measured rather than
// argued.
//
// The program runs under GODEBUG=gccheckmark=1, which marks a second time with
// the world stopped and reports an object that was reachable and not marked,
// and under clobberfree=1, so an object read through a stale pointer holds a
// recognisable pattern rather than its old contents. GOGC=1 collects as often
// as it can.
func TestNoSplitKeepsTheStackMapAtACall(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/nosplitgc\n\ngo 1.27\n",
		"main.go": nosplitPointerProgram,
	}, []string{"main"})

	out, err := h.build(t, "-o", "prog", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if !compiled(h.decisions(t), "main") {
		t.Fatalf("nanogo did not compile the main package, so this test measures gc:\n%s",
			strings.Join(h.decisions(t), "\n"))
	}
	cmd := exec.Command(filepath.Join(h.mod, "prog"))
	cmd.Env = env([]string{"GODEBUG=gccheckmark=1,clobberfree=1", "GOGC=1"})
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the pointer in the //go:nosplit frame did not survive the collection: %v\n%s", err, b)
	}
}

// TestNoSplitOverBudgetIsRefusedByTheLinker is the other half, and it is what
// no compile-only test can show.
//
// cmd/link adds up the frames of a chain of nosplit functions and fails the
// link when the chain does not fit below the guard. It only does that for a
// symbol that carries SymFlagNoSplit, so a diagnostic here is proof the flag
// was written and read. Without it the program links, runs, and grows its
// stack inside a function that told the runtime it would not.
func TestNoSplitOverBudgetIsRefusedByTheLinker(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/nosplit\n\ngo 1.27\n",
		"main.go": nosplitOverflows,
	}, []string{"main"})

	out, err := h.build(t, "-o", "prog", ".")
	if err == nil {
		t.Fatalf("the link succeeded, so a 1600-byte //go:nosplit frame was not counted:\n%s", out)
	}
	if !strings.Contains(out, "nosplit stack over") {
		t.Fatalf("the link failed for another reason than the budget:\n%s", out)
	}
	// The message names the function, which is what makes it actionable.
	if !strings.Contains(out, "main.hot") {
		t.Errorf("the diagnostic does not name the function:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.mod, "prog")); err == nil {
		t.Error("a refused link left an executable behind")
	}
}
