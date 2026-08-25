// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package gotest runs Go's own test corpus against nanogo.
//
// The corpus is [$GOROOT/test], 356 files vendored into testdata/go/test under
// Go's licence. Each file states in its first comment what a compiler must do
// with it: run it and compare its output, reject it at annotated positions,
// or merely build it. This package reads that statement and carries it out
// with nanogo, using the installed gc as the oracle.
//
// Nothing here is a golden file. specs/000-decisions.md decision 6 asks for
// differential execution: build the same source with gc and with nanogo, run
// both, and compare. An expectation derived from gc on the spot cannot go
// stale the way a recorded one can.
//
// [$GOROOT/test]: https://cs.opensource.google/go/go/+/master:test/
package gotest

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
)

// A Header is the execution recipe a corpus file states in its first comment.
//
// The shape follows cmd/internal/testdir, which is the driver the Go
// distribution runs this corpus with. Reproducing its reading of the header is
// the only way a disagreement here means something: a recipe nanogo read
// differently would produce a verdict about a test that was never asked for.
type Header struct {
	// Kind is the first word of the recipe: "run", "errorcheck", "compile"
	// and the rest. It is never empty in a Header ParseHeader returned
	// without error.
	Kind string

	// Flags are the compiler flags the recipe asks for, in order. A recipe
	// with flags is one nanogo cannot honour, because nanogo has no -gcflags
	// of its own; the harness reports such a file as skipped rather than
	// pretending the flags were passed.
	Flags []string

	// Args are what follows the flags. For "run" they are the program's
	// argv, or the names of further source files in the same directory.
	Args []string

	// WantError records whether the recipe expects the compiler to reject
	// the file. errorcheck sets it; the -1 and -0 flags override it.
	WantError bool

	// Timeout is the recipe's -t value in seconds, or zero for none.
	Timeout int

	// Env holds the GOEXPERIMENT and GODEBUG settings the recipe asks the
	// program to run with, as KEY=VALUE.
	Env []string

	// ModVersion is the recipe's -gomodversion value, or "" for none.
	ModVersion string
}

// Recipe errors. A caller distinguishes them because they mean different
// things about the corpus: a missing recipe is a file the harness must count
// somewhere and cannot run, while an unknown one means the corpus grew a kind
// this package has never seen.
var (
	// ErrNoRecipe reports a file whose first comment is not a recipe.
	ErrNoRecipe = errors.New("no execution recipe in the first comment")

	// ErrUnknownKind reports a recipe word this package does not know.
	ErrUnknownKind = errors.New("unknown recipe")
)

// kinds is every recipe word cmd/internal/testdir accepts, mapped to whether
// the recipe expects the compiler to reject the file.
//
// The map is exhaustive on purpose. A corpus that grew a new kind must fail
// loudly here rather than have its files fall out of the totals.
var kinds = map[string]bool{
	"asmcheck":            false,
	"build":               false,
	"builddir":            false,
	"buildrun":            false,
	"buildrundir":         false,
	"compile":             false,
	"compiledir":          false,
	"errorcheck":          true,
	"errorcheckandrundir": false, // no error is expected when it also runs
	"errorcheckdir":       true,
	"errorcheckoutput":    true,
	"errorcheckwithauto":  true,
	"run":                 false,
	"rundir":              false,
	"runindir":            false,
	"runoutput":           false,
	"skip":                false,
}

// ParseHeader reads the execution recipe out of a corpus file.
//
// The recipe is in the first non-empty line that is not a build constraint,
// with the leading "//" removed. That is where cmd/internal/testdir looks, and
// it is why almost every file in the corpus opens with "// run" above its
// copyright notice rather than below it.
func ParseHeader(src []byte) (Header, error) {
	line := recipeLine(string(src))
	if line == "" {
		return Header{}, ErrNoRecipe
	}
	fields, err := splitQuoted(line)
	if err != nil {
		return Header{}, err
	}
	if len(fields) == 0 {
		return Header{}, ErrNoRecipe
	}

	h := Header{Kind: fields[0]}
	wantError, known := kinds[h.Kind]
	if !known {
		return Header{}, ErrUnknownKind
	}
	h.WantError = wantError
	if h.Kind == "errorcheckwithauto" {
		h.Kind = "errorcheck"
	}

	args := fields[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-1":
			h.WantError = true
		case "-0":
			h.WantError = false
		case "-s":
			// singlefilepkgs, which only changes how a directory kind is
			// laid out. Nothing single-file depends on it.
		case "-t":
			if len(args) < 2 {
				return Header{}, errors.New("-t with no number of seconds")
			}
			args = args[1:]
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return Header{}, errors.New("-t wants a number of seconds, got " + strconv.Quote(args[0]))
			}
			h.Timeout = n
		case "-goexperiment":
			if len(args) < 2 {
				return Header{}, errors.New("-goexperiment with no value")
			}
			args = args[1:]
			h.Env = append(h.Env, "GOEXPERIMENT="+args[0])
		case "-godebug":
			if len(args) < 2 {
				return Header{}, errors.New("-godebug with no value")
			}
			args = args[1:]
			h.Env = append(h.Env, "GODEBUG="+args[0])
		case "-gomodversion":
			if len(args) < 2 {
				return Header{}, errors.New("-gomodversion with no value")
			}
			args = args[1:]
			h.ModVersion = args[0]
		default:
			h.Flags = append(h.Flags, args[0])
		}
		args = args[1:]
	}
	h.Args = args
	return h, nil
}

// recipeLine returns the first non-empty line of src that is not a build
// constraint, with "//" and surrounding space removed. It returns "" when
// there is none.
func recipeLine(src string) string {
	for src != "" {
		var line string
		line, src, _ = strings.Cut(src, "\n")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "//") {
			// Code, or a /* comment. Either way the recipe is not here and
			// cannot be further down: testdir stops at the first line that
			// carries anything.
			return ""
		}
		if isConstraint(trimmed) {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
	}
	return ""
}

// isConstraint reports whether a comment line is a build constraint rather
// than a recipe. Both spellings are skipped, because a file can open with
// either and still state its recipe on the next line.
func isConstraint(line string) bool {
	return strings.HasPrefix(line, "//go:build") ||
		strings.HasPrefix(line, "// +build") ||
		strings.HasPrefix(line, "//+build")
}

// splitQuoted splits a recipe into fields, honouring quotes and backslash
// escapes. It is cmd/internal/testdir's splitQuoted, which is in turn
// go/build's, and the corpus does rely on it: several recipes carry a flag
// whose value has a space or a comma inside quotes.
func splitQuoted(s string) ([]string, error) {
	var args []string
	arg := make([]rune, len(s))
	escaped := false
	quoted := false
	quote := '\x00'
	i := 0
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
			continue
		case quote != '\x00':
			if r == quote {
				quote = '\x00'
				continue
			}
		case r == '"' || r == '\'':
			quoted = true
			quote = r
			continue
		case unicode.IsSpace(r):
			if quoted || i > 0 {
				quoted = false
				args = append(args, string(arg[:i]))
				i = 0
			}
			continue
		}
		arg[i] = r
		i++
	}
	if quoted || i > 0 {
		args = append(args, string(arg[:i]))
	}
	if quote != 0 {
		return args, errors.New("unclosed quote in the recipe")
	}
	if escaped {
		return args, errors.New("unfinished escape in the recipe")
	}
	return args, nil
}
