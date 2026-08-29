// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// The dictionary of a body read out of an archive.
//
// [bodydict.go] builds a [Dict] while it encodes a body, because the slot
// numbers are an allocation. This is the same structure read back: the slots
// were numbered when the declaring package was compiled, and the body names
// them by number, so a consumer of the tree cannot resolve one without the
// list the archive holds.
//
// The stenciler of specs/013-generics.md is that consumer, and two of the
// lists are what it cannot do without. TypeParams is the domain of its
// substitution: the body's types are written in terms of the type parameters
// this decode created, which are not the objects the type checker made for
// the same declaration, so a substitution built from the checker's list would
// replace nothing. Subdicts names the callee of a call whose type arguments
// depend on the enclosing declaration's, which is how slices.Contains reaches
// slices.Index: the call carries a slot number and no reference to the
// declaration at all.

// readDict returns the exported form of one dictionary, building it once.
//
// pkg is the package the declaration belongs to. One declaration's bodies
// share one dictionary and a generic type shares one with every method it
// declares, so the reader's dictionary is the identity the exported one is
// cached under and each body of a declaration gets the same value.
func (pr *pkgReader) readDict(d *readerDict, pkg *types2.Package) *Dict {
	if d == nil {
		return nil
	}
	if have, ok := pr.dicts[d]; ok {
		return have
	}
	out := &Dict{Pkg: pkg}
	// Claimed before the lists are filled, because resolving a derived type
	// can reach a declaration whose own dictionary is this one.
	pr.dicts[d] = out

	out.Receivers = append(out.Receivers, d.rtparams...)
	out.TypeParams = append(out.TypeParams, d.tparams...)

	// The type section, resolved whole. The body reader resolves the slots
	// its own nodes name, and a slot no node named is still part of the
	// numbering a consumer reads, so a list with holes in it would say a
	// type is absent where it is merely unvisited.
	out.Derived = make([]types2.Type, len(d.derived))
	for i := range out.Derived {
		out.Derived[i] = pr.typIdx(typeInfo{idx: pkgbits.Index(i), derived: true}, d)
	}

	out.MethodExprs = make([]MethodExprSlot, len(d.typeParamMethodExprs))
	for i, info := range d.typeParamMethodExprs {
		out.MethodExprs[i] = MethodExprSlot{TypeParam: info.typeParamIdx, Sel: info.method}
	}
	out.Subdicts = make([]ObjUse, len(d.subdicts))
	for i, info := range d.subdicts {
		out.Subdicts[i] = pr.objUseOf(info, d)
	}
	out.RTypes = make([]TypeUse, len(d.rtypes))
	for i, info := range d.rtypes {
		out.RTypes[i] = pr.typeUseOf(info, d)
	}
	out.Itabs = make([]ItabSlot, len(d.itabs))
	for i, info := range d.itabs {
		out.Itabs[i] = ItabSlot{Type: pr.typeUseOf(info.typ, d), Iface: pr.typeUseOf(info.iface, d)}
	}
	return out
}

// typeUseOf resolves one type reference of a dictionary.
func (pr *pkgReader) typeUseOf(info typeInfo, d *readerDict) TypeUse {
	return TypeUse{Derived: info.derived, Idx: info.idx, Type: pr.typIdx(info, d)}
}

// objUseOf resolves one declaration reference of a dictionary.
//
// The object is looked up in the scope the decode filled, so it is the object
// the type checker holds for that declaration whenever the two share a
// package table. See [Reader.Bodies], which is what makes them share one.
func (pr *pkgReader) objUseOf(info objInfo, d *readerDict) ObjUse {
	pkg, name := pr.objIdx(info.idx)
	use := ObjUse{Idx: info.idx, Pkg: pkg, Name: name}
	switch {
	case pkg != nil:
		use.Obj = pkg.Scope().Lookup(name)
	case name != "":
		use.Obj = types2.Universe.Lookup(name)
	}
	use.Targs = make([]TypeUse, len(info.explicits))
	for i, e := range info.explicits {
		use.Targs[i] = pr.typeUseOf(e, d)
	}
	return use
}
