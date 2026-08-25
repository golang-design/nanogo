// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"fmt"
	"strings"
)

// The parser.
//
// Recursive descent, one method per production of the Go specification's
// grammar, named for the production. The parser holds one token of lookahead,
// which is the scanner's current token.
//
// Expression parsing is precedence climbing over Operator.Precedence rather
// than one method per precedence level. The specification defines five binary
// levels, so a method per level costs four calls on every expression parsed and
// buys nothing that the one loop in binaryExpr does not.
//
// Two rules keep error recovery from producing cascades. Neither is a matter
// of taste: drop either one and one mistake in the source produces a list of
// messages that describe the recovery rather than the mistake.
//
//  1. At most one error per position. A second error at a position already
//     reported is a consequence of the first, so errorAt drops it.
//  2. No production returns nil. A production that fails returns a BadExpr,
//     BadStmt or BadDecl covering the range it consumed, so that one syntax
//     error produces one message rather than one message plus the type errors
//     that follow from a hole in the tree.
//
// See specs/011-parser-and-ast.md.

type parser struct {
	Scanner // the token source; the parser owns its current token

	file  *SrcFile
	errh  ErrorHandler
	pragh PragmaHandler
	mode  Mode

	first    error        // the first error, returned by Parse
	errcnt   int          // errors reported, after deduplication
	reported map[Pos]bool // positions already reported, for rule 1 above
	pragma   Pragma       // the //go: directives waiting for a declaration
	fnest    int          // function nesting depth; a statement keyword is a
	// synchronisation point only inside a function
	xnest int // expression nesting depth; negative inside a
	// control clause header, where the specification
	// forbids a bare composite literal
}

func (p *parser) init(file *SrcFile, src []byte, errh ErrorHandler, pragh PragmaHandler, mode Mode) {
	p.file = file
	p.errh = errh
	p.pragh = pragh
	p.mode = mode

	p.first = nil
	p.errcnt = 0
	p.reported = make(map[Pos]bool)
	p.pragma = nil
	p.fnest = 0
	p.xnest = 0

	// Every field above must be set before Init runs. Init makes the first
	// token current, so it already scans the file's leading comments and calls
	// the two handlers below for them.
	var prag PragmaHandler
	if pragh != nil {
		// The parser owns the running pragma, not the scanner: a directive
		// accumulates onto the ones before it and only the parser knows where
		// the run ends, which is the next declaration.
		prag = func(pos Pos, blank bool, text string, _ Pragma) Pragma {
			p.pragma = p.pragh(pos, blank, text, p.pragma)
			return p.pragma
		}
	}
	// Scanner errors run through the parser's own reporting, so that a
	// malformed literal counts towards the error count and takes part in the
	// one-error-per-position rule like any other error.
	p.Scanner.Init(file, src, func(err Error) { p.errorAt(err.Pos, err.Msg) }, prag, mode)
}

// next advances to the next token, which is the parser's one token of
// lookahead.
func (p *parser) next() { p.Scanner.Next() }

// pos returns the position of the current token.
func (p *parser) pos() Pos { return p.Scanner.Pos }

func (p *parser) got(tok Token) bool {
	if p.Tok == tok {
		p.next()
		return true
	}
	return false
}

func (p *parser) want(tok Token) {
	if !p.got(tok) {
		p.syntaxError("expected " + tokstring(tok))
		p.advance()
	}
}

// gotAssign is got(_Assign) that also accepts ":=" and complains, because
// writing ":=" for "=" is a common mistake and recovering from it here gives a
// better tree than resynchronising would.
func (p *parser) gotAssign() bool {
	switch p.Tok {
	case _Define:
		p.syntaxError("expected =")
		fallthrough
	case _Assign:
		p.next()
		return true
	}
	return false
}

// takePragma returns the pragmas seen so far and clears them.
func (p *parser) takePragma() Pragma {
	prag := p.pragma
	p.pragma = nil
	return prag
}

// clearPragma returns the pending pragmas to the handler unused.
//
// It is called at the end of every form that does not accept a pragma. The
// handler decides whether a misplaced directive is worth an error;
// specs/016-directives-and-pragmas.md owns that decision.
func (p *parser) clearPragma() {
	if p.pragma != nil {
		p.pragh(p.pos(), p.Scanner.blank, "", p.pragma)
		p.pragma = nil
	}
}

// ----------------------------------------------------------------------------
// Error handling

func (p *parser) errorAt(pos Pos, msg string) {
	err := Error{Pos: pos, Msg: msg}
	if p.first == nil {
		p.first = err
	}
	if p.reported[pos] {
		return // rule 1: at most one error per position
	}
	p.reported[pos] = true
	p.errcnt++
	if p.errh != nil {
		p.errh(err)
	}
}

// syntaxErrorAt reports a syntax error at pos, naming the token that was found.
func (p *parser) syntaxErrorAt(pos Pos, msg string) {
	// An error at EOF after an earlier error is always a consequence of it:
	// the parser ran out of input while recovering.
	if p.Tok == _EOF && p.first != nil {
		return
	}

	switch {
	case msg == "":
		// nothing to add
	case strings.HasPrefix(msg, "in "), strings.HasPrefix(msg, "at "), strings.HasPrefix(msg, "after "):
		msg = " " + msg
	case strings.HasPrefix(msg, "expected "):
		msg = ", " + msg
	default:
		// The message does not talk about the current token, so do not name it.
		p.errorAt(pos, "syntax error: "+msg)
		return
	}

	var tok string
	switch p.Tok {
	case _Name:
		tok = "name " + p.Lit
	case _Semi:
		tok = p.Lit
	case _Literal:
		tok = "literal " + p.Lit
	case _Operator:
		tok = p.Op.String()
	case _AssignOp:
		tok = p.Op.String() + "="
	case _IncOp:
		tok = p.Op.String()
		tok += tok
	default:
		tok = tokstring(p.Tok)
	}

	p.errorAt(pos, "syntax error: unexpected "+tok+msg)
}

// tokstring names a token in an error message. Punctuation reads badly as
// itself, so the two tokens that appear most often in errors are spelled out.
func tokstring(tok Token) string {
	switch tok {
	case _Comma:
		return "comma"
	case _Semi:
		return "semicolon or newline"
	}
	s := tok.String()
	if tok.IsKeyword() {
		return "keyword " + s
	}
	return s
}

func (p *parser) error(msg string)       { p.errorAt(p.pos(), msg) }
func (p *parser) syntaxError(msg string) { p.syntaxErrorAt(p.pos(), msg) }

// stopset holds the keywords that begin a statement. They are the
// synchronisation points of specs/011: a statement boundary is where recovery
// can restart with a whole production ahead of it.
const stopset uint64 = 1<<_Break |
	1<<_Const |
	1<<_Continue |
	1<<_Defer |
	1<<_Fallthrough |
	1<<_For |
	1<<_Go |
	1<<_Goto |
	1<<_If |
	1<<_Return |
	1<<_Select |
	1<<_Switch |
	1<<_Type |
	1<<_Var

func contains(set uint64, tok Token) bool { return set&(1<<tok) != 0 }

// advance discards tokens until it reaches one that can follow the production
// that failed.
//
// The stopset is added only inside a function body, because outside one a
// statement keyword is not a boundary the parser can restart from. With an
// empty follow list advance consumes exactly one token, which is the minimum
// that guarantees progress.
func (p *parser) advance(followlist ...Token) {
	var followset uint64 = 1 << _EOF // never skip over EOF
	if len(followlist) > 0 {
		if p.fnest > 0 {
			followset |= stopset
		}
		for _, tok := range followlist {
			followset |= 1 << tok
		}
	}

	for !contains(followset, p.Tok) {
		p.next()
		if len(followlist) == 0 {
			break
		}
	}
}

// ----------------------------------------------------------------------------
// Source files

// SourceFile = PackageClause ";" { ImportDecl ";" } { TopLevelDecl ";" } .
func (p *parser) fileOrNil() *File {
	f := new(File)
	f.pos = p.pos()

	// PackageClause
	if !p.got(_Package) {
		p.syntaxError("package statement must be first")
		return nil
	}
	f.Pragma = p.takePragma()
	f.PkgName = p.name()
	p.want(_Semi)

	// A broken package clause makes every position after it doubtful, so stop
	// rather than report a file's worth of consequences.
	if p.first != nil {
		return nil
	}

	// Imports are accepted anywhere for tolerance, with a complaint, because a
	// file whose imports are misplaced still has declarations worth reading.
	// { ( ImportDecl | TopLevelDecl ) ";" }
	prev := _Import
	for p.Tok != _EOF {
		if p.Tok == _Import && prev != _Import {
			p.syntaxError("imports must appear before other declarations")
		}
		prev = p.Tok

		switch p.Tok {
		case _Import:
			p.next()
			f.DeclList = p.appendGroup(f.DeclList, p.importDecl)

		case _Const:
			p.next()
			f.DeclList = p.appendGroup(f.DeclList, p.constDecl)

		case _Type:
			p.next()
			f.DeclList = p.appendGroup(f.DeclList, p.typeDecl)

		case _Var:
			p.next()
			f.DeclList = p.appendGroup(f.DeclList, p.varDecl)

		case _Func:
			p.next()
			if d := p.funcDeclOrNil(); d != nil {
				f.DeclList = append(f.DeclList, d)
			}

		default:
			if p.Tok == _Lbrace && len(f.DeclList) > 0 && isEmptyFuncDecl(f.DeclList[len(f.DeclList)-1]) {
				// The "{" of a function body on the line after its signature.
				p.syntaxError("unexpected semicolon or newline before {")
			} else {
				p.syntaxError("non-declaration statement outside function body")
			}
			p.advance(_Import, _Const, _Type, _Var, _Func)
			continue
		}

		// Clear before consuming the ";", because a comment before the
		// semicolon belongs to the declaration that follows it.
		p.clearPragma()

		if p.Tok != _EOF && !p.got(_Semi) {
			p.syntaxError("after top level declaration")
			p.advance(_Import, _Const, _Type, _Var, _Func)
		}
	}

	p.clearPragma()
	f.EOF = p.pos()

	return f
}

func isEmptyFuncDecl(dcl Decl) bool {
	f, ok := dcl.(*FuncDecl)
	return ok && f.Body == nil
}

// ----------------------------------------------------------------------------
// Declarations

// list parses a possibly empty, sep-separated list closed by close, calling f
// for each element, and returns the position of the closing token.
//
// list = [ f { sep f } [sep] ] close .
func (p *parser) list(context string, sep, close Token, f func() bool) Pos {
	done := false
	for p.Tok != _EOF && p.Tok != close && !done {
		done = f()
		// The separator is optional before the closing token.
		if !p.got(sep) && p.Tok != close {
			p.syntaxError(fmt.Sprintf("in %s; possibly missing %s or %s", context, tokstring(sep), tokstring(close)))
			p.advance(_Rparen, _Rbrack, _Rbrace)
			if p.Tok != close {
				return p.pos()
			}
		}
	}

	pos := p.pos()
	p.want(close)
	return pos
}

// appendGroup(f) = f | "(" { f ";" } ")" . // ";" is optional before ")"
//
// Every declaration in one pair of parentheses shares one *Group. That pointer
// is how a const group's implicit repetition of the previous values is
// recognised later, so it is not bookkeeping that can be dropped.
func (p *parser) appendGroup(list []Decl, f func(*Group) Decl) []Decl {
	if p.Tok == _Lparen {
		g := new(Group)
		// Clear before consuming "(": a comment before it is not attached to
		// the first declaration inside the group.
		p.clearPragma()
		p.next()
		p.list("grouped declaration", _Semi, _Rparen, func() bool {
			if x := f(g); x != nil {
				list = append(list, x)
			}
			return false
		})
	} else {
		if x := f(nil); x != nil {
			list = append(list, x)
		}
	}
	return list
}

// ImportSpec = [ "." | PackageName ] ImportPath .
// ImportPath = string_lit .
func (p *parser) importDecl(group *Group) Decl {
	d := new(ImportDecl)
	d.pos = p.pos()
	d.Group = group
	d.Pragma = p.takePragma()

	switch p.Tok {
	case _Name:
		d.LocalPkgName = p.name()
	case _Dot:
		d.LocalPkgName = NewName(p.pos(), ".")
		p.next()
	}
	d.Path = p.oliteral()
	if d.Path == nil {
		p.syntaxError("missing import path")
		p.advance(_Semi, _Rparen)
		return d
	}
	if !d.Path.Bad && d.Path.Kind != StringLit {
		p.syntaxErrorAt(d.Path.Pos(), "import path must be a string")
		d.Path.Bad = true
	}

	return d
}

// ConstSpec = IdentifierList [ [ Type ] "=" ExpressionList ] .
func (p *parser) constDecl(group *Group) Decl {
	d := new(ConstDecl)
	d.pos = p.pos()
	d.Group = group
	d.Pragma = p.takePragma()

	d.NameList = p.nameList(p.name())
	if p.Tok != _EOF && p.Tok != _Semi && p.Tok != _Rparen {
		d.Type = p.typeOrNil()
		if p.gotAssign() {
			d.Values = p.exprList()
		}
	}

	return d
}

// TypeSpec = identifier [ TypeParams ] [ "=" ] Type .
func (p *parser) typeDecl(group *Group) Decl {
	d := new(TypeDecl)
	d.pos = p.pos()
	d.Group = group
	d.Pragma = p.takePragma()

	d.Name = p.name()
	if p.Tok == _Lbrack {
		// This is the first of the two ambiguities of specs/011. After the
		// name, "[" begins either an array or slice type or a type parameter
		// list, and the prefix "[N" is common to both.
		//
		// The parser does not save and restore the scanner. It parses the
		// expression after "[" once and then asks whether the tree it got can
		// be read as a type parameter name followed by a constraint. One pass,
		// no backtracking.
		pos := p.pos()
		p.next()
		switch p.Tok {
		case _Name:
			// A constraint may itself begin with "[", as in "P []E". Parsing an
			// expression there would fail on "P[]". An index or a slice is
			// never a constant, so it is never a valid array length either: a
			// name followed by "[" must be the start of a constraint, and only
			// when the name is not followed by "[" is a full expression parsed.
			var x Expr = p.name()
			if p.Tok != _Lbrack {
				p.xnest++
				x = p.binaryExpr(p.pexpr(x, false), 0)
				p.xnest--
			}
			// A single name followed by "]" tilts towards an array: the
			// specification reads "type B [N] int" as an array of N ints. A
			// constraint that could also be an ordinary expression tilts
			// towards a type parameter list when a comma follows it, because
			// an array length list does not exist.
			if pname, ptype := extractName(x, p.Tok == _Comma); pname != nil && (ptype != nil || p.Tok != _Rbrack) {
				d.TParamList = p.paramList(pname, ptype, _Rbrack, true, false)
				d.Alias = p.gotAssign()
				d.Type = p.typeOrNil()
			} else {
				d.Type = p.arrayType(pos, x)
			}
		case _Rbrack:
			p.next()
			d.Type = p.sliceType(pos)
		default:
			d.Type = p.arrayType(pos, nil)
		}
	} else {
		d.Alias = p.gotAssign()
		d.Type = p.typeOrNil()
	}

	if d.Type == nil {
		d.Type = p.badExpr()
		p.syntaxError("in type declaration")
		p.advance(_Semi, _Rparen)
	}

	return d
}

// extractName splits x into a name and the expression that follows it, if x can
// be written that way.
//
// The split decides the array against type parameter list ambiguity. It happens
// when the trailing expression is a type element, which an array length can
// never be, or when force is set, which the caller does when a comma follows.
// The result is (nil, x) when x cannot be split.
//
//	x           force    name    expr
//	------------------------------------
//	P*[]int     T/F      P       *[]int
//	P*E         T        P       *E
//	P*E         F        nil     P*E
//	P([]int)    T/F      P       []int
//	P(E)        T        P       E
//	P(E)        F        nil     P(E)
//	P*E|F|~G    T/F      P       *E|F|~G
func extractName(x Expr, force bool) (*Name, Expr) {
	switch x := x.(type) {
	case *Name:
		return x, nil

	case *Operation:
		if x.Y == nil {
			break // unary
		}
		switch x.Op {
		case Mul:
			if name, _ := x.X.(*Name); name != nil && (force || isTypeElem(x.Y)) {
				// x is "name *x.Y"; rebuild the binary * as a unary one.
				op := *x
				op.X, op.Y = op.Y, nil
				return name, &op
			}
		case Or:
			if name, lhs := extractName(x.X, force || isTypeElem(x.Y)); name != nil && lhs != nil {
				// x is "name lhs|x.Y".
				op := *x
				op.X = lhs
				return name, &op
			}
		}

	case *CallExpr:
		if name, _ := x.Fun.(*Name); name != nil {
			if len(x.ArgList) == 1 && !x.HasDots && (force || isTypeElem(x.ArgList[0])) {
				// x is "name (x.ArgList[0])". The tree does not keep
				// parentheses that carry no meaning.
				return name, Unparen(x.ArgList[0])
			}
		}
	}
	return nil, x
}

// isTypeElem reports whether x can only be a type element and not a value.
//
// It is false for anything that could be either, which is what makes it safe to
// decide the ambiguity with: a false answer only means the other evidence has
// to carry the decision.
func isTypeElem(x Expr) bool {
	switch x := x.(type) {
	case *ArrayType, *StructType, *FuncType, *InterfaceType, *SliceType, *MapType, *ChanType:
		return true
	case *Operation:
		return isTypeElem(x.X) || (x.Y != nil && isTypeElem(x.Y)) || x.Op == Tilde
	case *ParenExpr:
		return isTypeElem(x.X)
	}
	return false
}

// VarSpec = IdentifierList ( Type [ "=" ExpressionList ] | "=" ExpressionList ) .
func (p *parser) varDecl(group *Group) Decl {
	d := new(VarDecl)
	d.pos = p.pos()
	d.Group = group
	d.Pragma = p.takePragma()

	d.NameList = p.nameList(p.name())
	if p.gotAssign() {
		d.Values = p.exprList()
	} else {
		d.Type = p.type_()
		if p.gotAssign() {
			d.Values = p.exprList()
		}
	}

	return d
}

// FunctionDecl = "func" FunctionName [ TypeParams ] ( Function | Signature ) .
// MethodDecl   = "func" Receiver MethodName ( Function | Signature ) .
func (p *parser) funcDeclOrNil() *FuncDecl {
	f := new(FuncDecl)
	f.pos = p.pos()
	f.Pragma = p.takePragma()

	hasRecv := false
	if p.got(_Lparen) {
		hasRecv = true
		rcvr := p.paramList(nil, nil, _Rparen, false, false)
		switch len(rcvr) {
		case 0:
			p.error("method has no receiver")
		default:
			p.error("method has multiple receivers")
			fallthrough
		case 1:
			f.Recv = rcvr[0]
		}
	}

	if p.Tok == _Name {
		f.Name = p.name()
		f.TParamList, f.Type = p.funcType("")
	} else {
		// Give the declaration a blank name and an empty signature so that the
		// tree stays walkable and the rest of the file is still read.
		f.Name = NewName(p.pos(), "_")
		f.Type = new(FuncType)
		f.Type.pos = p.pos()
		msg := "expected name or ("
		if hasRecv {
			msg = "expected name"
		}
		p.syntaxError(msg)
		p.advance(_Lbrace, _Semi)
	}

	if p.Tok == _Lbrace {
		f.Body = p.funcBody()
	}

	return f
}

func (p *parser) funcBody() *BlockStmt {
	p.fnest++
	errcnt := p.errcnt
	body := p.blockStmt("")
	p.fnest--

	// A body with syntax errors has holes in it, and a branch check over holes
	// reports targets that are missing only because the parser could not read
	// them. Check only a body that parsed cleanly.
	if p.mode&CheckBranches != 0 && errcnt == p.errcnt {
		checkBranches(body, p)
	}

	return body
}

// ----------------------------------------------------------------------------
// Expressions

func (p *parser) expr() Expr {
	return p.binaryExpr(nil, 0)
}

// binaryExpr is the precedence climbing loop.
//
// Expression = UnaryExpr | Expression binary_op Expression .
func (p *parser) binaryExpr(x Expr, prec int) Expr {
	if x == nil {
		x = p.unaryExpr()
	}
	// _Star carries Mul, so it is a binary operator here as well as a pointer
	// marker in a type.
	for (p.Tok == _Operator || p.Tok == _Star) && p.Prec > prec {
		t := new(Operation)
		t.pos = p.pos()
		t.Op = p.Op
		tprec := p.Prec
		p.next()
		t.X = x
		t.Y = p.binaryExpr(nil, tprec)
		x = t
	}
	return x
}

// UnaryExpr = PrimaryExpr | unary_op UnaryExpr .
func (p *parser) unaryExpr() Expr {
	switch p.Tok {
	case _Operator, _Star:
		switch p.Op {
		case Mul, Add, Sub, Not, Xor, Tilde:
			x := new(Operation)
			x.pos = p.pos()
			x.Op = p.Op
			p.next()
			x.X = p.unaryExpr()
			return x

		case And:
			x := new(Operation)
			x.pos = p.pos()
			x.Op = And
			p.next()
			// &(T{}) and &T{} mean the same thing, and the parenthesis is not
			// worth a node here.
			x.X = Unparen(p.unaryExpr())
			return x
		}

	case _Arrow:
		// "<-x" is a receive and "<-chan E" is a type, and which one it is is
		// known only after the operand is parsed.
		pos := p.pos()
		p.next()
		x := p.unaryExpr()

		if _, ok := x.(*ChanType); ok {
			// The operand is a channel type, so the "<-" belongs to it:
			//   <-(chan E)   is  (<-chan E)
			//   <-(chan<-E)  is  (<-chan (<-E))
			dir := SendOnly
			t := x
			for dir == SendOnly {
				c, ok := t.(*ChanType)
				if !ok {
					break
				}
				dir = c.Dir
				if dir == RecvOnly {
					// "<-<-chan E" is not a type.
					p.syntaxError("unexpected <-, expected chan")
				}
				c.Dir = RecvOnly
				t = c.Elem
			}
			if dir == SendOnly {
				// The direction is "<-" but the element is not a channel.
				p.syntaxError(fmt.Sprintf("unexpected %s, expected chan", String(t)))
			}
			return x
		}

		o := new(Operation)
		o.pos = pos
		o.Op = Recv
		o.X = x
		return o
	}

	// Parentheses are kept here so that "(x) := true" can be rejected.
	return p.pexpr(nil, true)
}

// callStmt parses the call-like statement after "go" or "defer".
func (p *parser) callStmt() *CallStmt {
	s := new(CallStmt)
	s.pos = p.pos()
	s.Tok = p.Tok // _Defer or _Go
	p.next()

	// Keep the parentheses only to report them; the specification forbids them
	// around the expression of a go or defer statement.
	x := p.pexpr(nil, p.Tok == _Lparen)
	if t := Unparen(x); t != x {
		p.errorAt(x.Pos(), fmt.Sprintf("expression in %s must not be parenthesized", s.Tok))
		x = t
	}

	s.Call = x
	return s
}

// Operand     = Literal | OperandName | MethodExpr | "(" Expression ")" .
// Literal     = BasicLit | [ TypeName ] CompositeLit | FunctionLit .
// OperandName = identifier | QualifiedIdent .
func (p *parser) operand(keepParens bool) Expr {
	switch p.Tok {
	case _Name:
		return p.name()

	case _Literal:
		return p.oliteral()

	case _Lbrace:
		return p.compositeLit()

	case _Lparen:
		pos := p.pos()
		p.next()
		// A parenthesis clears the composite literal restriction: the
		// specification forbids a bare literal in a control clause header, and
		// this is where the header stops.
		p.xnest++
		x := p.expr()
		p.xnest--
		p.want(_Rparen)

		// Parentheses are recorded only where an error needs them. A "{" after
		// them means x may be a composite literal type, and parentheses around
		// a composite literal type are not permitted.
		if p.Tok == _Lbrace {
			keepParens = true
		}
		if keepParens {
			px := new(ParenExpr)
			px.pos = pos
			px.X = x
			x = px
		}
		return x

	case _Func:
		pos := p.pos()
		p.next()
		_, ftyp := p.funcType("function type")
		if p.Tok == _Lbrace {
			p.xnest++
			f := new(FuncLit)
			f.pos = pos
			f.Type = ftyp
			f.Body = p.funcBody()
			p.xnest--
			return f
		}
		return ftyp

	case _Lbrack, _Chan, _Map, _Struct, _Interface:
		return p.type_()

	default:
		x := p.badExpr()
		p.syntaxError("expected expression")
		p.advance(_Rparen, _Rbrack, _Rbrace)
		return x
	}
}

// pexpr parses a PrimaryExpr.
//
//	PrimaryExpr   = Operand | Conversion | PrimaryExpr Selector |
//	                PrimaryExpr Index | PrimaryExpr Slice |
//	                PrimaryExpr TypeAssertion | PrimaryExpr Arguments .
//	Selector      = "." identifier .
//	Index         = "[" Expression "]" .
//	Slice         = "[" ( [ Expression ] ":" [ Expression ] ) |
//	                    ( [ Expression ] ":" Expression ":" Expression ) "]" .
//	TypeAssertion = "." "(" Type ")" .
//	Arguments     = "(" [ ( ExpressionList | Type [ "," ExpressionList ] ) [ "..." ] [ "," ] ] ")" .
func (p *parser) pexpr(x Expr, keepParens bool) Expr {
	if x == nil {
		x = p.operand(keepParens)
	}

loop:
	for {
		pos := p.pos()
		switch p.Tok {
		case _Dot:
			p.next()
			switch p.Tok {
			case _Name:
				t := new(SelectorExpr)
				t.pos = pos
				t.X = x
				t.Sel = p.name()
				x = t

			case _Lparen:
				p.next()
				if p.got(_Type) {
					t := new(TypeSwitchGuard)
					// Lhs is filled in by simpleStmt if there is one.
					t.pos = pos
					t.X = x
					x = t
				} else {
					t := new(AssertExpr)
					t.pos = pos
					t.X = x
					t.Type = p.type_()
					x = t
				}
				p.want(_Rparen)

			default:
				p.syntaxError("expected name or (")
				p.advance(_Semi, _Rparen)
			}

		case _Lbrack:
			p.next()

			var i Expr
			if p.Tok != _Colon {
				var comma bool
				if p.Tok == _Rbrack {
					// x[] is neither an index nor an instantiation. Accept it
					// so that the rest of the expression is still read.
					p.syntaxError("expected operand")
					i = p.badExpr()
				} else {
					i, comma = p.typeList(false)
				}
				if comma || p.Tok == _Rbrack {
					p.want(_Rbrack)
					// This is the second ambiguity of specs/011, and the parser
					// does not resolve it. x[i] is an index if x is a value and
					// an instantiation if x is generic, which is a typing
					// question. Index holds a *ListExpr when there is more than
					// one operand, which can only be an instantiation.
					t := new(IndexExpr)
					t.pos = pos
					t.X = x
					t.Index = i
					x = t
					break
				}
			}

			// x[i: ...
			if !p.got(_Colon) {
				p.syntaxError("expected comma, : or ]")
				p.advance(_Comma, _Colon, _Rbrack)
			}
			p.xnest++
			t := new(SliceExpr)
			t.pos = pos
			t.X = x
			t.Index[0] = i
			if p.Tok != _Colon && p.Tok != _Rbrack {
				t.Index[1] = p.expr()
			}
			if p.Tok == _Colon {
				t.Full = true
				if t.Index[1] == nil {
					p.error("middle index required in 3-index slice")
					t.Index[1] = p.badExpr()
				}
				p.next()
				if p.Tok != _Rbrack {
					t.Index[2] = p.expr()
				} else {
					p.error("final index required in 3-index slice")
					t.Index[2] = p.badExpr()
				}
			}
			p.xnest--
			p.want(_Rbrack)
			x = t

		case _Lparen:
			t := new(CallExpr)
			t.pos = pos
			p.next()
			t.Fun = x
			t.ArgList, t.HasDots = p.argList()
			x = t

		case _Lbrace:
			// Decide whether "{" opens a composite literal or a block. A
			// negative xnest means the parser is in a control clause header,
			// where the specification forbids a bare composite literal.
			t := Unparen(x)
			complitOK := false
			switch t.(type) {
			case *Name, *SelectorExpr:
				if p.xnest >= 0 {
					complitOK = true
				}
			case *IndexExpr:
				if p.xnest >= 0 && !isValue(t) {
					complitOK = true
				}
			case *ArrayType, *SliceType, *StructType, *MapType:
				// These can only be types, so "{" can only be a literal.
				complitOK = true
			}
			if !complitOK {
				break loop
			}
			if t != x {
				p.syntaxError("cannot parenthesize type in composite literal")
			}
			n := p.compositeLit()
			n.Type = x
			x = n

		default:
			break loop
		}
	}

	return x
}

// isValue reports whether x can only be a value and not a type.
func isValue(x Expr) bool {
	switch x := x.(type) {
	case *BasicLit, *CompositeLit, *FuncLit, *SliceExpr, *AssertExpr, *TypeSwitchGuard, *CallExpr:
		return true
	case *Operation:
		return x.Op != Mul || x.Y != nil // *T may be a pointer type
	case *ParenExpr:
		return isValue(x.X)
	case *IndexExpr:
		return isValue(x.X) || isValue(x.Index)
	}
	return false
}

// LiteralValue = "{" [ ElementList [ "," ] ] "}" .
func (p *parser) compositeLit() *CompositeLit {
	x := new(CompositeLit)
	x.pos = p.pos()

	// Inside the braces the composite literal restriction no longer holds.
	p.xnest++
	p.want(_Lbrace)
	x.Rbrace = p.list("composite literal", _Comma, _Rbrace, func() bool {
		e := p.expr()
		if p.Tok == _Colon {
			l := new(KeyValueExpr)
			l.pos = p.pos()
			p.next()
			l.Key = e
			l.Value = p.expr()
			e = l
			x.NKeys++
		}
		x.ElemList = append(x.ElemList, e)
		return false
	})
	p.xnest--

	return x
}

// ----------------------------------------------------------------------------
// Types

func (p *parser) type_() Expr {
	typ := p.typeOrNil()
	if typ == nil {
		typ = p.badExpr()
		p.syntaxError("expected type")
		p.advance(_Comma, _Colon, _Semi, _Rparen, _Rbrack, _Rbrace)
	}
	return typ
}

func newIndirect(pos Pos, typ Expr) Expr {
	o := new(Operation)
	o.pos = pos
	o.Op = Mul
	o.X = typ
	return o
}

// typeOrNil is type_ but returns nil instead of reporting an error, for the
// places where a type is optional.
//
//	Type     = TypeName | TypeLit | "(" Type ")" .
//	TypeName = identifier | QualifiedIdent .
//	TypeLit  = ArrayType | StructType | PointerType | FunctionType |
//	           InterfaceType | SliceType | MapType | ChannelType .
func (p *parser) typeOrNil() Expr {
	pos := p.pos()
	switch p.Tok {
	case _Star:
		p.next()
		return newIndirect(pos, p.type_())

	case _Arrow:
		p.next()
		p.want(_Chan)
		t := new(ChanType)
		t.pos = pos
		t.Dir = RecvOnly
		t.Elem = p.chanElem()
		return t

	case _Func:
		p.next()
		_, t := p.funcType("function type")
		return t

	case _Lbrack:
		p.next()
		if p.got(_Rbrack) {
			return p.sliceType(pos)
		}
		return p.arrayType(pos, nil)

	case _Chan:
		p.next()
		t := new(ChanType)
		t.pos = pos
		if p.got(_Arrow) {
			t.Dir = SendOnly
		}
		t.Elem = p.chanElem()
		return t

	case _Map:
		p.next()
		p.want(_Lbrack)
		t := new(MapType)
		t.pos = pos
		t.Key = p.type_()
		p.want(_Rbrack)
		t.Value = p.type_()
		return t

	case _Struct:
		return p.structType()

	case _Interface:
		return p.interfaceType()

	case _Name:
		return p.qualifiedName(nil)

	case _Lparen:
		p.next()
		t := p.type_()
		p.want(_Rparen)
		// The tree does not keep parentheses that carry no meaning, and around
		// a type they never do.
		return t
	}

	return nil
}

// typeInstance parses the "[" T { "," T } "]" of an instantiated type.
func (p *parser) typeInstance(typ Expr) Expr {
	pos := p.pos()
	p.want(_Lbrack)
	x := new(IndexExpr)
	x.pos = pos
	x.X = typ
	if p.Tok == _Rbrack {
		p.syntaxError("expected type argument list")
		x.Index = p.badExpr()
	} else {
		x.Index, _ = p.typeList(true)
	}
	p.want(_Rbrack)
	return x
}

// funcType parses a signature. A non-empty context names a form that may not
// have type parameters, and is used in the error when one is written anyway.
func (p *parser) funcType(context string) ([]*Field, *FuncType) {
	typ := new(FuncType)
	typ.pos = p.pos()

	var tparamList []*Field
	if p.got(_Lbrack) {
		if context != "" {
			p.syntaxErrorAt(typ.pos, context+" must have no type parameters")
		}
		if p.Tok == _Rbrack {
			p.syntaxError("empty type parameter list")
			p.next()
		} else {
			tparamList = p.paramList(nil, nil, _Rbrack, true, false)
		}
	}

	p.want(_Lparen)
	typ.ParamList = p.paramList(nil, nil, _Rparen, false, true)
	typ.ResultList = p.funcResult()

	return tparamList, typ
}

// arrayType parses the rest of an array type. "[" is consumed and pos is its
// position; len is the length if it was already parsed.
func (p *parser) arrayType(pos Pos, len Expr) Expr {
	if len == nil && !p.got(_DotDotDot) {
		p.xnest++
		len = p.expr()
		p.xnest--
	}
	if p.Tok == _Comma {
		// A trailing comma is legal in a type parameter list and not in an
		// array length. Accept it so that the mistake produces one error.
		p.syntaxError("unexpected comma; expected ]")
		p.next()
	}
	p.want(_Rbrack)
	t := new(ArrayType)
	t.pos = pos
	t.Len = len
	t.Elem = p.type_()
	return t
}

// sliceType parses the element type of a slice. "[" and "]" are consumed and
// pos is the position of "[".
func (p *parser) sliceType(pos Pos) Expr {
	t := new(SliceType)
	t.pos = pos
	t.Elem = p.type_()
	return t
}

func (p *parser) chanElem() Expr {
	typ := p.typeOrNil()
	if typ == nil {
		typ = p.badExpr()
		p.syntaxError("missing channel element type")
		// The element is simply absent, so there is nothing to skip.
	}
	return typ
}

// StructType = "struct" "{" { FieldDecl ";" } "}" .
func (p *parser) structType() *StructType {
	typ := new(StructType)
	typ.pos = p.pos()

	p.want(_Struct)
	p.want(_Lbrace)
	p.list("struct type", _Semi, _Rbrace, func() bool {
		p.fieldDecl(typ)
		return false
	})

	return typ
}

// InterfaceType = "interface" "{" { ( MethodDecl | EmbeddedElem ) ";" } "}" .
func (p *parser) interfaceType() *InterfaceType {
	typ := new(InterfaceType)
	typ.pos = p.pos()

	p.want(_Interface)
	p.want(_Lbrace)
	p.list("interface type", _Semi, _Rbrace, func() bool {
		var f *Field
		if p.Tok == _Name {
			f = p.methodDecl()
		}
		if f == nil || f.Name == nil {
			f = p.embeddedElem(f)
		}
		typ.MethodList = append(typ.MethodList, f)
		return false
	})

	return typ
}

// Result = Parameters | Type .
func (p *parser) funcResult() []*Field {
	if p.got(_Lparen) {
		return p.paramList(nil, nil, _Rparen, false, false)
	}

	pos := p.pos()
	if typ := p.typeOrNil(); typ != nil {
		f := new(Field)
		f.pos = pos
		f.Type = typ
		return []*Field{f}
	}

	return nil
}

// addField appends one field, keeping TagList aligned with FieldList. TagList
// stays nil while no field has a tag, which is the common case.
func (p *parser) addField(styp *StructType, pos Pos, name *Name, typ Expr, tag *BasicLit) {
	if tag != nil {
		for i := len(styp.FieldList) - len(styp.TagList); i > 0; i-- {
			styp.TagList = append(styp.TagList, nil)
		}
		styp.TagList = append(styp.TagList, tag)
	}

	f := new(Field)
	f.pos = pos
	f.Name = name
	f.Type = typ
	styp.FieldList = append(styp.FieldList, f)
}

// FieldDecl      = (IdentifierList Type | AnonymousField) [ Tag ] .
// AnonymousField = [ "*" ] TypeName .
// Tag            = string_lit .
func (p *parser) fieldDecl(styp *StructType) {
	pos := p.pos()
	switch p.Tok {
	case _Name:
		name := p.name()
		if p.Tok == _Dot || p.Tok == _Literal || p.Tok == _Semi || p.Tok == _Rbrace {
			typ := p.qualifiedName(name)
			tag := p.oliteral()
			p.addField(styp, pos, nil, typ, tag)
			break
		}

		names := p.nameList(name)
		var typ Expr

		// "T[P]" is an embedded instantiated type and "T [P]E" is a field of
		// array type. Parse the bracket first and decide from what comes out.
		if len(names) == 1 && p.Tok == _Lbrack {
			typ = p.arrayOrTArgs()
			if typ, ok := typ.(*IndexExpr); ok {
				typ.X = name
				tag := p.oliteral()
				p.addField(styp, pos, nil, typ, tag)
				break
			}
		} else {
			typ = p.type_()
		}

		tag := p.oliteral()

		for _, name := range names {
			p.addField(styp, name.Pos(), name, typ, tag)
		}

	case _Star:
		p.next()
		var typ Expr
		if p.Tok == _Lparen {
			p.syntaxError("cannot parenthesize embedded type")
			p.next()
			typ = p.qualifiedName(nil)
			p.got(_Rparen)
		} else {
			typ = p.qualifiedName(nil)
		}
		tag := p.oliteral()
		p.addField(styp, pos, nil, newIndirect(pos, typ), tag)

	case _Lparen:
		p.syntaxError("cannot parenthesize embedded type")
		p.next()
		var typ Expr
		if p.Tok == _Star {
			pos := p.pos()
			p.next()
			typ = newIndirect(pos, p.qualifiedName(nil))
		} else {
			typ = p.qualifiedName(nil)
		}
		p.got(_Rparen)
		tag := p.oliteral()
		p.addField(styp, pos, nil, typ, tag)

	default:
		p.syntaxError("expected field name or embedded type")
		p.advance(_Semi, _Rbrace)
	}
}

// arrayOrTArgs parses "[" ... "]" where it may be an array or slice type or a
// type argument list. The caller fills in IndexExpr.X when the result is one.
func (p *parser) arrayOrTArgs() Expr {
	pos := p.pos()
	p.want(_Lbrack)
	if p.got(_Rbrack) {
		return p.sliceType(pos)
	}

	n, comma := p.typeList(false)
	p.want(_Rbrack)
	if !comma {
		if elem := p.typeOrNil(); elem != nil {
			// A type follows "]", so this was an array length.
			t := new(ArrayType)
			t.pos = pos
			t.Len = n
			t.Elem = elem
			return t
		}
	}

	t := new(IndexExpr)
	t.pos = pos
	t.Index = n
	return t
}

func (p *parser) oliteral() *BasicLit {
	if p.Tok == _Literal {
		b := new(BasicLit)
		b.pos = p.pos()
		b.Value = p.Lit
		b.Kind = p.Kind
		b.Bad = p.Bad
		p.next()
		return b
	}
	return nil
}

// MethodSpec        = MethodName Signature | InterfaceTypeName .
// InterfaceTypeName = TypeName .
func (p *parser) methodDecl() *Field {
	f := new(Field)
	f.pos = p.pos()
	name := p.name()

	const context = "interface method"

	switch p.Tok {
	case _Lparen:
		f.Name = name
		_, f.Type = p.funcType(context)

	case _Lbrack:
		// "m[T C](x T)" is a generic method and "T[P1, P2]" is an embedded
		// instantiated type. A generic method is not legal, but it is parsed
		// and then rejected, which gives a better message than a parse failure.
		pos := p.pos()
		p.next()

		if p.Tok == _Rbrack {
			// Neither list may be empty. Read the "[]" as absent.
			pos := p.pos()
			p.next()
			if p.Tok == _Lparen {
				p.errorAt(pos, "empty type parameter list")
				f.Name = name
				_, f.Type = p.funcType(context)
			} else {
				p.errorAt(pos, "empty type argument list")
				f.Type = name
			}
			break
		}

		// A type argument list is a parameter list whose entries have no
		// names. Parse it as one and decide afterwards.
		list := p.paramList(nil, nil, _Rbrack, false, false)
		if len(list) == 0 {
			// Errors inside the brackets were already reported. Read the list
			// as absent.
			if p.Tok == _Lparen {
				f.Name = name
				_, f.Type = p.funcType(context)
			} else {
				f.Type = name
			}
			break
		}

		if list[0].Name != nil {
			f.Name = name
			_, f.Type = p.funcType(context)
			p.errorAt(pos, "interface method must have no type parameters")
			break
		}

		t := new(IndexExpr)
		t.pos = pos
		t.X = name
		if len(list) == 1 {
			t.Index = list[0].Type
		} else {
			l := new(ListExpr)
			l.pos = list[0].Pos()
			l.ElemList = make([]Expr, len(list))
			for i := range list {
				l.ElemList[i] = list[i].Type
			}
			t.Index = l
		}
		f.Type = t

	default:
		f.Type = p.qualifiedName(name)
	}

	return f
}

// EmbeddedElem = MethodSpec | EmbeddedTerm { "|" EmbeddedTerm } .
func (p *parser) embeddedElem(f *Field) *Field {
	if f == nil {
		f = new(Field)
		f.pos = p.pos()
		f.Type = p.embeddedTerm()
	}

	for p.Tok == _Operator && p.Op == Or {
		t := new(Operation)
		t.pos = p.pos()
		t.Op = Or
		p.next()
		t.X = f.Type
		t.Y = p.embeddedTerm()
		f.Type = t
	}

	return f
}

// EmbeddedTerm = [ "~" ] Type .
func (p *parser) embeddedTerm() Expr {
	if p.Tok == _Operator && p.Op == Tilde {
		t := new(Operation)
		t.pos = p.pos()
		t.Op = Tilde
		p.next()
		t.X = p.type_()
		return t
	}

	t := p.typeOrNil()
	if t == nil {
		t = p.badExpr()
		p.syntaxError("expected ~ term or type")
		p.advance(_Operator, _Semi, _Rparen, _Rbrack, _Rbrace)
	}

	return t
}

// ParameterDecl = [ IdentifierList ] [ "..." ] Type .
func (p *parser) paramDeclOrNil(name *Name, follow Token) *Field {
	// Type set notation is legal in a type parameter list and nowhere else,
	// and a type parameter list is the one closed by "]".
	typeSetsOK := follow == _Rbrack

	pos := p.pos()
	if name != nil {
		pos = name.pos
	} else if typeSetsOK && p.Tok == _Operator && p.Op == Tilde {
		return p.embeddedElem(nil)
	}

	f := new(Field)
	f.pos = pos

	if p.Tok == _Name || name != nil {
		if name == nil {
			name = p.name()
		}

		if p.Tok == _Lbrack {
			// "name [" is either "name [n]E", a named parameter of array type,
			// or "name[T]", an instantiated type used unnamed.
			f.Type = p.arrayOrTArgs()
			if typ, ok := f.Type.(*IndexExpr); ok {
				typ.X = name
			} else {
				f.Name = name
			}
			if typeSetsOK && p.Tok == _Operator && p.Op == Or {
				f = p.embeddedElem(f)
			}
			return f
		}

		if p.Tok == _Dot {
			f.Type = p.qualifiedName(name)
			if typeSetsOK && p.Tok == _Operator && p.Op == Or {
				f = p.embeddedElem(f)
			}
			return f
		}

		if typeSetsOK && p.Tok == _Operator && p.Op == Or {
			f.Type = name
			return p.embeddedElem(f)
		}

		f.Name = name
	}

	if p.Tok == _DotDotDot {
		t := new(DotsType)
		t.pos = p.pos()
		p.next()
		t.Elem = p.typeOrNil()
		if t.Elem == nil {
			f.Type = p.badExpr()
			p.syntaxError("... is missing type")
		} else {
			f.Type = t
		}
		return f
	}

	if typeSetsOK && p.Tok == _Operator && p.Op == Tilde {
		f.Type = p.embeddedElem(nil).Type
		return f
	}

	f.Type = p.typeOrNil()
	if typeSetsOK && p.Tok == _Operator && p.Op == Or && f.Type != nil {
		f = p.embeddedElem(f)
	}
	if f.Name != nil || f.Type != nil {
		return f
	}

	p.syntaxError("expected " + tokstring(follow))
	p.advance(_Comma, follow)
	return nil
}

// paramList parses a parameter, result, receiver, or type parameter list. "("
// or "[" is consumed. name, if given, is the first name after it, and typ, if
// given, is that name's type.
//
// In the result every field has a name or no field does, which is the shape the
// specification requires and the type checker relies on.
func (p *parser) paramList(name *Name, typ Expr, close Token, requireNames, dddok bool) (list []*Field) {
	// list does not call its function when the list is already at its end, so
	// a complete first field has to be handled here.
	if name != nil && typ != nil && p.Tok == close {
		p.next()
		par := new(Field)
		par.pos = name.pos
		par.Name = name
		par.Type = typ
		return []*Field{par}
	}

	var named int // fields with both a name and a type
	var typed int // fields with a type
	end := p.list("parameter list", _Comma, close, func() bool {
		var par *Field
		if typ != nil {
			par = new(Field)
			par.pos = name.pos
			par.Name = name
			par.Type = typ
		} else {
			par = p.paramDeclOrNil(name, close)
		}
		name = nil
		typ = nil
		if par != nil {
			if par.Name != nil && par.Type != nil {
				named++
			}
			if par.Type != nil {
				typed++
			}
			list = append(list, par)
		}
		return false
	})

	if len(list) == 0 {
		return
	}

	// Distribute the types. "func(a, b int)" parses as two fields with names
	// and no types, and "func(int, string)" as two fields whose "names" are
	// really types, and only the whole list says which.
	if named == 0 && !requireNames {
		for _, par := range list {
			if typ := par.Name; typ != nil {
				par.Type = typ
				par.Name = nil
			}
		}
	} else if named != len(list) {
		var errPos Pos // leftmost position that needs an error
		var typ Expr   // the type in force, swept right to left
		for i := len(list) - 1; i >= 0; i-- {
			par := list[i]
			if par.Type != nil {
				typ = par.Type
				if par.Name == nil {
					errPos = StartPos(typ)
					par.Name = NewName(errPos, "_")
				}
			} else if typ != nil {
				par.Type = typ
			} else {
				// Only a name, and no type to its right to take.
				errPos = par.Name.Pos()
				t := p.badExpr()
				t.pos = errPos
				par.Type = t
			}
		}
		if errPos.IsKnown() {
			// Not every field is named. If named == typed then some fields have
			// no type at all, and they must be at the end of the list, because
			// the sweep above would have filled them in otherwise.
			var msg string
			if named == typed {
				errPos = end // report at the closing ) or ]
				if requireNames {
					msg = "missing type constraint"
				} else {
					msg = "missing parameter type"
				}
			} else {
				if requireNames {
					msg = "missing type parameter name"
					// A single entry may be an array length written where a
					// type parameter belongs, so name both possibilities.
					if len(list) == 1 {
						msg += " or invalid array length"
					}
				} else {
					msg = "missing parameter name"
				}
			}
			p.syntaxErrorAt(errPos, msg)
		}
	}

	// Only the final parameter may be variadic.
	first := true
	for i, f := range list {
		if t, _ := f.Type.(*DotsType); t != nil && (!dddok || i+1 < len(list)) {
			if first {
				first = false
				if dddok {
					p.errorAt(t.pos, "can only use ... with final parameter")
				} else {
					p.errorAt(t.pos, "invalid use of ...")
				}
			}
			// Keep T rather than the invalid ...T.
			f.Type = t.Elem
		}
	}

	return
}

func (p *parser) badExpr() *BadExpr {
	b := new(BadExpr)
	b.pos = p.pos()
	return b
}

// ----------------------------------------------------------------------------
// Statements

// SimpleStmt = EmptyStmt | ExpressionStmt | SendStmt | IncDecStmt |
//
//	Assignment | ShortVarDecl .
func (p *parser) simpleStmt(lhs Expr, keyword Token) SimpleStmt {
	if keyword == _For && p.Tok == _Range {
		return p.newRangeClause(nil, false)
	}

	if lhs == nil {
		lhs = p.exprList()
	}

	if _, ok := lhs.(*ListExpr); !ok && p.Tok != _Assign && p.Tok != _Define {
		pos := p.pos()
		switch p.Tok {
		case _AssignOp:
			op := p.Op
			p.next()
			return p.newAssignStmt(pos, op, lhs, p.expr())

		case _IncOp:
			op := p.Op
			p.next()
			// The right hand side is the shared ImplicitOne, so a walker can
			// recognise an increment by identity.
			return p.newAssignStmt(pos, op, lhs, ImplicitOne)

		case _Arrow:
			s := new(SendStmt)
			s.pos = pos
			p.next()
			s.Chan = lhs
			s.Value = p.expr()
			return s

		default:
			s := new(ExprStmt)
			s.pos = lhs.Pos()
			s.X = lhs
			return s
		}
	}

	switch p.Tok {
	case _Assign, _Define:
		pos := p.pos()
		var op Operator
		if p.Tok == _Define {
			op = Def
		}
		p.next()

		if keyword == _For && p.Tok == _Range {
			return p.newRangeClause(lhs, op == Def)
		}

		rhs := p.exprList()

		if x, ok := rhs.(*TypeSwitchGuard); ok && keyword == _Switch && op == Def {
			if lhs, ok := lhs.(*Name); ok {
				// switch lhs := rhs.(type)
				x.Lhs = lhs
				s := new(ExprStmt)
				s.pos = x.Pos()
				s.X = x
				return s
			}
		}

		return p.newAssignStmt(pos, op, lhs, rhs)

	default:
		p.syntaxError("expected := or = or comma")
		p.advance(_Semi, _Rbrace)
		// Keep the first expression so that the statement is not a hole.
		if x, ok := lhs.(*ListExpr); ok {
			lhs = x.ElemList[0]
		}
		s := new(ExprStmt)
		s.pos = lhs.Pos()
		s.X = lhs
		return s
	}
}

func (p *parser) newRangeClause(lhs Expr, def bool) *RangeClause {
	r := new(RangeClause)
	r.pos = p.pos()
	p.next() // consume "range"
	r.Lhs = lhs
	r.Def = def
	r.X = p.expr()
	return r
}

func (p *parser) newAssignStmt(pos Pos, op Operator, lhs, rhs Expr) *AssignStmt {
	a := new(AssignStmt)
	a.pos = pos
	a.Op = op
	a.Lhs = lhs
	a.Rhs = rhs
	return a
}

func (p *parser) labeledStmtOrNil(label *Name) Stmt {
	s := new(LabeledStmt)
	s.pos = p.pos()
	s.Label = label

	p.want(_Colon)

	if p.Tok == _Rbrace {
		// A statement must be terminated by a semicolon, and a semicolon may be
		// omitted before "}", so a "}" here means the statement was empty.
		e := new(EmptyStmt)
		e.pos = p.pos()
		s.Stmt = e
		return s
	}

	s.Stmt = p.stmtOrNil()
	if s.Stmt != nil {
		return s
	}

	p.syntaxErrorAt(s.pos, "missing statement after label")
	// The parser is already past the end of the labeled statement. Returning
	// nil here rather than a Bad node avoids a second error from the caller.
	return nil
}

// blockStmt parses a block. context names the form the block belongs to and
// must be non-empty unless the caller knows the current token is "{".
func (p *parser) blockStmt(context string) *BlockStmt {
	s := new(BlockStmt)
	s.pos = p.pos()

	// Braces are not optional in Go, which is a common mistake from C.
	if !p.got(_Lbrace) {
		p.syntaxError("expected { after " + context)
		p.advance(_Name, _Rbrace)
		s.Rbrace = p.pos()
		if p.got(_Rbrace) {
			return s
		}
	}

	s.List = p.stmtList()
	s.Rbrace = p.pos()
	p.want(_Rbrace)

	return s
}

func (p *parser) declStmt(f func(*Group) Decl) *DeclStmt {
	s := new(DeclStmt)
	s.pos = p.pos()

	p.next() // consume "const", "type" or "var"
	s.DeclList = p.appendGroup(nil, f)

	return s
}

func (p *parser) forStmt() Stmt {
	s := new(ForStmt)
	s.pos = p.pos()

	s.Init, s.Cond, s.Post = p.header(_For)
	s.Body = p.blockStmt("for clause")

	return s
}

// header parses the clause of an if, for, or switch statement.
//
// This is where the composite literal restriction is set. The specification
// forbids a bare composite literal as the operand of these headers, because
// "if x == T{}" cannot be told from "if x == T" followed by a block. xnest goes
// negative for the whole header and every bracket and parenthesis inside it
// raises it back, which is the restriction stated as a rule the parser can
// apply without lookahead.
func (p *parser) header(keyword Token) (init SimpleStmt, cond Expr, post SimpleStmt) {
	p.want(keyword)

	if p.Tok == _Lbrace {
		if keyword == _If {
			p.syntaxError("missing condition in if statement")
			cond = p.badExpr()
		}
		return
	}

	outer := p.xnest
	p.xnest = -1

	if p.Tok != _Semi {
		// A var declaration is not legal here. Accept it and complain, because
		// the rest of the loop is still worth reading.
		if p.got(_Var) {
			p.syntaxError(fmt.Sprintf("var declaration not allowed in %s initializer", keyword.String()))
		}
		init = p.simpleStmt(nil, keyword)
		// A range clause is the whole header. Only "for" can have one.
		if _, ok := init.(*RangeClause); ok {
			p.xnest = outer
			return
		}
	}

	var condStmt SimpleStmt
	var semi struct {
		pos Pos
		// The scanner sets Lit for a semicolon to "semicolon", "newline" or
		// "EOF", which is how the error below tells a written one from one the
		// scanner inserted at the end of a line.
		lit string // valid when pos is known
	}
	if p.Tok != _Lbrace {
		if p.Tok == _Semi {
			semi.pos = p.pos()
			semi.lit = p.Lit
			p.next()
		} else {
			// Asking for "{" rather than ";" gives the better message.
			p.want(_Lbrace)
			if p.Tok != _Lbrace {
				p.advance(_Lbrace, _Rbrace)
			}
		}
		if keyword == _For {
			if p.Tok != _Semi {
				if p.Tok == _Lbrace {
					p.syntaxError("expected for loop condition")
					goto done
				}
				condStmt = p.simpleStmt(nil, 0 /* no range here */)
			}
			p.want(_Semi)
			if p.Tok != _Lbrace {
				post = p.simpleStmt(nil, 0 /* no range here */)
				if a, _ := post.(*AssignStmt); a != nil && a.Op == Def {
					p.syntaxErrorAt(a.Pos(), "cannot declare in post statement of for loop")
				}
			}
		} else if p.Tok != _Lbrace {
			condStmt = p.simpleStmt(nil, keyword)
		}
	} else {
		condStmt = init
		init = nil
	}

done:
	switch s := condStmt.(type) {
	case nil:
		if keyword == _If && semi.pos.IsKnown() {
			if semi.lit != "semicolon" {
				p.syntaxErrorAt(semi.pos, fmt.Sprintf("unexpected %s, expected { after if clause", semi.lit))
			} else {
				p.syntaxErrorAt(semi.pos, "missing condition in if statement")
			}
			b := new(BadExpr)
			b.pos = semi.pos
			cond = b
		}
	case *ExprStmt:
		cond = s.X
	default:
		// Writing "=" for "==" turns the condition into an assignment. Say so,
		// because the generic message sends the reader looking in the wrong
		// place.
		var str string
		if as, ok := s.(*AssignStmt); ok && as.Op == 0 {
			str = "assignment " + emphasize(as.Lhs) + " = " + emphasize(as.Rhs)
		} else {
			str = String(s)
		}
		p.syntaxErrorAt(s.Pos(), fmt.Sprintf("cannot use %s as value", str))
	}

	p.xnest = outer
	return
}

// emphasize parenthesises a binary expression, so that the "=" of a mistaken
// assignment stands out from the operators around it.
func emphasize(x Expr) string {
	s := String(x)
	if op, _ := x.(*Operation); op != nil && op.Y != nil {
		return "(" + s + ")"
	}
	return s
}

func (p *parser) ifStmt() *IfStmt {
	s := new(IfStmt)
	s.pos = p.pos()

	s.Init, s.Cond, _ = p.header(_If)
	s.Then = p.blockStmt("if clause")

	if p.got(_Else) {
		switch p.Tok {
		case _If:
			s.Else = p.ifStmt()
		case _Lbrace:
			s.Else = p.blockStmt("")
		default:
			p.syntaxError("else must be followed by if or statement block")
			p.advance(_Name, _Rbrace)
		}
	}

	return s
}

func (p *parser) switchStmt() *SwitchStmt {
	s := new(SwitchStmt)
	s.pos = p.pos()

	s.Init, s.Tag, _ = p.header(_Switch)

	if !p.got(_Lbrace) {
		p.syntaxError("missing { after switch clause")
		p.advance(_Case, _Default, _Rbrace)
	}
	for p.Tok != _EOF && p.Tok != _Rbrace {
		s.Body = append(s.Body, p.caseClause())
	}
	s.Rbrace = p.pos()
	p.want(_Rbrace)

	return s
}

func (p *parser) selectStmt() *SelectStmt {
	s := new(SelectStmt)
	s.pos = p.pos()

	p.want(_Select)
	if !p.got(_Lbrace) {
		p.syntaxError("missing { after select clause")
		p.advance(_Case, _Default, _Rbrace)
	}
	for p.Tok != _EOF && p.Tok != _Rbrace {
		s.Body = append(s.Body, p.commClause())
	}
	s.Rbrace = p.pos()
	p.want(_Rbrace)

	return s
}

func (p *parser) caseClause() *CaseClause {
	c := new(CaseClause)
	c.pos = p.pos()

	switch p.Tok {
	case _Case:
		p.next()
		c.Cases = p.exprList()

	case _Default:
		p.next()

	default:
		p.syntaxError("expected case or default or }")
		p.advance(_Colon, _Case, _Default, _Rbrace)
	}

	c.Colon = p.pos()
	p.want(_Colon)
	c.Body = p.stmtList()

	return c
}

func (p *parser) commClause() *CommClause {
	c := new(CommClause)
	c.pos = p.pos()

	switch p.Tok {
	case _Case:
		p.next()
		// The specification allows only a send, a receive, or an assignment
		// from a receive here. simpleStmt accepts more; the type checker
		// rejects the rest, which keeps one rule in one place.
		c.Comm = p.simpleStmt(nil, 0)

	case _Default:
		p.next()

	default:
		p.syntaxError("expected case or default or }")
		p.advance(_Colon, _Case, _Default, _Rbrace)
	}

	c.Colon = p.pos()
	p.want(_Colon)
	c.Body = p.stmtList()

	return c
}

// stmtOrNil parses one statement, or returns nil if there is none.
//
//	Statement = Declaration | LabeledStmt | SimpleStmt | GoStmt | ReturnStmt |
//	            BreakStmt | ContinueStmt | GotoStmt | FallthroughStmt | Block |
//	            IfStmt | SwitchStmt | SelectStmt | ForStmt | DeferStmt .
func (p *parser) stmtOrNil() Stmt {
	// Most statements start with a name, so look for that before anything
	// more expensive.
	if p.Tok == _Name {
		p.clearPragma()
		lhs := p.exprList()
		if label, ok := lhs.(*Name); ok && p.Tok == _Colon {
			return p.labeledStmtOrNil(label)
		}
		return p.simpleStmt(lhs, 0)
	}

	switch p.Tok {
	case _Var:
		return p.declStmt(p.varDecl)

	case _Const:
		return p.declStmt(p.constDecl)

	case _Type:
		return p.declStmt(p.typeDecl)
	}

	p.clearPragma()

	switch p.Tok {
	case _Lbrace:
		return p.blockStmt("")

	case _Operator, _Star:
		switch p.Op {
		case Add, Sub, Mul, And, Xor, Not:
			return p.simpleStmt(nil, 0) // unary operator
		}

	case _Literal, _Func, _Lparen, // operands
		_Lbrack, _Struct, _Map, _Chan, _Interface, // composite types
		_Arrow: // receive
		return p.simpleStmt(nil, 0)

	case _For:
		return p.forStmt()

	case _Switch:
		return p.switchStmt()

	case _Select:
		return p.selectStmt()

	case _If:
		return p.ifStmt()

	case _Fallthrough:
		s := new(BranchStmt)
		s.pos = p.pos()
		p.next()
		s.Tok = _Fallthrough
		return s

	case _Break, _Continue:
		s := new(BranchStmt)
		s.pos = p.pos()
		s.Tok = p.Tok
		p.next()
		if p.Tok == _Name {
			s.Label = p.name()
		}
		return s

	case _Go, _Defer:
		return p.callStmt()

	case _Goto:
		s := new(BranchStmt)
		s.pos = p.pos()
		s.Tok = _Goto
		p.next()
		s.Label = p.name()
		return s

	case _Return:
		s := new(ReturnStmt)
		s.pos = p.pos()
		p.next()
		if p.Tok != _Semi && p.Tok != _Rbrace {
			s.Results = p.exprList()
		}
		return s

	case _Semi:
		s := new(EmptyStmt)
		s.pos = p.pos()
		return s
	}

	return nil
}

// StatementList = { Statement ";" } .
func (p *parser) stmtList() (l []Stmt) {
	for p.Tok != _EOF && p.Tok != _Rbrace && p.Tok != _Case && p.Tok != _Default {
		s := p.stmtOrNil()
		p.clearPragma()
		if s == nil {
			break
		}
		l = append(l, s)
		// ";" is optional before "}".
		if !p.got(_Semi) && p.Tok != _Rbrace {
			p.syntaxError("at end of statement")
			p.advance(_Semi, _Rbrace, _Case, _Default)
			p.got(_Semi) // do not leave an empty statement behind
		}
	}
	return
}

// argList = [ arg { "," arg } [ "..." ] [ "," ] ] ")" .
func (p *parser) argList() (list []Expr, hasDots bool) {
	// Inside the parentheses the composite literal restriction no longer holds.
	p.xnest++
	p.list("argument list", _Comma, _Rparen, func() bool {
		list = append(list, p.expr())
		hasDots = p.got(_DotDotDot)
		return hasDots
	})
	p.xnest--

	return
}

// ----------------------------------------------------------------------------
// Common productions

func (p *parser) name() *Name {
	if p.Tok == _Name {
		n := NewName(p.pos(), p.Lit)
		p.next()
		return n
	}

	n := NewName(p.pos(), "_")
	p.syntaxError("expected name")
	p.advance()
	return n
}

// IdentifierList = identifier { "," identifier } .
func (p *parser) nameList(first *Name) []*Name {
	l := []*Name{first}
	for p.got(_Comma) {
		l = append(l, p.name())
	}
	return l
}

// qualifiedName parses a type name, which may be qualified by a package name
// and may be instantiated. The first name may be given.
func (p *parser) qualifiedName(name *Name) Expr {
	var x Expr
	switch {
	case name != nil:
		x = name
	case p.Tok == _Name:
		x = p.name()
	default:
		x = NewName(p.pos(), "_")
		p.syntaxError("expected name")
		p.advance(_Dot, _Semi, _Rbrace)
	}

	if p.Tok == _Dot {
		s := new(SelectorExpr)
		s.pos = p.pos()
		p.next()
		s.X = x
		s.Sel = p.name()
		x = s
	}

	if p.Tok == _Lbrack {
		x = p.typeInstance(x)
	}

	return x
}

// ExpressionList = Expression { "," Expression } .
func (p *parser) exprList() Expr {
	x := p.expr()
	if p.got(_Comma) {
		list := []Expr{x, p.expr()}
		for p.got(_Comma) {
			list = append(list, p.expr())
		}
		t := new(ListExpr)
		t.pos = x.Pos()
		t.ElemList = list
		x = t
	}
	return x
}

// typeList parses a non-empty comma-separated list of types, which may end in a
// comma. When strict is false the first element may be an ordinary expression,
// which is what an index looks like. comma reports whether a comma was seen,
// and that alone decides an instantiation.
//
// typeList = arg { "," arg } [ "," ] .
func (p *parser) typeList(strict bool) (x Expr, comma bool) {
	// Inside the brackets the composite literal restriction no longer holds.
	p.xnest++
	if strict {
		x = p.type_()
	} else {
		x = p.expr()
	}
	if p.got(_Comma) {
		comma = true
		if t := p.typeOrNil(); t != nil {
			list := []Expr{x, t}
			for p.got(_Comma) {
				if t = p.typeOrNil(); t == nil {
					break
				}
				list = append(list, t)
			}
			l := new(ListExpr)
			l.pos = x.Pos()
			l.ElemList = list
			x = l
		}
	}
	p.xnest--
	return
}

// ----------------------------------------------------------------------------
// Branch statements
//
// CheckBranches mode resolves the target of every break, continue, goto and
// fallthrough and reports one that has none. The work is here rather than in
// the type checker because it needs only the tree, and because a branch with no
// target makes every later statement about control flow wrong.

func checkBranches(body *BlockStmt, p *parser) {
	if body == nil {
		return
	}

	ls := &labelScope{p: p}
	fwdGotos := ls.blockBranches(nil, targets{}, nil, body.Pos(), body.List)

	// A forward goto still unresolved has no matching label in scope. Either
	// the label does not exist, or it is inside a block the goto cannot enter.
	for _, fwd := range fwdGotos {
		name := fwd.Label.Value
		if l := ls.labels[name]; l != nil {
			l.used = true // do not also report it as unused
			ls.errf(fwd.Label.Pos(), "goto %s jumps into block starting at %s", name, ls.posString(l.parent.start))
		} else {
			ls.errf(fwd.Label.Pos(), "label %s not defined", name)
		}
	}

	// The specification: "It is illegal to define a label that is never used."
	// The report walks declaration order rather than the map, because
	// specs/053-determinism.md forbids a map iteration on a path that produces
	// output.
	for _, l := range ls.order {
		if !l.used {
			name := l.lstmt.Label
			ls.errf(name.Pos(), "label %s defined and not used", name.Value)
		}
	}
}

type labelScope struct {
	p      *parser
	labels map[string]*label // every label in the function body
	order  []*label          // the same labels in declaration order
}

type label struct {
	parent *block       // block holding the declaration
	lstmt  *LabeledStmt // the statement that declares it
	used   bool
}

type block struct {
	parent *block       // enclosing block, or nil
	start  Pos          // position of the start of the block
	lstmt  *LabeledStmt // the labeled statement this block belongs to, or nil
}

func (ls *labelScope) errf(pos Pos, format string, args ...any) {
	ls.p.errorAt(pos, fmt.Sprintf(format, args...))
}

func (ls *labelScope) posString(pos Pos) string {
	return ls.p.file.Position(pos).String()
}

// declare records the label that s introduces in block b.
func (ls *labelScope) declare(b *block, s *LabeledStmt) *label {
	name := s.Label.Value
	if ls.labels == nil {
		ls.labels = make(map[string]*label)
	} else if alt := ls.labels[name]; alt != nil {
		ls.errf(s.Label.Pos(), "label %s already defined at %s", name, ls.posString(alt.lstmt.Label.Pos()))
		return alt
	}
	l := &label{parent: b, lstmt: s}
	ls.labels[name] = l
	ls.order = append(ls.order, l)
	return l
}

// gotoTarget returns the labeled statement named by name and declared in b or
// an enclosing block, or nil.
func (ls *labelScope) gotoTarget(b *block, name string) *LabeledStmt {
	if l := ls.labels[name]; l != nil {
		l.used = true // used even when it is not a valid target
		for ; b != nil; b = b.parent {
			if l.parent == b {
				return l.lstmt
			}
		}
	}
	return nil
}

// invalidTarget marks a label that exists but does not label a statement the
// branch can name.
var invalidTarget = new(LabeledStmt)

// enclosingTarget returns the innermost enclosing labeled statement named by
// name, nil if there is none, and invalidTarget if the label is not on an
// enclosing statement.
func (ls *labelScope) enclosingTarget(b *block, name string) *LabeledStmt {
	if l := ls.labels[name]; l != nil {
		l.used = true
		for ; b != nil; b = b.parent {
			if l.lstmt == b.lstmt {
				return l.lstmt
			}
		}
		return invalidTarget
	}
	return nil
}

// targets holds the statements a break or continue may name.
type targets struct {
	breaks    Stmt     // *ForStmt, *SwitchStmt, *SelectStmt, or nil
	continues *ForStmt // or nil
	caseIndex int      // case index of the enclosing switch, or negative
}

// blockBranches walks one block and returns the forward gotos it could not
// resolve, which the enclosing block continues to look for.
func (ls *labelScope) blockBranches(parent *block, ctxt targets, lstmt *LabeledStmt, start Pos, body []Stmt) []*BranchStmt {
	b := &block{parent: parent, start: start, lstmt: lstmt}

	var varPos Pos
	var varName Expr
	var fwdGotos, badGotos []*BranchStmt

	recordVarDecl := func(pos Pos, name Expr) {
		varPos = pos
		varName = name
		// A forward goto seen before this declaration jumps over it. It may
		// still leave the block and be legal, so remember it rather than
		// report it.
		badGotos = append(badGotos[:0], fwdGotos...)
	}

	jumpsOverVarDecl := func(fwd *BranchStmt) bool {
		if varPos.IsKnown() {
			for _, bad := range badGotos {
				if fwd == bad {
					return true
				}
			}
		}
		return false
	}

	innerBlock := func(ctxt targets, start Pos, body []Stmt) {
		fwdGotos = append(fwdGotos, ls.blockBranches(b, ctxt, lstmt, start, body)...)
	}

	// A fallthrough is the last statement of a case even when empty statements
	// follow it.
	stmtList := trimTrailingEmptyStmts(body)
	for stmtIndex, stmt := range stmtList {
		lstmt = nil
	L:
		switch s := stmt.(type) {
		case *DeclStmt:
			for _, d := range s.DeclList {
				if v, ok := d.(*VarDecl); ok {
					recordVarDecl(v.Pos(), v.NameList[0])
					break // the first one is enough
				}
			}

		case *LabeledStmt:
			if name := s.Label.Value; name != "_" {
				l := ls.declare(b, s)
				// Resolve the forward gotos this label answers.
				i := 0
				for _, fwd := range fwdGotos {
					if fwd.Label.Value == name {
						fwd.Target = s
						l.used = true
						if jumpsOverVarDecl(fwd) {
							ls.errf(fwd.Label.Pos(), "goto %s jumps over declaration of %s at %s",
								name, String(varName), ls.posString(varPos))
						}
					} else {
						fwdGotos[i] = fwd
						i++
					}
				}
				fwdGotos = fwdGotos[:i]
				lstmt = s
			}
			stmt = s.Stmt
			goto L

		case *BranchStmt:
			if s.Label == nil {
				switch s.Tok {
				case _Break:
					if t := ctxt.breaks; t != nil {
						s.Target = t
					} else {
						ls.errf(s.Pos(), "break is not in a loop, switch, or select")
					}
				case _Continue:
					if t := ctxt.continues; t != nil {
						s.Target = t
					} else {
						ls.errf(s.Pos(), "continue is not in a loop")
					}
				case _Fallthrough:
					msg := "fallthrough statement out of place"
					if t, _ := ctxt.breaks.(*SwitchStmt); t != nil {
						if _, ok := t.Tag.(*TypeSwitchGuard); ok {
							msg = "cannot fallthrough in type switch"
						} else if ctxt.caseIndex < 0 || stmtIndex+1 < len(stmtList) {
							// Nested in a block, or not the last statement.
						} else if ctxt.caseIndex+1 == len(t.Body) {
							msg = "cannot fallthrough final case in switch"
						} else {
							break // a legal fallthrough
						}
					}
					ls.errf(s.Pos(), "%s", msg)
				}
				break
			}

			name := s.Label.Value
			switch s.Tok {
			case _Break:
				// The specification: the label must be that of an enclosing
				// for, switch, or select statement.
				if t := ls.enclosingTarget(b, name); t != nil {
					switch t := t.Stmt.(type) {
					case *SwitchStmt, *SelectStmt, *ForStmt:
						s.Target = t
					default:
						ls.errf(s.Label.Pos(), "invalid break label %s", name)
					}
				} else {
					ls.errf(s.Label.Pos(), "break label not defined: %s", name)
				}

			case _Continue:
				// The specification: the label must be that of an enclosing
				// for statement.
				if t := ls.enclosingTarget(b, name); t != nil {
					if t, ok := t.Stmt.(*ForStmt); ok {
						s.Target = t
					} else {
						ls.errf(s.Label.Pos(), "invalid continue label %s", name)
					}
				} else {
					ls.errf(s.Label.Pos(), "continue label not defined: %s", name)
				}

			case _Goto:
				if t := ls.gotoTarget(b, name); t != nil {
					s.Target = t
				} else {
					// The label may come later in the block.
					fwdGotos = append(fwdGotos, s)
				}
			}

		case *AssignStmt:
			if s.Op == Def {
				recordVarDecl(s.Pos(), s.Lhs)
			}

		case *BlockStmt:
			inner := targets{ctxt.breaks, ctxt.continues, -1}
			innerBlock(inner, s.Pos(), s.List)

		case *IfStmt:
			inner := targets{ctxt.breaks, ctxt.continues, -1}
			innerBlock(inner, s.Then.Pos(), s.Then.List)
			if s.Else != nil {
				innerBlock(inner, s.Else.Pos(), []Stmt{s.Else})
			}

		case *ForStmt:
			inner := targets{s, s, -1}
			innerBlock(inner, s.Body.Pos(), s.Body.List)

		case *SwitchStmt:
			inner := targets{s, ctxt.continues, -1}
			for i, cc := range s.Body {
				inner.caseIndex = i
				innerBlock(inner, cc.Pos(), cc.Body)
			}

		case *SelectStmt:
			inner := targets{s, ctxt.continues, -1}
			for _, cc := range s.Body {
				innerBlock(inner, cc.Pos(), cc.Body)
			}
		}
	}

	return fwdGotos
}

func trimTrailingEmptyStmts(list []Stmt) []Stmt {
	for i := len(list); i > 0; i-- {
		if _, ok := list[i-1].(*EmptyStmt); !ok {
			return list[:i]
		}
	}
	return nil
}
