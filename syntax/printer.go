// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"fmt"
	"io"
	"strings"
)

// Printing the tree.
//
// This is a debugging and diagnostic surface, not a formatter. It does not
// reproduce the source: the tree of specs/011-parser-and-ast.md makes no
// fidelity promise, comments are gone, and nothing here tries to lay code out.
//
// It exists for two consumers. The type checker of
// specs/012-type-checking.md names an expression in an error message, which is
// the ShortForm case. A compiler author reading a parse is the other, which is
// the full form.

// Form selects how much of a node is printed.
type Form uint

const (
	// FullForm prints the whole subtree.
	FullForm Form = iota
	// ShortForm abbreviates a composite node's interior to "…", so that an
	// error message names an expression without quoting a page of it.
	ShortForm
)

// String returns the printed form of n, abbreviated.
//
// An error message that quotes an expression wants the expression to be
// recognisable, not complete, so this uses ShortForm. Fprint with FullForm is
// the way to see everything.
func String(n Node) string {
	var buf strings.Builder
	Fprint(&buf, n, ShortForm)
	return buf.String()
}

// Fprint writes n to w in the given form.
func Fprint(w io.Writer, n Node, form Form) (int, error) {
	p := printer{w: w, form: form}
	p.print(n)
	return p.written, p.err
}

type printer struct {
	w       io.Writer
	form    Form
	written int
	err     error
}

func (p *printer) str(s string) {
	if p.err != nil {
		return
	}
	n, err := io.WriteString(p.w, s)
	p.written += n
	p.err = err
}

func (p *printer) printf(format string, args ...any) {
	p.str(fmt.Sprintf(format, args...))
}

// short reports whether an interior should be abbreviated.
func (p *printer) short() bool { return p.form == ShortForm }

func (p *printer) printList(list []Expr, sep string) {
	for i, x := range list {
		if i > 0 {
			p.str(sep)
		}
		p.print(x)
	}
}

func (p *printer) printFields(list []*Field, sep string) {
	for i, f := range list {
		if i > 0 {
			p.str(sep)
		}
		if f == nil {
			continue
		}
		if f.Name != nil {
			p.print(f.Name)
			p.str(" ")
		}
		p.print(f.Type)
	}
}

func (p *printer) print(n Node) {
	if n == nil || isNilNode(n) {
		return
	}

	switch n := n.(type) {
	case *Name:
		p.str(n.Value)

	case *BasicLit:
		p.str(n.Value)

	case *BadExpr:
		p.str("<bad expr>")

	case *ParenExpr:
		p.str("(")
		p.print(n.X)
		p.str(")")

	case *SelectorExpr:
		p.print(n.X)
		p.str(".")
		p.print(n.Sel)

	case *IndexExpr:
		p.print(n.X)
		p.str("[")
		p.print(n.Index)
		p.str("]")

	case *SliceExpr:
		p.print(n.X)
		p.str("[")
		if n.Index[0] != nil {
			p.print(n.Index[0])
		}
		p.str(":")
		if n.Index[1] != nil {
			p.print(n.Index[1])
		}
		if n.Full {
			p.str(":")
			if n.Index[2] != nil {
				p.print(n.Index[2])
			}
		}
		p.str("]")

	case *AssertExpr:
		p.print(n.X)
		p.str(".(")
		p.print(n.Type)
		p.str(")")

	case *TypeSwitchGuard:
		if n.Lhs != nil {
			p.print(n.Lhs)
			p.str(" := ")
		}
		p.print(n.X)
		p.str(".(type)")

	case *Operation:
		if n.Y == nil {
			p.str(n.Op.String())
			p.print(n.X)
			return
		}
		p.print(n.X)
		p.printf(" %s ", n.Op)
		p.print(n.Y)

	case *CallExpr:
		p.print(n.Fun)
		p.str("(")
		if p.short() && len(n.ArgList) > 0 {
			p.str("…")
		} else {
			p.printList(n.ArgList, ", ")
			if n.HasDots {
				p.str("...")
			}
		}
		p.str(")")

	case *ListExpr:
		p.printList(n.ElemList, ", ")

	case *CompositeLit:
		if n.Type != nil {
			p.print(n.Type)
		}
		p.str("{")
		if len(n.ElemList) > 0 {
			if p.short() {
				p.str("…")
			} else {
				p.printList(n.ElemList, ", ")
			}
		}
		p.str("}")

	case *KeyValueExpr:
		p.print(n.Key)
		p.str(": ")
		p.print(n.Value)

	case *FuncLit:
		p.print(n.Type)
		p.str(" {…}")

	// Types.

	case *ArrayType:
		p.str("[")
		if n.Len != nil {
			p.print(n.Len)
		} else {
			p.str("...")
		}
		p.str("]")
		p.print(n.Elem)

	case *SliceType:
		p.str("[]")
		p.print(n.Elem)

	case *DotsType:
		p.str("...")
		p.print(n.Elem)

	case *StructType:
		p.str("struct{")
		if len(n.FieldList) > 0 {
			if p.short() {
				p.str("…")
			} else {
				p.printFields(n.FieldList, "; ")
			}
		}
		p.str("}")

	case *InterfaceType:
		p.str("interface{")
		if len(n.MethodList) > 0 {
			if p.short() {
				p.str("…")
			} else {
				p.printFields(n.MethodList, "; ")
			}
		}
		p.str("}")

	case *FuncType:
		p.str("func(")
		p.printFields(n.ParamList, ", ")
		p.str(")")
		switch len(n.ResultList) {
		case 0:
		case 1:
			if n.ResultList[0] != nil && n.ResultList[0].Name == nil {
				p.str(" ")
				p.print(n.ResultList[0].Type)
				return
			}
			fallthrough
		default:
			p.str(" (")
			p.printFields(n.ResultList, ", ")
			p.str(")")
		}

	case *MapType:
		p.str("map[")
		p.print(n.Key)
		p.str("]")
		p.print(n.Value)

	case *ChanType:
		switch n.Dir {
		case SendOnly:
			p.str("chan<- ")
		case RecvOnly:
			p.str("<-chan ")
		default:
			p.str("chan ")
		}
		p.print(n.Elem)

	case *Field:
		if n.Name != nil {
			p.print(n.Name)
			p.str(" ")
		}
		p.print(n.Type)

	// Statements. These print in an abbreviated form always: a statement in a
	// diagnostic is named, not quoted.

	case *EmptyStmt:
		p.str(";")

	case *BadStmt:
		p.str("<bad stmt>")

	case *ExprStmt:
		p.print(n.X)

	case *SendStmt:
		p.print(n.Chan)
		p.str(" <- ")
		p.print(n.Value)

	case *AssignStmt:
		p.print(n.Lhs)
		switch {
		case n.Op == Def:
			p.str(" := ")
		case n.Op == 0:
			p.str(" = ")
		case n.Rhs == Expr(ImplicitOne):
			// ++ and -- share one right-hand node, so recognise it by identity
			// rather than by reading the literal's text.
			p.printf("%s%s", n.Op, n.Op)
			return
		default:
			p.printf(" %s= ", n.Op)
		}
		p.print(n.Rhs)

	case *ReturnStmt:
		p.str("return")
		if n.Results != nil {
			p.str(" ")
			p.print(n.Results)
		}

	case *BranchStmt:
		p.str(n.Tok.String())
		if n.Label != nil {
			p.str(" ")
			p.print(n.Label)
		}

	case *CallStmt:
		p.str(n.Tok.String())
		p.str(" ")
		p.print(n.Call)

	case *BlockStmt:
		p.str("{…}")

	case *LabeledStmt:
		p.print(n.Label)
		p.str(": ")
		p.print(n.Stmt)

	case *IfStmt:
		p.str("if …")

	case *ForStmt:
		p.str("for …")

	case *SwitchStmt:
		p.str("switch …")

	case *SelectStmt:
		p.str("select …")

	case *RangeClause:
		if n.Lhs != nil {
			p.print(n.Lhs)
			if n.Def {
				p.str(" := ")
			} else {
				p.str(" = ")
			}
		}
		p.str("range ")
		p.print(n.X)

	case *DeclStmt:
		p.str("<declaration>")

	case *CaseClause:
		if n.Cases == nil {
			p.str("default:")
			return
		}
		p.str("case ")
		p.print(n.Cases)
		p.str(":")

	case *CommClause:
		if n.Comm == nil {
			p.str("default:")
			return
		}
		p.str("case ")
		p.print(n.Comm)
		p.str(":")

	// Declarations and the file.

	case *ImportDecl:
		p.str("import ")
		if n.LocalPkgName != nil {
			p.print(n.LocalPkgName)
			p.str(" ")
		}
		p.print(n.Path)

	case *ConstDecl:
		p.str("const …")

	case *VarDecl:
		p.str("var …")

	case *TypeDecl:
		p.str("type ")
		p.print(n.Name)

	case *FuncDecl:
		p.str("func ")
		if n.Recv != nil {
			p.str("(")
			p.print(n.Recv)
			p.str(") ")
		}
		p.print(n.Name)

	case *File:
		p.str("package ")
		p.print(n.PkgName)

	default:
		p.printf("<%T>", n)
	}
}
