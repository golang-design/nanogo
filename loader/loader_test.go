// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package loader

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// GoList must satisfy the seam of specs/014-package-loader.md. The G2
// implementation replaces it here and nothing above the loader changes.
var _ Loader = (*GoList)(nil)

// graph builds packages from a path to imports map. The map is only an input
// to the test; the packages it produces carry sorted import paths, so no
// output derives from map order.
func graph(t *testing.T, edges map[string][]string) []*Package {
	t.Helper()
	index := make(map[string]*Package, len(edges))
	paths := make([]string, 0, len(edges))
	for path := range edges {
		paths = append(paths, path)
		index[path] = &Package{ImportPath: path}
	}
	sortForTest(paths)
	pkgs := make([]*Package, 0, len(paths))
	for _, path := range paths {
		p := index[path]
		imports := append([]string(nil), edges[path]...)
		sortForTest(imports)
		p.ImportPaths = imports
		p.Imports = make(map[string]*Package, len(imports))
		for _, dep := range imports {
			p.Imports[dep] = index[dep]
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func sortForTest(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func paths(pkgs []*Package) []string {
	out := make([]string, len(pkgs))
	for i, p := range pkgs {
		out[i] = p.ImportPath
	}
	return out
}

func TestTopoSort(t *testing.T) {
	pkgs := graph(t, map[string][]string{
		"app":  {"lib", "util"},
		"lib":  {"util"},
		"util": {"base"},
		"base": nil,
	})
	got, err := TopoSort(pkgs)
	if err != nil {
		t.Fatal(err)
	}
	// The walk starts at the sorted roots and takes sorted imports first, so
	// app pulls in lib before either is emitted.
	want := []string{"base", "util", "lib", "app"}
	if !reflect.DeepEqual(paths(got), want) {
		t.Errorf("TopoSort = %v, want %v", paths(got), want)
	}

	// A package comes after everything it imports.
	position := make(map[string]int, len(got))
	for i, p := range got {
		position[p.ImportPath] = i
	}
	for _, p := range got {
		for _, dep := range p.ImportPaths {
			if position[dep] > position[p.ImportPath] {
				t.Errorf("%s comes before its import %s", p.ImportPath, dep)
			}
		}
	}
}

// TestTopoSortDeterministic checks the rule of specs/053-determinism.md: the
// order of the input and the order of a map must not reach the output.
func TestTopoSortDeterministic(t *testing.T) {
	edges := map[string][]string{
		"a": {"b", "c", "d"},
		"b": {"e"},
		"c": {"e", "f"},
		"d": {"f"},
		"e": nil,
		"f": nil,
		"g": {"a"},
	}
	first, err := TopoSort(graph(t, edges))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		pkgs := graph(t, edges)
		rng.Shuffle(len(pkgs), func(a, b int) { pkgs[a], pkgs[b] = pkgs[b], pkgs[a] })
		got, err := TopoSort(pkgs)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(paths(got), paths(first)) {
			t.Fatalf("run %d gave %v, first run gave %v", i, paths(got), paths(first))
		}
	}
}

// TestTopoSortCycle checks that a cycle is reported by naming every package in
// it, which specs/014-package-loader.md requires instead of a depth counter.
func TestTopoSortCycle(t *testing.T) {
	pkgs := graph(t, map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
		"z": {"a"},
	})
	_, err := TopoSort(pkgs)
	if err == nil {
		t.Fatal("no error for a cyclic graph")
	}
	ce, ok := err.(*CycleError)
	if !ok {
		t.Fatalf("error is %T, want *CycleError", err)
	}
	want := []string{"a", "b", "c", "a"}
	if !reflect.DeepEqual(ce.Cycle, want) {
		t.Errorf("cycle is %v, want %v", ce.Cycle, want)
	}
	for _, name := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("message %q does not name %s", err.Error(), name)
		}
	}
	if strings.Contains(err.Error(), "z") {
		t.Errorf("message %q names z, which is not in the cycle", err.Error())
	}
}

// TestTopoSortCycleRotation checks that the message does not depend on where
// the walk entered the cycle.
func TestTopoSortCycleRotation(t *testing.T) {
	edges := map[string][]string{
		"m": {"n"},
		"n": {"o"},
		"o": {"m"},
	}
	_, err := TopoSort(graph(t, edges))
	if err == nil {
		t.Fatal("no error for a cyclic graph")
	}
	want := err.Error()

	// Enter from a package that only reaches the cycle through n.
	edges2 := map[string][]string{
		"m":     {"n"},
		"n":     {"o"},
		"o":     {"m"},
		"entry": {"n"},
	}
	pkgs := graph(t, edges2)
	// Put entry first, so the walk starts there.
	for i, p := range pkgs {
		if p.ImportPath == "entry" {
			pkgs[0], pkgs[i] = pkgs[i], pkgs[0]
		}
	}
	_, err = TopoSort(pkgs)
	if err == nil {
		t.Fatal("no error for a cyclic graph")
	}
	if err.Error() != want {
		t.Errorf("entering at n gives %q, entering at m gives %q", err.Error(), want)
	}
}

func TestTopoSortSelfCycle(t *testing.T) {
	pkgs := graph(t, map[string][]string{"a": {"a"}})
	_, err := TopoSort(pkgs)
	if err == nil {
		t.Fatal("no error for a self import")
	}
	if got, want := err.Error(), "import cycle: a -> a"; got != want {
		t.Errorf("message is %q, want %q", got, want)
	}
}

// TestTopoSortMissingImport checks that an import of a package the loader was
// not asked for is skipped. The loader reports the cause through Package.Err.
func TestTopoSortMissingImport(t *testing.T) {
	pkgs := []*Package{
		{ImportPath: "a", ImportPaths: []string{"absent"}},
	}
	got, err := TopoSort(pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths(got), []string{"a"}) {
		t.Errorf("TopoSort = %v, want [a]", paths(got))
	}
}

// TestTopoSortImportsMapOnly checks that a package that carries only the map
// still sorts, so a caller that builds a graph by hand does not have to fill
// both fields.
func TestTopoSortImportsMapOnly(t *testing.T) {
	base := &Package{ImportPath: "base"}
	top := &Package{ImportPath: "top", Imports: map[string]*Package{"base": base}}
	got, err := TopoSort([]*Package{top, base})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths(got), []string{"base", "top"}) {
		t.Errorf("TopoSort = %v, want [base top]", paths(got))
	}
}

func TestTopoSortEmpty(t *testing.T) {
	got, err := TopoSort(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("TopoSort(nil) = %v, want empty", paths(got))
	}
}

func TestSortPackages(t *testing.T) {
	pkgs := []*Package{{ImportPath: "c"}, {ImportPath: "a"}, {ImportPath: "b"}}
	if got := paths(SortPackages(pkgs)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("SortPackages = %v, want [a b c]", got)
	}
}

func TestErrorString(t *testing.T) {
	tests := []struct {
		err  *Error
		want string
	}{
		{&Error{ImportPath: "a/b", Pos: "a/b/x.go:3:1", Msg: "bad"}, "a/b/x.go:3:1: bad"},
		{&Error{ImportPath: "a/b", Msg: "bad"}, "a/b: bad"},
		{&Error{Msg: "bad"}, "bad"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}

func TestPackageString(t *testing.T) {
	p := &Package{ImportPath: "a/b"}
	if got := p.String(); got != "a/b" {
		t.Errorf("String() = %q, want a/b", got)
	}
}
