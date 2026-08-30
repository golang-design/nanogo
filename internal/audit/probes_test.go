// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package audit_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.design/x/nanogo/internal/audit"
)

const ratchetPath = "testdata/ratchet.txt"

// refreshRatchet reports whether this run rewrites the ratchet rather than
// gating against it.
func refreshRatchet() bool { return os.Getenv("NANOGO_REFRESH_RATCHET") == "1" }

// requireCorpus reports whether a missing corpus is a failure rather than a
// reason to skip. CI sets it, so a gate that found nothing fails there.
func requireCorpus() bool { return os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" }

func missingCorpus(t *testing.T, format string, args ...any) {
	t.Helper()
	if requireCorpus() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// requireLink reports whether a host that cannot run nanogo's output is a
// failure rather than a reason to skip. internal/gotest and internal/e2e use
// the same variable for the same reason: nanogo emits arm64 machine code and
// has no second backend, so on any other host the programs it builds die
// walking their own pc tables.
func requireLink() bool { return os.Getenv("NANOGO_REQUIRE_LINK") == "1" }

func hostRunsNanogoOutput(t *testing.T) {
	t.Helper()
	if runtime.GOARCH == "arm64" {
		return
	}
	if requireLink() {
		t.Fatalf("NANOGO_REQUIRE_LINK is set and GOARCH is %s; nanogo emits arm64 and cannot be run here", runtime.GOARCH)
	}
	t.Skipf("nanogo emits arm64 machine code and GOARCH is %s, so nothing it compiles can run here", runtime.GOARCH)
}

// The nanogo binary and the shared GOCACHE, built once for the whole package.
//
// One installation and one cache rather than one per test, as internal/gotest
// does and for the same reason. Every probe is different source, so nothing
// either compiler produces for a probe is ever a cache hit and only the
// standard library is replayed; replaying it is the difference between a two
// minute sweep and a twenty minute one. The directory is under TMPDIR and
// removed by TestMain, because a t.TempDir would be removed when the first
// test that asked for it finished and the second would find the tools gone.
//
// TestMain is the whole of the removal. It was missing until August 2026 and
// the probes leaked 145 directories, 7.1GB.
var (
	toolsOnce sync.Once
	toolsBin  string
	toolsGo   string
	toolsRoot string
	toolsErr  error
)

// TestMain removes the installation and cache that tools built, which no test
// can remove for itself: they outlive every test in the package.
func TestMain(m *testing.M) {
	code := m.Run()
	if toolsRoot != "" {
		os.RemoveAll(toolsRoot)
	}
	os.Exit(code)
}

func tools(t *testing.T) (nanogo, goCmd, cache string) {
	t.Helper()
	hostRunsNanogoOutput(t)
	toolsOnce.Do(func() {
		if toolsGo, toolsErr = exec.LookPath("go"); toolsErr != nil {
			return
		}
		if toolsRoot, toolsErr = os.MkdirTemp("", "nanogo-audit"); toolsErr != nil {
			return
		}
		toolsBin = filepath.Join(toolsRoot, "nanogo")
		build := exec.Command(toolsGo, "build", "-o", toolsBin, "golang.design/x/nanogo/cmd/nanogo")
		build.Dir = repoRoot(t)
		if out, err := build.CombinedOutput(); err != nil {
			toolsErr = fmt.Errorf("building nanogo: %w\n%s", err, out)
		}
	})
	if toolsErr != nil {
		missingCorpus(t, "the probe corpus needs a go command and a built nanogo: %v", toolsErr)
	}
	return toolsBin, toolsGo, filepath.Join(toolsRoot, "gocache")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func probesDir(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(audit.ProbesDir); err != nil {
		missingCorpus(t, "the probe corpus is not in this checkout: %v", err)
	}
	return audit.ProbesDir
}

// TestProbes is the behaviour gate.
//
// It compiles every probe twice, once with nanogo and once with gc, runs both
// and compares them, then holds the result against testdata/ratchet.txt. A
// probe whose class fell fails. A probe whose class rose does not fail and is
// reported, because that is the moment a capability claim in README.md, doc.go
// or driver/help.go goes stale.
func TestProbes(t *testing.T) {
	nanogo, goCmd, cache := tools(t)
	dir := probesDir(t)

	start := time.Now()
	rep, err := audit.Sweep(audit.Options{
		Probes:  dir,
		Nanogo:  nanogo,
		Go:      goCmd,
		Cache:   cache,
		Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("the sweep could not run: %v", err)
	}
	if rep.Probes() == 0 {
		t.Fatal("the corpus holds no probes, so this gate proves nothing")
	}
	by := rep.ByClass()
	t.Logf("swept %d probes in %v\n%s", rep.Probes(), time.Since(start).Round(time.Millisecond), rep)
	t.Logf("PROBES %d ok=%d refused=%d wrong=%d broken=%d", rep.Probes(),
		by[audit.ClassOK], by[audit.ClassRefused], by[audit.ClassWrong], by[audit.ClassBroken])

	checkRatchet(t, rep)
}

// checkRatchet compares the run with what was recorded, and fails on anything
// the corpus lost.
func checkRatchet(t *testing.T, rep *audit.Report) {
	t.Helper()
	if refreshRatchet() {
		if err := audit.WriteRatchet(ratchetPath, audit.FromReport(rep)); err != nil {
			t.Fatalf("writing the ratchet: %v", err)
		}
		t.Logf("wrote %s; read the diff before committing it", ratchetPath)
		return
	}

	rt, err := audit.ReadRatchet(ratchetPath)
	if err != nil {
		t.Fatalf("reading the ratchet: %v\n\t"+
			"It records what the corpus proved. Refresh it with NANOGO_REFRESH_RATCHET=1.", err)
	}
	if c := rt.CountChange(rep); c != "" {
		t.Errorf("the corpus changed size: %s\n\t"+
			"Every class below is measured against that count. A probe directory was added or "+
			"deleted, so refresh the ratchet in the same change and read the diff.", c)
	}
	for _, r := range rt.Regressions(rep) {
		t.Errorf("REGRESSION %s", r)
	}
	if gains := rt.Gains(rep); len(gains) > 0 {
		t.Logf("PROGRESS %d probes improved on what the ratchet records:\n\t%s\n"+
			"Growth is expected and does not fail this test. Two things follow from it. "+
			"A sentence in README.md, doc.go or driver/help.go describes the behaviour these "+
			"probes used to have and is now false, so correct it in this change. Then refresh "+
			"the ratchet with NANOGO_REFRESH_RATCHET=1 so that it guards them from tomorrow.",
			len(gains), strings.Join(gains, "\n\t"))
	}
}

// TestHarnessAgreesWithRunScript keeps the Go harness and the shell script
// from drifting apart.
//
// testdata/probes/run.sh is what CONTRIBUTING.md tells a contributor to run,
// and this package is what CI runs. Two implementations of one audit is one
// implementation too many unless something compares them.
//
// One probe of each recorded class, so the classification is exercised rather
// than only the plumbing. Classes are compared and messages are not: the shell
// script folds a refusal onto one line and the harness does not, and a gate
// that compared the text would fail on a formatting difference.
func TestHarnessAgreesWithRunScript(t *testing.T) {
	nanogo, goCmd, cache := tools(t)
	dir := probesDir(t)

	// One probe of each class the ratchet records, chosen from the ratchet
	// rather than named here. A probe named in this file would be a second
	// place the corpus is described, and it would go stale the first time
	// that probe changed class.
	rt, err := audit.ReadRatchet(ratchetPath)
	if err != nil {
		t.Fatalf("reading the ratchet: %v", err)
	}
	only, picked := map[string]bool{}, map[audit.Class]string{}
	for _, name := range sortedProbes(rt) {
		if c := rt.Class[name]; picked[c] == "" {
			picked[c], only[name] = name, true
		}
	}
	if len(only) < 2 {
		t.Fatalf("the ratchet records %d classes, so this proves little: %v", len(only), picked)
	}
	t.Logf("comparing one probe of each recorded class: %v", picked)
	rep, err := audit.Sweep(audit.Options{
		Probes: dir, Nanogo: nanogo, Go: goCmd, Cache: cache, Timeout: 90 * time.Second, Only: only,
	})
	if err != nil {
		t.Fatalf("the sweep could not run: %v", err)
	}

	args := []string{filepath.Join(dir, "run.sh")}
	for name := range only {
		args = append(args, name)
	}
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(), "NG="+nanogo, "GOCACHE="+cache)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run.sh: %v\n%s", err, out)
	}
	t.Logf("run.sh said:\n%s", out)

	script := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			script[f[0]] = strings.ToLower(f[1])
		}
	}
	if len(script) != len(only) {
		t.Fatalf("run.sh reported %d probes, want %d:\n%s", len(script), len(only), out)
	}
	for _, v := range rep.Verdicts {
		if got := script[v.Probe]; got != string(v.Class) {
			t.Errorf("%s: run.sh says %q and this package says %q; the two audits have drifted apart",
				v.Probe, got, v.Class)
		}
	}
}

// sortedProbes returns the ratchet's probe names in a fixed order, so that the
// test above picks the same probes on every run (specs/053-determinism.md).
func sortedProbes(rt *audit.Ratchet) []string {
	out := make([]string, 0, len(rt.Class))
	for name := range rt.Class {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
