// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The program that says a declaration is a fresh variable every time it runs.
//
// The specification gives each execution of a declaration its own variable, so
// "var n int" in the body of a loop is zero on every iteration. A compiler
// that zeroes the frame once at the entry and treats the declaration as no
// statement at all gives the second iteration what the first one left, and
// nothing at build time says so: the program compiles, links and runs, and it
// answers wrongly.
//
// Every storage class is exercised, because the zero is written differently
// for each: a variable that lives in a register takes a constant, one that
// lives in the frame is cleared through memory, and one a closure captures
// lives in a heap cell allocated where the declaration stands.
//
// The exit status is the assertion. Each check divides by zero and the process
// dies, as in the other programs here.
const declareProgram = `package main

//go:noinline
func crash() {
	d := 0
	d = d / d
}

type wide struct{ a, b, c, d int }

//go:noinline
func use(x int) int { return x }

func main() {
	// A scalar, which lives in a value.
	for i := 0; i < 4; i++ {
		var n int
		n += use(1)
		if n != 1 {
			crash()
		}
	}

	// A struct wider than a value, which lives in the frame and is cleared
	// through memory.
	for i := 0; i < 4; i++ {
		var w wide
		w.a += use(2)
		w.d += use(3)
		if w.a != 2 || w.b != 0 || w.c != 0 || w.d != 3 {
			crash()
		}
	}

	// A slice, whose zero is the nil header and not the previous length.
	for i := 0; i < 4; i++ {
		var xs []int
		if xs != nil || len(xs) != 0 {
			crash()
		}
		xs = append(xs, i)
		if len(xs) != 1 {
			crash()
		}
	}

	// An interface, whose zero is two nil words.
	for i := 0; i < 4; i++ {
		var v any
		if v != nil {
			crash()
		}
		v = i
	}

	// A variable a closure captures, which lives in a heap cell.
	var last func() int
	for i := 0; i < 4; i++ {
		var c int
		f := func() int {
			c += use(5)
			return c
		}
		if f() != 5 {
			crash()
		}
		last = f
	}
	if last() != 10 {
		// The last literal's own cell is still there and holds 5, so calling
		// it again adds 5 to it. A cell shared with the earlier iterations
		// would hold more.
		crash()
	}

	// A declaration inside a nested block, entered many times.
	total := 0
	for i := 0; i < 4; i++ {
		{
			var s string
			s += "x"
			total += len(s)
		}
	}
	if total != 4 {
		crash()
	}
}
`

// TestToolexecDeclarationIsFreshEveryIteration is the language rule the entry
// clear of specs/030-abi.md does not satisfy.
//
// It is a build-and-run test rather than a unit test because the claim is
// about what the program computes. The zero is written in three forms and the
// choice among them is made by where the variable lives, which nothing above
// the code generator decides, so a unit test on any one pass checks a part of
// the rule and not the rule.
func TestToolexecDeclarationIsFreshEveryIteration(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/declare\n\ngo 1.27\n",
		"main.go": declareProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "declare", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	if b, err := exec.Command(filepath.Join(h.mod, "declare")).CombinedOutput(); err != nil {
		t.Fatalf("a declaration kept the previous iteration's value: %v\n%s", err, b)
	}
}

// blankProgram declares the blank identifier in every position Go allows it.
//
// Two of them collide in a way nothing above the linker reports. A blank
// package-level variable with an initialiser is an assignment the init
// function makes, and giving it a data symbol names it main._, which is also
// the symbol of a blank *function*. Go allows both in one package, so the
// object then defines main._ as text and refers to it as data, and cmd/link
// says "relocation target main._ not defined for ABI0 (but is defined for
// ABIInternal)", which names neither declaration.
//
// A blank field is the other. The language does not compare one, so two values
// of struct{_, _, _ int} built out of different bytes are equal, and a
// comparison of every part answers unequal. Go's own test/blank.go builds
// exactly that pair through unsafe.
const blankProgram = `package main

import (
	"fmt"
	"unsafe"
)

type blanked struct {
	_, _, _ int
}

type nested struct {
	a int
	_ struct{ x, y int }
	b int
}

func i() int { return 7 }

var _ = i()
var _ int = 1
var _, _ = 3, 4

const _ = 3

type _ int

func _() { panic("a blank function is never called") }

func (blanked) _() {}

func (blanked) _() {}

//go:noinline
func words(a, b, c int) blanked { return *(*blanked)(unsafe.Pointer(&[3]int{a, b, c})) }

//go:noinline
func nestedOf(a, x, y, b int) nested {
	return *(*nested)(unsafe.Pointer(&[4]int{a, x, y, b}))
}

func main() {
	var _ = i()
	_, _ = i(), i()

	fmt.Println("blanked equal", words(1, 2, 3) == words(4, 5, 6))
	fmt.Println("nested equal", nestedOf(1, 2, 3, 4) == nestedOf(1, 9, 9, 4))
	fmt.Println("nested unequal", nestedOf(1, 2, 3, 4) == nestedOf(5, 2, 3, 4))
}
`

// TestToolexecCompilesEveryBlankDeclaration builds the program above and
// compares every line against gc.
func TestToolexecCompilesEveryBlankDeclaration(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/blank\n\ngo 1.27\n",
		"main.go": blankProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "blank", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "blank"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
