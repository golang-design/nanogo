// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/ssagen"
)

// counterType returns a defined struct with one value receiver method, which
// is the shape whose descriptor names a generated wrapper.
func counterType(t *testing.T) *ir.Type {
	t.Helper()
	sig := &ir.Type{Kind: ir.FuncKind, Results: []*ir.Type{mustLayout(t, &ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"})}}
	out := &ir.Type{
		Kind:    ir.Struct,
		Name:    "main.counter",
		PkgPath: "main",
		Fields:  []ir.Field{{Name: "n", Type: mustLayout(t, &ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"})}},
		Methods: []ir.Method{{Name: "get", Sig: mustLayout(t, sig)}},
	}
	return mustLayout(t, out)
}

func mustLayout(t *testing.T, x *ir.Type) *ir.Type {
	t.Helper()
	if err := ir.Layout(x); err != nil {
		t.Fatalf("layout %s: %v", x, err)
	}
	return x
}

// TestAddGeneratedCompilesTheMethodWrapper checks that the driver compiles the
// functions a descriptor names and no declaration defines.
//
// The wrapper is the one thing in an object that nothing in the source asked
// for. If the driver does not compile it, the descriptor names a symbol
// nothing defines and the build fails at link time, which is where a compiler
// must not send a user (specs/052-diagnostics.md).
func TestAddGeneratedCompilesTheMethodWrapper(t *testing.T) {
	cfg := &Config{Package: "main"}
	out := obj.NewPackage("main")
	fns, err := ssagen.MethodWrappers([]*ir.Type{counterType(t)})
	if err != nil {
		t.Fatalf("MethodWrappers: %v", err)
	}
	extra, generated, err := addGenerated(cfg, out, ssa.NewArm64Target(), nil, fns)
	if err != nil {
		t.Fatalf("addGenerated: %v", err)
	}
	if len(extra) != 0 {
		t.Errorf("the wrapper named %d descriptors, want none: it loads a receiver and calls a method", len(extra))
	}
	// The set the descriptor writer resolves the reference with. A wrapper
	// left out of it is referenced under ABI0 and the linker reports it as
	// not defined for that ABI.
	if !generated["main.(*counter).get"] {
		t.Errorf("the wrapper is not in the generated set, which is %v", generated)
	}
	ref := rtype.Reloc{Target: "main.(*counter).get"}
	if got := targetABI(ref, generated); got != obj.ABIInternal {
		t.Errorf("a descriptor references the wrapper under ABI %d, want ABIInternal", got)
	}
	if got := targetABI(ref, nil); got != obj.ABI0 {
		t.Errorf("a name that is not a generated function is ABI %d, want ABI0", got)
	}
	sym := findDef(out, "main.(*counter).get")
	if sym == nil {
		t.Fatalf("the object holds no definition of main.(*counter).get; it holds %v", defNames(out))
	}
	if sym.Type != obj.STEXT {
		t.Errorf("the wrapper is %v, want STEXT", sym.Type)
	}
	if sym.ABI != obj.ABIInternal {
		t.Errorf("the wrapper is ABI %d, want ABIInternal: a descriptor's Ifn is called the way a Go function is", sym.ABI)
	}
	if sym.Flag&obj.SymFlagDupok == 0 {
		t.Error("the wrapper is not duplicate-tolerant, so two packages that name it are two definitions of one symbol")
	}
	// And a duplicate-tolerant symbol belongs to no package. cmd/link
	// deduplicates by name in that index space only, so a wrapper written as a
	// package definition would survive beside the copy another object wrote.
	if findNonPkgDef(out, "main.(*counter).get") == nil {
		t.Error("the wrapper is a package definition, and cmd/link deduplicates a dupok symbol by name in the non-package index space only")
	}
	if len(sym.Data) == 0 {
		t.Error("the wrapper has no instructions")
	}
}

// TestAddGeneratedSkipsAPointerReceiver checks that a method the front end
// already spells with a pointer produces nothing.
//
// A wrapper generated for it would be a second definition of the method's own
// symbol, which is a duplicate the linker reports rather than merges.
func TestAddGeneratedSkipsAPointerReceiver(t *testing.T) {
	base := counterType(t)
	base.Methods[0].PtrOnly = true
	cfg := &Config{Package: "main"}
	out := obj.NewPackage("main")
	fns, err := ssagen.MethodWrappers([]*ir.Type{base})
	if err != nil {
		t.Fatalf("MethodWrappers: %v", err)
	}
	if _, _, err := addGenerated(cfg, out, ssa.NewArm64Target(), nil, fns); err != nil {
		t.Fatalf("addGenerated: %v", err)
	}
	if sym := findDef(out, "main.(*counter).get"); sym != nil {
		t.Error("a pointer receiver method produced a wrapper, which would define the method's own symbol twice")
	}
}

// TestGeneratedFuncsReportsARefusalByName checks that a method the generator
// cannot build is reported in the user's terms rather than as a panic.
func TestGeneratedFuncsReportsARefusalByName(t *testing.T) {
	base := counterType(t)
	base.Methods[0].Sig = nil
	cfg := &Config{Package: "main"}
	_, err := generatedFuncs(cfg, []*ir.Type{base})
	if err == nil {
		t.Fatal("a method with no signature produced a wrapper")
	}
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("the refusal is %T, want an *UnsupportedError a user can act on: %v", err, err)
	}
	if ue.Package != "main" {
		t.Errorf("the refusal names package %q, want main", ue.Package)
	}
}

// findDef returns a definition of the object by name.
//
// The object holds its definitions in an index space rather than by name, so
// the walk is over the indices and stops at the first one nothing defines.
// Both spaces that hold a named definition are walked: a duplicate-tolerant
// symbol belongs to no package, so a generated function is not where a
// declared one is.
func findDef(p *obj.Package, name string) *obj.Symbol {
	for _, space := range []uint32{obj.PkgIdxSelf, obj.PkgIdxNone} {
		for i := uint32(0); ; i++ {
			s := p.Def(obj.SymRef{PkgIdx: space, SymIdx: i})
			if s == nil {
				break
			}
			if s.Name == name {
				return s
			}
		}
	}
	return nil
}

// findNonPkgDef returns a non-package definition of the object by name.
func findNonPkgDef(p *obj.Package, name string) *obj.Symbol {
	for i := uint32(0); ; i++ {
		s := p.Def(obj.SymRef{PkgIdx: obj.PkgIdxNone, SymIdx: i})
		if s == nil {
			return nil
		}
		if s.Name == name {
			return s
		}
	}
}

// defNames lists the named definitions of the object, for a failure message.
func defNames(p *obj.Package) []string {
	var out []string
	for _, space := range []uint32{obj.PkgIdxSelf, obj.PkgIdxNone} {
		for i := uint32(0); ; i++ {
			s := p.Def(obj.SymRef{PkgIdx: space, SymIdx: i})
			if s == nil {
				break
			}
			if s.Name != "" {
				out = append(out, s.Name)
			}
		}
	}
	return out
}

// TestAlgFuncsReadsTheDescriptorsAnswer checks that the equality and hash
// functions are found by what the descriptor says it needs.
//
// rtype decides the type's algorithm and names the function the closure points
// at. Deciding it a second time here could disagree, and a disagreement is a
// descriptor pointing at a function nothing defines, which the linker reports
// and the compiler does not.
func TestAlgFuncsReadsTheDescriptorsAnswer(t *testing.T) {
	base := counterType(t)
	eq, err := ssagen.EqualSymbol(base)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := ssagen.HashSymbol(base)
	if err != nil {
		t.Fatal(err)
	}
	set := []rtype.Symbol{{
		Name: "type:.eqfunc.main.counter",
		Relocs: []rtype.Reloc{
			{Target: eq},
			{Target: hash},
			// Another type's function, which that type generates.
			{Target: "type:.eq.main.other"},
			// A runtime symbol, which nothing generates.
			{Target: "runtime.strequal"},
		},
	}}
	fns, err := algFuncs(base, set)
	if err != nil {
		t.Fatalf("algFuncs: %v", err)
	}
	var got []string
	for _, fn := range fns {
		got = append(got, fn.Sym)
	}
	want := []string{eq, hash}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("algFuncs returned %v, want %v", got, want)
	}
}

// TestAlgFuncsFindsNothingWithoutAReference checks that a descriptor that
// names no generated function produces none.
func TestAlgFuncsFindsNothingWithoutAReference(t *testing.T) {
	fns, err := algFuncs(counterType(t), []rtype.Symbol{{Relocs: []rtype.Reloc{{Target: "runtime.memequal64"}}}})
	if err != nil {
		t.Fatalf("algFuncs: %v", err)
	}
	if len(fns) != 0 {
		t.Errorf("algFuncs returned %d functions for a descriptor that names none", len(fns))
	}
}
