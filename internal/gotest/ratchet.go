// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"bufio"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
)

// A Ratchet is what the corpus proved on the day it was recorded.
//
// Two things are recorded and they guard different failures:
//
//   - the census, one count per recipe kind. A harness change that stops
//     finding files would otherwise go green with a smaller denominator, and
//     a pass set alone cannot see that.
//   - the pass set, one line per file the corpus proved. A file that passed
//     yesterday and does not today is a regression and fails the build.
//
// Growth is not a failure. nanogo is expected to compile more of Go every
// week, and a gate that failed on improvement is a gate people route around.
// A run that finds new passes says so and prints the refresh command.
type Ratchet struct {
	Files  int            // how many files the corpus held
	Census map[string]int // recipe kind to file count
	Pass   map[string]string
}

// ratchetHeader explains the file to whoever opens it first. It is written on
// every refresh, so it cannot drift away from the format below it.
const ratchetHeader = `# What Go's own test corpus proved about nanogo, on the day this was written.
#
# Written by internal/gotest. Refresh it with
#
#	NANOGO_REQUIRE_CORPUS=1 NANOGO_REQUIRE_LINK=1 NANOGO_REFRESH_RATCHET=1 \
#		go test ./internal/gotest/
#
# and read the diff before committing. A line that disappeared is a file that
# used to pass and no longer does, which is a regression and not a refresh.
#
# Three kinds of line:
#
#	files N            how many files the corpus holds
#	census KIND N      how many files carry that recipe kind
#	pass HOW FILE      a file nanogo did what the recipe asked with,
#	                   and how: matched, compiled or rejected
#
# A pass means nanogo ran the program and it behaved as gc's build did, or
# nanogo compiled what the recipe said to compile, or nanogo rejected what the
# recipe said to reject, at the annotated line. A refusal is never recorded:
# recording one would freeze a gap in place and call it progress.
#
# Sorted, so a refresh produces a diff and not a reshuffle
# (specs/053-determinism.md).
`

// FromReport builds the ratchet a run would record.
func FromReport(r *Report) *Ratchet {
	rt := &Ratchet{Files: r.Files, Census: r.ByKind(), Pass: make(map[string]string)}
	for _, v := range r.Passes() {
		rt.Pass[v.File] = string(v.Class)
	}
	return rt
}

// WriteRatchet writes the file, sorted.
func WriteRatchet(path string, rt *Ratchet) error {
	var b strings.Builder
	b.WriteString(ratchetHeader)
	b.WriteString("\nfiles " + strconv.Itoa(rt.Files) + "\n\n")
	for _, k := range sortedKeys(rt.Census) {
		b.WriteString("census " + k + " " + strconv.Itoa(rt.Census[k]) + "\n")
	}
	b.WriteString("\n")
	for _, f := range sortedKeys(rt.Pass) {
		b.WriteString("pass " + rt.Pass[f] + " " + f + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReadRatchet reads the file.
func ReadRatchet(path string) (*Ratchet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rt := &Ratchet{Census: make(map[string]int), Pass: make(map[string]string)}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		where := " on line " + strconv.Itoa(n)
		switch {
		case fields[0] == "files" && len(fields) == 2:
			if rt.Files, err = strconv.Atoi(fields[1]); err != nil {
				return nil, errors.New("files wants a number" + where)
			}
		case fields[0] == "census" && len(fields) == 3:
			c, err := strconv.Atoi(fields[2])
			if err != nil {
				return nil, errors.New("census wants a number" + where)
			}
			rt.Census[fields[1]] = c
		case fields[0] == "pass" && len(fields) == 3:
			rt.Pass[fields[2]] = fields[1]
		default:
			return nil, errors.New("unreadable ratchet line" + where + ": " + line)
		}
	}
	return rt, sc.Err()
}

// Regressions returns what the run lost against the ratchet: files the ratchet
// records as passing that this run did not prove, each with what happened to
// it instead.
//
// A change of class between two passing kinds is a regression too. A file the
// ratchet records as "matched" that now only "compiled" stopped being run, and
// that is a claim quietly withdrawn.
func (rt *Ratchet) Regressions(r *Report) []string {
	now := make(map[string]Verdict, len(r.Verdicts))
	for _, v := range r.Verdicts {
		now[v.File] = v
	}
	var out []string
	for _, file := range sortedKeys(rt.Pass) {
		was := rt.Pass[file]
		v, ok := now[file]
		if !ok {
			out = append(out, file+": the ratchet records it as "+was+" and the sweep did not read it at all")
			continue
		}
		if string(v.Class) == was {
			continue
		}
		if v.Class == ClassOracleFailed {
			// gc could not build or run the file, so this sweep has no
			// expectation to compare nanogo against and made no measurement of
			// it. The class says so in its own words, and a file that was not
			// measured is not a file that stopped passing.
			//
			// It is not hypothetical and it is not nanogo's doing either: the
			// oracle builds the original file with the go command, and under
			// the coverage pass, which runs every package of this repository
			// at once, linkmain_run.go's build ran past its budget. Reporting
			// that as a regression puts a red gate on machine load.
			continue
		}
		msg := file + ": the ratchet records it as " + was + " and it is now " + string(v.Class)
		if v.Reason != "" {
			msg += " (" + v.Reason + ")"
		}
		out = append(out, msg)
	}
	return out
}

// Gains returns the files this run proved that the ratchet does not record.
func (rt *Ratchet) Gains(r *Report) []string {
	var out []string
	for _, v := range r.Passes() {
		if _, ok := rt.Pass[v.File]; !ok {
			out = append(out, v.File+" ("+string(v.Class)+")")
		}
	}
	sort.Strings(out)
	return out
}

// CensusChanges returns the differences between the recorded census and this
// run's, in sorted order.
//
// A change here is not about nanogo at all. It means the corpus moved, or the
// header reader did, and every other number in the report must be re-read
// before it is believed.
func (rt *Ratchet) CensusChanges(r *Report) []string {
	var out []string
	if rt.Files != r.Files {
		out = append(out, "the corpus held "+strconv.Itoa(rt.Files)+" files and now holds "+strconv.Itoa(r.Files))
	}
	now := r.ByKind()
	seen := make(map[string]bool)
	for _, k := range sortedKeys(rt.Census) {
		seen[k] = true
		if now[k] != rt.Census[k] {
			out = append(out, k+": recorded "+strconv.Itoa(rt.Census[k])+", found "+strconv.Itoa(now[k]))
		}
	}
	for _, k := range sortedKeys(now) {
		if !seen[k] {
			out = append(out, k+": not recorded, found "+strconv.Itoa(now[k]))
		}
	}
	return out
}
