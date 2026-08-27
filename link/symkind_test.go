// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestKindsMatchTheToolchain compares the linker's symbol kinds with the
// installed toolchain's.
//
// The numeric order decides layout twice over, so a kind missing from the
// middle of the list moves every kind after it into another section.
func TestKindsMatchTheToolchain(t *testing.T) {
	goCmd := goTool(t)
	root := strings.TrimSpace(string(out(t, goCmd, "env", "GOROOT")))
	path := filepath.Join(root, "src", "cmd", "link", "internal", "sym", "symkind.go")
	src, err := os.ReadFile(path)
	if err != nil {
		if requireCorpus() {
			t.Fatalf("NANOGO_REQUIRE_CORPUS is set and the toolchain's kind list is not readable: %v", err)
		}
		t.Skipf("no toolchain source to compare against: %v", err)
	}
	block := regexp.MustCompile(`(?s)\n\tSxxx SymKind = iota\n(.*?)\n\)\n`).FindSubmatch(src)
	if block == nil {
		t.Fatalf("%s holds no kind list this test recognises", path)
	}
	names := []string{"xxx"}
	for _, line := range strings.Split(string(block[1]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if i := strings.Index(line, " "); i >= 0 {
			line = line[:i]
		}
		if !strings.HasPrefix(line, "S") {
			continue
		}
		names = append(names, strings.TrimPrefix(line, "S"))
	}
	if len(names) != NumKind() {
		t.Fatalf("the toolchain declares %d symbol kinds and this package pins %d\ntoolchain: %v",
			len(names), NumKind(), names)
	}
	for i, name := range names {
		if got := KindName(i); got != name {
			t.Fatalf("kind %d is S%s in the toolchain and S%s here", i, name, got)
		}
	}
	t.Logf("%d symbol kinds agree with the installed toolchain", NumKind())
}
