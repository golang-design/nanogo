// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"fmt"

	"golang.design/x/nanogo/ir"
)

// The bulk half of the write barrier, per specs/034-write-barriers.md.
//
// ssa/writebarrier.go covers a store of one pointer. A copy of a value that
// holds several is not one store and is not several either: it is one OpMove,
// which lowering turns into a call, and the words it writes are pointers the
// collector must be told about just the same.
//
// Go's own runtime draws the same line and draws it in two places. A clear
// needs nothing here, because runtime.memclrHasPointers calls
// bulkBarrierPreWrite itself before it writes, and ssa/rules/arm64.go's
// lowerZero already picks it whenever the region can hold a pointer. A copy
// has no such form: runtime.memmove writes the words and records nothing.
// runtime.typedmemmove is memmove with the barrier in front of it, and this
// pass is what decides that a copy needs it.
//
// # What the gap looked like
//
// A struct of twelve pointers, copied between two heap locations while the
// collector marked, lost objects on every run of a probe under GOGC=1 with
// GODEBUG=gccheckmark=1. The scalar barrier does not see it: the copy never
// becomes a pointer store, so there is nothing for that pass to match. The
// first probe written for this hid it, because it cleared the source field by
// field and each of those nil stores carried the deletion half of the scalar
// barrier, which shaded the object being moved. Clearing the source with a
// struct assignment instead removed the accident and the loss was immediate.
//
// # Why the decision is here and the call is not
//
// This pass runs before lowering and decides only which copies need a barrier,
// recording the answer in the operation's Aux. lowerMove emits the call.
//
// The split is forced by what each stage knows. The moved type is known here
// and gone by the time the copy is a call, because a call carries a callee and
// not a value type. The type descriptor is also a symbol this object must
// define, and the driver collects those from the list this pass returns, which
// a rule in the target's package has no way to reach.
func BulkBarriers(f *Func) ([]*ir.Type, error) {
	if f == nil {
		return nil, nil
	}
	var types []*ir.Type
	seen := make(map[string]bool)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			t, ok, err := bulkBarrierType(f, v)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			o, err := ir.DescriptorObject(t)
			if err != nil {
				return nil, fmt.Errorf("ssa: bulk barrier: %s: a copy of %s needs a type descriptor: %w", f.Name, t, err)
			}
			v.Aux = o
			if !seen[o.Name] {
				seen[o.Name] = true
				types = append(types, t)
			}
		}
	}
	return types, nil
}

// bulkBarrierType returns the type a copy moves, and whether that copy needs a
// barrier.
//
// Three questions, and each removes work that the next would otherwise do.
//
// Is it a copy at all. Only OpMove writes several words from one address to
// another. OpZero is answered by runtime.memclrHasPointers and OpStore of one
// pointer by ssa/writebarrier.go.
//
// Can the destination hold a pointer the collector follows. A frame slot
// cannot: the collector scans a stack by its maps, and specs/034 names it as
// the one destination a barrier may be omitted for. OpLocalAddr is the frame's
// own address form at this stage, the way ADDframe is after lowering, so the
// answer is the operation and not an analysis. A global is not omitted, and
// specs/034's table says why: globals are scanned, but Go's barrier is hybrid
// and the deletion half is needed wherever the old value may be the last
// reference.
//
// Does the value being moved hold a pointer. This is the type's own map and
// nothing else, which is the rule specs/027 states for every such question.
//
// The error is not a refusal of a construct. It says the pass could not
// establish the type of a copy it can see, and the alternative to reporting it
// is a copy that silently gets no barrier, which is the defect this file
// exists to close.
func bulkBarrierType(f *Func, v *Value) (*ir.Type, bool, error) {
	if v.Op != OpMove || len(v.Args) != 3 {
		return nil, false, nil
	}
	dst := v.Args[0]
	if dst == nil {
		return nil, false, nil
	}
	if dst.Op == OpLocalAddr {
		return nil, false, nil
	}
	if dst.Type == nil || dst.Type.Elem == nil {
		return nil, false, fmt.Errorf("ssa: bulk barrier: %s: v%d copies %d bytes through an address of type %s, which names no type to describe them with",
			f.Name, v.ID, v.AuxInt, dst.Type)
	}
	t := dst.Type.Elem
	// The size is what says the type is the type being moved and not merely
	// the type of the address. runtime.typedmemmove copies typ.Size_ bytes and
	// reads typ.PtrBytes words of pointer map, so a type of the wrong size
	// would copy the wrong number of bytes.
	//
	// It is asked before the pointer map, and the order is the whole of the
	// check. "This type holds no pointer" is only an answer about the copy
	// once the type is known to be the type the copy moves. Asked first, it
	// returns "no barrier" for a pointer-free type of any size that merely
	// happens to be what the address is spelled with, and the copy loses its
	// barrier with nothing reported.
	if t.Size != v.AuxInt {
		return nil, false, fmt.Errorf("ssa: bulk barrier: %s: v%d copies %d bytes through an address of %s, which is %d bytes",
			f.Name, v.ID, v.AuxInt, t, t.Size)
	}
	// A type ir.Layout never ran on has no pointer map rather than an empty
	// one, and Type.HasPointers reads the two the same way. Layout's own
	// invariant is that a laid-out type has a non-zero alignment, so this is
	// the question that separates them. ssa/rules/arm64.go's hasPointers
	// guards the same query for a clear, for the same reason, and the answers
	// differ only in what each can do about it: a rule has no error to
	// return, and this pass does.
	if t.Align == 0 {
		return nil, false, fmt.Errorf("ssa: bulk barrier: %s: v%d copies %s, which ir.Layout never ran on, so it has no pointer map to read",
			f.Name, v.ID, t)
	}
	if !t.HasPointers() {
		return nil, false, nil
	}
	return t, true, nil
}

// BulkBarrierDescriptor returns the descriptor object BulkBarriers recorded on
// a copy, or nil if the copy needs no barrier.
//
// It exists so that ssa/rules reads the answer through a name rather than by
// asserting on Aux, which is what keeps the two halves of this from disagreeing
// about what an untyped field means.
func BulkBarrierDescriptor(v *Value) *ir.Object {
	if v == nil || v.Op != OpMove {
		return nil
	}
	o, _ := v.Aux.(*ir.Object)
	return o
}
