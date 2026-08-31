// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// A frame object the collector reads before the generator writes it.
//
// ssa/liveness.go computes a frame object's lifetime backwards from its uses
// and nothing emits a lifetime marker, so an object is live from its last
// access up to the function entry. The locals bitmap therefore claims the
// object's pointer words at every safepoint above the last write, including
// the ones before anything wrote them, and the collector follows whatever the
// previous frame left at those addresses. ssagen/prologue.go's
// zeroFrameObjects is what makes the claim true.
//
// The failure this catches is not a wrong answer. It is
//
//	runtime: bad pointer in frame main.victim at 0x...: 0x97
//	fatal error: invalid pointer found on stack
//
// which the stack copier throws when it finds a word the bitmap calls a
// pointer holding a value below the first legal page. The same shape stopped
// stage 2 of specs/060-selfhost.md inside rtype.fieldName, which returns a
// struct too large for the registers and writes the result object only after
// its last call.

// framezeroProgram fills the stack with a value that is not a legal pointer,
// then leaves a frame object over it and grows the stack.
//
// Each function has a part and none of them is decoration.
//
// dirty writes 0x97 over a wide span of the stack. The array is of uintptr, so
// it holds no pointer, nothing describes it to the collector and nothing
// clears it. The value is what the runtime prints: it is above zero and below
// the first legal page, which is the whole trigger for the throw.
//
// victim returns a struct too large for the argument registers, so the result
// lives in a frame object, and its only write is the copy after inner returns.
// Its frame lands on the words dirty wrote, and its call to inner is a
// safepoint the object is described at.
//
// deep recurses far enough to grow the stack while victim's frame is on it.
// The stack copier walks every frame it moves and reads the locals bitmap of
// each one, so it is the copy and not a collection that reads victim's frame.
//
// wide is the second half and it is a size and not a shape. sym is ten pointer
// words, which zeroFrameObjects stores one at a time, and wideSym is
// twenty-four, which is over zeroUnroll and takes the loop instead. The two
// paths emit different instructions, so a program that only reached the first
// would leave the second to the self-host build to find.
const framezeroProgram = `package main

type sym struct {
	Name   string
	Kind   int
	Data   []byte
	Relocs []int
	Tag    string
}

type wideSym struct {
	A, B, C, D, E, F, G, H          string
	I, J, K, L, M, N, O, P          []byte
	Q                               int
}

//go:noinline
func dirty(n int) int {
	var a [192]uintptr
	for i := range a {
		a[i] = 0x97
	}
	s := 0
	for i := 0; i < len(a); i += 7 {
		s += int(a[i])
	}
	if s == 0 {
		return 0
	}
	return n
}

//go:noinline
func deep(n int) int {
	if n == 0 {
		return 0
	}
	var pad [96]int
	pad[0] = n
	pad[95] = n
	return pad[0] + pad[95] + deep(n-1)
}

//go:noinline
func inner(n int) sym {
	if deep(n) < 0 {
		return sym{}
	}
	return sym{Name: "n", Kind: n, Data: []byte("d"), Relocs: []int{n}, Tag: "t"}
}

//go:noinline
func victim(n int) sym {
	return inner(n)
}

//go:noinline
func innerWide(n int) wideSym {
	if deep(n) < 0 {
		return wideSym{}
	}
	return wideSym{A: "a", H: "h", I: []byte("i"), P: []byte("p"), Q: n}
}

//go:noinline
func victimWide(n int) wideSym {
	return innerWide(n)
}

func main() {
	dirty(3)
	s := victim(400)
	println(len(s.Name), s.Kind, len(s.Data), len(s.Relocs), len(s.Tag))
	dirty(3)
	w := victimWide(400)
	println(len(w.A), len(w.H), len(w.I), len(w.P), w.Q)
}
`

// TestToolexecClearsAFrameObjectTheCollectorReadsEarly compiles, links and
// runs the program, and compares what it printed against gc.
func TestToolexecClearsAFrameObjectTheCollectorReadsEarly(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/framezero\n\ngo 1.27\n",
		"main.go": framezeroProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "framezero", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "framezero"))
	if want := "1 400 1 1 1\n1 1 1 1 400\n"; string(got) != want {
		t.Errorf("the program printed %q, want %q", got, want)
	}
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed %q and gc's printed %q", got, want)
	}
}
