// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"runtime/debug"
	"sort"
	"strings"

	"golang.design/x/nanogo/loader"
)

// modInfoStart and modInfoEnd bracket the build information inside
// runtime.modinfo, per specs/015-export-data.md.
//
// The two sentinels are cmd/go's, byte for byte. runtime/debug.ReadBuildInfo
// drops sixteen bytes from each end of runtime.modinfo without reading them,
// and debug/buildinfo scans an executable's data for modInfoStart to find the
// blob without running the program. A blob with any other bracket is found by
// neither.
const (
	modInfoStart = "\x30\x77\xaf\x0c\x92\x74\x08\x02\x41\xe1\xc1\x07\xe6\xd6\x18\xe6"
	modInfoEnd   = "\xf9\x32\x43\x31\x86\x18\x20\x72\x00\x82\x42\x10\x41\x16\xd8\xf2"
)

// modInfoData is the value of runtime.modinfo for a rendered [debug.BuildInfo].
// It is cmd/go's modload.ModInfoData.
func modInfoData(info string) string {
	return modInfoStart + info + modInfoEnd
}

// devel is the version cmd/go records for a module that has none, which is
// every module built from a checkout rather than from the module cache.
const devel = "(devel)"

// listedModule is the module go list reported for one package.
//
// It is a decoding type. The fields are the four [debug.Module] holds, because
// that is what the modinfo blob carries and nothing else about a module
// reaches the executable.
type listedModule struct {
	Path    string
	Version string
	Sum     string
	Replace *listedModule
}

// module converts one record to the shape the blob is rendered from.
//
// The two rules are cmd/go's debugModFromModinfo. A module with no version is
// recorded as "(devel)". A replaced module carries no checksum of its own: the
// bytes that were built are the replacement's, so the checksum belongs to the
// replacement.
func (m *listedModule) module() *debug.Module {
	version := m.Version
	if version == "" {
		version = devel
	}
	d := &debug.Module{Path: m.Path, Version: version}
	if m.Replace != nil {
		d.Replace = m.Replace.module()
		return d
	}
	d.Sum = m.Sum
	return d
}

// modulesFormat asks go list for the module behind each package.
//
// The fields are tab separated and positional: the import path alone when the
// package belongs to no module, which is every standard library package and
// every package built outside a module; four fields when it does; seven when
// the module is replaced. The counts cannot collide, so the reader needs no
// lookahead.
const modulesFormat = "{{.ImportPath}}" +
	"{{with .Module}}\t{{.Path}}\t{{.Version}}\t{{.Sum}}" +
	"{{with .Replace}}\t{{.Path}}\t{{.Version}}\t{{.Sum}}{{end}}{{end}}"

// modules reports the module each package in this build came from.
//
// It is a second listing over the same patterns. The first one goes through
// the loader, whose Package does not carry the module, and a module is not a
// property of a package that nanogo can derive: it is the answer module
// resolution gave, so the go command is the only thing that has it. The
// listing asks for no export data, so it builds nothing.
func (b *builder) modules() (map[string]*listedModule, error) {
	out, err := b.runGo(append([]string{"list", "-deps", "-f", modulesFormat}, b.opts.Patterns...)...)
	if err != nil {
		return nil, err
	}
	return parseModules(out), nil
}

// parseModules decodes the listing [modulesFormat] asks for.
func parseModules(out []byte) map[string]*listedModule {
	mods := make(map[string]*listedModule)
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 || fields[0] == "" || fields[1] == "" {
			continue
		}
		m := &listedModule{Path: fields[1], Version: fields[2], Sum: fields[3]}
		if len(fields) >= 7 && fields[4] != "" {
			m.Replace = &listedModule{Path: fields[4], Version: fields[5], Sum: fields[6]}
		}
		mods[fields[0]] = m
	}
	return mods
}

// buildSettings is what nanogo can state as fact about the executable it is
// about to link.
//
// Every setting here is one nanogo decided or one the go command reported for
// this build. cmd/go's setBuildInfo records more, and the ones nanogo leaves
// out are left out because recording them would be a claim about an executable
// nanogo did not produce:
//
//   - DefaultGODEBUG. The go command computes it from the main module's go
//     directive and then passes -X runtime.godebugDefault to the linker.
//     nanogo passes no such -X, so the executable has the runtime's own
//     defaults and a recorded setting would describe a value it does not hold.
//   - GOARM64, and the architecture feature level of every other GOARCH.
//     nanogo emits baseline arm64 whatever the level says, so recording a
//     level would claim the code was tuned for it.
//   - GOEXPERIMENT, CGO_CFLAGS, CGO_CPPFLAGS, CGO_CXXFLAGS and CGO_LDFLAGS.
//     cmd/go records the raw environment variable, and go env answers with
//     the effective value: an unset CGO_CFLAGS is recorded as empty and go
//     env prints "-O2 -g". Recording go env's answer would disagree with the
//     record gc writes for the same build, and the raw value is not
//     obtainable from outside the go command.
//   - vcs, vcs.revision, vcs.time and vcs.modified. nanogo runs no version
//     control command and stamps nothing.
//
// -buildmode is exe because nanogo passes no -buildmode to go tool link, so
// the linker's default is what the executable gets. That is the value cmd/go
// records for its own default build too, on darwin/arm64 included, where the
// Mach-O header carries the PIE flag either way.
//
// -compiler is gc because the key names the toolchain flag, which selects an
// object format, an archive layout and a linker. nanogo writes gc objects,
// reads gc archives and links with gc's linker, so gc is the true value.
// Which binary compiled which package is a different question, and the report
// nanogo prints on every build is where it is answered.
func (b *builder) buildSettings() ([]debug.BuildSetting, error) {
	env, err := b.goEnvValues("CGO_ENABLED", "GOARCH", "GOOS")
	if err != nil {
		return nil, err
	}
	// The order is cmd/go's: the flags first, sorted, then the environment.
	return []debug.BuildSetting{
		{Key: "-buildmode", Value: "exe"},
		{Key: "-compiler", Value: "gc"},
		{Key: "CGO_ENABLED", Value: env[0]},
		{Key: "GOARCH", Value: env[1]},
		{Key: "GOOS", Value: env[2]},
	}, nil
}

// buildInfo is what runtime/debug.ReadBuildInfo will report for one executable.
//
// The shape is cmd/go's setBuildInfo. GoVersion is deliberately empty: the
// linker stores the toolchain version separately and ReadBuildInfo overwrites
// the field with runtime.Version(), so a version written here would be encoded
// twice and could disagree with itself.
//
// Deps is every module beneath the main package except the main module's own,
// one entry per module path. The transitive closure is the loader's Deps,
// which is the same set cmd/go walks, and a standard library package belongs
// to no module and so contributes nothing.
func buildInfo(main *loader.Package, mods map[string]*listedModule, settings []debug.BuildSetting) *debug.BuildInfo {
	info := &debug.BuildInfo{Path: main.ImportPath, Settings: settings}
	if m := mods[main.ImportPath]; m != nil {
		info.Main = *m.module()
	}
	seen := make(map[string]bool)
	for _, dep := range main.Deps {
		m := mods[dep]
		if m == nil || m.Path == info.Main.Path || seen[m.Path] {
			continue
		}
		seen[m.Path] = true
		info.Deps = append(info.Deps, m.module())
	}
	// Sorted by module path, which is a total order because the loop above
	// keeps one entry per path (specs/053-determinism.md). Two versions of one
	// module path cannot both be in a build, so no tie is possible.
	sort.Slice(info.Deps, func(i, j int) bool { return info.Deps[i].Path < info.Deps[j].Path })
	return info
}

// goEnvValues asks the go command for the named settings, in order.
func (b *builder) goEnvValues(names ...string) ([]string, error) {
	out, err := b.runGo(append([]string{"env"}, names...)...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	if len(lines) < len(names) {
		return nil, fmt.Errorf("go env %s answered %q", strings.Join(names, " "), out)
	}
	values := make([]string, len(names))
	for i := range names {
		values[i] = strings.TrimSpace(lines[i])
	}
	return values, nil
}
