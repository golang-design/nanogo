// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
)

// TestParseSymABIs covers the format and every way a line can be wrong.
//
// Every malformed line is an error and never a skip, for the reason gc calls
// log.Fatalf on each of the three: a reader that dropped the lines it did not
// understand would decide that a symbol needs no wrapper because it never read
// the line that says it does.
func TestParseSymABIs(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want string // a substring of the error, or "" when the file is good
	}{
		{name: "empty"},
		{name: "a comment and a blank line", in: "# a comment\n\n   \n"},
		{name: "a def and a ref", in: "def p.f ABI0\nref p.g ABIInternal\n"},
		{name: "leading and trailing space", in: "   def p.f ABI0   \n"},
		{name: "no trailing newline", in: "def p.f ABI0"},
		{name: "an unknown verb", in: "define p.f ABI0\n", want: `invalid symabi type "define"`},
		{name: "two fields", in: "def p.f\n", want: `syntax is "def sym abi"`},
		{name: "four fields", in: "def p.f ABI0 extra\n", want: `syntax is "def sym abi"`},
		{name: "an unknown abi", in: "def p.f ABI2\n", want: `unknown abi "ABI2"`},
		{name: "a lower case abi", in: "def p.f abi0\n", want: `unknown abi "abi0"`},
		{name: "the line number is 1-based", in: "\n\ndef p.f ABI3\n", want: "sym.abis:3:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSymABIs("sym.abis", []byte(tt.in))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("ParseSymABIs: %v", err)
			case tt.want == "":
				return
			case err == nil:
				t.Fatalf("ParseSymABIs accepted %q", tt.in)
			case !strings.Contains(err.Error(), tt.want):
				t.Errorf("the message does not carry %q:\n%v", tt.want, err)
			}
		})
	}
}

// TestSymABIsTables checks that a def is the symbol's ABI and a ref is a set.
//
// The two are different shapes on purpose and gc keeps them apart the same
// way. One assembly file defines a symbol under one ABI. Several assembly
// files may name it, under either ABI, and the wrapper decision is a set
// difference over the union.
func TestSymABIsTables(t *testing.T) {
	s, err := ParseSymABIs("sym.abis", []byte(
		"def p.f ABI0\n"+
			"ref p.g ABI0\n"+
			"ref p.g ABIInternal\n"+
			"ref p.g ABI0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if abi, ok := s.Def("p.f"); !ok || abi != obj.ABI0 {
		t.Errorf("Def(p.f) = %d, %v; want %d, true", abi, ok, obj.ABI0)
	}
	if _, ok := s.Def("p.g"); ok {
		t.Error("Def(p.g) found a definition and the file has only references")
	}
	refs := s.Refs("p.g")
	if !refs.Has(obj.ABI0) || !refs.Has(obj.ABIInternal) {
		t.Errorf("Refs(p.g) = %#b, want both ABIs", refs)
	}
	if got, want := len(s.Records), 4; got != want {
		t.Errorf("the file holds %d records, want %d", got, want)
	}
	if got, want := strings.Join(s.Defs(), " "), "p.f"; got != want {
		t.Errorf("Defs() = %q, want %q", got, want)
	}
	// A nil reader answers for a package with no assembly, so every caller
	// can ask without testing the flag first.
	var none *SymABIs
	if _, ok := none.Def("p.f"); ok || none.Refs("p.f") != 0 || none.Defs() != nil {
		t.Error("a nil SymABIs answered as though it held something")
	}
}

// TestSymABIsReadsTheRealFiles is the drift gate on the format.
//
// The files are the ones cmd/asm -gensymabis writes for the eight packages
// specs/047-abi-wrappers.md measured, generated here rather than checked in,
// so a change to the assembler's output fails this test rather than being
// read wrongly. The counts are the measured ones and the shape of each file
// is asserted rather than its bytes: what matters is that every line decodes
// and that the def and ref tables say what gc's do.
func TestSymABIsReadsTheRealFiles(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	goroot := stdSourceRoot(t)
	work := t.TempDir()

	for _, tt := range []struct {
		path string
		defs int // "def" lines, which is how many symbols the assembly defines
		abi0 int // how many of those are ABI0
	}{
		{"internal/abi", 1, 1},
		{"internal/bytealg", 10, 0},
		{"internal/chacha8rand", 1, 0},
		{"internal/cpu", 4, 4},
		{"internal/runtime/atomic", 49, 49},
		{"internal/runtime/maps", 3, 0},
		{"internal/runtime/sys", 3, 3},
	} {
		t.Run(tt.path, func(t *testing.T) {
			p := loadStdPackage(t, goroot, work, tt.path)
			if p.SymABIs == "" {
				t.Fatalf("%s has no assembly", tt.path)
			}
			s, err := ReadSymABIs(p.SymABIs)
			if err != nil {
				t.Fatalf("ReadSymABIs: %v", err)
			}
			defs := s.Defs()
			if len(defs) != tt.defs {
				t.Errorf("%d defined symbols, want %d:\n%s", len(defs), tt.defs,
					strings.Join(defs, "\n"))
			}
			abi0 := 0
			for _, sym := range defs {
				if abi, _ := s.Def(sym); abi == obj.ABI0 {
					abi0++
				}
			}
			if abi0 != tt.abi0 {
				t.Errorf("%d of the definitions are ABI0, want %d", abi0, tt.abi0)
			}
		})
	}
}

// TestSymABIsDefOutsideThePackagePrefix records a shape the reader must not
// reject.
//
// internal/bytealg's file holds "def runtime.cmpstring ABIInternal" and "def
// runtime.memequal ABIInternal", which are not internal/bytealg symbols. gc
// matches them through the //go:linkname on the Go declarations that stand in
// for them. A reader that refused a def outside the package prefix would
// refuse a real file, so the rule is in the wrapper gate and not here.
func TestSymABIsDefOutsideThePackagePrefix(t *testing.T) {
	s, err := ParseSymABIs("sym.abis", []byte("def runtime.cmpstring ABIInternal\n"))
	if err != nil {
		t.Fatalf("ParseSymABIs: %v", err)
	}
	if abi, ok := s.Def("runtime.cmpstring"); !ok || abi != obj.ABIInternal {
		t.Errorf("Def(runtime.cmpstring) = %d, %v", abi, ok)
	}
}
