// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// VersionFile is the name of the file that says which release a tree is.
// driver.IsNanogoRoot stats it, so its presence is what makes a directory a
// distribution rather than a checkout.
const VersionFile = "VERSION"

// A Version is the content of a distribution's VERSION file.
//
// Line one is the release, in Go's shape, so that the first line of the file
// answers "which nanogo is this" with no parsing. The rest are key and value,
// and the last three are the tally: a distribution states in writing how much
// of itself nanogo compiled, and [VerifyTree] fails when the statement and the
// archives disagree.
type Version struct {
	// Nanogo is the release, as "nanogo0.1.0".
	Nanogo string
	// Go is the pinned Go release, as "go1.27.0".
	Go string
	// Target is the GOOS_GOARCH the archives are for.
	Target string
	// Packages is the number of archives in pkg/Target.
	Packages int
	// ByNanogo and ByGc split that number by producer.
	ByNanogo int
	ByGc     int
}

// String is the file's content.
func (v Version) String() string {
	return v.Nanogo + "\n" +
		"go " + v.Go + "\n" +
		"target " + v.Target + "\n" +
		"packages " + strconv.Itoa(v.Packages) + "\n" +
		"nanogo " + strconv.Itoa(v.ByNanogo) + "\n" +
		"gc " + strconv.Itoa(v.ByGc) + "\n"
}

// ParseVersion reads a VERSION file.
//
// Strict, and for the same reason [ParseRecord] is: this file is a claim about
// the tree beside it, and a claim that is half read is worse than one that is
// not read at all.
func ParseVersion(b []byte) (Version, error) {
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 6 {
		return Version{}, fmt.Errorf("%s has %d lines and a nanogo %s has 6", VersionFile, len(lines), VersionFile)
	}
	v := Version{Nanogo: lines[0]}
	if !strings.HasPrefix(v.Nanogo, "nanogo") {
		return Version{}, fmt.Errorf("%s line 1 is %q, which is not a nanogo release", VersionFile, v.Nanogo)
	}
	strs := []struct {
		key string
		dst *string
	}{{"go", &v.Go}, {"target", &v.Target}}
	for i, f := range strs {
		val, ok := strings.CutPrefix(lines[1+i], f.key+" ")
		if !ok || val == "" {
			return Version{}, fmt.Errorf("%s line %d is %q, not %q", VersionFile, 2+i, lines[1+i], f.key)
		}
		*f.dst = val
	}
	nums := []struct {
		key string
		dst *int
	}{{"packages", &v.Packages}, {"nanogo", &v.ByNanogo}, {"gc", &v.ByGc}}
	for i, f := range nums {
		val, ok := strings.CutPrefix(lines[3+i], f.key+" ")
		if !ok {
			return Version{}, fmt.Errorf("%s line %d is %q, not %q", VersionFile, 4+i, lines[3+i], f.key)
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%s line %d has %q where a count belongs", VersionFile, 4+i, val)
		}
		*f.dst = n
	}
	if v.ByNanogo+v.ByGc != v.Packages {
		return Version{}, fmt.Errorf("%s says %d packages and %d+%d by producer", VersionFile, v.Packages, v.ByNanogo, v.ByGc)
	}
	return v, nil
}

// ReadVersion reads the VERSION file of the tree at root.
func ReadVersion(root string) (Version, error) {
	b, err := os.ReadFile(filepath.Join(root, VersionFile))
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(b)
}

// VerifyTree checks that a tree's VERSION agrees with the archives beside it.
//
// This is the check the release job fails on. A counter that is written once
// and never compared is a counter nobody has reason to trust, so the tally in
// VERSION is recomputed from the bytes of every archive and the two are
// required to match. The Go release is checked the same way: every gc-produced
// archive must name the release VERSION pins, which catches a release job
// whose toolchain resolved to a different patch release than the pin.
func VerifyTree(root string) error {
	v, err := ReadVersion(root)
	if err != nil {
		return err
	}
	t, err := TallyTree(root, v.Target)
	if err != nil {
		return err
	}
	if t.Total() != v.Packages {
		return fmt.Errorf("%s says %d packages and pkg/%s holds %d", VersionFile, v.Packages, v.Target, t.Total())
	}
	// The gc count is not checked separately. ParseVersion requires the two
	// producer counts to add up to the package count, and the two lines above
	// have already fixed the total and the nanogo half, so a gc count that
	// disagreed could not have parsed.
	if t.Nanogo() != v.ByNanogo {
		return fmt.Errorf("%s says nanogo compiled %d packages and the archives say %d", VersionFile, v.ByNanogo, t.Nanogo())
	}
	for _, r := range t.Records {
		if r.Producer.Tool == GcTool && r.Producer.Version != v.Go {
			return fmt.Errorf("%s: compiled by %s and %s pins %s", r.Path, r.Producer, VersionFile, v.Go)
		}
	}
	return nil
}
