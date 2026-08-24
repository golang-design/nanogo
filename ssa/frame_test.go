// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
)

// The layout is tested as a set of properties rather than as a table of
// offsets, because the offsets are not the contract. What the collector and
// the stack copier depend on is that the pointer-holding slots form one run
// directly below Varp, that every slot is aligned, and that the outgoing
// argument area is at the bottom. A table of offsets would pass while any of
// those broke.

// fiPtr and friends build items without an allocation behind them, which is
// what lets a property test cover shapes the allocator does not produce today.
func fiItem(name string, t *ir.Type, kind ItemKind, index int32) FrameItem {
	return FrameItem{Kind: kind, Index: index, Name: name, Type: t, Size: t.Size, Align: t.Align}
}

func TestFrameItemsFromAnAllocation(t *testing.T) {
	f, e, mem := raFunc("items")
	o := obj("box", tString, ir.ClassLocal)
	o.Addrtaken = true
	flat := obj("flat", tStruct, ir.ClassLocal)
	flat.Addrtaken = true
	f.Frame = []*ir.Object{o, flat}
	p := e.NewValue(0, OpArg, tIntPtr)
	m2 := raCall(e, mem)
	e.NewValue(0, OpLoad, tInt, p, m2)
	raRet(e, m2)

	a := raAllocate(t, f, raTarget())
	items, err := FrameItems(a)
	if err != nil {
		t.Fatalf("FrameItems: %v", err)
	}
	if len(items) != len(a.Slots)+2 {
		t.Fatalf("%d items for %d slots and two frame objects", len(items), len(a.Slots))
	}
	// The spill slots come first and the frame objects after them, which is
	// the order every other file indexes by.
	for i := range items {
		want := ItemSpill
		if i >= len(a.Slots) {
			want = ItemObject
		}
		if items[i].Kind != want {
			t.Errorf("item %d is %v, want %v", i, items[i].Kind, want)
		}
	}
	box := items[len(items)-2]
	if box.Obj != o || !box.StackObject {
		t.Errorf("the address-taken local is not a stack object: %+v", box)
	}
	if box.Type != tString {
		t.Errorf("the object item carries type %v, want %v", box.Type, tString)
	}
	// An address-taken local that holds no pointer is not a stack object. The
	// table is what lets the collector scan an object it reached, and there is
	// nothing in this one to scan.
	if last := items[len(items)-1]; last.Obj != flat || last.StackObject {
		t.Errorf("a pointer-free local is a stack object: %+v", last)
	}
}

func TestFrameItemsRejectASlotWithTwoLayouts(t *testing.T) {
	// specs/026-register-allocation.md gives two values one slot only when
	// their layouts agree, because the bitmap reads one type per slot. This is
	// the assertion, not the assumption.
	f, e, mem := raFunc("mixed")
	p := e.NewValue(0, OpArg, tIntPtr)
	n := e.NewValue(0, OpArg, tInt)
	raRet(e, mem, p, n)
	a := raAllocate(t, f, raTarget())
	a.Slots = []Slot{{Size: 8, Align: 8, Ptr: true, Values: []ID{p.ID, n.ID}}}
	if _, err := FrameItems(a); err == nil {
		t.Error("a slot holding a pointer and an int was accepted")
	} else if !strings.Contains(err.Error(), "different layouts") {
		t.Errorf("the error does not name the reason: %v", err)
	}

	a.Slots = []Slot{{Size: 8, Align: 8, Values: nil}}
	if _, err := FrameItems(a); err == nil {
		t.Error("an empty slot was accepted")
	}
	a.Slots = []Slot{{Size: 8, Align: 8, Values: []ID{ID(f.NumValues() + 3)}}}
	if _, err := FrameItems(a); err == nil {
		t.Error("a slot holding a value that does not exist was accepted")
	}
	if _, err := FrameItems(nil); err == nil {
		t.Error("a nil allocation was accepted")
	}
}

// frameCorpus is the set of item lists the property test runs over. The shapes
// are the ones a real frame holds: words, small scalars, aggregates with a
// pointer in the middle, and pointer-free aggregates.
func frameCorpus() [][]FrameItem {
	tPtrPtr := mkType(&ir.Type{Kind: ir.Ptr, Elem: tIntPtr})
	tMixed := mkType(&ir.Type{Kind: ir.Struct, Name: "mixed", Fields: []ir.Field{
		{Name: "n", Type: tInt}, {Name: "p", Type: tIntPtr}, {Name: "m", Type: tInt},
	}})
	tPtrArr := mkType(&ir.Type{Kind: ir.Array, Elem: tIntPtr, Len: 3})
	types := []*ir.Type{tInt, tBool, tByte, tFloat, tIntPtr, tString, tSlice, tStruct, tArr4, tMixed, tPtrArr, tPtrPtr}

	var out [][]FrameItem
	out = append(out, nil)
	for _, t := range types {
		out = append(out, []FrameItem{fiItem("only", t, ItemSpill, 0)})
	}
	// Every ordered pair, so a pointer item before a pointer-free one is
	// covered as well as the other way round.
	for i, a := range types {
		for j, b := range types {
			out = append(out, []FrameItem{
				fiItem("a", a, ItemSpill, int32(i)),
				fiItem("b", b, ItemObject, int32(j)),
			})
		}
	}
	all := make([]FrameItem, 0, len(types))
	for i, t := range types {
		all = append(all, fiItem("x", t, ItemSpill, int32(i)))
	}
	out = append(out, all)
	return out
}

func TestFrameLayoutProperties(t *testing.T) {
	cfgs := []FrameConfig{
		{},
		{SaveRA: true, SaveFP: true},
		{OutArgs: 8},
		{OutArgs: 24, SaveRA: true},
		{OutArgs: 4, SaveRA: true, SaveFP: true, Align: 16},
		{ArgsSize: 16, Args: []FrameArg{{Name: "p", Off: 0, Type: tIntPtr}, {Name: "n", Off: 8, Type: tInt}}},
	}
	ptrSize := int64(ir.PtrSize)
	for _, items := range frameCorpus() {
		for _, cfg := range cfgs {
			fr, err := LayoutFrame(nil, items, cfg)
			if err != nil {
				t.Fatalf("LayoutFrame(%d items, %+v): %v", len(items), cfg, err)
			}
			if fr.LocalsBase < cfg.OutArgs {
				t.Fatalf("the locals start at %d and the outgoing arguments need %d", fr.LocalsBase, cfg.OutArgs)
			}
			if fr.PtrBase%ptrSize != 0 || fr.Varp%ptrSize != 0 {
				t.Fatalf("the pointer area [%d,%d) is not aligned to %d", fr.PtrBase, fr.Varp, ptrSize)
			}
			if want := int32((fr.Varp - fr.PtrBase) / ptrSize); fr.LocalsBits != want {
				t.Fatalf("the locals map has %d bits and the pointer area holds %d words", fr.LocalsBits, want)
			}
			if fr.Size%cfg.align() != 0 {
				t.Fatalf("the frame is %d bytes and has to be a multiple of %d", fr.Size, cfg.align())
			}
			if fr.Argp != fr.Size {
				t.Fatalf("the incoming arguments start at %d and the frame is %d", fr.Argp, fr.Size)
			}
			// No two items overlap, every item is aligned, the pointer-holding
			// ones are exactly the pointer area and the pointer-free ones are
			// below it.
			for i := range fr.Items {
				it := &fr.Items[i]
				if it.Off%it.Align != 0 {
					t.Fatalf("%s is at %d and is aligned to %d", it.Name, it.Off, it.Align)
				}
				if it.Off < cfg.OutArgs {
					t.Fatalf("%s is at %d, inside the outgoing argument area of %d bytes", it.Name, it.Off, cfg.OutArgs)
				}
				if it.HasPointers() {
					if it.Off < fr.PtrBase || it.Off+it.Size > fr.Varp {
						t.Fatalf("%s holds a pointer and is at [%d,%d), outside the pointer area [%d,%d)",
							it.Name, it.Off, it.Off+it.Size, fr.PtrBase, fr.Varp)
					}
				} else if it.Size > 0 && it.Off+it.Size > fr.PtrBase {
					t.Fatalf("%s holds no pointer and is at [%d,%d), inside the pointer area that starts at %d",
						it.Name, it.Off, it.Off+it.Size, fr.PtrBase)
				}
				for j := range fr.Items {
					if i == j || fr.Items[j].Size == 0 || it.Size == 0 {
						continue
					}
					o := &fr.Items[j]
					if it.Off < o.Off+o.Size && o.Off < it.Off+it.Size {
						t.Fatalf("%s at [%d,%d) overlaps %s at [%d,%d)",
							it.Name, it.Off, it.Off+it.Size, o.Name, o.Off, o.Off+o.Size)
					}
				}
				// Every pointer word of every item has a bit, and it is inside
				// the map. This is the property the stack map builder relies on
				// and the one that turns a layout mistake into a wrong bitmap.
				for w := int64(0); w*ptrSize < it.Size; w++ {
					if !ptrBitSet(it.Type.PtrBits, w) {
						continue
					}
					b, ok := fr.LocalBit(it.Off + w*ptrSize)
					if !ok || b < 0 || b >= fr.LocalsBits {
						t.Fatalf("%s word %d at %d has bit %d of %d, ok=%v",
							it.Name, w, it.Off+w*ptrSize, b, fr.LocalsBits, ok)
					}
				}
			}
		}
	}
}

func TestFrameLayoutIsDeterministic(t *testing.T) {
	items := frameCorpus()[len(frameCorpus())-1]
	cfg := FrameConfig{OutArgs: 16, SaveRA: true, SaveFP: true}
	first := ""
	for i := 0; i < 4; i++ {
		fr, err := LayoutFrame(nil, items, cfg)
		if err != nil {
			t.Fatalf("LayoutFrame: %v", err)
		}
		got := fr.String()
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d differs\ngot:\n%s\nwant:\n%s", i, got, first)
		}
	}
}

func TestFrameLayoutPadsBelowTheLocals(t *testing.T) {
	// The frame is a multiple of the stack alignment and the padding goes
	// under the locals, so that Varp and the incoming arguments keep the
	// distance the calling convention gives them.
	items := []FrameItem{fiItem("p", tIntPtr, ItemSpill, 0)}
	fr, err := LayoutFrame(nil, items, FrameConfig{Align: 16})
	if err != nil {
		t.Fatalf("LayoutFrame: %v", err)
	}
	if fr.Size != 16 {
		t.Fatalf("a frame with one pointer word is %d bytes, want 16", fr.Size)
	}
	if fr.Varp != fr.Size {
		t.Errorf("Varp is %d and the frame ends at %d, and nothing sits above the locals here", fr.Varp, fr.Size)
	}
	if fr.Items[0].Off != 8 {
		t.Errorf("the item is at %d; the padding belongs below it", fr.Items[0].Off)
	}
	if _, ok := fr.LocalBit(fr.Items[0].Off); !ok {
		t.Error("the item has no bit")
	}
}

func TestFrameLayoutRejects(t *testing.T) {
	tBad := &ir.Type{Kind: ir.Ptr, Size: 8, Align: 4, PtrBits: []byte{1}}
	tests := []struct {
		name  string
		items []FrameItem
		cfg   FrameConfig
		want  string
	}{
		{"no type", []FrameItem{{Name: "x"}}, FrameConfig{}, "no type"},
		{"pointer aligned to less than a word",
			[]FrameItem{{Name: "x", Type: tBad, Size: 8, Align: 4}}, FrameConfig{}, "aligned"},
		{"negative outgoing area", nil, FrameConfig{OutArgs: -8}, "negative"},
		{"arguments with no description", nil, FrameConfig{ArgsSize: 16}, "no description"},
		{"argument with no type", nil,
			FrameConfig{ArgsSize: 8, Args: []FrameArg{{Name: "a"}}}, "no type"},
		{"pointer argument off a word boundary", nil,
			FrameConfig{ArgsSize: 8, Args: []FrameArg{{Name: "a", Off: 4, Type: tIntPtr}}}, "pointer word"},
	}
	for _, tt := range tests {
		_, err := LayoutFrame(nil, tt.items, tt.cfg)
		if err == nil {
			t.Errorf("%s: accepted", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: the error does not say %q: %v", tt.name, tt.want, err)
		}
	}
}

func TestFrameStackObjectsAreOrderedByAddress(t *testing.T) {
	a := fiItem("a", tIntPtr, ItemObject, 0)
	a.StackObject = true
	b := fiItem("b", tStruct, ItemObject, 1)
	b.StackObject = true
	c := fiItem("c", tInt, ItemObject, 2)
	fr, err := LayoutFrame(nil, []FrameItem{a, b, c}, FrameConfig{})
	if err != nil {
		t.Fatalf("LayoutFrame: %v", err)
	}
	objs := fr.StackObjects()
	if len(objs) != 2 {
		t.Fatalf("%d stack objects, want the two that are address taken", len(objs))
	}
	if fr.Items[objs[0]].Off > fr.Items[objs[1]].Off {
		t.Errorf("the table is not ordered by address: %d then %d",
			fr.Items[objs[0]].Off, fr.Items[objs[1]].Off)
	}
	// A local is described by a negative offset from Varp, which is how the
	// runtime tells it from an incoming argument.
	for _, i := range objs {
		if fr.VarpOff(fr.Items[i].Off) >= 0 {
			t.Errorf("%s is at %d from Varp, and a local has to be below it",
				fr.Items[i].Name, fr.VarpOff(fr.Items[i].Off))
		}
	}
}

func TestFrameBitsOutsideTheMap(t *testing.T) {
	fr, err := LayoutFrame(nil, []FrameItem{fiItem("p", tIntPtr, ItemSpill, 0)},
		FrameConfig{OutArgs: 16, ArgsSize: 8, Args: []FrameArg{{Name: "a", Off: 0, Type: tIntPtr}}})
	if err != nil {
		t.Fatalf("LayoutFrame: %v", err)
	}
	for _, off := range []int64{fr.PtrBase - 8, fr.Varp, fr.PtrBase + 4} {
		if _, ok := fr.LocalBit(off); ok {
			t.Errorf("%d is outside the pointer area [%d,%d) and has a bit", off, fr.PtrBase, fr.Varp)
		}
	}
	if b, ok := fr.LocalBit(fr.PtrBase); !ok || b != 0 {
		t.Errorf("the first word of the pointer area has bit %d, ok=%v", b, ok)
	}
	if b, ok := fr.ArgBit(0); !ok || b != 0 {
		t.Errorf("the first argument word has bit %d, ok=%v", b, ok)
	}
	for _, off := range []int64{-8, 4, 8} {
		if _, ok := fr.ArgBit(off); ok {
			t.Errorf("%d has an argument bit and the map holds %d bits", off, fr.ArgsBits)
		}
	}
	if fr.Item(ItemSpill, 0) == nil {
		t.Error("the item cannot be found by kind and index")
	}
	if fr.Item(ItemObject, 0) != nil {
		t.Error("an item that does not exist was found")
	}
	if !strings.Contains(fr.String(), "spill p off=") {
		t.Errorf("the dump does not name the item:\n%s", fr)
	}
}
