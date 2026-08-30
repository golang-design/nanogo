// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package selfhost

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// A Ratchet is which packages nanogo compiled on the day it was recorded.
//
// It exists because a package that compiled and no longer does is a
// regression that nothing else in this repository can see. The corpus ratchet
// of internal/gotest watches Go's own test files and says nothing about
// nanogo's own source, and the unit tests of each package test the compiler
// rather than what the compiler does to itself. A refusal added anywhere in
// the pipeline can take a package out of this set and every other gate stays
// green.
//
// That is not hypothetical. The wrapper generator refused a variadic method
// for a reason that turned out to be a misreading, and it took
// golang.design/x/nanogo/syntax out of this set. Nothing failed. It was found
// by running the measurement by hand, days later.
//
// Two things are recorded, and they guard different failures:
//
//   - the package count, so a list that stopped naming packages cannot go
//     green with a smaller denominator.
//   - the compiled set, one line each. A package that compiled yesterday and
//     does not today fails the build.
//
// A refusal is never recorded. Recording one freezes a gap in place and calls
// it progress, which is the rule internal/gotest's ratchet states and the
// same rule applies here. Growth does not fail: nanogo is expected to compile
// more of its own source every week, and a gate that failed on improvement is
// a gate people route around.
type Ratchet struct {
	Packages int             // how many packages the list held
	Compiles map[string]bool // the packages nanogo compiled
}

// ratchetHeader explains the file to whoever opens it first. It is written on
// every refresh, so it cannot drift away from the format below it.
const ratchetHeader = `# Which packages nanogo compiled, on the day this was written.
#
# Written by internal/selfhost. Refresh it with
#
#	NANOGO_REQUIRE_CORPUS=1 NANOGO_REFRESH_RATCHET=1 go test ./internal/selfhost/
#
# and read the diff before committing. A line that disappeared is a package
# that used to compile and no longer does, which is a regression and not a
# refresh.
#
# Two kinds of line:
#
#	packages N     how many packages were measured
#	compiles PATH  a package nanogo compiled on its own
#
# "On its own" is the whole method. Each package is built with an allowlist
# naming that package alone, against dependencies gc built, because a
# whole-tree build stops at the first failure and reports nothing about the
# rest. A compile means nanogo produced an archive the go command accepted. It
# does not mean the archive is correct, and nothing here is linked or run.
#
# A refusal is never recorded: recording one would freeze a gap in place and
# call it progress. Read specs/060-selfhost.md for what this measures and what
# it does not.
#
# Sorted, so a refresh produces a diff and not a reshuffle
# (specs/053-determinism.md).
`

// FromResults is the ratchet a measurement would record.
func FromResults(rs []Result) *Ratchet {
	rt := &Ratchet{Packages: len(rs), Compiles: map[string]bool{}}
	for _, r := range rs {
		if r.Decision == Compiled {
			rt.Compiles[r.Path] = true
		}
	}
	return rt
}

// WriteRatchet records rt at path.
func WriteRatchet(path string, rt *Ratchet) error {
	var b strings.Builder
	b.WriteString(ratchetHeader)
	fmt.Fprintf(&b, "\npackages %d\n\n", rt.Packages)
	pkgs := make([]string, 0, len(rt.Compiles))
	for p := range rt.Compiles {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		fmt.Fprintf(&b, "compiles %s\n", p)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ReadRatchet reads a recorded ratchet.
//
// A line it cannot read is an error rather than a skip. A ratchet that
// silently dropped the lines it did not understand would report no
// regressions for the packages it dropped, which is the failure this file
// exists to prevent, arriving through the file itself.
func ReadRatchet(path string) (*Ratchet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rt := &Ratchet{Compiles: map[string]bool{}}
	s := bufio.NewScanner(f)
	for line := 1; s.Scan(); line++ {
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		kind, rest, ok := strings.Cut(text, " ")
		if !ok {
			return nil, fmt.Errorf("%s:%d: %q is not a ratchet line", path, line, text)
		}
		switch kind {
		case "packages":
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return nil, fmt.Errorf("%s:%d: the package count %q is not a number", path, line, rest)
			}
			rt.Packages = n
		case "compiles":
			rt.Compiles[strings.TrimSpace(rest)] = true
		default:
			return nil, fmt.Errorf("%s:%d: %q is not a kind of ratchet line", path, line, kind)
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if rt.Packages == 0 {
		return nil, fmt.Errorf("%s: no package count, so a run that measured nothing would agree with it", path)
	}
	return rt, nil
}

// Regressions are the packages the ratchet says compiled and this run did
// not, with what nanogo said instead.
func (rt *Ratchet) Regressions(rs []Result) []string {
	got := map[string]Result{}
	for _, r := range rs {
		got[r.Path] = r
	}
	var out []string
	for p := range rt.Compiles {
		r, measured := got[p]
		switch {
		case !measured:
			out = append(out, fmt.Sprintf("%s compiled and was not measured this run", p))
		case r.Decision == Compiled:
		case r.Reason != "":
			out = append(out, fmt.Sprintf("%s compiled and now %s: %s", p, r.Decision, r.Reason))
		default:
			out = append(out, fmt.Sprintf("%s compiled and now %s", p, r.Decision))
		}
	}
	sort.Strings(out)
	return out
}

// Gains are the packages this run compiled that the ratchet does not record.
func (rt *Ratchet) Gains(rs []Result) []string {
	var out []string
	for _, r := range rs {
		if r.Decision == Compiled && !rt.Compiles[r.Path] {
			out = append(out, r.Path)
		}
	}
	sort.Strings(out)
	return out
}

// CountChanged reports the recorded and measured package counts when they
// disagree.
//
// Every number read against this ratchet has the package count for a
// denominator, so a denominator that moved makes all of them suspect. It is
// reported on its own rather than folded into the regressions.
func (rt *Ratchet) CountChanged(rs []Result) (recorded, measured int, changed bool) {
	return rt.Packages, len(rs), rt.Packages != len(rs)
}
