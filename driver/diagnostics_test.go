// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/syntax"
)

// firstErrorLine returns the line number of the first position in err.
func firstErrorLine(t *testing.T, err error) int {
	t.Helper()
	m := regexp.MustCompile(`a\.go:(\d+):`).FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("no position in the report:\n%s", err)
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		t.Fatal(convErr)
	}
	return n
}

// TestErrorsAreReportedInSourceOrder pins the rule of
// specs/052-diagnostics.md: the first message a user reads is the first
// mistake in the file, not the first one the checker happened to find.
//
// The two label rules discover in the opposite order to the source. types2
// reports a duplicate label where it meets it, and reports an unused label
// only when it has finished the function body, so a duplicate on the last
// line is found before an unused label on the first. Go's own test corpus
// pins the same inversion in test/label.go, and gc sorts for this reason in
// base.FlushErrors.
func TestErrorsAreReportedInSourceOrder(t *testing.T) {
	const src = `package main

func f() {
L1:
	for {
		break
	}
L2:
	for {
		break
	}
L2:
	for {
		break
	}
}

func main() {}
`
	_, err := compileSource(t, src, nil)
	if err == nil {
		t.Fatal("Compile accepted a function with a duplicate label")
	}
	if got := firstErrorLine(t, err); got != 4 {
		t.Errorf("the first error is on line %d, want line 4 (the unused L1)\n%s", got, err)
	}
	// The whole report must be ordered, not only its head.
	var last int
	for _, line := range strings.Split(err.Error(), "\n") {
		m := regexp.MustCompile(`^a\.go:(\d+):`).FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n < last {
			t.Errorf("line %d follows line %d in the report:\n%s", n, last, err)
		}
		last = n
	}
}

// TestDiagnosticsSortUnknownPositionsLast covers the message the compiler
// could not locate. It must not displace a message the user can act on, so it
// sorts to the end however early it was found.
func TestDiagnosticsSortUnknownPositionsLast(t *testing.T) {
	var d diagnostics
	d.add(syntax.NoPos, errors.New("nowhere"))
	d.add(syntax.Pos(20), errors.New("second"))
	d.add(syntax.Pos(10), errors.New("first"))
	got := strings.Split(d.err().Error(), "\n")
	want := []string{"first", "second", "nowhere"}
	if len(got) != len(want) {
		t.Fatalf("report has %d lines, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDiagnosticsApplyTheLimitAfterSorting checks that the cap keeps the ten
// messages the user reads first, not the ten the checker found first.
// Truncating before the sort is the same bug one step earlier.
func TestDiagnosticsApplyTheLimitAfterSorting(t *testing.T) {
	var d diagnostics
	for i := maxReportedErrors * 2; i > 0; i-- {
		d.add(syntax.Pos(i), errors.New("e"+strconv.Itoa(i)))
	}
	got := strings.Split(d.err().Error(), "\n")
	if len(got) != maxReportedErrors {
		t.Fatalf("report has %d lines, want %d", len(got), maxReportedErrors)
	}
	if got[0] != "e1" {
		t.Errorf("the first message is %q, want %q", got[0], "e1")
	}
	if want := "e" + strconv.Itoa(maxReportedErrors); got[len(got)-1] != want {
		t.Errorf("the last message is %q, want %q", got[len(got)-1], want)
	}
}

// TestDiagnosticsAreNilWhenEmpty keeps the caller's "if err != nil" honest.
func TestDiagnosticsAreNilWhenEmpty(t *testing.T) {
	var d diagnostics
	if err := d.err(); err != nil {
		t.Errorf("an empty report returned %v", err)
	}
}
