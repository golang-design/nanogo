// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"strings"

	"golang.design/x/nanogo/syntax"
)

// pragmaFlag names one //go: directive.
//
// The set is the one specs/016-directives-and-pragmas.md tabulates, and the
// verbs are gc's. A flag is a bit rather than a name because a declaration
// carries a set of directives and the rules below are set operations: which
// directives a declaration kind accepts, and which of the ones it collected
// are outside that set.
type pragmaFlag uint32

const (
	goBuild pragmaFlag = 1 << iota
	noescape
	norace
	nosplit
	noinline
	nocheckptr
	systemstack
	nowritebarrier
	nowritebarrierrec
	yeswritebarrierrec
	cgoUnsafeArgs
	uintptrKeepAlive
	uintptrEscapes
	registerParams
)

// funcPragmas is every directive a function declaration accepts.
//
// It is every flag but [goBuild], which is a property of the file. No other
// declaration kind accepts a directive at all: a //go: comment before an
// import, a constant, a type or a variable is a misplaced directive, because
// nothing below reads one from those declarations and silently dropping it is
// the failure specs/016-directives-and-pragmas.md rule 1 exists to prevent.
const funcPragmas = noescape | norace | nosplit | noinline | nocheckptr |
	systemstack | nowritebarrier | nowritebarrierrec | yeswritebarrierrec |
	cgoUnsafeArgs | uintptrKeepAlive | uintptrEscapes | registerParams

// misplacedDirective is gc's wording, and the conformance corpus matches on
// it (specs/004-conformance.md).
const misplacedDirective = "misplaced compiler directive"

// pragmaVerb maps a directive verb to its flag, or to zero for a verb nanogo
// does not know.
//
// Zero is not an error. specs/016-directives-and-pragmas.md rule 2 says a new
// Go release adds directives and decision 10 pins nanogo to one release, so an
// unknown verb outside nanogo's own source is not something to reject. It is
// also what makes the misplacement rule safe: only a directive nanogo
// recognises can be reported as misplaced, so an unrecognised comment stays a
// comment wherever it stands.
//
// Two of gc's verbs are absent on purpose. //go:nointerface is behind the
// FieldTrack experiment, which nanogo does not enable, and //go:wasmimport and
// //go:wasmexport belong to a target nanogo has no backend for
// (specs/016-directives-and-pragmas.md).
func pragmaVerb(verb string) pragmaFlag {
	switch verb {
	case "go:build":
		return goBuild
	case "go:noescape":
		return noescape
	case "go:norace":
		return norace
	case "go:nosplit":
		return nosplit | nocheckptr // implies nocheckptr
	case "go:noinline":
		return noinline
	case "go:nocheckptr":
		return nocheckptr
	case "go:systemstack":
		return systemstack
	case "go:nowritebarrier":
		return nowritebarrier
	case "go:nowritebarrierrec":
		return nowritebarrierrec | nowritebarrier // implies nowritebarrier
	case "go:yeswritebarrierrec":
		return yeswritebarrierrec
	case "go:cgo_unsafe_args":
		return cgoUnsafeArgs | nocheckptr // implies nocheckptr
	case "go:uintptrkeepalive":
		return uintptrKeepAlive
	case "go:uintptrescapes":
		return uintptrEscapes | uintptrKeepAlive // implies uintptrkeepalive
	case "go:registerparams":
		return registerParams
	}
	return 0
}

// pragmaAt is one directive with the position it was written at.
type pragmaAt struct {
	flag pragmaFlag
	pos  syntax.Pos
}

// pragma is the set of //go: directives one declaration collected.
//
// It keeps both the union of the flags and one entry per directive. The union
// answers "is this function nosplit". The list answers "where was the
// directive that does not belong here", which is the question a diagnostic has
// to answer, and a union cannot: two directives on one declaration produce two
// messages at two lines.
//
// It is the [syntax.Pragma] the parser attaches to a declaration, so it also
// reaches ir.Func.Pragma. No pass reads it yet; specs/016 owns that gap and
// TestDirectivesAreRecordedButNotHonoured gates it.
type pragma struct {
	flag pragmaFlag
	list []pragmaAt
}

// misplaced reports every directive in p that is outside allowed.
//
// The test is on the individual directive's flag, not on the union, so a
// declaration that legitimately carries one directive is not blamed for it
// when another beside it is wrong.
func (p *pragma) misplaced(allowed pragmaFlag, report func(syntax.Error)) {
	if p == nil {
		return
	}
	for _, at := range p.list {
		if at.flag&^allowed != 0 {
			report(syntax.Error{Pos: at.pos, Msg: misplacedDirective})
		}
	}
}

// newPragmaHandler returns the handler the parser calls for every //go:
// comment, reporting through report.
//
// The parser calls it twice for two different reasons. With a text, it is
// offering a directive to accumulate. With an empty text, it is handing back
// the directives no declaration claimed, which is the parser's way of asking
// whether they were misplaced; specs/016-directives-and-pragmas.md gives that
// decision to the driver, and this is where it is taken.
//
// A directive that does not stand on a line of its own is misplaced at once.
// The parser has no way to know that `var y int //go:noinline` is wrong,
// because the comment reaches the handler with the same text either way; the
// blank flag is the scanner's record that nothing preceded the comment on its
// line, and it is the only thing that separates the two.
func newPragmaHandler(report func(syntax.Error)) syntax.PragmaHandler {
	return func(pos syntax.Pos, blank bool, text string, old syntax.Pragma) syntax.Pragma {
		p, _ := old.(*pragma)
		if text == "" {
			// Only ever called with old != nil.
			p.misplaced(0, report)
			return nil
		}
		if p == nil {
			p = new(pragma)
		}
		if !blank {
			report(syntax.Error{Pos: pos, Msg: misplacedDirective})
			return p
		}
		verb := text
		if i := strings.Index(verb, " "); i >= 0 {
			verb = verb[:i]
		}
		flag := pragmaVerb(verb)
		p.flag |= flag
		p.list = append(p.list, pragmaAt{flag: flag, pos: pos})
		return p
	}
}

// checkDirectives reports every directive attached to a declaration that does
// not accept it.
//
// The handler above catches a directive no declaration claimed. This catches
// the opposite case: a declaration claimed it and has no use for it. Both are
// needed, because the parser attaches a pending directive to whatever
// declaration follows, so `//go:noinline` before a `var` is not unclaimed. It
// is claimed by a declaration that no pass will ever read it from.
//
// The walk covers declarations inside function bodies too, which is why it is
// a walk and not a loop over f.DeclList: a `//go:noinline` before a local
// `var` is as misplaced as one before a package-level `var`.
func checkDirectives(f *syntax.File, report func(syntax.Error)) {
	// A file accepts //go:build and nothing else. A directive before the
	// package clause is attached here.
	asPragma(f.Pragma).misplaced(goBuild, report)
	syntax.Inspect(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.ImportDecl:
			asPragma(n.Pragma).misplaced(0, report)
		case *syntax.ConstDecl:
			asPragma(n.Pragma).misplaced(0, report)
		case *syntax.TypeDecl:
			asPragma(n.Pragma).misplaced(0, report)
		case *syntax.VarDecl:
			asPragma(n.Pragma).misplaced(0, report)
		case *syntax.FuncDecl:
			asPragma(n.Pragma).misplaced(funcPragmas, report)
		}
		return true
	})
}

// lifetimePragmas are the directives whose whole meaning is object lifetime.
//
// //go:uintptrkeepalive says a uintptr parameter keeps its referent alive for
// the duration of the call, and //go:uintptrescapes says it keeps it alive
// past the call as well and implies the first. Both put an obligation on the
// caller: it has to hold the pointer the uintptr was made from, and
// specs/023-escape-analysis.md owns the pass that would.
//
// No pass does, so a function that carries one is refused. The alternative is
// a program that collects an object while a system call is reading it, which
// is the outcome specs/016-directives-and-pragmas.md rule 1 names: a directive
// in the correctness group that is recorded and not honoured is not a missing
// optimisation.
//
// The declaration is what is read, so this covers the callee's own package and
// no other. A package nanogo compiles that calls an imported function carrying
// the directive is not refused, and the obligation is the caller's. specs/016
// records that hole rather than leaving it silent: the directive does not
// travel in the export data.
const lifetimePragmas = uintptrKeepAlive | uintptrEscapes

// LifetimeDirective returns the directive a declaration carries that nanogo
// records and cannot honour, and the position it is at.
//
// The name is returned rather than the flag, because the only use of the
// answer is a diagnostic and a reader needs the word they wrote.
func LifetimeDirective(p syntax.Pragma) (string, syntax.Pos, bool) {
	q := asPragma(p)
	if q == nil || q.flag&lifetimePragmas == 0 {
		return "", syntax.Pos(0), false
	}
	for _, at := range q.list {
		switch {
		case at.flag&uintptrEscapes != 0:
			return "//go:uintptrescapes", at.pos, true
		case at.flag&uintptrKeepAlive != 0:
			return "//go:uintptrkeepalive", at.pos, true
		}
	}
	return "", syntax.Pos(0), false
}

// asPragma recovers the driver's record from the parser's opaque interface.
func asPragma(p syntax.Pragma) *pragma {
	q, _ := p.(*pragma)
	return q
}
