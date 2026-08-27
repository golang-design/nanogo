// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"fmt"
	gobuild "go/build"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The IR of these tests is built by hand. The IR builder of specs/020-ir.md is
// not finished, and a construction test that waited for it would test two
// things at once.

var (
	tInt    = mkType(&ir.Type{Kind: ir.Int64, Name: "int"})
	tBool   = mkType(&ir.Type{Kind: ir.Bool, Name: "bool"})
	tByte   = mkType(&ir.Type{Kind: ir.Uint8, Name: "byte"})
	tFloat  = mkType(&ir.Type{Kind: ir.Float64, Name: "float64"})
	tString = mkType(&ir.Type{Kind: ir.String, Name: "string"})
	tIntPtr = mkType(&ir.Type{Kind: ir.Ptr, Elem: tInt})
	tArr4   = mkType(&ir.Type{Kind: ir.Array, Elem: tInt, Len: 4})
	tSlice  = mkType(&ir.Type{Kind: ir.Slice, Elem: tInt})
	tStruct = mkType(&ir.Type{Kind: ir.Struct, Name: "point", Fields: []ir.Field{
		{Name: "x", Type: tInt}, {Name: "y", Type: tInt},
	}})
	tFunc = mkType(&ir.Type{Kind: ir.FuncKind, Name: "func()"})
	tVoid = mkType(&ir.Type{Kind: ir.Void})
)

func mkType(t *ir.Type) *ir.Type {
	if err := ir.Layout(t); err != nil {
		panic(err)
	}
	return t
}

// litVal is a constant as the IR carries one: its Go syntax and nothing else.
type litVal string

func (l litVal) String() string { return string(l) }

var objSeq int

func obj(name string, t *ir.Type, class ir.Class) *ir.Object {
	objSeq++
	return &ir.Object{Name: name, Type: t, Class: class, Pos: syntax.Pos(objSeq)}
}

func local(o *ir.Object) ir.Expr {
	op := ir.OLocal
	if o.Class == ir.ClassGlobal || o.Class == ir.ClassFunc {
		op = ir.OGlobal
	}
	return &ir.Node{Op: op, Obj: o, Type: o.Type}
}

func cst(t *ir.Type, text string) ir.Expr {
	return &ir.Node{Op: ir.OConst, Type: t, Val: litVal(text)}
}

func cint(v string) ir.Expr { return cst(tInt, v) }

// asn is a plain assignment: dst = src.
func asn(dst, src ir.Expr) ir.Stmt {
	return &ir.Node{Op: ir.OAssign, X: dst, Y: src, Type: tVoid}
}

// def is a declaring assignment: dst := src. ir.Build marks the form with
// syntax.Def, and a declaration inside a loop body is a new instance of its
// object every time it executes.
func def(dst, src ir.Expr) ir.Stmt {
	return &ir.Node{Op: ir.OAssign, Op1: syntax.Def, X: dst, Y: src, Type: tVoid}
}

// multiAsn is a multi-value assignment: X is nil and the destinations are in
// Args.
func multiAsn(src ir.Expr, dsts ...ir.Expr) ir.Stmt {
	return &ir.Node{Op: ir.OAssign, Args: dsts, Y: src, Type: tVoid}
}

func bin(op syntax.Operator, x, y ir.Expr) ir.Expr {
	return &ir.Node{Op: ir.OBinary, Op1: op, X: x, Y: y, Type: x.Type}
}

func cmp(op syntax.Operator, x, y ir.Expr) ir.Expr {
	return &ir.Node{Op: ir.OCompare, Op1: op, X: x, Y: y, Type: tBool}
}

func ifStmt(cond ir.Expr, body []ir.Stmt, els []ir.Stmt) ir.Stmt {
	return &ir.Node{Op: ir.OIf, X: cond, Body: body, Else: els}
}

func forStmt(init []ir.Stmt, cond ir.Expr, post []ir.Stmt, body []ir.Stmt) ir.Stmt {
	return &ir.Node{Op: ir.OFor, Init: init, X: cond, Post: post, Body: body}
}

// clause is one switch clause. No case expression is the default.
func clause(exprs []ir.Expr, body ...ir.Stmt) ir.Stmt {
	return &ir.Node{Op: ir.OCase, Args: exprs, Body: body}
}

func switchStmt(tag ir.Expr, clauses ...ir.Stmt) ir.Stmt {
	return &ir.Node{Op: ir.OSwitch, X: tag, Body: clauses}
}

func ret(args ...ir.Expr) ir.Stmt { return &ir.Node{Op: ir.OReturn, Args: args} }

// fallthroughStmt is the encoding ir.Build gives a fallthrough: a goto whose
// label is the keyword, which no source label can collide with.
func fallthroughStmt() ir.Stmt { return &ir.Node{Op: ir.OGoto, Label: "fallthrough"} }

// tuple is the type of a multi-value expression.
func tuple(types ...*ir.Type) *ir.Type {
	fields := make([]ir.Field, len(types))
	for i, t := range types {
		fields[i] = ir.Field{Name: "r", Type: t}
	}
	return mkType(&ir.Type{Kind: ir.Tuple, Fields: fields})
}

func label(name string, body ...ir.Stmt) ir.Stmt {
	return &ir.Node{Op: ir.OLabel, Label: name, Body: body}
}

func gotoStmt(name string) ir.Stmt  { return &ir.Node{Op: ir.OGoto, Label: name} }
func breakStmt(name string) ir.Stmt { return &ir.Node{Op: ir.OBreak, Label: name} }
func contStmt(name string) ir.Stmt  { return &ir.Node{Op: ir.OContinue, Label: name} }

func call(fn *ir.Object, t *ir.Type, args ...ir.Expr) ir.Expr {
	return &ir.Node{Op: ir.OCall, X: local(fn), Args: args, Type: t}
}

func index(x, i ir.Expr, t *ir.Type) ir.Expr {
	return &ir.Node{Op: ir.OIndex, X: x, Y: i, Type: t}
}

func field(x ir.Expr, i int, t *ir.Type) ir.Expr {
	return &ir.Node{Op: ir.OField, X: x, Index: i, Type: t}
}

func deref(x ir.Expr, t *ir.Type) ir.Expr {
	return &ir.Node{Op: ir.ODeref, X: x, Type: t}
}

func addrOf(x ir.Expr) ir.Expr {
	return &ir.Node{Op: ir.OAddr, X: x, Type: mkType(&ir.Type{Kind: ir.Ptr, Elem: x.Type})}
}

// fun assembles a function around a body.
func fun(name string, locals []*ir.Object, body ...ir.Stmt) *ir.Func {
	return &ir.Func{Name: name, Sym: name, Type: tFunc, Locals: locals, Body: body}
}

// build builds and verifies, and fails the test with a dump if either step
// fails. Every construction test goes through it, because the verifier is the
// instrument: a shape that builds and does not verify has not been built.
func build(t *testing.T, fn *ir.Func) *Func {
	t.Helper()
	f, err := Build(fn)
	if err != nil {
		t.Fatalf("Build(%s): %v", fn.Name, err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify(%s): %v\n%s", fn.Name, vs, f)
	}
	return f
}

func TestBuildStraightLine(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	fn := fun("straight", []*ir.Object{x},
		asn(local(x), cint("1")),
		asn(local(x), bin(syntax.Add, local(x), cint("2"))),
		ret(local(x)),
	)
	f := build(t, fn)
	if len(f.Blocks) != 1 {
		t.Errorf("straight line built %d blocks, want 1", len(f.Blocks))
	}
	if f.Blocks[0].Kind != BlockRet {
		t.Errorf("last block is %v, want Ret", f.Blocks[0].Kind)
	}
	if n := countOp(f, OpPhi); n != 0 {
		t.Errorf("straight line has %d phis, want 0", n)
	}
	if n := countOp(f, OpAdd); n != 1 {
		t.Errorf("got %d adds, want 1", n)
	}
}

// TestBuildShapes builds every control-flow shape Go's grammar produces and
// asserts the verifier is silent. specs/021-ssa-construction.md names the
// verifier as the test instrument, and this is the corpus it runs on.
func TestBuildShapes(t *testing.T) {
	tests := []struct {
		name string
		fn   func() *ir.Func
	}{
		{"if", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				ifStmt(cmp(syntax.Lss, local(x), cint("3")),
					[]ir.Stmt{asn(local(x), cint("1"))}, nil),
				ret(local(x)))
		}},
		{"if else", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				ifStmt(cmp(syntax.Gtr, local(x), cint("0")),
					[]ir.Stmt{asn(local(x), cint("1"))},
					[]ir.Stmt{asn(local(x), cint("2"))}),
				ret(local(x)))
		}},
		{"if both arms return", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				ifStmt(cmp(syntax.Geq, local(x), cint("0")),
					[]ir.Stmt{ret(cint("1"))},
					[]ir.Stmt{ret(cint("2"))}))
		}},
		{"nested if", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			y := obj("y", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x, y},
				ifStmt(cmp(syntax.Lss, local(x), cint("1")),
					[]ir.Stmt{ifStmt(cmp(syntax.Lss, local(y), cint("2")),
						[]ir.Stmt{asn(local(x), cint("3"))},
						[]ir.Stmt{asn(local(x), cint("4"))})},
					[]ir.Stmt{asn(local(y), cint("5"))}),
				ret(local(x), local(y)))
		}},
		{"loop", func() *ir.Func {
			i := obj("i", tInt, ir.ClassLocal)
			s := obj("s", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{i, s},
				forStmt(
					[]ir.Stmt{asn(local(i), cint("0"))},
					cmp(syntax.Lss, local(i), cint("10")),
					[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
					[]ir.Stmt{asn(local(s), bin(syntax.Add, local(s), local(i)))}),
				ret(local(s)))
		}},
		{"infinite loop with break", func() *ir.Func {
			i := obj("i", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{i},
				forStmt(nil, nil, nil, []ir.Stmt{
					asn(local(i), bin(syntax.Add, local(i), cint("1"))),
					ifStmt(cmp(syntax.Gtr, local(i), cint("9")),
						[]ir.Stmt{breakStmt("")}, nil),
				}),
				ret(local(i)))
		}},
		{"nested loops", func() *ir.Func {
			i := obj("i", tInt, ir.ClassLocal)
			j := obj("j", tInt, ir.ClassLocal)
			s := obj("s", tInt, ir.ClassLocal)
			inner := forStmt(
				[]ir.Stmt{asn(local(j), cint("0"))},
				cmp(syntax.Lss, local(j), local(i)),
				[]ir.Stmt{asn(local(j), bin(syntax.Add, local(j), cint("1")))},
				[]ir.Stmt{asn(local(s), bin(syntax.Add, local(s), local(j)))})
			return fun("f", []*ir.Object{i, j, s},
				forStmt(
					[]ir.Stmt{asn(local(i), cint("0"))},
					cmp(syntax.Lss, local(i), cint("4")),
					[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
					[]ir.Stmt{inner}),
				ret(local(s)))
		}},
		{"loop with early exit", func() *ir.Func {
			i := obj("i", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{i},
				forStmt(
					[]ir.Stmt{asn(local(i), cint("0"))},
					cmp(syntax.Lss, local(i), cint("10")),
					[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
					[]ir.Stmt{
						ifStmt(cmp(syntax.Eql, local(i), cint("5")),
							[]ir.Stmt{ret(local(i))}, nil),
					}),
				ret(cint("0")))
		}},
		{"loop with continue", func() *ir.Func {
			i := obj("i", tInt, ir.ClassLocal)
			s := obj("s", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{i, s},
				forStmt(
					[]ir.Stmt{asn(local(i), cint("0"))},
					cmp(syntax.Lss, local(i), cint("10")),
					[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
					[]ir.Stmt{
						ifStmt(cmp(syntax.Eql, local(i), cint("3")),
							[]ir.Stmt{contStmt("")}, nil),
						asn(local(s), bin(syntax.Add, local(s), local(i))),
					}),
				ret(local(s)))
		}},
		{"labelled break and continue", func() *ir.Func {
			i := obj("i", tInt, ir.ClassLocal)
			j := obj("j", tInt, ir.ClassLocal)
			inner := forStmt(
				[]ir.Stmt{asn(local(j), cint("0"))},
				cmp(syntax.Lss, local(j), cint("4")),
				[]ir.Stmt{asn(local(j), bin(syntax.Add, local(j), cint("1")))},
				[]ir.Stmt{
					ifStmt(cmp(syntax.Eql, local(j), cint("2")),
						[]ir.Stmt{contStmt("outer")},
						[]ir.Stmt{breakStmt("outer")}),
				})
			outer := forStmt(
				[]ir.Stmt{asn(local(i), cint("0"))},
				cmp(syntax.Lss, local(i), cint("4")),
				[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
				[]ir.Stmt{inner})
			return fun("f", []*ir.Object{i, j}, label("outer", outer), ret(local(i)))
		}},
		{"labelled break out of a switch", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			sw := switchStmt(local(x),
				clause([]ir.Expr{cint("1")}, breakStmt("sw")),
				clause(nil, asn(local(x), cint("2"))))
			return fun("f", []*ir.Object{x}, label("sw", sw), ret(local(x)))
		}},
		{"goto forwards", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), cint("1")),
				gotoStmt("done"),
				asn(local(x), cint("2")),
				label("done"),
				ret(local(x)))
		}},
		{"goto backwards", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				label("again"),
				asn(local(x), bin(syntax.Add, local(x), cint("1"))),
				ifStmt(cmp(syntax.Lss, local(x), cint("10")),
					[]ir.Stmt{gotoStmt("again")}, nil),
				ret(local(x)))
		}},
		{"goto into a loop body from before it", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), cint("0")),
				label("top"),
				asn(local(x), bin(syntax.Add, local(x), cint("1"))),
				ifStmt(cmp(syntax.Lss, local(x), cint("3")),
					[]ir.Stmt{gotoStmt("top")},
					[]ir.Stmt{gotoStmt("out")}),
				label("out"),
				ret(local(x)))
		}},
		{"switch with default", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			y := obj("y", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x, y},
				switchStmt(local(x),
					clause([]ir.Expr{cint("1"), cint("2")}, asn(local(y), cint("10"))),
					clause([]ir.Expr{cint("3")}, asn(local(y), cint("20"))),
					clause(nil, asn(local(y), cint("30")))),
				ret(local(y)))
		}},
		{"switch without default", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			y := obj("y", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x, y},
				switchStmt(local(x),
					clause([]ir.Expr{cint("1")}, asn(local(y), cint("10")))),
				ret(local(y)))
		}},
		{"expressionless switch", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				switchStmt(nil,
					clause([]ir.Expr{cmp(syntax.Lss, local(x), cint("0"))}, ret(cint("1"))),
					clause(nil, ret(cint("2")))))
		}},
		{"switch with break", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				switchStmt(local(x),
					clause([]ir.Expr{cint("1")}, breakStmt(""), asn(local(x), cint("9"))),
					clause(nil, asn(local(x), cint("2")))),
				ret(local(x)))
		}},
		{"short circuit and", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			cond := &ir.Node{Op: ir.OBinary, Op1: syntax.AndAnd, Type: tBool,
				X: cmp(syntax.Lss, local(x), cint("3")),
				Y: cmp(syntax.Gtr, local(x), cint("0"))}
			return fun("f", []*ir.Object{x},
				ifStmt(cond, []ir.Stmt{asn(local(x), cint("1"))}, nil),
				ret(local(x)))
		}},
		{"short circuit or", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			cond := &ir.Node{Op: ir.OBinary, Op1: syntax.OrOr, Type: tBool,
				X: cmp(syntax.Lss, local(x), cint("3")),
				Y: cmp(syntax.Gtr, local(x), cint("9"))}
			return fun("f", []*ir.Object{x},
				ifStmt(cond, []ir.Stmt{asn(local(x), cint("1"))}, nil),
				ret(local(x)))
		}},
		{"short circuit in a loop condition", func() *ir.Func {
			// The one place where an expression that creates blocks meets an
			// unsealed block: the loop header is not sealed until the back
			// edge exists, and evaluating the condition adds blocks before
			// then.
			i := obj("i", tInt, ir.ClassLocal)
			ok := obj("ok", tBool, ir.ClassLocal)
			cond := &ir.Node{Op: ir.OBinary, Op1: syntax.AndAnd, Type: tBool,
				X: cmp(syntax.Lss, local(i), cint("10")),
				Y: local(ok)}
			return fun("f", []*ir.Object{i, ok},
				forStmt([]ir.Stmt{asn(local(i), cint("0"))}, cond,
					[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
					[]ir.Stmt{asn(local(ok), cst(tBool, "false"))}),
				ret(local(i)))
		}},
		{"short circuit in a loop condition, or", func() *ir.Func {
			i := obj("i", tInt, ir.ClassLocal)
			ok := obj("ok", tBool, ir.ClassLocal)
			cond := &ir.Node{Op: ir.OBinary, Op1: syntax.OrOr, Type: tBool,
				X: local(ok),
				Y: cmp(syntax.Lss, local(i), cint("10"))}
			return fun("f", []*ir.Object{i, ok},
				forStmt(nil, cond,
					[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
					[]ir.Stmt{asn(local(ok), cst(tBool, "false"))}),
				ret(local(i)))
		}},
		{"address taken local", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			x.Addrtaken = true
			p := obj("p", tIntPtr, ir.ClassLocal)
			return fun("f", []*ir.Object{x, p},
				asn(local(x), cint("1")),
				asn(local(p), addrOf(local(x))),
				asn(local(x), bin(syntax.Add, local(x), deref(local(p), tInt))),
				ret(local(x)))
		}},
		{"array index", func() *ir.Func {
			a := obj("a", tArr4, ir.ClassLocal)
			i := obj("i", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{a, i},
				asn(index(local(a), local(i), tInt), cint("7")),
				ret(index(local(a), cint("0"), tInt)))
		}},
		{"slice index", func() *ir.Func {
			s := obj("s", tSlice, ir.ClassLocal)
			i := obj("i", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{s, i},
				ret(index(local(s), local(i), tInt)))
		}},
		{"string index", func() *ir.Func {
			s := obj("s", tString, ir.ClassLocal)
			return fun("f", []*ir.Object{s},
				ret(index(local(s), cint("0"), tByte)))
		}},
		{"pointer to array index", func() *ir.Func {
			pa := obj("pa", mkType(&ir.Type{Kind: ir.Ptr, Elem: tArr4}), ir.ClassLocal)
			return fun("f", []*ir.Object{pa},
				ret(index(local(pa), cint("1"), tInt)))
		}},
		{"struct field", func() *ir.Func {
			p := obj("p", tStruct, ir.ClassLocal)
			return fun("f", []*ir.Object{p},
				asn(field(local(p), 0, tInt), cint("1")),
				ret(field(local(p), 1, tInt)))
		}},
		{"field of a pointer", func() *ir.Func {
			pp := obj("pp", mkType(&ir.Type{Kind: ir.Ptr, Elem: tStruct}), ir.ClassLocal)
			return fun("f", []*ir.Object{pp},
				ret(field(local(pp), 1, tInt)))
		}},
		{"global", func() *ir.Func {
			g := obj("g", tInt, ir.ClassGlobal)
			return fun("f", nil,
				asn(local(g), cint("3")),
				ret(local(g)))
		}},
		{"static call", func() *ir.Func {
			callee := obj("callee", tFunc, ir.ClassFunc)
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), call(callee, tInt, cint("1"))),
				ret(local(x)))
		}},
		{"void call as a statement", func() *ir.Func {
			callee := obj("callee", tFunc, ir.ClassFunc)
			return fun("f", nil,
				&ir.Node{Op: ir.OCall, X: local(callee), Type: tVoid},
				ret())
		}},
		{"closure call", func() *ir.Func {
			fp := obj("fp", tFunc, ir.ClassLocal)
			return fun("f", []*ir.Object{fp},
				&ir.Node{Op: ir.OCall, X: local(fp), Type: tVoid},
				ret())
		}},
		{"conversions", func() *ir.Func {
			i8 := obj("i8", mkType(&ir.Type{Kind: ir.Int8}), ir.ClassLocal)
			u8 := obj("u8", mkType(&ir.Type{Kind: ir.Uint8}), ir.ClassLocal)
			x := obj("x", tInt, ir.ClassLocal)
			fl := obj("fl", tFloat, ir.ClassLocal)
			up := obj("up", mkType(&ir.Type{Kind: ir.UnsafePtr}), ir.ClassLocal)
			p := obj("p", tIntPtr, ir.ClassLocal)
			conv := func(x ir.Expr, t *ir.Type) ir.Expr {
				return &ir.Node{Op: ir.OConvert, X: x, Type: t}
			}
			return fun("f", []*ir.Object{i8, u8, x, fl, up, p},
				asn(local(x), conv(local(i8), tInt)),
				asn(local(x), conv(local(u8), tInt)),
				asn(local(i8), conv(local(x), i8.Type)),
				asn(local(fl), conv(local(x), tFloat)),
				asn(local(x), conv(local(fl), tInt)),
				asn(local(fl), conv(local(fl), tFloat)),
				asn(local(up), conv(local(p), up.Type)),
				ret(local(x)))
		}},
		{"unary operators", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			c := obj("c", tBool, ir.ClassLocal)
			un := func(op syntax.Operator, x ir.Expr) ir.Expr {
				return &ir.Node{Op: ir.OUnary, Op1: op, X: x, Type: x.Type}
			}
			return fun("f", []*ir.Object{x, c},
				asn(local(x), un(syntax.Sub, local(x))),
				asn(local(x), un(syntax.Xor, local(x))),
				asn(local(x), un(syntax.Add, local(x))),
				asn(local(c), un(syntax.Not, local(c))),
				ret(local(x)))
		}},
		{"every binary operator", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			ops := []syntax.Operator{syntax.Add, syntax.Sub, syntax.Mul, syntax.Div,
				syntax.Rem, syntax.And, syntax.Or, syntax.Xor, syntax.AndNot,
				syntax.Shl, syntax.Shr}
			var body []ir.Stmt
			for _, op := range ops {
				body = append(body, asn(local(x), bin(op, local(x), cint("2"))))
			}
			body = append(body, ret(local(x)))
			return fun("f", []*ir.Object{x}, body...)
		}},
		{"every comparison", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			c := obj("c", tBool, ir.ClassLocal)
			ops := []syntax.Operator{syntax.Eql, syntax.Neq, syntax.Lss,
				syntax.Leq, syntax.Gtr, syntax.Geq}
			var body []ir.Stmt
			for _, op := range ops {
				body = append(body, asn(local(c), cmp(op, local(x), cint("1"))))
			}
			body = append(body, ret(local(c)))
			return fun("f", []*ir.Object{x, c}, body...)
		}},
		{"constants of every kind", func() *ir.Func {
			s := obj("s", tString, ir.ClassLocal)
			fl := obj("fl", tFloat, ir.ClassLocal)
			c := obj("c", tBool, ir.ClassLocal)
			p := obj("p", tIntPtr, ir.ClassLocal)
			u := obj("u", mkType(&ir.Type{Kind: ir.Uint64}), ir.ClassLocal)
			return fun("f", []*ir.Object{s, fl, c, p, u},
				asn(local(s), cst(tString, `"hello"`)),
				asn(local(fl), cst(tFloat, "1.5")),
				asn(local(c), cst(tBool, "true")),
				asn(local(c), cst(tBool, "false")),
				asn(local(p), cst(tIntPtr, "nil")),
				asn(local(u), cst(u.Type, "18446744073709551615")),
				ret(local(c)))
		}},
		{"parameters and results", func() *ir.Func {
			a := obj("a", tInt, ir.ClassParam)
			b := obj("b", tInt, ir.ClassParam)
			r := obj("r", tInt, ir.ClassResult)
			recv := obj("recv", tIntPtr, ir.ClassParam)
			fn := fun("f", nil, asn(local(r), bin(syntax.Add, local(a), local(b))), ret())
			fn.Recv = recv
			fn.Params = []*ir.Object{a, b}
			fn.Results = []*ir.Object{r}
			return fn
		}},
		{"address taken parameter", func() *ir.Func {
			a := obj("a", tInt, ir.ClassParam)
			a.Addrtaken = true
			fn := fun("f", nil, ret(local(a)))
			fn.Params = []*ir.Object{a}
			return fn
		}},
		{"empty body", func() *ir.Func {
			return fun("f", nil)
		}},
		{"block statement", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				&ir.Node{Op: ir.OBlock,
					Init: []ir.Stmt{asn(local(x), cint("1"))},
					Body: []ir.Stmt{asn(local(x), cint("2"))}},
				ret(local(x)))
		}},
		{"bool to integer conversion", func() *ir.Func {
			c := obj("c", tBool, ir.ClassLocal)
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{c, x},
				asn(local(x), &ir.Node{Op: ir.OConvert, X: local(c), Type: tInt}),
				ret(local(x)))
		}},
		{"a conversion between two names for one representation", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			y := obj("y", mkType(&ir.Type{Kind: ir.Int64, Name: "myint"}), ir.ClassLocal)
			return fun("f", []*ir.Object{x, y},
				asn(local(y), &ir.Node{Op: ir.OConvert, X: local(x), Type: y.Type}),
				ret(local(y)))
		}},
		{"constants whose text does not parse", func() *ir.Func {
			// ir.Value carries Go syntax only, so a constant construction
			// cannot read stays in Aux rather than becoming a wrong number.
			x := obj("x", tInt, ir.ClassLocal)
			fl := obj("fl", tFloat, ir.ClassLocal)
			c := obj("c", tBool, ir.ClassLocal)
			s := obj("s", tString, ir.ClassLocal)
			return fun("f", []*ir.Object{x, fl, c, s},
				asn(local(x), cst(tInt, "not a number")),
				asn(local(fl), cst(tFloat, "not a number")),
				asn(local(c), cst(tBool, "not a bool")),
				asn(local(s), cst(tString, "unquoted")),
				ret(local(x)))
		}},
		{"an unreachable predecessor of a block with a phi", func() *ir.Func {
			// The label block L is reached by nothing, and it is a
			// predecessor of the block after the if. Construction must drop
			// the block, drop the phi argument for its edge, and notice that
			// the phi is then redundant.
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				gotoStmt("skip"),
				label("L"),
				asn(local(x), cint("5")),
				gotoStmt("done"),
				label("skip"),
				ifStmt(cmp(syntax.Lss, local(x), cint("1")),
					[]ir.Stmt{asn(local(x), cint("2"))},
					[]ir.Stmt{asn(local(x), cint("3"))}),
				label("done"),
				ret(local(x)))
		}},
		{"a function value", func() *ir.Func {
			callee := obj("callee", tFunc, ir.ClassFunc)
			fp := obj("fp", tFunc, ir.ClassLocal)
			return fun("f", []*ir.Object{fp},
				asn(local(fp), local(callee)),
				ret(local(fp)))
		}},
		{"a global named by a local node", func() *ir.Func {
			// The IR builder is expected to use OGlobal, but an object of
			// global class reaching the local path must still be addressed
			// rather than read from a value.
			g := obj("g", tInt, ir.ClassGlobal)
			asLocal := &ir.Node{Op: ir.OLocal, Obj: g, Type: g.Type}
			return fun("f", nil,
				asn(asLocal, cint("1")),
				ret(&ir.Node{Op: ir.OLocal, Obj: g, Type: g.Type}))
		}},
		{"a float32 to float64 conversion", func() *ir.Func {
			f32 := obj("f32", mkType(&ir.Type{Kind: ir.Float32}), ir.ClassLocal)
			f64 := obj("f64", tFloat, ir.ClassLocal)
			return fun("f", []*ir.Object{f32, f64},
				asn(local(f64), &ir.Node{Op: ir.OConvert, X: local(f32), Type: tFloat}),
				ret(local(f64)))
		}},
		{"a pointer to uintptr conversion", func() *ir.Func {
			p := obj("p", tIntPtr, ir.ClassLocal)
			u := obj("u", mkType(&ir.Type{Kind: ir.Uintptr}), ir.ClassLocal)
			return fun("f", []*ir.Object{p, u},
				asn(local(u), &ir.Node{Op: ir.OConvert, X: local(p), Type: u.Type}),
				ret(local(u)))
		}},
		{"a read in an unreachable label block", func() *ir.Func {
			// Nothing jumps to L, so it is sealed with no predecessor at the
			// end of the walk. The phi it left behind has no argument at all,
			// which is the case Braun et al. (2013) fills with the zero value
			// rather than leaving undefined.
			x := obj("x", tInt, ir.ClassLocal)
			g := obj("g", tInt, ir.ClassGlobal)
			return fun("f", []*ir.Object{x},
				gotoStmt("end"),
				label("L"),
				asn(local(x), bin(syntax.Add, local(x), cint("1"))),
				asn(local(g), bin(syntax.Add, local(g), cint("1"))),
				gotoStmt("end"),
				label("end"),
				ret(local(x)))
		}},
		{"unreachable statements after a return", func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				ret(cint("1")),
				asn(local(x), cint("2")))
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := build(t, tc.fn())
			// Nothing built here may leave a Go-specific operation behind, and
			// every block must have been given a kind.
			for _, b := range f.Blocks {
				if b.Kind == BlockInvalid {
					t.Errorf("%v has no kind\n%s", b, f)
				}
			}
		})
	}
}

// TestBuildDeterminism asserts two builds of one tree produce the same dump.
//
// specs/053-determinism.md is a gate, not hygiene, and the cheapest way for
// construction to break it is a map range in sealing or in the address-taken
// pass.
func TestBuildDeterminism(t *testing.T) {
	mk := func() *ir.Func {
		i := obj("i", tInt, ir.ClassLocal)
		s := obj("s", tInt, ir.ClassLocal)
		x := obj("x", tInt, ir.ClassLocal)
		x.Addrtaken = true
		return fun("f", []*ir.Object{i, s, x},
			label("top"),
			forStmt([]ir.Stmt{asn(local(i), cint("0"))},
				cmp(syntax.Lss, local(i), cint("10")),
				[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
				[]ir.Stmt{
					asn(local(s), bin(syntax.Add, local(s), local(i))),
					asn(local(x), local(s)),
					ifStmt(cmp(syntax.Eql, local(i), cint("5")),
						[]ir.Stmt{gotoStmt("top")}, nil),
				}),
			ret(local(s)))
	}
	first := build(t, mk()).String()
	for i := 0; i < 8; i++ {
		if got := build(t, mk()).String(); got != first {
			t.Fatalf("build %d differs:\n%s\nwant:\n%s", i, got, first)
		}
	}
}

// TestPhiMinimality compares the phis against a hand-computed set.
//
// The counts below are worked out from the shape, not read off the output. A
// phi is needed where two definitions of one variable meet, and Braun et al.
// (2013) removes the rest as it goes.
func TestPhiMinimality(t *testing.T) {
	tests := []struct {
		name string
		fn   func() *ir.Func
		// phis is the number of phis expected, and vars names what they merge.
		phis int
		why  string
	}{
		{
			name: "no merge, no phi",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{x},
					asn(local(x), cint("1")),
					ret(local(x)))
			},
			phis: 0,
			why:  "one block",
		},
		{
			name: "if with no assignment",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{x},
					ifStmt(cmp(syntax.Lss, local(x), cint("1")), nil, nil),
					ret(local(x)))
			},
			phis: 0,
			why:  "neither arm defines anything, so every phi is trivial",
		},
		{
			name: "if else assigning one variable",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{x},
					ifStmt(cmp(syntax.Lss, local(x), cint("1")),
						[]ir.Stmt{asn(local(x), cint("2"))},
						[]ir.Stmt{asn(local(x), cint("3"))}),
					ret(local(x)))
			},
			phis: 1,
			why:  "one variable, two definitions, one join",
		},
		{
			name: "if assigning one variable in one arm",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{x},
					ifStmt(cmp(syntax.Lss, local(x), cint("1")),
						[]ir.Stmt{asn(local(x), cint("2"))}, nil),
					ret(local(x)))
			},
			phis: 1,
			why:  "the zero from the entry and the 2 from the arm meet at the join",
		},
		{
			name: "if else assigning two variables",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				y := obj("y", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{x, y},
					ifStmt(cmp(syntax.Lss, local(x), cint("1")),
						[]ir.Stmt{asn(local(x), cint("2")), asn(local(y), cint("3"))},
						[]ir.Stmt{asn(local(x), cint("4")), asn(local(y), cint("5"))}),
					ret(local(x), local(y)))
			},
			phis: 2,
			why:  "one per variable",
		},
		{
			name: "counted loop",
			fn: func() *ir.Func {
				i := obj("i", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{i},
					forStmt([]ir.Stmt{asn(local(i), cint("0"))},
						cmp(syntax.Lss, local(i), cint("10")),
						[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
						nil),
					ret(local(i)))
			},
			phis: 1,
			why:  "the induction variable, in the header only",
		},
		{
			name: "counted loop with an accumulator",
			fn: func() *ir.Func {
				i := obj("i", tInt, ir.ClassLocal)
				s := obj("s", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{i, s},
					forStmt([]ir.Stmt{asn(local(i), cint("0"))},
						cmp(syntax.Lss, local(i), cint("10")),
						[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
						[]ir.Stmt{asn(local(s), bin(syntax.Add, local(s), local(i)))}),
					ret(local(s)))
			},
			phis: 2,
			why:  "the induction variable and the accumulator, both in the header",
		},
		{
			name: "nested loops",
			fn: func() *ir.Func {
				i := obj("i", tInt, ir.ClassLocal)
				j := obj("j", tInt, ir.ClassLocal)
				inner := forStmt([]ir.Stmt{asn(local(j), cint("0"))},
					cmp(syntax.Lss, local(j), cint("4")),
					[]ir.Stmt{asn(local(j), bin(syntax.Add, local(j), cint("1")))},
					nil)
				return fun("f", []*ir.Object{i, j},
					forStmt([]ir.Stmt{asn(local(i), cint("0"))},
						cmp(syntax.Lss, local(i), cint("4")),
						[]ir.Stmt{asn(local(i), bin(syntax.Add, local(i), cint("1")))},
						[]ir.Stmt{inner}),
					ret(local(i)))
			},
			phis: 2,
			why:  "one per header: the inner loop never reads i, so i needs no phi there",
		},
		{
			name: "a store in one arm needs a memory phi",
			fn: func() *ir.Func {
				g := obj("g", tInt, ir.ClassGlobal)
				x := obj("x", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{x},
					ifStmt(cmp(syntax.Lss, local(x), cint("1")),
						[]ir.Stmt{asn(local(g), cint("2"))}, nil),
					ret(local(g)))
			},
			phis: 1,
			why:  "memory is a variable, so the join merges it with an ordinary phi",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := build(t, tc.fn())
			got := countOp(f, OpPhi)
			if got != tc.phis {
				t.Errorf("got %d phis, want %d (%s)\n%s", got, tc.phis, tc.why, f)
			}
			// Minimality also means no surviving phi is redundant.
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					if v.Op != OpPhi {
						continue
					}
					if distinctArgs(v) < 2 {
						t.Errorf("%s is redundant\n%s", v.LongString(), f)
					}
				}
			}
		})
	}
}

// TestMemoryOrdering asserts a load cannot move across a call, by asserting
// that the data dependence which forbids it exists.
//
// specs/021-ssa-construction.md: calls take memory and produce memory, and
// that is what prevents a load from moving across a call. Nothing in a later
// pass is asked to know this; it falls out of the argument.
func TestMemoryOrdering(t *testing.T) {
	g := obj("g", tInt, ir.ClassGlobal)
	callee := obj("callee", tFunc, ir.ClassFunc)
	a := obj("a", tInt, ir.ClassLocal)
	b := obj("b", tInt, ir.ClassLocal)
	fn := fun("f", []*ir.Object{a, b},
		asn(local(a), local(g)),
		&ir.Node{Op: ir.OCall, X: local(callee), Type: tVoid},
		asn(local(b), local(g)),
		ret(local(a), local(b)))
	f := build(t, fn)

	var loads []*Value
	var callv *Value
	for _, blk := range f.Blocks {
		for _, v := range blk.Values {
			switch v.Op {
			case OpLoad:
				loads = append(loads, v)
			case OpStaticCall:
				callv = v
			}
		}
	}
	if callv == nil || len(loads) != 2 {
		t.Fatalf("got %d loads and call %v, want 2 loads and a call\n%s", len(loads), callv, f)
	}
	var initMem *Value
	for _, v := range f.Entry.Values {
		if v.Op == OpInitMem {
			initMem = v
		}
	}
	if loads[0].MemArg() != initMem {
		t.Errorf("the first load reads %v, want the initial memory", loads[0].MemArg())
	}
	if loads[1].MemArg() != callv {
		t.Errorf("the second load reads %v, want the call %v, so it cannot move above it\n%s",
			loads[1].MemArg(), callv, f)
	}
	if callv.MemArg() != initMem {
		t.Errorf("the call reads %v, want the initial memory", callv.MemArg())
	}
	// And the memory the function leaves with is the call's, not the entry's.
	last := f.Blocks[len(f.Blocks)-1]
	if last.Control == nil || last.Control.MemArg() != callv {
		t.Errorf("the return leaves with %v, want the call's memory\n%s", last.Control, f)
	}
}

// TestBoundsAndNilChecksAreInserted asserts the check is there even where it
// is obviously unnecessary.
//
// specs/021-ssa-construction.md: inserting all of them and removing some later
// is safe, inserting some is not. A constant index into a constant-length
// array is the case a builder is most tempted to skip.
func TestBoundsAndNilChecksAreInserted(t *testing.T) {
	a := obj("a", tArr4, ir.ClassLocal)
	p := obj("p", tIntPtr, ir.ClassLocal)
	fn := fun("f", []*ir.Object{a, p},
		asn(index(local(a), cint("0"), tInt), cint("1")),
		ret(deref(local(p), tInt)))
	f := build(t, fn)
	if n := countOp(f, OpBoundsCheck); n != 1 {
		t.Errorf("got %d bounds checks, want 1\n%s", n, f)
	}
	if n := countOp(f, OpNilCheck); n != 1 {
		t.Errorf("got %d nil checks, want 1\n%s", n, f)
	}
}

// TestAddrtakenLivesInTheFrame asserts the decision of specs/021 is made per
// variable, before construction, and is visible in the result.
func TestAddrtakenLivesInTheFrame(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	x.Addrtaken = true
	y := obj("y", tInt, ir.ClassLocal)
	s := obj("s", tSlice, ir.ClassLocal)
	fn := fun("f", []*ir.Object{x, y, s},
		asn(local(x), cint("1")),
		asn(local(y), cint("2")),
		ret(local(x), local(y)))
	f := build(t, fn)

	if len(f.Frame) != 2 || f.Frame[0] != x || f.Frame[1] != s {
		t.Fatalf("frame is %v, want x then s in declaration order", f.Frame)
	}
	// x is written through memory and y is not.
	if n := countOp(f, OpStore); n != 1 {
		t.Errorf("got %d stores, want 1, one for the address-taken local\n%s", n, f)
	}
	if n := countOp(f, OpLoad); n != 1 {
		t.Errorf("got %d loads, want 1\n%s", n, f)
	}
}

// ---------------------------------------------------------------------------
// Assignment

// TestBuildAssignForms builds every form of assignment and asserts what each
// one produces.
//
// The shape of each answer is the point rather than the fact that it verifies:
// a local that lives in a value is written into the variable map and produces
// no memory operation at all, and a local that lives in the frame is reached
// by an address and a store. Those two are the halves of specs/021's rule that
// an address-taken variable is not an SSA value, and a test that only checked
// that the graph verifies would pass with the two exchanged.
func TestBuildAssignForms(t *testing.T) {
	tests := []struct {
		name string
		fn   func() *ir.Func
		// want counts the operations the form must produce.
		want map[Op]int
	}{
		{
			name: "a local that lives in a value",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{x}, asn(local(x), cint("7")), ret(local(x)))
			},
			// No store and no local address: the value is the variable.
			want: map[Op]int{OpStore: 0, OpLoad: 0, OpLocalAddr: 0},
		},
		{
			name: "a local whose address is taken",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				x.Addrtaken = true
				return fun("f", []*ir.Object{x}, asn(local(x), cint("7")), ret(local(x)))
			},
			// The zero of the entry block, then the assignment, then the read.
			want: map[Op]int{OpStore: 1, OpLoad: 1, OpZero: 1},
		},
		{
			name: "a short variable declaration",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				return fun("f", []*ir.Object{x}, def(local(x), cint("7")), ret(local(x)))
			},
			want: map[Op]int{OpStore: 0, OpLoad: 0},
		},
		{
			name: "a short variable declaration of a frame-resident local",
			fn: func() *ir.Func {
				x := obj("x", tInt, ir.ClassLocal)
				x.Addrtaken = true
				return fun("f", []*ir.Object{x}, def(local(x), cint("7")), ret(local(x)))
			},
			want: map[Op]int{OpStore: 1, OpLoad: 1},
		},
		{
			name: "through a pointer",
			fn: func() *ir.Func {
				p := obj("p", tIntPtr, ir.ClassParam)
				return fun("f", nil, asn(deref(local(p), tInt), cint("7")), ret())
			},
			want: map[Op]int{OpStore: 1, OpNilCheck: 1},
		},
		{
			name: "a struct field",
			fn: func() *ir.Func {
				s := obj("s", tStruct, ir.ClassLocal)
				return fun("f", []*ir.Object{s},
					asn(field(local(s), 1, tInt), cint("7")), ret())
			},
			// A struct does not fit a register, so it is in the frame and the
			// field is an offset from its slot. The two local addresses are
			// the zeroing of the entry block and the assignment.
			want: map[Op]int{OpStore: 1, OpOffPtr: 1, OpLocalAddr: 2},
		},
		{
			name: "an array element",
			fn: func() *ir.Func {
				a := obj("a", tArr4, ir.ClassLocal)
				return fun("f", []*ir.Object{a},
					asn(index(local(a), cint("2"), tInt), cint("7")), ret())
			},
			want: map[Op]int{OpStore: 1, OpBoundsCheck: 1, OpPtrIndex: 1},
		},
		{
			name: "a slice element",
			fn: func() *ir.Func {
				sl := obj("sl", tSlice, ir.ClassLocal)
				return fun("f", []*ir.Object{sl},
					asn(index(local(sl), cint("2"), tInt), cint("7")), ret())
			},
			// The data pointer and the length are read out of the header, then
			// the bounds check, then the store.
			want: map[Op]int{OpStore: 1, OpBoundsCheck: 1, OpPtrIndex: 1, OpLoad: 2},
		},
		{
			name: "a global",
			fn: func() *ir.Func {
				g := obj("g", tInt, ir.ClassGlobal)
				return fun("f", nil, asn(local(g), cint("7")), ret())
			},
			// A global is reached by its symbol and never by a frame slot.
			want: map[Op]int{OpStore: 1, OpAddr: 1, OpLocalAddr: 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := build(t, tc.fn())
			for _, op := range sortedOps(tc.want) {
				if got := countOp(f, op); got != tc.want[op] {
					t.Errorf("got %d %v, want %d\n%s", got, op, tc.want[op], f)
				}
			}
		})
	}
}

// sortedOps returns the keys of an operation count in order.
//
// specs/053-determinism.md forbids a range over a map on a path that produces
// output, and a test failure is output.
func sortedOps(m map[Op]int) []Op {
	ops := make([]Op, 0, len(m))
	for op := range m {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i] < ops[j] })
	return ops
}

// TestBuildMultiValueAssign asserts the shape of an assignment from a call
// that produces several values.
//
// Two properties, and the second is the one a verifier would not catch on its
// own: every result is read before any destination is written. SelectN reads
// the call, which is a memory value, so a read placed after a store would name
// a memory the store has already superseded.
func TestBuildMultiValueAssign(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	y := obj("y", tInt, ir.ClassLocal)
	y.Addrtaken = true
	callee := obj("callee", tFunc, ir.ClassFunc)
	fn := fun("f", []*ir.Object{x, y},
		multiAsn(call(callee, tuple(tInt, tInt)), local(x), local(y)),
		ret(local(x), local(y)))
	f := build(t, fn)

	var sel []*Value
	firstStore := -1
	for _, b := range f.Blocks {
		for i, v := range b.Values {
			switch v.Op {
			case OpSelectN:
				sel = append(sel, v)
				if firstStore >= 0 {
					t.Errorf("a result is read after a destination is written\n%s", f)
				}
			case OpStore:
				if firstStore < 0 && b == f.Entry {
					firstStore = i
				}
			}
		}
	}
	if len(sel) != 2 {
		t.Fatalf("got %d result reads, want one per destination\n%s", len(sel), f)
	}
	for i, v := range sel {
		if v.AuxInt != int64(i) {
			t.Errorf("result read %d names index %d\n%s", i, v.AuxInt, f)
		}
		if v.Args[0].Op != OpStaticCall {
			t.Errorf("result read %d reads %v and not the call\n%s", i, v.Args[0].Op, f)
		}
	}
}

// TestBuildMultiValueAssignWideResult asserts that a result wider than a
// register builds.
//
// Construction refused this form while ssa/decompose.go numbered a part of
// result i as word i+j, because the string below owns two words and the
// integer after it would have been read from the second of them. The
// numbering is now a sum over the widths of the earlier results, so the form
// builds and reads what it names.
func TestBuildMultiValueAssignWideResult(t *testing.T) {
	s := obj("s", tString, ir.ClassLocal)
	n := obj("n", tInt, ir.ClassLocal)
	callee := obj("callee", tFunc, ir.ClassFunc)
	fn := fun("f", []*ir.Object{s, n},
		multiAsn(call(callee, tuple(tString, tInt)), local(s), local(n)),
		ret(local(s), local(n)))
	f := build(t, fn)

	var got []string
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpSelectN {
				got = append(got, fmt.Sprintf("%d:%v", v.AuxInt, v.Type))
			}
		}
	}
	// The index is still the result here. It becomes the word of the result
	// area in ssa/decompose.go, which is the pass that knows the widths.
	if want := "0:string 1:int"; strings.Join(got, " ") != want {
		t.Errorf("results are %q, want %q\n%s", strings.Join(got, " "), want, f)
	}
}

// TestBuildMultiValueAssignOneDestination asserts the form with one
// destination builds.
func TestBuildMultiValueAssignOneDestination(t *testing.T) {
	s := obj("s", tString, ir.ClassLocal)
	callee := obj("callee", tFunc, ir.ClassFunc)
	fn := fun("f", []*ir.Object{s},
		multiAsn(call(callee, tString), local(s)),
		ret(local(s)))
	f := build(t, fn)
	if n := countOp(f, OpSelectN); n != 1 {
		t.Errorf("got %d result reads, want 1\n%s", n, f)
	}
}

// TestBuildMultiValueAssignNamesEveryResult asserts a result assigned to the
// blank identifier is still read.
//
// The code generator places the results of a call together and names each one
// by the SelectN that reads it, so a gap in the sequence is a result it cannot
// place. ir.Build gives the blank identifier an object of its own that is in
// no declaration list, which is why nothing is stored for it.
func TestBuildMultiValueAssignNamesEveryResult(t *testing.T) {
	blank := &ir.Object{Name: "_", Type: tInt, Class: ir.ClassLocal}
	y := obj("y", tInt, ir.ClassLocal)
	callee := obj("callee", tFunc, ir.ClassFunc)
	fn := fun("f", []*ir.Object{y},
		multiAsn(call(callee, tuple(tInt, tInt)), local(blank), local(y)),
		ret(local(y)))
	f := build(t, fn)
	if n := countOp(f, OpSelectN); n != 2 {
		t.Errorf("got %d result reads, want 2 even though one result is discarded\n%s", n, f)
	}
}

// TestBuildPerIterationLoopVariable is the Go 1.22 loop variable, checked
// against the shape ir.Build produces for it.
//
// ir.Build gives each iteration its own instance already: a loop variable
// whose address is taken is replaced in the loop control by a carrier, the
// body opens by declaring the variable again from the carrier, and the post
// list opens by copying it back. Construction's obligation is to build that
// declaration where it stands, so that it runs on every iteration.
//
// The test fails under pre-1.22 semantics in two independent ways. If the
// declaration is treated as done by the entry block's zeroing, the store to
// the variable's slot is in the entry block and every iteration reads what the
// last one left. If the post list is dropped, which it was while forParts read
// Else, the copy back and the increment are both gone and the loop does not
// advance at all.
func TestBuildPerIterationLoopVariable(t *testing.T) {
	carrier := obj(".loopvar_i", tInt, ir.ClassLocal)
	i := obj("i", tInt, ir.ClassLocal)
	i.Addrtaken = true
	use := obj("use", tFunc, ir.ClassFunc)

	loop := forStmt(
		[]ir.Stmt{def(local(carrier), cint("0"))},
		cmp(syntax.Lss, local(carrier), cint("3")),
		[]ir.Stmt{
			asn(local(carrier), local(i)),
			asn(local(carrier), bin(syntax.Add, local(carrier), cint("1"))),
		},
		[]ir.Stmt{
			def(local(i), local(carrier)),
			&ir.Node{Op: ir.OCall, X: local(use), Args: []ir.Expr{addrOf(local(i))}, Type: tVoid},
		})
	fn := fun("f", []*ir.Object{carrier, i}, loop, ret())
	f := build(t, fn)

	if len(f.Frame) != 1 || f.Frame[0] != i {
		t.Fatalf("frame is %v, want the address-taken loop variable alone\n%s", f.Frame, f)
	}
	stores := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpStore {
				continue
			}
			stores++
			if b == f.Entry {
				t.Errorf("the declaration of %s is in the entry block, so every "+
					"iteration reads what the last one left\n%s", i.Name, f)
			}
			if a := v.Args[0]; a.Op != OpLocalAddr || a.Aux != any(i) {
				t.Errorf("the store writes %v and not the loop variable's slot\n%s", a.LongString(), f)
			}
		}
	}
	if stores != 1 {
		t.Errorf("got %d stores, want the one the declaration is\n%s", stores, f)
	}
	// The post list ran: the copy back reads the slot and the increment adds.
	if n := countOp(f, OpAdd); n != 1 {
		t.Errorf("got %d additions, want the loop increment\n%s", n, f)
	}
	loads := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpLoad && v.Args[0].Op == OpLocalAddr && v.Args[0].Aux == any(i) {
				loads++
			}
		}
	}
	if loads == 0 {
		t.Errorf("nothing reads the loop variable's slot, so the copy back is gone\n%s", f)
	}
}

// TestBuildForPostStatementsRun asserts a for statement's post list is built.
//
// The post statements are in Post. Construction read them out of Else until
// now, which is the field an if uses for something else, and ir.Build has
// never written them there: every counted loop of the corpus lost its
// increment and ran forever.
func TestBuildForPostStatementsRun(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	fn := fun("f", []*ir.Object{x},
		forStmt(
			[]ir.Stmt{def(local(x), cint("0"))},
			cmp(syntax.Lss, local(x), cint("3")),
			[]ir.Stmt{asn(local(x), bin(syntax.Add, local(x), cint("1")))},
			nil),
		ret(local(x)))
	f := build(t, fn)
	if n := countOp(f, OpAdd); n != 1 {
		t.Fatalf("got %d additions, want the post statement's\n%s", n, f)
	}
	// The increment feeds the phi of the loop header, which is the same thing
	// as saying it runs on the back edge.
	phis := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpPhi {
				continue
			}
			phis++
			fed := false
			for _, a := range v.Args {
				if a.Op == OpAdd {
					fed = true
				}
			}
			if !fed {
				t.Errorf("the loop phi %v does not read the increment\n%s", v.LongString(), f)
			}
		}
	}
	if phis != 1 {
		t.Errorf("got %d phis, want the one the induction variable is\n%s", phis, f)
	}
}

// TestBuildExpressionInitRuns asserts the statements an expression carries in
// Init are built.
//
// ir.Build puts statements there where there is no enclosing statement list to
// put them in, which is a loop condition and the right operand of && and ||.
// Both are evaluated somewhere other than where the statement holding them is,
// so the statements have to be built on the path that reaches the operand.
// Construction dropped them until now, which left every temporary they assign
// holding the zero the entry block wrote.
func TestBuildExpressionInitRuns(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	next := obj("next", tFunc, ir.ClassFunc)
	cond := cmp(syntax.Lss, local(x), cint("3"))
	// The temporary the condition needs, assigned immediately before it and
	// therefore once per iteration.
	cond.Init = []ir.Stmt{asn(local(x), call(next, tInt))}

	fn := fun("f", []*ir.Object{x},
		forStmt(nil, cond, nil, nil),
		ret(local(x)))
	f := build(t, fn)

	calls := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpStaticCall {
				continue
			}
			calls++
			if b == f.Entry {
				t.Errorf("the condition's temporary is assigned in the entry block, "+
					"so it is not re-evaluated\n%s", f)
			}
		}
	}
	if calls != 1 {
		t.Fatalf("got %d calls, want the one the condition's Init holds\n%s", calls, f)
	}
}

// TestBuildFallthrough asserts a fallthrough enters the next clause's body
// without evaluating its case expressions.
func TestBuildFallthrough(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	y := obj("y", tInt, ir.ClassLocal)
	fn := fun("f", []*ir.Object{x, y},
		switchStmt(local(x),
			clause([]ir.Expr{cint("1")}, asn(local(y), cint("10")), fallthroughStmt()),
			clause([]ir.Expr{cint("2")}, asn(local(y), cint("20"))),
		),
		ret(local(y)))
	f := build(t, fn)

	// Two comparisons, one per case expression, and the second clause's body
	// has two predecessors: its own test and the fallthrough.
	if n := countOp(f, OpEq); n != 2 {
		t.Errorf("got %d comparisons, want one per case expression\n%s", n, f)
	}
	second := blockDefining(f, 20)
	if second == nil {
		t.Fatalf("no block assigns the second clause's value\n%s", f)
	}
	if len(second.Preds) != 2 {
		t.Errorf("the second clause's body has %d predecessors, want its test and the fallthrough\n%s",
			len(second.Preds), f)
	}
}

// blockDefining returns the block holding the constant with value n.
func blockDefining(f *Func, n int64) *Block {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpConstInt && v.AuxInt == n {
				return b
			}
		}
	}
	return nil
}

// TestBuildRejects covers the inputs construction must refuse rather than
// guess at.
func TestBuildRejects(t *testing.T) {
	tests := []struct {
		name string
		inv  Invariant
		fn   func() *ir.Func
	}{
		{"a Go-specific statement", InvGoSpecific, func() *ir.Func {
			return fun("f", nil, &ir.Node{Op: ir.ORange, Type: tVoid})
		}},
		{"a Go-specific expression", InvGoSpecific, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), &ir.Node{Op: ir.OLen, Type: tInt}))
		}},
		{"a map index", InvNone, func() *ir.Func {
			m := obj("m", mkType(&ir.Type{Kind: ir.Map, Key: tInt, Elem: tInt}), ir.ClassLocal)
			return fun("f", []*ir.Object{m}, ret(index(local(m), cint("0"), tInt)))
		}},
		{"a goto to a label that does not exist", InvNone, func() *ir.Func {
			return fun("f", nil, gotoStmt("nowhere"))
		}},
		{"a label declared twice", InvNone, func() *ir.Func {
			return fun("f", nil, label("a"), label("a"), ret())
		}},
		{"a break outside a loop", InvNone, func() *ir.Func {
			return fun("f", nil, breakStmt(""))
		}},
		{"a continue outside a loop", InvNone, func() *ir.Func {
			return fun("f", nil, contStmt(""))
		}},
		{"a continue that names a switch", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			sw := switchStmt(local(x), clause(nil, contStmt("sw")))
			return fun("f", []*ir.Object{x}, label("sw", sw), ret())
		}},
		{"two default clauses", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				switchStmt(local(x), clause(nil), clause(nil)), ret())
		}},
		{"a switch clause that is not a case", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				&ir.Node{Op: ir.OSwitch, X: local(x), Body: []ir.Stmt{ret()}})
		}},
		{"a fallthrough in the last clause", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				switchStmt(local(x), clause([]ir.Expr{cint("1")}, fallthroughStmt())), ret())
		}},
		{"a binary expression as a statement", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				bin(syntax.Add, local(x), cint("1")))
		}},
		{"a statement construction does not know", InvNone, func() *ir.Func {
			return fun("f", nil, &ir.Node{Op: ir.OField, Type: tInt})
		}},
		{"an expression construction does not know", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), &ir.Node{Op: ir.OIf, Type: tInt}))
		}},
		{"an unknown binary operator", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), bin(syntax.Tilde, local(x), cint("1"))))
		}},
		{"an unknown unary operator", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), &ir.Node{Op: ir.OUnary, Op1: syntax.Tilde, X: local(x), Type: tInt}))
		}},
		{"an unknown comparison", InvNone, func() *ir.Func {
			c := obj("c", tBool, ir.ClassLocal)
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{c, x},
				asn(local(c), cmp(syntax.Tilde, local(x), cint("1"))))
		}},
		{"a conversion with no machine meaning", InvNone, func() *ir.Func {
			s := obj("s", tString, ir.ClassLocal)
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{s, x},
				asn(local(s), &ir.Node{Op: ir.OConvert, X: local(x), Type: tString}))
		}},
		{"the address of a value-resident local", InvNone, func() *ir.Func {
			// Addrtaken is false, so classify put x in a value. Taking its
			// address anyway is the corruption specs/021 warns about.
			x := obj("x", tInt, ir.ClassLocal)
			p := obj("p", tIntPtr, ir.ClassLocal)
			return fun("f", []*ir.Object{x, p}, asn(local(p), addrOf(local(x))))
		}},
		{"an index of something that is not indexable", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x}, ret(index(local(x), cint("0"), tInt)))
		}},
		{"an index of a pointer to something that is not an array", InvNone, func() *ir.Func {
			p := obj("p", tIntPtr, ir.ClassLocal)
			return fun("f", []*ir.Object{p}, ret(index(local(p), cint("0"), tInt)))
		}},
		{"a field index out of range", InvNone, func() *ir.Func {
			p := obj("p", tStruct, ir.ClassLocal)
			return fun("f", []*ir.Object{p}, ret(field(local(p), 9, tInt)))
		}},
		{"a local with no object", InvNone, func() *ir.Func {
			return fun("f", nil, ret(&ir.Node{Op: ir.OLocal, Type: tInt}))
		}},
		{"an address of a local with no object", InvNone, func() *ir.Func {
			p := obj("p", tIntPtr, ir.ClassLocal)
			return fun("f", []*ir.Object{p},
				asn(local(p), addrOf(&ir.Node{Op: ir.OLocal, Type: tInt})))
		}},
		{"a global with no object", InvNone, func() *ir.Func {
			return fun("f", nil, asn(&ir.Node{Op: ir.OGlobal, Type: tInt}, cint("1")))
		}},
		{"a constant with no type", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x}, asn(local(x), &ir.Node{Op: ir.OConst, Val: litVal("1")}))
		}},
		{"a conversion with no type", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), &ir.Node{Op: ir.OConvert, X: local(x)}))
		}},
		{"a call with no function", InvNone, func() *ir.Func {
			return fun("f", nil, &ir.Node{Op: ir.OCall, Type: tVoid})
		}},
		{"an indirect call through something that is not a function", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				&ir.Node{Op: ir.OCall, X: local(x), Type: tVoid})
		}},
		{"an index with no index expression", InvNone, func() *ir.Func {
			a := obj("a", tArr4, ir.ClassLocal)
			return fun("f", []*ir.Object{a},
				ret(&ir.Node{Op: ir.OIndex, X: local(a), Type: tInt}))
		}},
		{"an index of an expression with no type", InvNone, func() *ir.Func {
			return fun("f", nil,
				ret(&ir.Node{Op: ir.OIndex, X: &ir.Node{Op: ir.OConst}, Y: cint("0"), Type: tInt}))
		}},
		{"an assignment with no destination", InvNone, func() *ir.Func {
			return fun("f", nil, &ir.Node{Op: ir.OAssign, Y: cint("1"), Type: tVoid})
		}},
		{"an assignment with no value", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				&ir.Node{Op: ir.OAssign, X: local(x), Type: tVoid})
		}},
		{"a multi-value assignment with no value", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			y := obj("y", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x, y},
				&ir.Node{Op: ir.OAssign, Args: []ir.Expr{local(x), local(y)}, Type: tVoid})
		}},
		{"a multi-value assignment from something that is not a call", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			y := obj("y", tInt, ir.ClassLocal)
			m := obj("m", mkType(&ir.Type{Kind: ir.Map, Key: tInt, Elem: tInt}), ir.ClassLocal)
			return fun("f", []*ir.Object{x, y, m},
				multiAsn(index(local(m), cint("0"), tInt), local(x), local(y)))
		}},
		{"a multi-value assignment from a call with no type", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			y := obj("y", tInt, ir.ClassLocal)
			callee := obj("callee", tFunc, ir.ClassFunc)
			return fun("f", []*ir.Object{x, y},
				multiAsn(&ir.Node{Op: ir.OCall, X: local(callee)}, local(x), local(y)))
		}},
		{"an operand that is a call through something that is not a function", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				asn(local(x), &ir.Node{Op: ir.OCall, X: cint("1"), Type: tInt}))
		}},
		{"a multi-value assignment whose destinations do not match the results", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			y := obj("y", tInt, ir.ClassLocal)
			callee := obj("callee", tFunc, ir.ClassFunc)
			return fun("f", []*ir.Object{x, y},
				multiAsn(call(callee, tInt), local(x), local(y)))
		}},
		{"the address of an expression that has none", InvNone, func() *ir.Func {
			callee := obj("callee", tFunc, ir.ClassFunc)
			p := obj("p", tIntPtr, ir.ClassLocal)
			return fun("f", []*ir.Object{p},
				asn(local(p), addrOf(call(callee, tInt))))
		}},
		{"an object with no type", InvNone, func() *ir.Func {
			x := &ir.Object{Name: "x", Class: ir.ClassLocal}
			return fun("f", []*ir.Object{x}, ret())
		}},
		{"two gotos to labels that do not exist", InvNone, func() *ir.Func {
			// The second error must not replace the first one, so that the
			// message names where the trouble started.
			return fun("f", nil, gotoStmt("a"), label("b"), gotoStmt("c"))
		}},
		{"a field of an expression with no type", InvNone, func() *ir.Func {
			return fun("f", nil,
				ret(&ir.Node{Op: ir.OField, X: &ir.Node{Op: ir.OConst}, Type: tInt}))
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Build(tc.fn())
			if err == nil {
				t.Fatalf("Build accepted the input\n%s", f)
			}
			e, ok := err.(*Error)
			if !ok {
				t.Fatalf("Build returned %T, want *ssa.Error", err)
			}
			if e.Invariant != tc.inv {
				t.Errorf("Build reported %v, want %v: %v", e.Invariant, tc.inv, e)
			}
			if !strings.Contains(e.Error(), "ssa: ") {
				t.Errorf("error does not name the package: %v", e)
			}
		})
	}
}

func TestBuildNilFunc(t *testing.T) {
	if _, err := Build(nil); err == nil {
		t.Fatal("Build(nil) returned no error")
	}
}

// countOp counts the values with one operation.
func countOp(f *Func, op Op) int {
	n := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == op {
				n++
			}
		}
	}
	return n
}

// distinctArgs counts the arguments of v that are not v itself.
func distinctArgs(v *Value) int {
	n := 0
	seen := make([]*Value, 0, len(v.Args))
	for _, a := range v.Args {
		if a == v {
			continue
		}
		found := false
		for _, s := range seen {
			if s == a {
				found = true
				break
			}
		}
		if !found {
			seen = append(seen, a)
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// String concatenation, which specs/020-ir.md's table makes a runtime call

// concatCallOf returns the single static call in f and the symbol it names.
func concatCallOf(t *testing.T, f *Func) (*Value, string) {
	t.Helper()
	var call *Value
	n := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpStaticCall {
				continue
			}
			n++
			call = v
		}
	}
	if n != 1 {
		t.Fatalf("%d calls, want one\n%s", n, f)
	}
	o, _ := call.Aux.(*ir.Object)
	if o == nil {
		t.Fatalf("the call names %v", call.Aux)
	}
	return call, o.Name
}

// TestBuildStringConcat asserts the row of specs/020-ir.md's table: a + b over
// two strings is runtime.concatstring2 and never an Add.
//
// The builder is where the table puts it. Nothing below it can build the call
// as well: lowering would have to invent the operand order that Go's
// evaluation rules fix, and the corpus measured the cost of not building it
// here at 49 functions that could not lower.
func TestBuildStringConcat(t *testing.T) {
	s := obj("s", tString, ir.ClassLocal)
	a := obj("a", tString, ir.ClassLocal)
	c := obj("c", tString, ir.ClassLocal)
	fn := fun("cat", []*ir.Object{s, a, c},
		asn(local(s), bin(syntax.Add, local(a), local(c))),
		ret(local(s)),
	)
	f := build(t, fn)

	if n := countOp(f, OpAdd); n != 0 {
		t.Errorf("%d string adds survived construction, want none\n%s", n, f)
	}
	call, name := concatCallOf(t, f)
	if name != "runtime.concatstring2" {
		t.Errorf("the call is to %s", name)
	}
	// The temporary buffer, the two operands, and the memory the call is
	// ordered after.
	if len(call.Args) != 4 {
		t.Fatalf("the call takes %d arguments, want 4: %s", len(call.Args), call.LongString())
	}
	if call.Args[0].Op != OpConstNil {
		t.Errorf("the buffer is %s, want nil so that the runtime allocates", call.Args[0].LongString())
	}
	if !IsMemory(call.Args[3]) {
		t.Errorf("the last argument is %s, want memory", call.Args[3].LongString())
	}
	sel := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpSelectN && v.Args[0] == call {
				sel++
				if v.AuxInt != 0 || v.Type != tString {
					t.Errorf("the result is %s", v.LongString())
				}
			}
		}
	}
	if sel != 1 {
		t.Errorf("%d reads of the result, want one", sel)
	}
}

// TestBuildStringConcatChain asserts a chain is one call per runtime symbol
// and not one call per +.
//
// a + b + c allocates twice as two calls and once as one, and the runtime has
// a symbol for each width up to five. The sixth operand is the interesting
// one: there is no concatstring6, so the chain nests, and the bound is what
// rtsym carries rather than what the parser produced.
func TestBuildStringConcatChain(t *testing.T) {
	tests := []struct {
		operands int
		want     string
		calls    int
	}{
		{2, "runtime.concatstring2", 1},
		{3, "runtime.concatstring3", 1},
		{4, "runtime.concatstring4", 1},
		{5, "runtime.concatstring5", 1},
		{6, "", 2},
	}
	for _, tc := range tests {
		locals := []*ir.Object{obj("s", tString, ir.ClassLocal)}
		var sum ir.Expr
		for i := 0; i < tc.operands; i++ {
			o := obj("a", tString, ir.ClassLocal)
			locals = append(locals, o)
			if sum == nil {
				sum = local(o)
				continue
			}
			sum = bin(syntax.Add, sum, local(o))
		}
		fn := fun("cat", locals, asn(local(locals[0]), sum), ret(local(locals[0])))
		f := build(t, fn)

		if n := countOp(f, OpAdd); n != 0 {
			t.Errorf("%d operands: %d adds survived\n%s", tc.operands, n, f)
		}
		if n := countOp(f, OpStaticCall); n != tc.calls {
			t.Errorf("%d operands: %d calls, want %d\n%s", tc.operands, n, tc.calls, f)
		}
		if tc.calls != 1 {
			continue
		}
		call, name := concatCallOf(t, f)
		if name != tc.want {
			t.Errorf("%d operands: the call is to %s, want %s", tc.operands, name, tc.want)
		}
		// The buffer, one argument per operand, and memory.
		if len(call.Args) != tc.operands+2 {
			t.Errorf("%d operands: the call takes %d arguments: %s",
				tc.operands, len(call.Args), call.LongString())
		}
	}
}

// TestBuildStringConcatEvaluationOrder asserts the operands are evaluated left
// to right and before the call.
//
// Go evaluates the operands of an expression in source order, and a call in
// one of them is observable. Flattening a chain moves operands into one
// argument list, which is exactly where that order can be lost.
func TestBuildStringConcatEvaluationOrder(t *testing.T) {
	s := obj("s", tString, ir.ClassLocal)
	f1 := obj("f1", tFunc, ir.ClassFunc)
	f2 := obj("f2", tFunc, ir.ClassFunc)
	f3 := obj("f3", tFunc, ir.ClassFunc)
	fn := fun("cat", []*ir.Object{s},
		asn(local(s), bin(syntax.Add,
			bin(syntax.Add, call(f1, tString), call(f2, tString)),
			call(f3, tString))),
		ret(local(s)),
	)
	f := build(t, fn)

	var order []string
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpStaticCall {
				continue
			}
			o, _ := v.Aux.(*ir.Object)
			if o != nil {
				order = append(order, o.Name)
			}
		}
	}
	want := "f1 f2 f3 runtime.concatstring3"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("call order\ngot:  %s\nwant: %s\n%s", got, want, f)
	}
}

// ---------------------------------------------------------------------------
// The corpus

// The corpus test of construction itself.
//
// specs/021-ssa-construction.md's headline number is how many of the typed
// tree's functions reach SSA, and until this test existed it was measured by
// hand. The two corpus tests below construction, ssa/decompose_test.go and
// ssa/stackmap_test.go, both walk the same 536 packages and both return
// silently when Build refuses a function, so neither of their numbers can go
// down and neither says what was refused. This one counts the refusals and
// reports them by cause, which is what turns the acceptance rate into a
// measurement rather than a filter.
//
// The importer is this file's own rather than shared with the two tests below.
// They are in the external test package, because they lower with ssa/rules,
// and this test is in the internal one, because it reads the builder's own
// error type.

type bcPkg struct {
	pkg   *types2.Package
	files []*syntax.File
	info  *types2.Info
	err   error
}

type bcImporter struct {
	fset *syntax.FileSet
	done map[string]*bcPkg
}

func newBCImporter() *bcImporter {
	return &bcImporter{fset: syntax.NewFileSet(), done: make(map[string]*bcPkg)}
}

func (imp *bcImporter) Import(path string) (*types2.Package, error) {
	r := imp.check(path)
	if r.err != nil {
		return nil, r.err
	}
	return r.pkg, nil
}

func (imp *bcImporter) check(path string) *bcPkg {
	if have, ok := imp.done[path]; ok {
		if have == nil {
			return &bcPkg{err: fmt.Errorf("import cycle at %s", path)}
		}
		return have
	}
	if path == "unsafe" {
		r := &bcPkg{pkg: types2.Unsafe}
		imp.done[path] = r
		return r
	}
	// The nil entry is the cycle marker. It is written before the recursive
	// check and replaced by the result, so a package that imports itself
	// through the graph gets an error rather than an endless walk.
	imp.done[path] = nil
	r := &bcPkg{}
	imp.done[path] = r

	bp, err := gobuild.Import(path, "", 0)
	if err != nil {
		for _, prefix := range []string{"vendor/", "cmd/vendor/"} {
			if bp2, err2 := gobuild.Import(prefix+path, "", 0); err2 == nil {
				bp, err = bp2, nil
				break
			}
		}
	}
	if err != nil {
		r.err = err
		return r
	}
	if len(bp.CgoFiles) > 0 || len(bp.GoFiles) == 0 {
		r.err = fmt.Errorf("%s has no plain Go files", path)
		return r
	}
	for _, name := range bp.GoFiles {
		file, err := syntax.ParseFile(imp.fset, filepath.Join(bp.Dir, name), nil, nil, 0)
		if err != nil || file == nil {
			r.err = fmt.Errorf("parse %s: %v", name, err)
			return r
		}
		r.files = append(r.files, file)
	}
	r.info = &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{Fset: imp.fset, Importer: imp, Sizes: types2.SizesFor("gc", "arm64")}
	r.pkg, r.err = conf.Check(path, r.files, r.info)
	return r
}

// bcPaths returns the package directories under src.
func bcPaths(t *testing.T, src string, all bool) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != src && (name == "testdata" || name == "vendor" ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(paths)
	if all {
		return paths
	}
	// The unattended run takes a sample and leaves the tool chain out, as the
	// corpus tests below this one do. CI sets NANOGO_REQUIRE_CORPUS.
	var library []string
	for _, p := range paths {
		if !strings.HasPrefix(p, "cmd/") && p != "cmd" && p != "unsafe" {
			library = append(library, p)
		}
	}
	const sample = 40
	if len(library) > sample {
		step := len(library) / sample
		var taken []string
		for i := 0; i < len(library); i += step {
			taken = append(taken, library[i])
		}
		library = taken
	}
	return library
}

// buildCounts is one run of the construction corpus.
type buildCounts struct {
	pkgs  int
	funcs int
	built int

	// verifyNG counts a function Build accepted and Verify rejected. It is
	// counted apart from a refusal because it is worse than one: a refusal is
	// visible and a malformed graph is not.
	verifyNG int

	// refused counts the functions Build refused, by cause. The key is the
	// error detail with the parts that name a type or an object removed, so
	// that one cause is one row rather than one row per type.
	refused map[string]int
}

// bcCause reduces a construction error to the cause it reports.
//
// The detail of an unsupported form is "<op>: <what> is not built yet" and the
// detail of a Go-specific node is "<op> reached SSA construction". Both are
// already the cause. What is stripped is the tail of the few details that
// print a type, because "a conversion from int to any" and "a conversion from
// string to any" are one cause and two rows would hide it.
func bcCause(err error) string {
	e, ok := err.(*Error)
	if !ok {
		return "not a construction error"
	}
	d := e.Detail
	head := ""
	if i := strings.Index(d, ": "); i >= 0 {
		head, d = d[:i+2], d[i+2:]
	}
	for _, prefix := range []string{
		"a conversion from ", "an index of ", "operator ", "unary ",
		"comparison ", "the address of ", "field ",
	} {
		if strings.HasPrefix(d, prefix) {
			return head + prefix + "..."
		}
	}
	return head + d
}

// TestBuildCorpus measures how much of the Go distribution reaches SSA.
//
// The log line is deliberately worded so that it cannot be confused with the
// one ssa/decompose_test.go prints. internal/hygiene scrapes a count out of
// that line by pattern, and two lines matching one pattern would make the gate
// read whichever test happened to run first.
func TestBuildCorpus(t *testing.T) {
	required := os.Getenv("NANOGO_REQUIRE_CORPUS") == "1"
	src := filepath.Join(runtime.GOROOT(), "src")
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		if required {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s is not there", src)
		}
		t.Skipf("no corpus at %s", src)
	}

	imp := newBCImporter()
	c := &buildCounts{refused: make(map[string]int)}
	for _, path := range bcPaths(t, src, required) {
		if path == "unsafe" {
			continue
		}
		r := imp.check(path)
		if r.err != nil || r.pkg == nil {
			continue
		}
		pkg, _ := ir.Build(r.pkg, r.files, r.info)
		if pkg == nil {
			continue
		}
		c.pkgs++
		fns := append(append([]*ir.Func{}, pkg.Funcs...), pkg.Inits...)
		for _, fn := range fns {
			c.funcs++
			f, err := Build(fn)
			if err != nil || f == nil {
				c.refused[bcCause(err)]++
				continue
			}
			c.built++
			if vs := Verify(f); len(vs) != 0 {
				c.verifyNG++
				if c.verifyNG < 4 {
					t.Errorf("%s: %s built and did not verify: %v", path, fn.Name, vs)
				}
			}
		}
	}

	t.Logf("construction corpus: %d packages, %d functions built of %d",
		c.pkgs, c.built, c.funcs)
	bcLogRefusals(t, c.refused)
	if c.verifyNG != 0 {
		t.Errorf("%d functions built and did not verify", c.verifyNG)
	}
	if c.funcs == 0 {
		t.Fatal("the corpus produced no function")
	}
	if required && c.built < 1000 {
		t.Fatalf("only %d functions were built; the corpus collapsed", c.built)
	}
}

// bcLogRefusals prints the causes, largest first.
//
// By count and then by name, from a sorted slice of keys rather than from a
// range over the map: specs/053-determinism.md forbids output that depends on
// map order, and a report whose rows move between runs cannot be diffed.
func bcLogRefusals(t *testing.T, m map[string]int) {
	t.Helper()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		t.Logf("refused %6d  %s", m[k], k)
	}
}

// TestBuildReadsTheContextRegister is the callee half of
// specs/033-closures-defer-panic.md's closure object.
//
// A function literal that captures reads its captures off an object whose
// address the caller left in the context register. Construction is where that
// address becomes a value: OpGetClosurePtr, in the entry block, because a call
// overwrites the register.
func TestBuildReadsTheContextRegister(t *testing.T) {
	ctx := obj("ctx", tIntPtr, ir.ClassParam)
	x := obj("x", tInt, ir.ClassLocal)
	fn := fun("closure", []*ir.Object{x},
		asn(local(x), deref(local(ctx), tInt)),
		ret(local(x)),
	)
	fn.Closure = ctx
	f := build(t, fn)
	if !f.NeedCtxt {
		t.Error("the function reads the context register and NeedCtxt is false")
	}
	if n := countOp(f, OpGetClosurePtr); n != 1 {
		t.Fatalf("got %d GetClosurePtr values, want 1", n)
	}
	var got *Value
	for _, v := range f.Entry.Values {
		if v.Op == OpGetClosurePtr {
			got = v
		}
	}
	if got == nil {
		t.Fatalf("GetClosurePtr is not in the entry block:\n%s", f)
	}
	if got.Aux != ctx {
		t.Errorf("GetClosurePtr names %v, want the context parameter", got.Aux)
	}
	if got.Type != tIntPtr {
		t.Errorf("GetClosurePtr is %v, want the context parameter's type", got.Type)
	}
	if len(got.Args) != 0 {
		t.Errorf("GetClosurePtr takes %d arguments, want none", len(got.Args))
	}
}

// TestBuildWithoutAContextRegister is the other half of the rule above. A
// function with no closure must not read the register, because the caller left
// nothing in it and the stack-growth tail is chosen by the same answer.
func TestBuildWithoutAContextRegister(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	f := build(t, fun("plain", []*ir.Object{x}, asn(local(x), cint("1")), ret(local(x))))
	if f.NeedCtxt {
		t.Error("a function with no closure reports NeedCtxt")
	}
	if n := countOp(f, OpGetClosurePtr); n != 0 {
		t.Errorf("a function with no closure reads the context register %d times", n)
	}
}

// TestBuildInterfaceConversionByLeadingWord pins the distinction the identity
// path used to miss.
//
// A non-empty interface leads with an *itab and an empty one leads with a
// *_type. Both are two words of the same size and both report kind Interface,
// so a conversion between them satisfies "same kind, same size" and used to
// return its operand unchanged. That left an *itab where the runtime reads a
// type descriptor, which is why panic(err) died inside the runtime with "name
// offset out of range" instead of printing its value.
//
// The pair is not symmetric. Reading the descriptor out of an itab is a load,
// and it is built. Going the other way is not a load at all: an *itab is the
// method table of one (interface, concrete type) pair and nothing in an
// interface value holds one that was not put there, so the conversion is a
// runtime lookup and it is refused by name.
func TestBuildInterfaceConversionByLeadingWord(t *testing.T) {
	tEmpty := mkType(&ir.Type{Kind: ir.Interface, EmptyIface: true})
	tIface := mkType(&ir.Type{Kind: ir.Interface})
	if tEmpty.Size != tIface.Size {
		t.Fatalf("the two interface layouts are %d and %d bytes, so this test no longer pins what it says", tEmpty.Size, tIface.Size)
	}

	for _, tt := range []struct {
		name     string
		from, to *ir.Type
		refuse   bool
		// identity reports that the value comes out of the conversion
		// unchanged, which is true only when the two are one type.
		identity bool
	}{
		{"non-empty to empty", tIface, tEmpty, false, false},
		{"empty to non-empty", tEmpty, tIface, true, false},
		{"empty to empty", tEmpty, tEmpty, false, true},
		{"non-empty to non-empty", tIface, tIface, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := obj("src", tt.from, ir.ClassLocal)
			dst := obj("dst", tt.to, ir.ClassLocal)
			fn := fun("convert", []*ir.Object{src, dst},
				asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tt.to}),
			)
			f, err := Build(fn)
			if tt.refuse {
				if err == nil {
					t.Fatal("the conversion built, so an itab reaches a slot the runtime reads a type descriptor out of")
				}
				return
			}
			if err != nil {
				t.Fatalf("the conversion was refused: %v", err)
			}
			if made := findOp(f, OpIMake) != nil; made == tt.identity {
				t.Errorf("the conversion made a new interface value: %v, and the identity was expected: %v\n%s", made, tt.identity, f)
			}
		})
	}
}

// TestBuildRefusesAConversionBetweenTwoDifferentInterfaces pins the second
// half of the identity question, which the leading-word test above does not
// reach.
//
// io.ReadWriter and io.Reader are both non-empty, both 16 bytes, both kind
// Interface, and both lead with an *itab. Every shape test says the two are
// the same, and they are not: an itab holds the concrete type's methods in the
// order its own interface lists them, so an itab built for a two-method
// interface has a second entry where a one-method interface reads none, and
// passing the value along unchanged calls through a slot that holds another
// method.
//
// The types below are spelled with method lists rather than with names,
// because the link string is what separates them and a name would make the
// test pass on the name instead.
func TestBuildRefusesAConversionBetweenTwoDifferentInterfaces(t *testing.T) {
	sig := mkType(&ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{}})
	reader := mkType(&ir.Type{Kind: ir.Interface, Methods: []ir.Method{
		{Name: "Read", Sig: sig},
	}})
	readWriter := mkType(&ir.Type{Kind: ir.Interface, Methods: []ir.Method{
		{Name: "Read", Sig: sig},
		{Name: "Write", Sig: sig},
	}})
	if reader.Size != readWriter.Size || reader.EmptyIface != readWriter.EmptyIface {
		t.Fatalf("the two interfaces differ in shape, so this test no longer pins what it says")
	}

	for _, tt := range []struct {
		name     string
		from, to *ir.Type
		refuse   bool
	}{
		{"wide to narrow", readWriter, reader, true},
		{"narrow to wide", reader, readWriter, true},
		{"one interface to itself", reader, reader, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := obj("src", tt.from, ir.ClassLocal)
			dst := obj("dst", tt.to, ir.ClassLocal)
			fn := fun("convert", []*ir.Object{src, dst},
				asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tt.to}),
			)
			_, err := Build(fn)
			if tt.refuse && err == nil {
				t.Fatal("the conversion built, so an itab built for one interface reaches a value of another")
			}
			if !tt.refuse && err != nil {
				t.Fatalf("a conversion from one interface to itself was refused: %v", err)
			}
		})
	}
}

// TestSameTypeAnswersFromTheLinkString pins that the identity test above is
// the link string and not the pointer.
//
// Two ir.Type values for one Go type occur across package builds, and a
// conversion between them is the identity. A test that only ever compared
// pointers would refuse one, and a shape test would accept two interfaces that
// differ.
func TestSameTypeAnswersFromTheLinkString(t *testing.T) {
	sig := mkType(&ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{}})
	one := mkType(&ir.Type{Kind: ir.Interface, Methods: []ir.Method{{Name: "Read", Sig: sig}}})
	two := mkType(&ir.Type{Kind: ir.Interface, Methods: []ir.Method{{Name: "Read", Sig: sig}}})
	other := mkType(&ir.Type{Kind: ir.Interface, Methods: []ir.Method{{Name: "Write", Sig: sig}}})
	// An interface with no method list has no spelling at all, so nothing can
	// be proved about it beyond its own identity.
	noList := mkType(&ir.Type{Kind: ir.Interface})

	if !sameType(one, two) {
		t.Error("two IR types with one link string are not reported as one type")
	}
	if sameType(one, other) {
		t.Error("two interfaces with different method sets are reported as one type")
	}
	if !sameType(noList, noList) {
		t.Error("a type with no spelling is not reported as itself")
	}
	if sameType(noList, mkType(&ir.Type{Kind: ir.Interface})) {
		t.Error("two types that cannot be spelled are reported as one type")
	}
	if sameType(nil, one) || sameType(one, nil) {
		t.Error("a nil type is reported as some type")
	}
}

// ---------------------------------------------------------------------------
// Concrete to interface

// tEface is the empty interface, spelled the way ir.Converter spells it.
var tEface = mkType(&ir.Type{Kind: ir.Interface, EmptyIface: true, Methods: []ir.Method{}})

// convertToEface builds "var dst any = src" and returns the function.
func convertToEface(from *ir.Type) *ir.Func {
	src := obj("src", from, ir.ClassLocal)
	dst := obj("dst", tEface, ir.ClassLocal)
	return fun("convert", []*ir.Object{src, dst},
		asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tEface}),
	)
}

// findOp returns the first value with this operation, or nil.
func findOp(f *Func, op Op) *Value {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == op {
				return v
			}
		}
	}
	return nil
}

// TestBuildConvertsAConcreteValueToAnEmptyInterface pins the two words of
// specs/032's conversion: the type word is the concrete type's descriptor and
// the data word follows cmd/compile's dataWord.
func TestBuildConvertsAConcreteValueToAnEmptyInterface(t *testing.T) {
	// A one-word struct holding a pointer. It is not pointer shaped and it is
	// its own data word, which is the case types.IsDirectIface answers and a
	// kind test does not.
	tBox := mkType(&ir.Type{Kind: ir.Struct, Name: "box", Fields: []ir.Field{{Name: "p", Type: tIntPtr}}})
	tUintptr := mkType(&ir.Type{Kind: ir.Uintptr, Name: "uintptr"})
	tInt32 := mkType(&ir.Type{Kind: ir.Int32, Name: "int32"})
	tInt16 := mkType(&ir.Type{Kind: ir.Int16, Name: "int16"})

	for _, tt := range []struct {
		name string
		from *ir.Type
		// box is the runtime symbol the data word calls, or "" when the value
		// is its own data word.
		box string
		// bits reports that the value is reinterpreted before the call.
		bits bool
	}{
		{"pointer", tIntPtr, "", false},
		{"one-word struct holding a pointer", tBox, "", false},
		{"int", tInt, "runtime.convT64", false},
		{"uintptr", tUintptr, "runtime.convT64", false},
		{"float64", tFloat, "runtime.convT64", true},
		{"int32", tInt32, "runtime.convT32", false},
		{"int16", tInt16, "runtime.convT16", false},
		{"string", tString, "runtime.convTstring", false},
		{"slice", tSlice, "runtime.convTslice", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := build(t, convertToEface(tt.from))

			mk := findOp(f, OpIMake)
			if mk == nil {
				t.Fatalf("no interface was made:\n%s", f)
			}
			word := mk.Args[0]
			if word.Op != OpAddr {
				t.Fatalf("the type word is %v, want the address of a descriptor", word.Op)
			}
			name, err := ir.TypeSymbol(tt.from)
			if err != nil {
				t.Fatalf("TypeSymbol(%v): %v", tt.from, err)
			}
			if o, _ := word.Aux.(*ir.Object); o == nil || o.Name != name {
				t.Errorf("the type word names %v, want %s", word.Aux, name)
			}
			if len(f.Descriptors) != 1 || f.Descriptors[0] != tt.from {
				t.Errorf("the function records %v as the descriptors it names, want just %v", f.Descriptors, tt.from)
			}

			data := mk.Args[1]
			if tt.box == "" {
				if findOp(f, OpStaticCall) != nil {
					t.Errorf("a value that is its own data word was boxed:\n%s", f)
				}
				if data.Type != tt.from {
					t.Errorf("the data word has type %v, want the value itself, which is %v", data.Type, tt.from)
				}
				return
			}
			if data.Op != OpSelectN {
				t.Fatalf("the data word is %v, want the result of a boxing call", data.Op)
			}
			call := data.Args[0]
			o, _ := call.Aux.(*ir.Object)
			if o == nil || o.Name != tt.box {
				t.Errorf("the data word calls %v, want %s", call.Aux, tt.box)
			}
			arg := call.Args[0]
			if tt.bits != (arg.Op == OpBitcast) {
				t.Errorf("the boxed argument is %v, and a reinterpretation was %v", arg.Op, tt.bits)
			}
		})
	}
}

// TestBuildCallsThroughAnItab pins the shape of a call to a method of an
// interface.
//
// Three claims, and each one is a wrong call rather than a build failure when
// it is broken. The callee is loaded out of the value's first word, which is
// the itab. The receiver is the value's second word, which is the one-word
// receiver the itab's entry point takes. And the slot is at
// internal/abi.ITab.Fun plus the method's own index, which is the offset
// rtype/itab.go writes the array at.
func TestBuildCallsThroughAnItab(t *testing.T) {
	sig := mkType(&ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{tInt}})
	tPair := mkType(&ir.Type{Kind: ir.Interface, Name: "main.pair", PkgPath: "main", Methods: []ir.Method{
		{Name: "first", Sig: sig, Pkg: "main"},
		{Name: "second", Sig: sig, Pkg: "main"},
	}})

	for slot, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			iface := obj("i", tPair, ir.ClassLocal)
			dst := obj("dst", tInt, ir.ClassLocal)
			call := &ir.Node{Op: ir.OCall, Type: tInt, X: &ir.Node{
				Op: ir.OField, Type: sig, X: local(iface), Index: slot,
			}}
			f := build(t, fun("call", []*ir.Object{iface, dst}, asn(local(dst), call)))

			c := findOp(f, OpInterCall)
			if c == nil {
				t.Fatalf("the call is not a call through a table:\n%s", f)
			}
			if len(c.Args) != 3 {
				t.Fatalf("the call takes %d operands, want the table, the receiver and memory", len(c.Args))
			}
			if c.Args[0].Op != OpITab {
				t.Errorf("the first operand is %v, want the interface's first word", c.Args[0].Op)
			}
			if c.Args[1].Op != OpIData {
				t.Errorf("the receiver is %v, want the interface's second word", c.Args[1].Op)
			}
			if want := int64(24 + slot*8); c.AuxInt != want {
				t.Errorf("the call reads the table at %d, want %d", c.AuxInt, want)
			}
			if findOp(f, OpClosureCall) != nil || findOp(f, OpStaticCall) != nil {
				t.Errorf("the call also built a call of another kind:\n%s", f)
			}
		})
	}
}

// TestBuildRefusesACallThroughTheEmptyInterface guards the one interface with
// no table.
//
// The empty interface declares no method, so its first word is a *_type and a
// slot read out of it at an itab's offsets is whatever the descriptor holds
// there. No selection on one is legal Go, so reaching it means the IR was
// built wrongly and the answer is an error rather than a refusal.
func TestBuildRefusesACallThroughTheEmptyInterface(t *testing.T) {
	sig := mkType(&ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{tInt}})
	iface := obj("i", tEface, ir.ClassLocal)
	dst := obj("dst", tInt, ir.ClassLocal)
	call := &ir.Node{Op: ir.OCall, Type: tInt, X: &ir.Node{
		Op: ir.OField, Type: sig, X: local(iface), Index: 0,
	}}
	_, err := Build(fun("call", []*ir.Object{iface, dst}, asn(local(dst), call)))
	if err == nil {
		t.Fatal("the call built, so a *_type is read as a method table")
	}
	if !strings.Contains(err.Error(), "method table") {
		t.Errorf("the refusal is %q and does not say what is missing", err)
	}
}

// TestBuildConvertsAConcreteValueToAnInterfaceWithMethods pins the type word
// of the other half of specs/032's conversion.
//
// An interface with methods leads with an *itab and not with a *_type, and the
// itab is per (concrete type, interface) pair. So the claim is two claims: the
// word is the address of the pair's itab symbol, and the pair is recorded for
// the object writer, which owes the bytes because cmd/link defines no
// go:itab. symbol.
func TestBuildConvertsAConcreteValueToAnInterfaceWithMethods(t *testing.T) {
	sig := mkType(&ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{tInt}})
	tCoder := mkType(&ir.Type{Kind: ir.Interface, Name: "main.coder", PkgPath: "main",
		Methods: []ir.Method{{Name: "code", Sig: sig}}})
	tSeven := mkType(&ir.Type{Kind: ir.Ptr, Name: "*main.seven", Elem: mkType(&ir.Type{
		Kind: ir.Struct, Name: "main.seven", PkgPath: "main", Fields: []ir.Field{},
		Methods: []ir.Method{{Name: "code", Sig: sig, PtrOnly: true}},
	})})

	src := obj("src", tSeven, ir.ClassLocal)
	dst := obj("dst", tCoder, ir.ClassLocal)
	f := build(t, fun("convert", []*ir.Object{src, dst},
		asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tCoder}),
		asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tCoder}),
	))

	mk := findOp(f, OpIMake)
	if mk == nil {
		t.Fatalf("no interface was made:\n%s", f)
	}
	word := mk.Args[0]
	if word.Op != OpAddr {
		t.Fatalf("the type word is %v, want the address of an itab", word.Op)
	}
	name, err := ir.ItabSymbol(tSeven, tCoder)
	if err != nil {
		t.Fatalf("ItabSymbol: %v", err)
	}
	o, _ := word.Aux.(*ir.Object)
	if o == nil || o.Name != name {
		t.Fatalf("the type word names %v, want %s", word.Aux, name)
	}
	if len(f.Itabs) != 1 || f.Itabs[0].Type != tSeven || f.Itabs[0].Iface != tCoder {
		t.Fatalf("the function records %v as the itabs it names, want the one pair once", f.Itabs)
	}
	if len(f.Descriptors) != 0 {
		t.Errorf("the conversion also recorded %v, and an itab is not a descriptor", f.Descriptors)
	}
	// The data word is the concrete value: a pointer is its own data word.
	if data := mk.Args[1]; data.Type != tSeven {
		t.Errorf("the data word has type %v, want the pointer itself, which is %v", data.Type, tSeven)
	}
}

// TestBuildRefusesADataWordItCannotBox names the two shapes construction has
// no answer for, so that neither is compiled into a wrong one.
func TestBuildRefusesADataWordItCannotBox(t *testing.T) {
	for _, tt := range []struct {
		name string
		from *ir.Type
		to   *ir.Type
		want string
	}{
		// One byte wide. cmd/compile indexes runtime.staticuint64s and this
		// does not, so there is no helper left that takes it by value.
		{"a one-byte type", tBool, tEface, "runtime.convT"},
		// Two words. convT and convTnoptr take an address and construction has
		// no frame slot to give them.
		{"a two-word struct", tStruct, tEface, "runtime.convT"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := obj("src", tt.from, ir.ClassLocal)
			dst := obj("dst", tt.to, ir.ClassLocal)
			fn := fun("convert", []*ir.Object{src, dst},
				asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tt.to}),
			)
			_, err := Build(fn)
			if err == nil {
				t.Fatal("the conversion built, so a data word nothing can fill reaches the runtime")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the refusal is %q and does not name %q", err, tt.want)
			}
		})
	}
}

// TestBuildNamesOneDescriptorPerType asserts two conversions of one type name
// one symbol.
//
// The linker deduplicates descriptors by name, so two objects here are two
// relocations against one symbol and not a bug. Naming it once is what keeps
// the record on the function a set rather than a list with repeats, which is
// what the caller emits from.
func TestBuildNamesOneDescriptorPerType(t *testing.T) {
	src := obj("src", tInt, ir.ClassLocal)
	dst := obj("dst", tEface, ir.ClassLocal)
	other := obj("other", tString, ir.ClassLocal)
	fn := fun("convert", []*ir.Object{src, dst, other},
		asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tEface}),
		asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(other), Type: tEface}),
		asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tEface}),
	)
	f := build(t, fn)

	if len(f.Descriptors) != 2 || f.Descriptors[0] != tInt || f.Descriptors[1] != tString {
		t.Fatalf("the function records %v, want int then string, each once", f.Descriptors)
	}
	var objs []*ir.Object
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpAddr {
				continue
			}
			if o, ok := v.Aux.(*ir.Object); ok {
				objs = append(objs, o)
			}
		}
	}
	if len(objs) != 3 {
		t.Fatalf("%d descriptor addresses, want one per conversion", len(objs))
	}
	if objs[0] != objs[2] {
		t.Error("two conversions of one type name two objects")
	}
}

// TestDirectIfaceIsWidthAndPointerness pins the predicate the data word turns
// on, which is not "pointer shaped".
func TestDirectIfaceIsWidthAndPointerness(t *testing.T) {
	tUintptr := mkType(&ir.Type{Kind: ir.Uintptr, Name: "uintptr"})
	tBox := mkType(&ir.Type{Kind: ir.Struct, Name: "box", Fields: []ir.Field{{Name: "p", Type: tIntPtr}}})
	tMap := mkType(&ir.Type{Kind: ir.Map, Key: tInt, Elem: tInt})

	for _, tt := range []struct {
		name string
		typ  *ir.Type
		want bool
	}{
		{"a pointer", tIntPtr, true},
		{"a map", tMap, true},
		{"a function", tFunc, true},
		{"a one-field struct holding a pointer", tBox, true},
		{"a uintptr, which is one word and holds no pointer", tUintptr, false},
		{"an int", tInt, false},
		{"a string", tString, false},
		{"a one-byte type", tBool, false},
	} {
		if got := directIface(tt.typ); got != tt.want {
			t.Errorf("directIface(%s) is %v, want %v", tt.name, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Interface to interface

// TestBuildConvertsANonEmptyInterfaceToAnEmptyOne pins the shape of the
// conversion: a guarded load of the itab's descriptor, not a reinterpretation.
func TestBuildConvertsANonEmptyInterfaceToAnEmptyOne(t *testing.T) {
	tErr := mkType(&ir.Type{Kind: ir.Interface, Name: "error", Methods: []ir.Method{
		{Name: "Error", Sig: mkType(&ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{tString}})},
	}})
	src := obj("src", tErr, ir.ClassLocal)
	dst := obj("dst", tEface, ir.ClassLocal)
	f := build(t, fun("convert", []*ir.Object{src, dst},
		asn(local(dst), &ir.Node{Op: ir.OConvert, X: local(src), Type: tEface}),
	))

	mk := findOp(f, OpIMake)
	if mk == nil {
		t.Fatalf("no interface was made:\n%s", f)
	}
	if mk.Args[1].Op != OpIData {
		t.Errorf("the data word is %v, want the source's data word carried across", mk.Args[1].Op)
	}
	word := mk.Args[0]
	if word.Op != OpPhi || len(word.Args) != 2 {
		t.Fatalf("the type word is %v with %d arguments, want a join of the two paths:\n%s", word.Op, len(word.Args), f)
	}

	// One path is the itab itself, which is nil, and the other is the load.
	var tab, load *Value
	for _, a := range word.Args {
		switch a.Op {
		case OpITab:
			tab = a
		case OpLoad:
			load = a
		}
	}
	if tab == nil || load == nil {
		t.Fatalf("the join is over %v and %v, want the itab and a load of its descriptor:\n%s", word.Args[0].Op, word.Args[1].Op, f)
	}
	if tab.Args[0] != mk.Args[1].Args[0] {
		t.Error("the two words are read out of two different values")
	}
	addr := load.Args[0]
	if addr.Op != OpOffPtr || addr.AuxInt != ir.PtrSize {
		t.Errorf("the descriptor is loaded from %v [%d], want one word into the itab", addr.Op, addr.AuxInt)
	}
	if addr.Args[0] != tab {
		t.Error("the load does not read the itab the guard tested")
	}

	// The load is guarded, because a nil interface has a nil first word.
	guard := load.Block
	if len(guard.Preds) != 1 {
		t.Fatalf("the load is in a block with %d predecessors, want the one the guard branches to", len(guard.Preds))
	}
	from := guard.Preds[0]
	if from.Kind != BlockIf {
		t.Fatalf("the block before the load is %v, want a branch on the itab", from.Kind)
	}
	if c := from.Control; c.Op != OpNeq || c.Args[0] != tab || c.Args[1].Op != OpConstNil {
		t.Errorf("the guard is %v, want the itab compared against nil", c.LongString())
	}
	if from.Succs[0] != guard {
		t.Error("the load is on the false side of the guard, so a nil interface faults")
	}

	// The word and the data word both carry a pointer type, or the collector
	// stops seeing one of them.
	for i, a := range mk.Args {
		if !a.Type.HasPointers() {
			t.Errorf("word %d has type %v, which the collector does not scan", i, a.Type)
		}
	}
}

// TestItabTypeOffsetMatchesTheRuntime reads the offset out of the runtime
// nanogo links against rather than trusting the constant.
//
// A wrong offset hands the runtime the *InterfaceType where it reads the
// *Type, and every field it reads after that is another field. That failure
// prints as a name offset out of range, deep inside the runtime, with nothing
// pointing back here.
func TestItabTypeOffsetMatchesTheRuntime(t *testing.T) {
	path := filepath.Join(runtime.GOROOT(), "src", "internal", "abi", "iface.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s does not parse: %v", path, err)
		}
		t.Skipf("no runtime source at %s: %v", path, err)
	}

	var fields []string
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "ITab" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				fields = append(fields, name.Name)
			}
		}
		return false
	})
	if len(fields) == 0 {
		t.Fatalf("%s declares no ITab, so this test no longer reads the layout it checks", path)
	}
	// Every field before Type is one word: Inter is a pointer.
	want := -1
	for i, name := range fields {
		if name == "Type" {
			want = i * int(ir.PtrSize)
			break
		}
	}
	if want < 0 {
		t.Fatalf("ITab is %v and has no Type field", fields)
	}
	if int64(want) != itabTypeOffset {
		t.Errorf("ITab is %v, so Type is at %d, and itabTypeOffset is %d", fields, want, itabTypeOffset)
	}
}

// TestBuildConstantIsAValueAndNotItsPrintedForm is the row that made this pass
// change programs it compiled.
//
// go/constant's String is a display form. It prints a float with %.6g and it
// quotes a string and truncates one longer than 72 runes with an ellipsis, so
// a builder that parsed the text back put 0.333333 where the program wrote
// 1.0/3.0 and put a quote and three dots into a long string constant. Neither
// was reported by anything: both produce a program that links and runs.
func TestBuildConstantIsAValueAndNotItsPrintedForm(t *testing.T) {
	third := constant.BinaryOp(constant.MakeInt64(1), token.QUO, constant.MakeInt64(3))
	long := strings.Repeat("0123456789", 10)
	tF64 := mkType(&ir.Type{Kind: ir.Float64})

	for _, tc := range []struct {
		what string
		t    *ir.Type
		val  constant.Value
		want any
	}{
		{"a float that no binary float holds exactly", tF64, third, 1.0 / 3.0},
		{"the largest float64", tF64,
			constant.MakeFloat64(math.MaxFloat64), math.MaxFloat64},
		{"a string longer than the display form keeps", tString,
			constant.MakeString(long), long},
	} {
		t.Run(tc.what, func(t *testing.T) {
			x := obj("x", tc.t, ir.ClassLocal)
			fn := fun("f", []*ir.Object{x},
				asn(local(x), &ir.Node{Op: ir.OConst, Type: tc.t, Val: ir.Const{Val: tc.val}}),
				ret())
			f := build(t, fn)
			var got any
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					if v.Op == OpConstFloat || v.Op == OpConstString {
						got = v.Aux
					}
				}
			}
			if got != tc.want {
				t.Errorf("the constant reached SSA as %#v, want %#v", got, tc.want)
			}
		})
	}
}
