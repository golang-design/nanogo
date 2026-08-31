// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// The program that branches on a comparison made earlier in the same block.
//
// Every function here computes a boolean from a comparison, does something
// that writes the arm64 condition codes, and then branches on the boolean. The
// something differs per function, because each is a different way for an
// instruction to land between the compare and the branch:
//
//   - allocated builds a function literal, whose capture cell is a call to
//     runtime.newobject that the lowering inserts rather than the source;
//   - called makes a call the source wrote;
//   - compared makes a second comparison, which writes the condition codes
//     without any call at all;
//   - loop branches on a value compared before the loop was entered.
//
// The answers are printed and gc's build of the same source is the oracle. A
// branch that reads condition codes something else wrote takes whichever way
// the last write left, so the failure is a body that runs when its condition
// is false.
const flagsBranchProgram = `package main

import "fmt"

var sink func() bool

func allocated(m, t int) string {
	constArg := m == 4
	sink = func() bool { return t == 9 }
	if constArg && t == 1 {
		return "const"
	}
	return "other"
}

func length(s string) int { return len(s) }

func called(m, t int) string {
	constArg := m == 4
	n := length(fmt.Sprint(t))
	if constArg && n == 1 {
		return "const"
	}
	return "other"
}

func compared(m, t int) string {
	constArg := m == 4
	other := t > 100
	if constArg || other {
		return "either"
	}
	return "neither"
}

func loop(m, n int) string {
	constArg := m == 4
	total := 0
	for i := 0; i < n; i++ {
		total += i
	}
	if constArg {
		return fmt.Sprint("const", total)
	}
	return fmt.Sprint("other", total)
}

func main() {
	for _, m := range []int{0, 4, 5} {
		for _, t := range []int{1, 9} {
			fmt.Println(m, t, allocated(m, t), called(m, t), compared(m, t), loop(m, t))
		}
	}
}
`

// TestGcAndNanogoAgreeOnABranchAfterAComparison is the program that says a
// conditional branch reads the condition codes its own compare wrote.
//
// The arm64 lowering folds a conditional set that a block branches on into a
// branch that reads the flags directly, and it guarded the fold on the compare
// and the branch being in one block. The condition codes are not in the graph:
// nothing records which instructions write them and no pass keeps a flags
// value alive, so one block is not far enough apart to be safe. A call between
// the two writes the condition codes and the branch read what the call left.
//
// This is the second fault self-hosting stage 2 died of. types2's conversion
// has
//
//	constArg := x.mode() == constant_
//	constConvertibleTo := func(T Type, val *constant.Value) bool { ... }
//	switch {
//	case constArg && isConstType(T):
//
// and the cell the literal's captures need is allocated between the compare
// and the branch. The compiler nanogo built took the constant path for a
// variable, and dereferenced the nil constant.Value that path assumes is
// there.
func TestGcAndNanogoAgreeOnABranchAfterAComparison(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/flags\n\ngo 1.27\n",
		"main.go": flagsBranchProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "flags", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "flags"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
