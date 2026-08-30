// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"fmt"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// The shapes specs/030-abi.md's walk has to get right, and the two it is easy
// to get wrong: a value that just fits against one that just does not, and the
// argument area's two parts.

var (
	abiTInt   = &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}
	abiTI8    = &ir.Type{Kind: ir.Int8, Size: 1, Align: 1, Name: "int8"}
	abiTI32   = &ir.Type{Kind: ir.Int32, Size: 4, Align: 4, Name: "int32"}
	abiTF64   = &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}
	abiTPtr   = &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, PtrBits: []byte{1}, Name: "*int"}
	abiTStr   = &ir.Type{Kind: ir.String, Size: 16, Align: 8, PtrBits: []byte{1}, Name: "string"}
	abiTSlice = &ir.Type{Kind: ir.Slice, Size: 24, Align: 8, PtrBits: []byte{1}, Elem: abiTInt, Name: "[]int"}
	abiTIface = &ir.Type{Kind: ir.Interface, Size: 16, Align: 8, PtrBits: []byte{3}, Name: "any"}
	abiTEmpty = &ir.Type{Kind: ir.Struct, Size: 0, Align: 1, Name: "struct{}"}
	abiTC128  = &ir.Type{Kind: ir.Complex128, Size: 16, Align: 8, Name: "complex128"}
)

// abiStruct returns a struct of n fields of t, laid out the way ir.Layout
// lays one out.
func abiStruct(name string, n int, t *ir.Type) *ir.Type {
	s := &ir.Type{Kind: ir.Struct, Align: t.Align, Name: name}
	off := int64(0)
	for i := 0; i < n; i++ {
		if t.Align > 0 {
			off = abiRoundUp(off, t.Align)
		}
		s.Fields = append(s.Fields, ir.Field{Name: "f", Type: t, Offset: off})
		off += t.Size
	}
	s.Size = abiRoundUp(off, s.Align)
	return s
}

func abiArray(name string, n int64, t *ir.Type) *ir.Type {
	return &ir.Type{Kind: ir.Array, Size: n * t.Size, Align: t.Align, Elem: t, Len: n, Name: name}
}

// TestABILeavesMatchesDecomposition is the join this pass depends on.
//
// Every part of an argument carries the byte offset the decomposition pass
// gave it, and the assignment finds a part by that offset. The two walks are
// written twice, in two files, so this asserts that they produce the same
// offsets and the same widths for the types the language builds.
func TestABILeavesMatchesDecomposition(t *testing.T) {
	types := []*ir.Type{
		abiTInt, abiTStr, abiTSlice, abiTIface, abiTC128,
		abiStruct("s2", 2, abiTInt),
		abiStruct("s4", 4, abiTInt),
		abiArray("a3", 3, abiTI32),
		abiStruct("mixed", 2, abiTStr),
	}
	// The offsets are the same whether or not the convention gives the value
	// registers, because they describe the words and not the placement. A
	// three-element array is the one here the convention refuses.
	for _, typ := range types {
		want := decomposeOffsets(t, typ)
		parts, _ := ABILeaves(typ)
		if len(parts) != len(want) {
			t.Errorf("%v: the assignment walk has %d parts and decomposition made %d", typ, len(parts), len(want))
			continue
		}
		for i := range parts {
			if parts[i].Off != want[i].off || parts[i].Type.Size != want[i].size {
				t.Errorf("%v: part %d is %d bytes at %d and decomposition put %d bytes at %d",
					typ, i, parts[i].Type.Size, parts[i].Off, want[i].size, want[i].off)
			}
		}
	}
}

type abiOffSize struct{ off, size int64 }

// decomposeOffsets runs the decomposition pass over a function with one
// argument of type typ and returns the offsets and widths of the parts it
// made. Only a type at or below MaxDecomposeParts is split, which is why the
// table above stops there.
func decomposeOffsets(t *testing.T, typ *ir.Type) []abiOffSize {
	t.Helper()
	f, o := abiFuncWithArg(typ)
	Decompose(f)
	var out []abiOffSize
	for _, v := range f.Entry.Values {
		if v.Op != OpArg || v.Aux != any(o) {
			continue
		}
		out = append(out, abiOffSize{v.AuxInt, v.Type.Size})
	}
	return out
}

// abiFuncWithArg builds the shape specs/021-ssa-construction.md gives an
// aggregate parameter: the incoming value stored into a frame slot of its own.
func abiFuncWithArg(typ *ir.Type) (*Func, *ir.Object) {
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	o := &ir.Object{Name: "p", Type: typ, Class: ir.ClassLocal}
	f.Frame = append(f.Frame, o)
	mem := b.NewValue(0, OpInitMem, MemType)
	arg := b.NewValue(0, OpArg, typ)
	arg.Aux = o
	addr := b.NewValue(0, OpLocalAddr, abiPtrTo(typ), mem)
	addr.Aux = o
	st := b.NewValue(0, OpStore, MemType, addr, arg, mem)
	st.AuxInt = typ.Size
	b.Control = b.NewValue(0, OpMakeResult, MemType, st)
	return f, o
}

// TestABIWalkPlacesTheSpecShapes covers each rule of specs/030-abi.md's walk.
func TestABIWalkPlacesTheSpecShapes(t *testing.T) {
	tg := NewArm64Target()
	r := func(i int) Reg { return tg.ArgRegs[ClassInt][i] }

	tests := []struct {
		name  string
		types []*ir.Type
		// regs is one entry per value: the registers its parts take, or nil
		// when the value travels in the argument area.
		regs [][]Reg
		offs []int64
		size int64
	}{
		{"nothing", nil, nil, nil, 0},
		{"one scalar", []*ir.Type{abiTInt}, [][]Reg{{r(0)}}, []int64{0}, 8},
		{"three scalars", []*ir.Type{abiTInt, abiTInt, abiTInt},
			[][]Reg{{r(0)}, {r(1)}, {r(2)}}, []int64{0, 8, 16}, 24},
		// The area is packed by alignment and not by register, which is what
		// makes the spill space the same words gc reserves.
		{"packed", []*ir.Type{abiTI8, abiTI32},
			[][]Reg{{r(0)}, {r(1)}}, []int64{0, 4}, 8},
		// A struct is decomposed and each field takes a register of its own.
		{"a struct in registers", []*ir.Type{abiStruct("s5", 5, abiTInt)},
			[][]Reg{{r(0), r(1), r(2), r(3), r(4)}}, []int64{0}, 40},
		// A string is a pointer and a length, an interface is two pointers.
		{"a string and an interface", []*ir.Type{abiTStr, abiTIface},
			[][]Reg{{r(0), r(1)}, {r(2), r(3)}}, []int64{0, 16}, 32},
		// A value of no width takes no register and no word.
		{"nothing at all", []*ir.Type{abiTEmpty, abiTInt},
			[][]Reg{{}, {r(0)}}, []int64{0, 0}, 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, size, err := ABIArgs(tg, tc.types)
			if err != nil {
				t.Fatal(err)
			}
			if size != tc.size {
				t.Errorf("the area is %d bytes, want %d", size, tc.size)
			}
			for i := range tc.types {
				checkValue(t, i, &got[i], tc.regs[i], tc.offs[i])
			}
		})
	}
}

func checkValue(t *testing.T, i int, av *ABIValue, regs []Reg, off int64) {
	t.Helper()
	if av.Off != off {
		t.Errorf("value %d is at %d, want %d", i, av.Off, off)
	}
	if len(av.Parts) != len(regs) {
		t.Errorf("value %d has %d parts, want %d", i, len(av.Parts), len(regs))
		return
	}
	if want := len(regs) > 0 || av.Type.Size == 0; av.InReg != want {
		t.Errorf("value %d travels in registers (%v), want %v", i, av.InReg, want)
	}
	for j, want := range regs {
		if av.Parts[j].Reg != want {
			t.Errorf("value %d part %d is in %d, want %d", i, j, av.Parts[j].Reg, want)
		}
	}
}

// TestABIAllOrNothing is rule 3 of specs/030-abi.md.
//
// A value with one part more than the registers that are left takes none of
// them, and the registers it did not take stay available for the value after
// it. Placing its first parts would be a caller and a callee that disagree by
// one word.
func TestABIAllOrNothing(t *testing.T) {
	tg := NewArm64Target()
	n := len(tg.ArgRegs[ClassInt])

	// Fourteen scalars, then a three-word slice, then a scalar. Two registers
	// are left and the slice needs three, so the slice goes to the area and
	// the scalar after it takes one of the two.
	types := make([]*ir.Type, 0, n)
	for i := 0; i < n-2; i++ {
		types = append(types, abiTInt)
	}
	types = append(types, abiTSlice, abiTInt)
	got, size, err := ABIArgs(tg, types)
	if err != nil {
		t.Fatal(err)
	}
	slice, last := &got[n-2], &got[n-1]
	if slice.InReg {
		t.Errorf("a three-word value took the two registers that were left")
	}
	if !last.InReg || last.Parts[0].Reg != tg.ArgRegs[ClassInt][n-2] {
		t.Errorf("the scalar after it is not in the next free register")
	}
	// The slice is the only stack value, so it is at offset zero and the
	// spill area for the fifteen register values follows it.
	if slice.Off != 0 {
		t.Errorf("the stack value is at %d, want 0", slice.Off)
	}
	if got[0].Off != 24 {
		t.Errorf("the first spill slot is at %d, and the stack part is 24 bytes", got[0].Off)
	}
	if want := int64(24 + (n-1)*8); size != want {
		t.Errorf("the area is %d bytes, want %d", size, want)
	}
}

// TestABIJustFitsAndJustDoesNot is the boundary the all-or-nothing rule turns
// on.
func TestABIJustFitsAndJustDoesNot(t *testing.T) {
	tg := NewArm64Target()
	n := len(tg.ArgRegs[ClassInt])

	fits, _, err := ABIArgs(tg, []*ir.Type{abiStruct("fits", n, abiTInt)})
	if err != nil {
		t.Fatal(err)
	}
	if !fits[0].InReg {
		t.Errorf("a struct of exactly %d words did not fit %d registers", n, n)
	}
	over, size, err := ABIArgs(tg, []*ir.Type{abiStruct("over", n+1, abiTInt)})
	if err != nil {
		t.Fatal(err)
	}
	if over[0].InReg {
		t.Errorf("a struct of %d words fitted %d registers", n+1, n)
	}
	if over[0].Off != 0 || size != int64(n+1)*8 {
		t.Errorf("the struct is at %d in an area of %d bytes", over[0].Off, size)
	}

	// A value larger than any register file at all stops the walk at the
	// bound rather than costing one entry per element.
	huge, _, err := ABIArgs(tg, []*ir.Type{abiArray("huge", 100000, abiTInt)})
	if err != nil {
		t.Fatal(err)
	}
	if huge[0].InReg {
		t.Error("an array of a hundred thousand words was given registers")
	}
	if len(huge[0].Parts) != abiMaxParts {
		t.Errorf("the walk collected %d parts, and the bound is %d", len(huge[0].Parts), abiMaxParts)
	}
}

// TestABIResultsRestartTheCounters is rule 4.
func TestABIResultsRestartTheCounters(t *testing.T) {
	tg := NewArm64Target()
	f, _ := abiFuncWithArg(abiTInt)
	// f returns two integers, and the arguments took two registers, so the
	// results are in R0 and R1 and not in R2 and R3.
	b := f.Entry
	a1 := b.NewValue(0, OpArg, abiTInt)
	a1.Aux = &ir.Object{Name: "q", Type: abiTInt, Class: ir.ClassLocal}
	mem := b.Control.Args[0]
	b.Control = b.NewValue(0, OpMakeResult, MemType, a1, a1, mem)

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	abi := f.ABI
	if len(abi.Out) != 2 {
		t.Fatalf("%d results, want 2", len(abi.Out))
	}
	for i := range abi.Out {
		if !abi.Out[i].InReg || abi.Out[i].Parts[0].Reg != tg.ResultRegs[ClassInt][i] {
			t.Errorf("result %d is in %v, want %v", i, abi.Out[i].Parts[0].Reg, tg.ResultRegs[ClassInt][i])
		}
		// A result is not spilled: the function is re-executed from its first
		// instruction and has produced none yet.
		if abi.Out[i].Off != -1 {
			t.Errorf("result %d has a slot at %d, and a result is never spilled", i, abi.Out[i].Off)
		}
	}
	// The arguments took R0 and R1 as well, so the two walks really are
	// separate and not one counter.
	if len(abi.In) != 2 || abi.In[0].Parts[0].Reg != tg.ArgRegs[ClassInt][0] {
		t.Errorf("the arguments do not start at the first argument register")
	}
}

// TestABISpillSpaceIsReservedForEveryArgument is the reason the area exists.
//
// runtime.morestack re-executes the function from its first instruction, so
// the stack-growth tail saves the argument registers into the area and reloads
// them. A register argument with no slot would be lost.
func TestABISpillSpaceIsReservedForEveryArgument(t *testing.T) {
	tg := NewArm64Target()
	got, size, err := ABIArgs(tg, []*ir.Type{abiTInt, abiTStr, abiTInt})
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		if !got[i].InReg {
			t.Fatalf("value %d did not take registers", i)
		}
	}
	if got[0].Off != 0 || got[1].Off != 8 || got[2].Off != 24 {
		t.Errorf("the slots are at %d, %d and %d, want 0, 8 and 24", got[0].Off, got[1].Off, got[2].Off)
	}
	if size != 32 {
		t.Errorf("the area is %d bytes and holds three arguments of 8, 16 and 8", size)
	}
}

// TestABIFloatsTakeTheirOwnRegisters checks that the two classes are counted
// apart, which is what makes a mixture agree with gc.
func TestABIFloatsTakeTheirOwnRegisters(t *testing.T) {
	tg := NewArm64Target()
	got, _, err := ABIArgs(tg, []*ir.Type{abiTF64, abiTInt, abiTF64, abiTC128})
	if err != nil {
		t.Fatal(err)
	}
	want := []Reg{
		tg.ArgRegs[ClassFloat][0],
		tg.ArgRegs[ClassInt][0],
		tg.ArgRegs[ClassFloat][1],
		tg.ArgRegs[ClassFloat][2],
	}
	for i, w := range want {
		if !got[i].InReg || got[i].Parts[0].Reg != w {
			t.Errorf("value %d is in %v, want %v", i, got[i].Parts[0].Reg, w)
		}
	}
	// A complex128 is two floats, so it takes the third and fourth.
	if len(got[3].Parts) != 2 || got[3].Parts[1].Reg != tg.ArgRegs[ClassFloat][3] {
		t.Errorf("a complex128 took %v", got[3].Parts)
	}
}

// TestAssignABISplitsARegisterArgument is the rewrite that closes the gap.
//
// specs/025-lowering-and-rules.md stops decomposing at MaxDecomposeParts, and
// the convention passes a five-field struct in five registers. This pass
// finishes the split, and the store into the parameter's frame slot becomes
// one store per register.
func TestAssignABISplitsARegisterArgument(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s5", 5, abiTInt)
	f, o := abiFuncWithArg(typ)
	Decompose(f)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	var args, stores int
	for _, v := range f.Entry.Values {
		switch v.Op {
		case OpArg:
			args++
			if Multiword(v.Type) {
				t.Errorf("v%d is still %v, which does not fit one register", v.ID, v.Type)
			}
		case OpStore:
			stores++
		}
	}
	if args != 5 || stores != 5 {
		t.Errorf("the split made %d arguments and %d stores, want five of each\n%s", args, stores, f)
	}
	// The object keeps its frame slot, because the value really does arrive in
	// registers and has to be written somewhere.
	if len(f.Frame) != 1 || f.Frame[0] != o {
		t.Errorf("the parameter left the frame, and it arrived in registers")
	}
	// Every part is pre-coloured, in order.
	for _, v := range f.Entry.Values {
		if v.Op != OpArg {
			continue
		}
		r, ok := tg.DefReg(v)
		if !ok {
			t.Fatalf("v%d has no fixed register", v.ID)
		}
		if want := tg.ArgRegs[ClassInt][v.AuxInt/8]; r != want {
			t.Errorf("the part at %d is fixed to %v, want %v", v.AuxInt, r, want)
		}
	}
}

// TestAssignABIHomesALargeArgumentInTheArea is the answer to where the address
// of a value too large for the register set comes from.
//
// It comes from the parameter's own frame address, which the graph already
// holds: the object's storage becomes the incoming argument area, the copy the
// entry block made writes the value over itself and goes, and the object
// leaves the locals so that the layout does not give it a second slot.
func TestAssignABIHomesALargeArgumentInTheArea(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s20", 20, abiTInt)
	f, o := abiFuncWithArg(typ)
	Decompose(f)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	for _, v := range f.Entry.Values {
		if v.Op == OpArg || v.Op == OpStore {
			t.Errorf("v%d survived, and the copy writes the value over itself\n%s", v.ID, f)
		}
	}
	if len(f.Frame) != 0 {
		t.Errorf("the parameter is still a local, and its storage is the argument area")
	}
	off, ok := f.ABI.ArgHome(o)
	if !ok || off != 0 {
		t.Errorf("the parameter is homed at %d (%v), want 0", off, ok)
	}
	if f.ABI.ArgsSize != 160 {
		t.Errorf("the area is %d bytes and the parameter is 160", f.ABI.ArgsSize)
	}
	// The address the graph already had still names the parameter, so the
	// callee loads from where the caller wrote.
	found := false
	for _, v := range f.Entry.Values {
		if v.Op == OpLocalAddr && v.Aux == any(o) {
			found = true
		}
	}
	if !found {
		t.Error("the address of the parameter is gone, and the body reads it through that address")
	}
}

// TestAssignABIWritesALargeResultIntoTheResultArea is the callee's half of a
// result the register set cannot hold.
//
// Go's convention puts it in the incoming argument area after the arguments
// the registers could not hold, so the callee writes it there and returns
// nothing in a register.
func TestAssignABIWritesALargeResultIntoTheResultArea(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s20", 20, abiTInt)
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	o := &ir.Object{Name: "p", Type: abiTInt, Class: ir.ClassLocal}
	f.Frame = append(f.Frame, o)
	mem := b.NewValue(0, OpInitMem, MemType)
	arg := b.NewValue(0, OpArg, abiTInt)
	arg.Aux = o
	addr := b.NewValue(0, OpLocalAddr, abiPtrTo(abiTInt), mem)
	addr.Aux = o
	st := b.NewValue(0, OpStore, MemType, addr, arg, mem)
	st.AuxInt = abiTInt.Size
	src := b.NewValue(0, OpLocalAddr, abiPtrTo(typ), st)
	src.Aux = &ir.Object{Name: "v", Type: typ, Class: ir.ClassLocal}
	big := b.NewValue(0, OpLoad, typ, src, st)
	b.Control = b.NewValue(0, OpMakeResult, MemType, big, st)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	abi := f.ABI
	if len(abi.Out) != 1 || abi.Out[0].InReg {
		t.Fatalf("the result is %v and travels in registers (%v)", abi.Out[0].Type, abi.Out[0].InReg)
	}
	// The parameter takes one register and one spill slot, so the stack part
	// of the area is the result alone and the spill slot follows it.
	if abi.Out[0].Off != 0 {
		t.Errorf("the result is at %d, and it is the only value the registers could not hold", abi.Out[0].Off)
	}
	if av, _ := abi.ArgOf(o); av.Off != 160 {
		t.Errorf("the parameter's spill slot is at %d, over a result area of 160 bytes", av.Off)
	}
	if abi.ArgsSize != 168 {
		t.Errorf("the area is %d bytes: 160 of result and 8 of spill", abi.ArgsSize)
	}
	// The return passes nothing: the value is in the caller's frame.
	mr := b.Control
	if len(mr.Args) != 1 || !IsMemory(mr.Args[0]) {
		t.Errorf("the return passes %d operands, and the result is in memory\n%s", len(mr.Args), f)
	}
	// The copy is a block move into a slot the assignment named.
	var move *Value
	for _, v := range b.Values {
		if v.Op == OpMove {
			move = v
		}
	}
	if move == nil {
		t.Fatalf("no block move writes the result\n%s", f)
	}
	if move.AuxInt != typ.Size {
		t.Errorf("the move copies %d bytes and the result is %d", move.AuxInt, typ.Size)
	}
	dst := move.Args[0]
	if dst.Op != OpLocalAddr {
		t.Fatalf("the destination of the move is %v", dst.Op)
	}
	ho, _ := dst.Aux.(*ir.Object)
	found := false
	for _, h := range abi.Homes {
		if h.Obj == ho {
			found = true
			if !h.Incoming || h.Off != 0 {
				t.Errorf("the result slot is at %d, incoming %v", h.Off, h.Incoming)
			}
		}
	}
	if !found {
		t.Errorf("the destination of the move names no argument-area slot")
	}
	// The slot is not a local: the frame layout would give it a second one
	// and the locals bitmap would describe it twice.
	for _, fo := range f.Frame {
		if fo == ho {
			t.Errorf("the result slot is in the frame")
		}
	}
}

// TestAssignABIKeepsTheMemoryChainWhenAReturnBothCopiesAndSplits is the
// invariant a pass may not break.
//
// A return of two results, one the registers hold and one they do not, makes
// this pass do both of its rewrites ahead of the same value. rewriteResults
// copies the second into the result area, which puts a newer memory live at
// the return, and splitOperands then splits the first into one load per
// register. A load that reads the memory the load it replaces read is two
// memory values live at one point, which is InvMemChain, and below the
// verifier it is a load a later pass may move across a store.
func TestAssignABIKeepsTheMemoryChainWhenAReturnBothCopiesAndSplits(t *testing.T) {
	tg := NewArm64Target()
	small := abiStruct("s5", 5, abiTInt)
	large := abiStruct("s20", 20, abiTInt)
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)

	load := func(typ *ir.Type, name string) *Value {
		o := &ir.Object{Name: name, Type: typ, Class: ir.ClassLocal}
		f.Frame = append(f.Frame, o)
		addr := b.NewValue(0, OpLocalAddr, abiPtrTo(typ), mem)
		addr.Aux = o
		return b.NewValue(0, OpLoad, typ, addr, mem)
	}
	b.Control = b.NewValue(0, OpMakeResult, MemType, load(small, "a"), load(large, "c"), mem)

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	// The shape the invariant is about: the copy of the large result is
	// ahead of the loads of the small one, so the loads read what it wrote.
	var move *Value
	for _, v := range b.Values {
		switch {
		case v.Op == OpMove:
			move = v
		case v.Op == OpLoad && !Multiword(v.Type):
			if move == nil {
				t.Fatalf("v%d loads a part before the copy of the large result\n%s", v.ID, f)
			}
			if v.Args[1] != move {
				t.Errorf("v%d reads memory v%d, and v%d is live\n%s", v.ID, v.Args[1].ID, move.ID, f)
			}
		}
	}
	if move == nil {
		t.Fatalf("the large result was not copied into the result area\n%s", f)
	}
}

// TestAssignABIRejectsNoTarget covers the guards.
func TestAssignABIRejectsNoTarget(t *testing.T) {
	if err := AssignABI(nil, NewArm64Target()); err == nil {
		t.Error("a nil function was assigned")
	}
	f, _ := abiFuncWithArg(abiTInt)
	if err := AssignABI(f, nil); err == nil {
		t.Error("a nil target was accepted")
	}
	if _, _, err := ABIArgs(nil, nil); err == nil {
		t.Error("a nil target placed an argument list")
	}
	if _, _, err := ABIResults(nil, 0, nil); err == nil {
		t.Error("a nil target placed a result list")
	}
	// A value with no type has no placement, and guessing one would be a
	// wrong offset in the argument area.
	if _, _, err := ABIArgs(NewArm64Target(), []*ir.Type{nil}); err == nil {
		t.Error("a value with no type was placed")
	}
}

// TestABILookupsAreDefensive covers the readers a caller can reach with a
// value the assignment never saw.
func TestABILookupsAreDefensive(t *testing.T) {
	tg := NewArm64Target()
	f, o := abiFuncWithArg(abiTInt)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	var nilABI *ABI
	if _, ok := nilABI.ArgOf(o); ok {
		t.Error("a nil assignment found a parameter")
	}
	if _, ok := nilABI.ArgReg(nil); ok {
		t.Error("a nil assignment fixed a register")
	}
	if _, ok := f.ABI.ArgOf(nil); ok {
		t.Error("a parameter with no object was found")
	}
	if _, ok := f.ABI.ArgHome(o); ok {
		t.Error("an argument that travels in a register is homed in the area")
	}
	other := &ir.Object{Name: "z", Type: abiTInt}
	if _, ok := f.ABI.ArgOf(other); ok {
		t.Error("an object that is not a parameter was found")
	}
	// An argument whose offset names no part of its parameter.
	stray := f.Entry.NewValue(0, OpArg, abiTInt)
	stray.Aux = o
	stray.AuxInt = 4096
	if _, ok := f.ABI.ArgReg(stray); ok {
		t.Error("an argument at an offset the parameter does not have was placed")
	}
	if _, ok := tg.DefReg(f.Entry.NewValue(0, OpConstInt, abiTInt)); ok {
		t.Error("a constant was pre-coloured")
	}
	if _, ok := tg.DefReg(nil); ok {
		t.Error("a nil value was pre-coloured")
	}
}

// TestABIPlacesACallsOperands covers the placement both the allocator and the
// code generator read, and the operands in front of an indirect call that are
// not arguments.
func TestABIPlacesACallsOperands(t *testing.T) {
	tg := NewArm64Target()
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	x := b.NewValue(0, OpArg, abiTInt)
	y := b.NewValue(0, OpArg, abiTInt)

	call := b.NewValue(0, OpARM64CALLstatic, MemType, x, y, mem)
	vals, lo, size, err := ABICallArgs(tg, call)
	if err != nil {
		t.Fatal(err)
	}
	if lo != 0 || len(vals) != 2 || size != 16 {
		t.Fatalf("a static call with two operands gave lo=%d, %d values and %d bytes", lo, len(vals), size)
	}
	for i := range vals {
		if r, ok := tg.UseReg(call, i); !ok || r != tg.ArgRegs[ClassInt][i] {
			t.Errorf("operand %d is read from %v (%v), want %v", i, r, ok, tg.ArgRegs[ClassInt][i])
		}
	}
	// The memory operand is not an argument and takes no register.
	if _, ok := tg.UseReg(call, 2); ok {
		t.Error("the memory operand of a call was given a register")
	}
	if _, ok := tg.UseReg(call, 7); ok {
		t.Error("an operand past the end of the list was given a register")
	}

	// An indirect call carries the entry point and the closure in front of
	// its arguments, and neither is one.
	entry := b.NewValue(0, OpARM64MOVDload, abiTPtr, mem)
	clo := b.NewValue(0, OpArg, abiTPtr)
	ind := b.NewValue(0, OpARM64CALLclosure, MemType, entry, clo, x, mem)
	vals, lo, _, err = ABICallArgs(tg, ind)
	if err != nil {
		t.Fatal(err)
	}
	if lo != 2 || len(vals) != 1 {
		t.Fatalf("an indirect call with one argument gave lo=%d and %d values", lo, len(vals))
	}
	if r, ok := tg.UseReg(ind, 2); !ok || r != tg.ArgRegs[ClassInt][0] {
		t.Errorf("the one argument of an indirect call is read from %v (%v)", r, ok)
	}
	// The entry point is not an argument and the convention still fixes where
	// it is read from. It is the first scratch register: no argument uses one
	// and no value has one as a home, so the branch cannot land on a register
	// the arguments have already overwritten.
	er, eok := tg.UseReg(ind, 0)
	if !eok || er != tg.Scratch[ClassInt][0] {
		t.Errorf("the entry point of an indirect call is read from %v (%v), want %v",
			er, eok, tg.Scratch[ClassInt][0])
	}
	for _, ar := range tg.ArgRegs[ClassInt] {
		if er == ar {
			t.Errorf("the entry point of an indirect call is read from %v, which is an argument register", er)
		}
	}

	// A return's values are placed by the result walk.
	ret := b.NewValue(0, OpARM64RET, MemType, x, y, mem)
	for i := 0; i < 2; i++ {
		if r, ok := tg.UseReg(ret, i); !ok || r != tg.ResultRegs[ClassInt][i] {
			t.Errorf("result %d is read from %v (%v), want %v", i, r, ok, tg.ResultRegs[ClassInt][i])
		}
	}
	// Anything else is free.
	if _, ok := tg.UseReg(b.NewValue(0, OpARM64ADD, abiTInt, x, y), 0); ok {
		t.Error("an operand of an addition was fixed to a register")
	}
	if _, ok := tg.UseReg(ret, -1); ok {
		t.Error("a negative operand index was placed")
	}
}

// TestACallsResultsAreNotPreColoured records the one case the convention
// places and the allocator cannot hold.
//
// A call result comes back in a result register, and specs/026's scan spills a
// value that is live across a call before it looks at the register the
// convention fixed. The check that runs ahead of the scan does not, so an
// argument in R0 and a call result in R0 are refused as a pair even when the
// argument is going to be spilled. The code generator moves each result out of
// its register at the call instead.
func TestACallsResultsAreNotPreColoured(t *testing.T) {
	tg := NewArm64Target()
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	call := b.NewValue(0, OpARM64CALLstatic, MemType, mem)
	r0 := b.NewValue(0, OpSelectN, abiTInt, call)
	r1 := b.NewValue(0, OpSelectN, abiTPtr, call)
	r1.AuxInt = 1
	for i, v := range []*Value{r0, r1} {
		if _, ok := tg.DefReg(v); ok {
			t.Errorf("result %d is pre-coloured, and an argument in the same register is refused", i)
		}
	}
	// The placement itself is still the walk, and the code generator reads
	// it: results restart the counters, so they are the first two result
	// registers whatever the arguments took.
	out, _, err := ABIResults(tg, 0, []*ir.Type{abiTInt, abiTPtr})
	if err != nil {
		t.Fatal(err)
	}
	for i := range out {
		if !out[i].InReg || out[i].Parts[0].Reg != tg.ResultRegs[ClassInt][i] {
			t.Errorf("result %d comes back in %v, want %v", i, out[i].Parts[0].Reg, tg.ResultRegs[ClassInt][i])
		}
	}
}

// TestAssignABISplitsALargeCallOperand covers the other side of the boundary:
// a value passed to a call whole because decomposition stopped at its bound.
func TestAssignABISplitsALargeCallOperand(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s5", 5, abiTInt)
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	base := b.NewValue(0, OpArg, abiTPtr)
	base.Aux = &ir.Object{Name: "p", Type: abiTPtr, Class: ir.ClassLocal}
	load := b.NewValue(0, OpLoad, typ, base, mem)
	call := b.NewValue(0, OpStaticCall, MemType, load, mem)
	call.Aux = &ir.Object{Name: "main.g"}
	b.Control = b.NewValue(0, OpMakeResult, MemType, call)

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	if len(call.Args) != 6 {
		t.Fatalf("the call passes %d operands, want five words and memory\n%s", len(call.Args), f)
	}
	for i := 0; i < 5; i++ {
		if Multiword(call.Args[i].Type) {
			t.Errorf("operand %d is %v, which does not fit one register", i, call.Args[i].Type)
		}
		if r, ok := tg.UseReg(call, i); !ok || r != tg.ArgRegs[ClassInt][i] {
			t.Errorf("operand %d is read from %v (%v)", i, r, ok)
		}
	}
	// The whole load is gone: reading it as well would be a second copy.
	for _, v := range b.Values {
		if v == load {
			t.Error("the whole load survived beside its parts")
		}
	}
}

// abiCallReturning builds the shape specs/021-ssa-construction.md gives an
// aggregate a call returns: the result read once and stored into a frame slot
// of its own.
func abiCallReturning(typs ...*ir.Type) (*Func, *Value) {
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	call := b.NewValue(0, OpStaticCall, MemType, mem)
	call.Aux = &ir.Object{Name: "main.g"}

	// Every result is read before any destination is written, which is the
	// order construction builds and the order the verifier requires: a SelectN
	// reads the memory the call produced.
	sel := make([]*Value, 0, len(typs))
	for i, typ := range typs {
		v := b.NewValue(0, OpSelectN, typ, call)
		v.AuxInt = int64(i)
		sel = append(sel, v)
	}
	m := call
	for i, typ := range typs {
		o := &ir.Object{Name: fmt.Sprintf("v%d", i), Type: typ, Class: ir.ClassLocal}
		f.Frame = append(f.Frame, o)
		addr := b.NewValue(0, OpLocalAddr, abiPtrTo(typ), m)
		addr.Aux = o
		st := b.NewValue(0, OpStore, MemType, addr, sel[i], m)
		st.AuxInt = typ.Size
		m = st
	}
	b.Control = b.NewValue(0, OpMakeResult, MemType, m)
	return f, call
}

// abiResultWords describes a call's results as the code generator reads them:
// one entry per machine word of the result area, in index order.
func abiResultWords(f *Func, call *Value) []*ir.Type {
	var out []*ir.Type
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpSelectN || len(v.Args) == 0 || v.Args[0] != call {
				continue
			}
			for int(v.AuxInt) >= len(out) {
				out = append(out, nil)
			}
			out[v.AuxInt] = v.Type
		}
	}
	return out
}

// TestAssignABISplitsAWideCallResult is the caller's half of a result wider
// than MaxDecomposeParts, and the shape that used to reach lowering as a
// forty-byte store no rule has.
//
// The convention returns a five-field struct in five registers, so the call
// site reads five results and writes five words. The callee's half is
// splitOperands, which turns the same value into five operands of the return.
func TestAssignABISplitsAWideCallResult(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s5", 5, abiTInt)
	f, call := abiCallReturning(typ)
	Decompose(f)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}

	words := abiResultWords(f, call)
	if len(words) != 5 {
		t.Fatalf("the call site reads %d words of the result area, want five\n%s", len(words), f)
	}
	for i, w := range words {
		if w != abiTInt {
			t.Errorf("word %d is %v, want int\n%s", i, w, f)
		}
	}
	// Five stores, each of one word, and the whole read is gone: reading it
	// as well would leave the store no rule lowers.
	if n := abiCountOp(f, OpStore); n != 5 {
		t.Errorf("the split made %d stores, want five\n%s", n, f)
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpStore && v.AuxInt != abiTInt.Size {
				t.Errorf("v%d stores %d bytes, and every word is %d", v.ID, v.AuxInt, abiTInt.Size)
			}
			if v.Op == OpSelectN && Multiword(v.Type) {
				t.Errorf("v%d is still %v, which does not fit one register", v.ID, v.Type)
			}
		}
	}
	// The words come back in the first five result registers, which is where
	// the callee's return leaves them.
	out, _, err := ABIResults(tg, 0, words)
	if err != nil {
		t.Fatal(err)
	}
	for i := range out {
		if !out[i].InReg || out[i].Parts[0].Reg != tg.ResultRegs[ClassInt][i] {
			t.Errorf("word %d comes back in %v, want %v", i, out[i].Parts[0].Reg, tg.ResultRegs[ClassInt][i])
		}
	}
}

// TestAssignABIRenumbersTheResultsAfterAWideOne is why the walk takes a call's
// whole result list or none of it.
//
// The index of a SelectN is the machine word of the result area it reads, and
// decomposition counted a result it left whole as one word. Splitting it into
// five moves every result after it by four, and a slice read at word 1 would
// otherwise read the second word of the struct.
func TestAssignABIRenumbersTheResultsAfterAWideOne(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s5", 5, abiTInt)
	f, call := abiCallReturning(typ, abiTSlice)
	Decompose(f)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}

	words := abiResultWords(f, call)
	want := []*ir.Type{abiTInt, abiTInt, abiTInt, abiTInt, abiTInt, abiTPtr, abiTInt, abiTInt}
	if len(words) != len(want) {
		t.Fatalf("the call site reads %d words, want %d\n%s", len(words), len(want), f)
	}
	for i := range want {
		if words[i] == nil || words[i].Kind != want[i].Kind {
			t.Errorf("word %d is %v, want %v\n%s", i, words[i], want[i], f)
		}
	}
	// The slice starts at word 5, which is where the callee's return leaves
	// it once the struct ahead of it is five operands rather than one.
	if words[5].Kind != ir.Ptr {
		t.Errorf("the slice does not start at word 5\n%s", f)
	}
}

// TestAssignABIReadsAResultTheRegistersCannotHold covers the caller's half of
// a result that travels in the argument area.
//
// gc puts a result the sixteen result registers cannot hold in the incoming
// argument area of the callee, after the arguments the registers could not
// hold. rewriteResults writes it there and readFromArea reads it back, so the
// store the call site made becomes a block move out of a slot of the outgoing
// area. The SelectN stays with no reader, because it still names one place of
// the call's result list and the code generator counts that list by index.
func TestAssignABIReadsAResultTheRegistersCannotHold(t *testing.T) {
	tg := NewArm64Target()
	f, call := abiCallReturning(abiStruct("s20", 20, abiTInt))
	Decompose(f)
	if err := AssignABI(f, tg); err != nil {
		t.Fatalf("a twenty-word result was refused at the call site: %v\n%s", err, f)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}

	// The store became a move of the whole value out of the area.
	var mv *Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpMove {
				mv = v
			}
			if v.Op == OpStore {
				t.Errorf("a store of the whole result is left in the graph\n%s", f)
			}
		}
	}
	if mv == nil || mv.AuxInt != 160 {
		t.Fatalf("the call site does not move the 160-byte result out of the area\n%s", f)
	}
	src := mv.Args[1]
	if src.Op != OpLocalAddr {
		t.Fatalf("the move reads %v and not an address in the area\n%s", src.Op, f)
	}
	o, _ := src.Aux.(*ir.Object)
	if o == nil {
		t.Fatalf("the move reads an address of no object\n%s", f)
	}
	var home *ABIHome
	for i := range f.ABI.Homes {
		if f.ABI.Homes[i].Obj == o {
			home = &f.ABI.Homes[i]
		}
	}
	if home == nil {
		t.Fatalf("the slot the move reads is not named in the ABI\n%s", f)
	}
	if home.Incoming {
		t.Error("the call site reads its result out of its own incoming area")
	}
	if home.Off != 0 {
		t.Errorf("the result is at %d of the outgoing area, want 0", home.Off)
	}
	// The SelectN is still there, because the generator walks the list by
	// index and a missing entry moves every entry after it.
	found := false
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpSelectN && len(v.Args) > 0 && v.Args[0] == call {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the result the area holds is no longer named\n%s", f)
	}
}

// TestAssignABISpillsAFrameResultWithNowhereToPutIt covers a result the
// registers could not hold that the call site hands straight to another call.
//
// The callee wrote it into the call's result area, which is the outgoing
// argument area the second call is about to write over, and nothing in the
// graph names a place to copy it to. The pass makes one: a frame slot, filled
// by a move out of the result area ahead of everything else, and the second
// call copies its outgoing area out of that slot.
func TestAssignABISpillsAFrameResultWithNowhereToPutIt(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s20", 20, abiTInt)
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	call := b.NewValue(0, OpStaticCall, MemType, mem)
	call.Aux = &ir.Object{Name: "main.g"}
	sel := b.NewValue(0, OpSelectN, typ, call)
	// The result is read by a second call rather than written to one place.
	use := b.NewValue(0, OpStaticCall, MemType, sel, call)
	use.Aux = &ir.Object{Name: "main.h"}
	b.Control = b.NewValue(0, OpMakeResult, MemType, use)

	Decompose(f)
	if err := AssignABI(f, tg); err != nil {
		t.Fatalf("a result the registers could not hold was refused: %v", err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	// The slot is a local: it holds the value across the second call, so the
	// collector reads it through the locals bitmap.
	var slot *ir.Object
	for _, o := range f.Frame {
		if o.Type == typ {
			slot = o
		}
	}
	if slot == nil {
		t.Fatalf("the forwarded result got no frame slot\n%s", f)
	}
	// Two moves, in this order: out of the call's result area into the slot,
	// then out of the slot into the second call's outgoing area. The first has
	// to come first, because the second writes the area the first reads.
	var moves []*Value
	for _, v := range b.Values {
		if v.Op == OpMove {
			moves = append(moves, v)
		}
	}
	if len(moves) != 2 {
		t.Fatalf("the function makes %d block moves, want two\n%s", len(moves), f)
	}
	out := f.ABI.Homes
	homeOf := func(v *Value) (ABIHome, bool) {
		o, _ := v.Aux.(*ir.Object)
		for _, h := range out {
			if h.Obj == o {
				return h, true
			}
		}
		return ABIHome{}, false
	}
	if o, _ := moves[0].Args[0].Aux.(*ir.Object); o != slot {
		t.Errorf("the first move does not write the frame slot\n%s", f)
	}
	if h, ok := homeOf(moves[0].Args[1]); !ok || h.Incoming {
		t.Errorf("the first move does not read the call's result area\n%s", f)
	}
	if o, _ := moves[1].Args[1].Aux.(*ir.Object); o != slot {
		t.Errorf("the second move does not read the frame slot\n%s", f)
	}
	if h, ok := homeOf(moves[1].Args[0]); !ok || h.Incoming {
		t.Errorf("the second move does not write the second call's outgoing area\n%s", f)
	}
	// The second call no longer carries the value as an operand: it is in the
	// area, and the record is what says so.
	if c := f.ABI.CallAt(use.ID); c == nil || len(c.Vals) != 1 || !c.Vals[0].Copied {
		t.Errorf("the second call still passes the value as an operand\n%s", f)
	}
}

// TestAssignABISpillsAForwardedResult covers a wide result the call site does
// not write to one place, which is the forwarding return "return g()".
//
// The words come back in five registers and no value of the graph names a
// place to put them, so the pass makes one, writes the result into it, and the
// return reads its five registers back out of it.
func TestAssignABISpillsAForwardedResult(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s5", 5, abiTInt)
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	call := b.NewValue(0, OpStaticCall, MemType, mem)
	call.Aux = &ir.Object{Name: "main.g"}
	sel := b.NewValue(0, OpSelectN, typ, call)
	b.Control = b.NewValue(0, OpMakeResult, MemType, sel, call)

	if err := AssignABI(f, tg); err != nil {
		t.Fatalf("a forwarded wide result was refused: %v", err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	var slot *ir.Object
	for _, o := range f.Frame {
		if o.Type == typ {
			slot = o
		}
	}
	if slot == nil {
		t.Fatalf("the forwarded result got no frame slot\n%s", f)
	}
	// Both halves are one word at a time: five results read out of five
	// registers into the slot, five loads out of the slot into the return.
	var sels, stores, loads int
	for _, v := range b.Values {
		switch v.Op {
		case OpSelectN:
			sels++
			if Multiword(v.Type) {
				t.Errorf("v%d is still %v, which no register holds", v.ID, v.Type)
			}
		case OpStore:
			stores++
		case OpLoad:
			loads++
		}
	}
	if sels != 5 || stores != 5 || loads != 5 {
		t.Errorf("the split made %d results, %d stores and %d loads, want five of each\n%s",
			sels, stores, loads, f)
	}
	mr := b.Control
	if len(mr.Args) != 6 {
		t.Fatalf("the return passes %d operands, want five words and the memory\n%s", len(mr.Args), f)
	}
	for i, arg := range mr.Args[:5] {
		if arg.Op != OpLoad || Multiword(arg.Type) {
			t.Errorf("operand %d of the return is %v <%v>\n%s", i, arg.Op, arg.Type, f)
		}
	}
}

// TestAssignABIRenumbersPastADiscardedWideResult covers "_, err := g()" with a
// first result wider than a register.
//
// The words come back in the registers the convention names whether or not the
// call site wants them, so the result that follows starts at the word after
// the last of them. A discarded result needs no place to be written to, which
// is what separates it from a forwarded one, and it may not be left whole:
// ssagen indexes a call's results by the word each reads and refuses a list
// with a hole in it.
func TestAssignABIRenumbersPastADiscardedWideResult(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s5", 5, abiTInt)
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	call := b.NewValue(0, OpStaticCall, MemType, mem)
	call.Aux = &ir.Object{Name: "main.g"}
	// The wide result is read by nobody, which is what makes it discarded.
	b.NewValue(0, OpSelectN, typ, call)
	tail := b.NewValue(0, OpSelectN, abiTInt, call)
	tail.AuxInt = 1
	b.Control = b.NewValue(0, OpMakeResult, MemType, tail, call)

	if err := AssignABI(f, tg); err != nil {
		t.Fatalf("a discarded wide result was refused: %v", err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	if len(f.Frame) != 0 {
		t.Errorf("a discarded result was given a frame slot, and nothing reads it\n%s", f)
	}
	words := map[int64]*ir.Type{}
	for _, v := range b.Values {
		if v.Op != OpSelectN {
			continue
		}
		if Multiword(v.Type) {
			t.Errorf("v%d is still %v, which no register holds\n%s", v.ID, v.Type, f)
		}
		if _, seen := words[v.AuxInt]; seen {
			t.Errorf("two results read word %d\n%s", v.AuxInt, f)
		}
		words[v.AuxInt] = v.Type
	}
	// Five words of the discarded result, then the one the return passes.
	if len(words) != 6 {
		t.Fatalf("the call reads %d words, want six\n%s", len(words), f)
	}
	if tail.AuxInt != 5 {
		t.Errorf("the second result reads word %d, and the first occupies five\n%s", tail.AuxInt, f)
	}
}

// TestAssignABILeavesANarrowResultListAlone is the other half of the survey.
//
// Decomposition splits every result of up to MaxDecomposeParts already, so the
// pass has nothing to finish and must not renumber a list that is right.
func TestAssignABILeavesANarrowResultListAlone(t *testing.T) {
	tg := NewArm64Target()
	f, call := abiCallReturning(abiTStr, abiTInt)
	Decompose(f)
	before := abiResultWords(f, call)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	after := abiResultWords(f, call)
	if len(before) != 3 || len(after) != len(before) {
		t.Fatalf("the call reads %d words and read %d before\n%s", len(after), len(before), f)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("word %d was %v and is %v", i, before[i], after[i])
		}
	}
}

// TestAssignABIRefusesAWideResultItCannotMeasure covers the lists the walk
// cannot renumber.
//
// The index of a SelectN is the word of the result area it reads, and the word
// a result starts at is the sum of the widths of the results before it. A list
// with a gap in it, one that reads a result twice, or one spread over two
// blocks gives no such sum. The pass refuses rather than guess, but only when
// the list holds a result that would otherwise reach lowering as a store no
// rule has: a list of single-word results is one this pass never touches.
func TestAssignABIRefusesAWideResultItCannotMeasure(t *testing.T) {
	tg := NewArm64Target()
	// An unnamed type, so the refusal has to describe it by its width.
	wide := abiStruct("", 5, abiTInt)

	build := func(shape string) *Func {
		f := NewFunc("f")
		b := f.Entry
		b.Kind = BlockRet
		mem := b.NewValue(0, OpInitMem, MemType)
		call := b.NewValue(0, OpStaticCall, MemType, mem)
		call.Aux = &ir.Object{Name: "main.g"}
		sel := b.NewValue(0, OpSelectN, wide, call)
		read := b
		switch shape {
		case "a result read twice":
			b.NewValue(0, OpSelectN, wide, call)
		case "a result nothing reads":
			sel.AuxInt = 1
		case "a result read in another block":
			read = f.NewBlock(BlockRet)
			b.Kind = BlockPlain
			b.Succs = append(b.Succs, read)
			read.Preds = append(read.Preds, b)
			sel.Block = read
			b.Values = b.Values[:len(b.Values)-1]
			read.Values = append(read.Values, sel)
		}
		o := &ir.Object{Name: "v", Type: wide, Class: ir.ClassLocal}
		f.Frame = append(f.Frame, o)
		addr := read.NewValue(0, OpLocalAddr, abiPtrTo(wide), call)
		addr.Aux = o
		st := read.NewValue(0, OpStore, MemType, addr, sel, call)
		st.AuxInt = wide.Size
		if shape == "a result stored twice" {
			st2 := read.NewValue(0, OpStore, MemType, addr, sel, st)
			st2.AuxInt = wide.Size
			st = st2
		}
		read.Control = read.NewValue(0, OpMakeResult, MemType, st)
		return f
	}

	for _, shape := range []string{
		"a result read twice",
		"a result nothing reads",
		"a result read in another block",
		"a result stored twice",
	} {
		t.Run(shape, func(t *testing.T) {
			f := build(shape)
			err := AssignABI(f, tg)
			if err == nil {
				t.Fatalf("the pass placed a list it cannot measure\n%s", f)
			}
			if !strings.Contains(err.Error(), "a 40-byte value") {
				t.Errorf("the refusal is %q, and it has to name the value", err)
			}
		})
	}
}

// TestAssignABIIgnoresACallItDoesNotTouch is the other half of the survey.
//
// A call whose results are each one word is a call this pass has nothing to
// finish, so a result list it cannot measure is not a fault: the indices are
// decomposition's and they are already right.
func TestAssignABIIgnoresACallItDoesNotTouch(t *testing.T) {
	tg := NewArm64Target()
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	call := b.NewValue(0, OpStaticCall, MemType, mem)
	call.Aux = &ir.Object{Name: "main.g"}
	// One result of two read, which is a list with a gap in it.
	sel := b.NewValue(0, OpSelectN, abiTInt, call)
	sel.AuxInt = 1
	b.Control = b.NewValue(0, OpMakeResult, MemType, sel, call)

	if err := AssignABI(f, tg); err != nil {
		t.Fatalf("a call of single-word results was refused: %v\n%s", err, f)
	}
	if sel.AuxInt != 1 {
		t.Errorf("the result was renumbered to %d, and nothing here moved it", sel.AuxInt)
	}
}

// TestAssignABIKeepsAStoreItDoesNotOwn is the other half of the exact match.
//
// The pass deletes exactly the store that writes an incoming parameter into
// its own frame slot. A store of the argument anywhere else is a store the
// program made, and deleting it would drop a write.
func TestAssignABIKeepsAStoreItDoesNotOwn(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s20", 20, abiTInt)

	// The destination is another object's slot.
	f, _ := abiFuncWithArg(typ)
	other := &ir.Object{Name: "q", Type: typ, Class: ir.ClassLocal}
	for _, v := range f.Entry.Values {
		if v.Op == OpLocalAddr {
			v.Aux = other
		}
	}
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if abiCountOp(f, OpStore) != 1 || abiCountOp(f, OpArg) != 1 {
		t.Errorf("a store into another object's slot was taken for the parameter's own\n%s", f)
	}

	// The argument is read twice, so it is not only the copy into its slot.
	f, _ = abiFuncWithArg(typ)
	var arg, st *Value
	for _, v := range f.Entry.Values {
		switch v.Op {
		case OpArg:
			arg = v
		case OpStore:
			st = v
		}
	}
	addr := f.Entry.NewValue(0, OpLocalAddr, abiPtrTo(typ), st)
	addr.Aux = other
	second := f.Entry.NewValue(0, OpStore, MemType, addr, arg, st)
	second.AuxInt = typ.Size
	f.Entry.Control.SetArg(0, second)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if abiCountOp(f, OpStore) != 2 || abiCountOp(f, OpArg) != 1 {
		t.Errorf("an argument with a second reader was removed\n%s", f)
	}

	// The store writes a different number of bytes than the parameter holds.
	f, _ = abiFuncWithArg(typ)
	for _, v := range f.Entry.Values {
		if v.Op == OpStore {
			v.AuxInt = 8
		}
	}
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if abiCountOp(f, OpStore) != 1 {
		t.Errorf("a store of the wrong width was taken for the parameter's own\n%s", f)
	}
}

func abiCountOp(f *Func, op Op) int {
	n := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == op {
				n++
			}
		}
	}
	return n
}

// TestAssignABINamesAnArgumentThatHasNoParameter covers a function built by
// hand, which the tests of the passes below do: an argument that names no
// parameter is its own parameter.
func TestAssignABINamesAnArgumentThatHasNoParameter(t *testing.T) {
	tg := NewArm64Target()
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	a0 := b.NewValue(0, OpArg, abiTInt)
	a1 := b.NewValue(0, OpArg, abiTInt)
	b.Control = b.NewValue(0, OpMakeResult, MemType, a0, mem)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if len(f.ABI.In) != 2 {
		t.Fatalf("%d parameters, want 2", len(f.ABI.In))
	}
	for i, v := range []*Value{a0, a1} {
		r, ok := tg.DefReg(v)
		if !ok || r != tg.ArgRegs[ClassInt][i] {
			t.Errorf("argument %d is fixed to %v (%v), want %v", i, r, ok, tg.ArgRegs[ClassInt][i])
		}
	}
}

// TestABIFlattensAnArray covers the element walk and the empty element, which
// no built type reaches.
func TestABIFlattensAnArray(t *testing.T) {
	parts, complete := ABILeaves(abiArray("a4", 4, abiTI32))
	if len(parts) != 4 {
		t.Fatalf("a four-element array has %d parts", len(parts))
	}
	if complete {
		t.Error("a four-element array was given registers")
	}
	for i, p := range parts {
		if p.Off != int64(i)*4 || p.Type != abiTI32 {
			t.Errorf("element %d is %v at %d", i, p.Type, p.Off)
		}
	}
	// An array with no element type describes no storage.
	if parts, _ := ABILeaves(&ir.Type{Kind: ir.Array, Len: 3, Size: 24, Align: 8}); len(parts) != 0 {
		t.Errorf("an array of nothing has %d parts", len(parts))
	}
	if parts, _ := ABILeaves(nil); len(parts) != 0 {
		t.Errorf("no type has %d parts", len(parts))
	}
	// A pointer to nothing still points at a byte, so it has a width.
	if p := abiPtrTo(nil); p.Size != ir.PtrSize || p.Elem != abiByte {
		t.Errorf("a pointer to no type is %v", p)
	}
}

// TestAssignABICopiesALargeCallArgumentIntoTheArea is the caller's half of a
// value the register set cannot hold.
//
// The value goes into the outgoing argument area, which nothing names, so the
// pass makes an object for the slot and copies into it through that object's
// address. The value stops being an operand: it is in the area before the call
// runs, and the placement records that so the operands after it still land
// where the callee reads them.
func TestAssignABICopiesALargeCallArgumentIntoTheArea(t *testing.T) {
	tg := NewArm64Target()
	typ := abiStruct("s20", 20, abiTInt)
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	base := b.NewValue(0, OpArg, abiTPtr)
	base.Aux = &ir.Object{Name: "p", Type: abiTPtr, Class: ir.ClassLocal}
	load := b.NewValue(0, OpLoad, typ, base, mem)
	tail := b.NewValue(0, OpArg, abiTInt)
	tail.Aux = &ir.Object{Name: "q", Type: abiTInt, Class: ir.ClassLocal}
	call := b.NewValue(0, OpStaticCall, MemType, load, tail, mem)
	call.Aux = &ir.Object{Name: "main.g"}
	b.Control = b.NewValue(0, OpMakeResult, MemType, call)

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	// The call passes one operand and memory: the struct is in the area.
	if len(call.Args) != 2 || call.Args[0] != tail || !IsMemory(call.Args[1]) {
		t.Fatalf("the call passes %d operands\n%s", len(call.Args), f)
	}
	// The memory it reads is the copy's, so the words are written first.
	mv := call.Args[1]
	if mv.Op != OpMove || mv.AuxInt != typ.Size {
		t.Fatalf("the call reads %v, and a block move of %d bytes writes the area", mv.LongString(), typ.Size)
	}
	dst := mv.Args[0]
	if dst.Op != OpLocalAddr {
		t.Fatalf("the destination of the move is %v", dst.Op)
	}
	if mv.Args[1] != base {
		t.Errorf("the move reads from %v, and the argument was loaded from %v", mv.Args[1], base)
	}
	// The slot is in the outgoing area and is not a local.
	o, _ := dst.Aux.(*ir.Object)
	found := false
	for _, h := range f.ABI.Homes {
		if h.Obj != o {
			continue
		}
		found = true
		if h.Incoming || h.Off != 0 {
			t.Errorf("the slot is at %d of the %s area", h.Off, map[bool]string{true: "incoming", false: "outgoing"}[h.Incoming])
		}
	}
	if !found {
		t.Error("the destination of the move names no argument-area slot")
	}
	for _, fo := range f.Frame {
		if fo == o {
			t.Error("the outgoing slot is in the frame, and the layout would give it a second one")
		}
	}

	// The record is what keeps the operand after it in the right place. The
	// struct occupies the stack part, so the integer's spill slot follows it
	// and the integer is still in the first argument register.
	rec := f.ABI.CallAt(call.ID)
	if rec == nil {
		t.Fatal("the call has no recorded placement, and its operand list no longer describes it")
	}
	if len(rec.Vals) != 2 || !rec.Vals[0].Copied || rec.Vals[1].Copied {
		t.Fatalf("the record holds %d values and marks the wrong one copied", len(rec.Vals))
	}
	if rec.NumOperands() != 1 {
		t.Errorf("the record counts %d operands, and the call passes one", rec.NumOperands())
	}
	if rec.Vals[0].Off != 0 || rec.Vals[1].Off != 160 {
		t.Errorf("the struct is at %d and the integer's slot at %d, want 0 and 160",
			rec.Vals[0].Off, rec.Vals[1].Off)
	}
	if rec.Size != 168 {
		t.Errorf("the area is %d bytes: 160 of struct and 8 of spill", rec.Size)
	}
	// Every reader of the placement takes the record, so the walk that used
	// to compute it does not run and cannot disagree.
	vals, lo, size, err := ABICallArgs(tg, call)
	if err != nil || lo != 0 || size != rec.Size || len(vals) != 2 {
		t.Fatalf("ABICallArgs gave %d values, lo %d, size %d, err %v", len(vals), lo, size, err)
	}
	if r, ok := tg.UseReg(call, 0); !ok || r != tg.ArgRegs[ClassInt][0] {
		t.Errorf("the one operand is read from %v (%v), want the first argument register", r, ok)
	}
}

// TestABICallRecordIsIgnoredForACallSelectionMade covers the fallback: a call
// that lowering creates, runtime.memmove among them, is not in the table and
// is placed by the walk.
func TestABICallRecordIsIgnoredForACallSelectionMade(t *testing.T) {
	tg := NewArm64Target()
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	b.Control = b.NewValue(0, OpMakeResult, MemType, mem)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	// A call made after the pass ran has an identifier past the table.
	x := b.NewValue(0, OpArg, abiTInt)
	late := b.NewValue(0, OpARM64CALLstatic, MemType, x, mem)
	if c := f.ABI.CallAt(late.ID); c != nil {
		t.Error("a call made after the assignment has a record")
	}
	if _, ok := tg.UseReg(late, 0); !ok {
		t.Error("a call made after the assignment has no placement at all")
	}
	if c := f.ABI.CallAt(-1); c != nil {
		t.Error("a negative identifier found a record")
	}
	var nilABI *ABI
	if c := nilABI.CallAt(0); c != nil {
		t.Error("a nil assignment found a record")
	}
	if _, ok := (*ABICall)(nil).Operand(0); ok {
		t.Error("a nil record has an operand")
	}
	if (&ABICall{}).NumOperands() != 0 {
		t.Error("an empty record counts operands")
	}
	if _, ok := (&ABICall{Vals: []ABIValue{{Copied: true}}}).Operand(0); ok {
		t.Error("a copied value was returned as an operand")
	}
}

// TestArrayRegistersMatchTheConvention pins the rule that decides whether an
// array travels in registers.
//
// Go's internal ABI passes an array in registers only when its length is zero
// or one. gc states it in types.CalcArraySize and enforces it by giving a
// longer array a register count of MaxUint8, which no register file holds, and
// types.CalcStructSize propagates the refusal by capping the sum of its
// fields' counts the same way.
//
// The expectations are `go tool compile -S` on arm64:
//
//	f1(x [1]int, y int)     x in R0, y in R1
//	f0(x [0]int, y int)     y in R0, x nowhere
//	f2(x [2]byte, y int)    x at main.x(FP), y in R0
//	fs2(x struct{a [2]byte; b int}, y int)
//	                        x at main.x(FP) and main.x+8(FP), y in R2
//	fs1(x struct{a [1]int; b int}, y int)
//	                        x in R0 and R1, y in R2
//
// Before this rule nanogo gave [16]byte sixteen registers, which exhausted the
// result set and pushed the error after it into the frame. gc does the
// opposite with both: the array is in the frame and the error is in R0 and R1.
func TestArrayRegistersMatchTheConvention(t *testing.T) {
	byteT := &ir.Type{Kind: ir.Uint8, Size: 1, Align: 1, Name: "byte"}
	cases := []struct {
		name string
		typ  *ir.Type
		fits bool
	}{
		{"[16]byte", abiArray("a16", 16, byteT), false},
		{"[2]byte", abiArray("a2", 2, byteT), false},
		{"[1]int", abiArray("a1", 1, abiTInt), true},
		{"[0]int", abiArray("a0", 0, abiTInt), true},
		{"[1][2]byte", abiArray("aa", 1, abiArray("a2", 2, byteT)), false},
		{"[1][1]int", abiArray("aa", 1, abiArray("a1", 1, abiTInt)), true},
		{"struct{[2]byte; int}", &ir.Type{Kind: ir.Struct, Size: 16, Align: 8, Name: "s2",
			Fields: []ir.Field{
				{Name: "a", Type: abiArray("a2", 2, byteT), Offset: 0},
				{Name: "b", Type: abiTInt, Offset: 8},
			}}, false},
		{"struct{[1]int; int}", &ir.Type{Kind: ir.Struct, Size: 16, Align: 8, Name: "s1",
			Fields: []ir.Field{
				{Name: "a", Type: abiArray("a1", 1, abiTInt), Offset: 0},
				{Name: "b", Type: abiTInt, Offset: 8},
			}}, true},
		{"int", abiTInt, true},
		{"string", abiTStr, true},
		{"interface", abiTIface, true},
	}
	for _, c := range cases {
		if _, fits := ABILeaves(c.typ); fits != c.fits {
			t.Errorf("%s fits in registers = %v, want %v", c.name, fits, c.fits)
		}
	}

	// The placement gc makes for ([16]byte, error): the array in the frame at
	// offset zero, the error in the first two result registers.
	tg := NewArm64Target()
	iface := abiTIface
	out, size, err := ABIResults(tg, 0, []*ir.Type{abiArray("a16", 16, byteT), iface})
	if err != nil {
		t.Fatalf("([16]byte, error) was not placed: %v", err)
	}
	if out[0].InReg || out[0].Off != 0 {
		t.Errorf("the [16]byte result is InReg=%v at %d, want the frame at 0", out[0].InReg, out[0].Off)
	}
	if !out[1].InReg || len(out[1].Parts) != 2 {
		t.Fatalf("the error result is InReg=%v with %d parts", out[1].InReg, len(out[1].Parts))
	}
	for i, p := range out[1].Parts {
		if p.Reg != tg.ResultRegs[ClassInt][i] {
			t.Errorf("word %d of the error is in %v, want %v", i, p.Reg, tg.ResultRegs[ClassInt][i])
		}
	}
	if size != 16 {
		t.Errorf("the result area is %d bytes, want 16", size)
	}
}

// TestABIAreaHoldsAResultTheCallSiteDiscards checks that the outgoing argument
// area is sized from the callee's signature.
//
// A result the registers cannot hold is written into the area by the callee,
// and the area belongs to the caller's frame. The callee writes it whether or
// not the call site reads it, so the two calls below need the same area. The
// area was sized from the reads instead, and a statement call to a function
// returning [3000]byte was given none: gc gives such a caller 3024 bytes of
// frame and nanogo gave it 16, so the callee wrote over its caller.
func TestABIAreaHoldsAResultTheCallSiteDiscards(t *testing.T) {
	wide := mkType(&ir.Type{Kind: ir.Array, Len: 3000, Elem: mkType(&ir.Type{Kind: ir.Uint8})})
	sig := mkType(&ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{wide}})

	// One function reads the result and one throws it away. Both call the same
	// callee, so both need the same area.
	area := func(t *testing.T, fn *ir.Func) int64 {
		t.Helper()
		f := build(t, fn)
		if err := AssignABI(f, NewArm64Target()); err != nil {
			t.Fatalf("AssignABI(%s): %v", fn.Name, err)
		}
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op != OpStaticCall {
					continue
				}
				_, _, size, err := ABICallArgs(NewArm64Target(), v)
				if err != nil {
					t.Fatalf("ABICallArgs(%s): %v", fn.Name, err)
				}
				return size
			}
		}
		t.Fatalf("%s holds no call:\n%s", fn.Name, f)
		return 0
	}

	callee := obj("callee", sig, ir.ClassFunc)
	x := obj("x", wide, ir.ClassLocal)
	read := area(t, fun("read", []*ir.Object{x},
		asn(local(x), &ir.Node{Op: ir.OCall, X: local(callee), Type: wide}),
		ret()))
	dropped := area(t, fun("dropped", nil,
		&ir.Node{Op: ir.OCall, X: local(callee), Type: wide},
		ret()))

	if read < wide.Size {
		t.Fatalf("the area of a call whose result is read is %d bytes, and the result alone is %d", read, wide.Size)
	}
	if dropped != read {
		t.Errorf("the area is %d bytes when the result is discarded and %d when it is read; the callee writes it either way",
			dropped, read)
	}
}

// TestAssignABIPlacesResultsOverTheDeclaredList is the rule the word list and
// the declared list disagree about.
//
// The function returns a fifteen-word struct and an interface. The struct
// takes the first fifteen result registers and two are needed for the
// interface, so specs/030-abi.md's all-or-nothing rule puts the whole
// interface in the result area and leaves the sixteenth register unused. That
// is what gc does with (Collected, error) and with every other pair of this
// shape.
//
// Decomposition has already split the interface into its two words by the time
// this pass runs, so a walk of the values the return passes places the itab in
// the sixteenth register and only the data word in the area. Both halves of
// the value are then somewhere the caller does not read. Inside a nanogo-only
// program that is self-consistent, so the check is here and the comparison
// against gc is internal/e2e's.
func TestAssignABIPlacesResultsOverTheDeclaredList(t *testing.T) {
	tg := NewArm64Target()
	s15 := abiStruct("s15", 15, abiTInt)
	f := NewFunc("f")
	f.Type = &ir.Type{Kind: ir.FuncKind, Name: "func() (s15, any)",
		Results: []*ir.Type{s15, abiTIface}}
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	src := b.NewValue(0, OpLocalAddr, abiPtrTo(s15), mem)
	src.Aux = &ir.Object{Name: "v", Type: s15, Class: ir.ClassLocal}
	big := b.NewValue(0, OpLoad, s15, src, mem)
	// The interface reaches the return as its two words, which is what
	// decomposition leaves of a value of two parts.
	itab := b.NewValue(0, OpConstNil, abiUnsafePtr)
	data := b.NewValue(0, OpConstNil, abiUnsafePtr)
	b.Control = b.NewValue(0, OpMakeResult, MemType, big, itab, data, mem)

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	abi := f.ABI
	if len(abi.Out) != 2 {
		t.Fatalf("%d results are placed, and the function declares two", len(abi.Out))
	}
	if !abi.Out[0].InReg || len(abi.Out[0].Parts) != 15 {
		t.Fatalf("the struct is in %d parts, in registers %v", len(abi.Out[0].Parts), abi.Out[0].InReg)
	}
	for i := range abi.Out[0].Parts {
		if got, want := abi.Out[0].Parts[i].Reg, tg.ResultRegs[ClassInt][i]; got != want {
			t.Errorf("word %d of the struct is in %v, want %v", i, got, want)
		}
	}
	if abi.Out[1].InReg {
		t.Fatalf("the interface took registers, and one of the sixteen is left")
	}
	if abi.Out[1].Off != 0 || abi.Out[1].Type.Size != 16 {
		t.Errorf("the interface is %d bytes at %d, want 16 at 0", abi.Out[1].Type.Size, abi.Out[1].Off)
	}
	if abi.ArgsSize != 16 {
		t.Errorf("the area is %d bytes and holds the interface alone", abi.ArgsSize)
	}

	// The return passes the fifteen words of the struct, the two words of the
	// interface and the memory. The struct's words are in the registers the
	// placement named and the interface's are in the two words of the area.
	mr := b.Control
	if len(mr.Args) != 18 {
		t.Fatalf("the return passes %d operands, want 15 words, 2 words and the memory\n%s", len(mr.Args), f)
	}
	vals, err := ABIReturn(tg, mr)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 17 {
		t.Fatalf("%d operands are placed, want 17", len(vals))
	}
	for i := 0; i < 15; i++ {
		if !vals[i].InReg || vals[i].Parts[0].Reg != tg.ResultRegs[ClassInt][i] {
			t.Errorf("operand %d is in %v, want %v", i, vals[i].Parts[0].Reg, tg.ResultRegs[ClassInt][i])
		}
	}
	for i, want := range []int64{0, 8} {
		av := &vals[15+i]
		if av.InReg {
			t.Errorf("word %d of the interface took a register", i)
		}
		if av.Off != want {
			t.Errorf("word %d of the interface is at %d, want %d", i, av.Off, want)
		}
	}
	// The sixteenth result register holds nothing, which is what makes the
	// two compilers agree.
	for i := range vals {
		if vals[i].InReg && vals[i].Parts[0].Reg == tg.ResultRegs[ClassInt][15] {
			t.Errorf("operand %d took the sixteenth result register", i)
		}
	}
}

// TestAssignABIFallsBackToTheReturnWithoutASignature keeps the pass working on
// a function assembled value by value, which carries no signature and whose
// operand list is the only description of what it returns.
func TestAssignABIFallsBackToTheReturnWithoutASignature(t *testing.T) {
	tg := NewArm64Target()
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	a0 := b.NewValue(0, OpArg, abiTInt)
	a0.Aux = &ir.Object{Name: "p", Type: abiTInt, Class: ir.ClassLocal}
	b.Control = b.NewValue(0, OpMakeResult, MemType, a0, a0, mem)
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if len(f.ABI.Out) != 2 {
		t.Fatalf("%d results are placed, and the return passes two values", len(f.ABI.Out))
	}
	for i := range f.ABI.Out {
		if !f.ABI.Out[i].InReg {
			t.Errorf("result %d is not in a register, and two integers fit", i)
		}
	}
}

// TestAssignABIHomesAnArgumentDecompositionAlreadySplit is the second shape of
// the rule TestAssignABIHomesALargeArgumentInTheArea covers.
//
// A parameter the registers cannot hold keeps the storage the caller wrote it
// into, whether decomposition left it whole or split it into one value per
// word. Where it split it, specs/021-ssa-construction.md's copy into a frame
// slot is one store per word and the whole chain writes the value over itself.
//
// An array of two words is the smallest case, because Go's convention refuses
// an array of more than one element a register whatever it holds. Leaving the
// chain in place left a load per word at the head of the entry block, and
// those loads take the registers the caller left the *following* arguments in:
// an OpArg is live from the caller's store and the allocator's range for it
// begins at its own definition. Go's own test/method5.go is where that prints
// the receiver's words in place of the arguments.
func TestAssignABIHomesAnArgumentDecompositionAlreadySplit(t *testing.T) {
	tg := NewArm64Target()
	typ := abiArray("a2", 2, abiTInt)
	f, o := abiFuncWithArg(typ)
	Decompose(f)
	// Decomposition is what makes this the shape under test. Without it the
	// parameter reaches the pass whole and the whole-value rewrite covers it.
	args := 0
	for _, v := range f.Entry.Values {
		if v.Op == OpArg {
			args++
		}
	}
	if args != 2 {
		t.Fatalf("decomposition left %d arguments, and this test is about the two words\n%s", args, f)
	}

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	if f.ABI.In[0].InReg {
		t.Fatalf("the array took registers, and the convention gives an array of two elements none")
	}
	for _, v := range f.Entry.Values {
		if v.Op == OpArg || v.Op == OpStore {
			t.Errorf("v%d survived, and the copy writes the value over itself\n%s", v.ID, f)
		}
	}
	if len(f.Frame) != 0 {
		t.Errorf("the parameter is still a local, and its storage is the argument area")
	}
	off, ok := f.ABI.ArgHome(o)
	if !ok || off != 0 {
		t.Errorf("the parameter is homed at %d (%v), want 0", off, ok)
	}
	found := false
	for _, v := range f.Entry.Values {
		if v.Op == OpLocalAddr && v.Aux == any(o) {
			found = true
		}
	}
	if !found {
		t.Error("the address of the parameter is gone, and the body reads it through that address")
	}
}

// TestAssignABIKeepsAStoreChainItDoesNotOwn holds the match of selfStoreChain
// exact.
//
// A word of a parameter written anywhere but that parameter's own slot is not
// the copy specs/021-ssa-construction.md makes, so deleting it would delete a
// store the program wrote.
func TestAssignABIKeepsAStoreChainItDoesNotOwn(t *testing.T) {
	tg := NewArm64Target()
	typ := abiArray("a2", 2, abiTInt)
	f, o := abiFuncWithArg(typ)
	Decompose(f)
	// The second word goes to a slot of its own, so the chain is no longer
	// one parameter written over itself.
	other := &ir.Object{Name: "q", Type: typ, Class: ir.ClassLocal}
	f.Frame = append(f.Frame, other)
	moved := false
	for _, v := range f.Entry.Values {
		if v.Op != OpStore || v.AuxInt != 8 || v.Args[0].Op != OpOffPtr {
			continue
		}
		addr := v.Args[0].Args[0]
		if addr.Op == OpLocalAddr && addr.Aux == any(o) {
			addr.Aux = other
			moved = true
		}
	}
	if !moved {
		t.Fatalf("the store of the second word was not found\n%s", f)
	}

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	if f.ABI.In[0].Home {
		t.Error("the parameter was homed in the area, and one of its words is written elsewhere")
	}
	stores := 0
	for _, v := range f.Entry.Values {
		if v.Op == OpStore {
			stores++
		}
	}
	if stores != 2 {
		t.Errorf("%d stores are left, and neither of the two is this pass's to delete\n%s", stores, f)
	}
}

// abiCallWithSig builds a call to a function of the given signature, with the
// operands given, and returns the function and the call.
func abiCallWithSig(sig *ir.Type, operand func(b *Block, mem *Value) []*Value) (*Func, *Value) {
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	mem := b.NewValue(0, OpInitMem, MemType)
	args := append(operand(b, mem), mem)
	call := b.NewValue(0, OpStaticCall, MemType, args...)
	call.Aux = &ir.Object{Name: "main.g"}
	call.Sig = sig
	b.Control = b.NewValue(0, OpMakeResult, MemType, call)
	return f, call
}

// TestAssignABIPlacesArgumentsOverTheDeclaredList is the argument half of
// TestAssignABIPlacesResultsOverTheDeclaredList.
//
// Assignment is all-or-nothing per entry of the list it walks, so a list of
// machine words and a list of declared arguments part as soon as a register
// file runs out inside one argument. Fifteen integers and a two-word struct is
// where: gc leaves the sixteenth integer register unused and puts the whole
// struct in the area, and a walk of the words puts the struct's first word in
// that register and only its second in the area.
//
// The float after it is the other half of the same rule. The two register
// files are counted apart, so an argument in the area does not push the ones
// after it out of their own file.
func TestAssignABIPlacesArgumentsOverTheDeclaredList(t *testing.T) {
	tg := NewArm64Target()
	p := abiStruct("P", 2, abiTInt)
	params := make([]*ir.Type, 0, 18)
	for i := 0; i < 15; i++ {
		params = append(params, abiTInt)
	}
	params = append(params, p, abiTInt, abiTF64)
	sig := &ir.Type{Kind: ir.FuncKind, Name: "func(...)", Params: params, Results: []*ir.Type{}}

	f, call := abiCallWithSig(sig, func(b *Block, mem *Value) []*Value {
		out := make([]*Value, 0, 19)
		for i := 0; i < 15; i++ {
			out = append(out, b.NewValue(0, OpConstInt, abiTInt))
		}
		// The struct reaches the call as the two words decomposition left of
		// it, which is the list the placement is not over.
		out = append(out, b.NewValue(0, OpConstInt, abiTInt), b.NewValue(0, OpConstInt, abiTInt))
		out = append(out, b.NewValue(0, OpConstInt, abiTInt))
		out = append(out, b.NewValue(0, OpConstFloat, abiTF64))
		return out
	})

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	vals, lo, size, err := ABICallArgs(tg, call)
	if err != nil {
		t.Fatal(err)
	}
	if lo != 0 || len(vals) != 19 {
		t.Fatalf("a static call with nineteen operands gave lo=%d and %d placements", lo, len(vals))
	}
	for i := 0; i < 15; i++ {
		if !vals[i].InReg || vals[i].Parts[0].Reg != tg.ArgRegs[ClassInt][i] {
			t.Errorf("integer %d is in %v, want %v", i, vals[i].Parts[0].Reg, tg.ArgRegs[ClassInt][i])
		}
	}
	// The struct is all-or-nothing, so neither of its words takes the
	// sixteenth register and both sit in the first two words of the area.
	for i, want := range []int64{0, 8} {
		av := &vals[15+i]
		if av.InReg {
			t.Errorf("word %d of the struct took a register, and the struct does not fit", i)
		}
		if av.Off != want {
			t.Errorf("word %d of the struct is at %d, want %d", i, av.Off, want)
		}
	}
	// The integer after it takes the register the struct did not.
	if !vals[17].InReg || vals[17].Parts[0].Reg != tg.ArgRegs[ClassInt][15] {
		t.Errorf("the integer after the struct is in %v, want %v", vals[17].Parts[0].Reg, tg.ArgRegs[ClassInt][15])
	}
	// The float is in the first register of its own file, which the integers
	// did not touch.
	if !vals[18].InReg || vals[18].Parts[0].Reg != tg.ArgRegs[ClassFloat][0] {
		t.Errorf("the float is in %v, want %v", vals[18].Parts[0].Reg, tg.ArgRegs[ClassFloat][0])
	}
	// gc gives this call 152 bytes: sixteen for the struct, then one spill
	// slot per value that travelled in a register.
	if size != 152 {
		t.Errorf("the outgoing area is %d bytes, and gc gives it 152", size)
	}
}

// TestAssignABIPlacesAReceiverAheadOfTheParameters keeps a method call right.
//
// ir.Converter keeps a method's receiver out of a signature's parameter list,
// the way types2 keeps it out of Params and in Recv, so a call to a method of
// a concrete type passes one operand more than the signature declares. Reading
// the parameter list as though it were the whole of the argument list would
// place every argument one entry early, which is the receiver's words arriving
// as the arguments after it.
func TestAssignABIPlacesAReceiverAheadOfTheParameters(t *testing.T) {
	tg := NewArm64Target()
	recv := abiArray("a2", 2, abiTInt)
	sig := &ir.Type{Kind: ir.FuncKind, Name: "func(int, int8) ()",
		Params: []*ir.Type{abiTInt, abiTI8}, Results: []*ir.Type{}}

	var load *Value
	f, call := abiCallWithSig(sig, func(b *Block, mem *Value) []*Value {
		base := b.NewValue(0, OpArg, abiTPtr)
		base.Aux = &ir.Object{Name: "p", Type: abiTPtr, Class: ir.ClassLocal}
		load = b.NewValue(0, OpLoad, recv, base, mem)
		return []*Value{load, b.NewValue(0, OpConstInt, abiTInt), b.NewValue(0, OpConstInt, abiTI8)}
	})

	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify: %v\n%s", vs, f)
	}
	vals, _, size, err := ABICallArgs(tg, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 3 {
		t.Fatalf("%d values are placed, want the receiver and two arguments\n%s", len(vals), f)
	}
	if vals[0].InReg || !vals[0].Copied || vals[0].Off != 0 {
		t.Errorf("the receiver is at %d, in registers %v, copied %v; the convention gives an array of two elements the area",
			vals[0].Off, vals[0].InReg, vals[0].Copied)
	}
	// The two arguments take the first two registers, because the receiver
	// took none. A file that ran out mid-argument is the only thing that
	// moves them, and neither of these is that.
	for i, av := range []*ABIValue{&vals[1], &vals[2]} {
		if !av.InReg || av.Parts[0].Reg != tg.ArgRegs[ClassInt][i] {
			t.Errorf("argument %d is in %v, want %v", i, av.Parts[0].Reg, tg.ArgRegs[ClassInt][i])
		}
	}
	// The receiver is in the area already, so the call no longer carries it.
	if len(call.Args) != 3 {
		t.Errorf("the call passes %d operands, want two arguments and the memory\n%s", len(call.Args), f)
	}
	if size != 32 {
		t.Errorf("the outgoing area is %d bytes, and gc gives this call 32", size)
	}
}

// TestABICallArgTypesFallsBackToTheOperands names the bound
// specs/030-abi.md leaves.
//
// A receiver decomposition split is in no list: the signature does not carry
// it and the operands are its words rather than its declared type. Neither
// candidate list consumes the operands, so the placement is the walk of the
// operand list that this compiler made before it read the declared one, which
// is right wherever no register file runs out inside one argument.
func TestABICallArgTypesFallsBackToTheOperands(t *testing.T) {
	tg := NewArm64Target()
	sig := &ir.Type{Kind: ir.FuncKind, Name: "func(int) ()",
		Params: []*ir.Type{abiTInt}, Results: []*ir.Type{}}
	f, call := abiCallWithSig(sig, func(b *Block, mem *Value) []*Value {
		// Two words of a receiver and then the one declared parameter.
		return []*Value{
			b.NewValue(0, OpConstInt, abiTInt),
			b.NewValue(0, OpConstInt, abiTInt),
			b.NewValue(0, OpConstInt, abiTInt),
		}
	})
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	ops := abiOperands(call, 0)
	if _, _, ok := abiCallArgTypes(call, ops); ok {
		t.Fatal("the operands were mapped onto the signature, and the receiver's declared type is in neither")
	}
	vals, _, _, err := ABICallArgs(tg, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 3 {
		t.Fatalf("%d values are placed, want one per operand", len(vals))
	}
	for i := range vals {
		if !vals[i].InReg || vals[i].Parts[0].Reg != tg.ArgRegs[ClassInt][i] {
			t.Errorf("operand %d is in %v, want %v", i, vals[i].Parts[0].Reg, tg.ArgRegs[ClassInt][i])
		}
	}
}

// TestSelfStoreChainRejectsEveryShapeButTheCopy walks the ways a chain of
// stores is not specs/021-ssa-construction.md's copy of a parameter into its
// own slot.
//
// Deleting one of these would delete a store the program wrote, so the match
// is exact and each rejection is a case of its own.
func TestSelfStoreChainRejectsEveryShapeButTheCopy(t *testing.T) {
	tg := NewArm64Target()
	typ := abiArray("a2", 2, abiTInt)

	// wordStore returns the store of word k of the parameter, by the offset
	// the placement gives that word.
	wordStore := func(f *Func, off int64) *Value {
		for _, v := range f.Entry.Values {
			if v.Op != OpStore || len(v.Args) != 3 || v.Args[1].Op != OpArg {
				continue
			}
			if v.Args[1].AuxInt == off {
				return v
			}
		}
		return nil
	}

	tests := []struct {
		name   string
		break_ func(f *Func, o *ir.Object)
	}{
		{"a store of the wrong width", func(f *Func, o *ir.Object) {
			wordStore(f, 8).AuxInt = 4
		}},
		{"a word with a second reader", func(f *Func, o *ir.Object) {
			st := wordStore(f, 0)
			f.Entry.NewValue(0, OpARM64ADD, abiTInt, st.Args[1], st.Args[1])
		}},
		{"a word written at the wrong offset", func(f *Func, o *ir.Object) {
			wordStore(f, 8).Args[0].AuxInt = 16
		}},
		{"a chain the second store does not continue", func(f *Func, o *ir.Object) {
			st := wordStore(f, 8)
			st.Args[2] = wordStore(f, 0).Args[2]
		}},
		{"a word written through no local address", func(f *Func, o *ir.Object) {
			st := wordStore(f, 0)
			st.Args[0] = f.Entry.NewValue(0, OpArg, abiPtrTo(abiTInt))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, o := abiFuncWithArg(typ)
			Decompose(f)
			tc.break_(f, o)
			if err := AssignABI(f, tg); err != nil {
				t.Fatal(err)
			}
			if _, ok := f.ABI.ArgHome(o); ok {
				t.Errorf("the parameter was homed in the area, and the copy is not the one this pass owns\n%s", f)
			}
		})
	}
}

// TestAssignABIPlacesAnArgumentOfNoWidth keeps a zero-size argument out of the
// registers and out of the area.
//
// It occupies no word, so it takes no register and shifts nothing, and the
// arguments after it are placed as though it were not there. The operand is
// still an operand, because the graph passes a value for it.
func TestAssignABIPlacesAnArgumentOfNoWidth(t *testing.T) {
	tg := NewArm64Target()
	sig := &ir.Type{Kind: ir.FuncKind, Name: "func(struct{}, int) ()",
		Params: []*ir.Type{abiTEmpty, abiTInt}, Results: []*ir.Type{}}
	f, call := abiCallWithSig(sig, func(b *Block, mem *Value) []*Value {
		return []*Value{
			b.NewValue(0, OpConstInt, abiTEmpty),
			b.NewValue(0, OpConstInt, abiTInt),
		}
	})
	if err := AssignABI(f, tg); err != nil {
		t.Fatal(err)
	}
	vals, _, size, err := ABICallArgs(tg, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 2 {
		t.Fatalf("%d values are placed, want one per operand", len(vals))
	}
	if len(vals[0].Parts) != 0 {
		t.Errorf("the empty struct occupies %d words, and it occupies none", len(vals[0].Parts))
	}
	if !vals[1].InReg || vals[1].Parts[0].Reg != tg.ArgRegs[ClassInt][0] {
		t.Errorf("the integer after it is in %v, want the first argument register", vals[1].Parts[0].Reg)
	}
	// One spill slot, for the integer alone.
	if size != 8 {
		t.Errorf("the outgoing area is %d bytes, want 8", size)
	}
}
