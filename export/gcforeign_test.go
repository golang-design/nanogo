// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// gc reading back a generic declaration of a third package that nanogo copied.
//
// This is the shape foreign.go exists for. Package a holds a generic type that
// sync/atomic declares, package b holds a, and nanogo writes b. Nothing in
// what the checker recorded says how atomic.Pointer's dictionary was numbered
// or where its method bodies are, so b's export data can only carry the
// declaration by copying the elements out of an archive.
//
// The archive b is compiled against is a's and not sync/atomic's, because
// -importcfg names the direct imports and b imports only a. So the copy is
// taken out of a re-exporting archive, which is the case every package on
// nanogo's self-host gate hits.
//
// gc is the reader, because a copy is only right if the indices it was
// relocated to are the ones the reading compiler resolves. A unit test over
// nanogo's own bytes reads them with the numbering that wrote them and cannot
// see a wrong relocation at all. The program's output is compared with an
// all-gc build of the same module, so a dictionary slot that resolves to
// another type is a value that differs.

// foreignHolderSource declares a struct field of a generic type another
// package declares.
//
// Two instantiations, at int and at a slice, which have different sizes and
// different pointer maps. A dictionary standing in for the other one is then a
// value that differs rather than one that happens to agree.
const foreignHolderSource = "package a\n\n" +
	"import \"sync/atomic\"\n\n" +
	"type Holder struct {\n" +
	"\tP atomic.Pointer[int]\n" +
	"\tS atomic.Pointer[[]byte]\n" +
	"}\n\n" +
	// An alias to the instantiation, so that a program can name the type
	// without naming a defined type of this module. gc emits the type
	// descriptor of an instantiation in the package that instantiates it,
	// so nothing outside sync/atomic and the program itself has to be
	// linked in.
	"type IntPtr = atomic.Pointer[int]\n\n" +
	"type BytesPtr = atomic.Pointer[[]byte]\n"

// foreignWrapSource is the package nanogo writes.
//
// It names sync/atomic nowhere. The declaration reaches its exported surface
// through a's, which is what makes a's archive the only file the writer can
// copy the declaration out of.
const foreignWrapSource = "package b\n\n" +
	"import \"nanogo.example/foreign/a\"\n\n" +
	"type Wrap struct {\n\tH a.Holder\n}\n\n" +
	"type IntPtr = a.IntPtr\n\n" +
	"type BytesPtr = a.BytesPtr\n\n" +
	"func Held(w *Wrap) *a.Holder { return &w.H }\n"

// foreignMain instantiates the copied declaration and calls every method of
// it.
//
// It imports b and nothing else of the module, so every method it calls is
// stenciled out of the body and the dictionary nanogo's export data for b
// carries. No symbol of b is referenced, so the archive gc links against needs
// to hold no object, which is what nanogo's export data alone is.
const foreignMain = "package main\n\n" +
	"import (\n\t\"fmt\"\n\n\tlib \"nanogo.example/foreign/b\"\n)\n\n" +
	"func main() {\n" +
	"\tvar p lib.IntPtr\n" +
	"\tn, m := 7, 9\n" +
	"\tp.Store(&n)\n" +
	"\tfmt.Println(*p.Load())\n" +
	"\tfmt.Println(*p.Swap(&m), *p.Load())\n" +
	"\tfmt.Println(p.CompareAndSwap(&n, &m), p.CompareAndSwap(&m, &n), *p.Load())\n" +
	"\tvar s lib.BytesPtr\n" +
	"\tb, c := []byte(\"abc\"), []byte(\"de\")\n" +
	"\ts.Store(&b)\n" +
	"\tfmt.Println(string(*s.Load()), len(*s.Swap(&c)), string(*s.Load()))\n" +
	"}\n"

// TestGcInstantiatesAForeignGenericNanogoCopied is the oracle for foreign.go.
//
// nanogo writes the export data of a package whose surface reaches a generic
// type sync/atomic declares, gc compiles a program that instantiates it and
// calls each of its methods, and the program's output is compared with the
// same program built entirely by gc.
func TestGcInstantiatesAForeignGenericNanogoCopied(t *testing.T) {
	goCmd := goTool(t)
	tc, err := obj.VerifyToolchain()
	if err != nil {
		t.Skipf("cannot probe the installed toolchain: %v", err)
	}

	mod := t.TempDir()
	write := func(name, body string) {
		full := filepath.Join(mod, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/foreign\n\ngo 1.27\n")
	write("a/a.go", foreignHolderSource)
	write("b/b.go", foreignWrapSource)
	write("main.go", foreignMain)

	// What the same program prints when gc builds all of it. It is the
	// answer, and it is measured rather than written down so that the test
	// says nothing about sync/atomic's implementation.
	want, err := runGo(t, goCmd, mod, "run", ".")
	if err != nil {
		t.Skipf("the go command cannot build the module: %v\n%s", err, want)
	}

	deps := exportedDeps(t, goCmd, mod)
	const wrapPath = "nanogo.example/foreign/b"
	const holderPath = "nanogo.example/foreign/a"
	if deps[wrapPath] == "" || deps[holderPath] == "" {
		t.Skipf("the go command built no archive for the module's packages")
	}
	if _, ok := deps["sync/atomic"]; !ok {
		t.Fatal("the module does not depend on sync/atomic")
	}

	// The archives -importcfg would name when b is compiled, which is b's
	// direct imports and no more. sync/atomic is not among them, so the
	// writer has to find the declaration inside a's archive.
	archives := []Archive{{Path: holderPath, File: deps[holderPath]}}

	pkg, fset, funcs := buildAgainstArchives(t, wrapPath, foreignWrapSource, deps)
	payload, _, err := Write(pkg, false, &Source{Fset: fset, Funcs: funcs, Archives: archives})
	if err != nil {
		t.Fatalf("Write(%s): %v", wrapPath, err)
	}

	// The declaration is in the file under its own package's name, which is
	// what gc resolves a stub against. A copied package element that kept
	// the empty path its own file wrote would name it under b instead.
	if !holdsObj(payload, wrapPath, "sync/atomic.Pointer") {
		t.Fatal("the export data holds no definition of sync/atomic.Pointer")
	}

	nano := pkgdefArchive(t, tc.Header, mod, "nano-b.a", payload)
	cfg := func(name string) string {
		var b strings.Builder
		paths := make([]string, 0, len(deps))
		for p := range deps {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			file := deps[p]
			if p == wrapPath {
				file = nano
			}
			fmt.Fprintf(&b, "packagefile %s=%s\n", p, file)
		}
		file := filepath.Join(mod, name)
		if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}

	object := filepath.Join(mod, "main.o")
	if out, err := runGo(t, goCmd, mod, "tool", "compile", "-p", "main", "-o", object,
		"-importcfg", cfg("compilecfg"), filepath.Join(mod, "main.go")); err != nil {
		t.Fatalf("gc cannot compile against the declaration nanogo copied: %v\n%s", err, out)
	}
	prog := filepath.Join(mod, "prog")
	if out, err := runGo(t, goCmd, mod, "tool", "link", "-importcfg", cfg("linkcfg"),
		"-o", prog, object); err != nil {
		t.Fatalf("the program does not link: %v\n%s", err, out)
	}
	got, err := exec.Command(prog).CombinedOutput()
	if err != nil {
		t.Fatalf("the program did not run: %v\n%s", err, got)
	}
	if string(got) != want {
		t.Errorf("the program printed %q, the all-gc build printed %q", got, want)
	}
}

// TestWriteRefusesAForeignGenericWithNoArchive is the refusal the copy
// replaces.
//
// A writer given no archive cannot copy, and the declaration it cannot copy is
// refused by name rather than written from what the checker recorded. What the
// checker recorded holds no dictionary numbering and no body, so a declaration
// written from it is one gc reads as having neither.
func TestWriteRefusesAForeignGenericWithNoArchive(t *testing.T) {
	goCmd := goTool(t)
	mod := t.TempDir()
	write := func(name, body string) {
		full := filepath.Join(mod, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/foreign\n\ngo 1.27\n")
	write("a/a.go", foreignHolderSource)

	deps := exportedDeps(t, goCmd, mod, "./a")
	const holderPath = "nanogo.example/foreign/a"
	if deps[holderPath] == "" {
		t.Skipf("the go command built no archive for %s", holderPath)
	}

	pkg, fset, funcs := buildAgainstArchives(t, holderPath, foreignHolderSource, deps)
	_, _, err := Write(pkg, false, &Source{Fset: fset, Funcs: funcs})
	u, ok := err.(*UnsupportedError)
	if !ok {
		t.Fatalf("Write with no archive returned %v, want a refusal", err)
	}
	if u.Name != "sync/atomic.Pointer" {
		t.Errorf("the refusal names %q, want sync/atomic.Pointer", u.Name)
	}
	if !strings.Contains(u.Reason, "no archive the build named") {
		t.Errorf("the refusal says %q, which does not say no archive holds it", u.Reason)
	}
}

// TestCopiedDeclarationIsOneElement checks the rule the public root imposes.
//
// [Write] lists every element of the object section in the public root, so two
// elements for one declaration are two entries naming one symbol and gc
// resolves a stub to whichever it finds first. A reference out of a copied
// element into the object section is therefore routed back through the
// writer's own walk rather than copied (foreign.go).
func TestCopiedDeclarationIsOneElement(t *testing.T) {
	goCmd := goTool(t)
	mod := t.TempDir()
	write := func(name, body string) {
		full := filepath.Join(mod, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/foreign\n\ngo 1.27\n")
	write("a/a.go", foreignHolderSource)

	deps := exportedDeps(t, goCmd, mod, "./a")
	const holderPath = "nanogo.example/foreign/a"
	if deps[holderPath] == "" {
		t.Skipf("the go command built no archive for %s", holderPath)
	}
	archives := make([]Archive, 0, len(deps))
	for p, f := range deps {
		archives = append(archives, Archive{Path: p, File: f})
	}

	pkg, fset, funcs := buildAgainstArchives(t, holderPath, foreignHolderSource, deps)
	payload, _, err := Write(pkg, false, &Source{Fset: fset, Funcs: funcs, Archives: archives})
	if err != nil {
		t.Fatalf("Write(%s): %v", holderPath, err)
	}

	seen := make(map[string]int)
	for _, n := range objectList(holderPath, payload) {
		seen[n]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("the file holds %d elements for %s, want 1", n, name)
		}
	}
	if seen["sync/atomic.Pointer"] != 1 {
		t.Fatalf("the file holds no element for sync/atomic.Pointer")
	}
}

// buildAgainstArchives parses and type checks src as one package, importing
// from the archives the go command built, and builds every body it declares.
func buildAgainstArchives(t *testing.T, path, src string, deps map[string]string) (*types2.Package, *syntax.FileSet, []InlineFunc) {
	t.Helper()
	fset := syntax.NewFileSet()
	sf := fset.AddFile(path+"/a.go", len(src))
	f, err := syntax.Parse(sf, []byte(src), nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := &types2.Info{
		Types:        make(map[syntax.Expr]types2.TypeAndValue),
		Defs:         make(map[*syntax.Name]types2.Object),
		Uses:         make(map[*syntax.Name]types2.Object),
		Implicits:    make(map[syntax.Node]types2.Object),
		Selections:   make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:       make(map[syntax.Node]*types2.Scope),
		Instances:    make(map[*syntax.Name]types2.Instance),
		FileVersions: make(map[*syntax.SrcFile]string),
	}
	conf := types2.Config{
		Fset:     fset,
		Sizes:    types2.SizesFor("gc", "arm64"),
		Importer: &archiveImporter{reader: NewReader(), archives: deps, cache: make(map[string]*types2.Package)},
	}
	pkg, err := conf.Check(path, []*syntax.File{f}, info)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	decls := declaredFuncs(info, []*syntax.File{f})
	names := make([]string, 0, len(decls))
	for name := range decls {
		names = append(names, name)
	}
	sort.Strings(names)
	source := NewBodySource(pkg, info, fset)
	var funcs []InlineFunc
	for _, name := range names {
		fd := decls[name]
		fn := info.Defs[fd.Name].(*types2.Func)
		body, err := source.BuildBody(path+"."+name, fn.Signature(), fd.Body)
		if err != nil {
			continue
		}
		funcs = append(funcs, InlineFunc{Obj: fn, Name: name, Cost: MaxInlineCost, Body: body})
	}
	return pkg, fset, funcs
}

// exportedDeps returns the archive of every package the patterns depend on.
func exportedDeps(t *testing.T, goCmd, dir string, patterns ...string) map[string]string {
	t.Helper()
	if len(patterns) == 0 {
		patterns = []string{"."}
	}
	args := append([]string{"list", "-deps", "-export", "-f", "{{.ImportPath}}\t{{.Export}}"}, patterns...)
	out, err := runGo(t, goCmd, dir, args...)
	if err != nil {
		t.Skipf("go list -deps -export: %v\n%s", err, out)
	}
	deps := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\t", 2)
		if len(f) == 2 && f[1] != "" {
			deps[f[0]] = f[1]
		}
	}
	return deps
}

// objectList returns the declaration each object element of payload defines,
// qualified by its own package.
func objectList(path string, payload []byte) []string {
	dec := pkgbits.NewPkgDecoder(path, string(payload))
	out := make([]string, 0, dec.NumElems(pkgbits.SectionObj))
	for i := range dec.NumElems(pkgbits.SectionObj) {
		p, name, tag := dec.PeekObj(pkgbits.Index(i))
		if tag == pkgbits.ObjStub {
			continue
		}
		out = append(out, p+"."+name)
	}
	return out
}

// holdsObj reports whether payload defines the named declaration.
func holdsObj(payload []byte, path, want string) bool {
	for _, name := range objectList(path, payload) {
		if name == want {
			return true
		}
	}
	return false
}
