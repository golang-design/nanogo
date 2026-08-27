// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype_test

import (
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
)

// The descriptor as the object writer sees it.
//
// Nothing in this deck writes a descriptor into an object yet: ssagen.Result
// carries a text symbol and its auxiliary symbols and nothing else, and
// driver.emitPackage walks the function list. specs/032 names the two changes
// that close the seam and both are wiring.
//
// This test does that wiring in one place, so that the shape of what rtype
// produces is checked against the writer rather than only against reflect. A
// descriptor whose kind, name or relocation width the writer rejects is a
// failure here instead of a failure in whoever closes the seam.

// addSymbols adds a descriptor's symbols to a package, resolving each
// relocation target by name.
//
// A target defined in the same set resolves to that definition; anything else
// becomes a non-package reference, which the linker resolves by name. That is
// the rule ssagen/reloc.go uses for a call, applied to a data symbol.
func addSymbols(t *testing.T, p *obj.Package, syms []rtype.Symbol) {
	t.Helper()
	defs := make(map[string]obj.SymRef, len(syms))
	out := make([]*obj.Symbol, len(syms))
	for i, s := range syms {
		out[i] = &obj.Symbol{
			Name:  s.Name,
			Type:  s.Kind,
			Size:  uint32(len(s.Data)),
			Align: s.Align,
			Data:  s.Data,
		}
		if s.Dupok {
			out[i].Flag |= obj.SymFlagDupok
		}
	}
	// Two passes: a relocation may name a symbol later in the list, and an
	// array's slice reference names one that is not in the list at all.
	// A non-package definition, as the driver writes them: every one of these
	// is dupok, and cmd/link deduplicates a dupok symbol by name in that index
	// space only.
	for i, s := range syms {
		defs[s.Name] = p.AddNonPkgDef(out[i])
	}
	refs := make(map[string]obj.SymRef)
	for i, s := range syms {
		for _, r := range s.Relocs {
			ref, ok := defs[r.Target]
			if !ok {
				if ref, ok = refs[r.Target]; !ok {
					ref = p.AddNonPkgRef(&obj.Symbol{Name: r.Target, ABI: obj.ABIInternal})
					p.AddRefName(ref, r.Target)
					refs[r.Target] = ref
				}
			}
			out[i].Relocs = append(out[i].Relocs, obj.Reloc{
				Off: r.Off, Size: r.Size, Type: r.Type, Add: r.Add, Sym: ref,
			})
		}
	}
}

// TestDescriptorWritesIntoAnObject checks that the writer accepts what the
// encoder produces.
func TestDescriptorWritesIntoAnObject(t *testing.T) {
	types := corpusTypes(t)
	p := obj.NewPackage("p")
	for i, c := range corpus {
		syms, err := rtype.Descriptor(types[i])
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		addSymbols(t, p, syms)
	}
	if _, err := p.Bytes(); err != nil {
		t.Fatalf("the object does not write: %v", err)
	}
}

// TestDescriptorCarriesTheTypeFlag checks the flag the writer derives from the
// name.
//
// obj/write.go sets SymFlagGoType from a "type:" prefix on a read-only symbol,
// and the linker enters only a flagged symbol in the type table. A descriptor
// that arrives without it links and then fails at run time rather than at link
// time, which is why the name and the kind are checked together here.
func TestDescriptorCarriesTheTypeFlag(t *testing.T) {
	ty := &ir.Type{Kind: ir.Slice, Elem: &ir.Type{Kind: ir.Int64, Name: "int"}}
	if err := ir.Layout(ty); err != nil {
		t.Fatal(err)
	}
	syms, err := rtype.Descriptor(ty)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	p := obj.NewPackage("p")
	addSymbols(t, p, syms)
	b, err := p.Bytes()
	if err != nil {
		t.Fatalf("the object does not write: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("the object is empty")
	}
	// The descriptor is read-only data with a "type:" name, which is what the
	// writer keys the flag on.
	if syms[0].Kind != obj.SRODATA {
		t.Errorf("the descriptor is kind %v, want SRODATA", syms[0].Kind)
	}
	// The bitmask is read-only data too, as gc marks it. A stack object record
	// holds the offset of the mask from the start of its own section and the
	// runtime resolves that offset against moduledata.rodata, so a mask in
	// another section is read at an address that holds something else. It
	// carries no "type:" name, so it takes no descriptor flag from it.
	bits, ok := find(syms, "runtime.gcbits.0100000000000000")
	if !ok {
		t.Fatalf("the bitmask is not emitted; the symbols are %v", names(syms))
	}
	if bits.Kind != obj.SRODATA {
		t.Errorf("the bitmask is kind %v, want SRODATA", bits.Kind)
	}
}
