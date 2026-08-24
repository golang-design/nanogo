// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"runtime/debug"
	"strings"
)

// PinnedGoVersion is the Go release nanogo is object-compatible with, per
// specs/000-decisions.md decision 11. The object format, the export data
// format and the runtime's internal structures all change between releases,
// so nanogo tracks one release at a time.
const PinnedGoVersion = "go1.27.0"

// unknownIdentity is the build identity nanogo reports when the binary carries
// no VCS stamp. A binary built outside a repository, or with -buildvcs=false,
// reaches this.
const unknownIdentity = "unknown"

// BuildIdentity is nanogo's own identity, as it appears in the -V=full line.
//
// This value is load-bearing. The go command turns the whole -V=full line into
// the compiler's build ID and mixes it into every cache key, so a nanogo change
// invalidates the packages nanogo compiled only if the line changes with it. A
// driver that echoed gc's version string would build once and then reuse stale
// objects for ever. See specs/051-build-integration.md, section Caching.
func BuildIdentity() string {
	info, ok := debug.ReadBuildInfo()
	return buildIdentity(info, ok)
}

// buildIdentity is split out so that a test can supply build information the
// running binary does not carry.
func buildIdentity(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return unknownIdentity
	}
	// Settings is a slice, so this scan is deterministic.
	// specs/053-determinism.md forbids ranging over a map on a path that
	// produces output, and this path produces output.
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return unknownIdentity
	}
	if modified {
		// A dirty tree is not the revision it claims to be.
		return revision + "+dirty"
	}
	return revision
}

// VersionLine is the answer to <tool> -V=full.
//
// cmd/go/internal/work/buildid.go requires three or more whitespace separated
// fields, field 0 equal to the tool's own name and field 1 equal to "version".
// Malformed output is a fatal error in the go command with no fallback, which
// is why this function builds the line rather than formatting it at the call
// site. The trailing field is nanogo's identity; see [BuildIdentity].
func VersionLine(tool string) string {
	// A tool name with spaces would shift every field the go command reads.
	tool = strings.Join(strings.Fields(tool), "")
	if tool == "" {
		tool = "compile"
	}
	return tool + " version " + PinnedGoVersion + " X:nanogo-" + BuildIdentity()
}
