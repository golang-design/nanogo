// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The defined-type corpus, declared twice.
//
// Once here, so that gc compiles a descriptor for each and reflect can be
// asked about it, and once as source text in namedSource, so that the type
// checker produces the same type and this package encodes a descriptor for it.
// The two are checked against each other field by field, the same way the type
// literal corpus is.
//
// The source is type-checked under *this package's own import path*, which
// namedTypes reads out of reflect rather than spelling. That is what makes the
// comparison exact rather than approximate: a defined type's link string holds
// its import path, the type hash is computed over the link string, and gc
// computed the hash reflect reports. A hash that matches is proof that the two
// compilers spell the type identically, which is the property the linker
// deduplicates on.

// Family is a defined type over a predeclared one. Its abi kind must be Int
// and not Int64, which is the distinction ir.Type.Basic carries.
type Family int

// Flags is a struct that compares as one region of memory.
type Flags struct {
	Regabi bool
	Boring bool
}

// Record holds a slice, so it is not comparable and its Equal is nil.
type Record struct {
	Count int64
	Stack []uintptr
}

// Ident is the embedded field of Tagged.
type Ident int64

// Tagged carries a field tag, an unexported field and an embedded field, which
// are the three field facts a struct descriptor holds beyond the type. Every
// field is eight bytes so that nothing is padding and the struct still
// compares as one region of memory.
type Tagged struct {
	Ident
	Count  int64 `json:"count"`
	hidden int64
}

// Wide is a struct whose fields each compare as memory and which does not,
// because the bytes between them are padding.
type Wide struct {
	A byte
	B int64
}

// Label compares field by field because a string does.
type Label struct {
	Key   string
	Value string
}

// List and Buf put the two remaining kind-specific header sizes under the
// oracle: eight bytes for a slice and twenty-four for an array. Without them
// the UncommonType is only ever placed after a header of zero or thirty-two
// bytes, and a header the encoder sizes wrongly would go unnoticed.
type List []Flags

type Buf [4]int64

// Signal, Send and Recv are the three channel directions. They are one type to
// the machine and three types to the language, and the descriptor is where
// that stops being a distinction without a difference: the three have three
// link strings, three hashes and three descriptors, and a compiler that
// carried no direction would emit one symbol for all three.
type Signal chan int

type Send chan<- int

type Recv <-chan Flags

// Handler, Nullary and Variadic are function types. The descriptor's tail is
// two counts and an array of parameter and result descriptors, and the top bit
// of the result count is the variadic flag, so these three cover the counts,
// the order of the array and the flag.
type Handler func(n int, s string) error

type Nullary func()

type Variadic func(head Flags, rest ...*Ident) (int, error)

// NamedEmpty is an interface with a name and no methods. gc writes the
// declaring package's path into its InterfaceType header, and the encoder used
// to write thirty-two zero bytes for every interface, so reflect reported no
// package for a type that has one.
type NamedEmpty interface{}

// Unicode holds an exported field whose first letter is not ASCII. The
// language says an identifier is exported when its first character is an
// upper-case letter, and the letter need not be ASCII, so an exported test
// written as a byte range answers wrongly here.
type Unicode struct {
	Ärger int64
	ärger int64
}

// ReaderLit is the literal interface behind Reader, as an alias so that gc
// compiles the literal rather than a defined type.
//
// The unexported method is the point. A literal interface's spelling qualifies
// an unexported method name with the package that declares it, by import path
// in the link string and by package name in the name string, so this type is
// the one that checks the qualifier against gc's.
type ReaderLit = interface {
	Read(p []byte) (int, error)
	flush() error
}

// Reader is the same interface with a name. Its descriptor's Imethod array
// holds an offset to the descriptor of each method's signature, which is a
// function literal, so it is the row that was refused while a signature had no
// spelling.
type Reader interface {
	Read(p []byte) (int, error)
	flush() error
}

// Counter is a defined type with a method set that covers every row of the
// Method array at once.
//
// Value is exported with a value receiver, so T's descriptor names the method
// for Tfn and the generated wrapper for Ifn, because an int is not one pointer
// word. Ärger is exported and its first letter is not ASCII, which is the case
// that decides where the exported prefix ends: byte order by name would sort it
// after hidden, and Xcount counts the prefix, so reflect would report one
// exported method where there are two. Add has a pointer receiver, so it is in
// *Counter's set and not in Counter's. hidden is unexported, so it is in the
// array and not in the exported prefix.
type Counter int

func (c Counter) Value() int  { return int(c) }
func (c Counter) Ärger() int  { return int(c) + 1 }
func (c *Counter) Add(n int)  { *c += Counter(n) }
func (c Counter) hidden() int { return int(c) + 2 }

// CounterPtr puts *Counter in the corpus. A pointer carries no name of its own
// and carries the whole of the pointee's method set, so it is the descriptor
// that has an UncommonType because of its methods rather than because of a
// name, and the one whose Tfn and Ifn both name the pointer method.
type CounterPtr = *Counter

// Handle is one pointer word, so a value of it is its own interface word.
// TFlagDirectIface is set, and Ifn therefore names the method itself rather
// than a wrapper that dereferences a pointer.
type Handle struct{ p *int }

func (h Handle) Get() int {
	if h.p == nil {
		return 0
	}
	return *h.p
}

// Valuer is the interface Counter implements. It exists so that gc emits an
// itab for the pair, which is the oracle the itab tests read.
type Valuer interface{ Value() int }

// valuerHolder converts a Counter to a Valuer, which is what makes gc write
// go:itab.<path>.Counter,<path>.Valuer into the object.
var valuerHolder Valuer = Counter(3)

// Counted is the whole of Counter's value-receiver method set as an interface.
// It is what puts the order of the Fun array under test: gc puts every
// exported name first and Ärger is exported, so the slots are Value, Ärger,
// hidden and byte order by name would swap the last two.
type Counted interface {
	Value() int
	Ärger() int
	hidden() int
}

var countedHolder Counted = Counter(5)

var namedCorpus = []struct {
	src string
	rt  reflect.Type
}{
	{"Family", reflect.TypeOf(Family(0))},
	{"Ident", reflect.TypeOf(Ident(0))},
	{"Flags", reflect.TypeOf(Flags{})},
	{"Record", reflect.TypeOf(Record{})},
	{"Tagged", reflect.TypeOf(Tagged{})},
	{"List", reflect.TypeOf(List(nil))},
	{"Buf", reflect.TypeOf(Buf{})},
	{"Signal", reflect.TypeOf(Signal(nil))},
	{"Send", reflect.TypeOf(Send(nil))},
	{"Recv", reflect.TypeOf(Recv(nil))},
	{"Handler", reflect.TypeOf(Handler(nil))},
	{"Nullary", reflect.TypeOf(Nullary(nil))},
	{"Variadic", reflect.TypeOf(Variadic(nil))},
	{"NamedEmpty", reflect.TypeOf((*NamedEmpty)(nil)).Elem()},
	{"Unicode", reflect.TypeOf(Unicode{})},
	{"Counter", reflect.TypeOf(Counter(0))},
	{"CounterPtr", reflect.TypeOf((*Counter)(nil))},
	{"Handle", reflect.TypeOf(Handle{})},
	{"Valuer", reflect.TypeOf((*Valuer)(nil)).Elem()},
	{"Counted", reflect.TypeOf((*Counted)(nil)).Elem()},
}

// namedRefusals are the rows that must be refused, with the words the refusal
// has to contain. Each is a gap in this compiler that a descriptor cannot be
// written around, and a refusal is the only honest answer: an Equal of nil on
// a comparable type makes the runtime panic when a value of it is used as a
// map key.
var namedRefusals = []struct {
	src  string
	want string
}{
	// Empty, and kept rather than deleted. Wide and Label sat here because a
	// struct that compares field by field had no function to point Equal at.
	// ssagen generates one now, so both are described and
	// TestGeneratedEqualityDescribesAFieldWiseStruct asserts that instead.
	// The table stays because the next unwritable named type belongs in it.
}

// namedSource is the corpus as the type checker reads it.
const namedSource = "package rtype_test\n" + `
type Family int

type Flags struct {
	Regabi bool
	Boring bool
}

type Record struct {
	Count int64
	Stack []uintptr
}

type Ident int64

type Tagged struct {
	Ident
	Count  int64 ` + "`json:\"count\"`" + `
	hidden int64
}

type Wide struct {
	A byte
	B int64
}

type Label struct {
	Key   string
	Value string
}

type List []Flags

type Buf [4]int64

type Signal chan int

type Send chan<- int

type Recv <-chan Flags

type Handler func(n int, s string) error

type Nullary func()

type Variadic func(head Flags, rest ...*Ident) (int, error)

type NamedEmpty interface{}

type Unicode struct {
	Ärger int64
	ärger int64
}

type ReaderLit = interface {
	Read(p []byte) (int, error)
	flush() error
}

type Reader interface {
	Read(p []byte) (int, error)
	flush() error
}

type Counter int

func (c Counter) Value() int  { return int(c) }
func (c Counter) Ärger() int  { return int(c) + 1 }
func (c *Counter) Add(n int)  { *c += Counter(n) }
func (c Counter) hidden() int { return int(c) + 2 }

type CounterPtr = *Counter

type Handle struct{ p *int }

func (h Handle) Get() int {
	if h.p == nil {
		return 0
	}
	return *h.p
}

type Valuer interface{ Value() int }

var valuerHolder Valuer = Counter(3)

type Counted interface {
	Value() int
	Ärger() int
	hidden() int
}

var countedHolder Counted = Counter(5)
`

// namedPkgPath is the import path gc compiled this test package under.
func namedPkgPath() string { return reflect.TypeOf(Family(0)).PkgPath() }

// namedTypes type-checks namedSource and returns one IR type per corpus row,
// plus the checked package.
func namedTypes(t *testing.T) ([]*ir.Type, *types2.Package) {
	t.Helper()
	return namedTypesUnder(t, namedPkgPath())
}

// namedTypesUnder type-checks namedSource under one import path.
func namedTypesUnder(t *testing.T, path string) ([]*ir.Type, *types2.Package) {
	t.Helper()
	fset := syntax.NewFileSet()
	file, err := syntax.Parse(fset.AddFile("x.go", len(namedSource)), []byte(namedSource), nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types2.Config{
		Fset:  fset,
		Sizes: types2.SizesFor("gc", "arm64"),
	}
	pkg, err := conf.Check(path, []*syntax.File{file}, nil)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	c := ir.NewConverter()
	out := make([]*ir.Type, len(namedCorpus))
	for i, row := range namedCorpus {
		obj := pkg.Scope().Lookup(row.src)
		if obj == nil {
			t.Fatalf("%s is not declared", row.src)
		}
		got, err := c.Convert(obj.Type())
		if err != nil {
			t.Fatalf("%s: convert: %v", row.src, err)
		}
		out[i] = got
	}
	return out, pkg
}

// TestNamedDescriptorAgainstReflect checks every field of internal/abi.Type for
// a defined type against the descriptor gc emitted for the same type.
func TestNamedDescriptorAgainstReflect(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			want := abiOf(c.rt)
			d := syms[0].Data

			if got := binary.LittleEndian.Uint64(d[0:]); got != uint64(want.Size_) {
				t.Errorf("Size_ %d, want %d", got, want.Size_)
			}
			if got := binary.LittleEndian.Uint64(d[8:]); got != uint64(want.PtrBytes) {
				t.Errorf("PtrBytes %d, want %d", got, want.PtrBytes)
			}
			// The hash is computed over the link string, so this is the check
			// that the name matches gc's.
			if got := binary.LittleEndian.Uint32(d[16:]); got != want.Hash {
				t.Errorf("Hash %#08x, want %#08x; the link string differs from gc's", got, want.Hash)
			}
			if got := d[20]; got != want.TFlag {
				t.Errorf("TFlag %#08b, want %#08b", got, want.TFlag)
			}
			if got := d[21]; got != want.Align_ {
				t.Errorf("Align_ %d, want %d", got, want.Align_)
			}
			if got := d[23]; got != want.Kind_ {
				t.Errorf("Kind_ %d, want %d", got, want.Kind_)
			}
			if want := descriptorSize(c.rt); len(d) != want {
				t.Errorf("%d bytes, want %d", len(d), want)
			}
		})
	}
}

// TestUncommonTypeAgainstReflect checks the section a defined type exists for
// against the one gc wrote for the same type.
//
// gc's own descriptor is the oracle rather than reflect's accessors, because
// reflect reports the exported method set and Mcount counts the whole of it.
// The three numbers are what every reader of the section navigates by:
// reflect.Type.PkgPath reads PkgPath, reflect.Type.NumMethod on a type that is
// not an interface returns Xcount, and Moff is the distance to the array both
// of them index.
func TestUncommonTypeAgainstReflect(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			d := syms[0]
			want := gcUncommon(c.rt)
			if want == nil {
				t.Fatal("gc wrote no UncommonType for a type in the named corpus")
			}
			// B is where the UncommonType starts: the end of
			// internal/abi.Type plus the kind-specific header.
			b := uncommonOffsetOf(c.rt)
			if got := d.Data[20] & 1; got == 0 {
				t.Error("TFlagUncommon is clear and gc set it")
			}
			if got := binary.LittleEndian.Uint16(d.Data[b+4:]); got != want.Mcount {
				t.Errorf("Mcount %d, want %d", got, want.Mcount)
			}
			if got := binary.LittleEndian.Uint16(d.Data[b+6:]); got != want.Xcount {
				t.Errorf("Xcount %d, want %d", got, want.Xcount)
			}
			if got := binary.LittleEndian.Uint32(d.Data[b+8:]); got != want.Moff {
				t.Errorf("Moff %d, want %d", got, want.Moff)
			}

			r, ok := reloc(d, int32(b))
			if ok != (want.PkgPath != 0) {
				t.Fatalf("package path present %v, want %v", ok, want.PkgPath != 0)
			}
			if !ok {
				return
			}
			path := namedPkgPath()
			sym := "type:.importpath." + path + "."
			if r.Target != sym {
				t.Errorf("package path is %s, want %s", r.Target, sym)
			}
			if r.Size != 4 {
				t.Errorf("package path is %d bytes, want a four-byte NameOff", r.Size)
			}
			def, ok := find(syms, sym)
			if !ok {
				t.Fatalf("%s is not defined", sym)
			}
			if got := decodeName(t, def.Data); got != path {
				t.Errorf("package path is %q, want %q", got, path)
			}
		})
	}
}

// TestMethodArrayAgainstReflect checks the Method array against gc's own.
//
// The array is what reflect.Type.Method reads and what the linker walks to
// decide which methods a reachable type keeps, so three things have to hold at
// once: the entries are in gc's order, each names the method's name, its type
// with the receiver removed, and the two functions an ordinary call and an itab
// call reach, and the three code and type offsets of one entry are consecutive
// R_METHODOFF relocations. cmd/link's deadcode pass panics with "expect three
// consecutive R_METHODOFF relocs" if anything separates them.
func TestMethodArrayAgainstReflect(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		want := gcMethods(c.rt)
		if len(want) == 0 {
			continue
		}
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			d := syms[0]
			b := uncommonOffsetOf(c.rt)
			base := b + int(binary.LittleEndian.Uint32(d.Data[b+8:]))
			if got := len(d.Data); got != base+len(want)*16 {
				t.Fatalf("the descriptor is %d bytes and the array ends at %d", got, base+len(want)*16)
			}
			// The names, in order. gc's Name is an offset this process cannot
			// resolve, so the names come from the source: the exported ones in
			// order are what reflect reports, and the unexported ones are the
			// ones the declaration holds.
			names := methodNamesOf(t, c.rt)
			for j := range want {
				off := int32(base + j*16)
				r, ok := reloc(d, off)
				if !ok {
					t.Fatalf("method %d has no name reference", j)
				}
				if r.Type != obj.R_ADDROFF || r.Size != 4 {
					t.Errorf("method %d's name is a %v of %d bytes, want a four-byte R_ADDROFF", j, r.Type, r.Size)
				}
				def, ok := find(syms, r.Target)
				if !ok {
					t.Fatalf("method %d's name symbol %s is not defined", j, r.Target)
				}
				if got := decodeName(t, def.Data); got != names[j] {
					t.Errorf("method %d is named %q, want %q", j, got, names[j])
				}
				// The three offsets into text and type data, consecutive and
				// in the order Mtyp, Ifn, Tfn.
				for k, field := range []string{"Mtyp", "Ifn", "Tfn"} {
					r, ok := reloc(d, off+4+int32(4*k))
					if !ok {
						t.Fatalf("method %d has no %s reference", j, field)
					}
					if r.Type != obj.R_METHODOFF || r.Size != 4 {
						t.Errorf("method %d's %s is a %v of %d bytes, want a four-byte R_METHODOFF", j, field, r.Type, r.Size)
					}
				}
			}
		})
	}
}

// methodNamesOf returns the names of rt's methods in the order gc's array holds
// them: every exported name first, then the unexported ones.
//
// reflect reports only the exported ones, so the unexported names come from the
// declarations in this file. Counter is the only type in the corpus that has
// one.
func methodNamesOf(t *testing.T, rt reflect.Type) []string {
	t.Helper()
	out := make([]string, 0, rt.NumMethod())
	for i := 0; i < rt.NumMethod(); i++ {
		out = append(out, rt.Method(i).Name)
	}
	base := rt
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if base == reflect.TypeOf(Counter(0)) {
		out = append(out, "hidden")
	}
	if got := len(out); got != len(gcMethods(rt)) {
		t.Fatalf("%v has %d methods in the descriptor and this test names %d", rt, len(gcMethods(rt)), got)
	}
	return out
}

// TestDescriptorAsksToBeMarkedUsedInIface checks the mark without which every
// method in the array above is pruned.
//
// cmd/link collects a type's Method array only when the type carries the mark,
// so an unmarked type's entries all resolve to the sentinel -1 and
// runtime.getitab installs runtime.unreachableMethod in their place. The
// program links and dies with "unreachable method called. linker bug?" the
// first time a value of the type is used through an interface. Go's own
// test/const3.go is exactly that program: it formats a defined type with a
// String method through fmt.
//
// The mark is on the descriptor and on nothing else it points at. A pointer
// bitmask and a name are not types, and cmd/link panics on a marker aimed at a
// symbol that is not one.
func TestDescriptorAsksToBeMarkedUsedInIface(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			if !syms[0].UsedInIface {
				t.Error("the descriptor is not marked, so cmd/link prunes every method it holds")
			}
			for _, s := range syms[1:] {
				if s.UsedInIface {
					t.Errorf("%s is marked and it is not a type descriptor", s.Name)
				}
			}
		})
	}
}

// TestMethodArrayNamesTheFunctions checks that each entry names the method the
// front end compiled or the wrapper ssagen generates, and never anything else.
//
// The table is the one rtype/uncommon.go draws. Getting Ifn wrong is silent:
// both spellings exist, both are called with one word, and the word means a
// value in one and a pointer in the other.
func TestMethodArrayNamesTheFunctions(t *testing.T) {
	types, _ := namedTypes(t)
	byName := map[string]*ir.Type{}
	for i, c := range namedCorpus {
		byName[c.src] = types[i]
	}
	pkg := namedPkgPath()
	for _, tc := range []struct {
		src      string
		entries  []string
		tfn, ifn []string
	}{
		{
			// Counter is an int, so a value of it is not its own interface
			// word and Ifn is the wrapper on the pointer.
			src:     "Counter",
			entries: []string{"Value", "Ärger", "hidden"},
			tfn:     []string{pkg + ".Counter.Value", pkg + ".Counter.Ärger", pkg + ".Counter.hidden"},
			ifn:     []string{pkg + ".(*Counter).Value", pkg + ".(*Counter).Ärger", pkg + ".(*Counter).hidden"},
		},
		{
			// The pointer's set is the larger one and both fields name the
			// pointer method throughout.
			src:     "CounterPtr",
			entries: []string{"Add", "Value", "Ärger", "hidden"},
			tfn:     []string{pkg + ".(*Counter).Add", pkg + ".(*Counter).Value", pkg + ".(*Counter).Ärger", pkg + ".(*Counter).hidden"},
			ifn:     []string{pkg + ".(*Counter).Add", pkg + ".(*Counter).Value", pkg + ".(*Counter).Ärger", pkg + ".(*Counter).hidden"},
		},
		{
			// Handle is one pointer word, so the itab passes the value itself
			// and Ifn is the method rather than a wrapper.
			src:     "Handle",
			entries: []string{"Get"},
			tfn:     []string{pkg + ".Handle.Get"},
			ifn:     []string{pkg + ".Handle.Get"},
		},
	} {
		t.Run(tc.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(byName[tc.src])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			d := syms[0]
			b := uncommonOffsetOf(reflect.TypeOf(Counter(0)))
			if tc.src == "CounterPtr" {
				b = rtype.TypeSize + 8
			}
			if tc.src == "Handle" {
				b = rtype.TypeSize + 32
			}
			base := b + int(binary.LittleEndian.Uint32(d.Data[b+8:]))
			if got := binary.LittleEndian.Uint16(d.Data[b+4:]); int(got) != len(tc.entries) {
				t.Fatalf("Mcount %d, want %d", got, len(tc.entries))
			}
			for j := range tc.entries {
				off := int32(base + j*16)
				name, _ := reloc(d, off)
				def, _ := find(syms, name.Target)
				if got := decodeName(t, def.Data); got != tc.entries[j] {
					t.Errorf("method %d is %q, want %q", j, got, tc.entries[j])
				}
				ifn, _ := reloc(d, off+8)
				if ifn.Target != tc.ifn[j] {
					t.Errorf("%s's Ifn is %s, want %s", tc.entries[j], ifn.Target, tc.ifn[j])
				}
				tfn, _ := reloc(d, off+12)
				if tfn.Target != tc.tfn[j] {
					t.Errorf("%s's Tfn is %s, want %s", tc.entries[j], tfn.Target, tc.tfn[j])
				}
			}
		})
	}
}

// TestStructFieldsAgainstReflect checks the field array against gc's own.
func TestStructFieldsAgainstReflect(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		if c.rt.Kind() != reflect.Struct {
			continue
		}
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			d := syms[0]
			const header = 32
			if got := binary.LittleEndian.Uint64(d.Data[rtype.TypeSize+16:]); int(got) != c.rt.NumField() {
				t.Fatalf("%d fields, want %d", got, c.rt.NumField())
			}
			// The field array is addressed by a relocation against the
			// descriptor's own symbol, because a descriptor is one symbol and
			// the array has no name of its own.
			r, ok := reloc(d, int32(rtype.TypeSize+8))
			if !ok {
				t.Fatal("no field array reference")
			}
			if r.Target != d.Name {
				t.Errorf("field array is in %s, want this symbol", r.Target)
			}
			base := int(r.Add)
			if want := rtype.TypeSize + header + 16; base != want {
				t.Errorf("field array starts at %d, want %d", base, want)
			}
			for j := 0; j < c.rt.NumField(); j++ {
				f := c.rt.Field(j)
				off := base + j*24
				if got := binary.LittleEndian.Uint64(d.Data[off+16:]); got != uint64(f.Offset) {
					t.Errorf("%s: offset %d, want %d", f.Name, got, f.Offset)
				}
				nr, ok := reloc(d, int32(off))
				if !ok {
					t.Fatalf("%s: no name reference", f.Name)
				}
				sym, ok := find(syms, nr.Target)
				if !ok {
					t.Fatalf("%s: %s is not defined", f.Name, nr.Target)
				}
				name, tag, exported, embedded := decodeField(t, sym.Data)
				if name != f.Name {
					t.Errorf("field %d is %q, want %q", j, name, f.Name)
				}
				if tag != string(f.Tag) {
					t.Errorf("%s: tag %q, want %q", f.Name, tag, f.Tag)
				}
				if embedded != f.Anonymous {
					t.Errorf("%s: embedded %v, want %v", f.Name, embedded, f.Anonymous)
				}
				if exported != (f.PkgPath == "") {
					t.Errorf("%s: exported %v, want %v", f.Name, exported, f.PkgPath == "")
				}
				tr, ok := reloc(d, int32(off+8))
				if !ok {
					t.Fatalf("%s: no type reference", f.Name)
				}
				want, err := ir.TypeSymbol(types[i].Fields[j].Type)
				if err != nil {
					t.Fatal(err)
				}
				if tr.Target != want {
					t.Errorf("%s: type is %s, want %s", f.Name, tr.Target, want)
				}
			}
		})
	}
}

// TestStructPkgPathIsTheUnexportedFieldsPackage checks the second package path
// a struct descriptor carries, which reflect needs to name an unexported field
// from outside the package that declared it.
func TestStructPkgPathIsTheUnexportedFieldsPackage(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		if c.rt.Kind() != reflect.Struct {
			continue
		}
		unexported := false
		for j := 0; j < c.rt.NumField(); j++ {
			if c.rt.Field(j).PkgPath != "" {
				unexported = true
			}
		}
		syms, err := rtype.Descriptor(types[i])
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		r, ok := reloc(syms[0], int32(rtype.TypeSize))
		if ok != unexported {
			t.Errorf("%s: struct package path present %v, want %v", c.src, ok, unexported)
			continue
		}
		if !ok {
			continue
		}
		if r.Size != 8 {
			t.Errorf("%s: struct package path is %d bytes, want a pointer", c.src, r.Size)
		}
		want := "type:.importpath." + namedPkgPath() + "."
		if r.Target != want {
			t.Errorf("%s: struct package path is %s, want %s", c.src, r.Target, want)
		}
	}
}

// TestDescriptorDescribesAMethodSet checks that a method set reaches the
// descriptor rather than stopping it.
//
// It was refused for as long as the two ABI wrappers did not exist. Both halves
// are here now: ssagen generates the wrapper and ir.MethodSymbol spells it, so
// the row names a function the same object defines.
func TestDescriptorDescribesAMethodSet(t *testing.T) {
	_, pkg := namedTypes(t)
	c := ir.NewConverter()
	counter, err := c.Convert(pkg.Scope().Lookup("Counter").Type())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rtype.Descriptor(counter); err != nil {
		t.Fatalf("Counter: %v", err)
	}
	ptr := &ir.Type{Kind: ir.Ptr, Elem: counter}
	if err := ir.Layout(ptr); err != nil {
		t.Fatal(err)
	}
	if _, err := rtype.Descriptor(ptr); err != nil {
		t.Fatalf("*Counter: %v", err)
	}
	// A descriptor is not a leaf. Every Mtyp is an offset to the descriptor of
	// the method's signature, so whoever writes this one owes those as well.
	// The pointer's set is the larger one, so it is the one that owes them all.
	refs, err := rtype.Referenced(ptr)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range counter.Methods {
		found := false
		for _, r := range refs {
			if r == m.Sig {
				found = true
			}
		}
		if !found {
			t.Errorf("the signature of %s is not in *Counter's referenced set", m.Name)
		}
	}
	// Counter's own descriptor has no row for a pointer receiver method, so it
	// owes no descriptor for one either.
	refs, err = rtype.Referenced(counter)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range counter.Methods {
		found := false
		for _, r := range refs {
			if r == m.Sig {
				found = true
			}
		}
		if found == m.PtrOnly {
			t.Errorf("the signature of %s is in Counter's referenced set: %v, want %v", m.Name, found, !m.PtrOnly)
		}
	}
}

// TestMethodWithNoSignatureIsRefusedByName is the other half of the same
// refusal, and it is the half that is still a boundary gap.
//
// A method built below the type boundary carries no signature. The descriptor
// needs it for Mtyp, and zero is a legal Mtyp meaning "unexported, reflect may
// not call it", so a zero written for the gap would be read as a fact. The
// refusal has to name the signature for that method and not for one that has
// one, which is why the two messages are separate.
func TestMethodWithNoSignatureIsRefusedByName(t *testing.T) {
	byHand := &ir.Type{
		Kind: ir.Int64, Name: "p.T", PkgPath: "p", Basic: "int64",
		Methods: []ir.Method{{Name: "M"}},
	}
	if err := ir.Layout(byHand); err != nil {
		t.Fatal(err)
	}
	_, err := rtype.Descriptor(byHand)
	if err == nil {
		t.Fatal("a method with no signature produced a descriptor")
	}
	if !strings.Contains(err.Error(), "signature") || !strings.Contains(err.Error(), "M") {
		t.Errorf("the refusal is %q, want it to name the signature and the method", err)
	}

	// A set where one method has a signature and one does not names the one
	// that does not, because that is the gap a reader can close.
	sig := &ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{}}
	if err := ir.Layout(sig); err != nil {
		t.Fatal(err)
	}
	byHand.Methods = []ir.Method{{Name: "A", Sig: sig}, {Name: "Z"}}
	_, err = rtype.Descriptor(byHand)
	if err == nil {
		t.Fatal("a method with no signature produced a descriptor")
	}
	if !strings.Contains(err.Error(), "Z") {
		t.Errorf("the refusal is %q, want it to name Z, the method with no signature", err)
	}
}

// decodeName reads an internal/abi.Name's string.
func decodeName(t *testing.T, data []byte) string {
	t.Helper()
	n, err := decodeNameAt(data, 1)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// decodeField reads a struct field's name, tag and flags.
func decodeField(t *testing.T, data []byte) (name, tag string, exported, embedded bool) {
	t.Helper()
	bits := data[0]
	exported = bits&(1<<0) != 0
	embedded = bits&(1<<3) != 0
	name, err := decodeNameAt(data, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bits&(1<<1) != 0 {
		off := 1 + uvarintLen(data[1:]) + len(name)
		if tag, err = decodeNameAt(data, off); err != nil {
			t.Fatal(err)
		}
	}
	return name, tag, exported, embedded
}

func decodeNameAt(data []byte, off int) (string, error) {
	if off >= len(data) {
		return "", fmt.Errorf("a name that ends at %d of %d bytes", off, len(data))
	}
	n, used := binary.Uvarint(data[off:])
	if used <= 0 {
		return "", fmt.Errorf("a name with no length")
	}
	start := off + used
	if start+int(n) > len(data) {
		return "", fmt.Errorf("a name of %d bytes in %d", n, len(data)-start)
	}
	return string(data[start : start+int(n)]), nil
}

func uvarintLen(data []byte) int {
	_, used := binary.Uvarint(data)
	return used
}

// TestNamedDescriptorRefusals checks that the types this compiler cannot
// describe are refused with the reason, rather than described wrongly.
func TestNamedDescriptorRefusals(t *testing.T) {
	_, pkg := namedTypes(t)
	c := ir.NewConverter()
	for _, tc := range namedRefusals {
		obj := pkg.Scope().Lookup(tc.src)
		if obj == nil {
			t.Fatalf("%s is not declared", tc.src)
		}
		typ, err := c.Convert(obj.Type())
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		_, err = rtype.Descriptor(typ)
		if err == nil {
			t.Errorf("%s was described", tc.src)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the refusal is %q, want it to name %q", tc.src, err, tc.want)
		}
	}
}

// TestSymbolNamesAgainstGCObject is the oracle specs/032 names: gc's own
// object, read with go tool nm.
//
// The corpus is compiled a third time, by gc, into a package of its own, and
// every symbol nanogo emits for the same type has to be there under the same
// name and at the same size. A name that differs by one character is a second
// symbol for data that already has one and the linker cannot merge the two,
// which is the deduplication failure specs/032 records. A size that differs is
// a descriptor whose sections gc places elsewhere, which a name comparison
// alone would miss.
//
// The object and not the linked binary. The linker merges these symbols by
// content and drops their names, so nm on a binary reports none of them.
//
// Only the symbols this package defines are compared. A runtime function a
// descriptor points at is an undefined reference in gc's object, and rtsym is
// what checks those names.
func TestSymbolNamesAgainstGCObject(t *testing.T) {
	gc := gcObjectSymbols(t)
	types, _ := namedTypesUnder(t, oraclePkgPath)
	checked := 0
	for i, c := range namedCorpus {
		syms, err := rtype.Descriptor(types[i])
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		for _, s := range syms {
			if !strings.HasPrefix(s.Name, "type:") || len(s.Data) == 0 {
				// A runtime symbol is a reference in gc's object, and an
				// empty gcbits mask is a zero-size symbol nm does not report.
				continue
			}
			size, ok := gc[s.Name]
			if !ok {
				t.Errorf("%s: gc's object has no %s", c.src, s.Name)
				continue
			}
			checked++
			if size != int64(len(s.Data)) {
				t.Errorf("%s: %s is %d bytes and gc's is %d", c.src, s.Name, len(s.Data), size)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no symbol was compared, so the oracle proved nothing")
	}
	t.Logf("gc's object agreed on %d symbols", checked)
}

// oraclePkgPath is the import path of the package gc compiles for the oracle.
const oraclePkgPath = "nanogooracle"

// gcObjectSymbols compiles the corpus with gc and returns the size of every
// symbol go tool nm reports in the object.
func gcObjectSymbols(t *testing.T) map[string]int64 {
	t.Helper()
	goCmd, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go command: %v", err)
	}
	dir := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module "+oraclePkgPath+"\n\ngo 1.25\n")
	// The package path has to be the one the IR types are checked under,
	// because a defined type's link string holds it.
	write("x.go", strings.Replace(namedSource, "package rtype_test", "package "+oraclePkgPath, 1))

	cmd := exec.Command(goCmd, "build", "-o", filepath.Join(dir, "unused.a"), ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOARCH=arm64", "GOOS=darwin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gc could not build the oracle: %v\n%s", err, out)
	}
	list := exec.Command(goCmd, "list", "-export", "-f", "{{.Export}}", ".")
	list.Dir = dir
	list.Env = cmd.Env
	out, err := list.Output()
	if err != nil {
		t.Skipf("go list -export: %v", err)
	}
	file := strings.TrimSpace(string(out))
	if file == "" {
		t.Skip("go list -export named no object")
	}
	nm, err := exec.Command(goCmd, "tool", "nm", "-size", file).Output()
	if err != nil {
		t.Skipf("go tool nm: %v", err)
	}
	syms := make(map[string]int64)
	for _, line := range strings.Split(string(nm), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		size, err := strconv.ParseInt(f[len(f)-3], 10, 64)
		if err != nil {
			continue
		}
		syms[f[len(f)-1]] = size
	}
	if len(syms) == 0 {
		t.Skip("go tool nm reported nothing")
	}
	return syms
}

// TestChanTypeHeaderAgainstReflect checks the ChanType header field by field.
//
// The direction is the whole reason a channel's descriptor was refused.
// specs/032-type-descriptors-and-itabs.md records the failure it prevents: two
// types sharing one symbol, which the linker merges silently, so the program
// reads one channel type's descriptor for the other's values.
func TestChanTypeHeaderAgainstReflect(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		if c.rt.Kind() != reflect.Chan {
			continue
		}
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			d := syms[0]

			// Elem is a pointer to the element's descriptor, at the start of
			// the header.
			r, ok := reloc(d, rtype.TypeSize)
			if !ok {
				t.Fatal("no element reference")
			}
			want := "type:" + c.rt.Elem().String()
			if c.rt.Elem().PkgPath() != "" {
				want = "type:" + c.rt.Elem().PkgPath() + "." + c.rt.Elem().Name()
			}
			if r.Target != want {
				t.Errorf("element is %s, want %s", r.Target, want)
			}
			if r.Size != 8 {
				t.Errorf("element reference is %d bytes, want a pointer", r.Size)
			}

			// Dir is a full word, because internal/abi spells it as an int.
			got := binary.LittleEndian.Uint64(d.Data[rtype.TypeSize+8:])
			if got != uint64(c.rt.ChanDir()) {
				t.Errorf("Dir %d, want %d", got, c.rt.ChanDir())
			}
			if got > 3 || got == 0 {
				t.Errorf("Dir %d is not one of internal/abi's three directions", got)
			}
		})
	}
}

// TestChanDirectionsDoNotShareASymbol is the failure the direction exists to
// stop, stated as a test.
func TestChanDirectionsDoNotShareASymbol(t *testing.T) {
	types, _ := namedTypes(t)
	var chans []*ir.Type
	for i, c := range namedCorpus {
		if c.rt.Kind() == reflect.Chan {
			chans = append(chans, types[i])
		}
	}
	if len(chans) < 3 {
		t.Fatalf("the corpus holds %d channel types, want the three directions", len(chans))
	}
	// A defined channel type is named by its own name, so the three differ
	// there already. What must also differ is the header, because two
	// packages declaring the same channel type must produce one descriptor
	// and two different ones must not.
	seen := make(map[uint64]bool)
	for _, ct := range chans {
		syms, err := rtype.Descriptor(ct)
		if err != nil {
			t.Fatalf("%s: %v", ct, err)
		}
		dir := binary.LittleEndian.Uint64(syms[0].Data[rtype.TypeSize+8:])
		if seen[dir] {
			t.Errorf("%s writes direction %d, which another channel already wrote", ct, dir)
		}
		seen[dir] = true
	}
}

// TestChanWithNoDirectionIsRefused is type.go's second rule at the encoder: a
// fact the checker did not supply is refused by name and never filled in.
func TestChanWithNoDirectionIsRefused(t *testing.T) {
	byHand := &ir.Type{Kind: ir.Chan, Name: "p.C", PkgPath: "p", Elem: &ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"}, Methods: []ir.Method{}}
	if err := ir.Layout(byHand); err != nil {
		t.Fatal(err)
	}
	_, err := rtype.Descriptor(byHand)
	if err == nil {
		t.Fatal("a channel with no direction produced a descriptor")
	}
	if !strings.Contains(err.Error(), "channel direction") {
		t.Errorf("the refusal is %q and does not name the direction", err)
	}
	// With the direction it is emitted, so the refusal above is about the
	// missing fact and not about channels.
	byHand.ChanDir = ir.SendRecv
	if _, err := rtype.Descriptor(byHand); err != nil {
		t.Errorf("a channel with a direction was still refused: %v", err)
	}
}

// TestReferencedFollowsAChannel checks the edge cmd/link walks.
//
// A package that emits a descriptor owes every descriptor that one reaches.
// cmd/link's defgotype follows the element pointer to build a DWARF entry, and
// an element nobody emitted surfaces as an undefined go:info target that names
// neither the package that owes it nor the fix.
func TestReferencedFollowsAChannel(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		if c.rt.Kind() != reflect.Chan {
			continue
		}
		got, err := rtype.Referenced(types[i])
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		got = withoutPointerToThis(t, types[i], got)
		if len(got) != 1 || got[0] != types[i].Elem {
			t.Errorf("%s reaches %v, want its element", c.src, got)
		}
	}
}

// withoutPointerToThis takes the PtrToThis entry off a reference list.
//
// Every defined type reaches the descriptor of a pointer to it, because that
// is the symbol its PtrToThis field names, and it is the last entry. A test
// about the edges of one kind checks those edges and not this one, which
// rtype_test.go's TestReferencedFollowsPointerToThis owns.
func withoutPointerToThis(t *testing.T, of *ir.Type, got []*ir.Type) []*ir.Type {
	t.Helper()
	want, err := rtype.PointerToThis(of)
	if err != nil {
		t.Fatalf("%s: %v", of, err)
	}
	if want == nil {
		return got
	}
	if len(got) == 0 {
		t.Fatalf("%s reaches nothing and owes the descriptor of a pointer to it", of)
	}
	last := got[len(got)-1]
	if last.Kind != ir.Ptr || last.Elem != of {
		t.Fatalf("the last type %s reaches is %s, want a pointer to it", of, last)
	}
	return got[:len(got)-1]
}

// TestFuncTypeHeaderAgainstReflect checks the FuncType header and the array
// that follows it, against the descriptor gc emitted for the same type.
//
// The array is one array for both lists, split by the two counts. reflect
// reads In(i) from the first InCount entries and Out(i) from the rest, so an
// array written in the wrong order reports a function's results as its
// parameters and nothing about the bytes says so.
func TestFuncTypeHeaderAgainstReflect(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		if c.rt.Kind() != reflect.Func {
			continue
		}
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			d := syms[0]

			in := binary.LittleEndian.Uint16(d.Data[rtype.TypeSize:])
			if int(in) != c.rt.NumIn() {
				t.Errorf("InCount %d, want %d", in, c.rt.NumIn())
			}
			raw := binary.LittleEndian.Uint16(d.Data[rtype.TypeSize+2:])
			out := raw &^ (1 << 15)
			if int(out) != c.rt.NumOut() {
				t.Errorf("OutCount %d, want %d", out, c.rt.NumOut())
			}
			if variadic := raw&(1<<15) != 0; variadic != c.rt.IsVariadic() {
				t.Errorf("the variadic bit is %v, want %v", variadic, c.rt.IsVariadic())
			}

			// The array starts after the UncommonType, because a defined type
			// has one and Moff is measured past it.
			base := rtype.TypeSize + 8 + 16
			for j := 0; j < c.rt.NumIn()+c.rt.NumOut(); j++ {
				var want reflect.Type
				if j < c.rt.NumIn() {
					want = c.rt.In(j)
				} else {
					want = c.rt.Out(j - c.rt.NumIn())
				}
				r, ok := reloc(d, int32(base+j*8))
				if !ok {
					t.Errorf("entry %d has no reference", j)
					continue
				}
				if got := r.Target; got != linkSymbol(want) {
					t.Errorf("entry %d is %s, want %s", j, got, linkSymbol(want))
				}
				if r.Size != 8 {
					t.Errorf("entry %d is %d bytes, want a pointer", j, r.Size)
				}
			}
		})
	}
}

// linkSymbol is the descriptor symbol of a reflect.Type.
//
// It is the "type:" prefix and the link string. The link string qualifies a
// defined type by its import path rather than by its package name, which is
// the one difference from reflect.Type.String, so a composite type has to be
// spelled out rather than taken from String.
func linkSymbol(rt reflect.Type) string { return "type:" + linkString(rt) }

func linkString(rt reflect.Type) string {
	if rt.Name() != "" {
		if rt.PkgPath() != "" {
			return rt.PkgPath() + "." + rt.Name()
		}
		return rt.Name()
	}
	switch rt.Kind() {
	case reflect.Pointer:
		return "*" + linkString(rt.Elem())
	case reflect.Slice:
		return "[]" + linkString(rt.Elem())
	case reflect.Array:
		return "[" + strconv.Itoa(rt.Len()) + "]" + linkString(rt.Elem())
	}
	return rt.String()
}

// TestFuncWithNoSignatureIsRefused is the gap the encoder refuses by name.
//
// ir.Converter sets Params and Results on every function type, the empty list
// included, so a nil list means the type was built below the boundary. A
// descriptor written from it would claim func(), and reflect would report a
// function of no arguments for one that takes three.
func TestFuncWithNoSignatureIsRefused(t *testing.T) {
	byHand := &ir.Type{Kind: ir.FuncKind, Name: "p.F", PkgPath: "p", Methods: []ir.Method{}}
	if err := ir.Layout(byHand); err != nil {
		t.Fatal(err)
	}
	_, err := rtype.Descriptor(byHand)
	if err == nil {
		t.Fatal("a function with no signature produced a descriptor")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("the refusal is %q and does not name the signature", err)
	}
	byHand.Params = []*ir.Type{}
	byHand.Results = []*ir.Type{}
	if _, err := rtype.Descriptor(byHand); err != nil {
		t.Errorf("a function with an empty signature was still refused: %v", err)
	}
	// The variadic bit says the last parameter is a ... parameter. With no
	// parameters, reflect reads In(-1).
	byHand.Variadic = true
	if _, err := rtype.Descriptor(byHand); err == nil {
		t.Error("a variadic function with no parameters produced a descriptor")
	}
}

// TestReferencedFollowsAFunction checks the edges cmd/link walks.
func TestReferencedFollowsAFunction(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		if c.rt.Kind() != reflect.Func {
			continue
		}
		got, err := rtype.Referenced(types[i])
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		got = withoutPointerToThis(t, types[i], got)
		if len(got) != c.rt.NumIn()+c.rt.NumOut() {
			t.Errorf("%s reaches %d types, want %d", c.src, len(got), c.rt.NumIn()+c.rt.NumOut())
		}
	}
}

// TestNamedEmptyInterfaceCarriesItsPackage is the bug the InterfaceType
// encoder replaced.
//
// Every interface got thirty-two zero bytes, on the reasoning that an empty
// interface has a nil package path and an empty method slice. That is true of
// `any` and of `error`, whose descriptors the runtime owns, and false of a
// named empty interface: gc writes the declaring package's path, and
// reflect.Type.PkgPath reads it.
func TestNamedEmptyInterfaceCarriesItsPackage(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		if c.rt.Kind() != reflect.Interface || c.rt.NumMethod() != 0 {
			// An interface with methods has a method slice, and the three zero
			// words below are what an empty one has instead.
			continue
		}
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			d := syms[0]
			r, ok := reloc(d, rtype.TypeSize)
			if !ok {
				t.Fatal("no package path reference; reflect reports a package for this type")
			}
			want := "type:.importpath." + namedPkgPath() + "."
			if r.Target != want {
				t.Errorf("package path is %s, want %s", r.Target, want)
			}
			if r.Size != 8 {
				t.Errorf("package path is %d bytes, want a pointer", r.Size)
			}
			sym, ok := find(syms, want)
			if !ok {
				t.Fatalf("%s is not defined", want)
			}
			if got := decodeName(t, sym.Data); got != c.rt.PkgPath() {
				t.Errorf("package path is %q, want %q", got, c.rt.PkgPath())
			}
			// An empty interface's method slice is three zero words.
			for off := rtype.TypeSize + 8; off < rtype.TypeSize+32; off += 8 {
				if got := binary.LittleEndian.Uint64(d.Data[off:]); got != 0 {
					t.Errorf("the method slice word at %d is %#x, want 0", off, got)
				}
			}
			if _, ok := reloc(d, int32(rtype.TypeSize+8)); ok {
				t.Error("an empty interface's method slice is relocated")
			}
		})
	}
}

// TestAnyKeepsANilPackagePath is the other half of the rule above.
//
// `any` is a type literal with no package, and `error` is predeclared and the
// runtime owns its descriptor. A path written for either is a second symbol
// for a string that already has one, and for `error` a second descriptor for a
// type the runtime already defines.
func TestAnyKeepsANilPackagePath(t *testing.T) {
	types := corpusTypes(t)
	for i, c := range corpus {
		if c.rt.Kind() != reflect.Interface {
			continue
		}
		syms, err := rtype.Descriptor(types[i])
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if _, ok := reloc(syms[0], rtype.TypeSize); ok {
			t.Errorf("%s: the package path is relocated and gc leaves it zero", c.src)
		}
	}
}

// TestInterfaceWithMethodsIsDescribed is the row that was refused while a
// function literal had no spelling.
//
// An Imethod's Typ is an offset to the descriptor of the method's signature,
// and that signature is a function literal. The refusal came from one line
// above this package, in ir/rtype.go's naming function, and it is gone: the
// descriptor is written and each method points at the symbol gc names for the
// same signature.
func TestInterfaceWithMethodsIsDescribed(t *testing.T) {
	_, pkg := namedTypes(t)
	c := ir.NewConverter()
	reader, err := c.Convert(pkg.Scope().Lookup("Reader").Type())
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.Methods) != 2 {
		t.Fatalf("Reader carries %d methods, want 2", len(reader.Methods))
	}
	syms, err := rtype.Descriptor(reader)
	if err != nil {
		t.Fatalf("a defined interface with methods: %v", err)
	}
	// The Imethod array is in the C..D section, whose offset the header's
	// slice points at. Read the targets of the four-byte offsets instead, in
	// the order they were emitted: name, signature, name, signature.
	var offsets []string
	for _, r := range syms[0].Relocs {
		if r.Size == 4 && strings.HasPrefix(r.Target, "type:") {
			offsets = append(offsets, r.Target)
		}
	}
	// Exported first, which is gc's order for an Imethod array.
	for _, want := range []string{
		"type:func([]uint8) (int, error)",
		"type:func() error",
	} {
		found := false
		for _, got := range offsets {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no method points at %s; the offsets are %v", want, offsets)
		}
	}
}

// TestLiteralInterfaceIsNamedAsGcNamesIt checks the spelling that qualifies an
// unexported method with its package.
//
// Two packages may declare an unexported method of one name and they are
// different methods, so the qualifier is part of the type. gc writes the import
// path in the link string and the package name in the name string. The source
// is type-checked under this package's own import path, so the two compilers
// are naming one type, and the hash gc computed over its link string is the
// oracle for the link string this one builds.
func TestLiteralInterfaceIsNamedAsGcNamesIt(t *testing.T) {
	_, pkg := namedTypes(t)
	c := ir.NewConverter()
	lit, err := c.Convert(pkg.Scope().Lookup("ReaderLit").Type())
	if err != nil {
		t.Fatal(err)
	}
	rt := reflect.TypeOf((*ReaderLit)(nil)).Elem()
	if got, err := ir.TypeNameString(lit); err != nil || got != rt.String() {
		t.Errorf("the name string is %q (%v), want %q", got, err, rt.String())
	}
	link, err := ir.TypeLinkString(lit)
	if err != nil {
		t.Fatal(err)
	}
	if want := namedPkgPath() + ".flush"; !strings.Contains(link, want) {
		t.Errorf("the link string is %q, want it to qualify the unexported method as %s", link, want)
	}
	got, err := rtype.Hash(lit)
	if err != nil {
		t.Fatal(err)
	}
	if want := abiOf(rt).Hash; got != want {
		t.Errorf("the hash of %q is %#08x and gc computed %#08x, so the two link strings differ", link, got, want)
	}
}

// TestReferencedFollowsAnInterface checks the edges cmd/link walks.
//
// An Imethod's Typ is an offset to the descriptor of the method's signature,
// so a package that emits an interface's descriptor owes one descriptor per
// method.
func TestReferencedFollowsAnInterface(t *testing.T) {
	_, pkg := namedTypes(t)
	c := ir.NewConverter()
	reader, err := c.Convert(pkg.Scope().Lookup("Reader").Type())
	if err != nil {
		t.Fatal(err)
	}
	got, err := rtype.Referenced(reader)
	if err != nil {
		t.Fatal(err)
	}
	got = withoutPointerToThis(t, reader, got)
	if len(got) != 2 {
		t.Fatalf("Reader reaches %d types, want one per method", len(got))
	}
	// Exported first, then by name, which is gc's order for an Imethod array
	// and not the IR's.
	for i, want := range []string{"Read", "flush"} {
		if got[i] != reader.Methods[0].Sig && got[i].Kind != ir.FuncKind {
			t.Errorf("entry %d is %s, want the signature of %s", i, got[i].Kind, want)
		}
	}
	// An empty interface reaches no method, so what is left is its PtrToThis.
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		if c.src != "NamedEmpty" {
			continue
		}
		got, err := rtype.Referenced(types[i])
		if err != nil {
			t.Fatal(err)
		}
		got = withoutPointerToThis(t, types[i], got)
		if len(got) != 0 {
			t.Errorf("an empty interface reaches %v", got)
		}
	}
}

// TestGeneratedEqualityDescribesAFieldWiseStruct is what replaced two rows of
// namedRefusals.
//
// A struct that compares field by field has no runtime algorithm its Equal can
// point at, and that is why it used to be refused. ssagen generates the
// function now and rtype points a closure at it, so the descriptor is written.
//
// The assertion is that Equal is not nil rather than that the type is
// described. A nil Equal is not an absence the runtime tolerates: it panics on
// a map whose key type has one, so a descriptor emitted with a nil there is
// worse than the refusal it replaced.
func TestGeneratedEqualityDescribesAFieldWiseStruct(t *testing.T) {
	_, pkg := namedTypes(t)
	c := ir.NewConverter()
	for _, name := range []string{"Wide", "Label"} {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("%s is not declared", name)
		}
		typ, err := c.Convert(obj.Type())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		syms, err := rtype.Descriptor(typ)
		if err != nil {
			t.Errorf("%s is refused: %v", name, err)
			continue
		}
		var named bool
		for _, sym := range syms {
			for _, r := range sym.Relocs {
				if strings.Contains(r.Target, ".eqfunc.") || strings.Contains(r.Target, ".eq.") {
					named = true
				}
			}
		}
		if !named {
			t.Errorf("%s is described and nothing names its equality function, so Equal is nil and the runtime will panic on a map keyed by it", name)
		}
	}
}

// The itab, checked three ways: against the symbol gc wrote for the same pair,
// against the itab the running program holds for it, and against the rule that
// makes an itab identity rather than a table.

// abiITab mirrors internal/abi.ITab, for the reason abiType is mirrored: this
// file runs in the target's own binary.
type abiITab struct {
	Inter *abiType
	Type  *abiType
	Hash  uint32
	_     uint32
	Fun   [1]uintptr
}

// gcITab returns the itab gc built for the pair (Counter, Valuer).
//
// A non-empty interface leads with an *ITab, so the first word of the
// interface value is the itab itself. reflect exposes none of these fields,
// which is why the value is read through the word rather than through reflect.
func gcITab() *abiITab {
	return (*abiITab)((*eface)(unsafe.Pointer(&valuerHolder)).typ)
}

// TestItabAgainstTheRunningItab checks the three fields that are not code
// pointers against the itab this process is holding.
func TestItabAgainstTheRunningItab(t *testing.T) {
	types, _ := namedTypes(t)
	counter, iface := byCorpusName(t, types, "Counter"), byCorpusName(t, types, "Valuer")
	syms, err := rtype.Itab(counter, iface)
	if err != nil {
		t.Fatalf("Itab: %v", err)
	}
	if len(syms) != 1 {
		t.Fatalf("an itab is %d symbols, want 1", len(syms))
	}
	d := syms[0]
	want := gcITab()

	// One method, so Fun holds one word and the itab is thirty-two bytes.
	if got, wantN := len(d.Data), 24+8*reflect.TypeOf((*Valuer)(nil)).Elem().NumMethod(); got != wantN {
		t.Errorf("the itab is %d bytes, want %d", got, wantN)
	}
	if got := binary.LittleEndian.Uint32(d.Data[16:]); got != want.Hash {
		t.Errorf("Hash %#08x, want %#08x, which is the concrete type's", got, want.Hash)
	}
	// The hash is the concrete type's and not the interface's. A type switch
	// reads it out of the itab, so the two have to be one number.
	h, err := rtype.Hash(counter)
	if err != nil {
		t.Fatal(err)
	}
	if h != want.Hash {
		t.Errorf("the itab's hash is %#08x and Counter's descriptor holds %#08x", want.Hash, h)
	}

	// The first two words are pointers to the two descriptors, in that order.
	inter, ok := reloc(d, 0)
	if !ok {
		t.Fatal("the itab has no Inter reference")
	}
	if inter.Size != 8 || inter.Type != obj.R_ADDR {
		t.Errorf("Inter is a %v of %d bytes, want an eight-byte R_ADDR", inter.Type, inter.Size)
	}
	if wantSym, _ := ir.TypeSymbol(iface); inter.Target != wantSym {
		t.Errorf("Inter names %s, want %s", inter.Target, wantSym)
	}
	typ, ok := reloc(d, 8)
	if !ok {
		t.Fatal("the itab has no Type reference")
	}
	if wantSym, _ := ir.TypeSymbol(counter); typ.Target != wantSym {
		t.Errorf("Type names %s, want %s", typ.Target, wantSym)
	}

	// Fun[0] is Counter's Value under the entry point an itab call reaches. A
	// Counter is an int, so the word an itab passes is a pointer and the entry
	// point is the wrapper on it.
	fun, ok := reloc(d, 24)
	if !ok {
		t.Fatal("the itab has no Fun[0] reference")
	}
	if want := namedPkgPath() + ".(*Counter).Value"; fun.Target != want {
		t.Errorf("Fun[0] names %s, want %s", fun.Target, want)
	}
	// Strong and not weak. cmd/link resolves a weak reference to zero when it
	// cannot prove the target live, and nanogo emits no R_USEIFACEMETHOD to
	// prove it with, so a weak entry here is a call to address zero.
	if fun.Type&obj.R_WEAK != 0 {
		t.Error("Fun[0] is a weak reference, so cmd/link may resolve it to zero")
	}
}

// TestItabFunIsInTheInterfacesOrder checks the slot order, which is the itab's
// silent failure.
//
// The runtime reads Fun by the *interface's* index. Every entry holds a
// function of the right shape whatever the order is, so slots that are swapped
// call the wrong method and nothing between the conversion and the call
// notices. gc puts every exported name first, so Ärger comes second here and
// byte order by name would put it last.
func TestItabFunIsInTheInterfacesOrder(t *testing.T) {
	types, _ := namedTypes(t)
	counter, iface := byCorpusName(t, types, "Counter"), byCorpusName(t, types, "Counted")
	syms, err := rtype.Itab(counter, iface)
	if err != nil {
		t.Fatalf("Itab: %v", err)
	}
	d := syms[0]
	pkg := namedPkgPath()
	want := []string{
		pkg + ".(*Counter).Value",
		pkg + ".(*Counter).Ärger",
		pkg + ".(*Counter).hidden",
	}
	if got := len(d.Data); got != 24+8*len(want) {
		t.Fatalf("the itab is %d bytes and holds %d methods", got, len(want))
	}
	for i, w := range want {
		r, ok := reloc(d, int32(24+8*i))
		if !ok {
			t.Fatalf("Fun[%d] has no reference", i)
		}
		if r.Target != w {
			t.Errorf("Fun[%d] names %s, want %s", i, r.Target, w)
		}
	}
	// The interface's own Imethod array is the same list in the same order, so
	// the two cannot disagree without one of them being wrong.
	imethods, err := rtype.Descriptor(iface)
	if err != nil {
		t.Fatal(err)
	}
	// The array sits after the UncommonType, which a named interface has.
	base := uncommonOffsetOf(reflect.TypeOf((*Counted)(nil)).Elem()) + 16
	for i, w := range []string{"Value", "Ärger", "hidden"} {
		r, ok := reloc(imethods[0], int32(base+8*i))
		if !ok {
			t.Fatalf("Imethod %d has no name reference", i)
		}
		def, ok := find(imethods, r.Target)
		if !ok {
			t.Fatalf("%s is not defined", r.Target)
		}
		if got := decodeName(t, def.Data); got != w {
			t.Errorf("Imethod %d is %q and Fun[%d] is %s", i, got, i, want[i])
		}
	}
}

// TestItabNameIsTheSameFromTwoConverters is the identity property, stated as
// the compiler meets it.
//
// A package compiles under one converter and another package under another, so
// one Go type is two ir.Types. The name is a function of the type and not of
// the pointer, or the linker would keep two itabs for one pair and every
// comparison between an interface value from one package and one from the
// other would be false.
func TestItabNameIsTheSameFromTwoConverters(t *testing.T) {
	first, _ := namedTypes(t)
	second, _ := namedTypes(t)
	a, err := ir.ItabSymbol(byCorpusName(t, first, "Counter"), byCorpusName(t, first, "Counted"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ir.ItabSymbol(byCorpusName(t, second, "Counter"), byCorpusName(t, second, "Counted"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("two converters name one pair %s and %s", a, b)
	}
}

// TestItabNameAgainstGCObject is the identity check.
//
// One itab per (interface, concrete type) pair, and the linker merges two
// duplicate-tolerant symbols of one name. A name that differs from gc's by one
// character is a second itab for a pair that must have one, and the runtime
// compares two interface values by comparing their first words, so every such
// comparison is then false.
func TestItabNameAgainstGCObject(t *testing.T) {
	gc := gcObjectSymbols(t)
	types, _ := namedTypesUnder(t, oraclePkgPath)
	counter := byCorpusName(t, types, "Counter")
	for _, name := range []string{"Valuer", "Counted"} {
		syms, err := rtype.Itab(counter, byCorpusName(t, types, name))
		if err != nil {
			t.Fatalf("Itab: %v", err)
		}
		size, ok := gc[syms[0].Name]
		if !ok {
			var itabs []string
			for sym := range gc {
				if strings.HasPrefix(sym, ir.ItabSymbolPrefix) {
					itabs = append(itabs, sym)
				}
			}
			sort.Strings(itabs)
			t.Errorf("gc's object has no %s; it holds %v", syms[0].Name, itabs)
			continue
		}
		if size != int64(len(syms[0].Data)) {
			t.Errorf("%s is %d bytes and gc's is %d", syms[0].Name, len(syms[0].Data), size)
		}
	}
}

// TestItabRefusesWhatHasNoItab records the three shapes that have none.
func TestItabRefusesWhatHasNoItab(t *testing.T) {
	types, _ := namedTypes(t)
	counter := byCorpusName(t, types, "Counter")
	iface := byCorpusName(t, types, "Valuer")
	empty := byCorpusName(t, types, "NamedEmpty")
	flags := byCorpusName(t, types, "Flags")
	for _, tc := range []struct {
		what       string
		typ, iface *ir.Type
		want       string
	}{
		{"an empty interface", counter, empty, "empty interface"},
		{"an interface on both sides", iface, iface, "is an interface"},
		{"a type that does not implement it", flags, iface, "does not implement"},
	} {
		if _, err := rtype.Itab(tc.typ, tc.iface); err == nil {
			t.Errorf("%s: an itab was written", tc.what)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the refusal is %q, want it to say %q", tc.what, err, tc.want)
		}
	}
}

// TestItabReferencesBothDescriptors checks the set the caller owes.
//
// cmd/link resolves both of the itab's first two words by name, so a package
// that writes an itab and not the two descriptors links to nothing.
func TestItabReferencesBothDescriptors(t *testing.T) {
	types, _ := namedTypes(t)
	counter, iface := byCorpusName(t, types, "Counter"), byCorpusName(t, types, "Valuer")
	refs, err := rtype.ItabReferenced(counter, iface)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] != iface || refs[1] != counter {
		t.Fatalf("an itab references %v, want the interface and the concrete type", refs)
	}
}

// byCorpusName returns the IR type of one corpus row.
func byCorpusName(t *testing.T, types []*ir.Type, name string) *ir.Type {
	t.Helper()
	for i, c := range namedCorpus {
		if c.src == name {
			return types[i]
		}
	}
	t.Fatalf("%s is not in the named corpus", name)
	return nil
}
