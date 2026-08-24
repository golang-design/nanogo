// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj/arm64"
	"golang.design/x/nanogo/ssa"
)

// The frame, the prologue, the epilogue and the stack-growth tail of
// specs/042-arm64-backend.md and specs/035-goroutines-and-stack-growth.md.
//
// # The frame
//
// Addresses grow upwards. SP at entry is the caller's stack pointer, and the
// prologue subtracts the frame size from it. Written from the new stack
// pointer up:
//
//	newSP + size      the caller's stack pointer, and the caller's saved
//	                  frame pointer one word below it
//	newSP + size - 8  reserved: the caller wrote its frame pointer here
//	...               spill slots and frame objects
//	newSP + 8         the outgoing argument area
//	newSP + 0         the saved link register
//	newSP - 8         this frame's saved frame pointer, below the stack
//	                  pointer
//
// Three consequences, each of which is a bug that does not fail a test:
//
//   - The incoming argument area starts at SP+8 at entry, not at SP. The word
//     at SP is where the callee will save the link register, and every
//     argument offset is measured from SP+8. This is what
//     specs/030-abi.md's spill space means on this target.
//   - A call's outgoing arguments live at the bottom of the caller's frame,
//     because the callee reads them at its own SP+8, which is the caller's
//     newSP+8. The caller reserves that space even for arguments that travel
//     in registers, since the callee spills them there when it grows the
//     stack.
//   - A frame reserves eight or sixteen bytes at its top that nothing in it
//     uses. They hold the caller's saved frame pointer, which the caller
//     wrote below its own stack pointer before this frame existed. Without the
//     reservation, a call would overwrite its caller's saved frame pointer.
//     This is the reason the frame size is not simply the locals rounded up.
//
// specs/042 says the frame pointer is saved below the new stack pointer and
// that the frame size does not include it. Both halves are true and together
// they are misleading: the space is not in the frame that saves it, it is in
// the frame of the function that frame calls.
//
// # Where Varp is, and why the layout has to know
//
// The collector scans the block [Varp - nbit*PtrSize, Varp), and Varp is not
// this file's choice. The runtime computes it (runtime/traceback.go): the
// frame pointer is the caller's stack pointer, which is newSP + size, and the
// word below it is the caller's saved frame pointer, so
//
//	Varp = newSP + size - 8
//	Argp = newSP + size + 8
//
// The layout of specs/027-liveness-and-stackmaps.md must therefore put the
// top of the locals area one word below the top of the frame, and the group
// of pointer-holding slots directly under it. ssa.FrameConfig says that with
// SaveFP set and SaveRA clear: the reserved word above the locals is the
// caller's frame pointer, and the link register is the first word of the
// outgoing argument area rather than a word above the locals. layout builds
// that configuration and checkFrame asserts the result.

// The stack-growth thresholds of internal/abi/stack.go.
//
// StackSmall is the space the runtime guarantees below the stack guard, so a
// function whose frame fits in it can compare the stack pointer against the
// guard directly. StackBig is the bound below which neither SP - framesize nor
// guard - StackSmall can underflow.
//
// They are constants of the runtime, not of the compiler, and
// specs/035-goroutines-and-stack-growth.md says to read them from the runtime
// rather than hard-code them. The test does read them, from
// internal/abi/stack.go, and fails when they move.
const (
	stackSmall = 128
	stackBig   = 4096
)

// maxPreIndex is the largest frame a single MOVD.W can push.
//
// The pre-indexed store carries a signed 9-bit offset, so it reaches -256, and
// the stack pointer must stay 16-byte aligned, which leaves 240. A larger
// frame is pushed by computing the new stack pointer in a register first.
// specs/042's listing is the small form and does not say that the large form
// exists.
const maxPreIndex = 240

// frameScratch is the register the large-frame prologue computes the new stack
// pointer in.
//
// It is allocatable, and it is free here: the prologue runs before any value
// of the function is live, and the only live registers at that point are the
// incoming arguments, which the ABI places in R0 to R15. gc uses the same
// register for the same reason.
const frameScratch = arm64.R20

// frame is the layout of one function's stack frame.
type frame struct {
	// size is what the prologue subtracts from the stack pointer. It is a
	// multiple of 16 and it is zero for a leaf function that needs no frame.
	size int64

	// args is the size of the incoming argument area, which
	// specs/030-abi.md reserves whether or not the arguments arrive in
	// registers.
	args int64

	// outArgs is the size of the outgoing argument area, at the bottom of the
	// frame. It is the largest argument area of any call this function makes.
	outArgs int64

	// leaf reports that the function makes no call. Together with a small
	// frame it is what lets the stack-growth check be skipped entirely.
	leaf bool

	// nosplit reports that no stack-growth check is emitted.
	nosplit bool

	// in holds one entry per word of the incoming arguments, in the order
	// specs/030-abi.md assigns them. A five-field struct in registers is five
	// entries, because the spill and the store the arguments bitmap needs are
	// per word.
	in []place
}

// locals returns the value FuncInfo carries.
//
// It is the frame size without the link register slot, which is the number gc
// writes and the number go tool compile -S prints as locals.
func (f *frame) locals() int64 {
	if f.size == 0 {
		return 0
	}
	return f.size - 8
}

// place is where one word of an argument or a result travels.
//
// It is one part of one top-level value: specs/030-abi.md assigns a register
// per word, so a five-field struct in registers is five of these.
type place struct {
	// reg is the register the word arrives in, when inReg is set.
	reg   arm64.Reg
	inReg bool

	// off is the byte offset of the word in the argument area. Every word has
	// one, including one that travels in a register, because the stack-growth
	// tail spills it there.
	off int64

	// typ describes the word, so the spill can use the right store.
	typ *ir.Type
}

// wordPlaces flattens an assignment into one entry per word.
//
// It is what the stack-growth tail and the arguments bitmap need: both work a
// word at a time, and a five-field struct in registers is five words. The
// offsets become absolute, because a part's offset is within its value.
func wordPlaces(vals []ssa.ABIValue) []place {
	out := make([]place, 0, len(vals))
	for i := range vals {
		v := &vals[i]
		for j := range v.Parts {
			out = append(out, partPlace(v, &v.Parts[j]))
		}
	}
	return out
}

func partPlace(v *ssa.ABIValue, p *ssa.ABIPart) place {
	pl := place{off: v.Off + p.Off, typ: p.Type}
	if v.InReg && p.Reg != ssa.NoReg {
		pl.reg, pl.inReg = arm64.Reg(p.Reg), true
	}
	return pl
}

// valuePlaces returns one entry per value passed, which is what a call site
// and a return need: there the placement and the operand list are walked
// together, so they must have the same length.
//
// A value of more than one word is refused. specs/030-abi.md's assignment pass
// splits every such value that travels in registers, so one that reaches here
// travels in the argument area whole, and the code generator has no form that
// writes it there.
//
// A value a floating-point register would carry is refused too. obj/arm64's
// floating-point encoder is specs/042 group 6 and the rules that need it are
// not written, so such a value would be placed correctly and never encoded.
func valuePlaces(vals []ssa.ABIValue) ([]place, error) {
	out := make([]place, 0, len(vals))
	for i := range vals {
		v := &vals[i]
		if len(v.Parts) > 1 {
			return nil, fmt.Errorf("value %d is %v, which does not fit one register", i, v.Type)
		}
		if len(v.Parts) == 0 {
			// A value of no width occupies no register and no word.
			out = append(out, place{off: v.Off, typ: v.Type})
			continue
		}
		if c, _ := ssa.ClassOfType(v.Parts[0].Type); c == ssa.ClassFloat {
			return nil, fmt.Errorf("value %d is %v, and this target has no floating-point encoder", i, v.Type)
		}
		out = append(out, partPlace(v, &v.Parts[0]))
	}
	return out, nil
}

// linkSlot is the word at the bottom of the frame that holds the saved link
// register.
//
// specs/042-arm64-backend.md puts it there rather than above the locals, so it
// is reserved as the first word of the outgoing argument area: the callee
// reads its own arguments at its stack pointer plus this, which is the
// runtime's MinFrameSize for this target.
const linkSlot = 8

// layout computes the frame, from ssa.LayoutFrame and from nothing else.
//
// specs/027-liveness-and-stackmaps.md owns the geometry, because the stack map
// it builds describes words by their distance from Varp. A second placement
// here would be a map that describes the wrong words, which is why this
// function computes what ssa.FrameConfig cannot know and then reads the
// answer back.
//
// What it computes is the target's part: which word the link register
// occupies, how large the outgoing argument area is, and where the incoming
// arguments are.
func (e *emitter) layout() error {
	f := &e.frame
	abi := e.f.ABI

	// The outgoing argument area is the largest a call needs. A call whose
	// arguments all travel in registers still needs the space, because the
	// callee spills them into it when it grows the stack.
	for _, b := range e.f.Blocks {
		for _, v := range b.Values {
			if !v.Op.IsCall() {
				continue
			}
			f.leaf = false
			n, err := e.callArgsSize(v)
			if err != nil {
				return err
			}
			if n > f.outArgs {
				f.outArgs = n
			}
		}
	}

	items, err := ssa.FrameItems(e.a)
	if err != nil {
		return err
	}

	// A leaf with nothing in its frame keeps the caller's stack pointer and
	// never saves the link register. gc marks it NOFRAME, and the runtime
	// then reads Varp as the stack pointer itself, so the frame reserves no
	// word above the locals either.
	frameless := f.leaf && len(items) == 0
	cfg := ssa.FrameConfig{
		Align: 16,
		// The link register sits at the bottom of the frame rather than
		// above the locals, so SaveRA is false and the word is reserved
		// inside the outgoing argument area instead.
		SaveRA: false,
		// SaveFP reserves the word at the top of the frame. It holds the
		// *caller's* saved frame pointer, which the caller wrote below its
		// own stack pointer before this frame existed, and the runtime
		// excludes it from the locals: traceback.go computes
		// varp = fp - PtrSize whenever the frame is not empty. A layout that
		// did not reserve it would put a local where the caller's frame
		// pointer is and would place Varp one word above where the collector
		// looks.
		SaveFP:   !frameless,
		ArgsSize: f.args,
	}
	if !frameless {
		cfg.OutArgs = linkSlot + f.outArgs
	}
	// The arguments bitmap describes the area by whole parameters and not by
	// the words they were split into. The type of a parameter carries the
	// pointer map the collector reads, and reassembling it from the parts
	// would be a second computation of something ir.Layout already did.
	for i := range abi.In {
		cfg.Args = append(cfg.Args, ssa.FrameArg{
			Name: fmt.Sprintf("arg%d", i),
			Off:  abi.In[i].Off,
			Type: abi.In[i].Type,
		})
	}
	// A result the registers could not hold occupies words of the same area
	// and holds no pointer the collector may follow. It has none at any
	// safepoint of this function: the copy that fills it is the last thing
	// before the return, so every safepoint sees the words as the previous
	// frame left them. Describing them by the result's own type would make
	// the collector follow whatever is there, and gc's bitmap for such a
	// function is the same all-zero map over the same width.
	for i := range abi.Out {
		av := &abi.Out[i]
		if av.InReg || av.Type == nil || av.Type.Size == 0 {
			continue
		}
		cfg.Args = append(cfg.Args, ssa.FrameArg{
			Name: fmt.Sprintf("res%d", i),
			Off:  av.Off,
			Type: opaqueArea(av.Type.Size),
		})
	}
	fr, err := ssa.LayoutFrame(e.f, items, cfg)
	if err != nil {
		return err
	}
	e.items, e.fr = fr.Items, fr
	f.size = fr.Size

	if err := e.checkFrame(frameless); err != nil {
		return err
	}

	// The offsets the generator emits are the layout's, read back by item
	// rather than recomputed.
	e.slotOff = make([]int64, len(e.a.Slots))
	for i := range e.slotOff {
		e.slotOff[i] = -1
	}
	for i := range e.items {
		it := &e.items[i]
		switch it.Kind {
		case ssa.ItemSpill:
			e.slotOff[it.Index] = it.Off
		case ssa.ItemObject:
			e.frames[it.Obj] = it.Off
		}
	}
	// The slots of an argument area that this function addresses. The layout
	// knows nothing about them, because specs/030-abi.md's assignment keeps
	// them out of Func.Frame: they are not locals, and a second slot in the
	// locals would be described twice by the collector. This is where the
	// offset the assignment gave each of them becomes an offset from the
	// stack pointer.
	//
	// The incoming area is above the frame the prologue pushed, so the frame
	// is added back. The outgoing area is at the bottom of this frame, over
	// the word the callee saves its link register in.
	for i := range abi.In {
		av := &abi.In[i]
		if !av.Home || av.Obj == nil {
			continue
		}
		e.frames[av.Obj] = f.size + linkSlot + av.Off
	}
	for i := range abi.Homes {
		h := &abi.Homes[i]
		if h.Incoming {
			e.frames[h.Obj] = f.size + linkSlot + h.Off
			continue
		}
		e.frames[h.Obj] = linkSlot + h.Off
	}

	f.nosplit = f.size == 0 || (f.leaf && f.size < stackSmall)
	return nil
}

// checkFrame asserts that the frame the prologue will build is the frame the
// layout described.
//
// specs/027-liveness-and-stackmaps.md states the obligation this discharges:
// the runtime does not read the layout, it computes the block it scans from
// Varp and the bitmap, so a frame whose Varp is elsewhere is scanned at the
// wrong address with a bitmap that is internally consistent. The check is here
// rather than in a comment because the two sides of it are computed by
// different packages.
func (e *emitter) checkFrame(frameless bool) error {
	fr, size := e.fr, e.frame.size
	if size%16 != 0 {
		// The stack pointer must stay 16-byte aligned at every instruction.
		return fmt.Errorf("the frame is %d bytes, which is not 16-byte aligned", size)
	}
	if frameless {
		if size != 0 {
			return fmt.Errorf("the function has nothing in its frame and the layout gave it %d bytes", size)
		}
		return nil
	}
	if size < 2*linkSlot {
		return fmt.Errorf("the frame is %d bytes and holds the link register and the caller's frame pointer", size)
	}
	// varp = fp - PtrSize on this target, and fp is the caller's stack
	// pointer, which is this frame's own plus its size.
	if want := size - 8; fr.Varp != want {
		return fmt.Errorf("the layout puts Varp at %d and the frame the prologue builds puts it at %d", fr.Varp, want)
	}
	if fr.LocalsBase < linkSlot {
		return fmt.Errorf("the locals area starts at %d, over the saved link register at %d", fr.LocalsBase, linkSlot)
	}
	// The runtime reads the locals bitmap only when varp - sp is larger than
	// the target's stack alignment (runtime/stkframe.go, getStackMap), so a
	// pointer in a frame below that bound is a pointer it never scans.
	if fr.LocalsBits > 0 && size-8 <= 16 {
		return fmt.Errorf("%d pointer words live in a frame of %d bytes, which the runtime does not scan", fr.LocalsBits, size)
	}
	return nil
}

// opaqueArea returns a type of n bytes that holds no pointer.
//
// It describes a region of the argument area whose contents the collector must
// not follow. A type with no pointer bits is the way to say that, and the size
// is what keeps the region inside the map the layout builds.
func opaqueArea(n int64) *ir.Type {
	return &ir.Type{Kind: ir.Array, Size: n, Align: 8, Len: n / 8,
		Elem: &ir.Type{Kind: ir.Uintptr, Size: 8, Align: 8, Name: "uintptr"},
		Name: "result area"}
}

// callArgs places the operands of one call.
//
// The placement is ssa.ABICallArgs and not a second walk here, because the
// allocator pre-coloured the same operands from the same function: two
// placements that disagree is a call that reads its arguments from the wrong
// place. lo is the index of the first operand in v.Args, so the caller can
// walk the operands and the placement together.
func (e *emitter) callArgs(v *ssa.Value) (pl []place, lo int, size int64, err error) {
	vals, lo, size, err := ssa.ABICallArgs(e.a.Target, v)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("v%d (%v): %w", v.ID, v.Op, err)
	}
	pl, err = valuePlaces(operandPlaces(vals))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("v%d (%v): %w", v.ID, v.Op, err)
	}
	return pl, lo, size, nil
}

// operandPlaces drops the values that are in the argument area already, so
// that what is left lines up with the operand list one for one.
func operandPlaces(vals []ssa.ABIValue) []ssa.ABIValue {
	out := make([]ssa.ABIValue, 0, len(vals))
	for i := range vals {
		if vals[i].Copied {
			continue
		}
		out = append(out, vals[i])
	}
	return out
}

// callArgsSize returns the size of the argument area one call needs.
func (e *emitter) callArgsSize(v *ssa.Value) (int64, error) {
	_, _, size, err := e.callArgs(v)
	return size, err
}

// prologue emits the stack-growth check and the frame setup.
//
// The listing is specs/042-arm64-backend.md's, with the forms that listing
// does not have: the comparison that a frame larger than the guard region
// needs, and the frame push that a frame too large for a pre-indexed store
// needs. Both are gc's, and the note in each says why the simple form is not
// enough.
func (e *emitter) prologue() {
	f := &e.frame
	if !f.nosplit {
		// The goroutine register is spelled g in Plan 9 syntax and is R28
		// here. The stack guard is at a fixed offset in it.
		e.mem(arm64.LoadX, arm64.RegTrampLo, arm64.RegG, 2*8)

		switch {
		case f.size <= stackSmall:
			// CMP R16, RSP needs the add-and-subtract extended-register
			// class. In the shifted-register class register 31 reads as the
			// zero register, so the instruction has no encoding there.
			e.word(arm64.CmpRegRegSP(arm64.Size64, arm64.RSP, arm64.RegTrampLo))
		case f.size <= stackBig:
			// The stack pointer alone under-tests the guard once the frame is
			// larger than the guaranteed region, so the comparison is against
			// the stack pointer the frame will leave behind.
			e.subSP(arm64.RegTrampHi, f.size-stackSmall, false)
			e.word(arm64.CmpRegReg(arm64.Size64, arm64.RegTrampHi, arm64.RegTrampLo))
		default:
			// A frame this large can make SP - framesize underflow, and an
			// underflowed comparison succeeds when it must fail, so the
			// subtraction sets the flags and a borrow goes straight to the
			// tail.
			e.subSP(arm64.RegTrampHi, f.size-stackSmall, true)
			e.branchToGrowstack(arm64.LO)
			e.word(arm64.CmpRegReg(arm64.Size64, arm64.RegTrampHi, arm64.RegTrampLo))
		}
		e.branchToGrowstack(arm64.LS)
	}
	if f.size == 0 {
		return
	}
	if f.size <= maxPreIndex {
		// One instruction pushes the link register and moves the stack
		// pointer, so a signal that arrives inside the prologue never sees a
		// half-written frame.
		e.memIf(arm64.MemPreIndex, arm64.StoreX, arm64.RegLink, arm64.RSP, -f.size)
		// The delta is in effect from the instruction after the one that
		// moved the stack pointer. A row one instruction late tells the
		// runtime the frame is not there while it is, and the unwinder then
		// finds the caller at the wrong address.
		e.spDelta = f.size
		e.markSP()
		e.memIf(arm64.MemUnscaled, arm64.StoreX, arm64.RegFramePtr, arm64.RSP, -8)
	} else {
		// The frame pointer and the link register are written before the
		// stack pointer moves, for the same reason and by the other order:
		// nothing may be stored below the stack pointer that a signal handler
		// could overwrite.
		e.subSP(frameScratch, f.size, false)
		e.memIf(arm64.MemUnscaled, arm64.StoreX, arm64.RegFramePtr, frameScratch, -8)
		e.memIf(arm64.MemUnsignedOffset, arm64.StoreX, arm64.RegLink, frameScratch, 0)
		e.word(arm64.MovSP(arm64.Size64, arm64.RSP, frameScratch))
		e.spDelta = f.size
		e.markSP()
	}
	// The frame pointer points at the word below the stack pointer, which is
	// where this frame saved the caller's.
	w, ok := arm64.SubRegImm(arm64.Size64, arm64.RegFramePtr, arm64.RSP, 8)
	e.wordIf(w, ok, "SUB $8, RSP, R29")
}

// subSP emits dst = RSP - imm, setting the flags when the caller asks.
//
// The immediate field is twelve bits with an optional twelve-bit shift, so a
// frame between those two ranges does not fit. The constant then goes into a
// register first, and the register is R27, which specs/030-abi.md reserves as
// the assembler's scratch for exactly this expansion and which the allocator
// never chooses.
func (e *emitter) subSP(dst arm64.Reg, imm int64, flags bool) {
	if !flags {
		if w, ok := arm64.SubRegImm(arm64.Size64, dst, arm64.RSP, imm); ok {
			e.word(w)
			return
		}
	} else if w, ok := arm64.SubsRegImm(arm64.Size64, dst, arm64.RSP, imm); ok {
		e.word(w)
		return
	}
	e.constInto(arm64.RegAsmScratch, imm)
	if flags {
		e.word(arm64.SubsRegRegSP(arm64.Size64, dst, arm64.RSP, arm64.RegAsmScratch))
		return
	}
	e.word(arm64.SubRegRegSP(arm64.Size64, dst, arm64.RSP, arm64.RegAsmScratch))
}

// addSP emits dst = RSP + imm, with the same expansion.
func (e *emitter) addSP(dst arm64.Reg, imm int64) {
	if w, ok := arm64.AddRegImm(arm64.Size64, dst, arm64.RSP, imm); ok {
		e.word(w)
		return
	}
	e.constInto(arm64.RegAsmScratch, imm)
	e.word(arm64.AddRegRegSP(arm64.Size64, dst, arm64.RSP, arm64.RegAsmScratch))
}

// constInto materialises a constant into a register.
func (e *emitter) constInto(dst arm64.Reg, v int64) {
	if w, ok := arm64.MovLogicalImm(arm64.Size64, dst, uint64(v)); ok {
		e.word(w)
		return
	}
	var out [4]uint32
	n := arm64.MovConst(arm64.Size64, dst, v, out[:])
	if n == 0 {
		e.fail("the constant %d does not materialise", v)
		return
	}
	for i := 0; i < n; i++ {
		e.word(out[i])
	}
}

// epilogue tears the frame down and returns.
func (e *emitter) epilogue() {
	f := &e.frame
	at := int64(e.pc())
	defer func() {
		if f.size == 0 {
			// Nothing was torn down, because there was no frame. A function
			// that never moves the stack pointer has no range where its frame
			// is half gone.
			return
		}
		// The teardown is a range no asynchronous preemption may stop in: the
		// frame is gone from the stack pointer's point of view before the
		// return is taken, and specs/027-liveness-and-stackmaps.md marks such
		// a range rather than describing it. A function has one teardown per
		// return, so this is a range of its own rather than the single
		// EpilogueStart the pc-value builder also accepts.
		e.unsafe = append(e.unsafe, ssa.PCRange{Lo: at, Hi: int64(e.pc())})
	}()
	if f.size != 0 {
		e.memIf(arm64.MemUnscaled, arm64.LoadX, arm64.RegFramePtr, arm64.RSP, -8)
		if f.size <= maxPreIndex {
			e.memIf(arm64.MemPostIndex, arm64.LoadX, arm64.RegLink, arm64.RSP, f.size)
		} else {
			e.memIf(arm64.MemUnsignedOffset, arm64.LoadX, arm64.RegLink, arm64.RSP, 0)
			e.addSP(arm64.RSP, f.size)
		}
		e.spDelta = 0
		e.markSP()
	}
	e.word(arm64.Ret(arm64.RegLink))
}

// growstack emits the tail that grows the stack and runs the function again.
//
// It saves the argument registers, because the function is re-executed from
// its first instruction and its arguments must be intact. The argument area of
// specs/030-abi.md is where they go, and it is in the caller's frame, so the
// tail runs before the frame is pushed and the offsets are from the entry
// stack pointer.
//
// The link register is moved to R3 after the arguments are saved and not
// before. runtime.morestack reads the caller's return address from R3, and R3
// is also the fourth integer argument register, so the other order would
// destroy an argument. Neither specs/042 nor specs/035 says this, and without
// it the goroutine resumes at whatever R3 happened to hold.
func (e *emitter) growstack() {
	if e.frame.nosplit {
		return
	}
	e.growPC = e.pc()
	e.spDelta = 0
	e.markSP()
	for _, p := range e.frame.in {
		if !p.inReg {
			continue
		}
		e.argSpill(p, true)
	}
	e.word(arm64.MovRegReg(arm64.Size64, arm64.R3, arm64.RegLink))
	c, ok := morestackCallee()
	if !ok {
		e.fail("%s is not in rtsym, so the stack-growth tail has no callee", morestackName)
		return
	}
	e.call(e.pc(), c)
	w, wok := arm64.Bl(0)
	e.wordIf(w, wok, "BL %s", c.name)
	for _, p := range e.frame.in {
		if !p.inReg {
			continue
		}
		e.argSpill(p, false)
	}
	// Re-execute the function. The check runs again with the larger stack and
	// falls through this time.
	e.branchToPC(0)
	// The whole tail is unsafe, which specs/042-arm64-backend.md states: the
	// arguments are in flight between their registers and the argument area,
	// and the tail re-enters the function from its first instruction.
	e.unsafe = append(e.unsafe, ssa.PCRange{Lo: int64(e.growPC), Hi: int64(e.pc())})
}

// argSpill stores an argument register to its slot in the argument area, or
// loads it back.
//
// The area starts at eight bytes above the entry stack pointer, which is the
// word the link register will occupy. An offset measured from the stack
// pointer itself writes over the return address of the function that is about
// to be re-entered.
func (e *emitter) argSpill(p place, store bool) {
	op, ok := memOpFor(p.typ, store)
	if !ok {
		e.fail("argument of type %v has no load or store", p.typ)
		return
	}
	e.memIf(arm64.MemUnsignedOffset, op, p.reg, arm64.RSP, 8+p.off)
}

// memOpFor returns the load or store of a value of type t.
//
// A store writes the low bits and has no signedness. A load has one, and the
// wrong choice is a value that compares equal to the right one half the time,
// so the load comes from ssa rather than from a switch here.
func memOpFor(t *ir.Type, store bool) (arm64.MemOp, bool) {
	if t == nil {
		return 0, false
	}
	if store {
		op, ok := ssa.ARM64StoreOp(t.Size)
		if !ok {
			return 0, false
		}
		return ssa.ARM64MemOp(op)
	}
	op, ok := ssa.ARM64LoadOp(t)
	if !ok {
		return 0, false
	}
	return ssa.ARM64MemOp(op)
}
