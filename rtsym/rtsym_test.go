// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtsym

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// runtimeSigs reads the Go runtime's source and returns every top-level
// function's signature, keyed by name.
//
// The runtime's source is the only oracle available. These symbols are all
// unexported, so none of them appears in export data and go/importer cannot
// find any of them; a probe against the installed toolchain returns nothing for
// every one.
//
// Signatures are normalised the way the table stores them: parameter names
// removed, type spelling kept exactly. A spelling change is reported as a
// difference on purpose. It is a signal to look, not noise.
func runtimeSigs(t *testing.T) map[string]string {
	t.Helper()

	dir := filepath.Join(runtime.GOROOT(), "src", "runtime")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	fset := token.NewFileSet()
	sigs := make(map[string]string)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			// A file that does not parse for this platform is not this test's
			// problem. The parser corpus in syntax/ covers parsing.
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			// A name may be declared once per build tag. Keep the first, and
			// let the comparison below report a real difference if the one the
			// table records is a different variant.
			if _, seen := sigs[fn.Name.Name]; !seen {
				sigs[fn.Name.Name] = normalizeSig(fset, fn.Type)
			}
		}
	}

	// A symbol whose linker name is runtime.X need not be declared in package
	// runtime. cmpstring is written in internal/bytealg and reaches the
	// runtime's namespace through //go:linkname. Resolving those keeps the
	// table checked against real source rather than against an assertion.
	//
	// They fill gaps and never overwrite. A symbol declared in both places is
	// spelled differently in each: internal/runtime/maps defines mapassign as
	// taking a *Map and package runtime declares it as taking a *maps.Map,
	// because the two write the same type from inside and outside the package
	// that owns it. The table records one vocabulary, and package runtime's is
	// the one every other row is already in.
	for name, sig := range linknamedIntoRuntime(fset) {
		if _, seen := sigs[name]; !seen {
			sigs[name] = sig
		}
	}
	return sigs
}

// normalizeSig prints a signature with the parameter names removed.
func normalizeSig(fset *token.FileSet, ft *ast.FuncType) string {
	stripNames(ft.Params)
	stripNames(ft.Results)
	var b strings.Builder
	if err := printer.Fprint(&b, fset, ft); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func stripNames(fl *ast.FieldList) {
	if fl == nil {
		return
	}
	var out []*ast.Field
	for _, f := range fl.List {
		n := 1
		if len(f.Names) > 0 {
			n = len(f.Names)
		}
		for i := 0; i < n; i++ {
			out = append(out, &ast.Field{Type: f.Type})
		}
	}
	fl.List = out
}

func requireRuntime(t *testing.T, sigs map[string]string) {
	t.Helper()
	if len(sigs) > 0 {
		return
	}
	if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
		t.Fatal("NANOGO_REQUIRE_CORPUS=1 and the runtime source under GOROOT could not be read")
	}
	t.Skip("no runtime source under GOROOT")
}

// TestTableMatchesTheRuntime is the reason this package exists.
//
// specs/031-runtime-lowering.md: a signature that drifts from the runtime is a
// crash with no diagnostic, and a checked table turns that into a build
// failure. Without this test the table is a list of guesses.
func TestTableMatchesTheRuntime(t *testing.T) {
	sigs := runtimeSigs(t)
	requireRuntime(t, sigs)

	var missing, differs int
	for _, s := range All() {
		if s.Assembly {
			// No Go declaration exists to compare against.
			// TestAssemblySymbolsExist checks these instead.
			continue
		}
		got, ok := sigs[s.Base()]
		if !ok {
			// A symbol implemented only in assembly has no Go declaration in
			// this directory. Report it rather than pass silently, because a
			// name that no longer exists at all is the failure this test is
			// for.
			t.Errorf("%s is not declared in the runtime's Go source", s.Name)
			missing++
			continue
		}
		if got != s.Sig {
			t.Errorf("%s\n  table:   %s\n  runtime: %s", s.Name, s.Sig, got)
			differs++
		}
	}
	t.Logf("checked %d symbols against %d runtime functions, %d missing, %d differing",
		len(All()), len(sigs), missing, differs)
}

// TestNoReturnSymbolsAreReallyNoReturn checks the flag that liveness depends
// on. specs/031: a block after a call to one of these is unreachable, and
// liveness that thinks otherwise keeps values alive across every bounds check
// in the program.
//
// The runtime does not mark this in a way a parser can read, so the check is
// the next best thing: a function that never returns has no results, and a
// panic function's name says what it does.
func TestNoReturnSymbolsAreReallyNoReturn(t *testing.T) {
	sigs := runtimeSigs(t)
	requireRuntime(t, sigs)

	for _, s := range All() {
		if !s.NoReturn {
			continue
		}
		if strings.Contains(s.Sig, ")") && strings.TrimSpace(s.Sig[strings.LastIndex(s.Sig, ")")+1:]) != "" {
			t.Errorf("%s is marked no-return and has results: %s", s.Name, s.Sig)
		}
		if !strings.Contains(strings.ToLower(s.Base()), "panic") {
			t.Errorf("%s is marked no-return and is not a panic function; check it by hand", s.Name)
		}
	}
}

func TestLookupAndAll(t *testing.T) {
	if got := Lookup("runtime.newobject"); got == nil || got.Group != GroupAlloc {
		t.Fatalf("Lookup(runtime.newobject) = %#v", got)
	}
	if Lookup("runtime.nosuchfunction") != nil {
		t.Error("Lookup found a symbol that is not in the table")
	}

	all := All()
	if len(all) != len(syms) {
		t.Errorf("All returned %d symbols, the table holds %d", len(all), len(syms))
	}
	if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].Name < all[j].Name }) {
		t.Error("All is not sorted; a caller emitting relocations would not be deterministic")
	}

	// All must copy. A caller that sorts or mutates the result must not be able
	// to reorder the table underneath everyone else.
	all[0].Name = "mutated"
	if All()[0].Name == "mutated" {
		t.Error("All returned a view of the table rather than a copy")
	}
}

func TestEverySymbolIsWellFormed(t *testing.T) {
	seen := make(map[string]bool)
	for _, s := range All() {
		if !strings.HasPrefix(s.Name, "runtime.") {
			t.Errorf("%s does not carry the runtime prefix", s.Name)
		}
		if s.Base() == "" {
			t.Errorf("%s has an empty base name", s.Name)
		}
		if !strings.HasPrefix(s.Sig, "func(") {
			t.Errorf("%s has a signature that is not a function: %q", s.Name, s.Sig)
		}
		if s.Group == GroupInvalid {
			t.Errorf("%s has no group", s.Name)
		}
		if seen[s.Name] {
			t.Errorf("%s appears twice in the table", s.Name)
		}
		seen[s.Name] = true
	}
}

func TestGroupStrings(t *testing.T) {
	for g := GroupAlloc; g <= GroupPrint; g++ {
		if got := g.String(); got == "" || got == "group(?)" {
			t.Errorf("group %d prints %q", g, got)
		}
	}
	if got := Group(200).String(); got != "group(?)" {
		t.Errorf("an out-of-range group prints %q", got)
	}
	if got := GroupInvalid.String(); got != "invalid" {
		t.Errorf("GroupInvalid prints %q", got)
	}
}

// TestAssemblySymbolsExist checks the symbols that have no Go declaration.
//
// runtime.morestack_noctxt is written in assembly, so the signature check
// above has nothing to read. What can still be checked is that the symbol is
// defined, and that is worth checking: the function prologue calls it on every
// non-leaf function, so a rename would break every compiled program at link
// time with an error naming a symbol nobody wrote by hand.
func TestAssemblySymbolsExist(t *testing.T) {
	dir := filepath.Join(runtime.GOROOT(), "src", "runtime")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and the runtime source could not be read: %v", err)
		}
		t.Skip("no runtime source under GOROOT")
	}

	var asm strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".s") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		asm.Write(b)
	}
	if asm.Len() == 0 {
		t.Skip("no runtime assembly found")
	}
	text := asm.String()

	n := 0
	for _, s := range All() {
		if !s.Assembly {
			continue
		}
		n++
		// A TEXT directive names the symbol with the middle dot that Plan 9
		// assembly uses for the package separator, and may put an ABI tag
		// between the name and the (SB). morestack_noctxt carries none and
		// every gcWriteBarrier carries <ABIInternal>, so a plain substring
		// test finds one family and not the other.
		if !asmDefines(text, s.Base()) {
			t.Errorf("%s is not defined in the runtime's assembly", s.Name)
		}
	}
	if n == 0 {
		t.Error("no assembly symbol was checked; the table lost its Assembly entries")
	}
	t.Logf("checked %d assembly symbols", n)
}

// asmDefines reports whether the assembly text defines the symbol.
//
// The two spellings are "·name(SB)" and "·name<ABI>(SB)". The ABI is part of
// the symbol's identity to the linker, and it is not checked here: this test
// asserts the symbol exists, and ssagen decides which ABI a reference carries.
func asmDefines(text, base string) bool {
	re := regexp.MustCompile("·" + regexp.QuoteMeta(base) + `(<[A-Za-z0-9]+>)?\(SB\)`)
	return re.MatchString(text)
}

// linknamedIntoRuntime finds every //go:linkname that pushes a declaration
// into package runtime's symbol namespace, and returns the runtime-side name
// mapped to the declaration's signature.
//
// The scan is over GOROOT's whole source tree, which is bounded and on disk.
// The alternative is to record a signature by hand for these symbols, and the
// point of this package is that no signature is recorded by hand.
func linknamedIntoRuntime(fset *token.FileSet) map[string]string {
	out := map[string]string{}
	root := filepath.Join(runtime.GOROOT(), "src")

	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(src), "//go:linkname") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil
		}

		// Collect the directives first: a linkname may sit anywhere in the
		// file, not only above the declaration it names.
		local := map[string]string{} // local name -> runtime-side base name
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				fields := strings.Fields(c.Text)
				if len(fields) != 3 || fields[0] != "//go:linkname" {
					continue
				}
				if !strings.HasPrefix(fields[2], "runtime.") {
					continue
				}
				local[fields[1]] = strings.TrimPrefix(fields[2], "runtime.")
			}
		}
		if len(local) == 0 {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if base, ok := local[fn.Name.Name]; ok {
				if _, seen := out[base]; !seen {
					out[base] = normalizeSig(fset, fn.Type)
				}
			}
		}
		return nil
	})
	return out
}

// runtimeVarTypes reads the Go runtime's source and returns the type of every
// package-level variable that has one written down, keyed by name.
//
// A variable declared with a value and no type carries its type in the value
// when the value is a composite literal, because a composite literal names the
// type it builds. runtime.emptyTypeAssertCache is written that way and the
// compiler points a descriptor's Cache field at it, so the type is read from
// the literal.
//
// Nothing else is inferred. A variable whose type is neither written on the
// declaration nor named by a composite literal is absent, which is what the
// table wants: the type would have to be resolved rather than read, and a
// resolved type is not what this file compares against.
func runtimeVarTypes(t *testing.T) map[string]string {
	t.Helper()

	dir := filepath.Join(runtime.GOROOT(), "src", "runtime")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	out := make(map[string]string)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					typ := vs.Type
					if typ == nil {
						typ = compositeLitType(vs, i)
					}
					if typ == nil {
						continue
					}
					if _, seen := out[id.Name]; !seen {
						out[id.Name] = normalizeType(fset, typ)
					}
				}
			}
		}
	}
	return out
}

// normalizeType prints a type with its whitespace collapsed onto one line.
//
// A struct's fields are separated by a newline in the source and by "; " here,
// which is the separator the language accepts for the same declaration written
// on one line. So the recorded spelling is still Go the reader can paste back.
// compositeLitType returns the type a composite literal initialiser names, or
// nil when the i'th value of the declaration is not one.
//
// "var x = T{...}" writes T down as plainly as "var x T" does, and the runtime
// declares both empty caches that way. Anything else is left alone: an
// initialiser that is a call or a conversion states a type the reader would
// have to resolve, and this file compares spellings rather than resolving
// them.
func compositeLitType(vs *ast.ValueSpec, i int) ast.Expr {
	if i >= len(vs.Values) {
		return nil
	}
	lit, ok := vs.Values[i].(*ast.CompositeLit)
	if !ok {
		return nil
	}
	return lit.Type
}

func normalizeType(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return ""
	}
	var parts []string
	for _, line := range strings.Split(b.String(), "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			parts = append(parts, line)
		}
	}
	var out strings.Builder
	for i, p := range parts {
		if i > 0 {
			// A separator between two fields, and a plain space around the
			// braces that open and close the struct.
			if strings.HasSuffix(parts[i-1], "{") || p == "}" {
				out.WriteString(" ")
			} else {
				out.WriteString("; ")
			}
		}
		out.WriteString(p)
	}
	return out.String()
}

// TestVarTableMatchesTheRuntime is TestTableMatchesTheRuntime for the
// variables.
//
// The compiler reads runtime.writeBarrier by offset: it loads the first four
// bytes and branches on them. A field added in front of enabled, or a change
// of the padding that lets the load be 32 bit, is a branch on the wrong bytes
// and a write barrier that is skipped while the collector is marking.
func TestVarTableMatchesTheRuntime(t *testing.T) {
	types := runtimeVarTypes(t)
	if len(types) == 0 {
		if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
			t.Fatal("NANOGO_REQUIRE_CORPUS=1 and the runtime source under GOROOT could not be read")
		}
		t.Skip("no runtime source under GOROOT")
	}
	if len(AllVars()) == 0 {
		t.Fatal("the variable table is empty")
	}
	for _, v := range AllVars() {
		got, ok := types[v.Base()]
		if !ok {
			t.Errorf("%s is not declared in the runtime's Go source", v.Name)
			continue
		}
		if got != v.Type {
			t.Errorf("%s\n  table:   %s\n  runtime: %s", v.Name, v.Type, got)
		}
	}
	t.Logf("checked %d variables against %d runtime variables", len(AllVars()), len(types))
}

// TestTheVariablesInterfacesNeedAreNamed is the gate on the three rows the
// interface descriptors read.
//
// Each is a symbol the compiler writes a relocation to and never a call.
// runtime.zerobase is the address every zero-sized value carries, and the two
// caches are what the Cache field of an *abi.TypeAssert and an
// *abi.InterfaceSwitch point at before the runtime replaces them. A missing
// row here is an emitter that writes a nil where the runtime dereferences.
func TestTheVariablesInterfacesNeedAreNamed(t *testing.T) {
	for _, name := range []string{
		"runtime.zerobase",
		"runtime.emptyTypeAssertCache",
		"runtime.emptyInterfaceSwitchCache",
	} {
		v := LookupVar(name)
		if v == nil {
			t.Errorf("%s is not in the variable table", name)
			continue
		}
		if Lookup(name) != nil {
			t.Errorf("%s is reachable through Lookup, which names functions, and a call to it jumps into data", name)
		}
	}
}

func TestLookupAndAllVars(t *testing.T) {
	if got := LookupVar("runtime.writeBarrier"); got == nil || got.Group != GroupBarrier {
		t.Fatalf("LookupVar(runtime.writeBarrier) = %#v", got)
	}
	if LookupVar("runtime.nosuchvariable") != nil {
		t.Error("LookupVar found a variable that is not in the table")
	}
	// A variable is not a function. Nothing that emits a call may find one
	// through the symbol table, because a call to it jumps into data.
	if Lookup("runtime.writeBarrier") != nil {
		t.Error("a variable is reachable through Lookup, which names functions")
	}
	all := AllVars()
	if len(all) != len(vars) {
		t.Errorf("AllVars returned %d variables, the table holds %d", len(all), len(vars))
	}
	if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].Name < all[j].Name }) {
		t.Error("AllVars is not sorted")
	}
	all[0].Name = "mutated"
	if AllVars()[0].Name == "mutated" {
		t.Error("AllVars returned a view of the table rather than a copy")
	}
	for _, v := range AllVars() {
		if !strings.HasPrefix(v.Name, "runtime.") {
			t.Errorf("%s does not carry the runtime prefix", v.Name)
		}
		if v.Group == GroupInvalid {
			t.Errorf("%s has no group", v.Name)
		}
	}
}

// TestEveryGroupHasASymbol is the gate on a group that names a family the
// table does not carry.
//
// GroupMap and GroupBarrier were both declared and both empty, so the table
// said the compiler could call a map function and named none of them. A group
// with no members is a claim with nothing behind it.
func TestEveryGroupHasASymbol(t *testing.T) {
	seen := make(map[Group]int)
	for _, s := range All() {
		seen[s.Group]++
	}
	for _, v := range AllVars() {
		seen[v.Group]++
	}
	for g := GroupAlloc; g <= GroupPrint; g++ {
		if seen[g] == 0 {
			t.Errorf("the %s group is declared and empty", g)
		}
	}
}

// TestAssemblySymbolsCarryTheAbiTheRuntimeDeclares checks Internal against the
// runtime's own TEXT directives.
//
// An assembly symbol has no Go declaration, so nothing else says which ABI it
// is. The two families differ: runtime.morestack_noctxt is declared with no
// tag and is ABI0, and every runtime.gcWriteBarrier is declared <ABIInternal>.
//
// cmd/link resolves a by-name reference by name and ABI together, so a caller
// that names a barrier under ABI0 refers to a symbol nothing defines, and the
// message names neither the barrier nor the caller. The comment above this
// table has said which family is which since the entries were added; this is
// the check that says it to the compiler.
func TestAssemblySymbolsCarryTheAbiTheRuntimeDeclares(t *testing.T) {
	dir := filepath.Join(runtime.GOROOT(), "src", "runtime")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and the runtime source could not be read: %v", err)
		}
		t.Skip("no runtime source under GOROOT")
	}
	var asm strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".s") {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			asm.Write(b)
		}
	}
	if asm.Len() == 0 {
		t.Skip("no runtime assembly found")
	}

	n := 0
	for _, s := range All() {
		if !s.Assembly {
			continue
		}
		n++
		// The directive is TEXT <pkg>·<name><tag>(SB). The tag is what says
		// the ABI, so the two spellings are searched for by name.
		internal := strings.Contains(asm.String(), "·"+s.Base()+"<ABIInternal>(SB)")
		if internal != s.Internal {
			t.Errorf("%s is declared ABIInternal in the runtime: %v, and the table says %v",
				s.Name, internal, s.Internal)
		}
	}
	if n == 0 {
		t.Error("no assembly symbol was checked; the table lost its Assembly entries")
	}
}
