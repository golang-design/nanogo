// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"strings"
	"testing"
)

func lit(v string, k LitKind) *BasicLit { return &BasicLit{Value: v, Kind: k} }

func TestPrintExpressions(t *testing.T) {
	for _, tc := range []struct {
		what string
		n    Node
		want string
	}{
		{"name", name("x"), "x"},
		{"literal", lit("42", IntLit), "42"},
		{"bad", &BadExpr{}, "<bad expr>"},
		{"paren", &ParenExpr{X: name("x")}, "(x)"},
		{"selector", &SelectorExpr{X: name("p"), Sel: name("F")}, "p.F"},
		{"index", &IndexExpr{X: name("a"), Index: name("i")}, "a[i]"},
		{"instantiation", &IndexExpr{X: name("f"), Index: &ListExpr{ElemList: []Expr{name("int"), name("string")}}}, "f[int, string]"},
		{"slice", &SliceExpr{X: name("a"), Index: [3]Expr{name("i"), name("j")}}, "a[i:j]"},
		{"slice open", &SliceExpr{X: name("a")}, "a[:]"},
		{"slice full", &SliceExpr{X: name("a"), Index: [3]Expr{name("i"), name("j"), name("k")}, Full: true}, "a[i:j:k]"},
		{"assert", &AssertExpr{X: name("x"), Type: name("T")}, "x.(T)"},
		{"guard", &TypeSwitchGuard{Lhs: name("v"), X: name("x")}, "v := x.(type)"},
		{"guard no lhs", &TypeSwitchGuard{X: name("x")}, "x.(type)"},
		{"unary", &Operation{Op: Not, X: name("b")}, "!b"},
		{"binary", &Operation{Op: Add, X: name("a"), Y: name("b")}, "a + b"},
		{"array", &ArrayType{Len: lit("3", IntLit), Elem: name("int")}, "[3]int"},
		{"array implicit", &ArrayType{Elem: name("int")}, "[...]int"},
		{"slice type", &SliceType{Elem: name("byte")}, "[]byte"},
		{"dots", &DotsType{Elem: name("int")}, "...int"},
		{"map", &MapType{Key: name("K"), Value: name("V")}, "map[K]V"},
		{"chan", &ChanType{Elem: name("T")}, "chan T"},
		{"chan send", &ChanType{Dir: SendOnly, Elem: name("T")}, "chan<- T"},
		{"chan recv", &ChanType{Dir: RecvOnly, Elem: name("T")}, "<-chan T"},
		{"empty struct", &StructType{}, "struct{}"},
		{"empty interface", &InterfaceType{}, "interface{}"},
		{"func no result", &FuncType{ParamList: []*Field{{Name: name("a"), Type: name("int")}}}, "func(a int)"},
		{"func one result", &FuncType{ResultList: []*Field{{Type: name("error")}}}, "func() error"},
		{"func named result", &FuncType{ResultList: []*Field{{Name: name("n"), Type: name("int")}}}, "func() (n int)"},
		{"func two results", &FuncType{ResultList: []*Field{{Type: name("int")}, {Type: name("error")}}}, "func() (int, error)"},
	} {
		if got := String(tc.n); got != tc.want {
			t.Errorf("%s: printed %q, want %q", tc.what, got, tc.want)
		}
	}
}

// TestShortFormAbbreviates is the property the type checker depends on. An
// error message names an expression so the reader recognises it, and quoting a
// whole composite literal instead makes the message unreadable.
func TestShortFormAbbreviates(t *testing.T) {
	call := &CallExpr{Fun: name("f"), ArgList: []Expr{name("a"), name("b")}}
	if got, want := String(call), "f(…)"; got != want {
		t.Errorf("short form of a call is %q, want %q", got, want)
	}

	var buf strings.Builder
	if _, err := Fprint(&buf, call, FullForm); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if got, want := buf.String(), "f(a, b)"; got != want {
		t.Errorf("full form of a call is %q, want %q", got, want)
	}

	cl := &CompositeLit{Type: name("T"), ElemList: []Expr{name("x")}}
	if got, want := String(cl), "T{…}"; got != want {
		t.Errorf("short form of a composite literal is %q, want %q", got, want)
	}
	buf.Reset()
	Fprint(&buf, cl, FullForm)
	if got, want := buf.String(), "T{x}"; got != want {
		t.Errorf("full form of a composite literal is %q, want %q", got, want)
	}

	st := &StructType{FieldList: []*Field{{Name: name("A"), Type: name("int")}}}
	if got, want := String(st), "struct{…}"; got != want {
		t.Errorf("short form of a struct is %q, want %q", got, want)
	}
	buf.Reset()
	Fprint(&buf, st, FullForm)
	if got, want := buf.String(), "struct{A int}"; got != want {
		t.Errorf("full form of a struct is %q, want %q", got, want)
	}

	it := &InterfaceType{MethodList: []*Field{{Type: name("error")}}}
	buf.Reset()
	Fprint(&buf, it, FullForm)
	if got, want := buf.String(), "interface{error}"; got != want {
		t.Errorf("full form of an interface is %q, want %q", got, want)
	}
}

func TestPrintCallWithDots(t *testing.T) {
	var buf strings.Builder
	Fprint(&buf, &CallExpr{Fun: name("f"), ArgList: []Expr{name("xs")}, HasDots: true}, FullForm)
	if got, want := buf.String(), "f(xs...)"; got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
}

func TestPrintStatements(t *testing.T) {
	for _, tc := range []struct {
		what string
		n    Node
		want string
	}{
		{"empty", &EmptyStmt{}, ";"},
		{"bad", &BadStmt{}, "<bad stmt>"},
		{"expr", &ExprStmt{X: name("f")}, "f"},
		{"send", &SendStmt{Chan: name("c"), Value: name("v")}, "c <- v"},
		{"assign", &AssignStmt{Lhs: name("a"), Rhs: name("b")}, "a = b"},
		{"define", &AssignStmt{Op: Def, Lhs: name("a"), Rhs: name("b")}, "a := b"},
		{"op assign", &AssignStmt{Op: Add, Lhs: name("a"), Rhs: name("b")}, "a += b"},
		{"increment", &AssignStmt{Op: Add, Lhs: name("i"), Rhs: ImplicitOne}, "i++"},
		{"decrement", &AssignStmt{Op: Sub, Lhs: name("i"), Rhs: ImplicitOne}, "i--"},
		{"bare return", &ReturnStmt{}, "return"},
		{"return value", &ReturnStmt{Results: name("x")}, "return x"},
		{"break", &BranchStmt{Tok: Break}, "break"},
		{"goto label", &BranchStmt{Tok: Goto, Label: name("done")}, "goto done"},
		{"defer", &CallStmt{Tok: Defer, Call: &CallExpr{Fun: name("f")}}, "defer f()"},
		{"go", &CallStmt{Tok: Go, Call: &CallExpr{Fun: name("f")}}, "go f()"},
		{"block", &BlockStmt{}, "{…}"},
		{"labeled", &LabeledStmt{Label: name("L"), Stmt: &EmptyStmt{}}, "L: ;"},
		{"if", &IfStmt{}, "if …"},
		{"for", &ForStmt{}, "for …"},
		{"switch", &SwitchStmt{}, "switch …"},
		{"select", &SelectStmt{}, "select …"},
		{"range", &RangeClause{Lhs: name("i"), Def: true, X: name("xs")}, "i := range xs"},
		{"range assign", &RangeClause{Lhs: name("i"), X: name("xs")}, "i = range xs"},
		{"range bare", &RangeClause{X: name("xs")}, "range xs"},
		{"decl", &DeclStmt{}, "<declaration>"},
		{"case", &CaseClause{Cases: name("x")}, "case x:"},
		{"default case", &CaseClause{}, "default:"},
		{"comm", &CommClause{Comm: &ExprStmt{X: name("x")}}, "case x:"},
		{"default comm", &CommClause{}, "default:"},
	} {
		if got := String(tc.n); got != tc.want {
			t.Errorf("%s: printed %q, want %q", tc.what, got, tc.want)
		}
	}
}

func TestPrintDeclarations(t *testing.T) {
	for _, tc := range []struct {
		what string
		n    Node
		want string
	}{
		{"import", &ImportDecl{Path: lit(`"fmt"`, StringLit)}, `import "fmt"`},
		{"named import", &ImportDecl{LocalPkgName: name("f"), Path: lit(`"fmt"`, StringLit)}, `import f "fmt"`},
		{"const", &ConstDecl{}, "const …"},
		{"var", &VarDecl{}, "var …"},
		{"type", &TypeDecl{Name: name("T")}, "type T"},
		{"func", &FuncDecl{Name: name("f")}, "func f"},
		{"method", &FuncDecl{Recv: &Field{Name: name("r"), Type: name("T")}, Name: name("m")}, "func (r T) m"},
		{"file", &File{PkgName: name("p")}, "package p"},
		{"func literal", &FuncLit{Type: &FuncType{}}, "func() {…}"},
	} {
		if got := String(tc.n); got != tc.want {
			t.Errorf("%s: printed %q, want %q", tc.what, got, tc.want)
		}
	}
}

func TestPrintNilAndTypedNil(t *testing.T) {
	if got := String(nil); got != "" {
		t.Errorf("printing nil produced %q", got)
	}
	var typed *Name
	if got := String(typed); got != "" {
		t.Errorf("printing a typed nil produced %q", got)
	}
}

// failWriter fails on the first write, so the printer's error path is covered.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errWrite }

var errWrite = errWriteType{}

type errWriteType struct{}

func (errWriteType) Error() string { return "write failed" }

func TestPrintStopsOnWriteError(t *testing.T) {
	n, err := Fprint(failWriter{}, &Operation{Op: Add, X: name("a"), Y: name("b")}, FullForm)
	if err == nil {
		t.Fatal("a failing writer did not produce an error")
	}
	if n != 0 {
		t.Errorf("reported %d bytes written to a failing writer", n)
	}
}

func TestPrintReportsBytesWritten(t *testing.T) {
	var buf strings.Builder
	n, err := Fprint(&buf, name("hello"), FullForm)
	if err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if n != len("hello") || buf.String() != "hello" {
		t.Errorf("wrote %d bytes %q", n, buf.String())
	}
}
