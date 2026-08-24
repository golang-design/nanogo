// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

// The liveness analysis of specs/027-liveness-and-stackmaps.md.
//
// The question is: at each safepoint, which words of the frame hold a pointer
// the collector must follow, and which words does a stack copy have to
// rewrite. Two properties answer it and they are not the same property.
//
//   - Whether a word holds a pointer is EXACT. It comes from ir.Type.PtrBits
//     and from nothing else. A word wrongly called a pointer is followed by the
//     collector and, worse, is rewritten when specs/035-goroutines-and-stack-growth.md
//     copies the stack, which turns an integer into a wrong integer with no
//     diagnostic. spikes/stackmap's bogusmap is that mistake made deliberately.
//   - Whether a slot is live is a MAY-analysis. A slot live on any path is
//     live. Erring towards live keeps a dead object alive, which wastes
//     memory. Erring the other way frees a live object, which corrupts it.
//
// This file computes the second. stackmap.go applies the first to it.
//
// # Two analyses, because there are two kinds of item
//
// A spill slot holds a value the compiler created and whose every use it can
// see. Liveness over it is the backward dataflow the spec states,
//
//	live_out(b) = union over s in succ(b) of live_in(s)
//	live_in(b)  = gen(b) union (live_out(b) minus kill(b))
//
// iterated to a fixed point over blocks in reverse postorder. The lattice is
// the finite powerset of the tracked items and the transfer functions are
// monotone, so it converges, and a loop needs the fixed point rather than one
// pass: a slot used at the top of a loop body and defined at the bottom is
// live around the back edge, which a single reverse walk does not see.
//
// An object in Func.Frame is different. Its address is taken, or its type does
// not fit in a value, so a use of it can be a load through a pointer that
// arrived from anywhere. The compiler cannot enumerate its uses, so a backward
// analysis over them would be a lie. What bounds it instead is the lifetime
// markers of specs/025-lowering-and-rules.md: the object occupies its slot
// from OpVarDef to OpVarKill. That is a FORWARD may-analysis,
//
//	in(b)  = union over p in pred(b) of out(p)
//	out(b) = (in(b) union def(b)) minus kill(b)
//
// with in(entry) holding every object that has no OpVarDef anywhere, which is
// the conservative default for an object whose lifetime nothing marked: it is
// live for the whole function. Two objects that share a slot with disjoint
// lifetimes are then described correctly at each point, which is what the spec
// asks for, and a path that misses the OpVarKill keeps the object live, which
// is the may-direction.
//
// # Why not the allocator's liveness
//
// specs/026-register-allocation.md computes a liveness of its own, over
// values. It is a different analysis over a different domain: it drops
// rematerialised values, and Rematerialisable includes OpLocalAddr, which is a
// frame address. It is also not kept: Alloc carries no safepoint. This file
// therefore recomputes over slots, from Alloc.Home, and finds the safepoints
// again through Target.IsCall.
//
// # Why the kill of a spill slot is sound
//
// A definition into a slot kills it: whatever the slot held before is dead
// there. That is only true because specs/026-register-allocation.md gives two
// values one slot only when their live ranges are disjoint and the ranges have
// no holes, so no other value in the slot can be live across the definition.
// The argument lives in regalloc.go, in canShareSlot, and this file depends on
// it.

import (
	"fmt"
	"strings"

	"golang.design/x/nanogo/ir"
)

// Safepoint is a point where the goroutine can stop and the collector can read
// the frame.
//
// Every call is one, because the callee can allocate and start a collection.
// specs/027-liveness-and-stackmaps.md's other case, asynchronous preemption,
// needs no map: the runtime scans an asynchronously preempted frame
// conservatively, and the compiler's obligation there is to mark the ranges
// where even that is unsafe. See stackmap.go.
type Safepoint struct {
	Value ID // the call
	Block ID
}

// Liveness is the result of the analysis over one allocated function.
type Liveness struct {
	Alloc *Alloc
	Func  *Func
	Items []FrameItem

	// track maps an item index to its position in the bitsets, or -1 when the
	// item holds no pointer. specs/027 restricts the domain to pointer-holding
	// slots: a slot that cannot hold a reference does not concern the
	// collector, and leaving it out keeps the sets small. n is the size of
	// that domain.
	track []int32
	n     int

	// spillIn and spillOut are the backward analysis over the spill slots,
	// objIn and objOut the forward analysis over the frame objects. Both are
	// indexed by block identifier and both are over the tracked domain, so a
	// merged view is one union.
	spillIn, spillOut []bitmap
	objIn, objOut     []bitmap

	Safepoints []Safepoint
	live       []bitmap // by safepoint index
}

// ComputeLiveness runs the analysis over the items of an allocated function.
//
// items comes from FrameItems and is indexed the same way everywhere: the
// liveness, the layout and the maps all name an item by its position.
func ComputeLiveness(a *Alloc, items []FrameItem) (*Liveness, error) {
	if a == nil || a.Func == nil || a.Target == nil {
		return nil, fmt.Errorf("ssa: liveness: nil allocation")
	}
	if a.Target.IsCall == nil {
		return nil, fmt.Errorf("ssa: liveness: %s: the target has no IsCall, so no safepoint can be found", a.Func.Name)
	}
	f := a.Func
	lv := &Liveness{Alloc: a, Func: f, Items: items}
	lv.track = make([]int32, len(items))
	for i := range items {
		lv.track[i] = -1
		if items[i].HasPointers() {
			lv.track[i] = int32(lv.n)
			lv.n++
		}
	}

	// slotItem maps an Alloc.Slots index to a tracked position, and objItem
	// does the same for a Func.Frame index. Slices, not maps: the identifiers
	// are dense and specs/053-determinism.md keeps maps off this path.
	slotItem := make([]int32, len(a.Slots))
	for i := range slotItem {
		slotItem[i] = -1
	}
	objItem := make([]int32, len(f.Frame))
	for i := range objItem {
		objItem[i] = -1
	}
	for i := range items {
		t := lv.track[i]
		if t < 0 {
			continue
		}
		switch items[i].Kind {
		case ItemSpill:
			if items[i].Index < 0 || int(items[i].Index) >= len(slotItem) {
				return nil, fmt.Errorf("ssa: liveness: %s: item %d names slot %d, and there are %d",
					f.Name, i, items[i].Index, len(slotItem))
			}
			slotItem[items[i].Index] = t
		case ItemObject:
			if items[i].Index < 0 || int(items[i].Index) >= len(objItem) {
				return nil, fmt.Errorf("ssa: liveness: %s: item %d names frame object %d, and there are %d",
					f.Name, i, items[i].Index, len(objItem))
			}
			objItem[items[i].Index] = t
		}
	}
	// objOf finds the tracked position of an object named by a lifetime
	// marker. Func.Frame is a handful of entries, so the scan costs less than
	// the map it replaces, and it keeps the pass free of one.
	objOf := func(o *ir.Object) int32 {
		if o == nil {
			return -1
		}
		for i, x := range f.Frame {
			if x == o {
				return objItem[i]
			}
		}
		return -1
	}

	order := Dominators(f).ReversePostorder()
	if len(order) != len(f.Blocks) {
		// An unreachable block is not in the order, so its sets would stay
		// empty and a safepoint inside it would be described as holding
		// nothing. Allocate refuses such a function; this is the assertion
		// that it did.
		return nil, fmt.Errorf("ssa: liveness: %s: %d of %d blocks are reachable, run Verify before allocating",
			f.Name, len(order), len(f.Blocks))
	}
	lv.spillBackward(order, slotItem)
	lv.objForward(order, objOf)
	lv.safepoints(slotItem, objOf)
	return lv, nil
}

// newSet returns an empty set over the tracked domain.
//
// One word even when nothing is tracked, so that a union or a copy of two
// empty sets is a normal operation rather than a special case.
func (lv *Liveness) newSet() bitmap {
	n := lv.n
	if n == 0 {
		n = 1
	}
	return newBitmap(n)
}

// slotOf returns the tracked position of the slot a value lives in, or -1.
func (lv *Liveness) slotOf(slotItem []int32, id ID) int32 {
	if id < 0 || int(id) >= len(lv.Alloc.Home) {
		return -1
	}
	h := lv.Alloc.Home[id]
	if h.Kind != LocSlot || h.Slot < 0 || int(h.Slot) >= len(slotItem) {
		return -1
	}
	return slotItem[h.Slot]
}

// spillBackward is the backward dataflow of the spec, over the spill slots.
func (lv *Liveness) spillBackward(order []*Block, slotItem []int32) {
	f := lv.Func
	lv.spillIn = make([]bitmap, f.NumBlocks())
	lv.spillOut = make([]bitmap, f.NumBlocks())
	for _, b := range f.Blocks {
		lv.spillIn[b.ID] = lv.newSet()
		lv.spillOut[b.ID] = lv.newSet()
	}
	work := lv.newSet()
	// The sets only grow, so the fixed point is reached when no live-in set
	// grew in a whole round. Reverse of the reverse postorder visits a block
	// after its successors wherever the graph allows; a back edge still needs
	// a second round, which is why this is a loop and not a walk.
	for changed := true; changed; {
		changed = false
		for i := len(order) - 1; i >= 0; i-- {
			b := order[i]
			work.copyFrom(lv.spillOut[b.ID])
			lv.spillLiveOut(b, work, slotItem)
			lv.spillOut[b.ID].union(work)
			work.copyFrom(lv.spillOut[b.ID])
			lv.spillTransfer(b, work, slotItem)
			if lv.spillIn[b.ID].union(work) {
				changed = true
			}
		}
	}
}

// spillLiveOut adds what leaves b alive: what its successors need, and the phi
// arguments this edge carries.
//
// A phi argument is not a use in the phi's block. It is a use at the end of
// the predecessor it arrives from, which is where the copy that realises the
// phi runs, and a slot read by that copy is live there.
func (lv *Liveness) spillLiveOut(b *Block, live bitmap, slotItem []int32) {
	for i, s := range b.Succs {
		live.union(lv.spillIn[s.ID])
		j := predIndex(b, i)
		if j < 0 {
			continue
		}
		for _, v := range s.Values {
			if v.Op != OpPhi {
				break
			}
			if j >= len(v.Args) {
				continue
			}
			if a := v.Args[j]; a != nil {
				if t := lv.slotOf(slotItem, a.ID); t >= 0 {
					live.set(ID(t))
				}
			}
		}
	}
}

// spillTransfer turns the live-out set of b into its live-in set.
func (lv *Liveness) spillTransfer(b *Block, live bitmap, slotItem []int32) {
	if b.Control != nil && !b.Control.dead {
		if t := lv.slotOf(slotItem, b.Control.ID); t >= 0 {
			live.set(ID(t))
		}
	}
	for i := len(b.Values) - 1; i >= 0; i-- {
		v := b.Values[i]
		if t := lv.slotOf(slotItem, v.ID); t >= 0 {
			live.clear(ID(t))
		}
		if v.Op == OpPhi {
			continue
		}
		for _, a := range v.Args {
			if a == nil {
				continue
			}
			if t := lv.slotOf(slotItem, a.ID); t >= 0 {
				live.set(ID(t))
			}
		}
	}
}

// objForward is the forward lifetime analysis over the frame objects.
func (lv *Liveness) objForward(order []*Block, objOf func(*ir.Object) int32) {
	f := lv.Func
	lv.objIn = make([]bitmap, f.NumBlocks())
	lv.objOut = make([]bitmap, f.NumBlocks())
	for _, b := range f.Blocks {
		lv.objIn[b.ID] = lv.newSet()
		lv.objOut[b.ID] = lv.newSet()
	}

	// An object with no OpVarDef anywhere is live from the entry. Nothing
	// marked its lifetime, so the whole function is its lifetime, which is the
	// conservative answer and the one every object gets until
	// specs/025-lowering-and-rules.md emits the markers.
	defined := make([]bool, len(f.Frame))
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpVarDef {
				continue
			}
			for i, o := range f.Frame {
				if aux, _ := v.Aux.(*ir.Object); aux == o {
					defined[i] = true
				}
			}
		}
	}
	entry := lv.newSet()
	for i, o := range f.Frame {
		if defined[i] {
			continue
		}
		if t := objOf(o); t >= 0 {
			entry.set(ID(t))
		}
	}

	work := lv.newSet()
	for changed := true; changed; {
		changed = false
		for _, b := range order {
			clearAll(work)
			if b == f.Entry {
				work.union(entry)
			}
			for _, p := range b.Preds {
				work.union(lv.objOut[p.ID])
			}
			lv.objIn[b.ID].union(work)
			work.copyFrom(lv.objIn[b.ID])
			lv.objTransfer(b, work, objOf)
			if lv.objOut[b.ID].union(work) {
				changed = true
			}
		}
	}
}

// objTransfer runs the lifetime markers of b over a set.
func (lv *Liveness) objTransfer(b *Block, live bitmap, objOf func(*ir.Object) int32) {
	for _, v := range b.Values {
		switch v.Op {
		case OpVarDef:
			if t := objOf(auxObject(v)); t >= 0 {
				live.set(ID(t))
			}
		case OpVarKill:
			if t := objOf(auxObject(v)); t >= 0 {
				live.clear(ID(t))
			}
		}
	}
}

// clearAll empties a set.
func clearAll(b bitmap) {
	for i := range b {
		b[i] = 0
	}
}

// auxObject returns the object a value names, or nil.
func auxObject(v *Value) *ir.Object {
	o, _ := v.Aux.(*ir.Object)
	return o
}

// safepoints finds the calls and the set live at each one.
//
// The set at a call is what is live after it, minus the call's own result,
// which is exactly what has to be in the frame: Go's ABI destroys every
// register at a call (specs/026-register-allocation.md), so nothing survives
// anywhere else. An operand of the call that dies at it is not in the set,
// because the callee holds it and describes it in its own maps.
func (lv *Liveness) safepoints(slotItem []int32, objOf func(*ir.Object) int32) {
	f := lv.Func
	isCall := lv.Alloc.Target.IsCall
	// Blocks in layout order, so the safepoints come out in the order the code
	// generator emits them and a reader of the dump sees the function.
	index := make([]int32, f.NumValues())
	for i := range index {
		index[i] = -1
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !isCall(v) {
				continue
			}
			index[v.ID] = int32(len(lv.Safepoints))
			lv.Safepoints = append(lv.Safepoints, Safepoint{Value: v.ID, Block: b.ID})
			lv.live = append(lv.live, lv.newSet())
		}
	}

	live := lv.newSet()
	for _, b := range f.Blocks {
		// Backwards through the block, so live is the set live after the value
		// being visited.
		live.copyFrom(lv.spillOut[b.ID])
		if b.Control != nil && !b.Control.dead {
			if t := lv.slotOf(slotItem, b.Control.ID); t >= 0 {
				live.set(ID(t))
			}
		}
		for i := len(b.Values) - 1; i >= 0; i-- {
			v := b.Values[i]
			if k := index[v.ID]; k >= 0 {
				lv.live[k].union(live)
				// The call writes its own result, so the slot it lands in
				// holds nothing across the call.
				if t := lv.slotOf(slotItem, v.ID); t >= 0 {
					lv.live[k].clear(ID(t))
				}
			}
			if t := lv.slotOf(slotItem, v.ID); t >= 0 {
				live.clear(ID(t))
			}
			if v.Op == OpPhi {
				continue
			}
			for _, a := range v.Args {
				if a == nil {
					continue
				}
				if t := lv.slotOf(slotItem, a.ID); t >= 0 {
					live.set(ID(t))
				}
			}
		}
	}

	// Forwards through the block for the objects, whose lifetime runs the
	// other way.
	objs := lv.newSet()
	for _, b := range f.Blocks {
		objs.copyFrom(lv.objIn[b.ID])
		for _, v := range b.Values {
			switch v.Op {
			case OpVarDef:
				if t := objOf(auxObject(v)); t >= 0 {
					objs.set(ID(t))
				}
			case OpVarKill:
				if t := objOf(auxObject(v)); t >= 0 {
					objs.clear(ID(t))
				}
			}
			if k := index[v.ID]; k >= 0 {
				lv.live[k].union(objs)
			}
		}
	}
}

// NumSafepoints returns how many safepoints the function has.
func (lv *Liveness) NumSafepoints() int { return len(lv.Safepoints) }

// LiveAt reports whether item i is live at safepoint k.
func (lv *Liveness) LiveAt(k, i int) bool {
	if k < 0 || k >= len(lv.live) {
		return false
	}
	return lv.has(lv.live[k], i)
}

// LiveIn reports whether item i is live on entry to block b, and LiveOut on
// exit from it. The two are the merged view of the two analyses.
func (lv *Liveness) LiveIn(b ID, i int) bool {
	return lv.hasAt(lv.spillIn, b, i) || lv.hasAt(lv.objIn, b, i)
}

// LiveOut reports whether item i is live on exit from block b.
func (lv *Liveness) LiveOut(b ID, i int) bool {
	return lv.hasAt(lv.spillOut, b, i) || lv.hasAt(lv.objOut, b, i)
}

// Tracked reports whether item i is in the domain of the analysis. An item
// that holds no pointer is not.
func (lv *Liveness) Tracked(i int) bool {
	return i >= 0 && i < len(lv.track) && lv.track[i] >= 0
}

func (lv *Liveness) hasAt(sets []bitmap, b ID, i int) bool {
	if b < 0 || int(b) >= len(sets) || sets[b] == nil {
		return false
	}
	return lv.has(sets[b], i)
}

func (lv *Liveness) has(s bitmap, i int) bool {
	if i < 0 || i >= len(lv.track) {
		return false
	}
	t := lv.track[i]
	if t < 0 {
		return false
	}
	return s.has(ID(t))
}

// String returns a dump of the analysis, safepoint by safepoint.
//
// The order is the layout order of the blocks and the item order of the
// frame. Nothing derives from a map or an address, so two runs over one
// function print the same bytes, which is what specs/053-determinism.md's test
// compares.
func (lv *Liveness) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "liveness %s: %d items, %d tracked, %d safepoints\n",
		lv.Func.Name, len(lv.Items), lv.n, len(lv.Safepoints))
	for k, sp := range lv.Safepoints {
		fmt.Fprintf(&b, "  v%d in b%d:", sp.Value, sp.Block)
		for i := range lv.Items {
			if lv.LiveAt(k, i) {
				fmt.Fprintf(&b, " %s", lv.Items[i].Name)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
