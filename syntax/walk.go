// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

// Tree traversal.
//
// The type checker of specs/012-type-checking.md calls Inspect in seven places,
// so it is provided here with the reference implementation's name and
// behaviour.
//
// Children are visited in source order. That is not a convenience: a consumer
// that reports the first error in a subtree depends on it, and so does the
// position invariant that specs/011-parser-and-ast.md's tests assert.

// Visitor is called for each node during a Walk.
//
// Visit returns the visitor to use for the node's children, or nil to skip
// them.
type Visitor interface {
	Visit(node Node) Visitor
}

type inspector func(Node) bool

func (f inspector) Visit(node Node) Visitor {
	if f(node) {
		return f
	}
	return nil
}

// Inspect calls f for each node in the tree rooted at root, in source order.
//
// f returns false to skip the node's children.
func Inspect(root Node, f func(Node) bool) {
	Walk(root, inspector(f))
}

// Walk traverses the tree rooted at root in source order.
func Walk(root Node, v Visitor) {
	if root == nil || v == nil {
		return
	}
	walk(root, v)
}

func walk(n Node, v Visitor) {
	if n == nil || isNilNode(n) {
		return
	}
	v = v.Visit(n)
	if v == nil {
		return
	}
	walkChildren(n, v)
}

// isNilNode reports whether n is a nil pointer held in a non-nil interface.
//
// A tree is built with typed nil fields all over it: an ImportDecl with no
// local name, an IfStmt with no else. Walking one would call Visit with a node
// whose methods dereference nil, so the check belongs here rather than in every
// caller.
func isNilNode(n Node) bool {
	switch n := n.(type) {
	case *File:
		return n == nil
	case *ImportDecl:
		return n == nil
	case *ConstDecl:
		return n == nil
	case *TypeDecl:
		return n == nil
	case *VarDecl:
		return n == nil
	case *FuncDecl:
		return n == nil
	case *BadExpr:
		return n == nil
	case *Name:
		return n == nil
	case *BasicLit:
		return n == nil
	case *CompositeLit:
		return n == nil
	case *KeyValueExpr:
		return n == nil
	case *FuncLit:
		return n == nil
	case *ParenExpr:
		return n == nil
	case *SelectorExpr:
		return n == nil
	case *IndexExpr:
		return n == nil
	case *SliceExpr:
		return n == nil
	case *AssertExpr:
		return n == nil
	case *TypeSwitchGuard:
		return n == nil
	case *Operation:
		return n == nil
	case *CallExpr:
		return n == nil
	case *ListExpr:
		return n == nil
	case *ArrayType:
		return n == nil
	case *SliceType:
		return n == nil
	case *DotsType:
		return n == nil
	case *StructType:
		return n == nil
	case *InterfaceType:
		return n == nil
	case *FuncType:
		return n == nil
	case *MapType:
		return n == nil
	case *ChanType:
		return n == nil
	case *Field:
		return n == nil
	case *BadStmt:
		return n == nil
	case *EmptyStmt:
		return n == nil
	case *LabeledStmt:
		return n == nil
	case *BlockStmt:
		return n == nil
	case *ExprStmt:
		return n == nil
	case *SendStmt:
		return n == nil
	case *DeclStmt:
		return n == nil
	case *AssignStmt:
		return n == nil
	case *BranchStmt:
		return n == nil
	case *CallStmt:
		return n == nil
	case *ReturnStmt:
		return n == nil
	case *IfStmt:
		return n == nil
	case *ForStmt:
		return n == nil
	case *SwitchStmt:
		return n == nil
	case *SelectStmt:
		return n == nil
	case *RangeClause:
		return n == nil
	case *CaseClause:
		return n == nil
	case *CommClause:
		return n == nil
	}
	return false
}

func walkExprs(list []Expr, v Visitor) {
	for _, x := range list {
		if x != nil {
			walk(x, v)
		}
	}
}

func walkStmts(list []Stmt, v Visitor) {
	for _, s := range list {
		if s != nil {
			walk(s, v)
		}
	}
}

func walkFields(list []*Field, v Visitor) {
	for _, f := range list {
		if f != nil {
			walk(f, v)
		}
	}
}

func walkChildren(n Node, v Visitor) {
	switch n := n.(type) {
	case *File:
		if n.PkgName != nil {
			walk(n.PkgName, v)
		}
		for _, d := range n.DeclList {
			if d != nil {
				walk(d, v)
			}
		}

	case *ImportDecl:
		if n.LocalPkgName != nil {
			walk(n.LocalPkgName, v)
		}
		if n.Path != nil {
			walk(n.Path, v)
		}

	case *ConstDecl:
		for _, name := range n.NameList {
			walk(name, v)
		}
		if n.Type != nil {
			walk(n.Type, v)
		}
		if n.Values != nil {
			walk(n.Values, v)
		}

	case *TypeDecl:
		if n.Name != nil {
			walk(n.Name, v)
		}
		walkFields(n.TParamList, v)
		if n.Type != nil {
			walk(n.Type, v)
		}

	case *VarDecl:
		for _, name := range n.NameList {
			walk(name, v)
		}
		if n.Type != nil {
			walk(n.Type, v)
		}
		if n.Values != nil {
			walk(n.Values, v)
		}

	case *FuncDecl:
		if n.Recv != nil {
			walk(n.Recv, v)
		}
		if n.Name != nil {
			walk(n.Name, v)
		}
		walkFields(n.TParamList, v)
		if n.Type != nil {
			walk(n.Type, v)
		}
		if n.Body != nil {
			walk(n.Body, v)
		}

	case *BadExpr, *Name, *BasicLit:
		// No children.

	case *CompositeLit:
		if n.Type != nil {
			walk(n.Type, v)
		}
		walkExprs(n.ElemList, v)

	case *KeyValueExpr:
		walk(n.Key, v)
		walk(n.Value, v)

	case *FuncLit:
		if n.Type != nil {
			walk(n.Type, v)
		}
		if n.Body != nil {
			walk(n.Body, v)
		}

	case *ParenExpr:
		walk(n.X, v)

	case *SelectorExpr:
		walk(n.X, v)
		if n.Sel != nil {
			walk(n.Sel, v)
		}

	case *IndexExpr:
		walk(n.X, v)
		if n.Index != nil {
			walk(n.Index, v)
		}

	case *SliceExpr:
		walk(n.X, v)
		for _, x := range n.Index {
			if x != nil {
				walk(x, v)
			}
		}

	case *AssertExpr:
		walk(n.X, v)
		if n.Type != nil {
			walk(n.Type, v)
		}

	case *TypeSwitchGuard:
		if n.Lhs != nil {
			walk(n.Lhs, v)
		}
		walk(n.X, v)

	case *Operation:
		walk(n.X, v)
		if n.Y != nil {
			walk(n.Y, v)
		}

	case *CallExpr:
		walk(n.Fun, v)
		walkExprs(n.ArgList, v)

	case *ListExpr:
		walkExprs(n.ElemList, v)

	case *ArrayType:
		if n.Len != nil {
			walk(n.Len, v)
		}
		walk(n.Elem, v)

	case *SliceType:
		walk(n.Elem, v)

	case *DotsType:
		walk(n.Elem, v)

	case *StructType:
		walkFields(n.FieldList, v)
		for _, tag := range n.TagList {
			if tag != nil {
				walk(tag, v)
			}
		}

	case *InterfaceType:
		walkFields(n.MethodList, v)

	case *FuncType:
		walkFields(n.ParamList, v)
		walkFields(n.ResultList, v)

	case *MapType:
		walk(n.Key, v)
		walk(n.Value, v)

	case *ChanType:
		walk(n.Elem, v)

	case *Field:
		if n.Name != nil {
			walk(n.Name, v)
		}
		if n.Type != nil {
			walk(n.Type, v)
		}

	case *BadStmt, *EmptyStmt:
		// No children.

	case *LabeledStmt:
		if n.Label != nil {
			walk(n.Label, v)
		}
		if n.Stmt != nil {
			walk(n.Stmt, v)
		}

	case *BlockStmt:
		walkStmts(n.List, v)

	case *ExprStmt:
		walk(n.X, v)

	case *SendStmt:
		walk(n.Chan, v)
		walk(n.Value, v)

	case *DeclStmt:
		for _, d := range n.DeclList {
			if d != nil {
				walk(d, v)
			}
		}

	case *AssignStmt:
		if n.Lhs != nil {
			walk(n.Lhs, v)
		}
		// ImplicitOne is shared across every increment statement in the
		// program, so walking it would visit one node many times and would let
		// a visitor that mutates positions corrupt every other increment.
		if n.Rhs != nil && n.Rhs != Expr(ImplicitOne) {
			walk(n.Rhs, v)
		}

	case *BranchStmt:
		if n.Label != nil {
			walk(n.Label, v)
		}
		// Target is a back reference the type checker fills in. Walking it
		// would revisit a statement that is reached through the tree already,
		// and for a backward goto it would not terminate.

	case *CallStmt:
		walk(n.Call, v)

	case *ReturnStmt:
		if n.Results != nil {
			walk(n.Results, v)
		}

	case *IfStmt:
		if n.Init != nil {
			walk(n.Init, v)
		}
		if n.Cond != nil {
			walk(n.Cond, v)
		}
		if n.Then != nil {
			walk(n.Then, v)
		}
		if n.Else != nil {
			walk(n.Else, v)
		}

	case *ForStmt:
		if n.Init != nil {
			walk(n.Init, v)
		}
		if n.Cond != nil {
			walk(n.Cond, v)
		}
		if n.Post != nil {
			walk(n.Post, v)
		}
		if n.Body != nil {
			walk(n.Body, v)
		}

	case *SwitchStmt:
		if n.Init != nil {
			walk(n.Init, v)
		}
		if n.Tag != nil {
			walk(n.Tag, v)
		}
		for _, c := range n.Body {
			if c != nil {
				walk(c, v)
			}
		}

	case *SelectStmt:
		for _, c := range n.Body {
			if c != nil {
				walk(c, v)
			}
		}

	case *RangeClause:
		if n.Lhs != nil {
			walk(n.Lhs, v)
		}
		walk(n.X, v)

	case *CaseClause:
		if n.Cases != nil {
			walk(n.Cases, v)
		}
		walkStmts(n.Body, v)

	case *CommClause:
		if n.Comm != nil {
			walk(n.Comm, v)
		}
		walkStmts(n.Body, v)
	}
}
