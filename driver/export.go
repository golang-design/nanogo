// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The function bodies the export data carries.
//
// specs/015-export-data.md has the two paths a body reaches an importer by.
// This is the first one: the private root's list, which carries a body an
// importer can inline. gc reads it, decodes the body into its own IR and
// substitutes it at the call, so a body that reaches a file here is code gc
// compiles into another package.
//
// Which declarations are offered is decided here and which of them the writer
// can carry is decided in export. The split follows what each side can see: a
// //go: directive is the parser's record and never reaches the tree, and what
// a body names is only visible once the body is encoded.

// exportBodies builds the body of every declaration whose body the export data
// can carry.
//
// A declaration is skipped rather than refused. The export data is complete
// without a body, so a body nanogo cannot build is an importer that cannot
// inline the call rather than a package that does not compile. The one thing
// that would be a wrong answer, a body gc's inliner must not be given, is
// refused by name inside export.
func exportBodies(cfg *Config, pkg *types2.Package, info *types2.Info, fset *syntax.FileSet, files []*syntax.File) *export.Source {
	bodies := &export.Source{
		Fset:     fset,
		File:     func(name string) string { return TrimPath(cfg.TrimRewrites, name) },
		Archives: archives(cfg.ImportCfg),
	}
	source := export.NewBodySource(pkg, info, fset)

	// A method of a generic type is held back rather than built here. Its
	// slots are numbered into the dictionary the type shares with every
	// method it declares, between the type's own slots and the slots of the
	// method declared after it, so the whole type is built in one call
	// below.
	blocks := make(map[*types2.Func]*syntax.BlockStmt)
	for _, f := range files {
		for _, d := range f.DeclList {
			fd, ok := d.(*syntax.FuncDecl)
			if !ok || !exportableDecl(fd) {
				continue
			}
			obj, _ := info.Defs[fd.Name].(*types2.Func)
			if obj == nil {
				continue
			}
			name, ok := export.SymName(obj)
			if !ok {
				continue
			}
			sig := obj.Signature()
			if sig.RecvTypeParams().Len() != 0 && sig.TypeParams().Len() == 0 {
				blocks[obj] = fd.Body
				continue
			}
			body, err := source.BuildBody(pkg.Path()+"."+name, sig, fd.Body)
			if err != nil {
				// The builder refuses a type declared inside a generic
				// declaration and a loop over a function, each by name. A
				// generic declaration whose body is refused is refused by
				// the writer too, because a generic declaration cannot
				// reach a file without its body.
				continue
			}
			bodies.Funcs = append(bodies.Funcs, export.InlineFunc{
				Obj:  obj,
				Name: name,
				Cost: export.MaxInlineCost,
				Body: body,
			})
		}
	}
	bodies.Funcs = append(bodies.Funcs, genericTypeBodies(pkg, source, blocks)...)
	return bodies
}

// archives lists the compiled packages -importcfg names.
//
// They are what the writer copies a generic declaration of another package out
// of, because such a declaration reaches an importer as a dictionary and a
// body its own package numbered and the checker records neither
// (specs/015-export-data.md). Every packagefile entry is offered and not only
// the declaring package's own: -importcfg names the direct imports, and a
// generic declaration reaches this compilation through whichever of them
// re-exported it. The writer opens a file only when it has to copy.
//
// A packageshlib entry is left out, for the reason [ImportCfg.PackageFile]
// gives: it names a shared library and not an archive.
func archives(cfg *ImportCfg) []export.Archive {
	if cfg == nil {
		return nil
	}
	out := make([]export.Archive, 0, len(cfg.PackageFiles))
	for _, e := range cfg.PackageFiles {
		out = append(out, export.Archive{Path: e.Path, File: e.File})
	}
	return out
}

// genericTypeBodies builds the methods of every generic type the package
// declares at package scope.
//
// One call per type, because the dictionary is the type's: the slots run over
// the underlying type and then over each method in declaration order, and a
// method numbered on its own would name a slot that holds another type
// (specs/013-generics.md).
//
// The order of the types is the order of the sorted scope, so the elements the
// writer allocates are fixed by the names and not by the checker's map
// (specs/053-determinism.md).
//
// A type whose methods cannot all be built is skipped and not reported. The
// writer refuses it by name when the exported surface reaches it, and it needs
// no body at all when nothing reaches it.
func genericTypeBodies(pkg *types2.Package, source *export.BodySource, blocks map[*types2.Func]*syntax.BlockStmt) []export.InlineFunc {
	var out []export.InlineFunc
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj, _ := scope.Lookup(name).(*types2.TypeName)
		if obj == nil || obj.IsAlias() {
			continue
		}
		named, _ := obj.Type().(*types2.Named)
		if named == nil || named.TypeParams().Len() == 0 || named.NumMethods() == 0 {
			continue
		}
		_, built, err := source.BuildTypeBodies(named, blocks)
		if err != nil {
			continue
		}
		funcs := make([]export.InlineFunc, 0, len(built))
		for i, body := range built {
			m := named.Method(i)
			sym, ok := export.SymName(m)
			if !ok {
				funcs = nil
				break
			}
			funcs = append(funcs, export.InlineFunc{
				Obj:  m,
				Name: sym,
				Cost: export.MaxInlineCost,
				Body: body,
			})
		}
		out = append(out, funcs...)
	}
	return out
}

// exportableDecl reports whether a declaration's body can be offered.
//
// Two skips, and each is a property of the declaration rather than of its
// body. A declaration with no block is satisfied by assembly or by a
// linkname, and there is no body to carry. A declaration carrying a //go:
// directive the driver records is skipped because the writer carries no
// directive at all (specs/016-directives-and-pragmas.md): gc's inliner
// refuses //go:noinline, //go:norace, //go:nocheckptr, //go:cgo_unsafe_args,
// //go:uintptrescapes, //go:uintptrkeepalive and //go:yeswritebarrierrec
// outright, and it would read a pragma field of zero and inline what the
// source forbade.
//
// A verb the driver does not record is not one of the seven and does not skip
// the declaration. //go:linkname is the one that reaches here: the form that
// gives a body away leaves the body in place, and the form that takes one has
// no block and is skipped above.
//
// The blank name is skipped because nothing can name it, so nothing can
// inline it.
func exportableDecl(fd *syntax.FuncDecl) bool {
	if fd.Body == nil || fd.Name.Value == "_" {
		return false
	}
	p := asPragma(fd.Pragma)
	return p == nil || p.flag == 0
}
