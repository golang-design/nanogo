// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// The analysis is tested on functions built by hand, over the invented target
// of regalloc_test.go.
//
// specs/027-liveness-and-stackmaps.md says why the tests are shaped this way:
// a wrong stack map does not fail a unit test, it fails when a collection
// happens while the wrong bit is set. So each test here asserts a property the
// spec states rather than a bitmap somebody read off a dump, and the loop
// tests exist because a fixed point and a single pass differ exactly there.

// lvAnalyse allocates f and runs the analysis over it.
func lvAnalyse(t *testing.T, f *Func) (*Alloc, []FrameItem, *Liveness) {
	t.Helper()
	a := raAllocate(t, f, raTarget())
	items, err := FrameItems(a)
	if err != nil {
		t.Fatalf("FrameItems: %v", err)
	}
	lv, err := ComputeLiveness(a, items)
	if err != nil {
		t.Fatalf("ComputeLiveness: %v", err)
	}
	return a, items, lv
}

// lvItemOf returns the item index of the slot v lives in, or -1.
func lvItemOf(a *Alloc, items []FrameItem, v *Value) int {
	h := a.Home[v.ID]
	if h.Kind != LocSlot {
		return -1
	}
	for i := range items {
		if items[i].Kind == ItemSpill && items[i].Index == h.Slot {
			return i
		}
	}
	return -1
}

// lvObjectItem returns the item index of a frame object.
func lvObjectItem(items []FrameItem, o *ir.Object) int {
	for i := range items {
		if items[i].Kind == ItemObject && items[i].Obj == o {
			return i
		}
	}
	return -1
}

func TestLivenessPointerAcrossACallIsLive(t *testing.T) {
	// The spike's live case, at the level of the analysis: a pointer held only
	// by the frame while a call runs. Losing it here frees the object.
	f, e, mem := raFunc("across")
	p := e.NewValue(0, OpArg, tIntPtr)
	m2 := raCall(e, mem)
	use := e.NewValue(0, OpLoad, tInt, p, m2)
	raRet(e, m2, use)

	a, items, lv := lvAnalyse(t, f)
	i := lvItemOf(a, items, p)
	if i < 0 {
		t.Fatalf("the pointer is live across a call and was not spilled:\n%s%s", f, a)
	}
	if !lv.Tracked(i) {
		t.Fatalf("the slot holds a pointer and is not tracked:\n%s", lv)
	}
	if lv.NumSafepoints() != 1 {
		t.Fatalf("the function has one call and %d safepoints", lv.NumSafepoints())
	}
	if !lv.LiveAt(0, i) {
		t.Errorf("the pointer is not live at the call:\n%s", lv)
	}
}

func TestLivenessNonPointerSlotIsNotTracked(t *testing.T) {
	// specs/027: the set is over pointer-typed stack slots. A non-pointer slot
	// cannot hold a reference, so it is outside the analysis altogether.
	f, e, mem := raFunc("int")
	n := e.NewValue(0, OpArg, tInt)
	m2 := raCall(e, mem)
	use := e.NewValue(0, OpAdd, tInt, n, n)
	raRet(e, m2, use)

	a, items, lv := lvAnalyse(t, f)
	i := lvItemOf(a, items, n)
	if i < 0 {
		t.Fatalf("the value is live across a call and was not spilled:\n%s%s", f, a)
	}
	if lv.Tracked(i) {
		t.Errorf("an int slot is tracked by the collector's liveness")
	}
	if lv.LiveAt(0, i) {
		t.Errorf("an int slot is live at a safepoint")
	}
}

// lvBranch returns a function whose entry branches to two blocks that join.
//
//	entry -> yes, no
//	yes   -> join
//	no    -> join
//
// The pointer is defined in the entry and used after a call in yes only, so it
// is live on one path and dead on the other.
func lvBranch(name string) (f *Func, entry, yes, no, join *Block, p *Value) {
	f, entry, mem := raFunc(name)
	entry.Kind = BlockIf
	entry.Control = entry.NewValue(0, OpConstBool, tBool)
	p = entry.NewValue(0, OpArg, tIntPtr)

	yes = f.NewBlock(BlockPlain)
	no = f.NewBlock(BlockPlain)
	join = f.NewBlock(BlockRet)
	entry.AddEdgeTo(yes)
	entry.AddEdgeTo(no)
	yes.AddEdgeTo(join)
	no.AddEdgeTo(join)

	m2 := raCall(yes, mem)
	yes.NewValue(0, OpLoad, tInt, p, m2)
	// The verifier of specs/021-ssa-construction.md wants one memory value
	// live at each point, so the join takes a memory phi.
	raRet(join, join.NewValue(0, OpPhi, MemType, m2, mem))
	return f, entry, yes, no, join, p
}

func TestLivenessIsAMayAnalysis(t *testing.T) {
	// specs/027: a slot live on any path is live. The union at a join is the
	// whole safety argument, so it is asserted directly: the slot is live on
	// the way out of the entry although only one successor needs it.
	f, entry, yes, no, _, p := lvBranch("may")
	a, items, lv := lvAnalyse(t, f)
	i := lvItemOf(a, items, p)
	if i < 0 {
		t.Fatalf("the pointer was not spilled:\n%s%s", f, a)
	}
	if !lv.LiveIn(yes.ID, i) {
		t.Errorf("the slot is used in b%d and is not live in there", yes.ID)
	}
	if lv.LiveIn(no.ID, i) {
		t.Errorf("the slot is not used in b%d and is live in there", no.ID)
	}
	if !lv.LiveOut(entry.ID, i) {
		t.Errorf("the slot is live on one successor of b%d and is dead on the way out of it:\n%s",
			entry.ID, lv)
	}
}

// lvLoop returns a function with a loop whose body uses a pointer that is
// defined before the loop.
//
//	entry  -> header
//	header -> body, exit
//	body   -> header
func lvLoop(name string) (f *Func, header, body *Block, p *Value) {
	f, entry, mem := raFunc(name)
	entry.Kind = BlockPlain
	p = entry.NewValue(0, OpArg, tIntPtr)

	header = f.NewBlock(BlockIf)
	body = f.NewBlock(BlockPlain)
	exit := f.NewBlock(BlockRet)
	entry.AddEdgeTo(header)
	header.AddEdgeTo(body)
	header.AddEdgeTo(exit)
	body.AddEdgeTo(header)
	// The memory phi comes first, because a phi has to be at the top of its
	// block, and its arguments follow the header's predecessor order.
	mphi := header.NewValue(0, OpPhi, MemType)
	header.Control = header.NewValue(0, OpConstBool, tBool)

	m2 := raCall(body, mphi)
	body.NewValue(0, OpLoad, tInt, p, m2)
	mphi.AddArg(mem)
	mphi.AddArg(m2)
	raRet(exit, mphi)
	return f, header, body, p
}

func TestLivenessLoopNeedsTheFixedPoint(t *testing.T) {
	// The value is used in the body and reaches it again around the back edge.
	// One backward walk in reverse of the reverse postorder visits the body
	// before the header, so it sees an empty live-in for the header and leaves
	// the body's live-out empty. Only the second round carries it around.
	f, header, body, p := lvLoop("loop")
	a, items, lv := lvAnalyse(t, f)
	i := lvItemOf(a, items, p)
	if i < 0 {
		t.Fatalf("the pointer was not spilled:\n%s%s", f, a)
	}
	if !lv.LiveOut(body.ID, i) {
		t.Errorf("the slot is live around the back edge and is dead on the way out of the body:\n%s", lv)
	}
	if !lv.LiveIn(header.ID, i) {
		t.Errorf("the slot is dead on entry to the loop header:\n%s", lv)
	}
	if !lv.LiveAt(0, i) {
		t.Errorf("the slot is dead at the call in the loop body:\n%s", lv)
	}
}

func TestLivenessPhiArgumentIsLiveAtTheEndOfItsPredecessor(t *testing.T) {
	// A phi argument is not a use in the phi's block. It is a use at the end of
	// the predecessor it arrives from, which is where the copy that realises
	// the phi runs. A slot read by that copy is live there, and calling it dead
	// would let the collector free what the copy is about to move.
	f, entry, mem := raFunc("phiarg")
	entry.Kind = BlockIf
	entry.Control = entry.NewValue(0, OpConstBool, tBool)
	p1 := entry.NewValue(0, OpArg, tIntPtr)
	p2 := entry.NewValue(0, OpArg, tIntPtr)

	yes := f.NewBlock(BlockPlain)
	no := f.NewBlock(BlockPlain)
	join := f.NewBlock(BlockRet)
	entry.AddEdgeTo(yes)
	entry.AddEdgeTo(no)
	yes.AddEdgeTo(join)
	no.AddEdgeTo(join)

	c1 := raCall(yes, mem)
	c2 := raCall(no, mem)
	mphi := join.NewValue(0, OpPhi, MemType, c1, c2)
	pphi := join.NewValue(0, OpPhi, tIntPtr, p1, p2)
	join.NewValue(0, OpLoad, tInt, pphi, mphi)
	raRet(join, mphi)

	a, items, lv := lvAnalyse(t, f)
	i1, i2 := lvItemOf(a, items, p1), lvItemOf(a, items, p2)
	if i1 < 0 || i2 < 0 {
		t.Fatalf("the phi arguments span a call and were not spilled:\n%s%s", f, a)
	}
	if !lv.LiveOut(yes.ID, i1) {
		t.Errorf("the argument of the phi is dead at the end of the block it arrives from:\n%s", lv)
	}
	if lv.LiveOut(yes.ID, i2) {
		t.Errorf("the argument that arrives on the other edge is live here:\n%s", lv)
	}
	if !lv.LiveAt(0, i1) {
		t.Errorf("the argument is dead at the call it spans:\n%s", lv)
	}
}

func TestLivenessCallResultIsNotLiveAcrossItsOwnCall(t *testing.T) {
	// The call writes the slot, so nothing the slot held before survives it
	// and the result itself does not exist yet. The allocator makes the same
	// decision, and a disagreement would make the two describe one slot
	// differently.
	f, e, mem := raFunc("result")
	m2 := raCall(e, mem)
	r := e.NewValue(0, OpSelectN, tIntPtr, m2)
	m3 := raCall(e, m2)
	use := e.NewValue(0, OpLoad, tInt, r, m3)
	raRet(e, m3, use)

	a, items, lv := lvAnalyse(t, f)
	i := lvItemOf(a, items, r)
	if i < 0 {
		t.Fatalf("the result is live across the second call and was not spilled:\n%s%s", f, a)
	}
	if lv.LiveAt(0, i) {
		t.Errorf("the slot of a call result is live at the call that writes it:\n%s", lv)
	}
	if !lv.LiveAt(1, i) {
		t.Errorf("the slot is not live at the call it spans:\n%s", lv)
	}
}

// lvObjectFunc returns a function with one frame object and two calls, and the
// values that mark the object's lifetime if markers is true.
func lvObjectFunc(name string, markers bool) (*Func, *ir.Object) {
	f, e, mem := raFunc(name)
	o := obj("box", tIntPtr, ir.ClassLocal)
	o.Addrtaken = true
	f.Frame = []*ir.Object{o}

	m := mem
	if markers {
		d := e.NewValue(0, OpVarDef, MemType, m)
		d.Aux = o
		m = d
	}
	m = raCall(e, m)
	if markers {
		k := e.NewValue(0, OpVarKill, MemType, m)
		k.Aux = o
		m = k
	}
	m = raCall(e, m)
	raRet(e, m)
	return f, o
}

func TestLivenessObjectWithoutMarkersIsLiveThroughout(t *testing.T) {
	// Nothing emits the markers yet, so this is the path every frame object
	// takes today. The conservative answer is the whole function: an object
	// whose address escaped can be reached from anywhere, and claiming it is
	// dead frees it.
	f, o := lvObjectFunc("noMarkers", false)
	_, items, lv := lvAnalyse(t, f)
	i := lvObjectItem(items, o)
	if i < 0 {
		t.Fatalf("the frame object has no item: %v", items)
	}
	if lv.NumSafepoints() != 2 {
		t.Fatalf("the function has two calls and %d safepoints", lv.NumSafepoints())
	}
	for k := 0; k < lv.NumSafepoints(); k++ {
		if !lv.LiveAt(k, i) {
			t.Errorf("the object is dead at safepoint %d:\n%s", k, lv)
		}
	}
}

func TestLivenessObjectMarkersBoundTheLifetime(t *testing.T) {
	// specs/027: VarDef and VarKill mark the lifetime bounds, so that a slot
	// reused by two objects with disjoint lifetimes is described correctly at
	// each point. The kill is what makes the second half of the function
	// describable at all.
	f, o := lvObjectFunc("markers", true)
	_, items, lv := lvAnalyse(t, f)
	i := lvObjectItem(items, o)
	if i < 0 {
		t.Fatalf("the frame object has no item")
	}
	if !lv.LiveAt(0, i) {
		t.Errorf("the object is dead at the call inside its lifetime:\n%s", lv)
	}
	if lv.LiveAt(1, i) {
		t.Errorf("the object is live at the call after its VarKill:\n%s", lv)
	}
}

func TestLivenessObjectLifetimeSpansBlocks(t *testing.T) {
	// The shape a lowering pass emits: the marker that opens the lifetime and
	// the one that closes it are in different blocks, with the calls that need
	// the object described in between. A single-block test would pass with a
	// transfer function that never looked at the block boundary.
	f, entry, mem := raFunc("objSpan")
	entry.Kind = BlockPlain
	o := obj("box", tIntPtr, ir.ClassLocal)
	o.Addrtaken = true
	f.Frame = []*ir.Object{o}

	mid := f.NewBlock(BlockPlain)
	tail := f.NewBlock(BlockRet)
	entry.AddEdgeTo(mid)
	mid.AddEdgeTo(tail)

	d := entry.NewValue(0, OpVarDef, MemType, mem)
	d.Aux = o
	m1 := raCall(mid, d)
	k := tail.NewValue(0, OpVarKill, MemType, m1)
	k.Aux = o
	m2 := raCall(tail, k)
	raRet(tail, m2)

	_, items, lv := lvAnalyse(t, f)
	i := lvObjectItem(items, o)
	if i < 0 {
		t.Fatalf("the frame object has no item")
	}
	if lv.NumSafepoints() != 2 {
		t.Fatalf("%d safepoints, want two", lv.NumSafepoints())
	}
	if !lv.LiveOut(entry.ID, i) {
		t.Errorf("the object is dead on the way out of the block that defines it:\n%s", lv)
	}
	if !lv.LiveAt(0, i) {
		t.Errorf("the object is dead at a call inside its lifetime, in another block:\n%s", lv)
	}
	if lv.LiveAt(1, i) {
		t.Errorf("the object is live at the call after the VarKill in its own block:\n%s", lv)
	}
	// The object is dead before its definition, which is what lets a later
	// object take the same words.
	if lv.LiveIn(entry.ID, i) {
		t.Errorf("the object is live before its VarDef:\n%s", lv)
	}
}

func TestLivenessObjectLifetimeIsAMayAnalysisToo(t *testing.T) {
	// The kill is on one path only. The other path reaches the join with the
	// object still live, and the union at the join keeps it live, which is the
	// same direction the backward analysis errs in.
	f, entry, mem := raFunc("objJoin")
	entry.Kind = BlockIf
	entry.Control = entry.NewValue(0, OpConstBool, tBool)
	o := obj("box", tIntPtr, ir.ClassLocal)
	o.Addrtaken = true
	f.Frame = []*ir.Object{o}

	yes := f.NewBlock(BlockPlain)
	no := f.NewBlock(BlockPlain)
	join := f.NewBlock(BlockRet)
	entry.AddEdgeTo(yes)
	entry.AddEdgeTo(no)
	yes.AddEdgeTo(join)
	no.AddEdgeTo(join)

	k := yes.NewValue(0, OpVarKill, MemType, mem)
	k.Aux = o
	phi := join.NewValue(0, OpPhi, MemType, k, mem)
	m := raCall(join, phi)
	raRet(join, m)

	_, items, lv := lvAnalyse(t, f)
	i := lvObjectItem(items, o)
	if i < 0 {
		t.Fatalf("the frame object has no item")
	}
	if lv.LiveOut(yes.ID, i) {
		t.Errorf("the object is live after its VarKill on the path that kills it:\n%s", lv)
	}
	if !lv.LiveAt(0, i) {
		t.Errorf("the object reaches the join alive on one path and is dead at the call there:\n%s", lv)
	}
}

func TestLivenessTracksAWideSlotByItsPointerWords(t *testing.T) {
	// A string is two words and only the first holds a pointer. The slot is
	// tracked because it holds a pointer, and stackmap_test.go asserts that
	// only word zero gets a bit.
	f, e, mem := raFunc("wide")
	s := e.NewValue(0, OpArg, tString)
	m2 := raCall(e, mem)
	e.NewValue(0, OpLoad, tInt, s, m2)
	raRet(e, m2)

	a, items, lv := lvAnalyse(t, f)
	i := lvItemOf(a, items, s)
	if i < 0 {
		t.Fatalf("a string does not fit a register and was not given a slot:\n%s%s", f, a)
	}
	if !lv.Tracked(i) {
		t.Errorf("a string slot is not tracked")
	}
	if !lv.LiveAt(0, i) {
		t.Errorf("the string is dead at the call it spans:\n%s", lv)
	}
}

func TestLivenessRejectsBadInput(t *testing.T) {
	if _, err := ComputeLiveness(nil, nil); err == nil {
		t.Error("a nil allocation was accepted")
	}
	f, e, mem := raFunc("nocall")
	raRet(e, mem)
	a := raAllocate(t, f, raTarget())
	items, err := FrameItems(a)
	if err != nil {
		t.Fatalf("FrameItems: %v", err)
	}
	broken := *a
	broken.Target = &Target{Name: "no IsCall"}
	if _, err := ComputeLiveness(&broken, items); err == nil {
		t.Error("a target with no IsCall was accepted")
	} else if !strings.Contains(err.Error(), "IsCall") {
		t.Errorf("the error does not name the missing hook: %v", err)
	}
	if _, err := ComputeLiveness(a, []FrameItem{{Kind: ItemSpill, Index: 7, Type: tIntPtr}}); err == nil {
		t.Error("an item naming a slot that does not exist was accepted")
	}
	if _, err := ComputeLiveness(a, []FrameItem{{Kind: ItemObject, Index: 7, Type: tIntPtr}}); err == nil {
		t.Error("an item naming a frame object that does not exist was accepted")
	}
}

func TestLivenessIsDeterministic(t *testing.T) {
	// specs/053-determinism.md: two runs over one function produce one dump.
	// The analysis reads Alloc.Home and Func.Frame by index and no map, which
	// is what makes that true rather than likely.
	first := ""
	for i := 0; i < 4; i++ {
		f, _, _, p := lvLoop("determinism")
		a, items, lv := lvAnalyse(t, f)
		if lvItemOf(a, items, p) < 0 {
			t.Fatal("the pointer was not spilled")
		}
		got := lv.String()
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d differs\ngot:\n%s\nwant:\n%s", i, got, first)
		}
	}
	if !strings.Contains(first, "safepoints") {
		t.Errorf("the dump does not name the safepoints:\n%s", first)
	}
}
