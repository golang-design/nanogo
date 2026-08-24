// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package syntax reads Go source text and returns a syntax tree.
//
// The package holds the scanner, the source position model, and the parser.
// It does no name resolution and no type checking: a tree is a record of what
// the source says, not of what it means. specs/012-type-checking.md owns the
// meaning.
//
// The node set follows cmd/compile/internal/syntax. That is a constraint and
// not a coincidence; see the comment at the top of nodes.go.
//
// See specs/010-scanner-and-positions.md and specs/011-parser-and-ast.md.
package syntax

import "os"

// Parse reads one Go source file from src and returns its syntax tree.
//
// src must be the whole contents of file. Parse reports every error it finds
// through errh and returns the first one, so a caller that wants all of them
// collects them in the handler. The tree is returned even when there are
// errors, with a Bad node in place of each construct the parser could not
// read; it is nil only when the file has no usable package clause.
//
// pragh receives each //go: directive. The parser attaches the value it
// returns to the declaration that follows.
func Parse(file *SrcFile, src []byte, errh ErrorHandler, pragh PragmaHandler, mode Mode) (*File, error) {
	var p parser
	p.init(file, src, errh, pragh, mode)
	f := p.fileOrNil()
	if f == nil {
		return nil, p.first
	}
	return f, p.first
}

// ParseFile reads the named file from disk, adds it to fset, and parses it.
//
// A file that cannot be read is reported through errh as well as returned, so
// that a caller which only watches the handler still sees it.
func ParseFile(fset *FileSet, filename string, errh ErrorHandler, pragh PragmaHandler, mode Mode) (*File, error) {
	src, err := os.ReadFile(filename)
	if err != nil {
		if errh != nil {
			errh(Error{Pos: NoPos, Msg: err.Error()})
		}
		return nil, err
	}
	return Parse(fset.AddFile(filename, len(src)), src, errh, pragh, mode)
}

// NewName returns a *Name at pos.
//
// The parser synthesises a name for a construct it could not read, so that the
// tree stays walkable, and the type checker builds names for the same reason.
func NewName(pos Pos, value string) *Name {
	n := new(Name)
	n.pos = pos
	n.Value = value
	return n
}

// Unparen returns x with every enclosing ParenExpr stripped.
func Unparen(x Expr) Expr {
	for {
		p, ok := x.(*ParenExpr)
		if !ok {
			return x
		}
		x = p.X
	}
}

// UnpackListExpr returns the elements of x, which is one expression, a
// *ListExpr, or nil.
func UnpackListExpr(x Expr) []Expr {
	switch x := x.(type) {
	case nil:
		return nil
	case *ListExpr:
		return x.ElemList
	default:
		return []Expr{x}
	}
}

// StartPos returns the position of the first byte of n in the source.
//
// It is not n.Pos(). A node's own position is the position that identifies it,
// which for a binary operation is the operator and for a call is the "(", and
// both of those are inside the node rather than at its start. Scope ranges and
// the parser's error positions need the start.
func StartPos(n Node) Pos {
	for m := n; ; {
		switch n := m.(type) {
		case nil:
			return NoPos

		// The file block starts at the package clause. The byte offset of the
		// start of the file is not recoverable from a Pos without the FileSet,
		// and no caller needs the difference.
		case *File:
			return n.Pos()

		case *CompositeLit:
			if n.Type == nil {
				return n.Pos()
			}
			m = n.Type
		case *KeyValueExpr:
			m = n.Key
		case *SelectorExpr:
			m = n.X
		case *IndexExpr:
			m = n.X
		case *SliceExpr:
			m = n.X
		case *AssertExpr:
			m = n.X
		case *TypeSwitchGuard:
			if n.Lhs == nil {
				m = n.X
				continue
			}
			m = n.Lhs
		case *Operation:
			if n.Y == nil {
				return n.Pos() // unary: the operator is the first byte
			}
			m = n.X
		case *CallExpr:
			m = n.Fun
		case *ListExpr:
			if len(n.ElemList) == 0 {
				return n.Pos()
			}
			m = n.ElemList[0]
		case *SendStmt:
			m = n.Chan
		case *AssignStmt:
			m = n.Lhs
		case *ExprStmt:
			m = n.X
		case *LabeledStmt:
			m = n.Label
		case *RangeClause:
			// "for range x" has no left hand side, and then the clause starts
			// at the range keyword, which is the node's own position.
			if n.Lhs == nil {
				return n.Pos()
			}
			m = n.Lhs

		default:
			return n.Pos()
		}
	}
}

// EndPos returns the approximate position of the end of n in the source.
//
// Approximate: for a name or a literal it is the position one past the last
// byte, for a block it is the closing brace, and for a parenthesised
// expression it is the end of the expression rather than of the ")". It is
// enough to demarcate a scope, which is what it is for, and it is not enough
// to cut the node's text out of the source.
func EndPos(n Node) Pos {
	for m := n; ; {
		switch n := m.(type) {
		case nil:
			return NoPos

		case *File:
			return n.EOF

		case *ImportDecl:
			if n.Path == nil {
				return n.Pos()
			}
			m = n.Path
		case *ConstDecl:
			switch {
			case n.Values != nil:
				m = n.Values
			case n.Type != nil:
				m = n.Type
			case len(n.NameList) > 0:
				m = n.NameList[len(n.NameList)-1]
			default:
				return n.Pos()
			}
		case *TypeDecl:
			m = n.Type
		case *VarDecl:
			switch {
			case n.Values != nil:
				m = n.Values
			case n.Type != nil:
				m = n.Type
			case len(n.NameList) > 0:
				m = n.NameList[len(n.NameList)-1]
			default:
				return n.Pos()
			}
		case *FuncDecl:
			if n.Body != nil {
				m = n.Body
				continue
			}
			if n.Type == nil {
				return n.Pos()
			}
			m = n.Type

		case *Name:
			return n.Pos() + Pos(len(n.Value))
		case *BasicLit:
			return n.Pos() + Pos(len(n.Value))
		case *CompositeLit:
			return n.Rbrace
		case *KeyValueExpr:
			m = n.Value
		case *FuncLit:
			m = n.Body
		case *ParenExpr:
			m = n.X
		case *SelectorExpr:
			m = n.Sel
		case *IndexExpr:
			m = n.Index
		case *SliceExpr:
			m = n.X
			for i := len(n.Index) - 1; i >= 0; i-- {
				if x := n.Index[i]; x != nil {
					m = x
					break
				}
			}
		case *AssertExpr:
			m = n.Type
		case *TypeSwitchGuard:
			m = n.X
		case *Operation:
			if n.Y != nil {
				m = n.Y
				continue
			}
			m = n.X
		case *CallExpr:
			if len(n.ArgList) == 0 {
				m = n.Fun
				continue
			}
			m = n.ArgList[len(n.ArgList)-1]
		case *ListExpr:
			if len(n.ElemList) == 0 {
				return n.Pos()
			}
			m = n.ElemList[len(n.ElemList)-1]

		case *ArrayType:
			m = n.Elem
		case *SliceType:
			m = n.Elem
		case *DotsType:
			m = n.Elem
		case *StructType:
			// A tag follows the field it belongs to, so the last tag may be the
			// last thing in the struct. TagList runs parallel to FieldList
			// rather than inside it, which is why the two have to be compared
			// here rather than resolved by walking one node.
			end := NoPos
			if l := len(n.FieldList); l > 0 {
				end = EndPos(n.FieldList[l-1])
			}
			if l := len(n.TagList); l > 0 && n.TagList[l-1] != nil {
				if e := EndPos(n.TagList[l-1]); e > end {
					end = e
				}
			}
			if end == NoPos {
				return n.Pos()
			}
			return end
		case *Field:
			if n.Type != nil {
				m = n.Type
				continue
			}
			m = n.Name
		case *InterfaceType:
			if len(n.MethodList) == 0 {
				return n.Pos()
			}
			m = n.MethodList[len(n.MethodList)-1]
		case *FuncType:
			if len(n.ResultList) > 0 {
				m = n.ResultList[len(n.ResultList)-1]
				continue
			}
			if len(n.ParamList) > 0 {
				m = n.ParamList[len(n.ParamList)-1]
				continue
			}
			return n.Pos()
		case *MapType:
			m = n.Value
		case *ChanType:
			m = n.Elem

		case *LabeledStmt:
			m = n.Stmt
		case *BlockStmt:
			return n.Rbrace
		case *ExprStmt:
			m = n.X
		case *SendStmt:
			m = n.Value
		case *DeclStmt:
			if len(n.DeclList) == 0 {
				return n.Pos()
			}
			m = n.DeclList[len(n.DeclList)-1]
		case *AssignStmt:
			if n.Rhs == nil {
				return EndPos(n.Lhs)
			}
			// ImplicitOne is a shared node with no position of its own, so an
			// increment ends two bytes past its operand.
			if n.Rhs == ImplicitOne {
				return EndPos(n.Lhs) + 2
			}
			m = n.Rhs
		case *BranchStmt:
			if n.Label == nil {
				return n.Pos()
			}
			m = n.Label
		case *CallStmt:
			m = n.Call
		case *ReturnStmt:
			if n.Results == nil {
				return n.Pos()
			}
			m = n.Results
		case *IfStmt:
			if n.Else != nil {
				m = n.Else
				continue
			}
			m = n.Then
		case *ForStmt:
			m = n.Body
		case *SwitchStmt:
			return n.Rbrace
		case *SelectStmt:
			return n.Rbrace

		case *RangeClause:
			m = n.X
		case *CaseClause:
			if len(n.Body) == 0 {
				return n.Colon
			}
			m = n.Body[len(n.Body)-1]
		case *CommClause:
			if len(n.Body) == 0 {
				return n.Colon
			}
			m = n.Body[len(n.Body)-1]

		default:
			return n.Pos()
		}
	}
}
