// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package arm64

import (
	"sort"
	"testing"
)

// notAllocatable is specs/042's table, transcribed. It is written out here
// rather than derived from the package, so that the test fails if the table
// changes and nobody changed the spec.
var notAllocatable = []Reg{R16, R17, R18, R26, R27, R28, R29, R30, RSP, ZR}

func TestNotAllocatable(t *testing.T) {
	for _, r := range notAllocatable {
		if r.Allocatable() {
			t.Errorf("%v is allocatable", r)
		}
	}
	blocked := make(map[Reg]bool, len(notAllocatable))
	for _, r := range notAllocatable {
		blocked[r] = true
	}
	for r := Reg(0); r < numReg; r++ {
		if r.Allocatable() == blocked[r] {
			t.Errorf("%v: allocatable=%v, in the blocked list=%v",
				r, r.Allocatable(), blocked[r])
		}
	}
}

// TestR18IsNeverOffered is the darwin rule. R18 belongs to the platform, and a
// program that writes it corrupts state the fault shows up far away from, so
// the allocator must never see it and no encoder may reach it from an
// allocatable register.
func TestR18IsNeverOffered(t *testing.T) {
	if R18.Allocatable() {
		t.Fatal("R18 is allocatable")
	}
	for _, r := range AllocatableRegs() {
		if r == R18 {
			t.Fatal("AllocatableRegs returned R18")
		}
		// The two encoding helpers are the only places a Reg becomes bits, so
		// checking them covers every encoder in the package.
		if regZ(r) == 18 || regSP(r) == 18 {
			t.Fatalf("%v encodes as register 18", r)
		}
	}
}

func TestAllocatableRegsIsSortedAndStable(t *testing.T) {
	got := AllocatableRegs()
	if len(got) == 0 {
		t.Fatal("no allocatable registers")
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
		t.Error("AllocatableRegs is not in increasing order")
	}
	// The order is fixed rather than derived from a map, per
	// specs/053-determinism.md, so two calls must agree.
	again := AllocatableRegs()
	for i := range got {
		if got[i] != again[i] {
			t.Fatalf("AllocatableRegs differs between calls at %d", i)
		}
	}
	// specs/042: R0 to R15 and R19 to R25.
	want := []Reg{R0, R1, R2, R3, R4, R5, R6, R7, R8, R9, R10, R11, R12, R13, R14, R15,
		R19, R20, R21, R22, R23, R24, R25}
	if len(got) != len(want) {
		t.Fatalf("AllocatableRegs returned %d registers, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllocatableRegs[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRegString(t *testing.T) {
	cases := []struct {
		r    Reg
		want string
	}{
		{R0, "R0"}, {R15, "R15"}, {R18, "R18"}, {R30, "R30"},
		{ZR, "ZR"}, {RSP, "RSP"}, {Reg(99), "Reg(99)"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("Reg(%d).String() = %q, want %q", c.r, got, c.want)
		}
	}
	if Reg(99).Valid() {
		t.Error("Reg(99) is valid")
	}
	if !RSP.Valid() {
		t.Error("RSP is not valid")
	}
	if Reg(99).Allocatable() {
		t.Error("Reg(99) is allocatable")
	}
}

// TestAbiRoles pins the register roles specs/030-abi.md assigns. A deviation
// here is not a different choice, it is memory corruption.
func TestAbiRoles(t *testing.T) {
	cases := []struct {
		got  Reg
		want Reg
		what string
	}{
		{RegClosure, R26, "closure context"},
		{RegG, R28, "current goroutine"},
		{RegFramePtr, R29, "frame pointer"},
		{RegLink, R30, "link register"},
		{RegTrampLo, R16, "linker trampoline"},
		{RegTrampHi, R17, "linker trampoline"},
		{RegPlatform, R18, "reserved by darwin"},
		{RegAsmScratch, R27, "assembler scratch"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s is %v, want %v", c.what, c.got, c.want)
		}
		if c.got.Allocatable() {
			t.Errorf("%s (%v) is allocatable", c.what, c.got)
		}
	}
}

func TestCond(t *testing.T) {
	for c := EQ; c < numCond; c++ {
		if !c.Valid() {
			t.Errorf("%d is not valid", c)
		}
		if c.String() == "" {
			t.Errorf("%d has no name", c)
		}
		if c < AL && c.Invert().Invert() != c {
			t.Errorf("inverting %v twice does not return it", c)
		}
	}
	if EQ.Invert() != NE || NE.Invert() != EQ {
		t.Error("EQ and NE are not each other's inverse")
	}
	if GE.Invert() != LT || GT.Invert() != LE {
		t.Error("the signed conditions are not paired")
	}
	if HS.Invert() != LO || HI.Invert() != LS {
		t.Error("the unsigned conditions are not paired")
	}
	if Cond(99).Valid() {
		t.Error("Cond(99) is valid")
	}
	if got := Cond(99).String(); got != "Cond(99)" {
		t.Errorf("Cond(99).String() = %q", got)
	}
}

func TestSizeAndShiftStrings(t *testing.T) {
	if Size64.String() != "64" || Size32.String() != "32" {
		t.Error("Size.String is wrong")
	}
	if Size64.bits() != 64 || Size32.bits() != 32 {
		t.Error("Size.bits is wrong")
	}
	if Size64.sf() != 1<<31 || Size32.sf() != 0 {
		t.Error("Size.sf is wrong")
	}
	for _, c := range []struct {
		s    Shift
		want string
	}{{LSL, "LSL"}, {LSR, "LSR"}, {ASR, "ASR"}, {ROR, "ROR"}, {Shift(9), "Shift(9)"}} {
		if got := c.s.String(); got != c.want {
			t.Errorf("Shift(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
	for _, c := range []struct {
		e    Extend
		want string
	}{{UXTW, "UXTW"}, {LSLX, "LSL"}, {SXTW, "SXTW"}, {SXTX, "SXTX"}, {Extend(0), "Extend(0)"}} {
		if got := c.e.String(); got != c.want {
			t.Errorf("Extend(%d).String() = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestMemOpString(t *testing.T) {
	for m := MemOp(0); m < numMemOp; m++ {
		if m.String() == "" {
			t.Errorf("MemOp(%d) has no name", m)
		}
		if s := m.Scale(); s != 1 && s != 2 && s != 4 && s != 8 {
			t.Errorf("%v has scale %d", m, s)
		}
	}
	if got := numMemOp.String(); got != "MemOp(13)" {
		t.Errorf("numMemOp.String() = %q", got)
	}
}

func TestNop(t *testing.T) {
	if got := Nop(); got != 0xd503201f {
		t.Errorf("Nop() = %#08x, want 0xd503201f", got)
	}
}
