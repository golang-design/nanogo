// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.design/x/nanogo/loader"
)

// buildUsage is the one line form of "nanogo build".
const buildUsage = "usage: nanogo build [-o output] [-v] [-work] [packages]"

// unsafePkg is the one import path with no archive behind it. The compiler
// synthesises it, so no packagefile line names it and its absence is not a
// missing dependency.
const unsafePkg = "unsafe"

// buildOptions is a decoded "nanogo build" command line.
type buildOptions struct {
	// Output is -o, the executable to write. It is empty when the name comes
	// from the package.
	Output string

	// Verbose is -v: report each package as it is compiled, and report which
	// tree the standard library came from.
	Verbose bool

	// Work is -work: keep the scratch directory and print it.
	Work bool

	// Patterns are the package patterns. "." when none is given, as
	// "go build" does.
	Patterns []string
}

// ParseBuildArgs decodes the arguments of "nanogo build".
//
// Flags come before the patterns, which is the go command's rule. A flag after
// a pattern is a pattern, and a pattern that begins with "-" would be a flag,
// so the two cannot be told apart once the list has started.
func ParseBuildArgs(args []string) (*buildOptions, error) {
	opts := &buildOptions{}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		name := strings.TrimPrefix(a, "-")
		if len(name) > 1 && strings.HasPrefix(name, "-") {
			name = name[1:]
		}
		value, hasValue := "", false
		if j := strings.Index(name, "="); j > 0 {
			value, hasValue = name[j+1:], true
			name = name[:j]
		}
		switch name {
		case "o":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag -o needs a value\n%s", buildUsage)
				}
				i++
				value = args[i]
			}
			if value == "" {
				return nil, fmt.Errorf("flag -o needs a value\n%s", buildUsage)
			}
			opts.Output = value
		case "v":
			if err := setBool(&opts.Verbose, value); err != nil {
				return nil, &FlagError{Flag: "v", Reason: err.Error()}
			}
		case "work":
			if err := setBool(&opts.Work, value); err != nil {
				return nil, &FlagError{Flag: "work", Reason: err.Error()}
			}
		default:
			return nil, fmt.Errorf("unknown flag %q for nanogo build\n%s", a, buildUsage)
		}
	}
	opts.Patterns = args[i:]
	if len(opts.Patterns) == 0 {
		opts.Patterns = []string{"."}
	}
	return opts, nil
}

// buildPackage is one package that went into the link, and who compiled it.
//
// The producer is recorded and not inferred. A build in which nanogo compiles
// one package and the toolchain compiles the twenty-seven beneath it is the
// normal case, and a command that could not say which was which would let a
// green build stand for work nanogo did not do. That failure has already happened
// once in this project: an allowlist named a package, the go command sent
// -p main, nanogo delegated everything, and the program printed the right
// answer from gc-compiled code.
type buildPackage struct {
	// Path is the import path, which is also the packagefile key.
	Path string

	// Archive is the file the linker reads.
	Archive string

	// Producer is who compiled it: "nanogo", or the identity of the toolchain
	// that did.
	Producer string
}

// nanogoProducer is the producer string of a package nanogo compiled itself.
const nanogoProducer = "nanogo"

// builder runs one "nanogo build".
//
// The four fields below the data are seams. The command line, the package
// partitioning, the output naming and every refusal are decided here and are
// driven in process by driver/build_test.go; only these four reach outside.
type builder struct {
	env  *Env
	opts *buildOptions

	dir   string // the working directory, which is what -o is relative to
	work  string // the scratch directory holding the objects and the configurations
	goCmd string // the go binary

	// runGo runs the go command and returns its standard output.
	runGo func(args ...string) ([]byte, error)

	// loadPackages resolves patterns. export asks for the compiled archive of
	// each package, which costs a build of every one of them.
	loadPackages func(export bool, patterns []string) ([]*loader.Package, error)

	// compile is [Compile], so that a test can watch what the front end asked
	// the compiler for.
	compile func(*Config) error

	// findRoot is [FindRoot] with the go toolchain's root supplied.
	findRoot func(goroot string) (Root, error)
}

// RunBuild is "nanogo build".
//
// It is a front end over the same [Compile] the -toolexec path uses. The
// difference is who drives: here nanogo resolves the package graph, compiles
// the packages the user named, and calls the linker itself, so there is no
// allowlist, no environment variable and no -toolexec on the command line.
//
// What nanogo does not do, and must not appear to do: it compiles none of the
// standard library and none of the runtime. Those archives are built by the
// installed Go toolchain and the executable is produced by go tool link, per
// specs/045-linker.md, which records that nanogo has no linker.
func RunBuild(env *Env, args []string) (int, error) {
	opts, err := ParseBuildArgs(args)
	if err != nil {
		return 2, err
	}
	b, err := newBuilder(env, opts)
	if err != nil {
		return 1, err
	}
	defer b.cleanup()
	if err := b.run(); err != nil {
		return 1, err
	}
	return 0, nil
}

// newBuilder finds the go command and opens a scratch directory.
func newBuilder(env *Env, opts *buildOptions) (*builder, error) {
	goCmd, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("nanogo build needs the go command for the standard library and the linker: %v", err)
	}
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "nanogo-build")
	if err != nil {
		return nil, err
	}
	b := &builder{env: env, opts: opts, dir: dir, work: work, goCmd: goCmd}
	b.runGo = b.execGo
	b.loadPackages = b.listPackages
	b.compile = Compile
	b.findRoot = func(goroot string) (Root, error) {
		return FindRoot(env.Getenv, os.Executable, goroot)
	}
	if opts.Work {
		fmt.Fprintf(env.Stderr, "WORK=%s\n", work)
	}
	return b, nil
}

// cleanup removes the scratch directory, unless -work asked for it to stay.
func (b *builder) cleanup() {
	if b.work == "" || b.opts.Work {
		return
	}
	os.RemoveAll(b.work)
}

// run is the whole command.
func (b *builder) run() error {
	goroot, goversion, err := b.toolchain()
	if err != nil {
		return err
	}
	root, err := b.findRoot(goroot)
	if err != nil {
		return err
	}
	if root.Nanogo {
		// The distribution's own archives are the honest source, and nothing
		// reads them yet. Reporting that is the only answer that does not
		// hand the user gc-built archives under a nanogo label.
		return fmt.Errorf("%s holds a nanogo distribution and nanogo cannot read its pkg/%s tree yet: "+
			"the release job that builds it is unbuilt; unset %s to build against the installed Go toolchain",
			root.Path, TargetDir(), RootEnv)
	}

	targets, lang, err := b.targets()
	if err != nil {
		return err
	}
	mains := mainPackages(targets)
	// Before anything is compiled, because a command line that cannot name
	// its output is wrong however well the packages compile.
	if err := b.checkOutput(mains); err != nil {
		return err
	}
	deps, depPaths, err := b.dependencies(targets)
	if err != nil {
		return err
	}

	// One producer record per package that reaches the link. The dependency
	// records are written before anything is compiled, because the archives
	// already exist by the time go list answered.
	pkgs := make([]buildPackage, 0, len(targets)+len(depPaths))
	for _, p := range depPaths {
		pkgs = append(pkgs, buildPackage{Path: p, Archive: deps[p], Producer: goversion})
	}

	compileCfg, err := b.writeImportCfg("importcfg", depPaths, deps, nil)
	if err != nil {
		return err
	}
	imports, err := ReadImportCfg(compileCfg)
	if err != nil {
		return err
	}

	own := make(map[string]string, len(targets))
	for i, t := range targets {
		archive := filepath.Join(b.work, fmt.Sprintf("%d.a", i))
		if b.opts.Verbose {
			fmt.Fprintf(b.env.Stderr, "nanogo: compiling %s\n", t.ImportPath)
		}
		cfg := &Config{
			Output:    archive,
			Package:   compilePackageName(t),
			ImportCfg: imports,
			Lang:      lang,
			Pack:      true,
			Complete:  true,
			Files:     sourceFiles(t),
			GOARCH:    b.env.Getenv("GOARCH"),
		}
		if err := b.compile(cfg); err != nil {
			return err
		}
		own[t.ImportPath] = archive
		pkgs = append(pkgs, buildPackage{Path: t.ImportPath, Archive: archive, Producer: nanogoProducer})
	}

	if err := b.link(mains, depPaths, deps, own); err != nil {
		return err
	}
	b.report(pkgs, root, goversion)
	return nil
}

// toolchain asks the go command for the two facts a build needs about it: the
// root it was installed in and the release it is.
func (b *builder) toolchain() (goroot, goversion string, err error) {
	out, err := b.runGo("env", "GOROOT", "GOVERSION")
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return "", "", fmt.Errorf("go env GOROOT GOVERSION answered %q", out)
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), nil
}

// targets resolves the patterns to the packages nanogo compiles, and reports
// the language version the module asks for.
//
// Two listings are needed and they ask different questions. The first names
// the packages the patterns matched, which -deps cannot answer because it
// mixes the matches with everything beneath them. The second loads the graph,
// which is where the file lists and the dependency closure come from.
func (b *builder) targets() ([]*loader.Package, string, error) {
	names, lang, err := b.rootPaths()
	if err != nil {
		return nil, "", err
	}
	graph, err := b.loadPackages(false, b.opts.Patterns)
	if err != nil {
		return nil, "", err
	}
	index := make(map[string]*loader.Package, len(graph))
	for _, p := range graph {
		index[p.ImportPath] = p
	}
	targets := make([]*loader.Package, 0, len(names))
	for _, name := range names {
		p := index[name]
		if p == nil {
			return nil, "", fmt.Errorf("go list named %s and then did not describe it", name)
		}
		targets = append(targets, p)
	}
	// Sorted by import path, so that two runs over the same patterns compile
	// in the same order (specs/053-determinism.md). Nothing here depends on
	// dependency order: a target that imports another target is refused
	// below, because nanogo writes no export data.
	sort.Slice(targets, func(i, j int) bool { return targets[i].ImportPath < targets[j].ImportPath })
	if err := b.checkTargets(targets); err != nil {
		return nil, "", err
	}
	return targets, lang, nil
}

// rootPaths asks which packages the patterns matched, and what the main
// module's go directive says.
//
// The two travel together because they come from the same listing. -lang is
// what the go command derives from the go directive and sends to gc, and a
// compiler that left it empty would accept language constructs the module says
// it does not use.
func (b *builder) rootPaths() ([]string, string, error) {
	const format = "{{.ImportPath}}\t{{if .Module}}{{.Module.GoVersion}}{{end}}"
	out, err := b.runGo(append([]string{"list", "-f", format}, b.opts.Patterns...)...)
	if err != nil {
		return nil, "", err
	}
	var names []string
	lang := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, version, _ := strings.Cut(line, "\t")
		if name == "" {
			continue
		}
		names = append(names, name)
		if lang == "" && version != "" {
			lang = "go" + version
		}
	}
	if len(names) == 0 {
		return nil, "", fmt.Errorf("no packages match %s", strings.Join(b.opts.Patterns, " "))
	}
	return names, lang, nil
}

// checkTargets refuses a package nanogo cannot compile, before it reads a
// source file.
//
// Every refusal here names the package and the reason, and none of them hands
// the package to gc. A package the user named on the command line is a package
// the user asked nanogo to compile, so a silent fall back would make a green
// build mean nothing, which is the failure the allowlist had.
func (b *builder) checkTargets(targets []*loader.Package) error {
	own := make(map[string]bool, len(targets))
	for _, t := range targets {
		own[t.ImportPath] = true
	}
	for _, t := range targets {
		if t.Err != nil {
			return t.Err
		}
		if len(t.CgoFiles) > 0 {
			return &UnsupportedError{
				Package: t.ImportPath,
				What:    "a package that imports \"C\"",
				Detail:  "specs/000-decisions.md decision 8 puts cgo out of scope",
			}
		}
		if len(t.SFiles) > 0 {
			return &UnsupportedError{
				Package: t.ImportPath,
				What:    "a package with assembly in it",
				Detail: "an assembly definition is ABI0 and a Go call is ABIInternal, so it needs the ABI wrapper " +
					"of specs/030-abi.md, which is unbuilt",
			}
		}
		if len(t.GoFiles) == 0 {
			return fmt.Errorf("%s: no Go files nanogo can compile", t.ImportPath)
		}
		// Deps and not ImportPaths, because the edge may be transitive. A
		// dependency that is also a target is dropped from the dependency
		// closure, so it would reach go tool link as a missing package rather
		// than as a diagnostic here.
		for _, dep := range t.Deps {
			if own[dep] {
				return &UnsupportedError{
					Package: t.ImportPath,
					What:    "a package that depends on " + dep + ", which this command also compiles",
					Detail: "the archive nanogo writes carries no export data, so nothing can import it; " +
						"specs/015-export-data.md has no writer. Build " + dep + " with the go command, or " +
						"name only one of the two",
				}
			}
		}
	}
	return nil
}

// dependencies resolves every package beneath the targets to an archive.
//
// The archives are the installed toolchain's, and asking for them is what
// builds them. The dependency set is already transitively closed, so the go
// command compiles exactly the packages beneath the targets and never the
// targets themselves: a package the user asked nanogo to compile must not be
// compiled by gc, however cheap that would be.
func (b *builder) dependencies(targets []*loader.Package) (map[string]string, []string, error) {
	own := make(map[string]bool, len(targets))
	for _, t := range targets {
		own[t.ImportPath] = true
	}
	seen := make(map[string]bool)
	var paths []string
	for _, t := range targets {
		for _, dep := range t.Deps {
			if own[dep] || dep == unsafePkg || seen[dep] {
				continue
			}
			seen[dep] = true
			paths = append(paths, dep)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		// go list with no pattern lists the current directory, which would
		// answer with the target itself.
		return map[string]string{}, nil, nil
	}

	pkgs, err := b.loadPackages(true, paths)
	if err != nil {
		return nil, nil, err
	}
	index := make(map[string]*loader.Package, len(pkgs))
	for _, p := range pkgs {
		index[p.ImportPath] = p
	}
	archives := make(map[string]string, len(paths))
	for _, p := range paths {
		dep := index[p]
		switch {
		case dep == nil:
			return nil, nil, fmt.Errorf("%s: the go command did not describe this dependency", p)
		case dep.Err != nil:
			return nil, nil, dep.Err
		case dep.Export == "":
			// A missing packagefile line reaches the user as an undefined
			// symbol from the linker, which names neither the package nor the
			// reason.
			return nil, nil, fmt.Errorf("%s: the go command built no archive for this dependency", p)
		}
		archives[p] = dep.Export
	}
	return archives, paths, nil
}

// writeImportCfg writes an import configuration and returns its path.
//
// The order is the caller's slice order and never a map's, per
// specs/053-determinism.md.
func (b *builder) writeImportCfg(name string, paths []string, archives map[string]string, extra []buildPackage) (string, error) {
	var buf bytes.Buffer
	for _, p := range paths {
		fmt.Fprintf(&buf, "packagefile %s=%s\n", p, archives[p])
	}
	for _, e := range extra {
		fmt.Fprintf(&buf, "packagefile %s=%s\n", e.Path, e.Archive)
	}
	file := filepath.Join(b.work, name)
	if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	return file, nil
}

// link produces one executable per main package.
//
// nanogo has no linker. specs/045-linker.md is G2 work and unbuilt, so the
// executable is written by go tool link, which is the same call
// ssagen/ssagen_test.go makes to turn a compiled function into a running
// process.
func (b *builder) link(mains []*loader.Package, depPaths []string, deps, own map[string]string) error {
	for i, t := range mains {
		archive := own[t.ImportPath]
		// The main package's own entry, which is what the go command writes.
		// The linker takes the object positionally and reads this for the
		// package's identity.
		cfg, err := b.writeImportCfg(fmt.Sprintf("importcfg.link.%d", i), depPaths, deps,
			[]buildPackage{{Path: t.ImportPath, Archive: archive}})
		if err != nil {
			return err
		}
		out := b.outputPath(t)
		if b.opts.Verbose {
			fmt.Fprintf(b.env.Stderr, "nanogo: linking %s with go tool link\n", out)
		}
		if _, err := b.runGo("tool", "link", "-importcfg", cfg, "-o", out, archive); err != nil {
			return err
		}
	}
	return nil
}

// mainPackages is the targets an executable comes out of, in the order the
// targets were given.
func mainPackages(targets []*loader.Package) []*loader.Package {
	mains := make([]*loader.Package, 0, len(targets))
	for _, t := range targets {
		if t.Name == "main" {
			mains = append(mains, t)
		}
	}
	return mains
}

// checkOutput refuses an -o that cannot name one executable.
//
// go build has the same two rules, and for the same reason: -o is one file
// name, so it needs exactly one main package to write there.
func (b *builder) checkOutput(mains []*loader.Package) error {
	if b.opts.Output == "" {
		return nil
	}
	switch {
	case len(mains) == 0:
		return errors.New("-o names an executable and none of the packages is a main package")
	case len(mains) > 1:
		return fmt.Errorf("-o names one executable and %d of the packages are main packages", len(mains))
	}
	return nil
}

// outputPath is where the executable for one main package goes.
//
// The name follows go build: -o wins, and otherwise the last element of the
// import path names it. A package named on the command line as a list of files
// has no import path to take a name from, so the first file names it.
// Everything is relative to the working directory, as go build is.
func (b *builder) outputPath(t *loader.Package) string {
	name := b.opts.Output
	if name == "" {
		name = defaultOutputName(t)
	}
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(b.dir, name)
}

// commandLineArguments is the import path the go command gives a package named
// as a list of .go files.
const commandLineArguments = "command-line-arguments"

// defaultOutputName is the executable name go build would choose.
func defaultOutputName(t *loader.Package) string {
	if t.ImportPath != commandLineArguments && t.ImportPath != "" {
		return path.Base(t.ImportPath)
	}
	if len(t.GoFiles) > 0 {
		return strings.TrimSuffix(filepath.Base(t.GoFiles[0]), ".go")
	}
	return t.Name
}

// compilePackageName is the name the compiler is given for a package, which is
// the symbol prefix its object carries.
//
// It is the import path, except for a main package, which is always "main".
// That is not a convention: the linker looks for main.main, and the go command
// sends -p main for every main package in every module. Getting this wrong is
// what made the allowlist claim a package it never compiled.
func compilePackageName(t *loader.Package) string {
	if t.Name == "main" {
		return "main"
	}
	return t.ImportPath
}

// sourceFiles is the package's Go files, as absolute paths. The go command
// reports them relative to the package directory.
func sourceFiles(t *loader.Package) []string {
	files := make([]string, 0, len(t.GoFiles))
	for _, f := range t.GoFiles {
		if filepath.IsAbs(f) {
			files = append(files, f)
			continue
		}
		files = append(files, filepath.Join(t.Dir, f))
	}
	return files
}

// report says what nanogo compiled and what it did not.
//
// The summary is printed on every build and not only under -v. nanogo compiles
// one package of the twenty-nine a program needs to start, and a command that
// printed nothing would read as though it had built the program alone.
func (b *builder) report(pkgs []buildPackage, root Root, goversion string) {
	mine := 0
	for _, p := range pkgs {
		if p.Producer == nanogoProducer {
			mine++
		}
	}
	fmt.Fprintf(b.env.Stderr,
		"nanogo: %d of %d packages compiled by nanogo; %d by %s (everything not named on the command line)\n",
		mine, len(pkgs), len(pkgs)-mine, goversion)
	if !b.opts.Verbose {
		return
	}
	fmt.Fprintf(b.env.Stderr, "nanogo: the standard library and the runtime come from %s (%s)\n", root.Path, root.Origin)
	fmt.Fprintf(b.env.Stderr, "nanogo: the executable was written by go tool link; nanogo has no linker (specs/045-linker.md)\n")
	for _, p := range pkgs {
		if p.Producer == nanogoProducer {
			fmt.Fprintf(b.env.Stderr, "nanogo: %s compiled by %s\n", p.Path, p.Producer)
		}
	}
}

// execGo runs the go command in the working directory.
func (b *builder) execGo(args ...string) ([]byte, error) {
	cmd := exec.Command(b.goCmd, args...)
	cmd.Dir = b.dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return out, fmt.Errorf("go %s: %v", strings.Join(args, " "), err)
		}
		return out, fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// listPackages resolves patterns through the loader, which is the package that
// already knows how to ask the go command and decode the answer.
func (b *builder) listPackages(export bool, patterns []string) ([]*loader.Package, error) {
	g := &loader.GoList{Cmd: b.goCmd, Dir: b.dir, Export: export}
	return g.Load(patterns...)
}
