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

func TestVersionRoundTrips(t *testing.T) {
	want := Version{Release: "nanogo0.1.0", Go: "go1.27.0", Target: "darwin_arm64", Packages: 27, ByNanogo: 0, ByGc: 27}
	got, err := ParseVersion([]byte(want.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip gave %+v, want %+v", got, want)
	}
	// Line one answers "which nanogo is this" with no parsing, the way Go's
	// own VERSION file does.
	if first, _, _ := strings.Cut(want.String(), "\n"); first != "nanogo0.1.0" {
		t.Fatalf("line 1 is %q", first)
	}
}

func TestParseVersionIsStrict(t *testing.T) {
	good := Version{Release: "nanogo0.1.0", Go: "go1.27.0", Target: "darwin_arm64", Packages: 2, ByGc: 2}.String()
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", "has 1 lines"},
		{"short", "nanogo0.1.0\ngo go1.27.0\n", "has 2 lines"},
		{"not a nanogo release", strings.Replace(good, "nanogo0.1.0", "go1.27.0", 1), "not a nanogo release"},
		{"no go line", strings.Replace(good, "go go1.27.0", "goversion go1.27.0", 1), `not "go"`},
		{"empty target", strings.Replace(good, "target darwin_arm64", "target ", 1), `not "target"`},
		{"no packages line", strings.Replace(good, "packages 2", "count 2", 1), `not "packages"`},
		{"packages is not a number", strings.Replace(good, "packages 2", "packages many", 1), "where a count belongs"},
		{"a negative count", strings.Replace(good, "gc 2", "gc -1", 1), "where a count belongs"},
		{"the split does not add up", strings.Replace(good, "gc 2", "gc 1", 1), "and 0+1 by producer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseVersion([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ParseVersion = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// version writes a VERSION file into a tree writeTree built.
func version(t *testing.T, root string, v Version) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, VersionFile), []byte(v.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyTreeAcceptsATreeThatMatchesItsClaim(t *testing.T) {
	gc := Producer{GcTool, "go1.27.0"}
	root := writeTree(t, "darwin_arm64", Record{"internal/abi", gc}, Record{"runtime", gc})
	version(t, root, Version{Release: "nanogo0.1.0", Go: "go1.27.0", Target: "darwin_arm64", Packages: 2, ByGc: 2})
	if err := VerifyTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVersion(root); err != nil {
		t.Fatal(err)
	}
}

// The check the release job fails on. A counter written once and never
// compared is a counter nobody has reason to trust.
func TestVerifyTreeFailsWhenTheClaimAndTheArchivesDisagree(t *testing.T) {
	gc := Producer{GcTool, "go1.27.0"}
	base := Version{Release: "nanogo0.1.0", Go: "go1.27.0", Target: "darwin_arm64", Packages: 2, ByGc: 2}
	cases := []struct {
		name    string
		records []Record
		version Version
		want    string
	}{
		{
			name:    "a package count that is too high",
			records: []Record{{"runtime", gc}},
			version: base,
			want:    "says 2 packages and pkg/darwin_arm64 holds 1",
		},
		{
			// The claim this whole spec exists to stop: a tree of gc archives
			// reported as nanogo's work.
			name:    "nanogo credited for gc's archives",
			records: []Record{{"internal/abi", gc}, {"runtime", gc}},
			version: Version{Release: "nanogo0.1.0", Go: "go1.27.0", Target: "darwin_arm64", Packages: 2, ByNanogo: 2},
			want:    "says nanogo compiled 2 packages and the archives say 0",
		},
		{
			// The release job resolved go1.27.3 and the pin says go1.27.0.
			// setup-go's 1.27.x with check-latest makes this the likely
			// failure, not a hypothetical one.
			name:    "a gc release that is not the pinned one",
			records: []Record{{"internal/abi", Producer{GcTool, "go1.27.3"}}, {"runtime", gc}},
			version: base,
			want:    "compiled by gc go1.27.3 and VERSION pins go1.27.0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeTree(t, "darwin_arm64", c.records...)
			version(t, root, c.version)
			err := VerifyTree(root)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("VerifyTree = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestVerifyTreeFailsWithoutAVersionOrAPkgTree(t *testing.T) {
	if err := VerifyTree(t.TempDir()); err == nil {
		t.Error("a directory with no VERSION was accepted")
	}
	if _, err := ReadVersion(t.TempDir()); err == nil {
		t.Error("ReadVersion accepted a directory with no VERSION")
	}
	root := t.TempDir()
	version(t, root, Version{Release: "nanogo0.1.0", Go: "go1.27.0", Target: "darwin_arm64"})
	if err := VerifyTree(root); err == nil {
		t.Error("a VERSION with no pkg tree beside it was accepted")
	}
}

// The gc count is derived rather than read back, so a tree whose gc count is
// right for the wrong reason has to fail too.
func TestVerifyTreeDerivesTheGcCount(t *testing.T) {
	root := writeTree(t, "darwin_arm64",
		Record{"internal/abi", Producer{NanogoTool, "3fbcea1"}},
		Record{"runtime", Producer{GcTool, "go1.27.0"}})
	version(t, root, Version{Release: "nanogo0.1.0", Go: "go1.27.0", Target: "darwin_arm64", Packages: 2, ByNanogo: 1, ByGc: 1})
	if err := VerifyTree(root); err != nil {
		t.Fatal(err)
	}
}
