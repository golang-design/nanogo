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

// The program the cross-package half of specs/013-generics.md has to compile.
//
// Every generic here is declared by the standard library and instantiated
// here, so no syntax tree for any of the bodies exists in this build: each one
// is decoded out of the archive gc wrote (ir/foreign.go).
//
// The element types are chosen so that a substitution that went wrong shows as
// a wrong answer rather than as a link failure. int is one word, string is two
// and its comparison calls the runtime, and pair is three words with a pointer
// in the middle, so a body stencilled at the wrong type reads the wrong
// number of words and prints something else.
//
// The answers are printed rather than asserted with a division, because the
// claim is agreement with gc and the comparison below is against gc's own
// build of the same source.
const foreignProgram = `package main

import (
	"slices"
	"sync/atomic"
)

type pair struct {
	n    int
	name string
}

var box atomic.Pointer[[]pair]

func main() {
	ints := []int{3, 1, 4, 1, 5}
	println(slices.Contains(ints, 4), slices.Contains(ints, 9))
	println(slices.Index(ints, 1), slices.Index(ints, 9))

	strs := []string{"alpha", "beta", "gamma"}
	println(slices.Contains(strs, "beta"), slices.Contains(strs, "delta"))
	println(slices.Index(strs, "gamma"), slices.Index(strs, "delta"))

	pairs := []pair{{1, "one"}, {2, "two"}, {3, "three"}}
	println(slices.Contains(pairs, pair{2, "two"}), slices.Contains(pairs, pair{2, "three"}))
	println(slices.Index(pairs, pair{3, "three"}), slices.Index(pairs, pair{9, "nine"}))

	// An empty operand, so the loop the body is made of runs no iteration.
	println(slices.Contains([]int{}, 1), slices.Index([]string{}, "x"))

	// The method set of an instantiation of a generic type another package
	// declares. Store and Load are a call through unsafe.Pointer in each
	// direction, and CompareAndSwap reads the word back.
	box.Store(&pairs)
	got := box.Load()
	println(len(*got), (*got)[2].n, (*got)[2].name)
	println(box.CompareAndSwap(&pairs, &pairs), box.CompareAndSwap(nil, &pairs))
	old := box.Swap(&other)
	println(len(*old), len(*box.Load()))
}

var other = []pair{{7, "seven"}}
`

// foreignModule is the module the tests below build.
func foreignModule() map[string]string {
	return map[string]string{
		"go.mod":  "module nanogo.example/foreigngeneric\n\ngo 1.27\n",
		"main.go": foreignProgram,
	}
}

// TestToolexecRunsAGenericOfAnotherPackage is the evidence the join is
// correct and not merely accepted.
//
// A body stencilled out of an archive compiles whatever it computes, so the
// question is whether the linked program agrees with the one gc builds from
// the same source. It is answered by running both.
func TestToolexecRunsAGenericOfAnotherPackage(t *testing.T) {
	h := setup(t, foreignModule(), []string{"main"})

	if out, err := h.build(t, "-o", "foreign", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "foreign"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// TestToolexecNamesAForeignStencilCanonically reads the symbols back.
//
// The name of an instantiation does not say which package instantiated it, so
// two packages that both reach slices.Index at one type argument list name one
// symbol. It also has to be a name gc does not write, because gc's own copy of
// slices in the same binary stencils by GC shape.
func TestToolexecNamesAForeignStencilCanonically(t *testing.T) {
	h := setup(t, foreignModule(), []string{"main"})

	if out, err := h.build(t, "-o", "foreign", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	out, err := exec.Command(goTool(t), "tool", "nm", filepath.Join(h.mod, "foreign")).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm: %v\n%s", err, out)
	}
	nm := string(out)
	for _, want := range []string{
		"slices.Contains[[]int,int]",
		"slices.Index[[]int,int]",
		"slices.Contains[[]string,string]",
		"slices.Contains[[]main.pair,main.pair]",
		"sync/atomic.(*Pointer[[]main.pair]).Store",
		"sync/atomic.(*Pointer[[]main.pair]).Load",
	} {
		if !strings.Contains(nm, want) {
			t.Errorf("the program holds no symbol %s", want)
		}
	}
}

// The two packages the export half needs.
//
// gc reads what nanogo wrote for the middle package, so this is the path where
// a body nanogo offered for inlining reaches gc's inliner. A body naming an
// instantiation is not offered (export/bodyinline.go): gc inlines the offered
// body into main and then inlines the instantiation into that, and cmd/link
// then stops with "inlined function slices.Contains[go.shape.[]int,go.shape.int]
// missing func info".
func foreignExportModule() map[string]string {
	return map[string]string{
		"go.mod": "module nanogo.example/foreignexport\n\ngo 1.27\n",
		"lib/lib.go": `package lib

import (
	"slices"
	"sync/atomic"
)

type Pair struct {
	N    int
	Name string
}

var box atomic.Pointer[[]Pair]

func HasInt(s []int, v int) bool          { return slices.Contains(s, v) }
func HasPair(s []Pair, v Pair) bool       { return slices.Contains(s, v) }
func IndexOfName(s []string, v string) int { return slices.Index(s, v) }

func Store(v []Pair) { box.Store(&v) }
func Load() *[]Pair  { return box.Load() }
`,
		"main.go": `package main

import (
	"slices"

	"nanogo.example/foreignexport/lib"
)

func main() {
	// The same instantiation as the library's, in a package gc compiles. Both
	// compilers put a definition of slices.Contains[[]int,int] in the binary
	// and the linker keeps one.
	println(slices.Contains([]int{1, 2, 3}, 3), slices.Contains([]int{1, 2, 3}, 9))
	println(lib.HasInt([]int{1, 2, 3}, 2), lib.HasInt([]int{1, 2, 3}, 9))
	println(lib.HasPair([]lib.Pair{{1, "a"}, {2, "b"}}, lib.Pair{2, "b"}))
	println(lib.IndexOfName([]string{"x", "y"}, "y"), lib.IndexOfName([]string{"x"}, "z"))
	v := []lib.Pair{{7, "seven"}}
	lib.Store(v)
	got := lib.Load()
	println(len(*got), (*got)[0].N, (*got)[0].Name)
}
`,
	}
}

// TestToolexecExportsAPackageThatInstantiatesAForeignGeneric is the other side
// of the same program.
//
// nanogo compiles the middle package and gc compiles main against the export
// data nanogo wrote for it, which is the arrangement a real build has. The
// comparison is against an all-gc build of the same source.
//
// main instantiates slices.Contains at []int as well, so both compilers put a
// definition of slices.Contains[[]int,int] in the binary. gc's is the wrapper
// it generates for an instantiation of another package's generic and nanogo's
// is a full stencil, and the two compute the same function, which is what
// instanceSym's naming rule is for. A build that stopped on the duplicate, or
// a binary that answered differently, would show here.
func TestToolexecExportsAPackageThatInstantiatesAForeignGeneric(t *testing.T) {
	h := setup(t, foreignExportModule(), []string{"nanogo.example/foreignexport/lib"})

	if out, err := h.build(t, "-o", "foreign", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/foreignexport/lib") {
		t.Fatalf("nanogo delegated the library:\n%s", strings.Join(lines, "\n"))
	}
	if compiled(lines, "main") {
		t.Fatalf("nanogo compiled main, so gc never read the export data:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "foreign"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("the mixed build printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// The module two nanogo-compiled packages instantiate one foreign generic in.
//
// The name of an instantiation does not say which package reached it, so both
// packages here spell one symbol for slices.Contains[[]int,int] and both emit
// a body for it. gc has the same arrangement and marks each copy dupok, so
// cmd/link keeps one.
func foreignSharedModule() map[string]string {
	lib := func(name string) string {
		return "package " + name + "\n\nimport \"slices\"\n\n" +
			"func Has(s []int, v int) bool { return slices.Contains(s, v) }\n"
	}
	return map[string]string{
		"go.mod": "module nanogo.example/foreignshared\n\ngo 1.27\n",
		"a/a.go": lib("a"),
		"b/b.go": lib("b"),
		"main.go": `package main

import (
	"nanogo.example/foreignshared/a"
	"nanogo.example/foreignshared/b"
)

func main() {
	println(a.Has([]int{1, 2, 3}, 2), a.Has([]int{1, 2, 3}, 9))
	println(b.Has([]int{4, 5, 6}, 5), b.Has([]int{4, 5, 6}, 9))
}
`,
	}
}

// TestToolexecSharesAForeignStencilBetweenTwoPackages measures the
// deduplication instanceSym's naming rule is for.
//
// Before an instantiation of another package's generic could be built, no two
// packages could reach one, so the claim that two of them name one symbol and
// the linker keeps one copy had never been exercised. It is exercised here.
//
// The symbol count is the assertion and the output is not, because a program
// with two copies of one body still computes what Go says it computes. What
// two copies cost is size, and what they say is that an instantiation reaches
// the object as an ordinary package definition, which cmd/link takes as unique
// by construction. gc marks every stencil dupok so that its linker merges
// them, and ir.Func carries no flag that would say so.
func TestToolexecSharesAForeignStencilBetweenTwoPackages(t *testing.T) {
	h := setup(t, foreignSharedModule(), []string{
		"nanogo.example/foreignshared/a",
		"nanogo.example/foreignshared/b",
	})

	if out, err := h.build(t, "-o", "shared", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	for _, pkg := range []string{"nanogo.example/foreignshared/a", "nanogo.example/foreignshared/b"} {
		if !compiled(lines, pkg) {
			t.Fatalf("nanogo delegated %s:\n%s", pkg, strings.Join(lines, "\n"))
		}
	}

	got := runProgram(t, filepath.Join(h.mod, "shared"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}

	out, err := exec.Command(goTool(t), "tool", "nm", filepath.Join(h.mod, "shared")).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm: %v\n%s", err, out)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.LastIndexByte(line, ' '); i >= 0 && line[i+1:] == "slices.Contains[[]int,int]" {
			n++
		}
	}
	if n == 0 {
		t.Fatal("the program holds no symbol slices.Contains[[]int,int]")
	}
	if n != 1 {
		t.Errorf("the program holds %d definitions of slices.Contains[[]int,int], want 1: "+
			"an instantiation reaches the object as a package definition, which cmd/link takes as "+
			"unique by construction, and ir.Func has no flag that would mark it duplicate-tolerant "+
			"the way gc marks its own stencils", n)
	}
}
