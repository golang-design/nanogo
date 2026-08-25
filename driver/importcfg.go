// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"os"
	"strings"
)

// PackageEntry binds an import path to the file that holds the package.
type PackageEntry struct {
	Path string
	File string
}

// ImportMapping renames an import path. The go command uses it for vendored
// packages and for the test variants of a package.
type ImportMapping struct {
	Old string
	New string
}

// ImportCfg is a parsed -importcfg file. It maps an import path to the file
// that holds the package's export data, per specs/015-export-data.md.
//
// The entries are slices and not maps, so that anything built from them keeps
// the file's order, per specs/053-determinism.md. The lookup maps are for
// lookup only.
type ImportCfg struct {
	PackageFiles  []PackageEntry
	PackageShlibs []PackageEntry
	ImportMaps    []ImportMapping
	ModInfo       string

	byPath  map[string]string
	byShlib map[string]string
	byOld   map[string]string
}

// PackageFile reports the file that holds the named package's export data.
//
// It answers from the packagefile directives and from those only. A
// packageshlib directive names a shared library for the linker, not an
// archive, and gc's own compiler refuses the directive outright rather than
// reading it. A configuration that carries both for one path must resolve to
// the archive whichever order the two lines are in.
func (c *ImportCfg) PackageFile(path string) (string, bool) {
	if c == nil {
		return "", false
	}
	f, ok := c.byPath[path]
	return f, ok
}

// PackageShlib reports the shared library that holds the named package.
//
// Nothing in nanogo reads one yet. The accessor exists so that the two tables
// have the same shape when specs/045-linker.md needs the second one, and so
// that a caller reaching for a shared library cannot get an archive by
// accident.
func (c *ImportCfg) PackageShlib(path string) (string, bool) {
	if c == nil {
		return "", false
	}
	f, ok := c.byShlib[path]
	return f, ok
}

// Resolve applies the importmap directives to an import path.
func (c *ImportCfg) Resolve(path string) string {
	if c == nil {
		return path
	}
	if p, ok := c.byOld[path]; ok {
		return p
	}
	return path
}

// ReadImportCfg reads and parses the file named by -importcfg.
func ReadImportCfg(name string) (*ImportCfg, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("-importcfg: %v", err)
	}
	return ParseImportCfg(name, data)
}

// ParseImportCfg parses the contents of an -importcfg file.
//
// The format is one directive per line. An unknown directive is an error, for
// the reason specs/050-driver.md gives for unknown flags: a directive nanogo
// skipped would produce a build that silently differs from the one the go
// command described.
func ParseImportCfg(name string, data []byte) (*ImportCfg, error) {
	cfg := &ImportCfg{
		byPath:  make(map[string]string),
		byShlib: make(map[string]string),
		byOld:   make(map[string]string),
	}
	for i, line := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		verb, args, _ := strings.Cut(line, " ")
		args = strings.TrimSpace(args)
		before, after, hasEq := strings.Cut(args, "=")

		switch verb {
		case "packagefile":
			if !hasEq || before == "" || after == "" {
				return nil, syntaxError(name, lineNo, "packagefile", "packagefile path=filename")
			}
			cfg.PackageFiles = append(cfg.PackageFiles, PackageEntry{Path: before, File: after})
			cfg.byPath[before] = after
		case "packageshlib":
			if !hasEq || before == "" || after == "" {
				return nil, syntaxError(name, lineNo, "packageshlib", "packageshlib path=filename")
			}
			cfg.PackageShlibs = append(cfg.PackageShlibs, PackageEntry{Path: before, File: after})
			cfg.byShlib[before] = after
		case "importmap":
			if !hasEq || before == "" || after == "" {
				return nil, syntaxError(name, lineNo, "importmap", "importmap old=new")
			}
			cfg.ImportMaps = append(cfg.ImportMaps, ImportMapping{Old: before, New: after})
			cfg.byOld[before] = after
		case "modinfo":
			// modinfo carries an opaque blob, so it takes the rest of the line.
			if args == "" {
				return nil, syntaxError(name, lineNo, "modinfo", "modinfo data")
			}
			cfg.ModInfo = args
		default:
			return nil, fmt.Errorf("%s:%d: unknown directive %q", name, lineNo, verb)
		}
	}
	return cfg, nil
}

func syntaxError(name string, line int, verb, form string) error {
	return fmt.Errorf("%s:%d: invalid %s: syntax is %q", name, line, verb, form)
}
