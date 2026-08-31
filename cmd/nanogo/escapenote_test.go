// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.design/x/nanogo/driver"
)

// escapeNoteModule is the smallest program that shows what an escape analysis
// note decides.
//
// lib.Load reads b and keeps nothing. gc proves that and writes a note saying
// so. nanogo runs no analysis (specs/023-escape-analysis.md), so it writes the
// note that assumes the worst.
//
// //go:noinline is what makes the note the only evidence gc has. Without it gc
// inlines the call, re-runs its own analysis over the body it inlined, and
// never reads the note at all. A test without the directive passes whatever
// the note says and proves nothing.
//
// user.F holds the array in its frame and hands out a slice of it. Whether
// that array can stay in the frame is decided by lib.Load's note and by
// nothing else.
var escapeNoteModule = map[string]string{
	"go.mod": "module nanogo.example/escapenote\n\ngo 1.21\n",
	"lib/lib.go": `package lib

//go:noinline
func Load(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8
}
`,
	"user/user.go": `package user

import "nanogo.example/escapenote/lib"

func F() uint64 {
	var a [32]byte
	return lib.Load(a[:])
}
`,
}

// TestEscapeNoteIsTheConservativeOne records why gc cannot compile a runtime
// package against nanogo's archives.
//
// gc encodes an escape analysis note per receiver and parameter. The encoding
// is cmd/compile/internal/escape.leaks, a per-destination minimum dereference
// count, and cmd/compile/internal/escape.leaks.Encode returns the empty string
// exactly when the parameter leaks to the heap at zero dereferences:
//
//	if l.Heap() == 0 {
//		// Space optimization: empty string encodes more
//		// efficiently in export data.
//		return ""
//	}
//
// cmd/compile/internal/escape.parseLeaks is the other half and agrees:
//
//	if !strings.HasPrefix(s, "esc:") {
//		l.AddHeap(0)
//		return l
//	}
//
// So the empty note is not a missing note and gc does not reject it. It is the
// short form of the most pessimistic answer in the lattice, and it is the only
// answer nanogo can give without the analysis. There is no encoding that means
// "unknown", because gc does not need one: unknown and leaks-to-heap are the
// same claim, so the common one got the cheapest encoding.
//
// The consequence is not a slower program. gc compiles the twenty-three
// packages of cmd/internal/objabi.runtimePkgs under a rule that forbids a heap
// escape outright, and cmd/compile/internal/escape.(*batch).finish is where it
// fires:
//
//	if base.Flag.CompilingRuntime {
//		base.ErrorfAt(n.Pos(), 0, "%v escapes to heap, not allowed in runtime", n)
//	}
//
// Twenty of the twenty-four packages nanogo compiles in the bootstrap closure
// of specs/060-selfhost.md are on that list, so a build in which nanogo owns
// the closure and gc owns runtime stops here.
//
// This test will fail when the analysis of specs/023-escape-analysis.md lands
// and nanogo begins to write a proved note. That is the point: it is the mark
// on the blocker, and closing the blocker is what moves it.
func TestEscapeNoteIsTheConservativeOne(t *testing.T) {
	goBin := needGo(t)
	if runtime.GOARCH != driver.TargetArch {
		t.Skipf("nanogo emits %s machine code and GOARCH is %s, so it refuses every package here",
			driver.TargetArch, runtime.GOARCH)
	}
	dir := t.TempDir()
	bin := buildNanogo(t, goBin, dir)

	mod := filepath.Join(dir, "mod")
	for name, data := range escapeNoteModule {
		full := filepath.Join(mod, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(list, []byte("nanogo.example/escapenote/lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("gc writes a note that names the destinations", func(t *testing.T) {
		archive := libArchive(t, goBin, dir, mod, "archive-gc", nil)
		if !bytes.Contains(archive, []byte("esc:")) {
			t.Error("gc's archive for lib carries no esc: tag, so this test is not reading the notes")
		}
	})

	t.Run("nanogo writes the empty note", func(t *testing.T) {
		archive := libArchive(t, goBin, dir, mod, "archive-nanogo", []string{"-toolexec=" + bin})
		if bytes.Contains(archive, []byte("esc:")) {
			t.Error("nanogo's archive for lib carries an esc: tag; " +
				"either the analysis of specs/023-escape-analysis.md landed, " +
				"in which case this test records a blocker that is gone, " +
				"or a note is being written that nothing proved")
		}
	})

	// The two consumer builds. Each compiles user with -+, which is what
	// cmd/go's -std plus cmd/internal/objabi.LookupPkgSpecial gives every
	// package on the runtime list.
	t.Run("gc's note lets the array stay in the frame", func(t *testing.T) {
		out, err := buildUser(t, goBin, dir, mod, "consume-gc", nil, "")
		if err != nil {
			t.Fatalf("gc cannot compile user against gc's own archive: %v\n%s", err, out)
		}
	})

	t.Run("nanogo's note forces the array to the heap", func(t *testing.T) {
		logFile := filepath.Join(dir, "consume-nanogo.log")
		out, err := buildUser(t, goBin, dir, mod, "consume-nanogo",
			[]string{"-toolexec=" + bin}, logFile)
		if err == nil {
			t.Fatalf("gc compiled user against nanogo's archive under the runtime rule:\n%s", out)
		}
		if want := "escapes to heap, not allowed in runtime"; !strings.Contains(string(out), want) {
			t.Errorf("the failure does not name the runtime rule, so it is a different blocker:\n%s", out)
		}

		// A run in which nanogo never compiled lib proves nothing: the build
		// would then have failed for some other reason, or not at all. This
		// is the trap internal/selfhost/measure.go documents, and the fresh
		// GOCACHE below is what keeps the compile action from being reused.
		logged, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("reading the nanogo log: %v", err)
		}
		if !strings.Contains(string(logged), "compiled nanogo.example/escapenote/lib") {
			t.Fatalf("nanogo did not compile lib, so this run proved nothing:\n%s", logged)
		}
	})
}

// libArchive builds lib and returns the archive bytes.
//
// -work keeps the build directory, which is where the archive is. It is under
// the test's own TMPDIR, so it goes away with the test.
func libArchive(t *testing.T, goBin, dir, mod, tag string, extra []string) []byte {
	t.Helper()
	args := append([]string{"build", "-work"}, extra...)
	args = append(args, "-o", os.DevNull, "./lib")
	cmd := exec.Command(goBin, args...)
	cmd.Dir = mod
	cmd.Env = escapeNoteEnv(t, dir, tag, "")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building lib (%s): %v\n%s", tag, err, out)
	}
	var work string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "WORK="); ok {
			work = rest
		}
	}
	if work == "" {
		t.Fatalf("go build -work printed no WORK line:\n%s", out)
	}
	var archive []byte
	err = filepath.WalkDir(work, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "_pkg_.a" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		archive = b
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", work, err)
	}
	if archive == nil {
		t.Fatalf("no _pkg_.a under %s", work)
	}
	return archive
}

// buildUser compiles user with the runtime rule on and returns what the go
// command said. -+ is the compiler flag that sets CompilingRuntime, which
// cmd/go turns on for every package of cmd/internal/objabi.runtimePkgs.
func buildUser(t *testing.T, goBin, dir, mod, tag string, extra []string, logFile string) ([]byte, error) {
	t.Helper()
	args := append([]string{"build"}, extra...)
	args = append(args, "-gcflags=nanogo.example/escapenote/user=-+", "-o", os.DevNull, "./user")
	cmd := exec.Command(goBin, args...)
	cmd.Dir = mod
	cmd.Env = escapeNoteEnv(t, dir, tag, logFile)
	out, err := cmd.CombinedOutput()
	return out, err
}

// escapeNoteEnv gives one build its own cache and its own temporary
// directory, both under the test's directory.
//
// The cache is per build and not shared. A package gc built earlier has the
// same action ID when nanogo is asked for it later, so a shared cache serves
// the earlier archive and -toolexec never runs. The measurement would then
// read gc's notes and call them nanogo's.
func escapeNoteEnv(t *testing.T, dir, tag, logFile string) []string {
	t.Helper()
	cache := filepath.Join(dir, tag, "cache")
	tmp := filepath.Join(dir, tag, "tmp")
	for _, d := range []string{cache, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := append(buildEnv(),
		"GOCACHE="+cache,
		"TMPDIR="+tmp,
		"CGO_ENABLED=0",
		"NANOGO_ALLOWLIST="+filepath.Join(dir, "allowlist"),
	)
	if logFile != "" {
		env = append(env, "NANOGO_LOG="+logFile)
	}
	return env
}
