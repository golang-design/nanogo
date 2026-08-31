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
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/driver"
	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/export/pkgbits"
)

// The escape analysis note, end to end, against the toolchain that reads it.
//
// gc encodes one note per receiver and parameter. The encoding is
// cmd/compile/internal/escape.leaks, a minimum dereference count per
// destination, and cmd/compile/internal/escape.leaks.Encode returns the empty
// string exactly when the parameter leaks to the heap at zero dereferences:
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
// So the empty note is not a missing note. It is the top of the lattice, and
// it is what nanogo wrote for every parameter until the analysis of
// specs/023-escape-analysis.md landed. gc compiles the packages of
// cmd/internal/objabi.runtimePkgs under a rule that forbids a heap escape
// outright, in cmd/compile/internal/escape.(*batch).finish:
//
//	if base.Flag.CompilingRuntime {
//		base.ErrorfAt(n.Pos(), 0, "%v escapes to heap, not allowed in runtime", n)
//	}
//
// so a build in which nanogo owns the closure and gc owns runtime used to stop
// the first time such a package handed the address of a local to a function
// nanogo compiled. The two tests below are the two halves of what changed: a
// note nanogo proved lets that build through, and a parameter nanogo did not
// prove still takes the note that stops it.

// escapeNoteCases is one package per case, each with one exported function and
// one pointer-bearing parameter.
//
// One parameter per package is what makes the archive a per-parameter oracle.
// The tags live in the export data's string table with no separator between
// them, so a package with two of them cannot be read back without decoding the
// stream; with one there is nothing to tell apart.
//
// gc is the column that makes this a soundness check rather than a record of
// what nanogo happens to do. nanogo's note must be gc's own note or the empty
// one, and nothing else, for every case here and every case added later.
var escapeNoteCases = []struct {
	name string

	// decl is the package body. It declares F and whatever F needs.
	decl string

	// gc is the note gc writes for F's parameter, and it is checked against
	// gc's archive rather than trusted.
	gc string

	// nanogo is the note nanogo writes. The empty string is the
	// conservative answer and is always allowed.
	nanogo string
}{{
	name: "a slice that is only read through",
	decl: "func F(b []byte) uint64 { return uint64(b[0]) | uint64(b[1])<<8 }",
	gc:   "esc:",
	// Proved: nothing derived from b reaches a result, a global, a store
	// through a pointer, or a call.
	nanogo: "esc:",
}, {
	name: "a pointer that is only read through",
	decl: "type T struct{ A int }\n\nfunc F(t *T) int { return t.A }",
	gc:   "esc:",
	// Proved: reading through a pointer neither retains it nor writes
	// through it.
	nanogo: "esc:",
}, {
	name: "a pointer stored in a global",
	decl: "var g *int\n\nfunc F(p *int) { g = p }",
	// gc writes the empty note too: the parameter leaks to the heap.
	gc:     "",
	nanogo: "",
}, {
	name: "a pointer that is returned",
	decl: "func F(p *int) *int { return p }",
	// A flow to result 0 at zero dereferences. nanogo has no encoding for
	// it: stage 1 says "flows nowhere" or nothing at all
	// (specs/023-escape-analysis.md), so it takes the conservative note and
	// costs the caller an allocation.
	gc:     "esc:\x00\x00\x00\x01",
	nanogo: "",
}, {
	name: "a slice that is written through",
	decl: "func F(b []byte) { b[0] = 1 }",
	// gc's mutator flow. It says the parameter does not reach the heap and
	// that the callee writes through it, and gc's walk reads it: a
	// []byte(s) whose bytes nothing mutates may share the string's own
	// bytes. "esc:" would deny the write, so nanogo refuses the case
	// instead of weakening the claim.
	gc:     "esc:\x00\x01",
	nanogo: "",
}, {
	name: "a pointer passed on to another function",
	decl: "//go:noinline\nfunc g(p *int) *int { return p }\n\nfunc F(p *int) *int { return g(p) }",
	// gc follows g's own note. nanogo has no callee summary in stage 1, so
	// an argument that carries a reference is refused whatever the callee
	// does with it.
	gc:     "esc:\x00\x00\x00\x01",
	nanogo: "",
}}

// TestEscapeNoteMatchesGC reads both toolchains' archives for the same source.
//
// It asserts three things per case. The gc column is what gc's archive holds,
// so the table cannot go stale against a toolchain update. The nanogo column
// is what nanogo's archive holds. And the note nanogo wrote is gc's own or the
// empty one, which is the property that makes a note safe to write at all: a
// note gc did not write is a claim about a caller that nanogo never compiled.
func TestEscapeNoteMatchesGC(t *testing.T) {
	goBin := needGo(t)
	if runtime.GOARCH != driver.TargetArch {
		t.Skipf("nanogo emits %s machine code and GOARCH is %s, so it refuses every package here",
			driver.TargetArch, runtime.GOARCH)
	}
	dir := t.TempDir()
	bin := buildNanogo(t, goBin, dir)

	for i, c := range escapeNoteCases {
		t.Run(c.name, func(t *testing.T) {
			if c.nanogo != "" && c.nanogo != c.gc {
				t.Fatalf("the case claims nanogo writes %q where gc writes %q, "+
					"and a note gc did not write is a claim gc's caller acts on and nanogo never made",
					c.nanogo, c.gc)
			}
			root := filepath.Join(dir, "case", strconv.Itoa(i))
			mod := filepath.Join(root, "mod")
			writeFiles(t, mod, map[string]string{
				"go.mod":     "module nanogo.example/note\n\ngo 1.21\n",
				"lib/lib.go": "package lib\n\n" + c.decl + "\n",
			})
			list := filepath.Join(root, "allowlist.txt")
			if err := os.WriteFile(list, []byte("nanogo.example/note/lib\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			gcArchive := libArchive(t, goBin, root, mod, "archive-gc", list, nil)
			if got := escapeTag(t, gcArchive); got != c.gc {
				t.Errorf("gc's note is %q, and the case says %q", got, c.gc)
			}
			nanogoArchive := libArchive(t, goBin, root, mod, "archive-nanogo", list,
				[]string{"-toolexec=" + bin})
			got := escapeTag(t, nanogoArchive)
			if got != c.nanogo {
				t.Errorf("nanogo's note is %q, and the case says %q", got, c.nanogo)
			}
			if got != "" && got != c.gc {
				t.Errorf("nanogo wrote %q where gc wrote %q. A note gc did not write "+
					"tells gc it may leave the caller's local in the caller's frame, "+
					"and nanogo's callee may hold a pointer to it", got, c.gc)
			}
		})
	}
}

// escapeNoteModule is the build the runtime rule decides.
//
// lib.Load reads b and keeps nothing, and nanogo proves it. lib.Keep stores
// its parameter in a global, and nanogo proves nothing about it.
//
// //go:noinline is what makes the note the only evidence gc has. Without it gc
// inlines the call, re-runs its own analysis over the body it inlined, and
// never reads the note at all. A test without the directive passes whatever
// the note says and proves nothing.
//
// Each consumer holds an array in its frame and hands out a slice of it.
// Whether that array can stay in the frame is decided by the callee's note and
// by nothing else.
var escapeNoteModule = map[string]string{
	"go.mod": "module nanogo.example/escapenote\n\ngo 1.21\n",
	"lib/lib.go": `package lib

var sink []byte

//go:noinline
func Load(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8
}

//go:noinline
func Keep(b []byte) {
	sink = b
}
`,
	"user/user.go": `package user

import "nanogo.example/escapenote/lib"

func F() uint64 {
	var a [32]byte
	return lib.Load(a[:])
}
`,
	"leak/leak.go": `package leak

import "nanogo.example/escapenote/lib"

func F() {
	var a [32]byte
	lib.Keep(a[:])
}
`,
	"allowlist.txt": "nanogo.example/escapenote/lib\n",
}

// TestProvedNoteUnblocksTheRuntimeRule is the blocker and its edge.
//
// The first half is what changed: gc compiles a package under the runtime rule
// against nanogo's archive, because nanogo proved the parameter flows nowhere.
// The second half is the edge that must not move with it: a parameter nanogo
// proved nothing about still carries the note that stops the build, and it
// stops with the runtime rule's own words.
//
// Without the analysis both halves fail the same way, with
// "b escapes to heap, not allowed in runtime" on the first.
func TestProvedNoteUnblocksTheRuntimeRule(t *testing.T) {
	goBin := needGo(t)
	if runtime.GOARCH != driver.TargetArch {
		t.Skipf("nanogo emits %s machine code and GOARCH is %s, so it refuses every package here",
			driver.TargetArch, runtime.GOARCH)
	}
	dir := t.TempDir()
	bin := buildNanogo(t, goBin, dir)

	mod := filepath.Join(dir, "mod")
	writeFiles(t, mod, escapeNoteModule)
	list := filepath.Join(mod, "allowlist.txt")

	t.Run("gc writes a note that names the destinations", func(t *testing.T) {
		archive := libArchive(t, goBin, dir, mod, "archive-gc", list, nil)
		if !bytes.Contains(archive, []byte("esc:")) {
			t.Error("gc's archive for lib carries no esc: tag, so this test is not reading the notes")
		}
	})

	t.Run("nanogo writes the proved note", func(t *testing.T) {
		archive := libArchive(t, goBin, dir, mod, "archive-nanogo", list, []string{"-toolexec=" + bin})
		if !bytes.Contains(archive, []byte("esc:")) {
			t.Error("nanogo's archive for lib carries no esc: tag, " +
				"so the analysis of specs/023-escape-analysis.md proved nothing about Load")
		}
	})

	t.Run("the proved note lets the array stay in the frame", func(t *testing.T) {
		logFile := filepath.Join(dir, "consume-nanogo.log")
		out, err := buildConsumer(t, goBin, dir, mod, "consume-nanogo", "user",
			[]string{"-toolexec=" + bin}, list, logFile)
		if err != nil {
			t.Fatalf("gc refused to compile user against nanogo's archive under the runtime rule: %v\n%s", err, out)
		}
		requireCompiled(t, logFile, "nanogo.example/escapenote/lib")
	})

	t.Run("an unproved parameter still stops the build", func(t *testing.T) {
		logFile := filepath.Join(dir, "consume-leak.log")
		out, err := buildConsumer(t, goBin, dir, mod, "consume-leak", "leak",
			[]string{"-toolexec=" + bin}, list, logFile)
		if err == nil {
			t.Fatalf("gc compiled leak against a note nanogo did not prove:\n%s", out)
		}
		if want := "escapes to heap, not allowed in runtime"; !strings.Contains(string(out), want) {
			t.Errorf("the failure does not name the runtime rule, so it is a different blocker:\n%s", out)
		}
		requireCompiled(t, logFile, "nanogo.example/escapenote/lib")
	})
}

// requireCompiled fails unless nanogo compiled path in the run that wrote log.
//
// A run in which nanogo never compiled the package proves nothing: the build
// would then have failed for some other reason, or not at all. This is the
// trap internal/selfhost/measure.go documents, and the fresh GOCACHE in
// escapeNoteEnv is what keeps the compile action from being reused.
func requireCompiled(t *testing.T, logFile, path string) {
	t.Helper()
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading the nanogo log: %v", err)
	}
	if !strings.Contains(string(logged), "compiled "+path) {
		t.Fatalf("nanogo did not compile %s, so this run proved nothing:\n%s", path, logged)
	}
}

// escapeTag returns the escape analysis note an archive with exactly one
// pointer-bearing parameter carries, or the empty string when it carries none.
//
// The note is read out of the export data's string section rather than found
// in the bytes. gc stores every string of the stream there, deduplicated and
// with no separator, so a tag followed by another string's bytes is
// indistinguishable from a longer tag when the file is scanned. Decoding the
// section is exact.
//
// Every case here has one pointer-bearing parameter, so the section holds at
// most one string beginning with "esc:". A second one is a defect in the
// fixture and is reported as one.
func escapeTag(t *testing.T, archive []byte) string {
	t.Helper()
	payload, err := export.Payload(archive)
	if err != nil {
		t.Fatalf("reading the export data: %v", err)
	}
	pr := pkgbits.NewPkgDecoder("", string(payload))
	found := ""
	for i := 0; i < pr.NumElems(pkgbits.SectionString); i++ {
		s := pr.StringIdx(pkgbits.RelElemIdx(i))
		if !strings.HasPrefix(s, "esc:") {
			continue
		}
		if found != "" && found != s {
			t.Fatalf("the archive holds two notes, %q and %q, so the fixture has more "+
				"than one pointer-bearing parameter and this is not a per-parameter reading",
				found, s)
		}
		found = s
	}
	return found
}

// writeFiles writes a fixture module's files under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// libArchive builds ./lib and returns the archive bytes.
//
// -work keeps the build directory, which is where the archive is. It is under
// the test's own TMPDIR, so it goes away with the test.
func libArchive(t *testing.T, goBin, dir, mod, tag, list string, extra []string) []byte {
	t.Helper()
	args := append([]string{"build", "-work"}, extra...)
	args = append(args, "-o", os.DevNull, "./lib")
	cmd := exec.Command(goBin, args...)
	cmd.Dir = mod
	cmd.Env = escapeNoteEnv(t, dir, tag, list, "")
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

// buildConsumer compiles one package with the runtime rule on and returns what
// the go command said. -+ is the compiler flag that sets CompilingRuntime,
// which cmd/go turns on for every package of cmd/internal/objabi.runtimePkgs.
func buildConsumer(t *testing.T, goBin, dir, mod, tag, pkg string, extra []string, list, logFile string) ([]byte, error) {
	t.Helper()
	args := append([]string{"build"}, extra...)
	args = append(args, "-gcflags=nanogo.example/escapenote/"+pkg+"=-+", "-o", os.DevNull, "./"+pkg)
	cmd := exec.Command(goBin, args...)
	cmd.Dir = mod
	cmd.Env = escapeNoteEnv(t, dir, tag, list, logFile)
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
func escapeNoteEnv(t *testing.T, dir, tag, list, logFile string) []string {
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
		"NANOGO_ALLOWLIST="+list,
	)
	if logFile != "" {
		env = append(env, "NANOGO_LOG="+logFile)
	}
	return env
}
