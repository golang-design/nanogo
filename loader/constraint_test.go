// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package loader

import (
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// testContext returns a context for goos and goarch with the compiler tag set
// and cgo off.
func testContext(goos, goarch string) *Context {
	c := DefaultContext()
	c.GOOS = goos
	c.GOARCH = goarch
	return c
}

func TestDefaultContext(t *testing.T) {
	c := DefaultContext()
	if c.GOOS != runtime.GOOS || c.GOARCH != runtime.GOARCH {
		t.Errorf("DefaultContext targets %s/%s, want %s/%s", c.GOOS, c.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	// specs/014-package-loader.md: the compiler tag is gc, not nanogo,
	// because the distribution branches on gc against gccgo.
	if c.Compiler != "gc" {
		t.Errorf("compiler tag is %q, want gc", c.Compiler)
	}
	if c.CgoEnabled {
		t.Error("cgo is on by default; specs/000-decisions.md decision 8 says nanogo has no cgo")
	}
	last := c.ReleaseTags[len(c.ReleaseTags)-1]
	if last != "go1."+itoa(goReleaseMinor) {
		t.Errorf("last release tag is %q, want go1.%d", last, goReleaseMinor)
	}
	if c.ReleaseTags[0] != "go1.1" {
		t.Errorf("first release tag is %q, want go1.1", c.ReleaseTags[0])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestKnownTables(t *testing.T) {
	for _, s := range []string{"darwin", "linux", "windows", "nacl", "ios"} {
		if !KnownOS(s) {
			t.Errorf("KnownOS(%q) = false", s)
		}
	}
	for _, s := range []string{"amd64", "arm64", "wasm", "loong64", "sparc"} {
		if !KnownArch(s) {
			t.Errorf("KnownArch(%q) = false", s)
		}
	}
	for _, s := range []string{"test", "unix", "gc", "amd65"} {
		if KnownOS(s) || KnownArch(s) {
			t.Errorf("%q is in a known table", s)
		}
	}
}

func TestMatchTag(t *testing.T) {
	tests := []struct {
		goos, goarch string
		cgo          bool
		tags         []string
		tag          string
		want         bool
	}{
		{goos: "darwin", goarch: "arm64", tag: "darwin", want: true},
		{goos: "darwin", goarch: "arm64", tag: "arm64", want: true},
		{goos: "darwin", goarch: "arm64", tag: "gc", want: true},
		{goos: "darwin", goarch: "arm64", tag: "gccgo", want: false},
		{goos: "darwin", goarch: "arm64", tag: "nanogo", want: false},
		{goos: "darwin", goarch: "arm64", tag: "unix", want: true},
		{goos: "windows", goarch: "amd64", tag: "unix", want: false},
		{goos: "js", goarch: "wasm", tag: "unix", want: false},
		{goos: "darwin", goarch: "arm64", tag: "cgo", want: false},
		{goos: "darwin", goarch: "arm64", cgo: true, tag: "cgo", want: true},
		{goos: "android", goarch: "arm64", tag: "linux", want: true},
		{goos: "linux", goarch: "arm64", tag: "android", want: false},
		{goos: "illumos", goarch: "amd64", tag: "solaris", want: true},
		{goos: "ios", goarch: "arm64", tag: "darwin", want: true},
		{goos: "darwin", goarch: "arm64", tag: "go1.21", want: true},
		{goos: "darwin", goarch: "arm64", tag: "go1.99", want: false},
		{goos: "darwin", goarch: "arm64", tags: []string{"purego"}, tag: "purego", want: true},
		{goos: "darwin", goarch: "arm64", tag: "purego", want: false},
	}
	for _, tt := range tests {
		c := testContext(tt.goos, tt.goarch)
		c.CgoEnabled = tt.cgo
		c.BuildTags = tt.tags
		if got := c.MatchTag(tt.tag); got != tt.want {
			t.Errorf("%s/%s cgo=%v tags=%v: MatchTag(%q) = %v, want %v",
				tt.goos, tt.goarch, tt.cgo, tt.tags, tt.tag, got, tt.want)
		}
	}
}

func TestMatchTagBoringcrypto(t *testing.T) {
	c := testContext("linux", "amd64")
	if c.MatchTag("boringcrypto") {
		t.Error("boringcrypto matches with no experiment set")
	}
	c.ToolTags = []string{"goexperiment.boringcrypto"}
	if !c.MatchTag("boringcrypto") {
		t.Error("boringcrypto does not match goexperiment.boringcrypto")
	}
}

// TestMatchName covers the file name rules of
// specs/014-package-loader.md. The x_test.go against vector_amd64.go pair is
// the rule the spec calls out: a component constrains the file only when it is
// a known GOOS or GOARCH value.
func TestMatchName(t *testing.T) {
	tests := []struct {
		name         string
		goos, goarch string
		want         bool
	}{
		// A component that is not in the tables is not a constraint.
		{name: "x_test.go", goos: "darwin", goarch: "arm64", want: true},
		{name: "x_test.go", goos: "linux", goarch: "amd64", want: true},
		{name: "vector_amd64.go", goos: "darwin", goarch: "arm64", want: false},
		{name: "vector_amd64.go", goos: "linux", goarch: "amd64", want: true},

		// A leading _ or . excludes the file whatever else the name says.
		{name: "_amd64.go", goos: "linux", goarch: "amd64", want: false},
		{name: "_x.go", goos: "linux", goarch: "amd64", want: false},
		{name: ".x.go", goos: "linux", goarch: "amd64", want: false},
		{name: ".x_amd64.go", goos: "linux", goarch: "amd64", want: false},

		// GOOS only.
		{name: "x_linux.go", goos: "linux", goarch: "amd64", want: true},
		{name: "x_linux.go", goos: "darwin", goarch: "arm64", want: false},
		{name: "x_windows.go", goos: "linux", goarch: "amd64", want: false},

		// GOOS and GOARCH.
		{name: "x_linux_amd64.go", goos: "linux", goarch: "amd64", want: true},
		{name: "x_linux_amd64.go", goos: "linux", goarch: "arm64", want: false},
		{name: "x_linux_amd64.go", goos: "darwin", goarch: "amd64", want: false},

		// The order is GOOS then GOARCH. Reversed, only the last component
		// counts, and it is a known GOOS.
		{name: "x_amd64_linux.go", goos: "linux", goarch: "amd64", want: true},
		{name: "x_amd64_linux.go", goos: "linux", goarch: "arm64", want: true},
		{name: "x_amd64_linux.go", goos: "darwin", goarch: "amd64", want: false},

		// A _test suffix is stripped before the components are read.
		{name: "x_linux_test.go", goos: "linux", goarch: "amd64", want: true},
		{name: "x_linux_test.go", goos: "darwin", goarch: "arm64", want: false},
		{name: "x_linux_amd64_test.go", goos: "linux", goarch: "amd64", want: true},
		{name: "x_linux_amd64_test.go", goos: "linux", goarch: "arm64", want: false},

		// A name with no prefix before the first underscore is not tagged.
		// This is why an operating system can be added without breaking an
		// existing file called after it.
		{name: "linux.go", goos: "darwin", goarch: "arm64", want: true},
		{name: "amd64.go", goos: "darwin", goarch: "arm64", want: true},

		// The first dot ends the name, so a multi-dot name still matches.
		{name: "x_linux.pb.go", goos: "linux", goarch: "amd64", want: true},
		{name: "x_linux.pb.go", goos: "darwin", goarch: "arm64", want: false},

		// Aliases.
		{name: "x_linux.go", goos: "android", goarch: "arm64", want: true},
		{name: "x_darwin.go", goos: "ios", goarch: "arm64", want: true},
		{name: "x_solaris.go", goos: "illumos", goarch: "amd64", want: true},

		// Extensions a package cannot hold.
		{name: "x.txt", goos: "linux", goarch: "amd64", want: false},
		{name: "README", goos: "linux", goarch: "amd64", want: false},
		{name: "x_linux.s", goos: "linux", goarch: "amd64", want: true},
		{name: "x_linux.s", goos: "darwin", goarch: "arm64", want: false},
		{name: "x.syso", goos: "linux", goarch: "amd64", want: true},
		{name: "x.c", goos: "linux", goarch: "amd64", want: true},
		{name: "x.h", goos: "linux", goarch: "amd64", want: true},
	}
	for _, tt := range tests {
		c := testContext(tt.goos, tt.goarch)
		if got := c.MatchName(tt.name); got != tt.want {
			t.Errorf("%s/%s: MatchName(%q) = %v, want %v", tt.goos, tt.goarch, tt.name, got, tt.want)
		}
	}
}

// TestShouldBuild covers //go:build and // +build, including the rule that
// //go:build wins when both are present.
func TestShouldBuild(t *testing.T) {
	tests := []struct {
		desc    string
		content string
		want    bool
		wantErr bool
	}{
		{
			desc:    "no constraint",
			content: "package p\n",
			want:    true,
		},
		{
			desc:    "go:build true",
			content: "//go:build linux\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "go:build false",
			content: "//go:build windows\n\npackage p\n",
			want:    false,
		},
		{
			desc:    "go:build and",
			content: "//go:build linux && amd64\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "go:build and false",
			content: "//go:build linux && arm64\n\npackage p\n",
			want:    false,
		},
		{
			desc:    "go:build or",
			content: "//go:build windows || linux\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "go:build not",
			content: "//go:build !windows\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "go:build parens",
			content: "//go:build (linux || darwin) && !cgo\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "go:build ignore",
			content: "//go:build ignore\n\npackage main\n",
			want:    false,
		},
		{
			desc:    "plus build true",
			content: "// +build linux\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "plus build false",
			content: "// +build windows\n\npackage p\n",
			want:    false,
		},
		{
			desc:    "plus build space is or, comma is and",
			content: "// +build windows linux\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "plus build comma false",
			content: "// +build linux,arm64\n\npackage p\n",
			want:    false,
		},
		{
			desc:    "plus build two lines are and",
			content: "// +build linux\n// +build arm64\n\npackage p\n",
			want:    false,
		},
		{
			// specs/014-package-loader.md: when both are present the
			// //go:build line wins.
			desc:    "both present, go:build wins and allows",
			content: "//go:build linux\n// +build windows\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "both present, go:build wins and rejects",
			content: "//go:build windows\n// +build linux\n\npackage p\n",
			want:    false,
		},
		{
			desc:    "constraint after the package clause is not a constraint",
			content: "package p\n\n//go:build windows\n",
			want:    true,
		},
		{
			desc:    "constraint in a doc comment with no blank line before package",
			content: "// Package p does things.\n//go:build windows\npackage p\n",
			want:    false,
		},
		{
			desc:    "constraint after a license header",
			content: "// Copyright.\n\n//go:build linux\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "constraint inside a block comment is not one",
			content: "/*\n//go:build windows\n*/\n\npackage p\n",
			want:    true,
		},
		{
			// A block comment ends the run of // comments, so a +build line
			// after it is outside the header and is not a constraint.
			desc:    "plus build after a block comment is not a constraint",
			content: "/* c */\n// +build windows\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "two go:build lines are an error",
			content: "//go:build linux\n//go:build amd64\n\npackage p\n",
			wantErr: true,
		},
		{
			desc:    "unparseable go:build is an error",
			content: "//go:build linux &&\n\npackage p\n",
			wantErr: true,
		},
		{
			// A double negation is a legacy spelling that never holds.
			desc:    "plus build double negation never holds",
			content: "// +build !!linux\n\npackage p\n",
			want:    false,
		},
		{
			desc:    "plus build that does not parse is ignored",
			content: "// +build " + strings.Repeat("linux ", 200) + "\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "line with +build in prose is not a constraint",
			content: "// see the +build lines below\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "go:buildsomething is not a constraint",
			content: "//go:buildwindows\n\npackage p\n",
			want:    true,
		},
		{
			desc:    "empty file",
			content: "",
			want:    true,
		},
		{
			desc:    "no trailing newline",
			content: "//go:build windows",
			want:    false,
		},
	}
	c := testContext("linux", "amd64")
	for _, tt := range tests {
		got, err := c.ShouldBuild([]byte(tt.content))
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: error = %v, wantErr = %v", tt.desc, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("%s: ShouldBuild = %v, want %v", tt.desc, got, tt.want)
		}
	}
}

func TestMatchFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package p\n")
	write("b_windows.go", "package p\n")
	write("c.go", "//go:build windows\n\npackage p\n")
	write("d.go", "//go:build linux &&\n\npackage p\n")
	write("e.txt", "not source\n")
	write("f.syso", "\x00\x01")
	write("_g.go", "package p\n")

	c := testContext("linux", "amd64")
	tests := []struct {
		name    string
		want    bool
		wantErr bool
	}{
		{name: "a.go", want: true},
		{name: "b_windows.go", want: false},
		{name: "c.go", want: false},
		{name: "d.go", wantErr: true},
		{name: "e.txt", want: false},
		{name: "f.syso", want: true},
		{name: "_g.go", want: false},
	}
	for _, tt := range tests {
		got, err := c.MatchFile(dir, tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("MatchFile(%q) error = %v, wantErr = %v", tt.name, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("MatchFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}

	if _, err := c.MatchFile(dir, "missing.go"); err == nil {
		t.Error("MatchFile on a missing file returned no error")
	}
}

// goSrcRoot returns the source tree of the Go distribution, or "" when it is
// not on this machine.
func goSrcRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	src := filepath.Join(home, "dev", "go.dev", "go", "src")
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return ""
	}
	return src
}

// TestConstraintCorpus runs the evaluator over every .go file of the Go
// distribution and asserts it agrees with go/build.
//
// specs/014-package-loader.md requires the evaluator to agree with the go
// command. The distribution is the corpus that proves it: it is large, it is
// free, and it exercises every form of constraint that ships.
func TestConstraintCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus test is slow")
	}
	src := goSrcRoot()
	if src == "" {
		t.Skip("no Go source tree at ~/dev/go.dev/go/src")
	}

	targets := []struct{ goos, goarch string }{
		{"darwin", "arm64"},
		{"linux", "amd64"},
	}
	for _, target := range targets {
		t.Run(target.goos+"_"+target.goarch, func(t *testing.T) {
			bc := build.Default
			bc.GOOS = target.goos
			bc.GOARCH = target.goarch
			bc.CgoEnabled = false
			bc.Compiler = "gc"

			c := testContext(target.goos, target.goarch)
			// The experiment and target feature tags are caller supplied.
			// This is the seam: the evaluator is under test here, not the
			// computation of the experiment defaults, which is G2 work.
			c.ToolTags = bc.ToolTags

			var files, mismatches, errFiles int
			err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					// testdata holds files that are deliberately broken
					// and are never part of a package. go/build parses a
					// file and reports a syntax error from MatchFile; the
					// evaluator here only scans header comments, so the two
					// report errors differently on a broken file.
					name := d.Name()
					if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
						return fs.SkipDir
					}
					return nil
				}
				if !strings.HasSuffix(d.Name(), ".go") {
					return nil
				}
				dir := filepath.Dir(path)
				want, wantErr := bc.MatchFile(dir, d.Name())
				got, gotErr := c.MatchFile(dir, d.Name())
				files++
				if wantErr != nil || gotErr != nil {
					errFiles++
				}
				if got != want || (gotErr != nil) != (wantErr != nil) {
					mismatches++
					if mismatches <= 20 {
						t.Errorf("%s: nanogo (%v, %v), go/build (%v, %v)",
							path, got, gotErr, want, wantErr)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if files == 0 {
				t.Fatal("walked no files")
			}
			t.Logf("%s/%s: %d files, %d with an error on either side, %d mismatches",
				target.goos, target.goarch, files, errFiles, mismatches)
		})
	}
}

// TestConstraintCorpusTags checks the evaluator against go/build with a build
// tag set, which changes which files are in.
func TestConstraintCorpusTags(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus test is slow")
	}
	src := goSrcRoot()
	if src == "" {
		t.Skip("no Go source tree at ~/dev/go.dev/go/src")
	}
	crypto := filepath.Join(src, "crypto")

	bc := build.Default
	bc.GOOS = "linux"
	bc.GOARCH = "amd64"
	bc.CgoEnabled = true
	bc.Compiler = "gc"
	bc.BuildTags = []string{"purego"}

	c := testContext("linux", "amd64")
	c.CgoEnabled = true
	c.BuildTags = []string{"purego"}
	c.ToolTags = bc.ToolTags

	var files, mismatches int
	err := filepath.WalkDir(crypto, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") && !strings.HasSuffix(d.Name(), ".s") {
			return nil
		}
		dir := filepath.Dir(path)
		want, wantErr := bc.MatchFile(dir, d.Name())
		got, gotErr := c.MatchFile(dir, d.Name())
		files++
		if got != want || (gotErr != nil) != (wantErr != nil) {
			mismatches++
			if mismatches <= 20 {
				t.Errorf("%s: nanogo (%v, %v), go/build (%v, %v)", path, got, gotErr, want, wantErr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("crypto with -tags purego and cgo: %d files, %d mismatches", files, mismatches)
}

func TestReleaseTagsOrder(t *testing.T) {
	tags := ReleaseTags()
	if len(tags) != goReleaseMinor {
		t.Fatalf("got %d release tags, want %d", len(tags), goReleaseMinor)
	}
	if !slices.IsSorted([]string{tags[0], "go1.z"}) {
		t.Fatal("unexpected tag order")
	}
	// The tags must be the ones go/build has, up to the pinned release.
	want := build.Default.ReleaseTags
	if len(want) >= len(tags) {
		for i := range tags {
			if tags[i] != want[i] {
				t.Errorf("release tag %d is %q, go/build has %q", i, tags[i], want[i])
			}
		}
	}
}
