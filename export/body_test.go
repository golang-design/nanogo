// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// bodyPackages is what an unattended run reads.
//
// One package per shape the body encoding has: generic declarations reached
// through the extension data (slices, maps, cmp), inlinable bodies reached
// through the private root (bytes, strings, sort), composite literals and
// closures (fmt, net/url), select and channels (sync, context), and unsafe
// intrinsics (reflect).
var bodyPackages = []string{
	"bytes", "cmp", "context", "errors", "fmt", "internal/abi", "io", "maps",
	"math", "math/bits", "net/url", "os", "reflect", "slices", "sort",
	"strconv", "strings", "sync", "time", "unicode/utf8",
}

// bodyCensus counts what one run of the body reader read and refused.
type bodyCensus struct {
	packages int
	generic  int
	inline   int
	nested   int
	refused  map[string]int
	failures []string
}

func newCensus() *bodyCensus { return &bodyCensus{refused: make(map[string]int)} }

func (c *bodyCensus) report(t *testing.T) {
	t.Helper()
	t.Logf("read %d packages: %d generic bodies, %d inlinable bodies, %d function literal bodies",
		c.packages, c.generic, c.inline, c.nested)
	if len(c.refused) == 0 {
		return
	}
	reasons := make([]string, 0, len(c.refused))
	for r := range c.refused {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		t.Logf("refused %d time(s): %s", c.refused[r], r)
	}
}

// readBodyCorpus reads the bodies of each named package and returns the
// census.
func readBodyCorpus(t *testing.T, dir string, paths ...string) *bodyCensus {
	t.Helper()
	c := newCensus()
	for _, a := range archives(t, dir, paths...) {
		path, file := a[0], a[1]
		data, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		def, err := packageDefinition(data)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		payload, err := unified(def)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		// A fresh package table per archive, so that every declaration a
		// body names comes out of the archive being read. The file is the
		// linked form, so it is self-contained.
		dec := pkgbits.NewPkgDecoder(path, string(payload))
		_, bodies, err := ReadBodies(types2.NewContext(), make(map[string]*types2.Package), dec)
		if err != nil {
			if b, ok := err.(*BodyError); ok {
				c.refused[b.Reason]++
				c.failures = append(c.failures, b.Error())
				continue
			}
			c.failures = append(c.failures, err.Error())
			continue
		}
		c.packages++
		for _, b := range bodies {
			if b.Generic {
				c.generic++
			} else {
				c.inline++
			}
			c.nested += b.Nested
		}
	}
	return c
}

// TestReadBodies is the oracle the body reader is measured by.
//
// The bodies are gc's own, out of the archives the installed toolchain wrote,
// and the check is exact: [pkgReader.decodeBody] refuses a body that leaves a
// byte of its element unread or a reference in its table that no field
// resolved. A field read at the wrong width, or skipped, moves every byte
// after it, so a decode that finishes on the last byte with every reference
// resolved is a decode that agrees with the writer field for field.
//
// Under NANOGO_REQUIRE_CORPUS it reads the whole standard library and names
// every body it cannot read. Without it, it reads [bodyPackages], because an
// unattended run should not build the world.
func TestReadBodies(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/std\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := bodyPackages
	if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
		want = []string{"std"}
	}
	c := readBodyCorpus(t, dir, want...)
	c.report(t)
	for _, f := range c.failures {
		t.Errorf("%s", f)
	}
	if c.generic == 0 {
		t.Error("no generic body was read, so the path that unblocks instantiation proved nothing")
	}
	if c.inline == 0 {
		t.Error("no inlinable body was read, so the private root's list proved nothing")
	}
}
