// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"go/token"
	"testing"
)

func TestTokenStrings(t *testing.T) {
	// Every token in the set must print as something, because a parser error
	// message names the token it did not expect.
	for tok := _EOF; tok < tokenCount; tok++ {
		if got := tok.String(); got == "" || got == "token(?)" {
			t.Errorf("token %d prints %q", tok, got)
		}
	}
	if got := Token(tokenCount + 10).String(); got != "token(?)" {
		t.Errorf("an out-of-range token prints %q", got)
	}
	if got := Token(0).String(); got != "token(?)" {
		t.Errorf("the zero token prints %q", got)
	}
}

func TestTokenIsKeyword(t *testing.T) {
	if !_Break.IsKeyword() || !_Var.IsKeyword() || !_Func.IsKeyword() {
		t.Error("a keyword did not report as one")
	}
	if _Name.IsKeyword() || _Semi.IsKeyword() || _EOF.IsKeyword() {
		t.Error("a non-keyword reported as one")
	}
}

// TestKeywordSetMatchesGoToken pins the keyword set against the standard
// library's. A keyword nanogo does not know would be scanned as an identifier,
// and the program would parse as something else entirely rather than fail.
func TestKeywordSetMatchesGoToken(t *testing.T) {
	for tok := token.BREAK; tok <= token.VAR; tok++ {
		if !tok.IsKeyword() {
			continue
		}
		name := tok.String()
		if got := lookup(name); got == _Name {
			t.Errorf("%q is a keyword in go/token and an identifier here", name)
		}
	}
	// And nothing extra: every keyword nanogo knows must be one go/token knows.
	for _, name := range sortedKeywords() {
		if !token.Lookup(name).IsKeyword() {
			t.Errorf("%q is a keyword here and not in go/token", name)
		}
	}
	if lookup("notakeyword") != _Name {
		t.Error("an ordinary identifier was scanned as a keyword")
	}
}

// sortedKeywords returns the keyword spellings in a fixed order.
//
// The map is ranged over here and the result is sorted before use, which is the
// discipline specs/053-determinism.md requires everywhere output depends on it.
func sortedKeywords() []string {
	names := make([]string, 0, len(keywords))
	for name := range keywords {
		names = append(names, name)
	}
	// A small insertion sort keeps the test free of imports that would hide
	// what it is doing.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

func TestOperatorStrings(t *testing.T) {
	for op := Def; op < operatorCount; op++ {
		if got := op.String(); got == "" || got == "operator(?)" {
			t.Errorf("operator %d prints %q", op, got)
		}
	}
	if got := Operator(operatorCount + 5).String(); got != "operator(?)" {
		t.Errorf("an out-of-range operator prints %q", got)
	}
	if got := Operator(0).String(); got != "operator(?)" {
		t.Errorf("the zero operator prints %q", got)
	}
}

// TestPrecedenceMatchesGoToken is the property that matters. A precedence that
// disagrees with the specification silently reassociates expressions, which
// produces a program that compiles and computes the wrong answer.
func TestPrecedenceMatchesGoToken(t *testing.T) {
	pairs := []struct {
		op  Operator
		tok token.Token
	}{
		{OrOr, token.LOR}, {AndAnd, token.LAND},
		{Eql, token.EQL}, {Neq, token.NEQ},
		{Lss, token.LSS}, {Leq, token.LEQ},
		{Gtr, token.GTR}, {Geq, token.GEQ},
		{Add, token.ADD}, {Sub, token.SUB},
		{Or, token.OR}, {Xor, token.XOR},
		{Mul, token.MUL}, {Div, token.QUO}, {Rem, token.REM},
		{And, token.AND}, {AndNot, token.AND_NOT},
		{Shl, token.SHL}, {Shr, token.SHR},
	}
	for _, p := range pairs {
		if got, want := p.op.Precedence(), p.tok.Precedence(); got != want {
			t.Errorf("%v has precedence %d here and %d in go/token", p.op, got, want)
		}
	}
	// The whole binary set is covered above; check the count so a new operator
	// cannot be added without being checked.
	if len(pairs) != int(Shr-OrOr)+1 {
		t.Errorf("the table covers %d operators, the binary range holds %d", len(pairs), int(Shr-OrOr)+1)
	}
}

func TestNonBinaryOperatorsHaveNoPrecedence(t *testing.T) {
	for _, op := range []Operator{Def, Not, Recv, Tilde} {
		if got := op.Precedence(); got != PrecLowest {
			t.Errorf("%v has precedence %d, want PrecLowest: it is not a binary operator", op, got)
		}
	}
}

func TestLitKindStrings(t *testing.T) {
	for _, tc := range []struct {
		k    LitKind
		want string
	}{
		{IntLit, "untyped int"},
		{FloatLit, "untyped float"},
		{ImagLit, "untyped imaginary"},
		{RuneLit, "untyped rune"},
		{StringLit, "untyped string"},
	} {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("LitKind(%d) prints %q, want %q", tc.k, got, tc.want)
		}
	}
	if got := LitKind(99).String(); got != "untyped ?" {
		t.Errorf("an out-of-range LitKind prints %q", got)
	}
}

// TestExportedTokenAliases guards the arrangement token.go explains: the token
// constants are unexported because they collide with the node names, and only
// the ones stored in the tree are aliased out.
func TestExportedTokenAliases(t *testing.T) {
	for _, tc := range []struct {
		got, want Token
	}{
		{Break, _Break}, {Continue, _Continue}, {Fallthrough, _Fallthrough},
		{Goto, _Goto}, {Defer, _Defer}, {Go, _Go},
	} {
		if tc.got != tc.want {
			t.Errorf("alias %v does not equal its token %v", tc.got, tc.want)
		}
	}
}
