// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"golang.design/x/nanogo/types2"
)

// convCorpus is the type corpus, and it is written twice on purpose.
//
// src is the type as the type checker reads it, rt is the same type as the Go
// compiler that builds this test laid it out. Two independent oracles answer
// the same question: the type checker's gc sizes, and reflect on the running
// program. A conversion that agrees with both is right.
var convCorpus = []struct {
	src string
	rt  reflect.Type
}{
	{"bool", reflect.TypeOf((*bool)(nil)).Elem()},
	{"int8", reflect.TypeOf((*int8)(nil)).Elem()},
	{"int16", reflect.TypeOf((*int16)(nil)).Elem()},
	{"int32", reflect.TypeOf((*int32)(nil)).Elem()},
	{"int64", reflect.TypeOf((*int64)(nil)).Elem()},
	{"int", reflect.TypeOf((*int)(nil)).Elem()},
	{"uint8", reflect.TypeOf((*uint8)(nil)).Elem()},
	{"uint16", reflect.TypeOf((*uint16)(nil)).Elem()},
	{"uint32", reflect.TypeOf((*uint32)(nil)).Elem()},
	{"uint64", reflect.TypeOf((*uint64)(nil)).Elem()},
	{"uint", reflect.TypeOf((*uint)(nil)).Elem()},
	{"uintptr", reflect.TypeOf((*uintptr)(nil)).Elem()},
	{"float32", reflect.TypeOf((*float32)(nil)).Elem()},
	{"float64", reflect.TypeOf((*float64)(nil)).Elem()},
	{"complex64", reflect.TypeOf((*complex64)(nil)).Elem()},
	{"complex128", reflect.TypeOf((*complex128)(nil)).Elem()},
	{"string", reflect.TypeOf((*string)(nil)).Elem()},
	{"unsafe.Pointer", reflect.TypeOf((*unsafe.Pointer)(nil)).Elem()},
	{"*int", reflect.TypeOf((**int)(nil)).Elem()},
	{"**string", reflect.TypeOf((***string)(nil)).Elem()},
	{"[]byte", reflect.TypeOf((*[]byte)(nil)).Elem()},
	{"[]*int", reflect.TypeOf((*[]*int)(nil)).Elem()},
	{"[4]int", reflect.TypeOf((*[4]int)(nil)).Elem()},
	{"[0]int", reflect.TypeOf((*[0]int)(nil)).Elem()},
	{"[3][2]byte", reflect.TypeOf((*[3][2]byte)(nil)).Elem()},
	{"map[string]int", reflect.TypeOf((*map[string]int)(nil)).Elem()},
	{"chan int", reflect.TypeOf((*chan int)(nil)).Elem()},
	{"func(int) string", reflect.TypeOf((*func(int) string)(nil)).Elem()},
	{"any", reflect.TypeOf((*any)(nil)).Elem()},
	{"error", reflect.TypeOf((*error)(nil)).Elem()},
	{"struct{}", reflect.TypeOf((*struct{})(nil)).Elem()},
	{"struct{ A int }", reflect.TypeOf((*struct{ A int })(nil)).Elem()},
	{"struct{ A int8; B int64 }", reflect.TypeOf((*struct {
		A int8
		B int64
	})(nil)).Elem()},
	{"struct{ A int64; B int8 }", reflect.TypeOf((*struct {
		A int64
		B int8
	})(nil)).Elem()},
	{"struct{ A bool; B *int; C string }", reflect.TypeOf((*struct {
		A bool
		B *int
		C string
	})(nil)).Elem()},
	{"struct{ A [3]struct{ X *byte; Y int32 } }", reflect.TypeOf((*struct {
		A [3]struct {
			X *byte
			Y int32
		}
	})(nil)).Elem()},
	{"struct{ A struct{}; B int64 }", reflect.TypeOf((*struct {
		A struct{}
		B int64
	})(nil)).Elem()},
	{"struct{ A int8; B struct{} }", reflect.TypeOf((*struct {
		A int8
		B struct{}
	})(nil)).Elem()},
	{"struct{ A any; B []string; C map[int]int }", reflect.TypeOf((*struct {
		A any
		B []string
		C map[int]int
	})(nil)).Elem()},
	{"struct{ A complex128; B complex64 }", reflect.TypeOf((*struct {
		A complex128
		B complex64
	})(nil)).Elem()},
	{"struct{ A func(); B chan bool; C unsafe.Pointer }", reflect.TypeOf((*struct {
		A func()
		B chan bool
		C unsafe.Pointer
	})(nil)).Elem()},
}

// convCorpusTypes type-checks the corpus and returns the named types in the
// order of convCorpus.
func convCorpusTypes(t *testing.T) []*types2.Named {
	t.Helper()
	var b strings.Builder
	b.WriteString("package p\n\nimport \"unsafe\"\n\nvar _ unsafe.Pointer\n")
	for i, c := range convCorpus {
		fmt.Fprintf(&b, "type T%d %s\n", i, c.src)
	}
	pkg, _, _ := buildTypecheck(t, b.String())
	out := make([]*types2.Named, len(convCorpus))
	for i := range convCorpus {
		obj := pkg.Scope().Lookup(fmt.Sprintf("T%d", i))
		if obj == nil {
			t.Fatalf("T%d is not declared", i)
		}
		named, ok := obj.Type().(*types2.Named)
		if !ok {
			t.Fatalf("T%d is a %T, want a named type", i, obj.Type())
		}
		out[i] = named
	}
	return out
}

// TestConvertAgainstStdSizes checks every size, alignment and field offset
// against the type checker's own layout algorithm.
func TestConvertAgainstStdSizes(t *testing.T) {
	// types2.SizesFor("gc", "amd64") and not types2.StdSizes{WordSize: 8,
	// MaxAlign: 8}. StdSizes is the layout the specification permits and it
	// differs from the reference implementation twice: it does not round a
	// struct's size up to the struct's alignment, and it does not add the
	// padding byte after a trailing zero-size field. specs/030-abi.md takes
	// every rule from the reference implementation, so gc's sizes are the
	// oracle and reflect below is the second opinion that settles it.
	sizes := types2.SizesFor("gc", "amd64")
	c := NewConverter()
	for i, named := range convCorpusTypes(t) {
		src := convCorpus[i].src
		got, err := c.Convert(named)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if want := sizes.Sizeof(named); got.Size != want {
			t.Errorf("%s: size %d, want %d", src, got.Size, want)
		}
		if want := sizes.Alignof(named); got.Align != want {
			t.Errorf("%s: align %d, want %d", src, got.Align, want)
		}
		st, ok := named.Underlying().(*types2.Struct)
		if !ok {
			continue
		}
		fields := make([]*types2.Var, st.NumFields())
		for j := range fields {
			fields[j] = st.Field(j)
		}
		offs := sizes.Offsetsof(fields)
		if len(got.Fields) != len(offs) {
			t.Errorf("%s: %d fields, want %d", src, len(got.Fields), len(offs))
			continue
		}
		for j, f := range got.Fields {
			if f.Name != fields[j].Name() {
				t.Errorf("%s: field %d is %q, want %q", src, j, f.Name, fields[j].Name())
			}
			if f.Offset != offs[j] {
				t.Errorf("%s: field %s at %d, want %d", src, f.Name, f.Offset, offs[j])
			}
		}
	}
}

// TestConvertAgainstReflect checks the same corpus against the layout the
// reference implementation produced for the same types.
func TestConvertAgainstReflect(t *testing.T) {
	c := NewConverter()
	for i, named := range convCorpusTypes(t) {
		src := convCorpus[i].src
		rt := convCorpus[i].rt
		got, err := c.Convert(named)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if int64(rt.Size()) != got.Size {
			t.Errorf("%s: size %d, want %d from reflect", src, got.Size, rt.Size())
		}
		if int64(rt.Align()) != got.Align {
			t.Errorf("%s: align %d, want %d from reflect", src, got.Align, rt.Align())
		}
		if rt.Kind() == reflect.Struct {
			for j := 0; j < rt.NumField(); j++ {
				if int64(rt.Field(j).Offset) != got.Fields[j].Offset {
					t.Errorf("%s: field %s at %d, want %d from reflect",
						src, got.Fields[j].Name, got.Fields[j].Offset, rt.Field(j).Offset)
				}
			}
		}
		// The pointer map is the field a mistake in is memory corruption, so
		// it gets its own oracle: the words reflect says hold pointers.
		want := make(map[int64]bool)
		convReflectPtrWords(t, rt, 0, want)
		for w := int64(0); w < got.Size/PtrSize+1; w++ {
			if bitSet(got.PtrBits, w) != want[w] {
				t.Errorf("%s: word %d is a pointer=%v, reflect says %v",
					src, w, bitSet(got.PtrBits, w), want[w])
			}
		}
	}
}

// convReflectPtrWords records which pointer-sized words of rt hold pointers,
// reading the answer off reflect rather than off the code under test.
func convReflectPtrWords(t *testing.T, rt reflect.Type, base int64, set map[int64]bool) {
	t.Helper()
	switch rt.Kind() {
	case reflect.Pointer, reflect.UnsafePointer, reflect.Map, reflect.Chan, reflect.Func:
		set[base/PtrSize] = true
	case reflect.String, reflect.Slice:
		set[base/PtrSize] = true // the data pointer, then the length and cap
	case reflect.Interface:
		set[base/PtrSize] = true
		set[base/PtrSize+1] = true
	case reflect.Array:
		for i := 0; i < rt.Len(); i++ {
			convReflectPtrWords(t, rt.Elem(), base+int64(i)*int64(rt.Elem().Size()), set)
		}
	case reflect.Struct:
		for i := 0; i < rt.NumField(); i++ {
			convReflectPtrWords(t, rt.Field(i).Type, base+int64(rt.Field(i).Offset), set)
		}
	}
}

// TestConvertPtrBitsStayInsideTheType is the invariant the collector depends
// on: a pointer word must lie inside the object it describes.
func TestConvertPtrBitsStayInsideTheType(t *testing.T) {
	c := NewConverter()
	for i, named := range convCorpusTypes(t) {
		got, err := c.Convert(named)
		if err != nil {
			t.Fatalf("%s: %v", convCorpus[i].src, err)
		}
		if pb := got.PtrBytes(); pb > got.Size {
			t.Errorf("%s: PtrBytes %d exceeds size %d", convCorpus[i].src, pb, got.Size)
		}
		for w := int64(0); w < int64(len(got.PtrBits))*8; w++ {
			if bitSet(got.PtrBits, w) && (w+1)*PtrSize > got.Size {
				t.Errorf("%s: pointer word %d lies past size %d", convCorpus[i].src, w, got.Size)
			}
		}
	}
}

// TestConvertMemoisesThroughACycle is the reason the cache entry is installed
// before the recursion. Without it this test does not fail, it does not
// return.
func TestConvertMemoisesThroughACycle(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type node struct {
	next  *node
	left  *node
	value int
}

type ring struct {
	self *ring
	m    map[string]*ring
	s    []ring2
}

type ring2 struct{ back *ring }
`)
	c := NewConverter()
	obj := pkg.Scope().Lookup("node")
	got, err := c.Convert(obj.Type())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got.Size != 24 || got.Align != 8 {
		t.Errorf("node is %d/%d, want 24/8", got.Size, got.Align)
	}
	// One IR type per type checker type, and the cycle closes on it.
	if got.Fields[0].Type.Elem != got {
		t.Error("the pointer field's element is not the memoised type")
	}
	// The type checker builds one *types2.Pointer per written *node, so the
	// two fields are two IR pointer types. What must be shared is what the
	// cycle runs through: the pointee.
	if got.Fields[1].Type.Elem != got {
		t.Error("the second pointer field's element is not the memoised type")
	}
	again, err := c.Convert(obj.Type())
	if err != nil || again != got {
		t.Errorf("a second Convert returned %p, %v; want the cached %p", again, err, got)
	}

	r, err := c.Convert(pkg.Scope().Lookup("ring").Type())
	if err != nil {
		t.Fatalf("Convert ring: %v", err)
	}
	if r.Fields[2].Type.Elem.Fields[0].Type.Elem != r {
		t.Error("the cycle through a slice element did not close on the memoised type")
	}
}

// TestConvertNames checks the names carried for diagnostics. Nothing below the
// IR may branch on them, and a message that names the wrong type is the reason
// they are carried at all.
func TestConvertNames(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type Celsius float64
type Alias = Celsius
type Table map[string][]Celsius
`)
	c := NewConverter()
	celsius, err := c.Convert(pkg.Scope().Lookup("Celsius").Type())
	if err != nil {
		t.Fatal(err)
	}
	if celsius.Name != "p.Celsius" || celsius.Kind != Float64 {
		t.Errorf("Celsius is %s/%s, want p.Celsius/float64", celsius.Name, celsius.Kind)
	}
	alias, err := c.Convert(pkg.Scope().Lookup("Alias").Type())
	if err != nil {
		t.Fatal(err)
	}
	if alias != celsius {
		t.Error("an alias converted to a second type; it names one type")
	}
	table, err := c.Convert(pkg.Scope().Lookup("Table").Type())
	if err != nil {
		t.Fatal(err)
	}
	if table.Kind != Map || table.Key.Kind != String || table.Elem.Elem != celsius {
		t.Errorf("Table is %s, want map[string][]p.Celsius", table)
	}
}

// TestConvertUnsafePointerIsNotAPointer pins the distinction the collector
// depends on. An unsafe.Pointer may point into the middle of an object.
func TestConvertUnsafePointerIsNotAPointer(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

import "unsafe"

type P unsafe.Pointer
type Q *int
`)
	c := NewConverter()
	p, err := c.Convert(pkg.Scope().Lookup("P").Type())
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != UnsafePtr {
		t.Errorf("unsafe.Pointer is %s, want %s", p.Kind, UnsafePtr)
	}
	// The checker calls the type Pointer, because that is its name inside its
	// package. A diagnostic wants the name a reader wrote.
	if got := c.mustConvert(t, types2.Typ[types2.UnsafePointer]).Name; got != "unsafe.Pointer" {
		t.Errorf("the name of unsafe.Pointer is %q", got)
	}
	q, err := c.Convert(pkg.Scope().Lookup("Q").Type())
	if err != nil {
		t.Fatal(err)
	}
	if q.Kind != Ptr {
		t.Errorf("*int is %s, want %s", q.Kind, Ptr)
	}
}

// mustConvert converts a type or fails.
func (c *Converter) mustConvert(t *testing.T, typ types2.Type) *Type {
	t.Helper()
	out, err := c.Convert(typ)
	if err != nil {
		t.Fatalf("Convert(%s): %v", typ, err)
	}
	return out
}

// TestConvertFuncIsFuncKind. Func is the function being compiled; the kind is
// FuncKind, and it is one word because a function value is a closure pointer.
func TestConvertFuncIsFuncKind(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type F func(int, string) (bool, error)
`)
	c := NewConverter()
	f, err := c.Convert(pkg.Scope().Lookup("F").Type())
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != FuncKind || f.Size != PtrSize || !f.HasPointers() {
		t.Errorf("F is %s size %d pointers=%v, want func/8/true", f.Kind, f.Size, f.HasPointers())
	}
}

// TestConvertUntypedTakesTheDefaultShape. An untyped constant reaches the
// builder with its own basic type, and its machine shape is the default type's.
func TestConvertUntypedTakesTheDefaultShape(t *testing.T) {
	c := NewConverter()
	for _, tc := range []struct {
		basic types2.BasicKind
		want  Kind
	}{
		{types2.UntypedBool, Bool},
		{types2.UntypedInt, Int64},
		{types2.UntypedRune, Int32},
		{types2.UntypedFloat, Float64},
		{types2.UntypedComplex, Complex128},
		{types2.UntypedString, String},
	} {
		got, err := c.Convert(types2.Typ[tc.basic])
		if err != nil {
			t.Errorf("%v: %v", tc.basic, err)
			continue
		}
		if got.Kind != tc.want {
			t.Errorf("%s is %s, want %s", types2.Typ[tc.basic], got.Kind, tc.want)
		}
	}
}

// TestConvertRejectsTypesWithNoRepresentation. A type parameter reaching the
// builder means an uninstantiated body did, and a tuple is not the type of a
// value.
func TestConvertRejectsTypesWithNoRepresentation(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

func F[T any](x T) (int, string) { return 0, "" }

type C interface{ ~int | ~string }
`)
	c := NewConverter()
	sig := pkg.Scope().Lookup("F").Type().(*types2.Signature)
	tp := sig.TypeParams().At(0)
	if _, err := c.Convert(tp); err == nil {
		t.Error("a type parameter converted; specs/013 instantiates before the IR")
	}
	if _, err := c.Convert(sig.Results()); err == nil {
		t.Error("a tuple converted through Convert; it must go through Tuple")
	}
	if _, err := c.Convert(nil); err == nil {
		t.Error("a nil type converted")
	}
	iface := pkg.Scope().Lookup("C").Type().Underlying().(*types2.Interface)
	if _, err := c.Convert(iface.EmbeddedType(0)); err == nil {
		t.Error("a constraint union converted")
	}
	// A failed conversion leaves nothing behind: the cache entry installed
	// before the recursion is removed, so a later good conversion is not
	// served a half-built type.
	if _, err := c.Convert(pkg.Scope().Lookup("F").Type()); err != nil {
		t.Errorf("the signature failed after a failed conversion: %v", err)
	}
}

// TestTupleIsNotTheABI. The result struct is a place to hang a type on a
// multi-value call. specs/030-abi.md owns how results are passed.
func TestTupleIsNotTheABI(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

func f() (int, string, error) { return 0, "", nil }
func g()                      {}
`)
	c := NewConverter()
	sig := pkg.Scope().Lookup("f").Type().(*types2.Signature)
	tup, err := c.Tuple(sig.Results())
	if err != nil {
		t.Fatal(err)
	}
	if tup.Kind != Tuple || len(tup.Fields) != 3 {
		t.Fatalf("results are %s, want a tuple of 3", tup)
	}
	if tup.Fields[0].Name != "r0" || tup.Fields[2].Type.Kind != Interface {
		t.Errorf("results are %s, want r0 int, r1 string, r2 an interface", tup)
	}
	again, err := c.Tuple(sig.Results())
	if err != nil || again != tup {
		t.Error("Tuple did not memoise")
	}
	empty, err := c.Tuple(pkg.Scope().Lookup("g").Type().(*types2.Signature).Results())
	if err != nil || empty.Size != 0 || empty.Kind != Tuple {
		t.Errorf("an empty result tuple is %v, %v; want a zero-size tuple", empty, err)
	}
	// types2 spells an empty result list as a nil *Tuple, and both spellings
	// must give the same zero-size struct.
	if nilTup, err := c.Tuple(nil); err != nil || nilTup.Size != 0 {
		t.Errorf("a nil tuple is %v, %v; want a zero-size tuple", nilTup, err)
	}
}

// TestConvertPropagatesErrors checks that a type with no run-time
// representation is reported wherever it is reached from, rather than
// producing a type with a hole in it.
func TestConvertPropagatesErrors(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

func G[T comparable](
	a *T,
	b []T,
	c [4]T,
	d map[T]int,
	e map[int]T,
	f chan T,
	g struct{ X T },
	h [2][2]T,
) (int, T) {
	var zero T
	return 0, zero
}

func a2[T any]() (T, T) { var zero T; return zero, zero }
`)
	sig := pkg.Scope().Lookup("G").Type().(*types2.Signature)
	c := NewConverter()
	for i := 0; i < sig.Params().Len(); i++ {
		v := sig.Params().At(i)
		if _, err := c.Convert(v.Type()); err == nil {
			t.Errorf("%s (%s) converted without an error", v.Name(), v.Type())
		}
	}
	if _, err := c.Tuple(sig.Results()); err == nil {
		t.Error("a result tuple holding a type parameter converted")
	}
	// The failed tuple left nothing behind, so a later good one is not served
	// a half-built type.
	good := pkg.Scope().Lookup("a2").Type().(*types2.Signature)
	if _, err := c.Tuple(good.Results()); err == nil {
		t.Error("a tuple of one type parameter converted")
	}
}

// TestEmptyInterfaceIsDistinguished pins the one fact about an interface that
// the backend needs and that the type boundary nearly excluded.
//
// Both interface kinds are two pointer words, so size, alignment and pointer
// map are identical and none of them separates the two. The first word's
// meaning differs: an empty interface holds a *_type and a non-empty one holds
// an *itab. Equality calls runtime.efaceeq for the first and runtime.ifaceeq
// for the second, and calling the wrong one makes the runtime read a function
// pointer at the wrong offset and jump through it.
func TestEmptyInterfaceIsDistinguished(t *testing.T) {
	src := `package p

type Stringer interface{ String() string }
type Empty interface{}

var a any
var b Stringer
var c Empty
var d interface{ M(); N() }
`
	pkg, _, _ := buildTypecheck(t, src)
	c := NewConverter()

	for _, tc := range []struct {
		name  string
		empty bool
	}{
		{"a", true},
		{"b", false},
		{"c", true},
		{"d", false},
	} {
		ty := c.mustConvert(t, pkg.Scope().Lookup(tc.name).Type())
		if ty.Kind != Interface {
			t.Errorf("%s has kind %v, want Interface", tc.name, ty.Kind)
			continue
		}
		if ty.EmptyIface != tc.empty {
			t.Errorf("%s: EmptyIface = %v, want %v", tc.name, ty.EmptyIface, tc.empty)
		}
		// The layout facts really are identical, which is the reason the flag
		// has to exist at all.
		if ty.Size != 16 || ty.PtrBytes() != 16 {
			t.Errorf("%s: size %d, PtrBytes %d, want 16 and 16", tc.name, ty.Size, ty.PtrBytes())
		}
	}
}
