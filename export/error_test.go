// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// member is one archive member.
type member struct {
	name string
	body []byte
}

// buildArchive writes the members into a Unix archive.
//
// The test builds its own archives rather than damaging a real one, because
// each case has to name exactly one thing that is wrong. A mutated real
// archive fails for whichever check happens to come first.
func buildArchive(members ...member) []byte {
	out := []byte(archiveMagic)
	for _, m := range members {
		out = append(out, fmt.Appendf(nil, "%-16s%-12d%-6d%-6d%-8o%-10d`\n",
			m.name, 0, 0, 0, 0o644, len(m.body))...)
		out = append(out, m.body...)
		if len(m.body)%2 != 0 {
			out = append(out, 0)
		}
	}
	return out
}

// buildDefinition wraps a payload in the headers a __.PKGDEF member carries.
func buildDefinition(payload []byte) []byte {
	out := []byte("go object darwin arm64 go1.27.0\nbuild id \"x\"\n$$B\nu")
	out = append(out, payload...)
	return append(out, []byte(sectionEnd)...)
}

// fixturePayload returns the unified export data of the fixture package, so
// that a test can damage a stream gc really wrote.
func fixturePayload(t *testing.T) []byte {
	t.Helper()
	dir := fixtureModule(t)
	data, err := os.ReadFile(exportFile(t, dir, "."))
	if err != nil {
		t.Fatal(err)
	}
	def, err := packageDefinition(data)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := unified(def)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// TestReadRejectsMalformedFiles is the reader's refusal test.
//
// Every case is a file the build could hand nanogo, and each message has to
// say which of them it is. "cannot read export data" would be true of all of
// them and would tell a user nothing about whether to rebuild the package, to
// fix the configuration or to report a bug.
func TestReadRejectsMalformedFiles(t *testing.T) {
	payload := fixturePayload(t)

	// A version the format does not have yet. The word is the first four
	// bytes of the payload, so a release that adds a version reaches nanogo
	// as this message and never as a misread declaration.
	future := append([]byte(nil), payload...)
	binary.LittleEndian.PutUint32(future, 99)

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "not an archive",
			data: []byte("this is not an archive\n"),
			want: `does not start with "!<arch>\n"`,
		},
		{
			name: "no package definition member",
			data: buildArchive(member{"_go_.o", []byte("object")}),
			want: "holds no __.PKGDEF member",
		},
		{
			name: "a member size that is not a number",
			data: []byte(archiveMagic + "__.PKGDEF       0           0     0     644     nonsense  `\n"),
			want: "unreadable size",
		},
		{
			name: "a member that runs past the end of the file",
			data: append(buildArchive(member{definitionMember, make([]byte, 40)})[:len(archiveMagic)+headerSize], []byte("short")...),
			want: "only 5 are left in the file",
		},
		{
			name: "a definition with no object header",
			data: buildArchive(member{definitionMember, []byte("not an object header\n$$B\nu\n$$\n")}),
			want: "does not start with an object header",
		},
		{
			name: "a definition with no export data section",
			data: buildArchive(member{definitionMember, []byte("go object darwin arm64 go1.27.0\nbuild id \"x\"\n")}),
			want: "has no export data section",
		},
		{
			name: "the textual export format",
			data: buildArchive(member{definitionMember, []byte("go object darwin arm64 go1.27.0\n$$\nu\n$$\n")}),
			want: `"$$" is not the binary format`,
		},
		{
			name: "an empty section",
			data: buildArchive(member{definitionMember, []byte("go object darwin arm64 go1.27.0\n$$B\n")}),
			want: "the export data section is empty",
		},
		{
			name: "an export format that is not the unified one",
			data: buildArchive(member{definitionMember, []byte("go object darwin arm64 go1.27.0\n$$B\ni\n$$\n")}),
			want: `format "i" is not the unified format`,
		},
		{
			name: "a section with no end marker",
			data: buildArchive(member{definitionMember, []byte("go object darwin arm64 go1.27.0\n$$B\nupayload")}),
			want: `does not end with "\n$$\n"`,
		},
		{
			name: "a payload too short to hold a header",
			data: buildArchive(member{definitionMember, buildDefinition([]byte{1, 2})}),
			want: "export data is truncated",
		},
		{
			name: "a payload whose element table runs past the end",
			data: buildArchive(member{definitionMember, buildDefinition(payload[:len(payload)-16])}),
			want: "export data is truncated",
		},
		{
			name: "a version the format does not have",
			data: buildArchive(member{definitionMember, buildDefinition(future)}),
			want: "export data version 99 is greater than maximum supported version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "pkg.a")
			if err := os.WriteFile(file, tt.data, 0o600); err != nil {
				t.Fatal(err)
			}
			r := NewReader()
			pkg, err := r.Read("nanogo.example/broken", file)
			if err == nil {
				t.Fatalf("Read succeeded and returned %v", pkg)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Read = %v, want a message containing %q", err, tt.want)
			}
			if len(r.Imports()) != 0 {
				t.Errorf("a file that did not read was recorded as an import: %+v", r.Imports())
			}
		})
	}
}

// TestReadReportsAMissingFile checks the case the build itself produces: an
// -importcfg entry that names a file nobody wrote.
func TestReadReportsAMissingFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "absent.a")
	_, err := NewReader().Read("nanogo.example/absent", file)
	if err == nil {
		t.Fatal("Read on a missing file returned no error")
	}
	if !strings.Contains(err.Error(), file) {
		t.Errorf("Read = %v, and the message does not name %s", err, file)
	}
}

// TestPackageDefinitionSkipsOtherMembers checks that the definition is found
// when it is not the first member.
//
// gc writes it first and every reader in the Go tree assumes so. nanogo walks
// the members instead, because the assumption is not in the format and an
// archive that a tool rewrote is not a file nanogo should misread.
func TestPackageDefinitionSkipsOtherMembers(t *testing.T) {
	// The first member has an odd length, so a reader that forgot the
	// padding byte lands one byte inside the second member's header.
	data := buildArchive(
		member{"_go_.o", []byte("odd")},
		member{definitionMember, []byte("definition")},
	)
	got, err := packageDefinition(data)
	if err != nil {
		t.Fatalf("packageDefinition: %v", err)
	}
	if string(got) != "definition" {
		t.Errorf("packageDefinition = %q, want %q", got, "definition")
	}
}
