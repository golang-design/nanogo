// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"bytes"
	"fmt"
	"os"
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
