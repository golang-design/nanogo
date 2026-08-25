// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordRoundTrips(t *testing.T) {
	for _, r := range []Record{
		{Path: "internal/abi", Producer: Producer{GcTool, "go1.27.0"}},
		{Path: "runtime", Producer: Producer{NanogoTool, "3fbcea1+dirty"}},
	} {
		got, err := ParseRecord(r.String())
		if err != nil {
			t.Fatalf("%v: %v", r, err)
		}
		if got != r {
			t.Fatalf("round trip gave %v, want %v", got, r)
		}
	}
}

func TestParseRecordIsStrict(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", "not a"},
		{"wrong version", "nanogo-package 2\npath p\nproducer gc go1.27.0\n", "not a"},
		{"four lines", recordVersion + "\npath p\nproducer gc go1.27.0\nextra\n", "not a"},
		{"no path", recordVersion + "\nname p\nproducer gc go1.27.0\n", "not a path"},
		{"empty path", recordVersion + "\npath \nproducer gc go1.27.0\n", "not a path"},
		{"no producer", recordVersion + "\npath p\nby gc go1.27.0\n", "not a producer"},
		{"producer with no version", recordVersion + "\npath p\nproducer gc\n", "not a tool and a version"},
		{"unknown tool", recordVersion + "\npath p\nproducer clang 17\n", "neither"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseRecord(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ParseRecord = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestAddRecordIsReadBack(t *testing.T) {
	want := Record{Path: "internal/abi", Producer: Producer{GcTool, "go1.27.0"}}
	a, err := AddRecord(fakeArchive(t, gcHeader), want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecord(a)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ReadRecord = %v, want %v", got, want)
	}
	// The record goes last. __.PKGDEF must stay first or the compiler's
	// importer reports "not a package file", and internal/e2e proves that
	// against the real importer.
	ms, err := members(a)
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].name != pkgdefMember {
		t.Errorf("first member is %q, want %q", ms[0].name, pkgdefMember)
	}
	if ms[len(ms)-1].name != recordMember {
		t.Errorf("last member is %q, want %q", ms[len(ms)-1].name, recordMember)
	}
}

func TestAddRecordRefusesWhatItCannotStandBehind(t *testing.T) {
	good := fakeArchive(t, gcHeader)
	if _, err := AddRecord(good, Record{Producer: Producer{GcTool, "go1.27.0"}}); err == nil {
		t.Error("a record with no import path was accepted")
	}
	if _, err := AddRecord(good, Record{Path: "p", Producer: Producer{"clang", "17"}}); err == nil {
		t.Error("a record naming an unknown tool was accepted")
	}
	twice, err := AddRecord(good, Record{Path: "p", Producer: Producer{GcTool, "go1.27.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddRecord(twice, Record{Path: "p", Producer: Producer{NanogoTool, "x"}}); err == nil {
		t.Error("a second record was accepted, and two records make the producer ambiguous")
	}
	if _, err := AddRecord([]byte("nope"), Record{Path: "p", Producer: Producer{GcTool, "go1.27.0"}}); err == nil {
		t.Error("a non-archive was accepted")
	}
}

// An unmarked archive is the whole reason this record exists: nanogo writes
// gc's object header verbatim, so nothing else in the bytes distinguishes the
// two producers. Reading one must fail rather than assume.
func TestReadRecordRefusesAnUnmarkedArchive(t *testing.T) {
	_, err := ReadRecord(fakeArchive(t, gcHeader))
	if err == nil || !strings.Contains(err.Error(), "carries no "+recordMember) {
		t.Fatalf("ReadRecord = %v, want an error about the missing record", err)
	}
	if _, err := ReadRecord([]byte("nope")); err == nil {
		t.Error("a non-archive was accepted")
	}
}

func TestReadRecordFile(t *testing.T) {
	dir := t.TempDir()
	want := Record{Path: "internal/abi", Producer: Producer{GcTool, "go1.27.0"}}
	a, err := AddRecord(fakeArchive(t, gcHeader), want)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "abi.a")
	if err := os.WriteFile(name, a, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecordFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ReadRecordFile = %v, want %v", got, want)
	}

	bad := filepath.Join(dir, "bad.a")
	if err := os.WriteFile(bad, fakeArchive(t, gcHeader), 0o600); err != nil {
		t.Fatal(err)
	}
	// The file name is in the message, because a tally over a tree reports one
	// error and the reader has to know which archive it is about.
	if _, err := ReadRecordFile(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("ReadRecordFile = %v, want an error naming %s", err, bad)
	}
	if _, err := ReadRecordFile(filepath.Join(dir, "absent.a")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestProducerIsNanogo(t *testing.T) {
	if !(Producer{NanogoTool, "x"}).IsNanogo() {
		t.Error("nanogo's own producer does not report as nanogo")
	}
	if (Producer{GcTool, "go1.27.0"}).IsNanogo() {
		t.Error("gc reports as nanogo")
	}
}
