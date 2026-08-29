// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"

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

	// Pkg is the package the declaration belongs to. It is what tells a type
	// declared inside the declaration from one declared elsewhere.
	Pkg *types2.Package

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
	// A method names its receiver's type parameters through objects the
	// method declaration made, which are not the objects the receiver's
	// declaration made, so neither loop above finds one by identity. gc
	// resolves such a name by the declared position alone
	// (noder.writerDict.typeParamIndex), and the position is the answer in
	// the two dictionaries a receiver's type parameter can be named in: the
	// receiver's list of a generic method's own dictionary, and the type's
	// own list when the dictionary is the one a generic type shares with
	// its methods. specs/013-generics.md has the numbering.
	if i := tp.Index(); i >= 0 {
		if i < len(d.Receivers) {
			return len(d.Implicits) + i, true
		}
		if len(d.Receivers) == 0 && i < len(d.TypeParams) {
			return base + i, true
		}
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

// @@@ Allocating a derived type's slot

// Derive returns the dictionary slot a type is named by, and false when the
// type's identity does not depend on a type parameter.
//
// This is cmd/compile/internal/noder.pkgWriter.typIdx with the elements left
// out. gc allocates the slot of a type after it has written the element of
// every type that type names, so a composite type's slot follows the slots of
// its components, and the walk below is the order of gc's encoding rather than
// an order of this package's choosing. A slot numbered otherwise is a slot gc
// reads as a different type, and gc reads it without complaint.
//
// One allocator answers the body builder and the export data writer, so that
// the slot a body names and the slot the dictionary holds cannot disagree.
// The error is what neither can encode, and each reports it in its own shape.
func (d *Dict) Derive(typ types2.Type) (int, bool, error) {
	// A local alias is stripped to what it names, the way the type writer
	// strips it, so that two local aliases of one name are not one element.
	for {
		alias, ok := typ.(*types2.Alias)
		if !ok || isGlobal(alias.Obj()) {
			break
		}
		typ = alias.Rhs()
	}
	if slot, ok := d.derivedIdx[typ]; ok {
		return slot, true, nil
	}
	if d.nonDerived[typ] {
		return 0, false, nil
	}
	derived, err := d.walkDerived(typ)
	if err != nil {
		return 0, false, err
	}
	if !derived {
		if d.nonDerived == nil {
			d.nonDerived = make(map[types2.Type]bool)
		}
		d.nonDerived[typ] = true
		return 0, false, nil
	}
	slot := len(d.Derived)
	d.Derived = append(d.Derived, typ)
	if d.derivedIdx == nil {
		d.derivedIdx = make(map[types2.Type]int)
	}
	d.derivedIdx[typ] = slot
	return slot, true, nil
}

// derivedOf reports whether a type's identity depends on a type parameter,
// allocating its slot when it does.
func (d *Dict) derivedOf(typ types2.Type) (bool, error) {
	_, ok, err := d.Derive(typ)
	return ok, err
}

// walkDerived reports whether a type names a type parameter, allocating the
// slot of every derived type it names on the way.
//
// Every component is walked, and none is skipped once one has answered yes,
// because gc writes the element of each and the slots are allocated in that
// order.
func (d *Dict) walkDerived(typ types2.Type) (bool, error) {
	switch typ := typ.(type) {
	default:
		return false, fmt.Errorf("the type is a %T, which the format has no tag for", typ)

	case *types2.Basic:
		// byte and rune are written as a reference to their TypeName and
		// every other basic type as its kind. Neither names a type
		// parameter.
		return false, nil

	case *types2.Named:
		return d.walkNamed(typ.Obj(), typeSlice(typ.TypeArgs()))

	case *types2.Alias:
		return d.walkNamed(typ.Obj(), typeSlice(typ.TypeArgs()))

	case *types2.TypeParam:
		return true, nil

	case *types2.Array:
		return d.derivedOf(typ.Elem())

	case *types2.Chan:
		return d.derivedOf(typ.Elem())

	case *types2.Map:
		key, err := d.derivedOf(typ.Key())
		if err != nil {
			return false, err
		}
		elem, err := d.derivedOf(typ.Elem())
		return key || elem, err

	case *types2.Pointer:
		return d.derivedOf(typ.Elem())

	case *types2.Signature:
		return d.walkSignature(typ)

	case *types2.Slice:
		return d.derivedOf(typ.Elem())

	case *types2.Struct:
		derived := false
		for i := range typ.NumFields() {
			got, err := d.derivedOf(typ.Field(i).Type())
			if err != nil {
				return false, err
			}
			derived = derived || got
		}
		return derived, nil

	case *types2.Interface:
		// The canonical empty interface is written as a reference to the
		// TypeName any, and comparable's underlying interface as an
		// embedding of comparable. Neither names a type parameter.
		if anyName := types2.Universe.Lookup("any"); anyName != nil &&
			types2.Unalias(typ) == types2.Unalias(anyName.Type()) {
			return false, nil
		}
		if typ.NumEmbeddeds() == 0 && !typ.IsMethodSet() {
			return false, nil
		}
		derived := false
		// A method's signature is written inline, so the signature gets no
		// slot of its own and only the types it names do.
		for i := range typ.NumExplicitMethods() {
			sig, ok := typ.ExplicitMethod(i).Type().(*types2.Signature)
			if !ok {
				return false, fmt.Errorf("a method of an interface has no signature")
			}
			got, err := d.walkSignature(sig)
			if err != nil {
				return false, err
			}
			derived = derived || got
		}
		for i := range typ.NumEmbeddeds() {
			got, err := d.derivedOf(typ.EmbeddedType(i))
			if err != nil {
				return false, err
			}
			derived = derived || got
		}
		return derived, nil

	case *types2.Union:
		derived := false
		for i := range typ.Len() {
			got, err := d.derivedOf(typ.Term(i).Type())
			if err != nil {
				return false, err
			}
			derived = derived || got
		}
		return derived, nil
	}
}

// walkSignature walks the parameters and the results of a signature, which is
// what the signature's encoding names and all of it.
func (d *Dict) walkSignature(sig *types2.Signature) (bool, error) {
	derived := false
	for _, tuple := range []*types2.Tuple{sig.Params(), sig.Results()} {
		for i := range tuple.Len() {
			got, err := d.derivedOf(tuple.At(i).Type())
			if err != nil {
				return false, err
			}
			derived = derived || got
		}
	}
	return derived, nil
}

// walkNamed walks a use of a defined type or of a global alias, which names
// the declaration and its type arguments.
func (d *Dict) walkNamed(obj *types2.TypeName, targs []types2.Type) (bool, error) {
	// A type declared inside a generic declaration carries that
	// declaration's type parameters implicitly, so every use of it is
	// derived and the dictionary holds them ahead of the declaration's own.
	// Neither the builder nor the writer keeps a record of which declaration
	// a local type was declared in, so a use of one is refused rather than
	// read as a type that needs no slot.
	if d.Generic() && obj.Pkg() != nil && obj.Pkg() == d.Pkg && !isGlobal(obj) {
		return false, fmt.Errorf("%s is declared inside a generic declaration, and every use of it carries the enclosing type parameters", obj.Name())
	}
	derived := false
	for _, t := range targs {
		got, err := d.derivedOf(t)
		if err != nil {
			return false, err
		}
		derived = derived || got
	}
	return derived, nil
}
