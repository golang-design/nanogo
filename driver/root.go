// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// RootEnv names the tree nanogo takes the standard library from.
//
// It reads the way GOROOT does, and for the same reason. A toolchain that
// hunts for a standard library somewhere on the machine cannot say which one
// it used, and a compiler that cannot say that cannot be trusted about what it
// compiled. Go ships one tree holding the command, the sources and what was
// built from them, and nanogo's distribution is the same shape:
//
//	nanogo/bin/nanogo
//	nanogo/src/...              the pinned standard library sources
//	nanogo/pkg/darwin_arm64/    the archives built from them
//	nanogo/VERSION
//
// The variable is the one exception to "nanogo build takes no environment
// variables". It exists so that a developer running from a source checkout can
// point at a tree that is not beside the binary.
const RootEnv = "NANOGOROOT"

// Root is the tree a build takes the standard library and the runtime from.
type Root struct {
	// Path is the directory.
	Path string

	// Origin says how the directory was found, in a user's terms. It is
	// printed, so it is a phrase and not a token.
	Origin string

	// Nanogo reports whether the tree is a nanogo distribution rather than a
	// Go toolchain. The two are read differently: a distribution holds
	// archives under pkg, and a Go toolchain's archives are found through the
	// go command's build cache.
	Nanogo bool
}

// TargetDir is the name of the per-target directory inside a distribution's
// pkg tree, as GOOS_GOARCH.
func TargetDir() string { return runtime.GOOS + "_" + TargetArch }

// FindRoot resolves the tree the build takes the standard library from.
//
// The order is the nanogo distribution first and the Go toolchain last, so
// that shipping the tarball is a matter of populating a tree rather than of
// changing this function. The three steps are:
//
//  1. NANOGOROOT, when it is set. A value that does not hold a distribution is
//     an error rather than a silent fall back, for the reason
//     [AllowlistFromEnv] gives about a mistyped path: a variable that turned
//     itself off would read exactly like a working build.
//  2. The tree the running binary is installed in, one directory above bin.
//  3. The installed Go toolchain's GOROOT. No nanogo distribution is built
//     today, so this is the step every real build takes.
//
// goroot is the Go toolchain's root, which only the caller can ask the go
// command for. executable reports the running binary, which is os.Executable
// in production and a fake in a test.
func FindRoot(getenv func(string) string, executable func() (string, error), goroot string) (Root, error) {
	if dir := getenv(RootEnv); dir != "" {
		if !IsNanogoRoot(dir) {
			return Root{}, fmt.Errorf("%s=%s does not hold a nanogo distribution: it has no VERSION file and no pkg/%s directory",
				RootEnv, dir, TargetDir())
		}
		return Root{Path: dir, Origin: RootEnv, Nanogo: true}, nil
	}
	if exe, err := executable(); err == nil {
		// One directory above bin, which is where the command sits in every
		// tree of this shape.
		if dir := filepath.Dir(filepath.Dir(exe)); IsNanogoRoot(dir) {
			return Root{Path: dir, Origin: "the tree the nanogo binary is installed in", Nanogo: true}, nil
		}
	}
	if goroot == "" {
		return Root{}, fmt.Errorf("no nanogo distribution and no Go toolchain: set %s, or install Go", RootEnv)
	}
	return Root{Path: goroot, Origin: "the installed Go toolchain", Nanogo: false}, nil
}

// IsNanogoRoot reports whether dir holds a nanogo distribution.
//
// Two things are required and both are load-bearing. VERSION says which
// release the tree is, and pkg/GOOS_GOARCH holds the archives the linker
// reads. A tree with the sources and no archives is a checkout, and a build
// against it would have nothing to link.
func IsNanogoRoot(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "VERSION")); err != nil || fi.IsDir() {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "pkg", TargetDir()))
	return err == nil && fi.IsDir()
}
