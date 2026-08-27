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

// The tests here run programs whose answer depends on an init having run.
//
// Every other end-to-end program in this directory computes its answer from
// its own instructions, so it would run the same whether or not any package
// was initialised. That is what let nanogo write no initialisation record for
// a year of programs: a program that touches nothing initialised cannot tell
// the difference. See specs/040-object-format.md for the record, and
// specs/003-sequencing.md for what runs it.
//
// The exit status is the channel, as it is for the rest of this directory.
// A nanogo-compiled program cannot print: a string constant needs a data
// symbol and specs/032 has no writer, so os.Stdout.WriteString is refused
// before the linker sees it. os.Stdout itself is reachable, and reading it is
// the first thing a program does that only an init can have arranged.

// exitCode runs a program and returns the status it exited with.
func exitCode(t *testing.T, prog string) int {
	t.Helper()
	err := exec.Command(prog).Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("the program nanogo built did not run: %v", err)
	}
	return ee.ExitCode()
}

// TestBuildRunsTheStandardLibrarysInit is the case that was silent.
//
// os.Stdout is assigned by os's init, from os.NewFile. Without an
// initialisation record in the main package cmd/link schedules nothing, so
// os.Stdout stays nil and every program that touches it is wrong. The file
// descriptor is the assertion and not "not nil": a *File that reports 1 is
// the one os.NewFile built, not a zero value that happened to be non-nil.
//
// With no record this program exits 255, because Fd on a nil *File answers
// with the invalid descriptor and os.Exit turns -1 into 255.
func TestBuildRunsTheStandardLibrarysInit(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod": "module nanogo.example/stdinit\n\ngo 1.27\n",
		"main.go": `package main

import "os"

func main() { os.Exit(int(os.Stdout.Fd())) }
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "stdinit", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	if code := exitCode(t, filepath.Join(h.mod, "stdinit")); code != 1 {
		t.Errorf("os.Stdout.Fd() is %d, want 1: os's init did not run", code)
	}
}

// TestBuildRunsThePackagesOwnInit is the second half: not the standard
// library's init, but one nanogo compiled.
//
// The declared init is compiled under its own symbol, ir.Build synthesises the
// function that calls it, and the record points at the synthesised one. The
// value is read back through a package the toolchain compiled, because nanogo
// refuses a package-level variable of its own: see
// TestBuildRefusesAPackageLevelVariableByNameAndPosition.
func TestBuildRunsThePackagesOwnInit(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":       "module nanogo.example/owninit\n\ngo 1.27\n",
		"cell/cell.go": cellPackage,
		"main.go": `package main

import (
	"os"

	"nanogo.example/owninit/cell"
)

func init() { cell.Set(9) }

func main() { os.Exit(cell.Get()) }
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "owninit", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	if code := exitCode(t, filepath.Join(h.mod, "owninit")); code != 9 {
		t.Errorf("the program exited %d, want 9: main's own init did not run", code)
	}
}

// cellPackage is one package-level variable and the two functions that reach
// it. The toolchain compiles it, so the variable has a data symbol.
const cellPackage = `package cell

var v int

func Set(n int) { v = n }

func Get() int { return v }
`

// recorderPackage records the order in which it was called, as a number read
// left to right: Note(2) then Note(1) leaves 21.
const recorderPackage = `package rec

var v int

func Note(n int) { v = v*10 + n }

func Value() int { return v }
`

// TestBuildOrdersAnInitAfterTheImportItDependsOn is the ordering claim, and
// it is the one that can be passed by accident.
//
// main imports both a and b, so both records are reachable from main's
// whatever the edges between them say. cmd/link picks the lexicographically
// first record that has no unscheduled dependency left, and a..inittask comes
// before b..inittask, so the only thing that can put b first is the edge a
// writes because a imports b. Dropping that edge does not fail: it runs a
// first and the program exits 12 instead of 21.
//
// nanogo compiles a, b and main, which is what makes the edges under test
// nanogo's. rec is the toolchain's, because it is where the observation is
// stored and nanogo has no writer for a package-level variable.
func TestBuildOrdersAnInitAfterTheImportItDependsOn(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/order\n\ngo 1.27\n",
		"rec/rec.go": recorderPackage,
		"b/b.go": `package b

import "nanogo.example/order/rec"

func init() { rec.Note(2) }

func Zero() int { return 0 }
`,
		"a/a.go": `package a

import (
	"nanogo.example/order/b"
	"nanogo.example/order/rec"
)

func init() { rec.Note(1) }

func Zero() int { return b.Zero() }
`,
		"main.go": `package main

import (
	"os"

	"nanogo.example/order/a"
	"nanogo.example/order/b"
	"nanogo.example/order/rec"
)

func main() { os.Exit(rec.Value() + a.Zero() + b.Zero()) }
`,
	}, []string{
		"# nanogo owns the three packages whose init order is under test",
		"main",
		"nanogo.example/order/a",
		"nanogo.example/order/b",
	})

	// -toolexec and not "nanogo build": the build command refuses a package
	// that imports another package the same command compiles, and this test
	// needs nanogo to compile both sides of an import edge.
	if out, err := h.build(t, "-o", "order", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	for _, pkg := range []string{"main", "nanogo.example/order/a", "nanogo.example/order/b"} {
		if !compiled(lines, pkg) {
			t.Fatalf("nanogo delegated %s, so the edges under test are gc's:\n%s", pkg, strings.Join(lines, "\n"))
		}
	}
	if compiled(lines, "nanogo.example/order/rec") {
		t.Fatalf("nanogo compiled the recorder, which it has no data symbol writer for:\n%s",
			strings.Join(lines, "\n"))
	}

	switch code := exitCode(t, filepath.Join(h.mod, "order")); code {
	case 21: // b before a, which is what importing b means
	case 12:
		t.Error("the program exited 12: a's init ran before b's, so the edge from a to b is missing")
	case 0:
		t.Error("the program exited 0: no init ran at all")
	default:
		t.Errorf("the program exited %d, want 21", code)
	}
}

// TestBuildRunsTheInitOfABlankImport is the import that exists for its init
// alone.
//
// side is imported only for its effect, so no call, no constant and no type
// links main to it. The ordering edge is the only thing that reaches its
// record, and a compiler that wrote edges for the imports it emits code
// against would drop exactly this one.
func TestBuildRunsTheInitOfABlankImport(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/blank\n\ngo 1.27\n",
		"rec/rec.go": recorderPackage,
		"side/side.go": `package side

import "nanogo.example/blank/rec"

func init() { rec.Note(7) }
`,
		"main.go": `package main

import (
	"os"

	"nanogo.example/blank/rec"
	_ "nanogo.example/blank/side"
)

func main() { os.Exit(rec.Value()) }
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "blank", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	if code := exitCode(t, filepath.Join(h.mod, "blank")); code != 7 {
		t.Errorf("the program exited %d, want 7: the blank import's init did not run", code)
	}
}

// TestBuildRunsAPackageLevelVariableThroughTheRecord is the boundary of the
// work above.
//
// The record's whole job is to run the initialisation before main does, and a
// package-level variable is what the initialisation writes. The exit status is
// the assertion: a variable left zero exits 0 and a record that did not run
// exits 0, so 9 is the only status that says both halves worked.
func TestBuildRunsAPackageLevelVariableThroughTheRecord(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod": "module nanogo.example/global\n\ngo 1.27\n",
		"main.go": `package main

import "os"

var n = 9

func main() { os.Exit(n) }
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "global", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	if code := exitCode(t, filepath.Join(h.mod, "global")); code != 9 {
		t.Errorf("the program exited %d, want 9: the package-level variable did not hold its value", code)
	}
}

// TestBuildRunsAPackageLevelInterfaceVariable is the row that left the refusal
// below.
//
// A variable whose type holds a pointer needs its type descriptor, because
// cmd/link reads the pointer map of a data symbol through it. An interface with
// methods had no descriptor while a function literal had no canonical name, so
// this program was refused. Both spellings are written now, so it builds and
// runs, and the exit status says the variable holds the value the
// initialisation gave it.
func TestBuildRunsAPackageLevelInterfaceVariable(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod": "module nanogo.example/iface\n\ngo 1.27\n",
		"main.go": `package main

import (
	"errors"
	"os"
)

var errBad = errors.New("bad")

func main() {
	if errBad == nil {
		os.Exit(1)
	}
	os.Exit(42)
}
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "iface", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	if code := exitCode(t, filepath.Join(h.mod, "iface")); code != 42 {
		t.Errorf("the program exited %d, want 42: the package-level error variable did not hold its value", code)
	}
}

// TestBuildDescribesAPackageLevelMapToTheCollector is the half of that
// boundary a map used to sit on the wrong side of.
//
// A variable whose type holds a pointer needs its type descriptor, because
// cmd/link reads the pointer map of a data symbol through it. A map's was the
// descriptor rtype could not build, so the variable was refused rather than
// emitted where the collector would misread it. It is written now, and the
// program is what says the description is right: the collector reads the
// variable's pointer map on every cycle, and gccheckmark marks a second time
// with the world stopped and compares.
func TestBuildDescribesAPackageLevelMapToTheCollector(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod": "module nanogo.example/global\n\ngo 1.27\n",
		"main.go": `package main

import "os"

var table map[string]*int

func main() {
	if table != nil {
		os.Exit(1)
	}
	table = map[string]*int{}
	for i := 0; i < 64; i++ {
		n := i
		table["k"] = &n
	}
	if len(table) != 1 || *table["k"] != 63 {
		os.Exit(2)
	}
	os.Exit(42)
}
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "global", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	cmd := exec.Command(filepath.Join(h.mod, "global"))
	cmd.Env = append(os.Environ(), "GODEBUG=gccheckmark=1,clobberfree=1", "GOGC=1")
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("the program did not run: %v\n%s", err, b)
	}
	if code != 42 {
		t.Errorf("the program exited %d, want 42\n%s", code, b)
	}
}

// TestBuildRefusesAPackageLevelVariableByNameAndPosition is what is left of
// that refusal.
//
// The type is an array of two hundred pointers, which is past
// internal/abi.MaxPtrmaskBytes: gc stops writing an inline bitmask there and
// emits a symbol the runtime fills in on demand, which rtype does not write.
// The variable is refused before any function is compiled, because a record
// that listed an init which assigns to a symbol that does not exist would
// produce a program that runs and is wrong. The message names the variable and
// where it is declared, because those are what say what to do next.
func TestBuildRefusesAPackageLevelVariableByNameAndPosition(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod": "module nanogo.example/global\n\ngo 1.27\n",
		"main.go": `package main

import "os"

var table [200]*int

func main() {
	if table[0] != nil {
		os.Exit(1)
	}
}
`,
	}, nil)

	out, err := h.nanogoBuild(t, "-o", "global", ".")
	if err == nil {
		t.Fatalf("nanogo build accepted a variable it cannot describe to the collector:\n%s", out)
	}
	for _, want := range []string{"package-level variable main.table", "main.go:5:5", "type descriptor"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(h.mod, "global")); err == nil {
		t.Error("nanogo wrote an executable although it refused the package")
	}
}
