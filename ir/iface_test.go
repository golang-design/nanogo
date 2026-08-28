// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import "testing"

// TestIfaceDataWordOf pins the classification both passes read.
//
// It is one function because two passes act on it and neither can act alone:
// the lowering pass gives the address case a home and construction builds the
// interface value. A disagreement between them is a value spilled and not
// boxed, or boxed and not spilled, and nothing below would report either.
func TestIfaceDataWordOf(t *testing.T) {
	var (
		tInt     = layOut(t, &Type{Kind: Int64, Name: "int"})
		tUintptr = layOut(t, &Type{Kind: Uintptr, Name: "uintptr"})
		tUint8   = layOut(t, &Type{Kind: Uint8, Name: "uint8"})
		tInt16   = layOut(t, &Type{Kind: Int16, Name: "int16"})
		tInt32   = layOut(t, &Type{Kind: Int32, Name: "int32"})
		tFloat32 = layOut(t, &Type{Kind: Float32, Name: "float32"})
		tFloat64 = layOut(t, &Type{Kind: Float64, Name: "float64"})
		tString  = layOut(t, &Type{Kind: String, Name: "string"})
		tPtr     = layOut(t, &Type{Kind: Ptr, Elem: tInt})
		tSlice   = layOut(t, &Type{Kind: Slice, Elem: tInt})
		tMap     = layOut(t, &Type{Kind: Map, Key: tInt, Elem: tInt})
		tChan    = layOut(t, &Type{Kind: Chan, Elem: tInt, ChanDir: SendRecv})
		tEmpty   = layOut(t, &Type{Kind: Struct, Name: "empty"})
		tBox     = layOut(t, &Type{Kind: Struct, Name: "box",
			Fields: []Field{{Name: "p", Type: tPtr}}})
		tPair = layOut(t, &Type{Kind: Struct, Name: "pair",
			Fields: []Field{{Name: "a", Type: tInt}, {Name: "b", Type: tInt}}})
		tWord = layOut(t, &Type{Kind: Struct, Name: "word",
			Fields: []Field{{Name: "a", Type: tInt}}})
		tArr0 = layOut(t, &Type{Kind: Array, Len: 0, Elem: tInt})
		tArr4 = layOut(t, &Type{Kind: Array, Len: 4, Elem: tInt})
	)

	for _, tc := range []struct {
		what string
		typ  *Type
		want IfaceData
	}{
		{"a type with no bits", tEmpty, IfaceDataZero},
		{"an array with no elements", tArr0, IfaceDataZero},

		{"a pointer", tPtr, IfaceDataDirect},
		{"a map", tMap, IfaceDataDirect},
		{"a channel", tChan, IfaceDataDirect},
		{"a one-field struct holding a pointer, which is not pointer shaped", tBox, IfaceDataDirect},

		{"a string", tString, IfaceDataString},
		{"a slice", tSlice, IfaceDataSlice},

		{"an int", tInt, IfaceDataWord64},
		{"a float64", tFloat64, IfaceDataWord64},
		{"an int32", tInt32, IfaceDataWord32},
		{"a float32", tFloat32, IfaceDataWord32},
		{"an int16", tInt16, IfaceDataWord16},

		// A uintptr is one word and holds no pointer, so the interface carries
		// the address of a copy and not the integer. gc reads the same two
		// facts in types.IsDirectIface.
		{"a uintptr", tUintptr, IfaceDataWord64},

		// A struct of one word is not one machine value by the time
		// decomposition has run, and convT64 reads one.
		{"a one-word struct holding no pointer", tWord, IfaceDataAddress},
		{"a two-word struct", tPair, IfaceDataAddress},
		{"an array of four", tArr4, IfaceDataAddress},
		// No helper takes a byte by value.
		{"a uint8", tUint8, IfaceDataAddress},

		{"no type at all", nil, IfaceDataInvalid},
	} {
		if got := IfaceDataWordOf(tc.typ); got != tc.want {
			t.Errorf("%s is %v, want %v", tc.what, got, tc.want)
		}
	}
}

// TestIfaceConvSymFollowsThePointerMap pins the choice of helper.
//
// runtime.convT allocates an object the collector scans and runtime.convTnoptr
// one it does not, and the source type's pointer map is what decides. The
// runtime rejects the dangerous direction at the allocation, so this is a
// table rather than the only evidence: what it buys is that every row is
// listed in one place and a new shape is added here rather than discovered by
// a program that dies.
func TestIfaceConvSymFollowsThePointerMap(t *testing.T) {
	tInt := layOut(t, &Type{Kind: Int64, Name: "int"})
	tPtr := layOut(t, &Type{Kind: Ptr, Elem: tInt})
	tString := layOut(t, &Type{Kind: String, Name: "string"})

	for _, tc := range []struct {
		what string
		typ  *Type
		want string
	}{
		{"a struct of two integers", layOut(t, &Type{Kind: Struct, Name: "pair",
			Fields: []Field{{Name: "a", Type: tInt}, {Name: "b", Type: tInt}}}),
			"runtime.convTnoptr"},
		{"an array of integers", layOut(t, &Type{Kind: Array, Len: 4, Elem: tInt}),
			"runtime.convTnoptr"},
		{"a struct holding a pointer", layOut(t, &Type{Kind: Struct, Name: "held",
			Fields: []Field{{Name: "a", Type: tInt}, {Name: "p", Type: tPtr}}}),
			"runtime.convT"},
		{"a struct holding a string, whose first word is a pointer",
			layOut(t, &Type{Kind: Struct, Name: "named",
				Fields: []Field{{Name: "s", Type: tString}, {Name: "n", Type: tInt}}}),
			"runtime.convT"},
		{"an array of pointers", layOut(t, &Type{Kind: Array, Len: 2, Elem: tPtr}),
			"runtime.convT"},
	} {
		if got := IfaceConvSym(tc.typ); got != tc.want {
			t.Errorf("%s is copied by %s, want %s", tc.what, got, tc.want)
		}
	}
}
