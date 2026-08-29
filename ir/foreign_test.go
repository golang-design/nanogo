// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"go/constant"
	"strings"
	"testing"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/types2"
)

// The walk over a body of another package's, measured without an archive.
//
// A body decoded out of a real archive names the objects the type checker
// holds, and these tests type-check the standard library from source rather
// than reading archives, so a decoded body would name a parallel object graph.
// What they measure instead is the walk itself: the tree it accepts, the
// numbering of the locals, and the refusal it produces for a node it has no
// case for. [export.Body] is a public tree, so the input is written here.
//
// internal/e2e carries the other half, where the body is a real one out of the
// archive gc wrote and the program is run and compared against gc's answer.

// fixedBodies is a [BodySource] that answers with one body, whatever is asked.
type fixedBodies struct {
	body   *export.Body
	pragma int
	err    error
	// asked records the declarations the stenciler looked for, in order.
	asked []string
}

func (f *fixedBodies) Body(path, name string) (*export.FuncBody, error) {
	f.asked = append(f.asked, path+"."+name)
	if f.err != nil {
		return nil, f.err
	}
	return &export.FuncBody{Path: path, Name: name, Generic: true, Pragma: f.pragma, Body: f.body}, nil
}

// withBodies sets the process-wide source for one test and restores it.
func withBodies(t *testing.T, s BodySource) {
	t.Helper()
	saved := ForeignBodies
	ForeignBodies = s
	t.Cleanup(func() { ForeignBodies = saved })
}

// sortTypeParams returns the type parameters of slices.Sort as the checker
// made them, which is the domain a body written against that declaration
// substitutes through.
func sortTypeParams(t *testing.T, pkg *types2.Package) []*types2.TypeParam {
	t.Helper()
	for _, imp := range pkg.Imports() {
		if imp.Path() != "slices" {
			continue
		}
		fn, _ := imp.Scope().Lookup("Sort").(*types2.Func)
		if fn == nil {
			t.Fatal("the checked slices package declares no Sort")
		}
		return types2.TypeParamsOf(fn.Type())
	}
	t.Fatal("the checked package does not import slices")
	return nil
}

// sortCaller is a package whose one call instantiates slices.Sort at []int.
const sortCaller = "package p\n\nimport \"slices\"\n\nfunc user(s []int) { slices.Sort(s) }\n"

// TestForeignBodyBuildsAnInstantiationOfAnotherPackagesGeneric is the join.
//
// The body is slices.Sort's shape rather than its text: one parameter, no
// result, and a bare return. What it proves is the part that has no other
// witness in this package: the instantiation is built rather than refused, the
// symbol is the canonical one, and the parameter got the concrete type the
// substitution supplied rather than the type parameter the body was written
// against.
func TestForeignBodyBuildsAnInstantiationOfAnotherPackagesGeneric(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, sortCaller)
	src := &fixedBodies{body: &export.Body{
		Params:   []export.Local{{DictRType: -1}},
		HasBlock: true,
		Stmts:    []export.Stmt{&export.ReturnStmt{}},
		Dict:     &export.Dict{TypeParams: sortTypeParams(t, pkg)},
	}}
	withBodies(t, src)

	p, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("the instantiation was refused: %v", err)
	}
	if got, want := src.asked, []string{"slices.Sort"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("the stenciler asked for %v, want %v", got, want)
	}

	fn := buildFuncOf(t, p, "Sort[[]int,int]")
	if fn.Sym != "slices.Sort[[]int,int]" {
		t.Errorf("the instantiation has the symbol %s, want slices.Sort[[]int,int]", fn.Sym)
	}
	if len(fn.Params) != 1 {
		t.Fatalf("the instantiation has %d parameters, want 1", len(fn.Params))
	}
	// The body was written against S, whose core type is ~[]E. The parameter
	// here has to be the slice of int the call site named.
	if got := fn.Params[0].Type; got.Kind != Slice || got.Elem == nil || got.Elem.Kind != Int64 {
		t.Errorf("the parameter has the type %s, want []int", got)
	}
	if len(fn.Body) != 1 || fn.Body[0].Op != OReturn {
		t.Errorf("the body is %v, want one return", fn.Body)
	}
}

// TestForeignBodyRefusesANodeItDoesNotMap is the property that makes a partial
// mapping safe.
//
// The format has about thirty statement and expression kinds and this walk
// builds a few of them. A kind with no case has to stop the build with a
// message naming it, because the alternative is a guess inside a function
// nobody here wrote: a body that compiles and computes something else.
func TestForeignBodyRefusesANodeItDoesNotMap(t *testing.T) {
	for _, tt := range []struct {
		name string
		stmt export.Stmt
		want string
	}{
		{"a channel send", &export.SendStmt{}, `the statement "send"`},
		{"a select", &export.SelectStmt{}, `the statement "select"`},
		{"an assignment", &export.AssignStmt{}, `the statement "assignment"`},
		{"a switch", &export.SwitchStmt{}, `the statement "switch"`},
		{"a deferred call", &export.CallStmt{}, `the statement "go or defer"`},
		{"a composite literal", &export.ExprStmt{X: &export.CompLitExpr{}}, `the expression "composite literal"`},
		{"a function literal", &export.ExprStmt{X: &export.FuncLitExpr{}}, `the expression "function literal"`},
		{"make", &export.ExprStmt{X: &export.MakeExpr{}}, `the expression "make"`},
		{"a type assertion", &export.ExprStmt{X: &export.AssertExpr{}}, `the expression "type assertion"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pkg, files, info := buildTypecheckWithImports(t, sortCaller)
			withBodies(t, &fixedBodies{body: &export.Body{
				Params:   []export.Local{{DictRType: -1}},
				HasBlock: true,
				Stmts:    []export.Stmt{tt.stmt},
				Dict:     &export.Dict{TypeParams: sortTypeParams(t, pkg)},
			}})
			_, err := Build(pkg, files, info)
			if err == nil {
				t.Fatal("a node the walk has no case for was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the refusal is %q, want it to hold %q", err, tt.want)
			}
			// The declaration the node was met in, because the body is in no
			// file of this build and the reader of the message has to know
			// which function to go and look at.
			if !strings.Contains(err.Error(), "slices.Sort[[]int,int]") {
				t.Errorf("the refusal is %q and does not name the instantiation", err)
			}
		})
	}
}

// TestForeignBodyRefusesABodyItCannotRead names the declaration when the
// archive is the thing that is missing.
func TestForeignBodyRefusesABodyItCannotRead(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  *fixedBodies
		want string
	}{
		{"no body in the archive", &fixedBodies{}, "the archive holds no body for it"},
		{
			"a declaration with no block",
			&fixedBodies{body: &export.Body{Dict: &export.Dict{}}},
			"the declaration has no body",
		},
		{
			"a body with no dictionary",
			&fixedBodies{body: &export.Body{HasBlock: true}},
			"the body carries no dictionary",
		},
		{
			// Several //go: directives are correctness requirements rather
			// than hints, and the format carries gc's own bit set rather than
			// this compiler's. A declaration that carries one is refused
			// rather than built as though it carried none.
			"a declaration with a //go: directive",
			&fixedBodies{
				pragma: 1,
				body: &export.Body{
					Params:   []export.Local{{DictRType: -1}},
					HasBlock: true,
					Stmts:    []export.Stmt{&export.ReturnStmt{}},
					Dict:     &export.Dict{},
				},
			},
			"carries a //go: directive",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pkg, files, info := buildTypecheckWithImports(t, sortCaller)
			withBodies(t, tt.src)
			_, err := Build(pkg, files, info)
			if err == nil {
				t.Fatal("an instantiation with no readable body was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the refusal is %q, want it to hold %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "slices.Sort[[]int,int] is declared in package slices") {
				t.Errorf("the refusal is %q and does not name the declaration", err)
			}
		})
	}
}

// TestForeignBodyRefusesADictionaryOfTheWrongWidth guards the substitution.
//
// The substitution replaces the type parameters the body was written against,
// and those are the objects the decode of the archive created rather than the
// ones the checker made. A dictionary naming a different number of them is an
// archive that does not belong to this declaration, and types2.NewSubstitution
// panics on the mismatch rather than returning an error, so it is caught here.
func TestForeignBodyRefusesADictionaryOfTheWrongWidth(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, sortCaller)
	withBodies(t, &fixedBodies{body: &export.Body{
		Params:   []export.Local{{DictRType: -1}},
		HasBlock: true,
		Stmts:    []export.Stmt{&export.ReturnStmt{}},
		Dict:     &export.Dict{},
	}})
	_, err := Build(pkg, files, info)
	if err == nil {
		t.Fatal("a body written against the wrong number of type parameters was accepted")
	}
	if !strings.Contains(err.Error(), "written against 0 type parameter(s) and the instantiation names 2") {
		t.Errorf("the refusal is %q", err)
	}
}

// TestForeignBodyRefusesABodyWhoseLocalsDoNotMatchTheSignature is the check
// that keeps a misread body from being built.
//
// A local is named by its number and the first of them are the receiver, the
// parameters and the results. A body that opens with a different count is one
// whose numbering does not line up with the signature, and every use of a
// local in it would name the wrong variable.
func TestForeignBodyRefusesABodyWhoseLocalsDoNotMatchTheSignature(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, sortCaller)
	withBodies(t, &fixedBodies{body: &export.Body{
		Params:   []export.Local{{DictRType: -1}, {DictRType: -1}, {DictRType: -1}},
		HasBlock: true,
		Stmts:    []export.Stmt{&export.ReturnStmt{}},
		Dict:     &export.Dict{TypeParams: sortTypeParams(t, pkg)},
	}})
	_, err := Build(pkg, files, info)
	if err == nil {
		t.Fatal("a body whose locals do not match its signature was accepted")
	}
	if !strings.Contains(err.Error(), "opens with 3 local(s) and its signature has 1") {
		t.Errorf("the refusal is %q", err)
	}
}

// TestForeignBodyBuildsTheConstantsAndComparisonsOfAReturn covers the two
// nodes a return of a computed value is made of.
//
// The type of a binary operation is the one place the type cannot come out of
// the stream: gc writes the reshape node that carries a type in front of an
// operand and not in front of an operation, so a comparison has to be given
// the bool the language defines for it. A void type there produced a block
// whose control value had no type, which the SSA verifier caught only after
// the whole function had been built.
func TestForeignBodyBuildsTheConstantsAndComparisonsOfAReturn(t *testing.T) {
	const src = "package p\n\nimport \"slices\"\n\nfunc user(s []int, v int) bool { return slices.Contains(s, v) }\n"
	pkg, files, info := buildTypecheckWithImports(t, src)

	var tparams []*types2.TypeParam
	for _, imp := range pkg.Imports() {
		if imp.Path() == "slices" {
			fn, _ := imp.Scope().Lookup("Contains").(*types2.Func)
			tparams = types2.TypeParamsOf(fn.Type())
		}
	}
	// return 1 == 1, over the three locals Contains opens with: s, v and the
	// unnamed bool result.
	one := &export.ConstExpr{
		Type:  export.TypeUse{Type: types2.Typ[types2.Int]},
		Value: constant.MakeInt64(1),
	}
	withBodies(t, &fixedBodies{body: &export.Body{
		Params:   []export.Local{{DictRType: -1}, {DictRType: -1}, {DictRType: -1}},
		HasBlock: true,
		Stmts: []export.Stmt{&export.ReturnStmt{Results: export.MultiExpr{
			Exprs: []export.Expr{&export.BinaryExpr{Op: export.OpEq, X: one, Y: one}},
		}}},
		Dict: &export.Dict{TypeParams: tparams},
	}})

	p, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("the instantiation was refused: %v", err)
	}
	fn := buildFuncOf(t, p, "Contains[[]int,int]")
	if len(fn.Body) != 1 || fn.Body[0].Op != OReturn || len(fn.Body[0].Args) != 1 {
		t.Fatalf("the body is %v, want one return of one value", fn.Body)
	}
	cmp := fn.Body[0].Args[0]
	if cmp.Op != OCompare {
		t.Fatalf("the returned value is %v, want a comparison", cmp.Op)
	}
	if cmp.Type == nil || cmp.Type.Kind != Bool {
		t.Errorf("the comparison has the type %s, want bool", cmp.Type)
	}
}
