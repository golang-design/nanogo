// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestParseCompileFlags covers every flag in the tables of
// specs/050-driver.md, in both the -f v and the -f=v form.
func TestParseCompileFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want Config
	}{
		// Sent on every compile invocation.
		{"o space", []string{"-o", "a.a"}, Config{Output: "a.a"}},
		{"o equals", []string{"-o=a.a"}, Config{Output: "a.a"}},
		{"p space", []string{"-p", "strconv"}, Config{Package: "strconv"}},
		{"p equals", []string{"-p=strconv"}, Config{Package: "strconv"}},
		{"importcfg space", []string{"-importcfg", "$WORK/importcfg"}, Config{ImportCfgFile: "$WORK/importcfg"}},
		{"importcfg equals", []string{"-importcfg=$WORK/importcfg"}, Config{ImportCfgFile: "$WORK/importcfg"}},
		{"lang space", []string{"-lang", "go1.27"}, Config{Lang: "go1.27"}},
		{"lang equals", []string{"-lang=go1.27"}, Config{Lang: "go1.27"}},
		{"buildid space", []string{"-buildid", "abc123"}, Config{BuildID: "abc123"}},
		{"buildid equals", []string{"-buildid=abc123"}, Config{BuildID: "abc123"}},
		{"goversion space", []string{"-goversion", "go1.27.0"}, Config{GoVersion: "go1.27.0"}},
		{"goversion equals", []string{"-goversion=go1.27.0"}, Config{GoVersion: "go1.27.0"}},
		{
			"trimpath space",
			[]string{"-trimpath", "/src/pkg=>pkg;/work/b001/=>"},
			Config{
				TrimPath: "/src/pkg=>pkg;/work/b001/=>",
				TrimRewrites: []TrimRewrite{
					{Old: "/src/pkg", New: "pkg"},
					// The go command always ends the value with an erasing
					// rewrite, so an empty new side is valid.
					{Old: "/work/b001/", New: ""},
				},
			},
		},
		{
			"trimpath equals",
			[]string{"-trimpath=/a=>b"},
			Config{TrimPath: "/a=>b", TrimRewrites: []TrimRewrite{{Old: "/a", New: "b"}}},
		},
		{
			"trimpath trailing separator",
			[]string{"-trimpath=/a=>b;"},
			Config{TrimPath: "/a=>b;", TrimRewrites: []TrimRewrite{{Old: "/a", New: "b"}}},
		},
		{"trimpath empty", []string{"-trimpath="}, Config{}},
		{"c space", []string{"-c", "8"}, Config{Concurrency: 8}},
		{"c equals", []string{"-c=8"}, Config{Concurrency: 8}},
		{"pack", []string{"-pack"}, Config{Pack: true}},
		{"pack equals", []string{"-pack=true"}, Config{Pack: true}},
		{"pack equals false", []string{"-pack=false"}, Config{Pack: false}},
		{"nolocalimports", []string{"-nolocalimports"}, Config{NoLocalImports: true}},
		{"nolocalimports equals", []string{"-nolocalimports=true"}, Config{NoLocalImports: true}},
		{"shared", []string{"-shared"}, Config{Shared: true}},
		{"shared equals", []string{"-shared=true"}, Config{Shared: true}},

		// Sent conditionally.
		{"complete", []string{"-complete"}, Config{Complete: true}},
		{"complete equals", []string{"-complete=true"}, Config{Complete: true}},
		{"symabis space", []string{"-symabis", "$WORK/symabis"}, Config{SymABIs: "$WORK/symabis"}},
		{"symabis equals", []string{"-symabis=$WORK/symabis"}, Config{SymABIs: "$WORK/symabis"}},
		{"asmhdr space", []string{"-asmhdr", "$WORK/go_asm.h"}, Config{AsmHdr: "$WORK/go_asm.h"}},
		{"asmhdr equals", []string{"-asmhdr=$WORK/go_asm.h"}, Config{AsmHdr: "$WORK/go_asm.h"}},
		{"std", []string{"-std"}, Config{Std: true}},
		{"std equals", []string{"-std=true"}, Config{Std: true}},
		{"embedcfg space", []string{"-embedcfg", "$WORK/embedcfg"}, Config{EmbedCfgFile: "$WORK/embedcfg"}},
		{"embedcfg equals", []string{"-embedcfg=$WORK/embedcfg"}, Config{EmbedCfgFile: "$WORK/embedcfg"}},
		{"compiling runtime", []string{"-+"}, Config{CompilingRuntime: true}},
		{"compiling runtime equals", []string{"-+=true"}, Config{CompilingRuntime: true}},

		// Flags nanogo defines.
		{"N", []string{"-N"}, Config{NoOptimize: 1}},
		{"N equals", []string{"-N=1"}, Config{NoOptimize: 1}},
		{"l", []string{"-l"}, Config{NoInline: 1}},
		{"l twice", []string{"-l", "-l"}, Config{NoInline: 2}},
		{"l equals count", []string{"-l=4"}, Config{NoInline: 4}},
		{"l equals false", []string{"-l=false"}, Config{NoInline: 0}},
		{"S", []string{"-S"}, Config{AsmListing: 1}},
		{"S equals", []string{"-S=2"}, Config{AsmListing: 2}},
		{"m", []string{"-m"}, Config{OptDecisions: 1}},
		{"m equals", []string{"-m=2"}, Config{OptDecisions: 2}},
		{"m twice", []string{"-m", "-m"}, Config{OptDecisions: 2}},
		{"d space", []string{"-d", "ssa/prove/debug=1"}, Config{Debug: []string{"ssa/prove/debug=1"}}},
		{"d equals", []string{"-d=checkptr=0"}, Config{Debug: []string{"checkptr=0"}}},
		{"d repeated", []string{"-d=a", "-d", "b"}, Config{Debug: []string{"a", "b"}}},
		{"fallback", []string{"-fallback"}, Config{Fallback: true}},
		{"fallback equals", []string{"-fallback=true"}, Config{Fallback: true}},

		// A bool or a count flag must not swallow the argument after it. That
		// argument is a source file.
		{"pack before file", []string{"-pack", "a.go"}, Config{Pack: true, Files: []string{"a.go"}}},
		{"shared before file", []string{"-shared", "a.go"}, Config{Shared: true, Files: []string{"a.go"}}},
		{"std before file", []string{"-std", "a.go"}, Config{Std: true, Files: []string{"a.go"}}},
		{"complete before file", []string{"-complete", "a.go"}, Config{Complete: true, Files: []string{"a.go"}}},
		{"runtime before file", []string{"-+", "a.go"}, Config{CompilingRuntime: true, Files: []string{"a.go"}}},
		{"m before file", []string{"-m", "a.go"}, Config{OptDecisions: 1, Files: []string{"a.go"}}},
		{"N before file", []string{"-N", "a.go"}, Config{NoOptimize: 1, Files: []string{"a.go"}}},
		{"l before file", []string{"-l", "a.go"}, Config{NoInline: 1, Files: []string{"a.go"}}},
		{"S before file", []string{"-S", "a.go"}, Config{AsmListing: 1, Files: []string{"a.go"}}},
		{"fallback before file", []string{"-fallback", "a.go"}, Config{Fallback: true, Files: []string{"a.go"}}},

		// The double dash form, which a person typing by hand uses.
		{"double dash", []string{"--p", "strconv"}, Config{Package: "strconv"}},
		{"double dash equals", []string{"--p=strconv"}, Config{Package: "strconv"}},

		{"files only", []string{"a.go", "b.go"}, Config{Files: []string{"a.go", "b.go"}}},
		{"no arguments", nil, Config{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCompile(tt.args)
			if err != nil {
				t.Fatalf("ParseCompile(%q) = error %v, want no error", tt.args, err)
			}
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("ParseCompile(%q) =\n\t%+v\nwant\n\t%+v", tt.args, *got, tt.want)
			}
		})
	}
}

// TestParseCompileRealLine is the command line spikes/toolexec measured on a
// real build, so a change that breaks it breaks every build.
func TestParseCompileRealLine(t *testing.T) {
	args := []string{
		"-o", "/work/b002/_pkg_.a",
		"-trimpath", "/src/hello=>hello;/work/b002=>",
		"-p", "hello",
		"-lang=go1.27",
		"-complete",
		"-buildid", "n5Xk0d/n5Xk0d",
		"-goversion", "go1.27.0",
		"-c=4",
		"-shared",
		"-nolocalimports",
		"-importcfg", "/work/b002/importcfg",
		"-pack",
		"/src/hello/main.go",
	}
	cfg, err := ParseCompile(args)
	if err != nil {
		t.Fatalf("ParseCompile: %v", err)
	}
	want := Config{
		Output:   "/work/b002/_pkg_.a",
		TrimPath: "/src/hello=>hello;/work/b002=>",
		TrimRewrites: []TrimRewrite{
			{Old: "/src/hello", New: "hello"},
			{Old: "/work/b002", New: ""},
		},
		Package:        "hello",
		Lang:           "go1.27",
		Complete:       true,
		BuildID:        "n5Xk0d/n5Xk0d",
		GoVersion:      "go1.27.0",
		Concurrency:    4,
		Shared:         true,
		NoLocalImports: true,
		ImportCfgFile:  "/work/b002/importcfg",
		Pack:           true,
		Files:          []string{"/src/hello/main.go"},
	}
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("ParseCompile =\n\t%+v\nwant\n\t%+v", *cfg, want)
	}
}

func TestParseCompileRejects(t *testing.T) {
	tests := []struct {
		name string
		args []string
		flag string
		want string
	}{
		{"dynlink", []string{"-dynlink"}, "dynlink", "out of scope"},
		{"dynlink equals", []string{"-dynlink=true"}, "dynlink", "out of scope"},
		{"race", []string{"-race"}, "race", "instrumentation"},
		{"msan", []string{"-msan"}, "msan", "instrumentation"},
		{"asan", []string{"-asan"}, "asan", "instrumentation"},

		// An unrecognised flag is an error and is never ignored. A flag nanogo
		// skipped would produce an object the build did not ask for.
		{"unknown", []string{"-nosuchflag"}, "nosuchflag", "not recognised"},
		{"unknown with value", []string{"-nosuchflag=1"}, "nosuchflag", "not recognised"},
		{"linkobj", []string{"-linkobj", "x.o"}, "linkobj", "not recognised"},
		{"coveragecfg", []string{"-coveragecfg", "x"}, "coveragecfg", "not recognised"},
		{"pgoprofile", []string{"-pgoprofile", "x"}, "pgoprofile", "not recognised"},
		{"spectre", []string{"-spectre=all"}, "spectre", "not recognised"},

		// A value flag at the end of the line has nothing to take.
		{"missing value", []string{"-p"}, "p", "needs a value"},
		{"missing value o", []string{"-o"}, "o", "needs a value"},

		// A malformed value is an error too.
		{"bad int", []string{"-c=x"}, "c", "not an integer"},
		{"bad bool", []string{"-pack=maybe"}, "pack", "not a boolean"},
		{"bad count", []string{"-m=x"}, "m", "not a count"},
		{"bad trimpath", []string{"-trimpath=nosuchseparator"}, "trimpath", "old=>new"},
		{"empty trimpath old", []string{"-trimpath==>new"}, "trimpath", "old=>new"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCompile(tt.args)
			if err == nil {
				t.Fatalf("ParseCompile(%q) = no error, want one", tt.args)
			}
			var fe *FlagError
			if !errors.As(err, &fe) {
				t.Fatalf("ParseCompile(%q) error is %T, want *FlagError", tt.args, err)
			}
			if fe.Flag != tt.flag {
				t.Errorf("FlagError.Flag = %q, want %q", fe.Flag, tt.flag)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
			if !strings.HasPrefix(err.Error(), "flag -"+tt.flag) {
				t.Errorf("error %q does not name the flag first", err)
			}
		})
	}
}

// TestParseCompileCoversTables asserts that nanogo knows every flag the go
// command sends. It is the drift test of specs/050-driver.md in its cheapest
// form: a flag added to the tables and not to the parser fails here.
func TestParseCompileCoversTables(t *testing.T) {
	always := []string{"o", "p", "importcfg", "lang", "buildid", "goversion",
		"trimpath", "c", "pack", "nolocalimports", "shared"}
	conditional := []string{"complete", "symabis", "asmhdr", "std", "embedcfg", "+"}
	own := []string{"N", "l", "S", "m", "d", "c", "fallback"}

	for _, name := range append(append(always, conditional...), own...) {
		if findFlag(name) == nil {
			t.Errorf("flag -%s is in specs/050-driver.md but not in knownFlags", name)
		}
	}
	for _, name := range []string{"dynlink", "race", "msan", "asan"} {
		if findRejected(name) == "" {
			t.Errorf("flag -%s is in the rejected table but is not rejected", name)
		}
		if findFlag(name) != nil {
			t.Errorf("flag -%s is both known and rejected", name)
		}
	}
}
