// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

func TestIdentifiersAreDense(t *testing.T) {
	f := NewFunc("t")
	if f.NumBlocks() != 1 {
		t.Fatalf("a new function has %d block identifiers, want 1", f.NumBlocks())
	}
	b := f.NewBlock(BlockPlain)
	v1 := f.Entry.NewValue(0, OpConstInt, tInt)
	v2 := b.NewValue(0, OpConstInt, tInt)
	if v1.ID != 0 || v2.ID != 1 || b.ID != 1 {
		t.Fatalf("identifiers are %d, %d and %d, want 0, 1 and 1", v1.ID, v2.ID, b.ID)
	}
	if f.NumValues() != 2 || f.NumBlocks() != 2 {
		t.Fatalf("counts are %d values and %d blocks, want 2 and 2", f.NumValues(), f.NumBlocks())
	}
	// Removing a block leaves the identifier space alone, so a table sized by
	// NumBlocks still indexes correctly.
	f.removeBlock(b)
	if f.NumBlocks() != 2 || len(f.Blocks) != 1 {
		t.Fatalf("after removal there are %d identifiers and %d blocks, want 2 and 1",
			f.NumBlocks(), len(f.Blocks))
	}
	// Removing a block that is not there changes nothing.
	f.removeBlock(b)
	if len(f.Blocks) != 1 {
		t.Fatalf("removing a block twice left %d blocks", len(f.Blocks))
	}
}

func TestValuePrinting(t *testing.T) {
	var nilValue *Value
	if got := nilValue.String(); got != "<nil>" {
		t.Errorf("a nil value prints as %q", got)
	}
	var nilBlock *Block
	if got := nilBlock.String(); got != "<nil>" {
		t.Errorf("a nil block prints as %q", got)
	}

	f, entry, mem := minimalFunc()
	o := &ir.Object{Name: "g", Type: tInt, Class: ir.ClassGlobal}
	a := entry.NewValue(0, OpAddr, tIntPtr)
	a.Aux = o
	c := entry.NewValue(0, OpConstInt, tInt)
	c.AuxInt = 42
	s := entry.NewValue(0, OpConstString, tString)
	s.Aux = "text"
	l := entry.NewValue(0, OpLoad, tInt, a, mem)

	if got := c.LongString(); !strings.Contains(got, "[42]") {
		t.Errorf("a constant prints as %q, want the auxiliary integer", got)
	}
	if got := a.LongString(); !strings.Contains(got, "{g}") {
		t.Errorf("an address prints as %q, want the symbol", got)
	}
	if got := s.LongString(); !strings.Contains(got, "{text}") {
		t.Errorf("a string constant prints as %q", got)
	}
	if got := l.LongString(); !strings.Contains(got, "v"+"") || !strings.Contains(got, "Load") {
		t.Errorf("a load prints as %q", got)
	}
	// An ir.Value in Aux prints as its Go syntax.
	c.Aux = litVal("0x2a")
	if got := c.LongString(); !strings.Contains(got, "{0x2a}") {
		t.Errorf("a constant with an ir.Value prints as %q", got)
	}

	dump := f.String()
	for _, want := range []string{"func t:", "b0:", "InitMem", "Ret"} {
		if !strings.Contains(dump, want) {
			t.Errorf("the dump does not contain %q:\n%s", want, dump)
		}
	}
}

func TestMemArg(t *testing.T) {
	_, entry, mem := minimalFunc()
	c := entry.NewValue(0, OpConstInt, tInt)
	if got := c.MemArg(); got != nil {
		t.Errorf("a constant has memory argument %v", got)
	}
	l := entry.NewValue(0, OpLoad, tInt, c, mem)
	if got := l.MemArg(); got != mem {
		t.Errorf("a load has memory argument %v, want %v", got, mem)
	}
	// A value whose operation takes memory but has no arguments at all is
	// malformed, and asking for its memory must not fault.
	broken := entry.NewValue(0, OpLoad, tInt)
	if got := broken.MemArg(); got != nil {
		t.Errorf("a load with no arguments has memory argument %v", got)
	}
	if !IsMemory(mem) || IsMemory(c) || IsMemory(nil) {
		t.Error("IsMemory disagrees with the memory type")
	}
}

func TestSetArgAndAddArg(t *testing.T) {
	_, entry, _ := minimalFunc()
	a := entry.NewValue(0, OpConstInt, tInt)
	b := entry.NewValue(0, OpConstInt, tInt)
	v := entry.NewValue(0, OpNeg, tInt, a)
	if v.Args[0] != a {
		t.Fatal("AddArg did not record the argument")
	}
	if len(a.uses) != 1 || a.uses[0] != v {
		t.Fatal("AddArg did not record the use")
	}
	v.SetArg(0, b)
	if v.Args[0] != b || len(b.uses) != 1 {
		t.Fatal("SetArg did not replace the argument")
	}
	v.AddArg(nil)
	if len(v.Args) != 2 || v.Args[1] != nil {
		t.Fatal("AddArg refused a nil argument")
	}
}

func TestBlockKindNames(t *testing.T) {
	for k, want := range map[BlockKind]string{
		BlockInvalid: "invalid",
		BlockPlain:   "Plain",
		BlockIf:      "If",
		BlockRet:     "Ret",
		BlockExit:    "Exit",
	} {
		if got := k.String(); got != want {
			t.Errorf("BlockKind(%d) is %q, want %q", k, got, want)
		}
	}
	if got := BlockKind(200).String(); got != "blockkind(?)" {
		t.Errorf("an unnamed block kind is %q", got)
	}
	if n, ok := BlockKind(200).NumSuccs(); ok || n != 0 {
		t.Errorf("an unnamed block kind has %d successors and ok is %v", n, ok)
	}
}

// TestDuplicateEdges covers the case that breaks a naive predecessor list.
//
// An if whose two arms are the same block gives that block two identical
// predecessors, and a phi there needs one argument per slot rather than one
// per distinct predecessor.
func TestDuplicateEdges(t *testing.T) {
	f := NewFunc("t")
	entry := f.Entry
	entry.Kind = BlockIf
	mem := entry.NewValue(0, OpInitMem, MemType)
	entry.Control = entry.NewValue(0, OpConstBool, tBool)
	join := f.NewBlock(BlockRet)
	entry.AddEdgeTo(join)
	entry.AddEdgeTo(join)

	a := entry.NewValue(0, OpConstInt, tInt)
	b := entry.NewValue(0, OpConstInt, tInt)
	phi := f.newValue(OpPhi, tInt, 0)
	phi.Block = join
	phi.AddArg(a)
	phi.AddArg(b)
	join.Values = append(join.Values, phi)
	join.Control = join.NewValue(0, OpMakeResult, MemType, phi, mem)

	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v on two edges to one block\n%s", vs, f)
	}
	if got := predIndex(entry, 0); got != 0 {
		t.Errorf("the first edge is predecessor slot %d, want 0", got)
	}
	if got := predIndex(entry, 1); got != 1 {
		t.Errorf("the second edge is predecessor slot %d, want 1", got)
	}
}

func TestPredIndexOfAMissingEdge(t *testing.T) {
	f := NewFunc("t")
	b := f.NewBlock(BlockPlain)
	f.Entry.Succs = append(f.Entry.Succs, b)
	if got := predIndex(f.Entry, 0); got != -1 {
		t.Errorf("predIndex of an unmirrored edge is %d, want -1", got)
	}
}

// TestSplitCriticalEdges checks the pass specs/026-register-allocation.md
// needs: after it, no edge runs from a block with several successors into a
// block with several predecessors.
func TestSplitCriticalEdges(t *testing.T) {
	f := NewFunc("t")
	entry := f.Entry
	entry.Kind = BlockIf
	mem := entry.NewValue(0, OpInitMem, MemType)
	entry.Control = entry.NewValue(0, OpConstBool, tBool)
	a := entry.NewValue(0, OpConstInt, tInt)

	mid := f.NewBlock(BlockPlain)
	join := f.NewBlock(BlockRet)
	entry.AddEdgeTo(mid)
	entry.AddEdgeTo(join) // critical: two successors into two predecessors
	mid.AddEdgeTo(join)

	b := mid.NewValue(0, OpConstInt, tInt)
	phi := f.newValue(OpPhi, tInt, 0)
	phi.Block = join
	// The entry's edge was added first, so it is predecessor 0 and mid is
	// predecessor 1.
	phi.AddArg(a)
	phi.AddArg(b)
	join.Values = append(join.Values, phi)
	join.Control = join.NewValue(0, OpMakeResult, MemType, phi, mem)

	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the fixture does not verify: %v\n%s", vs, f)
	}
	before := f.String()

	n := SplitCriticalEdges(f)
	if n != 1 {
		t.Fatalf("split %d edges, want 1\n%s", n, before)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v after splitting\n%s", vs, f)
	}
	if phi.Args[0] != a || phi.Args[1] != b {
		t.Errorf("the phi arguments moved: %v", phi.LongString())
	}
	if join.Preds[1] != mid {
		t.Errorf("predecessor 1 is %v, want the block that was not split", join.Preds[1])
	}
	d := join.Preds[0]
	if d == entry || len(d.Preds) != 1 || d.Preds[0] != entry || len(d.Succs) != 1 || d.Succs[0] != join {
		t.Errorf("the new block is %v with preds %v and succs %v", d, d.Preds, d.Succs)
	}
	if entry.Succs[1] != d {
		t.Errorf("the entry still branches to %v", entry.Succs[1])
	}
	// The pass is idempotent: there is nothing left to split.
	if n := SplitCriticalEdges(f); n != 0 {
		t.Errorf("a second run split %d edges, want 0", n)
	}
}

// TestSplitCriticalEdgesAfterConstruction runs the pass on what the builder
// produces. A switch with two case expressions for one clause is the shape
// that gives a clause body two predecessors.
func TestSplitCriticalEdgesAfterConstruction(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	y := obj("y", tInt, ir.ClassLocal)
	fn := fun("f", []*ir.Object{x, y},
		switchStmt(local(x),
			clause([]ir.Expr{cint("1"), cint("2")}, asn(local(y), cint("10"))),
			clause(nil, asn(local(y), cint("20")))),
		ret(local(y)))
	f := build(t, fn)
	if n := SplitCriticalEdges(f); n == 0 {
		t.Fatalf("no critical edge in a switch with two case expressions\n%s", f)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v after splitting\n%s", vs, f)
	}
	for _, b := range f.Blocks {
		if len(b.Succs) < 2 {
			continue
		}
		for _, s := range b.Succs {
			if len(s.Preds) > 1 {
				t.Errorf("%v -> %v is still critical\n%s", b, s, f)
			}
		}
	}
}

func TestRemoveValue(t *testing.T) {
	_, entry, _ := minimalFunc()
	v := entry.NewValue(0, OpConstInt, tInt)
	n := len(entry.Values)
	entry.removeValue(v)
	if len(entry.Values) != n-1 || !v.dead {
		t.Fatalf("removeValue left %d values and dead is %v", len(entry.Values), v.dead)
	}
	// Removing it again is a no-op rather than a panic.
	entry.removeValue(v)
	if len(entry.Values) != n-1 {
		t.Fatalf("removing twice left %d values", len(entry.Values))
	}
}

func TestAuxString(t *testing.T) {
	if got := auxString(&ir.Object{Name: "sym"}); got != "sym" {
		t.Errorf("an object prints as %q", got)
	}
	if got := auxString(litVal("1")); got != "1" {
		t.Errorf("an ir.Value prints as %q", got)
	}
	if got := auxString(7); got != "7" {
		t.Errorf("an integer prints as %q", got)
	}
}
