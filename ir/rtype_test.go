// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"strings"
	"testing"
)

// The canonical names, checked against the names gc gives the same types.
//
// The spellings below were read out of a gc-compiled object with go tool nm
// rather than recalled, because specs/032-type-descriptors-and-itabs.md makes
// the linker's deduplication a function of the name: a name that differs from
// gc's by one character is a second descriptor for a type that already has
// one, and the failure is silent.
//
// rtype's own test closes the loop from the other end. It hashes the link
// string and compares the result with the hash gc put in the descriptor
// reflect is reading, so a link string that is wrong fails there as a hash
// mismatch even though nothing here compares strings with a running program.

// nameCorpus is the type as the checker reads it and the two spellings gc
// gives it.
//
// link is the symbol name after the "type:" prefix, which qualifies a defined
// type by its import path. name is what reflect.Type.String returns, which
// qualifies by package name instead.
var nameCorpus = []struct {
	src, link, name string
}{
	{"bool", "bool", "bool"},
	{"int", "int", "int"},
	{"int64", "int64", "int64"},
	{"uint8", "uint8", "uint8"},
	{"byte", "uint8", "uint8"},
	{"rune", "int32", "int32"},
	{"uintptr", "uintptr", "uintptr"},
	{"float64", "float64", "float64"},
	{"string", "string", "string"},
	{"unsafe.Pointer", "unsafe.Pointer", "unsafe.Pointer"},
	{"any", "interface {}", "interface {}"},
	{"[]int", "[]int", "[]int"},
	{"[]byte", "[]uint8", "[]uint8"},
	{"[]any", "[]interface {}", "[]interface {}"},
	{"[][]string", "[][]string", "[][]string"},
	{"*int", "*int", "*int"},
	{"**int", "**int", "**int"},
	{"[3]int", "[3]int", "[3]int"},
	{"[0]byte", "[0]uint8", "[0]uint8"},
	{"[2][3]*int", "[2][3]*int", "[2][3]*int"},
	{"map[string]int", "map[string]int", "map[string]int"},
	{"map[int][]any", "map[int][]interface {}", "map[int][]interface {}"},
	// A defined type is qualified by its import path in the link string and by
	// its package name in the name string. The checker is given "p" as the
	// path here, so the two coincide; the shortening is checked separately
	// below, where the path has a slash in it.
	{"T", "p.T", "p.T"},
	{"*T", "*p.T", "*p.T"},
	{"[]T", "[]p.T", "[]p.T"},
	{"map[T]*T", "map[p.T]*p.T", "map[p.T]*p.T"},
	{"error", "error", "error"},
	{"[]error", "[]error", "[]error"},
	// The three directions are three types and three symbols. The last row is
	// the one that needs parentheses: chan <-chan int reads back as
	// chan<- chan int, so gc writes chan (<-chan int) and so does this.
	{"chan int", "chan int", "chan int"},
	{"chan<- int", "chan<- int", "chan<- int"},
	{"<-chan int", "<-chan int", "<-chan int"},
	{"chan (<-chan int)", "chan (<-chan int)", "chan (<-chan int)"},
	{"chan chan<- int", "chan chan<- int", "chan chan<- int"},
	{"chan chan int", "chan chan int", "chan chan int"},
	// A receive-only channel of one needs no parentheses, because <-chan is
	// already the whole prefix and nothing reparses.
	{"<-chan (<-chan int)", "<-chan <-chan int", "<-chan <-chan int"},
	{"[]chan T", "[]chan p.T", "[]chan p.T"},
	// A signature. One result is bare and several are parenthesised, a
	// variadic parameter is spelled from the slice's element, and no parameter
	// name is in it: two functions differing only in a parameter's name are
	// one type.
	{"func()", "func()", "func()"},
	{"func() int", "func() int", "func() int"},
	{"func(int)", "func(int)", "func(int)"},
	{"func(a int, b string) error", "func(int, string) error", "func(int, string) error"},
	{"func(int, string) (bool, error)", "func(int, string) (bool, error)", "func(int, string) (bool, error)"},
	{"func(int, ...string) (bool, error)", "func(int, ...string) (bool, error)", "func(int, ...string) (bool, error)"},
	{"func(...[]byte) func(int) int", "func(...[]uint8) func(int) int", "func(...[]uint8) func(int) int"},
	{"func(chan<- T) *T", "func(chan<- p.T) *p.T", "func(chan<- p.T) *p.T"},
	{"[]func()", "[]func()", "[]func()"},
	// A literal interface. The methods are exported here, so nothing is
	// qualified; the qualifier is checked in rtype, against the package path
	// gc used.
	{"interface{ F() int }", "interface { F() int }", "interface { F() int }"},
	{"interface{ Close() error; Read([]byte) (int, error) }",
		"interface { Close() error; Read([]uint8) (int, error) }",
		"interface { Close() error; Read([]uint8) (int, error) }"},
	{"map[string]func(T)", "map[string]func(p.T)", "map[string]func(p.T)"},
	// A literal struct. The spelling holds the name, the type and the tag of
	// every field, and the two spellings differ here more than anywhere else:
	// the link string qualifies an unexported field name by the package that
	// declared it and the name string leaves it bare.
	{"struct{}", "struct {}", "struct {}"},
	{"struct{ A int }", "struct { A int }", "struct { A int }"},
	{"struct{ A int; B string }", "struct { A int; B string }", "struct { A int; B string }"},
	{"struct{ a int }", "struct { p.a int }", "struct { a int }"},
	{"struct{ a int; B string }", "struct { p.a int; B string }", "struct { a int; B string }"},
	// A blank field is not exported either, so it is qualified with the rest.
	{"struct{ _ int }", "struct { p._ int }", "struct { _ int }"},
	{"struct{ A int; _ [4]byte }", "struct { A int; p._ [4]uint8 }", "struct { A int; _ [4]uint8 }"},
	{"struct{ P unsafe.Pointer }", "struct { P unsafe.Pointer }", "struct { P unsafe.Pointer }"},
	{"struct{ A struct{ B int } }", "struct { A struct { B int } }", "struct { A struct { B int } }"},
	{"struct{ A []struct{ B int } }", "struct { A []struct { B int } }", "struct { A []struct { B int } }"},
	{"map[struct{ A int }]int", "map[struct { A int }]int", "map[struct { A int }]int"},
	// A tag is part of the type, and gc quotes it the way Go source does.
	{"struct{ A int `json:\"a\"` }", "struct { A int \"json:\\\"a\\\"\" }", "struct { A int \"json:\\\"a\\\"\" }"},
	{"struct{ a int `json:\"a,omitempty\"`; B string `xml:\"b\"` }", "struct { p.a int \"json:\\\"a,omitempty\\\"\"; B string \"xml:\\\"b\\\"\" }", "struct { a int \"json:\\\"a,omitempty\\\"\"; B string \"xml:\\\"b\\\"\" }"},
	// An embedded field is spelled by its type alone, and the name string
	// drops the name whatever it is.
	{"struct{ T }", "struct { p.T }", "struct { p.T }"},
	{"struct{ *T }", "struct { *p.T }", "struct { *p.T }"},
	{"struct{ u }", "struct { p.u }", "struct { p.u }"},
	// Embedded through an alias. The field's name is not the embedded type's
	// name, and gc writes the difference: struct{ Int } with type Int = int is
	// a different type from struct{ int } and from struct{ Int int }.
	{"struct{ Int }", "struct { Int = int }", "struct { int }"},
	{"struct{ int }", "struct { p.int = int }", "struct { int }"},
	{"struct{ error }", "struct { p.error = error }", "struct { error }"},
	{"struct{ byte }", "struct { p.byte = uint8 }", "struct { uint8 }"},
}

// TestChanDirectionsAreThreeNames is the naming half of rtype's
// TestChanDirectionsDoNotShareASymbol.
//
// chan int, chan<- int and <-chan int are three types. The linker deduplicates
// by symbol name, so a spelling that dropped the direction would merge three
// descriptors into one and the program would read one channel type's
// descriptor for another's values.
func TestChanDirectionsAreThreeNames(t *testing.T) {
	elem := mustLayoutNamed(Int64, "int")
	seen := make(map[string]ChanDir)
	for _, dir := range []ChanDir{SendRecv, SendOnly, RecvOnly} {
		ty := &Type{Kind: Chan, Elem: elem, ChanDir: dir}
		if err := Layout(ty); err != nil {
			t.Fatal(err)
		}
		sym, err := TypeSymbol(ty)
		if err != nil {
			t.Fatalf("%s int: %v", dir, err)
		}
		if other, ok := seen[sym]; ok {
			t.Errorf("%s int and %s int are both %s", dir, other, sym)
		}
		seen[sym] = dir
	}
}

// TestInterfaceMethodOrderIsGcsAndNotByteOrder is the case a byte-range
// exported test answers wrongly.
//
// gc puts every exported method ahead of every unexported one, then orders by
// name in byte order. Ärger is exported, because the language's rule is an
// upper-case letter and the letter need not be ASCII, and its first byte is
// 0xC3, which is above every lower-case ASCII letter. So plain byte order puts
// it after the unexported names and gc puts it before them. gc's own spelling
// of the same interface is
//
//	interface { Read() int; Ärger() int; example.com/outer.flush() int }
//
// read out of a gc-compiled object with go tool nm.
func TestInterfaceMethodOrderIsGcsAndNotByteOrder(t *testing.T) {
	sig := func() *Type { return layOut(t, &Type{Kind: FuncKind, Params: []*Type{}, Results: []*Type{}}) }
	// The order Converter produces, which is by name alone.
	in := []Method{
		{Name: "Ärger", Sig: sig()},
		{Name: "flush", Pkg: "p", Sig: sig()},
		{Name: "Read", Sig: sig()},
	}
	got := InterfaceMethodOrder(in)
	want := []string{"Read", "Ärger", "flush"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("the order is %v, want %v", []Method{got[0], got[1], got[2]}, want)
		}
	}
	// The input is left alone, so a caller that holds the IR's own order keeps
	// it.
	if in[0].Name != "Ärger" || in[1].Name != "flush" || in[2].Name != "Read" {
		t.Errorf("the input was reordered: %s, %s, %s", in[0].Name, in[1].Name, in[2].Name)
	}
	// The spelling reads the same list, so it is ordered the same way.
	ty := layOut(t, &Type{Kind: Interface, Methods: in})
	link, err := TypeLinkString(ty)
	if err != nil {
		t.Fatal(err)
	}
	if link != "interface { Read(); Ärger(); p.flush() }" {
		t.Errorf("the link string is %q", link)
	}
	name, err := TypeNameString(ty)
	if err != nil {
		t.Fatal(err)
	}
	if name != "interface { Read(); Ärger(); p.flush() }" {
		t.Errorf("the name string is %q", name)
	}
}

// TestMapGroupIsSpelledFromTheMap checks the one struct with a spelling.
//
// A slot group is a struct no Go source declares, so the literal-struct refusal
// would refuse it and a map's descriptor points at it. gc spells it
// map.group[K]V, from the *map's* key and element and not from the slot's, and
// puts the descriptor under a symbol prefixed "noalg." so that it cannot merge
// with the descriptor of a struct a program declared with the same two fields.
//
// rtype checks the same spelling against the hash gc computed for the group of
// the same map. This is the half that has no running program in it: the
// prefix belongs to the symbol and not to the link string, because the hash is
// computed over the link string.
func TestMapGroupIsSpelledFromTheMap(t *testing.T) {
	str := mustLayoutNamed(String, "string")
	num := mustLayoutNamed(Int64, "int")
	m := layOut(t, &Type{Kind: Map, Key: str, Elem: num})
	// The slot substitutes a pointer for a large key and the name does not, so
	// the group carries the map rather than the slot.
	slot := layOut(t, &Type{Kind: Struct, NoAlg: true, Fields: []Field{
		{Name: "key", Type: layOut(t, &Type{Kind: Ptr, Elem: str})},
		{Name: "elem", Type: num},
	}})
	group := layOut(t, &Type{Kind: Struct, NoAlg: true, MapGroup: m, Fields: []Field{
		{Name: "ctrl", Type: mustLayoutNamed(Uint64, "uint64")},
		{Name: "slots", Type: layOut(t, &Type{Kind: Array, NoAlg: true, Len: 8, Elem: slot})},
	}})

	link, err := TypeLinkString(group)
	if err != nil {
		t.Fatal(err)
	}
	if link != "map.group[string]int" {
		t.Errorf("the link string is %q, want %q", link, "map.group[string]int")
	}
	name, err := TypeNameString(group)
	if err != nil {
		t.Fatal(err)
	}
	if name != "map.group[string]int" {
		t.Errorf("the name string is %q, want %q", name, "map.group[string]int")
	}
	sym, err := TypeSymbol(group)
	if err != nil {
		t.Fatal(err)
	}
	if sym != "type:noalg.map.group[string]int" {
		t.Errorf("the symbol is %q, want %q", sym, "type:noalg.map.group[string]int")
	}

	// The mark reaches a type built out of a marked one, and stops where gc
	// stops it. A slice is not on the list, because gc gives a slice no
	// equality algorithm to begin with and so never raises it.
	for _, tc := range []struct {
		what string
		typ  *Type
		want bool
	}{
		{"the group", group, true},
		{"a pointer to the group", layOut(t, &Type{Kind: Ptr, Elem: group}), true},
		{"an array of slots", group.Fields[1].Type, true},
		{"the slot", slot, true},
		{"the control word", group.Fields[0].Type, false},
		// Through a field, which is the branch the literal-struct work will
		// reach: gc raises a struct to the mark from its contents.
		{"an unmarked struct holding a marked field", layOut(t, &Type{Kind: Struct, Fields: []Field{
			{Name: "g", Type: group},
		}}), true},
		{"an unmarked array of marked slots", layOut(t, &Type{Kind: Array, Len: 2, Elem: slot}), true},
		{"the map itself", m, false},
		{"a slice of the group", layOut(t, &Type{Kind: Slice, Elem: group}), false},
	} {
		if got := noAlg(tc.typ, 0); got != tc.want {
			t.Errorf("%s: noAlg is %v, want %v", tc.what, got, tc.want)
		}
	}

	// A struct of the same two fields and no MapGroup is spelled as the
	// literal struct it is, so the group's name is the group's and not every
	// struct's. The two must differ, or the linker would merge the group of
	// one map with a struct a program declared.
	plain := layOut(t, &Type{Kind: Struct, Fields: []Field{
		{Name: "ctrl", Type: mustLayoutNamed(Uint64, "uint64")},
		{Name: "slots", Type: group.Fields[1].Type},
	}})
	plainLink, err := TypeLinkString(plain)
	if err != nil {
		t.Fatal(err)
	}
	if plainLink == link {
		t.Errorf("the group and a struct of its fields are both %q", link)
	}
	if plainLink != "struct { ctrl uint64; slots [8]struct { key *string; elem int } }" {
		t.Errorf("the struct of the group's fields is %q", plainLink)
	}
	// A group whose MapGroup is not a map is refused rather than spelled from
	// whatever the field holds.
	bad := layOut(t, &Type{Kind: Struct, MapGroup: num, Fields: []Field{{Name: "A", Type: num}}})
	if _, err := TypeLinkString(bad); err == nil {
		t.Error("a slot group of a non-map was named")
	} else if !strings.Contains(err.Error(), "names the map it belongs to") {
		t.Errorf("the reason is %q", err)
	}
}

// nameCorpusTypes type-checks the corpus and returns one IR type per row.
func nameCorpusTypes(t *testing.T) []*Type {
	t.Helper()
	var b strings.Builder
	b.WriteString("package p\n\nimport \"unsafe\"\n\nvar _ unsafe.Pointer\n\ntype T struct{ A int }\n\ntype Int = int\n\ntype u int\n")
	for i, c := range nameCorpus {
		fmt.Fprintf(&b, "var v%d %s\n", i, c.src)
	}
	pkg, _, _ := buildTypecheck(t, b.String())
	c := NewConverter()
	out := make([]*Type, len(nameCorpus))
	for i := range nameCorpus {
		o := pkg.Scope().Lookup(fmt.Sprintf("v%d", i))
		if o == nil {
			t.Fatalf("v%d is not declared", i)
		}
		got, err := c.Convert(o.Type())
		if err != nil {
			t.Fatalf("%s: convert: %v", nameCorpus[i].src, err)
		}
		out[i] = got
	}
	return out
}

func TestTypeLinkString(t *testing.T) {
	for i, ty := range nameCorpusTypes(t) {
		c := nameCorpus[i]
		got, err := TypeLinkString(ty)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got != c.link {
			t.Errorf("%s: link string %q, want %q", c.src, got, c.link)
		}
		sym, err := TypeSymbol(ty)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if sym != TypeSymbolPrefix+c.link {
			t.Errorf("%s: symbol %q, want %q", c.src, sym, TypeSymbolPrefix+c.link)
		}
		// Every prefix in specs/032's table holds a colon, which is what
		// killed the text-assembly seam. A name that lost it would be a name
		// the linker stops collecting.
		if !strings.Contains(sym, ":") {
			t.Errorf("%s: the symbol %q holds no colon", c.src, sym)
		}
	}
}

func TestTypeNameString(t *testing.T) {
	for i, ty := range nameCorpusTypes(t) {
		c := nameCorpus[i]
		got, err := TypeNameString(ty)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got != c.name {
			t.Errorf("%s: name string %q, want %q", c.src, got, c.name)
		}
	}
}

// TestTypeNameStringShortensThePath checks the one way the two spellings
// differ.
//
// gc writes sync/atomic.Pointer in the link string and atomic.Pointer in the
// name string, and reflect.Type.String returns the second. The import path is
// the only thing an ir.Type carries, so the package name is derived from the
// path's last element.
func TestTypeNameStringShortensThePath(t *testing.T) {
	for _, tc := range []struct{ in, link, name string }{
		{"sync/atomic.Value", "sync/atomic.Value", "atomic.Value"},
		{"bytes.Buffer", "bytes.Buffer", "bytes.Buffer"},
		{"a/b/c.D", "a/b/c.D", "c.D"},
	} {
		ty := &Type{Kind: Struct, Name: tc.in}
		if err := Layout(ty); err != nil {
			t.Fatal(err)
		}
		if got, err := TypeLinkString(ty); err != nil || got != tc.link {
			t.Errorf("%s: link string %q (%v), want %q", tc.in, got, err, tc.link)
		}
		if got, err := TypeNameString(ty); err != nil || got != tc.name {
			t.Errorf("%s: name string %q (%v), want %q", tc.in, got, err, tc.name)
		}
	}
}

// TestTypeNameRefusals is the other half: the types an ir.Type cannot name.
//
// Each of these reduces two distinct Go types to one ir.Type, so a name built
// from it would be one name for two types. specs/032 says what that costs: the
// linker merges them and the program reads one type's descriptor for the
// other's values. The reason names the field the type boundary drops, so a
// count by cause says which field to add.
func TestTypeNameRefusals(t *testing.T) {
	for _, tc := range []struct {
		what string
		typ  *Type
		want string
	}{
		{"a channel", &Type{Kind: Chan, Elem: mustLayoutNamed(Int64, "int")}, "direction"},
		{"a function", &Type{Kind: FuncKind}, "signature"},
		{
			// The bit says the last parameter is a ... parameter and there is
			// no last parameter, so the spelling would drop the bit.
			"a variadic function with no parameters",
			&Type{Kind: FuncKind, Params: []*Type{}, Results: []*Type{}, Variadic: true},
			"variadic and its parameter list is empty",
		},
		{
			// ...T is spelled from the slice's element, and there is no slice.
			"a variadic function whose last parameter is not a slice",
			&Type{Kind: FuncKind, Params: []*Type{mustLayoutNamed(Int64, "int")}, Results: []*Type{}, Variadic: true},
			"rather than a slice",
		},
		{"an interface with methods", &Type{Kind: Interface}, "type of each method"},
		{
			"an interface method with no signature",
			&Type{Kind: Interface, Methods: []Method{{Name: "F"}}},
			"has no signature",
		},
		{
			// Two packages may declare an unexported method of one name and
			// they are different methods, so a spelling with no qualifier
			// would be one name for two interfaces.
			"an interface method that is unexported and carries no package",
			&Type{Kind: Interface, Methods: []Method{{Name: "f", Sig: &Type{Kind: FuncKind, Params: []*Type{}, Results: []*Type{}}}}},
			"the package that declares it is not in the IR type",
		},
		// A literal struct is spelled now, so what is refused is a field whose
		// own type has no spelling. The reason is the field's, not the
		// struct's, which is what makes a count by cause say which field
		// boundary is missing.
		{"a struct holding a channel", &Type{Kind: Struct, Fields: []Field{
			{Name: "c", Type: &Type{Kind: Chan, Elem: mustLayoutNamed(Int64, "int")}},
		}}, "direction"},
		{"a slice of channels", &Type{Kind: Slice, Elem: &Type{Kind: Chan, Elem: mustLayoutNamed(Int64, "int")}}, "direction"},
		{"an untyped constant", mustLayoutNamed(Int64, "untyped int"), "no canonical name"},
		{"a void", &Type{Kind: Void}, "no canonical name"},
		{"a tuple", &Type{Kind: Tuple}, "no canonical name"},
	} {
		if err := Layout(tc.typ); err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		_, err := TypeLinkString(tc.typ)
		if err == nil {
			t.Errorf("%s: was named", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the reason is %q, want it to name %q", tc.what, err, tc.want)
		}
	}
	if _, err := TypeLinkString(nil); err == nil {
		t.Error("a nil type was named")
	}
}

// TestTypeNameDepthIsBounded checks that a malformed graph is reported rather
// than recursed into.
//
// A well-formed type graph stops the walk at a defined type or a pointer, so
// nothing real reaches the limit. A malformed graph is exactly what an error
// message is describing, and a printer that recurses forever turns a
// reportable bug into a stack overflow.
func TestTypeNameDepthIsBounded(t *testing.T) {
	cyclic := &Type{Kind: Slice}
	cyclic.Elem = cyclic
	cyclic.Size, cyclic.Align = 24, 8
	if _, err := TypeLinkString(cyclic); err == nil {
		t.Error("a cyclic slice was named")
	} else if !strings.Contains(err.Error(), "nests deeper") {
		t.Errorf("the reason is %q", err)
	}
}

// TestTypeNameIsAFunctionOfTheTypeAlone is specs/032's first naming property.
//
// Two conversions of one Go type by two converters produce two ir.Type values,
// and both must name one symbol. A name that depended on the pointer, on the
// order of conversion, or on which package asked would break the linker's
// deduplication.
func TestTypeNameIsAFunctionOfTheTypeAlone(t *testing.T) {
	src := "package p\n\ntype T struct{ A int }\n\nvar v []map[string]*T\n"
	pkg, _, _ := buildTypecheck(t, src)
	o := pkg.Scope().Lookup("v")
	if o == nil {
		t.Fatal("v is not declared")
	}
	var names []string
	for i := 0; i < 2; i++ {
		ty, err := NewConverter().Convert(o.Type())
		if err != nil {
			t.Fatal(err)
		}
		name, err := TypeSymbol(ty)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if names[0] != names[1] {
		t.Errorf("two converters named %q and %q", names[0], names[1])
	}
	if names[0] != "type:[]map[string]*p.T" {
		t.Errorf("the name is %q", names[0])
	}
}

// TestNameRefusesAGenericInstantiation covers the case
// specs/032-type-descriptors-and-itabs.md records as refused by neither half.
//
// Converter's name for an instantiation is the generic type's, without the
// arguments, so two instantiations of one generic type share one name. The
// linker deduplicates by name, so a descriptor under that name would be one
// symbol for two different types and the runtime would read one
// instantiation's descriptor for the other's values.
//
// It was unreachable while every defined type was refused for having a method
// set the IR did not carry. It is reachable now, which is why the refusal is
// explicit rather than incidental.
func TestNameRefusesAGenericInstantiation(t *testing.T) {
	pkg, _, _ := buildTypecheck(t, `package p

type Box[T any] struct{ V T }

var (
	ints    Box[int]
	strings Box[string]
)
`)
	c := NewConverter()
	conv := func(name string) *Type {
		t.Helper()
		out, err := c.Convert(pkg.Scope().Lookup(name).Type())
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	a, b := conv("ints"), conv("strings")
	if !a.Instantiated || !b.Instantiated {
		t.Fatalf("Box[int] and Box[string] are not marked as instantiations (%v, %v)",
			a.Instantiated, b.Instantiated)
	}
	// The two would be one symbol, which is the failure the refusal prevents.
	if a.Name != b.Name {
		t.Fatalf("the names already differ (%s, %s), so this test proves nothing", a.Name, b.Name)
	}
	for _, tc := range []struct {
		what string
		typ  *Type
	}{
		{"Box[int]", a},
		{"Box[string]", b},
		{"a pointer to one", layOut(t, &Type{Kind: Ptr, Elem: a})},
		{"a slice of one", layOut(t, &Type{Kind: Slice, Elem: a})},
	} {
		if _, err := TypeSymbol(tc.typ); err == nil {
			t.Errorf("%s was given a symbol", tc.what)
		} else if !strings.Contains(err.Error(), "generic instantiation") {
			t.Errorf("%s: the refusal is %q", tc.what, err)
		}
	}
}

// layOut lays out a composite built over a corpus type.
func layOut(t *testing.T, typ *Type) *Type {
	t.Helper()
	if err := Layout(typ); err != nil {
		t.Fatal(err)
	}
	return typ
}
