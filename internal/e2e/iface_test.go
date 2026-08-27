// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// The dynamic type group of specs/032-type-descriptors-and-itabs.md, run as a
// program.
//
// A unit test reads the comparison a type assertion lowers to and cannot say
// whether the word it compares is the word the runtime wrote. Both sides of
// that comparison are things this compiler produced: the type descriptor the
// object writer emitted, and the interface value ssa.Build built when the
// concrete value went in. A descriptor with the wrong contents still compares
// equal to itself, so only a program that boxes a value here and reads it back
// here says the pair agrees.
//
// The program covers the three shapes: an assertion that succeeds, one that
// fails and is recovered, and a type switch that picks each of its arms. The
// recovered value is read and asserted on, which is the row that makes recover
// return two words rather than none.
//
// Every check divides by zero and the process dies, as in mapProgram: the exit
// status is the assertion.
const ifaceProgram = `package main

//go:noinline
func box(v any) any { return v }

//go:noinline
func class(v any) int {
	switch n := v.(type) {
	case nil:
		return 0
	case int:
		return n
	case string:
		return len(n)
	case *int, float64:
		if n == nil {
			return -1
		}
		return 100
	default:
		return 999
	}
}

// caught runs an assertion that fails and reports what reached the deferred
// function. The value a failed assertion panics with is a
// *runtime.TypeAssertionError, which this compiler cannot assert to yet, so
// the clause reads only that something arrived.
//
//go:noinline
func caught() {
	defer func() {
		if recover() != nil {
			state = 1
		}
	}()
	_ = box(7).(string)
	state = 2
}

// state and carried are package-level because a deferred literal that captures
// a named result is a row of specs/033 this compiler has not built.
var (
	state   int
	carried string
)

// carry panics with a value the program wrote and reads it back out of
// recover, which is the row that makes recover give back two words.
//
//go:noinline
func carry() {
	defer func() {
		r := recover()
		if r == nil {
			carried = "no panic"
			return
		}
		s, ok := r.(string)
		if !ok {
			carried = "not a string"
			return
		}
		carried = s
	}()
	panic("boom")
}

func main() {
	if box(7).(int) != 7 {
		crash()
	}
	if box("hi").(string) != "hi" {
		crash()
	}
	k := 3
	if *box(&k).(*int) != 3 {
		crash()
	}
	if n, ok := box(7).(int); !ok || n != 7 {
		crash()
	}
	if _, ok := box(7).(string); ok {
		crash()
	}
	if s, ok := box("hi").(string); !ok || s != "hi" {
		crash()
	}
	if class(box(nil)) != 0 || class(box(7)) != 7 || class(box("abcd")) != 4 {
		crash()
	}
	if class(box(&k)) != 100 || class(box(1.5)) != 100 || class(box(uint16(1))) != 999 {
		crash()
	}
	caught()
	if state != 1 {
		crash()
	}
	carry()
	if carried != "boom" {
		crash()
	}
}

//go:noinline
func crash() {
	d := 0
	d = d / d
}
`

// TestToolexecAssertsAndSwitchesOnTheDynamicType is the evidence that the
// descriptor this compiler writes is the word the interface value carries.
func TestToolexecAssertsAndSwitchesOnTheDynamicType(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/iface\n\ngo 1.27\n",
		"main.go": ifaceProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "iface", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	runProgram(t, filepath.Join(h.mod, "iface"))
}
