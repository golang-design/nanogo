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

	// The other three range lowerings. Each has a loop body of its own and
	// each writes the index before the element, which is what the hoist
	// depends on. The map holds one entry, so the order it iterates in is
	// not part of the comparison.
	sr := []rune{10, 20, 30, 40}
	si := 1
	for si, sr[si] = range "abc" {
	}
	fmt.Println("strrange", si, int(sr[0]), int(sr[1]), int(sr[2]), int(sr[3]))

	mr := map[int]int{5: 1}
	mv := []int{10, 20}
	mi := 1
	for mi, mv[mi] = range mr {
	}
	fmt.Println("maprange", mi, mv[0], mv[1])

	ch := make(chan int, 2)
	ch <- 7
	close(ch)
	cv := []int{10, 20}
	ci := 1
	for cv[ci] = range ch {
	}
	fmt.Println("chanrange", cv[0], cv[1])
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

// arrayResultProgram returns an array the result registers cannot hold.
//
// Go's internal ABI passes an array in registers only when its length is zero
// or one, which gc states in types.CalcArraySize. nanogo gave every element a
// register of its own, so ([16]byte, error) took sixteen result registers for
// the array and pushed the error into the frame. gc does the opposite with
// both: the array is in the frame at 8(RSP) and the error is in R0 and R1.
//
// Inside a nanogo-only program the wrong rule was self-consistent and gave
// right answers, so nothing but a comparison against gc could report it. This
// program is that comparison. Every function crosses the boundary in a
// different direction:
//
//   - hash returns an array in the frame and an error in registers,
//   - stackAndFrame has stack arguments as well, so the array is placed after
//     them rather than at the start of the area,
//   - two returns two frame results, so the second is placed after the first,
//   - takes reads an array parameter that arrives in the frame,
//   - trivial returns the arrays the convention does keep in registers.
const arrayResultProgram = `package main

import "fmt"

type myErr struct{ s string }

func (e *myErr) Error() string { return e.s }

//go:noinline
func hash(i int) ([16]byte, error) {
	var b [16]byte
	for j := range b {
		b[j] = byte(i*j + j)
	}
	if i == 7 {
		return b, &myErr{"seven"}
	}
	return b, nil
}

//go:noinline
func stackAndFrame(a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15, a16, a17 int) ([16]byte, int) {
	var b [16]byte
	b[0] = byte(a16)
	b[15] = byte(a17)
	return b, a0 + a17
}

//go:noinline
func two(i int) ([16]byte, [24]byte) {
	var a [16]byte
	var b [24]byte
	a[0], a[15] = byte(i), byte(i+1)
	b[0], b[23] = byte(i+2), byte(i+3)
	return a, b
}

//go:noinline
func takes(b [16]byte, n int) int {
	s := n
	for _, v := range b {
		s = s + int(v)
	}
	return s
}

//go:noinline
func trivial(x [1]int, y [0]int, n int) ([1]int, int) {
	_ = y
	return [1]int{x[0] * 3}, n + 1
}

func sum(b []byte) int {
	s := 0
	for i, v := range b {
		s = s + int(v)*(i+1)
	}
	return s
}

func main() {
	for i := 0; i < 9; i++ {
		b, err := hash(i)
		msg := "nil"
		if err != nil {
			msg = err.Error()
		}
		fmt.Println("hash", i, sum(b[:]), msg)
		fmt.Println("takes", takes(b, i))
	}

	b, n := stackAndFrame(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18)
	fmt.Println("stackAndFrame", sum(b[:]), n)

	p, q := two(5)
	fmt.Println("two", sum(p[:]), sum(q[:]))

	r, m := trivial([1]int{7}, [0]int{}, 4)
	fmt.Println("trivial", r[0], m)
}
`

// TestToolexecPassesAnArrayThroughTheFrame builds the program above and
// compares every line against gc.
func TestToolexecPassesAnArrayThroughTheFrame(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/arrayresult\n\ngo 1.27\n",
		"main.go": arrayResultProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "arrayresult", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "arrayresult"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// discardedResultProgram calls functions whose results it throws away.
//
// A result the registers cannot hold is written into the outgoing argument
// area by the callee, and the area is the caller's frame. The callee writes it
// whether or not the call site reads it, so a discarded result takes the same
// space as an assigned one and `f1()` needs the same frame as `x := f1()`.
//
// nanogo sized the area from what the call site read, so a statement call to a
// function returning [3000]byte was given no area at all: gc gives such a
// caller a frame of 3024 bytes and nanogo gave it 16, and the callee wrote
// three thousand bytes over its caller's frame and everything above it.
//
// Go's own test/stack.go is where this surfaced. Its f0 calls f1, which
// returns [3000]byte and is called for its stack depth alone, and the
// corrupted frame turned the next panic into "traceback did not unwind
// completely".
//
// The program calls in every direction the frame is written from: a result
// discarded, a result assigned, a result discarded from a method and from a
// value of an interface, and a caller that has stack arguments of its own so
// that the area starts above them.
const framedResultAreaProgram = `package main

import "fmt"

type box struct{ n int }

//go:noinline
func (b *box) wide() [3000]byte {
	var a [3000]byte
	a[0], a[2999] = byte(b.n), byte(b.n+1)
	return a
}

type wider interface{ wide() [3000]byte }

//go:noinline
func makeWide(n int) [3000]byte {
	var a [3000]byte
	a[0], a[1499], a[2999] = byte(n), byte(n+1), byte(n+2)
	return a
}

//go:noinline
func pairing(n int) ([24]byte, int) {
	var a [24]byte
	a[0], a[23] = byte(n), byte(n+1)
	return a, n * 3
}

// discard calls for the call's effect and throws every result away. Its own
// frame has to hold each callee's result area even so.
//
//go:noinline
func discard(n int) int {
	makeWide(n)
	bb := box{n}
	bb.wide()
	var w wider = &box{n + 1}
	w.wide()
	pairing(n)
	return n + 1
}

// viaIface makes one call and makes it through an interface, so no other call
// in the function can hold the area open for it. discard cannot check this:
// its area is the largest any of its calls needs, and the static call to
// makeWide alone is three thousand bytes, so an interface call given no area
// at all would leave the frame the right size and the program would pass.
//
// The signature reaches an interface call through the method the selection
// names, which is the third of the three forms and the one that carries the
// signature nowhere else: the call's first operand is the method table and its
// second is the receiver, and neither is a function value.
//
//go:noinline
func viaIface(w wider) { w.wide() }

// deep is discard with a caller that has stack arguments, so the results are
// placed above them rather than at the start of the area.
//
//go:noinline
func deep(a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15, a16, a17 int) int {
	makeWide(a16)
	pairing(a17)
	return a0 + a17
}

func sum(b []byte) int {
	s := 0
	for i, v := range b {
		s = s + int(v)*(i+1)
	}
	return s
}

func main() {
	for i := 0; i < 4; i++ {
		fmt.Println("discard", discard(i))
		a := makeWide(i)
		fmt.Println("wide", sum(a[:]))
		p, q := pairing(i)
		fmt.Println("pairing", sum(p[:]), q)
	}
	fmt.Println("deep", deep(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18))
	for i := 0; i < 4; i++ {
		viaIface(&box{i})
	}
	fmt.Println("viaIface returned four times")
	defer fmt.Println("the frame survived the calls above")
	discard(9)
}
`

// TestToolexecGivesADiscardedResultItsSpaceInTheFrame builds the program above
// and compares every line against gc.
func TestToolexecGivesADiscardedResultItsSpaceInTheFrame(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/discarded\n\ngo 1.27\n",
		"main.go": framedResultAreaProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "discarded", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "discarded"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
