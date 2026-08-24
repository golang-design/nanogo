// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package arm64

import (
	"math/bits"
	"strconv"
)

// Immediate ranges.
//
// Each range is here once and referenced from the encoder and from the
// lowering rule that has to respect it. They differ per instruction class in
// ways that are easy to assume away, which is why specs/041 names them as the
// most common source of encoder bugs.
const (
	// MaxAddSubImm is the largest unshifted add or subtract immediate. The
	// field is imm12, bits 21:10.
	MaxAddSubImm = 1<<12 - 1
	// MaxAddSubImmShifted is the largest add or subtract immediate that the
	// optional LSL #12 reaches. Bit 22 selects the shift, so the only two
	// choices are no shift and a shift of 12.
	MaxAddSubImmShifted = MaxAddSubImm << 12

	// MinMemOffsetUnscaled and MaxMemOffsetUnscaled bound the LDUR and STUR
	// form. The field is a signed imm9 at bits 20:12 and it is not scaled.
	MinMemOffsetUnscaled = -1 << 8
	MaxMemOffsetUnscaled = 1<<8 - 1

	// MaxMemOffsetScaled is the largest offset of the unsigned-offset form,
	// counted in units of the access size. The field is imm12 at bits 21:10
	// and the hardware multiplies it by the access size, so the byte range
	// depends on the width of the load or store.
	MaxMemOffsetScaled = 1<<12 - 1

	// MinBranchOffset and MaxBranchOffset bound B and BL. The field is a
	// signed imm26 at bits 25:0, scaled by 4.
	MinBranchOffset = -1 << 27
	MaxBranchOffset = 1<<27 - 4

	// MinCondBranchOffset and MaxCondBranchOffset bound B.cond, CBZ, and
	// CBNZ. The field is a signed imm19 at bits 23:5, scaled by 4.
	MinCondBranchOffset = -1 << 20
	MaxCondBranchOffset = 1<<20 - 4

	// MinTestBranchOffset and MaxTestBranchOffset bound TBZ and TBNZ. The
	// field is a signed imm14 at bits 18:5, scaled by 4. It is the shortest
	// branch range on the target because the instruction spends five more
	// bits on the bit number to test.
	MinTestBranchOffset = -1 << 15
	MaxTestBranchOffset = 1<<15 - 4

	// MinAdrpDelta and MaxAdrpDelta bound the page distance ADRP reaches.
	// The field is a signed imm21 split into immhi at bits 23:5 and immlo at
	// bits 30:29, scaled by the 4096-byte page.
	MinAdrpDelta = -1 << 32
	MaxAdrpDelta = 1<<32 - 4096

	// MaxMovConst is the largest number of instructions MovConst emits. Four
	// MOVZ or MOVK instructions cover the four halfwords of a 64-bit value.
	MaxMovConst = 4
)

// regZ encodes r where register 31 reads as the zero register.
func regZ(r Reg) uint32 {
	switch {
	case r <= R30:
		return uint32(r)
	case r == ZR:
		return 31
	case r == RSP:
		panic("arm64: RSP used where the encoding means the zero register")
	}
	panic("arm64: invalid register " + r.String())
}

// regSP encodes r where register 31 is the stack pointer.
func regSP(r Reg) uint32 {
	switch {
	case r <= R30:
		return uint32(r)
	case r == RSP:
		return 31
	case r == ZR:
		panic("arm64: ZR used where the encoding means the stack pointer")
	}
	panic("arm64: invalid register " + r.String())
}

// Nop encodes the HINT #0 instruction. It is here because instruction
// sequences need padding, not because any rule emits it.
func Nop() uint32 { return 0xd503201f }

// ---------------------------------------------------------------------------
// Add and subtract, immediate
//
// ARM class "Add/subtract (immediate)". Layout:
//
//	sf op S 1 0 0 0 1 0 sh imm12 Rn Rd
//
// Register 31 is the stack pointer in both Rd and Rn for ADD and SUB, and in
// Rn only for ADDS and SUBS. That is why these forms take regSP for the
// sources and why CMP against ZR is spelled with the flag-setting form.

const (
	opAddImm  = 0x11000000
	opSubImm  = 0x51000000
	opAddsImm = 0x31000000
	opSubsImm = 0x71000000
)

// addSubImm splits imm across the imm12 field and the optional LSL #12.
func addSubImm(base uint32, sz Size, rd, rn uint32, imm int64) (uint32, bool) {
	var sh uint32
	switch {
	case imm < 0:
		// A negative value is the other instruction's job. The rules decide
		// between ADD and SUB, and folding the choice in here would hide a
		// rule that produced a value it did not expect.
		return 0, false
	case imm <= MaxAddSubImm:
	case imm&0xfff == 0 && imm>>12 <= MaxAddSubImm:
		sh = 1
		imm >>= 12
	default:
		return 0, false
	}
	return base | sz.sf() | sh<<22 | uint32(imm)<<10 | rn<<5 | rd, true
}

// AddRegImm encodes ADD dst, a, #imm.
func AddRegImm(sz Size, dst, a Reg, imm int64) (uint32, bool) {
	return addSubImm(opAddImm, sz, regSP(dst), regSP(a), imm)
}

// SubRegImm encodes SUB dst, a, #imm.
func SubRegImm(sz Size, dst, a Reg, imm int64) (uint32, bool) {
	return addSubImm(opSubImm, sz, regSP(dst), regSP(a), imm)
}

// AddsRegImm encodes ADDS dst, a, #imm, which sets the flags.
func AddsRegImm(sz Size, dst, a Reg, imm int64) (uint32, bool) {
	return addSubImm(opAddsImm, sz, regZ(dst), regSP(a), imm)
}

// SubsRegImm encodes SUBS dst, a, #imm, which sets the flags.
func SubsRegImm(sz Size, dst, a Reg, imm int64) (uint32, bool) {
	return addSubImm(opSubsImm, sz, regZ(dst), regSP(a), imm)
}

// CmpRegImm encodes CMP a, #imm. It is SUBS with the zero register as the
// destination, which is how the architecture spells a compare.
func CmpRegImm(sz Size, a Reg, imm int64) (uint32, bool) {
	return addSubImm(opSubsImm, sz, 31, regSP(a), imm)
}

// CmnRegImm encodes CMN a, #imm, the compare that adds.
func CmnRegImm(sz Size, a Reg, imm int64) (uint32, bool) {
	return addSubImm(opAddsImm, sz, 31, regSP(a), imm)
}

// MovSP encodes MOV dst, src where one of the two is the stack pointer. It is
// ADD with a zero immediate, because the register move that the ORR form
// encodes cannot name the stack pointer.
func MovSP(sz Size, dst, src Reg) uint32 {
	v, ok := addSubImm(opAddImm, sz, regSP(dst), regSP(src), 0)
	if !ok {
		panic("arm64: unreachable, zero is always an add immediate")
	}
	return v
}

// ---------------------------------------------------------------------------
// Add and subtract, shifted register
//
// ARM class "Add/subtract (shifted register)". Layout:
//
//	sf op S 0 1 0 1 1 shift 0 Rm imm6 Rn Rd
//
// The shift is LSL, LSR, or ASR. ROR is not encodable here even though the
// field is two bits wide, which is the kind of gap that a table-driven encoder
// hides. Register 31 is the zero register throughout.

const (
	opAddShift  = 0x0b000000
	opSubShift  = 0x4b000000
	opAddsShift = 0x2b000000
	opSubsShift = 0x6b000000
)

func addSubShift(base uint32, sz Size, rd, rn, rm uint32, sh Shift, amount uint32) (uint32, bool) {
	if sh == ROR || sh >= numShift {
		return 0, false
	}
	if amount >= sz.bits() {
		return 0, false
	}
	return base | sz.sf() | uint32(sh)<<22 | rm<<16 | amount<<10 | rn<<5 | rd, true
}

// AddRegReg encodes ADD dst, a, b.
func AddRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(addSubShift(opAddShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// AddRegRegShift encodes ADD dst, a, b<<amount for the shift kind sh. It
// reports false for ROR, which the class cannot encode, and for a shift that
// is at least the operand width.
func AddRegRegShift(sz Size, dst, a, b Reg, sh Shift, amount uint32) (uint32, bool) {
	return addSubShift(opAddShift, sz, regZ(dst), regZ(a), regZ(b), sh, amount)
}

// SubRegReg encodes SUB dst, a, b.
func SubRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(addSubShift(opSubShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// SubRegRegShift encodes SUB dst, a, b shifted by amount.
func SubRegRegShift(sz Size, dst, a, b Reg, sh Shift, amount uint32) (uint32, bool) {
	return addSubShift(opSubShift, sz, regZ(dst), regZ(a), regZ(b), sh, amount)
}

// AddsRegReg encodes ADDS dst, a, b.
func AddsRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(addSubShift(opAddsShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// SubsRegReg encodes SUBS dst, a, b.
func SubsRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(addSubShift(opSubsShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// NegReg encodes NEG dst, src. It is SUB from the zero register.
func NegReg(sz Size, dst, src Reg) uint32 {
	return mustShift(addSubShift(opSubShift, sz, regZ(dst), 31, regZ(src), LSL, 0))
}

// CmpRegReg encodes CMP a, b, which is SUBS into the zero register.
func CmpRegReg(sz Size, a, b Reg) uint32 {
	return mustShift(addSubShift(opSubsShift, sz, 31, regZ(a), regZ(b), LSL, 0))
}

// CmnRegReg encodes CMN a, b, which is ADDS into the zero register.
func CmnRegReg(sz Size, a, b Reg) uint32 {
	return mustShift(addSubShift(opAddsShift, sz, 31, regZ(a), regZ(b), LSL, 0))
}

// mustShift unwraps a shifted-register encoding that cannot fail because the
// caller passed LSL #0.
func mustShift(v uint32, ok bool) uint32 {
	if !ok {
		panic("arm64: unreachable, LSL #0 is always encodable")
	}
	return v
}

// ---------------------------------------------------------------------------
// Logical, shifted register
//
// ARM class "Logical (shifted register)". Layout:
//
//	sf opc 0 1 0 1 0 shift N Rm imm6 Rn Rd
//
// N inverts the second source, which is what turns AND into BIC and ORR into
// ORN. Unlike the arithmetic class, ROR is encodable here.

const (
	opAndShift  = 0x0a000000
	opBicShift  = 0x0a200000
	opOrrShift  = 0x2a000000
	opOrnShift  = 0x2a200000
	opEorShift  = 0x4a000000
	opEonShift  = 0x4a200000
	opAndsShift = 0x6a000000
	opBicsShift = 0x6a200000
)

func logicalShift(base uint32, sz Size, rd, rn, rm uint32, sh Shift, amount uint32) (uint32, bool) {
	if sh >= numShift || amount >= sz.bits() {
		return 0, false
	}
	return base | sz.sf() | uint32(sh)<<22 | rm<<16 | amount<<10 | rn<<5 | rd, true
}

// AndRegReg encodes AND dst, a, b.
func AndRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(logicalShift(opAndShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// AndRegRegShift encodes AND dst, a, b shifted by amount.
func AndRegRegShift(sz Size, dst, a, b Reg, sh Shift, amount uint32) (uint32, bool) {
	return logicalShift(opAndShift, sz, regZ(dst), regZ(a), regZ(b), sh, amount)
}

// OrrRegReg encodes ORR dst, a, b.
func OrrRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(logicalShift(opOrrShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// OrrRegRegShift encodes ORR dst, a, b shifted by amount.
func OrrRegRegShift(sz Size, dst, a, b Reg, sh Shift, amount uint32) (uint32, bool) {
	return logicalShift(opOrrShift, sz, regZ(dst), regZ(a), regZ(b), sh, amount)
}

// EorRegReg encodes EOR dst, a, b.
func EorRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(logicalShift(opEorShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// EorRegRegShift encodes EOR dst, a, b shifted by amount.
func EorRegRegShift(sz Size, dst, a, b Reg, sh Shift, amount uint32) (uint32, bool) {
	return logicalShift(opEorShift, sz, regZ(dst), regZ(a), regZ(b), sh, amount)
}

// BicRegReg encodes BIC dst, a, b, which is a AND NOT b.
func BicRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(logicalShift(opBicShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// BicRegRegShift encodes BIC dst, a, b shifted by amount.
func BicRegRegShift(sz Size, dst, a, b Reg, sh Shift, amount uint32) (uint32, bool) {
	return logicalShift(opBicShift, sz, regZ(dst), regZ(a), regZ(b), sh, amount)
}

// OrnRegReg encodes ORN dst, a, b, which is a OR NOT b.
func OrnRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(logicalShift(opOrnShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// EonRegReg encodes EON dst, a, b, which is a EOR NOT b.
func EonRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(logicalShift(opEonShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// AndsRegReg encodes ANDS dst, a, b, which sets the flags.
func AndsRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(logicalShift(opAndsShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// BicsRegReg encodes BICS dst, a, b, which sets the flags.
func BicsRegReg(sz Size, dst, a, b Reg) uint32 {
	return mustShift(logicalShift(opBicsShift, sz, regZ(dst), regZ(a), regZ(b), LSL, 0))
}

// TstRegReg encodes TST a, b, which is ANDS into the zero register.
func TstRegReg(sz Size, a, b Reg) uint32 {
	return mustShift(logicalShift(opAndsShift, sz, 31, regZ(a), regZ(b), LSL, 0))
}

// MvnReg encodes MVN dst, src, which is ORN from the zero register.
func MvnReg(sz Size, dst, src Reg) uint32 {
	return mustShift(logicalShift(opOrnShift, sz, regZ(dst), 31, regZ(src), LSL, 0))
}

// MovRegReg encodes MOV dst, src for two general registers. It is ORR from the
// zero register. Neither operand may be the stack pointer: use MovSP for that.
func MovRegReg(sz Size, dst, src Reg) uint32 {
	return mustShift(logicalShift(opOrrShift, sz, regZ(dst), 31, regZ(src), LSL, 0))
}

// ---------------------------------------------------------------------------
// Logical immediates: the N, immr, imms bitmask encoding
//
// ARM class "Logical (immediate)". Layout:
//
//	sf opc 1 0 0 1 0 0 N immr imms Rn Rd
//
// The 13 bits of N, immr and imms do not hold a value. They hold a repeating
// pattern: an element of 2, 4, 8, 16, 32 or 64 bits, holding a run of between
// one and element-size-minus-one ones, rotated right by immr, and replicated
// to fill the register. So 0x5555555555555555 is encodable and 3 is encodable
// but 5 is not, and neither zero nor all ones is, because a run of ones cannot
// be empty or fill the element.
//
// This is the most intricate encoding on the target. A wrong implementation
// does not fail loudly, it produces a different mask, so the test enumerates
// every triple the scheme can produce and requires the encoder to reproduce it
// exactly.

// isShiftedMask reports whether x is a single contiguous run of ones that does
// not wrap around bit 63.
func isShiftedMask(x uint64) bool { return x != 0 && (x+(x&-x))&x == 0 }

// LogicalImm encodes val as the N, immr and imms fields of a logical
// immediate. It reports false when the bitmask scheme cannot represent val.
//
// For Size32 the value must fit in 32 bits. The 32-bit forms require N to be
// zero, which the replication below guarantees: an element size of 64 is the
// only one that sets N, and a value replicated into both halves can never
// need one.
func LogicalImm(sz Size, val uint64) (n, immr, imms uint32, ok bool) {
	if sz == Size32 {
		if val>>32 != 0 {
			return 0, 0, 0, false
		}
		// Replicate into the high half so that one 64-bit search finds the
		// element size. A 32-bit operand is a 64-bit pattern whose element is
		// at most 32 bits wide.
		val |= val << 32
	}
	if val == 0 || val == ^uint64(0) {
		return 0, 0, 0, false
	}

	// The element size is the smallest width whose repetition fills the
	// register. Halve while the two halves agree.
	size := uint32(64)
	for {
		size >>= 1
		mask := uint64(1)<<size - 1
		if val&mask != (val>>size)&mask {
			size <<= 1
			break
		}
		if size <= 2 {
			break
		}
	}

	mask := ^uint64(0) >> (64 - size)
	elem := val & mask

	// rot is how far right the run of ones sits, ones is its length.
	var rot, ones uint32
	if isShiftedMask(elem) {
		rot = uint32(bits.TrailingZeros64(elem))
		ones = uint32(bits.TrailingZeros64(^(elem >> rot)))
	} else {
		// The run wraps around the top of the element. Fill the bits above
		// the element so that the complement is a single run, then measure it.
		elem |= ^mask
		if !isShiftedMask(^elem) {
			return 0, 0, 0, false
		}
		leadingOnes := uint32(bits.LeadingZeros64(^elem))
		rot = 64 - leadingOnes
		ones = leadingOnes + uint32(bits.TrailingZeros64(^elem)) - (64 - size)
	}

	// immr counts the rotations that take the canonical run at the bottom of
	// the element to where it actually sits, which is the opposite direction
	// from rot.
	immr = (size - rot) % size

	// imms holds the run length minus one in its low bits, under a prefix of
	// ones that names the element size: 0SSSSS for 32, 10SSSS for 16, down to
	// 11110S for 2. Element size 64 overflows the six bits, and that overflow
	// bit is exactly what N carries.
	nimms := ^(size - 1) << 1
	nimms |= ones - 1
	n = ^(nimms >> 6) & 1
	imms = nimms & 0x3f
	return n, immr, imms, true
}

const (
	opAndImm  = 0x12000000
	opOrrImm  = 0x32000000
	opEorImm  = 0x52000000
	opAndsImm = 0x72000000
)

func logicalImm(base uint32, sz Size, rd, rn uint32, val uint64) (uint32, bool) {
	n, immr, imms, ok := LogicalImm(sz, val)
	if !ok {
		return 0, false
	}
	return base | sz.sf() | n<<22 | immr<<16 | imms<<10 | rn<<5 | rd, true
}

// AndRegImm encodes AND dst, a, #val.
func AndRegImm(sz Size, dst, a Reg, val uint64) (uint32, bool) {
	return logicalImm(opAndImm, sz, regSP(dst), regZ(a), val)
}

// OrrRegImm encodes ORR dst, a, #val.
func OrrRegImm(sz Size, dst, a Reg, val uint64) (uint32, bool) {
	return logicalImm(opOrrImm, sz, regSP(dst), regZ(a), val)
}

// EorRegImm encodes EOR dst, a, #val.
func EorRegImm(sz Size, dst, a Reg, val uint64) (uint32, bool) {
	return logicalImm(opEorImm, sz, regSP(dst), regZ(a), val)
}

// BicRegImm encodes BIC dst, a, #val.
//
// The architecture has no BIC with an immediate. It is AND of the complement,
// and the assembler spells it the same way, so a caller that checks the range
// of val must check the range of its complement instead.
func BicRegImm(sz Size, dst, a Reg, val uint64) (uint32, bool) {
	inv := ^val
	if sz == Size32 {
		inv &= 0xffffffff
	}
	return logicalImm(opAndImm, sz, regSP(dst), regZ(a), inv)
}

// AndsRegImm encodes ANDS dst, a, #val, which sets the flags. The destination
// is zero-register encoded here and stack-pointer encoded in AND, which is the
// one place the two forms differ.
func AndsRegImm(sz Size, dst, a Reg, val uint64) (uint32, bool) {
	return logicalImm(opAndsImm, sz, regZ(dst), regZ(a), val)
}

// TstRegImm encodes TST a, #val, which is ANDS into the zero register.
func TstRegImm(sz Size, a Reg, val uint64) (uint32, bool) {
	return logicalImm(opAndsImm, sz, 31, regZ(a), val)
}

// MovLogicalImm encodes MOV dst, #val through the bitmask scheme, which is
// ORR from the zero register. It reaches values MovConst needs several
// instructions for, so lowering should try it first.
func MovLogicalImm(sz Size, dst Reg, val uint64) (uint32, bool) {
	return logicalImm(opOrrImm, sz, regSP(dst), 31, val)
}

// ---------------------------------------------------------------------------
// Move wide immediate
//
// ARM class "Move wide (immediate)". Layout:
//
//	sf opc 1 0 0 1 0 1 hw imm16 Rd
//
// hw at bits 22:21 selects which 16-bit halfword the immediate lands in. A
// 32-bit operand has two halfwords, so hw is limited to 0 and 1 there.

const (
	opMovn = 0x12800000
	opMovz = 0x52800000
	opMovk = 0x72800000
)

func movWide(base uint32, sz Size, rd uint32, imm16 uint16, shift uint32) (uint32, bool) {
	if shift%16 != 0 || shift >= sz.bits() {
		return 0, false
	}
	return base | sz.sf() | (shift/16)<<21 | uint32(imm16)<<5 | rd, true
}

// Movz encodes MOVZ dst, #imm16, LSL #shift. It writes the halfword and zeroes
// the rest of the register.
func Movz(sz Size, dst Reg, imm16 uint16, shift uint32) (uint32, bool) {
	return movWide(opMovz, sz, regZ(dst), imm16, shift)
}

// Movn encodes MOVN dst, #imm16, LSL #shift. It writes the complement of the
// shifted halfword, so the rest of the register becomes ones.
func Movn(sz Size, dst Reg, imm16 uint16, shift uint32) (uint32, bool) {
	return movWide(opMovn, sz, regZ(dst), imm16, shift)
}

// Movk encodes MOVK dst, #imm16, LSL #shift. It replaces one halfword and
// leaves the others, which is what makes a multi-instruction constant work.
func Movk(sz Size, dst Reg, imm16 uint16, shift uint32) (uint32, bool) {
	return movWide(opMovk, sz, regZ(dst), imm16, shift)
}

// MovConst writes into out the shortest MOVZ, MOVN and MOVK sequence that
// leaves val in dst, and returns how many instructions it wrote.
//
// out must hold at least MaxMovConst instructions. The result is one
// instruction per halfword that has to be written: the encoder starts from
// MOVZ when most halfwords are zero and from MOVN when most are all-ones, so
// a small positive number and a small negative number both cost one
// instruction.
//
// The sequence never uses the bitmask forms. Lowering should try
// MovLogicalImm first, because a value such as 0x0000ffff0000ffff is one ORR
// and two MOVK.
func MovConst(sz Size, dst Reg, val int64, out []uint32) int {
	if len(out) < MaxMovConst {
		panic("arm64: MovConst needs room for MaxMovConst instructions")
	}
	rd := regZ(dst)
	halves := uint32(4)
	u := uint64(val)
	if sz == Size32 {
		halves = 2
		u &= 0xffffffff
	}

	// Count the halfwords each starting point would have to write.
	var zeros, ones uint32
	for i := uint32(0); i < halves; i++ {
		h := uint16(u >> (16 * i))
		if h != 0 {
			zeros++
		}
		if h != 0xffff {
			ones++
		}
	}

	n := 0
	if zeros <= ones {
		for i := uint32(0); i < halves; i++ {
			h := uint16(u >> (16 * i))
			if h == 0 {
				continue
			}
			if n == 0 {
				out[n] = mustWide(movWide(opMovz, sz, rd, h, 16*i))
			} else {
				out[n] = mustWide(movWide(opMovk, sz, rd, h, 16*i))
			}
			n++
		}
		if n == 0 {
			// Every halfword is zero, so nothing was written above.
			out[0] = mustWide(movWide(opMovz, sz, rd, 0, 0))
			n = 1
		}
		return n
	}
	for i := uint32(0); i < halves; i++ {
		h := uint16(u >> (16 * i))
		if h == 0xffff {
			continue
		}
		if n == 0 {
			out[n] = mustWide(movWide(opMovn, sz, rd, ^h, 16*i))
		} else {
			out[n] = mustWide(movWide(opMovk, sz, rd, h, 16*i))
		}
		n++
	}
	if n == 0 {
		// Every halfword is all ones, so the loop above wrote nothing. MOVN of
		// zero is the whole value.
		out[0] = mustWide(movWide(opMovn, sz, rd, 0, 0))
		n = 1
	}
	return n
}

func mustWide(v uint32, ok bool) uint32 {
	if !ok {
		panic("arm64: unreachable, MovConst shifts a halfword it counted")
	}
	return v
}

// ---------------------------------------------------------------------------
// Shifts and extensions by an immediate
//
// ARM class "Bitfield". Layout:
//
//	sf opc 1 0 0 1 1 0 N immr imms Rn Rd
//
// The architecture has no shift-by-immediate instruction. LSL, LSR and ASR by
// an immediate are aliases of UBFM and SBFM with immr and imms derived from
// the shift amount, and the sign extensions are the same class with a fixed
// pair of fields. N follows sf: a 64-bit bitfield sets it, a 32-bit one does
// not.

const (
	opSbfm = 0x13000000
	opUbfm = 0x53000000
	opExtr = 0x13800000
)

func bfm(base uint32, sz Size, rd, rn, immr, imms uint32) uint32 {
	var n uint32
	if sz == Size64 {
		n = 1
	}
	return base | sz.sf() | n<<22 | immr<<16 | imms<<10 | rn<<5 | rd
}

// LslRegImm encodes LSL dst, a, #shift, which is UBFM. The shift must be less
// than the operand width, because a shift equal to the width has no encoding
// rather than producing zero.
func LslRegImm(sz Size, dst, a Reg, shift uint32) (uint32, bool) {
	w := sz.bits()
	if shift >= w {
		return 0, false
	}
	return bfm(opUbfm, sz, regZ(dst), regZ(a), (w-shift)%w, w-1-shift), true
}

// LsrRegImm encodes LSR dst, a, #shift, which is UBFM to the top of the
// register.
func LsrRegImm(sz Size, dst, a Reg, shift uint32) (uint32, bool) {
	w := sz.bits()
	if shift >= w {
		return 0, false
	}
	return bfm(opUbfm, sz, regZ(dst), regZ(a), shift, w-1), true
}

// AsrRegImm encodes ASR dst, a, #shift, which is SBFM to the top of the
// register.
func AsrRegImm(sz Size, dst, a Reg, shift uint32) (uint32, bool) {
	w := sz.bits()
	if shift >= w {
		return 0, false
	}
	return bfm(opSbfm, sz, regZ(dst), regZ(a), shift, w-1), true
}

// RorRegImm encodes ROR dst, a, #shift. It is EXTR with both source registers
// set to a, which is a different class from the other three shifts.
func RorRegImm(sz Size, dst, a Reg, shift uint32) (uint32, bool) {
	if shift >= sz.bits() {
		return 0, false
	}
	var n uint32
	if sz == Size64 {
		n = 1
	}
	rn := regZ(a)
	return opExtr | sz.sf() | n<<22 | rn<<16 | shift<<10 | rn<<5 | regZ(dst), true
}

// SxtbReg encodes SXTB dst, src, the sign extension of the low 8 bits.
func SxtbReg(sz Size, dst, src Reg) uint32 {
	return bfm(opSbfm, sz, regZ(dst), regZ(src), 0, 7)
}

// SxthReg encodes SXTH dst, src, the sign extension of the low 16 bits.
func SxthReg(sz Size, dst, src Reg) uint32 {
	return bfm(opSbfm, sz, regZ(dst), regZ(src), 0, 15)
}

// SxtwReg encodes SXTW dst, src, the sign extension of the low 32 bits. It
// exists only at 64 bits, which is why it takes no Size.
func SxtwReg(dst, src Reg) uint32 {
	return bfm(opSbfm, Size64, regZ(dst), regZ(src), 0, 31)
}

// UxtbReg encodes UXTB dst, src, the zero extension of the low 8 bits.
func UxtbReg(sz Size, dst, src Reg) uint32 {
	return bfm(opUbfm, sz, regZ(dst), regZ(src), 0, 7)
}

// UxthReg encodes UXTH dst, src, the zero extension of the low 16 bits.
func UxthReg(sz Size, dst, src Reg) uint32 {
	return bfm(opUbfm, sz, regZ(dst), regZ(src), 0, 15)
}

// ---------------------------------------------------------------------------
// Two-source and three-source data processing
//
// ARM classes "Data-processing (2 source)" and "Data-processing (3 source)".
//
//	sf 0 0 1 1 0 1 0 1 1 0 Rm opcode Rn Rd
//	sf 0 0 1 1 0 1 1 op31 Rm o0 Ra Rn Rd

const (
	opDp2   = 0x1ac00000
	opcLslv = 8
	opcLsrv = 9
	opcAsrv = 10
	opcRorv = 11
	opcSdiv = 3
	opcUdiv = 2

	opDp3   = 0x1b000000
	bitMsub = 1 << 15
	raMul   = 31 // the addend of a plain multiply is the zero register
)

func dp2(sz Size, opc, rd, rn, rm uint32) uint32 {
	return opDp2 | sz.sf() | rm<<16 | opc<<10 | rn<<5 | rd
}

// LslRegReg encodes LSL dst, a, b, the shift by a register. The hardware takes
// the shift amount modulo the operand width, so there is no range to check.
func LslRegReg(sz Size, dst, a, b Reg) uint32 {
	return dp2(sz, opcLslv, regZ(dst), regZ(a), regZ(b))
}

// LsrRegReg encodes LSR dst, a, b.
func LsrRegReg(sz Size, dst, a, b Reg) uint32 {
	return dp2(sz, opcLsrv, regZ(dst), regZ(a), regZ(b))
}

// AsrRegReg encodes ASR dst, a, b.
func AsrRegReg(sz Size, dst, a, b Reg) uint32 {
	return dp2(sz, opcAsrv, regZ(dst), regZ(a), regZ(b))
}

// RorRegReg encodes ROR dst, a, b.
func RorRegReg(sz Size, dst, a, b Reg) uint32 {
	return dp2(sz, opcRorv, regZ(dst), regZ(a), regZ(b))
}

// SdivRegReg encodes SDIV dst, a, b. Division by zero produces zero rather
// than a trap, so the rules have to emit the panic check themselves.
func SdivRegReg(sz Size, dst, a, b Reg) uint32 {
	return dp2(sz, opcSdiv, regZ(dst), regZ(a), regZ(b))
}

// UdivRegReg encodes UDIV dst, a, b.
func UdivRegReg(sz Size, dst, a, b Reg) uint32 {
	return dp2(sz, opcUdiv, regZ(dst), regZ(a), regZ(b))
}

func dp3(sz Size, extra, rd, rn, rm, ra uint32) uint32 {
	return opDp3 | sz.sf() | extra | rm<<16 | ra<<10 | rn<<5 | rd
}

// Madd encodes MADD dst, a, b, addend, which computes addend + a*b.
func Madd(sz Size, dst, a, b, addend Reg) uint32 {
	return dp3(sz, 0, regZ(dst), regZ(a), regZ(b), regZ(addend))
}

// Msub encodes MSUB dst, a, b, minuend, which computes minuend - a*b.
func Msub(sz Size, dst, a, b, minuend Reg) uint32 {
	return dp3(sz, bitMsub, regZ(dst), regZ(a), regZ(b), regZ(minuend))
}

// MulRegReg encodes MUL dst, a, b. It is MADD with the zero register as the
// addend.
func MulRegReg(sz Size, dst, a, b Reg) uint32 {
	return dp3(sz, 0, regZ(dst), regZ(a), regZ(b), raMul)
}

// MnegRegReg encodes MNEG dst, a, b, which is MSUB from the zero register.
func MnegRegReg(sz Size, dst, a, b Reg) uint32 {
	return dp3(sz, bitMsub, regZ(dst), regZ(a), regZ(b), raMul)
}

// ---------------------------------------------------------------------------
// Loads and stores
//
// ARM classes "Load/store register (unsigned immediate)", "(unscaled
// immediate)", "(immediate pre-indexed)", "(immediate post-indexed)" and
// "(register offset)". They share a layout:
//
//	size 1 1 1 0 0 x opc ... Rn Rt
//
// size at bits 31:30 gives the access width and opc at bits 23:22 says whether
// the access stores, loads, or loads and sign-extends, and to which width.
// The base register is stack-pointer encoded, because loading from the stack
// is the common case. The transfer register is zero-register encoded, so a
// store of ZR is how a zero is written to memory.

// MemOp names a load or store width and signedness.
type MemOp uint8

// The load and store forms. The signed loads name the width they extend to,
// because LDRSB into a W register and LDRSB into an X register are different
// encodings of the same access.
const (
	StoreB   MemOp = iota // store the low 8 bits
	StoreH                // store the low 16 bits
	StoreW                // store the low 32 bits
	StoreX                // store 64 bits
	LoadBU                // load 8 bits, zero-extend
	LoadHU                // load 16 bits, zero-extend
	LoadWU                // load 32 bits, zero-extend
	LoadX                 // load 64 bits
	LoadBS32              // load 8 bits, sign-extend to 32
	LoadBS64              // load 8 bits, sign-extend to 64
	LoadHS32              // load 16 bits, sign-extend to 32
	LoadHS64              // load 16 bits, sign-extend to 64
	LoadWS64              // load 32 bits, sign-extend to 64

	numMemOp
)

// memOpEnc is indexed by MemOp. An array, not a map: nothing that produces
// output may range over a map (specs/053-determinism.md).
var memOpEnc = [numMemOp]struct {
	size  uint32 // bits 31:30
	opc   uint32 // bits 23:22
	scale int64  // the unsigned-offset form multiplies by this
}{
	StoreB:   {0, 0, 1},
	StoreH:   {1, 0, 2},
	StoreW:   {2, 0, 4},
	StoreX:   {3, 0, 8},
	LoadBU:   {0, 1, 1},
	LoadHU:   {1, 1, 2},
	LoadWU:   {2, 1, 4},
	LoadX:    {3, 1, 8},
	LoadBS32: {0, 3, 1},
	LoadBS64: {0, 2, 1},
	LoadHS32: {1, 3, 2},
	LoadHS64: {1, 2, 2},
	LoadWS64: {2, 2, 4},
}

var memOpNames = [numMemOp]string{
	StoreB: "StoreB", StoreH: "StoreH", StoreW: "StoreW", StoreX: "StoreX",
	LoadBU: "LoadBU", LoadHU: "LoadHU", LoadWU: "LoadWU", LoadX: "LoadX",
	LoadBS32: "LoadBS32", LoadBS64: "LoadBS64", LoadHS32: "LoadHS32",
	LoadHS64: "LoadHS64", LoadWS64: "LoadWS64",
}

func (m MemOp) String() string {
	if m >= numMemOp {
		return "MemOp(" + strconv.Itoa(int(m)) + ")"
	}
	return memOpNames[m]
}

// Scale returns the access size in bytes. The unsigned-offset form can only
// reach offsets that are a multiple of it.
func (m MemOp) Scale() int64 {
	if m >= numMemOp {
		panic("arm64: invalid MemOp")
	}
	return memOpEnc[m].scale
}

func (m MemOp) enc() (size, opc uint32, scale int64) {
	if m >= numMemOp {
		panic("arm64: invalid MemOp " + strconv.Itoa(int(m)))
	}
	e := memOpEnc[m]
	return e.size, e.opc, e.scale
}

const (
	memUnsignedBase = 0x39000000 // bits 25:24 are 01
	memUnscaledBase = 0x38000000 // bits 25:24 are 00
	memPostIndex    = 1 << 10    // bits 11:10 are 01
	memPreIndex     = 3 << 10    // bits 11:10 are 11
	memRegOffset    = 1 << 21    // bit 21 selects the register-offset form
)

// MemUnsignedOffset encodes a load or store with a 12-bit offset that the
// hardware scales by the access size. The offset must be a non-negative
// multiple of that size, so this form reaches 32760 bytes for a 64-bit access
// and 4095 for a byte access.
func MemUnsignedOffset(op MemOp, rt, base Reg, off int64) (uint32, bool) {
	size, opc, scale := op.enc()
	if off < 0 || off%scale != 0 {
		return 0, false
	}
	imm := off / scale
	if imm > MaxMemOffsetScaled {
		return 0, false
	}
	return memUnsignedBase | size<<30 | opc<<22 | uint32(imm)<<10 |
		regSP(base)<<5 | regZ(rt), true
}

// memImm9 encodes the LDUR, STUR, pre-index and post-index forms, which share
// a signed 9-bit unscaled offset at bits 20:12.
func memImm9(op MemOp, rt, base Reg, off int64, mode uint32) (uint32, bool) {
	size, opc, _ := op.enc()
	if off < MinMemOffsetUnscaled || off > MaxMemOffsetUnscaled {
		return 0, false
	}
	return memUnscaledBase | size<<30 | opc<<22 | uint32(off&0x1ff)<<12 | mode |
		regSP(base)<<5 | regZ(rt), true
}

// MemUnscaled encodes the LDUR or STUR form: a signed 9-bit offset that is not
// scaled, so it reaches negative offsets and offsets that are not a multiple
// of the access size. The unsigned form reaches further but only forwards and
// only on a multiple, which is why both exist.
func MemUnscaled(op MemOp, rt, base Reg, off int64) (uint32, bool) {
	return memImm9(op, rt, base, off, 0)
}

// MemPreIndex encodes the pre-indexed form: the base is updated by off and the
// access uses the updated value. The prologue's stack push is this form.
func MemPreIndex(op MemOp, rt, base Reg, off int64) (uint32, bool) {
	return memImm9(op, rt, base, off, memPreIndex)
}

// MemPostIndex encodes the post-indexed form: the access uses the old base and
// the base is then updated by off. The epilogue's stack pop is this form.
func MemPostIndex(op MemOp, rt, base Reg, off int64) (uint32, bool) {
	return memImm9(op, rt, base, off, memPostIndex)
}

// MemRegOffset encodes the register-offset form. ext says how the index
// register is extended, and shifted asks for the index to be scaled by the
// access size, which is the S bit rather than a general shift amount.
//
// Only the four 32-bit and 64-bit extensions index memory. The byte and
// halfword extensions are rejected, because the field is three bits wide and
// accepting all eight would encode instructions the architecture does not
// define.
func MemRegOffset(op MemOp, rt, base, index Reg, ext Extend, shifted bool) (uint32, bool) {
	switch ext {
	case UXTW, LSLX, SXTW, SXTX:
	default:
		return 0, false
	}
	size, opc, _ := op.enc()
	var s uint32
	if shifted {
		s = 1 << 12
	}
	return memUnscaledBase | size<<30 | opc<<22 | memRegOffset |
		regZ(index)<<16 | uint32(ext)<<13 | s | 2<<10 |
		regSP(base)<<5 | regZ(rt), true
}

// ---------------------------------------------------------------------------
// Branches
//
// ARM classes "Unconditional branch (immediate)", "Conditional branch
// (immediate)", "Compare and branch (immediate)", "Test and branch
// (immediate)" and "Unconditional branch (register)".
//
// Every offset here is a byte displacement from the branch instruction itself,
// and every one is stored divided by 4 because instructions are 4-byte
// aligned. A displacement that is not a multiple of 4 is rejected rather than
// rounded.

const (
	opB     = 0x14000000
	opBl    = 0x94000000
	opBcond = 0x54000000
	opCbz   = 0x34000000
	opCbnz  = 0x35000000
	opTbz   = 0x36000000
	opTbnz  = 0x37000000
	opBr    = 0xd61f0000
	opBlr   = 0xd63f0000
	opRet   = 0xd65f0000
)

func branchImm26(base uint32, off int64) (uint32, bool) {
	if off%4 != 0 || off < MinBranchOffset || off > MaxBranchOffset {
		return 0, false
	}
	return base | uint32(off/4)&0x03ffffff, true
}

// B encodes an unconditional branch by off bytes. The 26-bit field reaches
// 128MB in each direction, and a target further away needs the linker's
// trampoline, which is why R16 and R17 are never allocated.
func B(off int64) (uint32, bool) { return branchImm26(opB, off) }

// Bl encodes a call by off bytes, which writes the return address to R30.
func Bl(off int64) (uint32, bool) { return branchImm26(opBl, off) }

func branchImm19(base uint32, off int64, low uint32) (uint32, bool) {
	if off%4 != 0 || off < MinCondBranchOffset || off > MaxCondBranchOffset {
		return 0, false
	}
	return base | (uint32(off/4)&0x7ffff)<<5 | low, true
}

// BCond encodes a conditional branch by off bytes. The 19-bit field reaches
// 1MB in each direction.
func BCond(c Cond, off int64) (uint32, bool) {
	if !c.Valid() {
		return 0, false
	}
	return branchImm19(opBcond, off, uint32(c))
}

// Cbz encodes a branch taken when r is zero, which saves the separate compare.
func Cbz(sz Size, r Reg, off int64) (uint32, bool) {
	return branchImm19(opCbz|sz.sf(), off, regZ(r))
}

// Cbnz encodes a branch taken when r is not zero.
func Cbnz(sz Size, r Reg, off int64) (uint32, bool) {
	return branchImm19(opCbnz|sz.sf(), off, regZ(r))
}

func testBranch(base uint32, r Reg, bit uint32, off int64) (uint32, bool) {
	if bit >= 64 {
		return 0, false
	}
	if off%4 != 0 || off < MinTestBranchOffset || off > MaxTestBranchOffset {
		return 0, false
	}
	// The bit number is split: its top bit is bit 31 of the instruction, which
	// is also the position sf holds elsewhere, and the low five bits sit at
	// bits 23:19. That split is why the branch offset only gets 14 bits.
	return base | (bit>>5)<<31 | (bit&31)<<19 | (uint32(off/4)&0x3fff)<<5 | regZ(r), true
}

// Tbz encodes a branch taken when bit number bit of r is zero. The 14-bit
// offset reaches 32KB in each direction, the shortest branch on the target.
func Tbz(r Reg, bit uint32, off int64) (uint32, bool) {
	return testBranch(opTbz, r, bit, off)
}

// Tbnz encodes a branch taken when bit number bit of r is one.
func Tbnz(r Reg, bit uint32, off int64) (uint32, bool) {
	return testBranch(opTbnz, r, bit, off)
}

// Br encodes an indirect branch to r.
func Br(r Reg) uint32 { return opBr | regZ(r)<<5 }

// Blr encodes an indirect call through r, which writes the return address to
// R30. Interface and closure calls are this instruction.
func Blr(r Reg) uint32 { return opBlr | regZ(r)<<5 }

// Ret encodes a return through r, which is R30 in every function nanogo emits.
func Ret(r Reg) uint32 { return opRet | regZ(r)<<5 }

// ---------------------------------------------------------------------------
// PC-relative addresses
//
// ARM class "PC-rel. addressing". Layout:
//
//	op immlo 1 0 0 0 0 immhi Rd
//
// The 21-bit immediate is split, with its low two bits at 30:29 and the rest
// at 23:5. ADRP multiplies it by the 4096-byte page and clears the low 12 bits
// of the PC before adding, so it reaches 4GB but only to a page boundary. The
// remaining 12 bits come from an ADD, which is why a symbol address is always
// two instructions.

const opAdrp = 0x90000000

func adrp(rd uint32, pageDelta int64) (uint32, bool) {
	if pageDelta%4096 != 0 || pageDelta < MinAdrpDelta || pageDelta > MaxAdrpDelta {
		return 0, false
	}
	imm := uint32(pageDelta/4096) & 0x1fffff
	return opAdrp | (imm&3)<<29 | (imm>>2)<<5 | rd, true
}

// Adrp encodes ADRP dst, #pageDelta. pageDelta is the byte distance from the
// page holding this instruction to the page holding the target, so it is
// always a multiple of 4096.
func Adrp(dst Reg, pageDelta int64) (uint32, bool) {
	return adrp(regZ(dst), pageDelta)
}

// AdrpAdd encodes the ADRP and ADD pair that puts a symbol's address in dst.
// pageDelta is the page distance the ADRP covers and pageOffset is the
// symbol's offset within its page, which the ADD supplies.
//
// The compiler emits this pair with both values zero and a relocation, and the
// linker fills them in once it knows where the symbol landed. The arguments
// exist so that the encoder can be tested and so that a linker written against
// it has one place to look.
func AdrpAdd(dst Reg, pageDelta, pageOffset int64) (hi, lo uint32, ok bool) {
	if pageOffset < 0 || pageOffset > 4095 {
		return 0, 0, false
	}
	hi, ok = adrp(regZ(dst), pageDelta)
	if !ok {
		return 0, 0, false
	}
	lo, ok = AddRegImm(Size64, dst, dst, pageOffset)
	if !ok {
		return 0, 0, false
	}
	return hi, lo, true
}

// ---------------------------------------------------------------------------
// Add and subtract, extended register
//
// ARM class "Add/subtract (extended register)". Layout:
//
//	sf op S 0 1 0 1 1 0 0 1 Rm option imm3 Rn Rd
//
// This class exists here for one reason: it is the only add or subtract with a
// register operand that can name the stack pointer. The shifted-register class
// reads register 31 as the zero register, so the prologue's CMP R16, RSP of
// specs/042-arm64-backend.md has no encoding there.
//
// The option field is fixed at UXTX for a 64-bit operand and UXTW for a
// 32-bit one, with a zero shift. That pair is how the manual and the
// assembler spell "no extension", and it is all the prologue needs. Rm is
// zero-register encoded: the stack pointer can be the first source, never the
// second.

const (
	opAddExt  = 0x0b200000
	opSubExt  = 0x4b200000
	opAddsExt = 0x2b200000
	opSubsExt = 0x6b200000
)

func addSubExt(base uint32, sz Size, rd, rn, rm uint32) uint32 {
	option := uint32(3) // UXTX
	if sz == Size32 {
		option = 2 // UXTW
	}
	return base | sz.sf() | rm<<16 | option<<13 | rn<<5 | rd
}

// AddRegRegSP encodes ADD dst, a, b where dst and a may be the stack pointer.
func AddRegRegSP(sz Size, dst, a, b Reg) uint32 {
	return addSubExt(opAddExt, sz, regSP(dst), regSP(a), regZ(b))
}

// SubRegRegSP encodes SUB dst, a, b where dst and a may be the stack pointer.
func SubRegRegSP(sz Size, dst, a, b Reg) uint32 {
	return addSubExt(opSubExt, sz, regSP(dst), regSP(a), regZ(b))
}

// AddsRegRegSP encodes ADDS dst, a, b where a may be the stack pointer. The
// destination cannot: the flag-setting forms read register 31 as the zero
// register there.
func AddsRegRegSP(sz Size, dst, a, b Reg) uint32 {
	return addSubExt(opAddsExt, sz, regZ(dst), regSP(a), regZ(b))
}

// SubsRegRegSP encodes SUBS dst, a, b where a may be the stack pointer.
func SubsRegRegSP(sz Size, dst, a, b Reg) uint32 {
	return addSubExt(opSubsExt, sz, regZ(dst), regSP(a), regZ(b))
}

// CmpRegRegSP encodes CMP a, b where a may be the stack pointer. The prologue
// compares the stack pointer against the goroutine's stack guard with it.
func CmpRegRegSP(sz Size, a, b Reg) uint32 {
	return addSubExt(opSubsExt, sz, 31, regSP(a), regZ(b))
}

// CmnRegRegSP encodes CMN a, b where a may be the stack pointer.
func CmnRegRegSP(sz Size, a, b Reg) uint32 {
	return addSubExt(opAddsExt, sz, 31, regSP(a), regZ(b))
}
