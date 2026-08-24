// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"

	"golang.design/x/nanogo/types2"
)

// Converter turns a type checker type into the IR's type.
//
// This is the type boundary of specs/002-architecture.md in one direction.
// Everything the compiler needs below the IR is a kind, a size, an alignment
// and a pointer map, and this is where the type checker's answer is reduced to
// those four things. Nothing below the IR sees a types2.Type.
//
// A Converter memoises. The type checker's graph is shared and it is cyclic
// through pointers, so a converter that recursed without a cache would not
// terminate on
//
//	type T struct{ next *T }
//
// The cache entry is installed before the recursion into the type's parts, not
// after, which is what breaks the cycle: the second visit to T finds the entry
// that the first visit installed and stops.
type Converter struct {
	// cache maps one type checker type to one IR type. It is never ranged
	// over. specs/053-determinism.md forbids a map on a path that produces
	// output, and the order of this one is the order of a type graph walk.
	cache map[types2.Type]*Type

	// tuples is the cache for result tuples, which are not types of values and
	// so are kept apart from cache.
	tuples map[*types2.Tuple]*Type
}

// NewConverter returns a Converter with an empty cache.
//
// One converter per package build. Sharing one across packages is safe and
// wastes nothing, because a type's layout does not depend on who asks.
func NewConverter() *Converter {
	return &Converter{
		cache:  make(map[types2.Type]*Type),
		tuples: make(map[*types2.Tuple]*Type),
	}
}

// Convert returns the IR type of t, with Size, Align, PtrBits and every field
// offset already computed.
//
// It reports an error for a type that has no run-time representation: a type
// parameter, a constraint union, and the tuple of a multi-value expression.
// A tuple has Tuple for that reason.
func (c *Converter) Convert(t types2.Type) (*Type, error) {
	out, err := c.convert(t)
	if err != nil {
		return nil, err
	}
	if err := Layout(out); err != nil {
		return nil, err
	}
	return out, nil
}

// Tuple returns the type of a multi-value expression.
//
// The result has kind Tuple, with one field per component, named r0, r1 and so
// on. Read that kind's documentation before using the offsets: its layout is a
// struct's and is deliberately not the result layout of specs/030-abi.md,
// because results are assigned to registers and the stack by the calling
// convention rather than by struct packing.
//
// It is a method of its own rather than a case of Convert so that a tuple
// cannot be reached by accident: nothing that asks for the type of a value
// should get one.
func (c *Converter) Tuple(t *types2.Tuple) (*Type, error) {
	// A nil tuple is the empty result list, which types2 spells as a nil
	// *Tuple, and it converts to a zero-size struct.
	if have, ok := c.tuples[t]; ok {
		return have, nil
	}
	out := &Type{Kind: Tuple}
	c.tuples[t] = out
	for i := 0; i < t.Len(); i++ {
		ft, err := c.convert(t.At(i).Type())
		if err != nil {
			delete(c.tuples, t)
			return nil, err
		}
		out.Fields = append(out.Fields, Field{Name: fmt.Sprintf("r%d", i), Type: ft})
	}
	if err := Layout(out); err != nil {
		delete(c.tuples, t)
		return nil, err
	}
	return out, nil
}

// basicKinds maps the type checker's basic kinds to the IR's.
//
// int, uint and uintptr are 64 bit because both targets of
// specs/000-decisions.md decision 9 are, which is the same assumption PtrSize
// records. A 32-bit target makes this table a property of the target.
var basicKinds = map[types2.BasicKind]Kind{
	types2.Bool:           Bool,
	types2.Int:            Int64,
	types2.Int8:           Int8,
	types2.Int16:          Int16,
	types2.Int32:          Int32,
	types2.Int64:          Int64,
	types2.Uint:           Uint64,
	types2.Uint8:          Uint8,
	types2.Uint16:         Uint16,
	types2.Uint32:         Uint32,
	types2.Uint64:         Uint64,
	types2.Uintptr:        Uintptr,
	types2.Float32:        Float32,
	types2.Float64:        Float64,
	types2.Complex64:      Complex64,
	types2.Complex128:     Complex128,
	types2.String:         String,
	types2.UnsafePointer:  UnsafePtr,
	types2.UntypedBool:    Bool,
	types2.UntypedInt:     Int64,
	types2.UntypedRune:    Int32,
	types2.UntypedFloat:   Float64,
	types2.UntypedComplex: Complex128,
	types2.UntypedString:  String,
}

func (c *Converter) convert(t types2.Type) (*Type, error) {
	if t == nil {
		return nil, fmt.Errorf("ir: convert of a nil type")
	}
	// An alias is a second name for one type, so it converts to the type it
	// names. Unalias first, and cache on the result, so that T and its alias
	// share one IR type rather than two equal ones.
	t = types2.Unalias(t)
	if have, ok := c.cache[t]; ok {
		return have, nil
	}

	out := new(Type)
	// Installed before the recursion below. A type graph is cyclic through
	// pointers and this is what stops the walk from following the cycle.
	c.cache[t] = out
	if err := c.fill(out, t); err != nil {
		delete(c.cache, t)
		return nil, err
	}
	return out, nil
}

// fill sets out from t. out is already in the cache, so a recursive reference
// back to t finds it.
func (c *Converter) fill(out *Type, t types2.Type) error {
	switch t := t.(type) {
	case *types2.Basic:
		k, ok := basicKinds[t.Kind()]
		if !ok {
			return fmt.Errorf("ir: no IR kind for basic type %s", t)
		}
		out.Kind = k
		// unsafe.Pointer is UnsafePtr and not Ptr. The collector treats the two
		// differently: an unsafe.Pointer may point into the middle of an
		// object, so it is not a valid base for an interior pointer check.
		if out.Name == "" {
			// A defined type keeps the name it was given. This case is also
			// reached through Named, whose name is the one worth printing.
			out.Name = t.Name()
			if t.Kind() == types2.UnsafePointer {
				// The checker calls it Pointer, because that is its name in
				// its package. A diagnostic wants the name a reader wrote.
				out.Name = "unsafe.Pointer"
			}
		}
		return nil

	case *types2.Named:
		// The name is carried for diagnostics only, and the shape comes from
		// the underlying type. specs/020-ir.md states the rule: below the IR a
		// type is not a name and not a method set.
		out.Name = namedString(t)
		return c.fill(out, t.Underlying())

	case *types2.Pointer:
		out.Kind = Ptr
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Elem = elem
		return nil

	case *types2.Slice:
		out.Kind = Slice
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Elem = elem
		return nil

	case *types2.Array:
		out.Kind = Array
		out.Len = t.Len()
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Elem = elem
		return nil

	case *types2.Map:
		out.Kind = Map
		key, err := c.convert(t.Key())
		if err != nil {
			return err
		}
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Key, out.Elem = key, elem
		return nil

	case *types2.Chan:
		out.Kind = Chan
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Elem = elem
		return nil

	case *types2.Struct:
		out.Kind = Struct
		// In declaration order, which specs/030-abi.md requires: no field
		// reordering, because every use of unsafe.Offsetof and every assembly
		// file assumes the written order.
		for i := 0; i < t.NumFields(); i++ {
			f := t.Field(i)
			ft, err := c.convert(f.Type())
			if err != nil {
				return err
			}
			out.Fields = append(out.Fields, Field{Name: f.Name(), Type: ft})
		}
		return nil

	case *types2.Signature:
		// FuncKind, not Func. A function value is one word, a pointer to a
		// closure object; the signature is not part of the machine type.
		out.Kind = FuncKind
		return nil

	case *types2.Interface:
		out.Kind = Interface
		// Which of the two interface layouts this is. An empty interface holds
		// a *_type in its first word and a non-empty one holds an *itab, so
		// equality calls a different runtime function for each and calling the
		// wrong one reads a function pointer at the wrong offset. See the field
		// comment in type.go.
		out.EmptyIface = t.NumMethods() == 0
		return nil

	case *types2.TypeParam:
		// specs/013-generics.md instantiates before the IR is built, so a type
		// parameter here means an uninstantiated body reached the builder.
		return fmt.Errorf("ir: type parameter %s has no run-time representation", t)

	case *types2.Tuple:
		return fmt.Errorf("ir: a tuple is not the type of a value; use Converter.Tuple")

	case *types2.Union:
		return fmt.Errorf("ir: a constraint union has no run-time representation")
	}
	return fmt.Errorf("ir: no IR type for %T", t)
}

// namedString names a defined type for a diagnostic.
//
// The import path is included because two packages may define one name, and a
// message that says "T" when there are two Ts costs the reader the time this
// name is here to save.
func namedString(t *types2.Named) string {
	obj := t.Obj()
	if obj == nil {
		return ""
	}
	if pkg := obj.Pkg(); pkg != nil {
		return pkg.Path() + "." + obj.Name()
	}
	return obj.Name()
}
