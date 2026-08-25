// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sample is a tree with a nested path, an executable, and two files whose
// names sort the other way round from the order a walk might report them.
func sample(t *testing.T, dir string) {
	t.Helper()
	files := []struct {
		name string
		body string
		mode os.FileMode
	}{
		{"bin/nanogo", "binary", 0o755},
		{"VERSION", "nanogo0.1.0\n", 0o644},
		{"src/internal/abi/abi.go", "package abi\n", 0o600},
		{"pkg/darwin_arm64/runtime.a", "archive", 0o644},
	}
	for _, f := range files {
		full := filepath.Join(dir, filepath.FromSlash(f.name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(f.body), f.mode); err != nil {
			t.Fatal(err)
		}
	}
}

// read lists the tarball as name, mode, mtime and body.
func read(t *testing.T, b []byte) []string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name+" "+h.FileInfo().Mode().String()+" "+h.ModTime.UTC().String()+" "+string(body))
	}
	return out
}

func TestWriteTarGzHoldsTheTreeWithFixedModesAndTimes(t *testing.T) {
	dir := t.TempDir()
	sample(t, dir)
	var buf bytes.Buffer
	if err := WriteTarGz(&buf, dir, "nanogo"); err != nil {
		t.Fatal(err)
	}
	got := read(t, buf.Bytes())
	want := []string{
		"nanogo/ drwxr-xr-x 1970-01-01 00:00:00 +0000 UTC ",
		"nanogo/VERSION -rw-r--r-- 1970-01-01 00:00:00 +0000 UTC nanogo0.1.0\n",
		"nanogo/bin/ drwxr-xr-x 1970-01-01 00:00:00 +0000 UTC ",
		"nanogo/bin/nanogo -rwxr-xr-x 1970-01-01 00:00:00 +0000 UTC binary",
		"nanogo/pkg/ drwxr-xr-x 1970-01-01 00:00:00 +0000 UTC ",
		"nanogo/pkg/darwin_arm64/ drwxr-xr-x 1970-01-01 00:00:00 +0000 UTC ",
		"nanogo/pkg/darwin_arm64/runtime.a -rw-r--r-- 1970-01-01 00:00:00 +0000 UTC archive",
		"nanogo/src/ drwxr-xr-x 1970-01-01 00:00:00 +0000 UTC ",
		"nanogo/src/internal/ drwxr-xr-x 1970-01-01 00:00:00 +0000 UTC ",
		"nanogo/src/internal/abi/ drwxr-xr-x 1970-01-01 00:00:00 +0000 UTC ",
		// Written 0600 on disk and 0644 in the tarball. A checkout made with a
		// different umask must not change the bytes.
		"nanogo/src/internal/abi/abi.go -rw-r--r-- 1970-01-01 00:00:00 +0000 UTC package abi\n",
	}
	if len(got) != len(want) {
		t.Fatalf("tarball holds %d entries, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d is\n\t%q\nwant\n\t%q", i, got[i], want[i])
		}
	}
}

// The check specs/053-determinism.md asks for, in the form the CI determinism
// job uses: the second tree lives at a different absolute path, because a path
// that leaked into the output is the failure this shape finds and building
// twice in one directory does not.
func TestWriteTarGzIsByteIdenticalFromTwoDirectories(t *testing.T) {
	var out [2][]byte
	for i := range out {
		dir := t.TempDir()
		sample(t, dir)
		var buf bytes.Buffer
		if err := WriteTarGz(&buf, dir, "nanogo"); err != nil {
			t.Fatal(err)
		}
		out[i] = buf.Bytes()
	}
	if !bytes.Equal(out[0], out[1]) {
		t.Fatalf("two builds of the same tree differ: %d and %d bytes", len(out[0]), len(out[1]))
	}
	if len(out[0]) == 0 {
		t.Fatal("both tarballs are empty, so the comparison proves nothing")
	}
}

// The file system reports a directory in whatever order it likes. Creating the
// same files in the reverse order must not move a byte.
func TestWriteTarGzDoesNotDependOnCreationOrder(t *testing.T) {
	write := func(dir string, names []string) []byte {
		for _, n := range names {
			full := filepath.Join(dir, filepath.FromSlash(n))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(n), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		var buf bytes.Buffer
		if err := WriteTarGz(&buf, dir, "nanogo"); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	names := []string{"a/one", "b/two", "c/three", "a/zzz"}
	forward := write(t.TempDir(), names)
	reversed := append([]string(nil), names...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	if !bytes.Equal(forward, write(t.TempDir(), reversed)) {
		t.Fatal("the tarball depends on the order the files were created in")
	}
}

func TestWriteTarGzRefusesWhatItCannotPack(t *testing.T) {
	if err := WriteTarGz(io.Discard, filepath.Join(t.TempDir(), "absent"), "nanogo"); err == nil {
		t.Error("a missing tree was accepted")
	}

	// A symlink in a distribution is a bug in whatever built the tree. Copying
	// it along quietly would put a dangling path in the tarball.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Skipf("this file system has no symlinks: %v", err)
	}
	err := WriteTarGz(io.Discard, dir, "nanogo")
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("WriteTarGz = %v, want an error about the symlink", err)
	}
}

func TestWriteTarGzReportsAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	sample(t, dir)
	// A writer that fails after the gzip header is written, so the failure
	// surfaces from inside the tar writer rather than from its constructor.
	if err := WriteTarGz(failAfter{n: 20}, dir, "nanogo"); err == nil {
		t.Fatal("a failing writer was accepted")
	}
	if err := writeFile(tar.NewWriter(io.Discard), TarEntry{Name: "x", Source: filepath.Join(dir, "absent")}); err == nil {
		t.Fatal("an entry naming a missing file was accepted")
	}
}

// failAfter accepts n bytes and then fails.
type failAfter struct{ n int }

func (f failAfter) Write(b []byte) (int, error) {
	if len(b) > f.n {
		return 0, io.ErrShortWrite
	}
	return len(b), nil
}
