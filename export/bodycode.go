// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"

	"golang.design/x/nanogo/export/pkgbits"
)

// The codes a function body is written with.
//
// They are cmd/compile/internal/noder/codes.go, which pkgbits does not hold
// because the container knows nothing about Go. The ordinals reach the file,
// so a value here is fixed by the release nanogo is pinned to and cannot be
// reordered.

// A StmtKind names one statement encoding.
type StmtKind int

func (c StmtKind) Marker() pkgbits.SyncMarker { return pkgbits.SyncStmt1 }
func (c StmtKind) Value() int                 { return int(c) }

// The statement encodings, in the format's order.
const (
	StmtEnd StmtKind = iota
	StmtLabel
	StmtBlock
	StmtExpr
	StmtSend
	StmtAssign
	StmtAssignOp
	StmtIncDec
	StmtBranch
	StmtCall
	StmtReturn
	StmtIf
	StmtFor
	StmtSwitch
	StmtSelect

	numStmtKinds
)

var stmtNames = [numStmtKinds]string{
	StmtEnd:      "end",
	StmtLabel:    "label",
	StmtBlock:    "block",
	StmtExpr:     "expression",
	StmtSend:     "send",
	StmtAssign:   "assignment",
	StmtAssignOp: "operation assignment",
	StmtIncDec:   "increment or decrement",
	StmtBranch:   "branch",
	StmtCall:     "go or defer",
	StmtReturn:   "return",
	StmtIf:       "if",
	StmtFor:      "for",
	StmtSwitch:   "switch",
	StmtSelect:   "select",
}

func (c StmtKind) String() string {
	if c < 0 || c >= numStmtKinds {
		return fmt.Sprintf("statement code %d", int(c))
	}
	return stmtNames[c]
}

// An ExprKind names one expression encoding.
type ExprKind int

func (c ExprKind) Marker() pkgbits.SyncMarker { return pkgbits.SyncExpr }
func (c ExprKind) Value() int                 { return int(c) }

// The expression encodings, in the format's order.
const (
	ExprConst ExprKind = iota
	ExprLocal
	ExprGlobal
	ExprCompLit
	ExprFuncLit
	ExprFieldVal
	ExprMethodVal
	ExprMethodExpr
	ExprIndex
	ExprSlice
	ExprAssert
	ExprUnaryOp
	ExprBinaryOp
	ExprCall
	ExprConvert
	ExprNew
	ExprMake
	ExprSizeof
	ExprAlignof
	ExprOffsetof
	ExprZero
	ExprFuncInst
	ExprRecv
	ExprReshape
	ExprRuntimeBuiltin

	numExprKinds
)

var exprNames = [numExprKinds]string{
	ExprConst:          "constant",
	ExprLocal:          "local variable",
	ExprGlobal:         "package-scope declaration",
	ExprCompLit:        "composite literal",
	ExprFuncLit:        "function literal",
	ExprFieldVal:       "field selection",
	ExprMethodVal:      "method value",
	ExprMethodExpr:     "method expression",
	ExprIndex:          "index",
	ExprSlice:          "slice",
	ExprAssert:         "type assertion",
	ExprUnaryOp:        "unary operation",
	ExprBinaryOp:       "binary operation",
	ExprCall:           "call",
	ExprConvert:        "conversion",
	ExprNew:            "new",
	ExprMake:           "make",
	ExprSizeof:         "Sizeof",
	ExprAlignof:        "Alignof",
	ExprOffsetof:       "Offsetof",
	ExprZero:           "zero value",
	ExprFuncInst:       "function instantiation",
	ExprRecv:           "method receiver",
	ExprReshape:        "reshape",
	ExprRuntimeBuiltin: "runtime helper",
}

func (c ExprKind) String() string {
	if c < 0 || c >= numExprKinds {
		return fmt.Sprintf("expression code %d", int(c))
	}
	return exprNames[c]
}

// An AssignKind names one assignment destination encoding.
type AssignKind int

func (c AssignKind) Marker() pkgbits.SyncMarker { return pkgbits.SyncAssign }
func (c AssignKind) Value() int                 { return int(c) }

// The assignment destination encodings, in the format's order.
const (
	AssignBlank AssignKind = iota
	AssignDef
	AssignExpr

	numAssignKinds
)

func (c AssignKind) String() string {
	switch c {
	case AssignBlank:
		return "blank"
	case AssignDef:
		return "definition"
	case AssignExpr:
		return "expression"
	}
	return fmt.Sprintf("assignment code %d", int(c))
}

// An Op is the operator a body carries on an operation, a branch, or a
// deferred or concurrent call.
//
// The value is gc's ir.Op ordinal, because that is what reaches the file:
// noder/writer.go's op writes int(op) and nothing translates it. The ordinals
// are dense over gc's whole node set, and only the ones below can appear in a
// body, so an ordinal that is not one of them is a stream this reader refuses
// rather than a node it guesses at.
type Op int

// The operators a body can carry. The names are gc's without its O prefix,
// and the values are gc's ir.Op ordinals on the pinned release.
const (
	OpAdd      Op = 6   // x + y
	OpSub      Op = 7   // x - y
	OpOr       Op = 8   // x | y
	OpXor      Op = 9   // x ^ y
	OpAddr     Op = 11  // &x
	OpAndAnd   Op = 12  // x && y
	OpEq       Op = 57  // x == y
	OpNe       Op = 58  // x != y
	OpLt       Op = 59  // x < y
	OpLe       Op = 60  // x <= y
	OpGe       Op = 61  // x >= y
	OpGt       Op = 62  // x > y
	OpDeref    Op = 63  // *x
	OpMul      Op = 74  // x * y
	OpDiv      Op = 75  // x / y
	OpMod      Op = 76  // x % y
	OpLsh      Op = 77  // x << y
	OpRsh      Op = 78  // x >> y
	OpAnd      Op = 79  // x & y
	OpAndNot   Op = 80  // x &^ y
	OpNot      Op = 82  // !x
	OpBitNot   Op = 83  // ^x
	OpPlus     Op = 84  // +x
	OpNeg      Op = 85  // -x
	OpOrOr     Op = 86  // x || y
	OpRecv     Op = 100 // <-x
	OpBreak    Op = 116 // break
	OpContinue Op = 118 // continue
	OpDefer    Op = 119 // defer
	OpFall     Op = 120 // fallthrough
	OpGoto     Op = 122 // goto
	OpGo       Op = 125 // go
)

// opNames names every operator a body can carry. A lookup that misses is a
// stream the reader refuses by number.
var opNames = map[Op]string{
	OpAdd:      "+",
	OpSub:      "-",
	OpOr:       "|",
	OpXor:      "^",
	OpAddr:     "&",
	OpAndAnd:   "&&",
	OpEq:       "==",
	OpNe:       "!=",
	OpLt:       "<",
	OpLe:       "<=",
	OpGe:       ">=",
	OpGt:       ">",
	OpDeref:    "*",
	OpMul:      "*",
	OpDiv:      "/",
	OpMod:      "%",
	OpLsh:      "<<",
	OpRsh:      ">>",
	OpAnd:      "&",
	OpAndNot:   "&^",
	OpNot:      "!",
	OpBitNot:   "^",
	OpPlus:     "+",
	OpNeg:      "-",
	OpOrOr:     "||",
	OpRecv:     "<-",
	OpBreak:    "break",
	OpContinue: "continue",
	OpDefer:    "defer",
	OpFall:     "fallthrough",
	OpGoto:     "goto",
	OpGo:       "go",
}

func (op Op) String() string {
	if s, ok := opNames[op]; ok {
		return s
	}
	return fmt.Sprintf("operator %d", int(op))
}

// known reports whether op is one a body can carry.
func (op Op) known() bool { _, ok := opNames[op]; return ok }
