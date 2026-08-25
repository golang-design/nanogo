// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The Unix archive format, as cmd/internal/archive writes and reads it. The
// widths are exact: every reader parses a header by fixed offset and not by
// whitespace.
const (
	archiveMagic  = "!<arch>\n"
	headerSize    = 60
	nameWidth     = 16
	pkgdefMember  = "__.PKGDEF"
	objectPrefix  = "go object "
	headerTrailer = "`\n"
)

// A member is one file inside an archive.
type member struct {
	name string
	body []byte
}

// members lists the members of an archive, in the order they appear.
//
// The order matters to one reader and is therefore preserved rather than
// sorted: the compiler's importer requires __.PKGDEF first and reports
// "not a package file" when anything precedes it.
func members(a []byte) ([]member, error) {
	if !strings.HasPrefix(string(a), archiveMagic) {
		return nil, errors.New("not a Unix archive: it does not start with !<arch>")
	}
	var out []member
	for off := len(archiveMagic); off < len(a); {
		if off+headerSize > len(a) {
			return nil, fmt.Errorf("truncated member header at offset %d", off)
		}
		h := a[off : off+headerSize]
		if string(h[headerSize-len(headerTrailer):]) != headerTrailer {
			return nil, fmt.Errorf("member header at offset %d has no trailer", off)
		}
		name := strings.TrimRight(string(h[:nameWidth]), " ")
		size, err := strconv.Atoi(strings.TrimSpace(string(h[48:58])))
		if err != nil || size < 0 {
			return nil, fmt.Errorf("member %q at offset %d has an unreadable size", name, off)
		}
		off += headerSize
		if off+size > len(a) {
			return nil, fmt.Errorf("member %q claims %d bytes and the archive has %d left", name, size, len(a)-off)
		}
		out = append(out, member{name: name, body: a[off : off+size]})
		off += size
		// A member is padded to an even length and the padding is not counted
		// in the header. A reader that trusts the header and not the padding
		// lands one byte inside the next header.
		if size%2 != 0 {
			off++
		}
	}
	return out, nil
}

// ToolchainVersion is the Go release named in an archive's object header.
//
// The header line is written by whichever tool produced the object and reads
//
//	go object darwin arm64 go1.27.0 GOARM64=v8.0 X:regabiwrapper,...
//
// so the release is field 4. This is read out of the bytes rather than taken
// from the environment on purpose: a release job whose toolchain resolved to a
// different patch release than the pin claims is exactly the disagreement the
// distribution has to fail on.
func ToolchainVersion(archive []byte) (string, error) {
	ms, err := members(archive)
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if m.name != pkgdefMember {
			continue
		}
		line, _, _ := strings.Cut(string(m.body), "\n")
		if !strings.HasPrefix(line, objectPrefix) {
			return "", errors.New(pkgdefMember + " does not start with a Go object header")
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			return "", fmt.Errorf("object header %q has no release field", line)
		}
		return f[4], nil
	}
	return "", errors.New("the archive has no " + pkgdefMember + " member, so it declares no toolchain")
}
