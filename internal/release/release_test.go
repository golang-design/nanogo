// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package release holds the test that says whether a nanogo tarball is a
// thing a person can download and use.
//
// It builds the real tarball, unpacks it into a clean directory, and drives
// the unpacked binaries. Nothing here reaches into dist's internals: the claim
// is about the artefact, and a test that reached inside would not be testing
// that claim. specs/054-distribution.md is what it gates.
package release

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.design/x/nanogo/dist"
	"golang.design/x/nanogo/driver"
)

// required reports whether a host that cannot build or run a distribution is a
// failure rather than a reason to skip. CI sets NANOGO_REQUIRE_LINK on the
// arm64 runner, where nanogo's whole suite must run. Without the variable a
// green run would be indistinguishable from a run that built nothing.
func required() bool { return os.Getenv("NANOGO_REQUIRE_LINK") == "1" }

func skipUnless(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if ok {
		return
	}
	if required() {
		t.Fatalf("NANOGO_REQUIRE_LINK is set and "+format, args...)
	}
	t.Skipf(format, args...)
}

// repoRoot is the module root, two directories above this file.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// builder is one built pair of commands and the toolchain they were built with.
type builder struct {
	goCmd  string
	goroot string
	bin    string // the directory holding nanogo and nanogo-dist
}

// setup builds nanogo and nanogo-dist the way the release job does.
func setup(t *testing.T) *builder {
	t.Helper()
	// darwin/arm64 is nanogo's one target (specs/000-decisions.md decision 9),
	// and a tarball named for it that was packed anywhere else would be a
	// tarball nobody can run.
	skipUnless(t, runtime.GOOS == "darwin" && runtime.GOARCH == "arm64",
		"a darwin/arm64 distribution is built on darwin/arm64 and this host is %s/%s", runtime.GOOS, runtime.GOARCH)
	goCmd, err := exec.LookPath("go")
	skipUnless(t, err == nil, "there is no go command: %v", err)

	b := &builder{goCmd: goCmd, bin: t.TempDir()}
	b.goroot = strings.TrimSpace(run(t, "", goCmd, "env", "GOROOT"))
	skipUnless(t, exists(filepath.Join(b.goroot, "src", "runtime")),
		"%s holds no standard library sources to copy", b.goroot)

	for _, name := range []string{"nanogo", "nanogo-dist"} {
		// -trimpath, for the reason dist.Closure passes it to go list: without
		// it the binary carries the directory it was built in.
		run(t, repoRoot(t), goCmd, "build", "-trimpath",
			"-o", filepath.Join(b.bin, name), "golang.design/x/nanogo/cmd/"+name)
	}
	return b
}

// build makes one distribution tree and its tarball, and returns the tarball.
func (b *builder) build(t *testing.T, out, tarball string) {
	t.Helper()
	run(t, repoRoot(t), filepath.Join(b.bin, "nanogo-dist"), "build",
		"-version", "nanogo0.0.0-test",
		"-go", driver.PinnedGoVersion,
		"-goroot", b.goroot,
		"-gocmd", b.goCmd,
		"-binary", filepath.Join(b.bin, "nanogo"),
		"-self", filepath.Join(b.bin, "nanogo-dist"),
		"-license", filepath.Join(repoRoot(t), "LICENSE"),
		"-out", out,
		"-tarball", tarball)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// The whole claim, in the order a user meets it: download, unpack, run.
func TestTheTarballUnpacksIntoAWorkingDistribution(t *testing.T) {
	b := setup(t)
	work := t.TempDir()
	tarball := filepath.Join(work, "nanogo.tar.gz")
	b.build(t, filepath.Join(work, "tree"), tarball)

	// Unpacked with the system tar rather than with archive/tar, because what
	// is being checked is that the file a user downloads opens with the tool a
	// user has.
	unpacked := filepath.Join(work, "unpacked")
	if err := os.MkdirAll(unpacked, 0o755); err != nil {
		t.Fatal(err)
	}
	tarCmd, err := exec.LookPath("tar")
	skipUnless(t, err == nil, "there is no tar command: %v", err)
	run(t, "", tarCmd, "-xzf", tarball, "-C", unpacked)

	root := filepath.Join(unpacked, dist.TreeName)
	if !exists(root) {
		t.Fatalf("the tarball did not unpack to a directory called %s", dist.TreeName)
	}

	// The seam with driver, tested against the consumer's own predicate. The
	// tree is what driver.FindRoot resolves, and it resolves it from the
	// binary's own location with nothing in the environment and no Go
	// toolchain offered as a fallback.
	if !driver.IsNanogoRoot(root) {
		t.Fatalf("driver.IsNanogoRoot says %s is not a distribution", root)
	}
	exe := filepath.Join(root, "bin", "nanogo")
	got, err := driver.FindRoot(func(string) string { return "" }, func() (string, error) { return exe, nil }, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != root || !got.Nanogo {
		t.Fatalf("driver.FindRoot resolved %+v, want the unpacked tree at %s", got, root)
	}

	// The unpacked tree answers questions about itself, with no argument and
	// nothing else installed.
	distCmd := filepath.Join(root, "bin", "nanogo-dist")
	if out := run(t, work, distCmd, "verify"); !strings.Contains(out, "agrees with its 27 archives") {
		t.Errorf("verify said %q", strings.TrimSpace(out))
	}
	// 27 is the bootstrap closure, checked against the closure itself rather
	// than written down here. The tally line says "in this distribution",
	// because dist counts what is in pkg and cannot know whether that is
	// still the closure; this test can, and does.
	closure, err := dist.Closure(b.goCmd, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) != 27 {
		t.Fatalf("the bootstrap closure is %d archives and this test expects 27", len(closure))
	}
	line := strings.TrimSpace(run(t, work, distCmd, "tally"))
	want := "nanogo: 0 of 27 packages in this distribution compiled by nanogo; 27 by gc " + driver.PinnedGoVersion
	if line != want {
		t.Errorf("tally said\n\t%s\nwant\n\t%s", line, want)
	}

	// Compile and link a program whose every dependency comes out of the
	// tree, and run it.
	//
	// println and not fmt: the bootstrap closure is what a trivial program
	// needs, and fmt is not in it. The output goes to stderr, which is where
	// the runtime's own printing goes.
	prog := filepath.Join(work, "hello.go")
	const output = "hello from the unpacked distribution"
	if err := os.WriteFile(prog, []byte("package main\n\nfunc main() { println(\""+output+"\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := importcfg(t, work, root)
	mainArchive := filepath.Join(work, "main.a")

	// Driven through the unpacked bin/nanogo, in the shape the go command
	// invokes a -toolexec tool: argv[1] is the real tool's path.
	tool := filepath.Join(b.goroot, "pkg", "tool", runtime.GOOS+"_"+runtime.GOARCH)
	log := filepath.Join(work, "nanogo.log")
	compile := exec.Command(filepath.Join(root, "bin", "nanogo"),
		filepath.Join(tool, "compile"), "-p", "main", "-importcfg", cfg, "-pack", "-o", mainArchive, prog)
	// The ambient GOROOT is removed as far as the fallback allows. Every
	// archive the compile and the link read comes from the tree, named by
	// -importcfg. The two tool binaries do not, and cannot until
	// specs/045-linker.md is built.
	compile.Env = append(environWithout(os.Environ(), "GOROOT", driver.RootEnv), "NANOGO_LOG="+log)
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compiling through the unpacked nanogo: %v\n%s", err, out)
	}

	// What nanogo did, in nanogo's own words. This project has been caught
	// once by a build that succeeded because everything was delegated, so the
	// test asserts the delegation rather than inferring compilation from a
	// program that ran.
	b2, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), "delegated main") {
		t.Errorf("nanogo's log says %q; it compiles no package of this closure yet, so it must record a delegation", b2)
	}

	appendLine(t, cfg, "packagefile main="+mainArchive)
	hello := filepath.Join(work, "hello")
	run(t, work, filepath.Join(tool, "link"), "-importcfg", cfg, "-o", hello, mainArchive)
	if out := run(t, work, hello); strings.TrimSpace(out) != output {
		t.Fatalf("the program printed %q, want %q", strings.TrimSpace(out), output)
	}
}

// importcfg writes an -importcfg naming every archive in the tree and nothing
// else, so that a package resolved from anywhere but the distribution fails.
func importcfg(t *testing.T, dir, root string) string {
	t.Helper()
	pkg := filepath.Join(root, "pkg", driver.TargetDir())
	var b bytes.Buffer
	err := filepath.WalkDir(pkg, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".a" {
			return err
		}
		rel, err := filepath.Rel(pkg, p)
		if err != nil {
			return err
		}
		b.WriteString("packagefile " + strings.TrimSuffix(filepath.ToSlash(rel), ".a") + "=" + p + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "importcfg")
	if err := os.WriteFile(name, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func appendLine(t *testing.T, name, line string) {
	t.Helper()
	f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// environWithout is env with the named variables removed.
func environWithout(env []string, names ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, n := range names {
			if key == n {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// specs/053-determinism.md, applied to the distribution. The two trees are
// built at different absolute paths, because a leaked working directory is
// invisible to a comparison of two builds in one place.
func TestTheTarballIsByteIdenticalFromTwoDirectories(t *testing.T) {
	b := setup(t)
	var sum [2][]byte
	for i := range sum {
		// A directory of its own each time, at a path that differs in depth as
		// well as in name.
		work := t.TempDir()
		out := filepath.Join(work, strings.Repeat("deeper/", i+1), "tree")
		tarball := filepath.Join(work, "nanogo.tar.gz")
		b.build(t, out, tarball)
		got, err := os.ReadFile(tarball)
		if err != nil {
			t.Fatal(err)
		}
		sum[i] = got
	}
	if len(sum[0]) == 0 {
		t.Fatal("both tarballs are empty, so the comparison proves nothing")
	}
	if !bytes.Equal(sum[0], sum[1]) {
		t.Fatalf("two releases of the same tree differ: %d and %d bytes", len(sum[0]), len(sum[1]))
	}
	t.Logf("the tarball is %d bytes and byte-identical across two build directories", len(sum[0]))
}
