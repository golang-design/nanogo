// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// distributionTree writes a tree with the shape of a nanogo release: a VERSION
// file and a per-target archive directory.
func distributionTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", TargetDir()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("nanogo0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func noExecutable() (string, error) { return "", errors.New("unknown") }

func TestFindRootPrefersTheEnvironment(t *testing.T) {
	dir := distributionTree(t)
	getenv := func(name string) string {
		if name == RootEnv {
			return dir
		}
		return ""
	}
	root, err := FindRoot(getenv, noExecutable, "/usr/local/go")
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != dir || !root.Nanogo || root.Origin != RootEnv {
		t.Errorf("FindRoot = %+v, want the tree %s named by %s", root, dir, RootEnv)
	}
}

// TestFindRootRejectsAnEnvironmentThatIsNotADistribution is the rule
// AllowlistFromEnv states for its own variable: a mistyped path must be an
// error and never a silent fall back, because a variable that turned itself
// off reads exactly like a working build.
func TestFindRootRejectsAnEnvironmentThatIsNotADistribution(t *testing.T) {
	dir := t.TempDir()
	getenv := func(name string) string {
		if name == RootEnv {
			return dir
		}
		return ""
	}
	_, err := FindRoot(getenv, noExecutable, "/usr/local/go")
	if err == nil {
		t.Fatal("a directory with no distribution in it was accepted")
	}
	for _, want := range []string{RootEnv, dir, "VERSION", TargetDir()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %q", err, want)
		}
	}
}

func TestFindRootFindsTheTreeTheBinaryIsIn(t *testing.T) {
	dir := distributionTree(t)
	exe := func() (string, error) { return filepath.Join(dir, "bin", "nanogo"), nil }
	root, err := FindRoot(func(string) string { return "" }, exe, "/usr/local/go")
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != dir || !root.Nanogo {
		t.Errorf("FindRoot = %+v, want the distribution %s", root, dir)
	}
}

// TestFindRootFallsBackToTheGoToolchain is the step every build takes today,
// because no nanogo distribution is built.
func TestFindRootFallsBackToTheGoToolchain(t *testing.T) {
	// A binary installed by "go install" sits in GOPATH/bin, whose parent is
	// not a distribution.
	exe := func() (string, error) { return filepath.Join(t.TempDir(), "bin", "nanogo"), nil }
	root, err := FindRoot(func(string) string { return "" }, exe, "/usr/local/go")
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != "/usr/local/go" || root.Nanogo {
		t.Errorf("FindRoot = %+v, want the Go toolchain", root)
	}
	if !strings.Contains(root.Origin, "Go toolchain") {
		t.Errorf("origin %q does not say where the tree came from", root.Origin)
	}
}

func TestFindRootWithNothingToFind(t *testing.T) {
	_, err := FindRoot(func(string) string { return "" }, noExecutable, "")
	if err == nil || !strings.Contains(err.Error(), RootEnv) {
		t.Fatalf("FindRoot = %v, want it to name %s", err, RootEnv)
	}
}

func TestIsNanogoRoot(t *testing.T) {
	full := distributionTree(t)
	if !IsNanogoRoot(full) {
		t.Errorf("%s is a distribution and was not recognised", full)
	}
	// A source checkout has the sources and no archives, so a build against
	// it would have nothing to link.
	noPkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(noPkg, "VERSION"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsNanogoRoot(noPkg) {
		t.Errorf("%s has no pkg tree and was accepted", noPkg)
	}
	noVersion := t.TempDir()
	if err := os.MkdirAll(filepath.Join(noVersion, "pkg", TargetDir()), 0o755); err != nil {
		t.Fatal(err)
	}
	if IsNanogoRoot(noVersion) {
		t.Errorf("%s has no VERSION and was accepted", noVersion)
	}
	// A VERSION that is a directory is not a version.
	badVersion := t.TempDir()
	if err := os.MkdirAll(filepath.Join(badVersion, "VERSION"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(badVersion, "pkg", TargetDir()), 0o755); err != nil {
		t.Fatal(err)
	}
	if IsNanogoRoot(badVersion) {
		t.Errorf("%s has a VERSION directory and was accepted", badVersion)
	}
}
