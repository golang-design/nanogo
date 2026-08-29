// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// Equality over a composite whose fields are not all one word.
//
// specs/025-lowering-and-rules.md's "Equality is compared field by field":
// a value wider than a register is compared by the parts decomposition
// produced, and each field is compared the way the language compares that
// field's type. A string field is runtime.memequal over its bytes and an
// integer field is one machine comparison, and both terms are joined by And in
// the same expression.
//
// gc is the oracle. Every answer below is a place where a plausible
// implementation gives a different one: comparing the bytes of a padded struct
// answers unequal for two equal values, comparing the bits of a float answers
// equal for two NaNs, and comparing the two words of a string header answers
// unequal for two equal strings at two addresses. Nothing here is a literal
// written into this file, so a wrong expectation is a wrong expectation about
// Go rather than about nanogo.

const structEqualProgram = `package main

import "unsafe"

// padded holds a string, so the whole struct takes the field-wise walk, and it
// holds seven bytes of padding after its first field.
type padded struct {
	a int8
	b int64
	s string
}

//go:noinline
func eqPadded(x, y padded) bool { return x == y }

//go:noinline
func poke(p *padded, off uintptr, v byte) {
	*(*byte)(unsafe.Add(unsafe.Pointer(p), off)) = v
}

type withFloat struct {
	f float64
	s string
}

//go:noinline
func eqFloat(x, y withFloat) bool { return x == y }

type inner struct {
	s string
}

type nest struct {
	i inner
	n int
}

//go:noinline
func eqNest(x, y nest) bool { return x == y }

type two struct {
	a string
	b string
}

//go:noinline
func eqTwo(x, y two) bool { return x == y }

type blank struct {
	_ string
	n int
}

//go:noinline
func eqBlank(x, y blank) bool { return x == y }

//go:noinline
func eqArr(x, y [2]string) bool { return x == y }

// sel is the shape export.Dict.MethodExprIndex compares, and index is the
// loop it compares it in, so the call lands in a block with a memory phi
// rather than in straight-line code.
type sel struct {
	pkg  *int
	name string
}

//go:noinline
func index(have []sel, s sel) int {
	for i, h := range have {
		if h == s {
			return i
		}
	}
	return -1
}

// copyOf returns a string with the same bytes at a different address, so that
// a comparison of the two data pointers answers differently from Go.
//
//go:noinline
func copyOf(s string) string {
	b := []byte(s)
	return string(b)
}

func main() {
	var x, y padded
	x.a, x.b, x.s = 1, 2, "k"
	y.a, y.b, y.s = 1, 2, "k"
	poke(&x, 1, 0xaa)
	poke(&y, 1, 0x55)
	println("padding ignored:", eqPadded(x, y))
	y.b = 3
	println("field read:", eqPadded(x, y))

	var zero float64
	nan := zero / zero
	println("nan:", eqFloat(withFloat{nan, "s"}, withFloat{nan, "s"}))
	println("negzero:", eqFloat(withFloat{zero * -1, "s"}, withFloat{zero, "s"}))
	println("float differs:", eqFloat(withFloat{1, "s"}, withFloat{2, "s"}))

	a := copyOf("hello")
	println("bytes not pointers:", eqNest(nest{inner{a}, 7}, nest{inner{"hello"}, 7}))
	println("nested field:", eqNest(nest{inner{a}, 7}, nest{inner{"hello"}, 8}))

	println("two strings:", eqTwo(two{copyOf("ab"), copyOf("cd")}, two{"ab", "cd"}))
	println("two strings differ:", eqTwo(two{copyOf("ab"), copyOf("cd")}, two{"ab", "ce"}))
	println("lengths differ:", eqTwo(two{"ab", "cd"}, two{"abc", "cd"}))

	println("blank skipped:", eqBlank(blank{"p", 4}, blank{"q", 4}))
	println("blank field read:", eqBlank(blank{"p", 4}, blank{"p", 5}))

	println("array of strings:", eqArr([2]string{copyOf("x"), "y"}, [2]string{"x", "y"}))
	println("array differs:", eqArr([2]string{"x", "y"}, [2]string{"x", "z"}))

	p, q := new(int), new(int)
	table := []sel{{p, "a"}, {p, "b"}, {q, "a"}}
	println("in a loop:", index(table, sel{p, copyOf("b")}))
	println("loop misses:", index(table, sel{p, "c"}))
	println("loop reads the pointer:", index(table, sel{q, "b"}))
}
`

// TestStructEqualityWithAStringField compiles, links and runs the program, and
// compares every answer against the installed compiler.
func TestStructEqualityWithAStringField(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/structeq\n\ngo 1.27\n",
		"main.go": structEqualProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "structeq", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := string(runProgram(t, filepath.Join(h.mod, "structeq")))
	want := string(gcOutput(t, h))
	if got != want {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
	// The answers gc gives, named one by one, so that a failure says which
	// property broke rather than which line of output differs. A run where gc
	// itself printed something else is a wrong test and fails here too.
	for _, line := range []string{
		// Go leaves the bytes between two fields undefined.
		"padding ignored: true",
		"field read: false",
		// NaN is not equal to itself and negative zero is equal to positive
		// zero, so a float field is not a compare of the bits.
		"nan: false",
		"negzero: true",
		"float differs: false",
		// A string is its bytes and not the address they sit at.
		"bytes not pointers: true",
		"nested field: false",
		"two strings: true",
		"two strings differ: false",
		"lengths differ: false",
		// A field named _ is not compared.
		"blank skipped: true",
		"blank field read: false",
		"array of strings: true",
		"array differs: false",
		// The call inside a loop body, where repairMemory has to thread it
		// through a memory phi.
		"in a loop: 1",
		"loop misses: -1",
		"loop reads the pointer: -1",
	} {
		if !strings.Contains(want, line+"\n") {
			t.Errorf("gc did not print %q, so the case does not test what it says:\n%s", line, want)
		}
		if !strings.Contains(got, line+"\n") {
			t.Errorf("nanogo did not print %q:\n%s", line, got)
		}
	}
}
