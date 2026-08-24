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

	"golang.design/x/nanogo/obj"
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
	// A function with no safepoint gets no bitmap: BuildStackMaps writes one
	// per safepoint. That is what the reference compiler does as well for the
	// locals map, and it is safe for as long as nothing reads a map for the
	// frame. One thing does. A function with a stack-growth check reaches
	// runtime.morestack, and the stack copier reads the arguments bitmap of
	// every frame it moves, the one that called morestack included
	// (runtime/stack.go, adjustframe). With no bitmap the runtime throws
	// "missing stackmap" there. The shape that reaches it is a leaf whose
	// frame is at least StackSmall, which is an ordinary function and not a
	// corner.
	//
	// What closes it is an entry stack map. gc's liveness adds one for the
	// entry block whatever the function does (cmd/compile/internal/liveness,
	// compact), so its count is never zero. ssa.BuildStackMaps has no entry
	// safepoint, and building the arguments bitmap here instead would be a
	// second answer to which words hold pointers, which specs/027 says must
	// come from ir.Type.PtrBits through one path. So the function is refused
	// rather than emitted with a map the collector cannot read.
	if len(m.Locals.Maps) == 0 && !e.frame.nosplit && e.frame.args > 0 {
		return nil, fmt.Errorf("the function makes no call, so it has no safepoint and no stack map, "+
			"and its %d byte frame carries a stack-growth check whose copier reads the bitmap of "+
			"its %d byte argument area: ssa.BuildStackMaps needs an entry stack map",
			e.frame.size, e.frame.args)
	}

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

	// A stack object table is not written. specs/027 wants one for an
	// address-taken local whose type holds pointers, and the record holds an
	// offset to that type's descriptor, which specs/032 has no writer for. The
	// table is precision rather than coverage: such a local is in the locals
	// bitmap for the whole of its marked lifetime, which is the conservative
	// answer and the safe one. ssa.StackMaps.ObjectsSym is where it goes the
	// day a descriptor can be named.
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
