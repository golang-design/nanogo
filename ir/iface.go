// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

// An IfaceData says how a value of a concrete type becomes the data word of an
// interface value.
//
// An interface value is two words and the second is always a pointer, so a
// value that is not already one pointer has to be copied somewhere the second
// word can point at. Which copy the runtime makes is a function of the source
// type alone, and gc's dataWordFuncName is the same function.
//
// The answer is needed by two passes and neither can act on it alone.
// specs/025-lowering-and-rules.md's lowering pass owns the frame, so it is the
// only pass that can give a value a home to be addressed through.
// specs/021-ssa-construction.md's construction owns OpIMake, so it is the only
// pass that can build the interface value. One function here is what keeps the
// two from answering the question differently, which would be a value spilled
// and not boxed, or boxed and not spilled.
type IfaceData uint8

const (
	// IfaceDataInvalid is the answer for a type with no answer, which is a
	// type this file was asked about before it was laid out.
	IfaceDataInvalid IfaceData = iota

	// IfaceDataZero is a value of a zero-sized type. It has no bits to carry
	// and the word is still a pointer the collector scans, so the word is the
	// address of runtime.zerobase, which is what mallocgc returns for a
	// request of zero bytes.
	IfaceDataZero

	// IfaceDataDirect is a value that is already one pointer. The value is the
	// data word and nothing is copied.
	//
	// cmd/compile's types.IsDirectIface: one machine word wide, and that word
	// holds a pointer. It is not the same question as "pointer shaped". A
	// uintptr is one word and holds no pointer, so an interface holding one
	// carries the integer's address and not the integer, and a one-field
	// struct holding a pointer is not pointer shaped and is its own data word.
	IfaceDataDirect

	// IfaceDataString is a string, which runtime.convTstring copies.
	IfaceDataString

	// IfaceDataSlice is a slice, which runtime.convTslice copies. The element
	// type does not reach the helper: it takes []byte and copies the header.
	IfaceDataSlice

	// IfaceDataWord64, IfaceDataWord32 and IfaceDataWord16 are one scalar of
	// exactly that width, which runtime.convT64, convT32 and convT16 copy by
	// value. A struct or an array of the same width is not one of these: it is
	// several values by the time decomposition has run, and the helper reads
	// one.
	IfaceDataWord64
	IfaceDataWord32
	IfaceDataWord16

	// IfaceDataAddress is everything else: a struct, an array, a scalar of a
	// width no helper takes, and any type holding a pointer that is not
	// exactly one. runtime.convT and runtime.convTnoptr copy from a pointer
	// the caller supplies, so the value needs a home to be addressed through
	// and the lowering pass is what gives it one.
	IfaceDataAddress
)

// IfaceDataWordOf returns how a value of t becomes an interface's data word.
func IfaceDataWordOf(t *Type) IfaceData {
	if t == nil {
		return IfaceDataInvalid
	}
	switch {
	case t.Size == 0:
		return IfaceDataZero
	case t.Size == PtrSize && t.PtrBytes() == PtrSize:
		return IfaceDataDirect
	case t.Kind == String:
		return IfaceDataString
	case t.Kind == Slice:
		return IfaceDataSlice
	}
	// A scalar and not a struct or an array of the same width, for the reason
	// the constants above give. A scalar of these widths holds no pointer, so
	// the by-value helpers are reached only by a type the collector has
	// nothing to find in.
	if t.Kind.IsInteger() || t.Kind.IsFloat() {
		switch {
		case t.Size == 8 && t.Align == 8:
			return IfaceDataWord64
		case t.Size == 4 && t.Align == 4:
			return IfaceDataWord32
		case t.Size == 2 && t.Align == 2:
			return IfaceDataWord16
		}
	}
	return IfaceDataAddress
}

// IfaceConvSym is the runtime helper that copies a value of t into the heap
// for the address case, and it is the one place the choice between the two is
// made.
//
// runtime.convT and runtime.convTnoptr have one signature and two bodies. The
// first allocates an object the collector scans and the second one it does
// not, so a type holding a pointer copied through convTnoptr would leave its
// pointer in an object the collector never looks in.
//
// The two directions fail differently and only one of them is quiet. A
// pointer-holding type through convTnoptr is caught by the runtime at the
// allocation, which throws "objects with pointers must be zeroed" out of
// mallocgc, so any program that reaches it dies at once. A pointer-free type
// through convT is correct and only wasteful: the object is scanned and there
// is nothing in it to find.
//
// What is quiet is the descriptor rather than the choice. convT reads the size
// and the pointer map out of the descriptor it is given, so a copy made with
// the wrong one is a heap object whose pointers the collector does not know
// about, and the pointee is freed while the interface still reaches it with no
// message anywhere. internal/e2e holds a finaliser on such a pointee under
// GODEBUG=gccheckmark=1,clobberfree=1, which is the harness
// specs/027-liveness-and-stackmaps.md already uses.
func IfaceConvSym(t *Type) string {
	if t != nil && t.PtrBytes() > 0 {
		return "runtime.convT"
	}
	return "runtime.convTnoptr"
}
