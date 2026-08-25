// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// IncludeSource reports whether a path under GOROOT/src belongs in a
// distribution. rel is relative to GOROOT/src, with forward slashes.
//
// The rule is stated here rather than left as folklore in a shell script,
// because a filter nobody can read becomes a filter nobody dares change.
// Three things are dropped and each for a reason a reader can check:
//
//   - cmd, which is a separate module holding the Go toolchain's own source.
//     A nanogo distribution carries the standard library. specs/062 is about
//     nanogo compiling a Go distribution, and that reads a Go checkout.
//   - testdata directories and _test.go files. Nothing in the tree compiles a
//     test, and they are 43% of GOROOT/src.
//   - the build scripts at the top level, which drive Go's own bootstrap and
//     would be run by mistake from a tree that cannot bootstrap anything.
//
// Everything else is copied, including assembly, .h files and go.mod, because
// a package whose non-Go files were dropped is a package that does not build
// and says nothing about why.
func IncludeSource(rel string, isDir bool) bool {
	if rel == "." {
		return true
	}
	first, _, _ := strings.Cut(rel, "/")
	if first == "cmd" {
		return false
	}
	// Any component, not only the last one. The walk in [CopySource] never
	// descends into a testdata directory, so only the directory itself would
	// ever be asked about, but a predicate that answers differently depending
	// on who walks the tree is one nobody can reuse.
	for _, part := range strings.Split(rel, "/") {
		if part == "testdata" {
			return false
		}
	}
	base := path.Base(rel)
	if isDir {
		return true
	}
	if strings.HasSuffix(base, "_test.go") {
		return false
	}
	// The bootstrap scripts, which live only at the top level.
	if !strings.Contains(rel, "/") {
		for _, ext := range []string{".bash", ".bat", ".rc"} {
			if strings.HasSuffix(base, ext) {
				return false
			}
		}
	}
	return true
}

// CopySource copies the standard library sources from goroot into dst and
// reports how many files it wrote.
//
// Go's LICENSE and PATENTS travel with the sources, into the same directory,
// because this is a redistribution and the path has to make it unambiguous
// which files they govern. nanogo's own licence sits at the root of the tree
// and governs everything else.
func CopySource(goroot, dst string) (int, error) {
	src := filepath.Join(goroot, "src")
	n := 0
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !IncludeSource(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		if err := copyFile(p, filepath.Join(dst, filepath.FromSlash(rel)), fileMode); err != nil {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, name := range []string{"LICENSE", "PATENTS"} {
		if err := copyFile(filepath.Join(goroot, name), filepath.Join(dst, name), fileMode); err != nil {
			return 0, fmt.Errorf("the Go sources are redistributed and %s must travel with them: %v", name, err)
		}
		n++
	}
	return n, nil
}

// copyFile writes src to dst with the given mode, creating the directories
// above it. The mode is given rather than copied, for the reason
// [WriteTarGz] fixes the modes it writes.
func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}
