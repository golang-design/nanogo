// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtsym"
	"golang.design/x/nanogo/syntax"
)

// The write barrier of specs/034-write-barriers.md.
//
// Go's collector marks while the program runs. A pointer store the collector
// does not observe can hide a reachable object from a marking phase that has
// already passed the place the pointer came from, and the object is then freed
// while the program still reaches it. The failure is not at the store and it is
// not every time.
//
// Go's own test/chanlinear.go is the program that samples it. Under GOGC=1
// with the collector concurrent it deadlocks in about one run of two, under
// GODEBUG=gcstoptheworld=2 it never does, and gccheckmark names the object it
// loses: a capture of a two-capture closure, whose type descriptor and pointer
// mask are both correct. The mask being right and the object still being lost
// is what says the store and not the description is the gap.
//
// # The shape, which is gc's
//
//	if runtime.writeBarrier.enabled {
//	    buf := runtime.gcWriteBarrier2()
//	    buf[0] = val        // the value written
//	    buf[1] = *dst       // the value overwritten
//	}
//	*dst = val
//
// The barrier records a pair and does not perform the store. The store is
// unconditional and stands on both paths, which is what makes this a diamond
// and not a choice between two stores. Go's barrier is hybrid: it tracks the
// deletion as well as the insertion, which is what lets the collector leave a
// stack unscanned after the first time.
//
// # Why the pass runs after lowering
//
// The values it inserts are machine operations, so it runs after selection has
// made every other value one. Running before would mean a target-neutral
// operation for the barrier and a rule to select it, and the rule would have
// nothing to choose between.
//
// It runs before register allocation, which is what makes the barrier call a
// safepoint like any other: the allocator spills what is live across it and
// specs/027-liveness-and-stackmaps.md describes the frame at it. Critical edge
// splitting runs after this pass and repairs the edges the diamond creates.
func WriteBarriers(f *Func) {
	if f == nil {
		return
	}
	w := &barrierer{f: f, done: make(map[*Value]bool)}
	// The blocks are taken before the walk starts, because building a diamond
	// both appends to the list and reorders it, so an index into it does not
	// survive one. A block this pass creates holds no store it has not already
	// dealt with, so the original list is the whole of the work.
	blocks := append([]*Block(nil), f.Blocks...)
	for _, b := range blocks {
		w.block(b)
	}
}

// barrierer holds the types and objects the inserted values need, made once.
type barrierer struct {
	f *Func

	flagObj *ir.Object
	u32     *ir.Type
	bool    *ir.Type
	uptr    *ir.Type
	sb      *Value

	// done is the stores this pass has already given a barrier.
	//
	// The store survives the rewrite: it is the one on both paths of the
	// diamond, and it moves to the continuation block still a pointer store
	// into memory the collector may own. Without this it would answer the same
	// question the same way for ever.
	done map[*Value]bool
}

// block walks one block and gives every store that needs a barrier one.
//
// The walk restarts after each barrier, because building the diamond moves the
// rest of the block into a new one. What is left in this block after the cut is
// the part already walked, so the restart is on the continuation.
func (w *barrierer) block(b *Block) {
	for {
		at := -1
		for i, v := range b.Values {
			if w.needsBarrier(v) {
				at = i
				break
			}
		}
		if at < 0 {
			return
		}
		cont := w.barrier(b, at)
		if cont == nil {
			return
		}
		b = cont
	}
}

// needsBarrier reports whether v is a pointer store into memory the collector
// may own.
//
// The rule is specs/034's table: a store of a pointer to a location that may be
// in the heap. Both halves narrow it, and being wrong in the conservative
// direction costs a branch while being wrong in the other frees a live object.
func (w *barrierer) needsBarrier(v *Value) bool {
	val, ok := storedPointer(v)
	if !ok || w.done[v] {
		return false
	}
	_ = val
	if frameAddress(v.Args[0]) {
		// A frame slot, which specs/034 names as the one destination a barrier
		// may be omitted for. The collector scans a stack by its maps and a
		// write to one needs no record.
		return false
	}
	return true
}

// storedPointer returns the value a store writes, and whether that value is a
// pointer the collector may follow.
//
// Only the pointer-wide forms can write one, and both the plain and the
// indexed shapes can: `p.f = q` is the first and `a[i] = q` is the second. A
// narrower store cannot hold a pointer at all.
//
// The type's own map decides and the width does not. A uintptr is one word and
// holds no pointer, and so is the code word of a func value, which is why
// specs/033-closures-defer-panic.md gives that word the uintptr type.
func storedPointer(v *Value) (*Value, bool) {
	var val *Value
	switch v.Op {
	case OpARM64MOVDstore:
		if len(v.Args) < 3 {
			return nil, false
		}
		val = v.Args[1]
	case OpARM64MOVDstoreidx, OpARM64MOVDstoreidx8:
		if len(v.Args) < 4 {
			return nil, false
		}
		val = v.Args[2]
	default:
		return nil, false
	}
	if val == nil || val.Type == nil || val.Type.PtrBytes() == 0 {
		return nil, false
	}
	return val, true
}

// frameAddress reports whether a is provably the address of a frame slot.
//
// ADDframe is the frame layout's own address form and nothing else produces
// one, so the answer is the operation and not an analysis. A pointer that came
// from anywhere else may be in the heap, and specs/034 requires the barrier
// wherever that cannot be ruled out.
func frameAddress(a *Value) bool {
	for a != nil && a.Op == OpCopy && len(a.Args) == 1 {
		a = a.Args[0]
	}
	return a != nil && a.Op == OpARM64ADDframe
}

// barrier builds the diamond around the store at index at of b, and returns the
// continuation block the rest of b moved into.
func (w *barrierer) barrier(b *Block, at int) *Block {
	f := w.f
	st := b.Values[at]
	pos := st.Pos
	val, _ := storedPointer(st)
	mem, ok := storeMemory(st)
	if !ok {
		return nil
	}

	cont := f.NewBlock(b.Kind)
	wb := f.NewBlock(BlockPlain)
	// The continuation carries the block's kind, control and successors, and
	// the barrier block is a detour that rejoins it. The order in the layout
	// is b, wb, cont, so that neither new block sits between a block and the
	// successor it falls through to.
	w.place(b, wb, cont)

	cont.Values = append(cont.Values, b.Values[at:]...)
	for _, v := range cont.Values {
		v.Block = cont
	}
	b.Values = b.Values[:at]

	cont.Kind, cont.Control = b.Kind, b.Control
	cont.Succs = b.Succs
	for i, s := range cont.Succs {
		if j := predIndexOf(b, cont.Succs, i, s); j >= 0 {
			s.Preds[j] = cont
		}
	}
	b.Succs = nil

	// The flag, read as a 32-bit word. runtime.writeBarrier declares three
	// bytes of padding after the bool for exactly this load.
	// The flag's address is a static address and not a heap pointer, so it
	// carries the machine pointer type for the same reason the static base
	// does.
	addr := b.NewValue(pos, OpARM64MOVDaddr, machinePtrType, w.staticBase())
	addr.Aux = w.flag()
	flag := b.NewValue(pos, OpARM64MOVWUload, w.uint32Type(), addr, mem)
	// The control is a boolean, which is what a two-way block carries. The
	// width of the comparison comes from the operand's type, so the flag stays
	// a uint32 and the branch is the W form gc emits.
	ctrl := b.NewValue(pos, OpARM64CBNZ, w.boolType(), flag)
	b.Kind = BlockIf
	b.Control = ctrl
	// CBNZ takes the first successor when the operand is not zero, so the
	// barrier block is first and the plain store is the fall-through.
	b.AddEdgeTo(wb)
	b.AddEdgeTo(cont)

	// The address the store writes through, computed in the barrier block
	// because that is the only block that reads it. An indexed store carries
	// the base and the index apart, so the address is built the way the store
	// builds it and the barrier reads the word the store is about to
	// overwrite rather than the base.
	dst, ok := w.destination(wb, st, pos)
	if !ok {
		return nil
	}
	old := wb.NewValue(pos, OpARM64MOVDload, w.ptrType(), dst, mem)
	// The buffer pointer is typed as an integer and not as a pointer, which is
	// the difference between a stack map that describes the frame and one that
	// does not. It points into the P's own write barrier buffer, which is not
	// a heap object: a slot the map marks as holding a pointer is checked when
	// the stack is copied, and runtime.adjustpointers reports this one as
	// "bad pointer in frame" and stops the program. gc types its own barrier's
	// result the same way and for the same reason.
	buf := wb.NewValue(pos, OpARM64LoweredWB, w.uintptrType(), mem)
	// Two slots, because the barrier records the value written and the value
	// overwritten. The name comes from rtsym, which specs/031 makes the one
	// place a runtime symbol is spelled.
	buf.Aux = RuntimeFunc("runtime.gcWriteBarrier2")
	m1 := wb.NewValue(pos, OpARM64MOVDstore, MemType, buf, val, mem)
	m2 := wb.NewValue(pos, OpARM64MOVDstore, MemType, buf, old, m1)
	m2.AuxInt = ir.PtrSize
	wb.AddEdgeTo(cont)

	// The join. Argument i of the phi and slot i of the predecessor list are
	// the same edge, and the edges were added in that order.
	phi := f.newValue(OpPhi, MemType, pos)
	phi.Block = cont
	phi.AddArg(mem)
	phi.AddArg(m2)
	cont.Values = append([]*Value{phi}, cont.Values...)
	st.SetArg(len(st.Args)-1, phi)
	w.done[st] = true
	return cont
}

// destination returns the address a store writes through and the memory it
// takes.
//
// A plain store already holds the address. An indexed store holds a base and an
// index, and the address is the two added the way the store adds them, so the
// barrier reads the old value from the word the store is about to overwrite
// and not from the base.
func (w *barrierer) destination(b *Block, st *Value, pos syntax.Pos) (*Value, bool) {
	switch st.Op {
	case OpARM64MOVDstore:
		return st.Args[0], true
	case OpARM64MOVDstoreidx:
		return b.NewValue(pos, OpARM64ADD, w.ptrType(), st.Args[0], st.Args[1]), true
	case OpARM64MOVDstoreidx8:
		// The index is scaled by the width, which is what the 8 in the name
		// says and what the store's own addressing mode does.
		sh := b.NewValue(pos, OpARM64LSLconst, w.ptrType(), st.Args[1])
		sh.AuxInt = 3
		return b.NewValue(pos, OpARM64ADD, w.ptrType(), st.Args[0], sh), true
	}
	return nil, false
}

// storeMemory returns the memory a store takes, which is its last argument.
func storeMemory(st *Value) (*Value, bool) {
	if len(st.Args) == 0 {
		return nil, false
	}
	m := st.Args[len(st.Args)-1]
	if !IsMemory(m) {
		return nil, false
	}
	return m, true
}

// place puts the two new blocks immediately after b in the layout.
func (w *barrierer) place(b, wb, cont *Block) {
	f := w.f
	keep := f.Blocks[:0]
	for _, c := range f.Blocks {
		if c == wb || c == cont {
			continue
		}
		keep = append(keep, c)
	}
	f.Blocks = keep
	for i, c := range f.Blocks {
		if c == b {
			f.Blocks = append(f.Blocks, nil, nil)
			copy(f.Blocks[i+3:], f.Blocks[i+1:])
			f.Blocks[i+1], f.Blocks[i+2] = wb, cont
			return
		}
	}
	f.Blocks = append(f.Blocks, wb, cont)
}

// flag returns the object naming runtime.writeBarrier.
//
// The name comes from rtsym and is never spelled here, which
// specs/031-runtime-lowering.md requires of every runtime symbol: the table is
// checked against the runtime's own source and a name written at a call site
// is not.
func (w *barrierer) flag() *ir.Object {
	if w.flagObj != nil {
		return w.flagObj
	}
	v := rtsym.LookupVar("runtime.writeBarrier")
	if v == nil {
		panic("ssa: rtsym does not name runtime.writeBarrier")
	}
	w.flagObj = &ir.Object{Name: v.Name, Type: w.uint32Type(), Class: ir.ClassGlobal}
	return w.flagObj
}

func (w *barrierer) uint32Type() *ir.Type {
	if w.u32 == nil {
		t := &ir.Type{Kind: ir.Uint32, Name: "uint32"}
		if err := ir.Layout(t); err != nil {
			panic("ssa: write barrier: " + err.Error())
		}
		w.u32 = t
	}
	return w.u32
}

// ptrType is the type of a word that holds a pointer the collector follows:
// the value being stored, the value being overwritten, and the address they
// are stored through.
func (w *barrierer) ptrType() *ir.Type { return unsafePtrType }

func (w *barrierer) uintptrType() *ir.Type {
	if w.uptr == nil {
		t := &ir.Type{Kind: ir.Uintptr, Name: "uintptr"}
		if err := ir.Layout(t); err != nil {
			panic("ssa: write barrier: " + err.Error())
		}
		w.uptr = t
	}
	return w.uptr
}

func (w *barrierer) boolType() *ir.Type {
	if w.bool == nil {
		t := &ir.Type{Kind: ir.Bool, Name: "bool"}
		if err := ir.Layout(t); err != nil {
			panic("ssa: write barrier: " + err.Error())
		}
		w.bool = t
	}
	return w.bool
}

// staticBase returns the value every global's address is measured from.
//
// The function's own is reused when it has one. Making a second would be
// wrong twice over. It would put a second word in the frame for a value that
// is the same in every function, and, worse, the two would not be described
// the same way: a value's type decides which of its words the collector
// follows, and two OpSB values with two types are two answers to that
// question.
//
// The type is the machine pointer of specs/027, which holds no pointer the
// collector may follow. The static base is the address a global's address is
// measured from and it is neither in the heap nor in a frame. Typing it as a
// pointer marks its spill slot in the locals bitmap, runtime.adjustpointers
// reads that word when a stack is copied, and the program stops with "bad
// pointer in frame". Lowering's own SB is typed this way and for this reason.
func (w *barrierer) staticBase() *Value {
	if w.sb != nil && !w.sb.dead {
		return w.sb
	}
	f := w.f
	for _, v := range f.Entry.Values {
		if v.Op == OpSB {
			w.sb = v
			return v
		}
	}
	v := f.newValue(OpSB, machinePtrType, f.Entry.Pos)
	v.Block = f.Entry
	f.Entry.Values = append(f.Entry.Values, nil)
	copy(f.Entry.Values[1:], f.Entry.Values)
	f.Entry.Values[0] = v
	w.sb = v
	return v
}
