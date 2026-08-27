// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"go/constant"
	"math/big"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// The body encoder.
//
// It is the mirror of bodyread.go, field for field and in the same order, and
// it writes the tree of body.go back into one element of SectionBody. The
// oracle is byte identity with gc: every body element of every standard
// library package is decoded, encoded again, and compared with gc's own bytes
// and gc's own reference table (bodywrite_test.go).
//
// What the encoder decides and what it is told apart:
//
//   - The layout is the encoder's. Which field follows which, at which width,
//     and where a reference goes, is what byte identity proves.
//   - Which element a reference names is not the encoder's. A body refers to
//     a type, an object, a package, a string, a position base and another
//     body by index, and the index belongs to the package being written. So
//     the encoder asks a [bodyRefs] for it.
//
// The two modes that follow from that split are the reason for the interface.
// Encoding a tree that was read gives every reference the index the archive
// already gave it, which is [elemRefs] and is what makes byte identity
// meaningful. Encoding a tree built from syntax has no such index and must
// ask the package writer to allocate one, which is not built.

// bodyRefs answers, for each thing a body can name outside itself, which
// element of the package being written holds it.
//
// A derived type and a dictionary slot are not here. Both are indices into
// the enclosing declaration's own dictionary, which the encoder fills as it
// goes, so the tree carries them and no resolution is needed.
type bodyRefs interface {
	// strIdx returns the SectionString element holding s.
	strIdx(s string) pkgbits.Index

	// pkgIdx returns the SectionPkg element for pkg, which is nil for the
	// universe.
	pkgIdx(pkg *types2.Package) pkgbits.Index

	// typIdx returns the SectionType element for a type use that is not
	// derived.
	typIdx(t TypeUse) pkgbits.Index

	// objIdx returns the SectionObj element for a use of a package-scope
	// declaration.
	objIdx(o ObjUse) pkgbits.Index

	// posBaseIdx returns the SectionPosBase element a known position is
	// measured from.
	posBaseIdx(p Pos) pkgbits.Index

	// bodyIdx returns the SectionBody element holding a function literal's
	// body.
	bodyIdx(e *FuncLitExpr) pkgbits.Index

	// dictIdx returns the slot of the enclosing declaration's dictionary
	// the body may name, and refuses when the resolver fills no dictionary.
	//
	// A dictionary slot is not an element index, which is why it is not one
	// of the five above: it is an offset into a list the declaration carries
	// and the encoder is told it. It is still asked for, because a slot
	// written into a dictionary that holds nothing is a type gc reads
	// without complaint and reads wrong. what names the kind of slot, for
	// the refusal.
	dictIdx(what string, slot int) int
}

// bodyWriter encodes one body element.
type bodyWriter struct {
	*pkgbits.Encoder
	refs bodyRefs

	// path and name are the package being written and the declaration the
	// body belongs to, for a refusal.
	path string
	name string

	// nested collects every function literal this element named, in the
	// order it named them. A literal's body is an element of its own, so
	// whoever holds the elements has to write each one after this one.
	nested []*FuncLitExpr

	// check collects the reason the body cannot be offered for inlining,
	// and is nil on the ordinary write path. See bodyinline.go.
	check *inlineCheck
}

// refuse reports a body the encoder cannot write.
//
// It reuses [BodyError], because the two halves refuse the same thing: a tree
// the format has no shape for. The message names the declaration, which is
// what a user has to work around.
func (w *bodyWriter) refuse(format string, args ...any) {
	panic(&BodyError{Package: w.path, Name: w.name, Reason: fmt.Sprintf(format, args...), Writing: true})
}

// encodeBody writes b into the element w holds.
//
// The number of locals the body opens with is not written: it is the arity of
// the signature that names the body, which is why a body cannot be decoded on
// its own. See [pkgReader.decodeBody] from the other side.
func (w *bodyWriter) encodeBody(b *Body) {
	for _, l := range b.Params {
		w.local(l)
	}
	if w.Bool(b.HasBlock) {
		w.stmts(b.Stmts)
		w.pos(b.Rbrace)
	}
}

// local writes one local variable declaration.
func (w *bodyWriter) local(l Local) {
	w.Sync(pkgbits.SyncAddLocal)
	if w.Bool(l.DictRType >= 0) {
		w.Len(w.refs.dictIdx("a local variable's runtime type", l.DictRType))
	}
}

// @@@ Positions

// pos writes a position.
func (w *bodyWriter) pos(p Pos) {
	w.Sync(pkgbits.SyncPos)
	if !w.Bool(p.Known) {
		return
	}
	w.Reloc(pkgbits.SectionPosBase, w.refs.posBaseIdx(p))
	w.Uint(p.Line)
	w.Uint(p.Col)
}

// @@@ References

// pkg writes a package reference.
func (w *bodyWriter) pkg(pkg *types2.Package) {
	w.Sync(pkgbits.SyncPkg)
	w.Reloc(pkgbits.SectionPkg, w.refs.pkgIdx(pkg))
}

// str writes a string reference.
//
// Not [pkgbits.Encoder.String], which adds the string to the section it is
// writing. Which element holds a string is the resolver's answer and not this
// element's.
func (w *bodyWriter) str(s string) {
	w.StringRef(w.refs.strIdx(s))
}

// typeUse writes a use of a type.
func (w *bodyWriter) typeUse(t TypeUse) {
	w.Sync(pkgbits.SyncType)
	if w.Bool(t.Derived) {
		w.Len(w.refs.dictIdx("a derived type", int(t.Idx)))
		return
	}
	w.Reloc(pkgbits.SectionType, w.refs.typIdx(t))
}

// objUse writes a use of a package-scope declaration.
func (w *bodyWriter) objUse(o ObjUse) {
	w.check.obj(o)
	w.Sync(pkgbits.SyncObject)
	if w.Version().Has(pkgbits.DerivedFuncInstance) {
		w.Bool(false)
	}
	w.Reloc(pkgbits.SectionObj, w.refs.objIdx(o))
	w.Len(len(o.Targs))
	for _, t := range o.Targs {
		w.typeUse(t)
	}
}

// selector writes the name of a field or a method.
func (w *bodyWriter) selector(s Selector) {
	w.Sync(pkgbits.SyncSelector)
	w.pkg(s.Pkg)
	w.str(s.Name)
}

// localIdent writes the name of a local variable.
func (w *bodyWriter) localIdent(pkg *types2.Package, name string) {
	w.Sync(pkgbits.SyncLocalIdent)
	w.pkg(pkg)
	w.str(name)
}

// value writes a constant.
//
// Not [pkgbits.Encoder.Value], for the reason [bodyReader.value] gives from
// the other side: it writes a string through the container, and a reference
// this element did not make is a reference the resolver never saw.
func (w *bodyWriter) value(v constant.Value) {
	w.Sync(pkgbits.SyncValue)
	if w.Bool(v.Kind() == constant.Complex) {
		w.scalar(constant.Real(v))
		w.scalar(constant.Imag(v))
		return
	}
	w.scalar(v)
}

func (w *bodyWriter) scalar(v constant.Value) {
	switch x := constant.Val(v).(type) {
	default:
		w.refuse("the constant is %v (%v), which the format has no tag for", v, v.Kind())
	case bool:
		w.Code(pkgbits.ValBool)
		w.Bool(x)
	case string:
		w.Code(pkgbits.ValString)
		w.str(x)
	case int64:
		w.Code(pkgbits.ValInt64)
		w.Int64(x)
	case *big.Int:
		w.Code(pkgbits.ValBigInt)
		w.bigInt(x)
	case *big.Rat:
		w.Code(pkgbits.ValBigRat)
		w.bigInt(x.Num())
		w.bigInt(x.Denom())
	case *big.Float:
		w.Code(pkgbits.ValBigFloat)
		w.str(string(x.Append(nil, 'p', -1)))
	}
}

func (w *bodyWriter) bigInt(v *big.Int) {
	w.str(string(v.Bytes()))
	w.Bool(v.Sign() < 0)
}

// label writes a label.
func (w *bodyWriter) label(s string) {
	w.Sync(pkgbits.SyncLabel)
	w.str(s)
}

// optLabel writes a label that may be absent.
func (w *bodyWriter) optLabel(s string, present bool) {
	w.Sync(pkgbits.SyncOptLabel)
	if w.Bool(present) {
		w.label(s)
	}
}

// op writes an operator, and refuses one a body cannot carry.
func (w *bodyWriter) op(op Op) {
	if !op.known() {
		w.refuse("the operator is gc's ir.Op %d, which no statement or expression of a body writes", int(op))
	}
	w.Sync(pkgbits.SyncOp)
	w.Len(int(op))
}

// @@@ Runtime type information

// rtype writes a runtime type descriptor reference.
func (w *bodyWriter) rtype(t RType) {
	w.Sync(pkgbits.SyncRType)
	if w.Bool(t.Derived) {
		w.Len(w.refs.dictIdx("a runtime type descriptor", t.DictIdx))
		return
	}
	w.typeUse(t.Type)
}

// itab writes the pair of descriptors a conversion needs.
func (w *bodyWriter) itab(c ConvRTTI) {
	w.rtype(c.Src)
	w.rtype(c.Dst)
	if w.Bool(c.Derived) {
		w.Len(w.refs.dictIdx("an itab", c.DictIdx))
	}
}

// convRTTI writes the descriptors a conversion from one type to another needs.
func (w *bodyWriter) convRTTI(c ConvRTTI) {
	w.Sync(pkgbits.SyncConvRTTI)
	w.itab(c)
}

// exprType writes a type written where an expression is expected.
func (w *bodyWriter) exprType(t *ExprType) {
	w.Sync(pkgbits.SyncExprType)
	w.pos(t.Pos)
	if w.Bool(t.Itab != nil) {
		w.itab(*t.Itab)
		return
	}
	if t.RType == nil {
		w.refuse("a type written where an expression is expected carries neither a descriptor nor an itab")
	}
	w.rtype(*t.RType)
	w.Bool(t.Derived)
}

// @@@ Statements

// stmts writes a statement list.
func (w *bodyWriter) stmts(list []Stmt) {
	w.Sync(pkgbits.SyncStmts)
	for _, s := range list {
		kind := s.stmtKind()
		w.check.stmt(kind)
		w.Code(kind)
		w.stmt(s)
	}
	w.Code(StmtEnd)
	w.Sync(pkgbits.SyncStmtsEnd)
}

// stmt writes one statement, after its code.
func (w *bodyWriter) stmt(s Stmt) {
	switch s := s.(type) {
	default:
		w.refuse("the statement is a %T, which the format has no encoding for", s)

	case *LabelStmt:
		w.pos(s.Pos)
		w.label(s.Label)

	case *BlockStmt:
		w.blockStmt(s)

	case *ExprStmt:
		w.expr(s.X)

	case *SendStmt:
		w.pos(s.Pos)
		w.expr(s.Chan)
		w.expr(s.Value)

	case *AssignStmt:
		w.pos(s.Pos)
		w.assignList(s.Lhs)
		w.multiExpr(s.Rhs)

	case *AssignOpStmt:
		w.op(s.Op)
		w.expr(s.Lhs)
		w.pos(s.Pos)
		w.expr(s.Rhs)

	case *IncDecStmt:
		w.op(s.Op)
		w.expr(s.X)
		w.pos(s.Pos)

	case *BranchStmt:
		w.pos(s.Pos)
		w.op(s.Op)
		w.optLabel(s.Label, s.Labelled)

	case *CallStmt:
		w.pos(s.Pos)
		w.op(s.Op)
		w.expr(s.Call)
		if s.Op == OpDefer {
			w.optExpr(s.DeferAt)
		}

	case *ReturnStmt:
		w.pos(s.Pos)
		w.multiExpr(s.Results)

	case *IfStmt:
		w.ifStmt(s)

	case *ForStmt:
		w.forStmt(s)

	case *SwitchStmt:
		w.switchStmt(s)

	case *SelectStmt:
		w.selectStmt(s)
	}
}

// assignList writes the destinations of an assignment.
func (w *bodyWriter) assignList(list []Assignee) {
	w.Len(len(list))
	for _, a := range list {
		w.assign(a)
	}
}

// assign writes one destination of an assignment.
func (w *bodyWriter) assign(a Assignee) {
	w.Code(a.Kind)
	switch a.Kind {
	default:
		w.refuse("the assignment destination is %v, which the format has no encoding for", a.Kind)

	case AssignBlank:

	case AssignDef:
		w.pos(a.Pos)
		w.localIdent(a.Pkg, a.Name)
		w.typeUse(a.Type)
		w.local(a.Local)

	case AssignExpr:
		w.expr(a.Expr)
	}
}

// blockStmt writes a block and the scope it opens.
func (w *bodyWriter) blockStmt(b *BlockStmt) {
	w.Sync(pkgbits.SyncBlockStmt)
	w.openScope(b.Open)
	w.stmts(b.Body)
	w.closeScope(b.Close)
}

func (w *bodyWriter) openScope(p Pos) {
	w.Sync(pkgbits.SyncOpenScope)
	w.pos(p)
}

func (w *bodyWriter) closeScope(p Pos) {
	w.Sync(pkgbits.SyncCloseScope)
	w.pos(p)
	w.Sync(pkgbits.SyncCloseAnotherScope)
}

func (w *bodyWriter) ifStmt(s *IfStmt) {
	w.Sync(pkgbits.SyncIfStmt)
	w.openScope(s.Open)
	w.pos(s.Pos)
	w.stmts(s.Init)
	w.expr(s.Cond)

	// The constant value of the condition, if it has one. gc writes the arm
	// it could not prove unreachable and no other, so a positive value has
	// no else and a negative one has no then. Zero has both, which is why
	// the two tests overlap rather than being an if and an else.
	w.Int(s.Static)
	if s.Static >= 0 {
		if s.Then == nil {
			w.refuse("the if statement has no then block and its condition is not constantly false")
		}
		w.blockStmt(s.Then)
	} else {
		w.pos(s.ThenClose)
	}
	if s.Static <= 0 {
		w.stmts(s.Else)
	}
	w.Sync(pkgbits.SyncCloseAnotherScope)
}

func (w *bodyWriter) forStmt(s *ForStmt) {
	w.Sync(pkgbits.SyncForStmt)
	w.openScope(s.Open)

	if w.Bool(s.Range != nil) {
		c := s.Range
		w.pos(c.Pos)
		w.assignList(c.Lhs)
		w.expr(c.X)

		// The operand's type decides whether the descriptor of a map
		// follows, by the same test the reader makes on the same type.
		w.mapRType("range operand", exprTypeOf(c.X), c.MapRType)

		if len(c.Lhs) > 0 && c.Lhs[0].Kind != AssignBlank {
			if c.KeyConv == nil {
				w.refuse("the range clause assigns a key and carries no conversion for it")
			}
			w.convRTTI(*c.KeyConv)
		} else if c.KeyConv != nil {
			w.refuse("the range clause carries a key conversion and assigns no key")
		}
		if len(c.Lhs) > 1 && c.Lhs[1].Kind != AssignBlank {
			if c.ValueConv == nil {
				w.refuse("the range clause assigns a value and carries no conversion for it")
			}
			w.convRTTI(*c.ValueConv)
		} else if c.ValueConv != nil {
			w.refuse("the range clause carries a value conversion and assigns no value")
		}
	} else {
		w.pos(s.Pos)
		w.stmts(s.Init)
		w.optExpr(s.Cond)
		w.stmts(s.Post)
	}

	if s.Body == nil {
		w.refuse("the for statement has no body")
	}
	w.blockStmt(s.Body)
	w.Bool(s.DistinctVars)
	w.Sync(pkgbits.SyncCloseAnotherScope)
}

func (w *bodyWriter) switchStmt(s *SwitchStmt) {
	w.Sync(pkgbits.SyncSwitchStmt)
	w.openScope(s.Open)
	w.pos(s.Pos)
	w.stmts(s.Init)

	if w.Bool(s.Guard != nil) {
		g := s.Guard
		w.pos(g.Pos)
		if w.Bool(g.Named) {
			w.pos(g.NamePos)
			// Not localIdent: the guard's variable has no object, so gc
			// writes the package and the name without one.
			w.Sync(pkgbits.SyncLocalIdent)
			w.pkg(g.Pkg)
			w.str(g.Name)
		}
		w.expr(g.X)
	} else {
		w.optExpr(s.Tag)
	}

	w.Len(len(s.Clauses))
	for i := range s.Clauses {
		c := &s.Clauses[i]
		if i > 0 {
			if c.ScopeClose == nil {
				w.refuse("clause %d of the switch closes no scope, and every clause after the first does", i)
			}
			w.closeScope(*c.ScopeClose)
		}
		w.openScope(c.ScopeOpen)
		w.pos(c.Pos)

		if s.Guard != nil {
			w.Len(len(c.Types))
			for _, t := range c.Types {
				// A nil type is the case nil.
				if w.Bool(t == nil) {
					continue
				}
				w.exprType(t)
			}
		} else {
			w.Sync(pkgbits.SyncExprList)
			w.exprs(c.Exprs)
		}

		// A type switch whose guard names a variable declares it again in
		// every clause, with the clause's own type.
		if s.Guard != nil && s.Guard.Named {
			w.pos(c.VarPos)
			w.typeUse(c.VarType)
			if c.Var == nil {
				w.refuse("clause %d of the type switch declares no variable, and its guard names one", i)
			}
			w.local(*c.Var)
		}

		w.stmts(c.Body)
	}
	if len(s.Clauses) > 0 {
		w.closeScope(s.ClausesClose)
	}
	w.closeScope(s.Close)
}

func (w *bodyWriter) selectStmt(s *SelectStmt) {
	w.Sync(pkgbits.SyncSelectStmt)
	w.pos(s.Pos)
	w.Len(len(s.Clauses))
	for i := range s.Clauses {
		c := &s.Clauses[i]
		if i > 0 {
			if c.ScopeClose == nil {
				w.refuse("clause %d of the select closes no scope, and every clause after the first does", i)
			}
			w.closeScope(*c.ScopeClose)
		}
		w.openScope(c.ScopeOpen)
		w.pos(c.Pos)
		w.stmts(c.Comm)
		w.stmts(c.Body)
	}
	if len(s.Clauses) > 0 {
		w.closeScope(s.Close)
	}
}

// @@@ Expressions

// mapRType writes the descriptor of a map where the format calls for one.
//
// The test is the reader's, on the same type, so the two sides agree by
// construction. A tree that carries a descriptor the format has no room for,
// or that leaves one out where the format has room, is refused: writing
// either would move every byte after it.
func (w *bodyWriter) mapRType(what string, typ types2.Type, rt *RType) {
	if typ == nil {
		w.refuse("the %s carries no type, so whether a map descriptor follows cannot be decided", what)
	}
	_, isMap := types2.CoreType(typ).(*types2.Map)
	switch {
	case isMap && rt == nil:
		w.refuse("the %s is a map and carries no descriptor", what)
	case !isMap && rt != nil:
		w.refuse("the %s is not a map and carries a descriptor", what)
	case isMap:
		w.rtype(*rt)
	}
}

// exprs writes a list of expressions.
func (w *bodyWriter) exprs(list []Expr) {
	w.Sync(pkgbits.SyncExprs)
	w.Len(len(list))
	for _, e := range list {
		w.expr(e)
	}
}

// optExpr writes an expression that may be absent.
func (w *bodyWriter) optExpr(e Expr) {
	if w.Bool(e != nil) {
		w.expr(e)
	}
}

// multiExpr writes a list of values.
func (w *bodyWriter) multiExpr(m MultiExpr) {
	w.Sync(pkgbits.SyncMultiExpr)
	if w.Bool(m.Single) {
		w.pos(m.Pos)
		w.expr(m.Expr)
		w.Len(len(m.Results))
		for _, res := range m.Results {
			w.typeUse(res.Src)
			if w.Bool(res.Converted) {
				w.typeUse(res.Dst)
				w.convRTTI(res.Conv)
			}
		}
		return
	}
	w.Len(len(m.Exprs))
	for _, e := range m.Exprs {
		w.expr(e)
	}
}

// expr writes one expression, with the reshape node the format wrote in front
// of it.
func (w *bodyWriter) expr(e Expr) {
	if e == nil {
		w.refuse("an expression the format requires is absent")
	}
	if rs := e.Reshape(); rs != nil {
		w.Code(ExprReshape)
		w.typeUse(*rs)
	}
	kind := e.exprKind()
	w.check.expr(kind)
	w.Code(kind)
	w.expr1(e)
}

func (w *bodyWriter) expr1(e Expr) {
	switch e := e.(type) {
	default:
		w.refuse("the expression is a %T, which the format has no encoding for", e)

	case *ConstExpr:
		w.pos(e.Pos)
		w.typeUse(e.Type)
		w.value(e.Value)

	case *ZeroExpr:
		w.pos(e.Pos)
		w.typeUse(e.Type)

	case *LocalExpr:
		w.Sync(pkgbits.SyncUseObjLocal)
		w.Bool(!e.Captured)
		w.Len(e.Index)

	case *GlobalExpr:
		w.objUse(e.Obj)

	case *FuncInstExpr:
		w.pos(e.Pos)
		w.funcInst(e.Inst)

	case *CompLitExpr:
		w.compLit(e)

	case *FuncLitExpr:
		w.funcLit(e)

	case *FieldValExpr:
		w.expr(e.X)
		w.pos(e.Pos)
		w.selector(e.Sel)

	case *MethodValExpr:
		w.expr(e.Recv)
		w.pos(e.Pos)
		w.methodRef(e.Method)

	case *MethodExprExpr:
		w.typeUse(e.Recv)
		w.implicits(e.Implicits)
		w.derefOrAddr(e.Deref, e.Addr)
		w.pos(e.Pos)
		w.methodRef(e.Method)

	case *RecvExpr:
		w.expr(e.X)
		w.pos(e.Pos)
		w.implicits(e.Implicits)
		w.derefOrAddr(e.Deref, e.Addr)

	case *IndexExpr:
		w.expr(e.X)
		w.pos(e.Pos)
		w.expr(e.Index)
		w.mapRType("indexed operand", exprTypeOf(e.X), e.MapRType)

	case *SliceExpr:
		w.expr(e.X)
		w.pos(e.Pos)
		for _, x := range e.Index {
			w.optExpr(x)
		}

	case *AssertExpr:
		w.expr(e.X)
		w.pos(e.Pos)
		w.exprType(&e.Type)
		w.rtype(e.Src)

	case *UnaryExpr:
		w.op(e.Op)
		w.pos(e.Pos)
		w.expr(e.X)

	case *BinaryExpr:
		w.op(e.Op)
		w.expr(e.X)
		w.pos(e.Pos)
		w.expr(e.Y)

	case *CallExpr:
		w.call(e)

	case *ConvertExpr:
		w.Bool(e.Implicit)
		w.typeUse(e.Type)
		w.pos(e.Pos)
		w.convRTTI(e.Conv)
		w.Bool(e.TypeParam)
		w.Bool(e.Identical)
		w.expr(e.X)

	case *NewExpr:
		w.pos(e.Pos)
		if w.Bool(e.Value != nil) {
			w.expr(e.Value)
			break
		}
		if e.Type == nil {
			w.refuse("new names neither a type nor a value")
		}
		w.exprType(e.Type)

	case *MakeExpr:
		w.pos(e.Pos)
		w.exprType(&e.Type)
		w.exprs(e.Args)
		w.rtype(e.RType)

	case *SizeExpr:
		w.pos(e.Pos)
		w.typeUse(e.Type)

	case *OffsetofExpr:
		w.pos(e.Pos)
		w.typeUse(e.Type)
		// The count is one less than the number of steps, because a
		// selection always goes through at least one field.
		if len(e.Path) == 0 {
			w.refuse("Offsetof names no field")
		}
		w.Len(len(e.Path) - 1)
		for _, f := range e.Path {
			w.Len(f)
		}

	case *RuntimeBuiltinExpr:
		w.str(e.Name)
	}
}

// implicits writes the path of embedded fields a selection goes through.
func (w *bodyWriter) implicits(path []int) {
	w.Len(len(path))
	for _, f := range path {
		w.Len(f)
	}
}

// derefOrAddr writes whether the receiver of a selection needs a dereference
// or an address. The second bool is written only when the first is false.
func (w *bodyWriter) derefOrAddr(deref, addr bool) {
	if !w.Bool(deref) {
		w.Bool(addr)
	}
}

// funcInst writes a reference to an instantiated generic function.
func (w *bodyWriter) funcInst(f FuncInst) {
	if w.Bool(f.Derived) {
		w.Len(w.refs.dictIdx("a function instantiation", f.DictIdx))
		return
	}
	w.objUse(f.Obj)
}

// methodRef writes a reference to the method a selection names.
func (w *bodyWriter) methodRef(m MethodRef) {
	w.typeUse(m.Recv)
	if w.Version().Has(pkgbits.GenericMethods) {
		w.Bool(m.Generic)
	} else if m.Generic {
		w.refuse("the method declares type parameters of its own, which the stream's version has no encoding for")
	}
	if !m.Generic {
		w.typeUse(m.Sig)
	}
	w.pos(m.Pos)
	w.selector(m.Sel)

	// A method on a type parameter is reached through the dictionary and
	// nothing further follows.
	if w.Bool(m.TypeParam) {
		w.Len(w.refs.dictIdx("a method expression on a type parameter", m.DictIdx))
		return
	}
	if w.Bool(m.Subdict) {
		w.Len(w.refs.dictIdx("a subdictionary", m.SubdictIdx))
		return
	}
	if w.Bool(m.StaticDict) {
		w.objUse(m.Dict)
	}
}

// call writes a call.
func (w *bodyWriter) call(e *CallExpr) {
	switch {
	case w.Bool(e.Method != nil):
		w.expr(e.Method.Recv)
		w.methodRef(e.Method.Method)
	case w.Bool(e.Inst != nil):
		w.pos(e.InstPos)
		w.funcInst(*e.Inst)
	default:
		w.expr(e.Fun)
	}
	w.pos(e.Pos)
	w.multiExpr(e.Args)
	w.Bool(e.Dots)

	// append, copy, delete and unsafe.Slice each need the descriptor of an
	// element or key type, and no other callee does.
	switch needs := needsCallRType(e.Fun); {
	case needs && e.RType == nil:
		w.refuse("the call is of a builtin that needs a runtime type and carries none")
	case !needs && e.RType != nil:
		w.refuse("the call carries a runtime type and its callee needs none")
	case needs:
		w.rtype(*e.RType)
	}
}

// compLit writes a composite literal.
func (w *bodyWriter) compLit(e *CompLitExpr) {
	w.Sync(pkgbits.SyncCompLit)
	w.pos(e.Pos)
	w.typeUse(e.Type)

	// The element encoding depends on what is being built, and a literal
	// carries its own type, so nothing outside the element is consulted.
	lit := e.Type.Type
	if lit == nil {
		w.refuse("the composite literal carries no type")
	}
	if ptr, ok := types2.CoreType(lit).(*types2.Pointer); ok {
		lit = ptr.Elem()
	}
	w.mapRType("composite literal", lit, e.MapRType)
	if !w.Version().Has(pkgbits.CompactCompLiterals) {
		w.refuse("the stream is older than the compact composite literal encoding, which nanogo does not write")
	}

	// A negative length says every element carries a key.
	n := len(e.Elems)
	if e.Keyed {
		n = -n
	}
	w.Int(n)

	switch core := types2.CoreType(lit).(type) {
	default:
		w.refuse("the composite literal builds a %T, which the format has no element encoding for", core)

	case *types2.Array, *types2.Slice:
		w.arrayElems(e)

	case *types2.Map:
		w.mapElems(e)

	case *types2.Struct:
		w.structElems(e)
	}
}

func (w *bodyWriter) arrayElems(e *CompLitExpr) {
	for i := range e.Elems {
		el := &e.Elems[i]
		if e.Keyed {
			if w.Bool(el.Key != nil) {
				w.pos(el.Pos)
				w.expr(el.Key)
			}
		} else if el.Key != nil {
			w.refuse("an element of the composite literal carries a key and the literal is written without keys")
		}
		w.expr(el.Value)
	}
}

func (w *bodyWriter) mapElems(e *CompLitExpr) {
	// Every element of a map literal has a key.
	for i := range e.Elems {
		el := &e.Elems[i]
		w.pos(el.Pos)
		w.expr(el.Key)
		w.expr(el.Value)
	}
}

func (w *bodyWriter) structElems(e *CompLitExpr) {
	for i := range e.Elems {
		el := &e.Elems[i]
		w.pos(el.Pos)
		if !e.Keyed {
			if el.Field != i {
				w.refuse("element %d of the composite literal names field %d, and a literal written without keys fills its fields in order", i, el.Field)
			}
			w.expr(el.Value)
			continue
		}
		// A negative index is the depth of a path through embedded fields
		// to a promoted field, and the steps follow it.
		if n := len(el.Embedded); n > 0 {
			w.Int(-n)
			for _, f := range el.Embedded {
				w.Int(f)
			}
		} else {
			w.Int(el.Field)
		}
		w.expr(el.Value)
	}
}

// funcLit writes a function literal and the reference to its body.
//
// The body is another element and is written by whatever holds the elements,
// because the literal only names it. See [elemRefs.bodyIdx].
func (w *bodyWriter) funcLit(e *FuncLitExpr) {
	w.Sync(pkgbits.SyncFuncLit)
	w.pos(e.Pos)

	w.Sync(pkgbits.SyncSignature)
	w.params(e.Params)
	w.params(e.Results)
	w.Bool(e.Variadic)
	w.Bool(e.RangeFuncBody)

	w.Len(len(e.Captured))
	for _, c := range e.Captured {
		w.pos(c.Pos)
		w.Sync(pkgbits.SyncUseObjLocal)
		w.Bool(!c.Local.Captured)
		w.Len(c.Local.Index)
	}

	w.Reloc(pkgbits.SectionBody, w.refs.bodyIdx(e))
	w.nested = append(w.nested, e)
}

// params writes one parameter list of a signature written inside a body.
func (w *bodyWriter) params(list []Param) {
	w.Sync(pkgbits.SyncParams)
	w.Len(len(list))
	for _, p := range list {
		w.Sync(pkgbits.SyncParam)
		w.pos(p.Pos)
		w.localIdent(p.Pkg, p.Name)
		w.typeUse(p.Type)
	}
}

// @@@ The resolver for a body that was read

// elemRefs answers with the index the archive a body was read from already
// gave each element the body names.
//
// It is what makes the encoder's oracle a byte comparison: encoding a decoded
// tree through this resolver has to reproduce gc's own element, table entry
// for table entry and byte for byte. It resolves nothing that the archive did
// not already resolve, so it proves the layout and says nothing about how a
// tree built from syntax would find an index.
//
// A type, an object, a position base and a body already carry their index in
// the tree. A string and a package do not, because the reader resolved each
// to its value, so both are looked up in a reverse map of the section.
type elemRefs struct {
	strs map[string]pkgbits.Index
	pkgs map[*types2.Package]pkgbits.Index
}

// newElemRefs builds the reverse maps of pr's string and package sections.
//
// A section that holds one value twice would make the reverse map a choice
// rather than an answer, so it is reported rather than resolved: gc's encoder
// interns both sections, so a repeat is a stream this resolver has no basis
// to encode back.
func newElemRefs(pr *pkgReader) (*elemRefs, error) {
	e := &elemRefs{
		strs: make(map[string]pkgbits.Index),
		pkgs: make(map[*types2.Package]pkgbits.Index),
	}
	for i := range pr.NumElems(pkgbits.SectionString) {
		idx := pkgbits.Index(i)
		s := pr.StringIdx(idx)
		if prev, ok := e.strs[s]; ok {
			return nil, fmt.Errorf("export: %s: string elements %d and %d both hold %q, so a string does not name one element",
				pr.PkgPath(), prev, idx, s)
		}
		e.strs[s] = idx
	}
	for i := range pr.NumElems(pkgbits.SectionPkg) {
		idx := pkgbits.Index(i)
		pkg := pr.pkgIdx(idx)
		if prev, ok := e.pkgs[pkg]; ok {
			return nil, fmt.Errorf("export: %s: package elements %d and %d are both %s, so a package does not name one element",
				pr.PkgPath(), prev, idx, pkgRefName(pkg))
		}
		e.pkgs[pkg] = idx
	}
	return e, nil
}

// pkgRefName names a package a message reports, including the universe's
// absent one.
func pkgRefName(pkg *types2.Package) string {
	if pkg == nil {
		return "the universe"
	}
	return pkg.Path()
}

func (e *elemRefs) strIdx(s string) pkgbits.Index {
	idx, ok := e.strs[s]
	if !ok {
		panicf("export: the body names the string %q and no element of the archive holds it", s)
	}
	return idx
}

func (e *elemRefs) pkgIdx(pkg *types2.Package) pkgbits.Index {
	idx, ok := e.pkgs[pkg]
	if !ok {
		panicf("export: the body names the package %s and no element of the archive holds it", pkgRefName(pkg))
	}
	return idx
}

func (e *elemRefs) typIdx(t TypeUse) pkgbits.Index       { return t.Idx }
func (e *elemRefs) objIdx(o ObjUse) pkgbits.Index        { return o.Idx }
func (e *elemRefs) posBaseIdx(p Pos) pkgbits.Index       { return p.Base }
func (e *elemRefs) bodyIdx(f *FuncLitExpr) pkgbits.Index { return f.Body }

// dictIdx answers with the slot the archive's own dictionary holds, which is
// the one the tree was decoded against.
func (e *elemRefs) dictIdx(_ string, slot int) int { return slot }

var _ bodyRefs = (*elemRefs)(nil)
