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
//
// The list pins phrases, and a phrase pinned as a refusal becomes a lie when
// the refusal is lifted. "package-level variables" and "init function" were
// pinned here as things nanogo could not compile until August 2026, when both
// started working and this test went on requiring the sentence that said they
// did not. So a phrase belongs here only while a probe in
// internal/audit/testdata/probes shows the behaviour it describes.
func TestHelpStatesTheLimits(t *testing.T) {
	for _, want := range []string{
		"arm64",             // the one architecture
		"darwin/arm64",      // the target the tests run on
		"allowlist",         // how a package is selected
		"gc",                // what happens to everything else
		"export data",       // the archive carries it, so it can be imported
		"assembly",          // the ABI wrapper that is not generated
		"a type assertion",  // probes/type-assert
		"append",            // probes/append-int
		"map operation",     // probes/map-make-assign
		"channel operation", // probes/chan-buffered
		"A method value",    // probes/defer-method-value
		"generic function",
		`imports "C"`,
		"-race",
		AllowlistEnv,
		LogEnv,
		"-fallback",
		"specs/050-driver.md",
		// The failures with no diagnostic. The probe corpus reaches none
		// of them now, so what the help has to name is the two the corpus
		// cannot sample for: a local that outlives its frame and a pointer
		// store with no barrier. Both are silent and neither is a refusal.
		"What nanogo does not announce",
		"go:embed",
		"escape",
		"write barrier",
		// The two result shapes the convention refuses. These replaced the
		// crash that used to be pinned here as "wider than four machine
		// registers", which stopped being true when the ABI pass learned to
		// split a call's results.
		"sixteen result registers",
		// This pin used to hold a limitation and now holds a
		// capability. probes/buildinfo-named was recorded wrong until
		// nanogo build started writing the modinfo line, and the pin
		// is what failed then and made the sentence above it be
		// rewritten in the same change. It stays for the same reason
		// in the other direction: the help now says which build
		// settings the line carries and which it leaves out, and a
		// link that stopped writing it would leave that paragraph
		// claiming something nanogo no longer does.
		"modinfo",
		// nanogo build, and the three limits a user must not discover by
		// hitting them: one architecture, whose standard library it is,
		// and who writes the executable.
		"nanogo build",
		"the installed Go toolchain",
		"go tool link",
		"specs/045-linker.md",
		RootEnv,
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
