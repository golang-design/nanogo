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
			name: "a struct",
			src:  "package lib\n\ntype Point struct{ X, Y int }\n",
			want: []string{"Point", "a.go:3:6", "type:lib.Point", "specs/032"},
		},
		{
			name: "a named basic type",
			src:  "package lib\n\ntype Code int\n",
			want: []string{"Code", "type:lib.Code", "specs/032"},
		},
		{
			name: "an interface",
			src:  "package lib\n\ntype I interface{ F() int }\n",
			want: []string{"I", "type:lib.I"},
		},
		{
			// Every declared type, not only the exported ones. An importer
			// cannot name an unexported type, but cmd/link's defgotype walks
			// into a struct's fields, so a variable of an exported type
			// reaches the unexported types inside it.
			name: "an unexported type",
			src:  "package lib\n\ntype hidden struct{ n int }\n\nfunc F() int { return hidden{n: 1}.n }\n",
			want: []string{"hidden", "type:lib.hidden"},
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
