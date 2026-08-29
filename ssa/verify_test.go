// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// A verifier that never fires is not a verifier. Every invariant of
// specs/021-ssa-construction.md gets a function broken in exactly that way,
// and the test asserts the reported set is exactly that invariant. Asserting
// only that an error came back would pass on a function that is broken in some
// other way, which is the failure mode this file exists to rule out.

// minimalFunc returns the smallest function that verifies: an entry block that
// creates memory and returns it.
func minimalFunc() (f *Func, entry *Block, mem *Value) {
	f = NewFunc("t")
	entry = f.Entry
	entry.Kind = BlockRet
	mem = entry.NewValue(0, OpInitMem, MemType)
	entry.Control = entry.NewValue(0, OpMakeResult, MemType, mem)
	return f, entry, mem
}

// setRet replaces the block's return with one that leaves with m.
//
// The MakeResult has to be the last value in the block, because it reads the
// memory the block ends with. A fixture that adds memory operations calls this
// afterwards so that the only broken thing is the one it broke.
func setRet(b *Block, m *Value) {
	if b.Control != nil {
		b.removeValue(b.Control)
	}
	b.Control = b.NewValue(0, OpMakeResult, MemType, m)
}

// diamond returns a function with an if and a join, which is the shape the
// phi and dominance fixtures need.
//
//	entry -> then, els
//	then  -> join
//	els   -> join
//	join: Ret
func diamond() (f *Func, entry, then, els, join *Block, mem *Value) {
	f = NewFunc("t")
	entry = f.Entry
	entry.Kind = BlockIf
	mem = entry.NewValue(0, OpInitMem, MemType)
	c := entry.NewValue(0, OpConstBool, tBool)
	entry.Control = c

	then = f.NewBlock(BlockPlain)
	els = f.NewBlock(BlockPlain)
	join = f.NewBlock(BlockRet)
	entry.AddEdgeTo(then)
	entry.AddEdgeTo(els)
	then.AddEdgeTo(join)
	els.AddEdgeTo(join)
	join.Control = join.NewValue(0, OpMakeResult, MemType, mem)
	return f, entry, then, els, join, mem
}

// invariants returns the distinct invariants reported, in the order they were
// reported.
func invariants(vs []Violation) []Invariant {
	var out []Invariant
	for _, v := range vs {
		found := false
		for _, o := range out {
			if o == v.Invariant {
				found = true
			}
		}
		if !found {
			out = append(out, v.Invariant)
		}
	}
	return out
}

// wantOnly asserts that vs is non-empty and reports inv and nothing else.
func wantOnly(t *testing.T, f *Func, vs []Violation, inv Invariant) {
	t.Helper()
	if len(vs) == 0 {
		t.Fatalf("no violation reported, want %v\n%s", inv, f)
	}
	got := invariants(vs)
	if len(got) != 1 || got[0] != inv {
		t.Fatalf("reported %v, want only %v\n%v\n%s", got, inv, vs, f)
	}
}

func TestVerifyAcceptsAMinimalFunc(t *testing.T) {
	f, _, _ := minimalFunc()
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v on a well formed function\n%s", vs, f)
	}
	if err := Check(f); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestVerifyAcceptsADiamond(t *testing.T) {
	f, _, _, _, _, _ := diamond()
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v on a well formed function\n%s", vs, f)
	}
}

func TestVerifyCatchesUntypedValue(t *testing.T) {
	f, entry, _ := minimalFunc()
	v := entry.NewValue(0, OpConstInt, tInt)
	v.Type = nil
	wantOnly(t, f, Verify(f), InvTyped)
}

func TestVerifyCatchesOpForm(t *testing.T) {
	tests := []struct {
		name   string
		mangle func(f *Func, entry *Block, mem *Value)
	}{
		{"too few arguments", func(f *Func, entry *Block, mem *Value) {
			c := entry.NewValue(0, OpConstInt, tInt)
			entry.NewValue(0, OpAdd, tInt, c)
		}},
		{"too many arguments", func(f *Func, entry *Block, mem *Value) {
			c := entry.NewValue(0, OpConstInt, tInt)
			entry.NewValue(0, OpAdd, tInt, c, c, c)
		}},
		{"a memory operation with a value type", func(f *Func, entry *Block, mem *Value) {
			a := entry.NewValue(0, OpConstNil, tIntPtr)
			c := entry.NewValue(0, OpConstInt, tInt)
			s := entry.NewValue(0, OpStore, MemType, a, c, mem)
			s.Type = tInt
			setRet(entry, s)
		}},
		{"a nil argument", func(f *Func, entry *Block, mem *Value) {
			entry.NewValue(0, OpNeg, tInt, nil)
		}},
		{"memory where a value belongs", func(f *Func, entry *Block, mem *Value) {
			c := entry.NewValue(0, OpConstInt, tInt)
			entry.NewValue(0, OpAdd, tInt, c, mem)
		}},
		{"an operation with no table row", func(f *Func, entry *Block, mem *Value) {
			v := entry.NewValue(0, OpConstInt, tInt)
			v.Op = opNoRow()
		}},
		{"a value in the wrong block", func(f *Func, entry *Block, mem *Value) {
			v := entry.NewValue(0, OpConstInt, tInt)
			v.Block = f.NewBlock(BlockPlain)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, entry, mem := minimalFunc()
			tc.mangle(f, entry, mem)
			// The block added by the wrong-block case would be unreachable, so
			// it is dropped again before verifying.
			f.Blocks = f.Blocks[:1]
			wantOnly(t, f, Verify(f), InvOpForm)
		})
	}
}

func TestVerifyCatchesPhiNotAtBlockStart(t *testing.T) {
	f, _, _, _, join, mem := diamond()
	c := f.Entry.Values[1]
	phi := join.NewValue(0, OpPhi, tBool, c, c)
	// The MakeResult is already in the block, so the phi is not first.
	if join.Values[0] == phi {
		t.Fatal("the fixture did not put the phi second")
	}
	_ = mem
	wantOnly(t, f, Verify(f), InvOpForm)
}

func TestVerifyCatchesBlockControl(t *testing.T) {
	tests := []struct {
		name   string
		mangle func(f *Func, entry *Block, mem *Value)
	}{
		{"a control value with one successor", func(f *Func, entry *Block, mem *Value) {
			b := f.NewBlock(BlockRet)
			entry.Kind = BlockPlain
			entry.AddEdgeTo(b)
			b.Control = entry.Control
			entry.Control = entry.NewValue(0, OpConstBool, tBool)
		}},
		{"no control value with no successor", func(f *Func, entry *Block, mem *Value) {
			entry.Control = nil
		}},
		{"the wrong number of successors", func(f *Func, entry *Block, mem *Value) {
			b := f.NewBlock(BlockRet)
			b.Control = entry.Control
			entry.Kind = BlockIf
			entry.Control = entry.NewValue(0, OpConstBool, tBool)
			entry.AddEdgeTo(b)
		}},
		{"a block with no kind", func(f *Func, entry *Block, mem *Value) {
			entry.Kind = BlockInvalid
		}},
		{"an if controlled by something that is not a bool", func(f *Func, entry *Block, mem *Value) {
			b := f.NewBlock(BlockRet)
			c := f.NewBlock(BlockRet)
			b.Control = entry.Control
			c.Control = entry.Control
			entry.Kind = BlockIf
			entry.Control = entry.NewValue(0, OpConstInt, tInt)
			entry.AddEdgeTo(b)
			entry.AddEdgeTo(c)
		}},
		{"a return controlled by something that is not memory", func(f *Func, entry *Block, mem *Value) {
			entry.Control = entry.NewValue(0, OpConstInt, tInt)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, entry, mem := minimalFunc()
			tc.mangle(f, entry, mem)
			wantOnly(t, f, Verify(f), InvBlockControl)
		})
	}
}

// TestVerifyCatchesUnmirroredEdge breaks one half of an edge. The successor is
// still reachable through the other half, so nothing but the edge is wrong.
func TestVerifyCatchesUnmirroredEdge(t *testing.T) {
	f, _, _, els, _, _ := diamond()
	els.Preds = nil
	wantOnly(t, f, Verify(f), InvBlockControl)
}

func TestVerifyCatchesPhiArity(t *testing.T) {
	f, entry, _, _, join, _ := diamond()
	c := entry.Values[1]
	// A join of two edges with one argument.
	phi := f.newValue(OpPhi, tBool, 0)
	phi.Block = join
	phi.AddArg(c)
	join.Values = append([]*Value{phi}, join.Values...)
	wantOnly(t, f, Verify(f), InvPhiArity)
}

func TestVerifyCatchesArgumentThatDoesNotDominate(t *testing.T) {
	f, _, then, els, _, _ := diamond()
	// Defined in one arm of the if and used in the other. Neither dominates
	// the other, which is the shape a badly written pass produces.
	v := then.NewValue(0, OpConstInt, tInt)
	els.NewValue(0, OpNeg, tInt, v)
	wantOnly(t, f, Verify(f), InvArgDominates)
}

func TestVerifyCatchesUseBeforeDefinitionInOneBlock(t *testing.T) {
	f, entry, _ := minimalFunc()
	c := entry.NewValue(0, OpConstInt, tInt)
	use := entry.NewValue(0, OpNeg, tInt, c)
	// Put the use before the definition. Within a block the value list is the
	// order of execution.
	n := len(entry.Values)
	entry.Values[n-2], entry.Values[n-1] = entry.Values[n-1], entry.Values[n-2]
	_ = use
	wantOnly(t, f, Verify(f), InvArgDominates)
}

func TestVerifyCatchesPhiArgumentThatDoesNotDominateItsEdge(t *testing.T) {
	f, _, then, els, join, _ := diamond()
	// The argument for the edge from then is defined in els.
	v := els.NewValue(0, OpConstInt, tInt)
	w := then.NewValue(0, OpConstInt, tInt)
	phi := f.newValue(OpPhi, tInt, 0)
	phi.Block = join
	phi.AddArg(v)
	phi.AddArg(w)
	join.Values = append([]*Value{phi}, join.Values...)
	// then is predecessor 0, so argument 0 must be defined on that path.
	wantOnly(t, f, Verify(f), InvArgDominates)
}

// TestVerifyCatchesPhiArgumentInNoBlock covers a value that a pass created and
// forgot to insert. It has no definition point, so nothing can dominate a use
// of it.
func TestVerifyCatchesPhiArgumentInNoBlock(t *testing.T) {
	f, _, then, _, join, _ := diamond()
	loose := f.newValue(OpConstInt, tInt, 0)
	w := then.NewValue(0, OpConstInt, tInt)
	phi := f.newValue(OpPhi, tInt, 0)
	phi.Block = join
	phi.AddArg(w)
	phi.AddArg(loose)
	join.Values = append([]*Value{phi}, join.Values...)
	wantOnly(t, f, Verify(f), InvArgDominates)
}

func TestVerifyCatchesArgumentInNoBlock(t *testing.T) {
	f, entry, _ := minimalFunc()
	loose := f.newValue(OpConstInt, tInt, 0)
	entry.NewValue(0, OpNeg, tInt, loose)
	wantOnly(t, f, Verify(f), InvArgDominates)
}

func TestVerifyCatchesPhiWithANilArgument(t *testing.T) {
	f, _, _, _, join, _ := diamond()
	phi := f.newValue(OpPhi, tInt, 0)
	phi.Block = join
	phi.AddArg(nil)
	phi.AddArg(nil)
	join.Values = append([]*Value{phi}, join.Values...)
	wantOnly(t, f, Verify(f), InvOpForm)
}

func TestVerifyCatchesPhiThatMixesMemoryAndValues(t *testing.T) {
	f, _, then, _, join, mem := diamond()
	a := f.Entry.NewValue(0, OpConstNil, tIntPtr)
	c := f.Entry.NewValue(0, OpConstInt, tInt)
	m1 := then.NewValue(0, OpStore, MemType, a, c, mem)
	phi := f.newValue(OpPhi, MemType, 0)
	phi.Block = join
	phi.AddArg(m1)
	phi.AddArg(c) // the els edge carries a value where memory belongs
	join.Values = append([]*Value{phi}, join.Values...)
	setRet(join, phi)
	wantOnly(t, f, Verify(f), InvOpForm)
}

func TestVerifyCatchesMemoryChain(t *testing.T) {
	tests := []struct {
		name   string
		mangle func(f *Func, entry *Block, mem *Value)
	}{
		{"a stale memory argument", func(f *Func, entry *Block, mem *Value) {
			a := entry.NewValue(0, OpConstNil, tIntPtr)
			c := entry.NewValue(0, OpConstInt, tInt)
			entry.NewValue(0, OpStore, MemType, a, c, mem)
			// The second store reads the memory the first one replaced.
			s2 := entry.NewValue(0, OpStore, MemType, a, c, mem)
			setRet(entry, s2)
		}},
		{"a load of memory that is not live", func(f *Func, entry *Block, mem *Value) {
			a := entry.NewValue(0, OpConstNil, tIntPtr)
			c := entry.NewValue(0, OpConstInt, tInt)
			s := entry.NewValue(0, OpStore, MemType, a, c, mem)
			entry.NewValue(0, OpLoad, tInt, a, mem)
			setRet(entry, s)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, entry, mem := minimalFunc()
			tc.mangle(f, entry, mem)
			wantOnly(t, f, Verify(f), InvMemChain)
		})
	}
}

func TestVerifyCatchesTwoMemoryPhis(t *testing.T) {
	f, _, then, els, join, mem := diamond()
	a := f.Entry.NewValue(0, OpConstNil, tIntPtr)
	c := f.Entry.NewValue(0, OpConstInt, tInt)
	m1 := then.NewValue(0, OpStore, MemType, a, c, mem)
	m2 := els.NewValue(0, OpStore, MemType, a, c, mem)
	p1 := f.newValue(OpPhi, MemType, 0)
	p1.Block = join
	p1.AddArg(m1)
	p1.AddArg(m2)
	p2 := f.newValue(OpPhi, MemType, 0)
	p2.Block = join
	p2.AddArg(m1)
	p2.AddArg(m2)
	join.Values = append([]*Value{p1, p2}, join.Values...)
	setRet(join, p2)
	wantOnly(t, f, Verify(f), InvMemChain)
}

// TestVerifyCatchesAJoinWithNoMemoryPhi is the case specs/021 says needs no
// special handling: a block that merges control flow merges memory with an
// ordinary phi. A join that does not is exactly what this catches.
func TestVerifyCatchesAJoinWithNoMemoryPhi(t *testing.T) {
	f := NewFunc("t")
	e := f.Entry
	e.Kind = BlockIf
	mem := e.NewValue(0, OpInitMem, MemType)
	a := e.NewValue(0, OpConstNil, tIntPtr)
	c := e.NewValue(0, OpConstInt, tInt)
	e.Control = e.NewValue(0, OpConstBool, tBool)

	then := f.NewBlock(BlockPlain)
	els := f.NewBlock(BlockPlain)
	join := f.NewBlock(BlockPlain)
	exit := f.NewBlock(BlockRet)
	e.AddEdgeTo(then)
	e.AddEdgeTo(els)
	then.AddEdgeTo(join)
	els.AddEdgeTo(join)
	join.AddEdgeTo(exit)
	then.NewValue(0, OpStore, MemType, a, c, mem)
	els.NewValue(0, OpStore, MemType, a, c, mem)
	exit.Control = exit.NewValue(0, OpMakeResult, MemType, mem)

	wantOnly(t, f, Verify(f), InvMemChain)
}

// TestVerifyCatchesALoopHeaderWithNoMemoryPhi is the case a forward diamond
// cannot show. The header is reached before its back edge in reverse
// postorder, so the memory the body wrote is only known after the walk is
// finished. A pass that rotates a loop and forgets the memory phi produces
// exactly this.
func TestVerifyCatchesALoopHeaderWithNoMemoryPhi(t *testing.T) {
	f := NewFunc("t")
	e := f.Entry
	e.Kind = BlockPlain
	mem := e.NewValue(0, OpInitMem, MemType)
	a := e.NewValue(0, OpConstNil, tIntPtr)
	c := e.NewValue(0, OpConstInt, tInt)

	head := f.NewBlock(BlockIf)
	body := f.NewBlock(BlockPlain)
	exit := f.NewBlock(BlockRet)
	e.AddEdgeTo(head)
	head.Control = head.NewValue(0, OpConstBool, tBool)
	head.AddEdgeTo(body)
	head.AddEdgeTo(exit)
	body.NewValue(0, OpStore, MemType, a, c, mem)
	body.AddEdgeTo(head)
	exit.Control = exit.NewValue(0, OpMakeResult, MemType, mem)

	wantOnly(t, f, Verify(f), InvMemChain)
}

// TestVerifyAcceptsALoopWithAMemoryPhi is the same loop built correctly, so
// that the test above is known to fail for the reason it claims.
func TestVerifyAcceptsALoopWithAMemoryPhi(t *testing.T) {
	f := NewFunc("t")
	e := f.Entry
	e.Kind = BlockPlain
	mem := e.NewValue(0, OpInitMem, MemType)
	a := e.NewValue(0, OpConstNil, tIntPtr)
	c := e.NewValue(0, OpConstInt, tInt)

	head := f.NewBlock(BlockIf)
	body := f.NewBlock(BlockPlain)
	exit := f.NewBlock(BlockRet)
	e.AddEdgeTo(head)
	phi := f.newValue(OpPhi, MemType, 0)
	phi.Block = head
	head.Values = append(head.Values, phi)
	head.Control = head.NewValue(0, OpConstBool, tBool)
	head.AddEdgeTo(body)
	head.AddEdgeTo(exit)
	st := body.NewValue(0, OpStore, MemType, a, c, phi)
	body.AddEdgeTo(head)
	phi.AddArg(mem)
	phi.AddArg(st)
	exit.Control = exit.NewValue(0, OpMakeResult, MemType, phi)

	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v on a loop with a memory phi\n%s", vs, f)
	}
}

func TestVerifyCatchesUnreachableBlock(t *testing.T) {
	f, entry, _ := minimalFunc()
	orphan := f.NewBlock(BlockPlain)
	orphan.NewValue(0, OpConstInt, tInt)
	orphan.AddEdgeTo(entry)
	// The entry is not allowed a predecessor either, so the edge is removed
	// again and only the orphan is left dangling.
	entry.Preds = nil
	wantOnly(t, f, Verify(f), InvReachable)
}

func TestVerifyCatchesEntryWithPredecessor(t *testing.T) {
	f, entry, _ := minimalFunc()
	b := f.NewBlock(BlockPlain)
	b.AddEdgeTo(entry)
	// b is unreachable as well, so the reported set is the same invariant
	// twice rather than two different ones.
	vs := Verify(f)
	wantOnly(t, f, vs, InvReachable)
	if len(vs) != 2 {
		t.Fatalf("got %d violations, want one for the entry and one for the orphan: %v", len(vs), vs)
	}
}

func TestVerifyCatchesNoEntry(t *testing.T) {
	f := NewFunc("t")
	f.Blocks = nil
	f.Entry = nil
	wantOnly(t, f, Verify(f), InvReachable)
}

func TestVerifyCatchesGoSpecificOp(t *testing.T) {
	f, entry, _ := minimalFunc()
	v := entry.NewValue(0, OpConstInt, tInt)
	// A lowering that carried the Go operation across instead of replacing it.
	v.Aux = ir.ORange
	wantOnly(t, f, Verify(f), InvGoSpecific)
	// An operation that is not Go-specific is not reported.
	v.Aux = ir.OBinary
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v for a lowered operation", vs)
	}
}

func TestVerifyCatchesDeadArgument(t *testing.T) {
	f, entry, _ := minimalFunc()
	c := entry.NewValue(0, OpConstInt, tInt)
	entry.NewValue(0, OpNeg, tInt, c)
	entry.removeValue(c)
	wantOnly(t, f, Verify(f), InvOpForm)
}

func TestVerifyCatchesDeadControl(t *testing.T) {
	f, entry, _ := minimalFunc()
	entry.removeValue(entry.Control)
	wantOnly(t, f, Verify(f), InvBlockControl)
}

func TestInvariantAndViolationStrings(t *testing.T) {
	if got := InvMemChain.String(); !strings.Contains(got, "memory") {
		t.Errorf("InvMemChain is %q", got)
	}
	if got := Invariant(200).String(); got != "invariant(?)" {
		t.Errorf("an unnamed invariant is %q", got)
	}
	v := Violation{Invariant: InvTyped, Block: 3, Value: 7, Detail: "detail"}
	if got := v.String(); !strings.Contains(got, "b3 v7") || !strings.Contains(got, "detail") {
		t.Errorf("Violation.String is %q", got)
	}
	v.Value = -1
	if got := v.String(); !strings.Contains(got, "b3:") {
		t.Errorf("Violation.String without a value is %q", got)
	}
}

func TestCheckReportsTheFirstInvariant(t *testing.T) {
	f, entry, _ := minimalFunc()
	v := entry.NewValue(0, OpConstInt, tInt)
	v.Type = nil
	err := Check(f)
	if err == nil {
		t.Fatal("Check returned no error")
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("Check returned %T, want *ssa.Error", err)
	}
	if e.Invariant != InvTyped {
		t.Errorf("Check reported %v, want %v", e.Invariant, InvTyped)
	}
	if !strings.Contains(e.Error(), "every value has a type") {
		t.Errorf("the error does not name the invariant: %v", e)
	}
}

// TestVerifyStringConcatReachedSSA is the check that was missing.
//
// specs/002-architecture.md says no Go construct survives into SSA and
// specs/020-ir.md's table sends string concatenation to the runtime, and
// nothing checked it: an Add over two strings built, verified, and then failed
// to lower in 49 functions of the distribution corpus, every one of which
// looked like a gap in lowering rather than a construct the builder did not
// build.
func TestVerifyStringConcatReachedSSA(t *testing.T) {
	str := &ir.Type{Kind: ir.String, Size: 16, Align: 8, Name: "string"}
	f, entry, mem := minimalFunc()
	a := entry.NewValue(0, OpConstString, str)
	b := entry.NewValue(0, OpConstString, str)
	entry.NewValue(0, OpAdd, str, a, b)
	setRet(entry, mem)
	wantOnly(t, f, Verify(f), InvGoSpecific)
}

// TestVerifyAcceptsStringsThatAreNotConcatenated is the other half: the
// invariant must not fire on the operations a string legitimately reaches.
//
// The comparison rows of the same table also become runtime calls, and
// lowering is where they become them, because runtime.memequal takes the data
// pointer and the length and neither exists as a value until the string is
// split. An invariant that flagged them would be false from construction until
// decomposition.
func TestVerifyAcceptsStringsThatAreNotConcatenated(t *testing.T) {
	str := &ir.Type{Kind: ir.String, Size: 16, Align: 8, Name: "string"}
	boolean := &ir.Type{Kind: ir.Bool, Size: 1, Align: 1, Name: "bool"}
	f, entry, mem := minimalFunc()
	a := entry.NewValue(0, OpConstString, str)
	b := entry.NewValue(0, OpConstString, str)
	entry.NewValue(0, OpEq, boolean, a, b)
	entry.NewValue(0, OpLess, boolean, a, b)
	entry.NewValue(0, OpCopy, str, a)
	setRet(entry, mem)
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v on strings that are compared, not concatenated\n%s", vs, f)
	}
}

// A second static base is a second name for one address, and the two are free
// to disagree about the one thing that matters: whether the collector follows
// it. Go's own test/linkmain_run.go stopped with "bad pointer in frame" on
// every run because a pass made one and typed it as a pointer.
func TestVerifyCatchesASecondStaticBase(t *testing.T) {
	f, entry, _ := minimalFunc()
	entry.NewValue(0, OpSB, machinePtrType)
	if vs := Verify(f); len(vs) != 0 {
		t.Fatalf("Verify reported %v for one static base\n%s", vs, f)
	}
	entry.NewValue(0, OpSB, machinePtrType)
	wantOnly(t, f, Verify(f), InvOneBase)
}

func TestVerifyCatchesASecondStackPointer(t *testing.T) {
	f, entry, _ := minimalFunc()
	entry.NewValue(0, OpSP, machinePtrType)
	entry.NewValue(0, OpSP, machinePtrType)
	wantOnly(t, f, Verify(f), InvOneBase)
}

// A base described as holding a pointer gets its spill slot marked in the
// locals bitmap, and runtime.adjustpointers reads that word when the stack
// grows. Neither base points into the heap, so neither may say it does.
func TestVerifyCatchesAPointerTypedBase(t *testing.T) {
	f, entry, _ := minimalFunc()
	entry.NewValue(0, OpSB, unsafePtrType)
	wantOnly(t, f, Verify(f), InvOneBase)

	g, gentry, _ := minimalFunc()
	gentry.NewValue(0, OpSP, unsafePtrType)
	wantOnly(t, g, Verify(g), InvOneBase)
}
