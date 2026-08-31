// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// The program a type switch's clause variable is captured in.
//
// Every clause shape the lowering has a row for is here, and each one is read
// from inside a function literal, which is what moves the variable into a heap
// cell:
//
//   - a clause naming one concrete type, which is the row that reaches
//     typeSwitchCase;
//   - a clause naming one interface and a clause naming the empty interface,
//     which are typeSwitchCaseGrouped's own rows and put the whole switch on
//     the grouped path;
//   - a clause listing two types, whose variable keeps the guard's type;
//   - case nil, and the default clause, which do the same.
//
// The answers are printed rather than asserted, and gc's build of the same
// source is the oracle. A clause variable the compiler binds in the wrong
// place reads as the zero of its type, so a pointer clause prints a nil
// dereference and an interface clause prints the empty string.
const typeSwitchCaptureProgram = `package main

import "fmt"

type node interface{ isNode() }

type named interface {
	node
	name() string
}

type mapType struct{ Key string }

func (*mapType) isNode()       {}
func (m *mapType) name() string { return "map:" + m.Key }

type funcType struct{ N int }

func (*funcType) isNode()        {}
func (f *funcType) name() string { return fmt.Sprintf("func:%d", f.N) }

type chanType struct{ Dir int }

func (*chanType) isNode() {}

// concrete has no interface case, so the lowering takes the ungrouped row.
func concrete(n node) func() string {
	switch e := n.(type) {
	case *funcType:
		return func() string { return fmt.Sprintf("funcType %d", e.N) }
	case *mapType:
		return func() string { return "mapType " + e.Key }
	case *chanType:
		return func() string { return fmt.Sprintf("chanType %d", e.Dir) }
	}
	return func() string { return "none" }
}

// grouped names an interface, so the whole switch goes through the runtime's
// interface switch and every clause variable is bound by the grouped row.
func grouped(n node) func() string {
	switch e := n.(type) {
	case nil:
		return func() string { return fmt.Sprintf("nil %v", e == nil) }
	case *chanType:
		return func() string { return fmt.Sprintf("chanType %d", e.Dir) }
	case named:
		return func() string { return "named " + e.name() }
	case *funcType, *mapType:
		return func() string { return fmt.Sprintf("two %v", e != nil) }
	default:
		return func() string { return fmt.Sprintf("default %v", e != nil) }
	}
}

// anyCase names the empty interface, which every dynamic type satisfies.
func anyCase(n node) func() string {
	switch e := n.(type) {
	case *chanType:
		return func() string { return fmt.Sprintf("chanType %d", e.Dir) }
	case any:
		return func() string { return fmt.Sprintf("any %v", e != nil) }
	}
	return func() string { return "none" }
}

func main() {
	for _, n := range []node{&funcType{N: 7}, &mapType{Key: "k"}, &chanType{Dir: 3}, nil} {
		fmt.Println(concrete(n)(), grouped(n)(), anyCase(n)())
	}
}
`

// TestGcAndNanogoAgreeOnACapturedTypeSwitchVariable is the program that says a
// type switch binds its clause variable where the variable lives.
//
// ir/closure.go's openCaptures runs before the lowering walk: it gives every
// variable a function literal captures a heap cell and rewrites the references
// the tree holds at that moment into loads through the cell. A type switch's
// clause variable is not one of them. The checker records one variable per
// clause and the source writes no assignment for it, so nothing binds it until
// ir/lower.go's typeSwitchCase writes the binding, which is after the rewrite.
// The binding went to the frame slot and the literal read the cell, which held
// the zero it was allocated with.
//
// This is the fault self-hosting stage 2 died of. nanogo's own
// types2.(*Checker).typInternal has
//
//	case *syntax.MapType:
//		typ := new(Map)
//		typ.key = check.varType(e.Key)
//
// and a function literal further down the clause reads e.Key, so e is
// captured. The compiler nanogo built read e as nil and took a SIGSEGV inside
// its own type checker, on five of the seven packages it reached.
func TestGcAndNanogoAgreeOnACapturedTypeSwitchVariable(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/tsw\n\ngo 1.27\n",
		"main.go": typeSwitchCaptureProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "tsw", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "tsw"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}
