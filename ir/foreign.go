// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The second front half: an instantiation built from a body read out of an
// archive rather than from a syntax tree.
//
// stencil.go instantiates a generic this package declares, and it does that by
// walking the declaration's syntax tree with the checker's answers substituted.
// A generic another package declared has no syntax tree here and no entry in
// types2.Info, so that walk has nothing to run over. What it has instead is
// export/body.go's tree, which specs/015-export-data.md decodes out of the
// declaring package's archive, and this file is the walk over that tree.
// specs/020-ir.md asked for exactly this: a second front half for one back
// half, not a second ir.
//
// gc has the same obligation and discharges it the same way. It reads the body
// out of the export data and emits one stencil, and the method of every
// instantiation, in the package that instantiates.
//
// # What is different from the syntax walk
//
// Three things, and they are the whole of why this is a second walk rather
// than a parameter on the first.
//
//   - A local has no object and no name. gc numbers the locals of a body in
//     declaration order and refers to one by its number, so the frame below
//     holds the objects in that numbering and nothing looks a local up by
//     name. The receiver, the parameters and the results are the first of
//     them, in that order.
//   - A type comes out of the tree and not out of types2.Info. Every
//     expression the format types carries its type, and the type is written in
//     terms of the type parameters *this decode* created. Those are not the
//     objects the checker made for the same declaration, which is why the
//     substitution below is built from the body's own dictionary and not from
//     the checker's list.
//   - A call whose type arguments depend on the enclosing declaration's
//     carries a dictionary slot and no reference to the callee at all. The
//     dictionary the archive holds is what names it, which is what
//     [export.Body.Dict] carries.
//
// # What is refused
//
// The node set of the format is about thirty statement and expression kinds,
// and the ones below are mapped. Every other kind is refused by name, with the
// declaration it was met in. That is the point of the split: a body of another
// package's is code no one here wrote and no one here can read in the diff, so
// a node this walk guessed at would be a wrong answer inside somebody else's
// function. A refusal is a build that stops with a message naming the
// construct, which is the failure specs/013-generics.md asks for.

// A BodySource supplies the body of a generic declaration another package
// declares.
//
// The path is the declaring package's import path and the name is gc's linker
// symbol name, which is what the export data names a body by: "Contains" for a
// function and "(*Pointer).Store" for a method. A declaration the source
// cannot find is (nil, nil), which the caller reports by name.
//
// [export.Reader] implements it, and it has to be that Reader rather than a
// second one over the same archive: the body's declarations and types have to
// resolve to the objects the type checker already holds, and a second reader
// would produce a parallel object graph in which a call names a function this
// compilation has no object for.
type BodySource interface {
	Body(path, name string) (*export.FuncBody, error)
}

// ForeignBodies is where [Build] reads the body of a generic another package
// declares.
//
// It is set once per process, by the driver, when it builds the reader the
// type checker imports through. [Build] takes a package, its files and the
// checker's Info, which is specs/020-ir.md's door and what every caller passes;
// the source of a foreign body is a property of the compilation rather than of
// the package, and it is the same value for every Build one process runs. A
// nil source refuses every foreign instantiation by name, which is what a test
// that sets none gets.
var ForeignBodies BodySource

// foreignFrame is the body of one foreign instantiation being built.
//
// It is saved and restored around a body exactly as the function and the
// stencil are, because a body reached from inside another one is built by the
// same walk.
type foreignFrame struct {
	in   *instance
	body *export.Body
	dict *export.Dict

	// locals is the body's own numbering: the receiver, the parameters and
	// the results, then one entry for each variable a statement declares, in
	// the order the walk reaches them. That order is the order gc numbered
	// them in, because it is the order gc's writer wrote them in.
	locals []foreignLocal
}

// foreignLocal is one local of a foreign body.
//
// The checker type is kept beside the object because the walk needs one where
// the IR needs the other: an implicit conversion, an index and a field
// selection are all decided from the checker's type, and a use of a local
// carries no type in the format at all.
type foreignLocal struct {
	obj *Object
	typ types2.Type
}

// foreignBody returns the body of one declaration of another package.
func (b *builder) foreignBody(path, name string) (*export.Body, error) {
	if ForeignBodies == nil {
		return nil, fmt.Errorf("no source of foreign bodies was set")
	}
	fb, err := ForeignBodies.Body(path, name)
	if err != nil {
		return nil, err
	}
	if fb == nil || fb.Body == nil {
		return nil, fmt.Errorf("the archive holds no body for it")
	}
	if !fb.Body.HasBlock {
		return nil, fmt.Errorf("the declaration has no body, so it is implemented in assembly or by a linkname")
	}
	if fb.Body.Dict == nil {
		return nil, fmt.Errorf("the body carries no dictionary, and its slots cannot be resolved without one")
	}
	if fb.Pragma != 0 {
		// A //go: directive on the declaration. Several of them are
		// correctness requirements rather than hints
		// (specs/016-directives-and-pragmas.md), and the format carries gc's
		// own bit set, which is not this compiler's numbering. Translating it
		// is work no declaration here has needed yet, so a declaration that
		// carries one is refused rather than built without it.
		return nil, fmt.Errorf("the declaration carries a //go: directive, which is gc's pragma bit set %#x and is not translated", fb.Pragma)
	}
	return fb.Body, nil
}

// foreignDeclName is gc's linker symbol name for a generic declaration, which
// is the name its body is listed under.
//
// A method is named by the type it is declared on and not by an instantiation
// of it, because the body belongs to the declaration.
func foreignDeclName(origin *types2.Func) string {
	sig := origin.Signature()
	if sig == nil || sig.Recv() == nil {
		return origin.Name()
	}
	t := sig.Recv().Type()
	ptr := false
	if p, ok := types2.Unalias(t).(*types2.Pointer); ok {
		t, ptr = p.Elem(), true
	}
	named, ok := types2.Unalias(t).(*types2.Named)
	if !ok || named.Obj() == nil {
		return origin.Name()
	}
	if ptr {
		return "(*" + named.Obj().Name() + ")." + origin.Name()
	}
	return named.Obj().Name() + "." + origin.Name()
}

// foreignTypeParams returns the type parameters a foreign body's types are
// written in terms of.
//
// It is the dictionary's own numbering, which [export.Dict.TypeParamIndex]
// states: the enclosing declaration's type parameters, then the receiver's,
// then the declaration's own. A method of a generic type shares its type's
// dictionary, so the receiver's parameters are that type's and they are in the
// third list rather than the second.
func foreignTypeParams(d *export.Dict) []*types2.TypeParam {
	out := make([]*types2.TypeParam, 0, len(d.Implicits)+len(d.Receivers)+len(d.TypeParams))
	out = append(out, d.Implicits...)
	out = append(out, d.Receivers...)
	out = append(out, d.TypeParams...)
	return out
}

// foreignInstance queues one instantiation of a generic another package
// declares, and returns the object every use of it names.
//
// sym is the symbol the instantiation is known by, which is the caller's
// answer and not this one's: a function and a method spell it differently and
// both spellings are decided where the instantiation is discovered.
func (b *builder) foreignInstance(sym string, origin *types2.Func, targs []types2.Type, sig *types2.Signature, recv *types2.Named) *Object {
	path := origin.Pkg().Path()
	name := foreignDeclName(origin)
	body, err := b.foreignBody(path, name)
	if err != nil {
		b.errorf("ir: %s is declared in package %s and its body cannot be read out of an archive: %v", sym, path, err)
		return nil
	}
	if tparams := foreignTypeParams(body.Dict); len(tparams) != len(targs) {
		b.errorf("ir: %s is declared in package %s, whose body is written against %d type parameter(s) and the instantiation names %d",
			sym, path, len(tparams), len(targs))
		return nil
	}

	in := &instance{sym: sym, origin: origin, targs: targs, sig: sig, recv: recv, body: body}
	in.obj = &Object{Name: sym, Class: ClassFunc, Type: b.irType(sig)}
	b.instances[sym] = in
	b.todo = append(b.todo, in)
	return in.obj
}

// buildForeignInstance builds one instantiation whose body came out of an
// archive.
//
// It is [builder.buildInstance] over the other tree. The parameters are
// declared from the instantiated signature and not from the origin's
// variables, which is the opposite of the syntax walk and for the same reason:
// there, the body names the origin's variables, and here the body names no
// variable at all and refers to a local by its number.
//
// Every node of the body takes the unknown position. The tree carries the file,
// the line and the column the declaring package was compiled with, and nanogo
// has no coordinate space for a file of another package
// (specs/010-scanner-and-positions.md): a [syntax.Pos] here indexes this
// compilation's file set, so any value taken from the tree would name a line of
// one of this package's own files. The line table of the instantiation is
// therefore empty rather than wrong, and giving it a real one is what
// specs/010-scanner-and-positions.md would have to answer first.
func (b *builder) buildForeignInstance(in *instance) *Func {
	fn := &Func{Name: instanceName(in), Sym: in.sym}

	saveFn, saveSig, saveSinks, saveFree := b.fn, b.sig, b.sinks, b.free
	saveStencil, saveForeign := b.stencil, b.foreign
	b.fn, b.sig, b.sinks, b.free = fn, in.sig, nil, make(map[*Object]bool)
	b.stencil = &stencil{
		sym:   in.sym,
		subst: types2.NewSubstitution(b.ctxt, foreignTypeParams(in.body.Dict), in.targs),
	}
	b.foreign = &foreignFrame{in: in, body: in.body, dict: in.body.Dict}

	fn.Type = b.irType(in.sig)
	if recv := in.sig.Recv(); recv != nil {
		fn.Recv = b.foreignParam(recv, ClassParam)
	}
	for i := 0; i < in.sig.Params().Len(); i++ {
		fn.Params = append(fn.Params, b.foreignParam(in.sig.Params().At(i), ClassParam))
	}
	for i := 0; i < in.sig.Results().Len(); i++ {
		fn.Results = append(fn.Results, b.foreignParam(in.sig.Results().At(i), ClassResult))
	}
	if len(b.foreign.locals) != len(in.body.Params) {
		b.errorf("ir: %s opens with %d local(s) and its signature has %d",
			in.sym, len(in.body.Params), len(b.foreign.locals))
	} else {
		fn.Body = b.foreignStmts(in.body.Stmts)
	}

	b.fn, b.sig, b.sinks, b.free = saveFn, saveSig, saveSinks, saveFree
	b.stencil, b.foreign = saveStencil, saveForeign
	return fn
}

// foreignParam declares one of the locals a body opens with.
//
// The object is made here rather than through [builder.obj], because the
// variable it stands for is one the checker made for the instantiated
// signature and the body names no variable at all. Giving it a key in the
// object table would be an identity nothing looks up.
func (b *builder) foreignParam(v *types2.Var, class Class) *Object {
	t := b.subst(v.Type())
	o := &Object{Name: v.Name(), Class: class, Type: b.irType(t)}
	b.owner[o] = b.fn
	b.foreign.locals = append(b.foreign.locals, foreignLocal{obj: o, typ: t})
	return o
}

// foreignLocalDecl declares one variable a statement of the body declares.
func (b *builder) foreignLocalDecl(name string, t types2.Type) *Object {
	o := &Object{Name: name, Class: ClassLocal, Type: b.irType(t)}
	if b.fn != nil && name != "_" {
		b.fn.Locals = append(b.fn.Locals, o)
		b.owner[o] = b.fn
	}
	b.foreign.locals = append(b.foreign.locals, foreignLocal{obj: o, typ: t})
	return o
}

// refuseForeign records a construct this walk does not build.
//
// The declaration is named first, because the construct is in a body no source
// file of this build holds and the reader of the message has to know which
// function to go and look at.
func (b *builder) refuseForeign(format string, args ...any) {
	what := fmt.Sprintf(format, args...)
	b.errorf("ir: the body of %s, read from package %s, holds %s, which an instantiation of another package's generic is not built from",
		b.foreign.in.sym, b.foreign.in.origin.Pkg().Path(), what)
}

// foreignType returns a type reference of the body with the type arguments in
// place of the type parameters.
func (b *builder) foreignType(u export.TypeUse) types2.Type { return b.subst(u.Type) }

// foreignTypeOf returns the type of the value an expression of the body
// produces.
//
// Every expression the format types carries its type, because gc writes a
// reshape node holding it in front of nearly every one. The two that carry
// none are the two answered here: a use of a local, whose type is in the
// frame's numbering, and a reference to a declaration, whose type is the
// declaration's.
func (b *builder) foreignTypeOf(e export.Expr) types2.Type {
	switch e := e.(type) {
	case *export.LocalExpr:
		if l, ok := b.foreignLocalAt(e); ok {
			return l.typ
		}
		return nil
	case *export.GlobalExpr:
		if e.Obj.Obj != nil {
			return b.subst(e.Obj.Obj.Type())
		}
	}
	if t := e.ExprType(); t != nil {
		return b.subst(t)
	}
	return nil
}

// foreignLocalAt returns the local one use names.
func (b *builder) foreignLocalAt(e *export.LocalExpr) (foreignLocal, bool) {
	if e.Captured {
		b.refuseForeign("a use of a variable captured by a function literal")
		return foreignLocal{}, false
	}
	if e.Index < 0 || e.Index >= len(b.foreign.locals) {
		b.refuseForeign("a use of local %d, of %d the body declares", e.Index, len(b.foreign.locals))
		return foreignLocal{}, false
	}
	return b.foreign.locals[e.Index], true
}

// @@@ Statements

// foreignStmts builds a statement list into a list of its own.
func (b *builder) foreignStmts(list []export.Stmt) []Stmt {
	b.push()
	for _, s := range list {
		b.foreignStmt(s)
	}
	return b.pop()
}

func (b *builder) foreignStmt(s export.Stmt) {
	switch s := s.(type) {
	case nil:
		return
	case *export.BlockStmt:
		b.emit(&Node{Op: OBlock, Type: voidType, Body: b.foreignStmts(s.Body)})
	case *export.ExprStmt:
		if n := b.foreignExpr(s.X); n != nil {
			b.emit(n)
		}
	case *export.ReturnStmt:
		b.foreignReturn(s)
	case *export.IfStmt:
		b.foreignIf(s)
	case *export.ForStmt:
		b.foreignFor(s)
	default:
		b.refuseForeign("the statement %q", export.StmtKindOf(s).String())
	}
}

// foreignReturn builds a return.
func (b *builder) foreignReturn(s *export.ReturnStmt) {
	n := &Node{Op: OReturn, Type: voidType}
	if s.Results.Single {
		// return f(), where the results of f are the results of this
		// function. gc records the destination of each of them, and the tree
		// this pass builds keeps the call whole instead, so the two shapes
		// are not the same tree and the translation is not the identity.
		b.refuseForeign("a return of one call's results")
		return
	}
	res := b.sig.Results()
	for i, e := range s.Results.Exprs {
		var want types2.Type
		if i < res.Len() {
			want = res.At(i).Type()
		}
		val := b.foreignExpr(e)
		if len(s.Results.Exprs) > 1 {
			val = b.ordered(val)
		}
		n.Args = append(n.Args, b.assignConv(val, b.foreignTypeOf(e), want))
	}
	b.emit(n)
}

// foreignIf builds an if statement.
func (b *builder) foreignIf(s *export.IfStmt) {
	if s.Static != 0 {
		// gc proved the condition constant and wrote only the branch it kept,
		// so the tree holds neither the condition's value nor the branch it
		// dropped. Building the branch that is there would be right and
		// building the statement is not, because the two cannot be told apart
		// from a tree that is missing one.
		b.refuseForeign("an if statement whose condition gc folded to a constant")
		return
	}
	if s.Then == nil {
		b.refuseForeign("an if statement with no consequent")
		return
	}
	n := &Node{Op: OIf, Type: voidType}
	b.push()
	for _, init := range s.Init {
		b.foreignStmt(init)
	}
	n.X = b.foreignExpr(s.Cond)
	n.Init = b.pop()
	n.Body = b.foreignStmts(s.Then.Body)
	if len(s.Else) > 0 {
		n.Else = b.foreignStmts(s.Else)
	}
	b.emit(n)
}

// foreignFor builds a for statement.
//
// Only the range form over a slice is built. The three-clause form declares
// its variables in an assignment, which this walk does not build, so a loop of
// that shape cannot reach here with its declarations intact.
func (b *builder) foreignFor(s *export.ForStmt) {
	if s.Range == nil {
		b.refuseForeign("a three-clause for statement")
		return
	}
	if s.Body == nil {
		b.refuseForeign("a range statement with no body")
		return
	}
	rc := s.Range

	n := &Node{Op: ORange, Type: voidType}
	b.push()
	x := b.foreignExpr(rc.X)
	xt := b.foreignTypeOf(rc.X)
	elem, ok := coreType(xt).(*types2.Slice)
	if !ok {
		b.pop()
		b.refuseForeign("a range over %s, and only a range over a slice is built", types2.TypeString(xt, nil))
		return
	}
	n.X = x
	n.Init = b.pop()

	// The key of a slice is an int and the value is its element. A
	// destination of any other type would need the conversion the clause
	// records, and a destination that is not a declaration would need the
	// assignment this walk does not build.
	want := []types2.Type{types2.Typ[types2.Int], elem.Elem()}
	var decls []*Object
	for i, a := range rc.Lhs {
		if i >= len(want) {
			b.refuseForeign("a range clause with %d destinations", len(rc.Lhs))
			return
		}
		switch a.Kind {
		case export.AssignBlank:
			o := &Object{Name: "_", Class: ClassLocal, Type: b.irType(want[i])}
			n.Args = append(n.Args, &Node{Op: OLocal, Type: o.Type, Obj: o})
		case export.AssignDef:
			t := b.foreignType(a.Type)
			if !types2.Identical(t, want[i]) {
				b.refuseForeign("a range clause whose destination %d is %s and not %s",
					i, types2.TypeString(t, nil), types2.TypeString(want[i], nil))
				return
			}
			o := b.foreignLocalDecl(a.Name, t)
			decls = append(decls, o)
			n.Args = append(n.Args, &Node{Op: OLocal, Type: o.Type, Obj: o})
		default:
			b.refuseForeign("a range clause that assigns to a variable it does not declare")
			return
		}
	}

	n.Body = b.foreignStmts(s.Body.Body)
	if len(decls) > 0 {
		if !s.DistinctVars {
			// The loop shares one variable across the iterations, which is
			// the rule before Go 1.22. perIteration writes the rule after it,
			// and the two differ only for a variable whose address is taken.
			for _, o := range decls {
				if o.Addrtaken {
					b.refuseForeign("a loop that shares one address-taken variable across its iterations, which is the rule before Go 1.22")
					return
				}
			}
		}
		b.perIteration(n, decls)
	}
	b.emit(n)
}

// @@@ Expressions

// foreignOperand builds a subexpression in a position where the order of
// evaluation is observable.
func (b *builder) foreignOperand(e export.Expr) Expr { return b.ordered(b.foreignExpr(e)) }

// foreignGuarded builds an expression that is not evaluated unconditionally
// where it stands. See [builder.guarded].
func (b *builder) foreignGuarded(e export.Expr) Expr {
	b.noHoist++
	b.push()
	n := b.foreignExpr(e)
	pre := b.pop()
	b.noHoist--
	if n != nil && len(pre) > 0 {
		n.Init = append(pre, n.Init...)
	}
	return n
}

func (b *builder) foreignExpr(e export.Expr) Expr {
	switch e := e.(type) {
	case nil:
		return nil
	case *export.ConstExpr:
		return b.constNode(syntax.NoPos, e.Value, b.foreignType(e.Type))
	case *export.LocalExpr:
		l, ok := b.foreignLocalAt(e)
		if !ok {
			return b.badExpr(syntax.NoPos)
		}
		b.noteUse(l.obj)
		return &Node{Op: OLocal, Type: l.obj.Type, Obj: l.obj}
	case *export.GlobalExpr:
		return b.foreignGlobal(e)
	case *export.FieldValExpr:
		return b.foreignField(e)
	case *export.IndexExpr:
		return b.foreignIndex(e)
	case *export.UnaryExpr:
		return b.foreignUnary(e)
	case *export.BinaryExpr:
		return b.foreignBinary(e)
	case *export.ConvertExpr:
		return b.foreignConvert(e)
	case *export.CallExpr:
		return b.foreignCall(e)
	default:
		b.refuseForeign("the expression %q", export.ExprKindOf(e).String())
		return b.badExpr(syntax.NoPos)
	}
}

// foreignGlobal builds a reference to a package-scope declaration.
func (b *builder) foreignGlobal(e *export.GlobalExpr) Expr {
	use := e.Obj
	switch o := use.Obj.(type) {
	case nil:
		b.refuseForeign("a reference to %s, which the checker recorded no declaration for", use.Name)
	case *types2.Func:
		if len(use.Targs) != 0 {
			// An instantiated generic reaches a body as its own node, so a
			// reference here that carries type arguments is a shape the
			// reader produced and this walk has no case for.
			b.refuseForeign("a reference to the generic %s outside an instantiation node", use.Name)
			break
		}
		io := b.obj(o)
		return &Node{Op: OGlobal, Type: io.Type, Obj: io}
	case *types2.Var:
		io := b.obj(o)
		op := OLocal
		if io.Class == ClassGlobal {
			op = OGlobal
		}
		return &Node{Op: op, Type: io.Type, Obj: io}
	case *types2.Const:
		return &Node{Op: OConst, Type: b.irType(o.Type()), Val: Const{Val: o.Val()}}
	case *types2.Builtin:
		b.refuseForeign("a call of the builtin %s", o.Name())
	case *types2.TypeName:
		b.refuseForeign("a reference to the type %s", o.Name())
	default:
		b.refuseForeign("a reference to %s, which is a %T", use.Name, use.Obj)
	}
	return b.badExpr(syntax.NoPos)
}

// foreignField builds x.f, where f is a field.
//
// The selection is looked up again against the operand's type rather than read
// out of the tree, because the format names the field and not its position:
// the position is a fact about the type, and the type here is the substituted
// one. The lookup is the checker's own, so nothing here re-derives the
// language's rule for finding a field through an embedded one.
func (b *builder) foreignField(e *export.FieldValExpr) Expr {
	xt := b.foreignTypeOf(e.X)
	if xt == nil {
		b.refuseForeign("a selection of %s on an operand with no type", e.Sel.Name)
		return b.badExpr(syntax.NoPos)
	}
	obj, index, _ := types2.LookupFieldOrMethod(xt, true, e.Sel.Pkg, e.Sel.Name)
	if _, isField := obj.(*types2.Var); !isField || len(index) == 0 {
		b.refuseForeign("a selection of %s, which %s has no field for", e.Sel.Name, types2.TypeString(xt, nil))
		return b.badExpr(syntax.NoPos)
	}
	base, _ := b.fieldPath(b.foreignOperand(e.X), xt, index)
	return base
}

// foreignIndex builds x[i].
func (b *builder) foreignIndex(e *export.IndexExpr) Expr {
	if e.MapRType != nil {
		b.refuseForeign("an index of a map")
		return b.badExpr(syntax.NoPos)
	}
	xt := b.foreignTypeOf(e.X)
	base := b.foreignOperand(e.X)
	core := coreType(xt)
	if p, ok := core.(*types2.Pointer); ok {
		// Indexing a pointer to an array dereferences it, which the
		// specification makes implicit and this tree does not.
		base = &Node{Op: ODeref, Type: b.irType(p.Elem()), X: base}
		xt = p.Elem()
		core = coreType(xt)
	}
	switch core.(type) {
	case *types2.Slice, *types2.Array, *types2.Basic:
	default:
		b.refuseForeign("an index of %s", types2.TypeString(xt, nil))
		return b.badExpr(syntax.NoPos)
	}
	et := b.foreignTypeOf(e)
	if et == nil {
		b.refuseForeign("an index whose type the stream does not carry")
		return b.badExpr(syntax.NoPos)
	}
	idx := b.assignConv(b.foreignOperand(e.Index), b.foreignTypeOf(e.Index), types2.Typ[types2.Int])
	return &Node{Op: OIndex, Type: b.irType(et), X: base, Y: idx}
}

// foreignUnary builds a unary operation, an address or a dereference.
func (b *builder) foreignUnary(e *export.UnaryExpr) Expr {
	switch e.Op {
	case export.OpAddr:
		return b.addrOf(b.foreignExpr(e.X), b.foreignTypeOf(e.X))
	case export.OpDeref:
		t := b.foreignTypeOf(e)
		if t == nil {
			b.refuseForeign("a dereference whose type the stream does not carry")
			return b.badExpr(syntax.NoPos)
		}
		return &Node{Op: ODeref, Type: b.irType(t), X: b.foreignOperand(e.X)}
	}
	op, ok := foreignUnaryOps[e.Op]
	if !ok {
		b.refuseForeign("the unary operator %q", e.Op.String())
		return b.badExpr(syntax.NoPos)
	}
	// A unary operation has the type of its operand, which is what the
	// reshape node in front of the operand carries when the operation itself
	// has none.
	t := b.foreignTypeOf(e)
	if t == nil {
		t = b.foreignTypeOf(e.X)
	}
	if t == nil {
		b.refuseForeign("an operation whose type the stream does not carry")
		return b.badExpr(syntax.NoPos)
	}
	return &Node{Op: OUnary, Op1: op, Type: b.irType(t), X: b.foreignOperand(e.X)}
}

// foreignBinary builds a binary operation.
func (b *builder) foreignBinary(e *export.BinaryExpr) Expr {
	op, ok := foreignBinaryOps[e.Op]
	if !ok {
		b.refuseForeign("the binary operator %q", e.Op.String())
		return b.badExpr(syntax.NoPos)
	}
	bt := b.foreignBinaryType(e, op)
	if bt == nil {
		b.refuseForeign("an operation whose type the stream does not carry")
		return b.badExpr(syntax.NoPos)
	}
	t := b.irType(bt)
	switch op {
	case syntax.AndAnd, syntax.OrOr:
		// The right operand is evaluated only if the left does not decide the
		// result, so nothing may be hoisted out of it.
		return &Node{Op: OBinary, Op1: op, Type: t, X: b.foreignOperand(e.X), Y: b.foreignGuarded(e.Y)}
	case syntax.Shl, syntax.Shr:
		// The count keeps its own type.
		return &Node{Op: OBinary, Op1: op, Type: t, X: b.foreignOperand(e.X), Y: b.foreignOperand(e.Y)}
	}
	lt, rt := b.foreignTypeOf(e.X), b.foreignTypeOf(e.Y)
	lhs, rhs := b.foreignOperand(e.X), b.foreignOperand(e.Y)
	lhs, rhs = b.balance(lhs, rhs, lt, rt)
	kind := OBinary
	switch op {
	case syntax.Eql, syntax.Neq, syntax.Lss, syntax.Leq, syntax.Gtr, syntax.Geq:
		kind = OCompare
	}
	return &Node{Op: kind, Op1: op, Type: t, X: lhs, Y: rhs}
}

// foreignBinaryType returns the type of a binary operation.
//
// gc writes the reshape node that carries a type in front of an operand and
// not in front of an operation, so this is the one place the type has to come
// from the language's rule rather than out of the stream. The rule has two
// cases: a comparison and a conditional produce a bool, and every other
// operation has the type of its left operand, which is the one a shift keeps
// too.
func (b *builder) foreignBinaryType(e *export.BinaryExpr, op syntax.Operator) types2.Type {
	if t := b.foreignTypeOf(e); t != nil {
		return t
	}
	switch op {
	case syntax.Eql, syntax.Neq, syntax.Lss, syntax.Leq, syntax.Gtr, syntax.Geq, syntax.AndAnd, syntax.OrOr:
		return types2.Typ[types2.Bool]
	}
	if t := b.foreignTypeOf(e.X); t != nil {
		return t
	}
	return b.foreignTypeOf(e.Y)
}

// foreignConvert builds a conversion.
//
// An implicit one goes through assignConv, which is where the rest of this
// package makes an assignment's conversion explicit, so a conversion that
// changes nothing produces no node here either.
func (b *builder) foreignConvert(e *export.ConvertExpr) Expr {
	to := b.foreignType(e.Type)
	x := b.foreignOperand(e.X)
	if e.Implicit {
		return b.assignConv(x, b.foreignTypeOf(e.X), to)
	}
	return &Node{Op: OConvert, Type: b.irType(to), X: x}
}

// foreignCall builds a call.
func (b *builder) foreignCall(e *export.CallExpr) Expr {
	if e.Method != nil {
		b.refuseForeign("a call through a method selection")
		return b.badExpr(syntax.NoPos)
	}
	var fun Expr
	var sig *types2.Signature
	if e.Inst != nil {
		obj, isig := b.foreignInstCallee(e.Inst)
		if obj == nil {
			return b.badExpr(syntax.NoPos)
		}
		fun, sig = &Node{Op: OGlobal, Type: obj.Type, Obj: obj}, isig
	} else {
		sig, _ = coreType(b.foreignTypeOf(e.Fun)).(*types2.Signature)
		fun = b.ordered(b.foreignExpr(e.Fun))
	}
	if sig == nil {
		b.refuseForeign("a call of an operand with no signature")
		return b.badExpr(syntax.NoPos)
	}
	n := &Node{Op: OCall, Type: b.resultType(sig), X: fun}
	n.Args = b.foreignCallArgs(e, sig)
	if e.Dots {
		n.Index = spread
	}
	return n
}

// foreignInstCallee returns the object and the signature of an instantiated
// generic function a call names.
//
// A call whose type arguments are all known statically carries the callee. One
// whose arguments depend on the enclosing declaration's carries a dictionary
// slot instead, and the dictionary the archive holds is what names the callee
// and its arguments. That is how slices.Contains reaches slices.Index: the
// call node names neither.
func (b *builder) foreignInstCallee(inst *export.FuncInst) (*Object, *types2.Signature) {
	use := inst.Obj
	if inst.Derived {
		d := b.foreign.dict
		if inst.DictIdx < 0 || inst.DictIdx >= len(d.Subdicts) {
			b.refuseForeign("a call naming subdictionary slot %d of %d", inst.DictIdx, len(d.Subdicts))
			return nil, nil
		}
		use = d.Subdicts[inst.DictIdx]
	}
	origin, _ := use.Obj.(*types2.Func)
	if origin == nil {
		b.refuseForeign("a call of %s, which the checker recorded no declaration for", use.Name)
		return nil, nil
	}
	targs := make([]types2.Type, len(use.Targs))
	for i := range targs {
		targs[i] = types2.Canonical(b.ctxt, b.subst(use.Targs[i].Type))
	}
	return b.funcInstance(origin, targs)
}

// foreignCallArgs builds the arguments of a call, with the conversion each
// parameter makes explicit and the variadic parameter packed into its slice.
func (b *builder) foreignCallArgs(e *export.CallExpr, sig *types2.Signature) []Expr {
	if e.Args.Single {
		// f(g()), where the results of g are the parameters of f.
		b.refuseForeign("a call whose arguments are one call's results")
		return nil
	}
	np := sig.Params().Len()
	var out, variadic []Expr
	for i, a := range e.Args.Exprs {
		at := b.foreignTypeOf(a)
		if sig.Variadic() && i >= np-1 {
			last := sig.Params().At(np - 1).Type()
			if e.Dots {
				out = append(out, b.assignConv(b.foreignOperand(a), at, last))
				continue
			}
			want := last
			if s, ok := coreType(last).(*types2.Slice); ok {
				want = s.Elem()
			}
			variadic = append(variadic, b.assignConv(b.foreignOperand(a), at, want))
			continue
		}
		if i >= np {
			b.refuseForeign("a call with %d arguments of a function with %d parameters", len(e.Args.Exprs), np)
			continue
		}
		out = append(out, b.assignConv(b.foreignOperand(a), at, sig.Params().At(i).Type()))
	}
	if sig.Variadic() && !e.Dots {
		last := sig.Params().At(np - 1).Type()
		out = append(out, &Node{Op: OCompositeLit, Type: b.irType(last), Args: variadic})
	}
	return out
}

// foreignUnaryOps and foreignBinaryOps are the operators this walk builds.
//
// The format carries gc's ir.Op ordinal and this pass carries the source
// token, so the two sets are named against each other once. An operator that
// is not here is refused by its own spelling.
var foreignUnaryOps = map[export.Op]syntax.Operator{
	export.OpNot:    syntax.Not,
	export.OpBitNot: syntax.Xor,
	export.OpPlus:   syntax.Add,
	export.OpNeg:    syntax.Sub,
}

var foreignBinaryOps = map[export.Op]syntax.Operator{
	export.OpAdd:    syntax.Add,
	export.OpSub:    syntax.Sub,
	export.OpMul:    syntax.Mul,
	export.OpDiv:    syntax.Div,
	export.OpMod:    syntax.Rem,
	export.OpAnd:    syntax.And,
	export.OpAndNot: syntax.AndNot,
	export.OpOr:     syntax.Or,
	export.OpXor:    syntax.Xor,
	export.OpLsh:    syntax.Shl,
	export.OpRsh:    syntax.Shr,
	export.OpEq:     syntax.Eql,
	export.OpNe:     syntax.Neq,
	export.OpLt:     syntax.Lss,
	export.OpLe:     syntax.Leq,
	export.OpGt:     syntax.Gtr,
	export.OpGe:     syntax.Geq,
	export.OpAndAnd: syntax.AndAnd,
	export.OpOrOr:   syntax.OrOr,
}
