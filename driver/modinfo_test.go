// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"os"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/loader"
)

// readModInfo takes a link configuration apart the way the linker and the
// runtime do, and returns what a program would read from it.
//
// The path is nanogo's own parser, then the linker's strconv.Unquote
// (cmd/link/internal/ld/ld.go), then ReadBuildInfo's sixteen bytes off each
// end, then debug.ParseBuildInfo. A blob that is not a real BuildInfo fails
// somewhere along it, which is the point: a stub that only makes
// ReadBuildInfo answer true does not get through here.
func readModInfo(t *testing.T, file string) *debug.BuildInfo {
	t.Helper()
	cfg, err := ReadImportCfg(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModInfo == "" {
		data, _ := os.ReadFile(file)
		t.Fatalf("the link configuration carries no modinfo line:\n%s", data)
	}
	blob, err := strconv.Unquote(cfg.ModInfo)
	if err != nil {
		t.Fatalf("the linker cannot unquote the modinfo argument: %v", err)
	}
	if !strings.HasPrefix(blob, modInfoStart) || !strings.HasSuffix(blob, modInfoEnd) {
		t.Fatalf("the blob is not bracketed by the sentinels: %q", blob)
	}
	// What runtime/debug.ReadBuildInfo does before it parses.
	if len(blob) < 32 {
		t.Fatalf("ReadBuildInfo refuses a blob of %d bytes", len(blob))
	}
	info, err := debug.ParseBuildInfo(blob[16 : len(blob)-16])
	if err != nil {
		t.Fatalf("ParseBuildInfo: %v", err)
	}
	return info
}

// TestModInfoSurvivesTheQuoting is the one step of the chain that is not
// nanogo's code on either side. The sentinels are not valid UTF-8, so a
// quoting that lost a byte would be found by the linker and not before.
func TestModInfoSurvivesTheQuoting(t *testing.T) {
	blob := modInfoData("path\texample.com/m\n")
	got, err := strconv.Unquote(fmt.Sprintf("%q", blob))
	if err != nil {
		t.Fatalf("Unquote: %v", err)
	}
	if got != blob {
		t.Errorf("the blob did not survive %%q: %q", got)
	}
	if len(modInfoStart) != 16 || len(modInfoEnd) != 16 {
		t.Errorf("the sentinels are %d and %d bytes, want 16 each",
			len(modInfoStart), len(modInfoEnd))
	}
}

// TestLinkWritesTheBuildInfo is the fix for the buildinfo-named probe, checked
// where it can be checked precisely. The probe only compares an exit status,
// so it cannot tell a real BuildInfo from a blob of the right length.
func TestLinkWritesTheBuildInfo(t *testing.T) {
	b, _ := testBuilder(t, &buildOptions{Patterns: []string{"."}})
	b.runGo = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "env":
			return []byte("1\narm64\ndarwin\n"), nil
		case args[0] == "list":
			return []byte(strings.Join([]string{
				"math/bits",
				"golang.org/x/arch/arm64/arm64asm\tgolang.org/x/arch\tv0.11.0\th1:sum=",
				"example.com/hello\texample.com/hello\t\t",
				"example.com/hello/lib\texample.com/hello\t\t",
			}, "\n")), nil
		case args[0] == "tool" && args[1] == "link":
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected go %s", strings.Join(args, " "))
	}
	main := &loader.Package{
		ImportPath: "example.com/hello",
		Name:       "main",
		Deps: []string{
			"example.com/hello/lib",
			"golang.org/x/arch/arm64/arm64asm",
			"math/bits",
		},
	}
	own := map[string]string{"example.com/hello": "/work/0.a"}
	if err := b.link([]*loader.Package{main}, nil, nil, own); err != nil {
		t.Fatalf("link: %v", err)
	}

	info := readModInfo(t, b.work+"/importcfg.link.0")
	if info.Path != "example.com/hello" {
		t.Errorf("path = %q, want example.com/hello", info.Path)
	}
	// A module built from a checkout has no version, and cmd/go records
	// "(devel)" for it.
	want := debug.Module{Path: "example.com/hello", Version: devel}
	if info.Main != want {
		t.Errorf("mod = %+v, want %+v", info.Main, want)
	}
	// One dependency module. The main module's own package is not one, and a
	// standard library package belongs to no module.
	if len(info.Deps) != 1 {
		t.Fatalf("deps = %+v, want one module", info.Deps)
	}
	if d := *info.Deps[0]; d != (debug.Module{Path: "golang.org/x/arch", Version: "v0.11.0", Sum: "h1:sum="}) {
		t.Errorf("dep = %+v", d)
	}
	// The settings nanogo can state as fact, in cmd/go's order.
	got := make([]string, 0, len(info.Settings))
	for _, s := range info.Settings {
		got = append(got, s.Key+"="+s.Value)
	}
	wantSettings := []string{
		"-buildmode=exe", "-compiler=gc",
		"CGO_ENABLED=1", "GOARCH=arm64", "GOOS=darwin",
	}
	if !reflect.DeepEqual(got, wantSettings) {
		t.Errorf("settings = %q, want %q", got, wantSettings)
	}
	// GoVersion is the linker's to store, and ReadBuildInfo replaces it with
	// runtime.Version(). Writing it here would encode it twice.
	if info.GoVersion != "" {
		t.Errorf("the blob carries a go version %q", info.GoVersion)
	}
}

// A replaced module records the replacement's checksum and none of its own,
// because the bytes that were built are the replacement's.
func TestModulesRecordAReplacement(t *testing.T) {
	mods := parseModules([]byte(
		"internal/goarch\n" +
			"golang.org/x/arch/arm64/arm64asm\tgolang.org/x/arch\tv0.11.0\t\tgolang.org/x/arch\tv0.22.0\th1:new=\n" +
			"example.com/m\texample.com/m\t\t\n"))
	if len(mods) != 2 {
		t.Fatalf("parseModules found %d modules, want 2", len(mods))
	}
	if mods["internal/goarch"] != nil {
		t.Error("a standard library package was given a module")
	}
	got := mods["golang.org/x/arch/arm64/arm64asm"].module()
	want := &debug.Module{
		Path:    "golang.org/x/arch",
		Version: "v0.11.0",
		Replace: &debug.Module{Path: "golang.org/x/arch", Version: "v0.22.0", Sum: "h1:new="},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("module = %+v, want %+v", got, want)
	}
	// The rendering is the one debug.ParseBuildInfo reads back: three columns
	// after "=>", and no checksum on the line above it.
	info := &debug.BuildInfo{Deps: []*debug.Module{got}}
	back, err := debug.ParseBuildInfo(info.String())
	if err != nil {
		t.Fatalf("ParseBuildInfo: %v", err)
	}
	if !reflect.DeepEqual(back.Deps, info.Deps) {
		t.Errorf("the replacement did not round trip: %+v", back.Deps[0])
	}
	// A directory replacement has no version, and takes "(devel)" too.
	dir := (&listedModule{Path: "example.com/m", Replace: &listedModule{Path: "../m"}}).module()
	if dir.Replace.Version != devel {
		t.Errorf("a directory replacement is version %q, want %q", dir.Replace.Version, devel)
	}
}

// A build outside a module has no main module. The blob still carries the
// package path and the settings, and ReadBuildInfo still answers, which is
// what go build produces there too.
func TestBuildInfoOutsideAModule(t *testing.T) {
	main := &loader.Package{ImportPath: commandLineArguments, Name: "main"}
	info := buildInfo(main, map[string]*listedModule{}, nil)
	if info.Main != (debug.Module{}) {
		t.Errorf("mod = %+v, want none", info.Main)
	}
	if info.Path != commandLineArguments {
		t.Errorf("path = %q", info.Path)
	}
	if _, err := debug.ParseBuildInfo(info.String()); err != nil {
		t.Errorf("ParseBuildInfo: %v", err)
	}
}

// The blob is assembled per main package, so two executables out of one
// command describe themselves and not each other.
func TestBuildInfoIsPerMainPackage(t *testing.T) {
	mods := parseModules([]byte(
		"example.com/m/a\texample.com/m\t\t\n" +
			"example.com/m/b\texample.com/m\t\t\n" +
			"example.com/dep\texample.com/dep\tv1.2.3\th1:s=\n"))
	a := buildInfo(&loader.Package{ImportPath: "example.com/m/a", Deps: []string{"example.com/dep"}}, mods, nil)
	b := buildInfo(&loader.Package{ImportPath: "example.com/m/b"}, mods, nil)
	if a.Path == b.Path {
		t.Fatal("the two executables share a path")
	}
	if len(a.Deps) != 1 || len(b.Deps) != 0 {
		t.Errorf("deps = %d and %d, want 1 and 0", len(a.Deps), len(b.Deps))
	}
}

// The go command is the only source of the module graph, so a listing that
// fails stops the link rather than producing an executable that lies about
// what went into it.
func TestLinkStopsWhenTheModuleListingFails(t *testing.T) {
	for _, tt := range []struct {
		name string
		fail string
	}{
		{"the module listing", "list"},
		{"the environment", "env"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := testBuilder(t, &buildOptions{Patterns: []string{"."}})
			b.runGo = func(args ...string) ([]byte, error) {
				if args[0] == tt.fail {
					return nil, fmt.Errorf("go %s failed", tt.fail)
				}
				return []byte("example.com/hello\texample.com/hello\t\t\n"), nil
			}
			main := &loader.Package{ImportPath: "example.com/hello", Name: "main"}
			err := b.link([]*loader.Package{main}, nil, nil, map[string]string{"example.com/hello": "/work/0.a"})
			if err == nil || !strings.Contains(err.Error(), tt.fail) {
				t.Fatalf("link = %v, want it to report the failing go command", err)
			}
		})
	}
}
