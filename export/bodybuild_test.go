// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The oracle the body builder is measured by.
//
// gc's archive for a standard library package holds, for each function whose
// body it exported, the tree gc built out of that function's source. nanogo
// parses and type checks the same source. So for one function there are two
// trees that must be the same tree, and comparing them is a test of the
// builder over thousands of real functions, available before any generic
// declaration can be stenciled end to end.
//
// The comparison is made on the encoding rather than on the trees, because
// the encoder is already byte exact against gc (bodywrite_test.go) and the
// trees hold a types2.Type from two different type checks, which are equal by
// what they say and never by identity.
//
// What the encoding cannot be compared on is the element index of every
// reference. A body names a type, an object, a package, a string, a position
// base and another body by index, and the index belongs to the package being
// written. gc's tree carries the index gc's archive gave it, and a tree built
// from syntax has no index at all. So both sides are encoded through
// [symRefs], which numbers a reference by what it names. Two encodings that
// agree byte for byte then agree on every field and on the order and the
// identity of every reference, and disagree only about numbering, which
// [elemRefs] already proves from the other side.

// symRefs numbers each thing a body names by what it names rather than by
// where it landed.
//
// The numbering is first-use order within one walk, so an encoding of two
// trees that name the same things in the same order produces the same
// numbers. A tree that names one thing more, fewer, or in another order
// produces a different reference table, which is the difference the test is
// looking for.
type symRefs struct {
	tables map[pkgbits.SectionKind]map[string]pkgbits.Index
	next   map[pkgbits.SectionKind]pkgbits.Index

	// bodies numbers a function literal's body by the literal, because two
	// literals are never the same body and nothing else names one.
	bodies map[*FuncLitExpr]pkgbits.Index

	// locals numbers each type declared inside a function by identity.
	locals map[*types2.TypeName]int
}

func newSymRefs() *symRefs {
	return &symRefs{
		tables: make(map[pkgbits.SectionKind]map[string]pkgbits.Index),
		next:   make(map[pkgbits.SectionKind]pkgbits.Index),
		bodies: make(map[*FuncLitExpr]pkgbits.Index),
		locals: make(map[*types2.TypeName]int),
	}
}

func (s *symRefs) intern(k pkgbits.SectionKind, key string) pkgbits.Index {
	t := s.tables[k]
	if t == nil {
		t = make(map[string]pkgbits.Index)
		s.tables[k] = t
	}
	if idx, ok := t[key]; ok {
		return idx
	}
	idx := s.next[k]
	s.next[k] = idx + 1
	t[key] = idx
	return idx
}

func (s *symRefs) strIdx(str string) pkgbits.Index {
	return s.intern(pkgbits.SectionString, str)
}

func (s *symRefs) pkgIdx(pkg *types2.Package) pkgbits.Index {
	if pkg == nil {
		return s.intern(pkgbits.SectionPkg, "builtin")
	}
	return s.intern(pkgbits.SectionPkg, pkg.Path())
}

func (s *symRefs) typIdx(t TypeUse) pkgbits.Index {
	return s.intern(pkgbits.SectionType, s.typeKey(t.Type))
}

func (s *symRefs) objIdx(o ObjUse) pkgbits.Index {
	var b strings.Builder
	if o.Pkg != nil {
		b.WriteString(o.Pkg.Path())
	}
	b.WriteString(".")
	b.WriteString(o.Name)
	for _, t := range o.Targs {
		b.WriteString("|")
		b.WriteString(s.typeKey(t.Type))
	}
	return s.intern(pkgbits.SectionObj, b.String())
}

func (s *symRefs) posBaseIdx(p Pos) pkgbits.Index {
	return s.intern(pkgbits.SectionPosBase, normalizeFile(p.File))
}

func (s *symRefs) bodyIdx(e *FuncLitExpr) pkgbits.Index {
	if idx, ok := s.bodies[e]; ok {
		return idx
	}
	idx := s.next[pkgbits.SectionBody]
	s.next[pkgbits.SectionBody] = idx + 1
	s.bodies[e] = idx
	return idx
}

// dictIdx answers with the slot itself. The oracle compares two trees of the
// same declaration, so a slot means the same thing in both.
func (s *symRefs) dictIdx(_ string, slot int) int { return slot }

var _ bodyRefs = (*symRefs)(nil)

// typeKey names a type by the element a writer would put it in.
//
// The qualifier is the import path, because two packages of one name are two
// types and the default qualifier prints only the name. Two canonicalisations
// follow the type writer, and both are its rules and not this resolver's:
//
//   - The canonical empty interface is written as a reference to the TypeName
//     any, so "any" and "interface{}" are one element (writer.go).
//   - An alias declared inside a function is stripped to what it names,
//     because two local aliases of one name would collide.
func (s *symRefs) typeKey(t types2.Type) string {
	if t == nil {
		return "<nil>"
	}
	for {
		alias, ok := t.(*types2.Alias)
		if !ok || alias.Obj().Parent() == alias.Obj().Pkg().Scope() {
			break
		}
		t = alias.Rhs()
	}
	if anyName := types2.Universe.Lookup("any"); anyName != nil &&
		types2.Unalias(t) == types2.Unalias(anyName.Type()) {
		return "any"
	}
	key := types2.TypeString(t, func(pkg *types2.Package) string { return pkg.Path() })
	// A type declared inside a function has no qualified name: two of them
	// print alike and gc gives each an element of its own, disambiguating
	// the two by a number it derives from the declaring scope. Here they are
	// told apart by identity, which is the same distinction made without
	// reproducing gc's numbering.
	for _, obj := range localTypeNames(t) {
		n, ok := s.locals[obj]
		if !ok {
			n = len(s.locals)
			s.locals[obj] = n
		}
		key += fmt.Sprintf("#%d", n)
	}
	return key
}

// localTypeNames returns the objects of the types declared inside a function
// that a type reaches, in a deterministic order.
func localTypeNames(t types2.Type) []*types2.TypeName {
	var out []*types2.TypeName
	seen := make(map[types2.Type]bool)
	var walk func(types2.Type)
	walk = func(t types2.Type) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		switch t := t.(type) {
		case *types2.Alias:
			if isLocalTypeName(t.Obj()) {
				out = append(out, t.Obj())
			}
			walk(t.Rhs())
		case *types2.Named:
			if isLocalTypeName(t.Obj()) {
				out = append(out, t.Obj())
				walk(t.Underlying())
			}
		case *types2.Pointer:
			walk(t.Elem())
		case *types2.Slice:
			walk(t.Elem())
		case *types2.Array:
			walk(t.Elem())
		case *types2.Chan:
			walk(t.Elem())
		case *types2.Map:
			walk(t.Key())
			walk(t.Elem())
		case *types2.Tuple:
			for i := range t.Len() {
				walk(t.At(i).Type())
			}
		case *types2.Signature:
			walk(t.Params())
			walk(t.Results())
		case *types2.Struct:
			for i := range t.NumFields() {
				walk(t.Field(i).Type())
			}
		case *types2.Interface:
			for i := range t.NumMethods() {
				walk(t.Method(i).Type())
			}
		}
	}
	walk(t)
	return out
}

// isLocalTypeName reports whether a type was declared inside a function.
func isLocalTypeName(obj *types2.TypeName) bool {
	return obj.Pkg() != nil && obj.Parent() != obj.Pkg().Scope()
}

// normalizeFile puts the two sides' file names in one space.
//
// gc writes the name objabi.AbsFile produced, which for the standard library
// is rooted at $GOROOT. nanogo parses the file the go command listed, which is
// the same file under its real path.
func normalizeFile(name string) string {
	name = filepath.ToSlash(name)
	if root := filepath.ToSlash(goroot()); root != "" && strings.HasPrefix(name, root+"/") {
		return "$GOROOT" + name[len(root):]
	}
	return name
}

var cachedGoroot string

func goroot() string {
	if cachedGoroot == "" {
		out, err := exec.Command("go", "env", "GOROOT").Output()
		if err != nil {
			return ""
		}
		cachedGoroot = strings.TrimSpace(string(out))
	}
	return cachedGoroot
}

// @@@ The source side

// sourcePackage is one package's source and the archives its imports are in.
type sourcePackage struct {
	path  string
	dir   string
	files []string
	arch  string
}

// sourcePackages lists the named packages with their source files, and the
// archive of every package they import.
func sourcePackages(t *testing.T, dir string, paths ...string) ([]sourcePackage, map[string]string) {
	t.Helper()
	const format = "{{.ImportPath}}\t{{.Export}}\t{{.Dir}}\t{{join .GoFiles \" \"}}\t{{join .CgoFiles \"\"}}\t{{.Incomplete}}"
	args := append([]string{"list", "-deps", "-export", "-f", format}, paths...)
	cmd := exec.Command(goTool(t), args...)
	cmd.Dir = dir
	cmd.Env = env()
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Skipf("go list -deps -export: %v\n%s", err, stderr)
	}

	want := expandPatterns(t, dir, paths)
	archives := make(map[string]string)
	var list []sourcePackage
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			continue
		}
		path, export, pdir, gofiles, cgofiles, incomplete := f[0], f[1], f[2], f[3], f[4], f[5]
		if export != "" {
			archives[path] = export
		}
		// A package with cgo files, or one the go command could not
		// describe, is not a package nanogo parses.
		if !want[path] || cgofiles != "" || incomplete == "true" || gofiles == "" {
			continue
		}
		var files []string
		for _, name := range strings.Fields(gofiles) {
			files = append(files, filepath.Join(pdir, name))
		}
		list = append(list, sourcePackage{path: path, dir: pdir, files: files, arch: export})
	}
	return list, archives
}

// expandPatterns resolves the patterns on a go list command line to the set of
// import paths they name, so that "std" is every standard library package and
// not a package of that name.
func expandPatterns(t *testing.T, dir string, paths []string) map[string]bool {
	t.Helper()
	cmd := exec.Command(goTool(t), append([]string{"list", "-f", "{{.ImportPath}}"}, paths...)...)
	cmd.Dir = dir
	cmd.Env = env()
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	want := make(map[string]bool)
	for _, line := range strings.Fields(string(out)) {
		want[line] = true
	}
	return want
}

// archiveImporter resolves an import to the package gc's archive holds.
type archiveImporter struct {
	reader   *Reader
	archives map[string]string
	cache    map[string]*types2.Package
}

func (i *archiveImporter) Import(path string) (*types2.Package, error) {
	if pkg, ok := i.cache[path]; ok {
		return pkg, nil
	}
	if path == "unsafe" {
		return types2.Unsafe, nil
	}
	file, ok := i.archives[path]
	if !ok || file == "" {
		// A standard library package that imports golang.org/x/... gets the
		// copy under GOROOT/src/vendor, which the go command lists under
		// its vendor path.
		file, ok = i.archives["vendor/"+path]
	}
	if !ok || file == "" {
		return nil, fmt.Errorf("no archive for %q", path)
	}
	pkg, err := i.reader.Read(path, file)
	if err != nil {
		return nil, err
	}
	i.cache[path] = pkg
	return pkg, nil
}

// checkSource parses and type checks one package the way the driver does.
func checkSource(sp sourcePackage, imp types2.Importer) (*types2.Package, *types2.Info, *syntax.FileSet, []*syntax.File, error) {
	fset := syntax.NewFileSet()
	var files []*syntax.File
	for _, name := range sp.files {
		f, err := syntax.ParseFile(fset, name, nil, nil, 0)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		files = append(files, f)
	}
	info := &types2.Info{
		Types:        make(map[syntax.Expr]types2.TypeAndValue),
		Defs:         make(map[*syntax.Name]types2.Object),
		Uses:         make(map[*syntax.Name]types2.Object),
		Implicits:    make(map[syntax.Node]types2.Object),
		Selections:   make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:       make(map[syntax.Node]*types2.Scope),
		Instances:    make(map[*syntax.Name]types2.Instance),
		FileVersions: make(map[*syntax.SrcFile]string),
	}
	conf := types2.Config{
		Fset:     fset,
		Sizes:    types2.SizesFor("gc", "arm64"),
		Importer: imp,
		Error:    func(error) {},
	}
	pkg, err := conf.Check(sp.path, files, info)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return pkg, info, fset, files, nil
}

// declaredFuncs returns each function declaration of a package under the name
// gc's body list gives it.
func declaredFuncs(info *types2.Info, files []*syntax.File) map[string]*syntax.FuncDecl {
	out := make(map[string]*syntax.FuncDecl)
	for _, f := range files {
		for _, d := range f.DeclList {
			fd, ok := d.(*syntax.FuncDecl)
			if !ok || fd.Name.Value == "_" {
				continue
			}
			obj, _ := info.Defs[fd.Name].(*types2.Func)
			if obj == nil {
				continue
			}
			sig := obj.Signature()
			name := obj.Name()
			if recv := sig.Recv(); recv != nil {
				typ := recv.Type()
				if p, isPtr := typ.(*types2.Pointer); isPtr {
					typ = p.Elem()
				}
				named, ok := types2.Unalias(typ).(*types2.Named)
				if !ok {
					continue
				}
				name = methodSym(named, obj)
			}
			out[name] = fd
		}
	}
	return out
}

// @@@ The comparison

// buildCensus counts what one run of the oracle compared.
type buildCensus struct {
	packages int

	// matched is the number of body elements built and encoded to the same
	// bytes as gc's, and mismatched is the number that encoded to other
	// bytes. refused is the number the builder declined by name, which is
	// the part of the format it does not build yet.
	matched    int
	mismatched int
	refused    map[string]int

	// stmts and exprs count the encodings the bodies that matched used, so
	// that an encoding no standard library body exercises is named rather
	// than counted as proved. The corpus only holds the bodies gc chose to
	// export, so a shape it does not hold is a shape this oracle is blind
	// to.
	stmts map[StmtKind]int
	exprs map[ExprKind]int

	failures []string
}

func newBuildCensus() *buildCensus {
	return &buildCensus{
		refused: make(map[string]int),
		stmts:   make(map[StmtKind]int),
		exprs:   make(map[ExprKind]int),
	}
}

func (c *buildCensus) fail(format string, args ...any) {
	c.failures = append(c.failures, fmt.Sprintf(format, args...))
}

func (c *buildCensus) report(t *testing.T) {
	t.Helper()
	t.Logf("built %d body elements of %d packages to gc's own bytes; %d differed and %d were refused",
		c.matched, c.packages, c.mismatched, total(c.refused))
	reasons := make([]string, 0, len(c.refused))
	for r := range c.refused {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		t.Logf("refused %d time(s): %s", c.refused[r], r)
	}
	var unused []string
	for k := StmtKind(0); k < numStmtKinds; k++ {
		if k != StmtEnd && c.stmts[k] == 0 {
			unused = append(unused, "the "+k.String()+" statement")
		}
	}
	for k := ExprKind(0); k < numExprKinds; k++ {
		// The reshape node is folded onto the expression it wraps, so it is
		// never a node of its own.
		if k != ExprReshape && c.exprs[k] == 0 {
			unused = append(unused, "the "+k.String()+" expression")
		}
	}
	if len(unused) != 0 {
		t.Logf("no body that matched used %s, so the oracle says nothing about it", strings.Join(unused, ", "))
	}
	for _, f := range c.failures {
		t.Errorf("%s", f)
	}
}

// count records the encodings one built body used.
func (c *buildCensus) count(b *Body) {
	if b == nil {
		return
	}
	c.countStmts(b.Stmts)
}

func (c *buildCensus) countStmts(list []Stmt) {
	for _, s := range list {
		c.stmts[s.stmtKind()]++
		switch s := s.(type) {
		case *BlockStmt:
			c.countStmts(s.Body)
		case *ExprStmt:
			c.countExpr(s.X)
		case *SendStmt:
			c.countExpr(s.Chan)
			c.countExpr(s.Value)
		case *AssignStmt:
			c.countAssign(s.Lhs)
			c.countMulti(s.Rhs)
		case *AssignOpStmt:
			c.countExpr(s.Lhs)
			c.countExpr(s.Rhs)
		case *IncDecStmt:
			c.countExpr(s.X)
		case *CallStmt:
			c.countExpr(s.Call)
			c.countExpr(s.DeferAt)
		case *ReturnStmt:
			c.countMulti(s.Results)
		case *IfStmt:
			c.countStmts(s.Init)
			c.countExpr(s.Cond)
			if s.Then != nil {
				c.countStmts(s.Then.Body)
			}
			c.countStmts(s.Else)
		case *ForStmt:
			if s.Range != nil {
				c.countAssign(s.Range.Lhs)
				c.countExpr(s.Range.X)
			}
			c.countStmts(s.Init)
			c.countExpr(s.Cond)
			c.countStmts(s.Post)
			if s.Body != nil {
				c.countStmts(s.Body.Body)
			}
		case *SwitchStmt:
			c.countStmts(s.Init)
			if s.Guard != nil {
				c.countExpr(s.Guard.X)
			}
			c.countExpr(s.Tag)
			for i := range s.Clauses {
				for _, e := range s.Clauses[i].Exprs {
					c.countExpr(e)
				}
				c.countStmts(s.Clauses[i].Body)
			}
		case *SelectStmt:
			for i := range s.Clauses {
				c.countStmts(s.Clauses[i].Comm)
				c.countStmts(s.Clauses[i].Body)
			}
		}
	}
}

func (c *buildCensus) countAssign(list []Assignee) {
	for _, a := range list {
		c.countExpr(a.Expr)
	}
}

func (c *buildCensus) countMulti(m MultiExpr) {
	c.countExpr(m.Expr)
	for _, e := range m.Exprs {
		c.countExpr(e)
	}
}

func (c *buildCensus) countExpr(e Expr) {
	if e == nil {
		return
	}
	c.exprs[e.exprKind()]++
	switch e := e.(type) {
	case *CompLitExpr:
		for i := range e.Elems {
			c.countExpr(e.Elems[i].Key)
			c.countExpr(e.Elems[i].Value)
		}
	case *FuncLitExpr:
		c.count(e.Decoded)
	case *FieldValExpr:
		c.countExpr(e.X)
	case *MethodValExpr:
		c.countExpr(e.Recv)
	case *RecvExpr:
		c.countExpr(e.X)
	case *IndexExpr:
		c.countExpr(e.X)
		c.countExpr(e.Index)
	case *SliceExpr:
		c.countExpr(e.X)
		for _, x := range e.Index {
			c.countExpr(x)
		}
	case *AssertExpr:
		c.countExpr(e.X)
	case *UnaryExpr:
		c.countExpr(e.X)
	case *BinaryExpr:
		c.countExpr(e.X)
		c.countExpr(e.Y)
	case *CallExpr:
		if e.Method != nil {
			c.countExpr(e.Method.Recv)
		}
		c.countExpr(e.Fun)
		c.countMulti(e.Args)
	case *ConvertExpr:
		c.countExpr(e.X)
	case *NewExpr:
		c.countExpr(e.Value)
	case *MakeExpr:
		for _, a := range e.Args {
			c.countExpr(a)
		}
	}
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// compareBody encodes gc's tree and the built tree, and compares the two,
// following the function literals both name.
func compareBody(version pkgbits.Version, path, name string, want, got *Body) error {
	pe := pkgbits.NewPkgEncoder(version)
	wantRefs, gotRefs := newSymRefs(), newSymRefs()

	type pair struct {
		name string
		want *Body
		got  *Body
	}
	queue := []pair{{name, want, got}}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]

		wantEnc, wantNested, err := encodeOne(&pe, wantRefs, path, p.name, p.want)
		if err != nil {
			return fmt.Errorf("gc's own tree for %s does not encode: %v", p.name, err)
		}
		gotEnc, gotNested, err := encodeOne(&pe, gotRefs, path, p.name, p.got)
		if err != nil {
			return err
		}
		if err := compareEncodings(wantEnc, gotEnc); err != nil {
			return fmt.Errorf("%s: %v", p.name, err)
		}
		if len(wantNested) != len(gotNested) {
			return fmt.Errorf("%s: the body names %d function literals and gc's names %d", p.name, len(gotNested), len(wantNested))
		}
		for i := range wantNested {
			queue = append(queue, pair{
				name: p.name + ", in a function literal",
				want: wantNested[i].Decoded,
				got:  gotNested[i].Decoded,
			})
		}
	}
	return nil
}

// compareEncodings compares two encodings of what should be one tree.
func compareEncodings(want, got *pkgbits.Encoder) error {
	if len(got.Relocs) != len(want.Relocs) {
		return fmt.Errorf("the body makes %d references and gc's makes %d", len(got.Relocs), len(want.Relocs))
	}
	for i := range want.Relocs {
		if got.Relocs[i] != want.Relocs[i] {
			return fmt.Errorf("reference %d of the table is %v %d and gc's is %v %d",
				i, got.Relocs[i].Kind, got.Relocs[i].Idx, want.Relocs[i].Kind, want.Relocs[i].Idx)
		}
	}
	g, w := got.Data.Bytes(), want.Data.Bytes()
	if bytes.Equal(g, w) {
		return nil
	}
	for i := range min(len(g), len(w)) {
		if g[i] != w[i] {
			return fmt.Errorf("byte %d of %d is %#x and gc's is %#x", i, len(w), g[i], w[i])
		}
	}
	return fmt.Errorf("the body is %d bytes and gc's is %d", len(g), len(w))
}

// checkPackage builds every body gc exported for one package and compares it
// with gc's.
func (c *buildCensus) checkPackage(t *testing.T, sp sourcePackage, imp types2.Importer) {
	t.Helper()
	pr, bodies, err := readArchiveBodies(sp.path, sp.arch)
	if err != nil {
		// The reader's census, not this one's. TestReadBodies reports it.
		return
	}
	root := pr.NewDecoderRaw(pkgbits.SectionMeta, pkgbits.PrivateRootIdx)
	version := root.Version()

	pkg, info, fset, files, err := checkSource(sp, imp)
	if err != nil {
		c.fail("%s: cannot check the source: %v", sp.path, err)
		return
	}
	c.packages++

	decls := declaredFuncs(info, files)
	src := NewBodySource(pkg, info, fset)
	for _, fb := range bodies {
		// An archive re-exports the inlinable bodies of the packages it
		// imports, and this package's source holds none of those.
		if fb.Path != sp.path {
			continue
		}
		fd, ok := decls[fb.Name]
		if !ok {
			// A body of a declaration this package's source does not hold
			// under that name, which is a method promoted from an embedded
			// field or a declaration in a file the build did not list.
			continue
		}
		obj, _ := info.Defs[fd.Name].(*types2.Func)
		if obj == nil {
			continue
		}
		got, err := src.BuildBody(fb.Path+"."+fb.Name, obj.Signature(), fd.Body)
		if err != nil {
			e, ok := err.(*BodyError)
			if !ok {
				c.fail("%s: %s: %v", sp.path, fb.Name, err)
				continue
			}
			c.refused[e.Reason]++
			continue
		}
		if err := compareBody(version, sp.path, fb.Path+"."+fb.Name, fb.Body, got); err != nil {
			c.mismatched++
			c.fail("%s: %s: %v", sp.path, fb.Name, err)
			continue
		}
		c.matched++
		c.count(got)
	}
}

// buildPackages is what an unattended run builds.
var buildPackages = []string{
	"bytes", "errors", "io", "math", "math/bits", "sort", "strconv",
	"strings", "sync", "unicode/utf8",
}

// TestBuildBodies is the oracle the body builder is measured by.
//
// The gate is that nothing differs. A body the builder declines by name is
// counted and is not a failure: it names the part of the encoding the builder
// does not build yet, and a refusal is what the writer turns into a
// declaration it does not export. A body that differs is a wrong answer, and
// gc is the reader.
func TestBuildBodies(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/build\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := buildPackages
	if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
		want = []string{"std"}
	}
	list, archives := sourcePackages(t, dir, want...)
	if len(list) == 0 {
		t.Fatal("no package was listed, so the test proved nothing")
	}

	c := newBuildCensus()
	for _, sp := range list {
		imp := &archiveImporter{
			reader:   NewReader(),
			archives: archives,
			cache:    make(map[string]*types2.Package),
		}
		c.checkPackage(t, sp, imp)
	}
	c.report(t)
	if c.matched == 0 {
		t.Error("no body was built to gc's bytes, so the test proved nothing")
	}
}

// @@@ Refusals

// checkOne parses and type checks one source file as the package a.
func checkOne(t *testing.T, src string) (*types2.Package, *types2.Info, *syntax.FileSet, *syntax.File) {
	t.Helper()
	dir := t.TempDir()
	name := filepath.Join(dir, "a.go")
	if err := os.WriteFile(name, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := syntax.NewFileSet()
	file, err := syntax.ParseFile(fset, name, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types2.Info{
		Types:        make(map[syntax.Expr]types2.TypeAndValue),
		Defs:         make(map[*syntax.Name]types2.Object),
		Uses:         make(map[*syntax.Name]types2.Object),
		Implicits:    make(map[syntax.Node]types2.Object),
		Selections:   make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:       make(map[syntax.Node]*types2.Scope),
		Instances:    make(map[*syntax.Name]types2.Instance),
		FileVersions: make(map[*syntax.SrcFile]string),
	}
	conf := types2.Config{Fset: fset, Sizes: types2.SizesFor("gc", "arm64")}
	pkg, err := conf.Check("a", []*syntax.File{file}, info)
	if err != nil {
		t.Fatalf("the source does not check: %v", err)
	}
	return pkg, info, fset, file
}

// buildOne checks one source file and builds the body of its function f.
func buildOne(t *testing.T, src string) error {
	t.Helper()
	_, err := buildOneBody(t, src)
	return err
}

// buildOneBody checks one source file and returns the body of its function f.
func buildOneBody(t *testing.T, src string) (*Body, error) {
	t.Helper()
	pkg, info, fset, file := checkOne(t, src)
	for _, d := range file.DeclList {
		fd, ok := d.(*syntax.FuncDecl)
		if !ok || fd.Name.Value != "f" {
			continue
		}
		obj := info.Defs[fd.Name].(*types2.Func)
		return NewBodySource(pkg, info, fset).BuildBody("a.f", obj.Signature(), fd.Body)
	}
	t.Fatal("the source declares no function f")
	return nil, nil
}

// TestBuildBodyRefusals checks that a body the builder does not build is
// refused by name.
//
// The corpus proves what the builder builds and cannot prove what it declines:
// a shape built wrong that the corpus does not hold would reach gc as a body
// whose fields have all moved. Each of these is a shape gc writes and the
// builder does not, and each has to say so.
func TestBuildBodyRefusals(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{{
		name: "a method of a generic type",
		src:  "package a\n\ntype T[X any] struct{ x X }\n\nfunc (t T[X]) f() X { return t.x }\n",
		want: "the declaration is a method of a generic type",
	}, {
		name: "a type declared inside a generic declaration",
		src:  "package a\n\nfunc f[T any](x T) any { type s struct{ v T }; return s{x} }\n",
		want: "carries the enclosing type parameters",
	}, {
		name: "a loop over a function",
		src:  "package a\n\nfunc g(yield func(int) bool) {}\n\nfunc f() { for range g {} }\n",
		want: "the loop ranges over a function",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := buildOne(t, tt.src)
			if err == nil {
				t.Fatalf("the builder built the body, and %s is a shape it does not build", tt.name)
			}
			e, ok := err.(*BodyError)
			if !ok {
				t.Fatalf("the refusal is a %T and not a *BodyError: %v", err, err)
			}
			if !strings.Contains(e.Reason, tt.want) {
				t.Errorf("the refusal is %q and does not name %q", e.Reason, tt.want)
			}
			if !strings.Contains(e.Error(), "a.f") {
				t.Errorf("the refusal is %q and does not name the declaration", e.Error())
			}
		})
	}
}

// TestBuildBodyFillsTheDictionary checks the slots a generic declaration's
// body names.
//
// The corpus compares two encodings of one declaration, so it proves that the
// slot a body names is the slot gc names. What it cannot prove is what the
// slot holds: a dictionary with an entry too many, or with the entries in
// another order, encodes a body to the same bytes. This names the entries.
func TestBuildBodyFillsTheDictionary(t *testing.T) {
	src := "package a\n\nfunc f[T any](x T) T {\n\tvar y []T\n\t_ = y\n\treturn x\n}\n"
	body, err := buildOneBody(t, src)
	if err != nil {
		t.Fatalf("the builder refused a generic declaration: %v", err)
	}
	d := body.Dict
	if d == nil {
		t.Fatal("the body carries no dictionary")
	}
	if len(d.TypeParams) != 1 || d.TypeParams[0].Obj().Name() != "T" {
		t.Fatalf("the dictionary holds %d type parameters and the declaration has one", len(d.TypeParams))
	}
	// T takes the first slot, because the parameter names it before the
	// body does, and []T the second, because gc writes the element of the
	// type a slice names before the element of the slice.
	want := []string{"T", "[]T"}
	got := make([]string, len(d.Derived))
	for i, typ := range d.Derived {
		got[i] = types2.TypeString(typ, nil)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the derived types are %v and gc's order is %v", got, want)
	}
	// x is of type T and y of type []T, and each needs its runtime type to
	// lay the frame out.
	if len(d.RTypes) != 2 {
		t.Errorf("the dictionary holds %d runtime types and the body needs two", len(d.RTypes))
	}
	if len(d.Itabs)+len(d.Subdicts)+len(d.MethodExprs) != 0 {
		t.Errorf("the body names no method table, no subdictionary and no method expression, and the dictionary holds %d, %d and %d",
			len(d.Itabs), len(d.Subdicts), len(d.MethodExprs))
	}
}

// TestBuildTypeBodiesShareOneDictionary checks the numbering a generic type
// and its methods are read back with.
//
// The whole type is built in one call because the slots interleave: the
// underlying type takes the first of them, and then each method takes the
// slots of its receiver, of its signature and of its body before the next
// method takes any. A method built on its own would number its first slot
// where the type's last one is.
func TestBuildTypeBodiesShareOneDictionary(t *testing.T) {
	src := "package a\n\n" +
		"type List[T any] struct{ items []T }\n\n" +
		"func (l List[T]) Head() T { return l.items[0] }\n\n" +
		"func (l *List[T]) Push(v T) { l.items = append(l.items, v) }\n"
	pkg, info, fset, file := checkOne(t, src)
	named := pkg.Scope().Lookup("List").Type().(*types2.Named)
	blocks := make(map[*types2.Func]*syntax.BlockStmt)
	for _, fd := range declaredFuncs(info, []*syntax.File{file}) {
		obj := info.Defs[fd.Name].(*types2.Func)
		if obj.Signature().RecvTypeParams().Len() != 0 {
			blocks[obj] = fd.Body
		}
	}
	dict, bodies, err := NewBodySource(pkg, info, fset).BuildTypeBodies(named, blocks)
	if err != nil {
		t.Fatalf("BuildTypeBodies: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("the builder built %d bodies and the type declares two methods", len(bodies))
	}
	for i, b := range bodies {
		if b.Dict != dict {
			t.Errorf("the body of %s carries a dictionary of its own", named.Method(i).Name())
		}
		if !b.HasBlock {
			t.Errorf("the body of %s carries no block", named.Method(i).Name())
		}
	}
	if len(dict.TypeParams) != 1 || len(dict.Receivers) != 0 || len(dict.Implicits) != 0 {
		t.Fatalf("the dictionary holds %d type parameters, %d receiver ones and %d implicit ones, and the type declares one of the first",
			len(dict.TypeParams), len(dict.Receivers), len(dict.Implicits))
	}
	// The underlying type first, then, for each method, the receiver, the
	// signature and then the body. Head's body names the field, whose type
	// is a slot the body takes and not one the signature took, and Push's
	// body names it again, so a body built on its own would number its
	// slots where the next method's receiver goes.
	//
	// Each method's receiver names a type parameter the method declared,
	// which is not the object the type declared, so the checker built a
	// value of its own for List[T] under each method. gc keys the dictionary
	// by the same identity ([Dict.Derive]) and allocates the same slots.
	qual := func(p *types2.Package) string { return p.Name() }
	want := []string{
		"T", "[]T", "struct{items []T}",
		"T", "a.List[T]", "[]T",
		"T", "a.List[T]", "*a.List[T]", "[]T",
	}
	got := make([]string, len(dict.Derived))
	for i, typ := range dict.Derived {
		got[i] = types2.TypeString(typ, qual)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the derived types are %v and gc's order is %v", got, want)
	}
	// Every T of the three resolves to the type's own position, because the
	// dictionary an instantiation is passed holds one entry per type
	// parameter the type declares.
	for i, typ := range dict.Derived {
		tp, ok := typ.(*types2.TypeParam)
		if !ok {
			continue
		}
		if idx, ok := dict.TypeParamIndex(tp); !ok || idx != 0 {
			t.Errorf("slot %d is a type parameter numbered %d and the type declares one", i, idx)
		}
	}
}

// TestBuildTypeBodiesNumbersEachTypeParameterByItsPosition is the two
// parameter case, which is the one that measures the rule.
//
// A method names the receiver's type parameters through objects of its own
// declaration, so the position and nothing else says which one a name is. With
// one type parameter every position is zero and a rule that returned zero
// would pass, so the type here declares two and each is checked by the name
// the type declared it under.
func TestBuildTypeBodiesNumbersEachTypeParameterByItsPosition(t *testing.T) {
	src := "package a\n\n" +
		"type Pair[K, V any] struct {\n\tk K\n\tv V\n}\n\n" +
		"func (p Pair[K, V]) Key() K { return p.k }\n\n" +
		"func (p *Pair[K, V]) Set(k K, v V) { p.k, p.v = k, v }\n"
	pkg, info, fset, file := checkOne(t, src)
	named := pkg.Scope().Lookup("Pair").Type().(*types2.Named)
	blocks := make(map[*types2.Func]*syntax.BlockStmt)
	for _, fd := range declaredFuncs(info, []*syntax.File{file}) {
		obj := info.Defs[fd.Name].(*types2.Func)
		if obj.Signature().RecvTypeParams().Len() != 0 {
			blocks[obj] = fd.Body
		}
	}
	dict, _, err := NewBodySource(pkg, info, fset).BuildTypeBodies(named, blocks)
	if err != nil {
		t.Fatalf("BuildTypeBodies: %v", err)
	}
	want := map[string]int{"K": 0, "V": 1}
	seen := 0
	for i, typ := range dict.Derived {
		tp, ok := typ.(*types2.TypeParam)
		if !ok {
			continue
		}
		name := tp.Obj().Name()
		idx, ok := dict.TypeParamIndex(tp)
		if !ok {
			t.Errorf("slot %d holds %s and the dictionary resolves no position for it", i, name)
			continue
		}
		if idx != want[name] {
			t.Errorf("slot %d holds %s, numbered %d, and the type declares it at %d", i, name, idx, want[name])
		}
		seen++
	}
	if seen < 4 {
		t.Errorf("the dictionary holds %d type parameter slots, and the type's own two plus each method's copy is more", seen)
	}
}

// TestBuildTypeBodiesRefusesAMethodWithNoBody checks that a type whose
// dictionary would be short by a method's slots is refused.
func TestBuildTypeBodiesRefusesAMethodWithNoBody(t *testing.T) {
	src := "package a\n\ntype List[T any] struct{ items []T }\n\nfunc (l List[T]) Head() T { return l.items[0] }\n"
	pkg, info, fset, _ := checkOne(t, src)
	named := pkg.Scope().Lookup("List").Type().(*types2.Named)
	_, _, err := NewBodySource(pkg, info, fset).BuildTypeBodies(named, nil)
	if err == nil {
		t.Fatal("the builder built a type whose method has no body")
	}
	e, ok := err.(*BodyError)
	if !ok {
		t.Fatalf("the refusal is a %T and not a *BodyError: %v", err, err)
	}
	if !strings.Contains(e.Reason, "no body was offered") {
		t.Errorf("the refusal is %q and does not say the body is missing", e.Reason)
	}
	if !strings.Contains(e.Error(), "a.List.Head") {
		t.Errorf("the refusal is %q and does not name the method", e.Error())
	}
}

// TestBuildBodyOfAnOrdinaryDeclarationHasNoDictionary checks that a
// declaration with no type parameter names no slot.
func TestBuildBodyOfAnOrdinaryDeclarationHasNoDictionary(t *testing.T) {
	body, err := buildOneBody(t, "package a\n\nfunc f(x int) int { return x }\n")
	if err != nil {
		t.Fatal(err)
	}
	d := body.Dict
	if d == nil {
		t.Fatal("the body carries no dictionary")
	}
	if d.Generic() {
		t.Error("the declaration declares no type parameter and the dictionary says it does")
	}
	if n := len(d.Derived) + len(d.RTypes) + len(d.Itabs) + len(d.Subdicts) + len(d.MethodExprs); n != 0 {
		t.Errorf("the dictionary holds %d entries and an ordinary declaration needs none", n)
	}
	for i, l := range body.Params {
		if l.DictRType != -1 {
			t.Errorf("parameter %d holds dictionary slot %d and its type needs none", i, l.DictRType)
		}
	}
}
