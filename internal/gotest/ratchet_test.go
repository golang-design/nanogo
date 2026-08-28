// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"path/filepath"
	"strings"
	"testing"
)

// A report standing in for a sweep, so that the ratchet is tested in
// milliseconds rather than behind a forty-second corpus run. The bug this
// exists to catch is real: the census key for a file with no recipe was two
// words, the ratchet's census line is three fields, and every refresh wrote a
// file no read could parse. Nothing found it until a full sweep ran.
func fakeReport() *Report {
	return &Report{Files: 5, Verdicts: []Verdict{
		{File: "a.go", Kind: "run", Class: ClassMatched},
		{File: "b.go", Kind: "compile", Class: ClassCompiled},
		{File: "c.go", Kind: "errorcheck", Class: ClassRejected},
		{File: "d.go", Kind: "run", Class: ClassRefused, Reason: "maps"},
		{File: "e.go", Kind: "", Class: ClassNoRecipe, Reason: "unknown recipe"},
	}}
}

// A ratchet written and read back must be the ratchet that was written. Every
// key the census can hold has to survive the round trip, the key for a file
// with no recipe included.
func TestRatchetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratchet.txt")
	want := FromReport(fakeReport())
	if err := WriteRatchet(path, want); err != nil {
		t.Fatalf("WriteRatchet: %v", err)
	}
	got, err := ReadRatchet(path)
	if err != nil {
		t.Fatalf("ReadRatchet: %v", err)
	}
	if got.Files != want.Files {
		t.Errorf("files: got %d, want %d", got.Files, want.Files)
	}
	for k, v := range want.Census {
		if got.Census[k] != v {
			t.Errorf("census %q: got %d, want %d", k, got.Census[k], v)
		}
	}
	if len(got.Census) != len(want.Census) {
		t.Errorf("census has %d kinds, want %d", len(got.Census), len(want.Census))
	}
	for k, v := range want.Pass {
		if got.Pass[k] != v {
			t.Errorf("pass %q: got %q, want %q", k, got.Pass[k], v)
		}
	}
	if len(got.Pass) != 3 {
		t.Errorf("recorded %d passes, want 3: a refusal must never be recorded", len(got.Pass))
	}
	if _, ok := got.Pass["d.go"]; ok {
		t.Error("a refused file was recorded as a pass, which would freeze the gap in place")
	}
}

// Writing twice must produce the same bytes (specs/053-determinism.md).
func TestRatchetIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	rt := FromReport(fakeReport())
	var out [2]string
	for i := range out {
		path := filepath.Join(dir, "r"+string(rune('0'+i))+".txt")
		if err := WriteRatchet(path, rt); err != nil {
			t.Fatal(err)
		}
		out[i] = readFile(t, path)
	}
	if out[0] != out[1] {
		t.Error("two writes of one ratchet differ")
	}
	// Sorted, so that a refresh produces a readable diff.
	// Sorted by file name, which is the field a reader looks for and the
	// field a diff must line up on.
	var passes []string
	for _, line := range strings.Split(out[0], "\n") {
		if strings.HasPrefix(line, "pass ") {
			passes = append(passes, strings.Fields(line)[2])
		}
	}
	for i := 1; i < len(passes); i++ {
		if passes[i-1] >= passes[i] {
			t.Errorf("the pass lines are not sorted: %q then %q", passes[i-1], passes[i])
		}
	}
}

func TestRatchetSeesWhatWentBackwards(t *testing.T) {
	rt := FromReport(fakeReport())

	// a.go stops running and is only refused now.
	worse := fakeReport()
	worse.Verdicts[0] = Verdict{File: "a.go", Kind: "run", Class: ClassRefused, Reason: "closures"}
	got := rt.Regressions(worse)
	if len(got) != 1 || !strings.Contains(got[0], "a.go") || !strings.Contains(got[0], "closures") {
		t.Errorf("a file that stopped passing was not reported: %v", got)
	}

	// b.go is no longer read at all, which is the shape a shrinking
	// denominator takes.
	gone := fakeReport()
	gone.Verdicts = gone.Verdicts[:1]
	gone.Files = 1
	if g := rt.Regressions(gone); len(g) != 2 {
		t.Errorf("files that vanished from the sweep were not reported: %v", g)
	}

	// A pass that changed to a different passing class is still a claim
	// withdrawn: matched means the program ran, compiled does not.
	weaker := fakeReport()
	weaker.Verdicts[0] = Verdict{File: "a.go", Kind: "run", Class: ClassCompiled}
	if g := rt.Regressions(weaker); len(g) != 1 {
		t.Errorf("a pass that weakened was not reported: %v", g)
	}

	// An oracle failure is not a regression. gc could not build or run the
	// file, so the sweep has no expectation to compare nanogo against and
	// measured nothing. Under the coverage pass, which runs every package of
	// this repository at once, a corpus program's own build has run past its
	// budget, and reporting that as a file that stopped passing puts a red
	// gate on machine load.
	noOracle := fakeReport()
	noOracle.Verdicts[0] = Verdict{File: "a.go", Kind: "run", Class: ClassOracleFailed,
		Reason: "gc's build of the program did not finish"}
	if g := rt.Regressions(noOracle); len(g) != 0 {
		t.Errorf("a file gc could not build was reported as a regression: %v", g)
	}

	if g := rt.Regressions(fakeReport()); len(g) != 0 {
		t.Errorf("an unchanged run reported regressions: %v", g)
	}
}

func TestRatchetSeesGrowthAndSaysSoWithoutFailing(t *testing.T) {
	rt := FromReport(fakeReport())
	better := fakeReport()
	better.Verdicts[3] = Verdict{File: "d.go", Kind: "run", Class: ClassMatched}
	gains := rt.Gains(better)
	if len(gains) != 1 || !strings.Contains(gains[0], "d.go") {
		t.Errorf("a newly passing file was not reported: %v", gains)
	}
	if r := rt.Regressions(better); len(r) != 0 {
		t.Errorf("growth was reported as a regression: %v", r)
	}
}

// The census is the denominator guard. A harness that stopped finding files
// would otherwise go green having swept fewer of them.
func TestRatchetSeesTheDenominatorMove(t *testing.T) {
	rt := FromReport(fakeReport())

	smaller := fakeReport()
	smaller.Files = 4
	smaller.Verdicts = smaller.Verdicts[:4]
	changes := rt.CensusChanges(smaller)
	if len(changes) != 2 {
		t.Fatalf("a shrunken corpus produced %d complaints, want 2 (the file count and the kind): %v", len(changes), changes)
	}
	if !strings.Contains(changes[0], "5") || !strings.Contains(changes[0], "4") {
		t.Errorf("the file count change does not state both numbers: %q", changes[0])
	}

	// A kind that appears where none was recorded.
	grown := fakeReport()
	grown.Verdicts = append(grown.Verdicts, Verdict{File: "f.go", Kind: "runoutput", Class: ClassKindNotImplemented})
	grown.Files = 6
	found := false
	for _, c := range grown.ByKind() {
		_ = c
	}
	for _, c := range rt.CensusChanges(grown) {
		if strings.Contains(c, "runoutput") && strings.Contains(c, "not recorded") {
			found = true
		}
	}
	if !found {
		t.Errorf("a new recipe kind was not reported: %v", rt.CensusChanges(grown))
	}

	if c := rt.CensusChanges(fakeReport()); len(c) != 0 {
		t.Errorf("an unchanged corpus reported census changes: %v", c)
	}
}

func TestRatchetRejectsAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"files\n", "files many\n", "census run\n", "census run many\n", "pass run\n", "nonsense\n"} {
		path := filepath.Join(dir, "bad.txt")
		writeFile(t, path, bad)
		if _, err := ReadRatchet(path); err == nil {
			t.Errorf("%q was accepted", strings.TrimSpace(bad))
		}
	}
	if _, err := ReadRatchet(filepath.Join(dir, "absent.txt")); err == nil {
		t.Error("a missing ratchet was accepted")
	}
	// Comments and blank lines are not errors.
	path := filepath.Join(dir, "ok.txt")
	writeFile(t, path, "# a note\n\nfiles 1\ncensus run 1\npass matched a.go\n")
	rt, err := ReadRatchet(path)
	if err != nil {
		t.Fatalf("ReadRatchet: %v", err)
	}
	if rt.Files != 1 || rt.Census["run"] != 1 || rt.Pass["a.go"] != "matched" {
		t.Errorf("got %+v", rt)
	}
}
