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
}

func newSymRefs() *symRefs {
	return &symRefs{
		tables: make(map[pkgbits.SectionKind]map[string]pkgbits.Index),
		next:   make(map[pkgbits.SectionKind]pkgbits.Index),
		bodies: make(map[*FuncLitExpr]pkgbits.Index),
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
	return s.intern(pkgbits.SectionType, typeKey(t.Type))
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
		b.WriteString(typeKey(t.Type))
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
func typeKey(t types2.Type) string {
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
	return types2.TypeString(t, func(pkg *types2.Package) string { return pkg.Path() })
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

	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
	}
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

	failures []string
}

func newBuildCensus() *buildCensus { return &buildCensus{refused: make(map[string]int)} }

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
	for _, f := range c.failures {
		t.Errorf("%s", f)
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
