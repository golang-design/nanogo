// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype_test

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The oracle is the runtime's own view of the same type.
//
// specs/032-type-descriptors-and-itabs.md asks for exactly this check: emit a
// descriptor with nanogo and read it back with reflect in a gc-compiled
// program running in the same binary. Hosted mode makes it direct, because the
// descriptor nanogo produces is meant to be byte-for-byte what gc produces for
// the same type, and gc produced the one reflect is reading.
//
// reflect does not expose Hash, TFlag, PtrBytes or the pointer bitmask, so the
// descriptor is reached through the interface word instead of through
// reflect's own accessors. A reflect.Type is an interface holding a *abi.Type
// in its data word, which is the descriptor itself.

// abiType mirrors internal/abi.Type so the test can read every field.
//
// Mirrored in the test and not in the package under test: the package writes
// the offsets down as constants, because they are the layout of the target's
// runtime rather than of the compiler building nanogo, and a mirror there
// would be the compiler's answer to a question about the target. Here the
// mirror is the point, because this file runs in the target's own binary.
type abiType struct {
	Size_       uintptr
	PtrBytes    uintptr
	Hash        uint32
	TFlag       uint8
	Align_      uint8
	FieldAlign_ uint8
	Kind_       uint8
	Equal       func(unsafe.Pointer, unsafe.Pointer) bool
	GCData      *byte
	Str         int32
	PtrToThis   int32
}

type eface struct{ typ, data unsafe.Pointer }

// abiOf returns the descriptor gc emitted for rt.
func abiOf(rt reflect.Type) *abiType {
	return (*abiType)((*eface)(unsafe.Pointer(&rt)).data)
}

// corpus is the type written twice: once as the type checker reads it, and
// once as the compiler that built this test laid it out.
var corpus = []struct {
	src string
	rt  reflect.Type
}{
	{"bool", reflect.TypeOf(false)},
	{"int", reflect.TypeOf(int(0))},
	{"int8", reflect.TypeOf(int8(0))},
	{"int16", reflect.TypeOf(int16(0))},
	{"int32", reflect.TypeOf(int32(0))},
	{"int64", reflect.TypeOf(int64(0))},
	{"uint", reflect.TypeOf(uint(0))},
	{"uint8", reflect.TypeOf(uint8(0))},
	{"uint16", reflect.TypeOf(uint16(0))},
	{"uint32", reflect.TypeOf(uint32(0))},
	{"uint64", reflect.TypeOf(uint64(0))},
	{"uintptr", reflect.TypeOf(uintptr(0))},
	{"float32", reflect.TypeOf(float32(0))},
	{"float64", reflect.TypeOf(float64(0))},
	{"complex64", reflect.TypeOf(complex64(0))},
	{"complex128", reflect.TypeOf(complex128(0))},
	{"string", reflect.TypeOf("")},
	{"unsafe.Pointer", reflect.TypeOf(unsafe.Pointer(nil))},
	{"[]int", reflect.TypeOf([]int(nil))},
	{"[]byte", reflect.TypeOf([]byte(nil))},
	{"[]string", reflect.TypeOf([]string(nil))},
	{"[]any", reflect.TypeOf([]any(nil))},
	{"[][]int", reflect.TypeOf([][]int(nil))},
	{"[]*int", reflect.TypeOf([]*int(nil))},
	{"*int", reflect.TypeOf((*int)(nil))},
	{"*[]byte", reflect.TypeOf((*[]byte)(nil))},
	{"**int", reflect.TypeOf((**int)(nil))},
	{"*any", reflect.TypeOf((*any)(nil))},
	{"[0]int", reflect.TypeOf([0]int{})},
	{"[1]int", reflect.TypeOf([1]int{})},
	{"[3]int", reflect.TypeOf([3]int{})},
	{"[3]byte", reflect.TypeOf([3]byte{})},
	{"[2]*int", reflect.TypeOf([2]*int{})},
	{"[4]uint32", reflect.TypeOf([4]uint32{})},
	{"[2][3]int", reflect.TypeOf([2][3]int{})},
	{"[1]string", reflect.TypeOf([1]string{})},
	{"[16]byte", reflect.TypeOf([16]byte{})},
	{"any", reflect.TypeOf((*any)(nil)).Elem()},
	// The three channel directions, the four shapes of a signature and a
	// literal interface. Each row's Hash is checked against the hash gc put in
	// the descriptor reflect is reading, and gc computes it over the link
	// string, so a spelling that differs from gc's by one character fails here
	// as a hash mismatch.
	{"chan int", reflect.TypeOf(make(chan int))},
	{"chan<- int", reflect.TypeOf(make(chan<- int))},
	{"<-chan int", reflect.TypeOf(make(<-chan int))},
	{"chan (<-chan int)", reflect.TypeOf(make(chan (<-chan int)))},
	{"[]chan int", reflect.TypeOf([]chan int(nil))},
	{"func()", reflect.TypeOf(func() {})},
	{"func() int", reflect.TypeOf(func() int { return 0 })},
	{"func(int) error", reflect.TypeOf(func(int) error { return nil })},
	{"func(int, string) (bool, error)", reflect.TypeOf(func(int, string) (bool, error) { return false, nil })},
	{"func(int, ...string) (bool, error)", reflect.TypeOf(func(int, ...string) (bool, error) { return false, nil })},
	{"func(...[]byte) func(int) int", reflect.TypeOf(func(...[]byte) func(int) int { return nil })},
	{"[]func(chan<- int)", reflect.TypeOf(([]func(chan<- int))(nil))},
	{"interface{ F() int }", reflect.TypeOf((*interface{ F() int })(nil)).Elem()},
	{"interface{ Read([]byte) (int, error); Close() error }",
		reflect.TypeOf((*interface {
			Read([]byte) (int, error)
			Close() error
		})(nil)).Elem()},
}

// corpusTypes type-checks the corpus and returns one IR type per row.
//
// A variable and not a type declaration. A declaration makes a defined type,
// whose descriptor carries a method set the IR does not hold, and the corpus
// is about the type literals that carry none.
func corpusTypes(t *testing.T) []*ir.Type {
	t.Helper()
	var b strings.Builder
	b.WriteString("package p\n\nimport \"unsafe\"\n\nvar _ unsafe.Pointer\n")
	for i, c := range corpus {
		fmt.Fprintf(&b, "var v%d %s\n", i, c.src)
	}
	src := b.String()

	fset := syntax.NewFileSet()
	file, err := syntax.Parse(fset.AddFile("x.go", len(src)), []byte(src), nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types2.Config{
		Fset:     fset,
		Importer: unsafeImporter{},
		Sizes:    types2.SizesFor("gc", "arm64"),
	}
	pkg, err := conf.Check("p", []*syntax.File{file}, nil)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}

	c := ir.NewConverter()
	out := make([]*ir.Type, len(corpus))
	for i := range corpus {
		o := pkg.Scope().Lookup(fmt.Sprintf("v%d", i))
		if o == nil {
			t.Fatalf("v%d is not declared", i)
		}
		got, err := c.Convert(o.Type())
		if err != nil {
			t.Fatalf("%s: convert: %v", corpus[i].src, err)
		}
		out[i] = got
	}
	return out
}

// unsafeImporter resolves package unsafe and nothing else.
type unsafeImporter struct{}

func (unsafeImporter) Import(path string) (*types2.Package, error) {
	if path == "unsafe" {
		return types2.Unsafe, nil
	}
	return nil, fmt.Errorf("no importer for %q", path)
}

// find returns the symbol with the given name.
func find(syms []rtype.Symbol, name string) (rtype.Symbol, bool) {
	for _, s := range syms {
		if s.Name == name {
			return s, true
		}
	}
	return rtype.Symbol{}, false
}

// reloc returns the relocation at an offset.
func reloc(s rtype.Symbol, off int32) (rtype.Reloc, bool) {
	for _, r := range s.Relocs {
		if r.Off == off {
			return r, true
		}
	}
	return rtype.Reloc{}, false
}

// TestDescriptorAgainstReflect compares every field of an emitted descriptor
// with the one gc emitted for the same type.
//
// This is the strongest check available and it is the one specs/032 asks for.
// A field that agrees here agrees with the runtime that will read it.
func TestDescriptorAgainstReflect(t *testing.T) {
	types := corpusTypes(t)
	for i, c := range corpus {
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			want := abiOf(c.rt)
			d := syms[0]

			if got := binary.LittleEndian.Uint64(d.Data[0:]); got != uint64(want.Size_) {
				t.Errorf("Size_ %d, want %d", got, want.Size_)
			}
			if got := binary.LittleEndian.Uint64(d.Data[8:]); got != uint64(want.PtrBytes) {
				t.Errorf("PtrBytes %d, want %d", got, want.PtrBytes)
			}
			if got := binary.LittleEndian.Uint32(d.Data[16:]); got != want.Hash {
				t.Errorf("Hash %#08x, want %#08x", got, want.Hash)
			}
			if got := d.Data[20]; got != want.TFlag {
				t.Errorf("TFlag %#06b, want %#06b", got, want.TFlag)
			}
			if got := d.Data[21]; got != want.Align_ {
				t.Errorf("Align_ %d, want %d", got, want.Align_)
			}
			if got := d.Data[22]; got != want.FieldAlign_ {
				t.Errorf("FieldAlign_ %d, want %d", got, want.FieldAlign_)
			}
			if got := d.Data[23]; got != want.Kind_ {
				t.Errorf("Kind_ %d, want %d", got, want.Kind_)
			}
		})
	}
}

// TestHashAgainstReflect checks the type hash on its own.
//
// It is a check of the link string as much as of the hash: gc hashes the link
// string, so a hash that matches proves the two compilers spell the type the
// same way, which is what makes the linker's deduplication by name correct.
func TestHashAgainstReflect(t *testing.T) {
	types := corpusTypes(t)
	for i, c := range corpus {
		got, err := rtype.Hash(types[i])
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if want := abiOf(c.rt).Hash; got != want {
			t.Errorf("%s: hash %#08x, want %#08x", c.src, got, want)
		}
	}
}

// TestGCDataAgainstPtrBits checks the collector's view against the compiler's.
//
// specs/032 requires the descriptor's bitmask to be derived from
// ir.Type.PtrBits rather than computed again, so that the two cannot disagree.
// This asserts the derivation both ways: the emitted bytes are what PtrBits
// says, and they are also what gc emitted for the same type.
func TestGCDataAgainstPtrBits(t *testing.T) {
	types := corpusTypes(t)
	for i, c := range corpus {
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			r, ok := reloc(syms[0], 32)
			if !ok {
				t.Fatal("no relocation for GCData")
			}
			bits, ok := find(syms, r.Target)
			if !ok {
				t.Fatalf("the bitmask symbol %s is not emitted", r.Target)
			}
			if r.Type != obj.R_ADDR || r.Size != 8 {
				t.Errorf("GCData relocation is %v/%d, want a pointer", r.Type, r.Size)
			}

			words := types[i].PtrBytes() / ir.PtrSize
			if n := int64(len(bits.Data)); n%ir.PtrSize != 0 {
				t.Errorf("the bitmask is %d bytes, which the runtime reads as words", n)
			}
			want := abiOf(c.rt)
			for w := int64(0); w < words; w++ {
				got := bits.Data[w/8]&(1<<uint(w%8)) != 0
				// The mask must be exactly what ir.Type.PtrBits says.
				// specs/032 makes that the definition of the field, so this is
				// the check that the derivation happened and that no second
				// computation crept in.
				if from := ptrBit(types[i].PtrBits, w); got != from {
					t.Errorf("word %d: bit %v, PtrBits says %v", w, got, from)
				}
				gc := *(*byte)(unsafe.Add(unsafe.Pointer(want.GCData), w/8))&(1<<uint(w%8)) != 0
				switch {
				case got == gc:
				case gc && !got:
					// Never permitted. A pointer gc describes and nanogo does
					// not is a pointer the collector will not trace.
					t.Errorf("word %d: gc says pointer and nanogo does not", w)
				case ifaceTypeWord(types[i], w):
					// nanogo marks both words of an interface and gc marks only
					// the data word, because an itab lives in persistentalloc
					// space and a compile-time _type lives in the read-only
					// section, so neither keeps anything alive. nanogo's answer
					// is conservative and safe, and it is not gc's. The
					// divergence is in ir.scalarPtrBits and is recorded there.
				default:
					t.Errorf("word %d: nanogo says pointer and gc does not", w)
				}
			}
			// Every bit past the pointer prefix must be clear, because
			// PtrBytes is a prefix length and the collector stops there.
			for w := words; w < int64(len(bits.Data))*8; w++ {
				if bits.Data[w/8]&(1<<uint(w%8)) != 0 {
					t.Errorf("word %d is past PtrBytes and is set", w)
				}
			}
		})
	}
}

// TestEqualIsAClosure checks that the Equal field points at a func value.
//
// The field's type is func(unsafe.Pointer, unsafe.Pointer) bool, so it holds
// the address of a one-word symbol holding the code address, not the code
// address itself. Pointing it at the function makes the runtime call whatever
// the function's first instruction encodes.
func TestEqualIsAClosure(t *testing.T) {
	types := corpusTypes(t)
	for i, c := range corpus {
		syms, err := rtype.Descriptor(types[i])
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		r, ok := reloc(syms[0], 24)
		comparable := c.rt.Comparable()
		if ok != comparable {
			t.Errorf("%s: Equal set %v, want %v", c.src, ok, comparable)
			continue
		}
		if !ok {
			continue
		}
		closure, ok := find(syms, r.Target)
		if !ok {
			t.Errorf("%s: the closure %s is not emitted", c.src, r.Target)
			continue
		}
		if len(closure.Data) < 8 {
			t.Errorf("%s: the closure is %d bytes", c.src, len(closure.Data))
			continue
		}
		cr, ok := reloc(closure, 0)
		if !ok || !strings.HasPrefix(cr.Target, "runtime.") {
			t.Errorf("%s: the closure does not point at a runtime function", c.src)
		}
	}
}

// TestStrIsAnOffset checks that the name reference is an offset and not a
// pointer.
//
// specs/032 states the consequence of getting this wrong: a binary that fails
// at load. The check is on the relocation's width and kind, which is where the
// difference lives.
func TestStrIsAnOffset(t *testing.T) {
	types := corpusTypes(t)
	for i, c := range corpus {
		syms, err := rtype.Descriptor(types[i])
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		r, ok := reloc(syms[0], 40)
		if !ok {
			t.Errorf("%s: no relocation for Str", c.src)
			continue
		}
		if r.Type != obj.R_ADDROFF || r.Size != 4 {
			t.Errorf("%s: Str relocation is %v/%d, want R_ADDROFF/4", c.src, r.Type, r.Size)
		}
		nd, ok := find(syms, r.Target)
		if !ok {
			t.Fatalf("%s: the name data %s is not emitted", c.src, r.Target)
		}
		// The encoding is a flag byte, the length as a uvarint, then the
		// bytes. The name is "*T" so that T and *T share one string.
		want := c.rt.String()
		if !strings.HasPrefix(want, "*") {
			want = "*" + want
		}
		got := string(nd.Data[2 : 2+len(want)])
		if got != want {
			t.Errorf("%s: name data holds %q, want %q", c.src, got, want)
		}
		// PtrToThis is zero for a type with no name, which every type in this
		// corpus is. TestPtrToThisNamesThePointerDescriptor has the other half.
		if _, ok := reloc(syms[0], 44); ok {
			t.Errorf("%s: PtrToThis is relocated and a type with no name leaves it zero", c.src)
		}
	}
}

// TestTypelinkMarksWhatReflectWouldOtherwiseRebuild is the table reflect
// searches before it builds a descriptor of its own.
//
// Two descriptors of one type are two types: the runtime compares the pointer
// and reports "types from different scopes" on an assertion between them. Go's
// own test/reflectmethod2.go asserts m.Func.Interface().(func(main.M)) and
// died there, because reflect.FuncOf built a second func(main.M) rather than
// finding the one the program already held.
//
// The set is gc's, in writeType: a type with no name whose kind is one reflect
// can construct. A defined type is not in it, because reflect never builds
// one, and a synthesised type is not either, which is issue 22605.
func TestTypelinkMarksWhatReflectWouldOtherwiseRebuild(t *testing.T) {
	int64Type := &ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"}
	if err := ir.Layout(int64Type); err != nil {
		t.Fatal(err)
	}
	lay := func(typ *ir.Type) *ir.Type {
		t.Helper()
		if err := ir.Layout(typ); err != nil {
			t.Fatal(err)
		}
		return typ
	}
	sig := lay(&ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{int64Type}, Results: []*ir.Type{}})
	for _, tc := range []struct {
		what string
		typ  *ir.Type
		want bool
	}{
		{"a function type", sig, true},
		{"a pointer", lay(&ir.Type{Kind: ir.Ptr, Elem: int64Type}), true},
		{"a slice", lay(&ir.Type{Kind: ir.Slice, Elem: int64Type}), true},
		{"an array", lay(&ir.Type{Kind: ir.Array, Len: 2, Elem: int64Type}), true},
		{"a literal struct", lay(&ir.Type{Kind: ir.Struct, Fields: []ir.Field{{Name: "a", Type: int64Type}}}), true},
		{"a defined type", lay(&ir.Type{Kind: ir.Int64, Name: "p.S", PkgPath: "p", Basic: "int", Methods: []ir.Method{}}), false},
		{"a predeclared type", int64Type, false},
	} {
		syms, err := rtype.Descriptor(tc.typ)
		if err != nil {
			t.Errorf("%s: %v", tc.what, err)
			continue
		}
		if syms[0].Typelink != tc.want {
			t.Errorf("%s is marked %v, want %v", tc.what, syms[0].Typelink, tc.want)
		}
	}
}

// TestPtrToThisNamesThePointerDescriptor is what reflect.PointerTo reads.
//
// Without it reflect builds a descriptor for *T at run time, and a built one
// carries no method set, so reflect.PointerTo(T).NumMethod() answers zero for
// a T that has methods. Go's own test/reflectmethod7.go is that program: it
// asks reflect.PointerTo(T).MethodByName for a method the type has and gets
// nothing back.
func TestPtrToThisNamesThePointerDescriptor(t *testing.T) {
	named := &ir.Type{
		Kind:    ir.Int64,
		Name:    "p.S",
		PkgPath: "p",
		Basic:   "int",
		Methods: []ir.Method{},
	}
	if err := ir.Layout(named); err != nil {
		t.Fatal(err)
	}
	syms, err := rtype.Descriptor(named)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := reloc(syms[0], 44)
	if !ok {
		t.Fatal("a defined type leaves PtrToThis unrelocated")
	}
	if r.Type != obj.R_ADDROFF || r.Size != 4 {
		t.Errorf("PtrToThis relocation is %v/%d, want R_ADDROFF/4", r.Type, r.Size)
	}
	if r.Target != "type:*p.S" {
		t.Errorf("PtrToThis names %q, want type:*p.S", r.Target)
	}

	// The symbol it names is owed by whoever writes this descriptor, so it is
	// in the reference set and the closure emits it.
	refs, err := rtype.Referenced(named)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Fatal("a defined type reaches nothing and owes the descriptor of a pointer to it")
	}
	last := refs[len(refs)-1]
	if last.Kind != ir.Ptr || last.Elem != named {
		t.Fatalf("the last type reached is %s, want a pointer to p.S", last)
	}

	// A pointer has no PtrToThis of its own, which is what stops the closure
	// of one descriptor from growing without end.
	pt, err := rtype.PointerToThis(last)
	if err != nil {
		t.Fatal(err)
	}
	if pt != nil {
		t.Errorf("a pointer reaches %s through PtrToThis", pt)
	}

	// A type the runtime owns has the runtime's PtrToThis, and a second answer
	// here would name a symbol nobody in the link is obliged to define.
	owned := &ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"}
	if err := ir.Layout(owned); err != nil {
		t.Fatal(err)
	}
	if pt, err := rtype.PointerToThis(owned); err != nil || pt != nil {
		t.Errorf("int reaches %v through PtrToThis (%v)", pt, err)
	}
}

// TestTailsAreReferences checks the kind-specific fields.
// descriptorSize is the size the five sections of a descriptor add up to.
//
// internal/abi.Type, the kind-specific header, the optional UncommonType, the
// variable-length data the header points at and the method array. It is
// written out here rather than taken from the encoder, so that a section the
// encoder places wrongly is a failure rather than two agreeing computations.
func descriptorSize(rt reflect.Type) int {
	b := uncommonOffsetOf(rt)
	if u := gcUncommon(rt); u != nil {
		// B plus Moff is where the method array starts, because Moff is
		// measured from the UncommonType and skips the variable-length data.
		return b + int(u.Moff) + int(u.Mcount)*abiMethodSize
	}
	return b + kindDataSize(rt)
}

// uncommonOffsetOf is B: the end of internal/abi.Type plus the kind-specific
// header, which is where an UncommonType starts.
func uncommonOffsetOf(rt reflect.Type) int {
	n := rtype.TypeSize
	switch rt.Kind() {
	case reflect.Pointer, reflect.Slice:
		n += 8
	case reflect.Array:
		n += 24
	case reflect.Chan:
		n += 16
	case reflect.Func:
		n += 8
	case reflect.Interface, reflect.Struct:
		n += 32
	}
	return n
}

// kindDataSize is the variable-length section a kind-specific header points at,
// which sits between the UncommonType and the method array.
func kindDataSize(rt reflect.Type) int {
	switch rt.Kind() {
	case reflect.Func:
		return 8 * (rt.NumIn() + rt.NumOut())
	case reflect.Interface:
		return 8 * rt.NumMethod()
	case reflect.Struct:
		return 24 * rt.NumField()
	}
	return 0
}

// abiUncommon mirrors internal/abi.UncommonType, and abiMethod mirrors one
// entry of the array it points at.
//
// Mirrored here for the reason abiType is: this file runs in the target's own
// binary, so a mirror laid out by the compiler that built the test is the
// layout the runtime reads.
type abiUncommon struct {
	PkgPath int32
	Mcount  uint16
	Xcount  uint16
	Moff    uint32
	_       uint32
}

type abiMethod struct{ Name, Mtyp, Ifn, Tfn int32 }

const abiMethodSize = 16

// gcUncommon returns the UncommonType gc wrote for rt, or nil when gc wrote
// none.
//
// TFlagUncommon is what says whether there is one. Reading the section without
// asking is reading sixteen bytes past the end of the symbol, which is the
// failure the flag exists to prevent.
func gcUncommon(rt reflect.Type) *abiUncommon {
	d := abiOf(rt)
	if d.TFlag&1 == 0 {
		return nil
	}
	return (*abiUncommon)(unsafe.Add(unsafe.Pointer(d), uncommonOffsetOf(rt)))
}

// gcMethods returns the Method array gc wrote for rt.
func gcMethods(rt reflect.Type) []abiMethod {
	u := gcUncommon(rt)
	if u == nil || u.Mcount == 0 {
		return nil
	}
	base := unsafe.Add(unsafe.Pointer(abiOf(rt)), uncommonOffsetOf(rt)+int(u.Moff))
	return unsafe.Slice((*abiMethod)(base), int(u.Mcount))
}

func TestTailsAreReferences(t *testing.T) {
	types := corpusTypes(t)
	for i, c := range corpus {
		syms, err := rtype.Descriptor(types[i])
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		d := syms[0]
		if want := descriptorSize(c.rt); len(d.Data) != want {
			t.Errorf("%s: %d bytes, want %d", c.src, len(d.Data), want)
		}
		switch c.rt.Kind() {
		case reflect.Pointer, reflect.Slice:
			r, ok := reloc(d, rtype.TypeSize)
			if !ok {
				t.Errorf("%s: no element reference", c.src)
				continue
			}
			want, err := ir.TypeSymbol(types[i].Elem)
			if err != nil {
				t.Fatalf("%s: %v", c.src, err)
			}
			if r.Target != want {
				t.Errorf("%s: element is %s, want %s", c.src, r.Target, want)
			}
		case reflect.Array:
			got := binary.LittleEndian.Uint64(d.Data[rtype.TypeSize+16:])
			if got != uint64(c.rt.Len()) {
				t.Errorf("%s: length %d, want %d", c.src, got, c.rt.Len())
			}
			if _, ok := reloc(d, rtype.TypeSize+8); !ok {
				t.Errorf("%s: no slice reference", c.src)
			}
		}
	}
}

// TestSymbolNamesMatchGC checks the symbol names against the names gc gives
// the same symbols.
//
// The names are not cosmetic. specs/032 makes the linker's deduplication a
// function of the name, so a descriptor nanogo names differently from gc is a
// second descriptor for a type that already has one.
func TestSymbolNamesMatchGC(t *testing.T) {
	types := corpusTypes(t)
	for i, c := range corpus {
		syms, err := rtype.Descriptor(types[i])
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if !strings.HasPrefix(syms[0].Name, "type:") {
			t.Errorf("%s: the descriptor is named %q", c.src, syms[0].Name)
		}
		for _, s := range syms {
			if !s.Dupok {
				t.Errorf("%s: %s is not dupok and the linker merges the copies", c.src, s.Name)
			}
			if s.Kind != obj.SRODATA && s.Kind != obj.SNOPTRDATA {
				t.Errorf("%s: %s is kind %v", c.src, s.Name, s.Kind)
			}
			if int64(len(s.Data)) == 0 && !strings.HasPrefix(s.Name, "runtime.gcbits.") {
				t.Errorf("%s: %s has no data", c.src, s.Name)
			}
		}
	}
}

// ptrBit reports whether bit i of a pointer map is set.
func ptrBit(b []byte, i int64) bool {
	if i < 0 || i/8 >= int64(len(b)) {
		return false
	}
	return b[i/8]&(1<<uint(i%8)) != 0
}

// ifaceTypeWord reports whether word w of t is the first word of an interface.
//
// It is the one place nanogo's pointer map is knowingly wider than gc's, so it
// is a predicate rather than an exception in a list: a second divergence would
// fail the test instead of hiding behind the name of this one. The corpus here
// holds no struct, so an interface is either the whole type or the element of
// an array of them.
func ifaceTypeWord(t *ir.Type, w int64) bool {
	for t.Kind == ir.Array && t.Elem != nil && t.Elem.Size > 0 {
		w %= t.Elem.Size / ir.PtrSize
		t = t.Elem
	}
	return t.Kind == ir.Interface && w == 0
}

// TestDescriptorPastThePtrmaskBoundPointsAtTheOnDemandWord covers the one type
// whose GCData is not its pointer bitmask.
//
// Past internal/abi.MaxPtrmaskBytes*8 pointer words gc stops writing the mask
// into read-only data and points GCData at one word in BSS, with
// TFlagGCMaskOnDemand set, which the runtime fills in the first time it needs
// the mask. reflectdata.dgcsym is that rule.
//
// The flag and the symbol are one decision. Set with a bitmask behind GCData,
// the runtime writes a mask pointer over the first word of the bits. Clear
// with the word behind it, the collector reads a zeroed word as the mask of a
// type that holds pointers and scans nothing in it.
func TestDescriptorPastThePtrmaskBoundPointsAtTheOnDemandWord(t *testing.T) {
	tInt := lay(t, &ir.Type{Kind: ir.Int64, Name: "int"})
	tPtr := lay(t, &ir.Type{Kind: ir.Ptr, Elem: tInt})

	// 128 words is the bound and is still a bitmask; 129 is past it.
	for _, tc := range []struct {
		what     string
		n        int64
		onDemand bool
	}{
		{"at the bound", 128, false},
		{"one word past it", 129, true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			typ := lay(t, &ir.Type{Kind: ir.Array, Len: tc.n, Elem: tPtr})
			set, err := rtype.Descriptor(typ)
			if err != nil {
				t.Fatalf("Descriptor: %v", err)
			}
			var target string
			for _, r := range set[0].Relocs {
				if r.Off == rtype.GCDataOffset {
					target = r.Target
				}
			}
			if target == "" {
				t.Fatal("the descriptor names no GCData")
			}
			var gcdata *rtype.Symbol
			for i := range set[1:] {
				if set[1+i].Name == target {
					gcdata = &set[1+i]
				}
			}
			if gcdata == nil {
				t.Fatalf("the descriptor names %s and the set does not define it", target)
			}

			const flagOnDemand = 1 << 4
			flag := set[0].Data[20] & flagOnDemand // internal/abi.Type.TFlag_
			if tc.onDemand {
				if flag == 0 {
					t.Error("the descriptor points at the on-demand word and does not say so in its tflag")
				}
				if !strings.HasPrefix(gcdata.Name, "type:.gcmask.") {
					t.Errorf("GCData is %q, and gc names the on-demand word type:.gcmask.<type>", gcdata.Name)
				}
				if gcdata.Kind != obj.SNOPTRBSS {
					t.Errorf("the on-demand word is %v, want SNOPTRBSS: it holds a pointer to a persistentalloc block the collector does not own", gcdata.Kind)
				}
				if gcdata.Size != 8 || len(gcdata.Data) != 0 {
					t.Errorf("the on-demand word is %d bytes of size and %d of data, want 8 and 0", gcdata.Size, len(gcdata.Data))
				}
				return
			}
			if flag != 0 {
				t.Error("the descriptor points at a bitmask and its tflag says the runtime fills one in")
			}
			if !strings.HasPrefix(gcdata.Name, "runtime.gcbits.") {
				t.Errorf("GCData is %q, and a bitmask is named by its content", gcdata.Name)
			}
			if len(gcdata.Data) == 0 {
				t.Error("the bitmask has no bytes")
			}
		})
	}
}

// TestStackObjectMaskIsNeverTheOnDemandWord is the half of the rule that has
// no second chance.
//
// A stack object's record holds the offset of its mask from the start of the
// section and the runtime resolves that offset against moduledata.rodata, so a
// word in BSS is read at an address that is not a mask and the object is
// scanned by whatever bits are there. gc keeps the two apart by calling GCSym
// with onDemandAllowed false from the one place that describes a stack object,
// and the bitmask form has no upper size for exactly this reason.
func TestStackObjectMaskIsNeverTheOnDemandWord(t *testing.T) {
	tInt := lay(t, &ir.Type{Kind: ir.Int64, Name: "int"})
	tPtr := lay(t, &ir.Type{Kind: ir.Ptr, Elem: tInt})
	typ := lay(t, &ir.Type{Kind: ir.Array, Len: 300, Elem: tPtr})

	sym, err := rtype.StackObjectMask(typ)
	if err != nil {
		t.Fatalf("StackObjectMask: %v", err)
	}
	if !strings.HasPrefix(sym.Name, "runtime.gcbits.") {
		t.Errorf("the mask is %q, and a stack object needs the bitmask whatever the type's size", sym.Name)
	}
	if sym.Kind != obj.SRODATA {
		t.Errorf("the mask is %v, want SRODATA: the runtime resolves a record's offset against moduledata.rodata", sym.Kind)
	}
	// 300 pointer words is 300 bits, which is 38 bytes rounded up to a word.
	if want := 40; len(sym.Data) != want {
		t.Errorf("the mask is %d bytes, want %d", len(sym.Data), want)
	}
	for i, b := range sym.Data {
		want := byte(0xff)
		switch {
		case i >= 38:
			want = 0
		case i == 37:
			want = 0x0f // 300 - 37*8 = 4 bits
		}
		if b != want {
			t.Fatalf("byte %d of the mask is %#02x, want %#02x", i, b, want)
		}
	}
}
