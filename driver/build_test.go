// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.design/x/nanogo/loader"
)

func TestParseBuildArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want buildOptions
	}{
		{"no arguments", nil, buildOptions{Patterns: []string{"."}}},
		{"one pattern", []string{"./..."}, buildOptions{Patterns: []string{"./..."}}},
		{"two patterns", []string{"a", "b"}, buildOptions{Patterns: []string{"a", "b"}}},
		{"output space", []string{"-o", "prog"}, buildOptions{Output: "prog", Patterns: []string{"."}}},
		{"output equals", []string{"-o=prog", "."}, buildOptions{Output: "prog", Patterns: []string{"."}}},
		{"double dash flag", []string{"--o", "prog", "."}, buildOptions{Output: "prog", Patterns: []string{"."}}},
		{"verbose", []string{"-v", "."}, buildOptions{Verbose: true, Patterns: []string{"."}}},
		{"verbose false", []string{"-v=false", "."}, buildOptions{Patterns: []string{"."}}},
		{"work", []string{"-work"}, buildOptions{Work: true, Patterns: []string{"."}}},
		{"all", []string{"-v", "-work", "-o", "x", "./..."},
			buildOptions{Output: "x", Verbose: true, Work: true, Patterns: []string{"./..."}}},
		// A file argument that starts with a dot is a pattern and not a flag.
		{"file list", []string{"./hello.go"}, buildOptions{Patterns: []string{"./hello.go"}}},
		// After --, everything is a pattern. A directory called -v exists.
		{"terminator", []string{"--", "-v"}, buildOptions{Patterns: []string{"-v"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBuildArgs(tt.args)
			if err != nil {
				t.Fatalf("ParseBuildArgs(%q): %v", tt.args, err)
			}
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("ParseBuildArgs(%q) = %+v, want %+v", tt.args, *got, tt.want)
			}
		})
	}
}

func TestParseBuildArgsRejects(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"-x"}, `unknown flag "-x"`},
		{"output with no value", []string{"-o"}, "flag -o needs a value"},
		{"empty output", []string{"-o="}, "flag -o needs a value"},
		{"verbose is not a count", []string{"-v=maybe"}, "flag -v"},
		{"work is not a count", []string{"-work=maybe"}, "flag -work"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseBuildArgs(tt.args)
			if err == nil {
				t.Fatalf("ParseBuildArgs(%q) accepted the line", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseBuildArgs(%q) = %v, want it to mention %q", tt.args, err, tt.want)
			}
		})
	}
}

func TestDefaultOutputName(t *testing.T) {
	tests := []struct {
		name string
		pkg  loader.Package
		want string
	}{
		{"import path", loader.Package{ImportPath: "example.com/m/cmd/hello"}, "hello"},
		{"module root", loader.Package{ImportPath: "example.com/hello"}, "hello"},
		{"file list", loader.Package{ImportPath: commandLineArguments, GoFiles: []string{"hello.go", "b.go"}}, "hello"},
		{"file list with a path", loader.Package{ImportPath: commandLineArguments,
			GoFiles: []string{filepath.Join("sub", "hello.go")}}, "hello"},
		{"nothing to name it after", loader.Package{ImportPath: commandLineArguments, Name: "main"}, "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultOutputName(&tt.pkg); got != tt.want {
				t.Errorf("defaultOutputName = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCompilePackageNameIsMainForAMainPackage is the rule that the allowlist
// got wrong. The go command sends -p main for every main package, and the
// linker looks for main.main, so a main package compiled under its import path
// defines a symbol nothing calls.
func TestCompilePackageNameIsMainForAMainPackage(t *testing.T) {
	main := &loader.Package{ImportPath: "example.com/hello", Name: "main"}
	if got := compilePackageName(main); got != "main" {
		t.Errorf("compilePackageName(a main package) = %q, want %q", got, "main")
	}
	lib := &loader.Package{ImportPath: "example.com/hello/lib", Name: "lib"}
	if got := compilePackageName(lib); got != lib.ImportPath {
		t.Errorf("compilePackageName(a library) = %q, want %q", got, lib.ImportPath)
	}
}

func TestSourceFilesAreAbsolute(t *testing.T) {
	abs := filepath.Join(string(filepath.Separator), "elsewhere", "z.go")
	p := &loader.Package{Dir: filepath.Join(string(filepath.Separator), "m"), GoFiles: []string{"a.go", abs}}
	want := []string{filepath.Join(string(filepath.Separator), "m", "a.go"), abs}
	if got := sourceFiles(p); !reflect.DeepEqual(got, want) {
		t.Errorf("sourceFiles = %q, want %q", got, want)
	}
}

// testBuilder returns a builder whose four seams are fakes, so that the
// decisions the command makes are driven without a go command, a file system
// walk or a compiler run.
func testBuilder(t *testing.T, opts *buildOptions) (*builder, *bytes.Buffer) {
	t.Helper()
	stderr := &bytes.Buffer{}
	env := &Env{
		Stdout: io.Discard,
		Stderr: stderr,
		Getenv: func(string) string { return "" },
	}
	b := &builder{
		env:   env,
		opts:  opts,
		dir:   t.TempDir(),
		work:  t.TempDir(),
		goCmd: "go",
	}
	b.findRoot = func(goroot string) (Root, error) {
		return Root{Path: goroot, Origin: "the installed Go toolchain"}, nil
	}
	b.compile = func(*Config) error { return nil }
	b.runGo = func(args ...string) ([]byte, error) {
		return nil, fmt.Errorf("the test did not expect go %s", strings.Join(args, " "))
	}
	b.loadPackages = func(bool, []string) ([]*loader.Package, error) {
		return nil, errors.New("the test did not expect a package listing")
	}
	return b, stderr
}

func TestToolchainReadsGoEnv(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{})
	b.runGo = func(args ...string) ([]byte, error) {
		if want := "env GOROOT GOVERSION"; strings.Join(args, " ") != want {
			t.Errorf("go %s, want go %s", strings.Join(args, " "), want)
		}
		return []byte("/usr/local/go\r\ngo1.27.0\r\n"), nil
	}
	root, version, err := b.toolchain()
	if err != nil {
		t.Fatal(err)
	}
	if root != "/usr/local/go" || version != "go1.27.0" {
		t.Errorf("toolchain = %q, %q", root, version)
	}
}

func TestToolchainRejectsAShortAnswer(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{})
	b.runGo = func(...string) ([]byte, error) { return []byte("/usr/local/go"), nil }
	if _, _, err := b.toolchain(); err == nil {
		t.Fatal("a one line answer was accepted")
	}
	b.runGo = func(...string) ([]byte, error) { return nil, errors.New("no go command") }
	if _, _, err := b.toolchain(); err == nil {
		t.Fatal("a failing go command was accepted")
	}
}

func TestRootPathsReadsTheModuleLanguageVersion(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{Patterns: []string{"./..."}})
	b.runGo = func(args ...string) ([]byte, error) {
		if args[0] != "list" || args[len(args)-1] != "./..." {
			t.Errorf("go %s", strings.Join(args, " "))
		}
		return []byte("example.com/m\t1.27\nexample.com/m/lib\t1.27\n"), nil
	}
	names, lang, err := b.rootPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.com/m", "example.com/m/lib"}; !reflect.DeepEqual(names, want) {
		t.Errorf("rootPaths = %q, want %q", names, want)
	}
	if lang != "go1.27" {
		t.Errorf("lang = %q, want %q", lang, "go1.27")
	}
}

func TestRootPathsOutsideAModule(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{Patterns: []string{"./x.go"}})
	b.runGo = func(...string) ([]byte, error) { return []byte(commandLineArguments + "\t\n"), nil }
	names, lang, err := b.rootPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != commandLineArguments {
		t.Errorf("rootPaths = %q", names)
	}
	// An empty -lang disables the language version checks, which is what gc
	// does when there is no module to read a go directive from.
	if lang != "" {
		t.Errorf("lang = %q, want it empty", lang)
	}
}

func TestRootPathsRejectsAnEmptyMatch(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{Patterns: []string{"./nothing"}})
	b.runGo = func(...string) ([]byte, error) { return []byte("\n \n"), nil }
	_, _, err := b.rootPaths()
	if err == nil || !strings.Contains(err.Error(), "no packages match") {
		t.Fatalf("rootPaths = %v, want it to report an empty match", err)
	}
	b.runGo = func(...string) ([]byte, error) { return nil, errors.New("no such directory") }
	if _, _, err := b.rootPaths(); err == nil {
		t.Fatal("a failing listing was accepted")
	}
}

// TestCheckTargetsRefusesByName is the requirement that separates nanogo build
// from the allowlist: a package the user named is never handed to gc, so every
// case here must be an error that says which package and why.
func TestCheckTargetsRefusesByName(t *testing.T) {
	tests := []struct {
		name    string
		targets []*loader.Package
		want    string
	}{
		{"cgo", []*loader.Package{{ImportPath: "m", Name: "m", GoFiles: []string{"a.go"}, CgoFiles: []string{"c.go"}}},
			`imports "C"`},
		{"assembly", []*loader.Package{{ImportPath: "m", Name: "m", GoFiles: []string{"a.go"}, SFiles: []string{"a.s"}}},
			"assembly"},
		{"no files", []*loader.Package{{ImportPath: "m", Name: "m"}}, "no Go files"},
		{"a broken package", []*loader.Package{{ImportPath: "m", Err: &loader.Error{ImportPath: "m", Msg: "no such package"}}},
			"no such package"},
		{"one target imports another", []*loader.Package{
			{ImportPath: "m", Name: "main", GoFiles: []string{"a.go"},
				ImportPaths: []string{"m/lib"}, Deps: []string{"m/lib"}},
			{ImportPath: "m/lib", Name: "lib", GoFiles: []string{"b.go"}},
		}, "no export data"},
		// The edge may be transitive. dependencies drops a target from the
		// closure, so an unchecked transitive edge would reach go tool link as
		// a missing package instead of a diagnostic.
		{"one target depends on another through a third", []*loader.Package{
			{ImportPath: "m", Name: "main", GoFiles: []string{"a.go"},
				ImportPaths: []string{"m/mid"}, Deps: []string{"m/mid", "m/leaf"}},
			{ImportPath: "m/leaf", Name: "leaf", GoFiles: []string{"c.go"}},
		}, "m/leaf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := testBuilder(t, &buildOptions{})
			err := b.checkTargets(tt.targets)
			if err == nil {
				t.Fatal("the package was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %v does not mention %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "m") {
				t.Errorf("error %v does not name the package", err)
			}
		})
	}
}

func TestCheckTargetsAcceptsAPlainPackage(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{})
	targets := []*loader.Package{{ImportPath: "m", Name: "main", GoFiles: []string{"a.go"}, ImportPaths: []string{"fmt"}}}
	if err := b.checkTargets(targets); err != nil {
		t.Fatalf("checkTargets refused a plain package: %v", err)
	}
}

func TestDependenciesResolveArchives(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{})
	var asked []string
	b.loadPackages = func(export bool, patterns []string) ([]*loader.Package, error) {
		if !export {
			t.Error("the dependency listing must ask for the archives")
		}
		asked = patterns
		return []*loader.Package{
			{ImportPath: "fmt", Export: "/cache/fmt.a"},
			{ImportPath: "internal/abi", Export: "/cache/abi.a"},
		}, nil
	}
	targets := []*loader.Package{{ImportPath: "m", Deps: []string{"fmt", "internal/abi", unsafePkg, "m"}}}
	archives, paths, err := b.dependencies(targets)
	if err != nil {
		t.Fatal(err)
	}
	// unsafe has no archive and the target is not its own dependency. The
	// order is sorted, so two runs write the same import configuration.
	want := []string{"fmt", "internal/abi"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("dependency paths = %q, want %q", paths, want)
	}
	if !reflect.DeepEqual(asked, want) {
		t.Errorf("the listing asked for %q, want %q", asked, want)
	}
	if archives["fmt"] != "/cache/fmt.a" {
		t.Errorf("fmt resolved to %q", archives["fmt"])
	}
}

func TestDependenciesOfAPackageThatImportsNothing(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{})
	// The seam fails the test if it is called. go list with no pattern lists
	// the current directory, which would answer with the target itself.
	archives, paths, err := b.dependencies([]*loader.Package{{ImportPath: "m", Deps: []string{unsafePkg}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 || len(archives) != 0 {
		t.Errorf("dependencies = %q, %v, want both empty", paths, archives)
	}
}

// TestDependenciesRefuseAMissingArchive is the case that would otherwise reach
// the user as an undefined symbol from the linker.
func TestDependenciesRefuseAMissingArchive(t *testing.T) {
	tests := []struct {
		name   string
		listed []*loader.Package
		want   string
	}{
		{"not described", nil, "did not describe"},
		{"broken", []*loader.Package{{ImportPath: "fmt", Err: &loader.Error{ImportPath: "fmt", Msg: "build failed"}}},
			"build failed"},
		{"no archive", []*loader.Package{{ImportPath: "fmt"}}, "built no archive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := testBuilder(t, &buildOptions{})
			b.loadPackages = func(bool, []string) ([]*loader.Package, error) { return tt.listed, nil }
			_, _, err := b.dependencies([]*loader.Package{{ImportPath: "m", Deps: []string{"fmt"}}})
			if err == nil {
				t.Fatal("the dependency was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "fmt") {
				t.Errorf("error %v does not name the package and the reason %q", err, tt.want)
			}
		})
	}
}

func TestWriteImportCfgKeepsTheGivenOrder(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{})
	paths := []string{"b", "a"}
	archives := map[string]string{"a": "/cache/a.a", "b": "/cache/b.a"}
	file, err := b.writeImportCfg("importcfg", paths, archives, []buildPackage{{Path: "m", Archive: "/work/0.a"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "packagefile b=/cache/b.a\npackagefile a=/cache/a.a\npackagefile m=/work/0.a\n"
	if string(got) != want {
		t.Errorf("importcfg =\n%s\nwant\n%s", got, want)
	}
}

func TestCheckOutputRefusesAnOutputItCannotName(t *testing.T) {
	tests := []struct {
		name    string
		targets []*loader.Package
		want    string
	}{
		{"no main package", []*loader.Package{{ImportPath: "m/lib", Name: "lib"}}, "none of the packages is a main"},
		{"two main packages", []*loader.Package{
			{ImportPath: "m/a", Name: "main"}, {ImportPath: "m/b", Name: "main"},
		}, "2 of the packages are main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := testBuilder(t, &buildOptions{Output: "prog"})
			err := b.checkOutput(mainPackages(tt.targets))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("checkOutput = %v, want it to mention %q", err, tt.want)
			}
			// Without -o there is nothing to name, so both lists are legal.
			b.opts.Output = ""
			if err := b.checkOutput(mainPackages(tt.targets)); err != nil {
				t.Errorf("checkOutput without -o: %v", err)
			}
		})
	}
}

// TestLinkSkipsALibrary is what go build does: a non-main package is compiled
// and no executable comes out of it.
func TestLinkSkipsALibrary(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{})
	if err := b.link(mainPackages([]*loader.Package{{ImportPath: "m/lib", Name: "lib"}}), nil, nil, nil); err != nil {
		t.Fatalf("link over a library: %v", err)
	}
}

func TestOutputPathIsRelativeToTheWorkingDirectory(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{})
	target := &loader.Package{ImportPath: "example.com/hello", Name: "main"}
	if got, want := b.outputPath(target), filepath.Join(b.dir, "hello"); got != want {
		t.Errorf("outputPath = %q, want %q", got, want)
	}
	b.opts.Output = filepath.Join(string(filepath.Separator), "tmp", "prog")
	if got := b.outputPath(target); got != b.opts.Output {
		t.Errorf("outputPath = %q, want the absolute -o unchanged", got)
	}
	b.opts.Output = "bin/prog"
	if got, want := b.outputPath(target), filepath.Join(b.dir, "bin", "prog"); got != want {
		t.Errorf("outputPath = %q, want %q", got, want)
	}
}

// fakeBuild wires every seam for one whole run over a one package module.
type fakeBuild struct {
	b        *builder
	stderr   *bytes.Buffer
	compiled []*Config
	linked   [][]string
}

func newFakeBuild(t *testing.T, opts *buildOptions) *fakeBuild {
	t.Helper()
	f := &fakeBuild{}
	f.b, f.stderr = testBuilder(t, opts)
	dir := f.b.dir
	f.b.runGo = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "env":
			return []byte("/usr/local/go\ngo1.27.0\n"), nil
		case args[0] == "list":
			return []byte("example.com/hello\t1.27\n"), nil
		case args[0] == "tool" && args[1] == "link":
			f.linked = append(f.linked, args)
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected go %s", strings.Join(args, " "))
	}
	f.b.loadPackages = func(export bool, patterns []string) ([]*loader.Package, error) {
		if !export {
			return []*loader.Package{{
				ImportPath:  "example.com/hello",
				Name:        "main",
				Dir:         dir,
				GoFiles:     []string{"main.go"},
				ImportPaths: []string{"math/bits"},
				Deps:        []string{"math/bits", unsafePkg},
			}}, nil
		}
		return []*loader.Package{{ImportPath: "math/bits", Export: "/cache/bits.a"}}, nil
	}
	f.b.compile = func(cfg *Config) error {
		f.compiled = append(f.compiled, cfg)
		return os.WriteFile(cfg.Output, nil, 0o600)
	}
	return f
}

// TestBuildCompilesTheTargetAndLinksIt is the command in one piece, with the
// go command and the compiler replaced by fakes.
func TestBuildCompilesTheTargetAndLinksIt(t *testing.T) {
	f := newFakeBuild(t, &buildOptions{Patterns: []string{"."}, Verbose: true})
	if err := f.b.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.compiled) != 1 {
		t.Fatalf("nanogo compiled %d packages, want 1", len(f.compiled))
	}
	cfg := f.compiled[0]
	if cfg.Package != "main" {
		t.Errorf("-p was %q, want main: the linker looks for main.main", cfg.Package)
	}
	if cfg.Lang != "go1.27" {
		t.Errorf("-lang was %q, want go1.27", cfg.Lang)
	}
	if !cfg.Pack || !cfg.Complete {
		t.Errorf("cfg.Pack = %v, cfg.Complete = %v, want both set", cfg.Pack, cfg.Complete)
	}
	if want := []string{filepath.Join(f.b.dir, "main.go")}; !reflect.DeepEqual(cfg.Files, want) {
		t.Errorf("files = %q, want %q", cfg.Files, want)
	}
	if file, ok := cfg.ImportCfg.PackageFile("math/bits"); !ok || file != "/cache/bits.a" {
		t.Errorf("the import configuration resolves math/bits to %q, %v", file, ok)
	}

	if len(f.linked) != 1 {
		t.Fatalf("the linker ran %d times, want 1", len(f.linked))
	}
	link := strings.Join(f.linked[0], " ")
	if !strings.Contains(link, "tool link") || !strings.Contains(link, filepath.Join(f.b.dir, "hello")) {
		t.Errorf("the link command was %q", link)
	}

	// The report is the honesty requirement: one package of two is nanogo's.
	out := f.stderr.String()
	for _, want := range []string{
		"1 of 2 packages compiled by nanogo",
		"1 by go1.27.0",
		"the standard library and the runtime come from /usr/local/go",
		"go tool link",
		"example.com/hello compiled by nanogo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not contain %q:\n%s", want, out)
		}
	}
}

// TestBuildReportsTheCountsWithoutVerbose keeps the one line that says how
// little of the program nanogo compiled. It is printed on every build.
func TestBuildReportsTheCountsWithoutVerbose(t *testing.T) {
	f := newFakeBuild(t, &buildOptions{Patterns: []string{"."}})
	if err := f.b.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := f.stderr.String()
	if !strings.Contains(out, "1 of 2 packages compiled by nanogo") {
		t.Errorf("a quiet build hides what it compiled:\n%s", out)
	}
	if strings.Contains(out, "compiling example.com/hello") {
		t.Errorf("a quiet build printed the per package lines:\n%s", out)
	}
}

// TestBuildStopsWhenTheCompilerRefuses is the failure mode the allowlist had.
// A package the user named must never reach gc.
func TestBuildStopsWhenTheCompilerRefuses(t *testing.T) {
	f := newFakeBuild(t, &buildOptions{Patterns: []string{"."}})
	refusal := &UnsupportedError{Package: "main", What: "function f at main.go:3:6", Detail: "ir.Lower: append"}
	f.b.compile = func(*Config) error { return refusal }
	err := f.b.run()
	if err == nil {
		t.Fatal("the build succeeded although the compiler refused the package")
	}
	if !errors.Is(err, error(refusal)) {
		t.Errorf("run returned %v, want the compiler's own refusal", err)
	}
	if len(f.linked) != 0 {
		t.Error("the linker ran after the compiler refused the package")
	}
}

func TestBuildRefusesANanogoDistributionItCannotRead(t *testing.T) {
	f := newFakeBuild(t, &buildOptions{Patterns: []string{"."}})
	f.b.findRoot = func(string) (Root, error) {
		return Root{Path: "/opt/nanogo", Origin: RootEnv, Nanogo: true}, nil
	}
	err := f.b.run()
	if err == nil {
		t.Fatal("a distribution nanogo cannot read was accepted")
	}
	// The alternative is to fall back to the toolchain's archives while
	// NANOGOROOT says the distribution was used, which is the one lie this
	// command exists to avoid.
	for _, want := range []string{"/opt/nanogo", "unbuilt", RootEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %q", err, want)
		}
	}
}

func TestBuildPropagatesSeamFailures(t *testing.T) {
	tests := []struct {
		name   string
		broken func(*fakeBuild)
	}{
		{"the toolchain", func(f *fakeBuild) {
			f.b.runGo = func(...string) ([]byte, error) { return nil, errors.New("no go command") }
		}},
		{"the root", func(f *fakeBuild) {
			f.b.findRoot = func(string) (Root, error) { return Root{}, errors.New("no root") }
		}},
		{"the graph", func(f *fakeBuild) {
			f.b.loadPackages = func(bool, []string) ([]*loader.Package, error) { return nil, errors.New("no graph") }
		}},
		{"the link", func(f *fakeBuild) {
			run := f.b.runGo
			f.b.runGo = func(args ...string) ([]byte, error) {
				if args[0] == "tool" {
					return nil, errors.New("the linker failed")
				}
				return run(args...)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeBuild(t, &buildOptions{Patterns: []string{"."}})
			tt.broken(f)
			if err := f.b.run(); err == nil {
				t.Fatalf("%s failed and the build reported success", tt.name)
			}
		})
	}
}

// TestBuildNamesAPackageItCannotDescribe covers the listing that names a
// package the graph does not carry, which would otherwise be a nil target.
func TestBuildNamesAPackageItCannotDescribe(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{Patterns: []string{"."}})
	b.runGo = func(...string) ([]byte, error) { return []byte("example.com/ghost\t1.27\n"), nil }
	b.loadPackages = func(bool, []string) ([]*loader.Package, error) { return nil, nil }
	_, _, err := b.targets()
	if err == nil || !strings.Contains(err.Error(), "example.com/ghost") {
		t.Fatalf("targets = %v, want it to name the package", err)
	}
}

func TestRunBuildRejectsABadCommandLine(t *testing.T) {
	stderr := &bytes.Buffer{}
	code := Run(Env{
		Args:   []string{"build", "-x"},
		Stdout: io.Discard,
		Stderr: stderr,
		Getenv: func(string) string { return "" },
	})
	if code != 2 {
		t.Errorf("exit status %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown flag") {
		t.Errorf("stderr %q does not name the flag", stderr.String())
	}
}

// TestCleanupKeepsTheWorkDirectoryUnderWork covers both halves of -work.
func TestCleanupKeepsTheWorkDirectoryUnderWork(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{Work: true})
	b.cleanup()
	if _, err := os.Stat(b.work); err != nil {
		t.Errorf("-work removed the scratch directory: %v", err)
	}
	b.opts.Work = false
	b.cleanup()
	if _, err := os.Stat(b.work); !os.IsNotExist(err) {
		t.Errorf("the scratch directory survived: %v", err)
	}
	b.work = ""
	b.cleanup()
}

// TestNewBuilderOpensAScratchDirectory covers the constructor, which is the
// one place the four seams are bound to their production implementations.
func TestNewBuilderOpensAScratchDirectory(t *testing.T) {
	needGoCommand(t)
	stderr := &bytes.Buffer{}
	env := &Env{Stdout: io.Discard, Stderr: stderr, Getenv: func(string) string { return "" }}
	b, err := newBuilder(env, &buildOptions{Work: true, Patterns: []string{"."}})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(b.work)
	if _, err := os.Stat(b.work); err != nil {
		t.Errorf("the scratch directory was not created: %v", err)
	}
	if !strings.Contains(stderr.String(), "WORK="+b.work) {
		t.Errorf("-work did not print the directory: %q", stderr.String())
	}
	if b.goCmd == "" || b.dir == "" || b.runGo == nil || b.loadPackages == nil || b.compile == nil || b.findRoot == nil {
		t.Errorf("newBuilder left a seam unbound: %+v", b)
	}
	// The production root resolution reaches the go toolchain, because no
	// nanogo distribution is built.
	root, err := b.findRoot(filepath.Join(string(filepath.Separator), "usr", "local", "go"))
	if err != nil || root.Nanogo {
		t.Errorf("findRoot = %+v, %v", root, err)
	}
}

func TestExecGoRunsAndReports(t *testing.T) {
	needGoCommand(t)
	b, _ := testBuilder(t, &buildOptions{})
	goCmd, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	b.goCmd = goCmd
	out, err := b.execGo("env", "GOROOT")
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Error("go env GOROOT answered nothing")
	}
	// A failure must carry what the go command said, because that is the only
	// place the reason appears.
	if _, err := b.execGo("thisisnotasubcommand"); err == nil {
		t.Error("a failing go command was reported as success")
	} else if !strings.Contains(err.Error(), "thisisnotasubcommand") {
		t.Errorf("error %v does not name the command", err)
	}
}

func TestListPackagesResolvesTheGraph(t *testing.T) {
	needGoCommand(t)
	b, _ := testBuilder(t, &buildOptions{})
	goCmd, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	b.goCmd = goCmd
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	b.dir = wd
	pkgs, err := b.listPackages(false, []string{"."})
	if err != nil {
		t.Fatalf("listPackages: %v", err)
	}
	found := false
	for _, p := range pkgs {
		if p.ImportPath == "golang.design/x/nanogo/driver" {
			found = true
		}
	}
	if !found {
		t.Errorf("listPackages over this directory did not report this package: %d packages", len(pkgs))
	}
}

// The program the in-process build compiles.
//
// The exit status is the assertion, as in internal/e2e: a wrong answer divides
// by zero and the process dies.
const buildProgram = `package main

func total(xs ...int) int {
	sum := 0
	for _, x := range xs {
		sum = sum + x
	}
	return sum
}

func main() {
	d := total(1, 2, 3, 4) - 10
	if d != 0 {
		d = d / (d - d)
	}
}
`

// TestRunBuildProducesAnExecutable drives the whole command with nothing
// faked: a real go list, a real Compile, and a real go tool link.
//
// internal/e2e runs the installed binary over the same claim. This one runs in
// process, so it says which lines of the front end a real build reaches.
func TestRunBuildProducesAnExecutable(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module nanogo.example/inproc\n\ngo 1.27\n")
	write("main.go", buildProgram)
	t.Chdir(dir)

	stderr := &bytes.Buffer{}
	env := &Env{Stdout: io.Discard, Stderr: stderr, Getenv: func(string) string { return "" }}
	code, err := RunBuild(env, []string{"-v", "-o", "prog", "."})
	if err != nil || code != 0 {
		t.Fatalf("nanogo build: exit %d: %v\n%s", code, err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "packages compiled by nanogo") {
		t.Errorf("the build printed no count:\n%s", stderr.String())
	}
	if b, err := exec.Command(filepath.Join(dir, "prog")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo built did not run: %v\n%s", err, b)
	}
}
