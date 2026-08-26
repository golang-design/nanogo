// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"strings"
	"testing"
)

// The three ways nanogo's build can fail mean different things, and merging
// any two of them would hide a bug behind an expected gap. These are real
// messages, copied from sweeps.
func TestBuildFailureIsClassifiedByWhatItSays(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		want   Class
		reason string
	}{{
		name:   "a construct nanogo cannot compile is a refusal",
		out:    "nanogo: main: nanogo cannot compile function main at /tmp/x/map.go:3:6: ir.Lower: ir: lowering main: compositelit: a map literal needs runtime.makemap, and the row that calls it is not built\n",
		want:   ClassRefused,
		reason: "function NAME: ir.Lower: ir: lowering NAME: compositelit: a map literal needs runtime.makemap, and the row that calls it is not built",
	}, {
		name:   "a panic is a crash and never a refusal",
		out:    "panic: arm64: ZR used where the encoding means a floating-point register\n\ngoroutine 1 [running]:\n",
		want:   ClassCrashed,
		reason: "panic: arm64: ZR used where the encoding means a floating-point register",
	}, {
		name:   "a runtime fatal error is a crash too",
		out:    "fatal error: concurrent map writes\n",
		want:   ClassCrashed,
		reason: "fatal error: concurrent map writes",
	}, {
		name:   "a diagnostic in a program gc accepts is nanogo rejecting legal Go",
		out:    "nanogo: /tmp/x/a.go:4:14: cannot use \"hello\" (untyped string constant) as int value\n",
		want:   ClassFalseError,
		reason: "nanogo: /tmp/x/a.go:4:14: cannot use \"hello\" (untyped string constant) as int value",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := judgeBuildFailure(Verdict{File: "x.go"}, result{out: c.out, code: 1})
			if got.Class != c.want {
				t.Errorf("class: got %q, want %q", got.Class, c.want)
			}
			if got.Reason != c.reason {
				t.Errorf("reason:\n\tgot  %q\n\twant %q", got.Reason, c.reason)
			}
			if got.Detail != c.out {
				t.Error("the detail must carry the whole message, or a reader cannot act on it")
			}
		})
	}

	// A compile that never finished is neither, whatever it printed.
	got := judgeBuildFailure(Verdict{File: "x.go"}, result{out: "panic: something", code: 1, timedOut: true})
	if got.Class != ClassTimedOut {
		t.Errorf("a compile that ran out of time was classified %q", got.Class)
	}
}

// A refusal that mentions no construct still produces no reason rather than a
// wrong one.
func TestReasonOfSomethingThatIsNotARefusal(t *testing.T) {
	if r := Reason("nanogo: /tmp/a.go:1:1: some ordinary error\n"); r != "" {
		t.Errorf("got %q, want the empty string", r)
	}
}

// Two refusals for one reason must land in one bucket. Without the
// normalisation each file is its own bucket and the ranked list this package
// exists to produce is a list of ones.
func TestEqualRefusalsGroupTogether(t *testing.T) {
	a := Reason("nanogo: main: nanogo cannot compile function assertequal at /a/for.go:9:6: ssa.Build: ssa: assertequal: convert: a conversion from int to interface is not built yet")
	b := Reason("nanogo: main: nanogo cannot compile function check at /elsewhere/if.go:31:1: ssa.Build: ssa: check: convert: a conversion from int to interface is not built yet")
	if a != b {
		t.Errorf("two refusals for one reason did not group:\n\t%q\n\t%q", a, b)
	}
	if !strings.Contains(a, "ssa.Build") {
		t.Errorf("the stage was normalised away, and which stage refuses is half of what a reader needs: %q", a)
	}

	// Two refusals for different reasons must not group.
	c := Reason("nanogo: main: nanogo cannot compile function f at /a/x.go:9:6: ssa.Build: ssa: f: convert: a conversion from string to interface is not built yet")
	if a == c {
		t.Error("refusals about different types were merged")
	}
}

func TestFirstErrorLine(t *testing.T) {
	cases := []struct {
		out  string
		want int
		ok   bool
	}{
		{"nanogo: /tmp/x/a.go:4:14: cannot use x\n", 4, true},
		// nanogo resolves the package graph through the go command, so a
		// file the go command cannot parse comes back as its diagnostic,
		// with no nanogo prefix.
		{"nanogo: go list -f {{.ImportPath}} import5.go: exit status 1\nimport5.go:24:8: import path must be a string\n", 24, true},
		{"./a.go:7:1: syntax error\n", 7, true},
		{"nanogo: something went wrong with no position\n", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := FirstErrorLine(c.out)
		if got != c.want || ok != c.ok {
			t.Errorf("FirstErrorLine(%q) = %d, %v; want %d, %v", c.out, got, ok, c.want, c.ok)
		}
	}
}

func TestErrorAnnotations(t *testing.T) {
	src := `package main

func f() {
	var x int = "s" // ERROR "cannot use"
	_ = x
}

func g() int {} // GC_ERROR "missing return"

// GCCGO_ERROR "not nanogo's business"
`
	got := ErrorAnnotations([]byte(src))
	want := []int{4, 8}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if n := ErrorAnnotations([]byte("package main\n")); len(n) != 0 {
		t.Errorf("a file with no annotation reported %v", n)
	}
}

// The path this whole package exists to exercise, and until this test nothing
// in the repository had ever run it: the sweep has found no mismatch, so the
// code that reports one had never executed. A mismatch report that panicked or
// said nothing useful would be discovered on the day a miscompilation was
// found, which is the worst possible day to discover it.
func TestDiffReportsAMiscompilationInFull(t *testing.T) {
	t.Run("two runs that agree", func(t *testing.T) {
		a := result{out: "hello\n", code: 0}
		if d := diff(a, a); d != "" {
			t.Errorf("identical runs were reported as differing: %s", d)
		}
	})

	t.Run("a disagreement about the exit status", func(t *testing.T) {
		want := result{out: "", code: 0}
		got := result{out: "panic: index out of range\n", code: 2}
		d := diff(want, got)
		if d == "" {
			t.Fatal("a program that exited differently was reported as matching")
		}
		for _, s := range []string{"gc exited 0", "exited 2", "panic: index out of range", "(nothing)"} {
			if !strings.Contains(d, s) {
				t.Errorf("the report does not contain %q:\n%s", s, d)
			}
		}
	})

	t.Run("a disagreement about the output", func(t *testing.T) {
		d := diff(result{out: "6\n", code: 0}, result{out: "5\n", code: 0})
		if d == "" {
			t.Fatal("two different outputs were reported as matching")
		}
		if !strings.Contains(d, "both exited 0") || !strings.Contains(d, "6") || !strings.Contains(d, "5") {
			t.Errorf("the report does not carry both outputs:\n%s", d)
		}
	})

	t.Run("a traceback is not a disagreement", func(t *testing.T) {
		// Two runs of one program produce different addresses and
		// goroutine numbers. Reporting that as a miscompilation would
		// make every panicking program a false alarm.
		a := result{out: "panic: boom\n\ngoroutine 1 [running]:\nmain.main()\n\t/tmp/x/a.go:9 +0x1c\n", code: 2}
		b := result{out: "panic: boom\n\ngoroutine 7 [running]:\nmain.main()\n\t/tmp/x/a.go:9 +0x9f4\n", code: 2}
		if d := diff(a, b); d != "" {
			t.Errorf("two runs of one panicking program were reported as differing:\n%s", d)
		}
	})

	t.Run("a real difference under a traceback is still found", func(t *testing.T) {
		a := result{out: "panic: boom\n\ngoroutine 1 [running]:\n", code: 2}
		b := result{out: "panic: different boom\n\ngoroutine 1 [running]:\n", code: 2}
		if diff(a, b) == "" {
			t.Error("two different panic messages were normalised into agreement")
		}
	})
}

func TestItoaAndIndent(t *testing.T) {
	for n, want := range map[int]string{0: "0", 7: "7", 42: "42", -3: "-3", -100: "-100"} {
		if got := itoa(n); got != want {
			t.Errorf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
	if got := indent(""); got != "\t(nothing)\n" {
		t.Errorf("empty output rendered as %q; a report that shows nothing must say so", got)
	}
	if got := indent("a\nb\n"); got != "\ta\n\tb\n" {
		t.Errorf("got %q", got)
	}
}

// A recipe nanogo cannot be given faithfully must say which part it cannot
// honour. "skipped" with no reason is the thing this package refuses to do.
func TestUnhonourableRecipes(t *testing.T) {
	cases := []struct {
		h    Header
		want string
	}{
		{Header{Kind: "run"}, ""},
		{Header{Kind: "run", Flags: []string{"-m", "-l"}}, "compiler flags"},
		{Header{Kind: "run", Env: []string{"GOEXPERIMENT=x"}}, "build environment"},
		{Header{Kind: "runindir", ModVersion: "1.21"}, "module version"},
	}
	for _, c := range cases {
		got := unhonourable(c.h)
		if c.want == "" {
			if got != "" {
				t.Errorf("%+v was refused: %s", c.h, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%+v: got %q, want it to mention %q", c.h, got, c.want)
		}
	}
}

// A companion file the recipe names and the corpus does not hold must be an
// error rather than a build of the wrong inputs.
func TestInputsSplitsArgumentsFromSources(t *testing.T) {
	all := []File{{Name: "cmplxdivide1.go"}}
	f := File{Name: "cmplxdivide.go", Header: Header{Kind: "run", Args: []string{"cmplxdivide1.go"}}}
	srcs, argv, err := inputs("corpus", f, all)
	if err != nil {
		t.Fatalf("inputs: %v", err)
	}
	if len(srcs) != 2 || len(argv) != 0 {
		t.Fatalf("got srcs %v, argv %v", srcs, argv)
	}

	f = File{Name: "args.go", Header: Header{Kind: "run", Args: []string{"arg1", "arg2"}}}
	srcs, argv, err = inputs("corpus", f, all)
	if err != nil {
		t.Fatalf("inputs: %v", err)
	}
	if len(srcs) != 1 || len(argv) != 2 {
		t.Fatalf("got srcs %v, argv %v", srcs, argv)
	}

	f = File{Name: "x.go", Header: Header{Kind: "run", Args: []string{"absent.go"}}}
	if _, _, err := inputs("corpus", f, all); err == nil {
		t.Error("a companion file that is not vendored was accepted")
	}
}

// The totals check is the point of the design, so its own failure paths are
// tested rather than assumed.
func TestCheckTotalsFailsWhenAFileVanishes(t *testing.T) {
	ok := &Report{Files: 2, Verdicts: []Verdict{
		{File: "a.go", Class: ClassMatched},
		{File: "b.go", Class: ClassRefused},
	}}
	if err := ok.CheckTotals(); err != nil {
		t.Fatalf("a report that adds up was rejected: %v", err)
	}

	short := &Report{Files: 3, Verdicts: ok.Verdicts}
	if err := short.CheckTotals(); err == nil {
		t.Error("a report whose classes account for fewer files than were read was accepted")
	}

	// A class outside Classes is a file that would never be printed and
	// never be counted.
	stray := &Report{Files: 1, Verdicts: []Verdict{{File: "a.go", Class: Class("invented")}}}
	if err := stray.CheckTotals(); err == nil {
		t.Error("a verdict with a class outside Classes was accepted")
	}
}

func TestReadCorpusRejectsAMissingDirectory(t *testing.T) {
	if _, err := ReadCorpus("no/such/directory"); err == nil {
		t.Error("a missing corpus directory was accepted")
	}
}

// The stub is added only where it is needed, and never to a file that has a
// main of its own.
func TestNeedsMainStub(t *testing.T) {
	cases := map[string]bool{
		"package main\n\nfunc f() {}\n":              true,
		"package main\n\nfunc main() {}\n":           false,
		"package main\n\nfunc main()\t{}\n":          false,
		"package p\n\nfunc f() {}\n":                 false,
		"package mainly\n\nfunc f() {}\n":            false,
		"package main\n\n// func main() is a note\n": true,
	}
	for src, want := range cases {
		if got := needsMainStub([]byte(src)); got != want {
			t.Errorf("needsMainStub(%q) = %v, want %v", src, got, want)
		}
	}
}
