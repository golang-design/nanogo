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
	gcRunsAgainstNanogo(t, "nanogo.example/gen", genericSource, instantiatingMain,
		"7 [1 2] [3 7] 9\n2 4 2 1 4\n0 0 {<nil> 1}\n")
}

// gcRunsAgainstNanogo compiles main against nanogo's export data for lib and
// runs the program, and checks what it printed.
//
// Nothing of the library is compiled from source: the archive gc imports holds
// only the export data nanogo wrote, so every instantiation in the program is
// code gc stenciled out of nanogo's bodies and nanogo's dictionaries.
func gcRunsAgainstNanogo(t *testing.T, module, lib, main, want string) {
	t.Helper()
	goCmd := goTool(t)
	tc, err := obj.VerifyToolchain()
	if err != nil {
		t.Skipf("cannot probe the installed toolchain: %v", err)
	}

	path := module + "/lib"
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
	write("go.mod", "module "+module+"\n\ngo 1.27\n")
	write("lib/lib.go", lib)
	write("main.go", main)

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
	payload, _, _ := writeSource(t, path, lib, nil)
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
	if string(b) != want {
		t.Errorf("the program printed %q, want %q", b, want)
	}
}

// genericTypeSource declares two generic types whose methods name every kind
// of slot the shared dictionary holds.
//
// The dictionary is the type's and not the method's, so the slots run over the
// underlying type and then over each method in declaration order. A method
// numbered against a dictionary of its own would name a slot that holds
// another type, which gc resolves without complaint, so the values the program
// prints are the check.
//
// List declares more than one method and mixes a value receiver with a pointer
// one, so a method whose slots were numbered too low would name the slots of
// the method declared before it. Box exists so that two shared dictionaries
// are in the file at once and a body cannot be reading the other one.
//
// Pair declares two type parameters, which is what measures the position a
// receiver's type parameter resolves to. A method names them through objects
// of its own declaration, so the position and nothing else says which one it
// is ([Dict.TypeParamIndex]), and with one type parameter every position is
// zero and a wrong rule is right by accident. The two are instantiated at
// types that are not each other, so a swapped position is a value that
// differs.
const genericTypeSource = "package p\n\n" +
	"type List[T any] struct{ items []T }\n\n" +
	// A pointer receiver, which is a derived type of its own, and a
	// derived parameter.
	"func (l *List[T]) Push(v T) { l.items = append(l.items, v) }\n\n" +
	// A value receiver and no derived type in the signature at all.
	"func (l List[T]) Len() int { return len(l.items) }\n\n" +
	// A derived result.
	"func (l List[T]) At(i int) T { return l.items[i] }\n\n" +
	// A derived slice result.
	"func (l List[T]) All() []T { return l.items }\n\n" +
	// A derived value converted to an interface, which is a method table
	// the dictionary holds.
	"func (l List[T]) Any(i int) any { return l.items[i] }\n\n" +
	// A call of a generic function whose type argument is derived, which is
	// a subdictionary the dictionary holds.
	"func (l List[T]) Last() T { return last(l.items) }\n\n" +
	"func last[T any](xs []T) T {\n" +
	"\tvar zero T\n" +
	"\tif len(xs) == 0 {\n\t\treturn zero\n\t}\n" +
	"\treturn xs[len(xs)-1]\n" +
	"}\n\n" +
	"type Box[T any] struct{ v T }\n\n" +
	"func (b Box[T]) Get() T { return b.v }\n\n" +
	"func (b *Box[T]) Set(v T) { b.v = v }\n\n" +
	"type Pair[K, V any] struct {\n\tk K\n\tv V\n}\n\n" +
	"func (p Pair[K, V]) Key() K { return p.k }\n\n" +
	"func (p Pair[K, V]) Val() V { return p.v }\n\n" +
	"func (p *Pair[K, V]) Set(k K, v V) { p.k, p.v = k, v }\n\n" +
	"func (p Pair[K, V]) Swap() Pair[V, K] { return Pair[V, K]{p.v, p.k} }\n"

// instantiatingTypeMain is the program gc compiles against nanogo's export
// data for [genericTypeSource].
//
// Every method is called at int and at a struct holding a pointer, which have
// different sizes and different pointer maps, so a dictionary standing in for
// the other one is a value that differs rather than one that happens to agree.
const instantiatingTypeMain = "package main\n\n" +
	"import (\n\t\"fmt\"\n\n\tlib \"nanogo.example/gentype/lib\"\n)\n\n" +
	"type item struct {\n\tp *int\n\tn int\n}\n\n" +
	"func main() {\n" +
	"\tvar ints lib.List[int]\n" +
	"\tints.Push(3)\n\tints.Push(5)\n\tints.Push(7)\n" +
	"\tfmt.Println(ints.Len(), ints.At(1), ints.All(), ints.Any(2), ints.Last())\n" +
	"\tn := 4\n" +
	"\tvar items lib.List[item]\n" +
	"\titems.Push(item{nil, 1})\n\titems.Push(item{&n, 2})\n" +
	"\tfmt.Println(items.Len(), items.At(0).n, *items.At(1).p, items.Last().n, items.Any(0))\n" +
	"\tvar bi lib.Box[int]\n\tbi.Set(11)\n" +
	"\tvar bs lib.Box[item]\n\tbs.Set(item{&n, 6})\n" +
	"\tfmt.Println(bi.Get(), bs.Get().n, *bs.Get().p)\n" +
	"\tvar pr lib.Pair[int, string]\n\tpr.Set(9, \"nine\")\n" +
	"\tfmt.Println(pr.Key(), pr.Val(), pr.Swap().Key(), pr.Swap().Val())\n" +
	"\tvar pi lib.Pair[item, int]\n\tpi.Set(item{&n, 5}, 6)\n" +
	"\tfmt.Println(pi.Key().n, *pi.Key().p, pi.Val(), pi.Swap().Val().n)\n" +
	"}\n"

// TestGcInstantiatesAGenericTypeNanogoWrote is the oracle for a method of a
// generic type.
//
// The method is written inside the type's element and its body is numbered
// against the dictionary the type shares with every method it declares
// (specs/013-generics.md). A slot numbered against any other dictionary is a
// type gc resolves to whatever that slot holds, and gc reads it without
// complaint, so the program running and printing the right values is the
// check and a diagnostic is not.
func TestGcInstantiatesAGenericTypeNanogoWrote(t *testing.T) {
	gcRunsAgainstNanogo(t, "nanogo.example/gentype", genericTypeSource, instantiatingTypeMain,
		"3 5 [3 5 7] 7 7\n2 1 4 2 {<nil> 1}\n11 6 4\n9 nine nine 9\n5 4 6 5\n")
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
