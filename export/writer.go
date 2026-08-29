// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"io"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// Version is the unified IR version nanogo writes.
//
// It is cmd/compile/internal/noder.uirVersion, not the newest version the
// container knows. A reader refuses a stream newer than itself, so the
// version is a property of the release nanogo is pinned to
// (specs/000-decisions.md decision 10) and not of this package.
const Version = pkgbits.V4

// UnsupportedError reports a declaration the writer cannot encode.
//
// The declaration is named, because the package that holds it is what a user
// has to remove from the allowlist or work around.
type UnsupportedError struct {
	Package string // the package being written
	Name    string // the declaration, qualified by its own package
	Reason  string // what the writer cannot encode about it
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("export: %s: cannot write export data for %s: %s", e.Package, e.Name, e.Reason)
}

// Write encodes pkg's exported surface as gc's unified export data and
// returns the payload and its fingerprint.
//
// The payload is what goes between the "$$B\nu" and "\n$$\n" markers of an
// archive's __.PKGDEF member; [Definition] wraps it. The fingerprint has to
// reach the object nanogo writes for the same package: an importing object
// records it in its Autolib entry and the linker refuses a build whose two
// copies disagree.
//
// What is written is the linked form, which is the only form that reaches a
// file. gc writes a stub form first, in which a declaration of another
// package is a name with no definition, and its linker resolves every stub by
// copying the definition out of the archive it came from. nanogo has no such
// two-step, so a declaration of another package that the exported surface
// reaches is written out in full here. Only the universe and unsafe stay
// stubs, which is what every reader of the format expects.
//
// hasInit says the object carries the package's initialisation record. An
// importer reads it and orders its own record after this one, so a package
// that has a record and says it has none is a package whose initialisation
// never runs, and a package that says it has one and has none is a link
// failure. driver/inittask.go decides it.
//
// src is what the package's own source adds to what the checker recorded: the
// positions of the declarations and the function bodies an importer can
// inline. It is nil for a package written without either, and then every
// position is absent and no body reaches the file. A body the writer cannot
// allocate every element of is left out rather than guessed at: see
// [writableBodies].
func Write(pkg *types2.Package, hasInit bool, src *Source) (data []byte, fingerprint [8]byte, err error) {
	var funcs, bodies []InlineFunc
	var fset *syntax.FileSet
	var file func(string) string
	if src != nil {
		fset, file, bodies = src.Fset, src.File, src.Funcs
		funcs = writableBodies(pkg, fset, file, src.Funcs)
	}
	pw := newPkgWriter(pkg, funcs, bodies, fset, file)

	// A declaration the writer cannot encode is reported by panicking, the
	// way the reader reports a stream it cannot decode: the refusal is found
	// deep inside a recursive walk and every frame between it and here would
	// otherwise have to carry an error it cannot act on.
	//
	// The walk also forces declarations the checker had not looked at yet,
	// because an imported package is decoded lazily. So a malformed archive
	// can raise the reader's panic here, after the importer returned and has
	// no error channel left, and it has to come back as an error naming the
	// package rather than as a stack trace.
	defer func() {
		if v := recover(); v != nil {
			data, fingerprint = nil, [8]byte{}
			if u, ok := v.(*UnsupportedError); ok {
				err = u
				return
			}
			if b, ok := v.(*BodyError); ok {
				err = b
				return
			}
			err = fmt.Errorf("export: %s: %v", pkg.Path(), v)
		}
	}()

	public := pw.newWriter(pkgbits.SectionMeta, pkgbits.SyncPublic)
	private := pw.newWriter(pkgbits.SectionMeta, pkgbits.SyncPrivate)
	if public.Idx != pkgbits.PublicRootIdx || private.Idx != pkgbits.PrivateRootIdx {
		return nil, fingerprint, fmt.Errorf("export: the two root elements got indices %v and %v", public.Idx, private.Idx)
	}

	// Walk the exported package-scope declarations, which writes them and
	// everything they reach. Scope.Names is sorted, so the order of the walk
	// is fixed by the names and not by the checker's map
	// (specs/053-determinism.md), and the index of every object below is
	// fixed by the order it was first reached.
	//
	// Exported only. An unexported declaration is written when an exported
	// one refers to it and not otherwise.
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		if obj := scope.Lookup(name); obj != nil && obj.Exported() {
			pw.objIdx(obj)
		}
	}

	// The bodies, before the public root and after the walk. Encoding one
	// allocates the elements it names, so a body can add an object to the
	// file, and the public root's list is the count of the objects the file
	// ends up holding.
	entries := pw.writeBodies(pkg.Path(), funcs)

	// The public root: the package, then every object in the file.
	//
	// Every object, not only this package's: gc's linker lists what it
	// copied, which is the transitive closure, and gc's reader needs the
	// whole list. An importer writes a stub for each declaration of another
	// package it mentions, and resolves that stub by looking the name up in
	// the table this list builds. A root that named only this package's
	// declarations would leave gc unable to resolve, say, io.Reader in the
	// signature of a bufio function it imported, and the failure is an
	// internal compiler error a long way from here.
	public.pkg(pkg)
	n := pw.NumElems(pkgbits.SectionObj)
	public.Len(n)
	for i := range n {
		public.Sync(pkgbits.SyncObject)
		public.Reloc(pkgbits.SectionObj, pkgbits.Index(i))
		public.Len(0) // no explicit type arguments
	}
	public.Sync(pkgbits.SyncEOF)
	public.Flush()

	// The private root: whether the object carries an initialisation record,
	// and the list of function bodies an importer can inline. The entry
	// names its declaration by gc's linker symbol name and by the real
	// import path, which is what gc looks it up under: the empty path
	// pkgIdx writes for the package being compiled is a property of the
	// package element and not of this list.
	private.Bool(hasInit)
	private.Len(len(entries))
	for _, e := range entries {
		private.String(e.path)
		private.String(e.name)
		private.Reloc(pkgbits.SectionBody, e.idx)
	}
	private.Sync(pkgbits.SyncEOF)
	private.Flush()

	var buf sizedBuffer
	fingerprint, err = pw.DumpTo(&buf)
	if err != nil {
		return nil, fingerprint, err
	}
	return buf.b, fingerprint, nil
}

// sizedBuffer collects what the encoder writes. It exists so that this file
// depends on io and not on bytes for one method.
type sizedBuffer struct{ b []byte }

func (s *sizedBuffer) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

var _ io.Writer = (*sizedBuffer)(nil)

// pkgWriter holds the elements written so far for one package.
//
// Every index map is read by key and never ranged over. The order of a
// section is the order its elements were first written, which the walk below
// fixes (specs/053-determinism.md).
type pkgWriter struct {
	pkgbits.PkgEncoder

	curpkg *types2.Package

	pkgsIdx     map[*types2.Package]pkgbits.Index
	typsIdx     map[types2.Type]pkgbits.Index
	objsIdx     map[types2.Object]pkgbits.Index
	posBasesIdx map[string]pkgbits.Index

	// inline names the declarations whose body the file carries, so that a
	// declaration's extension data says it has one. The body itself is
	// reached through the private root and not through the extension data,
	// which is the shape of the inlining path.
	inline map[types2.Object]*InlineFunc

	// bodies holds every body the caller built, which is what a generic
	// declaration needs. Its body is reached through its own extension data
	// rather than through the private root, and it carries the dictionary
	// the file's dictionary is written from, so a generic declaration with
	// no entry here cannot be written at all.
	bodies map[types2.Object]*InlineFunc

	// lits holds the element claimed for each function literal a body
	// named, which is filled in after the body that named it.
	lits map[*FuncLitExpr]*pkgbits.Encoder

	// fileName maps a parsed file name to the one a position base records.
	fileName func(string) string

	// fset resolves a declaration's position. It is nil for a package
	// written without its source, and every position is then absent.
	fset *syntax.FileSet
}

// newPkgWriter returns a writer for one package.
//
// inline are the declarations whose body the private root offers an importer
// for inlining, and bodies is every body the caller built. The two are
// separate because a generic declaration's body reaches an importer through
// the declaration and never through the private root's list.
func newPkgWriter(pkg *types2.Package, inline, bodies []InlineFunc, fset *syntax.FileSet, file func(string) string) *pkgWriter {
	pw := &pkgWriter{
		PkgEncoder:  pkgbits.NewPkgEncoder(Version),
		curpkg:      pkg,
		pkgsIdx:     make(map[*types2.Package]pkgbits.Index),
		typsIdx:     make(map[types2.Type]pkgbits.Index),
		objsIdx:     make(map[types2.Object]pkgbits.Index),
		posBasesIdx: make(map[string]pkgbits.Index),
		inline:      make(map[types2.Object]*InlineFunc, len(inline)),
		bodies:      make(map[types2.Object]*InlineFunc, len(bodies)),
		lits:        make(map[*FuncLitExpr]*pkgbits.Encoder),
		fileName:    file,
		fset:        fset,
	}
	for i := range inline {
		pw.inline[inline[i].Obj] = &inline[i]
	}
	for i := range bodies {
		pw.bodies[bodies[i].Obj] = &bodies[i]
	}
	return pw
}

// writer writes one element.
type writer struct {
	*pkgbits.Encoder
	p *pkgWriter

	// dict is the dictionary of the declaration this element belongs to. It
	// is nil for an element that belongs to no declaration, and a type
	// written through it can then name no type parameter.
	dict *Dict
}

func (pw *pkgWriter) newWriter(k pkgbits.SectionKind, marker pkgbits.SyncMarker) *writer {
	return &writer{Encoder: pw.NewEncoder(k, marker), p: pw}
}

// refuse reports a declaration the writer cannot encode.
func (pw *pkgWriter) refuse(name, reason string) {
	panic(&UnsupportedError{Package: pw.curpkg.Path(), Name: name, Reason: reason})
}

// objName returns the name a refusal reports an object by.
func objName(obj types2.Object) string {
	if obj.Pkg() == nil {
		return obj.Name()
	}
	return obj.Pkg().Path() + "." + obj.Name()
}

// @@@ Positions

// pos writes the position of a declaration nanogo compiled.
//
// The position is absent for a declaration of another package, and that gap
// stays. nanogo's syntax.Pos is an offset into the FileSet the compiled files
// were parsed with (specs/010-scanner-and-positions.md), the reader drops the
// one gc wrote because a foreign file has no place in that FileSet, and the
// writer has nothing to put back. The field is written either way, because it
// is inline in the element and a reader that skipped it would desync.
//
// A declaration nanogo compiled needs a real position for a reason beyond a
// diagnostic. gc rebases every position of an inlined body onto the call it
// inlined into, and it rebases a parameter's position with the rest. An
// absent position has no base to rebase, and gc stops with "no old PosBase"
// naming neither the declaration nor the package that wrote it. So a position
// is written wherever the writer has one.
func (w *writer) pos(p syntax.Pos) {
	w.Sync(pkgbits.SyncPos)
	fset := w.p.fset
	if fset == nil || !p.IsKnown() {
		w.Bool(false)
		return
	}
	at := fset.Position(p)
	if at.Filename == "" {
		w.Bool(false)
		return
	}
	w.Bool(true)
	w.Reloc(pkgbits.SectionPosBase, w.p.posBaseIdx(at.Filename))
	w.Uint(uint(at.Line))
	w.Uint(uint(at.Col))
}

// @@@ Packages

func (w *writer) pkg(pkg *types2.Package) {
	w.pkgRef(w.p.pkgIdx(pkg))
}

func (w *writer) pkgRef(idx pkgbits.Index) {
	w.Sync(pkgbits.SyncPkg)
	w.Reloc(pkgbits.SectionPkg, idx)
}

// pkgIdx returns the index of pkg, writing its element if it is new.
func (pw *pkgWriter) pkgIdx(pkg *types2.Package) pkgbits.Index {
	if idx, ok := pw.pkgsIdx[pkg]; ok {
		return idx
	}

	w := pw.newWriter(pkgbits.SectionPkg, pkgbits.SyncPkgDef)
	pw.pkgsIdx[pkg] = w.Idx

	switch pkg {
	case nil:
		// The universe, under the path godoc gives it. A reader has to tell
		// it from a package a user wrote.
		w.String("builtin")
	case types2.Unsafe:
		w.String("unsafe")
	default:
		// The package being compiled writes an empty path, and a reader maps
		// it back through the path it opened the archive under. Writing the
		// real path instead makes gc report a mismatched import path, which
		// points at -p and not at here.
		path := ""
		if pkg != pw.curpkg {
			path = pkg.Path()
		}
		w.String(path)
		w.String(pkg.Name())

		imports := pkg.Imports()
		w.Len(len(imports))
		for _, imp := range imports {
			w.pkg(imp)
		}
	}

	return w.Flush()
}

// @@@ Types

// typ writes a use of typ.
func (w *writer) typ(typ types2.Type) {
	w.typeRef(w.p.typeRefOf(typ, w.dict))
}

// typeRef writes a reference to a type already resolved.
//
// A derived type is a slot of the enclosing declaration's dictionary and not
// an element of the package, because there is one such type per
// instantiation and the file holds no instantiation.
func (w *writer) typeRef(t TypeUse) {
	w.Sync(pkgbits.SyncType)
	if w.Bool(t.Derived) {
		w.Len(int(t.Idx))
		return
	}
	w.Reloc(pkgbits.SectionType, t.Idx)
}

// typeRefOf resolves a type to the dictionary slot or the element it is
// written as.
func (pw *pkgWriter) typeRefOf(typ types2.Type, dict *Dict) TypeUse {
	if dict != nil {
		slot, derived, err := dict.Derive(typ)
		if err != nil {
			pw.refuse(fmt.Sprintf("%v", typ), err.Error())
		}
		if derived {
			return TypeUse{Derived: true, Idx: pkgbits.Index(slot), Type: typ}
		}
	}
	return TypeUse{Idx: pw.typIdx(typ), Type: typ}
}

// derivedTypeIdx writes the element of a derived type and returns its index.
//
// The element is not interned. It names the slots of the dictionary it was
// written for, so one type reached from two declarations is two elements.
// That is the format's rule and not a choice this writer makes.
func (pw *pkgWriter) derivedTypeIdx(typ types2.Type, dict *Dict) pkgbits.Index {
	w := pw.newWriter(pkgbits.SectionType, pkgbits.SyncTypeIdx)
	w.dict = dict
	w.typDef(typ)
	return w.Flush()
}

// typIdx returns the index of typ, writing its element if it is new.
//
// Types are interned in first-use order, and a type that refers to itself is
// expressed by the index and not by recursion: the index is claimed before
// the element is written, so a struct with a pointer to itself terminates.
func (pw *pkgWriter) typIdx(typ types2.Type) pkgbits.Index {
	// A local alias appears inside a function body and a package-scope
	// alias to a local one can be reached from the surface. Upstream strips
	// the same way, to keep two local aliases from colliding on one symbol.
	for {
		alias, ok := typ.(*types2.Alias)
		if !ok || isGlobal(alias.Obj()) {
			break
		}
		typ = alias.Rhs()
	}

	if idx, ok := pw.typsIdx[typ]; ok {
		return idx
	}

	w := pw.newWriter(pkgbits.SectionType, pkgbits.SyncTypeIdx)
	pw.typsIdx[typ] = w.Idx
	w.typDef(typ)
	return w.Flush()
}

// typDef writes the definition of one type into its own element.
func (w *writer) typDef(typ types2.Type) {
	pw := w.p
	switch typ := typ.(type) {
	default:
		pw.refuse(fmt.Sprintf("%v", typ), fmt.Sprintf("the type is a %T, which the format has no tag for", typ))

	case *types2.Basic:
		switch kind := typ.Kind(); {
		case kind == types2.Invalid:
			pw.refuse(fmt.Sprintf("%v", typ), "the type is invalid, so the package did not type check")
		case types2.Typ[kind] == typ:
			w.Code(pkgbits.TypeBasic)
			w.Len(int(kind))
		default:
			// byte and rune, which are the two basic types that are not
			// their own kind's canonical type. They are written as a
			// reference to the universe's TypeName.
			obj := types2.Universe.Lookup(typ.Name()).(*types2.TypeName)
			w.Code(pkgbits.TypeNamed)
			w.obj(obj, nil)
		}

	case *types2.Named:
		w.Code(pkgbits.TypeNamed)
		w.obj(typ.Obj(), typeArgs(typ.TypeArgs()))

	case *types2.Alias:
		w.Code(pkgbits.TypeNamed)
		w.obj(typ.Obj(), typeArgs(typ.TypeArgs()))

	case *types2.TypeParam:
		// A type parameter is the dictionary's own numbering, which the
		// reader resolves against the type arguments the instantiation
		// supplied. Without a dictionary there is nothing to resolve it
		// against, and an index written anyway is a type gc reads as
		// whatever slot 0 happens to hold.
		if w.dict == nil {
			pw.refuse(objName(typ.Obj()), genericReason)
		}
		idx, ok := w.dict.TypeParamIndex(typ)
		if !ok {
			pw.refuse(objName(typ.Obj()), "the type parameter is not one the declaration's dictionary holds")
		}
		w.Code(pkgbits.TypeTypeParam)
		w.Len(idx)

	case *types2.Array:
		w.Code(pkgbits.TypeArray)
		w.Uint64(uint64(typ.Len()))
		w.typ(typ.Elem())

	case *types2.Chan:
		w.Code(pkgbits.TypeChan)
		w.Len(int(typ.Dir()))
		w.typ(typ.Elem())

	case *types2.Map:
		w.Code(pkgbits.TypeMap)
		w.typ(typ.Key())
		w.typ(typ.Elem())

	case *types2.Pointer:
		w.Code(pkgbits.TypePointer)
		w.typ(typ.Elem())

	case *types2.Signature:
		if typ.TypeParams().Len() != 0 {
			pw.refuse(fmt.Sprintf("%v", typ), genericReason)
		}
		w.Code(pkgbits.TypeSignature)
		w.signature(typ)

	case *types2.Slice:
		w.Code(pkgbits.TypeSlice)
		w.typ(typ.Elem())

	case *types2.Struct:
		w.Code(pkgbits.TypeStruct)
		w.structType(typ)

	case *types2.Interface:
		// any is written as a reference to its TypeName, so that every
		// reader reconstructs the one canonical empty interface rather than
		// a fresh one.
		if anyTypeName := types2.Universe.Lookup("any"); anyTypeName != nil &&
			types2.Unalias(typ) == types2.Unalias(anyTypeName.Type()) {
			w.Code(pkgbits.TypeNamed)
			w.obj(anyTypeName, nil)
			break
		}
		w.Code(pkgbits.TypeInterface)
		w.interfaceType(typ)

	case *types2.Union:
		w.Code(pkgbits.TypeUnion)
		w.unionType(typ)
	}
}

// genericReason is what a type parameter reached outside a dictionary means.
//
// The writer carries a generic function, whose dictionary is the one its body
// was built against ([Dict]). Everywhere else a type parameter is a name with
// no dictionary to resolve it, and an index written anyway is a type gc reads
// as whatever that slot holds.
const genericReason = "the type parameter is named where no object dictionary resolves it, and " +
	"specs/013-generics.md has which generic shapes the writer carries"

func typeArgs(l *types2.TypeList) []types2.Type {
	if l.Len() == 0 {
		return nil
	}
	s := make([]types2.Type, l.Len())
	for i := range s {
		s[i] = l.At(i)
	}
	return s
}

func (w *writer) structType(typ *types2.Struct) {
	w.Len(typ.NumFields())
	for i := 0; i < typ.NumFields(); i++ {
		f := typ.Field(i)
		w.pos(f.Pos())
		w.selector(f)
		w.typ(f.Type())
		w.String(typ.Tag(i))
		w.Bool(f.Embedded())
	}
}

func (w *writer) unionType(typ *types2.Union) {
	w.Len(typ.Len())
	for i := 0; i < typ.Len(); i++ {
		t := typ.Term(i)
		w.Bool(t.Tilde())
		w.typ(t.Type())
	}
}

func (w *writer) interfaceType(typ *types2.Interface) {
	// An interface with no embedded types that is not a method set is
	// comparable's underlying type, which only a type constraint can name.
	if typ.NumEmbeddeds() == 0 && !typ.IsMethodSet() {
		w.p.refuse(fmt.Sprintf("%v", typ), genericReason)
	}

	w.Len(typ.NumExplicitMethods())
	w.Len(typ.NumEmbeddeds())

	// The implicit flag is only written where it can be true, which is an
	// interface a constraint literal produced. Upstream skips it elsewhere
	// as a space optimisation and a reader knows the same rule.
	if typ.NumExplicitMethods() == 0 && typ.NumEmbeddeds() == 1 {
		w.Bool(typ.IsImplicit())
	}

	for i := 0; i < typ.NumExplicitMethods(); i++ {
		m := typ.ExplicitMethod(i)
		sig := m.Type().(*types2.Signature)
		if sig.TypeParams().Len() != 0 {
			w.p.refuse(objName(m), genericReason)
		}
		w.pos(m.Pos())
		w.selector(m)
		w.signature(sig)
	}

	for i := 0; i < typ.NumEmbeddeds(); i++ {
		w.typ(typ.EmbeddedType(i))
	}
}

func (w *writer) signature(sig *types2.Signature) {
	w.Sync(pkgbits.SyncSignature)
	w.params(sig.Params())
	w.params(sig.Results())
	w.Bool(sig.Variadic())
}

func (w *writer) params(typ *types2.Tuple) {
	w.Sync(pkgbits.SyncParams)
	w.Len(typ.Len())
	for i := 0; i < typ.Len(); i++ {
		w.param(typ.At(i))
	}
}

func (w *writer) param(param *types2.Var) {
	w.Sync(pkgbits.SyncParam)
	w.pos(param.Pos())
	w.localIdent(param)
	w.typ(param.Type())
}

// @@@ Objects

// obj writes a use of obj, instantiated with explicits.
func (w *writer) obj(obj types2.Object, explicits []types2.Type) {
	idx := w.p.objIdx(obj)
	w.Sync(pkgbits.SyncObject)
	w.Reloc(pkgbits.SectionObj, idx)
	w.Len(len(explicits))
	for _, t := range explicits {
		w.typ(t)
	}
}

// objIdx returns the index of obj, writing its four elements if it is new.
//
// An object is four elements at one index: the name and its kind, the public
// definition, the compiler's private extension data, and the dictionary. The
// index is claimed before any of them is written, so a declaration that
// refers to itself terminates.
func (pw *pkgWriter) objIdx(obj types2.Object) pkgbits.Index {
	if idx, ok := pw.objsIdx[obj]; ok {
		return idx
	}

	dict := pw.dictOf(obj)

	w := pw.newWriter(pkgbits.SectionObj, pkgbits.SyncObject1)
	wext := pw.newWriter(pkgbits.SectionObjExt, pkgbits.SyncObject1)
	wname := pw.newWriter(pkgbits.SectionName, pkgbits.SyncObject1)
	wdict := pw.newWriter(pkgbits.SectionObjDict, pkgbits.SyncObject1)

	pw.objsIdx[obj] = w.Idx // claim the index, so a cycle terminates
	if wext.Idx != w.Idx || wname.Idx != w.Idx || wdict.Idx != w.Idx {
		panic(fmt.Errorf("export: the four elements of %v got different indices", objName(obj)))
	}

	w.dict, wext.dict = dict, dict
	code := w.doObj(wext, obj)
	w.Flush()
	wext.Flush()

	wname.qualifiedIdent(obj)
	wname.Code(code)
	wname.Flush()

	wdict.objDict(dict)
	wdict.Flush()

	return w.Idx
}

// dictOf returns the dictionary obj is written with.
//
// A declaration with no type parameter gets an empty one, and nothing it
// names can be derived. A generic declaration gets the dictionary its body
// was built with, because the slots the body names and the entries the
// dictionary holds are one allocation ([Dict]) and a second one would be a
// second numbering.
//
// Two generic shapes are refused by name, and each is refused because its
// dictionary is not the one a body carries:
//
//   - A generic method's dictionary holds its receiver's type parameters
//     ahead of its own, and the body builder does not build one.
//   - A generic declaration of another package needs the body that package's
//     archive holds, and the dictionary that archive numbered it against.
func (pw *pkgWriter) dictOf(obj types2.Object) *Dict {
	tparams := objTypeParams(obj)
	fn, isFunc := obj.(*types2.Func)
	rtparams := isFunc && fn.Signature().RecvTypeParams().Len() != 0
	if len(tparams) == 0 && !rtparams {
		return &Dict{Pkg: pw.curpkg}
	}
	if !isFunc {
		return pw.typeDict(obj.(*types2.TypeName), tparams)
	}
	if rtparams {
		pw.refuse(objName(obj), "the declaration is a method with type parameters, and its dictionary holds the receiver's type parameters ahead of its own")
	}
	if obj.Pkg() != pw.curpkg {
		pw.refuse(objName(obj), "the declaration is generic and belongs to another package, whose body and dictionary this writer does not read back")
	}
	fb := pw.bodies[obj]
	if fb == nil || fb.Body == nil || fb.Body.Dict == nil {
		pw.refuse(objName(obj), "the declaration is generic and no body was built for it, and a generic declaration cannot reach a file without one")
	}
	if !fb.Body.HasBlock {
		pw.refuse(objName(obj), "the declaration is generic and has no body, and a generic declaration cannot reach a file without one")
	}
	return fb.Body.Dict
}

// typeDict returns the dictionary a generic type declaration is written with.
//
// It is one dictionary for the type and every method the type declares, and
// not one per method: gc writes the methods inside the type's element and
// their bodies against the type's dictionary, so a method numbered against a
// dictionary of its own is a method gc reads as naming different types
// (specs/013-generics.md).
//
// A type with no method needs nothing the writer cannot allocate as it goes,
// and its dictionary is allocated empty here. doObj writes the underlying
// type and each derived type takes its slot as it is written, and objDict
// runs after doObj, so the list it writes is complete.
//
// A type with methods needs the slots each body names, which are numbered
// between the signatures and not after them, and only
// [BodySource.BuildTypeBodies] can number those. So the dictionary of such a
// type is the one its bodies were built against, found through any of them,
// and a type whose methods did not all come from one such call is refused.
//
// The package the dictionary belongs to is the type's own and not the package
// being written. A file's export data is the linked form, so a generic type
// another package declares is written out here in full. Its method bodies
// exist only in that package's archive, which this writer does not read back,
// so a foreign generic type with methods is refused by name.
//
// An alias gets a dictionary of its own even when it names a generic type
// that declares methods. Its type parameters are objects of the alias
// declaration, so doObj writes a right-hand side that names them and every
// slot that walk allocates belongs here. The methods stay with the aliased
// type and are written inside that type's element against that type's
// dictionary.
func (pw *pkgWriter) typeDict(obj *types2.TypeName, tparams []*types2.TypeParam) *Dict {
	if obj.IsAlias() {
		return &Dict{Pkg: obj.Pkg(), TypeParams: tparams}
	}
	named, _ := obj.Type().(*types2.Named)
	if named == nil || named.NumMethods() == 0 {
		return &Dict{Pkg: obj.Pkg(), TypeParams: tparams}
	}
	if obj.Pkg() != pw.curpkg {
		pw.refuse(objName(obj), "the declaration is a generic type of another package and declares methods, whose bodies exist only in that package's archive, which this writer does not read back")
	}
	var shared *Dict
	for i := range named.NumMethods() {
		m := named.Method(i)
		fb := pw.bodies[m]
		if fb == nil || fb.Body == nil || fb.Body.Dict == nil || !fb.Body.HasBlock {
			pw.refuse(objName(m), "the method belongs to a generic type and no body was built for it, and the type's dictionary is short by the slots the body names")
		}
		if shared == nil {
			shared = fb.Body.Dict
			continue
		}
		if fb.Body.Dict != shared {
			pw.refuse(objName(m), "the method's body was numbered against a dictionary of its own rather than against the one the type shares with every method it declares")
		}
	}
	return shared
}

// doObj writes obj's public definition to w and its extension data to wext,
// and returns the code that says which kind of declaration it is.
func (w *writer) doObj(wext *writer, obj types2.Object) pkgbits.CodeObj {
	// Only the universe and unsafe stay stubs. Every other declaration is
	// written out in full, because a file's export data is the linked form
	// and a reader of one asserts that no other stub can appear.
	if obj.Pkg() == nil || obj.Pkg() == types2.Unsafe {
		return pkgbits.ObjStub
	}

	switch obj := obj.(type) {
	default:
		w.p.refuse(objName(obj), fmt.Sprintf("the declaration is a %T, which the format has no code for", obj))
		panic("unreachable")

	case *types2.Const:
		w.pos(obj.Pos())
		w.typ(obj.Type())
		w.Value(obj.Val())
		return pkgbits.ObjConst

	case *types2.Func:
		sig := obj.Signature()
		w.pos(obj.Pos())
		w.Bool(false) // not a method with type parameters of its own
		w.typeParamNames(objTypeParams(obj))
		w.signature(sig)
		w.pos(obj.Pos()) // the declaration's position, which the linker copies
		wext.funcExt(obj, sig, false)
		return pkgbits.ObjFunc

	case *types2.TypeName:
		if obj.IsAlias() {
			w.pos(obj.Pos())
			rhs := obj.Type()
			if alias, ok := rhs.(*types2.Alias); ok {
				rhs = alias.Rhs()
			}
			w.typeParamNames(objTypeParams(obj))
			w.typ(rhs)
			return pkgbits.ObjAlias
		}

		named := obj.Type().(*types2.Named)

		// A generic type with methods carries the dictionary its bodies were
		// numbered against ([pkgWriter.typeDict]), and every slot of it is
		// already allocated. A slot allocated here would land after every
		// slot a body took, where gc expects one of the type's own, and gc
		// reads the wrong slot without complaint. So the check is that the
		// walk below allocates none.
		sealed := named.NumMethods() != 0 && w.dict != nil && w.dict.Generic()
		before := 0
		if sealed {
			before = len(w.dict.Derived)
		}

		w.pos(obj.Pos())
		w.typeParamNames(objTypeParams(obj))
		wext.typeExt()
		w.typ(named.Underlying())

		// The methods, in the order the checker holds them, which is the
		// order they were declared in. That is gc's order too, so the two
		// compilers' export data for one package stays comparable.
		w.Len(named.NumMethods())
		for i := 0; i < named.NumMethods(); i++ {
			w.method(wext, named.Method(i))
		}
		if sealed && len(w.dict.Derived) != before {
			w.p.refuse(objName(obj), fmt.Sprintf("the type and its methods named %d derived types the dictionary its bodies were numbered against does not hold", len(w.dict.Derived)-before))
		}
		// No method has type parameters of its own: objIdx refuses one.
		w.Len(0)
		return pkgbits.ObjType

	case *types2.Var:
		w.pos(obj.Pos())
		w.typ(obj.Type())
		wext.varExt()
		return pkgbits.ObjVar
	}
}

// objDict writes the dictionary an object needs to be read back.
//
// The dictionary is the one the body was built against ([Dict]), so the slots
// the body names and the entries written here are one allocation and cannot
// disagree. A declaration with no type parameter carries an empty dictionary,
// which is eight zeros.
//
// The order is gc's objDict: the three type parameter counts, then the
// constraints, then the derived types, then one flag per type parameter, then
// the four runtime lists. The constraints come before the count of derived
// types because a constraint can name a derived type of its own, and it takes
// its slot there.
func (w *writer) objDict(dict *Dict) {
	w.dict = dict

	w.Len(len(dict.Implicits))
	if w.Version().Has(pkgbits.GenericMethods) {
		w.Len(len(dict.Receivers))
	} else if len(dict.Receivers) != 0 {
		w.p.refuse("a generic method", "the stream's version has no encoding for a method with its own type parameters")
	}
	w.Len(len(dict.TypeParams))

	for _, tp := range dict.Receivers {
		w.typ(tp.Constraint())
	}
	for _, tp := range dict.TypeParams {
		w.typ(tp.Constraint())
	}

	// The derived types, each as a reference to an element of its own. The
	// count is taken after the constraints and before the loop, because the
	// element of a derived type names only slots already allocated: its
	// components were walked before it was.
	n := len(dict.Derived)
	w.Len(n)
	for i := range n {
		w.Reloc(pkgbits.SectionType, w.p.derivedTypeIdx(dict.Derived[i], dict))
	}
	if len(dict.Derived) != n {
		w.p.refuse("an object dictionary", fmt.Sprintf("writing its %d derived types named %d more", n, len(dict.Derived)-n))
	}

	// One flag per type parameter, saying whether its constraint is a
	// method set. gc reads it to decide how far it may share one compiled
	// body between type arguments.
	for _, tps := range [][]*types2.TypeParam{dict.Implicits, dict.Receivers, dict.TypeParams} {
		for _, tp := range tps {
			iface, ok := tp.Underlying().(*types2.Interface)
			if !ok {
				w.p.refuse(objName(tp.Obj()), "the constraint of the type parameter is not an interface")
			}
			w.Bool(iface.IsMethodSet())
		}
	}

	w.Len(len(dict.MethodExprs))
	for _, m := range dict.MethodExprs {
		w.Len(m.TypeParam)
		w.selectorRef(m.Sel)
	}

	w.Len(len(dict.Subdicts))
	for _, o := range dict.Subdicts {
		w.objRef(o)
	}

	w.Len(len(dict.RTypes))
	for _, t := range dict.RTypes {
		w.typeRef(w.p.resolve(t, dict))
	}

	w.Len(len(dict.Itabs))
	for _, it := range dict.Itabs {
		w.typeRef(w.p.resolve(it.Type, dict))
		w.typeRef(w.p.resolve(it.Iface, dict))
	}
}

// resolve gives a reference the body built the index it is written with.
//
// A derived reference already carries its slot. An ordinary one carries only
// the type, because the builder had no package to allocate an element in.
func (pw *pkgWriter) resolve(t TypeUse, dict *Dict) TypeUse {
	if t.Derived {
		return t
	}
	return pw.typeRefOf(t.Type, dict)
}

// objRef writes a use of one declaration at one instantiation, as the
// dictionary holds it.
func (w *writer) objRef(o ObjUse) {
	if o.Obj == nil {
		w.p.refuse(o.Name, "the dictionary names a declaration the checker recorded no object for")
	}
	w.Sync(pkgbits.SyncObject)
	w.Reloc(pkgbits.SectionObj, w.p.objIdx(o.Obj))
	w.Len(len(o.Targs))
	for _, t := range o.Targs {
		w.typeRef(w.p.resolve(t, w.dict))
	}
}

// selectorRef writes a field or method name the dictionary holds.
func (w *writer) selectorRef(sel Selector) {
	w.Sync(pkgbits.SyncSelector)
	w.pkg(sel.Pkg)
	w.String(sel.Name)
}

// typeParamNames writes the name and the position of each type parameter.
func (w *writer) typeParamNames(tparams []*types2.TypeParam) {
	w.Sync(pkgbits.SyncTypeParamNames)
	for _, tp := range tparams {
		obj := tp.Obj()
		w.pos(obj.Pos())
		w.localIdent(obj)
	}
}

func (w *writer) method(wext *writer, meth *types2.Func) {
	sig := meth.Signature()
	if sig.TypeParams().Len() != 0 {
		// The format writes such a method as a declaration of its own,
		// whose dictionary holds the receiver's type parameters ahead of its
		// own. Written here it would be a member of its receiver's element
		// with no dictionary at all.
		w.p.refuse(objName(meth), "the method is generic, and the format writes a generic method as a declaration of its own whose dictionary holds the receiver's type parameters ahead of the method's")
	}
	// The receiver's type parameter names, which is what gc writes and what
	// gc reads: the reader takes the count from the dictionary the element
	// carries and not from a length in the stream, so a method of a generic
	// type that wrote none would leave every later field one name short.
	//
	// The dictionary the names are counted against is the type's, which
	// [pkgWriter.typeDict] built and which the body of this method was
	// numbered against.
	w.Sync(pkgbits.SyncMethod)
	w.pos(meth.Pos())
	w.selector(meth)
	w.typeParamNames(recvTypeParams(sig))
	w.param(sig.Recv())
	w.signature(sig)
	w.pos(meth.Pos()) // the declaration's position, which the linker copies

	wext.funcExt(meth, sig, true)
}

func (w *writer) qualifiedIdent(obj types2.Object) {
	w.Sync(pkgbits.SyncSym)
	w.pkg(obj.Pkg())
	w.String(obj.Name())
}

func (w *writer) localIdent(obj types2.Object) {
	w.Sync(pkgbits.SyncLocalIdent)
	w.pkg(obj.Pkg())
	w.String(obj.Name())
}

func (w *writer) selector(obj types2.Object) {
	w.Sync(pkgbits.SyncSelector)
	w.pkg(obj.Pkg())
	w.String(obj.Name())
}

// @@@ Compiler extensions

// funcExt writes the extension data of a function or method.
//
// This is the shape gc's linker writes, not the shape gc's writer writes:
// what reaches a file has the escape analysis results and the inlining cost
// in it and no reference to a body element. See README.md for what nanogo
// leaves out of it and what each omission costs.
func (w *writer) funcExt(fn *types2.Func, sig *types2.Signature, isMethod bool) {
	// A declaration with type parameters carries its body, and it is the
	// only shape in which one reaches a file. gc's reader records a
	// definition for a declaration with type parameters before it reads the
	// extension data, and the relocated form below asserts there is none, so
	// writing that form for a generic declaration stops gc inside an
	// assertion rather than at a diagnostic.
	if w.dict != nil && w.dict.Generic() {
		idx := w.p.declBodyIdx(fn)
		w.Sync(pkgbits.SyncFuncExt)
		w.pragmaFlag()
		w.linkname()
		w.Bool(false) // a reference to a body element, not relocated data
		w.Reloc(pkgbits.SectionBody, idx)
		w.Sync(pkgbits.SyncEOF)
		return
	}

	w.Sync(pkgbits.SyncFuncExt)
	w.pragmaFlag()
	w.linkname()

	// Relocated extension data, as opposed to a reference to a body element.
	w.Bool(true)

	// The ABI the definition is at. Every function nanogo compiles is
	// ABIInternal (specs/030-abi.md), and a declaration satisfied by
	// assembly is refused before this point.
	w.Uint64(uint64(obj.ABIInternal))

	// One escape analysis result per receiver and parameter, in that order.
	// An empty note parses as "leaks to the heap", which is the conservative
	// answer and the one a caller must assume when nanogo has run no escape
	// analysis (specs/026-escape-analysis.md is unbuilt).
	n := sig.Params().Len()
	if isMethod {
		n++
	}
	for i := 0; i < n; i++ {
		w.String("")
	}

	// Whether the file carries a body an importer can inline, and what it
	// costs gc's inlining budget. gc reads the body itself from the private
	// root's list, keyed by this declaration's linker symbol name.
	if inl := w.p.inline[fn]; w.Bool(inl != nil) {
		w.Len(inl.Cost)

		// Whether gc may leave the results of the inlined call in the
		// caller's own variables rather than in temporaries it creates
		// first. false is the answer that needs nothing proved about the
		// body, and gc reads it as an instruction to create them.
		w.Bool(false)
	}
	w.Sync(pkgbits.SyncEOF)
}

// typeExt writes the extension data of a defined type.
//
// Three fields and no more. Each method's extension data follows in the same
// element, and [writer.method] is what writes it: gc's linker re-encodes a
// type and its methods together, and gc's writer, which is the one this
// mirrors, writes the three fields here and leaves the methods to the loop
// that encodes them. Writing them in both places appends a second copy that
// no reader consumes, which nothing observes and which doubles the extension
// data of every method in the package.
func (w *writer) typeExt() {
	w.Sync(pkgbits.SyncTypeExt)
	w.pragmaFlag()

	// The linker symbol indices of the type descriptors for T and *T. -1
	// says the importer must find them by name, which is what gc writes
	// before it has assigned indices of its own.
	w.Int64(-1)
	w.Int64(-1)
}

func (w *writer) varExt() {
	w.Sync(pkgbits.SyncVarExt)
	w.linkname()
}

// pragmaFlag writes an empty set of //go: directives.
//
// The driver records the directives a declaration carries (driver/pragma.go)
// and nothing carries them this far: the flag bits there are nanogo's own
// numbering, and this field is read with gc's, so writing them would need the
// two to be one table. specs/016-directives-and-pragmas.md owns the gap and
// README.md records what it costs an importer.
func (w *writer) pragmaFlag() {
	w.Sync(pkgbits.SyncPragma)
	w.Int(0)
}

// linkname writes an absent linker symbol name.
//
// The leading -1 says the symbol has no index in this object's symbol table,
// so an importer refers to it by name. The name is empty because nanogo
// records no //go:linkname.
func (w *writer) linkname() {
	w.Sync(pkgbits.SyncLinkname)
	w.Int64(-1)
	w.String("")
	w.Bool(false)
}

// @@@ Helpers

// isGlobal reports whether obj was declared at package scope.
func isGlobal(obj types2.Object) bool {
	return obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope()
}

// recvTypeParams returns the type parameters a method's receiver declares.
func recvTypeParams(sig *types2.Signature) []*types2.TypeParam {
	l := sig.RecvTypeParams()
	if l == nil || l.Len() == 0 {
		return nil
	}
	s := make([]*types2.TypeParam, l.Len())
	for i := range s {
		s[i] = l.At(i)
	}
	return s
}

// objTypeParams returns the type parameters obj declares.
func objTypeParams(obj types2.Object) []*types2.TypeParam {
	var l *types2.TypeParamList
	switch obj := obj.(type) {
	case *types2.Func:
		l = obj.Signature().TypeParams()
	case *types2.TypeName:
		switch t := obj.Type().(type) {
		case *types2.Named:
			l = t.TypeParams()
		case *types2.Alias:
			l = t.TypeParams()
		}
	}
	if l == nil || l.Len() == 0 {
		return nil
	}
	s := make([]*types2.TypeParam, l.Len())
	for i := range s {
		s[i] = l.At(i)
	}
	return s
}

// Definition returns the body of an archive's __.PKGDEF member.
//
// It is [read.go]'s container from the other side: the object header line,
// then the header lines the compiler adds, then the export data section. The
// blank line ends the headers, "$$B" says the section is binary and 'u' says
// it is the unified format.
//
// header is the "go object ..." line the installed toolchain writes, with its
// newline; obj.VerifyToolchain reports it. main says the package is a main
// package, which is the one fact the linker reads out of the header lines.
func Definition(header string, main bool, payload []byte) ([]byte, error) {
	if len(header) == 0 || header[len(header)-1] != '\n' || !hasPrefix(header, "go object ") {
		return nil, fmt.Errorf("export: bad toolchain header %q", header)
	}
	var b []byte
	b = append(b, header...)
	if main {
		b = append(b, "main\n"...)
	}
	b = append(b, '\n') // the blank line that ends the headers
	b = append(b, "\n$$B\nu"...)
	b = append(b, payload...)
	b = append(b, sectionEnd...)
	return b, nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
