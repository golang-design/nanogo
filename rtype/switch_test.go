// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype_test

import (
	"encoding/binary"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
)

// ifaceType builds an interface with methods that has a link string.
//
// The methods list is what makes it not the empty interface, and the name is
// what makes it nameable: an anonymous interface is spelled from its method
// list, and this file is about the cache and not about that spelling.
func ifaceType(t *testing.T, name, path string) *ir.Type {
	t.Helper()
	return lay(t, &ir.Type{
		Kind:    ir.Interface,
		Name:    path + "." + name,
		PkgPath: path,
		Methods: []ir.Method{{Name: "M", Sig: lay(t, &ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{}})}},
	})
}

// TestTypeAssertLayout is the encoder against the structure.
func TestTypeAssertLayout(t *testing.T) {
	iface := ifaceType(t, "Reader", "io")
	for _, canFail := range []bool{false, true} {
		s, err := rtype.TypeAssert(ir.TypeAssert{Sym: "main.f..typeAssert.0", Iface: iface, CanFail: canFail})
		if err != nil {
			t.Fatalf("TypeAssert: %v", err)
		}
		if len(s.Data) != 24 {
			t.Errorf("the symbol is %d bytes, want 24", len(s.Data))
		}
		want := byte(0)
		if canFail {
			want = 1
		}
		if s.Data[16] != want {
			t.Errorf("CanFail is %d, want %d", s.Data[16], want)
		}
		// The first two words are relocations and hold no bytes of their own.
		for i := 0; i < 16; i++ {
			if s.Data[i] != 0 {
				t.Fatalf("byte %d of the two pointer words is %d, and a relocation adds to what is there", i, s.Data[i])
			}
		}
		if s.Kind != obj.SDATA {
			t.Errorf("the symbol is %v, and the runtime stores into it, so a read-only section faults", s.Kind)
		}
		if s.Align != ir.PtrSize {
			t.Errorf("the symbol is aligned to %d, and the install is an atomic compare-and-swap on the first word", s.Align)
		}
		if !s.Local {
			t.Error("the symbol is not local, and it belongs to one function of one package")
		}
		if s.Dupok {
			t.Error("the symbol is duplicate-tolerant, and cmd/link would merge two caches that are two sites")
		}
		if s.Gotype != "type:internal/abi.TypeAssert" {
			t.Errorf("the symbol's Go type is %q, and cmd/link builds the data section's pointer map out of it", s.Gotype)
		}
		if len(s.Relocs) != 2 {
			t.Fatalf("the symbol has %d relocations, want the cache and the interface", len(s.Relocs))
		}
		if got := s.Relocs[0]; got.Off != 0 || got.Target != "runtime.emptyTypeAssertCache" {
			t.Errorf("the Cache word is %+v, and runtime.typeAssert reads Cache.Mask before anything else", got)
		}
		name, err := ir.TypeSymbol(iface)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Relocs[1]; got.Off != 8 || got.Target != name {
			t.Errorf("the Inter word is %+v, want %s at 8", got, name)
		}
	}
}

// TestInterfaceSwitchLayout is the encoder against the structure, including
// the array that is longer than the type describes.
func TestInterfaceSwitchLayout(t *testing.T) {
	cases := []*ir.Type{
		ifaceType(t, "Reader", "io"),
		ifaceType(t, "Writer", "io"),
		ifaceType(t, "Closer", "io"),
	}
	s, err := rtype.InterfaceSwitch(ir.InterfaceSwitch{Sym: "main.g..interfaceSwitch.0", Cases: cases})
	if err != nil {
		t.Fatalf("InterfaceSwitch: %v", err)
	}
	if want := 16 + 3*8; len(s.Data) != want {
		t.Fatalf("the symbol is %d bytes, want %d", len(s.Data), want)
	}
	if n := binary.LittleEndian.Uint64(s.Data[8:]); n != 3 {
		t.Errorf("NCases is %d, want 3; the runtime slices Cases to this length", n)
	}
	if s.Gotype != "type:internal/abi.InterfaceSwitch" {
		t.Errorf("the symbol's Go type is %q", s.Gotype)
	}
	if len(s.Relocs) != 4 {
		t.Fatalf("the symbol has %d relocations, want the cache and three cases", len(s.Relocs))
	}
	if s.Relocs[0].Target != "runtime.emptyInterfaceSwitchCache" {
		t.Errorf("the Cache word is %q", s.Relocs[0].Target)
	}
	// The order is the answer. runtime.interfaceSwitch returns the index of
	// the first case the dynamic type implements, so a case written at another
	// offset runs another clause.
	for i, c := range cases {
		name, err := ir.TypeSymbol(c)
		if err != nil {
			t.Fatal(err)
		}
		got := s.Relocs[1+i]
		if got.Off != int32(16+i*8) || got.Target != name {
			t.Errorf("case %d is %+v, want %s at %d", i, got, name, 16+i*8)
		}
	}
}

// TestSwitchCacheRefusals records what has no cache.
func TestSwitchCacheRefusals(t *testing.T) {
	empty := lay(t, &ir.Type{Kind: ir.Interface, EmptyIface: true})
	iface := ifaceType(t, "Reader", "io")
	for _, tc := range []struct {
		what string
		run  func() error
		want string
	}{
		{"an assertion with no symbol", func() error {
			_, err := rtype.TypeAssert(ir.TypeAssert{Iface: iface})
			return err
		}, "has no symbol"},
		{"an assertion to a concrete type", func() error {
			_, err := rtype.TypeAssert(ir.TypeAssert{Sym: "main.f..typeAssert.0", Iface: intType(t)})
			return err
		}, "runtime.typeAssert answers which itab implements an interface"},
		{"an assertion to the empty interface", func() error {
			_, err := rtype.TypeAssert(ir.TypeAssert{Sym: "main.f..typeAssert.0", Iface: empty})
			return err
		}, "every type implements"},
		{"a switch with no case", func() error {
			_, err := rtype.InterfaceSwitch(ir.InterfaceSwitch{Sym: "main.g..interfaceSwitch.0"})
			return err
		}, "lists no case"},
		{"a switch over a concrete type", func() error {
			_, err := rtype.InterfaceSwitch(ir.InterfaceSwitch{Sym: "main.g..interfaceSwitch.0", Cases: []*ir.Type{intType(t)}})
			return err
		}, "every case of an interface switch is an interface"},
		{"a switch over the empty interface", func() error {
			_, err := rtype.InterfaceSwitch(ir.InterfaceSwitch{Sym: "main.g..interfaceSwitch.0", Cases: []*ir.Type{empty}})
			return err
		}, "every non-nil dynamic type implements"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("no refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestSwitchCacheReferenced is the set of descriptors the object owes.
//
// cmd/link resolves a relocation by name, and a name nothing defines is a link
// failure that reports the descriptor and not the cache that points at it.
func TestSwitchCacheReferenced(t *testing.T) {
	a := ifaceType(t, "Reader", "io")
	b := ifaceType(t, "Writer", "io")
	if got := rtype.TypeAssertReferenced(ir.TypeAssert{Sym: "s", Iface: a}); len(got) != 1 || got[0] != a {
		t.Errorf("an assertion points at %v, want just the interface", got)
	}
	got := rtype.InterfaceSwitchReferenced(ir.InterfaceSwitch{Sym: "s", Cases: []*ir.Type{a, b}})
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("a switch points at %v, want both cases in order", got)
	}
}

// TestSwitchStructsAgainstTheRuntimeSource reads internal/abi's declarations
// and checks the offsets this package writes against them.
//
// The offsets are constants and cannot be computed from a mirrored struct,
// because a mirrored struct is laid out by the compiler that builds nanogo and
// this has to be the layout of the runtime nanogo links against. So the
// runtime's own source is the oracle, exactly as it is for rtsym's signatures:
// a field inserted in front of Cache moves the word the compare-and-swap
// writes, and nothing else reports it.
func TestSwitchStructsAgainstTheRuntimeSource(t *testing.T) {
	file := filepath.Join(runtime.GOROOT(), "src", "internal", "abi", "switch.go")
	src, err := os.ReadFile(file)
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s could not be read: %v", file, err)
		}
		t.Skipf("no internal/abi source under GOROOT: %v", err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	structs := map[string][]field{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			structs[ts.Name.Name] = fieldsOf(st)
		}
	}
	// TypeAssert: a pointer, a pointer and a bool, so 24 bytes with the bool
	// at 16 and the padding after it.
	want := []field{{"Cache", 8}, {"Inter", 8}, {"CanFail", 1}}
	checkStruct(t, structs, "TypeAssert", want, []int{0, 8, 16}, 24)
	// InterfaceSwitch: a pointer, an int and an array of one pointer. The
	// symbol is longer than this by one word per case after the first, which
	// TestInterfaceSwitchLayout covers.
	want = []field{{"Cache", 8}, {"NCases", 8}, {"Cases", 8}}
	checkStruct(t, structs, "InterfaceSwitch", want, []int{0, 8, 16}, 24)
}

type field struct {
	name string
	size int
}

// fieldsOf returns the fields of a struct with the width of each, for the
// three shapes internal/abi's two structures use.
func fieldsOf(st *ast.StructType) []field {
	var out []field
	for _, f := range st.Fields.List {
		size := 0
		switch e := f.Type.(type) {
		case *ast.StarExpr:
			size = 8
		case *ast.Ident:
			switch e.Name {
			case "int", "uintptr":
				size = 8
			case "bool":
				size = 1
			}
		case *ast.ArrayType:
			// [1]*T, the variable-sized tail. One element here.
			if _, ok := e.Elt.(*ast.StarExpr); ok {
				size = 8
			}
		}
		for _, n := range f.Names {
			out = append(out, field{n.Name, size})
		}
	}
	return out
}

// checkStruct compares one declaration with the offsets this package writes.
func checkStruct(t *testing.T, structs map[string][]field, name string, want []field, offsets []int, size int) {
	t.Helper()
	got, ok := structs[name]
	if !ok {
		t.Fatalf("internal/abi declares no %s", name)
	}
	if len(got) != len(want) {
		t.Fatalf("internal/abi.%s has %v, and this package writes %v", name, got, want)
	}
	off := 0
	for i, f := range got {
		if f != want[i] {
			t.Fatalf("internal/abi.%s field %d is %+v, and this package writes %+v", name, i, f, want[i])
		}
		if f.size == 0 {
			t.Fatalf("internal/abi.%s field %s has a type this test does not size", name, f.name)
		}
		// Every field here is either pointer-width or a bool, so a field of
		// eight bytes is aligned to eight and a bool to one.
		if f.size == 8 {
			off = (off + 7) &^ 7
		}
		if off != offsets[i] {
			t.Errorf("internal/abi.%s.%s is at %d, and this package writes %d", name, f.name, off, offsets[i])
		}
		off += f.size
	}
	if got := (off + 7) &^ 7; got != size {
		t.Errorf("internal/abi.%s is %d bytes, and this package writes %d", name, got, size)
	}
}

// TestSwitchCacheSizesAgainstGCObject compiles a program with gc and compares
// the size of the two symbols gc wrote with the size this package writes.
//
// The layout test above reads the declarations. This reads what the compiler
// made of them, which is the number the runtime indexes by.
func TestSwitchCacheSizesAgainstGCObject(t *testing.T) {
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
	write("go.mod", "module nanogoswitch\n\ngo 1.25\n")
	write("x.go", switchOracleSource)
	build := exec.Command(goCmd, "build", "-o", filepath.Join(dir, "unused.a"), ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOARCH=arm64", "GOOS=darwin")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("gc could not build the oracle: %v\n%s", err, out)
	}
	list := exec.Command(goCmd, "list", "-export", "-f", "{{.Export}}", ".")
	list.Dir = dir
	list.Env = build.Env
	out, err := list.Output()
	if err != nil {
		t.Skipf("go list -export: %v", err)
	}
	object := strings.TrimSpace(string(out))
	if object == "" {
		t.Skip("go list -export named no object")
	}
	nm, err := exec.Command(goCmd, "tool", "nm", "-size", object).Output()
	if err != nil {
		t.Skipf("go tool nm: %v", err)
	}
	sizes := map[string]int64{}
	for _, line := range strings.Split(string(nm), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		size, err := strconv.ParseInt(f[len(f)-3], 10, 64)
		if err != nil {
			continue
		}
		sizes[f[len(f)-1]] = size
	}
	iface := ifaceType(t, "Reader", "io")
	ours, err := rtype.TypeAssert(ir.TypeAssert{Sym: "s", Iface: iface, CanFail: true})
	if err != nil {
		t.Fatal(err)
	}
	// gc numbers per package and the source below makes exactly one of each.
	if got, ok := sizes["nanogoswitch..typeAssert.0"]; !ok {
		t.Errorf("gc's object has no nanogoswitch..typeAssert.0")
	} else if got != int64(len(ours.Data)) {
		t.Errorf("gc's TypeAssert is %d bytes and this package writes %d", got, len(ours.Data))
	}
	// Two interface cases, so the array is two words and the symbol is one
	// word longer than the structure.
	sw, err := rtype.InterfaceSwitch(ir.InterfaceSwitch{
		Sym:   "s",
		Cases: []*ir.Type{ifaceType(t, "Reader", "io"), ifaceType(t, "Writer", "io")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := sizes["nanogoswitch..interfaceSwitch.0"]; !ok {
		t.Errorf("gc's object has no nanogoswitch..interfaceSwitch.0")
	} else if got != int64(len(sw.Data)) {
		t.Errorf("gc's InterfaceSwitch over two cases is %d bytes and this package writes %d", got, len(sw.Data))
	}
}

// switchOracleSource is one assertion to an interface and one type switch over
// two interface cases, which is one symbol of each kind.
const switchOracleSource = `package nanogoswitch

type I interface{ M() }
type J interface{ N() }

//go:noinline
func Assert(v any) I { return v.(I) }

//go:noinline
func Switch(v any) int {
	switch v.(type) {
	case I:
		return 1
	case J:
		return 2
	}
	return 0
}
`
