// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExec records the passthrough instead of running it.
type fakeExec struct {
	called bool
	path   string
	args   []string
	code   int
	err    error
}

func (f *fakeExec) run(path string, args []string) (int, error) {
	f.called = true
	f.path = path
	f.args = args
	return f.code, f.err
}

// runner drives Run in process.
type runner struct {
	t      *testing.T
	env    map[string]string
	exec   fakeExec
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newRunner(t *testing.T) *runner {
	return &runner{t: t, env: map[string]string{}}
}

func (r *runner) run(args ...string) int {
	r.stdout.Reset()
	r.stderr.Reset()
	return Run(Env{
		Args:   args,
		Stdout: &r.stdout,
		Stderr: &r.stderr,
		Getenv: func(k string) string { return r.env[k] },
		Exec:   r.exec.run,
	})
}

// allowlist writes an allowlist and points the environment at it.
func (r *runner) allowlist(pkgs ...string) {
	name := filepath.Join(r.t.TempDir(), "allowlist")
	if err := os.WriteFile(name, []byte(strings.Join(pkgs, "\n")+"\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.env[AllowlistEnv] = name
}

func TestRunVersionFull(t *testing.T) {
	r := newRunner(t)
	if code := r.run("/goroot/pkg/tool/darwin_arm64/compile", "-V=full"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
	}
	if r.exec.called {
		// Answering from the real compiler would hide nanogo's identity from
		// the go command's cache, and every nanogo change would then reuse the
		// objects of the nanogo before it.
		t.Fatal("the real compiler ran")
	}
	checkBuildIDLine(t, "compile", r.stdout.String())

	// The go command runs strings.Fields over the whole of standard output, so
	// a second line shifts the fields it reads.
	if n := strings.Count(strings.TrimRight(r.stdout.String(), "\n"), "\n"); n != 0 {
		t.Errorf("standard output has %d extra lines: %q", n, r.stdout.String())
	}
	if r.stderr.Len() != 0 {
		t.Errorf("standard error is not empty: %q", r.stderr.String())
	}
}

// TestRunVersionFullOtherTools states the other half of the rule. nanogo does
// not change what the assembler and the linker produce, so it must not answer
// for them: their build ID has to move when the real tool moves.
func TestRunVersionFullOtherTools(t *testing.T) {
	for _, tool := range []string{"asm", "link", "cgo"} {
		t.Run(tool, func(t *testing.T) {
			r := newRunner(t)
			path := "/goroot/pkg/tool/darwin_arm64/" + tool
			if code := r.run(path, "-V=full"); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
			}
			if !r.exec.called {
				t.Fatal("nanogo answered for a tool it does not implement")
			}
			if r.exec.path != path || !equal(r.exec.args, []string{"-V=full"}) {
				t.Errorf("exec %q %q, want %q -V=full", r.exec.path, r.exec.args, path)
			}
			if r.stdout.Len() != 0 {
				t.Errorf("nanogo wrote to standard output: %q", r.stdout.String())
			}
		})
	}
}

func TestRunVersionShort(t *testing.T) {
	r := newRunner(t)
	if code := r.run("compile", "-V"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
	}
	checkBuildIDLine(t, "compile", r.stdout.String())
}

// TestRunToolNameFromPath checks that the first field follows the tool path
// the go command passes, and not nanogo's own name.
func TestRunToolNameFromPath(t *testing.T) {
	for _, path := range []string{"compile", "/a/b/compile", "/a/b/compile.exe"} {
		r := newRunner(t)
		if code := r.run(path, "-V=full"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if got := strings.Fields(r.stdout.String())[0]; got != "compile" {
			t.Errorf("ToolName(%q) produced field 0 %q, want %q", path, got, "compile")
		}
	}
}

func TestRunPassesThroughOtherTools(t *testing.T) {
	for _, tool := range []string{"asm", "link", "cgo", "pack", "buildid"} {
		r := newRunner(t)
		r.allowlist("strconv")
		args := []string{"/goroot/pkg/tool/" + tool, "-o", "out.o", "in.s"}
		if code := r.run(args...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
		}
		if !r.exec.called {
			t.Fatalf("%s did not reach the real tool", tool)
		}
		if r.exec.path != args[0] {
			t.Errorf("path = %q, want %q", r.exec.path, args[0])
		}
		if !equal(r.exec.args, args[1:]) {
			t.Errorf("args = %q, want %q", r.exec.args, args[1:])
		}
	}
}

func TestRunPassthroughExitStatus(t *testing.T) {
	r := newRunner(t)
	r.exec.code = 2
	if code := r.run("/goroot/pkg/tool/asm", "x.s"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}

	r = newRunner(t)
	r.exec.err = errors.New("no such tool")
	if code := r.run("/goroot/pkg/tool/asm", "x.s"); code == 0 {
		t.Fatal("exit 0 although the tool did not start")
	}
	if !strings.Contains(r.stderr.String(), "no such tool") {
		t.Errorf("stderr %q does not carry the reason", r.stderr.String())
	}
}

func TestRunSelection(t *testing.T) {
	compileArgs := func(pkg string, extra ...string) []string {
		args := []string{"/goroot/pkg/tool/compile", "-p", pkg, "-o", "_pkg_.a",
			"-goversion", PinnedGoVersion, "-pack", "-shared", "-nolocalimports"}
		return append(append(args, extra...), "a.go")
	}

	t.Run("no allowlist falls through", func(t *testing.T) {
		r := newRunner(t)
		if code := r.run(compileArgs("strconv")...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
		}
		if !r.exec.called {
			t.Fatal("the package did not reach gc")
		}
	})

	t.Run("not on the allowlist falls through", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("errors")
		if code := r.run(compileArgs("strconv")...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
		}
		if !r.exec.called {
			t.Fatal("the package did not reach gc")
		}
	})

	t.Run("on the allowlist is nanogo's", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("errors", "strconv")
		code := r.run(compileArgs("strconv")...)
		if code == 0 {
			t.Fatal("exit 0 although nanogo has no compiler")
		}
		if r.exec.called {
			t.Fatal("the package reached gc although it is nanogo's")
		}
		if !strings.Contains(r.stderr.String(), "strconv") {
			t.Errorf("stderr %q does not name the package", r.stderr.String())
		}
		if !strings.Contains(r.stderr.String(), "not implemented") {
			t.Errorf("stderr %q does not say what is missing", r.stderr.String())
		}
	})

	t.Run("p with an equals sign", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("strconv")
		if code := r.run("/goroot/pkg/tool/compile", "-p=strconv", "a.go"); code == 0 {
			t.Fatal("exit 0 although the package is nanogo's")
		}
		if r.exec.called {
			t.Fatal("the package reached gc although it is nanogo's")
		}
	})

	// -pack starts with -p and must not be read as it.
	t.Run("pack is not p", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("ack")
		if code := r.run("/goroot/pkg/tool/compile", "-pack", "-p", "strconv", "a.go"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
		}
		if !r.exec.called {
			t.Fatal("the package did not reach gc")
		}
	})

	t.Run("driver fallback flag", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("strconv")
		args := append([]string{"-fallback"}, compileArgs("strconv")...)
		if code := r.run(args...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
		}
		if !r.exec.called {
			t.Fatal("-fallback did not reach gc")
		}
	})

	t.Run("compile fallback flag", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("strconv")
		if code := r.run(compileArgs("strconv", "-fallback")...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
		}
		if !r.exec.called {
			t.Fatal("-fallback did not reach gc")
		}
	})

	// specs/050-driver.md: a flag nanogo does not implement sends the package
	// down the fallback path, where gc handles it correctly.
	t.Run("unsupported flag falls through", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("strconv")
		if code := r.run(compileArgs("strconv", "-race")...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
		}
		if !r.exec.called {
			t.Fatal("a rejected flag did not send the package to gc")
		}
	})

	t.Run("unknown flag falls through", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("strconv")
		if code := r.run(compileArgs("strconv", "-nosuchflag")...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, r.stderr.String())
		}
		if !r.exec.called {
			t.Fatal("an unknown flag did not send the package to gc")
		}
	})

	t.Run("goversion mismatch", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("strconv")
		args := []string{"/goroot/pkg/tool/compile", "-p", "strconv",
			"-goversion", "go1.0.0", "a.go"}
		if code := r.run(args...); code == 0 {
			t.Fatal("exit 0 although the pinned release does not match")
		}
		if !strings.Contains(r.stderr.String(), "strconv") {
			t.Errorf("stderr %q does not name the package", r.stderr.String())
		}
		if !strings.Contains(r.stderr.String(), PinnedGoVersion) {
			t.Errorf("stderr %q does not name the pinned release", r.stderr.String())
		}
	})

	t.Run("unreadable allowlist", func(t *testing.T) {
		r := newRunner(t)
		r.env[AllowlistEnv] = filepath.Join(t.TempDir(), "missing")
		if code := r.run(compileArgs("strconv")...); code == 0 {
			t.Fatal("exit 0 although the allowlist could not be read")
		}
		if r.exec.called {
			t.Fatal("a broken allowlist silently turned nanogo off")
		}
	})
}

func TestRunReadsImportCfg(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "importcfg")
	if err := os.WriteFile(good, []byte("packagefile errors=/pkg/errors.a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "importcfg.bad")
	if err := os.WriteFile(bad, []byte("nonsense fmt=/pkg/fmt.a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("valid", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("strconv")
		code := r.run("/goroot/pkg/tool/compile", "-p", "strconv", "-importcfg", good, "a.go")
		if code == 0 {
			t.Fatal("exit 0 although nanogo has no compiler")
		}
		if !strings.Contains(r.stderr.String(), "not implemented") {
			t.Errorf("stderr %q, want the not implemented report", r.stderr.String())
		}
	})

	t.Run("malformed", func(t *testing.T) {
		r := newRunner(t)
		r.allowlist("strconv")
		code := r.run("/goroot/pkg/tool/compile", "-p", "strconv", "-importcfg", bad, "a.go")
		if code == 0 {
			t.Fatal("exit 0 although the import configuration is malformed")
		}
		if !strings.Contains(r.stderr.String(), "unknown directive") {
			t.Errorf("stderr %q does not report the bad directive", r.stderr.String())
		}
	})
}

func TestRunUsage(t *testing.T) {
	t.Run("no arguments", func(t *testing.T) {
		r := newRunner(t)
		if code := r.run(); code == 0 {
			t.Fatal("exit 0 with no arguments")
		}
		if !strings.Contains(r.stderr.String(), "usage:") {
			t.Errorf("stderr %q carries no usage", r.stderr.String())
		}
	})

	t.Run("only a driver flag", func(t *testing.T) {
		r := newRunner(t)
		if code := r.run("-fallback"); code == 0 {
			t.Fatal("exit 0 with no tool")
		}
	})

	t.Run("unknown driver flag", func(t *testing.T) {
		r := newRunner(t)
		if code := r.run("-nosuchflag", "compile"); code == 0 {
			t.Fatal("exit 0 with an unknown driver flag")
		}
		if !strings.Contains(r.stderr.String(), "-nosuchflag") {
			t.Errorf("stderr %q does not name the flag", r.stderr.String())
		}
	})
}

// TestRunDefaults checks that Run fills in the fields a plain caller leaves
// empty, which is what cmd/nanogo relies on.
func TestRunDefaults(t *testing.T) {
	var out bytes.Buffer
	code := Run(Env{Args: []string{"compile", "-V=full"}, Stdout: &out})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	checkBuildIDLine(t, "compile", out.String())
}

func TestToolName(t *testing.T) {
	tests := []struct{ path, want string }{
		{"compile", "compile"},
		{"/goroot/pkg/tool/darwin_arm64/compile", "compile"},
		{"/goroot/pkg/tool/windows_amd64/compile.exe", "compile"},
		{"/goroot/pkg/tool/darwin_arm64/asm", "asm"},
	}
	for _, tt := range tests {
		if got := ToolName(tt.path); got != tt.want {
			t.Errorf("ToolName(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestScanCompile(t *testing.T) {
	tests := []struct {
		args     []string
		pkg      string
		fallback bool
	}{
		{[]string{"-p", "strconv"}, "strconv", false},
		{[]string{"-p=strconv"}, "strconv", false},
		{[]string{"--p", "strconv"}, "strconv", false},
		{[]string{"--p=strconv"}, "strconv", false},
		{[]string{"-pack", "-p", "strconv"}, "strconv", false},
		{[]string{"-p"}, "", false},
		{[]string{"a.go"}, "", false},
		{[]string{"-fallback", "-p", "x"}, "x", true},
		{[]string{"-fallback=true"}, "", true},
		{[]string{"--fallback"}, "", true},
		// A flag nanogo does not know must not stop selection, because
		// selection decides whether nanogo is entitled to an opinion at all.
		{[]string{"-nosuchflag", "-p", "strconv"}, "strconv", false},
	}
	for _, tt := range tests {
		pkg, fallback := scanCompile(tt.args)
		if pkg != tt.pkg || fallback != tt.fallback {
			t.Errorf("scanCompile(%q) = %q, %v; want %q, %v",
				tt.args, pkg, fallback, tt.pkg, tt.fallback)
		}
	}
}

func TestCompileNotImplemented(t *testing.T) {
	err := Compile(&Config{Package: "strconv", GoVersion: PinnedGoVersion})
	var ni *NotImplementedError
	if !errors.As(err, &ni) {
		t.Fatalf("Compile = %v (%T), want *NotImplementedError", err, err)
	}
	if ni.Package != "strconv" {
		t.Errorf("Package = %q, want %q", ni.Package, "strconv")
	}
	if !strings.Contains(err.Error(), "strconv") {
		t.Errorf("error %q does not name the package", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestExecToolMissing covers the only branch of the passthrough that returns.
// On unix the success branch replaces the process image, so it is checked from
// cmd/nanogo, where a separate process observes the result.
func TestExecToolMissing(t *testing.T) {
	code, err := execTool(filepath.Join(t.TempDir(), "nosuchtool"), []string{"-V=full"})
	if err == nil {
		t.Fatal("execTool on a missing tool = no error, want one")
	}
	if code == 0 {
		t.Error("execTool on a missing tool reported success")
	}
}
