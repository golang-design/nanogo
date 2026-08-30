// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package selfhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadDecisionMatchesThePackageExactly is the trap that made
// internal/runtime/gc read as internal/runtime/gc/scan.
//
// Both are in the bootstrap closure and one is a prefix of the other, so a
// reader that matched on a prefix would report one package's decision for the
// other. The log holds a line for every package the build compiled, including
// the dependencies gc built, so the wrong line is always there to be found.
func TestReadDecisionMatchesThePackageExactly(t *testing.T) {
	log := filepath.Join(t.TempDir(), "decisions.log")
	write(t, log, strings.Join([]string{
		"delegated internal/goarch not on the allowlist",
		"failed internal/runtime/gc/scan internal/runtime/gc/scan: nanogo cannot compile x",
		"compiled internal/runtime/gc /tmp/b001/_pkg_.a",
	}, "\n"))

	for _, tc := range []struct {
		pkg    string
		want   Decision
		reason string
	}{
		{"internal/runtime/gc", Compiled, "/tmp/b001/_pkg_.a"},
		{"internal/runtime/gc/scan", Failed, "internal/runtime/gc/scan: nanogo cannot compile x"},
		{"internal/goarch", Delegated, "not on the allowlist"},
		{"internal/runtime", NotReached, ""},
	} {
		got, err := readDecision(log, tc.pkg)
		if err != nil {
			t.Fatalf("%s: %v", tc.pkg, err)
		}
		if got.Decision != tc.want || got.Reason != tc.reason {
			t.Errorf("%s is %q %q, want %q %q", tc.pkg, got.Decision, got.Reason, tc.want, tc.reason)
		}
	}
}

// TestReadDecisionReportsAMissingLogAsNotReached is the cached-compile case.
//
// The go command answers a compile action from its cache when the action ID
// is unchanged, and then -toolexec never runs and nanogo writes nothing. That
// run proved nothing, and calling it a refusal would report a regression
// where there was only a cache hit.
func TestReadDecisionReportsAMissingLogAsNotReached(t *testing.T) {
	got, err := readDecision(filepath.Join(t.TempDir(), "absent.log"), "example.com/p")
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != NotReached {
		t.Errorf("a missing log reads as %q, want %q", got.Decision, NotReached)
	}
}

// TestReadDecisionKeepsTheFirstDecision checks that a second line for the same
// package does not overwrite the first.
//
// nanogo returns on its first error, so a later line for one package is a
// different invocation of the same compile and the first is the one that
// describes the run.
func TestReadDecisionKeepsTheFirstDecision(t *testing.T) {
	log := filepath.Join(t.TempDir(), "decisions.log")
	write(t, log, "failed example.com/p the first reason\ncompiled example.com/p /tmp/x.a\n")
	got, err := readDecision(log, "example.com/p")
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != Failed || got.Reason != "the first reason" {
		t.Errorf("got %q %q, want failed \"the first reason\"", got.Decision, got.Reason)
	}
}

// TestReadDecisionIgnoresAWordItDoesNotKnow keeps a future log line from
// being read as a decision.
func TestReadDecisionIgnoresAWordItDoesNotKnow(t *testing.T) {
	log := filepath.Join(t.TempDir(), "decisions.log")
	write(t, log, "considered example.com/p something new\n")
	got, err := readDecision(log, "example.com/p")
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != NotReached {
		t.Errorf("an unknown word reads as %q, want %q", got.Decision, NotReached)
	}
}

// TestMeasureRefusesAnEnvironmentItOwns is the silent-unmeasure guard.
//
// A caller that set NANOGO_ALLOWLIST, NANOGO_LOG or GOCACHE through Env would
// override what Measure sets per package: one allowlist for every build, one
// log for every build, or one cache for every build. Each of those makes the
// whole measurement wrong in a way that reads as a plausible answer, so it is
// an error and not an override.
func TestMeasureRefusesAnEnvironmentItOwns(t *testing.T) {
	for _, kv := range []string{"NANOGO_ALLOWLIST=/tmp/a", "NANOGO_LOG=/tmp/l", "GOCACHE=/tmp/c"} {
		_, err := Measure(Options{
			Compiler: "nanogo", Packages: Paths("example.com/p"),
			Work: t.TempDir(), Env: []string{kv},
		})
		if err == nil {
			t.Errorf("Measure accepted Env %q", kv)
			continue
		}
		name, _, _ := strings.Cut(kv, "=")
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error for %q does not name it: %v", kv, err)
		}
	}
}

// TestMeasureRefusesAnEmptyMeasurement keeps a run that proves nothing from
// passing.
func TestMeasureRefusesAnEmptyMeasurement(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no compiler", Options{Packages: Paths("p"), Work: "/tmp"}, "no compiler"},
		{"no packages", Options{Compiler: "nanogo", Work: "/tmp"}, "no packages"},
		{"no work directory", Options{Compiler: "nanogo", Packages: Paths("p")}, "no work directory"},
	} {
		_, err := Measure(tc.opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the error is %v, want one naming %q", tc.name, err, tc.want)
		}
	}
}

// TestSlugKeepsPackagesApart checks that two import paths cannot share a work
// directory, which would let one build read another's allowlist.
func TestSlugKeepsPackagesApart(t *testing.T) {
	seen := map[string]string{}
	for _, p := range []string{
		"golang.design/x/nanogo", "golang.design/x/nanogo/ssa", "golang.design/x/nanogo/ssa/rules",
		"internal/runtime/gc", "internal/runtime/gc/scan", "math/bits",
	} {
		s := slug(p)
		if s == "" || strings.ContainsRune(s, filepath.Separator) {
			t.Errorf("the slug of %q is %q, which is not one path element", p, s)
		}
		if other, ok := seen[s]; ok {
			t.Errorf("%q and %q share the slug %q", p, other, s)
		}
		seen[s] = p
	}
	if got := slug(""); got == "" {
		t.Error("the slug of the empty path is empty, so it is not a directory name")
	}
}

// TestTableNamesTheReasonRatherThanCountingIt checks the report prints a
// refusal whole.
//
// A count says something is wrong and not what, and what is the only part
// that can be acted on.
func TestTableNamesTheReasonRatherThanCountingIt(t *testing.T) {
	got := Table([]Result{
		{Path: "a", Decision: Compiled},
		{Path: "b", Decision: Failed, Reason: "nanogo cannot compile a package with assembly in it"},
	})
	for _, want := range []string{"1 of 2 packages", "compiled", "a", "failed", "b",
		"nanogo cannot compile a package with assembly in it"} {
		if !strings.Contains(got, want) {
			t.Errorf("the table does not hold %q:\n%s", want, got)
		}
	}
}

// TestCountsAndSetsReadOneMeasurement checks the accessors against one report.
func TestCountsAndSetsReadOneMeasurement(t *testing.T) {
	rs := []Result{
		{Path: "z", Decision: Compiled},
		{Path: "a", Decision: Compiled},
		{Path: "b", Decision: Failed},
		{Path: "c", Decision: Delegated},
		{Path: "d", Decision: NotReached},
	}
	if got := Count(rs, Compiled); got != 2 {
		t.Errorf("Count(Compiled) = %d, want 2", got)
	}
	// Sorted, so two runs of the same list report in the same order
	// (specs/053-determinism.md).
	if got := strings.Join(WithDecision(rs, Compiled), ","); got != "a,z" {
		t.Errorf("WithDecision(Compiled) = %q, want \"a,z\"", got)
	}
	for _, d := range []Decision{Failed, Delegated, NotReached} {
		if got := Count(rs, d); got != 1 {
			t.Errorf("Count(%s) = %d, want 1", d, got)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMeasureRunsTheBuildAndReadsOnlyTheLog exercises the whole path with a
// stand-in for nanogo.
//
// The stand-in writes a decision line and exits without running the real tool,
// so the build FAILS. The result must still be read, because that is the rule
// the whole package rests on: a -toolexec build succeeds whether nanogo
// compiled the package or handed it to gc, so the exit status carries no
// information and only the log line does. A measurement that read the status
// would report this run as a refusal.
func TestMeasureRunsTheBuildAndReadsOnlyTheLog(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "stand-in")
	write(t, tool, `#!/bin/sh
# A stand-in for nanogo under -toolexec. It records a decision for the package
# the go command named with -p and exits without compiling anything, so the
# build fails and the log is the only evidence of what happened.
#
# -V=full is answered by the real tool. The go command parses the compiler's
# build ID out of that before it compiles anything and stops the build when it
# cannot, so a stand-in that answered it itself would never be asked about a
# package at all.
for a in "$@"; do
	if [ "$a" = "-V=full" ]; then exec "$@"; fi
done
pkg=""
prev=""
for a in "$@"; do
	if [ "$prev" = "-p" ]; then pkg="$a"; fi
	case "$a" in -p=*) pkg="${a#-p=}";; esac
	prev="$a"
done
if [ -n "$pkg" ] && [ -n "$NANOGO_LOG" ]; then
	echo "compiled $pkg $(cat "$NANOGO_ALLOWLIST")" >> "$NANOGO_LOG"
fi
exit 0
`)
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}

	mod := filepath.Join(dir, "mod")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(mod, "go.mod"), "module measured.example/m\n\ngo 1.27\n")
	write(t, filepath.Join(mod, "m.go"), "package m\n\nfunc F() int { return 1 }\n")

	rs, err := Measure(Options{
		Compiler: tool,
		Packages: Paths("measured.example/m"),
		Dir:      mod,
		Work:     filepath.Join(dir, "work"),
		Parallel: 1,
	})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(rs) != 1 || rs[0].Decision != Compiled {
		t.Fatalf("the result is %+v, want one compiled", rs)
	}
	// The allowlist the stand-in echoed back proves Measure wrote one naming
	// this package, which is the difference between measuring the compiler
	// and measuring gc.
	if got := rs[0].Reason; !strings.Contains(got, "measured.example/m") {
		t.Errorf("the allowlist the build saw was %q", got)
	}
}

// TestMeasureKeepsTheOrderItWasGiven checks that a parallel run reports in the
// caller's order.
//
// The builds finish in whatever order the machine decides, so a report built
// from completions would print a different table on every run
// (specs/053-determinism.md).
func TestMeasureKeepsTheOrderItWasGiven(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "nothing")
	write(t, tool, "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = \"-V=full\" ]; then exec \"$@\"; fi; done\nexit 0\n")
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []string{"c.example/three", "a.example/one", "b.example/two"}
	rs, err := Measure(Options{
		Compiler: tool, Packages: Paths(want...),
		Dir: dir, Work: filepath.Join(dir, "work"), Parallel: 3,
	})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	var got []string
	for _, r := range rs {
		got = append(got, r.Path)
		// Nothing was compiled and nothing was refused: the stand-in never
		// wrote a log, so the run proved nothing about any of them.
		if r.Decision != NotReached {
			t.Errorf("%s is %q, want %q", r.Path, r.Decision, NotReached)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the results are %v, want them in the order given, %v", got, want)
	}
}

// TestAMainPackageIsAllowlistedByTheNameTheGoCommandGivesIt is the trap that
// reported a program as never reached.
//
// The go command passes -p main for a main package whatever its import path
// is, so nanogo is told the package is called main. A measurement that used
// the import path would write the wrong name on the allowlist, the package
// would go to gc, and the log would carry no line for the import path at all.
// The build succeeds either way, so the run reads as a package nanogo was
// never asked about rather than as a mistake.
//
// The result is still keyed by the import path, because "main" names one
// package per build and a list holding two of them would collide.
func TestAMainPackageIsAllowlistedByTheNameTheGoCommandGivesIt(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "stand-in")
	// This stand-in records the package the allowlist names and then runs the
	// real tool for every invocation, so the build completes. The stand-in
	// above does not, which is right for a library with no imports and wrong
	// here: a main package needs the runtime, and a stand-in that compiled
	// nothing would stop the build before the main package was reached.
	write(t, tool, `#!/bin/sh
pkg=""
prev=""
for a in "$@"; do
	if [ "$prev" = "-p" ]; then pkg="$a"; fi
	case "$a" in -p=*) pkg="${a#-p=}";; esac
	prev="$a"
done
if [ -n "$pkg" ] && [ -n "$NANOGO_LOG" ] && [ "$pkg" = "$(cat "$NANOGO_ALLOWLIST")" ]; then
	echo "compiled $pkg $(cat "$NANOGO_ALLOWLIST")" >> "$NANOGO_LOG"
fi
exec "$@"
`)
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(dir, "mod")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(mod, "go.mod"), "module measured.example/prog\n\ngo 1.27\n")
	write(t, filepath.Join(mod, "main.go"), "package main\n\nfunc main() {}\n")

	rs, err := Measure(Options{
		Compiler: tool,
		Packages: []Package{MainPackage("measured.example/prog")},
		Dir:      mod,
		Work:     filepath.Join(dir, "work"),
		Parallel: 1,
	})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(rs) != 1 || rs[0].Decision != Compiled {
		t.Fatalf("the result is %+v, want one compiled", rs)
	}
	if rs[0].Path != "measured.example/prog" {
		t.Errorf("the result is keyed by %q, want the import path", rs[0].Path)
	}
	// The stand-in echoes the allowlist back, so this is what the build saw.
	if got := rs[0].Reason; !strings.Contains(got, "main") {
		t.Errorf("the allowlist the build saw was %q, and nanogo is told the package is called main", got)
	}
}

// TestPathsUsesOneNameForBoth checks the common case, where a package's build
// target and the name nanogo is told are the same.
func TestPathsUsesOneNameForBoth(t *testing.T) {
	got := Paths("a/b", "c")
	want := []Package{{Target: "a/b", Name: "a/b"}, {Target: "c", Name: "c"}}
	if len(got) != len(want) {
		t.Fatalf("Paths returned %d packages, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Paths[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if p := MainPackage("x/y"); p.Target != "x/y" || p.Name != "main" {
		t.Errorf("MainPackage = %+v, want target x/y named main", p)
	}
}
