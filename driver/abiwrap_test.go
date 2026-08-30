// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestABIWrapperGate is stage 1's gate, per specs/047-abi-wrappers.md.
//
// A package with assembly used to be refused whole. It is now refused only
// where ssagen.GenABIWrappers would make a wrapper, so the two packages the
// spec measured at zero wrappers pass it. Every row is one clause of that
// decision.
//
// The success rows matter as much as the refusals. A gate that refused
// everything would pass every refusal test and lift nothing.
func TestABIWrapperGate(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	for _, tt := range []struct {
		name    string
		src     string
		symabis string
		want    []string // empty means the compile must succeed
	}{
		{
			// internal/cpu.getisar0 and the forty-nine of
			// internal/runtime/atomic. gc sets fn.ABI to ABI0 from the def
			// and then puts ABIInternal in ABIRefs unconditionally, so the
			// ABIInternal wrapper is always owed, and gc/compile.go writes
			// the argument map for exactly this case. Stage 2 builds both,
			// and TestABI0DefinitionEmitsAWrapperAndItsMaps reads what comes
			// out.
			name:    "an ABI0 definition owes a wrapper and an argument map",
			src:     "package main\n\nfunc f(a int) int\n\nfunc g() int { return f(1) }\n",
			symabis: "def main.f ABI0\n",
		},
		{
			// internal/chacha8rand.block. The assembly defines the symbol
			// under the ABI a Go call already names, so need is empty and
			// nothing is owed. This is the row that lifts the refusal.
			name:    "an ABIInternal definition owes nothing",
			src:     "package main\n\nfunc f(a int) int\n\nfunc g() int { return f(1) }\n",
			symabis: "def main.f ABIInternal\n",
		},
		{
			// internal/abi.FuncPCTestFn. Its only Go declaration is in
			// export_test.go, which is not in the ordinary build, so the def
			// matches nothing and the compiler writes no wrapper and no
			// argument map for it. go tool nm over the built archive shows
			// internal/abi.FuncPCTestFn.args_stackmap undefined, and it never
			// resolves because the symbol is unreachable.
			name:    "a def that matches no declaration is nothing",
			src:     "package main\n\nfunc g() int { return 1 }\n",
			symabis: "def main.FuncPCTestFn ABI0\nref main.FuncPCTestFnAddr ABI0\n",
		},
		{
			name:    "an ABI0 reference to a Go function owes an ABI0 wrapper",
			src:     "package main\n\nfunc f(a int) int { return a }\n",
			symabis: "ref main.f ABI0\n",
			want: []string{"an assembly call to function f",
				"specs/047-abi-wrappers.md stage 3"},
		},
		{
			// A ref under the ABI the function is already defined with adds
			// nothing to need. internal/bytealg's file holds several.
			name:    "an ABIInternal reference to a Go function owes nothing",
			src:     "package main\n\nfunc f(a int) int { return a }\n",
			symabis: "ref main.f ABIInternal\n",
		},
		{
			// A ref may name a data symbol, as internal/chacha8rand's
			// chachaConst and internal/runtime/maps' aeskeysched do. It
			// reaches the table and is never matched against a function.
			name:    "a reference to something that is not a function is nothing",
			src:     "package main\n\nvar V = 1\n\nfunc g() int { return V }\n",
			symabis: "ref main.V ABI0\n",
		},
		{
			name:    "a def for a declaration that also has a Go body",
			src:     "package main\n\nfunc f(a int) int { return a }\n",
			symabis: "def main.f ABI0\n",
			want:    []string{"a.go:3:6", "f defined in both Go and assembly"},
		},
		{
			// internal/bytealg.abigen_runtime_cmpstring. The def line names
			// the linkname and not the declaration, so the match happens
			// only through the directive: fn.ABI is ABIInternal, the
			// linkname over a symbol this package's assembly defines adds
			// ABISetCallable, and need is left holding ABI0.
			name:    "a //go:linkname is matched against the def line",
			src:     "package main\n\n//go:linkname f runtime.cmpstring\nfunc f(a, b string) int\n",
			symabis: "def runtime.cmpstring ABIInternal\n",
			want: []string{"an assembly call to function f",
				"specs/047-abi-wrappers.md stage 3"},
		},
		{
			// The one-argument form. noder fills the target in with the
			// default name, so sym.Linkname is not empty and the symbol is
			// callable under both conventions. internal/cpu.sysctlEnabled is
			// this shape and it is why internal/cpu needs an ABI0 wrapper
			// that has nothing to do with its four ABI0 definitions.
			name:    "a one-argument //go:linkname over a Go body owes an ABI0 wrapper",
			src:     "package main\n\n//go:linkname f\nfunc f(a int) int { return a }\n",
			symabis: "",
			want: []string{"an assembly call to function f",
				"specs/047-abi-wrappers.md stage 3"},
		},
		{
			// internal/runtime/atomic.storePointer and
			// internal/runtime/maps.typeString. The package writes the
			// directive over a declaration it does not define at all, so
			// gc's conjunction fails on its second term and nothing is owed.
			// This row is what separates "the directive was written" from
			// "the package defines the symbol", and a model that read only
			// the first would refuse both packages for a wrapper neither
			// owes.
			name:    "a //go:linkname over a declaration this package does not define owes nothing",
			src:     "package main\n\n//go:linkname f\nfunc f(a int) int\n\nfunc g() int { return f(1) }\n",
			symabis: "",
		},
		{
			// The rename half of the directive is not built. A bodyless
			// declaration passes, because nanogo emits no definition for one
			// and the name is only ever matched.
			name:    "a //go:linkname that renames a definition nanogo emits",
			src:     "package main\n\n//go:linkname f other.f\nfunc f(a int) int { return a }\n",
			symabis: "",
			want: []string{"//go:linkname f other.f", "it renames the function nanogo defines",
				"specs/047-abi-wrappers.md"},
		},
		{
			// internal/runtime/maps writes //go:linkname zeroVal
			// runtime.zeroVal over a package-level variable it declares. The
			// storage is nanogo's to emit and it would be emitted under the
			// name the source wrote.
			name:    "a //go:linkname that renames a variable nanogo defines",
			src:     "package main\n\n//go:linkname V other.V\nvar V int\n\nfunc g() int { return V }\n",
			symabis: "",
			want: []string{"//go:linkname V other.V", "it renames the package-level variable nanogo defines",
				"specs/047-abi-wrappers.md"},
		},
		{
			name:    "a //go:linkname with no arguments",
			src:     "package main\n\n//go:linkname\nfunc f(a int) int { return a }\n",
			symabis: "",
			want:    []string{"usage: //go:linkname localname [linkname]"},
		},
		{
			name:    "//go:cgo_export_static is refused",
			src:     "package main\n\n//go:cgo_export_static f\nfunc f(a int) int { return a }\n",
			symabis: "",
			want:    []string{"//go:cgo_export_static", "specs/000-decisions.md decision 8"},
		},
		{
			name:    "//go:cgo_unsafe_args is refused",
			src:     "package main\n\n//go:cgo_unsafe_args\nfunc f(a int) int { return a }\n",
			symabis: "",
			want:    []string{"//go:cgo_unsafe_args on function f", "ABI0"},
		},
		{
			name:    "a symabis file nanogo cannot decode",
			src:     "package main\n\nfunc g() int { return 1 }\n",
			symabis: "def main.f ABI2\n",
			want:    []string{"unknown abi", "ABI2"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			edit := func(c *Config) {
				c.AsmHdr = filepath.Join(dir, "go_asm.h")
				path := filepath.Join(dir, "symabis")
				if err := os.WriteFile(path, []byte(tt.symabis), 0o600); err != nil {
					t.Fatal(err)
				}
				c.SymABIs = path
			}
			_, err := compileSource(t, tt.src, edit)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("Compile: %v", err)
				}
				// A package that passes the gate still owes the header, and
				// the second assembler run reads it.
				if _, err := os.Stat(filepath.Join(dir, "go_asm.h")); err != nil {
					t.Errorf("the header was not written: %v", err)
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

// TestAssemblyWithNoSymABIsIsNotRefused covers the -asmhdr-only shape.
//
// The go command sends -symabis and -asmhdr together, and a package whose
// assembly defines no text symbol at all still gets both. internal/runtime/sys
// has empty.s in its file list for exactly that reason. A gate that keyed off
// either flag refused such a package for a wrapper it does not owe.
func TestAssemblyWithNoSymABIsIsNotRefused(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	hdr := filepath.Join(dir, "go_asm.h")
	_, err := compileSource(t, "package main\n\nfunc g() int { return 1 }\n",
		func(c *Config) { c.AsmHdr = hdr })
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := os.Stat(hdr); err != nil {
		t.Errorf("the header was not written: %v", err)
	}
}
