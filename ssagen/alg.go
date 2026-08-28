// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"fmt"
	"go/constant"
	"strings"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtsym"
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
//
// gc also marks its function Noinline and turns nil checks off inside it, and
// does not mark it a wrapper. The two functions here carry ir.Func.Wrapper
// instead, because a comparison of two interface values panics inside them and
// the traceback is about the map operation that reached them, not about a
// frame of the program. What that changes is a traceback and nothing else.

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

// GeneratedAlg reports whether name is the symbol of a generated equality or
// hash function.
//
// The driver scans a descriptor's relocations for the functions the descriptor
// makes it owe, and resolves each name against the closed descriptor set. A
// name in this family that the set does not hold is a descriptor naming a
// function nobody will write, which is an undefined symbol at link, so the
// driver has to be able to tell such a name from every other target a
// descriptor points at.
//
// The two prefixes end in a dot, so neither matches the closure symbols
// type:.eqfunc. and type:.hashfunc. that point at these functions.
func GeneratedAlg(name string) bool {
	return strings.HasPrefix(name, equalSymbolPrefix) || strings.HasPrefix(name, hashSymbolPrefix)
}

func algSymbol(prefix string, t *ir.Type) (string, error) {
	s, err := ir.TypeLinkString(t)
	if err != nil {
		return "", err
	}
	return prefix + s, nil
}

// maxUnrolledElements bounds how many elements of an array this generator
// walks in a straight line. Past it the walk is a counted loop over one
// element.
//
// gc does the same and chooses the bound by bytes rather than by elements:
// reflectdata's unrollSize is 32, so it unrolls 32/elemsize elements per
// iteration and runs the loop for the rest. The shape here is one element per
// iteration, which is the same answer computed with more branches, and the
// bound is in elements because this generator walks a type and not a size.
//
// Both shapes are tested. A generated function that took a different shape
// above a threshold with nothing exercising it would be a second code path
// nobody reads, which is what this constant used to be the refusal for.
const maxUnrolledElements = 16

// arrayLoop appends a counted loop over the elements of an array.
//
//	for .i := 0; .i < n; .i++ {
//		<body(.i)>
//	}
//
// The index is a local of the function being generated, because a loop needs
// storage the passes below can spill. Every array in one function gets its own
// index, so a nested array is a nested loop with two variables rather than one
// shared counter walking two lengths.
//
// body builds the statements for one element and is given the index to spell
// x[.i] with. It is called once, which is what makes this a loop rather than
// an unrolling.
func arrayLoop(fn *ir.Func, out []ir.Stmt, n int64, body func(idx ir.Expr) ([]ir.Stmt, error)) ([]ir.Stmt, error) {
	i := &ir.Object{
		Name:      fmt.Sprintf(".i%d", len(fn.Locals)),
		Type:      intType(),
		Class:     ir.ClassLocal,
		Pos:       wrapperPos,
		Addrtaken: false,
	}
	fn.Locals = append(fn.Locals, i)
	idx := func() ir.Expr {
		return &ir.Node{Op: ir.OLocal, Pos: wrapperPos, Type: intType(), Obj: i}
	}
	inner, err := body(idx())
	if err != nil {
		return nil, err
	}
	return append(out, &ir.Node{
		Op:  ir.OFor,
		Pos: wrapperPos,
		Init: []ir.Stmt{{
			Op: ir.OAssign, Pos: wrapperPos, Type: voidType(),
			X: idx(), Y: intConst(0),
		}},
		X: &ir.Node{
			Op: ir.OCompare, Op1: syntax.Lss, Pos: wrapperPos,
			Type: boolType(), X: idx(), Y: intConst(n),
		},
		Body: inner,
		Post: []ir.Stmt{{
			Op: ir.OAssign, Pos: wrapperPos, Type: voidType(),
			X: idx(),
			Y: &ir.Node{
				Op: ir.OBinary, Op1: syntax.Add, Pos: wrapperPos,
				Type: intType(), X: idx(), Y: intConst(1),
			},
		}},
	}), nil
}

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
	body, err = appendEqual(fn, body, deref(p, t), deref(q, t), t)
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
func appendEqual(fn *ir.Func, out []ir.Stmt, x, y ir.Expr, t *ir.Type) ([]ir.Stmt, error) {
	switch t.Kind {
	case ir.Struct, ir.Tuple:
		for i, f := range t.Fields {
			if f.Name == "_" {
				// The language does not compare a blank field, and its bytes
				// are as unspecified as padding.
				continue
			}
			var err error
			out, err = appendEqual(fn, out, field(x, i, f.Type), field(y, i, f.Type), f.Type)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case ir.Array:
		if t.Len > maxUnrolledElements {
			return arrayLoop(fn, out, t.Len, func(i ir.Expr) ([]ir.Stmt, error) {
				return appendEqual(fn, nil, indexBy(x, i, t.Elem), indexBy(y, i, t.Elem), t.Elem)
			})
		}
		for i := int64(0); i < t.Len; i++ {
			var err error
			out, err = appendEqual(fn, out, index(x, i, t.Elem), index(y, i, t.Elem), t.Elem)
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
	return indexBy(x, intConst(i), t)
}

// indexBy spells x[i] for an index this generator computes, which is the loop
// variable of arrayLoop.
func indexBy(x ir.Expr, i ir.Expr, t *ir.Type) ir.Expr {
	return &ir.Node{Op: ir.OIndex, Pos: wrapperPos, Type: t, X: x, Y: i}
}

// intConst spells an int constant.
func intConst(i int64) ir.Expr {
	return &ir.Node{
		Op: ir.OConst, Pos: wrapperPos, Type: intType(),
		Val: ir.Const{Val: constant.MakeInt64(i)},
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

// The generated hash function.
//
// A map descriptor's Hasher holds a func value the runtime calls to hash a
// key. The set of types that need a generated one is the set that needs a
// generated equality function, and that is not a coincidence: two values that
// compare equal have to hash alike or a map loses keys it holds, so gc derives
// both from one algorithm and rtype reproduces that.
//
// # The shape, which is the runtime's own walk
//
//	func(p *T, h uintptr) uintptr {
//	    h = memhash32(&p.f0, h)
//	    h = strhash(&p.f1, h)
//	    ...
//	    return h
//	}
//
// One call per leaf, in field order, with the function chosen by the leaf's
// kind. That is what runtime.typehash does for a type it has no Hasher for
// (runtime/alg.go): it recurses through a struct's fields and an array's
// elements and hashes each leaf by its kind, skipping blank fields and never
// reading padding.
//
// gc collapses a run of adjacent memory-comparable fields into one memhash
// over the run, which this does not, for the reason the equality generator
// gives. The two shapes hash to different values and neither is more correct:
// a map uses one hasher for its key type and never mixes two.

// HashFunc returns the generated hash function of t.
func HashFunc(t *ir.Type) (*ir.Func, error) {
	sym, err := HashSymbol(t)
	if err != nil {
		return nil, err
	}
	ptr, err := pointerTo(t)
	if err != nil {
		return nil, fmt.Errorf("ssagen: %s: %w", sym, err)
	}
	p := &ir.Object{Name: ".p", Type: ptr, Class: ir.ClassParam, Pos: wrapperPos}
	h := &ir.Object{Name: ".h", Type: uintptrType(), Class: ir.ClassParam, Pos: wrapperPos}
	fn := &ir.Func{
		Name:    sym,
		Sym:     sym,
		Pos:     wrapperPos,
		Params:  []*ir.Object{p, h},
		Results: []*ir.Object{{Name: ".r", Type: uintptrType(), Class: ir.ClassResult, Pos: wrapperPos}},
		Wrapper: true,
	}
	body, err := appendHash(fn, nil, h, deref(p, t), t)
	if err != nil {
		return nil, fmt.Errorf("ssagen: %s: %w", sym, err)
	}
	fn.Body = append(body, &ir.Node{
		Op: ir.OReturn, Pos: wrapperPos, Type: voidType(),
		Args: []ir.Expr{{Op: ir.OLocal, Pos: wrapperPos, Type: uintptrType(), Obj: h}},
	})
	return fn, nil
}

// appendHash appends the statements that fold x into h.
func appendHash(fn *ir.Func, out []ir.Stmt, h *ir.Object, x ir.Expr, t *ir.Type) ([]ir.Stmt, error) {
	switch t.Kind {
	case ir.Struct, ir.Tuple:
		for i, f := range t.Fields {
			if f.Name == "_" {
				continue
			}
			var err error
			if out, err = appendHash(fn, out, h, field(x, i, f.Type), f.Type); err != nil {
				return nil, err
			}
		}
		return out, nil
	case ir.Array:
		if t.Len > maxUnrolledElements {
			return arrayLoop(fn, out, t.Len, func(i ir.Expr) ([]ir.Stmt, error) {
				return appendHash(fn, nil, h, indexBy(x, i, t.Elem), t.Elem)
			})
		}
		for i := int64(0); i < t.Len; i++ {
			var err error
			if out, err = appendHash(fn, out, h, index(x, i, t.Elem), t.Elem); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	name, err := leafHashFunc(t)
	if err != nil {
		return nil, err
	}
	// h = name(&x, h)
	ptr, err := pointerTo(t)
	if err != nil {
		return nil, err
	}
	call := &ir.Node{
		Op: ir.OCall, Pos: wrapperPos, Type: uintptrType(),
		X: &ir.Node{Op: ir.OGlobal, Pos: wrapperPos, Type: hashSigType(), Obj: runtimeFunc(name)},
		Args: []ir.Expr{
			{Op: ir.OAddr, Pos: wrapperPos, Type: ptr, X: x},
			{Op: ir.OLocal, Pos: wrapperPos, Type: uintptrType(), Obj: h},
		},
	}
	return append(out, &ir.Node{
		Op: ir.OAssign, Pos: wrapperPos, Type: voidType(),
		X: &ir.Node{Op: ir.OLocal, Pos: wrapperPos, Type: uintptrType(), Obj: h},
		Y: call,
	}), nil
}

// leafHashFunc names the runtime function that hashes one value of a kind that
// is not walked further.
//
// The choice is the same one rtype's algFuncs makes for a type that has a
// runtime function of its own, and the memory widths are the same table.
// A leaf reached from a walk is one scalar, so its size is always one of the
// widths the runtime declares and memhash_varlen never applies.
func leafHashFunc(t *ir.Type) (string, error) {
	switch t.Kind {
	case ir.Float32:
		return "runtime.f32hash", nil
	case ir.Float64:
		return "runtime.f64hash", nil
	case ir.Complex64:
		return "runtime.c64hash", nil
	case ir.Complex128:
		return "runtime.c128hash", nil
	case ir.String:
		return "runtime.strhash", nil
	case ir.Interface:
		// Which layout the first word has decides which function reads it.
		// Calling the other one reads a descriptor at the wrong offset.
		if t.EmptyIface {
			return "runtime.nilinterhash", nil
		}
		return "runtime.interhash", nil
	case ir.Slice, ir.Map, ir.FuncKind:
		return "", fmt.Errorf("%s cannot be a map key", t)
	}
	switch t.Size {
	case 0:
		return "runtime.memhash0", nil
	case 1:
		return "runtime.memhash8", nil
	case 2:
		return "runtime.memhash16", nil
	case 4:
		return "runtime.memhash32", nil
	case 8:
		return "runtime.memhash64", nil
	case 16:
		return "runtime.memhash128", nil
	}
	return "", fmt.Errorf("%s is %d bytes and the runtime has no hash of that width", t, t.Size)
}

// runtimeFunc returns the object that names a runtime function.
//
// The name is checked against rtsym, which specs/031-runtime-lowering.md makes
// the only place a runtime symbol may be spelled. A name that is not there
// would link against nothing and the call would jump wherever the linker left
// that address, so it is a build failure here rather than a run-time one.
func runtimeFunc(name string) *ir.Object {
	if rtsym.Lookup(name) == nil {
		panic("ssagen: " + name + " is not in rtsym")
	}
	return &ir.Object{Name: name, Type: hashSigType(), Class: ir.ClassFunc, Pos: wrapperPos}
}

// hashSigType is the type of every hash function the runtime declares:
// func(unsafe.Pointer, uintptr) uintptr.
//
// Below the IR a function value is one pointer-sized word whatever it is a
// function of, so the fields here are read only by a descriptor writer. They
// are filled in anyway, because a signature that claimed func() would be a
// FuncType descriptor that describes the wrong function.
var hashSigType = func() func() *ir.Type {
	t := &ir.Type{
		Kind:    ir.FuncKind,
		Params:  []*ir.Type{unsafePtrType(), uintptrType()},
		Results: []*ir.Type{uintptrType()},
	}
	if err := ir.Layout(t); err != nil {
		panic("ssagen: the hash signature does not lay out: " + err.Error())
	}
	return func() *ir.Type { return t }
}()

var (
	uintptrType   = mustLayout(&ir.Type{Kind: ir.Uintptr, Name: "uintptr", Basic: "uintptr"})
	unsafePtrType = mustLayout(&ir.Type{Kind: ir.UnsafePtr, Name: "unsafe.Pointer", PkgPath: "unsafe"})
)
