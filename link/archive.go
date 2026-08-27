// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// arMagic starts every archive.
const arMagic = "!<arch>\n"

// arHdrSize is the size of one member header: sixteen bytes of name, then
// the date, the two ids, the mode, the size, and the two byte terminator.
const arHdrSize = 16 + 12 + 6 + 6 + 8 + 10 + 2

// pkgdef is the archive member that holds export data rather than an
// object. cmd/link skips it, and so does this reader: it is absent from a
// -linkobj build, so reading it would make the two builds differ.
const pkgdef = "__.PKGDEF"

// An ArchiveMember is one entry of an archive.
type ArchiveMember struct {
	Name string
	Data []byte // aliases the archive buffer
}

// ReadArchiveMembers splits an archive into its members.
//
// The names are what the format holds, so a name of sixteen characters may
// be a truncation: the name field is fixed width and the writer does not
// escape it. [ReadArchive] takes that into account when it decides which
// members are objects.
func ReadArchiveMembers(b []byte, name string) ([]ArchiveMember, error) {
	if !strings.HasPrefix(string(b), arMagic) {
		return nil, errAt(name, "not an archive: it does not start with %q", arMagic)
	}
	var out []ArchiveMember
	off := len(arMagic)
	for off < len(b) {
		if off&1 != 0 {
			off++ // members are aligned to an even offset
		}
		if off == len(b) {
			break
		}
		if off+arHdrSize > len(b) {
			return nil, errorf(name, int64(off), "a member header needs %d bytes and %d are left", arHdrSize, len(b)-off)
		}
		h := b[off : off+arHdrSize]
		if string(h[arHdrSize-2:]) != "`\n" {
			return nil, errorf(name, int64(off), "the member header does not end in the archive terminator")
		}
		member := strings.TrimRight(string(h[:16]), " ")
		size, err := strconv.ParseInt(strings.TrimSpace(string(h[48:58])), 10, 64)
		if err != nil || size < 0 {
			return nil, errorf(name, int64(off), "the member %q has an unreadable size %q", member, string(h[48:58]))
		}
		start := off + arHdrSize
		if int64(start)+size > int64(len(b)) {
			return nil, errorf(name, int64(start), "the member %q is %d bytes and %d are left", member, size, len(b)-start)
		}
		out = append(out, ArchiveMember{Name: member, Data: b[start : int64(start)+size]})
		off = start + int(size)
	}
	if len(out) == 0 {
		return nil, errAt(name, "the archive holds no members")
	}
	return out, nil
}

// ReadArchive reads every object an archive holds.
//
// pkg is the import path of the package the archive was built for, which
// no object in it carries.
//
// Which members are objects is cmd/link's rule, kept because a rule of our
// own would load a member cmd/link skips or skip one it loads. __.PKGDEF
// is export data. A member whose name is shorter than the sixteen byte
// name field is an object only if it ends in .o or .syso, which is what
// keeps a build tool's added section out. A name that fills the field may
// have been truncated, so its extension proves nothing and it is read as
// an object: this is what loads rt0_darwin_arm64.o, whose name is eighteen
// characters.
func ReadArchive(b []byte, name, pkg string) ([]*Object, error) {
	members, err := ReadArchiveMembers(b, name)
	if err != nil {
		return nil, err
	}
	var out []*Object
	for _, m := range members {
		if m.Name == pkgdef || !isObjectMember(m.Name) {
			continue
		}
		o, err := ReadObject(m.Data, fmt.Sprintf("%s(%s)", name, m.Name), pkg)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		return nil, errAt(name, "the archive holds no object")
	}
	return out, nil
}

// isObjectMember reports whether a member name names an object.
func isObjectMember(name string) bool {
	if len(name) >= 16 {
		return true // the name field is full, so the extension may be cut off
	}
	switch filepath.Ext(name) {
	case ".o", ".syso":
		return true
	}
	return false
}
