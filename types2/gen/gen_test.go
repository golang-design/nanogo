// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var write = flag.Bool("write", false, "write the generated files into types2/")

// root is the types2 directory, which is the parent of this package.
const root = ".."

// TestGenerate regenerates the checker and compares it with the tree.
//
// The comparison is the drift gate. CI runs this test, so a hand edit to a
// generated file, or an upstream refresh that is not reflected in the output,
// fails the build rather than living on undetected.
func TestGenerate(t *testing.T) {
	files, err := Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("generated nothing")
	}

	generated := make(map[string]bool, len(files))
	for _, f := range files {
		generated[filepath.ToSlash(f.Name)] = true
		path := filepath.Join(root, f.Name)
		if *write {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, f.Data, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		have, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (run: go test ./types2/gen/ -run TestGenerate -write)", f.Name, err)
			continue
		}
		if !bytes.Equal(have, f.Data) {
			t.Errorf("%s differs from the generator output (run: go test ./types2/gen/ -run TestGenerate -write)", f.Name)
		}
	}

	if *write {
		t.Log("wrote", len(files), "files")
		return
	}

	// A generated file that the table no longer produces must not linger.
	for _, dir := range []string{"", "errors"} {
		ents, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || filepath.Ext(name) != ".go" {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(dir, name))
			if generated[rel] {
				continue
			}
			if dir == "" {
				if _, ok := handPorted[name]; ok {
					continue
				}
				if _, ok := handWritten[name]; ok {
					continue
				}
			}
			t.Errorf("%s is neither generated, hand-ported, nor hand-written", rel)
		}
	}
}

// TestEveryUpstreamTestIsAccountedFor fails when an upstream test file is
// neither ported nor recorded as skipped.
//
// The task the fork sets itself is that the upstream test suite comes with the
// sources. A test that is quietly dropped, or one that an upstream refresh adds
// and nobody notices, defeats that. This is the gate.
func TestEveryUpstreamTestIsAccountedFor(t *testing.T) {
	check := func(dir string, ported []string, skipped map[string]string) {
		t.Helper()
		names, err := upstreamNames(filepath.Join(root, "upstream", dir))
		if err != nil {
			t.Fatal(err)
		}
		seen := make(map[string]bool)
		for _, name := range names {
			if !strings.HasSuffix(name, "_test.go") {
				continue
			}
			seen[name] = true
			inPorted := slices.Contains(ported, name)
			_, inSkipped := skipped[name]
			switch {
			case inPorted && inSkipped:
				t.Errorf("%s/%s is both ported and skipped", dir, name)
			case !inPorted && !inSkipped:
				t.Errorf("%s/%s is neither ported nor recorded in the skipped table", dir, name)
			}
		}
		for _, name := range ported {
			if !seen[name] {
				t.Errorf("%s/%s is listed as ported but is not vendored", dir, name)
			}
		}
		for name := range skipped {
			if !seen[name] {
				t.Errorf("%s/%s is listed as skipped but is not vendored", dir, name)
			}
		}
	}
	check("types2", portedTests, skippedTests)
	check("errors", nil, skippedErrorTests)
}

// TestIdempotent checks that generating twice gives the same bytes.
func TestIdempotent(t *testing.T) {
	a, err := Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("file count %d then %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Name != b[i].Name || !bytes.Equal(a[i].Data, b[i].Data) {
			t.Errorf("%s is not stable across runs", a[i].Name)
		}
	}
}

// TestEveryRewriteHasAReason keeps the table readable. specs/012 calls the
// table the specification of the divergence from upstream, so an entry with no
// stated reason is a hole in that specification.
func TestEveryRewriteHasAReason(t *testing.T) {
	for _, r := range rules {
		if r.why == "" {
			t.Errorf("rule %q has no reason", r.old)
		}
	}
	for file, ps := range patches {
		for _, p := range ps {
			if p.why == "" {
				t.Errorf("%s: patch %q has no reason", file, p.old)
			}
		}
	}
	for name, why := range handPorted {
		if why == "" {
			t.Errorf("hand-ported %s has no reason", name)
		}
	}
	for name, why := range handWritten {
		if why == "" {
			t.Errorf("hand-written %s has no reason", name)
		}
	}
	for name, why := range skippedTests {
		if why == "" {
			t.Errorf("skipped test %s has no reason", name)
		}
	}
	for name, why := range skippedErrorTests {
		if why == "" {
			t.Errorf("skipped test %s has no reason", name)
		}
	}
}
