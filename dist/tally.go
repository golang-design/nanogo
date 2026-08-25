// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// archiveExt is the extension of a compiled package in pkg.
const archiveExt = ".a"

// A Tally is what a tree holds, package by package, with the producer each
// archive names.
type Tally struct {
	// Records are the archives found, sorted by import path.
	Records Manifest
}

// Total is the number of archives.
func (t Tally) Total() int { return len(t.Records) }

// Nanogo is the number nanogo compiled.
func (t Tally) Nanogo() int {
	n := 0
	for _, r := range t.Records {
		if r.Producer.IsNanogo() {
			n++
		}
	}
	return n
}

// Others counts the archives nanogo did not compile, grouped by producer and
// ordered by producer name.
//
// A slice and not a map. specs/053-determinism.md forbids ranging over a map
// on a path that produces output, and this path produces the line a user
// reads.
func (t Tally) Others() []struct {
	Producer Producer
	Count    int
} {
	type group = struct {
		Producer Producer
		Count    int
	}
	index := make(map[Producer]int)
	var out []group
	for _, r := range t.Records {
		if r.Producer.IsNanogo() {
			continue
		}
		if i, ok := index[r.Producer]; ok {
			out[i].Count++
			continue
		}
		index[r.Producer] = len(out)
		out = append(out, group{Producer: r.Producer, Count: 1})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Producer.String() < out[j].Producer.String() })
	return out
}

// Line is the one sentence a user needs about a tree.
//
// It names the denominator, so that a number cannot be read as a fraction of
// something else, and it names every producer that is not nanogo, so that a
// tree of gc-compiled archives cannot be read as nanogo's work.
//
// "in this distribution" and not "in the bootstrap closure". The two are the
// same set today, and this function counts what is in pkg and has no way to
// know whether that is still the closure. A sentence describing a set it did
// not measure is the same fault as a tally nobody checks, one sentence
// smaller. specs/054-distribution.md carries the closure count.
func (t Tally) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "nanogo: %d of %d packages in this distribution compiled by nanogo",
		t.Nanogo(), t.Total())
	for _, g := range t.Others() {
		fmt.Fprintf(&b, "; %d by %s", g.Count, g.Producer)
	}
	return b.String()
}

// TallyTree reads a tree's manifest and checks it against the archives.
//
// Three things are required and each closes a way the tree could lie:
//
//  1. Every archive under pkg/target has a manifest record. An archive that
//     appeared from somewhere else is not counted as anything, it fails the
//     whole tally, because a distribution that can account for all but one of
//     its packages cannot answer the question the tally is asked.
//  2. Every record names an archive that is there.
//  3. Every archive's SHA-256 matches its record. This is what makes the
//     record a statement about the bytes rather than about what somebody
//     expected the tree to hold. An archive swapped for another is caught
//     here, which is the accidental version of the fault, and the one that
//     actually happens.
func TallyTree(root, target string) (Tally, error) {
	m, err := ReadManifest(root, target)
	if err != nil {
		return Tally{}, err
	}
	dir := filepath.Join(root, "pkg", target)
	byPath := make(map[string]Record, len(m))
	for _, r := range m {
		byPath[r.Path] = r
	}
	found := make(map[string]bool, len(m))
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != archiveExt {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		p := filepath.ToSlash(strings.TrimSuffix(rel, archiveExt))
		r, ok := byPath[p]
		if !ok {
			return fmt.Errorf("%s is in the tree and not in %s, so nothing says which compiler produced it", path, ManifestFile)
		}
		sum, err := sumFile(path)
		if err != nil {
			return err
		}
		if sum != r.Sum {
			return fmt.Errorf("%s does not match its %s record: the file is %s and the record is %s", path, ManifestFile, sum, r.Sum)
		}
		found[p] = true
		return nil
	})
	if err != nil {
		return Tally{}, err
	}
	// The manifest is sorted, so this reports the first missing archive by
	// import path rather than by whatever order a map gave.
	for _, r := range m {
		if !found[r.Path] {
			return Tally{}, fmt.Errorf("%s lists %s and pkg/%s does not hold it", ManifestFile, r.Path, target)
		}
	}
	return Tally{Records: m}, nil
}

// TallyLine is [Tally.Line] for the tree at root.
//
// It is the whole surface a caller outside this package needs to answer "what
// did nanogo actually compile in this tree", and it is deliberately one
// function so that wiring it into a command is one line.
func TallyLine(root, target string) (string, error) {
	t, err := TallyTree(root, target)
	if err != nil {
		return "", err
	}
	return t.Line(), nil
}
