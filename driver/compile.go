// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtsym"
	"golang.design/x/nanogo/rtype"
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
	// specs/000-decisions.md decision 10 pins nanogo to one Go release. The
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
	owed, err := checkExportedTypes(cfg, pkg, fset)
	if err != nil {
		return err
	}
	p, err := ir.Build(pkg, files, info)
	if err != nil {
		return &UnsupportedError{Package: cfg.Package, What: "this package", Detail: err.Error()}
	}
	if err := checkIR(cfg, p, fset); err != nil {
		return err
	}

	out, hasInit, err := emitPackage(cfg, p, fset, imp.reader.Imports(), owed)
	if err != nil {
		return err
	}
	return writeOutput(cfg, out, pkg, hasInit, exportBodies(cfg, pkg, info, fset, files))
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
		// The data emitter is not what is missing: ssagen binds a data symbol
		// to a package-level variable already. The front end is: reading
		// -embedcfg, resolving its patterns to the listed files, and building
		// the embed.FS structure.
		return &UnsupportedError{
			Package: cfg.Package,
			What:    "a package that uses go:embed",
			Detail: "reading -embedcfg, resolving the patterns and building the embed.FS structure is the " +
				"unbuilt front end of specs/050-driver.md",
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
//     declaration. addGenerated builds generated functions already, but not
//     this one: an ABI0 wrapper is a text symbol whose own convention is ABI0,
//     so its arguments arrive in the outgoing argument area, and ssa.AssignABI
//     has one answer. Without it the call reaches nothing and the linker
//     reports a missing symbol, which is the wrong place to find out.
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
	var diags diagnostics
	// The handler reports the position through the file set, because
	// syntax.Error carries a bare offset and prints only its message. An
	// error a user cannot locate is not a diagnostic (specs/052). A file that
	// cannot be opened has no position at all, so the name stands in: the
	// alternative is "<unknown position>: no such file", which says nothing.
	current := ""
	report := func(err syntax.Error) {
		if p := fset.Position(err.Pos); p.Filename != "" {
			diags.add(err.Pos, fmt.Errorf("%s: %s", p, err.Msg))
			return
		}
		diags.add(err.Pos, fmt.Errorf("%s: %s", current, err.Msg))
	}
	pragh := newPragmaHandler(report)
	for _, name := range cfg.Files {
		current = name
		f, err := syntax.ParseFile(fset, name, report, pragh, syntax.CheckBranches)
		if f == nil {
			if err != nil && diags.len() == 0 {
				diags.add(syntax.NoPos, fmt.Errorf("%s: %v", name, err))
			}
			continue
		}
		checkDirectives(f, report)
		files = append(files, f)
	}
	if err := diags.err(); err != nil {
		return nil, nil, err
	}
	return files, fset, nil
}

// A //go: directive is recorded but never honoured, and that is a correctness
// gap rather than a missing feature.
//
// specs/016-directives-and-pragmas.md divides the directives into two groups,
// and the first is titled "required for correctness": getting one of them
// wrong produces a program that is wrong, not slow. A source file that says
//
//	//go:nosplit
//	func f() { ... }
//
// still compiles to a function with a stack-growth check in it. The check
// calls runtime.morestack, and the whole reason a function is marked nosplit
// is that it runs where that call is not allowed.
//
// nanogo does not compile the runtime today, and the runtime is the only
// consumer of most of that group, so most of it miscompiles nothing reachable
// yet. The gap is gated by TestDirectivesAreRecordedButNotHonoured rather than
// left as an absence, because the day nanogo reaches a package that uses the
// directive, the failure has no diagnostic at all.
//
// Two of them are not in that position and are refused instead. See
// [LifetimeDirective]: //go:uintptrescapes and //go:uintptrkeepalive are
// written by ordinary code that calls into a system, they were recorded and
// dropped, and the corpus caught the program that came out.
//
// What the record does buy is the other half of the rule: a directive that no
// declaration can use is now reported rather than dropped in silence. See
// [newPragmaHandler] and [checkDirectives].

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

		// FileVersions says which Go language version each file was
		// checked against, which is -lang for every file of the package:
		// nanogo's syntax.File carries no version of its own, so a
		// //go:build line that names one does not reach the checker.
		//
		// The checker fills the map only when it is there to fill, and a
		// nil map answers "no version" for every file. export's body
		// builder reads that as the current release, so a package
		// compiled with -lang=go1.21 would export a loop under the Go
		// 1.22 variable rule. gc reads the flag and rewrites the loop, so
		// the two disagree about which variable the loop body closes over.
		FileVersions: make(map[*syntax.SrcFile]string),
	}
	var diags diagnostics
	conf := types2.Config{
		Fset:      fset,
		Sizes:     types2.SizesFor("gc", TargetArch),
		GoVersion: cfg.Lang,
		Importer:  imp,
		Error: func(err error) {
			// types2 reports one Error per mistake, with the continuation
			// lines already folded into its message, so the position of the
			// Error is the position the whole message is filed under.
			//
			// The assertion is unchecked because the checker passes an Error
			// and nothing else. A fallback branch here would be code no test
			// can reach, and the zero Error already behaves as one: its Pos
			// is NoPos, which sorts to the end of the report.
			e, _ := err.(types2.Error)
			diags.add(e.Pos, err)
		},
	}
	// The name the checker is given is the import path, which is what
	// specs/032-type-descriptors-and-itabs.md makes the symbol prefix. The go
	// command sends "main" for every main package, and that is the prefix the
	// linker expects for one.
	pkg, err = conf.Check(cfg.Package, files, info)
	if e := diags.err(); e != nil {
		return nil, nil, e
	}
	if err != nil {
		return nil, nil, err
	}
	return pkg, info, nil
}

// checkIR refuses the package-level constructs the backend has no writer for.
//
// They are separate from the per-function refusals below because they are
// properties of the package: a package-level variable needs a data symbol and
// there is no writer for one. Compiling the functions first and discovering
// this at link time would report the gap as an undefined symbol.
//
// The refusal names the variable and its position rather than the package.
// The alternative is worse than a poor message: a package whose variables
// nanogo cannot write is a package whose initialisation record would list an
// init function that assigns to symbols that do not exist. A record that is
// silently short of the work the source asked for produces a program that
// runs and is wrong, which is the failure the record exists to prevent.
func checkIR(cfg *Config, p *ir.Package, fset *syntax.FileSet) error {
	var ge *ssagen.GlobalError
	if err := ssagen.CheckGlobals(p); errors.As(err, &ge) {
		return &UnsupportedError{
			Package: cfg.Package,
			What:    "package-level variable " + ge.Obj.Name + " at " + position(fset, ge.Obj.Pos, ge.Obj.Name),
			Detail:  ge.Reason,
		}
	} else if err != nil {
		return err
	}
	return nil
}

// checkHost refuses a host nanogo cannot emit code for.
//
// The check runs here and not at the top of [Compile] on purpose. Everything
// above code generation is architecture independent, so a build for another
// target still gets the parse errors, the type errors and the list of
// constructs nanogo refuses. Only the backend is arm64.
//
// arch is the target's, not the host's. An earlier version read
// runtime.GOARCH, which is the architecture of the nanogo binary, so
// GOARCH=amd64 passed the check on an arm64 host and nanogo emitted arm64
// machine code for an amd64 build. go tool link then reported an unknown
// relocation, which names neither the cause nor the fix.
func checkTarget(pkg, arch string) error {
	if arch == TargetArch {
		return nil
	}
	return &UnsupportedError{
		Package: pkg,
		What:    "for this target",
		Detail: fmt.Sprintf("nanogo emits %s machine code and the build is for %s (specs/043-amd64-backend.md is unbuilt)",
			TargetArch, arch),
	}
}

// emitPackage compiles every function in declaration order into one object.
//
// Declaration order, not map order: the object's symbol table is written by
// walking the lists in the order symbols were added, so two runs over the same
// input must add them in the same order (specs/053-determinism.md).
func emitPackage(cfg *Config, p *ir.Package, fset *syntax.FileSet, imports []export.Import, owed []*ir.Type) (*obj.Package, bool, error) {
	if err := checkTarget(cfg.Package, cfg.TargetArch()); err != nil {
		return nil, false, err
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
	// The type descriptors the lowered code names, unioned over the package.
	//
	// specs/032-type-descriptors-and-itabs.md makes the set a package emits
	// exactly the set its code names, and every reference comes from lowering:
	// an allocation passes a *_type to the runtime. The union is a slice in
	// first-use order rather than a set, because the object's symbol table is
	// written in the order symbols were added (specs/053-determinism.md).
	//
	// The descriptors the package owes an importer come first. gc emits one
	// for every type a package declares, used or not, because an importer
	// refers to it directly and through DWARF and cmd/link resolves neither on
	// its own. checkExportedTypes decided the set and refused the package if
	// any of it could not be written, so nothing here can fail on it.
	needed := append([]*ir.Type{}, owed...)
	// The itabs the constructed code names, unioned over the package the same
	// way. Only ssa.Build and ir.Lower name one, and both report what they
	// named, because cmd/link defines no go:itab. symbol.
	var itabs []ir.Itab
	// The two runtime caches an assertion to an interface and a type switch
	// over interface cases read, unioned the same way. Unlike a descriptor and
	// unlike an itab these are not canonical: each belongs to one site of one
	// function, and the runtime stores into it, so nothing merges two.
	// The functions whose static func value the code names. A literal that
	// captures nothing is one word holding the function's address, so one
	// symbol serves every evaluation and the linker merges the copies.
	var funcvals []string
	var (
		asserts  []ir.TypeAssert
		switches []ir.InterfaceSwitch
	)
	// The data symbols of the package-level variables. They go in before any
	// function is compiled, so that a variable nanogo cannot write is refused
	// by name and position rather than reported by the linker as an undefined
	// symbol, and so that the descriptors their pointer maps need join the set
	// above before it is emitted.
	globalTypes, err := ssagen.AddGlobals(out, p)
	if err != nil {
		var ge *ssagen.GlobalError
		if errors.As(err, &ge) {
			return nil, false, &UnsupportedError{
				Package: cfg.Package,
				What:    "package-level variable " + ge.Obj.Name + " at " + position(fset, ge.Obj.Pos, ge.Obj.Name),
				Detail:  ge.Reason,
			}
		}
		return nil, false, err
	}
	needed = append(needed, globalTypes...)
	// The functions the initialisation record runs, in the order they must
	// run. ir.Build puts one synthesised function in p.Inits: it assigns the
	// package-level variables in the order specs/012-type-checking.md
	// computed, and then calls each declared init in source order. The
	// declared inits are in p.Funcs and are compiled with everything else, so
	// only the synthesised one belongs in the record.
	var initFns []obj.SymRef
	for _, fn := range append(append([]*ir.Func{}, p.Funcs...), p.Inits...) {
		if fn.Bodyless {
			// A bodyless declaration is satisfied by assembly, and
			// checkAssembly has already refused a package that has any.
			// With -complete the go command promises there is none, so the
			// declaration is an error rather than an external definition.
			//
			// The test is ir.Func.Bodyless and not len(fn.Body), because
			// "func f() {}" leaves Body empty too and is a complete Go
			// function that this compiler must compile.
			if cfg.Complete {
				return nil, false, fmt.Errorf("%s: missing function body", position(fset, fn.Pos, fn.Name))
			}
			continue
		}
		if d, pos, ok := LifetimeDirective(fn.Pragma); ok {
			return nil, false, &UnsupportedError{
				Package: cfg.Package,
				What:    d + " on function " + fn.Name + " at " + position(fset, pos, fn.Name),
				Detail: "the directive makes a uintptr argument keep its referent alive across the call, " +
					"and no pass holds the pointer for it (specs/023-escape-analysis.md). " +
					"A function compiled without it collects the referent while the callee is reading it",
			}
		}
		r, c, err := compileFunc(cfg, fn, target, out, fset)
		if err != nil {
			return nil, false, err
		}
		ref, err := r.Add(out)
		if err != nil {
			return nil, false, &UnsupportedError{Package: cfg.Package, What: "function " + fn.Name, Detail: err.Error()}
		}
		if isPackageInit(p, fn) {
			initFns = append(initFns, ref)
		}
		needed = append(needed, c.Types...)
		itabs = append(itabs, c.Itabs...)
		funcvals = append(funcvals, c.FuncValues...)
		asserts = append(asserts, c.TypeAsserts...)
		switches = append(switches, c.InterfaceSwitches...)
	}
	// A package with no function body is not refused. internal/goarch and
	// internal/goos are constants and type aliases only, so what gc produces
	// for them is an archive whose whole content is the export data.
	// Refusing them said the writer of specs/015-export-data.md was missing,
	// not that code generation could not reach them, and the writer exists
	// now.
	// The descriptors go in after the text symbols because the list is not
	// complete until the last function is lowered. They are non-package
	// definitions and they land after ssagen's references, which obj allows:
	// a reference takes its index when the object is written.
	//
	// The closure is taken here rather than inside addDescriptors because the
	// generated functions are decided by the closed set and not by the roots.
	// A method wrapper is owed by whoever writes the descriptor that names it,
	// and a descriptor this object reached through another one is as much its
	// own as a root is.
	// An itab points at the interface's descriptor and the concrete type's,
	// and cmd/link resolves both by name, so the object that writes the itab
	// owes both. They join the roots before the set is closed, because the
	// method wrappers an itab's Fun entries name are decided by the closed set
	// and generatedFuncs reads that set.
	for _, it := range itabs {
		refs, err := rtype.ItabReferenced(it.Type, it.Iface)
		if err != nil {
			return nil, false, &UnsupportedError{
				Package: cfg.Package,
				What:    "a conversion its code makes to an interface with methods",
				Detail:  err.Error(),
			}
		}
		needed = append(needed, refs...)
	}
	// A runtime cache points at descriptors too: the interface a type
	// assertion targets, and every case an interface switch lists. They join
	// the roots here for the reason the itabs' do, and the failure a missing
	// one produces is the same: cmd/link resolves the relocation by name and
	// reports the descriptor, not the cache.
	for _, a := range asserts {
		needed = append(needed, rtype.TypeAssertReferenced(a)...)
	}
	for _, sw := range switches {
		needed = append(needed, rtype.InterfaceSwitchReferenced(sw)...)
	}
	types, err := descriptorSet(cfg, needed)
	if err != nil {
		return nil, false, err
	}
	fns, err := generatedFuncs(cfg, types)
	if err != nil {
		return nil, false, err
	}
	extra, generated, err := addGenerated(cfg, out, target, fset, fns)
	if err != nil {
		return nil, false, err
	}
	if len(extra) > 0 {
		// A generated function names descriptors of its own. They are roots
		// like any other, so the set is closed again over them.
		if types, err = descriptorSet(cfg, append(types, extra...)); err != nil {
			return nil, false, err
		}
	}
	if err := addDescriptors(cfg, out, types, itabs, funcvals, asserts, switches, generated); err != nil {
		return nil, false, err
	}
	// The record goes in last. It names the init function by the reference
	// r.Add returned, and it adds one non-package reference per import, so
	// every definition this object holds is already in place.
	//
	// Whether one was written travels to the export data, because an importer
	// orders its own record after this one only when the export data says
	// there is one to order after (specs/015-export-data.md).
	return out, addInitTask(out, imports, initFns), nil
}

// isPackageInit reports whether fn is the function ir.Build synthesised to run
// this package's initialisation.
//
// Identity, not the name: a declared "init" is an ordinary function in
// p.Funcs, and the synthesised one carries the same Name.
func isPackageInit(p *ir.Package, fn *ir.Func) bool {
	for _, in := range p.Inits {
		if in == fn {
			return true
		}
	}
	return false
}

// addGenerated compiles the generated functions into the object and returns
// the descriptors those functions name.
//
// generatedFuncs decided the set from the types whose descriptors this object
// writes, which is the same point gc decides it at.
//
// The functions go through compileFunc, the same passes a declared function
// goes through. A generated function that took a shorter path would be a
// second code generator, and the first bug it hit would be one the pipeline
// already handles.
// The second result names every symbol it defined. addDescriptors needs it:
// a reference is resolved by name and ABI together, and a generated function
// is a text symbol under ABIInternal while everything else a descriptor points
// at is data under ABI0.
func addGenerated(cfg *Config, out *obj.Package, target *ssa.Target, fset *syntax.FileSet, fns []*ir.Func) ([]*ir.Type, map[string]bool, error) {
	var needed []*ir.Type
	// generated is a lookup table and is never ranged over
	// (specs/053-determinism.md).
	generated := make(map[string]bool)
	for _, fn := range fns {
		if generated[fn.Sym] {
			// Two types reach one function: T and *T name one method wrapper,
			// and one type is in the closure once per path that reached it.
			continue
		}
		r, c, err := compileFunc(cfg, fn, target, out, fset)
		if err != nil {
			return nil, nil, err
		}
		if len(c.TypeAsserts) > 0 || len(c.InterfaceSwitches) > 0 {
			// A generated function questions no interface's dynamic type: a
			// method wrapper adjusts its receiver and tail-calls, and an
			// equality or hash routine reads memory. A cache named here would
			// be a symbol this pass reports to nobody, because the caller
			// takes the caches before the generated functions are compiled.
			return nil, nil, &UnsupportedError{
				Package: cfg.Package,
				What:    "the generated function " + fn.Sym,
				Detail: "it asserts a value to an interface, whose runtime cache this object owes and " +
					"the generated functions are compiled after the caches are collected; " +
					"specs/032-type-descriptors-and-itabs.md owns the gap",
			}
		}
		if len(c.Itabs) > 0 {
			// A generated function converts nothing to an interface: a method
			// wrapper adjusts its receiver and tail-calls, and an equality or
			// hash routine reads memory. So an itab named here would mean the
			// generator grew a construct whose own wrappers this pass would
			// have to close over a second time, and the closure above runs
			// once. It is reported rather than dropped, because a dropped itab
			// is an undefined symbol at link.
			return nil, nil, &UnsupportedError{
				Package: cfg.Package,
				What:    "the generated function " + fn.Sym,
				Detail: "it converts a value to an interface with methods, and the descriptor closure that " +
					"decides which wrappers an itab needs is taken before the generated functions are compiled; " +
					"specs/032-type-descriptors-and-itabs.md owns the gap",
			}
		}
		// Duplicate-tolerant, as gc marks it. Every package that writes a
		// descriptor naming this wrapper generates the wrapper too, because
		// neither can know which of them the linker will keep, and the two
		// definitions are the same function compiled from the same method set.
		r.Text.Flag |= obj.SymFlagDupok
		if _, err := r.Add(out); err != nil {
			return nil, nil, &UnsupportedError{Package: cfg.Package, What: "the generated function " + fn.Sym, Detail: err.Error()}
		}
		generated[fn.Sym] = true
		needed = append(needed, c.Types...)
	}
	return needed, generated, nil
}

// generatedFuncs returns the functions the descriptors of types name and no
// declaration defines.
//
// The types are descriptorSet's answer, so rtype could write every one of
// them and the descriptor asked for here cannot fail. It is asked rather than
// the decision being made a second time: rtype chose the type's algorithm and
// wrote the closure that names the function, so the closure's relocation is
// rtype's own answer, and a second decision could disagree with it.
func generatedFuncs(cfg *Config, types []*ir.Type) ([]*ir.Func, error) {
	fns, err := ssagen.MethodWrappers(types)
	if err != nil {
		return nil, &UnsupportedError{
			Package: cfg.Package,
			What:    "a method whose descriptor needs a generated wrapper",
			Detail:  err.Error(),
		}
	}
	for _, t := range types {
		set, err := rtype.Descriptor(t)
		if err != nil {
			return nil, &UnsupportedError{
				Package: cfg.Package,
				What:    "a type its code needs a descriptor for",
				Detail:  err.Error(),
			}
		}
		more, err := algFuncs(t, set)
		if err != nil {
			return nil, &UnsupportedError{
				Package: cfg.Package,
				What:    "a type whose descriptor needs a generated algorithm",
				Detail:  err.Error(),
			}
		}
		fns = append(fns, more...)
	}
	return fns, nil
}

// algFuncs returns the generated equality and hash functions t's descriptor
// names.
//
// The descriptor says what it needs. rtype decided the type's algorithm and
// wrote the closure that names the function, so the symbols it returned are
// scanned for those two names rather than the decision being made again here:
// a second decision could disagree with the first, and the disagreement would
// be a descriptor pointing at a function nothing defines.
func algFuncs(t *ir.Type, set []rtype.Symbol) ([]*ir.Func, error) {
	eq, err := ssagen.EqualSymbol(t)
	if err != nil {
		return nil, err
	}
	hash, err := ssagen.HashSymbol(t)
	if err != nil {
		return nil, err
	}
	var out []*ir.Func
	for _, s := range set {
		for _, r := range s.Relocs {
			var fn *ir.Func
			switch r.Target {
			case eq:
				fn, err = ssagen.EqualFunc(t)
			case hash:
				fn, err = ssagen.HashFunc(t)
			default:
				// A name belonging to another type is that type's own to
				// generate, and the closed set holds that type as well.
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, fn)
		}
	}
	return out, nil
}

// addDescriptors writes the type descriptors of the types the code names.
//
// The list is descriptorSet's answer and is already closed, so nothing here
// discovers a type. A type reached this far because the lowering pass named it
// and rtype could write it, which is the check descriptorSet made: a type that
// could be named and not written stops there, after the function it came from
// compiled, so the refusal names the type rather than the function.
//
// The descriptor itself is a named non-package definition and the data it
// points at is content-addressable. That is gc's split, not a choice.
//
// It is not a package definition. A descriptor is dupok, because every package
// that names a type writes one, and cmd/link deduplicates a dupok symbol by
// name. It does that only in the non-package index space: loader.addSym takes
// a package definition as unique by construction and adds it to the binary
// without asking whether the name is already there. gc states the same rule
// the other way round, in obj.isNonPkgSym: a dupok symbol is a non-package
// symbol. A descriptor written as a package definition is a second *int32 in
// a binary that already has the runtime's, and runtime.SetFinalizer, which
// compares two type descriptors by address, then refuses a *int32 for a
// func(*int32).
//
// It is not content-addressable either: cmd/link reads no name for a symbol in
// the hashed index space, so a hashed descriptor could not be resolved by name
// from another object and would not be collected into runtime.typelinks.
// specs/032 says AddHashedDef for all of them, and it is wrong about the
// first one.
//
// # The itabs
//
// They go in the same two passes and not in a pass of their own, because an
// itab points at the interface's descriptor and the concrete type's, and this
// object defines both. A second pass with its own tables would emit a
// non-package reference for a name this object already defines, and cmd/link
// would resolve the reference to the definition it merged rather than to the
// one beside it, if it resolved it at all.
//
// Each is one symbol, a named non-package definition, dupok. Named and not
// hashed for the reason a descriptor is named, and one reason more: cmd/link
// collects a go:itab. symbol into runtime.itablinks by its name prefix, and it
// reads no name for a symbol in the hashed index space.
func addDescriptors(cfg *Config, out *obj.Package, types []*ir.Type, itabs []ir.Itab, funcvals []string, asserts []ir.TypeAssert, switches []ir.InterfaceSwitch, generated map[string]bool) error {
	// defs and refs are lookup tables and are never ranged over
	// (specs/053-determinism.md). A symbol is emitted once however many types
	// name it: two descriptors share one pointer bitmask whenever their
	// pointer maps agree.
	defs := make(map[string]obj.SymRef)
	refs := make(map[string]obj.SymRef)
	// The relocations are applied in a second pass, because a descriptor
	// names symbols that come later in its own list and names the descriptor
	// of another type, which may be emitted by another package.
	var (
		syms   []*obj.Symbol
		relocs [][]rtype.Reloc
	)
	for _, t := range types {
		set, err := rtype.Descriptor(t)
		if err != nil {
			return &UnsupportedError{
				Package: cfg.Package,
				What:    "a type its code needs a descriptor for",
				Detail:  err.Error(),
			}
		}
		for i, s := range set {
			if _, ok := defs[s.Name]; ok {
				continue
			}
			d := &obj.Symbol{
				Name:  s.Name,
				Type:  s.Kind,
				Size:  rtypeSize(s),
				Align: s.Align,
				Data:  s.Data,
			}
			if s.Dupok {
				d.Flag |= obj.SymFlagDupok
			}
			if s.UsedInIface {
				// rtype says which symbols carry it and why. Without it
				// cmd/link prunes every method of the type and the runtime
				// installs runtime.unreachableMethod in its place.
				d.Flag2 |= obj.SymFlagUsedInIface
			}
			// rtype documents the first symbol as the descriptor and the rest
			// as the data it points at.
			if i == 0 {
				defs[s.Name] = out.AddNonPkgDef(d)
			} else {
				defs[s.Name] = out.AddHashedDef(d)
			}
			syms = append(syms, d)
			relocs = append(relocs, s.Relocs)
		}
	}
	for _, fn := range funcvals {
		name := fn + irFuncValueSuffix
		if _, ok := defs[name]; ok {
			// Two functions of this package took the value of one function,
			// or one took it twice through two literals of the same body.
			continue
		}
		// One word holding the function's address, which is what a func value
		// is when the literal captures nothing. gc writes the same symbol,
		// dupok and read-only.
		//
		// A non-package definition and not a hashed one, because the code
		// refers to it by name and a hashed symbol's identity is its content.
		// Not a package definition either: obj refuses a duplicate-tolerant
		// symbol there, because cmd/link deduplicates by name in the
		// non-package index space only, and two packages do emit this one when
		// both take the value of a function one of them declares.
		d := &obj.Symbol{
			Name:  name,
			Type:  obj.SRODATA,
			Size:  uint32(irPtrSize),
			Align: uint32(irPtrSize),
			Data:  make([]byte, irPtrSize),
			Flag:  obj.SymFlagDupok,
		}
		defs[name] = out.AddNonPkgDef(d)
		syms = append(syms, d)
		relocs = append(relocs, []rtype.Reloc{{
			Off: 0, Size: 8, Type: obj.R_ADDR, Target: fn, GoFunc: true,
		}})
	}
	for _, it := range itabs {
		set, err := rtype.Itab(it.Type, it.Iface)
		if err != nil {
			return &UnsupportedError{
				Package: cfg.Package,
				What:    "a conversion its code makes to an interface with methods",
				Detail:  err.Error(),
			}
		}
		for _, s := range set {
			if _, ok := defs[s.Name]; ok {
				// Two functions convert the same pair, or two passes named it.
				continue
			}
			d := &obj.Symbol{
				Name:  s.Name,
				Type:  s.Kind,
				Size:  rtypeSize(s),
				Align: s.Align,
				Data:  s.Data,
			}
			if s.Dupok {
				d.Flag |= obj.SymFlagDupok
			}
			defs[s.Name] = out.AddNonPkgDef(d)
			syms = append(syms, d)
			relocs = append(relocs, s.Relocs)
		}
	}
	// The two runtime caches. Each is a package definition and not a
	// non-package one, which is the opposite of everything above and follows
	// from the same rule: cmd/link deduplicates by name in the non-package
	// index space, and these must not be deduplicated. The runtime stores a
	// table into the symbol, so two sites that shared one would be two
	// questions writing one table.
	//
	// They are also the only symbols here in a writable section, and the only
	// ones that carry a Go type. rtype/switch.go states why.
	var cached []rtype.Symbol
	for _, a := range asserts {
		s, err := rtype.TypeAssert(a)
		if err != nil {
			return &UnsupportedError{
				Package: cfg.Package,
				What:    "an assertion its code makes to an interface with methods",
				Detail:  err.Error(),
			}
		}
		cached = append(cached, s)
	}
	for _, sw := range switches {
		s, err := rtype.InterfaceSwitch(sw)
		if err != nil {
			return &UnsupportedError{
				Package: cfg.Package,
				What:    "a type switch its code makes over interface cases",
				Detail:  err.Error(),
			}
		}
		cached = append(cached, s)
	}
	// nonPkgRef is the reference a name that no definition here covers takes.
	// It is shared by the relocation pass below and by the Go type of a cache,
	// so that one name is one reference however it is reached.
	nonPkgRef := func(name string, abi uint16) obj.SymRef {
		if ref, ok := refs[name]; ok {
			return ref
		}
		ref := out.AddNonPkgRef(&obj.Symbol{Name: name, ABI: abi})
		// go tool nm and go tool objdump print a name only for a symbol the
		// RefName block covers.
		out.AddRefName(ref, name)
		refs[name] = ref
		return ref
	}
	for _, s := range cached {
		if _, ok := defs[s.Name]; ok {
			return &UnsupportedError{
				Package: cfg.Package,
				What:    "the runtime cache " + s.Name,
				Detail:  "two sites name one cache, and the runtime writes a table into each",
			}
		}
		d := &obj.Symbol{
			Name:  s.Name,
			Type:  s.Kind,
			Size:  rtypeSize(s),
			Align: s.Align,
			Data:  s.Data,
		}
		if s.Local {
			d.Flag |= obj.SymFlagLocal
		}
		if s.Gotype != "" {
			// A data symbol, so ABI0. cmd/link builds the pointer map of the
			// data section out of this entry, and without it the link stops
			// with "missing Go type information for global symbol".
			d.Aux = append(d.Aux, obj.Aux{Type: obj.AuxGotype, Sym: nonPkgRef(s.Gotype, obj.ABI0)})
		}
		defs[s.Name] = out.AddDef(d)
		syms = append(syms, d)
		relocs = append(relocs, s.Relocs)
	}
	for i, s := range syms {
		for _, r := range relocs[i] {
			ref, ok := defs[r.Target]
			if !ok {
				ref = nonPkgRef(r.Target, targetABI(r, generated))
			}
			s.Relocs = append(s.Relocs, obj.Reloc{
				Off: r.Off, Size: r.Size, Type: r.Type, Add: r.Add, Sym: ref,
			})
		}
	}
	return nil
}

// targetABI returns the ABI of a symbol a descriptor points at.
//
// The ABI is half of a symbol's identity and cmd/link resolves a by-name
// reference by name and ABI together, so a reference under the wrong one names
// a symbol nothing defines. Almost everything a descriptor points at is data,
// which gc leaves at ABI0. Two things are not.
//
// The first is the equality or hash routine a closure holds when the runtime
// owns it. rtsym is what says so, because specs/031-runtime-lowering.md makes
// rtsym the only place a runtime symbol is spelled, and a runtime symbol that
// exists only in assembly is ABI0 again, for the reason ssagen's
// morestackCallee records.
//
// The second is a function this object generated. The linker reports the
// mismatch clearly when it is missed, and the message is worth recording
// because it reads like a missing definition rather than a wrong reference:
//
//	type:.eqfunc.main.key: relocation target type:.eq.main.key not defined
//	for ABI0 (but is defined for ABIInternal)
func targetABI(r rtype.Reloc, generated map[string]bool) uint16 {
	name := r.Target
	if r.GoFunc {
		// A Method's Ifn or Tfn. It names a function the front end compiled or
		// a wrapper ssagen generated, and either way it is a Go function and
		// therefore ABIInternal. The name does not say so: the method of a
		// type this package declares is not in the generated set, because
		// nothing generated it.
		return obj.ABIInternal
	}
	if generated[name] {
		// A function this object generated. It is a Go function and it is
		// compiled the way every Go function is, so it is ABIInternal. The
		// set is asked rather than the name matched: a method wrapper is
		// spelled pkg.(*T).M and nothing in that name says it was generated.
		return obj.ABIInternal
	}
	if s := rtsym.Lookup(name); s != nil && !s.Assembly {
		return obj.ABIInternal
	}
	return obj.ABI0
}

// compileFunc takes one function from IR to a symbol, and returns the types
// whose descriptors the lowered tree names.
//
// The passes are a list and not a sequence of statements because every one of
// them reports the same way: a failure names the pass, the function and the
// position, and the pass name is what tells a reader which spec owns the gap.
//
// The order is specs/002-architecture.md's: specs/020-ir.md's lowering table,
// then construction, then decomposition, then specs/030-abi.md's assignment,
// then selection. Lowering is first because ssa.Build refuses every
// Go-specific node, which is what keeps that table a finite list rather than a
// habit. The assignment runs between decomposition and selection because it
// finishes work decomposition stopped at its bound and because the rewrites it
// makes still need lowering rules. The verifier runs after each of the two
// rewriting passes, so a violation names the pass that made it.
//
// The stage is named ir.Lower and not "lowering" because ssa.Lower is in the
// same list, and a refusal has to say which of the two spec decks owns the gap.
func compileFunc(cfg *Config, fn *ir.Func, target *ssa.Target, out *obj.Package, fset *syntax.FileSet) (*ssagen.Result, ir.Collected, error) {
	var (
		f *ssa.Func
		a *ssa.Alloc
		r *ssagen.Result
		c ir.Collected
	)
	passes := []struct {
		name string
		run  func() error
	}{
		{"ir.Lower", func() (err error) { c, err = ir.LowerAndCollect(fn); return err }},
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
				Sym:           fn.Sym,
				ABI:           obj.ABIInternal,
				File:          file,
				Line:          line,
				Fset:          fset,
				ReflectMethod: fn.ReflectMethod,
			})
			return err
		}},
	}
	for _, p := range passes {
		if err := p.run(); err != nil {
			return nil, ir.Collected{}, unsupportedFunc(cfg, fset, fn, p.name, err)
		}
	}
	// Two passes name descriptors and the package owes both sets. ir.Lower
	// reports the ones its table names, and no row of that table reaches a
	// conversion to an interface, so ssa.Build names a second set: the type
	// word of every interface value it builds. Returning only the first leaves
	// the reference without a definition, and the failure is the linker's
	// rather than the compiler's: "relocation target type:main.myInt not
	// defined". type:int and type:string hide it, because the runtime defines
	// those already.
	//
	// The itabs are a third set and both passes name them, for the same two
	// reasons the descriptors have two sets: ir.Lower compares the first word
	// of a value that leads with an *itab against the itab of the pair, and
	// ssa.Build builds that word when a value goes into an interface. An itab
	// is not a descriptor: it is named per (concrete type, interface) pair,
	// cmd/link collects it into runtime.itablinks by name, and rtype writes it
	// from a different encoder.
	//
	// The two runtime caches have one set and not two. They come from
	// specs/020's assertion and type switch rows alone, which is where an
	// interface's dynamic type is questioned; construction builds interface
	// values and never asks one a question.
	c.Types = append(c.Types, f.Descriptors...)
	c.Itabs = append(c.Itabs, f.Itabs...)
	return r, c, nil
}

// unsupportedFunc is the refusal every pass of compileFunc reports.
//
// One function and not one per pass, because the shape is the content: the
// message names the pass, so a reader knows which spec deck owns the gap, and
// it names the function and the position, because the allowlist entry that let
// the package through names only the package. A refusal that named only the
// package would send a reader to a file rather than to a line.
func unsupportedFunc(cfg *Config, fset *syntax.FileSet, fn *ir.Func, pass string, err error) error {
	return &UnsupportedError{
		Package: cfg.Package,
		What:    "function " + fn.Name + " at " + position(fset, fn.Pos, fn.Name),
		Detail:  pass + ": " + err.Error(),
	}
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
//
// The archive carries the export data as well, so that a package nanogo
// compiled can be imported (specs/015-export-data.md), with the function
// bodies an importer can inline. The export data is
// written whether or not -pack asked for an archive, because its fingerprint
// goes into the object's header: an importing object records the same value
// in its Autolib entry and the linker refuses a build whose two copies
// disagree. A bare object has nowhere to put the __.PKGDEF member, so it
// carries the fingerprint and not the data.
func writeOutput(cfg *Config, p *obj.Package, pkg *types2.Package, hasInit bool, bodies *export.Source) error {
	tc, err := verifyToolchain()
	if err != nil {
		return fmt.Errorf("%s: %v", cfg.Package, err)
	}
	payload, fingerprint, err := export.Write(pkg, hasInit, bodies)
	if err != nil {
		return err
	}
	p.Fingerprint = fingerprint
	f, err := os.Create(cfg.Output)
	if err != nil {
		return err
	}
	write := func() error {
		if !cfg.Pack {
			return p.WriteObject(f, tc.Header)
		}
		definition, err := export.Definition(tc.Header, p.Main, payload)
		if err != nil {
			return err
		}
		return writeArchive(f, p, tc.Header, definition)
	}
	if err := write(); err != nil {
		f.Close()
		return fmt.Errorf("%s: %v", cfg.Package, err)
	}
	return f.Close()
}

// irFuncValueSuffix and irPtrSize are ir's, named here so that this file does
// not spell either a second time.
const (
	irFuncValueSuffix = "\u00b7f"
	irPtrSize         = 8
)

// rtypeSize is the size of the object symbol a descriptor symbol defines.
//
// The two disagree for the zero-filled kind, which rtype.Symbol.Size states:
// the pointer mask the runtime fills in on demand has a size and no bytes, and
// a definition of len(Data) for it is a symbol of no space that the runtime
// then writes a word into.
func rtypeSize(s rtype.Symbol) uint32 {
	if s.Size != 0 {
		return s.Size
	}
	return uint32(len(s.Data))
}
