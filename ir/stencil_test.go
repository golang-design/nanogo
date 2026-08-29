// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/types2"
)

// stencilSyms returns the symbols of the instantiations in p, which are the
// functions whose symbol holds a type argument list.
func stencilSyms(p *Package) []string {
	var out []string
	for _, fn := range p.Funcs {
		if strings.Contains(fn.Sym, "[") {
			out = append(out, fn.Sym)
		}
	}
	return out
}

// stencilRefusal builds src and returns the error, failing when there is none.
func stencilRefusal(t *testing.T, src string) string {
	t.Helper()
	pkg, files, info := buildTypecheck(t, "package p\n\nfunc sink(...any) {}\n"+src)
	out, err := Build(pkg, files, info)
	if err == nil {
		t.Fatalf("the build was accepted and produced %v", stencilSyms(out))
	}
	return err.Error()
}

// TestStencilNamesAnInstantiationTheWayGcCannot pins the symbol.
//
// The name has to be canonical, so that two packages instantiating one generic
// name one symbol, and it has to be a name gc does not write, because both
// compilers put functions into one binary. gc stencils by GC shape and writes
// main.pick[go.shape.int] for the body and main..dict.pick[int] for the
// dictionary, so a full stencil named main.pick[int] collides with neither.
// instanceSym's comment records the go tool nm output that says so.
func TestStencilNamesAnInstantiationTheWayGcCannot(t *testing.T) {
	p := buildSource(t, "func pick[T any](a, b T, first bool) T {\n"+
		"\tif first {\n\t\treturn a\n\t}\n\treturn b\n}\n\n"+
		"func mk[K comparable, V any](k K, v V) map[K]V { m := make(map[K]V); m[k] = v; return m }\n\n"+
		"func user() {\n"+
		"\tsink(pick(7, 1, true))\n"+
		"\tsink(pick(\"x\", \"y\", false))\n"+
		"\tsink(pick(T{}, T{}, true))\n"+
		"\tsink(mk(\"a\", 1))\n"+
		"}\n")

	want := []string{"p.pick[int]", "p.pick[string]", "p.pick[p.T]", "p.mk[string,int]"}
	got := stencilSyms(p)
	if len(got) != len(want) {
		t.Fatalf("the instantiations are %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("instantiation %d is %s, want %s", i, got[i], want[i])
		}
	}

	// The name a traceback prints is the source name with the arguments, and
	// it is not the symbol: the symbol carries the import path and this does
	// not, exactly as they differ for an ordinary declaration.
	if fn := buildFuncOf(t, p, "pick[int]"); fn.Sym != "p.pick[int]" {
		t.Errorf("pick[int] has the symbol %s", fn.Sym)
	}

	for _, sym := range got {
		if strings.Contains(sym, "go.shape.") || strings.Contains(sym, "..dict.") {
			t.Errorf("%s is spelled the way gc spells one of its own symbols", sym)
		}
	}
}

// TestStencilBuildsOneBodyPerTypeArgumentList is the deduplication rule:
// identity is the type argument list under the checker's type identity, so two
// call sites at one list are one body and two lists are two.
func TestStencilBuildsOneBodyPerTypeArgumentList(t *testing.T) {
	p := buildSource(t, "func id[T any](x T) T { return x }\n\n"+
		"type Alias = int\n\n"+
		"func user() {\n"+
		"\tsink(id(1))\n"+
		"\tsink(id[int](2))\n"+
		"\tsink(id[Alias](3))\n"+
		"\tsink(id(\"s\"))\n"+
		"}\n")
	if got, want := stencilSyms(p), []string{"p.id[int]", "p.id[string]"}; len(got) != len(want) ||
		got[0] != want[0] || got[1] != want[1] {
		t.Errorf("the instantiations are %v, want %v", got, want)
	}
}

// TestStencilGivesEachInstantiationItsOwnObjects is why an IR object is keyed
// by the instantiation as well as by the checker object.
//
// The checker records one *types2.Var for the parameter of id, whichever
// instantiation is being built, and one IR object cannot be an int in one body
// and a string in the other.
func TestStencilGivesEachInstantiationItsOwnObjects(t *testing.T) {
	p := buildSource(t, "func id[T any](x T) T { var y T; y = x; return y }\n\n"+
		"func user() { sink(id(1)); sink(id(\"s\")) }\n")

	for _, tt := range []struct{ name, param, local string }{
		{"id[int]", "int", "int"},
		{"id[string]", "string", "string"},
	} {
		fn := buildFuncOf(t, p, tt.name)
		if len(fn.Params) != 1 || fn.Params[0].Type.String() != tt.param {
			t.Fatalf("%s takes %v", tt.name, fn.Params)
		}
		if len(fn.Locals) != 1 || fn.Locals[0].Type.String() != tt.local {
			t.Fatalf("%s declares %v, want one %s", tt.name, fn.Locals, tt.local)
		}
	}

	a, b := buildFuncOf(t, p, "id[int]"), buildFuncOf(t, p, "id[string]")
	if a.Params[0] == b.Params[0] {
		t.Error("the two instantiations share one parameter object")
	}
	if a.Locals[0] == b.Locals[0] {
		t.Error("the two instantiations share one local object")
	}
}

// TestStencilFollowsCallsOutOfAnInstantiatedBody covers the worklist: an
// instantiation discovered inside an instantiated body is built too, and the
// type argument is the enclosing instantiation's and not the type parameter.
func TestStencilFollowsCallsOutOfAnInstantiatedBody(t *testing.T) {
	p := buildSource(t, "func outer[T any](x T) T { return middle(x) }\n\n"+
		"func middle[T any](x T) T { return inner(x) }\n\n"+
		"func inner[T any](x T) T { return x }\n\n"+
		"func user() { sink(outer(1)); sink(outer(\"s\")) }\n")

	want := []string{
		"p.outer[int]", "p.outer[string]",
		"p.middle[int]", "p.middle[string]",
		"p.inner[int]", "p.inner[string]",
	}
	got := stencilSyms(p)
	if len(got) != len(want) {
		t.Fatalf("the instantiations are %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("instantiation %d is %s, want %s", i, got[i], want[i])
		}
	}
}

// TestStencilTerminatesOnARecursiveInstantiation states the property
// specs/013-generics.md's first argument rests on: the set is finite because
// types2's mono check rejects a package whose instantiation graph grows a
// type, so the worklist needs no depth bound of its own. A generic that calls
// itself at its own type argument is one instantiation and not a loop.
func TestStencilTerminatesOnARecursiveInstantiation(t *testing.T) {
	p := buildSource(t, "func down[T any](x T, n int) T {\n"+
		"\tif n == 0 {\n\t\treturn x\n\t}\n\treturn down(x, n-1)\n}\n\n"+
		"func user() { sink(down(1, 3)) }\n")
	if got := stencilSyms(p); len(got) != 1 || got[0] != "p.down[int]" {
		t.Errorf("the instantiations are %v, want [p.down[int]]", got)
	}
}

// TestStencilCallsTheConcreteMethodOfAConstraint is a wrong answer this pass
// found and fixed.
//
// x.M() on a value whose type is a type parameter resolves, in the checker's
// record, to the method of the *constraint*. Building that selection unchanged
// emits a direct call to p.St.M, which is an interface's method and which
// nothing defines: the program would not link, and if a type of that name did
// exist it would call the wrong function. The selection is looked up again
// against the substituted receiver, so each instantiation calls its own
// concrete method.
func TestStencilCallsTheConcreteMethodOfAConstraint(t *testing.T) {
	p := buildSource(t, "type St interface{ M() int }\n\n"+
		"type A struct{}\n\nfunc (A) M() int { return 1 }\n\n"+
		"type B struct{}\n\nfunc (B) M() int { return 2 }\n\n"+
		"func callM[T St](x T) int { return x.M() }\n\n"+
		"func user() { sink(callM(A{})); sink(callM(B{})) }\n")

	for _, tt := range []struct{ name, callee string }{
		{"callM[p.A]", "p.A.M"},
		{"callM[p.B]", "p.B.M"},
	} {
		fn := buildFuncOf(t, p, tt.name)
		call := buildFirst(t, fn, OCall)
		if call.X == nil || call.X.Op != OGlobal || call.X.Obj == nil {
			t.Fatalf("%s does not call a known symbol:\n%s", tt.name, buildDump(fn))
		}
		if got := call.X.Obj.Name; got != tt.callee {
			t.Errorf("%s calls %s, want %s", tt.name, got, tt.callee)
		}
	}
}

// TestStencilCallsThroughAnInterfaceTypeArgument is the other half of the
// lookup above: a type parameter bound to an interface type keeps the
// interface call, and the method index is the one the interface has rather
// than the one the constraint had.
func TestStencilCallsThroughAnInterfaceTypeArgument(t *testing.T) {
	p := buildSource(t, "type One interface{ M() int }\n\n"+
		"type Two interface{ A() int; M() int }\n\n"+
		"func callM[T One](x T) int { return x.M() }\n\n"+
		"func user(v Two) { sink(callM(v)) }\n")

	fn := buildFuncOf(t, p, "callM[p.Two]")
	call := buildFirst(t, fn, OCall)
	if call.X == nil || call.X.Op != OField {
		t.Fatalf("the call does not go through an itab:\n%s", buildDump(fn))
	}
	// A and M sorted, so M is the second method of Two and the first of One.
	if call.X.Index != 1 {
		t.Errorf("the call reads method %d of the itab, want 1", call.X.Index)
	}
}

// TestStencilBuildsAClosureInsideAnInstantiation covers a function literal in
// an instantiated body: it is a function of the package like any other, and
// its name is derived from the instantiation's so that two instantiations do
// not name one literal.
func TestStencilBuildsAClosureInsideAnInstantiation(t *testing.T) {
	p := buildSource(t, "func hold[T any](x T) func() T { return func() T { return x } }\n\n"+
		"func user() { sink(hold(1)()); sink(hold(\"s\")()) }\n")
	for _, want := range []string{"p.hold[int].func1", "p.hold[string].func1"} {
		found := false
		for _, fn := range p.Funcs {
			if fn.Sym == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no function %s in %v", want, stencilSyms(p))
		}
	}
}

// TestStencilSubstitutesThroughEveryTypeAnOperationReads is the property that
// makes the walk correct without a case of its own: every type the builder
// reads comes through irType or typeOf, and both substitute, so an operation
// on a value of type parameter type sees the concrete type.
func TestStencilSubstitutesThroughEveryTypeAnOperationReads(t *testing.T) {
	p := buildSource(t, "func work[T any](n int) []T {\n"+
		"\ts := make([]T, n)\n"+
		"\tp := new(T)\n"+
		"\tvar m map[string]T\n"+
		"\tfor i := range s {\n\t\ts[i] = *p\n\t}\n"+
		"\tsink(m)\n"+
		"\treturn s\n}\n\n"+
		"func user() { sink(work[int](1)); sink(work[string](1)) }\n")

	for _, tt := range []struct{ name, elem string }{
		{"work[int]", "int"},
		{"work[string]", "string"},
	} {
		fn := buildFuncOf(t, p, tt.name)
		if len(fn.Results) != 1 {
			t.Fatalf("%s has %d results", tt.name, len(fn.Results))
		}
		got := fn.Results[0].Type
		if got.Kind != Slice || got.Elem == nil || got.Elem.String() != tt.elem {
			t.Errorf("%s returns %v, want []%s", tt.name, got, tt.elem)
		}
	}
}

// TestStencilRefusesWhatItDoesNotBuild names the line.
//
// Each of these produces no body, so a build that accepted it would emit a
// call to a symbol nothing defines. specs/013-generics.md says where each line
// is and why it is there.
func TestStencilRefusesWhatItDoesNotBuild(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  string
		want string
	}{
		{
			"a type declared inside a generic body",
			"func z[T any](x T) int { type S []T; return len(S{x}) }\n\n" +
				"func user() { sink(z(1)); sink(z(\"s\")) }\n",
			"a type declared inside a generic function is not instantiated",
		},
		{
			"a struct type declared inside a generic body",
			"func z[T any](x T) int { type S struct{ v T }; _ = S{x}; return 1 }\n\n" +
				"func user() { sink(z(1)) }\n",
			"a type declared inside a generic function is not instantiated",
		},
		{
			"a method with type parameters of its own",
			"type H struct{}\n\nfunc (H) Do[T any](x T) T { return x }\n\n" +
				"func user() { sink(H{}.Do(1)) }\n",
			"is a generic method",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := stencilRefusal(t, tt.src); !strings.Contains(got, tt.want) {
				t.Errorf("the refusal is %q, want it to hold %q", got, tt.want)
			}
		})
	}
}

// TestStencilBuildsTheMethodsOfAnInstantiatedType is specs/013-generics.md's
// generic type, in the package that declares it.
//
// The type and not the call site is the unit of discovery, because a method of
// an instantiation is reached by more than a call: an itab holds it and a
// descriptor names it in the Method array reflect indexes. So the method set
// of every instantiation the package names is built whole, which is what
// Set below is here to show: nothing calls it.
//
// The symbols are gc's own. gc writes a method of an instantiated type as
// xpkg/lib.(*Box[int]).Get, with the arguments inside the parentheses and each
// written by the link string's rule.
func TestStencilBuildsTheMethodsOfAnInstantiatedType(t *testing.T) {
	p := buildSource(t, "type Box[T any] struct{ v T }\n\n"+
		"func (b *Box[T]) Get() T  { return b.v }\n"+
		"func (b *Box[T]) Set(x T) { b.v = x }\n"+
		"func (b Box[T]) Copy() T  { return b.v }\n\n"+
		"type Pair[K comparable, V any] struct {\n\tk K\n\tv V\n}\n\n"+
		"func (q Pair[K, V]) Key() K { return q.k }\n\n"+
		"func user() {\n"+
		"\tsink((&Box[int]{1}).Get())\n"+
		"\tsink((&Box[string]{\"s\"}).Get())\n"+
		"\tsink(Pair[string, int]{\"k\", 1}.Key())\n"+
		"}\n")
	want := []string{
		"p.(*Box[int]).Get",
		"p.(*Box[int]).Set",
		"p.Box[int].Copy",
		"p.(*Box[string]).Get",
		"p.(*Box[string]).Set",
		"p.Box[string].Copy",
		"p.Pair[string,int].Key",
	}
	got := stencilSyms(p)
	if len(got) != len(want) {
		t.Fatalf("the instantiations are %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("instantiation %d is %q, want %q", i, got[i], want[i])
		}
	}

	// Each body is the instantiated one, so the two Get methods return two
	// different types and no object is shared between them.
	// The name a diagnostic and a traceback use carries the arguments on the
	// receiver, where the language writes them.
	for _, tt := range []struct{ name, res string }{
		{"Box[int].Get", "int"},
		{"Box[string].Get", "string"},
	} {
		fn := buildFuncOf(t, p, tt.name)
		if fn.Recv == nil {
			t.Fatalf("%s has no receiver", tt.name)
		}
		if len(fn.Results) != 1 || fn.Results[0].Type.String() != tt.res {
			t.Errorf("%s returns %v, want one %s", tt.name, fn.Results, tt.res)
		}
	}
	if a, b := buildFuncOf(t, p, "Box[int].Get"), buildFuncOf(t, p, "Box[string].Get"); a.Recv == b.Recv {
		t.Error("the two instantiations share one receiver object")
	}
}

// TestStencilBuildsAMethodOfAnInstantiationNothingCalls is the reason
// discovery is at the type.
//
// A descriptor's Method array names every method of the type and an itab holds
// the ones the interface asks for, and neither is a call site the builder
// walks. A value of the instantiation existing anywhere is what makes the
// whole method set owed.
func TestStencilBuildsAMethodOfAnInstantiationNothingCalls(t *testing.T) {
	p := buildSource(t, "type Box[T any] struct{ v T }\n\n"+
		"func (b Box[T]) Get() T { return b.v }\n\n"+
		"func user() { sink(Box[int]{1}) }\n")
	if got, want := stencilSyms(p), []string{"p.Box[int].Get"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("the instantiations are %v, want %v", got, want)
	}
}

// TestStencilFollowsAnInstantiationOutOfAMethodBody is the worklist across the
// two queues: an instantiation reached through another one is built too, and
// its own methods are owed with it.
//
// The order is the order the converter finished the types in, which is the
// order the type graph unwinds: Cell[int] is a field of Outer[int], so it is
// converted first and its methods are queued first. It is an order and not a
// preference, and specs/053-determinism.md asks only that it be the same on
// every build.
func TestStencilFollowsAnInstantiationOutOfAMethodBody(t *testing.T) {
	p := buildSource(t, "type Cell[T any] struct{ v T }\n\n"+
		"func (c Cell[T]) Get() T { return c.v }\n\n"+
		"type Outer[T any] struct{ c Cell[T] }\n\n"+
		"func (o Outer[T]) Get() T { return o.c.Get() }\n\n"+
		"func user() { sink(Outer[int]{}.Get()) }\n")
	got := stencilSyms(p)
	want := []string{"p.Cell[int].Get", "p.Outer[int].Get"}
	if len(got) != len(want) {
		t.Fatalf("the instantiations are %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("instantiation %d is %q, want %q", i, got[i], want[i])
		}
	}
}

// TestStencilRefusesAMethodOfAnotherPackagesGenericTypeWithNoArchive is what a
// build with no source of foreign bodies gets.
//
// The bodies of a generic type another package declares are in that package's
// archive, and foreign.go reads them through the reader the type checker
// imported with. A build that set none, which is every unit test in this
// package, has nowhere to read them from, and the refusal names the method and
// says so.
func TestStencilRefusesAMethodOfAnotherPackagesGenericTypeWithNoArchive(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, "package p\n\nimport \"sync/atomic\"\n\n"+
		"func user(p *atomic.Pointer[int], v *int) { p.Store(v) }\n")
	_, err := Build(pkg, files, info)
	if err == nil {
		t.Fatal("a method of another package's generic type was accepted")
	}
	for _, want := range []string{"sync/atomic.(*Pointer[int]).", "package sync/atomic", "no source of foreign bodies"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q, want it to hold %q", err, want)
		}
	}
}

// TestStencilRefusesAGenericOfAnotherPackageWithNoArchive is the same line for
// a generic function.
func TestStencilRefusesAGenericOfAnotherPackageWithNoArchive(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, "package p\n\nimport \"slices\"\n\n"+
		"func user(s []int) { slices.Sort(s) }\n")
	_, err := Build(pkg, files, info)
	if err == nil {
		t.Fatal("an instantiation of another package's generic function was accepted")
	}
	for _, want := range []string{"slices.Sort[", "package slices", "no source of foreign bodies"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q, want it to hold %q", err, want)
		}
	}
}

// TestStencilIsDeterministic runs the build twice and compares the symbols in
// order, which specs/053-determinism.md requires: discovery appends to a slice
// in walk order and never ranges over Info.Instances.
func TestStencilIsDeterministic(t *testing.T) {
	const src = "func a[T any](x T) T { return b(x) }\n\n" +
		"func b[T any](x T) T { return x }\n\n" +
		"func user() { sink(a(1)); sink(b(\"s\")); sink(a(true)); sink(b(1)) }\n"
	first := strings.Join(stencilSyms(buildSource(t, src)), " ")
	for i := 0; i < 8; i++ {
		if got := strings.Join(stencilSyms(buildSource(t, src)), " "); got != first {
			t.Fatalf("build %d produced %q, and the first produced %q", i, got, first)
		}
	}
	// The declared bodies are walked first, so user's four calls are found
	// in source order, and the two instantiations the drained bodies discover
	// come after them in the order the drain reaches them.
	if want := "p.a[int] p.b[string] p.a[bool] p.b[int] p.b[bool]"; first != want {
		t.Errorf("the instantiations are %q, want %q", first, want)
	}
}

// TestInstanceSymSpellsTheTypeArgumentsWithTheirImportPaths covers the naming
// function on its own, on the type shapes a symbol has to stay unique across.
func TestInstanceSymSpellsTheTypeArgumentsWithTheirImportPaths(t *testing.T) {
	pkg, files, info := buildTypecheck(t, "package p\n\ntype N struct{}\n\nfunc F[T any]() {}\n")
	if _, err := Build(pkg, files, info); err != nil {
		t.Fatalf("Build: %v", err)
	}
	origin, _ := pkg.Scope().Lookup("F").(*types2.Func)
	if origin == nil {
		t.Fatal("F is not declared")
	}
	named := pkg.Scope().Lookup("N").Type()

	for _, tt := range []struct {
		targs []types2.Type
		want  string
	}{
		{[]types2.Type{types2.Typ[types2.Int]}, "p.F[int]"},
		{[]types2.Type{named}, "p.F[p.N]"},
		{[]types2.Type{types2.NewPointer(named)}, "p.F[*p.N]"},
		{[]types2.Type{types2.NewSlice(named)}, "p.F[[]p.N]"},
		{[]types2.Type{types2.Typ[types2.Int], named}, "p.F[int,p.N]"},
		{[]types2.Type{types2.NewMap(types2.Typ[types2.String], named)}, "p.F[map[string]p.N]"},
	} {
		if got := instanceSym(origin, tt.targs); got != tt.want {
			t.Errorf("instanceSym is %s, want %s", got, tt.want)
		}
	}
}

// TestStencilBuildsTheShapesASubstitutionRebuilds is the other side of the
// refusal above.
//
// The checker's substituter rebuilds a type literal and stops at a name, so
// every shape that is a literal comes through concrete. These are the ones the
// refusal must not catch. Where the shape can hold a type parameter in more
// than one place it is instantiated at two type argument lists, so that a body
// sharing a type with the other would show.
func TestStencilBuildsTheShapesASubstitutionRebuilds(t *testing.T) {
	p := buildSource(t, "type St interface{ M() int }\n\n"+
		"func conv[T ~int](x T) int { return int(x) }\n\n"+
		"func keys[K comparable, V any](m map[K]V) int {\n"+
		"\tn := 0\n\tfor k := range m {\n\t\t_ = k\n\t\tn++\n\t}\n\treturn n\n}\n\n"+
		"func held[T any](x T) T { defer func() {}(); return x }\n\n"+
		"func bound[T St](x T) func() int { return x.M }\n\n"+
		"type A struct{}\n\nfunc (A) M() int { return 1 }\n\n"+
		"func user() {\n"+
		"\tsink(conv(1))\n"+
		"\tsink(keys(map[string]int{}))\n"+
		"\tsink(keys(map[int]string{}))\n"+
		"\tsink(held(1))\n"+
		"\tsink(held(\"s\"))\n"+
		"\tsink(bound(A{})())\n"+
		"}\n")

	for _, want := range []string{
		"p.conv[int]", "p.keys[string,int]", "p.keys[int,string]",
		"p.held[int]", "p.held[string]", "p.bound[p.A]",
	} {
		buildFuncOf(t, p, want[len("p."):])
	}

	// The range variable of keys is a local whose type is the map's key type,
	// so the two instantiations must not share it.
	a, b := buildFuncOf(t, p, "keys[string,int]"), buildFuncOf(t, p, "keys[int,string]")
	if len(a.Locals) != 2 || len(b.Locals) != 2 {
		t.Fatalf("keys declares %v and %v", a.Locals, b.Locals)
	}
	if got, want := a.Locals[1].Type.String(), "string"; got != want {
		t.Errorf("the range variable of keys[string,int] is %s, want %s", got, want)
	}
	if got, want := b.Locals[1].Type.String(), "int"; got != want {
		t.Errorf("the range variable of keys[int,string] is %s, want %s", got, want)
	}
}

// TestStencilNamesAnInstantiationInATypeSwitchCase is the type node of an
// instantiation.
//
// A case of a type switch names a type, and the node that names one carries an
// object beside the type. For a defined type that object is the declaration's,
// and the declaration of an instantiation is the generic: its type is
// "trie[V any]" and substituting a generic type that carries no type arguments
// is undefined, which types2 answers with the invalid type. Reading the object
// off the declaration therefore refused every instantiation named in a case,
// with a message about the invalid type that named neither the case nor the
// declaration.
func TestStencilNamesAnInstantiationInATypeSwitchCase(t *testing.T) {
	const src = `package p

type trie[V any] map[int]any

func (t trie[V]) find(k int) int {
	v, ok := t[k]
	if !ok {
		return 0
	}
	switch v.(type) {
	case trie[V]:
		return 1
	}
	return 2
}

func user(t trie[int]) int { return t.find(1) }
`
	pkg, files, info := buildTypecheck(t, src)
	p, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("the instantiation was refused: %v", err)
	}
	var clause *Node
	for _, fn := range p.Funcs {
		for _, s := range fn.Body {
			Walk(s, func(n *Node) bool {
				if n.Op == OTypeSwitch && len(n.Body) > 0 && len(n.Body[0].Args) > 0 {
					clause = n.Body[0].Args[0]
				}
				return true
			})
		}
	}
	if clause == nil {
		t.Fatal("no instantiation of the method with a type switch was built")
	}
	// The node's type is the instantiation and not the declaration, and the
	// object beside it carries the same type.
	if clause.Type == nil || clause.Type.Kind != Map {
		t.Fatalf("the case names the type %s, want the map trie[int] is", clause.Type)
	}
	if clause.Obj == nil || clause.Obj.Type != clause.Type {
		t.Errorf("the case's object carries %v and the node carries %v", clause.Obj, clause.Type)
	}
}
