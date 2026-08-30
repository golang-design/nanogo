// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package selfhost_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/internal/selfhost"
)

// The smallest Go program there is. It needs about thirty packages, because a
// Go program cannot start without a scheduler, an allocator and a collector.
const smallestProgram = "package main\n\nfunc main() {}\n"

// TestNanogoCompilesTheBootstrapClosure measures the standard library packages
// the smallest Go program needs.
//
// It is a reading and not a gate, and it has no ratchet. The closure is the
// installed Go distribution's, so it moves with every Go release: a package
// appears, a package grows an assembly file, a generic body changes shape.
// Ratcheting it would fail the build on a toolchain upgrade, which is not a
// regression in nanogo, and a gate that fires on somebody else's release is a
// gate people route around.
//
// What it replaces is folklore. This measurement was run by hand and written
// into specs/060-selfhost.md, and by the time it was re-run only one row of
// its reason table had survived. The recipe now lives in code, so the reading
// can be taken again in one command instead of being reconstructed.
//
// It runs behind its own variable rather than with the rest of the corpus. It
// is about as slow as the self-host measurement, and doubling the corpus run
// to refresh a number nothing is gated on is not a trade worth making.
func TestNanogoCompilesTheBootstrapClosure(t *testing.T) {
	if os.Getenv("NANOGO_MEASURE_CLOSURE") != "1" {
		t.Skip("a reading rather than a gate, and about as slow as the self-host measurement; " +
			"set NANOGO_MEASURE_CLOSURE=1")
	}
	hostIsTheTarget(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	root := repoRoot(t)
	work := t.TempDir()

	prog := filepath.Join(work, "prog")
	if err := os.MkdirAll(prog, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(prog, "go.mod"), "module nanogo.example/smallest\n\ngo 1.27\n")
	writeFile(t, filepath.Join(prog, "main.go"), smallestProgram)

	pkgs := closurePackages(t, prog, "nanogo.example/smallest")
	t.Logf("the closure holds %d compile invocations", len(pkgs))

	compiler := filepath.Join(work, "nanogo")
	build := exec.Command("go", "build", "-o", compiler, "./cmd/nanogo")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building nanogo: %v\n%s", err, out)
	}

	rs, err := selfhost.Measure(selfhost.Options{
		Compiler: compiler,
		Packages: pkgs,
		Dir:      prog,
		Work:     filepath.Join(work, "runs"),
		Env:      []string{"CGO_ENABLED=0"},
	})
	if err != nil {
		t.Fatalf("measuring: %v", err)
	}
	t.Logf("%s", selfhost.Table(rs))

	// A package nanogo was never asked about proves nothing, and a delegated
	// one means the allowlist did not name it. Both are faults in the
	// measurement rather than facts about the compiler, so both fail while a
	// refusal does not.
	for _, p := range selfhost.WithDecision(rs, selfhost.NotReached) {
		t.Errorf("nanogo was never asked about %s, so this run proved nothing about it", p)
	}
	for _, p := range selfhost.WithDecision(rs, selfhost.Delegated) {
		t.Errorf("nanogo delegated %s to gc, so the allowlist did not name it", p)
	}
	if selfhost.Count(rs, selfhost.Compiled) == 0 {
		t.Error("nanogo compiled none of the closure, which is a broken measurement rather than a reading")
	}
}

// closurePackages is what the smallest program needs, from the go command.
//
// unsafe is dropped: it has no archive and no compile invocation, so a
// denominator that counted it would report a package nanogo can never be
// asked about as one it failed to compile. The main package is kept, because
// nanogo does compile it and specs/060-selfhost.md counts it.
func closurePackages(t *testing.T, dir string, main string) []selfhost.Package {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	var pkgs []selfhost.Package
	for _, p := range strings.Fields(string(out)) {
		if p == "unsafe" {
			continue
		}
		if p == main {
			// The go command passes -p main for a main package whatever its
			// import path, so nanogo is told it is called main and that is
			// what the allowlist has to say.
			pkgs = append(pkgs, selfhost.MainPackage(p))
			continue
		}
		pkgs = append(pkgs, selfhost.Paths(p)...)
	}
	return pkgs
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
