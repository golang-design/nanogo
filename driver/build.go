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

	// Producer is who compiled it: "nanogo", or the identity of the compiler
	// that did. A distribution's archives carry the producer their manifest
	// records, which is where "gc go1.27.0" comes from.
	Producer string

	// Nanogo reports whether nanogo compiled it. It is a field and not a
	// comparison against [nanogoProducer], because a distribution records a
	// nanogo archive with the build identity that wrote it and no two of
	// those strings are equal.
	Nanogo bool
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

	// tree is the nanogo distribution the standard library comes from, and is
	// nil when the build takes it from the installed Go toolchain.
	tree *distribution

	// goEnv is added to the environment of every go command this build runs.
	goEnv []string

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
// The standard library comes from a nanogo distribution when [FindRoot]
// resolves one, archive by archive out of its pkg/GOOS_GOARCH tree, and from
// the installed Go toolchain otherwise. Either way nanogo compiles none of it:
// what nanogo compiled is counted per package and printed on every build, so a
// program built entirely from gc's archives says so.
//
// The executable is produced by go tool link, per specs/045-linker.md, which
// records that nanogo has no linker. That is stated in the report rather than
// left for a reader to discover.
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
		return nil, fmt.Errorf("nanogo build needs the go command to resolve the packages you name and to link them: %v", err)
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
		tree, err := openDistribution(root.Path)
		if err != nil {
			return err
		}
		if err := tree.checkToolchain(goversion); err != nil {
			return err
		}
		b.tree = tree
		// The tree's archives were built with CGO_ENABLED=0, per
		// specs/000-decisions.md decision 8 and dist.Closure. Resolving the
		// package graph with any other setting asks for a dependency set the
		// tree was never built to hold, and the difference would arrive as a
		// missing archive rather than as a decision anybody took.
		b.goEnv = append(b.goEnv, "CGO_ENABLED=0")
	}

	targets, graph, lang, err := b.targets()
	if err != nil {
		return err
	}
	mains := mainPackages(targets)
	// Before anything is compiled, because a command line that cannot name
	// its output is wrong however well the packages compile.
	if err := b.checkOutput(mains); err != nil {
		return err
	}
	depPkgs, err := b.dependencies(targets, graph, goversion)
	if err != nil {
		return err
	}
	depPaths, deps := archivePaths(depPkgs)

	// One producer record per package that reaches the link. The dependency
	// records are written before anything is compiled, because the archives
	// already exist by the time they were resolved.
	pkgs := make([]buildPackage, 0, len(targets)+len(depPkgs))
	pkgs = append(pkgs, depPkgs...)

	compileCfg, err := b.writeImportCfg("importcfg", depPaths, deps, nil, "")
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
		pkgs = append(pkgs, buildPackage{Path: t.ImportPath, Archive: archive, Producer: nanogoProducer, Nanogo: true})
	}

	if err := b.link(mains, depPaths, deps, own); err != nil {
		return err
	}
	b.report(pkgs, root, len(mains) > 0)
	return nil
}

// toolchain asks the go command for the two facts a build needs about it: the
// root it was installed in and the release it is.
func (b *builder) toolchain() (goroot, goversion string, err error) {
	v, err := b.goEnvValues("GOROOT", "GOVERSION")
	if err != nil {
		return "", "", err
	}
	return v[0], v[1], nil
}

// targets resolves the patterns to the packages nanogo compiles, and reports
// the language version the module asks for.
//
// Two listings are needed and they ask different questions. The first names
// the packages the patterns matched, which -deps cannot answer because it
// mixes the matches with everything beneath them. The second loads the graph,
// which is where the file lists and the dependency closure come from.
//
// The graph is returned as well as the targets. It already says of every
// dependency whether it is a standard library package, which is what decides
// where the archive may come from, and asking the go command a third time
// would be asking a question already answered.
func (b *builder) targets() ([]*loader.Package, map[string]*loader.Package, string, error) {
	roots, err := b.rootPaths()
	if err != nil {
		return nil, nil, "", err
	}
	graph, err := b.loadPackages(false, b.opts.Patterns)
	if err != nil {
		return nil, nil, "", err
	}
	index := make(map[string]*loader.Package, len(graph))
	for _, p := range graph {
		index[p.ImportPath] = p
	}
	targets := make([]*loader.Package, 0, len(roots.Paths))
	for _, name := range roots.Paths {
		p := index[name]
		if p == nil {
			return nil, nil, "", fmt.Errorf("go list named %s and then did not describe it", name)
		}
		targets = append(targets, p)
	}
	// Sorted by import path, so that two runs over the same patterns compile
	// in the same order (specs/053-determinism.md). Nothing here depends on
	// dependency order: a target that imports another target is refused
	// below, because nanogo writes no export data.
	sort.Slice(targets, func(i, j int) bool { return targets[i].ImportPath < targets[j].ImportPath })
	if err := b.checkTargets(targets, roots.Embeds); err != nil {
		return nil, nil, "", err
	}
	return targets, index, roots.Lang, nil
}

// rootListing is what the first go list answered about the packages the
// patterns matched.
//
// The three facts travel together because they come from one listing and
// because each of them is the go command's answer rather than something nanogo
// could derive: which packages matched, what the module's go directive says,
// and which of the matched packages carry a go:embed directive.
type rootListing struct {
	// Paths is the import path of each match, in the go command's order.
	Paths []string

	// Lang is the -lang value the module's go directive asks for. It is
	// empty outside a module, which disables the language version checks,
	// as gc does when there is no go directive to read.
	Lang string

	// Embeds is the go:embed patterns of each match that has any, keyed by
	// import path. A package with none is absent.
	Embeds map[string][]string
}

// rootFormat is the listing rootPaths reads.
//
// EmbedPatterns is the go command's own answer, computed from the same file
// set it reports, and it is what decides whether the go command sends
// -embedcfg to the compiler. Keying the refusal below off it means the two
// build paths refuse the same packages by construction, rather than by nanogo
// implementing the directive's rules a second time and drifting from them.
const rootFormat = "{{.ImportPath}}\t{{if .Module}}{{.Module.GoVersion}}{{end}}\t{{join .EmbedPatterns \" \"}}"

// rootPaths asks which packages the patterns matched, what the main module's
// go directive says, and which matches carry a go:embed directive.
//
// The three travel together because they come from the same listing. -lang is
// what the go command derives from the go directive and sends to gc, and a
// compiler that left it empty would accept language constructs the module says
// it does not use. The embed patterns are what [builder.checkTargets] refuses
// a package by.
func (b *builder) rootPaths() (*rootListing, error) {
	out, err := b.runGo(append([]string{"list", "-f", rootFormat}, b.opts.Patterns...)...)
	if err != nil {
		return nil, err
	}
	roots := &rootListing{Embeds: make(map[string][]string)}
	for _, line := range strings.Split(string(out), "\n") {
		// The fields are positional and the last one may be empty, so the
		// line is split and not trimmed first.
		line = strings.TrimRight(line, "\r\n")
		fields := strings.SplitN(line, "\t", 3)
		name := strings.TrimSpace(fields[0])
		if name == "" {
			continue
		}
		roots.Paths = append(roots.Paths, name)
		if len(fields) > 1 {
			if version := strings.TrimSpace(fields[1]); roots.Lang == "" && version != "" {
				roots.Lang = "go" + version
			}
		}
		if len(fields) > 2 {
			if patterns := strings.Fields(fields[2]); len(patterns) > 0 {
				roots.Embeds[name] = patterns
			}
		}
	}
	if len(roots.Paths) == 0 {
		return nil, fmt.Errorf("no packages match %s", strings.Join(b.opts.Patterns, " "))
	}
	return roots, nil
}

// checkTargets refuses a package nanogo cannot compile, before it reads a
// source file.
//
// Every refusal here names the package and the reason, and none of them hands
// the package to gc. A package the user named on the command line is a package
// the user asked nanogo to compile, so a silent fall back would make a green
// build mean nothing, which is the failure the allowlist had.
func (b *builder) checkTargets(targets []*loader.Package, embeds map[string][]string) error {
	own := make(map[string]bool, len(targets))
	for _, t := range targets {
		own[t.ImportPath] = true
	}
	for _, t := range targets {
		if t.Err != nil {
			return t.Err
		}
		if patterns := embeds[t.ImportPath]; len(patterns) > 0 {
			return &UnsupportedError{
				Package: t.ImportPath,
				What:    "a package that uses go:embed",
				Detail: "the directive binds " + someOf(patterns, 3) + " and nanogo has no front end for it: " +
					"reading -embedcfg, resolving the patterns and building the embed.FS structure is the " +
					"unbuilt part of specs/050-driver.md. A variable nanogo compiled would be its zero value " +
					"at run time and nothing would say so",
			}
		}
		if len(t.CgoFiles) > 0 {
			return &UnsupportedError{
				Package: t.ImportPath,
				What:    "a package that imports \"C\"",
				Detail:  "specs/000-decisions.md decision 8 puts cgo out of scope",
			}
		}
		if len(t.SFiles) > 0 {
			// This is nanogo build's own gate and not the compiler's.
			// Compile reads -symabis and writes -asmhdr now
			// (specs/047-abi-wrappers.md stage 1) and refuses only a package
			// that owes an ABI wrapper, but that path is the toolexec one,
			// where the go command runs cmd/asm on either side of the
			// compile. nanogo build runs no assembler at all: it would have to
			// write the empty go_asm.h, run cmd/asm -gensymabis, compile, run
			// cmd/asm again against the header and pack the objects into the
			// archive. Until it does, an .s file here is a file whose
			// definitions would be missing from the archive, and the failure
			// is an undefined symbol at link time.
			return &UnsupportedError{
				Package: t.ImportPath,
				What:    "a package with assembly in it",
				Detail: "nanogo build runs no assembler: the two cmd/asm passes around the compile, " +
					"and the ABI wrappers of specs/047-abi-wrappers.md, are unbuilt. " +
					"go build -toolexec is the path that assembles",
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
					Detail: "this command writes one import configuration before it compiles anything, so a " +
						"target has no packagefile entry for another target. The archive nanogo writes does " +
						"carry export data (specs/015-export-data.md), so this is an ordering nanogo build " +
						"does not do yet and not a limit of the format. Build " + dep + " with the go " +
						"command, or name only one of the two",
				}
			}
		}
	}
	return nil
}

// dependencies resolves every package beneath the targets to an archive and to
// the compiler that produced it.
//
// Two sources, and which one a package comes from is decided by where it is
// rather than by what is convenient. A nanogo distribution holds the standard
// library, so every standard library dependency is taken from its
// pkg/GOOS_GOARCH tree, named by the manifest and checked against it. Anything
// else is the go command's to build, and asking for its archive is what builds
// it. Without a distribution every dependency is the go command's, which is
// what a source checkout does.
//
// The dependency set is transitively closed already, so the targets themselves
// are never compiled by anything but nanogo: a package the user asked nanogo
// to compile must not be compiled by gc, however cheap that would be.
//
// The result is sorted by import path, so that two runs over the same patterns
// write the same import configuration (specs/053-determinism.md).
func (b *builder) dependencies(targets []*loader.Package, graph map[string]*loader.Package, goversion string) ([]buildPackage, error) {
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
		return nil, nil
	}

	resolved := make(map[string]buildPackage, len(paths))
	var fromGo, missing []string
	for _, p := range paths {
		if bp, ok := b.tree.lookup(p); ok {
			resolved[p] = bp
			continue
		}
		if b.tree != nil {
			// Which of the two sources may supply a package is decided by
			// whether it is a standard library package, so a dependency the
			// listing did not describe is a question with no answer rather
			// than a package to hand to the go command. Handing it over is
			// the substitution this command exists to prevent, so an
			// undescribed dependency stops the build.
			dep := graph[p]
			if dep == nil {
				return nil, fmt.Errorf("%s: go list did not describe this dependency, so nanogo cannot tell "+
					"whether the distribution at %s is the tree that must supply it", p, b.tree.Root)
			}
			if dep.Standard {
				// Substituting the installed toolchain's copy here would
				// produce a working program whose standard library did not
				// come from the tree the build says it used, which is the one
				// lie this command exists to avoid.
				missing = append(missing, p)
				continue
			}
		}
		fromGo = append(fromGo, p)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the distribution at %s holds %d packages and this program needs %s, "+
			"which it does not carry: a standard library package the tree does not hold cannot be taken "+
			"from the installed toolchain, which would make this something other than a build against "+
			"this tree. A nanogo run from outside a distribution builds against the installed Go "+
			"toolchain, which has them",
			b.tree.Root, b.tree.Packages, someOf(missing, 5))
	}

	if len(fromGo) > 0 {
		pkgs, err := b.loadPackages(true, fromGo)
		if err != nil {
			return nil, err
		}
		index := make(map[string]*loader.Package, len(pkgs))
		for _, p := range pkgs {
			index[p.ImportPath] = p
		}
		for _, p := range fromGo {
			dep := index[p]
			switch {
			case dep == nil:
				return nil, fmt.Errorf("%s: the go command did not describe this dependency", p)
			case dep.Err != nil:
				return nil, dep.Err
			case dep.Export == "":
				// A missing packagefile line reaches the user as an undefined
				// symbol from the linker, which names neither the package nor
				// the reason.
				return nil, fmt.Errorf("%s: the go command built no archive for this dependency", p)
			}
			resolved[p] = buildPackage{Path: p, Archive: dep.Export, Producer: goversion}
		}
	}

	out := make([]buildPackage, 0, len(paths))
	for _, p := range paths {
		out = append(out, resolved[p])
	}
	return out, nil
}

// someOf is a list of import paths, shortened.
//
// A program that imports os needs twenty-three packages the bootstrap closure
// does not hold, and a refusal that names all of them is accurate and unread.
// The first few and a count say the same thing in a line.
func someOf(paths []string, n int) string {
	if len(paths) <= n {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:n], ", "), len(paths)-n)
}

// archivePaths splits a package list into the order the import configuration
// is written in and the path to archive lookup it is written from.
func archivePaths(pkgs []buildPackage) ([]string, map[string]string) {
	paths := make([]string, 0, len(pkgs))
	archives := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		paths = append(paths, p.Path)
		archives[p.Path] = p.Archive
	}
	return paths, archives
}

// writeImportCfg writes an import configuration and returns its path.
//
// The order is the caller's slice order and never a map's, per
// specs/053-determinism.md.
//
// modinfo is the runtime.modinfo blob, and an empty one writes no modinfo
// line. Only the link configuration carries one: the directive is the
// linker's, per the table in specs/015-export-data.md, and the compiler has
// no use for it.
func (b *builder) writeImportCfg(name string, paths []string, archives map[string]string, extra []buildPackage, modinfo string) (string, error) {
	var buf bytes.Buffer
	for _, p := range paths {
		fmt.Fprintf(&buf, "packagefile %s=%s\n", p, archives[p])
	}
	for _, e := range extra {
		fmt.Fprintf(&buf, "packagefile %s=%s\n", e.Path, e.Archive)
	}
	if modinfo != "" {
		// %q, because cmd/link reads the argument with strconv.Unquote. The
		// blob's sentinels are not valid UTF-8 and fmt writes them as \xNN,
		// which Unquote restores byte for byte.
		fmt.Fprintf(&buf, "modinfo %q\n", modinfo)
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
//
// Each link configuration carries a modinfo line. cmd/link turns it into
// runtime.modinfo, which is the only thing runtime/debug.ReadBuildInfo reads,
// so a link without one produces a program that cannot describe its own build.
// The blob is assembled per main package, because the module set is the one
// beneath that package.
func (b *builder) link(mains []*loader.Package, depPaths []string, deps, own map[string]string) error {
	if len(mains) == 0 {
		return nil
	}
	mods, err := b.modules()
	if err != nil {
		return err
	}
	settings, err := b.buildSettings()
	if err != nil {
		return err
	}
	for i, t := range mains {
		archive := own[t.ImportPath]
		// The main package's own entry, which is what the go command writes.
		// The linker takes the object positionally and reads this for the
		// package's identity.
		cfg, err := b.writeImportCfg(fmt.Sprintf("importcfg.link.%d", i), depPaths, deps,
			[]buildPackage{{Path: t.ImportPath, Archive: archive}},
			modInfoData(buildInfo(t, mods, settings).String()))
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
// one package of the twenty-eight a program needs to start, and a command that
// printed nothing would read as though it had built the program alone. Every
// producer that is not nanogo is named with its count, so a program whose
// whole standard library came from gc says so in the first line of output.
//
// The second line is printed on every build too. It says which tree the
// standard library came from and that the executable was written by go tool
// link, because a reader who is told what nanogo compiled and not what wrote
// the program has been told half of it.
func (b *builder) report(pkgs []buildPackage, root Root, linked bool) {
	mine := 0
	for _, p := range pkgs {
		if p.Nanogo {
			mine++
		}
	}
	var line strings.Builder
	fmt.Fprintf(&line, "nanogo: %d of %d packages compiled by nanogo", mine, len(pkgs))
	for _, g := range otherProducers(pkgs) {
		fmt.Fprintf(&line, "; %d by %s", g.Count, g.Producer)
	}
	if mine < len(pkgs) {
		line.WriteString(" (everything not named on the command line)")
	}
	fmt.Fprintln(b.env.Stderr, line.String())

	fmt.Fprintf(b.env.Stderr, "nanogo: the standard library and the runtime come from %s (%s)\n", root.Path, root.Origin)
	if linked {
		fmt.Fprintf(b.env.Stderr, "nanogo: the executable was written by go tool link; nanogo has no linker (specs/045-linker.md)\n")
	}
	if !b.opts.Verbose {
		return
	}
	for _, p := range pkgs {
		if p.Nanogo {
			fmt.Fprintf(b.env.Stderr, "nanogo: %s compiled by %s\n", p.Path, p.Producer)
		}
	}
}

// producerCount is one compiler and how many archives of a build it wrote.
type producerCount struct {
	Producer string
	Count    int
}

// otherProducers counts the packages nanogo did not compile, grouped by
// producer and ordered by producer name.
//
// A slice and not a map. specs/053-determinism.md forbids ranging over a map
// on a path that produces output, and this path produces the line a user
// reads.
func otherProducers(pkgs []buildPackage) []producerCount {
	index := make(map[string]int)
	var out []producerCount
	for _, p := range pkgs {
		if p.Nanogo {
			continue
		}
		if i, ok := index[p.Producer]; ok {
			out[i].Count++
			continue
		}
		index[p.Producer] = len(out)
		out = append(out, producerCount{Producer: p.Producer, Count: 1})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Producer < out[j].Producer })
	return out
}

// execGo runs the go command in the working directory.
func (b *builder) execGo(args ...string) ([]byte, error) {
	cmd := exec.Command(b.goCmd, args...)
	cmd.Dir = b.dir
	if len(b.goEnv) > 0 {
		cmd.Env = append(os.Environ(), b.goEnv...)
	}
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
	if len(b.goEnv) > 0 {
		g.Env = append(os.Environ(), b.goEnv...)
	}
	return g.Load(patterns...)
}
