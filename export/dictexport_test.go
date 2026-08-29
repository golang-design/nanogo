// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The object dictionary a generic declaration carries into the file.
//
// bodydict.go allocates the slots and the body names them. This is the other
// end: the dictionary element the file holds has to be the same dictionary,
// entry for entry, because the body's slot numbers mean nothing against any
// other one. A dictionary with one entry too few is a slot gc resolves to
// whatever follows it, which gc reads without complaint.

// genericSource declares one generic function that names every kind of slot
// the writer fills except a method expression on a type parameter.
const genericSource = "package p\n\n" +
	// A parameter and a result of a derived type, a local of one, and an
	// index of one: each is a runtime type the dictionary holds.
	"func Last[T any](xs []T) T {\n" +
	"\tvar zero T\n" +
	"\tif len(xs) == 0 {\n\t\treturn zero\n\t}\n" +
	"\treturn xs[len(xs)-1]\n" +
	"}\n\n" +
	// A composite literal of a derived slice type.
	"func Pair[T any](a, b T) []T { return []T{a, b} }\n\n" +
	// A call of one generic declaration from another, whose type argument
	// is derived: the callee's dictionary is a subdictionary slot.
	"func Ends[T any](xs []T) []T { return Pair(xs[0], Last(xs)) }\n\n" +
	// A value of a derived type converted to an interface, which is a
	// method table the dictionary holds.
	"func Any[T any](x T) any { return x }\n"

// dictCounts is what one object dictionary element of the file holds.
type dictCounts struct {
	implicits, receivers, typeParams int
	derived                          int
	methodExprs, subdicts            int
	rtypes, itabs                    int
}

// readDict decodes the dictionary element of one declaration.
//
// The decode is the writer's own order read back, so a field the writer moved
// is a field this stops on rather than a count it silently misreads.
func readDict(t *testing.T, dec *pkgbits.PkgDecoder, name string) dictCounts {
	t.Helper()
	idx := -1
	for i := range dec.NumElems(pkgbits.SectionObj) {
		if _, have, _ := dec.PeekObj(pkgbits.Index(i)); have == name {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("the file holds no declaration named %s", name)
	}

	r := dec.NewDecoder(pkgbits.SectionObjDict, pkgbits.Index(idx), pkgbits.SyncObject1)
	typeRef := func() {
		r.Sync(pkgbits.SyncType)
		if r.Bool() {
			r.Len()
			return
		}
		r.Reloc(pkgbits.SectionType)
	}

	var c dictCounts
	c.implicits = r.Len()
	c.receivers = r.Len()
	c.typeParams = r.Len()
	for range c.receivers + c.typeParams {
		typeRef()
	}
	c.derived = r.Len()
	for range c.derived {
		r.Reloc(pkgbits.SectionType)
	}
	for range c.implicits + c.receivers + c.typeParams {
		r.Bool()
	}
	c.methodExprs = r.Len()
	for range c.methodExprs {
		r.Len()
		r.Sync(pkgbits.SyncSelector)
		r.Sync(pkgbits.SyncPkg)
		r.Reloc(pkgbits.SectionPkg)
		_ = r.String()
	}
	c.subdicts = r.Len()
	for range c.subdicts {
		r.Sync(pkgbits.SyncObject)
		r.Reloc(pkgbits.SectionObj)
		for range r.Len() {
			typeRef()
		}
	}
	c.rtypes = r.Len()
	for range c.rtypes {
		typeRef()
	}
	c.itabs = r.Len()
	for range c.itabs {
		typeRef()
		typeRef()
	}
	return c
}

// TestWriteCarriesTheDictionaryTheBodyWasBuiltAgainst is the invariant the
// slot numbers rest on.
//
// The body names a slot by number and nothing else, so the file has to hold
// the dictionary those numbers were allocated in. Comparing the counts the
// file holds with the lists the builder filled is what says the writer wrote
// that dictionary and not another one.
func TestWriteCarriesTheDictionaryTheBodyWasBuiltAgainst(t *testing.T) {
	payload, _, funcs := writeSource(t, "xtest/dict", genericSource, nil)
	dec := pkgbits.NewPkgDecoder("xtest/dict", string(payload))

	filled := 0
	for _, fn := range funcs {
		d := fn.Body.Dict
		if d == nil {
			t.Fatalf("%s carries no dictionary", fn.Name)
		}
		got := readDict(t, &dec, fn.Name)
		want := dictCounts{
			implicits:   len(d.Implicits),
			receivers:   len(d.Receivers),
			typeParams:  len(d.TypeParams),
			derived:     len(d.Derived),
			methodExprs: len(d.MethodExprs),
			subdicts:    len(d.Subdicts),
			rtypes:      len(d.RTypes),
			itabs:       len(d.Itabs),
		}
		if got != want {
			t.Errorf("%s: the file holds %+v and the body was built against %+v", fn.Name, got, want)
		}
		filled += got.derived + got.rtypes + got.itabs + got.subdicts
	}
	if filled == 0 {
		t.Fatal("no dictionary holds an entry, so the test proved nothing")
	}
}

// TestWriteFillsEachDictionaryList names one declaration per list, so that a
// list left at zero is reported by which list it is.
func TestWriteFillsEachDictionaryList(t *testing.T) {
	payload, _, _ := writeSource(t, "xtest/lists", genericSource, nil)
	dec := pkgbits.NewPkgDecoder("xtest/lists", string(payload))

	for _, tt := range []struct {
		name string
		list string
		got  func(dictCounts) int
	}{
		{"Last", "derived types", func(c dictCounts) int { return c.derived }},
		{"Last", "runtime types", func(c dictCounts) int { return c.rtypes }},
		{"Ends", "subdictionaries", func(c dictCounts) int { return c.subdicts }},
		{"Any", "itabs", func(c dictCounts) int { return c.itabs }},
	} {
		c := readDict(t, &dec, tt.name)
		if n := tt.got(c); n == 0 {
			t.Errorf("%s holds no %s, and its body names one", tt.name, tt.list)
		}
		if c.typeParams != 1 {
			t.Errorf("%s holds %d type parameters and declares one", tt.name, c.typeParams)
		}
	}
}

// TestWriteNumbersAGenericTypeAsGcDoes compares the dictionary nanogo writes
// for a generic type with the one gc writes for the same source.
//
// The dictionary and the bodies numbered against it are both nanogo's, so a
// numbering of nanogo's own would be self consistent and gc would read it.
// This is the stronger claim: the slots are gc's slots, so a shape gc walks in
// another order is a difference this reports rather than one that waits for a
// declaration whose two orders disagree.
//
// The alias is here because its type parameters are objects of the alias
// declaration and not of the type it names. gc gives it a dictionary of its
// own, and an alias handed the type's would hold the type's entries and take
// slots in it besides.
func TestWriteNumbersAGenericTypeAsGcDoes(t *testing.T) {
	const path = "nanogo.example/gcdict/lib"
	src := "package lib\n\n" +
		"type List[T any] struct{ items []T }\n\n" +
		"func (l *List[T]) Push(v T) { l.items = append(l.items, v) }\n\n" +
		"func (l List[T]) Len() int { return len(l.items) }\n\n" +
		"func (l List[T]) All() []T { return l.items }\n\n" +
		"func (l List[T]) Any(i int) any { return l.items[i] }\n\n" +
		"type Alias[T any] = List[T]\n\n" +
		"type Pair[K, V any] struct {\n\tk K\n\tv V\n}\n\n" +
		"func (p Pair[K, V]) Key() K { return p.k }\n\n" +
		"func (p *Pair[K, V]) Set(k K, v V) { p.k, p.v = k, v }\n\n" +
		"func (p Pair[K, V]) Swap() Pair[V, K] { return Pair[V, K]{p.v, p.k} }\n"

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go.mod":     "module nanogo.example/gcdict\n\ngo 1.27\n",
		"lib/lib.go": src,
		"lib/use.go": "package lib\n\nvar _ List[int]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := os.ReadFile(exportFile(t, dir, "./lib"))
	if err != nil {
		t.Fatal(err)
	}
	gcPayload, err := Payload(archive)
	if err != nil {
		t.Fatal(err)
	}
	gcDec := pkgbits.NewPkgDecoder(path, string(gcPayload))

	payload, _, _ := writeSource(t, path, src+"\nvar _ List[int]\n", nil)
	dec := pkgbits.NewPkgDecoder(path, string(payload))

	for _, name := range []string{"List", "Alias", "Pair"} {
		got, want := readDict(t, &dec, name), readDict(t, &gcDec, name)
		if got != want {
			t.Errorf("nanogo numbered %s %+v and gc numbered it %+v", name, got, want)
		}
		if want.derived == 0 {
			t.Errorf("gc numbered no derived type for %s, so the test proved nothing about it", name)
		}
	}
}

// TestWriteRefusesAGenericTypeOfAnotherPackageWithMethods names the half of
// the rule that needs the reader.
//
// A file's export data is the linked form, so a generic type another package
// declares is written out here in full, methods and all. A method of a generic
// type carries its body and nothing else: gc's reader records a definition for
// a declaration with type parameters before it reads the extension data, and
// the relocated form asserts there is none. The body exists only in the
// declaring package's archive, and this writer builds bodies from source and
// does not read one back, so the type is refused by name.
//
// sync/atomic.Pointer is the case that reaches nanogo's own packages: a
// package holding one cannot write its export data until the body of Load can
// be copied out of sync/atomic's archive.
func TestWriteRefusesAGenericTypeOfAnotherPackageWithMethods(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/foreign\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := archives(t, dir, "sync/atomic")
	files := make(map[string]string, len(found))
	for _, a := range found {
		files[a[0]] = a[1]
	}
	if files["sync/atomic"] == "" {
		t.Skip("the go command built no archive for sync/atomic")
	}
	imp := &archiveImporter{reader: NewReader(), archives: files, cache: make(map[string]*types2.Package)}

	const src = "package p\n\nimport \"sync/atomic\"\n\nvar P atomic.Pointer[int]\n"
	fset := syntax.NewFileSet()
	sf := fset.AddFile("xtest/foreign/a.go", len(src))
	f, err := syntax.Parse(sf, []byte(src), nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types2.Config{Fset: fset, Sizes: types2.SizesFor("gc", "arm64"), Importer: imp}
	pkg, err := conf.Check("xtest/foreign", []*syntax.File{f}, nil)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	_, _, err = Write(pkg, false, &Source{Fset: fset})
	if err == nil {
		t.Fatal("the writer wrote a generic type of another package that declares methods")
	}
	if !strings.Contains(err.Error(), "sync/atomic.Pointer") {
		t.Errorf("the refusal is %q and does not name the type", err)
	}
	if !strings.Contains(err.Error(), "that package's archive") {
		t.Errorf("the refusal is %q and does not say where the bodies are", err)
	}
}

// TestWriteRefusesTheGenericShapesItHasNoDictionaryFor names each shape whose
// dictionary is not the one a body carries.
//
// A refusal is a package that does not compile. The alternative is a
// dictionary numbered against the wrong declaration, which gc reads as a
// different type and does not complain about.
func TestWriteRefusesTheGenericShapesItHasNoDictionaryFor(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  string
		want string
	}{{
		name: "a method with type parameters",
		src:  "package p\n\ntype T struct{}\n\nfunc (T) M[X any](x X) X { return x }\n",
		want: "receiver's type parameters ahead of the method's",
	}} {
		t.Run(tt.name, func(t *testing.T) {
			pkg, fset, funcs := buildSource(t, "xtest/refuse", tt.src)
			_, _, err := Write(pkg, false, &Source{Fset: fset, Funcs: funcs})
			if err == nil {
				t.Fatalf("the writer wrote %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the refusal is %q and does not name %q", err, tt.want)
			}
		})
	}
}

// TestWriteRefusesAGenericTypeWhoseMethodHasNoBody is the rule a generic type
// with methods cannot be written around.
//
// The dictionary is the type's, and its slots run over the underlying type and
// then over each method in declaration order. A method whose body was not
// built leaves the slots it would have taken out of the dictionary, so every
// method declared after it is numbered too low, and gc reads a slot numbered
// too low as a different type without complaint.
func TestWriteRefusesAGenericTypeWhoseMethodHasNoBody(t *testing.T) {
	src := "package p\n\ntype List[T any] struct{ Head T }\n\nfunc (l List[T]) Get() T { return l.Head }\n"
	pkg, fset, _ := buildSource(t, "xtest/nomethodbody", src)
	_, _, err := Write(pkg, false, &Source{Fset: fset})
	if err == nil {
		t.Fatal("the writer wrote a method of a generic type with no body")
	}
	if !strings.Contains(err.Error(), "no body was built") {
		t.Errorf("the refusal is %q and does not say the body is missing", err)
	}
}

// TestWriteWritesAGenericTypeWithMethods checks that one dictionary carries
// the type and every method it declares.
//
// Two dictionaries would be two numberings, and a body numbered against the
// second one names slots the file holds under the first. So the check is that
// every method's body carries the one allocation, and that its entries are in
// the order gc writes them in: the underlying type first, then each method's
// receiver and signature in declaration order.
func TestWriteWritesAGenericTypeWithMethods(t *testing.T) {
	src := "package p\n\n" +
		"type List[T any] struct{ Head T }\n\n" +
		"func (l List[T]) Get() T { return l.Head }\n\n" +
		"func (l *List[T]) Set(v T) { l.Head = v }\n"
	pkg, fset, funcs := buildSource(t, "xtest/genericmethods", src)
	if len(funcs) != 2 {
		t.Fatalf("the harness built %d bodies and the type declares two methods", len(funcs))
	}
	dict := funcs[0].Body.Dict
	for _, fn := range funcs {
		if fn.Body.Dict != dict {
			t.Fatalf("%s carries a dictionary of its own and the type's is shared", fn.Name)
		}
	}
	// The underlying type takes the first slots, because gc writes it before
	// the first method. Then each method's receiver, in declaration order.
	//
	// Each method's receiver names a type parameter of its own: the checker
	// makes the T of "func (l List[T])" an object of the method declaration
	// and not the object the type declared, so List[T] under one method is
	// not the value the checker built for List[T] under another. gc keys the
	// dictionary by the same identity ([Dict.Derive]) and allocates the same
	// slots, and [Dict.TypeParamIndex] is what makes every one of them
	// resolve to the type's own position.
	want := []string{
		"T", "struct{Head T}",
		"T", "p.List[T]",
		"T", "p.List[T]", "*p.List[T]",
	}
	got := make([]string, len(dict.Derived))
	qual := func(pkg *types2.Package) string { return pkg.Name() }
	for i, typ := range dict.Derived {
		got[i] = types2.TypeString(typ, qual)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the derived types are %v and gc's order is %v", got, want)
	}
	if _, _, err := Write(pkg, false, &Source{Fset: fset, Funcs: funcs}); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// TestWriteWritesAGenericTypeWithNoMethods is the half of the generic type
// that needs no body at all.
//
// A generic type's dictionary spans the type and every method it declares, so
// it is allocated with the type and filled as the underlying type and then
// each method's signature is written. A type with no method needs nothing but
// the first of those, and it is what iter.Seq is: a program that names
// iter.Seq[int] could not write its own export data while every generic type
// was refused by name.
func TestWriteWritesAGenericTypeWithNoMethods(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  string
	}{
		{"a struct", "package p\n\ntype List[T any] struct{ Head T }\n"},
		{"a type whose underlying type is derived", "package p\n\ntype Slice[T any] []T\n"},
		{"a function type", "package p\n\ntype Seq[V any] func(yield func(V) bool)\n"},
		{"one named by an ordinary declaration", "package p\n\ntype List[T any] struct{ Head T }\n\nvar Ints List[int]\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pkg, fset, funcs := buildSource(t, "xtest/generictype", tt.src)
			if _, _, err := Write(pkg, false, &Source{Fset: fset, Funcs: funcs}); err != nil {
				t.Fatalf("Write: %v", err)
			}
		})
	}
}

// TestWriteRefusesAGenericDeclarationWithNoBody is the rule a generic
// declaration cannot be written around.
//
// gc records a definition for a declaration with type parameters before it
// reads its extension data, and the relocated form of that data asserts there
// is none. So the only shape a generic declaration has in a file is the one
// that carries a body, and a declaration whose body was not built is refused
// rather than written without one.
func TestWriteRefusesAGenericDeclarationWithNoBody(t *testing.T) {
	pkg, fset, _ := buildSource(t, "xtest/nobody", "package p\n\nfunc Id[T any](x T) T { return x }\n")
	_, _, err := Write(pkg, false, &Source{Fset: fset})
	if err == nil {
		t.Fatal("the writer wrote a generic declaration with no body")
	}
	if !strings.Contains(err.Error(), "no body was built") {
		t.Errorf("the refusal is %q and does not say the body is missing", err)
	}
}
