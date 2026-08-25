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

// TestToolexecCannotImportWhatNanogoCompiled is the limit that is left.
//
// nanogo reads export data and does not write it, so the archive it produces
// carries the object and no __.PKGDEF. With both packages on the allowlist the
// library is nanogo's, and the package that imports it then has nothing to
// read. The message names the missing member rather than reporting the import
// as undefined, because the two have different fixes: this one is
// specs/015-export-data.md's writer half, which is unbuilt.
func TestToolexecCannotImportWhatNanogoCompiled(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": libraryPackage,
		"main.go":    importingProgram,
	}, []string{"main", "nanogo.example/two/lib"})

	out, err := h.build(t, "-o", "two", ".")
	if err == nil {
		t.Fatalf("the build succeeded although the library nanogo compiled carries no export data:\n%s", out)
	}
	for _, want := range []string{"nanogo.example/two/lib", "__.PKGDEF"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, out)
		}
	}
}
