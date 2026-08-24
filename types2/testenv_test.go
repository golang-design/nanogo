// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package types2_test

import (
	"os"
	"os/exec"
	"testing"
)

// This file replaces the pieces of internal/testenv that the ported tests use.
// internal/testenv is not importable outside the Go repository.

// mustHaveGoBuild skips the test when there is no go command to build with.
//
// Upstream calls testenv.MustHaveGoBuild, which also knows about platforms
// that cannot exec. nanogo's tests run on developer machines and in CI, both
// of which can, so looking for the binary is enough.
func mustHaveGoBuild(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("skipping test: no go command: %v", err)
	}
}

// mustHaveCGO skips the test when cgo is not enabled.
//
// nanogo does not compile cgo input (specs/000 decision 8), so a test that
// needs it is skipped rather than run under a guess.
func mustHaveCGO(t *testing.T) {
	t.Helper()
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("skipping test: cgo is disabled")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("skipping test: no go command: %v", err)
	}
}
