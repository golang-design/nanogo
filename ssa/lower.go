// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
)

// The rewrite engine and the lowering pass of specs/025-lowering-and-rules.md.
//
// A rule is a Go function, one per operation, that matches on the shape of a
// value's arguments and rewrites it. There is no DSL and no generator, which
// specs/025 asks for and gives the reason for: a generator written for twenty
// rules has twenty users' worth of bugs and no test corpus.
//
// # Termination
//
// The engine applies rules to a block's values until no rule applies. Two
// properties make that a finite process, and neither of them is a bound on the
// number of passes.
//
// A selection rule replaces a target-neutral operation. Its replacement holds
// machine operations and pseudo-operations only, never a target-neutral one,
// so the number of target-neutral values in the function strictly decreases at
// every application and never rises. applyRule asserts exactly that after every
// rule fires, so the property is checked rather than reviewed.
//
// A folding rule rewrites a machine operation whose argument is a machine
// operation, and it may only replace an argument by one of that argument's own
// arguments, or merge two values into one. The use-def graph is acyclic away
// from phis, and a rule of that shape strictly reduces the sum over values of
// the height of their arguments in it, so a chain of foldable operations
// collapses in as many steps as the chain is long.
//
// The pass cap below is therefore not the termination argument. It is the
// assertion that the argument holds: a rule that violates either property
// makes the engine crash and name the operation, rather than hang.
//
// # Liveness runs after this pass
//
// Some operations have no machine instruction and become calls, per
// specs/031-runtime-lowering.md. Lowering therefore introduces a call into a
// block that had none, and a call is a safepoint. That is why
// specs/027-liveness-and-stackmaps.md runs after lowering and not before: the
// set of safepoints is not known until this pass has finished.

// The pseudo-operations of specs/025-lowering-and-rules.md's table.
//
// They are not machine operations and they are not target-neutral either: they
// survive lowering and are resolved by register allocation, by the ABI, or by
// the stack map builder. They are numbered above the target-neutral set, which
// is what registerOpInfos exists for.
const (
	// OpSP is the stack pointer. It is the base of every frame-slot address.
	OpSP Op = opCount + iota
	// OpSB is the static base. It is the base of every global's address.
	OpSB
	// OpVarDef and OpVarKill bound the lifetime of a stack object. They sit in
	// the memory chain so that nothing moves across them.
	OpVarDef
	OpVarKill

	opPseudoEnd
)

func init() {
	registerOpInfos(OpSP, []opInfo{
		{name: "SP", argLen: 0},
		{name: "SB", argLen: 0},
		{name: "VarDef", argLen: 1, takesMem: true, makesMem: true},
		{name: "VarKill", argLen: 1, takesMem: true, makesMem: true},
	})
}

// opNoRow is the first operation number that has no row in either table.
//
// It is what "an operation outside the table" means once a target has
// registered its machine operations: opCount was that number before this file
// existed, and the pseudo-operations above it now have rows.
func opNoRow() Op { return Op(len(opInfos) + len(extraOpInfos)) }

// NumNeutralOps is one more than the largest target-neutral operation. A rule
// table is indexed by operation, and this is the length the target-neutral
// part of one has.
const NumNeutralOps = int(opCount)

// FlagsType is the type of a condition-flags value.
//
// The flags are not a Go type, so like MemType they are identified by pointer
// and the name exists only for a dump. The kind is Bool for one reason worth
// stating: Verify requires the control value of a BlockIf to have Bool kind,
// and after lowering that control is the flags the machine actually branches
// on. Size is zero, so a pass that asks how much storage the value needs gets
// the right answer, and IsFlags is how the register allocator knows the value
// gets no register.
var FlagsType = &ir.Type{Kind: ir.Bool, Size: 0, Align: 1, Name: "flags"}

// IsFlags reports whether v is a condition-flags value.
//
// A flags value is live only inside the block that produced it, because every
// instruction in between may write the condition codes. A rule that folds a
// comparison into a use must check that both are in one block.
func IsFlags(v *Value) bool { return v != nil && v.Type == FlagsType }

// ValueRule lowers one value and reports whether it changed the graph.
//
// The engine calls the rule of v.Op. A rule for a target-neutral operation
// selects a machine operation for it. A rule for a machine operation folds one
// of its arguments into it, which is where address modes come from.
type ValueRule func(v *Value, e *Edit) bool

// BlockRule lowers the control transfer of one block.
type BlockRule func(b *Block, e *Edit) bool

// RuleSet is one target's rules, one function per operation.
//
// Value is indexed by Op and covers both halves of the pass: the entries below
// NumNeutralOps select machine operations, and the entries above it fold. A nil
// entry below NumNeutralOps is a missing rule, which is what the coverage
// report of specs/025 counts.
type RuleSet struct {
	Name  string
	Value []ValueRule
	Block BlockRule

	// Machine reports whether op is a machine operation of this target.
	Machine func(Op) bool

	// Essential reports whether a value with this operation must be kept even
	// when nothing uses its result. A nil-check load is the example: it exists
	// to fault, and its result is often dead.
	Essential func(Op) bool
}

// Rule returns the rule for op, or nil.
func (rs *RuleSet) Rule(op Op) ValueRule {
	if int(op) < len(rs.Value) {
		return rs.Value[op]
	}
	return nil
}

// IsLowered reports whether op may survive the pass: a machine operation of
// this target, a phi, or one of the pseudo-operations.
func (rs *RuleSet) IsLowered(op Op) bool {
	return isPseudo(op) || (rs.Machine != nil && rs.Machine(op))
}

// IsPseudoOp reports whether op is one of the operations that survive lowering
// without being a machine operation. The rule-coverage report of specs/025
// needs it: a pseudo-operation has no rule and needs none.
func IsPseudoOp(op Op) bool { return isPseudo(op) }

// isPseudo reports whether op is one of the operations that survive lowering.
//
// specs/025's table lists Phi, Arg, Copy, SP, SB, VarDef and VarKill. Three
// more are here, and each is a gap in that table rather than a liberty taken:
//
//   - InitMem is the root of the memory chain. It is not an instruction and
//     nothing can lower it.
//   - SelectN names a call result in its ABI location. It is the mirror of Arg
//     on the other side of a call, and it is resolved by specs/030-abi.md at
//     the same time and for the same reason.
//   - MakeResult is not here. It has a machine form, and the arm64 rules give
//     it one, because the return is an instruction.
func isPseudo(op Op) bool {
	switch op {
	case OpPhi, OpArg, OpCopy, OpSP, OpSB, OpVarDef, OpVarKill, OpInitMem, OpSelectN:
		return true
	}
	return false
}

// maxBlockPasses bounds the number of times the engine walks one block.
//
// Reaching it means a rule fired without moving towards machine form, which is
// a compiler bug and not a program that is hard to compile: the two properties
// in the file comment bound the real number of passes by the length of the
// longest foldable chain in the block, which no real block comes near.
const maxBlockPasses = 100

// LowerError is a bug in the pass or in a rule set. It is a panic, not a
// returned error, because specs/025 requires a crash: a function that is
// missing an operation is the hardest class of bug in the compiler to find,
// and a silent fallback here produces exactly that.
type LowerError struct {
	Func   string
	Block  ID
	Value  ID
	Op     Op
	Detail string
}

func (e *LowerError) Error() string {
	return fmt.Sprintf("ssa: lower: %s: b%d v%d: %v: %s", e.Func, e.Block, e.Value, e.Op, e.Detail)
}

// Lower rewrites every value of f into rs's machine operations.
//
// It panics with a *LowerError when an operation has no rule, when a rule
// leaves a target-neutral operation behind, or when a block does not converge.
// specs/025 requires the crash and names the operation in it.
func Lower(f *Func, rs *RuleSet) {
	l := &lowerer{f: f, rs: rs}
	l.run()
}

type lowerer struct {
	f  *Func
	rs *RuleSet

	edit Edit

	// exitMem holds the memory a block leaves with, indexed by block
	// identifier. It is filled as the blocks are walked, in reverse postorder,
	// so a block's predecessors are done before it is.
	exitMem []*Value

	sp, sb *Value

	// ptr is the type of a machine pointer, made once. Lowering computes
	// addresses that no Go type describes, such as the address of a frame slot
	// that holds several objects, and they are all one machine word.
	ptr *ir.Type
}

func (l *lowerer) fail(b *Block, v *Value, op Op, format string, args ...any) {
	bid, vid := ID(-1), ID(-1)
	if b != nil {
		bid = b.ID
	}
	if v != nil {
		vid = v.ID
	}
	panic(&LowerError{Func: l.f.Name, Block: bid, Value: vid, Op: op, Detail: fmt.Sprintf(format, args...)})
}

func (l *lowerer) run() {
	f := l.f
	l.exitMem = make([]*Value, f.NumBlocks()+8)
	l.edit.l = l

	queue := Dominators(f).ReversePostorder()
	// A split adds a block that holds the tail of the one it split, and that
	// tail still has to be lowered. The queue grows rather than being walked
	// twice, and the cap below is the assertion that it stops growing.
	visits, maxVisits := 0, 8*(len(f.Blocks)+16)
	for i := 0; i < len(queue); i++ {
		b := queue[i]
		visits++
		if visits > maxVisits {
			l.fail(b, nil, OpInvalid, "the block queue did not drain after %d visits", visits)
		}
		if cont := l.block(b); cont != nil {
			// The tail of b is now in cont, and the rest of b's rules may
			// still have work, so both go back in front of the queue.
			queue = append(queue, nil)
			copy(queue[i+2:], queue[i+1:])
			queue[i+1] = cont
			i--
		}
	}
	l.finishBlocks()
	l.deadValues()
	l.check()
}

// block lowers one block to a fixed point.
//
// It returns the continuation block when a rule split the block, because the
// values after the split are in that block now and have not been lowered.
func (l *lowerer) block(b *Block) *Block {
	e := &l.edit
	for pass := 0; ; pass++ {
		if pass >= maxBlockPasses {
			l.fail(b, nil, OpInvalid, "no fixed point after %d passes, last rule was %v", pass, e.last)
		}
		changed := false
		mem := l.incomingMem(b)
		for i := 0; i < len(b.Values); {
			v := b.Values[i]
			if v.Op == OpPhi {
				i++
				continue
			}
			e.reset(b, i, mem)
			r := l.rs.Rule(v.Op)
			if r != nil && r(v, e) {
				changed = true
				l.checkRule(b, v, e)
				if e.split != nil {
					l.exitMem[b.ID] = e.mem
					return e.split
				}
				if e.removed {
					i = e.pos
				} else {
					i = e.pos + 1
				}
				// The rewritten value replaces the memory that was live, if
				// it is still a memory value. Carrying the old one forward
				// would give a later rule the memory from before the store it
				// is standing after, and a call built with it breaks the chain
				// that orders every side effect in the function.
				mem = e.mem
				if !v.dead && infoOf(v.Op).makesMem {
					mem = v
				}
				continue
			}
			if infoOf(v.Op).makesMem {
				mem = v
			}
			i++
		}
		if !changed {
			l.exitMem[b.ID] = mem
			return nil
		}
	}
}

// checkRule asserts the property the termination argument rests on: a rule
// leaves no target-neutral operation behind, in the value it rewrote or in the
// values it created.
func (l *lowerer) checkRule(b *Block, v *Value, e *Edit) {
	if !v.dead && !l.rs.IsLowered(v.Op) {
		l.fail(b, v, v.Op, "the rule left a target-neutral operation behind")
	}
	for _, w := range e.created {
		if !l.rs.IsLowered(w.Op) {
			l.fail(b, w, w.Op, "the rule created a target-neutral operation")
		}
	}
}

// finishBlocks lowers each block's control transfer, once its values have
// reached a fixed point. The control of an If block is a comparison, and the
// comparison has to be lowered before the branch that reads it can be.
func (l *lowerer) finishBlocks() {
	if l.rs.Block == nil {
		return
	}
	e := &l.edit
	for _, b := range l.f.Blocks {
		if b.Kind != BlockIf {
			continue
		}
		e.reset(b, len(b.Values), l.exitMem[b.ID])
		if l.rs.Block(b, e) {
			l.checkRule(b, b.Control, e)
		}
	}
}

// check is the assertion of specs/025: after the pass, every value is a
// machine operation, a phi, or a pseudo-operation.
func (l *lowerer) check() {
	for _, b := range l.f.Blocks {
		for _, v := range b.Values {
			if !l.rs.IsLowered(v.Op) {
				l.fail(b, v, v.Op, "no %s rule lowered this operation", l.rs.Name)
			}
		}
	}
}

// incomingMem returns the memory value live at the start of b.
//
// It is the block's memory phi when it has one, and otherwise the memory a
// predecessor left with. The rule is the one Verify checks, so a block that
// disagrees with it is already a violation before this pass runs.
func (l *lowerer) incomingMem(b *Block) *Value {
	for _, v := range b.Values {
		if v.Op != OpPhi {
			break
		}
		if IsMemory(v) {
			return v
		}
	}
	for _, p := range b.Preds {
		if int(p.ID) < len(l.exitMem) && l.exitMem[p.ID] != nil {
			return l.exitMem[p.ID]
		}
	}
	return nil
}

// deadValues removes the values that folding orphaned.
//
// specs/022-optimization-passes.md owns dead code elimination in general. This
// is the narrow part of it that belongs here: an address computation that a
// load absorbed has no user left, and leaving it would make the lowered form
// of a rule impossible to assert and would cost a register.
func (l *lowerer) deadValues() {
	f := l.f
	for {
		used := make([]bool, f.NumValues())
		for _, b := range f.Blocks {
			if b.Control != nil {
				used[b.Control.ID] = true
			}
			for _, v := range b.Values {
				for _, a := range v.Args {
					if a != nil {
						used[a.ID] = true
					}
				}
			}
		}
		removed := false
		for _, b := range f.Blocks {
			kept := b.Values[:0]
			for _, v := range b.Values {
				if !used[v.ID] && l.removable(v) {
					v.dead = true
					removed = true
					continue
				}
				kept = append(kept, v)
			}
			b.Values = kept
		}
		if !removed {
			return
		}
	}
}

// removable reports whether a value with no user may be dropped.
func (l *lowerer) removable(v *Value) bool {
	info := infoOf(v.Op)
	if info.makesMem || info.call {
		return false
	}
	switch v.Op {
	case OpArg, OpSP, OpSB:
		// An argument names an ABI location whether or not the body reads it,
		// and the two base pointers are read by the frame layout.
		return false
	}
	return l.rs.Essential == nil || !l.rs.Essential(v.Op)
}

// Edit is the graph surgery a rule is allowed to do.
//
// A rule receives one, and every change it makes goes through it, so the
// engine knows what a rule created and can check it.
type Edit struct {
	l   *lowerer
	b   *Block
	pos int
	mem *Value

	created []*Value
	removed bool
	split   *Block
	last    Op
}

func (e *Edit) reset(b *Block, pos int, mem *Value) {
	e.b = b
	e.pos = pos
	e.mem = mem
	e.created = e.created[:0]
	e.removed = false
	e.split = nil
}

// Mem returns the memory value live at the value being rewritten.
//
// A rule that introduces a call needs it, because the call takes memory and
// the operation it replaces may not have taken any. Division is the case:
// nothing about a divide names memory, and the panic it can reach is a call.
func (e *Edit) Mem() *Value { return e.mem }

// Insert adds a value immediately before the one being rewritten.
func (e *Edit) Insert(pos syntax.Pos, op Op, t *ir.Type, args ...*Value) *Value {
	v := e.newValue(pos, op, t, args)
	insertAt(e.b, e.pos, v)
	e.pos++
	return v
}

// InsertBefore adds a value immediately before at, wherever at is.
//
// A rule needs it after Guard, which moves the value being rewritten into a
// new block: the values that go with it have to follow it there.
func (e *Edit) InsertBefore(at *Value, op Op, t *ir.Type, args ...*Value) *Value {
	b := at.Block
	i := 0
	for j, w := range b.Values {
		if w == at {
			i = j
			break
		}
	}
	v := e.newValue(at.Pos, op, t, args)
	v.Block = b
	insertAt(b, i, v)
	if b == e.b && i <= e.pos {
		e.pos++
	}
	return v
}

func (e *Edit) newValue(pos syntax.Pos, op Op, t *ir.Type, args []*Value) *Value {
	v := e.l.f.newValue(op, t, pos)
	v.Block = e.b
	for _, a := range args {
		v.AddArg(a)
	}
	e.created = append(e.created, v)
	e.last = op
	return v
}

func insertAt(b *Block, i int, v *Value) {
	v.Block = b
	b.Values = append(b.Values, nil)
	copy(b.Values[i+1:], b.Values[i:])
	b.Values[i] = v
}

// Set replaces the operation and the arguments of v in place.
//
// It is the common case by a wide margin: a machine operation with the same
// argument shape and the same result type needs no new value, no use to be
// redirected, and no edit to the memory chain.
func (e *Edit) Set(v *Value, op Op, args ...*Value) *Value {
	v.Op = op
	v.AuxInt = 0
	v.Aux = nil
	v.Args = v.Args[:0]
	for _, a := range args {
		v.AddArg(a)
	}
	e.last = op
	return v
}

// SetArg replaces one argument of v.
func (e *Edit) SetArg(v *Value, i int, a *Value) {
	v.SetArg(i, a)
	e.last = v.Op
}

// Replace redirects every use of old, in the whole function, to fresh.
//
// Function-wide and not block-local: Value.uses is construction bookkeeping
// and is documented as stale, so the graph is the only reliable answer to who
// reads a value. A memory value that is removed has readers in later blocks,
// which is exactly the case that a block-local scan gets wrong.
func (e *Edit) Replace(old, fresh *Value) {
	if old == fresh {
		return
	}
	for _, b := range e.l.f.Blocks {
		for _, v := range b.Values {
			for i, a := range v.Args {
				if a == old {
					v.SetArg(i, fresh)
				}
			}
		}
		if b.Control == old {
			b.Control = fresh
		}
	}
}

// Remove deletes the value being rewritten from its block.
func (e *Edit) Remove(v *Value) {
	for i, w := range e.b.Values {
		if w == v {
			e.b.Values = append(e.b.Values[:i], e.b.Values[i+1:]...)
			v.dead = true
			if i < e.pos {
				e.pos--
			}
			e.removed = true
			return
		}
	}
}

// NewBlock adds a block to the function, after the one being lowered.
func (e *Edit) NewBlock(kind BlockKind) *Block {
	b := e.l.f.NewBlock(kind)
	b.Pos = e.b.Pos
	b.sealed = true
	// NewBlock appends to the layout. Move it next to the block it belongs
	// with, so that a dump reads in control-flow order.
	f := e.l.f
	f.Blocks = f.Blocks[:len(f.Blocks)-1]
	at := len(f.Blocks)
	for i, c := range f.Blocks {
		if c == e.b {
			at = i + 1
			break
		}
	}
	f.Blocks = append(f.Blocks, nil)
	copy(f.Blocks[at+1:], f.Blocks[at:])
	f.Blocks[at] = b
	if int(b.ID) >= len(e.l.exitMem) {
		grown := make([]*Value, int(b.ID)+8)
		copy(grown, e.l.exitMem)
		e.l.exitMem = grown
	}
	return b
}

// Check turns the value v into a two-way branch on control.
//
// The block is cut after v. The values that followed it move to a new
// continuation block, which takes the original block's kind, control and
// successors. The original block becomes an If whose first successor is the
// continuation and whose second is a new block the caller fills with the
// failure path. specs/021-ssa-construction.md's checks become this, and
// specs/031-runtime-lowering.md's no-return symbols end the failure block.
//
// v is removed and every use of it is redirected to the memory it took, which
// is what keeps the memory chain single after a check that produced memory
// disappears.
func (e *Edit) Check(v *Value, control *Value) (fail, cont *Block) {
	return e.cut(v, control, false)
}

// Guard is Check for a value that must survive.
//
// The cut is made in front of v rather than behind it, so v moves to the
// continuation block and is reached only when the guard passed. A divide is
// this shape: the check is about the divide's own argument, and the divide
// still has to happen.
func (e *Edit) Guard(v *Value, control *Value) (fail, cont *Block) {
	return e.cut(v, control, true)
}

func (e *Edit) cut(v *Value, control *Value, keep bool) (fail, cont *Block) {
	b := e.b
	f := e.l.f

	cont = e.NewBlock(b.Kind)
	fail = e.NewBlock(BlockExit)
	// The continuation must come first in the layout, so that the failure
	// block does not sit between a block and its fall-through.
	for i, c := range f.Blocks {
		if c == fail {
			copy(f.Blocks[i:], f.Blocks[i+1:])
			f.Blocks[len(f.Blocks)-1] = fail
			break
		}
	}

	at := 0
	for i, w := range b.Values {
		if w == v {
			at = i
			if !keep {
				at = i + 1
			}
			break
		}
	}
	cont.Values = append(cont.Values, b.Values[at:]...)
	for _, w := range cont.Values {
		w.Block = cont
	}
	b.Values = b.Values[:at]

	cont.Kind, cont.Control = b.Kind, b.Control
	cont.Succs = b.Succs
	for i, s := range cont.Succs {
		// The edge moved from b to cont. Slot i of a successor's predecessor
		// list and argument i of every phi in it are the same edge, so the
		// slot is overwritten rather than appended to.
		j := predIndexOf(b, cont.Succs, i, s)
		if j >= 0 {
			s.Preds[j] = cont
		}
	}
	b.Succs = nil
	b.Kind = BlockIf
	b.Control = control
	b.AddEdgeTo(cont)
	b.AddEdgeTo(fail)

	if !keep {
		if IsMemory(v) {
			e.Replace(v, v.MemArg())
		}
		e.Remove(v)
	}
	e.pos = len(b.Values)
	e.split = cont
	return fail, cont
}

// predIndexOf finds the slot of the edge from b to succs[i] in the successor's
// predecessor list. It is predIndex with the successor list passed in, because
// the list has already moved off b.
func predIndexOf(b *Block, succs []*Block, i int, s *Block) int {
	k := 0
	for j := 0; j < i; j++ {
		if succs[j] == s {
			k++
		}
	}
	n := 0
	for j, p := range s.Preds {
		if p == b {
			if n == k {
				return j
			}
			n++
		}
	}
	return -1
}

// SP returns the stack pointer value, creating it once per function.
func (e *Edit) SP() *Value { return e.base(&e.l.sp, OpSP) }

// SB returns the static base value, creating it once per function.
func (e *Edit) SB() *Value { return e.base(&e.l.sb, OpSB) }

func (e *Edit) base(slot **Value, op Op) *Value {
	if *slot != nil && !(*slot).dead {
		return *slot
	}
	f := e.l.f
	v := f.newValue(op, e.PtrType(), f.Entry.Pos)
	v.Block = f.Entry
	// At the front of the entry block, after the phis it cannot have, so that
	// it dominates every use.
	f.Entry.Values = append(f.Entry.Values, nil)
	copy(f.Entry.Values[1:], f.Entry.Values)
	f.Entry.Values[0] = v
	if e.b == f.Entry {
		e.pos++
	}
	*slot = v
	return v
}

// PtrType returns the type of an untyped machine pointer.
func (e *Edit) PtrType() *ir.Type {
	if e.l.ptr == nil {
		e.l.ptr = &ir.Type{Kind: ir.UnsafePtr, Size: ir.PtrSize, Align: ir.PtrSize, Name: "unsafe.Pointer"}
	}
	return e.l.ptr
}

// CheckLowered reports the values that are still target-neutral.
//
// Verify checks the invariants that hold before and after this pass.
// specs/025 adds one that holds only after it, and this is it, kept separate
// so that Verify stays the checker of construction.
func CheckLowered(f *Func, rs *RuleSet) []Violation {
	var out []Violation
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !rs.IsLowered(v.Op) {
				out = append(out, Violation{
					Invariant: InvOpForm,
					Block:     b.ID,
					Value:     v.ID,
					Detail:    fmt.Sprintf("%v is not a %s machine operation", v.Op, rs.Name),
				})
			}
		}
	}
	return out
}
