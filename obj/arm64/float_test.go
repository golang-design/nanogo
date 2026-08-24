// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package arm64

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// The differential sweep of the floating-point encoders, against go tool asm
// and by the method encode_test.go states: every form over its operand range,
// one instruction per source line, and NOSPLIT|NOFRAME so that nothing grows
// a prologue in front of the instruction under test.
//
// Plan 9 spells the floating-point registers F0 to F31 and puts the source
// first, as it does for the integer forms, so FADDD F1, F2, F3 means
// F3 = F2 + F1.

// fregs is every floating-point register. All 32 are swept: F30 and F31 are
// not allocatable but the encoder must still reach them, because a
// materialised value goes to one of them.
func fregs() []Reg {
	out := make([]Reg, 0, 32)
	for r := F0; r <= F31; r++ {
		out = append(out, r)
	}
	return out
}

// fsmall is the subset used where the sweep is over triples. Sweeping all
// 32768 triples per form would prove nothing the full pairs below do not.
var fsmall = []Reg{F0, F7, F15, F16, F31}

// fregTriples covers every triple over fsmall and, over all 32 registers,
// every pair in each of the three positions.
func fregTriples() [][3]Reg {
	var out [][3]Reg
	for _, a := range fsmall {
		for _, b := range fsmall {
			for _, c := range fsmall {
				out = append(out, [3]Reg{a, b, c})
			}
		}
	}
	all := fregs()
	for _, a := range all {
		for _, b := range all {
			out = append(out,
				[3]Reg{a, b, F3},
				[3]Reg{F3, a, b},
				[3]Reg{a, F3, b})
		}
	}
	return out
}

func fregPairs() [][2]Reg {
	all := fregs()
	out := make([][2]Reg, 0, len(all)*len(all))
	for _, a := range all {
		for _, b := range all {
			out = append(out, [2]Reg{a, b})
		}
	}
	return out
}

// fsuffix is the Plan 9 suffix for a floating-point width: S for single and D
// for double.
func fsuffix(sz Size) string {
	if sz == Size32 {
		return "S"
	}
	return "D"
}

// ---------------------------------------------------------------------------
// Arithmetic

type fpRegForm struct {
	name string
	enc  func(sz Size, dst, a, b Reg) uint32
}

var fpRegForms = []fpRegForm{
	{"FADD", FaddRegReg},
	{"FSUB", FsubRegReg},
	{"FMUL", FmulRegReg},
	{"FDIV", FdivRegReg},
}

func TestDiffFloatThreeRegisterForms(t *testing.T) {
	needAsm(t)
	triples := fregTriples()
	var lines []string
	var want []uint32
	for _, f := range fpRegForms {
		bothSizes(func(sz Size) {
			for _, r := range triples {
				dst, a, b := r[0], r[1], r[2]
				lines = append(lines, fmt.Sprintf("\t%s%s\t%s, %s, %s",
					f.name, fsuffix(sz), b, a, dst))
				want = append(want, f.enc(sz, dst, a, b))
			}
		})
	}
	compare(t, lines, want)
}

type fpUnaryForm struct {
	name string
	enc  func(sz Size, dst, src Reg) uint32
}

var fpUnaryForms = []fpUnaryForm{
	{"FMOV", FmovRegReg},
	{"FNEG", FnegReg},
	{"FABS", FabsReg},
	{"FSQRT", FsqrtReg},
}

func TestDiffFloatTwoRegisterForms(t *testing.T) {
	needAsm(t)
	pairs := fregPairs()
	var lines []string
	var want []uint32
	for _, f := range fpUnaryForms {
		bothSizes(func(sz Size) {
			for _, p := range pairs {
				dst, src := p[0], p[1]
				lines = append(lines, fmt.Sprintf("\t%s%s\t%s, %s",
					f.name, fsuffix(sz), src, dst))
				want = append(want, f.enc(sz, dst, src))
			}
		})
	}
	compare(t, lines, want)
}

// ---------------------------------------------------------------------------
// Compare

func TestDiffFloatCompare(t *testing.T) {
	needAsm(t)
	pairs := fregPairs()
	var lines []string
	var want []uint32
	bothSizes(func(sz Size) {
		for _, p := range pairs {
			a, b := p[0], p[1]
			// Plan 9 is source first here too: FCMPD F1, F2 compares F2
			// against F1, so the second operand of the encoder is written
			// first.
			lines = append(lines, fmt.Sprintf("\tFCMP%s\t%s, %s", fsuffix(sz), b, a))
			want = append(want, FcmpRegReg(sz, a, b))
			lines = append(lines, fmt.Sprintf("\tFCMPE%s\t%s, %s", fsuffix(sz), b, a))
			want = append(want, FcmpeRegReg(sz, a, b))
		}
		for _, a := range fregs() {
			lines = append(lines, fmt.Sprintf("\tFCMP%s\t$(0.0), %s", fsuffix(sz), a))
			want = append(want, FcmpZero(sz, a))
			lines = append(lines, fmt.Sprintf("\tFCMPE%s\t$(0.0), %s", fsuffix(sz), a))
			want = append(want, FcmpeZero(sz, a))
		}
	})
	compare(t, lines, want)
}

// ---------------------------------------------------------------------------
// Conversions

// intSweepRegs is the integer half of a conversion sweep. It is every
// allocatable register plus the zero register, which is what sweepRegs
// returns, and it never holds R18.
func intSweepRegs() []Reg { return sweepRegs() }

// cvtName builds the Plan 9 mnemonic of an integer conversion. The name
// carries both widths: SCVTFWD is a W register to a double and FCVTZSDW is a
// double to a W register, so the W is a prefix on one side and a suffix on the
// other.
func cvtName(base string, fsz, isz Size, floatFirst bool) string {
	w := ""
	if isz == Size32 {
		w = "W"
	}
	if floatFirst {
		return base + fsuffix(fsz) + w
	}
	return base + w + fsuffix(fsz)
}

func TestDiffFloatFromInteger(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	forms := []struct {
		name string
		enc  func(from, to Size, dst, src Reg) uint32
	}{
		{"SCVTF", ScvtfReg},
		{"UCVTF", UcvtfReg},
	}
	for _, f := range forms {
		bothSizes(func(isz Size) {
			bothSizes(func(fsz Size) {
				for _, dst := range fregs() {
					for _, src := range intSweepRegs() {
						lines = append(lines, fmt.Sprintf("\t%s\t%s, %s",
							cvtName(f.name, fsz, isz, false), src, dst))
						want = append(want, f.enc(isz, fsz, dst, src))
					}
				}
			})
		})
	}
	compare(t, lines, want)
}

func TestDiffIntegerFromFloat(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	forms := []struct {
		name string
		enc  func(from, to Size, dst, src Reg) uint32
	}{
		{"FCVTZS", FcvtzsReg},
		{"FCVTZU", FcvtzuReg},
	}
	for _, f := range forms {
		bothSizes(func(fsz Size) {
			bothSizes(func(isz Size) {
				for _, dst := range intSweepRegs() {
					for _, src := range fregs() {
						lines = append(lines, fmt.Sprintf("\t%s\t%s, %s",
							cvtName(f.name, fsz, isz, true), src, dst))
						want = append(want, f.enc(fsz, isz, dst, src))
					}
				}
			})
		})
	}
	compare(t, lines, want)
}

// TestDiffFloatToFloat sweeps FCVT, the conversion between the two
// precisions. Plan 9 names it after the pair: FCVTSD widens a single to a
// double and FCVTDS narrows.
func TestDiffFloatToFloat(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, c := range []struct {
		name     string
		from, to Size
	}{
		{"FCVTSD", Size32, Size64},
		{"FCVTDS", Size64, Size32},
	} {
		for _, p := range fregPairs() {
			dst, src := p[0], p[1]
			v, ok := FcvtReg(c.from, c.to, dst, src)
			if !ok {
				t.Fatalf("%s %v, %v rejected", c.name, src, dst)
			}
			lines = append(lines, fmt.Sprintf("\t%s\t%s, %s", c.name, src, dst))
			want = append(want, v)
		}
	}
	compare(t, lines, want)
}

// TestFcvtRejectsEqualWidths is the range rejection of the one form that has
// no same-width encoding.
func TestFcvtRejectsEqualWidths(t *testing.T) {
	for _, sz := range []Size{Size32, Size64} {
		if _, ok := FcvtReg(sz, sz, F0, F1); ok {
			t.Errorf("FcvtReg accepted a conversion from %v to itself", sz)
		}
	}
}

func TestDiffFmovBetweenFiles(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	bothSizes(func(sz Size) {
		for _, f := range fregs() {
			for _, r := range intSweepRegs() {
				lines = append(lines, fmt.Sprintf("\tFMOV%s\t%s, %s", fsuffix(sz), r, f))
				want = append(want, FmovIntToFloat(sz, f, r))
				lines = append(lines, fmt.Sprintf("\tFMOV%s\t%s, %s", fsuffix(sz), f, r))
				want = append(want, FmovFloatToInt(sz, r, f))
			}
		}
	})
	compare(t, lines, want)
}

// ---------------------------------------------------------------------------
// The immediate
//
// The sweep is exhaustive: all 256 immediates, at both widths, against every
// destination register. The values come from an independent implementation of
// the manual's VFPExpandImm below, so a mistake in FloatImm8 cannot cancel
// against a mistake in the test.

// expandImm8 is VFPExpandImm from the ARM manual, written out for the 64-bit
// case. It is deliberately structural rather than arithmetic: it lays the
// exponent out bit by bit the way the manual's pseudocode does.
func expandImm8(imm8 uint32) float64 {
	sign := (imm8 >> 7) & 1
	b := (imm8 >> 6) & 1
	cd := (imm8 >> 4) & 3
	efgh := imm8 & 0xf

	// exp = NOT(b) : Replicate(b, 8) : cd, eleven bits wide.
	exp := uint64(b^1) << 10
	for i := 0; i < 8; i++ {
		exp |= uint64(b) << uint(9-i)
	}
	exp |= uint64(cd)

	bits := uint64(sign)<<63 | exp<<52 | uint64(efgh)<<48
	return math.Float64frombits(bits)
}

// p9Float is the Plan 9 spelling of a floating-point immediate. The assembler
// reads $(31) as an integer and rejects it against a floating-point
// destination, so a value with no fractional part still needs a point.
func p9Float(v float64) string {
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return "$(" + s + ")"
}

func TestDiffFmovImmediate(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for imm8 := uint32(0); imm8 < 256; imm8++ {
		val := expandImm8(imm8)
		got, ok := FloatImm8(val)
		if !ok {
			t.Fatalf("FloatImm8(%v) rejected a value the field reaches", val)
		}
		if got != imm8 {
			t.Fatalf("FloatImm8(%v) = %d, want %d", val, got, imm8)
		}
		if imm8 == 0 {
			// The oracle cannot reach this one. TestFmovImmediateZeroField
			// covers it and says why.
			continue
		}
		bothSizes(func(sz Size) {
			for _, dst := range fregs() {
				w, ok := FmovImm(sz, dst, val)
				if !ok {
					t.Fatalf("FmovImm(%v, %v, %v) rejected", sz, dst, val)
				}
				lines = append(lines, fmt.Sprintf("\tFMOV%s\t%s, %s",
					fsuffix(sz), p9Float(val), dst))
				want = append(want, w)
			}
		})
	}
	compare(t, lines, want)
}

// TestFmovImmediateZeroField covers the one immediate go tool asm cannot be
// asked for.
//
// The assembler decides between the immediate form and a load from a constant
// symbol with `chipfloat7(f64) > 0`, in cmd/internal/obj/arm64/obj7.go. The
// test is strict where the field it computes is an unsigned 8-bit value whose
// zero is a legal encoding, so the single value whose immediate field is zero,
// +2.0, is assembled as a PC-relative load instead. Every other value of the
// field, its negative -2.0 included, takes the immediate form.
//
// specs/041-instruction-encoding.md's rule for a range the assembler cannot
// reach applies: the encoding is checked by recovering the field from the
// encoded word and expanding it back through the manual's pseudocode, not by
// restating the encoder's own expression.
func TestFmovImmediateZeroField(t *testing.T) {
	for _, sz := range []Size{Size32, Size64} {
		w, ok := FmovImm(sz, F7, 2.0)
		if !ok {
			t.Fatalf("FmovImm(%v, F7, 2.0) rejected", sz)
		}
		if got := (w >> 13) & 0xff; got != 0 {
			t.Errorf("the immediate field of %#08x is %d, want 0", w, got)
		}
		if got := expandImm8((w >> 13) & 0xff); got != 2.0 {
			t.Errorf("the immediate field of %#08x expands to %v, want 2", w, got)
		}
		if got := w & 0x1f; got != uint32(F7-F0) {
			t.Errorf("the destination field of %#08x is %d, want %d", w, got, F7-F0)
		}
		if got := (w >> 22) & 3; got != uint32(sz&1) {
			t.Errorf("the ftype field of %#08x is %d, want %d", w, got, sz&1)
		}
		// Every other field is fixed by the class.
		if w&^(0xff<<13|0x1f|3<<22) != opFpImm {
			t.Errorf("%#08x is not in the floating-point immediate class", w)
		}
	}
}

// TestFloatImmediateRejectsExactly walks the values the field cannot reach.
//
// Zero, the infinities and every NaN are outside it, which is the fact the
// lowering rules turn on: a constant this rejects is materialised in an
// integer register and moved across with FMOV.
func TestFloatImmediateRejectsExactly(t *testing.T) {
	reachable := make(map[float64]bool, 256)
	for imm8 := uint32(0); imm8 < 256; imm8++ {
		reachable[expandImm8(imm8)] = true
	}
	if len(reachable) != 256 {
		t.Fatalf("the 256 immediates name %d distinct values", len(reachable))
	}
	for _, v := range []float64{
		0, math.Copysign(0, -1),
		math.Inf(1), math.Inf(-1), math.NaN(),
		0.1, 0.2, 1.0 / 3.0, 31.5, 32, -32, 0.0625, -0.0625,
		1e300, -1e300, math.SmallestNonzeroFloat64, math.MaxFloat64,
	} {
		if _, ok := FloatImm8(v); ok {
			t.Errorf("FloatImm8(%v) accepted a value the field cannot reach", v)
		}
		if _, ok := FmovImm(Size64, F0, v); ok {
			t.Errorf("FmovImm(%v) accepted", v)
		}
	}
	// The boundary of the exponent window, from both sides.
	for _, c := range []struct {
		val  float64
		want bool
	}{
		{0.125, true}, {0.1249999, false},
		{31, true}, {32, false},
		{-0.125, true}, {-31, true},
		{1.9375, true},   // the largest fraction at exponent zero
		{1.96875, false}, // one bit below it, which the four bits cannot hold
	} {
		if _, ok := FloatImm8(c.val); ok != c.want {
			t.Errorf("FloatImm8(%v) = %v, want %v", c.val, ok, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Loads and stores
//
// The floating-point transfers reuse the integer addressing forms, so the
// sweep is the same one encode_test.go runs, with the transfer register in the
// other file.

var fpMemForms = []memForm{
	{LoadF32, "FMOVS", false},
	{LoadF64, "FMOVD", false},
	{StoreF32, "FMOVS", true},
	{StoreF64, "FMOVD", true},
}

func TestDiffFloatMemUnsignedOffset(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, f := range fpMemForms {
		scale := f.op.Scale()
		for i := int64(0); i <= MaxMemOffsetScaled; i++ {
			off := i * scale
			v, ok := MemUnsignedOffset(f.op, F2, R1, off)
			if !ok {
				t.Fatalf("%v offset %d rejected", f.op, off)
			}
			lines = append(lines, f.line("", F2, off, R1))
			want = append(want, v)
		}
		bases := append(AllocatableRegs(), RSP)
		for _, base := range bases {
			for _, rt := range fregs() {
				off := 3 * scale
				v, ok := MemUnsignedOffset(f.op, rt, base, off)
				if !ok {
					t.Fatalf("%v rejected", f.op)
				}
				lines = append(lines, f.line("", rt, off, base))
				want = append(want, v)
			}
		}
	}
	compare(t, lines, want)
}

func TestDiffFloatMemUnscaledAndIndexed(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, f := range fpMemForms {
		scale := f.op.Scale()
		for off := int64(MinMemOffsetUnscaled); off <= MaxMemOffsetUnscaled; off++ {
			// go tool asm reaches the unscaled form only where the scaled one
			// cannot hold the offset, which is where it is negative or not a
			// multiple of the access size.
			if off >= 0 && off%scale == 0 {
				continue
			}
			v, ok := MemUnscaled(f.op, F2, R1, off)
			if !ok {
				t.Fatalf("%v unscaled offset %d rejected", f.op, off)
			}
			lines = append(lines, f.line("", F2, off, R1))
			want = append(want, v)
		}
		for off := int64(MinMemOffsetUnscaled); off <= MaxMemOffsetUnscaled; off++ {
			v, ok := MemPreIndex(f.op, F2, R1, off)
			if !ok {
				t.Fatalf("%v pre-index offset %d rejected", f.op, off)
			}
			lines = append(lines, f.line(".W", F2, off, R1))
			want = append(want, v)

			v, ok = MemPostIndex(f.op, F2, R1, off)
			if !ok {
				t.Fatalf("%v post-index offset %d rejected", f.op, off)
			}
			lines = append(lines, f.line(".P", F2, off, R1))
			want = append(want, v)
		}
	}
	compare(t, lines, want)
}

func TestDiffFloatMemRegisterOffset(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	exts := []Extend{UXTW, LSLX, SXTW, SXTX}
	bases := append(AllocatableRegs(), RSP)
	for _, f := range fpMemForms {
		scale := f.op.Scale()
		for _, ext := range exts {
			for _, shifted := range []bool{false, true} {
				v, ok := MemRegOffset(f.op, F3, R1, R2, ext, shifted)
				if !ok {
					t.Fatalf("%v %v rejected", f.op, ext)
				}
				idx := R2.String() + extendSuffix(ext, shifted, scale)
				if f.store {
					lines = append(lines, fmt.Sprintf("\t%s\t%s, (%s)(%s)", f.p9, F3, R1, idx))
				} else {
					lines = append(lines, fmt.Sprintf("\t%s\t(%s)(%s), %s", f.p9, R1, idx, F3))
				}
				want = append(want, v)
			}
		}
		for _, base := range bases {
			for _, index := range AllocatableRegs() {
				v, ok := MemRegOffset(f.op, F3, base, index, LSLX, false)
				if !ok {
					t.Fatal("rejected")
				}
				if f.store {
					lines = append(lines, fmt.Sprintf("\t%s\t%s, (%s)(%s)", f.p9, F3, base, index))
				} else {
					lines = append(lines, fmt.Sprintf("\t%s\t(%s)(%s), %s", f.p9, base, index, F3))
				}
				want = append(want, v)
			}
		}
	}
	compare(t, lines, want)
}

// TestFloatMemFieldLayout pins the two bits that separate a floating-point
// transfer from an integer one, because they are the whole difference and a
// wrong V bit produces an instruction that loads the right bytes into the
// wrong file.
func TestFloatMemFieldLayout(t *testing.T) {
	for _, c := range []struct {
		op        MemOp
		size, opc uint32
	}{
		{LoadF32, 2, 1},
		{LoadF64, 3, 1},
		{StoreF32, 2, 0},
		{StoreF64, 3, 0},
	} {
		v, ok := MemUnsignedOffset(c.op, F2, R1, c.op.Scale())
		if !ok {
			t.Fatalf("%v rejected", c.op)
		}
		if got := v >> 30; got != c.size {
			t.Errorf("%v size field %d, want %d", c.op, got, c.size)
		}
		if got := (v >> 22) & 3; got != c.opc {
			t.Errorf("%v opc field %d, want %d", c.op, got, c.opc)
		}
		if v&(1<<26) == 0 {
			t.Errorf("%v does not set the V bit", c.op)
		}
		if !c.op.IsFloat() {
			t.Errorf("%v does not report itself as floating point", c.op)
		}
	}
	for _, op := range []MemOp{StoreB, LoadX, LoadWS64} {
		v, ok := MemUnsignedOffset(op, R2, R1, 0)
		if !ok {
			t.Fatalf("%v rejected", op)
		}
		if v&(1<<26) != 0 {
			t.Errorf("%v sets the V bit", op)
		}
		if op.IsFloat() {
			t.Errorf("%v reports itself as floating point", op)
		}
	}
}

// ---------------------------------------------------------------------------
// The register files are kept apart

// TestFloatRegisterMisuseIsCaught asserts that an encoder given a register
// from the wrong file panics rather than encoding.
//
// This is the property the package comment's register-31 rule extends to the
// second file. Register number 5 is X5 in one instruction and D5 in another,
// and the file is a property of the instruction, so a caller that mixes them
// has written the wrong instruction and there is no value it could have meant.
func TestFloatRegisterMisuseIsCaught(t *testing.T) {
	cases := []struct {
		name string
		call func()
	}{
		{"FADD with an integer destination", func() { FaddRegReg(Size64, R0, F1, F2) }},
		{"FADD with an integer source", func() { FaddRegReg(Size64, F0, R1, F2) }},
		{"FMOV between two floats given an integer", func() { FmovRegReg(Size64, F0, R1) }},
		{"FCMP given an integer", func() { FcmpRegReg(Size64, R0, F1) }},
		{"SCVTF given a float source", func() { ScvtfReg(Size64, Size64, F0, F1) }},
		{"FCVTZS given a float destination", func() { FcvtzsReg(Size64, Size64, F0, F1) }},
		{"FMOV to a float from a float", func() { FmovIntToFloat(Size64, F0, F1) }},
		{"an integer add given a float", func() { AddRegReg(Size64, F0, R1, R2) }},
		{"an integer load given a float base", func() { MemUnsignedOffset(LoadX, R0, F1, 0) }},
		{"an integer load into a float", func() { MemUnsignedOffset(LoadX, F0, R1, 0) }},
		{"a float load into an integer register", func() { MemUnsignedOffset(LoadF64, R0, R1, 0) }},
		{"a float store of the zero register", func() { MemUnsignedOffset(StoreF64, ZR, R1, 0) }},
		{"a float store indexed by a float", func() { MemRegOffset(StoreF64, F0, R1, F2, LSLX, false) }},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", c.name)
				}
			}()
			c.call()
		}()
	}
}

// TestFloatRegisterFile pins the register table against specs/042 and
// specs/030.
func TestFloatRegisterFile(t *testing.T) {
	if NumIntReg != int(F0) {
		t.Fatalf("NumIntReg is %d and F0 is %d", NumIntReg, F0)
	}
	if int(numReg) != NumIntReg+32 {
		t.Fatalf("the register file holds %d registers, want %d", numReg, NumIntReg+32)
	}
	for r := F0; r <= F31; r++ {
		if !r.IsFloat() {
			t.Errorf("%v is not a floating-point register", r)
		}
		if !r.Valid() {
			t.Errorf("%v is not valid", r)
		}
		want := "F" + strconv.Itoa(int(r-F0))
		if got := r.String(); got != want {
			t.Errorf("Reg(%d).String() = %q, want %q", r, got, want)
		}
		if got := regF(r); got != uint32(r-F0) {
			t.Errorf("regF(%v) = %d", r, got)
		}
	}
	for r := R0; r <= RSP; r++ {
		if r.IsFloat() {
			t.Errorf("%v reports itself as floating point", r)
		}
	}
	// specs/042: F0 to F15 are the argument and result registers and F16 to
	// F31 are scratch. Both halves are allocatable; only the materialisation
	// pair is held back, which is what R16 and R17 are for the integer file.
	got := AllocatableFRegs()
	if len(got) != 30 {
		t.Fatalf("AllocatableFRegs returned %d registers, want 30", len(got))
	}
	for i, r := range got {
		if r != F0+Reg(i) {
			t.Fatalf("AllocatableFRegs[%d] = %v, want %v", i, r, F0+Reg(i))
		}
	}
	if RegFScratchLo.Allocatable() || RegFScratchHi.Allocatable() {
		t.Error("a floating-point materialisation register is allocatable")
	}
	// The two accessors must not leak into each other.
	for _, r := range AllocatableRegs() {
		if r.IsFloat() {
			t.Errorf("AllocatableRegs returned %v", r)
		}
	}
	for _, r := range AllocatableFRegs() {
		if !r.IsFloat() {
			t.Errorf("AllocatableFRegs returned %v", r)
		}
	}
}

// TestFloatBitsRoundTrip covers the two layout helpers, which are the only
// place the IEEE 754 pattern of a value is written and which the compiler
// above uses to carry a constant.
func TestFloatBitsRoundTrip(t *testing.T) {
	for _, v := range []float64{
		0, math.Copysign(0, -1), 1, -1, 1.5, 0.1, 31,
		math.Inf(1), math.Inf(-1),
		math.MaxFloat32, -math.MaxFloat32,
	} {
		if got := FloatFromBits(Size64, FloatBits(Size64, v)); got != v {
			t.Errorf("the double round trip of %v gave %v", v, got)
		}
		want := float64(float32(v))
		if got := FloatFromBits(Size32, FloatBits(Size32, v)); got != want {
			t.Errorf("the single round trip of %v gave %v, want %v", v, got, want)
		}
	}
	// The patterns themselves, so that a swap of the two widths is caught.
	if got := FloatBits(Size64, 1.5); got != 0x3ff8000000000000 {
		t.Errorf("FloatBits(Size64, 1.5) = %#x", got)
	}
	if got := FloatBits(Size32, 1.5); got != 0x3fc00000 {
		t.Errorf("FloatBits(Size32, 1.5) = %#x", got)
	}
	// A NaN does not compare equal to itself, so it is checked by its bits.
	if got := FloatBits(Size64, math.NaN()); got>>52&0x7ff != 0x7ff || got&(1<<52-1) == 0 {
		t.Errorf("FloatBits of a NaN gave %#x", got)
	}
	if !math.IsNaN(FloatFromBits(Size32, 0x7fc00000)) {
		t.Error("a single-precision NaN pattern did not come back as a NaN")
	}
}

// TestMemOpIsFloatRejectsAnInvalidForm covers the guard on the table lookup.
func TestMemOpIsFloatRejectsAnInvalidForm(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("IsFloat accepted a MemOp outside the table")
		}
	}()
	numMemOp.IsFloat()
}
