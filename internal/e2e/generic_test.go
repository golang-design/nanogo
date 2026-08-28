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

// The program the stenciler of specs/013-generics.md has to compile.
//
// One generic function per shape the substitution has to reach, each
// instantiated at more than one type argument list so that two bodies are
// built and neither can be the other:
//
//   - pick returns one of two values of the type parameter's type, so the
//     parameter and the result are both substituted.
//   - sum ranges over a slice of the type parameter's type and adds, so the
//     operation the instruction selector picks depends on the type argument:
//     int adds and string concatenates.
//   - count declares a local of a composite type built out of the type
//     parameter, which is the local whose frame slot has a different size in
//     each instantiation.
//   - size calls a method reached through the type parameter's constraint. The
//     checker records that selection against the constraint, whose method
//     nothing defines, so each instantiation has to call its own concrete
//     method.
//   - through calls another generic, so an instantiation is discovered inside
//     an instantiated body rather than in a declared one.
//
// The exit status is the assertion, as in the other end-to-end programs: a
// nanogo-compiled program cannot print, so a wrong answer divides by zero and
// the process dies. A zero exit means every value was the one Go defines.
const genericProgram = `package main

type sized interface{ size() int }

type small struct{ n int }

func (s small) size() int { return s.n }

type large struct {
	a, b, c int
	p       *int
}

func (l large) size() int { return l.a + l.b + l.c }

func pick[T any](a, b T, first bool) T {
	if first {
		return a
	}
	return b
}

func sum[T int | string](xs []T) T {
	var acc T
	for _, x := range xs {
		acc = acc + x
	}
	return acc
}

func count[T any](xs []T) int {
	seen := make([]T, 0, len(xs))
	for _, x := range xs {
		seen = append(seen, x)
	}
	return len(seen)
}

func size[T sized](v T) int { return v.size() }

func through[T any](a, b T) T { return pick(a, b, false) }

func die(d int) {
	if d != 0 {
		d = d / (d - d)
	}
}

func main() {
	// Two instantiations of one generic at two type arguments.
	die(pick(7, 1, true) - 7)
	die(len(pick("ab", "c", false)) - 1)

	// The operation depends on the type argument.
	die(sum([]int{1, 2, 3}) - 6)
	die(len(sum([]string{"a", "bc"})) - 3)

	// A local whose type is built out of the type parameter.
	die(count([]int{1, 2, 3}) - 3)
	die(count([]string{"a"}) - 1)

	// A method reached through the constraint, on two concrete types whose
	// method bodies differ.
	die(size(small{n: 4}) - 4)
	die(size(large{a: 1, b: 2, c: 3}) - 6)

	// An instantiation discovered inside an instantiated body.
	die(through(2, 9) - 9)
	die(len(through("xy", "z")) - 1)
}
`

// genericModule is the module the test below builds.
func genericModule() map[string]string {
	return map[string]string{
		"go.mod":  "module nanogo.example/generic\n\ngo 1.27\n",
		"main.go": genericProgram,
	}
}

// TestBuildRunsAStenciledProgram is the proof specs/013-generics.md asks for.
//
// A compile is not it. The stenciler produces a new function that the rest of
// the compiler must not be able to tell from a declared one, so the question
// is whether the linked program computes what Go says it computes, and that is
// answered by running it.
func TestBuildRunsAStenciledProgram(t *testing.T) {
	h := setup(t, genericModule(), nil)

	out, err := h.nanogoBuild(t, ".")
	if err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 of ") {
		t.Errorf("the build did not compile the package with nanogo:\n%s", out)
	}
	prog := filepath.Join(h.mod, "generic")
	if b, err := exec.Command(prog).CombinedOutput(); err != nil {
		t.Fatalf("the stenciled program did not run: %v\n%s", err, b)
	}
}

// TestBuildNamesAStencilTheWayTheSpecSays reads the symbols out of the linked
// program.
//
// The name is what makes an instantiation one function in the program rather
// than one per package that reached it, and it is what keeps a body nanogo
// wrote from colliding with one gc wrote for the same instantiation. gc
// stencils by GC shape and writes main.pick[go.shape.int] for a body and
// main..dict.pick[int] for a dictionary, so a full stencil named
// main.pick[int] collides with neither.
func TestBuildNamesAStencilTheWayTheSpecSays(t *testing.T) {
	h := setup(t, genericModule(), nil)

	if out, err := h.nanogoBuild(t, "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	nm, err := exec.Command(goTool(t), "tool", "nm", filepath.Join(h.mod, "generic")).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm: %v\n%s", err, nm)
	}
	syms := string(nm)

	for _, want := range []string{
		"main.pick[int]",
		"main.pick[string]",
		"main.sum[int]",
		"main.sum[string]",
		"main.size[main.small]",
		"main.size[main.large]",
		"main.through[int]",
		"main.through[string]",
	} {
		if !strings.Contains(syms, want) {
			t.Errorf("the program holds no symbol %s", want)
		}
	}
	// Only the package nanogo compiled. The standard library in the same
	// binary was compiled by gc, and it is full of go.shape. symbols and of
	// dictionaries, which is the point: nanogo's stencils sit beside them and
	// no name is claimed twice.
	for _, line := range strings.Split(syms, "\n") {
		i := strings.LastIndexByte(line, ' ')
		if i < 0 || !strings.HasPrefix(line[i+1:], "main.") {
			continue
		}
		sym := line[i+1:]
		if strings.Contains(sym, "go.shape.") {
			t.Errorf("%s is named the way gc names one of its own", sym)
		}
		if strings.HasPrefix(sym, "main..dict.") {
			t.Errorf("%s is a dictionary, and nanogo stencils fully", sym)
		}
	}
}
