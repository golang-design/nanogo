// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Scanner turns source bytes into tokens.
//
// It reads the whole file from a byte slice rather than an io.Reader. Files are
// read whole, the corpus is on disk, and the simplification is worth more than
// the memory.
//
// See specs/010-scanner-and-positions.md.
type Scanner struct {
	// Current token. Valid after Init and after each Next.
	Pos  Pos      // position of the first byte of the token
	Tok  Token    // the token
	Lit  string   // literal text, for _Name and _Literal
	Bad  bool     // the literal is malformed and an error was reported
	Kind LitKind  // literal kind, for _Literal
	Op   Operator // operator identity, for _Operator, _AssignOp and _IncOp
	Prec int      // binary precedence of Op, or PrecLowest

	file  *SrcFile
	src   []byte
	errh  ErrorHandler
	pragh PragmaHandler
	mode  Mode

	// Scanning state.
	off     int  // offset of the next byte to read
	tokOff  int  // offset of the first byte of the current token
	nlsemi  bool // a newline now inserts a semicolon
	blank   bool // the current line is blank before the current position
	numErrs int
}

// eof is the value ch returns past the end of the input. It is not a byte, so
// no input byte can be mistaken for it.
const eof = -1

// bom is the byte order mark. It is permitted as the first character of a file
// and nowhere else.
const bom = 0xfeff

// Init prepares s to scan src, which must be the whole contents of file.
//
// errh receives each error. It may be nil, in which case errors are counted and
// not reported. pragh receives each //go: directive and may be nil.
//
// Init makes the first token current, so the fields above are valid as soon as
// it returns and the parser reads a token before it calls Next. Init therefore
// scans the comments at the head of the file and routes their directives.
func (s *Scanner) Init(file *SrcFile, src []byte, errh ErrorHandler, pragh PragmaHandler, mode Mode) {
	s.Pos = file.Pos(0)
	s.Tok = _EOF
	s.Lit = ""
	s.Bad = false
	s.Kind = 0
	s.Op = 0
	s.Prec = PrecLowest

	s.file = file
	s.src = src
	s.errh = errh
	s.pragh = pragh
	s.mode = mode

	s.off = 0
	s.tokOff = 0
	s.nlsemi = false
	s.blank = true
	s.numErrs = 0

	// A byte order mark is a permitted first character and carries no meaning,
	// so it is skipped here rather than tested for in the scanning loop.
	if len(src) >= 3 && src[0] == 0xef && src[1] == 0xbb && src[2] == 0xbf {
		s.off = 3
	}

	s.Next()
}

// NumErrors returns the number of errors found so far.
func (s *Scanner) NumErrors() int { return s.numErrs }

// errorAt reports an error at a byte offset and counts it.
//
// Every error goes through here or through errorfAt, because the scanner
// reports and continues and the count is what tells the driver that the parse
// is not trustworthy. The two are separate so that vet can tell which one takes
// a format string; a message that is already built must not be reformatted.
func (s *Scanner) errorAt(off int, msg string) {
	s.numErrs++
	if s.errh != nil {
		s.errh(Error{Pos: s.file.Pos(off), Msg: msg})
	}
}

// errorfAt reports a formatted error at a byte offset and counts it.
func (s *Scanner) errorfAt(off int, format string, args ...any) {
	s.numErrs++
	errorf(s.errh, s.file.Pos(off), format, args...)
}

// ch returns the byte at the read offset, or eof at the end of the input.
//
// It is a byte and not a rune. Every decision the scanner makes on a single
// character is a decision on an ASCII one, so the ASCII test comes first and
// the decoder runs only where a multibyte rune is possible. This is the hottest
// loop in the front end.
func (s *Scanner) ch() int {
	if s.off < len(s.src) {
		return int(s.src[s.off])
	}
	return eof
}

// chAt returns the byte n positions after the read offset, or eof.
func (s *Scanner) chAt(n int) int {
	if s.off+n < len(s.src) {
		return int(s.src[s.off+n])
	}
	return eof
}

// nextch consumes one byte.
//
// It is the only place that grows the line table, so every path that consumes
// bytes must come through here or a file with newlines inside a raw string or a
// comment reports every later position on the wrong line.
func (s *Scanner) nextch() {
	if s.off < len(s.src) {
		if s.src[s.off] == '\n' {
			s.file.AddLine(s.off + 1)
		}
		s.off++
	}
}

// skipCh consumes one character of literal or comment text.
//
// The Go specification requires source to be UTF-8 text, so a NUL byte, an
// invalid encoding and a byte order mark are errors. They are reported and
// consumed rather than returned, which keeps the token stream after the error
// comparable with a scanner that saw valid text.
func (s *Scanner) skipCh() {
	c := s.src[s.off]
	if c < utf8.RuneSelf {
		if c == 0 {
			s.errorAt(s.off, "invalid NUL character")
		}
		s.nextch()
		return
	}
	r, w := utf8.DecodeRune(s.src[s.off:])
	switch {
	case r == utf8.RuneError && w == 1:
		s.errorAt(s.off, "invalid UTF-8 encoding")
	case r == bom:
		s.errorAt(s.off, "invalid BOM in the middle of the file")
	}
	s.off += w
}

func lower(c int) int       { return ('a' - 'A') | c } // ASCII letters only
func isLetter(c int) bool   { return 'a' <= lower(c) && lower(c) <= 'z' || c == '_' }
func isDecimal(c int) bool  { return '0' <= c && c <= '9' }
func isHex(c int) bool      { return isDecimal(c) || 'a' <= lower(c) && lower(c) <= 'f' }
func isNotASCII(c int) bool { return c >= utf8.RuneSelf }

// setLit records a literal token.
func (s *Scanner) setLit(kind LitKind, ok bool) {
	s.nlsemi = true
	s.Tok = _Literal
	s.Lit = string(s.src[s.tokOff:s.off])
	s.Bad = !ok
	s.Kind = kind
}

// Next advances to the next token.
//
// At the end of the input the token is _EOF and further calls leave it there,
// so a caller that fails to stop loops rather than reading out of range.
func (s *Scanner) Next() {
	nlsemi := s.nlsemi
	s.nlsemi = false

	// Every field is cleared rather than left stale, so that the value of one
	// never depends on which token came before it. The reference leaves them
	// stale and its _Arrow case carries the operator of the previous token.
	s.Lit = ""
	s.Bad = false
	s.Kind = 0
	s.Op = 0
	s.Prec = PrecLowest

redo:
	// Skip white space. A newline is white space only when the previous token
	// cannot end a statement. Otherwise it is the semicolon that the Go
	// specification inserts there, and the switch below returns it.
	nl := false
	startOff := s.off
	for s.off < len(s.src) {
		c := int(s.src[s.off])
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' && !nlsemi {
			nl = nl || c == '\n'
			s.nextch()
			continue
		}
		break
	}

	s.tokOff = s.off
	s.Pos = s.file.Pos(s.off)
	// A directive is blank when nothing but white space precedes it on its
	// line. The white space just skipped crossed a line, or the previous token
	// ended at the start of one.
	s.blank = nl || startOff == 0 || s.src[startOff-1] == '\n'

	c := s.ch()
	if isLetter(c) {
		s.nextch()
		s.ident()
		return
	}
	if isNotASCII(c) {
		// A rune outside ASCII is an identifier character or an error inside an
		// identifier, never anything else, so the identifier path owns it.
		if w, ok := s.atIdentChar(true); ok {
			s.off += w
			s.ident()
			return
		}
	}

	switch c {
	case eof:
		if nlsemi {
			s.Lit = "EOF"
			s.Tok = _Semi
			break
		}
		s.Tok = _EOF

	case '\n':
		s.nextch()
		s.Lit = "newline"
		s.Tok = _Semi

	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		s.number(false)

	case '"':
		s.stdString()

	case '`':
		s.rawString()

	case '\'':
		s.rune()

	case '(':
		s.nextch()
		s.Tok = _Lparen

	case '[':
		s.nextch()
		s.Tok = _Lbrack

	case '{':
		s.nextch()
		s.Tok = _Lbrace

	case ',':
		s.nextch()
		s.Tok = _Comma

	case ';':
		s.nextch()
		s.Lit = "semicolon"
		s.Tok = _Semi

	case ')':
		s.nextch()
		s.nlsemi = true
		s.Tok = _Rparen

	case ']':
		s.nextch()
		s.nlsemi = true
		s.Tok = _Rbrack

	case '}':
		s.nextch()
		s.nlsemi = true
		s.Tok = _Rbrace

	case ':':
		s.nextch()
		if s.ch() == '=' {
			s.nextch()
			s.Op = Def
			s.Tok = _Define
			break
		}
		s.Tok = _Colon

	case '.':
		s.nextch()
		if isDecimal(s.ch()) {
			s.number(true)
			break
		}
		if s.ch() == '.' && s.chAt(1) == '.' {
			s.nextch()
			s.nextch()
			s.Tok = _DotDotDot
			break
		}
		s.Tok = _Dot

	case '+':
		s.nextch()
		s.Op, s.Prec = Add, PrecAdd
		if s.ch() != '+' {
			goto assignop
		}
		s.nextch()
		s.nlsemi = true
		s.Tok = _IncOp

	case '-':
		s.nextch()
		s.Op, s.Prec = Sub, PrecAdd
		if s.ch() != '-' {
			goto assignop
		}
		s.nextch()
		s.nlsemi = true
		s.Tok = _IncOp

	case '*':
		s.nextch()
		s.Op, s.Prec = Mul, PrecMul
		// Not the assignop path: a lone '*' is _Star, because the parser must
		// tell a pointer type and a dereference from a multiplication.
		if s.ch() == '=' {
			s.nextch()
			s.Tok = _AssignOp
			break
		}
		s.Tok = _Star

	case '/':
		s.nextch()
		if s.ch() == '/' {
			s.nextch()
			s.lineComment()
			goto redo
		}
		if s.ch() == '*' {
			s.nextch()
			if s.fullComment() && nlsemi {
				// A general comment that holds a newline is a newline for the
				// semicolon rule. One that does not is white space.
				s.Lit = "newline"
				s.Tok = _Semi
				break
			}
			goto redo
		}
		s.Op, s.Prec = Div, PrecMul
		goto assignop

	case '%':
		s.nextch()
		s.Op, s.Prec = Rem, PrecMul
		goto assignop

	case '&':
		s.nextch()
		if s.ch() == '&' {
			s.nextch()
			s.Op, s.Prec = AndAnd, PrecAndAnd
			s.Tok = _Operator
			break
		}
		s.Op, s.Prec = And, PrecMul
		if s.ch() == '^' {
			s.nextch()
			s.Op = AndNot
		}
		goto assignop

	case '|':
		s.nextch()
		if s.ch() == '|' {
			s.nextch()
			s.Op, s.Prec = OrOr, PrecOrOr
			s.Tok = _Operator
			break
		}
		s.Op, s.Prec = Or, PrecAdd
		goto assignop

	case '^':
		s.nextch()
		s.Op, s.Prec = Xor, PrecAdd
		goto assignop

	case '<':
		s.nextch()
		if s.ch() == '=' {
			s.nextch()
			s.Op, s.Prec = Leq, PrecCmp
			s.Tok = _Operator
			break
		}
		if s.ch() == '<' {
			s.nextch()
			s.Op, s.Prec = Shl, PrecMul
			goto assignop
		}
		if s.ch() == '-' {
			s.nextch()
			s.Op, s.Prec = Recv, PrecLowest
			s.Tok = _Arrow
			break
		}
		s.Op, s.Prec = Lss, PrecCmp
		s.Tok = _Operator

	case '>':
		s.nextch()
		if s.ch() == '=' {
			s.nextch()
			s.Op, s.Prec = Geq, PrecCmp
			s.Tok = _Operator
			break
		}
		if s.ch() == '>' {
			s.nextch()
			s.Op, s.Prec = Shr, PrecMul
			goto assignop
		}
		s.Op, s.Prec = Gtr, PrecCmp
		s.Tok = _Operator

	case '=':
		s.nextch()
		if s.ch() == '=' {
			s.nextch()
			s.Op, s.Prec = Eql, PrecCmp
			s.Tok = _Operator
			break
		}
		s.Tok = _Assign

	case '!':
		s.nextch()
		if s.ch() == '=' {
			s.nextch()
			s.Op, s.Prec = Neq, PrecCmp
			s.Tok = _Operator
			break
		}
		s.Op, s.Prec = Not, PrecLowest
		s.Tok = _Operator

	case '~':
		s.nextch()
		s.Op, s.Prec = Tilde, PrecLowest
		s.Tok = _Operator

	default:
		if c == 0 {
			s.errorAt(s.off, "invalid NUL character")
		} else {
			s.errorfAt(s.off, "invalid character %#U", rune(c))
		}
		s.nextch()
		goto redo
	}

	return

assignop:
	if s.ch() == '=' {
		s.nextch()
		s.Tok = _AssignOp
		return
	}
	s.Tok = _Operator
}

// atIdentChar reports whether the rune at the read offset belongs to an
// identifier and returns its width.
//
// A rune outside ASCII that is not a letter or a digit is reported here rather
// than treated as an unknown character, so that one misspelled identifier
// produces one error instead of one per rune.
func (s *Scanner) atIdentChar(first bool) (int, bool) {
	r, w := utf8.DecodeRune(s.src[s.off:])
	switch {
	case unicode.IsLetter(r) || r == '_':
		// ok
	case unicode.IsDigit(r):
		if first {
			s.errorfAt(s.off, "identifier cannot begin with digit %#U", r)
		}
	case r == utf8.RuneError && w == 1:
		s.errorAt(s.off, "invalid UTF-8 encoding")
	case r == bom:
		s.errorAt(s.off, "invalid BOM in the middle of the file")
	case r >= utf8.RuneSelf:
		s.errorfAt(s.off, "invalid character %#U in identifier", r)
	default:
		return 0, false
	}
	return w, true
}

// ident scans the rest of an identifier or keyword.
func (s *Scanner) ident() {
	// Accelerate the common case of 7-bit ASCII.
	for s.off < len(s.src) {
		c := int(s.src[s.off])
		if isLetter(c) || isDecimal(c) {
			s.off++
			continue
		}
		if isNotASCII(c) {
			w, ok := s.atIdentChar(false)
			if !ok {
				break
			}
			s.off += w
			continue
		}
		break
	}

	lit := string(s.src[s.tokOff:s.off])
	if len(lit) >= 2 {
		if tok := lookup(lit); tok != _Name {
			// Only four keywords can end a statement. The Go specification
			// lists them for the semicolon rule.
			s.nlsemi = tok == _Break || tok == _Continue || tok == _Fallthrough || tok == _Return
			s.Tok = tok
			return
		}
	}

	s.nlsemi = true
	s.Lit = lit
	s.Tok = _Name
}

// digits accepts the sequence { digit | '_' }.
//
// If base is 10 or less it accepts any decimal digit but records the offset of
// the first digit that the base does not allow in *invalid, so that "0b12"
// reports the '2' rather than the literal. It returns bit 0 set when a digit
// was seen and bit 1 set when a separator was seen.
func (s *Scanner) digits(base int, invalid *int) (digsep int) {
	if base <= 10 {
		max := '0' + base
		for isDecimal(s.ch()) || s.ch() == '_' {
			ds := 1
			if s.ch() == '_' {
				ds = 2
			} else if s.ch() >= max && *invalid < 0 {
				*invalid = s.off
			}
			digsep |= ds
			s.nextch()
		}
	} else {
		for isHex(s.ch()) || s.ch() == '_' {
			ds := 1
			if s.ch() == '_' {
				ds = 2
			}
			digsep |= ds
			s.nextch()
		}
	}
	return
}

// number scans an integer, floating-point or imaginary literal. seenPoint is
// set when the leading '.' was already consumed.
func (s *Scanner) number(seenPoint bool) {
	ok := true
	kind := IntLit
	base := 10    // number base
	prefix := 0   // one of 0 (decimal), '0' (0-octal), 'x', 'o' or 'b'
	digsep := 0   // bit 0: digit present, bit 1: '_' present
	invalid := -1 // offset of the first digit the base does not allow

	// Integer part.
	if !seenPoint {
		if s.ch() == '0' {
			s.nextch()
			switch lower(s.ch()) {
			case 'x':
				s.nextch()
				base, prefix = 16, 'x'
			case 'o':
				s.nextch()
				base, prefix = 8, 'o'
			case 'b':
				s.nextch()
				base, prefix = 2, 'b'
			default:
				base, prefix = 8, '0'
				digsep = 1 // the leading 0 is a digit
			}
		}
		digsep |= s.digits(base, &invalid)
		if s.ch() == '.' {
			if prefix == 'o' || prefix == 'b' {
				s.errorfAt(s.off, "invalid radix point in %s literal", baseName(base))
				ok = false
			}
			s.nextch()
			seenPoint = true
		}
	}

	// Fractional part.
	if seenPoint {
		kind = FloatLit
		digsep |= s.digits(base, &invalid)
	}

	if digsep&1 == 0 && ok {
		s.errorfAt(s.off, "%s literal has no digits", baseName(base))
		ok = false
	}

	// Exponent.
	if e := lower(s.ch()); e == 'e' || e == 'p' {
		if ok {
			switch {
			case e == 'e' && prefix != 0 && prefix != '0':
				s.errorfAt(s.off, "%q exponent requires decimal mantissa", rune(s.ch()))
				ok = false
			case e == 'p' && prefix != 'x':
				s.errorfAt(s.off, "%q exponent requires hexadecimal mantissa", rune(s.ch()))
				ok = false
			}
		}
		s.nextch()
		kind = FloatLit
		if s.ch() == '+' || s.ch() == '-' {
			s.nextch()
		}
		digsep = s.digits(10, nil) | digsep&2 // keep the separator bit
		if digsep&1 == 0 && ok {
			s.errorAt(s.off, "exponent has no digits")
			ok = false
		}
	} else if prefix == 'x' && kind == FloatLit && ok {
		// The Go specification makes the 'p' exponent mandatory, because a
		// hexadecimal mantissa alone has no unambiguous value.
		s.errorAt(s.off, "hexadecimal mantissa requires a 'p' exponent")
		ok = false
	}

	// The imaginary suffix is legal on every numeric form.
	if s.ch() == 'i' {
		kind = ImagLit
		s.nextch()
	}

	s.setLit(kind, ok) // set now, so the checks below can read s.Lit

	if kind == IntLit && invalid >= 0 && ok {
		s.errorfAt(invalid, "invalid digit %q in %s literal", rune(s.src[invalid]), baseName(base))
		ok = false
	}

	if digsep&2 != 0 && ok {
		// The separator is reported by the scanner and not by the parser, so
		// that the position is the separator and not the literal.
		if i := invalidSep(s.Lit); i >= 0 {
			s.errorAt(s.tokOff+i, "'_' must separate successive digits")
			ok = false
		}
	}

	s.Bad = !ok // correct the value setLit recorded
}

func baseName(base int) string {
	switch base {
	case 2:
		return "binary"
	case 8:
		return "octal"
	case 10:
		return "decimal"
	case 16:
		return "hexadecimal"
	}
	return "invalid"
}

// invalidSep returns the index of the first '_' in x that does not separate two
// digits, or -1. A separator is legal between digits only, so a leading, a
// trailing and a doubled one are all errors.
func invalidSep(x string) int {
	x1 := ' ' // prefix character; only 'x' matters
	d := '.'  // previous character class: '_', '0' for a digit, '.' for anything else
	i := 0

	// A base prefix counts as a digit.
	if len(x) >= 2 && x[0] == '0' {
		x1 = rune(lower(int(x[1])))
		if x1 == 'x' || x1 == 'o' || x1 == 'b' {
			d = '0'
			i = 2
		}
	}

	// Mantissa and exponent.
	for ; i < len(x); i++ {
		p := d // previous character
		d = rune(x[i])
		switch {
		case d == '_':
			if p != '0' {
				return i
			}
		case isDecimal(int(d)) || x1 == 'x' && isHex(int(d)):
			d = '0'
		default:
			if p == '_' {
				return i - 1
			}
			d = '.'
		}
	}
	if d == '_' {
		return len(x) - 1
	}

	return -1
}

// rune scans a rune literal.
func (s *Scanner) rune() {
	ok := true
	s.nextch() // consume the opening quote

	n := 0
	for ; ; n++ {
		c := s.ch()
		if c == '\'' {
			if ok {
				if n == 0 {
					s.errorAt(s.off, "empty rune literal or unescaped '")
					ok = false
				} else if n != 1 {
					s.errorAt(s.tokOff, "more than one character in rune literal")
					ok = false
				}
			}
			s.nextch()
			break
		}
		if c == '\\' {
			s.nextch()
			if !s.escape('\'') {
				ok = false
			}
			continue
		}
		if c == '\n' {
			if ok {
				s.errorAt(s.off, "newline in rune literal")
				ok = false
			}
			break
		}
		if c == eof {
			if ok {
				s.errorAt(s.tokOff, "rune literal not terminated")
				ok = false
			}
			break
		}
		s.skipCh()
	}

	s.setLit(RuneLit, ok)
}

// stdString scans an interpreted string literal.
func (s *Scanner) stdString() {
	ok := true
	s.nextch() // consume the opening quote

	for {
		c := s.ch()
		if c == '"' {
			s.nextch()
			break
		}
		if c == '\\' {
			s.nextch()
			if !s.escape('"') {
				ok = false
			}
			continue
		}
		if c == '\n' {
			s.errorAt(s.off, "newline in string")
			ok = false
			break
		}
		if c == eof {
			s.errorAt(s.tokOff, "string not terminated")
			ok = false
			break
		}
		s.skipCh()
	}

	s.setLit(StringLit, ok)
}

// rawString scans a raw string literal.
func (s *Scanner) rawString() {
	ok := true
	cr := false
	s.nextch() // consume the opening quote

	for {
		c := s.ch()
		if c == '`' {
			s.nextch()
			break
		}
		if c == eof {
			s.errorAt(s.tokOff, "string not terminated")
			ok = false
			break
		}
		cr = cr || c == '\r'
		s.skipCh()
	}

	s.setLit(StringLit, ok)
	if cr {
		// A carriage return is not part of the value of a raw string, so it is
		// dropped here and the literal text is no longer a slice of the source.
		s.Lit = stripCR(s.Lit)
	}
}

// stripCR removes every carriage return from a raw string literal.
func stripCR(lit string) string {
	b := make([]byte, 0, len(lit))
	for i := 0; i < len(lit); i++ {
		if lit[i] != '\r' {
			b = append(b, lit[i])
		}
	}
	return string(b)
}

// escape scans the rest of an escape sequence and reports whether it is valid.
// The opening backslash is already consumed.
func (s *Scanner) escape(quote byte) bool {
	var n int
	var base, max uint32

	c := s.ch()
	switch {
	case c == int(quote) || c == 'a' || c == 'b' || c == 'f' || c == 'n' || c == 'r' || c == 't' || c == 'v' || c == '\\':
		s.nextch()
		return true
	case '0' <= c && c <= '7':
		n, base, max = 3, 8, 255
	case c == 'x':
		s.nextch()
		n, base, max = 2, 16, 255
	case c == 'u':
		s.nextch()
		n, base, max = 4, 16, unicode.MaxRune
	case c == 'U':
		s.nextch()
		n, base, max = 8, 16, unicode.MaxRune
	default:
		if c == eof {
			return true // the caller reports the unterminated literal
		}
		s.errorAt(s.off, "unknown escape")
		return false
	}

	var x uint32
	for i := n; i > 0; i-- {
		c := s.ch()
		if c == eof {
			return true // the caller reports the unterminated literal
		}
		d := base
		if isDecimal(c) {
			d = uint32(c - '0')
		} else if 'a' <= lower(c) && lower(c) <= 'f' {
			d = uint32(lower(c)-'a') + 10
		}
		if d >= base {
			r, _ := utf8.DecodeRune(s.src[s.off:])
			s.errorfAt(s.off, "invalid character %q in %s escape", r, baseName(int(base)))
			return false
		}
		x = x*base + d
		s.nextch()
	}

	if x > max && base == 8 {
		s.errorfAt(s.tokOff, "octal escape value %d > 255", x)
		return false
	}

	// A surrogate half has no UTF-8 encoding and a value above the maximum rune
	// has none either, so the Go specification rejects both.
	if x > max || 0xD800 <= x && x < 0xE000 {
		s.errorfAt(s.tokOff, "escape is invalid Unicode code point %#U", rune(x))
		return false
	}

	return true
}

// lineComment scans the rest of a // comment and routes a directive. The
// opening // is already consumed.
func (s *Scanner) lineComment() {
	for s.off < len(s.src) && s.src[s.off] != '\n' {
		s.skipCh()
	}

	// The offset the comment governs is the start of the next line, which is
	// what a //line directive renumbers.
	next := s.off
	if next < len(s.src) {
		next++
	}

	text := string(s.src[s.tokOff:s.off])
	// A line may end in "\r\n". The carriage return is not part of the
	// directive text, and the reference compiler drops it before looking.
	if strings.HasSuffix(text, "\r") {
		text = text[:len(text)-1]
	}
	body := text[2:] // after the //

	switch {
	case strings.HasPrefix(body, "line ") && s.atLineStart(s.tokOff):
		// The Go specification requires a //line directive to start a line.
		// A /*line*/ directive may stand anywhere.
		s.lineDirective(body[len("line "):], s.tokOff+2+len("line "), next)
	case strings.HasPrefix(body, "go:"), strings.HasPrefix(body, " +build"):
		// specs/014 consumes the build constraints and specs/016 the rest. The
		// scanner only hands them over, because the parser is what knows the
		// declaration a pragma binds to.
		s.pragma(body)
	}
}

// fullComment scans the rest of a /* */ comment and reports whether it holds a
// newline. The opening /* is already consumed.
func (s *Scanner) fullComment() bool {
	nl := false
	terminated := false
	for s.off < len(s.src) {
		if s.src[s.off] == '\n' {
			nl = true
		}
		if s.src[s.off] == '*' && s.chAt(1) == '/' {
			s.nextch()
			s.nextch()
			terminated = true
			break
		}
		s.skipCh()
	}
	if !terminated {
		s.errorAt(s.tokOff, "comment not terminated")
		return nl
	}

	text := string(s.src[s.tokOff:s.off])
	body := text[2 : len(text)-2] // between the /* and the */
	if strings.HasPrefix(body, "line ") {
		s.lineDirective(body[len("line "):], s.tokOff+2+len("line "), s.off)
	}
	return nl
}

// atLineStart reports whether offset is the first byte of a line.
func (s *Scanner) atLineStart(off int) bool {
	return off == 0 || s.src[off-1] == '\n'
}

// pragma hands a directive comment to the pragma handler.
//
// The current pragma is not threaded through, because the scanner holds no
// pragma state: the parser accumulates directives and binds them to the next
// declaration.
func (s *Scanner) pragma(text string) {
	if s.pragh == nil {
		return
	}
	// The position is the first byte after the //, which is where the
	// reference front end points a directive diagnostic.
	s.pragh(s.file.Pos(s.tokOff+2), s.blank, text, nil)
}

// maxLineCol caps a directive's line and column. The cap leaves room below the
// int32 that a position ends up in, and it is the reference compiler's cap.
const maxLineCol = 1 << 30

// lineDirective parses the text of a //line or /*line*/ directive.
//
// text is what follows "line ", textOff is its offset in the file, and next is
// the offset of the first byte the directive governs.
func (s *Scanner) lineDirective(text string, textOff, next int) {
	i, n, ok := trailingDigits(text)
	if i == 0 {
		return // not a directive after all
	}

	if !ok {
		// A suffix that is not a number is a mistyped directive and not a
		// filename, because a filename cannot be reached from here.
		s.errorAt(textOff+int(i), "invalid line number: "+text[i:])
		return
	}

	var line, col uint
	i2, n2, ok2 := trailingDigits(text[:i-1])
	if ok2 {
		// file:line:col
		i, i2 = i2, i
		line, col = n2, n
		if col == 0 || col > maxLineCol {
			s.errorAt(textOff+int(i2), "invalid column number: "+text[i2:])
			return
		}
		text = text[:i2-1]
	} else {
		// file:line
		line = n
	}

	if line == 0 || line > maxLineCol {
		s.errorAt(textOff+int(i), "invalid line number: "+text[i:])
		return
	}

	// An empty filename means two different things depending on the form, and
	// the difference is not a detail. go/scanner states the rule directly: "If
	// we have a column (//line filename:line:col form), an empty filename means
	// to use the previous filename."
	//
	//	//line :20        -> the filename is empty. `go tool compile` reports
	//	                     `:20`, with no file, and so does go/scanner.
	//	/*line :20:1*/    -> the filename in force continues. The Go
	//	                     distribution's own test/fixedbugs/issue24339.go
	//	                     depends on this, and reads as the containing file.
	//
	// Resolving it here rather than in the position model is deliberate: only
	// the scanner knows whether a column was written.
	name := text[:i-1]
	switch {
	case name == "" && ok2:
		name = s.file.Position(s.file.Pos(textOff)).Filename
	case name != "":
		// A relative name is relative to the directory of the file that holds
		// the directive, which is what the reference compiler and go/scanner
		// both do. See go.dev/issue/26671.
		name = filepath.Clean(name)
		if !filepath.IsAbs(name) {
			if dir := filepath.Dir(s.file.Name()); dir != "." {
				name = filepath.Join(dir, name)
			}
		}
	}
	if next >= len(s.src) {
		// The directive governs no byte. go/token drops it as well, and
		// keeping it would renumber the end of file position alone.
		return
	}
	s.file.AddLineDirective(next, name, line, col)
}

// trailingDigits returns the index just after the last ':' in text, the number
// that follows it, and whether that number parsed.
func trailingDigits(text string) (uint, uint, bool) {
	// The search runs from the right because a Windows filename may hold a ':'.
	i := strings.LastIndexByte(text, ':')
	if i < 0 {
		return 0, 0, false
	}
	n, err := strconv.ParseUint(text[i+1:], 10, 0)
	return uint(i + 1), uint(n), err == nil
}
