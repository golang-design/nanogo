// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// An operand of an index or a selection that names no storage.
//
// specs/021-ssa-construction.md gives such an operand a frame temporary,
// because construction reads a header, an element and a field through an
// address and Go does not require either operand to name a place. The two
// spellings are here: hex[i] indexes a constant, and f().x selects a field of
// a call result.
//
// The test runs the program. A copy into a temporary is the kind of thing that
// compiles either way and gives a wrong answer one way, and two of the three
// assertions below are answers rather than exit codes.

// spillProgram indexes a constant and selects a field of a call result.
//
// hexOf is the shape the gap was found with, cmd/internal/objabi's
// PathToPrefix: a constant string indexed inside a loop, in a function with a
// parameter that outlives the loop. The parameter is what makes the placement
// of the temporary's clear observable. Clearing a type that holds a pointer is
// a call, Go's convention leaves no register standing across a call, and a
// clear emitted ahead of the arguments would let this function read back a
// parameter the clear had already overwritten.
//
// pair is the other spelling. It returns a struct by value, so "makePair().b"
// selects a field of a value that names no place, and the struct holds a
// string so that the selection reaches a field the collector scans.
const spillProgram = `package main

const hex = "0123456789abcdef"

type pair struct {
	a int
	b string
}

//go:noinline
func makePair(n int) pair { return pair{a: n, b: hex[n : n+2]} }

//go:noinline
func hexOf(shift int) string {
	out := ""
	for i := 0; i < 4; i++ {
		out += string(hex[i+shift])
	}
	return out + string(hex[shift])
}

func main() {
	println(hexOf(2))
	println(makePair(10).b)
	println(makePair(3).a)
}
`

// TestToolexecSpillsAnOperandThatNamesNoStorage compiles, links and runs the
// two spellings, and compares what the program printed against gc.
func TestToolexecSpillsAnOperandThatNamesNoStorage(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/spill\n\ngo 1.27\n",
		"main.go": spillProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "spill", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "spill"))
	if want := "23452\nab\n3\n"; string(got) != want {
		t.Errorf("the program printed %q, want %q", got, want)
	}
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed %q and gc's printed %q", got, want)
	}
}
