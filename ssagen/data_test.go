// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"encoding/binary"
	"errors"
	"go/constant"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
)

// globalType lays a type out, because a symbol's size and alignment are the
// type's and ir.Layout is what computes them.
func globalType(t *testing.T, k ir.Kind, elem *ir.Type, n int64) *ir.Type {
	t.Helper()
	// The name is what makes a basic type a defined type to
	// ir.TypeLinkString, which is what names its descriptor. ir.Build's
	// converter fills it in for every basic type.
	ty := &ir.Type{Kind: k, Elem: elem, Len: n, Name: basicNames[k]}
	if err := ir.Layout(ty); err != nil {
		t.Fatalf("layout %v: %v", k, err)
	}
	return ty
}

// basicNames spells the basic types the way the type checker's universe does.
var basicNames = map[ir.Kind]string{
	ir.Bool: "bool", ir.Int8: "int8", ir.Int16: "int16", ir.Int32: "int32",
	ir.Int64: "int64", ir.Uint8: "uint8", ir.Uint16: "uint16",
	ir.Uint32: "uint32", ir.Uint64: "uint64", ir.Uintptr: "uintptr",
	ir.Float32: "float32", ir.Float64: "float64", ir.String: "string",
}

// globalPkg builds a package of one variable with the given constant
// initialiser, in the shape ir.Build produces: the variable is in Globals and
// the assignment is a statement of the synthesised initialisation function.
func globalPkg(name string, ty *ir.Type, init constant.Value) (*ir.Package, *ir.Object) {
	g := &ir.Object{Name: name, Type: ty, Class: ir.ClassGlobal}
	p := &ir.Package{Path: "main", Name: "main", Globals: []*ir.Object{g}}
	if init != nil {
		p.Inits = []*ir.Func{{Name: "init", Sym: "main.init", Body: []ir.Stmt{{
			Op: ir.OAssign,
			X:  &ir.Node{Op: ir.OGlobal, Type: ty, Obj: g},
			Y:  &ir.Node{Op: ir.OConst, Type: ty, Val: ir.Const{Val: init}},
		}}}}
	}
	return p, g
}

// TestGlobalWithAConstantHoldsItsValue checks that an initialiser the type
// checker reduced to a constant is written into the symbol's bytes.
func TestGlobalWithAConstantHoldsItsValue(t *testing.T) {
	ty := globalType(t, ir.Int64, nil, 0)
	p, _ := globalPkg("main.n", ty, constant.MakeInt64(42))
	out := obj.NewPackage("main")
	if _, err := AddGlobals(out, p); err != nil {
		t.Fatalf("AddGlobals: %v", err)
	}
	sym := out.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 0})
	if sym == nil || sym.Name != "main.n" {
		t.Fatalf("the object holds %v, want main.n", sym)
	}
	if sym.Size != 8 || len(sym.Data) != 8 {
		t.Fatalf("the symbol is %d bytes with %d of data", sym.Size, len(sym.Data))
	}
	if got := binary.LittleEndian.Uint64(sym.Data); got != 42 {
		t.Errorf("the symbol holds %d, want 42", got)
	}
	// No pointers, so the collector never scans it and it needs no
	// descriptor.
	if sym.Type != obj.SNOPTRDATA {
		t.Errorf("the symbol is of kind %d, want SNOPTRDATA", sym.Type)
	}
	if len(sym.Aux) != 0 {
		t.Errorf("the symbol carries %d auxiliary entries, want none", len(sym.Aux))
	}
}

// TestGlobalWithNoValueIsZeroFilled checks that a variable with no initialiser
// costs no bytes in the object.
//
// The linker allocates the space of a zero-filled symbol. Writing zeros
// instead would put the size of every zero variable into every object.
func TestGlobalWithNoValueIsZeroFilled(t *testing.T) {
	ty := globalType(t, ir.Int64, nil, 0)
	p, _ := globalPkg("main.zero", ty, nil)
	out := obj.NewPackage("main")
	if _, err := AddGlobals(out, p); err != nil {
		t.Fatalf("AddGlobals: %v", err)
	}
	sym := out.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 0})
	if sym.Type != obj.SNOPTRBSS {
		t.Errorf("the symbol is of kind %d, want SNOPTRBSS", sym.Type)
	}
	if sym.Size != 8 || len(sym.Data) != 0 {
		t.Errorf("the symbol is %d bytes with %d of data, want 8 and none", sym.Size, len(sym.Data))
	}
	// A constant that is the zero of its type is the same case: the bytes are
	// zero either way, and the linker writes them.
	p2, _ := globalPkg("main.zero", ty, constant.MakeInt64(0))
	out2 := obj.NewPackage("main")
	if _, err := AddGlobals(out2, p2); err != nil {
		t.Fatalf("AddGlobals: %v", err)
	}
	if got := out2.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 0}); got.Type != obj.SNOPTRBSS {
		t.Errorf("a constant zero is of kind %d, want SNOPTRBSS", got.Type)
	}
}

// TestGlobalStringPointsAtItsBytes checks the two words of a string variable.
//
// The first word is the address of a symbol the linker places, so it is a
// relocation and not a number. The second is the length, which is known now.
func TestGlobalStringPointsAtItsBytes(t *testing.T) {
	ty := globalType(t, ir.String, nil, 0)
	p, _ := globalPkg("main.greeting", ty, constant.MakeString("hello"))
	out := obj.NewPackage("main")
	types, err := AddGlobals(out, p)
	if err != nil {
		t.Fatalf("AddGlobals: %v", err)
	}
	sym := out.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 0})
	if sym.Size != 16 || len(sym.Data) != 16 {
		t.Fatalf("the header is %d bytes with %d of data, want 16 and 16", sym.Size, len(sym.Data))
	}
	if got := binary.LittleEndian.Uint64(sym.Data[8:]); got != 5 {
		t.Errorf("the length word is %d, want 5", got)
	}
	if len(sym.Relocs) != 1 {
		t.Fatalf("%d relocations, want one for the data pointer", len(sym.Relocs))
	}
	r := sym.Relocs[0]
	if r.Off != 0 || r.Size != 8 || r.Type != obj.R_ADDR {
		t.Errorf("the relocation is %+v, want an eight-byte address at offset 0", r)
	}
	data := out.Def(r.Sym)
	if data == nil || data.Name != `go:string."hello"` || string(data.Data) != "hello" {
		t.Errorf("the data pointer names %v, want the bytes of hello", data)
	}
	// A string holds a pointer, so the collector scans the symbol and reads
	// its pointer map through the type descriptor.
	if sym.Type != obj.SDATA {
		t.Errorf("the symbol is of kind %d, want SDATA", sym.Type)
	}
	if len(sym.Aux) != 1 || sym.Aux[0].Type != obj.AuxGotype {
		t.Errorf("the symbol carries %v, want one Gotype entry", sym.Aux)
	}
	if len(types) != 1 || types[0] != ty {
		t.Errorf("AddGlobals asked for %v descriptors, want the string's", types)
	}
}

// TestGlobalPointerIsScannedAndZero checks the fourth of the four kinds: a
// pointer with no initialiser is scanned and zero-filled.
func TestGlobalPointerIsScannedAndZero(t *testing.T) {
	ty := globalType(t, ir.Ptr, globalType(t, ir.Int64, nil, 0), 0)
	p, _ := globalPkg("main.p", ty, nil)
	out := obj.NewPackage("main")
	if _, err := AddGlobals(out, p); err != nil {
		t.Fatalf("AddGlobals: %v", err)
	}
	sym := out.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 0})
	if sym.Type != obj.SBSS {
		t.Errorf("the symbol is of kind %d, want SBSS", sym.Type)
	}
	if len(sym.Aux) != 1 || sym.Aux[0].Type != obj.AuxGotype {
		t.Errorf("the symbol carries %v, want one Gotype entry", sym.Aux)
	}
}

// TestGlobalWithNoDescriptorIsRefused checks that a variable the collector
// could not scan is refused rather than emitted.
//
// cmd/link builds the pointer bitmap of the data section from the descriptor
// each symbol names. A scanned symbol with no descriptor is a link error a
// long way from its cause, and a symbol quietly moved out of the scanned
// section is worse: the collector would not follow the pointer and the
// object it points at would be freed while it is live.
func TestGlobalWithNoDescriptorIsRefused(t *testing.T) {
	// A channel: rtype refuses the descriptor because the IR type carries no
	// direction.
	ty := globalType(t, ir.Chan, globalType(t, ir.Int64, nil, 0), 0)
	p, g := globalPkg("main.ch", ty, nil)
	_, err := AddGlobals(obj.NewPackage("main"), p)
	var ge *GlobalError
	if !errors.As(err, &ge) {
		t.Fatalf("AddGlobals gave %v, want a GlobalError", err)
	}
	if ge.Obj != g {
		t.Errorf("the refusal names %v, want the variable it is about", ge.Obj)
	}
	// CheckGlobals is the same answer without an object to write into, which
	// is what lets the driver refuse before it compiles a single function.
	if err := CheckGlobals(p); !errors.As(err, &ge) || ge.Obj != g {
		t.Errorf("CheckGlobals gave %v, want the same refusal", err)
	}
}

// TestGlobalsAreAddedInDeclarationOrder checks the property
// specs/053-determinism.md needs.
//
// The object's symbol table is written from a slice in insertion order, so two
// runs over one input must add the symbols in one order. Reading the
// initialisers out of a map and emitting in that order would give a different
// object every run.
func TestGlobalsAreAddedInDeclarationOrder(t *testing.T) {
	ty := globalType(t, ir.Int64, nil, 0)
	names := []string{"main.a", "main.b", "main.c", "main.d", "main.e"}
	p := &ir.Package{Path: "main", Name: "main"}
	fn := &ir.Func{Name: "init", Sym: "main.init"}
	for i, n := range names {
		g := &ir.Object{Name: n, Type: ty, Class: ir.ClassGlobal}
		p.Globals = append(p.Globals, g)
		fn.Body = append(fn.Body, &ir.Node{
			Op: ir.OAssign,
			X:  &ir.Node{Op: ir.OGlobal, Type: ty, Obj: g},
			Y:  &ir.Node{Op: ir.OConst, Type: ty, Val: ir.Const{Val: constant.MakeInt64(int64(i + 1))}},
		})
	}
	p.Inits = []*ir.Func{fn}
	for run := 0; run < 5; run++ {
		out := obj.NewPackage("main")
		if _, err := AddGlobals(out, p); err != nil {
			t.Fatalf("AddGlobals: %v", err)
		}
		for i, want := range names {
			got := out.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: uint32(i)})
			if got == nil || got.Name != want {
				t.Fatalf("run %d: symbol %d is %v, want %s", run, i, got, want)
			}
			if v := binary.LittleEndian.Uint64(got.Data); v != uint64(i+1) {
				t.Errorf("run %d: %s holds %d, want %d", run, want, v, i+1)
			}
		}
	}
}

// TestGlobalBoolAndUnsignedConstants covers the remaining constant layouts.
func TestGlobalBoolAndUnsignedConstants(t *testing.T) {
	for _, c := range []struct {
		name string
		kind ir.Kind
		val  constant.Value
		want []byte
	}{
		{"main.flag", ir.Bool, constant.MakeBool(true), []byte{1}},
		{"main.u8", ir.Uint8, constant.MakeInt64(200), []byte{200}},
		{"main.i16", ir.Int16, constant.MakeInt64(-2), []byte{0xfe, 0xff}},
		{"main.u64", ir.Uint64, constant.MakeUint64(1 << 63), []byte{0, 0, 0, 0, 0, 0, 0, 0x80}},
		{"main.f64", ir.Float64, constant.MakeFloat64(1.5), []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x3f}},
		{"main.f32", ir.Float32, constant.MakeFloat64(1.5), []byte{0, 0, 0xc0, 0x3f}},
	} {
		ty := globalType(t, c.kind, nil, 0)
		p, _ := globalPkg(c.name, ty, c.val)
		out := obj.NewPackage("main")
		if _, err := AddGlobals(out, p); err != nil {
			t.Fatalf("%s: AddGlobals: %v", c.name, err)
		}
		sym := out.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 0})
		if string(sym.Data) != string(c.want) {
			t.Errorf("%s holds %x, want %x", c.name, sym.Data, c.want)
		}
	}
}

// TestGlobalEmptyStringPointsAtNothing checks that the empty string needs no
// symbol.
//
// A symbol of no bytes is a definition the linker has to place for a pointer
// nothing dereferences, and every other part of this compiler makes the empty
// string a nil pointer and a zero length.
func TestGlobalEmptyStringPointsAtNothing(t *testing.T) {
	ty := globalType(t, ir.String, nil, 0)
	p, _ := globalPkg("main.empty", ty, constant.MakeString(""))
	out := obj.NewPackage("main")
	if _, err := AddGlobals(out, p); err != nil {
		t.Fatalf("AddGlobals: %v", err)
	}
	sym := out.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 0})
	if len(sym.Relocs) != 0 {
		t.Errorf("the empty string has %d relocations, want none", len(sym.Relocs))
	}
	if sym.Type != obj.SBSS {
		t.Errorf("the empty string is of kind %d, want SBSS", sym.Type)
	}
}

// TestGlobalWithoutTypeIsRefused checks the two malformed cases, which are
// compiler bugs rather than program errors and must not become a symbol of
// no size.
func TestGlobalWithoutTypeIsRefused(t *testing.T) {
	p := &ir.Package{Path: "main", Globals: []*ir.Object{{Name: "main.x", Class: ir.ClassGlobal}}}
	if _, err := AddGlobals(obj.NewPackage("main"), p); err == nil {
		t.Error("a variable with no type was accepted")
	}
	p = &ir.Package{Path: "main", Globals: []*ir.Object{{Class: ir.ClassGlobal}}}
	if _, err := AddGlobals(obj.NewPackage("main"), p); err == nil {
		t.Error("a variable with no symbol was accepted")
	}
	if _, err := AddGlobals(nil, nil); err == nil {
		t.Error("a nil object was accepted")
	}
	if err := CheckGlobals(nil); err != nil {
		t.Errorf("CheckGlobals of no package gave %v", err)
	}
}
