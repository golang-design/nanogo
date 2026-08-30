// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package selfhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheRatchetRoundTrips checks that what is written is what is read.
func TestTheRatchetRoundTrips(t *testing.T) {
	rs := []Result{
		{Path: "b", Decision: Compiled},
		{Path: "a", Decision: Compiled},
		{Path: "c", Decision: Failed, Reason: "nanogo cannot compile x"},
	}
	path := filepath.Join(t.TempDir(), "ratchet.txt")
	if err := WriteRatchet(path, FromResults(rs)); err != nil {
		t.Fatal(err)
	}
	rt, err := ReadRatchet(path)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Packages != 3 {
		t.Errorf("the count is %d, want 3", rt.Packages)
	}
	if !rt.Compiles["a"] || !rt.Compiles["b"] {
		t.Errorf("the compiled set is %v, want a and b", rt.Compiles)
	}
	// A refusal is never recorded. Recording one would freeze a gap in place
	// and call it progress.
	if rt.Compiles["c"] {
		t.Error("the ratchet recorded a package nanogo refused")
	}
}

// TestTheRatchetIsSorted checks the file is stable across refreshes, so a
// refresh produces a diff and not a reshuffle (specs/053-determinism.md).
func TestTheRatchetIsSorted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratchet.txt")
	if err := WriteRatchet(path, FromResults([]Result{
		{Path: "z", Decision: Compiled},
		{Path: "m", Decision: Compiled},
		{Path: "a", Decision: Compiled},
	})); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "compiles "); ok {
			got = append(got, rest)
		}
	}
	if strings.Join(got, ",") != "a,m,z" {
		t.Errorf("the file lists %v, want them sorted", got)
	}
	// The header explains the file to whoever opens it first, and it is
	// written on every refresh so it cannot drift from the format below it.
	for _, want := range []string{"NANOGO_REFRESH_RATCHET=1", "specs/060-selfhost.md", "packages N", "compiles PATH"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the header does not mention %q", want)
		}
	}
}

// TestTheRatchetReportsARegression is the gate this file exists for.
//
// A package that compiled and no longer does is the failure nothing else in
// the repository can see, and the message has to name what nanogo said instead
// so the reader does not have to re-run the measurement to act on it.
func TestTheRatchetReportsARegression(t *testing.T) {
	rt := &Ratchet{Packages: 3, Compiles: map[string]bool{"a": true, "b": true, "gone": true}}
	got := rt.Regressions([]Result{
		{Path: "a", Decision: Compiled},
		{Path: "b", Decision: Failed, Reason: "nanogo cannot compile a variadic wrapper"},
		{Path: "c", Decision: Compiled},
	})
	if len(got) != 2 {
		t.Fatalf("the regressions are %v, want one for b and one for gone", got)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "nanogo cannot compile a variadic wrapper") {
		t.Errorf("the regression does not carry what nanogo said:\n%s", joined)
	}
	// A package the ratchet records and this run did not measure is a
	// regression too. It is the shape a narrowed package list takes, and a
	// silent one, because the count alone would still be checked separately.
	if !strings.Contains(joined, "gone") {
		t.Errorf("an unmeasured package is not reported:\n%s", joined)
	}
}

// TestTheRatchetReportsADecisionWithNoReason covers a delegated package, which
// carries no message of its own.
func TestTheRatchetReportsADecisionWithNoReason(t *testing.T) {
	rt := &Ratchet{Packages: 1, Compiles: map[string]bool{"a": true}}
	got := rt.Regressions([]Result{{Path: "a", Decision: NotReached}})
	if len(got) != 1 || !strings.Contains(got[0], string(NotReached)) {
		t.Errorf("the regressions are %v, want one naming %q", got, NotReached)
	}
}

// TestGrowthIsNotAFailure checks that a package which newly compiles is
// reported and does not fail.
//
// nanogo is expected to compile more of its own source every week, and a gate
// that failed on improvement is a gate people route around.
func TestGrowthIsNotAFailure(t *testing.T) {
	rt := &Ratchet{Packages: 2, Compiles: map[string]bool{"a": true}}
	rs := []Result{{Path: "a", Decision: Compiled}, {Path: "b", Decision: Compiled}}
	if got := rt.Regressions(rs); len(got) != 0 {
		t.Errorf("growth was reported as a regression: %v", got)
	}
	if got := rt.Gains(rs); len(got) != 1 || got[0] != "b" {
		t.Errorf("the gains are %v, want [b]", got)
	}
}

// TestTheCountIsCheckedOnItsOwn checks the denominator.
//
// Every number read against this file has the package count under it, so a
// count that moved makes them all suspect and is reported separately from the
// regressions.
func TestTheCountIsCheckedOnItsOwn(t *testing.T) {
	rt := &Ratchet{Packages: 19, Compiles: map[string]bool{}}
	recorded, measured, changed := rt.CountChanged(make([]Result, 18))
	if !changed || recorded != 19 || measured != 18 {
		t.Errorf("CountChanged = %d, %d, %v; want 19, 18, true", recorded, measured, changed)
	}
	if _, _, changed := rt.CountChanged(make([]Result, 19)); changed {
		t.Error("an unchanged count was reported as changed")
	}
}

// TestReadRatchetRefusesALineItCannotRead is the silent-drop guard.
//
// A reader that skipped the lines it did not understand would report no
// regression for the packages it dropped, which is the failure this file
// exists to prevent, arriving through the file itself.
func TestReadRatchetRefusesALineItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"an unknown kind", "packages 1\nsomething else\n", "not a kind"},
		{"a bare word", "packages 1\nlonely\n", "not a ratchet line"},
		{"a count that is not a number", "packages many\n", "not a number"},
		{"no count at all", "compiles a\n", "no package count"},
	} {
		path := filepath.Join(t.TempDir(), "ratchet.txt")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ReadRatchet(path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the error is %v, want one saying %q", tc.name, err, tc.want)
		}
	}
}

// TestReadRatchetKeepsCommentsAndBlankLines checks the header does not have to
// be stripped before the file can be read.
func TestReadRatchetKeepsCommentsAndBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratchet.txt")
	if err := os.WriteFile(path, []byte("# a comment\n\npackages 2\n\ncompiles a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := ReadRatchet(path)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Packages != 2 || !rt.Compiles["a"] || len(rt.Compiles) != 1 {
		t.Errorf("read %d packages and %v", rt.Packages, rt.Compiles)
	}
}

// TestReadRatchetReportsAMissingFile checks the error reaches the caller
// rather than reading as an empty ratchet, which would agree with any run.
func TestReadRatchetReportsAMissingFile(t *testing.T) {
	if _, err := ReadRatchet(filepath.Join(t.TempDir(), "absent.txt")); err == nil {
		t.Error("a missing ratchet read as an empty one")
	}
}
