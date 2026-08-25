// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseClosureDropsWhatADistributionDoesNotHold(t *testing.T) {
	// The shape go list -deps -export prints: the throwaway main package has
	// an export file and does not belong in pkg, and unsafe has neither code
	// nor one.
	out := "internal/goarch\t/cache/goarch.a\n" +
		"unsafe\t\n" +
		"runtime\t/cache/runtime.a\n" +
		closureModule + "\t/cache/main.a\n"
	pkgs, err := parseClosure(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Package{{Path: "internal/goarch", Archive: "/cache/goarch.a"}, {Path: "runtime", Archive: "/cache/runtime.a"}}
	if len(pkgs) != len(want) {
		t.Fatalf("parseClosure gave %v, want %v", pkgs, want)
	}
	for i := range want {
		if pkgs[i] != want[i] {
			t.Fatalf("package %d is %v, want %v", i, pkgs[i], want[i])
		}
	}
}

func TestParseClosureRejectsWhatItCannotRead(t *testing.T) {
	if _, err := parseClosure("runtime /cache/runtime.a\n"); err == nil {
		t.Error("a line with no tab was accepted")
	}
	if _, err := parseClosure("unsafe\t\n"); err == nil {
		t.Error("an empty closure was accepted")
	}
}

func TestTarballName(t *testing.T) {
	got, err := TarballName("nanogo0.1.0", "darwin_arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "nanogo0.1.0.darwin-arm64.tar.gz" {
		t.Fatalf("TarballName = %q", got)
	}
	for _, c := range []struct{ release, target string }{
		{"nanogo0.1.0", "darwin"},
		{"nanogo0.1.0", "_arm64"},
		{"", "darwin_arm64"},
	} {
		if _, err := TarballName(c.release, c.target); err == nil {
			t.Errorf("TarballName(%q, %q) was accepted", c.release, c.target)
		}
	}
}

// options is a distribution built from fakes: a fake GOROOT, a fake binary and
// two fake archives. It exercises every step of Build without a go command.
func options(t *testing.T, pkgs ...Package) Options {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "nanogo")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	lic := filepath.Join(dir, "LICENSE")
	if err := os.WriteFile(lic, []byte("nanogo's licence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Options{
		Release:   "nanogo0.1.0",
		GoVersion: "go1.27.0",
		Target:    "darwin_arm64",
		GOROOT:    fakeGoroot(t),
		Binary:    bin,
		License:   lic,
		Packages:  pkgs,
		Out:       filepath.Join(dir, "tree"),
	}
}

// archive writes a gc archive with the given object header and no record,
// which is what the go command's build cache holds.
func archive(t *testing.T, header string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "pkg.a")
	if err := os.WriteFile(name, fakeArchive(t, header), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestBuildStampsEveryArchiveAndVerifiesTheResult(t *testing.T) {
	o := options(t,
		Package{Path: "internal/abi", Archive: archive(t, gcHeader)},
		Package{Path: "runtime", Archive: archive(t, gcHeader)})
	v, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	// The tally the tarball ships with today: nothing is nanogo's.
	if v.Packages != 2 || v.ByNanogo != 0 || v.ByGc != 2 {
		t.Fatalf("VERSION says %+v", v)
	}
	for _, name := range []string{
		"VERSION", "LICENSE",
		"bin/nanogo",
		"src/LICENSE", "src/PATENTS", "src/internal/abi/abi.go",
		"pkg/darwin_arm64/internal/abi.a", "pkg/darwin_arm64/runtime.a",
		"pkg/darwin_arm64/" + ManifestFile,
	} {
		if _, err := os.Stat(filepath.Join(o.Out, filepath.FromSlash(name))); err != nil {
			t.Errorf("the tree has no %s: %v", name, err)
		}
	}
	line, err := TallyLine(o.Out, o.Target)
	if err != nil {
		t.Fatal(err)
	}
	want := "nanogo: 0 of 2 packages in this distribution compiled by nanogo; 2 by gc go1.27.0"
	if line != want {
		t.Fatalf("TallyLine =\n\t%s\nwant\n\t%s", line, want)
	}
}

// An archive that already names its producer keeps that record. This is the
// case a nanogo-compiled package produces, and it is why Build never asserts a
// producer of its own.
func TestBuildKeepsARecordTheProducerWrote(t *testing.T) {
	stamped := archive(t, gcHeader)
	v, err := Build(options(t, Package{Path: "internal/abi", Archive: stamped, Producer: Producer{NanogoTool, "3fbcea1"}}, Package{Path: "runtime", Archive: archive(t, gcHeader)}))
	if err != nil {
		t.Fatal(err)
	}
	if v.ByNanogo != 1 || v.ByGc != 1 {
		t.Fatalf("VERSION says %+v, want one package each", v)
	}
}

// The release job's real failure mode: setup-go resolves 1.27.x to a patch
// release the pin does not name, and the archives say so.
func TestBuildFailsWhenTheToolchainIsNotThePinnedOne(t *testing.T) {
	o := options(t, Package{Path: "runtime", Archive: archive(t, "go object darwin arm64 go1.27.3 X:regabiwrapper")})
	_, err := Build(o)
	if err == nil || !strings.Contains(err.Error(), "compiled by gc go1.27.3 and VERSION pins go1.27.0") {
		t.Fatalf("Build = %v, want an error about the unpinned toolchain", err)
	}
}

func TestBuildRefusesWhatItCannotStandBehind(t *testing.T) {
	t.Run("no packages", func(t *testing.T) {
		o := options(t)
		if _, err := Build(o); err == nil {
			t.Error("a distribution with no archives was accepted")
		}
	})
	t.Run("no Go version", func(t *testing.T) {
		o := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader)})
		o.GoVersion = ""
		if _, err := Build(o); err == nil {
			t.Error("a distribution that pins no Go release was accepted")
		}
	})
	t.Run("a bad target", func(t *testing.T) {
		o := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader)})
		o.Target = "darwin"
		if _, err := Build(o); err == nil {
			t.Error("a target that is not GOOS_GOARCH was accepted")
		}
	})
	t.Run("an existing output directory", func(t *testing.T) {
		o := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader)})
		if err := os.MkdirAll(o.Out, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Build(o); err == nil {
			t.Error("an existing tree was built into, so a stale file could survive")
		}
	})
	t.Run("a missing binary", func(t *testing.T) {
		o := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader)})
		o.Binary = filepath.Join(t.TempDir(), "absent")
		if _, err := Build(o); err == nil {
			t.Error("a missing binary was accepted")
		}
	})
	t.Run("a missing licence", func(t *testing.T) {
		o := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader)})
		o.License = filepath.Join(t.TempDir(), "absent")
		if _, err := Build(o); err == nil {
			t.Error("a distribution with no licence was accepted")
		}
	})
	t.Run("a missing GOROOT", func(t *testing.T) {
		o := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader)})
		o.GOROOT = t.TempDir()
		if _, err := Build(o); err == nil {
			t.Error("a GOROOT with no sources was accepted")
		}
	})
	t.Run("a missing archive", func(t *testing.T) {
		o := options(t, Package{Path: "runtime", Archive: filepath.Join(t.TempDir(), "absent.a")})
		if _, err := Build(o); err == nil {
			t.Error("a missing archive was accepted")
		}
	})
	t.Run("an archive that is not one", func(t *testing.T) {
		name := filepath.Join(t.TempDir(), "runtime.a")
		if err := os.WriteFile(name, []byte("not an archive"), 0o600); err != nil {
			t.Fatal(err)
		}
		o := options(t, Package{Path: "runtime", Archive: name})
		if _, err := Build(o); err == nil {
			t.Error("a file that is not an archive was accepted")
		}
	})
	t.Run("a producer naming an unknown tool", func(t *testing.T) {
		o := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader), Producer: Producer{"clang", "17"}})
		if _, err := Build(o); err == nil {
			t.Error("an archive attributed to a tool the manifest cannot name was accepted")
		}
	})
}

// Build installs extra commands so that an unpacked tarball can answer
// questions about itself without a second download.
func TestBuildInstallsExtraCommands(t *testing.T) {
	o := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader)})
	extra := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(extra, []byte("tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	o.Commands = map[string]string{"nanogo-dist": extra}
	if _, err := Build(o); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(o.Out, "bin", "nanogo-dist"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("bin/nanogo-dist is %v and a command has to be executable", fi.Mode())
	}

	o2 := options(t, Package{Path: "runtime", Archive: archive(t, gcHeader)})
	o2.Commands = map[string]string{"gone": filepath.Join(t.TempDir(), "absent")}
	if _, err := Build(o2); err == nil {
		t.Error("a command that is not there was accepted")
	}
}
