// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseImportCfg(t *testing.T) {
	const data = `# comment

packagefile fmt=/pkg/fmt.a
packagefile errors=/pkg/errors.a
importmap golang.org/x/net=vendor/golang.org/x/net
packageshlib runtime=/pkg/libstd.so
modinfo "0w\xaf\f\x92t\b\x02A\xe1\xc1\a\xe6\xd6\x18\xe6"
`
	cfg, err := ParseImportCfg("importcfg", []byte(data))
	if err != nil {
		t.Fatalf("ParseImportCfg: %v", err)
	}
	wantFiles := []PackageEntry{
		{Path: "fmt", File: "/pkg/fmt.a"},
		{Path: "errors", File: "/pkg/errors.a"},
	}
	if !reflect.DeepEqual(cfg.PackageFiles, wantFiles) {
		t.Errorf("PackageFiles = %+v, want %+v", cfg.PackageFiles, wantFiles)
	}
	wantShlibs := []PackageEntry{{Path: "runtime", File: "/pkg/libstd.so"}}
	if !reflect.DeepEqual(cfg.PackageShlibs, wantShlibs) {
		t.Errorf("PackageShlibs = %+v, want %+v", cfg.PackageShlibs, wantShlibs)
	}
	wantMaps := []ImportMapping{{Old: "golang.org/x/net", New: "vendor/golang.org/x/net"}}
	if !reflect.DeepEqual(cfg.ImportMaps, wantMaps) {
		t.Errorf("ImportMaps = %+v, want %+v", cfg.ImportMaps, wantMaps)
	}
	if cfg.ModInfo == "" {
		t.Error("ModInfo is empty")
	}

	if f, ok := cfg.PackageFile("fmt"); !ok || f != "/pkg/fmt.a" {
		t.Errorf(`PackageFile("fmt") = %q, %v; want "/pkg/fmt.a", true`, f, ok)
	}
	if _, ok := cfg.PackageFile("strconv"); ok {
		t.Error(`PackageFile("strconv") reported found`)
	}
	if got := cfg.Resolve("golang.org/x/net"); got != "vendor/golang.org/x/net" {
		t.Errorf("Resolve = %q, want the mapped path", got)
	}
	if got := cfg.Resolve("fmt"); got != "fmt" {
		t.Errorf("Resolve(%q) = %q, want it unchanged", "fmt", got)
	}
}

// TestImportCfgNil checks that the accessors work before a file is read, so
// that a caller does not have to guard every use.
func TestImportCfgNil(t *testing.T) {
	var cfg *ImportCfg
	if _, ok := cfg.PackageFile("fmt"); ok {
		t.Error("PackageFile on a nil ImportCfg reported found")
	}
	if got := cfg.Resolve("fmt"); got != "fmt" {
		t.Errorf("Resolve on a nil ImportCfg = %q, want %q", got, "fmt")
	}
}

func TestParseImportCfgErrors(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"unknown directive", "packagefoo fmt=/pkg/fmt.a", `unknown directive "packagefoo"`},
		{"packagefile no equals", "packagefile fmt", "invalid packagefile"},
		{"packagefile no path", "packagefile =/pkg/fmt.a", "invalid packagefile"},
		{"packagefile no file", "packagefile fmt=", "invalid packagefile"},
		{"packageshlib no equals", "packageshlib runtime", "invalid packageshlib"},
		{"importmap no equals", "importmap old", "invalid importmap"},
		{"importmap no new", "importmap old=", "invalid importmap"},
		{"modinfo no data", "modinfo", "invalid modinfo"},
		{"bare word", "nonsense", `unknown directive "nonsense"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseImportCfg("importcfg", []byte("\n"+tt.data+"\n"))
			if err == nil {
				t.Fatalf("ParseImportCfg(%q) = no error, want one", tt.data)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
			// The line number lets the reader find the bad line.
			if !strings.HasPrefix(err.Error(), "importcfg:2:") {
				t.Errorf("error %q does not start with the file and line", err)
			}
		})
	}
}

func TestReadImportCfg(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "importcfg")
	if err := os.WriteFile(name, []byte("packagefile fmt=/pkg/fmt.a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadImportCfg(name)
	if err != nil {
		t.Fatalf("ReadImportCfg: %v", err)
	}
	if f, _ := cfg.PackageFile("fmt"); f != "/pkg/fmt.a" {
		t.Errorf("PackageFile = %q, want /pkg/fmt.a", f)
	}

	if _, err := ReadImportCfg(filepath.Join(dir, "missing")); err == nil {
		t.Error("ReadImportCfg on a missing file = no error, want one")
	} else if !strings.Contains(err.Error(), "-importcfg") {
		t.Errorf("error %q does not name the flag", err)
	}
}
