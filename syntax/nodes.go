// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

// The syntax tree.
//
// The node set follows cmd/compile/internal/syntax, and that is a constraint
// rather than a coincidence. specs/012-type-checking.md ports the type checker
// from cmd/compile/internal/types2, and 38 of that package's 69 files name the
// syntax tree directly. The port is mechanical only while the node set it names
// has the same shape, so a divergence here is paid for there and must be
// justified in the spec.
//
// ParenExpr is the worked example. A tree built only for lowering does not need
// it. It is kept because the checker uses it in a dozen places, because the
// rule that a composite literal may not appear in a control clause header needs
// to know a parenthesis was there, and because dropping one node to pay for a
// dozen edits in a ported 23,000-line checker is a bad trade.
//
// See specs/011-parser-and-ast.md.

// Node is any syntax tree node.
//
// The interface is closed by an unexported method, so a type switch over the
// node set is exhaustive by construction.
type Node interface {
	Pos() Pos
	SetPos(Pos)
	aNode()
}

type node struct {
	pos Pos
}

func (n *node) Pos() Pos     { return n.pos }
func (n *node) SetPos(p Pos) { n.pos = p }
func (*node) aNode()         {}

// File is one parsed source file.
type File struct {
	Pragma   Pragma
	PkgName  *Name
	DeclList []Decl
	EOF      Pos
	node
}

// Pragma carries the //go: directives attached to a declaration.
//
// The scanner routes the comments and the parser attaches them here. The type
// checker copies them onto the object it creates, because that is the only
// point at which the association between a directive and an object exists.
// See specs/016-directives-and-pragmas.md.
type Pragma interface{}

// Declarations.

type (
	Decl interface {
		Node
		aDecl()
	}

	// ImportDecl is one import spec. Group is the parenthesised group it
	// belongs to, or nil.
	ImportDecl struct {
		Group        *Group
		Pragma       Pragma
		LocalPkgName *Name // including "." and "_"; nil means the package's own name
		Path         *BasicLit
		decl
	}

	ConstDecl struct {
		Group    *Group
		Pragma   Pragma
		NameList []*Name
		Type     Expr // nil means no type was written
		Values   Expr // nil means the previous spec's values are repeated
		decl
	}

	TypeDecl struct {
		Group      *Group
		Pragma     Pragma
		Name       *Name
		TParamList []*Field // nil means not generic
		Alias      bool
		Type       Expr
		decl
	}

	VarDecl struct {
		Group    *Group
		Pragma   Pragma
		NameList []*Name
		Type     Expr // nil means the type is inferred from Values
		Values   Expr // nil means the zero value
		decl
	}

	FuncDecl struct {
		Pragma     Pragma
		Recv       *Field // nil means a function, not a method
		Name       *Name
		TParamList []*Field // nil means not generic; always nil for a method
		Type       *FuncType
		Body       *BlockStmt // nil means the body is elsewhere, in assembly
		decl
	}
)

type decl struct{ node }

func (*decl) aDecl() {}

// Group is the shared parenthesised group of a run of declarations. Two specs
// in one group share a Group pointer, which is how a const group's implicit
// repetition of the previous values is recognised.
type Group struct{ _ int }

// Expressions.

type (
	Expr interface {
		Node
		aExpr()
	}

	// BadExpr stands in for an expression the parser could not read.
	//
	// Every production returns a Bad node rather than nil on failure, so that
	// one syntax error produces one message rather than one message plus the
	// type errors that follow from a hole in the tree.
	BadExpr struct {
		expr
	}

	Name struct {
		Value string
		expr
	}

	BasicLit struct {
		Value string
		Kind  LitKind
		Bad   bool // the literal is malformed; an error was already reported
		expr
	}

	CompositeLit struct {
		Type     Expr // nil means the type is elided, as in a nested literal
		ElemList []Expr
		NKeys    int // number of elements that are KeyValueExpr
		Rbrace   Pos
		expr
	}

	KeyValueExpr struct {
		Key, Value Expr
		expr
	}

	FuncLit struct {
		Type *FuncType
		Body *BlockStmt
		expr
	}

	ParenExpr struct {
		X Expr
		expr
	}

	SelectorExpr struct {
		X   Expr
		Sel *Name
		expr
	}

	// IndexExpr is both an index and a generic instantiation.
	//
	// The parser does not resolve which. It cannot: f[x] is an index if f is a
	// value and an instantiation if f is generic, and that is a typing
	// question. Index holds a ListExpr when there is more than one operand,
	// which can only be an instantiation.
	IndexExpr struct {
		X     Expr
		Index Expr
		expr
	}

	SliceExpr struct {
		X     Expr
		Index [3]Expr // any may be nil
		Full  bool    // a three-index slice was written
		expr
	}

	AssertExpr struct {
		X    Expr
		Type Expr
		expr
	}

	// TypeSwitchGuard is the x := y.(type) of a type switch header.
	TypeSwitchGuard struct {
		Lhs *Name // nil means no short variable declaration
		X   Expr  // the operand of .(type)
		expr
	}

	// Operation is every unary and binary operator.
	//
	// Y is nil for a unary operator. One node rather than two removes a
	// duplicated half of every expression walker in the compiler.
	Operation struct {
		Op   Operator
		X, Y Expr
		expr
	}

	CallExpr struct {
		Fun     Expr
		ArgList []Expr
		HasDots bool // the last argument was followed by ...
		expr
	}

	// ListExpr is a comma-separated list where one expression was expected.
	ListExpr struct {
		ElemList []Expr
		expr
	}

	ArrayType struct {
		Len  Expr // nil means [...]T
		Elem Expr
		expr
	}

	SliceType struct {
		Elem Expr
		expr
	}

	DotsType struct {
		Elem Expr
		expr
	}

	StructType struct {
		FieldList []*Field
		TagList   []*BasicLit // len(TagList) == len(FieldList) or TagList is nil
		expr
	}

	InterfaceType struct {
		MethodList []*Field
		expr
	}

	FuncType struct {
		ParamList  []*Field
		ResultList []*Field
		expr
	}

	MapType struct {
		Key, Value Expr
		expr
	}

	ChanType struct {
		Dir  ChanDir // 0 means bidirectional
		Elem Expr
		expr
	}
)

type expr struct{ node }

func (*expr) aExpr() {}

// ChanDir is a channel direction.
type ChanDir uint

const (
	SendOnly ChanDir = iota + 1
	RecvOnly
)

// Field is one field of a struct, one parameter or result of a signature, one
// method or embedded element of an interface, or one type parameter.
//
// Name is nil for an embedded field, an unnamed parameter, or an embedded
// interface element.
type Field struct {
	Name *Name
	Type Expr
	node
}

// Statements.

type (
	Stmt interface {
		Node
		aStmt()
	}

	// SimpleStmt is a statement that may appear in a control clause header.
	SimpleStmt interface {
		Stmt
		aSimpleStmt()
	}

	// BadStmt stands in for a statement the parser could not read.
	BadStmt struct {
		stmt
	}

	EmptyStmt struct {
		simpleStmt
	}

	LabeledStmt struct {
		Label *Name
		Stmt  Stmt
		stmt
	}

	BlockStmt struct {
		List   []Stmt
		Rbrace Pos
		stmt
	}

	ExprStmt struct {
		X Expr
		simpleStmt
	}

	SendStmt struct {
		Chan, Value Expr
		simpleStmt
	}

	DeclStmt struct {
		DeclList []Decl
		stmt
	}

	// AssignStmt is assignment, short variable declaration, an operation
	// assignment, and increment and decrement.
	//
	// Op is 0 for a plain assignment, Def for :=, and the binary operator for
	// an operation assignment. Rhs is ImplicitOne for ++ and --.
	AssignStmt struct {
		Op       Operator
		Lhs, Rhs Expr
		simpleStmt
	}

	BranchStmt struct {
		Tok    Token // Break, Continue, Fallthrough or Goto
		Label  *Name
		Target Stmt // set by the type checker
		stmt
	}

	CallStmt struct {
		Tok     Token // Go or Defer
		Call    Expr
		DeferAt Expr // set by the compiler for an open-coded defer
		stmt
	}

	ReturnStmt struct {
		Results Expr // nil means a bare return
		stmt
	}

	IfStmt struct {
		Init SimpleStmt
		Cond Expr
		Then *BlockStmt
		Else Stmt // either an *IfStmt or a *BlockStmt
		stmt
	}

	ForStmt struct {
		Init SimpleStmt // including *RangeClause
		Cond Expr
		Post SimpleStmt
		Body *BlockStmt
		stmt
	}

	SwitchStmt struct {
		Init   SimpleStmt
		Tag    Expr // including *TypeSwitchGuard
		Body   []*CaseClause
		Rbrace Pos
		stmt
	}

	SelectStmt struct {
		Body   []*CommClause
		Rbrace Pos
		stmt
	}
)

type (
	RangeClause struct {
		Lhs Expr // nil means no assignment
		Def bool // the assignment was :=
		X   Expr // the operand of range
		simpleStmt
	}

	CaseClause struct {
		Cases Expr // nil means default
		Body  []Stmt
		Colon Pos
		node
	}

	CommClause struct {
		Comm  SimpleStmt // nil means default
		Body  []Stmt
		Colon Pos
		node
	}
)

type stmt struct{ node }

func (*stmt) aStmt() {}

type simpleStmt struct {
	stmt
}

func (*simpleStmt) aSimpleStmt() {}

// ImplicitOne is the Rhs of an AssignStmt produced by ++ or --.
//
// It is a single shared node rather than a fresh literal per statement,
// matching the reference implementation, so that a walker can recognise the
// increment form by identity.
var ImplicitOne = &BasicLit{Value: "1", Kind: IntLit}
