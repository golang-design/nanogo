// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"go/constant"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// The tree a function body decodes into.
//
// It is neither syntax nor IR. It is gc's body encoding, named:
// specs/015-export-data.md says why, and the short form is that the encoding
// has nodes no source can spell, that types2.Info cannot be filled from
// outside types2, and that ir.Build takes a package rather than a function.
// Its consumer is the stenciler of specs/013-generics.md, which substitutes
// the type arguments through it.
//
// Every node holds what the format holds, with two additions that cost
// nothing and that a consumer cannot recompute: a type reference carries the
// resolved [types2.Type] beside its index, and a position carries the raw
// triple rather than being dropped. Nothing is normalised away, so the tree
// is what the body writer encodes back: [WriteBody] writes the same bytes
// the tree was read from.

// A Body is one function body, decoded from one element of SectionBody.
//
// The element does not say how many parameters the declaration has, so it
// cannot be decoded on its own: Params is as long as the receiver plus the
// parameters plus the results of the signature that names the body. See
// [ReadBody].
type Body struct {
	// Params is one entry per receiver, parameter and result, in that
	// order. They are the first locals, and a local is referred to by its
	// index in declaration order.
	Params []Local

	// HasBlock is false for a declaration with no body, which is a function
	// implemented in assembly or by a linkname.
	HasBlock bool

	// Stmts is the body, and Rbrace is its closing brace.
	Stmts  []Stmt
	Rbrace Pos
}

// A Local is one local variable's declaration in a body.
//
// The variable has no name and no object here. gc numbers locals in
// declaration order and refers to one by its number, so a body carries its
// own local numbering and nothing outside the body can name a local.
type Local struct {
	// DictRType is the runtime type slot of a local whose type is derived
	// from the enclosing declaration's type parameters, and -1 for a local
	// whose type is known without a dictionary.
	DictRType int
}

// A Pos is a source position, as the format carries it.
//
// nanogo has no coordinate space for a file of another package
// (specs/010-scanner-and-positions.md), so a position read from an archive is
// not turned into a [syntax.Pos]. Known is false for the absent position.
//
// The base is named twice, and the two names have different owners. Base is
// the SectionPosBase element of the archive the position was read from, and
// is meaningless in a position built from syntax. File is the file name that
// element holds, and is the only name a position built from syntax has: a
// writer allocates the element from it.
type Pos struct {
	Known bool
	Base  pkgbits.Index
	File  string
	Line  uint
	Col   uint
}

// A TypeUse is a reference to a type from inside a body.
//
// A derived type is one whose identity depends on the enclosing
// declaration's type parameters. Idx is then a slot in that declaration's
// dictionary rather than an index into SectionType.
type TypeUse struct {
	Derived bool
	Idx     pkgbits.Index
	Type    types2.Type
}

// An ObjUse is a reference to a package-scope declaration from inside a body,
// with the type arguments it is instantiated with.
type ObjUse struct {
	Idx   pkgbits.Index
	Pkg   *types2.Package // nil for the universe
	Name  string
	Obj   types2.Object // nil for a declaration the scope has no object for
	Targs []TypeUse
}

// A Selector names a field or a method.
type Selector struct {
	Pkg  *types2.Package
	Name string
}

// An RType is the runtime type descriptor a body needs at run time.
type RType struct {
	Derived bool
	DictIdx int     // the dictionary slot, when Derived
	Type    TypeUse // the type, when not Derived
}

// A ConvRTTI is the pair of descriptors a conversion from one type to another
// needs at run time.
type ConvRTTI struct {
	Src     RType
	Dst     RType
	Derived bool
	DictIdx int // the itab dictionary slot, when Derived
}

// An ExprType is a type written where an expression is expected, which is a
// type assertion's type, a type switch case, and make's and new's first
// argument.
type ExprType struct {
	Pos Pos

	// Itab is set when the type is being matched against a non-empty
	// interface, and RType is set otherwise.
	Itab    *ConvRTTI
	RType   *RType
	Derived bool
}

// A MultiExpr is a list of values, which is either one expression per value
// or one multi-valued expression spread over all of them.
type MultiExpr struct {
	// Single is set for the one-expression form. Values are then the
	// results, in order, each with the type it is converted to.
	Single  bool
	Pos     Pos
	Expr    Expr
	Results []MultiResult

	// Exprs is the one-expression-per-value form.
	Exprs []Expr
}

// A MultiResult is one result of a multi-valued expression.
type MultiResult struct {
	Src       TypeUse
	Converted bool
	Dst       TypeUse
	Conv      ConvRTTI
}

// An Assignee is one destination of an assignment.
type Assignee struct {
	Kind AssignKind

	// Kind == AssignDef: the variable the assignment declares.
	Pos   Pos
	Name  string
	Pkg   *types2.Package
	Type  TypeUse
	Local Local

	// Kind == AssignExpr.
	Expr Expr
}

// A MethodRef is a reference to the method a selection names.
type MethodRef struct {
	Recv TypeUse

	// Generic is true for a method that declares type parameters of its
	// own, and Sig is the method's signature otherwise.
	Generic bool
	Sig     TypeUse

	Pos Pos
	Sel Selector

	// TypeParam is set when the receiver is a type parameter, so that the
	// call goes through the dictionary at DictIdx.
	TypeParam bool
	DictIdx   int

	// Subdict is set when the type arguments are not all known statically,
	// and StaticDict is set when they are and the method needs one.
	Subdict    bool
	SubdictIdx int
	StaticDict bool
	Dict       ObjUse
}

// A FuncInst is a reference to an instantiated generic function.
type FuncInst struct {
	// Derived is true when a type argument depends on the enclosing
	// declaration's type parameters, and DictIdx is then the subdictionary
	// slot the call takes its dictionary from.
	Derived bool
	DictIdx int
	Obj     ObjUse
}

// A Stmt is one statement of a body.
type Stmt interface{ stmtKind() StmtKind }

// A LabelStmt is a label. The statement it labels is the next statement of
// the same list, because gc writes the two flat.
type LabelStmt struct {
	Pos   Pos
	Label string
}

// A BlockStmt is a block, with the positions of the braces that open and
// close its scope.
type BlockStmt struct {
	Open  Pos
	Body  []Stmt
	Close Pos
}

// An ExprStmt is an expression evaluated for its effect.
type ExprStmt struct{ X Expr }

// A SendStmt is a channel send.
type SendStmt struct {
	Pos   Pos
	Chan  Expr
	Value Expr
}

// An AssignStmt is an assignment, a short variable declaration, or a var
// declaration inside a function.
type AssignStmt struct {
	Pos Pos
	Lhs []Assignee
	Rhs MultiExpr
}

// An AssignOpStmt is an assignment that applies an operator, such as x += y.
type AssignOpStmt struct {
	Op  Op
	Lhs Expr
	Pos Pos
	Rhs Expr
}

// An IncDecStmt is x++ or x--.
type IncDecStmt struct {
	Op  Op
	X   Expr
	Pos Pos
}

// A BranchStmt is break, continue, goto or fallthrough.
type BranchStmt struct {
	Pos      Pos
	Op       Op
	Labelled bool
	Label    string
}

// A CallStmt is go or defer.
type CallStmt struct {
	Pos  Pos
	Op   Op
	Call Expr

	// DeferAt is the defer record a defer statement runs in, and is nil for
	// a defer that uses the frame's own record and for go.
	DeferAt Expr
}

// A ReturnStmt is a return.
type ReturnStmt struct {
	Pos     Pos
	Results MultiExpr
}

// An IfStmt is an if statement.
//
// Static is the constant value of the condition, if it has one: 1 for a
// condition that is always true, -1 for one that is always false, and 0 for
// one that is not constant. gc omits the branch it proved unreachable, so
// Then is nil when Static is negative and Else is nil when Static is
// positive.
type IfStmt struct {
	Open      Pos
	Pos       Pos
	Init      []Stmt
	Cond      Expr
	Static    int
	Then      *BlockStmt
	ThenClose Pos
	Else      []Stmt
}

// A ForStmt is a for statement. Range is set for a range clause and the
// three-clause fields are set otherwise.
type ForStmt struct {
	Open  Pos
	Range *RangeClause

	Pos  Pos
	Init []Stmt
	Cond Expr
	Post []Stmt

	Body *BlockStmt

	// DistinctVars says the loop declares its variables anew on every
	// iteration, which is the Go 1.22 rule.
	DistinctVars bool
}

// A RangeClause is the range form of a for statement.
type RangeClause struct {
	Pos Pos
	Lhs []Assignee
	X   Expr

	// MapRType is the descriptor of the map being ranged over, and is nil
	// when the operand is not a map.
	MapRType *RType

	// KeyConv and ValueConv convert the key and the value to the type of
	// their destination. Each is nil when the clause has no such
	// destination or when the destination is blank.
	KeyConv   *ConvRTTI
	ValueConv *ConvRTTI
}

// A SwitchStmt is an expression switch or a type switch.
type SwitchStmt struct {
	Open Pos
	Pos  Pos
	Init []Stmt

	// Guard is set for a type switch and Tag for an expression switch. Tag
	// is nil for a switch with no tag, which is a switch on true.
	Guard *TypeSwitchGuard
	Tag   Expr

	Clauses []CaseClause

	// ClausesClose is the closing brace as it ends the last clause's scope,
	// and Close is the same brace as it ends the switch's own scope. gc
	// writes it twice and the second is written even for a switch with no
	// clause.
	ClausesClose Pos
	Close        Pos
}

// A TypeSwitchGuard is the x := y.(type) of a type switch.
type TypeSwitchGuard struct {
	Pos Pos

	// Named is true when the guard declares a variable, which each clause
	// then declares again with the clause's own type.
	Named   bool
	NamePos Pos
	Pkg     *types2.Package
	Name    string

	X Expr
}

// A CaseClause is one clause of a switch.
type CaseClause struct {
	// ScopeClose is the position that closes the previous clause's scope
	// and is only set for a clause after the first.
	ScopeClose *Pos
	ScopeOpen  Pos
	Pos        Pos

	// Types is set for a type switch, one entry per case. A nil Type is the
	// case nil.
	Types []*ExprType

	// Exprs is set for an expression switch.
	Exprs []Expr

	// Var is the variable a named type switch guard declares for this
	// clause.
	Var     *Local
	VarPos  Pos
	VarType TypeUse

	Body []Stmt
}

// A SelectStmt is a select statement.
type SelectStmt struct {
	Pos     Pos
	Clauses []CommClause
	Close   Pos
}

// A CommClause is one clause of a select.
type CommClause struct {
	ScopeClose *Pos
	ScopeOpen  Pos
	Pos        Pos
	Comm       []Stmt
	Body       []Stmt
}

func (*LabelStmt) stmtKind() StmtKind    { return StmtLabel }
func (*BlockStmt) stmtKind() StmtKind    { return StmtBlock }
func (*ExprStmt) stmtKind() StmtKind     { return StmtExpr }
func (*SendStmt) stmtKind() StmtKind     { return StmtSend }
func (*AssignStmt) stmtKind() StmtKind   { return StmtAssign }
func (*AssignOpStmt) stmtKind() StmtKind { return StmtAssignOp }
func (*IncDecStmt) stmtKind() StmtKind   { return StmtIncDec }
func (*BranchStmt) stmtKind() StmtKind   { return StmtBranch }
func (*CallStmt) stmtKind() StmtKind     { return StmtCall }
func (*ReturnStmt) stmtKind() StmtKind   { return StmtReturn }
func (*IfStmt) stmtKind() StmtKind       { return StmtIf }
func (*ForStmt) stmtKind() StmtKind      { return StmtFor }
func (*SwitchStmt) stmtKind() StmtKind   { return StmtSwitch }
func (*SelectStmt) stmtKind() StmtKind   { return StmtSelect }

// An Expr is one expression of a body.
//
// Every expression that has a type in the checker's record is preceded in the
// stream by its type, because gc writes a reshape node in front of it. The
// decoder attaches that type to the expression it wraps, so [Expr.ExprType]
// answers for nearly every node without the tree being type checked again.
type Expr interface {
	exprKind() ExprKind

	// Reshape is the reshape node the format wrote in front of the
	// expression, and is nil where it wrote none.
	Reshape() *TypeUse

	// ExprType is the type of the value the expression produces, and is nil
	// where the stream did not carry one. It is nil for an expression of a
	// tuple type, for a reference to a builtin, and for a use of a local.
	ExprType() types2.Type
}

// exprType is embedded by every expression node to carry the type the
// enclosing reshape node named.
//
// reshape is the reshape node itself, kept because the encoder writes it back
// and typ alone cannot say whether there was one: a constant, a zero value
// and a conversion each carry a type of their own, which the reader falls
// back to when no reshape node preceded them.
type exprType struct {
	reshape *TypeUse
	typ     types2.Type
}

func (e *exprType) ExprType() types2.Type { return e.typ }

// Reshape returns the type the reshape node in front of the expression named,
// and nil where the stream carried no such node.
func (e *exprType) Reshape() *TypeUse { return e.reshape }

// A ConstExpr is a constant. gc folds a constant expression before it writes
// it, so the source's operators are not here.
type ConstExpr struct {
	exprType
	Pos   Pos
	Type  TypeUse
	Value constant.Value
}

// A LocalExpr is a use of a local variable or of a variable captured by a
// function literal.
type LocalExpr struct {
	exprType
	// Captured is true when Index names a captured variable rather than a
	// local of this body.
	Captured bool
	Index    int
}

// A GlobalExpr is a use of a package-scope declaration, of a builtin, or of
// an unsafe intrinsic.
type GlobalExpr struct {
	exprType
	Obj ObjUse
}

// A CompLitExpr is a composite literal.
type CompLitExpr struct {
	exprType
	Pos  Pos
	Type TypeUse

	// MapRType is the descriptor of the map being built, and is nil for
	// every other kind of literal.
	MapRType *RType

	// Keyed is false when no element has a key, which is the compact form
	// the format's version V3 added.
	Keyed bool
	Elems []LitElem
}

// A LitElem is one element of a composite literal.
type LitElem struct {
	// Pos is the position of the key, or of the element for a struct
	// literal with no key.
	Pos Pos

	// Key is set for an element of an array, slice or map literal written
	// with a key.
	Key Expr

	// Field is the field index of an element of a struct literal, and
	// Embedded is the path to it through embedded fields when the key names
	// a promoted field.
	Field    int
	Embedded []int

	Value Expr
}

// A FuncLitExpr is a function literal.
type FuncLitExpr struct {
	exprType
	Pos Pos

	// Params and Results are the literal's signature. A literal has no
	// receiver, so the two together are as long as its body's Params.
	Params   []Param
	Results  []Param
	Variadic bool

	// RangeFuncBody is true for the closure gc builds out of the body of a
	// range over a function.
	RangeFuncBody bool

	// Captured is one entry per variable the literal captures, naming it in
	// the enclosing body's numbering.
	Captured []CapturedVar

	// Body is the index of the SectionBody element holding the literal's
	// body, and Decoded is that body once it has been read.
	Body    pkgbits.Index
	Decoded *Body
}

// A Param is one parameter or result of a signature written inside a body.
type Param struct {
	Pos  Pos
	Pkg  *types2.Package
	Name string
	Type TypeUse
}

// A CapturedVar is one variable a function literal captures.
type CapturedVar struct {
	Pos   Pos
	Local LocalExpr
}

// A FieldValExpr is x.f, where f is a field.
type FieldValExpr struct {
	exprType
	X   Expr
	Pos Pos
	Sel Selector
}

// A MethodValExpr is x.m, where m is a method and the result is a bound
// method value.
type MethodValExpr struct {
	exprType
	Recv   Expr
	Pos    Pos
	Method MethodRef
}

// A MethodExprExpr is T.m, where the result is a function whose first
// parameter is the receiver.
type MethodExprExpr struct {
	exprType
	Recv TypeUse

	// Implicits is the path of embedded fields the selection goes through.
	Implicits []int

	// Deref and Addr say whether the receiver needs one before the method
	// is applied.
	Deref bool
	Addr  bool

	Pos    Pos
	Method MethodRef
}

// A RecvExpr is the operand of a method selection, with the implicit field
// selections, dereference or address the selection applies.
type RecvExpr struct {
	exprType
	X         Expr
	Pos       Pos
	Implicits []int
	Deref     bool
	Addr      bool
}

// An IndexExpr is x[i].
type IndexExpr struct {
	exprType
	X     Expr
	Pos   Pos
	Index Expr

	// MapRType is the descriptor of the map being indexed, and is nil when
	// x is not a map.
	MapRType *RType
}

// A SliceExpr is x[a:b] or x[a:b:c]. An absent bound is nil.
type SliceExpr struct {
	exprType
	X     Expr
	Pos   Pos
	Index [3]Expr
}

// An AssertExpr is x.(T).
type AssertExpr struct {
	exprType
	X    Expr
	Pos  Pos
	Type ExprType
	Src  RType
}

// A UnaryExpr is a unary operation, an address, a dereference or a receive.
type UnaryExpr struct {
	exprType
	Op  Op
	Pos Pos
	X   Expr
}

// A BinaryExpr is a binary operation.
type BinaryExpr struct {
	exprType
	Op  Op
	X   Expr
	Pos Pos
	Y   Expr
}

// A CallExpr is a call.
type CallExpr struct {
	exprType

	// Method is set for a call through a method selection.
	Method *MethodCall

	// Inst is set for a call of an instantiated generic function.
	Inst    *FuncInst
	InstPos Pos

	// Fun is the callee for every other call.
	Fun Expr

	Pos  Pos
	Args MultiExpr
	Dots bool

	// RType is the descriptor append, copy, delete and unsafe.Slice need,
	// and is nil for every other callee.
	RType *RType
}

// A MethodCall is the callee of a call through a method selection.
type MethodCall struct {
	Recv   Expr
	Method MethodRef
}

// A ConvertExpr is a conversion.
type ConvertExpr struct {
	exprType

	// Implicit is true for a conversion the source did not spell, which the
	// assignability rules of the specification require.
	Implicit bool

	Type TypeUse
	Pos  Pos
	Conv ConvRTTI

	// TypeParam is true when the destination is a type parameter, and
	// Identical is true when the two types are identical, which happens for
	// an explicit conversion that changes nothing.
	TypeParam bool
	Identical bool

	X Expr
}

// A NewExpr is new(T) or new(x).
type NewExpr struct {
	exprType
	Pos Pos

	// Value is set for new(x), which Go 1.26 added, and Type is set for
	// new(T).
	Value Expr
	Type  *ExprType
}

// A MakeExpr is make(T, ...).
type MakeExpr struct {
	exprType
	Pos   Pos
	Type  ExprType
	Args  []Expr
	RType RType
}

// A SizeExpr is unsafe.Sizeof or unsafe.Alignof.
type SizeExpr struct {
	exprType
	Kind ExprKind // ExprSizeof or ExprAlignof
	Pos  Pos
	Type TypeUse
}

// An OffsetofExpr is unsafe.Offsetof.
type OffsetofExpr struct {
	exprType
	Pos  Pos
	Type TypeUse

	// Path is the field index at each step of the selection, so a selection
	// through an embedded field has more than one entry.
	Path []int
}

// A ZeroExpr is the predeclared nil, typed by its context.
type ZeroExpr struct {
	exprType
	Pos  Pos
	Type TypeUse
}

// A FuncInstExpr is a reference to an instantiated generic function that is
// not immediately called.
type FuncInstExpr struct {
	exprType
	Pos  Pos
	Inst FuncInst
}

// A RuntimeBuiltinExpr is a reference to a runtime function that gc's own
// transformations introduced, such as panicrangeexit. It has no source
// spelling.
type RuntimeBuiltinExpr struct {
	exprType
	Name string
}

func (*ConstExpr) exprKind() ExprKind          { return ExprConst }
func (*LocalExpr) exprKind() ExprKind          { return ExprLocal }
func (*GlobalExpr) exprKind() ExprKind         { return ExprGlobal }
func (*CompLitExpr) exprKind() ExprKind        { return ExprCompLit }
func (*FuncLitExpr) exprKind() ExprKind        { return ExprFuncLit }
func (*FieldValExpr) exprKind() ExprKind       { return ExprFieldVal }
func (*MethodValExpr) exprKind() ExprKind      { return ExprMethodVal }
func (*MethodExprExpr) exprKind() ExprKind     { return ExprMethodExpr }
func (*IndexExpr) exprKind() ExprKind          { return ExprIndex }
func (*SliceExpr) exprKind() ExprKind          { return ExprSlice }
func (*AssertExpr) exprKind() ExprKind         { return ExprAssert }
func (*UnaryExpr) exprKind() ExprKind          { return ExprUnaryOp }
func (*BinaryExpr) exprKind() ExprKind         { return ExprBinaryOp }
func (*CallExpr) exprKind() ExprKind           { return ExprCall }
func (*ConvertExpr) exprKind() ExprKind        { return ExprConvert }
func (*NewExpr) exprKind() ExprKind            { return ExprNew }
func (*MakeExpr) exprKind() ExprKind           { return ExprMake }
func (e *SizeExpr) exprKind() ExprKind         { return e.Kind }
func (*OffsetofExpr) exprKind() ExprKind       { return ExprOffsetof }
func (*ZeroExpr) exprKind() ExprKind           { return ExprZero }
func (*FuncInstExpr) exprKind() ExprKind       { return ExprFuncInst }
func (*RecvExpr) exprKind() ExprKind           { return ExprRecv }
func (*RuntimeBuiltinExpr) exprKind() ExprKind { return ExprRuntimeBuiltin }
