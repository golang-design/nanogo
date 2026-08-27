// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"bytes"
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
)

// object writes a package and reads it back, which is how these tests
// build an input without hand-writing bytes.
func object(t *testing.T, p *obj.Package, pkg string) *Object {
	t.Helper()
	var buf bytes.Buffer
	if err := p.WriteObject(&buf, testHeader); err != nil {
		t.Fatalf("writing %s: %v", pkg, err)
	}
	o, err := ReadObject(buf.Bytes(), pkg+".o", pkg)
	if err != nil {
		t.Fatalf("reading %s: %v", pkg, err)
	}
	return o
}

// load builds a symbol table from a list of objects and fails on a
// refusal.
func load(t *testing.T, objs ...*Object) *Loader {
	t.Helper()
	l := NewLoader()
	for _, o := range objs {
		l.AddObject(o)
	}
	if err := l.Load(); err != nil {
		t.Fatalf("building the symbol table: %v", err)
	}
	return l
}

// TestPackageDefinitionsAreNotMergedByName is the rule a reader is most
// likely to flatten.
//
// cmd/link deduplicates by name in the non-package index space only. Two
// packages that both define a package-scope symbol of one name keep two
// symbols, and a reader that keyed every definition by name would return
// one. obj.Package.check guards the same rule on the write side.
func TestPackageDefinitionsAreNotMergedByName(t *testing.T) {
	mk := func(pkg string, size uint32) *Object {
		p := obj.NewPackage(pkg)
		p.AddDef(&obj.Symbol{Name: "shared.V", Type: obj.SNOPTRDATA, Size: size, Align: 8, Data: make([]byte, size)})
		return object(t, p, pkg)
	}
	a, b := mk("example.com/a", 8), mk("example.com/b", 16)
	l := load(t, a, b)

	ga := l.Resolve(a, obj.SymRef{PkgIdx: obj.PkgIdxSelf})
	gb := l.Resolve(b, obj.SymRef{PkgIdx: obj.PkgIdxSelf})
	if ga == gb {
		t.Fatal("two package definitions of one name became one symbol")
	}
	if l.Def(ga).Size != 8 || l.Def(gb).Size != 16 {
		t.Errorf("the two definitions are %d and %d bytes, want 8 and 16", l.Def(ga).Size, l.Def(gb).Size)
	}
	// The name table holds one of them, because a linkname directive can
	// still name a package symbol.
	if g := l.Lookup("shared.V", VerABI0); g != ga && g != gb {
		t.Errorf("the name resolves to %d, which is neither definition", g)
	}
}

// TestNonPackageDefinitionsMergeByName is the other half of the rule.
func TestNonPackageDefinitionsMergeByName(t *testing.T) {
	mk := func(pkg string, size uint32, flag uint8) *Object {
		p := obj.NewPackage(pkg)
		p.AddNonPkgDef(&obj.Symbol{
			Name: "gclocals.shared", Type: obj.SRODATA, Flag: flag,
			Size: size, Align: 1, Data: make([]byte, size),
		})
		return object(t, p, pkg)
	}
	t.Run("two duplicate-tolerant definitions are one symbol", func(t *testing.T) {
		a, b := mk("example.com/a", 4, obj.SymFlagDupok), mk("example.com/b", 4, obj.SymFlagDupok)
		l := load(t, a, b)
		if l.Resolve(a, obj.SymRef{PkgIdx: obj.PkgIdxNone}) != l.Resolve(b, obj.SymRef{PkgIdx: obj.PkgIdxNone}) {
			t.Error("two duplicate-tolerant definitions of one name stayed two symbols")
		}
		if d := l.Duplicates(); len(d) != 0 {
			t.Errorf("the loader reported %v", d)
		}
	})
	t.Run("the larger of two duplicate-tolerant definitions wins", func(t *testing.T) {
		a, b := mk("example.com/a", 4, obj.SymFlagDupok), mk("example.com/b", 12, obj.SymFlagDupok)
		l := load(t, a, b)
		g := l.Resolve(a, obj.SymRef{PkgIdx: obj.PkgIdxNone})
		if s := l.Def(g); s.Size != 12 {
			t.Errorf("the symbol is %d bytes, and a reference through the smaller one would read past 4", s.Size)
		}
	})
	t.Run("a definition that is not duplicate tolerant wins over one that is", func(t *testing.T) {
		a, b := mk("example.com/a", 4, obj.SymFlagDupok), mk("example.com/b", 4, 0)
		l := load(t, a, b)
		g := l.Resolve(a, obj.SymRef{PkgIdx: obj.PkgIdxNone})
		if l.Owner(g) != b {
			t.Errorf("the definition came from %s, and only one of the two is not duplicate tolerant", l.Owner(g).Name)
		}
	})
	t.Run("two definitions with content are refused", func(t *testing.T) {
		a, b := mk("example.com/a", 4, 0), mk("example.com/b", 4, 0)
		l := load(t, a, b)
		if d := l.Duplicates(); len(d) != 1 || !strings.Contains(d[0], "gclocals.shared") {
			t.Errorf("the loader reported %v, and two definitions with content are a build whose symbol holds bytes one package did not write", d)
		}
	})
}

// TestZeroFilledLosesToContent checks the rule for a symbol that a Go file
// declares and an assembly file defines.
func TestZeroFilledLosesToContent(t *testing.T) {
	bss := obj.NewPackage("example.com/a")
	bss.AddNonPkgDef(&obj.Symbol{Name: "shared.g", Type: obj.SNOPTRBSS, Size: 8, Align: 8})
	data := obj.NewPackage("example.com/b")
	data.AddNonPkgDef(&obj.Symbol{Name: "shared.g", Type: obj.SNOPTRDATA, Size: 8, Align: 8, Data: make([]byte, 8)})

	for _, c := range []struct {
		name  string
		order []*Object
		want  string
	}{
		{"content second", []*Object{object(t, bss, "example.com/a"), object(t, data, "example.com/b")}, "example.com/b.o"},
		{"content first", []*Object{object(t, data, "example.com/b"), object(t, bss, "example.com/a")}, "example.com/b.o"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := load(t, c.order...)
			g := l.Lookup("shared.g", VerABI0)
			if got := l.Owner(g).Name; got != c.want {
				t.Errorf("the definition came from %s, want %s", got, c.want)
			}
			if d := l.Duplicates(); len(d) != 0 {
				t.Errorf("the loader reported %v", d)
			}
		})
	}
}

// TestStaticSymbolsStayApart checks the version a file-static symbol gets.
//
// A static is not in the name table, so two objects may define one of the
// same name and the two must not merge. Getting this wrong is silent: the
// program links and one of the two files reads the other's bytes.
func TestStaticSymbolsStayApart(t *testing.T) {
	mk := func(pkg string, b byte) *Object {
		p := obj.NewPackage(pkg)
		p.AddDef(&obj.Symbol{
			Name: "example..stmp_0", ABI: obj.ABIStatic, Type: obj.SRODATA,
			Size: 1, Align: 1, Data: []byte{b},
		})
		return object(t, p, pkg)
	}
	a, b := mk("example.com/a", 1), mk("example.com/b", 2)
	l := load(t, a, b)
	ga := l.Resolve(a, obj.SymRef{PkgIdx: obj.PkgIdxSelf})
	gb := l.Resolve(b, obj.SymRef{PkgIdx: obj.PkgIdxSelf})
	if ga == gb {
		t.Fatal("two file-static symbols of one name became one symbol")
	}
	if l.Def(ga).Data[0] != 1 || l.Def(gb).Data[0] != 2 {
		t.Error("a static symbol resolved to the other file's bytes")
	}
	if g := l.Lookup("example..stmp_0", VerABI0); g != 0 {
		t.Errorf("a static symbol is in the name table as %d, and nothing can name one", g)
	}
}

// TestContentAddressableSymbolsMergeByHash checks that identity is the
// content and not the name, and that the larger of two symbols with one
// hash is the one that is kept.
func TestContentAddressableSymbolsMergeByHash(t *testing.T) {
	mk := func(pkg, name string, data []byte, size uint32) *Object {
		p := obj.NewPackage(pkg)
		p.AddHashedDef(&obj.Symbol{Name: name, Type: obj.SRODATA, Size: size, Align: 1, Data: data})
		p.AddHashed64Def(&obj.Symbol{Name: name + ".short", Type: obj.SRODATA, Size: 8, Align: 8, Data: []byte{7, 0, 0, 0, 0, 0, 0, 0}})
		return object(t, p, pkg)
	}
	// The same bytes under two names are one symbol.
	a := mk("example.com/a", "go:string.hi", []byte("hi"), 2)
	b := mk("example.com/b", "go:string.other", []byte("hi"), 2)
	l := load(t, a, b)
	ga := l.Resolve(a, obj.SymRef{PkgIdx: obj.PkgIdxHashed})
	gb := l.Resolve(b, obj.SymRef{PkgIdx: obj.PkgIdxHashed})
	if ga != gb {
		t.Error("two symbols with one content hash stayed two symbols")
	}
	if got := l.Name(ga); got != "go:string.hi" {
		t.Errorf("the symbol kept the name %q, and the first definition of a hash is the one the program refers to", got)
	}
	// The short space merges by content the same way.
	if l.Resolve(a, obj.SymRef{PkgIdx: obj.PkgIdxHashed64}) != l.Resolve(b, obj.SymRef{PkgIdx: obj.PkgIdxHashed64}) {
		t.Error("two short content-addressable symbols with one content stayed two symbols")
	}
	// A hashed symbol is not in the name table: nothing refers to one by
	// name, and two of them may share a name and differ in content.
	if g := l.Lookup("go:string.hi", VerABI0); g != 0 {
		t.Errorf("a content-addressable symbol is in the name table as %d", g)
	}
}

// TestReferencesResolve checks the four ways a reference names a symbol.
func TestReferencesResolve(t *testing.T) {
	dep := obj.NewPackage("example.com/dep")
	dep.AddDef(&obj.Symbol{Name: "example.com/dep.F", ABI: obj.ABIInternal, Type: obj.STEXT, Size: 4, Align: 4, Data: []byte{0, 0, 0, 0}})
	depObj := object(t, dep, "example.com/dep")

	p := obj.NewPackage("example.com/main")
	p.AddImport("example.com/dep", [8]byte{})
	self := p.AddDef(&obj.Symbol{Name: "example.com/main.G", Type: obj.SNOPTRDATA, Size: 8, Align: 8, Data: make([]byte, 8)})
	cross := p.PkgRef("example.com/dep", 0)
	// A builtin reference carries an index into the pinned list and no
	// name, so it resolves only through that list.
	newobject := builtinIndex("runtime.newobject", obj.ABIInternal)
	if newobject < 0 {
		t.Fatal("runtime.newobject is not in the builtin list")
	}
	builtinRef := obj.SymRef{PkgIdx: obj.PkgIdxBuiltin, SymIdx: uint32(newobject)}
	missing := obj.SymRef{PkgIdx: obj.PkgIdxNone, SymIdx: 0}
	p.AddNonPkgRef(&obj.Symbol{Name: "example.com/other.H", ABI: obj.ABIInternal})
	p.AddDef(&obj.Symbol{
		Name: "example.com/main.F", ABI: obj.ABIInternal, Type: obj.STEXT, Size: 32, Align: 4,
		Data: make([]byte, 32),
		Relocs: []obj.Reloc{
			{Off: 0, Size: 8, Type: obj.R_ADDR, Sym: self},
			{Off: 8, Size: 8, Type: obj.R_ADDR, Sym: cross},
			{Off: 16, Size: 4, Type: obj.R_CALLARM64, Sym: builtinRef},
			{Off: 24, Size: 4, Type: obj.R_CALLARM64, Sym: missing},
		},
	})
	mainObj := object(t, p, "example.com/main")

	rt := obj.NewPackage("runtime")
	rt.AddDef(&obj.Symbol{Name: "runtime.newobject", ABI: obj.ABIInternal, Type: obj.STEXT, Size: 4, Align: 4, Data: []byte{0, 0, 0, 0}})
	rtObj := object(t, rt, "runtime")

	l := load(t, mainObj, depObj, rtObj)
	relocs := mainObj.Defs[1].Relocs
	for _, c := range []struct {
		i    int
		want string
	}{
		{0, "example.com/main.G"},
		{1, "example.com/dep.F"},
		{2, "runtime.newobject"},
		{3, "example.com/other.H"},
	} {
		if got := l.Name(l.Resolve(mainObj, relocs[c.i].Sym)); got != c.want {
			t.Errorf("relocation %d resolves to %q, want %q", c.i, got, c.want)
		}
	}
	// The one nothing defines is still a symbol, so the stage that reports
	// it can name it.
	if u := l.Undefined(); len(u) != 1 || u[0] != "example.com/other.H" {
		t.Errorf("the undefined symbols are %v, want one", u)
	}
	if l.Owner(l.Resolve(mainObj, relocs[3].Sym)) != nil {
		t.Error("an undefined symbol has an owner")
	}
	// A reference to an object that is not in the link is a refusal and
	// not a nil symbol.
	l2 := NewLoader()
	l2.AddObject(mainObj)
	l2.AddObject(rtObj)
	err := l2.Load()
	if err == nil || !strings.Contains(err.Error(), "not in the link") {
		t.Errorf("loading without the referenced package gave %v", err)
	}
}

// TestResolveEdges covers the references that name nothing.
func TestResolveEdges(t *testing.T) {
	p := obj.NewPackage("example.com/a")
	p.AddDef(&obj.Symbol{Name: "example.com/a.V", Type: obj.SNOPTRDATA, Size: 8, Align: 8, Data: make([]byte, 8)})
	o := object(t, p, "example.com/a")
	l := load(t, o)

	for _, c := range []struct {
		name string
		ref  obj.SymRef
	}{
		{"the nil symbol", obj.SymRef{}},
		{"a builtin past the end of the list", obj.SymRef{PkgIdx: obj.PkgIdxBuiltin, SymIdx: uint32(NumBuiltin())}},
	} {
		if g := l.Resolve(o, c.ref); g != 0 {
			t.Errorf("%s resolved to %d", c.name, g)
		}
	}
	if l.Resolve(&Object{Name: "not in the link"}, obj.SymRef{}) != 0 {
		t.Error("a reference from an object the loader does not hold resolved")
	}
	if l.Def(0) != nil || l.Name(0) != "" || l.Owner(0) != nil {
		t.Error("the nil symbol has a definition")
	}
	if l.Def(Global(l.NSym())) != nil {
		t.Error("a symbol past the end of the table has a definition")
	}
	if l.Lookup("example.com/a.V", 7) != 0 {
		t.Error("a lookup at a version the table does not hold resolved")
	}
	if !l.UsedInIface(0) == false {
		t.Error("the nil symbol reached an interface")
	}
}

// TestBuiltinName covers the pinned list's accessors.
func TestBuiltinName(t *testing.T) {
	name, abi, ok := BuiltinName(0)
	if !ok || name != "runtime.newobject" || abi != obj.ABIInternal {
		t.Errorf("BuiltinName(0) = %q, %d, %v", name, abi, ok)
	}
	if _, _, ok := BuiltinName(-1); ok {
		t.Error("a negative index names a builtin")
	}
	if _, _, ok := BuiltinName(NumBuiltin()); ok {
		t.Error("an index past the end names a builtin")
	}
	if builtinIndex("example.com/a.F", obj.ABI0) != -1 {
		t.Error("a symbol that is not predeclared has an index")
	}
}
