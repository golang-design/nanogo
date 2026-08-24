// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package loader

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

// GoList is the G1 implementation of Loader. It runs the go command and
// decodes the answer.
//
// specs/001-bootstrap-gates.md allows this at G1 and forbids it at G2. It is
// not a placeholder: the go command has already applied module resolution,
// vendoring, build constraints, and the file suffix rules, and its answer is
// the answer nanogo must reproduce. It is also the oracle the G2
// implementation is tested against.
type GoList struct {
	// Cmd is the go binary to run. An empty value means "go", found on PATH.
	Cmd string

	// Dir is the working directory of the go command. An empty value means
	// the current directory. The directory selects the main module, so it
	// selects the module graph.
	Dir string

	// Env is the environment of the go command. A nil value inherits the
	// current environment.
	Env []string

	// Tags is the -tags list.
	Tags []string

	// Export requests the path to each package's compiled export data.
	// Producing it costs a build of every dependency, so a caller that only
	// needs file lists turns it off.
	Export bool

	// Tests requests the test variants of the named packages.
	Tests bool
}

// NewGoList returns a GoList that runs go in dir and asks for export data.
func NewGoList(dir string) *GoList {
	return &GoList{Dir: dir, Export: true}
}

// jsonPackage is the subset of go list -json that nanogo reads.
//
// It is a decoding type and not the loader's type. Package is nanogo's, so
// that a G2 implementation is not writing to a shape the go command chose.
type jsonPackage struct {
	Dir            string
	ImportPath     string
	Name           string
	Standard       bool
	Export         string
	GoFiles        []string
	CgoFiles       []string
	IgnoredGoFiles []string
	SFiles         []string
	TestGoFiles    []string
	XTestGoFiles   []string
	Imports        []string
	Deps           []string
	Error          *jsonPackageError
	DepsErrors     []*jsonPackageError
}

type jsonPackageError struct {
	ImportStack []string
	Pos         string
	Err         string
}

// Load runs go list and returns the packages, sorted by import path.
//
// specs/014-package-loader.md gives the command as
// go list -json -deps -export. The -e flag is added, because without it a
// single unresolvable import makes the go command exit before it prints the
// packages that are fine, and the spec also requires a per-package error to
// leave the rest of the load intact. With -e the error arrives inside the JSON
// object for the package it belongs to.
func (g *GoList) Load(patterns ...string) ([]*Package, error) {
	bin := g.Cmd
	if bin == "" {
		bin = "go"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("loader: no go command: %w", err)
	}

	args := []string{"list", "-e", "-json", "-deps"}
	if g.Export {
		args = append(args, "-export")
	}
	if g.Tests {
		args = append(args, "-test")
	}
	if len(g.Tags) > 0 {
		args = append(args, "-tags", strings.Join(g.Tags, ","))
	}
	args = append(args, patterns...)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Dir = g.Dir
	cmd.Env = g.Env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	pkgs, decodeErr := decodeList(&stdout)
	if decodeErr != nil {
		return nil, commandError(args, decodeErr, stderr.Bytes())
	}
	// A non-zero exit with packages on stdout is the normal case for a build
	// that has a broken package: the errors are already attached. Only an exit
	// that produced nothing is a failure of the load.
	if runErr != nil && len(pkgs) == 0 {
		return nil, commandError(args, runErr, stderr.Bytes())
	}

	link(pkgs)
	return SortPackages(pkgs), nil
}

// commandError reports a failure of the go command with its stderr, which
// carries the reason.
func commandError(args []string, err error, stderr []byte) error {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		return fmt.Errorf("loader: go %s: %w", strings.Join(args, " "), err)
	}
	return fmt.Errorf("loader: go %s: %w: %s", strings.Join(args, " "), err, msg)
}

// decodeList reads the stream of JSON objects go list writes.
//
// go list -json emits concatenated objects and not an array, so the decoder
// runs in a loop until it reaches the end of the stream.
func decodeList(r io.Reader) ([]*Package, error) {
	dec := json.NewDecoder(r)
	var pkgs []*Package
	seen := make(map[string]bool)
	for {
		var jp jsonPackage
		if err := dec.Decode(&jp); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		if jp.ImportPath == "" {
			continue
		}
		// -deps lists a package once, but -test repeats the package under
		// test. Keep the first, which is the one -deps produced.
		if seen[jp.ImportPath] {
			continue
		}
		seen[jp.ImportPath] = true
		pkgs = append(pkgs, convert(&jp))
	}
	return pkgs, nil
}

// convert turns a decoded go list package into nanogo's.
func convert(jp *jsonPackage) *Package {
	p := &Package{
		ImportPath:     jp.ImportPath,
		Dir:            jp.Dir,
		Name:           jp.Name,
		Standard:       jp.Standard,
		Export:         jp.Export,
		GoFiles:        jp.GoFiles,
		CgoFiles:       jp.CgoFiles,
		IgnoredGoFiles: jp.IgnoredGoFiles,
		SFiles:         jp.SFiles,
		TestGoFiles:    jp.TestGoFiles,
		XTestGoFiles:   jp.XTestGoFiles,
		ImportPaths:    sortedCopy(jp.Imports),
		Deps:           sortedCopy(jp.Deps),
	}
	switch {
	case jp.Error != nil:
		p.Err = convertError(jp.ImportPath, jp.Error)
	case len(jp.DepsErrors) > 0:
		// A package that is fine but has a broken dependency cannot be built.
		// Report the first dependency error against this package, so that the
		// caller sees one cause and not a repeated stack.
		p.Err = convertError(jp.ImportPath, jp.DepsErrors[0])
	}
	return p
}

func convertError(path string, je *jsonPackageError) *Error {
	if je == nil {
		return nil
	}
	return &Error{ImportPath: path, Pos: je.Pos, Msg: je.Err}
}

// link resolves each package's import paths to the loaded packages.
func link(pkgs []*Package) {
	index := make(map[string]*Package, len(pkgs))
	for _, p := range pkgs {
		index[p.ImportPath] = p
	}
	for _, p := range pkgs {
		if len(p.ImportPaths) == 0 {
			continue
		}
		p.Imports = make(map[string]*Package, len(p.ImportPaths))
		for _, path := range p.ImportPaths {
			// A nil value records that the path was imported and not loaded.
			// The map is a lookup table only; ImportPaths keeps the order.
			p.Imports[path] = index[path]
		}
	}
}

// sortedCopy returns a sorted copy of list, so that the caller cannot observe
// the go command's order and no later sort mutates the decoded value.
func sortedCopy(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, len(list))
	copy(out, list)
	sort.Strings(out)
	return out
}
