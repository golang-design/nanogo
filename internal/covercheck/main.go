// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Command covercheck reports per-package statement coverage and fails below
// the gate.
//
// Usage:
//
//	go test -coverprofile=cover.out -coverpkg=./... ./...
//	go run ./internal/covercheck -profile=cover.out
//
// The gate is per package and never one repository average. An average lets a
// well-tested package carry an untested one, and it reports a number nobody
// can act on: "the repository is at 91%" does not name the package to test
// next. A per-package gate names it, and the failure message carries the
// count, so the reader knows how many statements are missing.
//
// A package is below the gate when its percentage is less than -min. Exactly
// -min passes, because a gate that rejects the number it advertises is a gate
// with an undocumented threshold.
//
// A package with no counted statements is reported and not failed. There is
// nothing to test in it, and a zero-statement package that failed would make
// the gate impossible to satisfy for a package that holds only declarations.
//
// A package listed in the exclusions file is reported with its reason and is
// not gated. See exclusions.txt.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is main with its process wiring passed in, so the command is testable in
// process like any other package.
//
// The tool that enforces the gate is gated by it. A tool nobody tests reports
// numbers nobody can trust, and this one does arithmetic over a format it
// parses itself, which is where a silent error lives.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "cover.out", "coverage profile to read")
	exclusionsPath := fs.String("exclusions", filepath.Join("internal", "covercheck", "exclusions.txt"),
		"file of packages that are reported but not gated")
	min := fs.Float64("min", 90.0, "per-package statement coverage gate, in per cent")
	verbose := fs.Bool("v", false, "also report exclusions that match no package in the profile")
	if err := fs.Parse(args); err != nil {
		// A usage error is not a failing gate. CI must be able to tell the two
		// apart, so it gets its own exit code.
		return 2
	}

	f, err := os.Open(*profile)
	if err != nil {
		fmt.Fprintf(stderr, "covercheck: %v\n", err)
		return 1
	}
	defer f.Close()

	blocks, err := parseProfile(f)
	if err != nil {
		fmt.Fprintf(stderr, "covercheck: %v\n", err)
		return 1
	}

	excluded, err := loadExclusions(*exclusionsPath)
	if err != nil {
		fmt.Fprintf(stderr, "covercheck: %v\n", err)
		return 1
	}

	rep := summarize(blocks)
	// A profile with nothing in it is the failure mode this gate cannot
	// afford: it prints an empty table, finds no package below the
	// threshold, and reports a pass having measured nothing. A test run that
	// died, a -coverpkg that matched no package, and a repository with no
	// test file at all all produce it.
	if len(rep.packages) == 0 && len(rep.empty) == 0 {
		fmt.Fprintf(stderr, "covercheck: %s holds no coverage blocks, so nothing was measured\n", *profile)
		return 1
	}
	rep.print(stdout, excluded)

	if *verbose {
		rep.printNotes(stdout, excluded)
	}

	failures := rep.failures(*min, excluded)
	if len(failures) == 0 {
		fmt.Fprintf(stdout, "\nevery gated package is at or above %.0f%%\n", *min)
		return 0
	}
	fmt.Fprintln(stderr)
	for _, msg := range failures {
		fmt.Fprintf(stderr, "::error::%s\n", msg)
	}
	return 1
}

// parseProfile reads a Go coverage profile.
func parseProfile(r io.Reader) ([]block, error) {
	var blocks []block
	s := bufio.NewScanner(r)
	// A generated file produces very long lines, and the default 64KB token
	// limit turns that into a truncated profile that parses as a short one.
	s.Buffer(make([]byte, 0, 64<<10), 4<<20)

	first := true
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if strings.HasPrefix(line, "mode:") {
				continue
			}
			return nil, fmt.Errorf("profile does not start with a mode line")
		}
		b, err := parseBlock(line)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return blocks, nil
}

// block is one statement block from a coverage profile.
type block struct {
	file                string
	startLine, startCol int
	endLine, endCol     int
	statements          int
	count               int
}

// parseBlock reads one "file:startLine.startCol,endLine.endCol stmts count" row.
func parseBlock(line string) (block, error) {
	malformed := func() (block, error) {
		return block{}, fmt.Errorf("malformed profile line %q", line)
	}

	// The last colon, not the first: a Windows profile path can carry a drive
	// letter, and a module path can carry a port.
	colon := strings.LastIndex(line, ":")
	if colon < 0 {
		return malformed()
	}
	file, rest := line[:colon], line[colon+1:]
	if file == "" {
		// A block with no file cannot be attributed to a package, so the
		// summary would invent one keyed on the empty string.
		return block{}, fmt.Errorf("profile line %q names no file", line)
	}

	fields := strings.Fields(rest)
	if len(fields) != 3 {
		return malformed()
	}
	span := strings.Split(fields[0], ",")
	if len(span) != 2 {
		return malformed()
	}
	start, err1 := parsePos(span[0])
	end, err2 := parsePos(span[1])
	statements, err3 := strconv.Atoi(fields[1])
	count, err4 := strconv.Atoi(fields[2])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return malformed()
	}

	// Negative numbers parse and are not counts. A negative statement count
	// subtracts from a package total and reports a percentage above 100 or
	// below zero, which a cancelled test run can produce. This tool gates the
	// build, so it refuses input it cannot compute over.
	if statements < 0 || count < 0 {
		return block{}, fmt.Errorf("profile line %q has a negative count", line)
	}

	return block{
		file: file, startLine: start[0], startCol: start[1],
		endLine: end[0], endCol: end[1],
		statements: statements, count: count,
	}, nil
}

func parsePos(s string) ([2]int, error) {
	lineText, colText, ok := strings.Cut(s, ".")
	if !ok {
		return [2]int{}, fmt.Errorf("bad position %q", s)
	}
	line, err := strconv.Atoi(lineText)
	if err != nil {
		return [2]int{}, err
	}
	col, err := strconv.Atoi(colText)
	if err != nil {
		return [2]int{}, err
	}
	if line <= 0 || col <= 0 {
		return [2]int{}, fmt.Errorf("%q is not a source position: lines and columns are numbered from one", s)
	}
	return [2]int{line, col}, nil
}

// merge folds duplicate blocks together.
//
// -coverpkg=./... makes every test binary report every package, so one
// statement appears once per binary with a different count each time. Summing
// those counts one statement several times, and it reports a statement that
// one binary ran and another did not as both covered and uncovered. A
// statement is covered when any binary ran it, which is the union: the counts
// add and the statement is counted once.
func merge(blocks []block) []block {
	type key struct {
		file                                 string
		startLine, startCol, endLine, endCol int
	}
	index := make(map[key]int, len(blocks))
	out := make([]block, 0, len(blocks))
	for _, b := range blocks {
		k := key{b.file, b.startLine, b.startCol, b.endLine, b.endCol}
		if i, ok := index[k]; ok {
			out[i].count += b.count
			continue
		}
		index[k] = len(out)
		out = append(out, b)
	}
	return out
}

// pkgCoverage is one package's statement coverage.
type pkgCoverage struct {
	pkg     string
	covered int
	total   int
	percent float64
}

// report is every package the profile mentions, in import path order.
type report struct {
	packages []pkgCoverage
	empty    []string // packages whose blocks hold no statements at all
}

// summarize groups blocks by the package their file sits in.
//
// The map is never ranged over into output. Its keys are collected and sorted
// first, because specs/053-determinism.md forbids map order on any path that
// produces output, and a gate whose report reorders between runs is a gate
// whose diffs cannot be read.
func summarize(blocks []block) report {
	type acc struct{ covered, total int }
	byPkg := make(map[string]*acc)

	for _, b := range merge(blocks) {
		pkg := path.Dir(b.file)
		a := byPkg[pkg]
		if a == nil {
			a = &acc{}
			byPkg[pkg] = a
		}
		a.total += b.statements
		if b.count > 0 {
			a.covered += b.statements
		}
	}

	names := make([]string, 0, len(byPkg))
	for name := range byPkg {
		names = append(names, name)
	}
	sort.Strings(names)

	var rep report
	for _, name := range names {
		a := byPkg[name]
		if a.total == 0 {
			// Nothing to cover. A package of declarations alone would fail
			// every gate forever, and there is no test that would fix it.
			rep.empty = append(rep.empty, name)
			continue
		}
		rep.packages = append(rep.packages, pkgCoverage{
			pkg:     name,
			covered: a.covered,
			total:   a.total,
			percent: 100 * float64(a.covered) / float64(a.total),
		})
	}
	return rep
}

// print writes the table. Excluded packages stay in it: an exclusion removes
// the gate, never the number, so the debt is visible on every run.
func (r report) print(w io.Writer, excluded map[string]string) {
	for _, p := range r.packages {
		fmt.Fprintf(w, "%-52s %6.1f%%  %5d/%-5d statements", p.pkg, p.percent, p.covered, p.total)
		if reason, ok := excluded[p.pkg]; ok {
			fmt.Fprintf(w, "  (not gated: %s)", reason)
		}
		fmt.Fprintln(w)
	}
	for _, pkg := range r.empty {
		fmt.Fprintf(w, "%-52s      -   no statements\n", pkg)
	}
}

// printNotes reports the exclusions that matched nothing.
//
// An exclusion whose package is gone, or renamed, or now above the gate, is a
// debt that was paid and never written off. It stays silent otherwise, so -v
// is where it is said.
func (r report) printNotes(w io.Writer, excluded map[string]string) {
	seen := make(map[string]bool, len(r.packages)+len(r.empty))
	for _, p := range r.packages {
		seen[p.pkg] = true
	}
	for _, pkg := range r.empty {
		seen[pkg] = true
	}

	names := make([]string, 0, len(excluded))
	for pkg := range excluded {
		if !seen[pkg] {
			names = append(names, pkg)
		}
	}
	sort.Strings(names)
	for _, pkg := range names {
		fmt.Fprintf(w, "note: %s is excluded and is not in the profile; remove the entry\n", pkg)
	}
}

// failures names every gated package below the threshold.
func (r report) failures(min float64, excluded map[string]string) []string {
	var out []string
	for _, p := range r.packages {
		if _, ok := excluded[p.pkg]; ok {
			continue
		}
		if p.percent < min {
			out = append(out, fmt.Sprintf("%s is at %.1f%% (%d/%d statements), below the %.0f%% gate",
				p.pkg, p.percent, p.covered, p.total, min))
		}
	}
	return out
}

// loadExclusions reads the exclusions file.
//
// Each entry is a package import path and a mandatory reason:
//
//	golang.design/x/nanogo/internal/thing  # reason the gate is off here
//
// The reason is mandatory because an exclusion is a debt with a name on it. An
// entry with no reason is rejected rather than warned about: a warning in a
// green build is not read, and the entry would then be permanent.
//
// A missing file is not an error. An exclusion can only weaken the gate, so a
// path that does not resolve leaves the gate at full strength, which fails
// loudly rather than passing quietly.
func loadExclusions(file string) (map[string]string, error) {
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	s := bufio.NewScanner(f)
	for n := 1; s.Scan(); n++ {
		line := strings.TrimSpace(s.Text())
		// A line whose first character is "#" is a comment, so an entry
		// always has its package before the reason.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pkg, reason, ok := strings.Cut(line, "#")
		pkg, reason = strings.TrimSpace(pkg), strings.TrimSpace(reason)
		if !ok || reason == "" {
			return nil, fmt.Errorf("%s:%d: %q has no reason; write \"%s # why\"", file, n, pkg, pkg)
		}
		out[pkg] = reason
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
