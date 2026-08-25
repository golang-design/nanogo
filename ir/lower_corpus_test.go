// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir_test

import (
	"fmt"
	gobuild "go/build"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The measurement of specs/020-ir.md's lowering pass.
//
// It is an external test package, and it has to be: the measurement is how far
// a lowered tree gets through specs/021-ssa-construction.md, and ssa imports
// ir, so a test inside package ir cannot import it back. ssa's own corpus
// tests cannot make the measurement either, because they call ir.Build and
// hand the result straight to ssa.Build.
//
// Before and after are measured in one run over one package list, because the
// two corpus walks of this deck sample differently and a number from one is
// not comparable with a number from the other.

type lcPkg struct {
	pkg   *types2.Package
	files []*syntax.File
	info  *types2.Info
	err   error
}

type lcImporter struct {
	fset *syntax.FileSet
	done map[string]*lcPkg
}

func newLCImporter() *lcImporter {
	return &lcImporter{fset: syntax.NewFileSet(), done: make(map[string]*lcPkg)}
}

func (imp *lcImporter) Import(path string) (*types2.Package, error) {
	r := imp.check(path)
	if r.err != nil {
		return nil, r.err
	}
	return r.pkg, nil
}

func (imp *lcImporter) check(path string) *lcPkg {
	if have, ok := imp.done[path]; ok {
		if have == nil {
			return &lcPkg{err: fmt.Errorf("import cycle at %s", path)}
		}
		return have
	}
	if path == "unsafe" {
		r := &lcPkg{pkg: types2.Unsafe}
		imp.done[path] = r
		return r
	}
	// The nil entry is the cycle marker, written before the recursive check
	// and replaced by the result.
	imp.done[path] = nil
	r := &lcPkg{}
	imp.done[path] = r

	bp, err := gobuild.Import(path, "", 0)
	if err != nil {
		for _, prefix := range []string{"vendor/", "cmd/vendor/"} {
			if bp2, err2 := gobuild.Import(prefix+path, "", 0); err2 == nil {
				bp, err = bp2, nil
				break
			}
		}
	}
	if err != nil {
		r.err = err
		return r
	}
	if len(bp.CgoFiles) > 0 || len(bp.GoFiles) == 0 {
		r.err = fmt.Errorf("%s has no plain Go files", path)
		return r
	}
	for _, name := range bp.GoFiles {
		file, err := syntax.ParseFile(imp.fset, filepath.Join(bp.Dir, name), nil, nil, 0)
		if err != nil || file == nil {
			r.err = fmt.Errorf("parse %s: %v", name, err)
			return r
		}
		r.files = append(r.files, file)
	}
	r.info = &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{Fset: imp.fset, Importer: imp, Sizes: types2.SizesFor("gc", "arm64")}
	r.pkg, r.err = conf.Check(path, r.files, r.info)
	return r
}

// lcPaths returns the package directories under src.
func lcPaths(t *testing.T, src string, all bool) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != src && (name == "testdata" || name == "vendor" ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(paths)
	if all {
		return paths
	}
	// The unattended run takes a sample and leaves the tool chain out, which
	// is what the other corpus tests of this deck do. CI sets
	// NANOGO_REQUIRE_CORPUS.
	var library []string
	for _, p := range paths {
		if !strings.HasPrefix(p, "cmd/") && p != "cmd" && p != "unsafe" {
			library = append(library, p)
		}
	}
	const sample = 40
	if len(library) > sample {
		step := len(library) / sample
		var taken []string
		for i := 0; i < len(library); i += step {
			taken = append(taken, library[i])
		}
		library = taken
	}
	return library
}

// lcCounts is one run of the lowering corpus.
type lcCounts struct {
	pkgs  int
	funcs int

	// before and after are how many functions ssa.Build accepted without the
	// lowering pass and with it. They are the deliverable.
	before int
	after  int

	// clean counts the functions ir.HasGoSpecific reports nothing for after
	// lowering. It is a different question from reaching SSA, because
	// construction also refuses forms that are not Go-specific at all, and it
	// is the invariant this pass exists to satisfy.
	clean int

	// verifyNG counts a function that built and did not verify. It is counted
	// apart from a refusal because it is worse than one: a refusal is visible
	// and a malformed graph is not.
	verifyNG int

	lowerRefused map[string]int
	buildRefused map[string]int
}

// lcCause reduces an error to the cause it reports, so that one cause is one
// row rather than one row per type.
func lcCause(err error) string {
	switch e := err.(type) {
	case *ir.LowerError:
		return e.Cause()
	case *ssa.Error:
		d := e.Detail
		head := ""
		if i := strings.Index(d, ": "); i >= 0 {
			head, d = d[:i+2], d[i+2:]
		}
		for _, prefix := range []string{
			"a conversion from ", "an index of ", "operator ", "unary ",
			"comparison ", "the address of ", "field ",
		} {
			if strings.HasPrefix(d, prefix) {
				return head + prefix + "..."
			}
		}
		return head + d
	}
	return "not a construction error"
}

// TestLowerCorpus measures what the lowering pass buys.
//
// The wording of the log lines is deliberate. internal/hygiene scrapes counts
// out of the corpus logs by pattern, and this test runs in the same -run
// Corpus invocation as the ones it scrapes, so no line here may match one of
// those patterns.
func TestLowerCorpus(t *testing.T) {
	required := os.Getenv("NANOGO_REQUIRE_CORPUS") == "1"
	src := filepath.Join(runtime.GOROOT(), "src")
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		if required {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s is not there", src)
		}
		t.Skipf("no corpus at %s", src)
	}

	imp := newLCImporter()
	c := &lcCounts{
		lowerRefused: make(map[string]int),
		buildRefused: make(map[string]int),
	}
	for _, path := range lcPaths(t, src, required) {
		if path == "unsafe" {
			continue
		}
		r := imp.check(path)
		if r.err != nil || r.pkg == nil {
			continue
		}
		c.pkgs++

		// Two trees from one type-checked package. The pass rewrites in place,
		// so the run without it needs a tree of its own.
		plain, _ := ir.Build(r.pkg, r.files, r.info)
		if plain != nil {
			for _, fn := range lcFuncs(plain) {
				if f, err := ssa.Build(fn); err == nil && f != nil {
					c.before++
				}
			}
		}

		lowered, _ := ir.Build(r.pkg, r.files, r.info)
		if lowered == nil {
			continue
		}
		for _, fn := range lcFuncs(lowered) {
			c.funcs++
			err := ir.Lower(fn)
			if err != nil {
				c.lowerRefused[lcCause(err)]++
			} else {
				c.clean++
			}
			f, berr := ssa.Build(fn)
			if berr != nil || f == nil {
				c.buildRefused[lcCause(berr)]++
				continue
			}
			c.after++
			if vs := ssa.Verify(f); len(vs) != 0 {
				c.verifyNG++
				if c.verifyNG < 4 {
					t.Errorf("%s: %s built and did not verify: %v", path, fn.Name, vs)
				}
			}
		}
	}

	t.Logf("lowering corpus: %d packages, %d functions, %d past construction with the pass, %d without",
		c.pkgs, c.funcs, c.after, c.before)
	t.Logf("lowering corpus: %d functions hold no Go-specific node after the pass", c.clean)
	lcLog(t, "lowering refused", c.lowerRefused)
	lcLog(t, "construction refused", c.buildRefused)
	if c.verifyNG != 0 {
		t.Errorf("%d functions built and did not verify", c.verifyNG)
	}
	if c.funcs == 0 {
		t.Fatal("the corpus produced no function")
	}
	if c.after < c.before {
		t.Errorf("the pass lost ground: %d past construction with it and %d without", c.after, c.before)
	}
	if required && c.after < 1000 {
		t.Fatalf("only %d functions got past construction; the corpus collapsed", c.after)
	}
}

// lcFuncs is every function of a package, in declaration order.
func lcFuncs(p *ir.Package) []*ir.Func {
	out := make([]*ir.Func, 0, len(p.Funcs)+len(p.Inits))
	out = append(out, p.Funcs...)
	return append(out, p.Inits...)
}

// lcLog prints the causes, largest first.
//
// From a sorted slice of keys rather than from a range over the map:
// specs/053-determinism.md forbids output that depends on map order, and a
// report whose rows move between runs cannot be diffed.
func lcLog(t *testing.T, what string, m map[string]int) {
	t.Helper()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		t.Logf("%s %6d  %s", what, m[k], k)
	}
}
