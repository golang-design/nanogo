// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package types2_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"golang.design/x/nanogo/syntax"
	. "golang.design/x/nanogo/types2"
)

// This file holds the parse and type-check helpers the ported upstream tests
// share. Upstream keeps them in api_test.go, check_test.go, self_test.go and
// importer_test.go, none of which is ported whole: each of those files also
// carries tests that need infrastructure nanogo does not have. See the
// skippedTests table in types2/gen/gen.go.
//
// The helpers keep the upstream names and signatures, so a ported test calls
// them unchanged.

// nopos indicates an unknown position.
var nopos syntax.Pos

// goVersionMinor is the minor version of the Go language this checker
// implements. It must match the constant of the same name in types2/version.go,
// which the generator writes there. Upstream reads internal/goversion.Version
// in both places, and that package is not importable outside the Go repository.
const goVersionMinor = 27

// testFset is the FileSet every helper here parses into.
//
// One set for the whole test binary, rather than one per parse, because a
// Config holds a single FileSet and a test that parses more than once must
// still resolve every position it produced. The mutex covers the only mutation
// a FileSet has, which is adding a file.
var (
	testMu   sync.Mutex
	testFset = syntax.NewFileSet()
)

// parseSrc parses src as the named file.
func parseSrc(filename, src string) (*syntax.File, error) {
	testMu.Lock()
	f := testFset.AddFile(filename, len(src))
	testMu.Unlock()
	return syntax.Parse(f, []byte(src), nil, nil, 0)
}

func mustParse(src string) *syntax.File {
	// The file is named for the package, with no extension, because upstream
	// names its position base that way and the ported tests print the name.
	f, err := parseSrc(pkgName(src), src)
	if err != nil {
		panic(err) // so we don't need to pass *testing.T
	}
	return f
}

func typecheck(src string, conf *Config, info *Info) (*Package, error) {
	f := mustParse(src)
	if conf == nil {
		conf = &Config{
			Error:    func(err error) {}, // collect all errors
			Importer: defaultImporter(),
		}
	}
	if conf.Fset == nil {
		conf.Fset = testFset
	}
	return conf.Check(f.PkgName.Value, []*syntax.File{f}, info)
}

func mustTypecheck(src string, conf *Config, info *Info) *Package {
	pkg, err := typecheck(src, conf, info)
	if err != nil {
		panic(err) // so we don't need to pass *testing.T
	}
	return pkg
}

// pkgName extracts the package name from src, which must contain a package header.
func pkgName(src string) string {
	const kw = "package "
	if i := strings.Index(src, kw); i >= 0 {
		after := src[i+len(kw):]
		n := len(after)
		if i := strings.IndexAny(after, "\n\t ;/"); i >= 0 {
			n = i
		}
		return after[:n]
	}
	panic("missing package header: " + src)
}

// position resolves p through the FileSet the helpers parse into.
//
// Upstream prints a syntax.Pos directly. nanogo's Pos is a bare offset, so a
// test that prints one goes through here. See types2/position.go.
func position(p syntax.Pos) syntax.Position { return testFset.Position(p) }

// boolFieldAddr(conf, name) returns the address of the boolean field conf.<name>.
// For accessing unexported fields.
func boolFieldAddr(conf *Config, name string) *bool {
	v := reflect.Indirect(reflect.ValueOf(conf))
	return (*bool)(v.FieldByName(name).Addr().UnsafePointer())
}

// pkgFiles parses the non-test Go files in the given directory.
//
// Upstream reads the file list through go/build in stdlib_test.go. Reading the
// directory is enough here: the callers point at nanogo's own source, which has
// no build-tagged files.
func pkgFiles(path string) ([]*syntax.File, error) {
	ents, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var files []*syntax.File
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		testMu.Lock()
		file, err := syntax.ParseFile(testFset, filepath.Join(path, name), nil, nil, 0)
		testMu.Unlock()
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}
