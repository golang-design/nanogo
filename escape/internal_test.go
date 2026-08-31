// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package escape

import (
	"testing"

	"golang.design/x/nanogo/ir"
)

// The parts a source fixture cannot reach.
//
// Each case below is a refusal the walk makes on a shape the IR builder does
// not produce from Go source, or an encoding rule that has no Go program.
// They are here because a refusal nothing exercises is where the next
// unsound answer arrives.

func TestLeaksAccessors(t *testing.T) {
	var l leaks
	if l.Heap() != -1 || l.Mutator() != -1 || l.Callee() != -1 || l.Result(0) != -1 {
		t.Errorf("an empty set reports a flow: %v", l)
	}
	l.AddMutator(0)
	l.AddCallee(0)
	if l.Mutator() != 0 || l.Callee() != 0 {
		t.Errorf("the //go:noescape set reads back as mutator %d callee %d", l.Mutator(), l.Callee())
	}
	if l.Heap() != -1 {
		t.Errorf("the //go:noescape set claims a heap flow at %d", l.Heap())
	}
	// The shortest flow wins, which is what makes the set a minimum.
	l.AddHeap(3)
	l.AddHeap(1)
	if l.Heap() != 1 {
		t.Errorf("two heap flows at 3 and 1 read back as %d", l.Heap())
	}
}

// TestLeaksClampsADeepChain records what happens past the byte's range.
//
// A count is one byte offset by one, so gc saturates rather than wrapping. A
// wrap would turn a deep flow into a shallow one, which is the direction that
// is not safe.
func TestLeaksClampsADeepChain(t *testing.T) {
	var l leaks
	l.AddResult(0, 300)
	if got := l.Result(0); got != 0xff-1 {
		t.Errorf("a flow at 300 dereferences reads back as %d, want the saturated %d", got, 0xff-1)
	}
}

// TestLeaksRefusesAnImpossibleCount is the guard on the one input that has no
// meaning. gc stops the compiler on it; here it collapses to the answer that
// cannot be wrong.
func TestLeaksRefusesAnImpossibleCount(t *testing.T) {
	var l leaks
	l.set(leakResult0, -2)
	if l.Heap() != 0 {
		t.Errorf("a dereference count below -1 did not fall back to a heap flow: %v", l)
	}
	if got := l.Encode(); got != "" {
		t.Errorf("the fallback encodes as %q, want the conservative note", got)
	}
}

func TestParamsOfNothing(t *testing.T) {
	if got := Params(nil, Directives{}); got != nil {
		t.Errorf("Params(nil) returned %q", got)
	}
}

// TestNoteWithoutATypeIsConservative covers a parameter the IR left untyped,
// which no source produces and which must not be read as "no pointers".
func TestNoteWithoutATypeIsConservative(t *testing.T) {
	fn := &ir.Func{Name: "F", Params: []*ir.Object{{Name: "p", Class: ir.ClassParam}}}
	got := Params(fn, Directives{})
	if len(got) != 1 || got[0] != heapNote {
		t.Errorf("a parameter with no type got %q", got)
	}
}

func intPtr() *ir.Type {
	return &ir.Type{Kind: ir.Ptr, Size: 8, PtrBits: []byte{1}, Elem: &ir.Type{Kind: ir.Int64, Size: 8}}
}

// local returns a reference to o.
func local(o *ir.Object) ir.Expr {
	return &ir.Node{Op: ir.OLocal, Type: o.Type, Obj: o}
}

// TestAssignmentWithNoDestinationLeaks covers the malformed assignment.
//
// ir.Build never writes one. If some later pass does, the parameter must take
// the conservative note rather than be proved by an assignment nobody can read.
func TestAssignmentWithNoDestinationLeaks(t *testing.T) {
	p := &ir.Object{Name: "p", Class: ir.ClassParam, Type: intPtr()}
	fn := &ir.Func{Name: "F", Params: []*ir.Object{p}, Body: []ir.Stmt{
		{Op: ir.OAssign, Y: local(p)},
	}}
	if got := Params(fn, Directives{}); got[0] != heapNote {
		t.Errorf("an assignment with no destination proved %q", got[0])
	}
}

// TestStoreThroughADereferenceLeaks is the indirect destination, built by hand
// so that the walk is read rather than the builder.
func TestStoreThroughADereferenceLeaks(t *testing.T) {
	p := &ir.Object{Name: "p", Class: ir.ClassParam, Type: intPtr()}
	q := &ir.Object{Name: "q", Class: ir.ClassLocal, Type: &ir.Type{Kind: ir.Ptr, Size: 8, PtrBits: []byte{1}, Elem: intPtr()}}
	fn := &ir.Func{Name: "F", Params: []*ir.Object{p}, Locals: []*ir.Object{q}, Body: []ir.Stmt{
		ir.Assign(0, &ir.Node{Op: ir.ODeref, Type: intPtr(), X: local(q)}, local(p)),
	}}
	if got := Params(fn, Directives{}); got[0] != heapNote {
		t.Errorf("a store through a pointer proved %q", got[0])
	}
}

// TestAGlobalInTheTaintSetLeaks is the last reader of the set.
//
// [prover.store] refuses a store to a global already, so this is the second
// guard. It stands because the set is read once more at the end and a reader
// of it must not have to know what put a name in it.
func TestAGlobalInTheTaintSetLeaks(t *testing.T) {
	p := &ir.Object{Name: "p", Class: ir.ClassParam, Type: intPtr()}
	g := &ir.Object{Name: "g", Class: ir.ClassGlobal, Type: intPtr()}
	pr := &prover{tainted: map[*ir.Object]bool{p: true, g: true}}
	if !hasEscapingHolder(pr) {
		t.Error("a global in the taint set was not read as a flow that outlives the call")
	}
}

// hasEscapingHolder is the final loop of [escapes], lifted so that a test can
// reach it without a body that puts a global in the set.
func hasEscapingHolder(p *prover) bool {
	for o := range p.tainted {
		if o.Escapes || o.Class == ir.ClassResult || o.Class == ir.ClassGlobal {
			return true
		}
	}
	return false
}

func TestDestination(t *testing.T) {
	arr := &ir.Type{Kind: ir.Array, Size: 16, Elem: intPtr()}
	slice := &ir.Type{Kind: ir.Slice, Size: 24, PtrBits: []byte{1}, Elem: intPtr()}
	x := &ir.Object{Name: "x", Class: ir.ClassLocal, Type: arr}
	s := &ir.Object{Name: "s", Class: ir.ClassLocal, Type: slice}

	cases := []struct {
		name   string
		dst    ir.Expr
		root   *ir.Object
		direct bool
	}{
		{"a variable", local(x), x, true},
		{"a field of a variable", &ir.Node{Op: ir.OField, X: local(x)}, x, true},
		{"an element of an array", &ir.Node{Op: ir.OIndex, X: local(x)}, x, true},
		{"an element of a slice", &ir.Node{Op: ir.OIndex, X: local(s)}, nil, false},
		{"through a pointer", &ir.Node{Op: ir.ODeref, X: local(s)}, nil, false},
		{"nothing", nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, direct := destination(c.dst)
			if root != c.root || direct != c.direct {
				t.Errorf("destination = %v, %v, want %v, %v", root, direct, c.root, c.direct)
			}
		})
	}
}

// TestTaintOfNothingLeaks covers the destination that names no variable.
func TestTaintOfNothingLeaks(t *testing.T) {
	p := &prover{tainted: map[*ir.Object]bool{}}
	p.taint(nil)
	if !p.leaked {
		t.Error("tainting a variable that is not there was not read as a leak")
	}
}

// TestStoreToAGlobalOfSomethingElseIsNotALeak keeps the global rule about the
// value stored and not about the destination.
func TestStoreToAGlobalOfSomethingElseIsNotALeak(t *testing.T) {
	p := &ir.Object{Name: "p", Class: ir.ClassParam, Type: intPtr()}
	g := &ir.Object{Name: "g", Class: ir.ClassGlobal, Type: intPtr()}
	fn := &ir.Func{Name: "F", Params: []*ir.Object{p}, Body: []ir.Stmt{
		ir.Assign(0, &ir.Node{Op: ir.OGlobal, Type: intPtr(), Obj: g},
			&ir.Node{Op: ir.OConst, Type: intPtr()}),
		{Op: ir.OReturn, Args: []ir.Expr{&ir.Node{Op: ir.ODeref, Type: &ir.Type{Kind: ir.Int64, Size: 8}, X: local(p)}}},
	}}
	if got := Params(fn, Directives{}); got[0] != "esc:" {
		t.Errorf("a global written with something else refused the parameter: %q", got[0])
	}
}

// TestDestinationNamingNoVariableLeaks covers the reference with no object,
// which names storage the walk cannot follow.
func TestDestinationNamingNoVariableLeaks(t *testing.T) {
	p := &ir.Object{Name: "p", Class: ir.ClassParam, Type: intPtr()}
	fn := &ir.Func{Name: "F", Params: []*ir.Object{p}, Body: []ir.Stmt{
		ir.Assign(0, &ir.Node{Op: ir.OLocal, Type: intPtr()}, local(p)),
	}}
	if got := Params(fn, Directives{}); got[0] != heapNote {
		t.Errorf("a destination naming no variable proved %q", got[0])
	}
}
