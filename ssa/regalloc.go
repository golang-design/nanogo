// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

// The register allocator of specs/026-register-allocation.md.
//
// Linear scan over a reverse postorder linearisation. Graph colouring would
// allocate better and needs an interference graph, a coalescing phase and a
// spill-cost model; specs/000-decisions.md decision 10 is the budget that
// rejects it.
//
// # The simplification Go's ABI provides
//
// Go's internal ABI has no callee-saved registers. Every register is destroyed
// by every call. Two consequences are implemented here rather than discovered:
//
//  1. A value live across a call is spilled. The allocator does not weigh the
//     alternatives, because there is no register that survives the call.
//  2. The garbage collector never needs a register map. Everything live at a
//     safepoint is in the frame, which is why
//     specs/027-liveness-and-stackmaps.md is a spec about frames.
//
// # What this pass produces, and what it does not
//
// Allocate does not touch the function. It returns an Alloc: a location for
// every value, the register each operand is read from, the spill slots, the
// copies each phi becomes, and the two-address fix-ups. The code generator
// reads that and emits instructions.
//
// Nothing here names a machine operation. Everything about the machine arrives
// through Target, which is what keeps specs/002-architecture.md's rule true:
// adding a target needs no edit above the lowering rules.

import (
	"fmt"
	"sort"
	"strings"

	"golang.design/x/nanogo/ir"
)

// Copy is one move made by phi resolution or by a two-address fix-up.
//
// A Src with kind LocNone means the value is rematerialised into Dst: it has
// no storage of its own, so there is nothing to move from.
type Copy struct {
	Dst   Loc
	Src   Loc
	Value ID // the value whose bits move, for rematerialisation and for types
}

// EdgeCopies is the sequence of moves one control-flow edge carries.
//
// AtPredEnd says where they go. A predecessor with one successor takes them at
// its end. A predecessor with several successors cannot, because the moves
// would run on the other edge too, so they go at the start of the successor,
// which is possible exactly when the successor has one predecessor. The edge
// that is neither is a critical edge, and SplitCriticalEdges must have removed
// it before Allocate runs.
type EdgeCopies struct {
	Pred, Succ ID
	PredSlot   int // the slot of this edge in Succ's predecessor list
	AtPredEnd  bool
	Copies     []Copy
}

// Fixup is a copy that a two-address instruction needs before it runs.
//
// specs/026-register-allocation.md confines the amd64 property to one pass,
// and this is its whole output: when the destination differs from the first
// source, the first source is copied into the destination first. Dst is free
// at that point, because the allocator holds every register that a live value
// occupies, including the operands that die at this instruction.
type Fixup struct {
	Value ID
	Dst   Reg
	Src   Reg
}

// Slot is one spill slot in the frame.
//
// Offsets are not here. specs/027-liveness-and-stackmaps.md lays out the frame
// after allocation and groups the pointer-containing slots together, and it
// needs the size, the alignment and the pointer map to do it.
type Slot struct {
	Size  int64
	Align int64
	Ptr   bool // the slot may hold a pointer, so the collector scans it

	// Values lists the values that share the slot, in increasing identifier
	// order.
	Values []ID
}

// Alloc is the result of allocating one function.
//
// Every table is indexed by value identifier, which is what
// specs/053-determinism.md's rule needs: no map is read on the path that
// produces this, and no order in it comes from an address.
type Alloc struct {
	Target *Target
	Func   *Func

	// Home is where a value lives between its definition and its last use.
	// LocNone means it has no storage: a memory value, or a rematerialised
	// one.
	Home []Loc

	// Fixed is the register specs/030-abi.md fixes for a value's definition,
	// or NoReg. When Home is a slot and Fixed is a register, the value arrives
	// in that register and is stored to the slot at once, which is what
	// happens to an argument that is live across a call.
	Fixed []Reg

	// Result is the register a value's instruction writes. It is the home
	// register, or a scratch register when the value is spilled, or NoReg when
	// the value has no storage.
	Result []Reg

	// Args gives, for each value, the register each of its operands is read
	// from. NoReg where the operand needs no register, which is memory and
	// nothing else.
	Args [][]Reg

	// Remat marks the values that are recomputed at each use rather than
	// stored anywhere.
	Remat []bool

	// Spilled marks the values whose home is a slot.
	Spilled []bool

	Slots  []Slot
	Edges  []EdgeCopies
	Fixups []Fixup
}

// AllocError is a failure of Allocate.
type AllocError struct {
	Func   string
	Detail string
}

func (e *AllocError) Error() string { return "ssa: regalloc: " + e.Func + ": " + e.Detail }

// ScratchError reports an instruction that needs more scratch registers than
// the target reserves.
//
// It is a bound of the design rather than a bug in the input. A value in a
// slot is read into a scratch register at each use, a rematerialised value is
// recomputed into one, and a spilled result of a two-address instruction needs
// one of its own. The demand of one instruction is therefore the number of its
// distinct operands the code generator materialises, plus one on a two-address
// machine, and the target must reserve as many as its widest instruction has.
// On arm64 that is three, from the indexed stores and from MADD and MSUB, and
// the machine is three-address so a spilled result adds nothing.
//
// The bound holds only because the values whose operand count no machine
// bounds draw nothing. A phi has one operand per predecessor and a call or a
// return has one per argument, and assignOperands says why neither reads an
// operand out of a scratch register. Counting either would make the demand
// unbounded, and then no fixed reservation could serve it and raising the
// count would only move the failure.
//
// So this error means the machine grew an instruction wider than the target
// reserves for. The error names the operation and the counts, and
// TestArm64ScratchCoversTheOperationTable fails on the same change before any
// program does.
type ScratchError struct {
	Func  string
	Value ID
	Op    Op
	Class RegClass
	Need  int
	Have  int
}

func (e *ScratchError) Error() string {
	return fmt.Sprintf("ssa: regalloc: %s: v%d (%v) needs %d %v scratch registers and the target reserves %d",
		e.Func, e.Value, e.Op, e.Need, e.Class, e.Have)
}

// Allocate assigns a location to every value of f.
//
// f is not modified. f must verify, must have no unreachable block, and must
// have had SplitCriticalEdges run on it: phi resolution needs a block on every
// edge that carries a copy, and specs/026-register-allocation.md makes that a
// precondition rather than something to repair here.
func Allocate(f *Func, t *Target) (*Alloc, error) {
	if f == nil {
		return nil, &AllocError{Func: "?", Detail: "nil function"}
	}
	if errs := t.Validate(); len(errs) > 0 {
		return nil, &AllocError{Func: f.Name, Detail: errs[0].Error()}
	}
	an, err := newAnalysis(f, t)
	if err != nil {
		return nil, err
	}
	a := &Alloc{
		Target:  t,
		Func:    f,
		Home:    make([]Loc, f.NumValues()),
		Fixed:   make([]Reg, f.NumValues()),
		Result:  make([]Reg, f.NumValues()),
		Args:    make([][]Reg, f.NumValues()),
		Remat:   an.remat,
		Spilled: make([]bool, f.NumValues()),
	}
	for i := range a.Fixed {
		a.Fixed[i] = NoReg
		a.Result[i] = NoReg
	}
	if err := an.scan(a); err != nil {
		return nil, err
	}
	an.assignSlots(a)
	if err := an.assignOperands(a); err != nil {
		return nil, err
	}
	if err := an.resolvePhis(a); err != nil {
		return nil, err
	}
	return a, nil
}

// String returns a dump of the allocation.
//
// Values in identifier order, then slots, then edges. Nothing derives from a
// map or an address, so two allocations of one function are the same bytes.
// The determinism test of specs/053-determinism.md compares these strings.
func (a *Alloc) String() string {
	var b strings.Builder
	t := a.Target
	fmt.Fprintf(&b, "alloc %s [%s]\n", a.Func.Name, t.Name)
	for i := 0; i < len(a.Home); i++ {
		if a.Home[i].Kind == LocNone && !a.Remat[i] && a.Result[i] == NoReg && a.Args[i] == nil {
			continue
		}
		fmt.Fprintf(&b, "  v%d: home=%s", i, t.LocString(a.Home[i]))
		if a.Remat[i] {
			b.WriteString(" remat")
		}
		if a.Fixed[i] != NoReg {
			fmt.Fprintf(&b, " fixed=%s", t.RegName(a.Fixed[i]))
		}
		if a.Result[i] != NoReg {
			fmt.Fprintf(&b, " result=%s", t.RegName(a.Result[i]))
		}
		for j, r := range a.Args[i] {
			if r != NoReg {
				fmt.Fprintf(&b, " arg%d=%s", j, t.RegName(r))
			}
		}
		b.WriteString("\n")
	}
	for i, s := range a.Slots {
		fmt.Fprintf(&b, "  slot %d: size=%d align=%d ptr=%v values=%v\n", i, s.Size, s.Align, s.Ptr, s.Values)
	}
	for _, e := range a.Edges {
		where := "succ start"
		if e.AtPredEnd {
			where = "pred end"
		}
		fmt.Fprintf(&b, "  edge b%d -> b%d [%d] at %s\n", e.Pred, e.Succ, e.PredSlot, where)
		for _, c := range e.Copies {
			fmt.Fprintf(&b, "    %s <- %s (v%d)\n", t.LocString(c.Dst), t.LocString(c.Src), c.Value)
		}
	}
	for _, x := range a.Fixups {
		fmt.Fprintf(&b, "  fixup v%d: %s <- %s\n", x.Value, t.RegName(x.Dst), t.RegName(x.Src))
	}
	return b.String()
}

// bitmap is a dense set of value identifiers.
//
// A bitset rather than a map, because liveness reads it once per block per
// round and specs/053-determinism.md forbids ranging a map on a path that
// reaches the output.
type bitmap []uint64

func newBitmap(n int) bitmap { return make(bitmap, (n+63)/64) }

func (b bitmap) has(i ID) bool { return b[i/64]&(1<<uint(i%64)) != 0 }

func (b bitmap) set(i ID) { b[i/64] |= 1 << uint(i%64) }

func (b bitmap) clear(i ID) { b[i/64] &^= 1 << uint(i%64) }

// union adds every member of c to b and reports whether b changed.
func (b bitmap) union(c bitmap) bool {
	changed := false
	for i := range b {
		w := b[i] | c[i]
		if w != b[i] {
			b[i] = w
			changed = true
		}
	}
	return changed
}

func (b bitmap) copyFrom(c bitmap) { copy(b, c) }

// regAnalysis holds everything the allocator computes before it places a
// single value: the linearisation, the liveness, the live ranges, the uses,
// and the safepoints.
type regAnalysis struct {
	f *Func
	t *Target

	order []*Block // reverse postorder, the linearisation

	// pos is the position of each value in the linearisation, indexed by value
	// identifier. blockStart and blockEnd bound each block, indexed by block
	// identifier. The end position is one past the last value and is where a
	// block's control value is read and where an edge's copies go.
	pos        []int32
	blockStart []int32
	blockEnd   []int32

	// posValue maps a position back to the value at it, and holds nil at a
	// block-end position, which belongs to the terminator and to the copies an
	// edge carries rather than to a value.
	posValue []*Value

	liveIn  []bitmap
	liveOut []bitmap

	// tracked marks the values that occupy storage. A memory value and a
	// rematerialised value occupy none, so they are outside liveness
	// altogether, which is what makes rematerialisation remove pressure rather
	// than only remove spills.
	tracked []bool
	remat   []bool
	class   []RegClass
	// wide marks a value whose type does not fit in one register. It gets a
	// slot and never a register.
	wide []bool

	start []int32 // first position of the live range, by value identifier
	end   []int32 // last position of the live range
	uses  [][]int32

	calls      []int32  // safepoint positions, ascending
	callLive   []bitmap // the values live across each safepoint
	acrossCall []bool   // value is live across a safepoint

	// value maps an identifier back to its value, for the type a slot needs.
	value []*Value
}

func newAnalysis(f *Func, t *Target) (*regAnalysis, error) {
	dom := Dominators(f)
	an := &regAnalysis{f: f, t: t, order: dom.ReversePostorder()}
	for _, b := range f.Blocks {
		if !dom.Reachable(b) {
			return nil, &AllocError{Func: f.Name, Detail: fmt.Sprintf("b%d is not reachable, run Verify before allocating", b.ID)}
		}
	}
	if err := an.checkEdges(); err != nil {
		return nil, err
	}
	an.classify()
	an.linearise()
	an.liveness()
	an.ranges()
	an.safepoints()
	return an, nil
}

// checkEdges asserts that SplitCriticalEdges has run.
//
// specs/026-register-allocation.md calls the alternative the lost copy
// problem: a critical edge has no block that runs exactly when the edge is
// taken, so the copies a phi becomes have nowhere to go. Repairing it here
// would change the function, and Allocate does not change the function.
func (an *regAnalysis) checkEdges() error {
	for _, b := range an.f.Blocks {
		if len(b.Succs) < 2 {
			continue
		}
		for _, s := range b.Succs {
			if len(s.Preds) > 1 {
				return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf(
					"the edge b%d -> b%d is critical, run SplitCriticalEdges before allocating", b.ID, s.ID)}
			}
		}
	}
	return nil
}

// classify decides, for every value, whether it occupies storage and of what
// kind.
func (an *regAnalysis) classify() {
	n := an.f.NumValues()
	an.tracked = make([]bool, n)
	an.remat = make([]bool, n)
	an.wide = make([]bool, n)
	an.class = make([]RegClass, n)
	an.value = make([]*Value, n)
	for _, b := range an.f.Blocks {
		for _, v := range b.Values {
			an.value[v.ID] = v
			switch {
			case IsMemory(v):
				// Memory is an ordering edge, not a datum. It occupies
				// nothing.
			case an.t.Remat(v):
				an.remat[v.ID] = true
			case v.Type == nil || v.Type.Size == 0:
				// A value of no width has nothing to hold.
			default:
				an.tracked[v.ID] = true
				c, ok := an.t.ClassOf(v.Type)
				an.class[v.ID] = c
				an.wide[v.ID] = !ok
			}
		}
	}
}

// linearise numbers the positions of the blocks and the values.
func (an *regAnalysis) linearise() {
	an.pos = make([]int32, an.f.NumValues())
	an.blockStart = make([]int32, an.f.NumBlocks())
	an.blockEnd = make([]int32, an.f.NumBlocks())
	for i := range an.pos {
		an.pos[i] = -1
	}
	n := 0
	for _, b := range an.order {
		n += len(b.Values) + 1
	}
	an.posValue = make([]*Value, n)
	p := int32(0)
	for _, b := range an.order {
		an.blockStart[b.ID] = p
		for _, v := range b.Values {
			an.pos[v.ID] = p
			an.posValue[p] = v
			p++
		}
		an.blockEnd[b.ID] = p
		p++
	}
}

// liveness computes live-in and live-out per block.
//
// The backward dataflow of specs/027-liveness-and-stackmaps.md, over values
// rather than over stack slots, because the allocator needs to know what is
// live before it knows what has a slot. A phi's argument is not a use in the
// phi's block: it is a use at the end of the predecessor the argument arrives
// from, which is where the copy that realises the phi is placed.
func (an *regAnalysis) liveness() {
	n := an.f.NumValues()
	an.liveIn = make([]bitmap, an.f.NumBlocks())
	an.liveOut = make([]bitmap, an.f.NumBlocks())
	for _, b := range an.f.Blocks {
		an.liveIn[b.ID] = newBitmap(n)
		an.liveOut[b.ID] = newBitmap(n)
	}
	work := newBitmap(n)
	// The sets only grow, so the fixed point is reached when no live-in set
	// grew in a whole round. A predecessor reads its successor's live-in set
	// and nothing else, so live-in is the only set whose growth can propagate.
	for changed := true; changed; {
		changed = false
		// Reverse of the reverse postorder, so a block is visited after its
		// successors wherever the graph allows it. A back edge still needs a
		// second round, which the fixed point provides.
		for i := len(an.order) - 1; i >= 0; i-- {
			b := an.order[i]
			work.copyFrom(an.liveOut[b.ID])
			an.liveOutOf(b, work)
			an.liveOut[b.ID].union(work)
			work.copyFrom(an.liveOut[b.ID])
			an.transfer(b, work)
			if an.liveIn[b.ID].union(work) {
				changed = true
			}
		}
	}
}

// liveOutOf adds to live everything that leaves b alive: what its successors
// need, and the phi arguments that this edge carries.
func (an *regAnalysis) liveOutOf(b *Block, live bitmap) {
	for i, s := range b.Succs {
		live.union(an.liveIn[s.ID])
		j := predIndex(b, i)
		if j < 0 {
			continue
		}
		for _, v := range s.Values {
			if v.Op != OpPhi {
				break
			}
			if j < len(v.Args) {
				if a := v.Args[j]; a != nil && an.tracked[a.ID] {
					live.set(a.ID)
				}
			}
		}
	}
}

// transfer turns the live-out set of b into its live-in set.
func (an *regAnalysis) transfer(b *Block, live bitmap) {
	if b.Control != nil && !b.Control.dead && an.tracked[b.Control.ID] {
		// A block's control value is a use, at the end of the block. Missing
		// it would collapse the range of a value that is only a branch
		// condition, and two such values would share a register while both are
		// live at the branch.
		live.set(b.Control.ID)
	}
	for i := len(b.Values) - 1; i >= 0; i-- {
		v := b.Values[i]
		if an.tracked[v.ID] {
			live.clear(v.ID)
		}
		if v.Op == OpPhi {
			continue
		}
		for _, a := range v.Args {
			if a != nil && an.tracked[a.ID] {
				live.set(a.ID)
			}
		}
	}
}

// ranges computes one live range and the use positions of every tracked value.
//
// The range is the smallest interval of the linearisation that covers every
// position where the value is live. It has no holes. A hole would let two
// values share a register over a region where one of them is dead, and it
// would also let two values share a spill slot where one of them still has to
// survive; the second is the harder mistake, so the conservative interval is
// the one both the register assignment and the slot assignment use.
func (an *regAnalysis) ranges() {
	n := an.f.NumValues()
	an.start = make([]int32, n)
	an.end = make([]int32, n)
	an.uses = make([][]int32, n)
	for i := range an.start {
		an.start[i] = -1
		an.end[i] = -1
	}
	for _, b := range an.order {
		bs, be := an.blockStart[b.ID], an.blockEnd[b.ID]
		for id := ID(0); id < ID(n); id++ {
			if !an.tracked[id] {
				continue
			}
			if an.liveIn[b.ID].has(id) {
				an.extend(id, bs)
			}
			if an.liveOut[b.ID].has(id) {
				an.extend(id, be)
			}
		}
		for _, v := range b.Values {
			if an.tracked[v.ID] {
				an.extend(v.ID, an.pos[v.ID])
			}
			if v.Op == OpPhi {
				// The copies that realise a phi run at the end of a
				// predecessor, so the phi's home is written there. Its range
				// has to cover those points, or a value live in the
				// predecessor could hold the same register.
				for _, p := range b.Preds {
					an.extend(v.ID, an.blockEnd[p.ID])
				}
				continue
			}
			for _, a := range v.Args {
				if a != nil && an.tracked[a.ID] {
					an.extend(a.ID, an.pos[v.ID])
					an.addUse(a.ID, an.pos[v.ID])
				}
			}
		}
		if b.Control != nil && !b.Control.dead && an.tracked[b.Control.ID] {
			an.extend(b.Control.ID, be)
			an.addUse(b.Control.ID, be)
		}
		for i, s := range b.Succs {
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
				if a := v.Args[j]; a != nil && an.tracked[a.ID] {
					an.extend(a.ID, be)
					an.addUse(a.ID, be)
				}
			}
		}
	}
}

func (an *regAnalysis) extend(id ID, p int32) {
	if an.start[id] < 0 || p < an.start[id] {
		an.start[id] = p
	}
	if p > an.end[id] {
		an.end[id] = p
	}
}

// addUse records a use position. The walk visits positions in increasing
// order, so the list stays sorted and nextUse can binary search it.
func (an *regAnalysis) addUse(id ID, p int32) {
	u := an.uses[id]
	if len(u) > 0 && u[len(u)-1] == p {
		return
	}
	an.uses[id] = append(u, p)
}

// nextUse returns the first use of id at or after p, or a position past the
// end of the function when there is none.
//
// specs/026-register-allocation.md spills the interval with the furthest next
// use, not the one with the furthest end. A value with no use left is the best
// candidate of all, which is what the large return value expresses.
func (an *regAnalysis) nextUse(id ID, p int32) int32 {
	u := an.uses[id]
	i := sort.Search(len(u), func(i int) bool { return u[i] >= p })
	if i == len(u) {
		return int32(1) << 30
	}
	return u[i]
}

// safepoints records the calls and what is live across each one.
//
// Every call is a safepoint (specs/027-liveness-and-stackmaps.md). The set
// recorded is what is live after the call and was live before it, which is
// exactly the set that must be in the frame, because Go's ABI leaves no
// register standing.
func (an *regAnalysis) safepoints() {
	n := an.f.NumValues()
	an.acrossCall = make([]bool, n)
	live := newBitmap(n)
	for _, b := range an.order {
		live.copyFrom(an.liveOut[b.ID])
		if b.Control != nil && !b.Control.dead && an.tracked[b.Control.ID] {
			live.set(b.Control.ID)
		}
		// Backwards through the block, so live is the set live after the value
		// being visited.
		calls := make([]int32, 0, 4)
		sets := make([]bitmap, 0, 4)
		for i := len(b.Values) - 1; i >= 0; i-- {
			v := b.Values[i]
			if an.t.IsCall(v) {
				s := newBitmap(n)
				s.copyFrom(live)
				s.clear(v.ID) // the call's own result is not live across it
				calls = append(calls, an.pos[v.ID])
				sets = append(sets, s)
			}
			if an.tracked[v.ID] {
				live.clear(v.ID)
			}
			if v.Op == OpPhi {
				continue
			}
			for _, a := range v.Args {
				if a != nil && an.tracked[a.ID] {
					live.set(a.ID)
				}
			}
		}
		// The walk collected the calls of this block backwards. Reverse them
		// so an.calls stays ascending across the whole function.
		for i := len(calls) - 1; i >= 0; i-- {
			an.calls = append(an.calls, calls[i])
			an.callLive = append(an.callLive, sets[i])
		}
	}
	for k, p := range an.calls {
		for id := ID(0); id < ID(n); id++ {
			if an.callLive[k].has(id) && an.start[id] < p && p < an.end[id] {
				an.acrossCall[id] = true
			}
		}
	}
}

// overlaps reports whether the live ranges of two values meet at all.
//
// The closed form: two values whose ranges touch at a single position are
// overlapping here. Spill slot assignment uses this, because a slot is written
// at a definition and read at a use, and the order of the two within one
// instruction is the code generator's business rather than a property this
// pass can rely on.
func (an *regAnalysis) overlaps(a, b ID) bool {
	return an.start[a] <= an.end[b] && an.start[b] <= an.end[a]
}

// freesAt reports whether a range ending at position p is over by the time the
// value defined at p writes its result.
//
// An instruction reads its operands and then writes its result, so the result
// may take the register of an operand that dies at it. Three cases are not
// like that. A position that belongs to no value is a block end, where the
// value is still live on the edge. A phi is realised by copies at that block
// end. And a two-address instruction has a fix-up copy that writes the
// destination before the instruction reads its second operand.
func (an *regAnalysis) freesAt(p int32, id ID) bool {
	if p < 0 || int(p) >= len(an.posValue) {
		return false
	}
	v := an.posValue[p]
	return v != nil && v.ID == id && v.Op != OpPhi && !an.t.TwoAddress(v)
}

// conflicts reports whether two values may not share a register.
//
// This is overlaps with the one exception freesAt describes, and it is the
// rule the scan expires ranges by. The verifier uses the same predicate, so a
// register the scan reused is not then reported as shared.
func (an *regAnalysis) conflicts(u, v ID) bool {
	if an.start[u] > an.start[v] {
		u, v = v, u
	}
	if an.end[u] < an.start[v] {
		return false
	}
	if an.end[u] == an.start[v] && an.freesAt(an.start[v], v) {
		return false
	}
	return true
}

// scan is the linear scan of specs/026-register-allocation.md.
//
// Values are visited in order of the start of their live range. At each one:
// expire the ranges that ended, take a free register of the right class, and
// otherwise spill the range with the furthest next use.
//
// Two decisions are made before the scan reaches a value and are not the
// scan's to weigh. A value that does not fit one register goes to a slot. A
// value live across a call goes to a slot, because Go's ABI leaves no register
// standing across a call.
func (an *regAnalysis) scan(a *Alloc) error {
	t := an.t

	// Pre-coloured definitions. specs/030-abi.md fixes where an argument and a
	// result live, so those ranges are known before the scan and every other
	// value works around them.
	fixedOn := make([][]ID, len(t.Regs))
	for _, b := range an.order {
		for _, v := range b.Values {
			if !an.tracked[v.ID] {
				continue
			}
			r, ok := t.DefReg(v)
			if !ok {
				continue
			}
			if !t.IsAllocatable(r) {
				return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf(
					"v%d is fixed to %s, which is not allocatable", v.ID, t.RegName(r))}
			}
			if t.RegClassOf(r) != an.class[v.ID] {
				return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf(
					"v%d is of class %v and is fixed to %s, which is of class %v",
					v.ID, an.class[v.ID], t.RegName(r), t.RegClassOf(r))}
			}
			for _, u := range fixedOn[r] {
				if an.conflicts(u, v.ID) {
					return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf(
						"v%d and v%d are both fixed to %s and are live at the same time",
						u, v.ID, t.RegName(r))}
				}
			}
			a.Fixed[v.ID] = r
			fixedOn[r] = append(fixedOn[r], v.ID)
		}
	}

	// The scan order. Sorting by the start of the range rather than by the
	// position of the definition matters for a phi, whose range reaches back
	// to the end of each predecessor: a phi in a loop header starts before the
	// values of the body that its own copies would otherwise overwrite.
	ids := make([]ID, 0, an.f.NumValues())
	for id := ID(0); id < ID(an.f.NumValues()); id++ {
		if an.tracked[id] {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if an.start[ids[i]] != an.start[ids[j]] {
			return an.start[ids[i]] < an.start[ids[j]]
		}
		return ids[i] < ids[j]
	})

	regOwner := make([]ID, len(t.Regs))
	for i := range regOwner {
		regOwner[i] = -1
	}
	var active []ID

	spill := func(id ID) {
		a.Spilled[id] = true
		a.Home[id] = Loc{}
	}
	evict := func(id ID) {
		if h := a.Home[id]; h.Kind == LocReg {
			regOwner[h.Reg] = -1
		}
		for i, w := range active {
			if w == id {
				active = append(active[:i], active[i+1:]...)
				break
			}
		}
		spill(id)
	}
	assign := func(id ID, r Reg) {
		regOwner[r] = id
		a.Home[id] = RegLoc(r)
		active = append(active, id)
	}
	// conflictsFixed reports that r is reserved for a pre-coloured value whose
	// range meets id's.
	conflictsFixed := func(r Reg, id ID) bool {
		for _, u := range fixedOn[r] {
			if u != id && an.conflicts(u, id) {
				return true
			}
		}
		return false
	}

	for _, id := range ids {
		pos := an.start[id]
		// Expire. A range that ended before this one started frees its
		// register.
		//
		// A range that ends exactly here frees it too, but only in one case.
		// An instruction reads its operands before it writes its result, so
		// the result may take the register of an operand that dies at it. That
		// is not true when the range ends at a block-end position, where the
		// value is still live on the edge, nor for a phi, whose copies run at
		// that block end, nor for a two-address instruction, where the fix-up
		// copy writes the destination before the instruction reads its second
		// operand.
		atEnd := false
		if v := an.posValue[pos]; v != nil && v.ID == id && v.Op != OpPhi && !t.TwoAddress(v) {
			atEnd = true
		}
		for i := 0; i < len(active); {
			w := active[i]
			if an.end[w] < pos || (atEnd && an.end[w] == pos) {
				if h := a.Home[w]; h.Kind == LocReg {
					regOwner[h.Reg] = -1
				}
				active = append(active[:i], active[i+1:]...)
				continue
			}
			i++
		}

		if an.wide[id] {
			spill(id)
			continue
		}
		if an.acrossCall[id] {
			// specs/026-register-allocation.md: not a choice. Every register
			// is destroyed by the call, so the value has to be in the frame.
			spill(id)
			continue
		}

		c := an.class[id]
		if r := a.Fixed[id]; r != NoReg {
			if w := regOwner[r]; w >= 0 && w != id {
				// The ABI wins. The value that held the register goes to a
				// slot, and every instruction that reads it reads the slot,
				// because Alloc describes the final home and nothing was
				// emitted yet.
				evict(w)
			}
			assign(id, r)
			continue
		}

		free := NoReg
		for _, r := range t.Allocatable[c].Regs() {
			if regOwner[r] < 0 && !conflictsFixed(r, id) {
				free = r
				break
			}
		}
		if free != NoReg {
			assign(id, free)
			continue
		}

		// No register. Spill the range with the furthest next use, which is
		// the one that costs least to reload.
		cands := make([]ID, 0, len(active))
		for _, w := range active {
			h := a.Home[w]
			if h.Kind != LocReg || t.RegClassOf(h.Reg) != c {
				continue
			}
			if conflictsFixed(h.Reg, id) {
				continue
			}
			cands = append(cands, w)
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i] < cands[j] })
		victim := id
		best := an.nextUse(id, pos)
		for _, w := range cands {
			if n := an.nextUse(w, pos); n > best {
				victim, best = w, n
			}
		}
		if victim == id {
			spill(id)
			continue
		}
		r := a.Home[victim].Reg
		evict(victim)
		assign(id, r)
	}
	return nil
}

// assignSlots gives every spilled value a slot, sharing one where the rules
// allow it.
//
// Values are visited in identifier order and take the first slot that accepts
// them, so the assignment depends on the function and on nothing else.
func (an *regAnalysis) assignSlots(a *Alloc) {
	for id := ID(0); id < ID(len(a.Spilled)); id++ {
		if !a.Spilled[id] {
			continue
		}
		v := an.value[id]
		placed := false
		for i := range a.Slots {
			if !an.slotAccepts(&a.Slots[i], id) {
				continue
			}
			a.Slots[i].Values = append(a.Slots[i].Values, id)
			a.Home[id] = SlotLoc(int32(i))
			placed = true
			break
		}
		if placed {
			continue
		}
		a.Slots = append(a.Slots, Slot{
			Size:   v.Type.Size,
			Align:  v.Type.Align,
			Ptr:    v.Type.HasPointers(),
			Values: []ID{id},
		})
		a.Home[id] = SlotLoc(int32(len(a.Slots) - 1))
	}
}

// slotAccepts reports whether the slot can also hold v.
func (an *regAnalysis) slotAccepts(s *Slot, v ID) bool {
	for _, u := range s.Values {
		if !an.canShareSlot(u, v) {
			return false
		}
	}
	return true
}

// canShareSlot reports whether two values may live in one spill slot.
//
// Three conditions, and they come from two places.
//
// The live ranges must be disjoint. This is the condition that carries
// correctness: the slot has to hold each value from its definition to its last
// use, and a range here has no holes precisely so that a value dead in the
// middle of its range still owns its slot there.
//
// The layouts must be identical: same size, same alignment, same pointer map.
// This is stronger than specs/026-register-allocation.md asks for and the
// reason is stack copying. specs/027-liveness-and-stackmaps.md's liveness is a
// may-analysis, so a slot that holds a pointer on any path reaching a
// safepoint is described as a pointer there. Growing a stack rewrites every
// word the map calls a pointer. A slot shared by a pointer and an integer
// would have the integer adjusted by the copy, which corrupts it silently.
// Requiring one layout per slot removes the case.
//
// Neither value may be a pointer that is live at a safepoint inside the
// other's range. This is specs/026-register-allocation.md's own condition,
// and with hole-free ranges the disjointness above already implies it: a
// pointer cannot be live inside a range that does not meet its own. It is
// computed here from the safepoint live sets rather than from the ranges, so
// that it is a real check on real data the moment ranges gain holes, which is
// the natural next step for this allocator. See the report in the spec.
func (an *regAnalysis) canShareSlot(u, v ID) bool {
	if an.overlaps(u, v) {
		return false
	}
	tu, tv := an.value[u].Type, an.value[v].Type
	if tu.Size != tv.Size || tu.Align != tv.Align || !samePtrBits(tu, tv) {
		return false
	}
	return !an.pointerLiveAtSafepointIn(u, v) && !an.pointerLiveAtSafepointIn(v, u)
}

// samePtrBits reports whether two types have the same pointer map.
func samePtrBits(a, b *ir.Type) bool {
	if len(a.PtrBits) != len(b.PtrBits) {
		return false
	}
	for i := range a.PtrBits {
		if a.PtrBits[i] != b.PtrBits[i] {
			return false
		}
	}
	return true
}

// pointerLiveAtSafepointIn reports whether u holds a pointer and is live at a
// safepoint that falls inside v's live range.
func (an *regAnalysis) pointerLiveAtSafepointIn(u, v ID) bool {
	if !an.value[u].Type.HasPointers() {
		return false
	}
	for k, p := range an.calls {
		if p < an.start[v] || p > an.end[v] {
			continue
		}
		if an.callLive[k].has(u) {
			return true
		}
	}
	return false
}

// classOf returns the register class of a value, whether it is tracked or
// rematerialised.
func (an *regAnalysis) classOf(id ID) RegClass {
	if an.tracked[id] {
		return an.class[id]
	}
	c, _ := an.t.ClassOf(an.value[id].Type)
	return c
}

// assignOperands names the register every instruction reads each operand from
// and the register it writes its result to.
//
// A value whose home is a register is read from it. A value in a slot is read
// into a scratch register, and a rematerialised value is recomputed into one.
// The result of a spilled value is computed into the first scratch register of
// its class and stored from there; reusing an operand's scratch register is
// safe, because an instruction reads its sources before it writes its
// destination.
//
// Two kinds of value read no operand out of a scratch register, and saying so
// is what gives the scratch demand a bound.
//
// A phi is not an instruction. resolvePhis turns it into a move on each edge,
// and the code generator emits nothing for the phi itself, so a register named
// for operand i would be a register nothing reads.
//
// A call and a return read their operands where specs/030-abi.md puts them.
// UseReg names the register for an operand the convention puts in one, and an
// operand it puts in the argument area is written there by a store of its own,
// which materialises it into one register and holds it no longer than that
// store.
//
// Both have an operand count the machine does not bound: a phi has one per
// predecessor, a call one per argument. Drawing a scratch register for them
// would make the demand unbounded, and a target that reserves a fixed number
// could then be defeated by a wider select or a longer parameter list.
func (an *regAnalysis) assignOperands(a *Alloc) error {
	t := an.t
	for _, b := range an.order {
		for _, v := range b.Values {
			regs := make([]Reg, len(v.Args))
			var used [NumRegClass]int
			abiPlaced := t.ABIPlaces != nil && t.ABIPlaces(v)
			for i, arg := range v.Args {
				regs[i] = NoReg
				if arg == nil || IsMemory(arg) {
					continue
				}
				if !an.tracked[arg.ID] && !an.remat[arg.ID] {
					// A value of no width. Nothing is read.
					continue
				}
				c, ok := t.ClassOf(arg.Type)
				if an.remat[arg.ID] && !ok {
					return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf(
						"the target calls v%d rematerialisable and its type %v does not fit one register",
						arg.ID, arg.Type)}
				}
				if v.Op == OpPhi {
					// A phi reads nothing here. Its operands become moves on
					// the edges, which resolvePhis places from the operands'
					// homes.
					continue
				}
				if r, fixed := t.UseReg(v, i); fixed {
					// specs/030-abi.md fixes where a call reads its operands.
					// The move from the operand's home into that register is
					// the code generator's, and the register is named here so
					// that it knows to make it.
					regs[i] = r
					continue
				}
				if abiPlaced {
					// The convention put this operand in the argument area
					// rather than a register. The code generator writes it
					// there with a store of its own and the instruction reads
					// no register for it.
					continue
				}
				if !an.remat[arg.ID] && a.Home[arg.ID].Kind == LocReg {
					regs[i] = a.Home[arg.ID].Reg
					continue
				}
				if !ok {
					// A value too wide for a register is read from its slot by
					// the instruction itself, not into a register.
					continue
				}
				// One value read twice is read once. Two operands of one
				// instruction that name the same value share the register it
				// was read into, which also keeps the scratch demand at the
				// number of distinct values.
				if j := sameEarlierArg(v, i); j >= 0 && regs[j] != NoReg {
					regs[i] = regs[j]
					continue
				}
				if used[c] >= len(t.Scratch[c]) {
					return &ScratchError{Func: an.f.Name, Value: v.ID, Op: v.Op, Class: c,
						Need: used[c] + 1, Have: len(t.Scratch[c])}
				}
				regs[i] = t.Scratch[c][used[c]]
				used[c]++
			}
			a.Args[v.ID] = regs

			if an.tracked[v.ID] {
				c := an.class[v.ID]
				switch h := a.Home[v.ID]; {
				case h.Kind == LocReg:
					a.Result[v.ID] = h.Reg
				case an.wide[v.ID]:
					// The instruction writes the slot itself. There is no
					// register wide enough to pass through.
					a.Result[v.ID] = NoReg
				case t.TwoAddress(v):
					// The fix-up copy below writes the destination before the
					// instruction reads its other operands, so a spilled
					// result needs a scratch register of its own. On a
					// three-address machine it can share one with an operand,
					// because an instruction reads its sources and then writes
					// its destination.
					if used[c] >= len(t.Scratch[c]) {
						return &ScratchError{Func: an.f.Name, Value: v.ID, Op: v.Op, Class: c,
							Need: used[c] + 1, Have: len(t.Scratch[c])}
					}
					a.Result[v.ID] = t.Scratch[c][used[c]]
					used[c]++
				default:
					a.Result[v.ID] = t.Scratch[c][0]
				}
			}

			// specs/026-register-allocation.md's two-address fix-up, the one
			// place a target property other than register class reaches above
			// the lowering rules. The destination is free at this point: every
			// register a live value occupies is held, including the operands
			// that die at this instruction, so the copy cannot destroy one.
			if t.TwoAddress(v) && len(regs) > 0 && regs[0] != NoReg &&
				a.Result[v.ID] != NoReg && a.Result[v.ID] != regs[0] {
				a.Fixups = append(a.Fixups, Fixup{Value: v.ID, Dst: a.Result[v.ID], Src: regs[0]})
			}
		}
	}
	return nil
}

// sameEarlierArg returns the index of the first operand of v that is the same
// value as operand i, or -1 when there is none before i.
func sameEarlierArg(v *Value, i int) int {
	for j := 0; j < i; j++ {
		if v.Args[j] == v.Args[i] {
			return j
		}
	}
	return -1
}

// resolvePhis turns every phi into copies on the edges that reach it.
//
// A phi is not an instruction. It means the value arriving on this edge, so
// after allocation it is a move on each edge, and the moves on one edge happen
// at once. Sequentialising them is the swap problem, and ParallelCopy is the
// answer to it.
func (an *regAnalysis) resolvePhis(a *Alloc) error {
	t := an.t
	var temp [NumRegClass]Loc
	for c := RegClass(0); c < NumRegClass; c++ {
		temp[c] = RegLoc(t.Scratch[c][0])
	}
	for _, s := range an.order {
		if len(s.Values) == 0 || s.Values[0].Op != OpPhi {
			continue
		}
		for j, p := range s.Preds {
			var cps []Copy
			for _, phi := range s.Values {
				if phi.Op != OpPhi {
					break
				}
				if !an.tracked[phi.ID] {
					// A memory phi is an ordering edge and moves nothing.
					continue
				}
				if j >= len(phi.Args) {
					return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf(
						"v%d has %d arguments and b%d has %d predecessors", phi.ID, len(phi.Args), s.ID, len(s.Preds))}
				}
				arg := phi.Args[j]
				if arg == nil {
					return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf(
						"argument %d of v%d is nil", j, phi.ID)}
				}
				dst := a.Home[phi.ID]
				var src Loc
				switch {
				case an.remat[arg.ID]:
					// No storage to move from. The copy recomputes the value
					// into the destination.
				case an.tracked[arg.ID]:
					src = a.Home[arg.ID]
					if src == dst {
						continue
					}
				default:
					continue
				}
				cps = append(cps, Copy{Dst: dst, Src: src, Value: arg.ID})
			}
			if len(cps) == 0 {
				continue
			}
			seq, err := ParallelCopy(cps, temp, an.classOf)
			if err != nil {
				return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf("b%d -> b%d: %v", p.ID, s.ID, err)}
			}
			// Where the moves go. A predecessor with one successor takes them
			// at its end. Otherwise the successor must have one predecessor,
			// and they go at its start. The edge that is neither is critical
			// and checkEdges already refused it.
			atPredEnd := len(p.Succs) == 1
			if !atPredEnd && len(s.Preds) != 1 {
				return &AllocError{Func: an.f.Name, Detail: fmt.Sprintf(
					"the edge b%d -> b%d carries copies and is critical", p.ID, s.ID)}
			}
			a.Edges = append(a.Edges, EdgeCopies{
				Pred: p.ID, Succ: s.ID, PredSlot: j, AtPredEnd: atPredEnd, Copies: seq,
			})
		}
	}
	return nil
}

// ParallelCopy sequentialises copies that must appear to happen at once.
//
// The copies on one edge form a permutation of locations. Emitting them in
// order overwrites a source that a later copy still needs, which is the swap
// problem of specs/026-register-allocation.md. The answer is the classical
// one: emit every copy whose destination is nobody's source, which resolves
// every chain, and when only cycles are left, break one by saving a source
// into a temporary and pointing the copies that read it at the temporary
// instead. A cycle of length n becomes n+1 moves.
//
// temp names one temporary location per register class, and class says which
// class a value belongs to. The temporary must be a location no copy writes,
// which is why the allocator passes a scratch register: it is outside the
// allocatable set, so it is never a value's home.
//
// copies is not modified.
func ParallelCopy(copies []Copy, temp [NumRegClass]Loc, class func(ID) RegClass) ([]Copy, error) {
	work := make([]Copy, len(copies))
	copy(work, copies)
	for i := range work {
		if work[i].Dst.Kind == LocNone {
			return nil, fmt.Errorf("a parallel copy writes nowhere: v%d", work[i].Value)
		}
		for j := 0; j < i; j++ {
			if work[j].Dst == work[i].Dst {
				return nil, fmt.Errorf("two parallel copies write %v: v%d and v%d", work[i].Dst, work[j].Value, work[i].Value)
			}
		}
	}

	done := make([]bool, len(work))
	out := make([]Copy, 0, len(work)+2)
	left := len(work)
	for left > 0 {
		progress := false
		for i := range work {
			if done[i] || isPendingSource(work, done, i) {
				continue
			}
			out = append(out, work[i])
			done[i] = true
			left--
			progress = true
		}
		if progress {
			continue
		}
		// Only cycles are left. Every remaining destination is somebody's
		// source, so nothing can be emitted until one source is saved.
		i := 0
		for done[i] {
			i++
		}
		c := work[i]
		if c.Src.Kind == LocNone {
			// A rematerialised source reads nothing, so it can never be part
			// of a cycle. Reaching here would mean the search above is wrong.
			return nil, fmt.Errorf("a rematerialised copy of v%d is in a cycle", c.Value)
		}
		t := temp[class(c.Value)]
		out = append(out, Copy{Dst: t, Src: c.Src, Value: c.Value})
		for j := range work {
			if !done[j] && work[j].Src == c.Src {
				work[j].Src = t
			}
		}
	}
	return out, nil
}

// isPendingSource reports whether the destination of copy i is read by a copy
// that has not been emitted yet.
func isPendingSource(work []Copy, done []bool, i int) bool {
	for j := range work {
		if j == i || done[j] {
			continue
		}
		if work[j].Src == work[i].Dst {
			return true
		}
	}
	return false
}

// The allocation invariants.
//
// verify.go states why the checker exists and the reason holds one pass later:
// a wrong allocation is found here rather than in a program that computes the
// wrong answer once, under load, after a garbage collection. Each violation
// names the invariant it broke, so a test that claims to exercise one cannot
// pass on the strength of another.

// AllocInvariant identifies one property VerifyAllocation checks.
type AllocInvariant uint8

const (
	// AllocInvNone is not an invariant. It marks a failure that is not a
	// broken invariant, such as a function the analysis cannot run on.
	AllocInvNone AllocInvariant = iota

	// AllocInvHome: every value that occupies storage has exactly one home,
	// and every value that occupies none has no home.
	AllocInvHome

	// AllocInvClass: a register holding a value belongs to the class of the
	// value's type.
	AllocInvClass

	// AllocInvReserved: only an allocatable register holds a value. A reserved
	// register and a scratch register never do.
	AllocInvReserved

	// AllocInvCall: no value stays in a register across a call. Go's ABI has
	// no callee-saved registers, so the register would not survive.
	AllocInvCall

	// AllocInvOverlap: no two values whose live ranges meet share a register.
	AllocInvOverlap

	// AllocInvEdge: the copies on an edge realise the phis. Running them in
	// the order given leaves every phi's home holding the value that arrives
	// on that edge, which is the swap problem stated as a check.
	AllocInvEdge

	// AllocInvPhi: every phi is resolved into copies, and they sit on an edge
	// that has a place to put them.
	AllocInvPhi

	// AllocInvSlot: two values share a spill slot only where
	// specs/027-liveness-and-stackmaps.md allows it.
	AllocInvSlot

	// AllocInvOperand: every instruction names the register each operand is
	// read from, and no two operands are read into one scratch register.
	AllocInvOperand
)

var allocInvariantNames = [...]string{
	AllocInvNone:     "none",
	AllocInvHome:     "every value has exactly one home",
	AllocInvClass:    "a register matches the class of the value",
	AllocInvReserved: "only an allocatable register holds a value",
	AllocInvCall:     "no value is in a register across a call",
	AllocInvOverlap:  "no two live values share a register",
	AllocInvEdge:     "the copies on an edge realise the phis",
	AllocInvPhi:      "every phi is resolved into copies on a non-critical edge",
	AllocInvSlot:     "a spill slot is shared only where the stack map allows",
	AllocInvOperand:  "every operand names the register it is read from",
}

func (i AllocInvariant) String() string {
	if int(i) < len(allocInvariantNames) && allocInvariantNames[i] != "" {
		return allocInvariantNames[i]
	}
	return "allocinvariant(?)"
}

// AllocViolation is one broken allocation invariant.
type AllocViolation struct {
	Invariant AllocInvariant
	Block     ID // -1 when the violation is about no block
	Value     ID // -1 when the violation is about no value
	Detail    string
}

func (v AllocViolation) String() string {
	where := ""
	if v.Block >= 0 {
		where = fmt.Sprintf("b%d ", v.Block)
	}
	if v.Value >= 0 {
		where += fmt.Sprintf("v%d ", v.Value)
	}
	return fmt.Sprintf("%s%s: %s", where, v.Invariant, v.Detail)
}

// CheckAllocation runs VerifyAllocation and returns the violations as an
// error.
func CheckAllocation(f *Func, a *Alloc) error {
	vs := VerifyAllocation(f, a)
	if len(vs) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ssa: regalloc: %s: %d violations", f.Name, len(vs))
	for _, v := range vs {
		fmt.Fprintf(&b, "\n\t%v", v)
	}
	return &AllocError{Func: f.Name, Detail: b.String()}
}

// VerifyAllocation checks an allocation against the function it came from.
//
// The liveness it checks against is recomputed here from f, not taken from the
// allocator's bookkeeping, so a decision the allocator made for a wrong reason
// is still caught. What it cannot catch is a wrong liveness, which is why the
// analysis has its own tests.
func VerifyAllocation(f *Func, a *Alloc) []AllocViolation {
	var out []AllocViolation
	add := func(inv AllocInvariant, b, v ID, format string, args ...any) {
		out = append(out, AllocViolation{Invariant: inv, Block: b, Value: v, Detail: fmt.Sprintf(format, args...)})
	}
	if a == nil || a.Target == nil {
		add(AllocInvNone, -1, -1, "no allocation")
		return out
	}
	t := a.Target
	an, err := newAnalysis(f, t)
	if err != nil {
		add(AllocInvNone, -1, -1, "%v", err)
		return out
	}
	if len(a.Home) < f.NumValues() {
		add(AllocInvHome, -1, -1, "the allocation has %d homes and the function has %d values", len(a.Home), f.NumValues())
		return out
	}

	verifyHomes(f, a, an, add)
	verifyOverlap(f, a, an, add)
	verifyOperands(f, a, an, add)
	verifySlots(f, a, an, add)
	verifyEdges(f, a, an, add)
	return out
}

type addViolation func(inv AllocInvariant, b, v ID, format string, args ...any)

// verifyHomes checks the location of every value, its class, and the rule that
// nothing stays in a register across a call.
func verifyHomes(f *Func, a *Alloc, an *regAnalysis, add addViolation) {
	t := a.Target
	for _, b := range an.order {
		for _, v := range b.Values {
			h := a.Home[v.ID]
			switch {
			case IsMemory(v):
				if h.Kind != LocNone {
					add(AllocInvHome, b.ID, v.ID, "a memory value has home %v", t.LocString(h))
				}
			case an.remat[v.ID]:
				if h.Kind != LocNone {
					add(AllocInvHome, b.ID, v.ID, "a rematerialised value has home %v, and is recomputed at each use", t.LocString(h))
				}
				if !a.Remat[v.ID] {
					add(AllocInvHome, b.ID, v.ID, "the target calls the value rematerialisable and the allocation does not")
				}
			case an.tracked[v.ID]:
				if h.Kind == LocNone {
					add(AllocInvHome, b.ID, v.ID, "the value occupies storage and has no home")
				}
			default:
				if h.Kind != LocNone {
					add(AllocInvHome, b.ID, v.ID, "a value of no width has home %v", t.LocString(h))
				}
			}
			switch h.Kind {
			case LocReg:
				if !t.IsAllocatable(h.Reg) {
					add(AllocInvReserved, b.ID, v.ID, "%s is not allocatable", t.RegName(h.Reg))
				}
				if t.IsScratch(h.Reg) {
					add(AllocInvReserved, b.ID, v.ID, "%s is a scratch register", t.RegName(h.Reg))
				}
				if an.tracked[v.ID] && t.RegClassOf(h.Reg) != an.class[v.ID] {
					add(AllocInvClass, b.ID, v.ID, "%v is of class %v and lives in %s, which is of class %v",
						v.Type, an.class[v.ID], t.RegName(h.Reg), t.RegClassOf(h.Reg))
				}
				if an.acrossCall[v.ID] {
					add(AllocInvCall, b.ID, v.ID, "the value is live across a call and lives in %s, and no register survives a call", t.RegName(h.Reg))
				}
			case LocSlot:
				if h.Slot < 0 || int(h.Slot) >= len(a.Slots) {
					add(AllocInvHome, b.ID, v.ID, "slot %d does not exist", h.Slot)
				}
			}
		}
	}
}

// verifyOverlap checks that no register holds two values whose live ranges
// meet.
func verifyOverlap(f *Func, a *Alloc, an *regAnalysis, add addViolation) {
	t := a.Target
	byReg := make([][]ID, len(t.Regs))
	for id := ID(0); id < ID(f.NumValues()); id++ {
		if h := a.Home[id]; h.Kind == LocReg && h.Reg >= 0 && int(h.Reg) < len(byReg) {
			byReg[h.Reg] = append(byReg[h.Reg], id)
		}
	}
	for r, ids := range byReg {
		// Every pair, not only the neighbours in range order. A checker that
		// looked at neighbours would depend on the ranges being intervals,
		// which is exactly what a later version of this pass may change.
		for i := range ids {
			for j := i + 1; j < len(ids); j++ {
				if an.conflicts(ids[i], ids[j]) {
					add(AllocInvOverlap, -1, ids[j], "v%d and v%d are both in %s and are live at the same time",
						ids[i], ids[j], t.RegName(Reg(r)))
				}
			}
		}
	}
}

// verifyOperands checks that every instruction names where it reads each
// operand from.
//
// It mirrors assignOperands case for case, the two kinds of value that read no
// operand out of a scratch register included: a register named for an operand
// of a phi, or for an operand a call or a return leaves in the argument area,
// is a register the code generator never reads, and naming one is how the
// scratch demand became unbounded.
func verifyOperands(f *Func, a *Alloc, an *regAnalysis, add addViolation) {
	t := a.Target
	for _, b := range an.order {
		for _, v := range b.Values {
			regs := a.Args[v.ID]
			if len(regs) != len(v.Args) {
				add(AllocInvOperand, b.ID, v.ID, "%d operand registers for %d arguments", len(regs), len(v.Args))
				continue
			}
			abiPlaced := t.ABIPlaces != nil && t.ABIPlaces(v)
			for i, arg := range v.Args {
				r := regs[i]
				if arg == nil || IsMemory(arg) || (!an.tracked[arg.ID] && !an.remat[arg.ID]) {
					if r != NoReg {
						add(AllocInvOperand, b.ID, v.ID, "operand %d needs no register and names %s", i, t.RegName(r))
					}
					continue
				}
				if v.Op == OpPhi {
					if r != NoReg {
						add(AllocInvOperand, b.ID, v.ID,
							"operand %d of a phi names %s, and a phi reads no register: its operands are moved on the edges",
							i, t.RegName(r))
					}
					continue
				}
				if fixed, ok := t.UseReg(v, i); ok {
					if r != fixed {
						add(AllocInvOperand, b.ID, v.ID, "the ABI reads operand %d from %s and the allocation names %s",
							i, t.RegName(fixed), t.RegName(r))
					}
					continue
				}
				if abiPlaced {
					if r != NoReg {
						add(AllocInvOperand, b.ID, v.ID,
							"operand %d travels in the argument area and names %s, and no register is read for it",
							i, t.RegName(r))
					}
					continue
				}
				if h := a.Home[arg.ID]; !an.remat[arg.ID] && h.Kind == LocReg {
					if r != h.Reg {
						add(AllocInvOperand, b.ID, v.ID, "operand %d lives in %s and is read from %s",
							i, t.RegName(h.Reg), t.RegName(r))
					}
					continue
				}
				if c, ok := t.ClassOf(arg.Type); ok {
					if !t.IsScratch(r) {
						add(AllocInvOperand, b.ID, v.ID, "operand %d comes out of the frame and is read into %s, which is not a scratch register",
							i, t.RegName(r))
					} else if t.RegClassOf(r) != c {
						add(AllocInvClass, b.ID, v.ID, "operand %d is of class %v and is read into %s, which is of class %v",
							i, c, t.RegName(r), t.RegClassOf(r))
					}
				}
			}
			// Two operands read out of the frame into one scratch register
			// would leave the instruction reading one value twice.
			for i := range v.Args {
				for j := i + 1; j < len(v.Args); j++ {
					if regs[i] == NoReg || regs[i] != regs[j] || v.Args[i] == v.Args[j] {
						continue
					}
					if t.IsScratch(regs[i]) {
						add(AllocInvOperand, b.ID, v.ID, "operands %d and %d are different values read into %s",
							i, j, t.RegName(regs[i]))
					}
				}
			}
			res := a.Result[v.ID]
			switch h := a.Home[v.ID]; {
			case h.Kind == LocReg:
				if res != h.Reg {
					add(AllocInvOperand, b.ID, v.ID, "the value lives in %s and its result is written to %s",
						t.RegName(h.Reg), t.RegName(res))
				}
			case h.Kind == LocSlot && !an.wide[v.ID]:
				if !t.IsScratch(res) {
					add(AllocInvOperand, b.ID, v.ID, "the value is spilled and its result is written to %s, which is not a scratch register",
						t.RegName(res))
				}
			case h.Kind == LocNone:
				if res != NoReg {
					add(AllocInvOperand, b.ID, v.ID, "the value has no home and its result is written to %s", t.RegName(res))
				}
			}
		}
	}
}

// verifySlots checks the slot table and the sharing rule.
func verifySlots(f *Func, a *Alloc, an *regAnalysis, add addViolation) {
	seen := make([]int, f.NumValues())
	for i := range a.Slots {
		s := &a.Slots[i]
		for k, u := range s.Values {
			if u < 0 || int(u) >= len(seen) || an.value[u] == nil {
				add(AllocInvSlot, -1, u, "slot %d holds a value that is not in the function", i)
				continue
			}
			seen[u]++
			if h := a.Home[u]; h.Kind != LocSlot || int(h.Slot) != i {
				add(AllocInvSlot, -1, u, "slot %d lists the value and its home is %v", i, h)
			}
			ty := an.value[u].Type
			if ty.Size > s.Size || ty.Align > s.Align {
				add(AllocInvSlot, -1, u, "slot %d is %d bytes aligned to %d and the value needs %d aligned to %d",
					i, s.Size, s.Align, ty.Size, ty.Align)
			}
			if ty.HasPointers() != s.Ptr {
				add(AllocInvSlot, -1, u, "slot %d has ptr=%v and the value's type %v has pointers=%v",
					i, s.Ptr, ty, ty.HasPointers())
			}
			for _, w := range s.Values[k+1:] {
				if w < 0 || int(w) >= len(seen) || an.value[w] == nil {
					// Reported when the walk reaches it. The checker must not
					// fault on a broken table, which is the whole reason it
					// runs on one.
					continue
				}
				if !an.canShareSlot(u, w) {
					add(AllocInvSlot, -1, w, "v%d and v%d share slot %d and may not", u, w, i)
				}
			}
		}
	}
	for id := ID(0); id < ID(f.NumValues()); id++ {
		if h := a.Home[id]; h.Kind == LocSlot && seen[id] != 1 {
			add(AllocInvSlot, -1, id, "the value lives in slot %d and is listed in %d slots", h.Slot, seen[id])
		}
	}
}

// verifyEdges replays the copies of every edge and checks that they realise
// the phis.
//
// Replaying is what catches the swap problem. A sequence that overwrites a
// source before another copy reads it leaves some phi holding the wrong value,
// and the final state says so whatever the shape of the permutation was.
func verifyEdges(f *Func, a *Alloc, an *regAnalysis, add addViolation) {
	t := a.Target
	// The copies of each edge, found by successor and predecessor slot.
	index := make([][]*EdgeCopies, f.NumBlocks())
	for i := range a.Edges {
		e := &a.Edges[i]
		if e.Succ < 0 || int(e.Succ) >= len(index) || e.PredSlot < 0 {
			add(AllocInvPhi, -1, -1, "an edge names b%d slot %d, which is not in the function", e.Succ, e.PredSlot)
			continue
		}
		for e.PredSlot >= len(index[e.Succ]) {
			index[e.Succ] = append(index[e.Succ], nil)
		}
		if index[e.Succ][e.PredSlot] != nil {
			add(AllocInvPhi, e.Succ, -1, "two copy sequences on the edge from slot %d", e.PredSlot)
			continue
		}
		index[e.Succ][e.PredSlot] = e
	}

	for _, s := range an.order {
		for j, p := range s.Preds {
			var e *EdgeCopies
			if int(s.ID) < len(index) && j < len(index[s.ID]) {
				e = index[s.ID][j]
			}
			if e != nil {
				if e.Pred != p.ID {
					add(AllocInvPhi, s.ID, -1, "the copies for predecessor slot %d name b%d and the predecessor is b%d", j, e.Pred, p.ID)
				}
				if e.AtPredEnd && len(p.Succs) != 1 {
					add(AllocInvPhi, s.ID, -1, "copies sit at the end of b%d, which has %d successors", p.ID, len(p.Succs))
				}
				if !e.AtPredEnd && len(s.Preds) != 1 {
					add(AllocInvPhi, s.ID, -1, "copies sit at the start of b%d, which has %d predecessors", s.ID, len(s.Preds))
				}
				for _, c := range e.Copies {
					if c.Dst.Kind == LocReg && c.Src.Kind == LocReg &&
						t.RegClassOf(c.Dst.Reg) != t.RegClassOf(c.Src.Reg) {
						add(AllocInvPhi, s.ID, c.Value, "a copy moves %s to %s across register classes",
							t.RegName(c.Src.Reg), t.RegName(c.Dst.Reg))
					}
					// An edge carries the phis of its successor and the
					// temporaries a cycle needs, and nothing else. A copy that
					// writes anywhere else destroys a value the block after it
					// still expects to find there.
					if c.Dst.Kind == LocReg && t.IsScratch(c.Dst.Reg) {
						continue
					}
					if !isPhiHome(s, a, an, c.Dst) {
						add(AllocInvPhi, s.ID, c.Value, "a copy writes %v, which is no phi of b%d and no temporary",
							t.LocString(c.Dst), s.ID)
					}
				}
			}
			replayEdge(s, j, e, a, an, add)
		}
	}
}

// isPhiHome reports whether l is where a phi of s lives.
func isPhiHome(s *Block, a *Alloc, an *regAnalysis, l Loc) bool {
	for _, phi := range s.Values {
		if phi.Op != OpPhi {
			break
		}
		if an.tracked[phi.ID] && a.Home[phi.ID] == l {
			return true
		}
	}
	return false
}

// replayEdge runs one edge's copies over a model of the machine and checks the
// phis of s hold the right values at the end.
func replayEdge(s *Block, j int, e *EdgeCopies, a *Alloc, an *regAnalysis, add addViolation) {
	t := a.Target
	hasPhi := false
	for _, phi := range s.Values {
		if phi.Op != OpPhi {
			break
		}
		if an.tracked[phi.ID] {
			hasPhi = true
		}
	}
	if !hasPhi {
		if e != nil && len(e.Copies) > 0 {
			add(AllocInvPhi, s.ID, -1, "b%d has no phi and the edge from slot %d carries %d copies", s.ID, j, len(e.Copies))
		}
		return
	}

	regs := make([]ID, len(t.Regs))
	for i := range regs {
		regs[i] = -1
	}
	slots := make([]ID, len(a.Slots))
	for i := range slots {
		slots[i] = -1
	}
	get := func(l Loc) ID {
		switch l.Kind {
		case LocReg:
			if l.Reg >= 0 && int(l.Reg) < len(regs) {
				return regs[l.Reg]
			}
		case LocSlot:
			if l.Slot >= 0 && int(l.Slot) < len(slots) {
				return slots[l.Slot]
			}
		}
		return -1
	}
	set := func(l Loc, id ID) {
		switch l.Kind {
		case LocReg:
			if l.Reg >= 0 && int(l.Reg) < len(regs) {
				regs[l.Reg] = id
			}
		case LocSlot:
			if l.Slot >= 0 && int(l.Slot) < len(slots) {
				slots[l.Slot] = id
			}
		}
	}

	// The state at the end of the predecessor: every value that arrives on
	// this edge sits in its own home.
	for _, phi := range s.Values {
		if phi.Op != OpPhi {
			break
		}
		if j >= len(phi.Args) {
			add(AllocInvPhi, s.ID, phi.ID, "the phi has %d arguments and the block has %d predecessors", len(phi.Args), len(s.Preds))
			return
		}
		arg := phi.Args[j]
		if arg == nil || !an.tracked[arg.ID] {
			continue
		}
		set(a.Home[arg.ID], arg.ID)
	}

	if e != nil {
		for _, c := range e.Copies {
			got := c.Value
			if c.Src.Kind != LocNone {
				got = get(c.Src)
				if got != c.Value {
					// The source no longer holds what the copy was written to
					// move. A sequence that does this has the swap problem.
					add(AllocInvEdge, s.ID, c.Value, "a copy reads %v expecting v%d and finds v%d",
						t.LocString(c.Src), c.Value, got)
				}
			}
			set(c.Dst, got)
		}
	}

	for _, phi := range s.Values {
		if phi.Op != OpPhi {
			break
		}
		if !an.tracked[phi.ID] {
			continue
		}
		arg := phi.Args[j]
		if arg == nil || !an.tracked[arg.ID] && !an.remat[arg.ID] {
			continue
		}
		if got := get(a.Home[phi.ID]); got != arg.ID {
			add(AllocInvEdge, s.ID, phi.ID, "after the copies of predecessor slot %d, %v holds v%d and the phi takes v%d there",
				j, t.LocString(a.Home[phi.ID]), got, arg.ID)
		}
	}
}
