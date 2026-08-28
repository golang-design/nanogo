// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package ssagen turns an allocated SSA function into an object file symbol.
//
// It is the last stage of specs/002-architecture.md's pipeline and the first
// place where a Go function exists as bytes. Above it, a function is a graph
// of values with registers assigned; below it, the linker reads a symbol with
// instructions, relocations and the tables the runtime needs.
//
// # What it does, in the order specs/041-instruction-encoding.md gives
//
//  1. Lay values out in scheduled order and compute each instruction's size.
//  2. Resolve local branch targets, now that the offsets are known.
//  3. Emit bytes and collect relocations.
//  4. Emit the pc-value tables of specs/040-object-format.md alongside, since
//     they are indexed by the offsets step 3 produced.
//
// Steps 1 and 2 are one pass here. Every arm64 instruction is four bytes, so
// no size depends on a distance and no iteration is needed. specs/043 owns the
// amd64 case, where an instruction's size depends on a displacement that
// depends on sizes, and the loop has to exist.
//
// # What it does with the calling convention
//
// Nothing is decided here. specs/030-abi.md's assignment pass placed the
// parameters, the results and every call's operands, and the allocator
// pre-coloured them from that same answer through ssa.Target's DefReg and
// UseReg. This package reads the assignment and emits the moves that realise
// it. Two placements computed apart would be a call that reads its arguments
// from the wrong place, so there is one and this is not it.
//
// The moves at an entry, a call and a return are parallel moves, because the
// home of argument 0 can be the register argument 1 arrives in.
//
// # The frame and the collector's tables
//
// The frame is specs/027-liveness-and-stackmaps.md's and this package does not
// compute one of its own. ssa.LayoutFrame places every item and this package
// supplies the target's part of the geometry: which word holds the saved link
// register, how large the outgoing argument area is, and where the incoming
// arguments are. prologue.go asserts that the frame the prologue builds is the
// frame the layout described, because the collector reads the layout's answer
// through a bitmap and the generator's answer through the stack pointer.
//
// The tables the collector reads are in stackmap.go. A function whose tables
// cannot be built is not emitted: a text symbol with a partial map is a
// program that runs until a collection happens at the wrong moment.
package ssagen

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/obj/arm64"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/syntax"
)

// minLC is the length of the shortest instruction on this target. A pc delta
// in a pc-value table counts instructions, not bytes.
const minLC = 4

// Options describe the symbol a function becomes.
type Options struct {
	// Sym is the linker symbol name. It defaults to the function's own.
	Sym string

	// ABI is the calling convention the symbol is defined with. nanogo emits
	// ABIInternal for a Go function; ABI0 belongs to symbols defined in
	// assembly (specs/030-abi.md).
	ABI uint16

	// File is the source file the pc-file table names, and Line is the line
	// the function starts on.
	File string
	Line int32

	// Fset resolves a value's position to a line for the pc-line table.
	//
	// A position is an offset in a file set and nothing below the front end
	// can resolve one on its own (specs/010-scanner-and-positions.md). Without
	// it every instruction is attributed to Line, which is a table the runtime
	// reads and a debugger believes, so the field is worth passing.
	Fset *syntax.FileSet

	// Gotype is the function's type descriptor.
	//
	// It is optional and it is empty in every caller today, because
	// specs/032-type-descriptors-and-itabs.md has no writer yet. gc attaches
	// a type descriptor to a global variable and not to a text symbol, so an
	// empty reference here is what gc produces as well.
	Gotype obj.SymRef
}

// A Result is one compiled function.
//
// The text symbol is the function. The other symbols are the auxiliary ones
// specs/040-object-format.md attaches to it, in the order gc writes them.
type Result struct {
	// Text is the function symbol, with its relocations.
	Text *obj.Symbol

	// FuncInfo carries the frame size and the argument size. specs/040
	// records that a text symbol without it crashes cmd/link in its DWARF
	// pass with no diagnostic, so it is mandatory and never nil.
	FuncInfo *obj.Symbol

	// Pcsp, Pcfile and Pcline are the pc-value tables. They are mandatory
	// too, and for a reason specs/040 does not give: cmd/link reads all three
	// through a symbol index it does not check, so a text symbol that carries
	// a FuncInfo and no pc-value table faults in the pclntab pass.
	Pcsp, Pcfile, Pcline *obj.Symbol

	// Funcdata and Pcdata are the tables the garbage collector reads, indexed
	// by the FUNCDATA and PCDATA index the runtime defines.
	//
	// The position is the index. The object format carries no index with an
	// entry: the linker reads them in order, so a table that is absent is an
	// empty entry and never a missing one.
	Funcdata []*obj.Symbol
	Pcdata   []*obj.Symbol

	// DwarfInfo is the subprogram symbol of specs/046-debug-info.md, and it
	// carries no DIE.
	//
	// It is mandatory, and the reason is not debugging. cmd/link's DWARF pass
	// walks every text symbol of a compilation unit and reads the relocations
	// of its AuxDwarfInfo symbol without checking that there is one
	// (cmd/link/internal/ld, writedebugaddr): the two lines beside it check
	// the range and the location symbol and this one does not. A text symbol
	// with no entry resolves to symbol 0 and the linker panics with "trying
	// to get oreader for invalid sym 0".
	//
	// That pass runs only under the dwarf5 experiment, which internal/buildcfg
	// turns on for every target except darwin, ios and aix. So an object
	// without this symbol links on the machine most of this compiler was
	// developed on and links nowhere else, which is why the symbol is here
	// rather than in specs/046's own milestone.
	DwarfInfo *obj.Symbol

	// Gotype is the type descriptor reference the symbol carries, if the
	// caller gave one.
	Gotype obj.SymRef

	// maps is what the bitmaps above were built from. A caller reads the
	// symbols; this is here so that a test of this package can ask which
	// bitmap a safepoint selected, which the bytes alone do not say.
	maps *ssa.StackMaps

	// Frame is the number of bytes the prologue subtracts from the stack
	// pointer, Locals is what FuncInfo carries, and Args is the size of the
	// incoming argument area.
	Frame, Locals, Args int64
}

// Emit compiles one allocated function into a symbol.
//
// a must be the allocation of f. p receives the references the relocations
// name; the symbols the result holds are not added to it, because the caller
// decides what goes into the object and in what order.
func Emit(f *ssa.Func, a *ssa.Alloc, p *obj.Package, opt Options) (*Result, error) {
	switch {
	case f == nil:
		return nil, errors.New("ssagen: nil function")
	case a == nil || a.Func != f:
		return nil, errors.New("ssagen: the allocation is not this function's")
	case p == nil:
		return nil, errors.New("ssagen: nil package")
	}
	e := &emitter{f: f, a: a, opt: opt, pkg: p, syms: newSymbols(p)}
	if opt.Sym == "" {
		e.opt.Sym = f.Sym
	}
	if e.opt.Sym == "" {
		e.opt.Sym = f.Name
	}
	if e.opt.File == "" {
		// A function with no file has no pc-file table, and the tools do not
		// tolerate one: go tool objdump reads the table for every instruction
		// it prints and faults on an empty one. gc gives a function it
		// synthesised this name for the same reason.
		e.opt.File = "<autogenerated>"
	}
	e.run()
	if err := e.err(); err != nil {
		return nil, err
	}
	return e.result()
}

// Add puts a result and its auxiliary symbols into an object.
//
// The auxiliary symbols are added first, because a symbol reference is an
// index into the definition array and the text symbol names them.
func (r *Result) Add(p *obj.Package) (obj.SymRef, error) {
	if r == nil || r.Text == nil {
		return obj.SymRef{}, errors.New("ssagen: empty result")
	}
	// Aux entries are written in the order they are given and gc's order is
	// Gotype, FuncInfo, Funcdata, DWARF, Pcsp, Pcfile, Pcline, Pcinline,
	// Pcdata.
	var entries []obj.Aux
	if !r.Gotype.IsZero() {
		entries = append(entries, obj.Aux{Type: obj.AuxGotype, Sym: r.Gotype})
	}
	if r.FuncInfo == nil {
		return obj.SymRef{}, errors.New("ssagen: the result carries no FuncInfo")
	}
	// A plain definition, not a content-addressable one: cmd/link reads the
	// FuncInfo of a text symbol at the symbol index the auxiliary entry names,
	// without resolving which index space that is.
	entries = append(entries, obj.Aux{Type: obj.AuxFuncInfo, Sym: p.AddDef(r.FuncInfo)})
	for i, s := range r.Funcdata {
		if s == nil {
			return obj.SymRef{}, fmt.Errorf("ssagen: funcdata %d is absent, and the position of an entry is its index", i)
		}
		entries = append(entries, obj.Aux{Type: obj.AuxFuncdata, Sym: p.AddHashedDef(s)})
	}
	if r.DwarfInfo == nil {
		return obj.SymRef{}, errors.New("ssagen: the result carries no DWARF subprogram symbol, and cmd/link reads one for every text symbol of a compilation unit")
	}
	// A plain definition, and named: the linker resolves it by index and then
	// asserts that its type is SDWARFFCN, so it is not an anonymous payload.
	entries = append(entries, obj.Aux{Type: obj.AuxDwarfInfo, Sym: p.AddDef(r.DwarfInfo)})
	// The pc-value tables are content-addressable, as gc makes them: two
	// functions with the same table are one symbol in the linked binary.
	for _, x := range []struct {
		typ obj.AuxType
		sym *obj.Symbol
	}{
		{obj.AuxPcsp, r.Pcsp},
		{obj.AuxPcfile, r.Pcfile},
		{obj.AuxPcline, r.Pcline},
	} {
		if x.sym == nil {
			continue
		}
		entries = append(entries, obj.Aux{Type: x.typ, Sym: p.AddHashedDef(x.sym)})
	}
	for i, s := range r.Pcdata {
		if s == nil {
			return obj.SymRef{}, fmt.Errorf("ssagen: pcdata %d is absent, and the position of an entry is its index", i)
		}
		entries = append(entries, obj.Aux{Type: obj.AuxPcdata, Sym: p.AddHashedDef(s)})
	}
	r.Text.Aux = entries
	if r.Text.Flag&obj.SymFlagDupok != 0 {
		// A duplicate-tolerant definition is a non-package definition, which
		// is gc's rule in obj.isNonPkgSym. cmd/link deduplicates by name in
		// that index space only: loader.addSym takes a package definition as
		// unique by construction, so two objects that both generate one
		// method wrapper would put two of it in the binary.
		return p.AddNonPkgDef(r.Text), nil
	}
	return p.AddDef(r.Text), nil
}

// fixup is a branch whose target is a block, resolved once every block's
// offset is known.
type fixup struct {
	at    int32 // the byte offset of the branch instruction
	block ssa.ID
	pc    int32 // an absolute target, used when block is invalid
	kind  fixupKind
	cond  arm64.Cond
	reg   arm64.Reg
	size  arm64.Size
}

type fixupKind uint8

const (
	fixB fixupKind = iota
	fixCond
	fixCbz
	fixCbnz
)

// emitter holds the state of one function's emission.
type emitter struct {
	f    *ssa.Func
	a    *ssa.Alloc
	opt  Options
	pkg  *obj.Package
	syms *symbols

	frame  frame
	frames map[*ir.Object]int64 // the offset of each frame object

	// gcbits holds the pointer mask symbol of each stack object type this
	// function named, by symbol name. It is a lookup table and is never
	// ranged over (specs/053-determinism.md).
	gcbits map[string]obj.SymRef

	// items, fr and slotOff are specs/027-liveness-and-stackmaps.md's frame,
	// which this package reads and does not compute. slotOff is indexed as
	// ssa.Alloc.Slots is and holds -1 for a slot the layout did not place.
	items   []ssa.FrameItem
	fr      *ssa.Frame
	slotOff []int64

	text    []uint32
	relocs  []obj.Reloc
	blockPC []int32
	fixups  []fixup
	growPC  int32

	// pcStart and pcEnd are the half-open range of program counters each
	// value's instructions occupy, indexed by value identifier, with -1 for a
	// value that produced none. The stack maps are indexed by program
	// counter and this is the only record of where a value landed.
	pcStart, pcEnd []int64
	prologueEnd    int64
	unsafe         []ssa.PCRange

	spDelta int64
	pcsp    []obj.PCEntry
	pcfile  []obj.PCEntry
	pcline  []obj.PCEntry
	line    int32

	// args lists the incoming argument values in the order the entry block
	// holds them, and argPlace says where each of them arrives. One parameter
	// is several of these when the convention gives it several registers.
	args     []*ssa.Value
	argPlace []place

	// byID finds a value by its identifier, which an edge copy names.
	byID []*ssa.Value

	done map[ssa.ID]bool // values already emitted out of order
	errs []error

	out [8]uint32
}

func (e *emitter) fail(format string, args ...any) {
	if len(e.errs) < 8 {
		e.errs = append(e.errs, fmt.Errorf("ssagen: %s: %s", e.opt.Sym, fmt.Sprintf(format, args...)))
	}
}

func (e *emitter) err() error { return errors.Join(e.errs...) }

// pc returns the offset of the next instruction.
func (e *emitter) pc() int32 { return int32(len(e.text)) * 4 }

// word appends one instruction.
func (e *emitter) word(w uint32) { e.text = append(e.text, w) }

// wordIf appends an instruction the encoder accepted, and reports the one it
// did not.
//
// An encoder that says no is a compiler bug and not a program that cannot be
// compiled: specs/041 makes the rules responsible for choosing a form that
// fits, and this is the check that they did.
func (e *emitter) wordIf(w uint32, ok bool, format string, args ...any) {
	if !ok {
		e.fail("%s does not encode", fmt.Sprintf(format, args...))
		return
	}
	e.word(w)
}

// mem appends a load or store with an offset the caller knows fits.
func (e *emitter) mem(op arm64.MemOp, rt, base arm64.Reg, off int64) {
	if !e.memClass(op, rt) {
		return
	}
	w, ok := arm64.MemUnsignedOffset(op, rt, base, off)
	e.wordIf(w, ok, "%v %v, %d(%v)", op, rt, off, base)
}

// memIf appends a load or store built by one of obj/arm64's addressing forms.
func (e *emitter) memIf(form func(arm64.MemOp, arm64.Reg, arm64.Reg, int64) (uint32, bool),
	op arm64.MemOp, rt, base arm64.Reg, off int64) {
	if !e.memClass(op, rt) {
		return
	}
	w, ok := form(op, rt, base, off)
	e.wordIf(w, ok, "%v %v, %d(%v)", op, rt, off, base)
}

// memClass checks that the transferred register is in the file the operation
// transfers.
//
// obj/arm64 panics on the mismatch rather than returning false, because a
// register of the wrong file is not an encoding that does not fit: it is a
// caller that lost track of which class a value is in. This package is the
// caller, so it makes the report itself, for the reason reg returns two
// values: a panic out of the encoder names an instruction and not the value
// or the pass that chose it.
func (e *emitter) memClass(op arm64.MemOp, rt arm64.Reg) bool {
	if op.IsFloat() == rt.IsFloat() {
		return true
	}
	e.fail("%v transfers %v, which is in the other register file", op, rt)
	return false
}

// markSP records the stack pointer delta in effect from the next instruction.
func (e *emitter) markSP() {
	e.pcsp = appendPC(e.pcsp, e.pc(), int32(e.spDelta))
}

// markLine records the source position of the next instruction.
func (e *emitter) markLine(pos syntax.Pos) {
	line := e.opt.Line
	if e.opt.Fset != nil && pos.IsKnown() {
		if p := e.opt.Fset.Position(pos); p.IsKnown() {
			line = int32(p.Line)
		}
	}
	if line == e.line {
		return
	}
	e.line = line
	e.pcline = appendPC(e.pcline, e.pc(), line)
}

// reg converts an allocator register number to obj/arm64's.
//
// ssa.Target numbers both register files exactly as obj/arm64 does, which is
// what makes this a conversion rather than a table: ssa.Arm64Reg is the
// identity and ssa.Arm64FReg counts from arm64.F0.
//
// It reports whether the conversion happened, and every caller has to ask.
// The earlier signature returned arm64.ZR for a register it had just refused,
// and ZR is a real register: the caller carried it into an encoder, and an
// encoder that wanted the floating-point file panicked on it rather than
// reporting. A diagnostic the emitter records and then walks past is not a
// diagnostic.
//
// A value the allocation gave no register at all is not that case. A store
// and a nil check produce nothing, and their memory argument occupies no
// register either, so ssa.NoReg is the encoding's own zero register and not a
// failure. value refuses separately when a value that has a width lands
// there.
func (e *emitter) reg(r ssa.Reg) (arm64.Reg, bool) {
	if r == ssa.NoReg {
		return arm64.ZR, true
	}
	x := arm64.Reg(r)
	if !x.Valid() {
		e.fail("register %d is not a register of this target", r)
		return arm64.ZR, false
	}
	return x, true
}

// run emits the whole function.
func (e *emitter) run() {
	e.done = make(map[ssa.ID]bool)
	e.frames = make(map[*ir.Object]int64)
	e.byID = make([]*ssa.Value, e.f.NumValues())
	for _, b := range e.f.Blocks {
		for _, v := range b.Values {
			e.byID[v.ID] = v
		}
	}
	e.frame.leaf = true
	e.line = -1

	if err := e.incoming(); err != nil {
		e.fail("%v", err)
		return
	}
	if err := e.layout(); err != nil {
		e.fail("%v", err)
		return
	}

	e.blockPC = make([]int32, e.f.NumBlocks())
	for i := range e.blockPC {
		e.blockPC[i] = -1
	}
	e.pcStart = make([]int64, e.f.NumValues())
	e.pcEnd = make([]int64, e.f.NumValues())
	for i := range e.pcStart {
		e.pcStart[i] = -1
		e.pcEnd[i] = -1
	}
	e.markLine(e.f.Pos)
	e.markSP()
	e.prologue()
	// The frame exists from here. Everything before it is a range no
	// asynchronous preemption may stop in, because the frame it would read is
	// half built (specs/027-liveness-and-stackmaps.md).
	e.prologueEnd = int64(e.pc())
	e.entryArgs()

	for i, b := range e.f.Blocks {
		e.blockPC[b.ID] = e.pc()
		// Every block other than the growth tail runs with the frame
		// established. An epilogue in an earlier block left the delta at
		// zero, and a pc-value table that says so here would make the
		// runtime unwind through a frame that is there.
		if e.spDelta != e.frame.size {
			e.spDelta = e.frame.size
			e.markSP()
		}
		e.edgeIn(b)
		for _, v := range b.Values {
			if e.skip(b, v) {
				continue
			}
			at := int64(e.pc())
			e.value(v)
			e.mark(v, at)
		}
		var next *ssa.Block
		if i+1 < len(e.f.Blocks) {
			next = e.f.Blocks[i+1]
		}
		e.terminator(b, next)
	}
	e.padExit()
	e.growstack()
	e.patch()
}

// padExit appends one instruction after a last block that ends in a call which
// does not return.
//
// The return address of a call to runtime.panicdivide is never executed, and
// it is still read: the traceback resolves the frame of the function that
// panicked from it, and every pc-value table is a map from a program counter
// to a range. When the call is the last instruction of the body, its return
// address is the first instruction of the stack-growth tail, and the tail
// declares a stack pointer delta of zero because it runs before the frame is
// pushed. The unwinder therefore reads the frame of a function that has one as
// though it had none, computes the caller's stack pointer a frame size too
// low, and reports
//
//	runtime: g 1: unexpected return pc for <fn> called from <garbage>
//
// instead of the panic. Every frame above it is wrong as well, so a panic
// anywhere under such a function has no usable traceback.
//
// One word of padding puts the return address back inside the block it belongs
// to. gc does the same thing for the same reason, in
// cmd/compile/internal/ssagen/ssa.go: "We need the return address of a panic
// call to still be inside the function in question."
func (e *emitter) padExit() {
	n := len(e.f.Blocks)
	if n == 0 || e.f.Blocks[n-1].Kind != ssa.BlockExit {
		return
	}
	e.word(arm64.Nop())
}

// mark records where a value's instructions landed.
//
// The stack maps are indexed by program counter and this is the only record
// of the mapping: a safepoint is a value here and a program counter there.
// A value that produced nothing keeps the -1 it started with, so an empty
// range is not confused with a range at offset zero.
func (e *emitter) mark(v *ssa.Value, at int64) {
	if int(v.ID) >= len(e.pcStart) || int64(e.pc()) == at {
		return
	}
	e.pcStart[v.ID] = at
	e.pcEnd[v.ID] = int64(e.pc())
}

// skip reports whether a value produces no instruction here.
//
// A rematerialised value is recomputed where it is read. A phi is realised by
// the copies on the edges. The two base pointers are
// operands that the address forms ignore: an address of a global is an ADRP
// and ADD pair that the linker patches, and an address in the frame is an ADD
// from the stack pointer, so neither reads the register the allocator gave
// its base. Memory values are ordering edges and occupy nothing.
//
// The control value of a block is emitted with the block's branch, because
// its displacement is not known until the successor's offset is.
func (e *emitter) skip(b *ssa.Block, v *ssa.Value) bool {
	if e.done[v.ID] {
		return true
	}
	if int(v.ID) < len(e.a.Remat) && e.a.Remat[v.ID] {
		// A rematerialised value is recomputed at each use and never held, so
		// its definition produces nothing (specs/026-register-allocation.md).
		return true
	}
	switch v.Op {
	case ssa.OpPhi, ssa.OpInitMem, ssa.OpVarDef, ssa.OpVarKill, ssa.OpSP, ssa.OpSB:
		return true
	case ssa.OpARM64BRcond, ssa.OpARM64CBZ, ssa.OpARM64CBNZ:
		return v == b.Control
	}
	return false
}

// value emits one value.
func (e *emitter) value(v *ssa.Value) {
	e.markLine(v.Pos)
	switch v.Op {
	case ssa.OpArg:
		// Reached only when the arguments travel through the frame, which
		// entryArgs explains. Otherwise they are placed at the entry and
		// marked done.
		e.loadArg(v)
		return
	case ssa.OpSelectN:
		e.fail("v%d: a call result that follows no call", v.ID)
		return
	case ssa.OpCopy:
		e.move(e.home(v), e.home(v.Args[0]), v.Args[0])
		return
	case ssa.OpARM64RET:
		e.ret(v)
		return
	case ssa.OpARM64ADDframe:
		// The offset is the frame layout's, which this package assigns
		// because no pass above it does. The encoder reads AuxInt, which is
		// still zero, so the instruction is built here.
		//
		// addSP and not arm64.AddRegImm, because a frame object further from
		// the stack pointer than the twelve-bit immediate reaches needs the
		// expansion through R27 that subSP already had. rtsym.init is such a
		// frame.
		off, ok := e.frameOff(v)
		if !ok {
			return
		}
		dst, dok := e.reg(e.a.Result[v.ID])
		if !dok {
			return
		}
		e.addSP(dst, off)
		e.spill(v)
		return
	}
	if !ssa.IsARM64Op(v.Op) {
		e.fail("v%d: %v is not a machine operation", v.ID, v.Op)
		return
	}
	if ssa.ARM64MissingEncoder(v.Op) {
		e.fail("v%d: %v has no encoder in obj/arm64", v.ID, v.Op)
		return
	}
	if v.Op.IsCall() {
		e.callValue(v)
		return
	}
	var target obj.SymRef
	if v.Op == ssa.OpARM64MOVDaddr {
		// The symbol is resolved before the instruction is built, because the
		// encoder for an address takes a destination register that a value
		// with no home does not have.
		ref, ok := e.addrTarget(v)
		if !ok {
			return
		}
		target = ref
	}
	if e.a.Result[v.ID] == ssa.NoReg && !v.Op.MakesMemory() &&
		v.Op != ssa.OpARM64LoweredNilCheck && v.Type != nil && v.Type.Size > 0 {
		// The value has a width and the allocation gave it nowhere to go.
		// Encoding it would write the zero register, which reads back as
		// zero and would be a value that is silently always zero.
		e.fail("v%d: %v produces a value and the allocation gives it no register", v.ID, v.Op)
		return
	}
	regs, rok := e.operands(v)
	if !rok {
		return
	}
	dst, dok := e.reg(e.a.Result[v.ID])
	if !dok {
		return
	}
	at := e.pc()
	n, ok := ssa.ARM64Encode(v, dst, regs, e.out[:])
	if !ok || n == 0 {
		e.fail("v%d: the encoder rejected %s", v.ID, v.LongString())
		return
	}
	for i := 0; i < n; i++ {
		e.word(e.out[i])
	}
	if v.Op == ssa.OpARM64MOVDaddr {
		// The pair is one edit and the linker splits the address across it.
		e.addr(at, target, v.AuxInt)
	}
	e.spill(v)
}

// operands materialises the arguments of a value into the registers the
// allocation says it reads them from.
//
// A value whose home is a register is already there. A spilled one is loaded
// from its slot, and a rematerialised one is recomputed: specs/026 says both
// go into a scratch register, and the allocation names which.
func (e *emitter) operands(v *ssa.Value) ([]arm64.Reg, bool) {
	regs := e.a.Args[v.ID]
	out := make([]arm64.Reg, len(v.Args))
	for i, arg := range v.Args {
		if i >= len(regs) || regs[i] == ssa.NoReg {
			out[i] = arm64.ZR
			continue
		}
		want, ok := e.reg(regs[i])
		if !ok {
			return nil, false
		}
		out[i] = want
		src := e.home(arg)
		if src.Kind == ssa.LocReg && arm64.Reg(src.Reg) == want {
			continue
		}
		e.move(ssa.RegLoc(ssa.Reg(want)), src, arg)
	}
	return out, true
}

// home returns where a value lives, or no location when it is rematerialised
// or occupies nothing.
func (e *emitter) home(v *ssa.Value) ssa.Loc {
	if v == nil || int(v.ID) >= len(e.a.Home) {
		return ssa.Loc{}
	}
	return e.a.Home[v.ID]
}

// spill stores a value whose home is a slot.
//
// The allocation writes such a value into a scratch register and this is the
// store that follows. Nothing here decides where the value lives; that is
// specs/026's answer and this is the instruction that realises it.
func (e *emitter) spill(v *ssa.Value) {
	h := e.home(v)
	if h.Kind != ssa.LocSlot {
		return
	}
	src, ok := e.reg(e.a.Result[v.ID])
	if !ok {
		return
	}
	e.store(src, h.Slot, v.Type)
}

// move copies a value from one location to another.
func (e *emitter) move(dst, src ssa.Loc, v *ssa.Value) {
	switch {
	case dst == src:
		return
	case dst.Kind == ssa.LocReg && src.Kind == ssa.LocReg:
		d, dok := e.reg(dst.Reg)
		s, sok := e.reg(src.Reg)
		if !dok || !sok {
			return
		}
		e.copyReg(d, s)
	case dst.Kind == ssa.LocReg && src.Kind == ssa.LocSlot:
		d, ok := e.reg(dst.Reg)
		if !ok {
			return
		}
		e.load(d, src.Slot, v.Type)
	case dst.Kind == ssa.LocSlot && src.Kind == ssa.LocReg:
		s, ok := e.reg(src.Reg)
		if !ok {
			return
		}
		e.store(s, dst.Slot, v.Type)
	case dst.Kind == ssa.LocReg && src.Kind == ssa.LocNone:
		d, ok := e.reg(dst.Reg)
		if !ok {
			return
		}
		e.remat(d, v)
	case dst.Kind == ssa.LocSlot && src.Kind == ssa.LocNone:
		// A rematerialised value whose home is a slot. specs/026 keeps a
		// constant, a frame address and a symbol address out of the frame by
		// recomputing them at each use, but the allocator still gives one a
		// slot when it is live across a call: the call clobbers every
		// register, so the value has to be somewhere, and recomputing it is
		// what the definition does rather than where it lives.
		//
		// There is no instruction that writes a computed value to memory, so
		// this is the recomputation followed by the store, staged through the
		// same reserved register argValue uses. That register holds no value
		// of the function, so nothing is destroyed between the two.
		//
		// Found by compiling `for _, x := range xs { n = add(n, x) }`, where
		// the slice's base address is rematerialisable and lives across the
		// call in the loop body.
		st := scratchFor(v.Type)
		e.remat(st, v)
		e.store(st, dst.Slot, v.Type)
	case dst.Kind == ssa.LocSlot && src.Kind == ssa.LocSlot:
		// One frame slot to another. arm64 has no memory-to-memory move, so
		// this is the load and the store that the pair of one-ended moves
		// above would each make on its own, staged through the same reserved
		// register the rematerialising arm uses.
		//
		// The register is safe here for the reason it is safe there. It is
		// outside the allocatable set, so it is no value's home, and it is
		// held only between these two instructions, which are adjacent.
		// specs/026-register-allocation.md's phi resolution breaks a cycle of
		// copies with Scratch[class][0] and this is Scratch[class][1], so a
		// slot-to-slot copy in the middle of a broken cycle does not destroy
		// the value the cycle is carrying.
		//
		// The type picks both the register file and the width. A float64 that
		// travels through an integer register would be an FMOV the encoder
		// refuses, and a byte moved with a 64-bit load would read seven bytes
		// of a neighbouring slot.
		//
		// The allocator emits this whenever a phi and one of its operands both
		// live in the frame, which is two values live across a call in a loop.
		// Before this arm nanogo refused
		//
		//	for _, v := range s { a = a + f(v); b = b + f(a) }
		//
		// with "no move from s3 to s2".
		st := scratchFor(v.Type)
		e.load(st, src.Slot, v.Type)
		e.store(st, dst.Slot, v.Type)
	default:
		e.fail("v%d: no move from %v to %v", v.ID, src, dst)
	}
}

// remat recomputes a value into a register.
//
// specs/026-register-allocation.md keeps a constant, a frame address and a
// symbol address out of the frame by recomputing them at each use. Each is a
// sequence that depends on nothing but the value itself, so the bits are the
// ones the definition would have produced. The length is not fixed: a constant
// takes up to four instructions and a frame address past the immediate takes
// two.
func (e *emitter) remat(dst arm64.Reg, v *ssa.Value) {
	if v.Op == ssa.OpARM64ADDframe {
		off, ok := e.frameOff(v)
		if !ok {
			return
		}
		e.addSP(dst, off)
		return
	}
	var target obj.SymRef
	if v.Op == ssa.OpARM64MOVDaddr {
		ref, ok := e.addrTarget(v)
		if !ok {
			return
		}
		target = ref
	}
	at := e.pc()
	n, ok := ssa.ARM64Encode(v, dst, nil, e.out[:])
	if !ok || n == 0 {
		e.fail("v%d: the encoder rejected the rematerialisation of %s", v.ID, v.LongString())
		return
	}
	for i := 0; i < n; i++ {
		e.word(e.out[i])
	}
	if v.Op == ssa.OpARM64MOVDaddr {
		e.addr(at, target, v.AuxInt)
	}
}

// frameOff returns the offset of the frame object a value addresses.
func (e *emitter) frameOff(v *ssa.Value) (int64, bool) {
	o, ok := v.Aux.(*ir.Object)
	if !ok || o == nil {
		e.fail("v%d: a frame address of no object", v.ID)
		return 0, false
	}
	off, ok := e.frames[o]
	if !ok {
		e.fail("v%d: %s has no frame slot", v.ID, o.Name)
		return 0, false
	}
	return off + v.AuxInt, true
}

// load reads a spill slot into a register.
func (e *emitter) load(dst arm64.Reg, slot int32, t *ir.Type) {
	op, ok := memOpFor(t, false)
	if !ok {
		e.fail("no load for type %v", t)
		return
	}
	e.mem(op, dst, arm64.RSP, e.slotOffset(slot))
}

// store writes a register into a spill slot.
func (e *emitter) store(src arm64.Reg, slot int32, t *ir.Type) {
	op, ok := memOpFor(t, true)
	if !ok {
		e.fail("no store for type %v", t)
		return
	}
	e.mem(op, src, arm64.RSP, e.slotOffset(slot))
}

func (e *emitter) slotOffset(slot int32) int64 {
	if slot < 0 || int(slot) >= len(e.slotOff) || e.slotOff[slot] < 0 {
		e.fail("slot %d is not in the frame", slot)
		return 0
	}
	return e.slotOff[slot]
}

// ---------------------------------------------------------------------------
// The calling convention, which specs/030-abi.md assigns and no pass performs

// incoming reads the parameter placement off the function.
//
// specs/030-abi.md's assignment pass computed it and the allocator has already
// pre-coloured the arguments from the same answer, so nothing is decided here.
// What is done here is the join: each OpArg value of the entry block is one
// word of one parameter, and this finds which.
func (e *emitter) incoming() error {
	abi := e.f.ABI
	if abi == nil {
		return errors.New("the function has no ABI assignment")
	}
	e.frame.in, e.frame.args = wordPlaces(abi.In), abi.ArgsSize
	for _, v := range e.f.Entry.Values {
		if v.Op != ssa.OpArg {
			continue
		}
		p, ok := e.placeOfArg(v)
		if !ok {
			return fmt.Errorf("v%d: %v is not a word of any parameter", v.ID, v.LongString())
		}
		e.args = append(e.args, v)
		e.argPlace = append(e.argPlace, p)
	}
	return nil
}

// placeOfArg returns where one OpArg value arrives.
//
// The value names its parameter in Aux and its byte offset within that
// parameter in AuxInt, which is the pair specs/025-lowering-and-rules.md
// leaves on every part of a decomposed argument.
func (e *emitter) placeOfArg(v *ssa.Value) (place, bool) {
	o, _ := v.Aux.(*ir.Object)
	av, ok := e.f.ABI.ArgOf(o)
	if !ok || v.Type == nil {
		return place{}, false
	}
	for j := range av.Parts {
		p := &av.Parts[j]
		if p.Off == v.AuxInt && p.Type != nil && p.Type.Size == v.Type.Size {
			return partPlace(av, p), true
		}
	}
	if v.Type.Size == 0 {
		// A parameter of no width occupies no word and no register. It is
		// still a value, and it still has to be accounted for.
		return place{off: av.Off, typ: v.Type}, true
	}
	return place{}, false
}

// entryArgs places the incoming arguments where the allocation wants them.
//
// It is one parallel move: all of them at once, because the home of the first
// argument can be the register the second arrives in, and a cycle is broken
// with the scratch register.
//
// Only the arguments that arrived in registers are moved here, and that is the
// whole of the reason the move is one instant. A register argument has to be
// rescued before anything overwrites the register the caller left it in, so
// every one of them moves at once. An argument that arrived in the argument
// area is in the caller's frame, where nothing this function does can reach
// it, so it is read where it is defined instead: loadArg.
//
// The distinction is not a refinement, it is what makes the move a permutation.
// The allocator may give a stack argument the register a register argument
// vacated, because their live ranges do not meet; moving both at the entry
// would put two moves into one destination, and the earlier argument would be
// destroyed before its last use. Before specs/030-abi.md's assignment pass
// existed, nothing was pre-coloured, two register arguments could share a home
// as well, and the answer was to send every argument through the argument
// area. Pre-colouring removed that half: two register arguments are fixed to
// two registers.
func (e *emitter) entryArgs() {
	e.spillPointerArgs()
	x := make([]xfer, 0, len(e.args))
	seen := make(map[ssa.Loc]bool, len(e.args))
	for i, v := range e.args {
		if !e.argPlace[i].inReg {
			continue
		}
		e.done[v.ID] = true
		dst := e.home(v)
		if dst.Kind == ssa.LocNone {
			continue // the argument is never read
		}
		if seen[dst] {
			// Two register arguments in one home is not a parallel move, and
			// pre-colouring is what makes it impossible. Saying so is cheaper
			// than the code that used to work around it.
			e.fail("v%d and an earlier argument share the home %v", v.ID, e.a.Target.LocString(dst))
			return
		}
		seen[dst] = true
		x = append(x, xfer{dst: dst, src: ssa.RegLoc(ssa.Reg(e.argPlace[i].reg)), v: v})
	}
	e.transfer(x)
	e.markLine(e.f.Entry.Pos)
}

// spillPointerArgs writes every pointer-holding argument that arrived in a
// register into its slot in the argument area.
//
// The arguments bitmap of specs/027-liveness-and-stackmaps.md is exact and it
// is the same at every safepoint: it says which words of the incoming
// argument area hold pointers, and the collector reads that word. An argument
// that travelled in a register leaves the word holding whatever the last
// frame at that address left there, so without this store the collector
// follows a stale value. gc writes the same store, for the narrower case of
// an argument that is live across a call; the condition here is the type
// rather than the liveness, because the bitmap this package emits is not
// narrowed by liveness either.
//
// It runs before anything is placed, because a placement move may overwrite
// the register the argument arrived in.
func (e *emitter) spillPointerArgs() {
	for i := range e.frame.in {
		p := &e.frame.in[i]
		if !p.inReg || p.typ == nil || !p.typ.HasPointers() {
			continue
		}
		op, ok := memOpFor(p.typ, true)
		if !ok {
			e.fail("an argument of type %v has no store", p.typ)
			continue
		}
		e.mem(op, p.reg, arm64.RSP, e.argOff(*p))
	}
}

// argOff returns the offset of one incoming word from the current stack
// pointer.
//
// The area starts eight bytes above the entry stack pointer, because the word
// at the entry stack pointer is where this function saved the link register.
// The prologue has already moved the stack pointer by then, so the frame is
// added back.
func (e *emitter) argOff(p place) int64 {
	return e.frame.size + linkSlot + p.off
}

// loadArg reads one incoming argument out of the argument area into its home.
//
// It runs where the argument is defined rather than at the entry, which is
// what makes two arguments that share a home correct: the second is read after
// the first is dead.
func (e *emitter) loadArg(v *ssa.Value) {
	i := -1
	for k, a := range e.args {
		if a == v {
			i = k
		}
	}
	if i < 0 {
		e.fail("v%d: an argument outside the entry block", v.ID)
		return
	}
	dst := e.home(v)
	if dst.Kind == ssa.LocNone {
		return
	}
	op, ok := memOpFor(v.Type, false)
	if !ok {
		e.fail("v%d: no load for type %v", v.ID, v.Type)
		return
	}
	reg := scratchFor(v.Type)
	if dst.Kind == ssa.LocReg {
		r, ok := e.reg(dst.Reg)
		if !ok {
			return
		}
		reg = r
	}
	e.mem(op, reg, arm64.RSP, e.argOff(e.argPlace[i]))
	if dst.Kind == ssa.LocSlot {
		e.store(reg, dst.Slot, v.Type)
	}
}

// xfer is one value that has to end up somewhere.
//
// src is where it is now, and a src of no kind is a value that is recomputed
// into its destination rather than moved.
type xfer struct {
	dst ssa.Loc
	src ssa.Loc
	v   *ssa.Value
}

// transfer performs a set of moves that happen at one instant.
//
// The order is the whole content of this function. A move that writes a
// register another move still has to read destroys it, so the moves out of
// registers come first, the register permutation is next, and the moves into
// registers from memory come last. The permutation itself can be a cycle,
// which no order of two-operand moves realises, so a cycle is broken with the
// scratch register specs/026-register-allocation.md reserves.
func (e *emitter) transfer(x []xfer) {
	// Out of registers first, so that nothing overwrites a source.
	for _, m := range x {
		if m.src.Kind == ssa.LocReg && m.dst.Kind != ssa.LocReg {
			e.move(m.dst, m.src, m.v)
		}
	}
	// The permutation.
	var dst, src []arm64.Reg
	for _, m := range x {
		if m.src.Kind == ssa.LocReg && m.dst.Kind == ssa.LocReg {
			d, dok := e.reg(m.dst.Reg)
			ss, sok := e.reg(m.src.Reg)
			if !dok || !sok {
				return
			}
			dst = append(dst, d)
			src = append(src, ss)
		}
	}
	e.permute(dst, src)
	// Into registers last.
	for _, m := range x {
		if m.src.Kind == ssa.LocReg {
			continue
		}
		e.move(m.dst, m.src, m.v)
	}
}

// permute realises a set of register-to-register moves that happen at once.
//
// The two register files are permuted separately. A move of an integer cannot
// free a floating-point register and cannot block one, so a mixed set is two
// parallel moves that share no register, and each needs the cycle breaking of
// its own file: the integer scratch register cannot hold a float.
func (e *emitter) permute(dst, src []arm64.Reg) {
	var idst, isrc, fdst, fsrc []arm64.Reg
	for i := range dst {
		switch {
		case dst[i].IsFloat() != src[i].IsFloat():
			e.fail("a parallel move copies %v to %v, which crosses the register files", src[i], dst[i])
			return
		case dst[i].IsFloat():
			fdst, fsrc = append(fdst, dst[i]), append(fsrc, src[i])
		default:
			idst, isrc = append(idst, dst[i]), append(isrc, src[i])
		}
	}
	e.permuteFile(idst, isrc, moveScratch)
	e.permuteFile(fdst, fsrc, fmoveScratch)
}

// permuteFile permutes one register file.
//
// A move whose destination is nobody's remaining source is safe to make now.
// When none is, the moves that are left form a cycle, and one source is copied
// to the scratch register so that the register it occupied can be written.
func (e *emitter) permuteFile(dst, src []arm64.Reg, scratch arm64.Reg) {
	pending := make([]int, 0, len(dst))
	for i := range dst {
		if dst[i] != src[i] {
			pending = append(pending, i)
		}
	}
	for len(pending) > 0 {
		progress := false
		for k := 0; k < len(pending); {
			i := pending[k]
			blocked := false
			for _, j := range pending {
				if j != i && src[j] == dst[i] {
					blocked = true
					break
				}
			}
			if blocked {
				k++
				continue
			}
			e.copyReg(dst[i], src[i])
			pending = append(pending[:k], pending[k+1:]...)
			progress = true
		}
		if progress || len(pending) == 0 {
			continue
		}
		// Every remaining move is blocked, so they form a cycle. Freeing one
		// source breaks it.
		i := -1
		for _, j := range pending {
			if src[j] != scratch {
				i = j
				break
			}
		}
		if i < 0 {
			// The scratch register is already the source of every move that
			// is left, so nothing can be freed. That is a set of moves with
			// two destinations the same, which is not a parallel move.
			e.fail("a parallel move of %d registers has no order", len(pending))
			return
		}
		e.copyReg(scratch, src[i])
		for _, j := range pending {
			if src[j] == src[i] {
				src[j] = scratch
			}
		}
	}
}

// moveScratch breaks a cycle in a parallel move.
//
// It is one of the two registers specs/030-abi.md reserves for the linker's
// trampolines and specs/026-register-allocation.md reserves for
// materialisation, so no value of the function is in it. The other one is the
// allocator's, which is why this is the second and not the first.
const moveScratch = arm64.RegTrampHi

// fmoveScratch is moveScratch for the floating-point file.
//
// ssa.Target gives ClassFloat the pair F30 and F31, for the reason it gives
// ClassInt R16 and R17, and this takes the second of the pair so that the
// allocator keeps the first. A move of a float therefore never touches an
// integer register and never disturbs the integer cycle breaking, which is
// what makes a mixed parallel move two independent ones.
const fmoveScratch = arm64.RegFScratchHi

// scratchFor returns the materialisation register of the file a value of type
// t lives in.
func scratchFor(t *ir.Type) arm64.Reg {
	if c, _ := ssa.ClassOfType(t); c == ssa.ClassFloat {
		return fmoveScratch
	}
	return moveScratch
}

// copyReg copies one register to another of the same file.
//
// The file decides the instruction and the type does not, because a register
// is a register: an integer copy is ORR with the zero register and a
// floating-point one is FMOV. FMOV in the double form is used for a float32
// too, because every single-precision operation zeroes bits 63 to 32, so the
// wider copy moves the same bits and needs no type to choose.
//
// A copy between the two files is refused rather than encoded as FMOV general
// to floating-point. Such a copy is a value whose class the allocation and the
// convention disagree about, and a bitcast instruction here would turn that
// disagreement into a program that computes an integer's bits as a float.
func (e *emitter) copyReg(dst, src arm64.Reg) {
	if dst == src {
		return
	}
	if dst.IsFloat() != src.IsFloat() {
		e.fail("a copy from %v to %v crosses the register files", src, dst)
		return
	}
	if dst.IsFloat() {
		e.word(arm64.FmovRegReg(arm64.Size64, dst, src))
		return
	}
	e.word(arm64.MovRegReg(arm64.Size64, dst, src))
}

// callValue emits a call: the arguments into their registers, the call
// itself, and the results out of theirs.
func (e *emitter) callValue(v *ssa.Value) {
	places, lo, _, err := e.callArgs(v)
	if err != nil {
		e.fail("v%d: %v", v.ID, err)
		return
	}
	args := v.Args
	if v.Op.TakesMemory() && len(args) > 0 {
		args = args[:len(args)-1]
	}
	indirect := v.Op == ssa.OpARM64CALLclosure || v.Op == ssa.OpARM64CALLinter
	var entry, ctx *ssa.Value
	if indirect {
		if len(args) < lo || lo < 2 {
			e.fail("v%d: an indirect call with no entry point", v.ID)
			return
		}
		entry, ctx = args[0], args[1]
	}
	args = args[lo:]

	x := make([]xfer, 0, len(args)+2)
	var entryReg arm64.Reg
	if indirect {
		// The entry point is read from the register specs/030-abi.md fixes
		// for it, which the allocation names. It is moved with the arguments
		// and not before them, because the move is one instant: a source of
		// one move may be the destination of another.
		regs := e.a.Args[v.ID]
		if len(regs) == 0 || regs[0] == ssa.NoReg {
			e.fail("v%d: the allocation gives the entry point of an indirect call no register", v.ID)
			return
		}
		r, ok := e.reg(regs[0])
		if !ok {
			return
		}
		entryReg = r
		x = append(x, xfer{dst: ssa.RegLoc(regs[0]), src: e.home(entry), v: entry})
	}
	if v.Op == ssa.OpARM64CALLclosure {
		// The closure travels in the closure register, not in the argument
		// sequence: the callee reads its captured variables through it
		// (specs/030-abi.md).
		x = append(x, xfer{dst: ssa.RegLoc(ssa.Reg(arm64.RegClosure)), src: e.home(ctx), v: ctx})
	}
	for i, a := range args {
		if i >= len(places) {
			break
		}
		p := places[i]
		if !p.inReg {
			// The outgoing area is at the bottom of the frame, above
			// the word the callee saves its link register in, which is
			// where the callee reads its arguments from.
			e.storeArg(a, linkSlot+p.off)
			continue
		}
		x = append(x, xfer{dst: ssa.RegLoc(ssa.Reg(p.reg)), src: e.home(a), v: a})
	}
	e.transfer(x)

	at := e.pc()
	if indirect {
		e.word(arm64.Blr(entryReg))
	} else {
		c, err := callTarget(v.Aux)
		if err != nil {
			e.fail("v%d: %v", v.ID, err)
			return
		}
		e.call(at, c)
		w, ok := arm64.Bl(0)
		e.wordIf(w, ok, "BL %s", c.name)
	}
	e.results(v)
}

// resultPlaces places a list of result values.
//
// specs/030-abi.md walks the results with the register counters restarted, and
// the allocator read the same walk when it pre-coloured a call's results and a
// return's values.
//
// base is where the arguments of the same boundary left the stack part of the
// area. A result the registers cannot hold sits after them, so a walk that
// started at zero would read it out of a word an argument occupies.
func (e *emitter) resultPlaces(base int64, types []*ir.Type) ([]place, error) {
	vals, _, err := ssa.ABIResults(e.a.Target, base, types)
	if err != nil {
		return nil, err
	}
	return valuePlaces(vals)
}

// callResultBase returns where one call's arguments left the stack part of the
// outgoing area.
func (e *emitter) callResultBase(v *ssa.Value) (int64, error) {
	vals, _, _, err := ssa.ABICallArgs(e.a.Target, v)
	if err != nil {
		return 0, err
	}
	return ssa.ABIStackEnd(vals), nil
}

// storeArg writes one outgoing argument into the frame.
func (e *emitter) storeArg(a *ssa.Value, off int64) {
	op, ok := memOpFor(a.Type, true)
	if !ok {
		e.fail("v%d: no store for type %v", a.ID, a.Type)
		return
	}
	h := e.home(a)
	src := scratchFor(a.Type)
	switch h.Kind {
	case ssa.LocReg:
		r, rok := e.reg(h.Reg)
		if !rok {
			return
		}
		src = r
	case ssa.LocSlot:
		e.load(src, h.Slot, a.Type)
	default:
		e.remat(src, a)
	}
	e.mem(op, src, arm64.RSP, off)
}

// results moves the values a call returns out of the result registers.
//
// The results are named by the OpSelectN values that follow the call, and they
// are moved at once for the same reason the arguments are: the home of the
// first result can be the register the second arrives in.
func (e *emitter) results(call *ssa.Value) {
	var sel []*ssa.Value
	var types []*ir.Type
	for _, v := range call.Block.Values {
		if v.Op != ssa.OpSelectN || len(v.Args) == 0 || v.Args[0] != call {
			continue
		}
		for int(v.AuxInt) >= len(sel) {
			sel = append(sel, nil)
			types = append(types, nil)
		}
		sel[v.AuxInt] = v
		types[v.AuxInt] = v.Type
	}
	if len(sel) == 0 {
		return
	}
	for i, t := range types {
		if t == nil {
			e.fail("v%d: result %d of the call is never named", call.ID, i)
			return
		}
	}
	base, err := e.callResultBase(call)
	if err != nil {
		e.fail("v%d: %v", call.ID, err)
		return
	}
	places, err := e.resultPlaces(base, types)
	if err != nil {
		e.fail("v%d: %v", call.ID, err)
		return
	}
	x := make([]xfer, 0, len(sel))
	for i, v := range sel {
		e.done[v.ID] = true
		p := places[i]
		if !p.inReg {
			// The result arrived in the outgoing argument area and not in a
			// register, so there is no register to move it out of. The abi
			// pass turned the call site's read of it into a block move out of
			// that area (ssa.readFromArea) and left this value with no
			// reader; a call whose result it could not rewrite that way is
			// refused there, so reaching here means the move is already in
			// the graph.
			continue
		}
		dst := e.home(v)
		if dst.Kind == ssa.LocNone {
			continue
		}
		x = append(x, xfer{dst: dst, src: ssa.RegLoc(ssa.Reg(p.reg)), v: v})
	}
	e.transfer(x)
}

// ret places the result values, tears the frame down and returns.
func (e *emitter) ret(v *ssa.Value) {
	args := v.Args
	if v.Op.TakesMemory() && len(args) > 0 {
		args = args[:len(args)-1]
	}
	types := make([]*ir.Type, 0, len(args))
	for _, a := range args {
		types = append(types, a.Type)
	}
	// A return continues from where this function's own arguments left the
	// stack part of its incoming area.
	places, err := e.resultPlaces(ssa.ABIStackEnd(e.f.ABI.In), types)
	if err != nil {
		e.fail("v%d: %v", v.ID, err)
		return
	}
	x := make([]xfer, 0, len(args))
	for i, a := range args {
		p := places[i]
		if !p.inReg {
			e.fail("v%d: a result that travels in the frame is not written", v.ID)
			continue
		}
		x = append(x, xfer{dst: ssa.RegLoc(ssa.Reg(p.reg)), src: e.home(a), v: a})
	}
	e.transfer(x)
	e.epilogue()
}

// ---------------------------------------------------------------------------
// Control flow

// edgeIn emits the copies an edge carries at the start of the successor.
func (e *emitter) edgeIn(b *ssa.Block) {
	for i := range e.a.Edges {
		ed := &e.a.Edges[i]
		if ed.Succ != b.ID || ed.AtPredEnd {
			continue
		}
		e.copies(ed.Copies)
	}
}

// edgeOut emits the copies an edge carries at the end of the predecessor.
func (e *emitter) edgeOut(b *ssa.Block) {
	for i := range e.a.Edges {
		ed := &e.a.Edges[i]
		if ed.Pred != b.ID || !ed.AtPredEnd {
			continue
		}
		e.copies(ed.Copies)
	}
}

// copies emits one edge's moves, in the order the allocation gives them.
//
// The order is already a correct sequence: specs/026-register-allocation.md
// resolves the phis and its verifier replays the sequence against a model of
// the machine, so a sequence that overwrote a source before another copy read
// it would have failed there.
func (e *emitter) copies(cs []ssa.Copy) {
	for _, c := range cs {
		v := e.valueOf(c.Value)
		if v == nil {
			e.fail("a copy names v%d, which is not in the function", c.Value)
			continue
		}
		e.move(c.Dst, c.Src, v)
	}
}

// terminator emits the branch a block ends with.
//
// A branch to the block that follows in the layout is not emitted, which is
// the only instruction selection this file does and the reason the block order
// is the one ssa.Func gives rather than one chosen here.
func (e *emitter) terminator(b *ssa.Block, next *ssa.Block) {
	switch b.Kind {
	case ssa.BlockPlain:
		e.edgeOut(b)
		if len(b.Succs) != 1 {
			e.fail("b%d is plain and has %d successors", b.ID, len(b.Succs))
			return
		}
		if b.Succs[0] != next {
			e.branchToBlock(fixup{kind: fixB, block: b.Succs[0].ID})
		}
	case ssa.BlockIf:
		if len(b.Succs) != 2 || b.Control == nil {
			e.fail("b%d is a branch with %d successors", b.ID, len(b.Succs))
			return
		}
		e.markLine(b.Control.Pos)
		regs, rok := e.operands(b.Control)
		if !rok {
			return
		}
		f := fixup{block: b.Succs[0].ID}
		switch c := b.Control; c.Op {
		case ssa.OpARM64BRcond:
			cond, ok := c.Aux.(arm64.Cond)
			if !ok {
				e.fail("v%d: a conditional branch with no condition", c.ID)
				return
			}
			f.kind, f.cond = fixCond, cond
		case ssa.OpARM64CBZ, ssa.OpARM64CBNZ:
			f.kind = fixCbz
			if c.Op == ssa.OpARM64CBNZ {
				f.kind = fixCbnz
			}
			if len(regs) == 0 {
				e.fail("v%d: a compare-and-branch with no operand", c.ID)
				return
			}
			f.reg, f.size = regs[0], ssa.ARM64Size(c.Args[0].Type)
		default:
			e.fail("v%d: %v does not end a block", c.ID, c.Op)
			return
		}
		e.branchToBlock(f)
		if b.Succs[1] != next {
			e.branchToBlock(fixup{kind: fixB, block: b.Succs[1].ID})
		}
	case ssa.BlockRet:
		// The return is a value, so it has already been emitted with the
		// epilogue. A block that ends without one is a graph this package
		// cannot emit.
		if b.Control == nil || b.Control.Op != ssa.OpARM64RET {
			e.fail("b%d returns and its control is %v", b.ID, b.Control)
		}
	case ssa.BlockExit:
		// The block ends in a call that does not return, which is already
		// emitted. Nothing follows it.
	default:
		e.fail("b%d has kind %v", b.ID, b.Kind)
	}
}

// branchToBlock emits a branch whose target is resolved once every block's
// offset is known.
func (e *emitter) branchToBlock(f fixup) {
	f.at = e.pc()
	e.fixups = append(e.fixups, f)
	e.word(0)
}

// branchToPC emits a branch to an offset that is already known.
func (e *emitter) branchToPC(pc int32) {
	e.fixups = append(e.fixups, fixup{at: e.pc(), block: -1, pc: pc, kind: fixB})
	e.word(0)
}

// branchToGrowstack emits the conditional branch to the stack-growth tail,
// whose offset is known only once the whole function is laid out.
func (e *emitter) branchToGrowstack(c arm64.Cond) {
	e.fixups = append(e.fixups, fixup{at: e.pc(), block: -1, pc: -1, kind: fixCond, cond: c})
	e.word(0)
}

// patch resolves every branch, which is step 2 of specs/041's algorithm.
//
// It runs once. On this target an instruction is four bytes whatever its
// displacement, so no offset moves when a branch is resolved and no second
// pass is needed. specs/043-amd64-backend.md owns the target where a branch
// that grows moves every later offset and the two steps iterate.
func (e *emitter) patch() {
	for _, f := range e.fixups {
		target := f.pc
		if f.block >= 0 {
			if int(f.block) >= len(e.blockPC) || e.blockPC[f.block] < 0 {
				e.fail("a branch names b%d, which was not laid out", f.block)
				continue
			}
			target = e.blockPC[f.block]
		} else if f.pc < 0 {
			target = e.growPC
		}
		off := int64(target - f.at)
		var w uint32
		var ok bool
		switch f.kind {
		case fixB:
			w, ok = arm64.B(off)
		case fixCond:
			w, ok = arm64.BCond(f.cond, off)
		case fixCbz:
			w, ok = arm64.Cbz(f.size, f.reg, off)
		case fixCbnz:
			w, ok = arm64.Cbnz(f.size, f.reg, off)
		}
		if !ok {
			// The branch is out of range. The linker inserts a trampoline for
			// a call, and specs/041 leaves the compiler's part at emitting the
			// relocation, but a branch inside one function has no relocation
			// and no trampoline: the fix is a longer form, which is a rule
			// this package does not have.
			e.fail("a branch at %d reaches %d, which does not encode", f.at, target)
			continue
		}
		e.text[f.at/4] = w
	}
}

// valueOf returns the value with the given identifier.
func (e *emitter) valueOf(id ssa.ID) *ssa.Value {
	if id < 0 || int(id) >= len(e.byID) {
		return nil
	}
	return e.byID[id]
}

// ---------------------------------------------------------------------------
// The symbol

// appendPC records a pc-value row, replacing the last one when it lands on the
// same offset.
//
// Two rows at one offset describe the same instruction twice, and
// obj.EncodePCData rejects the pair rather than guessing which one wins. The
// later row is the right one: it is the state the instruction at that offset
// runs in.
func appendPC(list []obj.PCEntry, pc int32, value int32) []obj.PCEntry {
	if n := len(list); n > 0 && list[n-1].PC == int64(pc) {
		list[n-1].Value = value
		return list
	}
	return append(list, obj.PCEntry{PC: int64(pc), Value: value})
}

// dwarfInfo returns the DWARF subprogram symbol of one function.
//
// The name is gc's (cmd/internal/dwarf, InfoPrefix) and the contents are
// empty. A compilation unit's function DIEs are the contents of these symbols
// concatenated, so an empty one contributes no DIE and the unit describes the
// functions gc compiled and not this one. That is what nanogo knows today:
// specs/046-debug-info.md puts DWARF at G3, and a DIE invented here would be a
// wrong one rather than a missing one.
func (e *emitter) dwarfInfo() *obj.Symbol {
	return &obj.Symbol{
		Name:  dwarfInfoPrefix + e.opt.Sym,
		Type:  obj.SDWARFFCN,
		Align: 1,
	}
}

// dwarfInfoPrefix is gc's name for a subprogram symbol
// (cmd/internal/dwarf, InfoPrefix).
const dwarfInfoPrefix = "go:info."

// FuncIDWrapper marks compiler-generated code the runtime must not count as a
// frame of the program.
//
// The value is the runtime's, not nanogo's: it is the position of
// abi.FuncIDWrapper in $GOROOT/src/internal/abi/symtab.go's FuncID block, the
// way the FUNCDATA indices of specs/027-liveness-and-stackmaps.md are.
// TestFuncIDWrapperMatchesTheRuntime reads that file and checks it, because a
// wrong value here is a recover that silently stops recovering.
const FuncIDWrapper = 23

// funcInfoBytes encodes the FuncInfo auxiliary symbol.
//
// The layout is cmd/internal/goobj's: the argument size, the frame size
// without the link register slot, the function identifier and flags padded to
// a word, the line the function starts on, the file table, and the inline
// tree. nanogo writes no inline tree yet, so the last count is zero and not
// absent: the reader computes the offset of everything after the file table
// from that count.
func funcInfoBytes(args, locals uint32, funcID byte, startLine int32, files []uint32) []byte {
	b := make([]byte, 0, 24+4*len(files))
	b = binary.LittleEndian.AppendUint32(b, args)
	b = binary.LittleEndian.AppendUint32(b, locals)
	b = append(b, funcID, 0 /* FuncFlag */, 0, 0 /* padding to a word */)
	b = binary.LittleEndian.AppendUint32(b, uint32(startLine))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(files)))
	for _, f := range files {
		b = binary.LittleEndian.AppendUint32(b, f)
	}
	return binary.LittleEndian.AppendUint32(b, 0) // the inline tree
}

// result builds the symbols.
//
// The auxiliary symbols carry a name derived from the function's, and gc
// leaves them unnamed. That difference is not a choice: cmd/link decides
// whether a symbol takes part in the data layout by asking whether it has a
// name (loader.topLevelSym), so a named pc-value table is placed in the
// read-only section, its offset in runtime.pctab is overwritten by that
// placement, and the linker faults while writing the table. obj.checkSym
// rejects an empty name, so the names are here until it does not. The link
// test names the same two functions and clears the names in the written bytes,
// which is the evidence that nothing else stands in the way.
func (e *emitter) result() (*Result, error) {
	size := int64(len(e.text)) * 4
	data := make([]byte, 0, size)
	for _, w := range e.text {
		data = binary.LittleEndian.AppendUint32(data, w)
	}
	text := &obj.Symbol{
		Name: e.opt.Sym,
		ABI:  obj.ABIInternal,
		Type: obj.STEXT,
		Size: uint32(size),
		Data: data,
	}
	if e.frame.leaf {
		text.Flag |= obj.SymFlagLeaf
	}
	if e.syms.reflectMethod {
		// The function reaches a method through reflect, so cmd/link cannot
		// decide which methods are live by following calls. reloc.go's
		// reflectMethodNames says what happens without the mark.
		text.Flag |= obj.SymFlagReflectMethod
	}
	// SymFlagNoSplit is not set even for a function that emits no check.
	// The flag is what makes cmd/link compute the nosplit budget of
	// specs/035-goroutines-and-stack-growth.md over the call graph, and
	// nanogo does not compute that budget yet. Claiming the property without
	// checking it is what specs/035 says must be rejected at compile time.
	text.Relocs = e.relocs

	var files []uint32
	if e.opt.File != "" {
		files = append(files, e.pkg.AddFile(e.opt.File))
	}
	r := &Result{
		Text:   text,
		Gotype: e.opt.Gotype,
		Frame:  e.frame.size,
		Locals: e.frame.locals(),
		Args:   e.frame.args,
	}
	// The collector's tables come before anything else can go wrong with
	// them: a function whose maps cannot be built is not emitted at all.
	t, err := e.buildTables(size)
	if err != nil {
		return nil, fmt.Errorf("ssagen: %s: %w", e.opt.Sym, err)
	}
	r.Funcdata, r.Pcdata, r.maps = t.funcdata, t.pcdata, t.maps
	funcID := byte(0)
	if e.f != nil && e.f.Wrapper {
		funcID = FuncIDWrapper
	}
	info := funcInfoBytes(uint32(r.Args), uint32(r.Locals), funcID, e.opt.Line, files)
	r.FuncInfo = &obj.Symbol{
		// Unnamed, for the reason the pc-value tables are: a named auxiliary
		// symbol takes part in the data layout and the linker then writes over
		// its own table offsets.
		Anonymous: true,
		// A plain definition, not a content-addressable one: cmd/link reads
		// the FuncInfo of a text symbol out of the object at the symbol index
		// the auxiliary entry names, without resolving which index space that
		// is, so it has to be this package's own.
		Type:  obj.SRODATA,
		Size:  uint32(len(info)),
		Align: 1,
		Data:  info,
	}

	r.DwarfInfo = e.dwarfInfo()

	pcfile := e.pcfile
	if len(files) > 0 {
		pcfile = appendPC(nil, 0, int32(files[0]))
	}
	tables := []struct {
		name    string
		entries []obj.PCEntry
		out     **obj.Symbol
	}{
		{".pcsp", e.pcsp, &r.Pcsp},
		{".pcfile", pcfile, &r.Pcfile},
		{".pcline", e.pcline, &r.Pcline},
	}
	for _, t := range tables {
		b, err := obj.EncodePCData(t.entries, size, minLC)
		if err != nil {
			return nil, fmt.Errorf("ssagen: %s%s: %w", e.opt.Sym, t.name, err)
		}
		*t.out = &obj.Symbol{
			// A pc-value table carries no name. cmd/link decides whether a
			// symbol takes part in the data layout by asking whether it has
			// one, so a named table is placed into the read-only section, its
			// offset in runtime.pctab is overwritten by that placement, and
			// the linker faults while writing the table.
			Anonymous: true,
			Type:      obj.SRODATA,
			Size:      uint32(len(b)),
			Align:     1,
			Data:      b,
			// The section class keeps a pc-value table from merging with
			// read-only data that happens to hold the same bytes.
			Pcdata: true,
		}
	}
	return r, nil
}
