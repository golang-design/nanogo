// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa_test

import (
	"fmt"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/ssa/rules"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The tests of the decomposition pass of specs/025-lowering-and-rules.md.
//
// An external test package, because the corpus test lowers with the arm64
// rules and ssa/rules imports ssa. Everything the pass owes a caller is
// exported, so nothing here reaches inside.

// ---------------------------------------------------------------------------
// Types

func decLaid(t *ir.Type) *ir.Type {
	if err := ir.Layout(t); err != nil {
		panic(err)
	}
	return t
}

var (
	decBool  = decLaid(&ir.Type{Kind: ir.Bool, Name: "bool"})
	decI8    = decLaid(&ir.Type{Kind: ir.Int8, Name: "int8"})
	decI32   = decLaid(&ir.Type{Kind: ir.Int32, Name: "int32"})
	decInt   = decLaid(&ir.Type{Kind: ir.Int64, Name: "int"})
	decF64   = decLaid(&ir.Type{Kind: ir.Float64, Name: "float64"})
	decPtr   = decLaid(&ir.Type{Kind: ir.Ptr, Elem: decInt, Name: "*int"})
	decStr   = decLaid(&ir.Type{Kind: ir.String, Name: "string"})
	decSlice = decLaid(&ir.Type{Kind: ir.Slice, Elem: decInt, Name: "[]int"})
	decIface = decLaid(&ir.Type{Kind: ir.Interface, Name: "error"})
	decCplx  = decLaid(&ir.Type{Kind: ir.Complex128, Name: "complex128"})
	decArr2  = decLaid(&ir.Type{Kind: ir.Array, Elem: decInt, Len: 2, Name: "[2]int"})
	decArr4  = decLaid(&ir.Type{Kind: ir.Array, Elem: decInt, Len: 4, Name: "[4]int"})
	decArr5  = decLaid(&ir.Type{Kind: ir.Array, Elem: decInt, Len: 5, Name: "[5]int"})

	// A struct whose pointer map is not the pointer map of any one field.
	decPair = decLaid(&ir.Type{Kind: ir.Struct, Name: "pair", Fields: []ir.Field{
		{Name: "P", Type: decPtr},
		{Name: "N", Type: decI32},
	}})
	// A struct that holds a string, so a field decomposes further.
	decNamed = decLaid(&ir.Type{Kind: ir.Struct, Name: "named", Fields: []ir.Field{
		{Name: "S", Type: decStr},
		{Name: "N", Type: decInt},
	}})
	decEmpty = decLaid(&ir.Type{Kind: ir.Struct, Name: "empty"})
	decBig   = decLaid(&ir.Type{Kind: ir.Struct, Name: "big", Fields: []ir.Field{
		{Name: "A", Type: decInt}, {Name: "B", Type: decInt},
		{Name: "C", Type: decInt}, {Name: "D", Type: decInt},
		{Name: "E", Type: decInt},
	}})
	decTuple = decLaid(&ir.Type{Kind: ir.Tuple, Fields: []ir.Field{
		{Type: decStr}, {Type: decInt}, {Type: decIface},
	}})
	decPtrStr = decLaid(&ir.Type{Kind: ir.Ptr, Elem: decStr, Name: "*string"})
)

// ---------------------------------------------------------------------------
// A minimal function builder

type decFn struct {
	f   *ssa.Func
	b   *ssa.Block
	mem *ssa.Value
}

func newDecFn() *decFn {
	f := ssa.NewFunc("f")
	b := f.Entry
	b.Kind = ssa.BlockRet
	p := &decFn{f: f, b: b}
	p.mem = b.NewValue(0, ssa.OpInitMem, ssa.MemType)
	return p
}

func (p *decFn) v(op ssa.Op, t *ir.Type, args ...*ssa.Value) *ssa.Value {
	return p.b.NewValue(0, op, t, args...)
}

func (p *decFn) arg(t *ir.Type, name string) *ssa.Value {
	v := p.v(ssa.OpArg, t)
	v.Aux = &ir.Object{Name: name, Type: t, Class: ir.ClassParam}
	return v
}

// load returns a load of t through a fresh pointer argument.
func (p *decFn) load(t *ir.Type) *ssa.Value {
	return p.v(ssa.OpLoad, t, p.arg(decPtr, "p"), p.mem)
}

func (p *decFn) ret(vals ...*ssa.Value) *ssa.Func {
	args := append(append([]*ssa.Value{}, vals...), p.mem)
	p.b.Control = p.b.NewValue(0, ssa.OpMakeResult, ssa.MemType, args...)
	return p.f
}

// decForm returns the dump with the leading func line removed, which is what a
// test asserts against.
func decForm(f *ssa.Func) string {
	s := f.String()
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimRight(s, "\n")
}

func decWantForm(t *testing.T, f *ssa.Func, want string) {
	t.Helper()
	got := decForm(f)
	if got != strings.TrimSpace(want) {
		t.Errorf("decomposed decForm\ngot:\n%s\nwant:\n%s", got, strings.TrimSpace(want))
	}
}

func decVerified(t *testing.T, f *ssa.Func) {
	t.Helper()
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("%s does not verify: %v\n%s", f.Name, vs, f)
	}
}

// ---------------------------------------------------------------------------
// One test per composite type: the exact values and their types

func TestDecomposeLoadPerType(t *testing.T) {
	tests := []struct {
		name string
		typ  *ir.Type
		want []string // op and type of each part, in order
	}{
		{"string", decStr, []string{"Load *uint8", "Load int"}},
		{"slice", decSlice, []string{"Load *int", "Load int", "Load int"}},
		{"interface", decIface, []string{"Load unsafe.Pointer", "Load unsafe.Pointer"}},
		{"complex128", decCplx, []string{"Load float64", "Load float64"}},
		{"struct", decPair, []string{"Load *int", "Load int32"}},
		{"nested struct", decNamed, []string{"Load *uint8", "Load int", "Load int"}},
		{"array", decArr2, []string{"Load int", "Load int"}},
		{"empty struct", decEmpty, nil},
		{"tuple", decTuple, []string{
			"Load *uint8", "Load int", "Load int",
			"Load unsafe.Pointer", "Load unsafe.Pointer",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newDecFn()
			f := p.ret(p.load(tc.typ))
			ssa.Decompose(f)
			decVerified(t, f)

			var got []string
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					if v.Op == ssa.OpLoad {
						got = append(got, fmt.Sprintf("%v %v", v.Op, v.Type))
					}
				}
			}
			if strings.Join(got, "; ") != strings.Join(tc.want, "; ") {
				t.Errorf("parts\ngot:  %v\nwant: %v", got, tc.want)
			}
			if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
				t.Errorf("a value wider than a register survived: %v", vs)
			}
		})
	}
}

// TestDecomposeLoadForm asserts the whole shape of one decomposition,
// including the address arithmetic, so that a change to it is reviewable.
func TestDecomposeLoadForm(t *testing.T) {
	p := newDecFn()
	f := p.ret(p.load(decSlice))
	ssa.Decompose(f)
	decVerified(t, f)
	decWantForm(t, f, `
b0:
    v0 = InitMem <mem>
    v1 = Arg <*int> {p}
    v5 = OffPtr <*int> [8] v1
    v7 = OffPtr <*int> [16] v1
    v4 = Load <*int> v1 v0
    v6 = Load <int> v5 v0
    v8 = Load <int> v7 v0
    v3 = MakeResult <mem> v4 v6 v8 v0
  Ret v3
`)
}

// ---------------------------------------------------------------------------
// The pointer map
//
// This is the property the pass exists to keep. A string split into two
// integers lowers perfectly and makes its data pointer invisible to the
// collector, and nothing else in the compiler would notice.

func decBitAt(b []byte, i int64) bool {
	if i < 0 || i/8 >= int64(len(b)) {
		return false
	}
	return b[i/8]&(1<<uint(i%8)) != 0
}

func TestPointerMap(t *testing.T) {
	types := []*ir.Type{
		decStr, decSlice, decIface, decCplx, decPair, decNamed, decArr2, decArr4, decTuple,
		decLaid(&ir.Type{Kind: ir.Array, Elem: decStr, Len: 2, Name: "[2]string"}),
		decLaid(&ir.Type{Kind: ir.Struct, Name: "two", Fields: []ir.Field{
			{Name: "A", Type: decIface}, {Name: "B", Type: decPtr},
		}}),
	}
	for _, ty := range types {
		t.Run(ty.String(), func(t *testing.T) {
			offs, parts, ok := ssa.PartsOfType(ty)
			if !ok {
				t.Fatalf("%v does not decompose", ty)
			}
			words := ty.Size / ir.PtrSize
			union := make([]bool, words+1)
			for i, p := range parts {
				if len(p.PtrBits) == 0 {
					continue
				}
				if offs[i]%ir.PtrSize != 0 {
					t.Fatalf("part %d holds a pointer at offset %d, which is not word aligned", i, offs[i])
				}
				base := offs[i] / ir.PtrSize
				for w := int64(0); w*ir.PtrSize < p.Size; w++ {
					if decBitAt(p.PtrBits, w) {
						union[base+w] = true
					}
				}
			}
			// Bit by bit over the words of the type. The byte slices may
			// differ in length while meaning the same map.
			for w := int64(0); w < words; w++ {
				if union[w] != decBitAt(ty.PtrBits, w) {
					t.Errorf("word %d: parts say %v, %v says %v", w, union[w], ty, decBitAt(ty.PtrBits, w))
				}
			}
			if ty.HasPointers() {
				any := false
				for _, p := range parts {
					if p.HasPointers() {
						any = true
					}
				}
				if !any {
					t.Errorf("%v holds pointers and no part does", ty)
				}
			}
		})
	}
}

// TestPointerMapAfterDecompose is the same property read off the graph rather
// than off the type, because it is the value's type that the stack map reads.
func TestPointerMapAfterDecompose(t *testing.T) {
	p := newDecFn()
	f := p.ret(p.load(decStr))
	ssa.Decompose(f)

	ptrs := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == ssa.OpLoad && v.Type.HasPointers() {
				ptrs++
			}
		}
	}
	if ptrs != 1 {
		t.Errorf("a decomposed string has %d pointer-typed parts, want 1", ptrs)
	}
}

// ---------------------------------------------------------------------------
// Store

func TestDecomposeStore(t *testing.T) {
	p := newDecFn()
	src := p.arg(decPtrStr, "src")
	dst := p.arg(decPtrStr, "dst")
	val := p.v(ssa.OpLoad, decStr, src, p.mem)
	st := p.v(ssa.OpStore, ssa.MemType, dst, val, p.mem)
	st.AuxInt = decStr.Size
	p.mem = st
	f := p.ret()
	ssa.Decompose(f)
	decVerified(t, f)
	decWantForm(t, f, `
b0:
    v0 = InitMem <mem>
    v1 = Arg <*string> {src}
    v2 = Arg <*string> {dst}
    v7 = OffPtr <*int> [8] v1
    v6 = Load <*uint8> v1 v0
    v8 = Load <int> v7 v0
    v9 = Store <mem> [8] v2 v6 v0
    v10 = OffPtr <*int> [8] v2
    v4 = Store <mem> [8] v10 v8 v9
    v5 = MakeResult <mem> v4
  Ret v5
`)
}

// TestDecomposeStoreEmpty covers a value with no parts. It writes nothing, so
// the store goes and its readers take the memory it took. Turning it into a
// Copy would break the chain the verifier walks, because a copy does not
// produce memory.
func TestDecomposeStoreEmpty(t *testing.T) {
	p := newDecFn()
	dst := p.arg(decPtr, "dst")
	val := p.v(ssa.OpLoad, decEmpty, p.arg(decPtr, "src"), p.mem)
	st := p.v(ssa.OpStore, ssa.MemType, dst, val, p.mem)
	st.AuxInt = 0
	p.mem = st
	f := p.ret()
	ssa.Decompose(f)
	decVerified(t, f)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == ssa.OpStore {
				t.Errorf("a store of an empty struct survived: %s", v.LongString())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Phi

// TestDecomposePhiBranch splits a phi at a join of two arms.
func TestDecomposePhiBranch(t *testing.T) {
	f := ssa.NewFunc("f")
	entry := f.Entry
	entry.Kind = ssa.BlockIf
	mem := entry.NewValue(0, ssa.OpInitMem, ssa.MemType)
	cond := entry.NewValue(0, ssa.OpArg, decBool)
	ptr := entry.NewValue(0, ssa.OpArg, decPtr)
	entry.Control = cond

	left := f.NewBlock(ssa.BlockPlain)
	right := f.NewBlock(ssa.BlockPlain)
	join := f.NewBlock(ssa.BlockRet)
	entry.AddEdgeTo(left)
	entry.AddEdgeTo(right)
	left.AddEdgeTo(join)
	right.AddEdgeTo(join)

	a := left.NewValue(0, ssa.OpLoad, decStr, ptr, mem)
	b := right.NewValue(0, ssa.OpConstString, decStr)
	b.Aux = "hi"
	phi := join.NewValue(0, ssa.OpPhi, decStr, a, b)
	join.Control = join.NewValue(0, ssa.OpMakeResult, ssa.MemType, phi, mem)

	decVerified(t, f)
	ssa.Decompose(f)
	decVerified(t, f)

	phis := 0
	for _, v := range join.Values {
		if v.Op == ssa.OpPhi {
			phis++
			if ssa.Multiword(v.Type) {
				t.Errorf("a phi of %v survived", v.Type)
			}
			if len(v.Args) != 2 {
				t.Errorf("phi %v has %d arguments, want 2", v, len(v.Args))
			}
		}
	}
	if phis != 2 {
		t.Errorf("%d phis, want 2 for a string", phis)
	}
	if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
		t.Errorf("%v", vs)
	}
}

// TestDecomposePhiLoop splits a phi whose second argument is defined in the
// block that branches back to it. It is the case that forces the pass to make
// every part before it fills any of them.
func TestDecomposePhiLoop(t *testing.T) {
	f := ssa.NewFunc("f")
	entry := f.Entry
	entry.Kind = ssa.BlockPlain
	mem := entry.NewValue(0, ssa.OpInitMem, ssa.MemType)
	ptr := entry.NewValue(0, ssa.OpArg, decPtr)
	init := entry.NewValue(0, ssa.OpConstString, decStr)
	init.Aux = ""

	head := f.NewBlock(ssa.BlockIf)
	body := f.NewBlock(ssa.BlockPlain)
	exit := f.NewBlock(ssa.BlockRet)
	entry.AddEdgeTo(head)
	head.AddEdgeTo(body)
	head.AddEdgeTo(exit)
	body.AddEdgeTo(head)

	phi := head.NewValue(0, ssa.OpPhi, decStr, init, nil)
	head.Control = head.NewValue(0, ssa.OpArg, decBool)
	next := body.NewValue(0, ssa.OpLoad, decStr, ptr, mem)
	phi.SetArg(1, next)
	exit.Control = exit.NewValue(0, ssa.OpMakeResult, ssa.MemType, phi, mem)

	decVerified(t, f)
	ssa.Decompose(f)
	decVerified(t, f)

	for _, v := range head.Values {
		if v.Op != ssa.OpPhi {
			continue
		}
		if len(v.Args) != 2 {
			t.Fatalf("phi %s has %d arguments", v.LongString(), len(v.Args))
		}
		if v.Args[1] == nil || v.Args[1].Block != body {
			t.Errorf("phi %s does not read the back edge", v.LongString())
		}
	}
	if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
		t.Errorf("%v", vs)
	}
}

// TestDecomposePhiNotSplit keeps a phi whole when what it merges is not split.
// A phi over a value that stays in memory has to stay whole too, which is why
// the plan is a fixed point rather than one walk.
func TestDecomposePhiNotSplit(t *testing.T) {
	f := ssa.NewFunc("f")
	entry := f.Entry
	entry.Kind = ssa.BlockIf
	mem := entry.NewValue(0, ssa.OpInitMem, ssa.MemType)
	ptr := entry.NewValue(0, ssa.OpArg, decPtr)
	entry.Control = entry.NewValue(0, ssa.OpArg, decBool)

	left := f.NewBlock(ssa.BlockPlain)
	right := f.NewBlock(ssa.BlockPlain)
	join := f.NewBlock(ssa.BlockRet)
	entry.AddEdgeTo(left)
	entry.AddEdgeTo(right)
	left.AddEdgeTo(join)
	right.AddEdgeTo(join)

	a := left.NewValue(0, ssa.OpLoad, decBig, ptr, mem)
	b := right.NewValue(0, ssa.OpLoad, decBig, ptr, mem)
	phi := join.NewValue(0, ssa.OpPhi, decBig, a, b)
	join.Control = join.NewValue(0, ssa.OpMakeResult, ssa.MemType, phi, mem)

	ssa.Decompose(f)
	decVerified(t, f)
	if phi.Op != ssa.OpPhi || phi.Type != decBig {
		t.Errorf("the phi of a value that stays in memory was rewritten: %s", phi.LongString())
	}
	if len(ssa.CheckDecomposed(f)) == 0 {
		t.Error("CheckDecomposed reported nothing for a value that stays in memory")
	}
}

// ---------------------------------------------------------------------------
// Copy, Arg, SelectN, constants

func TestDecomposeCopy(t *testing.T) {
	p := newDecFn()
	f := p.ret(p.v(ssa.OpCopy, decStr, p.load(decStr)))
	ssa.Decompose(f)
	decVerified(t, f)
	copies := 0
	for _, v := range p.b.Values {
		if v.Op == ssa.OpCopy {
			copies++
			if ssa.Multiword(v.Type) {
				t.Errorf("a copy of %v survived", v.Type)
			}
		}
	}
	if copies != 2 {
		t.Errorf("%d copies, want 2", copies)
	}
}

// TestDecomposeArg splits an incoming parameter. Each part keeps the object it
// came from in Aux and its byte offset within it in AuxInt, which is the pair
// specs/030-abi.md needs to give the part a location.
func TestDecomposeArg(t *testing.T) {
	p := newDecFn()
	obj := &ir.Object{Name: "s", Type: decStr, Class: ir.ClassParam}
	a := p.v(ssa.OpArg, decStr)
	a.Aux = obj
	f := p.ret(a)
	ssa.Decompose(f)
	decVerified(t, f)

	var got []string
	for _, v := range p.b.Values {
		if v.Op == ssa.OpArg {
			got = append(got, fmt.Sprintf("%v@%d", v.Type, v.AuxInt))
			if v.Aux != obj {
				t.Errorf("part %s lost its object", v.LongString())
			}
		}
	}
	if want := "*uint8@0 int@8"; strings.Join(got, " ") != want {
		t.Errorf("arg parts are %q, want %q", strings.Join(got, " "), want)
	}
}

func TestDecomposeConstString(t *testing.T) {
	p := newDecFn()
	c := p.v(ssa.OpConstString, decStr)
	c.Aux = "hello"
	e := p.v(ssa.OpConstString, decStr)
	e.Aux = ""
	f := p.ret(c, e)
	ssa.Decompose(f)
	decVerified(t, f)

	var addr, lens []*ssa.Value
	for _, v := range p.b.Values {
		switch v.Op {
		case ssa.OpAddr, ssa.OpConstNil:
			addr = append(addr, v)
		case ssa.OpConstInt:
			lens = append(lens, v)
		}
	}
	if len(addr) != 2 || len(lens) != 2 {
		t.Fatalf("got %d data words and %d lengths, want 2 and 2", len(addr), len(lens))
	}
	sym, ok := addr[0].Aux.(*ssa.StringSym)
	if !ok {
		t.Fatalf("the data word of a string constant has Aux %T", addr[0].Aux)
	}
	if sym.Text != "hello" {
		t.Errorf("the symbol holds %q", sym.Text)
	}
	if !strings.HasPrefix(sym.String(), "go:string.") {
		t.Errorf("the symbol is named %q", sym.String())
	}
	if lens[0].AuxInt != 5 {
		t.Errorf("len is %d, want 5", lens[0].AuxInt)
	}
	// The empty string points at nothing rather than at a symbol of no bytes.
	if addr[1].Op != ssa.OpConstNil || lens[1].AuxInt != 0 {
		t.Errorf("the empty string is %s and %s", addr[1].LongString(), lens[1].LongString())
	}
	// Two equal constants name one symbol: the name is the content.
	if newSym := decStringSymName(t, "hello"); newSym != sym.String() {
		t.Errorf("two spellings of one constant name %q and %q", sym.String(), newSym)
	}
}

func decStringSymName(t *testing.T, s string) string {
	t.Helper()
	p := newDecFn()
	c := p.v(ssa.OpConstString, decStr)
	c.Aux = s
	f := p.ret(c)
	ssa.Decompose(f)
	for _, v := range p.b.Values {
		if sym, ok := v.Aux.(*ssa.StringSym); ok {
			return sym.String()
		}
	}
	t.Fatal("no string symbol")
	return ""
}

func TestDecomposeConstNil(t *testing.T) {
	tests := []struct {
		typ  *ir.Type
		want string
	}{
		{decSlice, "ConstNil <*int>; ConstInt <int>; ConstInt <int>"},
		{decIface, "ConstNil <unsafe.Pointer>; ConstNil <unsafe.Pointer>"},
		{decCplx, "ConstFloat <float64>; ConstFloat <float64>"},
		{decPair, "ConstNil <*int>; ConstInt <int32>"},
	}
	for _, tc := range tests {
		t.Run(tc.typ.String(), func(t *testing.T) {
			p := newDecFn()
			f := p.ret(p.v(ssa.OpConstNil, tc.typ))
			ssa.Decompose(f)
			decVerified(t, f)
			var got []string
			for _, v := range p.b.Values {
				switch v.Op {
				case ssa.OpConstNil, ssa.OpConstInt, ssa.OpConstFloat, ssa.OpConstBool:
					got = append(got, fmt.Sprintf("%v <%v>", v.Op, v.Type))
				}
			}
			if strings.Join(got, "; ") != tc.want {
				t.Errorf("got %q, want %q", strings.Join(got, "; "), tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Calls

// TestDecomposeCallArgs passes a string and a slice to a call. Each becomes
// one argument per part, in place, so the argument order is still the source
// order and the memory argument is still last.
func TestDecomposeCallArgs(t *testing.T) {
	p := newDecFn()
	s := p.load(decStr)
	n := p.arg(decInt, "n")
	sl := p.load(decSlice)
	call := p.v(ssa.OpStaticCall, ssa.MemType, s, n, sl, p.mem)
	call.Aux = &ir.Object{Name: "callee", Class: ir.ClassFunc}
	p.mem = call
	f := p.ret()
	ssa.Decompose(f)
	decVerified(t, f)

	var got []string
	for _, a := range call.Args {
		got = append(got, a.Type.String())
	}
	want := "*uint8 int int *int int int mem"
	if strings.Join(got, " ") != want {
		t.Errorf("call arguments are %q, want %q", strings.Join(got, " "), want)
	}
}

// TestDecomposeCallResultTuple reads a call that returns several values. The
// tuple has no address and no memory decForm, so it is split whatever its width.
func TestDecomposeCallResultTuple(t *testing.T) {
	p := newDecFn()
	call := p.v(ssa.OpStaticCall, ssa.MemType, p.mem)
	call.Aux = &ir.Object{Name: "callee", Class: ir.ClassFunc}
	p.mem = call
	res := p.v(ssa.OpSelectN, decTuple, call)
	f := p.ret(res)
	ssa.Decompose(f)
	decVerified(t, f)

	var got []string
	for _, v := range p.b.Values {
		if v.Op == ssa.OpSelectN {
			got = append(got, fmt.Sprintf("%d:%v", v.AuxInt, v.Type))
		}
	}
	want := "0:*uint8 1:int 2:int 3:unsafe.Pointer 4:unsafe.Pointer"
	if strings.Join(got, " ") != want {
		t.Errorf("results are %q, want %q", strings.Join(got, " "), want)
	}
	if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
		t.Errorf("%v", vs)
	}
}

// TestDecomposeReturn returns a composite value. MakeResult carries the parts
// the same way a call carries its arguments.
func TestDecomposeReturn(t *testing.T) {
	p := newDecFn()
	f := p.ret(p.load(decIface), p.arg(decInt, "n"))
	ssa.Decompose(f)
	decVerified(t, f)
	mr := p.b.Control
	var got []string
	for _, a := range mr.Args {
		got = append(got, a.Type.String())
	}
	if want := "unsafe.Pointer unsafe.Pointer int mem"; strings.Join(got, " ") != want {
		t.Errorf("results are %q, want %q", strings.Join(got, " "), want)
	}
}

// ---------------------------------------------------------------------------
// Equality

func TestDecomposeEquality(t *testing.T) {
	tests := []struct {
		name  string
		typ   *ir.Type
		nil2  bool // compare against the literal nil rather than a second load
		op    ssa.Op
		want  ssa.Op // the operation the result value ends up with
		split bool
	}{
		{"struct of scalars", decPair, false, ssa.OpEq, ssa.OpAnd, true},
		{"struct of scalars neq", decPair, false, ssa.OpNeq, ssa.OpOr, true},
		{"array", decArr2, false, ssa.OpEq, ssa.OpAnd, true},
		{"complex", decCplx, false, ssa.OpEq, ssa.OpAnd, true},
		// A string compares its bytes and not its pointer, so a part
		// comparison would be wrong. It becomes the length check and the call
		// to runtime.memequal of specs/020's table, joined by And.
		{"string", decStr, false, ssa.OpEq, ssa.OpAnd, true},
		{"string neq", decStr, false, ssa.OpNeq, ssa.OpNot, true},
		// A string inside a struct is not reached: the expansion is written
		// for a whole string and the field-wise walk has no place to put a
		// call, so the struct stays whole.
		{"struct holding a string", decNamed, false, ssa.OpEq, ssa.OpEq, false},
		// Two general interfaces need the dynamic type's equality function,
		// which is the call to runtime.ifaceeq of specs/020's table, joined by
		// And with the comparison of the two itabs.
		{"interface", decIface, false, ssa.OpEq, ssa.OpAnd, true},
		{"interface neq", decIface, false, ssa.OpNeq, ssa.OpNot, true},
		// An interface against the literal nil is two zero words and nothing
		// else, so the part comparison is the whole answer.
		{"interface against nil", decIface, true, ssa.OpNeq, ssa.OpOr, true},
		// A slice against nil is the data pointer alone, so the comparison
		// keeps its operation and loses two of its three parts.
		{"slice against nil", decSlice, true, ssa.OpEq, ssa.OpEq, true},
		{"slice against nil neq", decSlice, true, ssa.OpNeq, ssa.OpNeq, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newDecFn()
			x := p.load(tc.typ)
			var y *ssa.Value
			if tc.nil2 {
				y = p.v(ssa.OpConstNil, tc.typ)
			} else {
				y = p.load(tc.typ)
			}
			eq := p.v(tc.op, decBool, x, y)
			f := p.ret(eq)
			ssa.Decompose(f)
			if eq.Op != tc.want {
				t.Errorf("the comparison became %v, want %v", eq.Op, tc.want)
			}
			if tc.split {
				decVerified(t, f)
				if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
					t.Errorf("%v", vs)
				}
				return
			}
			if len(ssa.CheckDecomposed(f)) == 0 {
				t.Error("the operands were split for a comparison that is not per part")
			}
		})
	}
}

// TestDecomposeEqualitySingleAndEmpty covers the two ends of the chain: one
// part needs no join, and no part is a constant answer.
func TestDecomposeEqualitySingleAndEmpty(t *testing.T) {
	one := decLaid(&ir.Type{Kind: ir.Struct, Name: "one", Fields: []ir.Field{{Name: "A", Type: decInt}}})

	p := newDecFn()
	eq := p.v(ssa.OpEq, decBool, p.load(one), p.load(one))
	f := p.ret(eq)
	ssa.Decompose(f)
	decVerified(t, f)
	if eq.Op != ssa.OpEq || eq.Args[0].Type != decInt {
		t.Errorf("one part became %s", eq.LongString())
	}

	q := newDecFn()
	eq2 := q.v(ssa.OpNeq, decBool, q.load(decEmpty), q.load(decEmpty))
	g := q.ret(eq2)
	ssa.Decompose(g)
	decVerified(t, g)
	if eq2.Op != ssa.OpConstBool || eq2.AuxInt != 0 {
		t.Errorf("no part became %s, want ConstBool [0]", eq2.LongString())
	}
}

// ---------------------------------------------------------------------------
// Where decomposition stops

// TestDecomposeThreshold tests both sides of MaxDecomposeParts.
func TestDecomposeThreshold(t *testing.T) {
	if _, _, ok := ssa.PartsOfType(decArr4); !ok {
		t.Errorf("%v has %d parts and does not decompose", decArr4, ssa.MaxDecomposeParts)
	}
	if _, _, ok := ssa.PartsOfType(decArr5); ok {
		t.Errorf("%v has more than %d parts and decomposes", decArr5, ssa.MaxDecomposeParts)
	}
	if _, _, ok := ssa.PartsOfType(decBig); ok {
		t.Errorf("%v has more than %d parts and decomposes", decBig, ssa.MaxDecomposeParts)
	}
	// A tuple has no memory decForm, so the bound does not apply to it.
	wide := decLaid(&ir.Type{Kind: ir.Tuple, Fields: []ir.Field{
		{Type: decSlice}, {Type: decSlice}, {Type: decInt},
	}})
	offs, parts, ok := ssa.PartsOfType(wide)
	if !ok || len(parts) != 7 || len(offs) != 7 {
		t.Errorf("a tuple of %d parts decomposes into %d, ok=%v", 7, len(parts), ok)
	}
	// A single machine value is not decomposed at all.
	for _, ty := range []*ir.Type{decInt, decPtr, decBool, decF64, decI8} {
		if _, _, ok := ssa.PartsOfType(ty); ok {
			t.Errorf("%v is one register and decomposes", ty)
		}
	}
}

// TestAggregateCopy covers the memory half of the bound: a value too wide to
// split is a memory object, and the copy of one is a block move. The addresses
// are already in the graph, which is what makes a move possible here and
// impossible for a store of a value that has no address.
func TestAggregateCopy(t *testing.T) {
	p := newDecFn()
	src := p.arg(decPtr, "src")
	dst := p.arg(decPtr, "dst")
	val := p.v(ssa.OpLoad, decBig, src, p.mem)
	st := p.v(ssa.OpStore, ssa.MemType, dst, val, p.mem)
	st.AuxInt = decBig.Size
	p.mem = st
	f := p.ret()
	ssa.Decompose(f)
	decVerified(t, f)
	if st.Op != ssa.OpMove || st.AuxInt != decBig.Size {
		t.Fatalf("the copy became %s", st.LongString())
	}
	if st.Args[0] != dst || st.Args[1] != src {
		t.Errorf("the move reads %v and writes %v", st.Args[1], st.Args[0])
	}
	if len(ssa.CheckDecomposed(f)) != 0 {
		t.Errorf("the load survived the move: %v", ssa.CheckDecomposed(f))
	}
}

// TestAggregateCopyRefused keeps the load when it is read twice, because the
// value is needed somewhere the move does not write.
func TestAggregateCopyRefused(t *testing.T) {
	p := newDecFn()
	src := p.arg(decPtr, "src")
	dst := p.arg(decPtr, "dst")
	val := p.v(ssa.OpLoad, decBig, src, p.mem)
	st := p.v(ssa.OpStore, ssa.MemType, dst, val, p.mem)
	st.AuxInt = decBig.Size
	p.mem = st
	st2 := p.v(ssa.OpStore, ssa.MemType, src, val, st)
	st2.AuxInt = decBig.Size
	p.mem = st2
	f := p.ret()
	ssa.Decompose(f)
	decVerified(t, f)
	if st.Op != ssa.OpStore {
		t.Errorf("a load with two readers became a move: %s", st.LongString())
	}
}

// ---------------------------------------------------------------------------
// General properties

func TestDecomposeIdempotent(t *testing.T) {
	build := func() *ssa.Func {
		p := newDecFn()
		s := p.load(decStr)
		st := p.v(ssa.OpStore, ssa.MemType, p.arg(decPtrStr, "dst"), s, p.mem)
		st.AuxInt = decStr.Size
		p.mem = st
		return p.ret(p.load(decSlice))
	}
	f := build()
	ssa.Decompose(f)
	once := decForm(f)
	ssa.Decompose(f)
	if got := decForm(f); got != once {
		t.Errorf("a second pass changed the function\ngot:\n%s\nwant:\n%s", got, once)
	}
}

func TestDecomposeDeterministic(t *testing.T) {
	build := func() *ssa.Func {
		p := newDecFn()
		c := p.v(ssa.OpConstString, decStr)
		c.Aux = "abc"
		st := p.v(ssa.OpStore, ssa.MemType, p.arg(decPtrStr, "dst"), c, p.mem)
		st.AuxInt = decStr.Size
		p.mem = st
		return p.ret(p.load(decNamed), p.v(ssa.OpConstNil, decIface))
	}
	f := build()
	ssa.Decompose(f)
	first := decForm(f)
	for i := 0; i < 4; i++ {
		g := build()
		ssa.Decompose(g)
		if got := decForm(g); got != first {
			t.Fatalf("run %d differs\ngot:\n%s\nwant:\n%s", i, got, first)
		}
	}
}

func TestMultiword(t *testing.T) {
	wide := []*ir.Type{decStr, decSlice, decIface, decCplx, decPair, decArr2, decBig, decTuple}
	for _, ty := range wide {
		if !ssa.Multiword(ty) {
			t.Errorf("%v is not reported as wider than a register", ty)
		}
	}
	narrow := []*ir.Type{decInt, decPtr, decBool, decF64, decI8, decI32, ssa.MemType, nil}
	for _, ty := range narrow {
		if ssa.Multiword(ty) {
			t.Errorf("%v is reported as wider than a register", ty)
		}
	}
}

// TestDecomposeLeavesUnknownProducers alone. A composite value whose operation
// has no per-part decForm must survive rather than be half rewritten, and
// CheckDecomposed is what makes that visible instead of silent.
func TestDecomposeUnknownProducer(t *testing.T) {
	p := newDecFn()
	bad := p.v(ssa.OpBitcast, decStr, p.arg(decInt, "n"))
	f := p.ret(bad)
	ssa.Decompose(f)
	decVerified(t, f)
	if bad.Op != ssa.OpBitcast || bad.Type != decStr {
		t.Errorf("an operation with no per-part decForm was rewritten: %s", bad.LongString())
	}
	vs := ssa.CheckDecomposed(f)
	if len(vs) != 1 || vs[0].Value != bad.ID {
		t.Errorf("CheckDecomposed reported %v", vs)
	}
}

// ---------------------------------------------------------------------------
// End to end through the arm64 rules

func TestDecomposeThenLower(t *testing.T) {
	p := newDecFn()
	src := p.arg(decPtrStr, "src")
	dst := p.arg(decPtrStr, "dst")
	c := p.v(ssa.OpConstString, decStr)
	c.Aux = "hi"
	st := p.v(ssa.OpStore, ssa.MemType, dst, c, p.mem)
	st.AuxInt = decStr.Size
	p.mem = st
	f := p.ret(p.v(ssa.OpLoad, decStr, src, p.mem))

	ssa.Lower(f, rules.ARM64)
	decVerified(t, f)
	if vs := ssa.CheckLowered(f, rules.ARM64); len(vs) != 0 {
		t.Fatalf("not lowered: %v", vs)
	}
	if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
		t.Fatalf("not decomposed: %v", vs)
	}
}

// ---------------------------------------------------------------------------
// The corpus
//
// specs/025's multi-word section names the number this pass exists to move:
// 3,164 of 3,483 refusals over the distribution corpus were a value wider than
// a register. This is that measurement, taken the way ssa/rules takes it, so
// that the two numbers are comparable.

type decCorpusPkg struct {
	pkg   *types2.Package
	files []*syntax.File
	info  *types2.Info
	err   error
}

type decCorpusImporter struct {
	fset *syntax.FileSet
	done map[string]*decCorpusPkg
}

func newDecCorpusImporter() *decCorpusImporter {
	return &decCorpusImporter{fset: syntax.NewFileSet(), done: make(map[string]*decCorpusPkg)}
}

func (imp *decCorpusImporter) Import(path string) (*types2.Package, error) {
	r := imp.check(path)
	return r.pkg, r.err
}

func (imp *decCorpusImporter) check(path string) *decCorpusPkg {
	if have, ok := imp.done[path]; ok {
		if have == nil {
			return &decCorpusPkg{err: fmt.Errorf("import cycle at %s", path)}
		}
		return have
	}
	if path == "unsafe" {
		r := &decCorpusPkg{pkg: types2.Unsafe}
		imp.done[path] = r
		return r
	}
	imp.done[path] = nil
	r := &decCorpusPkg{}
	imp.done[path] = r

	bp, err := build.Import(path, "", 0)
	if err != nil {
		for _, prefix := range []string{"vendor/", "cmd/vendor/"} {
			if bp2, err2 := build.Import(prefix+path, "", 0); err2 == nil {
				bp, err = bp2, nil
				break
			}
		}
	}
	if err != nil {
		r.err = err
		return r
	}
	if len(bp.CgoFiles) > 0 || len(bp.GoFiles) == 0 {
		r.err = fmt.Errorf("%s has no plain Go files", path)
		return r
	}
	for _, name := range bp.GoFiles {
		f, err := syntax.ParseFile(imp.fset, filepath.Join(bp.Dir, name), nil, nil, 0)
		if err != nil || f == nil {
			r.err = fmt.Errorf("parse %s: %v", name, err)
			return r
		}
		r.files = append(r.files, f)
	}
	r.info = &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{Fset: imp.fset, Importer: imp, Sizes: types2.SizesFor("gc", "arm64")}
	r.pkg, r.err = conf.Check(path, r.files, r.info)
	return r
}

// decomposeCounts is what one run of the corpus produced.
type decomposeCounts struct {
	pkgs    int
	built   int
	lowered int

	// refused counts the functions lowering refused, by the operation it
	// named. undecomposed counts the ones that still hold a value wider than
	// a register, by the operation that produces it: OpArg and OpSelectN are
	// pseudo-operations, so one of those survives lowering without a word and
	// would otherwise be counted as a function that lowered completely.
	refused      map[string]int
	undecomposed map[string]int

	// wide counts the types that were not decomposed, by kind, which is what
	// says whether the bound or a missing decForm is the reason.
	wide map[string]int

	// selectNAbove0 counts composite call results read at an index other than
	// zero. The pass numbers a part of result i as word i+j, which is only
	// correct for the first result, so it splits nothing else. This turns that
	// premise into a measurement instead of a reading of specs/021.
	selectNAbove0 int

	verifyNG int

	// abiNG counts the functions specs/030-abi.md's assignment refused, which
	// today is a result the result registers cannot hold.
	abiNG int
}

func decCorpusPaths(t *testing.T, src string, all bool) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != src && (name == "testdata" || name == "vendor" ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(paths)
	if all {
		return paths
	}
	// The unattended run takes a sample and leaves the tool chain out, as the
	// corpus test of ssa/rules does. CI sets NANOGO_REQUIRE_CORPUS.
	var library []string
	for _, p := range paths {
		if !strings.HasPrefix(p, "cmd/") && p != "cmd" && p != "unsafe" {
			library = append(library, p)
		}
	}
	const sample = 40
	if len(library) > sample {
		step := len(library) / sample
		var taken []string
		for i := 0; i < len(library); i += step {
			taken = append(taken, library[i])
		}
		library = taken
	}
	return library
}

// TestDecomposeCorpus is the headline measurement: how many of the standard
// library's functions lower completely once values wider than a register are
// split.
func TestDecomposeCorpus(t *testing.T) {
	required := os.Getenv("NANOGO_REQUIRE_CORPUS") == "1"
	src := filepath.Join(runtime.GOROOT(), "src")
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		if required {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s is not there", src)
		}
		t.Skipf("no corpus at %s", src)
	}

	imp := newDecCorpusImporter()
	c := &decomposeCounts{
		refused:      make(map[string]int),
		undecomposed: make(map[string]int),
		wide:         make(map[string]int),
	}
	for _, path := range decCorpusPaths(t, src, required) {
		if path == "unsafe" {
			continue
		}
		r := imp.check(path)
		if r.err != nil || r.pkg == nil {
			continue
		}
		pkg, _ := ir.Build(r.pkg, r.files, r.info)
		if pkg == nil {
			continue
		}
		c.pkgs++
		fns := append(append([]*ir.Func{}, pkg.Funcs...), pkg.Inits...)
		for _, fn := range fns {
			decomposeOne(t, path, fn, c)
		}
	}

	t.Logf("decompose corpus: %d packages, %d functions reached SSA, %d lowered completely",
		c.pkgs, c.built, c.lowered)
	decLogCounts(t, "unlowered", c.refused)
	decLogCounts(t, "still wider than a register: operation", c.undecomposed)
	decLogCounts(t, "still wider than a register: type", c.wide)
	t.Logf("the ABI assignment refused %d functions", c.abiNG)
	t.Logf("composite call results read at an index above zero: %d", c.selectNAbove0)
	if c.selectNAbove0 != 0 {
		t.Errorf("%d composite results are read at an index above zero; the part numbering of SelectN is wrong for them",
			c.selectNAbove0)
	}

	if c.verifyNG > 0 {
		t.Errorf("%d functions did not verify after decomposition", c.verifyNG)
	}
	if c.built == 0 {
		t.Fatal("the corpus produced no function")
	}
	// The measurement this pass was written to move. Before it, 4,755 of
	// 8,238 lowered; a regression below three quarters means the pass stopped
	// doing its work rather than that the corpus changed.
	if c.lowered*4 < c.built*3 {
		t.Errorf("only %d of %d functions lowered", c.lowered, c.built)
	}
	if required && c.built < 1000 {
		t.Fatalf("only %d functions reached SSA; the corpus collapsed", c.built)
	}
}

// decRefusedKind names the kind of the value lowering refused, which is what
// separates a value that is still too wide from one that has no rule for
// another reason, such as floating point.
func decRefusedKind(f *ssa.Func, id ssa.ID) string {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.ID != id {
				continue
			}
			t := v.Type
			if v.Op == ssa.OpStore && len(v.Args) == 3 {
				t = v.Args[1].Type
			}
			if t == nil {
				return "?"
			}
			return t.Kind.String()
		}
	}
	return "?"
}

func decLogCounts(t *testing.T, what string, m map[string]int) {
	t.Helper()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%s %s in %d functions", what, k, m[k])
	}
}

// decomposeOne builds, decomposes and lowers one function.
func decomposeOne(t *testing.T, path string, fn *ir.Func, c *decomposeCounts) {
	t.Helper()
	f, err := ssa.Build(fn)
	if err != nil || f == nil {
		return
	}
	c.built++
	if vs := ssa.Verify(f); len(vs) != 0 {
		// Construction is not this pass's problem and specs/021's own corpus
		// test owns it.
		return
	}

	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == ssa.OpSelectN && v.AuxInt != 0 && ssa.Multiword(v.Type) {
				c.selectNAbove0++
			}
		}
	}

	ssa.Decompose(f)
	if vs := ssa.Verify(f); len(vs) != 0 {
		c.verifyNG++
		if c.verifyNG < 4 {
			t.Errorf("%s: %s did not verify after decomposition: %v", path, fn.Name, vs)
		}
		return
	}
	// specs/030-abi.md's assignment walk runs between decomposition and
	// selection, so the pipeline this measures has it: a value that crosses a
	// call boundary in registers is split there and nowhere else, and a
	// measurement taken without it would not be the pipeline that compiles.
	if err := ssa.AssignABI(f, ssa.NewArm64Target()); err != nil {
		c.abiNG++
		return
	}
	if vs := ssa.Verify(f); len(vs) != 0 {
		c.verifyNG++
		if c.verifyNG < 4 {
			t.Errorf("%s: %s did not verify after the ABI pass: %v", path, fn.Name, vs)
		}
		return
	}
	seen := make(map[string]bool)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !ssa.Multiword(v.Type) {
				continue
			}
			if !seen[v.Op.String()] {
				seen[v.Op.String()] = true
				c.undecomposed[v.Op.String()]++
			}
			k := v.Type.Kind.String()
			if !seen[k] {
				seen[k] = true
				c.wide[k]++
			}
		}
	}

	ok := func() (ok bool) {
		defer func() {
			e := recover()
			if e == nil {
				return
			}
			le, isLower := e.(*ssa.LowerError)
			if !isLower {
				t.Fatalf("%s: %s: lowering panicked: %v\n%s", path, fn.Name, e, debug.Stack())
			}
			c.refused[le.Op.String()+" <"+decRefusedKind(f, le.Value)+">"]++
			ok = false
		}()
		ssa.Lower(f, rules.ARM64)
		return true
	}()
	if !ok {
		return
	}
	if vs := ssa.Verify(f); len(vs) != 0 {
		c.verifyNG++
		if c.verifyNG < 4 {
			t.Errorf("%s: %s did not verify after lowering: %v", path, fn.Name, vs)
		}
		return
	}
	c.lowered++
}

// ---------------------------------------------------------------------------
// Corners

// TestDecomposeEqualityChain covers the middle of the join chain: three parts
// need a join that reads a join.
func TestDecomposeEqualityChain(t *testing.T) {
	arr3 := decLaid(&ir.Type{Kind: ir.Array, Elem: decInt, Len: 3, Name: "[3]int"})
	p := newDecFn()
	eq := p.v(ssa.OpEq, decBool, p.load(arr3), p.load(arr3))
	f := p.ret(eq)
	ssa.Decompose(f)
	decVerified(t, f)

	eqs, ands := 0, 0
	for _, v := range p.b.Values {
		switch v.Op {
		case ssa.OpEq:
			eqs++
		case ssa.OpAnd:
			ands++
		}
	}
	if eqs != 3 || ands != 2 {
		t.Errorf("three parts gave %d comparisons and %d joins, want 3 and 2", eqs, ands)
	}
	if eq.Op != ssa.OpAnd || eq.Args[0].Op != ssa.OpAnd {
		t.Errorf("the result is %s and does not read a join", eq.LongString())
	}
}

// TestDecomposeZeroOfEveryPartKind covers the zero of a bool part, which is
// the one part kind that is neither an integer, a float, nor a pointer.
func TestDecomposeZeroOfEveryPartKind(t *testing.T) {
	mixed := decLaid(&ir.Type{Kind: ir.Struct, Name: "mixed", Fields: []ir.Field{
		{Name: "B", Type: decBool},
		{Name: "P", Type: decPtr},
		{Name: "F", Type: decF64},
		{Name: "I", Type: decI32},
	}})
	p := newDecFn()
	f := p.ret(p.v(ssa.OpConstNil, mixed))
	ssa.Decompose(f)
	decVerified(t, f)
	var got []string
	for _, v := range p.b.Values {
		switch v.Op {
		case ssa.OpConstBool, ssa.OpConstNil, ssa.OpConstFloat, ssa.OpConstInt:
			got = append(got, v.Op.String())
		}
	}
	want := "ConstBool ConstNil ConstFloat ConstInt"
	if strings.Join(got, " ") != want {
		t.Errorf("zero parts are %q, want %q", strings.Join(got, " "), want)
	}
}

// TestDecomposePhiUnsupportedArg keeps a value whole when a phi that reads it
// cannot be split. Splitting the value and not the phi would leave the phi
// reading a value that no longer exists.
func TestDecomposePhiUnsupportedArg(t *testing.T) {
	f := ssa.NewFunc("f")
	entry := f.Entry
	entry.Kind = ssa.BlockIf
	mem := entry.NewValue(0, ssa.OpInitMem, ssa.MemType)
	ptr := entry.NewValue(0, ssa.OpArg, decPtr)
	entry.Control = entry.NewValue(0, ssa.OpArg, decBool)

	left := f.NewBlock(ssa.BlockPlain)
	right := f.NewBlock(ssa.BlockPlain)
	join := f.NewBlock(ssa.BlockRet)
	entry.AddEdgeTo(left)
	entry.AddEdgeTo(right)
	left.AddEdgeTo(join)
	right.AddEdgeTo(join)

	good := left.NewValue(0, ssa.OpLoad, decStr, ptr, mem)
	bad := right.NewValue(0, ssa.OpBitcast, decStr, ptr)
	phi := join.NewValue(0, ssa.OpPhi, decStr, good, bad)
	join.Control = join.NewValue(0, ssa.OpMakeResult, ssa.MemType, phi, mem)

	ssa.Decompose(f)
	decVerified(t, f)
	if good.Op != ssa.OpLoad || good.Type != decStr {
		t.Errorf("the load was split although the phi over it was not: %s", good.LongString())
	}
	if phi.Op != ssa.OpPhi || phi.Type != decStr {
		t.Errorf("the phi was split: %s", phi.LongString())
	}
}

func TestStringSymString(t *testing.T) {
	var nilSym *ssa.StringSym
	if nilSym.String() != "<nil>" {
		t.Errorf("a nil symbol prints %q", nilSym.String())
	}
	if got := (&ssa.StringSym{}).String(); got != "<nil>" {
		t.Errorf("a symbol with no object prints %q", got)
	}
}

// ---------------------------------------------------------------------------
// String equality, which is a call and not a comparison of the parts

// stringEqFn returns a function that compares two loaded strings.
func stringEqFn(op ssa.Op) (*decFn, *ssa.Value) {
	p := newDecFn()
	eq := p.v(op, decBool, p.load(decStr), p.load(decStr))
	return p, eq
}

// TestDecomposeStringEqualForm asserts the whole expansion, so that a change
// to it is reviewable.
//
// specs/020-ir.md's table: runtime.memequal plus a length check. The mask is
// what makes one call safe for both answers, and it is the part of the shape
// that is easy to get wrong: without it the call reads len(x) bytes out of a
// shorter y.
func TestDecomposeStringEqualForm(t *testing.T) {
	p, _ := stringEqFn(ssa.OpEq)
	f := p.ret(p.b.Values[len(p.b.Values)-1])
	ssa.Decompose(f)
	decVerified(t, f)
	decWantForm(t, f, `
b0:
    v0 = InitMem <mem>
    v1 = Arg <*int> {p}
    v8 = OffPtr <*int> [8] v1
    v7 = Load <*uint8> v1 v0
    v9 = Load <int> v8 v0
    v3 = Arg <*int> {p}
    v11 = OffPtr <*int> [8] v3
    v10 = Load <*uint8> v3 v0
    v12 = Load <int> v11 v0
    v13 = Eq <bool> v9 v12
    v14 = ZeroExt <int> v13
    v15 = Neg <int> v14
    v16 = And <int> v9 v15
    v17 = StaticCall <mem> {runtime.memequal} v7 v10 v16 v0
    v18 = SelectN <bool> [0] v17
    v19 = ZeroExt <bool> v18
    v5 = And <bool> v13 v19
    v6 = MakeResult <mem> v5 v17
  Ret v6`)
	if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
		t.Errorf("a value wider than a register survived: %v", vs)
	}
}

// TestDecomposeStringEqualLowers takes the expansion through selection.
//
// The call is the point: lowering introduces one into a block that had none,
// which is why specs/027-liveness-and-stackmaps.md runs after this pass.
func TestDecomposeStringEqualLowers(t *testing.T) {
	for _, op := range []ssa.Op{ssa.OpEq, ssa.OpNeq} {
		p, eq := stringEqFn(op)
		f := p.ret(eq)
		ssa.Lower(f, rules.ARM64)
		decVerified(t, f)
		if vs := ssa.CheckLowered(f, rules.ARM64); len(vs) != 0 {
			t.Fatalf("%v: %v", op, vs)
		}
		calls := 0
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op != ssa.OpARM64CALLstatic {
					continue
				}
				calls++
				o, _ := v.Aux.(*ir.Object)
				if o == nil || o.Name != "runtime.memequal" {
					t.Errorf("%v: the call is to %v", op, v.Aux)
				}
				// The pointers of both strings, the masked length, and the
				// memory the call is ordered after.
				if len(v.Args) != 4 {
					t.Errorf("%v: the call takes %d arguments, want 4: %s", op, len(v.Args), v.LongString())
				}
			}
		}
		if calls != 1 {
			t.Errorf("%v: %d calls, want one", op, calls)
		}
	}
}

// TestDecomposeStringNotEqual asserts that != is the negation of ==.
//
// Not lowers to one bit flipped, which is only the negation of a value that is
// exactly 0 or 1. The And in front of it is what guarantees that, because a
// comparison answers 0 or 1 and an And with it cannot answer anything else.
func TestDecomposeStringNotEqual(t *testing.T) {
	p, ne := stringEqFn(ssa.OpNeq)
	f := p.ret(ne)
	ssa.Decompose(f)
	decVerified(t, f)
	if ne.Op != ssa.OpNot || len(ne.Args) != 1 || ne.Args[0].Op != ssa.OpAnd {
		t.Fatalf("!= became %s", ne.LongString())
	}
	and := ne.Args[0]
	if and.Args[0].Op != ssa.OpEq {
		t.Errorf("the length check is %s", and.Args[0].LongString())
	}
}

// TestDecomposeStringEqualConstant is the common shape: s == "abc". The
// constant's parts are its symbol and its length, and both reach the call.
func TestDecomposeStringEqualConstant(t *testing.T) {
	p := newDecFn()
	k := p.v(ssa.OpConstString, decStr)
	k.Aux = "abc"
	eq := p.v(ssa.OpEq, decBool, p.load(decStr), k)
	f := p.ret(eq)
	ssa.Decompose(f)
	decVerified(t, f)

	var call *ssa.Value
	for _, v := range p.b.Values {
		if v.Op == ssa.OpStaticCall {
			call = v
		}
	}
	if call == nil {
		t.Fatal("no call to runtime.memequal")
	}
	if call.Args[1].Op != ssa.OpAddr {
		t.Errorf("the constant's data is %s", call.Args[1].LongString())
	}
	// The length the call is given is the mask of the first string's length,
	// not the constant's: the two are equal exactly when the lengths agree,
	// and that is what the mask says.
	if call.Args[2].Op != ssa.OpAnd {
		t.Errorf("the length is %s", call.Args[2].LongString())
	}
}

// TestDecomposeStringEqualIdempotent runs the pass twice.
//
// ssa.Lower calls Decompose itself, so every function this pass touches is
// decomposed twice in the corpus. The second run must find nothing to do.
func TestDecomposeStringEqualIdempotent(t *testing.T) {
	p, eq := stringEqFn(ssa.OpEq)
	f := p.ret(eq)
	ssa.Decompose(f)
	decVerified(t, f)
	before := decForm(f)
	ssa.Decompose(f)
	decVerified(t, f)
	if got := decForm(f); got != before {
		t.Errorf("the second pass changed the function\nfirst:\n%s\nsecond:\n%s", before, got)
	}
}

// TestDecomposeStringEqualBetweenCallAndResult covers the refusal.
//
// SelectN names a call result by reading the call, and the verifier reads that
// argument as the memory the read is ordered against. A new call between the
// two would leave the read naming memory that is no longer live, so the
// expansion declines and the strings stay whole rather than the graph breaking.
func TestDecomposeStringEqualBetweenCallAndResult(t *testing.T) {
	p := newDecFn()
	ptr := p.arg(decPtr, "p")
	call := p.v(ssa.OpStaticCall, ssa.MemType, p.mem)
	call.Aux = ssa.RuntimeFunc("runtime.newobject")
	x := p.v(ssa.OpLoad, decStr, ptr, call)
	y := p.v(ssa.OpLoad, decStr, ptr, call)
	eq := p.v(ssa.OpEq, decBool, x, y)
	res := p.v(ssa.OpSelectN, decBool, call)
	p.mem = call
	f := p.ret(eq, res)
	decVerified(t, f)

	ssa.Decompose(f)
	decVerified(t, f)
	if eq.Op != ssa.OpEq {
		t.Errorf("the comparison was expanded where it cannot be: %s", eq.LongString())
	}
	if len(ssa.CheckDecomposed(f)) == 0 {
		t.Error("the operands were split for a comparison that was not built")
	}
}

// ---------------------------------------------------------------------------
// The memory chain a call has to be spliced into

// TestDecomposeMemoryPhiAtJoin covers the phi the expansion forces.
//
// The comparison is on one arm of a branch, so that arm leaves with the call's
// memory and the other leaves with the memory before it. A join whose
// predecessors disagree needs a memory phi, and nothing above this pass knows
// the call is coming.
func TestDecomposeMemoryPhiAtJoin(t *testing.T) {
	f := ssa.NewFunc("f")
	entry := f.Entry
	entry.Kind = ssa.BlockIf
	mem := entry.NewValue(0, ssa.OpInitMem, ssa.MemType)
	ptr := entry.NewValue(0, ssa.OpArg, decPtr)
	entry.Control = entry.NewValue(0, ssa.OpArg, decBool)

	left := f.NewBlock(ssa.BlockPlain)
	right := f.NewBlock(ssa.BlockPlain)
	join := f.NewBlock(ssa.BlockRet)
	entry.AddEdgeTo(left)
	entry.AddEdgeTo(right)
	left.AddEdgeTo(join)
	right.AddEdgeTo(join)

	x := left.NewValue(0, ssa.OpLoad, decStr, ptr, mem)
	y := left.NewValue(0, ssa.OpLoad, decStr, ptr, mem)
	eq := left.NewValue(0, ssa.OpEq, decBool, x, y)
	no := right.NewValue(0, ssa.OpConstBool, decBool)
	phi := join.NewValue(0, ssa.OpPhi, decBool, eq, no)
	join.Control = join.NewValue(0, ssa.OpMakeResult, ssa.MemType, phi, mem)

	decVerified(t, f)
	ssa.Decompose(f)
	decVerified(t, f)

	var memPhi *ssa.Value
	for _, v := range join.Values {
		if v.Op == ssa.OpPhi && v.Type == ssa.MemType {
			memPhi = v
		}
	}
	if memPhi == nil {
		t.Fatalf("the join has no memory phi:\n%s", f)
	}
	if len(memPhi.Args) != 2 {
		t.Fatalf("the memory phi has %d arguments: %s", len(memPhi.Args), memPhi.LongString())
	}
	if memPhi.Args[0].Op != ssa.OpStaticCall {
		t.Errorf("the arm that calls leaves with %s", memPhi.Args[0].LongString())
	}
	if memPhi.Args[1] != mem {
		t.Errorf("the arm that does not call leaves with %s", memPhi.Args[1].LongString())
	}
	if got := join.Control.Args[len(join.Control.Args)-1]; got != memPhi {
		t.Errorf("the return reads %s and not the phi", got.LongString())
	}
}

// TestDecomposeMemoryChainInLoop covers the round the fixed point needs.
//
// The call is in the body, so the loop header's predecessors disagree only
// once the body has been walked, and the phi that merges them changes the
// memory the header leaves with, which the body then reads. One walk cannot
// see that, which is why the dataflow runs to a fixed point.
func TestDecomposeMemoryChainInLoop(t *testing.T) {
	f := ssa.NewFunc("f")
	entry := f.Entry
	entry.Kind = ssa.BlockPlain
	mem := entry.NewValue(0, ssa.OpInitMem, ssa.MemType)
	ptr := entry.NewValue(0, ssa.OpArg, decPtr)

	head := f.NewBlock(ssa.BlockIf)
	body := f.NewBlock(ssa.BlockPlain)
	exit := f.NewBlock(ssa.BlockRet)
	entry.AddEdgeTo(head)
	head.AddEdgeTo(body)
	head.AddEdgeTo(exit)
	body.AddEdgeTo(head)
	head.Control = head.NewValue(0, ssa.OpArg, decBool)

	x := body.NewValue(0, ssa.OpLoad, decStr, ptr, mem)
	y := body.NewValue(0, ssa.OpLoad, decStr, ptr, mem)
	body.NewValue(0, ssa.OpEq, decBool, x, y)
	exit.Control = exit.NewValue(0, ssa.OpMakeResult, ssa.MemType, mem)

	decVerified(t, f)
	ssa.Decompose(f)
	decVerified(t, f)

	var memPhi *ssa.Value
	for _, v := range head.Values {
		if v.Op == ssa.OpPhi && v.Type == ssa.MemType {
			memPhi = v
		}
	}
	if memPhi == nil {
		t.Fatalf("the loop header has no memory phi:\n%s", f)
	}
	if len(memPhi.Args) != 2 || memPhi.Args[0] != mem || memPhi.Args[1].Op != ssa.OpStaticCall {
		t.Errorf("the header's phi is %s", memPhi.LongString())
	}
	if got := exit.Control.Args[len(exit.Control.Args)-1]; got != memPhi {
		t.Errorf("the exit reads %s and not the header's phi", got.LongString())
	}
}

// ---------------------------------------------------------------------------
// A slice against nil

// TestDecomposeSliceNilComparesPointer asserts the comparison is the data
// pointer alone.
//
// It is not a per-part comparison. A slice whose pointer is nil and whose
// length is not is a value unsafe can build, and gc answers true for it: one
// CMP against zero on the first word is the whole comparison. specs/000
// decision 11 makes a difference from gc a nanogo bug.
func TestDecomposeSliceNilComparesPointer(t *testing.T) {
	for _, op := range []ssa.Op{ssa.OpEq, ssa.OpNeq} {
		p := newDecFn()
		cmp := p.v(op, decBool, p.load(decSlice), p.v(ssa.OpConstNil, decSlice))
		f := p.ret(cmp)
		ssa.Decompose(f)
		decVerified(t, f)
		if cmp.Op != op || len(cmp.Args) != 2 {
			t.Fatalf("%v became %s", op, cmp.LongString())
		}
		if cmp.Args[0].Type.Kind != ir.Ptr || cmp.Args[1].Op != ssa.OpConstNil {
			t.Errorf("%v compares %s", op, cmp.LongString())
		}
		if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
			t.Errorf("%v: %v", op, vs)
		}
		ssa.Lower(f, rules.ARM64)
		if vs := ssa.CheckLowered(f, rules.ARM64); len(vs) != 0 {
			t.Errorf("%v: %v", op, vs)
		}
	}
}

// ---------------------------------------------------------------------------
// The symbol table

// TestRuntimeFunc covers the lookup that keeps every generated call inside
// rtsym, which specs/031-runtime-lowering.md requires to be checked against
// the runtime's source.
func TestRuntimeFunc(t *testing.T) {
	o := ssa.RuntimeFunc("runtime.memequal")
	if o == nil || o.Name != "runtime.memequal" || o.Class != ir.ClassFunc {
		t.Fatalf("runtime.memequal is %v", o)
	}
	if ssa.RuntimeFunc("runtime.memequal") != o {
		t.Error("two calls to one symbol named two objects, so they are two relocations")
	}
	defer func() {
		if recover() == nil {
			t.Error("a symbol that is not in rtsym did not panic")
		}
	}()
	ssa.RuntimeFunc("runtime.notasymbol")
}

// TestDecomposeStringEqualAsControl covers the shape the corpus is mostly
// made of: if s == "x". The comparison is the block's control, so the value
// has to keep its identity through the expansion or the branch loses it.
func TestDecomposeStringEqualAsControl(t *testing.T) {
	f := ssa.NewFunc("f")
	entry := f.Entry
	entry.Kind = ssa.BlockIf
	mem := entry.NewValue(0, ssa.OpInitMem, ssa.MemType)
	ptr := entry.NewValue(0, ssa.OpArg, decPtr)
	k := entry.NewValue(0, ssa.OpConstString, decStr)
	k.Aux = "x"
	eq := entry.NewValue(0, ssa.OpEq, decBool, entry.NewValue(0, ssa.OpLoad, decStr, ptr, mem), k)
	entry.Control = eq

	yes := f.NewBlock(ssa.BlockRet)
	no := f.NewBlock(ssa.BlockRet)
	entry.AddEdgeTo(yes)
	entry.AddEdgeTo(no)
	yes.Control = yes.NewValue(0, ssa.OpMakeResult, ssa.MemType, mem)
	no.Control = no.NewValue(0, ssa.OpMakeResult, ssa.MemType, mem)

	decVerified(t, f)
	ssa.Decompose(f)
	decVerified(t, f)
	if entry.Control != eq || eq.Op != ssa.OpAnd {
		t.Fatalf("the control is %s", entry.Control.LongString())
	}
	// Both arms leave with the memory the call produced, so no phi is needed
	// and the returns must read it rather than the memory before the call.
	for _, b := range []*ssa.Block{yes, no} {
		got := b.Control.Args[len(b.Control.Args)-1]
		if got.Op != ssa.OpStaticCall {
			t.Errorf("b%d returns with %s", b.ID, got.LongString())
		}
	}
	ssa.Lower(f, rules.ARM64)
	if vs := ssa.CheckLowered(f, rules.ARM64); len(vs) != 0 {
		t.Errorf("%v", vs)
	}
	decVerified(t, f)
}

// ---------------------------------------------------------------------------
// String ordering, which is runtime.cmpstring and a comparison against zero

// TestDecomposeStringOrderForm asserts the whole expansion of <, and that the
// comparison keeps its operation so that <= differs by nothing else.
func TestDecomposeStringOrderForm(t *testing.T) {
	p := newDecFn()
	lt := p.v(ssa.OpLess, decBool, p.load(decStr), p.load(decStr))
	f := p.ret(lt)
	ssa.Decompose(f)
	decVerified(t, f)
	decWantForm(t, f, `
b0:
    v0 = InitMem <mem>
    v1 = Arg <*int> {p}
    v8 = OffPtr <*int> [8] v1
    v7 = Load <*uint8> v1 v0
    v9 = Load <int> v8 v0
    v3 = Arg <*int> {p}
    v11 = OffPtr <*int> [8] v3
    v10 = Load <*uint8> v3 v0
    v12 = Load <int> v11 v0
    v13 = StaticCall <mem> {runtime.cmpstring} v7 v9 v10 v12 v0
    v14 = SelectN <int> [0] v13
    v15 = ConstInt <int> [0]
    v5 = Less <bool> v14 v15
    v6 = MakeResult <mem> v5 v13
  Ret v6`)
	if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
		t.Errorf("a value wider than a register survived: %v", vs)
	}
}

// TestDecomposeStringOrderLowers takes both spellings through selection.
//
// specs/021 canonicalises a comparison, so > and >= arrive here as < and <=
// with the arguments exchanged and need no rule of their own. The comparison
// against zero is signed, which is what cmpstring's result requires: it
// answers a negative number, zero, or a positive one.
func TestDecomposeStringOrderLowers(t *testing.T) {
	for _, op := range []ssa.Op{ssa.OpLess, ssa.OpLeq} {
		p := newDecFn()
		cmp := p.v(op, decBool, p.load(decStr), p.load(decStr))
		f := p.ret(cmp)
		ssa.Lower(f, rules.ARM64)
		decVerified(t, f)
		if vs := ssa.CheckLowered(f, rules.ARM64); len(vs) != 0 {
			t.Fatalf("%v: %v", op, vs)
		}
		calls := 0
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op != ssa.OpARM64CALLstatic {
					continue
				}
				calls++
				o, _ := v.Aux.(*ir.Object)
				if o == nil || o.Name != "runtime.cmpstring" {
					t.Errorf("%v: the call is to %v", op, v.Aux)
				}
				// The two string headers, four words, and memory.
				if len(v.Args) != 5 {
					t.Errorf("%v: the call takes %d arguments, want 5: %s", op, len(v.Args), v.LongString())
				}
			}
		}
		if calls != 1 {
			t.Errorf("%v: %d calls, want one", op, calls)
		}
	}
}

// TestDecomposeStringOrderIsNotPerPart is the miscompile this expansion
// replaces.
//
// A per-part < would compare the two data pointers, which orders strings by
// where they happen to sit in memory. The pass must build the call or leave
// the operands whole for lowering to refuse; it must never compare a part.
func TestDecomposeStringOrderIsNotPerPart(t *testing.T) {
	p := newDecFn()
	lt := p.v(ssa.OpLess, decBool, p.load(decStr), p.load(decStr))
	f := p.ret(lt)
	ssa.Decompose(f)
	if lt.Args[0].Type.Kind != ir.Int64 || lt.Args[1].Op != ssa.OpConstInt {
		t.Fatalf("< compares %s", lt.LongString())
	}
}

// ---------------------------------------------------------------------------
// Interface equality, which is the dynamic type's equality function

// decEface is an interface with no methods, whose first word is a *_type
// rather than an *itab. It is the same size and the same pointer map as
// decIface and a different runtime symbol, which is the whole reason
// ir.Type.EmptyIface exists.
var decEface = decLaid(&ir.Type{Kind: ir.Interface, Name: "any", EmptyIface: true})

// TestDecomposeIfaceEqualForm asserts the whole expansion, so that a change to
// it is reviewable.
//
// The masked descriptor is the part that is easy to get wrong and impossible
// to see: it is the itab when the two agree and nil when they do not, and a
// nil descriptor is what makes one unconditional call answer both cases.
func TestDecomposeIfaceEqualForm(t *testing.T) {
	p := newDecFn()
	eq := p.v(ssa.OpEq, decBool, p.load(decIface), p.load(decIface))
	f := p.ret(eq)
	ssa.Decompose(f)
	decVerified(t, f)
	decWantForm(t, f, `
b0:
    v0 = InitMem <mem>
    v1 = Arg <*int> {p}
    v8 = OffPtr <*unsafe.Pointer> [8] v1
    v7 = Load <unsafe.Pointer> v1 v0
    v9 = Load <unsafe.Pointer> v8 v0
    v3 = Arg <*int> {p}
    v11 = OffPtr <*unsafe.Pointer> [8] v3
    v10 = Load <unsafe.Pointer> v3 v0
    v12 = Load <unsafe.Pointer> v11 v0
    v13 = Eq <bool> v7 v10
    v14 = ZeroExt <int> v13
    v15 = Neg <int> v14
    v16 = And <unsafe.Pointer> v7 v15
    v17 = StaticCall <mem> {runtime.ifaceeq} v16 v9 v12 v0
    v18 = SelectN <bool> [0] v17
    v19 = ZeroExt <bool> v18
    v5 = And <bool> v13 v19
    v6 = MakeResult <mem> v5 v17
  Ret v6`)
	if vs := ssa.CheckDecomposed(f); len(vs) != 0 {
		t.Errorf("a value wider than a register survived: %v", vs)
	}
}

// TestDecomposeIfaceEqualPicksTheSymbol is the one that would be a
// corruption rather than a wrong answer.
//
// The first word of an interface with methods is an *itab and of one without
// is a *_type. efaceeq reads Equal out of that word's type descriptor and
// ifaceeq reads the descriptor out of the itab first. Calling the wrong one
// reads a function pointer at the wrong offset and jumps through it.
func TestDecomposeIfaceEqualPicksTheSymbol(t *testing.T) {
	tests := []struct {
		name string
		typ  *ir.Type
		want string
	}{
		{"with methods", decIface, "runtime.ifaceeq"},
		{"without methods", decEface, "runtime.efaceeq"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newDecFn()
			f := p.ret(p.v(ssa.OpEq, decBool, p.load(tc.typ), p.load(tc.typ)))
			ssa.Decompose(f)
			decVerified(t, f)
			var got string
			for _, v := range p.b.Values {
				if v.Op == ssa.OpStaticCall {
					o, _ := v.Aux.(*ir.Object)
					if o != nil {
						got = o.Name
					}
				}
			}
			if got != tc.want {
				t.Errorf("the call is to %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecomposeIfaceEqualMixedIsRefused covers the comparison of an empty
// interface with a non-empty one.
//
// Go allows it, because each is assignable to the other, and the two first
// words are then a *_type and an *itab: two words that never compare equal and
// two symbols that read them differently. The conversion that makes them one
// type belongs above SSA, so this pass refuses and lowering names it.
func TestDecomposeIfaceEqualMixedIsRefused(t *testing.T) {
	p := newDecFn()
	eq := p.v(ssa.OpEq, decBool, p.load(decIface), p.load(decEface))
	f := p.ret(eq)
	ssa.Decompose(f)
	decVerified(t, f)
	if eq.Op != ssa.OpEq {
		t.Errorf("a mixed comparison was expanded: %s", eq.LongString())
	}
	if len(ssa.CheckDecomposed(f)) == 0 {
		t.Error("the operands were split for a comparison that was not built")
	}
}

// TestDecomposeIfaceEqualLowers takes both spellings through selection.
func TestDecomposeIfaceEqualLowers(t *testing.T) {
	for _, op := range []ssa.Op{ssa.OpEq, ssa.OpNeq} {
		p := newDecFn()
		cmp := p.v(op, decBool, p.load(decIface), p.load(decIface))
		f := p.ret(cmp)
		ssa.Lower(f, rules.ARM64)
		decVerified(t, f)
		if vs := ssa.CheckLowered(f, rules.ARM64); len(vs) != 0 {
			t.Fatalf("%v: %v", op, vs)
		}
		calls := 0
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op != ssa.OpARM64CALLstatic {
					continue
				}
				calls++
				// The masked descriptor, the two data words, and memory.
				if len(v.Args) != 4 {
					t.Errorf("%v: the call takes %d arguments, want 4: %s", op, len(v.Args), v.LongString())
				}
			}
		}
		if calls != 1 {
			t.Errorf("%v: %d calls, want one", op, calls)
		}
	}
}

// TestDecomposeIfaceNilNeedsNoCall keeps the cheap answer cheap.
//
// The zero interface is two zero words and nothing else is, so a comparison
// against the literal nil is the comparison of both parts and needs no runtime
// call at all. Routing it through ifaceeq would be correct and would put a
// call on the most common interface comparison there is.
func TestDecomposeIfaceNilNeedsNoCall(t *testing.T) {
	p := newDecFn()
	eq := p.v(ssa.OpEq, decBool, p.load(decIface), p.v(ssa.OpConstNil, decIface))
	f := p.ret(eq)
	ssa.Decompose(f)
	decVerified(t, f)
	for _, v := range p.b.Values {
		if v.Op == ssa.OpStaticCall {
			t.Fatalf("a comparison against nil called %v", v.Aux)
		}
	}
	if eq.Op != ssa.OpAnd {
		t.Errorf("the comparison became %s", eq.LongString())
	}
}
