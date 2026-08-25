// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"fmt"
	"strings"
	"testing"
)

// appendMember writes one member onto the end of an archive. It builds the
// fixtures the readers here are aimed at; nothing outside a test writes an
// archive, because a distribution copies the ones gc wrote.
func appendMember(a []byte, name string, body []byte) ([]byte, error) {
	if len(name) > nameWidth {
		return nil, fmt.Errorf("member name %q is longer than %d bytes", name, nameWidth)
	}
	if len(a)%2 != 0 {
		a = append(a, 0)
	}
	h := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d%s", name, 0, 0, 0, 0o644, len(body), headerTrailer)
	if len(h) != headerSize {
		return nil, fmt.Errorf("member header for %q is %d bytes, not %d", name, len(h), headerSize)
	}
	a = append(a, h...)
	a = append(a, body...)
	if len(body)%2 != 0 {
		a = append(a, 0)
	}
	return a, nil
}

// fakeArchive builds an archive with a __.PKGDEF holding the given object
// header line, plus an empty object member. It is the shape gc writes, which
// is what every reader here is aimed at.
func fakeArchive(t *testing.T, header string) []byte {
	t.Helper()
	a := []byte(archiveMagic)
	var err error
	a, err = appendMember(a, pkgdefMember, []byte(header+"\n$$B\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, err = appendMember(a, "_go_.o", []byte(header+"\n!\n"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

const gcHeader = "go object darwin arm64 go1.27.0 GOARM64=v8.0 X:regabiwrapper"

func TestMembersReadsWhatAppendMemberWrote(t *testing.T) {
	a := fakeArchive(t, gcHeader)
	// An odd length body, so that the padding rule is exercised: a reader that
	// trusts the header and not the padding lands inside the next header.
	a, err := appendMember(a, "odd.o", []byte("odd"))
	if err != nil {
		t.Fatal(err)
	}
	a, err = appendMember(a, "tail.o", []byte("even"))
	if err != nil {
		t.Fatal(err)
	}
	ms, err := members(a)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{pkgdefMember, "_go_.o", "odd.o", "tail.o"}
	if len(ms) != len(want) {
		t.Fatalf("read %d members, wrote %d: %v", len(ms), len(want), ms)
	}
	for i, n := range want {
		if ms[i].name != n {
			t.Errorf("member %d is %q, want %q", i, ms[i].name, n)
		}
	}
	if string(ms[2].body) != "odd" || string(ms[3].body) != "even" {
		t.Errorf("bodies are %q and %q", ms[2].body, ms[3].body)
	}
}

func TestMembersRejectsMalformedArchives(t *testing.T) {
	good := fakeArchive(t, gcHeader)
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"not an archive", []byte("hello"), "not a Unix archive"},
		{"truncated header", append([]byte(archiveMagic), make([]byte, 10)...), "truncated member header"},
		{"no trailer", append([]byte(archiveMagic), []byte(strings.Repeat("x", headerSize))...), "no trailer"},
		{"unreadable size", func() []byte {
			b := append([]byte(nil), good...)
			copy(b[len(archiveMagic)+48:], []byte("        zz"))
			return b
		}(), "unreadable size"},
		{"size past the end", func() []byte {
			b := append([]byte(nil), good...)
			copy(b[len(archiveMagic)+48:], fmt.Sprintf("%-10d", 1<<20))
			return b
		}(), "claims 1048576 bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := members(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("members = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestAppendMemberRejectsALongName(t *testing.T) {
	if _, err := appendMember([]byte(archiveMagic), strings.Repeat("n", nameWidth+1), nil); err == nil {
		t.Fatal("a 17 byte member name was accepted, and the name field is 16 bytes wide")
	}
}

func TestToolchainVersionReadsTheReleaseOutOfTheHeader(t *testing.T) {
	got, err := ToolchainVersion(fakeArchive(t, gcHeader))
	if err != nil {
		t.Fatal(err)
	}
	if got != "go1.27.0" {
		t.Fatalf("ToolchainVersion = %q, want go1.27.0", got)
	}
}

func TestToolchainVersionRejectsWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"no PKGDEF", func() []byte {
			a, err := appendMember([]byte(archiveMagic), "_go_.o", []byte(gcHeader))
			if err != nil {
				t.Fatal(err)
			}
			return a
		}(), "no __.PKGDEF member"},
		{"not an object header", fakeArchive(t, "this is not an object"), "does not start with a Go object header"},
		{"header too short", fakeArchive(t, "go object darwin arm64"), "no release field"},
		{"not an archive", []byte("nope"), "not a Unix archive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ToolchainVersion(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ToolchainVersion = %v, want an error containing %q", err, c.want)
			}
		})
	}
}
