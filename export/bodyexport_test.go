// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"sort"
	"strings"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The writer's own resolver, measured through nanogo's reader.
//
// bodywrite_test.go proves the layout against gc's bytes and says nothing
// about which element a reference names, because the tree it encodes carries
// gc's own indices. This is the other half: the tree here was built from
// syntax and carries no index at all, so every reference is one the writer
// allocated. Reading the file back is what says the allocation was right.

// buildSource parses and type checks src as one package and builds the body of
// every function it declares, in name order so that the set is fixed.
func buildSource(t *testing.T, path, src string) (*types2.Package, *syntax.FileSet, []InlineFunc) {
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
	conf := types2.Config{Fset: fset, Sizes: types2.SizesFor("gc", "arm64")}
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
		obj := info.Defs[fd.Name].(*types2.Func)
		body, err := source.BuildBody(path+"."+name, obj.Signature(), fd.Body)
		if err != nil {
			t.Fatalf("BuildBody(%s): %v", name, err)
		}
		funcs = append(funcs, InlineFunc{Obj: obj, Name: name, Cost: MaxInlineCost, Body: body})
	}
	return pkg, fset, funcs
}

// writeSource builds every body of src and writes the export data.
func writeSource(t *testing.T, path, src string, file func(string) string) ([]byte, *types2.Package, []InlineFunc) {
	t.Helper()
	pkg, fset, funcs := buildSource(t, path, src)
	payload, _, err := Write(pkg, false, &Source{Fset: fset, Funcs: funcs, File: file})
	if err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
	return payload, pkg, funcs
}

// readBack reads the bodies out of what the writer produced.
func readBack(t *testing.T, path string, payload []byte) []*FuncBody {
	t.Helper()
	dec := pkgbits.NewPkgDecoder(path, string(payload))
	checkStubs(t, path, &dec)
	_, bodies, err := ReadBodies(types2.NewContext(), map[string]*types2.Package{}, dec)
	if err != nil {
		t.Fatalf("ReadBodies(%s): %v", path, err)
	}
	return bodies
}

// TestWriteCarriesABodyToTheFile is the narrowest shape the wiring has: one
// function whose body reaches the file and comes back out of it.
//
// It is the first check of the writer's resolver. Every element the body
// names is one the writer allocated, so a body that decodes with no byte and
// no reference left over says the allocation named the elements it meant to.
func TestWriteCarriesABodyToTheFile(t *testing.T) {
	const src = `package p

type T struct{ N int }

func (t *T) Get() int { return t.N }

func Add(a, b int) int { return a + b }
`
	payload, _, funcs := writeSource(t, "xtest/p", src, nil)
	if len(funcs) != 2 {
		t.Fatalf("built %d bodies, want 2", len(funcs))
	}

	bodies := readBack(t, "xtest/p", payload)
	got := make(map[string]bool)
	for _, b := range bodies {
		if b.Generic {
			t.Errorf("%s is reached through the extension data, which is the generic path", b.Name)
		}
		if b.Path != "xtest/p" {
			t.Errorf("%s is listed under path %q, want %q", b.Name, b.Path, "xtest/p")
		}
		got[b.Name] = true
	}
	for _, want := range []string{"Add", "(*T).Get"} {
		if !got[want] {
			t.Errorf("the file carries no body for %s; it carries %v", want, got)
		}
	}
}

// TestWriteRecordsADeclarationsPosition is the other half of the position
// base section: a declaration nanogo compiled carries where it stands.
//
// It is not only a diagnostic. gc rebases every position of an inlined body
// onto the call it inlined into, and it rebases the parameters with the rest,
// so an absent parameter position stops gc with "no old PosBase".
func TestWriteRecordsADeclarationsPosition(t *testing.T) {
	const src = `package p

func Add(a, b int) int { return a + b }
`
	pkg, fset, _ := buildSource(t, "xtest/pos", src)
	payload, _, err := Write(pkg, false, &Source{Fset: fset})
	if err != nil {
		t.Fatal(err)
	}
	dec := pkgbits.NewPkgDecoder("xtest/pos", string(payload))
	if n := dec.NumElems(pkgbits.SectionPosBase); n != 1 {
		t.Fatalf("the file holds %d position bases and carries no body, want 1", n)
	}
	r := dec.NewDecoder(pkgbits.SectionPosBase, 0, pkgbits.SyncPosBase)
	if got, want := r.String(), "xtest/pos/a.go"; got != want {
		t.Errorf("the position base names %q, want %q", got, want)
	}
	if !r.Bool() {
		t.Error("the position base is not a file base")
	}
}

// TestWriteAllocatesAPositionBase is the position base section, which the
// writer never allocated an element of until a body carried a position.
func TestWriteAllocatesAPositionBase(t *testing.T) {
	const src = `package p

func Add(a, b int) int { return a + b }
`
	payload, _, _ := writeSource(t, "xtest/q", src, func(name string) string {
		return "trimmed/" + name
	})
	dec := pkgbits.NewPkgDecoder("xtest/q", string(payload))
	if n := dec.NumElems(pkgbits.SectionPosBase); n != 1 {
		t.Fatalf("the file holds %d position bases, want 1", n)
	}

	bodies := readBack(t, "xtest/q", payload)
	if len(bodies) != 1 {
		t.Fatalf("the file carries %d bodies, want 1", len(bodies))
	}
	pos := bodies[0].Body.Rbrace
	if !pos.Known {
		t.Fatal("the body's closing brace has no position")
	}
	if want := "trimmed/xtest/q/a.go"; pos.File != want {
		t.Errorf("the position names file %q, want %q", pos.File, want)
	}
	if pos.Line != 3 {
		t.Errorf("the closing brace is on line %d, want 3", pos.Line)
	}
}

// litSource is a package whose one function holds a function literal.
const litSource = `package p

func Make(n int) func() int {
	return func() int { return n }
}
`

// TestWriteEncodesAFunctionLiteralAsItsOwnElement is the case that makes a
// body a tree of elements rather than one element.
func TestWriteEncodesAFunctionLiteralAsItsOwnElement(t *testing.T) {
	pkg, _, funcs := buildSource(t, "xtest/r", litSource)
	pw := newPkgWriter(pkg, nil, nil, nil, nil)
	pw.writeBody(&declRefs{pw: pw}, "xtest/r", funcs[0].Name, funcs[0].Body, nil)
	if n := pw.NumElems(pkgbits.SectionBody); n != 2 {
		t.Fatalf("the writer wrote %d body elements, want 2", n)
	}
}

// TestWriteDoesNotOfferABodyWithAFunctionLiteral is the first shape's limit.
//
// The writer encodes a function literal's body, and the set nanogo offers gc
// for inlining does not hold one yet. So the element is writable and the body
// is not offered, and the file carries neither.
func TestWriteDoesNotOfferABodyWithAFunctionLiteral(t *testing.T) {
	payload, _, _ := writeSource(t, "xtest/r", litSource, nil)
	if bodies := readBack(t, "xtest/r", payload); len(bodies) != 0 {
		t.Fatalf("the file carries %d bodies, want none", len(bodies))
	}
}

// TestWriteLeavesOutABodyItCannotAllocate is the rule that keeps a guessed
// index out of the file.
//
// A body that names a generic declaration needs an object element the writer
// refuses to write. The declaration the body belongs to is still written, so
// the package still exports; the body is what is left out.
func TestWriteLeavesOutABodyItCannotAllocate(t *testing.T) {
	const src = `package p

func id[T any](v T) T { return v }

func Use(n int) int { return id(n) }

func Add(a, b int) int { return a + b }
`
	fset := syntax.NewFileSet()
	sf := fset.AddFile("xtest/s/a.go", len(src))
	f, err := syntax.Parse(sf, []byte(src), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
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
	conf := types2.Config{Fset: fset, Sizes: types2.SizesFor("gc", "arm64")}
	pkg, err := conf.Check("xtest/s", []*syntax.File{f}, info)
	if err != nil {
		t.Fatal(err)
	}
	// The exported surface is what the writer refuses a generic declaration
	// for, so the package here exports only the two ordinary functions.
	src2 := NewBodySource(pkg, info, fset)
	var funcs []InlineFunc
	for _, name := range []string{"Use", "Add"} {
		obj := pkg.Scope().Lookup(name).(*types2.Func)
		fd := declaredFuncs(info, []*syntax.File{f})[name]
		body, err := src2.BuildBody("xtest/s."+name, obj.Signature(), fd.Body)
		if err != nil {
			t.Fatalf("BuildBody(%s): %v", name, err)
		}
		funcs = append(funcs, InlineFunc{Obj: obj, Name: name, Cost: 1, Body: body})
	}

	kept := writableBodies(pkg, nil, nil, funcs)
	if len(kept) != 1 || kept[0].Name != "Add" {
		names := make([]string, len(kept))
		for i, k := range kept {
			names[i] = k.Name
		}
		t.Fatalf("the writer kept the bodies of %v, want only Add", names)
	}
}

// TestWriteRefusesAGenericBodyItWasHanded is the guarantee underneath the
// filter: a body the writer cannot allocate an element for is refused rather
// than written with an index nothing holds.
func TestWriteRefusesAGenericBodyItWasHanded(t *testing.T) {
	pw := newPkgWriter(types2.NewPackage("xtest/t", "p"), nil, nil, nil, nil)
	body := &Body{
		HasBlock: true,
		Stmts: []Stmt{&ReturnStmt{
			Pos:     Pos{Known: true, File: "a.go", Line: 1},
			Results: MultiExpr{Exprs: []Expr{&ZeroExpr{Pos: Pos{Known: true, File: "a.go", Line: 1}, Type: TypeUse{Derived: true, Idx: 0}}}},
		}},
	}
	err := func() (err error) {
		defer func() {
			if v := recover(); v != nil {
				err, _ = v.(error)
			}
		}()
		pw.writeBody(&declRefs{pw: pw}, "xtest/t", "F", body, nil)
		return nil
	}()
	if err == nil {
		t.Fatal("a body naming a derived type was written")
	}
	if !strings.Contains(err.Error(), "dictionary") {
		t.Errorf("the refusal is %q and does not name the dictionary", err)
	}
}

// TestWriteDoesNotOfferABodyGcCannotInline names the two shapes that make an
// inlined body a wrong program rather than a slow one, and the one gc's own
// inliner refuses outright.
func TestWriteDoesNotOfferABodyGcCannotInline(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"recover", `package p

func F() any { return recover() }
`},
		{"defer", `package p

func F(f func()) { defer f() }
`},
		{"go", `package p

func F(f func()) { go f() }
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg, _, funcs := buildSource(t, "xtest/"+tc.name, tc.src)
			if len(funcs) != 1 {
				t.Fatalf("built %d bodies, want 1", len(funcs))
			}
			if kept := writableBodies(pkg, nil, nil, funcs); len(kept) != 0 {
				t.Fatalf("the writer offered %s for inlining", kept[0].Name)
			}
		})
	}
}

// TestWriteOffersAnOrdinaryBody is the other side of the check: a body with
// none of those shapes is offered, so the check is a filter and not a wall.
func TestWriteOffersAnOrdinaryBody(t *testing.T) {
	const src = `package p

func Abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
`
	pkg, _, funcs := buildSource(t, "xtest/ok", src)
	kept := writableBodies(pkg, nil, nil, funcs)
	if len(kept) != 1 {
		t.Fatalf("the writer offered %d bodies, want 1", len(kept))
	}
	if kept[0].Cost != MaxInlineCost {
		t.Errorf("the body is offered at cost %d, want %d", kept[0].Cost, MaxInlineCost)
	}
}

// TestWriteDoesNotOfferABodyTheFileCannotPair is the rule that keeps the
// private root's list answerable.
//
// The entry names its declaration rather than pointing at it, so an entry
// naming a declaration the file holds no element for is one no reader can
// pair with anything. nanogo's own reader refuses such a file by name.
func TestWriteDoesNotOfferABodyTheFileCannotPair(t *testing.T) {
	const src = `package p

func Add(a, b int) int { return a + b }

func sub(a, b int) int { return a - b }
`
	payload, _, funcs := writeSource(t, "xtest/pair", src, nil)
	if len(funcs) != 2 {
		t.Fatalf("built %d bodies, want 2", len(funcs))
	}
	bodies := readBack(t, "xtest/pair", payload)
	if len(bodies) != 1 || bodies[0].Name != "Add" {
		names := make([]string, len(bodies))
		for i, b := range bodies {
			names[i] = b.Name
		}
		t.Fatalf("the file carries bodies for %v, want only Add", names)
	}
}
