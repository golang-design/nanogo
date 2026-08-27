// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"fmt"
	"strconv"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/syntax"
)

// Construction from the typed tree.
//
// The phi placement algorithm is the on-the-fly one of Braun, Buchwald,
// Hack, Leissa, Bolz and Zwinkau, "Simple and Efficient Construction of Static
// Single Assignment Form" (CC 2013). specs/021-ssa-construction.md chooses it
// over the dominance frontiers of Cytron et al. (1991) and gives the reason: it
// needs no dominator tree, so nothing has to be computed before construction
// can finish. The tree is computed once afterwards, in dom.go.
//
// The algorithm is four functions. writeVariable and readVariable keep a
// per-block map from variable to current value. A read in a block with no
// definition recurses into the predecessors. A read in a block whose
// predecessors are not all known yet leaves an incomplete phi, which is filled
// when the block is sealed. A phi whose arguments are all itself or all one
// other value is redundant and is removed as soon as it is complete.
//
// Memory is a variable like any other, which is what makes the second half of
// specs/021-ssa-construction.md's memory section true without any code: a block
// that merges control flow merges memory with an ordinary phi, because memory
// goes through readVariable exactly as an integer local does.

// Build returns the SSA form of fn.
//
// The input is the typed tree after the IR passes. Every Go-specific
// construct of specs/020-ir.md's lowering table must be gone already; one that
// is not is an error naming InvGoSpecific, not a silent lowering here.
func Build(fn *ir.Func) (*Func, error) {
	if fn == nil {
		return nil, &Error{Func: "?", Detail: "nil function"}
	}
	b := &builder{
		fn:           fn,
		f:            NewFunc(fn.Name),
		frame:        make(map[*ir.Object]bool),
		labels:       make(map[string]*Block),
		labelDefined: make(map[string]bool),
		ptrTypes:     make(map[*ir.Type]*ir.Type),
		memVar:       &ir.Object{Name: "mem", Type: MemType, Class: ir.ClassLocal},
		descs:        make(map[string]*ir.Object),
		boolType:     &ir.Type{Kind: ir.Bool, Size: 1, Align: 1, Name: "bool"},
		intType:      &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"},
	}
	b.f.Sym = fn.Sym
	b.f.Type = fn.Type
	b.f.Pos = fn.Pos
	b.f.NeedCtxt = fn.Closure != nil
	b.f.Wrapper = fn.Wrapper

	b.classify()
	b.entry()
	b.stmts(fn.Body)
	b.finish()

	if b.err != nil {
		return nil, b.err
	}
	return b.f, nil
}

type builder struct {
	fn *ir.Func
	f  *Func

	// cur is the block statements are added to, or nil where control cannot
	// reach: after a return, a goto, a break or a continue.
	cur *Block

	err *Error

	// frame holds the objects that do not live in a value. It is looked up by
	// key and never ranged over; Func.Frame carries the same set in
	// declaration order for anything that needs to walk it.
	frame map[*ir.Object]bool

	// memVar is the variable that memory is stored under. A sentinel object
	// rather than a field, so that memory uses the same phi machinery as every
	// other variable.
	memVar  *ir.Object
	initMem *Value

	labels       map[string]*Block
	labelOrder   []string
	labelDefined map[string]bool

	// pendingLabel is the label of the statement about to be built, so that a
	// labelled break or continue can find the loop or switch it names.
	pendingLabel string

	// fallthru is the block a fallthrough in the clause being built jumps to,
	// or nil outside a clause and in the last one. It is a field rather than a
	// label because fallthrough names the next clause's body and not its test:
	// the case expressions of that clause are not evaluated.
	fallthru *Block

	ctrl []ctrlFrame

	ptrTypes map[*ir.Type]*ir.Type
	boolType *ir.Type
	intType  *ir.Type

	// descs names each type descriptor once, so that two conversions of one
	// type produce two relocations against one object. It is a lookup table
	// and is never ranged over; Func.Descriptors carries the same set in
	// first-use order for the caller that has to emit them
	// (specs/053-determinism.md).
	descs map[string]*ir.Object
}

// ctrlFrame is one enclosing loop or switch.
//
// A switch supplies a break target and no continue target: continue inside a
// switch belongs to the loop around it. That is why the two targets are
// separate fields rather than one, and why loop is a field rather than being
// inferred from cont being set.
type ctrlFrame struct {
	loop  bool
	label string
	brk   *Block
	cont  *Block
}

func (b *builder) errorf(inv Invariant, format string, args ...any) {
	if b.err != nil {
		return
	}
	b.err = &Error{Func: b.fn.Name, Invariant: inv, Detail: fmt.Sprintf(format, args...)}
}

// unsupported reports a construct construction does not handle yet.
func (b *builder) unsupported(n *ir.Node, what string) {
	b.errorf(InvNone, "%s: %s is not built yet", n.Op, what)
}

// classify decides, once and before construction, which objects live in a
// value and which live in the frame.
//
// specs/021-ssa-construction.md: a local whose address is taken cannot live in
// an SSA value, because two names would refer to one location. It is accessed
// by load and store through memory instead. A value that does not fit one
// machine word is treated the same way, because nothing here splits a string,
// a slice or a struct into components; specs/025-lowering-and-rules.md does
// that, and until it does, the frame is the safe answer.
//
// The walk is in declaration order, taken from the slices, never from the map.
// specs/053-determinism.md forbids the other order.
func (b *builder) classify() {
	add := func(o *ir.Object) {
		if o == nil || b.frame[o] {
			return
		}
		if o.Type == nil {
			// Every object reaching SSA construction has a layout. Without
			// one, neither a frame slot nor a value can be sized, and a guess
			// here would be a wrong stack map later.
			b.errorf(InvNone, "%s has no type", o.Name)
			return
		}
		if o.Addrtaken || !ssaAble(o.Type) {
			b.frame[o] = true
			b.f.Frame = append(b.f.Frame, o)
		}
	}
	add(b.fn.Recv)
	add(b.fn.Closure)
	for _, p := range b.fn.Params {
		add(p)
	}
	for _, r := range b.fn.Results {
		add(r)
	}
	for _, l := range b.fn.Locals {
		add(l)
	}
}

// ssaAble reports whether a value of type t can live in one SSA value.
func ssaAble(t *ir.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case ir.Bool, ir.Int8, ir.Int16, ir.Int32, ir.Int64,
		ir.Uint8, ir.Uint16, ir.Uint32, ir.Uint64, ir.Uintptr,
		ir.Float32, ir.Float64, ir.Ptr, ir.UnsafePtr,
		ir.Map, ir.Chan, ir.FuncKind:
		return true
	}
	return false
}

// entry fills the entry block: the initial memory, the parameters, and the
// zero value of every result and local.
func (b *builder) entry() {
	if b.err != nil {
		return
	}
	e := b.f.Entry
	e.Kind = BlockPlain
	e.sealed = true
	e.Pos = b.fn.Pos
	b.cur = e

	b.initMem = e.NewValue(b.fn.Pos, OpInitMem, MemType)
	b.write(b.memVar, e, b.initMem)

	params := make([]*ir.Object, 0, len(b.fn.Params)+1)
	if b.fn.Recv != nil {
		params = append(params, b.fn.Recv)
	}
	params = append(params, b.fn.Params...)
	for _, p := range params {
		arg := e.NewValue(p.Pos, OpArg, p.Type)
		arg.Aux = p
		if b.frame[p] {
			b.store(b.localAddr(p, p.Pos), arg, p.Type, p.Pos)
			continue
		}
		b.write(p, e, arg)
	}

	// The context parameter arrives in a register the convention names and
	// not in an argument register, so it is not one of the parameters above:
	// OpGetClosurePtr is what reads it, and it is here rather than at the
	// first use because a call overwrites the register
	// (specs/033-closures-defer-panic.md).
	if c := b.fn.Closure; c != nil {
		ctx := e.NewValue(c.Pos, OpGetClosurePtr, c.Type)
		ctx.Aux = c
		if b.frame[c] {
			b.store(b.localAddr(c, c.Pos), ctx, c.Type, c.Pos)
		} else {
			b.write(c, e, ctx)
		}
	}

	// Go zeroes a result and a local before the body runs. A value-resident
	// one gets the zero constant; a frame-resident one gets a Zero through
	// memory, which is what puts the initialisation in the memory order.
	for _, o := range append(append([]*ir.Object{}, b.fn.Results...), b.fn.Locals...) {
		if b.frame[o] {
			m := b.memory()
			z := e.NewValue(o.Pos, OpZero, MemType, b.localAddr(o, o.Pos), m)
			z.AuxInt = o.Type.Size
			b.setMemory(z)
			continue
		}
		b.write(o, e, b.zeroValue(o.Type))
	}
}

// finish closes the last block, seals what is still open, and removes what
// turned out to be unreachable.
func (b *builder) finish() {
	if b.cur != nil {
		b.returnStmt(nil, b.fn.Pos)
	}
	// Label blocks are sealed last, in the order the labels were first named.
	// A backward goto is the only reason a block stays unsealed to the end,
	// and specs/021-ssa-construction.md says why the rest is easy: a label is
	// a statement boundary, so a label always starts a block and the only work
	// is deferring the seal.
	for _, name := range b.labelOrder {
		if !b.labelDefined[name] {
			b.errorf(InvNone, "goto %s: no such label", name)
		}
		b.seal(b.labels[name])
	}
	for _, blk := range b.f.Blocks {
		b.seal(blk)
	}
	b.resolveAll()
	b.removeUnreachable()
	b.resolveAll()

	// Drop the construction bookkeeping. A pass that runs later must not see
	// a use list that construction stopped maintaining.
	for _, blk := range b.f.Blocks {
		blk.defs = nil
		blk.incomplete = nil
		for _, v := range blk.Values {
			v.uses = nil
			v.forward = nil
		}
	}
}

// resolveAll rewrites every reference to a phi that was removed.
//
// The use lists catch this as it happens, but a use list can go stale, and a
// block's control value is not in one. A sweep at the end is cheap and it is
// the difference between a dangling reference and a correct graph.
func (b *builder) resolveAll() {
	for _, blk := range b.f.Blocks {
		blk.Control = resolve(blk.Control)
		for _, v := range blk.Values {
			for i, a := range v.Args {
				if r := resolve(a); r != a {
					v.Args[i] = r
				}
			}
		}
	}
}

// removeUnreachable deletes the blocks the entry cannot reach.
//
// Construction produces them: a label that only unreachable code jumps to, the
// block after a loop that never exits. specs/021-ssa-construction.md requires
// that no value is unreachable from the entry, and deadcode in
// specs/022-optimization-passes.md is not allowed to be required for
// correctness, so the graph must already satisfy the invariant when
// construction hands it over.
func (b *builder) removeUnreachable() {
	live := make([]bool, b.f.NumBlocks())
	for _, blk := range b.f.Postorder() {
		live[blk.ID] = true
	}

	kept := b.f.Blocks[:0]
	for _, blk := range b.f.Blocks {
		if !live[blk.ID] {
			for _, v := range blk.Values {
				v.dead = true
			}
			continue
		}
		kept = append(kept, blk)
	}
	b.f.Blocks = kept

	var phis []*Value
	for _, blk := range b.f.Blocks {
		// A predecessor slot and a phi argument are the same edge, so the two
		// are filtered together and in the same order.
		drop := make([]bool, len(blk.Preds))
		n := 0
		for i, p := range blk.Preds {
			if !live[p.ID] {
				drop[i] = true
				n++
			}
		}
		if n == 0 {
			continue
		}
		preds := blk.Preds[:0]
		for i, p := range blk.Preds {
			if !drop[i] {
				preds = append(preds, p)
			}
		}
		blk.Preds = preds
		for _, v := range blk.Values {
			if v.Op != OpPhi {
				break
			}
			args := v.Args[:0]
			for i, a := range v.Args {
				if i < len(drop) && !drop[i] {
					args = append(args, a)
				}
			}
			v.Args = args
			phis = append(phis, v)
		}
	}
	for _, phi := range phis {
		if !phi.dead {
			b.tryRemoveTrivialPhi(phi)
		}
	}
}

// The variable map of Braun et al. (2013).

func (b *builder) write(v *ir.Object, blk *Block, val *Value) {
	if blk == nil || val == nil {
		return
	}
	blk.defs[v] = val
}

func (b *builder) read(v *ir.Object, blk *Block) *Value {
	if val, ok := blk.defs[v]; ok {
		val = resolve(val)
		blk.defs[v] = val
		return val
	}
	return b.readRecursive(v, blk)
}

func (b *builder) readRecursive(v *ir.Object, blk *Block) *Value {
	var val *Value
	switch {
	case !blk.sealed:
		// The predecessors are not all known, so the answer cannot be looked
		// up yet. Leave a phi with no arguments and fill it at the seal.
		val = b.newPhi(v, blk)
		blk.incomplete = append(blk.incomplete, incompletePhi{v, val})
	case len(blk.Preds) == 1:
		val = b.read(v, blk.Preds[0])
	case len(blk.Preds) == 0:
		// A read with no definition anywhere above it. The entry block writes
		// every local before the body runs, so this is either unreachable code
		// or a malformed tree, and the zero value keeps the graph well formed
		// either way.
		val = b.zeroValue(v.Type)
	default:
		// Write the phi before recursing. The recursion can reach this block
		// again through a back edge, and the incomplete phi is what stops it.
		val = b.newPhi(v, blk)
		b.write(v, blk, val)
		val = b.addPhiArgs(v, val)
	}
	b.write(v, blk, val)
	return val
}

// newPhi adds a phi for v at the start of blk.
//
// After the phis already there, because a phi selects on the edge taken and
// every phi of a block runs before anything else in it.
func (b *builder) newPhi(v *ir.Object, blk *Block) *Value {
	t := v.Type
	phi := b.f.newValue(OpPhi, t, blk.Pos)
	phi.Block = blk
	i := 0
	for i < len(blk.Values) && blk.Values[i].Op == OpPhi {
		i++
	}
	blk.Values = append(blk.Values, nil)
	copy(blk.Values[i+1:], blk.Values[i:])
	blk.Values[i] = phi
	return phi
}

func (b *builder) addPhiArgs(v *ir.Object, phi *Value) *Value {
	for _, p := range phi.Block.Preds {
		phi.AddArg(b.read(v, p))
	}
	return b.tryRemoveTrivialPhi(phi)
}

// tryRemoveTrivialPhi removes a phi whose arguments are all itself or all one
// other value, and returns what callers should use instead.
//
// This is where the minimality of Braun et al. (2013) comes from. A phi that
// merges one value merges nothing, and removing it can make the phis that used
// it trivial in turn, so the removal recurses into the users.
func (b *builder) tryRemoveTrivialPhi(phi *Value) *Value {
	var same *Value
	for _, a := range phi.Args {
		a = resolve(a)
		if a == same || a == phi {
			continue
		}
		if same != nil {
			return phi
		}
		same = a
	}
	if same == nil {
		// No argument other than itself: the phi is unreachable or is only
		// fed by itself. The zero value keeps every user well formed.
		if phi.Type == MemType {
			same = b.initMem
		} else {
			same = b.zeroValue(phi.Type)
		}
	}

	users := phi.uses
	phi.forward = same
	phi.Block.removeValue(phi)

	for _, u := range users {
		if u == phi || u.dead {
			continue
		}
		for i, a := range u.Args {
			if a == phi {
				u.Args[i] = same
				same.uses = append(same.uses, u)
			}
		}
	}
	for _, u := range users {
		if u != phi && !u.dead && u.Op == OpPhi {
			b.tryRemoveTrivialPhi(u)
		}
	}
	return resolve(same)
}

// resolve follows the chain a removed phi left behind.
func resolve(v *Value) *Value {
	for v != nil && v.forward != nil {
		v = v.forward
	}
	return v
}

// seal records that every predecessor of blk is known and fills the phis that
// were waiting for it.
func (b *builder) seal(blk *Block) {
	if blk.sealed {
		return
	}
	blk.sealed = true
	// By index: filling one phi can read the same variable again and the
	// slice can be appended to while this runs.
	for i := 0; i < len(blk.incomplete); i++ {
		ip := blk.incomplete[i]
		if ip.phi.dead {
			continue
		}
		b.addPhiArgs(ip.variable, ip.phi)
	}
	blk.incomplete = nil
}

// Memory.

func (b *builder) memory() *Value { return b.read(b.memVar, b.cur) }

func (b *builder) setMemory(v *Value) { b.write(b.memVar, b.cur, v) }

// Statements.

func (b *builder) stmts(list []ir.Stmt) {
	for _, s := range list {
		b.stmt(s)
	}
}

func (b *builder) stmt(n ir.Stmt) {
	if n == nil || b.err != nil {
		return
	}
	if n.Op.IsGoSpecific() {
		b.errorf(InvGoSpecific, "%s reached SSA construction", n.Op)
		return
	}
	// A label starts a block even where control cannot fall into it, because
	// a goto may still reach it. Everything else after a terminator is dead.
	if b.cur == nil && n.Op != ir.OLabel {
		return
	}

	label := b.pendingLabel
	b.pendingLabel = ""

	switch n.Op {
	case ir.OBlock:
		b.stmts(n.Init)
		b.stmts(n.Body)
	case ir.OIf:
		b.ifStmt(n)
	case ir.OFor:
		b.forStmt(n, label)
	case ir.OSwitch:
		b.switchStmt(n, label)
	case ir.OReturn:
		b.stmts(n.Init)
		b.returnStmt(n.Args, n.Pos)
	case ir.OLabel:
		b.labelStmt(n)
	case ir.OGoto:
		if n.Label == fallthroughLabel {
			if b.fallthru == nil {
				b.errorf(InvNone, "fallthrough outside a switch clause that has a next one")
				return
			}
			b.jump(b.fallthru)
			return
		}
		b.jump(b.labelBlock(n.Label))
	case ir.OBreak:
		t := b.target(n.Label, false)
		if t == nil {
			b.errorf(InvNone, "break %q: no enclosing loop or switch", n.Label)
			return
		}
		b.jump(t)
	case ir.OContinue:
		t := b.target(n.Label, true)
		if t == nil {
			b.errorf(InvNone, "continue %q: no enclosing loop", n.Label)
			return
		}
		b.jump(t)
	case ir.OCall:
		b.stmts(n.Init)
		// A call statement discards the results, so the call itself is built
		// and nothing reads it. Reading result zero and dropping the value
		// would leave a value the code generator has to place for nobody.
		b.callValue(n)
	case ir.OAssign:
		b.stmts(n.Init)
		b.assignStmt(n)
	default:
		b.unsupported(n, "statement")
	}
}

// forParts returns the four parts of a for statement.
//
// The post statements are in Post. An earlier version of this function read
// them out of Else, which was the convention before ir.Node had the field, and
// ir.Build has never written them there: every post statement of the corpus
// was silently dropped, so every counted loop ran forever.
func forParts(n *ir.Node) (init []ir.Stmt, cond ir.Expr, post []ir.Stmt, body []ir.Stmt) {
	return n.Init, n.X, n.Post, n.Body
}

// switchCases returns the clauses of a switch.
//
// A clause is an ir.OCase whose Args are the case expressions and whose Body
// is the clause body. A clause with no case expression is the default. An
// earlier version required an ir.OBlock, which was the convention before
// ir.OCase existed, and refused 992 functions of the corpus for having the
// node the IR does emit.
//
// A clause's Init is not built, and that is checked rather than assumed:
// ir.Build writes Init on a clause only in selectStmt, where it holds the
// communication statement the specification evaluates on entry to the select.
// ir.OSelect is Go-specific, so such a clause never reaches here.
func switchCases(n *ir.Node) (clauses []*ir.Node, ok bool) {
	for _, c := range n.Body {
		if c == nil || c.Op != ir.OCase {
			return nil, false
		}
		clauses = append(clauses, c)
	}
	return clauses, true
}

func (b *builder) ifStmt(n *ir.Node) {
	b.stmts(n.Init)
	cond := b.expr(n.X)
	if b.err != nil {
		return
	}
	then := b.f.NewBlock(BlockPlain)
	els := b.f.NewBlock(BlockPlain)
	done := b.f.NewBlock(BlockPlain)

	b.cur.Kind = BlockIf
	b.cur.Control = cond
	b.cur.AddEdgeTo(then)
	b.cur.AddEdgeTo(els)
	// Both arms have exactly one predecessor and it is known, so both are
	// sealed at once. This is why Go's statement structure needs no dominance
	// frontier: almost every block is sealed the moment it is created.
	b.seal(then)
	b.seal(els)

	b.cur = then
	b.stmts(n.Body)
	b.jumpIfLive(done)

	b.cur = els
	b.stmts(n.Else)
	b.jumpIfLive(done)

	b.startJoin(done)
}

func (b *builder) forStmt(n *ir.Node, label string) {
	init, cond, post, body := forParts(n)
	b.stmts(init)
	if b.err != nil {
		return
	}

	// The loop header is the one block Go's structure leaves unsealed: its
	// second predecessor is the back edge, which does not exist yet.
	head := b.f.NewBlock(BlockPlain)
	b.cur.Kind = BlockPlain
	b.cur.AddEdgeTo(head)

	bodyB := b.f.NewBlock(BlockPlain)
	postB := b.f.NewBlock(BlockPlain)
	exit := b.f.NewBlock(BlockPlain)

	b.cur = head
	if cond != nil {
		c := b.expr(cond)
		if b.err != nil {
			return
		}
		// Evaluating the condition can add blocks, so the branch goes on the
		// block the evaluation ended in, not on the header.
		b.cur.Kind = BlockIf
		b.cur.Control = c
		b.cur.AddEdgeTo(bodyB)
		b.cur.AddEdgeTo(exit)
	} else {
		b.cur.Kind = BlockPlain
		b.cur.AddEdgeTo(bodyB)
	}

	b.seal(bodyB)
	b.ctrl = append(b.ctrl, ctrlFrame{loop: true, label: label, brk: exit, cont: postB})
	b.cur = bodyB
	b.stmts(body)
	b.jumpIfLive(postB)
	b.ctrl = b.ctrl[:len(b.ctrl)-1]

	// continue jumps here, so the post block is sealed only after the body.
	b.seal(postB)
	if len(postB.Preds) == 0 {
		b.f.removeBlock(postB)
	} else {
		b.cur = postB
		b.stmts(post)
		b.jumpIfLive(head)
	}
	// The back edge is in place, so the header is complete.
	b.seal(head)

	b.startJoin(exit)
}

func (b *builder) switchStmt(n *ir.Node, label string) {
	b.stmts(n.Init)
	clauses, ok := switchCases(n)
	if !ok {
		b.unsupported(n, "a switch whose clauses are not block nodes")
		return
	}
	var tag *Value
	if n.X != nil {
		tag = b.expr(n.X)
	}
	if b.err != nil {
		return
	}

	bodies := make([]*Block, len(clauses))
	for i := range clauses {
		bodies[i] = b.f.NewBlock(BlockPlain)
	}
	done := b.f.NewBlock(BlockPlain)

	// The tests, in source order. A switch is a chain of comparisons here;
	// turning a dense one into a jump table is a lowering question and belongs
	// to specs/025-lowering-and-rules.md.
	deflt := -1
	for i, c := range clauses {
		if len(c.Args) == 0 {
			if deflt >= 0 {
				b.errorf(InvNone, "switch has two default clauses")
				return
			}
			deflt = i
			continue
		}
		for _, ce := range c.Args {
			cond := b.expr(ce)
			if tag != nil {
				cond = b.value(OpEq, b.boolType, ce.Pos, tag, cond)
			}
			if b.err != nil {
				return
			}
			next := b.f.NewBlock(BlockPlain)
			b.cur.Kind = BlockIf
			b.cur.Control = cond
			b.cur.AddEdgeTo(bodies[i])
			b.cur.AddEdgeTo(next)
			b.seal(next)
			b.cur = next
		}
	}
	if deflt >= 0 {
		b.jump(bodies[deflt])
	} else {
		b.jump(done)
	}

	b.ctrl = append(b.ctrl, ctrlFrame{label: label, brk: done})
	// The binding of fallthrough is saved and restored around the clauses, so
	// that a switch inside a clause binds its own and gives this one back.
	outer := b.fallthru
	for i, c := range clauses {
		b.fallthru = nil
		if i+1 < len(clauses) {
			b.fallthru = bodies[i+1]
		}
		// Every predecessor of this body is known by now: the test chain added
		// its edge before the loop, and a fallthrough from the clause before
		// added its edge in the previous iteration.
		b.seal(bodies[i])
		b.cur = bodies[i]
		b.stmts(c.Body)
		b.jumpIfLive(done)
	}
	b.fallthru = outer
	b.ctrl = b.ctrl[:len(b.ctrl)-1]

	b.startJoin(done)
}

// fallthroughLabel is the label ir.Build gives a fallthrough statement. A
// source label cannot collide with it, because fallthrough is a keyword.
const fallthroughLabel = "fallthrough"

func (b *builder) labelStmt(n *ir.Node) {
	blk := b.labelBlock(n.Label)
	if b.labelDefined[n.Label] {
		b.errorf(InvNone, "label %s declared twice", n.Label)
		return
	}
	b.labelDefined[n.Label] = true
	if b.cur != nil {
		b.jump(blk)
	}
	b.cur = blk
	if len(n.Body) > 0 {
		// A labelled statement. The label names the loop or the switch that
		// follows, so a labelled break or continue can find it.
		b.pendingLabel = n.Label
		b.stmts(n.Body)
		b.pendingLabel = ""
	}
}

func (b *builder) labelBlock(name string) *Block {
	if blk, ok := b.labels[name]; ok {
		return blk
	}
	blk := b.f.NewBlock(BlockPlain)
	b.labels[name] = blk
	// The order labels were first named. Sealing order decides the order phis
	// are created in, so it must not come from the map.
	b.labelOrder = append(b.labelOrder, name)
	return blk
}

// target returns the block a break or a continue jumps to.
func (b *builder) target(label string, cont bool) *Block {
	for i := len(b.ctrl) - 1; i >= 0; i-- {
		fr := b.ctrl[i]
		if cont && !fr.loop {
			// continue passes through a switch to the loop around it.
			continue
		}
		if label != "" && fr.label != label {
			continue
		}
		if cont {
			return fr.cont
		}
		return fr.brk
	}
	return nil
}

func (b *builder) returnStmt(args []ir.Expr, pos syntax.Pos) {
	vals := make([]*Value, 0, len(args)+1)
	for _, a := range args {
		vals = append(vals, b.expr(a))
	}
	if len(args) == 0 {
		// A bare return leaves with whatever the named results hold.
		for _, r := range b.fn.Results {
			vals = append(vals, b.readObject(r, pos))
		}
	}
	if b.err != nil {
		return
	}
	vals = append(vals, b.memory())
	mr := b.value(OpMakeResult, MemType, pos, vals...)
	b.cur.Kind = BlockRet
	b.cur.Control = mr
	b.cur = nil
}

// jump ends the current block with an edge to target.
func (b *builder) jump(target *Block) {
	b.cur.Kind = BlockPlain
	b.cur.AddEdgeTo(target)
	b.cur = nil
}

func (b *builder) jumpIfLive(target *Block) {
	if b.cur != nil {
		b.jump(target)
	}
}

// startJoin makes done the current block, or drops it when nothing reaches it.
func (b *builder) startJoin(done *Block) {
	if len(done.Preds) == 0 {
		b.f.removeBlock(done)
		b.cur = nil
		return
	}
	b.seal(done)
	b.cur = done
}

// Expressions.

func (b *builder) value(op Op, t *ir.Type, pos syntax.Pos, args ...*Value) *Value {
	return b.cur.NewValue(pos, op, t, args...)
}

func (b *builder) expr(n ir.Expr) *Value {
	if b.err != nil {
		return b.zeroValue(b.intType)
	}
	if n == nil {
		b.errorf(InvNone, "an operand is missing")
		return b.zeroValue(b.intType)
	}
	if n.Op.IsGoSpecific() {
		b.errorf(InvGoSpecific, "%s reached SSA construction", n.Op)
		return b.zeroValue(n.Type)
	}
	// An expression carries statements in Init where there is no enclosing
	// statement list to put them in, which ir.Build's conventions name as a
	// loop condition and the right operand of && and ||. Both are evaluated
	// somewhere other than where the statement holding them is, so building
	// them here is what puts them on the one path that reaches the operand.
	// Construction dropped them until now, which left every temporary they
	// assign holding the zero the entry block wrote.
	if len(n.Init) > 0 {
		b.stmts(n.Init)
		if b.cur == nil {
			// Control left the block the operand was to be built in, so the
			// operand has nowhere to go. Nothing in ir.Build produces this,
			// and the alternative to naming it is a nil block dereference.
			b.errorf(InvNone, "%s: control left the block its operands are in", n.Op)
		}
		if b.err != nil {
			return b.zeroValue(n.Type)
		}
	}
	switch n.Op {
	case ir.OConst:
		return b.constant(n)

	case ir.OLocal:
		return b.readObject(n.Obj, n.Pos)

	case ir.OGlobal:
		if n.Obj != nil && n.Obj.Class == ir.ClassFunc {
			v := b.value(OpAddr, b.ptrTo(n.Type), n.Pos)
			v.Aux = n.Obj
			return v
		}
		return b.load(b.addr(n), n.Type, n.Pos)

	case ir.OField, ir.OIndex, ir.ODeref:
		return b.load(b.addr(n), n.Type, n.Pos)

	case ir.OAddr:
		return b.addr(n.X)

	case ir.OUnary:
		return b.unary(n)

	case ir.OBinary:
		if n.Op1 == syntax.AndAnd || n.Op1 == syntax.OrOr {
			return b.shortCircuit(n)
		}
		if isStringConcat(n) {
			return b.concat(n)
		}
		op, ok := binaryOp(n.Op1)
		if !ok {
			b.unsupported(n, "operator "+n.Op1.String())
			return b.zeroValue(n.Type)
		}
		return b.value(op, n.Type, n.Pos, b.expr(n.X), b.expr(n.Y))

	case ir.OCompare:
		return b.compare(n)

	case ir.OConvert:
		return b.convert(n)

	case ir.OCall:
		v := b.call(n)
		if v == nil {
			return b.zeroValue(n.Type)
		}
		return v
	}
	b.unsupported(n, "expression")
	return b.zeroValue(n.Type)
}

// readObject returns the value of an object, from the variable map or from its
// frame slot.
func (b *builder) readObject(o *ir.Object, pos syntax.Pos) *Value {
	if o == nil {
		b.errorf(InvNone, "a local with no object")
		return b.zeroValue(b.intType)
	}
	if b.frame[o] {
		return b.load(b.localAddr(o, pos), o.Type, pos)
	}
	if o.Class == ir.ClassGlobal {
		return b.load(b.globalAddr(o, pos), o.Type, pos)
	}
	return b.read(o, b.cur)
}

// assignStmt builds an assignment.
//
// ir.Build's conventions: X is the destination and Y is the source, and a
// multi-value assignment leaves X nil and lists its destinations in Args. Op1
// is syntax.Def when the statement declares its destinations, which is x := y
// and var x = y.
//
// Both forms of Op1 build the same thing, and that is the whole of what
// construction owes the Go 1.22 loop variable rule. ir.Build has already given
// each iteration its own instance: a loop variable whose address is taken gets
// a carrier that the loop control works on, and the body opens by declaring
// the variable again from the carrier. Honouring that means building the
// declaration as a definition wherever it executes, every time it executes.
// Treating syntax.Def as "the entry block already zeroed this, so there is
// nothing to do" is what would put the pre-1.22 semantics back, because the
// body would then read whatever the previous iteration left.
//
// What a definition does not yet emit is the lifetime marker. OpVarDef says a
// frame slot's previous contents are dead, which is the other half of "a new
// instance each time"; specs/025-lowering-and-rules.md owns the markers and
// ssa/liveness.go reads them, so emitting one here would move a decision this
// pass does not own. Without it every stack object is live from the entry,
// which is the conservative answer.
func (b *builder) assignStmt(n *ir.Node) {
	if n.X != nil {
		if n.Y == nil {
			b.errorf(InvNone, "an assignment with no value")
			return
		}
		b.assign(n.X, b.expr(n.Y), n.Pos)
		return
	}
	if len(n.Args) == 0 {
		b.errorf(InvNone, "an assignment with no destination")
		return
	}
	b.multiAssign(n)
}

// multiAssign builds an assignment whose one value produces several.
//
// The value is a call, a two-value map read, a type assertion or a channel
// receive. Only the call is built. The other three are rows of
// specs/020-ir.md's lowering table that no pass performs, so they arrive here
// intact and are refused, the receive and the assertion by the Go-specific
// guard and the map read by addr.
func (b *builder) multiAssign(n *ir.Node) {
	src := n.Y
	if src == nil {
		b.errorf(InvNone, "a multi-value assignment with no value")
		return
	}
	if src.Op != ir.OCall {
		b.unsupported(n, "a multi-value assignment from "+src.Op.String())
		return
	}
	types := resultTypes(src.Type, len(n.Args))
	if len(types) != len(n.Args) {
		b.errorf(InvNone, "%d destinations and %d results", len(n.Args), len(types))
		return
	}
	c := b.callValue(src)
	if b.err != nil || c == nil {
		return
	}
	// Every result is read before any destination is written, and both halves
	// of that matter. SelectN reads the call, which is a memory value, so a
	// SelectN placed after a store would read a memory the store has already
	// superseded and break the one-memory-per-point invariant. The order is
	// also the specification's: a multi-value assignment produces its values
	// first and assigns them left to right afterwards.
	//
	// One SelectN per result, including a result assigned to the blank
	// identifier: the code generator places the results of a call together and
	// names each one by the SelectN that reads it, so a gap in the sequence is
	// a result it cannot place. ssa/decompose.go needs the same list for the
	// same reason, because it renumbers a result by the widths of the results
	// before it, and a result wider than a register is what makes those widths
	// differ.
	reads := make([]*Value, len(n.Args))
	for i := range n.Args {
		r := b.value(OpSelectN, types[i], n.Pos, c)
		r.AuxInt = int64(i)
		reads[i] = r
	}
	for i, dst := range n.Args {
		b.assign(dst, reads[i], n.Pos)
	}
}

// resultTypes returns the types of the values one expression produces.
//
// A tuple is the type of a multi-value expression and its fields are the
// values, in order. A single result is not a tuple, so a call with one result
// assigned to one destination reads its own type.
func resultTypes(t *ir.Type, want int) []*ir.Type {
	if t == nil {
		return nil
	}
	if t.Kind != ir.Tuple {
		if want == 1 {
			return []*ir.Type{t}
		}
		return nil
	}
	out := make([]*ir.Type, len(t.Fields))
	for i := range t.Fields {
		out[i] = t.Fields[i].Type
	}
	return out
}

func (b *builder) assign(dst ir.Expr, val *Value, pos syntax.Pos) {
	if b.err != nil || dst == nil {
		return
	}
	if dst.Op == ir.OLocal && dst.Obj != nil && !b.frame[dst.Obj] && dst.Obj.Class != ir.ClassGlobal {
		b.write(dst.Obj, b.cur, val)
		return
	}
	t := dst.Type
	if t == nil {
		t = b.intType
	}
	b.store(b.addr(dst), val, t, pos)
}

func (b *builder) store(addr, val *Value, t *ir.Type, pos syntax.Pos) {
	if b.err != nil {
		return
	}
	s := b.value(OpStore, MemType, pos, addr, val, b.memory())
	s.AuxInt = t.Size
	b.setMemory(s)
}

func (b *builder) load(addr *Value, t *ir.Type, pos syntax.Pos) *Value {
	return b.value(OpLoad, t, pos, addr, b.memory())
}

func (b *builder) localAddr(o *ir.Object, pos syntax.Pos) *Value {
	v := b.value(OpLocalAddr, b.ptrTo(o.Type), pos, b.memory())
	v.Aux = o
	return v
}

func (b *builder) globalAddr(o *ir.Object, pos syntax.Pos) *Value {
	v := b.value(OpAddr, b.ptrTo(o.Type), pos)
	v.Aux = o
	return v
}

// addr returns the address of an addressable expression.
func (b *builder) addr(n ir.Expr) *Value {
	if b.err != nil || n == nil {
		return b.zeroValue(b.intType)
	}
	switch n.Op {
	case ir.OLocal:
		if n.Obj == nil {
			b.errorf(InvNone, "a local with no object")
			return b.zeroValue(b.intType)
		}
		if n.Obj.Class == ir.ClassGlobal {
			return b.globalAddr(n.Obj, n.Pos)
		}
		if !b.frame[n.Obj] {
			// classify decides this before construction, so reaching here
			// means the address of a value-resident local was taken after the
			// decision was made. That is the corruption specs/021 warns about,
			// so it is an error rather than a repair.
			b.errorf(InvNone, "the address of %s is taken but it lives in a value", n.Obj.Name)
			return b.zeroValue(b.intType)
		}
		return b.localAddr(n.Obj, n.Pos)

	case ir.OGlobal:
		if n.Obj == nil {
			b.errorf(InvNone, "a global with no object")
			return b.zeroValue(b.intType)
		}
		return b.globalAddr(n.Obj, n.Pos)

	case ir.ODeref:
		return b.nilCheck(b.expr(n.X), n.Pos)

	case ir.OField:
		base, st := b.fieldBase(n.X)
		if b.err != nil {
			return base
		}
		if n.Index < 0 || n.Index >= len(st.Fields) {
			b.errorf(InvNone, "field %d of %v", n.Index, st)
			return base
		}
		v := b.value(OpOffPtr, b.ptrTo(st.Fields[n.Index].Type), n.Pos, base)
		v.AuxInt = st.Fields[n.Index].Offset
		return v

	case ir.OIndex:
		return b.indexAddr(n)
	}
	b.unsupported(n, "an address")
	return b.zeroValue(b.intType)
}

// fieldBase returns the address of the struct a field selects from, and the
// struct type.
func (b *builder) fieldBase(x ir.Expr) (*Value, *ir.Type) {
	if x == nil || x.Type == nil {
		b.errorf(InvNone, "a field of an expression with no type")
		return b.zeroValue(b.intType), &ir.Type{Kind: ir.Struct}
	}
	if x.Type.Kind == ir.Ptr {
		return b.nilCheck(b.expr(x), x.Pos), x.Type.Elem
	}
	return b.addr(x), x.Type
}

// indexAddr returns the address of an element, with the bounds check that
// specs/021-ssa-construction.md requires in front of it.
func (b *builder) indexAddr(n ir.Expr) *Value {
	x := n.X
	if x == nil || x.Type == nil {
		b.errorf(InvNone, "an index of an expression with no type")
		return b.zeroValue(b.intType)
	}
	if n.Y == nil {
		b.errorf(InvNone, "an index with no index expression")
		return b.zeroValue(b.intType)
	}
	idx := b.expr(n.Y)

	var base, length *Value
	var elem *ir.Type
	switch x.Type.Kind {
	case ir.Array:
		elem = x.Type.Elem
		base = b.addr(x)
		length = b.constInt(x.Type.Len, n.Pos)

	case ir.Ptr:
		if x.Type.Elem == nil || x.Type.Elem.Kind != ir.Array {
			b.unsupported(n, "an index of a pointer that is not to an array")
			return b.zeroValue(b.intType)
		}
		elem = x.Type.Elem.Elem
		base = b.nilCheck(b.expr(x), n.Pos)
		length = b.constInt(x.Type.Elem.Len, n.Pos)

	case ir.Slice, ir.String:
		// A slice header is a pointer, a length and a capacity, and a string
		// header is a pointer and a length. Both live in the frame here, so
		// both are read through memory.
		hdr := b.addr(x)
		elem = x.Type.Elem
		if x.Type.Kind == ir.String {
			elem = &ir.Type{Kind: ir.Uint8, Size: 1, Align: 1, Name: "byte"}
		}
		base = b.load(hdr, b.ptrTo(elem), n.Pos)
		lenPtr := b.value(OpOffPtr, b.ptrTo(b.intType), n.Pos, hdr)
		lenPtr.AuxInt = ir.PtrSize
		length = b.load(lenPtr, b.intType, n.Pos)

	default:
		b.unsupported(n, "an index of "+x.Type.Kind.String())
		return b.zeroValue(b.intType)
	}
	if b.err != nil {
		return b.zeroValue(b.intType)
	}

	// The check is inserted whether or not it can be discharged here.
	// specs/022-optimization-passes.md removes the ones prove can discharge,
	// and the asymmetry is the point: inserting all of them and removing some
	// is safe, inserting some is not.
	chk := b.value(OpBoundsCheck, MemType, n.Pos, idx, length, b.memory())
	b.setMemory(chk)

	v := b.value(OpPtrIndex, b.ptrTo(elem), n.Pos, base, idx)
	if elem != nil {
		v.AuxInt = elem.Size
	}
	return v
}

// nilCheck inserts the check a dereference needs.
func (b *builder) nilCheck(ptr *Value, pos syntax.Pos) *Value {
	return b.value(OpNilCheck, ptr.Type, pos, ptr, b.memory())
}

func (b *builder) unary(n ir.Expr) *Value {
	x := b.expr(n.X)
	switch n.Op1 {
	case syntax.Not:
		return b.value(OpNot, n.Type, n.Pos, x)
	case syntax.Sub:
		return b.value(OpNeg, n.Type, n.Pos, x)
	case syntax.Xor:
		return b.value(OpCom, n.Type, n.Pos, x)
	case syntax.Add:
		return x
	}
	b.unsupported(n, "unary "+n.Op1.String())
	return b.zeroValue(n.Type)
}

func (b *builder) compare(n ir.Expr) *Value {
	x, y := b.expr(n.X), b.expr(n.Y)
	op, swap, ok := compareOp(n.Op1)
	if !ok {
		b.unsupported(n, "comparison "+n.Op1.String())
		return b.zeroValue(n.Type)
	}
	if swap {
		x, y = y, x
	}
	t := n.Type
	if t == nil {
		t = b.boolType
	}
	return b.value(op, t, n.Pos, x, y)
}

// shortCircuit builds the control flow that && and || are.
//
// The result is read back through the variable map, so the phi that joins the
// two paths is the ordinary one Braun et al. (2013) produces. Nothing here
// knows that it is building a phi.
func (b *builder) shortCircuit(n ir.Expr) *Value {
	t := n.Type
	if t == nil {
		t = b.boolType
	}
	tmp := &ir.Object{Name: n.Op1.String(), Type: t, Class: ir.ClassLocal, Pos: n.Pos}

	x := b.expr(n.X)
	b.write(tmp, b.cur, x)

	rhs := b.f.NewBlock(BlockPlain)
	done := b.f.NewBlock(BlockPlain)
	b.cur.Kind = BlockIf
	b.cur.Control = x
	if n.Op1 == syntax.AndAnd {
		// x && y evaluates y only when x is true.
		b.cur.AddEdgeTo(rhs)
		b.cur.AddEdgeTo(done)
	} else {
		b.cur.AddEdgeTo(done)
		b.cur.AddEdgeTo(rhs)
	}
	b.seal(rhs)

	b.cur = rhs
	y := b.expr(n.Y)
	b.write(tmp, b.cur, y)
	b.jump(done)

	b.seal(done)
	b.cur = done
	return b.read(tmp, done)
}

func (b *builder) convert(n ir.Expr) *Value {
	x := b.expr(n.X)
	from, to := n.X.Type, n.Type
	if from == nil || to == nil {
		b.errorf(InvNone, "a conversion with no type")
		return x
	}
	// An interface is the one destination whose shape says nothing about what
	// the conversion has to do, so it is answered before the shape test below.
	if from.Kind == ir.Interface || to.Kind == ir.Interface {
		return b.convertInterface(n, x, from, to)
	}
	if from.Kind == to.Kind && from.Size == to.Size {
		return x
	}
	op, ok := convertOp(from, to)
	if !ok {
		b.unsupported(n, fmt.Sprintf("a conversion from %v to %v", from, to))
		return x
	}
	return b.value(op, to, n.Pos, x)
}

// convertInterface builds a conversion whose source or destination is an
// interface.
//
// Every interface is two words of the same width and reports one kind, so
// "same kind, same size" is true of every pair of them and says nothing. Two
// facts separate a pair, and neither is visible below the IR.
//
// The first is the word the value leads with. A non-empty interface leads with
// an *itab and an empty one leads with a *_type, so returning the value
// unchanged puts an *itab where the runtime reads a type descriptor. That is
// how panic(err) died in the runtime with "name offset out of range" rather
// than printing its value.
//
// The second is which interface an *itab was built for. An itab holds the
// concrete type's methods in the order the interface lists them, so an itab
// built for io.ReadWriter carries two entries where io.Reader reads one, and a
// value passed along unchanged calls through a slot that holds another
// method. Both interfaces are non-empty, both are 16 bytes, and the leading
// word is an *itab on each side, so the first fact does not separate them.
//
// ir.TypeLinkString is therefore the test, because two types have the same
// link string exactly when they are the same type. cmd/compile answers the
// same question the same way: walkConvInterface reaches its I2I path for every
// pair of interfaces that is not identical, whether or not the method sets
// happen to agree.
func (b *builder) convertInterface(n ir.Expr, x *Value, from, to *ir.Type) *Value {
	if to.Kind != ir.Interface {
		// A value leaves an interface through a type assertion, which is its
		// own node and not a conversion, so there is nothing to build here.
		b.unsupported(n, fmt.Sprintf("a conversion from %v to %v", from, to))
		return x
	}
	if from.Kind == ir.Interface {
		if sameType(from, to) {
			return x
		}
		b.unsupported(n, fmt.Sprintf("a conversion from %v to %v", from, to))
		return x
	}
	return b.concreteToInterface(n, x, from, to)
}

// concreteToInterface builds the interface value that holds x.
//
// The shape is cmd/compile's walkConvInterface: a type word and a data word,
// joined into one value. The type word is the concrete type's descriptor for an
// empty interface and the itab of the (interface, concrete type) pair for an
// interface with methods. Only the first is built, because an itab is a symbol
// this compiler has to define and specs/032 has no writer for one.
func (b *builder) concreteToInterface(n ir.Expr, x *Value, from, to *ir.Type) *Value {
	if !to.EmptyIface {
		b.unsupported(n, fmt.Sprintf("a conversion from %v to %v, which leads with the itab of that pair and specs/032 defines no itab", from, to))
		return b.zeroValue(to)
	}
	word := b.typeWord(n, from)
	data := b.dataWord(n, x, from)
	if b.err != nil {
		return b.zeroValue(to)
	}
	return b.value(OpIMake, to, n.Pos, word, data)
}

// typeWord returns the address of the type descriptor of t.
//
// The name comes from ir.TypeSymbol and is never built here. specs/032
// requires one naming function used by everything, because the linker
// deduplicates these symbols by name and two spellings of one type are two
// descriptors for a type that must have one.
//
// The type is recorded on the function, because a reference is not a
// definition: the descriptor of a type this package owns has to be written
// into the object, and only the caller holds the set a package emits.
func (b *builder) typeWord(n ir.Expr, t *ir.Type) *Value {
	name, err := ir.TypeSymbol(t)
	if err != nil {
		b.unsupported(n, fmt.Sprintf("a conversion from %v to an interface, whose type word is its descriptor: %v", t, err))
		return nil
	}
	o, ok := b.descs[name]
	if !ok {
		o = &ir.Object{Name: name, Type: descriptorType, Class: ir.ClassGlobal}
		b.descs[name] = o
		b.f.Descriptors = append(b.f.Descriptors, t)
	}
	v := b.value(OpAddr, b.ptrTo(descriptorType), n.Pos)
	v.Aux = o
	return v
}

// descriptorType is the type of a type descriptor, as construction sees it.
//
// The contents are rtype's business. What is needed here is a type of the right
// size and alignment to take the address of, because below the IR a type is a
// size, an alignment and a pointer map and nothing else. The size is
// rtype.TypeSize rather than a number written again here.
//
// It carries no name on purpose. A name would make ir.TypeSymbol produce
// type:runtime._type, which is a descriptor gc never emits and the linker would
// never resolve.
var descriptorType = func() *ir.Type {
	t := &ir.Type{
		Kind: ir.Array,
		Elem: &ir.Type{Kind: ir.Uintptr, Size: ir.PtrSize, Align: ir.PtrSize, Name: "uintptr"},
		Len:  rtype.TypeSize / ir.PtrSize,
	}
	if err := ir.Layout(t); err != nil {
		panic("ssa: a type descriptor does not lay out: " + err.Error())
	}
	return t
}()

// dataWord returns the second word of the interface value that holds x.
//
// It is cmd/compile's dataWord, minus the cases that only avoid an allocation.
// A pointer-shaped value is its own representation and goes in as it stands.
// Everything else is boxed, which means a copy in the heap and the address of
// the copy, and the runtime has one helper per shape it can box by value.
//
// The helpers that take an address are not reachable from here. runtime.convT
// and runtime.convTnoptr copy from a pointer the caller supplies, so the value
// needs a frame slot to be addressed through, and construction has already
// decided which objects live in the frame by the time an expression is built.
// A type they would be needed for is refused by name instead.
func (b *builder) dataWord(n ir.Expr, x *Value, from *ir.Type) *Value {
	switch {
	case directIface(from):
		// One word, and that word is a pointer. The value is the data word.
		return x

	case from.Kind == ir.String:
		return b.box(n, "runtime.convTstring", x)

	case from.Kind == ir.Slice:
		// convTslice takes []byte and copies the header, so the element type
		// does not reach it. cmd/compile passes any slice to it for the same
		// reason.
		return b.box(n, "runtime.convTslice", x)

	case boxableWord(from) && from.Size == 8 && from.Align == 8:
		return b.box(n, "runtime.convT64", b.rawBits(from, x, n.Pos))

	case boxableWord(from) && from.Size == 4 && from.Align == 4:
		return b.box(n, "runtime.convT32", b.rawBits(from, x, n.Pos))

	case boxableWord(from) && from.Size == 2 && from.Align == 2:
		return b.box(n, "runtime.convT16", x)
	}
	b.unsupported(n, fmt.Sprintf("a conversion from %v to an interface, whose data word needs runtime.convT with the address of a copy in the frame", from))
	return nil
}

// box calls one of the runtime's boxing helpers and returns the pointer it
// gives back.
func (b *builder) box(n ir.Expr, sym string, arg *Value) *Value {
	if arg == nil {
		return nil
	}
	c := b.value(OpStaticCall, MemType, n.Pos, arg, b.memory())
	c.Aux = RuntimeFunc(sym)
	b.setMemory(c)
	r := b.value(OpSelectN, b.ptrTo(nil), n.Pos, c)
	r.AuxInt = 0
	return r
}

// rawBits returns x as the integer the boxing helper takes.
//
// A floating-point value and an integer of the same width live in two register
// files, and specs/030-abi.md places an argument by the type of the value. A
// float passed to a helper that declares uint64 would be left in a
// floating-point register and read out of an integer one, so the
// reinterpretation is explicit.
func (b *builder) rawBits(t *ir.Type, x *Value, pos syntax.Pos) *Value {
	if !t.Kind.IsFloat() {
		return x
	}
	k := ir.Uint64
	if t.Size == 4 {
		k = ir.Uint32
	}
	u := &ir.Type{Kind: k, Size: t.Size, Align: t.Align, Name: k.String()}
	return b.value(OpBitcast, u, pos, x)
}

// directIface reports whether a value of t is its own data word.
//
// cmd/compile's types.IsDirectIface: one machine word wide, and that word
// holds a pointer. It is not the same question as "pointer shaped". A uintptr
// is one word and holds no pointer, so an interface holding one carries the
// integer's address and not the integer, and a one-field struct holding a
// pointer is not pointer shaped and is its own data word.
func directIface(t *ir.Type) bool {
	return t.Size == ir.PtrSize && t.PtrBytes() == ir.PtrSize
}

// boxableWord reports whether t is a single scalar the by-value helpers take.
//
// convT16, convT32 and convT64 take an unsigned integer of their own width, so
// what reaches them has to be one machine value of exactly that width. A
// struct or an array of the same width is not one: it is several values by the
// time specs/025's decomposition has run, and the helper reads one.
func boxableWord(t *ir.Type) bool {
	return t.Kind.IsInteger() || t.Kind.IsFloat()
}

// sameType reports whether two IR types are one type.
//
// The link string is the general answer: ir.TypeLinkString documents that two
// types have it in common exactly when they are the same type. It is not
// available for every type, and a type it cannot spell is reported as not the
// same, because the question is whether the two are provably one type and an
// unspellable type proves nothing.
//
// Pointer equality comes first and is not a shortcut. ir.Converter memoises,
// so one Go type of one package build is one *ir.Type, and the fast path is
// what answers for a type below the type boundary that carries no spelling at
// all.
func sameType(a, c *ir.Type) bool {
	if a == c {
		return true
	}
	if a == nil || c == nil {
		return false
	}
	x, err := ir.TypeLinkString(a)
	if err != nil {
		return false
	}
	y, err := ir.TypeLinkString(c)
	if err != nil {
		return false
	}
	return x == y
}

// call builds a call and returns its first result, or nil when it has none.
//
// A call with several results is read by multiAssign, which needs the call
// itself rather than one result, so the two are separate functions.
func (b *builder) call(n ir.Expr) *Value {
	c := b.callValue(n)
	if c == nil || b.err != nil {
		return nil
	}
	if n.Type == nil || n.Type.Kind == ir.Void {
		return nil
	}
	r := b.value(OpSelectN, n.Type, n.Pos, c)
	r.AuxInt = 0
	return r
}

// callValue builds a call and returns the call itself.
func (b *builder) callValue(n ir.Expr) *Value {
	fun := n.X
	if fun == nil {
		b.errorf(InvNone, "a call with no function")
		return nil
	}
	args := make([]*Value, 0, len(n.Args)+2)
	op := OpStaticCall
	var callee *ir.Object
	switch {
	case fun.Op == ir.OGlobal && fun.Obj != nil && fun.Obj.Class == ir.ClassFunc:
		callee = fun.Obj
	case fun.Type != nil && fun.Type.Kind == ir.FuncKind:
		op = OpClosureCall
		args = append(args, b.expr(fun))
	default:
		b.unsupported(n, "an indirect call")
		return nil
	}
	// The IR has already introduced temporaries for the order of evaluation
	// the specification requires, so the arguments are built in order here and
	// nothing reorders them.
	for _, a := range n.Args {
		args = append(args, b.expr(a))
	}
	if b.err != nil {
		return nil
	}
	args = append(args, b.memory())
	c := b.value(op, MemType, n.Pos, args...)
	c.Aux = callee
	b.setMemory(c)
	return c
}

// String concatenation.

// maxConcatOperands is the largest concatenation the runtime has a symbol for.
//
// concatstring5 is the last of the family, so a longer chain becomes nested
// calls. That is correct and it allocates once per call: runtime.concatstrings
// takes the operands as a slice and would do it in one, and rtsym does not
// carry it.
const maxConcatOperands = 5

// concatSyms names the runtime symbol for each operand count. Indexed by the
// count, so the entries below two are empty and unreachable.
var concatSyms = [maxConcatOperands + 1]string{
	2: "runtime.concatstring2",
	3: "runtime.concatstring3",
	4: "runtime.concatstring4",
	5: "runtime.concatstring5",
}

// isStringConcat reports whether n is the + of specs/020-ir.md's string
// concatenation row.
func isStringConcat(n ir.Expr) bool {
	return n != nil && n.Op == ir.OBinary && n.Op1 == syntax.Add &&
		n.Type != nil && n.Type.Kind == ir.String
}

// concat builds the runtime call specs/020-ir.md's table gives to string
// concatenation.
//
// It is here and not in lowering, which is the correction this function is.
// The table says the construct becomes runtime.concatstring{2,3,4,5}, and a +
// over two strings that reaches the operation set instead is a Go construct
// that survived into SSA, which specs/002-architecture.md forbids. Lowering
// could have compensated, and it would have been the wrong place: a
// concatenation needs no machine fact and no decomposed part, only the symbol
// and the operands, and both are here.
//
// A chain is one call rather than one call per +. a + b + c allocates twice as
// two calls and once as one, and the runtime has a symbol for each width up to
// five. Beyond that the chain nests, because the flattening is bounded by what
// rtsym carries rather than by what the parser produced.
//
// The temporary buffer is nil, which asks the runtime to allocate the result.
// A buffer in the frame is what makes a concatenation that does not escape
// allocation free, and it needs the escape analysis of
// specs/023-escape-analysis.md to say which concatenation that is.
func (b *builder) concat(n ir.Expr) *Value {
	parts := []ir.Expr{n.X, n.Y}
	if concatLen(n) <= maxConcatOperands {
		parts = concatParts(n, nil)
	}
	// The operands are evaluated in source order, before the call, because Go
	// evaluates the operands of an expression left to right and a call in one
	// of them is observable.
	args := make([]*Value, 0, len(parts)+2)
	args = append(args, b.value(OpConstNil, b.ptrTo(nil), n.Pos))
	for _, p := range parts {
		args = append(args, b.expr(p))
	}
	if b.err != nil {
		return b.zeroValue(n.Type)
	}
	args = append(args, b.memory())
	c := b.value(OpStaticCall, MemType, n.Pos, args...)
	c.Aux = RuntimeFunc(concatSyms[len(parts)])
	b.setMemory(c)
	r := b.value(OpSelectN, n.Type, n.Pos, c)
	r.AuxInt = 0
	return r
}

// concatParts appends the operands of a concatenation chain, left to right.
func concatParts(n ir.Expr, out []ir.Expr) []ir.Expr {
	if !isStringConcat(n) {
		return append(out, n)
	}
	return concatParts(n.Y, concatParts(n.X, out))
}

// concatLen counts the operands of a chain, saturating at one past the largest
// symbol so that a chain of a thousand costs the bound rather than a thousand.
func concatLen(n ir.Expr) int {
	if !isStringConcat(n) {
		return 1
	}
	x := concatLen(n.X)
	if x > maxConcatOperands {
		return x
	}
	return x + concatLen(n.Y)
}

// Constants.

func (b *builder) constant(n ir.Expr) *Value {
	t := n.Type
	if t == nil {
		b.errorf(InvNone, "a constant with no type")
		return b.zeroValue(b.intType)
	}
	text := ""
	if n.Val != nil {
		text = n.Val.String()
	}
	// ir.Value carries only its Go syntax, so a number is parsed back from
	// the text. Reported as a finding: specs/022-optimization-passes.md cannot
	// fold what it cannot read, and a parse of a printed form is not the
	// interface a constant folder should be given.
	var v *Value
	switch {
	case t.Kind == ir.Bool:
		v = b.value(OpConstBool, t, n.Pos)
		if text == "true" {
			v.AuxInt = 1
		} else if text != "false" {
			v.Aux = n.Val
		}
	case t.Kind.IsInteger():
		v = b.value(OpConstInt, t, n.Pos)
		if i, err := strconv.ParseInt(text, 0, 64); err == nil {
			v.AuxInt = i
		} else if u, err := strconv.ParseUint(text, 0, 64); err == nil {
			v.AuxInt = int64(u)
		} else {
			v.Aux = n.Val
		}
	case t.Kind.IsFloat():
		v = b.value(OpConstFloat, t, n.Pos)
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			v.Aux = f
		} else {
			v.Aux = n.Val
		}
	case t.Kind == ir.String:
		v = b.value(OpConstString, t, n.Pos)
		if s, err := strconv.Unquote(text); err == nil {
			v.Aux = s
		} else {
			v.Aux = text
		}
	default:
		v = b.value(OpConstNil, t, n.Pos)
	}
	return v
}

func (b *builder) constInt(x int64, pos syntax.Pos) *Value {
	v := b.value(OpConstInt, b.intType, pos)
	v.AuxInt = x
	return v
}

// zeroValue returns the zero value of t.
//
// It goes at the start of the entry block. The entry block dominates every
// other, and the start of it is before every use, so one value serves a use
// anywhere without a dominance question.
func (b *builder) zeroValue(t *ir.Type) *Value {
	if t == nil {
		t = b.intType
	}
	if t == MemType {
		return b.initMem
	}
	op := OpConstNil
	switch {
	case t.Kind == ir.Bool:
		op = OpConstBool
	case t.Kind.IsInteger():
		op = OpConstInt
	case t.Kind.IsFloat():
		op = OpConstFloat
	case t.Kind == ir.String:
		op = OpConstString
	}
	e := b.f.Entry
	v := b.f.newValue(op, t, b.fn.Pos)
	v.Block = e
	at := 0
	if len(e.Values) > 0 && e.Values[0].Op == OpInitMem {
		at = 1
	}
	e.Values = append(e.Values, nil)
	copy(e.Values[at+1:], e.Values[at:])
	e.Values[at] = v
	return v
}

// ptrTo returns the pointer type to t.
//
// Cached, so that two addresses of one type share a type. The cache is looked
// up by key and never ranged over.
func (b *builder) ptrTo(t *ir.Type) *ir.Type {
	if t == nil {
		t = b.intType
	}
	if p, ok := b.ptrTypes[t]; ok {
		return p
	}
	p := &ir.Type{Kind: ir.Ptr, Size: ir.PtrSize, Align: ir.PtrSize, Elem: t, PtrBits: []byte{1}}
	b.ptrTypes[t] = p
	return p
}

// Operator tables.

func binaryOp(o syntax.Operator) (Op, bool) {
	switch o {
	case syntax.Add:
		return OpAdd, true
	case syntax.Sub:
		return OpSub, true
	case syntax.Mul:
		return OpMul, true
	case syntax.Div:
		return OpDiv, true
	case syntax.Rem:
		return OpMod, true
	case syntax.And:
		return OpAnd, true
	case syntax.Or:
		return OpOr, true
	case syntax.Xor:
		return OpXor, true
	case syntax.AndNot:
		return OpAndNot, true
	case syntax.Shl:
		return OpShl, true
	case syntax.Shr:
		return OpShr, true
	}
	return OpInvalid, false
}

// compareOp maps a comparison to the canonical operation and says whether the
// arguments must be exchanged.
//
// There is no Greater and no GreaterEqual in the operation set. One rewrite
// rule then covers both spellings of a comparison, which is worth more than
// the two operations cost.
func compareOp(o syntax.Operator) (op Op, swap, ok bool) {
	switch o {
	case syntax.Eql:
		return OpEq, false, true
	case syntax.Neq:
		return OpNeq, false, true
	case syntax.Lss:
		return OpLess, false, true
	case syntax.Leq:
		return OpLeq, false, true
	case syntax.Gtr:
		return OpLess, true, true
	case syntax.Geq:
		return OpLeq, true, true
	}
	return OpInvalid, false, false
}

// convertOp picks the machine conversion between two representations.
func convertOp(from, to *ir.Type) (Op, bool) {
	switch {
	case from.Kind.IsInteger() && to.Kind.IsInteger():
		switch {
		case to.Size > from.Size:
			if from.Kind.IsSigned() {
				return OpSignExt, true
			}
			return OpZeroExt, true
		case to.Size < from.Size:
			return OpTrunc, true
		default:
			return OpBitcast, true
		}
	case from.Kind.IsInteger() && to.Kind.IsFloat():
		return OpCvtIntToFloat, true
	case from.Kind.IsFloat() && to.Kind.IsInteger():
		return OpCvtFloatToInt, true
	case from.Kind.IsFloat() && to.Kind.IsFloat():
		return OpCvtFloatToFloat, true
	case isPointerShaped(from) && isPointerShaped(to):
		return OpBitcast, true
	case from.Kind == ir.Bool && to.Kind.IsInteger():
		return OpZeroExt, true
	}
	return OpInvalid, false
}

func isPointerShaped(t *ir.Type) bool {
	switch t.Kind {
	case ir.Ptr, ir.UnsafePtr, ir.Uintptr, ir.Map, ir.Chan, ir.FuncKind:
		return true
	}
	return false
}
