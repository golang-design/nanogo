// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"encoding/binary"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/ssa"
)

// TestFrameGeometryMatchesTheRuntime checks the assertion that discharges
// specs/027-liveness-and-stackmaps.md's obligation on the code generator.
//
// The runtime does not read the frame layout. It computes the block it scans
// as [Varp - nbit*PtrSize, Varp), where Varp is the caller's stack pointer
// less one word on this target, so a frame whose Varp is elsewhere is scanned
// at the wrong address with a bitmap that is internally consistent. Each case
// here is a way for the two to disagree.
func TestFrameGeometryMatchesTheRuntime(t *testing.T) {
	tests := []struct {
		name      string
		frameless bool
		size      int64
		fr        ssa.Frame
		want      string
	}{
		{"a frame the layout and the prologue agree on", false, 32,
			ssa.Frame{Size: 32, Varp: 24, LocalsBase: 8, LocalsBits: 1}, ""},
		{"a frameless function", true, 0, ssa.Frame{}, ""},
		{"a frame the stack pointer cannot hold", false, 24,
			ssa.Frame{Size: 24, Varp: 16, LocalsBase: 8}, "16-byte aligned"},
		{"a frameless function with a frame", true, 32,
			ssa.Frame{Size: 32, Varp: 24, LocalsBase: 8}, "nothing in its frame"},
		{"a frame with no room for the two saved words", false, 0,
			ssa.Frame{Varp: 0}, "link register"},
		{"a frame that saves only the link register", false, 16,
			ssa.Frame{Size: 16, Varp: 16, LocalsBase: 8}, "Varp"},
		{"locals over the saved link register", false, 32,
			ssa.Frame{Size: 32, Varp: 24, LocalsBase: 0}, "saved link register"},
		{"a pointer in a frame the runtime does not scan", false, 16,
			ssa.Frame{Size: 16, Varp: 8, LocalsBase: 8, LocalsBits: 1}, "does not scan"},
	}
	for _, tt := range tests {
		e := &emitter{opt: Options{Sym: "test.f"}}
		fr := tt.fr
		e.fr, e.frame.size = &fr, tt.size
		err := e.checkFrame(tt.frameless)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.want != "" && err == nil:
			t.Errorf("%s: accepted", tt.name)
		case tt.want != "" && !strings.Contains(err.Error(), tt.want):
			t.Errorf("%s: %v, want a report of %q", tt.name, err, tt.want)
		}
	}
}

// TestTablesAreIndexedByPosition checks the shape of what a text symbol
// carries for the collector.
//
// The object format carries no index with a FUNCDATA or PCDATA entry: the
// linker reads them in order and the position is the index. An entry that is
// left out therefore moves every later table up by one, and the collector
// reads the arguments map where the locals map should be.
func TestTablesAreIndexedByPosition(t *testing.T) {
	// A pointer argument, live across a call: the arguments bitmap has a bit
	// and the locals bitmap has one for the slot the argument is spilled to,
	// so the two symbols differ and the test sees two of everything.
	const src = "package main\n\nfunc gcNow() int\nfunc use(p *int) int\n\n" +
		"func f(p *int, b int) int {\n\tgcNow()\n\treturn use(p)\n}\n"
	c := compile(t, src, "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)

	if len(r.Funcdata) != 2 {
		t.Fatalf("%d funcdata symbols, want the arguments map and the locals map", len(r.Funcdata))
	}
	if len(r.Pcdata) != 2 {
		t.Fatalf("%d pcdata streams, want the unsafe points and the stack map index", len(r.Pcdata))
	}
	for i, s := range r.Funcdata {
		// A bitmap symbol keeps a name, and the prefix is what puts it in the
		// go:func.* class of the content hash. Without it a bitmap can merge
		// with read-only data that happens to hold the same bytes.
		if !strings.HasPrefix(s.Name, "gclocals·") {
			t.Errorf("funcdata %d is named %q", i, s.Name)
		}
		if s.Anonymous {
			t.Errorf("funcdata %d is anonymous, and cmd/link places it by name", i)
		}
		if s.Size < 8 || s.Size != uint32(len(s.Data)) {
			t.Errorf("funcdata %d is %d bytes with %d of data, and a bitmap holds two counts", i, s.Size, len(s.Data))
		}
	}
	// The name is the content, so two maps that differ cannot share it. The
	// arguments map of this function marks the incoming pointer at every
	// safepoint and the locals map marks the slot it is spilled to at one, so
	// they differ.
	if r.Funcdata[0].Name == r.Funcdata[1].Name {
		t.Error("the arguments map and the locals map of this function are one symbol")
	}
	for i, s := range r.Pcdata {
		// A pc-value table is merged into runtime.pctab and must carry no
		// name: cmd/link decides whether a symbol takes part in the data
		// layout by asking whether it has one.
		if s.Name != "" || !s.Anonymous {
			t.Errorf("pcdata %d is named %q", i, s.Name)
		}
		if !s.Pcdata {
			t.Errorf("pcdata %d is not marked as a pc-value table, so it can merge with read-only data", i)
		}
	}
	// The function makes one call, so it has one safepoint and the stream
	// that selects a bitmap for it is not empty.
	if len(r.Pcdata[ssa.PCDATA_StackMapIndex].Data) == 0 {
		t.Error("the function makes a call and no stack map index is in effect at it")
	}
	if len(r.Pcdata[ssa.PCDATA_UnsafePoint].Data) == 0 {
		t.Error("the function has a prologue and no range of it is marked unsafe")
	}
}

// TestStackObjectsTableIsWritten checks that a function with an address-taken
// pointer local carries FUNCDATA_StackObjects.
//
// The table is what lets the collector reach such a local through a pointer to
// it rather than only through the locals bitmap, which is the precision
// specs/027-liveness-and-stackmaps.md asks for: below the point where the
// address was taken, the bitmap does not describe the object and this table
// is the only thing that does.
//
// The record's last field is an offset to the type's pointer mask, so a
// relocation is the assertion: a zero there is an offset the runtime would
// resolve into the module and scan whatever it found.
func TestStackObjectsTableIsWritten(t *testing.T) {
	const src = "package main\n\nfunc use(p **int) int\n\n" +
		"func f() int {\n\tvar p *int\n\treturn use(&p)\n}\n"
	c := compile(t, src, "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)

	if len(r.Funcdata) != 3 {
		t.Fatalf("%d funcdata symbols, want the two bitmaps and the stack objects table", len(r.Funcdata))
	}
	so := r.Funcdata[ssa.FUNCDATA_StackObjects]
	if !strings.HasSuffix(so.Name, ".stkobj") {
		// The suffix is what puts the symbol in the go:func.* class of obj's
		// content hash, which is where cmd/link places it.
		t.Errorf("the stack objects table is named %q", so.Name)
	}
	// One count of a word, then one sixteen-byte record.
	if want := uint32(8 + 16); so.Size != want || uint32(len(so.Data)) != want {
		t.Fatalf("the table is %d bytes with %d of data, want %d for one object", so.Size, len(so.Data), want)
	}
	if n := binary.LittleEndian.Uint64(so.Data); n != 1 {
		t.Errorf("the table says it holds %d objects, want one", n)
	}
	if off := int32(binary.LittleEndian.Uint32(so.Data[8:])); off >= 0 {
		t.Errorf("the object is at %d from Varp, and a local is below it", off)
	}
	if len(so.Relocs) != 1 {
		t.Fatalf("%d relocations, want one to the type's pointer mask", len(so.Relocs))
	}
	rel := so.Relocs[0]
	if rel.Off != 8+12 || rel.Size != 4 || rel.Type != obj.R_ADDROFF {
		t.Errorf("the mask relocation is %+v, want a four byte ADDROFF at 20", rel)
	}
	mask := p.Def(rel.Sym)
	if mask == nil {
		t.Fatalf("the mask relocation names %v, which this object does not define", rel.Sym)
	}
	if !strings.HasPrefix(mask.Name, "runtime.gcbits.") {
		t.Errorf("the record points at %q, want a pointer mask", mask.Name)
	}
}

// TestNoStackObjectKeepsTwoTables checks that a function without one writes no
// third entry.
//
// The position of an entry is its index, so a table written for a function
// that has no stack object is not spare bytes: it is an entry the runtime
// reads as a stack objects table for a frame that has none.
func TestNoStackObjectKeepsTwoTables(t *testing.T) {
	c := compile(t, "package main\n\nfunc gcNow() int\n\nfunc f(a int) int { return gcNow() + a }\n", "f")
	r := emit(t, c, obj.NewPackage("main"))
	if len(r.Funcdata) != 2 {
		t.Fatalf("%d funcdata symbols, want the two bitmaps", len(r.Funcdata))
	}
}

// TestAddRefusesAnIncompleteResult checks that a text symbol cannot reach an
// object without the tables the collector reads.
//
// specs/027-liveness-and-stackmaps.md: a binary with a partial map runs until
// a collection happens at the wrong moment, so the refusal is at the point
// where the symbol would be written and not later.
func TestAddRefusesAnIncompleteResult(t *testing.T) {
	c := compile(t, "package main\n\nfunc f(a int) int { return a }\n", "f")
	build := func() *Result {
		p := obj.NewPackage("main")
		return emit(t, c, p)
	}
	tests := []struct {
		name   string
		break_ func(r *Result)
		want   string
	}{
		{"no FuncInfo", func(r *Result) { r.FuncInfo = nil }, "FuncInfo"},
		{"no locals map", func(r *Result) { r.Funcdata[ssa.FUNCDATA_LocalsPointerMaps] = nil }, "funcdata 1"},
		{"no stack map index", func(r *Result) { r.Pcdata[ssa.PCDATA_StackMapIndex] = nil }, "pcdata 1"},
	}
	for _, tt := range tests {
		r := build()
		tt.break_(r)
		_, err := r.Add(obj.NewPackage("main"))
		if err == nil {
			t.Errorf("%s: added", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want a report of %q", tt.name, err, tt.want)
		}
	}
}

// TestPcdataSymIsAnEntryEvenWhenEmpty checks the placeholder.
//
// An index with no table is still an entry, because the position of an entry
// is its index. ssa.PCDataSym returns nothing for an empty stream, so the
// empty symbol is this package's to build.
func TestPcdataSymIsAnEntryEvenWhenEmpty(t *testing.T) {
	s := pcdataSym(nil)
	if s == nil {
		t.Fatal("an empty stream produced no entry")
	}
	if !s.Anonymous || s.Name != "" || !s.Pcdata || s.Size != 0 {
		t.Errorf("the empty entry is %+v", s)
	}
	s = pcdataSym([]byte{2, 4, 0})
	if s.Name != "" || !s.Anonymous || s.Size != 3 {
		t.Errorf("a stream of three bytes gave %+v", s)
	}
}

// TestGclocalsNameIsGcs checks the name a bitmap symbol carries.
//
// It is gc's name for the same bytes, so a nanogo bitmap and a gc bitmap that
// describe the same frame are one symbol in the linked binary. The digest is
// cmd/internal/hash.Sum32, which is sha256 with the first byte inverted, and
// the encoding is base64 of its first sixteen bytes.
func TestGclocalsNameIsGcs(t *testing.T) {
	// The arguments map of a function with one pointer word of arguments,
	// live at one safepoint. gc prints this name for these bytes.
	data := []byte{2, 0, 0, 0, 1, 0, 0, 0, 1, 0}
	if got, want := gclocalsName(data), "gclocals·wvjpxkknJ4nY1JtrArJJaw=="; got != want {
		t.Errorf("the name is %q, and gc writes %q for the same bytes", got, want)
	}
	if gclocalsName([]byte{0}) == gclocalsName([]byte{1}) {
		t.Error("two bitmaps that differ share a name")
	}
	comparisons++
}

// TestAnUndescribableObjectLeavesTheTable covers the fallback that keeps such
// an object conservatively live.
//
// A record names its type's pointer mask, and rtype refuses a type it cannot
// write. Refusing the whole function for that would turn a precision feature
// into a coverage loss, so the object leaves the table. What replaces the
// table for it is the locals bitmap: ssa.ComputeLiveness keeps a frame object
// that is not in the table live for the whole function, because nothing else
// can reach it. Clearing the mark is therefore the safe direction and leaving
// it set is not.
//
// The type is a pointer that carries a name. rtype reads that as a defined
// type and asks for its method set, which an ir.Type built here does not have.
func TestAnUndescribableObjectLeavesTheTable(t *testing.T) {
	named := &ir.Type{Kind: ir.Ptr, Elem: typeInt, Name: "*int"}
	if err := ir.Layout(named); err != nil {
		t.Fatal(err)
	}
	if _, err := rtype.Descriptor(named); err == nil {
		t.Fatal("the type has a descriptor, so this test measures nothing")
	}
	local := &ir.Object{Name: "obj", Type: named, Class: ir.ClassLocal, Addrtaken: true}
	c := hand(t, "undescribable", func(f *ssa.Func) {
		f.Frame = []*ir.Object{local}
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		a := e.NewValue(0, ssa.OpLocalAddr, named, mem)
		a.Aux = local
		call := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, a, mem)
		call.Aux = &ir.Object{Name: "main.use"}
		n := e.NewValue(0, ssa.OpSelectN, typeInt, call)
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, n, call)
	})
	r, err := Emit(c.f, c.a, obj.NewPackage("main"), Options{Sym: c.f.Sym})
	if err != nil {
		t.Fatalf("a function with an undescribable frame object was refused: %v", err)
	}
	if len(r.Funcdata) != 2 {
		t.Fatalf("%d funcdata symbols, want the two bitmaps: the object has no record to point at", len(r.Funcdata))
	}
	for i := range r.maps.Frame.Items {
		it := &r.maps.Frame.Items[i]
		if it.Kind == ssa.ItemObject && it.StackObject {
			t.Errorf("%s is in the table and its type has no pointer mask", it.Name)
		}
	}
	// And the bitmap covers it at the call, which is below its last address.
	if len(r.maps.Locals.Maps) == 0 {
		t.Fatal("the function makes a call and carries no bitmap")
	}
	if r.maps.Locals.Maps[r.maps.Index[0]][0]&1 == 0 {
		t.Error("the object is outside the table and outside the bitmap at the call, so nothing describes it")
	}
}

// TestALeafWithAGrowableFrameCarriesTheGrowthMap is what closed a gap
// ssa.BuildStackMaps had.
//
// A function that makes no call has no safepoint, so the safepoints write no
// bitmap at all. The runtime reads one anyway when such a function grows the
// stack: runtime.morestack copies the frames and adjustframe reads the
// arguments bitmap of the frame that called it. It throws "missing stackmap"
// when there is none, and the shape that reaches it is ordinary: a leaf whose
// frame is at least StackSmall, which is the frame that carries a growth
// check. Such a function used to be refused.
//
// The stack-growth tail's own map is what it has now, and the pointer argument
// is in it: the tail wrote that word before calling the runtime, so the copier
// has to move it.
func TestALeafWithAGrowableFrameCarriesTheGrowthMap(t *testing.T) {
	// Enough values live at once to fill the registers many times over, so
	// the frame is far past StackSmall, and one pointer argument so that the
	// arguments bitmap is one the runtime reads and one that has a bit.
	const n = 64
	c := hand(t, "bigleaf", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		e.NewValue(0, ssa.OpArg, typePtrInt)
		a := e.NewValue(0, ssa.OpArg, typeInt)
		vals := make([]*ssa.Value, n)
		for i := range vals {
			vals[i] = e.NewValue(0, ssa.OpAdd, typeInt, a, a)
		}
		sum := vals[0]
		for i := 1; i < n; i++ {
			sum = e.NewValue(0, ssa.OpAdd, typeInt, sum, vals[i])
		}
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, sum, mem)
	})
	r, err := Emit(c.f, c.a, obj.NewPackage("main"), Options{Sym: c.f.Sym})
	if err != nil {
		t.Fatalf("a leaf with a growable frame was refused: %v", err)
	}
	if n := len(r.maps.Locals.Maps); n != 1 {
		t.Fatalf("%d bitmaps, want the one the stack-growth tail uses", n)
	}
	if r.maps.GrowIndex != 0 {
		t.Fatalf("the growth map is index %d, and it is the only one", r.maps.GrowIndex)
	}
	// The pointer argument occupies the first word of the argument area, so
	// the tail's bitmap has its first bit.
	args := r.Funcdata[ssa.FUNCDATA_ArgsPointerMaps].Data
	if len(args) < 9 {
		t.Fatalf("the arguments bitmap is %d bytes", len(args))
	}
	if args[8]&1 == 0 {
		t.Errorf("the tail's arguments bitmap is %#x, and the tail wrote a pointer into the first word", args[8])
	}
}

// TestStackObjectMaskIsTheBitmapAndNotTheOnDemandWord covers the half of
// specs/032's GCData rule that has no second chance.
//
// Past internal/abi.MaxPtrmaskBytes*8 pointer words a type's descriptor points
// at a word the runtime fills the mask into, in BSS, rather than at the mask.
// A record here holds the offset of its mask from the start of the section and
// the runtime resolves that offset against moduledata.rodata, so that word
// would be read at an address that is not a mask and the object scanned by
// whatever bits are there. gc keeps the two apart by passing onDemandAllowed
// false from the one caller that describes a stack object, and this pass does
// it by asking rtype.StackObjectMask rather than by reading the descriptor's
// own GCData relocation, which is where it used to read it.
//
// The bound is 128 words, so the local here is past it and the local in the
// test above is not. The pair is the test.
func TestStackObjectMaskIsTheBitmapAndNotTheOnDemandWord(t *testing.T) {
	const src = "package main\n\ntype wide [200]*int\n\nfunc use(p *wide) int\n\n" +
		"func f() int {\n\tvar w wide\n\treturn use(&w)\n}\n"
	c := compile(t, src, "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)

	if len(r.Funcdata) != 3 {
		t.Fatalf("%d funcdata symbols, want the two bitmaps and the stack objects table", len(r.Funcdata))
	}
	so := r.Funcdata[ssa.FUNCDATA_StackObjects]
	if len(so.Relocs) != 1 {
		t.Fatalf("%d relocations, want one to the type's pointer mask", len(so.Relocs))
	}
	mask := p.Def(so.Relocs[0].Sym)
	if mask == nil {
		t.Fatalf("the mask relocation names %v, which this object does not define", so.Relocs[0].Sym)
	}
	if strings.HasPrefix(mask.Name, "type:.gcmask.") {
		t.Fatalf("the record points at %q, which is the word the runtime fills in and is in BSS", mask.Name)
	}
	if !strings.HasPrefix(mask.Name, "runtime.gcbits.") {
		t.Fatalf("the record points at %q, want a pointer mask", mask.Name)
	}
	if mask.Type != obj.SRODATA {
		t.Errorf("%s is %v, and the runtime resolves this offset against moduledata.rodata", mask.Name, mask.Type)
	}
	// 200 pointer words is 200 bits, which is 25 bytes rounded up to a word.
	if want := 32; len(mask.Data) != want {
		t.Errorf("the mask is %d bytes, want %d", len(mask.Data), want)
	}
}
