// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"go/constant"
	"go/token"
	"math/big"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// The body reader.
//
// It is cmd/compile/internal/noder/reader.go's function body half, decoding
// into the tree of body.go rather than into gc's IR. The divergences are in
// README.md. The one that shapes the file is that gc type checks each node as
// it builds it and reads the type off the result, where this reader takes the
// type out of the stream: gc writes a reshape node carrying the type in front
// of nearly every expression, so the four places the decoding depends on a
// type are answered without a second type checker.

// BodyError reports a body the reader cannot decode.
//
// The declaration is named, because it is what a user has to work around and
// what specs/015-export-data.md's census counts.
type BodyError struct {
	Package string // the package the body was read from or written for
	Name    string // the declaration the body belongs to
	Reason  string // what the reader cannot decode or the encoder cannot write

	// Writing is true for a body the encoder refused. The two halves refuse
	// different things, and a message that names one about the other sends a
	// user to the wrong side of the format.
	Writing bool
}

func (e *BodyError) Error() string {
	verb := "read"
	if e.Writing {
		verb = "write"
	}
	return fmt.Sprintf("export: %s: cannot %s the body of %s: %s", e.Package, verb, e.Name, e.Reason)
}

// bodyReader decodes one body element.
type bodyReader struct {
	*reader

	// name is the declaration the body belongs to, for a refusal.
	name string

	// relocs records every reference this element resolved, so that a
	// reference the decoder skipped can be told from one it read. The
	// element's own table is the answer: gc's encoder adds an entry only
	// where it writes one, so an entry nothing resolved is a field that was
	// not decoded.
	relocs map[pkgbits.RefTableEntry]bool
}

// refuse reports a body the reader cannot decode.
func (r *bodyReader) refuse(format string, args ...any) {
	panic(&BodyError{Package: r.p.PkgPath(), Name: r.name, Reason: fmt.Sprintf(format, args...)})
}

// reloc reads a reference and records that it was read.
func (r *bodyReader) reloc(k pkgbits.SectionKind) pkgbits.Index {
	idx := r.Reloc(k)
	r.relocs[pkgbits.RefTableEntry{Kind: k, Idx: idx}] = true
	return idx
}

// unread returns the reference table entries no field of this element
// resolved. gc writes a table entry only where it writes a reference, so a
// leftover entry names a field the decoder passed over.
func (r *bodyReader) unread() []pkgbits.RefTableEntry {
	var left []pkgbits.RefTableEntry
	for _, e := range r.Relocs {
		if !r.relocs[e] {
			left = append(left, e)
		}
	}
	return left
}

// decodeBody decodes the body element at idx.
//
// nparams is the number of locals the body declares before its first
// statement, which is the receiver plus the parameters plus the results of
// the signature that names the body. It is not in the element, so a caller
// that does not have it cannot decode a body at all.
//
// The decode is exact. A body that leaves bytes of the element unread, or a
// reference in the element's table that no field resolved, is refused rather
// than returned: gc's encoder writes a table entry only where it writes a
// reference, so either is a field this reader passed over.
func (pr *pkgReader) decodeBody(idx pkgbits.Index, dict *readerDict, nparams int, name string) *Body {
	r := &bodyReader{
		reader: pr.newReader(pkgbits.SectionBody, idx, pkgbits.SyncFuncBody),
		name:   name,
		relocs: make(map[pkgbits.RefTableEntry]bool),
	}
	r.dict = dict

	body := &Body{Params: make([]Local, nparams)}
	for i := range body.Params {
		body.Params[i] = r.local()
	}
	if body.HasBlock = r.Bool(); body.HasBlock {
		body.Stmts = r.stmts()
		body.Rbrace = r.pos()
	}
	if n := r.Data.Len(); n != 0 {
		r.refuse("%d bytes of body element %d were not decoded", n, idx)
	}
	if left := r.unread(); len(left) != 0 {
		r.refuse("body element %d holds %d reference(s) no field resolved, the first into %v",
			idx, len(left), left[0].Kind)
	}
	return body
}

// local decodes one local variable declaration.
func (r *bodyReader) local() Local {
	r.Sync(pkgbits.SyncAddLocal)
	// The locals table index follows only where the writer wrote sync
	// markers, and nanogo's container never does (README.md).
	l := Local{DictRType: -1}
	if r.Bool() {
		l.DictRType = r.Len()
	}
	return l
}

// @@@ Positions

// pos decodes a position.
//
// The base is kept as an index and is not resolved: nanogo has no coordinate
// space for a file of another package, and the element the index names is
// therefore never visited. The line and the column are inline in this
// element, so skipping them would desync everything after.
func (r *bodyReader) pos() Pos {
	r.Sync(pkgbits.SyncPos)
	var p Pos
	if p.Known = r.Bool(); !p.Known {
		return p
	}
	p.Base = r.reloc(pkgbits.SectionPosBase)
	p.Line = r.Uint()
	p.Col = r.Uint()
	return p
}

// @@@ References

// pkg decodes a package reference.
func (r *bodyReader) pkg() *types2.Package {
	r.Sync(pkgbits.SyncPkg)
	return r.p.pkgIdx(r.reloc(pkgbits.SectionPkg))
}

// str decodes a string reference.
func (r *bodyReader) str() string {
	r.Sync(pkgbits.SyncString)
	return r.p.StringIdx(r.reloc(pkgbits.SectionString))
}

// typeUse decodes a use of a type.
func (r *bodyReader) typeUse() TypeUse {
	r.Sync(pkgbits.SyncType)
	var t TypeUse
	if t.Derived = r.Bool(); t.Derived {
		t.Idx = pkgbits.Index(r.Len())
		if int(t.Idx) >= len(r.dict.derived) {
			r.refuse("a derived type names dictionary slot %d of %d", t.Idx, len(r.dict.derived))
		}
	} else {
		t.Idx = r.reloc(pkgbits.SectionType)
	}
	t.Type = r.p.typIdx(typeInfo{idx: t.Idx, derived: t.Derived}, r.dict)
	return t
}

// objUse decodes a use of a package-scope declaration.
func (r *bodyReader) objUse() ObjUse {
	r.Sync(pkgbits.SyncObject)
	if r.Version().Has(pkgbits.DerivedFuncInstance) {
		assert(!r.Bool())
	}
	return r.objUseAt(r.reloc(pkgbits.SectionObj))
}

// objUseAt decodes the type arguments of a use of the declaration at idx.
func (r *bodyReader) objUseAt(idx pkgbits.Index) ObjUse {
	pkg, name := r.p.objIdx(idx)
	use := ObjUse{Idx: idx, Pkg: pkg, Name: name}
	switch {
	case pkg != nil:
		use.Obj = pkg.Scope().Lookup(name)
	case name != "":
		// The universe, whose declarations are stubs. A builtin reaches a
		// body as the callee of a call, and the call's shape depends on
		// which builtin it is.
		use.Obj = types2.Universe.Lookup(name)
	}
	use.Targs = make([]TypeUse, r.Len())
	for i := range use.Targs {
		use.Targs[i] = r.typeUse()
	}
	return use
}

// selector decodes the name of a field or a method.
func (r *bodyReader) selector() Selector {
	r.Sync(pkgbits.SyncSelector)
	return Selector{Pkg: r.pkg(), Name: r.str()}
}

// localIdent decodes the name of a local variable.
func (r *bodyReader) localIdent() (*types2.Package, string) {
	r.Sync(pkgbits.SyncLocalIdent)
	return r.pkg(), r.str()
}

// value decodes a constant.
//
// pkgbits decodes one too, and this reads the same shape: a string, a big
// integer and a big float each reach the bitstream as a string reference, and
// a reference read through the container is a reference this element's table
// coverage check would not see.
func (r *bodyReader) value() constant.Value {
	r.Sync(pkgbits.SyncValue)
	isComplex := r.Bool()
	val := r.scalar()
	if isComplex {
		val = constant.BinaryOp(val, token.ADD, constant.MakeImag(r.scalar()))
	}
	return val
}

func (r *bodyReader) scalar() constant.Value {
	switch tag := pkgbits.CodeVal(r.Code(pkgbits.SyncVal)); tag {
	default:
		r.refuse("the constant is encoding %d, which the format has no tag for", int(tag))
		panic("unreachable")
	case pkgbits.ValBool:
		return constant.MakeBool(r.Bool())
	case pkgbits.ValString:
		return constant.MakeString(r.str())
	case pkgbits.ValInt64:
		return constant.MakeInt64(r.Int64())
	case pkgbits.ValBigInt:
		return constant.Make(r.bigInt())
	case pkgbits.ValBigRat:
		num := r.bigInt()
		denom := r.bigInt()
		return constant.Make(new(big.Rat).SetFrac(num, denom))
	case pkgbits.ValBigFloat:
		v := new(big.Float).SetPrec(512)
		if err := v.UnmarshalText([]byte(r.str())); err != nil {
			r.refuse("the constant is a floating point value the container cannot parse: %v", err)
		}
		return constant.Make(v)
	}
}

func (r *bodyReader) bigInt() *big.Int {
	v := new(big.Int).SetBytes([]byte(r.str()))
	if r.Bool() {
		v.Neg(v)
	}
	return v
}

// label decodes a label.
func (r *bodyReader) label() string {
	r.Sync(pkgbits.SyncLabel)
	return r.str()
}

// optLabel decodes a label that may be absent.
func (r *bodyReader) optLabel() (string, bool) {
	r.Sync(pkgbits.SyncOptLabel)
	if r.Bool() {
		return r.label(), true
	}
	return "", false
}

// op decodes an operator, and refuses one a body cannot carry.
func (r *bodyReader) op() Op {
	r.Sync(pkgbits.SyncOp)
	op := Op(r.Len())
	if !op.known() {
		r.refuse("the operator is gc's ir.Op %d, which no statement or expression of a body writes", int(op))
	}
	return op
}

// @@@ Runtime type information

// rtype decodes a runtime type descriptor reference.
func (r *bodyReader) rtype() RType {
	r.Sync(pkgbits.SyncRType)
	var t RType
	if t.Derived = r.Bool(); t.Derived {
		t.DictIdx = r.Len()
		if t.DictIdx >= len(r.dict.rtypes) {
			r.refuse("a runtime type names dictionary slot %d of %d", t.DictIdx, len(r.dict.rtypes))
		}
		return t
	}
	t.Type = r.typeUse()
	return t
}

// itab decodes the pair of descriptors a conversion needs.
func (r *bodyReader) itab() ConvRTTI {
	var c ConvRTTI
	c.Src = r.rtype()
	c.Dst = r.rtype()
	if c.Derived = r.Bool(); c.Derived {
		c.DictIdx = r.Len()
	}
	return c
}

// convRTTI decodes the descriptors a conversion from one type to another
// needs.
func (r *bodyReader) convRTTI() ConvRTTI {
	r.Sync(pkgbits.SyncConvRTTI)
	return r.itab()
}

// exprType decodes a type written where an expression is expected.
func (r *bodyReader) exprType() *ExprType {
	r.Sync(pkgbits.SyncExprType)
	t := &ExprType{Pos: r.pos()}
	if r.Bool() {
		conv := r.itab()
		t.Itab = &conv
		return t
	}
	rt := r.rtype()
	t.RType = &rt
	t.Derived = r.Bool()
	return t
}

// @@@ Statements

// stmts decodes a statement list.
func (r *bodyReader) stmts() []Stmt {
	r.Sync(pkgbits.SyncStmts)
	var out []Stmt
	for {
		kind := StmtKind(r.Code(pkgbits.SyncStmt1))
		if kind == StmtEnd {
			r.Sync(pkgbits.SyncStmtsEnd)
			return out
		}
		out = append(out, r.stmt(kind))
	}
}

// stmt decodes one statement of the given kind.
//
// A label is flat: the statement it labels is the next entry of the same
// list, which is how gc writes it and why nothing is nested here.
func (r *bodyReader) stmt(kind StmtKind) Stmt {
	switch kind {
	default:
		r.refuse("the statement is %v, which the format has no encoding for", kind)
		panic("unreachable")

	case StmtLabel:
		return &LabelStmt{Pos: r.pos(), Label: r.label()}

	case StmtBlock:
		return r.blockStmt()

	case StmtExpr:
		return &ExprStmt{X: r.expr()}

	case StmtSend:
		return &SendStmt{Pos: r.pos(), Chan: r.expr(), Value: r.expr()}

	case StmtAssign:
		s := &AssignStmt{Pos: r.pos()}
		s.Lhs = r.assignList()
		s.Rhs = r.multiExpr()
		return s

	case StmtAssignOp:
		return &AssignOpStmt{Op: r.op(), Lhs: r.expr(), Pos: r.pos(), Rhs: r.expr()}

	case StmtIncDec:
		return &IncDecStmt{Op: r.op(), X: r.expr(), Pos: r.pos()}

	case StmtBranch:
		s := &BranchStmt{Pos: r.pos(), Op: r.op()}
		s.Label, s.Labelled = r.optLabel()
		return s

	case StmtCall:
		s := &CallStmt{Pos: r.pos(), Op: r.op(), Call: r.expr()}
		if s.Op == OpDefer {
			s.DeferAt = r.optExpr()
		}
		return s

	case StmtReturn:
		s := &ReturnStmt{Pos: r.pos()}
		s.Results = r.multiExpr()
		return s

	case StmtIf:
		return r.ifStmt()

	case StmtFor:
		return r.forStmt()

	case StmtSwitch:
		return r.switchStmt()

	case StmtSelect:
		return r.selectStmt()
	}
}

// assignList decodes the destinations of an assignment.
func (r *bodyReader) assignList() []Assignee {
	out := make([]Assignee, r.Len())
	for i := range out {
		out[i] = r.assign()
	}
	return out
}

// assign decodes one destination of an assignment.
func (r *bodyReader) assign() Assignee {
	kind := AssignKind(r.Code(pkgbits.SyncAssign))
	a := Assignee{Kind: kind}
	switch kind {
	default:
		r.refuse("the assignment destination is %v, which the format has no encoding for", kind)

	case AssignBlank:

	case AssignDef:
		a.Pos = r.pos()
		a.Pkg, a.Name = r.localIdent()
		a.Type = r.typeUse()
		a.Local = r.local()

	case AssignExpr:
		a.Expr = r.expr()
	}
	return a
}

// blockStmt decodes a block and the scope it opens.
func (r *bodyReader) blockStmt() *BlockStmt {
	r.Sync(pkgbits.SyncBlockStmt)
	b := &BlockStmt{Open: r.openScope()}
	b.Body = r.stmts()
	b.Close = r.closeScope()
	return b
}

func (r *bodyReader) openScope() Pos {
	r.Sync(pkgbits.SyncOpenScope)
	return r.pos()
}

func (r *bodyReader) closeScope() Pos {
	r.Sync(pkgbits.SyncCloseScope)
	p := r.pos()
	r.Sync(pkgbits.SyncCloseAnotherScope)
	return p
}

func (r *bodyReader) ifStmt() *IfStmt {
	r.Sync(pkgbits.SyncIfStmt)
	s := &IfStmt{Open: r.openScope()}
	s.Pos = r.pos()
	s.Init = r.stmts()
	s.Cond = r.expr()

	// The constant value of the condition, if it has one. gc drops the
	// branch it proved unreachable, so the element holds one arm or the
	// other and a decoder that read both would desync.
	s.Static = r.Int()
	if s.Static >= 0 {
		s.Then = r.blockStmt()
	} else {
		s.ThenClose = r.pos()
	}
	if s.Static <= 0 {
		s.Else = r.stmts()
	}
	r.Sync(pkgbits.SyncCloseAnotherScope)
	return s
}

func (r *bodyReader) forStmt() *ForStmt {
	r.Sync(pkgbits.SyncForStmt)
	s := &ForStmt{Open: r.openScope()}

	if r.Bool() {
		c := &RangeClause{Pos: r.pos()}
		c.Lhs = r.assignList()
		c.X = r.expr()

		// The operand's type decides whether the descriptor of a map
		// follows, and the destinations decide whether a conversion
		// follows for the key and for the value. The operand's type is in
		// the stream, because gc writes a reshape node carrying it in
		// front of the expression.
		//
		// The test is types2.CoreType, which is the writer's own test on
		// the writer's own type, so the two sides agree by construction
		// and not by this reader guessing what gc decided.
		xtyp := exprTypeOf(c.X)
		if xtyp == nil {
			r.refuse("the range operand carries no type, so whether a map descriptor follows cannot be decided")
		}
		if _, isMap := types2.CoreType(xtyp).(*types2.Map); isMap {
			rt := r.rtype()
			c.MapRType = &rt
		}
		if len(c.Lhs) > 0 && c.Lhs[0].Kind != AssignBlank {
			conv := r.convRTTI()
			c.KeyConv = &conv
		}
		if len(c.Lhs) > 1 && c.Lhs[1].Kind != AssignBlank {
			conv := r.convRTTI()
			c.ValueConv = &conv
		}
		s.Range = c
	} else {
		s.Pos = r.pos()
		s.Init = r.stmts()
		s.Cond = r.optExpr()
		s.Post = r.stmts()
	}

	s.Body = r.blockStmt()
	s.DistinctVars = r.Bool()
	r.Sync(pkgbits.SyncCloseAnotherScope)
	return s
}

func (r *bodyReader) switchStmt() *SwitchStmt {
	r.Sync(pkgbits.SyncSwitchStmt)
	s := &SwitchStmt{Open: r.openScope()}
	s.Pos = r.pos()
	s.Init = r.stmts()

	if r.Bool() {
		g := &TypeSwitchGuard{Pos: r.pos()}
		if g.Named = r.Bool(); g.Named {
			g.NamePos = r.pos()
			// Not localIdent: the guard's variable has no object, so gc
			// writes the package and the name without one.
			r.Sync(pkgbits.SyncLocalIdent)
			g.Pkg = r.pkg()
			g.Name = r.str()
		}
		g.X = r.expr()
		s.Guard = g
	} else {
		s.Tag = r.optExpr()
	}

	s.Clauses = make([]CaseClause, r.Len())
	for i := range s.Clauses {
		c := &s.Clauses[i]
		if i > 0 {
			p := r.closeScope()
			c.ScopeClose = &p
		}
		c.ScopeOpen = r.openScope()
		c.Pos = r.pos()

		if s.Guard != nil {
			c.Types = make([]*ExprType, r.Len())
			for j := range c.Types {
				if r.Bool() { // case nil
					continue
				}
				c.Types[j] = r.exprType()
			}
		} else {
			r.Sync(pkgbits.SyncExprList)
			c.Exprs = r.exprs()
		}

		// A type switch whose guard names a variable declares it again in
		// every clause, with the clause's own type.
		if s.Guard != nil && s.Guard.Named {
			c.VarPos = r.pos()
			c.VarType = r.typeUse()
			l := r.local()
			c.Var = &l
		}

		c.Body = r.stmts()
	}
	if len(s.Clauses) > 0 {
		s.ClausesClose = r.closeScope()
	}
	s.Close = r.closeScope()
	return s
}

func (r *bodyReader) selectStmt() *SelectStmt {
	r.Sync(pkgbits.SyncSelectStmt)
	s := &SelectStmt{Pos: r.pos()}
	s.Clauses = make([]CommClause, r.Len())
	for i := range s.Clauses {
		c := &s.Clauses[i]
		if i > 0 {
			p := r.closeScope()
			c.ScopeClose = &p
		}
		c.ScopeOpen = r.openScope()
		c.Pos = r.pos()
		c.Comm = r.stmts()
		c.Body = r.stmts()
	}
	if len(s.Clauses) > 0 {
		s.Close = r.closeScope()
	}
	return s
}

// @@@ Expressions

// exprTypeOf returns the type of an expression, or nil where the stream
// carried none.
func exprTypeOf(e Expr) types2.Type {
	if e == nil {
		return nil
	}
	return e.ExprType()
}

// exprs decodes a list of expressions.
func (r *bodyReader) exprs() []Expr {
	r.Sync(pkgbits.SyncExprs)
	out := make([]Expr, r.Len())
	for i := range out {
		out[i] = r.expr()
	}
	return out
}

// optExpr decodes an expression that may be absent.
func (r *bodyReader) optExpr() Expr {
	if r.Bool() {
		return r.expr()
	}
	return nil
}

// multiExpr decodes a list of values.
func (r *bodyReader) multiExpr() MultiExpr {
	r.Sync(pkgbits.SyncMultiExpr)
	var m MultiExpr
	if m.Single = r.Bool(); m.Single {
		m.Pos = r.pos()
		m.Expr = r.expr()
		m.Results = make([]MultiResult, r.Len())
		for i := range m.Results {
			res := &m.Results[i]
			res.Src = r.typeUse()
			if res.Converted = r.Bool(); res.Converted {
				res.Dst = r.typeUse()
				res.Conv = r.convRTTI()
			}
		}
		return m
	}
	m.Exprs = make([]Expr, r.Len())
	for i := range m.Exprs {
		m.Exprs[i] = r.expr()
	}
	return m
}

// expr decodes one expression.
//
// A reshape node carries the type of the expression that follows it and is
// otherwise transparent, so it is unwrapped here and its type is attached to
// the node it wrapped. That is what makes the four type-dependent decisions
// below decidable without type checking the tree again.
func (r *bodyReader) expr() Expr {
	kind := ExprKind(r.Code(pkgbits.SyncExpr))
	if kind != ExprReshape {
		return r.expr1(kind, nil)
	}
	rs := r.typeUse()
	inner := ExprKind(r.Code(pkgbits.SyncExpr))
	if inner == ExprReshape {
		r.refuse("two reshape nodes in a row, which the writer never produces")
	}
	return r.expr1(inner, &rs)
}

// expr1 decodes one expression of the given kind, with the reshape node that
// preceded it or nil.
func (r *bodyReader) expr1(kind ExprKind, rs *TypeUse) Expr {
	et := exprType{reshape: rs}
	if rs != nil {
		et.typ = rs.Type
	}
	switch kind {
	default:
		r.refuse("the expression is %v, which the format has no encoding for", kind)
		panic("unreachable")

	case ExprConst:
		e := &ConstExpr{exprType: et, Pos: r.pos()}
		e.Type = r.typeUse()
		if e.typ == nil {
			e.typ = e.Type.Type
		}
		e.Value = r.value()
		return e

	case ExprZero:
		e := &ZeroExpr{exprType: et, Pos: r.pos()}
		e.Type = r.typeUse()
		if e.typ == nil {
			e.typ = e.Type.Type
		}
		return e

	case ExprLocal:
		r.Sync(pkgbits.SyncUseObjLocal)
		e := &LocalExpr{exprType: et}
		e.Captured = !r.Bool()
		e.Index = r.Len()
		return e

	case ExprGlobal:
		return &GlobalExpr{exprType: et, Obj: r.objUse()}

	case ExprFuncInst:
		e := &FuncInstExpr{exprType: et, Pos: r.pos()}
		e.Inst = r.funcInst()
		return e

	case ExprCompLit:
		return r.compLit(et)

	case ExprFuncLit:
		return r.funcLit(et)

	case ExprFieldVal:
		e := &FieldValExpr{exprType: et}
		e.X = r.expr()
		e.Pos = r.pos()
		e.Sel = r.selector()
		return e

	case ExprMethodVal:
		e := &MethodValExpr{exprType: et}
		e.Recv = r.expr()
		e.Pos = r.pos()
		e.Method = r.methodRef()
		return e

	case ExprMethodExpr:
		e := &MethodExprExpr{exprType: et}
		e.Recv = r.typeUse()
		e.Implicits = make([]int, r.Len())
		for i := range e.Implicits {
			e.Implicits[i] = r.Len()
		}
		if e.Deref = r.Bool(); !e.Deref {
			e.Addr = r.Bool()
		}
		e.Pos = r.pos()
		e.Method = r.methodRef()
		return e

	case ExprRecv:
		e := &RecvExpr{exprType: et}
		e.X = r.expr()
		e.Pos = r.pos()
		e.Implicits = make([]int, r.Len())
		for i := range e.Implicits {
			e.Implicits[i] = r.Len()
		}
		if e.Deref = r.Bool(); !e.Deref {
			e.Addr = r.Bool()
		}
		return e

	case ExprIndex:
		e := &IndexExpr{exprType: et}
		e.X = r.expr()
		e.Pos = r.pos()
		e.Index = r.expr()

		// The descriptor of a map follows only where the operand is one,
		// by the writer's own test on the writer's own type.
		xtyp := exprTypeOf(e.X)
		if xtyp == nil {
			r.refuse("the indexed operand carries no type, so whether a map descriptor follows cannot be decided")
		}
		if _, isMap := types2.CoreType(xtyp).(*types2.Map); isMap {
			rt := r.rtype()
			e.MapRType = &rt
		}
		return e

	case ExprSlice:
		e := &SliceExpr{exprType: et}
		e.X = r.expr()
		e.Pos = r.pos()
		for i := range e.Index {
			e.Index[i] = r.optExpr()
		}
		return e

	case ExprAssert:
		e := &AssertExpr{exprType: et}
		e.X = r.expr()
		e.Pos = r.pos()
		e.Type = *r.exprType()
		e.Src = r.rtype()
		return e

	case ExprUnaryOp:
		e := &UnaryExpr{exprType: et}
		e.Op = r.op()
		e.Pos = r.pos()
		e.X = r.expr()
		return e

	case ExprBinaryOp:
		e := &BinaryExpr{exprType: et}
		e.Op = r.op()
		e.X = r.expr()
		e.Pos = r.pos()
		e.Y = r.expr()
		return e

	case ExprCall:
		return r.call(et)

	case ExprConvert:
		e := &ConvertExpr{exprType: et}
		e.Implicit = r.Bool()
		e.Type = r.typeUse()
		if e.typ == nil {
			e.typ = e.Type.Type
		}
		e.Pos = r.pos()
		e.Conv = r.convRTTI()
		e.TypeParam = r.Bool()
		e.Identical = r.Bool()
		e.X = r.expr()
		return e

	case ExprNew:
		e := &NewExpr{exprType: et, Pos: r.pos()}
		if r.Bool() {
			e.Value = r.expr()
		} else {
			e.Type = r.exprType()
		}
		return e

	case ExprMake:
		e := &MakeExpr{exprType: et, Pos: r.pos()}
		e.Type = *r.exprType()
		e.Args = r.exprs()
		e.RType = r.rtype()
		return e

	case ExprSizeof, ExprAlignof:
		e := &SizeExpr{exprType: et, Kind: kind, Pos: r.pos()}
		e.Type = r.typeUse()
		return e

	case ExprOffsetof:
		e := &OffsetofExpr{exprType: et, Pos: r.pos()}
		e.Type = r.typeUse()
		// The count is one less than the number of steps, because a
		// selection always goes through at least one field.
		e.Path = make([]int, r.Len()+1)
		for i := range e.Path {
			e.Path[i] = r.Len()
		}
		return e

	case ExprRuntimeBuiltin:
		return &RuntimeBuiltinExpr{exprType: et, Name: r.str()}
	}
}

// funcInst decodes a reference to an instantiated generic function.
func (r *bodyReader) funcInst() FuncInst {
	var f FuncInst
	if f.Derived = r.Bool(); f.Derived {
		f.DictIdx = r.Len()
		return f
	}
	f.Obj = r.objUse()
	return f
}

// methodRef decodes a reference to the method a selection names.
func (r *bodyReader) methodRef() MethodRef {
	var m MethodRef
	m.Recv = r.typeUse()
	if r.Version().Has(pkgbits.GenericMethods) {
		m.Generic = r.Bool()
	}
	if !m.Generic {
		m.Sig = r.typeUse()
	}
	m.Pos = r.pos()
	m.Sel = r.selector()

	// A method on a type parameter is reached through the dictionary and
	// nothing further follows.
	if m.TypeParam = r.Bool(); m.TypeParam {
		m.DictIdx = r.Len()
		return m
	}
	if m.Subdict = r.Bool(); m.Subdict {
		m.SubdictIdx = r.Len()
		return m
	}
	if m.StaticDict = r.Bool(); m.StaticDict {
		m.Dict = r.objUse()
	}
	return m
}

// call decodes a call.
func (r *bodyReader) call(et exprType) Expr {
	e := &CallExpr{exprType: et}
	switch {
	case r.Bool(): // a call through a method selection
		c := &MethodCall{}
		c.Recv = r.expr()
		c.Method = r.methodRef()
		e.Method = c
	case r.Bool(): // a call of an instantiated generic function
		e.InstPos = r.pos()
		inst := r.funcInst()
		e.Inst = &inst
	default:
		e.Fun = r.expr()
	}
	e.Pos = r.pos()
	e.Args = r.multiExpr()
	e.Dots = r.Bool()

	// append, copy, delete and unsafe.Slice each need the descriptor of an
	// element or key type, and no other callee does. Which builtin it is
	// comes out of the callee, which is a reference to the universe's
	// declaration of it.
	if needsCallRType(e.Fun) {
		rt := r.rtype()
		e.RType = &rt
	}
	return e
}

// needsCallRType reports whether a call of fun is followed by a runtime type
// descriptor.
//
// The four are the builtins whose lowering needs the element or key type at
// run time. A user declaration of the same name is a different object in a
// different package, so the test is on the object and not on the name alone.
func needsCallRType(fun Expr) bool {
	g, ok := fun.(*GlobalExpr)
	if !ok {
		return false
	}
	switch g.Obj.Name {
	case "append", "copy", "delete":
		return g.Obj.Pkg == nil
	case "Slice":
		return g.Obj.Pkg == types2.Unsafe
	}
	return false
}

// compLit decodes a composite literal.
func (r *bodyReader) compLit(et exprType) Expr {
	r.Sync(pkgbits.SyncCompLit)
	e := &CompLitExpr{exprType: et, Pos: r.pos()}
	e.Type = r.typeUse()

	// The element encoding depends on what is being built, and a literal
	// carries its own type, so nothing outside the element is consulted.
	lit := e.Type.Type
	if lit == nil {
		r.refuse("the composite literal carries no type")
	}
	if ptr, ok := types2.CoreType(lit).(*types2.Pointer); ok {
		lit = ptr.Elem()
	}
	if _, isMap := types2.CoreType(lit).(*types2.Map); isMap {
		rt := r.rtype()
		e.MapRType = &rt
	}
	if !r.Version().Has(pkgbits.CompactCompLiterals) {
		r.refuse("the stream is older than the compact composite literal encoding, which nanogo does not read")
	}

	n := r.Int()
	e.Keyed = n < 0
	count := n
	if count < 0 {
		count = -count
	}
	e.Elems = make([]LitElem, count)

	switch core := types2.CoreType(lit).(type) {
	default:
		r.refuse("the composite literal builds a %T, which the format has no element encoding for", core)

	case *types2.Array, *types2.Slice:
		r.arrayElems(e)

	case *types2.Map:
		r.mapElems(e)

	case *types2.Struct:
		r.structElems(e)
	}
	return e
}

func (r *bodyReader) arrayElems(e *CompLitExpr) {
	for i := range e.Elems {
		el := &e.Elems[i]
		if e.Keyed && r.Bool() {
			el.Pos = r.pos()
			el.Key = r.expr()
		}
		el.Value = r.expr()
	}
}

func (r *bodyReader) mapElems(e *CompLitExpr) {
	// Every element of a map literal has a key.
	for i := range e.Elems {
		el := &e.Elems[i]
		el.Pos = r.pos()
		el.Key = r.expr()
		el.Value = r.expr()
	}
}

func (r *bodyReader) structElems(e *CompLitExpr) {
	for i := range e.Elems {
		el := &e.Elems[i]
		el.Pos = r.pos()
		if !e.Keyed {
			el.Field = i
			el.Value = r.expr()
			continue
		}
		// A negative index is the depth of a path through embedded fields
		// to a promoted field, and the steps follow it.
		if n := r.Int(); n < 0 {
			el.Embedded = make([]int, -n)
			for j := range el.Embedded {
				el.Embedded[j] = r.Int()
			}
			el.Field = el.Embedded[len(el.Embedded)-1]
		} else {
			el.Field = n
		}
		el.Value = r.expr()
	}
}

// funcLit decodes a function literal, and the body it names.
func (r *bodyReader) funcLit(et exprType) Expr {
	r.Sync(pkgbits.SyncFuncLit)
	e := &FuncLitExpr{exprType: et, Pos: r.pos()}

	r.Sync(pkgbits.SyncSignature)
	e.Params = r.params()
	e.Results = r.params()
	e.Variadic = r.Bool()
	e.RangeFuncBody = r.Bool()

	e.Captured = make([]CapturedVar, r.Len())
	for i := range e.Captured {
		c := &e.Captured[i]
		c.Pos = r.pos()
		r.Sync(pkgbits.SyncUseObjLocal)
		c.Local.Captured = !r.Bool()
		c.Local.Index = r.Len()
	}

	e.Body = r.reloc(pkgbits.SectionBody)

	// The literal's own body is another element, and its locals begin with
	// the literal's parameters and results. A literal has no receiver, so
	// the two lists together are its whole parameter count.
	e.Decoded = r.p.decodeBody(e.Body, r.dict, len(e.Params)+len(e.Results), r.name+", in a function literal")
	r.p.nested++
	return e
}

// params decodes one parameter list of a signature written inside a body.
func (r *bodyReader) params() []Param {
	r.Sync(pkgbits.SyncParams)
	out := make([]Param, r.Len())
	for i := range out {
		p := &out[i]
		r.Sync(pkgbits.SyncParam)
		p.Pos = r.pos()
		p.Pkg, p.Name = r.localIdent()
		p.Type = r.typeUse()
	}
	return out
}
