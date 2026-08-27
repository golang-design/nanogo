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
// not fit in a value, so it is never read or written directly: every access
// goes through an OpLocalAddr that names the object in its Aux. Those are its
// uses, and the analysis is the same backward dataflow, with one addition:
//
//	taking the address of an object is a use of it.
//
// That is gc's rule, in liveness.valueEffects, and its comment says why the
// address counts as a read: whoever holds the pointer afterwards must see
// every word written before it, so the object is live from its definition up
// to the point the address was taken.
//
// Below that point the object is described by REACHABILITY rather than by the
// bitmap. The pointer that was taken is itself a value the analysis tracks, so
// the collector finds it in a slot or an argument, sees that it points into
// this frame, and looks the object up in the FUNCDATA_StackObjects table that
// stackmap.go writes. That is what makes the answer both precise and sound:
// gc frees the heap object stackobj.go's frame points at as soon as the callee
// that was given &s stops needing it, and nanogo now does the same.
//
// The table is therefore the precondition, and an object that is not in it
// gets no use analysis at all. FrameItem.StackObject is what says so, and an
// object it is false for is live for the whole function: nothing below its
// last use could find it, so nothing may assume it is dead there. That covers
// the object whose type has no descriptor and the object whose address never
// escaped, and it is what this analysis did for every object before.
//
// Two OpLocalAddr values for one object cannot be merged across a safepoint,
// which is what keeps a use from moving above a call it must cover. Every
// safepoint is a call, a call produces a new memory value, and OpLocalAddr
// takes memory, so two accesses separated by a call have different arguments
// and are different values.
//
// OpVarDef and OpVarKill from specs/025-lowering-and-rules.md bound a
// lifetime, and both clear the object walking backwards: nothing above an
// OpVarDef is read below it, and nothing below an OpVarKill reads what is
// above it. Nothing emits either marker today, so an object is live from the
// entry to its last use.
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

	// spillIn and spillOut are the analysis over the spill slots, objIn and
	// objOut the analysis over the frame objects. Both run backwards, both are
	// indexed by block identifier and both are over the tracked domain, so a
	// merged view is one union. They stay apart because their uses are found
	// differently: a spill slot by the values that live in it, an object by
	// the values that name it.
	spillIn, spillOut []bitmap
	objIn, objOut     []bitmap

	// objAlways holds the frame objects that are not in the stack objects
	// table. They are live everywhere, because nothing outside the bitmap can
	// describe them, and the merged view unions them in.
	objAlways bitmap

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
	// The objects nothing but the bitmap can describe. They are decided before
	// the analyses run and folded in after, so the dataflow itself stays the
	// answer to one question.
	lv.objAlways = lv.newSet()
	for i := range items {
		if items[i].Kind == ItemObject && !items[i].StackObject && lv.track[i] >= 0 {
			lv.objAlways.set(ID(lv.track[i]))
		}
	}

	lv.spillBackward(order, slotItem)
	lv.objBackward(order, objOf)
	lv.safepoints(slotItem, objOf)
	lv.conserve()
	return lv, nil
}

// conserve makes every object outside the stack objects table live everywhere.
//
// It runs after the dataflow rather than inside it, so that a lifetime marker
// cannot clear a bit the analysis is not entitled to clear.
func (lv *Liveness) conserve() {
	for _, set := range [][]bitmap{lv.objIn, lv.objOut, lv.live} {
		for _, b := range set {
			if b != nil {
				b.union(lv.objAlways)
			}
		}
	}
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

// objBackward is the backward use analysis over the frame objects.
//
// It is the same dataflow as spillBackward and it converges for the same
// reason. There is no phi case: a frame object is never a phi argument,
// because it is memory and not a value.
func (lv *Liveness) objBackward(order []*Block, objOf func(*ir.Object) int32) {
	f := lv.Func
	lv.objIn = make([]bitmap, f.NumBlocks())
	lv.objOut = make([]bitmap, f.NumBlocks())
	for _, b := range f.Blocks {
		lv.objIn[b.ID] = lv.newSet()
		lv.objOut[b.ID] = lv.newSet()
	}
	work := lv.newSet()
	for changed := true; changed; {
		changed = false
		for i := len(order) - 1; i >= 0; i-- {
			b := order[i]
			work.copyFrom(lv.objOut[b.ID])
			for _, s := range b.Succs {
				work.union(lv.objIn[s.ID])
			}
			lv.objOut[b.ID].union(work)
			work.copyFrom(lv.objOut[b.ID])
			lv.objTransfer(b, work, objOf)
			if lv.objIn[b.ID].union(work) {
				changed = true
			}
		}
	}
}

// objTransfer turns the live-out set of b into its live-in set.
func (lv *Liveness) objTransfer(b *Block, live bitmap, objOf func(*ir.Object) int32) {
	if b.Control != nil && !b.Control.dead {
		lv.objEffect(b.Control, live, objOf)
	}
	for i := len(b.Values) - 1; i >= 0; i-- {
		lv.objEffect(b.Values[i], live, objOf)
	}
}

// objEffect applies one value to a set that is being walked backwards.
//
// A value that names an object uses it, and a lifetime marker ends the range
// the values above it belong to. Every other value is not about this object:
// a load or a store reaches the object through the address, so the address is
// where the use is recorded and the memory operation itself carries no name.
func (lv *Liveness) objEffect(v *Value, live bitmap, objOf func(*ir.Object) int32) {
	t := objOf(auxObject(v))
	if t < 0 {
		return
	}
	switch v.Op {
	case OpVarDef, OpVarKill:
		live.clear(ID(t))
	default:
		live.set(ID(t))
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

	// The objects, in a second walk of the same shape. The set recorded at a
	// call is what is live after it, for the reason above: an operand that
	// dies at the call is held and described by the callee, and an object
	// whose address the call was given is reached through the pointer the
	// callee holds.
	objs := lv.newSet()
	for _, b := range f.Blocks {
		objs.copyFrom(lv.objOut[b.ID])
		if b.Control != nil && !b.Control.dead {
			lv.objEffect(b.Control, objs, objOf)
		}
		for i := len(b.Values) - 1; i >= 0; i-- {
			v := b.Values[i]
			if k := index[v.ID]; k >= 0 {
				lv.live[k].union(objs)
			}
			lv.objEffect(v, objs, objOf)
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
