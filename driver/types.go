// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// checkExportedTypes refuses a package that declares a type whose runtime
// type descriptor nanogo cannot write.
//
// The reason is a seam that only appears once a package nanogo compiled is
// imported, which specs/015-export-data.md's writer made possible. gc, told by
// the export data that a type exists, compiles an importer that refers to it
// twice over: directly, where the code needs the descriptor at run time, and
// through DWARF, where a variable of that type names go:info.<path>.<Type>.
// cmd/link resolves neither by itself. It builds the DWARF entry out of the
// descriptor (ld/dwarf.go's defgotype, which looks up "type:"+name), so both
// references come back to the one symbol the defining package owes:
// type:<path>.<Type>.
//
// gc writes that descriptor for every type a package declares, used or not
// (gc/main.go's ir.OTYPE case calls reflectdata.NeedRuntimeType). So the check
// asks rather than assumes: it converts each declared type, closes over the
// descriptors that one reaches, and asks rtype for each. What it refuses is
// exactly what rtype cannot write today, and the refusal shrinks on its own as
// rtype grows.
//
// The types it returns are the ones the object owes, and emitPackage writes
// them. Checking and emitting are one question asked twice rather than two
// questions: a check that passed where the emitter then failed would put the
// package's failure back at link time, which is what this check exists to
// stop.
//
// Refusing is worth what it costs. The alternative is what the seam does
// without it: the compile succeeds, and the build fails at link time with
//
//	sym 5: relocation target go:info.xread/lib.Point not defined
//
// which names neither the package that owes the symbol, nor the type, nor the
// spec that owns the gap.
func checkExportedTypes(cfg *Config, pkg *types2.Package, fset *syntax.FileSet) ([]*ir.Type, error) {
	if pkg == nil {
		return nil, nil
	}
	// A main package is the one package nothing can import, which is why the
	// reader came before the writer at all (specs/015-export-data.md). Its
	// types are named by its own code and by nothing else, and the
	// descriptors that code needs are emitted from the lowering pass, which
	// reports what it cannot write where it arises. So the seam this check
	// guards does not exist for a main package, and refusing one would take
	// away the case nanogo started from.
	if pkg.Name() == "main" {
		return nil, nil
	}
	conv := ir.NewConverter()
	scope := pkg.Scope()
	var owed []*ir.Type
	// Scope.Names is sorted, so a package with several such types is refused
	// by the same one on every run (specs/053-determinism.md).
	for _, name := range scope.Names() {
		obj, _ := scope.Lookup(name).(*types2.TypeName)
		if obj == nil || obj.IsAlias() {
			// An alias declares no type. Its right-hand side is owned by
			// whichever package declares it, and that package writes the
			// descriptor.
			continue
		}
		// Every declared type, not only the exported ones. An importer cannot
		// name an unexported type, but cmd/link's defgotype walks into a
		// struct's fields, so a variable of an exported type reaches the
		// unexported types inside it.
		t, err := conv.Convert(obj.Type())
		var set []*ir.Type
		if err == nil {
			// The whole closure, because cmd/link's defgotype walks from the
			// descriptor into the type of every struct field, and each edge it
			// follows is a relocation that has to resolve.
			set, err = descriptorClosure([]*ir.Type{t})
		}
		if err == nil {
			owed = append(owed, set...)
			continue
		}
		return nil, &UnsupportedError{
			Package: cfg.Package,
			What:    "a package that declares the type " + name,
			Detail: position(fset, obj.Pos(), name) + ": an importer needs the runtime type descriptor " +
				ir.TypeSymbolPrefix + pkg.Path() + "." + name + " and nanogo cannot write one (" +
				err.Error() + "); specs/032-type-descriptors-and-itabs.md owns the gap",
		}
	}
	return owed, nil
}

// descriptorSet returns the descriptors this object owes, closed over what
// each one names, and reports what it cannot write in the caller's terms.
//
// A descriptor names other descriptors, and cmd/link resolves each by name, so
// the object owes the closure and not only the roots. The descriptors the
// runtime owns are left out of it: the runtime is in every link and gc refers
// to its copies rather than emitting a second one.
//
// It is separate from the walk below so that the set can be taken before the
// descriptors are written. The generated functions of specs/030-abi.md are
// decided by the closed set, and a set taken inside the writer would be taken
// after the point where they had to be compiled.
func descriptorSet(cfg *Config, roots []*ir.Type) ([]*ir.Type, error) {
	types, err := descriptorClosure(roots)
	if err != nil {
		return nil, &UnsupportedError{
			Package: cfg.Package,
			What:    "a type its code needs a descriptor for",
			Detail:  err.Error(),
		}
	}
	return types, nil
}

// descriptorClosure returns the roots and every descriptor they reach, in
// first-use order.
//
// A root is a type this package's own code names, which is the set
// specs/032-type-descriptors-and-itabs.md makes a package responsible for, plus
// the types it declares. Every root is emitted. A type reached from a root is
// emitted too, unless the runtime already owns its descriptor: cmd/link
// resolves the reference by name and the runtime is in every link, so a second
// copy of type:int would be two definitions of one symbol whose bytes could
// differ without anyone noticing. gc draws the line in the same place.
//
// Every element is a type rtype can encode: the walk asks for each one's
// descriptor as it goes, so a type in the closure that cannot be written stops
// the walk with its own reason rather than with the root's.
//
// The order is a slice and not a map, because the object's symbol table is
// written in the order symbols were added (specs/053-determinism.md).
func descriptorClosure(roots []*ir.Type) ([]*ir.Type, error) {
	var out []*ir.Type
	// seen is a lookup table and is never ranged over. The key is the
	// canonical symbol name and not the pointer, because two converters
	// produce two ir.Types for one Go type and the linker merges by name.
	seen := make(map[string]bool)
	var walk func(t *ir.Type, root bool) error
	walk = func(t *ir.Type, root bool) error {
		if !root && rtype.RuntimeOwned(t) {
			return nil
		}
		name, err := ir.TypeSymbol(t)
		if err != nil {
			return err
		}
		if seen[name] {
			return nil
		}
		seen[name] = true
		if _, err := rtype.Descriptor(t); err != nil {
			return err
		}
		out = append(out, t)
		refs, err := rtype.Referenced(t)
		if err != nil {
			return err
		}
		for _, r := range refs {
			if err := walk(r, false); err != nil {
				return err
			}
		}
		return nil
	}
	for _, r := range roots {
		if err := walk(r, true); err != nil {
			return nil, err
		}
	}
	return out, nil
}
