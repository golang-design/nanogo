// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.design/x/nanogo/driver"
)

// goCommand is the go command, or a skip. CI sets NANOGO_REQUIRE_LINK where
// nanogo's whole suite must run, and there a missing go command is a failure
// rather than a reason to report a green run that measured nothing.
func goCommand(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_LINK") == "1" {
			t.Fatalf("NANOGO_REQUIRE_LINK is set and there is no go command: %v", err)
		}
		t.Skipf("no go command: %v", err)
	}
	return p
}

// fakeGoroot is a GOROOT with the three things CopySource reads and nothing
// else, so that a test covers the whole build path in seconds rather than
// copying 57 MB of standard library.
func fakeGoroot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"LICENSE":                 "Go's licence\n",
		"PATENTS":                 "Go's patent grant\n",
		"src/internal/abi/abi.go": "package abi\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// file writes a file and returns its path.
func file(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// call runs one command and returns its code, stdout and stderr.
func call(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no arguments", nil, 2, "usage:"},
		{"an unknown command", []string{"frobnicate"}, 2, `unknown command "frobnicate"`},
		{"help", []string{"help"}, 0, "nanogo-dist tally"},
		{"pin", []string{"pin"}, 0, driver.PinnedGoVersion},
		{"a bad flag", []string{"build", "-nonesuch"}, 1, ""},
		{"build with no output", []string{"build"}, 1, "needs -out and -binary"},
		{"build with no goroot", []string{"build", "-out", "x", "-binary", "y"}, 1, "needs -goroot"},
		{"tally with two roots", []string{"tally", "a", "b"}, 1, "one root at a time"},
		{"verify with no tree", []string{"verify", "nonesuch"}, 1, "nonesuch"},
		{"tally with no tree", []string{"tally", "nonesuch"}, 1, "nonesuch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errs := call(c.args...)
			if code != c.code {
				t.Errorf("exit code %d, want %d (%s%s)", code, c.code, out, errs)
			}
			if !strings.Contains(out+errs, c.want) {
				t.Errorf("output was %q, want it to contain %q", out+errs, c.want)
			}
		})
	}
}

// The whole build path, over a GOROOT small enough to make it quick. The
// closure is real, because the archives are what the tally counts.
func TestBuildWritesATreeATallyAndATarball(t *testing.T) {
	goCmd := goCommand(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "tree")
	tarball := filepath.Join(dir, "nanogo.tar.gz")
	code, stdout, stderr := call("build",
		"-version", "nanogo0.0.0-test",
		"-go", goVersion(t, goCmd),
		"-goroot", fakeGoroot(t),
		"-gocmd", goCmd,
		"-target", runtime.GOOS+"_"+runtime.GOARCH,
		"-binary", file(t, dir, "nanogo", "binary", 0o755),
		"-self", file(t, dir, "nanogo-dist", "tool", 0o755),
		"-license", file(t, dir, "LICENSE", "nanogo's licence\n", 0o644),
		"-out", out,
		"-tarball", tarball)
	if code != 0 {
		t.Fatalf("build failed with %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "packages in this distribution compiled by nanogo") {
		t.Errorf("build did not print the tally: %q", stdout)
	}
	if !strings.Contains(stdout, ".tar.gz") {
		t.Errorf("build did not name the tarball: %q", stdout)
	}
	fi, err := os.Stat(tarball)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Fatal("the tarball is empty")
	}

	// The tree answers for itself, by path and by the running binary's own
	// location.
	if code, stdout, stderr = call("verify", out); code != 0 {
		t.Fatalf("verify failed with %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "agrees with its") {
		t.Errorf("verify said %q", stdout)
	}
	if code, stdout, stderr = call("tally", out); code != 0 {
		t.Fatalf("tally failed with %d: %s%s", code, stdout, stderr)
	}
	if !strings.HasPrefix(stdout, "nanogo: ") {
		t.Errorf("tally said %q", stdout)
	}
}

func TestBuildReportsAFailingClosure(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := call("build",
		"-goroot", fakeGoroot(t), "-gocmd", "this-go-command-does-not-exist",
		"-binary", file(t, dir, "nanogo", "binary", 0o755),
		"-out", filepath.Join(dir, "tree"))
	if code != 1 || !strings.Contains(stderr, "go list") {
		t.Fatalf("build gave %d and %q%q, want a failure naming go list", code, stdout, stderr)
	}
}

func TestBuildReportsAFailingTree(t *testing.T) {
	goCmd := goCommand(t)
	dir := t.TempDir()
	// No -version, so the tarball has no name and dist.Build refuses.
	code, _, stderr := call("build",
		"-go", goVersion(t, goCmd),
		"-goroot", fakeGoroot(t), "-gocmd", goCmd,
		"-binary", file(t, dir, "nanogo", "binary", 0o755),
		"-license", file(t, dir, "LICENSE", "x\n", 0o644),
		"-out", filepath.Join(dir, "tree"))
	if code != 1 || !strings.Contains(stderr, "release") {
		t.Fatalf("build gave %d and %q, want a failure about the missing release", code, stderr)
	}
}

// With no root, tally reads the tree the running binary is installed in. The
// test binary is not in one, so the answer is an error naming that tree, which
// is what a user outside a distribution should see.
func TestRootOfFallsBackToTheRunningBinary(t *testing.T) {
	got, err := rootOf(nil)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Dir(filepath.Dir(exe)); got != want {
		t.Fatalf("rootOf(nil) = %q, want %q", got, want)
	}
	if code, _, stderr := call("tally"); code != 1 || stderr == "" {
		t.Fatalf("tally outside a distribution gave %d and %q", code, stderr)
	}
}

func TestDefaultTargetIsTheHost(t *testing.T) {
	if got, want := defaultTarget(), runtime.GOOS+"_"+runtime.GOARCH; got != want {
		t.Fatalf("defaultTarget = %q, want %q", got, want)
	}
	goos, goarch, ok := cutTarget("darwin_arm64")
	if !ok || goos != "darwin" || goarch != "arm64" {
		t.Fatalf("cutTarget = %q, %q, %v", goos, goarch, ok)
	}
	if _, _, ok := cutTarget("darwin"); ok {
		t.Fatal("cutTarget accepted a target with no architecture")
	}
}

func TestWriteTarballReportsWhatItCannotWrite(t *testing.T) {
	if err := writeTarball(filepath.Join(t.TempDir(), "no", "such", "dir", "x.tar.gz"), t.TempDir(), "nanogo"); err == nil {
		t.Error("a tarball in a directory that is not there was accepted")
	}
	// A tree with a symlink, which dist refuses to pack.
	dir := t.TempDir()
	file(t, dir, "real", "x", 0o644)
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Skipf("this file system has no symlinks: %v", err)
	}
	if err := writeTarball(filepath.Join(t.TempDir(), "x.tar.gz"), dir, "nanogo"); err == nil {
		t.Error("a tree holding a symlink was packed")
	}
}

// goVersion is the release the go command reports, so that the fake GOROOT's
// archives, which come from the real toolchain, match what VERSION pins.
func goVersion(t *testing.T, goCmd string) string {
	t.Helper()
	out, err := exec.Command(goCmd, "env", "GOVERSION").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
