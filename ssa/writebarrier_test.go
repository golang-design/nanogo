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
