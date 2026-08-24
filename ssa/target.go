// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

// The target description the register allocator is parameterised by.
//
// specs/002-architecture.md requires that adding a target needs no edit above
// the lowering rules. The allocator is above them, so everything it needs to
// know about a machine arrives through this file: the register file, the
// allocatable set per class, what a call destroys, which registers the ABI
// fixes, and three predicates over operations.
//
// specs/026-register-allocation.md names the two properties that reach above
// specs/025-lowering-and-rules.md. Register class is one and is universal. The
// two-address form is the other and belongs to amd64 alone. Both are here, and
// nothing else is, which is the line this file exists to hold.
//
// The description is data plus function fields, not an interface. A target is
// a value a test can build in ten lines with the behaviour it wants to
// exercise, and the allocator can then be tested without any machine operation
// existing.

import (
	"fmt"
	"strconv"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj/arm64"
)

// Reg is a machine register, numbered densely within one target.
//
// The numbering is the target's own and covers every class, so one number
// names one register and a set over registers is one bitset. A Reg is an index
// into Target.Regs.
type Reg int16

// NoReg is the absence of a register.
const NoReg Reg = -1

// MaxRegs is the largest register file a Target may describe.
//
// arm64 needs 65 numbers: 33 integer spellings from obj/arm64 and 32
// floating-point. amd64 needs fewer. The bound exists so that RegSet is a
// fixed-size value with no allocation and no map.
const MaxRegs = 128

// RegClass is the kind of register file a value lives in.
//
// specs/026-register-allocation.md: integer and floating point are separate
// classes with separate free lists, and there is no case in Go where a value
// could go in either. The class of a value therefore follows from its type
// alone, which is why Target.ClassOf takes a type and not a value.
type RegClass uint8

const (
	// ClassInt holds integers, pointers, and anything else one machine word
	// wide that is not a float.
	ClassInt RegClass = iota
	// ClassFloat holds float32 and float64.
	ClassFloat

	// NumRegClass is the number of classes. It sizes the per-class tables.
	NumRegClass
)

var regClassNames = [NumRegClass]string{ClassInt: "int", ClassFloat: "float"}

func (c RegClass) String() string {
	if int(c) < len(regClassNames) {
		return regClassNames[c]
	}
	return "class(?)"
}

// RegSet is a set of registers.
//
// A fixed-size bitset rather than a slice or a map. Ranging a map would break
// specs/053-determinism.md, and the allocator ranges the free set on every
// value it places.
type RegSet struct {
	bits [MaxRegs / 64]uint64
}

// Add returns s with r added.
func (s RegSet) Add(r Reg) RegSet {
	if r < 0 || int(r) >= MaxRegs {
		return s
	}
	s.bits[r/64] |= 1 << uint(r%64)
	return s
}

// Remove returns s without r.
func (s RegSet) Remove(r Reg) RegSet {
	if r < 0 || int(r) >= MaxRegs {
		return s
	}
	s.bits[r/64] &^= 1 << uint(r%64)
	return s
}

// Contains reports whether r is in s.
func (s RegSet) Contains(r Reg) bool {
	if r < 0 || int(r) >= MaxRegs {
		return false
	}
	return s.bits[r/64]&(1<<uint(r%64)) != 0
}

// Union returns the union of s and t.
func (s RegSet) Union(t RegSet) RegSet {
	for i := range s.bits {
		s.bits[i] |= t.bits[i]
	}
	return s
}

// Intersect returns the intersection of s and t.
func (s RegSet) Intersect(t RegSet) RegSet {
	for i := range s.bits {
		s.bits[i] &= t.bits[i]
	}
	return s
}

// Empty reports whether s holds no register.
func (s RegSet) Empty() bool {
	for _, w := range s.bits {
		if w != 0 {
			return false
		}
	}
	return true
}

// Len returns the number of registers in s.
func (s RegSet) Len() int {
	n := 0
	for _, w := range s.bits {
		for ; w != 0; w &= w - 1 {
			n++
		}
	}
	return n
}

// Regs returns the members of s in increasing order.
//
// The order is the register numbering, which is fixed by the target, so a
// caller that walks it produces the same output on every run.
func (s RegSet) Regs() []Reg {
	out := make([]Reg, 0, MaxRegs/8)
	for i, w := range s.bits {
		for ; w != 0; w &= w - 1 {
			out = append(out, Reg(i*64+trailingZeros(w)))
		}
	}
	return out
}

// trailingZeros returns the index of the lowest set bit of w, which is not
// zero. It is written out rather than imported so that the ssa package's
// dependency list stays at ir and syntax.
func trailingZeros(w uint64) int {
	n := 0
	for w&1 == 0 {
		w >>= 1
		n++
	}
	return n
}

// RegInfo describes one register.
type RegInfo struct {
	Name  string
	Class RegClass
}

// LocKind is what kind of storage a location names.
type LocKind uint8

const (
	// LocNone is no storage. A memory value has none, and so does a
	// rematerialised value: it is recomputed at each use and is never held.
	LocNone LocKind = iota
	// LocReg is a machine register.
	LocReg
	// LocSlot is an index into Alloc.Slots.
	LocSlot
)

// Loc is where a value lives.
type Loc struct {
	Kind LocKind
	Reg  Reg   // when Kind is LocReg
	Slot int32 // when Kind is LocSlot
}

// RegLoc returns the location of register r.
func RegLoc(r Reg) Loc { return Loc{Kind: LocReg, Reg: r} }

// SlotLoc returns the location of spill slot i.
func SlotLoc(i int32) Loc { return Loc{Kind: LocSlot, Slot: i} }

func (l Loc) String() string {
	switch l.Kind {
	case LocReg:
		return "r" + strconv.Itoa(int(l.Reg))
	case LocSlot:
		return "s" + strconv.Itoa(int(l.Slot))
	}
	return "-"
}

// Target describes one machine to the register allocator.
type Target struct {
	// Name is the target's name, for diagnostics only.
	Name string

	// Regs describes every register, indexed by register number.
	Regs []RegInfo

	// Allocatable is the set the allocator may choose from, per class. A
	// register with an ABI role is not in it: specs/030-abi.md's goroutine,
	// closure, frame pointer and link registers, and on darwin R18, which the
	// operating system reserves.
	Allocatable [NumRegClass]RegSet

	// Scratch holds the registers reserved for materialisation, per class.
	//
	// A spilled value is read from its slot into a scratch register at each
	// use, a rematerialised value is recomputed into one, and a parallel copy
	// breaks a cycle with the first of them. They are outside Allocatable, so
	// one is always available and the allocator never has to spill to spill.
	//
	// The cost is len(Scratch) fewer registers for values. On arm64 the cost
	// is zero, because the registers taken are the two the linker already
	// reserves for trampolines and which specs/030-abi.md never allocates.
	Scratch [NumRegClass][]Reg

	// Clobbers is the set a call destroys. Go's internal ABI has no
	// callee-saved registers, so for every target here this is every
	// allocatable register (specs/026-register-allocation.md).
	Clobbers RegSet

	// ArgRegs and ResultRegs are the registers specs/030-abi.md assigns to
	// arguments and to results, per class, in assignment order.
	ArgRegs    [NumRegClass][]Reg
	ResultRegs [NumRegClass][]Reg

	// ClassOf returns the register class of a type, and false when a value of
	// that type cannot live in one register. A string, a slice and an
	// interface reach the allocator only if lowering did not decompose them,
	// and such a value gets a stack slot rather than a wrong register.
	ClassOf func(t *ir.Type) (RegClass, bool)

	// IsCall reports that a value transfers control to another function, and
	// therefore that it is a safepoint and clobbers Clobbers. It is a hook
	// because a machine call operation is not in the target-neutral table of
	// op.go and the allocator must not learn about the machine op set.
	IsCall func(v *Value) bool

	// Remat reports that a value is cheap to recompute: a constant, a frame
	// address, or a static symbol address (specs/026-register-allocation.md).
	// Such a value is never spilled and never held in a register.
	Remat func(v *Value) bool

	// TwoAddress reports that the machine instruction for a value overwrites
	// its first source operand. amd64 needs this and arm64 does not.
	TwoAddress func(v *Value) bool

	// DefReg returns the register specs/030-abi.md fixes for a value's
	// definition, and false when the value is free to go anywhere. An incoming
	// argument and a call result are the cases.
	DefReg func(v *Value) (Reg, bool)

	// UseReg returns the register specs/030-abi.md fixes for argument i of a
	// value, and false when the argument is free. A call's operands are the
	// case: the callee reads them where the convention says, not where the
	// allocator put them.
	UseReg func(v *Value, i int) (Reg, bool)
}

// RegName returns the name of r, or a number when the target does not describe
// it.
func (t *Target) RegName(r Reg) string {
	if r < 0 || int(r) >= len(t.Regs) {
		return "reg(" + strconv.Itoa(int(r)) + ")"
	}
	return t.Regs[r].Name
}

// RegClassOf returns the class r belongs to.
func (t *Target) RegClassOf(r Reg) RegClass {
	if r < 0 || int(r) >= len(t.Regs) {
		return ClassInt
	}
	return t.Regs[r].Class
}

// IsAllocatable reports whether the allocator may give r to a value.
func (t *Target) IsAllocatable(r Reg) bool {
	if r < 0 || int(r) >= len(t.Regs) {
		return false
	}
	return t.Allocatable[t.Regs[r].Class].Contains(r)
}

// IsScratch reports whether r is reserved for materialisation.
func (t *Target) IsScratch(r Reg) bool {
	if r < 0 || int(r) >= len(t.Regs) {
		return false
	}
	for _, s := range t.Scratch[t.Regs[r].Class] {
		if s == r {
			return true
		}
	}
	return false
}

// LocString returns a location in the form a dump uses, with register names
// rather than numbers.
func (t *Target) LocString(l Loc) string {
	if l.Kind == LocReg {
		return t.RegName(l.Reg)
	}
	return l.String()
}

// Validate reports the ways in which t is not a usable description.
//
// It is called by Allocate, so a target with a register in two classes or an
// allocatable scratch register fails at the first allocation rather than
// producing a binary that fails somewhere else. The list is returned whole:
// a description with two mistakes should show both.
func (t *Target) Validate() []error {
	var errs []error
	if t == nil {
		return []error{fmt.Errorf("ssa: nil target")}
	}
	if len(t.Regs) == 0 {
		errs = append(errs, fmt.Errorf("ssa: target %s describes no register", t.Name))
	}
	if len(t.Regs) > MaxRegs {
		errs = append(errs, fmt.Errorf("ssa: target %s describes %d registers, the limit is %d", t.Name, len(t.Regs), MaxRegs))
	}
	for _, h := range []struct {
		name string
		nil  bool
	}{
		{"ClassOf", t.ClassOf == nil},
		{"IsCall", t.IsCall == nil},
		{"Remat", t.Remat == nil},
		{"TwoAddress", t.TwoAddress == nil},
		{"DefReg", t.DefReg == nil},
		{"UseReg", t.UseReg == nil},
	} {
		if h.nil {
			errs = append(errs, fmt.Errorf("ssa: target %s has no %s", t.Name, h.name))
		}
	}
	for c := RegClass(0); c < NumRegClass; c++ {
		for _, r := range t.Allocatable[c].Regs() {
			if int(r) >= len(t.Regs) {
				errs = append(errs, fmt.Errorf("ssa: target %s: allocatable register %d is not described", t.Name, r))
				continue
			}
			if t.Regs[r].Class != c {
				errs = append(errs, fmt.Errorf("ssa: target %s: %s is allocatable in class %v and belongs to class %v", t.Name, t.RegName(r), c, t.Regs[r].Class))
			}
		}
		if len(t.Scratch[c]) == 0 {
			errs = append(errs, fmt.Errorf("ssa: target %s: class %v has no scratch register", t.Name, c))
		}
		for _, r := range t.Scratch[c] {
			if int(r) >= len(t.Regs) || r < 0 {
				errs = append(errs, fmt.Errorf("ssa: target %s: scratch register %d is not described", t.Name, r))
				continue
			}
			if t.Regs[r].Class != c {
				errs = append(errs, fmt.Errorf("ssa: target %s: scratch register %s is in class %v, not %v", t.Name, t.RegName(r), t.Regs[r].Class, c))
			}
			if t.Allocatable[c].Contains(r) {
				errs = append(errs, fmt.Errorf("ssa: target %s: scratch register %s is also allocatable", t.Name, t.RegName(r)))
			}
		}
		for _, r := range t.ArgRegs[c] {
			if !t.Allocatable[c].Contains(r) {
				errs = append(errs, fmt.Errorf("ssa: target %s: argument register %s is not allocatable", t.Name, t.RegName(r)))
			}
		}
		for _, r := range t.ResultRegs[c] {
			if !t.Allocatable[c].Contains(r) {
				errs = append(errs, fmt.Errorf("ssa: target %s: result register %s is not allocatable", t.Name, t.RegName(r)))
			}
		}
	}
	// Go's ABI clobbers every register at a call. A target that claims an
	// allocatable register survives a call would make the allocator keep a
	// value there, and specs/030-abi.md says there is no such register.
	for c := RegClass(0); c < NumRegClass; c++ {
		for _, r := range t.Allocatable[c].Regs() {
			if !t.Clobbers.Contains(r) {
				errs = append(errs, fmt.Errorf("ssa: target %s: %s is allocatable and not clobbered by a call, and Go's ABI has no callee-saved register", t.Name, t.RegName(r)))
			}
		}
	}
	return errs
}

// The arm64 target.
//
// The register numbering is obj/arm64's, whole. A Reg converts to an
// obj/arm64.Reg by a cast in both directions, for the floating-point file as
// well as the integer one, so the two packages cannot disagree about which
// register a number names. That identity is what lets the emitter of
// specs/042 hand a Reg straight to an encoder, and a test below pins it.

// numArm64Reg is the size of obj/arm64's register file, both classes. It is
// computed from that package rather than written as a constant, so the two
// cannot drift.
var numArm64Reg = func() int {
	n := 0
	for arm64.Reg(n).Valid() {
		n++
	}
	return n
}()

// Arm64Reg returns the allocator's number for an obj/arm64 register, of
// either class.
func Arm64Reg(r arm64.Reg) Reg { return Reg(r) }

// Arm64FReg returns the allocator's number for floating-point register n,
// which is F0 to F31 of specs/030-abi.md.
func Arm64FReg(n int) Reg { return Reg(arm64.F0) + Reg(n) }

// NewArm64Target returns the description of darwin/arm64.
//
// A function rather than a package-level variable, so that a test can take one
// and change one field without every other test seeing the change.
func NewArm64Target() *Target {
	t := &Target{Name: "arm64"}

	t.Regs = make([]RegInfo, numArm64Reg)
	for i := 0; i < numArm64Reg; i++ {
		r := arm64.Reg(i)
		c := ClassInt
		if r.IsFloat() {
			c = ClassFloat
		}
		t.Regs[i] = RegInfo{Name: r.String(), Class: c}
	}

	// The allocatable sets are obj/arm64's tables, not a second copy of them.
	// R18 is absent there, which is the whole point: darwin reserves it, and a
	// compiler that allocates it produces a binary that fails for reasons that
	// have nothing to do with the program (specs/026-register-allocation.md,
	// specs/030-abi.md).
	for _, r := range arm64.AllocatableRegs() {
		t.Allocatable[ClassInt] = t.Allocatable[ClassInt].Add(Arm64Reg(r))
	}
	// F0 to F29. F30 and F31 are the floating-point materialisation pair,
	// taken from the top of the file because specs/030-abi.md gives no role to
	// any register above F15.
	for _, r := range arm64.AllocatableFRegs() {
		t.Allocatable[ClassFloat] = t.Allocatable[ClassFloat].Add(Arm64Reg(r))
	}

	// R16 and R17 are the linker's trampoline scratch, so they are not
	// allocatable and the allocator loses nothing by taking them. A trampoline
	// is inserted at a branch, and a scratch register here is live only inside
	// one straight-line materialisation sequence, which contains no branch.
	t.Scratch[ClassInt] = []Reg{Arm64Reg(arm64.RegTrampLo), Arm64Reg(arm64.RegTrampHi)}
	t.Scratch[ClassFloat] = []Reg{Arm64Reg(arm64.RegFScratchLo), Arm64Reg(arm64.RegFScratchHi)}

	// specs/030-abi.md: R0 to R15 carry integer arguments and results, F0 to
	// F15 the floating-point ones, and results restart the counters.
	for i := 0; i < 16; i++ {
		t.ArgRegs[ClassInt] = append(t.ArgRegs[ClassInt], Arm64Reg(arm64.Reg(i)))
		t.ArgRegs[ClassFloat] = append(t.ArgRegs[ClassFloat], Arm64FReg(i))
	}
	t.ResultRegs[ClassInt] = t.ArgRegs[ClassInt]
	t.ResultRegs[ClassFloat] = t.ArgRegs[ClassFloat]

	// Every register a value can be in is destroyed by a call. The scratch
	// registers are included because a materialised value in one does not
	// survive a call either.
	for c := RegClass(0); c < NumRegClass; c++ {
		t.Clobbers = t.Clobbers.Union(t.Allocatable[c])
		for _, r := range t.Scratch[c] {
			t.Clobbers = t.Clobbers.Add(r)
		}
	}

	t.ClassOf = ClassOfType
	t.IsCall = func(v *Value) bool { return v.Op.IsCall() }
	t.Remat = Rematerialisable
	// arm64 has three-operand instructions, so no destination ever has to be a
	// source. specs/026-register-allocation.md confines the opposite case to
	// one flag, and this is the target that does not set it.
	t.TwoAddress = func(v *Value) bool { return false }
	// specs/030-abi.md's assignment walk is abi.go, and these are the two
	// places its answer reaches the allocator. An incoming argument and a call
	// result are pre-coloured, and a call's operands and a return's values are
	// read where the convention says. The walk is target-neutral and reads
	// ArgRegs, ResultRegs and ClassOf above, so the target names the registers
	// and the policy stays in one file.
	t.DefReg = func(v *Value) (Reg, bool) { return ABIDefReg(t, v) }
	t.UseReg = func(v *Value, i int) (Reg, bool) { return ABIUseReg(t, v, i) }

	return t
}

// ClassOfType returns the register class of t, and false when a value of that
// type does not fit in one register.
//
// The false cases are the multi-word types. Lowering decomposes a string, a
// slice and an interface into their words before allocation; one that arrives
// whole gets a stack slot, which is correct and slow, rather than the first
// word of a register, which is neither.
func ClassOfType(t *ir.Type) (RegClass, bool) {
	if t == nil || t == MemType {
		return ClassInt, false
	}
	switch {
	case t.Kind.IsFloat():
		return ClassFloat, true
	case t.Kind.IsComplex():
		// A complex is two floats. It needs two registers, so it is not one
		// value in one register.
		return ClassFloat, false
	}
	switch t.Kind {
	case ir.Bool, ir.Int8, ir.Int16, ir.Int32, ir.Int64,
		ir.Uint8, ir.Uint16, ir.Uint32, ir.Uint64, ir.Uintptr,
		ir.Ptr, ir.UnsafePtr, ir.Map, ir.Chan, ir.FuncKind:
		return ClassInt, true
	}
	return ClassInt, false
}

// Rematerialisable reports whether v can be recomputed at each use instead of
// being spilled.
//
// specs/026-register-allocation.md names three: a constant, a frame address,
// and a static symbol address. Each is a fixed number of instructions that
// depends on nothing that can change, so recomputing it at a use gives the
// same bits the definition would have.
//
// OpLocalAddr takes memory and is still on the list. The memory argument
// orders it against the stores that write the slot, which is a scheduling
// constraint; the address it computes is the frame pointer plus a constant and
// does not depend on what memory holds.
//
// The two machine address forms are named here rather than marked constant in
// the op table, and the distinction is real: opInfo.constant means "depends on
// nothing", and both of these take an SB or SP argument. The op table enforces
// that constant implies no arguments, so marking them there would be false.
//
// This list is the reason rematerialisation keeps working after lowering. A
// target whose lowering rules produce address forms that are absent from here
// loses rematerialisation for every one of them, and nothing fails: the code
// merely gets worse. specs/026-register-allocation.md records that failure mode
// and the test below is what catches it.
func Rematerialisable(v *Value) bool {
	if v == nil {
		return false
	}
	switch v.Op {
	case OpAddr, OpLocalAddr:
		return true
	case OpARM64MOVDaddr, OpARM64ADDframe:
		return true
	}
	// A multi-word constant is not one register, so recomputing it into one
	// would be a lie. ClassOfType is the same predicate the allocator uses.
	if v.Op.IsConstant() {
		_, ok := ClassOfType(v.Type)
		return ok
	}
	return false
}
