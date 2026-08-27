// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// discardedResultProgram reads one result of a two-result call and drops the
// other.
//
// The dropped result has no user by the time specs/025's lowering runs, and
// the pass used to remove it. An OpSelectN names one ABI location of the call,
// so removing it left the call with a result nothing named and ssagen stopped
// with "result 0 of the call is never named". Nothing about the shape of the
// callee matters, which is why the program is this small.
const discardedResultProgram = `package main

//go:noinline
func two() (int, int) { return 1, 7 }

//go:noinline
func first() (int, int) { return 7, 2 }

func main() {
	_, b := two()
	a, _ := first()
	if b != 7 || a != 7 {
		crash()
	}
}

//go:noinline
func crash() {
	d := 0
	d = d / d
}
`

// TestToolexecKeepsTheResultNobodyReads builds a call whose result is
// discarded and runs it.
func TestToolexecKeepsTheResultNobodyReads(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/discard\n\ngo 1.27\n",
		"main.go": discardedResultProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "discard", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	runProgram(t, filepath.Join(h.mod, "discard"))
}
