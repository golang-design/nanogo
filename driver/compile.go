// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/ssa/rules"
	"golang.design/x/nanogo/ssagen"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// TargetArch is the only architecture nanogo emits code for.
//
// specs/000-decisions.md decision 9 makes darwin/arm64 the first target and
// linux/amd64 the second, and specs/043-amd64-backend.md is unbuilt. An object
// full of arm64 instructions labelled with another host's object header links,
// and then dies inside the runtime as soon as it walks its own pc tables, so
// [checkHost] refuses the compile rather than letting the failure be blamed on
// code generation.
const TargetArch = "arm64"

// maxReportedErrors bounds how many type-checking errors one package reports.
//
// The checker keeps going after a soft error, and a file with a bad import
// produces one error per use of the import. A user acts on the first few, so
// the rest are noise that hides them.
const maxReportedErrors = 10

// UnsupportedError reports a Go construct nanogo cannot compile yet.
//
// It names the package and the construct, because the allowlist is the
// project's progress metric (specs/051-build-integration.md) and a failure
// must say which entry produced it and what stopped it.
type UnsupportedError struct {
	Package string // the import path from -p
	What    string // the construct, in the user's terms
	Detail  string // what the pipeline said, or the spec that owns the gap
}

func (e *UnsupportedError) Error() string {
	msg := e.Package + ": nanogo cannot compile " + e.What
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Compile is the single point where nanogo's own compiler runs.
//
// It is the pipeline of specs/002-architecture.md with nothing left out: parse,
// check, ir.Build, ssa.Build, decompose, assign the ABI, lower, allocate, emit,
// and write an object the real linker reads.
//
// What it refuses, it refuses by name. A package on the allowlist is a claim
// that nanogo owns it, so a construct nanogo cannot compile is an error that
// names the construct rather than a silent hand back to gc: a fallback here
// would make the allowlist say something that is not true.
func Compile(cfg *Config) error {
	if cfg == nil {
		return errors.New("nanogo: nil configuration")
	}
	// specs/000-decisions.md decision 11 pins nanogo to one Go release. The
	// go command states the release it expects with -goversion.
	if cfg.GoVersion != "" && cfg.GoVersion != PinnedGoVersion {
		return fmt.Errorf("%s: -goversion %s does not match the pinned %s",
			cfg.Package, cfg.GoVersion, PinnedGoVersion)
	}
	if err := checkSupported(cfg); err != nil {
		return err
	}

	files, fset, err := parseFiles(cfg)
	if err != nil {
		return err
	}
	imp := newImporter(cfg)
	pkg, info, err := checkFiles(cfg, imp, files, fset)
	if err != nil {
		return err
	}
	p, err := ir.Build(pkg, files, info)
	if err != nil {
		return &UnsupportedError{Package: cfg.Package, What: "this package", Detail: err.Error()}
	}
	if err := checkIR(cfg, p); err != nil {
		return err
	}

	out, err := emitPackage(cfg, p, fset, imp.reader.Imports())
	if err != nil {
		return err
	}
	return writeOutput(cfg, out)
}

// checkSupported refuses the inputs nanogo has no answer for, before it reads
// a single source file.
//
// Each refusal names the spec that owns the gap. A user who hits one needs to
// know whether to wait, to remove the package from the allowlist, or to write
// the missing pass.
func checkSupported(cfg *Config) error {
	if cfg.Output == "" {
		return fmt.Errorf("%s: no -o output file", cfg.Package)
	}
	if len(cfg.Files) == 0 {
		return fmt.Errorf("%s: no source files on the command line", cfg.Package)
	}
	if cfg.CompilingRuntime {
		return &UnsupportedError{
			Package: cfg.Package,
			What:    "the runtime",
			Detail:  "-+ turns on the write barrier and nosplit rules of specs/034 and specs/035, which are unbuilt",
		}
	}
	if cfg.EmbedCfgFile != "" {
		return &UnsupportedError{
			Package: cfg.Package,
			What:    "a package that uses go:embed",
			Detail:  "-embedcfg needs the data emitter of specs/050-driver.md, which is unbuilt",
		}
	}
	return checkAssembly(cfg)
}

// checkAssembly refuses a package with assembly in it.
//
// The go command sends -symabis and -asmhdr exactly when the package has .s
// files, so either flag is how nanogo learns that it does. Two things are
// missing and both are needed:
//
//  1. The ABI. cmd/asm refuses the <ABIInternal> marker outside the runtime,
//     so every assembly definition in an ordinary package is ABI0, and a Go
//     call to it under specs/030-abi.md names an ABIInternal symbol that does
//     not exist. gc closes the gap by generating a wrapper per bodyless
//     declaration. nanogo has no wrapper generator, so the call would reach
//     nothing and the linker would report it as a missing symbol, which is the
//     wrong place to find out.
//  2. The header. -asmhdr asks for the constants and the struct offsets the
//     assembly refers to by name, and nanogo writes none.
func checkAssembly(cfg *Config) error {
	if cfg.SymABIs == "" && cfg.AsmHdr == "" {
		return nil
	}
	return &UnsupportedError{
		Package: cfg.Package,
		What:    "a package with assembly in it",
		Detail: "an assembly definition is ABI0 and a Go call is ABIInternal, so it needs the ABI wrapper of " +
			"specs/030-abi.md, and -asmhdr needs the header of specs/050-driver.md; neither is built",
	}
}

// parseFiles parses every source file named on the command line.
//
// The files share one FileSet, because a position in nanogo's syntax package
// is a bare offset into the set (specs/010-scanner-and-positions.md) and every
// stage below reports positions through it.
func parseFiles(cfg *Config) ([]*syntax.File, *syntax.FileSet, error) {
	fset := syntax.NewFileSet()
	files := make([]*syntax.File, 0, len(cfg.Files))
	var errs []error
	// The handler reports the position through the file set, because
	// syntax.Error carries a bare offset and prints only its message. An
	// error a user cannot locate is not a diagnostic (specs/052). A file that
	// cannot be opened has no position at all, so the name stands in: the
	// alternative is "<unknown position>: no such file", which says nothing.
	current := ""
	report := func(err syntax.Error) {
		if len(errs) >= maxReportedErrors {
			return
		}
		if p := fset.Position(err.Pos); p.Filename != "" {
			errs = append(errs, fmt.Errorf("%s: %s", p, err.Msg))
			return
		}
		errs = append(errs, fmt.Errorf("%s: %s", current, err.Msg))
	}
	for _, name := range cfg.Files {
		current = name
		f, err := syntax.ParseFile(fset, name, report, pragmaHandler, syntax.CheckBranches)
		if f == nil {
			if err != nil && len(errs) == 0 {
				errs = append(errs, fmt.Errorf("%s: %v", name, err))
			}
			continue
		}
		files = append(files, f)
	}
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	return files, fset, nil
}

// pragmaHandler records nothing, and that is a correctness gap rather than a
// missing feature.
//
// specs/016-directives-and-pragmas.md divides the directives into two groups,
// and the first is titled "required for correctness": getting one of them
// wrong produces a program that is wrong, not slow. Dropping every one of them
// silently means a source file that says
//
//	//go:nosplit
//	func f() { ... }
//
// compiles to a function with a stack-growth check in it. The check calls
// runtime.morestack, and the whole reason a function is marked nosplit is that
// it runs where that call is not allowed.
//
// nanogo does not compile the runtime today, and the runtime is the only
// consumer of that group, so nothing reachable is miscompiled by this yet. The
// gap is recorded here and gated by TestNosplitIsStillDropped rather than
// left as an absence, because the day nanogo reaches a package that uses the
// directive, the failure has no diagnostic at all.
//
// The handler exists because the parser needs one to attach a //go: comment to
// the declaration that follows it, and a nil handler makes the parser treat
// the comment as ordinary text.
func pragmaHandler(_ syntax.Pos, _ bool, _ string, _ syntax.Pragma) syntax.Pragma {
	return nil
}

// checkFiles type-checks the package.
//
// The importer reads gc's export data (specs/015-export-data.md), so an import
// resolves to the package the archive -importcfg names for it.
//
// The deferred recover is not defensive. Export data is decoded lazily: a
// declaration is read when the checker first looks it up, which is after the
// importer returned and has no error channel left to use, so the reader
// signals a stream it cannot decode by panicking. This frame is the last one
// that still knows which package the build asked nanogo to compile.
func checkFiles(cfg *Config, imp *importer, files []*syntax.File, fset *syntax.FileSet) (pkg *types2.Package, info *types2.Info, err error) {
	defer func() {
		if v := recover(); v != nil {
			pkg, info, err = nil, nil, imp.recovered(v)
		}
	}()
	info = &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	var errs []error
	conf := types2.Config{
		Fset:      fset,
		Sizes:     types2.SizesFor("gc", TargetArch),
		GoVersion: cfg.Lang,
		Importer:  imp,
		Error: func(err error) {
			if len(errs) < maxReportedErrors {
				errs = append(errs, err)
			}
		},
	}
	// The name the checker is given is the import path, which is what
	// specs/032-type-descriptors-and-itabs.md makes the symbol prefix. The go
	// command sends "main" for every main package, and that is the prefix the
	// linker expects for one.
	pkg, err = conf.Check(cfg.Package, files, info)
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	if err != nil {
		return nil, nil, err
	}
	return pkg, info, nil
}

// checkIR refuses the package-level constructs the backend has no writer for.
//
// They are separate from the per-function refusals below because they are
// properties of the package: a global needs a data symbol and an initialiser
// needs an init task, and neither exists. Compiling the functions first and
// discovering this at link time would report the gap as an undefined symbol.
func checkIR(cfg *Config, p *ir.Package) error {
	if len(p.Globals) > 0 {
		return &UnsupportedError{
			Package: cfg.Package,
			What:    "a package with package-level variables",
			Detail:  "a global needs a data symbol, which specs/020-ir.md's object model does not carry yet",
		}
	}
	if len(p.Inits) > 0 {
		return &UnsupportedError{
			Package: cfg.Package,
			What:    "a package with an init function",
			Detail:  "an init needs the package init task of specs/040-object-format.md, which is unbuilt",
		}
	}
	return nil
}

// checkHost refuses a host nanogo cannot emit code for.
//
// The check runs here and not at the top of [Compile] on purpose. Everything
// above code generation is architecture independent, so a user on another host
// still gets the parse errors, the type errors and the list of constructs
// nanogo refuses. Only the backend is arm64.
func checkHost(pkg, arch string) error {
	if arch == TargetArch {
		return nil
	}
	return &UnsupportedError{
		Package: pkg,
		What:    "for this host",
		Detail: fmt.Sprintf("nanogo emits %s machine code and GOARCH is %s (specs/043-amd64-backend.md is unbuilt)",
			TargetArch, arch),
	}
}

// emitPackage compiles every function in declaration order into one object.
//
// Declaration order, not map order: the object's symbol table is written by
// walking the lists in the order symbols were added, so two runs over the same
// input must add them in the same order (specs/053-determinism.md).
func emitPackage(cfg *Config, p *ir.Package, fset *syntax.FileSet, imports []export.Import) (*obj.Package, error) {
	if err := checkHost(cfg.Package, runtime.GOARCH); err != nil {
		return nil, err
	}
	out := obj.NewPackage(p.Path)
	out.Main = p.Name == "main"
	// One Autolib entry per direct import. The linker builds its list of
	// libraries to load from these, so a call to an imported function whose
	// package no entry names is reported as an undefined symbol even though
	// -importcfg told the linker where the archive is. The fingerprint is the
	// export data's, and the linker refuses the build when it disagrees with
	// the one in that package's own object.
	for _, im := range imports {
		out.AddImport(im.Path, im.Fingerprint)
	}
	target := ssa.NewArm64Target()
	compiled := 0
	for _, fn := range p.Funcs {
		if len(fn.Body) == 0 {
			// A bodyless declaration is satisfied by assembly, and
			// checkAssembly has already refused a package that has any.
			// With -complete the go command promises there is none, so the
			// declaration is an error rather than an external definition.
			if cfg.Complete {
				return nil, fmt.Errorf("%s: missing function body", position(fset, fn.Pos, fn.Name))
			}
			continue
		}
		r, err := compileFunc(cfg, fn, target, out, fset)
		if err != nil {
			return nil, err
		}
		if _, err := r.Add(out); err != nil {
			return nil, &UnsupportedError{Package: cfg.Package, What: "function " + fn.Name, Detail: err.Error()}
		}
		compiled++
	}
	if compiled == 0 {
		return nil, &UnsupportedError{
			Package: cfg.Package,
			What:    "a package with no function bodies",
			Detail:  "nanogo writes text symbols and nothing else, so such an object holds nothing",
		}
	}
	return out, nil
}

// compileFunc takes one function from IR to a symbol.
//
// The passes are a list and not a sequence of statements because every one of
// them reports the same way: a failure names the pass, the function and the
// position, and the pass name is what tells a reader which spec owns the gap.
//
// The order is specs/002-architecture.md's: decomposition, then
// specs/030-abi.md's assignment, then selection. The assignment runs between
// them because it finishes work decomposition stopped at its bound and because
// the rewrites it makes still need lowering rules. The verifier runs after each
// of the two rewriting passes, so a violation names the pass that made it.
func compileFunc(cfg *Config, fn *ir.Func, target *ssa.Target, out *obj.Package, fset *syntax.FileSet) (*ssagen.Result, error) {
	var (
		f *ssa.Func
		a *ssa.Alloc
		r *ssagen.Result
	)
	passes := []struct {
		name string
		run  func() error
	}{
		{"ssa.Build", func() (err error) { f, err = ssa.Build(fn); return err }},
		{"decomposition", func() error { ssa.Decompose(f); return nil }},
		{"the ABI assignment", func() error { return ssa.AssignABI(f, target) }},
		{"verification after the ABI assignment", func() error { return verify(f) }},
		{"lowering", func() error { ssa.Lower(f, rules.ARM64); return nil }},
		{"verification after lowering", func() error { return verify(f) }},
		{"register allocation", func() (err error) {
			ssa.SplitCriticalEdges(f)
			a, err = ssa.Allocate(f, target)
			return err
		}},
		{"ssagen.Emit", func() (err error) {
			file, line := fileAndLine(cfg, fset, fn.Pos)
			r, err = ssagen.Emit(f, a, out, ssagen.Options{
				Sym:  fn.Sym,
				ABI:  obj.ABIInternal,
				File: file,
				Line: line,
				Fset: fset,
			})
			return err
		}},
	}
	for _, p := range passes {
		if err := p.run(); err != nil {
			return nil, &UnsupportedError{
				Package: cfg.Package,
				What:    "function " + fn.Name + " at " + position(fset, fn.Pos, fn.Name),
				Detail:  p.name + ": " + err.Error(),
			}
		}
	}
	return r, nil
}

// verify runs the SSA invariant checks and turns a finding into an error.
func verify(f *ssa.Func) error {
	if vs := ssa.Verify(f); len(vs) != 0 {
		return violations(vs)
	}
	return nil
}

// violations joins the verifier's findings into one error.
func violations(vs []ssa.Violation) error {
	errs := make([]error, 0, len(vs))
	for _, v := range vs {
		errs = append(errs, errors.New(v.String()))
	}
	return errors.Join(errs...)
}

// fileAndLine resolves a position to the file name and line the object
// records, with -trimpath applied.
func fileAndLine(cfg *Config, fset *syntax.FileSet, pos syntax.Pos) (string, int32) {
	if fset == nil {
		return "", 1
	}
	p := fset.Position(pos)
	name := TrimPath(cfg.TrimRewrites, p.Filename)
	line := int32(p.Line)
	if line == 0 {
		line = 1
	}
	return name, line
}

// position formats a position for a diagnostic, and falls back to the name
// when the position is unknown. specs/052-diagnostics.md wants file:line:col.
func position(fset *syntax.FileSet, pos syntax.Pos, name string) string {
	if fset == nil {
		return name
	}
	p := fset.Position(pos)
	if p.Filename == "" {
		return name
	}
	return p.String()
}

// TrimPath applies the -trimpath rewrites to one path.
//
// The go command sends a list of old=>new rewrites joined by ";", longest
// intent first, and the last one has an empty new side because it erases the
// build's temporary directory. The first rewrite that matches wins, which is
// what cmd/internal/objabi does.
func TrimPath(rewrites []TrimRewrite, path string) string {
	for _, r := range rewrites {
		if path == r.Old {
			return r.New
		}
		if strings.HasPrefix(path, r.Old+string(filepath.Separator)) {
			rest := path[len(r.Old)+1:]
			if r.New == "" {
				return rest
			}
			return r.New + string(filepath.Separator) + rest
		}
	}
	return path
}

// verifyToolchain reports what the installed toolchain writes into an object.
//
// It is a variable so that a test can supply a header the writer refuses. The
// real probe caches its answer for the process, so a test that broke it would
// break every later test in the same binary.
var verifyToolchain = obj.VerifyToolchain

// writeOutput writes the object to -o, as an archive when -pack asked for one.
func writeOutput(cfg *Config, p *obj.Package) error {
	tc, err := verifyToolchain()
	if err != nil {
		return fmt.Errorf("%s: %v", cfg.Package, err)
	}
	f, err := os.Create(cfg.Output)
	if err != nil {
		return err
	}
	if err := writeTo(f, p, tc.Header, cfg.Pack); err != nil {
		f.Close()
		return fmt.Errorf("%s: %v", cfg.Package, err)
	}
	return f.Close()
}
