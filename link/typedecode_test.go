// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"runtime"
	"strings"
	"testing"
)

// TestTypeDecodersAgreeWithTheNamesTheCompilerWrote checks the descriptor
// decoders against gc's own naming, over every type in a real program.
//
// The reachability pass reads a method's name and signature out of a type
// descriptor, and a wrong offset there is quiet: the pass keeps the wrong
// methods and the program still links. The check is that a decoded method
// name is the tail of the symbol name of the function the method's
// relocations point at. gc writes both, from the same source declaration,
// and they agree or one of the two decoders is wrong.
func TestTypeDecodersAgreeWithTheNamesTheCompilerWrote(t *testing.T) {
	b := reflectBuild.get(t)
	l := loadProgram(t, b)
	ps := ptrSize(runtime.GOARCH)

	types, methods := 0, 0
	for g := Global(1); g < Global(l.NSym()); g++ {
		st, s := l.def(g)
		if s == nil || !s.GoType() || !typeHasUncommon(s.Data, ps) {
			continue
		}
		sigs := l.typeMethods(st, g, s, ps)
		if len(sigs) == 0 {
			continue
		}
		// The relocations of a type descriptor list its methods in three
		// consecutive entries each, so the count must match.
		triples := 0
		var funcs []string
		for i := 0; i < len(s.Relocs); i++ {
			if s.Relocs[i].Type != rMethodOff {
				continue
			}
			triples++
			if i+2 < len(s.Relocs) {
				funcs = append(funcs, l.Name(l.resolve(st, s.Relocs[i+2].Sym)))
			}
			i += 2
		}
		if triples != len(sigs) {
			t.Errorf("%s: the descriptor declares %d methods and the relocations describe %d",
				s.Name, len(sigs), triples)
			continue
		}
		types++
		for i, sig := range sigs {
			methods++
			if sig.name == "" {
				t.Errorf("%s: method %d has no name", s.Name, i)
				continue
			}
			if i < len(funcs) && funcs[i] != "" && !strings.HasSuffix(funcs[i], "."+sig.name) {
				t.Errorf("%s: method %d decodes as %q and its function is %q", s.Name, i, sig.name, funcs[i])
			}
			if sig.typ == 0 {
				t.Errorf("%s: method %q has no signature type", s.Name, sig.name)
				continue
			}
			if n := l.Name(sig.typ); !strings.HasPrefix(n, "type:func(") && !strings.HasPrefix(n, "type:func()") {
				t.Errorf("%s: method %q has signature %q, which is not a function type", s.Name, sig.name, n)
			}
		}
	}
	if types < 20 {
		t.Fatalf("only %d types with methods were decoded, so this test proves little", types)
	}
	t.Logf("%d methods of %d types agree with the names the compiler wrote", methods, types)
}

// rMethodOff is the relocation that describes one field of a method
// record. It is spelled here so the test does not import obj for one
// constant.
const rMethodOff = 26

// TestItabTypeDecodesToTheNameTheItabCarries checks the offset at which an
// itab holds the type it is for.
//
// An itab is named for the pair it joins, "go:itab.<type>,<interface>", so
// the name says what the decoder must find. A wrong offset makes the
// reachability pass mark the wrong type as one that reached an interface,
// which changes which methods survive.
func TestItabTypeDecodesToTheNameTheItabCarries(t *testing.T) {
	b := reflectBuild.get(t)
	l := loadProgram(t, b)
	ps := ptrSize(runtime.GOARCH)

	n := 0
	for g := Global(1); g < Global(l.NSym()); g++ {
		st, s := l.def(g)
		if s == nil || !s.Itab() {
			continue
		}
		pair, ok := strings.CutPrefix(s.Name, "go:itab.")
		if !ok {
			t.Errorf("%s carries the itab flag and is not named like one", s.Name)
			continue
		}
		typeName, _, ok := strings.Cut(pair, ",")
		if !ok {
			t.Errorf("%s does not name a type and an interface", s.Name)
			continue
		}
		got := l.Name(l.itabType(st, s, ps))
		if got != "type:"+typeName {
			t.Errorf("%s holds the type %q, and its name says %q", s.Name, got, "type:"+typeName)
			continue
		}
		n++
	}
	if n < 5 {
		t.Fatalf("only %d itabs were decoded, so this test proves little", n)
	}
	t.Logf("%d itabs hold the type their name says", n)
}

// TestRTypeSizeCoversEveryKind checks the size of the kind-specific
// descriptor against the layout gc wrote.
//
// The uncommon record follows the kind-specific descriptor, and the method
// array is the tail of the whole thing. So a descriptor with methods
// states its own layout: the offset of the record, plus the offset the
// record gives for the array, plus one entry per method, is the size of
// the symbol. A wrong size for one kind fails here rather than producing a
// wrong method set later.
func TestRTypeSizeCoversEveryKind(t *testing.T) {
	b := reflectBuild.get(t)
	l := loadProgram(t, b)
	ps := ptrSize(runtime.GOARCH)

	kinds := map[byte]int{}
	for g := Global(1); g < Global(l.NSym()); g++ {
		_, s := l.def(g)
		if s == nil || !s.GoType() || !typeHasUncommon(s.Data, ps) {
			continue
		}
		kind := typeKind(s.Data, ps)
		off := rtypeSize(kind, ps)
		// gc trims the record's trailing padding when it is the end of
		// the symbol, so the bytes that must be present end at the method
		// offset.
		if off+12 > len(s.Data) {
			t.Errorf("%s: kind %d puts the uncommon record at %d and the descriptor is %d bytes",
				s.Name, kind, off, len(s.Data))
			continue
		}
		mcount := int(le16(s.Data, off+4))
		moff := int(le32(s.Data, off+8))
		if mcount == 0 {
			continue
		}
		if got, want := off+moff+uncommonMethodSize*mcount, int(s.Size); got != want {
			t.Errorf("%s: kind %d has %d methods ending at %d and the descriptor is %d bytes",
				s.Name, kind, mcount, got, want)
			continue
		}
		kinds[kind]++
	}
	if len(kinds) < 3 {
		t.Fatalf("only %d kinds were checked, so this test proves little", len(kinds))
	}
	t.Logf("the method array ends at the end of the descriptor for %d kinds", len(kinds))
}

// TestTypeDecoderRefusesShortData checks that a descriptor too small to
// hold what it claims decodes to nothing rather than to a panic.
func TestTypeDecoderRefusesShortData(t *testing.T) {
	if typeKind(nil, 8) != 0 {
		t.Error("an empty descriptor has a kind")
	}
	if typeHasUncommon(make([]byte, 8), 8) {
		t.Error("a descriptor too short to hold a flag byte has an uncommon record")
	}
	l := NewLoader()
	s := &Sym{Name: "type:short", Data: make([]byte, 24)}
	s.Data[2*8+4] = tflagUncommon
	if m := l.typeMethods(nil, 0, s, 8); m != nil {
		t.Errorf("a descriptor of %d bytes decoded %d methods", len(s.Data), len(m))
	}
	if _, ok := relocAt(s, 0); ok {
		t.Error("a symbol with no relocations has one at offset 0")
	}
	if m := (methodRef{}); m.exported() {
		t.Error("a method with no name is exported")
	}
}

// TestRTypeSizeIsTheDocumentedTable states the size of each kind-specific
// descriptor, so a change in internal/abi that this package did not follow
// is a failure here and not a wrong method set.
func TestRTypeSizeIsTheDocumentedTable(t *testing.T) {
	const ps = 8
	common := commonSize(ps)
	for _, c := range []struct {
		kind byte
		want int
	}{
		{kindStruct, common + 4*ps},
		{kindInterface, common + 4*ps},
		{kindPointer, common + ps},
		{kindFunc, common + ps},
		{kindSlice, common + ps},
		{kindArray, common + 3*ps},
		{kindChan, common + 2*ps},
		{kindMap, common + 10*ps + 8},
		{0, common},
	} {
		if got := rtypeSize(c.kind, ps); got != c.want {
			t.Errorf("rtypeSize(%d) = %d, want %d", c.kind, got, c.want)
		}
	}
	if commonSize(8) != 48 {
		t.Errorf("sizeof(abi.Type) is %d on a 64 bit target, want 48", commonSize(8))
	}
	if itabTypeOff(8) != 8 {
		t.Errorf("an itab holds its type at %d, want 8", itabTypeOff(8))
	}
	if ptrSize("386") != 4 || ptrSize("arm64") != 8 || ptrSize("amd64") != 8 {
		t.Error("the pointer width of a target is wrong")
	}
}
