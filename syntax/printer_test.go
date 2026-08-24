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

// TestShortFormAbbreviatesExactlyTwoConstructs pins the abbreviation set
// against upstream's.
//
// Upstream's ShortForm is documented as "print … for non-empty function or
// composite literal bodies", and that is the whole list. An earlier version of
// this printer also abbreviated call arguments, struct fields and interface
// methods. The type checker names every expression in every error message
// through this printer, so that made it print `cannot use f(…)` where the
// conformance corpus expects `cannot use f(x)`. It broke pattern matching in
// specs/004's errorcheck level, not merely appearance.
func TestShortFormAbbreviatesExactlyTwoConstructs(t *testing.T) {
	full := func(n Node) string {
		var buf strings.Builder
		if _, err := Fprint(&buf, n, FullForm); err != nil {
			t.Fatalf("Fprint: %v", err)
		}
		return buf.String()
	}

	// The two that do abbreviate.
	cl := &CompositeLit{Type: name("T"), ElemList: []Expr{name("x")}}
	if got, want := String(cl), "T{…}"; got != want {
		t.Errorf("short form of a composite literal is %q, want %q", got, want)
	}
	if got, want := full(cl), "T{x}"; got != want {
		t.Errorf("full form of a composite literal is %q, want %q", got, want)
	}
	if got, want := String(&CompositeLit{Type: name("T")}), "T{}"; got != want {
		t.Errorf("an empty composite literal is %q, want %q", got, want)
	}

	fl := &FuncLit{Type: &FuncType{}, Body: &BlockStmt{List: []Stmt{&EmptyStmt{}}}}
	if got, want := String(fl), "func() {…}"; got != want {
		t.Errorf("short form of a function literal is %q, want %q", got, want)
	}
	if got, want := String(&FuncLit{Type: &FuncType{}, Body: &BlockStmt{}}), "func() {}"; got != want {
		t.Errorf("an empty function literal is %q, want %q", got, want)
	}

	// The three that must not, in either form.
	for _, tc := range []struct {
		what string
		n    Node
		want string
	}{
		{"call", &CallExpr{Fun: name("f"), ArgList: []Expr{name("a"), name("b")}}, "f(a, b)"},
		{"struct", &StructType{FieldList: []*Field{{Name: name("A"), Type: name("int")}}}, "struct{A int}"},
		{"interface", &InterfaceType{MethodList: []*Field{{Type: name("error")}}}, "interface{error}"},
	} {
		if got := String(tc.n); got != tc.want {
			t.Errorf("short form of a %s is %q, want %q", tc.what, got, tc.want)
		}
		if got := full(tc.n); got != tc.want {
			t.Errorf("full form of a %s is %q, want %q", tc.what, got, tc.want)
		}
	}
}

// TestFieldGrouping pins the other printer rule the checker depends on.
// Upstream prints `func() (_, _ int)`; an ungrouped printer prints
// `func() (_ int, _ int)`, and the corpus matches against the first.
func TestFieldGrouping(t *testing.T) {
	intType := name("int")
	strType := name("string")

	for _, tc := range []struct {
		what string
		n    Node
		want string
	}{
		{
			"two results sharing a type",
			&FuncType{ResultList: []*Field{{Name: name("_"), Type: intType}, {Name: name("_"), Type: intType}}},
			"func() (_, _ int)",
		},
		{
			"two results with different types",
			&FuncType{ResultList: []*Field{{Name: name("a"), Type: intType}, {Name: name("b"), Type: strType}}},
			"func() (a int, b string)",
		},
		{
			"three params, two grouped",
			&FuncType{ParamList: []*Field{
				{Name: name("a"), Type: intType},
				{Name: name("b"), Type: intType},
				{Name: name("c"), Type: strType},
			}},
			"func(a, b int, c string)",
		},
		{
			"unnamed params are not grouped",
			&FuncType{ParamList: []*Field{{Type: intType}, {Type: intType}}},
			"func(int, int)",
		},
		{
			"struct fields sharing a type",
			&StructType{FieldList: []*Field{{Name: name("A"), Type: intType}, {Name: name("B"), Type: intType}}},
			"struct{A, B int}",
		},
		{
			"an embedded field prints as its type",
			&StructType{FieldList: []*Field{{Type: name("Base")}}},
			"struct{Base}",
		},
		{
			"equal but distinct type nodes are not grouped",
			&StructType{FieldList: []*Field{{Name: name("A"), Type: name("int")}, {Name: name("B"), Type: name("int")}}},
			"struct{A int; B int}",
		},
	} {
		if got := String(tc.n); got != tc.want {
			t.Errorf("%s: printed %q, want %q", tc.what, got, tc.want)
		}
	}
}

// TestFieldTagsArePrinted covers the tag path, which only a struct uses.
func TestFieldTagsArePrinted(t *testing.T) {
	st := &StructType{
		FieldList: []*Field{{Name: name("A"), Type: name("int")}},
		TagList:   []*BasicLit{{Value: "`json:\"a\"`", Kind: StringLit}},
	}
	if got, want := String(st), "struct{A int `json:\"a\"`}"; got != want {
		t.Errorf("printed %q, want %q", got, want)
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
		{"empty func literal", &FuncLit{Type: &FuncType{}}, "func() {}"},
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
