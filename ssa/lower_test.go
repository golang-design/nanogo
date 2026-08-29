// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// The engine's own tests. The arm64 rules are tested in ssa/rules, which is
// where they live; nothing here depends on a target beyond borrowing two
// machine operations to stand for one.

var (
	lowI64  = &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}
	lowPtr  = &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, Elem: lowI64, Name: "*int"}
	lowBool = &ir.Type{Kind: ir.Bool, Size: 1, Align: 1, Name: "bool"}
)

// lowFunc is a function with an entry block, its memory, and nothing else.
type lowFunc struct {
	f   *Func
	b   *Block
	mem *Value
}

func lowNew() *lowFunc {
	f := NewFunc("f")
	b := f.Entry
	b.Kind = BlockRet
	p := &lowFunc{f: f, b: b}
	p.mem = b.NewValue(0, OpInitMem, MemType)
	return p
}

func (p *lowFunc) ret(vals ...*Value) *Func {
	args := append(append([]*Value{}, vals...), p.mem)
	p.b.Control = p.b.NewValue(0, OpMakeResult, MemType, args...)
	return p.f
}

// lowRules is a rule set with one entry filled by each test.
func lowRules(rules map[Op]ValueRule) *RuleSet {
	v := make([]ValueRule, ARM64NumOps())
	for op, r := range rules {
		v[op] = r
	}
	return &RuleSet{
		Name:    "test",
		Value:   v,
		Machine: IsARM64Op,
	}
}

// lowFail runs Lower over a function it cannot lower and returns the refusal.
//
// Lower reports through its result and never panics past its own frame, so a
// construct with no rule is a refusal naming the function rather than a
// compiler that dies. A panic reaching here is a defect in the pass, which is
// why the two are told apart rather than both accepted.
func lowFail(t *testing.T, f *Func, rs *RuleSet) *LowerError {
	t.Helper()
	var out *LowerError
	func() {
		defer func() {
			if e := recover(); e != nil {
				t.Fatalf("Lower panicked with %T rather than returning it: %v", e, e)
			}
		}()
		err := Lower(f, rs)
		if err == nil {
			return
		}
		le, ok := err.(*LowerError)
		if !ok {
			t.Fatalf("Lower returned %T: %v", err, err)
		}
		out = le
	}()
	if out == nil {
		t.Fatal("Lower accepted a function it has no rule for")
	}
	return out
}

// retRule is the rule every test needs: it turns the return into a machine
// operation so that only the operation under test is left.
func retRule(v *Value, e *Edit) bool {
	e.Set(v, OpARM64RET, v.Args...)
	return true
}

func TestLowerPseudoOps(t *testing.T) {
	// specs/025's table, plus the three the table misses. Each one must have a
	// row, or a dump prints an empty name and the verifier reports an unknown
	// operation.
	for _, op := range []Op{OpPhi, OpArg, OpCopy, OpSP, OpSB, OpVarDef, OpVarKill, OpInitMem, OpSelectN} {
		if !IsPseudoOp(op) {
			t.Errorf("%v is not a pseudo-operation", op)
		}
		if infoOf(op).name == "" {
			t.Errorf("operation %d has no table row", op)
		}
	}
	if IsPseudoOp(OpAdd) {
		t.Error("Add is a pseudo-operation")
	}
	if !OpVarDef.MakesMemory() || !OpVarDef.TakesMemory() {
		t.Error("VarDef is not in the memory chain")
	}
}

func TestLowerFlagsType(t *testing.T) {
	// The kind must be Bool, because Verify requires the control of an If
	// block to have it, and after lowering that control is the flags.
	if FlagsType.Kind != ir.Bool {
		t.Errorf("FlagsType is %v, want bool", FlagsType.Kind)
	}
	if FlagsType.Size != 0 {
		t.Errorf("FlagsType has size %d, want 0", FlagsType.Size)
	}
	f := NewFunc("f")
	v := f.Entry.NewValue(0, OpARM64CMP, FlagsType)
	if !IsFlags(v) {
		t.Error("a value of FlagsType is not flags")
	}
	if IsFlags(nil) || IsFlags(f.Entry.NewValue(0, OpArg, lowI64)) {
		t.Error("a value that is not flags reports that it is")
	}
	if IsMemory(v) {
		t.Error("flags are memory")
	}
}

// TestLowerDoesNotSwallowADefectInARule checks the half of the recover that
// lets a panic through.
//
// Lower turns its own LowerError into a result so that a construct with no
// rule is a refusal and not a stack trace. Recovering anything else would turn
// a defect in this pass, a nil dereference in a rule, into a report that the
// operation has no rule, which sends whoever reads it looking for a rule that
// is already there.
func TestLowerDoesNotSwallowADefectInARule(t *testing.T) {
	const boom = "a rule dereferenced nothing"
	p := lowNew()
	f := p.ret(p.b.NewValue(0, OpAdd, lowI64, p.b.NewValue(0, OpArg, lowI64), p.b.NewValue(0, OpArg, lowI64)))
	rs := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpAdd:        func(v *Value, e *Edit) bool { panic(boom) },
	})

	defer func() {
		switch e := recover().(type) {
		case nil:
			t.Error("Lower returned rather than letting a rule's own panic through")
		case string:
			if e != boom {
				t.Errorf("Lower re-panicked with %q, want the rule's own %q", e, boom)
			}
		default:
			t.Errorf("Lower turned a rule's panic into %T: %v", e, e)
		}
	}()
	if err := Lower(f, rs); err != nil {
		t.Errorf("Lower reported %v as a missing rule, and it is a defect in a rule", err)
	}
}

// TestLowerMissingRule is the crash specs/025 requires. A silent fallback here
// produces a function that is missing an operation.
func TestLowerMissingRule(t *testing.T) {
	p := lowNew()
	f := p.ret(p.b.NewValue(0, OpAdd, lowI64, p.b.NewValue(0, OpArg, lowI64), p.b.NewValue(0, OpArg, lowI64)))
	err := lowFail(t, f, lowRules(map[Op]ValueRule{OpMakeResult: retRule}))
	if err.Op != OpAdd {
		t.Errorf("the crash names %v, want Add", err.Op)
	}
	if !strings.Contains(err.Error(), "Add") {
		t.Errorf("the message does not name the operation: %v", err)
	}
}

// TestLowerIterationCap is the assertion that the termination argument holds.
//
// The rule below fires forever without moving towards machine form, which no
// real rule may do. The engine must crash rather than hang, and it must name
// what it was doing.
func TestLowerIterationCap(t *testing.T) {
	p := lowNew()
	x := p.b.NewValue(0, OpArg, lowI64)
	y := p.b.NewValue(0, OpArg, lowI64)
	f := p.ret(p.b.NewValue(0, OpAdd, lowI64, x, y))
	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpAdd: func(v *Value, e *Edit) bool {
			e.Set(v, OpARM64ADD, v.Args[0], v.Args[1])
			return true
		},
		// A fold that swaps the arguments of a commutative operation reduces
		// nothing, so it never reaches a fixed point.
		OpARM64ADD: func(v *Value, e *Edit) bool {
			e.Set(v, OpARM64ADD, v.Args[1], v.Args[0])
			return true
		},
	})
	err := lowFail(t, f, rules)
	if !strings.Contains(err.Error(), "fixed point") {
		t.Errorf("the crash does not say what happened: %v", err)
	}
	if err.Block != p.b.ID {
		t.Errorf("the crash names b%d, want b%d", err.Block, p.b.ID)
	}
}

// TestLowerRuleMustLower asserts the first half of the termination argument:
// a rule that fires leaves no target-neutral operation behind.
func TestLowerRuleMustLower(t *testing.T) {
	p := lowNew()
	f := p.ret(p.b.NewValue(0, OpAdd, lowI64, p.b.NewValue(0, OpArg, lowI64), p.b.NewValue(0, OpArg, lowI64)))
	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpAdd: func(v *Value, e *Edit) bool {
			e.Set(v, OpSub, v.Args[0], v.Args[1])
			return true
		},
	})
	err := lowFail(t, f, rules)
	if !strings.Contains(err.Detail, "left a target-neutral operation") {
		t.Errorf("the crash says %q", err.Detail)
	}
}

// TestLowerRuleMustNotCreateNeutral asserts the other half: the replacement
// holds machine operations only, so the count of target-neutral values falls
// at every application and never rises.
func TestLowerRuleMustNotCreateNeutral(t *testing.T) {
	p := lowNew()
	f := p.ret(p.b.NewValue(0, OpAdd, lowI64, p.b.NewValue(0, OpArg, lowI64), p.b.NewValue(0, OpArg, lowI64)))
	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpAdd: func(v *Value, e *Edit) bool {
			bad := e.Insert(v.Pos, OpMul, lowI64, v.Args[0], v.Args[1])
			e.Set(v, OpARM64ADD, bad, v.Args[1])
			return true
		},
	})
	err := lowFail(t, f, rules)
	if !strings.Contains(err.Detail, "created a target-neutral operation") {
		t.Errorf("the crash says %q", err.Detail)
	}
	if err.Op != OpMul {
		t.Errorf("the crash names %v, want Mul", err.Op)
	}
}

// TestLowerReplace covers the function-wide use redirection.
//
// Value.uses is construction bookkeeping and is documented as stale, so a rule
// that removes a value has to find its readers in the graph. A reader in
// another block is the case a block-local scan gets wrong.
func TestLowerReplace(t *testing.T) {
	p := lowNew()
	x := p.b.NewValue(0, OpArg, lowI64)
	y := p.b.NewValue(0, OpArg, lowI64)
	sum := p.b.NewValue(0, OpAdd, lowI64, x, y)
	first := p.b
	first.Kind = BlockIf
	first.Control = p.b.NewValue(0, OpLess, lowBool, sum, x)

	second := p.f.NewBlock(BlockRet)
	third := p.f.NewBlock(BlockRet)
	first.AddEdgeTo(second)
	first.AddEdgeTo(third)
	use := second.NewValue(0, OpMul, lowI64, sum, sum)
	second.Control = second.NewValue(0, OpMakeResult, MemType, use, p.mem)
	third.Control = third.NewValue(0, OpMakeResult, MemType, sum, p.mem)

	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpLess: func(v *Value, e *Edit) bool {
			e.Set(v, OpARM64CBNZ, v.Args[0])
			return true
		},
		OpMul: func(v *Value, e *Edit) bool {
			e.Set(v, OpARM64MUL, v.Args[0], v.Args[1])
			return true
		},
		OpAdd: func(v *Value, e *Edit) bool {
			// Replace every use of the sum with its first argument, then keep
			// the value itself as a machine operation.
			e.Replace(v, v.Args[0])
			e.Set(v, OpARM64ADD, v.Args[0], v.Args[1])
			return true
		},
	})
	Lower(p.f, rules)
	if use.Args[0] != x || use.Args[1] != x {
		t.Errorf("a use in another block was not redirected: %v", use.LongString())
	}
	if third.Control.Args[0] != x {
		t.Errorf("a control in another block was not redirected: %v", third.Control.LongString())
	}
	if vs := Verify(p.f); len(vs) != 0 {
		t.Errorf("the function did not verify: %v\n%s", vs, p.f)
	}
}

// TestLowerCheckSplitsBlock covers the surgery a check needs.
//
// The continuation takes the block's kind, control and successors, and it
// takes the predecessor slot of every one of them. Slot i of a successor's
// predecessor list and argument i of every phi in it are the same edge, so a
// split that appends rather than overwrites moves every phi argument by one.
func TestLowerCheckSplitsBlock(t *testing.T) {
	f := NewFunc("f")
	entry := f.Entry
	entry.Kind = BlockIf
	mem := entry.NewValue(0, OpInitMem, MemType)
	idx := entry.NewValue(0, OpArg, lowI64)
	limit := entry.NewValue(0, OpArg, lowI64)
	side := entry.NewValue(0, OpArg, lowI64)
	entry.Control = entry.NewValue(0, OpARM64CBNZ, FlagsType, side)

	mid := f.NewBlock(BlockPlain)
	other := f.NewBlock(BlockPlain)
	join := f.NewBlock(BlockRet)
	entry.AddEdgeTo(mid)
	entry.AddEdgeTo(other)
	chk := mid.NewValue(0, OpBoundsCheck, MemType, idx, limit, mem)
	mid.AddEdgeTo(join)
	other.AddEdgeTo(join)
	phi := join.NewValue(0, OpPhi, lowI64, side, side)
	join.Control = join.NewValue(0, OpMakeResult, MemType, phi, chk)

	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpBoundsCheck: func(v *Value, e *Edit) bool {
			flags := e.Insert(v.Pos, OpARM64CMP, FlagsType, v.Args[0], v.Args[1])
			br := e.Insert(v.Pos, OpARM64BRcond, FlagsType, flags)
			fail, cont := e.Check(v, br)
			if cont == nil || fail == nil {
				t.Fatal("Check returned no block")
			}
			c := fail.NewValue(v.Pos, OpARM64CALLstatic, MemType, v.Args[2])
			fail.Kind = BlockExit
			fail.Control = c
			return true
		},
	})
	Lower(f, rules)
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify after the split: %v\n%s", vs, f)
	}
	if len(join.Preds) != 2 {
		t.Fatalf("the join has %d predecessors, want 2", len(join.Preds))
	}
	if join.Preds[0] == mid {
		t.Error("the join still lists the block that was split")
	}
	if join.Preds[1] != other {
		t.Error("the split moved the second predecessor")
	}
	if len(phi.Args) != 2 {
		t.Errorf("the phi has %d arguments after the split", len(phi.Args))
	}
	// The memory the check produced is gone, so its reader must now read the
	// memory the check took.
	if join.Control.Args[1] != mem {
		t.Errorf("the memory chain was not repaired: %v", join.Control.LongString())
	}
}

// TestLowerGuardKeepsValue covers the other cut: the value moves to the
// continuation instead of being removed, which is what a divide needs.
func TestLowerGuardKeepsValue(t *testing.T) {
	p := lowNew()
	x := p.b.NewValue(0, OpArg, lowI64)
	y := p.b.NewValue(0, OpArg, lowI64)
	div := p.b.NewValue(0, OpDiv, lowI64, x, y)
	f := p.ret(div)
	entry := p.b

	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpDiv: func(v *Value, e *Edit) bool {
			flags := e.Insert(v.Pos, OpARM64CMPconst, FlagsType, v.Args[1])
			br := e.Insert(v.Pos, OpARM64BRcond, FlagsType, flags)
			fail, cont := e.Guard(v, br)
			if v.Block != cont {
				t.Errorf("the guarded value stayed in %v", v.Block)
			}
			extra := e.InsertBefore(v, OpARM64MOVDconst, lowI64)
			if extra.Block != cont {
				t.Errorf("InsertBefore put the value in %v", extra.Block)
			}
			c := fail.NewValue(v.Pos, OpARM64CALLstatic, MemType, e.Mem())
			fail.Kind = BlockExit
			fail.Control = c
			e.Set(v, OpARM64SDIV, v.Args[0], v.Args[1])
			return true
		},
	})
	Lower(f, rules)
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify after the guard: %v\n%s", vs, f)
	}
	if div.dead {
		t.Error("the guarded value was removed")
	}
	if div.Block == entry {
		t.Error("the guarded value stayed in the block that was cut")
	}
	if entry.Kind != BlockIf {
		t.Errorf("the cut block is %v, want If", entry.Kind)
	}
}

// TestLowerDeadValues covers the narrow dead-value removal that belongs to
// this pass: the address computation a load absorbed has no user left.
func TestLowerDeadValues(t *testing.T) {
	p := lowNew()
	x := p.b.NewValue(0, OpArg, lowI64)
	unused := p.b.NewValue(0, OpArg, lowI64)
	orphan := p.b.NewValue(0, OpAdd, lowI64, x, x)
	nilcheck := p.b.NewValue(0, OpNilCheck, lowPtr, p.b.NewValue(0, OpArg, lowPtr), p.mem)
	f := p.ret(x)

	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpAdd: func(v *Value, e *Edit) bool {
			e.Set(v, OpARM64ADD, v.Args[0], v.Args[1])
			return true
		},
		OpNilCheck: func(v *Value, e *Edit) bool {
			e.Replace(v, v.Args[0])
			e.Set(v, OpARM64LoweredNilCheck, v.Args[0], v.Args[1])
			return true
		},
	})
	rules.Essential = func(op Op) bool { return op == OpARM64LoweredNilCheck }
	Lower(f, rules)

	if !orphan.dead {
		t.Error("a value with no user survived")
	}
	if unused.dead {
		t.Error("an argument with no user was removed, and it names an ABI location")
	}
	if nilcheck.dead {
		t.Error("the nil check was removed, and it exists to fault")
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Errorf("the function did not verify: %v\n%s", vs, f)
	}
}

// TestLowerKeepsCallResults asserts that a result nobody reads is kept.
//
// An OpSelectN names one ABI location of a call, the way an OpArg names one of
// the function's own, and specs/030-abi.md assigns the results of a call by
// counting them. Dropping the one with no user leaves the call with a result
// that has no name and ssagen stops with "result 0 of the call is never
// named". "_, b := f()" is the source that produces it.
func TestLowerKeepsCallResults(t *testing.T) {
	p := lowNew()
	call := p.b.NewValue(0, OpStaticCall, MemType, p.mem)
	call.Aux = RuntimeFunc("runtime.gorecover")
	unread := p.b.NewValue(0, OpSelectN, lowI64, call)
	read := p.b.NewValue(0, OpSelectN, lowI64, call)
	read.AuxInt = 1
	p.mem = call
	f := p.ret(read)

	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpStaticCall: func(v *Value, e *Edit) bool {
			e.Set(v, OpARM64CALLstatic, v.Args...)
			return true
		},
	})
	Lower(f, rules)

	if unread.dead {
		t.Error("the result with no user was removed, and it names an ABI location")
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Errorf("the function did not verify: %v\n%s", vs, f)
	}
}

// TestLowerBases asserts the two base pointers are made once and dominate
// every use.
func TestLowerBases(t *testing.T) {
	p := lowNew()
	g := p.b.NewValue(0, OpAddr, lowPtr)
	h := p.b.NewValue(0, OpAddr, lowPtr)
	f := p.ret(g, h)
	var sp, sb *Value
	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpAddr: func(v *Value, e *Edit) bool {
			if sb == nil {
				sb, sp = e.SB(), e.SP()
			}
			if e.SB() != sb || e.SP() != sp {
				t.Error("a base pointer was made twice")
			}
			e.Set(v, OpARM64MOVDaddr, e.SB())
			return true
		},
	})
	Lower(f, rules)
	if sb == nil || sb.Block != p.f.Entry {
		t.Fatal("the static base is not in the entry block")
	}
	if p.f.Entry.Values[0] != sb && p.f.Entry.Values[0] != sp {
		t.Error("a base pointer is not at the front of the entry block")
	}
	if vs := Verify(f); len(vs) != 0 {
		t.Errorf("the function did not verify: %v\n%s", vs, f)
	}
	// The stack pointer has no user here, and it is kept anyway: the frame
	// layout reads it.
	if sp.dead {
		t.Error("the stack pointer was removed")
	}
}

func TestLowerCheckLowered(t *testing.T) {
	p := lowNew()
	f := p.ret(p.b.NewValue(0, OpAdd, lowI64, p.b.NewValue(0, OpArg, lowI64), p.b.NewValue(0, OpArg, lowI64)))
	rs := lowRules(nil)
	vs := CheckLowered(f, rs)
	if len(vs) != 2 {
		t.Fatalf("CheckLowered found %d violations, want 2: %v", len(vs), vs)
	}
	if !strings.Contains(vs[0].Detail, "machine operation") {
		t.Errorf("the violation says %q", vs[0].Detail)
	}
	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpAdd: func(v *Value, e *Edit) bool {
			e.Set(v, OpARM64ADD, v.Args[0], v.Args[1])
			return true
		},
	})
	Lower(f, rules)
	if vs := CheckLowered(f, rules); len(vs) != 0 {
		t.Errorf("CheckLowered found %v after lowering", vs)
	}
}

// TestLowerRuleTable covers the lookup a rule set is.
func TestLowerRuleTable(t *testing.T) {
	rs := lowRules(map[Op]ValueRule{OpAdd: retRule})
	if rs.Rule(OpAdd) == nil {
		t.Error("the rule for Add is missing")
	}
	if rs.Rule(OpSub) != nil {
		t.Error("Sub has a rule it was not given")
	}
	if rs.Rule(Op(250)) != nil {
		t.Error("an operation outside the table has a rule")
	}
	if !rs.IsLowered(OpARM64ADD) || !rs.IsLowered(OpPhi) || rs.IsLowered(OpAdd) {
		t.Error("IsLowered disagrees with the machine and pseudo sets")
	}
	empty := &RuleSet{Name: "none"}
	if empty.IsLowered(OpARM64ADD) {
		t.Error("a rule set with no machine predicate lowered something")
	}
}

// TestLowerBlockRuleRunsAfterValues asserts the ordering the condition codes
// need: the comparison is lowered before the branch that reads its flags.
func TestLowerBlockRuleRunsAfterValues(t *testing.T) {
	p := lowNew()
	x := p.b.NewValue(0, OpArg, lowI64)
	c := p.b.NewValue(0, OpLess, lowI64, x, x)
	first := p.b
	first.Kind = BlockIf
	first.Control = c
	then := p.f.NewBlock(BlockRet)
	els := p.f.NewBlock(BlockRet)
	first.AddEdgeTo(then)
	first.AddEdgeTo(els)
	then.Control = then.NewValue(0, OpMakeResult, MemType, p.mem)
	els.Control = els.NewValue(0, OpMakeResult, MemType, p.mem)

	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpLess: func(v *Value, e *Edit) bool {
			flags := e.Insert(v.Pos, OpARM64CMP, FlagsType, v.Args[0], v.Args[1])
			e.Set(v, OpARM64CSET, flags)
			return true
		},
	})
	rules.Block = func(b *Block, e *Edit) bool {
		if b.Control.Op != OpARM64CSET {
			t.Errorf("the block rule saw %v, so it ran before the values", b.Control.Op)
			return false
		}
		br := e.Insert(b.Control.Pos, OpARM64BRcond, FlagsType, b.Control.Args[0])
		b.Control = br
		return true
	}
	Lower(p.f, rules)
	if first.Control.Op != OpARM64BRcond {
		t.Errorf("the control is %v", first.Control.Op)
	}
	if vs := Verify(p.f); len(vs) != 0 {
		t.Errorf("the function did not verify: %v\n%s", vs, p.f)
	}
}

// TestLowerManySplitsInOneBlock covers the visit cap's accounting.
//
// The cap was 8*(len(f.Blocks)+16), read once before the walk. A cut adds two
// blocks and queues one of them, so a block full of bounds checks needs about
// two visits per check, and the cap described none of that: it described the
// block count the function had before any of the checks was lowered. A
// function that converges fine crashed with
//
//	ssa: lower: determinant: b135 v-1: Invalid: the block queue did not drain
//
// Go's own test corpus reaches it on test/torture.go, whose determinant enters
// lowering as one block, leaves as 385, and needs exactly 385 visits: one per
// block and none repeated. The one-term cap allowed 136.
//
// Eighty checks in one block is past that cap and well inside the two-term
// one, so this test crashes without the fix and passes with it.
func TestLowerManySplitsInOneBlock(t *testing.T) {
	const checks = 80

	p := lowNew()
	idx := p.b.NewValue(0, OpArg, lowI64)
	limit := p.b.NewValue(0, OpArg, lowI64)
	mem := p.mem
	for i := 0; i < checks; i++ {
		mem = p.b.NewValue(0, OpBoundsCheck, MemType, idx, limit, mem)
	}
	p.mem = mem
	f := p.ret()

	rules := lowRules(map[Op]ValueRule{
		OpMakeResult: retRule,
		OpBoundsCheck: func(v *Value, e *Edit) bool {
			flags := e.Insert(v.Pos, OpARM64CMP, FlagsType, v.Args[0], v.Args[1])
			br := e.Insert(v.Pos, OpARM64BRcond, FlagsType, flags)
			fail, _ := e.Check(v, br)
			c := fail.NewValue(v.Pos, OpARM64CALLstatic, MemType, v.Args[2])
			fail.Kind = BlockExit
			fail.Control = c
			return true
		},
	})
	Lower(f, rules)
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify after %d splits: %v", checks, vs)
	}
	// Two blocks per check, plus the one the function started with.
	if want := 2*checks + 1; len(f.Blocks) != want {
		t.Errorf("%d blocks after %d splits, want %d", len(f.Blocks), checks, want)
	}
}
