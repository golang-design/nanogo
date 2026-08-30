// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package selfhost_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/driver"
	"golang.design/x/nanogo/internal/selfhost"
)

const (
	ratchetPath = "testdata/ratchet.txt"
	specPath    = "../../specs/060-selfhost.md"
)

// TestNanogoCompilesItsOwnPackages is the G1 precondition of
// specs/060-selfhost.md, run.
//
// Each of nanogo's own library packages is compiled on its own, against
// dependencies gc built, and the run is compared with testdata/ratchet.txt. A
// package that compiled and no longer does fails the build.
//
// This is the gate nothing else provides. The corpus of internal/gotest
// watches Go's own test files and says nothing about nanogo's source, and
// each package's unit tests test the compiler rather than what the compiler
// does to itself. A refusal added anywhere in the pipeline can take a package
// out of this set with every other gate green, which is what happened to
// syntax when the wrapper generator refused a variadic method.
//
// It is slow, because each package needs its own build cache, so it runs
// where the corpus runs.
func TestNanogoCompilesItsOwnPackages(t *testing.T) {
	if os.Getenv("NANOGO_REQUIRE_CORPUS") != "1" && !refreshing() {
		t.Skip("each package needs its own build cache, which is minutes; set NANOGO_REQUIRE_CORPUS=1")
	}
	hostIsTheTarget(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	root := repoRoot(t)
	pkgs := libraryPackages(t, root)
	if len(pkgs) == 0 {
		t.Fatal("no packages to measure, so this test would pass having proved nothing")
	}

	work := t.TempDir()
	compiler := filepath.Join(work, "nanogo")
	build := exec.Command("go", "build", "-o", compiler, "./cmd/nanogo")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building nanogo: %v\n%s", err, out)
	}

	rs, err := selfhost.Measure(selfhost.Options{
		Compiler: compiler,
		Packages: selfhost.Paths(pkgs...),
		Dir:      root,
		Work:     filepath.Join(work, "runs"),
	})
	if err != nil {
		t.Fatalf("measuring: %v", err)
	}
	t.Logf("%s", selfhost.Table(rs))

	// A package nanogo was never asked about proves nothing, and it would
	// otherwise be counted as a refusal and read as a regression. It is a
	// fault in the harness, so it is reported as one.
	for _, p := range selfhost.WithDecision(rs, selfhost.NotReached) {
		t.Errorf("nanogo was never asked about %s, so this run proved nothing about it; "+
			"the compile action was answered from a cache", p)
	}
	// A delegated package means the allowlist did not select it, which is the
	// failure mode that once reported twelve of twelve.
	for _, p := range selfhost.WithDecision(rs, selfhost.Delegated) {
		t.Errorf("nanogo delegated %s to gc, so the allowlist did not name it", p)
	}

	checkRatchet(t, rs)
}

func checkRatchet(t *testing.T, rs []selfhost.Result) {
	t.Helper()
	path := ratchetPath
	if refreshing() {
		if err := selfhost.WriteRatchet(path, selfhost.FromResults(rs)); err != nil {
			t.Fatalf("writing the ratchet: %v", err)
		}
		t.Logf("wrote %s; read the diff before committing", path)
		return
	}
	rt, err := selfhost.ReadRatchet(path)
	if err != nil {
		t.Fatalf("reading the ratchet: %v", err)
	}
	// The count first. Every number read against this file has it for a
	// denominator, so a denominator that moved makes them all suspect.
	if recorded, measured, changed := rt.CountChanged(rs); changed {
		t.Errorf("the ratchet records %d packages and this run measured %d; "+
			"either a package was added to the module or the list stopped finding one",
			recorded, measured)
	}
	for _, r := range rt.Regressions(rs) {
		t.Errorf("REGRESSION %s", r)
	}
	if gains := rt.Gains(rs); len(gains) > 0 {
		t.Logf("%d packages compile that the ratchet does not record:\n\t%s\n"+
			"Growth is expected and does not fail this test. Refresh with "+
			"NANOGO_REQUIRE_CORPUS=1 NANOGO_REFRESH_RATCHET=1 go test ./internal/selfhost/",
			len(gains), strings.Join(gains, "\n\t"))
	}
}

// TestTheSpecStatesWhatTheRatchetRecords keeps specs/060-selfhost.md's count
// equal to the recorded one.
//
// It runs on a plain go test in milliseconds, so the document is checked on
// every run even though the measurement behind it is not. That is the split
// internal/hygiene's docdrift gate uses: a slow producer and a fast reader,
// because a gate that only runs in CI is a gate a developer finds out about
// after pushing.
func TestTheSpecStatesWhatTheRatchetRecords(t *testing.T) {
	rt, err := selfhost.ReadRatchet(ratchetPath)
	if err != nil {
		t.Fatalf("reading the ratchet: %v", err)
	}
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	doc := string(b)

	// The pattern must match exactly once. A pattern that matched nothing
	// would switch the gate off the next time the sentence is reworded, which
	// is the commonest way a check like this disappears.
	stated := regexp.MustCompile(`\*\*(\d+) of (\d+) compile\*\*`)
	m := stated.FindAllStringSubmatch(doc, -1)
	if len(m) != 1 {
		t.Fatalf("%s states \"N of M compile\" %d times, and this gate reads exactly one", specPath, len(m))
	}
	gotN, _ := strconv.Atoi(m[0][1])
	gotM, _ := strconv.Atoi(m[0][2])
	if wantN := len(rt.Compiles); gotN != wantN {
		t.Errorf("%s says %d packages compile and %s records %d.\n"+
			"Correct the document, or refresh the ratchet if the measurement really moved.",
			specPath, gotN, ratchetPath, wantN)
	}
	if gotM != rt.Packages {
		t.Errorf("%s measures against %d packages and %s records %d",
			specPath, gotM, ratchetPath, rt.Packages)
	}
}

// TestTheListIsTheCompilerAndNotTheHarnesses gates the denominator.
//
// Every ratio this package reports is measured against what libraryPackages
// returns, so a filter that quietly dropped a package would raise the
// percentage while measuring less. The ratchet's package count catches that,
// but only after a refresh has baked the smaller number in, so the rule is
// checked here as well as recorded there.
//
// The two ends of the module are named because they are the ones a filter
// gets wrong: the root package, whose import path is the module path with
// nothing after it, and the deepest, which a prefix test can take for a
// parent.
func TestTheListIsTheCompilerAndNotTheHarnesses(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	got := libraryPackages(t, repoRoot(t))
	held := map[string]bool{}
	for _, p := range got {
		held[p] = true
	}
	for _, want := range []string{
		"golang.design/x/nanogo",
		"golang.design/x/nanogo/ssa/rules",
		"golang.design/x/nanogo/types2/gen",
	} {
		if !held[want] {
			t.Errorf("%s is a package nanogo is written in and the list does not name it:\n\t%s",
				want, strings.Join(got, "\n\t"))
		}
	}
	for _, unwanted := range []string{
		// The commands. The go command builds a main package into an
		// executable rather than an archive an importer reads.
		"golang.design/x/nanogo/cmd/nanogo",
		"golang.design/x/nanogo/cmd/nanogo-dist",
		// The harnesses. They run nanogo, and nanogo is not written in them,
		// so a refusal they tripped would be a fact about a test and not
		// about the compiler.
		"golang.design/x/nanogo/internal/gotest",
		"golang.design/x/nanogo/internal/e2e",
		"golang.design/x/nanogo/internal/selfhost",
	} {
		if held[unwanted] {
			t.Errorf("%s is not part of the compiler and the list names it", unwanted)
		}
	}
	// The ratchet's count is the same list, so the two must agree before a
	// refresh rather than after one.
	rt, err := selfhost.ReadRatchet(ratchetPath)
	if err != nil {
		t.Fatalf("reading the ratchet: %v", err)
	}
	if len(got) != rt.Packages {
		t.Errorf("the list holds %d packages and %s records %d:\n\t%s",
			len(got), ratchetPath, rt.Packages, strings.Join(got, "\n\t"))
	}
}

// hostIsTheTarget skips a measurement that cannot mean anything here.
//
// nanogo emits arm64, and it refuses a build for any other GOARCH before it
// compiles a function:
//
//	nanogo cannot compile for this target: nanogo emits arm64 machine code
//	and the build is for amd64
//
// So on an amd64 runner every package reads as refused and the ratchet reports
// every one of them as a regression. That is a fact about the host and not
// about the compiler, and the CI matrix holds both an arm64 runner and an
// amd64 one, so the gate runs where it means something and skips where it does
// not.
//
// The document gates in this package do not skip. They read the ratchet and
// the spec and run anywhere.
func hostIsTheTarget(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != driver.TargetArch {
		t.Skipf("nanogo emits %s machine code and GOARCH is %s, so it refuses every package here",
			driver.TargetArch, runtime.GOARCH)
	}
}

func refreshing() bool { return os.Getenv("NANOGO_REFRESH_RATCHET") == "1" }

// repoRoot is the module directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// libraryPackages are the packages nanogo is written in.
//
// It is derived rather than listed, so a package added to the compiler joins
// the measurement without anybody remembering to add it. A written list is
// how a package gets left out and the total goes up anyway.
//
// Two kinds are dropped and both are dropped by what they are. A main package
// is a command, and the go command builds it into an executable rather than
// an archive an importer reads. A package under internal/ is a test harness
// for this repository, and it is not the compiler: internal/gotest and
// internal/e2e run nanogo, and nanogo is not written in them.
func libraryPackages(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}} {{.Name}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, name, ok := strings.Cut(line, " ")
		if !ok || name == "main" {
			continue
		}
		if strings.Contains(path, "/internal/") {
			continue
		}
		pkgs = append(pkgs, path)
	}
	return pkgs
}
