// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa_test

// This file is the external test package of ssa, and it is external for one
// reason: the end to end test lowers with ssa/rules, which imports ssa. A test
// inside the package could not import it.
//
// It carries the tests that read like a consumer of the maps, which is what
// the encoding tests are: the runtime is a consumer, spikes/stackmap is a
// consumer written by hand, and both read bytes rather than structures.

import (
	"fmt"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/ssa/rules"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

var (
	smInt    = smType(&ir.Type{Kind: ir.Int64, Name: "int"})
	smBool   = smType(&ir.Type{Kind: ir.Bool, Name: "bool"})
	smPtr    = smType(&ir.Type{Kind: ir.Ptr, Name: "*int"})
	smString = smType(&ir.Type{Kind: ir.String, Name: "string"})
)

func smType(t *ir.Type) *ir.Type {
	if err := ir.Layout(t); err != nil {
		panic(err)
	}
	return t
}

// smFunc returns a function with an entry block and its initial memory.
func smFunc(name string) (*ssa.Func, *ssa.Block, *ssa.Value) {
	f := ssa.NewFunc(name)
	e := f.Entry
	e.Kind = ssa.BlockRet
	return f, e, e.NewValue(0, ssa.OpInitMem, ssa.MemType)
}

func smRet(b *ssa.Block, mem *ssa.Value, results ...*ssa.Value) {
	b.Control = b.NewValue(0, ssa.OpMakeResult, ssa.MemType, append(append([]*ssa.Value{}, results...), mem)...)
}

// smMaps runs the whole pipeline over an already built function.
func smMaps(t *testing.T, f *ssa.Func, cfg ssa.FrameConfig) (*ssa.Frame, *ssa.StackMaps) {
	t.Helper()
	if err := ssa.Check(f); err != nil {
		t.Fatalf("the function does not verify: %v", err)
	}
	ssa.SplitCriticalEdges(f)
	a, err := ssa.Allocate(f, ssa.NewArm64Target())
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	items, err := ssa.FrameItems(a)
	if err != nil {
		t.Fatalf("FrameItems: %v", err)
	}
	lv, err := ssa.ComputeLiveness(a, items)
	if err != nil {
		t.Fatalf("ComputeLiveness: %v", err)
	}
	fr, err := ssa.LayoutFrame(f, items, cfg)
	if err != nil {
		t.Fatalf("LayoutFrame: %v", err)
	}
	m, err := ssa.BuildStackMaps(lv, fr)
	if err != nil {
		t.Fatalf("BuildStackMaps: %v", err)
	}
	smCheck(t, f.Name, fr, m)
	return fr, m
}

// smCheck asserts the properties every stack map has to have, whatever the
// function was. The corpus test runs it over the standard library and the unit
// tests run it over what they build, so a violation is caught by whichever
// reaches it first.
func smCheck(t *testing.T, name string, fr *ssa.Frame, m *ssa.StackMaps) {
	t.Helper()
	ptrSize := int64(ir.PtrSize)
	if m.Locals.NBit != fr.LocalsBits || m.Args.NBit != fr.ArgsBits {
		t.Fatalf("%s: the maps hold %d and %d bits and the frame needs %d and %d",
			name, m.Locals.NBit, m.Args.NBit, fr.LocalsBits, fr.ArgsBits)
	}
	// nbit covers the frame: the runtime scans [Varp-nbit*PtrSize, Varp), so a
	// map that is too short leaves the words below it unscanned.
	if int64(m.Locals.NBit)*ptrSize != fr.Varp-fr.PtrBase {
		t.Fatalf("%s: %d bits cover %d bytes and the pointer area is %d bytes",
			name, m.Locals.NBit, int64(m.Locals.NBit)*ptrSize, fr.Varp-fr.PtrBase)
	}
	if len(m.Locals.Maps) != len(m.Args.Maps) {
		t.Fatalf("%s: %d locals bitmaps and %d arguments bitmaps, and one index selects both",
			name, len(m.Locals.Maps), len(m.Args.Maps))
	}
	stride := int(m.Locals.NBit+7) / 8
	for i, bm := range m.Locals.Maps {
		if len(bm) != stride {
			t.Fatalf("%s: bitmap %d is %d bytes and nbit=%d needs %d", name, i, len(bm), m.Locals.NBit, stride)
		}
		for b := int32(0); b < int32(len(bm))*8; b++ {
			if bm[b/8]&(1<<uint(b%8)) == 0 {
				continue
			}
			if b >= m.Locals.NBit {
				t.Fatalf("%s: bitmap %d sets bit %d and the map holds %d bits", name, i, b, m.Locals.NBit)
			}
			// Every set bit names a word inside the frame that some item calls
			// a pointer. A bit that no PtrBits justifies is a word specs/035
			// rewrites when it copies the stack.
			off := fr.PtrBase + int64(b)*ptrSize
			if off < fr.PtrBase || off >= fr.Varp {
				t.Fatalf("%s: bitmap %d bit %d is at %d, outside [%d,%d)", name, i, b, off, fr.PtrBase, fr.Varp)
			}
			if !smWordIsAPointer(fr, off) {
				t.Fatalf("%s: bitmap %d marks %d, and no item has a pointer word there:\n%s", name, i, off, fr)
			}
		}
	}
	for k, idx := range m.Index {
		if idx < 0 || int(idx) >= len(m.Locals.Maps) {
			t.Fatalf("%s: safepoint %d selects bitmap %d of %d", name, k, idx, len(m.Locals.Maps))
		}
	}
	// The round trip: at each safepoint, a bit is set exactly for the pointer
	// words of the items the liveness calls live there. The loop above proves
	// that a set bit names a pointer word of some item; this proves it names a
	// pointer word of an item that is live at that safepoint, and that no live
	// pointer word was left out.
	for k := range m.Index {
		bm := m.Locals.Maps[m.Index[k]]
		for i := range fr.Items {
			it := &fr.Items[i]
			live := m.Liveness.LiveAt(k, i)
			for w := int64(0); w*ptrSize < it.Size; w++ {
				if w/8 >= int64(len(it.Type.PtrBits)) || it.Type.PtrBits[w/8]&(1<<uint(w%8)) == 0 {
					continue
				}
				b, ok := fr.LocalBit(it.Off + w*ptrSize)
				if !ok {
					t.Fatalf("%s: %s word %d has no bit", name, it.Name, w)
				}
				set := bm[b/8]&(1<<uint(b%8)) != 0
				if set != live {
					t.Fatalf("%s: safepoint %d: %s word %d is live=%v and its bit is %v",
						name, k, it.Name, w, live, set)
				}
			}
		}
	}
	for _, o := range m.Objects {
		if o.Off >= 0 {
			t.Fatalf("%s: stack object %s is at %d from Varp and a local is below it", name, o.Name, o.Off)
		}
		if int64(o.Off)+int64(o.Size) > 0 || int64(o.PtrBytes) > int64(o.Size) {
			t.Fatalf("%s: stack object %s does not fit the frame: off=%d size=%d ptrbytes=%d",
				name, o.Name, o.Off, o.Size, o.PtrBytes)
		}
	}
}

// smWordIsAPointer reports whether an item claims the word at off.
func smWordIsAPointer(fr *ssa.Frame, off int64) bool {
	ptrSize := int64(ir.PtrSize)
	for i := range fr.Items {
		it := &fr.Items[i]
		if off < it.Off || off >= it.Off+it.Size {
			continue
		}
		w := (off - it.Off) / ptrSize
		if w/8 < int64(len(it.Type.PtrBits)) && it.Type.PtrBits[w/8]&(1<<uint(w%8)) != 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The encoding, against the spike

// spikeSyms parses the DATA and GLOBL directives of the spike's assembly and
// returns the bytes of each symbol.
//
// The spike is the evidence specs/000-decisions.md decision 3 rests on, and it
// is kept in the repository so that this comparison is possible: the bytes it
// declares are bytes a Go runtime demonstrably read and honoured.
func spikeSyms(t *testing.T) map[string][]byte {
	t.Helper()
	path := filepath.Join("..", "spikes", "stackmap", "spike_arm64.s")
	src, err := os.ReadFile(path)
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and the spike is not there: %v", err)
		}
		t.Skipf("no spike at %s", path)
	}
	data := regexp.MustCompile(`DATA\s+·(\w+)\+(\d+)\(SB\)/(\d+),\s*\$(-?\d+)`)
	globl := regexp.MustCompile(`GLOBL\s+·(\w+)\(SB\),\s*\w+,\s*\$(\d+)`)
	out := make(map[string][]byte)
	for _, mt := range globl.FindAllStringSubmatch(string(src), -1) {
		n, _ := strconv.Atoi(mt[2])
		out[mt[1]] = make([]byte, n)
	}
	for _, mt := range data.FindAllStringSubmatch(string(src), -1) {
		name, b := mt[1], out[mt[1]]
		if b == nil {
			t.Fatalf("the spike declares data for %s and no GLOBL", name)
		}
		off, _ := strconv.Atoi(mt[2])
		width, _ := strconv.Atoi(mt[3])
		val, _ := strconv.ParseInt(mt[4], 10, 64)
		for i := 0; i < width; i++ {
			b[off+i] = byte(uint64(val) >> uint(8*i))
		}
	}
	if len(out) == 0 {
		t.Fatal("the spike declares no symbol")
	}
	return out
}

func TestBitmapSetMatchesTheSpike(t *testing.T) {
	syms := spikeSyms(t)

	// The spike's livemap: one bitmap of three bits with the third set. The
	// GLOBL is twelve bytes and the structure is nine, so the tail is padding
	// that the assembler needed and the encoding does not.
	live := (&ssa.BitmapSet{NBit: 3, Maps: [][]byte{{0x04}}}).Bytes()
	if got, want := syms["livemap"], live; len(got) < len(want) || string(got[:len(want)]) != string(want) {
		t.Errorf("livemap: the encoder produced %x and the spike declares %x", want, got)
	}
	for i, b := range syms["livemap"][len(live):] {
		if b != 0 {
			t.Errorf("livemap: byte %d past the structure is %#x, not padding", len(live)+i, b)
		}
	}

	// The spike's multimap is the decisive one: two bitmaps of three bits, ten
	// bytes in total. That pins the stride at ceil(nbit/8) and not at four,
	// and the spike proved the runtime reads the second bitmap by collecting
	// the object at index 1 and not at index 0.
	multi := (&ssa.BitmapSet{NBit: 3, Maps: [][]byte{{0x04}, {0x00}}}).Bytes()
	if got := syms["multimap"]; string(got) != string(multi) {
		t.Errorf("multimap: the encoder produced %x and the spike declares %x", multi, got)
	}
	if len(multi) != 8+2*1 {
		t.Errorf("multimap is %d bytes; two bitmaps of three bits are eight plus two", len(multi))
	}

	// The bogus map of the spike is the same encoding with the wrong bit, which
	// is worth encoding here too: the difference between a correct map and a
	// corrupting one is one bit and no structural change at all.
	bogus := (&ssa.BitmapSet{NBit: 3, Maps: [][]byte{{0x01}}}).Bytes()
	if got := syms["bogusmap"]; len(got) < len(bogus) || string(got[:len(bogus)]) != string(bogus) {
		t.Errorf("bogusmap: the encoder produced %x and the spike declares %x", bogus, got)
	}
}

func TestBitmapSetShape(t *testing.T) {
	for _, tt := range []struct {
		nbit int32
		maps int
		want int
	}{
		{0, 0, 8}, {0, 3, 8}, {1, 1, 9}, {8, 2, 10}, {9, 2, 12}, {64, 1, 16},
	} {
		s := &ssa.BitmapSet{NBit: tt.nbit}
		for i := 0; i < tt.maps; i++ {
			s.Maps = append(s.Maps, make([]byte, int(tt.nbit+7)/8))
		}
		b := s.Bytes()
		if len(b) != tt.want {
			t.Errorf("nbit=%d n=%d encodes to %d bytes, want %d", tt.nbit, tt.maps, len(b), tt.want)
		}
		if got := int32(b[0]) | int32(b[1])<<8; got != int32(tt.maps) {
			t.Errorf("nbit=%d: the header says n=%d, want %d", tt.nbit, got, tt.maps)
		}
		if got := int32(b[4]) | int32(b[5])<<8; got != tt.nbit {
			t.Errorf("nbit=%d: the header says nbit=%d", tt.nbit, got)
		}
	}
}

// ---------------------------------------------------------------------------
// The maps of a function

// smAcross returns a function that holds a pointer in the frame across a call,
// which is the shape spikes/stackmap runs against a real collector.
func smAcross(name string) (*ssa.Func, *ssa.Value) {
	f, e, mem := smFunc(name)
	p := e.NewValue(0, ssa.OpArg, smPtr)
	call := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
	e.NewValue(0, ssa.OpLoad, smInt, p, call)
	smRet(e, call)
	return f, call
}

func TestStackMapMarksThePointerLiveAcrossACall(t *testing.T) {
	f, _ := smAcross("across")
	fr, m := smMaps(t, f, ssa.FrameConfig{})
	if len(m.Locals.Maps) != 1 {
		t.Fatalf("%d bitmaps for one safepoint", len(m.Locals.Maps))
	}
	if fr.LocalsBits != 1 {
		t.Fatalf("the frame holds one pointer word and the map has %d bits:\n%s", fr.LocalsBits, fr)
	}
	if m.Locals.Maps[0][0] != 1 {
		t.Errorf("the bitmap is %#x and the only pointer word is bit 0", m.Locals.Maps[0][0])
	}
}

func TestStackMapDoesNotMarkANonPointerSlot(t *testing.T) {
	// The failure this rules out is the spike's bogus case: a word that holds
	// an integer, declared as a pointer. The collector follows it and the
	// stack copier rewrites it.
	f, e, mem := smFunc("ints")
	n := e.NewValue(0, ssa.OpArg, smInt)
	call := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
	sum := e.NewValue(0, ssa.OpAdd, smInt, n, n)
	smRet(e, call, sum)

	fr, m := smMaps(t, f, ssa.FrameConfig{})
	if fr.LocalsBits != 0 {
		t.Errorf("the frame holds no pointer and the map has %d bits:\n%s", fr.LocalsBits, fr)
	}
	for i, bm := range m.Locals.Maps {
		for _, b := range bm {
			if b != 0 {
				t.Errorf("bitmap %d is %#x and nothing in the frame holds a pointer", i, bm)
			}
		}
	}
}

func TestStackMapDescribesOnlyThePointerWordsOfAWideSlot(t *testing.T) {
	// A string is two words and the second is a length. specs/027's contract
	// is per word, not per slot.
	f, e, mem := smFunc("wide")
	s := e.NewValue(0, ssa.OpArg, smString)
	call := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
	e.NewValue(0, ssa.OpLoad, smInt, s, call)
	smRet(e, call)

	fr, m := smMaps(t, f, ssa.FrameConfig{})
	if fr.LocalsBits != 2 {
		t.Fatalf("a string is two words and the map has %d bits:\n%s", fr.LocalsBits, fr)
	}
	if m.Locals.Maps[0][0] != 1 {
		t.Errorf("the bitmap is %#x; only the data word of a string is a pointer", m.Locals.Maps[0][0])
	}
}

func TestStackMapSharesOneBitmapBetweenIdenticalSafepoints(t *testing.T) {
	// One bitmap per distinct map, not one per safepoint. The index is what
	// grows, and the runtime reads it through PCDATA_StackMapIndex.
	f, e, mem := smFunc("two")
	p := e.NewValue(0, ssa.OpArg, smPtr)
	c1 := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
	c2 := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, c1)
	e.NewValue(0, ssa.OpLoad, smInt, p, c2)
	smRet(e, c2)

	_, m := smMaps(t, f, ssa.FrameConfig{})
	if len(m.Index) != 2 {
		t.Fatalf("%d safepoints, want two", len(m.Index))
	}
	if len(m.Locals.Maps) != 1 {
		t.Errorf("the two safepoints have the same live set and produced %d bitmaps", len(m.Locals.Maps))
	}
	if m.Index[0] != 0 || m.Index[1] != 0 {
		t.Errorf("the indices are %v, want both zero", m.Index)
	}
}

func TestStackMapDistinguishesTwoSafepoints(t *testing.T) {
	// The spike's multi case: one frame, two bitmaps, the slot live at one
	// index and dead at the other.
	f, e, mem := smFunc("multi")
	p := e.NewValue(0, ssa.OpArg, smPtr)
	c1 := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
	e.NewValue(0, ssa.OpLoad, smInt, p, c1)
	c2 := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, c1)
	smRet(e, c2)

	_, m := smMaps(t, f, ssa.FrameConfig{})
	if len(m.Locals.Maps) != 2 {
		t.Fatalf("%d bitmaps; the pointer is live at the first call and dead at the second:\n%s", len(m.Locals.Maps), m)
	}
	if m.Locals.Maps[m.Index[0]][0] != 1 {
		t.Errorf("the pointer is dead at the call it spans: %x", m.Locals.Maps[m.Index[0]])
	}
	if m.Locals.Maps[m.Index[1]][0] != 0 {
		t.Errorf("the pointer is live at a call after its last use: %x", m.Locals.Maps[m.Index[1]])
	}
}

func TestStackMapArgumentsBitmap(t *testing.T) {
	f, _ := smAcross("args")
	cfg := ssa.FrameConfig{
		ArgsSize: 24,
		Args: []ssa.FrameArg{
			{Name: "n", Off: 0, Type: smInt},
			{Name: "p", Off: 8, Type: smPtr},
			{Name: "m", Off: 16, Type: smInt},
		},
	}
	fr, m := smMaps(t, f, cfg)
	// The map stops after the last word that can hold a pointer, which is the
	// second one here, so the trailing int needs no bit.
	if fr.ArgsBits != 2 {
		t.Fatalf("the arguments map has %d bits, want 2:\n%s", fr.ArgsBits, fr)
	}
	if m.Args.Maps[0][0] != 0x02 {
		t.Errorf("the arguments bitmap is %#x and the pointer is the second word", m.Args.Maps[0][0])
	}
}

func TestStackMapObjectsSymbol(t *testing.T) {
	f, e, mem := smFunc("objects")
	o := &ir.Object{Name: "box", Type: smString, Class: ir.ClassLocal, Addrtaken: true}
	f.Frame = []*ir.Object{o}
	call := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
	smRet(e, call)

	fr, m := smMaps(t, f, ssa.FrameConfig{})
	if len(m.Objects) != 1 {
		t.Fatalf("%d stack objects for one address-taken local", len(m.Objects))
	}
	got := m.Objects[0]
	if got.Size != 16 || got.PtrBytes != 8 {
		t.Errorf("the object is size=%d ptrbytes=%d; a string is sixteen bytes of which eight can hold a pointer",
			got.Size, got.PtrBytes)
	}
	if int64(got.Off) != fr.VarpOff(fr.Items[got.Item].Off) {
		t.Errorf("the object is at %d and the item is at %d from Varp", got.Off, fr.VarpOff(fr.Items[got.Item].Off))
	}

	if _, err := m.ObjectsSym("f.stkobj", nil); err == nil {
		t.Error("a table with no type descriptor to point at was produced")
	}
	ref := obj.SymRef{PkgIdx: 3, SymIdx: 7}
	sym, err := m.ObjectsSym("f.stkobj", func(*ir.Type) (obj.SymRef, bool) { return ref, true })
	if err != nil {
		t.Fatalf("ObjectsSym: %v", err)
	}
	if len(sym.Data) != 8+16 {
		t.Fatalf("the table is %d bytes; a count and one record are 8+16", len(sym.Data))
	}
	if sym.Data[0] != 1 {
		t.Errorf("the table counts %d objects", sym.Data[0])
	}
	if len(sym.Relocs) != 1 || sym.Relocs[0].Off != 8+12 || sym.Relocs[0].Type != obj.R_ADDROFF || sym.Relocs[0].Sym != ref {
		t.Errorf("the type descriptor offset is not relocated: %+v", sym.Relocs)
	}
	if sym.Type != obj.SRODATA {
		t.Errorf("the table is of kind %v, want read-only data", sym.Type)
	}

	// A function with no address-taken local has no table at all, which is
	// what the runtime expects: it reads the funcdata and finds nothing.
	f2, _ := smAcross("nostkobj")
	_, m2 := smMaps(t, f2, ssa.FrameConfig{})
	s2, err := m2.ObjectsSym("g.stkobj", nil)
	if err != nil || s2 != nil {
		t.Errorf("a function with no stack object produced %v, %v", s2, err)
	}
}

func TestStackMapSymbols(t *testing.T) {
	f, _ := smAcross("syms")
	_, m := smMaps(t, f, ssa.FrameConfig{})
	for _, tt := range []struct {
		name string
		sym  *obj.Symbol
	}{
		{"locals", m.LocalsSym("f.gclocals")},
		{"args", m.ArgsSym("f.gcargs")},
	} {
		if tt.sym.Type != obj.SRODATA {
			t.Errorf("%s: kind %v, want read-only data", tt.name, tt.sym.Type)
		}
		if int(tt.sym.Size) != len(tt.sym.Data) {
			t.Errorf("%s: size %d and %d bytes of data", tt.name, tt.sym.Size, len(tt.sym.Data))
		}
		if tt.sym.Align != 4 {
			t.Errorf("%s: aligned to %d, and the header is two int32", tt.name, tt.sym.Align)
		}
	}
	if ssa.PCDataSym("f.pcdata", nil) != nil {
		t.Error("an empty pc-value table produced a symbol")
	}
	s := ssa.PCDataSym("f.pcdata", []byte{1, 2, 3})
	if s == nil || !s.Pcdata || int(s.Size) != 3 {
		t.Errorf("the pc-value symbol is %+v", s)
	}
}

// ---------------------------------------------------------------------------
// The pc-value streams

// smPCValue decodes a pc-value table the way the runtime does.
//
// The stream is pairs of a zig-zag value delta and an unsigned pc delta, and
// each pair says that the value applies up to the new program counter. A zero
// value delta ends the stream, except in the first pair. This is a second
// implementation of the decoder in $GOROOT/src/runtime/symtab.go, which is the
// point: it checks the encoder against the reader rather than against itself.
func smPCValue(data []byte, target int64, minLC int) int32 {
	pc := int64(0)
	val := int32(-1)
	first := true
	for len(data) > 0 {
		v, n := smVarint(data)
		if v == 0 && !first {
			return -1
		}
		data = data[n:]
		val += int32(v)
		u, n := smUvarint(data)
		data = data[n:]
		pc += int64(u) * int64(minLC)
		if target < pc {
			return val
		}
		first = false
	}
	return -1
}

func smVarint(b []byte) (int64, int) {
	u, n := smUvarint(b)
	x := int64(u >> 1)
	if u&1 != 0 {
		x = ^x
	}
	return x, n
}

func smUvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return x, len(b)
}

func TestStackMapPCData(t *testing.T) {
	f, e, mem := smFunc("pcdata")
	p := e.NewValue(0, ssa.OpArg, smPtr)
	c1 := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, mem)
	e.NewValue(0, ssa.OpLoad, smInt, p, c1)
	c2 := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, c1)
	smRet(e, c2)

	_, m := smMaps(t, f, ssa.FrameConfig{})
	pcs := &ssa.PCMap{FuncSize: 64, MinLC: 4, PrologueEnd: 8, EpilogueStart: 56,
		Start: make([]int64, f.NumValues()), End: make([]int64, f.NumValues())}
	for i := range pcs.Start {
		pcs.Start[i] = -1
	}
	pcs.Start[c1.ID], pcs.End[c1.ID] = 16, 20
	pcs.Start[c2.ID], pcs.End[c2.ID] = 32, 36

	data, err := m.StackMapPCData(pcs)
	if err != nil {
		t.Fatalf("StackMapPCData: %v", err)
	}
	if got, want := smPCValue(data, 16, 4), m.Index[0]; got != want {
		t.Errorf("the map at the first call is %d, want %d", got, want)
	}
	if got, want := smPCValue(data, 32, 4), m.Index[1]; got != want {
		t.Errorf("the map at the second call is %d, want %d", got, want)
	}
	if m.Index[0] == m.Index[1] {
		t.Fatalf("the two calls have the same map and the test cannot tell them apart")
	}
	// The value stays in effect until the next change, which is what the
	// runtime reads at a return address.
	if got, want := smPCValue(data, 20, 4), m.Index[0]; got != want {
		t.Errorf("the map just after the first call is %d, want %d", got, want)
	}

	// A safepoint the code generator gave no program counter is a bug in the
	// generator, not something to encode around.
	pcs.Start[c2.ID] = -1
	if _, err := m.StackMapPCData(pcs); err == nil {
		t.Error("a safepoint with no program counter was accepted")
	}
	if _, err := m.StackMapPCData(nil); err == nil {
		t.Error("a nil pc map was accepted")
	}
}

func TestUnsafePointPCData(t *testing.T) {
	f, e, mem := smFunc("unsafe")
	p := e.NewValue(0, ssa.OpArg, smPtr)
	str := e.NewValue(0, ssa.OpArg, smString)
	// A two-word store leaves a pointer half written while it runs.
	st := e.NewValue(0, ssa.OpStore, ssa.MemType, p, str, mem)
	st.AuxInt = smString.Size
	call := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, st)
	smRet(e, call)

	_, m := smMaps(t, f, ssa.FrameConfig{})
	pcs := &ssa.PCMap{FuncSize: 64, MinLC: 4, PrologueEnd: 8, EpilogueStart: 56,
		Start: make([]int64, f.NumValues()), End: make([]int64, f.NumValues())}
	for i := range pcs.Start {
		pcs.Start[i] = -1
	}
	pcs.Start[st.ID], pcs.End[st.ID] = 24, 32
	pcs.Start[call.ID], pcs.End[call.ID] = 36, 40

	// The stack-growth tail of specs/035 is emitted code and no value names
	// it, so the generator states it as a range of its own.
	pcs.Unsafe = []ssa.PCRange{{Lo: 44, Hi: 52}}
	data, err := m.UnsafePointPCData(pcs, ssa.HalfWrittenPointer)
	if err != nil {
		t.Fatalf("UnsafePointPCData: %v", err)
	}
	for _, tt := range []struct {
		pc   int64
		want int32
		why  string
	}{
		{0, ssa.UnsafePointUnsafe, "the prologue has not established the frame"},
		{4, ssa.UnsafePointUnsafe, "the prologue has not established the frame"},
		{8, ssa.UnsafePointSafe, "the frame is established"},
		{24, ssa.UnsafePointUnsafe, "the store is half way through a pointer"},
		{28, ssa.UnsafePointUnsafe, "the store is half way through a pointer"},
		{32, ssa.UnsafePointSafe, "the store is done"},
		{44, ssa.UnsafePointUnsafe, "the stack-growth tail re-executes the function"},
		{48, ssa.UnsafePointUnsafe, "the stack-growth tail re-executes the function"},
		{52, ssa.UnsafePointSafe, "the tail is over"},
		{56, ssa.UnsafePointUnsafe, "the epilogue tore the frame down"},
		{60, ssa.UnsafePointUnsafe, "the epilogue tore the frame down"},
	} {
		if got := smPCValue(data, tt.pc, 4); got != tt.want {
			t.Errorf("at %d the stream says %d, want %d: %s", tt.pc, got, tt.want, tt.why)
		}
	}

	// A function with no unsafe range at all encodes to nothing, and the
	// linker reads an empty table as no information.
	flat := &ssa.PCMap{FuncSize: 16, MinLC: 4, EpilogueStart: 16,
		Start: make([]int64, f.NumValues()), End: make([]int64, f.NumValues())}
	for i := range flat.Start {
		flat.Start[i] = -1
	}
	data, err = m.UnsafePointPCData(flat, nil)
	if err != nil || len(data) != 0 {
		t.Errorf("a function with no unsafe range encoded to %x, %v", data, err)
	}
	if _, err := m.UnsafePointPCData(nil, nil); err == nil {
		t.Error("a nil pc map was accepted")
	}
}

func TestHalfWrittenPointer(t *testing.T) {
	_, e, mem := smFunc("half")
	p := e.NewValue(0, ssa.OpArg, smPtr)
	str := e.NewValue(0, ssa.OpArg, smString)
	one := e.NewValue(0, ssa.OpStore, ssa.MemType, p, p, mem)
	one.AuxInt = 8
	two := e.NewValue(0, ssa.OpStore, ssa.MemType, p, str, one)
	two.AuxInt = 16
	move := e.NewValue(0, ssa.OpMove, ssa.MemType, p, p, two)
	move.AuxInt = 32
	small := e.NewValue(0, ssa.OpMove, ssa.MemType, p, p, move)
	small.AuxInt = 8
	smRet(e, small)

	for _, tt := range []struct {
		v    *ssa.Value
		want bool
	}{
		{one, false}, {two, true}, {move, true}, {small, false}, {p, false}, {nil, false},
	} {
		if got := ssa.HalfWrittenPointer(tt.v); got != tt.want {
			t.Errorf("%v: HalfWrittenPointer is %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestStackMapRejectsBadInput(t *testing.T) {
	if _, err := ssa.BuildStackMaps(nil, nil); err == nil {
		t.Error("a nil liveness was accepted")
	}
	f, _ := smAcross("mismatch")
	if err := ssa.Check(f); err != nil {
		t.Fatalf("the function does not verify: %v", err)
	}
	a, err := ssa.Allocate(f, ssa.NewArm64Target())
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	items, err := ssa.FrameItems(a)
	if err != nil {
		t.Fatalf("FrameItems: %v", err)
	}
	lv, err := ssa.ComputeLiveness(a, items)
	if err != nil {
		t.Fatalf("ComputeLiveness: %v", err)
	}
	fr, err := ssa.LayoutFrame(f, nil, ssa.FrameConfig{})
	if err != nil {
		t.Fatalf("LayoutFrame: %v", err)
	}
	if _, err := ssa.BuildStackMaps(lv, fr); err == nil {
		t.Error("a frame with fewer items than the liveness was accepted")
	}
}

func TestStackMapsAreDeterministic(t *testing.T) {
	first := ""
	for i := 0; i < 4; i++ {
		f, _ := smAcross("determinism")
		_, m := smMaps(t, f, ssa.FrameConfig{})
		got := m.String()
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d differs\ngot:\n%s\nwant:\n%s", i, got, first)
		}
	}
}

// ---------------------------------------------------------------------------
// End to end over the standard library
//
// The pipeline is the real one: the parser of specs/010, the checker of
// specs/012, the IR of specs/020, the construction of specs/021, the lowering
// of specs/025, the allocation of specs/026, and this spec. The assertions are
// the ones that hold for every function: nothing crashes, every map covers the
// frame it describes, and every bit is inside it.

// The parse, check and build harness is this file's own. Another test in this
// package has one of its own shape, and a test package holds one declaration
// per name, so the two cannot be shared without one file owning the other's
// corpus run.

type smCorpusPkg struct {
	pkg   *types2.Package
	files []*syntax.File
	info  *types2.Info
	err   error
}

type smCorpusImporter struct {
	fset *syntax.FileSet
	done map[string]*smCorpusPkg
}

func newSMCorpusImporter() *smCorpusImporter {
	return &smCorpusImporter{fset: syntax.NewFileSet(), done: make(map[string]*smCorpusPkg)}
}

func (imp *smCorpusImporter) Import(path string) (*types2.Package, error) {
	r := imp.check(path)
	return r.pkg, r.err
}

func (imp *smCorpusImporter) check(path string) *smCorpusPkg {
	if have, ok := imp.done[path]; ok {
		if have == nil {
			return &smCorpusPkg{err: fmt.Errorf("import cycle at %s", path)}
		}
		return have
	}
	if path == "unsafe" {
		r := &smCorpusPkg{pkg: types2.Unsafe}
		imp.done[path] = r
		return r
	}
	imp.done[path] = nil
	r := &smCorpusPkg{}
	imp.done[path] = r

	bp, err := build.Import(path, "", 0)
	if err != nil {
		for _, prefix := range []string{"vendor/", "cmd/vendor/"} {
			if bp2, err2 := build.Import(prefix+path, "", 0); err2 == nil {
				bp, err = bp2, nil
				break
			}
		}
	}
	if err != nil {
		r.err = err
		return r
	}
	if len(bp.CgoFiles) > 0 || len(bp.GoFiles) == 0 {
		r.err = fmt.Errorf("%s has no plain Go files", path)
		return r
	}
	for _, name := range bp.GoFiles {
		file, err := syntax.ParseFile(imp.fset, filepath.Join(bp.Dir, name), nil, nil, 0)
		if err != nil || file == nil {
			r.err = fmt.Errorf("parse %s: %v", name, err)
			return r
		}
		r.files = append(r.files, file)
	}
	r.info = &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{Fset: imp.fset, Importer: imp, Sizes: types2.SizesFor("gc", "arm64")}
	r.pkg, r.err = conf.Check(path, r.files, r.info)
	return r
}

type smCounts struct {
	pkgs     int
	funcs    int // functions the typed tree holds
	built    int // functions ssa.Build produced and the verifier accepted
	lowered  int // functions that lowered completely
	mapped   int // functions that reached a stack map
	pointers int // functions with at least one pointer bit set
	objects  int // functions with a stack object
	safe     int // safepoints described

	// notBuilt counts the functions ssa.Build refused, by cause, and verifyNG
	// the ones it accepted and the verifier did not. Until these existed the
	// test returned silently on both, so its numbers could only ever go up
	// and never said what they were measured out of.
	notBuilt map[string]int
	verifyNG int
}

func TestStackMapCorpus(t *testing.T) {
	required := os.Getenv("NANOGO_REQUIRE_CORPUS") == "1"
	src := filepath.Join(runtime.GOROOT(), "src")
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		if required {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s is not there", src)
		}
		t.Skipf("no corpus at %s", src)
	}

	var paths []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != src && (name == "testdata" || name == "vendor" ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(paths)
	if !required {
		// The unattended run takes a sample, as the corpus tests of the passes
		// below this one do. CI sets NANOGO_REQUIRE_CORPUS and gets all of it.
		var library []string
		for _, p := range paths {
			if !strings.HasPrefix(p, "cmd/") && p != "cmd" && p != "unsafe" {
				library = append(library, p)
			}
		}
		const sample = 30
		if len(library) > sample {
			step := len(library) / sample
			var taken []string
			for i := 0; i < len(library); i += step {
				taken = append(taken, library[i])
			}
			library = taken
		}
		paths = library
	}

	imp := newSMCorpusImporter()
	c := &smCounts{notBuilt: make(map[string]int)}
	for _, path := range paths {
		if path == "unsafe" {
			continue
		}
		r := imp.check(path)
		if r.err != nil || r.pkg == nil {
			continue
		}
		pkg, _ := ir.Build(r.pkg, r.files, r.info)
		if pkg == nil {
			continue
		}
		c.pkgs++
		fns := append(append([]*ir.Func{}, pkg.Funcs...), pkg.Inits...)
		for _, fn := range fns {
			c.funcs++
			smCorpusOne(t, path, fn, c)
		}
	}

	t.Logf("stack map corpus: %d packages, %d functions built, %d lowered, %d mapped, %d with a pointer bit, %d with a stack object, %d safepoints",
		c.pkgs, c.built, c.lowered, c.mapped, c.pointers, c.objects, c.safe)
	t.Logf("construction refused %d of the %d functions the typed tree holds, and %d more did not verify",
		c.funcs-c.built-c.verifyNG, c.funcs, c.verifyNG)
	corpusLogCounts(t, "construction refused", c.notBuilt)
	if c.built == 0 {
		t.Fatal("the corpus produced no function")
	}
	if c.mapped == 0 {
		t.Fatal("no function reached a stack map")
	}
	if required && c.mapped < 1000 {
		t.Fatalf("only %d functions reached a stack map; the corpus collapsed", c.mapped)
	}
}

// smCorpusPCData encodes both pc-value streams over a program counter map that
// gives every value one instruction, and reads them back.
//
// The code generator does not exist yet, so the program counters are invented.
// What is not invented is the encoding: obj.EncodePCData rejects an entry that
// is not a multiple of the instruction length or that does not advance, and the
// decoder here is the runtime's. A stream that encodes but decodes to the wrong
// index at a call is the failure this catches.
func smCorpusPCData(t *testing.T, name string, f *ssa.Func, m *ssa.StackMaps) {
	t.Helper()
	const minLC = 4
	pcs := &ssa.PCMap{MinLC: minLC, PrologueEnd: minLC,
		Start: make([]int64, f.NumValues()), End: make([]int64, f.NumValues())}
	for i := range pcs.Start {
		pcs.Start[i] = -1
	}
	pc := int64(minLC)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			pcs.Start[v.ID], pcs.End[v.ID] = pc, pc+minLC
			pc += minLC
		}
	}
	pcs.FuncSize = pc + minLC
	pcs.EpilogueStart = pc

	data, err := m.StackMapPCData(pcs)
	if err != nil {
		t.Fatalf("%s: StackMapPCData: %v", name, err)
	}
	for k, sp := range m.Liveness.Safepoints {
		if got := smPCValue(data, pcs.Start[sp.Value], minLC); got != m.Index[k] {
			t.Fatalf("%s: the stream says map %d at the call at %d and the safepoint has map %d",
				name, got, pcs.Start[sp.Value], m.Index[k])
		}
	}
	unsafeData, err := m.UnsafePointPCData(pcs, ssa.HalfWrittenPointer)
	if err != nil {
		t.Fatalf("%s: UnsafePointPCData: %v", name, err)
	}
	for _, tt := range []struct {
		pc   int64
		want int32
	}{{0, ssa.UnsafePointUnsafe}, {minLC, ssa.UnsafePointSafe}, {pcs.EpilogueStart, ssa.UnsafePointUnsafe}} {
		if got := smPCValue(unsafeData, tt.pc, minLC); got != tt.want {
			t.Fatalf("%s: the unsafe stream says %d at %d, want %d", name, got, tt.pc, tt.want)
		}
	}
}

// smCorpusOne runs one function through the whole pipeline.
func smCorpusOne(t *testing.T, path string, fn *ir.Func, c *smCounts) {
	t.Helper()
	f, err := ssa.Build(fn)
	if err != nil || f == nil {
		c.notBuilt[decBuildCause(err)]++
		return
	}
	if vs := ssa.Verify(f); len(vs) != 0 {
		// The violation itself is specs/021's corpus test to report. That it
		// happened is this one's, because a function that builds and does not
		// verify is not a function this pipeline measured.
		c.verifyNG++
		return
	}
	c.built++
	// The pipeline of specs/002-architecture.md: decomposition, then
	// specs/030-abi.md's assignment, then selection. The assignment is what
	// puts a value the registers cannot hold in the argument area, and where
	// it puts one decides which words the collector scans, so a stack map
	// measured without it is a map of a different program.
	ssa.Decompose(f)
	if err := ssa.AssignABI(f, ssa.NewArm64Target()); err != nil {
		return
	}
	ok := func() (ok bool) {
		defer func() {
			e := recover()
			if e == nil {
				return
			}
			t.Fatalf("%s: %s: lowering panicked rather than returning: %v\n%s", path, fn.Name, e, debug.Stack())
		}()
		err := ssa.Lower(f, rules.ARM64)
		if err == nil {
			return true
		}
		if _, isLower := err.(*ssa.LowerError); !isLower {
			t.Fatalf("%s: %s: lowering returned %T: %v", path, fn.Name, err, err)
		}
		return false
	}()
	if !ok {
		return
	}
	c.lowered++
	ssa.SplitCriticalEdges(f)
	a, err := ssa.Allocate(f, ssa.NewArm64Target())
	if err != nil {
		// specs/026 has its own corpus test and its refusals are its own.
		return
	}
	items, err := ssa.FrameItems(a)
	if err != nil {
		t.Fatalf("%s: %s: FrameItems: %v", path, fn.Name, err)
	}
	lv, err := ssa.ComputeLiveness(a, items)
	if err != nil {
		t.Fatalf("%s: %s: ComputeLiveness: %v", path, fn.Name, err)
	}
	fr, err := ssa.LayoutFrame(f, items, ssa.FrameConfig{SaveRA: true, SaveFP: true})
	if err != nil {
		t.Fatalf("%s: %s: LayoutFrame: %v", path, fn.Name, err)
	}
	m, err := ssa.BuildStackMaps(lv, fr)
	if err != nil {
		t.Fatalf("%s: %s: BuildStackMaps: %v", path, fn.Name, err)
	}
	smCheck(t, path+"."+fn.Name, fr, m)
	smCorpusPCData(t, path+"."+fn.Name, f, m)
	c.mapped++
	c.safe += len(m.Index)
	if len(m.Objects) > 0 {
		c.objects++
	}
	for _, bm := range m.Locals.Maps {
		for _, b := range bm {
			if b != 0 {
				c.pointers++
				return
			}
		}
	}
}
