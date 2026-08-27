// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// The allocator is tested against a target invented here, not against arm64.
//
// specs/025-lowering-and-rules.md's machine operations do not exist yet, and
// waiting for them would test two things at once. A target is data and five
// predicates, so a test can describe the machine it wants: four integer
// registers is enough to force spilling in ten lines, where arm64's
// twenty-three needs a generated corpus.
//
// The operations are the target-neutral ones of op.go standing in for machine
// operations. The allocator never asks what an operation is; it asks the
// target whether it is a call, whether it can be recomputed, and whether it
// overwrites its first source.

// The register file of the test target.
const (
	raR0 Reg = iota // allocatable, integer
	raR1
	raR2
	raR3
	raS0 // scratch, integer
	raS1
	raF0 // allocatable, floating point
	raF1
	raFS0 // scratch, floating point
	raNumReg
)

// raTarget returns a target with four integer registers and two
// floating-point ones.
func raTarget() *Target {
	t := &Target{Name: "test"}
	t.Regs = make([]RegInfo, raNumReg)
	for r := raR0; r < raNumReg; r++ {
		c := ClassInt
		if r >= raF0 {
			c = ClassFloat
		}
		t.Regs[r] = RegInfo{Name: [raNumReg]string{
			raR0: "r0", raR1: "r1", raR2: "r2", raR3: "r3",
			raS0: "s0", raS1: "s1", raF0: "f0", raF1: "f1", raFS0: "fs0",
		}[r], Class: c}
	}
	for _, r := range []Reg{raR0, raR1, raR2, raR3} {
		t.Allocatable[ClassInt] = t.Allocatable[ClassInt].Add(r)
	}
	for _, r := range []Reg{raF0, raF1} {
		t.Allocatable[ClassFloat] = t.Allocatable[ClassFloat].Add(r)
	}
	t.Scratch[ClassInt] = []Reg{raS0, raS1}
	t.Scratch[ClassFloat] = []Reg{raFS0}
	t.ArgRegs[ClassInt] = []Reg{raR0, raR1}
	t.ResultRegs[ClassInt] = []Reg{raR0}
	t.ArgRegs[ClassFloat] = []Reg{raF0}
	t.ResultRegs[ClassFloat] = []Reg{raF0}
	for c := RegClass(0); c < NumRegClass; c++ {
		t.Clobbers = t.Clobbers.Union(t.Allocatable[c])
		for _, r := range t.Scratch[c] {
			t.Clobbers = t.Clobbers.Add(r)
		}
	}
	t.ClassOf = ClassOfType
	t.IsCall = func(v *Value) bool { return v.Op.IsCall() }
	t.Remat = Rematerialisable
	t.TwoAddress = func(v *Value) bool { return false }
	t.DefReg = func(v *Value) (Reg, bool) { return NoReg, false }
	t.UseReg = func(v *Value, i int) (Reg, bool) { return NoReg, false }
	return t
}

// raFunc returns a function with an entry block that creates memory.
func raFunc(name string) (*Func, *Block, *Value) {
	f := NewFunc(name)
	e := f.Entry
	e.Kind = BlockRet
	mem := e.NewValue(0, OpInitMem, MemType)
	return f, e, mem
}

// raRet ends the block with a return of results and memory. MakeResult takes
// the memory last, which is the invariant verify.go checks.
func raRet(b *Block, mem *Value, results ...*Value) {
	if b.Control != nil {
		b.removeValue(b.Control)
	}
	args := append(append([]*Value{}, results...), mem)
	b.Control = b.NewValue(0, OpMakeResult, MemType, args...)
}

// raAllocate allocates and fails the test if the function does not verify
// before or the allocation does not verify after.
func raAllocate(t *testing.T, f *Func, tg *Target) *Alloc {
	t.Helper()
	if err := Check(f); err != nil {
		t.Fatalf("the function does not verify before allocation: %v", err)
	}
	a, err := Allocate(f, tg)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := CheckAllocation(f, a); err != nil {
		t.Fatalf("%v\n%s%s", err, f, a)
	}
	return a
}

func TestAllocateStraightLine(t *testing.T) {
	f, e, mem := raFunc("straight")
	a1 := e.NewValue(0, OpArg, tInt)
	a2 := e.NewValue(0, OpArg, tInt)
	sum := e.NewValue(0, OpAdd, tInt, a1, a2)
	prod := e.NewValue(0, OpMul, tInt, sum, a1)
	raRet(e, mem, prod)

	al := raAllocate(t, f, raTarget())
	for _, v := range []*Value{a1, a2, sum, prod} {
		if al.Home[v.ID].Kind != LocReg {
			t.Errorf("v%d has home %v, want a register", v.ID, al.Home[v.ID])
		}
	}
	if len(al.Slots) != 0 {
		t.Errorf("the allocation used %d slots and the function fits in registers", len(al.Slots))
	}
	if al.Args[sum.ID][0] != al.Home[a1.ID].Reg {
		t.Errorf("the sum reads its first operand from %v and it lives in %v",
			al.Args[sum.ID][0], al.Home[a1.ID])
	}
}

// raCall adds a call that takes and produces memory.
func raCall(b *Block, mem *Value) *Value {
	return b.NewValue(0, OpStaticCall, MemType, mem)
}

func TestValueLiveAcrossACallIsSpilled(t *testing.T) {
	// specs/026-register-allocation.md: not a decision the allocator makes.
	// Go's ABI destroys every register at a call, so a value that spans one
	// has to be in the frame.
	f, e, mem := raFunc("across")
	a := e.NewValue(0, OpArg, tInt)
	m2 := raCall(e, mem)
	use := e.NewValue(0, OpAdd, tInt, a, a)
	raRet(e, m2, use)

	al := raAllocate(t, f, raTarget())
	if al.Home[a.ID].Kind != LocSlot {
		t.Errorf("v%d lives across a call and its home is %v, want a slot", a.ID, al.Home[a.ID])
	}
	if al.Home[use.ID].Kind != LocReg {
		t.Errorf("v%d does not live across a call and its home is %v, want a register", use.ID, al.Home[use.ID])
	}
	// The use reads the value out of the frame, so it names a scratch
	// register and not the home.
	tg := al.Target
	if r := al.Args[use.ID][0]; !tg.IsScratch(r) {
		t.Errorf("the use reads the spilled value from %s, want a scratch register", tg.RegName(r))
	}
	if al.Args[use.ID][0] != al.Args[use.ID][1] {
		t.Errorf("one value read twice uses two scratch registers, %v and %v",
			al.Args[use.ID][0], al.Args[use.ID][1])
	}
}

func TestRematerialisedValueIsNeverSpilled(t *testing.T) {
	// A constant is recomputed at each use, so it is not in the frame and not
	// in a register, and a call does not touch it.
	f, e, mem := raFunc("remat")
	c := e.NewValue(0, OpConstInt, tInt)
	c.AuxInt = 7
	g := &ir.Object{Name: "g", Type: tInt, Class: ir.ClassGlobal}
	addr := e.NewValue(0, OpAddr, tIntPtr)
	addr.Aux = g
	m2 := raCall(e, mem)
	use := e.NewValue(0, OpAdd, tInt, c, c)
	load := e.NewValue(0, OpLoad, tInt, addr, m2)
	sum := e.NewValue(0, OpAdd, tInt, use, load)
	raRet(e, m2, sum)

	al := raAllocate(t, f, raTarget())
	for _, v := range []*Value{c, addr} {
		if !al.Remat[v.ID] {
			t.Errorf("v%d (%v) is not marked rematerialisable", v.ID, v.Op)
		}
		if al.Home[v.ID].Kind != LocNone {
			t.Errorf("v%d is rematerialisable and has home %v", v.ID, al.Home[v.ID])
		}
	}
	if len(al.Slots) != 0 {
		t.Errorf("the allocation used %d slots and every value crossing the call is rematerialisable", len(al.Slots))
	}
	if r := al.Args[use.ID][0]; !al.Target.IsScratch(r) {
		t.Errorf("a rematerialised operand is read from %s, want a scratch register", al.Target.RegName(r))
	}
}

func TestFloatAndIntegerHaveSeparateFreeLists(t *testing.T) {
	// specs/026-register-allocation.md: a value belongs to a class by its
	// type, and there is no case in Go where it could go in either.
	f, e, mem := raFunc("classes")
	i1 := e.NewValue(0, OpArg, tInt)
	i2 := e.NewValue(0, OpArg, tInt)
	x1 := e.NewValue(0, OpArg, tFloat)
	x2 := e.NewValue(0, OpArg, tFloat)
	si := e.NewValue(0, OpAdd, tInt, i1, i2)
	sx := e.NewValue(0, OpAdd, tFloat, x1, x2)
	raRet(e, mem, si, sx)

	al := raAllocate(t, f, raTarget())
	tg := al.Target
	for _, v := range []*Value{i1, i2, si} {
		if tg.RegClassOf(al.Home[v.ID].Reg) != ClassInt {
			t.Errorf("the integer v%d lives in %s", v.ID, tg.RegName(al.Home[v.ID].Reg))
		}
	}
	for _, v := range []*Value{x1, x2, sx} {
		if al.Home[v.ID].Kind != LocReg || tg.RegClassOf(al.Home[v.ID].Reg) != ClassFloat {
			t.Errorf("the float v%d lives in %v", v.ID, al.Home[v.ID])
		}
	}
}

// raPressure builds a function with n integer values live at once, consumed by
// a chain of two-operand additions.
//
// The chain matters: an instruction reads at most two values out of the frame,
// which is what the target reserves scratch registers for.
func raPressure(name string, n int) (*Func, []*Value) {
	f, e, mem := raFunc(name)
	args := make([]*Value, n)
	for i := range args {
		args[i] = e.NewValue(0, OpArg, tInt)
	}
	acc := e.NewValue(0, OpAdd, tInt, args[0], args[1])
	for i := 2; i < n; i++ {
		acc = e.NewValue(0, OpAdd, tInt, acc, args[i])
	}
	raRet(e, mem, acc)
	return f, args
}

func TestPressureForcesSpillsAndStaysCorrect(t *testing.T) {
	// The assertion is correctness, not quality: every value has one home, no
	// two live values share a register, and the operands name where they are
	// read from. VerifyAllocation checks all of it.
	for _, n := range []int{4, 8, 16, 40} {
		f, args := raPressure("pressure", n)
		al := raAllocate(t, f, raTarget())
		if n > 4 && len(al.Slots) == 0 {
			t.Errorf("%d live values in 4 registers used no spill slot", n)
		}
		for _, v := range args {
			if al.Home[v.ID].Kind == LocNone {
				t.Errorf("v%d has no home", v.ID)
			}
		}
	}
}

// The parallel copy tests.
//
// A permutation is built by hand, sequentialised, and then replayed over a
// model of the machine. Replaying is what makes the test general: a sequence
// that overwrites a source before another copy reads it produces a wrong final
// state whatever the shape of the permutation, so one assertion covers every
// case, including the ones a hand-checked expected sequence would miss.

// raState is a model of the registers and the slots.
type raState struct {
	at []struct {
		loc Loc
		val ID
	}
}

func (s *raState) get(l Loc) ID {
	for _, e := range s.at {
		if e.loc == l {
			return e.val
		}
	}
	return -1
}

func (s *raState) set(l Loc, v ID) {
	for i := range s.at {
		if s.at[i].loc == l {
			s.at[i].val = v
			return
		}
	}
	s.at = append(s.at, struct {
		loc Loc
		val ID
	}{l, v})
}

// raRunParallel sequentialises the copies, replays the result, and returns the
// sequence and the final state.
func raRunParallel(t *testing.T, copies []Copy) ([]Copy, *raState) {
	t.Helper()
	var temp [NumRegClass]Loc
	temp[ClassInt] = RegLoc(raS0)
	temp[ClassFloat] = RegLoc(raFS0)
	seq, err := ParallelCopy(copies, temp, func(ID) RegClass { return ClassInt })
	if err != nil {
		t.Fatalf("ParallelCopy: %v", err)
	}
	st := &raState{}
	for _, c := range copies {
		if c.Src.Kind != LocNone {
			st.set(c.Src, c.Value)
		}
	}
	for _, c := range seq {
		v := c.Value
		if c.Src.Kind != LocNone {
			v = st.get(c.Src)
		}
		st.set(c.Dst, v)
	}
	for _, c := range copies {
		if got := st.get(c.Dst); got != c.Value {
			t.Errorf("after the sequence %v, %v holds v%d and the parallel copy puts v%d there",
				seq, c.Dst, got, c.Value)
		}
	}
	return seq, st
}

func TestParallelCopyPermutations(t *testing.T) {
	r0, r1, r2, r3 := RegLoc(raR0), RegLoc(raR1), RegLoc(raR2), RegLoc(raR3)
	s0, s1 := SlotLoc(0), SlotLoc(1)
	cases := []struct {
		name   string
		copies []Copy
		want   int // the number of moves, temporaries included
	}{
		{"empty", nil, 0},
		{"one", []Copy{{Dst: r0, Src: r1, Value: 1}}, 1},
		{"chain", []Copy{
			{Dst: r0, Src: r1, Value: 1},
			{Dst: r1, Src: r2, Value: 2},
		}, 2},
		{"reversed chain", []Copy{
			{Dst: r1, Src: r2, Value: 2},
			{Dst: r0, Src: r1, Value: 1},
		}, 2},
		{"cycle of two", []Copy{
			{Dst: r0, Src: r1, Value: 1},
			{Dst: r1, Src: r0, Value: 0},
		}, 3},
		{"cycle of three", []Copy{
			{Dst: r0, Src: r1, Value: 1},
			{Dst: r1, Src: r2, Value: 2},
			{Dst: r2, Src: r0, Value: 0},
		}, 4},
		{"cycle of two and a chain", []Copy{
			{Dst: r0, Src: r1, Value: 1},
			{Dst: r1, Src: r0, Value: 0},
			{Dst: r3, Src: r2, Value: 2},
		}, 4},
		{"two disjoint cycles reuse one temporary", []Copy{
			{Dst: r0, Src: r1, Value: 1},
			{Dst: r1, Src: r0, Value: 0},
			{Dst: r2, Src: r3, Value: 3},
			{Dst: r3, Src: r2, Value: 2},
		}, 6},
		{"a cycle through the frame", []Copy{
			{Dst: s0, Src: s1, Value: 1},
			{Dst: s1, Src: s0, Value: 0},
		}, 3},
		{"one source read twice", []Copy{
			{Dst: r0, Src: r2, Value: 2},
			{Dst: r1, Src: r2, Value: 2},
		}, 2},
		{"a cycle whose source is read twice", []Copy{
			{Dst: r0, Src: r1, Value: 1},
			{Dst: r1, Src: r0, Value: 0},
			{Dst: r2, Src: r0, Value: 0},
		}, 4},
		{"rematerialised sources", []Copy{
			{Dst: r0, Src: Loc{}, Value: 9},
			{Dst: r1, Src: r0, Value: 0},
		}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ParallelCopy must not touch what it is given.
			before := make([]Copy, len(tc.copies))
			copy(before, tc.copies)
			seq, _ := raRunParallel(t, tc.copies)
			for i := range before {
				if before[i] != tc.copies[i] {
					t.Errorf("ParallelCopy changed its input at %d", i)
				}
			}
			if len(seq) != tc.want {
				t.Errorf("the sequence is %d moves and %d were expected:\n%v", len(seq), tc.want, seq)
			}
		})
	}
}

func TestParallelCopyRejectsTwoWritersOfOneLocation(t *testing.T) {
	var temp [NumRegClass]Loc
	temp[ClassInt] = RegLoc(raS0)
	_, err := ParallelCopy([]Copy{
		{Dst: RegLoc(raR0), Src: RegLoc(raR1), Value: 1},
		{Dst: RegLoc(raR0), Src: RegLoc(raR2), Value: 2},
	}, temp, func(ID) RegClass { return ClassInt })
	if err == nil {
		t.Fatal("two copies writing one register were accepted")
	}
	if !strings.Contains(err.Error(), "two parallel copies write") {
		t.Errorf("the error is %q", err)
	}
	_, err = ParallelCopy([]Copy{{Dst: Loc{}, Src: RegLoc(raR1), Value: 1}}, temp, func(ID) RegClass { return ClassInt })
	if err == nil || !strings.Contains(err.Error(), "writes nowhere") {
		t.Errorf("a copy with no destination gave %v", err)
	}
}

func TestPhiBecomesCopiesOnEveryEdge(t *testing.T) {
	f, _, then, els, join, mem := diamond()
	a := then.NewValue(0, OpArg, tInt)
	b := els.NewValue(0, OpArg, tInt)
	phi := join.NewValue(0, OpPhi, tInt, a, b)
	use := join.NewValue(0, OpAdd, tInt, phi, phi)
	raRet(join, mem, use)

	al := raAllocate(t, f, raTarget())
	if len(al.Edges) != 2 {
		t.Fatalf("the join has two predecessors and the allocation has %d copy sequences:\n%s", len(al.Edges), al)
	}
	for _, e := range al.Edges {
		if len(e.Copies) != 1 {
			t.Errorf("the edge b%d -> b%d carries %d copies, want one", e.Pred, e.Succ, len(e.Copies))
		}
		if !e.AtPredEnd {
			t.Errorf("the copies of b%d -> b%d sit at the start of the successor, and the predecessor has one successor",
				e.Pred, e.Succ)
		}
	}
	// The phi has a home and the copies write it. VerifyAllocation replayed
	// them; this asserts the shape a code generator reads.
	if al.Home[phi.ID].Kind == LocNone {
		t.Errorf("the phi has no home")
	}
	if al.Edges[0].Copies[0].Dst != al.Home[phi.ID] {
		t.Errorf("the copy writes %v and the phi lives in %v", al.Edges[0].Copies[0].Dst, al.Home[phi.ID])
	}
}

// raLoop returns a function with a loop and a phi in its header.
//
//	entry -> header
//	header -> body, exit
//	body -> header
func raLoop(name string, extra int) (f *Func, header, body *Block, phi *Value) {
	f, entry, mem := raFunc(name)
	entry.Kind = BlockPlain
	init := entry.NewValue(0, OpArg, tInt)

	header = f.NewBlock(BlockIf)
	body = f.NewBlock(BlockPlain)
	exit := f.NewBlock(BlockRet)
	entry.AddEdgeTo(header)
	header.AddEdgeTo(body)
	header.AddEdgeTo(exit)
	body.AddEdgeTo(header)

	phi = header.NewValue(0, OpPhi, tInt)
	one := header.NewValue(0, OpConstInt, tInt)
	one.AuxInt = 1
	header.Control = header.NewValue(0, OpLess, tBool, phi, one)

	// Values that live for the whole body, to press on the register file.
	live := make([]*Value, extra)
	for i := range live {
		live[i] = body.NewValue(0, OpArg, tInt)
	}
	next := body.NewValue(0, OpAdd, tInt, phi, one)
	for _, v := range live {
		next = body.NewValue(0, OpAdd, tInt, next, v)
	}
	phi.AddArg(init)
	phi.AddArg(next)
	raRet(exit, mem, phi)
	return f, header, body, phi
}

func TestLoopCarriedPhi(t *testing.T) {
	for _, extra := range []int{0, 2, 6} {
		f, _, _, phi := raLoop("loop", extra)
		al := raAllocate(t, f, raTarget())
		if len(al.Edges) != 2 {
			t.Fatalf("extra=%d: the header has two predecessors and the allocation has %d copy sequences:\n%s",
				extra, len(al.Edges), al)
		}
		// The phi's home is written at the end of the body, so nothing that is
		// live in the body may be in it. VerifyAllocation checks that through
		// the live range; this asserts the range itself reaches the back edge.
		if al.Home[phi.ID].Kind == LocNone {
			t.Errorf("extra=%d: the phi has no home", extra)
		}
	}
}

func TestCriticalEdgeIsRefused(t *testing.T) {
	// specs/026-register-allocation.md's lost copy problem. The copies of a
	// phi have nowhere to go on a critical edge, and Allocate does not repair
	// the function, so it refuses it.
	build := func() (*Func, *Value) {
		f, entry, mem := raFunc("critical")
		entry.Kind = BlockIf
		entry.Control = entry.NewValue(0, OpConstBool, tBool)
		a := entry.NewValue(0, OpArg, tInt)
		mid := f.NewBlock(BlockPlain)
		join := f.NewBlock(BlockRet)
		entry.AddEdgeTo(mid)
		entry.AddEdgeTo(join)
		mid.AddEdgeTo(join)
		b := mid.NewValue(0, OpArg, tInt)
		phi := join.NewValue(0, OpPhi, tInt, a, b)
		raRet(join, mem, phi)
		return f, phi
	}
	f, _ := build()
	if err := Check(f); err != nil {
		t.Fatalf("the function does not verify: %v", err)
	}
	if _, err := Allocate(f, raTarget()); err == nil {
		t.Fatal("a function with a critical edge was allocated")
	} else if !strings.Contains(err.Error(), "SplitCriticalEdges") {
		t.Errorf("the error does not name the pass that fixes it: %v", err)
	}

	f, _ = build()
	if n := SplitCriticalEdges(f); n != 1 {
		t.Fatalf("SplitCriticalEdges split %d edges, want 1", n)
	}
	al := raAllocate(t, f, raTarget())
	if len(al.Edges) != 2 {
		t.Errorf("after splitting, the join has %d copy sequences, want 2:\n%s", len(al.Edges), al)
	}
}

func TestAllocationIsDeterministic(t *testing.T) {
	// specs/053-determinism.md. Ten allocations of one function are the same
	// bytes. The dump covers homes, slots, operand registers and every copy,
	// so a difference anywhere in the result shows up here.
	f, _ := raPressure("determinism", 24)
	want := ""
	for i := 0; i < 10; i++ {
		a, err := Allocate(f, raTarget())
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got := a.String()
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("run %d differs from run 0:\n%s\nwant\n%s", i, got, want)
		}
	}

	g, _, _, _ := raLoop("determinism-loop", 8)
	SplitCriticalEdges(g)
	want = ""
	for i := 0; i < 10; i++ {
		a, err := Allocate(g, raTarget())
		if err != nil {
			t.Fatalf("loop run %d: %v", i, err)
		}
		if i == 0 {
			want = a.String()
		} else if a.String() != want {
			t.Fatalf("loop run %d differs from run 0", i)
		}
	}
}

// raShared reports whether two values live in one slot.
func raShared(a *Alloc, u, v *Value) bool {
	hu, hv := a.Home[u.ID], a.Home[v.ID]
	return hu.Kind == LocSlot && hv.Kind == LocSlot && hu.Slot == hv.Slot
}

// raTwoSpills builds a function with two values that are spilled because each
// is live across a call, and whose live ranges do not meet.
func raTwoSpills(name string, secondType *ir.Type) (*Func, *Value, *Value) {
	f, e, mem := raFunc(name)
	a := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	ua := e.NewValue(0, OpAdd, tInt, a, a)
	b := e.NewValue(0, OpArg, secondType)
	m2 := raCall(e, m1)
	ub := e.NewValue(0, OpEq, tBool, b, b)
	keep := e.NewValue(0, OpAdd, tInt, ua, ua)
	raRet(e, m2, keep, ub)
	return f, a, b
}

func TestSpillSlotsAreSharedWhenTheRulesAllow(t *testing.T) {
	f, a, b := raTwoSpills("share", tInt)
	al := raAllocate(t, f, raTarget())
	if al.Home[a.ID].Kind != LocSlot || al.Home[b.ID].Kind != LocSlot {
		t.Fatalf("both values live across a call and their homes are %v and %v",
			al.Home[a.ID], al.Home[b.ID])
	}
	if !raShared(al, a, b) {
		t.Errorf("v%d and v%d have the same layout and disjoint ranges and do not share a slot:\n%s",
			a.ID, b.ID, al)
	}
}

func TestSpillSlotsAreNotSharedAcrossPointerness(t *testing.T) {
	// specs/027-liveness-and-stackmaps.md's constraint. Its liveness is a
	// may-analysis, so a slot that holds a pointer on any path reaching a
	// safepoint is described as a pointer there, and growing a stack rewrites
	// every word the map calls a pointer. An integer sharing that slot would
	// be adjusted by the copy.
	f, a, b := raTwoSpills("noshare", tIntPtr)
	al := raAllocate(t, f, raTarget())
	if al.Home[a.ID].Kind != LocSlot || al.Home[b.ID].Kind != LocSlot {
		t.Fatalf("both values live across a call and their homes are %v and %v",
			al.Home[a.ID], al.Home[b.ID])
	}
	if raShared(al, a, b) {
		t.Errorf("an integer and a pointer share slot %d:\n%s", al.Home[a.ID].Slot, al)
	}
	var ptr, plain int
	for _, s := range al.Slots {
		if s.Ptr {
			ptr++
		} else {
			plain++
		}
	}
	if ptr == 0 || plain == 0 {
		t.Errorf("the slots are %d with pointers and %d without, want at least one of each", ptr, plain)
	}
}

func TestPointerLiveAtASafepoint(t *testing.T) {
	// The second condition of specs/026-register-allocation.md's sharing rule,
	// tested on its own. It is computed from the safepoint live sets rather
	// than from the ranges, so it is a real check the moment ranges gain
	// holes. Today the disjointness condition already implies it.
	f, e, mem := raFunc("safepoint")
	p := e.NewValue(0, OpArg, tIntPtr)
	q := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	up := e.NewValue(0, OpEq, tBool, p, p)
	uq := e.NewValue(0, OpAdd, tInt, q, q)
	raRet(e, m1, uq, up)

	an, err := newAnalysis(f, raTarget())
	if err != nil {
		t.Fatalf("newAnalysis: %v", err)
	}
	if len(an.calls) != 1 {
		t.Fatalf("the function has %d safepoints, want 1", len(an.calls))
	}
	if !an.callLive[0].has(p.ID) || !an.callLive[0].has(q.ID) {
		t.Fatalf("the safepoint live set does not hold both values")
	}
	if !an.pointerLiveAtSafepointIn(p.ID, q.ID) {
		t.Errorf("a pointer live at a safepoint inside the integer's range was not reported")
	}
	if an.pointerLiveAtSafepointIn(q.ID, p.ID) {
		t.Errorf("an integer was reported as a pointer live at a safepoint")
	}
	if an.canShareSlot(p.ID, q.ID) {
		t.Errorf("two values live at the same time may share a slot")
	}
}

func TestWideValueGetsASlotAndNoRegister(t *testing.T) {
	// A string is two words. Lowering decomposes it before allocation; one
	// that arrives whole gets a slot, which is correct and slow, rather than
	// the first word of a register, which is neither.
	f, e, mem := raFunc("wide")
	s := e.NewValue(0, OpArg, tString)
	u := e.NewValue(0, OpEq, tBool, s, s)
	raRet(e, mem, u)

	al := raAllocate(t, f, raTarget())
	if al.Home[s.ID].Kind != LocSlot {
		t.Errorf("a string value has home %v, want a slot", al.Home[s.ID])
	}
	if al.Result[s.ID] != NoReg {
		t.Errorf("a string value writes its result to %v", al.Result[s.ID])
	}
	if al.Args[u.ID][0] != NoReg {
		t.Errorf("a string operand is read into %v", al.Args[u.ID][0])
	}
}

func TestTwoAddressFixups(t *testing.T) {
	// specs/026-register-allocation.md's amd64 property, confined to one pass.
	f, e, mem := raFunc("twoaddr")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	sum := e.NewValue(0, OpAdd, tInt, a, b)
	raRet(e, mem, sum)

	tg := raTarget()
	tg.TwoAddress = func(v *Value) bool { return v.Op == OpAdd }
	al := raAllocate(t, f, tg)
	if len(al.Fixups) != 1 {
		t.Fatalf("the function has one two-address instruction and %d fix-ups:\n%s", len(al.Fixups), al)
	}
	x := al.Fixups[0]
	if x.Value != sum.ID || x.Dst != al.Result[sum.ID] || x.Src != al.Args[sum.ID][0] {
		t.Errorf("the fix-up is %+v and the instruction writes %v and reads %v",
			x, al.Result[sum.ID], al.Args[sum.ID])
	}
	// The destination must not be an operand of the instruction, or the copy
	// would destroy one before it is read.
	for i, r := range al.Args[sum.ID] {
		if i > 0 && r == x.Dst {
			t.Errorf("the fix-up writes %v, which operand %d is read from", x.Dst, i)
		}
	}

	// The three-address target makes no fix-up out of the same function.
	al = raAllocate(t, f, raTarget())
	if len(al.Fixups) != 0 {
		t.Errorf("a three-address target produced %d fix-ups", len(al.Fixups))
	}
}

func TestPreColouredDefinitionsAndUses(t *testing.T) {
	// specs/030-abi.md fixes where an argument and a result live. The ABI
	// assignment pass does not exist yet, so the policy is supplied here and
	// the allocator is what is under test.
	f, e, mem := raFunc("abi")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	sum := e.NewValue(0, OpAdd, tInt, a, b)
	raRet(e, mem, sum)

	tg := raTarget()
	tg.DefReg = func(v *Value) (Reg, bool) {
		switch v {
		case a:
			return raR0, true
		case b:
			return raR1, true
		}
		return NoReg, false
	}
	tg.UseReg = func(v *Value, i int) (Reg, bool) {
		if v.Op == OpMakeResult && i == 0 {
			return raR0, true
		}
		return NoReg, false
	}
	al := raAllocate(t, f, tg)
	if al.Home[a.ID] != RegLoc(raR0) || al.Home[b.ID] != RegLoc(raR1) {
		t.Errorf("the arguments live in %v and %v, want r0 and r1", al.Home[a.ID], al.Home[b.ID])
	}
	if al.Fixed[a.ID] != raR0 {
		t.Errorf("the allocation records %v as the fixed register of v%d", al.Fixed[a.ID], a.ID)
	}
	if r := al.Args[e.Control.ID][0]; r != raR0 {
		t.Errorf("the result is read from %v and the ABI reads it from r0", r)
	}
}

func TestPreColouredArgumentLiveAcrossACallIsSpilled(t *testing.T) {
	// The two rules meet here. The ABI says the value arrives in a register
	// and the call destroys every register, so the value arrives there and
	// goes straight to the frame.
	f, e, mem := raFunc("abicall")
	a := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	u := e.NewValue(0, OpAdd, tInt, a, a)
	raRet(e, m1, u)

	tg := raTarget()
	tg.DefReg = func(v *Value) (Reg, bool) {
		if v == a {
			return raR0, true
		}
		return NoReg, false
	}
	al := raAllocate(t, f, tg)
	if al.Home[a.ID].Kind != LocSlot {
		t.Errorf("the argument lives in %v and it is live across a call", al.Home[a.ID])
	}
	if al.Fixed[a.ID] != raR0 {
		t.Errorf("the allocation lost the register the argument arrives in: %v", al.Fixed[a.ID])
	}
}

// The invariant checker.
//
// verify.go's file comment applies one pass later. Each test breaks an
// allocation in exactly one way and asserts that the invariant it broke is
// reported. An invariant that never fires is worse than none, because it makes
// a test suite look like it covers something.

// raInvariants returns the distinct invariants reported, in report order.
func raInvariants(vs []AllocViolation) []AllocInvariant {
	var out []AllocInvariant
	for _, v := range vs {
		found := false
		for _, o := range out {
			if o == v.Invariant {
				found = true
			}
		}
		if !found {
			out = append(out, v.Invariant)
		}
	}
	return out
}

// raWantOnly asserts that the violations are non-empty and name inv and
// nothing else.
func raWantOnly(t *testing.T, f *Func, a *Alloc, inv AllocInvariant) {
	t.Helper()
	vs := VerifyAllocation(f, a)
	got := raInvariants(vs)
	if len(got) != 1 || got[0] != inv {
		t.Errorf("the violations are %v, want only %q:\n%v\n%s", got, inv, vs, a)
	}
}

// raRegisterHome moves a value to a register and keeps every operand that
// reads it in step, so that only the invariant under test is broken.
func raRegisterHome(f *Func, a *Alloc, v *Value, r Reg) {
	a.Home[v.ID] = RegLoc(r)
	a.Result[v.ID] = r
	for _, b := range f.Blocks {
		for _, u := range b.Values {
			for i, arg := range u.Args {
				if arg == v {
					a.Args[u.ID][i] = r
				}
			}
		}
	}
}

func TestVerifyAllocationCatchesAMissingHome(t *testing.T) {
	f, e, mem := raFunc("nohome")
	a := e.NewValue(0, OpArg, tInt)
	u := e.NewValue(0, OpAdd, tInt, a, a)
	raRet(e, mem, u)
	al := raAllocate(t, f, raTarget())

	// The value occupies storage and is given none. Its uses are made to read
	// a scratch register, which is what a spilled value looks like, so the
	// operand invariant stays satisfied.
	al.Home[a.ID] = Loc{}
	al.Result[a.ID] = NoReg
	al.Args[u.ID][0], al.Args[u.ID][1] = raS0, raS0
	raWantOnly(t, f, al, AllocInvHome)
}

func TestVerifyAllocationCatchesAScratchRegisterHome(t *testing.T) {
	f, e, mem := raFunc("scratchhome")
	a := e.NewValue(0, OpArg, tInt)
	u := e.NewValue(0, OpAdd, tInt, a, a)
	raRet(e, mem, u)
	al := raAllocate(t, f, raTarget())

	// A scratch register is not allocatable, so both halves of the invariant
	// fire and no other one does.
	raRegisterHome(f, al, a, raS0)
	raWantOnly(t, f, al, AllocInvReserved)
}

func TestVerifyAllocationCatchesAWrongClass(t *testing.T) {
	f, e, mem := raFunc("class")
	x := e.NewValue(0, OpArg, tFloat)
	u := e.NewValue(0, OpAdd, tFloat, x, x)
	raRet(e, mem, u)
	al := raAllocate(t, f, raTarget())

	raRegisterHome(f, al, x, raR0)
	raWantOnly(t, f, al, AllocInvClass)
}

func TestVerifyAllocationCatchesARegisterHeldAcrossACall(t *testing.T) {
	// The invariant Go's ABI makes unconditional. No register survives a call,
	// so a value in one across a call is a value the callee destroys.
	f, e, mem := raFunc("acrosscall")
	a := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	u := e.NewValue(0, OpAdd, tInt, a, a)
	raRet(e, m1, u)
	al := raAllocate(t, f, raTarget())

	// The value was spilled. Put it back in a register, empty the slot table
	// so the slot invariant stays satisfied, and check that the call rule
	// alone fires.
	raRegisterHome(f, al, a, raR3)
	al.Slots = nil
	raWantOnly(t, f, al, AllocInvCall)
}

func TestVerifyAllocationCatchesTwoLiveValuesInOneRegister(t *testing.T) {
	f, e, mem := raFunc("overlap")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	u := e.NewValue(0, OpAdd, tInt, a, b)
	raRet(e, mem, u)
	al := raAllocate(t, f, raTarget())

	raRegisterHome(f, al, b, al.Home[a.ID].Reg)
	raWantOnly(t, f, al, AllocInvOverlap)
}

func TestVerifyAllocationCatchesAWrongOperandRegister(t *testing.T) {
	f, e, mem := raFunc("operand")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	u := e.NewValue(0, OpAdd, tInt, a, b)
	raRet(e, mem, u)
	al := raAllocate(t, f, raTarget())

	// The instruction reads a register the value does not live in.
	al.Args[u.ID][0] = raR3
	if al.Home[a.ID].Reg == raR3 {
		al.Args[u.ID][0] = raR2
	}
	raWantOnly(t, f, al, AllocInvOperand)
}

func TestVerifyAllocationCatchesTwoOperandsInOneScratchRegister(t *testing.T) {
	// Two values read out of the frame into one register leaves the
	// instruction reading one of them twice.
	f, e, mem := raFunc("scratchclash")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	u := e.NewValue(0, OpAdd, tInt, a, b)
	raRet(e, m1, u)
	al := raAllocate(t, f, raTarget())
	if al.Home[a.ID].Kind != LocSlot || al.Home[b.ID].Kind != LocSlot {
		t.Fatalf("both values live across the call and their homes are %v and %v", al.Home[a.ID], al.Home[b.ID])
	}
	al.Args[u.ID][1] = al.Args[u.ID][0]
	raWantOnly(t, f, al, AllocInvOperand)
}

func TestVerifyAllocationCatchesAnIllegalSlotShare(t *testing.T) {
	f, e, mem := raFunc("slotshare")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	u := e.NewValue(0, OpAdd, tInt, a, b)
	raRet(e, m1, u)
	al := raAllocate(t, f, raTarget())

	// Both values are spilled and both are live across the call, so their
	// ranges meet and the slots may not be merged. Merge them anyway.
	if len(al.Slots) != 2 {
		t.Fatalf("the function needs two slots and has %d:\n%s", len(al.Slots), al)
	}
	al.Slots = al.Slots[:1]
	al.Slots[0].Values = []ID{a.ID, b.ID}
	al.Home[b.ID] = SlotLoc(0)
	raWantOnly(t, f, al, AllocInvSlot)
}

func TestVerifyAllocationCatchesTheSwapProblem(t *testing.T) {
	// The copies on an edge are a permutation. Emitting them in sequence
	// overwrites a source, and the replay is what notices.
	f, _, then, els, join, mem := diamond()
	a := then.NewValue(0, OpArg, tInt)
	b := then.NewValue(0, OpArg, tInt)
	c := els.NewValue(0, OpArg, tInt)
	d := els.NewValue(0, OpArg, tInt)
	p1 := join.NewValue(0, OpPhi, tInt, a, c)
	p2 := join.NewValue(0, OpPhi, tInt, b, d)
	u := join.NewValue(0, OpAdd, tInt, p1, p2)
	raRet(join, mem, u)
	al := raAllocate(t, f, raTarget())

	// Build the permutation that exchanges the two phis on the first edge and
	// emit it in sequence, which is the mistake ParallelCopy exists to avoid.
	e := &al.Edges[0]
	h1, h2 := al.Home[p1.ID], al.Home[p2.ID]
	e.Copies = []Copy{
		{Dst: h1, Src: h2, Value: p2.ID},
		{Dst: h2, Src: h1, Value: p1.ID},
	}
	vs := VerifyAllocation(f, al)
	found := false
	for _, v := range vs {
		if v.Invariant == AllocInvEdge {
			found = true
		}
	}
	if !found {
		t.Errorf("a sequence that overwrites its own source was accepted: %v\n%s", vs, al)
	}

	// The correctness of the sequentialised form is asserted by
	// TestParallelCopyPermutations, which replays it against the parallel
	// assignment it came from. Replaying it here would need the state of every
	// register at the edge, which the phis alone do not give.
}

func TestVerifyAllocationCatchesCopiesInTheWrongPlace(t *testing.T) {
	f, _, then, els, join, mem := diamond()
	a := then.NewValue(0, OpArg, tInt)
	b := els.NewValue(0, OpArg, tInt)
	phi := join.NewValue(0, OpPhi, tInt, a, b)
	u := join.NewValue(0, OpAdd, tInt, phi, phi)
	raRet(join, mem, u)
	al := raAllocate(t, f, raTarget())

	al.Edges[0].AtPredEnd = false
	raWantOnly(t, f, al, AllocInvPhi)
}

func TestVerifyAllocationCatchesAnUnresolvedPhi(t *testing.T) {
	f, _, then, els, join, mem := diamond()
	a := then.NewValue(0, OpArg, tInt)
	b := els.NewValue(0, OpArg, tInt)
	phi := join.NewValue(0, OpPhi, tInt, a, b)
	u := join.NewValue(0, OpAdd, tInt, phi, phi)
	raRet(join, mem, u)
	al := raAllocate(t, f, raTarget())

	// Drop the copies of one edge. The phi's home then holds nothing when the
	// edge is taken.
	al.Edges = al.Edges[1:]
	raWantOnly(t, f, al, AllocInvEdge)
}

func TestVerifyAllocationRejectsWhatItCannotCheck(t *testing.T) {
	f, e, mem := raFunc("nothing")
	raRet(e, mem)
	if vs := VerifyAllocation(f, nil); len(vs) != 1 || vs[0].Invariant != AllocInvNone {
		t.Errorf("verifying no allocation gave %v", vs)
	}
	al := raAllocate(t, f, raTarget())
	// A function the analysis cannot run on is reported once, as not an
	// invariant, rather than as every invariant at once.
	g, ge, gmem := raFunc("unreachable")
	raRet(ge, gmem)
	dead := g.NewBlock(BlockRet)
	dead.Control = dead.NewValue(0, OpMakeResult, MemType, gmem)
	if vs := VerifyAllocation(g, al); len(vs) != 1 || vs[0].Invariant != AllocInvNone {
		t.Errorf("verifying a function with an unreachable block gave %v", vs)
	}
	// A short home table is a mismatch between the function and the
	// allocation, not a broken invariant of either.
	short := &Alloc{Target: raTarget(), Func: f}
	if vs := VerifyAllocation(f, short); len(vs) != 1 || vs[0].Invariant != AllocInvHome {
		t.Errorf("verifying an allocation of the wrong size gave %v", vs)
	}
}

func TestAllocateRefusesWhatItCannotAllocate(t *testing.T) {
	if _, err := Allocate(nil, raTarget()); err == nil {
		t.Error("a nil function was allocated")
	}
	f, e, mem := raFunc("bad")
	raRet(e, mem)
	if _, err := Allocate(f, &Target{Name: "empty"}); err == nil {
		t.Error("a target that describes nothing was accepted")
	}
	// An unreachable block has no place in the linearisation and no liveness,
	// so the allocator refuses the function rather than allocating part of it.
	dead := f.NewBlock(BlockRet)
	dead.Control = dead.NewValue(0, OpMakeResult, MemType, mem)
	if _, err := Allocate(f, raTarget()); err == nil {
		t.Error("a function with an unreachable block was allocated")
	} else if !strings.Contains(err.Error(), "reachable") {
		t.Errorf("the error is %q", err)
	}
}

func TestScratchRegistersAreABound(t *testing.T) {
	// An instruction reads at most as many values out of the frame as the
	// target reserves registers to read them into. The error names the
	// operation and the counts, because the fix is to reserve one more.
	f, e, mem := raFunc("scratch")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	c := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	raRet(e, m1, a, b, c)

	_, err := Allocate(f, raTarget())
	var se *ScratchError
	if err == nil {
		t.Fatal("three spilled operands were allocated with two scratch registers")
	}
	se, ok := err.(*ScratchError)
	if !ok {
		t.Fatalf("the error is %T: %v", err, err)
	}
	if se.Need != 3 || se.Have != 2 || se.Class != ClassInt {
		t.Errorf("the error reports need=%d have=%d class=%v", se.Need, se.Have, se.Class)
	}
	if !strings.Contains(se.Error(), "MakeResult") || !strings.Contains(se.Error(), "scratch registers") {
		t.Errorf("the error does not name the operation: %v", se)
	}

	// One more scratch register and the same function allocates.
	tg := raTarget()
	tg.Regs = append(tg.Regs, RegInfo{Name: "s2", Class: ClassInt})
	tg.Scratch[ClassInt] = append(tg.Scratch[ClassInt], Reg(len(tg.Regs)-1))
	tg.Clobbers = tg.Clobbers.Add(Reg(len(tg.Regs) - 1))
	raAllocate(t, f, tg)
}

func TestAllocateRefusesAnImpossibleABI(t *testing.T) {
	build := func() (*Func, *Value, *Value) {
		f, e, mem := raFunc("abi")
		a := e.NewValue(0, OpArg, tInt)
		b := e.NewValue(0, OpArg, tInt)
		u := e.NewValue(0, OpAdd, tInt, a, b)
		raRet(e, mem, u)
		return f, a, b
	}

	f, a, _ := build()
	tg := raTarget()
	tg.DefReg = func(v *Value) (Reg, bool) {
		if v == a {
			return raS0, true // a scratch register is not allocatable
		}
		return NoReg, false
	}
	if _, err := Allocate(f, tg); err == nil || !strings.Contains(err.Error(), "not allocatable") {
		t.Errorf("fixing a value to a scratch register gave %v", err)
	}

	f, a, _ = build()
	tg = raTarget()
	tg.DefReg = func(v *Value) (Reg, bool) {
		if v == a {
			return raF0, true // a float register for an integer value
		}
		return NoReg, false
	}
	if _, err := Allocate(f, tg); err == nil || !strings.Contains(err.Error(), "class") {
		t.Errorf("fixing an integer value to a float register gave %v", err)
	}

	f, a, b := build()
	tg = raTarget()
	tg.DefReg = func(v *Value) (Reg, bool) {
		if v == a || v == b {
			return raR0, true // two values that are live at once, one register
		}
		return NoReg, false
	}
	if _, err := Allocate(f, tg); err == nil || !strings.Contains(err.Error(), "live at the same time") {
		t.Errorf("fixing two live values to one register gave %v", err)
	}
}

func TestAllocateRefusesAWideRematerialisation(t *testing.T) {
	// A target that calls a two-word value rematerialisable is describing
	// something that cannot happen: it would be recomputed into one register.
	f, e, mem := raFunc("widremat")
	s := e.NewValue(0, OpConstString, tString)
	s.Aux = "text"
	u := e.NewValue(0, OpEq, tBool, s, s)
	raRet(e, mem, u)

	tg := raTarget()
	tg.Remat = func(v *Value) bool { return v.Op == OpConstString }
	if _, err := Allocate(f, tg); err == nil || !strings.Contains(err.Error(), "does not fit one register") {
		t.Errorf("a wide rematerialisable value gave %v", err)
	}
}

func TestAllocErrorMessages(t *testing.T) {
	e := &AllocError{Func: "f", Detail: "detail"}
	if got := e.Error(); got != "ssa: regalloc: f: detail" {
		t.Errorf("the error prints as %q", got)
	}
	v := AllocViolation{Invariant: AllocInvHome, Block: 1, Value: 2, Detail: "d"}
	if got := v.String(); !strings.Contains(got, "b1 v2") {
		t.Errorf("the violation prints as %q", got)
	}
	v = AllocViolation{Invariant: AllocInvNone, Block: -1, Value: -1, Detail: "d"}
	if got := v.String(); !strings.HasPrefix(got, "none: ") {
		t.Errorf("a violation about nothing prints as %q", got)
	}
	if got := AllocInvariant(200).String(); got != "allocinvariant(?)" {
		t.Errorf("an unknown invariant prints as %q", got)
	}
	if got := AllocInvOverlap.String(); got == "" {
		t.Error("an invariant has no name")
	}
}

// raPhiFunc returns the diamond with one phi, allocated.
func raPhiFunc(t *testing.T) (*Func, *Alloc, *Value) {
	t.Helper()
	f, _, then, els, join, mem := diamond()
	a := then.NewValue(0, OpArg, tInt)
	b := els.NewValue(0, OpArg, tInt)
	phi := join.NewValue(0, OpPhi, tInt, a, b)
	u := join.NewValue(0, OpAdd, tInt, phi, phi)
	raRet(join, mem, u)
	return f, raAllocate(t, f, raTarget()), phi
}

func TestVerifyAllocationCatchesABrokenEdgeTable(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(f *Func, a *Alloc, phi *Value)
		want   AllocInvariant
	}{
		{"an edge names a block that is not there", func(f *Func, a *Alloc, phi *Value) {
			a.Edges[0].Succ = ID(f.NumBlocks() + 3)
		}, AllocInvPhi},
		{"an edge names no block at all", func(f *Func, a *Alloc, phi *Value) {
			a.Edges[0].Succ = -1
		}, AllocInvPhi},
		{"an edge names no predecessor slot", func(f *Func, a *Alloc, phi *Value) {
			a.Edges[0].PredSlot = -1
		}, AllocInvPhi},
		{"two sequences on one edge", func(f *Func, a *Alloc, phi *Value) {
			a.Edges = append(a.Edges, a.Edges[0])
		}, AllocInvPhi},
		{"an edge names the wrong predecessor", func(f *Func, a *Alloc, phi *Value) {
			a.Edges[0].Pred = a.Edges[1].Pred
		}, AllocInvPhi},
		{"a copy crosses register classes", func(f *Func, a *Alloc, phi *Value) {
			a.Edges[0].Copies[0].Src = RegLoc(raF0)
		}, AllocInvPhi},
		{"a copy writes somewhere no phi lives", func(f *Func, a *Alloc, phi *Value) {
			dst := RegLoc(raR3)
			if a.Home[phi.ID] == dst {
				dst = RegLoc(raR2)
			}
			a.Edges[0].Copies = append(a.Edges[0].Copies, Copy{Dst: dst, Src: RegLoc(raR0), Value: phi.ID})
		}, AllocInvPhi},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, al, phi := raPhiFunc(t)
			tc.break_(f, al, phi)
			found := false
			for _, v := range VerifyAllocation(f, al) {
				if v.Invariant == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("the violations are %v, want %v", VerifyAllocation(f, al), tc.want)
			}
		})
	}
}

func TestVerifyAllocationCatchesCopiesWithNoPhi(t *testing.T) {
	f, e, mem := raFunc("nophi")
	entry := e
	entry.Kind = BlockPlain
	a := entry.NewValue(0, OpArg, tInt)
	next := f.NewBlock(BlockRet)
	entry.AddEdgeTo(next)
	raRet(next, mem, a)
	al := raAllocate(t, f, raTarget())

	al.Edges = append(al.Edges, EdgeCopies{
		Pred: entry.ID, Succ: next.ID, PredSlot: 0, AtPredEnd: true,
		Copies: []Copy{{Dst: RegLoc(raR3), Src: RegLoc(raR2), Value: a.ID}},
	})
	raWantOnly(t, f, al, AllocInvPhi)
}

func TestVerifyAllocationCatchesABrokenHomeTable(t *testing.T) {
	f, e, mem := raFunc("homes")
	c := e.NewValue(0, OpConstInt, tInt)
	c.AuxInt = 3
	void := e.NewValue(0, OpArg, tVoid)
	a := e.NewValue(0, OpArg, tInt)
	u := e.NewValue(0, OpAdd, tInt, a, c)
	raRet(e, mem, u)
	_ = void

	cases := []struct {
		name   string
		break_ func(a *Alloc)
	}{
		{"a memory value has a home", func(al *Alloc) { al.Home[mem.ID] = RegLoc(raR3) }},
		{"a rematerialised value has a home", func(al *Alloc) { al.Home[c.ID] = RegLoc(raR3) }},
		{"a rematerialised value is not marked", func(al *Alloc) { al.Remat[c.ID] = false }},
		{"a value of no width has a home", func(al *Alloc) { al.Home[void.ID] = RegLoc(raR3) }},
		{"a home names a slot that is not there", func(al *Alloc) { al.Home[a.ID] = SlotLoc(4) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			al := raAllocate(t, f, raTarget())
			tc.break_(al)
			found := false
			for _, v := range VerifyAllocation(f, al) {
				if v.Invariant == AllocInvHome {
					found = true
				}
			}
			if !found {
				t.Errorf("the violations are %v", VerifyAllocation(f, al))
			}
		})
	}
}

func TestVerifyAllocationCatchesABrokenSlotTable(t *testing.T) {
	f, e, mem := raFunc("slots")
	a := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	u := e.NewValue(0, OpAdd, tInt, a, a)
	raRet(e, m1, u)

	cases := []struct {
		name   string
		break_ func(a *Alloc)
	}{
		{"the slot is too small", func(al *Alloc) { al.Slots[0].Size = 1 }},
		{"the slot has the wrong pointer flag", func(al *Alloc) { al.Slots[0].Ptr = !al.Slots[0].Ptr }},
		{"the value is in no slot", func(al *Alloc) { al.Slots[0].Values = nil }},
		{"the slot lists a value that lives elsewhere", func(al *Alloc) {
			al.Slots[0].Values = []ID{a.ID}
			al.Home[a.ID] = RegLoc(raR3)
		}},
		{"the slot lists a value that is not in the function", func(al *Alloc) {
			al.Slots[0].Values = append(al.Slots[0].Values, ID(f.NumValues()+2))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			al := raAllocate(t, f, raTarget())
			tc.break_(al)
			found := false
			for _, v := range VerifyAllocation(f, al) {
				if v.Invariant == AllocInvSlot {
					found = true
				}
			}
			if !found {
				t.Errorf("the violations are %v", VerifyAllocation(f, al))
			}
		})
	}
}

func TestCheckAllocationReportsEveryViolation(t *testing.T) {
	f, e, mem := raFunc("check")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	u := e.NewValue(0, OpAdd, tInt, a, b)
	raRet(e, mem, u)
	al := raAllocate(t, f, raTarget())
	raRegisterHome(f, al, b, al.Home[a.ID].Reg)
	err := CheckAllocation(f, al)
	if err == nil {
		t.Fatal("a broken allocation checked out")
	}
	if !strings.Contains(err.Error(), "1 violations") || !strings.Contains(err.Error(), "live at the same time") {
		t.Errorf("the error is %q", err)
	}
}

func TestRegisterClassOfAValue(t *testing.T) {
	// classOf answers for a rematerialised value as well as a tracked one,
	// which is what ParallelCopy needs to pick a temporary of the right class.
	f, e, mem := raFunc("classof")
	c := e.NewValue(0, OpConstFloat, tFloat)
	x := e.NewValue(0, OpArg, tFloat)
	i := e.NewValue(0, OpArg, tInt)
	u := e.NewValue(0, OpAdd, tFloat, x, c)
	raRet(e, mem, u, i)

	an, err := newAnalysis(f, raTarget())
	if err != nil {
		t.Fatalf("newAnalysis: %v", err)
	}
	if got := an.classOf(c.ID); got != ClassFloat {
		t.Errorf("a rematerialised float is of class %v", got)
	}
	if got := an.classOf(x.ID); got != ClassFloat {
		t.Errorf("a float value is of class %v", got)
	}
	if got := an.classOf(i.ID); got != ClassInt {
		t.Errorf("an integer value is of class %v", got)
	}
}

func TestTheSpilledRangeIsTheOneWithTheFurthestNextUse(t *testing.T) {
	// specs/026-register-allocation.md's third step. Five values in four
	// registers, and the one whose next use is furthest away is the one that
	// goes to the frame.
	f, e, mem := raFunc("furthest")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	c := e.NewValue(0, OpArg, tInt)
	d := e.NewValue(0, OpArg, tInt)
	x := e.NewValue(0, OpArg, tInt)
	// The uses run in the reverse order of the definitions, so at the point
	// where the fifth value needs a register, the next use of a is the
	// furthest away.
	u := e.NewValue(0, OpAdd, tInt, x, d)
	u = e.NewValue(0, OpAdd, tInt, u, c)
	u = e.NewValue(0, OpAdd, tInt, u, b)
	u = e.NewValue(0, OpAdd, tInt, u, a)
	raRet(e, mem, u)

	al := raAllocate(t, f, raTarget())
	if al.Home[a.ID].Kind != LocSlot {
		t.Errorf("the value with the furthest next use lives in %v, want a slot:\n%s",
			al.Target.LocString(al.Home[a.ID]), al)
	}
	for _, v := range []*Value{b, c, d, x} {
		if al.Home[v.ID].Kind != LocReg {
			t.Errorf("v%d lives in %v and its next use is nearer than v%d's",
				v.ID, al.Home[v.ID], a.ID)
		}
	}
}

func TestABlockControlValueIsAUse(t *testing.T) {
	// A value that is only a branch condition is still live at the branch.
	// Missing that would collapse its range to its definition, and two
	// conditions would share a register while both are live.
	f, e, mem := raFunc("control")
	e.Kind = BlockIf
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	c1 := e.NewValue(0, OpEq, tBool, a, b)
	c2 := e.NewValue(0, OpLess, tBool, b, a)
	e.Control = c1

	then := f.NewBlock(BlockIf)
	join := f.NewBlock(BlockRet)
	other := f.NewBlock(BlockRet)
	e.AddEdgeTo(then)
	e.AddEdgeTo(other)
	then.AddEdgeTo(join)
	then.AddEdgeTo(other)
	then.Control = c2
	raRet(join, mem)
	raRet(other, mem)
	SplitCriticalEdges(f)

	al := raAllocate(t, f, raTarget())
	if al.Home[c1.ID].Kind != LocReg || al.Home[c2.ID].Kind != LocReg {
		t.Fatalf("the conditions live in %v and %v:\n%s", al.Home[c1.ID], al.Home[c2.ID], al)
	}
	if al.Home[c1.ID] == al.Home[c2.ID] {
		t.Errorf("two conditions live at once share %v", al.Target.LocString(al.Home[c1.ID]))
	}
}

func TestTwoAddressFixupDoesNotClobberAReloadedOperand(t *testing.T) {
	// The fix-up copy runs before the instruction. If it wrote the register a
	// spilled operand was read into, the instruction would read the
	// destination instead of the operand.
	f, e, mem := raFunc("twoaddrspill")
	a := e.NewValue(0, OpArg, tInt)
	b := e.NewValue(0, OpArg, tInt)
	m1 := raCall(e, mem)
	sum := e.NewValue(0, OpAdd, tInt, a, b)
	m2 := raCall(e, m1)
	keep := e.NewValue(0, OpAdd, tInt, sum, sum)
	raRet(e, m2, keep)

	// Three scratch registers: two operands out of the frame, and one for the
	// result, which the fix-up copy writes before the instruction runs.
	tg := raTarget()
	tg.TwoAddress = func(v *Value) bool { return v.Op == OpAdd }
	tg.Regs = append(tg.Regs, RegInfo{Name: "s2", Class: ClassInt})
	tg.Scratch[ClassInt] = append(tg.Scratch[ClassInt], Reg(len(tg.Regs)-1))
	tg.Clobbers = tg.Clobbers.Add(Reg(len(tg.Regs) - 1))
	al := raAllocate(t, f, tg)
	if al.Home[sum.ID].Kind != LocSlot {
		t.Fatalf("the sum lives across a call and its home is %v", al.Home[sum.ID])
	}
	for i, r := range al.Args[sum.ID] {
		if i > 0 && r == al.Result[sum.ID] {
			t.Errorf("the result is written to %v, which operand %d is read from:\n%s",
				tg.RegName(r), i, al)
		}
	}
	if len(al.Fixups) == 0 {
		t.Errorf("a two-address instruction with a spilled result made no fix-up:\n%s", al)
	}
}

func TestPhiOfMemoryAndOfARematerialisedValue(t *testing.T) {
	// A memory phi moves nothing: memory is an ordering edge and occupies no
	// storage. A phi of a constant moves nothing either; the copy recomputes
	// the constant into the phi's home, because a rematerialised value has
	// nowhere to be moved from.
	f, entry, then, els, join, mem := diamond()
	g := &ir.Object{Name: "g", Type: tInt, Class: ir.ClassGlobal}
	addr := entry.NewValue(0, OpAddr, tIntPtr)
	addr.Aux = g
	v := entry.NewValue(0, OpArg, tInt)

	st1 := then.NewValue(0, OpStore, MemType, addr, v, mem)
	st1.AuxInt = 8
	a := then.NewValue(0, OpArg, tInt)
	st2 := els.NewValue(0, OpStore, MemType, addr, v, mem)
	st2.AuxInt = 8
	c := els.NewValue(0, OpConstInt, tInt)
	c.AuxInt = 5

	memphi := join.NewValue(0, OpPhi, MemType, st1, st2)
	phi := join.NewValue(0, OpPhi, tInt, a, c)
	u := join.NewValue(0, OpAdd, tInt, phi, phi)
	raRet(join, memphi, u)

	al := raAllocate(t, f, raTarget())
	if al.Home[memphi.ID].Kind != LocNone {
		t.Errorf("a memory phi has home %v", al.Home[memphi.ID])
	}
	if len(al.Edges) != 2 {
		t.Fatalf("the join has two predecessors and %d copy sequences:\n%s", len(al.Edges), al)
	}
	for _, e := range al.Edges {
		if len(e.Copies) != 1 {
			t.Fatalf("the edge b%d -> b%d carries %d copies, want one for the value phi alone",
				e.Pred, e.Succ, len(e.Copies))
		}
	}
	// The copy on the edge from the block holding the constant recomputes it.
	last := al.Edges[1]
	if last.Copies[0].Src.Kind != LocNone || last.Copies[0].Value != c.ID {
		t.Errorf("the copy of a rematerialised argument is %+v", last.Copies[0])
	}
}

// merge returns a function whose last block has n predecessors, with no
// critical edge on any of them.
//
// The arms are chained rather than fanned out, because a block branches two
// ways at most. Each arm block has one successor, so every edge into the join
// can carry copies at the end of its predecessor.
func merge(n int) (f *Func, arms []*Block, join *Block, mem *Value) {
	f = NewFunc("merge")
	entry := f.Entry
	mem = entry.NewValue(0, OpInitMem, MemType)
	join = f.NewBlock(BlockRet)
	join.Control = join.NewValue(0, OpMakeResult, MemType, mem)

	for i := 0; i < n; i++ {
		arm := f.NewBlock(BlockPlain)
		arms = append(arms, arm)
		arm.AddEdgeTo(join)
	}
	at := entry
	for i := 0; i < n-1; i++ {
		at.Kind = BlockIf
		at.Control = at.NewValue(0, OpConstBool, tBool)
		at.AddEdgeTo(arms[i])
		if i == n-2 {
			// The last branch takes the last two arms.
			at.AddEdgeTo(arms[n-1])
			break
		}
		next := f.NewBlock(BlockPlain)
		at.AddEdgeTo(next)
		at = next
	}
	return f, arms, join, mem
}

// TestAllocatePhiOfManyArmsNeedsNoScratchRegister is the merge a select with
// several arms builds.
//
// A phi is not an instruction. The code generator emits nothing for it and
// resolvePhis turns it into a move on each edge, so the allocation must name
// no register for its operands. Naming one drew a scratch register per
// predecessor, which is a demand no machine bounds: this target reserves two
// and the phi below has five operands, and reserving a third would only move
// the refusal to a merge of four arms.
//
// The operands are constants, so each one is rematerialised and has no
// register home, which is the case that reached the scratch registers. A
// switch that assigns a different constant in each arm is the Go program.
func TestAllocatePhiOfManyArmsNeedsNoScratchRegister(t *testing.T) {
	const arms = 5
	f, blocks, join, mem := merge(arms)
	args := make([]*Value, 0, arms)
	for i, b := range blocks {
		c := b.NewValue(0, OpConstInt, tInt)
		c.AuxInt = int64(i + 1)
		args = append(args, c)
	}
	phi := join.NewValue(0, OpPhi, tInt, args...)
	use := join.NewValue(0, OpAdd, tInt, phi, phi)
	raRet(join, mem, use)

	tg := raTarget()
	if len(tg.Scratch[ClassInt]) != 2 {
		t.Fatalf("the target reserves %d integer scratch registers; this test is about a phi wider than that",
			len(tg.Scratch[ClassInt]))
	}
	al := raAllocate(t, f, tg)

	for i, r := range al.Args[phi.ID] {
		if r != NoReg {
			t.Errorf("operand %d of the phi is read from %s, and a phi reads no register",
				i, tg.RegName(r))
		}
	}
	if len(al.Edges) != arms {
		t.Fatalf("the join has %d predecessors and the allocation has %d copy sequences:\n%s",
			arms, len(al.Edges), al)
	}
	for _, e := range al.Edges {
		if len(e.Copies) != 1 || e.Copies[0].Src.Kind != LocNone {
			t.Errorf("the edge b%d -> b%d carries %v, want one recomputation of the arm's constant",
				e.Pred, e.Succ, e.Copies)
		}
	}
}

// TestAllocateOperandsInTheArgumentAreaNeedNoScratchRegister is the call with
// more arguments than the convention has registers for.
//
// specs/030-abi.md places such an operand in the outgoing argument area. The
// code generator writes it there with a store of its own, which materialises
// it into one register and holds it no longer than that store, so the call
// reads no register for it. Drawing a scratch register per such operand made
// the demand grow with the parameter list, which no machine bounds.
func TestAllocateOperandsInTheArgumentAreaNeedNoScratchRegister(t *testing.T) {
	f, e, mem := raFunc("frameargs")
	args := make([]*Value, 0, 4)
	for i := 0; i < 4; i++ {
		c := e.NewValue(0, OpConstInt, tInt)
		c.AuxInt = int64(i)
		args = append(args, c)
	}
	raRet(e, mem, args...)
	ret := e.Control

	tg := raTarget()
	if len(tg.Scratch[ClassInt]) != 2 {
		t.Fatalf("the target reserves %d integer scratch registers; this test is about a value wider than that",
			len(tg.Scratch[ClassInt]))
	}
	// The convention gives the first two values a register each and leaves the
	// rest in the argument area.
	tg.ABIPlaces = func(v *Value) bool { return v.Op == OpMakeResult }
	tg.UseReg = func(v *Value, i int) (Reg, bool) {
		if v.Op != OpMakeResult || i >= 2 {
			return NoReg, false
		}
		return []Reg{raR0, raR1}[i], true
	}
	al := raAllocate(t, f, tg)

	got := al.Args[ret.ID]
	want := []Reg{raR0, raR1, NoReg, NoReg, NoReg}
	if len(got) != len(want) {
		t.Fatalf("the return has %d operand registers for %d operands:\n%s", len(got), len(want), al)
	}
	for i, r := range want {
		if got[i] != r {
			t.Errorf("operand %d is read from %s, want %s", i, tg.RegName(got[i]), tg.RegName(r))
		}
	}
}

// TestAllocateCallResultsDoNotShareARegister is the machine fact the
// linearisation hides.
//
// A call defines every one of its results at once, in the registers the
// convention names, and the linearisation gives each OpSelectN a position of
// its own. Two results of one call then have ranges that do not meet, and the
// scan gives them one register. The code generator moves the first result into
// the register the second arrived in, and the second reads what the move
// wrote.
//
// A result nobody reads is what makes the case reachable, because its range is
// one position wide and ends before the next result begins. It still names an
// ABI location the code generator moves out of, which is why it cannot simply
// be dropped: ssa/lower.go keeps it for that reason.
//
// runtime.decoderune is the first call in this compiler with two results where
// a caller reads only the second, and a range over a string that asks for no
// rune is the program that produces it. Before this, that loop advanced by the
// rune's value rather than by its width and stopped after two iterations.
func TestAllocateCallResultsDoNotShareARegister(t *testing.T) {
	f, e, mem := raFunc("two")
	call := e.NewValue(0, OpStaticCall, MemType, mem)
	unread := e.NewValue(0, OpSelectN, tInt, call)
	unread.AuxInt = 0
	read := e.NewValue(0, OpSelectN, tInt, call)
	read.AuxInt = 1
	use := e.NewValue(0, OpAdd, tInt, read, read)
	raRet(e, call, use)

	al := raAllocate(t, f, raTarget())
	first, second := al.Home[unread.ID], al.Home[read.ID]
	if first.Kind != LocReg || second.Kind != LocReg {
		t.Fatalf("the results are at %v and %v, want two registers", first, second)
	}
	if first.Reg == second.Reg {
		t.Errorf("both results of the call are in %v", first)
	}
}
