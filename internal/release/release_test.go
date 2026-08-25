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
	"fmt"
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

// unpack opens the tarball into dir and returns the tree it unpacked to.
//
// The system tar rather than archive/tar, because what is being checked is
// that the file a user downloads opens with the tool a user has.
func unpack(t *testing.T, tarball, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarCmd, err := exec.LookPath("tar")
	skipUnless(t, err == nil, "there is no tar command: %v", err)
	run(t, "", tarCmd, "-xzf", tarball, "-C", dir)
	root := filepath.Join(dir, dist.TreeName)
	if !exists(root) {
		t.Fatalf("the tarball did not unpack to a directory called %s", dist.TreeName)
	}
	return root
}

// The whole claim, in the order a user meets it: download, unpack, run.
func TestTheTarballUnpacksIntoAWorkingDistribution(t *testing.T) {
	b := setup(t)
	work := t.TempDir()
	tarball := filepath.Join(work, "nanogo.tar.gz")
	b.build(t, filepath.Join(work, "tree"), tarball)

	root := unpack(t, tarball, filepath.Join(work, "unpacked"))

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

// buildProgram is what a person compiles first. It has a package level
// variable, a slice, a range loop and a call, so a distribution that linked but
// compiled nothing real would not print 21.
const buildProgram = `package main

var greeting = "hello from nanogo"

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

func main() {
	println(greeting)
	println(sum([]int{1, 2, 3, 4, 5, 6}))
}
`

// TestTheUnpackedNanogoBuildsAndRunsAProgram is the thing a person does with a
// downloaded compiler: unpack it, point it at a program, run what comes out.
//
// It is separate from the -toolexec test above because the two prove different
// claims. That one drives the compiler the way the go command drives it. This
// one drives "nanogo build", which resolves the package graph itself and takes
// every standard library archive out of the tree, and it is the path a
// download is useless without.
//
// The go command is still required, and the test does not hide it: go list
// resolves the program's own package and go tool link writes the executable
// (specs/045-linker.md). What must not come from it is a standard library
// archive, and the -work directory is read to prove that none did.
func TestTheUnpackedNanogoBuildsAndRunsAProgram(t *testing.T) {
	b := setup(t)
	work := t.TempDir()
	tarball := filepath.Join(work, "nanogo.tar.gz")
	b.build(t, filepath.Join(work, "tree"), tarball)
	root := unpack(t, tarball, filepath.Join(work, "unpacked"))

	// Outside the repository, in a module of its own, because a build that
	// resolved anything through nanogo's own module would not be the build a
	// user runs.
	prog := filepath.Join(work, "prog")
	if err := os.MkdirAll(prog, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ name, body string }{
		{"go.mod", "module nanogo.example/hello\n\ngo 1.27\n"},
		{"main.go", buildProgram},
	} {
		if err := os.WriteFile(filepath.Join(prog, f.name), []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// NANOGOROOT unset, so the tree is found from the binary's own path, and
	// GOROOT unset, so nothing points the build at the installed toolchain's
	// standard library. PATH stays: go list and go tool link are the go
	// command's and nothing in this release replaces them.
	nanogo := filepath.Join(root, "bin", "nanogo")
	cmd := exec.Command(nanogo, "build", "-work", "-o", "hello", ".")
	cmd.Dir = prog
	cmd.Env = environWithout(os.Environ(), "GOROOT", driver.RootEnv)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s build in %s: %v\n%s", nanogo, prog, err, out)
	}

	// The closure is measured rather than written down, for the reason the
	// tally test measures it: it moves between Go releases.
	closure, err := dist.Closure(b.goCmd, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("nanogo: 1 of %d packages compiled by nanogo; %d by gc %s (everything not named on the command line)",
		len(closure)+1, len(closure), driver.PinnedGoVersion)
	if !strings.Contains(string(out), want) {
		t.Errorf("the build did not print\n\t%s\nit printed\n%s", want, out)
	}
	if !strings.Contains(string(out), "the standard library and the runtime come from "+root) {
		t.Errorf("the build did not say the tree it used:\n%s", out)
	}
	if !strings.Contains(string(out), "go tool link") {
		t.Errorf("the build did not say who wrote the executable:\n%s", out)
	}

	// Every archive the compile read, checked against the tree. A build that
	// took one from the go command's cache would print the same summary.
	scratch := scratchDir(t, string(out))
	defer os.RemoveAll(scratch)
	cfg, err := os.ReadFile(filepath.Join(scratch, "importcfg"))
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "pkg", driver.TargetDir()) + string(filepath.Separator)
	lines := strings.Split(strings.TrimSuffix(string(cfg), "\n"), "\n")
	if len(lines) != len(closure) {
		t.Errorf("the import configuration names %d packages and the closure is %d", len(lines), len(closure))
	}
	for _, line := range lines {
		_, file, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(file, pkg) {
			t.Errorf("the import configuration reads %q, which is not an archive from %s", line, pkg)
		}
	}

	// println writes to stderr, which is where the runtime's own printing
	// goes, so the two lines are read together with the exit status.
	hello := filepath.Join(prog, "hello")
	ran, err := exec.Command(hello).CombinedOutput()
	if err != nil {
		t.Fatalf("the program the distribution built did not run: %v\n%s", err, ran)
	}
	if got, want := strings.TrimSpace(string(ran)), "hello from nanogo\n21"; got != want {
		t.Fatalf("the program printed %q, want %q", got, want)
	}
}

// scratchDir is the directory -work reported, which is the first line of the
// build's output.
func scratchDir(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if dir, ok := strings.CutPrefix(line, "WORK="); ok {
			return dir
		}
	}
	t.Fatalf("-work printed no scratch directory:\n%s", out)
	return ""
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
