// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// discardedResultProgram reads one result of a two-result call and drops the
// other.
//
// The dropped result has no user by the time specs/025's lowering runs, and
// the pass used to remove it. An OpSelectN names one ABI location of the call,
// so removing it left the call with a result nothing named and ssagen stopped
// with "result 0 of the call is never named". Nothing about the shape of the
// callee matters, which is why the program is this small.
const discardedResultProgram = `package main

//go:noinline
func two() (int, int) { return 1, 7 }

//go:noinline
func first() (int, int) { return 7, 2 }

func main() {
	_, b := two()
	a, _ := first()
	if b != 7 || a != 7 {
		crash()
	}
}

//go:noinline
func crash() {
	d := 0
	d = d / d
}
`

// TestToolexecKeepsTheResultNobodyReads builds a call whose result is
// discarded and runs it.
func TestToolexecKeepsTheResultNobodyReads(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/discard\n\ngo 1.27\n",
		"main.go": discardedResultProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "discard", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	runProgram(t, filepath.Join(h.mod, "discard"))
}

// liveAcrossCallsProgram keeps two values live across two calls in a loop.
//
// The allocator gives each of them a frame slot, because specs/030-abi.md says
// a call clobbers every register, and the phi that closes the loop then reads
// one slot and writes another. ssagen.Emit had no instruction for that pair
// and refused the function with
//
//	no move from s3 to s2
//
// so a loop could carry about one value across a call and no more. Go's own
// corpus refused divmod.go and stackobj2.go for it.
//
// The four loops are the four ways the move is made. int takes the integer
// staging register and the 64-bit pair of instructions, float64 takes the
// floating-point one and the floating-point pair, uint8 takes the byte pair,
// and the range over a string is the loop whose byte index is live across the
// calls in its body. Every value is printed and compared against gc, because a
// move through the wrong register file or the wrong width still emits two
// instructions and still runs.
const liveAcrossCallsProgram = `package main

import "fmt"

//go:noinline
func step(x int) int { return x*3 + 1 }

//go:noinline
func fstep(x float64) float64 { return x*1.5 + 0.25 }

//go:noinline
func bstep(x uint8) uint8 { return x*7 + 3 }

func main() {
	s := []int{1, 2, 3, 4, 5, 6, 7}
	a, b := 0, 0
	for _, v := range s {
		a = a + step(v)
		b = b + step(a)
	}
	fmt.Println("int", a, b)

	fs := []float64{0.5, 1.25, 2, 3.75}
	x, y := 0.0, 0.0
	for _, v := range fs {
		x = x + fstep(v)
		y = y + fstep(x)
	}
	fmt.Println("float", x, y)

	var p, q uint8
	for i := 0; i < 6; i++ {
		p = p + bstep(uint8(i))
		q = q + bstep(p)
	}
	fmt.Println("byte", int(p), int(q))

	n, m, k := 0, 0, 0
	for _, r := range "aé漢\U0001f642z" {
		n = n + step(int(r))
		m = m + step(n)
		k = k + step(m)
	}
	fmt.Println("runes", n, m, k)
}
`

// TestToolexecKeepsTwoValuesLiveAcrossCalls builds the loop shape that needed
// a copy from one frame slot to another and compares every value against gc.
func TestToolexecKeepsTwoValuesLiveAcrossCalls(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/liveacross\n\ngo 1.27\n",
		"main.go": liveAcrossCallsProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "liveacross", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "liveacross"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// parallelAssignProgram writes a destination whose own index is a destination
// of the same statement.
//
// Go evaluates the operands of every index expression and every pointer
// indirection on the left before it carries out any assignment, so
// "i, a[i] = 0, 99" writes a[1] and not a[0]. nanogo read the index at the
// assignment that used it, which is after i was written, and every one of
// these lines printed a different answer from gc's. The program ran and said
// nothing, which is why it stayed wrong: the corpus refused test/range.go for
// the slot-to-slot move, and lifting that refusal is what reported it.
const parallelAssignProgram = `package main

import "fmt"

//go:noinline
func pair() (int, int) { return 0, 99 }

func main() {
	a := []int{10, 20}
	i := 1
	i, a[i] = 0, 99
	fmt.Println("slice", i, a[0], a[1])

	var arr [2]int
	arr[0], arr[1] = 10, 20
	j := 1
	j, arr[j] = 0, 99
	fmt.Println("array", j, arr[0], arr[1])

	c := []int{10, 20}
	k := 1
	k, c[k] = pair()
	fmt.Println("call", k, c[0], c[1])

	var m, n int = 5, 6
	p := &m
	p, *p = &n, 7
	fmt.Println("deref", m, n, *p)

	var st struct{ f [2]int }
	st.f[0], st.f[1] = 10, 20
	q := 1
	q, st.f[q] = 0, 99
	fmt.Println("field", q, st.f[0], st.f[1])

	x := []int{10, 20}
	y := []int{99}
	r := 1
	for r, x[r] = range y {
		break
	}
	fmt.Println("range", r, x[0], x[1])

	// A map index as the destination the loop writes first. It becomes a call
	// to runtime.mapassign rather than a location, so the operands held in
	// front of it are read where that call reads them and nowhere else.
	mf := map[int]int{}
	var w int
	for mf[three()], w = range []int{7, 8} {
	}
	fmt.Println("mapfirst", mf[3], w)

	ms := map[int]int{}
	u := 0
	for u, ms[u] = range []int{7, 8} {
	}
	fmt.Println("mapsecond", u, ms[0], ms[1])

	// The blank identifier as the destination the operands are held in front
	// of. Nothing is stored into it and the operands are still evaluated.
	bl := []int{10, 20}
	for _, bl[three()-3] = range []int{7, 8} {
	}
	fmt.Println("blank", bl[0], bl[1])
}

//go:noinline
func three() int { return 3 }
`

// TestToolexecAssignsInParallel compares every form of the statement against
// gc, because each one runs either way and only the values differ.
func TestToolexecAssignsInParallel(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/parallel\n\ngo 1.27\n",
		"main.go": parallelAssignProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "parallel", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "parallel"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
