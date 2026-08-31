// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"fmt"
	"testing"

	"golang.design/x/nanogo/ir"
)

// The function's own boundary, which is a different question from a call's.
//
// [ABIWalk] takes a list of types and places them, and abi_test.go covers
// every clause of that walk. This file covers the list: which types
// [AssignABI] hands it for the function being compiled, and with which
// register sets.

// abiFuncWithParams builds a function whose parameters are the given types, in
// declaration order, each read out of an OpArg the way construction writes
// one.
//
// It is the shape specs/021-ssa-construction.md hands the ABI pass: one OpArg
// per declared parameter, in the entry block, before the pass runs.
func abiFuncWithParams(results []*ir.Type, types ...*ir.Type) *Func {
	f := NewFunc("f")
	f.Type = &ir.Type{Kind: ir.FuncKind, Name: "f", Params: types, Results: results}
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	for i, t := range types {
		o := &ir.Object{Name: fmt.Sprintf("p%d", i), Type: t, Class: ir.ClassLocal}
		f.Params = append(f.Params, o)
		f.Frame = append(f.Frame, o)
		arg := b.NewValue(0, OpArg, t)
		arg.Aux = o
		addr := b.NewValue(0, OpLocalAddr, abiPtrTo(t), mem)
		addr.Aux = o
		st := b.NewValue(0, OpStore, MemType, addr, arg, mem)
		st.AuxInt = t.Size
		mem = st
	}
	res := []*Value{mem}
	for _, t := range results {
		res = append(res, b.NewValue(0, OpConstInt, t))
	}
	b.Control = b.NewValue(0, OpMakeResult, MemType, res...)
	return f
}

// TestAssignABIPlacesTheFunctionsOwnZeroSizeParameter is the callee half of
// the zero-size rule.
//
// specs/030-abi.md's rule 2 lives in abiAssigner.place, which acts on the list
// this pass hands the walk. Which list the pass builds is not the same
// question. A zero-size parameter reaches the walk only if it is in that list,
// and the list is built from the parameters the graph still carries an OpArg
// for rather than from the declaration.
//
// The numbers are gc's, read out of go tool compile -S over
// func(a [3]int8, b [0]int64, c [3]int8) int8. Its ABIInternal TEXT line is
// $0-16, so the area is 16, and the ABI0 wrapper gc writes for it is $32-24
// and loads a from 0, loads c from 8 and stores the result at 16.
//
// Dropping the zero-size parameter puts c at 3 and the result at 8 in both
// conventions, and a caller compiled by gc then writes and reads different
// words from the ones the callee touches. Nothing reports it.
func TestAssignABIPlacesTheFunctionsOwnZeroSizeParameter(t *testing.T) {
	tg := NewArm64Target()
	a3 := abiArray("own3a", 3, abiTI8)
	z := abiArray("own0", 0, abiTInt)
	c3 := abiArray("own3c", 3, abiTI8)

	f := abiFuncWithParams([]*ir.Type{abiTI8}, a3, z, c3)
	Decompose(f)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if len(f.ABI.In) != 3 {
		t.Fatalf("%d parameters are placed, want 3: the zero-size one is not in the walk", len(f.ABI.In))
	}
	for i, want := range []int64{0, 8, 8} {
		if got := f.ABI.In[i].Off; got != want {
			t.Errorf("parameter %d is at %d, want %d", i, got, want)
		}
	}
	if f.ABI.ArgsSize != 16 {
		t.Errorf("the ABIInternal area is %d bytes, want gc's 16", f.ABI.ArgsSize)
	}

	// The same signature under ABI0, which is the convention that reached the
	// rule. Here the result has an offset of its own, because ABI0 has no
	// register to return it in, and 16 is the word gc's own wrapper stores it
	// into.
	g := abiFuncWithParams([]*ir.Type{abiTI8}, a3, z, c3)
	g.ABI0 = true
	Decompose(g)
	if err := AssignABI(g, tg); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{0, 8, 8} {
		if got := g.ABI.In[i].Off; got != want {
			t.Errorf("ABI0 parameter %d is at %d, want %d", i, got, want)
		}
	}
	if len(g.ABI.Out) != 1 || g.ABI.Out[0].Off != 16 {
		t.Errorf("the ABI0 result is at %v, want one result at 16", g.ABI.Out)
	}
	if g.ABI.ArgsSize != 24 {
		t.Errorf("the ABI0 area is %d bytes, want gc's 24", g.ABI.ArgsSize)
	}
}

// TestAssignABIPlacesAnABI0FunctionsOwnBoundary is the ABI0 wrapper's own
// placement of specs/047-abi-wrappers.md stage 3.
//
// The wrapper's convention is the target's with both register sets emptied, so
// every parameter and every result is in the incoming area and none is in a
// register. The offsets are gc's, read out of the ABI0 wrapper it writes for
// func(p *int, s string) (*int, string): p at 0, the string at 8 and 16, the
// pointer result at 24 and the string result at 32 and 40, so the area is 48.
func TestAssignABIPlacesAnABI0FunctionsOwnBoundary(t *testing.T) {
	tg := NewArm64Target()
	f := abiFuncWithParams([]*ir.Type{abiTPtr, abiTStr}, abiTPtr, abiTStr)
	f.ABI0 = true
	Decompose(f)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{0, 8} {
		if got := f.ABI.In[i].Off; got != want {
			t.Errorf("parameter %d is at %d, want %d", i, got, want)
		}
		if f.ABI.In[i].InReg {
			t.Errorf("parameter %d travels in a register, and ABI0 has none", i)
		}
	}
	for i, want := range []int64{24, 32} {
		if got := f.ABI.Out[i].Off; got != want {
			t.Errorf("result %d is at %d, want %d", i, got, want)
		}
	}
	if f.ABI.ArgsSize != 48 {
		t.Errorf("the ABI0 area is %d bytes, want 48", f.ABI.ArgsSize)
	}

	// The same signature under ABIInternal takes registers, so the two are not
	// one placement under two names.
	g := abiFuncWithParams([]*ir.Type{abiTPtr, abiTStr}, abiTPtr, abiTStr)
	Decompose(g)
	if err := AssignABI(g, tg); err != nil {
		t.Fatal(err)
	}
	if !g.ABI.In[0].InReg {
		t.Error("the ABIInternal placement of the same signature puts the pointer in memory")
	}
}
