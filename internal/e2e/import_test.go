// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The library gc compiles and nanogo imports.
//
// It carries one of each kind of declaration a nanogo-compiled body can reach:
// a constant, which the checker folds into the instruction stream, and a
// function, which becomes a call to a symbol another package defines. Both
// have to survive the round trip through gc's export data for the program
// below to compute the right answer.
const libraryPackage = `package lib

const Scale = 7

func Add(a, b int) int { return a + b }

func Scaled(a int) int { return Add(a, a*Scale) }
`

// The program that imports it.
//
// The exit status is the assertion, as in the other end-to-end programs: a
// wrong answer divides by zero and the process dies. Scaled(3) is 3 + 21, so
// the whole expression is zero when every value crossed the package boundary
// intact.
const importingProgram = `package main

import "nanogo.example/two/lib"

func main() {
	d := lib.Add(20, 3) - lib.Scale - 16
	if d != 0 {
		d = d / (d - d)
	}
	e := lib.Scaled(3) - 24
	if e != 0 {
		e = e / (e - e)
	}
}
`

// The program that imports the standard library.
//
// math/bits is the reachable entry point: its constants are constants and
// OnesCount64 takes and returns a machine word, so a nanogo-compiled body can
// call it. The archive comes from the go command, and nobody wrote it for this
// test.
const stdlibProgram = `package main

import (
	"math/bits"
	"strconv"
)

func main() {
	d := bits.OnesCount64(7) - 3
	if d != 0 {
		d = d / (d - d)
	}
	e := bits.UintSize - strconv.IntSize
	if e != 0 {
		e = e / (e - e)
	}
	f := bits.TrailingZeros64(8) - 3
	if f != 0 {
		f = f / (f - f)
	}
}
`

// TestToolexecCompilesAPackageWithImports is what specs/015-export-data.md
// was blocking.
//
// gc compiles the library, nanogo compiles the package that imports it, the
// real linker joins them, and the program runs. Everything the importing
// package knows about the library it learned by reading the export data gc
// wrote, so a program that computes the right answer is the statement that the
// reader is correct.
func TestToolexecCompilesAPackageWithImports(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": libraryPackage,
		"main.go":    importingProgram,
	}, []string{"# nanogo owns the importing package, gc owns the library", "main"})

	out, err := h.build(t, "-o", "two", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the importing package instead of compiling it:\n%s", strings.Join(lines, "\n"))
	}
	if compiled(lines, "nanogo.example/two/lib") {
		t.Fatalf("nanogo compiled the library, so the test proves nothing about reading gc's export data:\n%s",
			strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "two")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled did not run: %v\n%s", err, b)
	}
}

// TestToolexecCompilesAPackageThatImportsTheStandardLibrary is the same claim
// against archives the toolchain ships.
//
// A package of the test's own is written by the test and could be written to
// suit it. math/bits and strconv are not, so what the reader has to decode is
// whatever gc happened to write for them, including the declarations the
// program never mentions.
func TestToolexecCompilesAPackageThatImportsTheStandardLibrary(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/std\n\ngo 1.27\n",
		"main.go": stdlibProgram,
	}, []string{"main"})

	out, err := h.build(t, "-o", "std", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "std")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled did not run: %v\n%s", err, b)
	}
}

// TestGcImportsWhatNanogoWrote is the cross-read, and it is the test that
// matters most.
//
// nanogo compiles the library and gc compiles the package that imports it, so
// everything the importing package knows about the library it learned by
// reading the export data nanogo wrote. A round trip through nanogo's own
// reader cannot make this claim: it would agree with itself about a format
// that was wrong. gc has to read the bytes, the linker has to join the two
// objects, and the program has to compute the right answer.
func TestGcImportsWhatNanogoWrote(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": libraryPackage,
		"main.go":    importingProgram,
	}, []string{"# nanogo owns the library, gc owns the package that imports it", "nanogo.example/two/lib"})

	out, err := h.build(t, "-o", "two", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/two/lib") {
		t.Fatalf("nanogo delegated the library, so gc read gc's own export data:\n%s", strings.Join(lines, "\n"))
	}
	if compiled(lines, "main") {
		t.Fatalf("nanogo compiled the importing package, so nothing read what nanogo wrote:\n%s",
			strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "two")).CombinedOutput(); err != nil {
		t.Fatalf("the program gc compiled against nanogo's export data did not run: %v\n%s", err, b)
	}
}

// dataPackage is the cheapest possible package for the writer.
//
// It declares constants and type aliases and no function at all, so its
// compiled form holds no symbol and its archive is the export data and
// nothing else. internal/goarch and internal/goos are this shape, which is
// why nanogo refused them until the writer existed: the refusal was about the
// missing export data and not about code generation.
const dataPackage = `package data

const (
	Width  = 8
	Name   = "data"
	Ratio  = 3.5
	Mask   = 1 << 62
)

// An alias and not a defined type. A defined type is refused, because an
// importer needs a runtime type descriptor for it that nanogo cannot write;
// see TestNanogoRefusesATypeAnImporterCouldNotLinkAgainst.
type Word = int
`

// dataImporter is the program that reads it.
const dataImporter = `package main

import "nanogo.example/data/data"

func main() {
	var w data.Word = data.Width
	d := w + 2 - 10
	if d != 0 {
		d = d / (d - d)
	}
	e := data.Mask >> 62
	if e != 1 {
		e = e / (e - e)
	}
	if len(data.Name) != 4 {
		panic("name")
	}
}
`

// TestGcImportsAPackageWithNoFunctions is the writer's narrowest case.
//
// The library compiles to no symbol, so what gc reads back is the export data
// and only the export data: the container, the constant encodings, an alias
// and a struct. A failure here is in the format and cannot be in code
// generation, because nanogo generated none.
func TestGcImportsAPackageWithNoFunctions(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":       "module nanogo.example/data\n\ngo 1.27\n",
		"data/data.go": dataPackage,
		"main.go":      dataImporter,
	}, []string{"nanogo.example/data/data"})

	out, err := h.build(t, "-o", "dataprog", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/data/data") {
		t.Fatalf("nanogo delegated the package with no functions:\n%s", strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "dataprog")).CombinedOutput(); err != nil {
		t.Fatalf("the program did not run: %v\n%s", err, b)
	}
}

// TestNanogoImportsWhatNanogoWrote closes the loop.
//
// Both packages are nanogo's, so nanogo reads its own export data. It is the
// weakest of the three claims and the one an allowlist that grows towards the
// leaves depends on: a build in which nanogo owns two packages in a row.
func TestNanogoImportsWhatNanogoWrote(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/two\n\ngo 1.27\n",
		"lib/lib.go": libraryPackage,
		"main.go":    importingProgram,
	}, []string{"main", "nanogo.example/two/lib"})

	out, err := h.build(t, "-o", "two", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	for _, pkg := range []string{"main", "nanogo.example/two/lib"} {
		if !compiled(lines, pkg) {
			t.Fatalf("nanogo delegated %s:\n%s", pkg, strings.Join(lines, "\n"))
		}
	}

	if b, err := exec.Command(filepath.Join(h.mod, "two")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled from end to end did not run: %v\n%s", err, b)
	}
}

// TestNanogoCompilesAStandardLibraryPackage is the keystone claim.
//
// internal/goos is in the closure of every Go program: it is constants and
// nothing else, it compiles to no symbol at all, and the runtime imports it.
// With it on the allowlist, nanogo compiles it and gc compiles the other
// twenty-eight packages of the build against the export data nanogo wrote,
// runtime included. The program then runs.
//
// internal/goarch is here too, and it is the one that says the type descriptor
// writer works against the real runtime. It declares ArchFamilyType, a defined
// type over int, so the object carries type:internal/goarch.ArchFamilyType with
// the UncommonType that names the package and with the abi kind Int rather than
// Int64. cmd/link resolves the runtime's own reference to that symbol, and the
// DWARF pass builds go:info.internal/goarch.ArchFamilyType out of it. Until the
// method set crossed the type boundary the package was refused, which is what
// this line used to say.
//
// This is what specs/015-export-data.md's missing writer was blocking, and
// these two packages are where it was most visible: the driver refused them
// with a message about having no function bodies, which said the export data
// was missing rather than that code generation could not reach them. Until
// the writer existed, nanogo could compile only a package that nothing
// imports, which in this closure is main and nothing else.
func TestNanogoCompilesAStandardLibraryPackage(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/goarch\n\ngo 1.27\n",
		"main.go": stdlibProgram,
	}, []string{"internal/goos", "internal/goarch"})

	out, err := h.build(t, "-o", "prog", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	for _, pkg := range []string{"internal/goos", "internal/goarch"} {
		if !compiled(lines, pkg) {
			t.Fatalf("nanogo delegated %s:\n%s", pkg, strings.Join(lines, "\n"))
		}
	}
	if compiled(lines, "runtime") {
		t.Fatalf("nanogo compiled the runtime, so the test says nothing about importing:\n%s",
			strings.Join(lines, "\n"))
	}

	if b, err := exec.Command(filepath.Join(h.mod, "prog")).CombinedOutput(); err != nil {
		t.Fatalf("the program did not run against a standard library package nanogo compiled: %v\n%s", err, b)
	}
}

// initLibrary is a package whose initialisation is observable.
//
// It has no package-level variable, because nanogo has no data symbol writer
// and refuses one. What is left is an init function with an effect, and the
// only effect a package with no state can have is to end the process. So the
// library's initialisation divides by zero, and the assertion is inverted:
// the program must fail.
const initLibrary = `package initlib

func boom(a, b int) int { return a / b }

func init() {
	zero := 0
	_ = boom(1, zero)
}
`

// initImporter blank-imports it and does nothing else.
const initImporter = `package main

import _ "nanogo.example/initrun/initlib"

func main() {}
`

// TestGcOrdersInitAfterAPackageNanogoCompiled is the private root's one bit,
// end to end.
//
// nanogo writes an initialisation record for the library and says so in the
// export data. gc compiles the importing package, reads that bit, and orders
// its own record after the library's, so cmd/link's walk reaches the
// library's init and the runtime runs it. A bit written wrongly is silent:
// the program would link and run and the library's initialisation would never
// happen, which is why the library's init is the thing that ends the process.
func TestGcOrdersInitAfterAPackageNanogoCompiled(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":             "module nanogo.example/initrun\n\ngo 1.27\n",
		"initlib/initlib.go": initLibrary,
		"main.go":            initImporter,
	}, []string{"nanogo.example/initrun/initlib"})

	out, err := h.build(t, "-o", "initrun", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/initrun/initlib") {
		t.Fatalf("nanogo delegated the library:\n%s", strings.Join(lines, "\n"))
	}

	b, err := exec.Command(filepath.Join(h.mod, "initrun")).CombinedOutput()
	if err == nil {
		t.Fatalf("the program exited zero, so the library's initialisation never ran:\n%s", b)
	}
	// The message and not only the name. A name in a traceback is not
	// evidence that the traceback is right: the runtime resolves the name of
	// the function that panicked from its program counter and prints it
	// before it fails on the frame above, so "initlib.init" appears in the
	// broken output as well. Until ssagen padded the return address of a
	// panic call, this test read
	//
	//	runtime: g 1: unexpected return pc for initlib.init.0
	//
	// and passed.
	got := string(b)
	if !strings.Contains(got, "integer divide by zero") {
		t.Errorf("the program failed for some other reason than the library's init:\n%s", got)
	}
	for _, want := range []string{"initlib.boom()", "initlib.init.0()", "initlib.init()"} {
		if !strings.Contains(got, want) {
			t.Errorf("the traceback does not name %q:\n%s", want, got)
		}
	}
}

// linkShape is one library shape and an importer that uses it.
//
// The importer is gc's and the library is nanogo's, and the assertion is the
// process exit status: every program below exits 42 when every value crossed
// the boundary intact.
type linkShape struct {
	name string
	lib  string
	main string
}

// linkingShapes are the library shapes gc can compile against, link and run.
//
// Each one is here for a part of the export data that only the linker can
// check. The cross-read test in export/ compiles an importer and stops there,
// which is one step short of the consumer: a relocation is emitted at compile
// time and resolved at link time, so a symbol the library owes and does not
// write is invisible until the link.
var linkingShapes = []linkShape{
	{
		name: "a constant and a function",
		lib:  "const S = 21\n\nfunc Double(x int) int { return x * 2 }\n",
		main: "\tos.Exit(lib.Double(lib.S))\n",
	},
	{
		name: "a function with several parameters and results",
		lib:  "func Split(x int) (int, int) { return x / 2, x - x/2 }\n",
		main: "\ta, b := lib.Split(42)\n\tos.Exit(a + b)\n",
	},
	{
		name: "a variadic function",
		lib:  "func Sum(xs ...int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal = total + x\n\t}\n\treturn total\n}\n",
		main: "\tos.Exit(lib.Sum(20, 20, 2))\n",
	},
	{
		name: "a type alias, which declares no type",
		lib:  "type Word = int\n\nfunc Widen(w Word) Word { return w }\n",
		main: "\tvar w lib.Word = 42\n\tos.Exit(int(lib.Widen(w)))\n",
	},
	{
		name: "a function that calls another in the same package",
		lib:  "func inner(x int) int { return x + 2 }\n\nfunc Outer(x int) int { return inner(x) }\n",
		main: "\tos.Exit(lib.Outer(40))\n",
	},
	{
		name: "a package whose only content is constants",
		lib:  "const (\n\tA = 40\n\tB = 2\n)\n",
		main: "\tos.Exit(lib.A + lib.B)\n",
	},
	// The four shapes below were in refusedShapes until the type boundary
	// carried a method set. Each declares a type, and each importer puts that
	// type on a *local variable*, which is the shape that made the link fail:
	// gc emits a DWARF reference for such a variable and cmd/link builds the
	// entry out of the type descriptor, so the library owes
	// type:<path>.<Type> whether the importer's code reads it or not.
	{
		name: "a struct",
		lib:  "type Point struct{ X, Y int }\n",
		main: "\tvar p lib.Point\n\tp.X, p.Y = 40, 2\n\tos.Exit(p.X + p.Y)\n",
	},
	{
		name: "a struct with a pointer field",
		lib:  "type Node struct {\n\tN *Node\n\tV int\n}\n",
		main: "\tvar n lib.Node\n\tn.V = 42\n\tn.N = &n\n\tos.Exit(n.N.V)\n",
	},
	{
		name: "a named slice",
		lib:  "type List []int\n",
		main: "\tvar l lib.List = []int{20, 22}\n\tos.Exit(l[0] + l[1])\n",
	},
	{
		name: "a named basic type",
		lib:  "type Code int\n",
		main: "\tvar c lib.Code = 42\n\tos.Exit(int(c))\n",
	},
	{
		// A struct whose field names carry a tag, an embedded field and an
		// unexported name. All three are in the field array, and the
		// unexported one is why the descriptor carries a package path of its
		// own.
		name: "a struct with a tag and an unexported field",
		lib:  "type Inner struct{ A int }\n\ntype Outer struct {\n\tInner\n\tB int `json:\"b\"`\n\tc int\n}\n",
		main: "\tvar o lib.Outer\n\to.A, o.B = 40, 2\n\tos.Exit(o.A + o.B)\n",
	},
}

// TestGcLinksAndRunsAgainstWhatNanogoWrote is the cross-read carried one step
// further, to the consumer.
//
// export/crossread_test.go has gc compile an importer of every standard
// library package nanogo wrote, which exercises both of gc's readers and
// nothing else. It cannot see a symbol the library owes: the reference is
// emitted at compile time and resolved at link time. This test links and runs,
// and the shape that found the gap is in the refusal table below rather than
// here.
func TestGcLinksAndRunsAgainstWhatNanogoWrote(t *testing.T) {
	for _, s := range linkingShapes {
		t.Run(s.name, func(t *testing.T) {
			h := setup(t, shapeModule(s), []string{"nanogo.example/shape/lib"})

			out, err := h.build(t, "-o", "prog", ".")
			if err != nil {
				t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
			}
			if !compiled(h.decisions(t), "nanogo.example/shape/lib") {
				t.Fatalf("nanogo delegated the library, so gc linked against gc's own object:\n%s",
					strings.Join(h.decisions(t), "\n"))
			}

			b, err := exec.Command(filepath.Join(h.mod, "prog")).CombinedOutput()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("the program did not run: %v\n%s", err, b)
			}
			if code != 42 {
				t.Errorf("the program exited %d, want 42\n%s", code, b)
			}
		})
	}
}

// shapeModule is the two-package module one shape is built in.
func shapeModule(s linkShape) map[string]string {
	return map[string]string{
		"go.mod":     "module nanogo.example/shape\n\ngo 1.27\n",
		"lib/lib.go": "package lib\n\n" + s.lib,
		"main.go": "package main\n\nimport (\n\t\"os\"\n\n\t\"nanogo.example/shape/lib\"\n)\n\n" +
			"func main() {\n" + s.main + "}\n",
	}
}

// refusedShapes are the library shapes nanogo will not compile, and the type
// each refusal has to name.
//
// Every one of them declares a type. gc, told by the export data that the type
// exists, compiles an importer that refers to it twice over: directly, where
// the code needs the runtime type descriptor, and through DWARF, where a
// variable of that type names go:info.<path>.<Type>. cmd/link builds the DWARF
// entry out of the descriptor, so both come back to the one symbol the
// defining package owes and nanogo cannot write, type:<path>.<Type>.
// The list is what is left of it. A declared type whose descriptor nanogo can
// write is in linkingShapes above and is linked and run rather than refused.
var refusedShapes = []linkShape{
	{name: "a named map", lib: "type M map[string]int\n", main: "M"},
	{name: "an interface", lib: "type I interface{ F() int }\n", main: "I"},
	{name: "a type with a method", lib: "type Code int\n\nfunc (c Code) V() int { return int(c) }\n", main: "Code"},
	{name: "a struct that compares field by field", lib: "type Label struct{ Key, Value string }\n", main: "Label"},
	{name: "a struct holding a map", lib: "type Table struct{ M map[string]int }\n", main: "Table"},
}

// TestNanogoRefusesATypeAnImporterCouldNotLinkAgainst is the other half of the
// seam, and the reason a named basic type is in the table above it.
//
// The library is not the variable. The same library, compiled by nanogo,
// linked against one importer and not against another: "os.Exit(int(lib.F()))"
// linked and "c := lib.F(); os.Exit(int(c))" did not, because the second put
// the imported type on a local variable and gc emitted a DWARF reference for
// it. So no property of the library separates the safe case from the unsafe
// one, and a compiler that refused only the unsafe ones would be guessing
// about a package it cannot see.
//
// nanogo therefore refuses at compile time, and the message names the package,
// the type, its position, the symbol an importer needs and the spec that owns
// the gap. What it replaces is a link failure that names none of them:
//
//	sym 5: relocation target go:info.xread/lib.Point not defined
func TestNanogoRefusesATypeAnImporterCouldNotLinkAgainst(t *testing.T) {
	for _, s := range refusedShapes {
		t.Run(s.name, func(t *testing.T) {
			files := shapeModule(linkShape{lib: s.lib, main: "\tos.Exit(42)\n"})
			// The importer must not mention the library's type, so that what
			// the build reports is the refusal and not a type error.
			files["main.go"] = "package main\n\nimport (\n\t\"os\"\n\n\t_ \"nanogo.example/shape/lib\"\n)\n\n" +
				"func main() { os.Exit(42) }\n"
			h := setup(t, files, []string{"nanogo.example/shape/lib"})

			out, err := h.build(t, "-o", "prog", ".")
			if err == nil {
				t.Fatalf("the build succeeded although the library declares a type nanogo cannot describe:\n%s", out)
			}
			for _, want := range []string{
				"nanogo.example/shape/lib", // the package
				s.main,                     // the type
				"lib.go:",                  // where it is declared
				"type:nanogo.example/shape/lib." + s.main, // the symbol an importer needs
				"specs/032", // the spec that owns the gap
			} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not mention %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "relocation target") {
				t.Errorf("the build reached the linker, so the refusal did not fire:\n%s", out)
			}
		})
	}
}

// The library whose descriptors a gc-compiled importer reflects over.
//
// Every declaration here is one nanogo writes the runtime type descriptor for,
// and nothing else in the link writes them: gc's writeType emits a descriptor
// for a type declared in another package only when it is compiling that
// package, so the bytes reflect reads below are nanogo's.
const reflectLibrary = `package lib

type Inner struct{ A int64 }

type Point struct {
	Inner
	X int
	Y int ` + "`json:\"y\"`" + `
	n int
}

type Code int

type List []Point

func Sink(p Point) int { return p.X + p.Y + p.n + int(p.A) }
`

// The importer, which asks reflect what nanogo wrote and exits 42 when every
// answer agrees.
//
// A distinct exit status per question, so a failure says which field of the
// descriptor is wrong rather than that something is.
const reflectProgram = `package main

import (
	"os"
	"reflect"

	"nanogo.example/shape/lib"
)

func check(ok bool, code int) {
	if !ok {
		os.Exit(code)
	}
}

func main() {
	t := reflect.TypeOf(lib.Point{})
	// Str, the name string, which is qualified by the package name and not by
	// the import path.
	check(t.String() == "lib.Point", 1)
	check(t.Name() == "Point", 2)
	// The UncommonType's package path.
	check(t.PkgPath() == "nanogo.example/shape/lib", 3)
	check(t.NumMethod() == 0, 4)
	// The StructType's field array.
	check(t.NumField() == 4, 5)
	check(t.Field(0).Name == "Inner" && t.Field(0).Anonymous, 6)
	check(t.Field(1).Name == "X" && !t.Field(1).Anonymous, 7)
	check(string(t.Field(2).Tag) == ` + "`json:\"y\"`" + `, 8)
	// The struct's own package path, which is the one an unexported field
	// name is reached through.
	check(t.Field(3).Name == "n" && t.Field(3).PkgPath == "nanogo.example/shape/lib", 9)
	// The offsets, which the descriptor carries per field.
	check(t.Field(0).Offset == 0 && t.Field(1).Offset == 8, 10)
	check(t.Size() == 32 && t.Align() == 8, 11)

	// Equal and Hash, read through an interface and through a map. A wrong
	// Equal makes these compare unequal or panic; a wrong Hash makes the map
	// miss.
	var a any = lib.Point{X: 1, Y: 2}
	var b any = lib.Point{X: 1, Y: 2}
	check(a == b, 12)
	m := map[any]int{a: 40}
	check(m[b] == 40, 13)
	check(lib.Sink(lib.Point{X: 1, Y: 1}) == 2, 14)

	// A defined type over a predeclared one. internal/abi distinguishes Int
	// from Int64 and the IR does not, so this is the check on ir.Type.Basic.
	c := reflect.TypeOf(lib.Code(0))
	check(c.Kind() == reflect.Int, 15)
	check(c.String() == "lib.Code" && c.PkgPath() == "nanogo.example/shape/lib", 16)

	// A defined slice, whose descriptor names the element's.
	l := reflect.TypeOf(lib.List(nil))
	check(l.Kind() == reflect.Slice && l.Elem() == t, 17)
	check(l.String() == "lib.List", 18)

	os.Exit(42)
}
`

// TestGcReflectsOverATypeNanogoDescribed reads the descriptor back with the
// runtime's own decoder.
//
// rtype's unit tests compare nanogo's bytes with gc's field by field, which
// says the two agree. This says the runtime agrees: the program links against
// the descriptor nanogo wrote, and reflect walks it the way the runtime walks
// every descriptor. The name string, the package path, the field array with
// its tags and its embedded and unexported names, the equality closure and the
// type hash are each read here through the accessor that reads them in a real
// program.
//
// specs/032-type-descriptors-and-itabs.md asks for exactly this and calls it
// hosted mode.
func TestGcReflectsOverATypeNanogoDescribed(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/shape\n\ngo 1.27\n",
		"lib/lib.go": reflectLibrary,
		"main.go":    reflectProgram,
	}, []string{"nanogo.example/shape/lib"})

	out, err := h.build(t, "-o", "prog", ".")
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if !compiled(h.decisions(t), "nanogo.example/shape/lib") {
		t.Fatalf("nanogo delegated the library, so reflect read gc's descriptors:\n%s",
			strings.Join(h.decisions(t), "\n"))
	}
	b, err := exec.Command(filepath.Join(h.mod, "prog")).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("the program did not run: %v\n%s", err, b)
	}
	if code != 42 {
		t.Fatalf("the program exited %d, want 42; the number is the check that failed\n%s", code, b)
	}
}
