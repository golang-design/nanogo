// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import "testing"

// wbStore builds a function whose one block stores a pointer through a pointer
// the barrier cannot prove is a frame address, which is the shape
// specs/034-write-barriers.md requires a barrier around.
func wbStore(name string) (*Func, *Block, *Value) {
	f, e, mem := raFunc(name)
	dst := e.NewValue(0, OpArg, tIntPtr)
	val := e.NewValue(0, OpArg, tIntPtr)
	st := e.NewValue(0, OpARM64MOVDstore, MemType, dst, val, mem)
	raRet(e, st)
	return f, e, st
}

// The static base is the address a global's address is measured from. It is
// neither in the heap nor in a frame, and a function has one.
//
// Both halves of that carry weight, and the second is what this test is for.
// The barrier reads runtime.writeBarrier, so it needs a static base, and it
// used to make its own rather than reuse the one lowering had already put in
// the entry block. The two were spelled with different types: lowering's holds
// no pointer the collector may follow and the barrier's, taken from the type
// interface data is built with, holds one.
//
// The consequence is not a slower program. Register allocation gave the second
// static base a spill slot, ssa/liveness.go read the value's type and set that
// slot's bit in the locals bitmap, and runtime.adjustpointers reads exactly
// those words the next time a stack grows under the frame. It finds whatever
// the slot happened to hold, and the program stops with "bad pointer in frame"
// and no way back to the compiler that wrote the map. Go's own
// test/linkmain_run.go died that way on every run.
func TestWriteBarrierUsesOneStaticBaseAndDoesNotCallItAPointer(t *testing.T) {
	f, _, _ := wbStore("sb")
	WriteBarriers(f)

	var sbs []*Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpSB {
				sbs = append(sbs, v)
			}
		}
	}
	if len(sbs) == 0 {
		t.Fatalf("the barrier reads a global and the function has no static base:\n%s", f)
	}
	if len(sbs) != 1 {
		t.Errorf("the function has %d static bases and a function has one:\n%s", len(sbs), f)
	}
	for _, v := range sbs {
		if v.Type.HasPointers() {
			t.Errorf("v%d is the static base and its type %s holds a pointer, so its spill slot is marked in the locals bitmap and runtime.adjustpointers reads it",
				v.ID, v.Type)
		}
	}
}

// The barrier reuses the static base the function already has rather than
// adding a second one to the entry block.
func TestWriteBarrierReusesTheStaticBaseTheFunctionHas(t *testing.T) {
	f, e, _ := wbStore("reuse")
	sb := f.newValue(OpSB, machinePtrType, e.Pos)
	sb.Block = f.Entry
	f.Entry.Values = append([]*Value{sb}, f.Entry.Values...)

	WriteBarriers(f)

	n := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpSB {
				n++
				if v != sb {
					t.Errorf("the barrier made v%d rather than reusing the static base v%d the function had", v.ID, sb.ID)
				}
			}
		}
	}
	if n != 1 {
		t.Errorf("the function has %d static bases and it had one before the pass:\n%s", n, f)
	}
}

// The two machine-word pointer types differ in their pointer map alone, and
// the pointer map is the whole of what the collector reads. This is the
// assertion that they still differ in it, and that neither has drifted into
// the other: a machinePtrType that gained a pointer bit repeats the bug above,
// and an unsafePtrType that lost one frees live objects and says nothing.
func TestMachinePointerAndUnsafePointerDifferInTheirPointerMap(t *testing.T) {
	if machinePtrType.HasPointers() {
		t.Errorf("machinePtrType describes the stack pointer, the static base and a code address, and it holds a pointer")
	}
	if !unsafePtrType.HasPointers() {
		t.Errorf("unsafePtrType describes an interface's data word, and it holds no pointer")
	}
	if machinePtrType.Size != unsafePtrType.Size {
		t.Errorf("the two machine pointers are %d and %d bytes wide", machinePtrType.Size, unsafePtrType.Size)
	}
}

// Every value the pass inserts that the collector will be told about must be
// one it can follow. This walks what the barrier added and asserts that a
// pointer-typed value is one that can really hold a heap pointer: the value
// stored, the value overwritten, and the address they go through. Anything
// else the pass builds is an address of a static symbol or a count.
func TestWriteBarrierInsertsNoFalsePointer(t *testing.T) {
	f, _, _ := wbStore("false")
	before := map[ID]bool{}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			before[v.ID] = true
		}
	}
	WriteBarriers(f)

	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if before[v.ID] || !v.Type.HasPointers() {
				continue
			}
			switch v.Op {
			case OpARM64MOVDload, OpARM64ADD, OpARM64LSLconst:
				// The value overwritten, and the address of an indexed store.
				// Both are heap addresses and the collector must follow them.
			default:
				t.Errorf("the barrier inserted v%d = %v with type %s, which the collector is told holds a pointer",
					v.ID, v.Op, v.Type)
			}
		}
	}
}

// wbStoreAt is wbStore with a displacement on the store, which is the shape a
// field of a struct has: lowering folds the offset into the addressing mode
// and leaves the base register alone.
func wbStoreAt(name string, off int64) (*Func, *Block, *Value) {
	f, e, st := wbStore(name)
	st.AuxInt = off
	return f, e, st
}

// barrierOld returns the load the barrier reads the overwritten word with.
func barrierOld(t *testing.T, f *Func) *Value {
	t.Helper()
	var found []*Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpARM64MOVDload {
				found = append(found, v)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("the barrier reads the overwritten word once and %d loads were built:\n%s", len(found), f)
	}
	return found[0]
}

// The barrier must read the word the store is about to overwrite, and an
// address on this target is a register and a displacement together.
//
// The load used to take the register alone. Every store to a field past the
// first therefore recorded the wrong pair: the pointer being overwritten was
// never shaded, so an object reachable only through that field could be freed
// while the program still held it, and word 0 of the same object was shaded in
// its place, which hands the collector a word that need not be a pointer at
// all. Neither shows without the collector marking concurrently, which is why
// it took a compiler nanogo built, over input large enough to allocate through
// a mark phase, to reach it.
func TestWriteBarrierReadsTheWordTheStoreOverwrites(t *testing.T) {
	for _, off := range []int64{0, 8, 16, 4088} {
		f, _, st := wbStoreAt("off", off)
		WriteBarriers(f)
		old := barrierOld(t, f)
		if old.AuxInt != off {
			t.Errorf("the store writes %d(base) and the barrier reads %d(base), so it records a word the store does not overwrite:\n%s",
				off, old.AuxInt, f)
		}
		if old.Args[0] != st.Args[0] {
			t.Errorf("the barrier reads through v%d and the store writes through v%d", old.Args[0].ID, st.Args[0].ID)
		}
	}
}

// An indexed store carries no displacement: the address is the base and the
// index, which destination adds, so the load takes the sum and an offset of
// zero. A displacement copied onto one of these would read past the element.
func TestWriteBarrierIndexedStoreCarriesNoDisplacement(t *testing.T) {
	for _, op := range []Op{OpARM64MOVDstoreidx, OpARM64MOVDstoreidx8} {
		f, e, mem := raFunc("idx")
		base := e.NewValue(0, OpArg, tIntPtr)
		idx := e.NewValue(0, OpArg, tInt)
		val := e.NewValue(0, OpArg, tIntPtr)
		st := e.NewValue(0, op, MemType, base, idx, val, mem)
		raRet(e, st)
		WriteBarriers(f)

		old := barrierOld(t, f)
		if old.AuxInt != 0 {
			t.Errorf("%v addresses with a register pair and the barrier's load carries a displacement of %d:\n%s", op, old.AuxInt, f)
		}
		if old.Args[0].Op != OpARM64ADD {
			t.Errorf("%v: the barrier reads through v%d = %v and the address of an indexed store is a sum", op, old.Args[0].ID, old.Args[0].Op)
		}
	}
}
