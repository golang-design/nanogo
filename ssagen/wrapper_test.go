// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/ssa/rules"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// checked is one type-checked source file with the IR built from it.
type checked struct {
	pkg  *types2.Package
	ir   *ir.Package
	conv *ir.Converter
	fset *syntax.FileSet
	file string
}

// check parses and type-checks one source file and builds its IR.
//
// It is compile without the SSA half, because a generated function is built
// from a type rather than from a declaration: the test needs the ir.Type that
// carries the method set, not an ir.Func the front end produced.
func check(t *testing.T, src string) *checked {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := syntax.NewFileSet()
	file, err := syntax.ParseFile(fset, path, nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{Fset: fset, Sizes: types2.SizesFor("gc", "arm64")}
	pkg, err := conf.Check("main", []*syntax.File{file}, info)
	if err != nil {
		t.Fatalf("type check: %v", err)
	}
	p, err := ir.Build(pkg, []*syntax.File{file}, info)
	if err != nil {
		t.Fatalf("ir.Build: %v", err)
	}
	return &checked{pkg: pkg, ir: p, conv: ir.NewConverter(), fset: fset, file: path}
}

// namedType returns the ir.Type of a type declared in the checked package.
func (c *checked) namedType(t *testing.T, name string) *ir.Type {
	t.Helper()
	obj, _ := c.pkg.Scope().Lookup(name).(*types2.TypeName)
	if obj == nil {
		t.Fatalf("%s is not a type in the package", name)
	}
	out, err := c.conv.Convert(obj.Type())
	if err != nil {
		t.Fatalf("converting %s: %v", name, err)
	}
	return out
}

// build takes one ir.Func through the whole back end.
//
// It is the pass list of driver.compileFunc, which is what a generated
// function goes through in the compiler: a function built here and a function
// the front end produced are the same input to the same pipeline, and a test
// that skipped ir.Lower would prove nothing about the one the driver runs.
func (c *checked) build(t *testing.T, fn *ir.Func) *compiled {
	t.Helper()
	if _, err := ir.LowerAndCollect(fn); err != nil {
		t.Fatalf("ir.Lower %s: %v", fn.Sym, err)
	}
	f, err := ssa.Build(fn)
	if err != nil {
		t.Fatalf("ssa.Build %s: %v", fn.Sym, err)
	}
	target := ssa.NewArm64Target()
	ssa.Decompose(f)
	if err := ssa.AssignABI(f, target); err != nil {
		t.Fatalf("ssa.AssignABI %s: %v", fn.Sym, err)
	}
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("%s did not verify after the ABI pass: %v", fn.Sym, vs)
	}
	if err := ssa.Lower(f, rules.ARM64); err != nil {
		t.Fatalf("lowering refused %s: %v", fn.Sym, err)
	}
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("%s did not verify after lowering: %v", fn.Sym, vs)
	}
	ssa.SplitCriticalEdges(f)
	a, err := ssa.Allocate(f, target)
	if err != nil {
		t.Fatalf("ssa.Allocate %s: %v", fn.Sym, err)
	}
	return &compiled{fn: fn, f: f, a: a, fset: c.fset, file: c.file}
}

// declaredSyms is the symbol of every function the checked package declares.
//
// It is driver.declaredSyms, built from the same field, because
// [MethodWrappers] reads it to tell a method declared on a type from one
// promoted into it and the two owe different wrappers.
func (c *checked) declaredSyms() map[string]bool {
	out := make(map[string]bool, len(c.ir.Funcs))
	for _, fn := range c.ir.Funcs {
		out[fn.Sym] = true
	}
	return out
}

// wrappers is MethodWrappers for the checked package.
func (c *checked) wrappers(t *testing.T, types ...*ir.Type) []*ir.Func {
	t.Helper()
	fns, err := MethodWrappers(types, "main", c.declaredSyms())
	if err != nil {
		t.Fatalf("MethodWrappers: %v", err)
	}
	return fns
}

// declared returns a function the front end built, by name.
func (c *checked) declared(t *testing.T, sym string) *ir.Func {
	t.Helper()
	for _, fn := range c.ir.Funcs {
		if fn.Sym == sym {
			return fn
		}
	}
	t.Fatalf("%s is not a function of the package", sym)
	return nil
}

// wrapperSource declares a type whose method takes a value receiver and whose
// value is not one pointer word, which is the shape that needs a wrapper.
const wrapperSource = `package main

type counter struct{ n int }

func (c counter) get() int { return c.n }

func (c counter) add(x, y int) int { return c.n + x + y }

func (c counter) sum(xs ...int) int {
	t := c.n
	for _, x := range xs {
		t += x
	}
	return t
}

func (c *counter) bump() { c.n++ }
`

func TestMethodWrappersNamesTheGeneratedFunction(t *testing.T) {
	c := check(t, wrapperSource)
	fns := c.wrappers(t, c.namedType(t, "counter"))
	var got []string
	for _, fn := range fns {
		got = append(got, fn.Sym)
	}
	// One wrapper per value receiver method and none for the pointer receiver
	// one, which the front end already spells with a pointer.
	want := []string{"main.(*counter).add", "main.(*counter).get", "main.(*counter).sum"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the wrappers are %v, want %v", got, want)
	}
	for _, fn := range fns {
		if !fn.Wrapper {
			t.Errorf("%s is not marked a wrapper, so its funcID is not FuncIDWrapper and a recover below it would count its frame", fn.Sym)
		}
		if fn.Recv == nil || fn.Recv.Type.Kind != ir.Ptr {
			t.Errorf("%s does not take a pointer receiver", fn.Sym)
		}
	}
}

// TestMethodWrappersCoverBothDescriptors checks that T and *T reach one
// wrapper rather than two.
func TestMethodWrappersCoverBothDescriptors(t *testing.T) {
	c := check(t, wrapperSource)
	base := c.namedType(t, "counter")
	ptr := &ir.Type{Kind: ir.Ptr, Elem: base}
	if err := ir.Layout(ptr); err != nil {
		t.Fatal(err)
	}
	fns := c.wrappers(t, base, ptr)
	if len(fns) != 3 {
		var got []string
		for _, fn := range fns {
			got = append(got, fn.Sym)
		}
		t.Fatalf("the wrappers are %v, want one per value receiver method", got)
	}
}

// TestMethodSymbolSpellsTheReceiver checks the two spellings against the ones
// the front end gives the methods themselves.
//
// A wrapper the descriptor names and nothing defines is a link failure, and a
// wrapper that calls a symbol the front end spelled differently is the same
// failure one step further away.
func TestMethodSymbolSpellsTheReceiver(t *testing.T) {
	c := check(t, wrapperSource)
	base := c.namedType(t, "counter")
	for _, m := range base.Methods {
		got, err := ir.MethodSymbol(base, m, m.PtrOnly)
		if err != nil {
			t.Fatalf("MethodSymbol %s: %v", m.Name, err)
		}
		c.declared(t, got)
	}
}

// TestMethodWrapperRuns links a generated wrapper with the method it wraps and
// runs the program.
//
// This is the gate the whole file exists for. A wrapper that is emitted but
// never executed proves that the bytes were written; this proves that the
// receiver reaches the method through the pointer, that the parameters keep
// their places across the extra frame, and that the result comes back.
func TestMethodWrapperRuns(t *testing.T) {
	hostRunsNanogoOutput(t)
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)

	tests := []struct {
		name string
		sym  string
		decl string
		call string
		want int
	}{
		// No parameters and one result: the receiver is loaded through the
		// pointer and nothing else moves.
		{"one result", "main.(*counter).get",
			"func (c *counter) get() int", "(&counter{n: 7}).get()", 7},
		// Parameters as well, so the wrapper's own arguments have to keep
		// their register places while the receiver takes the first one.
		{"parameters", "main.(*counter).add",
			"func (c *counter) add(x, y int) int", "(&counter{n: 100}).add(20, 3)", 123},
		// A variadic method. The caller packs the arguments into a slice
		// before the call, so the wrapper's last parameter is an ordinary
		// slice and forwarding it must not pack it a second time. This case
		// was a refusal until the reading behind it was checked: ir.Build
		// packs from the syntax of a call, and a wrapper has no syntax.
		{"variadic", "main.(*counter).sum",
			"func (c *counter) sum(xs ...int) int", "(&counter{n: 100}).sum(1, 2, 3)", 106},
		// The same method with the arguments already in a slice. The two
		// forms reach the wrapper identically, and a wrapper that repacked
		// would differ between them.
		{"variadic spread", "main.(*counter).sum",
			"func (c *counter) sum(xs ...int) int", "(&counter{n: 100}).sum([]int{4, 5, 6}...)", 115},
		// No variadic arguments at all, which is the empty slice rather than
		// a missing parameter.
		{"variadic empty", "main.(*counter).sum",
			"func (c *counter) sum(xs ...int) int", "(&counter{n: 100}).sum()", 100},
	}
	for _, tc2 := range tests {
		t.Run(tc2.name, func(t *testing.T) {
			c := check(t, wrapperSource)
			fns := c.wrappers(t, c.namedType(t, "counter"))
			p := newMainPackage()
			var wrapper *ir.Func
			for _, fn := range fns {
				if fn.Sym == tc2.sym {
					wrapper = fn
				}
			}
			if wrapper == nil {
				t.Fatalf("%s is not among the generated wrappers", tc2.sym)
			}
			// The method the wrapper calls, compiled from its declaration,
			// and the wrapper itself. Both go into the one object, which is
			// what the driver does.
			method, err := ir.MethodSymbol(c.namedType(t, "counter"), methodNamed(t, c, "counter", methodOf(tc2.sym)), false)
			if err != nil {
				t.Fatal(err)
			}
			for _, fn := range []*ir.Func{c.declared(t, method), wrapper} {
				r := emitFunc(t, c.build(t, fn), p)
				addFull(t, r, p)
			}
			caller := exitWrapper(t, goCmd, tc2.sym, tc2.call, "type counter struct{ n int }", tc2.decl)
			got := strings.TrimSpace(runLinked(t, goCmd, tc, cfg, p, caller))
			if want := strconv.Itoa(tc2.want); got != want {
				t.Fatalf("the program printed %q, and the wrapper returns %s", got, want)
			}
			t.Logf("linked and ran the generated wrapper %s, which returned %s", tc2.sym, got)
		})
	}
}

// methodOf returns the method name of a wrapper symbol.
func methodOf(sym string) string { return sym[strings.LastIndex(sym, ".")+1:] }

// methodNamed returns one entry of a type's method set.
func methodNamed(t *testing.T, c *checked, typ, name string) ir.Method {
	t.Helper()
	for _, m := range c.namedType(t, typ).Methods {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("%s has no method %s", typ, name)
	return ir.Method{}
}

// emitFunc compiles one function into a symbol under its own name.
//
// emit names the symbol after the SSA function, which is the declaration's
// name. A generated function's symbol is decided by the generator, so it is
// passed through Options.
func emitFunc(t *testing.T, c *compiled, p *obj.Package) *Result {
	t.Helper()
	r, err := Emit(c.f, c.a, p, Options{
		Sym:           c.fn.Sym,
		File:          c.file,
		Line:          1,
		Fset:          c.fset,
		ReflectMethod: c.fn.ReflectMethod,
	})
	if err != nil {
		t.Fatalf("Emit %s: %v", c.fn.Sym, err)
	}
	return r
}
