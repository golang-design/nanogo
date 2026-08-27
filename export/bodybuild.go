// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"go/constant"
	"go/token"
	"strconv"
	"strings"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The body builder.
//
// It is cmd/compile/internal/noder/writer.go's function body half. gc writes
// its bitstream straight out of syntax, and this builds the tree of body.go
// instead, which bodywrite.go then encodes. The split is what makes the
// builder testable: bodywrite.go is byte exact against gc, so a tree built
// here encodes to gc's bytes exactly when the tree is gc's.
//
// The oracle is bodybuild_test.go. It builds a body out of nanogo's own parse
// and type check of a standard library file, encodes that tree and the tree
// gc's archive holds for the same function, and compares the two encodings.
// Neither tree can supply the element indices of a package nanogo is not
// writing, so both are encoded through a resolver that numbers a reference by
// what it names rather than by where it landed. What that leaves unproved is
// only the numbering, which elemRefs already proves from the other side.
//
// What the builder does not build:
//
//   - A generic declaration. Its body names types derived from the enclosing
//     type parameters, and every such name is a slot of an object dictionary
//     the writer does not fill (writer.go). [BodySource.BuildBody] refuses
//     one by name.
//   - A body gc rewrote before it wrote it. Range over a function is the one
//     case: gc turns the loop into a closure and calls into the runtime, so
//     gc's tree is not a tree of this source at all.

// A BodySource is the checker's record of one package, which is everything a
// body is built from.
//
// It holds no state of its own between bodies: two calls of BuildBody are
// independent, because a body's locals are numbered inside the body and
// nothing outside it can name one.
type BodySource struct {
	pkg  *types2.Package
	info *types2.Info
	fset *syntax.FileSet
}

// NewBodySource returns the source of bodies for one checked package.
//
// info must carry Types, Defs, Uses, Implicits, Selections and Instances: the
// builder reads a node's type, the object a name denotes, the field or method
// a selector resolves to, and the type arguments an inferred instantiation
// got, and none of the four is recoverable from syntax.
func NewBodySource(pkg *types2.Package, info *types2.Info, fset *syntax.FileSet) *BodySource {
	return &BodySource{pkg: pkg, info: info, fset: fset}
}

// BuildBody builds the tree of one function body.
//
// name is the declaration the body belongs to and is what a refusal reports.
// sig is its signature, which is what numbers the first locals. block is nil
// for a declaration with no body, which is a function implemented in assembly
// or by a linkname.
func (s *BodySource) BuildBody(name string, sig *types2.Signature, block *syntax.BlockStmt) (body *Body, err error) {
	defer func() {
		if v := recover(); v != nil {
			e, ok := v.(*BodyError)
			if !ok {
				panic(v)
			}
			body, err = nil, e
		}
	}()
	// A method of a generic type shares the type's dictionary: gc writes the
	// method inside the type's element and its body into the type's
	// dictionary, after the underlying type and after every method declared
	// before it. Nothing here has the type's element, so the slots cannot be
	// numbered, and a slot numbered against a dictionary of its own is a
	// slot gc reads as a different type.
	if sig.RecvTypeParams().Len() != 0 && sig.TypeParams().Len() == 0 {
		return nil, &BodyError{
			Package: s.pkg.Path(), Name: name, Writing: true,
			Reason: "the declaration is a method of a generic type, and its dictionary is the type's rather than its own",
		}
	}
	dict := &Dict{Pkg: s.pkg, TypeParams: typeParamSlice(sig.TypeParams())}
	if sig.Recv() != nil && sig.TypeParams().Len() != 0 {
		dict.Receivers = typeParamSlice(sig.RecvTypeParams())
	}
	s.seed(name, sig, dict)
	body, _ = s.build(name, sig, dict, block)
	body.Dict = dict
	return body, nil
}

// seed allocates the dictionary slots the declaration's own definition names,
// before the body names any.
//
// gc writes an object's definition and its body into one dictionary, and the
// definition comes first: the receiver of a generic method, then the
// parameters and the results. A derived type one of them names takes its slot
// there, so a body built without them numbers every slot it names too low.
func (s *BodySource) seed(name string, sig *types2.Signature, dict *Dict) {
	b := &bodyBuilder{s: s, name: name, sig: sig, dict: dict}
	// A generic method writes its receiver before its own type parameters
	// and before its signature. Every other declaration writes no receiver
	// at this point: a method of a non-generic type has no derived type to
	// name, and a method of a generic type is refused above.
	if recv := sig.Recv(); recv != nil && sig.TypeParams().Len() != 0 {
		b.derivedOf(recv.Type())
	}
	b.walkSignature(sig)
}

// typeParamSlice unpacks a type parameter list.
func typeParamSlice(l *types2.TypeParamList) []*types2.TypeParam {
	if l == nil || l.Len() == 0 {
		return nil
	}
	out := make([]*types2.TypeParam, l.Len())
	for i := range out {
		out[i] = l.At(i)
	}
	return out
}

// build builds one body and returns the variables it captured from outside
// itself.
//
// A declaration captures nothing. A function literal captures whatever its
// body named and did not declare, and the builder of the enclosing body turns
// each into a reference in its own numbering, which is why the list comes
// back rather than being resolved here.
func (s *BodySource) build(name string, sig *types2.Signature, dict *Dict, block *syntax.BlockStmt) (*Body, []freeVar) {
	b := &bodyBuilder{s: s, name: name, sig: sig, dict: dict}
	body := &Body{Params: b.declareParams(sig)}
	if block != nil {
		body.HasBlock = true
		body.Stmts = b.stmts(block.List)
		body.Rbrace = b.pos(block.Rbrace)
	}
	return body, b.free
}

// bodyBuilder builds one body's tree.
type bodyBuilder struct {
	s    *BodySource
	name string
	sig  *types2.Signature

	// dict is the dictionary of the declaration this body belongs to. A
	// function literal shares the dictionary of the declaration that
	// encloses it, because gc writes both into one.
	dict *Dict

	// locals numbers each variable this body declares, in declaration
	// order. A local is referred to by its number and by nothing else.
	locals map[*types2.Var]int

	// free numbers each variable this body names and does not declare, in
	// first-use order, and freeIdx is its reverse.
	free    []freeVar
	freeIdx map[*types2.Var]int
}

// A freeVar is one variable a body names and does not declare, with the
// position of the use that first named it.
type freeVar struct {
	pos syntax.Pos
	obj *types2.Var
}

// refuse reports a body the builder cannot build.
func (b *bodyBuilder) refuse(format string, args ...any) {
	panic(&BodyError{
		Package: b.s.pkg.Path(), Name: b.name, Writing: true,
		Reason: fmt.Sprintf(format, args...),
	})
}

// @@@ Positions

// pos resolves one source position.
//
// The file name is the one the position resolves to under the //line
// directive in force, because that is the file gc measures the line and the
// column in.
func (b *bodyBuilder) pos(p syntax.Pos) Pos {
	if !p.IsKnown() {
		return Pos{}
	}
	at := b.s.fset.Position(p)
	return Pos{Known: true, File: at.Filename, Line: at.Line, Col: at.Col}
}

// @@@ References

// typeUse names one type.
//
// A type whose identity depends on the enclosing declaration's type
// parameters is a slot of that declaration's dictionary, and Idx is the slot.
// Otherwise Idx is left zero: the element that holds the type belongs to the
// package being written, and the builder is not writing it. The resolver the
// encoder asks answers with the index.
func (b *bodyBuilder) typeUse(typ types2.Type) TypeUse {
	if typ == nil {
		b.refuse("a type the encoding requires is absent")
	}
	slot, derived := b.deriveType(typ)
	if !derived {
		return TypeUse{Type: typ}
	}
	if !b.dict.Generic() {
		// Nothing this body names can be derived, because the declaration
		// has no type parameter for a type to depend on. Reaching here is a
		// defect in the builder, and writing the slot anyway is a type gc
		// resolves against an empty dictionary without complaint.
		b.refuse("the type %v is derived from a type parameter the declaration does not declare", typ)
	}
	return TypeUse{Derived: true, Idx: pkgbits.Index(slot), Type: typ}
}

// deriveType allocates the dictionary slot of a derived type, reporting what
// the dictionary cannot hold as a refusal of the body.
func (b *bodyBuilder) deriveType(typ types2.Type) (int, bool) {
	slot, derived, err := b.dict.Derive(typ)
	if err != nil {
		b.refuse("%s", err)
	}
	return slot, derived
}

// derivedOf reports whether a type's identity depends on a type parameter,
// allocating its dictionary slot when it does.
func (b *bodyBuilder) derivedOf(typ types2.Type) bool {
	_, ok := b.deriveType(typ)
	return ok
}

// walkSignature allocates the slots a signature's parameters and results
// name, which is what seeding a declaration's dictionary walks.
func (b *bodyBuilder) walkSignature(sig *types2.Signature) {
	if _, err := b.dict.walkSignature(sig); err != nil {
		b.refuse("%s", err)
	}
}

// objUse names one package-scope declaration, with the type arguments it is
// instantiated with.
func (b *bodyBuilder) objUse(obj types2.Object, explicits []types2.Type) ObjUse {
	use := ObjUse{Pkg: obj.Pkg(), Name: obj.Name(), Obj: obj}
	for _, t := range explicits {
		use.Targs = append(use.Targs, b.typeUse(t))
	}
	return use
}

// rtype names the runtime type descriptor a body needs at run time.
//
// A descriptor of a derived type is a slot of the dictionary, because the
// type is not known until the instantiation is.
func (b *bodyBuilder) rtype(typ types2.Type) RType {
	return b.rtypeOf(b.typeUse(types2.Default(typ)))
}

// rtypeOf names the descriptor of a type already named.
func (b *bodyBuilder) rtypeOf(t TypeUse) RType {
	if t.Derived {
		return RType{Derived: true, DictIdx: b.dict.RTypeIndex(t)}
	}
	return RType{Type: t}
}

// convRTTI names the pair of descriptors a conversion needs at run time.
//
// Both types are named before either descriptor is, which is the order gc
// writes them in and so the order the derived slots are allocated in. The two
// lists are separate, so only the order within each one is observable.
func (b *bodyBuilder) convRTTI(src, dst types2.Type) ConvRTTI {
	from := b.typeUse(types2.Default(src))
	to := b.typeUse(types2.Default(dst))
	out := ConvRTTI{Src: b.rtypeOf(from), Dst: b.rtypeOf(to)}
	if from.Derived || to.Derived {
		out.Derived = true
		out.DictIdx = b.dict.ItabIndex(from, to)
	}
	return out
}

// @@@ The checker's record

// typeAndValue returns what the checker recorded for an expression.
func (b *bodyBuilder) typeAndValue(e syntax.Expr) (types2.TypeAndValue, bool) {
	tv, ok := b.s.info.Types[e]
	if !ok {
		return tv, false
	}
	// A generic function whose type arguments were inferred from the
	// assignment context has its instantiated type in Instances and its
	// uninstantiated one here.
	if name, ok := e.(*syntax.Name); ok {
		if inst, ok := b.s.info.Instances[name]; ok {
			tv.Type = inst.Type
		}
	}
	return tv, tv.Type != nil
}

// typeOf returns the type of an expression that produces a value.
func (b *bodyBuilder) typeOf(e syntax.Expr) types2.Type {
	tv, ok := b.typeAndValue(e)
	if !ok {
		b.refuse("the checker recorded no type for %s", syntax.String(e))
	}
	if !tv.IsValue() {
		b.refuse("%s is not a value", syntax.String(e))
	}
	return tv.Type
}

// lookupObj returns the declaration an expression names, and the
// instantiation it names it at.
func (b *bodyBuilder) lookupObj(e syntax.Expr) (types2.Object, types2.Instance) {
	if index, ok := e.(*syntax.IndexExpr); ok {
		args := syntax.UnpackListExpr(index.Index)
		if len(args) == 1 {
			if tv, ok := b.typeAndValue(args[0]); ok && tv.IsValue() {
				return nil, types2.Instance{} // an ordinary index
			}
		}
		e = index.X
	}
	if sel, ok := e.(*syntax.SelectorExpr); ok {
		name, ok := sel.X.(*syntax.Name)
		if !ok {
			return nil, types2.Instance{}
		}
		if _, isPkg := b.s.info.Uses[name].(*types2.PkgName); !isPkg {
			return nil, types2.Instance{} // an ordinary selector
		}
		e = sel.Sel
	}
	if name, ok := e.(*syntax.Name); ok {
		return b.s.info.Uses[name], b.s.info.Instances[name]
	}
	return nil, types2.Instance{}
}

// @@@ Locals

// declareParams declares the receiver, the parameters and the results, in
// that order, which is the numbering every reference to a local uses.
func (b *bodyBuilder) declareParams(sig *types2.Signature) []Local {
	var out []Local
	if recv := sig.Recv(); recv != nil {
		out = append(out, b.addLocal(recv))
	}
	for _, params := range []*types2.Tuple{sig.Params(), sig.Results()} {
		for i := range params.Len() {
			out = append(out, b.addLocal(params.At(i)))
		}
	}
	return out
}

// addLocal records the declaration of one local variable.
func (b *bodyBuilder) addLocal(v *types2.Var) Local {
	if b.locals == nil {
		b.locals = make(map[*types2.Var]int)
	}
	b.locals[v] = len(b.locals)
	// A local whose type is derived from the enclosing type parameters
	// carries the slot its runtime type descriptor is in, because the
	// reader needs the descriptor to lay the frame out and the type is not
	// known until the instantiation is.
	if t := b.typeUse(v.Type()); t.Derived {
		return Local{DictRType: b.dict.RTypeIndex(t)}
	}
	return Local{DictRType: -1}
}

// useLocal names one local or captured variable in this body's numbering.
//
// A variable this body did not declare is captured: it is numbered in this
// body's capture list, and the builder of the enclosing body resolves it
// again in its own numbering when it builds the literal.
func (b *bodyBuilder) useLocal(pos syntax.Pos, v *types2.Var) LocalExpr {
	if idx, ok := b.locals[v]; ok {
		return LocalExpr{Index: idx}
	}
	idx, ok := b.freeIdx[v]
	if !ok {
		if b.freeIdx == nil {
			b.freeIdx = make(map[*types2.Var]int)
		}
		idx = len(b.free)
		b.free = append(b.free, freeVar{pos, v})
		b.freeIdx[v] = idx
	}
	return LocalExpr{Captured: true, Index: idx}
}

// @@@ Statements

// stmts builds a statement list.
//
// A statement after one that cannot fall through is dropped, which is what gc
// writes, with one exception it makes for itself: a label later in the list
// can be jumped to, so nothing before the last label is dropped.
func (b *bodyBuilder) stmts(list []syntax.Stmt) []Stmt {
	lastLabel := -1
	for i, s := range list {
		if _, ok := s.(*syntax.LabeledStmt); ok {
			lastLabel = i
		}
	}
	var out []Stmt
	dead := false
	for i, s := range list {
		if dead && i > lastLabel {
			if _, ok := s.(*syntax.LabeledStmt); !ok {
				continue
			}
		}
		out = append(out, b.stmt1(s)...)
		dead = b.terminates(s)
	}
	return out
}

// stmt builds a statement that may be absent, as a list, which is the shape
// the encoding gives an initialiser and a post statement.
func (b *bodyBuilder) stmt(s syntax.Stmt) []Stmt {
	if s == nil {
		return nil
	}
	return b.stmts([]syntax.Stmt{s})
}

// stmt1 builds one statement, which is a list because a declaration of
// several variables is one statement of the source and several of the
// encoding.
func (b *bodyBuilder) stmt1(s syntax.Stmt) []Stmt {
	switch s := s.(type) {
	default:
		b.refuse("the statement is a %T, which the builder does not build", s)
		panic("unreachable")

	case nil, *syntax.EmptyStmt:
		return nil

	case *syntax.AssignStmt:
		switch {
		// nanogo's parser gives ++ and -- the shared [syntax.ImplicitOne]
		// as their right side where gc's gives them none, so an increment
		// is recognised by identity and not by absence.
		case s.Rhs == nil || s.Rhs == syntax.ImplicitOne:
			return []Stmt{&IncDecStmt{Op: binOp(b, s.Op), X: b.expr(s.Lhs), Pos: b.pos(s.Pos())}}

		case s.Op != 0 && s.Op != syntax.Def:
			out := &AssignOpStmt{Op: binOp(b, s.Op), Lhs: b.expr(s.Lhs), Pos: b.pos(s.Pos())}
			// A shift's operands are allowed to differ, so the right
			// operand is not converted to the left one's type.
			var typ types2.Type
			if s.Op != syntax.Shl && s.Op != syntax.Shr {
				typ = b.typeOf(s.Lhs)
			}
			out.Rhs = b.implicitConv(typ, s.Rhs)
			return []Stmt{out}

		default:
			return []Stmt{b.assignStmt(s.Pos(), s.Lhs, s.Rhs)}
		}

	case *syntax.BlockStmt:
		return []Stmt{b.blockStmt(s)}

	case *syntax.BranchStmt:
		out := &BranchStmt{Pos: b.pos(s.Pos())}
		switch s.Tok {
		case syntax.Break:
			out.Op = OpBreak
		case syntax.Continue:
			out.Op = OpContinue
		case syntax.Fallthrough:
			out.Op = OpFall
		case syntax.Goto:
			out.Op = OpGoto
		default:
			b.refuse("the branch statement is %v, which the format has no operator for", s.Tok)
		}
		if s.Label != nil {
			out.Labelled, out.Label = true, s.Label.Value
		}
		return []Stmt{out}

	case *syntax.CallStmt:
		out := &CallStmt{Pos: b.pos(s.Pos())}
		switch s.Tok {
		case syntax.Defer:
			out.Op = OpDefer
		case syntax.Go:
			out.Op = OpGo
		default:
			b.refuse("the call statement is %v, which the format has no operator for", s.Tok)
		}
		out.Call = b.expr(s.Call)
		if s.Tok == syntax.Defer && s.DeferAt != nil {
			out.DeferAt = b.expr(s.DeferAt)
		}
		return []Stmt{out}

	case *syntax.DeclStmt:
		var out []Stmt
		for _, d := range s.DeclList {
			switch d := d.(type) {
			default:
				b.refuse("the declaration is a %T, which the format has no encoding for", d)
			case *syntax.ConstDecl, *syntax.TypeDecl:
				// Neither reaches the body: a constant is folded into
				// every use and a type is named by every use.
			case *syntax.VarDecl:
				out = append(out, b.assignStmt(d.Pos(), namesAsExpr(d.NameList), d.Values))
			}
		}
		return out

	case *syntax.ExprStmt:
		return []Stmt{&ExprStmt{X: b.expr(s.X)}}

	case *syntax.ForStmt:
		return []Stmt{b.forStmt(s)}

	case *syntax.IfStmt:
		return []Stmt{b.ifStmt(s)}

	case *syntax.LabeledStmt:
		out := []Stmt{&LabelStmt{Pos: b.pos(s.Pos()), Label: s.Label.Value}}
		return append(out, b.stmt1(s.Stmt)...)

	case *syntax.ReturnStmt:
		out := &ReturnStmt{Pos: b.pos(s.Pos())}
		results := b.sig.Results()
		out.Results = b.multiExpr(s.Pos(), func(i int) types2.Type {
			return results.At(i).Type()
		}, syntax.UnpackListExpr(s.Results))
		return []Stmt{out}

	case *syntax.SelectStmt:
		return []Stmt{b.selectStmt(s)}

	case *syntax.SendStmt:
		ch, ok := types2.CoreType(b.typeOf(s.Chan)).(*types2.Chan)
		if !ok {
			b.refuse("the operand of a send is not a channel")
		}
		return []Stmt{&SendStmt{
			Pos:   b.pos(s.Pos()),
			Chan:  b.expr(s.Chan),
			Value: b.implicitConv(ch.Elem(), s.Value),
		}}

	case *syntax.SwitchStmt:
		return []Stmt{b.switchStmt(s)}
	}
}

// assignList builds the destinations of an assignment.
func (b *bodyBuilder) assignList(list []syntax.Expr) []Assignee {
	out := make([]Assignee, len(list))
	for i, e := range list {
		out[i] = b.assign(e)
	}
	return out
}

// assign builds one destination of an assignment.
//
// A name the checker recorded in Defs is a declaration, and the local it
// declares is numbered here rather than where it comes into scope, which is
// what gc does.
func (b *bodyBuilder) assign(e syntax.Expr) Assignee {
	e = syntax.Unparen(e)
	if name, ok := e.(*syntax.Name); ok {
		if name.Value == "_" {
			return Assignee{Kind: AssignBlank}
		}
		if obj, ok := b.s.info.Defs[name]; ok {
			v, ok := obj.(*types2.Var)
			if !ok {
				b.refuse("the assignment declares a %T", obj)
			}
			return Assignee{
				Kind:  AssignDef,
				Pos:   b.pos(v.Pos()),
				Pkg:   v.Pkg(),
				Name:  v.Name(),
				Type:  b.typeUse(v.Type()),
				Local: b.addLocal(v),
			}
		}
	}
	return Assignee{Kind: AssignExpr, Expr: b.expr(e)}
}

// assignStmt builds an assignment of one list to another.
func (b *bodyBuilder) assignStmt(pos syntax.Pos, lhs0, rhs0 syntax.Expr) Stmt {
	lhs := syntax.UnpackListExpr(lhs0)
	rhs := syntax.UnpackListExpr(rhs0)

	out := &AssignStmt{Pos: b.pos(pos), Lhs: b.assignList(lhs)}
	out.Rhs = b.multiExpr(pos, func(i int) types2.Type { return b.destType(lhs[i]) }, rhs)
	return out
}

// destType returns the type a value is assigned to.
//
// The names of a var declaration are in Defs and the names of an ordinary
// assignment are in Uses, and neither is in Types, so a destination is not
// resolved the way an expression is.
func (b *bodyBuilder) destType(dst syntax.Expr) types2.Type {
	if name, ok := syntax.Unparen(dst).(*syntax.Name); ok {
		switch {
		case name.Value == "_":
			return nil // blank converts nothing
		case b.s.info.Defs[name] != nil:
			if v, ok := b.s.info.Defs[name].(*types2.Var); ok {
				return v.Type()
			}
		case b.s.info.Uses[name] != nil:
			if v, ok := b.s.info.Uses[name].(*types2.Var); ok {
				return v.Type()
			}
		}
		b.refuse("the destination %s has no object", name.Value)
	}
	return b.typeOf(dst)
}

// blockStmt builds a block and the scope it opens.
func (b *bodyBuilder) blockStmt(s *syntax.BlockStmt) *BlockStmt {
	return &BlockStmt{
		Open:  b.pos(s.Pos()),
		Body:  b.stmts(s.List),
		Close: b.pos(s.Rbrace),
	}
}

// ifStmt builds an if statement.
//
// gc drops the arm it proved unreachable, so a constant condition produces a
// tree with one arm and the other absent.
func (b *bodyBuilder) ifStmt(s *syntax.IfStmt) *IfStmt {
	cond := b.staticBool(&s.Cond)
	out := &IfStmt{
		Open:   b.pos(s.Pos()),
		Pos:    b.pos(s.Pos()),
		Init:   b.stmt(s.Init),
		Cond:   b.expr(s.Cond),
		Static: cond,
	}
	if cond >= 0 {
		out.Then = b.blockStmt(s.Then)
	} else {
		out.ThenClose = b.pos(s.Then.Rbrace)
	}
	if cond <= 0 {
		out.Else = b.stmt(s.Else)
	}
	return out
}

// forStmt builds a for statement in either of its two forms.
func (b *bodyBuilder) forStmt(s *syntax.ForStmt) *ForStmt {
	out := &ForStmt{Open: b.pos(s.Pos())}
	if rang, ok := s.Init.(*syntax.RangeClause); ok {
		out.Range = b.rangeClause(rang)
	} else {
		// A loop whose condition is constantly false runs nothing, and gc
		// writes neither the post statement nor the body.
		if s.Cond != nil && b.staticBool(&s.Cond) < 0 {
			s.Post = nil
			s.Body.List = nil
		}
		out.Pos = b.pos(s.Pos())
		out.Init = b.stmt(s.Init)
		if s.Cond != nil {
			out.Cond = b.expr(s.Cond)
		}
		out.Post = b.stmt(s.Post)
	}
	out.Body = b.blockStmt(s.Body)
	out.DistinctVars = b.distinctVars(s)
	return out
}

// rangeClause builds the range form of a for statement.
func (b *bodyBuilder) rangeClause(c *syntax.RangeClause) *RangeClause {
	xtyp := b.typeOf(c.X)
	if sig, ok := types2.CoreType(xtyp).(*types2.Signature); ok {
		_ = sig
		b.refuse("the loop ranges over a function, which gc rewrites into a closure before it writes the body")
	}
	lhs := syntax.UnpackListExpr(c.Lhs)
	out := &RangeClause{Pos: b.pos(c.Pos()), Lhs: b.assignList(lhs), X: b.expr(c.X)}
	if _, isMap := types2.CoreType(xtyp).(*types2.Map); isMap {
		rt := b.rtype(xtyp)
		out.MapRType = &rt
	}

	keyType, valueType := types2.RangeKeyVal(xtyp)
	conv := func(i int, src types2.Type) *ConvRTTI {
		if i >= len(lhs) {
			return nil
		}
		dst := syntax.Unparen(lhs[i])
		if name, ok := dst.(*syntax.Name); ok && name.Value == "_" {
			return nil
		}
		var dstType types2.Type
		if c.Def {
			// The names of a := clause are in Defs and not in Types.
			name, ok := dst.(*syntax.Name)
			if !ok {
				b.refuse("the range clause declares a %T", dst)
			}
			v, ok := b.s.info.Defs[name].(*types2.Var)
			if !ok {
				b.refuse("the range clause declares %s with no object", name.Value)
			}
			dstType = v.Type()
		} else {
			dstType = b.typeOf(dst)
		}
		rt := b.convRTTI(src, dstType)
		return &rt
	}
	out.KeyConv = conv(0, keyType)
	out.ValueConv = conv(1, valueType)
	return out
}

// distinctVars reports whether the loop declares its variables anew on every
// iteration, which is the Go 1.22 rule and is decided per file.
func (b *bodyBuilder) distinctVars(s *syntax.ForStmt) bool {
	file := b.s.fset.SrcFile(s.Pos())
	if file == nil {
		return true
	}
	v := b.s.info.FileVersions[file]
	return v == "" || goVersionAtLeast(v, 22)
}

// goVersionAtLeast reports whether a "go1.N" version string is at least
// go1.minor. A string in any other shape is treated as the current release,
// which is what a file with no version of its own gets.
func goVersionAtLeast(v string, minor int) bool {
	rest, ok := strings.CutPrefix(v, "go1.")
	if !ok {
		return true
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return true
	}
	return n >= minor
}

// selectStmt builds a select statement.
func (b *bodyBuilder) selectStmt(s *syntax.SelectStmt) *SelectStmt {
	out := &SelectStmt{Pos: b.pos(s.Pos())}
	out.Clauses = make([]CommClause, len(s.Body))
	for i, clause := range s.Body {
		c := &out.Clauses[i]
		if i > 0 {
			p := b.pos(clause.Pos())
			c.ScopeClose = &p
		}
		c.ScopeOpen = b.pos(clause.Pos())
		c.Pos = b.pos(clause.Pos())
		c.Comm = b.stmt(clause.Comm)
		c.Body = b.stmts(clause.Body)
	}
	if len(s.Body) > 0 {
		out.Close = b.pos(s.Rbrace)
	}
	return out
}

// switchStmt builds an expression switch or a type switch.
//
// gc rewrites a switch on a constant tag into the one clause it always takes,
// and widens the tag's type to the empty interface where a case cannot be
// converted to it, so the tree is not always a tree of the source.
func (b *bodyBuilder) switchStmt(s *syntax.SwitchStmt) *SwitchStmt {
	out := &SwitchStmt{
		Open: b.pos(s.Pos()),
		Pos:  b.pos(s.Pos()),
		Init: b.stmt(s.Init),
	}

	var iface, tagType types2.Type
	tagTypeIsChan := false
	if guard, ok := s.Tag.(*syntax.TypeSwitchGuard); ok {
		iface = b.typeOf(guard.X)
		g := &TypeSwitchGuard{Pos: b.pos(guard.Pos())}
		if guard.Lhs != nil {
			g.Named = true
			g.NamePos = b.pos(guard.Lhs.Pos())
			// The guard's variable has no object of its own: each clause
			// declares one with the clause's type.
			g.Pkg = b.s.pkg
			g.Name = guard.Lhs.Value
		}
		g.X = b.expr(guard.X)
		out.Guard = g
	} else {
		tag := s.Tag
		var tagValue constant.Value
		if tag != nil {
			tv, ok := b.typeAndValue(tag)
			if !ok {
				b.refuse("the switch tag has no type")
			}
			tagType, tagValue = tv.Type, tv.Value
			_, tagTypeIsChan = tagType.Underlying().(*types2.Chan)
		} else {
			tagType = types2.Typ[types2.Bool]
			tagValue = constant.MakeBool(true)
		}

		if tagValue != nil {
			if b.foldSwitch(s, tagValue) {
				tag, s.Tag, tagType = nil, nil, types2.Typ[types2.Bool]
			}
		}

		// gc compares the tag with each case at one type, so a case that
		// is not assignable to the tag's type widens both to the empty
		// interface. A channel is left alone, because wrapping a channel
		// in an interface changes what the comparison means.
		if !tagTypeIsChan {
		Outer:
			for _, clause := range s.Body {
				for _, cas := range syntax.UnpackListExpr(clause.Cases) {
					casType := b.typeOf(cas)
					if !types2.AssignableTo(casType, tagType) && (types2.IsInterface(casType) || types2.IsInterface(tagType)) {
						tagType = types2.NewInterfaceType(nil, nil)
						break Outer
					}
				}
			}
		}
		if tag != nil {
			out.Tag = b.implicitConv(tagType, tag)
		}
	}

	out.Clauses = make([]CaseClause, len(s.Body))
	for i, clause := range s.Body {
		c := &out.Clauses[i]
		if i > 0 {
			p := b.pos(clause.Pos())
			c.ScopeClose = &p
		}
		c.ScopeOpen = b.pos(clause.Pos())
		c.Pos = b.pos(clause.Pos())

		cases := syntax.UnpackListExpr(clause.Cases)
		if iface != nil {
			c.Types = make([]*ExprType, len(cases))
			for j, cas := range cases {
				if tv, ok := b.typeAndValue(cas); ok && tv.IsNil() {
					continue // the case nil
				}
				c.Types[j] = b.exprType(iface, cas)
			}
		} else {
			c.Exprs = make([]Expr, len(cases))
			for j, cas := range cases {
				typ := tagType
				if tagTypeIsChan {
					typ = nil
				}
				c.Exprs[j] = b.implicitConv(typ, cas)
			}
		}

		if obj, ok := b.s.info.Implicits[clause]; ok {
			// The variable a named guard declares for this clause. Its
			// position is the end of the last type the clause names, which
			// is what puts it in the right scope for the debug information.
			pos := clause.Pos()
			if typs := syntax.UnpackListExpr(clause.Cases); len(typs) != 0 {
				pos = typeExprEndPos(b, typs[len(typs)-1])
			}
			v, ok := obj.(*types2.Var)
			if !ok {
				b.refuse("the clause of a type switch declares a %T", obj)
			}
			c.VarPos = b.pos(pos)
			c.VarType = b.typeUse(v.Type())
			l := b.addLocal(v)
			c.Var = &l
		}

		c.Body = b.stmts(clause.Body)
	}
	if len(s.Body) > 0 {
		out.ClausesClose = b.pos(s.Rbrace)
	}
	out.Close = b.pos(s.Rbrace)
	return out
}

// foldSwitch rewrites a switch on a constant tag into the one clause it
// always takes, and reports whether it did.
//
// gc does this in place, so the tree is of the rewritten switch. A case whose
// value is not constant defeats it, because which clause runs then depends on
// a comparison. A clause that falls through defeats it too.
func (b *bodyBuilder) foldSwitch(s *syntax.SwitchStmt, tagValue constant.Value) bool {
	var target *syntax.CaseClause
	found := false
	for _, clause := range s.Body {
		if clause.Cases == nil {
			target = clause
		}
		for _, cas := range syntax.UnpackListExpr(clause.Cases) {
			tv, ok := b.typeAndValue(cas)
			if !ok || tv.Value == nil {
				return false // a case that is not constant
			}
			if constant.Compare(tagValue, token.EQL, tv.Value) {
				target, found = clause, true
				break
			}
		}
		if found {
			break
		}
	}
	if target == nil {
		s.Body = nil
		return true
	}
	if hasFallthrough(target.Body) {
		return false
	}
	target.Cases = nil
	s.Body = []*syntax.CaseClause{target}
	return true
}

// exprType builds a type written where an expression is expected.
//
// A type matched against a non-empty interface needs the pair of descriptors
// the match compares, and every other one needs the type's own descriptor.
func (b *bodyBuilder) exprType(iface types2.Type, e syntax.Expr) *ExprType {
	tv, ok := b.typeAndValue(e)
	if !ok || !tv.IsType() {
		b.refuse("%s is not a type", syntax.String(e))
	}
	out := &ExprType{Pos: b.pos(e.Pos())}
	if iface != nil && !iface.Underlying().(*types2.Interface).Empty() {
		conv := b.convRTTI(tv.Type, iface)
		out.Itab = &conv
		return out
	}
	rt := b.rtype(tv.Type)
	out.RType = &rt
	// Whether the type itself is derived, which the reader needs in order to
	// know that the case matches a type it can only name through the
	// dictionary.
	out.Derived = b.derivedOf(tv.Type)
	return out
}

// hasFallthrough reports whether a clause ends in fallthrough.
func hasFallthrough(list []syntax.Stmt) bool {
	s := lastNonEmptyStmt(list)
	for {
		ls, ok := s.(*syntax.LabeledStmt)
		if !ok {
			break
		}
		s = ls.Stmt
	}
	br, ok := s.(*syntax.BranchStmt)
	return ok && br.Tok == syntax.Fallthrough
}

// namesAsExpr turns the names of a var declaration into the expression an
// assignment's left side has.
func namesAsExpr(names []*syntax.Name) syntax.Expr {
	if len(names) == 1 {
		return names[0]
	}
	list := make([]syntax.Expr, len(names))
	for i, name := range names {
		list[i] = name
	}
	return &syntax.ListExpr{ElemList: list}
}

// typeExprEndPos returns the position gc gives the variable a type switch
// clause declares, which is the start of the last name in the clause's type.
func typeExprEndPos(b *bodyBuilder, e syntax.Expr) syntax.Pos {
	for {
		switch x := e.(type) {
		case *syntax.Name:
			return x.Pos()
		case *syntax.SelectorExpr:
			return x.X.Pos()
		case *syntax.ParenExpr:
			e = x.X
		case *syntax.Operation:
			e = x.X
		case *syntax.ArrayType:
			e = x.Elem
		case *syntax.ChanType:
			e = x.Elem
		case *syntax.DotsType:
			e = x.Elem
		case *syntax.MapType:
			e = x.Value
		case *syntax.SliceType:
			e = x.Elem
		case *syntax.StructType:
			return x.Pos()
		case *syntax.InterfaceType:
			e = lastFieldType(x.MethodList)
			if e == nil {
				return x.Pos()
			}
		case *syntax.FuncType:
			e = lastFieldType(x.ResultList)
			if e == nil {
				if e = lastFieldType(x.ParamList); e == nil {
					return x.Pos()
				}
			}
		case *syntax.IndexExpr:
			targs := syntax.UnpackListExpr(x.Index)
			e = targs[len(targs)-1]
		default:
			b.refuse("the type expression is a %T, which has no end position", e)
		}
	}
}

func lastFieldType(fields []*syntax.Field) syntax.Expr {
	if len(fields) == 0 {
		return nil
	}
	return fields[len(fields)-1].Type
}

// binOp returns the operator gc gives a binary operation.
func binOp(b *bodyBuilder, op syntax.Operator) Op {
	if out, ok := binOps[op]; ok {
		return out
	}
	b.refuse("the binary operator %v has no encoding", op)
	panic("unreachable")
}

// unOp returns the operator gc gives a unary operation.
func unOp(b *bodyBuilder, op syntax.Operator) Op {
	if out, ok := unOps[op]; ok {
		return out
	}
	b.refuse("the unary operator %v has no encoding", op)
	panic("unreachable")
}

// The operators, which are gc's noder tables.
var (
	unOps = map[syntax.Operator]Op{
		syntax.Recv: OpRecv,
		syntax.Mul:  OpDeref,
		syntax.And:  OpAddr,
		syntax.Not:  OpNot,
		syntax.Xor:  OpBitNot,
		syntax.Add:  OpPlus,
		syntax.Sub:  OpNeg,
	}
	binOps = map[syntax.Operator]Op{
		syntax.OrOr:   OpOrOr,
		syntax.AndAnd: OpAndAnd,
		syntax.Eql:    OpEq,
		syntax.Neq:    OpNe,
		syntax.Lss:    OpLt,
		syntax.Leq:    OpLe,
		syntax.Gtr:    OpGt,
		syntax.Geq:    OpGe,
		syntax.Add:    OpAdd,
		syntax.Sub:    OpSub,
		syntax.Or:     OpOr,
		syntax.Xor:    OpXor,
		syntax.Mul:    OpMul,
		syntax.Div:    OpDiv,
		syntax.Rem:    OpMod,
		syntax.And:    OpAnd,
		syntax.AndNot: OpAndNot,
		syntax.Shl:    OpLsh,
		syntax.Shr:    OpRsh,
	}
)

// terminates reports whether a statement cannot fall through, which is what
// makes the statements after it dead.
func (b *bodyBuilder) terminates(s syntax.Stmt) bool {
	switch s := s.(type) {
	case *syntax.BranchStmt:
		return s.Tok == syntax.Goto
	case *syntax.ReturnStmt:
		return true
	case *syntax.ExprStmt:
		call, ok := syntax.Unparen(s.X).(*syntax.CallExpr)
		return ok && b.isBuiltin(call.Fun, "panic")
	case *syntax.IfStmt:
		cond := b.staticBool(&s.Cond)
		return (cond < 0 || b.terminates(s.Then)) && (cond > 0 || b.terminates(s.Else))
	case *syntax.BlockStmt:
		return b.terminates(lastNonEmptyStmt(s.List))
	}
	return false
}

// lastNonEmptyStmt returns the last statement of a list that is not the empty
// statement, and nil for a list with none.
func lastNonEmptyStmt(list []syntax.Stmt) syntax.Stmt {
	for i := len(list) - 1; i >= 0; i-- {
		if _, empty := list[i].(*syntax.EmptyStmt); !empty {
			return list[i]
		}
	}
	return nil
}

// isBuiltin reports whether an expression names the given predeclared
// function.
func (b *bodyBuilder) isBuiltin(e syntax.Expr, name string) bool {
	if n, ok := syntax.Unparen(e).(*syntax.Name); ok && n.Value == name {
		tv, ok := b.typeAndValue(n)
		return ok && tv.IsBuiltin()
	}
	return false
}

// staticBool reports whether a condition is always true (a positive result),
// always false (a negative one), or neither (zero).
//
// It rewrites the condition where an operand is constant, because gc writes
// the rewritten condition and not the source's.
func (b *bodyBuilder) staticBool(ep *syntax.Expr) int {
	if *ep == nil {
		return 0
	}
	if tv, ok := b.typeAndValue(*ep); ok && tv.Value != nil {
		if constantBool(tv.Value) {
			return +1
		}
		return -1
	}
	e, ok := (*ep).(*syntax.Operation)
	if !ok {
		return 0
	}
	hasValue := func(x syntax.Expr) bool {
		tv, ok := b.typeAndValue(x)
		return ok && tv.Value != nil
	}
	switch e.Op {
	case syntax.Not:
		// gc returns the operand's own result and does not negate it. That
		// is reproduced and not corrected: the value decides which arm of
		// an if statement gc wrote, so a sign this does not share is an arm
		// that disagrees with the one the element holds.
		return b.staticBool(&e.X)

	case syntax.AndAnd:
		x := b.staticBool(&e.X)
		if x < 0 {
			*ep = e.X
			return x
		}
		y := b.staticBool(&e.Y)
		if x > 0 || y < 0 {
			if hasValue(e.X) {
				*ep = e.Y
			}
			return y
		}

	case syntax.OrOr:
		x := b.staticBool(&e.X)
		if x > 0 {
			*ep = e.X
			return x
		}
		y := b.staticBool(&e.Y)
		if x < 0 || y > 0 {
			if hasValue(e.X) {
				*ep = e.Y
			}
			return y
		}
	}
	return 0
}

// @@@ Expressions

// multiExpr builds a list of values.
//
// One expression of a tuple type spread over several values is the N:1 form,
// and one expression per value is the N:N form. dstType names the type each
// value is assigned to, and is nil where the value is not assigned to
// anything.
func (b *bodyBuilder) multiExpr(pos syntax.Pos, dstType func(int) types2.Type, exprs []syntax.Expr) MultiExpr {
	if len(exprs) == 1 {
		if tuple, ok := b.typeOf(exprs[0]).(*types2.Tuple); ok {
			m := MultiExpr{Single: true, Pos: b.pos(pos), Expr: b.expr(exprs[0])}
			m.Results = make([]MultiResult, tuple.Len())
			for i := range m.Results {
				res := &m.Results[i]
				src := tuple.At(i).Type()
				res.Src = b.typeUse(src)
				dst := dstType(i)
				if res.Converted = dst != nil && !types2.Identical(src, dst); res.Converted {
					res.Dst = b.typeUse(dst)
					res.Conv = b.convRTTI(src, dst)
				}
			}
			return m
		}
	}
	m := MultiExpr{Exprs: make([]Expr, len(exprs))}
	for i, e := range exprs {
		m.Exprs[i] = b.implicitConv(dstType(i), e)
	}
	return m
}

// implicitConv builds an expression with the conversion its assignment
// context puts on it, and builds it plain where the context converts nothing.
func (b *bodyBuilder) implicitConv(dst types2.Type, e syntax.Expr) Expr {
	src := b.typeOf(e)
	if dst == nil || types2.Identical(src, dst) {
		return b.expr(e)
	}
	if !types2.AssignableTo(src, dst) {
		b.refuse("%v is not assignable to %v", src, dst)
	}
	return b.convertExpr(dst, e, true)
}

// convertExpr builds a conversion.
func (b *bodyBuilder) convertExpr(dst types2.Type, e syntax.Expr, implicit bool) Expr {
	src := b.typeOf(e)
	out := &ConvertExpr{
		Implicit:  implicit,
		Type:      b.typeUse(dst),
		Pos:       b.pos(e.Pos()),
		Conv:      b.convRTTI(src, dst),
		TypeParam: isTypeParam(dst),
		Identical: dst == nil || types2.Identical(src, dst),
	}
	out.X = b.expr(e)
	out.typ = dst
	return out
}

// expr builds one expression.
//
// The reshape node in front of it is the type the checker gave it, and is
// what makes the tree decidable without a second type check. gc writes one in
// front of every expression that has a type of its own, which is every
// expression that is not a constant, the untyped nil, a builtin, or of a
// tuple type.
func (b *bodyBuilder) expr(e syntax.Expr) Expr {
	if e == nil {
		b.refuse("an expression the encoding requires is absent")
	}
	e = syntax.Unparen(e)

	obj, inst := b.lookupObj(e)
	var targs []types2.Type
	for i := range inst.TypeArgs.Len() {
		targs = append(targs, inst.TypeArgs.At(i))
	}

	var et exprType
	if tv, ok := b.typeAndValue(e); ok {
		if tv.IsType() {
			b.refuse("%s is a type where a value is expected", syntax.String(e))
		}
		if tv.Value != nil {
			typ := idealType(tv)
			if typ == nil {
				b.refuse("the constant %s has no type the encoding can name", syntax.String(e))
			}
			out := &ConstExpr{Pos: b.pos(e.Pos()), Type: b.typeUse(typ), Value: tv.Value}
			out.typ = typ
			return out
		}
		if _, isNil := obj.(*types2.Nil); isNil {
			out := &ZeroExpr{Pos: b.pos(e.Pos()), Type: b.typeUse(tv.Type)}
			out.typ = tv.Type
			return out
		}
		if typ := tv.Type; !tv.IsBuiltin() && !isTupleType(typ) && !isUntypedType(typ) {
			use := b.typeUse(typ)
			et = exprType{reshape: &use, typ: typ}
		}
	}

	if obj != nil {
		if len(targs) != 0 {
			fn, ok := obj.(*types2.Func)
			if !ok {
				b.refuse("%s is instantiated and is a %T", syntax.String(e), obj)
			}
			return &FuncInstExpr{
				exprType: et,
				Pos:      b.pos(e.Pos()),
				Inst:     b.funcInst(fn, targs),
			}
		}
		if obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope() || obj.Pkg() == nil {
			return &GlobalExpr{exprType: et, Obj: b.objUse(obj, nil)}
		}
		v, ok := obj.(*types2.Var)
		if !ok {
			b.refuse("%s names a %T that is neither global nor a variable", syntax.String(e), obj)
		}
		use := b.useLocal(e.Pos(), v)
		use.exprType = et
		return &use
	}

	return b.expr1(e, et)
}

// expr1 builds one expression that names no declaration of its own.
func (b *bodyBuilder) expr1(e syntax.Expr, et exprType) Expr {
	switch e := e.(type) {
	default:
		b.refuse("the expression is a %T, which the format has no encoding for", e)
		panic("unreachable")

	case *syntax.CompositeLit:
		return b.compLit(e, et)

	case *syntax.FuncLit:
		return b.funcLit(e, et)

	case *syntax.SelectorExpr:
		sel, ok := b.s.info.Selections[e]
		if !ok {
			b.refuse("the checker recorded no selection for %s", syntax.String(e))
		}
		switch sel.Kind() {
		default:
			b.refuse("the selection is %v, which the format has no encoding for", sel.Kind())
			panic("unreachable")
		case types2.FieldVal:
			return &FieldValExpr{
				exprType: et,
				X:        b.expr(e.X),
				Pos:      b.pos(e.Pos()),
				Sel:      selectorOf(sel.Obj()),
			}
		case types2.MethodVal:
			return b.methVal(e, sel, et)
		case types2.MethodExpr:
			return b.methExpr(e, sel, et)
		}

	case *syntax.IndexExpr:
		// The explicit instantiation of a generic method looks like an
		// index of a selection.
		if selector, ok := e.X.(*syntax.SelectorExpr); ok {
			if sel, ok := b.s.info.Selections[selector]; ok {
				switch sel.Kind() {
				case types2.MethodVal:
					return b.methVal(selector, sel, et)
				case types2.MethodExpr:
					return b.methExpr(selector, sel, et)
				case types2.FieldVal:
					// Not a method, so an ordinary index.
				default:
					b.refuse("the selection is %v, which the format has no encoding for", sel.Kind())
				}
			}
		}
		b.typeOf(e.Index) // an index and not an instantiation

		xtyp := b.typeOf(e.X)
		var keyType types2.Type
		if mapType, ok := types2.CoreType(xtyp).(*types2.Map); ok {
			keyType = mapType.Key()
		}
		out := &IndexExpr{
			exprType: et,
			X:        b.expr(e.X),
			Pos:      b.pos(e.Pos()),
			Index:    b.implicitConv(keyType, e.Index),
		}
		if keyType != nil {
			rt := b.rtype(xtyp)
			out.MapRType = &rt
		}
		return out

	case *syntax.SliceExpr:
		out := &SliceExpr{exprType: et, X: b.expr(e.X), Pos: b.pos(e.Pos())}
		for i, n := range &e.Index {
			if n != nil {
				out.Index[i] = b.expr(n)
			}
		}
		return out

	case *syntax.AssertExpr:
		iface := b.typeOf(e.X)
		out := &AssertExpr{exprType: et, X: b.expr(e.X), Pos: b.pos(e.Pos())}
		out.Type = *b.exprType(iface, e.Type)
		out.Src = b.rtype(iface)
		return out

	case *syntax.Operation:
		if e.Y == nil {
			return &UnaryExpr{exprType: et, Op: unOp(b, e.Op), Pos: b.pos(e.Pos()), X: b.expr(e.X)}
		}
		// gc compares and combines two operands at one type, so each is
		// converted to whichever of the two the other is assignable to. A
		// shift is the exception: its operands keep their own types.
		var commonType types2.Type
		switch e.Op {
		case syntax.Shl, syntax.Shr:
		default:
			xtyp, ytyp := b.typeOf(e.X), b.typeOf(e.Y)
			switch {
			case types2.AssignableTo(xtyp, ytyp):
				commonType = ytyp
			case types2.AssignableTo(ytyp, xtyp):
				commonType = xtyp
			default:
				b.refuse("the operands %v and %v have no common type", xtyp, ytyp)
			}
		}
		out := &BinaryExpr{exprType: et, Op: binOp(b, e.Op)}
		out.X = b.implicitConv(commonType, e.X)
		out.Pos = b.pos(e.Pos())
		out.Y = b.implicitConv(commonType, e.Y)
		return out

	case *syntax.CallExpr:
		return b.callExpr(e, et)
	}
}

// selectorOf names the field or the method a selection resolved to.
func selectorOf(obj types2.Object) Selector {
	return Selector{Pkg: obj.Pkg(), Name: obj.Name()}
}

// callExpr builds a call, a conversion, or one of the builtins the format
// gives a node of its own.
func (b *bodyBuilder) callExpr(e *syntax.CallExpr, et exprType) Expr {
	tv, ok := b.typeAndValue(e.Fun)
	if !ok {
		b.refuse("the callee %s has no type", syntax.String(e.Fun))
	}
	if tv.IsType() {
		if len(e.ArgList) != 1 || e.HasDots {
			b.refuse("a conversion takes one argument and no dots")
		}
		out := b.convertExpr(tv.Type, e.ArgList[0], false)
		if c, ok := out.(*ConvertExpr); ok {
			c.exprType = et
		}
		return out
	}

	// The element or key descriptor four builtins need at run time.
	var rtype types2.Type
	if tv.IsBuiltin() {
		obj, _ := b.lookupObj(syntax.Unparen(e.Fun))
		if obj == nil {
			b.refuse("the builtin %s names no declaration", syntax.String(e.Fun))
		}
		switch obj.Name() {
		case "make":
			if len(e.ArgList) < 1 || e.HasDots {
				b.refuse("make takes at least one argument and no dots")
			}
			out := &MakeExpr{exprType: et, Pos: b.pos(e.Pos())}
			out.Type = *b.exprType(nil, e.ArgList[0])
			out.Args = b.exprs(e.ArgList[1:])
			typ := b.typeOf(e)
			switch core := types2.CoreType(typ).(type) {
			default:
				b.refuse("make builds a %T", core)
			case *types2.Chan, *types2.Map:
				out.RType = b.rtype(typ)
			case *types2.Slice:
				out.RType = b.rtype(core.Elem())
			}
			return out

		case "new":
			if len(e.ArgList) != 1 || e.HasDots {
				b.refuse("new takes one argument and no dots")
			}
			arg := e.ArgList[0]
			out := &NewExpr{exprType: et, Pos: b.pos(e.Pos())}
			if tv, ok := b.typeAndValue(arg); ok && !tv.IsType() {
				out.Value = b.expr(arg) // new(expr), which Go 1.26 added
			} else {
				out.Type = b.exprType(nil, arg) // new(T)
			}
			return out

		case "Sizeof", "Alignof":
			if len(e.ArgList) != 1 || e.HasDots {
				b.refuse("%s takes one argument and no dots", obj.Name())
			}
			kind := ExprSizeof
			if obj.Name() == "Alignof" {
				kind = ExprAlignof
			}
			return &SizeExpr{
				exprType: et,
				Kind:     kind,
				Pos:      b.pos(e.Pos()),
				Type:     b.typeUse(b.typeOf(e.ArgList[0])),
			}

		case "Offsetof":
			if len(e.ArgList) != 1 || e.HasDots {
				b.refuse("Offsetof takes one argument and no dots")
			}
			selector, ok := syntax.Unparen(e.ArgList[0]).(*syntax.SelectorExpr)
			if !ok {
				b.refuse("Offsetof names no field")
			}
			sel, ok := b.s.info.Selections[selector]
			if !ok {
				b.refuse("the checker recorded no selection for %s", syntax.String(selector))
			}
			return &OffsetofExpr{
				exprType: et,
				Pos:      b.pos(e.Pos()),
				Type:     b.typeUse(deref2(b.typeOf(selector.X))),
				Path:     sel.Index(),
			}

		case "append":
			rtype = b.sliceElem(b.typeOf(e))
		case "copy":
			rtype = b.sliceElem(b.firstResult(b.typeOf(e.ArgList[0])))
		case "delete":
			rtype = b.firstResult(b.typeOf(e.ArgList[0]))
		case "Slice":
			rtype = b.sliceElem(b.typeOf(e))
		}
	}

	sigType, ok := types2.CoreType(tv.Type).(*types2.Signature)
	if !ok {
		b.refuse("the callee %s is not a function", syntax.String(e.Fun))
	}
	out := &CallExpr{exprType: et}
	b.callee(out, e)
	out.Pos = b.pos(e.Pos())

	paramTypes := sigType.Params()
	paramType := func(i int) types2.Type {
		if sigType.Variadic() && !e.HasDots && i >= paramTypes.Len()-1 {
			last, ok := paramTypes.At(paramTypes.Len() - 1).Type().(*types2.Slice)
			if !ok {
				b.refuse("the last parameter of a variadic function is not a slice")
			}
			return last.Elem()
		}
		return paramTypes.At(i).Type()
	}
	out.Args = b.multiExpr(e.Pos(), paramType, e.ArgList)
	out.Dots = e.HasDots
	if rtype != nil {
		rt := b.rtype(rtype)
		out.RType = &rt
	}
	return out
}

// callee builds the callee of a call, which has three shapes: a method
// selection, an instantiated generic function, and everything else.
func (b *bodyBuilder) callee(out *CallExpr, e *syntax.CallExpr) {
	fun := syntax.Unparen(e.Fun)

	inner := fun
	if idx, ok := inner.(*syntax.IndexExpr); ok {
		inner = idx.X
	}
	if selector, ok := inner.(*syntax.SelectorExpr); ok {
		if sel, ok := b.s.info.Selections[selector]; ok && sel.Kind() == types2.MethodVal {
			recv, typ := b.recvExpr(selector, sel)
			out.Method = &MethodCall{Recv: recv, Method: b.methodRef(selector, typ, sel)}
			return
		}
	}

	if obj, inst := b.lookupObj(fun); obj != nil && inst.TypeArgs.Len() != 0 {
		fn, ok := obj.(*types2.Func)
		if !ok {
			b.refuse("the instantiated callee is a %T", obj)
		}
		out.InstPos = b.pos(fun.Pos())
		f := b.funcInst(fn, typeSlice(inst.TypeArgs))
		out.Inst = &f
		return
	}
	out.Fun = b.expr(fun)
}

// sliceElem returns the element type of a slice.
func (b *bodyBuilder) sliceElem(typ types2.Type) types2.Type {
	s, ok := types2.CoreType(typ).(*types2.Slice)
	if !ok {
		b.refuse("the operand %v is not a slice", typ)
	}
	return s.Elem()
}

// firstResult returns the first result of a multi-valued expression, and the
// type itself for every other one. It is what "copy(g())" needs.
func (b *bodyBuilder) firstResult(typ types2.Type) types2.Type {
	if tuple, ok := typ.(*types2.Tuple); ok {
		return tuple.At(0).Type()
	}
	return typ
}

// exprs builds a list of expressions.
func (b *bodyBuilder) exprs(list []syntax.Expr) []Expr {
	out := make([]Expr, len(list))
	for i, e := range list {
		out[i] = b.expr(e)
	}
	return out
}

// methVal builds a bound method value.
func (b *bodyBuilder) methVal(e *syntax.SelectorExpr, sel *types2.Selection, et exprType) Expr {
	recv, typ := b.recvExpr(e, sel)
	return &MethodValExpr{
		exprType: et,
		Recv:     recv,
		Pos:      b.pos(e.Pos()),
		Method:   b.methodRef(e, typ, sel),
	}
}

// methExpr builds a method expression, whose result is a function taking the
// receiver as its first parameter.
func (b *bodyBuilder) methExpr(e *syntax.SelectorExpr, sel *types2.Selection, et exprType) Expr {
	tv, ok := b.typeAndValue(e.X)
	if !ok || !tv.IsType() {
		b.refuse("the operand of a method expression is not a type")
	}
	index := sel.Index()
	implicits := index[:len(index)-1]

	typ := tv.Type
	out := &MethodExprExpr{exprType: et, Recv: b.typeUse(typ)}
	for _, ix := range implicits {
		out.Implicits = append(out.Implicits, ix)
		typ = b.embeddedField(typ, ix)
	}
	recv := b.methodRecv(sel)
	switch {
	case isPtrTo(typ, recv):
		out.Deref, typ = true, recv
	case isPtrTo(recv, typ):
		out.Addr, typ = true, recv
	}
	out.Pos = b.pos(e.Pos())
	out.Method = b.methodRef(e, typ, sel)
	return out
}

// recvExpr builds the operand of a method selection, with the field
// selections, dereference or address the selection applies to it, and returns
// the type it produces.
func (b *bodyBuilder) recvExpr(e *syntax.SelectorExpr, sel *types2.Selection) (Expr, types2.Type) {
	index := sel.Index()
	implicits := index[:len(index)-1]

	out := &RecvExpr{X: b.expr(e.X), Pos: b.pos(e.Pos())}
	typ := b.typeOf(e.X)
	for _, ix := range implicits {
		typ = b.embeddedField(typ, ix)
		out.Implicits = append(out.Implicits, ix)
	}
	recv := b.methodRecv(sel)
	switch {
	case isPtrTo(typ, recv):
		out.Deref, typ = true, recv
	case isPtrTo(recv, typ):
		out.Addr, typ = true, recv
	}
	return out, typ
}

// methodRecv returns the receiver the method a selection names declares.
func (b *bodyBuilder) methodRecv(sel *types2.Selection) types2.Type {
	fn, ok := sel.Obj().(*types2.Func)
	if !ok {
		b.refuse("the selection names a %T where a method is expected", sel.Obj())
	}
	return fn.Signature().Recv().Type()
}

// embeddedField returns the type of one step of a path through embedded
// fields.
func (b *bodyBuilder) embeddedField(typ types2.Type, ix int) types2.Type {
	str, ok := deref2(typ).Underlying().(*types2.Struct)
	if !ok {
		b.refuse("the selection goes through %v, which is not a struct", typ)
	}
	return str.Field(ix).Type()
}

// methodRef builds a reference to the method a selection names.
func (b *bodyBuilder) methodRef(e *syntax.SelectorExpr, recv types2.Type, sel *types2.Selection) MethodRef {
	fn, ok := sel.Obj().(*types2.Func)
	if !ok {
		b.refuse("the selection names a %T where a method is expected", sel.Obj())
	}
	sig := fn.Signature()

	out := MethodRef{Recv: b.typeUse(recv)}
	// A method that declares type parameters of its own is an element of the
	// package rather than a member of its receiver's element, so its
	// signature is not written here: the reader takes it from that element.
	out.Generic = sig.TypeParams().Len() != 0
	if !out.Generic {
		out.Sig = b.typeUse(sig)
	}
	out.Pos = b.pos(e.Pos())
	out.Sel = selectorOf(fn)

	// A method named on a type parameter is not known until the type
	// argument is, so the call goes through a slot of the dictionary and
	// nothing further is written.
	if tp, ok := types2.Unalias(recv).(*types2.TypeParam); ok {
		idx, known := b.dict.TypeParamIndex(tp)
		if !known {
			b.refuse("the method is called on %v, which the declaration does not declare as a type parameter", tp)
		}
		out.TypeParam = true
		out.DictIdx = b.dict.MethodExprIndex(idx, out.Sel)
		return out
	}
	if isInterfaceType(recv) != isInterfaceType(sig.Recv().Type()) {
		b.refuse("the receiver %v and the method's receiver %v disagree about being an interface", recv, sig.Recv().Type())
	}
	// A concrete method of an instantiated type is called with a dictionary.
	// Which one depends on the type arguments: a dictionary the file holds
	// when every argument is known here, and a slot of this declaration's
	// dictionary when one of them is derived and the argument is therefore
	// not known until the enclosing instantiation is.
	if !isInterfaceType(sig.Recv().Type()) {
		named, ok := types2.Unalias(deref2(recv)).(*types2.Named)
		if !ok {
			b.refuse("the receiver %v of a concrete method is not a defined type", recv)
		}
		targs := typeSlice(named.TypeArgs())
		var use ObjUse
		if out.Generic {
			// For a generic method the shaped declaration is the method,
			// instantiated with the receiver's type arguments and then its
			// own.
			explicits := make([]types2.Type, 0, len(targs))
			explicits = append(explicits, targs...)
			explicits = append(explicits, typeSlice(b.s.info.Instances[e.Sel].TypeArgs)...)
			use = b.objUse(fn.Origin(), explicits)
		} else {
			// For a method of a generic type the shaped declaration is the
			// type, and the reader looks the method up on it by name.
			use = b.objUse(named.Obj(), targs)
		}
		if anyDerived(use.Targs) {
			out.Subdict = true
			out.SubdictIdx = b.dict.SubdictIndex(use)
			return out
		}
		if len(use.Targs) != 0 {
			out.StaticDict = true
			out.Dict = use
		}
	}
	return out
}

// funcInst builds a reference to an instantiated generic function.
//
// A type argument that is derived leaves the callee's dictionary unknown
// until the enclosing instantiation is known, so the call takes it from a
// subdictionary slot instead of naming it.
func (b *bodyBuilder) funcInst(fn *types2.Func, targs []types2.Type) FuncInst {
	use := b.objUse(fn, targs)
	if anyDerived(use.Targs) {
		return FuncInst{Derived: true, DictIdx: b.dict.SubdictIndex(use), Obj: use}
	}
	return FuncInst{Obj: use}
}

// anyDerived reports whether any type argument of an instantiation is
// derived, which is what makes the instantiation itself a dictionary slot.
func anyDerived(targs []TypeUse) bool {
	for _, t := range targs {
		if t.Derived {
			return true
		}
	}
	return false
}

// compLit builds a composite literal.
func (b *bodyBuilder) compLit(e *syntax.CompositeLit, et exprType) Expr {
	typ := b.typeOf(e)
	out := &CompLitExpr{exprType: et, Pos: b.pos(e.Pos()), Type: b.typeUse(typ)}

	lit := typ
	if ptr, ok := types2.CoreType(lit).(*types2.Pointer); ok {
		lit = ptr.Elem()
	}
	switch core := types2.CoreType(lit).(type) {
	default:
		b.refuse("the composite literal builds a %T, which the format has no element encoding for", core)

	case *types2.Array:
		b.arrayElems(out, core.Elem(), e.ElemList)
	case *types2.Slice:
		b.arrayElems(out, core.Elem(), e.ElemList)
	case *types2.Map:
		rt := b.rtype(lit)
		out.MapRType = &rt
		b.mapElems(out, core.Key(), core.Elem(), e.ElemList)
	case *types2.Struct:
		b.structElems(out, core, e.NKeys == 0, e.ElemList)
	}
	return out
}

// arrayElems builds the elements of an array or slice literal. The literal is
// written with keys when any element has one.
func (b *bodyBuilder) arrayElems(out *CompLitExpr, elemType types2.Type, elems []syntax.Expr) {
	keyed := false
	for _, el := range elems {
		if _, ok := el.(*syntax.KeyValueExpr); ok {
			keyed = true
			break
		}
	}
	out.Keyed = keyed && len(elems) > 0
	out.Elems = make([]LitElem, len(elems))
	for i, el := range elems {
		e := &out.Elems[i]
		if kv, ok := el.(*syntax.KeyValueExpr); ok && keyed {
			// The key's position and not the colon's.
			e.Pos = b.pos(kv.Key.Pos())
			e.Key = b.implicitConv(nil, kv.Key)
			el = kv.Value
		}
		e.Value = b.implicitConv(elemType, el)
	}
}

// mapElems builds the elements of a map literal, each of which has a key.
func (b *bodyBuilder) mapElems(out *CompLitExpr, keyType, valueType types2.Type, elems []syntax.Expr) {
	out.Keyed = len(elems) > 0
	out.Elems = make([]LitElem, len(elems))
	for i, el := range elems {
		kv, ok := el.(*syntax.KeyValueExpr)
		if !ok {
			b.refuse("an element of a map literal has no key")
		}
		e := &out.Elems[i]
		e.Pos = b.pos(kv.Key.Pos())
		e.Key = b.implicitConv(keyType, kv.Key)
		e.Value = b.implicitConv(valueType, kv.Value)
	}
}

// structElems builds the elements of a struct literal, which either fills
// every field in order or names each field it fills.
func (b *bodyBuilder) structElems(out *CompLitExpr, typ *types2.Struct, valuesOnly bool, elems []syntax.Expr) {
	out.Keyed = !valuesOnly && len(elems) > 0
	out.Elems = make([]LitElem, len(elems))
	if valuesOnly {
		for i, el := range elems {
			e := &out.Elems[i]
			e.Pos = b.pos(el.Pos())
			e.Field = i
			e.Value = b.implicitConv(typ.Field(i).Type(), el)
		}
		return
	}
	for i, el := range elems {
		kv, ok := el.(*syntax.KeyValueExpr)
		if !ok {
			b.refuse("an element of a keyed struct literal has no key")
		}
		name, ok := kv.Key.(*syntax.Name)
		if !ok {
			b.refuse("the key of a struct literal is not a field name")
		}
		e := &out.Elems[i]
		e.Pos = b.pos(kv.Key.Pos())
		fld, index, _ := types2.LookupFieldOrMethod(typ, false, b.s.pkg, name.Value)
		if fld == nil || len(index) == 0 {
			b.refuse("%s is not a field of the literal's type", name.Value)
		}
		if len(index) > 1 {
			e.Embedded = index
		}
		e.Field = index[0]
		e.Value = b.implicitConv(fld.Type(), kv.Value)
	}
}

// funcLit builds a function literal and the body it names.
//
// The literal's body is an element of its own. The variables it captures come
// back numbered in its own capture list, and each is named again here in this
// body's numbering, which is what lets a capture reach through two literals.
func (b *bodyBuilder) funcLit(e *syntax.FuncLit, et exprType) Expr {
	sig, ok := b.typeOf(e).(*types2.Signature)
	if !ok {
		b.refuse("the function literal has no signature")
	}
	body, free := b.s.build(b.name+", in a function literal", sig, b.dict, e.Body)

	out := &FuncLitExpr{exprType: et, Pos: b.pos(e.Pos())}
	out.Params = b.params(sig.Params())
	out.Results = b.params(sig.Results())
	out.Variadic = sig.Variadic()
	out.Captured = make([]CapturedVar, len(free))
	for i, cv := range free {
		out.Captured[i] = CapturedVar{Pos: b.pos(cv.pos), Local: b.useLocal(cv.pos, cv.obj)}
	}
	out.Decoded = body
	return out
}

// params builds one parameter list of a signature written inside a body.
func (b *bodyBuilder) params(tuple *types2.Tuple) []Param {
	out := make([]Param, tuple.Len())
	for i := range tuple.Len() {
		v := tuple.At(i)
		out[i] = Param{
			Pos:  b.pos(v.Pos()),
			Pkg:  v.Pkg(),
			Name: v.Name(),
			Type: b.typeUse(v.Type()),
		}
	}
	return out
}

// deref2 returns the element of a pointer, and the type itself otherwise.
func deref2(t types2.Type) types2.Type {
	if ptr := types2.AsPointer(t); ptr != nil {
		return ptr.Elem()
	}
	return t
}

// isPtrTo reports whether from is a pointer to to.
func isPtrTo(from, to types2.Type) bool {
	ptr, ok := types2.Unalias(from).(*types2.Pointer)
	return ok && types2.Identical(ptr.Elem(), to)
}

// isTypeParam reports whether a type is a type parameter, which a conversion
// to it records because the conversion is not decided until the type argument
// is known.
func isTypeParam(typ types2.Type) bool {
	_, ok := types2.Unalias(typ).(*types2.TypeParam)
	return ok
}

// isInterfaceType reports whether a type's underlying type is an interface.
func isInterfaceType(typ types2.Type) bool {
	_, ok := typ.Underlying().(*types2.Interface)
	return ok
}

// typeSlice turns a type list into a slice, and nil into nil.
func typeSlice(l *types2.TypeList) []types2.Type {
	if l.Len() == 0 {
		return nil
	}
	out := make([]types2.Type, l.Len())
	for i := range l.Len() {
		out[i] = l.At(i)
	}
	return out
}

// idealType returns the concrete type gc gives an expression the checker left
// untyped.
//
// The specification does not require converting these to a concrete type, and
// the encoding has no way to name an untyped one. The choices are gc's: an
// untyped integer becomes uint wherever it fits and int where it does not.
func idealType(tv types2.TypeAndValue) types2.Type {
	typ := types2.Unalias(tv.Type)
	basic, ok := typ.(*types2.Basic)
	if !ok || basic.Info()&types2.IsUntyped == 0 {
		return typ
	}
	switch basic.Kind() {
	case types2.UntypedNil:
		// A case of a type switch names it, and there it is a type.
	case types2.UntypedInt, types2.UntypedFloat, types2.UntypedComplex:
		typ = types2.Typ[types2.Uint]
		if tv.Value != nil {
			s := constant.ToInt(tv.Value)
			if s.Kind() != constant.Int {
				return nil
			}
			if constant.Sign(s) < 0 {
				typ = types2.Typ[types2.Int]
			}
		}
	case types2.UntypedBool:
		typ = types2.Typ[types2.Bool] // a condition of an if or a for
	case types2.UntypedString:
		typ = types2.Typ[types2.String] // an argument of append or copy
	case types2.UntypedRune:
		typ = types2.Typ[types2.Int32] // a range over a string
	default:
		return nil
	}
	return typ
}

// constantBool returns the value of a constant of boolean kind.
func constantBool(v constant.Value) bool {
	return v.Kind() == constant.Bool && constant.BoolVal(v)
}

// isTupleType reports whether a type is a tuple.
func isTupleType(typ types2.Type) bool {
	_, ok := typ.(*types2.Tuple)
	return ok
}

// isUntypedType reports whether a type is untyped.
func isUntypedType(typ types2.Type) bool {
	basic, ok := typ.(*types2.Basic)
	return ok && basic.Info()&types2.IsUntyped != 0
}
