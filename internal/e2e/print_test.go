// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// print and println of every shape the two builtins accept.
//
// specs/020-ir.md's row names one runtime symbol per operand type, and the
// choice is invisible from inside the compiler: a wrong symbol reads the
// operand's words at the wrong offsets and still prints something. gc is
// therefore the oracle, and the assertion is the bytes both programs write.

// printShapesProgram prints one operand of every type printSym has a symbol
// for, through both builtins.
//
// The slice and the interface rows are the ones the lowering table refused
// until runtime.printslice, runtime.printeface and runtime.printiface were
// named. Each is checked in three states, because the three read different
// words: a nil operand, an empty non-nil one and a full one.
//
// The nil slice and the nil interface matter most. A nil slice prints its
// length, its capacity and a null data pointer, and a compiler that passed the
// header by address rather than by value would print the address of the
// header and agree with nothing.
const printShapesProgram = `package main

type point struct{ X, Y int }

func main() {
	// The width classes, so that the rows this change did not touch are
	// still measured against gc in the same program.
	println(true, false)
	println(int8(-8), int16(-16), int32(-32), int64(-64))
	println(uint8(8), uint16(16), uint32(32), uint64(64))
	println(uintptr(4096))
	println(float32(1.5), float64(-2.25))
	println("string")

	// Slices. The element type does not reach the symbol, so a slice of
	// bytes, of ints and of a struct all go to runtime.printslice.
	var nilslice []int
	println(nilslice)
	println([]int{})
	println([]int{1, 2, 3})
	println(make([]byte, 2, 8))
	println([]point{{1, 2}})
	print(nilslice)
	print("\n")

	// Interfaces with no methods. The first word is a type descriptor and
	// the second is the data, and a nil interface has neither.
	var nilface any
	println(nilface)
	println(any(42))
	println(any("boxed"))
	println(any(&point{3, 4}))
	print(nilface)
	print("\n")

	// print writes no separator and no newline, so the two operands run
	// together and the next line's output follows on the same line.
	print(1, 2)
	print("\n")
	println(1, "two", 3.0, []int{4}, any(5))
}
`

// TestPrintOfEveryShapeMatchesGc builds the program with nanogo and with the
// installed compiler and compares what the two write.
//
// The comparison normalises hexadecimal addresses. A slice prints its data
// pointer and an interface prints both of its words, and neither is the same
// number in two separately linked binaries. What is left after the
// normalisation is every decimal number, every length and capacity, every
// separator and every newline, which is the whole of what the lowering row
// decides.
func TestPrintOfEveryShapeMatchesGc(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/printshapes\n\ngo 1.27\n",
		"main.go": printShapesProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "shapes", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := maskAddresses(runProgram(t, filepath.Join(h.mod, "shapes")))
	want := maskAddresses(gcOutput(t, h))
	if got != want {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
	// The shapes whose whole output is deterministic, spelled out, so that
	// two compilers agreeing on the wrong answer is still a failure. A nil
	// slice is three zeroes and a nil interface is two.
	for _, line := range []string{"[0/0]0x0", "(0x0,0x0)", "[3/3]0xADDR", "[2/8]0xADDR", "1 two 3 [1/1]"} {
		if !strings.Contains(got, line) {
			t.Errorf("the output has no %q:\n%s", line, got)
		}
	}
}

// hexNumber is an address the runtime's print functions write.
var hexNumber = regexp.MustCompile(`0x[0-9a-f]+`)

// maskAddresses replaces every address with a fixed word, leaving 0x0 alone.
//
// 0x0 survives because it is the one address that is the same in both
// binaries and the one that carries meaning: it is what a nil slice's data
// pointer and both words of a nil interface print as.
func maskAddresses(b []byte) string {
	return hexNumber.ReplaceAllStringFunc(string(b), func(s string) string {
		if s == "0x0" {
			return s
		}
		return "0xADDR"
	})
}
