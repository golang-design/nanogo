// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/syntax"
)

// parseLines parses src and returns the line of every message the parse
// produced, in the order they were reported.
//
// It stops at the parser, because every rule under test is decided there or
// immediately after: no import has to resolve and no type has to check for a
// misplaced directive to be a misplaced directive.
func parseLines(t *testing.T, src string) []int {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := parseFiles(&Config{Package: "main", Files: []string{path}})
	if err == nil {
		return nil
	}
	var lines []int
	re := regexp.MustCompile(`a\.go:(\d+):\d+: (.*)`)
	for _, line := range strings.Split(err.Error(), "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("a message with no position: %q\nin:\n%s", line, err)
		}
		if m[2] != misplacedDirective {
			t.Fatalf("unexpected message: %q", line)
		}
		n, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			t.Fatal(convErr)
		}
		lines = append(lines, n)
	}
	return lines
}

func sameLines(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestMisplacedDirectiveIsRejected covers specs/016-directives-and-pragmas.md
// rule 1: a directive must stand on its own line immediately before a
// declaration that has a use for it. Anywhere else it is an error, because a
// dropped //go:nosplit is a function that grows its stack where it must not.
//
// Every case here is one gc rejects, and the lines are gc's. Go's own corpus
// pins the whole set in test/directive.go and test/directive2.go.
func TestMisplacedDirectiveIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []int
	}{{
		name: "before the package clause",
		//        1               2 3               4
		src:  "//go:noinline\n\npackage main\n",
		want: []int{1},
	}, {
		name: "before a package-level var",
		src:  "package main\n\n//go:noinline\nvar x int\n",
		want: []int{3},
	}, {
		name: "before a package-level const",
		src:  "package main\n\n//go:noinline\nconst c = 1\n",
		want: []int{3},
	}, {
		name: "before a type",
		src:  "package main\n\n//go:noinline\ntype T int\n",
		want: []int{3},
	}, {
		name: "before an import",
		src:  "package main\n\n//go:noinline\nimport \"os\"\n",
		want: []int{3},
	}, {
		name: "inside a type group",
		src:  "package main\n\ntype (\n\t//go:noinline\n\tT int\n)\n",
		want: []int{4},
	}, {
		name: "before a type group",
		src:  "package main\n\n//go:noinline\ntype (\n\tT int\n)\n",
		want: []int{3},
	}, {
		name: "before a local declaration",
		src:  "package main\n\nfunc f() {\n\t//go:noinline\n\tvar y int\n\t_ = y\n}\n",
		want: []int{4},
	}, {
		name: "before a statement",
		src:  "package main\n\nfunc f() {\n\t//go:noinline\n\tx := 1\n\t_ = x\n}\n",
		want: []int{4},
	}, {
		name: "not on a line of its own",
		src:  "package main\n\nvar x int //go:noinline\n",
		want: []int{3},
	}, {
		name: "after a function body",
		src:  "package main\n\nfunc g() {} //go:noinline\n",
		want: []int{3},
	}, {
		name: "two directives on one declaration are two messages",
		src:  "package main\n\n//go:noinline\n//go:nosplit\nvar x int\n",
		want: []int{3, 4},
	}, {
		// gc rejects any //go: comment that shares a line, recognised verb or
		// not: the blank flag is what the rule is about, and the scanner
		// records it before anything reads the verb.
		name: "an unrecognised verb off a line of its own",
		src:  "package main\n\nvar x int //go:notadirective\n",
		want: []int{3},
	}, {
		name: "go:build after the package clause",
		src:  "package main\n\n//go:build bad\ntype T int\n",
		want: []int{3},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLines(t, tc.src); !sameLines(got, tc.want) {
				t.Errorf("messages on lines %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDirectiveInItsPlaceIsAccepted is the other half of rule 1. A rule that
// rejects everything is not a rule, and a compiler that refuses a legal
// //go:nosplit is worse than one that ignores it.
func TestDirectiveInItsPlaceIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{{
		name: "on a function",
		src:  "package main\n\n//go:nosplit\n//go:noinline\nfunc f() {}\n",
	}, {
		name: "on a method",
		src:  "package main\n\ntype T int\n\n//go:noescape\nfunc (t T) m() {}\n",
	}, {
		name: "go:build on the file",
		src:  "//go:build !ignore\n\npackage main\n",
	}, {
		name: "an unrecognised verb stays a comment",
		// specs/016 rule 2: a new Go release adds directives and decision 10
		// pins nanogo to one, so an unknown verb is not something to reject.
		src: "package main\n\n//go:notadirective\nvar x int\n",
	}, {
		name: "a directive with arguments",
		src:  "package main\n\n//go:linkname x runtime.x\nfunc f() {}\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLines(t, tc.src); got != nil {
				t.Errorf("messages on lines %v, want none", got)
			}
		})
	}
}

// TestPragmaVerbMapsTheTable pins the verb table of
// specs/016-directives-and-pragmas.md, including the three verbs that imply
// another. An implication that is dropped makes the implied rule unenforced
// for every function that relies on the shorthand.
func TestPragmaVerbMapsTheTable(t *testing.T) {
	for _, tc := range []struct {
		verb string
		want pragmaFlag
	}{
		{"go:build", goBuild},
		{"go:noescape", noescape},
		{"go:norace", norace},
		{"go:nosplit", nosplit | nocheckptr},
		{"go:noinline", noinline},
		{"go:nocheckptr", nocheckptr},
		{"go:systemstack", systemstack},
		{"go:nowritebarrier", nowritebarrier},
		{"go:nowritebarrierrec", nowritebarrierrec | nowritebarrier},
		{"go:yeswritebarrierrec", yeswritebarrierrec},
		{"go:cgo_unsafe_args", cgoUnsafeArgs | nocheckptr},
		{"go:uintptrkeepalive", uintptrKeepAlive},
		{"go:uintptrescapes", uintptrEscapes | uintptrKeepAlive},
		{"go:registerparams", registerParams},
		{"go:nointerface", 0}, // behind an experiment nanogo does not enable
		{"go:wasmimport", 0},  // no wasm target
		{"go:notaverb", 0},
	} {
		if got := pragmaVerb(tc.verb); got != tc.want {
			t.Errorf("pragmaVerb(%q) = %#b, want %#b", tc.verb, got, tc.want)
		}
	}
	// goBuild is the one flag a function must not accept, and the one every
	// other declaration must not accept either.
	if funcPragmas&goBuild != 0 {
		t.Error("funcPragmas accepts go:build")
	}
}

// TestDirectivesReachTheDeclaration checks that the record the handler builds
// is attached where a consumer will look for it. specs/016 puts the consumers
// below the checker, so ir.Func.Pragma is the endpoint and the parser puts it
// there from the declaration.
func TestDirectivesReachTheDeclaration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	src := "package main\n\n//go:nosplit\n//go:noinline\nfunc f() {}\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	files, _, err := parseFiles(&Config{Package: "main", Files: []string{path}})
	if err != nil {
		t.Fatalf("parseFiles: %v", err)
	}
	fn, ok := files[0].DeclList[0].(*syntax.FuncDecl)
	if !ok {
		t.Fatalf("the first declaration is a %T, want a *syntax.FuncDecl", files[0].DeclList[0])
	}
	p := asPragma(fn.Pragma)
	if p == nil {
		t.Fatal("the function declaration carries no directive")
	}
	if want := nosplit | nocheckptr | noinline; p.flag != want {
		t.Errorf("the declaration carries %#b, want %#b", p.flag, want)
	}
	if len(p.list) != 2 {
		t.Fatalf("the declaration records %d directives, want 2", len(p.list))
	}
	if p.list[0].pos >= p.list[1].pos {
		t.Error("the directives are not recorded in source order")
	}
}
