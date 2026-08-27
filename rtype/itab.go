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

// The layout of internal/abi.ITab on a 64-bit target.
//
//	type ITab struct {
//	    Inter *InterfaceType
//	    Type  *Type
//	    Hash  uint32     // a copy of Type.Hash, for type switches
//	    _     uint32
//	    Fun   [1]uintptr // variable sized; Fun[0] == 0 means Type does not
//	                    // implement Inter
//	}
//
// Fun is declared with one element and is as long as the interface's method
// list. The runtime reads it by index, and the index is the interface's and
// not the concrete type's: an itab built for io.ReadWriter holds two entries
// where io.Reader reads one, which is why the itab is per pair and not per
// type.
//
// The two pointers are pointers and not offsets. An itab is allocated in
// persistentalloc space when the runtime builds one, so the fields hold
// addresses rather than offsets into a module's data, and a compile-time itab
// has to look like the ones the runtime builds.
const (
	itabOffInter = 0
	itabOffType  = 8
	itabOffHash  = 16
	itabOffFun   = 24
)

// Itab returns the symbols that define the itab of the concrete type t and the
// interface iface.
//
// The first symbol is the itab. There are no others: everything it points at
// is a descriptor or a function that some other part of the object defines,
// which is what [ItabReferenced] and ssagen.MethodWrappers name.
//
// # Identity
//
// One itab per pair. The runtime compares two interface values by comparing
// their first words, so two itabs for one pair make every such comparison
// false and every type switch on the pair miss. ir.ItabSymbol is the one
// naming function and the symbol is duplicate-tolerant, so two packages that
// convert the same type to the same interface write one symbol after the link.
//
// It is a named non-package definition and not a content-addressable one, for
// the reason the descriptor is: cmd/link deduplicates a duplicate-tolerant
// symbol by name in the non-package index space, and it reads no name for a
// symbol in the hashed space. gc writes an itab into the hashed space instead
// and gets the same single copy from a content hash. The two mechanisms cannot
// merge with each other, so a pair converted by a nanogo package and by a gc
// package in one binary would have two itabs. That needs a gc package to name
// a nanogo package's type, which the hosted model of specs/000 decision 10
// does not produce: the standard library imports nothing a user wrote.
//
// # Why the method pointers are strong references
//
// gc writes each Fun entry with a weak relocation, so cmd/link may resolve one
// to zero when nothing reaches the method. What keeps a method that is reached
// is an R_USEIFACEMETHOD relocation, which gc emits at every interface call
// site and nanogo emits nowhere. A weak entry here would therefore be zero
// whenever the linker could not prove the method live by another route, and an
// interface call through a zero entry jumps to address zero.
//
// A strong reference is always correct and merely keeps more code alive. The
// weak form is an optimisation that needs machinery this compiler does not
// have, and it becomes available on the day an interface call names the
// interface and the method it selects.
func Itab(t, iface *ir.Type) ([]Symbol, error) {
	name, err := ir.ItabSymbol(t, iface)
	if err != nil {
		return nil, err
	}
	entries, err := itabEntries(t, iface)
	if err != nil {
		return nil, err
	}
	// Fun is declared [1]uintptr, so an itab for an interface with no methods
	// is still one word long. The language has no such interface with a name
	// and methods, and ir.ItabSymbol refuses the empty one, so this is the
	// floor gc's ITab.Size computes and not a case that arises.
	n := len(entries)
	if n == 0 {
		n = 1
	}
	data := make([]byte, itabOffFun+n*ir.PtrSize)

	// A copy of the concrete type's hash. runtime.getitab compares it and a
	// type switch reads it out of the itab rather than out of the descriptor,
	// so a hash that disagreed with the descriptor's would make the switch
	// miss on values that reached it through this itab alone.
	h, err := Hash(t)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(data[itabOffHash:], h)

	is, err := ir.TypeSymbol(iface)
	if err != nil {
		return nil, err
	}
	ts, err := ir.TypeSymbol(t)
	if err != nil {
		return nil, err
	}
	relocs := []Reloc{
		{Off: itabOffInter, Size: 8, Type: obj.R_ADDR, Target: is},
		{Off: itabOffType, Size: 8, Type: obj.R_ADDR, Target: ts},
	}
	for i, fn := range entries {
		relocs = append(relocs, Reloc{
			Off: int32(itabOffFun + i*ir.PtrSize), Size: 8, Type: obj.R_ADDR,
			Target: fn, GoFunc: true,
		})
	}
	return []Symbol{{
		Name:   name,
		Kind:   obj.SRODATA,
		Align:  ir.PtrSize,
		Dupok:  true,
		Data:   data,
		Relocs: relocs,
	}}, nil
}

// ItabReferenced returns the types whose descriptors an itab points at.
//
// An itab is not a leaf. Its first two words are the interface's descriptor
// and the concrete type's, and cmd/link resolves both by name, so whoever
// writes the itab owes both. gc closes the same set the same way, in
// writeITab, by calling writeType on each before it writes the bytes.
func ItabReferenced(t, iface *ir.Type) ([]*ir.Type, error) {
	if _, err := ir.ItabSymbol(t, iface); err != nil {
		return nil, err
	}
	return []*ir.Type{iface, t}, nil
}

// itabEntries returns the function each Fun slot names, in the interface's own
// method order.
//
// Both lists are in ir.MethodOrder, so the intersection is one pass. gc walks
// them the same way, in writeITab, and for the same reason: the order is a
// property of the two method sets and not of the walk, so a second ordering
// rule here would produce an itab whose slots hold the wrong methods while
// holding the right count.
//
// The function is the method's Ifn and not its Tfn. An itab call passes a
// one-word receiver, which is the value itself when the type is stored
// directly in an interface and a pointer to it otherwise, and Ifn is the entry
// point that takes that word. rtype/uncommon.go draws the same table for the
// descriptor's Method array, and the two have to agree: an interface call and
// a reflect call reach one function.
func itabEntries(t, iface *ir.Type) ([]string, error) {
	ims, err := imethods(iface)
	if err != nil {
		return nil, err
	}
	ms, err := methodSet(t)
	if err != nil {
		return nil, err
	}
	recv := t
	if t.Kind == ir.Ptr {
		recv = t.Elem
	}
	ifnPtr := t.Kind == ir.Ptr || !directIface(t)

	out := make([]string, 0, len(ims))
	next := 0
	for _, im := range ims {
		found := false
		for ; next < len(ms); next++ {
			if ms[next].Name == im.Name && ms[next].Pkg == im.Pkg {
				found = true
				break
			}
		}
		if !found {
			// gc writes an itab with Fun[0] == 0 here, which is how the
			// runtime says "this type does not implement this interface", and
			// it reaches that case only for a type assertion whose target is
			// parameterised. The type checker rejects every other route to it,
			// so an IR that arrives here was built wrongly, and an itab that
			// claimed the type does not implement the interface would turn
			// that into a panic at the conversion rather than a diagnostic.
			return nil, fmt.Errorf("rtype: the itab of %s and %s has no method for %s, which %s does not implement",
				t, iface, im.Name, t)
		}
		fn, err := ir.MethodSymbol(recv, ms[next], ifnPtr)
		if err != nil {
			return nil, err
		}
		out = append(out, fn)
		next++
	}
	return out, nil
}
