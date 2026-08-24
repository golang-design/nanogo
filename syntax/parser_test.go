// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"fmt"
	goparser "go/parser"
	goscanner "go/scanner"
	gotoken "go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test helpers

// parseString parses src and returns the tree and every error, in the order
// they were reported.
func parseString(t *testing.T, src string) (*File, *FileSet, []Error) {
	t.Helper()
	fset := NewFileSet()
	f := fset.AddFile("test.go", len(src))
	var errs []Error
	file, _ := Parse(f, []byte(src), func(err Error) { errs = append(errs, err) }, nil, 0)
	return file, fset, errs
}

// mustParse parses src and fails the test if there is any error.
func mustParse(t *testing.T, src string) (*File, *FileSet) {
	t.Helper()
	file, fset, errs := parseString(t, src)
	for _, err := range errs {
		t.Errorf("unexpected error at %s: %s", fset.Position(err.Pos), err.Msg)
	}
	if file == nil {
		t.Fatal("Parse returned no file")
	}
	return file, fset
}

// oneLevel visits the direct children of root and nothing below them.
//
// The traversal is the package's own Walk, so the test agrees with the walker
// the compiler uses rather than with a second copy of the child order.
type oneLevel struct {
	root Node
	out  *[]Node
}

func (o oneLevel) Visit(n Node) Visitor {
	if n == o.root {
		return o // descend into the root, once
	}
	*o.out = append(*o.out, n)
	return nil // and no further
}

// children returns the direct children of n in source order.
func children(n Node) []Node {
	var out []Node
	Walk(n, oneLevel{root: n, out: &out})
	return out
}

// nodeKind is the node's type name without the package qualifier.
func nodeKind(n Node) string {
	s := fmt.Sprintf("%T", n)
	return s[strings.LastIndexByte(s, '.')+1:]
}

// sketch renders the shape of a subtree as "Parent(Child,Child(Grandchild))".
//
// Shape is what the two ambiguities of specs/011 are decided on, so the tests
// for them assert the shape rather than the printed source: an ArrayType and an
// IndexExpr can print the same and mean different things.
func sketch(n Node) string {
	kids := children(n)
	if len(kids) == 0 {
		return nodeKind(n)
	}
	parts := make([]string, len(kids))
	for i, k := range kids {
		parts[i] = sketch(k)
	}
	return nodeKind(n) + "(" + strings.Join(parts, ",") + ")"
}

// ----------------------------------------------------------------------------
// The two ambiguities

func TestArrayAgainstTypeParameterList(t *testing.T) {
	// specs/011 ambiguity 1. After "type A [", the "[" begins either an array
	// or slice type or a type parameter list, and the prefix "[N" is common.
	tests := []struct {
		src     string
		sketch  string
		generic bool
	}{
		// An expression followed by "]" is a length: the specification reads
		// "type B [N] int" as an array even when N is a single name.
		{"type A [N]int", "TypeDecl(Name,ArrayType(Name,Name))", false},
		{"type A [4]int", "TypeDecl(Name,ArrayType(BasicLit,Name))", false},
		{"type A [N + 1]int", "TypeDecl(Name,ArrayType(Operation(Name,BasicLit),Name))", false},
		{"type A [...]int", "TypeDecl(Name,ArrayType(Name))", false},
		{"type A []int", "TypeDecl(Name,SliceType(Name))", false},

		// A name followed by a constraint is a type parameter list.
		{"type B [N any] struct{}", "TypeDecl(Name,Field(Name,Name),StructType)", true},
		{"type B [N, M any] struct{}", "TypeDecl(Name,Field(Name,Name),Field(Name,Name),StructType)", true},
		{"type B [N interface{ ~int }] struct{}", "TypeDecl(Name,Field(Name,InterfaceType(Field(Operation(Name)))),StructType)", true},
		// A constraint may begin with "[", which is why the parser looks at the
		// token after the name before it parses an expression.
		{"type B [P []E] struct{}", "TypeDecl(Name,Field(Name,SliceType(Name)),StructType)", true},
		{"type B [P *[]E] struct{}", "TypeDecl(Name,Field(Name,Operation(SliceType(Name))),StructType)", true},
		// "P *E" alone is an array length: *E could be a value expression, so
		// nothing tilts the decision and the specification reads it as a length.
		{"type B [P *E]struct{}", "TypeDecl(Name,ArrayType(Operation(Name,Name),StructType))", false},
		// A comma forces the type parameter reading: an array has one length.
		{"type B [P E, Q F] struct{}", "TypeDecl(Name,Field(Name,Name),Field(Name,Name),StructType)", true},
		{"type B [P ~int | ~string] struct{}", "TypeDecl(Name,Field(Name,Operation(Operation(Name),Operation(Name))),StructType)", true},
	}

	for _, test := range tests {
		t.Run(test.src, func(t *testing.T) {
			file, _ := mustParse(t, "package p\n"+test.src+"\n")
			d, ok := file.DeclList[0].(*TypeDecl)
			if !ok {
				t.Fatalf("got %T, want *TypeDecl", file.DeclList[0])
			}
			if got := sketch(d); got != test.sketch {
				t.Errorf("shape:\n got %s\nwant %s", got, test.sketch)
			}
			// The branch the parser took is recorded by TParamList, which the
			// type checker reads to decide whether the declaration is generic.
			if generic := d.TParamList != nil; generic != test.generic {
				t.Errorf("TParamList != nil = %v, want %v", generic, test.generic)
			}
		})
	}
}

func TestArrayLengthOfAConstantIsStillAnArray(t *testing.T) {
	// The parser cannot know that N is a constant, and it does not need to:
	// "type C [N]int" must produce the same tree whether N is declared or not,
	// because the decision is made on syntax alone.
	withConst, _ := mustParse(t, "package p\nconst N = 4\ntype C [N]int\n")
	without, _ := mustParse(t, "package p\ntype C [N]int\n")

	got := sketch(withConst.DeclList[1])
	want := sketch(without.DeclList[0])
	if got != want {
		t.Errorf("a declared length changed the tree:\n got %s\nwant %s", got, want)
	}
	if want != "TypeDecl(Name,ArrayType(Name,Name))" {
		t.Errorf("shape = %s, want an ArrayType", want)
	}
}

func TestIndexAgainstInstantiation(t *testing.T) {
	// specs/011 ambiguity 2. The parser does not resolve it. It produces an
	// IndexExpr and the type checker rewrites the node once it knows what the
	// operand is.
	tests := []struct {
		src    string
		sketch string
		check  func(*testing.T, Expr)
	}{
		{"f[int](x)", "CallExpr(IndexExpr(Name,Name),Name)", func(t *testing.T, x Expr) {
			call := x.(*CallExpr)
			idx := call.Fun.(*IndexExpr)
			if _, isList := idx.Index.(*ListExpr); isList {
				t.Error("one operand became a ListExpr")
			}
		}},
		{"f[int, string](x)", "CallExpr(IndexExpr(Name,ListExpr(Name,Name)),Name)", func(t *testing.T, x Expr) {
			idx := x.(*CallExpr).Fun.(*IndexExpr)
			l, ok := idx.Index.(*ListExpr)
			if !ok {
				t.Fatalf("Index is %T, want *ListExpr", idx.Index)
			}
			// More than one operand can only be an instantiation, because
			// indexing takes one operand. That is the whole of what the parser
			// records here.
			if len(l.ElemList) != 2 {
				t.Errorf("len(ElemList) = %d, want 2", len(l.ElemList))
			}
		}},
		{"a[i]", "IndexExpr(Name,Name)", func(t *testing.T, x Expr) {
			if _, ok := x.(*IndexExpr); !ok {
				t.Fatalf("got %T, want *IndexExpr", x)
			}
		}},
		{"a[i:j]", "SliceExpr(Name,Name,Name)", func(t *testing.T, x Expr) {
			s := x.(*SliceExpr)
			if s.Full {
				t.Error("Full is set for a two-index slice")
			}
			if s.Index[2] != nil {
				t.Error("Index[2] is set for a two-index slice")
			}
		}},
		{"a[i:j:k]", "SliceExpr(Name,Name,Name,Name)", func(t *testing.T, x Expr) {
			s := x.(*SliceExpr)
			if !s.Full {
				t.Error("Full is not set for a three-index slice")
			}
		}},
		{"a[:]", "SliceExpr(Name)", func(t *testing.T, x Expr) {
			s := x.(*SliceExpr)
			if s.Index[0] != nil || s.Index[1] != nil {
				t.Error("an empty slice expression has an index")
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.src, func(t *testing.T) {
			file, _ := mustParse(t, "package p\nfunc _() { _ = "+test.src+" }\n")
			body := file.DeclList[0].(*FuncDecl).Body
			x := body.List[0].(*AssignStmt).Rhs
			if got := sketch(x); got != test.sketch {
				t.Errorf("shape:\n got %s\nwant %s", got, test.sketch)
			}
			test.check(t, x)
		})
	}
}

func TestCompositeLiteralInAControlClauseHeader(t *testing.T) {
	// The specification forbids a bare composite literal as the operand of an
	// if, for or switch header. A parenthesis or a bracket ends the header's
	// reach, which is what makes this a context flag and not lookahead.
	valid := []string{
		"if x == (T{}) {}",
		"if (T{}) == x {}",
		"for _ = range []int{1} {}",
		"switch (T{}) {}",
		"if f(T{}) {}",
		"if x[T{}] == 0 {}",
		"for i := 0; i < len([]int{1}); i++ {}",
		"if func() bool { return T{} == T{} }() {}",
	}
	for _, src := range valid {
		t.Run("ok/"+src, func(t *testing.T) {
			mustParse(t, "package p\ntype T struct{}\nfunc _(x T) {\n"+src+"\n}\n")
		})
	}

	invalid := []string{
		"if x == T{} {}",
		"switch x == T{} {}",
		"for x == T{} {}",
	}
	for _, src := range invalid {
		t.Run("bad/"+src, func(t *testing.T) {
			_, _, errs := parseString(t, "package p\ntype T struct{}\nfunc _(x T) {\n"+src+"\n}\n")
			if len(errs) == 0 {
				t.Error("a bare composite literal in the header was accepted")
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Position invariants

func TestPositionInvariants(t *testing.T) {
	for _, src := range positionCorpus {
		t.Run(firstLine(src), func(t *testing.T) {
			file, fset := mustParse(t, src)
			checkPositions(t, fset, file)
		})
	}
}

func TestPositionInvariantsOverTheDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("the distribution walk is slow")
	}
	_, files := corpusFiles(t)
	n := 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fset := NewFileSet()
		f := fset.AddFile(path, len(src))
		file, perr := Parse(f, src, func(Error) {}, nil, 0)
		// A file the parser rejects has Bad nodes in it, and a Bad node stands
		// for source the parser could not measure. Only clean files can carry
		// the invariant.
		if perr != nil || file == nil {
			continue
		}
		n++
		checkPositions(t, fset, file)
		if t.Failed() {
			t.Fatalf("first failure in %s", path)
		}
	}
	if n == 0 {
		t.Fatal("no file in the corpus parsed cleanly")
	}
	t.Logf("checked positions in %d files", n)
}

// checkPositions asserts the two invariants that no output comparison sees.
//
// The invariants are stated over each node and its direct children rather than
// over a flat pre-order sequence of positions. A flat sequence is not monotone
// in a correct tree: "func(a, b int)" gives both parameters the same type node,
// so the type's position is visited twice, once before b and once after.
func checkPositions(t *testing.T, fset *FileSet, root Node) {
	t.Helper()
	Inspect(root, func(n Node) bool {
		at := func(p Pos) string { return fset.Position(p).String() }

		if !n.Pos().IsKnown() {
			t.Errorf("%s has no position", nodeKind(n))
			return false
		}
		lo, hi := StartPos(n), EndPos(n)
		if lo > n.Pos() {
			t.Errorf("%s: start %s is after its own position %s", nodeKind(n), at(lo), at(n.Pos()))
		}

		// Two nodes hold children whose source order is not the order they are
		// walked in, both by construction rather than by accident, so the order
		// check exempts them. Containment still holds for both.
		//
		// A struct's tags are a list parallel to its fields and are walked
		// after them, so a tag sits between two fields in the source. A generic
		// function's signature begins at the "[" of its type parameter list,
		// which is also where the type parameters themselves begin, and the
		// parser needs that position to report a type parameter list that is
		// not allowed.
		ordered := true
		switch n := n.(type) {
		case *StructType:
			ordered = false
		case *FuncDecl:
			ordered = n.TParamList == nil
		}

		prev := NoPos
		for _, c := range children(n) {
			clo, chi := StartPos(c), EndPos(c)
			if clo < lo || chi > hi {
				t.Errorf("%s child %s spans %s..%s, outside its parent's %s..%s",
					nodeKind(n), nodeKind(c), at(clo), at(chi), at(lo), at(hi))
			}
			if ordered && clo < prev {
				t.Errorf("%s child %s starts at %s, before the child before it at %s",
					nodeKind(n), nodeKind(c), at(clo), at(prev))
			}
			prev = clo
		}
		return true
	})
}

// positionCorpus is a small set of files that between them use every node the
// parser builds.
var positionCorpus = []string{
	`package p

import (
	"fmt"
	m "math"
	. "os"
	_ "unsafe"
)

const (
	A = iota
	B
	C int = 3
)

var x, y int = 1, 2

type (
	S struct {
		a, b int    ` + "`json:\"a\"`" + `
		fmt.Stringer
		*m.Float
		c func(int) (int, error)
	}
	I interface {
		M(x int) error
		fmt.Stringer
		~int | ~string
	}
	F  = func(a ...int) (n int, err error)
	M  map[string][]chan<- int
	Ch <-chan struct{}
	P  *[4]byte
	G[T any, U comparable] struct{ t T }
)

func (s *S) M(x int) error { return nil }

func f[T any](v T) (T, bool) { return v, true }

func g() {
	var a []int
	a = append(a, 1)
	b := a[0]
	c := a[1:2]
	d := a[1:2:3]
	e := a[:]
	_, _, _, _, _ = a, b, c, d, e
	s := S{a: 1, b: 2}
	_ = s
	m := map[string]int{"k": 1}
	delete(m, "k")
	go func() { _ = 1 }()
	defer func() { _ = 2 }()
	ch := make(chan int, 1)
	ch <- 1
	<-ch
	select {
	case v := <-ch:
		_ = v
	case ch <- 2:
	default:
	}
	switch v := any(1).(type) {
	case int, string:
		_ = v
	default:
	}
	switch {
	case true:
		fallthrough
	default:
	}
	for i := 0; i < 3; i++ {
		if i == 1 {
			continue
		} else if i == 2 {
			break
		} else {
			_ = i
		}
	}
	for k, v := range m {
		_, _ = k, v
	}
	for {
		break
	}
L:
	for {
		for {
			break L
		}
	}
	i := 0
	i++
	i--
	i += 1
	_ = -i
	_ = ^i
	_ = !true
	_ = &s
	_ = *P(nil)
	_ = f[int]
	_ = g
	goto End
End:
	return
}
`,
	`package p

func _() {
	_ = [...]int{1, 2, 3}
	_ = []struct{ x int }{{1}}
	_ = map[[2]int]bool{{1, 2}: true}
	_ = func(a, b int, c ...string) {}
	_ = interface{ M() }(nil)
	_ = (func())(nil)
	type local struct{ n int }
	var l local
	_ = l.n
}
`,
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// ----------------------------------------------------------------------------
// Agreement with go/parser over the distribution
//
// specs/004-conformance.md level L1: for every file in the distribution, nanogo
// must accept what go/parser accepts, reject what it rejects, and put the first
// error in the same place.

// goRoots returns every distribution the corpus tests read.
//
// The first is the toolchain's own GOROOT, which ships with any Go
// installation and is therefore present on a build runner. A development
// checkout named by NANOGO_TEST_GOROOT is added when it exists, because a
// newer tree carries grammar the released one does not.
func goRoots() []string {
	var roots []string
	if r := toolchainRoot(); r != "" {
		roots = append(roots, r)
	}
	extra := os.Getenv("NANOGO_TEST_GOROOT")
	if extra == "" {
		if home, err := os.UserHomeDir(); err == nil {
			extra = filepath.Join(home, "dev", "go.dev", "go")
		}
	}
	if extra != "" && extra != toolchainRoot() {
		if fi, err := os.Stat(filepath.Join(extra, "src")); err == nil && fi.IsDir() {
			roots = append(roots, extra)
		}
	}
	return roots
}

// toolchainRoot returns the root of the running toolchain's own tree.
//
// runtime.GOROOT reads a value baked in at build time and can be empty, so the
// toolchain is asked directly when it is. The corpus tests must not skip on a
// build runner, which is the whole reason this is not a personal path.
func toolchainRoot() string {
	if r := runtime.GOROOT(); r != "" {
		if fi, err := os.Stat(filepath.Join(r, "src")); err == nil && fi.IsDir() {
			return r
		}
	}
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// requireCorpus reports whether a missing corpus is a failure rather than a
// reason to skip. Continuous integration sets it, because a skipped
// conformance gate and a passing one read the same in a test log.
func requireCorpus() bool { return os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" }

// missingCorpus fails or skips, depending on that.
func missingCorpus(t *testing.T, format string, args ...any) {
	t.Helper()
	if requireCorpus() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// corpusFiles returns every .go file under the src directory of each root,
// paired with the root it came from.
func corpusFiles(t *testing.T) (roots []string, files []string) {
	t.Helper()
	roots = goRoots()
	if len(roots) == 0 {
		missingCorpus(t, "no Go distribution to read")
		return nil, nil
	}
	for _, root := range roots {
		src := filepath.Join(root, "src")
		err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", src, err)
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		missingCorpus(t, "no .go files under %v", roots)
	}
	return roots, files
}

// relativeTo strips whichever root's src directory prefixes path.
func relativeTo(roots []string, path string) string {
	for _, root := range roots {
		prefix := filepath.Join(root, "src") + string(filepath.Separator)
		if strings.HasPrefix(path, prefix) {
			return filepath.ToSlash(strings.TrimPrefix(path, prefix))
		}
	}
	return path
}

// corpusExceptions are the files of the distribution where nanogo and
// go/parser disagree on purpose.
//
// Every one of them is a rule that specs/011 assigns to the type checker and
// that go/parser enforces in the parser instead, or an error that both parsers
// report at a different place. In each case nanogo agrees with
// cmd/compile/internal/syntax, which is the parser the tree is shaped for, and
// the reason is recorded here rather than tolerated silently. The paths are
// relative to the distribution's src directory.
var corpusExceptions = map[string]string{
	// A composite literal with an elided type, "var _ []int = {1, 2, 3}". The
	// reference parser accepts the form and lets the type checker decide;
	// cmd/compile/internal/syntax/testdata/complit.go has no error annotation
	// in it at all, which is that decision written down.
	"cmd/compile/internal/syntax/testdata/complit.go": "elided composite literal type is a typing question",
	"internal/types/testdata/check/compliterals1.go":  "elided composite literal type is a typing question",
	"internal/types/testdata/check/compliterals2.go":  "elided composite literal type is a typing question",
	"internal/types/testdata/check/compliterals3.go":  "elided composite literal type is a typing question",

	// The number of iteration variables a range clause may have. go/parser
	// counts them; the reference defers to the type checker, and the corpus
	// says so: range.go annotates both messages and notes the difference.
	"internal/types/testdata/spec/range.go":           "range clause arity is a typing question",
	"internal/types/testdata/fixedbugs/issue50372.go": "range clause arity is a typing question",

	// "go" and "defer" must be followed by a call. go/parser checks it; the
	// reference only rejects a parenthesized operand, which nanogo reports at
	// the same place the reference does.
	"internal/types/testdata/check/stmt0.go": "go statement operand kind is a typing question",

	// A method with no receiver. The reference parser reports it, go/parser
	// accepts and leaves it to go/types. decls2a.go annotates the reference's
	// message at exactly the position nanogo reports.
	"go/doc/testdata/issue17788.go":                   "empty receiver list is reported by the parser, as the reference does",
	"internal/types/testdata/check/decls2/decls2a.go": "empty receiver list is reported by the parser, as the reference does",

	// Both parsers reject; they name a different token. In each case nanogo's
	// position is the one the reference's own error annotation marks.
	"cmd/compile/internal/syntax/testdata/issue23385.go": "position of \"cannot use assignment as value\" is at the =",
	"cmd/compile/internal/syntax/testdata/issue60599.go": "position of \"cannot use assignment as value\" is at the =",
	"cmd/compile/internal/syntax/testdata/issue46558.go": "a misplaced case is reported at the { that follows it",
	"cmd/compile/internal/syntax/testdata/issue48382.go": "type parameters on a function type are reported at the [",
	"internal/types/testdata/check/expr3.go":             "a missing middle index is reported at the second :",
}

func TestCorpusAgreesWithGoParser(t *testing.T) {
	if testing.Short() {
		t.Skip("the distribution corpus is slow")
	}
	roots, files := corpusFiles(t)

	var disagreements []string
	seen := make(map[string]bool)    // exceptions that fired
	present := make(map[string]bool) // files the roots actually hold
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		rel := relativeTo(roots, path)
		present[rel] = true

		gpos, grejected := goParserFirstError(path, src)
		npos, nrejected := nanogoFirstError(path, src)

		var why string
		switch {
		case !grejected && !nrejected:
			continue
		case grejected && !nrejected:
			why = fmt.Sprintf("go/parser rejects at %s, nanogo accepts", gpos)
		case !grejected && nrejected:
			why = fmt.Sprintf("nanogo rejects at %s, go/parser accepts", npos)
		case gpos != npos:
			why = fmt.Sprintf("first error at %s for go/parser and %s for nanogo", gpos, npos)
		default:
			continue
		}

		if _, ok := corpusExceptions[rel]; ok {
			seen[rel] = true
			continue
		}
		disagreements = append(disagreements, rel+": "+why)
	}

	for _, d := range disagreements {
		t.Errorf("%s", d)
	}
	// An exception that no longer fires is a stale reason, and a stale reason
	// is how an exception list turns into a place to hide a bug. A released
	// distribution ships without some of the testdata a development tree has,
	// so an exception whose file is absent is reported and not failed.
	absent := 0
	for _, rel := range sortedKeys(corpusExceptions) {
		switch {
		case seen[rel]:
		case present[rel]:
			t.Errorf("exception %s no longer disagrees; remove it", rel)
		default:
			absent++
		}
	}
	if absent > 0 {
		t.Logf("%d exceptions name a file no root holds", absent)
	}
	if len(files) == 0 {
		t.Fatal("no file was compared")
	}
	t.Logf("compared %d files from %v, %d documented exceptions", len(files), roots, len(corpusExceptions))
}

// goParserFirstError returns the position go/parser reports first, as
// "line:col", and whether it rejected the file.
func goParserFirstError(path string, src []byte) (string, bool) {
	fset := gotoken.NewFileSet()
	_, err := goparser.ParseFile(fset, path, src, goparser.SkipObjectResolution)
	if err == nil {
		return "", false
	}
	if list, ok := err.(goscanner.ErrorList); ok && len(list) > 0 {
		return fmt.Sprintf("%d:%d", list[0].Pos.Line, list[0].Pos.Column), true
	}
	return "unknown", true
}

// nanogoFirstError returns the position nanogo reports first, as "line:col",
// and whether it rejected the file.
func nanogoFirstError(path string, src []byte) (string, bool) {
	fset := NewFileSet()
	f := fset.AddFile(path, len(src))
	_, err := Parse(f, src, func(Error) {}, nil, 0)
	if err == nil {
		return "", false
	}
	// Compare raw positions. A //line directive rewrites what a diagnostic
	// prints, and go/parser applies its own rewrite, so a reported position
	// would compare two different conventions.
	e, ok := err.(Error)
	if !ok {
		return "unknown", true
	}
	p := fset.RawPosition(e.Pos)
	return fmt.Sprintf("%d:%d", p.Line, p.Col), true
}

func sortedKeys(m map[string]string) []string {
	// The list is built and then sorted rather than ranged over into output,
	// because specs/053-determinism.md forbids the second on a path that
	// produces a message.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ----------------------------------------------------------------------------
// The errorcheck corpus of syntax errors

// TestErrorcheckSyntaxFiles runs the distribution's test/syntax directory,
// which is the part of specs/004's L2 corpus that is pure syntax.
//
// The corpus annotates a line rather than a count, because gc collapses the
// syntax errors it reports on one line. So the assertion is on the set of
// lines: every annotated line must carry an error, and no other line may. That
// is position and count together, a parser that reports the right number of
// errors in the wrong place passes neither half alone, and the cascade test
// above pins the count for a single mistake.
func TestErrorcheckSyntaxFiles(t *testing.T) {
	var dirs []string
	for _, root := range goRoots() {
		dir := filepath.Join(root, "test", "syntax")
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		missingCorpus(t, "no errorcheck corpus in any distribution")
		return
	}
	// The released and the development distribution hold the same file names,
	// so run only the first and keep the report one line per file.
	dir := dirs[0]
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	files := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.HasPrefix(string(src), "// errorcheck") {
			continue
		}
		files++
		t.Run(e.Name(), func(t *testing.T) {
			want := annotatedLines(string(src))
			got := reportedLines(path, src)
			if len(want) == 0 {
				t.Fatal("no // ERROR annotation in the file")
			}
			for _, line := range sortedInts(want) {
				if got[line] == 0 {
					t.Errorf("line %d: no error, want one", line)
				}
			}
			for _, line := range sortedInts(got) {
				if want[line] == 0 {
					t.Errorf("line %d: %d errors, want none", line, got[line])
				}
			}
		})
	}
	if files == 0 {
		t.Fatalf("no errorcheck file in %s", dir)
	}
	t.Logf("ran %d errorcheck files from %s", files, dir)
}

// errorAnnotation matches the "// ERROR ..." of the errorcheck corpus. The
// gccgo and the type checker annotations are not this parser's business.
var errorAnnotation = regexp.MustCompile(`//\s*(ERROR|ERRORAUTO)\b`)

// annotatedLines counts the ERROR annotations on each line of src.
func annotatedLines(src string) map[int]int {
	out := make(map[int]int)
	for i, line := range strings.Split(src, "\n") {
		if errorAnnotation.MatchString(line) {
			out[i+1]++
		}
	}
	return out
}

// reportedLines counts the errors nanogo reports on each line.
func reportedLines(path string, src []byte) map[int]int {
	fset := NewFileSet()
	f := fset.AddFile(path, len(src))
	out := make(map[int]int)
	Parse(f, src, func(err Error) {
		out[int(fset.RawPosition(err.Pos).Line)]++
	}, nil, 0)
	return out
}

func sortedInts(m map[int]int) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// ----------------------------------------------------------------------------
// One message per mistake

// TestOneMessagePerMistake is the cascade test of specs/011.
//
// Each source has one mistake in it. One mistake must produce one message: two
// would mean the recovery rules failed, either because a production returned
// nil and the caller reported the hole, or because a second error was reported
// at a position that already had one.
//
// Three sources produce two, and they are kept here with the reason written
// down rather than dropped. In each of them the parser skips past the token it
// could not read while recovering, and the token after it is wrong in its own
// right at a second position. That is the limit of skip-and-continue recovery
// and cmd/compile/internal/syntax has the same limit on the same inputs.
// specs/011's rule is therefore stronger than either parser achieves, and the
// gap is one message per mistake, not a cascade.
func TestOneMessagePerMistake(t *testing.T) {
	tests := []struct {
		src  string
		want int // number of messages; more than one is a documented limit
	}{
		{"package p\nfunc f() { x := }\n", 1},
		{"package p\nfunc f() { if }\n", 1},
		{"package p\nfunc f() { return 1 + }\n", 1},
		{"package p\nvar x = \n", 1},
		{"package p\nfunc f() { x. }\n", 1},
		{"package p\nfunc f() { a[] }\n", 1},
		{"package p\nfunc f() { go }\n", 1},
		{"package p\ntype T interface { M( }\n", 1},
		{"package p\nfunc f() chan {}\n", 1},
		{"package p\nfunc f() { defer (g()) }\n", 1},
		{"package p\nfunc f() { x, y = }\n", 1},
		{"package p\nconst c = 1 +\n", 1},
		{"package p\ntype T [3,]int\n", 1},
		{"package p\nfunc f() { _ = a[1::2] }\n", 1},
		{"package p\nfunc (a, b T) m() {}\n", 1},
		{"package p\nimport 1\n", 1},
		{"package p\nfunc f[]() {}\n", 1},
		{"package p\ntype T interface{ int | }\n", 1},
		{"package p\nfunc f() { _ = <- <-chan int }\n", 1},
		{"package p\ntype T map[int]\n", 1},

		// The documented limit. "case:" errors at the ":" because a case needs
		// an expression, advance consumes the ":" to make progress, and then
		// caseClause has no ":" left to want. The other two are the same shape:
		// the token the parser skips is the one the next production needed.
		{"package p\nfunc f() { switch { case: } }\n", 2},
		{"package p\nvar x map[int = 0\n", 2},
		{"package p\nvar x, = 1\n", 2},
	}

	for _, test := range tests {
		t.Run(strings.TrimSpace(strings.SplitN(test.src, "\n", 2)[1]), func(t *testing.T) {
			_, fset, errs := parseString(t, test.src)
			if len(errs) != test.want {
				for _, e := range errs {
					t.Logf("  %s: %s", fset.Position(e.Pos), e.Msg)
				}
				t.Errorf("got %d messages, want %d", len(errs), test.want)
			}
			// Whatever the count, no two messages may share a position. That
			// is rule 1 of specs/011 and it is what stops a cascade.
			at := make(map[Pos]bool)
			for _, e := range errs {
				if at[e.Pos] {
					t.Errorf("two messages at %s", fset.Position(e.Pos))
				}
				at[e.Pos] = true
			}
		})
	}
}

func TestAtMostOneErrorPerPosition(t *testing.T) {
	// Rule 1 of specs/011's recovery. Reporting twice at one position is how a
	// recovering parser turns one mistake into a page of output.
	var p parser
	fset := NewFileSet()
	f := fset.AddFile("x.go", 10)
	var got []Error
	p.init(f, []byte("package p\n"), func(err Error) { got = append(got, err) }, nil, 0)

	p.errorAt(f.Pos(3), "first")
	p.errorAt(f.Pos(3), "second at the same place")
	p.errorAt(f.Pos(4), "elsewhere")

	if len(got) != 2 {
		t.Fatalf("got %d errors, want 2: %v", len(got), got)
	}
	if got[0].Msg != "first" || got[1].Msg != "elsewhere" {
		t.Errorf("got %q and %q", got[0].Msg, got[1].Msg)
	}
	if p.errcnt != 2 {
		t.Errorf("errcnt = %d, want 2", p.errcnt)
	}
	// The first error is still the first one reported, dropped or not.
	if p.first == nil || p.first.(Error).Msg != "first" {
		t.Errorf("first = %v, want the first message", p.first)
	}
}

func TestNoProductionReturnsNil(t *testing.T) {
	// Rule 2 of specs/011's recovery. Every node in a broken tree is walkable,
	// which is what lets the type checker skip a Bad node instead of tripping
	// over a hole.
	broken := []string{
		"package p\nfunc f() { x := ; y := 2 }\n",
		"package p\nvar x = 1 +\n",
		"package p\ntype T [\n",
		"package p\nfunc f(] {}\n",
		"package p\nfunc f() { for ; ; { } }\n",
		"package p\nfunc f() { x.(*) }\n",
		"package p\ntype T struct { * }\n",
		"package p\nfunc f() { switch x.(type) { case: } }\n",
	}
	for _, src := range broken {
		t.Run(firstLine(src[len("package p\n"):]), func(t *testing.T) {
			file, _, errs := parseString(t, src)
			if len(errs) == 0 {
				t.Skip("the source parsed cleanly")
			}
			if file == nil {
				return // a broken package clause is the one case that stops
			}
			Inspect(file, func(n Node) bool {
				if n == nil {
					t.Error("the tree holds a nil node")
					return false
				}
				return true
			})
		})
	}
}

// ----------------------------------------------------------------------------
// Declarations, groups and pragmas

func TestGroupIsSharedWithinParentheses(t *testing.T) {
	// A const group's implicit repetition of the previous values is recognised
	// through the shared Group pointer, so two specs in one group must hold the
	// same pointer and two specs in different groups must not.
	src := `package p

const (
	A = iota
	B
)

const (
	C = 1
)

const D = 2

var (
	e, f int
	g    string
)

type (
	T1 int
	T2 int
)

import (
	"os"
)
`
	file, _, _ := parseString(t, src)

	var constGroups []*Group
	var varGroups []*Group
	var typeGroups []*Group
	var importGroups []*Group
	for _, d := range file.DeclList {
		switch d := d.(type) {
		case *ConstDecl:
			constGroups = append(constGroups, d.Group)
		case *VarDecl:
			varGroups = append(varGroups, d.Group)
		case *TypeDecl:
			typeGroups = append(typeGroups, d.Group)
		case *ImportDecl:
			importGroups = append(importGroups, d.Group)
		}
	}

	if len(constGroups) != 4 {
		t.Fatalf("got %d const declarations, want 4", len(constGroups))
	}
	if constGroups[0] == nil || constGroups[0] != constGroups[1] {
		t.Error("A and B do not share a group")
	}
	if constGroups[1] == constGroups[2] {
		t.Error("B and C share a group across two parenthesised runs")
	}
	if constGroups[3] != nil {
		t.Error("an unparenthesised const has a group")
	}
	if len(varGroups) != 2 || varGroups[0] == nil || varGroups[0] != varGroups[1] {
		t.Error("the var group is not shared")
	}
	if len(typeGroups) != 2 || typeGroups[0] == nil || typeGroups[0] != typeGroups[1] {
		t.Error("the type group is not shared")
	}
	if len(importGroups) != 1 || importGroups[0] == nil {
		t.Error("the import group is missing")
	}
}

// testPragma records the directives a declaration collected.
type testPragma struct {
	texts []string
}

func TestPragmaAttachesToTheNextDeclaration(t *testing.T) {
	src := `//go:build ignore

//go:file
package p

//go:onimport
import "os"

//go:onconst
const C = 1

//go:ontype
type T int

//go:onvar
var V int

//go:onfunc1
//go:onfunc2
func F() {}
`
	fset := NewFileSet()
	f := fset.AddFile("p.go", len(src))
	var unused []string
	pragh := func(pos Pos, blank bool, text string, current Pragma) Pragma {
		if text == "" {
			// An empty text returns a pragma unused, which is how the parser
			// says the directive belonged to nothing.
			if p, _ := current.(*testPragma); p != nil {
				unused = append(unused, p.texts...)
			}
			return nil
		}
		p, _ := current.(*testPragma)
		if p == nil {
			p = new(testPragma)
		}
		p.texts = append(p.texts, text)
		return p
	}

	file, err := Parse(f, []byte(src), func(e Error) { t.Errorf("%s: %s", fset.Position(e.Pos), e.Msg) }, pragh, 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	texts := func(p Pragma) []string {
		if p == nil {
			return nil
		}
		return p.(*testPragma).texts
	}
	got := map[string][]string{"file": texts(file.Pragma)}
	for _, d := range file.DeclList {
		switch d := d.(type) {
		case *ImportDecl:
			got["import"] = texts(d.Pragma)
		case *ConstDecl:
			got["const"] = texts(d.Pragma)
		case *TypeDecl:
			got["type"] = texts(d.Pragma)
		case *VarDecl:
			got["var"] = texts(d.Pragma)
		case *FuncDecl:
			got["func"] = texts(d.Pragma)
		}
	}

	want := map[string][]string{
		"file":   {"go:build ignore", "go:file"},
		"import": {"go:onimport"},
		"const":  {"go:onconst"},
		"type":   {"go:ontype"},
		"var":    {"go:onvar"},
		"func":   {"go:onfunc1", "go:onfunc2"},
	}
	for _, key := range []string{"file", "import", "const", "type", "var", "func"} {
		if strings.Join(got[key], ",") != strings.Join(want[key], ",") {
			t.Errorf("%s pragma = %q, want %q", key, got[key], want[key])
		}
	}
}

func TestPragmaBeforeAStatementIsReturnedUnused(t *testing.T) {
	src := "package p\n\nfunc f() {\n\t//go:stray\n\tx := 1\n\t_ = x\n}\n"
	fset := NewFileSet()
	f := fset.AddFile("p.go", len(src))
	unused := 0
	pragh := func(pos Pos, blank bool, text string, current Pragma) Pragma {
		if text == "" {
			unused++
			return nil
		}
		return text
	}
	if _, err := Parse(f, []byte(src), nil, pragh, 0); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if unused != 1 {
		t.Errorf("the handler was told of %d unused pragmas, want 1", unused)
	}
}

// ----------------------------------------------------------------------------
// CheckBranches

func TestCheckBranches(t *testing.T) {
	tests := []struct {
		src  string
		want string // a substring of the expected message, or "" for none
	}{
		{"func f() { break }", "break is not in a loop"},
		{"func f() { continue }", "continue is not in a loop"},
		{"func f() { for { break } }", ""},
		{"func f() { for { continue } }", ""},
		{"func f() { switch { case true: break } }", ""},
		{"func f() { goto L }", "label L not defined"},
		{"func f() { goto L\nL:\n}", ""},
		{"func f() {\nL:\n\tfor { break L }\n}", ""},
		{"func f() {\nL:\n\tfor { continue L }\n}", ""},
		{"func f() {\nL:\n\tswitch { default: continue L }\n}", "invalid continue label"},
		{"func f() {\nL:\n\tvar x int\n\t_ = x\n}", "label L defined and not used"},
		{"func f() { break L }", "break label not defined"},
		{"func f() { continue L }", "continue label not defined"},
		{"func f() {\nL:\n\tfor {}\nL2:\n\tfor { break L }\n}", "label L2 defined and not used"},
		{"func f() { switch { case true: fallthrough\ndefault: } }", ""},
		{"func f() { switch { default: fallthrough } }", "cannot fallthrough final case"},
		{"func f() { switch any(1).(type) { case int: fallthrough\ndefault: } }", "cannot fallthrough in type switch"},
		{"func f() { fallthrough }", "fallthrough statement out of place"},
		{"func f() {\n\tgoto L\n\tx := 1\n\t_ = x\nL:\n}", "jumps over declaration"},
		{"func f() {\n\tgoto L\n\t{\nL:\n\t}\n}", "jumps into block"},
		{"func f() {\nL:\n\tfor {}\nL:\n\tfor {}\n}", "already defined"},
		{"func f() {\n_:\n\tfor {}\n}", ""},
		{"func f() {\nL:\n\tselect { default: break L }\n}", ""},
		{"func f() {\nL:\n\tif true { break L }\n}", "invalid break label"},
	}

	for _, test := range tests {
		t.Run(firstLine(test.src), func(t *testing.T) {
			src := "package p\n" + test.src + "\n"
			fset := NewFileSet()
			f := fset.AddFile("p.go", len(src))
			var msgs []string
			Parse(f, []byte(src), func(err Error) { msgs = append(msgs, err.Msg) }, nil, CheckBranches)

			joined := strings.Join(msgs, "\n")
			if test.want == "" {
				if len(msgs) != 0 {
					t.Errorf("got %q, want no message", joined)
				}
				return
			}
			if !strings.Contains(joined, test.want) {
				t.Errorf("got %q, want a message containing %q", joined, test.want)
			}
		})
	}
}

func TestCheckBranchesResolvesTargets(t *testing.T) {
	src := "package p\nfunc f() {\nL:\n\tfor {\n\t\tbreak L\n\t\tcontinue L\n\t}\n\tgoto E\nE:\n}\n"
	fset := NewFileSet()
	f := fset.AddFile("p.go", len(src))
	file, err := Parse(f, []byte(src), func(e Error) { t.Errorf("%s: %s", fset.Position(e.Pos), e.Msg) }, nil, CheckBranches)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	branches := 0
	Inspect(file, func(n Node) bool {
		if b, ok := n.(*BranchStmt); ok {
			branches++
			if b.Target == nil {
				t.Errorf("%s at %s has no target", b.Tok, fset.Position(b.Pos()))
			}
		}
		return true
	})
	if branches != 3 {
		t.Errorf("found %d branch statements, want 3", branches)
	}
}

func TestCheckBranchesSkipsABodyWithSyntaxErrors(t *testing.T) {
	// A body with a hole in it produces branch errors that are consequences of
	// the syntax error, not of the branches.
	src := "package p\nfunc f() {\n\tbreak\n\tx := \n}\n"
	fset := NewFileSet()
	f := fset.AddFile("p.go", len(src))
	var msgs []string
	Parse(f, []byte(src), func(err Error) { msgs = append(msgs, err.Msg) }, nil, CheckBranches)
	for _, m := range msgs {
		if strings.Contains(m, "break is not in a loop") {
			t.Errorf("the branch check ran over a body with a syntax error: %q", msgs)
		}
	}
}

// ----------------------------------------------------------------------------
// The public entry points

func TestParseFileReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	src := "package p\n\nfunc f() int { return 1 }\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	fset := NewFileSet()
	file, err := ParseFile(fset, path, nil, nil, 0)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if file.PkgName.Value != "p" {
		t.Errorf("package name = %q, want %q", file.PkgName.Value, "p")
	}
	// The file must have been added to the set, or no position in the tree
	// resolves.
	if got := fset.Position(file.Pos()).Filename; got != path {
		t.Errorf("position filename = %q, want %q", got, path)
	}
}

func TestParseFileReportsAMissingFile(t *testing.T) {
	fset := NewFileSet()
	var reported int
	_, err := ParseFile(fset, filepath.Join(t.TempDir(), "absent.go"), func(Error) { reported++ }, nil, 0)
	if err == nil {
		t.Fatal("ParseFile accepted a file that does not exist")
	}
	if reported != 1 {
		t.Errorf("the handler was called %d times, want 1", reported)
	}
}

func TestParseReturnsTheFirstError(t *testing.T) {
	_, fset, errs := parseString(t, "package p\nfunc f() { x := }\nfunc g() { y := }\n")
	if len(errs) < 2 {
		t.Fatalf("got %d errors, want at least 2", len(errs))
	}
	fset2 := NewFileSet()
	src := "package p\nfunc f() { x := }\nfunc g() { y := }\n"
	f := fset2.AddFile("test.go", len(src))
	_, err := Parse(f, []byte(src), nil, nil, 0)
	if err == nil {
		t.Fatal("Parse returned no error")
	}
	if err.(Error).Msg != errs[0].Msg {
		t.Errorf("first error = %q, want %q", err.(Error).Msg, errs[0].Msg)
	}
	_ = fset
}

func TestParseWithoutAPackageClause(t *testing.T) {
	file, _, errs := parseString(t, "func f() {}\n")
	if file != nil {
		t.Error("a file with no package clause produced a tree")
	}
	if len(errs) != 1 {
		t.Errorf("got %d errors, want 1", len(errs))
	}
}

func TestParseEmptyInput(t *testing.T) {
	file, _, errs := parseString(t, "")
	if file != nil {
		t.Error("an empty file produced a tree")
	}
	if len(errs) != 1 {
		t.Errorf("got %d errors, want 1", len(errs))
	}
}

// ----------------------------------------------------------------------------
// Constructs the corpus does not reach often enough to trust

func TestParsesEveryConstruct(t *testing.T) {
	for _, src := range positionCorpus {
		mustParse(t, src)
	}
}

func TestErrorRecoveryContinuesAfterTheMistake(t *testing.T) {
	// Recovery is worth having only if the parser reads what follows. Each of
	// these has a mistake in the first function and a sound second one.
	src := `package p

func broken() {
	x :=
}

func sound() int {
	return 42
}
`
	file, _, errs := parseString(t, src)
	if len(errs) == 0 {
		t.Fatal("the mistake was not reported")
	}
	if file == nil || len(file.DeclList) != 2 {
		t.Fatalf("got %d declarations, want 2", len(file.DeclList))
	}
	f, ok := file.DeclList[1].(*FuncDecl)
	if !ok || f.Name.Value != "sound" || f.Body == nil {
		t.Error("the declaration after the mistake was not read")
	}
}

func TestMisplacedImportIsReportedAndRead(t *testing.T) {
	file, _, errs := parseString(t, "package p\nvar x int\nimport \"os\"\n")
	if len(errs) != 1 {
		t.Errorf("got %d errors, want 1", len(errs))
	}
	if file == nil || len(file.DeclList) != 2 {
		t.Fatalf("the misplaced import was not read")
	}
	if _, ok := file.DeclList[1].(*ImportDecl); !ok {
		t.Errorf("got %T, want *ImportDecl", file.DeclList[1])
	}
}

func TestSelectedErrorMessages(t *testing.T) {
	// The messages that name a construct, which are the ones a reader acts on.
	tests := []struct {
		src  string
		want string
	}{
		{"package p\nfunc f() { if x = 1 {} }\n", "cannot use assignment"},
		{"package p\nfunc f() { go (g()) }\n", "must not be parenthesized"},
		{"package p\ntype T struct { (int) }\n", "cannot parenthesize embedded type"},
		{"package p\nfunc f() { for i := 0; ; i := 1 {} }\n", "cannot declare in post statement"},
		{"package p\ntype T interface{ M[P any]() }\n", "must have no type parameters"},
		{"package p\nfunc f(a ...int, b int) {}\n", "can only use ... with final parameter"},
		{"package p\ntype T func(a ...int) int\nfunc g() (...int) { return }\n", "invalid use of ..."},
		{"package p\nfunc f() { _ = a[1::2] }\n", "middle index required"},
		{"package p\nfunc f() { _ = a[1:2:] }\n", "final index required"},
		{"package p\ntype T [3,]int\n", "unexpected comma"},
		{"package p\nfunc (a, b T) m() {}\n", "method has multiple receivers"},
		{"package p\nfunc () m() {}\n", "method has no receiver"},
		{"package p\ntype T chan\n", "missing channel element type"},
		{"package p\nimport x\n", "missing import path"},
		{"package p\nimport 1\n", "import path must be a string"},
		{"package p\nfunc f[]() {}\n", "empty type parameter list"},
		{"package p\nfunc f() { _ = <-chan<- int }\n", "unexpected int, expected chan"},
		{"package p\nfunc f() { _ = <- <-chan int }\n", "unexpected <-, expected chan"},
		{"package p\ntype T [N,]int\n", "missing type constraint"},
		{"package p\nfunc f() { if var x = 1; x {} }\n", "var declaration not allowed"},
		{"package p\nfunc f() { x := (T{}) }\n", ""},
		{"package p\ntype T interface{ int | }\n", "expected ~ term or type"},
	}

	for _, test := range tests {
		t.Run(test.want, func(t *testing.T) {
			_, _, errs := parseString(t, test.src)
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, e.Msg)
			}
			joined := strings.Join(msgs, "\n")
			if test.want == "" {
				if len(errs) != 0 {
					t.Errorf("got %q, want no message", joined)
				}
				return
			}
			if !strings.Contains(joined, test.want) {
				t.Errorf("got %q, want a message containing %q", joined, test.want)
			}
		})
	}
}

func TestSourcesTheParserAccepts(t *testing.T) {
	// Forms that a hand-written corpus reaches and the distribution does not,
	// or reaches so rarely that a regression would not be caught.
	sources := []string{
		"package p\nfunc f() (int, error) { return 0, nil }\n",
		"package p\nfunc f(a, b int, c ...string) {}\n",
		"package p\ntype T struct{ *T }\n",
		"package p\ntype T struct{ p.Q }\n",
		"package p\ntype T struct{ *p.Q }\n",
		"package p\ntype T interface{ p.Q }\n",
		"package p\ntype T interface{ M[P] }\n",
		"package p\ntype T interface{ comparable }\n",
		"package p\ntype T [3][4]int\n",
		"package p\nvar x = struct{ a int }{1}\n",
		"package p\nvar x = [...][2]int{{1, 2}}\n",
		"package p\nfunc f() { var _ = func() {}; _ = 1 }\n",
		"package p\nfunc f() { x := 1; x, y := 2, 3; _, _ = x, y }\n",
		"package p\nfunc f() { _ = <-make(chan int) }\n",
		"package p\ntype T = int\n",
		"package p\ntype T[P any] = []P\n",
		"package p\nfunc f() { switch x := 1; x { case 1, 2: } }\n",
		"package p\nfunc f() { switch x := any(1).(type) { case nil: _ = x } }\n",
		"package p\nfunc f() { for range 10 {} }\n",
		"package p\nfunc f() { _ = map[string]struct{}{} }\n",
		"package p\nfunc f() { _ = []func(){nil} }\n",
		"package p\nfunc f() { _ = T{}.f }\n",
		"package p\nfunc f() { defer close(make(chan int)) }\n",
		"package p\nvar _ = func() (_ int) { return }\n",
		"package p\nfunc f() { l1: for { break l1 } }\n",
		"package p\nfunc f() { ; }\n",
		"package p\nfunc _()\n",
		"package p\nvar (\n)\n",
		"package p\nfunc f() { _ = (*T)(nil) }\n",
		"package p\nfunc f() { _ = a.(type1) }\n",
		"package p\nfunc f[T interface{ ~int | ~[]byte }](x T) {}\n",
		"package p\nfunc (T) m() {}\n",
		"package p\nfunc (*T[P]) m() {}\n",
		// Forms the parser accepts and the type checker rejects. specs/011
		// gives the parser the grammar and nothing else, so each of these has
		// to produce a tree rather than a message.
		"package p\ntype T[P int | string] struct{}\n",
		"package p\ntype T[P interface{ int } | string] struct{}\n",
		"package p\nfunc f[P ~int | ~string]() {}\n",
		"package p\ntype ()\n",
		"package p\nfunc f(int, x) {}\n",
		"package p\nfunc f(x, int y) {}\n",
		"package p\nfunc f() (x, y) {}\n",
	}
	for _, src := range sources {
		t.Run(firstLine(src[len("package p\n"):]), func(t *testing.T) {
			mustParse(t, src)
		})
	}
}

func TestUnparenAndUnpackListExpr(t *testing.T) {
	// The parser records a parenthesis around an expression, because "(x) := 1"
	// has to be rejected and the tree is what says the parenthesis was there.
	// Around a type it records none, because nothing needs one.
	file, _ := mustParse(t, "package p\nfunc f() { _ = (((1))) }\n")
	rhs := file.DeclList[0].(*FuncDecl).Body.List[0].(*AssignStmt).Rhs
	if got := sketch(rhs); got != "ParenExpr(ParenExpr(ParenExpr(BasicLit)))" {
		t.Errorf("shape = %s, want three parentheses", got)
	}
	if _, ok := Unparen(rhs).(*BasicLit); !ok {
		t.Errorf("Unparen left %T", Unparen(rhs))
	}
	typed, _ := mustParse(t, "package p\nvar x ((int))\n")
	if got := sketch(typed.DeclList[0]); got != "VarDecl(Name,Name)" {
		t.Errorf("type shape = %s, want no parentheses", got)
	}

	inner := NewName(NoPos, "x")
	var wrapped Expr = inner
	for i := 0; i < 3; i++ {
		p := new(ParenExpr)
		p.X = wrapped
		wrapped = p
	}
	if Unparen(wrapped) != Expr(inner) {
		t.Error("Unparen did not strip every parenthesis")
	}

	if got := UnpackListExpr(nil); got != nil {
		t.Errorf("UnpackListExpr(nil) = %v, want nil", got)
	}
	if got := UnpackListExpr(inner); len(got) != 1 || got[0] != Expr(inner) {
		t.Errorf("UnpackListExpr of one expression = %v", got)
	}
	list := &ListExpr{ElemList: []Expr{inner, inner}}
	if got := UnpackListExpr(list); len(got) != 2 {
		t.Errorf("UnpackListExpr of a list = %v", got)
	}
}

func TestStartAndEndPosOfEveryNode(t *testing.T) {
	// StartPos and EndPos are read by the type checker for every scope it
	// opens, so a node that answers NoPos is a scope that covers nothing.
	for _, src := range positionCorpus {
		file, fset := mustParse(t, src)
		Inspect(file, func(n Node) bool {
			if !StartPos(n).IsKnown() {
				t.Errorf("StartPos(%s) at %s is unknown", nodeKind(n), fset.Position(n.Pos()))
			}
			if !EndPos(n).IsKnown() {
				t.Errorf("EndPos(%s) at %s is unknown", nodeKind(n), fset.Position(n.Pos()))
			}
			return true
		})
	}
	if StartPos(nil) != NoPos || EndPos(nil) != NoPos {
		t.Error("a nil node has a position")
	}
}

func TestEndPosOfAnIncrement(t *testing.T) {
	// ImplicitOne is shared and has no position, so an increment's end is
	// computed from its operand.
	file, _ := mustParse(t, "package p\nfunc f() { i := 0; i++ }\n")
	inc := file.DeclList[0].(*FuncDecl).Body.List[1].(*AssignStmt)
	if inc.Rhs != Expr(ImplicitOne) {
		t.Fatalf("Rhs = %v, want ImplicitOne", inc.Rhs)
	}
	if EndPos(inc) != EndPos(inc.Lhs)+2 {
		t.Error("an increment does not end two bytes past its operand")
	}
}

func TestNumericPositionsAreBytes(t *testing.T) {
	// Columns count bytes, so a multibyte identifier moves the column by its
	// length in bytes. specs/010 fixes this and the corpus is annotated for it.
	src := "package p\nvar ä = 1\n"
	file, fset := mustParse(t, src)
	d := file.DeclList[0].(*VarDecl)
	pos := fset.Position(d.Values.Pos())
	if pos.Col != uint(strings.Index(strings.Split(src, "\n")[1], "1")+1) {
		t.Errorf("column = %d, want the byte column of the literal", pos.Col)
	}
	_ = strconv.Itoa
}

// TestErrorRecoveryPaths reaches the recovery branches that a corpus of valid
// source never touches. Each source names the branch it is here for.
func TestErrorRecoveryPaths(t *testing.T) {
	sources := []string{
		// blockStmt with no brace at all, and with one that closes at once.
		"package p\nfunc f() { if true }\n",
		"package p\nfunc f() { if true }\n}\n",
		"package p\nfunc f() { for true }\n",
		"package p\nfunc f() { switch true }\n",
		// select and switch bodies that do not open or do not hold clauses.
		"package p\nfunc f() { select }\n",
		"package p\nfunc f() { select { x } }\n",
		"package p\nfunc f() { switch { x } }\n",
		"package p\nfunc f() { switch x.(type) { x } }\n",
		// A label with no statement after it.
		"package p\nfunc f() { switch { case true: L: case false: } }\n",
		// simpleStmt with a list and no assignment operator.
		"package p\nfunc f() { a, b }\n",
		"package p\nfunc f() { a, b, c }\n",
		// A type switch guard whose left hand side is not a plain name.
		"package p\nfunc f() { switch a, b := x.(type) { } }\n",
		// Embedded fields written with parentheses.
		"package p\ntype T struct{ (int) }\n",
		"package p\ntype T struct{ (*int) }\n",
		"package p\ntype T struct{ *(int) }\n",
		"package p\ntype T struct{ 1 }\n",
		// Interface methods with an empty bracket pair, which is neither a type
		// parameter list nor a type argument list.
		"package p\ntype T interface{ M[]() }\n",
		"package p\ntype T interface{ M[] }\n",
		"package p\ntype T interface{ M[)]() }\n",
		// A type parameter list that reads as a union, with a missing bound.
		"package p\ntype T[P |] struct{}\n",
		// Assignment written with ":=" where "=" belongs.
		"package p\nvar x int := 1\n",
		"package p\nconst c := 1\n",
		// A declaration that runs into the next one.
		"package p\nfunc f() {} func g() {}\n",
		"package p\nvar (x int; y)\n",
		// Arguments and lists that do not close.
		"package p\nfunc f() { g(1 2) }\n",
		"package p\nfunc f() { _ = []int{1 2} }\n",
		"package p\nfunc f() { _ = a[1 2] }\n",
		// Assertions and selectors that do not continue.
		"package p\nfunc f() { _ = a.(1) }\n",
		"package p\nfunc f() { _ = a.1 }\n",
		// A statement keyword where a declaration belongs.
		"package p\nreturn\n",
		"package p\nfunc f()\n{\n}\n",
		// Signatures the parser has to guess at.
		"package p\nfunc f(...) {}\n",
		"package p\nfunc f[T]() {}\n",
	}
	for _, src := range sources {
		t.Run(firstLine(src[len("package p\n"):]), func(t *testing.T) {
			file, _, errs := parseString(t, src)
			if len(errs) == 0 {
				t.Fatal("the source parsed cleanly; it no longer covers a recovery branch")
			}
			// Recovery has to leave a tree that can be walked, whatever it
			// found, and every node in it has to carry a position.
			if file == nil {
				return
			}
			Inspect(file, func(n Node) bool {
				if !n.Pos().IsKnown() {
					t.Errorf("%s has no position", nodeKind(n))
				}
				return true
			})
		})
	}
}

func TestGotoIntoAClosedBlock(t *testing.T) {
	// The label exists but is not reachable from the goto, which is the branch
	// that separates "label not defined" from "jumps into block".
	src := "package p\nfunc f() {\n\t{\nL:\n\t}\n\tgoto L\n}\n"
	fset := NewFileSet()
	f := fset.AddFile("p.go", len(src))
	var msgs []string
	Parse(f, []byte(src), func(err Error) { msgs = append(msgs, err.Msg) }, nil, CheckBranches)
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "jumps into block") {
		t.Errorf("got %q, want a message about jumping into a block", joined)
	}
}

func TestErrorMessagesNameTheTokenFound(t *testing.T) {
	// syntaxErrorAt spells the current token into the message, and each token
	// class spells differently.
	tests := []struct {
		src  string
		want string
	}{
		{"package p\nfunc f() { _ = a.1 }\n", "literal .1"},
		{"package p\nfunc f() { if += 1 {} }\n", "+="},
		{"package p\nfunc f() { if ++ }\n", "++"},
		{"package p\nfunc f() { _ = a.* }\n", "*"},
		{"package p\nfunc 1() {}\n", "literal 1"},
		{"package p\nvar x = ,\n", "comma"},
		{"package p\nfunc f() { x := 1 2 }\n", "at end of statement"},
	}
	for _, test := range tests {
		t.Run(test.want, func(t *testing.T) {
			_, _, errs := parseString(t, test.src)
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, e.Msg)
			}
			joined := strings.Join(msgs, "\n")
			if !strings.Contains(joined, test.want) {
				t.Errorf("got %q, want a message containing %q", joined, test.want)
			}
		})
	}
}

func TestStartAndEndPosOfIncompleteNodes(t *testing.T) {
	// The type checker calls StartPos and EndPos on whatever the parser built,
	// including the nodes a recovery left half empty, and a scope that answers
	// NoPos covers nothing at all.
	at := func(off int) Pos { return Pos(100 + off) }
	nm := func(v string, off int) *Name { return NewName(at(off), v) }

	tests := []struct {
		node               Node
		wantStart, wantEnd Pos
	}{
		{&ImportDecl{decl: decl{node: node{pos: at(0)}}}, at(0), at(0)},
		{&ConstDecl{NameList: []*Name{nm("a", 1)}, decl: decl{node: node{pos: at(0)}}}, at(0), at(2)},
		{&ConstDecl{decl: decl{node: node{pos: at(0)}}}, at(0), at(0)},
		{&VarDecl{NameList: []*Name{nm("a", 1)}, decl: decl{node: node{pos: at(0)}}}, at(0), at(2)},
		{&VarDecl{decl: decl{node: node{pos: at(0)}}}, at(0), at(0)},
		{&VarDecl{Type: nm("int", 4), decl: decl{node: node{pos: at(0)}}}, at(0), at(7)},
		{&ConstDecl{Type: nm("int", 4), decl: decl{node: node{pos: at(0)}}}, at(0), at(7)},
		{&FuncDecl{decl: decl{node: node{pos: at(0)}}}, at(0), at(0)},
		{&CompositeLit{Rbrace: at(3), expr: expr{node: node{pos: at(0)}}}, at(0), at(3)},
		{&ListExpr{expr: expr{node: node{pos: at(0)}}}, at(0), at(0)},
		{&StructType{expr: expr{node: node{pos: at(0)}}}, at(0), at(0)},
		{&InterfaceType{expr: expr{node: node{pos: at(0)}}}, at(0), at(0)},
		{&FuncType{expr: expr{node: node{pos: at(0)}}}, at(0), at(0)},
		{&FuncType{ParamList: []*Field{{Name: nm("a", 5), node: node{pos: at(5)}}}, expr: expr{node: node{pos: at(0)}}}, at(0), at(6)},
		{&Field{Name: nm("a", 1), node: node{pos: at(1)}}, at(1), at(2)},
		{&SliceExpr{X: nm("a", 0), expr: expr{node: node{pos: at(1)}}}, at(0), at(1)},
		{&AssignStmt{Lhs: nm("a", 0), simpleStmt: simpleStmt{stmt: stmt{node: node{pos: at(1)}}}}, at(0), at(1)},
		{&BranchStmt{stmt: stmt{node: node{pos: at(0)}}}, at(0), at(0)},
		{&ReturnStmt{stmt: stmt{node: node{pos: at(0)}}}, at(0), at(0)},
		{&DeclStmt{stmt: stmt{node: node{pos: at(0)}}}, at(0), at(0)},
		{&CaseClause{Colon: at(4), node: node{pos: at(0)}}, at(0), at(4)},
		{&CommClause{Colon: at(4), node: node{pos: at(0)}}, at(0), at(4)},
		{&TypeSwitchGuard{X: nm("a", 0), expr: expr{node: node{pos: at(2)}}}, at(0), at(1)},
		{&Operation{Op: Not, X: nm("a", 1), expr: expr{node: node{pos: at(0)}}}, at(0), at(2)},
		{&CallExpr{Fun: nm("f", 0), expr: expr{node: node{pos: at(1)}}}, at(0), at(1)},
		{&BadExpr{expr: expr{node: node{pos: at(0)}}}, at(0), at(0)},
		{&BadStmt{stmt: stmt{node: node{pos: at(0)}}}, at(0), at(0)},
		{&EmptyStmt{simpleStmt: simpleStmt{stmt: stmt{node: node{pos: at(0)}}}}, at(0), at(0)},
	}

	for _, test := range tests {
		if got := StartPos(test.node); got != test.wantStart {
			t.Errorf("StartPos(%s) = %d, want %d", nodeKind(test.node), got, test.wantStart)
		}
		if got := EndPos(test.node); got != test.wantEnd {
			t.Errorf("EndPos(%s) = %d, want %d", nodeKind(test.node), got, test.wantEnd)
		}
	}
}

func TestEndPosOfAStructWithATag(t *testing.T) {
	// A tag is the last thing in the struct, and it lives in a list parallel to
	// the fields rather than inside the field it belongs to.
	file, _ := mustParse(t, "package p\ntype T struct{ a int `x` }\n")
	st := file.DeclList[0].(*TypeDecl).Type.(*StructType)
	if len(st.TagList) != 1 || st.TagList[0] == nil {
		t.Fatalf("TagList = %v", st.TagList)
	}
	if EndPos(st) != EndPos(st.TagList[0]) {
		t.Error("the struct does not end at its last tag")
	}
}
