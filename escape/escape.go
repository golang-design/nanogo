// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package escape decides what a function lets outlive its own frame.
//
// specs/023-escape-analysis.md owns the design and the staging. This is stage
// 1 of it, and stage 1 answers one question per parameter: does the value the
// caller passed flow anywhere at all. There are two answers and no third. The
// note "esc:" says the value flows nowhere, and it is written only where the
// walk below proved it. Every other case takes [heapNote], the empty string,
// which gc reads back as a flow to the heap at zero dereferences and which is
// what nanogo wrote for every parameter before this package existed.
//
// # Why the answers are only two
//
// gc's note carries a minimum dereference count per destination, so it can say
// "leaks to result 0 at one dereference". Saying that needs the flow graph of
// specs/023-escape-analysis.md, which is stage 3. Until then a flow this
// package can see but cannot measure is a flow it refuses to describe, and
// refusing to describe a flow is the empty note.
//
// # Why a refusal is safe and a wrong answer is not
//
// The note travels to gc in the export data and gc compiles the caller against
// it. A note that says a parameter does not escape tells gc it may leave the
// caller's local in the caller's frame. Three passes of gc act on it: the
// runtime rule that refuses a heap escape outright, the walk that lets
// []byte(s) share a string's bytes when nothing mutates them, and the slice
// pass that keeps an append's backing store in the frame. So a note this
// package cannot prove is not a missing optimisation in nanogo's own output.
// It is a caller gc compiled around a claim that is false.
//
// That is why every switch below is an allowlist. An operation the analysis
// does not name is not assumed harmless: a value derived from the parameter
// that reaches one is recorded as a leak, and the parameter takes the empty
// note. Adding an operation is what makes the analysis see more, and
// forgetting one costs an allocation.
package escape

import "golang.design/x/nanogo/ir"

// Directives is the part of a declaration's //go: directives that decides a
// note.
//
// The driver fills it, because the directives are the parser's record
// (specs/016-directives-and-pragmas.md) and their flags are private to the
// driver. ir.Func carries the record and not the flags.
type Directives struct {
	// Noescape is //go:noescape. On a declaration with no body it asserts
	// that no parameter leaks, and gc believes it without proof.
	Noescape bool

	// UintptrEscapes is //go:uintptrescapes or //go:uintptrkeepalive. Both
	// make a uintptr parameter an obligation on the caller, and nanogo
	// refuses to compile a function that carries either
	// (driver.LifetimeDirective), so every note of such a function is the
	// empty one here.
	UintptrEscapes bool
}

// Params returns one note per receiver and parameter of fn, receiver first.
//
// That is the order gc reads them in: cmd/compile/internal/noder's funcExt
// walks types.Type.RecvParams, which puts the receiver of a method before its
// declared parameters, and cmd/compile/internal/escape's batch.finish writes
// them in the same walk. A note in the wrong position is the one failure of
// this package that is not a refusal, so the caller checks the length against
// the signature it is about to write.
//
// The branch order below is cmd/compile/internal/escape.(*batch).paramTag's,
// so that the two can be read side by side.
func Params(fn *ir.Func, d Directives) []string {
	if fn == nil {
		return nil
	}
	out := make([]string, 0, len(fn.Params)+1)
	proved := prove(fn)
	if fn.Recv != nil {
		out = append(out, note(fn, fn.Recv, d, proved))
	}
	for _, p := range fn.Params {
		out = append(out, note(fn, p, d, proved))
	}
	return out
}

// note returns the note for one parameter.
func note(fn *ir.Func, o *ir.Object, d Directives, proved map[*ir.Object]bool) string {
	if o == nil || o.Type == nil {
		return heapNote
	}

	if fn.Bodyless {
		// gc's first branch. A declaration with no body is satisfied by
		// assembly or by a //go:linkname, so there is nothing to analyse and
		// the answer comes from the author.
		if o.Type.Kind == ir.Uintptr {
			// gc assumes a uintptr argument of a bodyless declaration is a
			// pointer the caller must hold live, and writes the empty note
			// so that the caller keeps it. specs/016 records that nanogo
			// does not carry the //go:uintptrkeepalive this implies.
			return heapNote
		}
		if !o.Type.HasPointers() {
			return heapNote
		}
		if d.Noescape {
			// The pragma is an assertion the compiler cannot check, and gc
			// honours it without proof for the same reason: syscall and the
			// runtime need it. The bytes are gc's own, and they are a
			// mutator flow and a callee flow at zero dereferences rather
			// than "esc:", which claims less than "no flow anywhere" does.
			var l leaks
			l.AddMutator(0)
			l.AddCallee(0)
			return l.Encode()
		}
		return heapNote
	}

	if d.UintptrEscapes {
		// gc keeps analysing the parameters that are not uintptr. nanogo
		// refuses to compile the function at all, so this is the answer that
		// costs nothing and cannot be wrong.
		return heapNote
	}

	if !o.Type.HasPointers() {
		// gc does not tag a scalar, and the empty note it writes is read back
		// as a heap flow that no scalar can carry. Writing the same thing
		// keeps the bytes identical to gc's for every scalar parameter.
		return heapNote
	}

	if o.Name == "" || o.Name == "_" {
		// The body cannot name the parameter, so nothing can flow out of it.
		// This is gc's own branch and it needs no analysis.
		var l leaks
		return l.Encode()
	}

	if proved[o] {
		var l leaks
		return l.Encode()
	}
	return heapNote
}

// prove returns the parameters whose value flows nowhere.
//
// One walk per parameter. The alternative is one walk that tracks every
// parameter at once, and it is not worth the sharing: a parameter is refused
// by a leak that belongs to it, and a shared walk has to keep the leaks apart
// anyway.
func prove(fn *ir.Func) map[*ir.Object]bool {
	if fn.Bodyless {
		return nil
	}
	out := map[*ir.Object]bool{}
	params := make([]*ir.Object, 0, len(fn.Params)+1)
	if fn.Recv != nil {
		params = append(params, fn.Recv)
	}
	params = append(params, fn.Params...)
	for _, o := range params {
		if o == nil || o.Name == "" || o.Name == "_" {
			continue
		}
		if !escapes(fn, o) {
			out[o] = true
		}
	}
	return out
}

// escapes reports whether anything derived from param can outlive the call.
//
// The answer is true unless the walk proved otherwise, so an error anywhere
// below costs an allocation in the caller and never a claim gc can act on.
func escapes(fn *ir.Func, param *ir.Object) bool {
	if param == nil || param.Type == nil {
		return true
	}
	if param.Escapes {
		// ir.Build marks a variable whose address the source took and a
		// variable a literal captures, and ir/lower.go moves every one of
		// them into a heap cell. So nanogo's own callee puts this
		// parameter's value in the heap whatever the body does with it, and
		// a note saying the value flows nowhere would be false about
		// nanogo's own code generation rather than about the source.
		return true
	}

	p := &prover{tainted: map[*ir.Object]bool{param: true}}

	// The taint set only grows, and it grows by whole objects, so the number
	// of rounds is bounded by the number of objects the body names. The
	// bound below is the safety net for a defect in that argument.
	for round := 0; ; round++ {
		if round > maxRounds {
			return true
		}
		p.changed = false
		p.stmts(fn.Body)
		if p.leaked {
			return true
		}
		if !p.changed {
			break
		}
	}

	for o := range p.tainted {
		switch {
		case o.Escapes:
			// The value reached a variable that lives in a heap cell.
			return true
		case o.Class == ir.ClassResult:
			// The value reached a named result, so it leaves with the call.
			return true
		case o.Class == ir.ClassGlobal:
			// A global outlives every frame. [prover.store] refuses this
			// already; the case stands because the set is the last thing
			// read and a second reader of it must not have to know that.
			return true
		}
	}
	return false
}

// maxRounds bounds the fixed point. A body naming more objects than this is
// refused rather than walked further, which costs an allocation.
const maxRounds = 1000

// A prover walks one function's body for one parameter.
//
// tainted holds every variable that may hold a value derived from the
// parameter. leaked records that such a value reached a position this package
// cannot prove harmless, and it is never cleared: one leak refuses the
// parameter.
type prover struct {
	tainted map[*ir.Object]bool
	leaked  bool
	changed bool
}

func (p *prover) leak() { p.leaked = true }

func (p *prover) taint(o *ir.Object) {
	if o == nil {
		p.leak()
		return
	}
	if p.tainted[o] {
		return
	}
	p.tainted[o] = true
	p.changed = true
}

// mentions reports whether any tainted variable appears anywhere below n.
//
// It is the answer for an operation the analysis does not understand: such an
// operation may store, mutate through, or call anything it is given, so a
// tainted variable under it is a leak. ir.Walk visits X, Y, Args, Init, Body,
// Post and Else, which is every field a node carries.
func (p *prover) mentions(n ir.Expr) bool {
	found := false
	ir.Walk(n, func(m *ir.Node) bool {
		if found {
			return false
		}
		if m.Obj != nil && p.tainted[m.Obj] {
			found = true
			return false
		}
		return true
	})
	return found
}

func (p *prover) stmts(list []ir.Stmt) {
	for _, s := range list {
		p.node(s)
	}
}

// carries reports whether a value the analysis derived from a tainted variable
// can hold a reference.
//
// A value whose type has no pointer word cannot carry one, which is what stops
// len(b) and b[0] from tainting everything they reach. A node with no type is
// carried, because the answer is unknown.
func carries(derived bool, t *ir.Type) bool {
	if !derived {
		return false
	}
	return t == nil || t.HasPointers()
}

// node visits one node and reports whether its value may carry a reference
// derived from the parameter.
//
// A statement returns false: a statement has no value. The default case is the
// whole soundness argument of this package, so it comes first in the reading
// even though it is written last: an operation not named here may do anything
// with what it is given, so a tainted variable anywhere below it is a leak.
func (p *prover) node(n ir.Expr) bool {
	if n == nil {
		return false
	}

	// A hoisted temporary lives in the node's own Init list, so every op
	// below has one to visit whether or not it names other statements.
	// [builder.guarded] is what puts them there.
	p.stmts(n.Init)

	switch n.Op {
	// Values.
	case ir.OConst:
		return false

	case ir.OLocal, ir.OGlobal:
		return n.Obj != nil && p.tainted[n.Obj]

	case ir.OField, ir.ODeref, ir.OIndex, ir.OSlice, ir.OLen, ir.OCap, ir.ONilCheck:
		// A read through a value. Reading retains nothing and writes
		// through nothing, so the only question is whether what was read can
		// itself carry a reference.
		derived := p.node(n.X)
		if p.node(n.Y) {
			derived = true
		}
		for _, a := range n.Args {
			if p.node(a) {
				derived = true
			}
		}
		return carries(derived, n.Type)

	case ir.OAddr:
		// The address of anything derived from the parameter names storage
		// the caller owns, whatever the type of the value at it. &b[0] is the
		// case that makes this ask about the whole subtree rather than about
		// the value: b[0] is a byte and carries nothing, and its address is a
		// pointer into the caller's slice.
		p.node(n.X)
		return p.mentions(n.X)

	case ir.OUnary, ir.OBinary, ir.OCompare:
		derived := p.node(n.X)
		if p.node(n.Y) {
			derived = true
		}
		return carries(derived, n.Type)

	case ir.OConvert:
		if !p.node(n.X) {
			return false
		}
		if n.Type == nil || n.Type.Kind == ir.Interface {
			// nanogo boxes a value into an interface on the heap, because
			// specs/023-escape-analysis.md is what would decide otherwise
			// and it is this pass. So the value is in the heap whether or
			// not the interface itself goes anywhere.
			p.leak()
			return true
		}
		if !n.Type.HasPointers() {
			// A pointer converted to uintptr. The reference survives in a
			// word the collector does not trace, so a later flow of that
			// word is a flow the analysis cannot follow. gc has the same
			// hole and closes it with //go:uintptrescapes; this refuses
			// instead.
			p.leak()
			return true
		}
		return true

	case ir.OCall:
		// Stage 1 has no summary for a callee, so an argument that carries a
		// reference is a leak whatever the callee does with it. So is a call
		// through a value derived from the parameter, which is gc's callee
		// flow.
		if p.node(n.X) {
			p.leak()
		}
		for _, a := range n.Args {
			if p.node(a) {
				p.leak()
			}
		}
		// Nothing derived from the parameter can come back out of a call
		// that was given nothing derived from it.
		return false

	// Statements.
	case ir.OAssign:
		// X is the single destination and Args is the multi-value form. An
		// assignment with neither is one no builder writes, and it is refused
		// rather than read as a value that went nowhere.
		src := p.node(n.Y)
		if n.X == nil && len(n.Args) == 0 {
			if src {
				p.leak()
			}
			return false
		}
		if n.X != nil {
			p.store(n.X, src)
		}
		for _, d := range n.Args {
			p.store(d, src)
		}
		return false

	case ir.OReturn:
		for _, a := range n.Args {
			if p.node(a) {
				p.leak()
			}
		}
		return false

	case ir.ODeclare:
		// "var x T" writes the zero value. It reads nothing and stores
		// nothing that came from anywhere.
		return false

	case ir.OBlock:
		p.stmts(n.Body)
		return false

	case ir.OIf:
		p.node(n.X)
		p.stmts(n.Body)
		p.stmts(n.Else)
		return false

	case ir.OFor:
		p.node(n.X)
		p.stmts(n.Body)
		p.stmts(n.Post)
		return false

	case ir.OSwitch:
		p.node(n.X)
		p.stmts(n.Body)
		return false

	case ir.OCase:
		// Reachable from OSwitch and from nothing else. An OSelect carries
		// OCase children too, and it takes the default below: a case of a
		// select sends or receives, which this pass does not understand.
		// Adding OSelect to this list without deciding what its cases do
		// would make a send of a tainted value invisible.
		for _, a := range n.Args {
			p.node(a)
		}
		p.stmts(n.Body)
		return false

	case ir.OLabel:
		p.stmts(n.Body)
		return false

	case ir.OGoto, ir.OBreak, ir.OContinue:
		return false

	default:
		if p.mentions(n) {
			p.leak()
		}
		return true
	}
}

// store visits an assignment destination.
//
// src says whether the value being assigned carries a reference derived from
// the parameter.
func (p *prover) store(dst ir.Expr, src bool) {
	if dst == nil {
		p.leak()
		return
	}
	root, direct := destination(dst)
	if !direct {
		// The destination is reached through a pointer, a slice or a map, so
		// the analysis cannot say whose storage it names. Two things are
		// refused here and they are different claims. A carried value stored
		// into it may reach the heap. A destination reached through a value
		// derived from the parameter is a write through that value, which is
		// gc's mutator flow and which the note "esc:" denies.
		if src || p.mentions(dst) {
			p.leak()
		}
		return
	}
	// The index expressions on the way to the destination are read, not
	// written, and a call hiding in one of them is still a call.
	for n := dst; n != nil; {
		switch n.Op {
		case ir.OField:
			n = n.X
		case ir.OIndex:
			p.node(n.Y)
			n = n.X
		default:
			n = nil
		}
	}
	if root == nil {
		p.leak()
		return
	}
	if root.Class == ir.ClassGlobal {
		if src {
			p.leak()
		}
		return
	}
	if src {
		p.taint(root)
	}
}

// destination walks an assignment destination to the storage it names.
//
// direct is true when every step stays inside one variable's own storage, so
// that a value stored there dies with that variable. A field of a struct and
// an element of an array are such steps. A dereference is not, and neither is
// an element of a slice or a map, whose storage is somewhere else and is
// reached through a pointer this analysis does not track.
//
// This is the walk ir/build.go's markEscapes makes, and it stops in the same
// places for the same reason.
func destination(n ir.Expr) (root *ir.Object, direct bool) {
	for n != nil {
		switch n.Op {
		case ir.OLocal, ir.OGlobal:
			return n.Obj, true
		case ir.OField:
			n = n.X
		case ir.OIndex:
			if n.X == nil || n.X.Type == nil || n.X.Type.Kind != ir.Array {
				return nil, false
			}
			n = n.X
		default:
			return nil, false
		}
	}
	return nil, false
}
