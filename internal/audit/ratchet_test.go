// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A report standing in for a sweep, so that the ratchet is tested in
// milliseconds rather than behind a two minute corpus run.
func fakeReport() *Report {
	return &Report{Verdicts: []Verdict{
		{Probe: "a", Class: ClassOK},
		{Probe: "b", Class: ClassRefused},
		{Probe: "c", Class: ClassWrong},
	}}
}

func readBack(t *testing.T, rt *Ratchet) *Ratchet {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ratchet.txt")
	if err := WriteRatchet(path, rt); err != nil {
		t.Fatalf("WriteRatchet: %v", err)
	}
	got, err := ReadRatchet(path)
	if err != nil {
		t.Fatalf("ReadRatchet: %v", err)
	}
	return got
}

// A ratchet written and read back must be the ratchet that was written, every
// class included. A refusal is recorded here, unlike internal/gotest, because
// the previous class is the only thing that makes a lifted refusal visible.
func TestRatchetRoundTrip(t *testing.T) {
	want := FromReport(fakeReport())
	got := readBack(t, want)
	if got.Probes != want.Probes || got.Probes != 3 {
		t.Errorf("probes: got %d, want %d", got.Probes, want.Probes)
	}
	for k, v := range want.Class {
		if got.Class[k] != v {
			t.Errorf("probe %q: got %q, want %q", k, got.Class[k], v)
		}
	}
	if got.Class["b"] != ClassRefused {
		t.Error("a refusal was not recorded, so a lifted refusal could not be seen")
	}
	if got.Class["c"] != ClassWrong {
		t.Error("a wrong answer was not recorded, so it was hidden rather than pinned")
	}
}

// Writing twice must produce the same bytes (specs/053-determinism.md), sorted
// by probe name so a refresh produces a readable diff.
func TestRatchetIsDeterministicAndSorted(t *testing.T) {
	dir := t.TempDir()
	rt := FromReport(&Report{Verdicts: []Verdict{
		{Probe: "zeta", Class: ClassOK},
		{Probe: "alpha", Class: ClassRefused},
		{Probe: "mu", Class: ClassOK},
	}})
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
	var names []string
	for _, line := range strings.Split(out[0], "\n") {
		if strings.HasPrefix(line, "probe ") {
			names = append(names, strings.Fields(line)[2])
		}
	}
	if len(names) != 3 {
		t.Fatalf("wrote %d probe lines, want 3", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("the probe lines are not sorted: %q then %q", names[i-1], names[i])
		}
	}
}

// The header is written on every refresh, so it cannot drift away from the
// format below it. It has to carry the rule and the coupling, because the file
// is what a contributor reads first when the gate goes red.
func TestRatchetHeaderStatesTheRuleAndTheCoupling(t *testing.T) {
	text := readFile(t, func() string {
		path := filepath.Join(t.TempDir(), "r.txt")
		if err := WriteRatchet(path, FromReport(fakeReport())); err != nil {
			t.Fatal(err)
		}
		return path
	}())
	for _, want := range []string{
		"NANOGO_REFRESH_RATCHET=1", // how to refresh it
		"read the diff before",     // and that the diff is the review
		"probes N",                 // the format
		"probe CLASS NAME",         //
		"ok is best",               // the total order
		"fails the build",          // what a fall does
		"reported loudly",          // and what a rise does
		"README.md",                // the documents a rise makes stale
		"doc.go",                   //
		"driver/help.go",           //
		"driver/help_test.go",      // the coupling that keeps them honest
		"go:embed",                 // and the three phrases it pins
		"name offset out of range", //
		"wider than four machine",  //
		"buildinfo-named",          // the three wrong rows, named
		"embed-directive",          //
		"panic-fires",              //
		"specs/053-determinism.md", // why it is sorted
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the ratchet header does not mention %q", want)
		}
	}
}

// A class that falls fails the build, whatever the two classes are.
func TestRatchetSeesWhatWentBackwards(t *testing.T) {
	rt := FromReport(fakeReport())

	for _, tc := range []struct {
		name  string
		probe string
		to    Class
	}{
		{"an ok that is refused now", "a", ClassRefused},
		{"an ok that is wrong now", "a", ClassWrong},
		// The transition the task's wording does not name and
		// CONTRIBUTING.md settles: a refusal that starts producing a
		// wrong answer is the worst outcome in the corpus.
		{"a refusal that is wrong now", "b", ClassWrong},
		{"a probe whose oracle broke", "a", ClassBroken},
	} {
		worse := fakeReport()
		for i := range worse.Verdicts {
			if worse.Verdicts[i].Probe == tc.probe {
				worse.Verdicts[i].Class = tc.to
			}
		}
		got := rt.Regressions(worse)
		if len(got) != 1 || !strings.Contains(got[0], tc.probe) {
			t.Errorf("%s was not reported as a regression: %v", tc.name, got)
		}
	}

	// A probe the sweep did not read at all, which is the shape a deleted
	// probe directory takes.
	gone := &Report{Verdicts: fakeReport().Verdicts[:1]}
	got := rt.Regressions(gone)
	if len(got) != 2 {
		t.Fatalf("probes that vanished from the sweep were not reported: %v", got)
	}
	if !strings.Contains(got[0], "did not read it at all") {
		t.Errorf("the message does not say the probe was never read: %q", got[0])
	}

	if r := rt.Regressions(fakeReport()); len(r) != 0 {
		t.Errorf("an unchanged run reported regressions: %v", r)
	}
}

// A class that rises is reported and does not fail. That is the case this gate
// exists for: it is the moment a documented limitation becomes a stale claim.
func TestRatchetSeesGrowthAndSaysSoWithoutFailing(t *testing.T) {
	rt := FromReport(fakeReport())
	for _, to := range []Class{ClassOK, ClassRefused} {
		better := fakeReport()
		better.Verdicts[2].Class = to // c was wrong
		gains := rt.Gains(better)
		if len(gains) != 1 || !strings.Contains(gains[0], "c") {
			t.Errorf("a probe that improved to %q was not reported: %v", to, gains)
		}
		if r := rt.Regressions(better); len(r) != 0 {
			t.Errorf("growth to %q was reported as a regression: %v", to, r)
		}
	}

	// The three probes named as the ones to watch when method sets land on
	// ir.Type. A refusal turning into an OK must read as progress.
	rt = &Ratchet{Probes: 3, Class: map[string]Class{
		"make-byte-slice": ClassRefused, "make-struct-slice": ClassRefused, "struct-ptr-field": ClassRefused,
	}}
	landed := &Report{Verdicts: []Verdict{
		{Probe: "make-byte-slice", Class: ClassOK},
		{Probe: "make-struct-slice", Class: ClassOK},
		{Probe: "struct-ptr-field", Class: ClassOK},
	}}
	if r := rt.Regressions(landed); len(r) != 0 {
		t.Errorf("three lifted refusals failed the build: %v", r)
	}
	if g := rt.Gains(landed); len(g) != 3 {
		t.Errorf("three lifted refusals produced %d progress lines, want 3: %v", len(g), g)
	}
}

// The count is the denominator guard. A corpus that shrank would otherwise go
// green having gated less.
func TestRatchetSeesTheDenominatorMove(t *testing.T) {
	rt := FromReport(fakeReport())

	smaller := &Report{Verdicts: fakeReport().Verdicts[:2]}
	got := rt.CountChange(smaller)
	if !strings.Contains(got, "3") || !strings.Contains(got, "2") {
		t.Errorf("the count change does not state both numbers: %q", got)
	}

	bigger := fakeReport()
	bigger.Verdicts = append(bigger.Verdicts, Verdict{Probe: "d", Class: ClassOK})
	if rt.CountChange(bigger) == "" {
		t.Error("a probe added without a refresh did not move the count")
	}
	if g := rt.Gains(bigger); len(g) != 1 || !strings.Contains(g[0], "not recorded") {
		t.Errorf("an unrecorded probe was not named: %v", g)
	}

	if c := rt.CountChange(fakeReport()); c != "" {
		t.Errorf("an unchanged corpus reported a count change: %q", c)
	}
}

func TestRatchetRejectsAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		"probes\n",
		"probes many\n",
		"probe ok\n",
		"nonsense\n",
		// A class no sweep can produce. Left unchecked it would read as
		// rank zero and gate nothing.
		"probes 1\nprobe fine a\n",
		// The count and the lines are two statements of one number.
		"probes 9\nprobe ok a\n",
		// One probe, two classes: which one gates?
		"probes 2\nprobe ok a\nprobe refused a\n",
		// No count at all, so no denominator.
		"probe ok a\n",
	} {
		path := filepath.Join(dir, "bad.txt")
		writeFile(t, path, bad)
		if _, err := ReadRatchet(path); err == nil {
			t.Errorf("%q was accepted", strings.ReplaceAll(strings.TrimSpace(bad), "\n", "; "))
		}
	}
	if _, err := ReadRatchet(filepath.Join(dir, "absent.txt")); err == nil {
		t.Error("a missing ratchet was accepted")
	}

	// Comments and blank lines are not errors.
	path := filepath.Join(dir, "ok.txt")
	writeFile(t, path, "# a note\n\nprobes 2\nprobe ok a\nprobe broken b\n")
	rt, err := ReadRatchet(path)
	if err != nil {
		t.Fatalf("ReadRatchet: %v", err)
	}
	if rt.Probes != 2 || rt.Class["a"] != ClassOK || rt.Class["b"] != ClassBroken {
		t.Errorf("got %+v", rt)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
