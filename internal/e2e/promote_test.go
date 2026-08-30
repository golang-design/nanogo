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

// The library whose unexported method names belong to it.
//
// An unexported name belongs to the package that spells it, so scalar here and
// scalar in the importer are two names and neither shadows the other. Scaler
// is how the importer reaches these ones: it cannot write scalar and mean
// lib's, and a value of a type in the importer satisfies the interface only
// through the methods it promotes from Encoder.
//
// Each method answers with a different multiple of N, and the importer's own
// methods answer with something else again, so a wrapper that reached the
// wrong function prints a wrong number rather than the right one.
const shadowLibrary = `package lib

type Encoder struct{ N int }

func (e *Encoder) scalar() int { return e.N }

func (e *Encoder) bigInt() int { return e.N * 100 }

func (e *Encoder) Value() int { return e.scalar() + e.bigInt() }

type Scaler interface {
	scalar() int
	bigInt() int
}

func Call(s Scaler) int { return s.scalar() }

func CallBig(s Scaler) int { return s.bigInt() }
`

// The importer that declares methods of the same names.
//
// This is export.bodyWriter, reduced. bodyWriter embeds *pkgbits.Encoder and
// declares its own scalar and bigInt, so its method set holds four methods
// under two names, and the two of each pair need two symbols.
//
// Two receiver shapes, because the descriptor and the itab name different
// forms of the wrapper for each. writer is two words, so an interface holding
// one holds a pointer and the itab names the pointer form. packed is one
// pointer word, so it is stored directly in an interface and the itab names
// the value form, which is the form that was missing.
//
// The pad in front of writer's embedded field puts the field at a nonzero
// offset, so a wrapper that dropped the offset would load the wrong word.
const shadowImporter = `package main

import (
	"fmt"

	"nanogo.example/shadow/lib"
)

type writer struct {
	pad int
	*lib.Encoder
}

func (w *writer) scalar() int { return w.pad * 3 }

func (w *writer) bigInt() int { return w.pad * 5 }

type packed struct{ *lib.Encoder }

func (p *packed) scalar() int { return 11 }

func main() {
	w := &writer{pad: 2, Encoder: &lib.Encoder{N: 3}}
	fmt.Println("declared", w.scalar(), w.bigInt())
	fmt.Println("promoted through a pointer", lib.Call(w), lib.CallBig(w))
	fmt.Println("promoted through a value", lib.Call(*w), lib.CallBig(*w))

	p := packed{&lib.Encoder{N: 7}}
	fmt.Println("stored directly in the interface", lib.Call(p), lib.CallBig(p))
	fmt.Println("declared on the direct one", p.scalar())

	// The descriptors of the two value types, which is where the missing
	// wrapper was named.
	var a any = *w
	var b any = p
	fmt.Println("boxed", a != nil, b != nil)

	// The exported method the library calls its own unexported ones from, so
	// the answers the wrappers give can be checked against the declarations
	// reached without one.
	fmt.Println("through the embedded type", w.Value(), p.Value())
}
`

// TestGcAndNanogoAgreeOnAShadowedUnexportedMethod is the evidence that two
// unexported methods of one name from two packages stay two methods.
//
// A method symbol carries the method's own package in front of the name when
// that package is not the receiver type's, which is gc's ir.MethodSymSuffix.
// Without it the declaration and the wrapper for the promoted method are one
// symbol: the object holds two definitions of main.(*writer).scalar and
// nothing defines main.writer.scalar, which the descriptor of the value type
// names. That is how nanogo's own export package left the self-hosting link
// with
//
//	type:golang.design/x/nanogo/export.bodyWriter: relocation target
//	golang.design/x/nanogo/export.bodyWriter.scalar not defined
//
// A link that succeeds is only half the claim. The other half is that each
// wrapper reaches the method the language selects, which is why every call
// here answers with a different number and why the comparison is against an
// all-gc build of the same source.
func TestGcAndNanogoAgreeOnAShadowedUnexportedMethod(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/shadow\n\ngo 1.27\n",
		"lib/lib.go": shadowLibrary,
		"main.go":    shadowImporter,
	}, []string{"# nanogo owns the importer, which is where both names meet", "main"})

	if out, err := h.build(t, "-o", "shadow", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the importing package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "shadow"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
