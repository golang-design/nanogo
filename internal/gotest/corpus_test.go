// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/internal/gotest"
)

// ratchetPath is the file that records what the corpus proved.
const ratchetPath = "testdata/ratchet.txt"

// refreshRatchet reports whether this run rewrites the ratchet rather than
// checking against it.
func refreshRatchet() bool { return os.Getenv("NANOGO_REFRESH_RATCHET") == "1" }

// subset is the slice of the corpus an ordinary `go test ./...` runs.
//
// It is small, it is fixed, and between them these files reach every part of
// the harness: a program that runs and agrees with gc, a program nanogo
// refuses, a file the checker must reject, a recipe kind that is not
// implemented, a recipe whose flags cannot be honoured, a file the corpus
// itself says to skip, and the one file with no readable recipe. A subset
// that only held programs nanogo compiles would be a subset that could not
// fail.
//
// The full sweep is TestCorpus, behind NANOGO_REQUIRE_CORPUS.
var subset = []string{
	"helloworld.go", // runs, and agrees with gc
	"empty.go",      // compiles
	"for.go",        // runs too, over a construct helloworld has none of
	"map.go",        // and again, over the complex rows
	"turing.go",     // refused
	"range4.go",     // refused, for a different reason
	"undef.go",      // errorcheck, rejected
	"escape2.go",    // errorcheck whose flags nanogo has no equivalent of
	"bom.go",        // runoutput, a kind this harness does not carry out
	"index.go",      // the corpus itself says to skip it
	"linkmain.go",   // no readable recipe
}

func subsetSet() map[string]bool {
	m := make(map[string]bool, len(subset))
	for _, n := range subset {
		m[n] = true
	}
	return m
}

// TestCorpusSubset is the unattended run. It keeps `go test ./...` fast and
// still fails when the harness breaks.
func TestCorpusSubset(t *testing.T) {
	rep := sweep(t, subsetSet())

	if rep.Files != len(subset) {
		t.Fatalf("the sweep read %d files and the subset names %d; a name in the subset no longer exists in the corpus", rep.Files, len(subset))
	}
	reportFailures(t, rep)

	// A subset that compiled nothing would pass every check above while
	// proving nothing at all, which is the failure this whole package is
	// written against.
	counts := rep.ByClass()
	if counts[gotest.ClassMatched] == 0 {
		t.Error("no file in the subset ran and agreed with gc, so the differential proved nothing")
	}
	if counts[gotest.ClassRefused] == 0 {
		t.Error("no file in the subset was refused; the subset is meant to exercise the refusal path too")
	}
}

// TestCorpus is the full sweep: every file in the corpus, doing what its
// header says.
//
// Gated behind NANOGO_REQUIRE_CORPUS the way the other corpora are, because it
// builds and runs several hundred programs with two compilers. The measured
// wall clock on an M-series laptop is under a minute with a warm build cache
// and about two minutes cold, so CI runs the whole thing rather than a sample.
func TestCorpus(t *testing.T) {
	if !requireCorpus() {
		t.Skip("the full corpus sweep is slow; set NANOGO_REQUIRE_CORPUS=1")
	}
	rep := sweep(t, nil)
	reportFailures(t, rep)
	checkRatchet(t, rep)
}

// reportFailures fails the build for every miscompilation, in full.
//
// A file nanogo refuses is not a failure and never becomes one. A file nanogo
// compiled into a program that behaves differently from gc's build of the same
// source is a miscompilation, and it is the most valuable thing this corpus
// can find, so all of it is printed rather than a count.
func reportFailures(t *testing.T, rep *gotest.Report) {
	t.Helper()
	for _, v := range rep.Failures() {
		t.Errorf("MISCOMPILATION in %s (%s)\n%s", v.File, v.Kind, v.Detail)
	}
}

// checkRatchet compares the run with what was recorded, and fails on anything
// that went backwards.
func checkRatchet(t *testing.T, rep *gotest.Report) {
	t.Helper()
	if refreshRatchet() {
		if err := gotest.WriteRatchet(ratchetPath, gotest.FromReport(rep)); err != nil {
			t.Fatalf("writing the ratchet: %v", err)
		}
		t.Logf("wrote %s; read the diff before committing", ratchetPath)
		return
	}

	rt, err := gotest.ReadRatchet(ratchetPath)
	if err != nil {
		t.Fatalf("reading the ratchet: %v", err)
	}

	// The census first. Every other number in the report is read against a
	// denominator, and a denominator that moved makes them all suspect.
	for _, c := range rt.CensusChanges(rep) {
		t.Errorf("the corpus census moved: %s\n\tEvery count in this report is measured against it. "+
			"Either the vendored corpus changed or the header reader did.", c)
	}
	for _, r := range rt.Regressions(rep) {
		t.Errorf("REGRESSION %s", r)
	}
	if gains := rt.Gains(rep); len(gains) > 0 {
		t.Logf("%d files pass that the ratchet does not record:\n\t%s\n"+
			"Growth is expected and does not fail this test. Refresh the ratchet with "+
			"NANOGO_REFRESH_RATCHET=1 so that it guards them from tomorrow.",
			len(gains), strings.Join(gains, "\n\t"))
	}
}

// The vendored corpus is the Go Authors' work, redistributed under Go's
// licence. This test is the gate on that claim: it fails if a file in the
// vendored tree ever carries nanogo's header, which would be a false claim of
// authorship, and it fails if the licence that permits the redistribution goes
// missing.
func TestVendoredCorpusIsTheGoAuthorsWork(t *testing.T) {
	root := filepath.Dir(gotest.CorpusDir)
	for _, name := range []string{"LICENSE", "PATENTS", "README.md"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("the vendored corpus is redistributed under Go's licence and %s is not beside it: %v", name, err)
		}
	}

	files, err := gotest.ReadCorpus(gotest.CorpusDir)
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no vendored files, so this test proves nothing")
	}
	for _, f := range files {
		src := string(f.Src)
		if strings.Contains(src, "golang.design") {
			t.Errorf("%s carries nanogo's name; a vendored file the Go Authors wrote must not", f.Name)
		}
		// Four files carry no copyright line at all, upstream included.
		// A file that carries one must say whose it is.
		if strings.Contains(src, "Copyright") && !strings.Contains(src, "The Go Authors") {
			t.Errorf("%s carries a copyright notice that is not the Go Authors'", f.Name)
		}
	}
	t.Logf("%d vendored files, none carrying nanogo's header", len(files))
}
