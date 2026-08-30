// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// The promoted method wrappers of specs/032-type-descriptors-and-itabs.md, run
// as a program.
//
// A type satisfies an interface through a method promoted from an embedded
// field, and the itab's Fun entry names a function with that type's receiver.
// No declaration carries the name, so the compiler generates it. A unit test
// says the function was generated and what it calls; only a program says the
// path selects the right field, that the receiver arrives whole, and that the
// answer is the one the language specifies.
//
// Every shape the wrapper has to walk is here: an embedded pointer, an
// embedded value whose method needs the address of the field, two levels of
// embedding, a type whose method set mixes promoted and declared methods, and
// a nil embedded pointer, which has to fault where gc's wrapper faults.
const promoteProgram = `package main

import "fmt"

type inner struct{ n int }

func (i *inner) Ptr() int { return i.n }

func (i inner) Val() int { return i.n * 2 }

// Through an embedded pointer both receiver forms of inner promote, so
// pointed's own value method set holds them both.
type pointed struct{ *inner }

// Through an embedded value only the value receiver method promotes into
// held's method set. Ptr is in the method set of *held alone, and the wrapper
// for it takes the address of the embedded field.
type held struct{ inner }

// Two levels, so the path is two field selections, and the second one is not
// at offset zero of the first.
type middle struct{ held }

type outer struct {
	k int
	middle
}

// Own is declared here, so outer's method set mixes a declaration with two
// promoted methods and the itab holds one of each.
func (o outer) Own() int { return o.k * 10 }

// An alias to an unnamed struct, which declares nothing itself, so the walk
// goes through it to reach inner.
type alias = struct{ inner }

type aliased struct{ alias }

type both interface {
	Ptr() int
	Val() int
}

type mixed interface {
	Val() int
	Own() int
}

type value interface{ Val() int }

// caught reports what a fault inside a wrapper does. The traceback of a
// process that dies of one counts frames, and gc tail-calls out of its wrapper
// where nanogo leaves a frame behind, so the frames differ in a way that says
// nothing about the fault. That there was one is the same on both sides.
func caught(f func() int) (r int) {
	defer func() {
		if recover() != nil {
			r = -1
		}
	}()
	return f()
}

func main() {
	var a both = pointed{&inner{n: 7}}
	fmt.Println("embedded pointer", a.Ptr(), a.Val())

	var b both = &held{inner{n: 9}}
	fmt.Println("embedded value", b.Ptr(), b.Val())

	var c value = held{inner{n: 11}}
	fmt.Println("embedded value by value", c.Val())

	var d mixed = outer{k: 3, middle: middle{held{inner{n: 4}}}}
	fmt.Println("two levels and a declaration", d.Val(), d.Own())

	var e both = &outer{k: 5, middle: middle{held{inner{n: 6}}}}
	fmt.Println("two levels through a pointer", e.Ptr(), e.Val())

	var h both = &aliased{alias{inner{n: 13}}}
	fmt.Println("through an unnamed struct", h.Ptr(), h.Val())

	var f both = pointed{}
	fmt.Println("nil embedded pointer", caught(func() int { return f.Ptr() }),
		caught(func() int { return f.Val() }))

	// The same methods reached without an interface, so the declaration and
	// the wrapper have to give one answer.
	g := pointed{&inner{n: 12}}
	fmt.Println("direct", g.Ptr(), g.Val(), outer{k: 1, middle: middle{held{inner{n: 2}}}}.Val())
}
`

// TestGcAndNanogoAgreeOnAPromotedMethod is the evidence that the function an
// itab names for a promoted method is generated, links, and computes what the
// language says.
//
// Before it was generated the compiler accepted this program and go tool link
// reported "relocation target main.pointed.Ptr not defined", which names
// neither the type nor the spec that owns the gap. The comparison is against
// an all-gc build of the same source, because the answer is the language's and
// not this compiler's.
func TestGcAndNanogoAgreeOnAPromotedMethod(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/promote\n\ngo 1.27\n",
		"main.go": promoteProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "promote", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "promote"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// The library that declares the types, so that the package which converts them
// to an interface is not the package that declares them.
//
// gc generates a promoted wrapper in the compilation unit that declares the
// type, and nanogo generates one wherever it writes the descriptor that names
// it. The two rules meet here: nanogo compiles the importer alone, so the
// wrapper it names has to be the one gc already wrote into the library.
const promoteLibrary = `package lib

type Inner struct{ N int }

func (i *Inner) Ptr() int { return i.N }

func (i Inner) Val() int { return i.N * 2 }

type Pointed struct{ *Inner }

type Held struct{ Inner }

// Outer's methods are promoted twice over: Mid promotes them from Inner and
// Outer promotes them from Mid. The importer stops at Outer and names its
// symbols, which are themselves wrappers gc generated rather than
// declarations, so this is the hop that says stopping at an imported type is
// right whether its method is declared or promoted.
type Mid struct{ Inner }

type Outer struct{ *Mid }

func NewPointed(n int) Pointed { return Pointed{&Inner{N: n}} }

func NewHeld(n int) Held { return Held{Inner{N: n}} }

func NewOuter(n int) Outer { return Outer{&Mid{Inner{N: n}}} }
`

const promoteImporter = `package main

import (
	"fmt"

	"nanogo.example/promote2/lib"
)

type both interface {
	Ptr() int
	Val() int
}

func main() {
	var a both = lib.NewPointed(7)
	fmt.Println("imported pointer", a.Ptr(), a.Val())
	h := lib.NewHeld(9)
	var b both = &h
	fmt.Println("imported value", b.Ptr(), b.Val())
	var c both = lib.NewOuter(11)
	fmt.Println("imported twice over", c.Ptr(), c.Val())
}
`

// TestGcAndNanogoAgreeOnAPromotedMethodOfAnImportedType checks the side of the
// rule that says the declaring package owns the wrapper.
//
// nanogo compiles only the importer, so every wrapper the itabs it writes name
// has to be resolved against the library gc compiled. A compiler that
// generated its own copy of the path here would be writing a function from a
// method set it read out of export data, which is the case
// specs/032-type-descriptors-and-itabs.md leaves to the declaring package.
func TestGcAndNanogoAgreeOnAPromotedMethodOfAnImportedType(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/promote2\n\ngo 1.27\n",
		"lib/lib.go": promoteLibrary,
		"main.go":    promoteImporter,
	}, []string{"# nanogo owns the importing package and gc owns the library that declares the types", "main"})

	if out, err := h.build(t, "-o", "promote2", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the importing package:\n%s", strings.Join(lines, "\n"))
	}
	if compiled(lines, "nanogo.example/promote2/lib") {
		t.Fatalf("nanogo compiled the library, so no wrapper crossed the boundary:\n%s",
			strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "promote2"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
