// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package e2e holds the test that says whether nanogo is a compiler a person
// can use.
//
// Every other test in this repository drives one stage from Go test code. This
// one installs the binary, runs a real `go build -toolexec=nanogo ./...`, and
// runs the program that comes out. Nothing here calls into nanogo's packages,
// on purpose: the claim is about the command line and the build, and a test
// that reached inside would not be testing that claim.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// requireHostCanRunOutput reports whether a host that cannot run nanogo's
// output is a failure rather than a reason to skip.
//
// CI sets NANOGO_REQUIRE_LINK on the arm64 runner only, so these tests are
// required there and skipped elsewhere. Without the variable a green run would
// be indistinguishable from a run that built nothing. ssagen/ssagen_test.go
// uses the same shape, for the same reason.
func requireHostCanRunOutput() bool { return os.Getenv("NANOGO_REQUIRE_LINK") == "1" }

// hostRunsNanogoOutput guards every test that compiles with nanogo and runs
// the result.
//
// nanogo emits arm64 machine code and has no second backend
// (specs/000-decisions.md decision 9 makes darwin/arm64 first and linux/amd64
// second, and specs/043-amd64-backend.md is unbuilt). On any other host the
// driver refuses to emit, and a build that reached the linker anyway would
// produce a binary that dies as soon as the runtime walks its pc tables.
func hostRunsNanogoOutput(t *testing.T) {
	t.Helper()
	if runtime.GOARCH == "arm64" {
		return
	}
	if requireHostCanRunOutput() {
		t.Fatalf("NANOGO_REQUIRE_LINK is set and GOARCH is %s; nanogo emits arm64 and cannot be run here", runtime.GOARCH)
	}
	t.Skipf("nanogo emits arm64 machine code and GOARCH is %s, so nothing it compiles can run here", runtime.GOARCH)
}

func goTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		if requireHostCanRunOutput() {
			t.Fatalf("NANOGO_REQUIRE_LINK is set and there is no go command: %v", err)
		}
		t.Skipf("no go command: %v", err)
	}
	return p
}

// harness is one temporary installation of nanogo and one module to build with
// it.
type harness struct {
	goCmd string
	dir   string // the root of the temporary tree
	mod   string // the module directory
	bin   string // the nanogo binary
	list  string // the allowlist file
	log   string // the file nanogo records its decisions in
	cache string // a build cache of this test's own
}

// setup builds nanogo the way a user installs it and writes the module.
func setup(t *testing.T, files map[string]string, allow []string) *harness {
	t.Helper()
	hostRunsNanogoOutput(t)
	h := &harness{goCmd: goTool(t), dir: t.TempDir()}
	h.mod = filepath.Join(h.dir, "mod")
	h.bin = filepath.Join(h.dir, "nanogo")
	h.list = filepath.Join(h.dir, "allowlist")
	h.log = filepath.Join(h.dir, "nanogo.log")
	// A cache of this test's own, so that the build compiles rather than
	// replaying an object the developer's last run left behind. The go command
	// caches per compiler identity, and a cached result would make the test
	// pass while nanogo did nothing.
	h.cache = filepath.Join(h.dir, "gocache")

	build := exec.Command(h.goCmd, "build", "-o", h.bin, "golang.design/x/nanogo/cmd/nanogo")
	build.Dir = repoRoot(t)
	build.Env = env(nil)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/nanogo: %v\n%s", err, out)
	}

	for name, body := range files {
		full := filepath.Join(h.mod, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(h.list, []byte(strings.Join(allow, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// repoRoot is the module root, which is two directories above this file.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// env keeps the go command off the network and away from the developer's own
// allowlist, which would change what these tests measure.
func env(extra []string) []string {
	out := append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=", "GO111MODULE=on", "GOPROXY=off")
	for i, kv := range out {
		if strings.HasPrefix(kv, "NANOGO_ALLOWLIST=") || strings.HasPrefix(kv, "NANOGO_LOG=") {
			out[i] = strings.SplitN(kv, "=", 2)[0] + "="
		}
	}
	return append(out, extra...)
}

// build runs the invocation specs/051-build-integration.md names, verbatim.
func (h *harness) build(t *testing.T, args ...string) (string, error) {
	t.Helper()
	all := append([]string{"build", "-toolexec=" + h.bin}, args...)
	cmd := exec.Command(h.goCmd, all...)
	cmd.Dir = h.mod
	cmd.Env = env([]string{
		"NANOGO_ALLOWLIST=" + h.list,
		"NANOGO_LOG=" + h.log,
		"GOCACHE=" + h.cache,
	})
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// decisions reads what nanogo recorded about each compile invocation.
func (h *harness) decisions(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatalf("nanogo wrote no log, so there is no evidence of what it compiled: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// compiled reports whether nanogo compiled the named package itself.
func compiled(lines []string, pkg string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, "compiled "+pkg+" ") {
			return true
		}
	}
	return false
}

// The program nanogo compiles.
//
// It is a main package with no imports, and that is not a simplification. A
// package nanogo compiles cannot be imported, because the archive it writes
// carries no export data (specs/015-export-data.md has no writer), and a
// package nanogo compiles cannot import, because there is no reader either. A
// main package is the one package in a build that neither imports nor is
// imported, so it is the only one a whole `go build` can hand to nanogo today.
//
// The body avoids an assignment statement, a short variable declaration and a
// call nested in another expression, because SSA construction builds none of
// them yet. What is left is enough for arithmetic, branches and calls.
const helloProgram = `package main

func compute(a, b int) int { return a*b + 1 }

func main() { compute(20, 3) }
`

// The program that prints.
//
// A divide by zero is the only output a nanogo-compiled program can produce
// today: print and println lower to runtime calls nanogo does not emit, and an
// assembly helper would need an ABI wrapper that nanogo does not generate. The
// panic goes through runtime.panicdivide, which the rules already emit, and the
// traceback the runtime prints is read back below.
//
// The division appears in two branches because the runtime's unwinder reads
// the pc-sp value at the return address of the call to runtime.panicdivide. gc
// puts a padding instruction after a call that does not return so that the
// return address stays inside the caller's frame range. nanogo's ssagen does
// not, so a function whose only trapping call is in its last block unwinds
// into the stack-growth tail and the runtime throws instead of printing the
// panic. With a second branch the return address lands in the block that
// follows and the traceback is correct.
const panicProgram = `package main

func ratio(a, b, c int) int {
	if a > b {
		return a / c
	}
	return b / c
}

func main() { ratio(1, 2, 0) }
`

// TestToolexecCompilesAndRuns is the deliverable.
//
// A real go build, with nanogo substituted for the compiler, over a module
// nobody wrote for a test harness. nanogo compiles the main package, gc
// compiles the standard library beneath it, the real linker joins them, and the
// program runs.
func TestToolexecCompilesAndRuns(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/hello\n\ngo 1.27\n",
		"main.go": helloProgram,
	}, []string{"# the one package nanogo owns in this build", "main"})

	out, err := h.build(t, "./...")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo ./...: %v\n%s", err, out)
	}

	lines := h.decisions(t)
	if !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package instead of compiling it:\n%s", strings.Join(lines, "\n"))
	}
	// Every other package in the build is the standard library, and nanogo
	// must have handed all of them to gc.
	for _, line := range lines {
		if strings.HasPrefix(line, "failed ") {
			t.Errorf("a package nanogo owns failed: %s", line)
		}
	}
	t.Logf("nanogo compiled %d packages and delegated %d", count(lines, "compiled "), count(lines, "delegated "))

	prog := filepath.Join(h.mod, "hello")
	if b, err := exec.Command(prog).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled did not run: %v\n%s", err, b)
	}
}

// TestToolexecProgramPrints reads the output of a nanogo-compiled program.
//
// Running and exiting zero says the object is well formed. This says the code
// nanogo generated computed something and that the tables it wrote describe
// the frames it built: the traceback names both functions, with the file and
// the line each one is on.
func TestToolexecProgramPrints(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/boom\n\ngo 1.27\n",
		"main.go": panicProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "boom", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	b, err := exec.Command(filepath.Join(h.mod, "boom")).CombinedOutput()
	if err == nil {
		t.Fatalf("the program exited zero, and it divides by zero:\n%s", b)
	}
	got := string(b)
	t.Logf("the program printed:\n%s", got)
	for _, want := range []string{
		"panic: runtime error: integer divide by zero",
		"main.ratio()",
		"main.main()",
		"main.go:7", // the division the program took: a is not greater than b
		"main.go:10",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the output does not contain %q:\n%s", want, got)
		}
	}
}

// TestToolexecImportIsRefused states the limit that decides everything else.
//
// nanogo has no reader for gc's export data, so a package with an import fails
// rather than compiling something that does not know what the import declares.
// The message names the import and says why, because a user who sees it needs
// to know that it is a missing component and not a broken build.
func TestToolexecImportIsRefused(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/imports\n\ngo 1.27\n",
		"main.go": "package main\n\nimport \"strconv\"\n\nfunc main() { println(strconv.IntSize) }\n",
	}, []string{"main"})

	out, err := h.build(t, ".")
	if err == nil {
		t.Fatalf("the build succeeded although nanogo cannot read export data:\n%s", out)
	}
	for _, want := range []string{"strconv", "export data"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, out)
		}
	}
	lines := h.decisions(t)
	if len(lines) == 0 || !strings.HasPrefix(lines[len(lines)-1], "failed main ") {
		t.Errorf("the log does not record the failure:\n%s", strings.Join(lines, "\n"))
	}
}

// TestToolexecDelegatesWhatItDoesNotOwn is the other half of the substitution.
//
// With an allowlist that names no package in this module, the build must still
// work, and it must work through gc. This is the state a checkout is in, so a
// regression here breaks every build that has nanogo on the tool path.
func TestToolexecDelegatesWhatItDoesNotOwn(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/mixed\n\ngo 1.27\n",
		"greet/g.go": "package greet\n\nfunc Message() string { return \"nanogo delegated this\" }\n",
		"main.go":    "package main\n\nimport (\n\t\"fmt\"\n\n\t\"nanogo.example/mixed/greet\"\n)\n\nfunc main() { fmt.Println(greet.Message()) }\n",
	}, []string{"# nothing in this module", "nanogo.example/other"})

	if out, err := h.build(t, "-o", "mixed", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if compiled(lines, "main") || compiled(lines, "nanogo.example/mixed/greet") {
		t.Errorf("nanogo compiled a package it does not own:\n%s", strings.Join(lines, "\n"))
	}
	if count(lines, "delegated ") == 0 {
		t.Error("nanogo recorded no delegation, so the log proves nothing")
	}

	b, err := exec.Command(filepath.Join(h.mod, "mixed")).CombinedOutput()
	if err != nil {
		t.Fatalf("the built program failed: %v\n%s", err, b)
	}
	if got := strings.TrimSpace(string(b)); got != "nanogo delegated this" {
		t.Errorf("the program printed %q", got)
	}
}

// TestInstalledBinaryAnswersTheProtocols checks the two command lines a user
// and the go command each send to an installed nanogo.
func TestInstalledBinaryAnswersTheProtocols(t *testing.T) {
	h := setup(t, map[string]string{"go.mod": "module nanogo.example/none\n\ngo 1.27\n"}, nil)

	// The go command's protocol. cmd/go/internal/work/buildid.go needs three
	// or more fields, field 0 equal to the tool's name and field 1 equal to
	// "version", and a malformed line is fatal with no fallback.
	out, err := exec.Command(h.bin, "compile", "-V=full").Output()
	if err != nil {
		t.Fatalf("nanogo compile -V=full: %v", err)
	}
	f := strings.Fields(string(out))
	if len(f) < 3 || f[0] != "compile" || f[1] != "version" {
		t.Errorf("the go command rejects %q", out)
	}

	// A person's. It names the host, because the host is what decides whether
	// nanogo can compile anything at all.
	out, err = exec.Command(h.bin, "version").Output()
	if err != nil {
		t.Fatalf("nanogo version: %v", err)
	}
	if !strings.HasPrefix(string(out), "nanogo ") || !strings.Contains(string(out), runtime.GOARCH) {
		t.Errorf("nanogo version printed %q", out)
	}

	out, err = exec.Command(h.bin, "help").Output()
	if err != nil {
		t.Fatalf("nanogo help: %v", err)
	}
	// The help states the limits. A user must not have to discover them by
	// hitting them, so the test names the ones that stop a real build.
	for _, want := range []string{"darwin/arm64", "export data", "allowlist", "assignment statement", "gc"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("nanogo help does not mention %q", want)
		}
	}
}

func count(lines []string, prefix string) int {
	n := 0
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}
