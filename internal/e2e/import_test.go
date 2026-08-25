// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The library gc compiles and nanogo imports.
//
// It carries one of each kind of declaration a nanogo-compiled body can reach:
// a constant, which the checker folds into the instruction stream, and a
// function, which becomes a call to a symbol another package defines. Both
// have to survive the round trip through gc's export data for the program
// below to compute the right answer.
const libraryPackage = `package lib

const Scale = 7

func Add(a, b int) int { return a + b }

func Scaled(a int) int { return Add(a, a*Scale) }
`

// The program that imports it.
//
// The exit status is the assertion, as in the other end-to-end programs: a
// wrong answer divides by zero and the process dies. Scaled(3) is 3 + 21, so
// the whole expression is zero when every value crossed the package boundary
// intact.
const importingProgram = `package main

import "nanogo.example/two/lib"

func main() {
	d := lib.Add(20, 3) - lib.Scale - 16
	if d != 0 {
		d = d / (d - d)
	}
	e := lib.Scaled(3) - 24
	if e != 0 {
		e = e / (e - e)
	}
}
`

// The program that imports the standard library.
//
// math/bits is the reachable entry point: its constants are constants and
// OnesCount64 takes and returns a machine word, so a nanogo-compiled body can
// call it. The archive comes from the go command, and nobody wrote it for this
// test.
const stdlibProgram = `package main

import (
	"math/bits"
	"strconv"
)

func main() {
	d := bits.OnesCount64(7) - 3
	if d != 0 {
		d = d / (d - d)
	}
	e := bits.UintSize - strconv.IntSize
	if e != 0 {
		e = e / (e - e)
	}
	f := bits.TrailingZeros64(8) - 3
	if f != 0 {
		f = f / (f - f)
	}
}
`

// TestToolexecCompilesAPackageWithImports is what specs/015-export-data.md
// was blocking.
//
// gc compiles the library, nanogo compiles the package that imports it, the
// real linker joins them, and the program runs. Everything the importing
// package knows about the library it learned by reading the export data gc
// wrote, so a program that computes the right answer is the statement that the
// reader is correct.
func TestToolexecCompilesAPackageWithImports(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": libraryPackage,
		"main.go":    importingProgram,
	}, []string{"# nanogo owns the importing package, gc owns the library", "main"})

	out, err := h.build(t, "-o", "two", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the importing package instead of compiling it:\n%s", strings.Join(lines, "\n"))
	}
	if compiled(lines, "nanogo.example/two/lib") {
		t.Fatalf("nanogo compiled the library, so the test proves nothing about reading gc's export data:\n%s",
			strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "two")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled did not run: %v\n%s", err, b)
	}
}

// TestToolexecCompilesAPackageThatImportsTheStandardLibrary is the same claim
// against archives the toolchain ships.
//
// A package of the test's own is written by the test and could be written to
// suit it. math/bits and strconv are not, so what the reader has to decode is
// whatever gc happened to write for them, including the declarations the
// program never mentions.
func TestToolexecCompilesAPackageThatImportsTheStandardLibrary(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/std\n\ngo 1.27\n",
		"main.go": stdlibProgram,
	}, []string{"main"})

	out, err := h.build(t, "-o", "std", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "std")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled did not run: %v\n%s", err, b)
	}
}

// TestGcImportsWhatNanogoWrote is the cross-read, and it is the test that
// matters most.
//
// nanogo compiles the library and gc compiles the package that imports it, so
// everything the importing package knows about the library it learned by
// reading the export data nanogo wrote. A round trip through nanogo's own
// reader cannot make this claim: it would agree with itself about a format
// that was wrong. gc has to read the bytes, the linker has to join the two
// objects, and the program has to compute the right answer.
func TestGcImportsWhatNanogoWrote(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": libraryPackage,
		"main.go":    importingProgram,
	}, []string{"# nanogo owns the library, gc owns the package that imports it", "nanogo.example/two/lib"})

	out, err := h.build(t, "-o", "two", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/two/lib") {
		t.Fatalf("nanogo delegated the library, so gc read gc's own export data:\n%s", strings.Join(lines, "\n"))
	}
	if compiled(lines, "main") {
		t.Fatalf("nanogo compiled the importing package, so nothing read what nanogo wrote:\n%s",
			strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "two")).CombinedOutput(); err != nil {
		t.Fatalf("the program gc compiled against nanogo's export data did not run: %v\n%s", err, b)
	}
}

// dataPackage is the cheapest possible package for the writer.
//
// It declares constants and type aliases and no function at all, so its
// compiled form holds no symbol and its archive is the export data and
// nothing else. internal/goarch and internal/goos are this shape, which is
// why nanogo refused them until the writer existed: the refusal was about the
// missing export data and not about code generation.
const dataPackage = `package data

const (
	Width  = 8
	Name   = "data"
	Ratio  = 3.5
	Mask   = 1 << 62
)

type Word = int

type Pair struct {
	A, B Word
}
`

// dataImporter is the program that reads it.
const dataImporter = `package main

import "nanogo.example/data/data"

func main() {
	p := data.Pair{A: data.Width, B: 2}
	d := p.A + p.B - 10
	if d != 0 {
		d = d / (d - d)
	}
	var w data.Word = data.Width
	e := w - 8
	if e != 0 {
		e = e / (e - e)
	}
	if len(data.Name) != 4 {
		panic("name")
	}
}
`

// TestGcImportsAPackageWithNoFunctions is the writer's narrowest case.
//
// The library compiles to no symbol, so what gc reads back is the export data
// and only the export data: the container, the constant encodings, an alias
// and a struct. A failure here is in the format and cannot be in code
// generation, because nanogo generated none.
func TestGcImportsAPackageWithNoFunctions(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":       "module nanogo.example/data\n\ngo 1.27\n",
		"data/data.go": dataPackage,
		"main.go":      dataImporter,
	}, []string{"nanogo.example/data/data"})

	out, err := h.build(t, "-o", "dataprog", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/data/data") {
		t.Fatalf("nanogo delegated the package with no functions:\n%s", strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "dataprog")).CombinedOutput(); err != nil {
		t.Fatalf("the program did not run: %v\n%s", err, b)
	}
}

// TestNanogoImportsWhatNanogoWrote closes the loop.
//
// Both packages are nanogo's, so nanogo reads its own export data. It is the
// weakest of the three claims and the one an allowlist that grows towards the
// leaves depends on: a build in which nanogo owns two packages in a row.
func TestNanogoImportsWhatNanogoWrote(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": libraryPackage,
		"main.go":    importingProgram,
	}, []string{"main", "nanogo.example/two/lib"})

	out, err := h.build(t, "-o", "two", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	for _, pkg := range []string{"main", "nanogo.example/two/lib"} {
		if !compiled(lines, pkg) {
			t.Fatalf("nanogo delegated %s:\n%s", pkg, strings.Join(lines, "\n"))
		}
	}

	if b, err := exec.Command(filepath.Join(h.mod, "two")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled from end to end did not run: %v\n%s", err, b)
	}
}

// TestNanogoCompilesAStandardLibraryPackage is the keystone claim.
//
// internal/goarch and internal/goos are in the closure of every Go program:
// each is constants and type aliases, each compiles to no symbol at all, and
// the runtime imports both. With them on the allowlist, nanogo compiles them
// and gc compiles the other twenty-seven packages of the build against the
// export data nanogo wrote, runtime included. The program then runs.
//
// This is what specs/015-export-data.md's missing writer was blocking, and
// these two packages are where it was most visible: the driver refused them
// with a message about having no function bodies, which said the export data
// was missing rather than that code generation could not reach them. Until
// the writer existed, nanogo could compile only a package that nothing
// imports, which in this closure is main and nothing else.
func TestNanogoCompilesAStandardLibraryPackage(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/goarch\n\ngo 1.27\n",
		"main.go": stdlibProgram,
	}, []string{"internal/goarch", "internal/goos"})

	out, err := h.build(t, "-o", "prog", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	for _, pkg := range []string{"internal/goarch", "internal/goos"} {
		if !compiled(lines, pkg) {
			t.Fatalf("nanogo delegated %s:\n%s", pkg, strings.Join(lines, "\n"))
		}
	}
	if compiled(lines, "runtime") {
		t.Fatalf("nanogo compiled the runtime, so the test says nothing about importing:\n%s",
			strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "prog")).CombinedOutput(); err != nil {
		t.Fatalf("the program did not run against a standard library package nanogo compiled: %v\n%s", err, b)
	}
}
