// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"os"
	"path/filepath"
	"sort"
)

// CorpusDir is the vendored corpus, relative to this package's directory.
//
// The files are the Go Authors' work, redistributed under Go's licence. See
// testdata/go/README.md for the provenance and for why they are vendored
// rather than read from the installed toolchain.
const CorpusDir = "testdata/go/test"

// A File is one corpus file: its name, its bytes, and what its header says to
// do with it.
//
// Err is set when the header could not be read. Such a file is still returned,
// because it still counts towards the corpus size, and a file that vanished
// from the totals is the failure mode this package is written against.
type File struct {
	Name   string // base name, such as "helloworld.go"
	Path   string // full path on disk
	Src    []byte
	Header Header
	Err    error
}

// ReadCorpus reads every .go file in dir, in name order.
//
// The order is the sort order of the names, so a run visits the same files in
// the same sequence on every machine (specs/053-determinism.md). Nothing here
// filters: a file the host platform excludes, or one with no recipe, is
// returned like any other and classified by the caller. Filtering here would
// shrink the denominator invisibly.
func ReadCorpus(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	files := make([]File, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f := File{Name: name, Path: path, Src: src}
		f.Header, f.Err = ParseHeader(src)
		files = append(files, f)
	}
	return files, nil
}
