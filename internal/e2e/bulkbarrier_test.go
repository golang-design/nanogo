// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// The bulk half of the write barrier, per specs/034-write-barriers.md.
//
// ssa/writebarrier.go covers a store of one pointer, and neither program below
// makes one. A copy of a value holding several pointers is one OpMove and a
// copy between two slices is one call, so there is no pointer store for that
// pass to match, and both wrote their words with nothing recorded until
// ssa/bulkbarrier.go and ir.sliceCopy existed.
//
// # How to make this kind of program fail
//
// Three properties, and leaving any one out gives a program that passes with
// the barrier missing. The first draft of each of these was such a program.
//
// The objects must be allocated BEFORE the shuffling starts. An object
// allocated while the collector is marking is allocated black, so it cannot be
// lost, and a probe that allocates as it goes measures nothing.
//
// The source must be cleared by a copy and never field by field. A nil store
// through a field carries the deletion half of the scalar barrier, which shades
// the very object being moved. That is a real barrier doing real work, and in a
// probe it is an accident that hides the gap under test.
//
// The only reference to each object must be the one being copied. Both banks
// below hold every object exactly once, so a copy that is not recorded is the
// last chance the collector had to see it.
//
// GODEBUG=gccheckmark=1 marks a second time with the world stopped and compares,
// so a lost object is a crash naming the object rather than a wrong answer
// later, and clobberfree=1 makes a read through a freed pointer recognisable.
// GOGC=1 collects as often as it can.

// bulkMoveProgram copies a struct of twelve pointers between two banks in the
// heap. nanogo lowers the assignment to one OpMove, which is runtime.memmove
// unless ssa/bulkbarrier.go marks it, and runtime.memmove records nothing.
const bulkMoveProgram = `package main

type wide struct {
	p [12]*int
	n int
}

//go:noinline
func copyWide(dst, src *wide) { *dst = *src }

// clear overwrites the source with the zero value. A field-by-field nil store
// would carry the deletion half of the scalar barrier and shade the object
// being moved, which is what made the first version of this program pass.
//
//go:noinline
func clear(w *wide) { *w = wide{} }

//go:noinline
func churn(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		p := new([64]int)
		p[0] = i
		s += p[0]
	}
	return s
}

func main() {
	a := make([]wide, 48)
	b := make([]wide, 48)
	want := 0
	for i := range a {
		for j := range a[i].p {
			x := new(int)
			*x = i*100 + j
			a[i].p[j] = x
			want += *x
		}
	}
	for r := 0; r < 4000; r++ {
		for i := range a {
			copyWide(&b[i], &a[i])
			clear(&a[i])
		}
		churn(32)
		for i := range b {
			copyWide(&a[i], &b[i])
			clear(&b[i])
		}
		churn(32)
		got := 0
		for i := range a {
			for j := range a[i].p {
				got += *a[i].p[j]
			}
		}
		if got != want {
			panic("an object the copy moved was collected")
		}
	}
}
`

// bulkSliceProgram is the same shape through the copy builtin, which
// ir.sliceCopy answers with runtime.typedslicecopy rather than
// runtime.memmove.
const bulkSliceProgram = `package main

//go:noinline
func move(dst, src []*int) { copy(dst, src) }

//go:noinline
func wipe(s []*int) {
	var zero [24]*int
	copy(s, zero[:])
}

//go:noinline
func churn(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		p := new([64]int)
		p[0] = i
		s += p[0]
	}
	return s
}

func main() {
	a := make([][]*int, 32)
	b := make([][]*int, 32)
	want := 0
	for i := range a {
		a[i] = make([]*int, 24)
		b[i] = make([]*int, 24)
		for j := range a[i] {
			x := new(int)
			*x = i*100 + j
			a[i][j] = x
			want += *x
		}
	}
	for r := 0; r < 4000; r++ {
		for i := range a {
			move(b[i], a[i])
			wipe(a[i])
		}
		churn(32)
		for i := range b {
			move(a[i], b[i])
			wipe(b[i])
		}
		churn(32)
		got := 0
		for i := range a {
			for j := range a[i] {
				got += *a[i][j]
			}
		}
		if got != want {
			panic("an object the copy moved was collected")
		}
	}
}
`

// buildAndCollect builds src with nanogo and runs it while the collector works
// as hard as it can. It is runUnderCollector with the build in front of it,
// because the program under test has to come from nanogo and not from gc.
func buildAndCollect(t *testing.T, mod, name, src string) {
	t.Helper()
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/" + mod + "\n\ngo 1.27\n",
		"main.go": src,
	}, []string{"main"})

	if out, err := h.build(t, "-o", name, "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package, so this measures gc:\n%s", strings.Join(lines, "\n"))
	}
	runUnderCollector(t, filepath.Join(h.mod, name))
}

// TestToolexecKeepsPointersThroughAStructCopy is the OpMove half.
//
// Without ssa/bulkbarrier.go this program failed 10 runs in 10 with
// "checkmarks found unexpected unmarked object".
func TestToolexecKeepsPointersThroughAStructCopy(t *testing.T) {
	buildAndCollect(t, "bulkmove", "bulkmove", bulkMoveProgram)
}

// TestToolexecKeepsPointersThroughASliceCopy is the copy builtin half, and it
// failed the same way and as often.
func TestToolexecKeepsPointersThroughASliceCopy(t *testing.T) {
	buildAndCollect(t, "bulkslice", "bulkslice", bulkSliceProgram)
}
