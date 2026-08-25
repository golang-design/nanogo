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
// Options.Cache. The directory is under TMPDIR and removed by the process that
// made it, because a t.TempDir would be removed when the first test that asked
// for it finished.
var (
	toolsOnce sync.Once
	toolsBin  string
	toolsGo   string
	toolsRoot string
	toolsErr  error
)

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
		Corpus:  corpusDir(t),
		Work:    work,
		Nanogo:  nanogo,
		Go:      goCmd,
		Cache:   cache,
		Timeout: 90 * time.Second,
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
