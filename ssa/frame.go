// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

// The frame layout of specs/027-liveness-and-stackmaps.md.
//
// The layout runs after register allocation, because the spill slots are part
// of it and specs/026-register-allocation.md does not know their offsets. It
// runs before the code generator, because every frame reference the generator
// emits is an offset computed here.
//
// # The picture
//
// Addresses grow upwards. SP is the bottom.
//
//	higher addresses
//	  +-------------------------+
//	  | caller's frame          |
//	  +-------------------------+ <- Argp: the incoming argument area
//	  | return address          |
//	  +-------------------------+
//	  | saved frame pointer     |
//	  +-------------------------+ <- Varp: the top of the locals area
//	  | pointer-holding slots   |   <- described by the locals bitmap
//	  +-------------------------+ <- PtrBase
//	  | pointer-free slots      |
//	  +-------------------------+ <- LocalsBase
//	  | outgoing arguments      |   <- described by the callee's arguments map
//	  +-------------------------+ <- SP
//	lower addresses
//
// # Why the pointer group sits directly below Varp
//
// The spec calls grouping the pointer-holding slots a size optimisation with a
// correctness benefit. It is more than that: the runtime scans the block
//
//	[Varp - nbit*PtrSize, Varp)
//
// where nbit is the bit count in the locals bitmap, so the anchor of bit 0 is
// not a choice this file makes. A pointer-holding slot placed below a
// pointer-free one would either be outside the scanned block or would force
// nbit to cover the pointer-free slots as well. The group is therefore against
// Varp, and bit i covers the word at PtrBase + i*PtrSize.
//
// # What is not decided here
//
// The incoming and outgoing argument areas are the calling convention's, and
// abi.go assigns them. They arrive through FrameConfig rather than being
// recomputed here: an argument area whose contents this file guessed would
// produce an arguments bitmap that describes words the convention put
// somewhere else, which is the failure the whole spec is about.
//
// Where the saved return address and the saved frame pointer physically land
// is also the target's, and the diagram of
// specs/027-liveness-and-stackmaps.md is not every target's answer.
//
// On arm64 the link register is saved at the bottom of the frame, at 0(RSP),
// inside OutArgs, so SaveRA is false. **SaveFP is true.** The word at the top
// of the frame holds the *caller's* saved frame pointer, and the runtime's
// traceback computes
//
//	varp = fp - PtrSize   (for a non-empty frame)
//	argp = fp + MinFrameSize
//
// so Varp sits one word below the top. An earlier version of this comment, and
// of specs/027, said an arm64 caller sets both false. That gives Varp == Size,
// which places a local on top of the caller's saved frame pointer and puts
// Varp one word above where the runtime looks. The stack map then describes
// the wrong words, and nothing reports it: the map is well formed, the
// collector reads it, and it scans the wrong slots.
//
// specs/043-amd64-backend.md is the other case: the call instruction pushes
// the return address above the frame, and the runtime moves Varp down by one
// word for it. The two words are optional here for that reason.

import (
	"fmt"
	"sort"
	"strings"

	"golang.design/x/nanogo/ir"
)

// ItemKind is where a frame item came from.
type ItemKind uint8

const (
	// ItemSpill is a spill slot, from Alloc.Slots.
	ItemSpill ItemKind = iota
	// ItemObject is an object that lives in the frame for reasons of its own,
	// from Func.Frame: its address is taken, or its type does not fit in one
	// value.
	ItemObject
)

var itemKindNames = [...]string{ItemSpill: "spill", ItemObject: "object"}

func (k ItemKind) String() string {
	if int(k) < len(itemKindNames) {
		return itemKindNames[k]
	}
	return "itemkind(?)"
}

// FrameItem is one addressable thing in the locals area.
//
// It is the unit both the liveness analysis and the stack map builder work
// over. The two sources are kept apart by Kind, because they are described to
// the collector by different mechanisms and their lifetimes are computed by
// different analyses. See liveness.go.
type FrameItem struct {
	Kind  ItemKind
	Index int32 // into Alloc.Slots for ItemSpill, into Func.Frame for ItemObject
	Name  string

	// Type is what decides which words of the item hold pointers. Nothing
	// else may decide it: ir.Type.PtrBits is computed once, by ir.Layout, and
	// the bitmap the collector reads has to be the one the code generator
	// laid out. See specs/020-ir.md.
	Type  *ir.Type
	Size  int64
	Align int64

	// Obj is the object of an ItemObject.
	Obj *ir.Object

	// StackObject marks an item the collector needs the extent and the type
	// of, not only a pointer map: a local it can reach through a pointer
	// rather than through the locals bitmap.
	// specs/027-liveness-and-stackmaps.md describes these through
	// FUNCDATA_StackObjects, and ssa/liveness.go stops describing such a local
	// in the bitmap below the last address taken of it, so the table is what
	// covers it there.
	//
	// Every frame object qualifies, not only an address-taken one. A frame
	// object that no source expression took the address of is in the frame
	// because its type does not fit one value, and the code that reads it
	// still reaches it through an address. The reference compiler has no such
	// object: it keeps the value in SSA and lists only the address-taken ones.
	//
	// A local that holds no pointer is not one. The table exists so that the
	// collector can scan an object it reached through a pointer, and there is
	// nothing in such an object to scan. A zero-sized local shows why that is
	// more than an optimisation: its offset from Varp would be zero, and the
	// runtime reads a non-negative offset as one into the incoming argument
	// area.
	StackObject bool

	// Off is the offset of the item from SP. LayoutFrame sets it.
	Off int64
}

// HasPointers reports whether any word of the item may hold a pointer.
func (it *FrameItem) HasPointers() bool { return it.Type != nil && it.Type.HasPointers() }

// FrameItems returns every item of the locals area, in a fixed order: the
// spill slots in slot order, then the frame objects in declaration order.
//
// The order is the identity of an item. Liveness, the layout and the stack
// maps all index the same slice, so nothing has to be matched up by name or by
// address, which is what specs/053-determinism.md needs.
func FrameItems(a *Alloc) ([]FrameItem, error) {
	if a == nil || a.Func == nil {
		return nil, fmt.Errorf("ssa: frame: nil allocation")
	}
	f := a.Func
	values := valuesByID(f)
	items := make([]FrameItem, 0, len(a.Slots)+len(f.Frame))
	for i := range a.Slots {
		s := &a.Slots[i]
		if len(s.Values) == 0 {
			return nil, fmt.Errorf("ssa: frame: %s: slot %d holds no value", f.Name, i)
		}
		var t *ir.Type
		for _, id := range s.Values {
			v := valueAt(values, id)
			if v == nil || v.Type == nil {
				return nil, fmt.Errorf("ssa: frame: %s: slot %d holds v%d, which has no type", f.Name, i, id)
			}
			if t == nil {
				t = v.Type
				continue
			}
			// specs/026-register-allocation.md requires one layout per slot,
			// and the reason is here: the bits of this slot are read from one
			// type. Two types in one slot would make the bitmap describe the
			// slot correctly for one of them and wrongly for the other.
			if v.Type.Size != t.Size || v.Type.Align != t.Align || !samePtrBits(v.Type, t) {
				return nil, fmt.Errorf("ssa: frame: %s: slot %d holds %v and %v, which have different layouts",
					f.Name, i, t, v.Type)
			}
		}
		items = append(items, FrameItem{
			Kind:  ItemSpill,
			Index: int32(i),
			Name:  fmt.Sprintf("spill%d", i),
			Type:  t,
			Size:  s.Size,
			Align: s.Align,
		})
	}
	for i, o := range f.Frame {
		if o == nil || o.Type == nil {
			return nil, fmt.Errorf("ssa: frame: %s: frame object %d has no type", f.Name, i)
		}
		items = append(items, FrameItem{
			Kind:        ItemObject,
			Index:       int32(i),
			Name:        o.Name,
			Type:        o.Type,
			Size:        o.Type.Size,
			Align:       o.Type.Align,
			Obj:         o,
			StackObject: o.Type.HasPointers(),
		})
	}
	for i := range items {
		if items[i].Align <= 0 {
			items[i].Align = 1
		}
		if items[i].Size < 0 {
			return nil, fmt.Errorf("ssa: frame: %s: %s has size %d", f.Name, items[i].Name, items[i].Size)
		}
	}
	return items, nil
}

// valuesByID returns the values of f indexed by identifier.
//
// A slice indexed by a dense identifier rather than a map keyed by pointer,
// per specs/053-determinism.md. Holes are nil: an identifier belongs to a
// value that was deleted, or to a block that is gone.
func valuesByID(f *Func) []*Value {
	out := make([]*Value, f.NumValues())
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if int(v.ID) < len(out) {
				out[v.ID] = v
			}
		}
	}
	return out
}

func valueAt(values []*Value, id ID) *Value {
	if id < 0 || int(id) >= len(values) {
		return nil
	}
	return values[id]
}

// FrameArg is one word range of the incoming argument area.
//
// Off is measured from Argp, so it is not negative, and it is the offset the
// calling convention assigned. specs/030-abi.md owns that assignment.
type FrameArg struct {
	Name string
	Off  int64
	Type *ir.Type

	// Spill marks an argument that travels in registers.
	//
	// The convention reserves its words in the argument area all the same, and
	// the only code that writes them is the stack-growth tail: it saves the
	// argument registers there, calls the runtime, and reads them back before
	// re-entering the function
	// (specs/035-goroutines-and-stack-growth.md). Nothing on the ordinary path
	// writes them, so at every safepoint of the body those words hold whatever
	// the caller's frame left there, and the arguments bitmap must not claim
	// they hold a pointer. The bitmap in effect inside the tail does, because
	// there they hold the arguments and the stack copier has to move them.
	Spill bool
}

// FrameConfig is what the layout needs and cannot derive.
type FrameConfig struct {
	// Grows reports that the function emits a stack-growth tail.
	//
	// The layout does not read it. The stack maps do: the tail is where the
	// arguments that travel in registers occupy their reserved words of the
	// argument area, so it is the one place the arguments bitmap describes
	// them, and a function without a tail needs no such bitmap.
	Grows bool

	// PtrSize is the width of a pointer. Zero means ir.PtrSize.
	PtrSize int64

	// Align is the alignment of the whole frame. Zero means 2*PtrSize, which
	// is what both targets of specs/000-decisions.md decision 9 require of SP.
	Align int64

	// SaveRA and SaveFP reserve the two words above the locals area for the
	// return address and the frame pointer. A target that keeps the return
	// address in the caller's frame sets SaveRA false.
	SaveRA bool
	SaveFP bool

	// OutArgs is the size of the outgoing argument area, at the bottom of the
	// frame. It is the largest argument area any call this function makes
	// needs, and the callee's arguments bitmap describes it.
	OutArgs int64

	// Args describes the incoming argument area. ArgsSize is its size.
	//
	// An area with a size and no description is rejected: the arguments
	// bitmap would then say that an area holding pointers holds none, which
	// is the error that frees a live object.
	ArgsSize int64
	Args     []FrameArg
}

func (c FrameConfig) ptrSize() int64 {
	if c.PtrSize <= 0 {
		return ir.PtrSize
	}
	return c.PtrSize
}

func (c FrameConfig) align() int64 {
	if c.Align <= 0 {
		return 2 * c.ptrSize()
	}
	return c.Align
}

// Frame is the laid out frame of one function.
type Frame struct {
	Config FrameConfig
	Func   *Func

	// Items is FrameItems with Off filled in.
	Items []FrameItem

	OutArgs    int64 // the outgoing argument area is [0, OutArgs)
	LocalsBase int64 // the locals area is [LocalsBase, Varp)
	PtrBase    int64 // the pointer group is [PtrBase, Varp)
	Varp       int64 // the top of the locals area
	Size       int64 // the whole frame, measured from SP
	Argp       int64 // the incoming argument area starts here

	// LocalsBits and ArgsBits are the nbit fields of the two bitmaps.
	LocalsBits int32
	ArgsBits   int32
}

// LayoutFrame assigns an offset to every item and returns the frame.
//
// Items are placed in the order FrameItems produced, the pointer-free ones
// first and the pointer-holding ones after them, so two layouts of one
// function are the same layout.
func LayoutFrame(f *Func, items []FrameItem, cfg FrameConfig) (*Frame, error) {
	ptrSize := cfg.ptrSize()
	if cfg.OutArgs < 0 || cfg.ArgsSize < 0 {
		return nil, fmt.Errorf("ssa: frame: negative argument area: out=%d in=%d", cfg.OutArgs, cfg.ArgsSize)
	}
	if cfg.ArgsSize > 0 && len(cfg.Args) == 0 {
		return nil, fmt.Errorf("ssa: frame: an incoming argument area of %d bytes with no description would be described as holding no pointer", cfg.ArgsSize)
	}
	for i := range items {
		if items[i].Type == nil {
			return nil, fmt.Errorf("ssa: frame: item %d (%s) has no type", i, items[i].Name)
		}
		if items[i].HasPointers() && items[i].Align%ptrSize != 0 {
			// A bit index is (Off - PtrBase)/PtrSize and has to be exact.
			return nil, fmt.Errorf("ssa: frame: %s holds a pointer and is aligned to %d, not to %d",
				items[i].Name, items[i].Align, ptrSize)
		}
	}

	fr := &Frame{Config: cfg, Func: f, Items: append([]FrameItem(nil), items...), OutArgs: cfg.OutArgs}

	// place assigns the offsets with the locals area starting at base and
	// returns the size of the whole frame. It runs twice: once to measure, and
	// once with the padding that frame alignment needs folded into base, so
	// that the padding sits below the locals rather than between the saved
	// return address and the incoming arguments, where it would move Argp.
	above := int64(0)
	if cfg.SaveFP {
		above += ptrSize
	}
	if cfg.SaveRA {
		above += ptrSize
	}
	place := func(base int64) int64 {
		off := base
		fr.LocalsBase = base
		for i := range fr.Items {
			if fr.Items[i].HasPointers() {
				continue
			}
			off = roundUpTo(off, fr.Items[i].Align)
			fr.Items[i].Off = off
			off += fr.Items[i].Size
		}
		off = roundUpTo(off, ptrSize)
		fr.PtrBase = off
		for i := range fr.Items {
			if !fr.Items[i].HasPointers() {
				continue
			}
			off = roundUpTo(off, fr.Items[i].Align)
			fr.Items[i].Off = off
			off += fr.Items[i].Size
		}
		fr.Varp = roundUpTo(off, ptrSize)
		return fr.Varp + above
	}
	natural := place(roundUpTo(cfg.OutArgs, ptrSize))
	pad := roundUpTo(natural, cfg.align()) - natural
	fr.Size = place(roundUpTo(cfg.OutArgs, ptrSize) + pad)
	fr.Argp = fr.Size

	fr.LocalsBits = int32((fr.Varp - fr.PtrBase) / ptrSize)
	// The arguments bitmap covers the words up to the last one that can hold a
	// pointer, which is what the reference compiler sizes it to. Padding past
	// it describes nothing and costs a byte per bitmap per safepoint.
	var argPtr int64
	for _, a := range cfg.Args {
		if a.Type == nil {
			return nil, fmt.Errorf("ssa: frame: incoming argument %s has no type", a.Name)
		}
		if a.Off < 0 || (a.Type.HasPointers() && a.Off%ptrSize != 0) {
			return nil, fmt.Errorf("ssa: frame: incoming argument %s is at %d, which cannot hold a pointer word", a.Name, a.Off)
		}
		if end := a.Off + a.Type.PtrBytes(); end > argPtr {
			argPtr = end
		}
	}
	fr.ArgsBits = int32(argPtr / ptrSize)
	return fr, nil
}

// LocalBit returns the bit of the locals bitmap that covers the word at off,
// which is measured from SP.
func (fr *Frame) LocalBit(off int64) (int32, bool) {
	ptrSize := fr.Config.ptrSize()
	if off < fr.PtrBase || off >= fr.Varp || (off-fr.PtrBase)%ptrSize != 0 {
		return 0, false
	}
	return int32((off - fr.PtrBase) / ptrSize), true
}

// ArgBit returns the bit of the arguments bitmap that covers the word at off,
// which is measured from Argp.
func (fr *Frame) ArgBit(off int64) (int32, bool) {
	ptrSize := fr.Config.ptrSize()
	if off < 0 || off%ptrSize != 0 {
		return 0, false
	}
	b := int32(off / ptrSize)
	if b >= fr.ArgsBits {
		return 0, false
	}
	return b, true
}

// VarpOff returns an offset from SP as an offset from Varp.
//
// The stack object table uses it: an entry in the locals area is negative from
// Varp, and one in the incoming argument area is not negative from Argp, and
// the sign is how the runtime tells the two apart.
func (fr *Frame) VarpOff(off int64) int64 { return off - fr.Varp }

// Item returns the item of kind k with index i, or nil.
func (fr *Frame) Item(k ItemKind, i int32) *FrameItem {
	for j := range fr.Items {
		if fr.Items[j].Kind == k && fr.Items[j].Index == i {
			return &fr.Items[j]
		}
	}
	return nil
}

// StackObjects returns the positions of the items the collector needs a type
// for, lowest address first.
//
// The order is the reference compiler's, and the runtime walks the table
// expecting it, so it is part of the format rather than a presentation choice.
func (fr *Frame) StackObjects() []int {
	var out []int
	for i := range fr.Items {
		if fr.Items[i].StackObject {
			out = append(out, i)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return fr.Items[out[i]].Off < fr.Items[out[j]].Off })
	return out
}

// String returns a dump of the layout. Nothing in it derives from a map or an
// address, so two layouts of one function print the same bytes.
func (fr *Frame) String() string {
	var b strings.Builder
	name := "?"
	if fr.Func != nil {
		name = fr.Func.Name
	}
	fmt.Fprintf(&b, "frame %s: size=%d outargs=%d locals=[%d,%d) ptr=[%d,%d) varp=%d argp=%d nbit=%d/%d\n",
		name, fr.Size, fr.OutArgs, fr.LocalsBase, fr.Varp, fr.PtrBase, fr.Varp, fr.Varp, fr.Argp,
		fr.LocalsBits, fr.ArgsBits)
	for i := range fr.Items {
		it := &fr.Items[i]
		fmt.Fprintf(&b, "  %d: %v %s off=%d size=%d align=%d ptr=%v", i, it.Kind, it.Name, it.Off, it.Size, it.Align, it.HasPointers())
		if it.StackObject {
			b.WriteString(" stackobject")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func roundUpTo(n, align int64) int64 {
	if align <= 1 {
		return n
	}
	return (n + align - 1) / align * align
}
