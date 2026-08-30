// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestRuntimePkgsMatchesTheToolchain is the reason [runtimePkgs] can be
// trusted.
//
// cmd/internal/objabi is not importable from outside cmd, so the list is
// transcribed. A transcribed list is a list of guesses until something checks
// it, and the failure a wrong one produces is silent in both directions: a
// package missing from it compiles with a directive nanogo cannot honour, and
// a package wrongly in it is refused for a rule that does not apply to it.
// rtsym checks its runtime symbol table against GOROOT for the same reason.
func TestRuntimePkgsMatchesTheToolchain(t *testing.T) {
	want := toolchainRuntimePkgs(t)
	got := RuntimePackages()
	if strings.Join(got, "\n") == strings.Join(want, "\n") {
		return
	}
	t.Errorf("runtimePkgs has drifted from objabi.runtimePkgs\nnanogo:\n%s\ntoolchain:\n%s",
		strings.Join(got, "\n"), strings.Join(want, "\n"))
}

// toolchainRuntimePkgs reads objabi.runtimePkgs out of GOROOT.
//
// The source is parsed rather than matched with a regular expression, because
// the value is a composite literal of string constants and go/parser is the
// only reader that cannot be fooled by a comment holding a package path.
func toolchainRuntimePkgs(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(goruntime.GOROOT(), "src", "cmd", "internal", "objabi", "pkgspecial.go")
	src, err := os.ReadFile(path)
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s could not be read: %v", path, err)
		}
		t.Skip("no cmd/internal/objabi source under GOROOT")
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var out []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}
		for _, spec := range g.Specs {
			v, ok := spec.(*ast.ValueSpec)
			if !ok || len(v.Names) != 1 || v.Names[0].Name != "runtimePkgs" || len(v.Values) != 1 {
				continue
			}
			lit, ok := v.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("%s: runtimePkgs is not a composite literal", path)
			}
			for _, e := range lit.Elts {
				s, ok := e.(*ast.BasicLit)
				if !ok || s.Kind != token.STRING {
					t.Fatalf("%s: runtimePkgs holds %T and not a string", path, e)
				}
				p, err := strconv.Unquote(s.Value)
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no runtimePkgs found", path)
	}
	sort.Strings(out)
	return out
}

// TestRuntimeRulesIsGcsDisjunction covers the derivation the whole of stage 0
// rests on.
//
// gc computes the property as -+ OR (-std AND the package is in
// objabi.runtimePkgs). The -std half is not decoration: a module of a user's
// own may hold a package whose import path is internal/abi, and it is not the
// standard library's.
func TestRuntimeRulesIsGcsDisjunction(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want bool
	}{
		{"an ordinary package", Config{Package: "example.com/x"}, false},
		{"a runtime package without -std", Config{Package: "internal/abi"}, false},
		{"a runtime package with -std", Config{Package: "internal/abi", Std: true}, true},
		{"the runtime with -std", Config{Package: "runtime", Std: true}, true},
		{"a standard package that is not one", Config{Package: "strings", Std: true}, false},
		{"the flag on its own", Config{Package: "example.com/x", CompilingRuntime: true}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.RuntimeRules(); got != tt.want {
				t.Errorf("RuntimeRules() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRuntimeGateFiresForTheRightPackages is stage 0's whole claim.
//
// The old gate was the -+ flag, which the go command sends for no package at
// all, so it fired for none of the eight packages the bootstrap closure
// refuses. The new one is derived, and its two halves are checked here: the
// package half refuses runtime by name and nothing else, and the directive
// half refuses a function that carries a rule nanogo does not implement.
//
// The negative cases are the point. Thirteen packages in objabi.runtimePkgs
// compile today, and a gate that took them out would be a regression dressed
// as a repair.
func TestRuntimeGateFiresForTheRightPackages(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	const nosplit = "package main\n\n//go:nosplit\nfunc f(a int) int { return a }\n"
	const barrier = "package main\n\n//go:nowritebarrier\nfunc f(a int) int { return a }\n"
	const plain = "package main\n\nfunc f(a int) int { return a }\n"

	for _, tt := range []struct {
		name string
		src  string
		edit func(*Config)
		want []string // empty means the compile must succeed
	}{
		{
			name: "the runtime is refused with no -+ on the command line",
			src:  plain,
			edit: func(c *Config) { c.Package, c.Std = "runtime", true },
			want: []string{"runtime: nanogo cannot compile the runtime", "specs/034", "specs/035", "specs/047"},
		},
		{
			name: "a runtime package that carries no such directive is not refused",
			src:  plain,
			edit: func(c *Config) { c.Package, c.Std = "internal/goarch", true },
		},
		{
			name: "a package named runtime outside the standard library is not refused",
			src:  plain,
			edit: func(c *Config) { c.Package = "example.com/runtime" },
		},
		{
			// //go:nosplit is honoured and not refused. ssagen emits no
			// stack growth check in such a function, the object carries
			// obj.SymFlagNoSplit, and cmd/link computes the budget over the
			// call graph. The gate refused it while it was recorded and
			// dropped, which was true then and is not true now.
			name: "//go:nosplit in a runtime package is compiled",
			src:  nosplit,
			edit: func(c *Config) { c.Package, c.Std = "internal/abi", true },
		},
		{
			name: "//go:nowritebarrier in a runtime package is refused",
			src:  barrier,
			edit: func(c *Config) { c.Package, c.Std = "internal/abi", true },
			want: []string{"//go:nowritebarrier on function f", "specs/034-write-barriers.md"},
		},
		{
			name: "//go:nosplit outside a runtime package is still only recorded",
			src:  nosplit,
			edit: nil,
		},
		{
			name: "//go:nosplit on a bodyless declaration is not refused",
			// Nothing here is compiled for it, and cmd/asm honours NOSPLIT on
			// the definition the assembly file writes.
			src:  "package main\n\n//go:nosplit\nfunc f(a int) int\n\nfunc g() int { return f(1) }\n",
			edit: func(c *Config) { c.Package, c.Std = "internal/abi", true },
		},
		{
			name: "-+ is still refused, because gc still honours it",
			src:  plain,
			edit: func(c *Config) { c.CompilingRuntime = true },
			want: []string{"nanogo cannot compile the runtime"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSource(t, tt.src, tt.edit)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("Compile: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Compile accepted it")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the message does not carry %q:\n%v", want, err)
				}
			}
		})
	}
}
