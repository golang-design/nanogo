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

// hashFuncs names the runtime hash function for each algorithm with one.
//
// One per entry of algFuncs and chosen by the same algKind, which is not a
// convenience. Two values that compare equal must hash alike or a map loses
// keys it holds, so gc derives both from one AlgKind and this reproduces that
// rather than making a second decision. A test asserts the two tables cover
// the same set.
var hashFuncs = map[algKind]string{
	algString:     "runtime.strhash",
	algIface:      "runtime.interhash",
	algNilIface:   "runtime.nilinterhash",
	algFloat32:    "runtime.f32hash",
	algFloat64:    "runtime.f64hash",
	algComplex64:  "runtime.c64hash",
	algComplex128: "runtime.c128hash",
}

// memHashFuncs names the fixed-width memory hashes, by byte count.
//
// The same widths memEqualFuncs covers, for the same reason: a size with no
// entry is hashed by memhash_varlen, which reads the size out of the closure.
var memHashFuncs = map[int64]string{
	0:  "runtime.memhash0",
	1:  "runtime.memhash8",
	2:  "runtime.memhash16",
	4:  "runtime.memhash32",
	8:  "runtime.memhash64",
	16: "runtime.memhash128",
}

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
	if ir.NoAlgType(t) {
		// A type the compiler synthesised, or one holding a part of one. gc
		// gives the mark the highest priority of any algorithm and ANOALG
		// implies ANOEQ, so the descriptor's Equal is nil and nothing compares
		// a value of this type. The map slot group is what this is for: a
		// group holding a string would otherwise be given the generated
		// field-wise comparison, and gc generates none.
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
	return algFunc(t, "equality", algFuncs, memEqualFuncs, "runtime.memequal_varlen")
}

// hashFunc returns the runtime function that hashes a value of t, and reports
// whether the closure that names it carries the size.
//
// An empty name means t cannot be a map key, which is the same set of types
// that cannot be compared: the language forbids a slice, a map or a function
// as a key exactly because it forbids comparing one.
func hashFunc(t *ir.Type) (name string, varlen bool, err error) {
	return algFunc(t, "hash", hashFuncs, memHashFuncs, "runtime.memhash_varlen")
}

// algFunc is the body both of the two above share.
//
// One function and not two, because the choice is one decision made twice. Two
// values that compare equal must hash alike or a map loses keys it holds, so a
// hash chosen by a rule of its own is a bug that shows up as a lookup miss
// rather than as a wrong answer.
func algFunc(t *ir.Type, what string, byAlg map[algKind]string, byWidth map[int64]string, varlenFn string) (string, bool, error) {
	switch a := alg(t); a {
	case algNone:
		return "", false, nil
	case algSpecial:
		return "", false, fmt.Errorf("rtype: %s needs a generated %s function, which specs/032 has no writer for", t, what)
	case algMem:
		if fn, ok := byWidth[t.Size]; ok {
			return fn, false, nil
		}
		return varlenFn, true, nil
	default:
		fn, ok := byAlg[a]
		if !ok {
			return "", false, fmt.Errorf("rtype: no %s function for %s", what, t)
		}
		return fn, false, nil
	}
}
