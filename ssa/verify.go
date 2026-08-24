// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"fmt"
	"strings"

	"golang.design/x/nanogo/ir"
)

// The invariant checker.
//
// specs/021-ssa-construction.md states what this is for, and it is not
// debugging: it is the reason a miscompile is found in the pass that caused it
// rather than in the register allocator. It runs after construction and after
// every pass in a test build.
//
// Verify collects violations rather than stopping at the first one, and every
// violation names which invariant it broke. A pass that breaks one invariant
// usually breaks a second one as a consequence, and a checker that reported
// only "invalid function" would let a test claiming to exercise one invariant
// pass on the strength of another.

// Invariant identifies one of the properties Verify checks.
type Invariant uint8

const (
	// InvNone is not an invariant. It marks a build error that is not a
	// violation, such as an unsupported construct.
	InvNone Invariant = iota

	// InvTyped: every value has a type.
	InvTyped

	// InvOpForm: a value matches the shape its operation declares. The
	// argument count, the memory argument last, a memory result on a memory
	// operation, phis at the start of the block, and each value in the block
	// that claims it.
	InvOpForm

	// InvBlockControl: every block has exactly one control value, or none if
	// it has one successor, and its successor count matches its kind.
	InvBlockControl

	// InvPhiArity: every phi has one argument per predecessor, in predecessor
	// order.
	InvPhiArity

	// InvArgDominates: every value's arguments dominate it, except phi
	// arguments, which dominate the corresponding predecessor's exit.
	InvArgDominates

	// InvMemChain: exactly one memory value is live at any point in a block.
	InvMemChain

	// InvReachable: no value is unreachable from the entry block.
	InvReachable

	// InvGoSpecific: no Go-specific operation of specs/020-ir.md's lowering
	// table survives into SSA.
	InvGoSpecific
)

var invariantNames = [...]string{
	InvNone:         "none",
	InvTyped:        "every value has a type",
	InvOpForm:       "a value matches the shape of its operation",
	InvBlockControl: "one control value, or none with one successor",
	InvPhiArity:     "one phi argument per predecessor, in order",
	InvArgDominates: "arguments dominate their use",
	InvMemChain:     "one memory value live at any point in a block",
	InvReachable:    "every value is reachable from the entry",
	InvGoSpecific:   "no Go-specific operation survives",
}

func (i Invariant) String() string {
	if int(i) < len(invariantNames) && invariantNames[i] != "" {
		return invariantNames[i]
	}
	return "invariant(?)"
}

// Violation is one broken invariant.
//
// Block and Value are identifiers rather than pointers so that a violation can
// be compared and printed without the graph it came from.
type Violation struct {
	Invariant Invariant
	Block     ID
	Value     ID // -1 when the violation is about the block itself
	Detail    string
}

func (v Violation) String() string {
	where := fmt.Sprintf("b%d", v.Block)
	if v.Value >= 0 {
		where = fmt.Sprintf("b%d v%d", v.Block, v.Value)
	}
	return fmt.Sprintf("%s: %s: %s", where, v.Invariant, v.Detail)
}

// isArithmetic reports whether o computes a value from operands of its own
// type, rather than moving one or comparing two.
//
// It is the set an operation over a string can only be a Go construct in. A
// load, a phi, a call argument and a comparison all take a string legitimately
// and are not here.
func isArithmetic(o Op) bool {
	switch o {
	case OpAdd, OpSub, OpMul, OpDiv, OpMod,
		OpAnd, OpOr, OpXor, OpAndNot, OpShl, OpShr, OpNeg, OpCom:
		return true
	}
	return false
}

// Error is a failure reported by Build or by Check.
//
// Invariant names which property failed, and is InvNone when the failure is
// not a broken invariant.
type Error struct {
	Func       string
	Invariant  Invariant
	Detail     string
	Violations []Violation
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ssa: %s: ", e.Func)
	if e.Invariant != InvNone {
		fmt.Fprintf(&b, "%s: ", e.Invariant)
	}
	b.WriteString(e.Detail)
	for _, v := range e.Violations {
		fmt.Fprintf(&b, "\n\t%v", v)
	}
	return b.String()
}

// Check runs Verify and returns the violations as an error.
func Check(f *Func) error {
	vs := Verify(f)
	if len(vs) == 0 {
		return nil
	}
	return &Error{
		Func:       f.Name,
		Invariant:  vs[0].Invariant,
		Detail:     fmt.Sprintf("%d violations", len(vs)),
		Violations: vs,
	}
}

// Verify checks every invariant of specs/021-ssa-construction.md and returns
// what it found.
//
// The result is deterministic: blocks are visited in layout order and values in
// block order, and nothing is read from a map.
func Verify(f *Func) []Violation {
	v := &verifier{f: f, dom: Dominators(f)}
	v.checkEntry()
	// Index of each value within its block, for the same-block part of the
	// dominance check. Sized by NumValues, which is what dense identifiers are
	// for.
	v.index = make([]int32, f.NumValues())
	v.owner = make([]*Block, f.NumValues())
	for i := range v.index {
		v.index[i] = -1
	}
	for _, b := range f.Blocks {
		for i, val := range b.Values {
			if int(val.ID) < len(v.index) {
				v.index[val.ID] = int32(i)
				v.owner[val.ID] = b
			}
		}
	}
	v.memory()
	for _, b := range f.Blocks {
		if !v.dom.Reachable(b) {
			// One violation for the block, then nothing else about it. Every
			// other check would fail as a consequence and would say nothing
			// new: a value in an unreachable block dominates nothing.
			v.addBlock(InvReachable, b, "block is not reachable from the entry")
			continue
		}
		v.checkBlock(b)
		for _, val := range b.Values {
			v.checkValue(b, val)
		}
	}
	return v.out
}

type verifier struct {
	f     *Func
	dom   *DomTree
	out   []Violation
	index []int32
	owner []*Block

	// entryMem and exitMem hold the memory value live at the start and at the
	// end of each block, indexed by block identifier.
	entryMem []*Value
	exitMem  []*Value
	memKnown []bool
}

func (v *verifier) add(inv Invariant, b *Block, val *Value, format string, args ...any) {
	v.out = append(v.out, Violation{
		Invariant: inv,
		Block:     b.ID,
		Value:     val.ID,
		Detail:    fmt.Sprintf(format, args...),
	})
}

func (v *verifier) addBlock(inv Invariant, b *Block, format string, args ...any) {
	v.out = append(v.out, Violation{
		Invariant: inv,
		Block:     b.ID,
		Value:     -1,
		Detail:    fmt.Sprintf(format, args...),
	})
}

func (v *verifier) checkEntry() {
	if v.f.Entry == nil {
		v.out = append(v.out, Violation{Invariant: InvReachable, Block: -1, Value: -1, Detail: "function has no entry block"})
		return
	}
	if len(v.f.Entry.Preds) != 0 {
		v.addBlock(InvReachable, v.f.Entry, "entry block has %d predecessors", len(v.f.Entry.Preds))
	}
}

// checkBlock checks the control value, the kind, and the edges.
func (v *verifier) checkBlock(b *Block) {
	want, ok := b.Kind.NumSuccs()
	if !ok {
		v.addBlock(InvBlockControl, b, "block kind %v", b.Kind)
		return
	}
	if len(b.Succs) != want {
		v.addBlock(InvBlockControl, b, "%v block with %d successors, want %d", b.Kind, len(b.Succs), want)
	}
	switch {
	case want == 1 && b.Control != nil:
		v.addBlock(InvBlockControl, b, "%v block with one successor has a control value %v", b.Kind, b.Control)
	case want != 1 && b.Control == nil:
		v.addBlock(InvBlockControl, b, "%v block has no control value", b.Kind)
	}
	if b.Control != nil && b.Control.dead {
		v.addBlock(InvBlockControl, b, "control value %v is not in the function", b.Control)
	}
	if b.Kind == BlockIf && b.Control != nil && b.Control.Type != nil && b.Control.Type.Kind != ir.Bool {
		v.addBlock(InvBlockControl, b, "If block controlled by %v of type %v", b.Control, b.Control.Type)
	}
	if (b.Kind == BlockRet || b.Kind == BlockExit) && b.Control != nil && !IsMemory(b.Control) {
		v.addBlock(InvBlockControl, b, "%v block controlled by %v, which is not memory", b.Kind, b.Control)
	}
	for i, s := range b.Succs {
		if predIndex(b, i) < 0 {
			v.addBlock(InvBlockControl, b, "successor %v does not list %v as a predecessor", s, b)
		}
	}
}

func (v *verifier) checkValue(b *Block, val *Value) {
	if val.Type == nil {
		v.add(InvTyped, b, val, "value has no type")
	}
	if val.Block != b {
		v.add(InvOpForm, b, val, "value claims to be in %v", val.Block)
	}
	if op, ok := val.Aux.(ir.Op); ok && op.IsGoSpecific() {
		v.add(InvGoSpecific, b, val, "operation %v reached SSA", op)
	}
	if isArithmetic(val.Op) && val.Type != nil && val.Type.Kind == ir.String {
		// Concatenation is the only arithmetic Go has over a string, and
		// specs/020-ir.md's table makes it runtime.concatstring{2,3,4,5}. An
		// Add of two strings here is that call not built, which
		// specs/002-architecture.md forbids: no Go construct survives into
		// SSA. It cost the corpus 49 functions before the builder built it,
		// and every one of them looked like a lowering gap.
		//
		// The comparison rows of the same table are deliberately not checked.
		// They become calls too, and lowering is where they become them,
		// because runtime.memequal takes the data pointer and the length of a
		// string and neither exists as a value until decomposition splits it.
		// This invariant holds from construction onwards, so it can only name
		// what the builder itself owes.
		v.add(InvGoSpecific, b, val, "%v over a string reached SSA; specs/020's table makes it a runtime call", val.Op)
	}

	info := infoOf(val.Op)
	if info.name == "" {
		v.add(InvOpForm, b, val, "unknown operation %d", val.Op)
		return
	}
	if info.argLen >= 0 && len(val.Args) != int(info.argLen) {
		v.add(InvOpForm, b, val, "%v with %d arguments, want %d", val.Op, len(val.Args), info.argLen)
	}
	if info.makesMem && !IsMemory(val) {
		v.add(InvOpForm, b, val, "%v produces memory but has type %v", val.Op, val.Type)
	}
	for i, a := range val.Args {
		if a == nil {
			v.add(InvOpForm, b, val, "argument %d is nil", i)
			continue
		}
		if a.dead {
			v.add(InvOpForm, b, val, "argument %d is %v, which is not in the function", i, a)
		}
		isMem := IsMemory(a)
		wantMem := info.takesMem && i == len(val.Args)-1
		if isMem != wantMem && val.Op != OpPhi {
			v.add(InvOpForm, b, val, "argument %d is %v, memory is %v but the operation wants memory %v", i, a, isMem, wantMem)
		}
	}

	if val.Op == OpPhi {
		v.checkPhi(b, val)
		return
	}
	for i, a := range val.Args {
		if a == nil || a.dead {
			continue
		}
		if !v.defDominatesUse(a, val) {
			v.add(InvArgDominates, b, val, "argument %d, %v, does not dominate its use", i, a)
		}
	}
}

func (v *verifier) checkPhi(b *Block, phi *Value) {
	if i := v.index[phi.ID]; i > 0 {
		if b.Values[i-1].Op != OpPhi {
			v.add(InvOpForm, b, phi, "phi is not at the start of the block")
		}
	}
	if len(phi.Args) != len(b.Preds) {
		v.add(InvPhiArity, b, phi, "phi has %d arguments and the block has %d predecessors", len(phi.Args), len(b.Preds))
		return
	}
	for i, a := range phi.Args {
		if a == nil || a.dead {
			continue
		}
		// The argument must dominate the exit of the predecessor it comes
		// from, not the phi's own block. A value defined in the predecessor
		// dominates that predecessor's exit.
		p := b.Preds[i]
		def := v.owner[a.ID]
		if def == nil {
			v.add(InvArgDominates, b, phi, "argument %d, %v, is in no block", i, a)
			continue
		}
		if !v.dom.Dominates(def, p) {
			v.add(InvArgDominates, b, phi, "argument %d, %v, does not dominate the exit of predecessor %v", i, a, p)
		}
		if IsMemory(phi) != IsMemory(a) {
			v.add(InvOpForm, b, phi, "argument %d, %v, mixes memory and non-memory in a phi", i, a)
		}
	}
}

// defDominatesUse reports whether the definition of a is in scope at use.
func (v *verifier) defDominatesUse(a, use *Value) bool {
	def := v.owner[a.ID]
	if def == nil {
		return false
	}
	if def == v.owner[use.ID] {
		// Within one block the order of the value list is the order of
		// execution, so the definition must come first.
		return v.index[a.ID] < v.index[use.ID]
	}
	return v.dom.StrictlyDominates(def, v.owner[use.ID])
}

// memory checks that exactly one memory value is live at any point in a block.
//
// The checkable form of that sentence is a single chain. The memory a block
// starts with is its memory phi, or the memory its one predecessor ended with.
// Every value that takes memory must take the memory that is current at that
// point, and every value that produces memory replaces it. A join with two
// predecessors that end with different memory needs a phi, exactly as any
// other variable does; specs/021-ssa-construction.md relies on that being
// ordinary.
func (v *verifier) memory() {
	f := v.f
	v.entryMem = make([]*Value, f.NumBlocks())
	v.exitMem = make([]*Value, f.NumBlocks())
	v.memKnown = make([]bool, f.NumBlocks())

	hasMemPhi := make([]bool, f.NumBlocks())

	for _, b := range v.dom.ReversePostorder() {
		var start *Value
		known := false

		var memPhi *Value
		for _, val := range b.Values {
			if val.Op != OpPhi {
				break
			}
			if IsMemory(val) {
				if memPhi != nil {
					v.add(InvMemChain, b, val, "second memory phi in one block, after %v", memPhi)
				}
				memPhi = val
			}
		}
		hasMemPhi[b.ID] = memPhi != nil

		switch {
		case memPhi != nil:
			start, known = memPhi, true
		case len(b.Preds) == 1:
			p := b.Preds[0]
			start, known = v.exitMem[p.ID], v.memKnown[p.ID]
		case len(b.Preds) > 1:
			// The first predecessor that is already computed supplies the
			// memory the block starts with. Whether the predecessors agree is
			// decided below, once every block has been walked.
			for _, p := range b.Preds {
				if v.memKnown[p.ID] {
					start, known = v.exitMem[p.ID], true
					break
				}
			}
		}

		cur, curKnown := start, known
		for _, val := range b.Values {
			if val.Op == OpPhi {
				continue
			}
			if infoOf(val.Op).takesMem && len(val.Args) > 0 {
				m := val.Args[len(val.Args)-1]
				switch {
				case !curKnown:
					// The entry block starts with no memory until InitMem
					// creates it. Adopt what the first reader used, so that
					// the rest of the block is still checked against it.
					cur, curKnown = m, true
				case m != cur:
					v.add(InvMemChain, b, val, "reads memory %v while %v is live", m, cur)
				}
			}
			if infoOf(val.Op).makesMem {
				cur, curKnown = val, true
			}
		}
		v.exitMem[b.ID], v.memKnown[b.ID] = cur, curKnown
	}

	// A join needs a memory phi when its predecessors leave with different
	// memory. This runs after the walk rather than inside it, because a loop
	// header is reached before its back edge is: checking during the walk
	// would skip the one predecessor that carries the memory the body wrote,
	// and a loop that writes memory and has no memory phi would pass.
	for _, b := range v.dom.ReversePostorder() {
		if len(b.Preds) < 2 || hasMemPhi[b.ID] {
			continue
		}
		var first *Value
		known := false
		for _, p := range b.Preds {
			if !v.memKnown[p.ID] {
				continue
			}
			if !known {
				first, known = v.exitMem[p.ID], true
				continue
			}
			if v.exitMem[p.ID] != first {
				v.addBlock(InvMemChain, b, "predecessors leave with %v and %v and the block has no memory phi", first, v.exitMem[p.ID])
				break
			}
		}
	}
}
