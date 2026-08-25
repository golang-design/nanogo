// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestIncludeSourceStatesTheRule(t *testing.T) {
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{".", true, true},
		{"internal", true, true},
		{"internal/abi/abi.go", false, true},
		{"runtime/asm_arm64.s", false, true},
		{"runtime/textflag.h", false, true},
		{"go.mod", false, true},
		{"go.sum", false, true},
		// cmd is a separate module holding the Go toolchain's own source. A
		// nanogo distribution carries the standard library.
		{"cmd", true, false},
		{"cmd/compile/main.go", false, false},
		// Nothing in the tree compiles a test.
		{"strconv/atoi_test.go", false, false},
		{"go/build/testdata", true, false},
		{"go/build/testdata/other.go", false, false},
		// A directory called testdata deeper in is dropped too.
		{"a/b/testdata", true, false},
		// The bootstrap scripts drive Go's own build and only exist at the
		// top. A package called make.bash is not a thing, but a file called
		// crypto/rand.bash would be, so the rule is anchored.
		{"make.bash", false, false},
		{"all.bat", false, false},
		{"clean.rc", false, false},
		{"crypto/keep.bash", false, true},
	}
	for _, c := range cases {
		if got := IncludeSource(c.rel, c.isDir); got != c.want {
			t.Errorf("IncludeSource(%q, %v) = %v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

// fakeGoroot lays out the parts of a GOROOT that CopySource reads.
func fakeGoroot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"LICENSE":                        "Go's licence\n",
		"PATENTS":                        "Go's patent grant\n",
		"src/internal/abi/abi.go":        "package abi\n",
		"src/internal/abi/abi_test.go":   "package abi\n",
		"src/internal/abi/testdata/x.go": "package x\n",
		"src/runtime/asm_arm64.s":        "TEXT ·f(SB),0,$0\n",
		"src/make.bash":                  "#!/bin/bash\n",
		"src/cmd/compile/main.go":        "package main\n",
		"src/go.mod":                     "module std\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCopySourceCarriesTheSourcesAndTheirLicence(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "src")
	n, err := CopySource(fakeGoroot(t), dst)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	err = filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	// LICENSE and PATENTS sit beside the sources they govern. This is a
	// redistribution and the path has to make that unambiguous.
	want := []string{"LICENSE", "PATENTS", "go.mod", "internal/abi/abi.go", "runtime/asm_arm64.s"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("copied\n\t%v\nwant\n\t%v", got, want)
	}
	if n != len(want) {
		t.Errorf("CopySource reported %d files and wrote %d", n, len(want))
	}
}

func TestCopySourceFailsWithoutAGoroot(t *testing.T) {
	if _, err := CopySource(t.TempDir(), filepath.Join(t.TempDir(), "src")); err == nil {
		t.Error("a directory with no src was accepted")
	}
	// A GOROOT with sources and no licence is not one this may redistribute.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CopySourceErr(t, root)
	if err == nil || !strings.Contains(err.Error(), "must travel with them") {
		t.Fatalf("CopySource = %v, want an error about the missing licence", err)
	}
}

// CopySourceErr runs CopySource into a fresh directory and returns the error.
func CopySourceErr(t *testing.T, goroot string) error {
	t.Helper()
	_, err := CopySource(goroot, filepath.Join(t.TempDir(), "src"))
	return err
}
