// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package hygiene holds checks over the repository's own source.
//
// It exists because the compiler is subject to the same Go rules it
// implements, and it got one of them wrong: a file named machop_arm64.go was
// excluded from every build that did not target arm64, so the arm64 machine
// operation table vanished when cross-compiling and the package that referenced
// it failed to build. The rule is the one loader/constraint.go implements and
// tests against go/build over 6,821 files.
//
// Nothing here builds into the compiler.
package hygiene

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// knownGOOS and knownGOARCH are the values that make a file-name suffix a
// build constraint. The lists are pinned to the release in go.mod, the same
// way loader/constraint.go pins them, because there is no exported go/build
// data for them.
var knownGOOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,
}

var knownGOARCH = map[string]bool{
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true,
	"arm64": true, "arm64be": true, "loong64": true, "mips": true,
	"mips64": true, "mips64le": true, "mips64p32": true, "mips64p32le": true,
	"mipsle": true, "ppc": true, "ppc64": true, "ppc64le": true, "riscv": true,
	"riscv64": true, "s390": true, "s390x": true, "sparc": true,
	"sparc64": true, "wasm": true,
}

// intentional lists the files whose platform suffix is deliberate.
//
// An entry is a claim that the file genuinely belongs to one platform. The
// test below still fails if such a file is referenced from code that builds
// everywhere, because that is the failure this package exists to prevent.
var intentional = map[string]string{
	"driver/exec_unix.go":  "syscall.Exec replaces the process image and exists only on unix; exec_other.go is the counterpart",
	"driver/exec_other.go": "the counterpart to exec_unix.go",
}

// TestNoAccidentalPlatformConstraint walks the repository and reports a Go file
// whose name constrains it to a platform without saying so deliberately.
//
// The rule is Go's: a file named x_GOOS.go, x_GOARCH.go or x_GOOS_GOARCH.go is
// constrained, and only a non-empty prefix before the underscore makes the
// trailing component count. So arm64.go is not constrained and machop_arm64.go
// is.
func TestNoAccidentalPlatformConstraint(t *testing.T) {
	root := repoRoot(t)

	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "spikes" || name == "testdata" ||
				name == "upstream" || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if _, ok := intentional[filepath.ToSlash(rel)]; ok {
			return nil
		}
		if constrainedBy(name) != "" {
			found = append(found, filepath.ToSlash(rel)+" (by _"+constrainedBy(name)+")")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	for _, f := range found {
		t.Errorf("%s is excluded from builds for other platforms because of its name.\n"+
			"Rename it, or add it to intentional with the reason.", f)
	}
}

// constrainedBy returns the suffix that constrains the file, or "".
func constrainedBy(name string) string {
	base := strings.TrimSuffix(name, ".go")
	base = strings.TrimSuffix(base, "_test")
	i := strings.LastIndex(base, "_")
	// A non-empty prefix is required. A file called arm64.go is not tagged,
	// which is the rule a first reading of the documentation misses.
	if i <= 0 {
		return ""
	}
	last := base[i+1:]
	if knownGOOS[last] || knownGOARCH[last] {
		return last
	}
	return ""
}

// TestCrossCompiles is the property the name rule protects, checked directly.
//
// A compiler must be buildable from any host: nanogo targets arm64 and amd64,
// and neither target may require a host of the same architecture. This is what
// CI on linux/amd64 found when the arm64 machine operation table was excluded
// by its file name.
func TestCrossCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiling the whole module is slow")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	root := repoRoot(t)

	for _, target := range []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"darwin", "arm64"},
		{"linux", "arm64"},
	} {
		t.Run(target.goos+"_"+target.goarch, func(t *testing.T) {
			cmd := exec.Command("go", "build", "./...")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"GOOS="+target.goos, "GOARCH="+target.goarch, "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("building for %s/%s failed:\n%s", target.goos, target.goarch, out)
			}
		})
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
