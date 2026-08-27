// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.design/x/nanogo/obj"
)

// Record widths in the file. They are stated here rather than derived
// because the reader checks a block length against a count, and a width
// computed from the fields it reads would agree with itself whatever the
// fields were.
const (
	symSize     = 8 + 2 + 1 + 1 + 1 + 4 + 4 // string ref, ABI, kind, two flag bytes, size, align
	relocSize   = 4 + 1 + 2 + 8 + 8         // offset, width, type, addend, target
	auxSize     = 1 + 8                     // type, symbol
	refFlagSize = 8 + 1 + 1                 // symbol, two flag bytes
	refNameSize = 8 + 8                     // symbol, string ref
	importSize  = 8 + 8                     // string ref, fingerprint
	strRefSize  = 8                         // length, offset
	hash64Size  = 8
	// HashSize is the width of a content hash. It is a truncated SHA-256,
	// and obj.Package writes the same 16 bytes.
	HashSize = 16
)

// headerSize is the fixed part of the object: the magic, the fingerprint,
// the flags, and one offset per block.
const headerSize = len(obj.Magic) + 8 + 4 + 4*obj.NBlk

// A Sym is one record of a symbol definition or reference block.
//
// The fields are the file's, unresolved. A relocation keeps the index pair
// the object holds rather than a name, because the pair means nothing
// outside the object it came from, and because flattening it to a name
// would lose the difference between a package definition, which cmd/link
// never deduplicates, and a non-package one, which it does.
//
// Data aliases the bytes the reader was given. It is not copied, so a
// caller that mutates the object buffer mutates every symbol read from it.
type Sym struct {
	Name   string
	ABI    uint16
	Type   obj.SymKind
	Flag   uint8
	Flag2  uint8
	Size   uint32
	Align  uint32
	Data   []byte
	Relocs []obj.Reloc
	Aux    []obj.Aux
}

// The first flag byte.
func (s *Sym) Dupok() bool         { return s.Flag&obj.SymFlagDupok != 0 }
func (s *Sym) Local() bool         { return s.Flag&obj.SymFlagLocal != 0 }
func (s *Sym) Typelink() bool      { return s.Flag&obj.SymFlagTypelink != 0 }
func (s *Sym) Leaf() bool          { return s.Flag&obj.SymFlagLeaf != 0 }
func (s *Sym) NoSplit() bool       { return s.Flag&obj.SymFlagNoSplit != 0 }
func (s *Sym) ReflectMethod() bool { return s.Flag&obj.SymFlagReflectMethod != 0 }
func (s *Sym) GoType() bool        { return s.Flag&obj.SymFlagGoType != 0 }

// The second flag byte.
func (s *Sym) UsedInIface() bool { return s.Flag2&obj.SymFlagUsedInIface != 0 }
func (s *Sym) Itab() bool        { return s.Flag2&obj.SymFlagItab != 0 }
func (s *Sym) Dict() bool        { return s.Flag2&obj.SymFlagDict != 0 }
func (s *Sym) PkgInit() bool     { return s.Flag2&obj.SymFlagPkgInit != 0 }
func (s *Sym) Linkname() bool    { return s.Flag2&obj.SymFlagLinkname != 0 }
func (s *Sym) LinknameStd() bool { return s.Flag2&obj.SymFlagLinknameStd != 0 }
func (s *Sym) ABIWrapper() bool  { return s.Flag2&obj.SymFlagABIWrapper != 0 }
func (s *Sym) WasmExport() bool  { return s.Flag2&obj.SymFlagWasmExport != 0 }

// An Object is one parsed object file.
//
// The five symbol lists are the five index spaces of the format, in the
// order a SymRef selects them. The four definition lists form one local
// index space, which the index blocks are keyed by and which [Object.Local]
// walks, and the reference list continues that space.
type Object struct {
	// Name is where the object came from. An archive member is named
	// "archive(member)", the way cmd/link names it in a diagnostic.
	Name string

	// Pkg is the import path, from the caller. The object does not carry
	// it: see the package comment.
	Pkg string

	// Header is the "go object ..." line, with its newline. It is empty
	// for an object read without one.
	Header string

	// Main is the mark the main package's object carries in its package
	// data. The linker refuses a first object that does not have it.
	Main bool

	Fingerprint [8]byte
	Flags       uint32

	Imports []obj.ImportedPkg
	Pkglist []string // referenced packages, index 0 is the invalid one
	Files   []string

	Defs         []*Sym
	Hashed64Defs []*Sym
	HashedDefs   []*Sym
	NonPkgDefs   []*Sym
	NonPkgRefs   []*Sym

	RefFlags []obj.RefFlag
	RefNames []obj.RefName

	Hash64 [][hash64Size]byte
	Hash   [][HashSize]byte

	// Trailing is the number of bytes after the last block. gc pads its
	// objects, so these bytes exist and belong to no block.
	Trailing int

	// StringBytes is the size of the string region and StringCovered is
	// how much of it some string reference reached. specs/045 keeps this a
	// measurement rather than a refusal until the corpus says the coverage
	// is total.
	StringBytes     int
	StringCovered   int
	stringIntervals []strSpan
}

type strSpan struct{ off, end uint32 }

// Shared reports whether the object was built with -shared.
func (o *Object) Shared() bool { return o.Flags&obj.ObjFlagShared != 0 }

// FromAssembly reports whether the object came from assembly source.
func (o *Object) FromAssembly() bool { return o.Flags&obj.ObjFlagFromAssembly != 0 }

// Unlinkable reports whether the object was compiled with no package path.
// cmd/link refuses one.
func (o *Object) Unlinkable() bool { return o.Flags&obj.ObjFlagUnlinkable != 0 }

// Std reports whether the object belongs to a standard library package.
func (o *Object) Std() bool { return o.Flags&obj.ObjFlagStd != 0 }

// NDef is the number of defined symbols, which is the length of the index
// blocks minus one. It excludes NonPkgRefs: a reference has no
// relocations, no auxiliary symbols and no data, so the index blocks skip
// it.
func (o *Object) NDef() int {
	return len(o.Defs) + len(o.Hashed64Defs) + len(o.HashedDefs) + len(o.NonPkgDefs)
}

// NSym is the number of records in the local index space, definitions
// first and then references.
func (o *Object) NSym() int { return o.NDef() + len(o.NonPkgRefs) }

// Local returns the symbol at local index i, or nil when i is out of
// range. The order is the one the index blocks use.
func (o *Object) Local(i int) *Sym {
	if i < 0 {
		return nil
	}
	for _, list := range [][]*Sym{o.Defs, o.Hashed64Defs, o.HashedDefs, o.NonPkgDefs, o.NonPkgRefs} {
		if i < len(list) {
			return list[i]
		}
		i -= len(list)
	}
	return nil
}

// LocalIndex returns the local index a reference to this object's own
// index spaces names, and whether the reference names one at all. A
// reference to another package, to a builtin, or the nil symbol does not.
func (o *Object) LocalIndex(r obj.SymRef) (int, bool) {
	switch r.PkgIdx {
	case obj.PkgIdxSelf:
		if int(r.SymIdx) < len(o.Defs) {
			return int(r.SymIdx), true
		}
	case obj.PkgIdxHashed64:
		if int(r.SymIdx) < len(o.Hashed64Defs) {
			return len(o.Defs) + int(r.SymIdx), true
		}
	case obj.PkgIdxHashed:
		if int(r.SymIdx) < len(o.HashedDefs) {
			return len(o.Defs) + len(o.Hashed64Defs) + int(r.SymIdx), true
		}
	case obj.PkgIdxNone:
		n := len(o.Defs) + len(o.Hashed64Defs) + len(o.HashedDefs)
		if int(r.SymIdx) < len(o.NonPkgDefs)+len(o.NonPkgRefs) {
			return n + int(r.SymIdx), true
		}
	}
	return 0, false
}

// ReadFile reads an object or an archive of objects.
//
// It dispatches on the archive magic, so a caller with a path out of an
// import configuration does not need to know which one it holds.
func ReadFile(b []byte, name, pkg string) ([]*Object, error) {
	if bytes.HasPrefix(b, []byte(arMagic)) {
		return ReadArchive(b, name, pkg)
	}
	o, err := ReadObject(b, name, pkg)
	if err != nil {
		return nil, err
	}
	return []*Object{o}, nil
}

// ReadObject reads one object file: the toolchain header line, the
// package data, the separator, and the goobj blocks.
//
// pkg is the import path, which the object does not carry.
func ReadObject(b []byte, name, pkg string) (*Object, error) {
	const hdrPrefix = "go object "
	if !bytes.HasPrefix(b, []byte(hdrPrefix)) {
		return nil, errAt(name, "not a Go object file: it does not start with %q", hdrPrefix)
	}
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 {
		return nil, errAt(name, "the toolchain header line has no newline")
	}
	o := &Object{Name: name, Pkg: pkg, Header: string(b[:nl+1])}

	// The package data runs from the end of the header line to the
	// separator. "\n!\n" can occur inside export data, so the scan counts
	// the "$$" delimiters that bracket it and only accepts a separator
	// outside them. cmd/link's ldobj does the same and names the issue.
	start := nl + 1
	end, ok := findSeparator(b, start)
	if !ok {
		return nil, errAt(name, "no \"\\n!\\n\" separator ends the package data")
	}
	// The lines in front of the separator, up to the first empty one,
	// carry the marks. Only "main" is read.
	for _, line := range strings.Split(string(b[start:end-len("!\n")]), "\n") {
		if line == "" {
			break
		}
		if line == "main" {
			o.Main = true
		}
	}
	if err := o.readBlocks(b[end:], int64(end)); err != nil {
		return nil, err
	}
	return o, nil
}

// findSeparator returns the offset just past the "\n!\n" that ends the
// package data. off is where the data begins, and the byte in front of it
// is the newline that ended the header line.
func findSeparator(b []byte, off int) (int, bool) {
	markers := 0
	for i := off; i+1 < len(b); i++ {
		if i == off || b[i-1] == '\n' {
			if markers%2 == 0 && b[i] == '!' && b[i+1] == '\n' {
				return i + 2, true
			}
			if b[i] == '$' && b[i+1] == '$' {
				markers++
			}
		}
	}
	return 0, false
}

// readBlocks parses the goobj block sequence. base is where the sequence
// starts in the enclosing file, so a refusal names an offset the caller can
// find with a hex dump.
func (o *Object) readBlocks(b []byte, base int64) error {
	fail := func(off int64, format string, args ...any) error {
		return errorf(o.Name, base+off, format, args...)
	}
	if len(b) < headerSize {
		return fail(0, "the object is %d bytes and a header is %d", len(b), headerSize)
	}
	if !bytes.HasPrefix(b, []byte(obj.Magic)) {
		return fail(0, "the goobj magic is %q, this reader writes and reads %q", b[:len(obj.Magic)], obj.Magic)
	}
	copy(o.Fingerprint[:], b[len(obj.Magic):])
	o.Flags = le32(b, len(obj.Magic)+8)

	var offsets [obj.NBlk]uint32
	at := len(obj.Magic) + 8 + 4
	for i := range offsets {
		offsets[i] = le32(b, at+4*i)
	}
	if offsets[obj.BlkAutolib] < uint32(headerSize) {
		return fail(0, "the first block starts at %d, inside the %d byte header", offsets[obj.BlkAutolib], headerSize)
	}
	for i := 1; i < obj.NBlk; i++ {
		if offsets[i] < offsets[i-1] {
			return fail(0, "block %d starts at %d, before block %d at %d", i, offsets[i], i-1, offsets[i-1])
		}
	}
	if int(offsets[obj.BlkEnd]) > len(b) {
		return fail(0, "the last block ends at %d, past the end of %d bytes", offsets[obj.BlkEnd], len(b))
	}
	o.Trailing = len(b) - int(offsets[obj.BlkEnd])

	block := func(i int) []byte { return b[offsets[i]:offsets[i+1]] }
	// The string region is what the header does not cover and the first
	// block has not started.
	strStart, strEnd := uint32(headerSize), offsets[obj.BlkAutolib]
	o.StringBytes = int(strEnd - strStart)

	// Every block of fixed-width records must hold a whole number of them.
	// A remainder is a block the writer and the reader disagree about.
	counts := map[int]int{}
	for _, w := range []struct {
		blk  int
		size int
		what string
	}{
		{obj.BlkAutolib, importSize, "autolib entries"},
		{obj.BlkPkgIdx, strRefSize, "package references"},
		{obj.BlkFile, strRefSize, "file names"},
		{obj.BlkSymdef, symSize, "package definitions"},
		{obj.BlkHashed64def, symSize, "short hashed definitions"},
		{obj.BlkHasheddef, symSize, "hashed definitions"},
		{obj.BlkNonpkgdef, symSize, "non-package definitions"},
		{obj.BlkNonpkgref, symSize, "non-package references"},
		{obj.BlkRefFlags, refFlagSize, "reference flags"},
		{obj.BlkHash64, hash64Size, "short content hashes"},
		{obj.BlkHash, HashSize, "content hashes"},
		{obj.BlkRelocIdx, 4, "relocation index entries"},
		{obj.BlkAuxIdx, 4, "auxiliary index entries"},
		{obj.BlkDataIdx, 4, "data index entries"},
		{obj.BlkReloc, relocSize, "relocations"},
		{obj.BlkAux, auxSize, "auxiliary entries"},
		{obj.BlkRefName, refNameSize, "reference names"},
	} {
		n := len(block(w.blk))
		if n%w.size != 0 {
			return fail(int64(offsets[w.blk]), "the block of %s is %d bytes and a record is %d", w.what, n, w.size)
		}
		counts[w.blk] = n / w.size
	}

	nDef := counts[obj.BlkSymdef] + counts[obj.BlkHashed64def] + counts[obj.BlkHasheddef] + counts[obj.BlkNonpkgdef]
	if counts[obj.BlkHash64] != counts[obj.BlkHashed64def] {
		return fail(int64(offsets[obj.BlkHash64]), "%d short content hashes for %d short hashed definitions",
			counts[obj.BlkHash64], counts[obj.BlkHashed64def])
	}
	if counts[obj.BlkHash] != counts[obj.BlkHasheddef] {
		return fail(int64(offsets[obj.BlkHash]), "%d content hashes for %d hashed definitions",
			counts[obj.BlkHash], counts[obj.BlkHasheddef])
	}
	for _, w := range []struct {
		blk  int
		what string
	}{
		{obj.BlkRelocIdx, "relocation"},
		{obj.BlkAuxIdx, "auxiliary"},
		{obj.BlkDataIdx, "data"},
	} {
		if counts[w.blk] != nDef+1 {
			return fail(int64(offsets[w.blk]), "the %s index holds %d entries for %d defined symbols, it must hold one more",
				w.what, counts[w.blk], nDef)
		}
	}

	relocIdx := uint32s(block(obj.BlkRelocIdx))
	auxIdx := uint32s(block(obj.BlkAuxIdx))
	dataIdx := uint32s(block(obj.BlkDataIdx))
	for _, w := range []struct {
		blk  int
		idx  []uint32
		what string
	}{
		{obj.BlkRelocIdx, relocIdx, "relocation"},
		{obj.BlkAuxIdx, auxIdx, "auxiliary"},
		{obj.BlkDataIdx, dataIdx, "data"},
	} {
		if w.idx[0] != 0 {
			return fail(int64(offsets[w.blk]), "the %s index starts at %d and it must start at 0", w.what, w.idx[0])
		}
		for i := 1; i < len(w.idx); i++ {
			if w.idx[i] < w.idx[i-1] {
				return fail(int64(offsets[w.blk]), "the %s index goes from %d to %d at entry %d",
					w.what, w.idx[i-1], w.idx[i], i)
			}
		}
	}
	// The last index entry is the total, so it states the block's length.
	// A block longer than the total holds records no symbol claims.
	if got, want := uint32(counts[obj.BlkReloc]), relocIdx[nDef]; got != want {
		return fail(int64(offsets[obj.BlkReloc]), "the block holds %d relocations and the index accounts for %d", got, want)
	}
	if got, want := uint32(counts[obj.BlkAux]), auxIdx[nDef]; got != want {
		return fail(int64(offsets[obj.BlkAux]), "the block holds %d auxiliary entries and the index accounts for %d", got, want)
	}
	if got, want := uint32(len(block(obj.BlkData))), dataIdx[nDef]; got != want {
		return fail(int64(offsets[obj.BlkData]), "the data block is %d bytes and the index accounts for %d", got, want)
	}

	str := func(blockStart uint32, at int) (string, error) {
		n := le32(b, int(blockStart)+at)
		off := le32(b, int(blockStart)+at+4)
		if off < strStart || uint64(off)+uint64(n) > uint64(strEnd) {
			return "", fail(int64(blockStart)+int64(at), "a string of %d bytes at %d is outside the string region [%d, %d)",
				n, off, strStart, strEnd)
		}
		if n > 0 {
			o.stringIntervals = append(o.stringIntervals, strSpan{off, off + n})
		}
		return string(b[off : off+n]), nil
	}

	// Autolib.
	o.Imports = make([]obj.ImportedPkg, counts[obj.BlkAutolib])
	for i := range o.Imports {
		s, err := str(offsets[obj.BlkAutolib], i*importSize)
		if err != nil {
			return err
		}
		o.Imports[i].Path = s
		copy(o.Imports[i].Fingerprint[:], b[int(offsets[obj.BlkAutolib])+i*importSize+strRefSize:])
	}

	// Referenced packages and file names.
	for _, w := range []struct {
		blk  int
		dest *[]string
	}{
		{obj.BlkPkgIdx, &o.Pkglist},
		{obj.BlkFile, &o.Files},
	} {
		list := make([]string, counts[w.blk])
		for i := range list {
			s, err := str(offsets[w.blk], i*strRefSize)
			if err != nil {
				return err
			}
			list[i] = s
		}
		*w.dest = list
	}

	// The five symbol blocks.
	for _, w := range []struct {
		blk  int
		dest *[]*Sym
	}{
		{obj.BlkSymdef, &o.Defs},
		{obj.BlkHashed64def, &o.Hashed64Defs},
		{obj.BlkHasheddef, &o.HashedDefs},
		{obj.BlkNonpkgdef, &o.NonPkgDefs},
		{obj.BlkNonpkgref, &o.NonPkgRefs},
	} {
		list := make([]*Sym, counts[w.blk])
		for i := range list {
			at := int(offsets[w.blk]) + i*symSize
			name, err := str(offsets[w.blk], i*symSize)
			if err != nil {
				return err
			}
			list[i] = &Sym{
				Name:  name,
				ABI:   binary.LittleEndian.Uint16(b[at+8:]),
				Type:  obj.SymKind(b[at+10]),
				Flag:  b[at+11],
				Flag2: b[at+12],
				Size:  le32(b, at+13),
				Align: le32(b, at+17),
			}
		}
		*w.dest = list
	}

	// Reference flags and reference names.
	o.RefFlags = make([]obj.RefFlag, counts[obj.BlkRefFlags])
	for i := range o.RefFlags {
		at := int(offsets[obj.BlkRefFlags]) + i*refFlagSize
		o.RefFlags[i] = obj.RefFlag{Sym: symRef(b, at), Flag: b[at+8], Flag2: b[at+9]}
	}
	o.RefNames = make([]obj.RefName, counts[obj.BlkRefName])
	for i := range o.RefNames {
		at := int(offsets[obj.BlkRefName]) + i*refNameSize
		name, err := str(offsets[obj.BlkRefName], i*refNameSize+8)
		if err != nil {
			return err
		}
		o.RefNames[i] = obj.RefName{Sym: symRef(b, at), Name: name}
	}

	// The two hash blocks, read by position.
	o.Hash64 = make([][hash64Size]byte, counts[obj.BlkHash64])
	for i := range o.Hash64 {
		copy(o.Hash64[i][:], b[int(offsets[obj.BlkHash64])+i*hash64Size:])
	}
	o.Hash = make([][HashSize]byte, counts[obj.BlkHash])
	for i := range o.Hash {
		copy(o.Hash[i][:], b[int(offsets[obj.BlkHash])+i*HashSize:])
	}

	// Relocations, auxiliary entries and data, by the index blocks.
	relocs := block(obj.BlkReloc)
	auxes := block(obj.BlkAux)
	data := block(obj.BlkData)
	for i := 0; i < nDef; i++ {
		s := o.Local(i)
		if n := relocIdx[i+1] - relocIdx[i]; n > 0 {
			s.Relocs = make([]obj.Reloc, n)
			for j := range s.Relocs {
				at := int(relocIdx[i]+uint32(j)) * relocSize
				s.Relocs[j] = obj.Reloc{
					Off:  int32(le32(relocs, at)),
					Size: relocs[at+4],
					Type: obj.RelocType(binary.LittleEndian.Uint16(relocs[at+5:])),
					Add:  int64(binary.LittleEndian.Uint64(relocs[at+7:])),
					Sym:  symRef(relocs, at+15),
				}
			}
		}
		if n := auxIdx[i+1] - auxIdx[i]; n > 0 {
			s.Aux = make([]obj.Aux, n)
			for j := range s.Aux {
				at := int(auxIdx[i]+uint32(j)) * auxSize
				t := obj.AuxType(auxes[at])
				if t > obj.AuxSehUnwindInfo {
					return fail(int64(offsets[obj.BlkAux])+int64(at), "%s: auxiliary entry %d has type %d, which the format does not define",
						s.Name, j, t)
				}
				s.Aux[j] = obj.Aux{Type: t, Sym: symRef(auxes, at+1)}
			}
		}
		s.Data = data[dataIdx[i]:dataIdx[i+1]]
	}

	if err := o.checkRefs(base); err != nil {
		return err
	}
	covered, err := o.checkStringRegion(b, strStart, strEnd, base)
	if err != nil {
		return err
	}
	o.StringCovered = covered
	o.stringIntervals = nil
	return nil
}

// checkStringRegion returns how many bytes of the string region a
// reference reached, and refuses a region that holds anything but strings.
//
// The region is not covered completely, and the reason is gc's. Its string
// table adds the name of every symbol the writer traverses, and the
// traversal reaches the inlined callees an AuxFuncInfo inline tree names,
// while the block that writes reference names does not: it walks
// references only, and an inlined callee of another package is reached
// through the auxiliary symbols. A cross-package reference needs no name
// in the object, so nothing points at those bytes and they are dead. A
// hello-world program links about 9,000 such bytes.
//
// Dead strings are still strings, so what the reader can require is that
// the region holds nothing else. A byte that is not graphic text is a
// string reference the reader failed to resolve, and that is the failure
// this accounting exists to catch.
func (o *Object) checkStringRegion(b []byte, start, end uint32, base int64) (int, error) {
	spans := o.stringIntervals
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].off != spans[j].off {
			return spans[i].off < spans[j].off
		}
		return spans[i].end < spans[j].end
	})
	covered := 0
	at := start
	gap := func(lo, hi uint32) error {
		if lo >= hi {
			return nil
		}
		g := b[lo:hi]
		if !utf8.Valid(g) {
			return errorf(o.Name, base+int64(lo), "%d bytes of the string region that no reference reached are not text", hi-lo)
		}
		for i, r := range string(g) {
			if !unicode.IsGraphic(r) {
				return errorf(o.Name, base+int64(lo)+int64(i), "a byte of the string region that no reference reached is %q, and the region holds strings", r)
			}
		}
		return nil
	}
	for _, s := range spans {
		if err := gap(at, s.off); err != nil {
			return 0, err
		}
		if s.off > at {
			at = s.off
		}
		if s.end > at {
			covered += int(s.end - at)
			at = s.end
		}
	}
	if err := gap(at, end); err != nil {
		return 0, err
	}
	return covered, nil
}

// checkRefs verifies that every symbol reference the object holds resolves
// in the index space it names. An unresolvable reference reaches a later
// stage as an index into an array it reads past the end of.
func (o *Object) checkRefs(base int64) error {
	check := func(where string, r obj.SymRef) error {
		if r.IsZero() {
			return nil // the nil symbol, which marker relocations use
		}
		var n int
		switch r.PkgIdx {
		case obj.PkgIdxInvalid:
			return errorf(o.Name, base, "%s: package index 0 with symbol index %d", where, r.SymIdx)
		case obj.PkgIdxBuiltin:
			return nil // the builtin list belongs to the linker, not to the object
		case obj.PkgIdxSelf:
			n = len(o.Defs)
		case obj.PkgIdxHashed64:
			n = len(o.Hashed64Defs)
		case obj.PkgIdxHashed:
			n = len(o.HashedDefs)
		case obj.PkgIdxNone:
			n = len(o.NonPkgDefs) + len(o.NonPkgRefs)
		default:
			if int(r.PkgIdx) >= len(o.Pkglist) {
				return errorf(o.Name, base, "%s: package index %d, the object references %d packages", where, r.PkgIdx, len(o.Pkglist)-1)
			}
			return nil // the index belongs to the other package's array
		}
		if int(r.SymIdx) >= n {
			return errorf(o.Name, base, "%s: symbol index %d, that index space holds %d symbols", where, r.SymIdx, n)
		}
		return nil
	}
	for i := 0; i < o.NDef(); i++ {
		s := o.Local(i)
		for j, r := range s.Relocs {
			if err := check(s.Name+": relocation "+itoa(j), r.Sym); err != nil {
				return err
			}
		}
		for j, a := range s.Aux {
			if err := check(s.Name+": auxiliary entry "+itoa(j), a.Sym); err != nil {
				return err
			}
		}
	}
	for i, rf := range o.RefFlags {
		if err := check("reference flags "+itoa(i), rf.Sym); err != nil {
			return err
		}
	}
	for i, rn := range o.RefNames {
		if err := check("reference name "+itoa(i), rn.Sym); err != nil {
			return err
		}
	}
	return nil
}

func le16(b []byte, off int) uint16 { return binary.LittleEndian.Uint16(b[off:]) }

func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

func symRef(b []byte, off int) obj.SymRef {
	return obj.SymRef{PkgIdx: le32(b, off), SymIdx: le32(b, off+4)}
}

func uint32s(b []byte) []uint32 {
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = le32(b, 4*i)
	}
	return out
}

// itoa keeps the diagnostics out of fmt on a path that runs once per
// reference in every object of the program.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[n:])
}
