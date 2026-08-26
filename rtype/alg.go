// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"fmt"

	"golang.design/x/nanogo/ir"
)

// The equality algorithm of a type.
//
// gc calls this the type's AlgKind and derives two things from it: the
// descriptor's TFlagRegularMemory bit, and which runtime function the Equal
// field points at. Both are reproduced here because both end up in bytes the
// runtime reads.
//
// The set is gc's: the basic kinds, the pointer-shaped kinds, an array, whose
// algorithm is its element's, and a struct, whose algorithm is in struct.go
// because it needs the field offsets to find the padding.
type algKind uint8

const (
	// algNone is a type that cannot be compared at all. Its Equal field is
	// nil, which is what makes the type unusable as a map key.
	algNone algKind = iota

	// algSpecial is a type that compares as something other than one region of
	// memory and has no runtime function of its own. specs/032 is right that
	// this is the case where the compiler must emit a function.
	algSpecial

	// algMem is a type that compares as one region of t.Size bytes.
	algMem

	algString
	algIface
	algNilIface
	algFloat32
	algFloat64
	algComplex64
	algComplex128
)

// algFuncs names the runtime function for each algorithm with one.
//
// algMem is absent because its function depends on the size, and algNone and
// algSpecial are absent because neither has one.
var algFuncs = map[algKind]string{
	algString:     "runtime.strequal",
	algIface:      "runtime.interequal",
	algNilIface:   "runtime.nilinterequal",
	algFloat32:    "runtime.f32equal",
	algFloat64:    "runtime.f64equal",
	algComplex64:  "runtime.c64equal",
	algComplex128: "runtime.c128equal",
}

// memEqualFuncs names the fixed-width memory comparisons, by byte count.
//
// A size with no entry is compared by memequal_varlen instead, which reads the
// size out of the closure. Three, five, six and seven are absent because the
// runtime has no such function and not because they cannot occur.
var memEqualFuncs = map[int64]string{
	0:  "runtime.memequal0",
	1:  "runtime.memequal8",
	2:  "runtime.memequal16",
	4:  "runtime.memequal32",
	8:  "runtime.memequal64",
	16: "runtime.memequal128",
}

// alg returns the equality algorithm of t.
func alg(t *ir.Type) algKind {
	if t == nil {
		return algNone
	}
	switch t.Kind {
	case ir.Float32:
		return algFloat32
	case ir.Float64:
		return algFloat64
	case ir.Complex64:
		return algComplex64
	case ir.Complex128:
		return algComplex128
	case ir.String:
		return algString
	case ir.Interface:
		// Which of the two interface layouts the first word has decides which
		// function reads it. type.go's EmptyIface field exists for exactly
		// this choice, and calling the other one reads a function pointer at
		// the wrong offset.
		if t.EmptyIface {
			return algNilIface
		}
		return algIface
	case ir.Slice, ir.Map, ir.FuncKind:
		// The language forbids comparing these with ==.
		return algNone
	case ir.Array:
		return arrayAlg(t)
	case ir.Struct, ir.Tuple:
		return structAlg(t)
	case ir.Void, ir.Invalid:
		return algNone
	}
	// Every remaining kind is an integer, a bool, a pointer or a channel, and
	// each compares as its bytes.
	return algMem
}

// arrayAlg returns the algorithm of an array, which is its element's.
//
// The three special counts are gc's. An empty array compares as nothing, which
// is a memory comparison of zero bytes. A one-element array compares exactly
// as its element. Anything longer, with an element that is not plain memory,
// needs a loop, and a loop is a generated function.
func arrayAlg(t *ir.Type) algKind {
	a := alg(t.Elem)
	switch a {
	case algMem, algNone, algSpecial:
		return a
	}
	switch t.Len {
	case 0:
		return algMem
	case 1:
		return a
	}
	return algSpecial
}

// equalFunc returns the runtime function that compares two values of t, and
// reports whether the closure that names it carries the size.
//
// An empty name means t is not comparable.
func equalFunc(t *ir.Type) (name string, varlen bool, err error) {
	switch a := alg(t); a {
	case algNone:
		return "", false, nil
	case algSpecial:
		return "", false, fmt.Errorf("rtype: %s needs a generated equality function, which specs/032 has no writer for", t)
	case algMem:
		if fn, ok := memEqualFuncs[t.Size]; ok {
			return fn, false, nil
		}
		return "runtime.memequal_varlen", true, nil
	default:
		fn, ok := algFuncs[a]
		if !ok {
			return "", false, fmt.Errorf("rtype: no equality function for %s", t)
		}
		return fn, false, nil
	}
}
