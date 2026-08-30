// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// A wrapper for a variadic method, run as a program.
//
// specs/032-type-descriptors-and-itabs.md names (*T).M in the itab of every
// method M declared with a value receiver, so a variadic method with a value
// receiver needs a generated wrapper the same way a fixed one does. That
// wrapper was refused, and the reason given was that the wrapper would pack
// its last parameter into a second slice.
//
// It does not. Packing happens once, in ir.Build's callArgs, which reads the
// "..." from the syntax of a call. ir.Type.Params is what comes out of that,
// and for func(...int) its last entry is already []int, so a wrapper built
// from the list forwards a slice into a slice. The refusal was a reading and
// not a measurement, and this program is the measurement.
//
// The three call forms are here because they diverge only under a wrapper that
// repacks: a spread call would then get one extra level and an empty call
// would get a slice of one empty slice rather than an empty slice.
const variadicWrapperProgram = `package main

import "fmt"

type counter struct{ base int }

// A value receiver, so the itab for *counter names a wrapper.
func (c counter) Sum(xs ...int) int {
	t := c.base
	for _, x := range xs {
		t += x
	}
	return t
}

// A variadic method with fixed parameters in front of the "...", so the
// wrapper has to keep those in their places as well.
func (c counter) Tag(label string, xs ...int) string {
	return fmt.Sprint(label, c.base, xs)
}

// A variadic method of a type wide enough that the receiver does not travel in
// one register, which is the shape the argument placement of specs/030-abi.md
// gets wrong when the slice and the receiver are laid out from the word list.
type wide struct {
	a, b, c, d int
}

func (w wide) Sum(xs ...int) int {
	t := w.a + w.b + w.c + w.d
	for _, x := range xs {
		t += x
	}
	return t
}

type adder interface {
	Sum(xs ...int) int
}

type tagger interface {
	Tag(label string, xs ...int) string
}

func main() {
	var a adder = &counter{base: 100}
	fmt.Println("through the wrapper", a.Sum(), a.Sum(1), a.Sum(1, 2, 3))

	s := []int{4, 5, 6}
	fmt.Println("spread", a.Sum(s...), a.Sum([]int{}...), a.Sum(nil...))

	var g tagger = &counter{base: 7}
	fmt.Println("fixed before the dots", g.Tag("x"), g.Tag("y", 1, 2))

	var w adder = &wide{a: 1, b: 2, c: 3, d: 4}
	fmt.Println("wide receiver", w.Sum(), w.Sum(10, 20))

	// The same methods without an interface, so the declaration and the
	// wrapper have to give one answer.
	c := counter{base: 100}
	fmt.Println("direct", c.Sum(1, 2, 3), c.Sum(s...), (&c).Sum())
}
`

// TestGcAndNanogoAgreeOnAVariadicMethodWrapper is the evidence that the
// wrapper an itab names for a variadic method is generated, links, and passes
// the packed slice through once.
func TestGcAndNanogoAgreeOnAVariadicMethodWrapper(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/variadicwrapper\n\ngo 1.27\n",
		"main.go": variadicWrapperProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "variadicwrapper", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "variadicwrapper"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
