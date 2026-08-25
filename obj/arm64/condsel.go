// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package arm64

// The conditional select instructions, part of rule group 1 of
// specs/042-arm64-backend.md.
//
// This is the only way to put a general condition into a register. ADC reads
// the carry flag and nothing else reads the rest, so a comparison whose result
// is a value rather than a branch has no other lowering: every integer and
// floating-point comparison that Go treats as a bool ends in a CSET. A
// comparison that is only branched on becomes B.cond and needs nothing here.
//
// One class covers six instructions. Four of them are base forms that differ
// only in the two opcode bits, and CSET and CSETM are aliases of two of the
// base forms with the zero register in both sources. The alias is written
// once, in terms of the base form, because the mapping is the part of this
// file a reader cannot check against a disassembly by eye.

// ---------------------------------------------------------------------------
// Conditional select
//
// ARM class "Conditional select". Layout:
//
//	sf op S 1 1 0 1 0 1 0 0 Rm cond 0 o2 Rn Rd
//
// op at bit 30 and o2 at bit 10 name the four base forms. Each of them selects
// Rn when the condition holds and a function of Rm when it does not:
//
//	CSEL  Rd, Rn, Rm, cond   Rd = cond ? Rn : Rm
//	CSINC Rd, Rn, Rm, cond   Rd = cond ? Rn : Rm + 1
//	CSINV Rd, Rn, Rm, cond   Rd = cond ? Rn : ^Rm
//	CSNEG Rd, Rn, Rm, cond   Rd = cond ? Rn : -Rm
//
// The asymmetry is the point: the "else" arm carries the increment, the
// complement or the negation, and that is what lets one instruction with two
// zero registers produce a constant that depends on the flags.
//
// cond is a full 4-bit field, so AL and NV are encodable in the base forms and
// both mean "always Rn". No rule emits either, but allowing them costs nothing
// and refusing them would be this package inventing a restriction the
// architecture does not have.

const (
	opCsel  = 0x1a800000 // op 0, o2 0
	opCsinc = 0x1a800400 // op 0, o2 1
	opCsinv = 0x5a800000 // op 1, o2 0
	opCsneg = 0x5a800400 // op 1, o2 1
)

func condSelect(base uint32, sz Size, rd, rn, rm uint32, c Cond) (uint32, bool) {
	if !c.Valid() {
		return 0, false
	}
	return base | sz.sf() | rm<<16 | uint32(c)<<12 | rn<<5 | rd, true
}

// Csel encodes CSEL dst, a, b, c, which computes dst = c ? a : b.
//
// It reports false only for a condition outside the 16 the field holds. A Cond
// is an enum, so that is a bug in the caller and not a value that came from a
// Go program, but the whole class returns the same pair rather than half of it
// panicking and half of it reporting.
func Csel(sz Size, dst, a, b Reg, c Cond) (uint32, bool) {
	return condSelect(opCsel, sz, regZ(dst), regZ(a), regZ(b), c)
}

// Csinc encodes CSINC dst, a, b, c, which computes dst = c ? a : b + 1.
func Csinc(sz Size, dst, a, b Reg, c Cond) (uint32, bool) {
	return condSelect(opCsinc, sz, regZ(dst), regZ(a), regZ(b), c)
}

// Csinv encodes CSINV dst, a, b, c, which computes dst = c ? a : ^b.
func Csinv(sz Size, dst, a, b Reg, c Cond) (uint32, bool) {
	return condSelect(opCsinv, sz, regZ(dst), regZ(a), regZ(b), c)
}

// Csneg encodes CSNEG dst, a, b, c, which computes dst = c ? a : -b.
//
// The negation is the two's complement of the whole operand at the width sz,
// so a most-negative value negates to itself. No rule emits this form yet: it
// is here because the class is one encoding and leaving out a quarter of it
// would hide the o2 bit.
func Csneg(sz Size, dst, a, b Reg, c Cond) (uint32, bool) {
	return condSelect(opCsneg, sz, regZ(dst), regZ(a), regZ(b), c)
}

// ---------------------------------------------------------------------------
// The two aliases
//
// CSET and CSETM are not instructions. Each is a base form with the zero
// register in both sources and the condition inverted:
//
//	CSET  dst, c   is  CSINC dst, ZR, ZR, invert(c)   dst = c ? 1 : 0
//	CSETM dst, c   is  CSINV dst, ZR, ZR, invert(c)   dst = c ? -1 : 0
//
// The inversion follows from the shape of the class. The base form selects Rn
// when its field's condition holds, and Rn is the zero register here, so the
// value the alias wants (1 from ZR + 1, or -1 from ^ZR) is in the "else" arm.
// Putting invert(c) in the field is what moves the wanted value into the arm c
// selects. Reading the encoded field of a CSET as the condition the
// instruction tests therefore gives the complement of it, and that is the one
// mistake a reader checking this file against a disassembly will make.
//
// AL and NV have no alias, and the reason is architectural rather than a
// syntax gap. CSET dst, AL would need NV in the field, and NV is not "never":
// it executes as always, so the instruction would select Rn, which is ZR, and
// set dst to 0 for a condition that is always true. The one form the assembler
// could build is the opposite of what the mnemonic says, so the architecture
// excludes the pair and go tool asm rejects it. That is the false these two
// return, in the shape FcvtReg uses for the conversion width that has no
// encoding.

// hasComplement reports whether c is one of the 14 conditions that has an
// inverse. AL and NV are the pair that does not: Invert maps each to the
// other, and both execute as always.
func hasComplement(c Cond) bool { return c.Valid() && c != AL && c != NV }

// Cset encodes CSET dst, c, which sets dst to 1 when c holds and to 0
// otherwise. It reports false for AL and NV, which have no CSET.
//
// This is the instruction every comparison that produces a value ends in. The
// condition is the same value the conditional branch would carry, so a
// comparison lowered to a branch and the same comparison lowered to a bool
// read the same flags with the same 4-bit code.
func Cset(sz Size, dst Reg, c Cond) (uint32, bool) {
	if !hasComplement(c) {
		return 0, false
	}
	return condSelect(opCsinc, sz, regZ(dst), regZ(ZR), regZ(ZR), c.Invert())
}

// Csetm encodes CSETM dst, c, which sets dst to -1 when c holds and to 0
// otherwise, so the result is a mask of the whole register. It reports false
// for AL and NV.
func Csetm(sz Size, dst Reg, c Cond) (uint32, bool) {
	if !hasComplement(c) {
		return 0, false
	}
	return condSelect(opCsinv, sz, regZ(dst), regZ(ZR), regZ(ZR), c.Invert())
}
