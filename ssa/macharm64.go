// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj/arm64"
)

// The arm64 machine operation set, per specs/042-arm64-backend.md rule groups
// 1 to 6. Atomics and the inline memmove forms are groups 7 and 8 and are not
// here, which is the same line obj/arm64 draws.
//
// Three conventions run through the table.
//
// The operand width is not in the operation. specs/041-instruction-encoding.md
// made the width a parameter of the encoder rather than doubling the function
// count, and this table follows it: ADD covers both the W and the X form and
// the width comes from the value's type. The exception is a value whose type
// says nothing about the width, which is every flag-setting compare, so CMP
// and CMPW are separate operations.
//
// The address modes are separate operations rather than a mode field, because
// each one is a different encoder function with a different immediate range.
// A rule that folds an address into a load changes the operation, and the
// operation is what ARM64Encode switches on.
//
// A condition code lives in Aux and not in AuxInt. It prints as EQ or LO in a
// dump rather than as 0 or 3, and the per-rule tests of specs/025 assert the
// dump.

const (
	// Arithmetic and logic, register and register.
	OpARM64ADD Op = opPseudoEnd + iota
	OpARM64SUB
	OpARM64MUL
	OpARM64MADD // addend + a*b
	OpARM64MSUB // minuend - a*b, which is how a remainder is computed
	OpARM64SDIV
	OpARM64UDIV
	OpARM64AND
	OpARM64ORR
	OpARM64EOR
	OpARM64BIC // a AND NOT b
	OpARM64MVN
	OpARM64NEG
	OpARM64LSL
	OpARM64LSR
	OpARM64ASR

	// The immediate forms. AuxInt holds the immediate, and the rule that
	// creates one has already asked the encoder whether it fits.
	OpARM64ADDconst
	OpARM64SUBconst
	OpARM64ANDconst
	OpARM64ORRconst
	OpARM64EORconst
	OpARM64BICconst
	OpARM64LSLconst
	OpARM64LSRconst
	OpARM64ASRconst
	OpARM64MOVDconst

	// The shifted-register form, which is what a scaled index folds into.
	// AuxInt is the shift amount.
	OpARM64ADDshiftLL

	// Width changes and register moves.
	OpARM64SXTB
	OpARM64SXTH
	OpARM64SXTW
	OpARM64UXTB
	OpARM64UXTH
	OpARM64MOVWUreg // a 32-bit register move, which zeroes the upper half
	OpARM64MOVDreg

	// Flag producers. The result type is FlagsType and it is live only inside
	// its own block.
	OpARM64CMP
	OpARM64CMPW
	OpARM64CMPconst
	OpARM64CMPWconst
	OpARM64CMN
	OpARM64CMNW
	OpARM64CMNconst
	OpARM64CMNWconst
	OpARM64TST
	OpARM64TSTW
	OpARM64TSTconst
	OpARM64TSTWconst

	// CSET turns the flags into a 0 or a 1 in a register. Aux is the
	// condition.
	OpARM64CSET

	// Block controls. BRcond reads the flags, and the two compare-and-branch
	// forms read a register and need no compare in front of them.
	OpARM64BRcond
	OpARM64CBZ
	OpARM64CBNZ

	// Loads. AuxInt is the byte offset from the base register.
	OpARM64MOVDload
	OpARM64MOVWload
	OpARM64MOVWUload
	OpARM64MOVHload
	OpARM64MOVHUload
	OpARM64MOVBload
	OpARM64MOVBUload

	// Stores. The argument order is the one OpStore has, so a store lowers
	// without moving an argument.
	OpARM64MOVDstore
	OpARM64MOVWstore
	OpARM64MOVHstore
	OpARM64MOVBstore

	// The register-offset loads. The plain form adds the index register, the
	// scaled form shifts it by the access width first.
	OpARM64MOVDloadidx
	OpARM64MOVWloadidx
	OpARM64MOVWUloadidx
	OpARM64MOVHloadidx
	OpARM64MOVHUloadidx
	OpARM64MOVBloadidx
	OpARM64MOVBUloadidx
	OpARM64MOVDloadidx8
	OpARM64MOVWloadidx4
	OpARM64MOVWUloadidx4
	OpARM64MOVHloadidx2
	OpARM64MOVHUloadidx2

	// The register-offset stores.
	OpARM64MOVDstoreidx
	OpARM64MOVWstoreidx
	OpARM64MOVHstoreidx
	OpARM64MOVBstoreidx
	OpARM64MOVDstoreidx8
	OpARM64MOVWstoreidx4
	OpARM64MOVHstoreidx2

	// Addresses. Aux is the *ir.Object the address names.
	OpARM64MOVDaddr // ADRP and ADD from the static base
	OpARM64ADDframe // ADD from the stack pointer, offset assigned by the ABI

	// Calls, in the four shapes of specs/042 group 5.
	OpARM64CALLstatic
	OpARM64CALLclosure
	OpARM64CALLinter
	OpARM64CALLdefer

	// The return. It carries the result values so that register allocation
	// can place them, and it is the control of a BlockRet.
	OpARM64RET

	// The nil check is a load that faults. It is not a branch, so it costs one
	// instruction and no block.
	OpARM64LoweredNilCheck

	// Group 6: floating point.
	//
	// The width follows the same convention as the integer forms: FADD covers
	// the single and the double instruction and the width comes from the
	// value's type, because a float32 is four bytes and a float64 is eight.
	// The compares are the exception, as CMP and CMPW are, because a flags
	// value has no width.
	OpARM64FADD
	OpARM64FSUB
	OpARM64FMUL
	OpARM64FDIV
	OpARM64FNEG
	OpARM64FABS
	OpARM64FSQRT

	// The floating-point flag producers. FCMPD0 and FCMPS0 compare against
	// zero, which the class encodes as an opcode and not as an operand, so
	// they take one argument.
	OpARM64FCMPD
	OpARM64FCMPS
	OpARM64FCMPD0
	OpARM64FCMPS0

	// Constants and the two moves between the register files. AuxInt of
	// FMOVconst is the IEEE bit pattern and not a value, because that is what
	// the instruction stream holds and because two NaNs that compare equal as
	// floats are two different constants.
	OpARM64FMOVconst
	OpARM64FMOVgpfp // the bits of an integer register into a floating-point one
	OpARM64FMOVfpgp // and back

	// Conversions. Each takes both widths from the types it stands between,
	// so one operation covers the four combinations the architecture encodes.
	OpARM64SCVTF  // signed integer to floating point
	OpARM64UCVTF  // unsigned integer to floating point
	OpARM64FCVTZS // floating point to signed integer, rounding toward zero
	OpARM64FCVTZU // floating point to unsigned integer
	OpARM64FCVT   // between the two precisions

	// Floating-point loads and stores. They are the integer addressing forms
	// with the transfer register in the other file, so they carry the same
	// AuxInt offset and fold the same way.
	OpARM64FMOVDload
	OpARM64FMOVSload
	OpARM64FMOVDstore
	OpARM64FMOVSstore
	OpARM64FMOVDloadidx
	OpARM64FMOVSloadidx
	OpARM64FMOVDloadidx8
	OpARM64FMOVSloadidx4
	OpARM64FMOVDstoreidx
	OpARM64FMOVSstoreidx
	OpARM64FMOVDstoreidx8
	OpARM64FMOVSstoreidx4

	opARM64End
)

// The operation numbering must stay inside Op, which is a uint8. The set grows
// only when a rule is added, so this is the assertion that says when the type
// has to grow with it.
const _ = uint8(opARM64End - 1)

// arm64Enc names the encoder family an operation reaches.
type arm64Enc uint8

const (
	encNone          arm64Enc = iota // no encoder in obj/arm64 yet
	encArith                         // sz, dst, a, b
	encArith3                        // sz, dst, a, b, c
	encArithImm                      // sz, dst, a, imm
	encLogicalImm                    // sz, dst, a, uimm
	encShiftImm                      // sz, dst, a, shift
	encAddShift                      // sz, dst, a, b, LSL, shift
	encUnary                         // sz, dst, a
	encExt                           // sz, dst, a, with a fixed width
	encMovConst                      // a sequence
	encCmp                           // sz, a, b
	encCmpImm                        // sz, a, imm
	encLogicalCmpImm                 // sz, a, uimm
	encMem                           // MemOp, rt, base, offset
	encMemIdx                        // MemOp, rt, base, index, extend, scaled
	encAddr                          // ADRP and ADD
	encFrame                         // ADD from RSP
	encBranchCond                    // B.cond
	encCompareBranch                 // CBZ, CBNZ
	encCall                          // BL
	encCallInd                       // BLR
	encRet                           // RET
	encFArith                        // sz, dst, a, b in the floating-point file
	encFUnary                        // sz, dst, a
	encFCmp                          // sz, a, b
	encFCmpZero                      // sz, a
	encFConst                        // FMOV with the immediate
	encFMove                         // FMOV between the two register files
	encFCvt                          // a conversion, which reads two widths
)

// arm64Op is one row of the machine operation table.
//
// It holds the shared opInfo the rest of the package reads and the arm64 facts
// that only the encoder and the rules need. One table, because an operation
// with an opInfo and no encoder is the failure specs/041 says must be a build
// failure rather than a crash.
type arm64Op struct {
	info opInfo
	enc  arm64Enc

	// mem is the load or store form, for the memory operations.
	mem arm64.MemOp
	// scaled asks the register-offset form to shift the index by the access
	// width.
	scaled bool
	// width fixes the operand width of an operation whose result type does not
	// say it, which is every compare and every fixed-width register move. Zero
	// means the width comes from the type.
	width int8
}

func ai(name string, argLen int8, enc arm64Enc) arm64Op {
	return arm64Op{info: opInfo{name: name, argLen: argLen}, enc: enc}
}

func (o arm64Op) comm() arm64Op   { o.info.commutative = true; return o }
func (o arm64Op) aux() arm64Op    { o.info.hasAuxInt = true; return o }
func (o arm64Op) takes() arm64Op  { o.info.takesMem = true; return o }
func (o arm64Op) makes() arm64Op  { o.info.makesMem = true; return o }
func (o arm64Op) callop() arm64Op { o.info.call = true; return o }
func (o arm64Op) konst() arm64Op  { o.info.constant = true; return o }
func (o arm64Op) w32() arm64Op    { o.width = 32; return o }
func (o arm64Op) w64() arm64Op    { o.width = 64; return o }
func (o arm64Op) m(x arm64.MemOp) arm64Op {
	o.mem = x
	return o
}
func (o arm64Op) sc() arm64Op { o.scaled = true; return o }

// arm64Ops is indexed by Op - OpARM64ADD. An array and never a map, because
// this table is walked to produce output (specs/053-determinism.md).
var arm64Ops = [opARM64End - OpARM64ADD]arm64Op{
	OpARM64ADD - OpARM64ADD:  ai("ARM64ADD", 2, encArith).comm(),
	OpARM64SUB - OpARM64ADD:  ai("ARM64SUB", 2, encArith),
	OpARM64MUL - OpARM64ADD:  ai("ARM64MUL", 2, encArith).comm(),
	OpARM64MADD - OpARM64ADD: ai("ARM64MADD", 3, encArith3),
	OpARM64MSUB - OpARM64ADD: ai("ARM64MSUB", 3, encArith3),
	OpARM64SDIV - OpARM64ADD: ai("ARM64SDIV", 2, encArith),
	OpARM64UDIV - OpARM64ADD: ai("ARM64UDIV", 2, encArith),
	OpARM64AND - OpARM64ADD:  ai("ARM64AND", 2, encArith).comm(),
	OpARM64ORR - OpARM64ADD:  ai("ARM64ORR", 2, encArith).comm(),
	OpARM64EOR - OpARM64ADD:  ai("ARM64EOR", 2, encArith).comm(),
	OpARM64BIC - OpARM64ADD:  ai("ARM64BIC", 2, encArith),
	OpARM64MVN - OpARM64ADD:  ai("ARM64MVN", 1, encUnary),
	OpARM64NEG - OpARM64ADD:  ai("ARM64NEG", 1, encUnary),
	OpARM64LSL - OpARM64ADD:  ai("ARM64LSL", 2, encArith),
	OpARM64LSR - OpARM64ADD:  ai("ARM64LSR", 2, encArith),
	OpARM64ASR - OpARM64ADD:  ai("ARM64ASR", 2, encArith),

	OpARM64ADDconst - OpARM64ADD:  ai("ARM64ADDconst", 1, encArithImm).aux(),
	OpARM64SUBconst - OpARM64ADD:  ai("ARM64SUBconst", 1, encArithImm).aux(),
	OpARM64ANDconst - OpARM64ADD:  ai("ARM64ANDconst", 1, encLogicalImm).aux(),
	OpARM64ORRconst - OpARM64ADD:  ai("ARM64ORRconst", 1, encLogicalImm).aux(),
	OpARM64EORconst - OpARM64ADD:  ai("ARM64EORconst", 1, encLogicalImm).aux(),
	OpARM64BICconst - OpARM64ADD:  ai("ARM64BICconst", 1, encLogicalImm).aux(),
	OpARM64LSLconst - OpARM64ADD:  ai("ARM64LSLconst", 1, encShiftImm).aux(),
	OpARM64LSRconst - OpARM64ADD:  ai("ARM64LSRconst", 1, encShiftImm).aux(),
	OpARM64ASRconst - OpARM64ADD:  ai("ARM64ASRconst", 1, encShiftImm).aux(),
	OpARM64MOVDconst - OpARM64ADD: ai("ARM64MOVDconst", 0, encMovConst).aux().konst(),

	OpARM64ADDshiftLL - OpARM64ADD: ai("ARM64ADDshiftLL", 2, encAddShift).aux(),

	OpARM64SXTB - OpARM64ADD:     ai("ARM64SXTB", 1, encExt),
	OpARM64SXTH - OpARM64ADD:     ai("ARM64SXTH", 1, encExt),
	OpARM64SXTW - OpARM64ADD:     ai("ARM64SXTW", 1, encExt),
	OpARM64UXTB - OpARM64ADD:     ai("ARM64UXTB", 1, encExt),
	OpARM64UXTH - OpARM64ADD:     ai("ARM64UXTH", 1, encExt),
	OpARM64MOVWUreg - OpARM64ADD: ai("ARM64MOVWUreg", 1, encUnary).w32(),
	OpARM64MOVDreg - OpARM64ADD:  ai("ARM64MOVDreg", 1, encUnary).w64(),

	OpARM64CMP - OpARM64ADD:       ai("ARM64CMP", 2, encCmp).w64(),
	OpARM64CMPW - OpARM64ADD:      ai("ARM64CMPW", 2, encCmp).w32(),
	OpARM64CMPconst - OpARM64ADD:  ai("ARM64CMPconst", 1, encCmpImm).aux().w64(),
	OpARM64CMPWconst - OpARM64ADD: ai("ARM64CMPWconst", 1, encCmpImm).aux().w32(),
	OpARM64CMN - OpARM64ADD:       ai("ARM64CMN", 2, encCmp).w64().comm(),
	OpARM64CMNW - OpARM64ADD:      ai("ARM64CMNW", 2, encCmp).w32().comm(),
	OpARM64CMNconst - OpARM64ADD:  ai("ARM64CMNconst", 1, encCmpImm).aux().w64(),
	OpARM64CMNWconst - OpARM64ADD: ai("ARM64CMNWconst", 1, encCmpImm).aux().w32(),
	OpARM64TST - OpARM64ADD:       ai("ARM64TST", 2, encCmp).w64().comm(),
	OpARM64TSTW - OpARM64ADD:      ai("ARM64TSTW", 2, encCmp).w32().comm(),
	OpARM64TSTconst - OpARM64ADD:  ai("ARM64TSTconst", 1, encLogicalCmpImm).aux().w64(),
	OpARM64TSTWconst - OpARM64ADD: ai("ARM64TSTWconst", 1, encLogicalCmpImm).aux().w32(),

	OpARM64CSET - OpARM64ADD: ai("ARM64CSET", 1, encNone),

	OpARM64BRcond - OpARM64ADD: ai("ARM64BRcond", 1, encBranchCond),
	OpARM64CBZ - OpARM64ADD:    ai("ARM64CBZ", 1, encCompareBranch),
	OpARM64CBNZ - OpARM64ADD:   ai("ARM64CBNZ", 1, encCompareBranch),

	OpARM64MOVDload - OpARM64ADD:  ai("ARM64MOVDload", 2, encMem).aux().takes().m(arm64.LoadX),
	OpARM64MOVWload - OpARM64ADD:  ai("ARM64MOVWload", 2, encMem).aux().takes().m(arm64.LoadWS64),
	OpARM64MOVWUload - OpARM64ADD: ai("ARM64MOVWUload", 2, encMem).aux().takes().m(arm64.LoadWU),
	OpARM64MOVHload - OpARM64ADD:  ai("ARM64MOVHload", 2, encMem).aux().takes().m(arm64.LoadHS64),
	OpARM64MOVHUload - OpARM64ADD: ai("ARM64MOVHUload", 2, encMem).aux().takes().m(arm64.LoadHU),
	OpARM64MOVBload - OpARM64ADD:  ai("ARM64MOVBload", 2, encMem).aux().takes().m(arm64.LoadBS64),
	OpARM64MOVBUload - OpARM64ADD: ai("ARM64MOVBUload", 2, encMem).aux().takes().m(arm64.LoadBU),

	OpARM64MOVDstore - OpARM64ADD: ai("ARM64MOVDstore", 3, encMem).aux().takes().makes().m(arm64.StoreX),
	OpARM64MOVWstore - OpARM64ADD: ai("ARM64MOVWstore", 3, encMem).aux().takes().makes().m(arm64.StoreW),
	OpARM64MOVHstore - OpARM64ADD: ai("ARM64MOVHstore", 3, encMem).aux().takes().makes().m(arm64.StoreH),
	OpARM64MOVBstore - OpARM64ADD: ai("ARM64MOVBstore", 3, encMem).aux().takes().makes().m(arm64.StoreB),

	OpARM64MOVDloadidx - OpARM64ADD:   ai("ARM64MOVDloadidx", 3, encMemIdx).takes().m(arm64.LoadX),
	OpARM64MOVWloadidx - OpARM64ADD:   ai("ARM64MOVWloadidx", 3, encMemIdx).takes().m(arm64.LoadWS64),
	OpARM64MOVWUloadidx - OpARM64ADD:  ai("ARM64MOVWUloadidx", 3, encMemIdx).takes().m(arm64.LoadWU),
	OpARM64MOVHloadidx - OpARM64ADD:   ai("ARM64MOVHloadidx", 3, encMemIdx).takes().m(arm64.LoadHS64),
	OpARM64MOVHUloadidx - OpARM64ADD:  ai("ARM64MOVHUloadidx", 3, encMemIdx).takes().m(arm64.LoadHU),
	OpARM64MOVBloadidx - OpARM64ADD:   ai("ARM64MOVBloadidx", 3, encMemIdx).takes().m(arm64.LoadBS64),
	OpARM64MOVBUloadidx - OpARM64ADD:  ai("ARM64MOVBUloadidx", 3, encMemIdx).takes().m(arm64.LoadBU),
	OpARM64MOVDloadidx8 - OpARM64ADD:  ai("ARM64MOVDloadidx8", 3, encMemIdx).takes().m(arm64.LoadX).sc(),
	OpARM64MOVWloadidx4 - OpARM64ADD:  ai("ARM64MOVWloadidx4", 3, encMemIdx).takes().m(arm64.LoadWS64).sc(),
	OpARM64MOVWUloadidx4 - OpARM64ADD: ai("ARM64MOVWUloadidx4", 3, encMemIdx).takes().m(arm64.LoadWU).sc(),
	OpARM64MOVHloadidx2 - OpARM64ADD:  ai("ARM64MOVHloadidx2", 3, encMemIdx).takes().m(arm64.LoadHS64).sc(),
	OpARM64MOVHUloadidx2 - OpARM64ADD: ai("ARM64MOVHUloadidx2", 3, encMemIdx).takes().m(arm64.LoadHU).sc(),

	OpARM64MOVDstoreidx - OpARM64ADD:  ai("ARM64MOVDstoreidx", 4, encMemIdx).takes().makes().m(arm64.StoreX),
	OpARM64MOVWstoreidx - OpARM64ADD:  ai("ARM64MOVWstoreidx", 4, encMemIdx).takes().makes().m(arm64.StoreW),
	OpARM64MOVHstoreidx - OpARM64ADD:  ai("ARM64MOVHstoreidx", 4, encMemIdx).takes().makes().m(arm64.StoreH),
	OpARM64MOVBstoreidx - OpARM64ADD:  ai("ARM64MOVBstoreidx", 4, encMemIdx).takes().makes().m(arm64.StoreB),
	OpARM64MOVDstoreidx8 - OpARM64ADD: ai("ARM64MOVDstoreidx8", 4, encMemIdx).takes().makes().m(arm64.StoreX).sc(),
	OpARM64MOVWstoreidx4 - OpARM64ADD: ai("ARM64MOVWstoreidx4", 4, encMemIdx).takes().makes().m(arm64.StoreW).sc(),
	OpARM64MOVHstoreidx2 - OpARM64ADD: ai("ARM64MOVHstoreidx2", 4, encMemIdx).takes().makes().m(arm64.StoreH).sc(),

	OpARM64MOVDaddr - OpARM64ADD: ai("ARM64MOVDaddr", 1, encAddr).aux(),
	OpARM64ADDframe - OpARM64ADD: ai("ARM64ADDframe", 1, encFrame).aux(),

	OpARM64CALLstatic - OpARM64ADD:  ai("ARM64CALLstatic", -1, encCall).takes().makes().callop(),
	OpARM64CALLclosure - OpARM64ADD: ai("ARM64CALLclosure", -1, encCallInd).takes().makes().callop(),
	OpARM64CALLinter - OpARM64ADD:   ai("ARM64CALLinter", -1, encCallInd).takes().makes().callop(),
	OpARM64CALLdefer - OpARM64ADD:   ai("ARM64CALLdefer", -1, encCall).takes().makes().callop(),

	OpARM64RET - OpARM64ADD: ai("ARM64RET", -1, encRet).takes().makes(),

	OpARM64LoweredNilCheck - OpARM64ADD: ai("ARM64LoweredNilCheck", 2, encMem).takes().m(arm64.LoadBU),

	OpARM64FADD - OpARM64ADD:  ai("ARM64FADD", 2, encFArith).comm(),
	OpARM64FSUB - OpARM64ADD:  ai("ARM64FSUB", 2, encFArith),
	OpARM64FMUL - OpARM64ADD:  ai("ARM64FMUL", 2, encFArith).comm(),
	OpARM64FDIV - OpARM64ADD:  ai("ARM64FDIV", 2, encFArith),
	OpARM64FNEG - OpARM64ADD:  ai("ARM64FNEG", 1, encFUnary),
	OpARM64FABS - OpARM64ADD:  ai("ARM64FABS", 1, encFUnary),
	OpARM64FSQRT - OpARM64ADD: ai("ARM64FSQRT", 1, encFUnary),

	OpARM64FCMPD - OpARM64ADD:  ai("ARM64FCMPD", 2, encFCmp).w64(),
	OpARM64FCMPS - OpARM64ADD:  ai("ARM64FCMPS", 2, encFCmp).w32(),
	OpARM64FCMPD0 - OpARM64ADD: ai("ARM64FCMPD0", 1, encFCmpZero).w64(),
	OpARM64FCMPS0 - OpARM64ADD: ai("ARM64FCMPS0", 1, encFCmpZero).w32(),

	OpARM64FMOVconst - OpARM64ADD: ai("ARM64FMOVconst", 0, encFConst).aux().konst(),
	OpARM64FMOVgpfp - OpARM64ADD:  ai("ARM64FMOVgpfp", 1, encFMove),
	OpARM64FMOVfpgp - OpARM64ADD:  ai("ARM64FMOVfpgp", 1, encFMove),

	OpARM64SCVTF - OpARM64ADD:  ai("ARM64SCVTF", 1, encFCvt),
	OpARM64UCVTF - OpARM64ADD:  ai("ARM64UCVTF", 1, encFCvt),
	OpARM64FCVTZS - OpARM64ADD: ai("ARM64FCVTZS", 1, encFCvt),
	OpARM64FCVTZU - OpARM64ADD: ai("ARM64FCVTZU", 1, encFCvt),
	OpARM64FCVT - OpARM64ADD:   ai("ARM64FCVT", 1, encFCvt),

	OpARM64FMOVDload - OpARM64ADD:  ai("ARM64FMOVDload", 2, encMem).aux().takes().m(arm64.LoadF64),
	OpARM64FMOVSload - OpARM64ADD:  ai("ARM64FMOVSload", 2, encMem).aux().takes().m(arm64.LoadF32),
	OpARM64FMOVDstore - OpARM64ADD: ai("ARM64FMOVDstore", 3, encMem).aux().takes().makes().m(arm64.StoreF64),
	OpARM64FMOVSstore - OpARM64ADD: ai("ARM64FMOVSstore", 3, encMem).aux().takes().makes().m(arm64.StoreF32),

	OpARM64FMOVDloadidx - OpARM64ADD:   ai("ARM64FMOVDloadidx", 3, encMemIdx).takes().m(arm64.LoadF64),
	OpARM64FMOVSloadidx - OpARM64ADD:   ai("ARM64FMOVSloadidx", 3, encMemIdx).takes().m(arm64.LoadF32),
	OpARM64FMOVDloadidx8 - OpARM64ADD:  ai("ARM64FMOVDloadidx8", 3, encMemIdx).takes().m(arm64.LoadF64).sc(),
	OpARM64FMOVSloadidx4 - OpARM64ADD:  ai("ARM64FMOVSloadidx4", 3, encMemIdx).takes().m(arm64.LoadF32).sc(),
	OpARM64FMOVDstoreidx - OpARM64ADD:  ai("ARM64FMOVDstoreidx", 4, encMemIdx).takes().makes().m(arm64.StoreF64),
	OpARM64FMOVSstoreidx - OpARM64ADD:  ai("ARM64FMOVSstoreidx", 4, encMemIdx).takes().makes().m(arm64.StoreF32),
	OpARM64FMOVDstoreidx8 - OpARM64ADD: ai("ARM64FMOVDstoreidx8", 4, encMemIdx).takes().makes().m(arm64.StoreF64).sc(),
	OpARM64FMOVSstoreidx4 - OpARM64ADD: ai("ARM64FMOVSstoreidx4", 4, encMemIdx).takes().makes().m(arm64.StoreF32).sc(),
}

func init() {
	rows := make([]opInfo, len(arm64Ops))
	for i := range arm64Ops {
		rows[i] = arm64Ops[i].info
	}
	registerOpInfos(OpARM64ADD, rows)
}

// ARM64NumOps is one more than the largest arm64 machine operation. A rule
// table is indexed by operation, and this is the length one has.
func ARM64NumOps() int { return int(opARM64End) }

// IsARM64Op reports whether op is an arm64 machine operation.
func IsARM64Op(op Op) bool { return op >= OpARM64ADD && op < opARM64End }

// arm64Row returns the table row of op.
func arm64Row(op Op) arm64Op {
	if !IsARM64Op(op) {
		return arm64Op{}
	}
	return arm64Ops[op-OpARM64ADD]
}

// ARM64MemOp returns the load or store form of op, and whether op is one.
func ARM64MemOp(op Op) (arm64.MemOp, bool) {
	r := arm64Row(op)
	if r.enc != encMem && r.enc != encMemIdx {
		return 0, false
	}
	return r.mem, true
}

// ARM64MemFits reports whether off is reachable from the base register in the
// operation's own addressing form.
//
// The range is the encoder's, asked rather than restated, which is what
// specs/041 requires of every rule that produces an immediate. The unsigned
// form reaches furthest but only forwards and only on a multiple of the access
// size, and the unscaled form reaches backwards, so both are tried.
func ARM64MemFits(op Op, off int64) bool {
	m, ok := ARM64MemOp(op)
	if !ok {
		return false
	}
	// The probe register has to come from the file the form transfers, or the
	// encoder refuses it for the wrong reason.
	rt := arm64.R0
	if m.IsFloat() {
		rt = arm64.F0
	}
	if _, ok := arm64.MemUnsignedOffset(m, rt, arm64.R1, off); ok {
		return true
	}
	_, ok = arm64.MemUnscaled(m, rt, arm64.R1, off)
	return ok
}

// ARM64Size returns the operand width a value of type t is computed in.
//
// Anything narrower than a word is computed in the 32-bit form, which is what
// the architecture gives: a W register write zeroes the upper half, so a
// narrow value in a register is already in the shape the next instruction
// wants.
func ARM64Size(t *ir.Type) arm64.Size {
	if t != nil && t.Size > 4 {
		return arm64.Size64
	}
	return arm64.Size32
}

// ARM64LoadOp returns the load for a value of type t.
//
// The signedness decides between the two byte and halfword loads, and getting
// it wrong is a value that compares equal to the right one half the time.
func ARM64LoadOp(t *ir.Type) (Op, bool) {
	if t == nil {
		return OpInvalid, false
	}
	if t.Kind.IsFloat() {
		// A floating-point load has no signedness and no narrow form: it
		// fills the register it names.
		switch t.Size {
		case 4:
			return OpARM64FMOVSload, true
		case 8:
			return OpARM64FMOVDload, true
		}
		return OpInvalid, false
	}
	signed := t.Kind.IsSigned()
	switch t.Size {
	case 1:
		if signed {
			return OpARM64MOVBload, true
		}
		return OpARM64MOVBUload, true
	case 2:
		if signed {
			return OpARM64MOVHload, true
		}
		return OpARM64MOVHUload, true
	case 4:
		if signed {
			return OpARM64MOVWload, true
		}
		return OpARM64MOVWUload, true
	case 8:
		return OpARM64MOVDload, true
	}
	return OpInvalid, false
}

// ARM64StoreOpForType returns the store of a value of type t.
//
// The type and not the size, because a four-byte store from an integer
// register and one from a floating-point register are different instructions
// and the size cannot tell them apart. ARM64StoreOp is the size-only form and
// answers for the integer file alone.
func ARM64StoreOpForType(t *ir.Type) (Op, bool) {
	if t == nil {
		return OpInvalid, false
	}
	if t.Kind.IsFloat() {
		switch t.Size {
		case 4:
			return OpARM64FMOVSstore, true
		case 8:
			return OpARM64FMOVDstore, true
		}
		return OpInvalid, false
	}
	return ARM64StoreOp(t.Size)
}

// ARM64StoreOp returns the integer store of a value of the given size. A
// store has no signedness: it writes the low bits and nothing else.
func ARM64StoreOp(size int64) (Op, bool) {
	switch size {
	case 1:
		return OpARM64MOVBstore, true
	case 2:
		return OpARM64MOVHstore, true
	case 4:
		return OpARM64MOVWstore, true
	case 8:
		return OpARM64MOVDstore, true
	}
	return OpInvalid, false
}

// arm64Idx maps a load or store with an offset to its register-offset form.
// The two columns are the plain form, which adds the index, and the scaled
// form, which shifts the index by the access width first.
var arm64Idx = [...]struct {
	base, plain, scaled Op
}{
	{OpARM64MOVDload, OpARM64MOVDloadidx, OpARM64MOVDloadidx8},
	{OpARM64MOVWload, OpARM64MOVWloadidx, OpARM64MOVWloadidx4},
	{OpARM64MOVWUload, OpARM64MOVWUloadidx, OpARM64MOVWUloadidx4},
	{OpARM64MOVHload, OpARM64MOVHloadidx, OpARM64MOVHloadidx2},
	{OpARM64MOVHUload, OpARM64MOVHUloadidx, OpARM64MOVHUloadidx2},
	{OpARM64MOVBload, OpARM64MOVBloadidx, OpInvalid},
	{OpARM64MOVBUload, OpARM64MOVBUloadidx, OpInvalid},
	{OpARM64MOVDstore, OpARM64MOVDstoreidx, OpARM64MOVDstoreidx8},
	{OpARM64MOVWstore, OpARM64MOVWstoreidx, OpARM64MOVWstoreidx4},
	{OpARM64MOVHstore, OpARM64MOVHstoreidx, OpARM64MOVHstoreidx2},
	{OpARM64MOVBstore, OpARM64MOVBstoreidx, OpInvalid},
	{OpARM64FMOVDload, OpARM64FMOVDloadidx, OpARM64FMOVDloadidx8},
	{OpARM64FMOVSload, OpARM64FMOVSloadidx, OpARM64FMOVSloadidx4},
	{OpARM64FMOVDstore, OpARM64FMOVDstoreidx, OpARM64FMOVDstoreidx8},
	{OpARM64FMOVSstore, OpARM64FMOVSstoreidx, OpARM64FMOVSstoreidx4},
}

// ARM64IndexOp returns the register-offset form of a load or store.
//
// scaled asks for the form that shifts the index by the access width. A byte
// access has no scaled form, because a shift of zero is the plain form.
func ARM64IndexOp(op Op, scaled bool) (Op, bool) {
	for _, r := range arm64Idx {
		if r.base != op {
			continue
		}
		out := r.plain
		if scaled {
			out = r.scaled
		}
		return out, out != OpInvalid
	}
	return OpInvalid, false
}

// ARM64MissingEncoder reports that an operation the rules emit has no encoder
// in obj/arm64 yet.
//
// specs/041 says a rule with no encoder must be a build failure rather than a
// crash. It cannot be one here, because the gap is in the other direction: the
// conditional-set instruction is the only way to put a general condition into
// a register on this architecture, and obj/arm64 has no CSET, CSEL or CSINC.
// Lowering emits CSET anyway, because leaving every comparison unlowered would
// hide the whole rule set behind one missing encoder. This function is how the
// gap stays counted: a test asserts that this is the complete list.
func ARM64MissingEncoder(op Op) bool { return arm64Row(op).enc == encNone }

// ARM64Encode writes the instructions for v into out and returns how many.
//
// dst is the register the value was allocated and args are the registers of
// its arguments, both from specs/026-register-allocation.md. It reports false
// when an immediate does not fit, which is a lowering bug: the rule that made
// the value is the one that had to choose a form that fits, and this is the
// check that it did.
//
// Branch and call displacements are not known here. They are relocations, or
// they are filled in by the layout of specs/041, so a zero is encoded and the
// range check runs against it.
func ARM64Encode(v *Value, dst arm64.Reg, args []arm64.Reg, out []uint32) (int, bool) {
	r := arm64Row(v.Op)
	sz := ARM64Size(v.Type)
	switch r.width {
	case 32:
		sz = arm64.Size32
	case 64:
		sz = arm64.Size64
	}
	a := func(i int) arm64.Reg {
		if i < len(args) {
			return args[i]
		}
		return arm64.R0
	}
	one := func(w uint32) (int, bool) { out[0] = w; return 1, true }
	oneIf := func(w uint32, ok bool) (int, bool) {
		if !ok {
			return 0, false
		}
		return one(w)
	}

	switch r.enc {
	case encArith:
		return one(arm64EncArith(v.Op, sz, dst, a(0), a(1)))
	case encArith3:
		if v.Op == OpARM64MADD {
			return one(arm64.Madd(sz, dst, a(0), a(1), a(2)))
		}
		return one(arm64.Msub(sz, dst, a(0), a(1), a(2)))
	case encArithImm:
		if v.Op == OpARM64ADDconst {
			return oneIf(arm64.AddRegImm(sz, dst, a(0), v.AuxInt))
		}
		return oneIf(arm64.SubRegImm(sz, dst, a(0), v.AuxInt))
	case encLogicalImm:
		return oneIf(arm64EncLogicalImm(v.Op, sz, dst, a(0), uint64(v.AuxInt)))
	case encShiftImm:
		switch v.Op {
		case OpARM64LSLconst:
			return oneIf(arm64.LslRegImm(sz, dst, a(0), uint32(v.AuxInt)))
		case OpARM64LSRconst:
			return oneIf(arm64.LsrRegImm(sz, dst, a(0), uint32(v.AuxInt)))
		default:
			return oneIf(arm64.AsrRegImm(sz, dst, a(0), uint32(v.AuxInt)))
		}
	case encAddShift:
		return oneIf(arm64.AddRegRegShift(sz, dst, a(0), a(1), arm64.LSL, uint32(v.AuxInt)))
	case encUnary:
		switch v.Op {
		case OpARM64MVN:
			return one(arm64.MvnReg(sz, dst, a(0)))
		case OpARM64NEG:
			return one(arm64.NegReg(sz, dst, a(0)))
		default:
			return one(arm64.MovRegReg(sz, dst, a(0)))
		}
	case encExt:
		switch v.Op {
		case OpARM64SXTB:
			return one(arm64.SxtbReg(sz, dst, a(0)))
		case OpARM64SXTH:
			return one(arm64.SxthReg(sz, dst, a(0)))
		case OpARM64SXTW:
			return one(arm64.SxtwReg(dst, a(0)))
		case OpARM64UXTB:
			return one(arm64.UxtbReg(sz, dst, a(0)))
		default:
			return one(arm64.UxthReg(sz, dst, a(0)))
		}
	case encMovConst:
		// The bitmask form first: it reaches values such as
		// 0x0000ffff0000ffff in one instruction that the move-wide sequence
		// needs three for. obj/arm64 says so and this is the caller it means.
		if w, ok := arm64.MovLogicalImm(sz, dst, uint64(v.AuxInt)); ok {
			return one(w)
		}
		return arm64.MovConst(sz, dst, v.AuxInt, out), true
	case encCmp:
		return one(arm64EncCmp(v.Op, sz, a(0), a(1)))
	case encCmpImm:
		if v.Op == OpARM64CMPconst || v.Op == OpARM64CMPWconst {
			return oneIf(arm64.CmpRegImm(sz, a(0), v.AuxInt))
		}
		return oneIf(arm64.CmnRegImm(sz, a(0), v.AuxInt))
	case encLogicalCmpImm:
		return oneIf(arm64.TstRegImm(sz, a(0), uint64(v.AuxInt)))
	case encMem:
		rt, base := dst, a(0)
		if r.info.makesMem {
			// A store transfers its value argument and has no destination.
			rt, base = a(1), a(0)
		}
		if v.Op == OpARM64LoweredNilCheck {
			rt = arm64.ZR
		}
		if w, ok := arm64.MemUnsignedOffset(r.mem, rt, base, v.AuxInt); ok {
			return one(w)
		}
		return oneIf(arm64.MemUnscaled(r.mem, rt, base, v.AuxInt))
	case encMemIdx:
		rt := dst
		if r.info.makesMem {
			rt = a(2)
		}
		return oneIf(arm64.MemRegOffset(r.mem, rt, a(0), a(1), arm64.LSLX, r.scaled))
	case encAddr:
		hi, lo, ok := arm64.AdrpAdd(dst, 0, 0)
		if !ok {
			return 0, false
		}
		out[0], out[1] = hi, lo
		return 2, true
	case encFrame:
		return oneIf(arm64.AddRegImm(arm64.Size64, dst, arm64.RSP, v.AuxInt))
	case encBranchCond:
		c, ok := v.Aux.(arm64.Cond)
		if !ok {
			return 0, false
		}
		return oneIf(arm64.BCond(c, 0))
	case encCompareBranch:
		if v.Op == OpARM64CBZ {
			return oneIf(arm64.Cbz(ARM64Size(v.Args[0].Type), a(0), 0))
		}
		return oneIf(arm64.Cbnz(ARM64Size(v.Args[0].Type), a(0), 0))
	case encCall:
		return oneIf(arm64.Bl(0))
	case encCallInd:
		return one(arm64.Blr(a(0)))
	case encRet:
		return one(arm64.Ret(arm64.RegLink))
	case encFArith:
		return one(arm64EncFArith(v.Op, sz, dst, a(0), a(1)))
	case encFUnary:
		switch v.Op {
		case OpARM64FNEG:
			return one(arm64.FnegReg(sz, dst, a(0)))
		case OpARM64FABS:
			return one(arm64.FabsReg(sz, dst, a(0)))
		default:
			return one(arm64.FsqrtReg(sz, dst, a(0)))
		}
	case encFCmp:
		return one(arm64.FcmpRegReg(sz, a(0), a(1)))
	case encFCmpZero:
		return one(arm64.FcmpZero(sz, a(0)))
	case encFConst:
		return oneIf(arm64.FmovImm(sz, dst, arm64.FloatFromBits(sz, uint64(v.AuxInt))))
	case encFMove:
		if v.Op == OpARM64FMOVgpfp {
			return one(arm64.FmovIntToFloat(sz, dst, a(0)))
		}
		return one(arm64.FmovFloatToInt(sz, dst, a(0)))
	case encFCvt:
		// A conversion stands between two widths and reads both of them from
		// the types it joins, which is why it is the one family that does not
		// use the single sz above.
		from, to := ARM64Size(v.Args[0].Type), ARM64Size(v.Type)
		switch v.Op {
		case OpARM64SCVTF:
			return one(arm64.ScvtfReg(from, to, dst, a(0)))
		case OpARM64UCVTF:
			return one(arm64.UcvtfReg(from, to, dst, a(0)))
		case OpARM64FCVTZS:
			return one(arm64.FcvtzsReg(from, to, dst, a(0)))
		case OpARM64FCVTZU:
			return one(arm64.FcvtzuReg(from, to, dst, a(0)))
		default:
			return oneIf(arm64.FcvtReg(from, to, dst, a(0)))
		}
	}
	return 0, false
}

func arm64EncFArith(op Op, sz arm64.Size, dst, x, y arm64.Reg) uint32 {
	switch op {
	case OpARM64FADD:
		return arm64.FaddRegReg(sz, dst, x, y)
	case OpARM64FSUB:
		return arm64.FsubRegReg(sz, dst, x, y)
	case OpARM64FMUL:
		return arm64.FmulRegReg(sz, dst, x, y)
	}
	return arm64.FdivRegReg(sz, dst, x, y)
}

func arm64EncArith(op Op, sz arm64.Size, dst, x, y arm64.Reg) uint32 {
	switch op {
	case OpARM64ADD:
		return arm64.AddRegReg(sz, dst, x, y)
	case OpARM64SUB:
		return arm64.SubRegReg(sz, dst, x, y)
	case OpARM64MUL:
		return arm64.MulRegReg(sz, dst, x, y)
	case OpARM64SDIV:
		return arm64.SdivRegReg(sz, dst, x, y)
	case OpARM64UDIV:
		return arm64.UdivRegReg(sz, dst, x, y)
	case OpARM64AND:
		return arm64.AndRegReg(sz, dst, x, y)
	case OpARM64ORR:
		return arm64.OrrRegReg(sz, dst, x, y)
	case OpARM64EOR:
		return arm64.EorRegReg(sz, dst, x, y)
	case OpARM64BIC:
		return arm64.BicRegReg(sz, dst, x, y)
	case OpARM64LSL:
		return arm64.LslRegReg(sz, dst, x, y)
	case OpARM64LSR:
		return arm64.LsrRegReg(sz, dst, x, y)
	}
	return arm64.AsrRegReg(sz, dst, x, y)
}

func arm64EncLogicalImm(op Op, sz arm64.Size, dst, x arm64.Reg, val uint64) (uint32, bool) {
	switch op {
	case OpARM64ANDconst:
		return arm64.AndRegImm(sz, dst, x, val)
	case OpARM64ORRconst:
		return arm64.OrrRegImm(sz, dst, x, val)
	case OpARM64EORconst:
		return arm64.EorRegImm(sz, dst, x, val)
	}
	return arm64.BicRegImm(sz, dst, x, val)
}

func arm64EncCmp(op Op, sz arm64.Size, x, y arm64.Reg) uint32 {
	switch op {
	case OpARM64CMP, OpARM64CMPW:
		return arm64.CmpRegReg(sz, x, y)
	case OpARM64CMN, OpARM64CMNW:
		return arm64.CmnRegReg(sz, x, y)
	}
	return arm64.TstRegReg(sz, x, y)
}

// ARM64ImmFits reports whether a rule may use the immediate form of op with
// the value imm.
//
// Every answer comes from the encoder. That is the whole point:
// specs/041-instruction-encoding.md says each range is a constant in the
// encoder and a condition in the rule, written once and referenced. Two of the
// answers are not the ones a reader expects, and both come out right because
// the encoder is asked rather than a range restated:
//
//   - BIC has no immediate form in the architecture. It is AND of the
//     complement, so the range that matters is the complement's.
//   - A logical immediate can represent neither zero nor all-ones, whatever
//     its width, because the encoded run of ones can be neither empty nor fill
//     the element.
func ARM64ImmFits(op Op, sz arm64.Size, imm int64) bool {
	switch op {
	case OpARM64ADDconst:
		_, ok := arm64.AddRegImm(sz, arm64.R0, arm64.R1, imm)
		return ok
	case OpARM64SUBconst:
		_, ok := arm64.SubRegImm(sz, arm64.R0, arm64.R1, imm)
		return ok
	case OpARM64CMPconst, OpARM64CMPWconst:
		_, ok := arm64.CmpRegImm(sz, arm64.R0, imm)
		return ok
	case OpARM64CMNconst, OpARM64CMNWconst:
		_, ok := arm64.CmnRegImm(sz, arm64.R0, imm)
		return ok
	case OpARM64ANDconst:
		_, ok := arm64.AndRegImm(sz, arm64.R0, arm64.R1, uint64(imm))
		return ok
	case OpARM64ORRconst:
		_, ok := arm64.OrrRegImm(sz, arm64.R0, arm64.R1, uint64(imm))
		return ok
	case OpARM64EORconst:
		_, ok := arm64.EorRegImm(sz, arm64.R0, arm64.R1, uint64(imm))
		return ok
	case OpARM64BICconst:
		_, ok := arm64.BicRegImm(sz, arm64.R0, arm64.R1, uint64(imm))
		return ok
	case OpARM64TSTconst, OpARM64TSTWconst:
		_, ok := arm64.TstRegImm(sz, arm64.R0, uint64(imm))
		return ok
	case OpARM64LSLconst:
		_, ok := arm64.LslRegImm(sz, arm64.R0, arm64.R1, uint32(imm))
		return imm >= 0 && ok
	case OpARM64LSRconst:
		_, ok := arm64.LsrRegImm(sz, arm64.R0, arm64.R1, uint32(imm))
		return imm >= 0 && ok
	case OpARM64ASRconst:
		_, ok := arm64.AsrRegImm(sz, arm64.R0, arm64.R1, uint32(imm))
		return imm >= 0 && ok
	case OpARM64ADDshiftLL:
		_, ok := arm64.AddRegRegShift(sz, arm64.R0, arm64.R1, arm64.R2, arm64.LSL, uint32(imm))
		return imm >= 0 && ok
	}
	return false
}
