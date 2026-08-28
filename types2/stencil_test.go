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

// canonSrc names one alias and the shapes a type argument can be built out of,
// so that a test can ask for a type holding an alias below the top level.
const canonSrc = `package q

type Alias = int

type List[T any] struct{ v T }

type Str struct {
	F Alias
	G string
}

type Iface interface{ M(Alias) Alias }

var (
	A  Alias
	P  *Alias
	S  []Alias
	R  [3]Alias
	C  chan Alias
	M  map[Alias][]*Alias
	F  func(Alias) (Alias, error)
	L  List[Alias]
	L2 List[int]
	N  List[[]Alias]
	St Str
	I  interface{ M(Alias) Alias }
	D  struct{ X Alias }
)
`

// canonType returns the type of the package-level variable named name.
func canonType(t *testing.T, pkg *Package, name string) Type {
	t.Helper()
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		t.Fatalf("%s is not declared", name)
	}
	return obj.Type()
}

// TestCanonicalResolvesAnAliasAtEveryDepth is the property a linker symbol
// rests on.
//
// "type Alias = int" makes Alias and int identical, so an instantiation
// written at Alias and one written at int are one instantiation. A name
// derived from the spelling rather than from the identity would give one body
// two symbols and put two copies of it in the program.
func TestCanonicalResolvesAnAliasAtEveryDepth(t *testing.T) {
	pkg := mustTypecheck(canonSrc, nil, &Info{})
	ctxt := NewContext()
	for _, tt := range []struct{ name, want string }{
		{"A", "int"},
		{"P", "*int"},
		{"S", "[]int"},
		{"R", "[3]int"},
		{"C", "chan int"},
		{"M", "map[int][]*int"},
		{"F", "func(int) (int, error)"},
		{"L", "q.List[int]"},
		{"N", "q.List[[]int]"},
		{"D", "struct{X int}"},
		{"I", "interface{M(int) int}"},
	} {
		got := TypeString(Canonical(ctxt, canonType(t, pkg, tt.name)), nil)
		if got != tt.want {
			t.Errorf("%s canonicalises to %s, want %s", tt.name, got, tt.want)
		}
	}
}

// TestCanonicalIsTheIdentityOnATypeWithNoAlias pins that a type holding no
// alias is returned and not rebuilt, because the naming function calls this on
// every type argument and almost none of them holds one. An instantiated
// defined type is the exception, and the test below says why.
func TestCanonicalIsTheIdentityOnATypeWithNoAlias(t *testing.T) {
	pkg := mustTypecheck(canonSrc, nil, &Info{})
	ctxt := NewContext()
	for _, name := range []string{"St"} {
		have := canonType(t, pkg, name)
		if got := Canonical(ctxt, have); got != have {
			t.Errorf("%s was rebuilt: %s", name, TypeString(got, nil))
		}
	}
	if Canonical(ctxt, nil) != nil {
		t.Error("Canonical of no type returned a type")
	}
	if got := Canonical(nil, Typ[Int]); got != Typ[Int] {
		t.Errorf("Canonical with no Context returned %s", TypeString(got, nil))
	}
}

// TestCanonicalGivesOneNamedForOneInstantiation is why the Context is a
// parameter: List[Alias] and List[int] are one type, and a caller that keys an
// IR type by the checker type's pointer must get one answer for both.
func TestCanonicalGivesOneNamedForOneInstantiation(t *testing.T) {
	pkg := mustTypecheck(canonSrc, nil, &Info{})
	ctxt := NewContext()
	aliased := Canonical(ctxt, canonType(t, pkg, "L"))
	plain := Canonical(ctxt, canonType(t, pkg, "L2"))
	if aliased != plain {
		t.Errorf("List[Alias] canonicalises to %p and List[int] to %p", aliased, plain)
	}
	if !Identical(aliased, plain) {
		t.Error("List[Alias] and List[int] canonicalise to types that are not identical")
	}
}

// TestSubstitutesSaysWhereSubstitutionStops is why the method exists.
//
// Substitution rebuilds a type literal and stops at a name, so a defined type
// with no type parameters of its own comes back unchanged with the type
// parameter still inside it. A consumer that assumed substitution is total
// would give two instantiations one type.
func TestSubstitutesSaysWhereSubstitutionStops(t *testing.T) {
	pkg := substFixture(t)
	sig := lookupSig(t, pkg, "F")
	tparams := TypeParamsOf(sig)
	s := NewSubstitution(nil, tparams, []Type{Typ[String], Typ[Int]})

	// The parameter types hold the type parameters, and their substituted
	// forms do not.
	for i := 0; i < sig.Params().Len(); i++ {
		have := sig.Params().At(i).Type()
		if !s.Substitutes(have) {
			t.Errorf("parameter %d holds no type parameter of the substitution", i)
		}
		if s.Substitutes(s.Type(have)) {
			t.Errorf("parameter %d still holds one after the substitution", i)
		}
	}

	// A defined type is its name, so the answer for List[V] is about the type
	// argument it carries. The declaration's own body is reached through its
	// underlying type, which is what a caller asking about a declaration
	// passes.
	list := sig.Params().At(2).Type().(*Named)
	if !s.Substitutes(list) {
		t.Error("List[V] holds no type parameter of the substitution")
	}
	if s.Substitutes(list.Origin()) {
		t.Error("List, with no type arguments, was reported as holding one")
	}
	if !s.Substitutes(list.Origin().Underlying()) {
		t.Error("the underlying type of List holds no type parameter")
	}

	var none *Substitution
	if none.Substitutes(sig.Params().At(0).Type()) {
		t.Error("a nil Substitution substitutes something")
	}
	if s.Substitutes(nil) {
		t.Error("no type holds a type parameter")
	}
}
