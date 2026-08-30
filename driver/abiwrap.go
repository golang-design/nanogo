// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/ssagen"
	"golang.design/x/nanogo/syntax"
)

// The ABI wrapper decision of specs/047-abi-wrappers.md.
//
// It is ssagen.GenABIWrappers, read out of gc and written here:
//
//	symName = sym.Linkname, or sym.Pkg.Prefix + "." + sym.Name
//	fn.ABI  = defABI                       // where the symabis file has a def
//	fn.ABIRefs |= refs                     // from the ref lines
//	fn.ABIRefs.Set(obj.ABIInternal, true)  // unconditionally
//	fn.ABIRefs |= obj.ABISetCallable       // linkname, defined here, not cgo
//	need := fn.ABIRefs &^ obj.ABISetOf(fn.ABI)
//
// One wrapper comes out per ABI still in need, and the two shapes go opposite
// ways.
//
// A bodyless declaration the assembly defines under ABI0 owes an ABIInternal
// wrapper, because the unconditional ABIInternal reference is what need holds.
// The wrapper takes its arguments in registers, writes them into the outgoing
// area at ABI0 offsets, calls the assembly, and reads the results back. That
// is stage 2 and it is built: [abiWrapperSet] collects them and
// [ssagen.ABIWrapper] builds each one.
//
// A function assembly calls under ABI0, or one carrying a //go:linkname the
// package defines, owes an ABI0 wrapper whose own convention is ABI0. That is
// stage 3 and it is not built, so it is refused by name.

// abiWrapperSet is what a package's assembly asks the compiler for.
type abiWrapperSet struct {
	// Wrappers holds one bodyless declaration per ABIInternal wrapper owed,
	// in declaration order.
	//
	// The same declaration owes exactly one argument map, so the two are one
	// list and not two. specs/047-abi-wrappers.md records why they cannot be
	// separated: cmd/internal/obj emits a FUNCDATA reference to
	// <sym>.args_stackmap for every ABI0 text symbol the assembler produces,
	// gc defines that symbol only where fn.ABI is ABI0, fn.ABI is ABI0 only
	// from a symabis def that matched a Go declaration, and GenABIWrappers
	// then puts ABIInternal in need unconditionally.
	Wrappers []*abiWrapper
}

// abiWrapper is one bodyless declaration the assembly defines under ABI0.
type abiWrapper struct {
	// Fn is the declaration. It has no body and nanogo compiles none for it.
	Fn *ir.Func

	// Sym is the linker symbol the assembly defines and the wrapper takes,
	// after the //go:linkname. It is the name both the wrapper's own text
	// symbol and its argument map are built from.
	Sym string

	// Linkname and LinknameStd are the attribute the declaration's own symbol
	// carries, which gc copies onto the wrapper.
	//
	// The copy is not cosmetic. cmd/link's loader checks a reference to an
	// assembly symbol of another package by looking the *other* ABI's symbol
	// up by name, which is this wrapper, and reading the attribute off it:
	// "For an assembly symbol, check if there is a linkname applied to its ABI
	// wrapper." A wrapper that loses the attribute turns a legitimate pull
	// into a link error.
	//
	// The two are separate bits and never both. //go:linkname sets the first
	// and //go:linknamestd sets the second, which is what gc prints:
	// internal/runtime/atomic.Xadd's wrapper carries LINKNAMESTD alone and
	// internal/cpu.sysctlEnabled's carries LINKNAME alone.
	Linkname    bool
	LinknameStd bool
}

// checkABIWrappers takes the wrapper decision and refuses what stage 2 does
// not build.
//
// It runs after ir.Build, because the question it asks is which of this
// package's declarations the assembly defines, and that needs the
// declarations.
func checkABIWrappers(cfg *Config, s *SymABIs, seen *sourceDirectives, p *ir.Package, fset *syntax.FileSet) (*abiWrapperSet, error) {
	if cfg.SymABIs == "" && cfg.AsmHdr == "" {
		// No assembly. A def or ref line cannot exist and no wrapper can be
		// owed, whatever the package writes. A //go:linkname here is
		// specs/016-directives-and-pragmas.md's recorded-and-dropped gap and
		// not this spec's, and refusing it would take out packages that
		// compile today.
		return &abiWrapperSet{}, nil
	}
	// A cgo export pragma is refused before any of the decision below is
	// taken, because gc's decision reads it: the cgo branch suppresses every
	// wrapper for the symbol and fails the build if one was owed anyway.
	if seen.ABI != "" {
		return nil, &UnsupportedError{
			Package: cfg.Package,
			What:    seen.ABI + " in a package with assembly at " + position(fset, seen.Pos, cfg.Package),
			Detail:  seen.Reason,
		}
	}
	links, err := ParseLinknames(cfg.Package, seen)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", cfg.Package, err)
	}
	if err := checkLinknameRenames(cfg, links, p, fset); err != nil {
		return nil, err
	}
	set := &abiWrapperSet{}
	for _, fn := range p.Funcs {
		if pos, ok := cgoUnsafeArgsDirective(fn.Pragma); ok {
			return nil, &UnsupportedError{
				Package: cfg.Package,
				What:    "//go:cgo_unsafe_args on function " + fn.Name + " at " + position(fset, pos, fn.Name),
				Detail: "it pins the function to ABI0 and propagates the linkname attribute to the ABI0 " +
					"symbol, because the callee walks its arguments by offset (specs/047-abi-wrappers.md)",
			}
		}
		sym := links.SymOf(fn)
		defABI, hasDef := s.Def(sym)
		if hasDef && !fn.Bodyless {
			// gc's "%v defined in both Go and assembly". The symbol would be
			// defined twice and the linker would report the duplicate without
			// naming either source.
			return nil, fmt.Errorf("%s: %s defined in both Go and assembly",
				position(fset, fn.Pos, fn.Name), fn.Name)
		}
		abi := SymABI(obj.ABIInternal)
		if hasDef {
			abi = defABI
		}
		refs := s.Refs(sym) | abiSetOf(obj.ABIInternal)
		if _, ok := links.Of(fn.Name); ok && fn.Recv == nil && (!fn.Bodyless || hasDef) {
			// gc: a symbol this package defines and gives a linkname to may
			// be named from another package under either convention, so it is
			// callable under both. The first term is true for the
			// one-argument form as well, because noder fills the target in
			// with the default name, and that is what puts
			// internal/runtime/atomic's forty-nine assembly declarations in
			// this branch.
			refs |= abiSetCallable
		}
		need := refs &^ abiSetOf(abi)
		if need == 0 {
			continue
		}
		what := "function " + fn.Name + " at " + position(fset, fn.Pos, fn.Name)
		if need.Has(obj.ABI0) {
			return nil, &UnsupportedError{
				Package: cfg.Package,
				What:    "an assembly call to " + what,
				Detail: "the assembly names it under ABI0, or a //go:linkname makes it callable under " +
					"both conventions, and the Go definition is ABIInternal, so it needs the ABI0 " +
					"wrapper of specs/047-abi-wrappers.md stage 3, which is unbuilt",
			}
		}
		if fn.Recv != nil {
			// gc errors with "makeABIWrapper support for wrapping methods not
			// implemented", so refusing costs no compatibility. The receiver
			// is placed before the arguments and nanogo's call walk recovers
			// one by trying two operand lists, which specs/030-abi.md already
			// names as a bound.
			return nil, &UnsupportedError{
				Package: cfg.Package,
				What:    "a Go call to method " + fn.Name + " at " + position(fset, fn.Pos, fn.Name),
				Detail: "gc refuses the same shape with \"makeABIWrapper support for wrapping methods not " +
					"implemented\", and the receiver takes offset 0 of the ABI0 area " +
					"(specs/047-abi-wrappers.md)",
			}
		}
		if !fn.Bodyless {
			// need holds ABIInternal and fn.ABI is ABIInternal, so this is
			// unreachable by the arithmetic above. It is checked rather than
			// assumed, because the wrapper below would define a second symbol
			// under a name the ordinary pipeline is already defining.
			return nil, fmt.Errorf("%s: %s owes an ABIInternal wrapper and has a Go body",
				position(fset, fn.Pos, fn.Name), fn.Name)
		}
		w := &abiWrapper{Fn: fn, Sym: sym}
		if l, ok := links.Of(fn.Name); ok && fn.Recv == nil {
			w.Linkname, w.LinknameStd = !l.Std, l.Std
		}
		set.Wrappers = append(set.Wrappers, w)
	}
	return set, nil
}

// checkLinknameRenames refuses a //go:linkname that renames a definition
// nanogo emits.
//
// The ABI decision needs the linkname only as the name a symabis line is
// matched against, and that is built. What is not built is the other half:
// emitting the definition under the new name. nanogo derives a symbol from
// the declaration in ir.Build and every pass below reads it, so a renamed
// function would be defined under the name the source wrote and every
// reference to the renamed symbol would resolve to nothing.
//
// A bodyless declaration is not refused. nanogo emits no definition for one,
// so nothing is named wrongly: internal/bytealg's
// `//go:linkname abigen_runtime_cmpstring runtime.cmpstring` stands over a
// declaration the assembly defines, and matching the def line against the new
// name is the whole of what that package needs from this pass.
//
// The scope is a package with assembly, which is this file's scope. A package
// without one that writes a renaming directive still compiles with the
// directive recorded and dropped, which is
// specs/016-directives-and-pragmas.md's gap and predates this spec.
func checkLinknameRenames(cfg *Config, links *Linknames, p *ir.Package, fset *syntax.FileSet) error {
	defined := make(map[string]*ir.Func, len(p.Funcs))
	for _, fn := range p.Funcs {
		if fn.Recv == nil && !fn.Bodyless {
			defined[fn.Name] = fn
		}
	}
	// A package-level variable is keyed by the object symbol ir.Build gives
	// it, which carries the package prefix already, where a function is keyed
	// by the bare name. The two spellings are why the lookups below differ:
	// keying a global by the identifier alone finds nothing, and a renamed
	// variable then goes through and is emitted under the name the source
	// wrote.
	globals := make(map[string]*ir.Object, len(p.Globals))
	for _, o := range p.Globals {
		globals[o.Name] = o
	}
	for _, l := range links.All() {
		if !l.Renames {
			continue
		}
		var (
			what string
			pos  syntax.Pos
			name string
		)
		switch {
		case defined[l.Local] != nil:
			what, pos, name = "function", defined[l.Local].Pos, l.Local
		case globals[l.Default] != nil:
			what, pos, name = "package-level variable", globals[l.Default].Pos, l.Local
		default:
			// A renaming directive over a bodyless declaration, or over a
			// name this package does not define at all. Neither makes nanogo
			// emit a definition, so the name is only ever matched and the
			// match is built.
			continue
		}
		return &UnsupportedError{
			Package: cfg.Package,
			What: "//go:linkname " + l.Local + " " + l.Target + " at " +
				position(fset, pos, name),
			Detail: "it renames the " + what + " nanogo defines, and nanogo derives every symbol from " +
				"the declaration, so the definition would be emitted under the name the source wrote " +
				"and every reference to " + l.Target + " would resolve to nothing " +
				"(specs/047-abi-wrappers.md)",
		}
	}
	return nil
}

// abiSetCallable is gc's obj.ABISetCallable: the set of every ABI a function
// could be called with.
//
// It is spelled out rather than taken as "every bit", because obj.ABICount is
// the number of conventions the object format knows and a third one would not
// make a symbol callable under it.
const abiSetCallable = abiSet(1<<obj.ABI0) | abiSet(1<<obj.ABIInternal)

// addABIWrappers compiles the wrapper each entry owes into the object, with
// the argument map and the argument info that go with it.
//
// The order is the declaration order the decision collected, which is a walk
// of ir.Package.Funcs and never of a map (specs/053-determinism.md).
//
// All three symbols are emitted together or none is. The wrapper without the
// map leaves the assembly object's FUNCDATA reference undefined and the link
// fails, and the map without the wrapper leaves every Go call to the
// declaration naming a symbol nothing defines. specs/047-abi-wrappers.md
// records that the two cannot be separated, and this is the one place that
// depends on it.
func addABIWrappers(cfg *Config, out *obj.Package, target *ssa.Target, fset *syntax.FileSet, set *abiWrapperSet) error {
	if set == nil {
		return nil
	}
	for _, w := range set.Wrappers {
		where := "a Go call to function " + w.Fn.Name + " at " + position(fset, w.Fn.Pos, w.Fn.Name)
		fn, err := ssagen.ABIWrapper(w.Fn, w.Sym)
		if err != nil {
			return &UnsupportedError{Package: cfg.Package, What: where, Detail: err.Error()}
		}
		stackmap, arginfo, err := ssagen.ArgMaps(w.Fn, w.Sym, target)
		if err != nil {
			return &UnsupportedError{Package: cfg.Package, What: where, Detail: err.Error()}
		}
		r, _, err := compileFunc(cfg, fn, target, out, fset)
		if err != nil {
			return err
		}
		// Duplicate-tolerant, as gc marks it. The declaration and its
		// assembly may be built into more than one archive, and the wrapper
		// is the same function derived from the same signature either way.
		r.Text.Flag |= obj.SymFlagDupok
		r.Text.Flag2 |= obj.SymFlagABIWrapper
		if w.Linkname {
			r.Text.Flag2 |= obj.SymFlagLinkname
		}
		if w.LinknameStd {
			r.Text.Flag2 |= obj.SymFlagLinknameStd
		}
		// The three second-byte flags gc sets on this symbol. NOSPLIT is the
		// fourth and nanogo does not set it: specs/035-goroutines-and-stack-growth.md
		// forbids claiming a property nanogo does not compute, and the cost
		// is one stack-growth check in the wrapper.
		if _, err := r.Add(out); err != nil {
			return &UnsupportedError{Package: cfg.Package, What: where, Detail: err.Error()}
		}
		out.AddDef(stackmap)
		out.AddDef(arginfo)
	}
	return nil
}
