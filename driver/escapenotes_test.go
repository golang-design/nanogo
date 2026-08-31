// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"testing"

	"golang.design/x/nanogo/ir"
)

// ptrType is a pointer to an integer, which is the smallest type a note is
// written for: it has a pointer word, so escape.Params does not skip it.
func ptrType() *ir.Type {
	return &ir.Type{Kind: ir.Ptr, Size: 8, PtrBits: []byte{1}, Elem: &ir.Type{Kind: ir.Int64, Size: 8}}
}

// param returns a named parameter of pointer type.
func param(name string) *ir.Object {
	return &ir.Object{Name: name, Class: ir.ClassParam, Type: ptrType()}
}

// TestEscapeNotesKeysBySymbol pins the key the writer rebuilds.
func TestEscapeNotesKeysBySymbol(t *testing.T) {
	p := &ir.Package{Path: "example.com/p", Funcs: []*ir.Func{
		{Name: "F", Sym: "example.com/p.F", Params: []*ir.Object{param("q")}},
	}}
	notes := escapeNotes(p)
	got, ok := notes["example.com/p.F"]
	if !ok {
		t.Fatalf("no entry for example.com/p.F, only %v", keys(notes))
	}
	if len(got) != 1 || got[0] != "esc:" {
		t.Errorf("the notes for F are %q, want one proved note: the body names nothing", got)
	}
}

// TestEscapeNotesDropsASharedSymbol is the collision rule.
//
// Two functions under one key would leave whichever was written last, and a
// proved note landing on the other function is a claim gc's caller acts on and
// nanogo never made about it. Dropping the key costs an allocation instead.
func TestEscapeNotesDropsASharedSymbol(t *testing.T) {
	leaks := &ir.Object{Name: "g", Class: ir.ClassGlobal, Type: ptrType()}
	q := param("q")
	p := &ir.Package{Path: "example.com/p", Funcs: []*ir.Func{
		// The first proves its parameter: the body is empty.
		{Name: "F", Sym: "example.com/p.F", Params: []*ir.Object{param("q")}},
		// The second stores its parameter in a global.
		{Name: "F", Sym: "example.com/p.F", Params: []*ir.Object{q}, Body: []ir.Stmt{
			ir.Assign(0,
				&ir.Node{Op: ir.OGlobal, Type: ptrType(), Obj: leaks},
				&ir.Node{Op: ir.OLocal, Type: ptrType(), Obj: q}),
		}},
	}}
	if notes := escapeNotes(p); len(notes) != 0 {
		t.Errorf("a symbol two functions share produced %v, want no entry at all", notes)
	}
}

// TestEscapeNotesSkipsAnUnnamedSymbol keeps a function with no symbol out of
// the map, where it would claim the empty key.
func TestEscapeNotesSkipsAnUnnamedSymbol(t *testing.T) {
	p := &ir.Package{Path: "example.com/p", Funcs: []*ir.Func{
		{Name: "F", Params: []*ir.Object{param("q")}},
	}}
	if notes := escapeNotes(p); len(notes) != 0 {
		t.Errorf("a function with no linker symbol produced %v", notes)
	}
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
