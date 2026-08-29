// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// Finding the bodies a package's export data carries.
//
// A body element does not say which declaration it belongs to and does not
// say how many parameters that declaration has, so it cannot be found or
// decoded on its own. It is reached through whatever names it, and
// specs/015-export-data.md has the two things that do: the private root's
// list, which carries an inlinable body, and a function's extension data,
// which carries a generic one.

// A FuncBody is one function body with the declaration it belongs to.
type FuncBody struct {
	// Path and Name are the declaration, as the export data names it. Name
	// is gc's linker symbol name, so a method is "T.M" or "(*T).M".
	Path string
	Name string

	// Generic is true for a body reached through the declaration's
	// extension data, which is the shape a generic declaration has and the
	// one an importer needs in order to instantiate it
	// (specs/013-generics.md). It is false for a body reached through the
	// private root, which is an inlinable body.
	Generic bool

	// Pragma is the //go: directives the declaration carries, as gc's own
	// bit set. It is not translated, because the two compilers do not agree
	// on the numbering and nothing here reads one: a consumer that acts on a
	// directive has to translate it, and one that cannot has to refuse a
	// declaration that carries any (specs/016-directives-and-pragmas.md).
	Pragma int

	// Idx is the SectionBody element, and Body is it decoded.
	Idx  pkgbits.Index
	Body *Body

	// Nested is the number of function literal bodies inside this one,
	// each of which is an element of its own that was decoded with it.
	Nested int
}

// ReadBodies reads a package's declarations and then every function body its
// export data carries.
//
// The package is returned whether or not a body could be read, because the
// declarations are what an importer needs first and a body that cannot be
// read is a refusal about one declaration rather than about the package. A
// refusal is a [*BodyError] naming the declaration.
func ReadBodies(ctxt *types2.Context, imports map[string]*types2.Package, input pkgbits.PkgDecoder) (*types2.Package, []*FuncBody, error) {
	pkg, _, bodies, err := readBodies(ctxt, imports, input)
	return pkg, bodies, err
}

// readBodies is [ReadBodies] with the decoder it built kept.
//
// The encoder needs the same one: a body it writes back names the elements
// the archive already holds, and the reverse maps [newElemRefs] builds are of
// that archive's sections.
func readBodies(ctxt *types2.Context, imports map[string]*types2.Package, input pkgbits.PkgDecoder) (pkg *types2.Package, pr *pkgReader, bodies []*FuncBody, err error) {
	pkg, pr = readPackage(ctxt, imports, input)

	defer func() {
		if v := recover(); v != nil {
			if b, ok := v.(*BodyError); ok {
				err = b
				return
			}
			err = fmt.Errorf("export: %s: %v", input.PkgPath(), v)
		}
	}()

	owners := pr.declarations(pkg)
	for _, o := range owners {
		if o.body < 0 {
			continue
		}
		before := pr.nested
		body := pr.decodeBody(o.body, o.dict, o.params, o.path+"."+o.name)
		bodies = append(bodies, &FuncBody{
			Path:    o.path,
			Name:    o.name,
			Generic: true,
			Pragma:  o.pragma,
			Idx:     o.body,
			Body:    body,
			Nested:  pr.nested - before,
		})
	}
	bodies = append(bodies, pr.inlineBodies(owners)...)
	return pkg, pr, bodies, nil
}

// owner is one declaration that can name a body.
type owner struct {
	path   string
	name   string        // gc's linker symbol name
	params int           // receiver, parameters and results
	notes  int           // receiver and parameters
	dict   *readerDict   // the dictionary the body's derived types resolve in
	body   pkgbits.Index // the body named by the extension data, or -1
	pragma int           // the //go: directives, as gc's bit set
}

// declarations walks every object in the file and returns the ones that can
// name a body.
//
// Every object, not only this package's: the file is the linked form, so a
// declaration of another package that the exported surface reaches is written
// out in full and can carry a body of its own.
func (pr *pkgReader) declarations(pkg *types2.Package) []owner {
	var out []owner
	for i := range pr.NumElems(pkgbits.SectionObj) {
		idx := pkgbits.Index(i)
		_, _, tag := pr.PeekObj(idx)
		if tag != pkgbits.ObjFunc && tag != pkgbits.ObjType {
			continue
		}
		objPkg, objName := pr.objIdx(idx)
		if objPkg == nil {
			continue
		}
		obj := objPkg.Scope().Lookup(objName)
		if obj == nil {
			continue
		}

		switch tag {
		case pkgbits.ObjFunc:
			fn, ok := obj.(*types2.Func)
			if !ok {
				continue
			}
			o := owner{path: objPkg.Path(), name: objName, body: -1}
			o.params, o.notes = sigLocals(fn.Signature())
			o.dict = pr.bodyDict(idx, tag, objPkg)
			o.body, o.pragma = pr.funcExtBody(idx, o.notes, o.path+"."+o.name)
			out = append(out, o)

		case pkgbits.ObjType:
			tn, ok := obj.(*types2.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types2.Named)
			if !ok {
				continue
			}
			out = append(out, pr.methodExtBodies(idx, objPkg, named)...)
		}
	}
	return out
}

// sigLocals returns the two counts a signature is read with.
//
// locals is the number of locals the body declares before its first
// statement, which is the receiver plus the parameters plus the results.
// notes is the number of escape analysis notes the extension data holds,
// which is the receiver plus the parameters and no result.
func sigLocals(sig *types2.Signature) (locals, notes int) {
	notes = sig.Params().Len()
	if sig.Recv() != nil {
		notes++
	}
	return notes + sig.Results().Len(), notes
}

// methodSym returns the name gc's linker gives a method, which is the name
// the private root's body list uses.
func methodSym(named *types2.Named, m *types2.Func) string {
	name := named.Obj().Name()
	if sig := m.Signature(); sig.Recv() != nil {
		if _, isPtr := sig.Recv().Type().(*types2.Pointer); isPtr {
			return "(*" + name + ")." + m.Name()
		}
	}
	return name + "." + m.Name()
}

// funcExtBody returns the body a function's extension data names, or -1, with
// the declaration's pragma flags.
func (pr *pkgReader) funcExtBody(idx pkgbits.Index, notes int, name string) (pkgbits.Index, int) {
	r := &bodyReader{
		reader: pr.newReader(pkgbits.SectionObjExt, idx, pkgbits.SyncObject1),
		name:   name,
		relocs: make(map[pkgbits.RefTableEntry]bool),
	}
	body, pragma := r.funcExt(notes)
	if n := r.Data.Len(); n != 0 {
		r.refuse("%d bytes of the extension data were not decoded", n)
	}
	return body, pragma
}

// methodExtBodies returns the bodies the methods of a defined type name.
//
// The extension data of a defined type holds the type's own three fields and
// then one function's worth for each method it declares, in declaration
// order, which is the order the type's element wrote them in. So the methods
// are paired with the element positionally and no name is matched.
func (pr *pkgReader) methodExtBodies(idx pkgbits.Index, objPkg *types2.Package, named *types2.Named) []owner {
	name := objPkg.Path() + "." + named.Obj().Name()
	r := &bodyReader{
		reader: pr.newReader(pkgbits.SectionObjExt, idx, pkgbits.SyncObject1),
		name:   name,
		relocs: make(map[pkgbits.RefTableEntry]bool),
	}
	dict := pr.bodyDict(idx, pkgbits.ObjType, objPkg)

	// The three fields. The two descriptor symbol indices are always
	// present: gc's reader has no branch here, so the shape gc's linker
	// re-encodes is the only shape that reaches a file for a defined type,
	// whether or not the type is generic. A function is the opposite and
	// carries either shape, which is why [bodyReader.funcExt] branches.
	r.Sync(pkgbits.SyncTypeExt)
	r.pragmaFlag()
	r.Int64()
	r.Int64()

	var out []owner
	for i := 0; r.Data.Len() > 0; i++ {
		if i >= named.NumMethods() {
			r.refuse("the extension data holds more methods than the declaration does")
		}
		m := named.Method(i)
		locals, notes := sigLocals(m.Signature())
		o := owner{
			path:   objPkg.Path(),
			name:   methodSym(named, m),
			params: locals,
			notes:  notes,
			dict:   dict,
		}
		o.body, o.pragma = r.funcExt(notes)
		out = append(out, o)
	}
	return out
}

// funcExt decodes a function's extension data and returns the body it names,
// or -1 when it names none, with the declaration's pragma flags.
//
// Two shapes reach a file. gc's linker writes the relocated one, which holds
// the ABI, the escape analysis notes and the inlining cost, for an object it
// has a definition for. It copies the writer's own shape verbatim for one it
// does not, and a generic declaration is exactly that case: the shape is a
// reference to a body element. The leading bool tells the two apart.
//
// notes is the receiver plus the parameters, because the relocated shape
// writes one escape analysis note for each of them and no note for a result.
// A reader that miscounted them would desync.
func (r *bodyReader) funcExt(notes int) (pkgbits.Index, int) {
	r.Sync(pkgbits.SyncFuncExt)
	pragma := r.pragmaFlag()
	r.linkname()

	// The two WebAssembly fields are not read. They are written only where
	// the compiling toolchain targeted wasm, and nanogo reads the archives
	// of its own target (specs/030-abi.md).

	if !r.Bool() {
		return r.reloc(pkgbits.SectionBody), pragma
	}
	r.Uint64() // the ABI the definition is at
	for range notes {
		r.str() // one escape analysis note per receiver and parameter
	}
	if r.Bool() {
		r.Len()  // the inlining cost
		r.Bool() // whether the results can be delayed
	}
	return -1, pragma
}

func (r *bodyReader) pragmaFlag() int {
	r.Sync(pkgbits.SyncPragma)
	return r.Int()
}

func (r *bodyReader) linkname() {
	r.Sync(pkgbits.SyncLinkname)
	if r.Int64() >= 0 {
		return
	}
	r.str()  // the linker symbol name
	r.Bool() // whether it names a standard library symbol
}

// inlineBodies decodes the bodies the private root lists.
//
// The entry names its declaration by gc's linker symbol name, and the
// declaration is what says how many locals the body opens with, so the walk
// above is what makes the list decodable.
func (pr *pkgReader) inlineBodies(owners []owner) []*FuncBody {
	byName := make(map[string]owner, len(owners))
	for _, o := range owners {
		byName[o.path+" "+o.name] = o
	}

	r := &bodyReader{
		reader: pr.newReader(pkgbits.SectionMeta, pkgbits.PrivateRootIdx, pkgbits.SyncPrivate),
		name:   pr.PkgPath(),
		relocs: make(map[pkgbits.RefTableEntry]bool),
	}
	r.Bool() // whether the object holds an initialisation record

	var out []*FuncBody
	for range r.Len() {
		path := r.str()
		name := r.str()
		idx := r.reloc(pkgbits.SectionBody)
		o, ok := byName[path+" "+name]
		if !ok {
			r.refuse("the private root lists a body for %s.%s and no declaration in the file names it", path, name)
		}
		before := pr.nested
		body := pr.decodeBody(idx, o.dict, o.params, path+"."+name)
		out = append(out, &FuncBody{
			Path:   path,
			Name:   name,
			Pragma: o.pragma,
			Idx:    idx,
			Body:   body,
			Nested: pr.nested - before,
		})
	}
	return out
}

// bodyDict reads the whole dictionary of the declaration at idx.
//
// The declaration reader stops at the derived types, because that is all a
// signature needs. A body needs the rest: it refers to a slot of the runtime
// lists wherever it needs a descriptor whose identity depends on the type
// arguments. The type parameters themselves are read from the declaration's
// own element, because a derived type is written in terms of them.
func (pr *pkgReader) bodyDict(idx pkgbits.Index, tag pkgbits.CodeObj, objPkg *types2.Package) *readerDict {
	dict := pr.objDictIdx(idx)

	r := pr.newReader(pkgbits.SectionObjDict, idx, pkgbits.SyncObject1)
	r.dict = dict

	// Skip what objDictIdx already read: the counts, the bounds and the
	// derived types. The skip states that prefix a second time, so the two
	// have to move together if the format's dictionary ever changes.
	r.Len()
	nreceivers := 0
	if r.Version().Has(pkgbits.GenericMethods) {
		nreceivers = r.Len()
	}
	nexplicits := r.Len()
	for range nreceivers + nexplicits {
		r.typInfo()
	}
	for range r.Len() {
		r.Reloc(pkgbits.SectionType)
		if r.Version().Has(pkgbits.DerivedInfoNeeded) {
			assert(!r.Bool())
		}
	}

	// One bool per type argument, saying whether its constraint is a basic
	// interface. gc uses it to decide how far it can shape the argument,
	// and nanogo stencils fully (specs/013-generics.md), so it is read and
	// dropped.
	for range nreceivers + nexplicits {
		r.Bool()
	}

	dict.typeParamMethodExprs = make([]methodExprInfo, r.Len())
	for i := range dict.typeParamMethodExprs {
		info := &dict.typeParamMethodExprs[i]
		info.typeParamIdx = r.Len()
		r.Sync(pkgbits.SyncSelector)
		info.method = Selector{Pkg: r.pkg(), Name: r.String()}
	}

	dict.subdicts = make([]objInfo, r.Len())
	for i := range dict.subdicts {
		dict.subdicts[i] = r.objInfo()
	}

	dict.rtypes = make([]typeInfo, r.Len())
	for i := range dict.rtypes {
		dict.rtypes[i] = r.typInfo()
	}

	dict.itabs = make([]itabInfo, r.Len())
	for i := range dict.itabs {
		dict.itabs[i] = itabInfo{typ: r.typInfo(), iface: r.typInfo()}
	}

	pr.bindTypeParams(idx, tag, dict)

	// The exported form, built here because this is the one place that has
	// the whole dictionary and the declaration it belongs to. A body carries
	// it, and the stenciler of specs/013-generics.md reads the slots the body
	// names out of it (bodydictread.go).
	pr.readDict(dict, objPkg)
	return dict
}

func (r *reader) objInfo() objInfo {
	r.Sync(pkgbits.SyncObject)
	if r.Version().Has(pkgbits.DerivedFuncInstance) {
		assert(!r.Bool())
	}
	info := objInfo{idx: r.Reloc(pkgbits.SectionObj)}
	info.explicits = make([]typeInfo, r.Len())
	for i := range info.explicits {
		info.explicits[i] = r.typInfo()
	}
	return info
}

// bindTypeParams fills the dictionary's type parameters from the
// declaration's own element.
//
// A derived type is written in terms of the type parameters, so the
// dictionary cannot resolve one until the parameters exist. The declaration
// reader creates them inside the lazy scope entry it inserts, which nothing
// outside that closure can reach, so they are read again here.
func (pr *pkgReader) bindTypeParams(idx pkgbits.Index, tag pkgbits.CodeObj, dict *readerDict) {
	if len(dict.tbounds) == 0 && len(dict.rtbounds) == 0 {
		return
	}
	r := pr.newReader(pkgbits.SectionObj, idx, pkgbits.SyncObject1)
	r.dict = dict
	r.pos()
	switch tag {
	case pkgbits.ObjFunc:
		if r.Version().Has(pkgbits.GenericMethods) && r.Bool() {
			r.selector()
			r.typeParamNames(false, true)
			r.param()
		}
		r.typeParamNames(false, false)
	case pkgbits.ObjType, pkgbits.ObjAlias:
		r.typeParamNames(false, false)
	}
}
