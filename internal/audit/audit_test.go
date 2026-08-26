// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCorpus builds a corpus out of shell scripts, so that every class the
// harness can reach is reachable in milliseconds and without a compiler.
//
// Each probe directory holds a main.go, which is what makes it a probe, and up
// to two scripts. nanogo.sh and gc.sh are the programs the two "compilers"
// produce. A missing script is a compiler that refuses.
func fakeCorpus(t *testing.T, probes map[string][2]string) Options {
	t.Helper()
	dir := t.TempDir()
	for name, scripts := range probes {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(p, "main.go"), "package main\n\nfunc main() {}\n")
		for i, which := range []string{"nanogo.sh", "gc.sh"} {
			if scripts[i] == "" {
				continue
			}
			writeFile(t, filepath.Join(p, which), "#!/bin/sh\n"+scripts[i]+"\n")
		}
	}
	// A file that is not a directory, and a directory that is not a probe.
	// Both are in the real corpus: run.sh and go.mod.
	writeFile(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\n")
	if err := os.MkdirAll(filepath.Join(dir, "notaprobe"), 0o700); err != nil {
		t.Fatal(err)
	}

	// The two compilers. Each copies the probe's script for its own name
	// into the output, or fails saying so.
	compiler := func(which string) string {
		bin := filepath.Join(t.TempDir(), which)
		writeFile(t, bin, "#!/bin/sh\n"+
			"# usage: build -o OUT ./NAME\n"+
			"out=$3\n"+
			"name=${4#./}\n"+
			"src=\"$PROBES/$name/"+which+".sh\"\n"+
			"echo 'nanogo: 1 of 2 packages compiled by nanogo'\n"+
			"if [ ! -f \"$src\" ]; then echo \""+which+": refused $name\" >&2; exit 1; fi\n"+
			"cp \"$src\" \"$out\" && chmod +x \"$out\"\n")
		if err := os.Chmod(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		return bin
	}
	t.Setenv("PROBES", dir)
	return Options{
		Probes:   dir,
		Nanogo:   compiler("nanogo"),
		Go:       compiler("gc"),
		Work:     filepath.Join(t.TempDir(), "work"),
		Timeout:  20 * time.Second,
		Parallel: 4,
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
}

func sweepClasses(t *testing.T, opts Options) map[string]Class {
	t.Helper()
	rep, err := Sweep(opts)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	got := make(map[string]Class)
	for _, v := range rep.Verdicts {
		got[v.Probe] = v.Class
	}
	return got
}

// Every class the harness can reach, reached.
func TestSweepClassifies(t *testing.T) {
	opts := fakeCorpus(t, map[string][2]string{
		// Both agree, in status and in output.
		"agree": {"echo hi; exit 7", "echo hi; exit 7"},
		// nanogo built something that exits differently.
		"differs": {"exit 1", "exit 7"},
		// Same status, different output. run.sh compares both and so
		// must this: a program that prints the wrong answer and exits
		// zero is the failure the corpus exists to catch.
		"prints-differently": {"echo two", "echo seven"},
		// nanogo has no script, so it refuses.
		"refused": {"", "exit 0"},
		// gc has no script, so the run has no oracle.
		"broken": {"exit 0", ""},
	})
	want := map[string]Class{
		"agree":              ClassOK,
		"differs":            ClassWrong,
		"prints-differently": ClassWrong,
		"refused":            ClassRefused,
		"broken":             ClassBroken,
	}
	got := sweepClasses(t, opts)
	if len(got) != len(want) {
		t.Errorf("swept %d probes, want %d: %v", len(got), len(want), got)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: got %q, want %q", name, got[name], w)
		}
	}
}

// A probe that hangs must be killed and named. The corpus probes channels,
// select and goroutines, so a miscompiled one deadlocks rather than fails, and
// without this the gate would burn the runner's whole budget saying nothing.
func TestSweepKillsAProbeThatHangs(t *testing.T) {
	opts := fakeCorpus(t, map[string][2]string{"hangs": {"sleep 120", "exit 0"}})
	opts.Timeout = 300 * time.Millisecond
	start := time.Now()
	rep, err := Sweep(opts)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("the sweep took %v, so the timeout did not stop the probe", d)
	}
	v := rep.Verdicts[0]
	if v.Class != ClassWrong {
		t.Errorf("a probe that hung was classed %q", v.Class)
	}
	if !strings.Contains(v.Nanogo.Out, "did not finish before the audit's timeout") {
		t.Errorf("the verdict does not say the probe was killed: %q", v.Nanogo.Out)
	}
}

// The report's own numbers must add up, or the totals in the CI log mean
// nothing.
func TestReportCountsAddUp(t *testing.T) {
	opts := fakeCorpus(t, map[string][2]string{
		"a": {"exit 0", "exit 0"},
		"b": {"", "exit 0"},
		"c": {"exit 1", "exit 2"},
	})
	rep, err := Sweep(opts)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, n := range rep.ByClass() {
		total += n
	}
	if total != rep.Probes() || rep.Probes() != 3 {
		t.Errorf("the classes total %d and the sweep read %d probes, want 3 of each", total, rep.Probes())
	}
	text := rep.String()
	for _, want := range []string{"a", "OK", "b", "REFUSED", "c", "WRONG", "TOTAL"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not mention %q:\n%s", want, text)
		}
	}
}

// Only restricts the sweep, which is how a person re-runs one probe.
func TestSweepOnlyRunsWhatItIsAsked(t *testing.T) {
	opts := fakeCorpus(t, map[string][2]string{
		"a": {"exit 0", "exit 0"},
		"b": {"exit 0", "exit 0"},
	})
	opts.Only = map[string]bool{"b": true}
	got := sweepClasses(t, opts)
	if len(got) != 1 || got["b"] != ClassOK {
		t.Errorf("Only was ignored: %v", got)
	}
}

func TestSweepRefusesToRunWithoutBothCompilers(t *testing.T) {
	opts := fakeCorpus(t, map[string][2]string{"a": {"exit 0", "exit 0"}})
	opts.Go = ""
	if _, err := Sweep(opts); err == nil {
		t.Error("a sweep with no oracle was accepted")
	}
	if _, err := Sweep(Options{Probes: filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Error("a sweep of a corpus that is not there was accepted")
	}
}

// ReadProbes counts directories holding a main.go, and nothing else. run.sh
// and go.mod are in the corpus directory and are not probes.
func TestReadProbesCountsProbesOnly(t *testing.T) {
	opts := fakeCorpus(t, map[string][2]string{"a": {"exit 0", ""}, "b": {"exit 0", ""}})
	names, err := ReadProbes(opts.Probes)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("got %v, want [a b]", names)
	}
}

// The three progress lines a nanogo build prints are not part of a refusal.
func TestStripLeavesTheRefusal(t *testing.T) {
	got := strip("nanogo: 2 of 3 packages compiled by nanogo\n" +
		"nanogo: the standard library came from the installed toolchain\n" +
		"nanogo: the executable was written by go tool link\n" +
		"nanogo: main: nanogo cannot compile function main\n")
	want := "nanogo: main: nanogo cannot compile function main"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestClassRankIsATotalOrder(t *testing.T) {
	order := []Class{ClassBroken, ClassWrong, ClassRefused, ClassOK}
	for i := 1; i < len(order); i++ {
		if order[i-1].Rank() >= order[i].Rank() {
			t.Errorf("%q does not rank below %q", order[i-1], order[i])
		}
	}
	if Class("nonsense").Rank() != ClassBroken.Rank() {
		t.Error("an unknown class must rank at the bottom, never above a real one")
	}
}

func TestResultSameSeparatesABuildFailureFromAnExitCode(t *testing.T) {
	failed := Result{Built: false}
	ran := Result{Built: true, Exit: 0}
	if failed.Same(ran) {
		t.Error("a build that failed compared equal to a program that ran and exited zero")
	}
	if !ran.Same(Result{Built: true}) {
		t.Error("two identical runs compared unequal")
	}
}

// A long refusal is cut so that the report stays readable, and the cut is
// visible.
func TestDetailIsOneLine(t *testing.T) {
	v := Verdict{Probe: "p", Class: ClassRefused, Nanogo: Result{Build: strings.Repeat("x", 400) + "\ny"}}
	got := v.Detail()
	if strings.Contains(got, "\n") {
		t.Errorf("the detail spans lines: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("a cut detail does not say it was cut: %q", got)
	}
	broken := Verdict{Probe: "p", Class: ClassBroken, Gc: Result{Build: "no such package"}}
	if !strings.Contains(broken.Detail(), "gc could not build") {
		t.Errorf("a broken probe does not say gc is what failed: %q", broken.Detail())
	}
	wrong := Verdict{Probe: "p", Class: ClassWrong, Nanogo: Result{Build: "boom"}, Gc: Result{Built: true, Exit: 7}}
	if !strings.Contains(wrong.Detail(), "build-failed") {
		t.Errorf("a wrong verdict does not show which side failed to build: %q", wrong.Detail())
	}
}
