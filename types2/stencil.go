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

// Canonical returns t with every alias resolved.
//
// An alias is not a type. "type Alias = int" makes Alias and int identical
// under the language's identity rule, so a generic instantiated at Alias and
// one instantiated at int are one instantiation. A naming function that spelled
// the two differently would give one body two symbols, and the linker would
// keep two copies of it.
//
// The walk is over the type language and not over the type's contents. A
// defined type is its name, so Canonical recurses into its type arguments and
// leaves its underlying type alone, which is also why there is no cycle to
// guard against: every recursive type reaches itself through a name.
//
// ctxt is where a defined type rebuilt from canonical type arguments is
// deduplicated, so Canonical(List[Alias]) and Canonical(List[int]) return one
// *Named. It may be nil, and a fresh Context is used for the call alone.
func Canonical(ctxt *Context, t Type) Type {
	if t == nil {
		return nil
	}
	if ctxt == nil {
		ctxt = NewContext()
	}
	return canonical(ctxt, t)
}

func canonical(ctxt *Context, t Type) Type {
	switch t := t.(type) {
	case *Alias:
		return canonical(ctxt, Unalias(t))

	case *Basic, *TypeParam:
		return t

	case *Named:
		// A defined type is its name, so its underlying type is not walked.
		// An instantiated one is its name and its type arguments, and it is
		// rebuilt through ctxt even when no argument changed: that is what
		// makes List[Alias] and List[int] one *Named rather than two the
		// checker happened to create in two places.
		targs := t.TypeArgs()
		if targs.Len() == 0 {
			return t
		}
		list := make([]Type, targs.Len())
		for i := range list {
			list[i] = canonical(ctxt, targs.At(i))
		}
		out, err := Instantiate(ctxt, t.Origin(), list, false)
		if err != nil {
			return t
		}
		return out

	case *Pointer:
		if elem := canonical(ctxt, t.Elem()); elem != t.Elem() {
			return NewPointer(elem)
		}
		return t

	case *Slice:
		if elem := canonical(ctxt, t.Elem()); elem != t.Elem() {
			return NewSlice(elem)
		}
		return t

	case *Array:
		if elem := canonical(ctxt, t.Elem()); elem != t.Elem() {
			return NewArray(elem, t.Len())
		}
		return t

	case *Chan:
		if elem := canonical(ctxt, t.Elem()); elem != t.Elem() {
			return NewChan(t.Dir(), elem)
		}
		return t

	case *Map:
		key, elem := canonical(ctxt, t.Key()), canonical(ctxt, t.Elem())
		if key != t.Key() || elem != t.Elem() {
			return NewMap(key, elem)
		}
		return t

	case *Struct:
		fields := make([]*Var, t.NumFields())
		tags := make([]string, t.NumFields())
		changed := false
		for i := range fields {
			f := t.Field(i)
			ft := canonical(ctxt, f.Type())
			tags[i] = t.Tag(i)
			if ft == f.Type() {
				fields[i] = f
				continue
			}
			fields[i] = NewField(f.Pos(), f.Pkg(), f.Name(), ft, f.Embedded())
			changed = true
		}
		if !changed {
			return t
		}
		return NewStruct(fields, tags)

	case *Tuple:
		vars, changed := canonicalVars(ctxt, t)
		if !changed {
			return t
		}
		return NewTuple(vars...)

	case *Signature:
		params, pchanged := canonicalVars(ctxt, t.Params())
		results, rchanged := canonicalVars(ctxt, t.Results())
		if !pchanged && !rchanged {
			return t
		}
		return NewSignatureType(t.Recv(), nil, TypeParamsOf(t), NewTuple(params...), NewTuple(results...), t.Variadic())

	case *Interface:
		methods := make([]*Func, t.NumExplicitMethods())
		embeddeds := make([]Type, t.NumEmbeddeds())
		changed := false
		for i := range methods {
			m := t.ExplicitMethod(i)
			sig, _ := canonical(ctxt, m.Type()).(*Signature)
			if sig == nil || sig == m.Type() {
				methods[i] = m
				continue
			}
			methods[i] = NewFunc(m.Pos(), m.Pkg(), m.Name(), sig)
			changed = true
		}
		for i := range embeddeds {
			e := t.EmbeddedType(i)
			embeddeds[i] = canonical(ctxt, e)
			if embeddeds[i] != e {
				changed = true
			}
		}
		if !changed {
			return t
		}
		return NewInterfaceType(methods, embeddeds)

	case *Union:
		terms := make([]*Term, t.Len())
		changed := false
		for i := range terms {
			term := t.Term(i)
			ct := canonical(ctxt, term.Type())
			terms[i] = term
			if ct != term.Type() {
				terms[i] = NewTerm(term.Tilde(), ct)
				changed = true
			}
		}
		if !changed {
			return t
		}
		return NewUnion(terms)
	}
	return t
}

// canonicalVars returns the canonical form of a tuple's variables, and says
// whether any of them changed.
func canonicalVars(ctxt *Context, tup *Tuple) ([]*Var, bool) {
	if tup == nil {
		return nil, false
	}
	out := make([]*Var, tup.Len())
	changed := false
	for i := range out {
		v := tup.At(i)
		vt := canonical(ctxt, v.Type())
		out[i] = v
		if vt != v.Type() {
			out[i] = cloneVar(v, vt)
			changed = true
		}
	}
	return out, changed
}
