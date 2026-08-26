// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"encoding/binary"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
)

// The layout of internal/abi.StructType after the common Type, on a 64-bit
// target, and of one internal/abi.StructField.
//
//	type StructType struct {
//	    Type
//	    PkgPath Name
//	    Fields  []StructField
//	}
//
//	type StructField struct {
//	    Name   Name
//	    Typ    *Type
//	    Offset uintptr
//	}
//
// A Name is one pointer and a slice is three words, so the header is four
// words. The fields themselves are variable-length data that the header's
// slice points at, which is why they go in the C..D section of Descriptor
// rather than here.
const (
	structOffPkgPath = 0
	structOffFields  = 8
	structOffLen     = 16
	structOffCap     = 24
	structTailSize   = 32

	fieldOffName   = 0
	fieldOffType   = 8
	fieldOffOffset = 16
	fieldSize      = 24
)

// structTail returns the StructType header that follows internal/abi.Type.
//
// dataOff is the offset of the field array within the descriptor, which the
// header's slice points at with a relocation against the descriptor's own
// symbol. self names that symbol.
func structTail(t *ir.Type, self string, dataOff int) ([]byte, []Reloc, []Symbol, error) {
	path, err := structPkgPath(t)
	if err != nil {
		return nil, nil, nil, err
	}
	data := make([]byte, structTailSize)
	binary.LittleEndian.PutUint64(data[structOffLen:], uint64(len(t.Fields)))
	binary.LittleEndian.PutUint64(data[structOffCap:], uint64(len(t.Fields)))

	var (
		relocs []Reloc
		syms   []Symbol
	)
	if path != "" {
		ip := importPathSymbol(path)
		syms = append(syms, ip)
		relocs = append(relocs, Reloc{
			Off: int32(TypeSize + structOffPkgPath), Size: 8, Type: obj.R_ADDR, Target: ip.Name,
		})
	}
	// The slice points inside this same symbol. A descriptor is one symbol,
	// so the field array cannot be addressed any other way, and the addend is
	// what makes the relocation land at D rather than at the descriptor.
	relocs = append(relocs, Reloc{
		Off: int32(TypeSize + structOffFields), Size: 8, Type: obj.R_ADDR,
		Add: int64(dataOff), Target: self,
	})
	return data, relocs, syms, nil
}

// structFields returns the field array the header points at.
func structFields(t *ir.Type, base int) ([]byte, []Reloc, []Symbol, error) {
	data := make([]byte, len(t.Fields)*fieldSize)
	var (
		relocs []Reloc
		syms   []Symbol
	)
	for i, f := range t.Fields {
		off := base + i*fieldSize
		n := fieldName(f)
		syms = append(syms, n)
		relocs = append(relocs, Reloc{
			Off: int32(off + fieldOffName), Size: 8, Type: obj.R_ADDR, Target: n.Name,
		})
		ft, err := ir.TypeSymbol(f.Type)
		if err != nil {
			return nil, nil, nil, err
		}
		relocs = append(relocs, Reloc{
			Off: int32(off + fieldOffType), Size: 8, Type: obj.R_ADDR, Target: ft,
		})
		binary.LittleEndian.PutUint64(data[i*fieldSize+fieldOffOffset:], uint64(f.Offset))
	}
	return data, relocs, syms, nil
}

// structAlg returns the equality algorithm of a struct.
//
// It is cmd/compile/internal/types.AlgType's rule and the reasons are the
// runtime's. A struct compares as one region of memory only when every field
// does and no byte between or after the fields is padding, because padding
// holds whatever the last write left there and two equal values would compare
// unequal. A blank field is the same problem: it is not compared at all, so a
// memory comparison would compare it.
//
// The one-field case is not an optimisation. A struct with one non-blank field
// compares exactly as that field, so a struct holding one string reaches
// runtime.strequal rather than a generated function.
func structAlg(t *ir.Type) algKind {
	if len(t.Fields) == 1 && t.Fields[0].Name != "_" {
		return alg(t.Fields[0].Type)
	}
	ret := algMem
	for i, f := range t.Fields {
		a := alg(f.Type)
		if a == algNone {
			// Not comparable at all, whatever the other fields are. This wins
			// over algSpecial, which is why it returns rather than assigns.
			return algNone
		}
		if a != algMem || f.Name == "_" || paddedField(t, i) {
			ret = algSpecial
		}
	}
	return ret
}

// paddedField reports whether field i is followed by padding.
//
// The bytes after the last field are padding too, which is why the end of the
// struct is the bound for the last one.
func paddedField(t *ir.Type, i int) bool {
	end := t.Size
	if i+1 < len(t.Fields) {
		end = t.Fields[i+1].Offset
	}
	f := t.Fields[i]
	return f.Offset+f.Type.Size != end
}
