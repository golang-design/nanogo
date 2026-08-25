// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The string constant is the first data symbol nanogo writes.
//
// Every test here compiles, links and runs. A test that stopped at compilation
// would miss the half of the claim that matters: a relocation against a symbol
// no object defines is reported by the linker and by nobody else.
//
// The assertion is the bytes the program wrote, compared against the bytes the
// same source writes when gc compiles it. gc is the oracle rather than a
// string written into this file, so a wrong expectation here is a wrong
// expectation about Go and not about nanogo.

// helloStringProgram is the program the gap was reported with.
const helloStringProgram = `package main

func main() { println("hi") }
`

// TestToolexecPrintsAStringConstant is the smallest program that needs a data
// symbol.
//
// "hi" decomposes into the address of a symbol holding two bytes and the
// length 2. Before the data symbol existed, ssagen refused the address with
// "an address of no symbol" and the program did not compile.
func TestToolexecPrintsAStringConstant(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/hi\n\ngo 1.27\n",
		"main.go": helloStringProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "hi", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "hi"))
	if want := "hi\n"; string(got) != want {
		t.Errorf("the program printed %q, want %q", got, want)
	}
	// The same source under the installed compiler. Comparing against gc
	// rather than against a literal is what makes this a conformance check.
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed %q and gc's printed %q", got, want)
	}
}

// stringOpsProgram compares, indexes and takes the length of a string.
//
// Every operation reaches the constant through a different path. Equality is
// runtime.memequal over the data pointer, ordering is runtime.cmpstring, the
// index is a load through the data pointer, and the length is the second word
// of the header. A constant whose symbol was wrong would give a wrong answer
// in the first three and the right one in the fourth.
//
// "alpha" appears in two functions on purpose. ssagen defines the bytes once
// per function, so an object holds two definitions of one content-addressable
// symbol and the linker merges them. A build that rejected the duplicate would
// fail here and nowhere else.
const stringOpsProgram = `package main

func describe(s string) string {
	if s == "alpha" {
		return "one"
	}
	if len(s) == 4 {
		return "four"
	}
	return "other"
}

func first(s string) byte { return s[0] }

func size(s string) int { return len(s) }

func alpha() string { return "alpha" }

func main() {
	println(describe("alpha"))
	println(describe("beta"))
	println(describe("gamma"))
	println(describe(alpha()))
	println(first("gamma"))
	println(size("gamma"))
	println("gamma" < "zeta")
	println("a" + "b")
}
`

// TestToolexecComparesIndexesAndMeasuresAString runs the operations a program
// does to a string.
func TestToolexecComparesIndexesAndMeasuresAString(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/strings\n\ngo 1.27\n",
		"main.go": stringOpsProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "strings", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "strings"))
	want := gcOutput(t, h)
	if string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%q\nand gc's printed\n%q", got, want)
	}
	// The answers, spelled out, so that two compilers agreeing on the wrong
	// answer is still a failure. 103 is 'g' and 5 is len("gamma").
	if lines := "one\nfour\nother\none\n103\n5\ntrue\nab\n"; string(got) != lines {
		t.Errorf("the program printed\n%q\nwant\n%q", got, lines)
	}
}

// TestStringSymbolNamesMatchGc compares the symbol table of the object nanogo
// writes against the object gc writes for the same source.
//
// The linker deduplicates a string constant by name and by content hash
// together, so a name one character away from gc's is a second symbol holding
// bytes the binary already carries, and nothing reports it. The oracle is
// therefore external: go tool nm over gc's own object.
//
// The comparison is a subset and not an equality. gc folds the newline of a
// println into the constant and emits go:string."hi\n" as well; nanogo calls
// runtime.printstring and runtime.printnl, so it needs no such constant. What
// must hold is that every name nanogo writes is a name gc writes.
func TestStringSymbolNamesMatchGc(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/symbols\n\ngo 1.27\n",
		"main.go": stringOpsProgram,
	}, []string{"main"})

	mine := stringSymbols(t, compileWithNanogo(t, h))
	theirs := stringSymbols(t, compileWithGc(t, h))
	if len(mine) == 0 {
		t.Fatalf("nanogo's object holds no string symbol at all")
	}
	for _, name := range mine {
		if !contains(theirs, name) {
			t.Errorf("nanogo writes %s and gc writes %v", name, theirs)
		}
	}
	// The constants the source spells out must be in both, so that a nanogo
	// object holding one symbol cannot pass the subset check above.
	for _, want := range []string{
		`go:string."alpha"`, `go:string."beta"`, `go:string."gamma"`,
		`go:string."one"`, `go:string."four"`, `go:string."other"`,
		`go:string."ab"`,
	} {
		if !contains(mine, want) {
			t.Errorf("nanogo's object has no %s: %v", want, mine)
		}
		if !contains(theirs, want) {
			t.Errorf("gc's object has no %s, so the expectation is wrong: %v", want, theirs)
		}
	}
}

// compileWithNanogo runs nanogo the way the go command runs it under
// -toolexec, and returns the object file.
//
// It compiles one file into one object rather than building the module,
// because the object is what carries the symbol table this test reads and a
// linked binary no longer names a local symbol.
func compileWithNanogo(t *testing.T, h *harness) string {
	t.Helper()
	out := filepath.Join(h.dir, "nanogo.o")
	cmd := exec.Command(h.bin, filepath.Join(toolDir(t, h), "compile"),
		"-p", "main", "-o", out, filepath.Join(h.mod, "main.go"))
	cmd.Dir = h.mod
	cmd.Env = env([]string{"NANOGO_ALLOWLIST=" + h.list, "GOCACHE=" + h.cache})
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nanogo compile: %v\n%s", err, b)
	}
	return out
}

// compileWithGc runs the installed compiler over the same file.
func compileWithGc(t *testing.T, h *harness) string {
	t.Helper()
	out := filepath.Join(h.dir, "gc.o")
	cmd := exec.Command(h.goCmd, "tool", "compile", "-p", "main", "-o", out,
		filepath.Join(h.mod, "main.go"))
	cmd.Dir = h.mod
	cmd.Env = env([]string{"GOCACHE=" + h.cache})
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go tool compile: %v\n%s", err, b)
	}
	return out
}

// toolDir is where the installed toolchain keeps the compiler.
func toolDir(t *testing.T, h *harness) string {
	t.Helper()
	cmd := exec.Command(h.goCmd, "env", "GOTOOLDIR")
	cmd.Env = env(nil)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOTOOLDIR: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// stringSymbols returns the names of the string constants an object defines,
// sorted and without repeats.
//
// nanogo defines the bytes of a constant once per function that names it, so
// one name can appear more than once in one object. The linker merges them on
// the content hash, which is what content addressing is for.
func stringSymbols(t *testing.T, object string) []string {
	t.Helper()
	cmd := exec.Command(goTool(t), "tool", "nm", object)
	cmd.Env = env(nil)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("go tool nm %s: %v", object, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		name := f[len(f)-1]
		if !strings.HasPrefix(name, "go:string.") || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// runProgram runs a built executable and returns everything it wrote.
//
// println writes to file descriptor 2, so the two streams are read together.
func runProgram(t *testing.T, path string) []byte {
	t.Helper()
	b, err := exec.Command(path).CombinedOutput()
	if err != nil {
		t.Fatalf("%s did not run: %v\n%s", filepath.Base(path), err, b)
	}
	return b
}

// gcOutput builds the module's program with the installed compiler and returns
// what it writes.
//
// The build is a plain go build rather than go run, so that the go command's
// own output cannot reach the comparison.
func gcOutput(t *testing.T, h *harness) []byte {
	t.Helper()
	out := filepath.Join(h.dir, "gc-built")
	cmd := exec.Command(h.goCmd, "build", "-o", out, ".")
	cmd.Dir = h.mod
	cmd.Env = env([]string{"GOCACHE=" + h.cache})
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build with the installed compiler: %v\n%s", err, b)
	}
	defer os.Remove(out)
	return runProgram(t, out)
}

// globalProgram reads every package-level variable it declares.
//
// The declarations cover the four kinds of data symbol. greeting and n hold
// contents the linker writes: a string header pointing at a constant, and an
// integer. zero and buf are zero-filled and cost no bytes in the object. p is
// zero-filled and scanned, because a pointer variable is a root the collector
// follows.
//
// sl and computed are the other half of the rule. Neither initialiser is a
// constant, so both symbols start zero and the initialisation function assigns
// them before main runs. A variable silently left zero would print 0 here and
// the test would say so.
const globalProgram = `package main

var greeting = "hello"
var n = 42
var flag = true
var zero int
var empty string
var buf [8]byte
var p *int
var sl = []int{1, 2, 3}
var computed = n * 2

type myInt int

var typed myInt = 7

func main() {
	println(greeting)
	println(n)
	println(flag)
	println(zero)
	println(len(empty))
	println(len(buf))
	println(p == nil)
	println(len(sl), sl[0], sl[2])
	println(computed)
	println(int(typed))
}
`

// TestToolexecReadsPackageLevelVariables builds, links and runs a program
// whose answers are all in package-level variables.
func TestToolexecReadsPackageLevelVariables(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/globals\n\ngo 1.27\n",
		"main.go": globalProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "globals", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := runProgram(t, filepath.Join(h.mod, "globals"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%q\nand gc's printed\n%q", got, want)
	}
	if want := "hello\n42\ntrue\n0\n0\n8\ntrue\n3 1 3\n84\n7\n"; string(got) != want {
		t.Errorf("the program printed\n%q\nwant\n%q", got, want)
	}
}

// TestGlobalSymbolsMatchGc compares the data symbols nanogo writes against the
// ones gc writes for the same source.
//
// Three things have to agree, and go tool nm reports all three: the name,
// because the linker resolves a variable by name and one character of drift is
// an undefined symbol; the size, because the linker allocates it; and the
// section letter, because it says whether the linker copies bytes or allocates
// zeros.
//
// main.sl is the one divergence and it is stated rather than skipped. gc lays
// a composite literal out into a static temporary and writes a slice header
// pointing at it, so the symbol carries contents. nanogo has no static
// temporary, so the symbol is zero-filled and the initialisation function
// builds the slice before main runs. The size is the same and the program
// prints the same answer; only who writes the bytes differs.
func TestGlobalSymbolsMatchGc(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/globalsyms\n\ngo 1.27\n",
		"main.go": globalProgram,
	}, []string{"main"})

	mine := dataSymbols(t, compileWithNanogo(t, h))
	theirs := dataSymbols(t, compileWithGc(t, h))
	for _, name := range []string{
		"main.greeting", "main.n", "main.flag", "main.zero", "main.empty",
		"main.buf", "main.p", "main.sl", "main.computed", "main.typed",
	} {
		got, ok := mine[name]
		if !ok {
			t.Errorf("nanogo's object defines no %s", name)
			continue
		}
		want, ok := theirs[name]
		if !ok {
			t.Errorf("gc's object defines no %s, so the expectation is wrong", name)
			continue
		}
		if name == "main.sl" {
			if got != "24 B" || want != "24 D" {
				t.Errorf("nanogo writes %s as %s and gc writes it as %s; the composite literal case changed", name, got, want)
			}
			continue
		}
		if got != want {
			t.Errorf("nanogo writes %s as %s and gc writes it as %s", name, got, want)
		}
	}
}

// dataSymbols returns the size and the section letter of every symbol an
// object defines under the main package's name.
//
// A definition has an address and a reference does not, which is how go tool
// nm shows the difference: an undefined symbol prints as U with no size.
// nanogo names a symbol of the package being compiled by name even when the
// same object defines it, so one name can appear as both.
func dataSymbols(t *testing.T, object string) map[string]string {
	t.Helper()
	cmd := exec.Command(goTool(t), "tool", "nm", "-size", object)
	cmd.Env = env(nil)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("go tool nm -size %s: %v", object, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		// address, size, code, name
		if len(f) != 4 || !strings.HasPrefix(f[3], "main.") {
			continue
		}
		if f[2] == "U" || f[2] == "T" {
			continue
		}
		out[f[3]] = f[1] + " " + f[2]
	}
	return out
}
