// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package obj

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// object is a minimal reader for the blocks this package writes. It exists
// so a test can state what the bytes must be without trusting the writer
// that produced them. The real reader is go tool nm, which write_test.go
// runs.
type object struct {
	b       []byte
	offsets [NBlk]uint32
}

func parse(t *testing.T, b []byte) *object {
	t.Helper()
	if !bytes.HasPrefix(b, []byte(Magic)) {
		t.Fatalf("object does not start with the magic: %q", b[:min(len(b), 8)])
	}
	o := &object{b: b}
	off := uint32(len(Magic) + 8 + 4)
	for i := range o.offsets {
		o.offsets[i] = binary.LittleEndian.Uint32(b[off:])
		off += 4
	}
	for i := 1; i < NBlk; i++ {
		if o.offsets[i] < o.offsets[i-1] {
			t.Fatalf("block %d starts at %d, before block %d at %d", i, o.offsets[i], i-1, o.offsets[i-1])
		}
	}
	if got := int(o.offsets[BlkEnd]); got != len(b) {
		t.Fatalf("BlkEnd is %d, the object is %d bytes", got, len(b))
	}
	return o
}

// parsePrefix parses an object that has bytes after the last block. The
// compiler pads its output, so its objects are one byte longer than the
// blocks they hold.
func parsePrefix(t *testing.T, b []byte) *object {
	t.Helper()
	at := len(Magic) + 8 + 4 + 4*BlkEnd
	if len(b) < at+4 {
		t.Fatal("object is too short to hold a header")
	}
	end := binary.LittleEndian.Uint32(b[at:])
	if int(end) > len(b) {
		t.Fatalf("the last block ends at %d, past the end of %d bytes", end, len(b))
	}
	return parse(t, b[:end])
}

func (o *object) block(i int) []byte { return o.b[o.offsets[i]:o.offsets[i+1]] }

func (o *object) str(off uint32) string {
	n := binary.LittleEndian.Uint32(o.b[off:])
	at := binary.LittleEndian.Uint32(o.b[off+4:])
	return string(o.b[at : at+n])
}

// symRecord is one decoded SymbolDefs entry.
type symRecord struct {
	Name  string
	ABI   uint16
	Type  SymKind
	Flag  uint8
	Flag2 uint8
	Size  uint32
	Align uint32
}

const symRecordSize = 8 + 2 + 1 + 1 + 1 + 4 + 4

func (o *object) syms(blk int) []symRecord {
	b := o.block(blk)
	out := make([]symRecord, 0, len(b)/symRecordSize)
	for off := o.offsets[blk]; off < o.offsets[blk+1]; off += symRecordSize {
		out = append(out, symRecord{
			Name:  o.str(off),
			ABI:   binary.LittleEndian.Uint16(o.b[off+8:]),
			Type:  SymKind(o.b[off+10]),
			Flag:  o.b[off+11],
			Flag2: o.b[off+12],
			Size:  binary.LittleEndian.Uint32(o.b[off+13:]),
			Align: binary.LittleEndian.Uint32(o.b[off+17:]),
		})
	}
	return out
}

func (o *object) uint32s(blk int) []uint32 {
	b := o.block(blk)
	out := make([]uint32, 0, len(b)/4)
	for i := 0; i+4 <= len(b); i += 4 {
		out = append(out, binary.LittleEndian.Uint32(b[i:]))
	}
	return out
}

// sample builds a package that uses every block the writer knows.
func sample() *Package {
	p := NewPackage("example.com/x")
	p.Flags = ObjFlagStd
	p.Fingerprint = [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	p.AddImport("runtime", [8]byte{9})
	p.AddFile("/build/example.com/x/a.go")

	str := p.AddHashedDef(&Symbol{
		Name: "go:string.hello", Type: SRODATA, Size: 5, Align: 1,
		Data: []byte("hello"),
	})
	small := p.AddHashed64Def(&Symbol{
		Name: "example.com/x..stmp_0", Type: SRODATA, Size: 8, Align: 8,
		Data: []byte{1, 0, 0, 0, 0, 0, 0, 0},
	})
	gotype := p.AddNonPkgDef(&Symbol{Name: "type:int", Type: SRODATA, Size: 4, Align: 4, Data: []byte{0, 0, 0, 0}})
	ext := p.AddNonPkgRef(&Symbol{Name: "runtime.morestack_noctxt"})
	other := p.PkgRef("runtime", 7)

	p.AddDef(&Symbol{
		Name: "example.com/x.data", Type: SRODATA, Size: 16, Align: 8,
		Data: make([]byte, 16),
		Relocs: []Reloc{
			{Off: 8, Size: 8, Type: R_ADDR, Add: 3, Sym: str},
			{Off: 0, Size: 8, Type: R_ADDR, Sym: small},
		},
		Aux: []Aux{{Type: AuxGotype, Sym: gotype}},
	})
	p.AddDef(&Symbol{
		Name: "example.com/x.fn", ABI: ABIInternal, Type: STEXT, Size: 4, Align: 4,
		Flag: SymFlagNoSplit | SymFlagLeaf,
		Data: []byte{0xc0, 0x03, 0x5f, 0xd6},
		Relocs: []Reloc{
			{Off: 0, Size: 4, Type: R_CALLARM64, Sym: ext},
		},
		Aux: []Aux{{Type: AuxFuncInfo, Sym: other}, {Type: AuxFuncdata, Sym: str}},
	})
	p.AddDef(&Symbol{Name: "example.com/x.zero", Type: SNOPTRBSS, Size: 32, Align: 8})

	p.AddRefFlag(other, 0, SymFlagUsedInIface)
	p.AddRefName(other, "runtime.printlock")
	return p
}

func TestBytesLayout(t *testing.T) {
	p := sample()
	b, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	o := parse(t, b)

	if got := string(o.b[len(Magic) : len(Magic)+8]); got != string(p.Fingerprint[:]) {
		t.Errorf("fingerprint = %q, want %q", got, p.Fingerprint)
	}
	if got := binary.LittleEndian.Uint32(o.b[len(Magic)+8:]); got != ObjFlagStd {
		t.Errorf("flags = %d, want %d", got, ObjFlagStd)
	}

	// Autolib: one import, its path and its fingerprint.
	autolib := o.offsets[BlkAutolib]
	if got, want := o.str(autolib), "runtime"; got != want {
		t.Errorf("autolib[0] = %q, want %q", got, want)
	}
	if got := o.b[autolib+8]; got != 9 {
		t.Errorf("autolib[0] fingerprint = %d, want 9", got)
	}

	// PkgIndex: the invalid package, then runtime, in first-use order.
	if got, want := o.str(o.offsets[BlkPkgIdx]), ""; got != want {
		t.Errorf("pkgidx[0] = %q, want %q", got, want)
	}
	if got, want := o.str(o.offsets[BlkPkgIdx]+8), "runtime"; got != want {
		t.Errorf("pkgidx[1] = %q, want %q", got, want)
	}

	// Files are stored with forward slashes.
	if got, want := o.str(o.offsets[BlkFile]), "/build/example.com/x/a.go"; got != want {
		t.Errorf("file[0] = %q, want %q", got, want)
	}

	defs := o.syms(BlkSymdef)
	if len(defs) != 3 {
		t.Fatalf("got %d symbol definitions, want 3", len(defs))
	}
	want := []symRecord{
		{Name: "example.com/x.data", Type: SRODATA, Size: 16, Align: 8},
		{Name: "example.com/x.fn", ABI: ABIInternal, Type: STEXT, Flag: SymFlagNoSplit | SymFlagLeaf, Size: 4, Align: 4},
		{Name: "example.com/x.zero", Type: SNOPTRBSS, Size: 32, Align: 8},
	}
	for i := range want {
		if defs[i] != want[i] {
			t.Errorf("symbol %d = %+v, want %+v", i, defs[i], want[i])
		}
	}
	if got := o.syms(BlkHashed64def); len(got) != 1 || got[0].Name != "example.com/x..stmp_0" {
		t.Errorf("hashed64 defs = %+v", got)
	}
	if got := o.syms(BlkHasheddef); len(got) != 1 || got[0].Name != "go:string.hello" {
		t.Errorf("hashed defs = %+v", got)
	}
	if got := o.syms(BlkNonpkgdef); len(got) != 1 || got[0].Flag != SymFlagGoType {
		t.Errorf("non-package defs = %+v, want type:int with SymFlagGoType", got)
	}
	if got := o.syms(BlkNonpkgref); len(got) != 1 || got[0].Name != "runtime.morestack_noctxt" {
		t.Errorf("non-package refs = %+v", got)
	}

	// A short content-addressable symbol stores its content, not a hash.
	if got, want := o.block(BlkHash64), []byte{1, 0, 0, 0, 0, 0, 0, 0}; !bytes.Equal(got, want) {
		t.Errorf("Hash64 block = %v, want the symbol content %v", got, want)
	}
	if got := len(o.block(BlkHash)); got != 16 {
		t.Errorf("Hash block is %d bytes, want 16 for one hashed symbol", got)
	}

	// The index blocks cover the four definition lists and carry a final
	// total, so they hold one more entry than there are definitions.
	nDefs := 3 + 1 + 1 + 1
	for _, blk := range []int{BlkRelocIdx, BlkAuxIdx, BlkDataIdx} {
		if got := len(o.uint32s(blk)); got != nDefs+1 {
			t.Errorf("index block %d has %d entries, want %d", blk, got, nDefs+1)
		}
	}
	relocIdx := o.uint32s(BlkRelocIdx)
	if got, want := relocIdx, []uint32{0, 2, 3, 3, 3, 3, 3}; !equal(got, want) {
		t.Errorf("RelocIndex = %v, want %v", got, want)
	}
	auxIdx := o.uint32s(BlkAuxIdx)
	if got, want := auxIdx, []uint32{0, 1, 3, 3, 3, 3, 3}; !equal(got, want) {
		t.Errorf("AuxIndex = %v, want %v", got, want)
	}
	dataIdx := o.uint32s(BlkDataIdx)
	if got, want := dataIdx, []uint32{0, 16, 20, 20, 28, 33, 37}; !equal(got, want) {
		t.Errorf("DataIndex = %v, want %v", got, want)
	}

	// Relocations are written in offset order, whatever order they arrived in.
	rel := o.block(BlkReloc)
	if len(rel) != 3*23 {
		t.Fatalf("Reloc block is %d bytes, want 3 records of 23", len(rel))
	}
	if got := int32(binary.LittleEndian.Uint32(rel)); got != 0 {
		t.Errorf("first relocation is at offset %d, want 0", got)
	}
	if got := int32(binary.LittleEndian.Uint32(rel[23:])); got != 8 {
		t.Errorf("second relocation is at offset %d, want 8", got)
	}
	if got, want := binary.LittleEndian.Uint64(rel[23+7:]), uint64(3); got != want {
		t.Errorf("second relocation addend = %d, want %d", got, want)
	}

	aux := o.block(BlkAux)
	if len(aux) != 3*9 {
		t.Fatalf("Aux block is %d bytes, want 3 records of 9", len(aux))
	}
	if AuxType(aux[0]) != AuxGotype || AuxType(aux[9]) != AuxFuncInfo || AuxType(aux[18]) != AuxFuncdata {
		t.Errorf("aux types = %d %d %d", aux[0], aux[9], aux[18])
	}

	// Data holds the four definition lists back to back. A zero-filled
	// symbol contributes nothing, which is why DataIndex repeats.
	if got, want := string(o.block(BlkData)), string(make([]byte, 16))+"\xc0\x03\x5f\xd6"+"\x01\x00\x00\x00\x00\x00\x00\x00"+"hello"+string(make([]byte, 4)); got != want {
		t.Errorf("Data block = %q, want %q", got, want)
	}

	refFlags := o.block(BlkRefFlags)
	if len(refFlags) != 10 || refFlags[9] != SymFlagUsedInIface {
		t.Errorf("RefFlags block = %v", refFlags)
	}
	refName := o.block(BlkRefName)
	if len(refName) != 16 {
		t.Fatalf("RefName block is %d bytes, want 16", len(refName))
	}
	if got := o.str(o.offsets[BlkRefName] + 8); got != "runtime.printlock" {
		t.Errorf("RefName[0] = %q", got)
	}
}

func equal(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEmptyPackage(t *testing.T) {
	// A package with nothing in it still has every block, because the
	// reader finds a block's length from the offset of the next one.
	b, err := NewPackage("p").Bytes()
	if err != nil {
		t.Fatal(err)
	}
	o := parse(t, b)
	for i := BlkFile; i < NBlk-1; i++ {
		if got := o.offsets[i+1] - o.offsets[i]; got != 0 && i != BlkRelocIdx && i != BlkAuxIdx && i != BlkDataIdx {
			t.Errorf("block %d holds %d bytes in an empty package", i, got)
		}
	}
	// The index blocks still carry their final total, which is zero.
	for _, blk := range []int{BlkRelocIdx, BlkAuxIdx, BlkDataIdx} {
		if got := o.uint32s(blk); len(got) != 1 || got[0] != 0 {
			t.Errorf("index block %d = %v, want one zero", blk, got)
		}
	}
	// The package index always holds the invalid package at index 0.
	if got := o.offsets[BlkFile] - o.offsets[BlkPkgIdx]; got != 8 {
		t.Errorf("PkgIndex holds %d bytes, want the 8 of the invalid package", got)
	}
}

func TestStringTableDeduplicates(t *testing.T) {
	p := NewPackage("p")
	// The same name arrives three times, from three index spaces.
	p.AddDef(&Symbol{Name: "dup", Type: SRODATA})
	p.AddNonPkgDef(&Symbol{Name: "dup", Type: SRODATA})
	p.AddNonPkgRef(&Symbol{Name: "dup"})
	b, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	o := parse(t, b)
	strings := o.b[len(Magic)+8+4+4*NBlk : o.offsets[BlkAutolib]]
	if n := bytes.Count(strings, []byte("dup")); n != 1 {
		t.Errorf("the string table holds %q %d times, want 1", "dup", n)
	}
	// Every reference points at the one copy.
	defs := o.syms(BlkSymdef)
	refs := o.syms(BlkNonpkgref)
	if defs[0].Name != "dup" || refs[0].Name != "dup" {
		t.Errorf("names = %q %q, want both dup", defs[0].Name, refs[0].Name)
	}
}

func TestIndexSpaces(t *testing.T) {
	p := NewPackage("p")
	if got := p.AddDef(&Symbol{Name: "a"}); got != (SymRef{PkgIdxSelf, 0}) {
		t.Errorf("first definition = %v", got)
	}
	if got := p.AddHashed64Def(&Symbol{Name: "b"}); got != (SymRef{PkgIdxHashed64, 0}) {
		t.Errorf("first short hashed definition = %v", got)
	}
	if got := p.AddHashedDef(&Symbol{Name: "c"}); got != (SymRef{PkgIdxHashed, 0}) {
		t.Errorf("first hashed definition = %v", got)
	}
	d0 := p.AddNonPkgDef(&Symbol{Name: "d"})
	d1 := p.AddNonPkgDef(&Symbol{Name: "e"})
	// References continue the definition numbering, they do not restart it.
	// A reference holds its position and gets its index when the object is
	// written, so it is read through resolve.
	r0 := p.AddNonPkgRef(&Symbol{Name: "f"})
	if d0 != (SymRef{PkgIdxNone, 0}) || d1 != (SymRef{PkgIdxNone, 1}) || p.resolve(r0) != (SymRef{PkgIdxNone, 2}) {
		t.Errorf("non-package indices = %v %v %v", d0, d1, p.resolve(r0))
	}
	if got, want := p.PkgIndex("runtime"), uint32(1); got != want {
		t.Errorf("first referenced package index = %d, want %d", got, want)
	}
	if got := p.PkgIndex("runtime"); got != 1 {
		t.Errorf("interning a package twice gave index %d", got)
	}
	if got := p.PkgRef("sync", 3); got != (SymRef{2, 3}) {
		t.Errorf("PkgRef = %v, want {2 3}", got)
	}
	if (SymRef{}).IsZero() != true || (SymRef{PkgIdxSelf, 0}).IsZero() {
		t.Error("SymRef.IsZero is wrong")
	}
}

// TestNonPkgDefAfterRefKeepsTheReference is the rule that a definition added
// after a reference does not move the reference.
//
// The NonPkgRefs array continues the NonPkgDefs array, so a reference numbered
// when it was added takes the index of a definition added later. The linker
// then reads the definition where the reference should be, and a relocation
// that named an undefined symbol resolves to a symbol this object defines.
// That is how one dupok definition and one by-name reference to the same
// symbol end up as two symbols in the linked binary.
func TestNonPkgDefAfterRefKeepsTheReference(t *testing.T) {
	p := NewPackage("p")
	ref := p.AddNonPkgRef(&Symbol{Name: "runtime.newobject"})
	def := p.AddNonPkgDef(&Symbol{Name: "type:*int32", Type: SRODATA, Size: 1, Align: 1, Flag: SymFlagDupok, Data: []byte{0}})
	if got, want := p.resolve(def), (SymRef{PkgIdxNone, 0}); got != want {
		t.Errorf("the definition is %v, want %v", got, want)
	}
	if got, want := p.resolve(ref), (SymRef{PkgIdxNone, 1}); got != want {
		t.Errorf("the reference is %v, want %v; a definition added after it took its index", got, want)
	}
	// The name the linker resolves the reference by is the referenced one.
	if got, err := p.nonPkgName(p.resolve(ref).SymIdx); err != nil || got != "runtime.newobject" {
		t.Errorf("the reference names %q (%v), want %q", got, err, "runtime.newobject")
	}
	if got, err := p.nonPkgName(p.resolve(def).SymIdx); err != nil || got != "type:*int32" {
		t.Errorf("the definition names %q (%v), want %q", got, err, "type:*int32")
	}
}

func TestAddFileNormalizesAndDeduplicates(t *testing.T) {
	p := NewPackage("p")
	i := p.AddFile("a/b.go")
	if j := p.AddFile("a/b.go"); i != j {
		t.Errorf("the same file got indices %d and %d", i, j)
	}
	if k := p.AddFile("c.go"); k != 1 {
		t.Errorf("second file index = %d, want 1", k)
	}
}

func TestCheckRejects(t *testing.T) {
	tests := []struct {
		name string
		fix  func(*Package)
		want string
	}{
		{"empty path", func(p *Package) { p.Path = "" }, "empty package path"},
		{"empty name", func(p *Package) { p.AddDef(&Symbol{Type: SDATA}) }, "empty name"},
		{"size disagrees with data", func(p *Package) {
			p.AddDef(&Symbol{Name: "s", Type: SDATA, Size: 4, Data: []byte{1}})
		}, "does not match"},
		{"symbol too large", func(p *Package) {
			p.AddDef(&Symbol{Name: "s", Type: SBSS, Size: 3e9})
		}, "too large"},
		{"relocation off the end", func(p *Package) {
			p.AddDef(&Symbol{Name: "s", Type: SDATA, Size: 4, Data: []byte{0, 0, 0, 0},
				Relocs: []Reloc{{Off: 2, Size: 4, Type: R_ADDR}}})
		}, "outside the symbol"},
		{"duplicate-tolerant package definition", func(p *Package) {
			p.AddDef(&Symbol{Name: "type:*int32", Type: SRODATA, Size: 1, Align: 1, Flag: SymFlagDupok, Data: []byte{0}})
		}, "non-package index space"},
		{"short hashed symbol too long", func(p *Package) {
			p.AddHashed64Def(&Symbol{Name: "s", Type: SRODATA, Size: 9, Align: 1, Data: make([]byte, 9)})
		}, "at most 8"},
		{"short hashed symbol with relocations", func(p *Package) {
			p.AddHashed64Def(&Symbol{Name: "s", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8),
				Relocs: []Reloc{{Off: 0, Size: 8, Type: R_ADDR}}})
		}, "not its identity"},
		{"short hashed symbol in another section", func(p *Package) {
			p.AddHashed64Def(&Symbol{Name: "type:x", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8)})
		}, "default section"},
		{"content-addressable without alignment", func(p *Package) {
			p.AddHashedDef(&Symbol{Name: "s", Type: SRODATA, Size: 16, Data: make([]byte, 16)})
		}, "no alignment"},
		{"reference carrying content", func(p *Package) {
			p.AddNonPkgRef(&Symbol{Name: "s", Data: []byte{1}, Size: 1})
		}, "index blocks do not cover references"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPackage("p")
			tt.fix(p)
			_, err := p.Bytes()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestDerivedFlags(t *testing.T) {
	p := NewPackage("example.com/x")
	p.AddDef(&Symbol{Name: "type:int", Type: SRODATA, Size: 1, Data: []byte{0}})
	p.AddDef(&Symbol{Name: "type:.eq.int", Type: SRODATA, Size: 1, Data: []byte{0}})
	p.AddDef(&Symbol{Name: "type:int", Type: SDATA, Size: 1, Data: []byte{0}})
	p.AddDef(&Symbol{Name: "go:itab.a,b", Type: SRODATA, Size: 1, Data: []byte{0}})
	p.AddDef(&Symbol{Name: "main.main", Type: STEXT, Size: 1, Data: []byte{0}})
	p.AddDef(&Symbol{Name: "example.com/x..dict.F[int]", Type: SRODATA, Size: 1, Data: []byte{0}})
	b, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	defs := parse(t, b).syms(BlkSymdef)
	want := []struct{ flag, flag2 uint8 }{
		{SymFlagGoType, 0},
		{0, 0}, // a type: method, not a type descriptor
		{0, 0}, // the right name in the wrong section
		{0, SymFlagItab},
		{0, SymFlagLinkname}, // the runtime linknames main.main
		{0, SymFlagDict},
	}
	for i, w := range want {
		if defs[i].Flag != w.flag || defs[i].Flag2 != w.flag2 {
			t.Errorf("%s: flags = %d %d, want %d %d", defs[i].Name, defs[i].Flag, defs[i].Flag2, w.flag, w.flag2)
		}
	}
}

func TestContentHashIdentity(t *testing.T) {
	// Identity is content, so the same bytes in two packages hash the same
	// and different bytes do not. Naming is what a relocation uses before
	// the linker merges anything, so the name is not in the hash for data.
	hashOf := func(mutate func(*Symbol), path string) [16]byte {
		p := NewPackage(path)
		s := &Symbol{Name: "go:string.a", Type: SRODATA, Size: 5, Align: 1, Data: []byte("hello")}
		mutate(s)
		p.AddHashedDef(s)
		h, err := p.contentHash(0)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hashOf(func(*Symbol) {}, "a")
	if other := hashOf(func(*Symbol) {}, "b"); other != base {
		t.Error("the same content in two packages hashed differently, so the linker would keep two copies")
	}
	if other := hashOf(func(s *Symbol) { s.Name = "go:string.b" }, "a"); other != base {
		t.Error("a rename changed the hash of a data symbol, but identity is content")
	}
	if other := hashOf(func(s *Symbol) { s.Data = []byte("world") }, "a"); other == base {
		t.Error("different content hashed the same")
	}
	if other := hashOf(func(s *Symbol) { s.Size, s.Data = 6, []byte("hello\x00") }, "a"); other == base {
		t.Error("a longer symbol with the same leading bytes hashed the same")
	}
	if other := hashOf(func(s *Symbol) { s.Pcdata = true }, "a"); other == base {
		t.Error("a pc-value table hashed the same as read only data, so content addressing could move it between sections")
	}
	if other := hashOf(func(s *Symbol) { s.Name = "type:x" }, "a"); other == base {
		t.Error("a type descriptor hashed the same as an ordinary symbol")
	}

	// A function's name is in the hash: two unrelated functions that
	// encode to the same bytes are still two functions.
	textHash := func(name string) [16]byte {
		p := NewPackage("a")
		p.AddHashedDef(&Symbol{Name: name, Type: STEXT, Size: 4, Align: 4, Data: []byte{1, 2, 3, 4}})
		h, err := p.contentHash(0)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if textHash("a.f") == textHash("a.g") {
		t.Error("two functions with the same code hashed the same")
	}
}

func TestContentHashSection(t *testing.T) {
	// The mnemonic keeps content addressing from moving a symbol into
	// another section. Two symbols with the same bytes but different
	// classes must not merge, so every class needs its own letter.
	tests := []struct {
		sym  Symbol
		want byte
	}{
		{Symbol{Name: "a.f", Type: STEXT}, 't'},
		{Symbol{Name: "a.f", Type: STEXTFIPS}, 'f'},
		{Symbol{Name: "a.f.pcsp", Type: SRODATA, Pcdata: true}, 'P'},
		{Symbol{Name: "gcargs.0", Type: SRODATA}, 'F'},
		{Symbol{Name: "gclocals.0", Type: SRODATA}, 'F'},
		{Symbol{Name: "gclocals·abc", Type: SRODATA}, 'F'},
		{Symbol{Name: "a.f.opendefer", Type: SRODATA}, 'F'},
		{Symbol{Name: "a.f.arginfo0", Type: SRODATA}, 'F'},
		{Symbol{Name: "a.f.arginfo1", Type: SRODATA}, 'F'},
		{Symbol{Name: "a.f.argliveinfo", Type: SRODATA}, 'F'},
		{Symbol{Name: "a.f.wrapinfo", Type: SRODATA}, 'F'},
		{Symbol{Name: "a.f.args_stackmap", Type: SRODATA}, 'F'},
		{Symbol{Name: "a.f.stkobj", Type: SRODATA}, 'F'},
		{Symbol{Name: "type:int", Type: SRODATA}, 'T'},
		{Symbol{Name: "go:string.hello", Type: SRODATA}, 0},
		{Symbol{Name: "a.data", Type: SDATA}, 0},
	}
	for _, tt := range tests {
		if got := contentHashSection(&tt.sym); got != tt.want {
			t.Errorf("%s: section %q, want %q", tt.sym.Name, got, tt.want)
		}
	}
}

func TestContentHashCoversRelocations(t *testing.T) {
	build := func(target func(*Package) SymRef) [16]byte {
		p := NewPackage("example.com/x")
		p.AddHashed64Def(&Symbol{Name: "small", Type: SRODATA, Size: 8, Align: 8, Data: []byte{7, 0, 0, 0, 0, 0, 0, 0}})
		p.AddNonPkgDef(&Symbol{Name: "runtime.x", Type: SRODATA, Size: 1, Data: []byte{0}})
		p.AddDef(&Symbol{Name: "example.com/x.y", Type: SRODATA, Size: 1, Data: []byte{0}})
		ref := p.AddHashedDef(&Symbol{Name: "h", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8)})
		p.AddHashedDef(&Symbol{
			Name: "s", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8),
			Relocs: []Reloc{{Off: 0, Size: 8, Type: R_ADDR, Sym: target(p)}},
		})
		_ = ref
		h, err := p.contentHash(1)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	kinds := map[string]func(*Package) SymRef{
		"nil":        func(*Package) SymRef { return SymRef{} },
		"self":       func(*Package) SymRef { return SymRef{PkgIdxHashed, 1} },
		"hashed64":   func(*Package) SymRef { return SymRef{PkgIdxHashed64, 0} },
		"hashed":     func(*Package) SymRef { return SymRef{PkgIdxHashed, 0} },
		"non-pkg":    func(*Package) SymRef { return SymRef{PkgIdxNone, 0} },
		"builtin":    func(*Package) SymRef { return SymRef{PkgIdxBuiltin, 2} },
		"self pkg":   func(*Package) SymRef { return SymRef{PkgIdxSelf, 0} },
		"other pkg":  func(p *Package) SymRef { return p.PkgRef("runtime", 4) },
		"other idx":  func(p *Package) SymRef { return p.PkgRef("runtime", 5) },
		"other pkg2": func(p *Package) SymRef { return p.PkgRef("sync", 4) },
	}
	seen := map[[16]byte]string{}
	for name, target := range kinds {
		h := build(target)
		if prev, ok := seen[h]; ok {
			t.Errorf("a relocation to %s hashed the same as one to %s", name, prev)
		}
		seen[h] = name
	}
	if len(seen) != len(kinds) {
		t.Errorf("got %d distinct hashes for %d relocation targets", len(seen), len(kinds))
	}
}

func TestContentHashFollowsRelocationOrder(t *testing.T) {
	// gc writes the hash blocks before it sorts the relocations of a
	// symbol, so the hash covers the relocations in the order the compiler
	// produced them. Hashing them sorted would give a different identity
	// for the same symbol, and the linker would keep two copies of what is
	// one thing.
	hash := func(rs []Reloc) [16]byte {
		p := NewPackage("p")
		p.AddNonPkgDef(&Symbol{Name: "runtime.a", Type: SRODATA, Size: 1, Data: []byte{0}})
		p.AddNonPkgDef(&Symbol{Name: "runtime.b", Type: SRODATA, Size: 1, Data: []byte{0}})
		p.AddHashedDef(&Symbol{Name: "s", Type: SRODATA, Size: 16, Align: 8, Data: make([]byte, 16), Relocs: rs})
		h, err := p.contentHash(0)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	a := Reloc{Off: 0, Size: 8, Type: R_ADDR, Sym: SymRef{PkgIdxNone, 0}}
	b := Reloc{Off: 8, Size: 8, Type: R_ADDR, Sym: SymRef{PkgIdxNone, 1}}
	if hash([]Reloc{a, b}) == hash([]Reloc{b, a}) {
		t.Error("the hash is the same for two relocation orders, so it sorts before hashing and gc does not")
	}
}

func TestContentHashErrors(t *testing.T) {
	// These references never reach a written object, because check
	// rejects them first. The hash still has to refuse them rather than
	// index past the end of a list.
	tests := []struct {
		name   string
		target SymRef
		want   string
	}{
		{"short hashed out of range", SymRef{PkgIdxHashed64, 3}, "short hashed symbol 3"},
		{"hashed out of range", SymRef{PkgIdxHashed, 9}, "hashed symbol 9"},
		{"non-package out of range", SymRef{PkgIdxNone, 4}, "non-package symbol 4"},
		{"package out of range", SymRef{7, 0}, "package 7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPackage("p")
			p.AddHashedDef(&Symbol{Name: "s", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8),
				Relocs: []Reloc{{Off: 0, Size: 8, Type: R_ADDR, Sym: tt.target}}})
			_, err := p.contentHash(0)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestCheckRefRejects(t *testing.T) {
	// A reference to a symbol that is not there becomes an index the
	// linker reads past the end of an array with, so it stops here.
	tests := []struct {
		name string
		ref  SymRef
		want string
	}{
		{"invalid package", SymRef{PkgIdxInvalid, 2}, "package index 0"},
		{"unknown package", SymRef{9, 0}, "only 0 packages are referenced"},
		{"own package", SymRef{PkgIdxSelf, 5}, "only 1 symbols"},
		{"short hashed", SymRef{PkgIdxHashed64, 1}, "only 0 symbols"},
		{"hashed", SymRef{PkgIdxHashed, 1}, "only 0 symbols"},
		{"non-package", SymRef{PkgIdxNone, 1}, "only 0 symbols"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, where := range []string{"relocation", "auxiliary symbol", "reference flags", "reference name"} {
				p := NewPackage("p")
				s := &Symbol{Name: "s", Type: SDATA, Size: 8, Data: make([]byte, 8)}
				switch where {
				case "relocation":
					s.Relocs = []Reloc{{Off: 0, Size: 8, Type: R_ADDR, Sym: tt.ref}}
				case "auxiliary symbol":
					s.Aux = []Aux{{Type: AuxGotype, Sym: tt.ref}}
				case "reference flags":
					p.AddRefFlag(tt.ref, 0, SymFlagUsedInIface)
				case "reference name":
					p.AddRefName(tt.ref, "other.sym")
				}
				p.AddDef(s)
				_, err := p.Bytes()
				if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), where) {
					t.Errorf("%s: error = %v, want one containing %q and %q", where, err, tt.want, where)
				}
			}
		})
	}
	// The nil symbol is a marker, not a mistake, and a builtin index is
	// the linker's to check.
	p := NewPackage("p")
	p.AddDef(&Symbol{Name: "s", Type: SDATA, Size: 8, Data: make([]byte, 8), Relocs: []Reloc{
		{Off: 0, Size: 0, Type: R_USEIFACE},
		{Off: 0, Size: 0, Type: R_CALL, Sym: SymRef{PkgIdxBuiltin, 3}},
	}})
	if _, err := p.Bytes(); err != nil {
		t.Errorf("a marker relocation and a builtin reference were rejected: %v", err)
	}
}

func TestContentHashCycle(t *testing.T) {
	// gc assumes hashed symbols form no cycle and recurses until the stack
	// runs out. Report it instead.
	p := NewPackage("p")
	p.AddHashedDef(&Symbol{Name: "a", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8),
		Relocs: []Reloc{{Off: 0, Size: 8, Type: R_ADDR, Sym: SymRef{PkgIdxHashed, 1}}}})
	p.AddHashedDef(&Symbol{Name: "b", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8),
		Relocs: []Reloc{{Off: 0, Size: 8, Type: R_ADDR, Sym: SymRef{PkgIdxHashed, 0}}}})
	_, err := p.Bytes()
	if err == nil || !strings.Contains(err.Error(), "reference cycle") {
		t.Fatalf("error = %v, want one about a reference cycle", err)
	}
}

func TestContentHashIsCached(t *testing.T) {
	// A chain of references costs exponential time without the cache. 40
	// links finish instantly with it and never finish without it.
	const n = 40
	p := NewPackage("p")
	p.AddHashedDef(&Symbol{Name: "s0", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8)})
	for i := 1; i < n; i++ {
		prev := SymRef{PkgIdxHashed, uint32(i - 1)}
		p.AddHashedDef(&Symbol{
			Name: fmt.Sprintf("s%d", i), Type: SRODATA, Size: 24, Align: 8, Data: make([]byte, 24),
			Relocs: []Reloc{
				{Off: 0, Size: 8, Type: R_ADDR, Sym: prev},
				{Off: 8, Size: 8, Type: R_ADDR, Sym: prev},
				{Off: 16, Size: 8, Type: R_ADDR, Sym: prev},
			},
		})
	}
	if _, err := p.Bytes(); err != nil {
		t.Fatal(err)
	}
}

func TestSortRelocsIsTotal(t *testing.T) {
	// Two relocations at one offset must not swap between runs.
	in := []Reloc{
		{Off: 4, Size: 8, Type: R_ADDR, Add: 2, Sym: SymRef{PkgIdxSelf, 1}},
		{Off: 4, Size: 8, Type: R_ADDR, Add: 1, Sym: SymRef{PkgIdxSelf, 1}},
		{Off: 4, Size: 8, Type: R_ADDR, Add: 1, Sym: SymRef{PkgIdxSelf, 0}},
		{Off: 4, Size: 8, Type: R_ADDR, Add: 1, Sym: SymRef{PkgIdxNone, 0}},
		{Off: 4, Size: 4, Type: R_CALL, Add: 1, Sym: SymRef{PkgIdxSelf, 0}},
		{Off: 0, Size: 8, Type: R_ADDR, Sym: SymRef{PkgIdxSelf, 0}},
		{Off: 4, Size: 4, Type: R_ADDR, Add: 1, Sym: SymRef{PkgIdxSelf, 0}},
	}
	first := sortRelocs(in)
	for i := 0; i < 20; i++ {
		if got := sortRelocs(in); !reflectEqual(got, first) {
			t.Fatalf("sortRelocs is not stable:\n%v\n%v", got, first)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i].Off < first[i-1].Off {
			t.Errorf("relocation %d is at %d, after %d", i, first[i].Off, first[i-1].Off)
		}
	}
	if len(sortRelocs(nil)) != 0 {
		t.Error("sorting no relocations produced some")
	}
}

func reflectEqual(a, b []Reloc) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWriteObjectHeaderCheck(t *testing.T) {
	p := sample()
	var buf bytes.Buffer
	for _, bad := range []string{"", "go object darwin arm64", "not a header\n"} {
		if err := p.WriteObject(&buf, bad); err == nil {
			t.Errorf("WriteObject accepted the header %q", bad)
		}
	}
	if err := p.WriteObject(&buf, "go object darwin arm64 go1.27.0\n"); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("go object darwin arm64 go1.27.0\n!\n\x00go120ld")) {
		t.Errorf("object starts with %q", buf.Bytes()[:40])
	}
	// A failing package must not reach the writer.
	bad := NewPackage("")
	if err := bad.WriteObject(&buf, "go object x\n"); err == nil {
		t.Error("WriteObject wrote a package with no path")
	}
}

func TestWriteObjectPropagatesWriteErrors(t *testing.T) {
	p := sample()
	hdr := "go object darwin arm64 go1.27.0\n"
	for _, n := range []int{0, len(hdr) + 2} {
		if err := p.WriteObject(&shortWriter{n: n}, hdr); err == nil {
			t.Errorf("WriteObject ignored a write error after %d bytes", n)
		}
	}
}

type shortWriter struct{ n int }

func (w *shortWriter) Write(b []byte) (int, error) {
	if len(b) > w.n {
		return 0, fmt.Errorf("short write")
	}
	w.n -= len(b)
	return len(b), nil
}

func TestParseToolchainObject(t *testing.T) {
	good := "go object darwin arm64 go1.27.0 X:none\n!\n" + Magic + "rest"
	tc, err := parseToolchainObject([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if tc.Header != "go object darwin arm64 go1.27.0 X:none\n" || tc.Magic != Magic {
		t.Errorf("parsed %+v", tc)
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"not an object", "ELF...\n", "not a Go object file"},
		{"no newline", "go object darwin arm64", "not a Go object file"},
		{"no separator", "go object x\n\x00go120ld", "no block separator"},
		{"truncated", "go object x\n!\n\x00go1", "no goobj magic"},
		{"other version", "go object x\n!\n\x00go121ld", "format mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseToolchainObject([]byte(tt.in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// decodePCData reads a pc-value table the way the runtime does. It is
// written from the format description rather than from the encoder, so a
// round trip through both checks the encoder against the format and not
// against itself.
func decodePCData(b []byte, minLC int) (entries []PCEntry, end int64, err error) {
	pc := int64(0)
	val := int32(-1)
	for len(b) > 0 {
		d, n := binary.Varint(b)
		if n <= 0 {
			return nil, 0, fmt.Errorf("bad value delta at %d", len(b))
		}
		b = b[n:]
		if d == 0 {
			return entries, pc, nil
		}
		val += int32(d)
		entries = append(entries, PCEntry{PC: pc, Value: val})
		u, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, 0, fmt.Errorf("bad pc delta at %d", len(b))
		}
		b = b[n:]
		pc += int64(u) * int64(minLC)
	}
	return nil, 0, fmt.Errorf("table ended without a terminator")
}

func TestEncodePCData(t *testing.T) {
	tests := []struct {
		name     string
		entries  []PCEntry
		funcSize int64
		minLC    int
		want     []byte
	}{
		{
			// The value goes from the assumed -1 to 0, so the first delta
			// is 1, which zig-zag codes as 2. Then 48 bytes of code are 12
			// instructions, and the table ends.
			name: "one value", entries: []PCEntry{{0, 0}}, funcSize: 48, minLC: 4,
			want: []byte{0x02, 0x0c, 0x00},
		},
		{
			name: "two values", entries: []PCEntry{{0, 0}, {8, 16}}, funcSize: 16, minLC: 4,
			want: []byte{0x02, 0x02, 0x20, 0x02, 0x00},
		},
		{
			// A value that falls stays one byte because the delta is
			// zig-zag coded: -16 becomes 31.
			name: "falling value", entries: []PCEntry{{0, 16}, {4, 0}}, funcSize: 8, minLC: 4,
			want: []byte{0x22, 0x01, 0x1f, 0x01, 0x00},
		},
		{
			// One byte per instruction on a target with no alignment.
			name: "minLC 1", entries: []PCEntry{{0, 1}, {3, 2}}, funcSize: 5, minLC: 1,
			want: []byte{0x04, 0x03, 0x02, 0x02, 0x00},
		},
		{
			// A repeated value is not a change, so it takes no bytes.
			name: "repeated value", entries: []PCEntry{{0, 3}, {4, 3}, {8, 3}}, funcSize: 12, minLC: 4,
			want: []byte{0x08, 0x03, 0x00},
		},
		{
			// A large delta needs a second byte, and the pc delta is
			// counted in instructions, so 4096 bytes are 1024 of them.
			name: "multi byte", entries: []PCEntry{{0, 300}}, funcSize: 4096, minLC: 4,
			want: []byte{0xda, 0x04, 0x80, 0x08, 0x00},
		},
		{name: "no entries", entries: nil, funcSize: 16, minLC: 4, want: nil},
		{
			name: "only no-op entries", entries: []PCEntry{{0, -1}}, funcSize: 16, minLC: 4, want: nil,
		},
	}
	n := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EncodePCData(tt.entries, tt.funcSize, tt.minLC)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("encoded % x, want % x", got, tt.want)
			}
			if len(got) == 0 {
				return
			}
			n++
			// The decoder reads back the changes, which are the entries
			// with the no-op ones removed.
			var want []PCEntry
			val := int32(-1)
			for _, e := range tt.entries {
				if e.Value != val {
					want = append(want, e)
					val = e.Value
				}
			}
			back, end, err := decodePCData(got, tt.minLC)
			if err != nil {
				t.Fatal(err)
			}
			if end != tt.funcSize {
				t.Errorf("decoded end %d, want %d", end, tt.funcSize)
			}
			if len(back) != len(want) {
				t.Fatalf("decoded %v, want %v", back, want)
			}
			for i := range want {
				if back[i] != want[i] {
					t.Errorf("entry %d = %v, want %v", i, back[i], want[i])
				}
			}
		})
	}
	if n == 0 {
		t.Fatal("no table was decoded")
	}
	t.Logf("decoded %d tables back to their entries", n)
}

func TestEncodePCDataRejects(t *testing.T) {
	tests := []struct {
		name     string
		entries  []PCEntry
		funcSize int64
		minLC    int
		want     string
	}{
		{"bad minLC", []PCEntry{{0, 1}}, 4, 0, "minimum instruction length"},
		{"late start", []PCEntry{{4, 1}}, 8, 4, "must start at 0"},
		{"unaligned function", []PCEntry{{0, 1}}, 6, 4, "not a multiple"},
		{"unaligned entry", []PCEntry{{0, 1}, {6, 2}}, 8, 4, "not a multiple"},
		{"out of order", []PCEntry{{0, 1}, {8, 2}, {4, 3}}, 12, 4, "does not follow"},
		{"past the end", []PCEntry{{0, 1}, {16, 2}}, 8, 4, "past the end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EncodePCData(tt.entries, tt.funcSize, tt.minLC)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestSymKindIsText(t *testing.T) {
	for _, k := range []SymKind{STEXT, STEXTFIPS} {
		if !k.IsText() {
			t.Errorf("%d is not reported as text", k)
		}
	}
	for _, k := range []SymKind{Sxxx, SRODATA, SDATA, SBSS} {
		if k.IsText() {
			t.Errorf("%d is reported as text", k)
		}
	}
}

func TestDeterminismInOneProcess(t *testing.T) {
	first, err := sample().Bytes()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		b, err := sample().Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, first) {
			t.Fatalf("run %d produced different bytes", i)
		}
	}
	// The same Package written twice must also agree: nothing in the
	// writer may consume state.
	p := sample()
	a, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("writing one package twice produced different bytes")
	}
	t.Logf("compared %d objects of %d bytes", 27, len(first))
}

// TestInitOrderRelocationCarriesNoBytes states the two properties cmd/link
// relies on for an ordering edge between initialisation records.
//
// The relocation changes nothing: it has no offset, no width, and no addend,
// so it can point at a symbol whose bytes this object never sees. relocsym
// returns on a zero width before it looks at the target at all, which is what
// lets a compiler emit one edge per import without first knowing which
// imports have a record. The check that bounds a relocation by its symbol's
// size must let a zero-width edge into a zero-length symbol through.
func TestInitOrderRelocationCarriesNoBytes(t *testing.T) {
	if R_INITORDER != 102 {
		t.Fatalf("R_INITORDER = %d, want 102: cmd/internal/objabi's stringer asserts _ = x[R_INITORDER-102]", R_INITORDER)
	}
	p := NewPackage("p")
	dep := p.AddNonPkgRef(&Symbol{Name: "q..inittask"})
	// Eight bytes: the state word and the count, with no function pointers
	// after them. That is the whole of the record for a package that has
	// nothing of its own to run.
	p.AddDef(&Symbol{
		Name:   "p..inittask",
		Type:   SNOPTRDATA,
		Size:   8,
		Align:  8,
		Data:   make([]byte, 8),
		Relocs: []Reloc{{Type: R_INITORDER, Sym: dep}},
	})
	b, err := p.Bytes()
	if err != nil {
		t.Fatalf("an ordering edge was refused: %v", err)
	}
	o := parse(t, b)
	relocs := o.block(BlkReloc)
	if len(relocs) != 23 {
		t.Fatalf("the reloc block is %d bytes, want one 23 byte record", len(relocs))
	}
	if off := binary.LittleEndian.Uint32(relocs[0:]); off != 0 {
		t.Errorf("the edge is at offset %d, want 0", off)
	}
	if size := relocs[4]; size != 0 {
		t.Errorf("the edge is %d bytes wide, want 0: relocsym would then resolve its target", size)
	}
	if typ := binary.LittleEndian.Uint16(relocs[5:]); typ != uint16(R_INITORDER) {
		t.Errorf("the edge has type %d, want %d", typ, R_INITORDER)
	}
	if add := binary.LittleEndian.Uint64(relocs[7:]); add != 0 {
		t.Errorf("the edge has addend %d, want 0", add)
	}
}
