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
// the object. nanogo writes only the second, because specs/015-export-data.md
// has no writer. That is legal for the two readers a build has: cmd/link skips
// __.PKGDEF by name, and cmd/internal/archive, which go tool pack uses to
// append the assembler's objects, treats it as optional. It is not legal for a
// third: a package with no __.PKGDEF cannot be imported, which is the same
// limit from the other side.
const (
	archiveMagic  = "!<arch>\n"
	archiveHeader = 60

	// objectMember is the name gc gives the object inside the archive.
	// cmd/link skips a member whose name is short and whose extension is not
	// .o or .syso, so the name is load-bearing and not a convention.
	objectMember = "_go_.o"
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
	var body sizedBuffer
	if err := p.WriteObject(&body, header); err != nil {
		return err
	}
	if _, err := io.WriteString(w, archiveMagic); err != nil {
		return err
	}
	if err := writeArchiveHeader(w, objectMember, len(body.b)); err != nil {
		return err
	}
	if _, err := w.Write(body.b); err != nil {
		return err
	}
	// A member is padded to an even length, and the padding is not counted in
	// the header. A reader that trusts the header and not the padding lands
	// one byte inside the next header without it.
	if len(body.b)%2 != 0 {
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
