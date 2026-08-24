// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtsym"
)

// TestMorestackIsInTheRuntime checks the one runtime symbol this package
// spells by hand.
//
// specs/031-runtime-lowering.md requires every runtime symbol the compiler
// generates a call to be checked against the runtime's source rather than
// typed in and trusted, and rtsym is where that check lives. The prologue's
// symbol is not in rtsym's table, so the check is here: the name has to be a
// TEXT symbol in the runtime's assembly, and the call has to be ABI0 because
// that is how the runtime defines it.
func TestMorestackIsInTheRuntime(t *testing.T) {
	if rtsym.Lookup(morestack.name) != nil {
		t.Fatalf("%s is in rtsym now, so this package should take it from there", morestack.name)
	}
	path := filepath.Join(runtimeGOROOT(t), "src", "runtime", "asm_arm64.s")
	b, err := os.ReadFile(path)
	if err != nil {
		if requireCorpus() {
			t.Fatalf("NANOGO_REQUIRE_CORPUS is set and the runtime source is not there: %v", err)
		}
		t.Skipf("no runtime source: %v", err)
	}
	// The runtime writes the middle dot, and the linker name uses a full stop.
	want := regexp.MustCompile(`(?m)^TEXT\s+runtime·` +
		regexp.QuoteMeta(strings.TrimPrefix(morestack.name, "runtime.")) + `\(SB\)`)
	if !want.MatchString(string(b)) {
		t.Fatalf("%s does not define %s", path, morestack.name)
	}
	if morestack.abi != obj.ABI0 {
		t.Errorf("the call names ABI %d, and the runtime defines the symbol in assembly, which is ABI0", morestack.abi)
	}
	t.Logf("%s is defined in %s", morestack.name, path)
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
	for _, want := range []string{"R_CALLARM64:main.g", "R_CALLARM64:" + morestack.name} {
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
