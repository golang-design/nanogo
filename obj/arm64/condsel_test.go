// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package arm64

import (
	"fmt"
	"testing"
)

// The conditional select class against go tool asm.
//
// Two sweeps per form rather than their product. The condition and the
// registers are independent fields, so every condition over a few registers
// and every register triple at one condition covers each field over its whole
// range; the product would be a quarter of a million assembler lines and would
// prove nothing more. TestDiffBranchConditional splits the offset and the
// condition the same way.
//
// The condition names are Cond.String, which is the assembler's spelling for
// all 16. condNamesP9 is the wrong table here: it holds branch mnemonics, and
// this class takes a bare condition as its first operand.

// condSelForm is one base form of the class and its Plan 9 spelling. Plan 9
// writes the operands as cond, Rn, Rm, Rd, which is the reverse of this
// package's destination-first order in the last two.
type condSelForm struct {
	name string
	enc  func(sz Size, dst, a, b Reg, c Cond) (uint32, bool)
}

var condSelForms = []condSelForm{
	{"CSEL", Csel},
	{"CSINC", Csinc},
	{"CSINV", Csinv},
	{"CSNEG", Csneg},
}

// condSetForm is one of the two aliases.
type condSetForm struct {
	name string
	enc  func(sz Size, dst Reg, c Cond) (uint32, bool)
}

var condSetForms = []condSetForm{
	{"CSET", Cset},
	{"CSETM", Csetm},
}

// allConds is every condition the base forms take, which is all 16. AL and NV
// select the first source unconditionally and the assembler encodes them.
func allConds() []Cond {
	out := make([]Cond, 0, numCond)
	for c := EQ; c < numCond; c++ {
		out = append(out, c)
	}
	return out
}

// setConds is every condition the two aliases take, which is the 14 with a
// complement. AL and NV have none, so they have no CSET and go tool asm
// refuses them: a line holding one could not be compared at all.
func setConds() []Cond {
	out := make([]Cond, 0, numCond-2)
	for c := EQ; c < numCond; c++ {
		if hasComplement(c) {
			out = append(out, c)
		}
	}
	return out
}

func mustCond(t *testing.T, v uint32, ok bool, what string) uint32 {
	t.Helper()
	if !ok {
		t.Fatalf("%s: the encoder refused a form the assembler accepts", what)
	}
	return v
}

func TestDiffCondSelectConditions(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, f := range condSelForms {
		bothSizes(func(sz Size) {
			for _, c := range allConds() {
				for _, r := range smallRegs {
					dst, a, b := r, R1, R2
					lines = append(lines, fmt.Sprintf("\t%s\t%v, %v, %v, %v",
						mnemonic(f.name, sz), c, a, b, dst))
					v, ok := f.enc(sz, dst, a, b, c)
					want = append(want, mustCond(t, v, ok, f.name+" "+c.String()))
				}
			}
		})
	}
	compare(t, lines, want)
}

func TestDiffCondSelectRegisters(t *testing.T) {
	needAsm(t)
	triples := regTriples()
	var lines []string
	var want []uint32
	// One condition per form, and a different one per form, so that a field
	// which only ever saw EQ could not pass by holding zero.
	conds := []Cond{EQ, LT, HI, LE}
	for i, f := range condSelForms {
		c := conds[i]
		bothSizes(func(sz Size) {
			for _, r := range triples {
				dst, a, b := r[0], r[1], r[2]
				lines = append(lines, fmt.Sprintf("\t%s\t%v, %v, %v, %v",
					mnemonic(f.name, sz), c, a, b, dst))
				v, ok := f.enc(sz, dst, a, b, c)
				want = append(want, mustCond(t, v, ok, f.name))
			}
		})
	}
	compare(t, lines, want)
}

// TestDiffCondSet sweeps both aliases over every condition they take and every
// destination register. There is no third field to split off: the two sources
// are the zero register by definition of the alias.
func TestDiffCondSet(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, f := range condSetForms {
		bothSizes(func(sz Size) {
			for _, c := range setConds() {
				for _, dst := range sweepRegs() {
					lines = append(lines, fmt.Sprintf("\t%s\t%v, %v",
						mnemonic(f.name, sz), c, dst))
					v, ok := f.enc(sz, dst, c)
					want = append(want, mustCond(t, v, ok, f.name+" "+c.String()))
				}
			}
		})
	}
	compare(t, lines, want)
}

// TestCondSetIsTheBaseFormAlias states the alias mapping as an identity
// against the base form, over every condition and both widths.
//
// The differential sweep above already proves the bytes are the assembler's.
// This says which base form they are, which is the claim the file's comment
// makes and the one a reader cannot check by eye. If the inversion were
// dropped, this test and the differential one would both fail, and this one
// would name the reason.
func TestCondSetIsTheBaseFormAlias(t *testing.T) {
	bothSizes(func(sz Size) {
		for _, c := range setConds() {
			for _, dst := range []Reg{R0, R9, R25, ZR} {
				set, ok := Cset(sz, dst, c)
				if !ok {
					t.Fatalf("CSET %v, %v was refused", c, dst)
				}
				inc, ok := Csinc(sz, dst, ZR, ZR, c.Invert())
				if !ok {
					t.Fatalf("CSINC %v, ZR, ZR, %v was refused", dst, c.Invert())
				}
				if set != inc {
					t.Errorf("CSET %v, %v is %#08x and CSINC with the inverted condition is %#08x",
						c, dst, set, inc)
				}
				setm, ok := Csetm(sz, dst, c)
				if !ok {
					t.Fatalf("CSETM %v, %v was refused", c, dst)
				}
				inv, ok := Csinv(sz, dst, ZR, ZR, c.Invert())
				if !ok {
					t.Fatalf("CSINV %v, ZR, ZR, %v was refused", dst, c.Invert())
				}
				if setm != inv {
					t.Errorf("CSETM %v, %v is %#08x and CSINV with the inverted condition is %#08x",
						c, dst, setm, inv)
				}
			}
		}
	})
}

// TestCondSelectFieldLayout recovers the condition from the encoded word.
//
// The base forms carry the condition they test. The two aliases carry its
// complement, for the reason condsel.go gives, so a reader who takes the field
// of a CSET as its condition reads the opposite of the truth. This pins that
// difference rather than restating either encoder's expression.
func TestCondSelectFieldLayout(t *testing.T) {
	field := func(w uint32) Cond { return Cond(w >> 12 & 0xf) }
	bothSizes(func(sz Size) {
		for _, c := range allConds() {
			w, ok := Csel(sz, R3, R1, R2, c)
			if !ok {
				t.Fatalf("CSEL %v was refused", c)
			}
			if got := field(w); got != c {
				t.Errorf("CSEL %v encodes condition %v", c, got)
			}
			if !hasComplement(c) {
				continue
			}
			w, ok = Cset(sz, R3, c)
			if !ok {
				t.Fatalf("CSET %v was refused", c)
			}
			if got := field(w); got != c.Invert() {
				t.Errorf("CSET %v encodes condition %v, want the complement %v", c, got, c.Invert())
			}
		}
	})
}

// TestCondSetRejectsTheConditionsWithNoComplement is the range rejection of
// this class. AL and NV are to CSET what a same-width conversion is to FCVT: a
// form the mnemonic can name and the architecture does not have.
func TestCondSetRejectsTheConditionsWithNoComplement(t *testing.T) {
	bothSizes(func(sz Size) {
		for _, c := range []Cond{AL, NV, numCond, Cond(200)} {
			if _, ok := Cset(sz, R0, c); ok {
				t.Errorf("Cset accepted %v", c)
			}
			if _, ok := Csetm(sz, R0, c); ok {
				t.Errorf("Csetm accepted %v", c)
			}
		}
		// The base forms take AL and NV and refuse only a value outside the
		// field, which is the one case that is a caller bug rather than an
		// architectural gap.
		for _, f := range condSelForms {
			if _, ok := f.enc(sz, R0, R1, R2, numCond); ok {
				t.Errorf("%s accepted a condition outside the field", f.name)
			}
			if _, ok := f.enc(sz, R0, R1, R2, AL); !ok {
				t.Errorf("%s refused AL, which the class encodes", f.name)
			}
		}
	})
}
