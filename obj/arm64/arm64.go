// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package arm64 encodes arm64 machine instructions.
//
// Every arm64 instruction is 4 bytes, so an encoder is a pure function from
// operands to a uint32. Instruction layout needs no iteration, which is what
// specs/041-instruction-encoding.md means when it says arm64 needs no size
// fixed point.
//
// The package covers the instructions a first code generator emits: rule
// groups 1 to 6 of specs/042-arm64-backend.md. Atomics and the inline memmove
// forms (groups 7 and 8) are not here yet.
//
// # The contract on immediates
//
// A form that takes an immediate returns (uint32, bool). The bool is false
// when the value does not fit the field. It is not an error to ask: the
// lowering rules choose the form, and the encoder is the check that they
// chose one that fits. A rule that produces an out-of-range immediate is a
// compiler bug, and it is caught here rather than silently truncated into an
// instruction that runs and computes the wrong answer.
//
// Registers are different. A Reg is an enum, so passing one a form cannot
// encode is a bug in the caller's types, not a value that came from a Go
// program. Those cases panic.
//
// # Register 31
//
// Register 31 is the zero register in most instruction classes and the stack
// pointer in a few. The two are distinct Reg values here, ZR and RSP, and each
// encoder accepts only the one its class defines. Silently accepting either
// produces an instruction that assembles, disassembles, and touches the wrong
// storage.
//
// # The two register files
//
// Reg numbers the integer registers and the floating-point registers in one
// enum, because the register allocator wants one number per register and one
// bitset over all of them. The three encoding helpers keep the files apart:
// regZ and regSP accept only an integer register and regF only a
// floating-point one. Register number 5 names X5 in one instruction and D5 in
// another, and an encoder that accepted either would read the wrong file with
// no diagnostic at all.
//
// The architecture calls the floating-point file V0 to V31, with S and D
// naming its 32-bit and 64-bit views. Go's Plan 9 assembler and
// specs/030-abi.md spell the same registers F0 to F31, and this package
// follows that spelling so that one name serves the ABI table, a dump, and the
// differential test against go tool asm. The view is not in the name: it is
// the Size the encoder is given, which is the same parameter that selects W
// or X for an integer instruction.
package arm64

import "strconv"

// Reg is a machine register, integer or floating-point.
//
// R0 to R30 encode as their own number. ZR and RSP both encode as 31 and are
// separate values here for the reason the package comment gives. F0 to F31
// encode as 0 to 31 in the floating-point file, which is a different file
// from the integer one and not a different range of the same one.
type Reg uint8

// The integer registers. Roles are from specs/030-abi.md.
const (
	R0 Reg = iota
	R1
	R2
	R3
	R4
	R5
	R6
	R7
	R8
	R9
	R10
	R11
	R12
	R13
	R14
	R15
	R16 // linker trampoline scratch
	R17 // linker trampoline scratch
	R18 // reserved by darwin, never allocated
	R19
	R20
	R21
	R22
	R23
	R24
	R25
	R26 // closure context at a call
	R27 // assembler scratch for expanded instructions
	R28 // current goroutine
	R29 // frame pointer
	R30 // link register
	ZR  // reads as zero, writes discarded
	RSP // stack pointer, 16-byte aligned always

	// The floating-point file, V0 to V31 in the manual. F0 to F15 carry
	// floating-point arguments and results and F16 to F31 have no ABI role
	// (specs/030-abi.md, specs/042-arm64-backend.md).
	F0
	F1
	F2
	F3
	F4
	F5
	F6
	F7
	F8
	F9
	F10
	F11
	F12
	F13
	F14
	F15
	F16
	F17
	F18
	F19
	F20
	F21
	F22
	F23
	F24
	F25
	F26
	F27
	F28
	F29
	F30 // materialisation scratch, never allocated
	F31 // materialisation scratch, never allocated

	numReg
)

// NumIntReg is the number of integer register spellings, which is also the
// number of the first floating-point register. Nothing here depends on the
// two files being adjacent, but the register allocator numbers registers with
// these values and a test pins the identity.
const NumIntReg = int(F0)

// Aliases for the registers that carry an ABI role.
const (
	RegClosure    = R26
	RegG          = R28
	RegFramePtr   = R29
	RegLink       = R30
	RegTrampHi    = R17
	RegTrampLo    = R16
	RegPlatform   = R18
	RegAsmScratch = R27

	// RegScratchThird is the third integer register the allocator holds back
	// for materialisation. R16 and R17 are the first two and cost nothing,
	// because the linker reserves them already. This one is not free: it comes
	// out of the allocatable set.
	//
	// A third is needed because an instruction can read three registers.
	// MOVDstoreidx8 reads a base, an index and the value to store, and MADD
	// and MSUB read three, so a program whose three operands are all in slots
	// or all rematerialised needs three registers to read them into at once.
	// `var a [4]int; a[2] = 7` is such a program.
	//
	// R25 is the register to lose. It is the top of the range
	// specs/030-abi.md gives no role to, so taking it costs one general
	// register and touches no argument, no result, and no runtime register.
	// R27 is not available for this: the assembler and the linker write it
	// when they expand an instruction or insert a trampoline, and a value read
	// into it before an instruction that the linker then expands would be
	// destroyed between the read and the use.
	RegScratchThird = R25

	// RegWBBuf is the register runtime.gcWriteBarrier2 returns its buffer
	// pointer in. It is R25, and that is not a coincidence to be tidied away:
	// the runtime chose the register and this compiler only reads it.
	//
	// It is the same register as RegScratchThird, which is why
	// specs/042-arm64-backend.md makes the call and the read of it one
	// operation. A reload staged through R25 between the two would destroy the
	// pointer, and the reload is the allocator's to place.
	RegWBBuf = R25

	// The floating-point pair the allocator holds back for materialisation,
	// which is what R16 and R17 are for the integer file. F30 and F31 are
	// taken because specs/030-abi.md gives no role to any register above F15,
	// so the pair costs nothing that an argument register would.
	RegFScratchLo = F30
	RegFScratchHi = F31
)

// regNames is indexed by Reg. An array, not a map, so that any output derived
// from it is deterministic (specs/053-determinism.md).
var regNames = [numReg]string{
	R0: "R0", R1: "R1", R2: "R2", R3: "R3", R4: "R4", R5: "R5", R6: "R6", R7: "R7",
	R8: "R8", R9: "R9", R10: "R10", R11: "R11", R12: "R12", R13: "R13", R14: "R14", R15: "R15",
	R16: "R16", R17: "R17", R18: "R18", R19: "R19", R20: "R20", R21: "R21", R22: "R22", R23: "R23",
	R24: "R24", R25: "R25", R26: "R26", R27: "R27", R28: "R28", R29: "R29", R30: "R30",
	ZR: "ZR", RSP: "RSP",
	F0: "F0", F1: "F1", F2: "F2", F3: "F3", F4: "F4", F5: "F5", F6: "F6", F7: "F7",
	F8: "F8", F9: "F9", F10: "F10", F11: "F11", F12: "F12", F13: "F13", F14: "F14", F15: "F15",
	F16: "F16", F17: "F17", F18: "F18", F19: "F19", F20: "F20", F21: "F21", F22: "F22", F23: "F23",
	F24: "F24", F25: "F25", F26: "F26", F27: "F27", F28: "F28", F29: "F29", F30: "F30", F31: "F31",
}

// allocatable is indexed by Reg. The false entries are specs/042's table: the
// two linker trampoline scratch registers, the third materialisation register,
// darwin's reserved register, the closure, goroutine, frame pointer and link
// registers, the assembler's scratch, and the two spellings of register 31.
//
// Every floating-point register carries a value except F30 and F31, which are
// the materialisation pair. specs/042 marks F0 to F15 as the argument and
// result registers and F16 to F31 as scratch, and both halves are allocatable:
// an argument register is a register like any other between calls.
var allocatable = [numReg]bool{
	R0: true, R1: true, R2: true, R3: true, R4: true, R5: true, R6: true, R7: true,
	R8: true, R9: true, R10: true, R11: true, R12: true, R13: true, R14: true, R15: true,
	R19: true, R20: true, R21: true, R22: true, R23: true, R24: true,
	F0: true, F1: true, F2: true, F3: true, F4: true, F5: true, F6: true, F7: true,
	F8: true, F9: true, F10: true, F11: true, F12: true, F13: true, F14: true, F15: true,
	F16: true, F17: true, F18: true, F19: true, F20: true, F21: true, F22: true, F23: true,
	F24: true, F25: true, F26: true, F27: true, F28: true, F29: true,
}

// Valid reports whether r names a register this package can encode.
func (r Reg) Valid() bool { return r < numReg }

// Allocatable reports whether the register allocator may choose r.
//
// R18 is false on every platform this package targets. darwin reserves it, and
// a program that writes it corrupts platform state in a way that shows up far
// from the store (specs/030-abi.md).
func (r Reg) Allocatable() bool { return r.Valid() && allocatable[r] }

func (r Reg) String() string {
	if !r.Valid() {
		return "Reg(" + strconv.Itoa(int(r)) + ")"
	}
	return regNames[r]
}

// IsFloat reports whether r names a floating-point register.
func (r Reg) IsFloat() bool { return r >= F0 && r < numReg }

// AllocatableRegs returns the allocatable integer registers in increasing
// order.
//
// The order is fixed rather than derived from a map so that a caller that
// walks it produces the same output on every run.
//
// The two files have separate accessors because the register allocator keeps
// a separate free list per class (specs/026-register-allocation.md), so a
// caller always wants one file or the other and never both mixed together.
func AllocatableRegs() []Reg { return allocatableIn(R0, F0) }

// AllocatableFRegs returns the allocatable floating-point registers in
// increasing order.
func AllocatableFRegs() []Reg { return allocatableIn(F0, numReg) }

func allocatableIn(lo, hi Reg) []Reg {
	out := make([]Reg, 0, hi-lo)
	for r := lo; r < hi; r++ {
		if allocatable[r] {
			out = append(out, r)
		}
	}
	return out
}

// Cond is a condition code. The values are the 4-bit cond field.
type Cond uint8

// The condition codes, in encoding order.
const (
	EQ Cond = iota // equal
	NE             // not equal
	HS             // unsigned higher or same, also CS
	LO             // unsigned lower, also CC
	MI             // negative
	PL             // positive or zero
	VS             // overflow set
	VC             // overflow clear
	HI             // unsigned higher
	LS             // unsigned lower or same
	GE             // signed greater or equal
	LT             // signed less than
	GT             // signed greater than
	LE             // signed less or equal
	AL             // always
	NV             // always, the never encoding that behaves as always

	numCond
)

var condNames = [numCond]string{
	EQ: "EQ", NE: "NE", HS: "HS", LO: "LO", MI: "MI", PL: "PL", VS: "VS", VC: "VC",
	HI: "HI", LS: "LS", GE: "GE", LT: "LT", GT: "GT", LE: "LE", AL: "AL", NV: "NV",
}

// Valid reports whether c is one of the 16 condition codes.
func (c Cond) Valid() bool { return c < numCond }

func (c Cond) String() string {
	if !c.Valid() {
		return "Cond(" + strconv.Itoa(int(c)) + ")"
	}
	return condNames[c]
}

// Invert returns the condition that is true exactly when c is false.
//
// The encoding puts each pair next to the other, so flipping the low bit is
// the whole operation. AL and NV are the exception: they are the pair that has
// no complement, and inverting either is a caller bug.
func (c Cond) Invert() Cond { return c ^ 1 }

// Size is the operand width of a data-processing instruction. It is the sf bit
// of the encoding: 0 selects the 32-bit W registers, 1 the 64-bit X registers.
type Size uint8

// The two operand widths.
const (
	Size32 Size = 0
	Size64 Size = 1
)

func (s Size) sf() uint32 { return uint32(s&1) << 31 }

// bits returns the datasize the operand width implies.
func (s Size) bits() uint32 {
	if s == Size64 {
		return 64
	}
	return 32
}

func (s Size) String() string {
	if s == Size64 {
		return "64"
	}
	return "32"
}

// Shift names a shift applied to the second source register of a
// data-processing instruction. The values are the 2-bit shift field.
type Shift uint8

// The shift kinds. ROR is encodable only in the logical shifted-register
// class; the add and subtract class rejects it.
const (
	LSL Shift = iota
	LSR
	ASR
	ROR

	numShift
)

var shiftNames = [numShift]string{LSL: "LSL", LSR: "LSR", ASR: "ASR", ROR: "ROR"}

func (s Shift) String() string {
	if s >= numShift {
		return "Shift(" + strconv.Itoa(int(s)) + ")"
	}
	return shiftNames[s]
}

// Extend names the extension applied to the index register of a
// register-offset load or store. The values are the 3-bit option field.
type Extend uint8

// The extensions a memory operand accepts. The narrower byte and halfword
// extends exist in the architecture but never index memory, so they are not
// here.
const (
	UXTW Extend = 2 // zero-extend the low 32 bits
	LSLX Extend = 3 // use the whole 64-bit register, the plain form
	SXTW Extend = 6 // sign-extend the low 32 bits
	SXTX Extend = 7 // use the whole 64-bit register, spelled as an extend
)

func (e Extend) String() string {
	switch e {
	case UXTW:
		return "UXTW"
	case LSLX:
		return "LSL"
	case SXTW:
		return "SXTW"
	case SXTX:
		return "SXTX"
	}
	return "Extend(" + strconv.Itoa(int(e)) + ")"
}
