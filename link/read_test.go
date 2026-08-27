// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
)

// testHeader is a toolchain header line. The round trip needs no
// toolchain: obj.Package.WriteObject takes the line as an argument and
// checks only its shape.
const testHeader = "go object darwin arm64 go1.27.0 X:regabiwrappers\n"

// sample builds a package that uses every part of the format the reader
// has to account for: all five index spaces, both hash blocks, imports,
// file names, relocations, auxiliary entries, an anonymous payload, a
// symbol with no data, and both reference blocks.
type sample struct {
	pkg *obj.Package

	// The references the writer resolves, as the object holds them.
	hashed64, hashed, nonPkgDef, pcsp, nonPkgRef, pkgRef obj.SymRef
}

func newSample() *sample {
	p := obj.NewPackage("example.com/round")
	p.Main = true
	p.Flags = obj.ObjFlagStd
	p.Fingerprint = [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	p.AddImport("runtime", [8]byte{9, 10, 11, 12, 13, 14, 15, 16})
	p.AddImport("errors", [8]byte{17, 18, 19, 20, 21, 22, 23, 24})
	p.AddFile("dir/a.go")
	p.AddFile("b.go")

	s := &sample{pkg: p}
	s.hashed64 = p.AddHashed64Def(&obj.Symbol{
		Name: "round..stmp_0", Type: obj.SRODATA, Size: 8, Align: 8,
		Data: []byte{1, 0, 0, 0, 0, 0, 0, 0},
	})
	s.hashed = p.AddHashedDef(&obj.Symbol{
		Name: "go:string.hi", Type: obj.SRODATA, Size: 2, Align: 1, Data: []byte("hi"),
	})
	s.nonPkgDef = p.AddNonPkgDef(&obj.Symbol{
		Name: "gclocals.a", Type: obj.SRODATA, Flag: obj.SymFlagDupok,
		Size: 4, Align: 1, Data: []byte{0, 1, 2, 3},
	})
	// An anonymous pc-value table. It is a real symbol with no name, so it
	// is the case a reader that treats an empty name as an error breaks on.
	s.pcsp = p.AddNonPkgDef(&obj.Symbol{
		Anonymous: true, Pcdata: true, Type: obj.SRODATA, Size: 3, Align: 1, Data: []byte{2, 4, 0},
	})
	s.nonPkgRef = obj.SymRef{PkgIdx: obj.PkgIdxNone, SymIdx: 2} // after the two definitions
	p.AddNonPkgRef(&obj.Symbol{Name: "runtime.morestack_noctxt", ABI: obj.ABI0})
	s.pkgRef = p.PkgRef("runtime", 42)

	p.AddDef(&obj.Symbol{
		Name: "example.com/round.F", ABI: obj.ABIInternal, Type: obj.STEXT,
		Size: 8, Align: 4, Flag: obj.SymFlagNoSplit | obj.SymFlagLeaf,
		Flag2: obj.SymFlagPkgInit,
		Data:  []byte{1, 2, 3, 4, 5, 6, 7, 8},
		Relocs: []obj.Reloc{
			{Off: 4, Size: 4, Type: obj.R_CALLARM64, Sym: s.nonPkgRef},
			{Off: 0, Size: 0, Type: obj.R_USEIFACE, Sym: s.pkgRef},
			{Off: 0, Size: 4, Type: obj.R_ADDRARM64, Add: 3, Sym: s.hashed},
		},
		Aux: []obj.Aux{{Type: obj.AuxGotype, Sym: s.pkgRef}, {Type: obj.AuxPcsp, Sym: s.pcsp}},
	})
	// A type descriptor, so the round trip sees a flag the writer derives
	// from the name rather than one the caller set.
	p.AddDef(&obj.Symbol{
		Name: "type:example.com/round.T", Type: obj.SRODATA, Size: 8, Align: 8, Data: make([]byte, 8),
	})
	// A zero-filled symbol: a size with no data, which is the pair the
	// data index has to keep straight.
	p.AddDef(&obj.Symbol{Name: "example.com/round.G", Type: obj.SNOPTRBSS, Size: 16, Align: 8})

	p.AddRefFlag(s.pkgRef, 0, obj.SymFlagUsedInIface)
	p.AddRefName(s.nonPkgRef, "runtime.morestack_noctxt")
	return s
}

// write returns the object bytes of the sample.
func (s *sample) write(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := s.pkg.WriteObject(&buf, testHeader); err != nil {
		t.Fatalf("writing the sample object: %v", err)
	}
	return buf.Bytes()
}

// read parses the sample and fails the test on a refusal.
func (s *sample) read(t *testing.T) *Object {
	t.Helper()
	o, err := ReadObject(s.write(t), "round.o", "example.com/round")
	if err != nil {
		t.Fatalf("the reader refused an object the writer produced: %v", err)
	}
	return o
}

// TestRoundTrip reads back what obj wrote and compares every field with
// the structure that produced it.
//
// This is the oracle specs/045-linker.md names for the first stage. The
// writer is checked against gc by obj's own tests, so a reader that agrees
// with the writer agrees with gc.
func TestRoundTrip(t *testing.T) {
	s := newSample()
	o := s.read(t)

	if o.Header != testHeader {
		t.Errorf("header is %q, the object carries %q", o.Header, testHeader)
	}
	if !o.Main {
		t.Error("the main mark was not read, and the linker refuses a first object without it")
	}
	if o.Pkg != "example.com/round" {
		t.Errorf("package path is %q; it comes from the caller", o.Pkg)
	}
	if o.Fingerprint != s.pkg.Fingerprint {
		t.Errorf("fingerprint is %v, the writer put %v", o.Fingerprint, s.pkg.Fingerprint)
	}
	if o.Flags != obj.ObjFlagStd || !o.Std() {
		t.Errorf("flags are %#x, the writer put %#x", o.Flags, uint32(obj.ObjFlagStd))
	}
	if o.Trailing != 0 {
		t.Errorf("the object has %d bytes after the last block; obj writes none", o.Trailing)
	}
	if o.StringCovered != o.StringBytes {
		t.Errorf("%d of %d string bytes were reached by a reference", o.StringCovered, o.StringBytes)
	}

	wantImports := []obj.ImportedPkg{
		{Path: "runtime", Fingerprint: [8]byte{9, 10, 11, 12, 13, 14, 15, 16}},
		{Path: "errors", Fingerprint: [8]byte{17, 18, 19, 20, 21, 22, 23, 24}},
	}
	if !reflect.DeepEqual(o.Imports, wantImports) {
		t.Errorf("imports are %v, want %v", o.Imports, wantImports)
	}
	if want := []string{"", "runtime"}; !reflect.DeepEqual(o.Pkglist, want) {
		t.Errorf("package list is %q, want %q", o.Pkglist, want)
	}
	if want := []string{"dir/a.go", "b.go"}; !reflect.DeepEqual(o.Files, want) {
		t.Errorf("file table is %q, want %q", o.Files, want)
	}

	if n := o.NDef(); n != 7 {
		t.Errorf("the object defines %d symbols, want 7", n)
	}
	if n := o.NSym(); n != 8 {
		t.Errorf("the local index space holds %d records, want 8", n)
	}

	// The text symbol carries everything a later stage reads.
	f := o.Defs[0]
	if f.Name != "example.com/round.F" || f.ABI != obj.ABIInternal || f.Type != obj.STEXT {
		t.Fatalf("the first definition is %q ABI %d kind %d", f.Name, f.ABI, f.Type)
	}
	if f.Size != 8 || f.Align != 4 || !bytes.Equal(f.Data, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Errorf("size %d align %d data %v", f.Size, f.Align, f.Data)
	}
	if !f.NoSplit() || !f.Leaf() || !f.PkgInit() {
		t.Errorf("flags %#x %#x lost a bit the caller set", f.Flag, f.Flag2)
	}
	if f.Dupok() || f.GoType() || f.UsedInIface() {
		t.Errorf("flags %#x %#x hold a bit the caller did not set", f.Flag, f.Flag2)
	}
	// The writer sorts relocations by offset and breaks ties by type, so
	// the order in the object is not the order the caller gave.
	wantRelocs := []obj.Reloc{
		{Off: 0, Size: 4, Type: obj.R_ADDRARM64, Add: 3, Sym: s.hashed},
		{Off: 0, Size: 0, Type: obj.R_USEIFACE, Sym: s.pkgRef},
		{Off: 4, Size: 4, Type: obj.R_CALLARM64, Sym: s.nonPkgRef},
	}
	if !reflect.DeepEqual(f.Relocs, wantRelocs) {
		t.Errorf("relocations are\n%v\nwant\n%v", f.Relocs, wantRelocs)
	}
	wantAux := []obj.Aux{{Type: obj.AuxGotype, Sym: s.pkgRef}, {Type: obj.AuxPcsp, Sym: s.pcsp}}
	if !reflect.DeepEqual(f.Aux, wantAux) {
		t.Errorf("auxiliary entries are %v, want %v", f.Aux, wantAux)
	}

	// A flag the writer derives from the name arrives set.
	if td := o.Defs[1]; td.Name != "type:example.com/round.T" || !td.GoType() {
		t.Errorf("the type descriptor is %q with flags %#x, and the linker enters it in the type table by that flag", td.Name, td.Flag)
	}
	// A zero-filled symbol has a size and no data.
	if g := o.Defs[2]; g.Size != 16 || len(g.Data) != 0 {
		t.Errorf("the bss symbol has size %d and %d bytes of data, want 16 and 0", g.Size, len(g.Data))
	}

	if n := len(o.Hashed64Defs); n != 1 || o.Hashed64Defs[0].Name != "round..stmp_0" {
		t.Fatalf("the short hashed space holds %d symbols", n)
	}
	// A short symbol's content is its identity, stored whole in the hash
	// block. The linker reads that block by position.
	if want := [8]byte{1, 0, 0, 0, 0, 0, 0, 0}; o.Hash64[0] != want {
		t.Errorf("the short content hash is %v, want the content %v", o.Hash64[0], want)
	}
	if n := len(o.HashedDefs); n != 1 || o.HashedDefs[0].Name != "go:string.hi" {
		t.Fatalf("the hashed space holds %d symbols", n)
	}
	if o.Hash[0] == ([HashSize]byte{}) {
		t.Error("the content hash of the hashed symbol is zero")
	}

	// A duplicate-tolerant definition is a non-package definition, and the
	// two index spaces must not be flattened together.
	if d := o.NonPkgDefs[0]; d.Name != "gclocals.a" || !d.Dupok() {
		t.Errorf("the non-package definition is %q with flags %#x", d.Name, d.Flag)
	}
	if a := o.NonPkgDefs[1]; a.Name != "" || !bytes.Equal(a.Data, []byte{2, 4, 0}) {
		t.Errorf("the anonymous payload is %q with data %v", a.Name, a.Data)
	}
	if r := o.NonPkgRefs[0]; r.Name != "runtime.morestack_noctxt" || len(r.Data) != 0 {
		t.Errorf("the non-package reference is %q with %d bytes of data", r.Name, len(r.Data))
	}

	wantFlags := []obj.RefFlag{{Sym: s.pkgRef, Flag: 0, Flag2: obj.SymFlagUsedInIface}}
	if !reflect.DeepEqual(o.RefFlags, wantFlags) {
		t.Errorf("reference flags are %v, want %v", o.RefFlags, wantFlags)
	}
	wantNames := []obj.RefName{{Sym: s.nonPkgRef, Name: "runtime.morestack_noctxt"}}
	if !reflect.DeepEqual(o.RefNames, wantNames) {
		t.Errorf("reference names are %v, want %v", o.RefNames, wantNames)
	}
}

// TestLocalIndexSpace checks the order the index blocks are keyed by.
//
// A reader that counts the references into the definition space slices
// every later block wrongly, and the failure is silent: the symbols are
// all there and each one holds another one's relocations.
func TestLocalIndexSpace(t *testing.T) {
	o := newSample().read(t)
	want := []string{
		"example.com/round.F", "type:example.com/round.T", "example.com/round.G", // Defs
		"round..stmp_0",  // Hashed64Defs
		"go:string.hi",   // HashedDefs
		"gclocals.a", "", // NonPkgDefs
		"runtime.morestack_noctxt", // NonPkgRefs
	}
	for i, name := range want {
		s := o.Local(i)
		if s == nil {
			t.Fatalf("local index %d is out of range, and the space holds %d records", i, o.NSym())
		}
		if s.Name != name {
			t.Errorf("local index %d is %q, want %q", i, s.Name, name)
		}
	}
	if o.Local(len(want)) != nil {
		t.Errorf("local index %d resolved, and the space holds %d records", len(want), o.NSym())
	}
	if o.Local(-1) != nil {
		t.Error("a negative local index resolved")
	}

	// A reference resolves to the position the index blocks use.
	for _, c := range []struct {
		ref  obj.SymRef
		want int
		ok   bool
	}{
		{obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 2}, 2, true},
		{obj.SymRef{PkgIdx: obj.PkgIdxHashed64, SymIdx: 0}, 3, true},
		{obj.SymRef{PkgIdx: obj.PkgIdxHashed, SymIdx: 0}, 4, true},
		{obj.SymRef{PkgIdx: obj.PkgIdxNone, SymIdx: 1}, 6, true},
		{obj.SymRef{PkgIdx: obj.PkgIdxNone, SymIdx: 2}, 7, true},
		{obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 9}, 0, false},
		{obj.SymRef{PkgIdx: obj.PkgIdxBuiltin, SymIdx: 3}, 0, false},
		{obj.SymRef{PkgIdx: 1, SymIdx: 42}, 0, false},
		{obj.SymRef{}, 0, false},
	} {
		got, ok := o.LocalIndex(c.ref)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("LocalIndex(%v) = %d, %v; want %d, %v", c.ref, got, ok, c.want, c.ok)
		}
	}
}

// TestReadFileDispatches checks that a caller with a path out of an import
// configuration does not have to know whether it holds an archive.
func TestReadFileDispatches(t *testing.T) {
	b := newSample().write(t)
	got, err := ReadFile(b, "round.o", "example.com/round")
	if err != nil {
		t.Fatalf("reading a bare object: %v", err)
	}
	if len(got) != 1 || got[0].Pkg != "example.com/round" {
		t.Fatalf("a bare object read as %d objects", len(got))
	}

	ar := buildArchive(t, [][2]string{{"__.PKGDEF", "export data, not an object"}, {"_go_.o", string(b)}})
	got, err = ReadFile(ar, "round.a", "example.com/round")
	if err != nil {
		t.Fatalf("reading an archive: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the archive read as %d objects, want 1", len(got))
	}
	if want := "round.a(_go_.o)"; got[0].Name != want {
		t.Errorf("the member is named %q, want %q", got[0].Name, want)
	}
}

// buildArchive writes an ar archive of the named members.
func buildArchive(t *testing.T, members [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	for _, m := range members {
		name, data := m[0], m[1]
		hdr := make([]byte, arHdrSize)
		for i := range hdr {
			hdr[i] = ' '
		}
		copy(hdr, name)
		copy(hdr[16:], "0")
		copy(hdr[28:], "0")
		copy(hdr[34:], "0")
		copy(hdr[40:], "644")
		copy(hdr[48:], itoa(len(data)))
		copy(hdr[arHdrSize-2:], "`\n")
		buf.Write(hdr)
		buf.WriteString(data)
		if len(data)%2 != 0 {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// TestArchiveMemberSelection checks the rule that decides which members
// are objects. It is cmd/link's, and a rule of our own would load a member
// cmd/link skips.
func TestArchiveMemberSelection(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"_go_.o", true},
		{"asm_arm64.o", true},
		{"x.syso", true},
		{"dynimportfail", false},
		{"preferlinkext", false},
		{"__.PKGDEF", false},
		// Eighteen characters, truncated to sixteen by the name field, so
		// the extension is gone and the member is still an object.
		{"rt0_darwin_arm64", true},
	} {
		if got := isObjectMember(c.name); got != c.want {
			t.Errorf("isObjectMember(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestArchiveRefusals checks that a damaged archive is refused rather than
// read to the point where it looks empty.
func TestArchiveRefusals(t *testing.T) {
	good := newSample().write(t)
	for _, c := range []struct {
		name string
		ar   []byte
		want string
	}{
		{"no magic", []byte("not an archive at all"), "not an archive"},
		{"header cut short", []byte(arMagic + "_go_.o          0"), "member header needs"},
		{"no terminator", func() []byte {
			b := buildArchive(t, [][2]string{{"_go_.o", "x"}})
			b[len(arMagic)+arHdrSize-1] = 'X'
			return b
		}(), "archive terminator"},
		{"unreadable size", func() []byte {
			b := buildArchive(t, [][2]string{{"_go_.o", "x"}})
			copy(b[len(arMagic)+48:], "zz")
			return b
		}(), "unreadable size"},
		{"member past the end", func() []byte {
			b := buildArchive(t, [][2]string{{"_go_.o", string(good)}})
			copy(b[len(arMagic)+48:], "99999999  ")
			return b
		}(), "are left"},
		{"no members", []byte(arMagic), "holds no members"},
		{"no object", buildArchive(t, [][2]string{{"__.PKGDEF", "export data"}}), "holds no object"},
		{"member is not an object", buildArchive(t, [][2]string{{"_go_.o", "not an object"}}), "not a Go object file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadArchive(c.ar, "x.a", "example.com/round")
			if err == nil {
				t.Fatal("the reader accepted a damaged archive")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal is %q, it must name %q", err, c.want)
			}
		})
	}
}

// TestObjectRefusals is the accounting rule stated as failures.
//
// Each case damages one identity the reader requires and checks that the
// reader refuses rather than parsing what is left. specs/045-linker.md
// lists the identities and what a violation of each one is.
func TestObjectRefusals(t *testing.T) {
	s := newSample()
	good := s.write(t)
	// blockAt is where the goobj blocks start, past the header line and
	// the separator.
	blockAt := bytes.Index(good, []byte(obj.Magic))
	if blockAt < 0 {
		t.Fatal("the sample object holds no goobj magic")
	}
	offsetAt := blockAt + len(obj.Magic) + 8 + 4
	setOffset := func(b []byte, blk int, v uint32) {
		binary.LittleEndian.PutUint32(b[offsetAt+4*blk:], v)
	}
	offset := func(b []byte, blk int) uint32 {
		return binary.LittleEndian.Uint32(b[offsetAt+4*blk:])
	}
	damage := func(f func(b []byte) []byte) []byte {
		b := make([]byte, len(good))
		copy(b, good)
		return f(b)
	}

	for _, c := range []struct {
		name string
		obj  []byte
		want string
	}{
		{"not an object", []byte("package main\n"), "not a Go object file"},
		{"header line has no newline", []byte("go object darwin arm64"), "no newline"},
		{"no separator", []byte(testHeader + "main\n"), "separator"},
		{"truncated blocks", good[:blockAt+headerSize-1], "a header is"},
		{"wrong magic", damage(func(b []byte) []byte {
			copy(b[blockAt:], "\x00go119ld")
			return b
		}), "goobj magic"},
		{"first block inside the header", damage(func(b []byte) []byte {
			setOffset(b, obj.BlkAutolib, 4)
			return b
		}), "inside the"},
		{"offsets go backwards", damage(func(b []byte) []byte {
			setOffset(b, obj.BlkFile, offset(b, obj.BlkPkgIdx)-1)
			return b
		}), "before block"},
		{"last block past the end", damage(func(b []byte) []byte {
			setOffset(b, obj.BlkEnd, uint32(len(b))+1)
			return b
		}), "past the end"},
		{"a partial record", damage(func(b []byte) []byte {
			setOffset(b, obj.BlkHashed64def, offset(b, obj.BlkSymdef)+1)
			return b
		}), "and a record is"},
		{"one content hash too few", damage(func(b []byte) []byte {
			setOffset(b, obj.BlkNonpkgdef, offset(b, obj.BlkNonpkgdef)+symSize)
			return b
		}), "content hashes for"},
		// One symbol record moves from the hashed space into the short
		// hashed one, so the short hash block is a hash short.
		{"one short content hash too few", damage(func(b []byte) []byte {
			setOffset(b, obj.BlkHasheddef, offset(b, obj.BlkHasheddef)+symSize)
			return b
		}), "short content hashes for"},
		{"the index is one entry short", damage(func(b []byte) []byte {
			setOffset(b, obj.BlkAuxIdx, offset(b, obj.BlkAuxIdx)-4)
			return b
		}), "it must hold one more"},
		{"the index does not start at zero", damage(func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[blockAt+int(offset(b, obj.BlkAuxIdx)):], 1)
			return b
		}), "must start at 0"},
		{"the index goes backwards", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkDataIdx))
			binary.LittleEndian.PutUint32(b[at+4:], ^uint32(0))
			binary.LittleEndian.PutUint32(b[at+8:], 0)
			return b
		}), "goes from"},
		// The index totals one relocation fewer than the block holds, so
		// the last one belongs to no symbol.
		{"a relocation no symbol claims", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkRelocIdx))
			last := binary.LittleEndian.Uint32(b[at+4*7:])
			for i := 1; i <= 7; i++ {
				binary.LittleEndian.PutUint32(b[at+4*i:], last-1)
			}
			return b
		}), "the index accounts for"},
		{"an auxiliary entry no symbol claims", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkAuxIdx))
			last := binary.LittleEndian.Uint32(b[at+4*7:])
			for i := 1; i <= 7; i++ {
				binary.LittleEndian.PutUint32(b[at+4*i:], last-1)
			}
			return b
		}), "the index accounts for"},
		{"data no symbol claims", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkDataIdx)) + 4*7
			binary.LittleEndian.PutUint32(b[at:], binary.LittleEndian.Uint32(b[at:])-1)
			return b
		}), "the index accounts for"},
		{"a string outside the region", damage(func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[blockAt+int(offset(b, obj.BlkPkgIdx))+4:], 0)
			return b
		}), "outside the string region"},
		{"an auxiliary type the format does not define", damage(func(b []byte) []byte {
			b[blockAt+int(offset(b, obj.BlkAux))] = 200
			return b
		}), "which the format does not define"},
		{"a relocation to nothing", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkReloc))
			binary.LittleEndian.PutUint32(b[at+15:], obj.PkgIdxSelf)
			binary.LittleEndian.PutUint32(b[at+19:], 99)
			return b
		}), "that index space holds"},
		{"a relocation to package zero", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkReloc))
			binary.LittleEndian.PutUint32(b[at+15:], 0)
			binary.LittleEndian.PutUint32(b[at+19:], 1)
			return b
		}), "package index 0"},
		{"a relocation to a package the object does not reference", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkReloc))
			binary.LittleEndian.PutUint32(b[at+15:], 7)
			return b
		}), "references"},
		{"an auxiliary entry to nothing", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkAux))
			binary.LittleEndian.PutUint32(b[at+1:], obj.PkgIdxHashed)
			binary.LittleEndian.PutUint32(b[at+5:], 99)
			return b
		}), "that index space holds"},
		{"reference flags to nothing", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkRefFlags))
			binary.LittleEndian.PutUint32(b[at:], obj.PkgIdxNone)
			binary.LittleEndian.PutUint32(b[at+4:], 99)
			return b
		}), "reference flags 0"},
		{"a reference name to nothing", damage(func(b []byte) []byte {
			at := blockAt + int(offset(b, obj.BlkRefName))
			binary.LittleEndian.PutUint32(b[at:], obj.PkgIdxHashed64)
			binary.LittleEndian.PutUint32(b[at+4:], 99)
			return b
		}), "reference name 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadObject(c.obj, "round.o", "example.com/round")
			if err == nil {
				t.Fatal("the reader accounted for an object it cannot account for")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal is %q, it must name %q", err, c.want)
			}
			var fe *FormatError
			if !errorsAs(err, &fe) {
				t.Errorf("the refusal is %T, and a caller reading an archive needs the member and the offset", err)
			}
		})
	}
}

// errorsAs is errors.As, spelled out so the test names the one type it
// asks about.
func errorsAs(err error, target **FormatError) bool {
	fe, ok := err.(*FormatError)
	if ok {
		*target = fe
	}
	return ok
}

func TestFormatErrorMessage(t *testing.T) {
	if got := (&FormatError{File: "a.o", Off: 12, Msg: "bad"}).Error(); got != "link: a.o: at byte 12: bad" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&FormatError{File: "a.o", Off: -1, Msg: "bad"}).Error(); got != "link: a.o: bad" {
		t.Errorf("Error() = %q", got)
	}
}

func TestCoveredBytes(t *testing.T) {
	for _, c := range []struct {
		name  string
		spans []strSpan
		want  int
	}{
		{"none", nil, 0},
		{"one", []strSpan{{2, 5}}, 3},
		{"the same string twice", []strSpan{{2, 5}, {2, 5}}, 3},
		{"a suffix of another", []strSpan{{2, 8}, {5, 8}}, 6},
		{"disjoint, out of order", []strSpan{{9, 11}, {2, 5}}, 5},
		{"touching", []strSpan{{2, 5}, {5, 9}}, 7},
		{"overlapping", []strSpan{{2, 6}, {4, 9}}, 7},
	} {
		if got := coveredBytes(c.spans); got != c.want {
			t.Errorf("%s: coveredBytes = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestItoa(t *testing.T) {
	for _, c := range []int{0, 1, 9, 10, 4096, 1234567890} {
		if got, want := itoa(c), fmtInt(c); got != want {
			t.Errorf("itoa(%d) = %q, want %q", c, got, want)
		}
	}
}

func fmtInt(i int) string {
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	return string(out)
}
