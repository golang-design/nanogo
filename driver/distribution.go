// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"path/filepath"

	"golang.design/x/nanogo/dist"
)

// archiveExt is the extension of a compiled package under pkg.
const archiveExt = ".a"

// A distribution is the pkg tree of an unpacked nanogo release, read for one
// build.
//
// It is the answer to "where does the standard library come from" when
// [FindRoot] resolved a nanogo tree. The archives are named one by one from
// the manifest and never found by walking the directory: the manifest is the
// only thing in a distribution that says which compiler produced an archive,
// and a build that read the files and not the record would have no way to
// report that (specs/054-distribution.md).
type distribution struct {
	// Root is the tree.
	Root string

	// Go is the Go release the tree's gc-produced archives were compiled by,
	// from VERSION.
	Go string

	// Packages is the number of archives the tree holds.
	Packages int

	// byPath maps an import path to the archive and its producer.
	byPath map[string]buildPackage
}

// openDistribution reads a tree's VERSION and manifest and checks both against
// the archives beside them.
//
// [dist.TallyTree] is what does the checking, and it is the reason this is a
// read rather than a directory listing: every archive must have a manifest
// record, every record an archive, and every archive's SHA-256 must match its
// record. A tree that fails any of the three cannot say what compiled it, so a
// build against it is refused here rather than reported wrongly at the end.
func openDistribution(root string) (*distribution, error) {
	v, err := dist.ReadVersion(root)
	if err != nil {
		return nil, fmt.Errorf("%s holds a nanogo distribution whose %s cannot be read: %v", root, dist.VersionFile, err)
	}
	if v.Target != TargetDir() {
		return nil, fmt.Errorf("the distribution at %s holds %s archives and this build is for %s",
			root, v.Target, TargetDir())
	}
	t, err := dist.TallyTree(root, v.Target)
	if err != nil {
		return nil, fmt.Errorf("the distribution at %s does not agree with its own %s: %v",
			root, dist.ManifestFile, err)
	}
	dir := filepath.Join(root, "pkg", v.Target)
	d := &distribution{Root: root, Go: v.Go, Packages: t.Total(), byPath: make(map[string]buildPackage, t.Total())}
	for _, r := range t.Records {
		d.byPath[r.Path] = buildPackage{
			Path:     r.Path,
			Archive:  filepath.Join(dir, filepath.FromSlash(r.Path)+archiveExt),
			Producer: r.Producer.String(),
			Nanogo:   r.Producer.IsNanogo(),
		}
	}
	return d, nil
}

// lookup reports the archive a distribution holds for an import path.
func (d *distribution) lookup(path string) (buildPackage, bool) {
	if d == nil {
		return buildPackage{}, false
	}
	p, ok := d.byPath[path]
	return p, ok
}

// checkToolchain refuses a build whose installed toolchain is not the release
// the tree's archives were compiled by.
//
// The two have to agree because of a decision taken in
// specs/054-distribution.md for a different reason. Every object nanogo writes
// carries the "go object ..." header line verbatim from the installed
// toolchain, because a build is part nanogo and part gc and the linker rejects
// objects whose headers disagree. The tree's archives carry the pinned
// release's line. So a machine whose go command is a different release
// produces a main object the tree's archives cannot be linked with, and the
// failure arrives from go tool link, which names two releases and neither
// tree.
func (d *distribution) checkToolchain(installed string) error {
	if d.Go == installed {
		return nil
	}
	return fmt.Errorf("the distribution at %s was built with %s and the installed go command is %s: "+
		"nanogo copies the object header from the installed toolchain, so the two must be the same release; "+
		"install %s, or build against a distribution made with %s",
		d.Root, d.Go, installed, d.Go, installed)
}
