// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

// NoKind is the census key for a file whose recipe could not be read.
//
// The parentheses keep it out of the space of real recipe words, and it holds
// no space of its own because the ratchet's census lines are three fields.
const NoKind = "(none)"

// ByClass counts the verdicts of each class.
func (r *Report) ByClass() map[Class]int {
	n := make(map[Class]int, len(Classes))
	for _, v := range r.Verdicts {
		n[v.Class]++
	}
	return n
}

// ByKind counts the verdicts of each recipe kind. A file with no readable
// recipe is counted under [NoKind], so the kinds add up too.
func (r *Report) ByKind() map[string]int {
	n := make(map[string]int)
	for _, v := range r.Verdicts {
		k := v.Kind
		if k == "" {
			k = NoKind
		}
		n[k]++
	}
	return n
}

// CheckTotals reports an error when the classes do not account for every file.
//
// This is the check the whole package is arranged around. A corpus test whose
// categories do not sum to the file count is producing a number that can only
// rise, because the files it could not handle left the denominator instead of
// entering a category.
func (r *Report) CheckTotals() error {
	sum := 0
	counts := r.ByClass()
	for _, c := range Classes {
		sum += counts[c]
	}
	if len(counts) != countedClasses(counts) {
		return errors.New("a verdict carries a class that is not in Classes, so the report order is incomplete")
	}
	if sum != r.Files {
		return errors.New("the classes account for " + strconv.Itoa(sum) +
			" files and the sweep read " + strconv.Itoa(r.Files))
	}
	if len(r.Verdicts) != r.Files {
		return errors.New("the sweep read " + strconv.Itoa(r.Files) +
			" files and produced " + strconv.Itoa(len(r.Verdicts)) + " verdicts")
	}
	return nil
}

// countedClasses is how many of the classes present in counts are in Classes.
func countedClasses(counts map[Class]int) int {
	n := 0
	for _, c := range Classes {
		if _, ok := counts[c]; ok {
			n++
		}
	}
	return n
}

// Failures are the verdicts that fail the build on their own: the
// miscompilations.
func (r *Report) Failures() []Verdict {
	var out []Verdict
	for _, v := range r.Verdicts {
		if v.Class.IsFailure() {
			out = append(out, v)
		}
	}
	return out
}

// Passes are the file names this run proved, keyed by kind, in sorted order.
func (r *Report) Passes() []Verdict {
	var out []Verdict
	for _, v := range r.Verdicts {
		if v.Class.IsPass() {
			out = append(out, v)
		}
	}
	return out
}

// A Group is one reason and the files that share it.
type Group struct {
	Reason string
	Files  []string
}

// GroupByReason ranks the verdicts of one class by how many files share a
// reason. Ties are broken by the reason text, so the ranking is the same on
// every machine (specs/053-determinism.md).
//
// This is what makes the corpus useful to a contributor: "41 files refused
// because a map literal needs runtime.makemap" is a ranked list of what to
// build next, and a bare count is not.
func (r *Report) GroupByReason(class Class) []Group {
	byReason := make(map[string][]string)
	for _, v := range r.Verdicts {
		if v.Class == class {
			byReason[v.Reason] = append(byReason[v.Reason], v.File)
		}
	}
	groups := make([]Group, 0, len(byReason))
	for reason, files := range byReason {
		sort.Strings(files)
		groups = append(groups, Group{Reason: reason, Files: files})
	}
	sort.Slice(groups, func(i, j int) bool {
		if len(groups[i].Files) != len(groups[j].Files) {
			return len(groups[i].Files) > len(groups[j].Files)
		}
		return groups[i].Reason < groups[j].Reason
	})
	return groups
}

// String renders the report: the class table with its total, then the ranked
// reasons for every class that has any.
func (r *Report) String() string {
	var b strings.Builder
	counts := r.ByClass()
	b.WriteString("corpus: " + strconv.Itoa(r.Files) + " files\n")
	sum := 0
	for _, c := range Classes {
		n := counts[c]
		sum += n
		if n == 0 {
			continue
		}
		b.WriteString("\t" + pad(string(c), 24) + strconv.Itoa(n) + "\n")
	}
	b.WriteString("\t" + pad("TOTAL", 24) + strconv.Itoa(sum) + "\n")

	// Every class but the passes gets its files named. A count with no
	// file names behind it cannot be acted on, and a class left out of
	// this list is a set of files nobody can find.
	for _, c := range []Class{
		ClassMismatched, ClassRefused, ClassCrashed, ClassFalseError,
		ClassTimedOut, ClassMissed, ClassWrongPosition, ClassOracleFailed,
		ClassKindNotImplemented, ClassRecipeNotImplemented,
		ClassRecipeSaysSkip, ClassPlatformExcluded, ClassNoRecipe,
	} {
		groups := r.GroupByReason(c)
		if len(groups) == 0 {
			continue
		}
		b.WriteString("\n" + string(c) + ", by reason:\n")
		for _, g := range groups {
			b.WriteString("\t" + pad(strconv.Itoa(len(g.Files)), 5) + g.Reason + "\n")
			b.WriteString("\t      " + strings.Join(g.Files, " ") + "\n")
		}
	}
	return b.String()
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s + " "
	}
	return s + strings.Repeat(" ", n-len(s))
}
