// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"strings"

	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The stenciler of specs/013-generics.md.
//
// One compiled body per distinct list of type arguments. The generic
// declaration itself produces no code, because it has none: buildFunc skips it
// and gc emits nothing for one either. Every instantiation the checker
// resolved becomes an ordinary monomorphic function of this package, so that
// nothing below specs/020-ir.md sees a type parameter at all.
//
// # How a body is instantiated
//
// The builder already walks a syntax tree and asks the checker for the type of
// every expression and the object of every name. A generic body is the same
// walk with one difference: the checker's answers hold type parameters, and
// the answer this pass wants is that answer with the type arguments in place
// of them. So the substitution is applied at the two doors every type comes
// through, irType and typeOf, plus typeNode which names a type rather than
// producing a value. No case of the walk is duplicated and none of them knows
// it is building an instantiation.
//
// An object declared inside the body needs one IR object per instantiation,
// because a local of type T is an int in one instantiation and a string in
// another. Objects are therefore keyed by the instantiation as well as by the
// checker object, and a package-level object keeps the one identity it had,
// which objKey states.
//
// Substitution is not total, and there is one place it stops: a name. The
// checker's substituter rebuilds a type literal and returns a defined type
// with no type parameters of its own unchanged, so "type S []T" declared
// inside the body still holds T afterwards. That is the whole of what
// checkLocalType refuses and it is the only construct that can reach the IR
// still holding a type parameter.
//
// # The worklist
//
// A call to a generic function is discovered where the callee's name is
// resolved, which is the one place both spellings of a call reach: the
// explicit F[int] and the inferred F(1) both put the type arguments on the
// same *syntax.Name, in Info.Instances. Discovery appends to a slice in walk
// order and never ranges over the map (specs/053-determinism.md), and the
// worklist is drained after the declared functions are built, so an
// instantiation found inside an instantiated body is built too.
//
// The set is finite by the language definition. types2's mono check rejects a
// package that cannot be statically instantiated, so the drain terminates
// without a depth bound of its own (specs/013-generics.md, decision 1).

// instance is one instantiation to stencil: one generic declaration and one
// list of type arguments.
type instance struct {
	// sym is the linker symbol, and is the identity of the instantiation. Two
	// call sites with identical type arguments produce one sym and so one
	// body.
	sym string

	// origin is the generic declaration and decl is its source. decl is what
	// makes this pass possible and what bounds it: a generic declared in
	// another package has no syntax tree here, and instanceOf refuses it by
	// name rather than emitting a call to a symbol nothing defines.
	origin *types2.Func
	decl   *syntax.FuncDecl

	// targs are the type arguments, already substituted through the enclosing
	// instantiation if this one was discovered inside another.
	targs []types2.Type

	// sig is the instantiated signature: the origin's signature with the type
	// arguments in place of its type parameters, and no type parameters of its
	// own.
	sig *types2.Signature

	// recv is the instantiated receiver of a method of a generic type, and is
	// nil for an instantiation of a generic function.
	//
	// It is what says which type parameters the substitution replaces. A
	// generic function binds them on its signature and a method of a generic
	// type binds them on its receiver, and the two lists are different
	// *TypeParam values even when they are spelled alike: the checker gives a
	// method its own copy, and the body's recorded types name that copy.
	recv *types2.Named

	// obj is the one IR object every call site of this instantiation names.
	obj *Object
}

// instanceSym is the linker symbol of one instantiation.
//
// The name is the generic's symbol with the type arguments after it, each
// written with its import path, which is what specs/013-generics.md chose:
//
//	golang.design/x/nanogo/ssa.Map[int,*golang.design/x/nanogo/ir.Node]
//
// It is canonical and it does not name the package that triggered the
// instantiation, so two packages that both instantiate slices.Sort[int] name
// one symbol and the linker keeps one copy.
//
// # Why this cannot collide with a symbol gc wrote
//
// Both compilers put functions in one binary, so the question is not whether
// this spelling is reasonable but whether gc ever writes it. It does not. gc
// stencils by GC shape, and for
//
//	func pick[T any](a, b T, f bool) T
//
// instantiated at int, at string and at two distinct one-pointer structs, the
// symbols in the linked binary are
//
//	main.pick[go.shape.int]
//	main.pick[go.shape.string]
//	main.pick[go.shape.struct { main.p *int }]
//	main..dict.pick[int]
//	main..dict.pick[string]
//	main..dict.pick[main.S]
//
// A body of gc's carries go.shape. inside the brackets and a dictionary of
// gc's carries ..dict. before the name. nanogo's full stencil carries neither,
// so main.pick[int] is a name gc has no way to produce. That is the fact this
// scheme rests on, and it was read out of go tool nm rather than out of a
// source file.
//
// The type argument is spelled by types2.TypeString with no qualifier, which
// writes the import path of every defined type in it. gc spells its dictionary
// the same way, through types.LinkString, which is why the two lists above
// agree on int, string and main.S.
func instanceSym(origin *types2.Func, targs []types2.Type) string {
	var b strings.Builder
	b.WriteString(funcSym(origin))
	writeTypeArgs(&b, targs)
	return b.String()
}

// instanceName is what a diagnostic and a traceback call the instantiation.
//
// It is the source name with the type arguments after it, and it is not the
// symbol: the symbol qualifies the name by the import path and this does not,
// exactly as Func.Name and Func.Sym differ for an ordinary declaration. A
// method of a generic type carries the arguments on the receiver, where the
// language writes them and where its symbol carries them.
func instanceName(in *instance) string {
	var b strings.Builder
	if in.recv != nil {
		b.WriteString(in.recv.Obj().Name())
		writeTypeArgs(&b, in.targs)
		b.WriteByte('.')
		b.WriteString(in.origin.Name())
		return b.String()
	}
	b.WriteString(in.origin.Name())
	writeTypeArgs(&b, in.targs)
	return b.String()
}

// writeTypeArgs writes a bracketed type argument list, which is the spelling
// both names use.
func writeTypeArgs(b *strings.Builder, targs []types2.Type) {
	b.WriteByte('[')
	for i, a := range targs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(types2.TypeString(a, nil))
	}
	b.WriteByte(']')
}

// collectGenericDecls records the source of every generic function this
// package declares, so that an instantiation can be built from it.
//
// A method with type parameters of its own, and a method of a generic type,
// are both recorded: instanceOf refuses them by name, and a refusal that says
// which declaration it is about needs the declaration.
func (b *builder) collectGenericDecls(files []*syntax.File) {
	for _, f := range files {
		for _, d := range f.DeclList {
			fd, ok := d.(*syntax.FuncDecl)
			if !ok {
				continue
			}
			obj, _ := b.info.Defs[fd.Name].(*types2.Func)
			if obj == nil {
				continue
			}
			sig, _ := obj.Type().(*types2.Signature)
			if sig == nil {
				continue
			}
			if sig.TypeParams().Len() == 0 && sig.RecvTypeParams().Len() == 0 {
				continue
			}
			b.generic[obj] = fd
		}
	}
}

// instanceOf returns the object a use of an instantiated generic function
// names, and queues the instantiation to be built.
//
// x is the identifier the checker recorded the instantiation on. It is the
// callee's name in both spellings of a call: F[int] puts it on the F inside
// the index expression and F(1) puts it on the F itself, so this one place
// covers the explicit instantiation and the inferred one.
//
// nil is returned when the instantiation cannot be built, and the reason is
// recorded as an error. The caller then goes on to the generic declaration's
// own object, whose type holds a type parameter, so the build fails at the
// type boundary with the two messages rather than emitting a call to a symbol
// nothing defines.
func (b *builder) instanceOf(x *syntax.Name, origin *types2.Func) *Object {
	inst, ok := b.info.Instances[x]
	if !ok || inst.TypeArgs.Len() == 0 {
		return nil
	}

	// Substituted through the enclosing instantiation first. Inside the body
	// of f[T] a call to g[T] has T as its type argument, and the instantiation
	// to build is g at whatever T is here.
	targs := make([]types2.Type, inst.TypeArgs.Len())
	for i := range targs {
		// Canonical, because the symbol is the identity of the instantiation
		// and an alias is not a type: id[Alias] and id[int] are one
		// instantiation, and two symbols for it would put two copies of one
		// body in the program.
		targs[i] = types2.Canonical(b.ctxt, b.subst(inst.TypeArgs.At(i)))
	}

	if tparams := types2.TypeParamsOf(origin.Type()); len(tparams) != len(targs) {
		// Instantiate panics on a count mismatch rather than returning an
		// error, and so does NewSubstitution. The checker cannot record one,
		// so this is a guard against a broken Info and not against a program:
		// a compiler that crashes has replaced a diagnostic with a stack
		// trace.
		b.errorf("ir: %s has %d type parameters and an instantiation of it names %d",
			funcSym(origin), len(tparams), len(targs))
		return nil
	}

	sym := instanceSym(origin, targs)
	if have, ok := b.instances[sym]; ok {
		return have.obj
	}

	// The signature is instantiated from the same list the symbol was spelled
	// from and the body will be substituted with, so the type the function
	// carries and the types inside it cannot come from two lists.
	inSig, err := types2.Instantiate(b.ctxt, origin.Type(), targs, false)
	sig, _ := inSig.(*types2.Signature)
	if err != nil || sig == nil {
		b.errorf("ir: the instantiation %s has no signature", sym)
		return nil
	}

	decl := b.generic[origin]
	switch {
	case origin.Pkg() != b.tpkg:
		// The body lives in the archive of the package that declared it.
		// specs/015-export-data.md reads one and specs/020-ir.md has no entry
		// point that takes one, so there is nothing to substitute through
		// here yet.
		b.errorf("ir: %s is declared in package %s and an instantiation of another package's generic function is not built",
			sym, origin.Pkg().Path())
		return nil
	case decl == nil:
		b.errorf("ir: %s has no declaration in this package", sym)
		return nil
	case decl.Body == nil:
		b.errorf("ir: %s has no body", sym)
		return nil
	case origin.Signature().RecvTypeParams().Len() > 0 || origin.Signature().Recv() != nil:
		// A method's dictionary holds the receiver's type parameters ahead of
		// its own, so the key of the instantiation is not the list on this
		// name alone. specs/013-generics.md leaves that question open.
		b.errorf("ir: %s is a method and an instantiation of a generic method is not built", sym)
		return nil
	}

	in := &instance{sym: sym, origin: origin, decl: decl, targs: targs, sig: sig}
	in.obj = &Object{
		Name:  sym,
		Class: ClassFunc,
		Type:  b.irType(sig),
		Pos:   decl.Pos(),
	}
	b.instances[sym] = in
	b.todo = append(b.todo, in)
	return in.obj
}

// stencil is the instantiation being built, and is nil while an ordinary
// declaration is.
//
// It is saved and restored around a body exactly as the function and the
// signature are, because a function literal inside an instantiated body is
// built by the same walk and is part of the same instantiation.
type stencil struct {
	sym   string
	subst *types2.Substitution
}

// subst returns t with the type arguments of the instantiation being built in
// place of its type parameters.
//
// It is the identity outside an instantiation, which is every ordinary
// declaration, so the cost on a package with no generics in it is one nil
// test per type.
func (b *builder) subst(t types2.Type) types2.Type {
	if b.stencil == nil {
		return t
	}
	return b.stencil.subst.Type(t)
}

// drainInstances builds every queued instantiation, and every instantiation
// those bodies discover.
//
// The queue is a slice in discovery order, so the output is the same on every
// build (specs/053-determinism.md). It terminates because the set of
// instantiations of a package the checker accepted is finite: types2's mono
// check rejects a package whose instantiation graph has a cycle that grows a
// type, and it runs before this pass does.
func (b *builder) drainInstances() {
	// Two queues, because the two feed each other: a type's method is built
	// from the notice the converter sent, and building it converts the types
	// that body names, which is where the next instantiation is found. Both
	// are slices in discovery order, so the output is the same on every build.
	for i, j := 0, 0; i < len(b.types) || j < len(b.todo); {
		for ; i < len(b.types); i++ {
			b.instantiateType(b.types[i])
		}
		for ; j < len(b.todo); j++ {
			if fn := b.buildInstance(b.todo[j]); fn != nil {
				b.out.Funcs = append(b.out.Funcs, fn)
			}
		}
	}
	b.todo, b.types = nil, nil
}

// instantiateType queues every method of one instantiation of a generic type
// this package declares.
//
// The type and not the call site is the unit, because a method of an
// instantiation is reached by more than a call. An itab holds it, a descriptor
// names it in the Method array reflect indexes, and neither is a call site the
// builder walks. So the method set of the instantiation is built whole,
// exactly as gc builds it: gc stencils the shape body and emits one wrapper
// per method per instantiation, both dupok, in the package that instantiates.
//
// An instantiation of a generic type another package declares is left alone.
// Its bodies are in that package's archive, specs/015-export-data.md reads one
// and specs/020-ir.md has no entry point that takes one, so there is no tree
// here to substitute through. The type itself still converts and still gets a
// descriptor, which is right for a generic type with no methods: iter.Seq[int]
// needs no body at all.
func (b *builder) instantiateType(named *types2.Named) {
	n := named.TypeArgs()
	if n.Len() == 0 {
		return
	}
	obj := named.Origin().Obj()
	if obj == nil || obj.Pkg() != b.tpkg {
		return
	}
	targs := make([]types2.Type, n.Len())
	for i := range targs {
		targs[i] = n.At(i)
	}
	for i := 0; i < named.NumMethods(); i++ {
		b.methodInstance(named, named.Method(i), targs)
	}
}

// methodInstance queues one method of one instantiation, and returns the
// object every use of it names.
//
// m is the instantiated method, which the checker produced by substituting the
// receiver's type arguments through the declared one. Its origin is the
// declaration, and the declaration is what has a body here.
func (b *builder) methodInstance(named *types2.Named, m *types2.Func, targs []types2.Type) *Object {
	origin := m.Origin()
	sig, _ := m.Type().(*types2.Signature)
	osig, _ := origin.Type().(*types2.Signature)
	if sig == nil || osig == nil || osig.Recv() == nil {
		b.errorf("ir: %s is not a method of %s", m.Name(), types2.TypeString(named, nil))
		return nil
	}
	_, ptr := unalias(osig.Recv().Type()).(*types2.Pointer)
	sym, err := MethodSymbol(b.irType(named), Method{Name: m.Name()}, ptr)
	if err != nil {
		b.errorf("ir: the method %s of %s: %v", m.Name(), types2.TypeString(named, nil), err)
		return nil
	}
	if have, ok := b.instances[sym]; ok {
		return have.obj
	}

	decl := b.generic[origin]
	switch {
	case osig.RecvTypeParams().Len() != len(targs):
		// NewSubstitution panics on a count mismatch rather than returning an
		// error. The checker cannot record one, so this is a guard against a
		// broken Info and not against a program: a compiler that crashes has
		// replaced a diagnostic with a stack trace.
		b.errorf("ir: %s has %d receiver type parameters and the instantiation names %d",
			sym, osig.RecvTypeParams().Len(), len(targs))
		return nil
	case osig.TypeParams().Len() > 0:
		// A method with type parameters of its own. Instantiating the receiver
		// does not instantiate the method, so the key of the instantiation is
		// not this type argument list. specs/013-generics.md leaves that
		// question open.
		b.errorf("ir: %s is a generic method and an instantiation of one is not built", sym)
		return nil
	case decl == nil:
		b.errorf("ir: %s has no declaration in this package", sym)
		return nil
	case decl.Body == nil:
		b.errorf("ir: %s has no body", sym)
		return nil
	}

	in := &instance{sym: sym, origin: origin, decl: decl, targs: targs, sig: sig, recv: named}
	in.obj = &Object{
		Name:  sym,
		Class: ClassFunc,
		Type:  b.irType(sig),
		Pos:   decl.Pos(),
	}
	b.instances[sym] = in
	b.todo = append(b.todo, in)
	return in.obj
}

// instanceTypeParams returns the type parameters the instantiation replaces.
//
// A generic function binds them on its signature and a method of a generic
// type binds them on its receiver. The receiver's list is the method's own
// copy, which is the list the body's recorded types name, so it is read off
// the declared signature rather than off the type the method belongs to.
func instanceTypeParams(in *instance, origin *types2.Signature) []*types2.TypeParam {
	if in.recv == nil {
		return types2.TypeParamsOf(origin)
	}
	list := origin.RecvTypeParams()
	out := make([]*types2.TypeParam, list.Len())
	for i := range out {
		out[i] = list.At(i)
	}
	return out
}

// buildInstance builds one instantiation as an ordinary function.
//
// The parameters and results are declared from the origin's own variables and
// not from the instantiated signature's, because the body refers to the
// origin's: the checker records the origin's *types2.Var in Info.Uses for
// every mention of a parameter, and the instantiated signature holds copies of
// them that no mention names. The type each one gets is still the concrete
// one, because irType substitutes.
//
// The signature on the builder is the instantiated one, so that a return
// statement converts to the concrete result type rather than to a type
// parameter.
func (b *builder) buildInstance(in *instance) *Func {
	origin, _ := in.origin.Type().(*types2.Signature)
	if origin == nil {
		b.errorf("ir: %s is not a function", in.sym)
		return nil
	}

	fn := &Func{
		Name:   instanceName(in),
		Sym:    in.sym,
		Pos:    in.decl.Pos(),
		Pragma: in.decl.Pragma,
	}

	saveFn, saveSig, saveSinks, saveFree := b.fn, b.sig, b.sinks, b.free
	saveStencil := b.stencil
	b.fn, b.sig, b.sinks, b.free = fn, in.sig, nil, nil
	b.stencil = &stencil{
		sym:   in.sym,
		subst: types2.NewSubstitution(b.ctxt, instanceTypeParams(in, origin), in.targs),
	}

	fn.Type = b.irType(in.sig)
	if recv := origin.Recv(); recv != nil {
		fn.Recv = b.declare(recv, fn)
	}
	for i := 0; i < origin.Params().Len(); i++ {
		fn.Params = append(fn.Params, b.declare(origin.Params().At(i), fn))
	}
	for i := 0; i < origin.Results().Len(); i++ {
		fn.Results = append(fn.Results, b.declare(origin.Results().At(i), fn))
	}

	b.free = make(map[*Object]bool)
	fn.Body = b.block(in.decl.Body.List)

	b.fn, b.sig, b.sinks, b.free = saveFn, saveSig, saveSinks, saveFree
	b.stencil = saveStencil
	return fn
}

// objKey is the identity of an IR object.
//
// A local, a parameter and a result of an instantiated body are one object per
// instantiation, because a local of type T holds an int in one instantiation
// and a string in another, and one IR object cannot have two types. Everything
// declared at package scope keeps the one identity it has: a package-level
// variable is one symbol in the program whoever reads it, and giving a
// generic body its own copy would make two objects naming one symbol.
type objKey struct {
	// stencil is the symbol of the instantiation the object belongs to, and is
	// empty for an object that belongs to the package rather than to one body.
	stencil string
	obj     types2.Object
}

// key returns the identity o has in the build now running.
//
// Parent is not the test on its own. An unnamed result is in no scope at all,
// and it is still one object per instantiation: work[int] returns []int and
// work[string] returns []string, and one IR object cannot be both. So the kind
// of the object decides, and Parent settles only where a name may be declared
// at package scope as well as inside a function.
func (b *builder) key(o types2.Object) objKey {
	if b.stencil == nil {
		return objKey{obj: o}
	}
	switch o.(type) {
	case *types2.Var:
		// A receiver, a parameter, a result and a local belong to one body.
		// A package-level variable is one symbol in the program whoever reads
		// it, and giving a generic body a copy would make two objects naming
		// one symbol.
		if v, _ := o.(*types2.Var); v.Kind() == types2.PackageVar {
			return objKey{obj: o}
		}
	case *types2.Const, *types2.TypeName:
		// A type declared inside a generic body is a different type in each
		// instantiation. One declared at package scope is not.
		if p := o.Parent(); p == nil || p == b.tpkg.Scope() || p == types2.Universe {
			return objKey{obj: o}
		}
	default:
		// A function, a method, nil, a builtin and a package name. None of
		// them is declared inside a body.
		return objKey{obj: o}
	}
	return objKey{stencil: b.stencil.sym, obj: o}
}

// A resolvedSelection is a selection read against the type its receiver has in
// the build now running.
//
// The checker recorded every selection against the receiver type as it is
// written in the source, and inside a generic body that type may be a type
// parameter. x.M() on a value of type parameter T constrained by an interface
// resolves to the *constraint's* method, whose symbol is the interface's and
// which nothing defines. The stencil's receiver is concrete, so the selection
// is looked up again against the concrete type and the call reaches the
// method the program actually runs.
type resolvedSelection struct {
	// obj is the field or the method, and index is the path to it: the
	// embedded fields to walk through, and for a method its position in the
	// method set of the type it was found in.
	obj   types2.Object
	index []int
}

// methodIndex is the position of a method in the method set of the interface
// it is selected from, which is what an itab is indexed by.
func (rs resolvedSelection) methodIndex() int {
	if len(rs.index) == 0 {
		return 0
	}
	return rs.index[len(rs.index)-1]
}

// resolveSelection returns the selection x names, looked up again when the
// stenciler changed the receiver's type.
//
// Outside an instantiation, and inside one whose receiver type holds no type
// parameter, this is the checker's own answer unchanged. The lookup is the
// checker's own too, so nothing here re-derives the language's rule for
// finding a method through embedded fields.
func (b *builder) resolveSelection(x *syntax.SelectorExpr, sel *types2.Selection) resolvedSelection {
	have := resolvedSelection{obj: sel.Obj(), index: sel.Index()}
	if fn, ok := have.obj.(*types2.Func); ok {
		// Here rather than in obj, because the caller reads the method's
		// signature before it asks for the object, and a signature holding a
		// type parameter fails at the type boundary first. The refusal that
		// names the declaration has to be the one recorded first.
		b.checkMethodIsBuilt(fn)
	}
	if b.stencil == nil {
		return have
	}
	recv := sel.Recv()
	sub := b.subst(recv)
	if sub == recv {
		return have
	}
	obj, index, _ := types2.LookupFieldOrMethod(sub, true, b.tpkg, x.Sel.Value)
	if obj == nil {
		b.errorf("ir: %s has no field or method %s after %s was substituted",
			types2.TypeString(sub, nil), x.Sel.Value, types2.TypeString(recv, nil))
		return have
	}
	return resolvedSelection{obj: obj, index: index}
}

// checkMethodIsBuilt reports a method this pass produces no body for.
//
// Two of them, and each is a symbol nothing defines rather than a wrong
// answer, so the diagnostic here is what turns a link failure with no source
// position into a message that names the declaration.
//
// A method with type parameters of its own is the third place the language
// binds one, beside a function and a type. Instantiating the receiver does not
// instantiate the method, so the key of the instantiation is not the list on
// the selector alone, and specs/013-generics.md leaves that question open.
//
// A method of an instantiation of a generic type another package declares has
// its body in that package's archive. specs/015-export-data.md reads one and
// specs/020-ir.md has no entry point that takes one, so there is no tree here
// to substitute through. gc has the same obligation and discharges it from the
// export data: it emits the method of every instantiation, dupok, in the
// package that instantiates.
//
// A method of an instantiation this package declares is built, by
// instantiateType, and is not refused here. A generic type used for its fields
// alone is not refused either, and is correct: the field types are substituted
// by the checker and the IR type is an ordinary struct.
func (b *builder) checkMethodIsBuilt(fn *types2.Func) {
	sig, _ := fn.Type().(*types2.Signature)
	if sig == nil || sig.Recv() == nil {
		return
	}
	if sig.TypeParams().Len() > 0 {
		b.errorf("ir: %s is a generic method and an instantiation of one is not built", funcSym(fn))
		return
	}
	t := sig.Recv().Type()
	if p, ok := types2.Unalias(t).(*types2.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types2.Unalias(t).(*types2.Named)
	if !ok || named.TypeArgs().Len() == 0 {
		return
	}
	if obj := named.Origin().Obj(); obj != nil && obj.Pkg() == b.tpkg {
		return
	}
	b.errorf("ir: %s is a method of %s and an instantiation of a generic type another package declares is not built",
		funcSym(fn), types2.TypeString(named, nil))
}

// checkLocalType reports a type declared inside a generic body that the
// substitution does not reach.
//
// Substitution rebuilds a type literal and stops at a name. A defined type
// with no type parameters of its own is returned unchanged, and
//
//	func f[T any]() { type S []T; ... }
//
// declares exactly that, so S still holds T after the substitution and every
// instantiation of f would share one S. The IR type boundary sees the type
// parameter and refuses, which is the honest failure, and this says which
// declaration it is about.
//
// Instantiating S is instantiating a generic type, which this pass does not
// do. The export writer refuses the same declaration for the same reason: every
// use of S carries the enclosing type parameters implicitly
// (specs/013-generics.md).
func (b *builder) checkLocalType(d *syntax.TypeDecl) {
	if b.stencil == nil || d.Name == nil {
		return
	}
	o := b.info.Defs[d.Name]
	if o == nil || o.Type() == nil {
		return
	}
	// The underlying type, because a defined type is its name: the question is
	// what the declaration is made of and not what it is called.
	if !b.stencil.subst.Substitutes(o.Type().Underlying()) {
		return
	}
	b.errorf("ir: %s declares the type %s, which holds a type parameter, and a type declared inside a generic function is not instantiated",
		b.stencil.sym, d.Name.Value)
}
