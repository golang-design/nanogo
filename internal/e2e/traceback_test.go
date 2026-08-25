// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests here read a traceback, and they read the message as well as the
// names in it.
//
// A traceback that names the right functions is not a traceback that walked
// the stack correctly. When the frame of the function that panicked is read as
// though it had none, the runtime still resolves that function's name from the
// program counter, prints it, and then fails on the frame above with
//
//	runtime: g 1: unexpected return pc for <fn> called from <garbage>
//
// The names are all present in that output. Only the message tells the two
// apart, so the message is what these tests assert.

// TestBuildPanicsFromTheLastBlockOfAFunction is the regression for the frame
// of a function whose body ends in a call that does not return.
//
// boom has one statement and one panic path, so the call to
// runtime.panicdivide is the last instruction of its body. Its return address
// is never executed and it is still read: it is what the unwinder resolves
// boom's frame from. Without a word of padding after the call, that address is
// the first instruction of the stack-growth tail, which declares a stack
// pointer delta of zero because it runs before the frame is pushed, and every
// frame from boom upwards comes out a frame size too low.
//
// main.ratio in build_test.go does not catch this, and the reason is worth
// knowing: it has two divisions, so its two panic calls are adjacent and the
// first one's return address is the second one, still inside the body.
func TestBuildPanicsFromTheLastBlockOfAFunction(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod": "module nanogo.example/lastblock\n\ngo 1.27\n",
		"main.go": `package main

func boom(a, b int) int { return a / b }

func main() {
	zero := 0
	_ = boom(1, zero)
}
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "lastblock", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	b, err := exec.Command(filepath.Join(h.mod, "lastblock")).CombinedOutput()
	if err == nil {
		t.Fatalf("the program exited zero, and it divides by zero:\n%s", b)
	}
	got := string(b)
	if strings.Contains(got, "unexpected return pc") {
		t.Fatalf("the runtime could not walk out of boom's frame:\n%s", got)
	}
	for _, want := range []string{
		"panic: runtime error: integer divide by zero",
		"main.boom()",
		"main.main()",
		"main.go:3",
		"main.go:7",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the output does not contain %q:\n%s", want, got)
		}
	}
}

// TestBuildPanicsInsideAnInit walks out of the function ir.Build synthesises.
//
// The record calls main.init, which calls the declared main.init.0, which
// calls boom. All three frames have to be walkable, and the synthesised one is
// the only function in a nanogo-compiled program that no source line declares.
func TestBuildPanicsInsideAnInit(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod": "module nanogo.example/initpanic\n\ngo 1.27\n",
		"main.go": `package main

func boom(a, b int) int { return a / b }

func init() {
	zero := 0
	_ = boom(1, zero)
}

func main() {}
`,
	}, nil)

	if out, err := h.nanogoBuild(t, "-o", "initpanic", "."); err != nil {
		t.Fatalf("nanogo build .: %v\n%s", err, out)
	}
	b, err := exec.Command(filepath.Join(h.mod, "initpanic")).CombinedOutput()
	if err == nil {
		t.Fatalf("the program exited zero, so the init never ran:\n%s", b)
	}
	got := string(b)
	if strings.Contains(got, "unexpected return pc") {
		t.Fatalf("the runtime could not walk out of a frame in the init chain:\n%s", got)
	}
	for _, want := range []string{
		"panic: runtime error: integer divide by zero",
		"main.boom()",
		"main.init.0()",
		"main.init()",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the output does not contain %q:\n%s", want, got)
		}
	}
}
