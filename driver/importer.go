// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/types2"
)

// ImportError reports that nanogo cannot read the export data of a package an
// allowlisted package imports.
//
// The error is its own type, and it says why rather than "not found", because
// the reason decides what the reader does next. A missing -importcfg entry is
// a build the go command constructed wrongly. An entry that is present and
// unreadable is a file that is not the archive the configuration promised, or
// one written by a release nanogo is not pinned to.
type ImportError struct {
	// Path is the import path as written in the source.
	Path string

	// File is the archive -importcfg names for it, empty when the
	// configuration has no entry.
	File string

	// Package is the package being compiled, so that a build that compiles
	// many packages says which one stopped.
	Package string

	// Err is what the reader said about the file. It is nil when there was
	// no file to read.
	Err error
}

func (e *ImportError) Error() string {
	if e.File == "" {
		return fmt.Sprintf("%s: cannot import %q: -importcfg has no entry for it",
			e.Package, e.Path)
	}
	return fmt.Sprintf("%s: cannot import %q from %s: %v",
		e.Package, e.Path, e.File, e.Err)
}

// Unwrap gives the reader's own error, so that a caller can test for it.
func (e *ImportError) Unwrap() error { return e.Err }

// importer resolves an import to the package gc's export data describes.
//
// One importer serves one compilation. It holds the [export.Reader], which is
// what makes a package reached through two different archives one package
// (see [export.Reader]), and it holds the configuration, which is what turns
// an import path into a file.
type importer struct {
	cfg    *Config
	reader *export.Reader
}

func newImporter(cfg *Config) *importer {
	return &importer{cfg: cfg, reader: export.NewReader()}
}

// Import implements [types2.Importer].
func (i *importer) Import(path string) (*types2.Package, error) {
	pkg := ""
	var cfg *ImportCfg
	if i.cfg != nil {
		pkg = i.cfg.Package
		cfg = i.cfg.ImportCfg
	}
	// The importmap directives rename a path before it is looked up, which is
	// how the go command expresses vendoring and a package's test variant.
	// The renamed path is also the identity the export data carries, so it is
	// the path the reader is given.
	resolved := cfg.Resolve(path)
	if resolved == "unsafe" {
		return types2.Unsafe, nil
	}
	file, ok := cfg.PackageFile(resolved)
	if !ok {
		return nil, &ImportError{Path: path, Package: pkg}
	}
	p, err := i.reader.Read(resolved, file)
	if err != nil {
		return nil, &ImportError{Path: path, File: file, Package: pkg, Err: err}
	}
	return p, nil
}

// recovered turns a panic raised below the type checker into an error.
//
// A declaration in export data is decoded when the checker first looks it up,
// which is after [importer.Import] returned, so its failure has no error
// channel to travel back through. The reader signals it by panicking, and the
// panic is converted here because this is the last frame that still knows
// which package the build asked nanogo to compile.
//
// The message says only what this frame knows: the checker panicked on this
// package, and here is what it panicked with. It does not say the export data
// is at fault, because a bug anywhere under [types2.Config.Check] arrives the
// same way, and naming a cause that has not been established would send a
// reader to the wrong file. The reader's own panics identify themselves, so a
// failure that is the export data reads as one.
func (i *importer) recovered(v any) error {
	pkg := ""
	if i.cfg != nil {
		pkg = i.cfg.Package
	}
	return fmt.Errorf("%s: the type checker panicked: %v", pkg, v)
}
