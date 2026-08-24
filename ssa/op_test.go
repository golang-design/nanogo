// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import "testing"

// TestOpTableIsComplete asserts every operation has a row.
//
// A missing row reads as a zero, which would make an unnamed operation take no
// arguments and touch no memory. The verifier would then accept a value it
// cannot describe, so the table is checked rather than trusted.
func TestOpTableIsComplete(t *testing.T) {
	for op := OpInvalid; op < opCount; op++ {
		info := opInfos[op]
		if info.name == "" {
			t.Errorf("operation %d has no row in the table", op)
			continue
		}
		if got := op.String(); got != info.name {
			t.Errorf("operation %d prints as %q, want %q", op, got, info.name)
		}
		if info.argLen < -1 {
			t.Errorf("%v declares %d arguments", op, info.argLen)
		}
		if info.takesMem && info.argLen == 0 {
			t.Errorf("%v takes memory and no arguments", op)
		}
		if info.makesMem && !info.takesMem && op != OpInitMem {
			t.Errorf("%v produces memory out of nothing", op)
		}
		if info.constant && info.argLen != 0 {
			t.Errorf("%v is constant and takes %d arguments", op, info.argLen)
		}
		if info.call && !(info.takesMem && info.makesMem) {
			t.Errorf("%v is a call and does not thread memory", op)
		}
		if info.commutative && info.argLen != 2 {
			t.Errorf("%v is commutative with %d arguments", op, info.argLen)
		}
	}
	if got := Op(opCount).String(); got != "op(?)" {
		t.Errorf("an operation outside the table prints as %q", got)
	}
	if info := infoOf(opCount); info.name != "" {
		t.Errorf("an operation outside the table has row %v", info)
	}
}

// TestOpAccessors checks the accessors against the table they read.
func TestOpAccessors(t *testing.T) {
	tests := []struct {
		op          Op
		takes       bool
		makes       bool
		commutative bool
		constant    bool
		call        bool
		argLen      int
	}{
		{OpAdd, false, false, true, false, false, 2},
		{OpSub, false, false, false, false, false, 2},
		{OpConstInt, false, false, false, true, false, 0},
		{OpInitMem, false, true, false, false, false, 0},
		{OpLoad, true, false, false, false, false, 2},
		{OpStore, true, true, false, false, false, 3},
		{OpStaticCall, true, true, false, false, true, -1},
		{OpPhi, false, false, false, false, false, -1},
	}
	for _, tc := range tests {
		if got := tc.op.TakesMemory(); got != tc.takes {
			t.Errorf("%v.TakesMemory is %v, want %v", tc.op, got, tc.takes)
		}
		if got := tc.op.MakesMemory(); got != tc.makes {
			t.Errorf("%v.MakesMemory is %v, want %v", tc.op, got, tc.makes)
		}
		if got := tc.op.IsCommutative(); got != tc.commutative {
			t.Errorf("%v.IsCommutative is %v, want %v", tc.op, got, tc.commutative)
		}
		if got := tc.op.IsConstant(); got != tc.constant {
			t.Errorf("%v.IsConstant is %v, want %v", tc.op, got, tc.constant)
		}
		if got := tc.op.IsCall(); got != tc.call {
			t.Errorf("%v.IsCall is %v, want %v", tc.op, got, tc.call)
		}
		if got := tc.op.ArgLen(); got != tc.argLen {
			t.Errorf("%v.ArgLen is %d, want %d", tc.op, got, tc.argLen)
		}
	}
	// The accessors are safe on an operation outside the table.
	bad := Op(opCount)
	if bad.TakesMemory() || bad.MakesMemory() || bad.IsCommutative() ||
		bad.IsConstant() || bad.IsCall() || bad.ArgLen() != 0 {
		t.Error("an operation outside the table has properties")
	}
}

// TestNoGreaterThan pins the canonicalisation. A comparison is Less or
// LessEqual with the arguments in the right order, and a pass may rely on
// that: there is no Greater to write a second rewrite rule for.
func TestNoGreaterThan(t *testing.T) {
	for op := OpInvalid; op < opCount; op++ {
		switch opInfos[op].name {
		case "Greater", "GreaterEqual", "Geq", "Gtr":
			t.Errorf("the operation set contains %v", op)
		}
	}
}
