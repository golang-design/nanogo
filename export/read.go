// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// The gc archive, from the outside in.
//
// A compiled package is a Unix archive whose first member is __.PKGDEF. That
// member holds the object header, then any number of further header lines,
// then a section that starts with "$$B\n" and ends with "\n$$\n". The first
// byte of the section names the format, and 'u' is the unified format this
// package reads. driver/archive.go writes the other half of the same
// container.
const (
	archiveMagic = "!<arch>\n"

	// headerSize is the fixed width of one archive member header. Every
	// field is read by offset, so the width is exact and not a guess.
	headerSize = 60

	// definitionMember is the name gc gives the export data member.
	definitionMember = "__.PKGDEF"

	// sectionEnd terminates the export data section.
	sectionEnd = "\n$$\n"
)

// Reader reads the export data of the packages one compilation imports.
//
// One Reader serves a whole compilation, because a package reached through two
// different archives must be the same [types2.Package]. gc writes the
// declarations an archive depends on into that archive, so reading "net/http"
// materialises "io" as a side effect. A second Reader would produce a second
// "io" and the checker would report the two io.Writer types as unrelated.
//
// The same reasoning gives the Reader one [types2.Context]: it is what makes
// two instantiations of one generic type with the same arguments identical.
type Reader struct {
	ctxt     *types2.Context
	packages map[string]*types2.Package

	// imports records what was read, in the order it was read. The object
	// nanogo writes carries one Autolib entry per direct import
	// (specs/040-object-format.md), and the linker will not load an archive
	// that no entry names.
	imports []Import

	// bodies caches the function bodies of each archive [Reader.Bodies]
	// decoded, and bodyErrs the reason one could not be decoded. Both are
	// lookup tables and neither is ranged over (specs/053-determinism.md).
	bodies   map[string][]*FuncBody
	bodyErrs map[string]error
}

// Import is one package this compilation read export data for.
type Import struct {
	// Path is the import path the export data was read under, after any
	// importmap rename.
	Path string

	// File is the archive it came from.
	File string

	// Fingerprint identifies the export data. The linker compares it with
	// the one in the imported package's own object and refuses a build
	// whose two copies disagree.
	Fingerprint [8]byte
}

// NewReader returns a Reader with no package read yet.
func NewReader() *Reader {
	return &Reader{
		ctxt:     types2.NewContext(),
		packages: make(map[string]*types2.Package),
	}
}

// Imports returns what was read, in the order it was read.
//
// A package is read once, so the order is the order the type checker first
// asked for each import, which is source order. The slice is what the object
// writer walks, and walking it rather than a map is what keeps the object's
// Autolib block identical between runs (specs/053-determinism.md).
func (r *Reader) Imports() []Import { return r.imports }

// Read returns the package at the import path path, reading it from the
// archive file.
//
// A package already read is returned only when it is complete. A package that
// another archive mentioned exists but holds nothing, because [ReadPackage]
// marks only the package the archive is for, so an incomplete entry is a
// package whose own archive has still to be read.
func (r *Reader) Read(path, file string) (pkg *types2.Package, err error) {
	// unsafe has no archive. The checker asks the importer for it like any
	// other import, and -importcfg carries no entry, so the answer is here.
	if path == "unsafe" {
		return types2.Unsafe, nil
	}
	if p := r.packages[path]; p != nil && p.Complete() {
		return p, nil
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	def, err := packageDefinition(data)
	if err != nil {
		return nil, err
	}
	payload, err := unified(def)
	if err != nil {
		return nil, err
	}

	// The decoder reports a stream it cannot read by panicking, because gc
	// treats a bad stream as a compiler bug and ends the process. nanogo is
	// handed the file by the build, so the same event is an error about that
	// file. The recovered value is carried through verbatim: a panic from a
	// bug in this package rather than from the data must stay identifiable.
	defer func() {
		if v := recover(); v != nil {
			pkg, err = nil, fmt.Errorf("%v", v)
		}
	}()
	dec := pkgbits.NewPkgDecoder(path, string(payload))
	pkg = ReadPackage(r.ctxt, r.packages, dec)
	if pkg == nil {
		return nil, fmt.Errorf("the export data declares no package")
	}
	r.imports = append(r.imports, Import{Path: path, File: file, Fingerprint: dec.Fingerprint()})
	return pkg, nil
}

// Payload returns the unified export data one gc archive carries.
//
// It is [Definition] from the other side, and it is what a reader that wants
// the bytes rather than the package needs: [pkgbits.NewPkgDecoder] takes
// exactly this, and [ReadBodies] takes the decoder.
func Payload(archive []byte) ([]byte, error) {
	def, err := packageDefinition(archive)
	if err != nil {
		return nil, err
	}
	return unified(def)
}

// packageDefinition returns the __.PKGDEF member of a gc archive.
//
// The members are walked rather than assumed, although gc writes __.PKGDEF
// first. An archive with no such member is the one nanogo itself writes
// (specs/015-export-data.md has no writer), so the message that names the
// missing member is the message a user is most likely to see.
func packageDefinition(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte(archiveMagic)) {
		return nil, fmt.Errorf("not an archive: it does not start with %q", archiveMagic)
	}
	off := len(archiveMagic)
	for off+headerSize <= len(data) {
		header := data[off : off+headerSize]
		name := strings.TrimRight(string(header[0:16]), " ")
		size, err := strconv.Atoi(strings.TrimRight(string(header[48:58]), " "))
		if err != nil || size < 0 {
			return nil, fmt.Errorf("archive member %q has an unreadable size %q", name, header[48:58])
		}
		body := off + headerSize
		if body+size > len(data) {
			return nil, fmt.Errorf("archive member %q claims %d bytes and only %d are left in the file",
				name, size, len(data)-body)
		}
		if name == definitionMember {
			return data[body : body+size], nil
		}
		// A member is padded to an even length and the padding is not
		// counted in the header, so a reader that trusts only the header
		// lands one byte inside the next header.
		off = body + size + size%2
	}
	return nil, fmt.Errorf("the archive holds no %s member, so it carries no export data", definitionMember)
}

// unified strips the package definition's headers and returns the unified
// export data payload.
func unified(def []byte) ([]byte, error) {
	line, rest, ok := cutLine(def)
	if !ok || !strings.HasPrefix(line, "go object ") {
		return nil, fmt.Errorf("%s does not start with an object header", definitionMember)
	}
	// The object header is followed by further header lines, of which the
	// build ID is one. They end at the first line that starts the export
	// data section.
	for !bytes.HasPrefix(rest, []byte("$$")) {
		if _, rest, ok = cutLine(rest); !ok {
			return nil, fmt.Errorf("%s has no export data section", definitionMember)
		}
	}
	line, rest, _ = cutLine(rest)
	if line != "$$B" {
		// "$$" alone is the textual format gc stopped writing long ago.
		return nil, fmt.Errorf("export data section %q is not the binary format", line)
	}
	if len(rest) == 0 {
		return nil, fmt.Errorf("the export data section is empty")
	}
	// The first byte names the format. 'i' was the indexed format and 'c',
	// 'd' and 'v' the binary formats before it.
	if format := rest[0]; format != 'u' {
		return nil, fmt.Errorf("export data format %q is not the unified format nanogo reads", string(format))
	}
	rest = rest[1:]
	if !bytes.HasSuffix(rest, []byte(sectionEnd)) {
		return nil, fmt.Errorf("the export data section does not end with %q", sectionEnd)
	}
	return rest[:len(rest)-len(sectionEnd)], nil
}

// cutLine splits one newline-terminated line off the front of b. The newline
// is dropped from the line and consumed from the rest.
func cutLine(b []byte) (line string, rest []byte, ok bool) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return "", b, false
	}
	return string(b[:i]), b[i+1:], true
}

// Bodies returns every function body the archive of path carries, decoded
// against the packages this Reader already read.
//
// It is the door the stenciler of specs/013-generics.md reaches a generic
// another package declared through. The archive is the one this compilation
// imported the package from, and the decode shares this Reader's package
// table and its [types2.Context], which is what makes the answer usable: a
// declaration the body names resolves to the object the type checker holds
// for it, and an instantiation the body names is the checker's own type. A
// second reader over the same archive would produce a parallel object graph,
// and a call in the body would then name a function the compilation has no
// object for.
//
// The archive is remembered from [Reader.Read], so a package this compilation
// never imported has no file here and is refused by name rather than
// searched for.
//
// The list is read once per package and kept, because one compilation
// instantiates several declarations of one package and each decode is the
// whole archive.
func (r *Reader) Bodies(path string) ([]*FuncBody, error) {
	if r.bodies == nil {
		r.bodies = make(map[string][]*FuncBody)
		r.bodyErrs = make(map[string]error)
	}
	if have, ok := r.bodies[path]; ok {
		return have, r.bodyErrs[path]
	}
	bodies, err := r.readBodies(path)
	r.bodies[path], r.bodyErrs[path] = bodies, err
	return bodies, err
}

// Body returns the body of one declaration, or nil when no archive this
// compilation read carries one for it.
//
// name is gc's linker symbol name, which is what the export data names a body
// by: "Contains" for a function and "(*Pointer).Store" for a method.
//
// # Why more than the declaring package's archive is searched
//
// A generic declaration is copied whole into the archive of every package
// whose exported surface reaches it, which is what [Write] does through
// foreign.go and what gc's linker.relocObj does. So the body of
// sync/atomic.(*Pointer).Load is in os's archive as well as in sync/atomic's,
// and a package that holds an os.File without importing sync/atomic still
// owes the method set of sync/atomic.Pointer[os.dirInfo]
// (specs/017-export-data-reading.md). Its -importcfg names no archive for the
// declaring package at all, so the declaring package's own archive is the
// first place to look and cannot be the only one.
//
// The search order is the sorted import path order, which is the order
// foreign.go's writer searches in and for the same reason: which archive a
// declaration is taken from must be fixed by the import paths and not by the
// order a map produced them (specs/053-determinism.md).
func (r *Reader) Body(path, name string) (*FuncBody, error) {
	var err error
	for _, p := range r.searchOrder(path) {
		b, e := r.bodyIn(p, path, name)
		if b != nil {
			return b, nil
		}
		if err == nil {
			err = e
		}
	}
	// The error is reported only when nothing was found. An archive this
	// compilation cannot decode is a fault worth naming, and it is not a fault
	// in a build whose body came out of the next archive along.
	return nil, err
}

// searchOrder is the archives [Reader.Body] looks in for a declaration of
// path, in the order it looks.
//
// The declaring package's own archive comes first when this compilation read
// one, because it is the copy every other archive holds a copy of. That this
// compilation read none is not a fault and not an error: it is one fewer place
// to look.
func (r *Reader) searchOrder(path string) []string {
	all := r.archivePaths()
	out := make([]string, 0, len(all)+1)
	first := false
	for _, p := range all {
		if p == path {
			first = true
		}
	}
	if first {
		out = append(out, path)
	}
	for _, p := range all {
		if p != path {
			out = append(out, p)
		}
	}
	return out
}

// bodyIn returns the body of path.name that the archive of archive carries.
//
// The two paths differ wherever a declaration was copied: archive is the
// package whose file is read and path is the package that declares the body.
func (r *Reader) bodyIn(archive, path, name string) (*FuncBody, error) {
	bodies, err := r.Bodies(archive)
	if err != nil {
		return nil, err
	}
	for _, b := range bodies {
		// Generic first: the same declaration can reach a file twice, once
		// through its extension data and once through the private root's
		// inlining list, and the generic one is the shape an importer
		// instantiates from (specs/015-export-data.md).
		if b.Generic && b.Path == path && b.Name == name {
			return b, nil
		}
	}
	for _, b := range bodies {
		if b.Path == path && b.Name == name {
			return b, nil
		}
	}
	return nil, nil
}

// archivePaths is the import paths of the archives this compilation read, in
// sorted order and once each.
func (r *Reader) archivePaths() []string {
	all := make([]string, 0, len(r.imports))
	for _, imp := range r.imports {
		all = append(all, imp.Path)
	}
	sort.Strings(all)
	// Two entries for one path are one archive to search. Bodies caches by
	// path, so a duplicate would not decode the file twice, but it would
	// walk the decoded list twice for nothing.
	out := all[:0]
	for i, p := range all {
		if i == 0 || p != all[i-1] {
			out = append(out, p)
		}
	}
	return out
}

// readBodies decodes one archive's bodies.
func (r *Reader) readBodies(path string) ([]*FuncBody, error) {
	file := ""
	for _, imp := range r.imports {
		if imp.Path == path {
			file = imp.File
			break
		}
	}
	if file == "" {
		return nil, fmt.Errorf("this compilation read no archive for %q", path)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	payload, err := Payload(data)
	if err != nil {
		return nil, err
	}
	var bodies []*FuncBody
	err = func() (err error) {
		// The decoder reports a stream it cannot read by panicking, for the
		// reason [Reader.Read] states. A body the reader refuses arrives as a
		// *BodyError instead, which ReadBodies already returns.
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("%v", v)
			}
		}()
		dec := pkgbits.NewPkgDecoder(path, string(payload))
		_, bodies, err = ReadBodies(r.ctxt, r.packages, dec)
		return err
	}()
	if err != nil {
		return nil, err
	}
	return bodies, nil
}
