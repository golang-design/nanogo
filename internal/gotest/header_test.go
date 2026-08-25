// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"errors"
	"strings"
	"testing"
)

// The recipes the corpus actually carries, plus the corners of the grammar
// that testdir implements and the corpus does not currently reach. A recipe
// read wrongly makes a verdict about a test nobody asked for, so the reading
// is pinned here rather than assumed from the sweep.
func TestParseHeader(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want Header
	}{{
		name: "the ordinary run recipe, above the copyright notice",
		src:  "// run\n\n// Copyright 2009 The Go Authors.\n\npackage main\n",
		want: Header{Kind: "run"},
	}, {
		name: "a recipe below a build constraint",
		src:  "//go:build !nacl\n// +build !nacl\n\n// run\n\npackage main\n",
		want: Header{Kind: "run"},
	}, {
		name: "the unspaced +build spelling",
		src:  "//+build linux\n// compile\n\npackage main\n",
		want: Header{Kind: "compile"},
	}, {
		name: "errorcheck expects a rejection",
		src:  "// errorcheck\n\npackage main\n",
		want: Header{Kind: "errorcheck", WantError: true},
	}, {
		name: "errorcheckwithauto is an errorcheck",
		src:  "// errorcheckwithauto -0 -m\n\npackage main\n",
		want: Header{Kind: "errorcheck", Flags: []string{"-m"}},
	}, {
		name: "-1 asks for a rejection where the kind does not",
		src:  "// compile -1\n\npackage main\n",
		want: Header{Kind: "compile", WantError: true},
	}, {
		name: "run with argv",
		src:  "// run arg1 arg2\n\npackage main\n",
		want: Header{Kind: "run", Args: []string{"arg1", "arg2"}},
	}, {
		name: "run with a companion source file",
		src:  "// run cmplxdivide1.go\n\npackage main\n",
		want: Header{Kind: "run", Args: []string{"cmplxdivide1.go"}},
	}, {
		name: "run with a compiler flag",
		src:  "// run -gcflags=-d=converthash=qy\n\npackage main\n",
		want: Header{Kind: "run", Flags: []string{"-gcflags=-d=converthash=qy"}},
	}, {
		name: "a flag and its value are two fields",
		src:  "// run -gcflags -l=4\n\npackage main\n",
		want: Header{Kind: "run", Flags: []string{"-gcflags", "-l=4"}},
	}, {
		name: "a timeout",
		src:  "// run -t 120\n\npackage main\n",
		want: Header{Kind: "run", Timeout: 120},
	}, {
		name: "an experiment becomes an environment setting",
		src:  "// run -goexperiment fieldtrack\n\npackage main\n",
		want: Header{Kind: "run", Env: []string{"GOEXPERIMENT=fieldtrack"}},
	}, {
		name: "a godebug setting",
		src:  "// run -godebug gotypesalias=1\n\npackage main\n",
		want: Header{Kind: "run", Env: []string{"GODEBUG=gotypesalias=1"}},
	}, {
		name: "a module version",
		src:  "// runindir -gomodversion 1.21\n\npackage main\n",
		want: Header{Kind: "runindir", ModVersion: "1.21"},
	}, {
		name: "-s is read and has no effect on a single file",
		src:  "// errorcheckdir -s\n\npackage main\n",
		want: Header{Kind: "errorcheckdir", WantError: true},
	}, {
		name: "a quoted flag value keeps its spaces",
		src:  "// compile \"-gcflags=-d a b\"\n\npackage main\n",
		want: Header{Kind: "compile", Flags: []string{"-gcflags=-d a b"}},
	}, {
		name: "an argument that is not a flag stops flag reading",
		src:  "// run x.go -notaflag\n\npackage main\n",
		want: Header{Kind: "run", Args: []string{"x.go", "-notaflag"}},
	}, {
		name: "errorcheckandrundir expects no error, because it also runs",
		src:  "// errorcheckandrundir\n\npackage main\n",
		want: Header{Kind: "errorcheckandrundir"},
	}, {
		name: "-0 cancels the kind's expectation of a rejection",
		src:  "// errorcheck -0 -m\n\npackage main\n",
		want: Header{Kind: "errorcheck", Flags: []string{"-m"}},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseHeader([]byte(c.src))
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if !sameHeader(got, c.want) {
				t.Errorf("ParseHeader\n\tgot  %+v\n\twant %+v", got, c.want)
			}
		})
	}
}

func sameHeader(a, b Header) bool {
	return a.Kind == b.Kind && a.WantError == b.WantError &&
		a.Timeout == b.Timeout && a.ModVersion == b.ModVersion &&
		strings.Join(a.Flags, "\x00") == strings.Join(b.Flags, "\x00") &&
		strings.Join(a.Args, "\x00") == strings.Join(b.Args, "\x00") &&
		strings.Join(a.Env, "\x00") == strings.Join(b.Env, "\x00")
}

// A file whose recipe cannot be read must say so, and must say which of the
// two failures it is. Both are counted in the sweep, and a file counted in
// neither would leave the totals short.
func TestParseHeaderRejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want error // nil means "any error, checked by message"
		msg  string
	}{{
		name: "a file that opens with its copyright notice has no recipe",
		src:  "// Copyright 2015 The Go Authors. All rights reserved.\n\npackage main\n",
		want: ErrUnknownKind,
	}, {
		name: "a file that opens with code",
		src:  "package main\n",
		want: ErrNoRecipe,
	}, {
		name: "an empty file",
		src:  "",
		want: ErrNoRecipe,
	}, {
		name: "a file of nothing but blank lines",
		src:  "\n\n   \n",
		want: ErrNoRecipe,
	}, {
		name: "a file of nothing but build constraints",
		src:  "//go:build linux\n// +build linux\n",
		want: ErrNoRecipe,
	}, {
		name: "an empty comment is not a recipe",
		src:  "//\n\npackage main\n",
		want: ErrNoRecipe,
	}, {
		name: "a block comment is not where the recipe lives",
		src:  "/* run */\n\npackage main\n",
		want: ErrNoRecipe,
	}, {
		name: "a word that is no recipe at all",
		src:  "// frobnicate\n\npackage main\n",
		want: ErrUnknownKind,
	}, {
		name: "an unclosed quote",
		src:  "// run \"x\n\npackage main\n",
		msg:  "unclosed quote",
	}, {
		name: "an unfinished escape",
		src:  "// run x\\",
		msg:  "unfinished escape",
	}, {
		name: "-t with no number",
		src:  "// run -t\n\npackage main\n",
		msg:  "-t with no number",
	}, {
		name: "-t with something that is not a number",
		src:  "// run -t soon\n\npackage main\n",
		msg:  "-t wants a number",
	}, {
		name: "-goexperiment with no value",
		src:  "// run -goexperiment\n\npackage main\n",
		msg:  "-goexperiment with no value",
	}, {
		name: "-godebug with no value",
		src:  "// run -godebug\n\npackage main\n",
		msg:  "-godebug with no value",
	}, {
		name: "-gomodversion with no value",
		src:  "// runindir -gomodversion\n\npackage main\n",
		msg:  "-gomodversion with no value",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseHeader([]byte(c.src))
			if err == nil {
				t.Fatal("the recipe was accepted")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if c.msg != "" && !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("got %q, want it to mention %q", err, c.msg)
			}
		})
	}
}

// The recipe is read out of the first comment even when the file after it is
// not Go at all. Several corpus files are deliberately malformed, and a header
// reader that parsed the file would classify them by its own failure instead
// of by what they ask for.
func TestParseHeaderDoesNotParseTheProgram(t *testing.T) {
	h, err := ParseHeader([]byte("// errorcheck\n\npackage main\nfunc ( { ) }\n"))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Kind != "errorcheck" || !h.WantError {
		t.Fatalf("got %+v", h)
	}
}
