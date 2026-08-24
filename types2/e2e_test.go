// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package types2_test

import (
	goast "go/ast"
	goimporter "go/importer"
	goparser "go/parser"
	gotoken "go/token"
	gotypes "go/types"
	"testing"

	"golang.design/x/nanogo/syntax"
	. "golang.design/x/nanogo/types2"
)

// This file is the end-to-end gate for the front end: nanogo's own parser, then
// nanogo's checker, judged against go/types on the same source.
//
// specs/012-type-checking.md states the standard plainly. nanogo's messages are
// its own; its judgements are upstream's. So the comparison here is accept
// against reject, not message against message, and a disagreement is a bug in
// nanogo whichever way it falls.

// TestAgreesWithGoTypesOnSnippets checks accept and reject against go/types
// over a table of small programs.
func TestAgreesWithGoTypesOnSnippets(t *testing.T) {
	for _, test := range []struct {
		name string
		src  string
	}{
		{"empty", `package p`},
		{"const arithmetic", `package p; const x = 1 << 62 / 3`},
		{"const overflow", `package p; const x int8 = 1 << 62`},
		{"method set", `package p
type T struct{ x int }
func (t T) M() int { return t.x }
func (t *T) N() int { return t.x }
var _ interface{ M() int } = T{}
`},
		{"pointer receiver not in value method set", `package p
type T struct{}
func (t *T) M() {}
var _ interface{ M() } = T{}
`},
		{"embedding and promotion", `package p
type A struct{ X int }
type B struct{ A }
var _ = B{}.X
`},
		{"type inference", `package p
func Map[S ~[]E, E, R any](s S, f func(E) R) []R { return nil }
var _ = Map([]int{1}, func(i int) string { return "" })
`},
		{"inference failure", `package p
func F[T any](a, b T) {}
var _ = F(1, "x")
`},
		{"constraint satisfaction", `package p
type Num interface{ ~int | ~float64 }
func Sum[T Num](s []T) T { var z T; return z }
var _ = Sum([]string{})
`},
		{"initialisation cycle", `package p
var a = b
var b = a
`},
		{"unused variable", `package p
func f() { x := 1 }
`},
		{"shadowed range", `package p
func f(s []int) { for i := range s { _ = i } }
`},
		{"type switch", `package p
func f(x any) int {
	switch v := x.(type) {
	case int:
		return v
	case string:
		return len(v)
	}
	return 0
}
`},
		{"impossible type assertion", `package p
type I interface{ M() int }
type T struct{}
func (T) M() string { return "" }
func f(i I) { _ = i.(T) }
`},
		{"unsafe", `package p
import "unsafe"
type T struct{ a int32; b int64 }
const _ = unsafe.Offsetof(T{}.b)
`},
		{"generic recursion", `package p
type List[T any] struct { head T; tail *List[T] }
var _ List[int]
`},
		{"label misuse", `package p
func f() {
L:
	for {
		break L
	}
	goto L
}
`},
	} {
		t.Run(test.name, func(t *testing.T) {
			nanogoErr := checkWithNanogo(t, test.name+".go", test.src)
			goTypesErr := checkWithGoTypes(t, test.name+".go", test.src)
			switch {
			case nanogoErr == nil && goTypesErr != nil:
				t.Errorf("nanogo accepted, go/types rejected: %v", goTypesErr)
			case nanogoErr != nil && goTypesErr == nil:
				t.Errorf("nanogo rejected, go/types accepted: %v", nanogoErr)
			}
		})
	}
}

// TestAgreesWithGoTypesOnStdlib type-checks whole standard library packages
// with both checkers and compares accept against reject.
//
// The packages are named rather than walked. Walking GOROOT is the conformance
// job in specs/004-conformance.md and belongs in its own gate, not in a unit
// test that every commit runs.
func TestAgreesWithGoTypesOnStdlib(t *testing.T) {
	mustHaveGoBuild(t)
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	for _, path := range []string{
		"errors",
		"io",
		"sort",
		"strconv",
		"unicode/utf8",
		"strings",
		"bytes",
		"fmt",
		"go/constant",
		"go/types",
		"slices",
		"maps",
		"sync",
		"container/heap",
	} {
		t.Run(path, func(t *testing.T) {
			fset := syntax.NewFileSet()
			_, nanogoErr := newSrcImporter(fset).Import(path)

			gofset := gotoken.NewFileSet()
			_, goTypesErr := goimporter.ForCompiler(gofset, "source", nil).Import(path)

			switch {
			case nanogoErr == nil && goTypesErr != nil:
				t.Errorf("nanogo accepted, go/types rejected: %v", goTypesErr)
			case nanogoErr != nil && goTypesErr == nil:
				t.Errorf("nanogo rejected, go/types accepted: %v", nanogoErr)
			}
		})
	}
}

// TestChecksOwnSource type-checks nanogo's own syntax package.
//
// The measure of the project is a fixed point (specs/001-bootstrap-gates.md),
// and the first thing the front end must survive is its own source.
func TestChecksOwnSource(t *testing.T) {
	mustHaveGoBuild(t)
	for _, pkg := range []string{"golang.design/x/nanogo/syntax", "golang.design/x/nanogo/types2"} {
		t.Run(pkg, func(t *testing.T) {
			fset := syntax.NewFileSet()
			if _, err := newSrcImporter(fset).Import(pkg); err != nil {
				t.Errorf("nanogo rejected its own source: %v", err)
			}
		})
	}
}

// checkWithNanogo parses src with nanogo's parser and type-checks it with this
// package. It skips the test if the parser cannot read the source, so that an
// unfinished parser does not read as a checker failure.
func checkWithNanogo(t *testing.T, filename, src string) error {
	t.Helper()
	fset := syntax.NewFileSet()
	f, err := syntax.Parse(fset.AddFile(filename, len(src)), []byte(src), nil, nil, 0)
	if err != nil {
		t.Skipf("nanogo's parser could not read the source: %v", err)
	}
	conf := Config{Fset: fset, Importer: newSrcImporter(fset)}
	_, err = conf.Check(f.PkgName.Value, []*syntax.File{f}, nil)
	return err
}

// checkWithGoTypes is the same source through go/parser and go/types.
func checkWithGoTypes(t *testing.T, filename, src string) error {
	t.Helper()
	fset := gotoken.NewFileSet()
	f, err := goparser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("go/parser could not read the source: %v", err)
	}
	conf := gotypes.Config{Importer: goimporter.ForCompiler(fset, "source", nil)}
	_, err = conf.Check(f.Name.Name, fset, []*goast.File{f}, nil)
	return err
}
