// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// needGo skips a test that cannot run without the go command. The end to end
// tests below are M0's gate, so they run the real go command and nothing else
// stands in for it.
func needGo(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("the end to end tests run the go command")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	return goBin
}

// buildNanogo builds cmd/nanogo into dir and reports the path.
func buildNanogo(t *testing.T, goBin, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "nanogo")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command(goBin, "build", "-o", bin, ".")
	cmd.Dir = sourceDir(t)
	cmd.Env = buildEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building nanogo: %v\n%s", err, out)
	}
	return bin
}

// sourceDir is the directory of this test, which is cmd/nanogo.
func sourceDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// buildEnv keeps the go command off the network and away from the developer's
// own allowlist.
func buildEnv() []string {
	env := append(os.Environ(),
		"GOTOOLCHAIN=local",
		"GOFLAGS=",
		"GO111MODULE=on",
	)
	// The allowlist decides which packages nanogo claims. A value inherited
	// from the developer's shell would change what these tests measure.
	for i, kv := range env {
		if strings.HasPrefix(kv, "NANOGO_ALLOWLIST=") {
			env[i] = "NANOGO_ALLOWLIST="
		}
	}
	return env
}

// writeModule writes a module with a main package and one library package.
// It has no dependency outside the standard library, so no download happens.
func writeModule(t *testing.T, dir string) {
	t.Helper()
	write := func(name, data string) {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogotest\n\ngo 1.21\n")
	write("greet/greet.go", "package greet\n\n// Message is what the built program prints.\nfunc Message() string { return \"nanogo toolexec works\" }\n")
	write("main.go", "package main\n\nimport (\n\t\"fmt\"\n\n\t\"nanogotest/greet\"\n)\n\nfunc main() { fmt.Println(greet.Message()) }\n")
}

// TestVersionFull checks the protocol the go command applies to -V=full
// output, with the same condition cmd/go/internal/work/buildid.go applies.
func TestVersionFull(t *testing.T) {
	goBin := needGo(t)
	bin := buildNanogo(t, goBin, t.TempDir())

	const tool = "compile"
	out, err := exec.Command(bin, tool, "-V=full").Output()
	if err != nil {
		t.Fatalf("%s %s -V=full: %v", bin, tool, err)
	}
	line := string(out)
	f := strings.Fields(line)
	if len(f) < 3 || f[0] != tool || f[1] != "version" ||
		strings.Contains(f[2], "devel") && !strings.HasPrefix(f[len(f)-1], "buildID=") {
		t.Fatalf("the go command rejects the -V=full output of %s:\n\t%s", tool, line)
	}
	if !strings.Contains(line, "X:nanogo-") {
		t.Errorf("%q carries no nanogo identity, so the build cache cannot notice a nanogo change", line)
	}
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Errorf("standard output is not exactly one line: %q", line)
	}
}

// TestVersionFullOtherTools checks that the assembler and the linker keep
// their own build ID. nanogo passes them through, so an answer from nanogo
// would freeze their cache keys at nanogo's pinned release.
func TestVersionFullOtherTools(t *testing.T) {
	goBin := needGo(t)
	bin := buildNanogo(t, goBin, t.TempDir())

	toolDir, err := exec.Command(goBin, "env", "GOTOOLDIR").Output()
	if err != nil {
		t.Fatalf("go env GOTOOLDIR: %v", err)
	}
	for _, tool := range []string{"asm", "link"} {
		path := filepath.Join(strings.TrimSpace(string(toolDir)), tool)
		if _, err := os.Stat(path); err != nil {
			t.Skipf("no %s in GOTOOLDIR", tool)
		}
		out, err := exec.Command(bin, path, "-V=full").Output()
		if err != nil {
			t.Fatalf("%s %s -V=full: %v", bin, path, err)
		}
		if strings.Contains(string(out), "nanogo") {
			t.Errorf("nanogo answered for %s: %q", tool, out)
		}
		if f := strings.Fields(string(out)); len(f) < 3 || f[0] != tool || f[1] != "version" {
			t.Errorf("the go command rejects the -V=full output of %s:\n\t%s", tool, out)
		}
	}
}

// TestToolexecPassthrough is M0's gate: a real go build with -toolexec=nanogo
// completes, and the program it produces runs.
func TestToolexecPassthrough(t *testing.T) {
	goBin := needGo(t)
	dir := t.TempDir()
	bin := buildNanogo(t, goBin, dir)

	mod := filepath.Join(dir, "mod")
	writeModule(t, mod)

	out := filepath.Join(dir, "hello")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	// The invocation specs/051-build-integration.md names, verbatim.
	all := exec.Command(goBin, "build", "-toolexec="+bin, "./...")
	all.Dir = mod
	all.Env = buildEnv()
	if o, err := all.CombinedOutput(); err != nil {
		t.Fatalf("go build -toolexec=nanogo ./...: %v\n%s", err, o)
	}

	build := exec.Command(goBin, "build", "-toolexec="+bin, "-o", out, ".")
	build.Dir = mod
	build.Env = buildEnv()
	if o, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, o)
	}

	o, err := exec.Command(out).CombinedOutput()
	if err != nil {
		t.Fatalf("running the built program: %v\n%s", err, o)
	}
	if got := strings.TrimSpace(string(o)); got != "nanogo toolexec works" {
		t.Errorf("the built program printed %q, want %q", got, "nanogo toolexec works")
	}
}

// TestToolexecAllowlisted proves the selection wiring runs in the real binary.
//
// The package on the allowlist holds a construct nanogo cannot compile, so the
// build stops. What is checked is that it stops in nanogo, naming the package
// and the construct: an allowlist entry is a claim that nanogo owns the
// package, so a construct nanogo cannot compile is an error and never a silent
// hand back to gc.
//
// The construct is a conversion to an interface, which ssa.Build refuses. It
// has been two constructs before: the string constant Message returns, which
// needed a data symbol nothing wrote, and a floating-point constant, which
// specs/042-arm64-backend.md group 6 had no encoder for. Both gaps are closed.
// What this test asserts is where the build stops and what the message names,
// so the construct is whichever one is open and never the point.
func TestToolexecAllowlisted(t *testing.T) {
	goBin := needGo(t)
	dir := t.TempDir()
	bin := buildNanogo(t, goBin, dir)

	mod := filepath.Join(dir, "mod")
	writeModule(t, mod)
	if err := os.WriteFile(filepath.Join(mod, "greet", "refuse.go"),
		[]byte("package greet\n\nfunc boxed() any { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	list := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(list, []byte("# one package\nnanogotest/greet\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	build := exec.Command(goBin, "build", "-toolexec="+bin, "-o", filepath.Join(dir, "hello"), ".")
	build.Dir = mod
	build.Env = append(buildEnv(), "NANOGO_ALLOWLIST="+list)
	o, err := build.CombinedOutput()
	if err == nil {
		t.Fatalf("go build succeeded although nanogo owns a package:\n%s", o)
	}
	if !strings.Contains(string(o), "nanogotest/greet") {
		t.Errorf("the failure does not name the package:\n%s", o)
	}
	if !strings.Contains(string(o), "cannot compile") {
		t.Errorf("the failure does not say what is missing:\n%s", o)
	}
}

// TestPassthroughExitStatus checks that the go command sees the real tool's
// status. nanogo replaces its own process image on unix, so the status is the
// tool's own and not a translation of it.
func TestPassthroughExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test drives a shell")
	}
	goBin := needGo(t)
	bin := buildNanogo(t, goBin, t.TempDir())

	t.Run("exit code", func(t *testing.T) {
		err := exec.Command(bin, "/bin/sh", "-c", "exit 7").Run()
		var exit *exec.ExitError
		if !asExitError(err, &exit) {
			t.Fatalf("err = %v, want an exit error", err)
		}
		if got := exit.ExitCode(); got != 7 {
			t.Errorf("exit code = %d, want 7", got)
		}
	})

	t.Run("standard streams", func(t *testing.T) {
		cmd := exec.Command(bin, "/bin/sh", "-c", "cat; echo err >&2")
		cmd.Stdin = strings.NewReader("in\n")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if string(out) != "in\n" {
			t.Errorf("stdout = %q, want %q", out, "in\n")
		}
		if stderr.String() != "err\n" {
			t.Errorf("stderr = %q, want %q", stderr.String(), "err\n")
		}
	})

	t.Run("signal", func(t *testing.T) {
		err := exec.Command(bin, "/bin/sh", "-c", "kill -TERM $$").Run()
		var exit *exec.ExitError
		if !asExitError(err, &exit) {
			t.Fatalf("err = %v, want an exit error", err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok {
			t.Skip("no wait status on this platform")
		}
		// A wrapper that forked and translated the status could only report
		// an ordinary non-zero exit here.
		if !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Errorf("wait status = %v, want death by SIGTERM", exit)
		}
	})
}

func asExitError(err error, dst **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*dst = e
	}
	return ok
}
