// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// A descriptor that names a nested slice, which nothing defined.
//
// gc writes the descriptors of the basic types, any, error, and a slice of one
// of those, and every other package refers to the runtime's copy. nanogo
// applied that rule recursively, so it decided the runtime owns [][]string
// because it reduces to []string and then to string. The runtime owns no such
// thing, so the descriptor of [10][]string named type:[][]string and the link
// failed.
//
// It has to be a program that links, because that is the only place the fault
// appears. The compile succeeds, the object is written, and every byte in it
// is correct apart from a reference to a symbol nobody emits. A rule that
// wrongly decides somebody else defines a symbol cannot be caught by running
// the program: it is caught by the linker or not at all.
const nestedSliceProgram = `package main

import "fmt"

// The array's descriptor names its element's descriptor, and the element is a
// slice of a slice.
var table [10][]string

// A map and a channel of one, so the other kinds that name an element
// descriptor are covered by the same program.
var index map[string][][]int

type deep struct {
	rows [][]byte
	seen [][]string
}

func main() {
	table[0] = []string{"a", "b"}
	table[9] = []string{"z"}

	index = map[string][][]int{"k": {{1, 2}, {3}}}

	d := deep{rows: [][]byte{{1}}, seen: [][]string{{"s"}}}

	// Into an interface, which is what forces each descriptor to be written.
	//
	// Only the composites are named here, never a nested slice on its own. A
	// type converted to an interface is a root of the descriptor closure, and
	// a root is emitted whether or not the runtime is thought to own it, so
	// naming [][]string directly would emit it for the wrong reason and the
	// test would pass with the fault in place. It has to be reached only as
	// the element of something else.
	for _, v := range []any{table, index, d} {
		fmt.Printf("%T ", v)
	}
	fmt.Println()
	fmt.Println(len(table[0]), len(index["k"]), len(d.rows), len(d.seen))
}
`

// TestGcAndNanogoAgreeOnANestedSliceDescriptor is the evidence that a
// descriptor naming a nested slice reaches the linker with a definition.
func TestGcAndNanogoAgreeOnANestedSliceDescriptor(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/nestedslice\n\ngo 1.27\n",
		"main.go": nestedSliceProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "nestedslice", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "nestedslice"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
