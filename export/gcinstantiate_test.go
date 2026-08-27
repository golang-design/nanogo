// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
)

// gc reading a generic declaration nanogo wrote, and running it.
//
// TestGcReadsWhatNanogoWrote compiles an importer that names a declaration,
// which runs gc's readers over the bytes. A generic declaration needs more
// than that: gc has to resolve every slot of the object dictionary against
// the type arguments an importer instantiated it with, and compile the body
// nanogo wrote into the importing package. A slot resolved to a different
// type is a type gc reads without complaint, so what the program prints is
// the check and a diagnostic is not.
//
// The instantiations are at int and at a struct holding a pointer, which have
// different sizes and different pointer maps. A dictionary standing in for
// the other one is then a value that differs rather than one that happens to
// agree.

// pkgdefArchive writes an archive whose only member is the export data.
//
// internal/exportdata reads __.PKGDEF and a compile does not look at the
// object, so this is everything gc needs in order to import a package.
func pkgdefArchive(t *testing.T, header, dir, name string, payload []byte) string {
	t.Helper()
	def, err := Definition(header, false, payload)
	if err != nil {
		t.Fatal(err)
	}
	var b []byte
	b = append(b, archiveMagic...)
	b = append(b, fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", definitionMember, 0, 0, 0, 0o644, len(def))...)
	b = append(b, def...)
	if len(def)%2 != 0 {
		b = append(b, 0)
	}
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// instantiatingMain is the program gc compiles against nanogo's export data.
//
// Every call instantiates one of the generic declarations of [genericSource],
// so gc stencils each body out of nanogo's bytes. The values printed are the
// answer, because a wrong dictionary slot is a plausible wrong value.
const instantiatingMain = "package main\n\n" +
	"import (\n\t\"fmt\"\n\n\tlib \"nanogo.example/gen/lib\"\n)\n\n" +
	"type item struct {\n\tp *int\n\tn int\n}\n\n" +
	"func main() {\n" +
	"\tints := []int{3, 5, 7}\n" +
	"\tfmt.Println(lib.Last(ints), lib.Pair(1, 2), lib.Ends(ints), lib.Any(9))\n" +
	"\tn := 4\n" +
	"\titems := []item{{nil, 1}, {&n, 2}}\n" +
	"\tlast := lib.Last(items)\n" +
	"\tends := lib.Ends(items)\n" +
	"\tfmt.Println(last.n, *last.p, len(ends), ends[0].n, *ends[1].p)\n" +
	"\tfmt.Println(lib.Last([]int{}), lib.Last([]item{}).n, lib.Any(items[0]))\n" +
	"}\n"

// TestGcInstantiatesAGenericNanogoWrote is the oracle specs/013-generics.md's
// M2 gate is measured on.
//
// nanogo writes the export data of a library of generic functions, gc
// compiles a program that instantiates each of them at two concrete types,
// and the program runs. Nothing of the library is compiled from source here:
// every instantiation is code gc built out of the body and the dictionary
// nanogo wrote.
//
// The link takes the same archive, which holds no object at all. A package
// whose declarations are all generic has no code of its own: the archive the
// go command builds for this library holds two DWARF compilation unit symbols
// and nothing else, because every instantiation is code the importing package
// generates.
func TestGcInstantiatesAGenericNanogoWrote(t *testing.T) {
	goCmd := goTool(t)
	tc, err := obj.VerifyToolchain()
	if err != nil {
		t.Skipf("cannot probe the installed toolchain: %v", err)
	}

	const path = "nanogo.example/gen/lib"
	mod := t.TempDir()
	write := func(name, body string) {
		full := filepath.Join(mod, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/gen\n\ngo 1.27\n")
	write("lib/lib.go", genericSource)
	write("main.go", instantiatingMain)

	// The archive of every package the program needs, which is what an
	// -importcfg names. The library's own entry is replaced below.
	deps := make(map[string]string)
	out, err := runGo(t, goCmd, mod, "list", "-deps", "-export", "-f", "{{.ImportPath}}\t{{.Export}}", ".")
	if err != nil {
		t.Skipf("go list -deps -export: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\t", 2)
		if len(f) == 2 && f[1] != "" {
			deps[f[0]] = f[1]
		}
	}
	if deps[path] == "" {
		t.Skipf("the go command built no archive for %s", path)
	}

	// nanogo's export data for the library, from the same source.
	payload, _, _ := writeSource(t, path, genericSource, nil)
	nano := pkgdefArchive(t, tc.Header, mod, "nano-lib.a", payload)

	cfg := func(name, lib string) string {
		var b strings.Builder
		for p, a := range deps {
			if p == path {
				a = lib
			}
			fmt.Fprintf(&b, "packagefile %s=%s\n", p, a)
		}
		file := filepath.Join(mod, name)
		if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}

	// The compile reads nanogo's export data, so every instantiation gc puts
	// in this object is stenciled from the body and the dictionary nanogo
	// wrote.
	object := filepath.Join(mod, "main.o")
	if out, err := runGo(t, goCmd, mod, "tool", "compile", "-p", "main", "-o", object,
		"-importcfg", cfg("compilecfg", nano), filepath.Join(mod, "main.go")); err != nil {
		t.Fatalf("gc cannot compile against the generic declarations nanogo wrote: %v\n%s", err, out)
	}

	prog := filepath.Join(mod, "prog")
	if out, err := runGo(t, goCmd, mod, "tool", "link", "-importcfg", cfg("linkcfg", nano),
		"-o", prog, object); err != nil {
		t.Fatalf("the program does not link: %v\n%s", err, out)
	}

	b, err := exec.Command(prog).CombinedOutput()
	if err != nil {
		t.Fatalf("the program did not run: %v\n%s", err, b)
	}
	want := "7 [1 2] [3 7] 9\n2 4 2 1 4\n0 0 {<nil> 1}\n"
	if string(b) != want {
		t.Errorf("the program printed %q, want %q", b, want)
	}
}

// runGo runs the go command in dir and returns its combined output.
func runGo(t *testing.T, goCmd, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(goCmd, args...)
	cmd.Dir = dir
	cmd.Env = env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}
