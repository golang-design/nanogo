// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/types2"
)

// gcArchive compiles a package with gc and returns the archive that holds its
// export data.
//
// It is the real thing, not a fixture: the go command writes it exactly as it
// does in a build, so what the importer reads here is what it reads under
// -toolexec.
func gcArchive(t *testing.T, dir, path string) string {
	t.Helper()
	needGoCommand(t)
	cmd := exec.Command("go", "list", "-export", "-f", "{{.Export}}", path)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=", "GO111MODULE=on", "GOPROXY=off")
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Skipf("go list -export %s: %v\n%s", path, err, stderr)
	}
	file := strings.TrimSpace(string(out))
	if file == "" {
		t.Fatalf("go list -export %s reported no archive", path)
	}
	return file
}

// libModule writes a module with one library package in it and returns the
// module directory.
func libModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/two\n\ngo 1.27\n")
	write("lib/lib.go", "package lib\n\nconst Scale = 7\n\nfunc Add(a, b int) int { return a + b }\n")
	return dir
}

// TestCompileReadsAnImport is what specs/015-export-data.md was blocking.
//
// gc compiles the library, nanogo compiles the package that imports it, and
// the import resolves through the archive -importcfg names. The body uses both
// kinds of imported declaration a compiled function can reach today: a
// constant, which the checker folds, and a function, which becomes a call to a
// symbol another package defines.
func TestCompileReadsAnImport(t *testing.T) {
	arm64Only(t)
	dir := libModule(t)
	archive := gcArchive(t, dir, "./lib")

	cfgFile := filepath.Join(t.TempDir(), "importcfg")
	if err := os.WriteFile(cfgFile, []byte("packagefile nanogo.example/two/lib="+archive+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := `package main

import "nanogo.example/two/lib"

func main() {
	d := lib.Add(20, 3) - lib.Scale - 16
	if d != 0 {
		d = d / (d - d)
	}
}
`
	out, err := compileSource(t, src, func(c *Config) {
		c.ImportCfgFile = cfgFile
		c.ImportCfg = mustReadImportCfg(t, cfgFile)
		c.Pack = true
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// The object has to name the imported package twice: once in the Autolib
	// block, which is what makes the linker load the archive at all, and once
	// as the symbol the call refers to.
	if !strings.Contains(string(b), "nanogo.example/two/lib.Add") {
		t.Error("the object does not refer to the imported function")
	}
	if !strings.Contains(string(b), "nanogo.example/two/lib") {
		t.Error("the object has no Autolib entry for the imported package")
	}
}

// TestCompileWithImportsIsDeterministic is specs/053-determinism.md over the
// output path this component added.
//
// The object carries one Autolib entry per direct import, and the order is the
// order the type checker asked for them. Nothing else in the compiler would
// notice if that order started coming from a map, and the failure it would
// produce is the one specs/053 is written to prevent: a fixed point that
// breaks with the first suspicion falling on code generation.
//
// Two imports, because a single entry is in order whatever produced it.
func TestCompileWithImportsIsDeterministic(t *testing.T) {
	arm64Only(t)
	dir := libModule(t)
	cfgFile := filepath.Join(dir, "importcfg")
	body := "packagefile nanogo.example/two/lib=" + gcArchive(t, dir, "./lib") + "\n" +
		"packagefile math/bits=" + gcArchive(t, dir, "math/bits") + "\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte(`package main

import (
	"math/bits"

	"nanogo.example/two/lib"
)

func main() {
	d := lib.Add(20, 3) - lib.Scale - 16 + bits.OnesCount64(7) - 3
	if d != 0 {
		d = d / (d - d)
	}
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The same source path both times, because the file name reaches the
	// pc-file table and two paths are two different objects for a good
	// reason.
	compile := func(out string) []byte {
		t.Helper()
		cfg := &Config{
			Package:   "main",
			Output:    out,
			Lang:      "go1.27",
			Files:     []string{src},
			ImportCfg: mustReadImportCfg(t, cfgFile),
		}
		if err := Compile(cfg); err != nil {
			t.Fatalf("Compile: %v", err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if string(compile(filepath.Join(dir, "1.o"))) != string(compile(filepath.Join(dir, "2.o"))) {
		t.Error("two compilations of one package with imports produced different objects")
	}
}

// TestCompileReadsAStandardLibraryImport is the same claim against an archive
// nobody wrote for this test.
//
// math/bits is the entry point into the standard library that a nanogo
// compiled body can actually use: its constants are constants and
// OnesCount64 takes and returns a machine word.
func TestCompileReadsAStandardLibraryImport(t *testing.T) {
	arm64Only(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/std\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := gcArchive(t, dir, "math/bits")
	cfgFile := filepath.Join(dir, "importcfg")
	if err := os.WriteFile(cfgFile, []byte("packagefile math/bits="+archive+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := `package main

import "math/bits"

func main() {
	d := bits.OnesCount64(7) - 3 + bits.UintSize - 64
	if d != 0 {
		d = d / (d - d)
	}
}
`
	if _, err := compileSource(t, src, func(c *Config) {
		c.ImportCfg = mustReadImportCfg(t, cfgFile)
	}); err != nil {
		t.Fatalf("Compile: %v", err)
	}
}

// TestCompileResolvesUnsafe covers the one import with no archive.
func TestCompileResolvesUnsafe(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	src := `package main

import "unsafe"

func main() {
	d := int(unsafe.Sizeof(int64(0))) - 8
	if d != 0 {
		d = d / (d - d)
	}
}
`
	if _, err := compileSource(t, src, nil); err != nil {
		t.Fatalf("Compile: %v", err)
	}
}

// TestImportNamesTheFileAndTheReason checks the message a build gets when the
// archive is there and is not readable.
//
// The package being compiled, the import path, the file and what is wrong with
// it all have to be in the message. A build that hits this has hundreds of
// packages in it, and the three questions a user asks are which package, which
// import, and whether the file is stale or the compiler is wrong.
func TestImportNamesTheFileAndTheReason(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "junk.a")
	if err := os.WriteFile(archive, []byte("this is not an archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustImportCfg(t, "packagefile nanogo.example/junk="+archive+"\n")
	imp := newImporter(&Config{Package: "main", ImportCfg: cfg})
	pkg, err := imp.Import("nanogo.example/junk")
	if pkg != nil {
		t.Error("the importer returned a package")
	}
	var ie *ImportError
	if !errors.As(err, &ie) {
		t.Fatalf("Import = %v (%T), want *ImportError", err, err)
	}
	for _, want := range []string{"main", "nanogo.example/junk", archive, "not an archive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name %q: %v", want, err)
		}
	}
	if ie.Unwrap() == nil {
		t.Error("the error carries no reason to unwrap")
	}
}

// TestImportWithNoEntryIsADifferentFailure separates the two reasons an import
// fails, because they need different fixes.
func TestImportWithNoEntryIsADifferentFailure(t *testing.T) {
	imp := newImporter(&Config{Package: "main"})
	if _, err := imp.Import("errors"); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Errorf("Import with no configuration = %v, want a message about the missing entry", err)
	}
	// An importmap renames the path before the lookup, and the renamed path
	// is the one the message and the reader use.
	imp = newImporter(&Config{
		Package:   "main",
		ImportCfg: mustImportCfg(t, "importmap errors=vendor/errors\npackagefile vendor/errors=/pkg/v.a\n"),
	})
	if _, err := imp.Import("errors"); err == nil || !strings.Contains(err.Error(), "/pkg/v.a") {
		t.Errorf("Import through an importmap = %v, want the mapped archive", err)
	}
}

// TestImporterResolvesUnsafeThroughTheConfiguration checks that unsafe is
// answered even when -importcfg exists and has no entry for it, which is every
// build: the go command never lists unsafe.
func TestImporterResolvesUnsafeThroughTheConfiguration(t *testing.T) {
	imp := newImporter(&Config{Package: "main", ImportCfg: mustImportCfg(t, "packagefile errors=/pkg/e.a\n")})
	pkg, err := imp.Import("unsafe")
	if err != nil {
		t.Fatalf("Import(unsafe) = %v", err)
	}
	if pkg != types2.Unsafe {
		t.Errorf("Import(unsafe) = %v, want the universe's unsafe", pkg)
	}
}

// TestCompileRecoversADamagedDeclaration is why checkFiles defers a recover.
//
// A declaration is decoded when the checker first looks it up, which is long
// after the importer returned, so the reader has no error channel left and
// signals the failure by panicking. Without the recover the compiler would
// die on a build the go command constructed, and the user would see a stack
// trace instead of the name of the package that stopped.
func TestCompileRecoversADamagedDeclaration(t *testing.T) {
	arm64Only(t)
	dir := libModule(t)
	damaged := damageDeclarations(t, gcArchive(t, dir, "./lib"))

	cfgFile := filepath.Join(t.TempDir(), "importcfg")
	if err := os.WriteFile(cfgFile, []byte("packagefile nanogo.example/two/lib="+damaged+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := `package main

import "nanogo.example/two/lib"

func main() {
	d := lib.Add(20, 3)
	if d != 23 {
		d = d / (d - d)
	}
}
`
	_, err := compileSource(t, src, func(c *Config) { c.ImportCfg = mustReadImportCfg(t, cfgFile) })
	if err == nil {
		t.Fatal("Compile succeeded on export data whose declarations are damaged")
	}
	// The message names the package the build asked nanogo to compile, and
	// carries the reader's own message, which names the import and says what
	// it could not decode.
	for _, want := range []string{"main", "pkgbits", "nanogo.example/two/lib"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not carry %q: %v", want, err)
		}
	}
}

// TestRecoveredCarriesTheValue checks that a panic from a nanogo bug is not
// renamed into a story about export data.
func TestRecoveredCarriesTheValue(t *testing.T) {
	imp := newImporter(&Config{Package: "strconv"})
	err := imp.recovered(errors.New("a bug in a pass"))
	for _, want := range []string{"strconv", "a bug in a pass"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("recovered() = %v, and it does not carry %q", err, want)
		}
	}
	// The importer also works with no configuration at all, because a
	// package that imports nothing gets no -importcfg.
	if err := newImporter(nil).recovered("boom"); !strings.Contains(err.Error(), "boom") {
		t.Errorf("recovered() with no configuration = %v", err)
	}
}

// mustReadImportCfg reads a configuration file the test just wrote.
func mustReadImportCfg(t *testing.T, name string) *ImportCfg {
	t.Helper()
	cfg, err := ReadImportCfg(name)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// damageDeclarations overwrites the declaration section of an archive's export
// data and returns the path of the damaged copy.
//
// The header and the name section are left intact on purpose, so that the
// archive still opens and still lists the names it declares. The failure then
// happens where the test needs it: at the moment the checker asks what one of
// those names is.
//
// The layout it walks is the container's header, which is documented in
// export/pkgbits/doc.go: a version word, a flags word, one end offset per
// section, then one end offset per element, then the element data.
func damageDeclarations(t *testing.T, archive string) string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	const (
		sections = 10 // pkgbits has ten sections
		objIdx   = 6  // and SectionObj is the seventh
		marker   = "$$B\nu"
	)
	i := strings.Index(string(data), marker)
	if i < 0 {
		t.Fatalf("%s does not hold unified export data", archive)
	}
	payload := data[i+len(marker):]
	word := func(n int) int { return int(binary.LittleEndian.Uint32(payload[n*4:])) }
	// Two header words, then the section ends, then the element ends.
	sectionEnds := 2
	elemEnds := sectionEnds + sections
	elems := word(elemEnds - 1)
	body := (elemEnds + elems) * 4
	// Every element of a section is contiguous, so the section's bytes run
	// from the end of the element before its first to the end of its last.
	start, end := 0, word(elemEnds+word(sectionEnds+objIdx)-1)
	if first := word(sectionEnds + objIdx - 1); first > 0 {
		start = word(elemEnds + first - 1)
	}
	for n := body + start; n < body+end && n < len(payload); n++ {
		payload[n] = 0xff
	}

	out := filepath.Join(t.TempDir(), "damaged.a")
	if err := os.WriteFile(out, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestImportErrorMessages checks the two forms of the message directly, so
// that a change to either one is a change to a test and not a surprise in a
// build log.
func TestImportErrorMessages(t *testing.T) {
	e := &ImportError{Path: "errors", Package: "strconv"}
	if !strings.Contains(e.Error(), "no entry") {
		t.Errorf("an import with no configuration entry reads %q", e)
	}
	e.File = "/pkg/errors.a"
	e.Err = fmt.Errorf("not an archive")
	got := e.Error()
	for _, want := range []string{"strconv", "errors", "/pkg/errors.a", "not an archive"} {
		if !strings.Contains(got, want) {
			t.Errorf("the message does not name %q: %q", want, got)
		}
	}
}

// stdImportCfg returns an ImportCfg naming the real archive of each package,
// which is what a call into the standard library needs.
//
// The archives come from the installed toolchain through go list -export, so
// the export data they hold is gc's own and not a fixture. That is the point:
// a body read out of one is the shape nanogo must actually decode.
func stdImportCfg(t *testing.T, dir string, paths ...string) *ImportCfg {
	t.Helper()
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("packagefile " + p + "=" + gcArchive(t, dir, p) + "\n")
	}
	cfg, err := ParseImportCfg("importcfg", []byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestCompileStencilsAGenericOfAnotherPackage runs the foreign stencil inside
// this process.
//
// internal/e2e covers the same path end to end and compares the program's
// output against gc's, which is the evidence that the substitution is right.
// It cannot cover this code, because it drives nanogo as a separate process
// through go build -toolexec, so nothing it exercises appears in this
// package's coverage and a walk with no in-process test would look tested and
// not be.
//
// slices.Contains is the smallest instantiation that exercises the whole join:
// the declaration is read out of the standard library's own archive, the body
// carries the dictionary its slots were numbered against, and its call to
// slices.Index reaches that dictionary through a subdictionary slot rather
// than through any reference to Index.
func TestCompileStencilsAGenericOfAnotherPackage(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		what string
		src  string
	}{
		{"a generic function at one word", "package main\n\nimport \"slices\"\n\n" +
			"func f(xs []int, v int) bool { return slices.Contains(xs, v) }\n\n" +
			"func main() {\n\tif !f([]int{1, 2, 3}, 2) {\n\t\tpanic(\"no\")\n\t}\n}\n"},
		{"a generic function at two words", "package main\n\nimport \"slices\"\n\n" +
			"func f(xs []string, v string) bool { return slices.Contains(xs, v) }\n\n" +
			"func main() {\n\tif !f([]string{\"a\"}, \"a\") {\n\t\tpanic(\"no\")\n\t}\n}\n"},
		{"a method of a foreign generic type", "package main\n\nimport \"sync/atomic\"\n\n" +
			"func main() {\n\tvar p atomic.Pointer[int]\n\tn := 3\n\tp.Store(&n)\n\t" +
			"if *p.Load() != 3 {\n\t\tpanic(\"no\")\n\t}\n}\n"},
		// Every method of one instantiation, so the walk meets a swap and a
		// compare-and-swap beside the load and the store, and meets them
		// against a receiver whose type argument is a pointer.
		{"every method of a foreign generic type", "package main\n\nimport \"sync/atomic\"\n\n" +
			"func main() {\n\tvar p atomic.Pointer[[]int]\n\ta, b := []int{1}, []int{2}\n" +
			"\tp.Store(&a)\n\tp.Swap(&b)\n\tif !p.CompareAndSwap(&b, &a) {\n\t\tpanic(\"cas\")\n\t}\n" +
			"\tif len(*p.Load()) != 1 {\n\t\tpanic(\"load\")\n\t}\n}\n"},
		{"a generic returning a boolean comparison", "package main\n\nimport \"cmp\"\n\n" +
			"func f(a, b string) bool { return cmp.Less(a, b) }\n\n" +
			"func main() {\n\tif !f(\"a\", \"b\") {\n\t\tpanic(\"no\")\n\t}\n}\n"},
		{"a generic reached through another generic", "package main\n\nimport \"slices\"\n\n" +
			"func f(xs []string, v string) int { return slices.Index(xs, v) }\n\n" +
			"func main() {\n\tif f([]string{\"a\", \"b\"}, \"b\") != 1 {\n\t\tpanic(\"no\")\n\t}\n}\n"},
		// The four the self-host gate's last package reaches, and the whole of
		// pdqsort behind them: a body of assignments, three-clause loops,
		// parallel swaps, an expression switch, break and continue, len, and a
		// method of a concrete type another package declares.
		{"a three-way comparison", "package main\n\nimport \"cmp\"\n\n" +
			"func f(a, b int) int { return cmp.Compare(a, b) }\n\n" +
			"func main() {\n\tif f(1, 2) != -1 {\n\t\tpanic(\"no\")\n\t}\n}\n"},
		// cmp is imported because slices.Sort reaches cmp.Less, whose body is
		// in cmp's own archive: [export.Reader] reads the bodies of a package
		// this compilation read an archive for, and the archive list is the
		// checker's imports.
		{"a sort of an ordered slice", "package main\n\nimport (\n\t\"cmp\"\n\t\"slices\"\n)\n\n" +
			"func main() {\n\txs := []int{3, 1, 2}\n\tslices.Sort(xs)\n" +
			"\tif xs[0] != 1 || cmp.Compare(1, 2) != -1 {\n\t\tpanic(\"no\")\n\t}\n}\n"},
		{"a sort with a comparison function", "package main\n\nimport (\n\t\"cmp\"\n\t\"slices\"\n)\n\n" +
			"func main() {\n\txs := []int{3, 1, 2}\n" +
			"\tslices.SortFunc(xs, func(a, b int) int { return cmp.Compare(a, b) })\n" +
			"\tif !slices.IsSortedFunc(xs, func(a, b int) int { return cmp.Compare(a, b) }) {\n\t\tpanic(\"no\")\n\t}\n}\n"},
		{"an element-by-element comparison", "package main\n\nimport \"slices\"\n\n" +
			"func main() {\n\txs := []int{1, 2}\n" +
			"\tif !slices.EqualFunc(xs, xs, func(a, b int) bool { return a == b }) {\n\t\tpanic(\"no\")\n\t}\n}\n"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			_, err := compileSource(t, tc.src, func(c *Config) {
				c.ImportCfg = stdImportCfg(t, dir, "slices", "sync/atomic", "cmp")
			})
			if err != nil {
				t.Errorf("%s was refused: %v", tc.what, err)
			}
		})
	}
}

// TestCompileRefusesAForeignBodyItCannotMap is the boundary of the walk above,
// measured against a real body rather than a written one.
//
// The mapping covers what the instantiations nanogo's own source reaches need
// and refuses the rest by name. slices.Clone is the smallest declaration in the
// standard library that falls outside it: its body names the zero value of a
// slice type, and a zero value is a kind the walk does not build. The refusal
// has to name the declaration and the kind, because that pair is the whole of
// what somebody extending the mapping needs to know.
//
// A test that only asserted the compile would pass just as well if the walk
// guessed at the node and produced a function that computes something else,
// which is the failure this refusal exists to prevent.
func TestCompileRefusesAForeignBodyItCannotMap(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	src := "package main\n\nimport \"slices\"\n\n" +
		"func f(s []int) int { return len(slices.Clone(s)) }\n\n" +
		"func main() {\n\tif f([]int{1, 2}) != 2 {\n\t\tpanic(\"no\")\n\t}\n}\n"
	_, err := compileSource(t, src, func(c *Config) {
		c.ImportCfg = stdImportCfg(t, dir, "slices", "sync/atomic", "cmp")
	})
	if err == nil {
		t.Fatal("a body holding a kind the walk does not map was built")
	}
	for _, want := range []string{"slices.Clone[[]int,int]", "zero value"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// genericLibModule writes a module whose library declares generics covering
// the constructs the foreign walk maps, and returns the module directory.
//
// The standard library is not enough on its own. Every generic in it that the
// walk accepts reaches the same few nodes, so whole branches of the mapping
// are built and never met: the unary operators beside the address and the
// dereference, an index of an array rather than a slice, a field of a struct
// parameter, a package-scope declaration read from inside a foreign body.
// A body that meets none of them is a body that proves nothing about them.
//
// gc compiles this library, so its export data is gc's own and the bodies are
// the shape nanogo must really decode, exactly as for a standard library
// package. What it adds is the choice of what those bodies contain.
func genericLibModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/gen\n\ngo 1.27\n")
	write("lib/lib.go", `package lib

// Zero is the package-scope declaration a foreign body reads.
const Zero = 0

type Box[T any] struct {
	V T
	N int
}

// Field reads a field of a generic struct.
func Field[T any](b Box[T]) int { return b.N }

// Negate is the unary minus and the unary plus.
func Negate[T ~int](v T) T { return -(+v) }

// Complement is the unary xor.
func Complement[T ~int](v T) T { return ^v }

// Not is the unary not.
func Not(v bool) bool { return !v }

// Deref is the dereference, and Addr the address of a parameter.
func Deref[T any](p *T) T { return *p }

// ArrayAt indexes an array rather than a slice.
func ArrayAt[T any](a [3]T, i int) T { return a[i] }

// Arith is the binary operators that are not comparisons.
func Arith[T ~int](a, b T) T { return a*b + a/b - a%b }

// Shifted is the shifts and the bitwise binaries.
func Shifted[T ~int](a, b T) T { return (a<<2 | b>>1) & (a ^ b) }

// Global reads a package-scope constant from inside a generic body.
func Global[T ~int](v T) T { return v + Zero }

// Counter is the package-scope variable a foreign body reads, which reaches a
// different arm of the walk from a constant: a constant is folded into the
// tree and a variable is a symbol another package defines.
var Counter = 11

// ReadVar reads it.
func ReadVar[T ~int](v T) int { return int(v) + Counter }

// SliceAt indexes a slice at a value rather than at a constant, so the index
// is an operand the walk has to build rather than a number it can fold.
func SliceAt[T any](s []T, i int) T { return s[i] }

// PtrField reads a field through a pointer, which is a dereference the walk
// has to insert rather than one the body wrote.
func PtrField[T any](b *Box[T]) int { return b.N }

// Outer embeds Box, so a selection of N through it is a promoted field and
// reaches the walk as a path of more than one step.
type Outer[T any] struct {
	Box[T]
	M int
}

// Promoted reads the embedded field.
func Promoted[T any](o Outer[T]) int { return o.N }

// PtrArrayAt indexes a pointer to an array, which the specification
// dereferences implicitly and the tree does not.
func PtrArrayAt[T any](p *[3]T, i int) T { return p[i] }

// The slice expression. It is the one node whose lowering is decided by the
// class of its operand, so each of the four classes is met at every bound form
// the language gives it. A slice, a string and a pointer to an array are read
// through a header or a length; an array is the class whose address the walk
// has to take, because the result points into the array's storage.

func SliceAll[T any](s []T) int { return len(s[:]) }

func SliceLo[T any](s []T) int { return len(s[1:]) }

func SliceHi[T any](s []T) int { return len(s[:2]) }

func SliceLoHi[T any](s []T) int { return len(s[1:3]) }

func SliceMax[T any](s []T) int { return cap(s[1:2:4]) }

// SliceVar takes its bounds from parameters, so each bound is an operand the
// walk builds rather than a number the tree already folded.
func SliceVar[T any](s []T, lo, hi int) int { return len(s[lo:hi]) }

// SliceVarMax is the three-index form at bounds that are not constants, which
// is the shape every bounds check of ir/lower.go is emitted for.
func SliceVarMax[T any](s []T, lo, hi, max int) int { return cap(s[lo:hi:max]) }

func SliceStrAll[T ~string](s T) int { return len(s[:]) }

func SliceStrLo[T ~string](s T) int { return len(s[2:]) }

func SliceStrHi[T ~string](s T) int { return len(s[:2]) }

func SliceStrLoHi[T ~string](s T) int { return len(s[1:3]) }

func SliceArrAll[T any](a [4]T) int { return len(a[:]) }

func SliceArrLoHi[T any](a [4]T) int { return len(a[1:3]) }

func SliceArrMax[T any](a [4]T) int { return cap(a[1:2:4]) }

func SlicePtrArrAll[T any](p *[4]T) int { return len(p[:]) }

func SlicePtrArrLoHi[T any](p *[4]T) int { return len(p[1:3]) }

func SlicePtrArrMax[T any](p *[4]T) int { return cap(p[1:2:4]) }

// Arr holds an array field, so SliceField takes the address of a field of a
// variable rather than of the variable.
type Arr[T any] struct{ A [4]T }

func SliceField[T any](v Arr[T]) int { return len(v.A[1:]) }

// SliceLocal slices an array the body itself declares, which is the array that
// has no address until this walk takes one.
func SliceLocal[T any](v T) int {
	var a [4]T
	a[0] = v
	return len(a[1:]) + cap(a[:2:3])
}

// SliceValue returns the slice rather than its length, so the pointer the
// lowering computes is read and not only measured.
func SliceValue[T any](s []T) T { return s[1:][0] }

// SliceStrValue is the same for a string, whose result is a string and not a
// slice of one.
func SliceStrValue[T ~string](s T) byte { return s[1:][0] }

// SliceArrValue is the same for an array, where a wrong build gives the caller
// a pointer into a copy and the write below is then lost.
func SliceArrValue[T any](a [4]T, v T) T {
	s := a[1:]
	s[0] = v
	return a[1]
}

// StrAt indexes a string, which is neither a slice nor an array.
func StrAt[T ~string](s T, i int) byte { return s[i] }

// Addr takes the address of a parameter.
func Addr[T any](v T) *T { return &v }

// Variadic is a variadic generic, whose call site the walk has to build twice:
// once packing the loose arguments into a slice and once passing a slice
// straight through.
func Variadic[T ~int](base T, vs ...T) T { return base + vs[0] }

// CallVariadic calls it with loose arguments from inside a foreign body, and
// SpreadVariadic with a slice, so both arms are met through an instantiation
// rather than only from this test's own call site.
func CallVariadic[T ~int](a, b T) T { return Variadic(a, b, a) }

func SpreadVariadic[T ~int](vs []T) T { return Variadic(vs[0], vs...) }

// Guarded is an if with an else, over a comparison chain.
func Guarded[T ~int](v T) int {
	if v < 0 {
		return -1
	} else if v > 0 {
		return 1
	}
	return 0
}

// Length is the builtin len, and Capacity the builtin cap.
func Length[T any](s []T) int { return len(s) }

func Capacity[T any](s []T) int { return cap(s) }

// Sum is an assignment, an operation assignment, and a three-clause loop.
func Sum[T ~int](vs []T) T {
	total := T(0)
	for i := 0; i < len(vs); i++ {
		total += vs[i]
	}
	return total
}

// Reverse is the parallel assignment, which is the one a wrong build turns
// into two copies of one value rather than a swap, and a loop whose post
// statement assigns two variables at once.
func Reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// Counted is a var declaration with no initialiser, ++, break and continue.
func Counted[T ~int](s []T, skip T) int {
	var n int
	for _, v := range s {
		if v == skip {
			continue
		}
		if v < 0 {
			break
		}
		n++
	}
	return n
}

// Classify is an expression switch with a multi-value case and a default,
// inside a loop, so that the break in one clause and the continue in another
// have two plausible targets each: break ends the switch and reaches the
// statement after it, and continue reaches the loop's post statement.
func Classify[T ~int](v T) int {
	n := 0
	for i := 0; i < 3; i++ {
		switch v {
		case 0:
			n += 10
			break
		case 1, 2:
			n += 20
			continue
		default:
			n += 30
		}
		n++
	}
	return n
}

// Two returns two values and Pair assigns them at once, then swaps them and
// throws one away, which is the blank destination.
func Two[T ~int](v T) (T, T) { return v, v + 1 }

func Pair[T ~int](v T) T {
	a, b := Two(v)
	a, b = b, a
	_ = b
	return a - b
}

// Shifted assigns through a shift and through a bitwise or, and -- decrements.
func ShiftAssign[T ~int](v T) T {
	v <<= 2
	v |= 1
	v--
	return v
}

// Tick has a method with a pointer receiver, so a foreign body that calls it
// takes the address of a local first.
type Tick struct{ N int }

func (t *Tick) Bump() int { t.N++; return t.N }

// Bumped calls it twice, so the two calls have to see one another's write.
func Bumped[T ~int](v T) int {
	var t Tick
	t.N = int(v)
	return t.Bump() + t.Bump()
}

// Capture is a function literal that reads and writes a variable of the body
// around it. The variable is read after the literal has run, so a capture by
// value is a different answer and not a link failure.
func Capture[T ~int](v T) T {
	acc := v
	add := func(d T) { acc += d }
	add(v)
	add(1)
	return acc
}

// Nested is a literal inside a literal, whose capture reaches through two
// levels: the outer literal captures a variable it does not read itself.
func Nested[T ~int](v T) T {
	acc := v
	outer := func() func() {
		return func() { acc += 2 }
	}
	outer()()
	return acc
}
`)
	write("refuse/refuse.go", `package refuse

// Each of these is one construct the foreign walk does not map. They live in
// their own package so that a test can instantiate exactly one of them and see
// the refusal that names it.

func MapAt[T comparable](m map[T]int, k T) int { return m[k] }

func Assert[T any](v any) bool { _, ok := v.(T); return ok }

func Lit[T any](v T) []T { return []T{v} }

func Appended[T any](s []T, v T) int { return len(append(s, v)) }

func Ranged[T comparable](m map[T]int) int {
	n := 0
	for range m {
		n++
	}
	return n
}

func TypeSwitched[T any](v any, d T) int {
	switch v.(type) {
	case int:
		return 1
	}
	return 0
}

type Namer interface{ Name() int }

// ThroughDict calls a method on a type parameter, whose callee is a slot of
// the dictionary rather than a symbol the call names.
func ThroughDict[T Namer](v T) int { return v.Name() }

type Box[T any] struct{ V T }

func (b *Box[T]) Get() T { return b.V }

// WithSubdict calls a method of an instantiation whose type argument is the
// enclosing declaration's type parameter, so the call carries a dictionary.
func WithSubdict[T any](v T) T {
	var b Box[T]
	b.V = v
	return b.Get()
}

func Deferred[T any](f func(T), v T) { defer f(v) }
`)
	return dir
}

// TestCompileRefusesEachForeignConstructItDoesNotMap is the boundary of the
// walk, one construct at a time, against archives gc wrote.
//
// The mapping is partial on purpose and every kind outside it is refused by
// name. A refusal that named the kind but not the declaration would leave the
// reader of the message with nothing to open, and one that named neither would
// be indistinguishable from the walk guessing, which is the failure this whole
// design is arranged against.
//
// ir/foreign_test.go asserts the same property over trees written by hand.
// This asserts it over trees gc encoded, which is where the kinds actually
// come from.
func TestCompileRefusesEachForeignConstructItDoesNotMap(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := genericLibModule(t)
	archive := gcArchive(t, dir, "./refuse")

	cfgFile := filepath.Join(t.TempDir(), "importcfg")
	if err := os.WriteFile(cfgFile, []byte("packagefile nanogo.example/gen/refuse="+archive+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what string
		call string
		want string
	}{
		{"a map index", "refuse.MapAt(map[int]int{1: 2}, 1)", "an index of a map"},
		{"a type assertion", "btoi(refuse.Assert[int](any(1)))", `the expression "type assertion"`},
		{"a composite literal", "len(refuse.Lit(1))", `the expression "composite literal"`},
		{"a builtin that needs a descriptor", "refuse.Appended([]int{1}, 2)", "a call of the builtin append"},
		{"a range over a map", "refuse.Ranged(map[int]int{1: 2})", "and only a range over a slice is built"},
		{"a type switch", "refuse.TypeSwitched(any(1), 2)", "a type switch"},
		{"a method through a dictionary slot", "refuse.ThroughDict(named(3))", "a call of Name on a type parameter"},
		{"a method of an instantiation", "refuse.WithSubdict(4)", "a call of Get"},
		{"a deferred call", "func() int { refuse.Deferred(func(int) {}, 1); return 0 }()", `the statement "go or defer"`},
	} {
		t.Run(tc.what, func(t *testing.T) {
			src := "package main\n\nimport \"nanogo.example/gen/refuse\"\n\n" +
				"func btoi(b bool) int {\n\tif b {\n\t\treturn 1\n\t}\n\treturn 0\n}\n\n" +
				// A concrete type satisfying refuse.Namer, so that the call
				// through a dictionary slot is reached with no interface
				// value crossing the call site.
				"type named int\n\nfunc (n named) Name() int { return int(n) }\n\n" +
				"func main() {\n\tif " + tc.call + " == 12345 {\n\t\tpanic(\"no\")\n\t}\n}\n"
			_, err := compileSource(t, src, func(c *Config) {
				c.ImportCfgFile = cfgFile
				c.ImportCfg = mustReadImportCfg(t, cfgFile)
			})
			if err == nil {
				t.Fatalf("%s was built rather than refused", tc.what)
			}
			// The declaration, because the body is in no file of this build.
			if !strings.Contains(err.Error(), "nanogo.example/gen/refuse.") {
				t.Errorf("the refusal does not name the declaration: %v", err)
			}
			// The construct, because a refusal that named only the
			// declaration would be indistinguishable from the walk stopping
			// for some other reason.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name %q: %v", tc.want, err)
			}
		})
	}
}

// TestCompileStencilsTheConstructsTheForeignWalkMaps runs each mapped
// construct through a real archive.
//
// internal/e2e proves the answers are gc's, and it drives nanogo as a separate
// process, so none of the walk appears in this package's coverage. These build
// in this process. Each line of the program below is one instantiation, and
// each instantiation is one shape of body the walk has to substitute through.
func TestCompileStencilsTheConstructsTheForeignWalkMaps(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := genericLibModule(t)
	archive := gcArchive(t, dir, "./lib")

	cfgFile := filepath.Join(t.TempDir(), "importcfg")
	if err := os.WriteFile(cfgFile, []byte("packagefile nanogo.example/gen/lib="+archive+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := `package main

import "nanogo.example/gen/lib"

func main() {
	n := 3
	d := lib.Field(lib.Box[int]{V: 1, N: 4}) - 4
	d += lib.Negate(5) + 5
	d += lib.Complement(^7) - 7
	d += lib.Deref(&n) - 3
	d += lib.ArrayAt([3]int{1, 2, 3}, 1) - 2
	d += lib.Arith(6, 3) - 18
	d += lib.Shifted(4, 2) - 16
	d += lib.Global(9) - 9
	d += lib.Guarded(-2) + 1
	d += lib.ReadVar(4) - 15
	d += lib.SliceAt([]int{7, 8}, 1) - 8
	d += lib.PtrField(&lib.Box[int]{V: 1, N: 6}) - 6
	d += lib.Promoted(lib.Outer[int]{Box: lib.Box[int]{V: 1, N: 5}, M: 2}) - 5
	d += lib.PtrArrayAt(&[3]int{1, 2, 3}, 2) - 3
	d += int(lib.StrAt("abc", 1)) - 98
	d += lib.SliceAll([]int{1, 2, 3, 4}) - 4
	d += lib.SliceLo([]int{1, 2, 3, 4}) - 3
	d += lib.SliceHi([]int{1, 2, 3, 4}) - 2
	d += lib.SliceLoHi([]int{1, 2, 3, 4}) - 2
	d += lib.SliceMax([]int{1, 2, 3, 4}) - 3
	d += lib.SliceVar([]int{1, 2, 3, 4}, 1, 4) - 3
	d += lib.SliceVarMax([]int{1, 2, 3, 4}, 1, 2, 4) - 3
	d += lib.SliceStrAll("abcd") - 4
	d += lib.SliceStrLo("abcd") - 2
	d += lib.SliceStrHi("abcd") - 2
	d += lib.SliceStrLoHi("abcd") - 2
	d += lib.SliceArrAll([4]int{1, 2, 3, 4}) - 4
	d += lib.SliceArrLoHi([4]int{1, 2, 3, 4}) - 2
	d += lib.SliceArrMax([4]int{1, 2, 3, 4}) - 3
	d += lib.SlicePtrArrAll(&[4]int{1, 2, 3, 4}) - 4
	d += lib.SlicePtrArrLoHi(&[4]int{1, 2, 3, 4}) - 2
	d += lib.SlicePtrArrMax(&[4]int{1, 2, 3, 4}) - 3
	d += lib.SliceField(lib.Arr[int]{A: [4]int{1, 2, 3, 4}}) - 3
	d += lib.SliceLocal(7) - 6
	d += lib.SliceValue([]int{5, 6, 7, 8}) - 6
	d += int(lib.SliceStrValue("abcd")) - 98
	d += lib.SliceArrValue([4]int{1, 2, 3, 4}, 9) - 9
	d += *lib.Addr(4) - 4
	d += lib.Variadic(1, 2, 3) - 3
	d += lib.CallVariadic(2, 3) - 5
	d += lib.SpreadVariadic([]int{1, 2}) - 2
	d += lib.Length([]int{1, 2, 3}) - 3
	d += lib.Capacity(make([]int, 2, 5)) - 5
	d += lib.Sum([]int{1, 2, 3, 4}) - 10
	d += lib.Counted([]int{1, 2, 3, 2, 5}, 2) - 3
	d += lib.Classify(0) + lib.Classify(2) + lib.Classify(7) - 186
	d += lib.Pair(4) - 1
	d += lib.ShiftAssign(3) - 12
	d += lib.Bumped(5) - 13
	d += lib.Capture(6) - 13
	d += lib.Nested(6) - 8
	rev := []int{1, 2, 3, 4}
	lib.Reverse(rev)
	d += rev[0] - 4
	if lib.Not(false) {
		d += 0
	}
	if d != 0 {
		d = d / (d - d)
	}
}
`
	if _, err := compileSource(t, src, func(c *Config) {
		c.ImportCfgFile = cfgFile
		c.ImportCfg = mustReadImportCfg(t, cfgFile)
	}); err != nil {
		t.Fatalf("a mapped construct was refused: %v", err)
	}
}
