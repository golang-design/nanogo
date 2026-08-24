// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj/arm64"
	"golang.design/x/nanogo/syntax"
)

func TestRegSet(t *testing.T) {
	var s RegSet
	if !s.Empty() || s.Len() != 0 || len(s.Regs()) != 0 {
		t.Fatal("a new set is not empty")
	}
	s = s.Add(3).Add(70).Add(0)
	if s.Empty() {
		t.Error("a set with three registers is empty")
	}
	if s.Len() != 3 {
		t.Errorf("the set holds %d registers, want 3", s.Len())
	}
	for _, r := range []Reg{0, 3, 70} {
		if !s.Contains(r) {
			t.Errorf("the set does not hold %d", r)
		}
	}
	if s.Contains(4) {
		t.Error("the set holds a register that was never added")
	}
	// The order is the numbering, which is what makes any output derived from
	// a set the same on every run (specs/053-determinism.md).
	got := s.Regs()
	if len(got) != 3 || got[0] != 0 || got[1] != 3 || got[2] != 70 {
		t.Errorf("the members are %v, want 0, 3 and 70", got)
	}
	s = s.Remove(3)
	if s.Contains(3) || s.Len() != 2 {
		t.Errorf("after removing 3 the set is %v", s.Regs())
	}

	var u RegSet
	u = u.Add(3).Add(70)
	if n := s.Union(u).Len(); n != 3 {
		t.Errorf("the union holds %d registers, want 3", n)
	}
	if n := s.Intersect(u).Len(); n != 1 {
		t.Errorf("the intersection holds %d registers, want 1", n)
	}
	// A register outside the range is not in the set and adding it changes
	// nothing, so a caller with a bad number gets an empty answer rather than
	// a corrupted set.
	if s.Contains(-1) || s.Contains(MaxRegs) {
		t.Error("a register outside the range is in the set")
	}
	if s.Add(-1).Len() != 2 || s.Add(MaxRegs).Len() != 2 || s.Remove(-1).Len() != 2 {
		t.Error("a register outside the range changed the set")
	}
}

func TestLocPrinting(t *testing.T) {
	if got := (Loc{}).String(); got != "-" {
		t.Errorf("no location prints as %q", got)
	}
	if got := RegLoc(7).String(); got != "r7" {
		t.Errorf("a register location prints as %q", got)
	}
	if got := SlotLoc(2).String(); got != "s2" {
		t.Errorf("a slot location prints as %q", got)
	}
	if got := ClassFloat.String(); got != "float" {
		t.Errorf("the float class prints as %q", got)
	}
	if got := RegClass(9).String(); got != "class(?)" {
		t.Errorf("an unknown class prints as %q", got)
	}
	tg := NewArm64Target()
	if got := tg.LocString(RegLoc(Arm64Reg(arm64.R3))); got != "R3" {
		t.Errorf("a register location prints as %q", got)
	}
	if got := tg.LocString(SlotLoc(1)); got != "s1" {
		t.Errorf("a slot location prints as %q", got)
	}
	if got := tg.RegName(-1); got != "reg(-1)" {
		t.Errorf("a register outside the file prints as %q", got)
	}
	if tg.RegClassOf(-1) != ClassInt || tg.IsAllocatable(-1) || tg.IsScratch(-1) {
		t.Error("a register outside the file is not reported as absent")
	}
}

func TestArm64TargetMatchesTheABI(t *testing.T) {
	tg := NewArm64Target()
	if errs := tg.Validate(); len(errs) != 0 {
		t.Fatalf("the arm64 target does not validate: %v", errs)
	}

	// R18 is reserved by darwin. A compiler that allocates it produces a
	// binary that fails for reasons that have nothing to do with the program
	// (specs/026-register-allocation.md, specs/030-abi.md).
	r18 := Arm64Reg(arm64.R18)
	if tg.IsAllocatable(r18) {
		t.Error("R18 is allocatable")
	}
	if tg.Allocatable[ClassInt].Contains(r18) || tg.Allocatable[ClassFloat].Contains(r18) {
		t.Error("R18 is in an allocatable set")
	}
	for _, r := range tg.ArgRegs[ClassInt] {
		if r == r18 {
			t.Error("R18 carries an argument")
		}
	}

	// The registers with an ABI role are not allocatable either.
	for _, r := range []arm64.Reg{arm64.RegG, arm64.RegClosure, arm64.RegFramePtr,
		arm64.RegLink, arm64.RegAsmScratch, arm64.RegTrampLo, arm64.RegTrampHi, arm64.ZR, arm64.RSP} {
		if tg.IsAllocatable(Arm64Reg(r)) {
			t.Errorf("%v is allocatable and specs/030-abi.md gives it a role", r)
		}
	}

	// The allocatable set is obj/arm64's table and not a second copy of it.
	want := 0
	for _, r := range arm64.AllocatableRegs() {
		want++
		if !tg.Allocatable[ClassInt].Contains(Arm64Reg(r)) {
			t.Errorf("%v is allocatable in obj/arm64 and not in the target", r)
		}
	}
	if got := tg.Allocatable[ClassInt].Len(); got != want {
		t.Errorf("the target has %d allocatable integer registers and obj/arm64 has %d", got, want)
	}

	// Every register a value can be in is destroyed by a call. Go's internal
	// ABI has no callee-saved registers (specs/030-abi.md).
	for c := RegClass(0); c < NumRegClass; c++ {
		for _, r := range tg.Allocatable[c].Regs() {
			if !tg.Clobbers.Contains(r) {
				t.Errorf("%s survives a call", tg.RegName(r))
			}
		}
		for _, r := range tg.Scratch[c] {
			if tg.IsAllocatable(r) {
				t.Errorf("the scratch register %s is allocatable", tg.RegName(r))
			}
			if !tg.IsScratch(r) {
				t.Errorf("%s is not reported as a scratch register", tg.RegName(r))
			}
		}
	}

	// specs/030-abi.md: R0 to R15 and F0 to F15 carry arguments and results.
	if len(tg.ArgRegs[ClassInt]) != 16 || tg.ArgRegs[ClassInt][0] != Arm64Reg(arm64.R0) ||
		tg.ArgRegs[ClassInt][15] != Arm64Reg(arm64.R15) {
		t.Errorf("the integer argument registers are %v", tg.ArgRegs[ClassInt])
	}
	if len(tg.ArgRegs[ClassFloat]) != 16 || tg.ArgRegs[ClassFloat][0] != Arm64FReg(0) {
		t.Errorf("the float argument registers are %v", tg.ArgRegs[ClassFloat])
	}

	// The integer numbering is obj/arm64's, so the two packages cannot
	// disagree about which register R7 is.
	for _, r := range arm64.AllocatableRegs() {
		if got := tg.RegName(Arm64Reg(r)); got != r.String() {
			t.Errorf("register %d is %q in the target and %q in obj/arm64", r, got, r.String())
		}
		if tg.RegClassOf(Arm64Reg(r)) != ClassInt {
			t.Errorf("%v is not an integer register", r)
		}
	}
	if tg.RegClassOf(Arm64FReg(0)) != ClassFloat || tg.RegName(Arm64FReg(31)) != "F31" {
		t.Errorf("the floating-point file is %v to %v", tg.RegName(Arm64FReg(0)), tg.RegName(Arm64FReg(31)))
	}
	// A fresh instance each time, so a test that changes one field does not
	// change what every other test sees.
	other := NewArm64Target()
	other.Name = "changed"
	if NewArm64Target().Name != "arm64" {
		t.Error("the targets share state")
	}
	// arm64 is a three-address machine, so no instruction overwrites a source.
	if tg.TwoAddress(&Value{Op: OpAdd}) {
		t.Error("arm64 was described as two-address")
	}
	if _, ok := tg.DefReg(&Value{Op: OpArg}); ok {
		t.Error("the arm64 target pre-colours a value and the ABI pass does not exist yet")
	}
	if _, ok := tg.UseReg(&Value{Op: OpStaticCall}, 0); ok {
		t.Error("the arm64 target pre-colours an operand and the ABI pass does not exist yet")
	}
}

func TestArm64TargetAllocates(t *testing.T) {
	// The target the compiler ships with, on a function that needs more values
	// than it has registers.
	f, _ := raPressure("arm64", 40)
	tg := NewArm64Target()
	if err := Check(f); err != nil {
		t.Fatalf("the function does not verify: %v", err)
	}
	a, err := Allocate(f, tg)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := CheckAllocation(f, a); err != nil {
		t.Fatalf("%v", err)
	}
	for _, h := range a.Home {
		if h.Kind == LocReg && h.Reg == Arm64Reg(arm64.R18) {
			t.Fatal("a value was placed in R18")
		}
	}
	if len(a.Slots) == 0 {
		t.Error("40 live values in 23 registers used no slot")
	}
}

func TestClassOfType(t *testing.T) {
	cases := []struct {
		t     *ir.Type
		class RegClass
		ok    bool
	}{
		{tInt, ClassInt, true},
		{tBool, ClassInt, true},
		{tByte, ClassInt, true},
		{tIntPtr, ClassInt, true},
		{tFunc, ClassInt, true},
		{tFloat, ClassFloat, true},
		{mkType(&ir.Type{Kind: ir.Float32}), ClassFloat, true},
		// The multi-word types. Lowering decomposes them; one that arrives
		// whole gets a slot rather than the first word of a register.
		{tString, ClassInt, false},
		{tSlice, ClassInt, false},
		{tStruct, ClassInt, false},
		{tArr4, ClassInt, false},
		{mkType(&ir.Type{Kind: ir.Complex128}), ClassFloat, false},
		{tVoid, ClassInt, false},
		{MemType, ClassInt, false},
		{nil, ClassInt, false},
	}
	for _, tc := range cases {
		c, ok := ClassOfType(tc.t)
		if c != tc.class || ok != tc.ok {
			t.Errorf("%v is class %v ok=%v, want %v ok=%v", tc.t, c, ok, tc.class, tc.ok)
		}
	}
}

func TestRematerialisable(t *testing.T) {
	// specs/026-register-allocation.md names three: a constant, a frame
	// address, and a static symbol address.
	_, e, _ := raFunc("remat")
	yes := []*Value{
		e.NewValue(0, OpConstInt, tInt),
		e.NewValue(0, OpConstBool, tBool),
		e.NewValue(0, OpConstFloat, tFloat),
		e.NewValue(0, OpConstNil, tIntPtr),
		e.NewValue(0, OpAddr, tIntPtr),
	}
	for _, v := range yes {
		if !Rematerialisable(v) {
			t.Errorf("%v is not rematerialisable", v.Op)
		}
	}
	no := []*Value{
		e.NewValue(0, OpArg, tInt),
		e.NewValue(0, OpAdd, tInt),
		// A two-word constant would need two registers, so recomputing it
		// into one would be a lie.
		e.NewValue(0, OpConstString, tString),
	}
	for _, v := range no {
		if Rematerialisable(v) {
			t.Errorf("%v is rematerialisable", v.Op)
		}
	}
	if Rematerialisable(nil) {
		t.Error("no value is rematerialisable")
	}
}

func TestTargetValidateReportsEveryMistake(t *testing.T) {
	has := func(errs []error, want string) bool {
		for _, e := range errs {
			if strings.Contains(e.Error(), want) {
				return true
			}
		}
		return false
	}
	var nilTarget *Target
	if errs := nilTarget.Validate(); len(errs) != 1 {
		t.Errorf("a nil target gave %v", errs)
	}
	if errs := (&Target{Name: "empty"}).Validate(); !has(errs, "describes no register") ||
		!has(errs, "has no ClassOf") || !has(errs, "has no scratch register") {
		t.Errorf("an empty target gave %v", errs)
	}

	tg := raTarget()
	tg.Regs = make([]RegInfo, MaxRegs+1)
	if errs := tg.Validate(); !has(errs, "the limit is") {
		t.Errorf("a target above the register limit gave %v", errs)
	}

	// A register allocatable in the wrong class, a scratch register that is
	// also allocatable, and an argument register that is not allocatable are
	// each named.
	tg = raTarget()
	tg.Allocatable[ClassFloat] = tg.Allocatable[ClassFloat].Add(raR0)
	if errs := tg.Validate(); !has(errs, "belongs to class") {
		t.Errorf("a register allocatable in the wrong class gave %v", errs)
	}

	tg = raTarget()
	tg.Scratch[ClassInt] = []Reg{raR0}
	if errs := tg.Validate(); !has(errs, "is also allocatable") {
		t.Errorf("an allocatable scratch register gave %v", errs)
	}

	tg = raTarget()
	tg.Scratch[ClassInt] = []Reg{raF0}
	if errs := tg.Validate(); !has(errs, "is in class") {
		t.Errorf("a scratch register of the wrong class gave %v", errs)
	}

	tg = raTarget()
	tg.Scratch[ClassInt] = []Reg{Reg(len(tg.Regs))}
	if errs := tg.Validate(); !has(errs, "is not described") {
		t.Errorf("a scratch register outside the file gave %v", errs)
	}

	tg = raTarget()
	tg.Allocatable[ClassInt] = tg.Allocatable[ClassInt].Add(Reg(len(tg.Regs)))
	if errs := tg.Validate(); !has(errs, "is not described") {
		t.Errorf("an allocatable register outside the file gave %v", errs)
	}

	tg = raTarget()
	tg.ArgRegs[ClassInt] = []Reg{raS0}
	if errs := tg.Validate(); !has(errs, "argument register") {
		t.Errorf("an argument register that is not allocatable gave %v", errs)
	}

	tg = raTarget()
	tg.ResultRegs[ClassInt] = []Reg{raS0}
	if errs := tg.Validate(); !has(errs, "result register") {
		t.Errorf("a result register that is not allocatable gave %v", errs)
	}

	// Go's ABI has no callee-saved register. A target that claims one is
	// describing a machine the rest of this pass does not implement.
	tg = raTarget()
	tg.Clobbers = tg.Clobbers.Remove(raR2)
	if errs := tg.Validate(); !has(errs, "no callee-saved register") {
		t.Errorf("a target with a callee-saved register gave %v", errs)
	}
}

// TestRematerialisationSurvivesLowering is the guard for a failure mode that
// produces no error at all.
//
// Rematerialisation keeps constants and addresses out of spill slots. It reads
// the op table and the Rematerialisable list, and a target whose lowering rules
// produce forms absent from both loses it silently: nothing fails, the code
// merely gets worse. The register allocator found exactly that after the arm64
// rules landed.
func TestRematerialisationSurvivesLowering(t *testing.T) {
	f := NewFunc("t")
	b := f.NewBlock(BlockPlain)

	i64 := &ir.Type{Kind: ir.Int64, Size: 8, Align: 8}
	ptr := &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8}

	cases := []struct {
		what string
		v    *Value
		want bool
	}{
		{"machine constant", b.NewValue(syntax.NoPos, OpARM64MOVDconst, i64), true},
		{"static symbol address", b.NewValue(syntax.NoPos, OpARM64MOVDaddr, ptr), true},
		{"frame address", b.NewValue(syntax.NoPos, OpARM64ADDframe, ptr), true},
		{"an ordinary machine op", b.NewValue(syntax.NoPos, OpARM64ADD, i64), false},
	}
	for _, c := range cases {
		if got := Rematerialisable(c.v); got != c.want {
			t.Errorf("%s (%v): Rematerialisable = %v, want %v", c.what, c.v.Op, got, c.want)
		}
	}

	// And the property that matters rather than the list: at least one machine
	// constant form and at least one machine address form must be
	// rematerialisable, whatever they end up being called.
	var consts, addrs int
	for _, c := range cases {
		if !c.want {
			continue
		}
		if Rematerialisable(c.v) {
			if c.v.Op.IsConstant() {
				consts++
			} else {
				addrs++
			}
		}
	}
	if consts == 0 {
		t.Error("no machine constant form is rematerialisable; every constant will be spilled")
	}
	if addrs == 0 {
		t.Error("no machine address form is rematerialisable; every address will be spilled")
	}
}
