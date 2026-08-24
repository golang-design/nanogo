// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"runtime"
	"strings"
	"testing"
)

// TestHelpStatesTheLimits is a documentation test, and it is a real one.
//
// A user who finds out what nanogo cannot compile by hitting an error has
// already lost the afternoon, so every refusal driver/compile.go implements
// has to appear here. When a refusal goes away, this test is what says the
// help text still describes it.
func TestHelpStatesTheLimits(t *testing.T) {
	for _, want := range []string{
		"darwin/arm64",      // one target
		"allowlist",         // how a package is selected
		"gc",                // what happens to everything else
		"export data",       // why a package cannot import or be imported
		"composite literal", // the largest gap in SSA construction
		"assembly",          // the ABI wrapper that is not generated
		"package-level variables",
		"init function",
		"go:embed",
		"-race",
		AllowlistEnv,
		LogEnv,
		"-fallback",
		"specs/050-driver.md",
	} {
		if !strings.Contains(Help, want) {
			t.Errorf("the help does not mention %q", want)
		}
	}
	if !strings.HasSuffix(Help, "\n") {
		t.Error("the help does not end with a newline")
	}
	// A line the terminal wraps is a line a reader skips.
	for _, line := range strings.Split(Help, "\n") {
		if len(line) > 78 {
			t.Errorf("this line is %d columns wide:\n%s", len(line), line)
		}
	}
}

func TestHumanVersionNamesTheHost(t *testing.T) {
	got := HumanVersion()
	for _, want := range []string{"nanogo ", PinnedGoVersion, TargetArch, runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("nanogo version %q does not name %q", got, want)
		}
	}
}

// TestRunAnswersAPerson covers the two command lines the go command never
// sends, and checks that they reach standard output rather than standard
// error.
func TestRunAnswersAPerson(t *testing.T) {
	for _, args := range [][]string{
		{"help"}, {"-h"}, {"-help"}, {"--help"},
		{"version"}, {"-V"}, {"--version"},
	} {
		var out, errOut strings.Builder
		code := Run(Env{
			Args:   args,
			Stdout: &out,
			Stderr: &errOut,
			Getenv: func(string) string { return "" },
			Exec:   func(string, []string) (int, error) { return 0, nil },
		})
		if code != 0 {
			t.Errorf("nanogo %v exited %d: %s", args, code, errOut.String())
		}
		if out.Len() == 0 {
			t.Errorf("nanogo %v printed nothing to standard output", args)
		}
		if errOut.Len() != 0 {
			t.Errorf("nanogo %v printed to standard error: %q", args, errOut.String())
		}
	}
}

// TestRunDoesNotMistakeAToolForASubcommand checks that the words are read only
// when they stand alone. The go command appends an absolute tool path, so a
// real invocation never looks like one of them, and a tool called version must
// still be passed through.
func TestRunDoesNotMistakeAToolForASubcommand(t *testing.T) {
	var called bool
	var out strings.Builder
	code := Run(Env{
		Args:   []string{"version", "-V=full"},
		Stdout: &out,
		Stderr: new(strings.Builder),
		Getenv: func(string) string { return "" },
		Exec:   func(string, []string) (int, error) { called = true; return 0, nil },
	})
	if code != 0 || !called {
		t.Fatalf("exit %d, the tool ran %v", code, called)
	}
	if out.Len() != 0 {
		t.Errorf("nanogo answered for the tool: %q", out.String())
	}
}
