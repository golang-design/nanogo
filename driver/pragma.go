// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"strings"

	"golang.design/x/nanogo/escape"
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

// pragmaAt is one directive with the verb as written and the position it was
// written at.
//
// The verb is kept rather than recovered from the flag. [pragmaVerb] folds
// implications into the flag it returns, so //go:nosplit and //go:nocheckptr
// are one bit apart and //go:nowritebarrierrec cannot be told from
// //go:nowritebarrier by the flag alone. A diagnostic has to print the word
// the author wrote, and a rule that fires on one verb and not on the verb it
// implies has to test the verb.
//
// It is recorded for a verb [pragmaVerb] does not know as well, which is what
// lets [sourceDirectives] see //go:linkname without giving it a flag: giving
// it one would make it misplaced everywhere but a function, and gc accepts it
// anywhere in a file.
type pragmaAt struct {
	flag pragmaFlag
	verb string
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
// reaches ir.Func.Pragma. Two passes read it there: [NoSplit], which the back
// end honours, and [LifetimeDirective], which is a refusal. Every other
// directive is recorded and not read, and specs/016 owns that gap.
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
func newPragmaHandler(report func(syntax.Error), seen *sourceDirectives) syntax.PragmaHandler {
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
		p.list = append(p.list, pragmaAt{flag: flag, verb: verb, pos: pos})
		seen.record(verb, text, pos)
		return p
	}
}

// abiDirectives are the directives that decide which ABI a symbol is defined
// under, with what each one does to that decision.
//
// specs/047-abi-wrappers.md reads the decision out of ssagen.GenABIWrappers.
// //go:cgo_export_static and //go:cgo_export_dynamic tell the linker to export
// the definition ABI to C, and gc then suppresses every wrapper for the symbol
// and fails the build if one was owed anyway.
//
// nanogo models neither. A package that writes one is refused by name rather
// than decided wrongly: the failure of a wrong answer here is a symbol defined
// twice or a reference to an ABI nothing defines, and both surface as a link
// error naming neither the directive nor the function.
//
// //go:linkname was in this set until stage 2 and is not any more. It is
// modelled in [linkname.go] instead, because it holds four of the eight
// packages the assembly refusal covers and because the model it needs is the
// name the def and ref lines are matched against, which is a rule and not a
// gap.
var abiDirectives = map[string]string{
	"go:cgo_export_static": "the cgo export pragmas carry the definition ABI to the linker and suppress " +
		"every ABI wrapper, and cgo is out of scope (specs/000-decisions.md decision 8)",
	"go:cgo_export_dynamic": "the cgo export pragmas carry the definition ABI to the linker and suppress " +
		"every ABI wrapper, and cgo is out of scope (specs/000-decisions.md decision 8)",
}

// sourceDirectives is the record of directives the package wrote that no
// declaration is asked about.
//
// [checkDirectives] reads a declaration's own directives off the declaration,
// which is where every rule in specs/016-directives-and-pragmas.md wants
// them. The [abiDirectives] are the ones the assembly gate needs and the ones
// a declaration cannot answer for, because gc accepts them anywhere in a file
// and applies them by name: the //go:linkname form that gives a body away
// stands before the function, and the form that takes one is often written on
// its own beside the import of unsafe.
//
// The handler is the only thing that sees every //go: comment, claimed or
// not, so this is filled there.
type sourceDirectives struct {
	// ABI is the first directive the package wrote that decides which ABI a
	// symbol is defined under, Reason is what it does to that decision, and
	// Pos is where it is. Empty when the package wrote none.
	ABI    string
	Reason string
	Pos    syntax.Pos

	// Linknames is every //go:linkname and //go:linknamestd the package
	// wrote, in source order, undecoded. [ParseLinkname] decodes one and
	// [Linknames.bind] resolves the local names against the declarations.
	//
	// The list is kept in file order and never in map order, because the
	// refusals below name one directive and a message that named a different
	// one between two runs over one input would not be a diagnostic
	// (specs/053-determinism.md).
	Linknames []LinknameText
}

// LinknameText is one //go:linkname or //go:linknamestd directive as written.
type LinknameText struct {
	Verb string     // "go:linkname" or "go:linknamestd"
	Text string     // the whole directive, verb included
	Pos  syntax.Pos // where it was written
}

// record notes a directive the parser handed the handler.
//
// The first [abiDirectives] entry wins, because the refusal names one position
// and a reader acts on the first occurrence. A linkname is kept in full,
// because its arguments are the decision and not only its presence.
func (d *sourceDirectives) record(verb, text string, pos syntax.Pos) {
	if d == nil {
		return
	}
	switch verb {
	case "go:linkname", "go:linknamestd":
		d.Linknames = append(d.Linknames, LinknameText{Verb: verb, Text: text, Pos: pos})
	}
	if d.ABI != "" {
		return
	}
	if reason, ok := abiDirectives[verb]; ok {
		d.ABI, d.Reason, d.Pos = "//"+verb, reason, pos
	}
}

// runtimeDirectives are the directives whose whole meaning is a rule nanogo
// does not implement, with the spec that owns the rule.
//
// The set is read only for a package gc would apply the runtime rules to, and
// the caller is what applies that scope. //go:nosplit is written outside the
// runtime as well, and specs/047-abi-wrappers.md's gate is deliberately the
// narrower one: a wider refusal would take out packages that compile today for
// a rule that is not reachable in them, and the wide form belongs to
// specs/035-goroutines-and-stack-growth.md. //go:systemstack and the three
// write-barrier directives are refused by gc's own noder outside a runtime
// package, so for those the scope costs nothing.
//
// //go:nosplit says the function runs where the stack cannot grow.
// specs/035-goroutines-and-stack-growth.md records that nanogo emits the
// stack-growth check anyway, and specs/047-abi-wrappers.md measured which
// packages that reaches: five of the seven non-runtime packages the assembly
// refusal covers carry the directive, so lifting the assembly refusal makes
// this gap reachable for the first time. A function with the check in it that
// is called from a context where the stack may not grow throws "morestack on
// g0" at run time, which is loud and far from its cause.
//
// //go:systemstack says the function runs only on the system stack, and the
// three write-barrier directives say what the function may and may not do to
// a pointer field. specs/034-write-barriers.md owns all four and nanogo emits
// no barrier at all, so a function that says it may not have one is
// indistinguishable from one that says nothing.
//
// The verbs are matched rather than the flags, because [pragmaVerb] folds
// //go:nowritebarrierrec into //go:nowritebarrier and //go:nosplit into
// //go:nocheckptr, and //go:nocheckptr on its own is not in this set: nanogo
// emits no pointer checks, so a function that asks for none gets what it
// asked for.
// //go:nosplit is not in this set. It is honoured rather than recorded:
// ssagen emits no stack growth check in such a function, the object carries
// obj.SymFlagNoSplit, and cmd/link computes the budget over the call graph and
// fails the link on an overflow. Refusing it here after building all of that
// would refuse a directive nanogo obeys.
var runtimeDirectives = map[string]string{
	"go:systemstack":        "specs/034-write-barriers.md",
	"go:nowritebarrier":     "specs/034-write-barriers.md",
	"go:nowritebarrierrec":  "specs/034-write-barriers.md",
	"go:yeswritebarrierrec": "specs/034-write-barriers.md",
}

// RuntimeDirective returns the runtime-only directive a declaration carries
// that nanogo records and cannot honour, the spec that owns the gap, and the
// position it is at.
//
// It is the per-function half of the runtime gate. specs/047-abi-wrappers.md
// takes the decision apart: a blanket refusal of objabi.runtimePkgs would
// refuse thirteen packages nanogo compiles today, and the property that
// actually decides a program's meaning is per directive and per function.
// This is that property, and [Config.RuntimeRules] is where the caller asks
// whether the package is one gc would apply the runtime rules to.
//
// The name is returned rather than the flag, for the reason
// [LifetimeDirective] gives: the only use of the answer is a diagnostic and a
// reader needs the word they wrote.
func RuntimeDirective(p syntax.Pragma) (verb, spec string, pos syntax.Pos, ok bool) {
	q := asPragma(p)
	if q == nil {
		return "", "", syntax.Pos(0), false
	}
	for _, at := range q.list {
		if s, found := runtimeDirectives[at.verb]; found {
			return "//" + at.verb, s, at.pos, true
		}
	}
	return "", "", syntax.Pos(0), false
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

// cgoUnsafeArgsDirective returns the position of //go:cgo_unsafe_args on a
// declaration, if it carries one.
//
// The directive pins the function to ABI0 whatever the symabis file says, and
// gc propagates the linkname attribute onto the ABI0 symbol with it. It
// exists because the callee walks its arguments by offset, so the argument
// area's layout is part of the contract rather than an implementation detail.
// specs/047-abi-wrappers.md refuses it by name for that reason.
//
// It is asked for separately from [RuntimeDirective] because it is not a
// runtime directive: a package outside the runtime writes it, and the reason
// it is refused is the ABI and not the stack.
func cgoUnsafeArgsDirective(p syntax.Pragma) (syntax.Pos, bool) {
	q := asPragma(p)
	if q == nil {
		return syntax.Pos(0), false
	}
	for _, at := range q.list {
		if at.verb == "go:cgo_unsafe_args" {
			return at.pos, true
		}
	}
	return syntax.Pos(0), false
}

// NoSplit reports whether a declaration carries //go:nosplit.
//
// The directive says the function must have no stack-growth check in it,
// because it runs where the call to runtime.morestack is not allowed. The
// back end reads the answer through ssagen.Options.NoSplit and emits no
// check, and it sets the object file's SymFlagNoSplit so cmd/link adds the
// function to the budget it computes over the call graph
// (specs/035-goroutines-and-stack-growth.md).
//
// The declaration is what is read, so this covers the callee's own package
// and no other. gc does the same: the directive is a property of the
// definition and the caller needs no knowledge of it.
func NoSplit(p syntax.Pragma) bool {
	q := asPragma(p)
	return q != nil && q.flag&nosplit != 0
}

// asPragma recovers the driver's record from the parser's opaque interface.
func asPragma(p syntax.Pragma) *pragma {
	q, _ := p.(*pragma)
	return q
}

// EscapeDirectives returns the directives that decide a parameter's escape
// analysis note (specs/023-escape-analysis.md).
//
// //go:noescape is an assertion about a declaration with no body, and the two
// uintptr directives are an obligation on the caller that nanogo refuses to
// compile. Both answers belong to the parser's record, which is why the
// package that writes the note asks for them here rather than reading
// ir.Func.Pragma itself.
func EscapeDirectives(p syntax.Pragma) escape.Directives {
	q := asPragma(p)
	if q == nil {
		return escape.Directives{}
	}
	return escape.Directives{
		Noescape:       q.flag&noescape != 0,
		UintptrEscapes: q.flag&lifetimePragmas != 0,
	}
}
