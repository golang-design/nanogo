// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
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
			"a defined type that has a method",
			&ir.Type{
				Kind: ir.Int64, Name: "p.T", PkgPath: "p", Basic: "int",
				Methods: []ir.Method{{Name: "String"}},
			},
			"the first is String",
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

// TestStructPkgPathRefusesWhatItCannotAttribute covers the two shapes that
// cannot come from the checker.
//
// ir.Converter sets Field.Pkg on every unexported field, and the language puts
// the fields of one struct literal in one file, so both of these mean the IR
// was built by hand and wrongly. A descriptor written from one would send
// reflect to the wrong package for a field name, or to none at all.
func TestStructPkgPathRefusesWhatItCannotAttribute(t *testing.T) {
	i64 := &ir.Type{Kind: ir.Int64, Name: "int64", Basic: "int64"}
	noPkg := structOf(t, ir.Field{Name: "hidden", Type: i64})
	if _, err := structPkgPath(noPkg); err == nil {
		t.Error("an unexported field with no package was attributed")
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
// array and a channel. It is unreachable through Descriptor while a type with
// a method is refused, because a composite with no name has no UncommonType
// unless it has methods, and it can only have methods through a defined
// element. It is the answer for the day methods are written.
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

// TestMethodRefusalQualifiesAnUnexportedName checks that the refusal names one
// method the way the language names it.
//
// Two packages may declare an unexported method of the same name and they are
// different methods, so the refusal has to say which.
func TestMethodRefusalQualifiesAnUnexportedName(t *testing.T) {
	typ := &ir.Type{Kind: ir.Int64, Name: "p.T", PkgPath: "p", Basic: "int"}
	err := methodRefusal(typ, []ir.Method{{Name: "hidden", Pkg: "p"}, {Name: "Visible"}})
	if err == nil {
		t.Fatal("no refusal")
	}
	if !strings.Contains(err.Error(), "p.hidden") {
		t.Errorf("the refusal is %q, want it to qualify the unexported name", err)
	}
	if !strings.Contains(err.Error(), "2 method") {
		t.Errorf("the refusal is %q, want it to count the methods", err)
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
