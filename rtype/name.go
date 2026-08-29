// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"encoding/binary"
	"fmt"
	"strings"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
)

// A descriptor names four different things with one encoding: the type's own
// name, a struct field's name, a method's name and a package's import path.
// internal/abi calls all four a Name, cmd/compile builds all four with dname,
// and this file is the one definition of it here.
//
// # The encoding
//
// A flag byte, the name as a uvarint length and its bytes, and then the tag as
// a uvarint length and its bytes when the flag says there is one. The four
// flag bits are internal/abi.Name's:
//
//	1<<0  the name is exported
//	1<<1  tag data follows the name
//	1<<2  a four-byte package-path offset follows the name and the tag
//	1<<3  the name is an embedded field's
//
// Bit 2 is set for a name that belongs to a different package from the type
// carrying it, which is reachable by embedding a type from another package
// that has an unexported method. Two unexported methods of one name from two
// packages are different methods, and without the path reflect attributes both
// to the type's own package and they become one.
//
// # The symbol name matters as much as the bytes
//
// These symbols are content-addressable and the linker merges them by name, so
// a name spelled differently from gc's is a second symbol for a string that
// already has one. gc's spelling puts the exported bit in the separator: a
// trailing dot for an exported name and a trailing dash for an unexported one,
// with the tag after the separator and ".embedded" after that when the name is
// an embedded field's. So "Name" with tag `json:"name"` is
// type:.namedata.Name.json:"name", unexported "changed" with no tag is
// type:.namedata.changed- and an embedded "Ident" is
// type:.namedata.Ident..embedded.
//
// A name that carries a package path is spelled differently and cannot be
// spelled gc's way. gc names it type:.namedata."".N with a counter, so two
// packages give one name two symbols and the linker merges them by content
// rather than by name. A counter here would number by the order the writer
// reaches types in, which specs/053-determinism.md does not allow, so the path
// goes in the name instead: it is the one thing that makes these names differ,
// so a name derived from it collides exactly when gc's content hash would
// merge.

// nameSymbol returns the symbol holding one encoded Name.
func nameSymbol(name, tag string, exported, embedded bool) Symbol {
	sym, _ := namePathSymbol(name, tag, "", exported, embedded)
	return sym
}

// namePathSymbol returns the symbol holding one encoded Name, and the symbol
// holding the package path it points at when it carries one.
//
// The second result is not optional for a caller that passes a path. The four
// bytes after the name are an offset the linker resolves by name, so a Name
// with bit 2 set and no such symbol in the object is a relocation against
// nothing.
func namePathSymbol(name, tag, pkgPath string, exported, embedded bool) (Symbol, Symbol) {
	var bits byte
	if exported {
		bits |= 1 << 0
	}
	if tag != "" {
		bits |= 1 << 1
	}
	if pkgPath != "" {
		bits |= 1 << 2
	}
	if embedded {
		bits |= 1 << 3
	}
	data := []byte{bits}
	data = binary.AppendUvarint(data, uint64(len(name)))
	data = append(data, name...)
	if tag != "" {
		data = binary.AppendUvarint(data, uint64(len(tag)))
		data = append(data, tag...)
	}

	sep := "-"
	if exported {
		sep = "."
	}
	sname := "type:.namedata." + name + sep + tag
	if embedded {
		// The embedded bit is in the flag byte and the flag byte is not in the
		// symbol name, so without this suffix an embedded field and a field of
		// the same name that is not embedded would share one symbol and one
		// flag byte. gc appends the same suffix for the same reason.
		sname += ".embedded"
	}
	sym := Symbol{
		Name:  sname,
		Kind:  obj.SRODATA,
		Align: 1,
		Dupok: true,
		Data:  data,
	}
	if pkgPath == "" {
		return sym, Symbol{}
	}
	// The four bytes are written as zero and the relocation fills them in.
	// They are the last thing in the encoding, after the name and the tag,
	// which is where internal/abi.Name.PkgPath reads them from.
	ip := importPathSymbol(pkgPath)
	sym.Name += ".pkg." + pathToPrefix(pkgPath)
	sym.Data = append(sym.Data, 0, 0, 0, 0)
	sym.Relocs = append(sym.Relocs, Reloc{
		Off:    int32(len(sym.Data) - 4),
		Size:   4,
		Type:   obj.R_ADDROFF,
		Target: ip.Name,
	})
	return sym, ip
}

// importPathSymbol returns the symbol holding an import path.
//
// It is a Name with no flags set, under a prefix of its own, because the
// linker's own decoder looks a package path up by this name.
func importPathSymbol(path string) Symbol {
	data := []byte{0}
	data = binary.AppendUvarint(data, uint64(len(path)))
	data = append(data, path...)
	return Symbol{
		Name:  "type:.importpath." + pathToPrefix(path) + ".",
		Kind:  obj.SRODATA,
		Align: 1,
		Dupok: true,
		Data:  data,
	}
}

// pathToPrefix escapes an import path for use inside a symbol name.
//
// It is cmd/internal/objabi.PathToPrefix, reproduced rather than approximated.
// A symbol name is parsed by the object reader and by every tool that prints
// one, so a dot in the last element of a path would be read as the separator
// between the path and the identifier that follows it. No path in the standard
// library needs an escape, which is exactly why this is worth writing out: the
// case that needs it is the case nobody tests.
func pathToPrefix(s string) string {
	slash := strings.LastIndex(s, "/")
	escaped := func(r int) bool {
		c := s[r]
		return c <= ' ' || (c == '.' && r > slash) || c == '%' || c == '"' || c >= 0x7F
	}
	n := 0
	for r := 0; r < len(s); r++ {
		if escaped(r) {
			n++
		}
	}
	if n == 0 {
		return s
	}
	const hex = "0123456789abcdef"
	p := make([]byte, 0, len(s)+2*n)
	for r := 0; r < len(s); r++ {
		if escaped(r) {
			p = append(p, '%', hex[s[r]>>4], hex[s[r]&0xf])
		} else {
			p = append(p, s[r])
		}
	}
	return string(p)
}

// isExportedName reports whether an unqualified identifier is exported.
//
// The rule is ir.ExportedName's, because one identifier's exportedness decides
// two things that have to agree. Here it decides the flag in an
// internal/abi.Name's first byte and the separator of the symbol that holds it,
// a trailing dot for an exported name and a trailing dash otherwise. In ir it
// decides whether a literal interface's spelling qualifies the method name with
// a package, and where the method sorts in the list. A second rule here would
// let a method named Ärger be exported to one and unexported to the other.
func isExportedName(name string) bool { return ir.ExportedName(name) }

// fieldName returns the symbol holding one struct field's name.
//
// The package path of an unexported field is not encoded in the name. gc puts
// it in the struct descriptor's own PkgPath instead, once for the whole struct,
// and refuses a struct whose unexported fields do not all come from one
// package. The language cannot produce one: every field of a struct type is
// declared where the type literal is written.
func fieldName(f ir.Field) Symbol {
	return nameSymbol(f.Name, f.Tag, isExportedName(f.Name), f.Embedded)
}

// structPkgPath returns the import path a struct descriptor carries.
//
// It is the package of the struct's first unexported field, and it is empty
// when every field is exported. reflect needs it to reach a field name from
// outside the package that declared the field; a struct with no unexported
// field has no such name to reach.
//
// An unexported field with no package is a field no Go source declared, which
// is what the map slot group's ctrl, slots, key and elem fields are.
// ir.Converter sets the path for every unexported field it converts, so the
// empty one can only come from below the type boundary, and gc gives exactly
// those fields a nil package and writes a nil PkgPath for them. The spelling
// says the same thing from the other side: ir.TypeLinkString leaves such a
// field unqualified, and a descriptor that named a package the spelling did
// not would be two answers to one question.
func structPkgPath(t *ir.Type) (string, error) {
	path := ""
	for _, f := range t.Fields {
		if isExportedName(f.Name) || f.Name == "_" || f.Pkg == "" {
			continue
		}
		if path == "" {
			path = f.Pkg
			continue
		}
		if path != f.Pkg {
			// gc treats this as impossible and stops the compiler. The
			// language agrees: the fields of one struct literal are declared
			// in one file. Reaching it means the IR was built by hand and
			// wrongly, and a descriptor written from it would send reflect to
			// the wrong package for half the field names.
			return "", fmt.Errorf("rtype: %s has unexported fields from %s and %s", t, path, f.Pkg)
		}
	}
	return path, nil
}
