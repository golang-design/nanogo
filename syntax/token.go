// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

// Token is a lexical token.
//
// The set is smaller than go/token's because every binary and unary operator
// shares the _Operator token and carries its identity in an Operator value. The
// parser needs an operator's precedence far more often than it needs to switch
// on which operator it is, and specs/011-parser-and-ast.md folds unary and
// binary operators into one Operation node for the same reason.
//
// The constants are unexported and a small set of exported aliases follows
// them. This is the reference implementation's arrangement and it is copied
// deliberately: the token names collide with the node names, Name against
// *Name and Var against a variable declaration, and the collision has to be
// resolved somewhere. Resolving it the same way the reference does keeps the
// type checker port of specs/012-type-checking.md mechanical.
//
// See specs/010-scanner-and-positions.md.
type Token uint8

const (
	_ Token = iota

	_EOF

	// Names and literals.
	_Name    // identifier
	_Literal // an integer, float, imaginary, rune or string literal

	// Operators and delimiters. _Operator carries its identity in Scanner.Op.
	_Operator // + - * / % & | ^ &^ << >> && || ! < <= > >= == != ~
	_AssignOp // += -= *= /= %= &= |= ^= &^= <<= >>=
	_IncOp    // ++ --
	_Assign   // =
	_Define   // :=
	_Arrow    // <-, both as a channel direction and as a receive; Op is Recv
	_Star     // * as a pointer type or a dereference

	_Lparen
	_Lbrack
	_Lbrace
	_Rparen
	_Rbrack
	_Rbrace

	_Comma
	_Semi // ; including one the scanner inserted at a newline
	_Colon
	_Dot
	_DotDotDot

	// Keywords.
	_Break
	_Case
	_Chan
	_Const
	_Continue
	_Default
	_Defer
	_Else
	_Fallthrough
	_For
	_Func
	_Go
	_Goto
	_If
	_Import
	_Interface
	_Map
	_Package
	_Range
	_Return
	_Select
	_Struct
	_Switch
	_Type
	_Var

	tokenCount
)

// Exported aliases, for the tokens that appear in the tree and that the type
// checker therefore compares against. Nothing else is exported, because
// nothing else crosses the package boundary.
const (
	Break       = _Break
	Continue    = _Continue
	Fallthrough = _Fallthrough
	Goto        = _Goto
	Defer       = _Defer
	Go          = _Go
)

var tokenNames = [...]string{
	_EOF:         "EOF",
	_Name:        "name",
	_Literal:     "literal",
	_Operator:    "operator",
	_AssignOp:    "op=",
	_IncOp:       "opop",
	_Assign:      "=",
	_Define:      ":=",
	_Arrow:       "<-",
	_Star:        "*",
	_Lparen:      "(",
	_Lbrack:      "[",
	_Lbrace:      "{",
	_Rparen:      ")",
	_Rbrack:      "]",
	_Rbrace:      "}",
	_Comma:       ",",
	_Semi:        ";",
	_Colon:       ":",
	_Dot:         ".",
	_DotDotDot:   "...",
	_Break:       "break",
	_Case:        "case",
	_Chan:        "chan",
	_Const:       "const",
	_Continue:    "continue",
	_Default:     "default",
	_Defer:       "defer",
	_Else:        "else",
	_Fallthrough: "fallthrough",
	_For:         "for",
	_Func:        "func",
	_Go:          "go",
	_Goto:        "goto",
	_If:          "if",
	_Import:      "import",
	_Interface:   "interface",
	_Map:         "map",
	_Package:     "package",
	_Range:       "range",
	_Return:      "return",
	_Select:      "select",
	_Struct:      "struct",
	_Switch:      "switch",
	_Type:        "type",
	_Var:         "var",
}

func (t Token) String() string {
	if int(t) < len(tokenNames) && tokenNames[t] != "" {
		return tokenNames[t]
	}
	return "token(?)"
}

// IsKeyword reports whether t is a keyword token.
func (t Token) IsKeyword() bool { return t >= _Break && t <= _Var }

// Operator identifies which operator an _Operator, _AssignOp or _IncOp token
// is.
type Operator uint8

const (
	_ Operator = iota

	Def   // :=, recorded on an AssignStmt that declares
	Not   // !
	Recv  // <- as a receive
	Tilde // ~ in a type constraint element

	// Binary operators, in increasing precedence. The order matters:
	// Precedence indexes ranges of it.
	OrOr   // ||
	AndAnd // &&

	Eql // ==
	Neq // !=
	Lss // <
	Leq // <=
	Gtr // >
	Geq // >=

	Add // +
	Sub // -
	Or  // |
	Xor // ^

	Mul    // *
	Div    // /
	Rem    // %
	And    // &
	AndNot // &^
	Shl    // <<
	Shr    // >>

	operatorCount
)

var operatorNames = [...]string{
	Def:    ":=",
	Not:    "!",
	Recv:   "<-",
	Tilde:  "~",
	OrOr:   "||",
	AndAnd: "&&",
	Eql:    "==",
	Neq:    "!=",
	Lss:    "<",
	Leq:    "<=",
	Gtr:    ">",
	Geq:    ">=",
	Add:    "+",
	Sub:    "-",
	Or:     "|",
	Xor:    "^",
	Mul:    "*",
	Div:    "/",
	Rem:    "%",
	And:    "&",
	AndNot: "&^",
	Shl:    "<<",
	Shr:    ">>",
}

func (o Operator) String() string {
	if int(o) < len(operatorNames) && operatorNames[o] != "" {
		return operatorNames[o]
	}
	return "operator(?)"
}

// Precedence levels for binary operators. The specification defines five.
const (
	PrecLowest = iota // not a binary operator
	PrecOrOr
	PrecAndAnd
	PrecCmp
	PrecAdd
	PrecMul
)

// Precedence returns the binary precedence of o, or PrecLowest if o is not a
// binary operator.
//
// Precedence climbing over this one table replaces the five nested parsing
// methods that one method per level would need, which removes four calls from
// every expression parsed.
func (o Operator) Precedence() int {
	switch {
	case o == OrOr:
		return PrecOrOr
	case o == AndAnd:
		return PrecAndAnd
	case o >= Eql && o <= Geq:
		return PrecCmp
	case o >= Add && o <= Xor:
		return PrecAdd
	case o >= Mul && o <= Shr:
		return PrecMul
	}
	return PrecLowest
}

// LitKind is the kind of a _Literal token.
type LitKind uint8

const (
	IntLit LitKind = iota
	FloatLit
	ImagLit
	RuneLit
	StringLit
)

var litKindNames = [...]string{
	IntLit:    "untyped int",
	FloatLit:  "untyped float",
	ImagLit:   "untyped imaginary",
	RuneLit:   "untyped rune",
	StringLit: "untyped string",
}

func (k LitKind) String() string {
	if int(k) < len(litKindNames) {
		return litKindNames[k]
	}
	return "untyped ?"
}

// keywords maps a keyword's spelling to its token. It is built once and only
// read afterwards, so it never reaches an output path and specs/053's rule
// against ranging over a map does not apply.
var keywords = map[string]Token{
	"break":       _Break,
	"case":        _Case,
	"chan":        _Chan,
	"const":       _Const,
	"continue":    _Continue,
	"default":     _Default,
	"defer":       _Defer,
	"else":        _Else,
	"fallthrough": _Fallthrough,
	"for":         _For,
	"func":        _Func,
	"go":          _Go,
	"goto":        _Goto,
	"if":          _If,
	"import":      _Import,
	"interface":   _Interface,
	"map":         _Map,
	"package":     _Package,
	"range":       _Range,
	"return":      _Return,
	"select":      _Select,
	"struct":      _Struct,
	"switch":      _Switch,
	"type":        _Type,
	"var":         _Var,
}

// lookup returns the keyword token for name, or _Name if it is not a keyword.
func lookup(name string) Token {
	if t, ok := keywords[name]; ok {
		return t
	}
	return _Name
}
