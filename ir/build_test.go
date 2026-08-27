// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"go/build"
	"go/constant"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"

	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// buildTypecheck parses and type-checks one source file.
//
// The corpus of this package is Go source text, not hand-built trees. A test
// that builds its own input proves that the builder agrees with the test, and
// the question is whether the builder agrees with the type checker.
func buildTypecheck(t *testing.T, src string) (*types2.Package, []*syntax.File, *types2.Info) {
	t.Helper()
	fset := syntax.NewFileSet()
	file, err := syntax.Parse(fset.AddFile("x.go", len(src)), []byte(src), nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	files := []*syntax.File{file}
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{
		Fset:     fset,
		Importer: buildUnsafeImporter{},
		Sizes:    types2.SizesFor("gc", "amd64"),
	}
	pkg, err := conf.Check("p", files, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	return pkg, files, info
}

// buildUnsafeImporter resolves package unsafe and nothing else. A snippet in
// this file imports nothing else, and an importer that reached the file system
// would make a unit test depend on a GOROOT.
type buildUnsafeImporter struct{}

func (buildUnsafeImporter) Import(path string) (*types2.Package, error) {
	if path == "unsafe" {
		return types2.Unsafe, nil
	}
	return nil, errNoImporter{path}
}

type errNoImporter struct{ path string }

func (e errNoImporter) Error() string { return "no importer for " + e.path }

// corpusImporter type-checks a package from source, and the packages it
// imports, so that the corpus test can reach a real standard library.
//
// The test-only source importer of types2 is not importable from here, so this
// is a small one of its own: go/build answers which files are in a package,
// nanogo's parser reads them and nanogo's checker checks them.
type corpusImporter struct {
	fset *syntax.FileSet
	done map[string]*corpusResult
}

type corpusResult struct {
	pkg   *types2.Package
	files []*syntax.File
	info  *types2.Info
	err   error
}

func newCorpusImporter() *corpusImporter {
	return &corpusImporter{fset: syntax.NewFileSet(), done: make(map[string]*corpusResult)}
}

func (imp *corpusImporter) Import(path string) (*types2.Package, error) {
	r := imp.check(path)
	return r.pkg, r.err
}

func (imp *corpusImporter) check(path string) *corpusResult {
	if have, ok := imp.done[path]; ok {
		if have == nil {
			// A cycle. The standard library has none, so this is a broken
			// build tag rather than a program.
			return &corpusResult{err: fmt.Errorf("import cycle at %s", path)}
		}
		return have
	}
	if path == "unsafe" {
		r := &corpusResult{pkg: types2.Unsafe}
		imp.done[path] = r
		return r
	}
	imp.done[path] = nil // in progress

	r := &corpusResult{}
	imp.done[path] = r
	bp, err := build.Import(path, "", 0)
	if err != nil {
		// The standard library vendors a few packages under
		// GOROOT/src/vendor and GOROOT/src/cmd/vendor, and an import of one
		// of them is written without the prefix. Resolving it is what lets
		// crypto/tls and the other users of them into the corpus.
		for _, prefix := range []string{"vendor/", "cmd/vendor/"} {
			if bp2, err2 := build.Import(prefix+path, "", 0); err2 == nil {
				bp, err = bp2, nil
				break
			}
		}
	}
	if err != nil {
		r.err = err
		return r
	}
	if len(bp.CgoFiles) > 0 {
		r.err = fmt.Errorf("%s needs cgo", path)
		return r
	}
	if len(bp.GoFiles) == 0 {
		r.err = fmt.Errorf("%s has no Go files", path)
		return r
	}
	for _, name := range bp.GoFiles {
		full := filepath.Join(bp.Dir, name)
		f, err := syntax.ParseFile(imp.fset, full, nil, nil, 0)
		if err != nil || f == nil {
			r.err = fmt.Errorf("parse %s: %v", full, err)
			return r
		}
		r.files = append(r.files, f)
	}
	r.info = &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{
		Fset:     imp.fset,
		Importer: imp,
		Sizes:    types2.SizesFor("gc", "amd64"),
	}
	r.pkg, r.err = conf.Check(path, r.files, r.info)
	return r
}

// TestBuildCorpus builds the IR for every package of the standard library that
// type-checks, and asserts that nothing panics and that no node is missing.
//
// The bar is deliberately low and wide. A table test says the builder is right
// about what it was asked; this says the builder does not fall over on Go that
// nobody wrote for it.
func TestBuildCorpus(t *testing.T) {
	required := os.Getenv("NANOGO_REQUIRE_CORPUS") == "1"
	src := filepath.Join(runtime.GOROOT(), "src")
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		if required {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s is not there", src)
		}
		t.Skipf("no corpus at %s", src)
	}

	var paths []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != src && (name == "testdata" || name == "vendor" ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(paths)
	if !required {
		// The race build runs this too, so an unattended run takes a sample of
		// the library and leaves the tool chain out: cmd/compile alone pulls
		// in a third of the corpus through its imports. CI sets
		// NANOGO_REQUIRE_CORPUS and gets all of it.
		var library []string
		for _, p := range paths {
			if !strings.HasPrefix(p, "cmd/") && p != "cmd" {
				library = append(library, p)
			}
		}
		const sample = 60
		if len(library) > sample {
			step := len(library) / sample
			var taken []string
			for i := 0; i < len(library); i += step {
				taken = append(taken, library[i])
			}
			library = taken
		}
		paths = library
	}

	imp := newCorpusImporter()
	built, skipped, nodes, partial, funcs := 0, 0, 0, 0, 0
	var reasons []string
	kinds := map[string]int{}
	for _, path := range paths {
		if path == "unsafe" {
			// The package has no source of its own to build.
			continue
		}
		r := imp.check(path)
		if r.err != nil || r.pkg == nil {
			skipped++
			kinds[buildSkipKind(r.err)]++
			if len(reasons) < 6 {
				reasons = append(reasons, fmt.Sprintf("%s: %.120v", path, r.err))
			}
			continue
		}
		n, nfunc, incomplete := buildOnePackage(t, path, r)
		if n < 0 {
			continue
		}
		nodes += n
		funcs += nfunc
		if incomplete {
			partial++
		}
		built++
	}
	// built counts a package whose every function produced a tree. partial
	// counts the ones where Build also reported something it could not build,
	// which is a generic declaration and nothing else so far, so the two
	// numbers together say how much of the corpus is really covered.
	t.Logf("built %d packages of %d, %d functions, %d nodes, %d skipped, %d partial",
		built, len(paths), funcs, nodes, skipped, partial)
	for _, k := range []string{"cgo", "no Go files", "outside GOROOT", "type error", "other"} {
		if kinds[k] > 0 {
			t.Logf("skipped %d for %s", kinds[k], k)
		}
	}
	for _, r := range reasons {
		t.Logf("skipped %s", r)
	}
	if built == 0 {
		t.Fatal("the corpus built no package")
	}
	if required && built < 100 {
		t.Fatalf("only %d packages built; the corpus collapsed", built)
	}
}

// buildSkipKind classifies why a package of the corpus was not built, so that
// the count of what was covered is reported next to what it was not.
func buildSkipKind(err error) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	switch {
	case strings.Contains(msg, "needs cgo"):
		return "cgo"
	case strings.Contains(msg, "no Go files"), strings.Contains(msg, "no buildable Go source"):
		return "no Go files"
	case strings.Contains(msg, "no required module provides"), strings.Contains(msg, "cannot find module"):
		return "outside GOROOT"
	case strings.Contains(msg, "could not import"), strings.Contains(msg, ".go:"):
		return "type error"
	}
	return "other"
}

// buildOnePackage builds one package and checks the tree it produced.
//
// It returns the number of nodes and functions, and whether Build also
// reported a declaration it could not build. A count of packages that built
// is worth little without the count of packages that built everything.
func buildOnePackage(t *testing.T, path string, r *corpusResult) (n, nfunc int, incomplete bool) {
	t.Helper()
	defer func() {
		if e := recover(); e != nil {
			t.Errorf("panic building %s: %v\n%s", path, e, debug.Stack())
			n = -1
		}
	}()
	pkg, err := Build(r.pkg, r.files, r.info)
	if pkg == nil {
		t.Errorf("%s built no package", path)
		return -1, 0, true
	}
	incomplete = err != nil
	nfunc = len(pkg.Funcs) + len(pkg.Inits)
	for _, fn := range pkg.Funcs {
		n += buildCheckFunc(t, path, fn)
	}
	for _, fn := range pkg.Inits {
		n += buildCheckFunc(t, path, fn)
	}
	for _, g := range pkg.Globals {
		if g == nil || g.Type == nil || g.Class != ClassGlobal {
			t.Errorf("%s: a global is %v", path, g)
		}
	}
	return n, nfunc, incomplete
}

// buildCheckFunc is the invariant check over one function.
//
// Every node exists, has an operation, has a type, and every OBinary in a
// statement list is an assignment while every OBinary in an operand position
// is not. The last one is what makes the encoding of an assignment a rule
// rather than a comment.
func buildCheckFunc(t *testing.T, path string, fn *Func) int {
	t.Helper()
	count := 0
	// A node is in one of three positions: a statement list, an operand, or
	// an element of a composite literal, where a key/value pair is written as
	// an assignment.
	const (
		asStmt = iota
		asOperand
		asElem
	)
	count = 0
	var check func(n *Node, pos int)
	check = func(n *Node, pos int) {
		stmt := pos == asStmt
		if n == nil {
			t.Errorf("%s: %s has a nil node", path, fn.Sym)
			return
		}
		count++
		if n.Op == OpInvalid {
			t.Errorf("%s: %s has a node with no operation", path, fn.Sym)
		}
		if n.Type == nil {
			t.Errorf("%s: %s has an %s with no type", path, fn.Sym, n.Op)
		}
		if n.Op == OBinary && n.Op1 == 0 {
			t.Errorf("%s: %s has an OBinary with no operator", path, fn.Sym)
		}
		if pos == asOperand && n.Op == OAssign {
			t.Errorf("%s: %s has an assignment in an operand position", path, fn.Sym)
		}
		if stmt && n.Op == OBinary {
			t.Errorf("%s: %s has an OBinary %s in a statement list", path, fn.Sym, n.Op1)
		}
		if n.Op == OLocal || n.Op == OGlobal {
			if n.Obj == nil {
				t.Errorf("%s: %s has an unresolved %s", path, fn.Sym, n.Op)
			}
		}
		list := func(l []Stmt, p int) {
			for _, s := range l {
				check(s, p)
			}
		}
		if n.X != nil {
			check(n.X, asOperand)
		}
		if n.Y != nil {
			check(n.Y, asOperand)
		}
		args := asOperand
		if n.Op == OCompositeLit {
			args = asElem
		}
		if n.Op == OSlice {
			// A bound the source left out is nil, and the three places are
			// the contract: Args[2] is the maximum of a three-index slice.
			if len(n.Args) != 3 {
				t.Errorf("%s: %s has a slice with %d bounds", path, fn.Sym, len(n.Args))
			}
			for _, a := range n.Args {
				if a != nil {
					check(a, asOperand)
				}
			}
		} else {
			list(n.Args, args)
		}
		list(n.Init, asStmt)
		body := asStmt
		if !isStmtList(n) {
			body = asOperand
		}
		list(n.Body, body)
		list(n.Else, body)
		list(n.Post, asStmt)
	}
	for _, s := range fn.Body {
		check(s, asStmt)
	}
	for _, o := range fn.Locals {
		if o == nil || o.Type == nil {
			t.Errorf("%s: %s has a local with no type", path, fn.Sym)
		}
	}
	return count
}

// isStmtList reports whether the Body and Else of a node hold statements. A
// switch and a select hold their clauses there, and a clause is not a
// statement.
func isStmtList(n *Node) bool {
	switch n.Op {
	case OSwitch, OTypeSwitch, OSelect:
		return false
	}
	return true
}

// buildPrelude is the package every table case is built into. It holds the
// declarations the cases need and nothing else, so that a case is one
// construct.
const buildPrelude = `package p

import "unsafe"

var _ unsafe.Pointer

type T struct {
	A, B int
	S    []string
}

func (t T) M() int  { return t.A }
func (t *T) P() int { return t.B }

type C struct{ A, B int }

type I interface{ M() int }

type V int

func (v V) M() int { return int(v) }

type E struct{ T }

var g int
var gi I
var gt T

func one() int             { return 1 }
func p1() int              { return 1 }
func p2() int              { return 2 }
func two() (int, string)   { return 1, "x" }
func sink(...any)          {}
func pair(int, string)     {}
func take(i I)             {}
func iter(func(int) bool)  {}
func str() string          { return "" }
`

// buildSource builds one source file and fails on an error the builder
// reports, so that a case that silently built nothing cannot pass.
func buildSource(t *testing.T, body string) *Package {
	t.Helper()
	pkg, files, info := buildTypecheck(t, buildPrelude+"\n"+body)
	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out == nil {
		t.Fatal("Build returned no package")
	}
	return out
}

// buildFuncOf returns the function with the given name.
func buildFuncOf(t *testing.T, p *Package, name string) *Func {
	t.Helper()
	for _, fn := range p.Funcs {
		if fn.Name == name {
			return fn
		}
	}
	t.Fatalf("no function %s", name)
	return nil
}

// buildFind returns every node with an operation, in the order a walk reaches
// them.
func buildFind(fn *Func, op Op) []*Node {
	var out []*Node
	for _, s := range fn.Body {
		Walk(s, func(n *Node) bool {
			if n.Op == op {
				out = append(out, n)
			}
			return true
		})
	}
	return out
}

// buildFirst returns the first node with an operation, or fails.
func buildFirst(t *testing.T, fn *Func, op Op) *Node {
	t.Helper()
	found := buildFind(fn, op)
	if len(found) == 0 {
		t.Fatalf("%s has no %s:\n%s", fn.Name, op, buildDump(fn))
	}
	return found[0]
}

// buildDump renders a function for a failure message.
func buildDump(fn *Func) string {
	var b strings.Builder
	var walk func(n *Node, depth int)
	walk = func(n *Node, depth int) {
		if n == nil {
			return
		}
		fmt.Fprintf(&b, "%s%s", strings.Repeat("  ", depth), n.Op)
		if n.Op == OBinary || n.Op == OCompare || n.Op == OUnary {
			fmt.Fprintf(&b, " op%d", n.Op1)
		}
		if n.Op == OAssign && n.Op1 == syntax.Def {
			b.WriteString(" :=")
		}
		if n.Obj != nil {
			fmt.Fprintf(&b, " %s(%s)", n.Obj.Name, n.Obj.Class)
		}
		if n.Val != nil {
			fmt.Fprintf(&b, " %s", n.Val)
		}
		if n.Type != nil {
			fmt.Fprintf(&b, " : %s", n.Type)
		}
		b.WriteString("\n")
		for _, c := range []*Node{n.X, n.Y} {
			walk(c, depth+1)
		}
		for _, l := range [][]Stmt{n.Args, n.Init, n.Body, n.Post, n.Else} {
			for _, c := range l {
				walk(c, depth+1)
			}
		}
	}
	fmt.Fprintf(&b, "func %s\n", fn.Sym)
	for _, s := range fn.Body {
		walk(s, 1)
	}
	return b.String()
}

// TestBuildLoweringTable is specs/020-ir.md's lowering table as a checklist.
//
// One source snippet per row, built, asserting that the node the row names
// appears with the operands the lowering will need. The table is the contract
// between this spec, specs/021-ssa-construction.md and
// specs/031-runtime-lowering.md, and a row with no node here is a construct
// that would reach SSA construction as something else.
func TestBuildLoweringTable(t *testing.T) {
	for _, tc := range []struct {
		row   string
		body  string
		op    Op
		check func(t *testing.T, p *Package, fn *Func, n *Node)
	}{
		{
			row:  "range over slice",
			body: `func f(s []int) { for i, v := range s { sink(i, v) } }`,
			op:   ORange,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X == nil || n.X.Type.Kind != Slice {
					t.Errorf("the range operand is %v", n.X)
				}
				if len(n.Args) != 2 {
					t.Errorf("%d iteration variables, want 2", len(n.Args))
				}
				if len(n.Body) == 0 {
					t.Error("the range has no body")
				}
			},
		},
		{
			row:  "range over array",
			body: `func f() { var a [4]int; for i, v := range a { sink(i, v) } }`,
			op:   ORange,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				// By pointer: the array is not copied, so its address is
				// taken and the object cannot live in a register.
				if n.X.Op != OAddr {
					t.Errorf("the range operand is %s, want the address of the array", n.X.Op)
				}
				if !n.X.X.Obj.Addrtaken {
					t.Error("the array is not marked address-taken")
				}
			},
		},
		{
			row:  "range over pointer to array",
			body: `func f(a *[4]int) { for i, v := range a { sink(i, v) } }`,
			op:   ORange,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Ptr {
					t.Errorf("the range operand is %s", n.X.Type)
				}
			},
		},
		{
			row:  "range over string",
			body: `func f(s string) { for i, c := range s { sink(i, c) } }`,
			op:   ORange,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != String {
					t.Errorf("the range operand is %s", n.X.Type)
				}
				if n.Args[1].Type.Kind != Int32 {
					t.Errorf("the value of a string range is %s, want a rune", n.Args[1].Type)
				}
			},
		},
		{
			row:  "range over map",
			body: `func f(m map[string]int) { for k, v := range m { sink(k, v) } }`,
			op:   ORange,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Map {
					t.Errorf("the range operand is %s", n.X.Type)
				}
			},
		},
		{
			row:  "range over channel",
			body: `func f(c chan int) { for v := range c { sink(v) } }`,
			op:   ORange,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Chan || len(n.Args) != 1 {
					t.Errorf("the range is over %s with %d variables", n.X.Type, len(n.Args))
				}
			},
		},
		{
			row:  "range over integer",
			body: `func f(n int) { for i := range n { sink(i) } }`,
			op:   ORange,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if !n.X.Type.Kind.IsInteger() {
					t.Errorf("the range operand is %s", n.X.Type)
				}
			},
		},
		{
			row:  "range over function",
			body: `func f(seq func(func(int) bool)) { for v := range seq { sink(v) } }`,
			op:   ORange,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != FuncKind {
					t.Errorf("the range operand is %s", n.X.Type)
				}
			},
		},
		{
			row:  "map read",
			body: `func f(m map[string]int) int { return m["a"] }`,
			op:   OIndex,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Map || n.Y == nil {
					t.Errorf("the index is %s[%v]", n.X.Type, n.Y)
				}
			},
		},
		{
			row:  "map write",
			body: `func f(m map[string]int) { m["a"] = 1 }`,
			op:   OIndex,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if !IsAssign(fn.Body[0]) || fn.Body[0].X != n {
					t.Errorf("the map write is not an assignment to the index:\n%s", buildDump(fn))
				}
			},
		},
		{
			row:  "map delete",
			body: `func f(m map[string]int) { delete(m, "a") }`,
			op:   ODelete,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X == nil || n.Y == nil {
					t.Error("delete has no map or no key")
				}
			},
		},
		{
			row:  "channel send",
			body: `func f(c chan int) { c <- 1 }`,
			op:   OSend,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Chan || n.Y.Type.Kind != Int64 {
					t.Errorf("send %s <- %s", n.X.Type, n.Y.Type)
				}
			},
		},
		{
			row:  "channel receive",
			body: `func f(c chan int) int { return <-c }`,
			op:   ORecv,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Chan {
					t.Errorf("receive from %s", n.X.Type)
				}
			},
		},
		{
			row: "select",
			body: `func f(c chan int) {
				select {
				case v := <-c:
					sink(v)
				case c <- 1:
				default:
				}
			}`,
			op: OSelect,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(n.Body) != 3 {
					t.Fatalf("%d clauses, want 3", len(n.Body))
				}
				if len(n.Body[0].Init) == 0 || len(n.Body[1].Init) == 0 {
					t.Error("a communication clause has no communication statement")
				}
				if len(n.Body[2].Init) != 0 {
					t.Error("the default clause has a communication statement")
				}
			},
		},
		{
			row:  "type assertion",
			body: `func f(i I) T { return i.(T) }`,
			op:   OTypeAssert,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Interface {
					t.Errorf("the operand is %s", n.X.Type)
				}
				if n.Y == nil || n.Y.Obj == nil || n.Y.Obj.Class != ClassType {
					t.Error("the asserted type is not a type operand")
				}
			},
		},
		{
			row: "type switch",
			body: `func f(a any) {
				switch v := a.(type) {
				case int:
					sink(v)
				case nil:
				default:
					sink(v)
				}
			}`,
			op: OTypeSwitch,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(n.Body) != 3 {
					t.Fatalf("%d clauses, want 3", len(n.Body))
				}
				if n.Body[0].Obj == nil || n.Body[0].Obj.Type.Kind != Int64 {
					t.Errorf("the first clause's variable is %v", n.Body[0].Obj)
				}
				if n.Body[0].Args[0].Obj.Class != ClassType {
					t.Error("a clause type is not a type operand")
				}
				if n.Body[1].Args[0].Op != OConst {
					t.Error("case nil is not a constant")
				}
				if len(n.Body[2].Args) != 0 {
					t.Error("the default clause has case types")
				}
			},
		},
		{
			row:  "interface conversion",
			body: `func f() { gi = gt }`,
			op:   OConvert,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Type.Kind != Interface || n.X.Type.Kind != Struct {
					t.Errorf("the conversion is %s to %s", n.X.Type, n.Type)
				}
			},
		},
		{
			row:  "method value",
			body: `func f(t T) func() int { return t.M }`,
			op:   OClosure,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Obj == nil || n.Obj.Name != "p.T.M" {
					t.Errorf("the method value names %v", n.Obj)
				}
				if len(n.Args) != 1 {
					t.Fatalf("the method value holds %d values, want the receiver", len(n.Args))
				}
				// A method value and a literal with one capture are otherwise
				// the same node, and the receiver is held by value while a
				// capture is shared.
				if n.Index == closureLiteral {
					t.Error("the method value is marked a function literal")
				}
			},
		},
		{
			row:  "method expression",
			body: `func f() func(T) int { return T.M }`,
			op:   OGlobal,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Obj.Class != ClassFunc || n.Obj.Name != "p.T.M" {
					t.Errorf("the method expression is %v", n.Obj)
				}
			},
		},
		{
			row:  "closure",
			body: `func f() func() int { x := 1; return func() int { return x } }`,
			op:   OClosure,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(n.Args) != 1 || n.Args[0].Obj.Name != "x" {
					t.Errorf("the closure captures %v", n.Args)
				}
				if n.Index != closureLiteral {
					t.Errorf("the literal is marked a method value with index %d", n.Index)
				}
				if !n.Args[0].Obj.Addrtaken {
					t.Error("a captured variable is not address-taken")
				}
				if len(p.Funcs) < 2 {
					t.Error("the literal did not become a function of the package")
				}
			},
		},
		{
			row:  "defer",
			body: `func f() { defer sink(1) }`,
			op:   ODefer,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X == nil || n.X.Op != OCall {
					t.Error("defer has no call")
				}
			},
		},
		{
			row:  "go",
			body: `func f() { go sink(1) }`,
			op:   OGo,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X == nil || n.X.Op != OCall {
					t.Error("go has no call")
				}
			},
		},
		{
			row:  "panic",
			body: `func f() { panic("x") }`,
			op:   OPanic,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Interface {
					t.Errorf("the operand of panic is %s, want an interface", n.X.Type)
				}
			},
		},
		{
			row:  "recover",
			body: `func f() { defer func() { _ = recover() }() }`,
			op:   OClosure,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				lit := buildFuncOf(t, p, fn.Name+".func0")
				if len(buildFind(lit, ORecover)) != 1 {
					t.Errorf("no recover in the deferred literal:\n%s", buildDump(lit))
				}
			},
		},
		{
			row:  "append",
			body: `func f(s []int) []int { return append(s, 1, 2) }`,
			op:   OAppend,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(n.Args) != 3 || n.Index == spread {
					t.Errorf("append has %d operands, spread=%v", len(n.Args), n.Index == spread)
				}
			},
		},
		{
			row:  "append with a spread",
			body: `func f(s, u []int) []int { return append(s, u...) }`,
			op:   OAppend,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Index != spread {
					t.Error("the spread of append is not recorded")
				}
			},
		},
		{
			row:  "copy",
			body: `func f(a, b []int) { copy(a, b) }`,
			op:   OCopy,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X == nil || n.Y == nil {
					t.Error("copy has no destination or no source")
				}
			},
		},
		{
			row:  "make",
			body: `func f() []int { return make([]int, 1, 2) }`,
			op:   OMake,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Type.Kind != Slice || len(n.Args) != 2 {
					t.Errorf("make %s with %d sizes", n.Type, len(n.Args))
				}
			},
		},
		{
			row:  "new",
			body: `func f() *int { return new(int) }`,
			op:   ONew,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Type.Kind != Ptr || len(n.Args) != 0 {
					t.Errorf("new is %s with %d elements", n.Type, len(n.Args))
				}
			},
		},
		{
			row:  "composite literal",
			body: `func f() T { return T{A: 1} }`,
			op:   OCompositeLit,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Type.Kind != Struct || len(n.Args) != 3 {
					t.Fatalf("the literal is %s with %d elements:\n%s", n.Type, len(n.Args), buildDump(fn))
				}
				// Normalised: one element per field, in declaration order,
				// with the zero value written out for a field the literal
				// left out.
				if n.Args[0].Val == nil || n.Args[0].Val.String() != "1" {
					t.Errorf("field A is %v", n.Args[0].Val)
				}
				if n.Args[1].Val == nil || n.Args[1].Val.String() != "0" {
					t.Errorf("field B is %v, want its zero value", n.Args[1].Val)
				}
				if n.Args[2].Val == nil || n.Args[2].Val.String() != "nil" {
					t.Errorf("field S is %v, want nil", n.Args[2].Val)
				}
			},
		},
		{
			row:  "escaping composite literal",
			body: `func f() *T { return &T{A: 1} }`,
			op:   OAddr,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Op != OCompositeLit || n.X.Type.Kind != Struct {
					t.Errorf("the address is of %s", n.X.Op)
				}
			},
		},
		{
			row:  "string concatenation",
			body: `func f(a, b string) string { return a + b }`,
			op:   OBinary,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Type.Kind != String || n.Op1 != syntax.Add {
					t.Errorf("the concatenation is %s %d", n.Type, n.Op1)
				}
			},
		},
		{
			row:  "string comparison",
			body: `func f(a, b string) bool { return a < b }`,
			op:   OCompare,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != String || n.Type.Kind != Bool {
					t.Errorf("the comparison is %s, yielding %s", n.X.Type, n.Type)
				}
			},
		},
		{
			row:  "string to []byte",
			body: `func f(s string) []byte { return []byte(s) }`,
			op:   OConvert,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Type.Kind != Slice || n.X.Type.Kind != String {
					t.Errorf("the conversion is %s to %s", n.X.Type, n.Type)
				}
			},
		},
		{
			row:  "[]rune to string",
			body: `func f(r []rune) string { return string(r) }`,
			op:   OConvert,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Type.Kind != String || n.X.Type.Elem.Kind != Int32 {
					t.Errorf("the conversion is %s to %s", n.X.Type, n.Type)
				}
			},
		},
		{
			row:  "struct comparison",
			body: `func f(a, b C) bool { return a == b }`,
			op:   OCompare,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Struct {
					t.Errorf("the comparison is of %s", n.X.Type)
				}
			},
		},
		{
			row:  "array comparison",
			body: `func f(a, b [2]int) bool { return a == b }`,
			op:   OCompare,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X.Type.Kind != Array {
					t.Errorf("the comparison is of %s", n.X.Type)
				}
			},
		},
		{
			row:  "slice expression",
			body: `func f(s []int) []int { return s[1:] }`,
			op:   OSlice,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(n.Args) != 3 {
					t.Fatalf("the slice has %d bounds, want three places", len(n.Args))
				}
				if n.Args[0].Val.String() != "1" {
					t.Errorf("the low bound is %v", n.Args[0].Val)
				}
				// A bound the source left out is nil, which says the default
				// applies rather than that a zero was written.
				if n.Args[1] != nil || n.Args[2] != nil {
					t.Errorf("the missing bounds are %v and %v", n.Args[1], n.Args[2])
				}
			},
		},
		{
			row:  "three-index slice expression",
			body: `func f(s []int) []int { return s[1:2:3] }`,
			op:   OSlice,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Args[2] == nil {
					t.Error("the three-index slice has no maximum")
				}
			},
		},
		{
			row:  "complex arithmetic",
			body: `func f(a, b complex128) complex128 { return a * b }`,
			op:   OBinary,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.Type.Kind != Complex128 {
					t.Errorf("the operation is on %s", n.Type)
				}
			},
		},
		{
			row:  "complex, real and imag",
			body: `func f(re, im float64) float64 { c := complex(re, im); return real(c) + imag(c) }`,
			op:   OComplex,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(buildFind(fn, OReal)) != 1 || len(buildFind(fn, OImag)) != 1 {
					t.Errorf("real and imag are not nodes:\n%s", buildDump(fn))
				}
			},
		},
		{
			row:  "multi-value assignment",
			body: `func f() { a, b := two(); sink(a, b) }`,
			op:   OAssign,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if !IsAssign(n) || n.X != nil || len(n.Args) != 2 {
					t.Fatalf("the multi-value assignment is:\n%s", buildDump(fn))
				}
				if n.Y.Op != OCall {
					t.Errorf("the source is %s", n.Y.Op)
				}
			},
		},
		{
			row:  "len and cap",
			body: `func f(s []int) int { return len(s) + cap(s) }`,
			op:   OLen,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(buildFind(fn, OCap)) != 1 {
					t.Error("cap is not a node")
				}
			},
		},
		{
			row:  "clear, min and max",
			body: `func f(m map[string]int, a, b int) int { clear(m); return min(a, b) + max(a, b) }`,
			op:   OClear,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(buildFind(fn, OMin)) != 1 || len(buildFind(fn, OMax)) != 1 {
					t.Errorf("min and max are not nodes:\n%s", buildDump(fn))
				}
			},
		},
		{
			row:  "close",
			body: `func f(c chan int) { close(c) }`,
			op:   OClose,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X == nil || n.X.Type.Kind != Chan {
					t.Errorf("close is of %v", n.X)
				}
			},
		},
		{
			row:  "print and println",
			body: `func f(x int) { print(x, "\n"); println(x) }`,
			op:   OPrint,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if len(n.Args) != 2 {
					t.Errorf("print has %d operands", len(n.Args))
				}
				ln := buildFind(fn, OPrintln)
				if len(ln) != 1 || len(ln[0].Args) != 1 {
					t.Errorf("println is %v", ln)
				}
			},
		},
		{
			// Operations, not calls. None of them reaches the runtime, and a
			// pass that had to recognise an intrinsic by matching the name of
			// a global would emit a call to a function that does not exist
			// the first time it forgot.
			row: "the unsafe intrinsics",
			body: `func f(p unsafe.Pointer, s []int, str string) unsafe.Pointer {
				_ = unsafe.Slice((*int)(p), 3)
				_ = unsafe.SliceData(s)
				_ = unsafe.String((*byte)(p), 3)
				_ = unsafe.StringData(str)
				return unsafe.Add(p, 1)
			}`,
			op: OUnsafeAdd,
			check: func(t *testing.T, p *Package, fn *Func, n *Node) {
				if n.X == nil || n.X.Type.Kind != UnsafePtr || n.Y == nil {
					t.Errorf("unsafe.Add is %s(%v, %v)", n.Op, n.X, n.Y)
				}
				for _, tc := range []struct {
					op    Op
					want  Kind
					binop bool
				}{
					{OUnsafeSlice, Slice, true},
					{OUnsafeSliceData, Ptr, false},
					{OUnsafeString, String, true},
					{OUnsafeStringData, Ptr, false},
				} {
					got := buildFind(fn, tc.op)
					if len(got) != 1 {
						t.Errorf("%d nodes for %s", len(got), tc.op)
						continue
					}
					if got[0].Type.Kind != tc.want {
						t.Errorf("%s yields %s, want %s", tc.op, got[0].Type, tc.want)
					}
					if got[0].X == nil || (tc.binop && got[0].Y == nil) {
						t.Errorf("%s has no operands", tc.op)
					}
				}
				// Nothing is left that names an intrinsic as a symbol.
				for _, g := range buildFind(fn, OGlobal) {
					if strings.HasPrefix(g.Obj.Name, "unsafe.") {
						t.Errorf("an intrinsic is encoded as a call to %s", g.Obj.Name)
					}
				}
			},
		},
	} {
		t.Run(tc.row, func(t *testing.T) {
			p := buildSource(t, tc.body)
			fn := buildFuncOf(t, p, "f")
			n := buildFirst(t, fn, tc.op)
			tc.check(t, p, fn, n)
		})
	}
}

// TestBuildPackageInitialisation is the table's last row: one init function,
// ordered by specs/012-type-checking.md.
func TestBuildPackageInitialisation(t *testing.T) {
	p := buildSource(t, `
var a = b + 1
var b = one()
var c, d = two()

func init() { g = 1 }
func init() { g = 2 }
`)
	if len(p.Inits) != 1 {
		t.Fatalf("%d init functions, want one", len(p.Inits))
	}
	init := p.Inits[0]
	// The initialisation order is the checker's: b before a, because a
	// depends on b, and the declared init functions after the variables.
	var order []string
	for _, s := range init.Body {
		switch {
		case IsAssign(s) && s.X != nil && s.X.Obj != nil:
			order = append(order, s.X.Obj.Name)
		case IsAssign(s):
			names := ""
			for _, d := range s.Args {
				names += d.Obj.Name
			}
			order = append(order, names)
		case s.Op == OCall:
			order = append(order, s.X.Obj.Name)
		}
	}
	// The names are the linker symbols: a package-level variable carries its
	// package as a function does, so that every package that reads it names
	// the same symbol.
	want := []string{"p.b", "p.a", "p.cp.d", "p.init.0", "p.init.1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("the init order is %v, want %v\n%s", order, want, buildDump(init))
	}
	// Two init functions in one package need two symbols.
	if buildFuncOf(t, p, "init").Sym == "p.init" {
		t.Error("a declared init took the package's init symbol")
	}
}

// buildStr renders an expression compactly, so that a test can state the shape
// it wants as text.
func buildStr(n *Node) string {
	if n == nil {
		return "<nil>"
	}
	switch n.Op {
	case OConst:
		if n.Val == nil {
			return "?"
		}
		return n.Val.String()
	case OLocal, OGlobal:
		return n.Obj.Name
	case OAssign:
		dst := buildStr(n.X)
		if n.X == nil {
			var parts []string
			for _, d := range n.Args {
				parts = append(parts, buildStr(d))
			}
			dst = strings.Join(parts, ", ")
		}
		return dst + " = " + buildStr(n.Y)
	case OBinary, OCompare:
		return "(" + buildStr(n.X) + " " + buildOpName(n.Op1) + " " + buildStr(n.Y) + ")"
	case OCall:
		var args []string
		for _, a := range n.Args {
			args = append(args, buildStr(a))
		}
		return buildStr(n.X) + "(" + strings.Join(args, ", ") + ")"
	case OField:
		return buildStr(n.X) + ".f" + fmt.Sprint(n.Index)
	case OIndex:
		return buildStr(n.X) + "[" + buildStr(n.Y) + "]"
	case OSlice:
		bounds := make([]string, 0, len(n.Args))
		for _, a := range n.Args {
			if a == nil {
				bounds = append(bounds, "")
				continue
			}
			bounds = append(bounds, buildStr(a))
		}
		return buildStr(n.X) + "[" + strings.Join(bounds, ":") + "]"
	case ODeref:
		return "*" + buildStr(n.X)
	case OAddr:
		return "&" + buildStr(n.X)
	case OConvert:
		return n.Type.String() + "(" + buildStr(n.X) + ")"
	}
	var parts []string
	for _, c := range []*Node{n.X, n.Y} {
		if c != nil {
			parts = append(parts, buildStr(c))
		}
	}
	for _, a := range n.Args {
		parts = append(parts, buildStr(a))
	}
	out := n.Op.String() + "(" + strings.Join(parts, ", ") + ")"
	var body []string
	for _, l := range [][]Stmt{n.Body, n.Post, n.Else} {
		for _, c := range l {
			body = append(body, buildStr(c))
		}
	}
	if len(body) > 0 {
		out += "{" + strings.Join(body, "; ") + "}"
	}
	return out
}

func buildOpName(op syntax.Operator) string {
	switch op {
	case syntax.Add:
		return "+"
	case syntax.Sub:
		return "-"
	case syntax.Mul:
		return "*"
	case syntax.Eql:
		return "=="
	case syntax.Neq:
		return "!="
	case syntax.Lss:
		return "<"
	case syntax.AndAnd:
		return "&&"
	case syntax.OrOr:
		return "||"
	case syntax.Shl:
		return "<<"
	}
	return fmt.Sprintf("op%d", op)
}

// buildLines renders the top-level statements of a function.
func buildLines(fn *Func) []string {
	out := make([]string, 0, len(fn.Body))
	for _, s := range fn.Body {
		out = append(out, buildStr(s))
	}
	return out
}

// TestBuildEvaluationOrder is the rule of specs/020-ir.md's order of
// evaluation section.
//
// The specification pins the order of function calls, communication operations
// and index expressions within a statement, and this is the last pass that can
// pin it. The temporaries below are that order written down. The negative
// cases matter as much: a call that the program may not make cannot be hoisted
// to a place where it is always made.
func TestBuildEvaluationOrder(t *testing.T) {
	for _, tc := range []struct {
		what string
		body string
		want []string
	}{
		{
			what: "the arguments of a call, left to right",
			body: `func f() { sink(p1(), p2()) }`,
			want: []string{
				".autotmp_0 = p.p1()",
				".autotmp_1 = p.p2()",
				"p.sink(compositelit(interface(.autotmp_0), interface(.autotmp_1)))",
			},
		},
		{
			what: "the operands of a binary operation",
			body: `func f() int { return p1() + p2() }`,
			want: []string{
				".autotmp_0 = p.p1()",
				".autotmp_1 = p.p2()",
				"return((.autotmp_0 + .autotmp_1))",
			},
		},
		{
			what: "an index on the left before the value on the right",
			body: `func f(m map[int]int) { m[p1()] = p2() }`,
			want: []string{
				".autotmp_0 = p.p1()",
				"m[.autotmp_0] = p.p2()",
			},
		},
		{
			what: "every right-hand side before any assignment",
			body: `func f(x, y int) { x, y = y, x }`,
			want: []string{
				".autotmp_0 = y",
				".autotmp_1 = x",
				"x = .autotmp_0",
				"y = .autotmp_1",
			},
		},
		{
			what: "a call in a parallel assignment",
			body: `func f(x, y int) { x, y = p1(), p2() }`,
			want: []string{
				".autotmp_0 = p.p1()",
				".autotmp_1 = p.p2()",
				"x = .autotmp_0",
				"y = .autotmp_1",
			},
		},
		{
			what: "the value of a send",
			body: `func f(c chan int) { c <- p1() }`,
			want: []string{
				".autotmp_0 = p.p1()",
				"send(c, .autotmp_0)",
			},
		},
		{
			what: "the results of a return",
			body: `func f() (int, int) { return p1(), p2() }`,
			want: []string{
				".autotmp_0 = p.p1()",
				".autotmp_1 = p.p2()",
				"return(.autotmp_0, .autotmp_1)",
			},
		},
		{
			what: "one call needs no temporary",
			body: `func f() int { return p1() }`,
			want: []string{"return(p.p1())"},
		},
		{
			what: "the right operand of && is not hoisted",
			body: `func f() bool { return p1() == 0 && p2() == 0 }`,
			want: []string{
				".autotmp_0 = p.p1()",
				"return(((.autotmp_0 == 0) && (p.p2() == 0)))",
			},
		},
		{
			what: "the right operand of || is not hoisted",
			body: `func f() bool { return p1() == 0 || p2() == 0 }`,
			want: []string{
				".autotmp_0 = p.p1()",
				"return(((.autotmp_0 == 0) || (p.p2() == 0)))",
			},
		},
		{
			what: "a loop condition is not hoisted out of the loop",
			body: `func f() { for p1() == 0 { } }`,
			want: []string{"for((p.p1() == 0))"},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := buildSource(t, tc.body)
			fn := buildFuncOf(t, p, "f")
			got := buildLines(fn)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("got\n%s\nwant\n%s\n\n%s",
					strings.Join(got, "\n"), strings.Join(tc.want, "\n"), buildDump(fn))
			}
		})
	}
}

// buildObjOf returns the object of a name in a function.
func buildObjOf(t *testing.T, fn *Func, name string) *Object {
	t.Helper()
	for _, list := range [][]*Object{fn.Params, fn.Results, fn.Locals} {
		for _, o := range list {
			if o.Name == name {
				return o
			}
		}
	}
	if fn.Recv != nil && fn.Recv.Name == name {
		return fn.Recv
	}
	t.Fatalf("%s has no object %s:\n%s", fn.Name, name, buildDump(fn))
	return nil
}

// TestBuildAddrtaken checks the flag that specs/020-ir.md warns about: being
// conservative is safe and expensive, being wrong is memory corruption.
//
// The implicit cases are the ones worth having a test for. Nobody forgets &x.
func TestBuildAddrtaken(t *testing.T) {
	for _, tc := range []struct {
		what string
		body string
		obj  string
		want bool
	}{
		{"the address is taken", `func f() { x := 1; sink(&x) }`, "x", true},
		{"a value is copied", `func f() { x := 1; y := x; sink(y) }`, "x", false},
		{"a value is passed", `func f() { x := 1; sink(x) }`, "x", false},
		{"a method with a pointer receiver on an addressable value",
			`func f() { var t T; t.P() }`, "t", true},
		{"a method with a value receiver", `func f() { var t T; t.M() }`, "t", false},
		{"a method value with a pointer receiver",
			`func f() func() int { var t T; return t.P }`, "t", true},
		{"a method through a pointer takes no new address",
			`func f(t *T) { t.P() }`, "t", false},
		{"a field of a struct", `func f() { var t T; sink(&t.A) }`, "t", true},
		{"an element of an array", `func f() { var a [4]int; sink(&a[0]) }`, "a", true},
		{"an element of a slice", `func f() { s := []int{1}; sink(&s[0]) }`, "s", false},
		{"slicing an array", `func f() { var a [4]int; sink(a[:]) }`, "a", true},
		{"slicing a slice", `func f(s []int) { sink(s[1:]) }`, "s", false},
		{"a range over an array with both variables",
			`func f() { var a [4]int; for i, v := range a { sink(i, v) } }`, "a", true},
		{"a closure capture", `func f() { x := 1; sink(func() int { return x }) }`, "x", true},
		{"a variable a closure does not capture",
			`func f() { x := 1; sink(x, func() int { return 2 }) }`, "x", false},
		{"a field assignment", `func f() { var t T; t.A = 1 }`, "t", false},
		{"an assignment to an element", `func f() { var a [4]int; a[0] = 1 }`, "a", false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := buildSource(t, tc.body)
			fn := buildFuncOf(t, p, "f")
			o := buildObjOf(t, fn, tc.obj)
			if o.Addrtaken != tc.want {
				t.Errorf("%s is address-taken=%v, want %v:\n%s",
					tc.obj, o.Addrtaken, tc.want, buildDump(fn))
			}
		})
	}
}

// TestBuildNamesResolveToObjects is specs/020-ir.md's second difference from
// the syntax tree. An identifier is a pointer to its declaration, and
// shadowing is gone because two declarations are two objects.
func TestBuildNamesResolveToObjects(t *testing.T) {
	p := buildSource(t, `
func f(a int) (r int) {
	x := a
	{
		x := x + 1
		sink(x)
	}
	sink(x)
	r = x
	return
}
`)
	fn := buildFuncOf(t, p, "f")
	if len(fn.Params) != 1 || fn.Params[0].Class != ClassParam || fn.Params[0].Name != "a" {
		t.Errorf("the parameters are %v", fn.Params)
	}
	if len(fn.Results) != 1 || fn.Results[0].Class != ClassResult || fn.Results[0].Name != "r" {
		t.Errorf("the results are %v", fn.Results)
	}
	var xs []*Object
	for _, o := range fn.Locals {
		if o.Name == "x" {
			xs = append(xs, o)
		}
	}
	if len(xs) != 2 {
		t.Fatalf("%d locals named x, want two declarations:\n%s", len(xs), buildDump(fn))
	}
	if xs[0] == xs[1] {
		t.Error("two declarations of x share one object; the shadowing survived")
	}
	// Every use of one declaration is one object.
	seen := map[*Object]int{}
	for _, s := range fn.Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OLocal && n.Obj.Name == "x" {
				seen[n.Obj]++
			}
			return true
		})
	}
	if len(seen) != 2 {
		t.Errorf("the uses of x resolve to %d objects, want 2", len(seen))
	}
}

// TestBuildObjectClasses covers the classes an object can have, including the
// two that only a declaration reaches.
func TestBuildObjectClasses(t *testing.T) {
	pkg, files, info := buildTypecheck(t, `package p

const K = 3

type T int

var G int

func F() {}
`)
	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out.Globals) != 1 || out.Globals[0].Class != ClassGlobal {
		t.Errorf("the globals are %v", out.Globals)
	}
	if len(out.Funcs) != 1 || out.Funcs[0].Sym != "p.F" {
		t.Errorf("the functions are %v", out.Funcs)
	}
	// A constant and a type declare no storage, so nothing in the package
	// refers to them. The classes exist for the objects the builder makes for
	// a type operand and for a constant the checker did not fold, and they
	// are checked here directly.
	b := &builder{
		conv: NewConverter(), info: info, tpkg: pkg,
		objs: make(map[types2.Object]*Object), owner: make(map[*Object]*Func),
		ptrs: make(map[types2.Type]*Type), out: out,
	}
	for _, tc := range []struct {
		name  string
		class Class
	}{
		{"K", ClassConst},
		{"T", ClassType},
		{"G", ClassGlobal},
		{"F", ClassFunc},
	} {
		o := b.obj(pkg.Scope().Lookup(tc.name))
		if o.Class != tc.class {
			t.Errorf("%s is a %s, want a %s", tc.name, o.Class, tc.class)
		}
		if again := b.obj(pkg.Scope().Lookup(tc.name)); again != o {
			t.Errorf("%s made two objects", tc.name)
		}
	}
	if b.obj(nil) != nil {
		t.Error("a nil object made an object")
	}
}

// TestBuildImplicitConversions is specs/020-ir.md's first difference from the
// syntax tree: every implicit conversion the specification permits is a node.
//
// The cases are the contexts the specification calls an assignment. After this
// pass no assignability rule is left to apply: the type on a node is the type
// of the value it produces.
func TestBuildImplicitConversions(t *testing.T) {
	for _, tc := range []struct {
		what string
		body string
		want string
	}{
		{
			what: "an assignment to an interface variable",
			body: `func f(t T) { gi = t }`,
			want: "gi = p.I(t)",
		},
		{
			what: "an argument",
			body: `func f(t T) { take(t) }`,
			want: "p.take(p.I(t))",
		},
		{
			what: "a result",
			body: `func f(t T) I { return t }`,
			want: "return(p.I(t))",
		},
		{
			what: "a channel send",
			body: `func f(c chan I, t T) { c <- t }`,
			want: "send(c, p.I(t))",
		},
		{
			what: "a composite literal element",
			body: `func f(t T) []I { return []I{t} }`,
			want: "return(compositelit(p.I(t)))",
		},
		{
			what: "a map literal key and value",
			body: `func f(t T) map[I]I { return map[I]I{t: t} }`,
			want: "return(compositelit(p.I(t) = p.I(t)))",
		},
		{
			what: "a case of an expression switch",
			body: `func f(i I, v V) { switch i { case v: } }`,
			want: "switch(i){case(p.I(v))}",
		},
		{
			what: "an untyped constant takes the type of its destination",
			body: `func f() float64 { var x float64 = 1; return x }`,
			want: "x = 1",
		},
		{
			what: "nil takes the shape of a pointer",
			body: `func f() { var q *int = nil; sink(q) }`,
			want: "q = nil",
		},
		{
			what: "nil takes the shape of an interface",
			body: `func f() { var i I = nil; sink(i) }`,
			want: "i = nil",
		},
		{
			what: "an index is converted to int",
			body: `func f(s []int, i int32) int { return s[i] }`,
			want: "return(s[int(i)])",
		},
		{
			what: "a comparison of a concrete value against an interface",
			body: `func f(i I, v V) bool { return i == v }`,
			want: "return((i == p.I(v)))",
		},
		{
			what: "an explicit conversion is a node too",
			body: `func f(x int32) int64 { return int64(x) }`,
			want: "return(int64(x))",
		},
		{
			what: "an implicit dereference of an embedded pointer",
			body: `func f(e *E) int { return e.A }`,
			want: "return(*e.f0.f0)",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := buildSource(t, tc.body)
			fn := buildFuncOf(t, p, "f")
			got := strings.Join(buildLines(fn), "\n")
			if !strings.Contains(got, tc.want) {
				t.Errorf("got\n%s\nwant a line holding\n%s\n\n%s", got, tc.want, buildDump(fn))
			}
		})
	}
}

// TestBuildAssignmentForms checks the rewrites that make an assignment
// recognisable without context: after them, an OBinary in a statement list is
// an assignment and an OBinary in an operand position is an operation.
func TestBuildAssignmentForms(t *testing.T) {
	for _, tc := range []struct {
		what string
		body string
		want []string
	}{
		{
			what: "an operation assignment is rewritten",
			body: `func f(x, y int) { x += y }`,
			want: []string{"x = (x + y)"},
		},
		{
			what: "an increment is rewritten",
			body: `func f(x int) { x++ }`,
			want: []string{"x = (x + 1)"},
		},
		{
			what: "a shift assignment keeps the type of its count",
			body: `func f(x int, s uint) { x <<= s }`,
			want: []string{"x = (x << s)"},
		},
		{
			what: "the destination of an operation assignment is evaluated once",
			body: `func f(m map[int]int) { m[p1()] += 1 }`,
			want: []string{
				".autotmp_0 = p.p1()",
				"m[.autotmp_0] = (m[.autotmp_0] + 1)",
			},
		},
		{
			what: "a comma-ok map read",
			body: `func f(m map[string]int) { v, ok := m["a"]; sink(v, ok) }`,
			want: []string{`v, ok = m["a"]`},
		},
		{
			what: "a comma-ok type assertion",
			body: `func f(a any) { v, ok := a.(int); sink(v, ok) }`,
			want: []string{"v, ok = typeassert(a, int)"},
		},
		{
			what: "a comma-ok receive",
			body: `func f(c chan int) { v, ok := <-c; sink(v, ok) }`,
			want: []string{"v, ok = recv(c)"},
		},
		{
			what: "a multi-value assignment with a conversion",
			body: `func f() { var a any; var b string; a, b = two(); sink(a, b) }`,
			want: []string{
				".autotmp_0, .autotmp_1 = p.two()",
				"a = interface(.autotmp_0)",
				"b = .autotmp_1",
			},
		},
		{
			what: "an assignment to the blank identifier keeps its right-hand side",
			body: `func f() { _ = p1() }`,
			want: []string{"_ = p.p1()"},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := buildSource(t, tc.body)
			fn := buildFuncOf(t, p, "f")
			got := strings.Join(buildLines(fn), "\n")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("got\n%s\nwant a line holding\n%s\n\n%s", got, want, buildDump(fn))
				}
			}
		})
	}
}

// TestBuildBlankTakesNoSlot. The blank identifier names no storage, so nothing
// allocates a frame slot for it, and the right-hand side of an assignment to
// it is still evaluated.
func TestBuildBlankTakesNoSlot(t *testing.T) {
	p := buildSource(t, `func f() { _ = p1(); _, x := two(); sink(x) }`)
	fn := buildFuncOf(t, p, "f")
	for _, o := range fn.Locals {
		if o.Name == "_" {
			t.Errorf("the blank identifier took a frame slot:\n%s", buildDump(fn))
		}
	}
	if len(buildFind(fn, OCall)) < 2 {
		t.Errorf("an assignment to the blank identifier dropped its call:\n%s", buildDump(fn))
	}
}

// TestBuildCallForms checks the three shapes of a call, which
// specs/031-runtime-lowering.md lowers differently.
func TestBuildCallForms(t *testing.T) {
	t.Run("a method of a concrete type takes the receiver first", func(t *testing.T) {
		p := buildSource(t, `func f(t T) int { return t.M() }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if call.X.Op != OGlobal || call.X.Obj.Name != "p.T.M" {
			t.Fatalf("the callee is %v", call.X.Obj)
		}
		if len(call.Args) != 1 || call.Args[0].Obj.Name != "t" {
			t.Errorf("the arguments are %s", buildStr(call))
		}
	})
	t.Run("a pointer receiver takes an address", func(t *testing.T) {
		p := buildSource(t, `func f() int { var t T; return t.P() }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if call.Args[0].Op != OAddr {
			t.Errorf("the receiver is %s, want its address", buildStr(call.Args[0]))
		}
	})
	t.Run("a value receiver from a pointer dereferences", func(t *testing.T) {
		p := buildSource(t, `func f(t *T) int { return t.M() }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if call.Args[0].Op != ODeref {
			t.Errorf("the receiver is %s, want a dereference", buildStr(call.Args[0]))
		}
	})
	t.Run("a method of an interface keeps the receiver in the selection", func(t *testing.T) {
		p := buildSource(t, `func f(i I) int { return i.M() }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if call.X.Op != OField || call.X.X.Obj.Name != "i" {
			t.Fatalf("the callee is %s", buildStr(call.X))
		}
		if call.X.Obj == nil || call.X.Obj.Class != ClassFunc {
			t.Errorf("the selected method is %v", call.X.Obj)
		}
		if len(call.Args) != 0 {
			t.Errorf("an interface call carries %d arguments", len(call.Args))
		}
	})
	t.Run("a promoted method takes the embedded field as the receiver", func(t *testing.T) {
		p := buildSource(t, `func f(e E) int { return e.M() }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if buildStr(call.Args[0]) != "e.f0" {
			t.Errorf("the receiver is %s, want the embedded field", buildStr(call.Args[0]))
		}
	})
	t.Run("a call through a function value", func(t *testing.T) {
		p := buildSource(t, `func f(g func(int)) { g(1) }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if call.X.Op != OLocal || call.X.Obj.Name != "g" {
			t.Errorf("the callee is %s", buildStr(call.X))
		}
	})
	t.Run("the variadic parameter is packed", func(t *testing.T) {
		p := buildSource(t, `func f() { sink(1, 2) }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if len(call.Args) != 1 || call.Args[0].Op != OCompositeLit || len(call.Args[0].Args) != 2 {
			t.Errorf("the arguments are %s", buildStr(call))
		}
		if call.Args[0].Type.Kind != Slice {
			t.Errorf("the packed argument is %s", call.Args[0].Type)
		}
	})
	t.Run("a spread is not packed", func(t *testing.T) {
		p := buildSource(t, `func f(xs []any) { sink(xs...) }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if call.Index != spread || len(call.Args) != 1 || call.Args[0].Op != OLocal {
			t.Errorf("the call is %s with spread=%v", buildStr(call), call.Index == spread)
		}
	})
	t.Run("the results of one call fill the parameters of another", func(t *testing.T) {
		p := buildSource(t, `func f() { pair(two()) }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if len(call.Args) != 1 || call.Args[0].Op != OCall {
			t.Errorf("the arguments are %s", buildStr(call))
		}
		if call.Args[0].Type.Kind != Tuple || len(call.Args[0].Type.Fields) != 2 {
			t.Errorf("the inner call produces %s, want a result tuple", call.Args[0].Type)
		}
	})
	t.Run("an empty variadic call passes the zero slice", func(t *testing.T) {
		p := buildSource(t, `func f() { sink() }`)
		fn := buildFuncOf(t, p, "f")
		call := buildFirst(t, fn, OCall)
		if len(call.Args) != 1 || call.Args[0].Op != OCompositeLit || len(call.Args[0].Args) != 0 {
			t.Errorf("the arguments are %s", buildStr(call))
		}
	})
}

// TestBuildControlForms checks the encodings of the control statements that
// the node set does not name.
func TestBuildControlForms(t *testing.T) {
	t.Run("a for loop keeps its post statement in Else", func(t *testing.T) {
		p := buildSource(t, `func f() { for i := 0; i < 3; i++ { sink(i) } }`)
		fn := buildFuncOf(t, p, "f")
		loop := buildFirst(t, fn, OFor)
		if len(loop.Init) != 1 || !IsAssign(loop.Init[0]) {
			t.Errorf("the init is %v", loop.Init)
		}
		if loop.X == nil || loop.X.Op != OCompare {
			t.Errorf("the condition is %v", loop.X)
		}
		if len(loop.Post) != 1 || !IsAssign(loop.Post[0]) {
			t.Errorf("the post statement is %v", loop.Post)
		}
	})
	t.Run("a switch with no tag switches on true", func(t *testing.T) {
		p := buildSource(t, `func f(x int) { switch { case x > 0: sink(1) } }`)
		fn := buildFuncOf(t, p, "f")
		sw := buildFirst(t, fn, OSwitch)
		if sw.X.Op != OConst || sw.X.Val.String() != "true" {
			t.Errorf("the tag is %v", sw.X)
		}
	})
	t.Run("fallthrough is a labelled goto", func(t *testing.T) {
		p := buildSource(t, `func f(x int) { switch x { case 1: fallthrough; default: } }`)
		fn := buildFuncOf(t, p, "f")
		got := buildFind(fn, OGoto)
		if len(got) != 1 || got[0].Label != "fallthrough" {
			t.Errorf("fallthrough became %v", got)
		}
	})
	t.Run("a label and a labelled branch", func(t *testing.T) {
		p := buildSource(t, `func f() { L: for { break L }; goto L2; L2: }`)
		fn := buildFuncOf(t, p, "f")
		lbl := buildFirst(t, fn, OLabel)
		if lbl.Label != "L" || len(lbl.Body) != 1 || lbl.Body[0].Op != OFor {
			t.Errorf("the label holds %v", lbl)
		}
		brk := buildFirst(t, fn, OBreak)
		if brk.Label != "L" {
			t.Errorf("the break is to %q", brk.Label)
		}
		if buildFirst(t, fn, OGoto).Label != "L2" {
			t.Error("the goto lost its label")
		}
	})
	t.Run("a continue and a block", func(t *testing.T) {
		p := buildSource(t, `func f() { for { { continue } } }`)
		fn := buildFuncOf(t, p, "f")
		if len(buildFind(fn, OContinue)) != 1 || len(buildFind(fn, OBlock)) != 1 {
			t.Errorf("the loop is:\n%s", buildDump(fn))
		}
	})
	t.Run("an if with an init statement and an else", func(t *testing.T) {
		p := buildSource(t, `func f() { if x := p1(); x > 0 { sink(1) } else if x < 0 { sink(2) } else { sink(3) } }`)
		fn := buildFuncOf(t, p, "f")
		n := buildFirst(t, fn, OIf)
		if len(n.Init) == 0 || len(n.Body) == 0 || len(n.Else) != 1 {
			t.Errorf("the if is:\n%s", buildDump(fn))
		}
		if n.Else[0].Op != OIf {
			t.Errorf("the else is a %s", n.Else[0].Op)
		}
	})
}

// TestBuildErrors checks that what the builder cannot build is named rather
// than silently missing.
func TestBuildErrors(t *testing.T) {
	if _, err := Build(nil, nil, nil); err == nil {
		t.Error("Build with no package returned no error")
	}
	pkg, files, info := buildTypecheck(t, `package p

func G[T any](x T) T { return x }

func f() int { return G(1) }
`)
	out, err := Build(pkg, files, info)
	if err == nil {
		t.Error("a generic function built without an error")
	}
	if out == nil {
		t.Fatal("a generic function stopped the package")
	}
	// The rest of the package is built. A partial IR is what the diagnostic
	// for the skipped declaration is written against.
	if len(out.Funcs) != 1 || out.Funcs[0].Name != "f" {
		t.Errorf("the functions are %v", out.Funcs)
	}
}

// buildTypecheckWithImports type-checks a snippet that imports real packages,
// through the same source importer the corpus test uses.
func buildTypecheckWithImports(t *testing.T, src string) (*types2.Package, []*syntax.File, *types2.Info) {
	t.Helper()
	if fi, err := os.Stat(filepath.Join(runtime.GOROOT(), "src")); err != nil || !fi.IsDir() {
		t.Skip("no standard library to import from")
	}
	imp := newCorpusImporter()
	file, err := syntax.Parse(imp.fset.AddFile("x.go", len(src)), []byte(src), nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	files := []*syntax.File{file}
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{Fset: imp.fset, Importer: imp, Sizes: types2.SizesFor("gc", "amd64")}
	pkg, err := conf.Check("q", files, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	return pkg, files, info
}

// TestBuildQualifiedIdentifiers checks the names that live in another package.
// A qualified identifier is a name, not a selection, and the object it
// resolves to is the other package's.
func TestBuildQualifiedIdentifiers(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, `package q

import (
	"errors"
	"sort"
	"strings"
)

var ErrX = errors.New("x")

func f(s string) (bool, error) {
	var b strings.Builder
	b.WriteString(s)
	sort.Ints(nil)
	return strings.Contains(b.String(), "a"), ErrX
}
`)
	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	fn := buildFuncOf(t, out, "f")
	var callees []string
	for _, call := range buildFind(fn, OCall) {
		switch call.X.Op {
		case OGlobal:
			callees = append(callees, call.X.Obj.Name)
		case OField:
			callees = append(callees, "itab:"+call.X.Obj.Name)
		}
	}
	got := strings.Join(callees, ",")
	for _, want := range []string{"strings.(*Builder).WriteString", "sort.Ints", "strings.Contains"} {
		if !strings.Contains(got, want) {
			t.Errorf("the calls are %s, want one to %s", got, want)
		}
	}
	// A package-level variable of another package is a global of that package.
	if len(out.Inits) != 1 {
		t.Fatalf("%d init functions", len(out.Inits))
	}
	// The package is part of the symbol, as it is for a function. A global
	// that carried the bare name would be relocated against the package
	// being compiled, so a read of errors.ErrUnsupported would name
	// q.ErrUnsupported, which nothing defines.
	if len(out.Globals) != 1 || out.Globals[0].Name != "q.ErrX" {
		t.Errorf("the globals are %v, want one named q.ErrX", out.Globals)
	}
}

// TestBuildGlobalOfAnotherPackageKeepsItsPackage is the read side of the same
// rule.
//
// The declaration is in one package and the read is in another, and both name
// one symbol in the program. The name an ir.Object carries is what ssagen
// relocates against, and the only package available there is the package
// being compiled: a bare "Stdout" became "main.Stdout", which the linker
// reported as an undefined symbol with no source position on it.
func TestBuildGlobalOfAnotherPackageKeepsItsPackage(t *testing.T) {
	pkg, files, info := buildTypecheckWithImports(t, `package q

import "os"

func f() *os.File { return os.Stdout }
`)
	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	fn := buildFuncOf(t, out, "f")
	var names []string
	for _, n := range buildFind(fn, OGlobal) {
		if n.Obj != nil && n.Obj.Class == ClassGlobal {
			names = append(names, n.Obj.Name)
		}
	}
	if len(names) != 1 || names[0] != "os.Stdout" {
		t.Errorf("the globals read are %v, want [os.Stdout]", names)
	}
}

// TestBuildUntypedConstantContexts checks the type an untyped constant takes
// from the context, including the case where the context is an interface and
// the constant takes its default type first.
func TestBuildUntypedConstantContexts(t *testing.T) {
	p := buildSource(t, `
func f() {
	var a any = 1
	var b float64 = 2
	var c I = nil
	sink(3, nil)
	take(nil)
	_, _, _ = a, b, c
}
`)
	fn := buildFuncOf(t, p, "f")
	lines := strings.Join(buildLines(fn), "\n")
	// The constant takes its default type, and the conversion to the
	// interface is a node of its own.
	if !strings.Contains(lines, "a = interface(1)") {
		t.Errorf("an untyped constant assigned to an interface is:\n%s", lines)
	}
	if !strings.Contains(lines, "b = 2") {
		t.Errorf("an untyped constant assigned to a float is:\n%s", lines)
	}
	for _, n := range buildFind(fn, OConst) {
		if n.Type == nil || n.Type.Kind == Void {
			t.Errorf("a constant has no shape: %s\n%s", buildStr(n), buildDump(fn))
		}
	}
	// nil takes the shape of its destination and is not converted.
	var nils []*Type
	for _, n := range buildFind(fn, OConst) {
		if n.Val != nil && n.Val.String() == "nil" {
			nils = append(nils, n.Type)
		}
	}
	if len(nils) != 3 {
		t.Fatalf("%d nil constants, want 3:\n%s", len(nils), buildDump(fn))
	}
	for _, ty := range nils {
		if ty.Kind != Interface {
			t.Errorf("a nil assigned to an interface has kind %s", ty.Kind)
		}
	}
}

// TestBuildDeclarationsWithNoStatements checks that a declaration with no
// initialiser still declares, and that a constant and a type declare nothing.
func TestBuildDeclarationsWithNoStatements(t *testing.T) {
	p := buildSource(t, `
func f() int {
	var x int
	var y, z = 1, 2
	const k = 3
	type local struct{ A int }
	var w local
	return x + y + z + k + w.A
}
`)
	fn := buildFuncOf(t, p, "f")
	names := ""
	for _, o := range fn.Locals {
		names += o.Name + " "
	}
	for _, want := range []string{"x", "y", "z", "w"} {
		if !strings.Contains(names, want) {
			t.Errorf("the locals are %q, want one named %s", names, want)
		}
	}
	if strings.Contains(names, "k") {
		t.Errorf("a constant took a frame slot: %q", names)
	}
	// A variable with no initialiser emits no statement: a frame is zeroed.
	if got := len(fn.Body); got != 3 {
		t.Errorf("%d statements, want the two initialisations and the return:\n%s",
			got, buildDump(fn))
	}
}

// TestBuildToleratesIncompleteInfo checks the rule that every path either
// builds a node or reports an error, and that none of them returns nil.
//
// The input is a checked package whose recorded types and selections have been
// taken away, which is what a bug in an earlier pass looks like from here. A
// builder that dereferenced what it found would crash on it, and a crash in a
// compiler is the diagnostic nobody can act on.
func TestBuildToleratesIncompleteInfo(t *testing.T) {
	pkg, files, info := buildTypecheck(t, buildPrelude+`
func f(t T, i I, m map[string]int, c chan int) int {
	x := 1
	x += 2
	x++
	t.A = x
	sink(t.M(), i.M(), m["a"], <-c, T{A: 1}, &t, t.S[0], t.S[1:], []int{1}[0])
	for k := range m {
		delete(m, k)
	}
	switch v := any(i).(type) {
	case int:
		sink(v)
	}
	return x
}
`)
	stripped := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       info.Defs,
		Uses:       info.Uses,
		Implicits:  info.Implicits,
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     info.Scopes,
		InitOrder:  info.InitOrder,
	}
	var out *Package
	var err error
	func() {
		defer func() {
			if e := recover(); e != nil {
				t.Fatalf("the builder crashed on an incomplete Info: %v\n%s", e, debug.Stack())
			}
		}()
		out, err = Build(pkg, files, stripped)
	}()
	if err == nil {
		t.Error("an incomplete Info built without an error")
	}
	if out == nil {
		t.Fatal("an incomplete Info produced no package")
	}
	for _, fn := range out.Funcs {
		buildCheckNoNil(t, fn)
	}
}

// buildCheckNoNil asserts that every node of a function exists and has an
// operation.
func buildCheckNoNil(t *testing.T, fn *Func) {
	t.Helper()
	for _, s := range fn.Body {
		if s == nil {
			t.Fatalf("%s has a nil statement", fn.Sym)
		}
		Walk(s, func(n *Node) bool {
			if n.Op == OpInvalid {
				t.Errorf("%s has a node with no operation", fn.Sym)
			}
			for _, a := range n.Args {
				// A bound of a slice expression is nil when the source left
				// it out. Everywhere else a nil operand is a hole.
				if a == nil && n.Op != OSlice {
					t.Errorf("%s has a nil operand under %s", fn.Sym, n.Op)
				}
			}
			return true
		})
	}
}

// TestBuildImplicitConversionRules covers the conversion helper directly for
// the untyped cases, which the checker usually resolves before the builder
// sees them and which must still be right when it does not.
func TestBuildImplicitConversionRules(t *testing.T) {
	pkg, _, info := buildTypecheck(t, `package p

type I interface{ M() int }

var i I
var f64 float64
var p8 *int
`)
	b := &builder{
		conv: NewConverter(), info: info, tpkg: pkg,
		objs: make(map[types2.Object]*Object), owner: make(map[*Object]*Func),
		ptrs: make(map[types2.Type]*Type), out: &Package{Path: "p"},
	}
	iface := pkg.Scope().Lookup("i").Type()
	f64 := pkg.Scope().Lookup("f64").Type()
	ptr := pkg.Scope().Lookup("p8").Type()
	untypedInt := types2.Type(types2.Typ[types2.UntypedInt])
	untypedNil := types2.Type(types2.Typ[types2.UntypedNil])

	// An untyped constant takes the type of its destination, with no
	// conversion node: a constant has no representation until then.
	n := b.constNode(0, constant.MakeInt64(1), untypedInt)
	got := b.assignConv(n, untypedInt, f64)
	if got != n || got.Type.Kind != Float64 {
		t.Errorf("an untyped constant assigned to a float is %s %s", got.Op, got.Type)
	}
	// Assigned to an interface it takes its default type first, and the
	// conversion to the interface is a node.
	n = b.constNode(0, constant.MakeInt64(1), untypedInt)
	got = b.assignConv(n, untypedInt, iface)
	if got.Op != OConvert || got.Type.Kind != Interface || got.X.Type.Kind != Int64 {
		t.Errorf("an untyped constant assigned to an interface is %s of %s", got.Op, got.X.Type)
	}
	// nil is not converted. It is the zero value of its destination.
	n = &Node{Op: OConst, Type: voidType, Val: Const{}}
	got = b.assignConv(n, untypedNil, ptr)
	if got != n || got.Type.Kind != Ptr {
		t.Errorf("nil assigned to a pointer is %s %s", got.Op, got.Type)
	}
	// Nothing to convert to, or from, is nothing to do.
	if b.assignConv(nil, untypedInt, f64) != nil {
		t.Error("a nil node converted")
	}
	if got := b.assignConv(n, untypedNil, nil); got != n {
		t.Error("a conversion to no type made a node")
	}
	if got := b.assignConv(n, nil, f64); got != n {
		t.Error("a conversion from no type made a node")
	}

	// The two operands of a binary operation: the untyped one takes the type
	// of the other, whichever side it is on.
	l := b.constNode(0, constant.MakeInt64(1), untypedInt)
	r := &Node{Op: OLocal, Type: b.irType(f64)}
	l2, r2 := b.balance(l, r, untypedInt, f64)
	if l2.Type.Kind != Float64 || r2 != r {
		t.Errorf("the operands balanced to %s and %s", l2.Type, r2.Type)
	}
	l3, r3 := b.balance(r, l, f64, untypedInt)
	if r3.Type.Kind != Float64 || l3 != r {
		t.Errorf("the operands balanced to %s and %s", l3.Type, r3.Type)
	}
	// A concrete value compared against an interface is converted to it.
	c := &Node{Op: OLocal, Type: b.irType(f64)}
	iv := &Node{Op: OLocal, Type: b.irType(iface)}
	l4, r4 := b.balance(c, iv, f64, iface)
	if l4.Op != OConvert || r4 != iv {
		t.Errorf("the concrete operand is %s", l4.Op)
	}
	l5, r5 := b.balance(iv, c, iface, f64)
	if r5.Op != OConvert || l5 != iv {
		t.Errorf("the concrete operand is %s", r5.Op)
	}
	if a, bb := b.balance(l, r, nil, nil); a != l || bb != r {
		t.Error("operands with no types were changed")
	}
}

// TestBuildDeferAndGoSaveTheirOperands pins the rule of the specification that
// the operands of go and defer are evaluated when the statement runs, not when
// the call runs.
//
// The bug this test was written for saved only what a temporary already held:
// a variable named at the statement was left as a name, so a later assignment
// to it changed what the call would see. It is invisible in a program that
// does not assign afterwards, which is most of them, and it is a wrong answer
// in the one that does.
//
// The temporaries then become the captures of the literal the call is wrapped
// in, because runtime.deferproc takes one word and calls it with nothing
// (specs/033-closures-defer-panic.md). Each temporary is written once, so the
// capture by reference the closure builds and a capture by value are the same
// value, and the rule above survives the wrapping.
func TestBuildDeferAndGoSaveTheirOperands(t *testing.T) {
	p := buildSource(t, `
func f(x int, g func(int)) {
	defer g(x)
	go g(x)
	x = 2
	g = nil
}
`)
	fn := buildFuncOf(t, p, "f")
	got := buildLines(fn)
	want := []string{
		".autotmp_0 = g",
		".autotmp_1 = x",
		"defer(closure(.autotmp_0, .autotmp_1)())",
		".autotmp_2 = g",
		".autotmp_3 = x",
		"go(closure(.autotmp_2, .autotmp_3)())",
		"x = 2",
		"g = nil",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s\n\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"), buildDump(fn))
	}

	// The literals the two statements were wrapped in are compiled, marked as
	// wrappers, and capture what the statement saved. The mark is not
	// cosmetic: runtime.gorecover counts the frames between itself and
	// runtime.gopanic and skips the ones marked, so an unmarked wrapper turns
	// "defer f(x)" where f recovers into a program that does not recover.
	wrappers := 0
	for _, w := range p.Funcs {
		if !w.Wrapper {
			continue
		}
		wrappers++
		if len(w.Captures) != 2 {
			t.Errorf("%s captures %d objects, want the callee and the operand", w.Name, len(w.Captures))
		}
		if w.Closure == nil {
			t.Errorf("%s captures and has no context parameter", w.Name)
		}
	}
	if wrappers != 2 {
		t.Errorf("%d wrapper literals, want one for the defer and one for the go", wrappers)
	}

	// A known function symbol cannot be reassigned, so it needs no temporary,
	// and a constant is already a value. The call still has an operand, so it
	// is still wrapped.
	p = buildSource(t, `func f() { defer sink(1) }`)
	fn = buildFuncOf(t, p, "f")
	d := buildFirst(t, fn, ODefer)
	if d.X.X.Op != OClosure {
		t.Fatalf("the deferred call is not wrapped: %s", buildStr(d.X.X))
	}
	inner := buildFuncOf(t, p, "f.func0")
	if len(inner.Body) != 1 || inner.Body[0].X == nil ||
		inner.Body[0].X.Op != OGlobal || inner.Body[0].X.Obj.Name != "p.sink" {
		t.Errorf("the wrapped callee is not p.sink:\n%s", buildDump(inner))
	}

	// A call with no operand needs no literal and gets none. Wrapping it
	// would put a frame between the deferred call and runtime.gopanic.
	p = buildSource(t, `func f() { defer one() }`)
	fn = buildFuncOf(t, p, "f")
	d = buildFirst(t, fn, ODefer)
	if d.X.X.Op != OGlobal || d.X.X.Obj.Name != "p.one" {
		t.Errorf("a deferred call with no operand was wrapped: %s", buildStr(d.X.X))
	}

	// The receiver of a deferred method call is saved too, both for a
	// concrete receiver and for one inside an interface selection.
	p = buildSource(t, `func f(t T, i I) { defer t.M(); defer i.M() }`)
	fn = buildFuncOf(t, p, "f")
	defers := buildFind(fn, ODefer)
	if len(defers) != 2 {
		t.Fatalf("%d defers", len(defers))
	}
	// A concrete receiver is the call's first operand, so the call is wrapped
	// and the temporary is the literal's capture.
	if recv := defers[0].X.X.Args[0]; recv.Op != OLocal || !strings.HasPrefix(recv.Obj.Name, ".autotmp_") {
		t.Errorf("the concrete receiver is %s, want a temporary:\n%s", buildStr(recv), buildDump(fn))
	}
	// An interface receiver stays inside the selection, so the call takes no
	// operand and is not wrapped.
	if recv := defers[1].X.X.X; recv.Op != OLocal || !strings.HasPrefix(recv.Obj.Name, ".autotmp_") {
		t.Errorf("the interface receiver is %s, want a temporary:\n%s", buildStr(recv), buildDump(fn))
	}
}

// TestBuildAssignmentAndClauseOps checks the node set's own operations, which
// replaced the conventions this builder used before they existed.
func TestBuildAssignmentAndClauseOps(t *testing.T) {
	p := buildSource(t, `
func f(x int, c chan int) {
	y := 1
	var z = 2
	x = 3
	switch x {
	case 1:
	default:
	}
	select {
	case <-c:
	default:
	}
	for i := 0; i < 1; i++ {
	}
	sink(y, z)
}
`)
	fn := buildFuncOf(t, p, "f")
	var defines, plains int
	for _, s := range fn.Body {
		if s.Op != OAssign {
			continue
		}
		if s.Op1 == syntax.Def {
			defines++
		} else {
			plains++
		}
	}
	// y := 1 and var z = 2 declare; x = 3 does not.
	if defines != 2 || plains != 1 {
		t.Errorf("%d declaring and %d plain assignments:\n%s", defines, plains, buildDump(fn))
	}
	sw := buildFirst(t, fn, OSwitch)
	for i, c := range sw.Body {
		if c.Op != OCase {
			t.Errorf("switch clause %d is a %s", i, c.Op)
		}
	}
	if len(sw.Body[1].Args) != 0 {
		t.Error("the default clause carries case expressions")
	}
	sel := buildFirst(t, fn, OSelect)
	for i, c := range sel.Body {
		if c.Op != OCase {
			t.Errorf("select clause %d is a %s", i, c.Op)
		}
	}
	loop := buildFirst(t, fn, OFor)
	if len(loop.Post) != 1 || len(loop.Else) != 0 {
		t.Errorf("the post statement is in %v and Else holds %v", loop.Post, loop.Else)
	}
	// An OBinary is always an operation now, in a statement list or not.
	for _, n := range buildFind(fn, OBinary) {
		if n.Op1 == 0 {
			t.Errorf("an OBinary has no operator:\n%s", buildDump(fn))
		}
	}
}

// TestConstReadsAsANumber checks the ConstValue side of a constant. The
// constant folding of specs/022-optimization-passes.md cannot fold what it
// cannot read, and a reader that rounds quietly makes a folded constant
// differ from an unfolded one.
func TestConstReadsAsANumber(t *testing.T) {
	var v Value = Const{Val: constant.MakeInt64(42)}
	c, ok := v.(ConstValue)
	if !ok {
		t.Fatal("a constant of the IR cannot be read as a number")
	}
	if i, ok := c.Int64(); i != 42 || !ok {
		t.Errorf("42 reads as %d, %v", i, ok)
	}
	if u, ok := c.Uint64(); u != 42 || !ok {
		t.Errorf("42 reads as %d, %v", u, ok)
	}
	if f, ok := c.Float64(); f != 42 || !ok {
		t.Errorf("42 reads as %f, %v", f, ok)
	}
	if c.IsZero() {
		t.Error("42 is zero")
	}

	for _, tc := range []struct {
		what string
		val  constant.Value
		i64  bool    // Int64 is exact
		u64  bool    // Uint64 is exact
		f64  float64 // the value Float64 returns
		f    bool    // Float64 is exact
		zero bool
	}{
		{what: "a negative integer", val: constant.MakeInt64(-1), i64: true, f64: -1, f: true},
		{what: "zero", val: constant.MakeInt64(0), i64: true, u64: true, f: true, zero: true},
		{
			what: "an integer wider than 64 bits",
			val:  constant.Shift(constant.MakeInt64(1), token.SHL, 70),
			f64:  1 << 70, f: true,
		},
		{what: "a float", val: constant.MakeFloat64(1.5), f64: 1.5, f: true},
		{
			// The checker's constants are arbitrary precision. A reader that
			// rounded quietly would make a folded constant differ from an
			// unfolded one.
			what: "a rational that no float64 holds exactly",
			val:  constant.BinaryOp(constant.MakeInt64(1), token.QUO, constant.MakeInt64(3)),
			f64:  1.0 / 3.0,
		},
		{what: "a string", val: constant.MakeString("x")},
		{what: "the empty string", val: constant.MakeString(""), zero: true},
		{what: "false", val: constant.MakeBool(false), zero: true},
		{what: "true", val: constant.MakeBool(true)},
		{what: "an unknown constant", val: constant.MakeUnknown()},
	} {
		t.Run(tc.what, func(t *testing.T) {
			c := Const{Val: tc.val}
			if _, ok := c.Int64(); ok != tc.i64 {
				t.Errorf("Int64 is exact=%v, want %v", ok, tc.i64)
			}
			if _, ok := c.Uint64(); ok != tc.u64 {
				t.Errorf("Uint64 is exact=%v, want %v", ok, tc.u64)
			}
			f, exact := c.Float64()
			if f != tc.f64 || exact != tc.f {
				t.Errorf("Float64 is %v, %v; want %v, %v", f, exact, tc.f64, tc.f)
			}
			if c.IsZero() != tc.zero {
				t.Errorf("IsZero is %v, want %v", c.IsZero(), tc.zero)
			}
		})
	}

	// The constant with no value is nil, which is the zero of every type it
	// can be written for and is not a number.
	n := Const{}
	if !n.IsZero() || n.String() != "nil" {
		t.Errorf("nil reads as %s, zero=%v", n.String(), n.IsZero())
	}
	if _, ok := n.Int64(); ok {
		t.Error("nil reads as an integer")
	}
	if _, ok := n.Uint64(); ok {
		t.Error("nil reads as an unsigned integer")
	}
	if _, ok := n.Float64(); ok {
		t.Error("nil reads as a float")
	}

	// A complex constant is not a single number, and its zero is still
	// recognisable.
	im := constant.MakeImag(constant.MakeInt64(1))
	if (Const{Val: im}).IsZero() {
		t.Error("1i is zero")
	}
	if !(Const{Val: constant.MakeImag(constant.MakeInt64(0))}).IsZero() {
		t.Error("0i is not zero")
	}
	if _, ok := (Const{Val: im}).Float64(); ok {
		t.Error("1i reads as a float")
	}
}

// buildRunWithGc compiles and runs a program with the toolchain that built
// this test, and returns what it printed.
//
// It is the oracle for a rule about what a program means, which is a question
// no assertion about the shape of a tree can answer on its own.
func buildRunWithGc(t *testing.T, src string) string {
	t.Helper()
	gc, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to compare against")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "x.go")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command(gc, "run", file)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go run: %v", err)
	}
	return string(out)
}

// TestBuildPerIterationLoopVariable is the Go 1.22 loop variable, checked
// against what the reference implementation does with the same program.
//
// A variable declared by a for clause or a range clause is a new variable in
// every iteration. The difference is invisible until something captures it or
// takes its address, and then it is the difference between 0 1 2 and 3 3 3.
// That is a fact about the declaration, so it is settled here, where
// declarations become objects: a consumer of the IR is given two objects and
// cannot invent the second one.
func TestBuildPerIterationLoopVariable(t *testing.T) {
	const src = `package main

import "fmt"

func main() {
	var byFor []func() int
	for i := 0; i < 3; i++ {
		byFor = append(byFor, func() int { return i })
	}
	var byRange []func() int
	for _, v := range []int{10, 20, 30} {
		byRange = append(byRange, func() int { return v })
	}
	var addrFor []*int
	for k := 0; k < 3; k++ {
		addrFor = append(addrFor, &k)
	}
	var addrRange []*int
	for m := range 3 {
		addrRange = append(addrRange, &m)
	}
	for _, f := range byFor {
		fmt.Print(f(), " ")
	}
	for _, f := range byRange {
		fmt.Print(f(), " ")
	}
	for _, q := range addrFor {
		fmt.Print(*q, " ")
	}
	for _, q := range addrRange {
		fmt.Print(*q, " ")
	}
	fmt.Println()
}
`
	printed := strings.Fields(buildRunWithGc(t, src))
	if len(printed) != 12 {
		t.Fatalf("the reference implementation printed %q", printed)
	}
	// Every one of the four loops must hand out a different variable, whether
	// the body captured it or took its address.
	for _, at := range []int{0, 3, 6, 9} {
		if printed[at] == printed[at+1] {
			t.Fatalf("the reference implementation printed %q, which is one variable per loop: "+
				"the premise of this test is gone and the builder must follow it", printed)
		}
	}

	pkg, files, info := buildTypecheckWithImports(t, src)
	p, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	fn := buildFuncOf(t, p, "main")

	for _, tc := range []struct {
		name string
		op   Op
	}{
		{"i", OFor},   // captured by a closure, declared by a for clause
		{"v", ORange}, // captured by a closure, declared by a range clause
		{"k", OFor},   // its address taken, declared by a for clause
		{"m", ORange}, // its address taken, ranging over an integer
	} {
		loop, per, carrier := buildLoopDeclaring(t, fn, tc.name)
		if loop.Op != tc.op {
			t.Errorf("%s is declared by a %s, want a %s", tc.name, loop.Op, tc.op)
		}
		if per == carrier {
			t.Fatalf("%s is one variable for every iteration:\n%s", tc.name, buildDump(fn))
		}
		// What the body handed out is the per-iteration variable, and it is
		// the one the body declared.
		held := buildHeldBy(t, loop)
		if held != per {
			t.Errorf("the body of the %s loop handed out %s, want %s", tc.name, held.Name, per.Name)
		}
		if loop.Op == ORange {
			// A range statement needs no copy back: the next iteration takes
			// its value from the range expression.
			continue
		}
		// The loop control works on the carrier. Nothing outside the body may
		// write the variable an iteration handed out.
		if o := loop.Init[0].X.Obj; o != carrier {
			t.Errorf("the init statement of the %s loop declares %s, want the carrier", tc.name, o.Name)
		}
		buildAssertNoRef(t, loop.X, per, "the condition")
		back := loop.Post[0]
		if !IsAssign(back) || back.X.Obj != carrier || back.Y.Obj != per {
			t.Fatalf("the post list of the %s loop does not open by carrying the value forward:\n%s",
				tc.name, buildDump(fn))
		}
		// The copy back is the first post statement and not the last body
		// statement, because continue reaches the post list and does not
		// reach the end of the body.
		for _, st := range loop.Post[1:] {
			buildAssertNoRef(t, st, per, "the post statement")
		}
	}

	// A loop variable nothing captures and nothing addresses keeps one
	// object: one instance and many are indistinguishable without an address,
	// and the copy would be work for a difference nobody can observe.
	q := buildSource(t, `func f() { for i := 0; i < 3; i++ { sink(i) }; for _, v := range []int{1} { sink(v) } }`)
	plain := buildFuncOf(t, q, "f")
	for _, st := range buildFind(plain, OFor)[0].Body {
		if IsAssign(st) && st.Op1 == syntax.Def {
			t.Errorf("an uncaptured loop variable was copied per iteration:\n%s", buildDump(plain))
		}
	}
	for _, o := range plain.Locals {
		if strings.HasPrefix(o.Name, ".loopvar_") {
			t.Errorf("an uncaptured loop variable got a carrier:\n%s", buildDump(plain))
		}
	}
}

// buildLoopDeclaring returns the loop whose body opens by declaring the named
// variable, that variable, and the carrier it is declared from.
func buildLoopDeclaring(t *testing.T, fn *Func, name string) (loop *Node, per, carrier *Object) {
	t.Helper()
	for _, s := range fn.Body {
		Walk(s, func(n *Node) bool {
			if loop != nil {
				return false
			}
			if n.Op != OFor && n.Op != ORange {
				return true
			}
			if len(n.Body) == 0 {
				return true
			}
			d := n.Body[0]
			if IsAssign(d) && d.Op1 == syntax.Def && d.X != nil && d.X.Obj != nil &&
				d.X.Obj.Name == name && d.Y != nil && d.Y.Obj != nil {
				loop, per, carrier = n, d.X.Obj, d.Y.Obj
				return false
			}
			return true
		})
	}
	if loop == nil {
		t.Fatalf("no loop body declares %s:\n%s", name, buildDump(fn))
	}
	return loop, per, carrier
}

// buildHeldBy returns the object a loop body hands out, either to a closure or
// through its address.
func buildHeldBy(t *testing.T, loop *Node) *Object {
	t.Helper()
	var found *Object
	for _, s := range loop.Body {
		Walk(s, func(n *Node) bool {
			if found != nil {
				return false
			}
			switch {
			case n.Op == OClosure && n.Index == closureLiteral && len(n.Args) == 1:
				found = n.Args[0].Obj
			case n.Op == OAddr && n.X != nil && n.X.Op == OLocal:
				found = n.X.Obj
			default:
				return true
			}
			return false
		})
	}
	if found == nil {
		t.Fatal("the loop body neither captures a variable nor takes an address")
	}
	return found
}

// buildAssertNoRef fails if a node refers to an object.
func buildAssertNoRef(t *testing.T, n *Node, o *Object, what string) {
	t.Helper()
	Walk(n, func(m *Node) bool {
		if m.Obj == o {
			t.Errorf("%s refers to %s, which belongs to one iteration", what, o.Name)
		}
		return true
	})
}

// TestBuildNewOfAnExpressionKeepsItsOperand pins the difference Go 1.26 added.
//
// new(T) allocates a zero value and new(expr) allocates and stores the value
// the expression produced. The builder read the result type and emitted ONew
// from that alone, which turned the second into the first: a pointer to a zero
// where the program asked for a pointer to 123. Nothing said so, because both
// forms produce a pointer of the right type and the corpus is what caught it,
// in Go's own test/newexpr.go.
// TestBuildRangeDestinationIsEvaluatedEachIteration is the one destination in
// this file that is not stabilized.
//
// A range clause with no := writes its destination once per iteration, and the
// specification evaluates an index expression's operands with the assignment
// that reads it. So "for a[one()] = range [2]int{}" calls one twice, and a
// temporary holding the index in front of the loop calls it once. The failure
// is silent: the program runs and the loop writes the same element every time.
//
// The temporaries stay on the destination's own Init, which runs immediately
// before the destination, and the lowering pass writes the destination inside
// the loop body.
func TestBuildRangeDestinationIsEvaluatedEachIteration(t *testing.T) {
	fn := buildFuncOf(t, buildSource(t, `
func f(a *[2]int) {
	for a[one()] = range [2]int{} {
	}
}
`), "f")

	var loop *Node
	for _, s := range fn.Body {
		if s != nil && s.Op == ORange {
			loop = s
		}
	}
	if loop == nil {
		t.Fatalf("no range statement:\n%s", buildDump(fn))
	}
	if len(loop.Args) != 1 || loop.Args[0] == nil {
		t.Fatalf("the range clause has %d destinations:\n%s", len(loop.Args), buildDump(fn))
	}
	calls := func(list []Stmt) bool {
		found := false
		for _, s := range list {
			Walk(s, func(n *Node) bool {
				if n.Op == OCall && n.X != nil && n.X.Op == OGlobal &&
					n.X.Obj != nil && n.X.Obj.Name == "p.one" {
					found = true
				}
				return true
			})
		}
		return found
	}
	if !calls(loop.Args[0].Init) {
		t.Errorf("the index is not evaluated with the destination:\n%s", buildDump(fn))
	}
	// And it is nowhere else: a copy in front of the loop is the evaluation
	// this test exists to catch.
	for _, s := range fn.Body {
		if s == loop {
			break
		}
		if calls([]Stmt{s}) {
			t.Errorf("the index is evaluated in front of the loop:\n%s", buildDump(fn))
		}
	}
}

func TestBuildNewOfAnExpressionKeepsItsOperand(t *testing.T) {
	for _, tc := range []struct {
		what string
		body string
		want bool
	}{
		{"new of a type", `func f() *int { return new(int) }`, false},
		{"new of an untyped constant", `func f() *int { return new(123) }`, true},
		{"new of a variable", `func f(x int) *int { return new(x) }`, true},
		{"new of an expression", `func f(x int) *int { return new(x + 1) }`, true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := buildSource(t, tc.body)
			fn := buildFuncOf(t, p, "f")
			var found, carries bool
			for _, st := range fn.Body {
				Walk(st, func(n *Node) bool {
					if n.Op == ONew {
						found = true
						carries = n.X != nil
					}
					return true
				})
			}
			if !found {
				t.Fatalf("no new node was built:\n%s", buildDump(fn))
			}
			if carries != tc.want {
				t.Errorf("the new node carries an operand=%v, want %v:\n%s",
					carries, tc.want, buildDump(fn))
			}
		})
	}
}
