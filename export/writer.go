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
	var funcs []InlineFunc
	var fset *syntax.FileSet
	var file func(string) string
	if src != nil {
		fset, file = src.Fset, src.File
		funcs = writableBodies(pkg, fset, file, src.Funcs)
	}
	pw := newPkgWriter(pkg, funcs, fset, file)

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

	// lits holds the element claimed for each function literal a body
	// named, which is filled in after the body that named it.
	lits map[*FuncLitExpr]*pkgbits.Encoder

	// fileName maps a parsed file name to the one a position base records.
	fileName func(string) string

	// fset resolves a declaration's position. It is nil for a package
	// written without its source, and every position is then absent.
	fset *syntax.FileSet

	refs *declRefs
}

// newPkgWriter returns a writer for one package.
func newPkgWriter(pkg *types2.Package, funcs []InlineFunc, fset *syntax.FileSet, file func(string) string) *pkgWriter {
	pw := &pkgWriter{
		PkgEncoder:  pkgbits.NewPkgEncoder(Version),
		curpkg:      pkg,
		pkgsIdx:     make(map[*types2.Package]pkgbits.Index),
		typsIdx:     make(map[types2.Type]pkgbits.Index),
		objsIdx:     make(map[types2.Object]pkgbits.Index),
		posBasesIdx: make(map[string]pkgbits.Index),
		inline:      make(map[types2.Object]*InlineFunc, len(funcs)),
		lits:        make(map[*FuncLitExpr]*pkgbits.Encoder),
		fileName:    file,
		fset:        fset,
	}
	pw.refs = &declRefs{pw: pw}
	for i := range funcs {
		pw.inline[funcs[i].Obj] = &funcs[i]
	}
	return pw
}

// writer writes one element.
type writer struct {
	*pkgbits.Encoder
	p *pkgWriter
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
	w.Sync(pkgbits.SyncType)
	w.Bool(false) // not a derived type: the writer refuses type parameters
	w.Reloc(pkgbits.SectionType, w.p.typIdx(typ))
}

// typIdx returns the index of typ, writing its element if it is new.
//
// Types are interned in first-use order, and a type that refers to itself is
// expressed by the index and not by recursion: the index is claimed before
// the element is written, so a struct with a pointer to itself terminates.
func (pw *pkgWriter) typIdx(typ types2.Type) pkgbits.Index {
	// A local alias only appears inside a function body, which this writer
	// never encodes, but a package-scope alias to a local one can still be
	// reached. Upstream strips the same way, to keep two local aliases from
	// colliding on one symbol.
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
		pw.refuse(objName(typ.Obj()), genericReason)

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

	return w.Flush()
}

// genericReason is the one refusal this writer makes on purpose.
const genericReason = "the declaration is generic, and stenciling it in an importing package needs the " +
	"function bodies specs/015-export-data.md's body reader would carry; nanogo writes declarations only"

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

	if tparams := objTypeParams(obj); len(tparams) != 0 {
		pw.refuse(objName(obj), genericReason)
	}
	if f, ok := obj.(*types2.Func); ok && f.Signature().RecvTypeParams().Len() != 0 {
		pw.refuse(objName(obj), genericReason)
	}

	w := pw.newWriter(pkgbits.SectionObj, pkgbits.SyncObject1)
	wext := pw.newWriter(pkgbits.SectionObjExt, pkgbits.SyncObject1)
	wname := pw.newWriter(pkgbits.SectionName, pkgbits.SyncObject1)
	wdict := pw.newWriter(pkgbits.SectionObjDict, pkgbits.SyncObject1)

	pw.objsIdx[obj] = w.Idx // claim the index, so a cycle terminates
	if wext.Idx != w.Idx || wname.Idx != w.Idx || wdict.Idx != w.Idx {
		panic(fmt.Errorf("export: the four elements of %v got different indices", objName(obj)))
	}

	code := w.doObj(wext, obj)
	w.Flush()
	wext.Flush()

	wname.qualifiedIdent(obj)
	wname.Code(code)
	wname.Flush()

	wdict.objDict()
	wdict.Flush()

	return w.Idx
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
		w.typeParamNames()
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
			w.typeParamNames()
			w.typ(rhs)
			return pkgbits.ObjAlias
		}

		named := obj.Type().(*types2.Named)
		w.pos(obj.Pos())
		w.typeParamNames()
		wext.typeExt()
		w.typ(named.Underlying())

		// The methods, in the order the checker holds them, which is the
		// order they were declared in. That is gc's order too, so the two
		// compilers' export data for one package stays comparable.
		w.Len(named.NumMethods())
		for i := 0; i < named.NumMethods(); i++ {
			w.method(wext, named.Method(i))
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
// Every count is zero, because the writer refuses a generic declaration: no
// implicit type parameter, no receiver or explicit type parameter, no derived
// type, and none of the four runtime dictionary lists.
func (w *writer) objDict() {
	w.Len(0) // implicit type parameters
	if w.Version().Has(pkgbits.GenericMethods) {
		w.Len(0) // receiver type parameters
	}
	w.Len(0) // explicit type parameters
	w.Len(0) // derived types
	w.Len(0) // type parameter method expressions
	w.Len(0) // subdictionaries
	w.Len(0) // runtime types
	w.Len(0) // itabs
}

// typeParamNames writes an empty type parameter list.
func (w *writer) typeParamNames() {
	w.Sync(pkgbits.SyncTypeParamNames)
}

func (w *writer) method(wext *writer, meth *types2.Func) {
	sig := meth.Signature()
	if sig.TypeParams().Len() != 0 || sig.RecvTypeParams().Len() != 0 {
		w.p.refuse(objName(meth), genericReason)
	}

	w.Sync(pkgbits.SyncMethod)
	w.pos(meth.Pos())
	w.selector(meth)
	w.typeParamNames()
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
