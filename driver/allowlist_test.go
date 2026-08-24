// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAllowlist(t *testing.T) {
	const data = `# the packages nanogo compiles, ordered by dependency depth

unicode/utf8
strconv   # a trailing comment
	errors

# an empty line and a comment only line are ignored
`
	a := ParseAllowlist([]byte(data))
	if a.Len() != 3 {
		t.Errorf("Len = %d, want 3", a.Len())
	}
	for _, p := range []string{"unicode/utf8", "strconv", "errors"} {
		if !a.Has(p) {
			t.Errorf("Has(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"fmt", "", "#", "strconv   "} {
		if a.Has(p) {
			t.Errorf("Has(%q) = true, want false", p)
		}
	}
}

// TestAllowlistEmpty checks the safe state. Nothing on the list means nanogo
// compiles nothing and every package goes to gc.
func TestAllowlistEmpty(t *testing.T) {
	var nilList *Allowlist
	if nilList.Has("fmt") {
		t.Error("a nil Allowlist claims to hold fmt")
	}
	if nilList.Len() != 0 {
		t.Error("a nil Allowlist has a length")
	}
	if (&Allowlist{}).Has("fmt") {
		t.Error("an empty Allowlist claims to hold fmt")
	}
}

func TestAllowlistFromEnv(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(name, []byte("strconv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("unset", func(t *testing.T) {
		a, err := AllowlistFromEnv(func(string) string { return "" })
		if err != nil {
			t.Fatalf("AllowlistFromEnv: %v", err)
		}
		if a.Len() != 0 {
			t.Errorf("Len = %d, want 0", a.Len())
		}
	})

	t.Run("set", func(t *testing.T) {
		a, err := AllowlistFromEnv(func(k string) string {
			if k == AllowlistEnv {
				return name
			}
			return ""
		})
		if err != nil {
			t.Fatalf("AllowlistFromEnv: %v", err)
		}
		if !a.Has("strconv") {
			t.Error("strconv is not on the list")
		}
	})

	// A path that names no file is an error. Treating it as an empty list
	// would turn nanogo off without saying so.
	t.Run("unreadable", func(t *testing.T) {
		_, err := AllowlistFromEnv(func(string) string {
			return filepath.Join(dir, "missing")
		})
		if err == nil {
			t.Fatal("AllowlistFromEnv on a missing file = no error, want one")
		}
		if !strings.Contains(err.Error(), AllowlistEnv) {
			t.Errorf("error %q does not name %s", err, AllowlistEnv)
		}
	})
}

func TestLoadAllowlist(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(name, []byte("errors\nstrconv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAllowlist(name)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}
	if a.Len() != 2 {
		t.Errorf("Len = %d, want 2", a.Len())
	}
}
