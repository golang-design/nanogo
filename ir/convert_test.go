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

func G(x int) (int, string) { return 0, "" }

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
	// A generic function's own signature is refused for the same reason its
	// type parameter is: converting it converts every parameter type, and one
	// of them is the parameter. specs/013 instantiates before this pass, and
	// ir/build.go refuses a generic declaration before it asks.
	if _, err := c.Convert(pkg.Scope().Lookup("F").Type()); err == nil {
		t.Error("an uninstantiated generic signature converted")
	}
	// A failed conversion leaves nothing behind: the cache entry installed
	// before the recursion is removed, so a later good conversion is not
	// served a half-built type.
	if _, err := c.Convert(pkg.Scope().Lookup("G").Type()); err != nil {
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

// TestConvertCarriesTheDescriptorFields checks the second half of the type
// boundary: the name, the package, the predeclared spelling, the method set and
// a struct field's tag, embedding and package.
//
// Every one of them is a fact no instruction depends on and no type descriptor
// can be written without, which is the rule at the top of type.go.
func TestConvertCarriesTheDescriptorFields(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type Family int

type Info struct {
	Name    string `+"`json:\"name\"`"+`
	changed int
}

type Set struct {
	Info
	List []Info
}

type Counter int

func (c Counter) Value() int  { return int(c) }
func (c *Counter) Add(n int)  { *c += Counter(n) }
func (c Counter) private() {}
`)
	c := NewConverter()
	conv := func(name string) *Type {
		t.Helper()
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("%s is not declared", name)
		}
		out, err := c.Convert(obj.Type())
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	family := conv("Family")
	if family.Name != "p.Family" || family.PkgPath != "p" || family.Basic != "int" {
		t.Errorf("Family carries %q/%q/%q, want p.Family/p/int",
			family.Name, family.PkgPath, family.Basic)
	}
	// The empty set is the point of the field. A nil Methods on a defined type
	// would be indistinguishable from "not computed", and a descriptor writer
	// reading it would claim an empty method set it never established.
	if family.Methods == nil || len(family.Methods) != 0 {
		t.Errorf("Family's method set is %v, want a non-nil empty set", family.Methods)
	}

	info := conv("Info")
	if info.Basic != "" {
		t.Errorf("Info's underlying type is a struct, so Basic is %q, want empty", info.Basic)
	}
	if len(info.Fields) != 2 {
		t.Fatalf("Info has %d fields, want 2", len(info.Fields))
	}
	if got := info.Fields[0]; got.Tag != `json:"name"` || got.Embedded || got.Pkg != "" {
		t.Errorf("Info.Name carries tag %q embedded %v pkg %q, want the tag, false and empty",
			got.Tag, got.Embedded, got.Pkg)
	}
	if got := info.Fields[1]; got.Tag != "" || got.Pkg != "p" {
		t.Errorf("Info.changed carries tag %q pkg %q, want empty and p", got.Tag, got.Pkg)
	}

	set := conv("Set")
	if got := set.Fields[0]; !got.Embedded {
		t.Error("Set.Info is embedded and the field does not say so")
	}
	if got := set.Fields[1]; got.Embedded {
		t.Error("Set.List is not embedded and the field says it is")
	}

	counter := conv("Counter")
	want := []methodShape{
		{Name: "Add", PtrOnly: true},
		{Name: "Value"},
		{Name: "private", Pkg: "p"},
	}
	if got := methodShapes(counter.Methods); !reflect.DeepEqual(got, want) {
		t.Errorf("Counter's method set is %+v, want %+v", got, want)
	}
	// Every method carries a signature. The descriptor's Mtyp is a TypeOff to
	// exactly this type, and zero is a legal Mtyp meaning "unexported, reflect
	// may not call it", so a nil here would be written as a fact.
	for _, m := range counter.Methods {
		if m.Sig == nil {
			t.Errorf("Counter.%s carries no signature", m.Name)
		} else if m.Sig.Kind != FuncKind {
			t.Errorf("Counter.%s has a signature of kind %s", m.Name, m.Sig.Kind)
		}
	}
}

// methodShape is a Method without its signature, so that a test about which
// methods are in a set is not also a test about what each one's type is.
type methodShape struct {
	Name    string
	Pkg     string
	PtrOnly bool
}

func methodShapes(ms []Method) []methodShape {
	out := make([]methodShape, 0, len(ms))
	for _, m := range ms {
		out = append(out, methodShape{Name: m.Name, Pkg: m.Pkg, PtrOnly: m.PtrOnly})
	}
	return out
}

// TestConvertMethodSetFollowsPromotion checks that the method set is the
// checker's answer and not the list of methods declared on the type.
func TestConvertMethodSetFollowsPromotion(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type base struct{}

func (base) Promoted() {}
func (*base) PtrPromoted() {}

// Shadow embeds base and hides Promoted behind a field of the same name.
type Shadow struct {
	base
	Promoted int
}

// ByValue embeds base by value, so PtrPromoted needs an address.
type ByValue struct{ base }

// ByPointer embeds *base, so both are in the value type's own set.
type ByPointer struct{ *base }
`)
	c := NewConverter()
	names := func(name string) []methodShape {
		t.Helper()
		out, err := c.Convert(pkg.Scope().Lookup(name).Type())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Methods {
			if m.Sig == nil {
				t.Errorf("%s.%s carries no signature", name, m.Name)
			}
		}
		return methodShapes(out.Methods)
	}

	// Promoted is shadowed by the field of the same name; PtrPromoted is not
	// shadowed and stays, which is what makes this a shadowing test rather
	// than a test that embedding promotes nothing.
	want := []methodShape{{Name: "PtrPromoted", PtrOnly: true}}
	if got := names("Shadow"); !reflect.DeepEqual(got, want) {
		t.Errorf("Shadow's method set is %+v, want %+v", got, want)
	}
	want = []methodShape{{Name: "Promoted"}, {Name: "PtrPromoted", PtrOnly: true}}
	if got := names("ByValue"); !reflect.DeepEqual(got, want) {
		t.Errorf("ByValue's method set is %+v, want %+v", got, want)
	}
	want = []methodShape{{Name: "Promoted"}, {Name: "PtrPromoted"}}
	if got := names("ByPointer"); !reflect.DeepEqual(got, want) {
		t.Errorf("ByPointer's method set is %+v, want %+v", got, want)
	}
}

// TestConvertCarriesTheSignature pins the half of the type boundary a
// FuncType descriptor and a method's Mtyp both need.
//
// specs/032-type-descriptors-and-itabs.md refused a channel, a function and an
// interface with methods on the same sentence: "a function's signature is not
// in the IR type". It is now, and this is what says so.
func TestConvertCarriesTheSignature(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

func Nullary()                        {}
func Args(a int, b string) error      { return nil }
func Two() (int, string)              { return 0, "" }
func Variadic(a int, rest ...string)  {}
func Slice(a int, rest []string)      {}
`)
	c := NewConverter()
	conv := func(name string) *Type {
		t.Helper()
		out, err := c.Convert(pkg.Scope().Lookup(name).Type())
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != FuncKind {
			t.Fatalf("%s has kind %s, want func", name, out.Kind)
		}
		return out
	}

	// Empty and not nil, for the reason Methods is empty and not nil: a nil
	// list is what a type built below the boundary carries, and the two have
	// to be told apart.
	nullary := conv("Nullary")
	if nullary.Params == nil || len(nullary.Params) != 0 {
		t.Errorf("Nullary's parameters are %v, want a non-nil empty list", nullary.Params)
	}
	if nullary.Results == nil || len(nullary.Results) != 0 {
		t.Errorf("Nullary's results are %v, want a non-nil empty list", nullary.Results)
	}

	args := conv("Args")
	if len(args.Params) != 2 || args.Params[0].Kind != Int64 || args.Params[1].Kind != String {
		t.Errorf("Args takes %v, want int and string", args.Params)
	}
	if len(args.Results) != 1 || args.Results[0].Name != "error" {
		t.Errorf("Args returns %v, want error", args.Results)
	}

	if got := conv("Two"); len(got.Results) != 2 {
		t.Errorf("Two returns %d results, want 2", len(got.Results))
	}

	// The variadic bit is not recoverable from the parameter list. The last
	// parameter's own type is already the slice, so these two types have the
	// same parameters and are different types.
	variadic, slice := conv("Variadic"), conv("Slice")
	if !variadic.Variadic {
		t.Error("Variadic's last parameter is ... and the type does not say so")
	}
	if slice.Variadic {
		t.Error("Slice takes a slice and the type calls it variadic")
	}
	if len(variadic.Params) != len(slice.Params) ||
		variadic.Params[1].Kind != Slice || slice.Params[1].Kind != Slice {
		t.Fatalf("the two parameter lists differ: %v and %v", variadic.Params, slice.Params)
	}
}

// TestConvertMethodSignatureDropsTheReceiver checks the shape a descriptor's
// Mtyp needs.
//
// reflect.Type.Method reports the method's type with the receiver removed, and
// so does the descriptor. types2 keeps the receiver out of Params, so the
// check is that nothing put it back.
func TestConvertMethodSignatureDropsTheReceiver(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type T struct{ n int }

func (t T) Get() int       { return t.n }
func (t *T) Set(v int)     {}
func (t T) Mixed(a int, b string) (bool, error) { return false, nil }
`)
	c := NewConverter()
	out, err := c.Convert(pkg.Scope().Lookup("T").Type())
	if err != nil {
		t.Fatal(err)
	}
	byName := func(name string) Method {
		t.Helper()
		for _, m := range out.Methods {
			if m.Name == name {
				return m
			}
		}
		t.Fatalf("T has no method %s", name)
		return Method{}
	}

	get := byName("Get")
	if len(get.Sig.Params) != 0 {
		t.Errorf("Get's signature takes %v, want nothing; the receiver is not a parameter", get.Sig.Params)
	}
	if len(get.Sig.Results) != 1 || get.Sig.Results[0].Kind != Int64 {
		t.Errorf("Get returns %v, want int", get.Sig.Results)
	}
	set := byName("Set")
	if len(set.Sig.Params) != 1 || set.Sig.Params[0].Kind != Int64 {
		t.Errorf("Set takes %v, want one int", set.Sig.Params)
	}
	mixed := byName("Mixed")
	if len(mixed.Sig.Params) != 2 || len(mixed.Sig.Results) != 2 {
		t.Errorf("Mixed is %d in and %d out, want 2 and 2", len(mixed.Sig.Params), len(mixed.Sig.Results))
	}
}

// TestConvertLaysOutASignaturesParts is the layout invariant applied to the
// two lists this change added.
//
// ir.Layout promises that every type reachable from t has a non-zero
// alignment, and a descriptor writer reads the size and pointer map of every
// parameter and every result. A parameter left unlaid out has size zero, which
// the writer would put in the descriptor without noticing.
func TestConvertLaysOutASignaturesParts(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type Big struct {
	p *int
	s string
}

func F(a Big, b *Big) (Big, []Big) { return a, nil }

type T struct{}

func (T) M(a Big) *Big { return nil }
`)
	c := NewConverter()
	f, err := c.Convert(pkg.Scope().Lookup("F").Type())
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range append(append([]*Type{}, f.Params...), f.Results...) {
		if p.Align == 0 {
			t.Errorf("part %d of F is not laid out: %s", i, p)
		}
	}
	if got := f.Params[0]; got.Size != 3*PtrSize {
		t.Errorf("F's first parameter is %d bytes, want %d", got.Size, 3*PtrSize)
	}

	tt, err := c.Convert(pkg.Scope().Lookup("T").Type())
	if err != nil {
		t.Fatal(err)
	}
	m := tt.Methods[0]
	if m.Sig.Align == 0 {
		t.Fatalf("M's signature is not laid out")
	}
	if m.Sig.Params[0].Align == 0 || m.Sig.Results[0].Align == 0 {
		t.Error("M's signature is laid out and its parameter or result is not")
	}
}

// TestConvertSignatureOfARecursiveMethod checks that a method whose signature
// names the type it is declared on terminates.
//
// The converter installs a cache entry before it recurses, and the method set
// is built while that entry is still empty. A method returning *T reaches the
// entry, and the walk has to stop there rather than convert T again.
func TestConvertSignatureOfARecursiveMethod(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type Node struct{ next *Node }

func (n *Node) Next() *Node      { return n.next }
func (n *Node) Link(o *Node)     {}
`)
	c := NewConverter()
	out, err := c.Convert(pkg.Scope().Lookup("Node").Type())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Methods) != 2 {
		t.Fatalf("Node has %d methods, want 2", len(out.Methods))
	}
	for _, m := range out.Methods {
		if m.Sig == nil {
			t.Fatalf("Node.%s carries no signature", m.Name)
		}
	}
	next := out.Methods[1]
	if next.Name != "Next" {
		t.Fatalf("the second method is %s, want Next", next.Name)
	}
	// The result is *Node, and its element is the very type being converted.
	// One IR type per checker type is what the cache is for.
	if got := next.Sig.Results[0]; got.Kind != Ptr || got.Elem != out {
		t.Errorf("Next returns %s, want a pointer back to the type itself", got)
	}
}

// TestConvertCarriesTheChannelDirection pins the other half of the boundary
// gap specs/032-type-descriptors-and-itabs.md names.
//
// chan int and chan<- int are different types. Below the boundary they were
// one ir.Type, so the naming function computed one symbol for both and the
// linker would have merged two descriptors into one.
func TestConvertCarriesTheChannelDirection(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

var Both chan int
var Send chan<- int
var Recv <-chan int
`)
	c := NewConverter()
	conv := func(name string) *Type {
		t.Helper()
		out, err := c.Convert(pkg.Scope().Lookup(name).Type())
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != Chan {
			t.Fatalf("%s has kind %s, want chan", name, out.Kind)
		}
		return out
	}
	both, send, recv := conv("Both"), conv("Send"), conv("Recv")
	if both.ChanDir != SendRecv {
		t.Errorf("chan int carries %s, want %s", both.ChanDir, SendRecv)
	}
	if send.ChanDir != SendOnly {
		t.Errorf("chan<- int carries %s, want %s", send.ChanDir, SendOnly)
	}
	if recv.ChanDir != RecvOnly {
		t.Errorf("<-chan int carries %s, want %s", recv.ChanDir, RecvOnly)
	}
	// The three are distinguishable, which is the whole point. Element type,
	// size and pointer map are equal for all three.
	if both.Elem != send.Elem || send.Elem != recv.Elem {
		t.Error("the three channel types converted to three element types")
	}
	if both.Size != send.Size || both.Size != recv.Size {
		t.Error("a direction changed a channel's size")
	}
}

// TestChanDirIsInvalidUntilItIsSet is type.go's second rule applied to the
// field: a fact the checker did not supply is not filled in.
func TestChanDirIsInvalidUntilItIsSet(t *testing.T) {
	byHand := &Type{Kind: Chan, Elem: &Type{Kind: Int64}}
	if err := Layout(byHand); err != nil {
		t.Fatal(err)
	}
	if byHand.ChanDir != InvalidDir {
		t.Errorf("a channel built by hand carries %s, want an invalid direction", byHand.ChanDir)
	}
	// The numbers are internal/abi.ChanDir's, because a descriptor carries the
	// value directly and reflect compares against those constants.
	if RecvOnly != 1 || SendOnly != 2 || SendRecv != 3 {
		t.Errorf("the directions are %d, %d and %d, want internal/abi's 1, 2 and 3",
			RecvOnly, SendOnly, SendRecv)
	}
	// The spelling precedes the element type, so direction+" "+elem is the
	// type's own spelling.
	for _, tc := range []struct {
		dir  ChanDir
		want string
	}{
		{SendRecv, "chan"}, {SendOnly, "chan<-"}, {RecvOnly, "<-chan"},
		{InvalidDir, "chandir(invalid)"}, {ChanDir(200), "chandir(?)"},
	} {
		if got := tc.dir.String(); got != tc.want {
			t.Errorf("direction %d prints %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// TestConvertCarriesALiteralInterfacesMethods is the gap that stopped every
// non-empty interface descriptor.
//
// A defined interface reached the boundary through the Named case, which asks
// the checker for the method set. A literal one is not a *types2.Named, so
// that case never ran and it arrived with no methods at all: EmptyIface said
// "this has methods" and Methods said there were none.
func TestConvertCarriesALiteralInterfacesMethods(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type Named interface {
	Read(p []byte) (int, error)
	priv() int
}

var Lit interface {
	Read(p []byte) (int, error)
	priv() int
}

var Any interface{}

type NamedEmpty interface{}
`)
	c := NewConverter()
	conv := func(name string) *Type {
		t.Helper()
		out, err := c.Convert(pkg.Scope().Lookup(name).Type())
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != Interface {
			t.Fatalf("%s has kind %s, want interface", name, out.Kind)
		}
		return out
	}

	// The two spellings of one interface produce one method list. They are two
	// types to the linker, because one has a name, and the contents of the
	// method array have to agree or reflect reports two different interfaces.
	named, lit := conv("Named"), conv("Lit")
	if len(lit.Methods) != 2 {
		t.Fatalf("the literal interface carries %d methods, want 2", len(lit.Methods))
	}
	if !reflect.DeepEqual(methodShapes(named.Methods), methodShapes(lit.Methods)) {
		t.Errorf("the named form carries %+v and the literal form %+v",
			methodShapes(named.Methods), methodShapes(lit.Methods))
	}
	for _, m := range lit.Methods {
		if m.Sig == nil {
			t.Errorf("the literal interface's %s carries no signature", m.Name)
		}
		if m.PtrOnly {
			t.Errorf("%s is marked pointer-only; an interface has no receiver form to promote", m.Name)
		}
	}
	// The unexported method is qualified by the package that declared it. Two
	// packages may declare priv() and they are different methods.
	if got := lit.Methods[1]; got.Name != "priv" || got.Pkg != "p" {
		t.Errorf("the unexported method is %q in %q, want priv in p", got.Name, got.Pkg)
	}

	// An empty interface has an empty set and not a missing one, in both
	// spellings, which is what makes a descriptor's zero method count a fact.
	for _, name := range []string{"Any", "NamedEmpty"} {
		got := conv(name)
		if !got.EmptyIface {
			t.Errorf("%s is empty and EmptyIface says otherwise", name)
		}
		if got.Methods == nil || len(got.Methods) != 0 {
			t.Errorf("%s carries %v, want a non-nil empty set", name, got.Methods)
		}
	}
}

// TestConvertFlattensAnEmbeddedInterface checks that embedding is the
// checker's answer here too.
//
// types2.Interface.NumMethods is the complete set, so a walk that collected
// the embedded interface's methods a second time would double them.
func TestConvertFlattensAnEmbeddedInterface(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type Reader interface{ Read() int }

var Both interface {
	Reader
	Close() error
}
`)
	c := NewConverter()
	out, err := c.Convert(pkg.Scope().Lookup("Both").Type())
	if err != nil {
		t.Fatal(err)
	}
	want := []methodShape{{Name: "Close"}, {Name: "Read"}}
	if got := methodShapes(out.Methods); !reflect.DeepEqual(got, want) {
		t.Errorf("the embedded set is %+v, want %+v", got, want)
	}
}

// TestConvertRecursiveInterface checks that an interface naming itself
// terminates.
//
// The cache entry is installed before the recursion, and a defined
// interface's method set is built while that entry is still empty, so a method
// returning the interface reaches the entry rather than converting it again.
func TestConvertRecursiveInterface(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type Chain interface{ Next() Chain }
`)
	c := NewConverter()
	out, err := c.Convert(pkg.Scope().Lookup("Chain").Type())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Methods) != 1 {
		t.Fatalf("Chain has %d methods, want 1", len(out.Methods))
	}
	if got := out.Methods[0].Sig.Results[0]; got != out {
		t.Errorf("Next returns %s, want the interface itself", got)
	}
}

// TestConvertExportedIsTheCheckersAnswer pins which rule decides that a name
// is exported.
//
// The language says the first character is an upper-case letter, and the
// letter need not be ASCII. This was a byte range here, so a field or method
// named Ärger was treated as unexported: the field got a package path it
// should not carry, which put a path on a struct descriptor that has none, and
// the method got one that qualifies a name reflect can reach without it.
func TestConvertExportedIsTheCheckersAnswer(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type S struct {
	Ärger int64
	ärger int64
}

type T int64

func (T) Ärger() {}
func (T) ärger() {}
`)
	c := NewConverter()
	s, err := c.Convert(pkg.Scope().Lookup("S").Type())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Fields[0]; got.Name != "Ärger" || got.Pkg != "" {
		t.Errorf("the exported field is %q with package %q, want Ärger with none", got.Name, got.Pkg)
	}
	if got := s.Fields[1]; got.Pkg != "p" {
		t.Errorf("the unexported field carries package %q, want p", got.Pkg)
	}

	tt, err := c.Convert(pkg.Scope().Lookup("T").Type())
	if err != nil {
		t.Fatal(err)
	}
	if len(tt.Methods) != 2 {
		t.Fatalf("T has %d methods, want 2", len(tt.Methods))
	}
	for _, m := range tt.Methods {
		switch m.Name {
		case "Ärger":
			if m.Pkg != "" {
				t.Errorf("the exported method carries package %q, want none", m.Pkg)
			}
		case "ärger":
			if m.Pkg != "p" {
				t.Errorf("the unexported method carries package %q, want p", m.Pkg)
			}
		default:
			t.Errorf("T has an unexpected method %q", m.Name)
		}
	}
}
