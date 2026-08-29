// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// bbType returns a named struct of n pointer words, laid out.
func bbType(name string, n int) *ir.Type {
	t := &ir.Type{Kind: ir.Struct, Name: name, PkgPath: "main"}
	for i := 0; i < n; i++ {
		t.Fields = append(t.Fields, ir.Field{Name: "p", Type: tIntPtr})
	}
	return mkType(t)
}

// bbMove builds a function whose one block copies a value of type t from one
// address to another. dstOp decides where the destination came from.
func bbMove(t *ir.Type, dstOp Op) (*Func, *Value) {
	f, e, mem := raFunc("move")
	pt := mkType(&ir.Type{Kind: ir.Ptr, Elem: t})
	var dst *Value
	if dstOp == OpLocalAddr {
		dst = e.NewValue(0, OpLocalAddr, pt, mem)
		dst.Aux = &ir.Object{Name: "local", Type: t, Class: ir.ClassLocal}
	} else {
		dst = e.NewValue(0, OpArg, pt)
	}
	src := e.NewValue(0, OpArg, pt)
	mv := e.NewValue(0, OpMove, MemType, dst, src, mem)
	mv.AuxInt = t.Size
	raRet(e, mv)
	return f, mv
}

// A copy of a value holding pointers into memory the collector may own is
// runtime.memmove today and records nothing. The pass marks it so that
// lowering emits runtime.typedmemmove instead, and the mark is the type's
// descriptor.
func TestBulkBarrierMarksAPointerCopy(t *testing.T) {
	ty := bbType("wide", 12)
	f, mv := bbMove(ty, OpArg)

	types, err := BulkBarriers(f)
	if err != nil {
		t.Fatalf("BulkBarriers: %v", err)
	}
	o := BulkBarrierDescriptor(mv)
	if o == nil {
		t.Fatalf("a copy of %s was not marked, so lowering emits runtime.memmove and the collector is told nothing:\n%s", ty, f)
	}
	if !strings.HasPrefix(o.Name, "type:") {
		t.Errorf("the mark names %q, which is not a type descriptor", o.Name)
	}
	if len(types) != 1 || types[0] != ty {
		t.Errorf("the pass reported %v as the descriptors this object owes, want just %s", types, ty)
	}
}

// A copy of a value that holds no pointer needs no barrier. The collector has
// nothing to follow in it, and runtime.typedmemmove would cost a call into the
// runtime and a descriptor this object would then owe.
func TestBulkBarrierLeavesAPointerFreeCopyAlone(t *testing.T) {
	ty := mkType(&ir.Type{Kind: ir.Array, Elem: tInt, Len: 12})
	f, mv := bbMove(ty, OpArg)

	types, err := BulkBarriers(f)
	if err != nil {
		t.Fatalf("BulkBarriers: %v", err)
	}
	if o := BulkBarrierDescriptor(mv); o != nil {
		t.Errorf("a copy of %s, which holds no pointer, was marked with %s", ty, o.Name)
	}
	if len(types) != 0 {
		t.Errorf("the pass reported %v as descriptors owed for a copy with no pointer in it", types)
	}
}

// A frame slot is the one destination specs/034 names as needing no barrier.
// The collector scans a stack by its maps, so a write to one is described
// already, and OpLocalAddr is the frame's own address form at this stage.
func TestBulkBarrierLeavesACopyIntoAFrameSlotAlone(t *testing.T) {
	ty := bbType("wide", 12)
	f, mv := bbMove(ty, OpLocalAddr)

	if _, err := BulkBarriers(f); err != nil {
		t.Fatalf("BulkBarriers: %v", err)
	}
	if o := BulkBarrierDescriptor(mv); o != nil {
		t.Errorf("a copy into a frame slot was marked with %s, which costs a call the collector does not need", o.Name)
	}
}

// The size is what says the type is the type being moved and not merely the
// type of the address. runtime.typedmemmove copies typ.Size_ bytes, so a type
// of the wrong size copies the wrong number of bytes, and reporting that is
// the alternative to a silent miscompilation.
func TestBulkBarrierRefusesACopyWhoseSizeDisagrees(t *testing.T) {
	ty := bbType("wide", 12)
	f, mv := bbMove(ty, OpArg)
	mv.AuxInt = ty.Size - 8

	_, err := BulkBarriers(f)
	if err == nil {
		t.Fatalf("a copy of %d bytes through an address of %s, which is %d, was accepted", mv.AuxInt, ty, ty.Size)
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("the error does not say the sizes disagree: %v", err)
	}
}

// An address whose type names nothing cannot say which words of what it points
// at hold pointers. Reporting it is the alternative to a copy with no barrier.
func TestBulkBarrierRefusesACopyThroughAnUntypedAddress(t *testing.T) {
	ty := bbType("wide", 12)
	f, mv := bbMove(ty, OpArg)
	mv.Args[0].Type = machinePtrType

	if _, err := BulkBarriers(f); err == nil {
		t.Fatalf("a copy through an address of %s was accepted:\n%s", machinePtrType, f)
	}
}

// Two copies of one type owe one descriptor. The linker resolves a descriptor
// by name, so a second is a second symbol for one type.
func TestBulkBarrierReportsEachTypeOnce(t *testing.T) {
	ty := bbType("wide", 12)
	f, mv := bbMove(ty, OpArg)
	second := mv.Block.NewValue(0, OpMove, MemType, mv.Args[0], mv.Args[1], mv)
	second.AuxInt = ty.Size
	raRet(mv.Block, second)

	types, err := BulkBarriers(f)
	if err != nil {
		t.Fatalf("BulkBarriers: %v", err)
	}
	if BulkBarrierDescriptor(second) == nil {
		t.Errorf("the second copy was not marked:\n%s", f)
	}
	if len(types) != 1 {
		t.Errorf("two copies of one type reported %d descriptors, want 1", len(types))
	}
}
