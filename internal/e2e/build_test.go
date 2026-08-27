// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nanogoBuild runs the command a user runs.
//
// The environment is deliberately bare. No NANOGO_ALLOWLIST, no NANOGO_LOG and
// no -toolexec: env() blanks the first two, so a build that only worked
// because the developer's own variables were set would fail here. GOCACHE is
// this test's own, so the go command builds the dependencies rather than
// replaying a cache the last run left behind.
func (h *harness) nanogoBuild(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(h.bin, append([]string{"build"}, args...)...)
	cmd.Dir = h.mod
	cmd.Env = env([]string{"GOCACHE=" + h.cache})
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The program the session compiles.
//
// It is written in the language nanogo accepts today and it uses every part of
// it that a first program reaches: arithmetic, an if, a switch, a for with
// range over a slice, a variadic call, and an import of the standard library.
// Nothing here is a construct picked to be easy; every one of them appears in
// the first twenty lines a person writes.
//
// The exit status is the assertion, as in the other end-to-end programs. A
// nanogo-compiled program cannot print: print and println lower to runtime
// calls nanogo does not emit. A wrong answer divides by zero and the process
// dies, so a zero exit means every value was the one Go defines.
const sessionProgram = `package main

import "math/bits"

func total(xs ...int) int {
	sum := 0
	for _, x := range xs {
		sum = sum + x
	}
	return sum
}

func classify(n int) int {
	switch {
	case n < 0:
		return -1
	case n == 0:
		return 0
	}
	return 1
}

func main() {
	d := total(1, 2, 3, 4) - 10
	if d != 0 {
		d = d / (d - d)
	}
	e := classify(-5) + 1
	if e != 0 {
		e = e / (e - e)
	}
	f := bits.OnesCount64(7) - 3
	if f != 0 {
		f = f / (f - f)
	}
}
`

// sessionModule is the module the session tests build.
func sessionModule() map[string]string {
	return map[string]string{
		"go.mod":  "module nanogo.example/hello\n\ngo 1.27\n",
		"main.go": sessionProgram,
	}
}

// TestBuildIsTheSessionAUserRuns is the deliverable of the front end.
//
//	nanogo build .
//	./hello
//
// No allowlist, no environment variable and no -toolexec. The name of the
// executable comes from the package, as go build derives it.
func TestBuildIsTheSessionAUserRuns(t *testing.T) {
	h := setup(t, sessionModule(), nil)

	out, err := h.nanogoBuild(t, ".")
	if err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	// The one line every build prints. A build in which nanogo compiled one
	// package of twenty-eight must not read as though nanogo built the
	// program.
	if !strings.Contains(out, "packages compiled by nanogo") {
		t.Errorf("the build did not say what it compiled:\n%s", out)
	}
	if !strings.Contains(out, "1 of ") {
		t.Errorf("the build did not count the packages nanogo compiled:\n%s", out)
	}

	prog := filepath.Join(h.mod, "hello")
	if _, err := os.Stat(prog); err != nil {
		t.Fatalf("nanogo build wrote no executable named after the package: %v", err)
	}
	if b, err := exec.Command(prog).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo built did not run: %v\n%s", err, b)
	}
}

// TestBuildTakesAFileList is the other form of the session, and the one the
// first line of a tutorial uses.
func TestBuildTakesAFileList(t *testing.T) {
	h := setup(t, sessionModule(), nil)

	if out, err := h.nanogoBuild(t, "./main.go"); err != nil {
		t.Fatalf("nanogo build ./main.go: %v\n%s", err, out)
	}
	// A file list has no import path, so the first file names the executable.
	prog := filepath.Join(h.mod, "main")
	if b, err := exec.Command(prog).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo built did not run: %v\n%s", err, b)
	}
}

func TestBuildWritesWhereMinusOSays(t *testing.T) {
	h := setup(t, sessionModule(), nil)

	if out, err := h.nanogoBuild(t, "-o", "greeter", "."); err != nil {
		t.Fatalf("nanogo build -o greeter .: %v\n%s", err, out)
	}
	if b, err := exec.Command(filepath.Join(h.mod, "greeter")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo built did not run: %v\n%s", err, b)
	}
}

// TestBuildSaysWhatItDidNotCompile is the honesty requirement, checked against
// the real binary.
//
// nanogo compiles the package the user named and nothing else. Everything
// beneath it, the standard library and the runtime, is built by the installed
// Go toolchain, and the executable is written by go tool link. A user who read
// only the output of this command must not believe otherwise.
func TestBuildSaysWhatItDidNotCompile(t *testing.T) {
	h := setup(t, sessionModule(), nil)

	out, err := h.nanogoBuild(t, "-v", ".")
	if err != nil {
		t.Fatalf("nanogo build -v .: %v\n%s", err, out)
	}
	for _, want := range []string{
		"compiling nanogo.example/hello",
		"packages compiled by nanogo",
		"the standard library and the runtime come from",
		"the installed Go toolchain",
		"go tool link",
		"nanogo has no linker",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not contain %q:\n%s", want, out)
		}
	}
}

// The program nanogo cannot compile.
//
// min over floats is refused rather than built, because the language
// propagates a NaN operand to the result and a compare and a select do not.
// The construct has to be one the pipeline still refuses, and append, which
// this program used before, is built now.
const unsupportedProgram = `package main

func smaller(a, b float64) float64 {
	return min(a, b)
}

func main() {
	d := int(smaller(2, 3)) - 2
	if d != 0 {
		d = d / (d - d)
	}
}
`

// TestBuildRefusesByNameAndWritesNothing is the failure mode the allowlist
// had, checked from the other side.
//
// A package the user named is a package the user asked nanogo to compile, so
// nanogo must not hand it to gc. If it did, this build would succeed, the
// program would run, and the success would say nothing about nanogo at all.
func TestBuildRefusesByNameAndWritesNothing(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/hello\n\ngo 1.27\n",
		"main.go": unsupportedProgram,
	}, nil)

	out, err := h.nanogoBuild(t, ".")
	if err == nil {
		t.Fatalf("nanogo build succeeded over a program it cannot compile:\n%s", out)
	}
	// The message names the function, its position and the construct, which
	// are the three things that say what to do next. The position is the
	// function's own, because that is what an ir.Func carries.
	for _, want := range []string{"function smaller", "main.go:3:6", "min"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not name %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(h.mod, "hello")); err == nil {
		t.Error("nanogo wrote an executable although it refused the package")
	}
}

// TestBuildRefusesAPackageThatImportsAnother is the export data limit, seen
// from the front end.
//
// The archive nanogo writes carries the object and no export data, so a
// package nanogo compiled cannot be imported. Compiling both in one command
// would need it, so the command says so rather than letting the linker report
// a fingerprint that does not match.
func TestBuildRefusesAPackageThatImportsAnother(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": "package lib\n\nfunc Add(a, b int) int { return a + b }\n",
		"main.go": "package main\n\nimport \"nanogo.example/two/lib\"\n\n" +
			"func main() {\n\td := lib.Add(20, 3) - 23\n\tif d != 0 {\n\t\td = d / (d - d)\n\t}\n}\n",
	}, nil)

	out, err := h.nanogoBuild(t, "./...")
	if err == nil {
		t.Fatalf("nanogo build compiled two packages where one imports the other:\n%s", out)
	}
	for _, want := range []string{"nanogo.example/two/lib", "export data"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, out)
		}
	}
}

// TestBuildCompilesAPackageThatImportsOneTheToolchainBuilt is the same module
// with only the importing package named.
//
// gc builds the library because it is a dependency and not a target, nanogo
// compiles the package that imports it, and the program runs. That is the
// division this command is built on and it is the division the report states.
func TestBuildCompilesAPackageThatImportsOneTheToolchainBuilt(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": "package lib\n\nfunc Add(a, b int) int { return a + b }\n",
		"main.go": "package main\n\nimport \"nanogo.example/two/lib\"\n\n" +
			"func main() {\n\td := lib.Add(20, 3) - 23\n\tif d != 0 {\n\t\td = d / (d - d)\n\t}\n}\n",
	}, nil)

	out, err := h.nanogoBuild(t, "-o", "two", ".")
	if err != nil {
		t.Fatalf("nanogo build -o two .: %v\n%s", err, out)
	}
	if b, err := exec.Command(filepath.Join(h.mod, "two")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo built did not run: %v\n%s", err, b)
	}
}

// TestBuildProgramPrintsATraceback reads the output of a program the front end
// built.
//
// Exiting zero says the object is well formed. This says the tables nanogo
// wrote describe the frames it built: the runtime's unwinder names both
// functions with the file and the line each one is on. panicProgram is the
// program the -toolexec tests use, so a difference here is a difference the
// front end introduced and not a difference in the compiler.
func TestBuildProgramPrintsATraceback(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/boom\n\ngo 1.27\n",
		"main.go": panicProgram,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "boom", "."); err != nil {
		t.Fatalf("nanogo build -o boom .: %v\n%s", err, out)
	}
	b, err := exec.Command(filepath.Join(h.mod, "boom")).CombinedOutput()
	if err == nil {
		t.Fatalf("the program exited zero, and it divides by zero:\n%s", b)
	}
	got := string(b)
	for _, want := range []string{
		"panic: runtime error: integer divide by zero",
		"main.ratio()",
		"main.main()",
		"main.go:7",
		"main.go:10",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the output does not contain %q:\n%s", want, got)
		}
	}
}

// TestBuildHelpStatesItsLimits checks the help of the installed binary.
//
// The three limits are the ones a user cannot discover from a successful
// build: which target, whose standard library, and who writes the executable.
func TestBuildHelpStatesItsLimits(t *testing.T) {
	h := setup(t, map[string]string{"go.mod": "module nanogo.example/none\n\ngo 1.27\n"}, nil)

	out, err := exec.Command(h.bin, "help").Output()
	if err != nil {
		t.Fatalf("nanogo help: %v", err)
	}
	for _, want := range []string{
		"nanogo build",
		"darwin/arm64",
		"The standard library and the runtime come from the installed Go",
		"toolchain. nanogo compiles neither.",
		"The executable is written by go tool link. nanogo has no linker",
		"NANOGOROOT",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("nanogo help does not state %q", want)
		}
	}
}

// TestBuildCompilesARangeLoopWithACallInIt is the regression for the move the
// register allocator asks for when a rematerialisable value is live across a
// call.
//
// The slice's base address is rematerialisable, and the call in the loop body
// is what makes it live across a call, so the allocator gives it a slot as
// well. ssagen had no arm of its move table for a value that is recomputed
// into memory and reported "no move from - to s1". nanogo refused this loop,
// which is the shape of nearly every loop that does work.
//
// The exit status carries the answer because println is not lowered and fmt
// is out of reach: its closure is 58 packages against 29 for an empty main.
func TestBuildCompilesARangeLoopWithACallInIt(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod": "module nanogo.example/loop\n\ngo 1.27\n",
		"main.go": `package main

import "os"

func add(a, b int) int { return a + b }

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n = add(n, x)
	}
	return n
}

func main() { os.Exit(sum([]int{1, 2, 3, 4, 5, 6})) }
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "loop", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	err := exec.Command(filepath.Join(h.mod, "loop")).Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("the program nanogo built did not run: %v", err)
	}
	if code != 21 {
		t.Errorf("the program exited %d, want 21: the loop summed wrongly", code)
	}
}
