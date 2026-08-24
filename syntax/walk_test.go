// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"fmt"
	"testing"
)

// name returns a name node, for building trees by hand.
func name(s string) *Name { return NewName(NoPos, s) }

func collect(root Node) []string {
	var got []string
	Inspect(root, func(n Node) bool {
		got = append(got, fmt.Sprintf("%T", n))
		return true
	})
	return got
}

func TestWalkVisitsChildrenInSourceOrder(t *testing.T) {
	// a.b + c, as the tree the parser would build.
	tree := &Operation{
		Op: Add,
		X:  &SelectorExpr{X: name("a"), Sel: name("b")},
		Y:  name("c"),
	}
	got := collect(tree)
	want := []string{
		"*syntax.Operation",
		"*syntax.SelectorExpr",
		"*syntax.Name", // a
		"*syntax.Name", // b
		"*syntax.Name", // c
	}
	if len(got) != len(want) {
		t.Fatalf("visited %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("visit %d is %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestWalkStopsWhenTheVisitorReturnsFalse(t *testing.T) {
	tree := &Operation{Op: Add, X: &ParenExpr{X: name("a")}, Y: name("b")}
	var got []string
	Inspect(tree, func(n Node) bool {
		got = append(got, fmt.Sprintf("%T", n))
		// Do not descend into the parenthesised operand.
		_, isParen := n.(*ParenExpr)
		return !isParen
	})
	for _, s := range got {
		_ = s
	}
	if len(got) != 3 {
		t.Fatalf("visited %v, want the operation, the paren and the second operand only", got)
	}
	if got[1] != "*syntax.ParenExpr" || got[2] != "*syntax.Name" {
		t.Fatalf("visited %v", got)
	}
}

// TestWalkSkipsTypedNilChildren guards the case that a tree is full of: an
// ImportDecl with no local name, an IfStmt with no else. A typed nil in a
// non-nil interface is not caught by a plain nil check.
func TestWalkSkipsTypedNilChildren(t *testing.T) {
	var missing *Name
	tree := &ImportDecl{LocalPkgName: missing, Path: &BasicLit{Value: `"fmt"`, Kind: StringLit}}
	got := collect(tree)
	if len(got) != 2 {
		t.Fatalf("visited %v, want the declaration and the path only", got)
	}

	// The same through an interface-typed field.
	var noElse Stmt
	iff := &IfStmt{Cond: name("c"), Then: &BlockStmt{}, Else: noElse}
	if got := collect(iff); len(got) != 3 {
		t.Fatalf("visited %v, want the if, its condition and its block", got)
	}
}

// TestWalkDoesNotFollowBranchTargets is the termination property. Target is a
// back reference the type checker fills in, and a backward goto would make the
// walk loop.
func TestWalkDoesNotFollowBranchTargets(t *testing.T) {
	block := &BlockStmt{}
	br := &BranchStmt{Tok: Goto, Label: name("loop"), Target: block}
	block.List = []Stmt{br}

	done := make(chan []string, 1)
	go func() { done <- collect(block) }()
	select {
	case got := <-done:
		if len(got) != 3 {
			t.Fatalf("visited %v, want the block, the branch and its label", got)
		}
	case <-timeout():
		t.Fatal("the walk did not terminate: it followed BranchStmt.Target")
	}
}

// TestWalkDoesNotVisitImplicitOne pins the reason walk.go special-cases it.
// One node is shared by every increment in the program, so visiting it would
// report the same node many times and let a mutating visitor corrupt them all.
func TestWalkDoesNotVisitImplicitOne(t *testing.T) {
	inc := &AssignStmt{Op: Add, Lhs: name("i"), Rhs: ImplicitOne}
	got := collect(inc)
	if len(got) != 2 {
		t.Fatalf("visited %v, want the statement and its left side only", got)
	}
	// An ordinary assignment still walks its right side.
	asn := &AssignStmt{Lhs: name("i"), Rhs: name("j")}
	if got := collect(asn); len(got) != 3 {
		t.Fatalf("visited %v, want the statement and both sides", got)
	}
}

func TestWalkHandlesEveryNode(t *testing.T) {
	// Every node type must be walkable without panicking, and a node whose
	// children are all absent must visit exactly itself. A node missing from
	// walkChildren silently truncates every traversal that reaches it, which is
	// the failure this test exists to prevent.
	for _, n := range allNodes() {
		got := collect(n)
		if len(got) != 1 {
			t.Errorf("%T visited %d nodes when empty, want 1: %v", n, len(got), got)
		}
	}
}

func TestWalkNilRootAndNilVisitor(t *testing.T) {
	Walk(nil, inspector(func(Node) bool { return true })) // must not panic
	Walk(name("a"), nil)                                  // must not panic
	var typed *Name
	Walk(typed, inspector(func(Node) bool {
		t.Error("a typed nil root was visited")
		return true
	}))
}

// TestWalkDescendsThroughEveryCompositeNode builds one tree holding a child in
// every composite field, so a field omitted from walkChildren shows up as a
// missing visit rather than as nothing at all.
func TestWalkDescendsThroughEveryCompositeNode(t *testing.T) {
	marker := name("MARKER")
	cases := []struct {
		what string
		root Node
	}{
		{"File.PkgName", &File{PkgName: marker}},
		{"File.DeclList", &File{DeclList: []Decl{&VarDecl{NameList: []*Name{marker}}}}},
		{"ConstDecl.Type", &ConstDecl{Type: marker}},
		{"ConstDecl.Values", &ConstDecl{Values: marker}},
		{"TypeDecl.TParamList", &TypeDecl{TParamList: []*Field{{Type: marker}}}},
		{"TypeDecl.Type", &TypeDecl{Type: marker}},
		{"VarDecl.Type", &VarDecl{Type: marker}},
		{"FuncDecl.Recv", &FuncDecl{Recv: &Field{Type: marker}}},
		{"FuncDecl.Body", &FuncDecl{Body: &BlockStmt{List: []Stmt{&ExprStmt{X: marker}}}}},
		{"CompositeLit.Type", &CompositeLit{Type: marker}},
		{"CompositeLit.ElemList", &CompositeLit{ElemList: []Expr{marker}}},
		{"KeyValueExpr.Key", &KeyValueExpr{Key: marker, Value: name("v")}},
		{"KeyValueExpr.Value", &KeyValueExpr{Key: name("k"), Value: marker}},
		{"FuncLit.Type", &FuncLit{Type: &FuncType{ParamList: []*Field{{Type: marker}}}}},
		{"ParenExpr.X", &ParenExpr{X: marker}},
		{"SelectorExpr.Sel", &SelectorExpr{X: name("x"), Sel: marker}},
		{"IndexExpr.Index", &IndexExpr{X: name("x"), Index: marker}},
		{"SliceExpr.Index", &SliceExpr{X: name("x"), Index: [3]Expr{nil, marker, nil}}},
		{"AssertExpr.Type", &AssertExpr{X: name("x"), Type: marker}},
		{"TypeSwitchGuard.Lhs", &TypeSwitchGuard{Lhs: marker, X: name("x")}},
		{"TypeSwitchGuard.X", &TypeSwitchGuard{X: marker}},
		{"Operation.Y", &Operation{Op: Add, X: name("x"), Y: marker}},
		{"CallExpr.ArgList", &CallExpr{Fun: name("f"), ArgList: []Expr{marker}}},
		{"ListExpr.ElemList", &ListExpr{ElemList: []Expr{marker}}},
		{"ArrayType.Len", &ArrayType{Len: marker, Elem: name("T")}},
		{"ArrayType.Elem", &ArrayType{Elem: marker}},
		{"SliceType.Elem", &SliceType{Elem: marker}},
		{"DotsType.Elem", &DotsType{Elem: marker}},
		{"StructType.FieldList", &StructType{FieldList: []*Field{{Type: marker}}}},
		{"InterfaceType.MethodList", &InterfaceType{MethodList: []*Field{{Type: marker}}}},
		{"FuncType.ResultList", &FuncType{ResultList: []*Field{{Type: marker}}}},
		{"MapType.Key", &MapType{Key: marker, Value: name("V")}},
		{"MapType.Value", &MapType{Key: name("K"), Value: marker}},
		{"ChanType.Elem", &ChanType{Elem: marker}},
		{"Field.Name", &Field{Name: marker}},
		{"LabeledStmt.Label", &LabeledStmt{Label: marker, Stmt: &EmptyStmt{}}},
		{"LabeledStmt.Stmt", &LabeledStmt{Label: name("l"), Stmt: &ExprStmt{X: marker}}},
		{"BlockStmt.List", &BlockStmt{List: []Stmt{&ExprStmt{X: marker}}}},
		{"SendStmt.Chan", &SendStmt{Chan: marker, Value: name("v")}},
		{"SendStmt.Value", &SendStmt{Chan: name("c"), Value: marker}},
		{"DeclStmt.DeclList", &DeclStmt{DeclList: []Decl{&VarDecl{Type: marker}}}},
		{"BranchStmt.Label", &BranchStmt{Tok: Goto, Label: marker}},
		{"CallStmt.Call", &CallStmt{Tok: Defer, Call: marker}},
		{"ReturnStmt.Results", &ReturnStmt{Results: marker}},
		{"IfStmt.Init", &IfStmt{Init: &ExprStmt{X: marker}, Cond: name("c"), Then: &BlockStmt{}}},
		{"IfStmt.Else", &IfStmt{Cond: name("c"), Then: &BlockStmt{}, Else: &BlockStmt{List: []Stmt{&ExprStmt{X: marker}}}}},
		{"ForStmt.Post", &ForStmt{Post: &ExprStmt{X: marker}, Body: &BlockStmt{}}},
		{"ForStmt.Body", &ForStmt{Body: &BlockStmt{List: []Stmt{&ExprStmt{X: marker}}}}},
		{"SwitchStmt.Tag", &SwitchStmt{Tag: marker}},
		{"SwitchStmt.Body", &SwitchStmt{Body: []*CaseClause{{Cases: marker}}}},
		{"SelectStmt.Body", &SelectStmt{Body: []*CommClause{{Comm: &ExprStmt{X: marker}}}}},
		{"RangeClause.Lhs", &RangeClause{Lhs: marker, X: name("x")}},
		{"RangeClause.X", &RangeClause{X: marker}},
		{"CaseClause.Body", &CaseClause{Body: []Stmt{&ExprStmt{X: marker}}}},
		{"CommClause.Body", &CommClause{Body: []Stmt{&ExprStmt{X: marker}}}},
	}

	// A struct's tags are *BasicLit, so the *Name marker cannot stand in for
	// one. The tag node itself is the marker instead.
	tag := &BasicLit{Value: `"t"`, Kind: StringLit}
	foundTag := false
	Inspect(&StructType{FieldList: []*Field{{Type: name("A")}}, TagList: []*BasicLit{tag}},
		func(n Node) bool {
			if n == Node(tag) {
				foundTag = true
			}
			return true
		})
	if !foundTag {
		t.Error("StructType.TagList: the walk did not reach the tag")
	}

	for _, tc := range cases {
		found := false
		Inspect(tc.root, func(n Node) bool {
			if n == Node(marker) {
				found = true
			}
			return true
		})
		if !found {
			t.Errorf("%s: the walk did not reach the child", tc.what)
		}
	}
}
