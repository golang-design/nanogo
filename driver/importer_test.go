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
