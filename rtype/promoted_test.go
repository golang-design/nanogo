// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype_test

import (
	"sort"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/ssagen"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// promotedSource declares a type whose whole method set is promoted from an
// embedded field, in each of the two forms of embedding.
//
// Neither Ptr nor Val is declared on Pointed or on Held, so every function
// their descriptors and their itabs name is one a generator has to write.
const promotedSource = `package p

type Inner struct{ N int }

func (i *Inner) Ptr() int { return i.N }

func (i Inner) Val() int { return i.N * 2 }

type Pointed struct{ *Inner }

type Held struct{ Inner }

// Mixed's method set holds one method it declares and two it promotes, so the
// itab of Mixed and Three names a declaration and two generated wrappers.
type Mixed struct{ *Inner }

func (m Mixed) Own() int { return 1 }

type Both interface {
	Ptr() int
	Val() int
}

type Three interface {
	Own() int
	Ptr() int
	Val() int
}

type Value interface{ Val() int }
`

// promotedIR type-checks promotedSource and converts the named types.
func promotedIR(t *testing.T) map[string]*ir.Type {
	t.Helper()
	fset := syntax.NewFileSet()
	file, err := syntax.Parse(fset.AddFile("p.go", len(promotedSource)), []byte(promotedSource), nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types2.Config{Fset: fset, Sizes: types2.SizesFor("gc", "arm64")}
	pkg, err := conf.Check("p", []*syntax.File{file}, nil)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	c := ir.NewConverter()
	out := map[string]*ir.Type{}
	for _, name := range []string{"Inner", "Pointed", "Held", "Mixed", "Both", "Three", "Value"} {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("%s is not declared", name)
		}
		got, err := c.Convert(obj.Type())
		if err != nil {
			t.Fatalf("%s: convert: %v", name, err)
		}
		out[name] = got
		if name == "Inner" || name == "Pointed" || name == "Held" || name == "Mixed" {
			ptr, err := c.Convert(types2.NewPointer(obj.Type()))
			if err != nil {
				t.Fatalf("*%s: convert: %v", name, err)
			}
			out["*"+name] = ptr
		}
	}
	return out
}

// TestPromotedItabAndDescriptorNameAGeneratedFunction closes the loop between
// the two halves of specs/032-type-descriptors-and-itabs.md.
//
// An itab's Fun entry and a descriptor's Method row name a function by symbol
// and cmd/link resolves them by name, so every one of those names has to be
// defined by the same object. For a method the type declares that is the
// compiled declaration or the deref wrapper; for a promoted method no
// declaration carries the name at all, and a set of wrappers that missed one
// is the relocation cmd/link reports and the compiler does not.
//
// This is the invariant, checked as one: every function name the descriptors
// and the itabs of these types write is either a declaration of this package
// or a function ssagen.MethodWrappers generates.
func TestPromotedItabAndDescriptorNameAGeneratedFunction(t *testing.T) {
	types := promotedIR(t)
	// The symbols the front end would compile for this package, which is one
	// per declaration and nothing else.
	declared := map[string]bool{"p.(*Inner).Ptr": true, "p.Inner.Val": true, "p.Mixed.Own": true}

	var want []*ir.Type
	for _, name := range []string{"Inner", "*Inner", "Pointed", "*Pointed", "Held", "*Held", "Mixed", "*Mixed"} {
		want = append(want, types[name])
	}
	fns, err := ssagen.MethodWrappers(want, "p", declared)
	if err != nil {
		t.Fatalf("MethodWrappers: %v", err)
	}
	defined := map[string]bool{}
	for k := range declared {
		defined[k] = true
	}
	var generated []string
	for _, fn := range fns {
		defined[fn.Sym] = true
		generated = append(generated, fn.Sym)
	}

	named := map[string]string{}
	for _, tp := range want {
		syms, err := rtype.Descriptor(tp)
		if err != nil {
			t.Fatalf("Descriptor %s: %v", tp, err)
		}
		collectGoFuncs(syms, tp.String()+" descriptor", named)
	}
	for _, pair := range [][2]string{
		{"Pointed", "Both"}, {"*Pointed", "Both"}, {"Held", "Value"}, {"*Held", "Both"},
		// One declaration and two promotions in one itab, which is the row
		// that says the two kinds of Fun entry are written from one method set
		// and each names the function that kind owes.
		{"Mixed", "Three"}, {"*Mixed", "Three"},
	} {
		syms, err := rtype.Itab(types[pair[0]], types[pair[1]])
		if err != nil {
			t.Fatalf("Itab(%s, %s): %v", pair[0], pair[1], err)
		}
		collectGoFuncs(syms, "the itab of "+pair[0]+" and "+pair[1], named)
	}

	if len(named) == 0 {
		t.Fatal("no descriptor or itab named a function, so this test measures nothing")
	}
	for _, sym := range sortedKeys(named) {
		if !defined[sym] {
			t.Errorf("%s names %s, which no declaration and no generated wrapper defines; "+
				"the wrappers are %s", named[sym], sym, strings.Join(sortedKeys(defined), " "))
		}
	}
	// And the generated set is the promotion of both methods into both
	// receiver forms of each type, plus the deref wrapper each declared value
	// receiver method owes.
	got := strings.Join(sortedKeys(setOf(generated)), " ")
	wantSyms := strings.Join(sortedKeys(setOf([]string{
		"p.(*Inner).Val",
		"p.Pointed.Ptr", "p.(*Pointed).Ptr",
		"p.Pointed.Val", "p.(*Pointed).Val",
		"p.(*Held).Ptr",
		"p.Held.Val", "p.(*Held).Val",
		"p.Mixed.Ptr", "p.(*Mixed).Ptr",
		"p.Mixed.Val", "p.(*Mixed).Val",
		"p.(*Mixed).Own",
	})), " ")
	if got != wantSyms {
		t.Errorf("the generated wrappers are\n%s\nwant\n%s", got, wantSyms)
	}
}

// collectGoFuncs records every function symbol a group of symbols relocates
// against, with what named it.
func collectGoFuncs(syms []rtype.Symbol, what string, out map[string]string) {
	for _, s := range syms {
		for _, r := range s.Relocs {
			if r.GoFunc {
				out[r.Target] = what
			}
		}
	}
}

// setOf is the set of ss, and sortedKeys is its members in order.
func setOf(ss []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range ss {
		out[s] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
