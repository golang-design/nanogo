// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

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

// Init prepares s to scan src, which must be the whole contents of file.
//
// errh receives each error. It may be nil, in which case errors are counted and
// not reported. pragh receives each //go: directive and may be nil.
func (s *Scanner) Init(file *SrcFile, src []byte, errh ErrorHandler, pragh PragmaHandler, mode Mode) {
	panic("syntax: Scanner.Init is not implemented")
}

// Next advances to the next token.
//
// At the end of the input the token is _EOF and further calls leave it there,
// so a caller that fails to stop loops rather than reading out of range.
func (s *Scanner) Next() {
	panic("syntax: Scanner.Next is not implemented")
}

// NumErrors returns the number of errors found so far.
func (s *Scanner) NumErrors() int { return s.numErrs }
