// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The conditional set, from Go source to a process that exits.
//
// Every other test of this operation stops at bytes: ssa/macharm64_test.go
// asserts that ARM64Encode reaches an encoder, and obj/arm64 compares the
// bytes against go tool asm. Neither says that the instruction computes the
// answer the Go program asks for, because neither runs it. This does.
//
// It is here rather than in internal/e2e or ssagen because the bridge under
// test is ssa/macharm64.go: the operation, the condition it carries in Aux,
// and the one switch that turns the pair into an instruction. The harness
// duplicates a few lines of internal/e2e's, which is the price of the test
// living next to the code it is about.
//
// The condition matters as much as the instruction. A comparison lowered to a
// branch and the same comparison lowered to a bool must read the same
// condition code, and specs/042 group 6 records what happens when a family
// gets that wrong: a float comparison needs MI and LS where an integer one
// needs LT and LE, because LT and LE are true in the unordered row. The
// program below asks for both outcomes of every integer condition the rules
// produce, and the disassembly check names them.

// arm64Host guards a test that compiles with nanogo and runs the result.
//
// nanogo emits arm64 machine code and has no second back end, so on any other
// host there is nothing to run. NANOGO_REQUIRE_LINK turns the skip into a
// failure on the runner that is meant to run it, which is what keeps a green
// run from being indistinguishable from a run that built nothing.
// internal/e2e and ssagen guard the same way, for the same reason.
func arm64Host(t *testing.T) {
	t.Helper()
	if runtime.GOARCH == "arm64" {
		return
	}
	if os.Getenv("NANOGO_REQUIRE_LINK") == "1" {
		t.Fatalf("NANOGO_REQUIRE_LINK is set and GOARCH is %s; nanogo emits arm64 and cannot be run here",
			runtime.GOARCH)
	}
	t.Skipf("nanogo emits arm64 machine code and GOARCH is %s, so nothing it compiles can run here",
		runtime.GOARCH)
}

// condProgram is the program under test.
//
// classify is the function that found the gap, and the reason it found it is
// worth naming. A switch with no tag is a switch on true (ir/build.go says so),
// so each case expression is compared for equality against the constant true.
// The case expression is therefore an operand of another comparison and not a
// block control, and the fold of specs/042 group 4 that turns a comparison
// into a conditional branch cannot take it: an operand has to be a value in a
// register. The disassembly of classify shows the pair, a CSET for the case
// expression and a B.cond for the equality against true. That is the shape
// every comparison-as-a-value takes, and the target had no encoding for it.
//
// signed and unsigned cover the six conditions ssa/rules/arm64.go's condOf
// produces, in both of their outcomes. specs/021 rewrites x > y as y < x, so
// there is no separate greater-than to ask for: LT, LE, EQ, NE cover the
// signed spellings and LO, LS the unsigned ones. Each case contributes a
// distinct power of two, so a wrong answer cannot cancel another wrong answer.
//
// The exit status is the assertion. A wrong total divides by zero, the runtime
// panics through runtime.panicdivide, and the process dies. print and println
// lower to runtime calls nanogo does not emit, so a division is the only
// output a nanogo-compiled program has (internal/e2e says the same).
const condProgram = `package main

func classify(n int) int {
	switch {
	case n < 0:
		return -1
	case n == 0:
		return 0
	}
	return 1
}

func signed(a, b int) int {
	n := 0
	switch {
	case a < b:
		n = n + 1
	}
	switch {
	case a <= b:
		n = n + 2
	}
	switch {
	case a == b:
		n = n + 4
	}
	switch {
	case a != b:
		n = n + 8
	}
	return n
}

func unsigned(a, b uint) int {
	n := 0
	switch {
	case a < b:
		n = n + 1
	}
	switch {
	case a <= b:
		n = n + 2
	}
	return n
}

// conditions asks every comparison for both of its outcomes. The answers are
// signed(3, 4) = 11, signed(4, 3) = 8, signed(5, 5) = 6, unsigned(3, 4) = 3,
// unsigned(4, 3) = 0 and unsigned(5, 5) = 2, each in its own digit.
func conditions() int {
	return signed(3, 4) + signed(4, 3)*16 + signed(5, 5)*256 +
		unsigned(3, 4)*4096 + unsigned(4, 3)*16384 + unsigned(5, 5)*65536
}

func main() {
	d := conditions() - 145035
	d = d + classify(-5)*100 + classify(0)*10 + classify(7) + 99
	if d != 0 {
		d = d / (d - d)
	}
}
`

// TestARM64ConditionalSetRunsOnHardware is the deliverable of the conditional
// set: source text in, a process that exits zero out.
func TestARM64ConditionalSetRunsOnHardware(t *testing.T) {
	arm64Host(t)
	goCmd, err := exec.LookPath("go")
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_LINK") == "1" {
			t.Fatalf("NANOGO_REQUIRE_LINK is set and there is no go command: %v", err)
		}
		t.Skipf("no go command: %v", err)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "nanogo")
	mod := filepath.Join(dir, "mod")
	list := filepath.Join(dir, "allowlist")
	logFile := filepath.Join(dir, "nanogo.log")
	prog := filepath.Join(dir, "cond")
	// A build cache of this test's own. The go command caches per compiler
	// identity, and a cached object would let this pass while nanogo compiled
	// nothing at all.
	cache := filepath.Join(dir, "gocache")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(wd)

	build := exec.Command(goCmd, "build", "-o", bin, "golang.design/x/nanogo/cmd/nanogo")
	build.Dir = root
	build.Env = childEnv(nil)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/nanogo: %v\n%s", err, out)
	}

	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(mod, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/cond\n\ngo 1.27\n")
	write("main.go", condProgram)
	if err := os.WriteFile(list, []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(goCmd, "build", "-toolexec="+bin, "-o", prog, ".")
	cmd.Dir = mod
	cmd.Env = childEnv([]string{
		"NANOGO_ALLOWLIST=" + list,
		"NANOGO_LOG=" + logFile,
		"GOCACHE=" + cache,
	})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}

	// nanogo's own record of what it compiled. Without it a build that
	// delegated the package to gc would look exactly like a build nanogo did,
	// and the process would exit zero either way.
	decisions, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("nanogo wrote no log, so there is no evidence of what it compiled: %v", err)
	}
	if !strings.Contains(string(decisions), "compiled main ") {
		t.Fatalf("nanogo did not compile the main package, so nothing here ran its code:\n%s", decisions)
	}

	if out, err := exec.Command(prog).CombinedOutput(); err != nil {
		t.Fatalf("the compiled program did not exit zero, so a comparison gave the wrong answer: %v\n%s",
			err, out)
	}

	// The disassembly, read back with a tool that is not this compiler. It
	// names the condition each CSET carries, which is the half of the
	// operation that the exit status alone would not tell apart from its
	// complement in every case.
	dump, err := exec.Command(goCmd, "tool", "objdump", "-s", "main.(classify|signed|unsigned)", prog).Output()
	if err != nil {
		t.Fatalf("go tool objdump: %v", err)
	}
	text := string(dump)
	for _, want := range []string{
		"CSETW LT", "CSETW LE", "CSETW EQ", "CSETW NE", "CSETW LO", "CSETW LS",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the disassembly holds no %q; the six conditions condOf produces "+
				"are the ones the program asks for", want)
		}
	}
}

// childEnv keeps the go command off the network and away from the developer's
// own allowlist, which would change what this test measures.
func childEnv(extra []string) []string {
	out := append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=", "GO111MODULE=on", "GOPROXY=off")
	for i, kv := range out {
		if strings.HasPrefix(kv, "NANOGO_ALLOWLIST=") || strings.HasPrefix(kv, "NANOGO_LOG=") {
			out[i] = strings.SplitN(kv, "=", 2)[0] + "="
		}
	}
	return append(out, extra...)
}
