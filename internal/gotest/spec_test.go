// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/internal/gotest"
)

// specPath is the document that owns this package.
const specPath = "../../specs/004-conformance.md"

// The spec states two numbers about the corpus and this is the gate that keeps
// them true.
//
// specs/004-conformance.md deliberately states no counts that move week to
// week: the run prints those and the ratchet records them. The two it does
// state are structural, and they are checked here against the ratchet rather
// than against a sweep, so the gate costs milliseconds and runs on a plain
// `go test`. internal/hygiene makes the same argument at length about the
// numbers in README.md.
func TestTheSpecStatesWhatTheRatchetRecords(t *testing.T) {
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	spec := string(b)

	rt, err := gotest.ReadRatchet(ratchetPath)
	if err != nil {
		t.Fatalf("reading the ratchet: %v", err)
	}

	cases := []struct {
		what    string
		pattern *regexp.Regexp
		want    int
	}{{
		what:    "the size of the corpus",
		pattern: regexp.MustCompile(`the corpus is \*\*([\d,]+)\*\* files`),
		want:    rt.Files,
	}, {
		what:    "how many files pass",
		pattern: regexp.MustCompile(`\*\*([\d,]+)\*\* of them pass`),
		want:    len(rt.Pass),
	}}

	for _, c := range cases {
		m := c.pattern.FindStringSubmatch(spec)
		if m == nil {
			t.Errorf("%s: %s no longer states it in a form this gate can read (%v). "+
				"A number in a document with nothing behind it is what this gate exists to stop.",
				c.what, specPath, c.pattern)
			continue
		}
		got, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
		if err != nil {
			t.Errorf("%s: %q is not a number", c.what, m[1])
			continue
		}
		if got != c.want {
			t.Errorf("%s: %s says %d and %s records %d.\n"+
				"Correct the document, or refresh the ratchet if the corpus really moved.",
				c.what, specPath, got, ratchetPath, c.want)
		}
	}

	// The remainder is stated as prose rather than as a number, and the
	// prose must not become a number by accident.
	if n := rt.Files - len(rt.Pass); !strings.Contains(spec, "The remaining "+strconv.Itoa(n)+" are not failures") {
		t.Errorf("%s no longer says what the other %d files are. "+
			"They are refusals, unimplemented kinds and unhonourable recipes, and a document that "+
			"states a pass count without them reads as a failure count.", specPath, n)
	}
}
