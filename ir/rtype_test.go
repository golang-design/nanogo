// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"strings"
	"testing"
)

// The canonical names, checked against the names gc gives the same types.
//
// The spellings below were read out of a gc-compiled object with go tool nm
// rather than recalled, because specs/032-type-descriptors-and-itabs.md makes
// the linker's deduplication a function of the name: a name that differs from
// gc's by one character is a second descriptor for a type that already has
// one, and the failure is silent.
//
// rtype's own test closes the loop from the other end. It hashes the link
// string and compares the result with the hash gc put in the descriptor
// reflect is reading, so a link string that is wrong fails there as a hash
// mismatch even though nothing here compares strings with a running program.

// nameCorpus is the type as the checker reads it and the two spellings gc
// gives it.
//
// link is the symbol name after the "type:" prefix, which qualifies a defined
// type by its import path. name is what reflect.Type.String returns, which
// qualifies by package name instead.
var nameCorpus = []struct {
	src, link, name string
}{
	{"bool", "bool", "bool"},
	{"int", "int", "int"},
	{"int64", "int64", "int64"},
	{"uint8", "uint8", "uint8"},
	{"byte", "uint8", "uint8"},
	{"rune", "int32", "int32"},
	{"uintptr", "uintptr", "uintptr"},
	{"float64", "float64", "float64"},
	{"string", "string", "string"},
	{"unsafe.Pointer", "unsafe.Pointer", "unsafe.Pointer"},
	{"any", "interface {}", "interface {}"},
	{"[]int", "[]int", "[]int"},
	{"[]byte", "[]uint8", "[]uint8"},
	{"[]any", "[]interface {}", "[]interface {}"},
	{"[][]string", "[][]string", "[][]string"},
	{"*int", "*int", "*int"},
	{"**int", "**int", "**int"},
	{"[3]int", "[3]int", "[3]int"},
	{"[0]byte", "[0]uint8", "[0]uint8"},
	{"[2][3]*int", "[2][3]*int", "[2][3]*int"},
	{"map[string]int", "map[string]int", "map[string]int"},
	{"map[int][]any", "map[int][]interface {}", "map[int][]interface {}"},
	// A defined type is qualified by its import path in the link string and by
	// its package name in the name string. The checker is given "p" as the
	// path here, so the two coincide; the shortening is checked separately
	// below, where the path has a slash in it.
	{"T", "p.T", "p.T"},
	{"*T", "*p.T", "*p.T"},
	{"[]T", "[]p.T", "[]p.T"},
	{"map[T]*T", "map[p.T]*p.T", "map[p.T]*p.T"},
	{"error", "error", "error"},
	{"[]error", "[]error", "[]error"},
}

// nameCorpusTypes type-checks the corpus and returns one IR type per row.
func nameCorpusTypes(t *testing.T) []*Type {
	t.Helper()
	var b strings.Builder
	b.WriteString("package p\n\nimport \"unsafe\"\n\nvar _ unsafe.Pointer\n\ntype T struct{ A int }\n")
	for i, c := range nameCorpus {
		fmt.Fprintf(&b, "var v%d %s\n", i, c.src)
	}
	pkg, _, _ := buildTypecheck(t, b.String())
	c := NewConverter()
	out := make([]*Type, len(nameCorpus))
	for i := range nameCorpus {
		o := pkg.Scope().Lookup(fmt.Sprintf("v%d", i))
		if o == nil {
			t.Fatalf("v%d is not declared", i)
		}
		got, err := c.Convert(o.Type())
		if err != nil {
			t.Fatalf("%s: convert: %v", nameCorpus[i].src, err)
		}
		out[i] = got
	}
	return out
}

func TestTypeLinkString(t *testing.T) {
	for i, ty := range nameCorpusTypes(t) {
		c := nameCorpus[i]
		got, err := TypeLinkString(ty)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got != c.link {
			t.Errorf("%s: link string %q, want %q", c.src, got, c.link)
		}
		sym, err := TypeSymbol(ty)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if sym != TypeSymbolPrefix+c.link {
			t.Errorf("%s: symbol %q, want %q", c.src, sym, TypeSymbolPrefix+c.link)
		}
		// Every prefix in specs/032's table holds a colon, which is what
		// killed the text-assembly seam. A name that lost it would be a name
		// the linker stops collecting.
		if !strings.Contains(sym, ":") {
			t.Errorf("%s: the symbol %q holds no colon", c.src, sym)
		}
	}
}

func TestTypeNameString(t *testing.T) {
	for i, ty := range nameCorpusTypes(t) {
		c := nameCorpus[i]
		got, err := TypeNameString(ty)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got != c.name {
			t.Errorf("%s: name string %q, want %q", c.src, got, c.name)
		}
	}
}

// TestTypeNameStringShortensThePath checks the one way the two spellings
// differ.
//
// gc writes sync/atomic.Pointer in the link string and atomic.Pointer in the
// name string, and reflect.Type.String returns the second. The import path is
// the only thing an ir.Type carries, so the package name is derived from the
// path's last element.
func TestTypeNameStringShortensThePath(t *testing.T) {
	for _, tc := range []struct{ in, link, name string }{
		{"sync/atomic.Value", "sync/atomic.Value", "atomic.Value"},
		{"bytes.Buffer", "bytes.Buffer", "bytes.Buffer"},
		{"a/b/c.D", "a/b/c.D", "c.D"},
	} {
		ty := &Type{Kind: Struct, Name: tc.in}
		if err := Layout(ty); err != nil {
			t.Fatal(err)
		}
		if got, err := TypeLinkString(ty); err != nil || got != tc.link {
			t.Errorf("%s: link string %q (%v), want %q", tc.in, got, err, tc.link)
		}
		if got, err := TypeNameString(ty); err != nil || got != tc.name {
			t.Errorf("%s: name string %q (%v), want %q", tc.in, got, err, tc.name)
		}
	}
}

// TestTypeNameRefusals is the other half: the types an ir.Type cannot name.
//
// Each of these reduces two distinct Go types to one ir.Type, so a name built
// from it would be one name for two types. specs/032 says what that costs: the
// linker merges them and the program reads one type's descriptor for the
// other's values. The reason names the field the type boundary drops, so a
// count by cause says which field to add.
func TestTypeNameRefusals(t *testing.T) {
	for _, tc := range []struct {
		what string
		typ  *Type
		want string
	}{
		{"a channel", &Type{Kind: Chan, Elem: mustLayoutNamed(Int64, "int")}, "direction"},
		{"a function", &Type{Kind: FuncKind}, "signature"},
		{"an interface with methods", &Type{Kind: Interface}, "method set"},
		{"a literal struct", &Type{Kind: Struct, Fields: []Field{{Name: "A", Type: mustLayoutNamed(Int64, "int")}}}, "field tags"},
		{"a slice of channels", &Type{Kind: Slice, Elem: &Type{Kind: Chan, Elem: mustLayoutNamed(Int64, "int")}}, "direction"},
		{"an untyped constant", mustLayoutNamed(Int64, "untyped int"), "no canonical name"},
		{"a void", &Type{Kind: Void}, "no canonical name"},
		{"a tuple", &Type{Kind: Tuple}, "no canonical name"},
	} {
		if err := Layout(tc.typ); err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		_, err := TypeLinkString(tc.typ)
		if err == nil {
			t.Errorf("%s: was named", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the reason is %q, want it to name %q", tc.what, err, tc.want)
		}
	}
	if _, err := TypeLinkString(nil); err == nil {
		t.Error("a nil type was named")
	}
}

// TestTypeNameDepthIsBounded checks that a malformed graph is reported rather
// than recursed into.
//
// A well-formed type graph stops the walk at a defined type or a pointer, so
// nothing real reaches the limit. A malformed graph is exactly what an error
// message is describing, and a printer that recurses forever turns a
// reportable bug into a stack overflow.
func TestTypeNameDepthIsBounded(t *testing.T) {
	cyclic := &Type{Kind: Slice}
	cyclic.Elem = cyclic
	cyclic.Size, cyclic.Align = 24, 8
	if _, err := TypeLinkString(cyclic); err == nil {
		t.Error("a cyclic slice was named")
	} else if !strings.Contains(err.Error(), "nests deeper") {
		t.Errorf("the reason is %q", err)
	}
}

// TestTypeNameIsAFunctionOfTheTypeAlone is specs/032's first naming property.
//
// Two conversions of one Go type by two converters produce two ir.Type values,
// and both must name one symbol. A name that depended on the pointer, on the
// order of conversion, or on which package asked would break the linker's
// deduplication.
func TestTypeNameIsAFunctionOfTheTypeAlone(t *testing.T) {
	src := "package p\n\ntype T struct{ A int }\n\nvar v []map[string]*T\n"
	pkg, _, _ := buildTypecheck(t, src)
	o := pkg.Scope().Lookup("v")
	if o == nil {
		t.Fatal("v is not declared")
	}
	var names []string
	for i := 0; i < 2; i++ {
		ty, err := NewConverter().Convert(o.Type())
		if err != nil {
			t.Fatal(err)
		}
		name, err := TypeSymbol(ty)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if names[0] != names[1] {
		t.Errorf("two converters named %q and %q", names[0], names[1])
	}
	if names[0] != "type:[]map[string]*p.T" {
		t.Errorf("the name is %q", names[0])
	}
}
