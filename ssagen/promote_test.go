// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// promotedSource declares the shapes a promoted method reaches its declaration
// through.
//
// A method promoted from an embedded field is declared on that field's type,
// so neither T.M nor (*T).M names a function the front end compiled. The
// language decides which of the two receiver forms carries it: through an
// embedded pointer both forms of the declaration promote, through an embedded
// value only the value form does, and the pointer form of the declaration is
// then in the method set of the pointer alone.
//
// aliased is the row that says an unnamed struct does not end the walk. An
// alias to one can be embedded, the struct declares nothing itself, and the
// methods of what it embeds are promoted through it.
//
// A scalar sits in front of the embedded field in held, outer and the alias,
// so the path steps through a nonzero offset and two of them compose. Every
// embedded field at offset zero would let a wrapper that dropped the field
// offset altogether pass.
const promotedSource = `package main

type inner struct{ n int }

func (i *inner) ptr() int { return i.n }

func (i inner) val() int { return i.n * 2 }

type pointed struct{ *inner }

type held struct {
	pad int
	inner
}

type middle struct{ held }

type outer struct {
	k int
	middle
}

func (o outer) own() int { return o.k }

type shadow struct{ inner }

func (s shadow) val() int { return 99 }

type alias = struct {
	pad int
	inner
}

type aliased struct{ alias }
`

// promotedTypes is the set of types the tests below ask for wrappers for.
func promotedTypes(t *testing.T, c *checked, names ...string) []*ir.Type {
	t.Helper()
	var out []*ir.Type
	for _, name := range names {
		out = append(out, c.namedType(t, name))
	}
	return out
}

// TestMethodWrappersGeneratesThePromotedEntryPoints pins which functions a
// promoted method owes.
//
// The itab's Fun entries and the descriptor's Method array name T.M and
// (*T).M, by specs/032-type-descriptors-and-itabs.md, and for a promoted
// method neither is a declaration, so a missing one is a relocation cmd/link
// cannot resolve. The list below is that answer for every shape at once, and
// the shadowed type is the row that says a declaration of the same name still
// owes the deref wrapper and no forward.
func TestMethodWrappersGeneratesThePromotedEntryPoints(t *testing.T) {
	c := check(t, promotedSource)
	types := promotedTypes(t, c, "inner", "pointed", "held", "middle", "outer", "shadow", "aliased")
	var got []string
	for _, fn := range c.wrappers(t, types...) {
		got = append(got, fn.Sym)
	}
	want := []string{
		// A value receiver declared on inner owes the pointer form, which is
		// the wrapper that existed before a promoted one was generated at all.
		"main.(*inner).val",
		// Through an embedded pointer, both receiver forms of both methods are
		// in pointed's own value method set.
		"main.pointed.ptr", "main.(*pointed).ptr",
		"main.pointed.val", "main.(*pointed).val",
		// Through an embedded value, the pointer receiver method is in the
		// method set of the pointer alone.
		"main.(*held).ptr",
		"main.held.val", "main.(*held).val",
		"main.(*middle).ptr",
		"main.middle.val", "main.(*middle).val",
		"main.(*outer).ptr",
		"main.outer.val", "main.(*outer).val",
		// own is declared on outer, so it owes the deref wrapper and nothing
		// promoted.
		"main.(*outer).own",
		"main.(*shadow).ptr",
		// val is declared on shadow and shadows the promoted one, so shadow
		// owes the deref wrapper and not a forward to inner.
		"main.(*shadow).val",
		// Through an alias to an unnamed struct, which declares nothing, so
		// the path is two field selections and the second one reaches inner.
		"main.(*aliased).ptr",
		"main.aliased.val", "main.(*aliased).val",
	}
	if strings.Join(sorted(got), "\n") != strings.Join(sorted(want), "\n") {
		t.Errorf("the wrappers are\n%s\nwant\n%s",
			strings.Join(sorted(got), "\n"), strings.Join(sorted(want), "\n"))
	}
}

// sorted is a copy of ss in order, so that two lists compare as sets.
func sorted(ss []string) []string {
	out := append([]string{}, ss...)
	sort.Strings(out)
	return out
}

// TestPromotedWrapperCallsTheDeclaration checks the symbol each promoted
// wrapper forwards to.
//
// The wrapper exists to reach the declaration, and the declaration is on the
// embedded type. A wrapper that called its own type's symbol would be infinite
// recursion, and one that named a receiver form the declaration does not have
// is a call whose first argument is the wrong width.
func TestPromotedWrapperCallsTheDeclaration(t *testing.T) {
	c := check(t, promotedSource)
	want := map[string]string{
		"main.pointed.ptr":    "main.(*inner).ptr",
		"main.(*pointed).ptr": "main.(*inner).ptr",
		"main.pointed.val":    "main.inner.val",
		"main.(*held).ptr":    "main.(*inner).ptr",
		"main.held.val":       "main.inner.val",
		"main.outer.val":      "main.inner.val",
		"main.(*outer).ptr":   "main.(*inner).ptr",
		"main.aliased.val":    "main.inner.val",
		"main.(*aliased).ptr": "main.(*inner).ptr",
	}
	seen := 0
	for _, fn := range c.wrappers(t, promotedTypes(t, c, "pointed", "held", "outer", "aliased")...) {
		w, ok := want[fn.Sym]
		if !ok {
			continue
		}
		seen++
		got := calledSymbol(t, fn)
		if got != w {
			t.Errorf("%s calls %s, want %s", fn.Sym, got, w)
			continue
		}
		// The target is a declaration the front end compiled, so the wrapper
		// reaches a body rather than a second wrapper that may not be owed.
		c.declared(t, got)
	}
	if seen != len(want) {
		t.Errorf("%d of the %d wrappers were generated", seen, len(want))
	}
}

// calledSymbol is the symbol the one call in a generated wrapper names.
func calledSymbol(t *testing.T, fn *ir.Func) string {
	t.Helper()
	for _, s := range fn.Body {
		for _, n := range []ir.Expr{s, firstArg(s)} {
			if n != nil && n.Op == ir.OCall && n.X != nil && n.X.Obj != nil {
				return n.X.Obj.Name
			}
		}
	}
	t.Fatalf("%s holds no call", fn.Sym)
	return ""
}

// firstArg is the first argument of n, which is where a return statement holds
// the call whose results it passes on.
func firstArg(n ir.Expr) ir.Expr {
	if n == nil || len(n.Args) == 0 {
		return nil
	}
	return n.Args[0]
}

// TestMethodWrappersRefusesAnEmbeddedInterface checks that a method promoted
// from an embedded interface is refused by name.
//
// An interface's method is declared by nobody, so there is no symbol for the
// wrapper to call and a generated one would relocate against a name nothing
// defines. ir.Build refuses the sibling case, a method expression on an
// interface, in the same words. A refusal names the type and the method; the
// link failure it replaces names neither.
func TestMethodWrappersRefusesAnEmbeddedInterface(t *testing.T) {
	c := check(t, `package main

type reader interface{ read() int }

type wrapped struct{ reader }
`)
	_, err := MethodWrappers([]*ir.Type{c.namedType(t, "wrapped")}, "main", c.declaredSyms())
	if err == nil {
		t.Fatal("a method promoted from an embedded interface produced a wrapper, which would call a symbol nothing defines")
	}
	if !strings.Contains(err.Error(), "embedded interface") || !strings.Contains(err.Error(), "read") {
		t.Errorf("the refusal is %q, and it has to name the method and the embedded interface", err)
	}
}

// TestMethodWrappersReadsTheDeclaredSet checks that the symbols the package
// declares are what tell a declared method from a promoted one.
//
// ir.Type.Methods is one method set and carries no such flag, so without the
// declared set a type that declares a method and embeds one of the same name
// reads as promoting it. The wrapper would then forward past the declaration
// to the embedded type, which is the wrong function under the right name.
func TestMethodWrappersReadsTheDeclaredSet(t *testing.T) {
	c := check(t, promotedSource)
	blind, err := MethodWrappers([]*ir.Type{c.namedType(t, "shadow")}, "main", nil)
	if err != nil {
		t.Fatalf("MethodWrappers: %v", err)
	}
	for _, fn := range blind {
		if fn.Sym != "main.(*shadow).val" {
			continue
		}
		if got := calledSymbol(t, fn); got != "main.inner.val" {
			t.Fatalf("with no declared set the wrapper already calls %s, so this test no longer measures what the set decides", got)
		}
	}
	for _, fn := range c.wrappers(t, c.namedType(t, "shadow")) {
		if fn.Sym != "main.(*shadow).val" {
			continue
		}
		if got := calledSymbol(t, fn); got != "main.shadow.val" {
			t.Errorf("the wrapper calls %s, want the declaration main.shadow.val that shadows the promoted method", got)
		}
		return
	}
	t.Fatal("no wrapper for main.(*shadow).val was generated")
}

// TestPromotedWrapperRuns links a promoted wrapper with the declaration gc
// compiled and runs the program.
//
// This is the gate the promotion half of wrapper.go exists for. The symbol
// list above says a function was generated; only a program says the path
// reaches the right field, that a step through an embedded pointer loads it,
// that a pointer receiver reached through an embedded value gets the address
// of the field rather than of a copy, and that a nil embedded pointer faults
// where gc's wrapper faults.
func TestPromotedWrapperRuns(t *testing.T) {
	hostRunsNanogoOutput(t)
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)

	tests := []struct {
		name string
		sym  string
		decl string
		call string
		want int
	}{
		// An embedded pointer and a pointer receiver: the wrapper loads the
		// embedded pointer and passes it on with no address arithmetic.
		{"embedded pointer", "main.pointed.ptr",
			"func (p pointed) ptr() int", "pointed{inner: &inner{n: 7}}.ptr()", 7},
		// An embedded pointer and a value receiver: the load is followed by a
		// dereference, because the declaration takes the value.
		{"embedded pointer to a value receiver", "main.pointed.val",
			"func (p pointed) val() int", "pointed{inner: &inner{n: 5}}.val()", 10},
		// An embedded value and a pointer receiver: the wrapper takes the
		// address of the field, which is a nonzero offset from its own
		// receiver.
		{"embedded value", "main.(*held).ptr",
			"func (h *held) ptr() int", "(&held{inner: inner{n: 9}}).ptr()", 9},
		// Two levels of embedding, so the path is two field selections and not
		// one, and neither is at offset zero.
		{"two levels", "main.outer.val",
			"func (o outer) val() int", "outer{middle: middle{held: held{inner: inner{n: 4}}}}.val()", 8},
		// An alias to an unnamed struct in the middle of the path, which
		// declares nothing and is walked through rather than stopped at.
		{"through an unnamed struct", "main.aliased.val",
			"func (a aliased) val() int", "aliased{alias: alias{inner: inner{n: 6}}}.val()", 12},
		// A nil embedded pointer. The wrapper passes the nil on and the
		// declaration faults reading through it, which is where gc's wrapper
		// faults too.
		{"nil embedded pointer", "main.pointed.ptr",
			"func (p pointed) ptr() int", "caught(func() int { return pointed{}.ptr() })", -1},
		// The same nil, reaching a value receiver, which faults on the
		// dereference in the wrapper's own body rather than in the callee.
		{"nil embedded pointer to a value receiver", "main.pointed.val",
			"func (p pointed) val() int", "caught(func() int { return pointed{}.val() })", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := check(t, promotedSource)
			p := newMainPackage()
			var wrapper *ir.Func
			for _, fn := range c.wrappers(t, promotedTypes(t, c, "pointed", "held", "outer", "aliased")...) {
				if fn.Sym == tt.sym {
					wrapper = fn
				}
			}
			if wrapper == nil {
				t.Fatalf("%s is not among the generated wrappers", tt.sym)
			}
			// Only the wrapper goes into nanogo's object. The declaration it
			// forwards to is compiled by gc, in the caller, so the call is a
			// real cross-toolchain one and the receiver has to arrive where
			// specs/030-abi.md puts it.
			addFull(t, emitFunc(t, c.build(t, wrapper), p), p)
			defs := strings.TrimPrefix(promotedSource, "package main\n") + callerHelpers
			caller := exitWrapper(t, goCmd, tt.sym, tt.call, defs, tt.decl)
			got := strings.TrimSpace(runLinked(t, goCmd, tc, cfg, p, caller))
			if want := strconv.Itoa(tt.want); got != want {
				t.Fatalf("the program printed %q, and the wrapper returns %s", got, want)
			}
			t.Logf("linked and ran the generated wrapper %s, which returned %s", tt.sym, got)
		})
	}
}

// callerHelpers is the recover the nil case reads its answer through.
//
// A process that dies of the fault prints a traceback, and a traceback counts
// frames: gc tail-calls out of its wrapper and nanogo leaves a frame behind,
// so the two differ in a way that says nothing about the fault. That the fault
// happened at all is what recover reports, and it is the same on both sides.
const callerHelpers = `
func caught(f func() int) (r int) {
	defer func() {
		if recover() != nil {
			r = -1
		}
	}()
	return f()
}
`
