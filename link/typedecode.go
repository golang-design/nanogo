// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"encoding/binary"

	"golang.design/x/nanogo/obj"
)

// Enough of internal/abi's layout to decide which methods of a reachable
// type can be called.
//
// The decoders live here and not in rtype, which writes descriptors, for
// the reason cmd/link keeps its own in ld/decodesym.go: reading a
// descriptor out of a linked program is the linker's work and the offsets
// it needs are the ones the runtime reads, not the ones a compiler writes.
// specs/032-type-descriptors-and-itabs.md owns the layout.
const (
	// tflagUncommon says the descriptor is followed by an UncommonType,
	// which is where a defined type's methods are.
	tflagUncommon = 1 << 0
	// kindMask selects the kind out of the byte that also holds the
	// direct-interface and garbage collection bits.
	kindMask = (1 << 5) - 1

	// uncommonMethodSize is sizeof(abi.Method): a name offset and a type
	// offset, then the two code pointers.
	uncommonMethodSize = 4 * 4
)

// Kinds this package names. The values are internal/abi's and the order is
// exact.
const (
	kindInterface = 20
	kindMap       = 21
	kindPointer   = 22
	kindSlice     = 23
	kindStruct    = 25
	kindArray     = 17
	kindChan      = 18
	kindFunc      = 19
)

// commonSize is sizeof(abi.Type).
func commonSize(ptrSize int) int { return 4*ptrSize + 8 + 8 }

// rtypeSize is sizeof of the kind-specific descriptor, which is what an
// UncommonType follows.
func rtypeSize(kind byte, ptrSize int) int {
	cs := commonSize(ptrSize)
	switch kind {
	case kindStruct, kindInterface:
		return cs + 4*ptrSize
	case kindPointer, kindFunc, kindSlice:
		return cs + ptrSize
	case kindArray:
		return cs + 3*ptrSize
	case kindChan:
		return cs + 2*ptrSize
	case kindMap:
		sz := cs + 10*ptrSize + 4
		if ptrSize == 8 {
			sz += 4 // the final uint32 is padded to a pointer boundary
		}
		return sz
	}
	return cs
}

// itabTypeOff is where an itab holds the type it is for.
func itabTypeOff(ptrSize int) int { return ptrSize }

// typeKind returns the kind byte of a descriptor.
func typeKind(data []byte, ptrSize int) byte {
	if len(data) <= 2*ptrSize+7 {
		return 0
	}
	return data[2*ptrSize+7] & kindMask
}

// typeHasUncommon reports whether a descriptor carries a method list.
func typeHasUncommon(data []byte, ptrSize int) bool {
	if len(data) <= 2*ptrSize+4 {
		return false
	}
	return data[2*ptrSize+4]&tflagUncommon != 0
}

// relocAt returns the relocation of s at exactly offset off, and whether
// there is one. A descriptor stores a pointer or an offset to another
// symbol as a relocation, so this is how one field is followed.
func relocAt(s *Sym, off int32) (obj.Reloc, bool) {
	for _, r := range s.Relocs {
		if r.Off == off {
			return r, true
		}
	}
	return obj.Reloc{}, false
}

// typeName decodes the name a descriptor's field at off points at.
//
// A name is a flag byte, a variable length length, and the bytes. It is
// the encoding reflect reads.
func (l *Loader) typeName(st *objState, s *Sym, off int) string {
	r, ok := relocAt(s, int32(off))
	if !ok {
		return ""
	}
	n := l.Def(l.resolve(st, r.Sym))
	if n == nil || len(n.Data) < 2 {
		return ""
	}
	data := n.Data
	end := 1 + binary.MaxVarintLen64
	if len(data) < end {
		end = len(data)
	}
	size, used := binary.Uvarint(data[1:end])
	if used <= 0 || 1+used+int(size) > len(data) {
		return ""
	}
	return string(data[1+used : 1+used+int(size)])
}

// A methodSig is a method's name and the descriptor of its signature. Two
// methods match when both agree, which is what lets an interface method
// keep the concrete methods that could satisfy it.
type methodSig struct {
	name string
	typ  Global
}

// A methodRef is where a type names one of its methods: three consecutive
// R_METHODOFF relocations, for the signature, the interface call wrapper
// and the direct one.
type methodRef struct {
	sig methodSig
	src Global // the type descriptor
	rel int    // the index of the first of the three relocations
}

// exported reports whether the method is exported, which is what decides
// whether reflection can reach it once the pass has given up on static
// analysis.
func (m methodRef) exported() bool {
	for _, r := range m.sig.name {
		return r >= 'A' && r <= 'Z'
	}
	return false
}

// typeMethods decodes the method list of a defined type.
func (l *Loader) typeMethods(st *objState, g Global, s *Sym, ptrSize int) []methodSig {
	if !typeHasUncommon(s.Data, ptrSize) {
		return nil
	}
	off := rtypeSize(typeKind(s.Data, ptrSize), ptrSize)
	// The record is sixteen bytes and its last four are padding, which gc
	// trims when they are the end of the symbol. So what must be present
	// is the twelve bytes up to and including the method offset.
	if off+12 > len(s.Data) {
		return nil
	}
	count := int(le16(s.Data, off+4))
	moff := int(le32(s.Data, off+8))
	off += moff
	out := make([]methodSig, count)
	for i := range out {
		at := off + i*uncommonMethodSize
		out[i].name = l.typeName(st, s, at)
		if r, ok := relocAt(s, int32(at+4)); ok {
			out[i].typ = l.resolve(st, r.Sym)
		}
	}
	return out
}

// ifaceMethod decodes one method of an interface descriptor, at the offset
// a marker relocation names.
func (l *Loader) ifaceMethod(st *objState, s *Sym, off int64) (methodSig, bool) {
	var m methodSig
	m.name = l.typeName(st, s, int(off))
	if r, ok := relocAt(s, int32(off+4)); ok {
		m.typ = l.resolve(st, r.Sym)
	}
	return m, m.name != ""
}

// itabType returns the type an itab is for.
func (l *Loader) itabType(st *objState, s *Sym, ptrSize int) Global {
	if r, ok := relocAt(s, int32(itabTypeOff(ptrSize))); ok {
		return l.resolve(st, r.Sym)
	}
	return 0
}

// tflagOff is where a descriptor holds its flag byte, and tflagExtraStar
// says the string the descriptor names has a star in front of it that the
// type's name does not. internal/abi owns both.
func tflagOff(ptrSize int) int { return 2*ptrSize + 4 }

const tflagExtraStar = 1 << 1

// typeStr decodes the string a type descriptor names itself by.
//
// It is the key the linker sorts the type descriptors of the typelink
// table by, so that reflect can rely on one descriptor per type string.
// The extra star an indirect type carries is removed, which is what makes
// the sort agree with the name the runtime prints.
func (l *Loader) typeStr(st *objState, s *Sym, ptrSize int) string {
	str := l.typeName(st, s, 4*ptrSize+8)
	if off := tflagOff(ptrSize); off < len(s.Data) && s.Data[off]&tflagExtraStar != 0 && str != "" {
		return str[1:]
	}
	return str
}
