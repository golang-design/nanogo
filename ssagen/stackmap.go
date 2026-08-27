// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

// The garbage collector's tables, per specs/027-liveness-and-stackmaps.md.
//
// ssa/liveness.go and ssa/stackmap.go decide what the collector is told. This
// file decides where it reads it from: two FUNCDATA symbols holding the
// bitmaps, and two PCDATA streams that select a bitmap and mark the ranges
// where the frame is not readable at all.
//
// # Why a bitmap symbol is named and a pc-value table is not
//
// The two look alike and the linker treats them as opposites.
//
// A pc-value table is merged into runtime.pctab. cmd/link decides whether a
// symbol takes part in the data layout by asking whether it has a name
// (loader.topLevelSym), so a *named* table is also placed into the read-only
// section and that placement overwrites the offset the linker recorded in its
// own table. It must be anonymous.
//
// A bitmap symbol is the other case. cmd/link's pclntab pass collects every
// FUNCDATA symbol into go:func.*, sets its value to the offset it was given
// there and marks it special, before dodata runs, so it never enters the data
// layout either. It keeps its name, and the name matters: obj's content hash
// covers a section class that is derived from the name, and the "gclocals·"
// prefix is what puts a bitmap in the go:func.* class. Without the prefix a
// bitmap hashes in the same class as ordinary read-only data and can merge
// with a symbol that happens to hold the same bytes, which would then be
// placed twice. gc names them the same way and for the same reason.
//
// # Why the position of an entry is its index
//
// The object format carries no index with a FUNCDATA or PCDATA entry: the
// linker reads them in order and the position is the index
// (cmd/link/internal/loader, Funcdata and Pcdata). An absent table is
// therefore an empty entry and never an omission, or every later table moves
// up by one and the collector reads the arguments map as the locals map.

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/ssa"
)

// tables holds the collector's symbols for one function.
type tables struct {
	funcdata []*obj.Symbol
	pcdata   []*obj.Symbol
	maps     *ssa.StackMaps
}

// buildTables computes the liveness, the stack maps and the pc-value streams.
//
// Every failure returns an error, and Emit turns that into a refusal to emit
// the function. A text symbol with no maps, or with one of the two, is a
// program that runs until a collection happens at the wrong moment
// (specs/027-liveness-and-stackmaps.md).
func (e *emitter) buildTables(size int64) (*tables, error) {
	lv, err := ssa.ComputeLiveness(e.a, e.items)
	if err != nil {
		return nil, err
	}
	m, err := ssa.BuildStackMaps(lv, e.fr)
	if err != nil {
		return nil, err
	}
	// A function with no safepoint gets no bitmap from the safepoints, because
	// BuildStackMaps writes one per safepoint. One thing reads a map for such
	// a frame all the same: a function with a stack-growth check reaches
	// runtime.morestack, and the stack copier reads the arguments bitmap of
	// every frame it moves, the one that called morestack included
	// (runtime/stack.go, adjustframe). With no bitmap the runtime throws
	// "missing stackmap" there, and the shape that reaches it is ordinary: a
	// leaf whose frame is at least StackSmall.
	//
	// The stack-growth tail's own map closes it. Every function that emits a
	// tail has one, whether or not it makes a call, which is the property gc
	// gets from adding a stack map for the entry block of every function
	// (cmd/compile/internal/liveness, compact). A leaf with a growable frame
	// used to be refused here for want of it.

	pcs := &ssa.PCMap{
		FuncSize:    size,
		MinLC:       minLC,
		Start:       e.pcStart,
		End:         e.pcEnd,
		PrologueEnd: e.prologueEnd,
		// A function has one teardown per return, so there is no single
		// program counter at which the frame is gone. Each teardown is a
		// range in Unsafe instead, which epilogue appends, and so is the
		// stack-growth tail.
		EpilogueStart: size,
		Unsafe:        e.unsafe,
		GrowPC:        int64(e.growPC),
	}
	stackMap, err := m.StackMapPCData(pcs)
	if err != nil {
		return nil, err
	}
	// HalfWrittenPointer is the target-neutral predicate and it names
	// operations lowering has already removed, so it reports nothing here. It
	// is passed rather than nil because the answer for a machine operation
	// that expands into several stores belongs to the target, and this is
	// where such a predicate would arrive.
	unsafePoint, err := m.UnsafePointPCData(pcs, ssa.HalfWrittenPointer)
	if err != nil {
		return nil, err
	}

	t := &tables{
		funcdata: make([]*obj.Symbol, 2),
		pcdata:   make([]*obj.Symbol, 2),
		maps:     m,
	}
	argsBits, localsBits := m.Args.Bytes(), m.Locals.Bytes()
	t.funcdata[ssa.FUNCDATA_ArgsPointerMaps] = m.ArgsSym(gclocalsName(argsBits))
	t.funcdata[ssa.FUNCDATA_LocalsPointerMaps] = m.LocalsSym(gclocalsName(localsBits))
	t.pcdata[ssa.PCDATA_UnsafePoint] = pcdataSym(unsafePoint)
	t.pcdata[ssa.PCDATA_StackMapIndex] = pcdataSym(stackMap)

	// The stack objects table, when the function has one. It is what lets the
	// collector reach an address-taken local through a pointer to it rather
	// than only through the locals bitmap, which is the precision
	// specs/027-liveness-and-stackmaps.md asks for.
	//
	// It is appended and not written into a fixed slot: FUNCDATA_StackObjects
	// is the highest index this package writes, and a function with no stack
	// object must keep a two-entry array rather than one with a hole in it,
	// because the position of an entry is its index.
	objects, err := m.ObjectsSym(e.opt.Sym+".stkobj", e.gcdata)
	if err != nil {
		return nil, err
	}
	if objects != nil {
		if len(t.funcdata) != ssa.FUNCDATA_StackObjects {
			return nil, fmt.Errorf("the stack objects table is index %d and %d entries are written",
				ssa.FUNCDATA_StackObjects, len(t.funcdata))
		}
		t.funcdata = append(t.funcdata, objects)
	}
	for i, s := range t.funcdata {
		if s == nil || s.Size == 0 {
			// cmd/link refuses a zero-size funcdata symbol, and a bitmap is
			// never empty: it holds the two counts even when it holds no bit.
			return nil, fmt.Errorf("funcdata %d is empty", i)
		}
	}
	return t, nil
}

// pcdataSym returns the symbol holding one pc-value stream.
//
// An empty stream is still an entry, because the position of an entry is its
// index. ssa.PCDataSym returns nothing for one, so the empty symbol is built
// here.
func pcdataSym(data []byte) *obj.Symbol {
	s := ssa.PCDataSym("", data)
	if s == nil {
		s = &obj.Symbol{Type: obj.SRODATA, Align: 1, Pcdata: true}
	}
	// The name is cleared rather than never set, because ssa.PCDataSym takes
	// one and the linker requires none. See the note at the top of this file.
	s.Name = ""
	s.Anonymous = true
	return s
}

// gclocalsName returns the linker name of a bitmap symbol.
//
// It is gc's name for the same bytes: the prefix puts the symbol in the
// go:func.* class of the content hash, and the hash of the content makes two
// functions with the same bitmap resolve to one symbol. The digest is gc's as
// well (cmd/internal/hash.Sum32, which is sha256 with the first byte
// inverted), so a nanogo bitmap and a gc bitmap that describe the same frame
// are one symbol in the linked binary rather than two.
func gclocalsName(data []byte) string {
	sum := sha256.Sum256(data)
	sum[0] ^= 0xff
	return "gclocals·" + base64.StdEncoding.EncodeToString(sum[:16])
}

// gcdata returns the pointer mask symbol of a stack object's type, defining it
// in this object the first time the type is seen.
//
// The name is not built here. rtype writes the descriptor of the type and the
// descriptor points at its own mask, so the mask is found in the descriptor's
// own words: the relocation at the GCData field names it, and the symbol it
// names is in the same set. Deciding the name a second time here could
// disagree with the one the descriptor carries, and then the collector would
// read one mask for a type described by another.
//
// The symbol is content-addressable, as every mask rtype returns is. Two types
// with the same pointer map are one symbol in the linked binary, and a package
// that emits the descriptor as well emits these bytes once.
func (e *emitter) gcdata(t *ir.Type) (obj.SymRef, bool) {
	if t == nil {
		return obj.SymRef{}, false
	}
	set, err := rtype.Descriptor(t)
	if err != nil || len(set) == 0 {
		return obj.SymRef{}, false
	}
	var name string
	for _, r := range set[0].Relocs {
		if r.Off == rtype.GCDataOffset {
			name = r.Target
			break
		}
	}
	if name == "" {
		return obj.SymRef{}, false
	}
	if r, ok := e.gcbits[name]; ok {
		return r, true
	}
	for _, sym := range set[1:] {
		if sym.Name != name {
			continue
		}
		d := &obj.Symbol{
			Name:   sym.Name,
			Type:   sym.Kind,
			Align:  sym.Align,
			Size:   uint32(len(sym.Data)),
			Data:   sym.Data,
			Relocs: nil,
		}
		if sym.Dupok {
			d.Flag |= obj.SymFlagDupok
		}
		if d.Align == 0 {
			d.Align = 1
		}
		r := e.pkg.AddHashedDef(d)
		if e.gcbits == nil {
			e.gcbits = make(map[string]obj.SymRef)
		}
		e.gcbits[name] = r
		return r, true
	}
	return obj.SymRef{}, false
}

// keepDescribableObjects drops from the stack objects table every object whose
// type has no descriptor.
//
// A record names the type's pointer mask, and rtype refuses a type it cannot
// write. Refusing the whole function for that would turn a precision feature
// into a coverage loss, so the object leaves the table instead. What replaces
// the table for it is the locals bitmap: ssa.ComputeLiveness keeps a frame
// object that is not in the table live for the whole function, because there
// is then nothing else that can reach it.
//
// The items slice is the frame's own, so clearing the mark here is what
// ssa.Frame.StackObjects reads later.
func (e *emitter) keepDescribableObjects() {
	for i := range e.items {
		it := &e.items[i]
		if !it.StackObject {
			continue
		}
		if _, ok := e.gcdata(it.Type); !ok {
			it.StackObject = false
		}
	}
}
