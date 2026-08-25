// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// goCommand is the go command, or a skip. CI sets NANOGO_REQUIRE_LINK on the
// runner where nanogo's whole suite must run, and there a missing go command
// is a failure rather than a reason to report a green run that measured
// nothing. internal/e2e uses the same shape.
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

// The bootstrap closure, measured rather than quoted. The denominator in the
// tally line comes from this number, so a change in it is a change in every
// claim the distribution makes about itself.
func TestClosureIsTheSmallestProgramsDependencies(t *testing.T) {
	goCmd := goCommand(t)
	pkgs, err := Closure(goCmd, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	// Not pinned to an exact count: the closure moves between Go releases, and
	// a gate that failed on a Go upgrade would be a gate people switch off.
	// The floor is the claim: a trivial program pulls in a lot.
	if len(pkgs) < 20 {
		t.Fatalf("the closure is %d packages, and a Go program's floor is far above that: %v", len(pkgs), pkgs)
	}
	t.Logf("the bootstrap closure holds %d archives on %s/%s", len(pkgs), runtime.GOOS, runtime.GOARCH)

	var seen bool
	for _, p := range pkgs {
		if p.Path == "runtime" {
			seen = true
		}
		if p.Path == "unsafe" || p.Path == closureModule {
			t.Errorf("%s has no archive and must not be in the closure", p.Path)
		}
		b, err := os.ReadFile(p.Archive)
		if err != nil {
			t.Fatalf("%s: %v", p.Path, err)
		}
		// Every archive the release job installs must be readable as one, and
		// must name the release it was built by, because that is what VERSION
		// is checked against.
		if _, err := ToolchainVersion(b); err != nil {
			t.Fatalf("%s: %v", p.Path, err)
		}
		// None of them carries a producer record yet. gc does not write one,
		// so Build stamps them, and the day nanogo compiles one of these this
		// assertion is what says so.
		if _, err := ReadRecord(b); err == nil {
			t.Fatalf("%s already carries a producer record, which gc does not write", p.Path)
		}
	}
	if !seen {
		t.Error("the closure has no runtime, so it is not a Go program's closure")
	}
}

func TestClosureReportsAFailingGoCommand(t *testing.T) {
	if _, err := Closure("this-go-command-does-not-exist", "darwin", "arm64"); err == nil {
		t.Fatal("a go command that is not there was accepted")
	}
	// A target the go command refuses, so the failure comes back through the
	// same path a real error would.
	if _, err := Closure(goCommand(t), "darwin", "vax"); err == nil {
		t.Fatal("an unknown GOARCH was accepted")
	}
}
