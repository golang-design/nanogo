// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Options is one distribution to build.
type Options struct {
	// Release is the nanogo release, in Go's shape, as "nanogo0.1.0". It is
	// line one of VERSION and the version in the tarball's name.
	Release string

	// GoVersion is the Go release the sources and archives are pinned to. It
	// is checked against what every gc-produced archive says about itself, so
	// a release job whose toolchain resolved to another patch release fails
	// here rather than shipping.
	GoVersion string

	// Target is the GOOS_GOARCH the archives are for, as "darwin_arm64".
	Target string

	// GOROOT is the pinned toolchain's root, which the sources are copied
	// from. Nothing of Go's standard library is committed to this repository:
	// it is enormous, the pin already exists, and a committed copy would have
	// to be kept in step by hand.
	GOROOT string

	// Binary is the nanogo command to install as bin/nanogo.
	Binary string

	// Commands are further binaries to install in bin, by name.
	Commands map[string]string

	// License is nanogo's own licence, installed at the root of the tree. It
	// governs everything except src, which carries Go's own.
	License string

	// Packages are the archives to install, from [Closure].
	Packages []Package

	// Out is the directory to build the tree in. It must not exist.
	Out string
}

// Build assembles a distribution tree and returns what its VERSION says.
//
// The last thing it does is [VerifyTree], so a tree this function returns
// without an error is a tree whose VERSION has been recomputed from the bytes
// of every archive in it.
func Build(o Options) (Version, error) {
	if o.GoVersion == "" {
		return Version{}, fmt.Errorf("a distribution names the Go release it is pinned to")
	}
	if _, err := TarballName(o.Release, o.Target); err != nil {
		return Version{}, err
	}
	if len(o.Packages) == 0 {
		return Version{}, fmt.Errorf("a distribution with no archives can link nothing")
	}
	if _, err := os.Stat(o.Out); err == nil {
		return Version{}, fmt.Errorf("%s already exists; a distribution is built into a fresh directory so that a stale file cannot survive into it", o.Out)
	}
	if err := os.MkdirAll(o.Out, dirMode); err != nil {
		return Version{}, err
	}

	if err := copyFile(o.Binary, filepath.Join(o.Out, "bin", "nanogo"), exeMode); err != nil {
		return Version{}, err
	}
	// A slice built from the map's keys would range over the map, which
	// specs/053-determinism.md forbids on a path that produces output. Every
	// destination here is fixed by its key, so the writes are independent and
	// the order does not reach the tree.
	for name, path := range o.Commands {
		if err := copyFile(path, filepath.Join(o.Out, "bin", name), exeMode); err != nil {
			return Version{}, err
		}
	}
	if err := copyFile(o.License, filepath.Join(o.Out, "LICENSE"), fileMode); err != nil {
		return Version{}, err
	}
	if _, err := CopySource(o.GOROOT, filepath.Join(o.Out, "src")); err != nil {
		return Version{}, err
	}

	v := Version{Release: o.Release, Go: o.GoVersion, Target: o.Target}
	for _, p := range o.Packages {
		r, err := installArchive(o.Out, o.Target, p)
		if err != nil {
			return Version{}, err
		}
		v.Packages++
		if r.Producer.IsNanogo() {
			v.ByNanogo++
		} else {
			v.ByGc++
		}
	}
	if err := os.WriteFile(filepath.Join(o.Out, VersionFile), []byte(v.String()), fileMode); err != nil {
		return Version{}, err
	}
	if err := VerifyTree(o.Out); err != nil {
		return Version{}, err
	}
	return v, nil
}

// installArchive copies one archive into pkg and makes it name its producer.
//
// An archive that already carries a record keeps it. That is the case a nanogo
// build produces, and it is why the producer is written by the producer rather
// than asserted here: this function knows only that the go command handed it a
// file, and the day nanogo compiles a standard library package the file will
// come from somewhere else.
//
// An archive with no record is gc's, and the release it names is read out of
// its own object header rather than taken from [Options.GoVersion]. The two
// are compared afterwards by [VerifyTree], which is the whole point of reading
// it rather than assuming it.
func installArchive(root, target string, p Package) (Record, error) {
	b, err := os.ReadFile(p.Archive)
	if err != nil {
		return Record{}, err
	}
	r, err := ReadRecord(b)
	if err != nil {
		version, verr := ToolchainVersion(b)
		if verr != nil {
			return Record{}, fmt.Errorf("%s: %v", p.Path, verr)
		}
		r = Record{Path: p.Path, Producer: Producer{Tool: GcTool, Version: version}}
		if b, err = AddRecord(b, r); err != nil {
			return Record{}, err
		}
	}
	if r.Path != p.Path {
		return Record{}, fmt.Errorf("%s carries a record for %s", p.Path, r.Path)
	}
	dst := filepath.Join(root, "pkg", target, filepath.FromSlash(p.Path)+archiveExt)
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		return Record{}, err
	}
	return r, os.WriteFile(dst, b, fileMode)
}

// TarballName is the file a release publishes, in Go's shape:
// nanogo0.1.0.darwin-arm64.tar.gz. The target is GOOS_GOARCH and the name
// spells it with a hyphen, which is the convention every Go download follows.
func TarballName(release, target string) (string, error) {
	goos, goarch, ok := strings.Cut(target, "_")
	if !ok || goos == "" || goarch == "" {
		return "", fmt.Errorf("target %q is not GOOS_GOARCH", target)
	}
	if release == "" {
		return "", fmt.Errorf("a tarball needs a release in its name")
	}
	return release + "." + goos + "-" + goarch + ".tar.gz", nil
}
