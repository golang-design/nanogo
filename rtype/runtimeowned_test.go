// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype_test

import (
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtype"
)

// TestRuntimeOwnedStripsOneSliceAndNotTwo is gc's rule, which decides who
// defines a descriptor.
//
// gc writes the basic types, any, error, and a slice of one of those, and
// every other package refers to the runtime's copy. writtenByWriteBasicTypes
// strips a slice once and then asks whether what is left is basic, with an if
// and not a loop, under the comment "Now we have left the basic types plus any
// and error, plus slices of them. Strip the slice."
//
// nanogo recursed instead, so it decided the runtime owns [][]string, because
// that reduces to []string and then to string. It does not. The descriptor of
// [10][]string then named a symbol nothing defined and the link failed with
// "relocation target type:[][]string not defined".
//
// This is the direction of error that running a program cannot catch. A rule
// that wrongly decides somebody else defines a symbol produces no wrong
// answer and no crash: it produces a link failure, and only because the linker
// checks. The same rule in the other direction would emit a second definition
// of a symbol the runtime already has.
func TestRuntimeOwnedStripsOneSliceAndNotTwo(t *testing.T) {
	str := lay(t, &ir.Type{Kind: ir.String, Name: "string"})
	sliceOf := func(e *ir.Type) *ir.Type {
		return lay(t, &ir.Type{Kind: ir.Slice, Elem: e})
	}

	for _, tc := range []struct {
		name string
		typ  *ir.Type
		want bool
	}{
		// The runtime writes the basic types themselves.
		{"string", str, true},
		// And a slice of one of them, which is the single strip.
		{"[]string", sliceOf(str), true},
		// And nothing deeper. This is the case that failed the link.
		{"[][]string", sliceOf(sliceOf(str)), false},
		{"[][][]string", sliceOf(sliceOf(sliceOf(str))), false},
		// A slice of a composite is not basic after one strip either.
		{"[]*string", sliceOf(lay(t, &ir.Type{Kind: ir.Ptr, Elem: str})), false},
		// A pointer is not stripped at all: gc's comment says any pointer has
		// been taken off before the question is asked.
		{"*string", lay(t, &ir.Type{Kind: ir.Ptr, Elem: str}), false},
		// A defined type is the declaring package's, whatever it is made of.
		{"a defined slice", lay(t, &ir.Type{
			Kind: ir.Slice, Elem: str, Name: "p.Names", PkgPath: "p",
		}), false},
	} {
		if got := rtype.RuntimeOwned(tc.typ); got != tc.want {
			t.Errorf("RuntimeOwned(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
