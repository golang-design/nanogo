// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogRecordsOneLinePerDecision checks the record a build reports itself
// through. A build looks the same whether nanogo compiled a package or handed
// it to gc, so this file is the only evidence either way.
func TestLogRecordsOneLinePerDecision(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "nanogo.log")
	getenv := func(k string) string {
		if k == LogEnv {
			return name
		}
		return ""
	}
	logDecision(getenv, DecisionCompiled, "strconv", "/work/b001/_pkg_.a")
	logDecision(getenv, DecisionDelegated, "fmt", "not on the allowlist")
	logDecision(getenv, DecisionFailed, "errors", "")
	// A -V=full invocation has no package, and a line with an empty field
	// would shift every reader's columns.
	logDecision(getenv, DecisionDelegated, "", "")

	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	want := "compiled strconv /work/b001/_pkg_.a\n" +
		"delegated fmt not on the allowlist\n" +
		"failed errors\n" +
		"delegated ?\n"
	if string(b) != want {
		t.Errorf("the log reads\n%s\nwant\n%s", b, want)
	}
}

func TestLogIsOffWithoutTheVariable(t *testing.T) {
	dir := t.TempDir()
	logDecision(func(string) string { return "" }, DecisionCompiled, "strconv", "")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("an unset %s wrote %d files", LogEnv, len(entries))
	}
}

// TestLogFailureDoesNotStopTheBuild checks the rule that the log is a report
// about the build and never a part of it.
func TestLogFailureDoesNotStopTheBuild(t *testing.T) {
	unwritable := filepath.Join(t.TempDir(), "nosuchdir", "log")
	logDecision(func(string) string { return unwritable }, DecisionCompiled, "strconv", "")
}

// TestRunWritesTheLog checks the wiring, which is what a test of logDecision
// alone would not.
func TestRunWritesTheLog(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "log")
	list := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(list, []byte("strconv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := &Env{
		Stdout: new(strings.Builder),
		Stderr: new(strings.Builder),
		Getenv: func(k string) string {
			switch k {
			case LogEnv:
				return name
			case AllowlistEnv:
				return list
			}
			return ""
		},
		Exec: func(string, []string) (int, error) { return 0, nil },
	}
	// One package gc takes, one nanogo claims and cannot compile, and one
	// invocation nanogo was told to hand over.
	env.Args = []string{"/goroot/pkg/tool/compile", "-p", "fmt", "-o", "o.a", "x.go"}
	Run(*env)
	env.Args = []string{"/goroot/pkg/tool/compile", "-p", "strconv", "-o", "o.a", "x.go"}
	Run(*env)
	env.Args = []string{"-fallback", "/goroot/pkg/tool/compile", "-p", "strconv", "-o", "o.a", "x.go"}
	Run(*env)

	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"delegated fmt not on the allowlist\n",
		"failed strconv ",
		"delegated strconv -fallback\n",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the log does not contain %q:\n%s", want, b)
		}
	}
}

// TestRunLogsAFlagFallback covers the branch where a flag nanogo does not know
// sends a package it owns to gc.
func TestRunLogsAFlagFallback(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "log")
	list := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(list, []byte("strconv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var called bool
	code := Run(Env{
		Args:   []string{"/goroot/pkg/tool/compile", "-p", "strconv", "-nosuchflag", "x.go"},
		Stdout: new(strings.Builder),
		Stderr: new(strings.Builder),
		Getenv: func(k string) string {
			switch k {
			case LogEnv:
				return name
			case AllowlistEnv:
				return list
			}
			return ""
		},
		Exec: func(string, []string) (int, error) { called = true; return 0, nil },
	})
	if code != 0 || !called {
		t.Fatalf("exit %d, gc called %v", code, called)
	}
	b, _ := os.ReadFile(name)
	if !strings.Contains(string(b), "delegated strconv") {
		t.Errorf("the log does not record the flag fallback:\n%s", b)
	}
}

// TestRunCompilesAndLogsIt is the whole driver in one call: selection, flag
// parsing, the pipeline, the object, and the record that says it happened.
func TestRunCompilesAndLogsIt(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package p\n\nfunc f(a int) int { return a + 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(list, []byte("nanogo.example/p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "log")
	out := filepath.Join(dir, "_pkg_.a")

	var stderr strings.Builder
	code := Run(Env{
		Args: []string{"/goroot/pkg/tool/compile",
			"-p", "nanogo.example/p", "-o", out, "-lang=go1.27",
			"-goversion", PinnedGoVersion, "-trimpath", dir + "=>", "-pack",
			"-shared", "-nolocalimports", "-complete", "-c=4", "-buildid", "abc/def",
			src},
		Stdout: new(strings.Builder),
		Stderr: &stderr,
		Getenv: func(k string) string {
			switch k {
			case LogEnv:
				return name
			case AllowlistEnv:
				return list
			}
			return ""
		},
		Exec: func(string, []string) (int, error) {
			t.Error("the package reached gc although nanogo owns it")
			return 0, nil
		},
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("nanogo exited zero and wrote no object: %v", err)
	}
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if want := "compiled nanogo.example/p " + out + "\n"; string(b) != want {
		t.Errorf("the log reads %q, want %q", b, want)
	}
}
