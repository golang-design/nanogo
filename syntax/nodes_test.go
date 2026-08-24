// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import "testing"

// allNodes returns one of every node type.
//
// The list is written out rather than generated, so that adding a node without
// adding it here is a visible omission. specs/011-parser-and-ast.md constrains
// this set to match the reference implementation's, because the type checker
// port of specs/012-type-checking.md names it directly.
func allNodes() []Node {
	return []Node{
		&File{},
		&ImportDecl{}, &ConstDecl{}, &TypeDecl{}, &VarDecl{}, &FuncDecl{},
		&BadExpr{}, &Name{}, &BasicLit{}, &CompositeLit{}, &KeyValueExpr{},
		&FuncLit{}, &ParenExpr{}, &SelectorExpr{}, &IndexExpr{}, &SliceExpr{},
		&AssertExpr{}, &TypeSwitchGuard{}, &Operation{}, &CallExpr{}, &ListExpr{},
		&ArrayType{}, &SliceType{}, &DotsType{}, &StructType{}, &InterfaceType{},
		&FuncType{}, &MapType{}, &ChanType{}, &Field{},
		&BadStmt{}, &EmptyStmt{}, &LabeledStmt{}, &BlockStmt{}, &ExprStmt{},
		&SendStmt{}, &DeclStmt{}, &AssignStmt{}, &BranchStmt{}, &CallStmt{},
		&ReturnStmt{}, &IfStmt{}, &ForStmt{}, &SwitchStmt{}, &SelectStmt{},
		&RangeClause{}, &CaseClause{}, &CommClause{},
	}
}

func TestEveryNodeCarriesAPosition(t *testing.T) {
	for _, n := range allNodes() {
		if n.Pos() != NoPos {
			t.Errorf("%T starts with a position", n)
		}
		n.SetPos(Pos(42))
		if n.Pos() != Pos(42) {
			t.Errorf("%T did not keep the position it was given", n)
		}
	}
}

// TestNodeCategories checks that each node implements the interfaces it should
// and none that it should not. A node in the wrong category is accepted by a
// production that must reject it, which turns a syntax error into a tree that
// the type checker has to defend against.
func TestNodeCategories(t *testing.T) {
	isExpr := map[string]bool{}
	for _, n := range []Node{
		&BadExpr{}, &Name{}, &BasicLit{}, &CompositeLit{}, &KeyValueExpr{},
		&FuncLit{}, &ParenExpr{}, &SelectorExpr{}, &IndexExpr{}, &SliceExpr{},
		&AssertExpr{}, &TypeSwitchGuard{}, &Operation{}, &CallExpr{}, &ListExpr{},
		&ArrayType{}, &SliceType{}, &DotsType{}, &StructType{}, &InterfaceType{},
		&FuncType{}, &MapType{}, &ChanType{},
	} {
		if _, ok := n.(Expr); !ok {
			t.Errorf("%T is not an Expr", n)
		}
		if _, ok := n.(Stmt); ok {
			t.Errorf("%T is an Expr and also a Stmt", n)
		}
		isExpr[typeName(n)] = true
	}

	for _, n := range []Node{
		&ImportDecl{}, &ConstDecl{}, &TypeDecl{}, &VarDecl{}, &FuncDecl{},
	} {
		if _, ok := n.(Decl); !ok {
			t.Errorf("%T is not a Decl", n)
		}
	}

	for _, n := range []Node{
		&BadStmt{}, &EmptyStmt{}, &LabeledStmt{}, &BlockStmt{}, &ExprStmt{},
		&SendStmt{}, &DeclStmt{}, &AssignStmt{}, &BranchStmt{}, &CallStmt{},
		&ReturnStmt{}, &IfStmt{}, &ForStmt{}, &SwitchStmt{}, &SelectStmt{},
		&RangeClause{},
	} {
		if _, ok := n.(Stmt); !ok {
			t.Errorf("%T is not a Stmt", n)
		}
	}

	// A SimpleStmt may appear in a control clause header. A Stmt that is not
	// simple may not, and the parser relies on the interface to enforce it.
	for _, n := range []Node{
		&EmptyStmt{}, &ExprStmt{}, &SendStmt{}, &AssignStmt{}, &RangeClause{},
	} {
		if _, ok := n.(SimpleStmt); !ok {
			t.Errorf("%T is not a SimpleStmt", n)
		}
	}
	for _, n := range []Node{
		&BlockStmt{}, &IfStmt{}, &ForStmt{}, &ReturnStmt{}, &SelectStmt{},
	} {
		if _, ok := n.(SimpleStmt); ok {
			t.Errorf("%T is a SimpleStmt and must not be", n)
		}
	}

	// CaseClause and CommClause are neither statements nor expressions. They
	// belong to their switch, and treating one as a statement would let it
	// escape into a block.
	for _, n := range []Node{&CaseClause{}, &CommClause{}, &Field{}, &File{}} {
		if _, ok := n.(Stmt); ok {
			t.Errorf("%T is a Stmt and must not be", n)
		}
		if _, ok := n.(Expr); ok {
			t.Errorf("%T is an Expr and must not be", n)
		}
		if _, ok := n.(Decl); ok {
			t.Errorf("%T is a Decl and must not be", n)
		}
	}
}

func typeName(n Node) string {
	type named interface{ String() string }
	if s, ok := n.(named); ok {
		return s.String()
	}
	return ""
}

func TestImplicitOneIsShared(t *testing.T) {
	// The increment and decrement forms share one node, matching the reference
	// implementation, so a walker can recognise them by identity rather than by
	// comparing the literal's text.
	if ImplicitOne == nil || ImplicitOne.Value != "1" || ImplicitOne.Kind != IntLit {
		t.Fatalf("ImplicitOne is %#v", ImplicitOne)
	}
	a := &AssignStmt{Op: Add, Rhs: ImplicitOne}
	b := &AssignStmt{Op: Sub, Rhs: ImplicitOne}
	if a.Rhs != b.Rhs {
		t.Error("two increment statements do not share ImplicitOne")
	}
}

func TestChanDir(t *testing.T) {
	// Zero means bidirectional, so neither direction may be the zero value.
	if SendOnly == 0 || RecvOnly == 0 || SendOnly == RecvOnly {
		t.Fatalf("SendOnly=%d RecvOnly=%d: zero must stay bidirectional", SendOnly, RecvOnly)
	}
	if (&ChanType{}).Dir != 0 {
		t.Error("a channel type does not default to bidirectional")
	}
}
