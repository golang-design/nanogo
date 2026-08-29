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
		// An aggregate of one machine word is not a multi-word type, whatever
		// it holds. TestAOneWordAggregateIsOneIntegerRegister below is why the
		// float field is in the integer class.
		{tWordStruct, ClassInt, true},
		{tWordArr, ClassInt, true},
		{tByteStruct, ClassInt, true},
		{tFloatStruct, ClassInt, true},
		// Three bytes is no load and no store, so it is no register either.
		{tOddStruct, ClassInt, false},
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

// The aggregates the case above names. They are here rather than in
// build_test.go's list because nothing else builds a value of one.
var (
	tWordStruct = mkType(&ir.Type{Kind: ir.Struct, Name: "u", Fields: []ir.Field{
		{Name: "p", Type: tIntPtr},
	}})
	tWordArr     = mkType(&ir.Type{Kind: ir.Array, Elem: tInt, Len: 1})
	tByteStruct  = mkType(&ir.Type{Kind: ir.Struct, Name: "b1", Fields: []ir.Field{{Name: "a", Type: tByte}}})
	tFloatStruct = mkType(&ir.Type{Kind: ir.Struct, Name: "f1", Fields: []ir.Field{{Name: "f", Type: tFloat}}})
	tOddStruct   = mkType(&ir.Type{Kind: ir.Struct, Name: "b3", Fields: []ir.Field{
		{Name: "a", Type: tByte}, {Name: "b", Type: tByte}, {Name: "c", Type: tByte},
	}})
)

// TestAOneWordAggregateIsOneIntegerRegister holds ClassOfType against the load
// and the store the rules select for the same type.
//
// The two decide the same question from different sides. ARM64LoadOp and
// ARM64StoreOpForType answer by the width and by the integer file, and
// ClassOfType answers whether one register holds the value. While ClassOfType
// answered by the kind alone the two disagreed: lowering selected
// ARM64MOVDload for a value of type struct{*T}, the allocator called the type
// too wide for a register, and the code generator refused the function because
// a value with a width had been given nowhere to go. test/ddd.go and
// test/method.go of Go's own corpus are the two programs that found it.
//
// The invariant is one direction. Every type the load takes has a class, and a
// type with no class is a type no single load reaches. A complex is the one
// type this table does not name, because the rules refuse a complex before
// ARM64LoadOp is asked: specs/042-arm64-backend.md group 6 says a complex is
// two floating-point registers and the decomposition pass splits it.
func TestAOneWordAggregateIsOneIntegerRegister(t *testing.T) {
	types := []*ir.Type{
		tInt, tBool, tByte, tIntPtr, tFunc, tFloat,
		tWordStruct, tWordArr, tByteStruct, tFloatStruct,
		tOddStruct, tStruct, tArr4, tString, tSlice, tVoid,
	}
	n := 0
	for _, typ := range types {
		ld, hasLoad := ARM64LoadOp(typ)
		st, hasStore := ARM64StoreOpForType(typ)
		c, hasClass := ClassOfType(typ)
		if hasLoad != hasStore {
			t.Errorf("%v has a load=%v and a store=%v; one instruction moves it or neither does", typ, hasLoad, hasStore)
		}
		if hasLoad && !hasClass {
			t.Errorf("%v is loaded by %v and has no register class, so the allocation gives the load nowhere to go", typ, ld)
		}
		n++
		switch typ.Kind {
		case ir.Struct, ir.Array:
			// The integer file, whatever the fields are. The load and the
			// store an aggregate takes are the integer ones, so a
			// floating-point class here would name a register no instruction
			// of the pair writes.
			if hasClass && c != ClassInt {
				t.Errorf("%v is in class %v, and %v and %v are integer instructions", typ, c, ld, st)
			}
		}
	}
	if n != len(types) {
		t.Fatalf("%d types were checked, want %d", n, len(types))
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

// TestArm64FloatRegisterFile pins the floating-point half of the description.
//
// One property matters more than the rest: a ssa.Reg and an obj/arm64.Reg are
// the same number for every register of both classes. The emitter of specs/042
// hands a Reg straight to an encoder, so if the two numberings drifted a
// floating-point value would be encoded as an integer register with the same
// number and the program would read the wrong file with no diagnostic.
func TestArm64FloatRegisterFile(t *testing.T) {
	tg := NewArm64Target()
	for i := 0; i < 32; i++ {
		r := Arm64FReg(i)
		if r != Arm64Reg(arm64.F0+arm64.Reg(i)) {
			t.Fatalf("Arm64FReg(%d) is %d and obj/arm64 numbers F%d as %d",
				i, r, i, arm64.F0+arm64.Reg(i))
		}
		if got, want := tg.RegName(r), arm64.Reg(arm64.F0+arm64.Reg(i)).String(); got != want {
			t.Errorf("register %d is %q in the target and %q in obj/arm64", r, got, want)
		}
		if tg.RegClassOf(r) != ClassFloat {
			t.Errorf("%v is not in the float class", tg.RegName(r))
		}
	}
	for r := arm64.R0; r <= arm64.RSP; r++ {
		if tg.RegClassOf(Arm64Reg(r)) != ClassInt {
			t.Errorf("%v is not in the integer class", r)
		}
	}

	// The float allocatable set is obj/arm64's table and not a second copy.
	want := 0
	for _, r := range arm64.AllocatableFRegs() {
		want++
		if !tg.Allocatable[ClassFloat].Contains(Arm64Reg(r)) {
			t.Errorf("%v is allocatable in obj/arm64 and not in the target", r)
		}
	}
	if got := tg.Allocatable[ClassFloat].Len(); got != want {
		t.Errorf("the target has %d allocatable float registers and obj/arm64 has %d", got, want)
	}
	if want == 0 {
		t.Fatal("no floating-point register is allocatable, so specs/042 group 6 has nowhere to put a value")
	}

	// The two classes must not overlap, or one register would be handed to
	// two values at once.
	if !tg.Allocatable[ClassInt].Intersect(tg.Allocatable[ClassFloat]).Empty() {
		t.Error("a register is allocatable in both classes")
	}
	if errs := tg.Validate(); len(errs) != 0 {
		t.Errorf("the target does not validate: %v", errs)
	}

	// ClassOfType is what the allocator asks, and a float has to reach the
	// float class or its value goes to an integer register.
	for _, c := range []struct {
		t     *ir.Type
		class RegClass
		ok    bool
	}{
		{&ir.Type{Kind: ir.Float32, Size: 4, Align: 4}, ClassFloat, true},
		{&ir.Type{Kind: ir.Float64, Size: 8, Align: 8}, ClassFloat, true},
		{&ir.Type{Kind: ir.Complex128, Size: 16, Align: 8}, ClassFloat, false},
		{&ir.Type{Kind: ir.Int64, Size: 8, Align: 8}, ClassInt, true},
	} {
		got, ok := ClassOfType(c.t)
		if got != c.class || ok != c.ok {
			t.Errorf("ClassOfType(%v) = %v, %v; want %v, %v", c.t.Kind, got, ok, c.class, c.ok)
		}
	}

	// A floating-point constant is rematerialised rather than spilled, which
	// is the property specs/026 relies on to keep it out of the frame.
	f := NewFunc("f")
	cf := f.Entry.NewValue(0, OpARM64FMOVconst, &ir.Type{Kind: ir.Float64, Size: 8, Align: 8})
	if !Rematerialisable(cf) {
		t.Error("a floating-point constant is not rematerialisable")
	}
}
