// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package types2_test

import (
	"testing"

	. "golang.design/x/nanogo/types2"
)

// substSrc declares one generic function whose signature reaches every shape a
// substitution has to walk through, and one generic defined type, so that a
// test can ask for the type of a name and get a type with a type parameter
// somewhere inside it.
const substSrc = `package p

type List[T any] struct{ head *node[T] }

type node[T any] struct {
	val  T
	next *node[T]
}

func F[K comparable, V any](m map[K][]*V, f func(K) V, l List[V]) (V, error) {
	var zero V
	return zero, nil
}
`

// substFixture type-checks substSrc and returns the checked package.
func substFixture(t *testing.T) *Package {
	t.Helper()
	pkg, err := typecheck(substSrc, nil, &Info{})
	if err != nil {
		t.Fatalf("the fixture does not type-check: %v", err)
	}
	return pkg
}

// lookupSig returns the signature of the package-level function named name.
func lookupSig(t *testing.T, pkg *Package, name string) *Signature {
	t.Helper()
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		t.Fatalf("%s is not declared", name)
	}
	sig, ok := obj.Type().(*Signature)
	if !ok {
		t.Fatalf("%s is a %T and not a function", name, obj.Type())
	}
	return sig
}

// TestSubstitutionReplacesThroughEveryTypeShape is the property the stenciler
// of specs/013-generics.md rests on: a type parameter anywhere inside a type
// is replaced, not only one that is the whole type.
//
// Instantiate cannot do this. It takes a generic *Alias, *Named or *Signature
// and the parameter list of F holds map[K][]*V, func(K) V and List[V], none of
// which is generic in its own right.
func TestSubstitutionReplacesThroughEveryTypeShape(t *testing.T) {
	pkg := substFixture(t)
	sig := lookupSig(t, pkg, "F")

	tparams := TypeParamsOf(sig)
	if len(tparams) != 2 {
		t.Fatalf("F has %d type parameters, want 2", len(tparams))
	}
	s := NewSubstitution(nil, tparams, []Type{Typ[String], Typ[Int]})

	for i, want := range []string{
		"map[string][]*int",
		"func(string) int",
		"p.List[int]",
	} {
		got := TypeString(s.Type(sig.Params().At(i).Type()), nil)
		if got != want {
			t.Errorf("parameter %d substitutes to %s, want %s", i, got, want)
		}
	}
	if got, want := TypeString(s.Type(sig.Results().At(0).Type()), nil), "int"; got != want {
		t.Errorf("the first result substitutes to %s, want %s", got, want)
	}
	if got, want := TypeString(s.Type(sig.Results().At(1).Type()), nil), "error"; got != want {
		t.Errorf("the second result substitutes to %s, want %s", got, want)
	}
}

// TestSubstitutionSharesOneNamedThroughOneContext is why the Context is a
// parameter rather than a local.
//
// A consumer that caches an IR type by the checker type's pointer sees two
// types for List[int] when two substitutions each make their own, and a
// descriptor writer would then emit two descriptors for one type.
func TestSubstitutionSharesOneNamedThroughOneContext(t *testing.T) {
	pkg := substFixture(t)
	sig := lookupSig(t, pkg, "F")
	tparams := TypeParamsOf(sig)
	listType := sig.Params().At(2).Type()

	ctxt := NewContext()
	a := NewSubstitution(ctxt, tparams, []Type{Typ[String], Typ[Int]})
	b := NewSubstitution(ctxt, tparams, []Type{Typ[Bool], Typ[Int]})
	if a.Type(listType) != b.Type(listType) {
		t.Error("two substitutions over one Context produced two List[int] types")
	}

	c := NewSubstitution(NewContext(), tparams, []Type{Typ[Bool], Typ[Int]})
	if !Identical(a.Type(listType), c.Type(listType)) {
		t.Error("two substitutions over two Contexts produced types that are not identical")
	}
}

// TestSubstitutionInstantiatesTheMethodsOfANamed covers the half of
// substitution a stenciler calls a method through: node[V] is reached only
// from inside List[V], so the walk has to go through the defined type rather
// than stop at its name.
func TestSubstitutionInstantiatesTheMethodsOfANamed(t *testing.T) {
	pkg := substFixture(t)
	sig := lookupSig(t, pkg, "F")
	s := NewSubstitution(nil, TypeParamsOf(sig), []Type{Typ[String], Typ[Int]})

	list, ok := s.Type(sig.Params().At(2).Type()).(*Named)
	if !ok {
		t.Fatalf("List[V] substitutes to a %T", s.Type(sig.Params().At(2).Type()))
	}
	st, ok := list.Underlying().(*Struct)
	if !ok {
		t.Fatalf("the underlying type of List[int] is a %T", list.Underlying())
	}
	if got, want := TypeString(st.Field(0).Type(), nil), "*p.node[int]"; got != want {
		t.Errorf("List[int].head is %s, want %s", got, want)
	}
}

// TestSubstitutionOfNothingIsTheIdentity pins the two cheap cases, because a
// builder calls Type on every type it reads and most of them hold no type
// parameter at all.
func TestSubstitutionOfNothingIsTheIdentity(t *testing.T) {
	pkg := substFixture(t)
	sig := lookupSig(t, pkg, "F")

	var none *Substitution
	if !none.Empty() {
		t.Error("a nil Substitution is not empty")
	}
	if none.Type(Typ[Int]) != Typ[Int] {
		t.Error("a nil Substitution changed a type")
	}
	if got := NewSubstitution(nil, nil, nil); !got.Empty() {
		t.Error("a Substitution over no type parameter is not empty")
	}

	s := NewSubstitution(nil, TypeParamsOf(sig), []Type{Typ[String], Typ[Int]})
	errType := sig.Results().At(1).Type()
	if s.Type(errType) != errType {
		t.Error("a type holding no type parameter came back as a different type")
	}
}

// TestNewSubstitutionRefusesMismatchedLists states the one precondition, and
// states it as a panic rather than an error because every caller derives both
// lists from one instantiation the checker already resolved: a caller that can
// get this wrong has a bug and not a bad input.
func TestNewSubstitutionRefusesMismatchedLists(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewSubstitution accepted one type argument for two type parameters")
		}
	}()
	pkg := substFixture(t)
	NewSubstitution(nil, TypeParamsOf(lookupSig(t, pkg, "F")), []Type{Typ[Int]})
}

// TestTypeParamsOfReadsEveryGenericDeclaration covers the accessor, whose
// point is that a *TypeParamList is indexed and not ranged over.
func TestTypeParamsOfReadsEveryGenericDeclaration(t *testing.T) {
	pkg := substFixture(t)
	if got := len(TypeParamsOf(lookupSig(t, pkg, "F"))); got != 2 {
		t.Errorf("F has %d type parameters, want 2", got)
	}
	list := pkg.Scope().Lookup("List").Type()
	if got := len(TypeParamsOf(list)); got != 1 {
		t.Errorf("List has %d type parameters, want 1", got)
	}
	if got := TypeParamsOf(Typ[Int]); got != nil {
		t.Errorf("int has type parameters %v", got)
	}
	if got := TypeParamsOf(NewSlice(Typ[Int])); got != nil {
		t.Errorf("[]int has type parameters %v", got)
	}
}
