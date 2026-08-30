// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package hygiene

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A test that makes a directory under TMPDIR and does not remove it leaks it
// until the machine is rebooted, and macOS does not clear the per-user folder
// on a reboot either.
//
// This is not hypothetical. Three sites in this repository leaked for months:
// internal/gotest left 258 directories of 790MB each, 160GB in all, because
// its corpus sweep builds the standard library into a cache it never deleted.
// internal/audit left 7.1GB the same way, and ssagen left 432 directories.
// Two of the three carried a comment saying the directory was "removed by the
// process that made it", which was never true. The disk filled to 8.2GB free
// on a 926GB volume.
//
// t.TempDir is the right answer wherever it fits, and it fits everywhere
// except a directory shared by every test in a package: the testing package
// removes a t.TempDir when the test that asked for it returns, so the second
// test finds the tools gone. Those sites need a TestMain, and a TestMain is
// easy to forget because nothing fails when it is missing.
//
// The check is a static one so it runs in a second on every push. The gate
// that proves the property rather than approximating it is `lateregate
// tempdir`, which runs the suite against an empty TMPDIR and reads what
// survives; that one needs the corpus and takes minutes.

// TestTempDirsAreRemoved reports an os.MkdirTemp in a test file whose
// directory nothing in the package removes.
//
// The rule: every os.MkdirTemp binding must reach an os.RemoveAll. A binding
// reaches one when it appears in the arguments of an os.RemoveAll call, or in
// a composite literal inside a function that calls os.RemoveAll, which is how
// a TestMain that ranges over the directories it has to clear is written.
func TestTempDirsAreRemoved(t *testing.T) {
	root := repoRoot(t)

	dirs, err := testPackageDirs(root)
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	// A check that read no test files proves nothing.
	if len(dirs) == 0 {
		t.Fatalf("no test files found under %s; the check would pass vacuously", root)
	}

	checked := 0
	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, isTestFile, 0)
		if err != nil {
			t.Errorf("parsing %s: %v", dir, err)
			continue
		}
		for _, pkg := range pkgs {
			removed := removedNames(pkg)
			for _, site := range tempDirSites(pkg) {
				checked++
				if site.name != "" && removed[site.name] {
					continue
				}
				rel, rerr := filepath.Rel(root, fset.Position(site.pos).Filename)
				if rerr != nil {
					rel = fset.Position(site.pos).Filename
				}
				t.Errorf("%s:%d: os.MkdirTemp binds %s and nothing in package %s removes it.\n"+
					"Use t.TempDir if one test owns the directory, or remove it from a TestMain.",
					filepath.ToSlash(rel), fset.Position(site.pos).Line, site.describe(), pkg.Name)
			}
		}
	}
	// The three sites this test was written for are all in test files. A run
	// that found no os.MkdirTemp at all means the walk stopped matching, not
	// that the repository stopped making temporary directories.
	if checked == 0 {
		t.Errorf("no os.MkdirTemp call found in any test file; the check no longer matches anything")
	}
}

// site is one os.MkdirTemp call and the name its result is bound to.
type site struct {
	pos  token.Pos
	name string // the identifier, or the field name of a selector; "" if unassigned
}

func (s site) describe() string {
	if s.name == "" {
		return "nothing"
	}
	return s.name
}

// tempDirSites returns every os.MkdirTemp call in the package with the name
// its directory is bound to.
func tempDirSites(pkg *ast.Package) []*site {
	var sites []*site
	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			names, rhs := assignment(n)
			if rhs == nil {
				return true
			}
			for i, expr := range rhs {
				call, ok := expr.(*ast.CallExpr)
				if !ok || !isCall(call, "os", "MkdirTemp") {
					continue
				}
				s := &site{pos: call.Pos()}
				if i < len(names) {
					s.name = names[i]
				}
				sites = append(sites, s)
			}
			return true
		})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].pos < sites[j].pos })
	return sites
}

// assignment returns the names a node binds and the expressions it binds them
// to, for both `a, b := f()` and `var a, b = f()`.
func assignment(n ast.Node) ([]string, []ast.Expr) {
	switch n := n.(type) {
	case *ast.AssignStmt:
		names := make([]string, len(n.Lhs))
		for i, lhs := range n.Lhs {
			names[i] = boundName(lhs)
		}
		return names, n.Rhs
	case *ast.ValueSpec:
		names := make([]string, len(n.Names))
		for i, id := range n.Names {
			names[i] = id.Name
		}
		return names, n.Values
	}
	return nil, nil
}

// boundName is the name an assignment target contributes. A selector binds
// under its field name, because that is the name a TestMain clearing b.dir
// mentions and the receiver it hangs off is not the same identifier there.
func boundName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// removedNames collects the names that reach an os.RemoveAll: those in the
// arguments of one, and those in a composite literal inside a function that
// calls one. The second case is a TestMain ranging over a slice of the
// directories it clears.
func removedNames(pkg *ast.Package) map[string]bool {
	removed := map[string]bool{}
	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			body := funcBody(n)
			if body == nil {
				return true
			}
			if !callsRemoveAll(body) {
				return true
			}
			ast.Inspect(body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if isCall(n, "os", "RemoveAll") {
						for _, arg := range n.Args {
							collectNames(arg, removed)
						}
					}
				case *ast.CompositeLit:
					for _, elt := range n.Elts {
						collectNames(elt, removed)
					}
				}
				return true
			})
			return true
		})
	}
	return removed
}

// funcBody returns the body of a function declaration or literal.
func funcBody(n ast.Node) *ast.BlockStmt {
	switch n := n.(type) {
	case *ast.FuncDecl:
		return n.Body
	case *ast.FuncLit:
		return n.Body
	}
	return nil
}

func callsRemoveAll(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isCall(call, "os", "RemoveAll") {
			found = true
		}
		return !found
	})
	return found
}

// collectNames records every identifier and selector field an expression
// mentions, so os.RemoveAll(filepath.Join(dir, "x")) credits dir.
func collectNames(e ast.Expr, into map[string]bool) {
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			into[n.Sel.Name] = true
		case *ast.Ident:
			into[n.Name] = true
		}
		return true
	})
}

// isCall reports whether the call is pkg.name(...).
func isCall(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func isTestFile(fi os.FileInfo) bool { return strings.HasSuffix(fi.Name(), "_test.go") }

// testPackageDirs returns every directory under root holding a test file.
func testPackageDirs(root string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			// testdata holds the Go project's own corpus, which is not this
			// repository's code and does not answer to its rules. .claude
			// holds worktrees, which are second checkouts of this one: a
			// violation there is the same violation reported again.
			if name == ".git" || name == ".claude" || name == "spikes" ||
				name == "testdata" || name == "upstream" ||
				strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			seen[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, nil
}
