// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/dist"
)

// treeGoVersion is the release a fabricated distribution says its archives
// were compiled by. It is the one the fake go command in build_test.go
// reports, so the two agree unless a test makes them disagree on purpose.
const treeGoVersion = "go1.27.0"

// writeDistribution fabricates a distribution holding one archive per import
// path, with a VERSION and a MANIFEST that agree with the bytes.
//
// The archives are not real objects. Nothing this file tests reads inside one:
// [openDistribution] resolves paths and checks checksums, and the checksum is
// over whatever bytes are there.
func writeDistribution(t *testing.T, paths ...string) string {
	t.Helper()
	return writeDistributionBy(t, dist.Producer{Tool: dist.GcTool, Version: treeGoVersion}, paths...)
}

// writeNanogoDistribution is the same tree with every archive recorded as
// nanogo's work. It is the tree this project is building towards, and the one
// case where the honesty line has nothing to say about gc.
func writeNanogoDistribution(t *testing.T, paths ...string) string {
	t.Helper()
	return writeDistributionBy(t, dist.Producer{Tool: dist.NanogoTool, Version: "nanogo0.0.0-test"}, paths...)
}

func writeDistributionBy(t *testing.T, producer dist.Producer, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "pkg", TargetDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var pkgs []dist.Package
	for _, p := range paths {
		src := filepath.Join(root, filepath.FromSlash(p)+".src")
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src, []byte("archive of "+p), 0o600); err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, dist.Package{Path: p, Archive: src, Producer: producer})
	}
	var m dist.Manifest
	for _, p := range pkgs {
		b, err := os.ReadFile(p.Archive)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(p.Path)+".a")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
		m = append(m, dist.Record{Path: p.Path, Producer: p.Producer, Sum: sha256Hex(t, dst)})
	}
	if err := dist.WriteManifest(root, TargetDir(), m); err != nil {
		t.Fatal(err)
	}
	v := dist.Version{Release: "nanogo0.0.0-test", Go: treeGoVersion, Target: TargetDir(), Packages: len(paths)}
	if producer.IsNanogo() {
		v.ByNanogo = len(paths)
	} else {
		v.ByGc = len(paths)
	}
	if err := os.WriteFile(filepath.Join(root, dist.VersionFile), []byte(v.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsNanogoRoot(root) {
		t.Fatalf("the fabricated tree at %s is not a distribution", root)
	}
	return root
}

func sha256Hex(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestOpenDistributionNamesEveryArchiveFromTheManifest(t *testing.T) {
	root := writeDistribution(t, "math/bits", "runtime")
	d, err := openDistribution(root)
	if err != nil {
		t.Fatal(err)
	}
	if d.Go != treeGoVersion || d.Packages != 2 {
		t.Errorf("openDistribution = %+v, want %s and 2 packages", d, treeGoVersion)
	}
	p, ok := d.lookup("math/bits")
	if !ok {
		t.Fatal("the tree holds math/bits and openDistribution did not find it")
	}
	want := filepath.Join(root, "pkg", TargetDir(), "math", "bits.a")
	if p.Archive != want {
		t.Errorf("math/bits resolved to %q, want %q", p.Archive, want)
	}
	// The producer comes from the record and not from the file name, which is
	// the whole reason the manifest exists.
	if p.Nanogo || p.Producer != dist.GcTool+" "+treeGoVersion {
		t.Errorf("math/bits was recorded as %+v", p)
	}
	if _, ok := d.lookup("fmt"); ok {
		t.Error("the tree does not hold fmt and openDistribution found it")
	}
}

// TestOpenDistributionRefusesATreeThatDisagreesWithItself is the fault the
// checksum exists to catch: one archive replaced by another.
func TestOpenDistributionRefusesATreeThatDisagreesWithItself(t *testing.T) {
	root := writeDistribution(t, "math/bits", "runtime")
	swapped := filepath.Join(root, "pkg", TargetDir(), "runtime.a")
	if err := os.WriteFile(swapped, []byte("some other archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := openDistribution(root)
	if err == nil {
		t.Fatal("a tree whose archive does not match its record was accepted")
	}
	for _, want := range []string{root, dist.ManifestFile} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %q", err, want)
		}
	}
}

func TestOpenDistributionRefusesAnotherTarget(t *testing.T) {
	root := writeDistribution(t, "runtime")
	v := dist.Version{Release: "nanogo0.0.0-test", Go: treeGoVersion, Target: "linux_amd64", Packages: 1, ByGc: 1}
	if err := os.WriteFile(filepath.Join(root, dist.VersionFile), []byte(v.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := openDistribution(root)
	if err == nil || !strings.Contains(err.Error(), "linux_amd64") {
		t.Fatalf("openDistribution = %v, want a refusal naming the target the tree holds", err)
	}
}

func TestOpenDistributionRefusesAnUnreadableVersion(t *testing.T) {
	root := writeDistribution(t, "runtime")
	if err := os.WriteFile(filepath.Join(root, dist.VersionFile), []byte("nanogo0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := openDistribution(root)
	if err == nil || !strings.Contains(err.Error(), dist.VersionFile) {
		t.Fatalf("openDistribution = %v, want a refusal naming %s", err, dist.VersionFile)
	}
}

// TestCheckToolchainRefusesAReleaseTheTreeWasNotBuiltWith names both releases.
// The alternative is a link that fails on an object header mismatch, which
// names two releases and neither tree.
func TestCheckToolchainRefusesAReleaseTheTreeWasNotBuiltWith(t *testing.T) {
	root := writeDistribution(t, "runtime")
	d, err := openDistribution(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.checkToolchain(treeGoVersion); err != nil {
		t.Fatalf("checkToolchain refused the release the tree was built with: %v", err)
	}
	err = d.checkToolchain("go1.28.1")
	if err == nil {
		t.Fatal("a toolchain that is not the tree's release was accepted")
	}
	for _, want := range []string{root, treeGoVersion, "go1.28.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %q", err, want)
		}
	}
}
