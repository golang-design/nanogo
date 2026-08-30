// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/types2"
)

// fixture is the package the reader is measured against.
//
// It is one file rather than a set of small ones because the reader decodes a
// whole archive, so every construct that shares an archive is read by the same
// pass. What is here is chosen by what the format encodes differently, not by
// what Go source looks like: every type tag, every constant encoding, a
// generic type with a method, an alias with type parameters, an interface with
// a union, and a type that refers to itself.
const fixture = `package fixture

import "fmt"

// Every constant encoding the format has.
const (
	B     = true
	Str   = "text"
	I64   = 42
	Big   = 1 << 100
	NegBig = -(1 << 100)
	Rat   = 1.5
	Tiny  = 1e-1000
	Denormal = 1e-2000
	Cplx  = 3 + 4i
)

// Every type tag.
type (
	Point struct {
		X, Y   int
		hidden string ` + "`json:\"h\"`" + `
	}
	Rec     struct{ next *Rec }
	Arr     [3]byte
	Sl      []Point
	Ch      chan int
	RecvCh  <-chan int
	SendCh  chan<- int
	M       map[string]Arr
	Fn      func(a int, rest ...string) (int, error)
	Alias   = Point
	Shape   interface {
		Area() float64
		fmt.Stringer
	}
	Num interface{ ~int | ~float64 }
)

func (p Point) Sum() int     { return p.X + p.Y }
func (p *Point) Set(x int)   { p.X = x }
func (p Point) String() string { return "point" }

// A method with type parameters of its own, which the format encodes as a
// standalone function object rather than inline with its type.
func (p Point) Map[U any](f func(int) U) U { return f(p.X) }

// A generic type with a method, which the format encodes as a standalone
// function object.
type List[T any] struct{ items []T }

func (l *List[T]) Push(v T) { l.items = append(l.items, v) }
func (l *List[T]) All() []T { return l.items }

// A generic method on a generic type, so the receiver has type parameters and
// the method has its own.
func (l *List[T]) Zip[U any](u U) (T, U) {
	var zero T
	if len(l.items) > 0 {
		zero = l.items[0]
	}
	return zero, u
}

func Min[T Num](a, b T) T {
	if a < b {
		return a
	}
	return b
}

// A generic alias, which carries type parameter names of its own.
type GenAlias[T any] = List[T]

var V Point
`

// goTool finds the go command, which the tests need to make gc write the
// export data they read.
func goTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go command: %v", err)
	}
	return p
}

// env keeps the go command off the network and away from the developer's
// settings, so that what the test builds is what it describes.
func env() []string {
	return append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=", "GO111MODULE=on", "GOPROXY=off")
}

// exportFile builds a package with gc and returns the archive that holds its
// export data.
//
// It goes through `go list -export` rather than `go tool compile`, because the
// go command is what writes the -importcfg a package with imports needs, and
// the fixture has one.
func exportFile(t *testing.T, dir, pkg string, gcflags ...string) string {
	t.Helper()
	args := []string{"list", "-export"}
	args = append(args, gcflags...)
	args = append(args, "-f", "{{.Export}}", pkg)
	cmd := exec.Command(goTool(t), args...)
	cmd.Dir = dir
	cmd.Env = env()
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Skipf("go list -export %s: %v\n%s", pkg, err, stderr)
	}
	file := strings.TrimSpace(string(out))
	if file == "" {
		t.Fatalf("go list -export %s reported no archive", pkg)
	}
	return file
}

// fixtureModule writes the fixture into a module of its own and returns its
// directory.
func fixtureModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/fixture\n\ngo 1.27\n")
	write("fixture.go", fixture)
	return dir
}

// resolve forces every declaration in the package, which is what the type
// checker does as it uses them. The reader is lazy, so a package that reads
// without error can still hold a declaration it cannot decode.
func resolve(t *testing.T, pkg *types2.Package) int {
	t.Helper()
	names := pkg.Scope().Names()
	for _, name := range names {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Errorf("%s: %s resolved to nothing", pkg.Path(), name)
			continue
		}
		// Underlying forces the lazily loaded body of a named type, and
		// String forces every type it mentions.
		_ = obj.Type().Underlying()
		_ = obj.String()
	}
	return len(names)
}

// TestReadFixture is the reader's agreement test.
//
// Each row names a declaration and the type the checker must reconstruct for
// it. The string form is the assertion because it is what the checker prints
// in a diagnostic, so a field, a tag, a variadic parameter or a type parameter
// that decoded wrongly shows up in it.
func TestReadFixture(t *testing.T) {
	dir := fixtureModule(t)
	file := exportFile(t, dir, ".")

	r := NewReader()
	pkg, err := r.Read("nanogo.example/fixture", file)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := pkg.Name(); got != "fixture" {
		t.Errorf("package name is %q, want %q", got, "fixture")
	}
	if !pkg.Complete() {
		t.Error("the package is not marked complete")
	}
	resolve(t, pkg)

	tests := []struct {
		name string
		want string
	}{
		{"Point", "type nanogo.example/fixture.Point struct{X int; Y int; hidden string \"json:\\\"h\\\"\"}"},
		{"Rec", "type nanogo.example/fixture.Rec struct{next *nanogo.example/fixture.Rec}"},
		{"Arr", "type nanogo.example/fixture.Arr [3]byte"},
		{"Sl", "type nanogo.example/fixture.Sl []nanogo.example/fixture.Point"},
		{"Ch", "type nanogo.example/fixture.Ch chan int"},
		{"RecvCh", "type nanogo.example/fixture.RecvCh <-chan int"},
		{"SendCh", "type nanogo.example/fixture.SendCh chan<- int"},
		{"M", "type nanogo.example/fixture.M map[string]nanogo.example/fixture.Arr"},
		{"Fn", "type nanogo.example/fixture.Fn func(a int, rest ...string) (int, error)"},
		{"Alias", "type nanogo.example/fixture.Alias = nanogo.example/fixture.Point"},
		{"Shape", "type nanogo.example/fixture.Shape interface{Area() float64; fmt.Stringer}"},
		{"Num", "type nanogo.example/fixture.Num interface{~int | ~float64}"},
		{"List", "type nanogo.example/fixture.List[T any] struct{items []T}"},
		{"GenAlias", "type nanogo.example/fixture.GenAlias[T any] = nanogo.example/fixture.List[T]"},
		{"Min", "func nanogo.example/fixture.Min[T nanogo.example/fixture.Num](a T, b T) T"},
		{"V", "var nanogo.example/fixture.V nanogo.example/fixture.Point"},
	}
	for _, tt := range tests {
		obj := pkg.Scope().Lookup(tt.name)
		if obj == nil {
			t.Errorf("%s is not declared", tt.name)
			continue
		}
		if got := obj.String(); got != tt.want {
			t.Errorf("%s is\n\t%s\nwant\n\t%s", tt.name, got, tt.want)
		}
	}

	// The constants, by value rather than by declaration, because the value
	// is what the format encodes six different ways and an Object prints only
	// its type.
	consts := []struct {
		name string
		want string
	}{
		{"B", "true"},
		{"Str", `"text"`},
		{"I64", "42"},
		{"Big", "1267650600228229401496703205376"},
		{"NegBig", "-1267650600228229401496703205376"},
		{"Rat", "1.5"},
		{"Tiny", "1e-1000"},
		{"Denormal", "1e-2000"},
		{"Cplx", "(3 + 4i)"},
	}
	for _, tt := range consts {
		obj, _ := pkg.Scope().Lookup(tt.name).(*types2.Const)
		if obj == nil {
			t.Errorf("%s is not a constant", tt.name)
			continue
		}
		if got := obj.Val().String(); got != tt.want {
			t.Errorf("%s = %s, want %s", tt.name, got, tt.want)
		}
	}

	// The methods of a named type and of a generic named type. A method is
	// not in the package scope, so it is reached through the type.
	point, _ := pkg.Scope().Lookup("Point").Type().(*types2.Named)
	if point == nil {
		t.Fatal("Point is not a named type")
	}
	// The non-generic methods come first because the format encodes them
	// inline with the type and appends the generic ones as references.
	if got := methodNames(point); got != "Sum,Set,String,Map" {
		t.Errorf("Point has methods %s, want Sum,Set,String,Map", got)
	}
	if got := methodString(point, "Map"); got != "func (nanogo.example/fixture.Point).Map[U any](f func(int) U) U" {
		t.Errorf("Point.Map is %s", got)
	}
	list, _ := pkg.Scope().Lookup("List").Type().(*types2.Named)
	if list == nil {
		t.Fatal("List is not a named type")
	}
	if got := methodNames(list); got != "Push,All,Zip" {
		t.Errorf("List has methods %s, want Push,All,Zip", got)
	}
	if got := methodString(list, "Zip"); got != "func (*nanogo.example/fixture.List[T]).Zip[U any](u U) (T, U)" {
		t.Errorf("List.Zip is %s", got)
	}

	// Every read package has a file and a fingerprint recorded for it,
	// because the object nanogo writes carries them.
	imports := r.Imports()
	if len(imports) != 1 || imports[0].Path != "nanogo.example/fixture" || imports[0].File != file {
		t.Fatalf("Imports() = %+v", imports)
	}
	if imports[0].Fingerprint == ([8]byte{}) {
		t.Error("the fingerprint is zero")
	}
}

// methodNames lists a named type's methods in the order the reader added them.
//
// gc writes a type's methods in declaration order, so the order is part of
// what the reader has to preserve. Sorting here would hide a reader that lost
// it, and the comparison is against declaration order for that reason.
func methodNames(n *types2.Named) string {
	names := make([]string, 0, n.NumMethods())
	for i := range n.NumMethods() {
		names = append(names, n.Method(i).Name())
	}
	return strings.Join(names, ",")
}

// methodString returns the signature of one method, so that a generic method
// is checked by what it declares and not only by its name.
func methodString(n *types2.Named, name string) string {
	for i := range n.NumMethods() {
		if m := n.Method(i); m.Name() == name {
			return m.String()
		}
	}
	return ""
}

// syncFixture is a package with no import, for the sync marker test.
//
// It has no import because gc's sync-marked export data desyncs at the first
// object that stands in for a declaration of another package. That is not a
// defect in this port: go/internal/gcimporter, which is the same reader
// against go/types, fails on the same archive at the same offset. Everything
// that is not a cross-package stub reads, so the fixture stays inside one
// package and the marker path is still exercised.
const syncFixture = `package syncfixture

const Big = 1 << 100

type Point struct{ X, Y int }

func (p Point) Sum() int { return p.X + p.Y }

type List[T any] struct{ items []T }

func (l *List[T]) Push(v T) { l.items = append(l.items, v) }

type Alias = Point

func Min[T int | float64](a, b T) T {
	if a < b {
		return a
	}
	return b
}
`

// TestReadSyncMarkedFixture reads export data that carries sync markers.
//
// gc writes a marker before every field when it is built with -d=syncframes,
// and a reader that skipped one would desync on it. Ordinary export data has
// no markers, so this is the only way that path is exercised at all.
func TestReadSyncMarkedFixture(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/syncfixture\n\ngo 1.27\n")
	write("syncfixture.go", syncFixture)
	file := exportFile(t, dir, ".", "-gcflags=-d=syncframes=0")

	r := NewReader()
	pkg, err := r.Read("nanogo.example/syncfixture", file)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n := resolve(t, pkg); n == 0 {
		t.Fatal("the package declares nothing")
	}
	want := "func nanogo.example/syncfixture.Min[T int | float64](a T, b T) T"
	if got := pkg.Scope().Lookup("Min").String(); got != want {
		t.Errorf("Min is\n\t%s\nwant\n\t%s", got, want)
	}
}

// stdlib is the standard library the reader is checked against by default.
//
// The list is not a sample. Each entry is here for a construct: errors for the
// smallest useful package, strconv and math/bits for the constants a compiled
// body can actually use, sort for an interface, slices and iter for generics
// and for a generic alias, net/http and go/types for size, sync for a type
// with unexported fields, and runtime because every build links it.
var stdlib = []string{
	"errors", "strconv", "math", "math/bits", "sort", "strings", "bytes",
	"io", "bufio", "fmt", "os", "sync", "time", "unicode/utf8",
	"slices", "maps", "iter", "cmp", "net/http", "go/types", "runtime",
}

// archives builds the named packages with gc and returns each one's import
// path with the archive that holds its export data.
//
// One invocation for the whole list, because the go command builds the
// transitive closure once and a call per package would build it again per
// package.
func archives(t *testing.T, dir string, paths ...string) [][2]string {
	t.Helper()
	args := append([]string{"list", "-export", "-f", "{{.ImportPath}}\t{{.Export}}"}, paths...)
	cmd := exec.Command(goTool(t), args...)
	cmd.Dir = dir
	cmd.Env = env()
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Skipf("go list -export: %v\n%s", err, stderr)
	}
	var list [][2]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, file, ok := strings.Cut(line, "\t")
		// A package with no archive is one the go command has nothing to
		// build, such as unsafe. There is nothing for the reader to read.
		if !ok || file == "" {
			continue
		}
		list = append(list, [2]string{path, file})
	}
	return list
}

// TestReadStandardLibrary is the strongest evidence the reader is correct.
//
// The archives are gc's own, written by the toolchain the developer is
// running, and every declaration in them is forced. Reading a package whose
// export data nanogo did not write is the whole point of the component.
//
// Under NANOGO_REQUIRE_CORPUS it reads the whole standard library and names
// every package it cannot read. Without it, it reads [stdlib], because an
// unattended run should not build the world.
func TestReadStandardLibrary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/std\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := stdlib
	if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
		want = []string{"std"}
	}
	// One Reader for the whole run, which is how the driver uses it: a
	// package reached through two archives must be one package.
	r := NewReader()
	total, read := 0, 0
	for _, a := range archives(t, dir, want...) {
		path, file := a[0], a[1]
		pkg, err := r.Read(path, file)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if pkg.Path() != path {
			t.Errorf("%s: the export data says the path is %q", path, pkg.Path())
		}
		total += resolve(t, pkg)
		read++
	}
	if read == 0 {
		t.Fatal("no archive was read, so the test proved nothing")
	}
	t.Logf("read %d standard library packages and %d declarations", read, total)
}

// TestPackagesAreSharedAcrossArchives is the reason the Reader outlives one
// import.
//
// gc writes the declarations an archive depends on into that archive, so
// reading bufio materialises io as a side effect. A second Reader would build
// a second io, and the checker would then report io.Writer and io.Writer as
// unrelated types.
func TestPackagesAreSharedAcrossArchives(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/share\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewReader()
	bufioPkg, err := r.Read("bufio", exportFile(t, dir, "bufio"))
	if err != nil {
		t.Fatalf("bufio: %v", err)
	}
	ioPkg, err := r.Read("io", exportFile(t, dir, "io"))
	if err != nil {
		t.Fatalf("io: %v", err)
	}
	// bufio.NewWriter takes an io.Writer. Read through bufio's archive it
	// must be the very object io's own archive declares.
	fn, _ := bufioPkg.Scope().Lookup("NewWriter").(*types2.Func)
	if fn == nil {
		t.Fatal("bufio.NewWriter is not a function")
	}
	sig, _ := fn.Type().(*types2.Signature)
	param, _ := sig.Params().At(0).Type().(*types2.Named)
	if param == nil {
		t.Fatalf("bufio.NewWriter takes %s, want a named type", sig.Params().At(0).Type())
	}
	want := ioPkg.Scope().Lookup("Writer")
	if param.Obj() != want {
		t.Errorf("bufio.NewWriter takes %v, which is not the io.Writer io declares", param.Obj())
	}
	// io was read as a side effect of bufio and then again from its own
	// archive, so it appears once in the import list and once only.
	n := 0
	for _, im := range r.Imports() {
		if im.Path == "io" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("io appears %d times in Imports(), want 1", n)
	}
}

// TestReadUnsafe covers the one import with no archive.
//
// The checker asks the importer for unsafe like any other import and
// -importcfg carries no entry for it, so the answer has to come from here.
func TestReadUnsafe(t *testing.T) {
	r := NewReader()
	pkg, err := r.Read("unsafe", "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if pkg != types2.Unsafe {
		t.Errorf("Read(unsafe) = %v, want the universe's unsafe", pkg)
	}
	if len(r.Imports()) != 0 {
		t.Errorf("unsafe was recorded as an import: %+v", r.Imports())
	}
}

// TestReadIsCached checks that a package is read once.
//
// The type checker asks for the same import once per directory that names it,
// so a package imported by two files of one package is asked for twice.
func TestReadIsCached(t *testing.T) {
	dir := fixtureModule(t)
	file := exportFile(t, dir, ".")
	r := NewReader()
	first, err := r.Read("nanogo.example/fixture", file)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	second, err := r.Read("nanogo.example/fixture", file)
	if err != nil {
		t.Fatalf("Read again: %v", err)
	}
	if first != second {
		t.Error("the second read produced a different package")
	}
	if len(r.Imports()) != 1 {
		t.Errorf("Imports() = %+v, want one entry", r.Imports())
	}
}

// TestBodyIsFoundInAnArchiveThatCopiedIt is the reading half of
// specs/017-export-data-reading.md's stage 3.
//
// A package that holds an os.File owes the method set of
// sync/atomic.Pointer[os.dirInfo], because the descriptor of that
// instantiation names four methods and no other package can have compiled
// them. Its -importcfg names no archive for sync/atomic at all: the import is
// os's and not its own. The bodies are still reachable, because a generic
// declaration is copied whole into the archive of every package whose
// exported surface reaches it, so os's archive carries sync/atomic's.
//
// The declaring package is not read here, on purpose. A reader that searched
// only the declaring package's archive answers "this compilation read no
// archive for sync/atomic" and the package is refused for a body that is on
// disk.
func TestBodyIsFoundInAnArchiveThatCopiedIt(t *testing.T) {
	dir := fixtureModule(t)
	file := exportFile(t, dir, "os")

	r := NewReader()
	if _, err := r.Read("os", file); err != nil {
		t.Fatalf("Read os: %v", err)
	}
	for _, name := range []string{
		"(*Pointer).Load",
		"(*Pointer).Store",
		"(*Pointer).Swap",
		"(*Pointer).CompareAndSwap",
	} {
		b, err := r.Body("sync/atomic", name)
		if err != nil {
			t.Fatalf("Body(sync/atomic, %s): %v", name, err)
		}
		if b == nil {
			t.Errorf("os's archive carries no body for sync/atomic.%s", name)
			continue
		}
		if b.Path != "sync/atomic" || b.Name != name {
			t.Errorf("the body found for sync/atomic.%s is %s.%s", name, b.Path, b.Name)
		}
	}
	// A declaration no archive this compilation read holds is still (nil, nil)
	// rather than an error, which is what the caller reports by name.
	b, err := r.Body("sync/atomic", "(*Pointer).NoSuchMethod")
	if b != nil || err != nil {
		t.Errorf("a declaration no archive holds gave (%v, %v), want (nil, nil)", b, err)
	}
}
