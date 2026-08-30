// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// specs/020-ir.md's unsafe.Slice and unsafe.String rows, run.
//
// The row is a header plus a length check, and only the check needs a running
// program to be checked at all. A header built without it returns a slice over
// memory the program does not own: the result has the length that was asked
// for, every read of it succeeds, and nothing reports anything. So the cases
// below include the ones that must panic, and the assertion is on the panic
// message and the exit status as well as on the output, compared against the
// same source built by gc.

// unsafeProgram exercises both rows through every case the checks distinguish.
//
// The lengths come from variables. A constant length is folded by the type
// checker, and a constant negative one is rejected by it outright, so a case
// written with constants would say that gc's front end agrees with itself and
// nothing about the code the row emits.
//
// The argument selects the case, because each panic ends the process and the
// panicking cases have to run one at a time.
const unsafeProgram = `package main

import (
	"os"
	"unsafe"
)

var (
	arr  = [8]byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h'}
	wide = [4]int64{10, 20, 30, 40}
	nilb *byte
	nilw *int64
	nile *struct{}
	zero int
	one  int    = 1
	neg  int    = -1
	u8   uint8  = 3
	u64  uint64 = 1 << 63
	i8   int8   = -1
)

func main() {
	switch os.Args[1] {
	case "ok":
		b := unsafe.Slice(&arr[0], 4)
		println(len(b), cap(b), b[0], b[3])
		w := unsafe.Slice(&wide[0], 3)
		println(len(w), cap(w), w[0]+w[1]+w[2])
		s := unsafe.String(&arr[0], 5)
		println(len(s), s)
		u := unsafe.Slice(&arr[0], u8)
		println(len(u), u[2])
	case "nil":
		b := unsafe.Slice(nilb, zero)
		println(b == nil, len(b), cap(b))
		w := unsafe.Slice(nilw, zero)
		println(w == nil, len(w), cap(w))
		s := unsafe.String(nilb, zero)
		println(s == "", len(s))
		e := unsafe.Slice(nile, zero)
		println(e == nil, len(e), cap(e))
		c := unsafe.Slice((*int)(nil), zero)
		println(c == nil, len(c), cap(c))
	case "slicelen":
		println(len(unsafe.Slice(&arr[0], neg)))
	case "slicelen8":
		println(len(unsafe.Slice(&arr[0], i8)))
	case "sliceu64":
		println(len(unsafe.Slice(&wide[0], u64)))
	case "slicenil":
		println(len(unsafe.Slice(nilb, one)))
	case "zerosize":
		println(len(unsafe.Slice(nile, one)))
	case "stringlen":
		println(len(unsafe.String(&arr[0], neg)))
	case "stringnil":
		println(len(unsafe.String(nilb, one)))
	}
}
`

// hexAddress is every address a traceback prints. Two builds of one program
// place their frames differently, so the comparison drops them.
var hexAddress = regexp.MustCompile(`0x[0-9a-f]+`)

// runWith runs a built program with one argument and returns what it wrote and
// the status it exited with.
//
// It is separate from runProgram because half of these cases must not exit
// zero, and runProgram treats that as the program failing to run.
func runWith(t *testing.T, path, arg string) (string, int) {
	t.Helper()
	b, err := exec.Command(path, arg).CombinedOutput()
	code := 0
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("%s %s did not run: %v\n%s", filepath.Base(path), arg, err, b)
	}
	return string(b), code
}

// firstLines keeps the head of a program's output, with addresses removed.
//
// A panic prints the message, a blank line, and then a traceback of the
// goroutines. The message is the part the two compilers must agree on. The
// traceback is a different claim, and build_test.go's traceback case is where
// it is made.
func firstLines(out string, n int) string {
	lines := strings.Split(hexAddress.ReplaceAllString(out, "0x"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestToolexecBuildsUnsafeSliceAndString compares nanogo's build of both rows
// against gc's, case by case.
//
// gc is the oracle rather than a string written into this file, so a wrong
// expectation here is a wrong expectation about Go and not about nanogo. The
// panic messages are asserted by name as well, because a program that agrees
// with gc by panicking for the wrong reason still agrees on the exit status.
func TestToolexecBuildsUnsafeSliceAndString(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/unsafeslice\n\ngo 1.27\n",
		"main.go": unsafeProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "unsafeslice", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package, so this test measures gc:\n%s",
			strings.Join(lines, "\n"))
	}
	ours := filepath.Join(h.mod, "unsafeslice")

	theirs := filepath.Join(h.dir, "gc-unsafeslice")
	cmd := exec.Command(h.goCmd, "build", "-o", theirs, ".")
	cmd.Dir = h.mod
	cmd.Env = env([]string{"GOCACHE=" + h.cache})
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build with the installed compiler: %v\n%s", err, b)
	}

	for _, tc := range []struct {
		arg  string
		want string // the panic the case must raise, empty when it must not
	}{
		{"ok", ""},
		{"nil", ""},
		{"slicelen", "panic: runtime error: unsafe.Slice: len out of range"},
		{"slicelen8", "panic: runtime error: unsafe.Slice: len out of range"},
		{"sliceu64", "panic: runtime error: unsafe.Slice: len out of range"},
		{"slicenil", "panic: runtime error: unsafe.Slice: ptr is nil and len is not zero"},
		// An element of no size has no range to check, so the nil pointer is
		// the only thing wrong with it and the row says so on its own arm.
		{"zerosize", "panic: runtime error: unsafe.Slice: ptr is nil and len is not zero"},
		{"stringlen", "panic: runtime error: unsafe.String: len out of range"},
		{"stringnil", "panic: runtime error: unsafe.String: ptr is nil and len is not zero"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			got, code := runWith(t, ours, tc.arg)
			want, wantCode := runWith(t, theirs, tc.arg)
			if code != wantCode {
				t.Errorf("nanogo's program exited %d and gc's exited %d\nnanogo:\n%s\ngc:\n%s",
					code, wantCode, got, want)
			}
			if g, w := firstLines(got, 3), firstLines(want, 3); g != w {
				t.Errorf("nanogo's program wrote\n%s\nand gc's wrote\n%s", g, w)
			}
			if tc.want == "" {
				if code != 0 {
					t.Errorf("the case exited %d and must not panic:\n%s", code, got)
				}
				return
			}
			if code != 2 {
				t.Errorf("the case exited %d, want 2, which is the status a panic ends in:\n%s", code, got)
			}
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("the panic is not %q:\n%s", tc.want, got)
			}
		})
	}
}
