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

// The program the statement half of the foreign walk has to compile.
//
// slices.Sort and slices.SortFunc reach pdqsort, which is where the walk stops
// being a handful of expressions: assignments, three-clause loops, parallel
// swaps, an expression switch, break and continue, len, and a method of a
// concrete type of the declaring package, all inside a body no source file of
// this build holds.
//
// Every slice is printed whole rather than asserted with a predicate, and every
// one holds duplicates. A parallel assignment built as two assignments in order
// copies one value into both places, which leaves the result sorted and the
// multiset wrong: "is it sorted" passes and the printed slice does not.
//
// The element types are one word, two words whose comparison calls the runtime,
// and a struct with a pointer in it, so a body stencilled at the wrong type
// reads the wrong number of words.
//
// The long slice is what reaches the rest of pdqsort. Below thirteen elements
// the body sorts by insertion and returns, and below fifty it does not choose a
// pivot by the Tukey ninther, which is the arm that calls a method of the
// declaring package's own xorshift type.
const foreignSortProgram = `package main

import (
	"cmp"
	"slices"
)

type pair struct {
	n    int
	name string
}

func printInts(s []int) {
	for i := 0; i < len(s); i++ {
		println(i, s[i])
	}
}

func main() {
	ints := []int{5, 3, 9, 1, 3, 7, 2, 8, 6, 4, 0, 5, 3, 9, 1, 7, 2, 8, 6, 4}
	slices.Sort(ints)
	printInts(ints)
	println(slices.IsSortedFunc(ints, func(a, b int) int { return cmp.Compare(a, b) }))

	long := make([]int, 61)
	for i := range long {
		long[i] = (i * 37) % 61
	}
	slices.SortFunc(long, func(a, b int) int { return cmp.Compare(a, b) })
	printInts(long)

	strs := []string{"pear", "fig", "apple", "fig", "date", "cherry", "banana", "apple", "elderberry", "date", "grape", "cherry", "kiwi", "lemon"}
	slices.Sort(strs)
	for i := 0; i < len(strs); i++ {
		println(i, strs[i])
	}

	pairs := []pair{{3, "c"}, {1, "a"}, {2, "b"}, {1, "aa"}, {5, "e"}, {4, "d"}, {2, "bb"}, {0, "z"}, {3, "cc"}, {5, "ee"}, {4, "dd"}, {0, "zz"}, {6, "f"}, {6, "ff"}}
	slices.SortFunc(pairs, func(a, b pair) int { return cmp.Compare(a.n, b.n) })
	for i := 0; i < len(pairs); i++ {
		println(i, pairs[i].n, pairs[i].name)
	}
	println(slices.IsSortedFunc(pairs, func(a, b pair) int { return cmp.Compare(a.n, b.n) }))

	println(cmp.Compare(1, 2), cmp.Compare(2, 2), cmp.Compare(3, 2))
	println(cmp.Compare("a", "b"), cmp.Compare("b", "b"), cmp.Compare("c", "b"))
	println(slices.EqualFunc(ints, ints, func(a, b int) bool { return a == b }))
	println(slices.EqualFunc(ints, ints[:3], func(a, b int) bool { return a == b }))
	println(slices.EqualFunc(strs, strs, func(a, b string) bool { return a == b }))
}
`

// foreignSortModule is the module the test below builds.
func foreignSortModule() map[string]string {
	return map[string]string{
		"go.mod":  "module nanogo.example/foreignsort\n\ngo 1.27\n",
		"main.go": foreignSortProgram,
	}
}

// TestToolexecRunsTheStatementFormsOfAForeignGeneric is the evidence for the
// statement half of the walk.
//
// A stencilled body compiles whatever it computes, so the question is whether
// the linked program agrees with the one gc builds from the same source. It is
// answered by running both.
func TestToolexecRunsTheStatementFormsOfAForeignGeneric(t *testing.T) {
	h := setup(t, foreignSortModule(), []string{"main"})

	if out, err := h.build(t, "-o", "foreignsort", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "foreignsort"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// The module whose library holds one generic per construct the foreign walk
// maps, so that a construct the standard library does not reach is still run.
//
// driver's TestCompileStencilsTheConstructsTheForeignWalkMaps builds the same
// library in this process, which is where the coverage comes from. What this
// adds is the answer: each function below returns a value that differs if the
// construct was built wrongly rather than merely accepted.
func foreignFormsModule() map[string]string {
	return map[string]string{
		"go.mod": "module nanogo.example/foreignforms\n\ngo 1.27\n",
		"lib/lib.go": `package lib

type Tick struct{ N int }

func (t *Tick) Bump() int { t.N++; return t.N }

// Sum is an assignment, an operation assignment and a three-clause loop.
func Sum[T ~int](vs []T) T {
	total := T(0)
	for i := 0; i < len(vs); i++ {
		total += vs[i]
	}
	return total
}

// Reverse is the parallel assignment, and a loop whose post statement assigns
// two variables at once.
func Reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// Counted is a var declaration with no initialiser, ++, break and continue.
func Counted[T ~int](s []T, skip T) int {
	var n int
	for _, v := range s {
		if v == skip {
			continue
		}
		if v < 0 {
			break
		}
		n++
	}
	return n
}

// Classify is an expression switch with a multi-value case and a default,
// inside a loop, so that the break in one clause and the continue in another
// have two plausible targets each: break ends the switch and reaches the
// statement after it, and continue reaches the loop's post statement.
func Classify[T ~int](v T) int {
	n := 0
	for i := 0; i < 3; i++ {
		switch v {
		case 0:
			n += 10
			break
		case 1, 2:
			n += 20
			continue
		default:
			n += 30
		}
		n++
	}
	return n
}

func Two[T ~int](v T) (T, T) { return v, v + 1 }

// Pair assigns two results at once, swaps them, and throws one away.
func Pair[T ~int](v T) T {
	a, b := Two(v)
	a, b = b, a
	_ = b
	return a - b
}

// Lengths is len and cap.
func Lengths[T any](s []T) int { return len(s) + cap(s) }

// Bumped calls a method of a concrete type twice, so the two calls have to see
// one another's write.
func Bumped[T ~int](v T) int {
	var t Tick
	t.N = int(v)
	return t.Bump() + t.Bump()
}

// ShiftAssign assigns through a shift, a bitwise or, and a decrement.
func ShiftAssign[T ~int](v T) T {
	v <<= 2
	v |= 1
	v--
	return v
}

// Capture is a function literal that writes a variable of the body around it.
// The variable is read after the literal has run, so a capture by value is a
// different answer and not a link failure.
func Capture[T ~int](v T) T {
	acc := v
	add := func(d T) { acc += d }
	add(v)
	add(1)
	return acc
}

// Nested is a literal inside a literal, whose capture reaches through two
// levels: the outer literal captures a variable it does not read itself.
func Nested[T ~int](v T) T {
	acc := v
	outer := func() func() {
		return func() { acc += 2 }
	}
	outer()()
	return acc
}
`,
		"main.go": `package main

import "nanogo.example/foreignforms/lib"

type pair struct {
	n    int
	name string
}

func main() {
	println(lib.Sum([]int{1, 2, 3, 4}))
	println(lib.Counted([]int{1, 2, 3, 2, 5}, 2), lib.Counted([]int{1, -2, 3}, 9))
	println(lib.Classify(0), lib.Classify(1), lib.Classify(2), lib.Classify(7))
	println(lib.Pair(4))
	println(lib.Lengths([]int{1, 2, 3}))
	println(lib.Bumped(5))
	println(lib.ShiftAssign(3))
	println(lib.Capture(6))
	println(lib.Nested(6))

	// One word, two words, and a struct with a pointer in it, so a body
	// stencilled at the wrong type moves the wrong number of words.
	ns := []int{1, 2, 3, 4, 5}
	lib.Reverse(ns)
	for i := 0; i < len(ns); i++ {
		println(i, ns[i])
	}
	ss := []string{"a", "b", "c", "d"}
	lib.Reverse(ss)
	for i := 0; i < len(ss); i++ {
		println(i, ss[i])
	}
	ps := []pair{{1, "a"}, {2, "b"}, {3, "c"}}
	lib.Reverse(ps)
	for i := 0; i < len(ps); i++ {
		println(i, ps[i].n, ps[i].name)
	}
}
`,
	}
}

// TestToolexecRunsEachConstructTheForeignWalkMaps runs one instantiation per
// construct and compares every answer with gc's.
//
// The standard library is not enough on its own: every generic in it that the
// walk accepts reaches the same few nodes, so whole branches of the mapping
// would be built and never run.
func TestToolexecRunsEachConstructTheForeignWalkMaps(t *testing.T) {
	h := setup(t, foreignFormsModule(), []string{"main"})

	if out, err := h.build(t, "-o", "foreignforms", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "foreignforms"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// The module whose library is a generic slice expression at every operand
// class and every bound form.
//
// Only main is on the allowlist, so gc compiles lib and nanogo reads each body
// below out of the archive gc wrote. That is what makes this the end-to-end
// check on ir/foreign.go's slice expression: no syntax tree of any of these
// bodies exists in the build that stencils them.
//
// The element types are one word, two words and three words with a pointer in
// the middle, because a bound the lowering scaled by the wrong element size
// points at the wrong element rather than off the object, and prints a wrong
// answer rather than crashing.
//
// The array class is the one that has to take an address, so ArrWrite and
// PtrArrWrite write through the result. A build that sliced a copy of the
// array returns the element the caller passed in and not the one it wrote,
// which no length or capacity would show.
func foreignSliceModule() map[string]string {
	return map[string]string{
		"go.mod": "module nanogo.example/foreignslice\n\ngo 1.27\n",
		"lib/lib.go": `package lib

func All[T any](s []T) []T  { return s[:] }
func Lo[T any](s []T) []T   { return s[2:] }
func Hi[T any](s []T) []T   { return s[:2] }
func LoHi[T any](s []T) []T { return s[1:3] }
func Max[T any](s []T) []T  { return s[1:2:4] }

// Var takes its bounds from parameters, so every bounds check the lowering
// emits is reached with a value rather than with a number it can fold.
func Var[T any](s []T, lo, hi, max int) []T { return s[lo:hi:max] }

func StrAll[T ~string](s T) T  { return s[:] }
func StrLo[T ~string](s T) T   { return s[2:] }
func StrHi[T ~string](s T) T   { return s[:2] }
func StrLoHi[T ~string](s T) T { return s[1:3] }

func ArrAll[T any](a [4]T) []T { return a[:] }
func ArrMax[T any](a [4]T) []T { return a[1:2:4] }

// ArrWrite writes through the slice of its array parameter and reads the
// array back, which the copy a wrong build would slice does not show.
func ArrWrite[T any](a [4]T, v T) T {
	s := a[1:]
	s[0] = v
	return a[1]
}

func PtrArrAll[T any](p *[4]T) []T { return p[:] }
func PtrArrMax[T any](p *[4]T) []T { return p[1:2:4] }

// PtrArrWrite writes into the caller's array, so the caller reads the write.
func PtrArrWrite[T any](p *[4]T, v T) {
	s := p[1:]
	s[0] = v
}

// Rec holds an array field, so Field takes the address of a field through a
// pointer rather than the address of a variable.
type Rec[T any] struct{ A [4]T }

func Field[T any](r *Rec[T]) []T { return r.A[1:] }

// Local slices an array the body itself declares, which is the array that has
// no address until the walk takes one.
func Local[T any](v T) []T {
	var a [4]T
	a[3] = v
	return a[1:4:4]
}
`,
		"main.go": `package main

import "nanogo.example/foreignslice/lib"

type trio struct {
	n    int
	name string
	m    int
}

func show(s []int) { println(len(s), cap(s), s[0]) }

func main() {
	ints := []int{10, 20, 30, 40}
	show(lib.All(ints))
	show(lib.Lo(ints))
	show(lib.Hi(ints))
	show(lib.LoHi(ints))
	show(lib.Max(ints))
	show(lib.Var(ints, 1, 3, 4))

	// A two-word element, so a bound scaled by the wrong size reads half of
	// one string and half of the next.
	strs := []string{"a", "bb", "ccc", "dddd"}
	ss := lib.LoHi(strs)
	println(len(ss), cap(ss), ss[0], ss[1])

	// A three-word element with a pointer in the middle.
	trios := []trio{{1, "one", 2}, {3, "three", 4}, {5, "five", 6}, {7, "seven", 8}}
	ts := lib.Max(trios)
	println(len(ts), cap(ts), ts[0].n, ts[0].name, ts[0].m)

	println(lib.StrAll("hello"), lib.StrLo("hello"), lib.StrHi("hello"), lib.StrLoHi("hello"))

	arr := [4]int{1, 2, 3, 4}
	println(lib.ArrWrite(arr, 9), arr[1])
	show(lib.ArrAll(arr))
	show(lib.ArrMax(arr))

	show(lib.PtrArrAll(&arr))
	show(lib.PtrArrMax(&arr))
	lib.PtrArrWrite(&arr, 7)
	println(arr[0], arr[1], arr[2])

	r := lib.Rec[int]{A: [4]int{5, 6, 7, 8}}
	show(lib.Field(&r))

	// The array the body itself declares. It has no address until the walk
	// takes one, and it escapes because the slice outlives the call, so the
	// element the caller reads is the evidence the base pointer is right.
	sl := lib.Local(11)
	println(len(sl), cap(sl), sl[0], sl[2])
}
`,
	}
}

// TestToolexecRunsAForeignGenericThatSlices is the evidence for ir/foreign.go's
// slice expression.
//
// A stencil compiles whatever it computes, so the claim is agreement with the
// program gc builds from the same source, and it is answered by running both.
// The lengths, the capacities and the elements are all printed, because the
// three parts of the lowering fail apart: a wrong length is a wrong len, a
// wrong capacity is a wrong cap, and a wrong base pointer is a wrong element.
func TestToolexecRunsAForeignGenericThatSlices(t *testing.T) {
	h := setup(t, foreignSliceModule(), []string{"# gc owns lib, nanogo owns main", "main"})

	if out, err := h.build(t, "-o", "foreignslice", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "foreignslice"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// The module whose library holds the zero value, the composite literal, new,
// and go and defer, which are the four codes the walk mapped last.
//
// Only main is on the allowlist, so gc compiles lib and nanogo reads each body
// below out of the archive gc wrote. No syntax tree of any of them exists in
// the build that stencils them.
//
// The element types are one word, two words and three words with a pointer in
// the middle, because a literal whose elements were placed by position where
// the format gave them an index, or a zero whose width came from the wrong
// type, writes the wrong bytes rather than crashing, and prints a wrong answer
// rather than failing to link.
func foreignLatestModule() map[string]string {
	return map[string]string{
		"go.mod": "module nanogo.example/foreignlatest\n\ngo 1.27\n",
		"lib/lib.go": `package lib

// The zero value, once per nil-shaped class. The widths differ, so an answer
// that came from the wrong type writes part of a value.

func NilPtr[T any]() *T { return nil }

func NilSlice[T any]() []T { return nil }

func NilMap[T comparable]() map[T]int { return nil }

func NilFunc[T any]() func(T) T { return nil }

func NilIface[T any]() any { return nil }

// NilCompare reads the node in an operand position, where the type it takes is
// the type of the value it is compared against.
func NilCompare[T any](s []T) int {
	if s == nil {
		return 1
	}
	return 0
}

// Holder gives a nil a field to be stored in, so the node reaches an element
// of a composite literal and its width is read back through the struct.
type Holder[T any] struct {
	P *T
	S []T
	N int
}

func NilHolder[T any](n int) Holder[T] { return Holder[T]{P: nil, S: nil, N: n} }

// The composite literal, at each of the three element encodings and at each
// form inside them.

type Rec[T any] struct {
	A T
	B int
}

func LitKeyed[T any](v T) Rec[T] { return Rec[T]{A: v, B: 2} }

func LitPositional[T any](v T) Rec[T] { return Rec[T]{v, 3} }

// LitPartial leaves B out, so the walk writes B's zero value out rather than
// handing the lowering a short element list.
func LitPartial[T any](v T) Rec[T] { return Rec[T]{A: v} }

// LitEmpty names no field at all, which the lowering clears rather than
// writes.
func LitEmpty[T any]() Rec[T] { return Rec[T]{} }

func LitSlice[T any](a, b T) []T { return []T{a, b} }

// LitSliceKeyed gives one element an index, and the element after it takes the
// index after that one. A walk that renumbered the elements by position would
// write both at the front.
func LitSliceKeyed[T any](a, b T) []T { return []T{2: a, b} }

func LitArray[T any](a, b T) [4]T { return [4]T{a, b} }

func LitArrayKeyed[T any](v T) [4]T { return [4]T{3: v} }

func LitMap[T comparable](k T, v int) map[T]int { return map[T]int{k: v} }

// LitNested elides the type of the inner literals, which the format writes out
// whether or not the source did.
func LitNested[T any](v T) [][]T { return [][]T{{v}, {v, v}} }

// LitPtrElem elides the pointer of an element of a slice of pointers, which is
// the one literal whose value is the address of another.
func LitPtrElem[T any](v T) []*Rec[T] { return []*Rec[T]{{A: v, B: 1}} }

// LitAddr writes the address, which is a unary operator around a literal
// rather than the pointer form of one.
func LitAddr[T any](v T) *Rec[T] { return &Rec[T]{A: v, B: 5} }

// new. The value is read back through the pointer, because dropping the
// operand of new(x) compiles it into new(T) and returns a pointer to a zero,
// which no allocation or link check would show.

func NewT[T any]() *T { return new(T) }

func NewVal[T any](v T) *T { return new(v) }

func NewRec[T any](v T) *Rec[T] { return new(Rec[T]{A: v, B: 4}) }

// go and defer. Each operand is changed after the statement, so the answer
// says whether the call read the value at the statement or at the call.

// Store is the callee named by symbol, which is the one callee that needs no
// temporary because nothing can reassign it.
func Store(p *int, v int) { *p = v }

func DeferStore[T ~int](p *int, v, w T) {
	defer Store(p, int(v))
	v = w
	_ = v
}

// DeferValue defers a call of a function value, which is the callee that has
// to go into a temporary.
func DeferValue[T any](f func(T), v, w T) {
	defer f(v)
	v = w
	_ = v
}

// DeferNoArgs defers a call with no operands, which needs no wrapper literal
// and gets none.
func DeferNoArgs[T ~int](f func(), v T) T {
	defer f()
	return v + 1
}

// DeferOrder defers three calls, so the order they run in is read and not
// assumed: the specification runs them last in, first out.
func DeferOrder[T any](f func(T), a, b, c T) {
	defer f(a)
	defer f(b)
	defer f(c)
}

// Spawned starts a call on a new goroutine, which reads its operand at the
// statement the way a defer does.
func Spawned[T any](f func(T), v, w T) {
	go f(v)
	v = w
	_ = v
}
`,
		"main.go": `package main

import "nanogo.example/foreignlatest/lib"

type trio struct {
	n    int
	name string
	m    int
}

func main() {
	println(lib.NilPtr[int]() == nil, lib.NilPtr[trio]() == nil)
	println(lib.NilSlice[string]() == nil, len(lib.NilSlice[trio]()))
	println(lib.NilMap[string]() == nil, len(lib.NilMap[int]()))
	println(lib.NilFunc[int]() == nil, lib.NilIface[trio]() == nil)
	println(lib.NilCompare([]int(nil)), lib.NilCompare([]int{1}))

	// The nil inside a struct, read back through the fields around it. A zero
	// of the wrong width writes over N.
	h := lib.NilHolder[trio](11)
	println(h.P == nil, h.S == nil, len(h.S), h.N)

	// The struct literal, at one word and at three with a pointer in the
	// middle. LitPartial leaves B out and LitEmpty leaves everything out.
	println(lib.LitKeyed(7).A, lib.LitKeyed(7).B)
	println(lib.LitPositional("x").A, lib.LitPositional("x").B)
	tk := lib.LitKeyed(trio{1, "one", 2})
	println(tk.A.n, tk.A.name, tk.A.m, tk.B)
	tp := lib.LitPartial(trio{3, "three", 4})
	println(tp.A.n, tp.A.name, tp.A.m, tp.B)
	te := lib.LitEmpty[trio]()
	println(te.A.n, te.A.name == "", te.A.m, te.B)
	ts := lib.LitEmpty[string]()
	println(ts.A == "", ts.B)

	// The element list of an array and a slice, where an element carries its
	// index and not its position.
	ss := lib.LitSlice("a", "bb")
	println(len(ss), cap(ss), ss[0], ss[1])
	sk := lib.LitSliceKeyed(trio{5, "five", 6}, trio{7, "seven", 8})
	println(len(sk), sk[0].n, sk[2].name, sk[3].name)
	ar := lib.LitArray("a", "bb")
	println(len(ar), ar[0], ar[1], ar[3] == "")
	ak := lib.LitArrayKeyed(trio{9, "nine", 10})
	println(ak[0].n, ak[0].name == "", ak[3].n, ak[3].name)

	m := lib.LitMap("k", 5)
	println(len(m), m["k"], m["absent"])

	nested := lib.LitNested(trio{1, "one", 2})
	println(len(nested), len(nested[0]), len(nested[1]), nested[1][1].name)

	pe := lib.LitPtrElem("v")
	println(len(pe), pe[0].A, pe[0].B)
	pa := lib.LitAddr(trio{3, "three", 4})
	println(pa.A.name, pa.B)

	// new, read back through the pointer.
	println(*lib.NewT[int](), *lib.NewT[string]() == "")
	nt := lib.NewT[trio]()
	println(nt.n, nt.name == "", nt.m)
	println(*lib.NewVal(41), *lib.NewVal("val"))
	nv := lib.NewVal(trio{5, "five", 6})
	println(nv.n, nv.name, nv.m)
	nr := lib.NewRec("r")
	println(nr.A, nr.B)

	// defer, with the operand changed after the statement.
	var stored int
	lib.DeferStore(&stored, 4, 99)
	println(stored)
	var byValue string
	lib.DeferValue(func(s string) { byValue = s }, "read", "later")
	println(byValue)
	ran := 0
	println(lib.DeferNoArgs(func() { ran++ }, 6), ran)
	order := ""
	lib.DeferOrder(func(s string) { order += s }, "a", "b", "c")
	println(order)

	// go, joined through a channel so that the answer is read and not raced.
	//
	// The gate is what makes the answer a fact rather than a coin toss. The
	// goroutine reads its operand and Spawned assigns to that variable after
	// the statement, so a build that gave the call the variable's cell rather
	// than a temporary would print the later value only when the goroutine
	// happened to run second. Holding the goroutine until Spawned has returned
	// makes it run second every time.
	gate := make(chan int)
	done := make(chan trio)
	lib.Spawned(func(v trio) { <-gate; done <- v }, trio{1, "one", 2}, trio{9, "nine", 9})
	gate <- 0
	got := <-done
	println(got.n, got.name, got.m)
}
`,
	}
}

// TestToolexecRunsAForeignGenericAtTheLastFourCodes is the evidence for the
// zero value, the composite literal, new, and go and defer.
//
// A stencil compiles whatever it computes, so the claim is agreement with the
// program gc builds from the same source, and it is answered by running both.
// The values are printed rather than counted, because each of the four fails
// as a wrong value and not as a wrong shape: a zero of the wrong width, an
// element at the wrong index, a pointer to a zero where the source asked for a
// pointer to a value, and a call that read its operand a statement too late.
func TestToolexecRunsAForeignGenericAtTheLastFourCodes(t *testing.T) {
	h := setup(t, foreignLatestModule(), []string{"# gc owns lib, nanogo owns main", "main"})

	if out, err := h.build(t, "-o", "foreignlatest", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "foreignlatest"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
