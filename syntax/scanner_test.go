// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scanAll scans src and returns the tokens and the errors, in source order.
type tok struct {
	tok  Token
	lit  string
	kind LitKind
	op   Operator
	bad  bool
	pos  Position
}

func scanAll(t *testing.T, name, src string) ([]tok, []Error) {
	t.Helper()
	fset := NewFileSet()
	f := fset.AddFile(name, len(src))
	var errs []Error
	var s Scanner
	s.Init(f, []byte(src), func(err Error) { errs = append(errs, err) }, nil, 0)
	var toks []tok
	for {
		toks = append(toks, tok{s.Tok, s.Lit, s.Kind, s.Op, s.Bad, f.Position(s.Pos)})
		if s.Tok == _EOF {
			break
		}
		if len(toks) > 100000 {
			t.Fatal("scanner does not reach EOF")
		}
		s.Next()
	}
	if s.NumErrors() != len(errs) {
		t.Errorf("%s: NumErrors = %d, handler saw %d", name, s.NumErrors(), len(errs))
	}
	return toks, errs
}

// tokens returns only the token values, which is what most tests compare.
func tokens(ts []tok) []Token {
	out := make([]Token, len(ts))
	for i, x := range ts {
		out[i] = x.tok
	}
	return out
}

func TestScannerInit(t *testing.T) {
	// The parser reads the current token before it calls Next, so Init must
	// leave the first token current.
	src := "package p"
	fset := NewFileSet()
	f := fset.AddFile("t.go", len(src))
	var s Scanner
	s.Init(f, []byte(src), nil, nil, 0)
	if s.Tok != _Package {
		t.Fatalf("after Init: Tok = %s, want %s", s.Tok, _Package)
	}
	if got := f.Position(s.Pos); got.Line != 1 || got.Col != 1 {
		t.Fatalf("after Init: Pos = %s, want 1:1", got)
	}
	s.Next()
	if s.Tok != _Name || s.Lit != "p" {
		t.Fatalf("after Next: %s %q, want name p", s.Tok, s.Lit)
	}
}

func TestScannerAtEOF(t *testing.T) {
	// The token stays at _EOF, so a caller that fails to stop loops rather
	// than reading past the end of the input.
	const src = "a"
	fset := NewFileSet()
	f := fset.AddFile("t.go", len(src))
	var s Scanner
	s.Init(f, []byte(src), func(err Error) { t.Errorf("unexpected error %v", err) }, nil, 0)
	for s.Tok != _EOF {
		s.Next()
	}
	pos := s.Pos
	for range 5 {
		s.Next()
		if s.Tok != _EOF || s.Pos != pos {
			t.Fatalf("past the end: %s at %s, want EOF at %s", s.Tok, f.Position(s.Pos), f.Position(pos))
		}
	}
}

func TestScannerEmpty(t *testing.T) {
	// The last two hold a directive with no handler installed, which the
	// scanner must drop rather than route.
	for _, src := range []string{"", "\n", "  \t\n\n", "// only a comment", "/* only a comment */",
		"//go:build ignore\n", "//line foo.go:10\n"} {
		toks, errs := scanAll(t, "t.go", src)
		if len(errs) != 0 {
			t.Errorf("%q: unexpected errors %v", src, errs)
		}
		if len(toks) != 1 || toks[0].tok != _EOF {
			t.Errorf("%q: got %v, want [EOF]", src, tokens(toks))
		}
	}
}

func TestScannerTokens(t *testing.T) {
	// One case per token, so that every branch of the operator switch runs.
	tests := []struct {
		src  string
		want []Token
	}{
		{"a", []Token{_Name, _Semi, _EOF}},
		{"_x9 X é 日本", []Token{_Name, _Name, _Name, _Name, _Semi, _EOF}},
		{"break", []Token{_Break, _Semi, _EOF}},
		{"case chan const default defer else", []Token{_Case, _Chan, _Const, _Default, _Defer, _Else, _EOF}},
		{"continue fallthrough for func go goto", []Token{_Continue, _Fallthrough, _For, _Func, _Go, _Goto, _EOF}},
		{"if import interface map package range", []Token{_If, _Import, _Interface, _Map, _Package, _Range, _EOF}},
		{"return select struct switch type var", []Token{_Return, _Select, _Struct, _Switch, _Type, _Var, _EOF}},
		{"( ) [ ] { } , ; : .", []Token{_Lparen, _Rparen, _Lbrack, _Rbrack, _Lbrace, _Rbrace, _Comma, _Semi, _Colon, _Dot, _EOF}},
		{"...", []Token{_DotDotDot, _EOF}},
		{"..", []Token{_Dot, _Dot, _EOF}},
		{"= := * <- ++ --", []Token{_Assign, _Define, _Star, _Arrow, _IncOp, _IncOp, _Semi, _EOF}},
		{"+ - | ^ / % & &^ << >>", []Token{_Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _EOF}},
		{"+= -= |= ^= *= /= %= &= &^= <<= >>=", []Token{_AssignOp, _AssignOp, _AssignOp, _AssignOp, _AssignOp, _AssignOp, _AssignOp, _AssignOp, _AssignOp, _AssignOp, _AssignOp, _EOF}},
		{"== != < <= > >= && || ! ~", []Token{_Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _Operator, _EOF}},
		{"1 1.5 1i 'a' \"s\" `r`", []Token{_Literal, _Literal, _Literal, _Literal, _Literal, _Literal, _Semi, _EOF}},
	}
	for _, test := range tests {
		toks, errs := scanAll(t, "t.go", test.src)
		if len(errs) != 0 {
			t.Errorf("%q: unexpected errors %v", test.src, errs)
		}
		got := tokens(toks)
		if fmt.Sprint(got) != fmt.Sprint(test.want) {
			t.Errorf("%q:\n got %v\nwant %v", test.src, got, test.want)
		}
	}
}

func TestScannerOperators(t *testing.T) {
	// The operator identity and its precedence travel with the token, because
	// the parser climbs precedence rather than calling one method per level.
	tests := []struct {
		src  string
		tok  Token
		op   Operator
		prec int
	}{
		{"||", _Operator, OrOr, PrecOrOr},
		{"&&", _Operator, AndAnd, PrecAndAnd},
		{"==", _Operator, Eql, PrecCmp},
		{"!=", _Operator, Neq, PrecCmp},
		{"<", _Operator, Lss, PrecCmp},
		{"<=", _Operator, Leq, PrecCmp},
		{">", _Operator, Gtr, PrecCmp},
		{">=", _Operator, Geq, PrecCmp},
		{"+", _Operator, Add, PrecAdd},
		{"-", _Operator, Sub, PrecAdd},
		{"|", _Operator, Or, PrecAdd},
		{"^", _Operator, Xor, PrecAdd},
		{"*", _Star, Mul, PrecMul},
		{"/", _Operator, Div, PrecMul},
		{"%", _Operator, Rem, PrecMul},
		{"&", _Operator, And, PrecMul},
		{"&^", _Operator, AndNot, PrecMul},
		{"<<", _Operator, Shl, PrecMul},
		{">>", _Operator, Shr, PrecMul},
		{"!", _Operator, Not, PrecLowest},
		{"~", _Operator, Tilde, PrecLowest},
		{"<-", _Arrow, Recv, PrecLowest},
		{"+=", _AssignOp, Add, PrecAdd},
		{"&^=", _AssignOp, AndNot, PrecMul},
		{"*=", _AssignOp, Mul, PrecMul},
		{"++", _IncOp, Add, PrecAdd},
		{"--", _IncOp, Sub, PrecAdd},
		{":=", _Define, Def, PrecLowest},
	}
	for _, test := range tests {
		fset := NewFileSet()
		f := fset.AddFile("t.go", len(test.src))
		var s Scanner
		s.Init(f, []byte(test.src), func(err Error) { t.Errorf("%q: %v", test.src, err) }, nil, 0)
		if s.Tok != test.tok || s.Op != test.op || s.Prec != test.prec {
			t.Errorf("%q: got %s %s prec %d, want %s %s prec %d",
				test.src, s.Tok, s.Op, s.Prec, test.tok, test.op, test.prec)
		}
		if s.Op.Precedence() != test.prec && test.tok != _IncOp && test.tok != _Define && test.tok != _Arrow {
			t.Errorf("%q: Prec disagrees with Op.Precedence", test.src)
		}
	}
}

func TestScannerLiterals(t *testing.T) {
	tests := []struct {
		src  string
		kind LitKind
	}{
		{"0", IntLit},
		{"07", IntLit},
		{"0_7", IntLit},
		{"0b1010", IntLit},
		{"0B_1_0", IntLit},
		{"0o17", IntLit},
		{"0O_1_7", IntLit},
		{"0xdeadBEEF", IntLit},
		{"0X_1_f", IntLit},
		{"1_000_000", IntLit},
		{"1.", FloatLit},
		{".5", FloatLit},
		{"1_0.5_0e-1_0", FloatLit},
		{"1e10", FloatLit},
		{"1E+10", FloatLit},
		{"0x1.8p3", FloatLit},
		{"0x1p-2", FloatLit},
		{"0x.8p1", FloatLit},
		{"0x1_8.p0", FloatLit},
		{"1i", ImagLit},
		{"1.5i", ImagLit},
		{"0x1p3i", ImagLit},
		{"0b11i", ImagLit},
		{"'a'", RuneLit},
		{"'\\n'", RuneLit},
		{"'\\377'", RuneLit},
		{"'\\xff'", RuneLit},
		{"'\\u00e9'", RuneLit},
		{"'\\U0001F600'", RuneLit},
		{"'\\''", RuneLit},
		{"'é'", RuneLit},
		{`"abc"`, StringLit},
		{`"\x00\007\n\"\\"`, StringLit},
		{"``", StringLit},
		{"`a\nb`", StringLit},
	}
	for _, test := range tests {
		toks, errs := scanAll(t, "t.go", test.src)
		if len(errs) != 0 {
			t.Errorf("%q: unexpected errors %v", test.src, errs)
			continue
		}
		if toks[0].tok != _Literal || toks[0].kind != test.kind {
			t.Errorf("%q: got %s %s, want literal %s", test.src, toks[0].tok, toks[0].kind, test.kind)
			continue
		}
		if toks[0].lit != test.src {
			t.Errorf("%q: literal text is %q", test.src, toks[0].lit)
		}
	}
}

func TestScannerRawStringCR(t *testing.T) {
	// A carriage return is not part of the value of a raw string.
	toks, errs := scanAll(t, "t.go", "`a\r\nb`")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors %v", errs)
	}
	if toks[0].lit != "`a\nb`" {
		t.Errorf("raw string literal is %q, want %q", toks[0].lit, "`a\nb`")
	}
}

func TestScannerSemicolons(t *testing.T) {
	tests := []struct {
		src  string
		want []Token
	}{
		// Every token the Go specification lists ends a line with a semicolon.
		{"a\n", []Token{_Name, _Semi, _EOF}},
		{"1\n", []Token{_Literal, _Semi, _EOF}},
		{"1.5\n", []Token{_Literal, _Semi, _EOF}},
		{"1i\n", []Token{_Literal, _Semi, _EOF}},
		{"'a'\n", []Token{_Literal, _Semi, _EOF}},
		{`"a"` + "\n", []Token{_Literal, _Semi, _EOF}},
		{"break\n", []Token{_Break, _Semi, _EOF}},
		{"continue\n", []Token{_Continue, _Semi, _EOF}},
		{"fallthrough\n", []Token{_Fallthrough, _Semi, _EOF}},
		{"return\n", []Token{_Return, _Semi, _EOF}},
		{"a++\n", []Token{_Name, _IncOp, _Semi, _EOF}},
		{"a--\n", []Token{_Name, _IncOp, _Semi, _EOF}},
		{")\n", []Token{_Rparen, _Semi, _EOF}},
		{"]\n", []Token{_Rbrack, _Semi, _EOF}},
		{"}\n", []Token{_Rbrace, _Semi, _EOF}},
		// No other token does.
		{"go\n", []Token{_Go, _EOF}},
		{"+\n", []Token{_Operator, _EOF}},
		{"(\n", []Token{_Lparen, _EOF}},
		{",\n", []Token{_Comma, _EOF}},
		// One semicolon per line, not one per newline.
		{"a\n\n\nb\n", []Token{_Name, _Semi, _Name, _Semi, _EOF}},
		// At the end of the input, with or without a final newline.
		{"a", []Token{_Name, _Semi, _EOF}},
		{"a\n", []Token{_Name, _Semi, _EOF}},
		// A written semicolon is not doubled by an inserted one.
		{"a;\n", []Token{_Name, _Semi, _EOF}},
		// A line comment does not hide the newline under it.
		{"a // c\nb", []Token{_Name, _Semi, _Name, _Semi, _EOF}},
		{"a // c", []Token{_Name, _Semi, _EOF}},
		// A general comment with a newline acts as a newline; one without
		// does not. The Go specification says so and the distribution has
		// both forms.
		{"a /* c */ b", []Token{_Name, _Name, _Semi, _EOF}},
		{"a /* \n */ b", []Token{_Name, _Semi, _Name, _Semi, _EOF}},
		{"a /*\n*/", []Token{_Name, _Semi, _EOF}},
		{"+ /* \n */ b", []Token{_Operator, _Name, _Semi, _EOF}},
		{"a /* c */\nb", []Token{_Name, _Semi, _Name, _Semi, _EOF}},
	}
	for _, test := range tests {
		toks, errs := scanAll(t, "t.go", test.src)
		if len(errs) != 0 {
			t.Errorf("%q: unexpected errors %v", test.src, errs)
		}
		got := tokens(toks)
		if fmt.Sprint(got) != fmt.Sprint(test.want) {
			t.Errorf("%q:\n got %v\nwant %v", test.src, got, test.want)
		}
	}
}

func TestScannerSemicolonText(t *testing.T) {
	// The parser tells an inserted semicolon from a written one by the text,
	// and words its errors differently for each.
	toks, _ := scanAll(t, "t.go", "a\nb;\nc")
	want := []string{"", "newline", "", "semicolon", "", "EOF", ""}
	for i, x := range toks {
		if x.lit != want[i] && x.tok == _Semi {
			t.Errorf("token %d (%s): Lit = %q, want %q", i, x.tok, x.lit, want[i])
		}
	}
}

func TestScannerErrors(t *testing.T) {
	// Each case pins the position of the first error. The scanner reports and
	// continues, so the token stream after the error is still produced.
	tests := []struct {
		src  string
		pos  string // line:col of the first error
		msg  string // substring of the message
		bad  bool   // the literal token is marked bad
		errs int    // number of errors
	}{
		{"0b", "1:3", "binary literal has no digits", true, 1},
		{"0o", "1:3", "octal literal has no digits", true, 1},
		{"0x", "1:3", "hexadecimal literal has no digits", true, 1},
		{"0b12", "1:4", "invalid digit '2' in binary literal", true, 1},
		{"0o18", "1:4", "invalid digit '8' in octal literal", true, 1},
		{"08", "1:2", "invalid digit '8' in octal literal", true, 1},
		{"0b1.1", "1:4", "invalid radix point in binary literal", true, 1},
		{"0o1.1", "1:4", "invalid radix point in octal literal", true, 1},
		{"1e", "1:3", "exponent has no digits", true, 1},
		{"1e+", "1:4", "exponent has no digits", true, 1},
		{"0x1.8", "1:6", "hexadecimal mantissa requires a 'p' exponent", true, 1},
		{"0x1p", "1:5", "exponent has no digits", true, 1},
		{"1p3", "1:2", "exponent requires hexadecimal mantissa", true, 1},
		{"0b1e3", "1:4", "exponent requires decimal mantissa", true, 1},
		// A misplaced separator is reported at the separator itself.
		{"_1", "", "", false, 0}, // an identifier, not a literal
		{"1__2", "1:3", "'_' must separate successive digits", true, 1},
		{"1_", "1:2", "'_' must separate successive digits", true, 1},
		{"0_x1", "1:2", "'_' must separate successive digits", true, 1},
		{"0x_1", "", "", false, 0},
		{"1_.5", "1:2", "'_' must separate successive digits", true, 1},
		{"1._5", "1:3", "'_' must separate successive digits", true, 1},
		{"1.5e_1", "1:5", "'_' must separate successive digits", true, 1},
		// Rune and string literals.
		{"''", "1:2", "empty rune literal", true, 1},
		{"'ab'", "1:1", "more than one character in rune literal", true, 1},
		{"'a", "1:1", "rune literal not terminated", true, 1},
		{"'\n'", "1:2", "newline in rune literal", true, 2},
		{`"a`, "1:1", "string not terminated", true, 1},
		{"\"a\n\"", "1:3", "newline in string", true, 2},
		{"`a", "1:1", "string not terminated", true, 1},
		{`"\q"`, "1:3", "unknown escape", true, 1},
		{`"\x0g"`, "1:5", "invalid character 'g' in hexadecimal escape", true, 1},
		{`"\800"`, "1:3", "unknown escape", true, 1},
		{`'\400'`, "1:1", "octal escape value 256 > 255", true, 1},
		{`'\ud800'`, "1:1", "escape is invalid Unicode code point", true, 1},
		{`'\U00110000'`, "1:1", "escape is invalid Unicode code point", true, 1},
		{"'\\u{}'", "1:4", "invalid character '{' in hexadecimal escape", true, 1},
		// Comments and characters.
		{"/* a", "1:1", "comment not terminated", false, 1},
		{"a\x00b", "1:2", "invalid NUL character", false, 1},
		{"\"a\x00b\"", "1:3", "invalid NUL character", false, 1},
		{"a\xffb", "1:2", "invalid UTF-8 encoding", false, 1},
		{"\"a\xffb\"", "1:3", "invalid UTF-8 encoding", false, 1},
		{"a\ufeffb", "1:2", "invalid BOM in the middle of the file", false, 1},
		{"#", "1:1", "invalid character U+0023 '#'", false, 1},
		{"a\x01", "1:2", "invalid character U+0001", false, 1},
		{"日本 = 1", "", "", false, 0},
		{"\u0661x", "1:1", "identifier cannot begin with digit U+0661", false, 1},
		{"\"a\ufeffb\"", "1:3", "invalid BOM in the middle of the file", false, 1},
		{`"\`, "1:1", "string not terminated", true, 1},
		{`"\x`, "1:1", "string not terminated", true, 1},
		{"a·b", "1:2", "invalid character U+00B7 '·' in identifier", false, 1},
	}
	for _, test := range tests {
		toks, errs := scanAll(t, "t.go", test.src)
		if len(errs) != test.errs {
			t.Errorf("%q: got %d errors %v, want %d", test.src, len(errs), errs, test.errs)
			continue
		}
		if test.errs == 0 {
			continue
		}
		fset := NewFileSet()
		f := fset.AddFile("t.go", len(test.src))
		_ = f
		pos := positionOf(test.src, errs[0].Pos)
		if pos != test.pos {
			t.Errorf("%q: error at %s, want %s (%s)", test.src, pos, test.pos, errs[0].Msg)
		}
		if !strings.Contains(errs[0].Msg, test.msg) {
			t.Errorf("%q: message %q does not contain %q", test.src, errs[0].Msg, test.msg)
		}
		if test.bad && !toks[0].badLiteral() {
			t.Errorf("%q: the literal is not marked bad", test.src)
		}
	}
}

// badLiteral reports whether the token is a literal the scanner rejected. The
// flag is recorded on the Scanner, so the test re-scans to read it.
func (x tok) badLiteral() bool { return x.bad }

func positionOf(src string, p Pos) string {
	// The scanner in scanAll builds its own FileSet, whose first file always
	// starts at Pos 1, so the offset is recoverable without it.
	off := int(p) - 1
	line, col := 1, 1
	for i := 0; i < off && i < len(src); i++ {
		if src[i] == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return fmt.Sprintf("%d:%d", line, col)
}

func TestScannerErrorRecovery(t *testing.T) {
	// The scanner never stops at the first error, because the conformance
	// corpus annotates several errors in one file.
	toks, errs := scanAll(t, "t.go", "a := 1__2 + 0b2\nb := `x")
	if len(errs) != 3 {
		t.Fatalf("got %d errors %v, want 3", len(errs), errs)
	}
	if toks[len(toks)-1].tok != _EOF {
		t.Errorf("the scanner did not reach EOF: %v", tokens(toks))
	}
}

// The tests below compare against go/scanner and go/token. The token sets
// differ, so the comparison needs a mapping; the position models do not, so the
// positions are compared directly.

// goTok is the token go/scanner produced.
type goTok struct {
	pos token.Pos
	tok token.Token
	lit string
}

// mapToken returns the go/token token that x stands for.
func mapToken(x tok) token.Token {
	switch x.tok {
	case _EOF:
		return token.EOF
	case _Name:
		return token.IDENT
	case _Literal:
		switch x.kind {
		case IntLit:
			return token.INT
		case FloatLit:
			return token.FLOAT
		case ImagLit:
			return token.IMAG
		case RuneLit:
			return token.CHAR
		case StringLit:
			return token.STRING
		}
	case _Operator, _AssignOp, _IncOp:
		return mapOperator(x.tok, x.op)
	case _Assign:
		return token.ASSIGN
	case _Define:
		return token.DEFINE
	case _Arrow:
		return token.ARROW
	case _Star:
		return token.MUL
	case _Lparen:
		return token.LPAREN
	case _Lbrack:
		return token.LBRACK
	case _Lbrace:
		return token.LBRACE
	case _Rparen:
		return token.RPAREN
	case _Rbrack:
		return token.RBRACK
	case _Rbrace:
		return token.RBRACE
	case _Comma:
		return token.COMMA
	case _Semi:
		return token.SEMICOLON
	case _Colon:
		return token.COLON
	case _Dot:
		return token.PERIOD
	case _DotDotDot:
		return token.ELLIPSIS
	}
	if x.tok.IsKeyword() {
		return token.Lookup(x.tok.String())
	}
	return token.ILLEGAL
}

func mapOperator(t Token, op Operator) token.Token {
	if t == _IncOp {
		if op == Add {
			return token.INC
		}
		return token.DEC
	}
	var plain token.Token
	switch op {
	case Not:
		plain = token.NOT
	case Recv:
		plain = token.ARROW
	case Tilde:
		plain = token.TILDE
	case OrOr:
		plain = token.LOR
	case AndAnd:
		plain = token.LAND
	case Eql:
		plain = token.EQL
	case Neq:
		plain = token.NEQ
	case Lss:
		plain = token.LSS
	case Leq:
		plain = token.LEQ
	case Gtr:
		plain = token.GTR
	case Geq:
		plain = token.GEQ
	case Add:
		plain = token.ADD
	case Sub:
		plain = token.SUB
	case Or:
		plain = token.OR
	case Xor:
		plain = token.XOR
	case Mul:
		plain = token.MUL
	case Div:
		plain = token.QUO
	case Rem:
		plain = token.REM
	case And:
		plain = token.AND
	case AndNot:
		plain = token.AND_NOT
	case Shl:
		plain = token.SHL
	case Shr:
		plain = token.SHR
	default:
		return token.ILLEGAL
	}
	if t != _AssignOp {
		return plain
	}
	switch plain {
	case token.ADD:
		return token.ADD_ASSIGN
	case token.SUB:
		return token.SUB_ASSIGN
	case token.MUL:
		return token.MUL_ASSIGN
	case token.QUO:
		return token.QUO_ASSIGN
	case token.REM:
		return token.REM_ASSIGN
	case token.AND:
		return token.AND_ASSIGN
	case token.OR:
		return token.OR_ASSIGN
	case token.XOR:
		return token.XOR_ASSIGN
	case token.SHL:
		return token.SHL_ASSIGN
	case token.SHR:
		return token.SHR_ASSIGN
	case token.AND_NOT:
		return token.AND_NOT_ASSIGN
	}
	return token.ILLEGAL
}

// scanNanogo scans src and returns the tokens, the file, and the error count.
func scanNanogo(name string, src []byte) (*SrcFile, []tok, int) {
	fset := NewFileSet()
	f := fset.AddFile(name, len(src))
	var s Scanner
	s.Init(f, src, nil, nil, 0)
	var out []tok
	for {
		// Only the offset is kept here. Resolving it is what the comparison
		// tests, so it must not be resolved while scanning.
		out = append(out, tok{s.Tok, s.Lit, s.Kind, s.Op, s.Bad, Position{Line: uint(f.Offset(s.Pos))}})
		if s.Tok == _EOF {
			break
		}
		s.Next()
	}
	return f, out, s.NumErrors()
}

// scanGo scans src with go/scanner.
func scanGo(name string, src []byte) (*token.FileSet, []goTok, int) {
	fset := token.NewFileSet()
	f := fset.AddFile(name, fset.Base(), len(src))
	var s scanner.Scanner
	nerr := 0
	s.Init(f, src, func(token.Position, string) { nerr++ }, 0)
	var out []goTok
	for {
		p, tk, lit := s.Scan()
		out = append(out, goTok{p, tk, lit})
		if tk == token.EOF {
			break
		}
	}
	return fset, out, nerr
}

// compare scans src with both scanners and returns what disagrees. The token
// streams must be equal; the messages need not be.
func compare(name string, src []byte) []string {
	var bad []string
	report := func(format string, args ...any) {
		if len(bad) < 5 {
			bad = append(bad, fmt.Sprintf(format, args...))
		}
	}

	nf, nts, _ := scanNanogo(name, src)
	gfset, gts, _ := scanGo(name, src)

	n := len(nts)
	if len(gts) < n {
		n = len(gts)
	}
	for i := 0; i < n; i++ {
		nt, gt := nts[i], gts[i]
		gpos := gfset.Position(gt.pos)
		if got := mapToken(nt); got != gt.tok {
			report("token %d at %s: nanogo %s (%s), go/scanner %s", i, gpos, got, nt.tok, gt.tok)
			continue
		}
		switch gt.tok {
		case token.IDENT, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING:
			if nt.lit != gt.lit {
				report("token %d at %s: literal %q, go/scanner %q", i, gpos, nt.lit, gt.lit)
			}
		}

		// Positions. The offsets agree except for the semicolon a general
		// comment with a newline inserts: nanogo reports it at the comment,
		// go/scanner at the newline inside it.
		noff := int(nt.pos.Line)
		if noff != gpos.Offset && !(gt.tok == token.SEMICOLON && noff < len(src) && src[noff] == '/') {
			report("token %d (%s): offset %d, go/scanner %d", i, gt.tok, noff, gpos.Offset)
			continue
		}
		if noff != gpos.Offset {
			continue // the inserted semicolon above: nothing to resolve
		}
		np := nf.Position(nf.Pos(noff))
		if int(np.Line) != gpos.Line {
			report("token %d (%s) at offset %d: %s, go/token %s", i, gt.tok, noff, np, gpos)
			continue
		}
		// Filename, line and column are all compared, everywhere, including
		// under a line directive.
		//
		// This comparison used to carry two tolerances, and both were bugs in
		// pos.go rather than differences worth tolerating: a /*line*/ that
		// ended mid-line resolved its column from the start of the line, and a
		// directive with an empty filename inherited the name in force. Both
		// are fixed and both are now checked here, which is the point of
		// removing a tolerance rather than keeping it.
		if np.Filename != gpos.Filename {
			report("token %d (%s) at offset %d: %s, go/token %s", i, gt.tok, noff, np, gpos)
			continue
		}
		if int(np.Col) != gpos.Column {
			report("token %d (%s) at offset %d: column %d, go/token %d", i, gt.tok, noff, np.Col, gpos.Column)
		}
	}
	if len(nts) != len(gts) {
		report("%d tokens, go/scanner %d", len(nts), len(gts))
	}
	return bad
}

// corpusRoots returns the Go distribution trees to compare against.
//
// The corpus is the source of the Go installation that runs the test, so every
// machine that can build nanogo can run the gate. NANOGO_GOSRC replaces the
// roots. NANOGO_REQUIRE_CORPUS=1 turns a missing corpus into a failure, which
// is what a continuous integration job that promises one needs: a silent skip
// there is indistinguishable from a pass.
func corpusRoots(t *testing.T) []string {
	t.Helper()
	var roots []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return
		}
		for _, r := range roots {
			if r == dir {
				return
			}
		}
		roots = append(roots, dir)
	}
	if custom := os.Getenv("NANOGO_GOSRC"); custom != "" {
		add(custom)
	} else {
		add(filepath.Join(runtime.GOROOT(), "src"))
		// The test directory holds the errorcheck corpus of specs/004. Most of
		// those files are rejected by both scanners and are not compared, but a
		// file that only nanogo rejects is found here.
		add(filepath.Join(runtime.GOROOT(), "test"))
		if home, err := os.UserHomeDir(); err == nil {
			add(filepath.Join(home, "dev", "go.dev", "go", "src"))
		}
	}
	if len(roots) == 0 {
		if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
			t.Fatal("NANOGO_REQUIRE_CORPUS is set and no Go source tree was found")
		}
		t.Skip("no Go source tree to compare against")
	}
	return roots
}

// TestScannerCorpus is the milestone gate of specs/010: the same token stream
// and the same positions as go/scanner over the whole Go distribution.
//
// A file that either scanner rejects is not compared, because the streams after
// an error are two recoveries and not two readings of the same text. A file
// that go/scanner accepts and nanogo does not is a bug and fails the test, so
// skipping cannot hide one.
func TestScannerCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("the corpus is thousands of files")
	}
	roots := corpusRoots(t)

	walked, compared, skipped, failed, accepted := 0, 0, 0, 0, 0
	var rejected, acceptedOnly []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			walked++
			_, _, nerr := scanNanogo(path, src)
			_, _, gerr := scanGo(path, src)
			if gerr != 0 {
				// The reverse direction of the gate. A file that go/scanner
				// rejects and nanogo reads is a missing error. It is counted
				// and not asserted, because the two recoveries differ by
				// design: nanogo follows the reference compiler, which reads
				// an identifier where go/scanner stops.
				if nerr == 0 {
					accepted++
					if len(acceptedOnly) < 5 {
						acceptedOnly = append(acceptedOnly, path)
					}
				}
				skipped++
				return nil
			}
			if nerr != 0 {
				// go/scanner read the file and nanogo did not.
				if len(rejected) < 10 {
					rejected = append(rejected, path)
				}
				failed++
				return nil
			}
			compared++
			if bad := compare(path, src); len(bad) > 0 {
				failed++
				if failed < 10 {
					t.Errorf("%s:\n\t%s", path, strings.Join(bad, "\n\t"))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(rejected) > 0 {
		t.Errorf("%d files go/scanner accepts and nanogo rejects, first: %v", failed, rejected)
	}
	// A gate that compared nothing tests nothing, and it must not be reported
	// as a pass.
	if compared == 0 {
		t.Fatalf("walked %d files under %v and compared none", walked, roots)
	}
	if os.Getenv("NANOGO_GOSRC") == "" && compared < 7000 {
		t.Errorf("compared %d files, want at least 7000 under %v", compared, roots)
	}
	t.Logf("compared %d of %d files under %v, skipped %d that go/scanner rejects, %d failures",
		compared, walked, roots, skipped, failed)
	// The reverse direction holds over the whole corpus today: nanogo rejects
	// every file go/scanner rejects. Relax this to a log line if a file ever
	// divides the two recoveries in a defensible way, such as a non-ASCII
	// character in an identifier, which the reference compiler reads and
	// go/scanner does not.
	if accepted > 0 {
		t.Errorf("%d files go/scanner rejects and nanogo reads without an error, first: %v", accepted, acceptedOnly)
	}
}

// TestScannerLineDirectives checks the position round trip under //line and
// /*line*/ against go/token, which is the other implementation of the same
// rules.
func TestScannerLineDirectives(t *testing.T) {
	tests := []string{
		"package p\n//line foo.go:10\nvar x = 1\nvar y = 2\n",
		"package p\n//line foo.go:10:5\nvar x = 1\nvar y = 2\n",
		"package p\n//line /abs/foo.go:1\nvar x = 1\n",
		"package p\n//line foo.go:10\n//line bar.go:20\nvar x = 1\n",
		"package p\n\tvar x = 1 //line foo.go:10\nvar y = 2\n", // not at the start of a line
		"package p\n//line foo.go:0\nvar x = 1\n",              // line 0 is ignored
		"package p\n//line foo.go:x\nvar x = 1\n",              // not a number
		"package p\n//line foo.go:1:0\nvar x = 1\n",            // column 0 is ignored
		"package p\n//line 10\nvar x = 1\n",                    // no filename
		"package p\n//linefoo.go:10\nvar x = 1\n",              // no space: not a directive
		"package p\nvar x = 1\n//line foo.go:10",               // at the end of the file
		"package p\n//line foo.go:10\r\nvar x = 1\n",           // a line ending in \r\n
	}
	for _, src := range tests {
		if bad := compare("dir/t.go", []byte(src)); len(bad) > 0 {
			t.Errorf("%q:\n\t%s", src, strings.Join(bad, "\n\t"))
		}
	}
}

func TestScannerLineDirectiveErrors(t *testing.T) {
	tests := []struct {
		src string
		pos string
		msg string
	}{
		{"//line foo.go:x\n", "1:15", "invalid line number: x"},
		{"//line foo.go:1:x\n", "1:17", "invalid line number: x"},
		{"//line foo.go:1:0\n", "1:17", "invalid column number: 0"},
		{"//line foo.go:0\n", "1:15", "invalid line number: 0"},
	}
	for _, test := range tests {
		_, errs := scanAll(t, "t.go", test.src)
		if len(errs) != 1 {
			t.Errorf("%q: got %d errors %v, want 1", test.src, len(errs), errs)
			continue
		}
		if got := positionOf(test.src, errs[0].Pos); got != test.pos {
			t.Errorf("%q: error at %s, want %s", test.src, got, test.pos)
		}
		if !strings.Contains(errs[0].Msg, test.msg) {
			t.Errorf("%q: message %q does not contain %q", test.src, errs[0].Msg, test.msg)
		}
	}
}

// TestScannerLineDirectiveReported checks the reported position itself, which
// is what a diagnostic prints.
//
// The /*line*/ case was a known divergence and is now fixed in pos.go. A
// directive that ends mid-line asserts its column at its own offset, so the
// column of a later token on that line is measured from there. This test
// pinned 28 while pos.go counted from the start of the line; go/scanner and
// `go tool compile` both say 7, and pos.go now agrees.
//
// The //line case reports no column at all, because the directive gives none.
// That is also the reference compiler's behaviour, checked directly.
func TestScannerLineDirectiveReported(t *testing.T) {
	const src = "package p\n//line gen.go:100\nvar x = 1\n/*line other.go:7:3*/var y = 2\n"
	fset := NewFileSet()
	f := fset.AddFile("t.go", len(src))
	var s Scanner
	s.Init(f, []byte(src), func(err Error) { t.Errorf("unexpected error %v", err) }, nil, 0)
	var got []string
	for s.Tok != _EOF {
		if s.Tok == _Name {
			got = append(got, s.Lit+"@"+f.Position(s.Pos).String()+" raw "+f.RawPosition(s.Pos).String())
		}
		s.Next()
	}
	want := []string{
		"p@t.go:1:9 raw t.go:1:9",
		"x@gen.go:100 raw t.go:3:5",
		"y@other.go:7:7 raw t.go:4:26",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

func TestScannerPragmas(t *testing.T) {
	const src = "//go:build linux\n// +build linux\n\npackage p\n\n//go:noinline\nfunc f() {}\n\nvar x = 1 //go:notadirective\n"
	type call struct {
		pos   string
		blank bool
		text  string
	}
	var got []call
	fset := NewFileSet()
	f := fset.AddFile("t.go", len(src))
	var s Scanner
	s.Init(f, []byte(src), func(err Error) { t.Errorf("unexpected error %v", err) },
		func(pos Pos, blank bool, text string, current Pragma) Pragma {
			got = append(got, call{f.Position(pos).String(), blank, text})
			if current != nil {
				t.Errorf("the scanner threaded a pragma value it does not hold")
			}
			return nil
		}, 0)
	for s.Tok != _EOF {
		s.Next()
	}
	want := []call{
		{"t.go:1:3", true, "go:build linux"},
		{"t.go:2:3", true, " +build linux"},
		{"t.go:6:3", true, "go:noinline"},
		{"t.go:9:13", false, "go:notadirective"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

func TestScannerLineTable(t *testing.T) {
	// The line table is filled as newlines are consumed, wherever they are.
	const src = "package p\n\nvar s = `a\nb`\n/*\n*/\nvar t = 1\n"
	fset := NewFileSet()
	f := fset.AddFile("t.go", len(src))
	var s Scanner
	s.Init(f, []byte(src), func(err Error) { t.Errorf("unexpected error %v", err) }, nil, 0)
	var got []string
	for s.Tok != _EOF {
		if s.Tok == _Name {
			got = append(got, s.Lit+"@"+f.Position(s.Pos).String())
		}
		s.Next()
	}
	// The newlines inside the raw string and inside the general comment are
	// counted, so the last name is on line 7 and not on line 4.
	want := []string{"p@t.go:1:9", "s@t.go:3:5", "t@t.go:7:5"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

func TestScannerBOM(t *testing.T) {
	// A byte order mark is permitted as the first character and nowhere else.
	toks, errs := scanAll(t, "t.go", "\ufeffpackage p\n")
	if len(errs) != 0 {
		t.Errorf("leading BOM: unexpected errors %v", errs)
	}
	if toks[0].tok != _Package {
		t.Errorf("leading BOM: first token is %s", toks[0].tok)
	}
	_, errs = scanAll(t, "t.go", "package \ufeffp\n")
	if len(errs) != 1 || !strings.Contains(errs[0].Msg, "BOM") {
		t.Errorf("BOM in the middle: got %v, want one BOM error", errs)
	}
}
