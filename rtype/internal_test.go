// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtsym"
)

// The guards that Descriptor cannot reach today.
//
// Descriptor asks for the canonical name first, so a type with no name never
// reaches the encoders, and every kind whose contents are missing has no name.
// The encoders still answer for those kinds, because the set of emittable
// types grows as the IR type boundary gains fields, and a guard that is added
// at the same time as its caller is a guard that was never checked.
//
// So they are called here directly. It is the contract the next author reads
// when a refusal above is lifted: this is what each encoder does with a kind
// it cannot describe, and it is a refusal rather than a plausible number.

// structOf lays out a literal struct for the algorithm tests.
func structOf(t *testing.T, fields ...ir.Field) *ir.Type {
	t.Helper()
	out := &ir.Type{Kind: ir.Struct, Fields: fields}
	if err := ir.Layout(out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAlgAnswersForEveryKind(t *testing.T) {
	str := &ir.Type{Kind: ir.String, Name: "string", Basic: "string"}
	b := &ir.Type{Kind: ir.Uint8, Name: "uint8", Basic: "uint8"}
	i64 := &ir.Type{Kind: ir.Int64, Name: "int64", Basic: "int64"}
	twoStrings := structOf(t, ir.Field{Name: "A", Type: str}, ir.Field{Name: "B", Type: str})
	oneString := structOf(t, ir.Field{Name: "A", Type: str})
	twoBytes := structOf(t, ir.Field{Name: "A", Type: b}, ir.Field{Name: "B", Type: b})
	// One byte then an eight-byte field leaves seven bytes of padding, which a
	// memory comparison would read.
	padded := structOf(t, ir.Field{Name: "A", Type: b}, ir.Field{Name: "B", Type: i64})
	withSlice := structOf(t,
		ir.Field{Name: "A", Type: i64},
		ir.Field{Name: "B", Type: &ir.Type{Kind: ir.Slice, Elem: i64}})
	for _, tc := range []struct {
		what string
		typ  *ir.Type
		want algKind
	}{
		{"nothing", nil, algNone},
		{"a void", &ir.Type{Kind: ir.Void}, algNone},
		{"an invalid type", &ir.Type{Kind: ir.Invalid}, algNone},
		// A struct compares field by field whenever a field does not compare
		// as memory, or is blank, or is followed by padding. One with no
		// fields at all is nothing to compare and reaches memequal0.
		{"an empty struct", &ir.Type{Kind: ir.Struct}, algMem},
		{"a struct of two strings", twoStrings, algSpecial},
		{"a struct of one string", oneString, algString},
		{"a struct of two bytes", twoBytes, algMem},
		{"a padded struct", padded, algSpecial},
		{"a struct holding a slice", withSlice, algNone},
		// A tuple has a struct's layout and so has a struct's algorithm. No
		// tuple is ever compared, and answering as a struct is what keeps the
		// two from drifting apart.
		{"a tuple of two strings", &ir.Type{Kind: ir.Tuple, Fields: twoStrings.Fields}, algSpecial},
		// The two interface layouts read their first word differently, so each
		// has its own function and calling the other reads a function pointer
		// at the wrong offset.
		{"an interface with methods", &ir.Type{Kind: ir.Interface}, algIface},
		{"an empty interface", &ir.Type{Kind: ir.Interface, EmptyIface: true}, algNilIface},
		{"a map", &ir.Type{Kind: ir.Map}, algNone},
		{"a function", &ir.Type{Kind: ir.FuncKind}, algNone},
		{"a slice", &ir.Type{Kind: ir.Slice}, algNone},
		{"a channel", &ir.Type{Kind: ir.Chan}, algMem},
		{"a string", str, algString},
		{"an array of structs", &ir.Type{Kind: ir.Array, Len: 2, Elem: twoStrings}, algSpecial},
		{"an array of slices", &ir.Type{Kind: ir.Array, Len: 2, Elem: &ir.Type{Kind: ir.Slice}}, algNone},
		{"an array of strings", &ir.Type{Kind: ir.Array, Len: 2, Elem: str}, algSpecial},
	} {
		if got := alg(tc.typ); got != tc.want {
			t.Errorf("%s: algorithm %d, want %d", tc.what, got, tc.want)
		}
	}
}

func TestEqualFuncRefusesWhatItCannotName(t *testing.T) {
	// A struct whose fields do not compare as one region of memory needs the
	// generated function specs/032 describes.
	str := &ir.Type{Kind: ir.String, Name: "string", Basic: "string"}
	two := structOf(t, ir.Field{Name: "A", Type: str}, ir.Field{Name: "B", Type: str})
	if _, _, err := equalFunc(two); err == nil {
		t.Error("a struct of two strings was given an equality function")
	}
	// An interface with methods reaches runtime.interequal, which reads the
	// first word as an *itab.
	fn, varlen, err := equalFunc(&ir.Type{Kind: ir.Interface})
	if err != nil || fn != "runtime.interequal" || varlen {
		t.Errorf("an interface reaches %q (%v, %v)", fn, varlen, err)
	}
}

func TestAbiKindRefusesAnAmbiguousWord(t *testing.T) {
	for _, tc := range []struct {
		what string
		typ  *ir.Type
		want string
	}{
		{"a word with no name", &ir.Type{Kind: ir.Int64}, "does not say which"},
		{"an unsigned word with no name", &ir.Type{Kind: ir.Uint64}, "does not say which"},
		{"a defined word", &ir.Type{Kind: ir.Int64, Name: "time.Duration"}, "does not say which"},
		{"a kind with no descriptor", &ir.Type{Kind: ir.Void}, "no abi kind"},
	} {
		_, err := abiKind(tc.typ)
		if err == nil {
			t.Errorf("%s: was given a kind", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the reason is %q, want it to name %q", tc.what, err, tc.want)
		}
	}
	// The two spellings that are decidable.
	for name, want := range map[string]uint8{"int": abiInt, "int64": abiInt64, "uint": abiUint, "uint64": abiUint64} {
		k := ir.Int64
		if strings.HasPrefix(name, "u") {
			k = ir.Uint64
		}
		got, err := abiKind(&ir.Type{Kind: k, Name: name})
		if err != nil || got != want {
			t.Errorf("%s: kind %d (%v), want %d", name, got, err, want)
		}
	}
}

func TestKindTailRefusesWhatItCannotDescribe(t *testing.T) {
	// A kind with no header written.
	if _, _, _, err := kindTail(&ir.Type{Kind: ir.Map}, "type:m", 64); err == nil {
		t.Error("a map was given a header")
	}
	// An element with no canonical name stops the composite that holds it.
	for _, tc := range []struct {
		what string
		typ  *ir.Type
	}{
		{"a slice of channels", &ir.Type{Kind: ir.Slice, Elem: &ir.Type{Kind: ir.Chan}}},
		{"a pointer to a function", &ir.Type{Kind: ir.Ptr, Elem: &ir.Type{Kind: ir.FuncKind}}},
		{"an array of channels", &ir.Type{Kind: ir.Array, Len: 2, Elem: &ir.Type{Kind: ir.Chan}}},
	} {
		if _, _, _, err := kindTail(tc.typ, "type:x", 64); err == nil {
			t.Errorf("%s: was given a header", tc.what)
		}
	}
	// A struct's fields are in the variable-length section rather than in the
	// header, so a field whose type cannot be named stops kindData.
	chans := &ir.Type{Kind: ir.Struct, Fields: []ir.Field{{Name: "A", Type: &ir.Type{Kind: ir.Chan}}}}
	if _, _, _, err := kindData(chans, 64); err == nil {
		t.Error("a struct of channels was given a field array")
	}
	// An array whose element does not lay out cannot name its slice type.
	if _, _, _, err := kindTail(&ir.Type{Kind: ir.Array, Len: 2, Elem: &ir.Type{Kind: ir.Void}}, "type:a", 64); err == nil {
		t.Error("an array of voids was given a header")
	}
	// kindTailSize is what places the UncommonType, so a kind whose header it
	// sizes differently from what kindTail builds would overlap the two.
	// Descriptor checks that, and this checks the check is not vacuous.
	for _, k := range []ir.Kind{ir.Ptr, ir.Slice, ir.Array, ir.Interface, ir.Struct, ir.Int64, ir.String} {
		typ := &ir.Type{Kind: k, Len: 1, Elem: &ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"}}
		if err := ir.Layout(typ); err != nil {
			continue
		}
		got, _, _, err := kindTail(typ, "type:x", 64)
		if err != nil {
			continue
		}
		if len(got) != kindTailSize(typ) {
			t.Errorf("%s: kindTail built %d bytes and kindTailSize says %d", k, len(got), kindTailSize(typ))
		}
	}
}

func TestEmittableRefusalsSurviveWithoutTheName(t *testing.T) {
	// Each row is a type built below the boundary and missing a fact the
	// checker supplies, so each is unreachable through Descriptor today. They
	// are the answer for the day a name exists and the contents still do not,
	// and emittable answers for them anyway, because a refusal that depends on
	// which check runs first is a refusal nobody has checked.
	for _, tc := range []struct {
		what string
		typ  *ir.Type
		want string
	}{
		{"a channel", &ir.Type{Kind: ir.Chan}, "direction"},
		{"a function", &ir.Type{Kind: ir.FuncKind}, "signature"},
		{"an interface whose method set nobody computed", &ir.Type{Kind: ir.Interface}, "method set of the interface"},
		{
			"an interface whose EmptyIface and method set disagree",
			&ir.Type{Kind: ir.Interface, EmptyIface: true, Methods: []ir.Method{{Name: "M"}}},
			"EmptyIface",
		},
		{
			"an interface method with no signature",
			&ir.Type{Kind: ir.Interface, Methods: []ir.Method{{Name: "M"}}},
			"has no signature",
		},
		{"nothing", nil, "nil type"},
		{
			"a defined type whose method set nobody computed",
			&ir.Type{Kind: ir.Int64, Name: "p.T", PkgPath: "p", Basic: "int"},
			"method set of p.T is not in the IR type",
		},
		{
			"a pointer to one",
			&ir.Type{Kind: ir.Ptr, Elem: &ir.Type{Kind: ir.Int64, Name: "p.T", PkgPath: "p", Basic: "int"}},
			"method set of p.T is not in the IR type",
		},
		{
			"a method with no signature",
			&ir.Type{
				Kind: ir.Int64, Name: "p.T", PkgPath: "p", Basic: "int",
				Methods: []ir.Method{{Name: "String"}},
			},
			"the method String of p.T has no signature",
		},
	} {
		err := emittable(tc.typ)
		if err == nil {
			t.Errorf("%s: was emittable", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the reason is %q, want it to name %q", tc.what, err, tc.want)
		}
	}
}

func TestTflagAndNameDataRefuseAnUnnamedType(t *testing.T) {
	if _, err := tflag(&ir.Type{Kind: ir.Chan}); err == nil {
		t.Error("a channel was given a tflag")
	}
	if _, err := nameData(&ir.Type{Kind: ir.Chan}); err == nil {
		t.Error("a channel was given name data")
	}
}

func TestBitSetIsBounded(t *testing.T) {
	// A pointer map is as long as it needs to be and no longer, so a reader
	// asking past the end asks about a word with no pointer rather than
	// reading another type's bytes.
	if bitSet(nil, 0) || bitSet([]byte{1}, 8) || bitSet([]byte{1}, -1) {
		t.Error("a bit past the end of a pointer map is set")
	}
	if !bitSet([]byte{1}, 0) {
		t.Error("bit zero is not set")
	}
}

func TestZeroAlignmentBecomesOne(t *testing.T) {
	// The runtime rounds an allocation by the alignment, so a zero would round
	// nothing. Layout gives every type a non-zero alignment, so this is the
	// answer for a type that reached here without one.
	ty := &ir.Type{Kind: ir.Slice, Elem: &ir.Type{Kind: ir.Uint8, Name: "uint8"}}
	if err := ir.Layout(ty); err != nil {
		t.Fatal(err)
	}
	ty.Align = 0
	syms, err := Descriptor(ty)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	if syms[0].Data[offAlign] != 1 || syms[0].Data[offFieldAlign] != 1 {
		t.Errorf("alignment %d, want 1", syms[0].Data[offAlign])
	}
}

func TestEqualClosureRefusesASymbolRtsymDoesNotHold(t *testing.T) {
	// Every runtime name a descriptor points at is checked against the
	// runtime's own source in rtsym, per specs/031. The guard is here so that
	// a name added to the tables above without a matching rtsym entry is a
	// refusal rather than a relocation to a symbol that does not exist.
	saved := algFuncs[algString]
	algFuncs[algString] = "runtime.notARealSymbol"
	defer func() { algFuncs[algString] = saved }()

	if _, _, err := equalClosure(&ir.Type{Kind: ir.String, Name: "string"}); err == nil {
		t.Error("a closure was built for a symbol rtsym does not hold")
	} else if !strings.Contains(err.Error(), "rtsym") {
		t.Errorf("the reason is %q", err)
	}
}

// TestPathToPrefixEscapes covers the case no standard library path reaches.
//
// It is cmd/internal/objabi.PathToPrefix, and the symbol names it builds are
// read by the object reader and by every tool that prints one, so a dot in the
// last element of a path has to be escaped or it is read as the separator
// between the path and the identifier after it.
func TestPathToPrefixEscapes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"internal/goarch", "internal/goarch"},
		{"", ""},
		// A dot before the last slash is part of a host name and is left
		// alone; one after it would be read as the separator.
		{"gopkg.in/yaml.v3", "gopkg.in/yaml%2ev3"},
		{"a.b", "a%2eb"},
		{`p/with"quote`, "p/with%22quote"},
		{"p/with space", "p/with%20space"},
		{"p/with%pct", "p/with%25pct"},
		{"p/é", "p/%c3%a9"},
	} {
		if got := pathToPrefix(tc.in); got != tc.want {
			t.Errorf("pathToPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if isExportedName("") {
		t.Error("the empty name is exported")
	}
}

// TestIsExportedNameIsTheLanguagesRule pins the test that decides an
// internal/abi.Name's first byte and the separator of the symbol that holds
// it.
//
// The language's rule is that the first character is an upper-case letter, and
// "letter" is Unicode's. This was a byte range, so a method or field named
// Ärger came out unexported: reflect would refuse to call it, and its name
// symbol carried a trailing dash where gc wrote a dot, so the two did not
// merge.
func TestIsExportedNameIsTheLanguagesRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"A", true},
		{"Ann", true},
		{"a", false},
		{"ann", false},
		{"_hidden", false},
		{"Ärger", true},  // U+00C4, an upper-case letter outside ASCII
		{"ärger", false}, // U+00E4, its lower-case form
		{"Ελλάς", true},  // U+0395, Greek capital epsilon
		{"日本", false},    // a letter with no case at all
		{"", false},
	} {
		if got := isExportedName(tc.name); got != tc.want {
			t.Errorf("isExportedName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStructPkgPathRefusesWhatItCannotAttribute covers the shape that cannot
// come from the checker.
//
// The language puts the fields of one struct literal in one file, so a struct
// whose unexported fields come from two packages means the IR was built by hand
// and wrongly. A descriptor written from one would send reflect to the wrong
// package for half the field names.
func TestStructPkgPathRefusesWhatItCannotAttribute(t *testing.T) {
	i64 := &ir.Type{Kind: ir.Int64, Name: "int64", Basic: "int64"}
	// An unexported field with no package is a field no Go source declared,
	// which is what a map slot group's are. gc gives those a nil package and
	// writes no path, and ir.TypeLinkString leaves them unqualified, so the
	// descriptor and the spelling agree.
	noPkg := structOf(t, ir.Field{Name: "hidden", Type: i64})
	if got, err := structPkgPath(noPkg); err != nil || got != "" {
		t.Errorf("a synthesised field gave %q (%v), want no path", got, err)
	}
	two := structOf(t,
		ir.Field{Name: "a", Type: i64, Pkg: "p"},
		ir.Field{Name: "b", Type: i64, Pkg: "q"})
	if _, err := structPkgPath(two); err == nil {
		t.Error("unexported fields from two packages were attributed")
	}
	// A blank field is skipped, as gc skips it, and it does not decide the
	// path even though its name is not exported.
	blank := structOf(t,
		ir.Field{Name: "_", Type: i64},
		ir.Field{Name: "n", Type: i64, Pkg: "p"})
	got, err := structPkgPath(blank)
	if err != nil || got != "p" {
		t.Errorf("a blank field gave %q (%v), want p", got, err)
	}
}

// TestUncommonPkgPathOfAComposite covers gc's rule for a type with no name.
//
// gc takes the package from the element's symbol for a pointer, a slice, an
// array and a channel. A composite with no name has no UncommonType unless it
// has methods, and it can only have methods through a defined element, so a
// pointer to a defined type is the row that reaches this through Descriptor.
func TestUncommonPkgPathOfAComposite(t *testing.T) {
	elem := &ir.Type{Kind: ir.Int64, Name: "p.T", PkgPath: "p", Basic: "int"}
	for _, tc := range []struct {
		what string
		typ  *ir.Type
		want string
	}{
		{"the type's own path", &ir.Type{Name: "p.T", PkgPath: "p"}, "p"},
		{"a predeclared type", &ir.Type{Kind: ir.Int64, Name: "int"}, ""},
		{"a pointer to a defined type", &ir.Type{Kind: ir.Ptr, Elem: elem}, "p"},
		{"a slice of one", &ir.Type{Kind: ir.Slice, Elem: elem}, "p"},
		{"an array of one", &ir.Type{Kind: ir.Array, Elem: elem}, "p"},
		{"a channel of one", &ir.Type{Kind: ir.Chan, Elem: elem}, "p"},
		{"a pointer to nothing", &ir.Type{Kind: ir.Ptr}, ""},
		{"a literal struct", &ir.Type{Kind: ir.Struct}, ""},
	} {
		if got := uncommonPkgPath(tc.typ); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.what, got, tc.want)
		}
	}
}

// TestMethodNamePkgIsTheDisambiguatingPathAndNothingElse covers which methods
// carry a package path in their name.
//
// Two packages may declare an unexported method of the same name and they are
// different methods, so the descriptor has to say which package. Nothing else
// does: reflect falls back to the path the UncommonType carries, so a name
// without the offset is attributed to the type's own package and the two
// become one.
//
// The other three rows are the ones that need no path, and each would cost a
// symbol and a relocation for nothing. An exported name is nameable from any
// package. A name from the type's own package is the fallback already. A
// method with no package recorded has nothing to write.
func TestMethodNamePkgIsTheDisambiguatingPathAndNothingElse(t *testing.T) {
	typ := &ir.Type{Kind: ir.Int64, Name: "p.T", PkgPath: "p", Basic: "int"}
	for _, tc := range []struct {
		what string
		m    ir.Method
		want string
	}{
		{"unexported, from another package", ir.Method{Name: "hidden", Pkg: "other"}, "other"},
		{"unexported, from the type's own package", ir.Method{Name: "hidden", Pkg: "p"}, ""},
		{"exported, from another package", ir.Method{Name: "Shown", Pkg: "other"}, ""},
		{"unexported, with no package recorded", ir.Method{Name: "hidden"}, ""},
	} {
		if got := methodNamePkg(typ, tc.m); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.what, got, tc.want)
		}
	}

	// The row that used to be refused is written now.
	sig := &ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{}}
	if err := methodEmittable(typ, ir.Method{Name: "hidden", Pkg: "other", Sig: sig}); err != nil {
		t.Errorf("a method whose name carries a package path was refused: %v", err)
	}
}

// TestNamePathSymbolWritesTheOffsetAfterTheNameAndTag covers the encoding.
//
// internal/abi.Name reads the four bytes after the name and the tag, so an
// offset written anywhere else is read as part of whatever follows. Bit 2 is
// what says the four bytes are there at all, and a Name with the bit and no
// relocation is a relocation against nothing, which is why the path's own
// symbol comes back with it.
func TestNamePathSymbolWritesTheOffsetAfterTheNameAndTag(t *testing.T) {
	sym, ip := namePathSymbol("hidden", "atag", "other/pkg", false, false)

	if sym.Data[0]&(1<<2) == 0 {
		t.Errorf("bit 2 is not set, so nothing reads the offset: flags %#02x", sym.Data[0])
	}
	// One flag byte, the name with its length, the tag with its length, then
	// the four bytes.
	want := 1 + 1 + len("hidden") + 1 + len("atag") + 4
	if len(sym.Data) != want {
		t.Fatalf("the encoding is %d bytes and the offset belongs at %d", len(sym.Data), want)
	}
	if len(sym.Relocs) != 1 {
		t.Fatalf("the name carries %d relocations, want the one that fills the offset in", len(sym.Relocs))
	}
	r := sym.Relocs[0]
	if r.Off != int32(want-4) || r.Size != 4 {
		t.Errorf("the relocation is %d bytes at %d, want 4 at %d", r.Size, r.Off, want-4)
	}
	if ip.Name == "" || r.Target != ip.Name {
		t.Errorf("the relocation targets %q and the path symbol is %q; a name with bit 2 and no such symbol is a relocation against nothing", r.Target, ip.Name)
	}

	// A name with no path is unchanged: no bit, no bytes, no relocation, and
	// the shared spelling every other package's copy has.
	plain, none := namePathSymbol("hidden", "atag", "", false, false)
	if plain.Data[0]&(1<<2) != 0 || len(plain.Relocs) != 0 || none.Name != "" {
		t.Errorf("a name with no package path gained an offset: %#02x, %d relocations", plain.Data[0], len(plain.Relocs))
	}
	if plain.Name == sym.Name {
		t.Errorf("both spellings are %q, so one symbol would hold two different encodings", plain.Name)
	}
}

// TestSliceOfRefusesAnElementThatDoesNotLayOut covers the error an array's
// header returns when it cannot build the slice type it names.
func TestSliceOfRefusesAnElementThatDoesNotLayOut(t *testing.T) {
	if _, err := SliceOf(&ir.Type{Kind: ir.Invalid}); err == nil {
		t.Error("a slice of an element that does not lay out was built")
	}
}

// TestImethodOrderIsGcs pins the order of an InterfaceType's method array.
//
// The runtime and reflect both binary-search this array, so an array in a
// different order from gc's is an array a lookup misses. gc's order is
// types.CompareSyms: every exported name ahead of every unexported one, then
// by name, then by package path. The IR sorts by name alone, which is enough
// for determinism and is a different rule, so the encoder sorts.
func TestImethodOrderIsGcs(t *testing.T) {
	sig := &ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{}}
	if err := ir.Layout(sig); err != nil {
		t.Fatal(err)
	}
	iface := &ir.Type{
		Kind: ir.Interface, Name: "p.I", PkgPath: "p",
		Methods: []ir.Method{
			{Name: "zeta", Pkg: "p", Sig: sig},
			{Name: "Zed", Sig: sig},
			{Name: "alpha", Pkg: "p", Sig: sig},
			{Name: "Ann", Sig: sig},
		},
	}
	if err := ir.Layout(iface); err != nil {
		t.Fatal(err)
	}
	got, err := imethods(iface)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Ann", "Zed", "alpha", "zeta"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("entry %d is %s, want %s", i, got[i].Name, w)
		}
	}
	// The input is not reordered. ir.Type is shared through the converter's
	// cache, so an encoder that sorted in place would change what every other
	// reader sees.
	if iface.Methods[0].Name != "zeta" {
		t.Error("imethods sorted the IR type's own slice")
	}
}

// TestImethodOrderSeparatesTwoPackages checks the third key.
//
// Two packages may declare an unexported method of one name and they are
// different methods, so the package path breaks the tie rather than the order
// the boundary happened to produce.
func TestImethodOrderSeparatesTwoPackages(t *testing.T) {
	sig := &ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{}}
	if err := ir.Layout(sig); err != nil {
		t.Fatal(err)
	}
	iface := &ir.Type{
		Kind: ir.Interface, Name: "p.I", PkgPath: "p",
		Methods: []ir.Method{
			{Name: "m", Pkg: "z/pkg", Sig: sig},
			{Name: "m", Pkg: "a/pkg", Sig: sig},
		},
	}
	if err := ir.Layout(iface); err != nil {
		t.Fatal(err)
	}
	// Both are refused for a different reason, so the order is checked on the
	// sort alone. Neither package is the interface's own, and gc encodes such
	// a name with a package-path offset the name encoder does not write.
	if err := ifaceEmittable(iface); err == nil {
		t.Fatal("a method from another package was accepted")
	}
	iface.PkgPath = "z/pkg"
	if err := ifaceEmittable(iface); err == nil {
		t.Fatal("a method from another package was accepted")
	}
}

// TestHashAndEqualCoverTheSameAlgorithms is the invariant a map depends on.
//
// Two values that compare equal must hash alike, or a map loses keys it holds.
// gc derives both functions from one AlgKind for that reason, and the two
// tables here have to answer for the same set of kinds: an algorithm with an
// equality function and no hash would give a comparable type a nil Hasher,
// which the runtime calls.
func TestHashAndEqualCoverTheSameAlgorithms(t *testing.T) {
	for a := range algFuncs {
		if _, ok := hashFuncs[a]; !ok {
			t.Errorf("algorithm %d has an equality function and no hash function", a)
		}
	}
	for a := range hashFuncs {
		if _, ok := algFuncs[a]; !ok {
			t.Errorf("algorithm %d has a hash function and no equality function", a)
		}
	}
	for w := range memEqualFuncs {
		if _, ok := memHashFuncs[w]; !ok {
			t.Errorf("width %d has a fixed-width memory comparison and no hash", w)
		}
	}
	for w := range memHashFuncs {
		if _, ok := memEqualFuncs[w]; !ok {
			t.Errorf("width %d has a fixed-width memory hash and no comparison", w)
		}
	}
	// Every name in both tables reaches the linker, so rtsym is what checks it
	// against the runtime's source rather than this file spelling it.
	for _, m := range []map[algKind]string{algFuncs, hashFuncs} {
		for _, fn := range m {
			if rtsym.Lookup(fn) == nil {
				t.Errorf("%s is not in rtsym", fn)
			}
		}
	}
	for _, m := range []map[int64]string{memEqualFuncs, memHashFuncs} {
		for _, fn := range m {
			if rtsym.Lookup(fn) == nil {
				t.Errorf("%s is not in rtsym", fn)
			}
		}
	}
	for _, fn := range []string{"runtime.memequal_varlen", "runtime.memhash_varlen"} {
		if rtsym.Lookup(fn) == nil {
			t.Errorf("%s is not in rtsym", fn)
		}
	}
}

// TestHashFuncFollowsTheType checks the choice per kind, against the same
// table equalFunc is checked with.
func TestHashFuncFollowsTheType(t *testing.T) {
	str := &ir.Type{Kind: ir.String}
	iface := &ir.Type{Kind: ir.Interface}
	eface := &ir.Type{Kind: ir.Interface, EmptyIface: true}
	slice := &ir.Type{Kind: ir.Slice, Elem: &ir.Type{Kind: ir.Int64}}
	for _, tc := range []struct {
		what   string
		typ    *ir.Type
		want   string
		varlen bool
	}{
		{"a bool", &ir.Type{Kind: ir.Bool}, "runtime.memhash8", false},
		{"an int64", &ir.Type{Kind: ir.Int64}, "runtime.memhash64", false},
		{"a float32", &ir.Type{Kind: ir.Float32}, "runtime.f32hash", false},
		{"a float64", &ir.Type{Kind: ir.Float64}, "runtime.f64hash", false},
		{"a complex64", &ir.Type{Kind: ir.Complex64}, "runtime.c64hash", false},
		{"a complex128", &ir.Type{Kind: ir.Complex128}, "runtime.c128hash", false},
		{"a string", str, "runtime.strhash", false},
		{"an interface with methods", iface, "runtime.interhash", false},
		{"an empty interface", eface, "runtime.nilinterhash", false},
		{"a slice", slice, "", false},
		{"a 24-byte array of bytes", &ir.Type{Kind: ir.Array, Len: 24, Elem: &ir.Type{Kind: ir.Uint8}}, "runtime.memhash_varlen", true},
	} {
		if err := ir.Layout(tc.typ); err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		got, varlen, err := hashFunc(tc.typ)
		if err != nil {
			t.Errorf("%s: %v", tc.what, err)
			continue
		}
		if got != tc.want || varlen != tc.varlen {
			t.Errorf("%s hashes with %q varlen %v, want %q varlen %v", tc.what, got, varlen, tc.want, tc.varlen)
		}
	}
}

// TestHashClosureIsAFuncValue checks the symbol the Hasher field points at.
//
// The field holds a func value and not a code address, so it points at a
// one-word closure. Pointing it at the function makes the runtime call
// whatever the first instruction encodes. gc names the variable-length form
// type:.hashfunc.M<size>, with the size in the closure's second word, which is
// how memhash_varlen learns how much to hash.
func TestHashClosureIsAFuncValue(t *testing.T) {
	str := &ir.Type{Kind: ir.String}
	if err := ir.Layout(str); err != nil {
		t.Fatal(err)
	}
	name, syms, err := hashClosure(str)
	if err != nil {
		t.Fatal(err)
	}
	if name != "runtime.strhash·f" {
		t.Errorf("the closure is %s, want runtime.strhash·f", name)
	}
	if len(syms) != 1 || len(syms[0].Data) != ir.PtrSize || !syms[0].Dupok {
		t.Fatalf("the closure symbol is %+v", syms)
	}
	if len(syms[0].Relocs) != 1 || syms[0].Relocs[0].Target != "runtime.strhash" {
		t.Errorf("the closure points at %+v, want runtime.strhash", syms[0].Relocs)
	}

	arr := &ir.Type{Kind: ir.Array, Len: 200, Elem: &ir.Type{Kind: ir.Uint8}}
	if err := ir.Layout(arr); err != nil {
		t.Fatal(err)
	}
	name, syms, err = hashClosure(arr)
	if err != nil {
		t.Fatal(err)
	}
	if name != "type:.hashfunc.M200" {
		t.Errorf("the closure is %s, want type:.hashfunc.M200", name)
	}
	if got := binary.LittleEndian.Uint64(syms[0].Data[ir.PtrSize:]); got != 200 {
		t.Errorf("the closure carries size %d, want 200", got)
	}
	if syms[0].Relocs[0].Target != "runtime.memhash_varlen" {
		t.Errorf("the closure points at %s, want runtime.memhash_varlen", syms[0].Relocs[0].Target)
	}

	// A type that is not comparable cannot be a map key and has no hash. An
	// empty name says so, the way it does for Equal.
	slice := &ir.Type{Kind: ir.Slice, Elem: &ir.Type{Kind: ir.Int64}}
	if err := ir.Layout(slice); err != nil {
		t.Fatal(err)
	}
	if name, _, err := hashClosure(slice); err != nil || name != "" {
		t.Errorf("a slice hashes with %q, %v", name, err)
	}
}

// The runtime's own view of a map type, for the map tests below.
//
// Mirrored here and not taken from the package, for the reason rtype_test.go's
// abiType is mirrored: the package writes the offsets down as constants
// because they are the target runtime's layout, and this file runs inside the
// target's own binary, so reading them through a Go struct is the independent
// answer.
type abiTypeHead struct {
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

type abiMapType struct {
	abiTypeHead
	Key        *abiTypeHead
	Elem       *abiTypeHead
	Group      *abiTypeHead
	Hasher     func(unsafe.Pointer, uintptr) uintptr
	GroupSize  uintptr
	KeysOff    uintptr
	KeyStride  uintptr
	ElemsOff   uintptr
	ElemStride uintptr
	ElemOff    uintptr
	Flags      uint32
}

type ifaceWords struct{ typ, data unsafe.Pointer }

func abiMapOf(rt reflect.Type) *abiMapType {
	return (*abiMapType)((*ifaceWords)(unsafe.Pointer(&rt)).data)
}

type bigKey struct {
	A [40]byte
	B string
}

// bigKeyPkgPath is the import path gc compiled this test package under.
func bigKeyPkgPath() string { return reflect.TypeOf(bigKey{}).PkgPath() }

// mapCorpus is one map type written twice: as the IR sees it, and as the
// compiler that built this test laid it out.
var mapCorpus = []struct {
	what string
	key  func(*testing.T) *ir.Type
	elem func(*testing.T) *ir.Type
	rt   reflect.Type
}{
	{"map[string]int", tString, tInt, reflect.TypeOf(map[string]int(nil))},
	{"map[int]*int", tInt, tPtrInt, reflect.TypeOf(map[int]*int(nil))},
	{"map[int8]int8", tInt8, tInt8, reflect.TypeOf(map[int8]int8(nil))},
	{"map[float64]struct{}", tFloat64, tEmptyStruct, reflect.TypeOf(map[float64]struct{}(nil))},
	{"map[any]any", tAny, tAny, reflect.TypeOf(map[any]any(nil))},
	{"map[[200]byte][300]byte", tBigArray, tBiggerArray, reflect.TypeOf(map[[200]byte][300]byte(nil))},
	{"map[bigKey]bigKey", tBigKey, tBigKey, reflect.TypeOf(map[bigKey]bigKey(nil))},
	{"map[[16]byte]bool", tArray16, tBool, reflect.TypeOf(map[[16]byte]bool(nil))},
}

func tString(t *testing.T) *ir.Type { return lay2(t, &ir.Type{Kind: ir.String, Name: "string"}) }
func tInt(t *testing.T) *ir.Type    { return lay2(t, &ir.Type{Kind: ir.Int64, Name: "int", Basic: "int"}) }
func tInt8(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Int8, Name: "int8", Basic: "int8"})
}
func tBool(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Bool, Name: "bool", Basic: "bool"})
}
func tFloat64(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Float64, Name: "float64", Basic: "float64"})
}
func tPtrInt(t *testing.T) *ir.Type { return lay2(t, &ir.Type{Kind: ir.Ptr, Elem: tInt(t)}) }
func tAny(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Interface, EmptyIface: true, Methods: []ir.Method{}})
}
func tEmptyStruct(t *testing.T) *ir.Type { return lay2(t, &ir.Type{Kind: ir.Struct}) }

func tBigArray(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Array, Len: 200, Elem: lay2(t, &ir.Type{Kind: ir.Uint8, Name: "uint8", Basic: "uint8"})})
}

func tBiggerArray(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Array, Len: 300, Elem: lay2(t, &ir.Type{Kind: ir.Uint8, Name: "uint8", Basic: "uint8"})})
}

func tArray16(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Array, Len: 16, Elem: lay2(t, &ir.Type{Kind: ir.Uint8, Name: "uint8", Basic: "uint8"})})
}

// tBigKey is the defined type the oracle was compiled under, named.
//
// The name is not decoration. A slot group's spelling holds the map's key, gc
// spells a defined key by its name, and a literal struct of the same two fields
// is a different spelling and therefore a different hash. Before the literal
// struct had a spelling at all, this row was skipped for want of one and the
// name was not needed.
func tBigKey(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Struct,
		Name:    bigKeyPkgPath() + ".bigKey",
		PkgPath: bigKeyPkgPath(),
		Fields: []ir.Field{
			{Name: "A", Type: tBigArray40(t)},
			{Name: "B", Type: tString(t)},
		}})
}

func tBigArray40(t *testing.T) *ir.Type {
	return lay2(t, &ir.Type{Kind: ir.Array, Len: 40, Elem: lay2(t, &ir.Type{Kind: ir.Uint8, Name: "uint8", Basic: "uint8"})})
}

func lay2(t *testing.T, ty *ir.Type) *ir.Type {
	t.Helper()
	if err := ir.Layout(ty); err != nil {
		t.Fatal(err)
	}
	return ty
}

func mapOf(t *testing.T, key, elem *ir.Type) *ir.Type {
	t.Helper()
	return lay2(t, &ir.Type{Kind: ir.Map, Key: key, Elem: elem})
}

// TestMapGroupAgainstReflect is the check that answers the refusal this
// package carried for a map.
//
// The slot group is a struct the compiler synthesises. It is written nowhere
// in the runtime's source, the collector scans it, and getting its layout
// wrong is a map that reads a key where an element is. So it is checked
// against the group gc synthesised for the same map type, read out of the real
// descriptor, field by field.
func TestMapGroupAgainstReflect(t *testing.T) {
	for _, c := range mapCorpus {
		t.Run(c.what, func(t *testing.T) {
			group, err := GroupOf(mapOf(t, c.key(t), c.elem(t)))
			if err != nil {
				t.Fatal(err)
			}
			want := abiMapOf(c.rt)
			if uint64(group.Size) != uint64(want.Group.Size_) {
				t.Errorf("group size %d, want %d", group.Size, want.Group.Size_)
			}
			if uint64(group.PtrBytes()) != uint64(want.Group.PtrBytes) {
				t.Errorf("group PtrBytes %d, want %d", group.PtrBytes(), want.Group.PtrBytes)
			}
			if uint8(group.Align) != want.Group.Align_ {
				t.Errorf("group alignment %d, want %d", group.Align, want.Group.Align_)
			}
		})
	}
}

// TestMapPlanAgainstReflect checks every computed field of the MapType header.
//
// The runtime finds a key with KeysOff + i*KeyStride and an element with
// ElemsOff + i*ElemStride, so an offset computed apart from the stride is a
// read at the wrong address rather than a wrong answer anything reports.
func TestMapPlanAgainstReflect(t *testing.T) {
	for _, c := range mapCorpus {
		t.Run(c.what, func(t *testing.T) {
			p, err := mapPlanOf(mapOf(t, c.key(t), c.elem(t)))
			if err != nil {
				t.Fatal(err)
			}
			w := abiMapOf(c.rt)
			for _, f := range []struct {
				name string
				got  int64
				want uintptr
			}{
				{"GroupSize", p.groupSize, w.GroupSize},
				{"KeysOff", p.keysOff, w.KeysOff},
				{"KeyStride", p.keyStride, w.KeyStride},
				{"ElemsOff", p.elemsOff, w.ElemsOff},
				{"ElemStride", p.elemStride, w.ElemStride},
				{"ElemOff", p.elemOff, w.ElemOff},
			} {
				if uint64(f.got) != uint64(f.want) {
					t.Errorf("%s %d, want %d", f.name, f.got, f.want)
				}
			}
			if p.flags != w.Flags {
				t.Errorf("Flags %#b, want %#b", p.flags, w.Flags)
			}
			// GroupSize is documented as Group.Size_, and the two come from
			// one computation here, so this asserts the documentation rather
			// than a second answer.
			if p.groupSize != p.group.Size {
				t.Errorf("GroupSize %d and the group is %d bytes", p.groupSize, p.group.Size)
			}
		})
	}
}

// TestMapTypeLayoutMatchesInternalAbi checks the offsets the header is written
// at.
//
// They are constants in map.go because they are the target runtime's layout
// rather than the layout of the compiler that built this test. Here the mirror
// is the point: this binary holds the runtime the descriptor is written for.
func TestMapTypeLayoutMatchesInternalAbi(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want uintptr
	}{
		{"Key", TypeSize + mapOffKey, unsafe.Offsetof(abiMapType{}.Key)},
		{"Elem", TypeSize + mapOffElem, unsafe.Offsetof(abiMapType{}.Elem)},
		{"Group", TypeSize + mapOffGroup, unsafe.Offsetof(abiMapType{}.Group)},
		{"Hasher", TypeSize + mapOffHasher, unsafe.Offsetof(abiMapType{}.Hasher)},
		{"GroupSize", TypeSize + mapOffGroupSize, unsafe.Offsetof(abiMapType{}.GroupSize)},
		{"KeysOff", TypeSize + mapOffKeysOff, unsafe.Offsetof(abiMapType{}.KeysOff)},
		{"KeyStride", TypeSize + mapOffKeyStride, unsafe.Offsetof(abiMapType{}.KeyStride)},
		{"ElemsOff", TypeSize + mapOffElemsOff, unsafe.Offsetof(abiMapType{}.ElemsOff)},
		{"ElemStride", TypeSize + mapOffElemStride, unsafe.Offsetof(abiMapType{}.ElemStride)},
		{"ElemOff", TypeSize + mapOffElemOff, unsafe.Offsetof(abiMapType{}.ElemOff)},
		{"Flags", TypeSize + mapOffFlags, unsafe.Offsetof(abiMapType{}.Flags)},
	} {
		if uintptr(tc.got) != tc.want {
			t.Errorf("%s is written at %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	if got := TypeSize + mapTailSize; uintptr(got) != unsafe.Sizeof(abiMapType{}) {
		t.Errorf("a MapType is %d bytes and internal/abi's is %d", got, unsafe.Sizeof(abiMapType{}))
	}
}

// TestMapHeaderBytes checks that the header the encoder writes holds the plan.
func TestMapHeaderBytes(t *testing.T) {
	m := mapOf(t, tString(t), tInt(t))
	data, relocs, _, err := mapTail(m)
	if err != nil {
		t.Fatalf("mapTail: %v", err)
	}
	p, err := mapPlanOf(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		off  int
		want int64
	}{
		{"GroupSize", mapOffGroupSize, p.groupSize},
		{"KeysOff", mapOffKeysOff, p.keysOff},
		{"KeyStride", mapOffKeyStride, p.keyStride},
		{"ElemsOff", mapOffElemsOff, p.elemsOff},
		{"ElemStride", mapOffElemStride, p.elemStride},
		{"ElemOff", mapOffElemOff, p.elemOff},
	} {
		if got := int64(binary.LittleEndian.Uint64(data[tc.off:])); got != tc.want {
			t.Errorf("%s is %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := binary.LittleEndian.Uint32(data[mapOffFlags:]); got != p.flags {
		t.Errorf("Flags is %#b, want %#b", got, p.flags)
	}
	// The three pointers the header carries, plus the hasher.
	want := map[string]bool{
		"type:string": false, "type:int": false,
		"type:noalg.map.group[string]int": false,
	}
	for _, r := range relocs {
		if _, ok := want[r.Target]; ok {
			want[r.Target] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("the header does not point at %s", name)
		}
	}

	// The three flags a string key sets and does not set.
	if p.flags&mapNeedKeyUpdate == 0 {
		t.Error("a string key does not ask for a key update; an equal string may point at a larger backing array")
	}
	if p.flags&mapHashMightPanic != 0 {
		t.Error("a string key is marked as able to panic while hashing")
	}
	if p.flags&(mapIndirectKey|mapIndirectElem) != 0 {
		t.Error("a string key or an int element is stored indirectly")
	}
}

// TestMapGroupIsNamedAsGcNamesIt checks the two spellings of the slot group.
//
// The group is a struct no Go source declares, so gc gives it a spelling of its
// own: map.group[K]V, read from the *map's* key and element and not from the
// slot's. A key past internal/abi.MapMaxKeyBytes is a pointer in the slot and
// is itself in the name, which is why ir.Type.MapGroup carries the map.
//
// The symbol takes a "noalg." prefix and the link string does not. The type
// hash is computed over the link string, so the hash gc put in the group
// descriptor of the same map is the oracle for the link string this package
// builds: a spelling that differs from gc's by one character fails here.
func TestMapGroupIsNamedAsGcNamesIt(t *testing.T) {
	for _, c := range mapCorpus {
		t.Run(c.what, func(t *testing.T) {
			m := mapOf(t, c.key(t), c.elem(t))
			p, err := mapPlanOf(m)
			if err != nil {
				t.Fatal(err)
			}
			// A row whose key or element is a type literal this package
			// cannot name is skipped. The group's own name holds the map's key
			// and element, so such a row fails for a reason that is not the
			// group's spelling, and the corpus builds those two without the
			// defined name gc compiled them under anyway.
			if _, err := ir.TypeSymbol(m.Key); err != nil {
				t.Skipf("the key has no canonical name: %v", err)
			}
			if _, err := ir.TypeSymbol(m.Elem); err != nil {
				t.Skipf("the element has no canonical name: %v", err)
			}
			link, err := ir.TypeLinkString(p.group)
			if err != nil {
				t.Fatalf("the slot group has no link string: %v", err)
			}
			sym, err := ir.TypeSymbol(p.group)
			if err != nil {
				t.Fatalf("the slot group has no symbol: %v", err)
			}
			if want := "type:noalg." + link; sym != want {
				t.Errorf("the symbol is %s, want %s", sym, want)
			}
			name, err := ir.TypeNameString(p.group)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(name, "map.group[") {
				t.Errorf("the name string is %q, want it to begin with map.group[", name)
			}
			got, err := Hash(p.group)
			if err != nil {
				t.Fatal(err)
			}
			if want := abiMapOf(c.rt).Group.Hash; got != want {
				t.Errorf("the hash of %q is %#08x and gc computed %#08x, so the two link strings differ",
					link, got, want)
			}
		})
	}
}

// TestMapGroupIsDescribed is what the last map refusal turned into.
//
// The group's slots are a literal struct, and a literal struct had no spelling
// until ir/rtype.go grew one. That was the only thing between a map type and a
// descriptor: everything the header holds was already computed and the group
// was already named as gc names it. So this asserts the whole chain rather than
// the one link, because the chain is what a map's descriptor needs.
func TestMapGroupIsDescribed(t *testing.T) {
	m := mapOf(t, tString(t), tInt(t))
	p, err := mapPlanOf(m)
	if err != nil {
		t.Fatal(err)
	}
	// The map, its key, its element and the group, and now the group's own
	// fields: the array of slots and the slot.
	slots := p.group.Fields[1].Type
	for _, ty := range []*ir.Type{m, m.Key, m.Elem, p.group, slots, slots.Elem} {
		if _, err := ir.TypeSymbol(ty); err != nil {
			t.Errorf("%s has no name: %v", ty, err)
		}
		if _, err := Descriptor(ty); err != nil {
			t.Errorf("%s has no descriptor: %v", ty, err)
		}
	}
	if err := mapEmittable(m); err != nil {
		t.Fatalf("a map is not emittable: %v", err)
	}
	// The two spellings gc gives the slot, which is the pair the refusal used
	// to stand in for. The field names are the compiler's and belong to no
	// package, so neither spelling qualifies them.
	link, err := ir.TypeLinkString(slots.Elem)
	if err != nil {
		t.Fatal(err)
	}
	if want := "struct { key string; elem int }"; link != want {
		t.Errorf("the slot's link string is %q, want %q", link, want)
	}
	sym, err := ir.TypeSymbol(slots)
	if err != nil {
		t.Fatal(err)
	}
	if want := "type:noalg.[8]struct { key string; elem int }"; sym != want {
		t.Errorf("the slot array's symbol is %q, want %q", sym, want)
	}
}

// TestMapKeyMustBeHashable is the other refusal a map carries.
//
// A nil Hasher is not the option a nil Equal is. The runtime calls the Hasher
// on every operation, so a key with no hash function is a map this compiler
// cannot describe rather than one that panics only when used.
func TestMapKeyMustBeHashable(t *testing.T) {
	slice := lay2(t, &ir.Type{Kind: ir.Slice, Elem: tInt(t)})
	err := mapEmittable(mapOf(t, slice, tInt(t)))
	if err == nil {
		t.Fatal("a map with a slice key was emittable")
	}
	if !strings.Contains(err.Error(), "hash function") {
		t.Errorf("the refusal is %q, want it to name the hash function", err)
	}
	// A key that needs a generated hash is a key this compiler can describe.
	// The check asks hashClosure, which is what the writer calls, so the two
	// answer alike: a key with no hash at all is refused above and a key whose
	// hash the compiler generates passes here and reaches the generator.
	pair := lay2(t, &ir.Type{Kind: ir.Struct, Fields: []ir.Field{
		{Name: "A", Type: tString(t)},
		{Name: "B", Type: tString(t)},
	}})
	if err := mapEmittable(mapOf(t, pair, tInt(t))); err != nil {
		t.Fatalf("a map whose key needs a generated hash is not emittable: %v", err)
	}
	name, _, err := hashClosure(pair)
	if err != nil {
		t.Fatalf("hashClosure: %v", err)
	}
	if want := "type:.hashfunc.struct { A string; B string }"; name != want {
		t.Errorf("the Hasher points at %q, want %q", name, want)
	}
}

// TestReferencedFollowsAMap checks the edges cmd/link walks.
//
// The slot group is the one nobody else emits. It is synthesised here, so a
// map's descriptor owes it as well as the key's and the element's.
func TestReferencedFollowsAMap(t *testing.T) {
	m := mapOf(t, tString(t), tInt(t))
	got, err := Referenced(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("a map reaches %d types, want the key, the element and the group", len(got))
	}
	if got[0] != m.Key || got[1] != m.Elem {
		t.Error("the first two are not the key and the element")
	}
	if got[2].Kind != ir.Struct || len(got[2].Fields) != 2 {
		t.Errorf("the third is %s, want the slot group struct", got[2])
	}
}
