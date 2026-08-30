// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"errors"
	"go/constant"
	"strings"
	"testing"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/syntax"
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
		{"make", &export.ExprStmt{X: &export.MakeExpr{}}, `the expression "make"`},
		{"a type assertion", &export.ExprStmt{X: &export.AssertExpr{}}, `the expression "type assertion"`},

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
		{"no body in the archive", &fixedBodies{}, "no archive this compilation read holds a body for it"},
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

// foreignValue is a statement that walks one expression and throws the value
// away, which is what a refusal row for an expression needs.
func foreignValue(e export.Expr) export.Stmt { return &export.ExprStmt{X: e} }

// foreignPairStruct is a struct of two int fields, which is the smallest type
// a struct literal's field index can be out of range for.
var foreignPairStruct = types2.NewStruct([]*types2.Var{
	types2.NewField(syntax.NoPos, nil, "A", types2.Typ[types2.Int], false),
	types2.NewField(syntax.NoPos, nil, "B", types2.Typ[types2.Int], false),
}, nil)

// foreignDefOf is a destination that declares a variable of a given type.
func foreignDefOf(name string, t types2.Type) export.Assignee {
	return export.Assignee{
		Kind:  export.AssignDef,
		Name:  name,
		Type:  export.TypeUse{Type: t},
		Local: export.Local{DictRType: -1},
	}
}

// foreignDeclaredValue is a body that declares one variable of a given type
// and gives it one value, and returns the assignment the walk built for it.
//
// It is the shape every success test of a value-producing node needs here. A
// hand-written node carries no reshape, so the walk reads the type off the
// node's own field, and a declared destination is what gives the assignment a
// type on the other side. The variable takes the number after the three the
// body opens with.
func foreignDeclaredValue(t *testing.T, typ types2.Type, value export.Expr) Expr {
	t.Helper()
	p, err := foreignIndexBody(t, &export.AssignStmt{
		Lhs: []export.Assignee{foreignDefOf("x", typ)},
		Rhs: export.MultiExpr{Exprs: []export.Expr{value}},
	})
	if err != nil {
		t.Fatalf("the instantiation was refused: %v", err)
	}
	fn := buildFuncOf(t, p, "Index[[]int,int]")
	for _, s := range fn.Body {
		if s.Op == OAssign && s.Y != nil {
			return s.Y
		}
	}
	t.Fatalf("the body holds no assignment: %v", fn.Body)
	return nil
}

// TestForeignBodyBuildsTheZeroValueWrittenForNil is the success half of the
// zero value.
//
// gc writes the node for the predeclared nil and for nothing else, so the two
// rows below are the two nil-shaped classes a body can carry it at: a pointer,
// whose zero is one word of zeroes, and an interface, whose zero is two. Both
// are the nil constant [builder.zeroValue] returns, and the type on it is what
// tells every pass below how wide it is.
func TestForeignBodyBuildsTheZeroValueWrittenForNil(t *testing.T) {
	for _, tt := range []struct {
		name string
		typ  types2.Type
		want Kind
	}{
		{"a pointer", types2.NewPointer(types2.Typ[types2.Int]), Ptr},
		{"an interface", types2.NewInterfaceType(nil, nil), Interface},
		{"a slice", types2.NewSlice(types2.Typ[types2.Int]), Slice},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := foreignDeclaredValue(t, tt.typ, &export.ZeroExpr{Type: export.TypeUse{Type: tt.typ}})
			if n.Op != OConst {
				t.Fatalf("the zero value is %v, want a constant", n.Op)
			}
			if n.Val == nil || n.Val.String() != "nil" {
				t.Errorf("the zero value carries the constant %v, want nil", n.Val)
			}
			if n.Type == nil || n.Type.Kind != tt.want {
				t.Errorf("the zero value has the type %s, want a %s", n.Type, tt.want)
			}
		})
	}
}

// TestForeignBodyBuildsTheZeroValueOfAStruct is the arm zeroValue answers with
// a literal rather than a constant.
//
// gc does not write this node at a struct type today, because it writes it
// only for nil. The walk still goes through the one local zero builder rather
// than a nil constant of its own, and this pins that: a struct zero is the
// empty composite literal ir/lower.go clears, and a nil constant of struct
// type would be one word where the value is several.
func TestForeignBodyBuildsTheZeroValueOfAStruct(t *testing.T) {
	n := foreignDeclaredValue(t, foreignPairStruct,
		&export.ZeroExpr{Type: export.TypeUse{Type: foreignPairStruct}})
	if n.Op != OCompositeLit || len(n.Args) != 0 {
		t.Fatalf("the zero value is %v with %d element(s), want an empty literal", n.Op, len(n.Args))
	}
	if n.Type == nil || n.Type.Kind != Struct {
		t.Errorf("the zero value has the type %s, want a struct", n.Type)
	}
}

// TestForeignBodyNormalisesAStructLiteralToOneElementPerField is the shape
// ir/lower.go's structLit reads.
//
// The format writes a keyed literal as the elements the body wrote, each with
// its field index, and a positional one as every element in order. Both become
// one element per field in declaration order here, and a field the literal
// left out is written out as its zero value, because structLit writes every
// field of a literal that has elements and clears nothing before it. A walk
// that passed the keyed list through would write the value of field 1 into
// field 0.
func TestForeignBodyNormalisesAStructLiteralToOneElementPerField(t *testing.T) {
	for _, tt := range []struct {
		name  string
		lit   *export.CompLitExpr
		wantA int64
		wantB int64
	}{
		{
			"positional",
			&export.CompLitExpr{
				Type:  export.TypeUse{Type: foreignPairStruct},
				Elems: []export.LitElem{{Field: 0, Value: foreignConst(3)}, {Field: 1, Value: foreignConst(4)}},
			},
			3, 4,
		},
		{
			"keyed, with the first field left out",
			&export.CompLitExpr{
				Type:  export.TypeUse{Type: foreignPairStruct},
				Keyed: true,
				Elems: []export.LitElem{{Field: 1, Value: foreignConst(9)}},
			},
			0, 9,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := foreignDeclaredValue(t, foreignPairStruct, tt.lit)
			if n.Op != OCompositeLit || len(n.Args) != 2 {
				t.Fatalf("the literal is %v with %d element(s), want two", n.Op, len(n.Args))
			}
			for i, want := range []int64{tt.wantA, tt.wantB} {
				got, ok := n.Args[i].Val.(Const).Int64()
				if !ok || got != want {
					t.Errorf("field %d holds %v, want %d", i, n.Args[i], want)
				}
			}
		})
	}
}

// TestForeignBodyKeepsTheElementListOfAnArrayLiteral is the arm that is not
// normalised, and the reason it is not.
//
// An element of an array or a slice literal carries its index and not its
// position, so a keyed element becomes an assignment of the value to the key
// and a positional one stays a value. ir/lower.go's litIndices reads exactly
// that pair, and it is what gives [...]int{5: 1, 2} the indices 5 and 6.
func TestForeignBodyKeepsTheElementListOfAnArrayLiteral(t *testing.T) {
	at := types2.NewArray(types2.Typ[types2.Int], 4)
	n := foreignDeclaredValue(t, at, &export.CompLitExpr{
		Type:  export.TypeUse{Type: at},
		Keyed: true,
		Elems: []export.LitElem{{Key: foreignConst(2), Value: foreignConst(7)}, {Value: foreignConst(8)}},
	})
	if n.Op != OCompositeLit || len(n.Args) != 2 {
		t.Fatalf("the literal is %v with %d element(s), want two", n.Op, len(n.Args))
	}
	keyed := n.Args[0]
	if keyed.Op != OAssign {
		t.Fatalf("the keyed element is %v, want an assignment of the value to the key", keyed.Op)
	}
	if got, ok := keyed.X.Val.(Const).Int64(); !ok || got != 2 {
		t.Errorf("the key is %v, want 2", keyed.X)
	}
	if got, ok := keyed.Y.Val.(Const).Int64(); !ok || got != 7 {
		t.Errorf("the keyed value is %v, want 7", keyed.Y)
	}
	if n.Args[1].Op != OConst {
		t.Errorf("the positional element is %v, want the value itself", n.Args[1].Op)
	}
}

// TestForeignBodyBuildsAMapLiteralAsPairs is the third element encoding.
//
// Every element of a map literal has a key, and ir/lower.go's mapLit reads
// each element as a key and a value and refuses anything else. The descriptor
// it makes the map with comes from the node's own type, which is why
// [export.CompLitExpr.MapRType] is not read here.
func TestForeignBodyBuildsAMapLiteralAsPairs(t *testing.T) {
	mt := types2.NewMap(types2.Typ[types2.Int], types2.Typ[types2.Int])
	n := foreignDeclaredValue(t, mt, &export.CompLitExpr{
		Type:  export.TypeUse{Type: mt},
		Keyed: true,
		Elems: []export.LitElem{{Key: foreignConst(1), Value: foreignConst(2)}},
	})
	if n.Op != OCompositeLit || n.Type == nil || n.Type.Kind != Map {
		t.Fatalf("the literal is %v of %s, want a map literal", n.Op, n.Type)
	}
	if len(n.Args) != 1 || n.Args[0].Op != OAssign {
		t.Fatalf("the literal holds %v, want one key and value pair", n.Args)
	}
}

// TestForeignBodyBuildsAPointerLiteralAsTheAddressOfOne is the form an element
// of []*T is written as.
//
// The literal is built at the element type and the address of it is the value,
// which is the pair ir/lower.go's addrLit turns into an allocation. A walk
// that built the literal at the pointer type would ask the lowering for a
// literal of a pointer, which has no elements at all.
func TestForeignBodyBuildsAPointerLiteralAsTheAddressOfOne(t *testing.T) {
	pt := types2.NewPointer(foreignPairStruct)
	n := foreignDeclaredValue(t, pt, &export.CompLitExpr{
		Type:  export.TypeUse{Type: pt},
		Keyed: true,
		Elems: []export.LitElem{{Field: 0, Value: foreignConst(5)}},
	})
	if n.Op != OAddr || n.Type == nil || n.Type.Kind != Ptr {
		t.Fatalf("the literal is %v of %s, want the address of one", n.Op, n.Type)
	}
	if n.X == nil || n.X.Op != OCompositeLit || n.X.Type == nil || n.X.Type.Kind != Struct {
		t.Fatalf("the address is taken of %v, want a struct literal", n.X)
	}
	if len(n.X.Args) != 2 {
		t.Errorf("the literal holds %d element(s), want one per field", len(n.X.Args))
	}
}

// foreignDeferBody builds a body that declares one variable of function type
// and then defers or spawns a call of it, and returns the statements the walk
// emitted.
func foreignDeferBody(t *testing.T, op export.Op, sig *types2.Signature, args ...export.Expr) []Stmt {
	t.Helper()
	p, err := foreignIndexBody(t,
		// var f func(...)
		&export.AssignStmt{Lhs: []export.Assignee{foreignDefOf("f", sig)}},
		&export.CallStmt{Op: op, Call: &export.CallExpr{
			Fun:  foreignLocalUse(3),
			Args: export.MultiExpr{Exprs: args},
		}},
	)
	if err != nil {
		t.Fatalf("the instantiation was refused: %v", err)
	}
	return buildFuncOf(t, p, "Index[[]int,int]").Body
}

// TestForeignBodyEvaluatesTheOperandsOfAGoAndADeferAtTheStatement is the
// requirement the node alone does not carry.
//
// The specification evaluates the callee and the operands where the statement
// is written and not where the call runs. Each one is held in a temporary
// assigned ahead of the ODefer, so a variable assigned between the statement
// and the call still calls the value the statement read. A walk that left them
// in place compiles and calls something else.
func TestForeignBodyEvaluatesTheOperandsOfAGoAndADeferAtTheStatement(t *testing.T) {
	intParam := types2.NewTuple(types2.NewParam(syntax.NoPos, nil, "d", types2.Typ[types2.Int]))
	sig := types2.NewSignatureType(nil, nil, nil, intParam, types2.NewTuple(), false)
	body := foreignDeferBody(t, export.OpDefer, sig, foreignLocalUse(1))

	var temps int
	for _, s := range body {
		if s.Op == OAssign && s.Op1 == defineOp {
			temps++
			continue
		}
		if s.Op != ODefer {
			continue
		}
		if temps != 2 {
			t.Errorf("%d temporaries stand before the defer, want one for the callee and one for the operand", temps)
		}
		return
	}
	t.Fatalf("the body holds no defer: %v", body)
}

// TestForeignBodyWrapsADeferredCallThatHasOperands is the shape
// runtime.deferproc takes.
//
// It takes one word, a func value, and calls it with no arguments, so an
// operand travels inside that value as a capture. A call with no operands
// needs no literal and gets none, because a frame between the deferred call
// and runtime.gopanic is the one thing recover counts.
func TestForeignBodyWrapsADeferredCallThatHasOperands(t *testing.T) {
	empty := types2.NewTuple()
	intParam := types2.NewTuple(types2.NewParam(syntax.NoPos, nil, "d", types2.Typ[types2.Int]))
	for _, tt := range []struct {
		name    string
		op      export.Op
		sig     *types2.Signature
		args    []export.Expr
		want    Op
		wrapped bool
	}{
		{"a defer with an operand", export.OpDefer, types2.NewSignatureType(nil, nil, nil, intParam, empty, false),
			[]export.Expr{foreignLocalUse(1)}, ODefer, true},
		{"a defer with none", export.OpDefer, types2.NewSignatureType(nil, nil, nil, empty, empty, false),
			nil, ODefer, false},
		{"a go with an operand", export.OpGo, types2.NewSignatureType(nil, nil, nil, intParam, empty, false),
			[]export.Expr{foreignLocalUse(1)}, OGo, true},
		{"a go with none", export.OpGo, types2.NewSignatureType(nil, nil, nil, empty, empty, false),
			nil, OGo, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := foreignDeferBody(t, tt.op, tt.sig, tt.args...)
			var n Expr
			for _, s := range body {
				if s.Op == tt.want {
					n = s
				}
			}
			if n == nil {
				t.Fatalf("the body holds no %v: %v", tt.want, body)
			}
			if n.X == nil || n.X.Op != OCall {
				t.Fatalf("the statement runs %v, want a call", n.X)
			}
			if got := n.X.X != nil && n.X.X.Op == OClosure; got != tt.wrapped {
				t.Errorf("the call is of %v, and wrapping it was %v", n.X.X, tt.wrapped)
			}
		})
	}
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
		// The composite literal. The type decides which of the four element
		// encodings the elements are read under, and each row below is a
		// shape inside one of them that no writer produces and that would
		// write the wrong storage if it were built.
		{
			"a composite literal with no type",
			foreignValue(&export.CompLitExpr{}),
			"a composite literal whose type the stream does not carry",
		},
		{
			"a composite literal of a type with no element encoding",
			foreignValue(&export.CompLitExpr{Type: intUse}),
			"a composite literal of int",
		},
		{
			// A key naming a promoted field is not legal Go, so gc's own
			// compile of the declaring package rejected every body holding
			// one. The field a one-step index would pick belongs to the
			// embedded struct and not to this one.
			"an element of a struct literal that names a promoted field",
			foreignValue(&export.CompLitExpr{
				Type:  export.TypeUse{Type: foreignPairStruct},
				Keyed: true,
				Elems: []export.LitElem{{Field: 0, Embedded: []int{0, 0}, Value: foreignConst(1)}},
			}),
			"whose key names a promoted field 2 level(s) down",
		},
		{
			"an element of a struct literal at a field the struct has not",
			foreignValue(&export.CompLitExpr{
				Type:  export.TypeUse{Type: foreignPairStruct},
				Keyed: true,
				Elems: []export.LitElem{{Field: 7, Value: foreignConst(1)}},
			}),
			"at field 7, of 2 it has",
		},
		{
			"two elements of a struct literal for one field",
			foreignValue(&export.CompLitExpr{
				Type:  export.TypeUse{Type: foreignPairStruct},
				Keyed: true,
				Elems: []export.LitElem{{Field: 1, Value: foreignConst(1)}, {Field: 1, Value: foreignConst(2)}},
			}),
			"for field 1",
		},
		{
			"an element of a map literal with no key",
			foreignValue(&export.CompLitExpr{
				Type:  export.TypeUse{Type: types2.NewMap(types2.Typ[types2.Int], types2.Typ[types2.Int])},
				Keyed: true,
				Elems: []export.LitElem{{Value: foreignConst(1)}},
			}),
			"an element of a literal of map[int]int with no key",
		},

		// new. The format writes one bit choosing between new(T) and new(x)
		// and the operand that follows it, so a node carrying both or neither
		// is a stream that was misread.
		{
			"a new naming neither a value nor a type",
			foreignValue(&export.NewExpr{}),
			"a new naming neither a value nor a type",
		},
		{
			"a new naming both a value and a type",
			foreignValue(&export.NewExpr{Value: foreignConst(1), Type: &export.ExprType{}}),
			"a new naming both a value and a type",
		},
		{
			"a new whose type the stream does not carry",
			foreignValue(&export.NewExpr{Value: foreignConst(1)}),
			"a new whose type the stream does not carry",
		},

		// go and defer. The operator says which of the two it is, the call is
		// what the statement runs, and the defer record is the second runtime
		// entry point this compilation has no allocation for.
		{
			"a go or defer written with neither operator",
			&export.CallStmt{Op: export.OpAdd, Call: foreignConst(1)},
			`the statement "go or defer" written with the operator "+"`,
		},
		{
			"a defer that names the record it runs in",
			&export.CallStmt{Op: export.OpDefer, Call: foreignConst(1), DeferAt: foreignConst(2)},
			"a defer that names the defer record it runs in",
		},
		{
			"a defer of nothing",
			&export.CallStmt{Op: export.OpDefer},
			"a defer of nothing",
		},
		{
			"a go of something that is not a call",
			&export.CallStmt{Op: export.OpGo, Call: foreignConst(1)},
			"a go of const, which is not a call",
		},
		{
			// The zero value. The type is the whole of the node, because the
			// zero of a pointer and the zero of a struct are values of
			// different widths, so a node without one is refused rather than
			// given the nil the name suggests.
			"a zero value with no type",
			&export.AssignStmt{
				Lhs: []export.Assignee{foreignDefOf("x", types2.Typ[types2.Int])},
				Rhs: export.MultiExpr{Exprs: []export.Expr{&export.ZeroExpr{}}},
			},
			"a zero value whose type the stream does not carry",
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

// The method set of an instantiation of a generic type another package
// declares (specs/017-export-data-reading.md).
//
// The descriptor of such an instantiation names every method it has, so the
// package that emits the descriptor owes every body. The three tests below
// measure the halves of that: the builder asks for the whole method set rather
// than for the methods the source calls, every instantiation it builds is
// duplicate-tolerant, and a method whose body cannot be built is recorded
// rather than turned into a refusal of the package.

// cellSource is a package that holds an instantiation of sync/atomic.Pointer
// and calls no method of it at all.
//
// No call, on purpose. Every method of the four is then reached by the method
// set pass and by nothing else, so a build that asks for four bodies asks for
// them because a descriptor would name them.
const cellSource = "package p\n\nimport \"sync/atomic\"\n\n" +
	"type cell struct{ p atomic.Pointer[[]int] }\n\n" +
	"func use(c *cell) *cell { return c }\n"

// pointerTypeParams returns the type parameters of sync/atomic.Pointer as the
// checker made them, which is the domain a body of one of its methods
// substitutes through.
func pointerTypeParams(t *testing.T, pkg *types2.Package) []*types2.TypeParam {
	t.Helper()
	for _, imp := range pkg.Imports() {
		if imp.Path() != "sync/atomic" {
			continue
		}
		obj, _ := imp.Scope().Lookup("Pointer").(*types2.TypeName)
		named, _ := obj.Type().(*types2.Named)
		if named == nil {
			t.Fatal("the checked sync/atomic package declares no Pointer type")
		}
		list := named.TypeParams()
		out := make([]*types2.TypeParam, list.Len())
		for i := range out {
			out[i] = list.At(i)
		}
		return out
	}
	t.Fatal("the checked package does not import sync/atomic")
	return nil
}

// pointerLocals is the number of locals each method of sync/atomic.Pointer
// opens with: the receiver, then the parameters, then the results.
var pointerLocals = map[string]int{
	"(*Pointer).Load":           2,
	"(*Pointer).Store":          2,
	"(*Pointer).Swap":           3,
	"(*Pointer).CompareAndSwap": 4,
}

// methodBodies is a [BodySource] that answers with a body of the right shape
// for each method of sync/atomic.Pointer.
//
// The number of locals is the number the signature declares, because
// buildForeignInstance compares the two and a body that opened with the wrong
// count would be refused for that rather than for what is measured here.
type methodBodies struct {
	dict *export.Dict
	err  error
	// refuse holds, per declaration, a statement the walk has no case for.
	// A body carrying one decodes and starts building, which is what makes
	// the undo path different from a body that could not be read at all.
	refuse map[string]export.Stmt
	// asked records the declarations the pass looked for, in order.
	asked []string
}

func (m *methodBodies) Body(path, name string) (*export.FuncBody, error) {
	m.asked = append(m.asked, name)
	if m.err != nil {
		return nil, m.err
	}
	n, ok := pointerLocals[name]
	if !ok {
		return nil, nil
	}
	params := make([]export.Local, n)
	for i := range params {
		params[i].DictRType = -1
	}
	stmts := []export.Stmt{&export.ReturnStmt{}}
	if s, ok := m.refuse[name]; ok {
		stmts = []export.Stmt{s}
	}
	return &export.FuncBody{Path: path, Name: name, Generic: true, Body: &export.Body{
		Params:   params,
		HasBlock: true,
		Stmts:    stmts,
		Dict:     m.dict,
	}}, nil
}

// TestForeignMethodSetIsBuiltWhole is stage 3 of
// specs/017-export-data-reading.md.
//
// Nothing in the source calls a method, so the builder used to ask for no body
// at all and the object defined none. The descriptor of the instantiation
// names four, and this is the test that the four are asked for and built.
func TestForeignMethodSetIsBuiltWhole(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, cellSource)
	src := &methodBodies{dict: &export.Dict{TypeParams: pointerTypeParams(t, pkg)}}
	withBodies(t, src)

	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, name := range []string{
		"(*Pointer).Load",
		"(*Pointer).Store",
		"(*Pointer).Swap",
		"(*Pointer).CompareAndSwap",
	} {
		if !askedFor(src.asked, name) {
			t.Errorf("the build never asked for the body of %s; it asked for %v", name, src.asked)
		}
	}
	if len(out.ForeignInsts) != 1 {
		t.Fatalf("the package records %d foreign instantiations and it holds one", len(out.ForeignInsts))
	}
	fi := out.ForeignInsts[0]
	if fi.Origin != "sync/atomic" || fi.Decl != "Pointer" {
		t.Errorf("the instantiation is recorded as %s.%s and it is sync/atomic.Pointer", fi.Origin, fi.Decl)
	}
	if len(fi.Methods) != 4 {
		t.Fatalf("the record names %d methods and the instantiation has 4", len(fi.Methods))
	}
	for _, m := range fi.Methods {
		if m.Reason != "" {
			t.Errorf("the method %s was not built: %s", m.Name, m.Reason)
		}
		if !m.PtrOnly {
			t.Errorf("the method %s is declared with a pointer receiver and the record says otherwise", m.Name)
		}
		if !definesSym(out, m.Sym) {
			t.Errorf("the package defines no function %s", m.Sym)
		}
	}
}

// TestForeignInstancesAreDuplicateTolerant reads the flag every program whose
// packages share one instantiation depends on.
//
// An instantiation belongs to no package, so every package that names it
// compiles it and defines the symbol. Without the mark cmd/link reports a
// duplicate symbol for a program whose two packages both hold the type, and
// the method set pass above makes that arrangement the ordinary one.
func TestForeignInstancesAreDuplicateTolerant(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, cellSource)
	withBodies(t, &methodBodies{dict: &export.Dict{TypeParams: pointerTypeParams(t, pkg)}})

	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := 0
	for _, fn := range out.Funcs {
		if !strings.Contains(fn.Sym, "sync/atomic.(*Pointer[") {
			continue
		}
		found++
		if !fn.Dupok {
			t.Errorf("%s is an instantiation and is not marked duplicate-tolerant", fn.Sym)
		}
	}
	if found != 4 {
		t.Errorf("the package defines %d methods of the instantiation and it owes 4", found)
	}
}

// TestForeignMethodSetRecordsAReasonRatherThanRefusing is the other half of
// the rule.
//
// A method nothing calls is built on speculation, and whether the descriptor
// that would name it is emitted is decided after lowering, by the driver. So a
// body this pass cannot read is a row with a reason on it and not a refusal:
// refusing here would refuse a package that emits no descriptor for the type
// and needs none of the bodies. driver/compile.go's checkForeignMethodSets is
// where a reason becomes a refusal.
func TestForeignMethodSetRecordsAReasonRatherThanRefusing(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, cellSource)
	withBodies(t, &methodBodies{err: errors.New("no archive holds it")})

	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build refused the package for a body nothing in it calls: %v", err)
	}
	if len(out.ForeignInsts) != 1 {
		t.Fatalf("the package records %d foreign instantiations and it holds one", len(out.ForeignInsts))
	}
	for _, m := range out.ForeignInsts[0].Methods {
		if m.Reason == "" {
			t.Errorf("the method %s has no body and the record carries no reason", m.Name)
		}
		if definesSym(out, m.Sym) {
			t.Errorf("the package defines %s and no body for it could be read", m.Sym)
		}
	}
}

// TestForeignMethodSetUndoesAnAttemptThatStartedBuilding is the other shape
// the reason can arrive in, and the one the undo exists for.
//
// A body that cannot be read is refused before anything is queued. A body that
// decodes and then meets a statement the walk has no case for has already been
// queued as an instance and already appended a function, and the walk's refusal
// is in b.errs. Leaving any of the three behind is what the undo prevents: the
// error would refuse the package, the function would be a body compiled from a
// half-built tree, and the instance would answer a later lookup with a symbol
// nothing defines.
func TestForeignMethodSetUndoesAnAttemptThatStartedBuilding(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, cellSource)
	src := &methodBodies{
		dict: &export.Dict{TypeParams: pointerTypeParams(t, pkg)},
		// An increment the walk has no operator for, which
		// TestForeignBodyRefusesTheShapesItCannotBuild names as well.
		refuse: map[string]export.Stmt{
			"(*Pointer).Swap": &export.IncDecStmt{Op: export.OpMul, X: foreignLocalUse(1)},
		},
	}
	withBodies(t, src)

	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build refused the package for a body nothing in it calls: %v", err)
	}
	if len(out.ForeignInsts) != 1 {
		t.Fatalf("the package records %d foreign instantiations and it holds one", len(out.ForeignInsts))
	}
	built := 0
	for _, m := range out.ForeignInsts[0].Methods {
		if m.Name == "Swap" {
			if !strings.Contains(m.Reason, "the increment or decrement") {
				t.Errorf("Swap's reason is %q and the walk refused an increment", m.Reason)
			}
			if definesSym(out, m.Sym) {
				t.Errorf("the package defines %s and the walk refused its body part way through", m.Sym)
			}
			continue
		}
		if m.Reason != "" {
			t.Errorf("the method %s was not built: %s", m.Name, m.Reason)
		}
		if !definesSym(out, m.Sym) {
			t.Errorf("the package defines no function %s", m.Sym)
		}
		built++
	}
	if built != 3 {
		t.Fatalf("%d of the four methods were built and three of them have a body", built)
	}
	// The count and not only the membership. A partial function left behind by
	// an attempt that failed carries a symbol of its own, so it would pass
	// every test above and show only here.
	if got := len(out.Funcs); got != len(declaredFuncSyms(out))+3 {
		t.Errorf("the package holds %d functions and it owes its own plus the three methods", got)
	}
}

// declaredFuncSyms is the functions of the package that are not methods of a
// foreign instantiation.
func declaredFuncSyms(p *Package) []string {
	var out []string
	for _, fn := range p.Funcs {
		if !strings.Contains(fn.Sym, "sync/atomic.(*Pointer[") {
			out = append(out, fn.Sym)
		}
	}
	return out
}

// askedFor reports whether the source was asked for the declaration.
func askedFor(list []string, name string) bool {
	for _, v := range list {
		if v == name {
			return true
		}
	}
	return false
}

// definesSym reports whether the package defines a function with the symbol.
func definesSym(p *Package, sym string) bool {
	for _, fn := range p.Funcs {
		if fn.Sym == sym {
			return true
		}
	}
	return false
}
