// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// A zero-size argument decides where the arguments after it sit, so a caller
// and a callee that place it differently disagree about every one of them.
//
// This is the shape of specs/030-abi.md's rule 2, which is step 2 of gc's
// assignment algorithm: a zero-size value takes a stack slot and no register.
// It occupies no space. What it does is align the argument area to its own
// alignment before the next value is placed, and abi-internal.md states that
// the rule exists to keep the internal ABI equivalent to ABI0, where the same
// padding appears.
//
// nanogo placed such a value in registers, by accident rather than by
// decision: a zero-size type has no parts, so no part failed to fit and the
// register path took it. The library is compiled by gc and the program by
// nanogo, which is the only configuration where the difference shows: a build
// where one compiler does both is wrong in the same way at both ends and
// agrees with itself.
const zeroSizeLibrary = `package lib

type Empty struct{}

// Neither [3]int8 is trivial, so both take the argument area, and the
// zero-size value between them aligns the area to 8 before the second is
// placed. gc puts c at FP+8. Placing the zero-size value in registers puts it
// at FP+3.
func Between(a [3]int8, b [0]int64, c [3]int8) int32 {
	return int32(a[0])*100 + int32(c[0])
}

// The same with a zero-size struct, which is the form a program is far more
// likely to hold than an array of length zero.
func Struct(a [3]int8, b Empty, c [3]int8) int32 {
	return int32(a[0])*100 + int32(c[0])
}

// A zero-size value first, so it aligns the whole area rather than one
// argument in the middle of it.
func First(a [0]int64, b [3]int8, c [3]int8) int32 {
	return int32(b[0])*100 + int32(c[0])
}

// A zero-size value with nothing after it. It still pads the area, so a caller
// that left the padding out writes a frame the callee does not read.
func Last(a [3]int8, b [0]int64) int32 { return int32(a[0]) }

// Every value in registers, which is the case that was always right and must
// stay right.
func Registers(a int8, b Empty, c int8) int32 { return int32(a)*100 + int32(c) }
`

const zeroSizeProgram = `package main

import (
	"fmt"

	"nanogo.example/zerosize/lib"
)

func main() {
	a := [3]int8{7, 1, 2}
	c := [3]int8{9, 3, 4}
	fmt.Println(lib.Between(a, [0]int64{}, c))
	fmt.Println(lib.Struct(a, lib.Empty{}, c))
	fmt.Println(lib.First([0]int64{}, a, c))
	fmt.Println(lib.Last(a, [0]int64{}))
	fmt.Println(lib.Registers(7, lib.Empty{}, 9))
}
`

func zeroSizeModule() map[string]string {
	return map[string]string{
		"go.mod":     "module nanogo.example/zerosize\n\ngo 1.27\n",
		"lib/lib.go": zeroSizeLibrary,
		"main.go":    zeroSizeProgram,
	}
}

// TestGcAndNanogoAgreeAcrossAZeroSizeArgument is the evidence that nanogo
// places a zero-size argument where gc places it.
//
// gc compiles the library and nanogo compiles the program, so the two have to
// agree about the frame. Before the fix this printed 700 where gc's build
// printed 709: the callee read c at FP+8 and the caller wrote it at FP+3, and
// nothing refused, crashed or warned.
func TestGcAndNanogoAgreeAcrossAZeroSizeArgument(t *testing.T) {
	h := setup(t, zeroSizeModule(), []string{"main"})

	if out, err := h.build(t, "-o", "zerosize", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "zerosize"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
