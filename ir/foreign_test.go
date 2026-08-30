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
		{"a label", &export.LabelStmt{Label: "loop"}, `the statement "label"`},
		{"a deferred call", &export.CallStmt{}, `the statement "go or defer"`},
		{"a composite literal", &export.ExprStmt{X: &export.CompLitExpr{}}, `the expression "composite literal"`},
		{"make", &export.ExprStmt{X: &export.MakeExpr{}}, `the expression "make"`},
		{"a type assertion", &export.ExprStmt{X: &export.AssertExpr{}}, `the expression "type assertion"`},
		{"a zero value", &export.ExprStmt{X: &export.ZeroExpr{}}, `the expression "zero value"`},

		// The branches. Each of the three below is a jump to a label, and gc
		// writes a label flat: the statement it labels is the next entry of
		// the same list, so a branch that names one has to be paired with a
		// statement this walk refuses on its own.
		{"a goto", &export.BranchStmt{Op: export.OpGoto}, `the branch "goto"`},
		{"a fallthrough", &export.BranchStmt{Op: export.OpFall}, `the branch "fallthrough"`},
		{
			"a labelled break",
			&export.BranchStmt{Op: export.OpBreak, Labelled: true, Label: "loop"},
			"a break naming the label loop",
		},

		// The switch. An expression switch is built and the two forms below
		// are not.
		{"a type switch", &export.SwitchStmt{Guard: &export.TypeSwitchGuard{}}, "a type switch"},
		{
			"a switch clause that declares a variable",
			&export.SwitchStmt{Clauses: []export.CaseClause{{Var: &export.Local{DictRType: -1}}}},
			"a switch clause that declares a variable",
		},

		// The function literal. Its body is another element of the archive
		// and its signature says how many locals that element opens with, so a
		// literal whose element is missing or whose counts disagree is refused
		// rather than walked against the wrong numbering.
		{
			"a function literal with no body element",
			&export.ExprStmt{X: &export.FuncLitExpr{}},
			"a function literal whose body the archive holds no element for",
		},
		{
			"a function literal whose body does not match its signature",
			&export.ExprStmt{X: &export.FuncLitExpr{
				Params:  []export.Param{{Type: export.TypeUse{Type: types2.Typ[types2.Int]}}},
				Decoded: &export.Body{HasBlock: true},
			}},
			"opens with 0 local(s) and whose signature has 1",
		},
		{
			"the closure of a range over a function",
			&export.ExprStmt{X: &export.FuncLitExpr{RangeFuncBody: true, Decoded: &export.Body{}}},
			"the body of a range over a function",
		},
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

// indexCaller is a package whose one call instantiates slices.Index at []int.
//
// It is the fixture the tests below write bodies against, because its three
// locals are the three shapes a written body needs: a slice, an int parameter
// and an int result. slices.Sort's one local is a slice, and nothing can be
// added to a slice.
const indexCaller = "package p\n\nimport \"slices\"\n\nfunc user(s []int, v int) int { return slices.Index(s, v) }\n"

// foreignIndexBody builds one instantiation of slices.Index at []int from a
// body written here.
func foreignIndexBody(t *testing.T, stmts ...export.Stmt) (*Package, error) {
	t.Helper()
	pkg, files, info := buildTypecheckWithImports(t, indexCaller)
	var tparams []*types2.TypeParam
	for _, imp := range pkg.Imports() {
		if imp.Path() == "slices" {
			fn, _ := imp.Scope().Lookup("Index").(*types2.Func)
			tparams = types2.TypeParamsOf(fn.Type())
		}
	}
	withBodies(t, &fixedBodies{body: &export.Body{
		// s, v and the unnamed int result.
		Params:   []export.Local{{DictRType: -1}, {DictRType: -1}, {DictRType: -1}},
		HasBlock: true,
		Stmts:    stmts,
		Dict:     &export.Dict{TypeParams: tparams},
	}})
	return Build(pkg, files, info)
}

// intUse is a reference to int from inside a written body.
var intUse = export.TypeUse{Type: types2.Typ[types2.Int]}

// foreignConst is a constant of type int in a written body.
func foreignConst(v int64) export.Expr {
	return &export.ConstExpr{Type: intUse, Value: constant.MakeInt64(v)}
}

// foreignSliceOfDeclared is a body that declares one variable of a given type
// and then slices it.
//
// The operand of a slice expression has to have a type, and the only
// expression a written body can give one to is a use of a local: a constant
// and a call carry the type in a field this package cannot set. A declaration
// with no value on the right is "var x T", so the type is the test's to pick.
// The variable takes the number after the three the body opens with.
func foreignSliceOfDeclared(typ types2.Type, index [3]export.Expr) export.Stmt {
	return &export.BlockStmt{Body: []export.Stmt{
		&export.AssignStmt{Lhs: []export.Assignee{{
			Kind:  export.AssignDef,
			Name:  "x",
			Type:  export.TypeUse{Type: typ},
			Local: export.Local{DictRType: -1},
		}}},
		&export.AssignStmt{
			Lhs: []export.Assignee{{Kind: export.AssignBlank}},
			Rhs: export.MultiExpr{Exprs: []export.Expr{&export.SliceExpr{
				X:     foreignLocalUse(3),
				Index: index,
			}}},
		},
	}}
}

// foreignLocalUse is a use of one local of a written body.
func foreignLocalUse(i int) export.Expr { return &export.LocalExpr{Index: i} }

// foreignDef is a destination that declares a variable of type int.
func foreignDef(name string) export.Assignee {
	return export.Assignee{Kind: export.AssignDef, Name: name, Type: intUse, Local: export.Local{DictRType: -1}}
}

// foreignTo is a destination that assigns to an expression.
func foreignTo(e export.Expr) export.Assignee {
	return export.Assignee{Kind: export.AssignExpr, Expr: e}
}

// TestForeignBodyNumbersTheLocalsAnAssignmentDeclares is the numbering half of
// the assignment.
//
// A local has no name in the format and a use of one is a number, so the
// variable an assignment declares has to take the next number at the point the
// destination list was written. A walk that numbered it anywhere else reads a
// different variable, which compiles and computes something else.
func TestForeignBodyNumbersTheLocalsAnAssignmentDeclares(t *testing.T) {
	p, err := foreignIndexBody(t,
		// n := v
		&export.AssignStmt{
			Lhs: []export.Assignee{foreignDef("n")},
			Rhs: export.MultiExpr{Exprs: []export.Expr{foreignLocalUse(1)}},
		},
		// m := n + 1
		&export.AssignStmt{
			Lhs: []export.Assignee{foreignDef("m")},
			Rhs: export.MultiExpr{Exprs: []export.Expr{
				&export.BinaryExpr{Op: export.OpAdd, X: foreignLocalUse(3), Y: foreignConst(1)},
			}},
		},
		// return m
		&export.ReturnStmt{Results: export.MultiExpr{Exprs: []export.Expr{foreignLocalUse(4)}}},
	)
	if err != nil {
		t.Fatalf("the instantiation was refused: %v", err)
	}
	fn := buildFuncOf(t, p, "Index[[]int,int]")
	if len(fn.Locals) != 2 || fn.Locals[0].Name != "n" || fn.Locals[1].Name != "m" {
		t.Fatalf("the body declares %v, want n then m", fn.Locals)
	}
	if len(fn.Body) != 3 {
		t.Fatalf("the body is %v, want three statements", fn.Body)
	}
	if fn.Body[0].Op != OAssign || fn.Body[0].Op1 != defineOp {
		t.Errorf("the first statement is %v, want a declaring assignment", fn.Body[0].Op)
	}
	// n is the destination of the first and an operand of the second, and the
	// two have to be the same object.
	if fn.Body[0].X.Obj != fn.Locals[0] {
		t.Errorf("the first assignment writes %v, want n", fn.Body[0].X.Obj)
	}
	if got := fn.Body[1].Y.X.Obj; got != fn.Locals[0] {
		t.Errorf("the second assignment reads %v, want n", got)
	}
	if got := fn.Body[2].Args[0].Obj; got != fn.Locals[1] {
		t.Errorf("the return reads %v, want m", got)
	}
}

// TestForeignBodyBuildsAParallelAssignmentAsASwap is the one shape a wrong
// build turns into two copies of one value.
//
// "a, b = b, a" is a swap because the specification reads every value before
// it writes any destination. Emitting the two assignments in order without
// that leaves b in both, which compiles, links and computes something else.
func TestForeignBodyBuildsAParallelAssignmentAsASwap(t *testing.T) {
	p, err := foreignIndexBody(t,
		// n := 1, so that the swap is between two locals of the body.
		&export.AssignStmt{
			Lhs: []export.Assignee{foreignDef("n")},
			Rhs: export.MultiExpr{Exprs: []export.Expr{foreignConst(1)}},
		},
		// v, n = n, v
		&export.AssignStmt{
			Lhs: []export.Assignee{foreignTo(foreignLocalUse(1)), foreignTo(foreignLocalUse(3))},
			Rhs: export.MultiExpr{Exprs: []export.Expr{foreignLocalUse(3), foreignLocalUse(1)}},
		},
		&export.ReturnStmt{Results: export.MultiExpr{Exprs: []export.Expr{foreignLocalUse(1)}}},
	)
	if err != nil {
		t.Fatalf("the instantiation was refused: %v", err)
	}
	fn := buildFuncOf(t, p, "Index[[]int,int]")
	// Each value is read into a temporary before either destination is
	// written, so the two assignments of the swap read temporaries and not the
	// variables the other assignment writes.
	var writes []*Node
	for _, s := range fn.Body {
		if s.Op == OAssign && s.Op1 == 0 {
			writes = append(writes, s)
		}
	}
	if len(writes) != 2 {
		t.Fatalf("the body holds %d plain assignments, want the two of the swap: %v", len(writes), fn.Body)
	}
	for i, w := range writes {
		if w.Y == nil || w.Y.Op != OLocal || w.Y.Obj == nil || !strings.HasPrefix(w.Y.Obj.Name, ".autotmp_") {
			t.Errorf("assignment %d reads %v, want a temporary holding the value from before the first write", i, w.Y)
		}
	}
}

// foreignBuiltinUse is a reference to a predeclared function from inside a
// written body.
func foreignBuiltinUse(name string) export.Expr {
	return &export.GlobalExpr{Obj: export.ObjUse{Name: name, Obj: types2.Universe.Lookup(name)}}
}

// TestForeignBodyBuildsTheStatementFormsItMaps is one written body per
// statement kind, checked by the node each one has to become.
//
// The end-to-end tests run the real bodies and compare the answers with gc's.
// What is measured here is the node, because a form that built the wrong node
// and still produced the right answer for one program is a form that is wrong
// for the next one.
func TestForeignBodyBuildsTheStatementFormsItMaps(t *testing.T) {
	for _, tt := range []struct {
		name string
		stmt export.Stmt
		want Op
	}{
		{
			"a var declaration with no value",
			&export.AssignStmt{Lhs: []export.Assignee{foreignDef("n")}},
			ODeclare,
		},
		{
			"an operation assignment",
			&export.AssignOpStmt{Op: export.OpAdd, Lhs: foreignLocalUse(1), Rhs: foreignConst(1)},
			OAssign,
		},
		{
			"a shift assignment, whose count keeps its own type",
			&export.AssignOpStmt{Op: export.OpLsh, Lhs: foreignLocalUse(1), Rhs: foreignConst(2)},
			OAssign,
		},
		{
			"an increment",
			&export.IncDecStmt{Op: export.OpAdd, X: foreignLocalUse(1)},
			OAssign,
		},
		{
			"a decrement",
			&export.IncDecStmt{Op: export.OpSub, X: foreignLocalUse(1)},
			OAssign,
		},
		{
			"a three-clause loop",
			&export.ForStmt{
				Init: []export.Stmt{&export.AssignStmt{
					Lhs: []export.Assignee{foreignDef("i")},
					Rhs: export.MultiExpr{Exprs: []export.Expr{foreignConst(0)}},
				}},
				Cond:         &export.BinaryExpr{Op: export.OpLt, X: foreignLocalUse(3), Y: foreignLocalUse(1)},
				Post:         []export.Stmt{&export.IncDecStmt{Op: export.OpAdd, X: foreignLocalUse(3)}},
				Body:         &export.BlockStmt{Body: []export.Stmt{&export.BranchStmt{Op: export.OpContinue}}},
				DistinctVars: true,
			},
			OFor,
		},
		{
			"a loop with no clause at all",
			&export.ForStmt{
				Body:         &export.BlockStmt{Body: []export.Stmt{&export.BranchStmt{Op: export.OpBreak}}},
				DistinctVars: true,
			},
			OFor,
		},
		{
			"an expression switch",
			&export.SwitchStmt{
				Tag: foreignLocalUse(1),
				Clauses: []export.CaseClause{
					{Exprs: []export.Expr{foreignConst(0), foreignConst(1)}},
					{},
				},
			},
			OSwitch,
		},
		{
			"a switch with no tag",
			&export.SwitchStmt{Clauses: []export.CaseClause{{}}},
			OSwitch,
		},
		{
			"the builtin len",
			&export.AssignStmt{
				Lhs: []export.Assignee{foreignTo(foreignLocalUse(1))},
				Rhs: export.MultiExpr{Exprs: []export.Expr{&export.CallExpr{
					Fun:  foreignBuiltinUse("len"),
					Args: export.MultiExpr{Exprs: []export.Expr{foreignLocalUse(0)}},
				}}},
			},
			OAssign,
		},
		{
			"the builtin cap",
			&export.AssignStmt{
				Lhs: []export.Assignee{foreignTo(foreignLocalUse(1))},
				Rhs: export.MultiExpr{Exprs: []export.Expr{&export.CallExpr{
					Fun:  foreignBuiltinUse("cap"),
					Args: export.MultiExpr{Exprs: []export.Expr{foreignLocalUse(0)}},
				}}},
			},
			OAssign,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := foreignIndexBody(t, tt.stmt)
			if err != nil {
				t.Fatalf("the form was refused: %v", err)
			}
			fn := buildFuncOf(t, p, "Index[[]int,int]")
			if len(fn.Body) == 0 {
				t.Fatal("the form built nothing")
			}
			last := fn.Body[len(fn.Body)-1]
			if last.Op != tt.want {
				t.Errorf("the form built %v, want %v", last.Op, tt.want)
			}
		})
	}
}

// TestForeignBodyBuildsAFunctionLiteralWithTheCapturesTheFormatLists is the
// capture half of specs/033-closures-defer-panic.md over a foreign body.
//
// The capture list is gc's own closureVars and is taken from the format rather
// than recovered from the objects the literal turned out to name. What that
// buys is an order: every object of a foreign body takes the unknown position,
// so a list recovered here and sorted by position and name would put two
// locals of one name in whichever order a map produced.
func TestForeignBodyBuildsAFunctionLiteralWithTheCapturesTheFormatLists(t *testing.T) {
	empty := types2.NewTuple()
	lit := &export.FuncLitExpr{
		// The literal captures v, the second local of the body, and writes it.
		Captured: []export.CapturedVar{{Local: export.LocalExpr{Index: 1}}},
		Decoded: &export.Body{
			HasBlock: true,
			Stmts: []export.Stmt{&export.AssignStmt{
				Lhs: []export.Assignee{foreignTo(&export.LocalExpr{Captured: true, Index: 0})},
				Rhs: export.MultiExpr{Exprs: []export.Expr{foreignConst(7)}},
			}},
		},
	}
	p, err := foreignIndexBody(t, &export.AssignStmt{
		Lhs: []export.Assignee{{
			Kind:  export.AssignDef,
			Name:  "f",
			Type:  export.TypeUse{Type: types2.NewSignatureType(nil, nil, nil, empty, empty, false)},
			Local: export.Local{DictRType: -1},
		}},
		Rhs: export.MultiExpr{Exprs: []export.Expr{lit}},
	})
	if err != nil {
		t.Fatalf("the literal was refused: %v", err)
	}
	outer := buildFuncOf(t, p, "Index[[]int,int]")
	inner := buildFuncOf(t, p, "Index[[]int,int].func1")
	if len(inner.Captures) != 1 {
		t.Fatalf("the literal captures %v, want one variable", inner.Captures)
	}
	// The object the literal captures is the object the body around it names,
	// which is what makes the capture by reference: an assignment in either is
	// seen by the other.
	want := outer.Params[1]
	if inner.Captures[0] != want {
		t.Fatalf("the literal captures %v, want the parameter %s", inner.Captures[0], want.Name)
	}
	if !want.Addrtaken || !want.Escapes {
		t.Errorf("the captured variable is not marked for a heap cell: addrtaken=%v escapes=%v", want.Addrtaken, want.Escapes)
	}
	// The closure node's operands and the function's capture list are one list
	// in one order, which is what makes a capture's position in the closure
	// object a fact both halves read.
	var node *Node
	for _, s := range outer.Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OClosure {
				node = n
			}
			return true
		})
	}
	if node == nil {
		t.Fatal("the body holds no closure node")
	}
	if len(node.Args) != 1 || node.Args[0].Obj != want {
		t.Errorf("the closure node's operands are %v, want the one capture", node.Args)
	}
}

// foreignMethodCallStmt is a call through a method selection of the given
// shape, written as a statement.
func foreignMethodCallStmt(m export.MethodRef) export.Stmt {
	return &export.ExprStmt{X: &export.CallExpr{Method: &export.MethodCall{
		Recv:   &export.RecvExpr{X: foreignLocalUse(1)},
		Method: m,
	}}}
}

// TestForeignBodyRefusesTheShapesItCannotBuild is the refusal table of the
// arms inside a kind the walk does map.
//
// A kind the walk has no case for is refused by name, and the rows below are
// the other half: a kind the walk does map, in a shape it does not build. Each
// has to name itself rather than be built as something near it.
func TestForeignBodyRefusesTheShapesItCannotBuild(t *testing.T) {
	sig := types2.NewSignatureType(nil, nil, nil, types2.NewTuple(), types2.NewTuple(), false)
	for _, tt := range []struct {
		name string
		stmt export.Stmt
		want string
	}{
		{
			"an assignment of two values to one destination",
			&export.AssignStmt{
				Lhs: []export.Assignee{foreignTo(foreignLocalUse(1))},
				Rhs: export.MultiExpr{Exprs: []export.Expr{foreignConst(1), foreignConst(2)}},
			},
			"an assignment of 2 value(s) to 1 destination(s)",
		},
		{
			"a multi-value assignment whose value count does not match",
			&export.AssignStmt{
				Lhs: []export.Assignee{foreignTo(foreignLocalUse(1)), foreignTo(foreignLocalUse(2))},
				Rhs: export.MultiExpr{Single: true, Expr: foreignConst(1)},
			},
			"an assignment of 0 value(s) to 2 destination(s)",
		},
		{
			"a multi-value assignment whose value is converted where it is assigned",
			&export.AssignStmt{
				Lhs: []export.Assignee{foreignTo(foreignLocalUse(1))},
				Rhs: export.MultiExpr{
					Single:  true,
					Expr:    foreignConst(1),
					Results: []export.MultiResult{{Src: intUse, Converted: true, Dst: intUse}},
				},
			},
			"whose value 0 is converted where it is assigned",
		},
		{
			"a declaration whose destination declares no variable",
			&export.AssignStmt{Lhs: []export.Assignee{foreignTo(foreignLocalUse(1))}},
			"a declaration whose destination 0 declares no variable",
		},
		{
			"a destination the format has no encoding for",
			&export.AssignStmt{
				Lhs: []export.Assignee{{Kind: export.AssignKind(99)}},
				Rhs: export.MultiExpr{Exprs: []export.Expr{foreignConst(1)}},
			},
			"an assignment to a destination the format writes as",
		},
		{
			"a use of a captured variable in a body that captures nothing",
			&export.AssignStmt{
				Lhs: []export.Assignee{{Kind: export.AssignBlank}},
				Rhs: export.MultiExpr{Exprs: []export.Expr{&export.LocalExpr{Captured: true, Index: 0}}},
			},
			"a use of captured variable 0, of 0 the body has",
		},
		{
			"an operation assignment the walk has no operator for",
			&export.AssignOpStmt{Op: export.OpDeref, Lhs: foreignLocalUse(1), Rhs: foreignConst(1)},
			"the operation assignment",
		},
		{
			"an increment the walk has no operator for",
			&export.IncDecStmt{Op: export.OpMul, X: foreignLocalUse(1)},
			"the increment or decrement",
		},
		{
			"a for statement with no body",
			&export.ForStmt{},
			"a for statement with no body",
		},
		{
			// The rule before Go 1.22 shares one variable across the
			// iterations, and it differs from the rule after it only for a
			// variable whose address is taken. Building the loop under the
			// later rule would give the address a different variable on every
			// iteration.
			"a loop that shares one address-taken variable across its iterations",
			&export.ForStmt{
				Init: []export.Stmt{&export.AssignStmt{
					Lhs: []export.Assignee{foreignDef("i")},
					Rhs: export.MultiExpr{Exprs: []export.Expr{foreignConst(0)}},
				}},
				Body: &export.BlockStmt{Body: []export.Stmt{&export.ExprStmt{
					X: &export.UnaryExpr{Op: export.OpAddr, X: foreignLocalUse(3)},
				}}},
			},
			"shares one address-taken variable across its iterations",
		},
		{
			"a switch on a value with no type",
			&export.SwitchStmt{Tag: &export.LocalExpr{Captured: true, Index: 0}},
			"a switch on a value whose type the stream does not carry",
		},
		{
			"a call of a builtin the walk does not build",
			&export.ExprStmt{X: &export.CallExpr{
				Fun:  foreignBuiltinUse("new"),
				Args: export.MultiExpr{Exprs: []export.Expr{foreignConst(1)}},
			}},
			"a call of the builtin new",
		},
		{
			"a call of len carrying a type descriptor",
			&export.ExprStmt{X: &export.CallExpr{
				Fun:   foreignBuiltinUse("len"),
				RType: &export.RType{},
				Args:  export.MultiExpr{Exprs: []export.Expr{foreignLocalUse(0)}},
			}},
			"a call of the builtin len carrying a type descriptor",
		},
		{
			"a call of len with the wrong number of arguments",
			&export.ExprStmt{X: &export.CallExpr{
				Fun:  foreignBuiltinUse("len"),
				Args: export.MultiExpr{Exprs: []export.Expr{foreignLocalUse(0), foreignLocalUse(0)}},
			}},
			"a call of the builtin len with 2 argument(s)",
		},
		{
			"a method with type parameters of its own",
			foreignMethodCallStmt(export.MethodRef{Generic: true, Sel: export.Selector{Name: "M"}}),
			"a method with type parameters of its own",
		},
		{
			"a method of a type parameter",
			foreignMethodCallStmt(export.MethodRef{TypeParam: true, Sel: export.Selector{Name: "M"}}),
			"a call of M on a type parameter",
		},
		{
			"a method through a subdictionary",
			foreignMethodCallStmt(export.MethodRef{Subdict: true, Sel: export.Selector{Name: "M"}}),
			"a call of M through a subdictionary",
		},
		{
			"a method with a dictionary argument",
			foreignMethodCallStmt(export.MethodRef{StaticDict: true, Sel: export.Selector{Name: "M"}}),
			"a call of M with a dictionary argument",
		},
		{
			"a method of a type that has none",
			foreignMethodCallStmt(export.MethodRef{Sel: export.Selector{Name: "M"}, Recv: intUse}),
			"a call of M, which int has no method for",
		},
		{
			"a method selection with no receiver at all",
			&export.ExprStmt{X: &export.CallExpr{Method: &export.MethodCall{
				Method: export.MethodRef{Sel: export.Selector{Name: "M"}, Recv: intUse},
			}}},
			"a method selection with no receiver",
		},
		{
			"a method selection on an operand with no type",
			&export.ExprStmt{X: &export.CallExpr{Method: &export.MethodCall{
				Recv:   &export.RecvExpr{X: &export.LocalExpr{Captured: true, Index: 0}},
				Method: export.MethodRef{Sel: export.Selector{Name: "M"}, Recv: intUse},
			}}},
			"a method selection on an operand with no type",
		},
		{
			"a method selection that dereferences a value that is no pointer",
			&export.ExprStmt{X: &export.CallExpr{Method: &export.MethodCall{
				Recv:   &export.RecvExpr{X: foreignLocalUse(1), Deref: true},
				Method: export.MethodRef{Sel: export.Selector{Name: "M"}, Recv: intUse},
			}}},
			"a method selection that dereferences int",
		},
		{
			// The adjustment read out of the tree and the type the tree
			// recorded for the selection disagree, so the method would be
			// called with a receiver of another type.
			"a method selection whose receiver is not the type it recorded",
			&export.ExprStmt{X: &export.CallExpr{Method: &export.MethodCall{
				Recv:   &export.RecvExpr{X: foreignLocalUse(1), Addr: true},
				Method: export.MethodRef{Sel: export.Selector{Name: "M"}, Recv: intUse},
			}}},
			"whose receiver is *int where int was recorded",
		},
		{
			"a method through an interface",
			foreignMethodCallStmt(export.MethodRef{
				Sel:  export.Selector{Name: "M"},
				Recv: export.TypeUse{Type: types2.NewInterfaceType(nil, nil)},
			}),
			"through the interface",
		},
		{
			"a call of a function literal whose own type the stream does not carry",
			&export.ExprStmt{X: &export.CallExpr{
				Fun: &export.FuncLitExpr{Decoded: &export.Body{HasBlock: true}},
			}},
			"a call of an operand with no signature",
		},
		// The slice expression. The walk builds it over a slice, a string,
		// an array and a pointer to an array, and the three rows below are
		// the shapes outside that set. Each names the operand, because the
		// operand is what decides the lowering.
		{
			"a slice of an operand with no type",
			&export.AssignStmt{
				Lhs: []export.Assignee{{Kind: export.AssignBlank}},
				Rhs: export.MultiExpr{Exprs: []export.Expr{&export.SliceExpr{X: foreignConst(1)}}},
			},
			"a slice expression over an operand with no type",
		},
		{
			"a slice of a value that is no slice, string or array",
			&export.AssignStmt{
				Lhs: []export.Assignee{{Kind: export.AssignBlank}},
				Rhs: export.MultiExpr{Exprs: []export.Expr{&export.SliceExpr{X: foreignLocalUse(1)}}},
			},
			"a slice expression over int",
		},
		{
			// A string has no capacity to give the result, so the language
			// has no three-index slice of one. gc rejects it, which means an
			// archive carrying one was not written from Go the checker
			// accepted, and building it would need a capacity invented here.
			"a three-index slice of a string",
			foreignSliceOfDeclared(types2.Typ[types2.String], [3]export.Expr{
				foreignConst(0), foreignConst(1), foreignConst(2),
			}),
			"a three-index slice of the string string",
		},
		{
			// A pointer is an operand class only when it points at an array,
			// which is the one shape whose length the lowering can read.
			"a slice of a pointer to something that is no array",
			foreignSliceOfDeclared(types2.NewPointer(types2.Typ[types2.Int]), [3]export.Expr{}),
			"a slice expression over *int",
		},
		{
			"a slice of a map",
			foreignSliceOfDeclared(types2.NewMap(types2.Typ[types2.Int], types2.Typ[types2.Int]), [3]export.Expr{}),
			"a slice expression over map[int]int",
		},
		{
			// The bounds are built and the operand is a slice, so the only
			// thing missing is the type of the result, which decides the
			// element size every bound is scaled by.
			"a slice expression the stream carries no type for",
			&export.AssignStmt{
				Lhs: []export.Assignee{{Kind: export.AssignBlank}},
				Rhs: export.MultiExpr{Exprs: []export.Expr{&export.SliceExpr{X: foreignLocalUse(0)}}},
			},
			"a slice expression whose type the stream does not carry",
		},
		{
			"a function literal that names a capture it does not list",
			&export.AssignStmt{
				Lhs: []export.Assignee{{
					Kind:  export.AssignDef,
					Name:  "f",
					Type:  export.TypeUse{Type: sig},
					Local: export.Local{DictRType: -1},
				}},
				Rhs: export.MultiExpr{Exprs: []export.Expr{&export.FuncLitExpr{
					Decoded: &export.Body{
						HasBlock: true,
						Stmts: []export.Stmt{&export.ExprStmt{
							X: &export.LocalExpr{Captured: true, Index: 0},
						}},
					},
				}}},
			},
			"a use of captured variable 0, of 0 the body has",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := foreignIndexBody(t, tt.stmt)
			if err == nil {
				t.Fatal("a shape the walk cannot build was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the refusal is %q, want it to hold %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "slices.Index[[]int,int]") {
				t.Errorf("the refusal is %q and does not name the instantiation", err)
			}
		})
	}
}

// TestForeignBodyRefusesAMethodSelectionWithNoReceiverNode is the shape gc
// never writes and a misread stream would produce.
//
// gc writes a receiver node in front of every method selection, carrying the
// embedded fields, the dereference and the address the selection applies. A
// selection without one is not the shape this walk read, and taking the
// operand as the receiver would call the method with a value of another type.
func TestForeignBodyRefusesAMethodSelectionWithNoReceiverNode(t *testing.T) {
	_, err := foreignIndexBody(t, &export.ExprStmt{X: &export.CallExpr{Method: &export.MethodCall{
		Recv:   foreignLocalUse(1),
		Method: export.MethodRef{Sel: export.Selector{Name: "M"}, Recv: intUse},
	}}})
	if err == nil {
		t.Fatal("a method selection with no receiver node was accepted")
	}
	if !strings.Contains(err.Error(), "rather than a receiver node") {
		t.Errorf("the refusal is %q", err)
	}
}

// TestForeignNumLocalsCountsEveryDeclaringSite is the count the walk is
// checked against.
//
// The format writes a local at three sites: an assignment that declares its
// destination, a range clause that declares one, and a clause of a type switch
// whose guard names a variable. A count that missed one would let a walk that
// missed the same one through, so the two are written apart and compared.
func TestForeignNumLocalsCountsEveryDeclaringSite(t *testing.T) {
	def := foreignDef("x")
	body := &export.Body{
		Params: []export.Local{{DictRType: -1}, {DictRType: -1}},
		Stmts: []export.Stmt{
			&export.AssignStmt{Lhs: []export.Assignee{def, {Kind: export.AssignBlank}}},
			&export.BlockStmt{Body: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}}},
			&export.IfStmt{
				Init: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
				Then: &export.BlockStmt{Body: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}}},
				Else: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
			},
			&export.ForStmt{
				Init: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
				Post: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
				Body: &export.BlockStmt{Body: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}}},
			},
			&export.ForStmt{
				Range: &export.RangeClause{Lhs: []export.Assignee{def, def}},
				Body:  &export.BlockStmt{},
			},
			&export.SwitchStmt{
				Init: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
				Clauses: []export.CaseClause{{
					Var:  &export.Local{DictRType: -1},
					Body: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
				}},
			},
			&export.SelectStmt{Clauses: []export.CommClause{{
				Comm: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
				Body: []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
			}}},
			// A function literal's body is another element with a numbering of
			// its own, so its declarations belong to no count here.
			&export.ExprStmt{X: &export.FuncLitExpr{Decoded: &export.Body{
				Params: []export.Local{{DictRType: -1}},
				Stmts:  []export.Stmt{&export.AssignStmt{Lhs: []export.Assignee{def}}},
			}}},
		},
	}
	if got, want := foreignNumLocals(body), 2+15; got != want {
		t.Errorf("the body numbers %d local(s), want %d", got, want)
	}
}
