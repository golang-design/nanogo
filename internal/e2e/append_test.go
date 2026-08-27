// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// specs/020-ir.md's append row, run as a program.
//
// A unit test can read the growslice call the row lowers to and cannot say
// whether the bounds it computed are inside the allocation. The fast path
// writes through the slice's own backing array, so a length that is one too
// large stores past the end of an object the collector owns, and neither the
// collector nor the store reports it. The failure appears later, somewhere
// else, as another object's field.
//
// So the program grows a slice past its capacity many times and reads every
// element back. The reads are what turn a wrong bound into an exit status: a
// store that landed outside the array leaves the element it was meant for
// holding whatever growslice put there.
//
// Every check divides by zero and the process dies, as in helloProgram: the
// exit status is the assertion.
const appendProgram = `package main

//go:noinline
func crash() {
	d := 0
	d = d / d
}

//go:noinline
func value(i int) int { return i*7 + 1 }

func main() {
	// Growth past capacity, over and over. 400 elements from an empty slice
	// is nine calls to growslice, each of which moves the backing array.
	var xs []int
	for i := 0; i < 400; i++ {
		xs = append(xs, value(i))
	}
	if len(xs) != 400 {
		crash()
	}
	for i := 0; i < 400; i++ {
		if xs[i] != value(i) {
			crash()
		}
	}

	// More than one element per call, which stores at newLen-num onwards.
	var ys []int
	for i := 0; i < 100; i++ {
		ys = append(ys, value(i), value(i)+1, value(i)+2)
	}
	if len(ys) != 300 {
		crash()
	}
	for i := 0; i < 100; i++ {
		if ys[i*3] != value(i) || ys[i*3+1] != value(i)+1 || ys[i*3+2] != value(i)+2 {
			crash()
		}
	}

	// The spread form, growing as it goes.
	var zs []int
	for i := 0; i < 100; i++ {
		zs = append(zs, xs[i:i+4]...)
	}
	if len(zs) != 400 {
		crash()
	}
	for i := 0; i < 100; i++ {
		for j := 0; j < 4; j++ {
			if zs[i*4+j] != value(i+j) {
				crash()
			}
		}
	}

	// A spread of nothing into a slice with no spare capacity. The element the
	// move would start at is one past the end of the allocation.
	full := make([]int, 3, 3)
	var empty []int
	full = append(full, empty...)
	if len(full) != 3 || cap(full) != 3 {
		crash()
	}

	// A slice appended to itself, which reads the operand out of the header
	// the source named and not out of the one growslice wrote.
	self := []int{1, 2, 3}
	self = append(self, self...)
	if len(self) != 6 || self[0] != 1 || self[3] != 1 || self[5] != 3 {
		crash()
	}

	// Bytes from a string, which is the other operand the spread takes.
	var bs []byte
	for i := 0; i < 50; i++ {
		bs = append(bs, "xyz"...)
	}
	if len(bs) != 150 || bs[0] != 'x' || bs[149] != 'z' {
		crash()
	}

	// append(s) is the operand and grows nothing.
	same := make([]int, 2, 9)
	same = append(same)
	if len(same) != 2 || cap(same) != 9 {
		crash()
	}
}
`

// TestToolexecAppendGrowsPastCapacity reads every element of a slice that
// outgrew its backing array nine times.
func TestToolexecAppendGrowsPastCapacity(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/appendgrow\n\ngo 1.27\n",
		"main.go": appendProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "appendgrow", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	if b, err := exec.Command(filepath.Join(h.mod, "appendgrow")).CombinedOutput(); err != nil {
		t.Fatalf("an element did not hold what append put there: %v\n%s", err, b)
	}
}

// appendPointerProgram is the pointer-element half of the row.
//
// growslice past the capacity allocates a new backing array and copies the old
// one into it, and it reads the pointer map of that array out of the element
// descriptor the call was given. A descriptor that named the slice rather than
// the element, or an element type spelled wrong, produces an array the
// collector scans with the wrong bits: it frees the elements while the slice
// still holds them.
//
// Nothing but the slice refers to any node, and each node's next is the node
// before it, so a chain the collector broke is a read through a freed object.
// It runs under GODEBUG=gccheckmark=1, which marks a second time with the
// world stopped and compares, and under clobberfree=1, so a freed object read
// through a stale pointer holds a recognisable pattern rather than its old
// contents. GOGC=1 collects as often as it can.
const appendPointerProgram = `package main

import "runtime"

type node struct {
	a, b, c, d int
	next       *node
}

//go:noinline
func crash() {
	d := 0
	d = d / d
}

//go:noinline
func churn() {
	for i := 0; i < 4096; i++ {
		_ = &node{a: i}
	}
}

//go:noinline
func fresh(i int, prev *node) *node {
	return &node{a: i, next: prev}
}

func main() {
	// The slice is the only reference to any node, and it moves eight times.
	var ns []*node
	var prev *node
	for i := 0; i < 300; i++ {
		prev = fresh(i, prev)
		ns = append(ns, prev)
		if i%32 == 0 {
			churn()
			runtime.GC()
		}
	}
	churn()
	runtime.GC()
	churn()

	if len(ns) != 300 {
		crash()
	}
	// Every element, and the chain each one leads back along.
	for i := 0; i < 300; i++ {
		if ns[i].a != i {
			crash()
		}
		if i > 0 && ns[i].next != ns[i-1] {
			crash()
		}
	}
	// The chain from the last node reaches the first, through 299 pointers the
	// collector had to keep.
	n := ns[299]
	count := 0
	for n.next != nil {
		n = n.next
		count = count + 1
	}
	runtime.GC()
	if count != 299 || n.a != 0 {
		crash()
	}

	// The spread form moves pointers too, and memmove is what copies them.
	var ms []*node
	for i := 0; i < 60; i++ {
		ms = append(ms, ns[i*5:i*5+5]...)
	}
	churn()
	runtime.GC()
	if len(ms) != 300 {
		crash()
	}
	for i := 0; i < 300; i++ {
		if ms[i] != ns[i] || ms[i].a != i {
			crash()
		}
	}
}
`

// TestToolexecAppendKeepsPointersThroughACollection is the evidence that the
// element descriptor growslice was given describes the new backing array to
// the collector.
func TestToolexecAppendKeepsPointersThroughACollection(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/appendgc\n\ngo 1.27\n",
		"main.go": appendPointerProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "appendgc", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	cmd := exec.Command(filepath.Join(h.mod, "appendgc"))
	cmd.Env = append(os.Environ(), "GODEBUG=gccheckmark=1,clobberfree=1", "GOGC=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the collector or the program rejected what the slice held: %v\n%s", err, b)
	}
}
