// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

// The stack maps of specs/027-liveness-and-stackmaps.md.
//
// liveness.go says which items of the frame are live at each safepoint. This
// file turns that into the bytes the runtime reads: two bitmap symbols, a
// stack object table, and two pc-value streams that select a bitmap per
// program counter.
//
// # The encoding, and where it is proved
//
// A bitmap symbol is runtime.stackmap:
//
//	n     int32                  the number of bitmaps
//	nbit  int32                  the number of bits in each one
//	bits  [n][ceil(nbit/8)]byte  the bitmaps, one after another
//
// There is no padding between bitmaps. spikes/stackmap is where that is
// proved rather than assumed: its multimap is ten bytes for two bitmaps of
// three bits, so the stride is one byte and not four, and the runtime honours
// the second bitmap. specs/000-decisions.md decision 3 cites the spike, and
// stackmap_test.go compares these bytes with the spike's own.
//
// # One index selects both maps
//
// PCDATA_StackMapIndex selects a bitmap in FUNCDATA_LocalsPointerMaps and in
// FUNCDATA_ArgsPointerMaps at once, so the two symbols hold the same number of
// bitmaps and a bitmap is identified by the same index in both. Two safepoints
// are therefore merged into one index only when both of their bitmaps agree.
//
// # The leading region of a pc-value stream
//
// obj.EncodePCData drops an entry whose value is the initial -1, so a table
// that begins with a -1 region encodes as if the next value applied from the
// function entry. For both streams here that is harmless and it is worth
// stating why, because it is not obvious:
//
//   - For PCDATA_StackMapIndex the region before the first call decodes as
//     index 0 instead of -1, and the runtime substitutes 0 for -1 anyway
//     (runtime/stkframe.go, getStackMap). No program counter in that region is
//     ever looked up: a frame is read at a call's return address, or
//     conservatively after an asynchronous preemption.
//   - For PCDATA_UnsafePoint a leading safe region would decode as unsafe,
//     which loses a preemption opportunity and nothing else. A function with a
//     prologue starts unsafe, so the case is rare as well as harmless.
//
// A leading -1 region that had to survive would need a zero value delta, which
// the runtime accepts only as the first pair of a stream. See the report on
// specs/040-object-format.md.
//
// # What the code generator must guarantee
//
// The runtime does not read the frame layout. It computes the block it scans
// as [Varp - nbit*PtrSize, Varp), from the bitmap and from the frame size the
// object file declares. Two obligations follow, and neither is visible in the
// bytes this file writes.
//
// The frame the generator emits must put Varp where LayoutFrame put it. A
// frame whose locals area ends somewhere else is scanned at the wrong address
// with a bitmap that is internally consistent and describes another function's
// idea of the frame.
//
// A function with no call has no safepoint, so its bitmap symbol holds n=0.
// The runtime throws "missing stackmap" when it scans a frame whose locals
// area is larger than the target's minimum frame size and finds n=0. Such a
// frame is only reached conservatively today, because a function that calls
// nothing is on the stack only at an asynchronous preemption, and the
// reference compiler leaves n at zero in the same case. A generator that gives
// a function a safepoint of its own without a call has to give it a bitmap
// as well.

import (
	"fmt"
	"sort"
	"strings"

	"golang.design/x/nanogo/ir"
	// The object writer is imported under a name of its own because a test
	// helper of this package is called obj, and an import is file scoped while
	// a package level declaration is not.
	goobj "golang.design/x/nanogo/obj"
)

// The FUNCDATA and PCDATA indices.
//
// They are the runtime's, not nanogo's: the values come from
// $GOROOT/src/internal/abi/symtab.go and $GOROOT/src/runtime/funcdata.h,
// which state that the two files must agree. A wrong index here produces a
// program that runs until a collection reads the wrong table.
const (
	FUNCDATA_ArgsPointerMaps   = 0
	FUNCDATA_LocalsPointerMaps = 1
	FUNCDATA_StackObjects      = 2

	PCDATA_UnsafePoint   = 0
	PCDATA_StackMapIndex = 1
)

// The values of the PCDATA_UnsafePoint stream.
//
// The stream is not a flag. The safe value is the same -1 that a pc-value
// stream starts at, and unsafe is -2. The restart values, which back a
// preempted program counter up to the start of a sequence, are not emitted
// here: nanogo has no sequence that needs restarting yet.
const (
	UnsafePointSafe   = -1
	UnsafePointUnsafe = -2
)

// BitmapSet is the runtime.stackmap of one FUNCDATA symbol.
type BitmapSet struct {
	NBit int32
	Maps [][]byte
}

// Bytes returns the symbol's data.
func (s *BitmapSet) Bytes() []byte {
	stride := int(s.NBit+7) / 8
	out := make([]byte, 8, 8+len(s.Maps)*stride)
	putInt32(out[0:], int32(len(s.Maps)))
	putInt32(out[4:], s.NBit)
	for _, m := range s.Maps {
		out = append(out, m...)
	}
	return out
}

func putInt32(b []byte, v int32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

// StackObject is one entry of the FUNCDATA_StackObjects table.
//
// Off is measured from Varp and is negative for a local, which is how the
// runtime tells a local from an incoming argument.
type StackObject struct {
	Item     int   // the index in Frame.Items
	Off      int32 // from Varp
	Size     int32
	PtrBytes int32
	Type     *ir.Type
	Name     string
}

// StackMaps is everything specs/027-liveness-and-stackmaps.md hands to the
// collector for one function.
type StackMaps struct {
	Func     *Func
	Frame    *Frame
	Liveness *Liveness

	// Locals and Args hold one bitmap per stack map index, and Index gives the
	// stack map index of each safepoint of Liveness.Safepoints.
	Locals BitmapSet
	Args   BitmapSet
	Index  []int32

	Objects []StackObject

	// GrowIndex is the stack map index of the stack-growth tail. See
	// BuildStackMaps.
	GrowIndex int32
}

// BuildStackMaps produces the maps of one function.
func BuildStackMaps(lv *Liveness, fr *Frame) (*StackMaps, error) {
	if lv == nil || fr == nil {
		return nil, fmt.Errorf("ssa: stackmap: nil liveness or frame")
	}
	if len(lv.Items) != len(fr.Items) {
		return nil, fmt.Errorf("ssa: stackmap: %s: liveness has %d items and the frame has %d",
			lv.Func.Name, len(lv.Items), len(fr.Items))
	}
	ptrSize := fr.Config.ptrSize()
	m := &StackMaps{Func: lv.Func, Frame: fr, Liveness: lv}
	m.Locals.NBit = fr.LocalsBits
	m.Args.NBit = fr.ArgsBits

	// Two arguments bitmaps, and the difference between them is FrameArg.Spill.
	//
	// args is what every safepoint of the body uses. It describes the words
	// the caller wrote and the callee reads, and nothing here narrows their
	// liveness: an argument the caller still holds a copy of is live for as
	// long as the frame is, and claiming otherwise would be the unsafe
	// direction. specs/030-abi.md is where a narrower answer comes from.
	//
	// argsGrow adds the reserved words of the arguments that travel in
	// registers. Only the stack-growth tail writes those, and only the stack
	// map in effect inside the tail may say they hold a pointer: on the
	// ordinary path they hold whatever the caller's frame left at that
	// address, and a bitmap that describes them makes the collector follow it
	// and the stack copier rewrite it.
	args := make([]byte, int(fr.ArgsBits+7)/8)
	argsGrow := make([]byte, len(args))
	for _, a := range fr.Config.Args {
		for w := int64(0); w*ptrSize < a.Type.Size; w++ {
			if !ptrBitSet(a.Type.PtrBits, w) {
				continue
			}
			b, ok := fr.ArgBit(a.Off + w*ptrSize)
			if !ok {
				return nil, fmt.Errorf("ssa: stackmap: %s: argument %s word %d is at %d, outside the %d bit arguments map",
					lv.Func.Name, a.Name, w, a.Off+w*ptrSize, fr.ArgsBits)
			}
			argsGrow[b/8] |= 1 << uint(b%8)
			if !a.Spill {
				args[b/8] |= 1 << uint(b%8)
			}
		}
	}

	// seen maps the bytes of both bitmaps to the index they were given. It is
	// read by key and never ranged over, so specs/053-determinism.md is
	// satisfied: the order of the output is the order the safepoints were
	// visited in.
	//
	// The key covers the arguments map as well as the locals map although the
	// arguments map is the same at every safepoint today. One index selects a
	// bitmap in both symbols, so when specs/030-abi.md makes the arguments map
	// vary, two safepoints with equal locals and different arguments must not
	// collapse into one index.
	seen := make(map[string]int32)
	m.Index = make([]int32, len(lv.Safepoints))
	for k := range lv.Safepoints {
		locals := make([]byte, int(fr.LocalsBits+7)/8)
		for i := range fr.Items {
			if !lv.LiveAt(k, i) {
				continue
			}
			it := &fr.Items[i]
			for w := int64(0); w*ptrSize < it.Size; w++ {
				// Which words hold a pointer is not a liveness question. It is
				// ir.Type.PtrBits and nothing else: a bit set here that
				// PtrBits does not justify is a word specs/035 rewrites when
				// it copies the stack.
				if !ptrBitSet(it.Type.PtrBits, w) {
					continue
				}
				b, ok := fr.LocalBit(it.Off + w*ptrSize)
				if !ok {
					return nil, fmt.Errorf("ssa: stackmap: %s: %s word %d is at %d, outside the pointer area [%d,%d)",
						lv.Func.Name, it.Name, w, it.Off+w*ptrSize, fr.PtrBase, fr.Varp)
				}
				locals[b/8] |= 1 << uint(b%8)
			}
		}
		key := string(locals) + "|" + string(args)
		if idx, ok := seen[key]; ok {
			m.Index[k] = idx
			continue
		}
		idx := int32(len(m.Locals.Maps))
		seen[key] = idx
		m.Locals.Maps = append(m.Locals.Maps, locals)
		// A copy per index. The bitmaps are the same bytes today, and one
		// slice shared by every index would make a caller that edits one edit
		// all of them.
		m.Args.Maps = append(m.Args.Maps, append([]byte(nil), args...))
		m.Index[k] = idx
	}

	// The stack-growth tail's own map. Its arguments bitmap is the full one,
	// because the tail has just saved the argument registers into those words
	// and runtime.morestack may copy the stack from there. Its locals bitmap
	// is empty, because the tail runs before the frame is pushed, so there are
	// no locals at the address the runtime would compute.
	//
	// It is also the map that makes a function with no call describable at
	// all. Such a function has no safepoint and would carry no bitmap, and the
	// stack copier reads the arguments bitmap of every frame it moves, the one
	// that called runtime.morestack included.
	m.GrowIndex = -1
	if fr.Config.Grows {
		m.GrowIndex = int32(len(m.Locals.Maps))
		m.Locals.Maps = append(m.Locals.Maps, make([]byte, int(fr.LocalsBits+7)/8))
		m.Args.Maps = append(m.Args.Maps, argsGrow)
	}

	for _, i := range fr.StackObjects() {
		it := &fr.Items[i]
		off := fr.VarpOff(it.Off)
		if off >= 0 {
			// The runtime reads a non-negative offset as one from Argp, so a
			// local described that way would be scanned in the caller's frame.
			return nil, fmt.Errorf("ssa: stackmap: %s: stack object %s is at %d from Varp, and a local is below it",
				lv.Func.Name, it.Name, off)
		}
		if off != int64(int32(off)) || it.Size != int64(int32(it.Size)) {
			return nil, fmt.Errorf("ssa: stackmap: %s: stack object %s does not fit a 32 bit frame offset", lv.Func.Name, it.Name)
		}
		m.Objects = append(m.Objects, StackObject{
			Item:     i,
			Off:      int32(off),
			Size:     int32(it.Size),
			PtrBytes: int32(it.Type.PtrBytes()),
			Type:     it.Type,
			Name:     it.Name,
		})
	}
	return m, nil
}

// ptrBitSet reports whether word w of a type's pointer map holds a pointer.
func ptrBitSet(bits []byte, w int64) bool {
	if w < 0 || w/8 >= int64(len(bits)) {
		return false
	}
	return bits[w/8]&(1<<uint(w%8)) != 0
}

// LocalsSym returns the FUNCDATA_LocalsPointerMaps symbol.
//
// The kind is read-only data: the collector reads it and nothing writes it.
// The alignment is four, because the two counts are int32 and the runtime
// reads them as such.
func (m *StackMaps) LocalsSym(name string) *goobj.Symbol {
	return bitmapSym(name, m.Locals.Bytes())
}

// ArgsSym returns the FUNCDATA_ArgsPointerMaps symbol.
func (m *StackMaps) ArgsSym(name string) *goobj.Symbol {
	return bitmapSym(name, m.Args.Bytes())
}

func bitmapSym(name string, data []byte) *goobj.Symbol {
	return &goobj.Symbol{
		Name:  name,
		Type:  goobj.SRODATA,
		Align: 4,
		Size:  uint32(len(data)),
		Data:  data,
		Flag:  goobj.SymFlagLocal,
	}
}

// ObjectsSym returns the FUNCDATA_StackObjects symbol, or nil when the
// function has no stack object.
//
// gcdata gives the type descriptor's pointer mask symbol, which
// specs/032-type-descriptors-and-itabs.md owns. The record holds an offset to
// it rather than a pointer, so the field is a relocation of four bytes. A
// caller that cannot supply the symbol gets an error rather than a record with
// a zero in it: the runtime would read that zero as an offset into the module
// and scan whatever it finds. The address-taken locals stay covered by the
// locals bitmap either way, so a caller without specs/032 can leave the table
// out.
func (m *StackMaps) ObjectsSym(name string, gcdata func(*ir.Type) (goobj.SymRef, bool)) (*goobj.Symbol, error) {
	if len(m.Objects) == 0 {
		return nil, nil
	}
	if gcdata == nil {
		return nil, fmt.Errorf("ssa: stackmap: %s: %d stack objects and no type descriptor to point them at",
			m.Func.Name, len(m.Objects))
	}
	ptrSize := m.Frame.Config.ptrSize()
	data := make([]byte, ptrSize)
	// The count is a uintptr, the records follow it.
	for i := int64(0); i < ptrSize; i++ {
		data[i] = byte(uint64(len(m.Objects)) >> uint(8*i))
	}
	var relocs []goobj.Reloc
	for _, o := range m.Objects {
		ref, ok := gcdata(o.Type)
		if !ok {
			return nil, fmt.Errorf("ssa: stackmap: %s: stack object %s of type %v has no type descriptor",
				m.Func.Name, o.Name, o.Type)
		}
		off := len(data)
		data = append(data, make([]byte, 16)...)
		putInt32(data[off:], o.Off)
		putInt32(data[off+4:], o.Size)
		putInt32(data[off+8:], o.PtrBytes)
		relocs = append(relocs, goobj.Reloc{
			Off:  int32(off + 12),
			Size: 4,
			Type: goobj.R_ADDROFF,
			Sym:  ref,
		})
	}
	return &goobj.Symbol{
		Name:   name,
		Type:   goobj.SRODATA,
		Align:  uint32(ptrSize),
		Size:   uint32(len(data)),
		Data:   data,
		Relocs: relocs,
		Flag:   goobj.SymFlagLocal,
	}, nil
}

// PCMap is what the code generator knows about a function and this file does
// not: where each value's instructions landed.
//
// Start and End are indexed by value identifier and are the half-open range of
// program counters the value's instructions occupy. A value that produced no
// instruction has Start of -1.
type PCMap struct {
	FuncSize int64
	MinLC    int

	Start []int64
	End   []int64

	// PrologueEnd is the first program counter at which the frame is
	// established, and EpilogueStart the first one at which it is gone. The
	// range outside them is unsafe: specs/035-goroutines-and-stack-growth.md
	// needs the frame to exist before a preemption can read it.
	PrologueEnd   int64
	EpilogueStart int64

	// Unsafe lists further unsafe ranges that no value names. The
	// stack-growth tail of specs/035-goroutines-and-stack-growth.md is the
	// case: it is emitted code rather than a value, it re-executes the
	// function, and specs/042-arm64-backend.md marks the whole of it.
	Unsafe []PCRange

	// GrowPC is the first instruction of the stack-growth tail, or zero when
	// the function emits none. The tail is where the arguments that travel in
	// registers are in their reserved words of the argument area, so it is the
	// only place the arguments bitmap describes them.
	GrowPC int64
}

// PCRange is a half-open range of program counters.
type PCRange struct{ Lo, Hi int64 }

func (p *PCMap) start(id ID) (int64, bool) {
	if id < 0 || int(id) >= len(p.Start) {
		return 0, false
	}
	return p.Start[id], p.Start[id] >= 0
}

// StackMapPCData returns the PCDATA_StackMapIndex stream.
//
// One entry per safepoint, at the program counter of the call, holding the
// index of the bitmap that describes the frame there. The value stays in
// effect until the next entry, which costs nothing: the runtime reads it only
// at a call's return address.
func (m *StackMaps) StackMapPCData(pcs *PCMap) ([]byte, error) {
	if pcs == nil {
		return nil, fmt.Errorf("ssa: stackmap: %s: no program counters", m.Func.Name)
	}
	entries := []goobj.PCEntry{{PC: 0, Value: -1}}
	for k, sp := range m.Liveness.Safepoints {
		pc, ok := pcs.start(sp.Value)
		if !ok {
			return nil, fmt.Errorf("ssa: stackmap: %s: the safepoint at v%d has no program counter", m.Func.Name, sp.Value)
		}
		entries = append(entries, goobj.PCEntry{PC: pc, Value: m.Index[k]})
	}
	// The tail is the last thing in the function, so one entry at its first
	// instruction covers it and nothing switches back.
	if pcs.GrowPC > 0 && m.GrowIndex >= 0 {
		entries = append(entries, goobj.PCEntry{PC: pcs.GrowPC, Value: m.GrowIndex})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].PC < entries[j].PC })
	return goobj.EncodePCData(dedupPC(entries), pcs.FuncSize, pcs.MinLC)
}

// UnsafePointPCData returns the PCDATA_UnsafePoint stream.
//
// Three kinds of range are unsafe, and the spec's point is that marking them
// is the whole obligation: the runtime scans an asynchronously preempted frame
// conservatively, so no precise map is needed per instruction, only the
// statement that a frame is not readable at all.
//
//   - the prologue, before the frame is established;
//   - the epilogue, after it is torn down;
//   - any sequence that leaves a pointer half written, which isUnsafe reports.
//
// isUnsafe may be nil. HalfWrittenPointer is the target-neutral answer for it.
func (m *StackMaps) UnsafePointPCData(pcs *PCMap, isUnsafe func(*Value) bool) ([]byte, error) {
	if pcs == nil {
		return nil, fmt.Errorf("ssa: stackmap: %s: no program counters", m.Func.Name)
	}
	type span struct{ lo, hi int64 }
	var spans []span
	add := func(lo, hi int64) {
		if lo < 0 {
			lo = 0
		}
		if hi > pcs.FuncSize {
			hi = pcs.FuncSize
		}
		if lo < hi {
			spans = append(spans, span{lo, hi})
		}
	}
	add(0, pcs.PrologueEnd)
	add(pcs.EpilogueStart, pcs.FuncSize)
	for _, r := range pcs.Unsafe {
		add(r.Lo, r.Hi)
	}
	if isUnsafe != nil {
		for _, b := range m.Func.Blocks {
			for _, v := range b.Values {
				if !isUnsafe(v) {
					continue
				}
				lo, ok := pcs.start(v.ID)
				if !ok {
					continue
				}
				hi := lo
				if int(v.ID) < len(pcs.End) {
					hi = pcs.End[v.ID]
				}
				add(lo, hi)
			}
		}
	}
	if len(spans) == 0 {
		return nil, nil
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].lo != spans[j].lo {
			return spans[i].lo < spans[j].lo
		}
		return spans[i].hi < spans[j].hi
	})
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.lo <= last.hi {
			if s.hi > last.hi {
				last.hi = s.hi
			}
			continue
		}
		merged = append(merged, s)
	}
	entries := []goobj.PCEntry{{PC: 0, Value: UnsafePointSafe}}
	for _, s := range merged {
		entries = append(entries, goobj.PCEntry{PC: s.lo, Value: UnsafePointUnsafe})
		if s.hi < pcs.FuncSize {
			entries = append(entries, goobj.PCEntry{PC: s.hi, Value: UnsafePointSafe})
		}
	}
	return goobj.EncodePCData(dedupPC(entries), pcs.FuncSize, pcs.MinLC)
}

// dedupPC keeps the last entry of each program counter.
//
// Two entries at one program counter is not a contradiction to resolve here:
// the later one is the state that instruction runs with, and a call at the
// start of a range is described by the range it is in.
func dedupPC(entries []goobj.PCEntry) []goobj.PCEntry {
	out := entries[:0:0]
	for i, e := range entries {
		if i+1 < len(entries) && entries[i+1].PC == e.PC {
			continue
		}
		out = append(out, e)
	}
	return out
}

// PCDataSym returns a symbol holding a pc-value stream, or nil when the stream
// is empty.
//
// The Pcdata flag is not written to the file. It selects the section class the
// content hash covers, so that a pc-value table cannot merge with read-only
// data that happens to hold the same bytes. See specs/040-object-format.md.
func PCDataSym(name string, data []byte) *goobj.Symbol {
	if len(data) == 0 {
		return nil
	}
	return &goobj.Symbol{
		Name:   name,
		Type:   goobj.SRODATA,
		Align:  1,
		Size:   uint32(len(data)),
		Data:   data,
		Flag:   goobj.SymFlagLocal,
		Pcdata: true,
	}
}

// HalfWrittenPointer reports whether v writes a pointer-holding location in
// more than one instruction.
//
// A single pointer word is written by one store and is never half written. A
// string, a slice or an interface is two or three words, and between the
// stores the location holds one word of the new value and the rest of the old
// one. specs/027-liveness-and-stackmaps.md calls that an unsafe range.
//
// The operations named here are target-neutral and lowering removes them, so
// this is the answer for a function that has not been lowered and the starting
// point for a target's own predicate. A machine operation that expands to
// several stores belongs on a target's list, not on this one.
func HalfWrittenPointer(v *Value) bool {
	if v == nil {
		return false
	}
	switch v.Op {
	case OpStore:
		if len(v.Args) < 2 || v.Args[1] == nil || v.Args[1].Type == nil {
			return false
		}
		t := v.Args[1].Type
		return t.HasPointers() && t.Size > ir.PtrSize
	case OpMove:
		// The source type is not on the value, so size is the only evidence.
		// A multi-word move may carry pointers and is conservatively unsafe.
		return v.AuxInt > ir.PtrSize
	}
	return false
}

// String returns a dump of the maps.
func (m *StackMaps) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stackmaps %s: %d bitmaps, locals nbit=%d, args nbit=%d\n",
		m.Func.Name, len(m.Locals.Maps), m.Locals.NBit, m.Args.NBit)
	for k, sp := range m.Liveness.Safepoints {
		fmt.Fprintf(&b, "  safepoint %d at v%d: map %d\n", k, sp.Value, m.Index[k])
	}
	for i, mp := range m.Locals.Maps {
		fmt.Fprintf(&b, "  locals %d: %x\n", i, mp)
	}
	for _, o := range m.Objects {
		fmt.Fprintf(&b, "  object %s: off=%d size=%d ptrbytes=%d\n", o.Name, o.Off, o.Size, o.PtrBytes)
	}
	return b.String()
}
