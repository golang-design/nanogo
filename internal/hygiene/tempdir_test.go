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
// The check is a static one so it runs in a second on every push. It sees
// only what this package's own source creates: a directory a subprocess makes
// is invisible to it, which is why ssagen's second directory, the one
// `go build -work` keeps, is not covered here. The gate that proves the
// property rather than approximating it is `lateregate tempdir`, which runs
// the suite against an empty TMPDIR and reads what survives.

// TestTempDirsAreRemoved reports an os.MkdirTemp in a test file whose
// directory nothing in the package removes.
//
// Matching is by scope, not by spelling. An earlier version of this check
// credited a binding whenever its name appeared in an os.RemoveAll anywhere
// in the package, and that passed ssagen on a coincidence: the directory is
// bound to a local named dir, and TestMain removes a range variable that also
// happens to be named dir. Deleting the line that connects the two left the
// leak in place and the check green, which is the failure a gate exists to
// prevent. So:
//
//   - A package-level variable is removed when an os.RemoveAll in the package
//     reaches that variable.
//   - A struct field is removed when an os.RemoveAll reaches that field.
//   - A local is removed when the function that declared it removes it, or
//     when it escapes into a package variable or a field that is removed.
//
// "Reaches" covers both a direct argument and an element of a composite
// literal inside a function that calls os.RemoveAll, which is how a TestMain
// ranging over the directories it clears is written.
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
		files, err := parseTestFiles(fset, dir)
		if err != nil {
			t.Errorf("parsing %s: %v", dir, err)
			continue
		}
		for pkgName, files := range files {
			p := newPkg(pkgName, files)
			for _, s := range p.tempDirSites() {
				checked++
				if p.removes(s) {
					continue
				}
				pos := fset.Position(s.pos)
				rel, rerr := filepath.Rel(root, pos.Filename)
				if rerr != nil {
					rel = pos.Filename
				}
				t.Errorf("%s:%d: os.MkdirTemp binds %s and nothing in package %s removes it.\n"+
					"Use t.TempDir if one test owns the directory, or remove it from a TestMain.",
					filepath.ToSlash(rel), pos.Line, s.describe(), pkgName)
			}
		}
	}
	// The sites this check was written for are all in test files. A run that
	// found no os.MkdirTemp at all means the walk stopped matching, not that
	// the repository stopped making temporary directories.
	if checked == 0 {
		t.Errorf("no os.MkdirTemp call found in any test file; the check no longer matches anything")
	}
}

// scope says how a binding is reached from elsewhere, which decides where a
// removal of it has to be looked for.
type scope int

const (
	local  scope = iota // a function's own variable
	global              // a package-level variable
	field               // a struct field, bound as x.name
)

// site is one os.MkdirTemp call and the name its result is bound to.
type site struct {
	pos   token.Pos
	name  string // "" when the result is discarded
	scope scope
	fn    *ast.FuncDecl // the function holding the call, for a local
}

func (s *site) describe() string {
	if s.name == "" {
		return "nothing"
	}
	switch s.scope {
	case field:
		return "the field " + s.name
	case global:
		return "the package variable " + s.name
	}
	return s.name
}

// pkg is one package's test files with the facts the rules need.
type pkg struct {
	name    string
	files   []*ast.File
	globals map[string]bool // package-level variable names
	removed map[string]bool // globals and fields an os.RemoveAll reaches
}

func newPkg(name string, files []*ast.File) *pkg {
	p := &pkg{name: name, files: files, globals: map[string]bool{}}
	for _, f := range files {
		for _, d := range f.Decls {
			gen, ok := d.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, id := range vs.Names {
						p.globals[id.Name] = true
					}
				}
			}
		}
	}
	p.removed = p.removedNames()
	return p
}

// removes reports whether the package removes what a site bound.
func (p *pkg) removes(s *site) bool {
	if s.name == "" {
		// os.MkdirTemp whose result is discarded makes a directory nobody
		// holds, which cannot be removed by anyone.
		return false
	}
	switch s.scope {
	case global, field:
		return p.removed[s.name]
	}
	// A local. Removed by its own function, or escaped into something that is
	// removed: `c.dir = dir` is what makes ssagen's TestMain able to reach it.
	if s.fn != nil && reaches(s.fn.Body, s.name) {
		return true
	}
	for _, escaped := range p.escapes(s) {
		if p.removed[escaped] {
			return true
		}
	}
	return false
}

// escapes returns the package variables and fields a local is assigned to
// inside the function that declared it.
func (p *pkg) escapes(s *site) []string {
	if s.fn == nil {
		return nil
	}
	var out []string
	ast.Inspect(s.fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			id, ok := rhs.(*ast.Ident)
			if !ok || id.Name != s.name || i >= len(as.Lhs) {
				continue
			}
			switch lhs := as.Lhs[i].(type) {
			case *ast.SelectorExpr:
				out = append(out, lhs.Sel.Name)
			case *ast.Ident:
				if p.globals[lhs.Name] {
					out = append(out, lhs.Name)
				}
			}
		}
		return true
	})
	return out
}

// tempDirSites returns every os.MkdirTemp call in the package with the name
// its directory is bound to, in source order.
func (p *pkg) tempDirSites() []*site {
	var sites []*site
	for _, f := range p.files {
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				names, kinds, rhs := assignment(n, p.globals, fn != nil)
				for i, expr := range rhs {
					call, ok := expr.(*ast.CallExpr)
					if !ok || !isCall(call, "os", "MkdirTemp") {
						continue
					}
					s := &site{pos: call.Pos(), fn: fn}
					if i < len(names) {
						s.name, s.scope = names[i], kinds[i]
					}
					sites = append(sites, s)
				}
				return true
			})
		}
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].pos < sites[j].pos })
	return sites
}

// assignment returns the names a node binds, their scopes, and the
// expressions it binds them to, for both `a, b := f()` and `var a, b = f()`.
// inFunc distinguishes a function's own variable from a package-level one.
func assignment(n ast.Node, globals map[string]bool, inFunc bool) ([]string, []scope, []ast.Expr) {
	var lhs []ast.Expr
	var rhs []ast.Expr
	switch n := n.(type) {
	case *ast.AssignStmt:
		lhs, rhs = n.Lhs, n.Rhs
	case *ast.ValueSpec:
		for _, id := range n.Names {
			lhs = append(lhs, id)
		}
		rhs = n.Values
	default:
		return nil, nil, nil
	}
	names := make([]string, len(lhs))
	kinds := make([]scope, len(lhs))
	for i, e := range lhs {
		switch e := e.(type) {
		case *ast.SelectorExpr:
			names[i], kinds[i] = e.Sel.Name, field
		case *ast.Ident:
			names[i] = e.Name
			switch {
			case !inFunc || globals[e.Name]:
				kinds[i] = global
			default:
				kinds[i] = local
			}
		}
	}
	return names, kinds, rhs
}

// removedNames collects the package variables and fields an os.RemoveAll
// reaches: those in its arguments, and those in a composite literal inside a
// function that calls one, which is a TestMain ranging over what it clears.
//
// A local is never collected. Two functions may each have a variable called
// dir without one being the other, and crediting a binding for a name that
// happens to match somewhere else is how this check first passed a live leak.
func (p *pkg) removedNames() map[string]bool {
	removed := map[string]bool{}
	take := func(e ast.Expr) {
		ast.Inspect(e, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				removed[n.Sel.Name] = true
				return false
			case *ast.Ident:
				if p.globals[n.Name] {
					removed[n.Name] = true
				}
			}
			return true
		})
	}
	for _, f := range p.files {
		ast.Inspect(f, func(n ast.Node) bool {
			body := funcBody(n)
			if body == nil || !reachesAny(body) {
				return true
			}
			ast.Inspect(body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if isCall(n, "os", "RemoveAll") {
						for _, arg := range n.Args {
							take(arg)
						}
					}
				case *ast.CompositeLit:
					for _, elt := range n.Elts {
						take(elt)
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

// reaches reports whether an os.RemoveAll under body mentions name.
func reaches(body ast.Node, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isCall(call, "os", "RemoveAll") {
			return !found
		}
		for _, arg := range call.Args {
			ast.Inspect(arg, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == name {
					found = true
				}
				return !found
			})
		}
		return !found
	})
	return found
}

// reachesAny reports whether body calls os.RemoveAll at all.
func reachesAny(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isCall(call, "os", "RemoveAll") {
			found = true
		}
		return !found
	})
	return found
}

// isCall reports whether the call is pkg.name(...).
func isCall(call *ast.CallExpr, pkgName, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkgName
}

// parseTestFiles parses the test files in one directory, grouped by package
// name: a directory holds both foo and foo_test, and they do not share a
// TestMain.
func parseTestFiles(fset *token.FileSet, dir string) (map[string][]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]*ast.File{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, err
		}
		out[f.Name.Name] = append(out[f.Name.Name], f)
	}
	return out, nil
}

// testPackageDirs returns every directory under root holding a test file.
func testPackageDirs(root string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
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

// skipDir names the directories no check in this package walks into.
//
// .claude holds worktrees, which are second checkouts of this repository: a
// violation found there is the same violation reported once per agent that
// happened to have one open. testdata holds the Go project's own corpus,
// which is not this repository's code and does not answer to its rules.
func skipDir(name string) bool {
	switch name {
	case ".git", ".claude", "spikes", "testdata", "upstream":
		return true
	}
	return strings.HasPrefix(name, "_")
}
