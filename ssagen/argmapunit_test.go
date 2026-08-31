// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/ssa"
)

// The argument map is what the collector reads for a frame it cannot walk any
// other way, so every refusal in it is a refusal to hand the runtime a map
// that might be wrong. These drive those refusals directly.
//
// They are unit tests rather than programs because each one is a shape the
// placement cannot produce today: a pointer at a half-word offset, an area
// wider than the runtime's own arithmetic, a signature the IR did not carry.
// A refusal that cannot be reached is still worth writing, because the
// alternative is a map that is wrong in silence when the placement changes.

var argPtr = &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, PtrBits: []byte{1}, Name: "*int"}

// TestArgMapsRefusesWhatItCannotDescribe covers the entry conditions.
func TestArgMapsRefusesWhatItCannotDescribe(t *testing.T) {
	tg := ssa.NewArm64Target()
	fn := &ir.Func{Name: "f", Type: &ir.Type{Kind: ir.FuncKind}}

	if _, _, err := ArgMaps(nil, "p.f", tg); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("a nil declaration gave %v, want a refusal naming the signature", err)
	}
	if _, _, err := ArgMaps(&ir.Func{Name: "f"}, "p.f", tg); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("a declaration with no type gave %v, want a refusal naming the signature", err)
	}
	if _, _, err := ArgMaps(fn, "p.f", nil); err == nil || !strings.Contains(err.Error(), "target") {
		t.Errorf("a nil target gave %v, want a refusal naming it", err)
	}
	// The ordinary case still works, so the guards above are guards and not
	// the whole function.
	stackmap, arginfo, err := ArgMaps(fn, "p.f", tg)
	if err != nil {
		t.Fatalf("an empty signature: %v", err)
	}
	if stackmap == nil || arginfo == nil {
		t.Fatalf("an empty signature produced %v and %v, want both symbols", stackmap, arginfo)
	}
	if want := "p.f.args_stackmap"; stackmap.Name != want {
		t.Errorf("the stack map is named %q, want %q", stackmap.Name, want)
	}
	if want := "p.f.arginfo0"; arginfo.Name != want {
		t.Errorf("the argument info is named %q, want %q", arginfo.Name, want)
	}
}

// TestArgsStackMapRefusesAnAreaItCannotAddress covers the two width rules.
//
// The runtime reads this map with int32 arithmetic over whole words, so an
// area that is not a whole number of words, or wider than that arithmetic
// holds, is refused rather than truncated. A truncated map is a map that
// describes the wrong words.
func TestArgsStackMapRefusesAnAreaItCannotAddress(t *testing.T) {
	if _, err := argsStackMap("p.f", nil, nil, 12); err == nil || !strings.Contains(err.Error(), "whole number of words") {
		t.Errorf("a 12 byte area gave %v, want a refusal naming the width", err)
	}
	if _, err := argsStackMap("p.f", nil, nil, (maxArgMapWords+1)*ir.PtrSize); err == nil ||
		!strings.Contains(err.Error(), "int32 arithmetic") {
		t.Errorf("an oversized area gave %v, want a refusal naming the runtime's arithmetic", err)
	}
	// A whole number of words within the bound is written.
	if _, err := argsStackMap("p.f", nil, nil, 8); err != nil {
		t.Errorf("a one word area: %v", err)
	}
}

// TestSetArgBitsRefusesAPointerItCannotPlace covers the two offset rules.
//
// A pointer word is a bit in a bitmap indexed by word, so a value holding one
// at a half-word offset has no bit to set, and one past the end of the area
// has no bit either. Both are refused. Writing outside the bitmap would be a
// map describing words that are not in the frame.
func TestSetArgBitsRefusesAPointerItCannotPlace(t *testing.T) {
	bv := make([]byte, 1)

	// Not on a word boundary.
	off := &ssa.ABIValue{Type: argPtr, Off: 4}
	if err := setArgBits("p.f", bv, 8, off); err == nil || !strings.Contains(err.Error(), "word boundary") {
		t.Errorf("a pointer at offset 4 gave %v, want a refusal naming the boundary", err)
	}

	// Past the end of the area.
	past := &ssa.ABIValue{Type: argPtr, Off: 64}
	if err := setArgBits("p.f", bv, 8, past); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("a pointer past the area gave %v, want a refusal naming it", err)
	}

	// A value that holds no pointer sets nothing and refuses nothing, whatever
	// its offset, because the map only describes pointers.
	scalar := &ssa.ABIValue{Type: &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}, Off: 4}
	if err := setArgBits("p.f", bv, 8, scalar); err != nil {
		t.Errorf("a scalar at a half-word offset was refused: %v", err)
	}
	if bv[0] != 0 {
		t.Errorf("a scalar set bits: %08b", bv[0])
	}

	// A pointer on a boundary sets exactly its own word.
	ok := &ssa.ABIValue{Type: argPtr, Off: 16}
	if err := setArgBits("p.f", bv, 8, ok); err != nil {
		t.Fatalf("a pointer at offset 16: %v", err)
	}
	if bv[0] != 1<<2 {
		t.Errorf("the bitmap is %08b, want only word 2 set", bv[0])
	}
}
