// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"strings"
	"testing"
)

// TestSweepEnvPointsTheChildAtTheSweepsOwnTemporaryDirectory pins the
// containment.
//
// nanogo build opens a scratch directory with os.MkdirTemp, which reads
// TMPDIR, and removes it with a defer. This sweep kills a build that passes
// its deadline, and a defer does not run in a process that was killed, so the
// directory outlives the run and nothing removes it: the child is gone and the
// parent never knew the name. There is no fix for the kill, so the parent
// chooses the name instead and the sweep's own scratch directory takes it.
//
// The old value is not left in place beside the new one. A variable set twice
// is read by the last entry on some systems and the first on others, and a
// containment that depends on which is not a containment.
func TestSweepEnvPointsTheChildAtTheSweepsOwnTemporaryDirectory(t *testing.T) {
	t.Setenv("TMPDIR", "/inherited/and/wrong")

	var got []string
	for _, kv := range sweepEnv("/cache", "/work") {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			got = append(got, v)
		}
	}
	if len(got) != 1 || got[0] != "/work" {
		t.Errorf("TMPDIR is %v, want exactly one entry naming the sweep's work directory", got)
	}

	// With no work directory there is nothing to contain the child in, and
	// inheriting is the honest answer rather than inventing a path.
	var none []string
	for _, kv := range sweepEnv("/cache", "") {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			none = append(none, v)
		}
	}
	if len(none) != 1 || none[0] != "/inherited/and/wrong" {
		t.Errorf("with no work directory TMPDIR is %v, want the inherited one", none)
	}
}

// TestOnlyABuildIsContained keeps the corpus program's own temporary directory
// where it was.
//
// The containment exists for a build, because a build that passes its deadline
// is killed and leaves its scratch directory behind. A corpus program is a
// different thing: it can make a directory under TMPDIR and print the path,
// and this sweep compares the output of the two compilers' programs. Moving
// that path moves the output.
//
// linkmain_run.go does exactly that. Containing its run made the two outputs
// differ and the sweep reported a miscompilation in a file that had none,
// which is the worst way for a harness to be wrong: it accuses the compiler.
func TestOnlyABuildIsContained(t *testing.T) {
	tmpdirOf := func(env []string) string {
		var last string
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
				last = v
			}
		}
		return last
	}
	if got := tmpdirOf(sweepEnv("/cache", "/work")); got != "/work" {
		t.Errorf("a build runs with TMPDIR %q, want the sweep's work directory", got)
	}
	// The empty work directory is what run passes, and it means "inherit".
	t.Setenv("TMPDIR", "/inherited")
	if got := tmpdirOf(sweepEnv("/cache", "")); got != "/inherited" {
		t.Errorf("a program runs with TMPDIR %q, want the inherited one", got)
	}
}
