// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"fmt"
	"go/constant"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
)

// The generated equality function.
//
// A descriptor's Equal field holds a func value the runtime calls to compare
// two values of the type. Most types reach one of the runtime's own: a string
// through strequal, a float through f64equal, one region of plain memory
// through memequal or memequal_varlen. The types left over are the ones whose
// bytes are not the value:
//
//   - a struct with padding between or after its fields, because the padding
//     holds whatever the last write left there;
//   - a struct with a blank field, which the language does not compare;
//   - a struct or an array with a field or an element that is a string, a
//     float or an interface, each of which compares by its own rule.
//
// For those, gc generates a function and rtype's algSpecial is the same set.
// The runtime panics on a map whose key type has a nil Equal, so the field
// cannot be left out and the function has to exist.
//
// # The shape, which is gc's
//
//	func(p, q *T) bool {
//	    if p.f0 == q.f0 {
//	    } else {
//	        return false
//	    }
//	    ...
//	    return true
//	}
//
// One statement per comparison and an early return, rather than one long
// conjunction. gc emits the same shape with a goto to a shared tail
// (cmd/compile/internal/reflectdata, eqFunc), and the short circuit is the
// point of it: a comparison that panics, which an interface field can, must
// not run after a comparison that already answered false.
//
// A field is compared by its own kind and never by its bytes, so padding and
// blank fields are skipped by construction rather than by arithmetic. A field
// that is itself a struct or an array is walked in the same way rather than
// compared whole, which is how a nested type's own rule reaches the leaves.
//
// # What gc does here and this does not
//
// gc collapses a run of adjacent fields that all compare as plain memory into
// one runtime.memequal call, when the run costs more than four register-sized
// loads (cmd/compile/internal/compare, EqStruct and Memrun). That is a choice
// about how many instructions the comparison takes and not about what it
// answers: gc itself compares such a run field by field below the threshold.
// This generator always takes the field-by-field path.

// algSymbolPrefix is gc's name for a generated algorithm function
// (cmd/compile/internal/reflectdata, TypeSymPrefix). The symbol is the prefix,
// then the type's link string, so two packages that generate the function for
// one type generate one symbol and the linker keeps one copy.
const (
	equalSymbolPrefix = ir.TypeSymbolPrefix + ".eq."
	hashSymbolPrefix  = ir.TypeSymbolPrefix + ".hash."
)

// EqualSymbol returns the linker symbol of the generated equality function of
// t.
//
// The name is a function of the type alone, as every symbol a descriptor names
// has to be: the descriptor is written by whichever package needs it, and two
// spellings of one type would be two functions where the linker expects one.
func EqualSymbol(t *ir.Type) (string, error) { return algSymbol(equalSymbolPrefix, t) }

// HashSymbol returns the linker symbol of the generated hash function of t.
func HashSymbol(t *ir.Type) (string, error) { return algSymbol(hashSymbolPrefix, t) }

func algSymbol(prefix string, t *ir.Type) (string, error) {
	s, err := ir.TypeLinkString(t)
	if err != nil {
		return "", err
	}
	return prefix + s, nil
}

// maxUnrolledElements bounds how many elements of an array this generator
// compares in a straight line.
//
// gc emits a loop for a long array and unrolls a short one. The loop is the
// piece that is not built here, so the bound is a refusal rather than a
// silent fall back to something slower: a generated function that took a
// different shape above a threshold would be a second code path with no test
// exercising it.
const maxUnrolledElements = 16

// EqualFunc returns the generated equality function of t.
//
// The caller decides that t needs one. This builds the function for whatever
// it is given, which is gc's split as well: geneq decides from the type's
// algorithm and eqFunc builds the body.
func EqualFunc(t *ir.Type) (*ir.Func, error) {
	sym, err := EqualSymbol(t)
	if err != nil {
		return nil, err
	}
	ptr, err := pointerTo(t)
	if err != nil {
		return nil, fmt.Errorf("ssagen: %s: %w", sym, err)
	}
	p := &ir.Object{Name: ".p", Type: ptr, Class: ir.ClassParam, Pos: wrapperPos}
	q := &ir.Object{Name: ".q", Type: ptr, Class: ir.ClassParam, Pos: wrapperPos}
	fn := &ir.Func{
		Name: sym,
		Sym:  sym,
		Pos:  wrapperPos,
		// The parameters and not a receiver: the runtime calls this through a
		// func value and it is a function of two pointers, not a method.
		Params:  []*ir.Object{p, q},
		Results: []*ir.Object{{Name: ".r", Type: boolType(), Class: ir.ClassResult, Pos: wrapperPos}},
		// Compiler-generated code the runtime must not count as a frame of
		// the program: a comparison of two interface values panics inside
		// this function, and the traceback is about the map operation that
		// reached it.
		Wrapper: true,
	}
	var body []ir.Stmt
	body, err = appendEqual(body, deref(p, t), deref(q, t), t)
	if err != nil {
		return nil, fmt.Errorf("ssagen: %s: %w", sym, err)
	}
	fn.Body = append(body, &ir.Node{
		Op: ir.OReturn, Pos: wrapperPos, Type: voidType(),
		Args: []ir.Expr{boolConst(true)},
	})
	return fn, nil
}

// appendEqual appends the statements that answer false unless x equals y.
//
// x and y are the two values, already spelled as expressions over the two
// parameters. The walk is the type's structure: a struct is its fields, an
// array is its elements, and anything else is one comparison the passes below
// know how to build. A string, a float and an interface each reach that last
// case, because specs/025-lowering-and-rules.md already expands == on them
// into the runtime call the language requires.
func appendEqual(out []ir.Stmt, x, y ir.Expr, t *ir.Type) ([]ir.Stmt, error) {
	switch t.Kind {
	case ir.Struct, ir.Tuple:
		for i, f := range t.Fields {
			if f.Name == "_" {
				// The language does not compare a blank field, and its bytes
				// are as unspecified as padding.
				continue
			}
			var err error
			out, err = appendEqual(out, field(x, i, f.Type), field(y, i, f.Type), f.Type)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case ir.Array:
		if t.Len > maxUnrolledElements {
			return nil, fmt.Errorf("an array of %d elements needs a comparison loop, which is not built", t.Len)
		}
		for i := int64(0); i < t.Len; i++ {
			var err error
			out, err = appendEqual(out, index(x, i, t.Elem), index(y, i, t.Elem), t.Elem)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case ir.Slice, ir.Map, ir.FuncKind:
		// The language forbids ==, so a type holding one of these is not
		// comparable and no caller should have asked for this function.
		return nil, fmt.Errorf("%s is not comparable", t)
	}
	// if x == y {} else { return false }
	return append(out, &ir.Node{
		Op:  ir.OIf,
		Pos: wrapperPos,
		X: &ir.Node{
			Op: ir.OCompare, Op1: syntax.Eql, Pos: wrapperPos,
			Type: boolType(), X: x, Y: y,
		},
		Else: []ir.Stmt{{
			Op: ir.OReturn, Pos: wrapperPos, Type: voidType(),
			Args: []ir.Expr{boolConst(false)},
		}},
	}), nil
}

// deref spells (*p) for a parameter holding a pointer to t.
func deref(o *ir.Object, t *ir.Type) ir.Expr {
	return &ir.Node{
		Op: ir.ODeref, Pos: wrapperPos, Type: t,
		X: &ir.Node{Op: ir.OLocal, Pos: wrapperPos, Type: o.Type, Obj: o},
	}
}

// field spells x.f for field i of a struct.
func field(x ir.Expr, i int, t *ir.Type) ir.Expr {
	return &ir.Node{Op: ir.OField, Pos: wrapperPos, Type: t, X: x, Index: i}
}

// index spells x[i] for a constant index into an array.
func index(x ir.Expr, i int64, t *ir.Type) ir.Expr {
	return &ir.Node{
		Op: ir.OIndex, Pos: wrapperPos, Type: t, X: x,
		Y: &ir.Node{
			Op: ir.OConst, Pos: wrapperPos, Type: intType(),
			Val: ir.Const{Val: constant.MakeInt64(i)},
		},
	}
}

// boolConst spells true or false.
func boolConst(v bool) ir.Expr {
	return &ir.Node{
		Op: ir.OConst, Pos: wrapperPos, Type: boolType(),
		Val: ir.Const{Val: constant.MakeBool(v)},
	}
}

// pointerTo returns *t, laid out.
func pointerTo(t *ir.Type) (*ir.Type, error) {
	p := &ir.Type{Kind: ir.Ptr, Elem: t}
	if err := ir.Layout(p); err != nil {
		return nil, err
	}
	return p, nil
}

// boolType and intType are the two predeclared types this file builds
// expressions with.
//
// They carry the predeclared name, because a descriptor written from one has
// to be the one the runtime already owns rather than a second copy under a
// name nothing resolves.
var (
	boolType = mustLayout(&ir.Type{Kind: ir.Bool, Name: "bool", Basic: "bool"})
	intType  = mustLayout(&ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"})
)

// mustLayout lays a type out once and returns it from a function, so that a
// package-level variable holding it cannot be reassigned by a caller.
func mustLayout(t *ir.Type) func() *ir.Type {
	if err := ir.Layout(t); err != nil {
		panic("ssagen: " + t.String() + " does not lay out: " + err.Error())
	}
	return func() *ir.Type { return t }
}
