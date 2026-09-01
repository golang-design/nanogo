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
// "One" counts every declaration the archive carries a note for and not only
// the ones this file writes. A package that imports one whose functions carry
// notes re-exports those notes into its own stream, and the string section is
// deduplicated, so a callee whose note happens to equal F's leaves one entry
// that reads back as F's. A case needing a callee therefore cannot be written
// here at all, in this package or in another: [escape_test.TestParamNotes]'s
// "a parameter passed to a call leaks" is where that shape is checked, and
// [TestProvedNoteUnblocksTheRuntimeRule] is where a note gc acts on is.
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
	name: "a slice that is written through",
	decl: "func F(b []byte) { b[0] = 1 }",
	// gc's mutator flow. It says the parameter does not reach the heap and
	// that the callee writes through it, and gc's walk reads it: a
	// []byte(s) whose bytes nothing mutates may share the string's own
	// bytes. "esc:" would deny the write, so nanogo refuses the case
	// instead of weakening the claim.
	gc:     "esc:\x00\x01",
	nanogo: "",
},

	// The flows to a result, which specs/023-escape-analysis.md's stage 3
	// describes rather than refuses. Every one of them is at zero
	// dereferences, because that is the only depth the walk measures exactly.
	{
		name:   "a pointer that is returned",
		decl:   "func F(p *int) *int { return p }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "a pointer assigned to a named result",
		decl:   "func F(p *int) (r *int) { r = p; return }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "a pointer returned in the second position",
		decl:   "func F(p *int) (int, *int) { return 1, p }",
		gc:     "esc:\x00\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x00\x01",
	}, {
		name:   "a pointer returned twice",
		decl:   "func F(p *int) (*int, *int) { return p, p }",
		gc:     "esc:\x00\x00\x00\x01\x01",
		nanogo: "esc:\x00\x00\x00\x01\x01",
	}, {
		name:   "a pointer at the last result a note can name",
		decl:   "func F(p *int) (a, b, c, d, e *int) { e = p; return }",
		gc:     "esc:\x00\x00\x00\x00\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x00\x00\x00\x00\x01",
	}, {
		name: "a pointer at a result past the note",
		decl: "func F(p *int) (a, b, c, d, e, f *int) { f = p; return }",
		// gc takes the heap flow for a result its note cannot name, and
		// nanogo refuses, which reaches gc as the same claim.
		gc:     "",
		nanogo: "",
	}, {
		name: "a pointer that reaches a result and the heap",
		decl: "var g *int\n\nfunc F(p *int) *int { g = p; return p }",
		// The heap flow swallows the result flow in gc's own encoding.
		gc:     "",
		nanogo: "",
	}, {
		name:   "a pointer that reaches a result and an interface",
		decl:   "var g any\n\nfunc F(p *int) *int { g = p; return p }",
		gc:     "",
		nanogo: "",
	}, {
		name:   "a slice of a slice parameter that is returned",
		decl:   "func F(b []byte) []byte { return b[1:] }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "a slice of a string parameter that is returned",
		decl:   "func F(s string) string { return s[1:] }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "the address of an element of a slice parameter",
		decl:   "func F(b []byte) *byte { return &b[0] }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "a field of a struct parameter passed by value",
		decl:   "type T struct{ P *int }\n\nfunc F(t T) *int { return t.P }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name: "a field read through a pointer parameter",
		decl: "type T struct{ P *int }\n\nfunc F(t *T) *int { return t.P }",
		// One dereference down. The walk counts it and refuses, because it
		// writes a flow only at the depth it measures exactly.
		gc:     "esc:\x00\x00\x00\x02",
		nanogo: "",
	}, {
		name:   "an element of an array parameter",
		decl:   "func F(a [2]*int) *int { return a[0] }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "an element of a slice parameter",
		decl:   "func F(s []*int) *int { return s[0] }",
		gc:     "esc:\x00\x00\x00\x02",
		nanogo: "",
	}, {
		name: "a value read out of a map parameter",
		decl: "func F(m map[int]*int) *int { return m[0] }",
		// gc records no flow at all: what comes out of a map comes out of
		// the heap and not out of the map's own header.
		gc:     "esc:",
		nanogo: "",
	}, {
		name:   "a dereference of a parameter that is returned",
		decl:   "func F(q **int) *int { return *q }",
		gc:     "esc:\x00\x00\x00\x02",
		nanogo: "",
	}, {
		name:   "a converted parameter that is returned",
		decl:   "import \"unsafe\"\n\nfunc F(p *int) unsafe.Pointer { return unsafe.Pointer(p) }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name: "a string converted to bytes",
		decl: "func F(s string) []byte { return []byte(s) }",
		// The conversion copies into an allocation of its own, so nothing
		// of the operand reaches the result and neither toolchain records a
		// flow.
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a byte slice converted to a string",
		decl:   "func F(b []byte) string { return string(b) }",
		gc:     "esc:",
		nanogo: "esc:",
	},
	// A conversion from a slice to an array or to an array pointer has no
	// case here: nanogo's SSA builder has no row for either, so there is no
	// archive to read. escape_test.go's table covers what the walk answers
	// for them, because it stops at ir.Build.

	// The operations stage 2 added to the allowlist.
	{
		name:   "a range that only reads its operand",
		decl:   "func F(s []*int) int {\n\tn := 0\n\tfor _, v := range s {\n\t\tif v != nil {\n\t\t\tn++\n\t\t}\n\t}\n\treturn n\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name: "a range element stored in a global",
		decl: "var g *int\n\nfunc F(s []*int) {\n\tfor _, v := range s {\n\t\tg = v\n\t}\n}",
		// gc's "leaking param content": a heap flow one dereference down.
		// The walk has no encoding for a heap flow at any depth, so it
		// refuses.
		gc:     "esc:\x02",
		nanogo: "",
	}, {
		name:   "a range over a map",
		decl:   "func F(m map[string]int) int {\n\tn := 0\n\tfor range m {\n\t\tn++\n\t}\n\treturn n\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a range over a string",
		decl:   "func F(s string) int {\n\tn := 0\n\tfor _, c := range s {\n\t\tn += int(c)\n\t}\n\treturn n\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a type switch that only reads",
		decl:   "type T struct{ A int }\n\nfunc F(i any) int {\n\tswitch v := i.(type) {\n\tcase *T:\n\t\treturn v.A\n\t}\n\treturn 0\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a type switch variable that is returned",
		decl:   "func F(i any) *int {\n\tswitch v := i.(type) {\n\tcase *int:\n\t\treturn v\n\t}\n\treturn nil\n}",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "a type switch variable stored in a global",
		decl:   "var g *int\n\nfunc F(i any) {\n\tswitch v := i.(type) {\n\tcase *int:\n\t\tg = v\n\t}\n}",
		gc:     "",
		nanogo: "",
	}, {
		name:   "a type assertion that only reads",
		decl:   "type T struct{ A int }\n\nfunc F(i any) int { return i.(*T).A }",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a type assertion that is returned",
		decl:   "func F(i any) *int { return i.(*int) }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name: "a type assertion to a value type that is returned",
		decl: "type T struct {\n\tP *int\n\tA int\n}\n\nfunc F(i any) T { return i.(T) }",
		// A value that is not pointer-shaped sits in an allocation of its
		// own and the interface's second word points at it, so reading it
		// out is a dereference and the walk refuses the depth.
		gc:     "esc:\x00\x00\x00\x02",
		nanogo: "",
	}, {
		name:   "a struct literal built in the frame",
		decl:   "type T struct{ P *int }\n\nfunc F(p *int) int {\n\tt := T{P: p}\n\treturn *t.P\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a struct literal that is returned",
		decl:   "type T struct{ P *int }\n\nfunc F(p *int) T { return T{P: p} }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "an array literal built in the frame",
		decl:   "func F(p *int) int {\n\ta := [2]*int{p, nil}\n\treturn *a[0]\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a keyed array literal built in the frame",
		decl:   "func F(p *int) int {\n\ta := [4]*int{2: p}\n\treturn *a[2]\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name: "a slice literal",
		decl: "func F(p *int) int {\n\ts := []*int{p}\n\treturn *s[0]\n}",
		// gc keeps the backing store in the frame. nanogo allocates every
		// literal that is not a struct or an array, so the parameter is in
		// the heap whatever this pass decides.
		gc:     "esc:",
		nanogo: "",
	}, {
		name:   "the address of a struct literal",
		decl:   "type T struct{ P *int }\n\nfunc F(p *int) int {\n\tt := &T{P: p}\n\treturn *t.P\n}",
		gc:     "esc:",
		nanogo: "",
	},

	// The round trip through a word the collector does not trace. unsafe's
	// rule (3) is what ends the flow at a variable, and internal/abi.NoEscape
	// is the declaration that turns on it.
	{
		name:   "a uintptr held in a variable, which is internal/abi.NoEscape's own shape",
		decl:   "import \"unsafe\"\n\nfunc F(p unsafe.Pointer) unsafe.Pointer {\n\tx := uintptr(p)\n\treturn unsafe.Pointer(x ^ 0)\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a round trip inside one expression, which unsafe allows",
		decl:   "import \"unsafe\"\n\nfunc F(p unsafe.Pointer) unsafe.Pointer { return unsafe.Pointer(uintptr(p) ^ 0) }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "an offset inside one expression, which unsafe allows",
		decl:   "import \"unsafe\"\n\nfunc F(p *int) unsafe.Pointer { return unsafe.Pointer(uintptr(unsafe.Pointer(p)) + 8) }",
		gc:     "esc:\x00\x00\x00\x01",
		nanogo: "esc:\x00\x00\x00\x01",
	}, {
		name:   "a round trip inside one expression into a global",
		decl:   "import \"unsafe\"\n\nvar g unsafe.Pointer\n\nfunc F(p *int) { g = unsafe.Pointer(uintptr(unsafe.Pointer(p))) }",
		gc:     "",
		nanogo: "",
	}, {
		name:   "a round trip through a variable into a global, which unsafe calls invalid",
		decl:   "import \"unsafe\"\n\nvar g unsafe.Pointer\n\nfunc F(p *int) {\n\tu := uintptr(unsafe.Pointer(p))\n\tg = unsafe.Pointer(u)\n}",
		gc:     "esc:",
		nanogo: "esc:",
	}, {
		name:   "a parameter converted to a uintptr result",
		decl:   "import \"unsafe\"\n\nfunc F(p *int) uintptr { return uintptr(unsafe.Pointer(p)) }",
		gc:     "esc:",
		nanogo: "esc:",
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

// noEscapeModule is internal/abi.NoEscape's own shape, end to end.
//
// lib.NoEscape is the body in go1.27.0's internal/abi/escape.go, character for
// character. lib.Read is the callee the address reaches, and //go:noinline is
// what makes the note the only evidence gc has: gc re-analyses a body it
// inlined and never reads the note for it.
//
// user is compiled with -+, the flag cmd/go sets for every package of
// cmd/internal/objabi.runtimePkgs, and it hands the address of its own local
// through NoEscape to Read. gc may leave that local in user's frame only
// because of the two notes lib's archive carries, so the number the program
// prints is the answer those notes authorised. main runs it twice with a
// collection in between, because a pointer into a frame that is gone reads
// correctly until something overwrites that memory.
var noEscapeModule = map[string]string{
	"go.mod": "module nanogo.example/noescape\n\ngo 1.21\n",
	"lib/lib.go": `package lib

import "unsafe"

//go:nosplit
//go:nocheckptr
func NoEscape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

//go:noinline
func Read(p unsafe.Pointer) int {
	return *(*int)(p)
}
`,
	"user/user.go": `package user

import (
	"unsafe"

	"nanogo.example/noescape/lib"
)

func Run() int {
	total := 0
	for i := 0; i < 100; i++ {
		key := i * 7
		total += lib.Read(lib.NoEscape(unsafe.Pointer(&key)))
	}
	return total
}
`,
	"main.go": `package main

import (
	"runtime"

	"nanogo.example/noescape/user"
)

func main() {
	n := user.Run()
	runtime.GC()
	m := user.Run()
	println("run1", n)
	println("run2", m)
}
`,
	"allowlist.txt": "nanogo.example/noescape/lib\n",
}

// TestTheNoEscapeShapeBuildsAndRuns is the measured blocker, and it reads the
// program and not only the archive.
//
// specs/023-escape-analysis.md's three internal/runtime/maps sites are all of
// the form m.Delete(typ, abi.NoEscape(unsafe.Pointer(&key))). Before the round
// trip through a uintptr was in the allowlist, nanogo wrote the empty note for
// NoEscape's parameter and gc stopped this build with the runtime rule's own
// words. The assertions below are both halves: gc compiles it now, and the
// binary agrees with an all-gc build of the same source.
func TestTheNoEscapeShapeBuildsAndRuns(t *testing.T) {
	goBin := needGo(t)
	if runtime.GOARCH != driver.TargetArch {
		t.Skipf("nanogo emits %s machine code and GOARCH is %s, so it refuses every package here",
			driver.TargetArch, runtime.GOARCH)
	}
	dir := t.TempDir()
	bin := buildNanogo(t, goBin, dir)

	mod := filepath.Join(dir, "mod")
	writeFiles(t, mod, noEscapeModule)
	list := filepath.Join(mod, "allowlist.txt")

	want := runProgram(t, goBin, dir, mod, "run-gc", nil, list, "")
	if !strings.Contains(want, "34650") {
		t.Fatalf("the all-gc build printed %q, so the fixture is not computing what this test reads", want)
	}

	logFile := filepath.Join(dir, "nanogo.log")
	got := runProgram(t, goBin, dir, mod, "run-nanogo", []string{"-toolexec=" + bin}, list, logFile)
	requireCompiled(t, logFile, "nanogo.example/noescape/lib")
	if got != want {
		t.Errorf("the program built against nanogo's archive printed\n%s\nand the all-gc build printed\n%s", got, want)
	}
}

// runProgram builds the fixture's main package with the runtime rule on for
// user, runs it, and returns what it printed.
func runProgram(t *testing.T, goBin, dir, mod, tag string, extra []string, list, logFile string) string {
	t.Helper()
	out := filepath.Join(dir, tag+".bin")
	args := append([]string{"build"}, extra...)
	args = append(args, "-gcflags=nanogo.example/noescape/user=-+", "-o", out, ".")
	build := exec.Command(goBin, args...)
	build.Dir = mod
	build.Env = escapeNoteEnv(t, dir, tag, list, logFile)
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the %s binary: %v\n%s", tag, err, b)
	}
	run := exec.Command(out)
	run.Env = escapeNoteEnv(t, dir, tag, list, "")
	b, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("running the %s binary: %v\n%s", tag, err, b)
	}
	return string(b)
}
