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
	fns, err := ssagen.MethodWrappers([]*ir.Type{counterType(t)}, "main", nil)
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
	fns, err := ssagen.MethodWrappers([]*ir.Type{base}, "main", nil)
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
	_, err := generatedFuncs(cfg, []*ir.Type{base}, nil)
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
	other := twoStringsType(t)
	owners, err := algOwners([]*ir.Type{base, other})
	if err != nil {
		t.Fatalf("algOwners: %v", err)
	}
	eq, err := ssagen.EqualSymbol(base)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := ssagen.HashSymbol(base)
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := ssagen.HashSymbol(other)
	if err != nil {
		t.Fatal(err)
	}
	set := []rtype.Symbol{{
		Name: "type:.eqfunc.main.counter",
		Relocs: []rtype.Reloc{
			{Target: eq},
			{Target: hash},
			// Another type's function. A map's Hasher names one, so the name
			// is resolved against the closed set and not against the type
			// whose descriptor named it.
			{Target: otherHash},
			// A runtime symbol, which nothing generates.
			{Target: "runtime.strequal"},
		},
	}}
	fns, err := algFuncs(owners, set)
	if err != nil {
		t.Fatalf("algFuncs: %v", err)
	}
	var got []string
	for _, fn := range fns {
		got = append(got, fn.Sym)
	}
	want := []string{eq, hash, otherHash}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("algFuncs returned %v, want %v", got, want)
	}
}

// TestAlgFuncsReportsANameTheSetDoesNotOwn checks that a generated name whose
// type is not in the closed descriptor set is reported here.
//
// It would otherwise be a relocation cmd/link resolves against nothing, and
// the diagnostic would name a symbol rather than a type.
func TestAlgFuncsReportsANameTheSetDoesNotOwn(t *testing.T) {
	owners, err := algOwners([]*ir.Type{counterType(t)})
	if err != nil {
		t.Fatalf("algOwners: %v", err)
	}
	set := []rtype.Symbol{{Name: "type:.hashfunc.main.absent", Relocs: []rtype.Reloc{{Target: "type:.hash.main.absent"}}}}
	if _, err := algFuncs(owners, set); err == nil {
		t.Fatal("a generated name no type in the set owns produced no refusal")
	}
}

// TestAlgFuncsFindsNothingWithoutAReference checks that a descriptor that
// names no generated function produces none.
func TestAlgFuncsFindsNothingWithoutAReference(t *testing.T) {
	owners, err := algOwners([]*ir.Type{counterType(t)})
	if err != nil {
		t.Fatalf("algOwners: %v", err)
	}
	fns, err := algFuncs(owners, []rtype.Symbol{{Relocs: []rtype.Reloc{{Target: "runtime.memequal64"}}}})
	if err != nil {
		t.Fatalf("algFuncs: %v", err)
	}
	if len(fns) != 0 {
		t.Errorf("algFuncs returned %d functions for a descriptor that names none", len(fns))
	}
}

// twoStringsType returns a defined struct of two strings, which is a type that
// compares as something other than one region of memory: it needs a generated
// equality function and a generated hash function.
func twoStringsType(t *testing.T) *ir.Type {
	t.Helper()
	str := mustLayout(t, &ir.Type{Kind: ir.String, Name: "string", Basic: "string"})
	return mustLayout(t, &ir.Type{
		Kind:    ir.Struct,
		Name:    "main.producer",
		PkgPath: "main",
		Fields:  []ir.Field{{Name: "Tool", Type: str}, {Name: "Version", Type: str}},
		Methods: []ir.Method{},
	})
}

// TestGeneratedFuncsWritesTheKeysHashForAMap is the map half of the rule that
// whoever writes a descriptor defines the functions it names.
//
// A map's Hasher points at the hash of the *key* type, and no descriptor names
// a key's hash anywhere else: a struct descriptor has an Equal field and no
// Hasher. So a scan that resolved a name against the type whose descriptor
// named it found nothing here, the function was never generated, and the map
// descriptor would carry a relocation against an undefined symbol. rtype
// refused the map rather than emit that, which is the refusal this closes.
func TestGeneratedFuncsWritesTheKeysHashForAMap(t *testing.T) {
	key := twoStringsType(t)
	elem := mustLayout(t, &ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"})
	m := mustLayout(t, &ir.Type{Kind: ir.Map, Key: key, Elem: elem})
	cfg := &Config{Package: "main"}
	types, err := descriptorSet(cfg, []*ir.Type{m})
	if err != nil {
		t.Fatalf("descriptorSet: %v", err)
	}
	fns, err := generatedFuncs(cfg, types, nil)
	if err != nil {
		t.Fatalf("generatedFuncs: %v", err)
	}
	want, err := ssagen.HashSymbol(key)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, fn := range fns {
		got = append(got, fn.Sym)
	}
	found := false
	for _, s := range got {
		if s == want {
			found = true
		}
	}
	if !found {
		t.Errorf("the map's key hash %s is not generated; the driver generated %v", want, got)
	}
}
