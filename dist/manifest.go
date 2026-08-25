// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NanogoTool is the producer name nanogo writes. GcTool is the one the Go
// toolchain's compiler is recorded under. A tally is a count of archives by
// producer, so these two strings are what the count is grouped by.
const (
	NanogoTool = "nanogo"
	GcTool     = "gc"
)

// ManifestFile is the record of what compiled each archive. It sits beside
// them, in pkg/GOOS_GOARCH.
const ManifestFile = "MANIFEST"

// manifestVersion is the first line of the file. It is a format version, so a
// tree written by another release fails to parse rather than being read
// wrongly.
const manifestVersion = "nanogo-manifest 1"

// A Producer is the compiler that wrote one archive.
type Producer struct {
	// Tool is [NanogoTool] or [GcTool].
	Tool string
	// Version is the release the tool identified itself as. For gc it is the
	// release in the archive's own object header, and for nanogo it is the
	// build identity driver.VersionLine carries.
	Version string
}

// String is the form the manifest and the tally line use.
func (p Producer) String() string { return p.Tool + " " + p.Version }

// IsNanogo reports whether nanogo compiled the archive.
func (p Producer) IsNanogo() bool { return p.Tool == NanogoTool }

// A Record is one archive's line in the manifest.
type Record struct {
	// Path is the import path of the package the archive holds.
	Path string
	// Producer is the compiler that wrote it.
	Producer Producer
	// Sum is the SHA-256 of the archive, in hex.
	//
	// The hash is what binds the record to the bytes. Without it the manifest
	// would say what somebody expected the tree to hold, and an archive
	// swapped for another would be reported under the name of the compiler
	// that did not write it. That is the fault this whole file exists to
	// prevent, so the record is checked against the file on every read.
	Sum string
}

// String is one manifest line.
func (r Record) String() string { return r.Path + " " + r.Producer.String() + " " + r.Sum }

// A Manifest is the record for every archive in a tree, sorted by import path.
type Manifest []Record

// String is the file's content.
func (m Manifest) String() string {
	var b strings.Builder
	b.WriteString(manifestVersion + "\n")
	for _, r := range m {
		b.WriteString(r.String() + "\n")
	}
	return b.String()
}

// ParseManifest reads a manifest.
//
// The parse is strict. This file is the only evidence a distribution offers
// about who compiled it, so a line this function does not fully understand is
// an error and never a partial answer.
func ParseManifest(b []byte) (Manifest, error) {
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) == 0 || lines[0] != manifestVersion {
		return nil, fmt.Errorf("%s does not start with %q", ManifestFile, manifestVersion)
	}
	var m Manifest
	for i, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) != 4 {
			return nil, fmt.Errorf("%s line %d is %q, and a record is a path, a tool, a version and a checksum", ManifestFile, i+2, line)
		}
		if f[1] != NanogoTool && f[1] != GcTool {
			return nil, fmt.Errorf("%s line %d names tool %q, which is neither %q nor %q", ManifestFile, i+2, f[1], NanogoTool, GcTool)
		}
		if _, err := hex.DecodeString(f[3]); err != nil || len(f[3]) != sha256.Size*2 {
			return nil, fmt.Errorf("%s line %d has %q where a SHA-256 belongs", ManifestFile, i+2, f[3])
		}
		m = append(m, Record{Path: f[0], Producer: Producer{Tool: f[1], Version: f[2]}, Sum: f[3]})
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("%s lists no archives", ManifestFile)
	}
	for i := 1; i < len(m); i++ {
		if m[i-1].Path >= m[i].Path {
			return nil, fmt.Errorf("%s is not sorted by import path: %s comes after %s", ManifestFile, m[i].Path, m[i-1].Path)
		}
	}
	return m, nil
}

// manifestPath is where the manifest sits in a tree.
func manifestPath(root, target string) string {
	return filepath.Join(root, "pkg", target, ManifestFile)
}

// WriteManifest writes the manifest of a tree, sorted.
func WriteManifest(root, target string, m Manifest) error {
	sorted := append(Manifest(nil), m...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	if _, err := ParseManifest([]byte(sorted.String())); err != nil {
		return err
	}
	return os.WriteFile(manifestPath(root, target), []byte(sorted.String()), fileMode)
}

// ReadManifest reads the manifest of a tree.
func ReadManifest(root, target string) (Manifest, error) {
	b, err := os.ReadFile(manifestPath(root, target))
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", manifestPath(root, target), err)
	}
	return m, nil
}

// sumFile is the SHA-256 of a file, in hex.
func sumFile(name string) (string, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
