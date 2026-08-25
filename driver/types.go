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
// (gc/main.go's ir.OTYPE case calls reflectdata.NeedRuntimeType). nanogo
// writes descriptors only for the types its own code names, and rtype cannot
// encode a defined type at all: a descriptor carries an UncommonType tail
// whenever the type has methods, and an ir.Type carries no method set. That is
// the same gap that stops itabs, and specs/032-type-descriptors-and-itabs.md
// owns it.
//
// So the check asks rather than assumes: it converts each declared type and
// asks rtype for its descriptor. What it refuses is exactly what rtype cannot
// write today, and the refusal shrinks on its own as rtype grows.
//
// Refusing is worth what it costs. The alternative is what the seam does
// without it: the compile succeeds, and the build fails at link time with
//
//	sym 5: relocation target go:info.xread/lib.Point not defined
//
// which names neither the package that owes the symbol, nor the type, nor the
// spec that owns the gap.
func checkExportedTypes(cfg *Config, pkg *types2.Package, fset *syntax.FileSet) error {
	if pkg == nil {
		return nil
	}
	// A main package is the one package nothing can import, which is why the
	// reader came before the writer at all (specs/015-export-data.md). Its
	// types are named by its own code and by nothing else, and the
	// descriptors that code needs are emitted from the lowering pass, which
	// reports what it cannot write where it arises. So the seam this check
	// guards does not exist for a main package, and refusing one would take
	// away the case nanogo started from.
	if pkg.Name() == "main" {
		return nil
	}
	conv := ir.NewConverter()
	scope := pkg.Scope()
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
		if err == nil {
			_, err = rtype.Descriptor(t)
		}
		if err == nil {
			continue
		}
		return &UnsupportedError{
			Package: cfg.Package,
			What:    "a package that declares the type " + name,
			Detail: position(fset, obj.Pos(), name) + ": an importer needs the runtime type descriptor " +
				ir.TypeSymbolPrefix + pkg.Path() + "." + name + " and nanogo cannot write one (" +
				err.Error() + "); specs/032-type-descriptors-and-itabs.md owns the gap",
		}
	}
	return nil
}
