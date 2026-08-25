// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// NanogoTool is the producer name nanogo writes. GcTool is the one the Go
// toolchain's compiler is recorded under. A tally is a count of archives by
// producer, so these two strings are what the count is grouped by.
const (
	NanogoTool = "nanogo"
	GcTool     = "gc"
)

// recordVersion is the first line of a producer record. It is a format
// version, so a tree written by an older release fails to parse rather than
// being read wrongly.
const recordVersion = "nanogo-package 1"

// A Producer is the compiler that wrote one archive.
type Producer struct {
	// Tool is [NanogoTool] or [GcTool].
	Tool string
	// Version is the release the tool identified itself as. For gc it is the
	// release in the object header, and for nanogo it is the build identity
	// driver.VersionLine carries.
	Version string
}

// String is the form the record and the tally line use.
func (p Producer) String() string { return p.Tool + " " + p.Version }

// IsNanogo reports whether nanogo compiled the archive.
func (p Producer) IsNanogo() bool { return p.Tool == NanogoTool }

// A Record is what one archive says about itself.
type Record struct {
	// Path is the import path of the package the archive holds.
	Path string
	// Producer is the compiler that wrote it.
	Producer Producer
}

// String is the body of the __.NANOGO member.
func (r Record) String() string {
	return recordVersion + "\npath " + r.Path + "\nproducer " + r.Producer.String() + "\n"
}

// ParseRecord reads a record body.
//
// The parse is strict. A record is the only evidence a distribution offers
// about who compiled it, so a body this function does not fully understand is
// an error and never a partial answer.
func ParseRecord(body string) (Record, error) {
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != 3 || lines[0] != recordVersion {
		return Record{}, fmt.Errorf("not a %q record: %q", recordVersion, body)
	}
	var r Record
	path, ok := strings.CutPrefix(lines[1], "path ")
	if !ok || path == "" {
		return Record{}, fmt.Errorf("record line 2 is %q, not a path", lines[1])
	}
	prod, ok := strings.CutPrefix(lines[2], "producer ")
	if !ok {
		return Record{}, fmt.Errorf("record line 3 is %q, not a producer", lines[2])
	}
	tool, version, ok := strings.Cut(prod, " ")
	if !ok || tool == "" || version == "" {
		return Record{}, fmt.Errorf("producer %q is not a tool and a version", prod)
	}
	if tool != NanogoTool && tool != GcTool {
		return Record{}, fmt.Errorf("producer tool %q is neither %q nor %q", tool, NanogoTool, GcTool)
	}
	r.Path = path
	r.Producer = Producer{Tool: tool, Version: version}
	return r, nil
}

// AddRecord returns the archive with a producer record appended.
//
// Appended, never prepended. The compiler's importer requires __.PKGDEF to be
// the archive's first member and reports "not a package file" for anything
// else, so a record written at the front makes the package unimportable. At
// the end it is invisible to both readers a build has: cmd/link skips a member
// whose name is short and whose extension is not .o or .syso, and the importer
// has already found __.PKGDEF.
func AddRecord(archive []byte, r Record) ([]byte, error) {
	if r.Path == "" {
		return nil, errors.New("a record with no import path says nothing")
	}
	if _, err := ParseRecord(r.String()); err != nil {
		return nil, err
	}
	ms, err := members(archive)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.name == recordMember {
			return nil, fmt.Errorf("%s already carries a producer record", r.Path)
		}
	}
	return appendMember(archive, recordMember, []byte(r.String()))
}

// ReadRecord reports what an archive says about its producer.
//
// An archive with no record is an error and not an assumption. Defaulting an
// unmarked archive to gc would recreate, one level up, the fault this record
// exists to prevent: a tree that reports what it was expected to hold rather
// than what it holds.
func ReadRecord(archive []byte) (Record, error) {
	ms, err := members(archive)
	if err != nil {
		return Record{}, err
	}
	for _, m := range ms {
		if m.name == recordMember {
			return ParseRecord(string(m.body))
		}
	}
	return Record{}, errors.New("the archive carries no " + recordMember + " member, so nothing says which compiler produced it")
}

// ReadRecordFile reports what the archive in a file says about its producer.
func ReadRecordFile(name string) (Record, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return Record{}, err
	}
	r, err := ReadRecord(b)
	if err != nil {
		return Record{}, fmt.Errorf("%s: %v", name, err)
	}
	return r, nil
}
