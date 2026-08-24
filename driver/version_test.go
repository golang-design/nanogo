// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"runtime/debug"
	"strings"
	"testing"
)

// checkBuildIDLine applies the test cmd/go/internal/work/buildid.go applies to
// -V=full output. The condition below is the negation of the one in toolID,
// copied field for field, because malformed output is a fatal error in the go
// command with no fallback.
func checkBuildIDLine(t *testing.T, tool, out string) {
	t.Helper()
	f := strings.Fields(out)
	if len(f) < 3 || f[0] != tool || f[1] != "version" ||
		strings.Contains(f[2], "devel") && !strings.HasPrefix(f[len(f)-1], "buildID=") {
		t.Fatalf("the go command rejects the -V=full output of %s:\n\t%s", tool, out)
	}
}

func TestVersionLine(t *testing.T) {
	for _, tool := range []string{"compile", "asm", "link", "cgo", "pack"} {
		line := VersionLine(tool)
		checkBuildIDLine(t, tool, line)

		// The go command uses the whole line as the build ID for a release
		// version, so nanogo's identity must be in it. Without the identity a
		// nanogo change would reuse objects built by the previous nanogo.
		// See specs/051-build-integration.md, section Caching.
		if !strings.Contains(line, "X:nanogo-") {
			t.Errorf("%q carries no nanogo identity", line)
		}
		if !strings.Contains(line, PinnedGoVersion) {
			t.Errorf("%q does not name the pinned Go release", line)
		}
		if strings.Contains(line, "\n") {
			t.Errorf("%q is more than one line", line)
		}
	}
}

// TestVersionLineFieldZero states the trap on its own: field 0 is the tool's
// name and never nanogo's.
func TestVersionLineFieldZero(t *testing.T) {
	f := strings.Fields(VersionLine("compile"))
	if f[0] != "compile" {
		t.Fatalf("field 0 = %q, want %q", f[0], "compile")
	}
	if f[0] == "nanogo" {
		t.Fatal("field 0 is the driver's name, so the go command rejects it")
	}
}

func TestVersionLineOddToolName(t *testing.T) {
	// A name with spaces would shift every field the go command reads.
	if got := VersionLine(""); strings.Fields(got)[0] != "compile" {
		t.Errorf("VersionLine(%q) = %q, want a usable name", "", got)
	}
	line := VersionLine("two words")
	if n := len(strings.Fields(line)); n != 4 {
		t.Errorf("VersionLine(%q) has %d fields: %q", "two words", n, line)
	}
}

func TestBuildIdentity(t *testing.T) {
	id := BuildIdentity()
	if id == "" {
		t.Fatal("BuildIdentity is empty")
	}
	if strings.ContainsAny(id, " \t\n") {
		t.Fatalf("BuildIdentity %q has whitespace, so it adds fields to the -V=full line", id)
	}
}

func TestBuildIdentityFromInfo(t *testing.T) {
	info := func(settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Settings: settings}
	}
	tests := []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"no build info", nil, false, unknownIdentity},
		{"nil info", nil, true, unknownIdentity},
		{"no vcs", info(), true, unknownIdentity},
		{
			"clean tree", info(
				debug.BuildSetting{Key: "vcs.revision", Value: "abc123"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			), true, "abc123",
		},
		{
			// A dirty tree is not the revision it claims to be, and the go
			// command's cache must see the difference.
			"dirty tree", info(
				debug.BuildSetting{Key: "vcs.revision", Value: "abc123"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			), true, "abc123+dirty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildIdentity(tt.info, tt.ok); got != tt.want {
				t.Errorf("buildIdentity = %q, want %q", got, tt.want)
			}
		})
	}
}
