// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func scalar(k Kind) *Type { return &Type{Kind: k} }

func ptrTo(t *Type) *Type   { return &Type{Kind: Ptr, Elem: t} }
func sliceOf(t *Type) *Type { return &Type{Kind: Slice, Elem: t} }
func arrayOf(n int64, t *Type) *Type {
	return &Type{Kind: Array, Len: n, Elem: t}
}
func structOf(fields ...Field) *Type { return &Type{Kind: Struct, Fields: fields} }
func field(name string, t *Type) Field {
	return Field{Name: name, Type: t}
}

func mustLayout(t *testing.T, ty *Type) *Type {
	t.Helper()
	if err := Layout(ty); err != nil {
		t.Fatalf("Layout(%s): %v", ty, err)
	}
	return ty
}

// The types the oracle compares against. Each has an ir.Type built by hand and
// a real Go type whose layout reflect reports.
type (
	twoInts struct{ A, B int64 }
	padded  struct {
		A int8
		B int64
		C int8
	}
	nested struct {
		A int32
		B twoInts
		C *int64
	}
	withSlice struct {
		A []int64
		B int32
	}
	withIface struct {
		A any
		B int8
	}
	withStr struct {
		A string
		B bool
	}
	arrays struct {
		A [3]int32
		B [2]*int64
	}
	empty     struct{}
	trailZero struct {
		A int64
		B struct{}
	}
	onlyZero struct{ A struct{} }
)

// TestLayoutMatchesReflect is the whole point of this file. Sizes, alignments
// and field offsets are not nanogo's to choose: every program that uses
// unsafe.Offsetof, every assembly file, and the garbage collector all assume
// the reference layout. reflect reports it exactly.
func TestLayoutMatchesReflect(t *testing.T) {
	i8, i32, i64 := scalar(Int8), scalar(Int32), scalar(Int64)
	b := scalar(Bool)

	cases := []struct {
		ir  *Type
		go_ any
	}{
		{scalar(Bool), false},
		{scalar(Int8), int8(0)},
		{scalar(Int16), int16(0)},
		{scalar(Int32), int32(0)},
		{scalar(Int64), int64(0)},
		{scalar(Uint8), uint8(0)},
		{scalar(Uint64), uint64(0)},
		{scalar(Uintptr), uintptr(0)},
		{scalar(Float32), float32(0)},
		{scalar(Float64), float64(0)},
		{scalar(Complex64), complex64(0)},
		{scalar(Complex128), complex128(0)},
		{scalar(String), ""},
		{scalar(Interface), any(nil)},
		{sliceOf(i64), []int64(nil)},
		{ptrTo(i64), (*int64)(nil)},
		{arrayOf(3, i32), [3]int32{}},
		{arrayOf(0, i64), [0]int64{}},
		{structOf(), empty{}},
		{structOf(field("A", i64), field("B", i64)), twoInts{}},
		{structOf(field("A", i8), field("B", i64), field("C", i8)), padded{}},
		{structOf(
			field("A", i32),
			field("B", structOf(field("A", i64), field("B", i64))),
			field("C", ptrTo(i64)),
		), nested{}},
		{structOf(field("A", sliceOf(i64)), field("B", i32)), withSlice{}},
		{structOf(field("A", scalar(Interface)), field("B", i8)), withIface{}},
		{structOf(field("A", scalar(String)), field("B", b)), withStr{}},
		{structOf(field("A", arrayOf(3, i32)), field("B", arrayOf(2, ptrTo(i64)))), arrays{}},
		{structOf(field("A", i64), field("B", structOf())), trailZero{}},
		{structOf(field("A", structOf())), onlyZero{}},
	}

	for _, c := range cases {
		mustLayout(t, c.ir)
		rt := reflect.TypeOf(c.go_)
		if rt == nil {
			// any(nil) has no reflect type; compare against the interface's
			// own layout instead.
			rt = reflect.TypeOf(&c.go_).Elem()
		}
		if got, want := c.ir.Size, int64(rt.Size()); got != want {
			t.Errorf("%s: size %d, reflect says %d", c.ir, got, want)
		}
		if got, want := c.ir.Align, int64(rt.Align()); got != want {
			t.Errorf("%s: align %d, reflect says %d", c.ir, got, want)
		}
		if rt.Kind() == reflect.Struct {
			if got, want := len(c.ir.Fields), rt.NumField(); got != want {
				t.Errorf("%s: %d fields, reflect says %d", c.ir, got, want)
				continue
			}
			for i := range c.ir.Fields {
				if got, want := c.ir.Fields[i].Offset, int64(rt.Field(i).Offset); got != want {
					t.Errorf("%s: field %d offset %d, reflect says %d", c.ir, i, got, want)
				}
			}
		}
	}
}

// TestTrailingZeroSizeFieldIsPadded pins the rule separately, because it is the
// one a reader is most likely to think is a mistake. A struct ending in a
// zero-size field gets a padding byte, so that the address of that field cannot
// point one past the end of the allocation.
func TestTrailingZeroSizeFieldIsPadded(t *testing.T) {
	ty := mustLayout(t, structOf(field("A", scalar(Int64)), field("B", structOf())))
	if got, want := ty.Size, int64(unsafe.Sizeof(trailZero{})); got != want {
		t.Fatalf("size %d, want %d", got, want)
	}
	if ty.Size <= ty.Fields[1].Offset {
		t.Errorf("the trailing zero-size field at offset %d is not inside a struct of size %d",
			ty.Fields[1].Offset, ty.Size)
	}

	// A zero-size field that is not last costs nothing.
	mid := mustLayout(t, structOf(field("A", structOf()), field("B", scalar(Int64))))
	if got, want := mid.Size, int64(8); got != want {
		t.Errorf("a leading zero-size field made the struct %d bytes, want %d", got, want)
	}
}

func TestPointerMaps(t *testing.T) {
	i64, p := scalar(Int64), ptrTo(scalar(Int64))

	for _, tc := range []struct {
		what     string
		ty       *Type
		wantBits []int64 // word indexes that must be set, in order
		wantPtrB int64
	}{
		{"int64", scalar(Int64), nil, 0},
		{"pointer", p, []int64{0}, 8},
		{"unsafe.Pointer", scalar(UnsafePtr), []int64{0}, 8},
		{"string", scalar(String), []int64{0}, 8},
		{"slice", scalar(Slice), []int64{0}, 8},
		{"interface", scalar(Interface), []int64{0, 1}, 16},
		{"map", scalar(Map), []int64{0}, 8},
		{"chan", scalar(Chan), []int64{0}, 8},
		{"func", scalar(FuncKind), []int64{0}, 8},
		{"array of pointers", arrayOf(3, p), []int64{0, 1, 2}, 24},
		{"array of ints", arrayOf(3, i64), nil, 0},
		{"struct int then pointer", structOf(field("A", i64), field("B", p)), []int64{1}, 16},
		{"struct pointer then int", structOf(field("A", p), field("B", i64)), []int64{0}, 8},
		{"struct with a slice", structOf(field("A", sliceOf(i64)), field("B", i64)), []int64{0}, 8},
		{"array of structs", arrayOf(2, structOf(field("A", i64), field("B", p))), []int64{1, 3}, 32},
	} {
		ty := mustLayout(t, tc.ty)
		var got []int64
		for i := int64(0); i < ty.Size/PtrSize; i++ {
			if bitSet(ty.PtrBits, i) {
				got = append(got, i)
			}
		}
		if len(got) != len(tc.wantBits) {
			t.Errorf("%s: pointer words %v, want %v", tc.what, got, tc.wantBits)
			continue
		}
		for i := range got {
			if got[i] != tc.wantBits[i] {
				t.Errorf("%s: pointer words %v, want %v", tc.what, got, tc.wantBits)
				break
			}
		}
		if ty.PtrBytes() != tc.wantPtrB {
			t.Errorf("%s: PtrBytes %d, want %d", tc.what, ty.PtrBytes(), tc.wantPtrB)
		}
		if ty.HasPointers() != (len(tc.wantBits) > 0) {
			t.Errorf("%s: HasPointers %v", tc.what, ty.HasPointers())
		}
	}
}

// TestPtrBytesIsAPrefixWithinTheType is the invariant that keeps a wrong
// pointer map from being a collector crash: every pointer word lies inside the
// type, and PtrBytes never exceeds its size.
func TestPtrBytesIsAPrefixWithinTheType(t *testing.T) {
	i64, p := scalar(Int64), ptrTo(scalar(Int64))
	types := []*Type{
		scalar(String), scalar(Slice), scalar(Interface),
		arrayOf(4, p), arrayOf(4, i64),
		structOf(field("A", i64), field("B", p), field("C", scalar(Int8))),
		structOf(field("A", scalar(Interface)), field("B", sliceOf(p))),
		arrayOf(3, structOf(field("A", p), field("B", i64))),
	}
	for _, ty := range types {
		mustLayout(t, ty)
		if ty.PtrBytes() > ty.Size {
			t.Errorf("%s: PtrBytes %d exceeds size %d", ty, ty.PtrBytes(), ty.Size)
		}
		for i := int64(0); i < int64(len(ty.PtrBits))*8; i++ {
			if bitSet(ty.PtrBits, i) && (i+1)*PtrSize > ty.Size {
				t.Errorf("%s: pointer word %d lies past the end of a %d byte type", ty, i, ty.Size)
			}
		}
	}
}

func TestLayoutIsIdempotent(t *testing.T) {
	ty := structOf(field("A", scalar(Int64)), field("B", ptrTo(scalar(Int8))))
	mustLayout(t, ty)
	size, align, off := ty.Size, ty.Align, ty.Fields[1].Offset
	mustLayout(t, ty)
	if ty.Size != size || ty.Align != align || ty.Fields[1].Offset != off {
		t.Errorf("a second Layout changed the result: %d/%d/%d then %d/%d/%d",
			size, align, off, ty.Size, ty.Align, ty.Fields[1].Offset)
	}
}

func TestLayoutSharedSubtypes(t *testing.T) {
	// One element type used twice must be laid out once and must be correct in
	// both places. A shared type graph is the normal case, not the exception.
	elem := structOf(field("A", scalar(Int64)), field("B", ptrTo(scalar(Int64))))
	outer := structOf(field("X", elem), field("Y", elem), field("Z", arrayOf(2, elem)))
	mustLayout(t, outer)
	if got, want := outer.Size, int64(16*2+16*2); got != want {
		t.Errorf("size %d, want %d", got, want)
	}
	if outer.Fields[1].Offset != 16 {
		t.Errorf("the second use is at offset %d, want 16", outer.Fields[1].Offset)
	}
}

func TestLayoutErrors(t *testing.T) {
	if err := Layout(nil); err == nil {
		t.Error("Layout(nil) returned no error")
	}
	if err := Layout(&Type{Kind: Array}); err == nil {
		t.Error("an array with no element type laid out without error")
	}
	if err := Layout(&Type{Kind: Array, Len: -1, Elem: scalar(Int8)}); err == nil {
		t.Error("an array with a negative length laid out without error")
	}
	if err := Layout(&Type{Kind: Struct, Fields: []Field{{Name: "A"}}}); err == nil {
		t.Error("a struct field with no type laid out without error")
	}
	if err := Layout(&Type{Kind: Invalid}); err == nil {
		t.Error("an invalid kind laid out without error")
	}

	// A type that contains itself by value is impossible in Go and possible in
	// a malformed graph. It must be an error and not a stack overflow.
	self := &Type{Kind: Struct}
	self.Fields = []Field{{Name: "A", Type: self}}
	if err := Layout(self); err == nil {
		t.Error("a self-containing type laid out without error")
	}

	// The same through an array.
	loop := &Type{Kind: Array, Len: 2}
	loop.Elem = loop
	if err := Layout(loop); err == nil {
		t.Error("a self-containing array laid out without error")
	}
}

func TestKindPredicates(t *testing.T) {
	for _, k := range []Kind{Int8, Int16, Int32, Int64, Uint8, Uint16, Uint32, Uint64, Uintptr} {
		if !k.IsInteger() {
			t.Errorf("%s is not reported as an integer", k)
		}
	}
	for _, k := range []Kind{Int8, Int16, Int32, Int64} {
		if !k.IsSigned() {
			t.Errorf("%s is not reported as signed", k)
		}
	}
	for _, k := range []Kind{Uint8, Uint64, Uintptr, Float64, Ptr} {
		if k.IsSigned() {
			t.Errorf("%s is reported as signed", k)
		}
	}
	if !Float32.IsFloat() || !Float64.IsFloat() || Int64.IsFloat() {
		t.Error("IsFloat is wrong")
	}
	if !Complex64.IsComplex() || !Complex128.IsComplex() || Float64.IsComplex() {
		t.Error("IsComplex is wrong")
	}
	if Ptr.IsInteger() || String.IsInteger() {
		t.Error("a non-integer kind is reported as an integer")
	}
}

func TestKindAndTypeStrings(t *testing.T) {
	for k := Bool; k <= Void; k++ {
		if got := k.String(); got == "" || got == "kind(?)" {
			t.Errorf("kind %d prints %q", k, got)
		}
	}
	if got := Kind(200).String(); got != "kind(?)" {
		t.Errorf("an out-of-range kind prints %q", got)
	}

	i64 := scalar(Int64)
	for _, tc := range []struct {
		ty   *Type
		want string
	}{
		{nil, "<nil>"},
		{i64, "int64"},
		{ptrTo(i64), "*int64"},
		{sliceOf(i64), "[]int64"},
		{arrayOf(3, i64), "[3]int64"},
		{&Type{Kind: Map, Key: scalar(String), Elem: i64}, "map[string]int64"},
		{&Type{Kind: Chan, Elem: i64}, "chan int64"},
		{structOf(field("A", i64), field("B", ptrTo(i64))), "struct{A int64; B *int64}"},
		{structOf(), "struct{}"},
		{&Type{Kind: Int64, Name: "MyInt"}, "MyInt"},
	} {
		if got := tc.ty.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

// TestAllZeroSizeStructIsZeroSized pins the exception to the padding rule.
// Checked against the compiler directly:
//
//	struct{A struct{}}           0
//	struct{A, B struct{}}        0
//	struct{A int8; B struct{}}   2
//	struct{A struct{}; B int64}  8
func TestAllZeroSizeStructIsZeroSized(t *testing.T) {
	for _, tc := range []struct {
		what string
		ty   *Type
		want int64
	}{
		{"one zero-size field", structOf(field("A", structOf())), 0},
		{"two zero-size fields", structOf(field("A", structOf()), field("B", structOf())), 0},
		{"a zero-size array field", structOf(field("A", arrayOf(0, scalar(Int64)))), 0},
		{"padded after a byte", structOf(field("A", scalar(Int8)), field("B", structOf())), 2},
		{"zero-size first", structOf(field("A", structOf()), field("B", scalar(Int64))), 8},
	} {
		if got := mustLayout(t, tc.ty).Size; got != tc.want {
			t.Errorf("%s: size %d, want %d", tc.what, got, tc.want)
		}
	}
}

// TestStringOnACyclicTypeTerminates guards the printer. A malformed type graph
// is exactly what an error message is trying to describe, so a printer that
// recurses forever turns a diagnosable bug into a stack overflow.
func TestStringOnACyclicTypeTerminates(t *testing.T) {
	self := &Type{Kind: Struct}
	self.Fields = []Field{{Name: "A", Type: self}}

	done := make(chan string, 1)
	go func() { done <- self.String() }()
	select {
	case s := <-done:
		if !strings.Contains(s, "...") {
			t.Errorf("the printer produced %q, which does not show it stopped", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("String did not terminate on a cyclic type")
	}

	loop := &Type{Kind: Ptr}
	loop.Elem = loop
	if s := loop.String(); !strings.Contains(s, "...") {
		t.Errorf("a cyclic pointer printed %q", s)
	}
}
