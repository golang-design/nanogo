// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"

	"golang.design/x/nanogo/types2"
)

// ImportError reports that nanogo cannot read the export data of a package an
// allowlisted package imports.
//
// The error is its own type, and it says why rather than "not found", because
// the reason decides what the reader does next. A missing -importcfg entry is
// a build the go command constructed wrongly. An entry that is present and
// unreadable is the missing half of specs/015-export-data.md, which is a
// component of the compiler and not a mistake in the build.
type ImportError struct {
	// Path is the import path as written in the source.
	Path string

	// File is the archive -importcfg names for it, empty when the
	// configuration has no entry.
	File string

	// Package is the package being compiled, so that a build that compiles
	// many packages says which one stopped.
	Package string
}

func (e *ImportError) Error() string {
	if e.File == "" {
		return fmt.Sprintf("%s: cannot import %q: -importcfg has no entry for it",
			e.Package, e.Path)
	}
	return fmt.Sprintf("%s: cannot import %q from %s: nanogo has no reader for gc's export data",
		e.Package, e.Path, e.File)
}

// noExportData is the [types2.Importer] nanogo has today: one that refuses,
// precisely.
//
// gc writes its export data in the unified format carried by
// internal/pkgbits, and reading it needs the container, the declaration
// decoder and the body decoder. specs/015-export-data.md sizes that work at
// 6,000 to 8,000 lines and it is unbuilt, so nanogo compiles a package that
// imports nothing and says so at the first import rather than at the first
// name the import defines.
//
// The type also carries the -importcfg lookup, so a build whose configuration
// is missing an entry is told that instead. The two failures need different
// fixes and a single message would send the reader to the wrong one.
type noExportData struct {
	cfg *Config
}

// Import implements [types2.Importer].
func (n *noExportData) Import(path string) (*types2.Package, error) {
	pkg := ""
	var cfg *ImportCfg
	if n.cfg != nil {
		pkg = n.cfg.Package
		cfg = n.cfg.ImportCfg
	}
	// The importmap directives rename a path before it is looked up, which is
	// how the go command expresses vendoring and a package's test variant.
	file, _ := cfg.PackageFile(cfg.Resolve(path))
	return nil, &ImportError{Path: path, File: file, Package: pkg}
}
