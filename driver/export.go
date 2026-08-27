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
		Fset: fset,
		File: func(name string) string { return TrimPath(cfg.TrimRewrites, name) },
	}
	source := export.NewBodySource(pkg, info, fset)
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
			body, err := source.BuildBody(pkg.Path()+"."+name, obj.Signature(), fd.Body)
			if err != nil {
				// The builder refuses a generic declaration and a loop over
				// a function by name. Both are declarations whose body no
				// importer can be given yet, and neither stops the package.
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
	return bodies
}

// exportableDecl reports whether a declaration's body can be offered.
//
// Two skips, and each is a property of the declaration rather than of its
// body. A declaration with no block is satisfied by assembly or by a
// linkname, and there is no body to carry. A declaration carrying any //go:
// directive is skipped because the writer carries no directive at all
// (specs/016-directives-and-pragmas.md): gc's inliner refuses //go:noinline,
// //go:norace, //go:nocheckptr, //go:cgo_unsafe_args, //go:uintptrescapes,
// //go:uintptrkeepalive and //go:yeswritebarrierrec outright, and it would
// read a pragma field of zero and inline what the source forbade.
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
