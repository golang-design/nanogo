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

// The order of the two questions is the whole of the check.
//
// "This type holds no pointer" is an answer about the copy only once the type
// is known to be the type the copy moves, and the size is what knows that. A
// pass that asks the pointer map first returns "no barrier" for a pointer-free
// type that merely happens to be what the destination address is spelled with,
// and the copy then loses its barrier with nothing reported. The error is not
// the point: the point is that the silent answer is withdrawn.
func TestBulkBarrierRefusesAPointerFreeCopyWhoseSizeDisagrees(t *testing.T) {
	ty := mkType(&ir.Type{Kind: ir.Array, Elem: tInt, Len: 12})
	f, mv := bbMove(ty, OpArg)
	mv.AuxInt = ty.Size + 8

	_, err := BulkBarriers(f)
	if err == nil {
		t.Fatalf("a copy of %d bytes through an address of %s, which is %d bytes, was skipped in silence rather than reported",
			mv.AuxInt, ty, ty.Size)
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("the error does not say the sizes disagree: %v", err)
	}
}

// A type ir.Layout never ran on has no pointer map, and that is not the same
// as having an empty one. ir.Type.HasPointers reads the two the same way, so
// asking it about such a type answers "holds no pointer" about a type whose
// contents are unknown, and a copy of it loses its barrier.
//
// Layout's own invariant is that a laid-out type has a non-zero alignment,
// which is the question that separates the two. ssa/rules/arm64.go's
// hasPointers guards the same query for a clear and names the same reason.
func TestBulkBarrierRefusesACopyOfATypeLayoutNeverRanOn(t *testing.T) {
	// Built by hand rather than through mkType, because running Layout is
	// exactly what this type has not had done to it: Size is set so that the
	// copy's byte count agrees, Align is zero and PtrBits is absent.
	ty := &ir.Type{Kind: ir.Struct, Name: "unlaid", PkgPath: "main", Size: 16}
	f, e, mem := raFunc("move")
	pt := &ir.Type{Kind: ir.Ptr, Elem: ty, Size: ir.PtrSize, Align: ir.PtrSize}
	dst := e.NewValue(0, OpArg, pt)
	src := e.NewValue(0, OpArg, pt)
	mv := e.NewValue(0, OpMove, MemType, dst, src, mem)
	mv.AuxInt = ty.Size
	raRet(e, mv)

	_, err := BulkBarriers(f)
	if err == nil {
		t.Fatalf("a copy of %s, which ir.Layout never ran on, was read as holding no pointer and skipped in silence", ty)
	}
	if !strings.Contains(err.Error(), "pointer map") {
		t.Errorf("the error does not say the type has no pointer map to read: %v", err)
	}
	if o := BulkBarrierDescriptor(mv); o != nil {
		t.Errorf("the copy was marked with %s despite the type having no pointer map", o.Name)
	}
}
