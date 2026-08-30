// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The ABIInternal wrapper of specs/047-abi-wrappers.md stage 2, run as a
// program.
//
// A bodyless Go declaration that an assembly file defines under ABI0 is two
// symbols. The assembly defines one, under ABI0, and the compiler owes the
// other, under ABIInternal, because a Go call names that one. The wrapper
// takes its arguments where a Go caller puts them, writes them into the
// outgoing area at the ABI0 offsets, calls the assembly and reads the results
// back.
//
// Every failure this can have is silent inside one toolchain, which is why the
// package the wrapper is in is compiled by nanogo and the package that calls
// it is compiled by gc. A wrapper that laid its outgoing area out with the
// register sets of ABIInternal would pass every argument in a register the
// assembly never reads, and the assembly would return whatever was in the
// words it did read. The program links and prints an answer either way.

// abiWrapperAsm defines each declaration under ABI0.
//
// The frame sizes are the ABI0 areas of specs/047-abi-wrappers.md's
// recurrence, worked through per function. A wrong one is caught by the
// assembler, which is part of why they are spelled out.
const abiWrapperAsm = `#include "textflag.h"

// func Zero() uint64
// The area is the result alone: 8 bytes.
TEXT ·Zero(SB), NOSPLIT, $0-8
	MOVD	$4242, R0
	MOVD	R0, ret+0(FP)
	RET

// func AddOne(a int64) int64
// a at 0, the result at 8, so 16 bytes.
TEXT ·AddOne(SB), NOSPLIT, $0-16
	MOVD	a+0(FP), R0
	ADD	$1, R0, R0
	MOVD	R0, ret+8(FP)
	RET

// func Narrow(a int8) (r int8)
// a at 0 and r at 8, not at 1: the pointer-alignment field between the
// arguments and the results is 8 bytes wide whatever the arguments hold.
TEXT ·Narrow(SB), NOSPLIT, $0-16
	MOVB	a+0(FP), R0
	ADD	$3, R0, R0
	MOVB	R0, r+8(FP)
	RET

// func Two(a int8, b int8) (r1 int8, r2 int32)
// a at 0, b at 1, r1 at 8, r2 at 12, so 16 bytes.
TEXT ·Two(SB), NOSPLIT, $0-16
	MOVB	a+0(FP), R0
	MOVB	b+1(FP), R1
	ADD	R1, R0, R2
	MOVB	R2, r1+8(FP)
	MOVW	R0, r2+12(FP)
	RET

// func Mix(a int8, b int64, c string, d float64) (r1 int32, r2 int64)
// a at 0, b at 8, the string at 16 and 24, d at 32, r1 at 40, r2 at 48,
// so 56 bytes. This is the row of the spec's table that exercises every
// clause of the recurrence.
TEXT ·Mix(SB), NOSPLIT, $0-56
	MOVB	a+0(FP), R0
	MOVD	b+8(FP), R1
	MOVD	c_len+24(FP), R2
	FMOVD	d+32(FP), F0
	FCVTZSD	F0, R3
	ADD	R1, R0, R4
	ADD	R2, R4, R4
	ADD	R3, R4, R4
	MOVW	R4, r1+40(FP)
	MOVD	R4, r2+48(FP)
	RET

// func FirstByte(b []byte) byte
// The slice at 0, 8 and 16, the result at 24, so 32 bytes. The pointer is
// the word the args_stackmap has to mark.
TEXT ·FirstByte(SB), NOSPLIT, $0-32
	MOVD	b_base+0(FP), R0
	MOVBU	(R0), R1
	MOVB	R1, ret+24(FP)
	RET

// func Tail(s string) string
// The argument at 0 and 8, the result at 16 and 24, so 32 bytes. Both
// bitmaps of the map have a pointer in them, and the second is the
// cumulative one.
TEXT ·Tail(SB), NOSPLIT, $0-32
	MOVD	s_base+0(FP), R0
	MOVD	s_len+8(FP), R1
	ADD	$1, R0, R0
	SUB	$1, R1, R1
	MOVD	R0, ret_base+16(FP)
	MOVD	R1, ret_len+24(FP)
	RET
`

// abiWrapperPkg declares what the assembly defines, and nothing else.
//
// Each declaration is bodyless, so nanogo compiles no code for it. What it
// compiles is the wrapper, and what it writes beside the wrapper is the
// argument map the assembler's own FUNCDATA reference names.
const abiWrapperPkg = `package asmpkg

func Zero() uint64

func AddOne(a int64) int64

func Narrow(a int8) (r int8)

func Two(a int8, b int8) (r1 int8, r2 int32)

func Mix(a int8, b int64, c string, d float64) (r1 int32, r2 int64)

func FirstByte(b []byte) byte

func Tail(s string) string
`

// abiWrapperMain calls each one and prints what came back.
//
// It is compiled by gc, because the package under test is the one nanogo
// compiles and a caller from the same compiler would agree with a wrong
// wrapper as readily as with a right one.
const abiWrapperMain = `package main

import (
	"fmt"

	"nanogo.example/abiwrapper/asmpkg"
)

func main() {
	fmt.Println("zero", asmpkg.Zero())
	fmt.Println("addone", asmpkg.AddOne(41), asmpkg.AddOne(-1))
	fmt.Println("narrow", asmpkg.Narrow(4), asmpkg.Narrow(-3))
	r1, r2 := asmpkg.Two(5, 6)
	fmt.Println("two", r1, r2)
	m1, m2 := asmpkg.Mix(1, 2, "abc", 4.0)
	fmt.Println("mix", m1, m2)
	b := []byte("hello")
	fmt.Println("firstbyte", asmpkg.FirstByte(b))
	fmt.Println("tail", asmpkg.Tail("gopher"))

	// The same calls behind a func value, so the wrapper is reached through
	// its address as well as through a direct call.
	f := asmpkg.AddOne
	fmt.Println("indirect", f(100))

	// A call in a loop that allocates, so the collector runs while the
	// wrapper's frame and the assembly's frame are on the stack.
	total := 0
	for i := 0; i < 20000; i++ {
		s := fmt.Sprint(i)
		total += len(asmpkg.Tail(s))
		total += int(asmpkg.FirstByte([]byte(s)))
	}
	fmt.Println("under load", total)
}
`

// TestGcAndNanogoAgreeOnAnABIInternalWrapper is the evidence for stage 2.
//
// nanogo compiles the package the assembly is in and gc compiles the caller,
// so the two toolchains have to agree on where the arguments of an ABI0
// function are. The program's output is compared against an all-gc build of
// the same module, which is the only comparison a wrong placement cannot pass:
// a nanogo-only program is self-consistent whatever the offsets are.
func TestGcAndNanogoAgreeOnAnABIInternalWrapper(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":             "module nanogo.example/abiwrapper\n\ngo 1.27\n",
		"main.go":            abiWrapperMain,
		"asmpkg/asmpkg.go":   abiWrapperPkg,
		"asmpkg/asm_arm64.s": abiWrapperAsm,
	}, []string{"nanogo.example/abiwrapper/asmpkg"})

	if out, err := h.build(t, "-o", "abiwrapper", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/abiwrapper/asmpkg") {
		t.Fatalf("nanogo delegated the package with the assembly in it:\n%s", strings.Join(lines, "\n"))
	}
	if compiled(lines, "main") {
		t.Fatalf("nanogo compiled the caller as well, so the comparison is not against gc:\n%s",
			strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "abiwrapper"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// TestABIWrapperObjectHoldsBothSymbols reads what nanogo wrote.
//
// The program above says the answers agree. This says why they can: the
// archive holds the wrapper under ABIInternal, the argument map the
// assembler's FUNCDATA reference names, and the argument info beside it. A
// build that produced the right answer with a missing map would keep producing
// it until a collection ran over the assembly's frame.
func TestABIWrapperObjectHoldsBothSymbols(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":             "module nanogo.example/abiwrapper\n\ngo 1.27\n",
		"main.go":            abiWrapperMain,
		"asmpkg/asmpkg.go":   abiWrapperPkg,
		"asmpkg/asm_arm64.s": abiWrapperAsm,
	}, []string{"nanogo.example/abiwrapper/asmpkg"})

	// Only the package with the assembly in it, so the work tree holds one
	// archive that names its symbols and the caller's archive cannot be
	// mistaken for it.
	//
	// -work is what keeps the archive on disk, and it is also what makes the
	// build leave a directory behind. The go command puts that directory under
	// GOTMPDIR, so this build is given one the framework owns and removes: a
	// process killed on a deadline runs no deferred cleanup, and CI fails on a
	// temporary directory the suite leaks.
	tmp := t.TempDir()
	cmd := exec.Command(h.goCmd, "build", "-toolexec="+h.bin, "-work", "./asmpkg")
	cmd.Dir = h.mod
	cmd.Env = env([]string{
		"NANOGO_ALLOWLIST=" + h.list,
		"NANOGO_LOG=" + h.log,
		"GOCACHE=" + h.cache,
		"GOTMPDIR=" + tmp,
		"TMPDIR=" + tmp,
	})
	b, err := cmd.CombinedOutput()
	out := string(b)
	if err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	work := ""
	for _, line := range strings.Split(out, "\n") {
		if w, ok := strings.CutPrefix(strings.TrimSpace(line), "WORK="); ok {
			work = w
		}
	}
	if work == "" {
		t.Fatalf("go build -work printed no work directory:\n%s", out)
	}

	archive := findArchive(t, work, "asmpkg")
	nm, err := exec.Command(h.goCmd, "tool", "nm", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm %s: %v\n%s", archive, err, nm)
	}
	syms := string(nm)
	for _, want := range []string{
		"nanogo.example/abiwrapper/asmpkg.Zero.args_stackmap",
		"nanogo.example/abiwrapper/asmpkg.Zero.arginfo0",
		"nanogo.example/abiwrapper/asmpkg.Tail.args_stackmap",
		"nanogo.example/abiwrapper/asmpkg.FirstByte.args_stackmap",
	} {
		if !strings.Contains(syms, want) {
			t.Errorf("the archive %s holds no %s", archive, want)
		}
	}
	// Two definitions of one name, one per convention: the wrapper nanogo
	// wrote and the assembly the assembler wrote. go tool nm prints a T for
	// each.
	texts := 0
	for _, line := range strings.Split(syms, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[len(f)-2] == "T" && f[len(f)-1] == "nanogo.example/abiwrapper/asmpkg.Zero" {
			texts++
		}
	}
	if texts != 2 {
		t.Errorf("the archive holds %d definitions of asmpkg.Zero, want 2, one per ABI", texts)
	}
}

// findArchive locates the compiled archive of a package in the work tree.
func findArchive(t *testing.T, work, pkg string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(work, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return err
		}
		if filepath.Base(path) != "_pkg_.a" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), pkg+".Zero.args_stackmap") || strings.Contains(string(b), pkg+".Zero") {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("no archive for %s under %s", pkg, work)
	}
	return found
}
