// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
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
		name: "a generic type",
		src:  "package p\n\ntype List[T any] struct{ Head T }\n",
		want: "generic type",
	}, {
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
