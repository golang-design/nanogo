// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"encoding/binary"
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
)

// The layout of internal/abi.MapType after the common Type, on a 64-bit
// target.
//
//	type MapType struct {
//	    Type
//	    Key   *Type
//	    Elem  *Type
//	    Group *Type // the slot group, a struct the compiler synthesises
//	    Hasher    func(unsafe.Pointer, uintptr) uintptr
//	    GroupSize uintptr // == Group.Size_
//	    KeysOff    uintptr
//	    KeyStride  uintptr
//	    ElemsOff   uintptr
//	    ElemStride uintptr
//	    ElemOff    uintptr
//	    Flags      uint32
//	}
//
// Go's map is a swiss table from 1.24 on. It is not the bucket map the older
// runtime had, and this header is not that one's: there is no bucket size, no
// key or elem offset within a bucket, and no overflow pointer.
const (
	mapOffKey        = 0
	mapOffElem       = 8
	mapOffGroup      = 16
	mapOffHasher     = 24
	mapOffGroupSize  = 32
	mapOffKeysOff    = 40
	mapOffKeyStride  = 48
	mapOffElemsOff   = 56
	mapOffElemStride = 64
	mapOffElemOff    = 72
	mapOffFlags      = 80
	mapTailSize      = 88
)

// The Flags bits, from internal/abi.
const (
	mapNeedKeyUpdate  = 1 << 0
	mapHashMightPanic = 1 << 1
	mapIndirectKey    = 1 << 2
	mapIndirectElem   = 1 << 3
)

// The two thresholds above which a key or an element is stored behind a
// pointer, from internal/abi.
//
// A group holds MapGroupSlots slots inline, so a large key would make every
// group enormous whether the slots are used or not. Past the threshold the
// slot holds a pointer and the value is allocated separately, which is what
// the two Indirect flags tell the runtime.
const (
	mapMaxKeyBytes  = 128
	mapMaxElemBytes = 128
	mapGroupSlots   = 8
)

// mapPlan is everything the MapType header holds that is computed rather than
// referenced.
//
// It is a value rather than ten return values, and it is computed in one place
// because every field of it is derived from the group's layout. Reading one of
// them out of a different computation from the others is how a stride and an
// offset stop agreeing.
type mapPlan struct {
	group *ir.Type

	groupSize  int64
	keysOff    int64
	keyStride  int64
	elemsOff   int64
	elemStride int64
	elemOff    int64
	flags      uint32
}

// GroupOf returns the slot group type of a map.
//
// The runtime's map is a swiss table: a flat array of groups, each holding a
// control word and MapGroupSlots slots. The group is a struct the *compiler*
// synthesises, because the collector needs a pointer map for it and only a
// type carries one, and it is not written anywhere in the runtime's source.
// internal/runtime/maps/group.go is what it has to stay in step with.
//
//	type group struct {
//	    ctrl  uint64
//	    slots [8]struct {
//	        key  K
//	        elem E
//	    }
//	}
//
// A key or an element larger than the threshold is replaced by a pointer to
// it, which is what MapIndirectKey and MapIndirectElem tell the runtime. The
// substitution happens here rather than in the flags alone, because the group
// is what the collector scans: a group built from the value type with the
// pointer flag set would have the collector read a 200-byte array as a
// pointer.
func GroupOf(key, elem *ir.Type) (*ir.Type, error) {
	if key == nil || elem == nil {
		return nil, fmt.Errorf("rtype: a map group needs a key type and an element type")
	}
	if key.Align == 0 || elem.Align == 0 {
		return nil, fmt.Errorf("rtype: the group of map[%s]%s is asked for before either is laid out", key.Kind, elem.Kind)
	}
	slotKey, slotElem := key, elem
	if key.Size > mapMaxKeyBytes {
		slotKey = &ir.Type{Kind: ir.Ptr, Elem: key}
	}
	if elem.Size > mapMaxElemBytes {
		slotElem = &ir.Type{Kind: ir.Ptr, Elem: elem}
	}
	slot := &ir.Type{Kind: ir.Struct, Fields: []ir.Field{
		{Name: "key", Type: slotKey},
		{Name: "elem", Type: slotElem},
	}}
	group := &ir.Type{Kind: ir.Struct, Fields: []ir.Field{
		{Name: "ctrl", Type: &ir.Type{Kind: ir.Uint64}},
		{Name: "slots", Type: &ir.Type{Kind: ir.Array, Len: mapGroupSlots, Elem: slot}},
	}}
	if err := ir.Layout(group); err != nil {
		return nil, err
	}
	return group, nil
}

// mapPlanOf computes the group and every derived field of the header.
//
// The four offset and stride fields are read by one pair of formulas in the
// runtime, key(i) = KeysOff + i*KeyStride and elem(i) = ElemsOff +
// i*ElemStride, and the slots are interleaved, so the two strides are both the
// slot's size and the two offsets differ by where the element sits inside a
// slot. Writing either from a separate calculation is how a stride and an
// offset stop agreeing, and the runtime reads a key where an element is.
func mapPlanOf(t *ir.Type) (mapPlan, error) {
	if t.Key == nil || t.Elem == nil {
		return mapPlan{}, fmt.Errorf("rtype: %s has no key type or no element type", t)
	}
	group, err := GroupOf(t.Key, t.Elem)
	if err != nil {
		return mapPlan{}, err
	}
	slots := group.Fields[1]
	slot := slots.Type.Elem
	p := mapPlan{
		group:      group,
		groupSize:  group.Size,
		keysOff:    slots.Offset,
		keyStride:  slot.Size,
		elemOff:    slot.Fields[1].Offset,
		elemStride: slot.Size,
	}
	p.elemsOff = p.keysOff + p.elemOff

	if needKeyUpdate(t.Key) {
		p.flags |= mapNeedKeyUpdate
	}
	if hashMightPanic(t.Key) {
		p.flags |= mapHashMightPanic
	}
	if t.Key.Size > mapMaxKeyBytes {
		p.flags |= mapIndirectKey
	}
	if t.Elem.Size > mapMaxElemBytes {
		p.flags |= mapIndirectElem
	}
	return p, nil
}

// needKeyUpdate reports whether the runtime must overwrite a key when a value
// is stored under a key that is already there.
//
// Two keys can be equal and not identical, and then the stored one has to be
// replaced. A float carries a signed zero, so +0 and -0 compare equal and are
// different bytes. A string that compares equal may point at a larger backing
// array that the map would otherwise keep alive. An interface holds either.
// Every other comparable kind is its bytes, so equal keys are identical and
// the overwrite is work with no effect.
func needKeyUpdate(t *ir.Type) bool {
	switch t.Kind {
	case ir.Float32, ir.Float64, ir.Complex64, ir.Complex128, ir.String, ir.Interface:
		return true
	case ir.Array:
		return t.Elem != nil && needKeyUpdate(t.Elem)
	case ir.Struct:
		for _, f := range t.Fields {
			if needKeyUpdate(f.Type) {
				return true
			}
		}
		return false
	}
	return false
}

// hashMightPanic reports whether hashing a key of this type can panic.
//
// Only an interface can. Its dynamic type may be unhashable, and the hash
// finds out at run time, so the runtime has to be ready for a panic in the
// middle of a map operation and leave the table consistent. A key with no
// interface anywhere in it cannot panic, and telling the runtime otherwise
// costs it the slow path on every insert.
func hashMightPanic(t *ir.Type) bool {
	switch t.Kind {
	case ir.Interface:
		return true
	case ir.Array:
		return t.Elem != nil && hashMightPanic(t.Elem)
	case ir.Struct:
		for _, f := range t.Fields {
			if hashMightPanic(f.Type) {
				return true
			}
		}
		return false
	}
	return false
}

// mapTail returns the MapType header that follows internal/abi.Type.
func mapTail(t *ir.Type) ([]byte, []Reloc, []Symbol, error) {
	p, err := mapPlanOf(t)
	if err != nil {
		return nil, nil, nil, err
	}
	data := make([]byte, mapTailSize)
	put := func(off int, v int64) { binary.LittleEndian.PutUint64(data[off:], uint64(v)) }
	put(mapOffGroupSize, p.groupSize)
	put(mapOffKeysOff, p.keysOff)
	put(mapOffKeyStride, p.keyStride)
	put(mapOffElemsOff, p.elemsOff)
	put(mapOffElemStride, p.elemStride)
	put(mapOffElemOff, p.elemOff)
	binary.LittleEndian.PutUint32(data[mapOffFlags:], p.flags)

	var (
		relocs []Reloc
		syms   []Symbol
	)
	for _, r := range []struct {
		off int
		typ *ir.Type
	}{
		{mapOffKey, t.Key},
		{mapOffElem, t.Elem},
		{mapOffGroup, p.group},
	} {
		name, err := ir.TypeSymbol(r.typ)
		if err != nil {
			return nil, nil, nil, err
		}
		relocs = append(relocs, Reloc{
			Off: int32(TypeSize + r.off), Size: 8, Type: obj.R_ADDR, Target: name,
		})
	}

	// The Hasher is a func value, as Equal is, so it points at a closure and
	// never at the code. A nil Hasher is not an option the way a nil Equal is:
	// the runtime calls it on every operation, so a key with no hash is a key
	// this compiler cannot build a map for.
	fn, extra, err := hashClosure(t.Key)
	if err != nil {
		return nil, nil, nil, err
	}
	if fn == "" {
		return nil, nil, nil, fmt.Errorf("rtype: the key type %s of %s has no hash function, so it cannot be a map key", t.Key, t)
	}
	syms = append(syms, extra...)
	relocs = append(relocs, Reloc{
		Off: int32(TypeSize + mapOffHasher), Size: 8, Type: obj.R_ADDR, Target: fn,
	})
	return data, relocs, syms, nil
}

// mapEmittable reports the reason a map's descriptor cannot be filled in.
//
// The group type is the one that stops it, and the stop is a spelling rather
// than a fact the IR is missing. gc names the group noalg.map.group[K]V in the
// link string and map.group[K]V in the name string, two spellings for one
// synthesised struct, and ir/rtype.go's naming function produces neither: a
// struct with no name reaches its literal-struct case and is refused. So the
// bytes of the header are computable and the pointer to the group has no
// target to name.
func mapEmittable(t *ir.Type) error {
	p, err := mapPlanOf(t)
	if err != nil {
		return err
	}
	if fn, _, err := hashFunc(t.Key); err != nil {
		return err
	} else if fn == "" {
		return fmt.Errorf("rtype: the key type %s of %s has no hash function, so it cannot be a map key", t.Key, t)
	}
	if _, err := ir.TypeSymbol(p.group); err != nil {
		return fmt.Errorf("rtype: %s needs a descriptor for its slot group, which gc names "+
			"noalg.map.group[K]V and the naming function has no spelling for: %v", t, err)
	}
	return nil
}
