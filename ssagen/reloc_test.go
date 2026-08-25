// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtsym"
)

// TestMorestackComesFromRtsym checks that the stack-growth tail names the
// symbol rtsym carries rather than a literal of its own.
//
// specs/031-runtime-lowering.md requires every runtime symbol the compiler
// generates a call to be checked against the runtime's source, and rtsym is
// where that check lives. The symbol has no Go declaration, so rtsym marks it
// Assembly and checks that the runtime's assembly defines it; this test checks
// that the emitter reads that record, because a literal here would not move
// when the runtime does.
func TestMorestackComesFromRtsym(t *testing.T) {
	s := rtsym.Lookup(morestackName)
	if s == nil {
		t.Fatalf("rtsym does not carry %s", morestackName)
	}
	if !s.Assembly {
		t.Errorf("%s is not marked Assembly, and only that justifies calling it at ABI0", s.Name)
	}
	c, ok := morestackCallee()
	if !ok {
		t.Fatal("the emitter found no callee for the stack-growth tail")
	}
	if c.name != s.Name {
		t.Errorf("the tail calls %q and rtsym names %q", c.name, s.Name)
	}
	// The runtime defines the symbol in assembly, which cmd/internal/obj
	// looks up in the ABI0 table.
	if c.abi != obj.ABI0 {
		t.Errorf("the tail calls %s at ABI %d, want ABI0", c.name, c.abi)
	}
	comparisons++
}

// TestRuntimeCalleeComesFromRtsym checks that a call to a runtime function
// that lowering emits is looked up rather than spelled.
func TestRuntimeCalleeComesFromRtsym(t *testing.T) {
	c, ok := runtimeCallee("runtime.newobject")
	if !ok {
		t.Fatal("rtsym does not know runtime.newobject")
	}
	if c.name != "runtime.newobject" || c.abi != obj.ABIInternal {
		t.Errorf("callee %+v, want runtime.newobject at ABIInternal", c)
	}
	if _, ok := runtimeCallee("runtime.notAFunction"); ok {
		t.Error("rtsym claims to know a symbol that is not in it")
	}
	// A call to a runtime symbol reaches the same record whether the rule
	// spelled the prefix or not, because callTarget asks rtsym first.
	got, err := callTarget(&ir.Object{Name: "runtime.newobject"})
	if err != nil || got != c {
		t.Errorf("callTarget gave %+v, %v", got, err)
	}
	if _, err := callTarget(nil); err == nil {
		t.Error("a call with no symbol was accepted")
	}
}

// TestSymbolReferencesAreInterned checks that one name produces one reference.
//
// A relocation names a symbol by index, so two calls to one function must
// resolve to one index. A second reference would be a second entry in the
// object and a different byte sequence for the same program, which
// specs/053-determinism.md forbids.
func TestSymbolReferencesAreInterned(t *testing.T) {
	p := obj.NewPackage("main")
	s := newSymbols(p)
	a := s.ref(callee{name: "main.g", abi: obj.ABIInternal})
	b := s.ref(callee{name: "main.g", abi: obj.ABIInternal})
	if a != b {
		t.Errorf("one name gave two references, %v and %v", a, b)
	}
	// The ABI is part of a symbol's identity, so the same name under two of
	// them is two symbols.
	c := s.ref(callee{name: "main.g", abi: obj.ABI0})
	if c == a {
		t.Error("the same name under two ABIs gave one reference")
	}
}

// TestCallRelocations checks the offsets and the types against the
// disassembler.
//
// A call is a BL with a zero displacement and a relocation that names the
// callee, so the check is that the relocation sits on the BL and that objdump
// resolves it to the name. A relocation one instruction out produces a program
// that jumps into the middle of the prologue.
func TestCallRelocations(t *testing.T) {
	c := compile(t, "package main\n\nfunc g(a int) int\n\nfunc f(a int) int { return g(a) }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)

	if len(r.Text.Relocs) != 2 {
		t.Fatalf("%d relocations, want two: the call and the growth tail", len(r.Text.Relocs))
	}
	for _, rel := range r.Text.Relocs {
		if rel.Type != obj.R_CALLARM64 {
			t.Errorf("relocation at %d has type %v, want R_CALLARM64", rel.Off, rel.Type)
		}
		if rel.Size != 4 {
			t.Errorf("relocation at %d is %d bytes, and a call immediate is four", rel.Off, rel.Size)
		}
		if rel.Off%4 != 0 {
			t.Errorf("relocation at %d is not on an instruction boundary", rel.Off)
		}
		// The instruction the relocation covers must be the branch with the
		// link, with no displacement of its own.
		if w := words(r.Text)[rel.Off/4]; w != 0x94000000 {
			t.Errorf("the instruction at %d is %#08x, and a call with no displacement is 0x94000000", rel.Off, w)
		}
	}
	// go tool objdump prints the relocation and the name it resolves to, so
	// the type and the target are checked by the disassembler rather than by
	// this package's own record of them.
	text := disassemble(t, r, p)
	for _, want := range []string{"R_CALLARM64:main.g", "R_CALLARM64:" + morestackName} {
		if !strings.Contains(text, want) {
			t.Errorf("the disassembly does not hold %s:\n%s", want, text)
		}
	}
}

// TestAddressRelocations checks the address of a global.
//
// An address is an ADRP and an ADD, and the linker splits one value across
// them, so it is one relocation of eight bytes and not two of four. Two
// relocations would let the linker patch one instruction and not the other,
// and the address would be the page of the symbol with the offset of whatever
// the ADD held.
func TestAddressRelocations(t *testing.T) {
	c := compile(t, "package main\n\nvar G int\n\nfunc f() int { return G }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	n := 0
	for _, rel := range r.Text.Relocs {
		if rel.Type != obj.R_ADDRARM64 {
			continue
		}
		if rel.Size != 8 {
			t.Errorf("the address relocation is %d bytes, and it covers an ADRP and an ADD", rel.Size)
		}
		w := words(r.Text)
		if got := w[rel.Off/4] & 0x9f000000; got != 0x90000000 {
			t.Errorf("the instruction at %d is %#08x, which is not an ADRP", rel.Off, w[rel.Off/4])
		}
		n++
	}
	if n != 1 {
		t.Fatalf("%d address relocations, want 1", n)
	}
	text := disassemble(t, r, p)
	if !strings.Contains(text, "R_ADDRARM64:main.G") {
		t.Errorf("the disassembly does not name the global:\n%s", text)
	}
}

// TestTypeDescriptorNamesSurviveThePrefix is the third change of specs/032's
// seam.
//
// symbolName puts this package's path back onto a name with no dot in it,
// because ir.Object holds a package-level variable's name as written. A type
// descriptor's name is not a Go identifier and is already a linker symbol, so
// the rule does not apply to it: type:p.T survived by accident because it
// holds a dot, and type:int, type:[]int and type:interface {} became
// main.type:int and friends, symbols nothing defines and cmd/link never
// collects into runtime.typelinks.
func TestTypeDescriptorNamesSurviveThePrefix(t *testing.T) {
	// The name and the ABI are decided by the package path alone, so the
	// emitter needs nothing else.
	e := &emitter{pkg: obj.NewPackage("test")}
	for _, name := range []string{"type:int", "type:[]int", "type:interface {}", "type:*int", "type:p.T"} {
		if got := e.symbolName(&ir.Object{Name: name, Class: ir.ClassGlobal}); got != name {
			t.Errorf("the descriptor %q became %q", name, got)
		}
	}
	// The rule the exception is carved out of still holds.
	if got := e.symbolName(&ir.Object{Name: "G", Class: ir.ClassGlobal}); got != "test.G" {
		t.Errorf("a package-level name became %q, want test.G", got)
	}
}

// TestGlobalReferencesAreABI0 checks the half of a symbol's identity that is
// not its name.
//
// cmd/link resolves a by-name reference by name and ABI together, so a data
// symbol referenced under ABIInternal names a symbol nothing defines and the
// link reports the descriptor as undefined. gc gives a data symbol no ABI,
// which is ABI0.
func TestGlobalReferencesAreABI0(t *testing.T) {
	// The name and the ABI are decided by the package path alone, so the
	// emitter needs nothing else.
	e := &emitter{pkg: obj.NewPackage("test")}
	if c := e.globalCallee(&ir.Object{Name: "type:int", Class: ir.ClassGlobal}); c.abi != obj.ABI0 {
		t.Errorf("a type descriptor is referenced at ABI %d, want ABI0", c.abi)
	}
	if c := e.globalCallee(&ir.Object{Name: "G", Class: ir.ClassGlobal}); c.name != "test.G" || c.abi != obj.ABI0 {
		t.Errorf("a package-level variable is referenced as %+v, want test.G at ABI0", c)
	}
	// A text symbol keeps ABIInternal, which is what specs/030-abi.md gives
	// every function nanogo compiles.
	if c := e.globalCallee(&ir.Object{Name: "main.f", Class: ir.ClassFunc}); c.abi != obj.ABIInternal {
		t.Errorf("a function is referenced at ABI %d, want ABIInternal", c.abi)
	}
}
