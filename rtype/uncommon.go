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
//
// One entry of the method array it points at is internal/abi.Method:
//
//	type Method struct {
//	    Name NameOff // the method's name
//	    Mtyp TypeOff // the method's type with the receiver removed
//	    Ifn  TextOff // the function an itab call reaches, one-word receiver
//	    Tfn  TextOff // the function an ordinary method call reaches
//	}
//
// Four four-byte offsets and no pointer. Name is an offset into the module's
// name data and the other three are offsets into its text and type data, which
// is why the relocation on Name is not the relocation on the other three:
// cmd/link resolves R_METHODOFF to -1 when the target was eliminated as dead,
// and it reads the three of them as one group. Its deadcode pass panics with
// "expect three consecutive R_METHODOFF relocs" if anything separates them, so
// the three are emitted together and in this order.
const (
	uncommonOffPkgPath = 0
	uncommonOffMcount  = 4
	uncommonOffXcount  = 6
	uncommonOffMoff    = 8
	uncommonSize       = 16

	methodOffName = 0
	methodOffMtyp = 4
	methodOffIfn  = 8
	methodOffTfn  = 12
	methodSize    = 16
)

// hasUncommon reports whether t's descriptor carries an UncommonType.
//
// gc's rule is "the type has a name or a method", and both halves are live.
// The second one is what a pointer to a defined type reaches: *T carries no
// name of its own and carries T's whole method set, so a rule that read the
// name alone would put T's methods in a descriptor with nowhere to hold them.
//
// A predeclared type has a name, so int carries an UncommonType with an empty
// package path and no methods. That is not an oddity to optimise away. The
// flag and the section are one decision: TFlagUncommon set with no section
// makes the runtime read sixteen bytes past the end of the symbol, which is
// what nanogo emitted for every named type before this file existed.
func hasUncommon(t *ir.Type) (bool, error) {
	if t.Name != "" {
		return true, nil
	}
	ms, err := methodSet(t)
	if err != nil {
		return false, err
	}
	return len(ms) > 0, nil
}

// methodSet returns the methods t's descriptor must describe, in the order
// the Method array holds them.
//
// # Which set belongs in which descriptor
//
// A method with a value receiver is in the method sets of both T and *T; one
// with a pointer receiver is in *T's only. ir.Type.Methods carries the
// pointer's set, so T's is that set with the PtrOnly entries dropped, and *T's
// is the whole of the pointee's.
//
// # The order
//
// ir.MethodOrder, which is gc's types.CompareSyms, and it is applied here
// rather than trusted from the IR because the descriptor's Xcount is derived
// from it. Dropping the PtrOnly entries keeps the order, so the filtered set
// of T is ordered exactly when the set of *T is.
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
		return ir.MethodOrder(t.Elem.Methods), nil
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
	for _, m := range ir.MethodOrder(t.Methods) {
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
	if len(ms) != int(uint16(len(ms))) {
		// Mcount is a uint16. gc stops the compiler on the same count.
		return nil, nil, nil, fmt.Errorf("rtype: %s has %d methods and Mcount holds %d", t, len(ms), int(^uint16(0)))
	}
	moff := uncommonSize + dataAdd
	if moff != int(uint32(moff)) {
		return nil, nil, nil, fmt.Errorf("rtype: the methods of %s are %d bytes away and Moff is four bytes", t, moff)
	}
	data := make([]byte, uncommonSize)
	// Moff is written whether there are methods or not, as gc writes it: it is
	// the distance from here to the method array, and the array being empty
	// does not make the distance meaningless.
	binary.LittleEndian.PutUint16(data[uncommonOffMcount:], uint16(len(ms)))
	binary.LittleEndian.PutUint16(data[uncommonOffXcount:], uint16(exportedCount(ms)))
	binary.LittleEndian.PutUint32(data[uncommonOffMoff:], uint32(moff))

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

// exportedCount is the length of the exported prefix of ms, which is Xcount.
//
// reflect.Type.NumMethod on a type that is not an interface returns Xcount and
// reflect.Type.Method indexes the array by it, so this number is the exported
// method set as every reflecting program sees it. gc computes it with a binary
// search for the first unexported name, which is the same number because the
// array is in ir.MethodOrder and that order puts every exported name first. It
// is counted here rather than searched for, because a count is right whatever
// the order is and a search silently is not.
func exportedCount(ms []ir.Method) int {
	n := 0
	for _, m := range ms {
		if !isExportedName(m.Name) {
			break
		}
		n++
	}
	return n
}

// uncommonMethods returns the internal/abi.Method array the UncommonType's Moff
// points at, which starts at base within the descriptor.
//
// # Which function each of the two code pointers names
//
// Tfn is reached by an ordinary method call and takes the receiver the method
// was declared with. Ifn is reached through an itab and takes a one-word
// receiver, which is the value itself when the type is stored directly in an
// interface and a pointer to it otherwise. gc decides both with one call,
// methodWrapper(rcvr, m, forItab), which replaces rcvr with *rcvr when the
// call is an itab call and rcvr is not direct-interface, and then spells the
// method symbol of that receiver. The same two lines are here:
//
//	descriptor  receiver of M  Tfn         Ifn
//	T           value          T.M         T.M when T is one pointer word,
//	                                       else (*T).M
//	*T          value          (*T).M      (*T).M
//	*T          pointer        (*T).M      (*T).M
//
// A pointer receiver is not in T's own method set, so the row that would ask T
// for it does not exist. ssagen.MethodWrappers generates (*T).M for every value
// receiver method, so every name this writes is either a method the front end
// compiled or a wrapper the same object defines.
func uncommonMethods(t *ir.Type, base int) ([]byte, []Reloc, []Symbol, error) {
	ms, err := methodSet(t)
	if err != nil {
		return nil, nil, nil, err
	}
	recv := t
	if t.Kind == ir.Ptr {
		recv = t.Elem
	}
	// The descriptor's own receiver form. *T's rows name the pointer method
	// throughout; T's name the value method, and its Ifn names the pointer
	// method unless a value of T is its own interface word.
	tfnPtr := t.Kind == ir.Ptr
	ifnPtr := tfnPtr || !directIface(t)

	data := make([]byte, len(ms)*methodSize)
	var (
		relocs []Reloc
		syms   []Symbol
	)
	for i, m := range ms {
		if err := methodEmittable(t, m); err != nil {
			return nil, nil, nil, err
		}
		off := base + i*methodSize
		n := nameSymbol(m.Name, "", isExportedName(m.Name), false)
		syms = append(syms, n)
		relocs = append(relocs, Reloc{
			Off: int32(off + methodOffName), Size: 4, Type: obj.R_ADDROFF, Target: n.Name,
		})
		mtyp, err := ir.TypeSymbol(m.Sig)
		if err != nil {
			return nil, nil, nil, err
		}
		tfn, err := ir.MethodSymbol(recv, m, tfnPtr)
		if err != nil {
			return nil, nil, nil, err
		}
		ifn, err := ir.MethodSymbol(recv, m, ifnPtr)
		if err != nil {
			return nil, nil, nil, err
		}
		// The three R_METHODOFFs, in this order and with nothing between them.
		// cmd/link's deadcode pass reads them as one group and panics with
		// "expect three consecutive R_METHODOFF relocs" otherwise. obj sorts a
		// symbol's relocations by offset, so the order of the three is the
		// order of the three fields and not the order of these lines.
		relocs = append(relocs,
			Reloc{Off: int32(off + methodOffMtyp), Size: 4, Type: obj.R_METHODOFF, Target: mtyp},
			Reloc{Off: int32(off + methodOffIfn), Size: 4, Type: obj.R_METHODOFF, Target: ifn, GoFunc: true},
			Reloc{Off: int32(off + methodOffTfn), Size: 4, Type: obj.R_METHODOFF, Target: tfn, GoFunc: true},
		)
	}
	return data, relocs, syms, nil
}

// methodEmittable reports the reason one method's row cannot be filled in.
func methodEmittable(t *ir.Type, m ir.Method) error {
	if m.Name == "" {
		return fmt.Errorf("rtype: a method of %s has no name", t)
	}
	if m.Sig == nil {
		// Mtyp is a TypeOff to the descriptor of the method's type with the
		// receiver removed, and zero is a legal Mtyp meaning "unexported,
		// reflect may not call it". So a zero written for an absent signature
		// would be read as a fact rather than as a gap. ir.Converter carries
		// the signature of every method it converts, so this is reachable only
		// from an IR built below the type boundary by hand.
		return fmt.Errorf("rtype: the method %s of %s has no signature, which its Mtyp is an offset to", m.Name, t)
	}
	if !isExportedName(m.Name) && m.Pkg != "" && m.Pkg != uncommonPkgPath(t) {
		// gc encodes such a name with internal/abi.Name's bit 2 set and a
		// package-path offset after the name, which rtype/name.go does not
		// write. Without it reflect attributes the method to the type's own
		// package, and two unexported methods of one name from two packages
		// become one. It is reachable only by embedding a type from another
		// package that has an unexported method.
		return fmt.Errorf("rtype: the method %s of %s is unexported and declared in %s, "+
			"so its name needs a package path, which the name encoder does not write",
			m.Name, t, m.Pkg)
	}
	return nil
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
