// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package loader

import (
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// moduleDir returns nanogo's module root, which is the parent of this package.
func moduleDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

// needGo skips the test when there is no go command.
func needGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
}

// hostEnv returns the environment for a load pinned to the host target with
// cgo off, so that the file lists do not depend on the caller's settings.
func hostEnv() []string {
	return append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+runtime.GOOS,
		"GOARCH="+runtime.GOARCH,
	)
}

func TestGoListLoad(t *testing.T) {
	needGo(t)
	g := NewGoList(moduleDir(t))
	g.Env = hostEnv()
	pkgs, err := g.Load("golang.design/x/nanogo/loader")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) < 2 {
		t.Fatalf("loaded %d packages, want the package and its dependencies", len(pkgs))
	}

	// specs/053-determinism.md: the result is sorted by import path.
	for i := 1; i < len(pkgs); i++ {
		if pkgs[i-1].ImportPath >= pkgs[i].ImportPath {
			t.Fatalf("packages are not sorted: %q then %q", pkgs[i-1].ImportPath, pkgs[i].ImportPath)
		}
	}

	var target *Package
	for _, p := range pkgs {
		if p.ImportPath == "golang.design/x/nanogo/loader" {
			target = p
		}
		if p.Err != nil {
			t.Errorf("%s: %v", p.ImportPath, p.Err)
		}
	}
	if target == nil {
		t.Fatal("the loaded package is not in the result")
	}
	if target.Name != "loader" {
		t.Errorf("Name is %q, want loader", target.Name)
	}
	if target.Standard {
		t.Error("the loader package is marked standard")
	}
	if target.Dir == "" {
		t.Error("Dir is empty")
	}
	if len(target.GoFiles) == 0 {
		t.Error("GoFiles is empty")
	}
	if !contains(target.ImportPaths, "go/build/constraint") {
		t.Errorf("ImportPaths is %v, want go/build/constraint in it", target.ImportPaths)
	}
	dep := target.Imports["go/build/constraint"]
	if dep == nil {
		t.Fatal("the import of go/build/constraint is not resolved to a package")
	}
	if !dep.Standard {
		t.Error("go/build/constraint is not marked standard")
	}
	if dep.Export == "" {
		t.Error("Export is empty although the load asked for export data")
	}
	if !contains(target.Deps, "sort") {
		t.Errorf("Deps is %v, want sort in it", target.Deps)
	}
	if !sort.StringsAreSorted(target.Deps) {
		t.Error("Deps is not sorted")
	}
	if !sort.StringsAreSorted(target.ImportPaths) {
		t.Error("ImportPaths is not sorted")
	}
}

// TestGoListDeterminism checks the rule of specs/053-determinism.md on the
// loader's own output path: the same patterns give the same order.
func TestGoListDeterminism(t *testing.T) {
	needGo(t)
	g := NewGoList(moduleDir(t))
	g.Export = false
	g.Env = hostEnv()

	first, err := g.Load("golang.design/x/nanogo/loader")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		next, err := g.Load("golang.design/x/nanogo/loader")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(paths(first), paths(next)) {
			t.Fatalf("run %d ordered packages differently", i)
		}
		for j := range first {
			if !reflect.DeepEqual(first[j].GoFiles, next[j].GoFiles) {
				t.Fatalf("%s: GoFiles differ between runs", first[j].ImportPath)
			}
			if !reflect.DeepEqual(first[j].ImportPaths, next[j].ImportPaths) {
				t.Fatalf("%s: ImportPaths differ between runs", first[j].ImportPath)
			}
			if !reflect.DeepEqual(first[j].Deps, next[j].Deps) {
				t.Fatalf("%s: Deps differ between runs", first[j].ImportPath)
			}
		}
	}

	// The topological order is stable across loads too.
	a, err := TopoSort(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.Load("golang.design/x/nanogo/loader")
	if err != nil {
		t.Fatal(err)
	}
	b, err := TopoSort(second)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths(a), paths(b)) {
		t.Error("TopoSort of two loads of the same patterns differs")
	}
}

// TestGoListTags checks that -tags reaches the go command and changes the file
// set.
func TestGoListTags(t *testing.T) {
	needGo(t)
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/tagged\n\ngo 1.27\n")
	write("always.go", "package tagged\n")
	write("tagged.go", "//go:build extra\n\npackage tagged\n")
	write("legacy.go", "// +build extra\n\npackage tagged\n")

	load := func(tags ...string) *Package {
		t.Helper()
		g := &GoList{Dir: dir, Tags: tags, Env: hostEnv()}
		pkgs, err := g.Load("example.com/tagged")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkgs {
			if p.ImportPath == "example.com/tagged" {
				return p
			}
		}
		t.Fatal("the package is not in the result")
		return nil
	}

	off := load()
	if !reflect.DeepEqual(off.GoFiles, []string{"always.go"}) {
		t.Errorf("without the tag GoFiles is %v, want [always.go]", off.GoFiles)
	}
	if len(off.IgnoredGoFiles) != 2 {
		t.Errorf("without the tag IgnoredGoFiles is %v, want two files", off.IgnoredGoFiles)
	}

	on := load("extra")
	if !reflect.DeepEqual(on.GoFiles, []string{"always.go", "legacy.go", "tagged.go"}) {
		t.Errorf("with the tag GoFiles is %v, want all three files", on.GoFiles)
	}

	// The constraint evaluator must reach the same answer as the go command.
	c := DefaultContext()
	c.BuildTags = []string{"extra"}
	for _, name := range on.GoFiles {
		ok, err := c.IncludeFile(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("the evaluator rejects %s, which the go command keeps", name)
		}
	}
	c.BuildTags = nil
	for _, name := range []string{"tagged.go", "legacy.go"} {
		ok, err := c.IncludeFile(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("the evaluator keeps %s without the tag", name)
		}
	}
}

// TestGoListPackageError checks the rule of specs/014-package-loader.md that a
// per-package error is attached to the package and does not fail the load.
func TestGoListPackageError(t *testing.T) {
	needGo(t)
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/broken\n\ngo 1.27\n",
		"a.go":   "package broken\n\nimport _ \"example.com/broken/absent\"\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	g := &GoList{Dir: dir, Env: hostEnv()}
	pkgs, err := g.Load("example.com/broken")
	if err != nil {
		t.Fatalf("a per-package error failed the whole load: %v", err)
	}
	var withError int
	for _, p := range pkgs {
		if p.Err != nil {
			withError++
			if p.Err.Error() == "" {
				t.Errorf("%s: the error has no message", p.ImportPath)
			}
		}
	}
	if withError == 0 {
		t.Fatal("no package carries the error for the missing import")
	}
}

func TestGoListNoGoCommand(t *testing.T) {
	g := &GoList{Cmd: "nanogo-no-such-go-binary"}
	_, err := g.Load("std")
	if err == nil {
		t.Fatal("a missing go command gave no error")
	}
	if !strings.Contains(err.Error(), "no go command") {
		t.Errorf("error is %q, want it to say the go command is missing", err)
	}
}

// TestGoListCommandFailure checks that a failure of the go command itself
// carries stderr, which is where the reason is.
func TestGoListCommandFailure(t *testing.T) {
	needGo(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("this is not a go.mod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &GoList{Dir: dir, Env: hostEnv()}
	_, err := g.Load("./...")
	if err == nil {
		t.Fatal("a broken module gave no error")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("error is %q, want the go command's stderr in it", err)
	}
}

func TestDecodeList(t *testing.T) {
	// go list writes concatenated objects and not an array.
	const stream = `{"ImportPath":"b","Name":"b","Imports":["a"],"Deps":["a"]}
{"ImportPath":"a","Name":"a"}
{"ImportPath":"a","Name":"a"}
{"Name":"no path"}
`
	pkgs, err := decodeList(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("decoded %d packages, want 2", len(pkgs))
	}
	link(pkgs)
	SortPackages(pkgs)
	if !reflect.DeepEqual(paths(pkgs), []string{"a", "b"}) {
		t.Errorf("decoded %v, want [a b]", paths(pkgs))
	}
	if pkgs[1].Imports["a"] != pkgs[0] {
		t.Error("the import of a is not linked to the decoded package")
	}
}

func TestDecodeListErrors(t *testing.T) {
	const stream = `{"ImportPath":"a","Error":{"Pos":"a/x.go:1:1","Err":"broken"}}
{"ImportPath":"b","Imports":["a"],"DepsErrors":[{"Err":"broken dependency"}]}
`
	pkgs, err := decodeList(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	SortPackages(pkgs)
	if got, want := pkgs[0].Err.Error(), "a/x.go:1:1: broken"; got != want {
		t.Errorf("error is %q, want %q", got, want)
	}
	if got, want := pkgs[1].Err.Error(), "b: broken dependency"; got != want {
		t.Errorf("dependency error is %q, want %q", got, want)
	}
}

func TestDecodeListMalformed(t *testing.T) {
	if _, err := decodeList(strings.NewReader(`{"ImportPath":`)); err == nil {
		t.Fatal("malformed JSON gave no error")
	}
}

// TestGoListDifferential is the differential test of
// specs/014-package-loader.md. For a real tree it asserts that nanogo's
// constraint evaluator puts every file on the same side as the go command.
//
// The tree is the GOROOT of the go command, because the go command is the
// oracle only for its own GOROOT. The evaluator is also checked against
// go/build over ~/dev/go.dev/go/src in constraint_test.go, which needs no go
// command and so has no such restriction.
func TestGoListDifferential(t *testing.T) {
	if testing.Short() {
		t.Skip("the differential test runs the go command over a whole tree")
	}
	needGo(t)

	for _, pattern := range []string{"std", "golang.design/x/nanogo/..."} {
		t.Run(strings.TrimSuffix(strings.TrimSuffix(pattern, "/..."), "..."), func(t *testing.T) {
			g := NewGoList(moduleDir(t))
			g.Export = false // file lists do not need a build
			g.Env = hostEnv()
			pkgs, err := g.Load(pattern)
			if err != nil {
				t.Fatal(err)
			}
			if len(pkgs) == 0 {
				t.Fatal("the load returned no packages")
			}

			c := DefaultContext()
			c.CgoEnabled = false
			// The experiment and target feature tags are caller supplied.
			// The go command sets its own, so the loader is told the same
			// set. See the Context documentation.
			// go/build computes them the same way the go command does.
			c.ToolTags = build.Default.ToolTags

			var checked, files, mismatches int
			for _, p := range pkgs {
				if p.Err != nil || p.Dir == "" {
					continue
				}
				n, bad := comparePackageFiles(t, c, p)
				checked++
				files += n
				mismatches += bad
			}
			if checked == 0 {
				t.Fatal("no package was checked")
			}
			t.Logf("%s: %d packages, %d files, %d mismatches", pattern, checked, files, mismatches)
		})
	}
}

// comparePackageFiles asserts that the evaluator agrees with the go command
// about every .go file in the package directory. It returns the number of
// files checked and the number that disagreed.
func comparePackageFiles(t *testing.T, c *Context, p *Package) (files, mismatches int) {
	t.Helper()
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		t.Errorf("%s: %v", p.ImportPath, err)
		return 0, 0
	}

	// The go command reports every .go file of the directory in exactly one
	// of these lists. Assert the partition first: if it does not hold, the
	// field list here is incomplete and every later comparison is noise.
	kept := make(map[string]bool)
	for _, name := range p.GoFiles {
		kept[name] = true
	}
	for _, name := range p.CgoFiles {
		kept[name] = true
	}
	for _, name := range p.TestGoFiles {
		kept[name] = true
	}
	for _, name := range p.XTestGoFiles {
		kept[name] = true
	}
	rejected := make(map[string]bool)
	for _, name := range p.IgnoredGoFiles {
		rejected[name] = true
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// A leading _ or . excludes the file entirely, and the go command
		// does not report it at all.
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			if c.MatchName(name) {
				t.Errorf("%s: the evaluator keeps %s, which has an excluding prefix", p.ImportPath, name)
				mismatches++
			}
			continue
		}
		if !kept[name] && !rejected[name] {
			// The go command knows a file the partition does not cover.
			// Report it rather than guessing which side it belongs to.
			t.Errorf("%s: %s is in no file list of the go command", p.ImportPath, name)
			mismatches++
			continue
		}

		files++
		got, err := c.IncludeFile(p.Dir, name)
		if err != nil {
			t.Errorf("%s: %s: %v", p.ImportPath, name, err)
			mismatches++
			continue
		}
		if got != kept[name] {
			t.Errorf("%s: the evaluator says %v for %s, the go command says %v",
				p.ImportPath, got, name, kept[name])
			mismatches++
		}
	}
	return files, mismatches
}

// TestGoListTestVariant checks that -test reaches the go command.
// specs/014-package-loader.md makes a test variant a distinct package, so the
// loader must be able to ask for one.
func TestGoListTestVariant(t *testing.T) {
	needGo(t)
	g := &GoList{Dir: moduleDir(t), Tests: true, Env: hostEnv()}
	pkgs, err := g.Load("golang.design/x/nanogo/loader")
	if err != nil {
		t.Fatal(err)
	}
	var variants int
	for _, p := range pkgs {
		if strings.HasPrefix(p.ImportPath, "golang.design/x/nanogo/loader") {
			variants++
		}
	}
	if variants < 2 {
		t.Errorf("with -test the load returned %d variants of the package, want the package and its tests", variants)
	}
}

// fakeGo writes a shell script that stands in for the go command.
func fakeGo(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakego")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGoListMalformedOutput checks that output the decoder cannot read fails
// the load with a message, and does not return a half-decoded graph.
func TestGoListMalformedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test uses a shell script")
	}
	g := &GoList{Cmd: fakeGo(t, `printf '{"ImportPath":'`)}
	_, err := g.Load("std")
	if err == nil {
		t.Fatal("malformed output gave no error")
	}
	if !strings.Contains(err.Error(), "decoding go list output") {
		t.Errorf("error is %q, want it to name the decoding step", err)
	}
}

// TestGoListSilentFailure checks the message when the go command fails and
// says nothing on stderr.
func TestGoListSilentFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test uses a shell script")
	}
	g := &GoList{Cmd: fakeGo(t, "exit 3")}
	_, err := g.Load("std")
	if err == nil {
		t.Fatal("a failing go command gave no error")
	}
	if !strings.Contains(err.Error(), "list -e -json -deps") {
		t.Errorf("error is %q, want the command line in it", err)
	}
}

func TestConvertErrorNil(t *testing.T) {
	if got := convertError("a", nil); got != nil {
		t.Errorf("convertError with no error returned %v", got)
	}
}
