// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"golang.design/x/nanogo/types2"
)

// The object dictionary of one declaration.
//
// A generic declaration's body names types whose identity depends on the
// enclosing type parameters, and it names runtime values that only exist once
// the type arguments are known. Neither can be an element of the package,
// because there is one element per type and there is one such type per
// instantiation. The format puts them in a dictionary instead: the body names
// a slot, and the reader resolves the slot against the type arguments the
// instantiation supplied.
//
// This is cmd/compile/internal/noder.writerDict, with the elements left out.
// gc allocates a slot while it writes the bitstream, so the slot numbers are
// a property of the order gc's writer walks a declaration in. A slot the
// builder numbers differently is a slot gc reads as another type, and gc
// reads it without complaint, so the numbering is the whole of the problem.
//
// The five lists are what gc's objDict writes and what the four call sites
// the body encoder has fill:
//
//	rtype and varDictIndex -> RTypes
//	itab, which convRTTI is a use of -> Itabs
//	a call of an instantiated function or method -> Subdicts
//	a method expression on a type parameter -> MethodExprs
//
// Derived is not a runtime list. It is the type section of the dictionary:
// every type the declaration names whose identity depends on a type
// parameter, in the order gc writes their elements.
type Dict struct {
	// Implicits are the type parameters of an enclosing declaration, which
	// only a type declared inside a generic function has. Receivers are the
	// receiver's type parameters, which only a generic method has, and
	// TypeParams are the declaration's own.
	//
	// The three are one numbering, in that order, which is what a reference
	// to a type parameter is written in. See [Dict.TypeParamIndex].
	Implicits  []*types2.TypeParam
	Receivers  []*types2.TypeParam
	TypeParams []*types2.TypeParam

	// Derived is every type the declaration names whose identity depends on
	// a type parameter, in allocation order.
	Derived []types2.Type

	// MethodExprs, Subdicts, RTypes and Itabs are the runtime lists, in
	// allocation order. Each is a slot of the dictionary an instantiation
	// is passed.
	MethodExprs []MethodExprSlot
	Subdicts    []ObjUse
	RTypes      []TypeUse
	Itabs       []ItabSlot

	// derivedIdx is Derived by type identity. A composite type is keyed by
	// the value the checker built for it, so two spellings of one type that
	// the checker did not share are two slots, which is what gc does.
	derivedIdx map[types2.Type]int

	// nonDerived remembers a type whose identity does not depend on a type
	// parameter, so that the walk visits one type once. Nothing is
	// allocated under such a type, so remembering the answer changes
	// nothing but the time.
	nonDerived map[types2.Type]bool
}

// A MethodExprSlot is a method named on a type parameter, which the body
// reaches through the dictionary because the receiver's method is not known
// until the type argument is.
type MethodExprSlot struct {
	// TypeParam is the type parameter's index in the dictionary's own
	// numbering, which [Dict.TypeParamIndex] gives.
	TypeParam int
	Sel       Selector
}

// An ItabSlot is the pair of types an interface conversion compares.
type ItabSlot struct {
	Type  TypeUse
	Iface TypeUse
}

// Generic reports whether the declaration the dictionary belongs to has any
// type parameter, which is the only case in which a slot can be named.
func (d *Dict) Generic() bool {
	return len(d.Implicits)+len(d.Receivers)+len(d.TypeParams) != 0
}

// TypeParamIndex returns the index a reference to tp is written as.
//
// The enclosing declaration's type parameters come first, then the
// receiver's, and the declaration's own follow at the position they were
// declared in. A type parameter that is none of the three cannot be named by
// this declaration, and the index is then out of range for its dictionary,
// which is why it is a refusal rather than a fallthrough.
func (d *Dict) TypeParamIndex(tp *types2.TypeParam) (int, bool) {
	for i, t := range d.Implicits {
		if t == tp {
			return i, true
		}
	}
	for i, t := range d.Receivers {
		if t == tp {
			return len(d.Implicits) + i, true
		}
	}
	base := len(d.Implicits) + len(d.Receivers)
	for i, t := range d.TypeParams {
		if t == tp {
			return base + i, true
		}
	}
	// A generic method names its receiver's type parameters through the
	// receiver declaration's own objects, which are not the objects the
	// type declared. gc falls back on the declared index for exactly that
	// case, so an index inside the receiver's list is the answer.
	if i := tp.Index(); i >= 0 && i < len(d.Receivers) && len(d.Receivers) != 0 {
		return len(d.Implicits) + i, true
	}
	return 0, false
}

// RTypeIndex returns the slot holding t's runtime type descriptor, adding it
// if it is new.
func (d *Dict) RTypeIndex(t TypeUse) int {
	for i, have := range d.RTypes {
		if sameTypeUse(have, t) {
			return i
		}
	}
	d.RTypes = append(d.RTypes, t)
	return len(d.RTypes) - 1
}

// ItabIndex returns the slot holding the method table for typ as iface,
// adding it if it is new.
func (d *Dict) ItabIndex(typ, iface TypeUse) int {
	for i, have := range d.Itabs {
		if sameTypeUse(have.Type, typ) && sameTypeUse(have.Iface, iface) {
			return i
		}
	}
	d.Itabs = append(d.Itabs, ItabSlot{Type: typ, Iface: iface})
	return len(d.Itabs) - 1
}

// SubdictIndex returns the slot holding the dictionary of one instantiation,
// adding it if it is new.
func (d *Dict) SubdictIndex(use ObjUse) int {
	for i, have := range d.Subdicts {
		if sameObjUse(have, use) {
			return i
		}
	}
	d.Subdicts = append(d.Subdicts, use)
	return len(d.Subdicts) - 1
}

// MethodExprIndex returns the slot holding the method a type parameter's
// method expression resolves to, adding it if it is new.
func (d *Dict) MethodExprIndex(tp int, sel Selector) int {
	for i, have := range d.MethodExprs {
		if have.TypeParam == tp && have.Sel == sel {
			return i
		}
	}
	d.MethodExprs = append(d.MethodExprs, MethodExprSlot{TypeParam: tp, Sel: sel})
	return len(d.MethodExprs) - 1
}

// sameTypeUse reports whether two references name the same slot or the same
// type element.
//
// A derived reference is the slot it names. An ordinary one is the type the
// checker built, by identity and not by [types2.Identical], because the
// element a type goes in is interned by identity too: two structurally equal
// types the checker did not share are two elements to gc, and so two slots.
func sameTypeUse(a, b TypeUse) bool {
	if a.Derived != b.Derived {
		return false
	}
	if a.Derived {
		return a.Idx == b.Idx
	}
	return a.Type == b.Type
}

// sameObjUse reports whether two references name the same declaration at the
// same instantiation.
func sameObjUse(a, b ObjUse) bool {
	if a.Obj != b.Obj || len(a.Targs) != len(b.Targs) {
		return false
	}
	for i := range a.Targs {
		if !sameTypeUse(a.Targs[i], b.Targs[i]) {
			return false
		}
	}
	return true
}
