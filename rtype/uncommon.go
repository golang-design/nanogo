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

// The layout of internal/abi.UncommonType on a 64-bit target.
//
//	type UncommonType struct {
//	    PkgPath NameOff
//	    Mcount  uint16
//	    Xcount  uint16
//	    Moff    uint32
//	    _       uint32
//	}
//
// Moff is an offset from the start of the UncommonType and not from the start
// of the descriptor, because UncommonType.Methods does the arithmetic on a
// *UncommonType. gc writes it whether there are methods or not.
const (
	uncommonOffPkgPath = 0
	uncommonOffMcount  = 4
	uncommonOffXcount  = 6
	uncommonOffMoff    = 8
	uncommonSize       = 16
)

// hasUncommon reports whether t's descriptor carries an UncommonType.
//
// gc's rule is "the type has a name or a method". Only the first half is here,
// and the second half is unreachable: the only type with a method and no name
// is a pointer to a defined type, and a type with a method is refused. When
// methods are written this has to grow the second half, or *T will carry
// methods with nowhere to put them.
//
// A predeclared type has a name, so int carries an UncommonType with an empty
// package path and no methods. That is not an oddity to optimise away. The
// flag and the section are one decision: TFlagUncommon set with no section
// makes the runtime read sixteen bytes past the end of the symbol, which is
// what nanogo emitted for every named type before this file existed.
func hasUncommon(t *ir.Type) bool {
	return t.Name != ""
}

// methodSet returns the methods t's descriptor must describe, sorted by name.
//
// # Which set belongs in which descriptor
//
// A method with a value receiver is in the method sets of both T and *T; one
// with a pointer receiver is in *T's only. ir.Type.Methods carries the
// pointer's set, so T's is that set with the PtrOnly entries dropped, and *T's
// is the whole of the pointee's.
//
// # Why a nil set on a defined type is refused rather than read as empty
//
// ir.Converter sets Methods on every defined type, empty set included, so an
// unset field means the type was built below the type boundary by hand. A
// descriptor written from it would claim a method set that nobody established,
// reflect would report no methods, and an itab built against it would find no
// functions. That is the failure specs/032 records, so the gap is refused.
func methodSet(t *ir.Type) ([]ir.Method, error) {
	// The receiver base type, which is what a method is declared on. gc
	// computes the same thing with ReceiverBaseType.
	base := t
	if base.Kind == ir.Ptr && base.Elem != nil {
		base = base.Elem
	}
	if base.Kind == ir.Interface {
		// An interface declares no method on itself. Its methods are the
		// interface's own and they go in the InterfaceType header as Imethods,
		// which is a different section with a different encoding, so an
		// UncommonType built from them would describe the same methods twice
		// and in the wrong place.
		return nil, nil
	}
	if t.Kind == ir.Ptr && defined(t.Elem) {
		if t.Elem.Methods == nil {
			return nil, fmt.Errorf("rtype: the method set of %s is not in the IR type", t.Elem)
		}
		return t.Elem.Methods, nil
	}
	if !defined(t) {
		// The language declares a method on a defined type and on nothing
		// else, so every other type's method set is empty by the language
		// rather than by anything the IR carries.
		return nil, nil
	}
	if t.Methods == nil {
		return nil, fmt.Errorf("rtype: the method set of %s is not in the IR type", t)
	}
	out := make([]ir.Method, 0, len(t.Methods))
	for _, m := range t.Methods {
		if m.PtrOnly {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// uncommonTail returns the UncommonType section of t's descriptor.
//
// dataAdd is the size of the variable-length section that follows it, which
// Moff has to skip to reach the method array.
func uncommonTail(t *ir.Type, dataAdd int) ([]byte, []Reloc, []Symbol, error) {
	ms, err := methodSet(t)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(ms) > 0 {
		return nil, nil, nil, methodRefusal(t, ms)
	}
	data := make([]byte, uncommonSize)
	// Mcount and Xcount stay zero. Moff is written anyway, as gc writes it:
	// it is the distance from here to the method array, and the array being
	// empty does not make the distance meaningless.
	binary.LittleEndian.PutUint32(data[uncommonOffMoff:], uint32(uncommonSize+dataAdd))

	var (
		relocs []Reloc
		syms   []Symbol
	)
	if path := uncommonPkgPath(t); path != "" {
		ip := importPathSymbol(path)
		syms = append(syms, ip)
		// A NameOff and not a pointer. specs/032 says what a pointer where an
		// offset belongs costs: a binary that fails at load.
		relocs = append(relocs, Reloc{Size: 4, Type: obj.R_ADDROFF, Target: ip.Name})
	}
	return data, relocs, syms, nil
}

// uncommonPkgPath returns the import path an UncommonType carries.
//
// gc takes it from the type's own symbol, and from the element's symbol for a
// pointer, a slice, an array or a channel whose element is a defined type. A
// predeclared type's package is the universe, which has no path, so int
// carries an UncommonType with a zero PkgPath.
func uncommonPkgPath(t *ir.Type) string {
	if t.PkgPath != "" {
		return t.PkgPath
	}
	if t.Name != "" {
		return ""
	}
	switch t.Kind {
	case ir.Ptr, ir.Slice, ir.Array, ir.Chan:
		if t.Elem != nil {
			return t.Elem.PkgPath
		}
	}
	return ""
}

// methodRefusal names the first method that stops the descriptor.
//
// A descriptor's Method holds four fields and this compiler can write two of
// them. The refusal says which of the other two is missing, and the two are
// not the same kind of gap:
//
//   - Mtyp is a TypeOff to the descriptor of the method's type with the
//     receiver removed. ir.Method.Sig carries that type now, so this is a gap
//     only for a method built below the type boundary by hand. Zero is a legal
//     Mtyp meaning "unexported, reflect may not call it", so a zero written
//     for an absent signature would be read as a fact.
//   - Ifn and Tfn are TextOffs to the two ABI wrappers, and no signature
//     closes them. Tfn is the method itself when the receiver is a value.
//     Ifn takes a one-word receiver, so for a type that is not stored directly
//     in an interface it is a wrapper on the pointer. gc generates both in the
//     declaring package, as code, and nanogo generates neither: rtype returns
//     data symbols and a wrapper is a text symbol with a body.
//
// So the message names the wrappers, and names the signature as well only when
// one is actually absent. The clause was unconditional and is now false for
// every type the converter produced, and a refusal that states a fact that is
// no longer true sends the next reader to the wrong file.
//
// The set is sorted, so the method named is the same one on every run
// (specs/053-determinism.md).
func methodRefusal(t *ir.Type, ms []ir.Method) error {
	m := ms[0]
	if gap := firstWithoutSignature(ms); gap != nil {
		m = *gap
	}
	name := m.Name
	if m.Pkg != "" {
		name = m.Pkg + "." + m.Name
	}
	if m.Sig == nil {
		return fmt.Errorf("rtype: %s has %d method(s) and a descriptor for one needs "+
			"its signature, which %s does not carry, and the two ABI wrappers, "+
			"which nanogo does not generate; the first is %s", t, len(ms), t, name)
	}
	return fmt.Errorf("rtype: %s has %d method(s) and a descriptor for one needs "+
		"the two ABI wrappers Ifn and Tfn, which nanogo does not generate; "+
		"the first is %s", t, len(ms), name)
}

// firstWithoutSignature returns the first method with no signature, or nil.
//
// The set is sorted, so the one returned is the same on every run. A method
// with no signature is named ahead of one that has one, because it is the
// gap the reader can close and the wrappers are not.
func firstWithoutSignature(ms []ir.Method) *ir.Method {
	for i := range ms {
		if ms[i].Sig == nil {
			return &ms[i]
		}
	}
	return nil
}
