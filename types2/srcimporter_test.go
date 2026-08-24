// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package types2_test

import (
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"sync"

	"golang.design/x/nanogo/syntax"
	. "golang.design/x/nanogo/types2"
)

// srcImporter type-checks an imported package from its source.
//
// Upstream's tests import through cmd/compile/internal/importer, which reads
// gc export data. nanogo has no export data yet (specs/015-export-data.md), so
// the importer reads source instead, with nanogo's own parser and this checker.
//
// That is a heavier importer than upstream's, and it is also a better test:
// importing fmt this way type-checks fmt, which is the conformance job in
// specs/004-conformance.md done one package at a time.
type srcImporter struct {
	mu       sync.Mutex
	fset     *syntax.FileSet
	packages map[string]*Package
	busy     map[string]bool
}

func newSrcImporter(fset *syntax.FileSet) *srcImporter {
	return &srcImporter{
		fset:     fset,
		packages: make(map[string]*Package),
		busy:     make(map[string]bool),
	}
}

// defaultImporter returns the importer the ported tests use by default.
func defaultImporter() Importer { return newSrcImporter(testFset) }

func (imp *srcImporter) Import(path string) (*Package, error) {
	return imp.ImportFrom(path, "", 0)
}

func (imp *srcImporter) ImportFrom(path, dir string, _ ImportMode) (*Package, error) {
	if path == "unsafe" {
		return Unsafe, nil
	}

	imp.mu.Lock()
	if pkg, ok := imp.packages[path]; ok {
		imp.mu.Unlock()
		return pkg, nil
	}
	if imp.busy[path] {
		imp.mu.Unlock()
		return nil, fmt.Errorf("import cycle through %q", path)
	}
	imp.busy[path] = true
	imp.mu.Unlock()

	pkg, err := imp.load(path, dir)

	imp.mu.Lock()
	delete(imp.busy, path)
	if err == nil {
		imp.packages[path] = pkg
	}
	imp.mu.Unlock()
	return pkg, err
}

// srcContext is the build context the source importer resolves packages in.
//
// CgoEnabled is false, and that is not a convenience. build.Default follows the
// ambient CGO_ENABLED, so on a machine where cgo is on, `net` resolves to its
// cgo files and cannot be type-checked from source at all, while on a machine
// where cgo is off the same import succeeds. A test whose corpus depends on the
// host's cgo setting passes locally and fails on CI, which is exactly what it
// did.
//
// Disabling cgo is also nanogo's own configuration, by specs/000-decisions.md
// decision 8, so this context is the one the compiler will actually use rather
// than a setting chosen to make a test pass.
func srcContext() build.Context {
	ctxt := build.Default
	ctxt.CgoEnabled = false
	return ctxt
}

func (imp *srcImporter) load(path, dir string) (*Package, error) {
	ctxt := srcContext()
	bp, err := ctxt.Import(path, dir, 0)
	if err != nil {
		return nil, err
	}
	if len(bp.CgoFiles) > 0 {
		// Unreachable while srcContext disables cgo, and kept because the
		// alternative to an explicit refusal is a package that type-checks
		// with its cgo declarations missing.
		return nil, fmt.Errorf("cannot import %q from source: it uses cgo", path)
	}
	if len(bp.GoFiles) == 0 {
		return nil, fmt.Errorf("no Go files in %s", bp.Dir)
	}

	var files []*syntax.File
	for _, name := range bp.GoFiles {
		filename := filepath.Join(bp.Dir, name)
		src, err := os.ReadFile(filename)
		if err != nil {
			return nil, err
		}
		imp.mu.Lock()
		f := imp.fset.AddFile(filename, len(src))
		imp.mu.Unlock()
		file, err := syntax.Parse(f, src, nil, nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}

	var firstErr error
	conf := Config{
		Fset:     imp.fset,
		Importer: imp,
		Error: func(err error) {
			if firstErr == nil {
				firstErr = err
			}
		},
	}
	pkg, err := conf.Check(bp.ImportPath, files, nil)
	if firstErr != nil {
		return nil, firstErr
	}
	if err != nil {
		return nil, err
	}
	return pkg, nil
}
