// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package obj

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// MaxSymSize is the largest symbol the linker accepts. The limit is the
// linker's, not the format's: it reports a clearer error at 2 GB than the
// 32 bit size field would at 4 GB.
const MaxSymSize = 2e9

// A Package is one object file under construction.
//
// The five symbol lists are the five index spaces of the format. A symbol
// keeps the position it was added at, and every block is written by
// walking the lists in order, so two runs over the same input produce the
// same bytes.
//
// A Package is not safe for concurrent use. specs/002-architecture.md
// compiles functions in parallel and merges the results in declaration
// order, so one goroutine owns the Package and fills it after the wait.
type Package struct {
	// Path is the import path of the package, escaped. The linker needs
	// it to expand the names of symbols this package defines.
	Path string

	// Fingerprint identifies the export data this object was compiled
	// from. An importer records the same value in its Autolib entry, and
	// the linker refuses the build if the two disagree.
	Fingerprint [8]byte

	// Flags holds the ObjFlag bits.
	Flags uint32

	// Main marks the object of the main package. The linker reads the
	// mark from a line in front of the goobj blocks, and it refuses to
	// link a first object that does not carry it.
	Main bool

	imports []ImportedPkg

	pkgList  []string          // referenced packages, index 0 is the invalid one
	pkgIndex map[string]uint32 // dedup only, never ranged

	files    []string
	fileSeen map[string]uint32 // dedup only, never ranged

	defs         []*Symbol
	hashed64Defs []*Symbol
	hashedDefs   []*Symbol
	nonPkgDefs   []*Symbol
	nonPkgRefs   []*Symbol

	refFlags []RefFlag
	refNames []RefName

	// hashCache holds the computed content hash of each entry in
	// hashedDefs. A hashed symbol can reference another hashed symbol, so
	// the hash is recursive, and without the cache a chain of references
	// costs exponential time.
	hashCache []*[16]byte
	hashBusy  []bool
}

// NewPackage returns a Package for the named import path. The path must be
// the escaped form, and it must not be empty: the linker cannot expand
// symbol names without it.
func NewPackage(path string) *Package {
	return &Package{
		Path:     path,
		pkgList:  []string{""}, // index 0 is invalid by definition
		pkgIndex: map[string]uint32{},
		fileSeen: map[string]uint32{},
	}
}

// AddImport records an imported package and the fingerprint it was
// compiled against.
func (p *Package) AddImport(path string, fingerprint [8]byte) {
	p.imports = append(p.imports, ImportedPkg{Path: path, Fingerprint: fingerprint})
}

// PkgIndex interns a referenced package path and returns its index. The
// same path always returns the same index, and indices are handed out in
// first-use order.
func (p *Package) PkgIndex(path string) uint32 {
	if i, ok := p.pkgIndex[path]; ok {
		return i
	}
	i := uint32(len(p.pkgList))
	p.pkgList = append(p.pkgList, path)
	p.pkgIndex[path] = i
	return i
}

// AddFile interns a source file name and returns its index. The pc-file
// table stores these indices, so the runtime resolves a program counter to
// a file name with one lookup. Names use forward slashes on every host,
// which keeps the object identical across build machines.
func (p *Package) AddFile(name string) uint32 {
	name = filepath.ToSlash(name)
	if i, ok := p.fileSeen[name]; ok {
		return i
	}
	i := uint32(len(p.files))
	p.files = append(p.files, name)
	p.fileSeen[name] = i
	return i
}

// AddDef adds a symbol this package defines and returns its reference.
func (p *Package) AddDef(s *Symbol) SymRef {
	p.defs = append(p.defs, s)
	return SymRef{PkgIdxSelf, uint32(len(p.defs) - 1)}
}

// AddHashed64Def adds a short content-addressable symbol.
//
// Identity here is content, not name. Two packages that build the same
// eight bytes produce the same entry, and the linker keeps one copy. The
// name still matters, because a relocation names its target before any
// merge happens: see specs/040-object-format.md, "Content-addressable
// symbols". The name is what points at the symbol and the content is what
// the symbol is.
//
// A short symbol carries its content in place of a hash, so the content
// must be at most eight bytes and the symbol must have no relocations.
// [Package.Bytes] rejects anything else, because a truncated identity
// would merge two symbols that differ.
func (p *Package) AddHashed64Def(s *Symbol) SymRef {
	p.hashed64Defs = append(p.hashed64Defs, s)
	return SymRef{PkgIdxHashed64, uint32(len(p.hashed64Defs) - 1)}
}

// AddHashedDef adds a content-addressable symbol. Its identity is a hash
// of its size, its section class, its content, and its relocations. See
// [Package.AddHashed64Def] for what identity means here.
func (p *Package) AddHashedDef(s *Symbol) SymRef {
	p.hashedDefs = append(p.hashedDefs, s)
	return SymRef{PkgIdxHashed, uint32(len(p.hashedDefs) - 1)}
}

// AddNonPkgDef adds a symbol that belongs to no package, such as one named
// by a linkname directive.
func (p *Package) AddNonPkgDef(s *Symbol) SymRef {
	p.nonPkgDefs = append(p.nonPkgDefs, s)
	return SymRef{PkgIdxNone, uint32(len(p.nonPkgDefs) - 1)}
}

// AddNonPkgRef adds a reference to a symbol that belongs to no package.
//
// The index space of NonPkgRefs continues that of NonPkgDefs, so the index a
// reference ends up with depends on how many definitions the object holds when
// it is written, not on how many it held when the reference was added. The
// reference this returns therefore carries [pkgIdxPendingRef] and is given its
// final index by [Package.resolve] at write time. A caller may add definitions
// and references in any order.
func (p *Package) AddNonPkgRef(s *Symbol) SymRef {
	p.nonPkgRefs = append(p.nonPkgRefs, s)
	return SymRef{pkgIdxPendingRef, uint32(len(p.nonPkgRefs) - 1)}
}

// pkgIdxPendingRef marks a reference whose symbol index is not yet final.
//
// It exists in memory only. Every path that reads or writes a SymRef sends it
// through [Package.resolve] first, so no pending reference reaches the object.
// The value is above PkgIdxNone, which is the largest index the format uses,
// so it cannot collide with a real package index.
const pkgIdxPendingRef = 1 << 31

// resolve gives a pending non-package reference its final index.
//
// The NonPkgRefs array continues the NonPkgDefs array, so the index of a
// reference is the number of definitions plus its own position. Every other
// reference passes through unchanged.
func (p *Package) resolve(r SymRef) SymRef {
	if r.PkgIdx != pkgIdxPendingRef {
		return r
	}
	return SymRef{PkgIdxNone, uint32(len(p.nonPkgDefs)) + r.SymIdx}
}

// PkgRef returns a reference to symbol symIdx of the named package. The
// index is the position of the symbol in that package's own SymbolDefs
// array, which the importer learns from the export data.
func (p *Package) PkgRef(path string, symIdx uint32) SymRef {
	return SymRef{p.PkgIndex(path), symIdx}
}

// AddRefFlag records flags for a symbol another package defines. The
// linker reads this before it reads the defining object, which is why the
// flags travel with the reference and not with the definition.
//
// Add an entry only for a symbol that has a flag. gc writes no entry for
// zero flags, and an extra entry moves every later block, so an object
// with one would not be byte-identical to gc's.
func (p *Package) AddRefFlag(ref SymRef, flag, flag2 uint8) {
	p.refFlags = append(p.refFlags, RefFlag{Sym: ref, Flag: flag, Flag2: flag2})
}

// AddRefName records the name of a symbol another package defines. Only
// go tool nm and go tool objdump read this block.
func (p *Package) AddRefName(ref SymRef, name string) {
	p.refNames = append(p.refNames, RefName{Sym: ref, Name: name})
}

// buf is the output buffer. It tracks its own offset because every block
// offset in the header is an absolute byte offset from the start of the
// object.
type buf struct {
	b []byte
}

func (w *buf) off() uint32          { return uint32(len(w.b)) }
func (w *buf) bytes(x []byte)       { w.b = append(w.b, x...) }
func (w *buf) str(s string)         { w.b = append(w.b, s...) }
func (w *buf) uint8(x uint8)        { w.b = append(w.b, x) }
func (w *buf) uint16(x uint16)      { w.b = binary.LittleEndian.AppendUint16(w.b, x) }
func (w *buf) uint32(x uint32)      { w.b = binary.LittleEndian.AppendUint32(w.b, x) }
func (w *buf) uint64(x uint64)      { w.b = binary.LittleEndian.AppendUint64(w.b, x) }
func (w *buf) symRef(r SymRef)      { w.uint32(r.PkgIdx); w.uint32(r.SymIdx) }
func (w *buf) patch32(at, x uint32) { binary.LittleEndian.PutUint32(w.b[at:], x) }

// strtab is the string table. A string is written once and referenced by
// length and offset, so the table deduplicates. Order is first-add order,
// which is deterministic because the writer adds strings by walking the
// symbol lists.
type strtab struct {
	off map[string]uint32 // lookup only, never ranged
}

func (t *strtab) add(w *buf, s string) {
	if _, ok := t.off[s]; ok {
		return
	}
	t.off[s] = w.off()
	w.str(s)
}

// ref writes a string reference: the length, then the offset of the bytes.
func (t *strtab) ref(w *buf, s string) {
	off, ok := t.off[s]
	if !ok {
		// A string reached a block without reaching the table. That is a
		// writer bug, not a caller error, so it is worth a panic: the
		// object would otherwise point at unrelated bytes.
		panic("obj: string not in table: " + s)
	}
	w.uint32(uint32(len(s)))
	w.uint32(off)
}

// lists returns the four definition lists, in the order every index block
// uses. NonPkgRefs is not here: it is a reference list, so it has no
// relocations, no auxiliary symbols, and no data, and the index arrays
// skip it. A reader that expects it slices every later block wrongly.
func (p *Package) lists() [][]*Symbol {
	return [][]*Symbol{p.defs, p.hashed64Defs, p.hashedDefs, p.nonPkgDefs}
}

// Def returns the symbol a reference names, or nil when the reference is to
// something this package does not define.
//
// It reads back what an Add call put in. A caller that has just added a symbol
// holds it already, so this exists for the caller that holds only the
// reference: a relocation names an index pair, and the only way to say what
// that pair means is to ask the object.
//
// A reference to another package, or to a symbol resolved by name, is not a
// definition and returns nil rather than a symbol of no content.
func (p *Package) Def(r SymRef) *Symbol {
	var list []*Symbol
	r = p.resolve(r)
	switch r.PkgIdx {
	case PkgIdxSelf:
		list = p.defs
	case PkgIdxHashed64:
		list = p.hashed64Defs
	case PkgIdxHashed:
		list = p.hashedDefs
	case PkgIdxNone:
		if int(r.SymIdx) < len(p.nonPkgDefs) {
			return p.nonPkgDefs[r.SymIdx]
		}
		return nil
	default:
		return nil
	}
	if int(r.SymIdx) >= len(list) {
		return nil
	}
	return list[r.SymIdx]
}

// Bytes returns the goobj representation of p.
//
// The result is the block sequence specs/040-object-format.md lists,
// starting at the magic. It is not a complete object file: see
// [Package.WriteObject] for the toolchain header that goes in front of it.
func (p *Package) Bytes() ([]byte, error) {
	if err := p.check(); err != nil {
		return nil, err
	}

	// A second write must not reuse hashes computed from the symbols as
	// they were before, because the caller may have changed them.
	p.hashCache, p.hashBusy = nil, nil

	w := &buf{b: make([]byte, 0, 4096)}
	t := &strtab{off: map[string]uint32{}}
	var offsets [NBlk]uint32

	// The header comes first and its block offsets are only known at the
	// end, so reserve the space and patch it once everything is written.
	// This is why the writer builds the object in memory: back-patching a
	// stream needs a seek, and a seek needs a file.
	w.str(Magic)
	w.bytes(p.Fingerprint[:])
	w.uint32(p.Flags)
	offsetsAt := w.off()
	for range offsets {
		w.uint32(0)
	}

	p.strings(w, t)

	offsets[BlkAutolib] = w.off()
	for _, im := range p.imports {
		t.ref(w, im.Path)
		w.bytes(im.Fingerprint[:])
	}

	offsets[BlkPkgIdx] = w.off()
	for _, pkg := range p.pkgList {
		t.ref(w, pkg)
	}

	offsets[BlkFile] = w.off()
	for _, f := range p.files {
		t.ref(w, f)
	}

	blocks := []struct {
		blk  int
		syms []*Symbol
	}{
		{BlkSymdef, p.defs},
		{BlkHashed64def, p.hashed64Defs},
		{BlkHasheddef, p.hashedDefs},
		{BlkNonpkgdef, p.nonPkgDefs},
		{BlkNonpkgref, p.nonPkgRefs},
	}
	for _, b := range blocks {
		offsets[b.blk] = w.off()
		for _, s := range b.syms {
			p.writeSym(w, t, s)
		}
	}

	offsets[BlkRefFlags] = w.off()
	for _, rf := range p.refFlags {
		w.symRef(p.resolve(rf.Sym))
		w.uint8(rf.Flag)
		w.uint8(rf.Flag2)
	}

	// A short content-addressable symbol stores its content where a hash
	// would go. Eight bytes of content are their own identity, so hashing
	// them would only lose information.
	offsets[BlkHash64] = w.off()
	for _, s := range p.hashed64Defs {
		var h [8]byte
		copy(h[:], s.Data)
		w.bytes(h[:])
	}

	offsets[BlkHash] = w.off()
	for i := range p.hashedDefs {
		h, err := p.contentHash(uint32(i))
		if err != nil {
			return nil, err
		}
		w.bytes(h[:])
	}

	// The index blocks let a reader find a symbol's relocations, its
	// auxiliary symbols, and its data without scanning. Each holds one
	// entry per defined symbol plus a final total, so entry i+1 minus
	// entry i is the count for symbol i.
	offsets[BlkRelocIdx] = w.off()
	nreloc := uint32(0)
	for _, list := range p.lists() {
		for _, s := range list {
			w.uint32(nreloc)
			nreloc += uint32(len(s.Relocs))
		}
	}
	w.uint32(nreloc)

	offsets[BlkAuxIdx] = w.off()
	naux := uint32(0)
	for _, list := range p.lists() {
		for _, s := range list {
			w.uint32(naux)
			naux += uint32(len(s.Aux))
		}
	}
	w.uint32(naux)

	offsets[BlkDataIdx] = w.off()
	dataOff := uint64(0)
	for _, list := range p.lists() {
		for _, s := range list {
			w.uint32(uint32(dataOff))
			dataOff += uint64(len(s.Data))
		}
	}
	if dataOff != uint64(uint32(dataOff)) {
		return nil, fmt.Errorf("obj: data block too large: %d bytes", dataOff)
	}
	w.uint32(uint32(dataOff))

	offsets[BlkReloc] = w.off()
	for _, list := range p.lists() {
		for _, s := range list {
			for _, r := range sortRelocs(s.Relocs) {
				w.uint32(uint32(r.Off))
				w.uint8(r.Size)
				w.uint16(uint16(r.Type))
				w.uint64(uint64(r.Add))
				w.symRef(p.resolve(r.Sym))
			}
		}
	}

	offsets[BlkAux] = w.off()
	for _, list := range p.lists() {
		for _, s := range list {
			for _, a := range s.Aux {
				w.uint8(uint8(a.Type))
				w.symRef(p.resolve(a.Sym))
			}
		}
	}

	offsets[BlkData] = w.off()
	for _, list := range p.lists() {
		for _, s := range list {
			w.bytes(s.Data)
		}
	}

	offsets[BlkRefName] = w.off()
	for _, rn := range p.refNames {
		w.symRef(p.resolve(rn.Sym))
		t.ref(w, rn.Name)
	}

	offsets[BlkEnd] = w.off()

	for i, o := range offsets {
		w.patch32(offsetsAt+uint32(4*i), o)
	}
	return w.b, nil
}

// strings fills the string table. Every string a later block references
// must arrive here first, and in a fixed order.
func (p *Package) strings(w *buf, t *strtab) {
	t.add(w, "") // the invalid package at index 0, and every empty name
	for _, im := range p.imports {
		t.add(w, im.Path)
	}
	for _, pkg := range p.pkgList {
		t.add(w, pkg)
	}
	for _, list := range [][]*Symbol{p.defs, p.hashed64Defs, p.hashedDefs, p.nonPkgDefs, p.nonPkgRefs} {
		for _, s := range list {
			t.add(w, s.Name)
		}
	}
	for _, rn := range p.refNames {
		t.add(w, rn.Name)
	}
	for _, f := range p.files {
		t.add(w, f)
	}
}

// writeSym writes one symbol definition or reference record.
func (p *Package) writeSym(w *buf, t *strtab, s *Symbol) {
	t.ref(w, s.Name)
	w.uint16(s.ABI)
	w.uint8(uint8(s.Type))
	w.uint8(s.Flag | p.derivedFlag(s))
	w.uint8(s.Flag2 | p.derivedFlag2(s))
	w.uint32(s.Size)
	w.uint32(s.Align)
}

// derivedFlag returns the first-byte flags that follow from the name and
// the kind. gc derives them the same way, and the linker trusts them: a
// type descriptor that arrives without SymFlagGoType is not entered in the
// type table, and reflection then fails at run time rather than at link
// time.
func (p *Package) derivedFlag(s *Symbol) uint8 {
	// A name of "type:" alone is not a descriptor, and a "type:." prefix
	// names a method of one rather than the descriptor itself.
	if s.Type == SRODATA && len(s.Name) > len("type:") && strings.HasPrefix(s.Name, "type:") && s.Name[len("type:")] != '.' {
		return SymFlagGoType
	}
	return 0
}

// derivedFlag2 returns the second-byte flags that follow from the name and
// the kind.
func (p *Package) derivedFlag2(s *Symbol) uint8 {
	var f uint8
	if s.Type == SRODATA && strings.HasPrefix(s.Name, "go:itab.") {
		f |= SymFlagItab
	}
	// The runtime reaches main.main through a linkname, so the symbol must
	// carry the flag even though the source never wrote the directive.
	if s.Name == "main.main" {
		f |= SymFlagLinkname
	}
	if dict := p.Path + "." + ".dict"; strings.HasPrefix(s.Name, dict) {
		f |= SymFlagDict
	}
	return f
}

// sortRelocs returns the relocations of a symbol in write order.
//
// The order is by offset because some object formats, PE among them,
// require relocations in address order. The remaining fields break ties so
// that two relocations at the same offset cannot swap between runs, which
// specs/053-determinism.md requires.
func sortRelocs(rs []Reloc) []Reloc {
	out := make([]Reloc, len(rs))
	copy(out, rs)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Off != b.Off:
			return a.Off < b.Off
		case a.Type != b.Type:
			return a.Type < b.Type
		case a.Sym.PkgIdx != b.Sym.PkgIdx:
			return a.Sym.PkgIdx < b.Sym.PkgIdx
		case a.Sym.SymIdx != b.Sym.SymIdx:
			return a.Sym.SymIdx < b.Sym.SymIdx
		case a.Add != b.Add:
			return a.Add < b.Add
		default:
			return a.Size < b.Size
		}
	})
	return out
}

// check validates the package before any byte is written. Every failure
// here would otherwise become a linker error a long way from its cause, or
// worse, a silent merge of two symbols that differ.
func (p *Package) check() error {
	if p.Path == "" {
		return errors.New("obj: empty package path")
	}
	for _, list := range [][]*Symbol{p.defs, p.hashedDefs, p.nonPkgDefs} {
		for _, s := range list {
			if err := checkSym(s); err != nil {
				return err
			}
		}
	}
	for _, s := range p.hashed64Defs {
		if err := checkSym(s); err != nil {
			return err
		}
		// A short symbol's identity is its content, stored whole. More
		// than eight bytes would not fit, and the linker would merge two
		// symbols that share a prefix.
		if len(s.Data) > 8 {
			return fmt.Errorf("obj: %s: short content-addressable symbol has %d bytes of data, at most 8 fit", s.Name, len(s.Data))
		}
		if len(s.Relocs) != 0 {
			return fmt.Errorf("obj: %s: short content-addressable symbol has %d relocations, its content is not its identity", s.Name, len(s.Relocs))
		}
		if contentHashSection(s) != 0 {
			return fmt.Errorf("obj: %s: short content-addressable symbol is not in the default section", s.Name)
		}
	}
	for _, list := range [][]*Symbol{p.hashed64Defs, p.hashedDefs} {
		for _, s := range list {
			// The linker places a content-addressable symbol itself, so it
			// needs the alignment. gc reports the same case.
			if s.Size != 0 && s.Align == 0 && !s.Type.IsText() {
				return fmt.Errorf("obj: %s: content-addressable symbol of size %d has no alignment", s.Name, s.Size)
			}
		}
	}
	for _, s := range p.nonPkgRefs {
		if len(s.Data) != 0 || len(s.Relocs) != 0 || len(s.Aux) != 0 {
			return fmt.Errorf("obj: %s: non-package reference carries content, but the index blocks do not cover references", s.Name)
		}
	}
	for _, list := range p.lists() {
		for _, s := range list {
			for i, r := range s.Relocs {
				if err := p.checkRef(r.Sym); err != nil {
					return fmt.Errorf("obj: %s: relocation %d: %w", s.Name, i, err)
				}
			}
			for i, a := range s.Aux {
				if err := p.checkRef(a.Sym); err != nil {
					return fmt.Errorf("obj: %s: auxiliary symbol %d: %w", s.Name, i, err)
				}
			}
		}
	}
	for i, rf := range p.refFlags {
		if err := p.checkRef(rf.Sym); err != nil {
			return fmt.Errorf("obj: reference flags %d: %w", i, err)
		}
	}
	for i, rn := range p.refNames {
		if err := p.checkRef(rn.Sym); err != nil {
			return fmt.Errorf("obj: reference name %d: %w", i, err)
		}
	}
	return nil
}

// checkRef reports whether a reference names a symbol that exists. A
// reference to nothing reaches the linker as an index into an array it
// then reads past the end of, so the error belongs here.
func (p *Package) checkRef(r SymRef) error {
	if r.IsZero() {
		return nil // the nil symbol, which marker relocations use
	}
	r = p.resolve(r)
	var n int
	switch r.PkgIdx {
	case PkgIdxInvalid:
		return fmt.Errorf("package index 0 with symbol index %d", r.SymIdx)
	case PkgIdxBuiltin:
		return nil // the builtin list belongs to the linker, not to this object
	case PkgIdxSelf:
		n = len(p.defs)
	case PkgIdxHashed64:
		n = len(p.hashed64Defs)
	case PkgIdxHashed:
		n = len(p.hashedDefs)
	case PkgIdxNone:
		n = len(p.nonPkgDefs) + len(p.nonPkgRefs)
	default:
		if int(r.PkgIdx) >= len(p.pkgList) {
			return fmt.Errorf("package index %d, only %d packages are referenced", r.PkgIdx, len(p.pkgList)-1)
		}
		return nil // the index belongs to the other package's array
	}
	if int(r.SymIdx) >= n {
		return fmt.Errorf("symbol index %d, only %d symbols are in that space", r.SymIdx, n)
	}
	return nil
}

func checkSym(s *Symbol) error {
	if s.Name == "" && !s.Anonymous {
		return errors.New("obj: symbol with empty name; use Anonymous for an auxiliary symbol")
	}
	if float64(s.Size) > MaxSymSize {
		return fmt.Errorf("obj: %s: symbol too large: %d bytes, the limit is %d", s.Name, s.Size, int64(MaxSymSize))
	}
	// A zero-filled kind states a size and carries no data. Every other
	// kind must agree with its data, or the linker copies the wrong number
	// of bytes.
	if len(s.Data) != 0 && uint64(s.Size) != uint64(len(s.Data)) {
		return fmt.Errorf("obj: %s: size %d does not match %d bytes of data", s.Name, s.Size, len(s.Data))
	}
	for i, r := range s.Relocs {
		if r.Off < 0 || uint64(r.Off)+uint64(r.Size) > uint64(s.Size) {
			return fmt.Errorf("obj: %s: relocation %d at offset %d size %d is outside the symbol, which is %d bytes", s.Name, i, r.Off, r.Size, s.Size)
		}
	}
	return nil
}

// contentHashSection returns a one-byte mnemonic for the section a symbol
// must live in. Content addressing must not move a symbol between
// sections, so the mnemonic goes into the hash and keeps symbols of
// different classes apart even when their bytes agree.
func contentHashSection(s *Symbol) byte {
	switch {
	case s.Type == STEXT:
		return 't'
	case s.Type == STEXTFIPS:
		return 'f'
	case s.Pcdata:
		return 'P'
	}
	name := s.Name
	switch {
	case strings.HasPrefix(name, "gcargs."),
		strings.HasPrefix(name, "gclocals."),
		strings.HasPrefix(name, "gclocals·"),
		strings.HasSuffix(name, ".opendefer"),
		strings.HasSuffix(name, ".arginfo0"),
		strings.HasSuffix(name, ".arginfo1"),
		strings.HasSuffix(name, ".argliveinfo"),
		strings.HasSuffix(name, ".wrapinfo"),
		strings.HasSuffix(name, ".args_stackmap"),
		strings.HasSuffix(name, ".stkobj"):
		return 'F' // go:func.* or go:funcrel.*
	case strings.HasPrefix(name, "type:"):
		return 'T'
	}
	return 0
}

// hashPrefix makes the content hash differ from a plain SHA-256 of the
// same bytes. gc does the same, so the two agree.
var hashPrefix = []byte{1}

// contentHash computes the identity of hashedDefs[i].
//
// The hash covers the size, the section class, the content, and every
// relocation. A relocation contributes the identity of its target, chosen
// so that the result is the same in every package that builds the same
// symbol: a content-addressable target contributes its own hash, a package
// symbol contributes its package path and index, and a non-package symbol
// contributes its expanded name.
func (p *Package) contentHash(i uint32) ([16]byte, error) {
	if p.hashCache == nil {
		p.hashCache = make([]*[16]byte, len(p.hashedDefs))
		p.hashBusy = make([]bool, len(p.hashedDefs))
	}
	if h := p.hashCache[i]; h != nil {
		return *h, nil
	}
	if p.hashBusy[i] {
		// gc assumes hashed symbols form no cycle and would recurse until
		// the stack ran out. Report it instead.
		return [16]byte{}, fmt.Errorf("obj: %s: content-addressable symbols form a reference cycle", p.hashedDefs[i].Name)
	}
	p.hashBusy[i] = true
	defer func() { p.hashBusy[i] = false }()

	s := p.hashedDefs[i]
	h := sha256.New()
	h.Write(hashPrefix)

	var tmp [14]byte
	// The size is in the hash so that a short symbol and a longer one that
	// starts with the same bytes stay distinct. [2]int{1,2} and
	// [10]int{1,2,0,...} would otherwise merge, and the larger allocation
	// would survive whenever the smaller one was live.
	binary.LittleEndian.PutUint64(tmp[:8], uint64(s.Size))
	tmp[8] = contentHashSection(s)
	h.Write(tmp[:9])

	if s.Type.IsText() {
		// Two unrelated functions that happen to encode to the same bytes
		// are still two functions. The name keeps them apart.
		io.WriteString(h, s.Name)
	}

	// gc trims trailing zeros from data sometimes and not always. Trimming
	// here every time makes the hash agree with gc's in both cases.
	h.Write(bytes.TrimRight(s.Data, "\x00"))

	// The relocations enter the hash in the order the caller gave them,
	// not in the order the Reloc block writes them. gc hashes before it
	// sorts, so hashing sorted relocations would give a different identity
	// for the same symbol and the linker would keep both copies.
	self := SymRef{PkgIdxHashed, i}
	for _, r := range s.Relocs {
		binary.LittleEndian.PutUint32(tmp[:4], uint32(r.Off))
		tmp[4] = r.Size
		tmp[5] = uint8(r.Type)
		binary.LittleEndian.PutUint64(tmp[6:14], uint64(r.Add))
		h.Write(tmp[:])
		if err := p.hashTarget(h, r.Sym, self); err != nil {
			return [16]byte{}, err
		}
	}

	var out [16]byte
	copy(out[:], h.Sum(nil))
	p.hashCache[i] = &out
	return out, nil
}

// hashTarget writes the globally stable identity of one relocation target.
func (p *Package) hashTarget(h io.Writer, rs, self SymRef) error {
	rs = p.resolve(rs)
	switch {
	case rs.IsZero():
		io.WriteString(h, "nil symbol") // a marker relocation, with no target
		return nil
	case rs == self:
		io.WriteString(h, "self symbol") // a self reference is the same in every copy
		return nil
	}
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], rs.SymIdx)
	switch rs.PkgIdx {
	case PkgIdxHashed64:
		if int(rs.SymIdx) >= len(p.hashed64Defs) {
			return fmt.Errorf("obj: relocation targets short hashed symbol %d, only %d exist", rs.SymIdx, len(p.hashed64Defs))
		}
		var c [8]byte
		copy(c[:], p.hashed64Defs[rs.SymIdx].Data)
		h.Write([]byte{0})
		h.Write(c[:])
	case PkgIdxHashed:
		if int(rs.SymIdx) >= len(p.hashedDefs) {
			return fmt.Errorf("obj: relocation targets hashed symbol %d, only %d exist", rs.SymIdx, len(p.hashedDefs))
		}
		t, err := p.contentHash(rs.SymIdx)
		if err != nil {
			return err
		}
		h.Write([]byte{1})
		h.Write(t[:])
	case PkgIdxNone:
		name, err := p.nonPkgName(rs.SymIdx)
		if err != nil {
			return err
		}
		h.Write([]byte{2})
		io.WriteString(h, name)
	case PkgIdxBuiltin:
		h.Write([]byte{3})
		h.Write(tmp[:])
	case PkgIdxSelf:
		io.WriteString(h, p.Path)
		h.Write(tmp[:])
	default:
		if int(rs.PkgIdx) >= len(p.pkgList) {
			return fmt.Errorf("obj: relocation targets package %d, only %d are referenced", rs.PkgIdx, len(p.pkgList)-1)
		}
		io.WriteString(h, p.pkgList[rs.PkgIdx])
		h.Write(tmp[:])
	}
	return nil
}

// nonPkgName returns the name at index i of the joined NonPkgDefs and
// NonPkgRefs space.
func (p *Package) nonPkgName(i uint32) (string, error) {
	if int(i) < len(p.nonPkgDefs) {
		return p.nonPkgDefs[i].Name, nil
	}
	j := int(i) - len(p.nonPkgDefs)
	if j < len(p.nonPkgRefs) {
		return p.nonPkgRefs[j].Name, nil
	}
	return "", fmt.Errorf("obj: relocation targets non-package symbol %d, only %d exist", i, len(p.nonPkgDefs)+len(p.nonPkgRefs))
}

// WriteObject writes a complete object file: the toolchain header line,
// the separator, and the goobj blocks.
//
// The header line is what the linker compares first, before it looks at
// the magic. It carries the operating system, the architecture, the
// release, and the enabled experiments, and the linker rejects an object
// whose line differs from its own by one character. [VerifyToolchain]
// returns the line the installed toolchain writes.
func (p *Package) WriteObject(w io.Writer, header string) error {
	if !strings.HasPrefix(header, "go object ") || !strings.HasSuffix(header, "\n") {
		return fmt.Errorf("obj: bad toolchain header %q", header)
	}
	b, err := p.Bytes()
	if err != nil {
		return err
	}
	// The lines between the header and the separator are the package
	// data. The linker reads only one thing from them, the main mark, and
	// stops at the first empty line.
	prefix := header
	if p.Main {
		prefix += "main\n\n"
	}
	// The separator ends the variable part of the file. The linker scans
	// for "\n!\n" and starts the goobj blocks after it.
	if _, err := io.WriteString(w, prefix+"!\n"); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// A Toolchain is what the installed gc toolchain writes into an object.
type Toolchain struct {
	// Header is the "go object ..." line, with its newline.
	Header string
	// Magic is the goobj format tag the toolchain writes.
	Magic string
}

var (
	toolchainOnce sync.Once
	toolchain     *Toolchain
	toolchainErr  error
)

// VerifyToolchain reports what the installed toolchain writes and fails if
// its object format is not the one this package produces.
//
// specs/040-object-format.md pins the version: there is no compatibility
// range and no negotiation. The check is empirical rather than declared,
// because the header line carries the enabled experiment list, and no
// go env variable reports it. So the probe assembles an empty file and
// reads the two facts out of the result.
//
// The result is computed once and reused.
func VerifyToolchain() (*Toolchain, error) {
	toolchainOnce.Do(func() {
		toolchain, toolchainErr = probeToolchain()
	})
	return toolchain, toolchainErr
}

func probeToolchain() (*Toolchain, error) {
	goCmd, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("obj: no go command to check the object format against: %w", err)
	}
	dir, err := os.MkdirTemp("", "nanogo-objprobe")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "probe.s")
	out := filepath.Join(dir, "probe.o")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.Command(goCmd, "tool", "asm", "-p", "nanogo/probe", "-o", out, src)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("obj: cannot probe the object format: %v: %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	return parseToolchainObject(b)
}

// parseToolchainObject reads the header line and the magic out of an
// object the installed toolchain produced.
func parseToolchainObject(b []byte) (*Toolchain, error) {
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 || !bytes.HasPrefix(b, []byte("go object ")) {
		return nil, errors.New("obj: probe output is not a Go object file")
	}
	header := string(b[:nl+1])
	rest := b[nl+1:]
	if !bytes.HasPrefix(rest, []byte("!\n")) {
		return nil, errors.New("obj: probe output has no block separator")
	}
	rest = rest[len("!\n"):]
	if len(rest) < len(Magic) {
		return nil, errors.New("obj: probe output has no goobj magic")
	}
	magic := string(rest[:len(Magic)])
	if magic != Magic {
		return nil, fmt.Errorf("obj: object format mismatch: nanogo writes %q, the installed toolchain (%s) writes %q",
			Magic, strings.TrimSpace(header), magic)
	}
	return &Toolchain{Header: header, Magic: magic}, nil
}

// A PCEntry says that Value is in effect from PC until the next entry.
type PCEntry struct {
	PC    int64
	Value int32
}

// EncodePCData encodes one pc-value table.
//
// A pc-value table maps a program counter to a small integer. The runtime
// decodes it linearly, from the start, so the encoding is a delta of a
// delta and never an index. Every PCDATA stream and the line and file
// tables use it: see specs/040-object-format.md, "PC-value tables".
//
// The stream is a sequence of pairs. Each pair is a value delta then a pc
// delta, both variable length base-128:
//
//   - the value delta is signed and zig-zag coded, because values move up
//     and down and small negative deltas must stay one byte;
//   - the pc delta is unsigned and counted in instructions, not bytes: it
//     is divided by minLC, which is 4 on arm64, so a delta that would need
//     two bytes needs one.
//
// The first value delta is measured from -1, which is the value the
// runtime assumes before it reads anything, and it carries no pc delta
// because the first entry always starts at the function entry. The stream
// ends with the pc delta that reaches funcSize and then a zero byte. Zero
// is not a valid value delta, so the zero terminates.
//
// An empty table encodes to nothing. The linker treats a zero-length
// pc-value symbol as "no information", which is what a function with no
// changes needs.
func EncodePCData(entries []PCEntry, funcSize int64, minLC int) ([]byte, error) {
	if minLC <= 0 {
		return nil, fmt.Errorf("obj: bad minimum instruction length %d", minLC)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	if entries[0].PC != 0 {
		return nil, fmt.Errorf("obj: pc-value table starts at %d, it must start at 0", entries[0].PC)
	}
	if funcSize < 0 || funcSize%int64(minLC) != 0 {
		return nil, fmt.Errorf("obj: function size %d is not a multiple of %d", funcSize, minLC)
	}

	var out []byte
	var scratch [binary.MaxVarintLen64]byte
	pc := int64(0)
	val := int32(-1)
	started := false
	for i, e := range entries {
		if e.PC%int64(minLC) != 0 {
			return nil, fmt.Errorf("obj: pc-value entry %d at %d is not a multiple of %d", i, e.PC, minLC)
		}
		if i > 0 && e.PC <= entries[i-1].PC {
			return nil, fmt.Errorf("obj: pc-value entry %d at %d does not follow %d", i, e.PC, entries[i-1].PC)
		}
		if e.PC > funcSize {
			return nil, fmt.Errorf("obj: pc-value entry %d at %d is past the end of a %d byte function", i, e.PC, funcSize)
		}
		if e.Value == val {
			continue // no change, so nothing to record
		}
		if started {
			n := binary.PutUvarint(scratch[:], uint64((e.PC-pc)/int64(minLC)))
			out = append(out, scratch[:n]...)
			pc = e.PC
		}
		n := binary.PutVarint(scratch[:], int64(e.Value)-int64(val))
		out = append(out, scratch[:n]...)
		val = e.Value
		started = true
	}
	if !started {
		return nil, nil
	}
	n := binary.PutUvarint(scratch[:], uint64((funcSize-pc)/int64(minLC)))
	out = append(out, scratch[:n]...)
	return append(out, 0), nil
}
