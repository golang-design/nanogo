// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Substitution for the stenciler of specs/013-generics.md.
//
// The checker substitutes type parameters already, in subst.go, and it does so
// through an unexported method on a *Checker that the caller cannot reach.
// Instantiate is the only exported door and it takes a generic *Alias, *Named
// or *Signature, so it cannot substitute through []T, map[K]V, *T or func(T),
// which is most of what the body of a generic function is made of.
//
// So the door is here and the room is upstream's. Nothing below re-derives
// substitution: Substitution.Type calls the same method Instantiate calls,
// with a nil *Checker, which is the mode subst.go is already written for.

package types2

// A Substitution replaces the type parameters of one generic declaration with
// the type arguments of one instantiation.
//
// One Substitution stands for one instantiation. It is immutable and it may be
// used from one goroutine at a time, because the *Context it holds is where
// two substitutions producing one instantiated defined type agree on one
// *Named. Two Substitutions built from one Context therefore return pointer
// equal types for List[int], which is what a consumer that caches by type
// pointer needs.
type Substitution struct {
	smap substMap
	ctxt *Context
}

// NewSubstitution returns the substitution that replaces tparams[i] by
// targs[i].
//
// ctxt may be nil, and a fresh Context is made for the Substitution alone. A
// caller that makes more than one Substitution should pass one Context to all
// of them, so that a defined type instantiated by two of them is one type.
//
// The lists must be the same length. A nil Substitution, and one over no type
// parameters, are both the identity.
func NewSubstitution(ctxt *Context, tparams []*TypeParam, targs []Type) *Substitution {
	if len(tparams) != len(targs) {
		panic("types2: NewSubstitution needs one type argument per type parameter")
	}
	if ctxt == nil {
		ctxt = NewContext()
	}
	return &Substitution{smap: makeSubstMap(tparams, targs), ctxt: ctxt}
}

// TypeParamsOf returns the type parameters of a generic declaration's type as
// a slice, and nil for a type that has none.
//
// It exists because *TypeParamList is indexed and not ranged over, and every
// caller of NewSubstitution would otherwise write the same loop.
func TypeParamsOf(t Type) []*TypeParam {
	var list *TypeParamList
	switch t := t.(type) {
	case *Signature:
		list = t.TypeParams()
	case *Named:
		list = t.TypeParams()
	case *Alias:
		list = t.TypeParams()
	default:
		return nil
	}
	if list.Len() == 0 {
		return nil
	}
	out := make([]*TypeParam, list.Len())
	for i := range out {
		out[i] = list.At(i)
	}
	return out
}

// Type returns t with every type parameter replaced by its type argument.
//
// t is not modified. A type holding no type parameter of this substitution is
// returned unchanged, so the result is pointer equal to t whenever nothing was
// substituted.
func (s *Substitution) Type(t Type) Type {
	if s == nil || t == nil || s.smap.empty() {
		return t
	}
	return (*Checker)(nil).subst(nopos, t, s.smap, nil, s.ctxt)
}

// Empty reports whether the substitution replaces nothing.
func (s *Substitution) Empty() bool { return s == nil || s.smap.empty() }
