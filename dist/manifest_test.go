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

const (
	sumA = "0000000000000000000000000000000000000000000000000000000000000001"
	sumB = "0000000000000000000000000000000000000000000000000000000000000002"
)

func TestManifestRoundTrips(t *testing.T) {
	want := Manifest{
		{Path: "internal/abi", Producer: Producer{GcTool, "go1.27.0"}, Sum: sumA},
		{Path: "runtime", Producer: Producer{NanogoTool, "3fbcea1+dirty"}, Sum: sumB},
	}
	got, err := ParseManifest([]byte(want.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("round trip gave %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d is %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseManifestIsStrict(t *testing.T) {
	good := Manifest{{Path: "runtime", Producer: Producer{GcTool, "go1.27.0"}, Sum: sumA}}.String()
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", "does not start with"},
		{"a version this release does not write", "nanogo-manifest 2\n", "does not start with"},
		{"no records", manifestVersion + "\n", "lists no archives"},
		{"three fields", manifestVersion + "\nruntime gc go1.27.0\n", "a path, a tool, a version and a checksum"},
		{"an unknown tool", manifestVersion + "\nruntime clang 17 " + sumA + "\n", "neither"},
		{"a checksum that is not one", manifestVersion + "\nruntime gc go1.27.0 deadbeef\n", "where a SHA-256 belongs"},
		{"a checksum that is not hex", manifestVersion + "\nruntime gc go1.27.0 " + strings.Repeat("z", 64) + "\n", "where a SHA-256 belongs"},
		// Sorted, because the tally is read by people and a duplicate path
		// would let one archive be counted twice.
		{"out of order", manifestVersion + "\nruntime gc go1.27.0 " + sumA + "\ninternal/abi gc go1.27.0 " + sumB + "\n", "not sorted"},
		{"a repeated path", manifestVersion + "\nruntime gc go1.27.0 " + sumA + "\nruntime gc go1.27.0 " + sumB + "\n", "not sorted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ParseManifest = %v, want an error containing %q", err, c.want)
			}
		})
	}
	if _, err := ParseManifest([]byte(good)); err != nil {
		t.Fatalf("the good manifest was rejected: %v", err)
	}
}

func TestWriteManifestSortsAndRefusesWhatItCannotParse(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pkg", "darwin_arm64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gc := Producer{GcTool, "go1.27.0"}
	unsorted := Manifest{{Path: "runtime", Producer: gc, Sum: sumA}, {Path: "internal/abi", Producer: gc, Sum: sumB}}
	if err := WriteManifest(root, "darwin_arm64", unsorted); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(root, "darwin_arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Path != "internal/abi" || got[1].Path != "runtime" {
		t.Fatalf("the manifest was written unsorted: %v", got)
	}
	// The caller's slice is not reordered under it.
	if unsorted[0].Path != "runtime" {
		t.Error("WriteManifest sorted the caller's slice")
	}
	if err := WriteManifest(root, "darwin_arm64", Manifest{{Path: "x", Producer: Producer{"clang", "17"}, Sum: sumA}}); err == nil {
		t.Error("a manifest naming a tool the parser refuses was written")
	}
	if _, err := ReadManifest(t.TempDir(), "darwin_arm64"); err == nil {
		t.Error("a tree with no manifest was read")
	}
	// The file name is in the message, because a tree reports one error and
	// the reader has to know which file it is about.
	bad := filepath.Join(dir, ManifestFile)
	if err := os.WriteFile(bad, []byte("rubbish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(root, "darwin_arm64"); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("ReadManifest = %v, want an error naming %s", err, bad)
	}
}

func TestSumFile(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "x")
	if err := os.WriteFile(name, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := sumFile(name)
	if err != nil {
		t.Fatal(err)
	}
	// The SHA-256 of "abc", so a wrong hash function is caught rather than a
	// hash compared against itself.
	if want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("sumFile = %s, want %s", got, want)
	}
	if _, err := sumFile(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing file was hashed")
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
