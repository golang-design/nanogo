// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"strings"
	"testing"
)

// TestExportedTypeRefusalNamesWhatIsMissing is the unit form of
// internal/e2e's TestNanogoRefusesATypeAnImporterCouldNotLinkAgainst.
//
// Each row is a declaration an importer would need a runtime type descriptor
// for. The message is the whole point of the check: what it replaces is a
// link failure reading "relocation target go:info.<path>.<Type> not defined",
// which names neither the package that owes the symbol nor the type nor the
// fix.
func TestExportedTypeRefusalNamesWhatIsMissing(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			// A type with a method needs an UncommonType whose entries carry
			// the method's signature and the two ABI wrappers. Neither is
			// built, and an entry left out would make reflect report an empty
			// method set for a type that has one.
			name: "a type with a method",
			src:  "package lib\n\ntype Code int\n\nfunc (c Code) String() string { return \"\" }\n",
			want: []string{"Code", "type:lib.Code", "String", "ABI wrappers"},
		},
		{
			// A struct whose fields do not compare as one region of memory
			// needs a generated equality function, which specs/032 has no
			// writer for. Leaving Equal nil would make the runtime panic on a
			// map whose key is this type.
			name: "a struct that compares field by field",
			src:  "package lib\n\ntype Label struct{ Key, Value string }\n",
			want: []string{"Label", "type:lib.Label", "equality function"},
		},
		{
			// The closure and not only the root. cmd/link's defgotype walks
			// from a struct descriptor into the type of every field, so a
			// field whose own descriptor cannot be written stops the package
			// even though the struct's could be.
			name: "a struct holding a map",
			src:  "package lib\n\ntype Table struct{ M map[string]int }\n",
			want: []string{"Table", "type:lib.Table", "group type"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSource(t, tt.src, func(c *Config) { c.Package = "lib" })
			if err == nil {
				t.Fatal("the package compiled although it declares a type no importer could link against")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// TestExportedTypeIsDescribedForAnInterface is the row that left the table
// above.
//
// A descriptor for an interface with methods needs an Imethod per method, each
// an offset to the descriptor of the method's signature. That signature is a
// function literal, and it had no canonical name until ir/rtype.go spelled one,
// so the whole package was refused. Both spellings are there now, so the
// package compiles and the importer's link resolves.
func TestExportedTypeIsDescribedForAnInterface(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	for _, tc := range []struct{ what, src string }{
		{"an exported method", "package lib\n\ntype I interface{ F() int }\n"},
		{
			// An unexported method's name is qualified by the package that
			// declares it, which is this one, so the name encoder writes no
			// package path of its own.
			"an unexported method beside an exported one",
			"package lib\n\ntype I interface {\n\tRead(p []byte) (int, error)\n\tflush() error\n}\n",
		},
		{"an embedded interface", "package lib\n\ntype R interface{ Read([]byte) (int, error) }\n\ntype RC interface {\n\tR\n\tClose() error\n}\n"},
		{"a literal interface in a struct field", "package lib\n\ntype Holder struct{ V interface{ Len() int } }\n"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if _, err := compileSource(t, tc.src, func(c *Config) { c.Package = "lib" }); err != nil {
				t.Errorf("the package was refused: %v", err)
			}
		})
	}
}

// TestExportedTypeRefusalSkipsMainAndAliases records the two shapes the check
// lets through, and why each is safe.
//
// A main package is the one package nothing can import, which is the reason
// the export data reader came before the writer at all
// (specs/015-export-data.md). Refusing one would take away the case nanogo
// started from, and it would buy nothing: the descriptors a main package's own
// code needs come from the lowering pass, which reports what it cannot write
// where it arises.
//
// An alias declares no type. Its right-hand side belongs to whichever package
// declares that, and the descriptor is owed by that package.
func TestExportedTypeRefusalSkipsMainAndAliases(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)

	// A main package with a struct in it, which the lowering pass can name a
	// descriptor for because its own code allocates one.
	if _, err := compileSource(t, "package main\n\ntype point struct{ x, y int }\n\n"+
		"func f() int {\n\tp := point{x: 1, y: 2}\n\treturn p.x + p.y\n}\n\nfunc main() { f() }\n", nil); err != nil {
		t.Errorf("a main package with a type in it was refused: %v", err)
	}

	// A library whose only type declaration is an alias.
	if _, err := compileSource(t, "package lib\n\ntype Word = int\n\nfunc F(w Word) Word { return w }\n",
		func(c *Config) { c.Package = "lib" }); err != nil {
		t.Errorf("a library whose only type declaration is an alias was refused: %v", err)
	}
}
