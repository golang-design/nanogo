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

// writeTree lays out a pkg directory holding one archive per record, at the
// path the record's import path names.
func writeTree(t *testing.T, target string, rs ...Record) string {
	t.Helper()
	root := t.TempDir()
	for _, r := range rs {
		a, err := AddRecord(fakeArchive(t, gcHeader), r)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(root, "pkg", target, filepath.FromSlash(r.Path)+archiveExt)
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, a, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTallyLineNamesTheDenominatorAndEveryProducer(t *testing.T) {
	gc := Producer{GcTool, "go1.27.0"}
	ng := Producer{NanogoTool, "3fbcea1"}
	cases := []struct {
		name string
		in   []Record
		want string
	}{
		{
			// The tarball as it ships today. Nothing is nanogo's, and the line
			// has to say so rather than reporting a success.
			name: "all gc",
			in:   []Record{{"internal/abi", gc}, {"internal/goarch", gc}, {"runtime", gc}},
			want: "nanogo: 0 of 3 packages in the bootstrap closure compiled by nanogo; 3 by gc go1.27.0",
		},
		{
			name: "a mixed tree",
			in:   []Record{{"internal/abi", ng}, {"internal/goarch", gc}, {"runtime", gc}},
			want: "nanogo: 1 of 3 packages in the bootstrap closure compiled by nanogo; 2 by gc go1.27.0",
		},
		{
			// Two gc releases in one tree is a broken release job, and the
			// line names both rather than adding them up under one heading.
			name: "two gc releases",
			in:   []Record{{"internal/abi", Producer{GcTool, "go1.27.1"}}, {"runtime", gc}},
			want: "nanogo: 0 of 2 packages in the bootstrap closure compiled by nanogo; 1 by gc go1.27.0; 1 by gc go1.27.1",
		},
		{
			name: "everything nanogo",
			in:   []Record{{"internal/abi", ng}, {"runtime", ng}},
			want: "nanogo: 2 of 2 packages in the bootstrap closure compiled by nanogo",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeTree(t, "darwin_arm64", c.in...)
			got, err := TallyLine(root, "darwin_arm64")
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("TallyLine =\n\t%s\nwant\n\t%s", got, c.want)
			}
		})
	}
}

func TestTallyTreeSortsByImportPath(t *testing.T) {
	gc := Producer{GcTool, "go1.27.0"}
	root := writeTree(t, "darwin_arm64", Record{"runtime", gc}, Record{"internal/abi", gc}, Record{"math/bits", gc})
	tally, err := TallyTree(root, "darwin_arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"internal/abi", "math/bits", "runtime"}
	for i, w := range want {
		if tally.Records[i].Path != w {
			t.Fatalf("record %d is %s, want %s", i, tally.Records[i].Path, w)
		}
	}
}

func TestTallyTreeFailsOnATreeItCannotAccountFor(t *testing.T) {
	gc := Producer{GcTool, "go1.27.0"}

	t.Run("an unmarked archive", func(t *testing.T) {
		root := writeTree(t, "darwin_arm64", Record{"runtime", gc})
		name := filepath.Join(root, "pkg", "darwin_arm64", "sneaky.a")
		if err := os.WriteFile(name, fakeArchive(t, gcHeader), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := TallyTree(root, "darwin_arm64"); err == nil {
			t.Fatal("an archive with no record was counted, so a gc archive can enter the tree unnamed")
		}
	})

	t.Run("a record that names another package", func(t *testing.T) {
		root := writeTree(t, "darwin_arm64", Record{"runtime", gc})
		a, err := AddRecord(fakeArchive(t, gcHeader), Record{"math/bits", gc})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "pkg", "darwin_arm64", "strconv.a"), a, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = TallyTree(root, "darwin_arm64")
		if err == nil || !strings.Contains(err.Error(), "its record says") {
			t.Fatalf("TallyTree = %v, want an error about the path disagreeing", err)
		}
	})

	t.Run("no archives at all", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "pkg", "darwin_arm64"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := TallyTree(root, "darwin_arm64")
		if err == nil || !strings.Contains(err.Error(), "holds no archives") {
			t.Fatalf("TallyTree = %v, want an error about an empty pkg directory", err)
		}
	})

	t.Run("no tree", func(t *testing.T) {
		if _, err := TallyLine(t.TempDir(), "darwin_arm64"); err == nil {
			t.Fatal("a directory with no pkg tree was accepted")
		}
	})
}
