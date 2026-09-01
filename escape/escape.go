// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package escape decides what a function lets outlive its own frame.
//
// specs/023-escape-analysis.md owns the design and the staging. This is stages
// 1 to 3 of it. Stage 1 answered one question per parameter, does the value
// the caller passed flow anywhere at all. Stage 2 widened the allowlist of
// operations the walk understands. Stage 3 added the one destination the note
// can name besides "nowhere": a result of the function itself, reached without
// a dereference.
//
// So there are three answers per parameter. "esc:" says the value flows
// nowhere. "esc:" followed by a result byte says the value flows to that
// result and to nothing else. Everything else takes [heapNote], the empty
// string, which gc reads back as a flow to the heap at zero dereferences and
// which is what nanogo wrote for every parameter before this package existed.
//
// # Why the dereference count is only ever zero
//
// gc's note carries a minimum dereference count per destination, so it can say
// "leaks to result 0 at one dereference", and it reads the count back as a
// shift on the flow into the result.
//
// Writing a count other than zero would mean matching gc's own arithmetic
// exactly, because the note ratchet is that nanogo's note is gc's own or the
// empty one. So a flow the walk measured at any depth but zero is refused
// rather than written, and an operation whose depth the walk is not sure of
// counts upwards so that the answer is a refusal. Stage 4 is what makes the
// deeper counts exact enough to write. [flow] holds the rest of the argument.
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
//
// [prover.unsafeWord] is the one switch whose default ends a flow instead of
// refusing it. Its own comment says why the inversion is sound there and
// nowhere else.
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
func note(fn *ir.Func, o *ir.Object, d Directives, proved map[*ir.Object]string) string {
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

	return proved[o]
}

// prove returns the note the walk proved for each parameter it proved one for.
//
// A parameter with no entry takes [heapNote] through the zero value of the
// map, so a walk that never ran and a walk that refused are one answer.
//
// One walk per parameter. The alternative is one walk that tracks every
// parameter at once, and it is not worth the sharing: a parameter is refused
// by a leak that belongs to it, and a shared walk has to keep the leaks apart
// anyway.
func prove(fn *ir.Func) map[*ir.Object]string {
	if fn.Bodyless {
		return nil
	}
	out := map[*ir.Object]string{}
	params := make([]*ir.Object, 0, len(fn.Params)+1)
	if fn.Recv != nil {
		params = append(params, fn.Recv)
	}
	params = append(params, fn.Params...)
	for _, o := range params {
		if o == nil || o.Name == "" || o.Name == "_" {
			continue
		}
		if s, ok := analyse(fn, o); ok {
			out[o] = s
		}
	}
	return out
}

// analyse returns the note for param and whether the walk proved one.
//
// It answers "no" unless the walk proved otherwise, so an error anywhere below
// costs an allocation in the caller and never a claim gc can act on.
func analyse(fn *ir.Func, param *ir.Object) (string, bool) {
	if param == nil || param.Type == nil {
		return "", false
	}
	if param.Escapes {
		// ir.Build marks a variable whose address the source took and a
		// variable a literal captures, and ir/lower.go moves every one of
		// them into a heap cell. So nanogo's own callee puts this
		// parameter's value in the heap whatever the body does with it, and
		// a note saying the value stayed out of the heap would be false
		// about nanogo's own code generation rather than about the source.
		return "", false
	}

	p := &prover{
		fn:      fn,
		tainted: map[*ir.Object]int{param: 0},
		results: map[int]int{},
	}

	// The taint set grows by whole objects and each object's depth only
	// falls, and a depth is a bounded non-negative integer, so the walk
	// reaches a fixed point. The bound below is the safety net for a defect
	// in that argument.
	for round := 0; ; round++ {
		if round > maxRounds {
			return "", false
		}
		p.changed = false
		p.stmts(fn.Body)
		if p.leaked {
			return "", false
		}
		if !p.changed {
			break
		}
	}

	for o, derefs := range p.tainted {
		if outlivesFrame(o) {
			return "", false
		}
		if o.Class == ir.ClassResult {
			// The value reached a named result, so it leaves with the call
			// whether or not any return statement names it.
			i := resultIndex(fn, o)
			if i < 0 {
				return "", false
			}
			p.result(i, derefs)
		}
	}
	if p.leaked {
		return "", false
	}

	var l leaks
	for i, derefs := range p.results {
		if derefs != 0 {
			// A flow the walk measured below the surface. Its count is an
			// upper bound on the true one and not the minimum gc writes, so
			// writing it would put a note in the archive that gc never wrote.
			// [flow] holds that argument in full.
			return "", false
		}
		l.AddResult(i, 0)
	}
	return l.Encode(), true
}

// outlivesFrame reports whether o's storage can be read after the call
// returns, so that a value that reached o left the frame.
func outlivesFrame(o *ir.Object) bool {
	switch {
	case o.Escapes:
		// The value reached a variable that lives in a heap cell.
		return true
	case o.Class == ir.ClassGlobal:
		// A global outlives every frame. [prover.store] refuses a store to
		// one already; this stands because the set is the last thing read and
		// a second reader of it must not have to know that.
		return true
	}
	return false
}

// resultIndex returns the position of o among fn's results, or -1.
//
// A position gc's note cannot name is -1 as well, because gc takes the heap
// flow for a result past [numResults] and this package refuses instead.
func resultIndex(fn *ir.Func, o *ir.Object) int {
	for i, r := range fn.Results {
		if r == o {
			if i >= numResults {
				return -1
			}
			return i
		}
	}
	return -1
}

// maxRounds bounds the fixed point. A body needing more rounds than this is
// refused rather than walked further, which costs an allocation.
const maxRounds = 1000

// maxDerefs saturates a dereference count.
//
// It is the largest count the note's byte can hold, and saturating rather than
// growing is what makes the lattice the fixed point walks finite. Saturation
// is also the safe direction: a count that stopped rising is still no smaller
// than the true one, and only a count of zero is ever written.
const maxDerefs = 0xff

// A flow is what the walk knows about the value of one expression.
//
// derived says the value may hold a reference the caller's argument reached.
// derefs is how many pointer indirections separate the argument from this
// value.
//
// A count is written into a note only when it is zero, so both ways of being
// wrong are safe for memory and only one of them is safe for the note. gc
// reads the count back as a shift on the flow into the result, so a count
// below the truth describes a stronger flow than the one that exists and makes
// gc's caller more careful than it needs to be, and a count above the truth
// would describe a weaker one. Neither reaches gc, because a count above zero
// is refused and a count below zero cannot arise.
//
// What a wrong count costs is the note ratchet: nanogo's note must be gc's own
// or the empty one, and a count of zero where gc measured one writes a note gc
// never wrote. So a depth the walk is not sure of is counted upwards, which
// turns it into a refusal rather than a disagreement, and every depth in
// [prover.node] that is counted downwards was read out of gc's own archive
// first (specs/023-escape-analysis.md holds the table).
type flow struct {
	derived bool
	derefs  int
}

// noRef is the value of an expression nothing derived from the parameter
// reaches.
var noRef = flow{}

// shift returns f read through k more dereferences.
func (f flow) shift(k int) flow {
	if !f.derived {
		return f
	}
	f.derefs += k
	if f.derefs > maxDerefs {
		f.derefs = maxDerefs
	}
	if f.derefs < 0 {
		// Only [prover.node]'s address-of shifts downwards, and it shifts by
		// one less than the indirections it walked past, so this is a defect
		// in that arithmetic rather than a shape a program can write. Zero is
		// the answer that describes the strongest flow.
		f.derefs = 0
	}
	return f
}

// join returns the shallower of two flows into one value.
func (f flow) join(g flow) flow {
	switch {
	case !f.derived:
		return g
	case !g.derived:
		return f
	case g.derefs < f.derefs:
		return g
	}
	return f
}

// carries drops a flow into a value whose type cannot hold a reference.
//
// A value with no pointer word cannot carry one, which is what stops len(b)
// and b[0] from tainting everything they reach. A value with no type is
// carried, because the answer is unknown.
func (f flow) carries(t *ir.Type) flow {
	if !f.derived {
		return noRef
	}
	if t == nil || t.HasPointers() {
		return f
	}
	return noRef
}

// A prover walks one function's body for one parameter.
//
// tainted maps each variable that may hold a value derived from the parameter
// to the smallest number of dereferences any such value passed through.
// results is the same for each result of the function. leaked records that a
// value reached a position this package cannot describe, and it is never
// cleared: one leak refuses the parameter.
type prover struct {
	fn      *ir.Func
	tainted map[*ir.Object]int
	results map[int]int
	leaked  bool
	changed bool
}

func (p *prover) leak() { p.leaked = true }

// taint records that o may hold a value derived from the parameter, reached
// through derefs dereferences.
func (p *prover) taint(o *ir.Object, derefs int) {
	if o == nil {
		p.leak()
		return
	}
	if old, ok := p.tainted[o]; ok && old <= derefs {
		return
	}
	p.tainted[o] = derefs
	p.changed = true
}

// result records a flow to the i'th result of the function.
func (p *prover) result(i, derefs int) {
	if i < 0 || i >= numResults {
		// gc takes the heap flow for a result its note cannot name. This
		// refuses, which is the same claim in the note it writes.
		p.leak()
		return
	}
	if old, ok := p.results[i]; ok && old <= derefs {
		return
	}
	p.results[i] = derefs
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
		if m.Obj == nil {
			return true
		}
		if _, ok := p.tainted[m.Obj]; ok {
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

// node visits one node and returns what its value carries.
//
// A statement returns [noRef]: a statement has no value. The default case is
// the whole soundness argument of this package, so it comes first in the
// reading even though it is written last: an operation not named here may do
// anything with what it is given, so a tainted variable anywhere below it is a
// leak.
func (p *prover) node(n ir.Expr) flow {
	if n == nil {
		return noRef
	}

	// A hoisted temporary lives in the node's own Init list, so every op
	// below has one to visit whether or not it names other statements.
	// [builder.guarded] is what puts them there.
	p.stmts(n.Init)

	switch n.Op {
	// Values.
	case ir.OConst:
		return noRef

	case ir.OLocal, ir.OGlobal:
		if n.Obj == nil {
			return noRef
		}
		derefs, ok := p.tainted[n.Obj]
		if !ok {
			return noRef
		}
		return flow{derived: true, derefs: derefs}

	case ir.OField:
		// A field of a value. A field of a pointer's referent is this node
		// over an [ir.ODeref], which is where that dereference is counted.
		return p.node(n.X).carries(n.Type)

	case ir.ODeref:
		return p.node(n.X).shift(1).carries(n.Type)

	case ir.OIndex:
		p.node(n.Y)
		for _, a := range n.Args {
			p.node(a)
		}
		return p.node(n.X).shift(indexDerefs(n.X)).carries(n.Type)

	case ir.OSlice:
		for _, a := range n.Args {
			p.node(a)
		}
		return p.node(n.X).shift(sliceDerefs(n.X)).carries(n.Type)

	case ir.OLen, ir.OCap, ir.ONilCheck:
		// A measurement, and a check that reads nothing out. Neither retains
		// what it is given and neither writes through it, and the value each
		// produces has no pointer word for [flow.carries] to keep.
		return p.node(n.X).carries(n.Type)

	case ir.OTypeAssert:
		// The dynamic value out of an interface. The type node in Y names a
		// type and holds no value.
		return p.node(n.X).shift(ifaceDerefs(n.Type)).carries(n.Type)

	case ir.OAddr:
		return p.addr(n)

	case ir.OUnary, ir.OBinary, ir.OCompare:
		// Arithmetic. Go has no pointer arithmetic, so an operand that
		// carries a reference can only reach an operator through unsafe, and
		// the value an operator produces is a pointer only where
		// [prover.unsafeWord] follows it.
		f := p.node(n.X).join(p.node(n.Y))
		return f.carries(n.Type)

	case ir.OConvert:
		return p.convert(n)

	case ir.OCompositeLit:
		return p.compositeLit(n)

	case ir.OCall:
		// There is no summary for a callee, so an argument that carries a
		// reference is a leak whatever the callee does with it. So is a call
		// through a value derived from the parameter, which is gc's callee
		// flow.
		if p.node(n.X).derived {
			p.leak()
		}
		for _, a := range n.Args {
			if p.node(a).derived {
				p.leak()
			}
		}
		// Nothing derived from the parameter can come back out of a call
		// that was given nothing derived from it.
		return noRef

	// Statements.
	case ir.OAssign:
		// X is the single destination and Args is the multi-value form. An
		// assignment with neither is one no builder writes, and it is refused
		// rather than read as a value that went nowhere.
		src := p.node(n.Y)
		if n.X == nil && len(n.Args) == 0 {
			if src.derived {
				p.leak()
			}
			return noRef
		}
		if n.X != nil {
			p.store(n.X, src)
		}
		for _, d := range n.Args {
			p.store(d, src)
		}
		return noRef

	case ir.OReturn:
		p.ret(n)
		return noRef

	case ir.ODeclare:
		// "var x T" writes the zero value. It reads nothing and stores
		// nothing that came from anywhere.
		return noRef

	case ir.OBlock:
		p.stmts(n.Body)
		return noRef

	case ir.OIf:
		p.node(n.X)
		p.stmts(n.Body)
		p.stmts(n.Else)
		return noRef

	case ir.OFor:
		p.node(n.X)
		p.stmts(n.Body)
		p.stmts(n.Post)
		return noRef

	case ir.ORange:
		p.rangeStmt(n)
		return noRef

	case ir.OSwitch:
		p.node(n.X)
		p.stmts(n.Body)
		return noRef

	case ir.OTypeSwitch:
		p.typeSwitch(n)
		return noRef

	case ir.OCase:
		// Reachable from OSwitch and from nothing else: [prover.typeSwitch]
		// walks its own clauses, because a clause of a type switch declares a
		// variable and a clause of an expression switch does not. An OSelect
		// carries OCase children too, and it takes the default below: a case
		// of a select sends or receives, which this pass does not understand.
		// Adding OSelect to this list without deciding what its cases do
		// would make a send of a tainted value invisible.
		for _, a := range n.Args {
			p.node(a)
		}
		p.stmts(n.Body)
		return noRef

	case ir.OLabel:
		p.stmts(n.Body)
		return noRef

	case ir.OGoto, ir.OBreak, ir.OContinue:
		return noRef

	default:
		if p.mentions(n) {
			p.leak()
		}
		return noRef
	}
}

// indexDerefs returns the dereferences x[i] reads through, given the operand x.
//
// An element of an array is inside the array's own storage and is read without
// an indirection. Everything else is reached through a pointer the operand
// holds, and an operand this pass cannot place is counted as one indirection
// rather than none, which turns the answer into a refusal instead of a note
// nobody checked.
func indexDerefs(x ir.Expr) int {
	if x != nil && x.Type != nil && x.Type.Kind == ir.Array {
		return 0
	}
	return 1
}

// sliceDerefs returns the dereferences x[lo:hi] reads through.
//
// A slice of a slice and a slice of a string share the operand's own backing
// store, so the result holds the pointer the operand held and no indirection
// happened. Both were read out of gc's archive. ir.Build takes the address of
// an array before slicing it, so an array operand arrives here as that address
// and is counted with everything else, which refuses it.
func sliceDerefs(x ir.Expr) int {
	if x == nil || x.Type == nil {
		return 1
	}
	switch x.Type.Kind {
	case ir.Slice, ir.String:
		return 0
	}
	return 1
}

// ifaceDerefs returns the dereferences reading a value of type t out of an
// interface goes through.
//
// An interface holds a pointer-shaped value in its own second word, so reading
// one out is not an indirection, and gc's note for a returned type assertion
// to a pointer says the same. Anything else is stored in a separate allocation
// the second word points at. gc calls a one-word struct holding a pointer
// pointer-shaped and this does not, which refuses that case rather than
// describing it, and ir/lower.go refuses to compile it anyway.
func ifaceDerefs(t *ir.Type) int {
	if t == nil {
		return 1
	}
	switch t.Kind {
	case ir.Ptr, ir.UnsafePtr, ir.Map, ir.Chan, ir.FuncKind:
		return 0
	}
	return 1
}

// addr visits an address-of and returns what the address carries.
//
// The three branches are the three kinds of storage an address names, and the
// first two are refusals.
//
// A variable's own storage is the -1 flow of specs/023-escape-analysis.md's
// model: the address outlives nothing but the frame, and a note that let it
// leave would hand gc a pointer into a frame that is gone. ir.Build's
// markEscapes walks to the same root through the same steps and marks it, so
// this refusal and [analyse]'s reading of ir.Object.Escapes agree; it is
// written here as well because the walk must not depend on a mark another
// package makes.
//
// Storage this pass cannot place is a composite literal, which ir/lower.go
// puts in an allocation, and every shape not named. Both are refused.
//
// Storage reached through a pointer, a slice or a map belongs to whatever that
// pointer names and not to this frame, so the address is the operand's own
// flow read through one indirection less than the chain walked past: &(*q).f
// and &s[i] hold what q and s held.
func (p *prover) addr(n ir.Expr) flow {
	// The subtree is read whatever the branch below decides, because an index
	// on the way to the storage is evaluated and a call hiding in one is
	// still a call.
	p.node(n.X)

	root, base, derefs, ok := addressed(n.X)
	if !ok || root != nil {
		if p.mentions(n.X) {
			p.leak()
		}
		return noRef
	}
	return p.node(base).shift(derefs - 1).carries(n.Type)
}

// addressed walks the operand of an address-of to the storage it names.
//
// root is the variable whose own storage the address names. base is the
// expression whose value the storage is reached through when root is nil, and
// derefs is how many indirections separate base's value from that storage. ok
// is false for an operand this pass does not place, which is every shape not
// named below.
//
// The steps are ir/build.go markEscapes's, and they stop in the same places
// for the same reason.
func addressed(n ir.Expr) (root *ir.Object, base ir.Expr, derefs int, ok bool) {
	for n != nil {
		switch n.Op {
		case ir.OLocal, ir.OGlobal:
			if n.Obj == nil {
				return nil, nil, 0, false
			}
			return n.Obj, nil, derefs, true
		case ir.OField:
			n = n.X
		case ir.ODeref:
			return nil, n.X, derefs + 1, true
		case ir.OIndex:
			if n.X == nil || n.X.Type == nil {
				return nil, nil, 0, false
			}
			switch n.X.Type.Kind {
			case ir.Array:
				n = n.X
			case ir.Slice:
				return nil, n.X, derefs + 1, true
			default:
				return nil, nil, 0, false
			}
		default:
			return nil, nil, 0, false
		}
	}
	return nil, nil, 0, false
}

// convert visits a conversion.
func (p *prover) convert(n ir.Expr) flow {
	if n.Type == nil {
		if p.mentions(n.X) {
			p.leak()
		}
		return noRef
	}
	switch {
	case n.Type.Kind == ir.Interface:
		// nanogo boxes a value into an interface on the heap, because
		// specs/023-escape-analysis.md is what would decide otherwise and it
		// is this pass. So the value is in the heap whether or not the
		// interface itself goes anywhere.
		if p.node(n.X).derived {
			p.leak()
		}
		return noRef

	case !n.Type.HasPointers():
		// A pointer converted to a word the collector does not trace. The
		// flow ends here; [prover.unsafeWord] is where a conversion back
		// picks it up again and says why that is sound.
		p.node(n.X)
		return noRef

	case n.X != nil && n.X.Type != nil && !n.X.Type.HasPointers():
		// A word with no pointer in it converted to one that has. This is
		// the second half of unsafe's rule (3).
		return p.unsafeWord(n.X).carries(n.Type)

	case copies(n):
		// A string and a slice of bytes or runes do not share storage: the
		// conversion copies the elements into an allocation of its own
		// (ir/lower.go's runtime.stringtoslicebyte and its three siblings).
		// The element of either half is a byte or a rune, so no reference
		// the caller holds is among what is copied, and nothing of the
		// operand reaches the result or the allocation.
		p.node(n.X)
		return noRef

	default:
		return p.node(n.X).shift(convertDerefs(n)).carries(n.Type)
	}
}

// copies reports whether a conversion writes its result into storage of its
// own rather than sharing the operand's.
func copies(n ir.Expr) bool {
	if n.Type == nil || n.X == nil || n.X.Type == nil {
		return false
	}
	from, to := n.X.Type.Kind, n.Type.Kind
	return (from == ir.String && to == ir.Slice) || (from == ir.Slice && to == ir.String)
}

// convertDerefs returns the dereferences a conversion reads through.
//
// A conversion reinterprets a value, so it reads through nothing and the
// answer is zero for every pair but one. [N]T(s) reads the elements out of the
// slice's backing store, which is one indirection, and (*[N]T)(s) does not:
// it keeps the pointer the slice held. gc draws the same line, in
// cmd/compile/internal/escape's OSLICE2ARR and OSLICE2ARRPTR.
func convertDerefs(n ir.Expr) int {
	if n.Type == nil || n.X == nil || n.X.Type == nil {
		return 0
	}
	if n.X.Type.Kind == ir.Slice && n.Type.Kind == ir.Array {
		return 1
	}
	return 0
}

// unsafeWord follows a value with no pointer in it back to a pointer it was
// converted from.
//
// The default here ends the flow where every other switch in this package
// refuses the parameter. The inversion is sound because the language, and not
// this analysis, is what ends the flow. unsafe's rule (3) allows a pointer to
// travel through a uintptr only when both conversions and the arithmetic
// between them stand in one expression, and it names the other form invalid:
//
//	// INVALID: uintptr cannot be stored in variable
//	//  before conversion back to Pointer.
//	u := uintptr(p)
//	p = unsafe.Pointer(u + offset)
//
// So a flow this walk stops following is a flow no valid program has, and an
// operation it does not name costs precision rather than correctness. That is
// the opposite trade from [prover.node]'s default, and it is available here
// and nowhere else because the rule that ends the flow is the language's.
//
// cmd/compile/internal/escape.(*escape).unsafeValue is the same walk over the
// same shapes, which is what keeps nanogo's note equal to gc's for a
// declaration built on the pattern. internal/abi.NoEscape is the one that
// matters: it holds the uintptr in a variable, so gc's note for it is "esc:"
// and this walk reaches the same answer.
func (p *prover) unsafeWord(n ir.Expr) flow {
	if n == nil {
		return noRef
	}
	p.stmts(n.Init)
	switch n.Op {
	case ir.OConvert:
		if n.X != nil && n.X.Type != nil && n.X.Type.HasPointers() {
			// The conversion that started the round trip.
			return p.node(n.X)
		}
		// A word converted from another word. gc discards it rather than
		// walking further, and so does this.
		p.node(n.X)
		return noRef

	case ir.OUnary, ir.OBinary:
		// The arithmetic rule (3) allows between the two conversions. A
		// shift's count is not itself an offset, and following it reaches
		// the default below, which is the answer gc writes for it too.
		return p.unsafeWord(n.X).join(p.unsafeWord(n.Y))

	default:
		// The flow ends. The node is still walked, because a call or an
		// assignment under it is one whatever its value means here.
		p.node(n)
		return noRef
	}
}

// compositeLit visits T{...}.
//
// ir/lower.go builds a struct and an array literal in the frame, one temporary
// per literal, and its reader copies the value out. So such a literal retains
// nothing on its own and the value it produces carries whatever its elements
// carry, at the depth they were read at: no indirection stands between an
// element and the value it is part of.
//
// A slice and a map literal are allocations in nanogo, whatever this pass
// decides, so an element that carries a reference reaches the heap and is
// refused. specs/023-escape-analysis.md's stage 5 is what would change that,
// because it is the stage that changes what nanogo emits.
func (p *prover) compositeLit(n ir.Expr) flow {
	var f flow
	for _, a := range n.Args {
		if a != nil && a.Op == ir.OAssign {
			// A keyed element. ir/build.go's indexedElems pairs an index
			// with a value, and ir/lower.go reads the pair the same way: the
			// left half is the index of the element and not a destination
			// anything is stored into.
			p.stmts(a.Init)
			p.node(a.X)
			f = f.join(p.node(a.Y))
			continue
		}
		f = f.join(p.node(a))
	}
	if n.Type == nil || (n.Type.Kind != ir.Struct && n.Type.Kind != ir.Array) {
		if f.derived || p.mentions(n) {
			p.leak()
		}
		return noRef
	}
	return f.carries(n.Type)
}

// ret visits a return statement.
//
// The arguments line up with the function's results by position, which is the
// alignment gc's note is read back through. A statement whose count does not
// match is the naked return, which names nothing and leaves the named results
// to [analyse]'s reading of the taint set, and "return f()" spreading one
// call over several results, whose value this pass cannot separate: an
// argument that carries a reference is refused there rather than assigned to a
// result it may not belong to.
func (p *prover) ret(n ir.Expr) {
	if len(n.Args) == len(p.fn.Results) {
		for i, a := range n.Args {
			f := p.node(a)
			if r := p.fn.Results[i]; r != nil {
				f = f.carries(r.Type)
			}
			if f.derived {
				p.result(i, f.derefs)
			}
		}
		return
	}
	for _, a := range n.Args {
		if p.node(a).derived {
			p.leak()
		}
	}
}

// rangeStmt visits a range statement.
//
// Ranging reads the operand and retains nothing of it, so the question is what
// the iteration variables receive. An element is read out of the operand's own
// storage, which is one indirection for every operand shape here: a slice, a
// string, a map and a channel each hold a pointer to the storage the value
// comes out of, and an array arrives as its address because ir/build.go takes
// one before ranging over it in place.
//
// The operand shapes are an allowlist. A range over a function is a call, and
// ir/build.go builds it as one (ir/rangefunc.go); the ORange it leaves behind
// for a shape that row does not carry would be a call this walk cannot see, so
// it is refused with everything else not named.
func (p *prover) rangeStmt(n ir.Expr) {
	f := p.node(n.X)
	if !rangeable(n.X) {
		if p.mentions(n) {
			p.leak()
		}
		return
	}
	for _, d := range n.Args {
		if d == nil {
			continue
		}
		p.stmts(d.Init)
		p.store(d, f.shift(1))
	}
	p.stmts(n.Body)
}

// rangeable reports whether ranging over x retains nothing of it.
//
// A type with no pointer word carries nothing for a range to retain, which is
// the integer of Go 1.22's rule. The named kinds are the ones whose element
// this pass can place. A function is not among them and neither is an
// interface.
func rangeable(x ir.Expr) bool {
	if x == nil || x.Type == nil {
		return false
	}
	switch x.Type.Kind {
	case ir.Slice, ir.String, ir.Map, ir.Chan, ir.Array:
		return true
	case ir.Ptr:
		return x.Type.Elem != nil && x.Type.Elem.Kind == ir.Array
	}
	return !x.Type.HasPointers()
}

// typeSwitch visits a type switch.
//
// The clause variable holds the operand's dynamic value narrowed to the
// clause's type, which is a read out of the interface and retains nothing.
// The clauses are walked here rather than through [prover.node]'s OCase,
// because that case belongs to an expression switch, which declares nothing.
func (p *prover) typeSwitch(n ir.Expr) {
	f := p.node(n.X)
	for _, c := range n.Body {
		if c == nil {
			continue
		}
		if c.Op != ir.OCase {
			// A child ir.Build does not write. Refusing it keeps a later
			// shape from being walked as a clause it is not.
			if p.mentions(c) {
				p.leak()
			}
			continue
		}
		p.stmts(c.Init)
		for _, a := range c.Args {
			p.node(a)
		}
		if c.Obj != nil {
			if g := f.shift(ifaceDerefs(c.Obj.Type)).carries(c.Obj.Type); g.derived {
				p.taint(c.Obj, g.derefs)
			}
		}
		p.stmts(c.Body)
	}
}

// store visits an assignment destination.
//
// src says what the value being assigned carries.
func (p *prover) store(dst ir.Expr, src flow) {
	if dst == nil {
		p.leak()
		return
	}
	// A destination whose type has no pointer word cannot hold a reference,
	// which is what keeps the bool of "v, ok := i.(T)" out of the taint set.
	src = src.carries(dst.Type)
	root, direct := destination(dst)
	if !direct {
		// The destination is reached through a pointer, a slice or a map, so
		// the analysis cannot say whose storage it names. Two things are
		// refused here and they are different claims. A carried value stored
		// into it may reach the heap. A destination reached through a value
		// derived from the parameter is a write through that value, which is
		// gc's mutator flow and which the notes here deny.
		if src.derived || p.mentions(dst) {
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
		if src.derived {
			p.leak()
		}
		return
	}
	if src.derived {
		p.taint(root, src.derefs)
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
