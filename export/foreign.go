// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"io"
	"os"
	"sort"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// Copying a declaration of another package out of an archive that holds it.
//
// [Write] writes the linked form, so a declaration of another package that the
// exported surface reaches is written out in full. Everywhere else in this
// package that is done by re-encoding what the checker recorded, which is
// enough for every declaration whose whole meaning the checker holds.
//
// A generic declaration is the one shape whose meaning the checker does not
// hold. It reaches an importer as an object dictionary and a body element, the
// body names the dictionary's slots by number, and the numbering is an
// allocation that happened when the declaring package was compiled
// (specs/013-generics.md). Neither the dictionary nor the body is in the type
// checker's tree, so neither can be re-encoded from it.
//
// gc has the same problem and this is gc's answer.
// cmd/compile/internal/noder's linker.relocObj copies the elements of a
// foreign object out of that object's own file with relocCommon, raw, and
// relocates the cross references rather than re-encoding them. The body of a
// generic declaration comes across with it, because the extension data gc
// copies verbatim is a reference to the body element and relocating that
// reference pulls the body in (specs/015-export-data.md).
//
// nanogo's shape differs from gc's in one place, and it is forced by the
// public root. [Write] lists every element of the object section there, so two
// elements for one declaration are two entries naming one symbol and gc
// resolves a stub to whichever it finds. gc has no such risk, because it
// copies every foreign object and re-encodes none. nanogo copies only what it
// cannot re-encode, so a reference out of a copied element into the object
// section is routed back through [pkgWriter.objIdx] rather than copied, which
// is what keeps one declaration to one element.

// An Archive is one compiled package the build named.
//
// It is a -importcfg packagefile entry. The writer reads one only when it has
// to copy a declaration out of it, and the file is the archive as it is on
// disk, not the export data payload.
type Archive struct {
	// Path is the import path the archive was named under, which is the
	// identity its own package element stands for. A package writes its own
	// path as the empty string, so a reader that had the wrong path here
	// would give the declarations of one package the name of another.
	Path string

	// File is the archive file.
	File string
}

// archiveSet is the archives a writer may copy out of.
//
// A file is opened once and only when a lookup reaches it. Every map here is
// read by key and never ranged over, and the order of the search is the sorted
// order of list, so which archive a declaration is taken from is fixed by the
// import paths and not by a map (specs/053-determinism.md).
type archiveSet struct {
	list  []Archive
	open  map[string]*openArchive // by file, nil when the file is unusable
	found map[string]*foreignDecl // by declKey, nil when no archive holds it
}

// openArchive is one archive's export data, decoded.
type openArchive struct {
	dec pkgbits.PkgDecoder

	// objs maps a declaration to the object element that defines it. A stub
	// is left out: it names a declaration and defines none, so an archive
	// that carries only a stub for something is not an archive to copy it
	// from.
	objs map[string]pkgbits.Index

	// moved records what has already been copied out of this archive, so
	// that one element reached twice is one element in the output. It is
	// per archive because an index means nothing without the file it
	// indexes into.
	moved map[pkgbits.RefTableEntry]pkgbits.Index
}

// foreignDecl is one declaration, in the archive it is copied from.
type foreignDecl struct {
	src *openArchive
	idx pkgbits.Index
}

// declKey names a declaration across archives. The separator is a byte no
// import path and no identifier can hold.
func declKey(path, name string) string { return path + "\x00" + name }

// newArchiveSet returns the set the writer searches, sorted by import path.
func newArchiveSet(archives []Archive) *archiveSet {
	list := make([]Archive, len(archives))
	copy(list, archives)
	sort.Slice(list, func(i, j int) bool {
		if list[i].Path != list[j].Path {
			return list[i].Path < list[j].Path
		}
		return list[i].File < list[j].File
	})
	return &archiveSet{
		list:  list,
		open:  make(map[string]*openArchive),
		found: make(map[string]*foreignDecl),
	}
}

// find returns an archive that defines path.name, or nil.
//
// The declaring package's own archive is preferred, because that is the file
// gc's linker would have taken the elements out of. It is often not there: a
// -importcfg names the direct imports, and a generic declaration reaches this
// writer through whatever package re-exported it. So every other archive is
// searched after it, in sorted order, and each of them carries the same
// elements, copied out of the declaring archive when it was compiled.
func (a *archiveSet) find(version pkgbits.Version, path, name string) *foreignDecl {
	if a == nil {
		return nil
	}
	key := declKey(path, name)
	if d, ok := a.found[key]; ok {
		return d
	}
	a.found[key] = nil
	for _, ar := range a.order(path) {
		src := a.load(version, ar)
		if src == nil {
			continue
		}
		if idx, ok := src.objs[key]; ok {
			d := &foreignDecl{src: src, idx: idx}
			a.found[key] = d
			return d
		}
	}
	return nil
}

// order returns the archives to search, the declaring package's own first.
func (a *archiveSet) order(path string) []Archive {
	out := make([]Archive, 0, len(a.list))
	for _, ar := range a.list {
		if ar.Path == path {
			out = append(out, ar)
		}
	}
	for _, ar := range a.list {
		if ar.Path != path {
			out = append(out, ar)
		}
	}
	return out
}

// load decodes one archive, or returns nil for one that cannot be copied out
// of.
//
// Three things make an archive unusable and none of them is an error about the
// build. A file that will not read or will not decode is one this compilation
// never had to read at all, because the checker reads an archive only when the
// source imports it. A file at another version and a file with sync markers
// hold elements whose bytes mean something else in the file being written, and
// the copy is bytes. The refusal that matters is the one [pkgWriter.copyObj]
// raises when no archive at all holds the declaration, and it names the
// declaration.
func (a *archiveSet) load(version pkgbits.Version, ar Archive) *openArchive {
	if src, ok := a.open[ar.File]; ok {
		return src
	}
	a.open[ar.File] = nil

	raw, err := os.ReadFile(ar.File)
	if err != nil {
		return nil
	}
	payload, err := Payload(raw)
	if err != nil {
		return nil
	}
	src := &openArchive{
		objs:  make(map[string]pkgbits.Index),
		moved: make(map[pkgbits.RefTableEntry]pkgbits.Index),
	}
	if !decodeArchive(ar.Path, payload, &src.dec) {
		return nil
	}
	if src.dec.Version() != version || src.dec.SyncMarkers() {
		return nil
	}
	for i := range src.dec.NumElems(pkgbits.SectionObj) {
		idx := pkgbits.Index(i)
		path, name, tag := src.dec.PeekObj(idx)
		if tag == pkgbits.ObjStub {
			continue
		}
		key := declKey(path, name)
		if _, ok := src.objs[key]; !ok {
			src.objs[key] = idx
		}
	}
	a.open[ar.File] = src
	return src
}

// decodeArchive decodes payload into dec and reports whether it could.
//
// The decoder reports a payload it cannot read by panicking, the way read.go
// records: gc treats a bad stream as a compiler bug. Here it is a file this
// compilation did not have to read, so the panic is turned into a file that is
// not searched.
func decodeArchive(path string, payload []byte, dec *pkgbits.PkgDecoder) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	*dec = pkgbits.NewPkgDecoder(path, string(payload))
	return true
}

// copyable reports whether obj is a declaration of another package that this
// writer cannot re-encode and must copy.
//
// Two shapes, and each is a generic declaration whose body and dictionary were
// numbered together when its own package was compiled:
//
//   - A generic type that declares methods. The methods are written inside the
//     type's element against the type's dictionary, and the slots their bodies
//     name are numbered between the signatures ([pkgWriter.typeDict]).
//   - A generic function. Its body is reached through its own extension data,
//     and it cannot reach a file without one ([pkgWriter.dictOf]).
//
// A generic type with no method is not here. Nothing it holds was numbered by
// a body, so the writer allocates its dictionary as it writes it and the
// re-encoded form is complete. A method of a generic type is not here either,
// because the format gives it no element of its own to copy.
func copyable(pw *pkgWriter, obj types2.Object) bool {
	if obj.Pkg() == nil || obj.Pkg() == pw.curpkg || obj.Pkg() == types2.Unsafe {
		return false
	}
	switch obj := obj.(type) {
	case *types2.Func:
		return len(objTypeParams(obj)) != 0 && obj.Signature().RecvTypeParams().Len() == 0
	case *types2.TypeName:
		if obj.IsAlias() {
			return false
		}
		named, _ := obj.Type().(*types2.Named)
		return named != nil && named.TypeParams().Len() != 0 && named.NumMethods() != 0
	}
	return false
}

// copyObj copies obj's four elements out of an archive and returns the index
// they got, or reports that obj is not one this writer copies.
//
// The four are claimed and recorded before anything is copied, because an
// element of a generic declaration names the declaration itself: the type of a
// method's receiver is the type being copied.
func (pw *pkgWriter) copyObj(obj types2.Object) (pkgbits.Index, bool) {
	if !copyable(pw, obj) {
		return 0, false
	}
	path, name := obj.Pkg().Path(), obj.Name()
	d := pw.archives.find(pw.Version(), path, name)
	if d == nil {
		pw.refuse(objName(obj), "the declaration is generic and belongs to another package, and no archive the build named holds the body and the dictionary its own package numbered for it")
	}

	w := pw.NewEncoderRaw(pkgbits.SectionObj)
	wext := pw.NewEncoderRaw(pkgbits.SectionObjExt)
	wname := pw.NewEncoderRaw(pkgbits.SectionName)
	wdict := pw.NewEncoderRaw(pkgbits.SectionObjDict)

	pw.objsIdx[obj] = w.Idx // claim the index, so a cycle terminates
	if wext.Idx != w.Idx || wname.Idx != w.Idx || wdict.Idx != w.Idx {
		panic(fmt.Errorf("export: the four elements of %v got different indices", objName(obj)))
	}

	c := &copier{pw: pw, src: d.src, what: objName(obj)}
	c.element(w, pkgbits.SectionObj, d.idx)
	c.element(wname, pkgbits.SectionName, d.idx)
	c.element(wdict, pkgbits.SectionObjDict, d.idx)
	c.element(wext, pkgbits.SectionObjExt, d.idx)
	return w.Idx, true
}

// copier copies elements out of one archive into the package being written.
type copier struct {
	pw   *pkgWriter
	src  *openArchive
	what string // the declaration the copy was started for, for a refusal
}

// element copies the element at (k, idx) into w.
//
// The reference table goes through [copier.relocate] and the rest of the
// bitstream is bytes, which is what makes this a copy and not a re-encoding:
// the numbering inside the element, which is what a dictionary and a body
// agree on, is carried over untouched.
func (c *copier) element(w *pkgbits.Encoder, k pkgbits.SectionKind, idx pkgbits.Index) {
	r := c.src.dec.NewDecoderRaw(k, idx)
	w.Relocs = c.relocate(r.Relocs)
	if _, err := io.Copy(&w.Data, &r.Data); err != nil {
		c.pw.refuse(c.what, fmt.Sprintf("the archive holding it could not be copied out of: %v", err))
	}
	w.Flush()
}

// relocate rewrites a reference table so that each entry names the element the
// package being written holds for it.
func (c *copier) relocate(relocs []pkgbits.RefTableEntry) []pkgbits.RefTableEntry {
	out := make([]pkgbits.RefTableEntry, len(relocs))
	for i, e := range relocs {
		out[i] = pkgbits.RefTableEntry{Kind: e.Kind, Idx: c.index(e.Kind, e.Idx)}
	}
	return out
}

// index returns the index the package being written holds for one element of
// the archive.
//
// Three sections are resolved rather than copied. A string is interned, so
// that the strings section keeps one copy of each. A package element states
// its own path as the empty string, so copying one renames the package it
// stands for to the package being written; it is resolved by path, which is
// what gc's relocPkg does for the same reason. An object element is routed
// back through the writer's own walk, so that one declaration is one element
// of the object section, which the public root [Write] writes requires.
//
// The rest are copied. A position base, a type and a body carry no identity a
// reader resolves by name, so two elements for one of them are two elements a
// reader treats as equal.
func (c *copier) index(k pkgbits.SectionKind, idx pkgbits.Index) pkgbits.Index {
	switch k {
	case pkgbits.SectionString:
		return c.pw.StringIdx(c.src.dec.StringIdx(idx))
	case pkgbits.SectionPkg:
		return c.pw.pkgIdx(c.pkg(idx))
	case pkgbits.SectionObj:
		return c.pw.objIdx(c.obj(idx))
	case pkgbits.SectionObjExt, pkgbits.SectionName, pkgbits.SectionObjDict:
		// The three share the object element's index and no reference
		// table names them. One that did would be a file whose objects
		// cannot be renumbered at all.
		c.pw.refuse(c.what, fmt.Sprintf("an element of the archive holding it refers to section %v, which the format addresses by the object's own index", k))
	case pkgbits.SectionMeta:
		c.pw.refuse(c.what, "an element of the archive holding it refers to a root element, which belongs to that file and not to this one")
	}

	ent := pkgbits.RefTableEntry{Kind: k, Idx: idx}
	if i, ok := c.src.moved[ent]; ok {
		return i
	}
	w := c.pw.NewEncoderRaw(k)
	c.src.moved[ent] = w.Idx // claim the index, so a cycle terminates
	c.element(w, k, idx)
	return w.Idx
}

// pkg returns the checker's package for one package element of the archive.
func (c *copier) pkg(idx pkgbits.Index) *types2.Package {
	path := c.src.dec.PeekPkgPath(idx)
	switch path {
	case "builtin":
		// The universe, which the writer states as a nil package.
		return nil
	case "unsafe":
		return types2.Unsafe
	}
	pkg := c.pw.packageByPath(path)
	if pkg == nil {
		c.pw.refuse(c.what, fmt.Sprintf("an element of the archive holding it names the package %q, which this compilation did not read", path))
	}
	return pkg
}

// obj returns the checker's object for one object element of the archive.
//
// The object is looked up rather than copied, so that the element the writer
// already holds for it is the one the reference names. A declaration the
// checker has no object for is refused by name: an index guessed here is a
// declaration an importer reads as another one.
func (c *copier) obj(idx pkgbits.Index) types2.Object {
	path, name, _ := c.src.dec.PeekObj(idx)
	var scope *types2.Scope
	switch path {
	case "builtin":
		scope = types2.Universe
	case "unsafe":
		scope = types2.Unsafe.Scope()
	default:
		pkg := c.pw.packageByPath(path)
		if pkg == nil {
			c.pw.refuse(c.what, fmt.Sprintf("an element of the archive holding it names %s.%s, whose package this compilation did not read", path, name))
		}
		scope = pkg.Scope()
	}
	obj := scope.Lookup(name)
	if obj == nil {
		c.pw.refuse(c.what, fmt.Sprintf("an element of the archive holding it names %s.%s, which the checker recorded no declaration for", path, name))
	}
	return obj
}

// packageByPath returns the checker's package for an import path.
//
// The map is built from the package being written and the packages it reaches,
// which is every package the checker built for this compilation. A copied
// element can name any of them, because the archive it came from was written
// by a compilation of one of them.
func (pw *pkgWriter) packageByPath(path string) *types2.Package {
	if pw.byPath == nil {
		pw.byPath = make(map[string]*types2.Package)
		var walk func(*types2.Package)
		walk = func(p *types2.Package) {
			if p == nil || pw.byPath[p.Path()] != nil {
				return
			}
			pw.byPath[p.Path()] = p
			for _, imp := range p.Imports() {
				walk(imp)
			}
		}
		walk(pw.curpkg)
	}
	return pw.byPath[path]
}
