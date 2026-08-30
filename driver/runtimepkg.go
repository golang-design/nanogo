// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import "sort"

// runtimePkgs is objabi.runtimePkgs, transcribed.
//
// gc decides that it is compiling a runtime package from the package path and
// not from a flag. cmd/compile/internal/base/flag.go:
//
//	// Compute whether we're compiling the runtime from the package path. Test
//	// code can also use the flag to set this explicitly.
//	if Flag.Std && objabi.LookupPkgSpecial(Ctxt.Pkgpath).Runtime {
//	    Flag.CompilingRuntime = true
//	}
//
// The go command sends -+ for no package: there is no occurrence of the flag
// anywhere in cmd/go. specs/050-driver.md listed it as "sent conditionally,
// when compiling the runtime" and that row was wrong, so nanogo's refusal for
// it fired for nothing. specs/047-abi-wrappers.md measured the consequence:
// every one of the eight packages the bootstrap closure refuses for assembly
// is also a runtime package, and the assembly refusal was the only thing
// standing in front of them.
//
// The list is transcribed rather than derived because cmd/internal/objabi is
// not importable from outside cmd, and it is checked against GOROOT by
// TestRuntimePkgsMatchesTheToolchain rather than trusted, for the reason
// rtsym gives for the runtime symbol table: a list that drifts from the
// toolchain is a wrong answer with no diagnostic, and a checked list turns
// that into a build failure.
//
// The order is objabi's, which is the order a reader comparing the two files
// needs. Lookup goes through [RuntimePackage], which does not depend on it.
var runtimePkgs = []string{
	"runtime",

	"internal/runtime/atomic",
	"internal/runtime/cgroup",
	"internal/runtime/exithook",
	"internal/runtime/gc",
	"internal/runtime/gc/scan",
	"internal/runtime/maps",
	"internal/runtime/math",
	"internal/runtime/sys",
	"internal/runtime/syscall/linux",
	"internal/runtime/syscall/windows",

	"internal/abi",
	"internal/bytealg",
	"internal/byteorder",
	"internal/chacha8rand",
	"internal/coverage/rtcov",
	"internal/cpu",
	"internal/goarch",
	"internal/godebugs",
	"internal/goexperiment",
	"internal/goos",
	"internal/profilerecord",
	"internal/strconv",
	"internal/stringslite",
}

// runtimePkgSet is the lookup table. It is built once and never ranged over
// (specs/053-determinism.md).
var runtimePkgSet = func() map[string]bool {
	m := make(map[string]bool, len(runtimePkgs))
	for _, p := range runtimePkgs {
		m[p] = true
	}
	return m
}()

// RuntimePackage reports whether path is one of the packages gc compiles with
// the runtime rules on.
//
// It answers for the path alone. The property gc computes is this and -std
// together, because a module of a user's own may hold a package whose import
// path is "internal/abi" and it is not the standard library's. See
// [Config.RuntimeRules].
func RuntimePackage(path string) bool {
	return runtimePkgSet[path]
}

// RuntimePackages returns the transcribed list, sorted.
//
// It exists for the drift test and for a diagnostic that has to name the set.
// The copy is sorted rather than in objabi's order so that a caller comparing
// two lists compares sets and not spellings.
func RuntimePackages() []string {
	out := append([]string(nil), runtimePkgs...)
	sort.Strings(out)
	return out
}

// RuntimeRules reports whether gc would compile this package with
// Flag.CompilingRuntime set.
//
// It is gc's own disjunction: the explicit flag, or the standard library's
// copy of a package in objabi.runtimePkgs. -std is half of it because the
// property belongs to the standard library's package and not to any package
// that happens to share its import path.
//
// What the property changes in gc is mostly optimization and instrumentation
// policy (base/flag.go pins -N off, turns optimization on, and disables
// checkptr and libfuzzer). Two clauses change what a program means, and
// specs/047-abi-wrappers.md names both: noder permits //go:systemstack and
// the three write-barrier directives only in a runtime package, and
// liveness.IsUnsafe treats every function in one as though it were nosplit.
//
// nanogo does not refuse the whole property. Thirteen of the twenty-three
// packages in the list compile today, so a blanket refusal would be a
// regression rather than a repair. The refusals are package runtime itself,
// in [checkSupported], and the per-function directive gate of
// [RuntimeDirective].
func (c *Config) RuntimeRules() bool {
	return c.CompilingRuntime || (c.Std && RuntimePackage(c.Package))
}
