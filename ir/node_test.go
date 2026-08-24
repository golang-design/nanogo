// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import "testing"

func TestOpStrings(t *testing.T) {
	for op := OConst; op < opCount; op++ {
		if got := op.String(); got == "" || got == "op(?)" {
			t.Errorf("op %d prints %q", op, got)
		}
	}
	if got := Op(200).String(); got != "op(?)" {
		t.Errorf("an out-of-range op prints %q", got)
	}
	if got := OpInvalid.String(); got != "invalid" {
		t.Errorf("OpInvalid prints %q", got)
	}
}

func TestClassStrings(t *testing.T) {
	for c := ClassParam; c <= ClassType; c++ {
		if got := c.String(); got == "" || got == "class(?)" {
			t.Errorf("class %d prints %q", c, got)
		}
	}
	if got := Class(200).String(); got != "class(?)" {
		t.Errorf("an out-of-range class prints %q", got)
	}
}

// TestGoSpecificSetIsTheLoweringTable checks the set against
// specs/020-ir.md's lowering table. Every construct in that table must be
// recognised, because the SSA builder's invariant check is what enforces the
// claim that no Go construct survives into SSA.
func TestGoSpecificSetIsTheLoweringTable(t *testing.T) {
	mustLower := []Op{
		ORange, OTypeAssert, OTypeSwitch, OClosure, ODefer, OGo, OSend, ORecv,
		OMake, OAppend, OCopy, ODelete, OPanic, ORecover, OLen, OCap, ONew,
		OComplex, OReal, OImag, OClear, OMin, OMax, OSelect,
		OCompositeLit, OSlice, OClose, OPrint, OPrintln,
		OUnsafeAdd, OUnsafeSlice, OUnsafeSliceData, OUnsafeString, OUnsafeStringData,
	}
	for _, op := range mustLower {
		if !op.IsGoSpecific() {
			t.Errorf("%s is in the lowering table and is not marked Go-specific", op)
		}
	}

	// The machine-level operations must not be marked, or the invariant check
	// would reject every function.
	for _, op := range []Op{
		OConst, OLocal, OGlobal, OField, OIndex, ODeref, OAddr,
		OUnary, OBinary, OCompare, OConvert, OCall,
		OIf, OFor, OSwitch, OBlock, OGoto, OLabel, OReturn, OBreak, OContinue,
		OAssign, OCase,
	} {
		if op.IsGoSpecific() {
			t.Errorf("%s is marked Go-specific and must survive into SSA", op)
		}
	}
}

func TestWalkVisitsEveryChildList(t *testing.T) {
	marker := &Node{Op: OConst}
	for _, tc := range []struct {
		what string
		root *Node
	}{
		{"X", &Node{Op: OUnary, X: marker}},
		{"Y", &Node{Op: OBinary, X: &Node{Op: OConst}, Y: marker}},
		{"Args", &Node{Op: OCall, Args: []Expr{marker}}},
		{"Init", &Node{Op: OIf, Init: []Stmt{marker}}},
		{"Body", &Node{Op: OFor, Body: []Stmt{marker}}},
		{"Post", &Node{Op: OFor, Post: []Stmt{marker}}},
		{"Else", &Node{Op: OIf, Else: []Stmt{marker}}},
		{"nested", &Node{Op: OBlock, Body: []Stmt{{Op: OIf, Body: []Stmt{marker}}}}},
	} {
		found := false
		Walk(tc.root, func(n *Node) bool {
			if n == marker {
				found = true
			}
			return true
		})
		if !found {
			t.Errorf("%s: the walk did not reach the child", tc.what)
		}
	}
}

func TestWalkSkipsAndStops(t *testing.T) {
	deep := &Node{Op: OConst}
	root := &Node{Op: OBlock, Body: []Stmt{{Op: OFor, Body: []Stmt{deep}}}}

	visited := 0
	Walk(root, func(n *Node) bool {
		visited++
		return n.Op != OFor // do not descend into the loop
	})
	if visited != 2 {
		t.Errorf("visited %d nodes, want the block and the loop only", visited)
	}

	// A nil node is not visited, so a caller need not check at every use.
	Walk(nil, func(*Node) bool {
		t.Error("Walk visited a nil node")
		return true
	})
}

// TestHasGoSpecific is the invariant check itself. It must find a construct
// anywhere in the tree, and it must report the first one so a diagnostic can
// name it.
func TestHasGoSpecific(t *testing.T) {
	clean := &Node{Op: OBlock, Body: []Stmt{
		{Op: OIf, X: &Node{Op: OCompare}, Body: []Stmt{{Op: OReturn}}},
	}}
	if op, bad := HasGoSpecific(clean); bad {
		t.Errorf("a lowered tree reported %s as unlowered", op)
	}

	dirty := &Node{Op: OBlock, Body: []Stmt{
		{Op: OFor, Body: []Stmt{{Op: OAppend}}},
	}}
	op, bad := HasGoSpecific(dirty)
	if !bad {
		t.Fatal("an unlowered tree passed the invariant check")
	}
	if op != OAppend {
		t.Errorf("reported %s, want %s", op, OAppend)
	}

	// It must look inside every child list, not only the body.
	for _, root := range []*Node{
		{Op: OCall, Args: []Expr{{Op: ORecover}}},
		{Op: OIf, Init: []Stmt{{Op: ODefer}}},
		{Op: OFor, Post: []Stmt{{Op: OAppend}}},
		{Op: OIf, Else: []Stmt{{Op: OGo}}},
		{Op: OUnary, X: &Node{Op: OMake}},
		{Op: OBinary, X: &Node{Op: OConst}, Y: &Node{Op: OLen}},
	} {
		if _, bad := HasGoSpecific(root); !bad {
			t.Errorf("%s: an unlowered child was not found", root.Op)
		}
	}
}

func TestObjectAndFuncCarryTheirFacts(t *testing.T) {
	// The three fields the backend reads off an Object are the ones a mistake
	// in makes memory corruption rather than a failed test, so their defaults
	// are pinned.
	o := &Object{Name: "x", Class: ClassLocal}
	if o.Addrtaken || o.Escapes {
		t.Error("an object starts address-taken or escaping")
	}

	f := &Func{Name: "f"}
	if f.Inlinable {
		t.Error("a function starts inlinable; the decision belongs to specs/024")
	}
}

// constFor is a ConstValue for the tests below.
type constFor struct {
	text string
	i    int64
	ok   bool
}

func (c constFor) String() string           { return c.text }
func (c constFor) Int64() (int64, bool)     { return c.i, c.ok }
func (c constFor) Uint64() (uint64, bool)   { return uint64(c.i), c.ok && c.i >= 0 }
func (c constFor) Float64() (float64, bool) { return float64(c.i), c.ok }
func (c constFor) IsZero() bool             { return c.ok && c.i == 0 }

// TestConstValueIsOptional pins the reason ConstValue is a second interface
// rather than a wider Value: a consumer that only prints a constant must not
// have to implement four more methods, and a consumer that needs a number
// learns from the assertion that it cannot have one.
func TestConstValueIsOptional(t *testing.T) {
	var plain Value = constString("x")
	if _, ok := plain.(ConstValue); ok {
		t.Error("a plain Value satisfied ConstValue")
	}

	var rich Value = constFor{text: "42", i: 42, ok: true}
	cv, ok := rich.(ConstValue)
	if !ok {
		t.Fatal("a ConstValue did not satisfy ConstValue")
	}
	if n, exact := cv.Int64(); n != 42 || !exact {
		t.Errorf("Int64 = %d, %v", n, exact)
	}
	if cv.IsZero() {
		t.Error("42 reported as zero")
	}
	if zero := (constFor{text: "0", i: 0, ok: true}); !zero.IsZero() {
		t.Error("0 did not report as zero")
	}
}

// constString is a Value that can only be printed.
type constString string

func (c constString) String() string { return string(c) }

// TestAssignAndCaseAreOps guards the two ops that were added after their first
// consumers had invented conventions for them. A convention in two packages is
// two conventions.
func TestAssignAndCaseAreOps(t *testing.T) {
	if OAssign.String() != "assign" || OCase.String() != "case" {
		t.Errorf("OAssign prints %q and OCase prints %q", OAssign, OCase)
	}
	if OAssign.IsGoSpecific() || OCase.IsGoSpecific() {
		t.Error("an assignment or a case was marked Go-specific and would be rejected after lowering")
	}
}
