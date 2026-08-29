// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// Range over a function, run as a program.
//
// The row is a rewrite and not a lowering: ir/rangefunc.go turns the loop body
// into a closure and the loop into a call (specs/020-ir.md). Everything hard
// about it is the control flow out of that closure, and every one of those
// shapes is a wrong answer that still compiles and links: a break that leaves
// the iteration running, a return that returns from the yield function rather
// than from the function the source wrote it in, a continue that stops the
// loop. gc is the oracle, because a compiler can be self-consistently wrong
// about all three.
//
// Each answer below is a number chosen so that a wrong one differs from a
// right one rather than merely being unlikely. A break that did not stop the
// iteration sums 1+2+3+4 rather than 1+2, a continue that stopped it sums 1
// rather than 1+3+4, and a return from the wrong frame either runs off the end
// of the loop and returns the other value or leaves the results zero.
const rangeFuncProgram = `package main

// count4 hands out 1, 2, 3, 4 and stops as soon as the body says stop, which
// is what a well-behaved iterator does with a false result.
func count4(yield func(int) bool) {
	for i := 1; i <= 4; i++ {
		if !yield(i) {
			return
		}
	}
}

// pairs hands out two values at a time.
func pairs(yield func(int, string) bool) {
	if !yield(1, "one") {
		return
	}
	if !yield(2, "two") {
		return
	}
	yield(3, "three")
}

// stopsEarly ends the sequence itself, with the body never asking it to.
func stopsEarly(yield func(int) bool) {
	yield(10)
	yield(20)
}

// Seq and Pairs are defined types whose underlying type is the iterator's,
// which is the shape the standard library hands out: iter.Seq and iter.Seq2
// are defined types and every range over one reads through to the underlying
// signature. A row that read the operand's type rather than its core type
// would carry the literal spellings above and refuse these.
type Seq func(func(int) bool)

type Pairs func(func(int, string) bool)

// noValues takes a yield of no arguments, so the loop has no variables at all.
func noValues(yield func() bool) {
	for i := 0; i < 3; i++ {
		if !yield() {
			return
		}
	}
}

func plain() int {
	sum := 0
	for v := range count4 {
		sum = sum + v
	}
	return sum
}

func breaks() int {
	sum := 0
	for v := range count4 {
		if v == 3 {
			break
		}
		sum = sum + v
	}
	return sum
}

func continues() int {
	sum := 0
	for v := range count4 {
		if v == 2 {
			continue
		}
		sum = sum + v
	}
	return sum
}

// returns leaves the function around the loop from inside the body, which is
// the shape that has to cross two frames: the body stops the iteration and the
// frame that called the iterator returns.
func returns() (int, string) {
	for v := range count4 {
		if v == 3 {
			return v * 100, "from the body"
		}
	}
	return -1, "ran off the end"
}

// bare returns with no values from a body, leaving the named result as the
// body last wrote it.
func bare() (n int) {
	for v := range count4 {
		n = n + v
		if v == 2 {
			return
		}
	}
	return -1
}

func nested() int {
	n := 0
	for a := range count4 {
		for b := range count4 {
			n = n + a*10 + b
			if b == 2 {
				break
			}
		}
	}
	return n
}

// returnFromNested returns from the body of the inner loop, so both
// iterations have to stop before the return happens.
func returnFromNested() int {
	for a := range count4 {
		for b := range count4 {
			if a == 2 && b == 3 {
				return a*100 + b
			}
		}
	}
	return -1
}

func early() int {
	sum := 0
	for v := range stopsEarly {
		sum = sum + v
	}
	return sum
}

func novars() int {
	n := 0
	for range noValues {
		n = n + 1
	}
	return n
}

func two() string {
	s := ""
	for k, v := range pairs {
		if k == 3 {
			break
		}
		s = s + v
	}
	return s
}

// assigns uses the "= range" form, whose variable belongs to the frame around
// the loop and holds its last value after the loop.
func assigns() int {
	v := 0
	last := 0
	for v = range count4 {
		last = last*10 + v
	}
	return v*10000 + last
}

// inner has a break that belongs to a switch and a break that belongs to an
// ordinary loop, and neither stops the range.
func inner() int {
	n := 0
	for v := range count4 {
		switch v {
		case 2:
			break
		case 4:
			n = n + 1000
		}
		for i := 0; i < 3; i++ {
			if i == 1 {
				break
			}
			n = n + 1
		}
		n = n + v
	}
	return n
}

// literalInBody writes a function literal inside the body. Its return is its
// own and returns from neither the loop nor the function around it.
func literalInBody() int {
	n := 0
	for v := range count4 {
		f := func() int {
			if v == 2 {
				return 100
			}
			return v
		}
		n = n + f()
	}
	return n
}

// asSeq hands back the iterator as a value of the defined type, so the range
// operand is a call whose result is named rather than a function name.
func asSeq() Seq { return count4 }

func named() int {
	sum := 0
	for v := range asSeq() {
		if v == 4 {
			break
		}
		sum = sum + v
	}
	return sum
}

func namedReturn() int {
	var s Seq = count4
	for v := range s {
		if v == 3 {
			return v * 11
		}
	}
	return -1
}

func namedPairs() string {
	var p Pairs = pairs
	s := ""
	for k, v := range p {
		if k == 2 {
			return s + "!"
		}
		s = s + v
	}
	return s
}

// voidSeen is what voidReturn writes, because a function with no results has
// nothing to return and the count is the only way to see how far it went.
var voidSeen int

// voidReturn returns from inside the body of a function that has no results at
// all. It is the one path where the frame that holds the loop returns with
// nothing to copy out, and where the body's report is the whole of what
// crosses the two frames.
func voidReturn() {
	for v := range count4 {
		voidSeen = voidSeen + v
		if v == 2 {
			return
		}
	}
	voidSeen = voidSeen + 1000
}

// deferAround defers in the function that holds the loop and returns from
// inside the body. The deferred function runs after the loop is over and sees
// the result the body wrote, which is the whole of the interaction between
// this row and the single exit of specs/033-closures-defer-panic.md.
func deferAround() (n int) {
	defer func() { n = n + 7 }()
	for v := range count4 {
		if v == 2 {
			return v
		}
	}
	return 0
}

func main() {
	println("plain", plain())
	println("breaks", breaks())
	println("continues", continues())
	a, b := returns()
	println("returns", a, b)
	println("bare", bare())
	println("nested", nested())
	println("returnFromNested", returnFromNested())
	println("early", early())
	println("novars", novars())
	println("two", two())
	println("assigns", assigns())
	println("inner", inner())
	println("literalInBody", literalInBody())
	println("named", named())
	println("namedReturn", namedReturn())
	println("namedPairs", namedPairs())
	voidReturn()
	println("voidReturn", voidSeen)
	println("deferAround", deferAround())
}
`

// TestRangeOverFuncMatchesGc builds the program with nanogo and with the
// installed compiler and compares what the two write.
//
// The comparison is the whole output, line for line. Nothing here prints an
// address, so there is nothing to normalise: every byte of the answer is
// decided by the row.
func TestRangeOverFuncMatchesGc(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/rangefunc\n\ngo 1.27\n",
		"main.go": rangeFuncProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "rangefunc", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := string(runProgram(t, filepath.Join(h.mod, "rangefunc")))
	want := string(gcOutput(t, h))
	if got != want {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
	// The answers spelled out, so that two compilers agreeing on a wrong one
	// is still a failure. Each is a number a plausible mistake changes:
	// breaks sums 1+2 and not 1+2+3+4, continues sums 1+3+4 and not 1,
	// returns comes from inside the body and not from the line after the
	// loop, and deferAround runs its deferred function after a return the
	// body asked for.
	for _, line := range []string{
		"plain 10",
		"breaks 3",
		"continues 8",
		"returns 300 from the body",
		"bare 3",
		"nested 212",
		"returnFromNested 203",
		"early 30",
		"novars 3",
		"two onetwo",
		"assigns 41234",
		"inner 1014",
		"literalInBody 108",
		"named 6",
		"namedReturn 33",
		"namedPairs one!",
		"voidReturn 3",
		"deferAround 9",
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("the output has no %q:\n%s", line, got)
		}
	}
}
