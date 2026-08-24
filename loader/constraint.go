// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package loader

import (
	"bytes"
	"errors"
	"fmt"
	"go/build/constraint"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// goReleaseMinor is the minor version of the Go release nanogo targets.
//
// The release tags go1.1 through go1.N follow from it. The value is pinned to
// the release in go.mod. Raise it when the module moves to a newer release.
const goReleaseMinor = 27

// CompilerTag is the value of the compiler build tag nanogo sets.
//
// specs/014-package-loader.md requires "gc" and not "nanogo". The distribution
// branches on gc against gccgo in several places. A third value would select
// neither branch, so nanogo would silently lose code the runtime needs.
const CompilerTag = "gc"

// Context holds the build configuration a file is tested against.
//
// The field set is the tag set of specs/014-package-loader.md plus ToolTags.
// The spec omits ToolTags, but the go command consults them, and files in the
// distribution are gated on goexperiment tags. Without the field, nanogo and
// the go command disagree about the runtime.
type Context struct {
	GOOS   string
	GOARCH string

	// CgoEnabled controls the cgo tag. nanogo does not support cgo
	// (specs/000-decisions.md decision 8), but the field must exist because
	// the loader must reproduce the file set of a cgo-enabled build when it
	// compares itself against the go command.
	CgoEnabled bool

	// Compiler is the compiler tag. Use CompilerTag.
	Compiler string

	// BuildTags is the -tags list from the command line.
	BuildTags []string

	// ToolTags holds experiment and target feature tags, such as
	// goexperiment.jsonv2 or arm64.v8.0. The caller supplies them.
	// Computing the experiment defaults is module resolution work and
	// belongs to G2.
	ToolTags []string

	// ReleaseTags holds go1.1 through go1.N. DefaultContext fills it.
	ReleaseTags []string
}

// DefaultContext returns a context for the host, with the compiler tag set and
// cgo off.
func DefaultContext() *Context {
	return &Context{
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		CgoEnabled:  false,
		Compiler:    CompilerTag,
		ReleaseTags: ReleaseTags(),
	}
}

// ReleaseTags returns go1.1 through the pinned release, in order.
func ReleaseTags() []string {
	tags := make([]string, 0, goReleaseMinor)
	for i := 1; i <= goReleaseMinor; i++ {
		tags = append(tags, "go1."+strconv.Itoa(i))
	}
	return tags
}

// knownOS is the set of past, present, and future GOOS values.
//
// The list comes from internal/syslist in the Go distribution, which go/build
// uses for the same purpose. That package is internal to the standard library,
// so the table is copied here. It is pinned to the Go release named by
// goReleaseMinor. Entries are never removed, because file name matching must
// keep rejecting a file for an operating system that no longer exists.
var knownOS = map[string]bool{
	"aix":       true,
	"android":   true,
	"darwin":    true,
	"dragonfly": true,
	"freebsd":   true,
	"hurd":      true,
	"illumos":   true,
	"ios":       true,
	"js":        true,
	"linux":     true,
	"nacl":      true,
	"netbsd":    true,
	"openbsd":   true,
	"plan9":     true,
	"solaris":   true,
	"wasip1":    true,
	"windows":   true,
	"zos":       true,
}

// unixOS is the set of GOOS values the unix tag matches. It is not used for
// file name matching. Source: internal/syslist.
var unixOS = map[string]bool{
	"aix":       true,
	"android":   true,
	"darwin":    true,
	"dragonfly": true,
	"freebsd":   true,
	"hurd":      true,
	"illumos":   true,
	"ios":       true,
	"linux":     true,
	"netbsd":    true,
	"openbsd":   true,
	"solaris":   true,
}

// knownArch is the set of past, present, and future GOARCH values.
// Source and pinning as for knownOS.
var knownArch = map[string]bool{
	"386":         true,
	"amd64":       true,
	"amd64p32":    true,
	"arm":         true,
	"armbe":       true,
	"arm64":       true,
	"arm64be":     true,
	"loong64":     true,
	"mips":        true,
	"mipsle":      true,
	"mips64":      true,
	"mips64le":    true,
	"mips64p32":   true,
	"mips64p32le": true,
	"ppc":         true,
	"ppc64":       true,
	"ppc64le":     true,
	"riscv":       true,
	"riscv64":     true,
	"s390":        true,
	"s390x":       true,
	"sparc":       true,
	"sparc64":     true,
	"wasm":        true,
}

// KnownOS reports whether name is a known GOOS value.
func KnownOS(name string) bool { return knownOS[name] }

// KnownArch reports whether name is a known GOARCH value.
func KnownArch(name string) bool { return knownArch[name] }

// sourceExts is the set of file extensions that can belong to a package.
// The list mirrors fileListForExt in go/build.
var sourceExts = map[string]bool{
	".go":      true,
	".c":       true,
	".cc":      true,
	".cpp":     true,
	".cxx":     true,
	".m":       true,
	".h":       true,
	".hh":      true,
	".hpp":     true,
	".hxx":     true,
	".f":       true,
	".F":       true,
	".for":     true,
	".f90":     true,
	".s":       true,
	".S":       true,
	".sx":      true,
	".swig":    true,
	".swigcxx": true,
	".syso":    true,
}

// MatchTag reports whether the build tag holds in this context.
//
// The alias rules follow the go command: android also matches linux, illumos
// also matches solaris, and ios also matches darwin, because those pairs share
// source. boringcrypto is the old spelling of goexperiment.boringcrypto.
func (c *Context) MatchTag(name string) bool {
	if c.CgoEnabled && name == "cgo" {
		return true
	}
	if name == c.GOOS || name == c.GOARCH || name == c.Compiler {
		return true
	}
	if c.GOOS == "android" && name == "linux" {
		return true
	}
	if c.GOOS == "illumos" && name == "solaris" {
		return true
	}
	if c.GOOS == "ios" && name == "darwin" {
		return true
	}
	if name == "unix" && unixOS[c.GOOS] {
		return true
	}
	if name == "boringcrypto" {
		name = "goexperiment.boringcrypto"
	}
	return contains(c.BuildTags, name) || contains(c.ToolTags, name) || contains(c.ReleaseTags, name)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// MatchName reports whether the file name alone allows the file.
//
// It applies the two name rules of specs/014-package-loader.md: a leading _ or
// . excludes the file, and a trailing _GOOS, _GOARCH, or _GOOS_GOARCH
// component constrains it. A component is a constraint only when it is a known
// GOOS or GOARCH value, which is why x_test.go is kept and vector_amd64.go is
// not, on a machine that is not amd64. The extension must be one a package can
// hold.
func (c *Context) MatchName(name string) bool {
	if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
		return false
	}
	i := strings.LastIndex(name, ".")
	if i < 0 {
		i = len(name)
	}
	if !sourceExts[name[i:]] {
		return false
	}
	return c.goodOSArchFile(name)
}

// goodOSArchFile reports whether the GOOS and GOARCH components of the name
// match the context.
//
// The recognised forms are name_GOOS.*, name_GOARCH.*, name_GOOS_GOARCH.*, and
// each of those with a _test before the extension.
func (c *Context) goodOSArchFile(name string) bool {
	name, _, _ = strings.Cut(name, ".")

	// A file called linux.go is not tagged. Only a non-empty prefix before the
	// first underscore makes the trailing component a constraint, so a new
	// operating system name cannot silently take over an existing file. This
	// rule dates from Go 1.4.
	i := strings.Index(name, "_")
	if i < 0 {
		return true
	}
	name = name[i:]

	l := strings.Split(name, "_")
	if n := len(l); n > 0 && l[n-1] == "test" {
		l = l[:n-1]
	}
	n := len(l)
	if n >= 2 && knownOS[l[n-2]] && knownArch[l[n-1]] {
		return c.MatchTag(l[n-1]) && c.MatchTag(l[n-2])
	}
	if n >= 1 && (knownOS[l[n-1]] || knownArch[l[n-1]]) {
		return c.MatchTag(l[n-1])
	}
	return true
}

// errMultipleGoBuild reports a file with more than one //go:build line.
var errMultipleGoBuild = errors.New("multiple //go:build comments")

var (
	slashSlash = []byte("//")
	slashStar  = []byte("/*")
	starSlash  = []byte("*/")
	plusBuild  = []byte("+build")
)

// ShouldBuild reports whether the build constraint comments in content allow
// the file.
//
// A //go:build line wins when both forms are present. When only the legacy
// // +build form is present, every such line must hold. The distribution still
// ships legacy-only files, so the form cannot be dropped
// (specs/014-package-loader.md).
//
// The expression itself is parsed by go/build/constraint. That package is the
// parser the go command uses. Writing a second one would create a source of
// disagreement with the go command and buy nothing.
func (c *Context) ShouldBuild(content []byte) (bool, error) {
	header, goBuild, err := parseFileHeader(content)
	if err != nil {
		return false, err
	}
	if goBuild != nil {
		x, err := constraint.Parse(string(goBuild))
		if err != nil {
			return false, fmt.Errorf("parsing //go:build line: %v", err)
		}
		return c.eval(x), nil
	}

	shouldBuild := true
	p := header
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, p = line[:i], p[i+1:]
		} else {
			p = p[len(p):]
		}
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, slashSlash) || !bytes.Contains(line, plusBuild) {
			continue
		}
		text := string(line)
		if !constraint.IsPlusBuild(text) {
			continue
		}
		// A legacy line that does not parse is ignored, not an error. The go
		// command does the same, because such lines predate the parser.
		if x, err := constraint.Parse(text); err == nil {
			if !c.eval(x) {
				shouldBuild = false
			}
		}
	}
	return shouldBuild, nil
}

func (c *Context) eval(x constraint.Expr) bool {
	return x.Eval(c.MatchTag)
}

// parseFileHeader returns the leading run of comments and blank lines that can
// hold constraints, and the //go:build line if there is one.
//
// The header ends at the last blank line before the first line that is neither
// blank nor a comment. A //go:build line after that point is not a constraint,
// which is the rule that stops a package doc comment from being read as one.
func parseFileHeader(content []byte) (header, goBuild []byte, err error) {
	end := 0
	p := content
	ended := false       // saw a non-blank, non-// line
	inSlashStar := false // inside a /* */ comment

Lines:
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, p = line[:i], p[i+1:]
		} else {
			p = p[len(p):]
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			// Remember the latest blank line. A //go:build line must come
			// before a blank line that precedes the first non-comment line.
			end = len(content) - len(p)
			continue Lines
		}
		if !bytes.HasPrefix(line, slashSlash) {
			ended = true
		}

		if !inSlashStar && constraint.IsGoBuild(string(line)) {
			if goBuild != nil {
				return nil, nil, errMultipleGoBuild
			}
			goBuild = line
		}

	Comments:
		for len(line) > 0 {
			if inSlashStar {
				if i := bytes.Index(line, starSlash); i >= 0 {
					inSlashStar = false
					line = bytes.TrimSpace(line[i+len(starSlash):])
					continue Comments
				}
				continue Lines
			}
			if bytes.HasPrefix(line, slashSlash) {
				continue Lines
			}
			if bytes.HasPrefix(line, slashStar) {
				inSlashStar = true
				line = bytes.TrimSpace(line[len(slashStar):])
				continue Comments
			}
			break Lines
		}
	}

	return content[:end], goBuild, nil
}

// MatchFile reports whether the file named name in dir belongs to the package
// built in this context.
//
// It applies the name rules first and reads the file only when they pass. The
// order matches go/build.Context.MatchFile, so the two agree file by file.
//
// One difference is deliberate. go/build parses the file and reports a syntax
// error from MatchFile. nanogo only scans the header comments here, so a file
// that does not parse is still selected. Syntax errors belong to the parser,
// which reports them with a position.
func (c *Context) MatchFile(dir, name string) (bool, error) {
	if !c.MatchName(name) {
		return false, nil
	}
	if strings.HasSuffix(name, ".syso") {
		// A .syso file is a linker input with no text to read.
		return true, nil
	}
	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return false, err
	}
	ok, err := c.ShouldBuild(content)
	if err != nil {
		return false, fmt.Errorf("%s: %v", name, err)
	}
	return ok, nil
}
