// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// A Package is one archive to install into a distribution.
type Package struct {
	// Path is the import path.
	Path string
	// Archive is the .a file on disk, as the go command's build cache holds
	// it.
	Archive string
	// Producer is the compiler that wrote it. The zero value means the
	// archive is gc's and its release is read out of its own object header,
	// which is what [Closure] produces. A caller that compiled the archive
	// itself sets this, because a producer is declared by the producer.
	Producer Producer
}

// closureProgram is the program whose dependencies define the bootstrap
// closure: the smallest Go program there is.
//
// Its closure is what a distribution must hold before it can link anything at
// all, and its size is the denominator the tally reports. It is 29 packages by
// `go list -deps`, of which one is the program itself and one is unsafe, which
// has no code and therefore no archive. So 27 archives.
const closureProgram = "package main\n\nfunc main() {}\n"

// closureModule is the module path of the throwaway module. It is never
// resolved, and it is the one import path go list reports that a distribution
// does not hold.
const closureModule = "nanogodist"

// Closure builds the bootstrap closure with the go command and reports the
// archives it produced.
//
// The archives come from the go command's build cache rather than from a
// prebuilt GOROOT/pkg, because Go has shipped no prebuilt standard library
// since 1.20. -trimpath is not optional: without it the archives carry the
// absolute path of the directory they were built in, and two release runs in
// different directories produce different bytes.
func Closure(goCmd, goos, goarch string) ([]Package, error) {
	dir, err := os.MkdirTemp("", "nanogo-closure")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	files := map[string]string{
		"go.mod":  "module " + closureModule + "\n\ngo 1.27\n",
		"main.go": closureProgram,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return nil, err
		}
	}
	cmd := exec.Command(goCmd, "list", "-deps", "-export", "-trimpath",
		"-f", "{{.ImportPath}}\t{{.Export}}", ".")
	cmd.Dir = dir
	// CGO_ENABLED=0 per specs/000-decisions.md decision 8. Nothing in this
	// closure uses cgo, and pinning it keeps the archives the same whether or
	// not the release runner has a C toolchain.
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if e, ok := err.(*exec.ExitError); ok {
			stderr = e.Stderr
		}
		return nil, fmt.Errorf("go list for %s/%s: %v: %s", goos, goarch, err, stderr)
	}
	return parseClosure(string(out))
}

// parseClosure reads go list's answer.
//
// Two lines are dropped, and both would otherwise put something in pkg that
// does not belong there: the throwaway main package, and unsafe, which has no
// code and so no export file.
func parseClosure(out string) ([]Package, error) {
	var pkgs []Package
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		path, archive, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("go list wrote %q, which is not an import path and an export file", line)
		}
		if path == closureModule || archive == "" {
			continue
		}
		pkgs = append(pkgs, Package{Path: path, Archive: archive})
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("go list reported no archives, so the closure is empty")
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Path < pkgs[j].Path })
	return pkgs, nil
}
