// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package arm64

import "math"

// The floating-point instructions, rule group 6 of
// specs/042-arm64-backend.md.
//
// One thing runs through every encoding here and is worth stating once. The
// architecture puts the operand width in a 2-bit ftype field at bits 23:22,
// where single precision is 0 and double is 1. That is the same numbering
// Size uses for the W and X registers, so the Size parameter that selects an
// integer width also selects a floating-point one and the package needs no
// second width type. Half precision is ftype 3, has no Go type, and is
// therefore unreachable.
//
// The loads and stores are not here. They are the integer addressing forms
// with the V bit set and the transfer register read from the other file, so
// they are MemOp rows in encode.go rather than a second set of functions:
// LoadF32, LoadF64, StoreF32 and StoreF64 go through MemUnsignedOffset,
// MemUnscaled, MemPreIndex, MemPostIndex and MemRegOffset unchanged.

// ftype returns the 2-bit type field a floating-point instruction carries at
// bits 23:22.
func (s Size) ftype() uint32 { return uint32(s&1) << 22 }

// ---------------------------------------------------------------------------
// Floating-point data processing, one source
//
// ARM class "Floating-point data-processing (1 source)". Layout:
//
//	0 0 0 1 1 1 1 0 ftype 1 opcode 1 0 0 0 0 Rn Rd
//
// opcode at bits 20:15 names the operation. FCVT is four of its values: the
// low two bits of the opcode are the destination ftype, which is why the
// conversion between the two precisions lives in this class rather than in a
// class of its own.

const (
	opFpData1 = 0x1e204000

	opcFmov  = 0
	opcFabs  = 1
	opcFneg  = 2
	opcFsqrt = 3
	opcFcvt  = 4 // or the destination ftype into the low two bits
)

func fpData1(sz Size, opcode, rd, rn uint32) uint32 {
	return opFpData1 | sz.ftype() | opcode<<15 | rn<<5 | rd
}

// FmovRegReg encodes FMOV dst, src between two floating-point registers.
//
// It moves the whole register at the named width and it is not an arithmetic
// operation: a NaN keeps its payload and a signalling NaN does not trap.
func FmovRegReg(sz Size, dst, src Reg) uint32 {
	return fpData1(sz, opcFmov, regF(dst), regF(src))
}

// FabsReg encodes FABS dst, src, which clears the sign bit. It is defined on
// a NaN, where it clears the sign and keeps the payload.
func FabsReg(sz Size, dst, src Reg) uint32 {
	return fpData1(sz, opcFabs, regF(dst), regF(src))
}

// FnegReg encodes FNEG dst, src, which inverts the sign bit.
//
// It is not a subtraction from zero and the difference is observable: 0.0 -
// (+0.0) is +0.0 under the default rounding mode, where Go's -x of +0.0 is
// -0.0. The sign flip is the only correct lowering of a floating-point
// negation.
func FnegReg(sz Size, dst, src Reg) uint32 {
	return fpData1(sz, opcFneg, regF(dst), regF(src))
}

// FsqrtReg encodes FSQRT dst, src.
func FsqrtReg(sz Size, dst, src Reg) uint32 {
	return fpData1(sz, opcFsqrt, regF(dst), regF(src))
}

// FcvtReg encodes FCVT dst, src, the conversion between the two precisions.
//
// from is the width of src and to the width of dst. They must differ: the
// architecture has no FCVT from a width to itself, and the encoding a caller
// would get by asking for one is the reserved opcode of that ftype.
func FcvtReg(from, to Size, dst, src Reg) (uint32, bool) {
	if from&1 == to&1 {
		return 0, false
	}
	return fpData1(from, opcFcvt|uint32(to&1), regF(dst), regF(src)), true
}

// ---------------------------------------------------------------------------
// Floating-point data processing, two sources
//
// ARM class "Floating-point data-processing (2 source)". Layout:
//
//	0 0 0 1 1 1 1 0 ftype 1 Rm opcode 1 0 Rn Rd
//
// opcode is at bits 15:12. The four this package needs are the arithmetic
// ones; the minimum, maximum and multiply-subtract forms are in the same
// class and no rule emits them yet.

const (
	opFpData2 = 0x1e200800

	opcFmul = 0
	opcFdiv = 1
	opcFadd = 2
	opcFsub = 3
)

func fpData2(sz Size, opcode, rd, rn, rm uint32) uint32 {
	return opFpData2 | sz.ftype() | rm<<16 | opcode<<12 | rn<<5 | rd
}

// FaddRegReg encodes FADD dst, a, b.
func FaddRegReg(sz Size, dst, a, b Reg) uint32 {
	return fpData2(sz, opcFadd, regF(dst), regF(a), regF(b))
}

// FsubRegReg encodes FSUB dst, a, b, which computes a - b.
func FsubRegReg(sz Size, dst, a, b Reg) uint32 {
	return fpData2(sz, opcFsub, regF(dst), regF(a), regF(b))
}

// FmulRegReg encodes FMUL dst, a, b.
func FmulRegReg(sz Size, dst, a, b Reg) uint32 {
	return fpData2(sz, opcFmul, regF(dst), regF(a), regF(b))
}

// FdivRegReg encodes FDIV dst, a, b, which computes a / b.
//
// Division by zero is defined: it produces an infinity of the right sign, or
// a NaN when the numerator is zero too. There is no trap and no check, which
// is the opposite of the integer divide.
func FdivRegReg(sz Size, dst, a, b Reg) uint32 {
	return fpData2(sz, opcFdiv, regF(dst), regF(a), regF(b))
}

// ---------------------------------------------------------------------------
// Floating-point compare
//
// ARM class "Floating-point compare". Layout:
//
//	0 0 0 1 1 1 1 0 ftype 1 Rm op 1 0 0 0 Rn opcode2
//
// opcode2 at bits 4:0 picks the four forms: compare against a register or
// against zero, each of them quiet or signalling.
//
// The flags are the point of the instruction and they carry four outcomes,
// not three. Writing NZCV in that order:
//
//	less than    1 0 0 0
//	equal        0 1 1 0
//	greater than 0 0 1 0
//	unordered    0 0 1 1
//
// Unordered is the case where either operand is a NaN, and V is set only
// there. That is what makes an IEEE 754 comparison expressible: the condition
// a Go operator lowers to has to be false in the unordered row for every
// ordered operator and true for the not-equal one. specs/042's rule group 6
// names MI for less-than and LS for less-or-equal for that reason, where the
// integer forms use LT and LE: LT and LE are both true in the unordered row,
// because N != V holds there.

const (
	opFpCmp = 0x1e202000

	opcode2FcmpReg   = 0x00
	opcode2FcmpZero  = 0x08
	opcode2FcmpeReg  = 0x10
	opcode2FcmpeZero = 0x18
)

func fpCmp(sz Size, rn, rm, opcode2 uint32) uint32 {
	return opFpCmp | sz.ftype() | rm<<16 | rn<<5 | opcode2
}

// FcmpRegReg encodes FCMP a, b, the quiet compare. It raises the invalid
// operation exception only for a signalling NaN, which is the comparison Go's
// operators mean.
func FcmpRegReg(sz Size, a, b Reg) uint32 {
	return fpCmp(sz, regF(a), regF(b), opcode2FcmpReg)
}

// FcmpZero encodes FCMP a, #0.0.
//
// The immediate is not a field: the only immediate the class encodes is zero,
// and opcode2 is what says so. A comparison against negative zero uses this
// form too, because IEEE 754 makes +0.0 and -0.0 compare equal, so the sign
// of the constant cannot change any of the four outcomes.
func FcmpZero(sz Size, a Reg) uint32 {
	return fpCmp(sz, regF(a), 0, opcode2FcmpZero)
}

// FcmpeRegReg encodes FCMPE a, b, the signalling compare. It raises the
// invalid operation exception for any NaN, which is what an ordered
// comparison in a language that traps would use. Go does not trap, so no rule
// emits it; it is here because the class is one encoding and leaving out half
// of it would hide the difference.
func FcmpeRegReg(sz Size, a, b Reg) uint32 {
	return fpCmp(sz, regF(a), regF(b), opcode2FcmpeReg)
}

// FcmpeZero encodes FCMPE a, #0.0.
func FcmpeZero(sz Size, a Reg) uint32 {
	return fpCmp(sz, regF(a), 0, opcode2FcmpeZero)
}

// ---------------------------------------------------------------------------
// Conversion between floating-point and integer
//
// ARM class "Conversion between floating-point and integer". Layout:
//
//	sf 0 0 1 1 1 1 0 ftype 1 rmode opcode 0 0 0 0 0 0 Rn Rd
//
// sf at bit 31 is the width of the integer register and ftype the width of
// the floating-point one, so the two widths are independent and every one of
// the four combinations is a real instruction. rmode at bits 20:19 is the
// rounding mode and opcode at bits 18:16 the direction.
//
// The register files differ per direction, which is why each function names
// which of its two operands is which.

const (
	opFpInt = 0x1e200000

	rmodeNearest = 0 // the current rounding mode, which Go leaves at nearest
	rmodeZero    = 3 // round toward zero, which is Go's integer conversion

	opcScvtf  = 2
	opcUcvtf  = 3
	opcFcvtzs = 0
	opcFcvtzu = 1
	opcFmovFI = 6 // floating point to integer, moving the bits
	opcFmovIF = 7 // integer to floating point, moving the bits
)

func fpInt(isz, fsz Size, rmode, opcode, rd, rn uint32) uint32 {
	return opFpInt | isz.sf() | fsz.ftype() | rmode<<19 | opcode<<16 | rn<<5 | rd
}

// ScvtfReg encodes SCVTF dst, src, the conversion of a signed integer to a
// floating-point value. from is the width of the integer register and to the
// width of the result.
//
// The result is rounded to nearest when the integer has more significant bits
// than the format holds, which is the rounding Go's specification requires of
// a conversion from int to float.
func ScvtfReg(from, to Size, dst, src Reg) uint32 {
	return fpInt(from, to, rmodeNearest, opcScvtf, regF(dst), regZ(src))
}

// UcvtfReg encodes UCVTF dst, src, the conversion of an unsigned integer.
func UcvtfReg(from, to Size, dst, src Reg) uint32 {
	return fpInt(from, to, rmodeNearest, opcUcvtf, regF(dst), regZ(src))
}

// FcvtzsReg encodes FCVTZS dst, src, the conversion of a floating-point value
// to a signed integer, rounding toward zero. from is the width of the
// floating-point register and to the width of the integer result.
//
// The instruction saturates: a value above the largest representable integer
// produces that integer, a value below the smallest produces that, and a NaN
// produces zero. Go leaves the result of a conversion whose value is not
// representable in the destination type unspecified, so no rule adds a range
// check in front of this. What the rules rely on is only that the in-range
// case truncates toward zero, which the instruction does.
func FcvtzsReg(from, to Size, dst, src Reg) uint32 {
	return fpInt(to, from, rmodeZero, opcFcvtzs, regZ(dst), regF(src))
}

// FcvtzuReg encodes FCVTZU dst, src, the conversion to an unsigned integer.
// It saturates the same way, with a negative value producing zero.
func FcvtzuReg(from, to Size, dst, src Reg) uint32 {
	return fpInt(to, from, rmodeZero, opcFcvtzu, regZ(dst), regF(src))
}

// FmovIntToFloat encodes FMOV dst, src from an integer register into a
// floating-point one, moving the bits and not the value.
//
// The two widths are one parameter because the class has no encoding that
// mixes them: a 32-bit general register pairs with S and a 64-bit one with D.
// This is the instruction a bit conversion such as math.Float64frombits
// becomes, and it is also how a constant that FMOV's immediate cannot reach
// arrives in a floating-point register.
func FmovIntToFloat(sz Size, dst, src Reg) uint32 {
	return fpInt(sz, sz, rmodeNearest, opcFmovIF, regF(dst), regZ(src))
}

// FmovFloatToInt encodes FMOV dst, src from a floating-point register into an
// integer one, moving the bits.
func FmovFloatToInt(sz Size, dst, src Reg) uint32 {
	return fpInt(sz, sz, rmodeNearest, opcFmovFI, regZ(dst), regF(src))
}

// ---------------------------------------------------------------------------
// Floating-point immediate
//
// ARM class "Floating-point immediate". Layout:
//
//	0 0 0 1 1 1 1 0 ftype 1 imm8 1 0 0 0 0 0 0 0 Rd
//
// The 8 bits at 20:13 are not a value. They are the fields of one number in
// the manual's VFPExpandImm: a sign, three bits that select an exponent from
// a window of eight around 1, and four fraction bits. So 1.5 and -0.125 are
// one instruction and 0.1 is not, and neither zero, infinity nor any NaN is,
// because every one of those needs an exponent outside the window.
//
// The consequence binds the rules. A floating-point constant that this cannot
// reach is materialised in an integer register and moved across with FMOV, so
// arm64 needs no constant pool for a float any more than it does for an
// integer.

const opFpImm = 0x1e201000

// The exponent windows VFPExpandImm reaches, as biased exponent fields. Bit 6
// of the immediate chooses the half and bits 5:4 the entry within it, so the
// window is eight wide and centred on the exponent of 1.0.
const (
	fpImmMinExp64 = 1020 // 1023 - 3
	fpImmMaxExp64 = 1027 // 1023 + 4
)

// FloatImm8 returns the 8-bit immediate that FMOV expands to val, and false
// when no immediate expands to it.
//
// There is no width parameter. The 256 immediates name the same 256 real
// numbers at single and at double precision, and every one of them is exactly
// representable in both, so the answer does not depend on the width the
// instruction will carry.
func FloatImm8(val float64) (uint32, bool) {
	bits := math.Float64bits(val)
	sign := uint32(bits >> 63)
	exp := uint32(bits>>52) & 0x7ff
	frac := bits & (1<<52 - 1)

	// The fraction has four bits and they are the top four. Anything below
	// them is a value the immediate cannot name.
	if frac&(1<<48-1) != 0 {
		return 0, false
	}
	if exp < fpImmMinExp64 || exp > fpImmMaxExp64 {
		return 0, false
	}
	// Bit 6 of the immediate is the complement of the exponent's top bit, and
	// the manual replicates it across the middle of the exponent. The low two
	// bits of the exponent survive as bits 5:4.
	b := uint32(0)
	if exp <= 1023 {
		b = 1
	}
	return sign<<7 | b<<6 | (exp&3)<<4 | uint32(frac>>48), true
}

// FmovImm encodes FMOV dst, #val. It reports false when val is not one of the
// 256 values the immediate reaches.
func FmovImm(sz Size, dst Reg, val float64) (uint32, bool) {
	imm8, ok := FloatImm8(val)
	if !ok {
		return 0, false
	}
	return opFpImm | sz.ftype() | imm8<<13 | regF(dst), true
}

// ---------------------------------------------------------------------------
// The bit layout of a floating-point value
//
// The compiler carries a floating-point constant as its bit pattern and not
// as a Go float64, because the pattern is what the instruction stream holds
// and because two NaNs with different payloads are one float64 comparison and
// two different constants. These two functions are the only place the layout
// is written, so nothing above has to import math to hold a float.

// FloatBits returns the IEEE 754 pattern of val at the width sz names. A
// Size32 result holds the 32-bit pattern in its low half.
func FloatBits(sz Size, val float64) uint64 {
	if sz == Size32 {
		return uint64(math.Float32bits(float32(val)))
	}
	return math.Float64bits(val)
}

// FloatFromBits returns the value the pattern bits names at the width sz.
func FloatFromBits(sz Size, bits uint64) float64 {
	if sz == Size32 {
		return float64(math.Float32frombits(uint32(bits)))
	}
	return math.Float64frombits(bits)
}
