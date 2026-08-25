// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// surface is the exported surface of a package, as a list of lines.
//
// It is what the round trip compares. A declaration's String is the form the
// checker prints in a diagnostic, so a field, a tag, a variadic parameter or
// an embedded interface that did not survive the write shows up in it. A
// named type also contributes its underlying type and its methods in
// declaration order, because neither is part of the TypeName's own string.
func surface(pkg *types2.Package) []string {
	var lines []string
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if obj == nil || !obj.Exported() {
			continue
		}
		lines = append(lines, obj.String())
		if c, ok := obj.(*types2.Const); ok {
			lines = append(lines, "\tvalue "+c.Val().String())
		}
		tn, ok := obj.(*types2.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types2.Named)
		if !ok {
			continue
		}
		lines = append(lines, "\tunderlying "+named.Underlying().String())
		for i := range named.NumMethods() {
			lines = append(lines, "\tmethod "+named.Method(i).String())
		}
	}
	return lines
}

// roundTrip writes pkg's export data, reads it back and returns the package
// the reader reconstructed.
func roundTrip(t *testing.T, pkg *types2.Package) (*types2.Package, [8]byte) {
	t.Helper()
	payload, fp, err := Write(pkg)
	if err != nil {
		t.Fatalf("Write(%s): %v", pkg.Path(), err)
	}
	dec := pkgbits.NewPkgDecoder(pkg.Path(), string(payload))
	checkStubs(t, pkg.Path(), &dec)
	if dec.Fingerprint() != fp {
		t.Errorf("%s: Write returned fingerprint %x and the payload carries %x", pkg.Path(), fp, dec.Fingerprint())
	}
	got := ReadPackage(types2.NewContext(), map[string]*types2.Package{}, dec)
	if got == nil {
		t.Fatalf("%s: reading back what was written produced no package", pkg.Path())
	}
	return got, fp
}

// checkStubs pins the one invariant a file's export data has to satisfy: the
// only declaration without a definition is one from the universe or from
// unsafe.
//
// gc writes a stub for every declaration of another package and its linker
// resolves each one by copying the definition in. What reaches a file has no
// other stub left, and every reader of the format asserts it: gc reports a
// stub it cannot resolve as an internal compiler error, naming a declaration
// that is nowhere near the package that wrote it.
func checkStubs(t *testing.T, path string, dec *pkgbits.PkgDecoder) {
	t.Helper()
	for i := range dec.NumElems(pkgbits.SectionName) {
		pkgPath, name, tag := dec.PeekObj(pkgbits.Index(i))
		if tag != pkgbits.ObjStub {
			continue
		}
		if pkgPath != "builtin" && pkgPath != "unsafe" {
			t.Errorf("%s: object %d, %s.%s, is a stub with no definition", path, i, pkgPath, name)
		}
	}
}

// compareSurface reports every declaration that did not survive the round
// trip, and returns the number of lines compared.
func compareSurface(t *testing.T, path string, want, got *types2.Package) int {
	t.Helper()
	a, b := surface(want), surface(got)
	// The empty interface prints two ways and they are one type. gc's
	// checker builds a distinct Interface for a source "interface{}" and
	// shares one for "any", and the reader cannot tell them apart:
	// types2.NewInterfaceType returns the one canonical empty interface for
	// both, so a package that came back through the reader has already lost
	// the spelling. Comparing the spelling would report a reader property as
	// a writer failure.
	for _, lines := range [][]string{a, b} {
		for i, line := range lines {
			lines[i] = strings.ReplaceAll(line, "interface{}", "any")
		}
	}
	if len(a) != len(b) {
		t.Errorf("%s: the surface has %d lines and the round trip produced %d\nwant:\n%s\ngot:\n%s",
			path, len(a), len(b), strings.Join(a, "\n"), strings.Join(b, "\n"))
		return 0
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("%s: line %d is\n\t%s\nwant\n\t%s", path, i, b[i], a[i])
		}
	}
	return len(a)
}

// TestWriteRoundTripsTheFixture is the writer's agreement test.
//
// The fixture is the reader's, minus what the writer refuses: it carries
// every type tag, every constant encoding, an interface with a union, a self
// referential type and an alias.
func TestWriteRoundTripsTheFixture(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/wfixture\n\ngo 1.27\n")
	write("wfixture.go", writerFixture)
	file := exportFile(t, dir, ".")

	r := NewReader()
	pkg, err := r.Read("nanogo.example/wfixture", file)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	resolve(t, pkg)

	got, _ := roundTrip(t, pkg)
	if got.Name() != pkg.Name() {
		t.Errorf("the package name came back as %q, want %q", got.Name(), pkg.Name())
	}
	if !got.Complete() {
		t.Error("the package that was read back is not marked complete")
	}
	if n := compareSurface(t, pkg.Path(), pkg, got); n == 0 {
		t.Fatal("nothing was compared")
	}
}

// writerFixture is the reader's fixture with the generic declarations taken
// out, because the writer refuses one by name.
const writerFixture = `package wfixture

import "fmt"

const (
	B        = true
	Str      = "text"
	I64      = 42
	Big      = 1 << 100
	NegBig   = -(1 << 100)
	Rat      = 1.5
	Tiny     = 1e-1000
	Denormal = 1e-2000
	Cplx     = 3 + 4i
)

type (
	Point struct {
		X, Y   int
		hidden string ` + "`json:\"h\"`" + `
	}
	Rec    struct{ next *Rec }
	Arr    [3]byte
	Sl     []Point
	Ch     chan int
	RecvCh <-chan int
	SendCh chan<- int
	M      map[string]Arr
	Fn     func(a int, rest ...string) (int, error)
	Alias  = Point
	Shape  interface {
		Area() float64
		fmt.Stringer
	}
	Empty  interface{}
	Any    = any
	Hidden = hidden
)

type hidden struct{ n int }

func (p Point) Sum() int       { return p.X + p.Y }
func (p *Point) Set(x int)     { p.X = x }
func (p Point) String() string { return "point" }

func Take(s Shape, e error, f Fn) (Point, error) { return Point{}, nil }

var V Point
var E error
`

// TestWriteRefusesAGeneric names the one thing the writer will not encode.
//
// A generic declaration an importer instantiates needs the function body
// specs/015-export-data.md's body reader would carry, and nanogo writes
// declarations only. Writing the declaration and no body would let a build
// get as far as gc trying to stencil it, which fails with no mention of the
// generic at all.
func TestWriteRefusesAGeneric(t *testing.T) {
	dir := fixtureModule(t)
	file := exportFile(t, dir, ".")
	r := NewReader()
	pkg, err := r.Read("nanogo.example/fixture", file)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	resolve(t, pkg)

	_, _, err = Write(pkg)
	if err == nil {
		t.Fatal("the fixture's generic declarations were written")
	}
	u, ok := err.(*UnsupportedError)
	if !ok {
		t.Fatalf("Write returned %T: %v", err, err)
	}
	if u.Package != "nanogo.example/fixture" {
		t.Errorf("the refusal names package %q", u.Package)
	}
	// Whichever generic declaration the walk reaches first, the refusal has
	// to name it. Point.Map is a method with type parameters of its own, so
	// it is reached through Point and not through the package scope.
	switch {
	case strings.HasSuffix(u.Name, ".List"), strings.HasSuffix(u.Name, ".Min"),
		strings.HasSuffix(u.Name, ".GenAlias"), strings.HasSuffix(u.Name, ".Map"),
		strings.HasSuffix(u.Name, ".Zip"), strings.HasSuffix(u.Name, ".Push"),
		strings.HasSuffix(u.Name, ".All"):
	default:
		t.Errorf("the refusal names %q, which is none of the fixture's generic declarations", u.Name)
	}
	if !strings.Contains(u.Error(), "generic") {
		t.Errorf("the refusal does not say what is wrong with it: %s", u.Error())
	}
}

// TestWriteIsDeterministic is specs/053-determinism.md applied to the export
// data, which is part of a compiled package's bytes.
//
// Two writes in one process must agree, and the same package written from two
// separate reads must agree too: the second is what catches a walk whose order
// comes from a map the type checker filled.
func TestWriteIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/det\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := exportFile(t, dir, "unicode/utf8")

	read := func() *types2.Package {
		r := NewReader()
		pkg, err := r.Read("unicode/utf8", file)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		resolve(t, pkg)
		return pkg
	}

	pkg := read()
	first, fp1, err := Write(pkg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	second, fp2, err := Write(pkg)
	if err != nil {
		t.Fatalf("Write again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two writes of one package differ")
	}
	if fp1 != fp2 {
		t.Errorf("two writes of one package have fingerprints %x and %x", fp1, fp2)
	}

	third, fp3, err := Write(read())
	if err != nil {
		t.Fatalf("Write after a second read: %v", err)
	}
	if !bytes.Equal(first, third) {
		t.Errorf("the package written after a second read differs: %d bytes against %d", len(third), len(first))
	}
	if fp1 != fp3 {
		t.Errorf("the fingerprint after a second read is %x, want %x", fp3, fp1)
	}
}

// TestDefinitionWrapsThePayload checks the archive member this package builds
// against the one it reads.
func TestDefinitionWrapsThePayload(t *testing.T) {
	def, err := Definition("go object darwin arm64 go1.27.0 X:\n", true, []byte("payload"))
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	got, err := unified(def)
	if err != nil {
		t.Fatalf("unified: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("the payload came back as %q", got)
	}
	if !bytes.Contains(def, []byte("\nmain\n")) {
		t.Error("the main mark is missing")
	}
	if _, err := Definition("not a header\n", false, nil); err == nil {
		t.Error("a bad toolchain header was accepted")
	}
}

// TestWriteStandardLibrary is the widest oracle the writer has.
//
// Every package of the standard library is read from the archive gc wrote,
// written again by nanogo, and read back, and the two exported surfaces are
// compared declaration by declaration. It is a self-consistency test: it says
// the two halves of nanogo agree and nothing about gc, which is what
// internal/e2e's cross-read is for.
//
// Both numbers are reported. A test that returns quietly when a package fails
// produces a numerator that can only rise, so a refusal is counted, named and
// logged, and anything that is not a refusal is a failure.
//
// Under NANOGO_REQUIRE_CORPUS it covers the whole standard library. Without
// it, it covers [stdlib], because an unattended run should not build the
// world.
func TestWriteStandardLibrary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/wstd\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := stdlib
	if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
		want = []string{"std"}
	}

	list := archives(t, dir, want...)
	if len(list) == 0 {
		t.Fatal("no archive was found, so the test proved nothing")
	}

	// One Reader for the whole run, so that a package reached through two
	// archives is one package, exactly as the driver uses it.
	r := NewReader()
	total, written, decls := len(list), 0, 0
	var refused []string
	for _, a := range list {
		path, file := a[0], a[1]
		pkg, err := r.Read(path, file)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		resolve(t, pkg)

		payload, _, err := Write(pkg)
		if err != nil {
			if u, ok := err.(*UnsupportedError); ok {
				refused = append(refused, u.Package+": "+u.Name)
				continue
			}
			t.Errorf("%s: Write: %v", path, err)
			continue
		}
		dec := pkgbits.NewPkgDecoder(path, string(payload))
		checkStubs(t, path, &dec)
		back := ReadPackage(types2.NewContext(), map[string]*types2.Package{}, dec)
		if back == nil {
			t.Errorf("%s: reading back what was written produced no package", path)
			continue
		}
		decls += compareSurface(t, path, pkg, back)
		written++
	}

	t.Logf("round tripped %d of %d standard library packages and %d declarations; %d were refused",
		written, total, decls, len(refused))
	for _, name := range refused {
		t.Logf("refused %s", name)
	}
	if written == 0 {
		t.Fatal("no package round tripped")
	}
}

// digestEnv names the archive a subprocess of [TestWriteIsDeterministic]
// should read, write and report a digest for.
const digestEnv = "NANOGO_EXPORT_DIGEST_ARCHIVE"

// TestWriteDigestHelper is the subprocess half of the two-process
// determinism check. It is a no-op unless the parent asked for it.
func TestWriteDigestHelper(t *testing.T) {
	file := os.Getenv(digestEnv)
	if file == "" {
		t.Skip("not the subprocess of TestWriteIsDeterministicAcrossProcesses")
	}
	r := NewReader()
	pkg, err := r.Read("net/url", file)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	resolve(t, pkg)
	payload, _, err := Write(pkg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	fmt.Printf("DIGEST %x\n", sha256.Sum256(payload))
}

// TestWriteIsDeterministicAcrossProcesses is the half of
// specs/053-determinism.md that one process cannot check.
//
// A walk whose order comes from a pointer value, or from anything else the
// runtime chooses per process, agrees with itself for as long as the process
// lives. Two processes over the same archive are what catches it, and it is
// the shape the fixed point of G1 needs: nanogo compiling nanogo is many
// processes.
//
// net/url is the package, because it has interfaces, a self-referential type,
// maps and a type with methods, and it declares nothing generic.
func TestWriteIsDeterministicAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/det2\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := exportFile(t, dir, "net/url")

	digest := func() string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestWriteDigestHelper$", "-test.v")
		cmd.Env = append(os.Environ(), digestEnv+"="+file)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the subprocess failed: %v\n%s", err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if s, ok := strings.CutPrefix(strings.TrimSpace(line), "DIGEST "); ok {
				return s
			}
		}
		t.Fatalf("the subprocess reported no digest:\n%s", out)
		return ""
	}

	first, second := digest(), digest()
	if first != second {
		t.Errorf("two processes wrote different export data for net/url: %s and %s", first, second)
	}
}
