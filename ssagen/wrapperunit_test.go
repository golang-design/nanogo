// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// The promoted-wrapper walk decides which function a descriptor's entry calls,
// and it decides it from the method set alone, because ir.Type.Methods is one
// flattened set and carries no path. These tests drive the decision functions
// directly.
//
// They are here rather than only behind a compile because the interesting
// answers are the refusals, and a refusal is reached from a type shape that
// legal Go can express but that a fixture built through the checker cannot be
// made to produce on demand: a tie at equal depth, a promotion deeper than the
// bound, a method set that claims a method no embedded field carries.

// unitInt is a scalar, so an embedded field in these fixtures is not at
// offset zero and a wrapper that dropped the offset would be visible.
var unitInt = &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}

// unitMethod is the method every fixture below promotes.
func unitMethod() ir.Method { return ir.Method{Name: "M", Pkg: "p"} }

// named builds a defined type carrying the given method set.
func named(name string, methods ...ir.Method) *ir.Type {
	return &ir.Type{Kind: ir.Struct, Name: "p." + name, PkgPath: "p", Methods: methods}
}

// embeds builds a defined struct whose fields are the given types, every one
// embedded, each preceded by a scalar so that no field sits at offset zero.
func embeds(name string, methods []ir.Method, fields ...*ir.Type) *ir.Type {
	t := &ir.Type{Kind: ir.Struct, Name: "p." + name, PkgPath: "p", Methods: methods}
	off := int64(0)
	for i, f := range fields {
		t.Fields = append(t.Fields, ir.Field{Name: "pad", Type: unitInt, Offset: off})
		off += unitInt.Size
		t.Fields = append(t.Fields, ir.Field{
			Name: "F", Type: f, Offset: off, Embedded: true,
		})
		off += 8
		_ = i
	}
	t.Size, t.Align = off, 8
	return t
}

// TestCallResultTypeIsTheShapeTheCallSiteReads covers the three arities.
//
// It is ir.Build's resultType and the tuple arm is the one a wrapper reaches,
// because a method with two results is ordinary and the wrapper forwards them
// as one value. Only the single-result arm was exercised before.
func TestCallResultTypeIsTheShapeTheCallSiteReads(t *testing.T) {
	got, err := callResultType(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ir.Void {
		t.Errorf("no results gives %s, want void", got)
	}

	if got, err = callResultType([]*ir.Type{unitInt}); err != nil {
		t.Fatal(err)
	} else if got != unitInt {
		t.Errorf("one result gives %s, want the type itself", got)
	}

	got, err = callResultType([]*ir.Type{unitInt, unitInt, unitInt})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ir.Tuple || len(got.Fields) != 3 {
		t.Fatalf("three results give %s with %d fields, want a tuple of 3", got, len(got.Fields))
	}
	// The field names are ir.Converter's, because specs/021-ssa-construction.md
	// reads a call's components out of them by name.
	for i, want := range []string{"r0", "r1", "r2"} {
		if got.Fields[i].Name != want {
			t.Errorf("field %d is named %q, want %q", i, got.Fields[i].Name, want)
		}
	}
}

// TestSigKeyIsEmptyWhenTheSignatureHasNoLinkString pins the narrowing rule.
//
// methodEntry compares signatures only when both sides have a link string, so
// a gap in the IR narrows the test rather than failing it and calling two
// different methods one method.
func TestSigKeyIsEmptyWhenTheSignatureHasNoLinkString(t *testing.T) {
	if got := sigKey(nil); got != "" {
		t.Errorf("sigKey(nil) = %q, want empty", got)
	}
	// A signature with no parameter list at all has no link string, which is
	// the shape a type built below the IR boundary by hand carries.
	if got := sigKey(&ir.Type{Kind: ir.FuncKind}); got != "" {
		t.Errorf("a signature with no lists gives %q, want empty", got)
	}
}

// TestMethodEntryComparesNamePackageAndSignature checks the three parts of a
// method's identity.
//
// A name alone is not the method. Two unexported methods of one name from two
// packages are two methods, and the same name with another signature is a
// different one again.
func TestMethodEntryComparesNamePackageAndSignature(t *testing.T) {
	m := unitMethod()
	if _, ok := methodEntry(nil, m); ok {
		t.Error("a nil type carries a method")
	}
	if _, ok := methodEntry(named("T"), m); ok {
		t.Error("an empty method set carries a method")
	}
	if _, ok := methodEntry(named("T", ir.Method{Name: "N", Pkg: "p"}), m); ok {
		t.Error("a method of another name matched")
	}
	if _, ok := methodEntry(named("T", ir.Method{Name: "M", Pkg: "q"}), m); ok {
		t.Error("a method of another package matched")
	}
	if _, ok := methodEntry(named("T", m), m); !ok {
		t.Error("the method did not match itself")
	}
}

// TestCarriesMethodPassesThroughAnUnnamedStruct is the alias case.
//
// An unnamed struct declares no method, and it is not opaque either: an alias
// to one can be embedded, and what that struct embeds promotes through it.
func TestCarriesMethodPassesThroughAnUnnamedStruct(t *testing.T) {
	m := unitMethod()
	inner := named("Inner", m)
	// struct{ Inner }, with no name of its own.
	anon := &ir.Type{Kind: ir.Struct, Fields: []ir.Field{{Name: "F", Type: inner, Embedded: true}}}

	if !carriesMethod(anon, m, maxPromotionDepth) {
		t.Error("an unnamed struct does not pass the question down to what it embeds")
	}
	if carriesMethod(nil, m, maxPromotionDepth) {
		t.Error("a nil type carries a method")
	}
	// The budget is the bound on the walk, and exhausting it is an answer of
	// no rather than a deeper search.
	if carriesMethod(anon, m, 0) {
		t.Error("an exhausted budget still carried a method")
	}
	// An unnamed non-struct embeds nothing, so it carries nothing.
	if carriesMethod(&ir.Type{Kind: ir.Slice, Elem: unitInt}, m, maxPromotionDepth) {
		t.Error("an unnamed slice carried a method")
	}
}

// TestBestSourceRefusesWhatTheLanguageDoesNotSelect covers the two refusals.
//
// Neither is a gap in this compiler. A method set that claims a method no
// embedded field carries is a malformed set, and a tie at equal depth is not
// selectable by the language either, so resolving it would be this compiler
// choosing where Go does not.
func TestBestSourceRefusesWhatTheLanguageDoesNotSelect(t *testing.T) {
	m := unitMethod()
	declared := map[string]bool{}

	// The set claims M and nothing embedded carries it.
	orphan := named("Orphan", m)
	_, _, err := bestSource(orphan, m, "p", declared, maxPromotionDepth)
	if err == nil || !strings.Contains(err.Error(), "no embedded field") {
		t.Errorf("an orphaned method gave %v, want a refusal naming the empty search", err)
	}

	// Two embedded fields declare M at the same distance. The language does
	// not select one, so neither does this.
	a, b := named("A", m), named("B", m)
	tie := embeds("Tie", []ir.Method{m}, a, b)
	_, _, err = bestSource(tie, m, "p", declared, maxPromotionDepth)
	if err == nil || !strings.Contains(err.Error(), "the same distance") {
		t.Errorf("a tie gave %v, want a refusal naming it", err)
	}

	// An exhausted budget is the depth bound, and it names the bound.
	_, _, err = bestSource(tie, m, "p", declared, 0)
	if err == nil || !strings.Contains(err.Error(), "embedded fields away") {
		t.Errorf("an exhausted budget gave %v, want a refusal naming the bound", err)
	}
}

// TestBestSourceTakesTheShallowestDeclaration is the language's rule.
//
// A method declared one field away wins over the same method two fields away,
// and the deeper one is not a tie with it.
func TestBestSourceTakesTheShallowestDeclaration(t *testing.T) {
	m := unitMethod()
	declared := map[string]bool{"p.Shallow.M": true, "p.Deep.M": true}

	shallow := named("Shallow", m)
	deep := embeds("Mid", []ir.Method{m}, named("Deep", m))
	// Field order puts the deep one first, so a walk that took the first
	// candidate rather than the shallowest would pick the wrong one.
	outer := embeds("Outer", []ir.Method{m}, deep, shallow)

	i, src, err := bestSource(outer, m, "p", declared, maxPromotionDepth)
	if err != nil {
		t.Fatalf("bestSource: %v", err)
	}
	if src != shallow {
		t.Errorf("the source is %s, want the shallowest declaration %s", src, shallow)
	}
	if outer.Fields[i].Type != shallow {
		t.Errorf("the field index %d names %s, want %s", i, outer.Fields[i].Type, shallow)
	}
}
