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

// writeTree lays out a pkg directory holding one archive per record, with the
// manifest that accounts for them.
func writeTree(t *testing.T, target string, rs ...Record) string {
	t.Helper()
	root := t.TempDir()
	var m Manifest
	for _, r := range rs {
		name := filepath.Join(root, "pkg", target, filepath.FromSlash(r.Path)+archiveExt)
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		// One distinct archive per package, so that a record cannot match the
		// wrong file by accident.
		if err := os.WriteFile(name, fakeArchive(t, gcHeader+" "+r.Path), 0o600); err != nil {
			t.Fatal(err)
		}
		sum, err := sumFile(name)
		if err != nil {
			t.Fatal(err)
		}
		r.Sum = sum
		m = append(m, r)
	}
	if len(m) > 0 {
		if err := WriteManifest(root, target, m); err != nil {
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
			in:   []Record{{Path: "internal/abi", Producer: gc}, {Path: "internal/goarch", Producer: gc}, {Path: "runtime", Producer: gc}},
			want: "nanogo: 0 of 3 packages in this distribution compiled by nanogo; 3 by gc go1.27.0",
		},
		{
			name: "a mixed tree",
			in:   []Record{{Path: "internal/abi", Producer: ng}, {Path: "internal/goarch", Producer: gc}, {Path: "runtime", Producer: gc}},
			want: "nanogo: 1 of 3 packages in this distribution compiled by nanogo; 2 by gc go1.27.0",
		},
		{
			// Two gc releases in one tree is a broken release job, and the
			// line names both rather than adding them up under one heading.
			name: "two gc releases",
			in:   []Record{{Path: "internal/abi", Producer: Producer{GcTool, "go1.27.1"}}, {Path: "runtime", Producer: gc}},
			want: "nanogo: 0 of 2 packages in this distribution compiled by nanogo; 1 by gc go1.27.0; 1 by gc go1.27.1",
		},
		{
			name: "everything nanogo",
			in:   []Record{{Path: "internal/abi", Producer: ng}, {Path: "runtime", Producer: ng}},
			want: "nanogo: 2 of 2 packages in this distribution compiled by nanogo",
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
	root := writeTree(t, "darwin_arm64", Record{Path: "runtime", Producer: gc}, Record{Path: "internal/abi", Producer: gc}, Record{Path: "math/bits", Producer: gc})
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

	t.Run("an archive the manifest does not name", func(t *testing.T) {
		root := writeTree(t, "darwin_arm64", Record{Path: "runtime", Producer: gc})
		name := filepath.Join(root, "pkg", "darwin_arm64", "sneaky.a")
		if err := os.WriteFile(name, fakeArchive(t, gcHeader), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := TallyTree(root, "darwin_arm64")
		if err == nil || !strings.Contains(err.Error(), "not in "+ManifestFile) {
			t.Fatalf("TallyTree = %v, want an error about the unnamed archive", err)
		}
	})

	t.Run("an archive the manifest names and the tree does not hold", func(t *testing.T) {
		root := writeTree(t, "darwin_arm64", Record{Path: "runtime", Producer: gc}, Record{Path: "math/bits", Producer: gc})
		if err := os.Remove(filepath.Join(root, "pkg", "darwin_arm64", "math", "bits.a")); err != nil {
			t.Fatal(err)
		}
		_, err := TallyTree(root, "darwin_arm64")
		if err == nil || !strings.Contains(err.Error(), "does not hold it") {
			t.Fatalf("TallyTree = %v, want an error about the missing archive", err)
		}
	})

	// The fault the checksum exists for: one archive put where another
	// belongs. Without the hash the tree reports the producer of the file that
	// is no longer there.
	t.Run("an archive swapped for another", func(t *testing.T) {
		root := writeTree(t, "darwin_arm64",
			Record{Path: "runtime", Producer: Producer{NanogoTool, "3fbcea1"}},
			Record{Path: "math/bits", Producer: gc})
		dir := filepath.Join(root, "pkg", "darwin_arm64")
		b, err := os.ReadFile(filepath.Join(dir, "math", "bits.a"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "runtime.a"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = TallyTree(root, "darwin_arm64")
		if err == nil || !strings.Contains(err.Error(), "does not match its "+ManifestFile+" record") {
			t.Fatalf("TallyTree = %v, want an error about the checksum", err)
		}
	})

	t.Run("no manifest", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "pkg", "darwin_arm64"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := TallyTree(root, "darwin_arm64"); err == nil {
			t.Fatal("a pkg directory with no manifest was accepted")
		}
	})

	t.Run("no tree", func(t *testing.T) {
		if _, err := TallyLine(t.TempDir(), "darwin_arm64"); err == nil {
			t.Fatal("a directory with no pkg tree was accepted")
		}
	})
}
