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
	Records []Record
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
func (t Tally) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "nanogo: %d of %d packages in the bootstrap closure compiled by nanogo",
		t.Nanogo(), t.Total())
	for _, g := range t.Others() {
		fmt.Fprintf(&b, "; %d by %s", g.Count, g.Producer)
	}
	return b.String()
}

// TallyTree reads every archive under root/pkg/target and reports what they
// say about themselves.
//
// Every archive must carry a record. One that does not fails the whole tally,
// because a distribution that can account for all but one of its packages
// cannot answer the question the tally is asked.
func TallyTree(root, target string) (Tally, error) {
	dir := filepath.Join(root, "pkg", target)
	var t Tally
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != archiveExt {
			return nil
		}
		r, err := ReadRecordFile(path)
		if err != nil {
			return err
		}
		// The record names the package and the path in the tree names it
		// again. Two copies of a fact drift, so the copy is checked.
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if want := filepath.ToSlash(strings.TrimSuffix(rel, archiveExt)); want != r.Path {
			return fmt.Errorf("%s: the archive sits at %s and its record says %s", path, want, r.Path)
		}
		t.Records = append(t.Records, r)
		return nil
	})
	if err != nil {
		return Tally{}, err
	}
	if len(t.Records) == 0 {
		return Tally{}, fmt.Errorf("%s holds no archives, so the tree can link nothing", dir)
	}
	// WalkDir is lexical, which is the sorted order already. Sorting again is
	// the cheap way to keep that true if the walk is ever replaced.
	sort.Slice(t.Records, func(i, j int) bool { return t.Records[i].Path < t.Records[j].Path })
	return t, nil
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
