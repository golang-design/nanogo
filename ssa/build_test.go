// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
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

// asn is the assignment convention of assignParts.
func asn(dst, src ir.Expr) ir.Stmt {
	return &ir.Node{Op: ir.OBinary, X: dst, Y: src}
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

// forStmt uses the convention of forParts: Else carries the post statements.
func forStmt(init []ir.Stmt, cond ir.Expr, post []ir.Stmt, body []ir.Stmt) ir.Stmt {
	return &ir.Node{Op: ir.OFor, Init: init, X: cond, Else: post, Body: body}
}

// clause uses the convention of switchCases.
func clause(exprs []ir.Expr, body ...ir.Stmt) ir.Stmt {
	return &ir.Node{Op: ir.OBlock, Args: exprs, Body: body}
}

func switchStmt(tag ir.Expr, clauses ...ir.Stmt) ir.Stmt {
	return &ir.Node{Op: ir.OSwitch, X: tag, Body: clauses}
}

func ret(args ...ir.Expr) ir.Stmt { return &ir.Node{Op: ir.OReturn, Args: args} }

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
		{"a switch clause that is not a block", InvNone, func() *ir.Func {
			x := obj("x", tInt, ir.ClassLocal)
			return fun("f", []*ir.Object{x},
				&ir.Node{Op: ir.OSwitch, X: local(x), Body: []ir.Stmt{ret()}})
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
		{"an assignment with no left side", InvNone, func() *ir.Func {
			return fun("f", nil, &ir.Node{Op: ir.OBinary, Y: cint("1")})
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
