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
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
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
	{"Wide", "generated equality function"},
	{"Label", "generated equality function"},
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

// Counter is not in the corpus. It is the type whose descriptor must be
// refused, because a method needs two things this compiler does not have.
type Counter int

func (Counter) String() string { return "" }
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

// TestUncommonTypeCarriesThePackagePath checks the section that a defined type
// exists for.
//
// reflect.Type.PkgPath reads the UncommonType, so a descriptor without one
// reports an empty path for a type that has one, and a TFlagUncommon set with
// no section makes the runtime read past the end of the symbol.
func TestUncommonTypeCarriesThePackagePath(t *testing.T) {
	types, _ := namedTypes(t)
	for i, c := range namedCorpus {
		t.Run(c.src, func(t *testing.T) {
			syms, err := rtype.Descriptor(types[i])
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			d := syms[0]
			// B is where the UncommonType starts: the end of
			// internal/abi.Type plus the kind-specific header.
			b := rtype.TypeSize
			switch c.rt.Kind() {
			case reflect.Struct:
				b += 32
			case reflect.Slice:
				b += 8
			case reflect.Array:
				b += 24
			case reflect.Chan:
				b += 16
			case reflect.Func:
				b += 8
			}
			if got := binary.LittleEndian.Uint16(d.Data[b+4:]); got != uint16(c.rt.NumMethod()) {
				t.Errorf("Mcount %d, want %d", got, c.rt.NumMethod())
			}
			if got := binary.LittleEndian.Uint16(d.Data[b+6:]); got != 0 {
				t.Errorf("Xcount %d, want 0", got)
			}
			// Moff is measured from the UncommonType and not from the
			// descriptor, and it skips the variable-length data, so it is the
			// distance from B to the end.
			if got := binary.LittleEndian.Uint32(d.Data[b+8:]); int(got) != len(d.Data)-b {
				t.Errorf("Moff %d, want %d", got, len(d.Data)-b)
			}

			r, ok := reloc(d, int32(b))
			if !ok {
				t.Fatal("no package path reference")
			}
			want := "type:.importpath." + namedPkgPath() + "."
			if r.Target != want {
				t.Errorf("package path is %s, want %s", r.Target, want)
			}
			if r.Size != 4 {
				t.Errorf("package path is %d bytes, want a four-byte NameOff", r.Size)
			}
			sym, ok := find(syms, want)
			if !ok {
				t.Fatalf("%s is not defined", want)
			}
			if got := decodeName(t, sym.Data); got != c.rt.PkgPath() {
				t.Errorf("package path is %q, want %q", got, c.rt.PkgPath())
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

// TestDescriptorRefusesAMethod checks that a type with a method is refused by
// name rather than described with an empty method set.
//
// The refusal names the two ABI wrappers and no longer names the signature.
// ir.Method.Sig carries the method's type now, so a refusal that still said
// the signature was absent would send the next reader to a file with nothing
// left to fix in it.
func TestDescriptorRefusesAMethod(t *testing.T) {
	_, pkg := namedTypes(t)
	c := ir.NewConverter()
	counter, err := c.Convert(pkg.Scope().Lookup("Counter").Type())
	if err != nil {
		t.Fatal(err)
	}
	if counter.Methods[0].Sig == nil {
		t.Fatal("Counter.String carries no signature, so this test cannot tell the two gaps apart")
	}
	_, err = rtype.Descriptor(counter)
	if err == nil {
		t.Fatal("a type with a method was described")
	}
	for _, want := range []string{"String", "ABI wrappers", "Ifn", "Tfn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q, want it to name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "signature") {
		t.Errorf("the refusal is %q and still claims the signature is missing", err)
	}
	// The pointer's set is the larger one, so a pointer to it is refused too.
	ptr := &ir.Type{Kind: ir.Ptr, Elem: counter}
	if err := ir.Layout(ptr); err != nil {
		t.Fatal(err)
	}
	if _, err := rtype.Descriptor(ptr); err == nil {
		t.Error("a pointer to a type with a method was described")
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
		if len(got) != 1 || got[0] != types[i].Elem {
			t.Errorf("%s reaches %v, want its element", c.src, got)
		}
	}
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
		if len(got) != c.rt.NumIn()+c.rt.NumOut() {
			t.Errorf("%s reaches %d types, want %d", c.src, len(got), c.rt.NumIn()+c.rt.NumOut())
		}
	}
}
