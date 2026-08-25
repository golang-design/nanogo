// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"io"

	"golang.design/x/nanogo/obj"
)

// The archive -pack asks for.
//
// The go command sends -pack on every compile invocation of a real build, so
// an object written bare is an object the build cannot use. The format is the
// Unix archive: the magic, then a 60 byte header per member, then the member's
// bytes padded to an even length.
//
// gc writes two members, __.PKGDEF holding the export data and _go_.o holding
// the object, and nanogo writes both. __.PKGDEF comes first, because
// internal/exportdata.FindPackageDefinition reads member zero and does not
// search. A package with no __.PKGDEF cannot be imported at all.
const (
	archiveMagic  = "!<arch>\n"
	archiveHeader = 60

	// objectMember is the name gc gives the object inside the archive.
	// cmd/link skips a member whose name is short and whose extension is not
	// .o or .syso, so the name is load-bearing and not a convention.
	objectMember = "_go_.o"

	// definitionMember is the name gc gives the export data. Every importer
	// looks it up by this name.
	definitionMember = "__.PKGDEF"
)

// writeTo writes the object, wrapped in an archive when pack is set.
//
// The whole object is built in memory first, because an archive member's
// header carries its length and the length is only known once the member is
// written. Seeking back over an io.Writer is not available and buffering is
// cheaper than making the caller pass a file.
func writeTo(w io.Writer, p *obj.Package, header string, pack bool) error {
	if !pack {
		return p.WriteObject(w, header)
	}
	return writeArchive(w, p, header, nil)
}

// writeArchive writes the archive -pack asks for.
//
// definition is the body of the __.PKGDEF member, which export.Definition
// builds. When it is nil the archive carries the object alone, which is what
// nanogo wrote before specs/015-export-data.md had a writer: legal for
// cmd/link, which skips __.PKGDEF by name, and for cmd/internal/archive,
// which treats it as optional, and not legal for an importer.
//
// Each member is built in memory first, because its header carries its length
// and the length is only known once the member is written. Seeking back over
// an io.Writer is not available and buffering is cheaper than making the
// caller pass a file.
func writeArchive(w io.Writer, p *obj.Package, header string, definition []byte) error {
	var body sizedBuffer
	if err := p.WriteObject(&body, header); err != nil {
		return err
	}
	if _, err := io.WriteString(w, archiveMagic); err != nil {
		return err
	}
	if definition != nil {
		if err := writeArchiveMember(w, definitionMember, definition); err != nil {
			return err
		}
	}
	return writeArchiveMember(w, objectMember, body.b)
}

// writeArchiveMember writes one member, its header and its padding.
func writeArchiveMember(w io.Writer, name string, body []byte) error {
	if err := writeArchiveHeader(w, name, len(body)); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	// A member is padded to an even length, and the padding is not counted in
	// the header. A reader that trusts the header and not the padding lands
	// one byte inside the next header without it.
	if len(body)%2 != 0 {
		if _, err := w.Write([]byte{0}); err != nil {
			return err
		}
	}
	return nil
}

// writeArchiveHeader writes one member header.
//
// The field widths are cmd/internal/archive.FormatHeader's, and they are
// exact: every reader parses the header by fixed offset and not by
// whitespace. The modification time, the owner and the group are zero so that
// two runs over the same input produce the same bytes
// (specs/053-determinism.md).
func writeArchiveHeader(w io.Writer, name string, size int) error {
	if len(name) > 16 {
		return fmt.Errorf("obj: archive member name %q is longer than 16 bytes", name)
	}
	_, err := fmt.Fprintf(w, "%-16s%-12d%-6d%-6d%-8o%-10d`\n", name, 0, 0, 0, 0o644, size)
	return err
}

// sizedBuffer is a minimal io.Writer that keeps what it was given. It exists
// so that this file does not depend on bytes.Buffer's whole surface for one
// method.
type sizedBuffer struct {
	b []byte
}

func (s *sizedBuffer) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}
