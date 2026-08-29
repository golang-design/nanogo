// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"os"
	"path/filepath"
	"testing"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/export/pkgbits"
)

// The archives a compile hands the export writer.
//
// A generic declaration of another package cannot be written from what the
// checker recorded: its object dictionary and its body were numbered together
// when its own package was compiled. The writer copies it out of an archive
// instead (specs/015-export-data.md), and the only place that knows which
// archives the build named is here.

// TestArchivesAreEveryPackageFile checks what the writer is offered.
//
// Every packagefile entry, because a generic declaration usually reaches a
// compilation through a package that re-exported it and not through its own.
// No packageshlib entry, because that names a shared library and not an
// archive.
func TestArchivesAreEveryPackageFile(t *testing.T) {
	cfg := mustImportCfg(t, "packagefile b=/pkg/b.a\npackagefile a=/pkg/a.a\npackageshlib c=/pkg/c.so\n")
	got := archives(cfg)
	want := []export.Archive{{Path: "b", File: "/pkg/b.a"}, {Path: "a", File: "/pkg/a.a"}}
	if len(got) != len(want) {
		t.Fatalf("archives listed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("archives listed %v, want %v", got, want)
			break
		}
	}
	if archives(nil) != nil {
		t.Error("archives of no configuration is not empty")
	}
}

// TestCompileCarriesAForeignGenericTypeWithMethods is the driver's half of
// specs/015-export-data.md's copy.
//
// sync/atomic.Pointer is the declaration that stops four of nanogo's own
// packages. It is a generic type of another package that declares methods, so
// a package holding one in its exported surface can only write its export data
// by copying the declaration and its four method bodies out of an archive. The
// archive is named by -importcfg, and this is what says the compile passes it
// through.
func TestCompileCarriesAForeignGenericTypeWithMethods(t *testing.T) {
	arm64Only(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/foreign\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := gcArchive(t, dir, "sync/atomic")
	cfgFile := filepath.Join(dir, "importcfg")
	if err := os.WriteFile(cfgFile, []byte("packagefile sync/atomic="+archive+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const src = `package main

import "sync/atomic"

// P is exported, so the export data has to carry atomic.Pointer in full.
var P atomic.Pointer[int]

func main() {}
`
	out, err := compileSource(t, src, func(c *Config) {
		c.Pack = true
		c.ImportCfg = mustReadImportCfg(t, cfgFile)
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := export.Payload(raw)
	if err != nil {
		t.Fatal(err)
	}
	dec := pkgbits.NewPkgDecoder("main", string(payload))
	found := false
	for i := range dec.NumElems(pkgbits.SectionObj) {
		path, name, tag := dec.PeekObj(pkgbits.Index(i))
		if path == "sync/atomic" && name == "Pointer" {
			if tag == pkgbits.ObjStub {
				t.Fatal("the export data holds sync/atomic.Pointer as a stub, which no reader of a file accepts")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("the export data holds no element for sync/atomic.Pointer")
	}
}
