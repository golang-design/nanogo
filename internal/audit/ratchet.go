// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package audit

import (
	"bufio"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
)

// A Ratchet is the class every probe held on the day it was recorded.
//
// It gates behaviour, not prose. The question it answers is "does the compiler
// still do what the documentation says it does", which is a property of the
// compiler and not of anybody's wording.
//
// Two things are recorded and they guard different failures:
//
//   - the probe count. A harness that stopped finding probes would otherwise
//     go green having compared fewer of them, and a class map alone cannot
//     see that.
//   - one class per probe, refusals included. internal/gotest deliberately
//     records no refusal, because there a recorded refusal would freeze a gap
//     in place. Here the opposite is true: the previous class is the only
//     thing that makes a refusal turning into an OK detectable, and that
//     moment is exactly when a documented limitation becomes a stale claim.
type Ratchet struct {
	Probes int
	Class  map[string]Class
}

// ratchetHeader explains the file to whoever opens it first. It is written on
// every refresh, so it cannot drift away from the format below it.
const ratchetHeader = `# What the probe corpus proved about nanogo, on the day this was written.
#
# Written by internal/audit. Refresh it with
#
#	NANOGO_REQUIRE_CORPUS=1 NANOGO_REQUIRE_LINK=1 NANOGO_REFRESH_RATCHET=1 \
#		go test -timeout 20m ./internal/audit/
#
# and read the diff before committing. This gate cannot see a refresh made in
# the same commit as the change that moved a probe, so the diff is the review.
#
# Two kinds of line:
#
#	probes N           how many probes the corpus holds
#	probe CLASS NAME   what nanogo did with that probe
#
# The probe lines must add up to N, or the file is unreadable.
#
# Every probe is compiled twice, once by nanogo and once by the Go toolchain,
# and both programs are run and compared. gc is the oracle, so no probe carries
# an expected value that can go stale. The classes are totally ordered:
#
#	ok        nanogo compiled it and the program agreed with gc
#	refused   nanogo would not build it, or the compiler crashed
#	wrong     nanogo compiled it and the program disagreed with gc
#	broken    gc could not build it, so the run had no oracle
#
# ok is best and broken is worst. A probe whose class falls fails the build,
# whatever the two classes are. CONTRIBUTING.md settles the pair that is not
# obvious: "WRONG is the row that matters: a program nanogo compiled into
# something that behaves differently is worse than one it refused." So a
# refusal that starts producing a wrong answer is a regression, not progress.
#
# A probe whose class rises does not fail the build, and is reported loudly.
# That is the point of this file. When a refusal becomes an OK, a sentence in
# README.md, doc.go or driver/help.go has just become false, and it has to be
# corrected in the same change that lifted the refusal.
#
# There is no wrong row. Every program in this corpus that nanogo compiles
# behaves the way the same program compiled by gc behaves. Three rows used to
# sit here and each left in a different way:
#
#	buildinfo-named   ok. nanogo build writes the modinfo line, so
#	                  runtime/debug.ReadBuildInfo answers.
#	embed-directive   refused. nanogo build reads the embed patterns out of
#	                  the go list answer it already reads and will not compile
#	                  a package carrying a go:embed directive. A refusal is not
#	                  the whole fix and is the right resting state until the
#	                  front end exists, because a variable that is silently
#	                  empty is worse than a build that stops.
#	panic-fires       ok. A non-empty interface leads with an *itab and an
#	                  empty one leads with a *_type, and the conversion between
#	                  them was the identity, so an *itab reached the slot the
#	                  runtime reads a descriptor out of. It is a guarded load of
#	                  the descriptor out of the itab now, the way
#	                  cmd/compile's walkConvInterface does it.
#
# That fix passed through a reviewed fall on the way. Refusing the conversion
# took panic-iface from ok to refused for as long as the conversion was unbuilt,
# because panic-iface holds the identical construct and passes nil: it was ok
# because the probe did not reach the bug, not because the program was compiled
# right. Both are ok now.
#
# The go:embed refusal is pinned as an exact string in driver/help_test.go:
# "go:embed". That coupling is deliberate. Changing one
# fails that test, which forces the documentation to be corrected in the same
# change rather than months later.
#
# Sorted, so a refresh produces a diff and not a reshuffle
# (specs/053-determinism.md).
`

// FromReport builds the ratchet a run would record.
func FromReport(r *Report) *Ratchet {
	rt := &Ratchet{Probes: r.Probes(), Class: make(map[string]Class, r.Probes())}
	for _, v := range r.Verdicts {
		rt.Class[v.Probe] = v.Class
	}
	return rt
}

// WriteRatchet writes the file, sorted.
func WriteRatchet(path string, rt *Ratchet) error {
	var b strings.Builder
	b.WriteString(ratchetHeader)
	b.WriteString("\nprobes " + strconv.Itoa(rt.Probes) + "\n\n")
	names := make([]string, 0, len(rt.Class))
	for n := range rt.Class {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString("probe " + string(rt.Class[n]) + " " + n + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// ReadRatchet reads the file.
func ReadRatchet(path string) (*Ratchet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rt := &Ratchet{Class: make(map[string]Class)}
	seen := false
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		where := " on line " + strconv.Itoa(n)
		switch {
		case fields[0] == "probes" && len(fields) == 2:
			if rt.Probes, err = strconv.Atoi(fields[1]); err != nil {
				return nil, errors.New("probes wants a number" + where)
			}
			seen = true
		case fields[0] == "probe" && len(fields) == 3:
			c := Class(fields[1])
			if c.Rank() == 0 && c != ClassBroken {
				return nil, errors.New("unknown class " + strconv.Quote(fields[1]) + where)
			}
			if _, dup := rt.Class[fields[2]]; dup {
				return nil, errors.New("probe " + fields[2] + " is recorded twice" + where)
			}
			rt.Class[fields[2]] = c
		default:
			return nil, errors.New("unreadable ratchet line" + where + ": " + line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !seen {
		return nil, errors.New("the ratchet records no probe count, so it gates no denominator")
	}
	// The count and the lines are two statements of one number, so a file
	// where they disagree cannot be trusted to gate either.
	if len(rt.Class) != rt.Probes {
		return nil, errors.New("the ratchet says " + strconv.Itoa(rt.Probes) +
			" probes and lists " + strconv.Itoa(len(rt.Class)))
	}
	return rt, nil
}

// CountChange reports whether the corpus is a different size than the one that
// was recorded, and returns the empty string when it is not.
//
// A shrunken corpus is the failure this exists for. Deleting a probe directory
// makes every other number in the report smaller, and without this the gate
// would pass having gated less.
func (rt *Ratchet) CountChange(r *Report) string {
	if rt.Probes == r.Probes() {
		return ""
	}
	return "the corpus held " + strconv.Itoa(rt.Probes) + " probes and now holds " +
		strconv.Itoa(r.Probes())
}

// Regressions returns what the run lost against the ratchet, sorted: every
// probe whose class fell, and every probe the ratchet records that the sweep
// did not read at all.
func (rt *Ratchet) Regressions(r *Report) []string {
	now := make(map[string]Verdict, r.Probes())
	for _, v := range r.Verdicts {
		now[v.Probe] = v
	}
	var out []string
	for _, name := range sortedNames(rt.Class) {
		was := rt.Class[name]
		v, ok := now[name]
		if !ok {
			out = append(out, name+": the ratchet records it as "+string(was)+
				" and the sweep did not read it at all")
			continue
		}
		if v.Class.Rank() >= was.Rank() {
			continue
		}
		out = append(out, name+": the ratchet records it as "+string(was)+
			" and it is now "+string(v.Class)+" ("+v.Detail()+")")
	}
	return out
}

// Gains returns the probes whose class rose, and the probes the ratchet does
// not record at all, sorted.
//
// Growth is not a failure. nanogo is expected to compile more of Go every
// week, and a gate that failed on improvement is a gate people route around.
// It is reported loudly instead, because a lifted refusal is the moment a
// capability claim in README.md, doc.go or driver/help.go goes stale.
func (rt *Ratchet) Gains(r *Report) []string {
	var out []string
	for _, v := range r.Verdicts {
		was, ok := rt.Class[v.Probe]
		switch {
		case !ok:
			out = append(out, v.Probe+": not recorded, and it is "+string(v.Class))
		case v.Class.Rank() > was.Rank():
			out = append(out, v.Probe+": recorded as "+string(was)+
				", and it is now "+string(v.Class))
		}
	}
	sort.Strings(out)
	return out
}

func sortedNames(m map[string]Class) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
