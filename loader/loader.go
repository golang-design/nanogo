// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package loader answers one question: given an import path and a build
// configuration, which files are in that package, and what does it import?
//
// Everything downstream of the loader depends on that answer being the one the
// go command would give, so the package is written against the go command as
// its oracle. See specs/014-package-loader.md.
//
// The package holds one interface and two implementations of it. Loader is the
// seam. At G1 the implementation is GoList, which runs go list and decodes the
// answer. At G2 there is no go binary, so a second implementation resolves the
// module graph itself. Nothing above the loader knows which one answered, so
// the G2 work is a new implementation and not a refactor
// (specs/001-bootstrap-gates.md).
//
// Every output path of this package is deterministic. Package lists are sorted
// by import path and no map is ranged over to produce a result, as required by
// specs/053-determinism.md.
package loader

import (
	"sort"
	"strings"
)

// Loader resolves patterns to packages.
//
// This is the seam of specs/014-package-loader.md. The interface is
// deliberately one method, because a caller that can name the implementation
// is a caller that will depend on it.
type Loader interface {
	// Load resolves the patterns and returns the packages, sorted by import
	// path. An error that belongs to one package is attached to that package
	// and does not fail the load. The returned error reports a failure of the
	// load itself.
	Load(patterns ...string) ([]*Package, error)
}

// Package is one resolved Go package.
//
// The shape follows go list -json, because that is the answer the loader must
// reproduce, but the type is nanogo's. A G2 implementation fills the same
// fields from the module graph and the file system.
type Package struct {
	// ImportPath is the package identity and the symbol prefix
	// (specs/014-package-loader.md). A test variant is a distinct package
	// with a distinct path.
	ImportPath string

	// Dir is the absolute directory holding the package source.
	Dir string

	// Name is the package clause name.
	Name string

	// Standard reports whether the package is in the standard library. At G1
	// the go command reports it. At G2 it follows from the import path: a
	// path whose first element holds no dot resolves under GOROOT/src.
	Standard bool

	// Export is the path to the compiled export data of this package, when
	// the loader was asked for it. It is empty otherwise.
	Export string

	// GoFiles holds the .go files in the package, without cgo files and
	// without test files, in the order the go command reports.
	GoFiles []string

	// CgoFiles holds the .go files that import "C". nanogo does not compile
	// them (specs/000-decisions.md decision 8), but it must know they exist
	// to explain why a package cannot be built.
	CgoFiles []string

	// IgnoredGoFiles holds the .go files the build constraints rejected.
	IgnoredGoFiles []string

	// SFiles holds the assembly files.
	SFiles []string

	// TestGoFiles holds the in-package test files, and XTestGoFiles the
	// external ones. They belong to the test variant of the package.
	TestGoFiles  []string
	XTestGoFiles []string

	// ImportPaths holds the paths this package imports, sorted. This is the
	// field to range over. Imports is a lookup table for the same set and is
	// never ranged over on an output path (specs/053-determinism.md).
	ImportPaths []string

	// Imports maps an import path to the loaded package. A value is nil when
	// the loader was not asked for that package.
	Imports map[string]*Package

	// Deps holds every transitive dependency path, sorted.
	Deps []string

	// Err holds an error that belongs to this package. It is reported here
	// and not returned, so that one broken package does not hide the rest of
	// the graph.
	Err *Error
}

// Error is an error that belongs to one package.
type Error struct {
	// ImportPath names the package the error belongs to.
	ImportPath string

	// Pos is a file:line:col position, when there is one.
	Pos string

	// Msg is the message.
	Msg string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Pos != "" {
		b.WriteString(e.Pos)
		b.WriteString(": ")
	} else if e.ImportPath != "" {
		b.WriteString(e.ImportPath)
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// CycleError reports an import cycle. It names every package in the cycle,
// which a depth counter cannot do (specs/014-package-loader.md).
type CycleError struct {
	// Cycle holds the packages of the cycle in import order, starting and
	// ending at the same package. It is rotated so that the
	// lexicographically smallest path comes first, which makes the message
	// independent of where the walk entered the cycle.
	Cycle []string
}

func (e *CycleError) Error() string {
	return "import cycle: " + strings.Join(e.Cycle, " -> ")
}

// SortPackages sorts packages by import path in place and returns them.
func SortPackages(pkgs []*Package) []*Package {
	sort.Slice(pkgs, func(i, j int) bool {
		return pkgs[i].ImportPath < pkgs[j].ImportPath
	})
	return pkgs
}

// TopoSort returns the packages in dependency order: a package comes after
// every package it imports.
//
// The order is deterministic for a given input. The walk starts from the
// packages sorted by import path and visits each package's imports in sorted
// order, so neither map iteration nor the input slice order can change the
// result (specs/053-determinism.md).
//
// An import path that no package in pkgs provides is skipped. The loader
// reports a missing package through Package.Err; the walk does not repeat it.
func TopoSort(pkgs []*Package) ([]*Package, error) {
	index := make(map[string]*Package, len(pkgs))
	for _, p := range pkgs {
		index[p.ImportPath] = p
	}

	roots := make([]*Package, len(pkgs))
	copy(roots, pkgs)
	SortPackages(roots)

	const (
		onStack = 1
		done    = 2
	)
	state := make(map[string]int, len(pkgs))
	var stack []string
	out := make([]*Package, 0, len(pkgs))

	var visit func(p *Package) error
	visit = func(p *Package) error {
		switch state[p.ImportPath] {
		case done:
			return nil
		case onStack:
			return &CycleError{Cycle: cycleFrom(stack, p.ImportPath)}
		}
		state[p.ImportPath] = onStack
		stack = append(stack, p.ImportPath)

		for _, path := range sortedImports(p) {
			dep := p.Imports[path]
			if dep == nil {
				dep = index[path]
			}
			if dep == nil {
				continue
			}
			if err := visit(dep); err != nil {
				return err
			}
		}

		stack = stack[:len(stack)-1]
		state[p.ImportPath] = done
		out = append(out, p)
		return nil
	}

	for _, p := range roots {
		if err := visit(p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sortedImports returns the import paths of p in sorted order. It prefers
// ImportPaths and falls back to the keys of Imports, which it sorts, because a
// hand-built graph may set only the map.
func sortedImports(p *Package) []string {
	if len(p.ImportPaths) > 0 {
		paths := make([]string, len(p.ImportPaths))
		copy(paths, p.ImportPaths)
		sort.Strings(paths)
		return paths
	}
	paths := make([]string, 0, len(p.Imports))
	for path := range p.Imports {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// cycleFrom extracts the cycle that closes at path from the walk stack and
// rotates it to start at its smallest element.
func cycleFrom(stack []string, path string) []string {
	start := 0
	for i, s := range stack {
		if s == path {
			start = i
			break
		}
	}
	ring := stack[start:]

	min := 0
	for i, s := range ring {
		if s < ring[min] {
			min = i
		}
	}

	cycle := make([]string, 0, len(ring)+1)
	for i := range ring {
		cycle = append(cycle, ring[(min+i)%len(ring)])
	}
	return append(cycle, cycle[0])
}

// String returns the import path, so that a package prints as its identity.
func (p *Package) String() string { return p.ImportPath }
