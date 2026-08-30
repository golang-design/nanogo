// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.design/x/nanogo/internal/gotest"
)

// requireCorpus reports whether a missing corpus is a failure rather than a
// reason to skip. CI sets it, so a gate that found nothing fails there.
//
// The corpus is vendored, so "missing" now means the checkout is broken rather
// than the machine is. That is a stronger claim than the other corpus tests
// can make and it is the reason the files were copied in.
func requireCorpus() bool { return os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" }

func missingCorpus(t *testing.T, format string, args ...any) {
	t.Helper()
	if requireCorpus() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// requireLink reports whether a host that cannot run nanogo's output is a
// failure rather than a reason to skip. internal/e2e uses the same variable
// for the same reason: nanogo emits arm64 machine code and has no second
// backend, so on any other host the programs it builds die walking their own
// pc tables.
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

// The nanogo binary and the shared caches, built once for the whole package.
//
// One installation and one GOCACHE, rather than one per test: see
// Options.Cache. The directory is under TMPDIR and removed by TestMain,
// because a t.TempDir would be removed when the first test that asked for it
// finished and the second would find the tools gone.
//
// TestMain is the whole of the removal. It was missing until August 2026 and
// the sweep leaked 258 directories, 160GB, because the cache holds a standard
// library built by both compilers and each run makes a new one.
var (
	toolsOnce sync.Once
	toolsBin  string
	toolsGo   string
	toolsRoot string
	toolsErr  error
)

// TestMain removes the installation and caches that tools built, which no
// test can remove for itself: they outlive every test in the package.
func TestMain(m *testing.M) {
	code := m.Run()
	if toolsRoot != "" {
		os.RemoveAll(toolsRoot)
	}
	os.Exit(code)
}

func tools(t *testing.T) (nanogo, goCmd, work, cache string) {
	t.Helper()
	toolsOnce.Do(func() {
		toolsGo, toolsErr = exec.LookPath("go")
		if toolsErr != nil {
			return
		}
		toolsRoot, toolsErr = os.MkdirTemp("", "nanogo-corpus")
		if toolsErr != nil {
			return
		}
		toolsBin = filepath.Join(toolsRoot, "nanogo")
		build := exec.Command(toolsGo, "build", "-o", toolsBin, "golang.design/x/nanogo/cmd/nanogo")
		build.Dir = repoRoot(t)
		if out, err := build.CombinedOutput(); err != nil {
			toolsErr = &buildError{err, string(out)}
		}
	})
	if toolsErr != nil {
		missingCorpus(t, "the corpus needs a go command and a built nanogo: %v", toolsErr)
	}
	return toolsBin, toolsGo, filepath.Join(toolsRoot, "work"), filepath.Join(toolsRoot, "gocache")
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func corpusDir(t *testing.T) string {
	t.Helper()
	dir := gotest.CorpusDir
	if _, err := os.Stat(dir); err != nil {
		missingCorpus(t, "the vendored corpus is not in this checkout: %v", err)
	}
	return dir
}

// sweep runs the corpus, or the named subset of it.
func sweep(t *testing.T, only map[string]bool) *gotest.Report {
	t.Helper()
	hostRunsNanogoOutput(t)
	nanogo, goCmd, work, cache := tools(t)
	start := time.Now()
	rep, err := gotest.Sweep(gotest.Options{
		Corpus: corpusDir(t),
		Work:   work,
		Nanogo: nanogo,
		Go:     goCmd,
		Cache:  cache,
		// Generous on purpose. This bounds a nanogo bug, an infinite loop it
		// generated or a compile that does not finish, and it is not a
		// performance budget. A recipe that spawns the toolchain several
		// times legitimately takes tens of seconds: linkmain_run.go runs
		// go tool compile and go tool link and takes about 44 seconds on an
		// idle machine.
		//
		// It was 90 seconds, and that was close enough to 44 that a loaded
		// machine crossed it. A refresh taken while four other test binaries
		// were running classed linkmain_run.go as timed-out, and because the
		// ratchet records it as matched, that reads as a regression and fails
		// the build. A gate that fails on machine load is a gate people learn
		// to re-run rather than read.
		Timeout: 180 * time.Second,
		Only:    only,
	})
	if err != nil {
		t.Fatalf("the sweep could not run: %v", err)
	}
	t.Logf("swept %d files in %v\n%s", rep.Files, time.Since(start).Round(time.Millisecond), rep)
	if err := rep.CheckTotals(); err != nil {
		t.Fatalf("the totals do not add up, so the numbers above mean nothing: %v", err)
	}
	return rep
}
