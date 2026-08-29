// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"go/constant"

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
//   - A function literal is a body of its own, in another element of the
//     archive, with a numbering of its own and a capture list naming variables
//     of the body around it. The frame below therefore holds two numberings
//     and a use says which one it indexes.
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
//
// A kind the walk maps is refused again inside itself wherever the shape it
// met is one it cannot build: a builtin that needs a run-time descriptor, a
// method reached through a dictionary, a value converted where it is assigned.
// Each of those names itself, for the same reason the kind does.

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

	// captures is the second numbering a body of a function literal has: the
	// variables of the body around it that the literal reads, in the order the
	// format lists them. A use names one numbering or the other and the format
	// says which (specs/033-closures-defer-panic.md), so the two are kept
	// apart here rather than joined into one list.
	captures []foreignLocal
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
		errs := len(b.errs)
		fn.Body = b.foreignStmts(in.body.Stmts)
		b.foreignCheckNumbering(errs, in.body)
	}

	b.fn, b.sig, b.sinks, b.free = saveFn, saveSig, saveSinks, saveFree
	b.stencil, b.foreign = saveStencil, saveForeign
	return fn
}

// foreignCheckNumbering compares the locals the walk declared with the locals
// the tree numbers.
//
// A use of a local is a number and nothing else, so a walk that declared one
// local too few numbers every later local one place out. That is not a build
// that stops: it is a body that reads the wrong variable, compiles, links and
// computes something else, which is the one failure this whole file is
// arranged against. Counting the declaring sites of the tree and comparing the
// answer turns it into a build that stops.
//
// errs is the number of errors recorded before the body was walked. A walk
// that refused something stopped part way through, so its count is expected to
// disagree and the comparison is skipped: the refusal is already the message.
func (b *builder) foreignCheckNumbering(errs int, body *export.Body) {
	if len(b.errs) != errs {
		return
	}
	if want := foreignNumLocals(body); len(b.foreign.locals) != want {
		b.errorf("ir: %s numbers %d local(s) and the walk of it declared %d",
			b.foreign.in.sym, want, len(b.foreign.locals))
	}
}

// foreignNumLocals returns the number of locals one body numbers.
//
// It is the receiver, the parameters and the results, then one for each
// variable a statement declares: an assignment that declares its destination,
// a range clause that declares one, and a clause of a type switch whose guard
// names a variable. Those are the three sites the format writes a local at.
//
// A function literal's body is another element with a numbering of its own, so
// nothing here descends into one.
func foreignNumLocals(body *export.Body) int {
	n := len(body.Params)
	assignees := func(list []export.Assignee) {
		for _, a := range list {
			if a.Kind == export.AssignDef {
				n++
			}
		}
	}
	var stmts func([]export.Stmt)
	stmts = func(list []export.Stmt) {
		for _, s := range list {
			switch s := s.(type) {
			case *export.BlockStmt:
				stmts(s.Body)
			case *export.AssignStmt:
				assignees(s.Lhs)
			case *export.IfStmt:
				stmts(s.Init)
				if s.Then != nil {
					stmts(s.Then.Body)
				}
				stmts(s.Else)
			case *export.ForStmt:
				if s.Range != nil {
					assignees(s.Range.Lhs)
				} else {
					stmts(s.Init)
					stmts(s.Post)
				}
				if s.Body != nil {
					stmts(s.Body.Body)
				}
			case *export.SwitchStmt:
				stmts(s.Init)
				for i := range s.Clauses {
					if s.Clauses[i].Var != nil {
						n++
					}
					stmts(s.Clauses[i].Body)
				}
			case *export.SelectStmt:
				for i := range s.Clauses {
					stmts(s.Clauses[i].Comm)
					stmts(s.Clauses[i].Body)
				}
			}
		}
	}
	stmts(body.Stmts)
	return n
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
// reshape node holding it in front of nearly every one. Three kinds carry
// none, and each is answered here rather than by the caller that met it.
//
// A use of a local and a reference to a declaration are the first two: the
// type of a local is in the frame's numbering and the type of a declaration is
// the declaration's.
//
// An operation is the third, and it is the one that needs the language's rule.
// gc writes the reshape node in front of an operand and not in front of an
// operation, so an operation whose operand is another operation has no type in
// the stream at either end. The rule has two cases: a comparison and a
// conditional produce a bool, and every other operation has the type of the
// operand it keeps, which is the left one for a binary operation and a shift
// and the only one for a unary operation.
func (b *builder) foreignTypeOf(e export.Expr) types2.Type {
	switch e := e.(type) {
	case nil:
		return nil
	case *export.LocalExpr:
		// The silent lookup, because this is the type query and not the walk.
		// The walk reaches the same use and refuses it there, and refusing it
		// here as well would record the same message once per operation the
		// use is nested in.
		if l, ok := b.foreignVarAt(e); ok {
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
	switch e := e.(type) {
	case *export.BinaryExpr:
		switch e.Op {
		case export.OpEq, export.OpNe, export.OpLt, export.OpLe, export.OpGt, export.OpGe,
			export.OpAndAnd, export.OpOrOr:
			return types2.Typ[types2.Bool]
		}
		if t := b.foreignTypeOf(e.X); t != nil {
			return t
		}
		return b.foreignTypeOf(e.Y)
	case *export.UnaryExpr:
		switch e.Op {
		case export.OpAddr, export.OpDeref, export.OpRecv:
			// Each produces a type its operand's does not name, and the
			// operand alone does not say which. The caller refuses one whose
			// type the stream did not carry.
			return nil
		}
		return b.foreignTypeOf(e.X)
	}
	return nil
}

// foreignLocalAt returns the variable one use names.
//
// The two numberings are separate lists and the format says which one a use
// indexes. A body that is not a function literal's captures nothing, so its
// capture list is empty and a use that names one is out of range, which is the
// refusal a malformed body gets for free.
func (b *builder) foreignLocalAt(e *export.LocalExpr) (foreignLocal, bool) {
	l, ok := b.foreignVarAt(e)
	if !ok {
		what, n := "local", len(b.foreign.locals)
		if e.Captured {
			what, n = "captured variable", len(b.foreign.captures)
		}
		b.refuseForeign("a use of %s %d, of %d the body has", what, e.Index, n)
	}
	return l, ok
}

// foreignVarAt is [builder.foreignLocalAt] without the refusal.
func (b *builder) foreignVarAt(e *export.LocalExpr) (foreignLocal, bool) {
	list := b.foreign.locals
	if e.Captured {
		list = b.foreign.captures
	}
	if e.Index < 0 || e.Index >= len(list) {
		return foreignLocal{}, false
	}
	return list[e.Index], true
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
	case *export.AssignStmt:
		b.foreignAssign(s)
	case *export.AssignOpStmt:
		b.foreignAssignOp(s)
	case *export.IncDecStmt:
		b.foreignIncDec(s)
	case *export.BranchStmt:
		b.foreignBranch(s)
	case *export.ReturnStmt:
		b.foreignReturn(s)
	case *export.IfStmt:
		b.foreignIf(s)
	case *export.ForStmt:
		b.foreignFor(s)
	case *export.SwitchStmt:
		b.foreignSwitch(s)
	default:
		b.refuseForeign("the statement %q", export.StmtKindOf(s).String())
	}
}

// foreignAssign builds an assignment, a short variable declaration, and a var
// declaration inside a body.
//
// The destinations are built before the values, and that order is a
// requirement rather than a preference. gc's writer wrote the destination list
// first, so a variable a destination declares takes its number before anything
// on the right takes one, and a walk that built the right first would number
// every later local one place out and read the wrong variable with no
// diagnostic. The specification's order of evaluation is the same order, so
// nothing is traded for it.
//
// The rest is [builder.assign] over the other tree: the operands of the
// destinations are evaluated before any destination is written, which is what
// makes "a, b = b, a" a swap and not two copies of one value.
func (b *builder) foreignAssign(s *export.AssignStmt) {
	parallel := len(s.Lhs) > 1
	dsts := make([]Expr, len(s.Lhs))
	dtypes := make([]types2.Type, len(s.Lhs))
	for i := range s.Lhs {
		a := &s.Lhs[i]
		switch a.Kind {
		case export.AssignBlank:
			// The blank identifier names no storage, and its destination is
			// built below, once the value it is assigned says what type it
			// has. Nothing is numbered for it: the format writes no local for
			// a blank destination.
		case export.AssignDef:
			t := b.foreignType(a.Type)
			o := b.foreignLocalDecl(a.Name, t)
			dsts[i], dtypes[i] = &Node{Op: OLocal, Type: o.Type, Obj: o}, t
		case export.AssignExpr:
			dsts[i], dtypes[i] = b.foreignExpr(a.Expr), b.foreignTypeOf(a.Expr)
			if parallel {
				dsts[i] = b.stabilizeParallel(dsts[i])
			}
		default:
			b.refuseForeign("an assignment to a destination the format writes as %q", a.Kind.String())
			return
		}
	}

	switch {
	case s.Rhs.Single:
		b.foreignMultiAssign(s, dsts, dtypes)
	case len(s.Rhs.Exprs) == 0:
		b.foreignDeclare(s, dsts)
	case len(s.Lhs) == 1 && len(s.Rhs.Exprs) == 1:
		e := s.Rhs.Exprs[0]
		et := b.foreignTypeOf(e)
		val := b.assignConv(b.foreignExpr(e), et, dtypes[0])
		b.emit(b.foreignAssignTo(s.Lhs[0], b.foreignBlank(dsts[0], dtypes[0], et), val))
	case len(s.Lhs) == len(s.Rhs.Exprs):
		srcs := make([]Expr, len(s.Rhs.Exprs))
		for i, e := range s.Rhs.Exprs {
			et := b.foreignTypeOf(e)
			dsts[i] = b.foreignBlank(dsts[i], dtypes[i], et)
			srcs[i] = b.snapshot(b.assignConv(b.foreignExpr(e), et, dtypes[i]))
		}
		for i := range dsts {
			b.emit(b.foreignAssignTo(s.Lhs[i], dsts[i], srcs[i]))
		}
	default:
		b.refuseForeign("an assignment of %d value(s) to %d destination(s)", len(s.Rhs.Exprs), len(s.Lhs))
	}
}

// foreignAssignTo returns the assignment of src to one destination.
//
// A destination the statement declares is marked, which is what the syntax
// walk's define does. The mark is per destination rather than per statement,
// because the format writes one kind per destination and "x, err := f()" where
// err already exists carries both.
func (b *builder) foreignAssignTo(a export.Assignee, dst, src Expr) Stmt {
	if a.Kind == export.AssignDef {
		return define(syntax.NoPos, dst, src)
	}
	return Assign(syntax.NoPos, dst, src)
}

// foreignBlank fills in the destination of a blank assignee, whose type is the
// type of the value assigned to it and is carried nowhere else.
func (b *builder) foreignBlank(dst Expr, dstType, srcType types2.Type) Expr {
	if dst != nil {
		return dst
	}
	t := dstType
	if t == nil {
		t = srcType
	}
	if t == nil {
		b.refuseForeign("an assignment to the blank identifier of a value whose type the stream does not carry")
		return b.badExpr(syntax.NoPos)
	}
	// The object is in no frame, so nothing allocates a slot for it and the
	// assignment survives only for the effects of its right-hand side.
	o := &Object{Name: "_", Class: ClassLocal, Type: b.irType(t)}
	return &Node{Op: OLocal, Type: o.Type, Obj: o}
}

// foreignDeclare builds "var x T", whose value is the zero of its type.
//
// It is a declaration and not an assignment, which is what ODeclare exists
// for: the specification makes each execution of a declaration a fresh
// variable, so a var in a loop body is zero on every iteration.
func (b *builder) foreignDeclare(s *export.AssignStmt, dsts []Expr) {
	for i := range s.Lhs {
		if s.Lhs[i].Kind != export.AssignDef {
			b.refuseForeign("a declaration whose destination %d declares no variable", i)
			return
		}
		if s.Lhs[i].Name == "_" {
			// The blank identifier names no storage, so there is nothing to
			// zero.
			continue
		}
		b.emit(&Node{Op: ODeclare, Type: voidType, X: dsts[i]})
	}
}

// foreignMultiAssign builds an assignment whose one right-hand side produces
// several values.
//
// The types of the values come out of the tree rather than out of the callee's
// signature: the format writes one entry per result, with the conversion the
// destination applies to it, and that entry is the checker's own answer for a
// body no syntax tree here holds.
func (b *builder) foreignMultiAssign(s *export.AssignStmt, dsts []Expr, dtypes []types2.Type) {
	m := &s.Rhs
	if len(m.Results) != len(s.Lhs) {
		b.refuseForeign("an assignment of %d value(s) to %d destination(s)", len(m.Results), len(s.Lhs))
		return
	}
	for i := range m.Results {
		if m.Results[i].Converted {
			// The value is converted where it is assigned, and one node
			// cannot hold a conversion per destination. The syntax walk
			// answers it with a temporary per value; doing the same here needs
			// the descriptors the entry carries for a conversion to an
			// interface, which nothing below this pass builds.
			b.refuseForeign("an assignment whose value %d is converted where it is assigned", i)
			return
		}
		dsts[i] = b.foreignBlank(dsts[i], dtypes[i], b.foreignType(m.Results[i].Src))
	}
	def := len(s.Lhs) > 0
	for i := range s.Lhs {
		if s.Lhs[i].Kind != export.AssignDef {
			def = false
		}
	}
	op1 := syntax.Operator(0)
	if def {
		op1 = defineOp
	}
	b.emit(&Node{Op: OAssign, Op1: op1, Type: voidType, Args: dsts, Y: b.foreignExpr(m.Expr)})
}

// foreignAssignOp builds x op= y, by rewriting it to x = x op y.
//
// The rewrite is what keeps an OBinary an operation and never an assignment.
// The left operand is evaluated once, which is why its parts are held in
// temporaries before it is used twice.
func (b *builder) foreignAssignOp(s *export.AssignOpStmt) {
	op, ok := foreignBinaryOps[s.Op]
	if !ok {
		b.refuseForeign("the operation assignment %q", s.Op.String())
		return
	}
	dst := b.stabilize(b.foreignExpr(s.Lhs))
	if dst == nil {
		return
	}
	lt := b.foreignTypeOf(s.Lhs)
	var rhs Expr
	if op == syntax.Shl || op == syntax.Shr {
		// The count of a shift keeps its own type. The specification does not
		// convert it to the type of the shifted operand.
		rhs = b.foreignOperand(s.Rhs)
	} else {
		rhs = b.assignConv(b.foreignOperand(s.Rhs), b.foreignTypeOf(s.Rhs), lt)
	}
	n := &Node{Op: OBinary, Op1: op, Type: dst.Type, X: cloneExpr(dst), Y: rhs}
	b.emit(Assign(syntax.NoPos, dst, n))
}

// foreignIncDec builds x++ and x--.
func (b *builder) foreignIncDec(s *export.IncDecStmt) {
	op, ok := foreignBinaryOps[s.Op]
	if !ok || (op != syntax.Add && op != syntax.Sub) {
		b.refuseForeign("the increment or decrement %q", s.Op.String())
		return
	}
	dst := b.stabilize(b.foreignExpr(s.X))
	if dst == nil {
		return
	}
	one := &Node{Op: OConst, Type: dst.Type, Val: Const{Val: constant.MakeInt64(1)}}
	n := &Node{Op: OBinary, Op1: op, Type: dst.Type, X: cloneExpr(dst), Y: one}
	b.emit(Assign(syntax.NoPos, dst, n))
}

// foreignBranch builds break and continue.
//
// A label is refused rather than built. gc writes a label flat, as a statement
// of its own that the next statement of the same list is the target of, and a
// branch that names one has to be paired with it. goto and fallthrough are
// refused for the same reason, because each is a jump to a label.
func (b *builder) foreignBranch(s *export.BranchStmt) {
	if s.Labelled {
		b.refuseForeign("a %s naming the label %s", s.Op.String(), s.Label)
		return
	}
	var op Op
	switch s.Op {
	case export.OpBreak:
		op = OBreak
	case export.OpContinue:
		op = OContinue
	default:
		b.refuseForeign("the branch %q", s.Op.String())
		return
	}
	b.emit(&Node{Op: op, Type: voidType})
}

// foreignSwitch builds an expression switch.
//
// A type switch is refused: its guard declares a variable in every clause and
// each clause matches a type against an interface, which is the descriptor
// work specs/032-type-descriptors-and-itabs.md owns and which the walk over
// the syntax tree reaches through the checker's Implicits rather than through
// anything the format carries.
func (b *builder) foreignSwitch(s *export.SwitchStmt) {
	if s.Guard != nil {
		b.refuseForeign("a type switch")
		return
	}
	n := &Node{Op: OSwitch, Type: voidType}
	b.push()
	for _, init := range s.Init {
		b.foreignStmt(init)
	}
	tagType := types2.Type(types2.Typ[types2.Bool])
	if s.Tag != nil {
		xt := b.foreignTypeOf(s.Tag)
		if xt == nil {
			b.pop()
			b.refuseForeign("a switch on a value whose type the stream does not carry")
			return
		}
		tagType = types2.Default(xt)
		n.X = b.assignConv(b.foreignOperand(s.Tag), xt, tagType)
	} else {
		// A switch with no tag is a switch on true. The comparison against
		// each case is then the same operation in both forms.
		n.X = &Node{Op: OConst, Type: b.irType(tagType), Val: Const{Val: constant.MakeBool(true)}}
	}
	n.Init = b.pop()

	for i := range s.Clauses {
		c := &s.Clauses[i]
		if c.Var != nil {
			b.refuseForeign("a switch clause that declares a variable")
			return
		}
		cn := &Node{Op: OCase, Type: voidType, Index: i}
		for _, e := range c.Exprs {
			cn.Args = append(cn.Args, b.assignConv(b.foreignExpr(e), b.foreignTypeOf(e), tagType))
		}
		cn.Body = b.foreignStmts(c.Body)
		n.Body = append(n.Body, cn)
	}
	b.emit(n)
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
// Only the range form over a slice is built. A range over anything else needs
// the conversion the clause records for its key and its value, which this walk
// does not read.
func (b *builder) foreignFor(s *export.ForStmt) {
	if s.Body == nil {
		b.refuseForeign("a for statement with no body")
		return
	}
	if s.Range == nil {
		b.foreignForClauses(s)
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
	if !b.foreignPerIteration(n, decls, s.DistinctVars) {
		return
	}
	b.emit(n)
}

// foreignForClauses builds the three-clause form of a for statement.
//
// The clauses are built in the order the format wrote them, which is the order
// a variable the init statement declares is numbered in: init, condition,
// post, body. The condition and the post statement are built where they are
// evaluated again on every iteration, so nothing may be hoisted out of either.
func (b *builder) foreignForClauses(s *export.ForStmt) {
	n := &Node{Op: OFor, Type: voidType}
	b.push()
	first := len(b.foreign.locals)
	for _, init := range s.Init {
		b.foreignStmt(init)
	}
	decls := b.foreignDeclsSince(first)
	n.Init = b.pop()
	if s.Cond != nil {
		n.X = b.foreignGuarded(s.Cond)
	}
	n.Body = b.foreignStmts(s.Body.Body)
	if len(s.Post) > 0 {
		b.noHoist++
		n.Post = b.foreignStmts(s.Post)
		b.noHoist--
	}
	// After the body, because whether the address of a loop variable is taken
	// is not known until the body that takes it has been built.
	if !b.foreignPerIteration(n, decls, s.DistinctVars) {
		return
	}
	b.emit(n)
}

// foreignDeclsSince returns the variables the body declared after the frame
// held first of them, which for a loop is the variables its init statement
// declares.
//
// The frame's numbering is the list, so nothing here has to recognise the
// shape of the statement that declared them.
func (b *builder) foreignDeclsSince(first int) []*Object {
	var out []*Object
	for _, l := range b.foreign.locals[first:] {
		if l.obj != nil && l.obj.Name != "_" {
			out = append(out, l.obj)
		}
	}
	return out
}

// foreignPerIteration gives each iteration its own copy of the variables the
// loop declares, and reports whether the loop is built.
//
// A loop written before Go 1.22 shares one variable across the iterations.
// [builder.perIteration] writes the rule after it, and the two differ only for
// a variable whose address is taken, so a loop that shares one is refused
// rather than built under the other rule.
func (b *builder) foreignPerIteration(loop *Node, decls []*Object, distinct bool) bool {
	if len(decls) == 0 {
		return true
	}
	if !distinct {
		for _, o := range decls {
			if o.Addrtaken {
				b.refuseForeign("a loop that shares one address-taken variable across its iterations, which is the rule before Go 1.22")
				return false
			}
		}
	}
	b.perIteration(loop, decls)
	return true
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
	case *export.FuncLitExpr:
		return b.foreignFuncLit(e)
	default:
		b.refuseForeign("the expression %q", export.ExprKindOf(e).String())
		return b.badExpr(syntax.NoPos)
	}
}

// foreignFuncLit builds a function literal of a foreign body.
//
// A literal is two things and the format writes them apart: a body of its own,
// which is another element with its own numbering, and a capture list, which
// names variables of the body around it in that body's numbering. The list is
// taken from the format and not recomputed from the objects the literal's body
// turned out to name, for two reasons. It is gc's own closureVars, so it is
// exactly the set the literal reads from outside and it cannot be a guess. And
// it is ordered, where a set recovered here would have to be sorted by
// something: every object of a foreign body takes the unknown position, so the
// syntax walk's order, which is position then name, would put two locals of one
// name in whichever order a map produced and specs/053-determinism.md forbids
// that.
//
// The capture is by reference, which is the one mechanism
// specs/033-closures-defer-panic.md has: the object the literal names is the
// object the enclosing body names, and ir/closure.go gives it a heap cell.
func (b *builder) foreignFuncLit(e *export.FuncLitExpr) Expr {
	if e.RangeFuncBody {
		// The closure gc builds out of the body of a range over a function.
		// It returns through a hidden state variable and a runtime call that
		// no source spells, and nothing here builds either.
		b.refuseForeign("the closure gc builds out of the body of a range over a function")
		return b.badExpr(syntax.NoPos)
	}
	if e.Decoded == nil {
		b.refuseForeign("a function literal whose body the archive holds no element for")
		return b.badExpr(syntax.NoPos)
	}
	if len(e.Params)+len(e.Results) != len(e.Decoded.Params) {
		// A literal has no receiver, so its parameters and its results are the
		// whole of the locals its body opens with. A body that opens with a
		// different count is one whose numbering does not line up with its
		// signature, and every use of a local in it would name the wrong
		// variable.
		b.refuseForeign("a function literal whose body opens with %d local(s) and whose signature has %d",
			len(e.Decoded.Params), len(e.Params)+len(e.Results))
		return b.badExpr(syntax.NoPos)
	}

	// The captures are resolved against the body around the literal, which is
	// the frame that is still current here. Nothing looks upward later: a use
	// inside the literal indexes the resolved list.
	caps := make([]foreignLocal, len(e.Captured))
	objs := make([]*Object, len(e.Captured))
	for i := range e.Captured {
		l, ok := b.foreignLocalAt(&e.Captured[i].Local)
		if !ok || l.obj == nil {
			return b.badExpr(syntax.NoPos)
		}
		caps[i], objs[i] = l, l.obj
	}

	sig := b.foreignLitSignature(e)
	name, sym := b.literalNames()
	fn := &Func{Name: name, Sym: sym, Type: b.irType(sig)}
	b.isLiteral[fn] = true

	saveFn, saveSig, saveSinks, saveFree := b.fn, b.sig, b.sinks, b.free
	saveForeign := b.foreign
	b.fn, b.sig, b.sinks, b.free = fn, sig, nil, make(map[*Object]bool)
	b.foreign = &foreignFrame{
		in:       saveForeign.in,
		body:     e.Decoded,
		dict:     saveForeign.dict,
		captures: caps,
	}
	for i := 0; i < sig.Params().Len(); i++ {
		fn.Params = append(fn.Params, b.foreignParam(sig.Params().At(i), ClassParam))
	}
	for i := 0; i < sig.Results().Len(); i++ {
		fn.Results = append(fn.Results, b.foreignParam(sig.Results().At(i), ClassResult))
	}
	errs := len(b.errs)
	fn.Body = b.foreignStmts(e.Decoded.Stmts)
	b.foreignCheckNumbering(errs, e.Decoded)
	b.fn, b.sig, b.sinks, b.free = saveFn, saveSig, saveSinks, saveFree
	b.foreign = saveForeign

	// The capture list needs no check against the objects the literal turned
	// out to name. A body of the literal reaches a variable of the body around
	// it through the capture list and through nothing else, because the frame
	// above holds that list and the literal's own locals and no third one, so a
	// capture the format did not list is a use whose index is out of range and
	// is refused above.
	return b.closureNode(fn, syntax.NoPos, objs)
}

// foreignLitSignature returns the signature of a function literal of a foreign
// body.
//
// It is built from the parameter and result lists the format wrote rather than
// from the type of the expression, because the reshape node that would carry
// that type is not written in front of every literal and the two lists are.
func (b *builder) foreignLitSignature(e *export.FuncLitExpr) *types2.Signature {
	vars := func(list []export.Param) *types2.Tuple {
		out := make([]*types2.Var, len(list))
		for i, p := range list {
			out[i] = types2.NewParam(syntax.NoPos, p.Pkg, p.Name, b.foreignType(p.Type))
		}
		return types2.NewTuple(out...)
	}
	return types2.NewSignatureType(nil, nil, nil, vars(e.Params), vars(e.Results), e.Variadic)
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
	bt := b.foreignTypeOf(e)
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
		return b.foreignMethodCall(e)
	}
	if bi := foreignBuiltinOf(e.Fun); bi != nil {
		return b.foreignBuiltin(e, bi)
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

// foreignBuiltinOf returns the predeclared function a call names, and nil for
// every other callee.
//
// A builtin is not a value and has no signature, so it has to be recognised
// before the callee is built as an operand: [builder.foreignCall] would
// otherwise refuse it as a call of something with no signature, which names
// neither the builtin nor the reason.
func foreignBuiltinOf(fun export.Expr) *types2.Builtin {
	g, ok := fun.(*export.GlobalExpr)
	if !ok {
		return nil
	}
	bi, _ := g.Obj.Obj.(*types2.Builtin)
	return bi
}

// foreignBuiltin builds a call to a predeclared function.
//
// len and cap are the two that are pure operations on their operand. Every
// other builtin either allocates, writes through a pointer or reaches the
// runtime, and each of those needs the type descriptor the tree carries in
// [export.CallExpr.RType] or the shape of a composite literal, so each is
// refused by its own name.
func (b *builder) foreignBuiltin(e *export.CallExpr, bi *types2.Builtin) Expr {
	name := bi.Name()
	if pkg := bi.Pkg(); pkg != nil && pkg.Name() == "unsafe" {
		name = "unsafe." + name
	}
	var op Op
	switch name {
	case "len":
		op = OLen
	case "cap":
		op = OCap
	default:
		b.refuseForeign("a call of the builtin %s", name)
		return b.badExpr(syntax.NoPos)
	}
	if e.RType != nil {
		// The descriptor a builtin needs at run time. Neither len nor cap has
		// one, so a tree that carries one here is not the call this walk
		// recognised.
		b.refuseForeign("a call of the builtin %s carrying a type descriptor", name)
		return b.badExpr(syntax.NoPos)
	}
	if e.Dots || e.Args.Single || len(e.Args.Exprs) != 1 {
		b.refuseForeign("a call of the builtin %s with %d argument(s)", name, len(e.Args.Exprs))
		return b.badExpr(syntax.NoPos)
	}
	// The language gives both an int, which is what the stream carries when it
	// carries anything at all.
	t := b.foreignTypeOf(e)
	if t == nil {
		t = types2.Typ[types2.Int]
	}
	return &Node{Op: op, Type: b.irType(t), X: b.foreignOperand(e.Args.Exprs[0])}
}

// foreignMethodCall builds a call through a method selection.
//
// Only a method of a concrete type reached without a dictionary is built. The
// four flags below are the four ways gc writes a selection that needs one, and
// each is refused by its own name rather than as one refusal covering the
// node: a call through a dictionary reads the callee out of a slot, and
// building it as a direct call would name a method of the wrong type.
func (b *builder) foreignMethodCall(e *export.CallExpr) Expr {
	m := &e.Method.Method
	switch {
	case m.Generic:
		b.refuseForeign("a call of %s, which is a method with type parameters of its own", m.Sel.Name)
	case m.TypeParam:
		b.refuseForeign("a call of %s on a type parameter, whose callee is a dictionary slot", m.Sel.Name)
	case m.Subdict:
		b.refuseForeign("a call of %s through a subdictionary", m.Sel.Name)
	case m.StaticDict:
		b.refuseForeign("a call of %s with a dictionary argument", m.Sel.Name)
	default:
		return b.foreignConcreteMethodCall(e, m)
	}
	return b.badExpr(syntax.NoPos)
}

// foreignConcreteMethodCall builds a call of a method of a concrete type.
//
// The method is looked up again against the substituted receiver type, for the
// reason [builder.foreignField] states: the format names the method and the
// position of a method in a type is a fact about the type, and the type here is
// the substituted one. What is read out of the tree instead is the shape of the
// receiver, because the embedded fields, the dereference and the address gc
// recorded are the adjustment gc already made and re-deriving it would be a
// second answer to a question already answered.
func (b *builder) foreignConcreteMethodCall(e *export.CallExpr, m *export.MethodRef) Expr {
	recvType := b.foreignType(m.Recv)
	if recvType == nil {
		b.refuseForeign("a call of %s on a receiver whose type the stream does not carry", m.Sel.Name)
		return b.badExpr(syntax.NoPos)
	}
	if isInterface(recvType) {
		// The function is read out of the itab, which is the row
		// specs/032-type-descriptors-and-itabs.md owns.
		b.refuseForeign("a call of %s through the interface %s", m.Sel.Name, types2.TypeString(recvType, nil))
		return b.badExpr(syntax.NoPos)
	}
	// The receiver first, because that is the order the format wrote the two
	// in and a receiver the walk cannot build is a fact about the tree rather
	// than about the type.
	recv := b.foreignRecv(e.Method.Recv, recvType)
	if recv == nil {
		return b.badExpr(syntax.NoPos)
	}
	obj, index, _ := types2.LookupFieldOrMethod(recvType, true, m.Sel.Pkg, m.Sel.Name)
	fn, _ := obj.(*types2.Func)
	if fn == nil || len(index) == 0 {
		b.refuseForeign("a call of %s, which %s has no method for", m.Sel.Name, types2.TypeString(recvType, nil))
		return b.badExpr(syntax.NoPos)
	}
	fsig, _ := fn.Type().(*types2.Signature)
	if fsig == nil {
		b.refuseForeign("a call of the method %s, which has no signature", m.Sel.Name)
		return b.badExpr(syntax.NoPos)
	}
	io := b.obj(fn)
	n := &Node{Op: OCall, Type: b.resultType(fsig), X: &Node{Op: OGlobal, Type: io.Type, Obj: io}}
	if e.Dots {
		n.Index = spread
	}
	n.Args = append([]Expr{recv}, b.foreignCallArgs(e, fsig)...)
	return n
}

// foreignRecv builds the operand of a method selection, with the implicit
// field selections, dereference and address the selection applies.
//
// b.foreignOperand and not b.foreignExpr: the receiver is evaluated before the
// arguments, so a receiver that is a call goes into a temporary where it
// stands.
func (b *builder) foreignRecv(x export.Expr, want types2.Type) Expr {
	if x == nil {
		b.refuseForeign("a method selection with no receiver")
		return nil
	}
	r, ok := x.(*export.RecvExpr)
	if !ok {
		// A receiver the format wrote without the node that carries the
		// adjustment. gc writes one in front of every method selection, so a
		// tree without one is not the shape this walk read.
		b.refuseForeign("a method selection whose receiver is %q rather than a receiver node", export.ExprKindOf(x).String())
		return nil
	}
	xt := b.foreignTypeOf(r.X)
	if xt == nil {
		b.refuseForeign("a method selection on an operand with no type")
		return nil
	}
	base, t := b.fieldPath(b.foreignOperand(r.X), xt, r.Implicits)
	if r.Deref {
		p, ok := coreType(t).(*types2.Pointer)
		if !ok {
			b.refuseForeign("a method selection that dereferences %s", types2.TypeString(t, nil))
			return nil
		}
		base, t = &Node{Op: ODeref, Type: b.irType(p.Elem()), X: base}, p.Elem()
	}
	if r.Addr {
		base, t = b.addrOf(base, t), types2.NewPointer(t)
	}
	if !types2.Identical(t, want) {
		// The receiver the tree built is not the one the method's selection
		// names, which means the adjustment read out of the tree and the type
		// the tree recorded for the selection disagree. Passing it would call
		// the method with a receiver of another type.
		b.refuseForeign("a method selection whose receiver is %s where %s was recorded",
			types2.TypeString(t, nil), types2.TypeString(want, nil))
		return nil
	}
	return base
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
