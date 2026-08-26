// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"encoding/binary"
	"fmt"
	"sort"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
)

// The layout of internal/abi.InterfaceType after the common Type, on a 64-bit
// target, and of one internal/abi.Imethod.
//
//	type InterfaceType struct {
//	    Type
//	    PkgPath Name      // import path
//	    Methods []Imethod
//	}
//
//	type Imethod struct {
//	    Name NameOff // name of the method
//	    Typ  TypeOff // the *FuncType underneath
//	}
//
// A Name is one pointer and a slice is three words, so the header is four
// words. The methods themselves are variable-length data the header's slice
// points at, so they go in the C..D section of Descriptor rather than here,
// which is also where a struct's fields go and for the same reason.
//
// Both of an Imethod's fields are four-byte offsets and neither is a pointer.
// specs/032 says what a pointer where an offset belongs costs: a binary that
// fails at load.
const (
	ifaceOffPkgPath = 0
	ifaceOffMethods = 8
	ifaceOffLen     = 16
	ifaceOffCap     = 24
	ifaceTailSize   = 32

	imethodOffName = 0
	imethodOffTyp  = 4
	imethodSize    = 8
)

// ifaceTail returns the InterfaceType header that follows internal/abi.Type.
//
// dataOff is the offset of the method array within the descriptor, which the
// header's slice points at with a relocation against the descriptor's own
// symbol. self names that symbol.
func ifaceTail(t *ir.Type, self string, dataOff int) ([]byte, []Reloc, []Symbol, error) {
	ms, err := imethods(t)
	if err != nil {
		return nil, nil, nil, err
	}
	data := make([]byte, ifaceTailSize)
	binary.LittleEndian.PutUint64(data[ifaceOffLen:], uint64(len(ms)))
	binary.LittleEndian.PutUint64(data[ifaceOffCap:], uint64(len(ms)))

	var (
		relocs []Reloc
		syms   []Symbol
	)
	// gc writes the declaring package's path for every named interface except
	// any and error, whose descriptors the runtime owns. A literal interface
	// has no package. RuntimeOwned answers the same question for the same two
	// types, so the rule is stated once.
	if t.PkgPath != "" && !RuntimeOwned(t) {
		ip := importPathSymbol(t.PkgPath)
		syms = append(syms, ip)
		// A pointer and not an offset. The field is a Name, which gc writes
		// with dgopkgpath, and a struct descriptor's PkgPath is written the
		// same way.
		relocs = append(relocs, Reloc{
			Off: int32(TypeSize + ifaceOffPkgPath), Size: 8, Type: obj.R_ADDR, Target: ip.Name,
		})
	}
	if len(ms) > 0 {
		// The slice points inside this same symbol, as a struct's field slice
		// does. A descriptor is one symbol, so the method array cannot be
		// addressed any other way, and the addend is what makes the
		// relocation land at D rather than at the descriptor.
		relocs = append(relocs, Reloc{
			Off: int32(TypeSize + ifaceOffMethods), Size: 8, Type: obj.R_ADDR,
			Add: int64(dataOff), Target: self,
		})
	}
	return data, relocs, syms, nil
}

// ifaceMethods returns the Imethod array the header points at, which starts at
// base within the descriptor.
func ifaceMethods(t *ir.Type, base int) ([]byte, []Reloc, []Symbol, error) {
	ms, err := imethods(t)
	if err != nil {
		return nil, nil, nil, err
	}
	data := make([]byte, len(ms)*imethodSize)
	var (
		relocs []Reloc
		syms   []Symbol
	)
	for i, m := range ms {
		off := base + i*imethodSize
		n := nameSymbol(m.Name, "", isExportedName(m.Name), false)
		syms = append(syms, n)
		relocs = append(relocs, Reloc{
			Off: int32(off + imethodOffName), Size: 4, Type: obj.R_ADDROFF, Target: n.Name,
		})
		sig, err := ir.TypeSymbol(m.Sig)
		if err != nil {
			return nil, nil, nil, err
		}
		relocs = append(relocs, Reloc{
			Off: int32(off + imethodOffTyp), Size: 4, Type: obj.R_ADDROFF, Target: sig,
		})
	}
	return data, relocs, syms, nil
}

// imethods returns the methods an InterfaceType describes, in the order the
// descriptor holds them.
//
// The order is gc's and not the IR's. gc sorts an interface's methods with
// types.CompareSyms, which puts every exported name ahead of every unexported
// one and orders within each group by name and then by package path. The IR
// sorts by name alone, which is enough for determinism and is not this order,
// so the encoder sorts rather than trusting the boundary.
//
// The two agree for every ASCII identifier, because a capital letter sorts
// below every lower-case one, and agreeing is not the same as being the same
// rule. A name beginning with a capital outside ASCII is exported and sorts
// above no lower-case letter at all, so byte order would put it after the
// unexported names and gc would put it before them.
func imethods(t *ir.Type) ([]ir.Method, error) {
	if err := ifaceEmittable(t); err != nil {
		return nil, err
	}
	out := make([]ir.Method, len(t.Methods))
	copy(out, t.Methods)
	sort.SliceStable(out, func(i, j int) bool {
		ei, ej := isExportedName(out[i].Name), isExportedName(out[j].Name)
		if ei != ej {
			return ei
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Pkg < out[j].Pkg
	})
	return out, nil
}

// ifaceEmittable reports the reason an interface's descriptor cannot be filled
// in.
func ifaceEmittable(t *ir.Type) error {
	if t.Methods == nil {
		// ir.Converter sets the set on every interface it converts, the empty
		// set included, so a nil one means the type was built below the type
		// boundary. A descriptor written from it would claim an interface with
		// no methods, and every type would satisfy it.
		return fmt.Errorf("rtype: the method set of the interface %s is not in the IR type", t)
	}
	if t.EmptyIface != (len(t.Methods) == 0) {
		// The two say the same thing and they disagree. EmptyIface decides
		// which of two layouts the first word has and therefore which equality
		// routine reads it, so the pair cannot be left to whichever the reader
		// happens to consult.
		return fmt.Errorf("rtype: %s says EmptyIface is %v and carries %d method(s)",
			t, t.EmptyIface, len(t.Methods))
	}
	for _, m := range t.Methods {
		if m.Sig == nil {
			return fmt.Errorf("rtype: the interface method %s of %s has no signature, "+
				"which its Imethod's Typ is an offset to", m.Name, t)
		}
		if m.Pkg != "" && m.Pkg != t.PkgPath {
			// gc encodes such a name with internal/abi.Name's bit 2 set and a
			// package-path offset after the name, which rtype/name.go does not
			// write. It is reachable only by embedding an interface from
			// another package that has an unexported method.
			return fmt.Errorf("rtype: the interface method %s of %s is unexported and declared in %s, "+
				"so its name needs a package path, which the name encoder does not write",
				m.Name, t, m.Pkg)
		}
	}
	return nil
}
