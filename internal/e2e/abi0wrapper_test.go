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

// The ABI0 wrapper of specs/047-abi-wrappers.md stage 3, run as a program.
//
// This is the other direction of abiwrapper_test.go. There a Go caller called
// an assembly definition; here an assembly caller calls a Go definition. The
// Go function is ABIInternal, an assembly file names it under ABI0, and the
// compiler owes the ABI0 half of the pair: a text symbol of the same name
// whose own convention is stack-only, which takes every argument out of its
// incoming argument area, calls the Go function with the arguments in
// registers, and writes the results back into that area.
//
// Every offset in the assembly below was read out of go tool compile -S over
// the same signature compiled by gc, from the ABI0 wrapper gc writes for it.
// None of them is computed here from a rule, because a remembered ABI rule has
// cost this project a silent miscompile once already.
//
// The identity the frames rest on is specs/047's: the callee's ABI0 offset 0
// is at 8(RSP) of the caller, because the arm64 CALL does not move RSP and the
// callee's prologue subtracts its own frame. So an outgoing offset here is
// eight more than the ABI0 offset it writes.

// abi0Asm calls each Go function under ABI0 and hands the answer back.
//
// Each TEXT is itself an ABI0 definition, so nanogo owes it the ABIInternal
// wrapper of stage 2 as well, and a gc caller reaches it through that. One
// call therefore crosses the boundary four times: gc, the stage 2 wrapper, the
// assembly, the stage 3 wrapper, the Go function.
//
// The frame sizes are the ABI0 areas of the callees, raised to the next value
// that keeps the stack pointer sixteen-byte aligned. The assembler checks
// them, which is part of why they are spelled out.
const abi0Asm = `#include "textflag.h"

// func CallZero() uint64
// sZero's area is the result alone at 0, so 8 bytes.
TEXT ·CallZero(SB), NOSPLIT, $8-8
	CALL	·sZero(SB)
	MOVD	8(RSP), R0
	MOVD	R0, ret+0(FP)
	RET

// func CallNarrow(a int8) (r int8)
// sNarrow places a at 0 and r at 8, not at 1: the pointer-alignment field
// between the arguments and the results is a whole word whatever the
// arguments hold. The area is 16.
TEXT ·CallNarrow(SB), NOSPLIT, $24-16
	MOVB	a+0(FP), R0
	MOVB	R0, 8(RSP)
	CALL	·sNarrow(SB)
	MOVB	16(RSP), R0
	MOVB	R0, r+8(FP)
	RET

// func CallTwo(a int8, b int8) (r1 int8, r2 int32)
// sTwo places a at 0, b at 1, r1 at 8 and r2 at 12, so the area is 16.
TEXT ·CallTwo(SB), NOSPLIT, $24-16
	MOVB	a+0(FP), R0
	MOVB	b+1(FP), R1
	MOVB	R0, 8(RSP)
	MOVB	R1, 9(RSP)
	CALL	·sTwo(SB)
	MOVB	16(RSP), R0
	MOVW	20(RSP), R1
	MOVB	R0, r1+8(FP)
	MOVW	R1, r2+12(FP)
	RET

// func CallMix(a int8, b int64, c string, d float64) (r1 int32, r2 int64)
// sMix places a at 0, b at 8, the string at 16 and 24, d at 32, r1 at 40 and
// r2 at 48, so the area is 56. This is the row of specs/047's table that
// exercises every clause of the recurrence, and the float is what proves the
// wrapper reloads a value of the other register class.
TEXT ·CallMix(SB), NOSPLIT, $56-56
	MOVB	a+0(FP), R0
	MOVD	b+8(FP), R1
	MOVD	c_base+16(FP), R2
	MOVD	c_len+24(FP), R3
	FMOVD	d+32(FP), F0
	MOVB	R0, 8(RSP)
	MOVD	R1, 16(RSP)
	MOVD	R2, 24(RSP)
	MOVD	R3, 32(RSP)
	FMOVD	F0, 40(RSP)
	CALL	·sMix(SB)
	MOVW	48(RSP), R0
	MOVD	56(RSP), R1
	MOVW	R0, r1+40(FP)
	MOVD	R1, r2+48(FP)
	RET

// func CallFirstByte(b []byte) byte
// sFirstByte places the slice at 0, 8 and 16 and the result at 24, so the
// area is 32. The base pointer at 0 is a word the wrapper's own arguments
// bitmap has to mark.
TEXT ·CallFirstByte(SB), NOSPLIT, $40-32
	MOVD	b_base+0(FP), R0
	MOVD	b_len+8(FP), R1
	MOVD	b_cap+16(FP), R2
	MOVD	R0, 8(RSP)
	MOVD	R1, 16(RSP)
	MOVD	R2, 24(RSP)
	CALL	·sFirstByte(SB)
	MOVBU	32(RSP), R0
	MOVB	R0, ret+24(FP)
	RET

// func CallTail(s string) string
// sTail places the argument at 0 and 8 and the result at 16 and 24, so the
// area is 32. Two of those four words are pointers.
TEXT ·CallTail(SB), NOSPLIT, $40-32
	MOVD	s_base+0(FP), R0
	MOVD	s_len+8(FP), R1
	MOVD	R0, 8(RSP)
	MOVD	R1, 16(RSP)
	CALL	·sTail(SB)
	MOVD	24(RSP), R0
	MOVD	32(RSP), R1
	MOVD	R0, ret_base+16(FP)
	MOVD	R1, ret_len+24(FP)
	RET

// func CallZeroSize(a [3]int8, b [0]int64, c [3]int8) (r int8)
// sZeroSize places a at 0, c at 8 and the result at 16, so the area is 24.
// The zero-size [0]int64 between the two arrays takes no space and forces the
// running offset to the next multiple of eight, which moves c from 3 to 8 and
// the result from 4 to 16. A walk that skipped it would place every value
// after it wrongly, and nothing inside one toolchain would say so.
TEXT ·CallZeroSize(SB), NOSPLIT, $24-24
	MOVB	a+0(FP), R0
	MOVB	a+1(FP), R1
	MOVB	a+2(FP), R2
	MOVB	R0, 8(RSP)
	MOVB	R1, 9(RSP)
	MOVB	R2, 10(RSP)
	MOVB	c+8(FP), R0
	MOVB	c+9(FP), R1
	MOVB	c+10(FP), R2
	MOVB	R0, 16(RSP)
	MOVB	R1, 17(RSP)
	MOVB	R2, 18(RSP)
	CALL	·sZeroSize(SB)
	MOVB	24(RSP), R0
	MOVB	R0, r+16(FP)
	RET

// func CallPtrThrough(p *int, s string) (*int, string)
// sPtrThrough places p at 0, the string at 8 and 16, the pointer result at 24
// and the string result at 32 and 40, so the area is 48. Four of its six
// words are pointers, and the callee allocates, so a collection can run while
// this frame and the wrapper's frame are both on the stack. The collector
// finds these words through the wrapper's own arguments bitmap: the assembly's
// outgoing area is the wrapper's incoming area.
TEXT ·CallPtrThrough(SB), NOSPLIT, $56-48
	MOVD	p+0(FP), R0
	MOVD	s_base+8(FP), R1
	MOVD	s_len+16(FP), R2
	MOVD	R0, 8(RSP)
	MOVD	R1, 16(RSP)
	MOVD	R2, 24(RSP)
	CALL	·sPtrThrough(SB)
	MOVD	32(RSP), R0
	MOVD	40(RSP), R1
	MOVD	48(RSP), R2
	MOVD	R0, rp+24(FP)
	MOVD	R1, rs_base+32(FP)
	MOVD	R2, rs_len+40(FP)
	RET
`

// abi0Pkg holds the Go functions the assembly calls and the declarations of
// the assembly itself.
//
// Each Go function carries the one-argument //go:linkname, which is what puts
// it in gc's callable set and therefore what makes it owe an ABI0 wrapper.
// The directive names no second symbol, so it renames nothing.
const abi0Pkg = `package asmpkg

import _ "unsafe" // for go:linkname

func CallZero() uint64

func CallNarrow(a int8) (r int8)

func CallTwo(a int8, b int8) (r1 int8, r2 int32)

func CallMix(a int8, b int64, c string, d float64) (r1 int32, r2 int64)

func CallFirstByte(b []byte) byte

func CallTail(s string) string

func CallZeroSize(a [3]int8, b [0]int64, c [3]int8) (r int8)

func CallPtrThrough(p *int, s string) (*int, string)

//go:linkname sZero
func sZero() uint64 { return 4242 }

//go:linkname sNarrow
func sNarrow(a int8) (r int8) { return a + 3 }

//go:linkname sTwo
func sTwo(a int8, b int8) (r1 int8, r2 int32) { return a + b, int32(a) * 100 }

//go:linkname sMix
func sMix(a int8, b int64, c string, d float64) (r1 int32, r2 int64) {
	return int32(a) + int32(len(c)), b + int64(d)
}

//go:linkname sFirstByte
func sFirstByte(b []byte) byte { return b[0] }

//go:linkname sTail
func sTail(s string) string { return s[1:] }

//go:linkname sZeroSize
func sZeroSize(a [3]int8, b [0]int64, c [3]int8) int8 { return a[0]*10 + c[2] }

// sPtrThrough allocates, so a collection can run with the assembly's frame
// and the wrapper's frame both on the stack. Its arguments and its results
// hold four pointer words between them.
//
//go:linkname sPtrThrough
func sPtrThrough(p *int, s string) (*int, string) {
	*p++
	return p, s + "!"
}
`

// abi0Main calls each assembly entry point and prints what came back.
//
// gc compiles it, because the package under test is the one nanogo compiles
// and a caller from the same compiler would agree with a wrong wrapper as
// readily as with a right one.
const abi0Main = `package main

import (
	"fmt"
	"runtime"

	"nanogo.example/abi0wrapper/asmpkg"
)

func main() {
	fmt.Println("zero", asmpkg.CallZero())
	fmt.Println("narrow", asmpkg.CallNarrow(4), asmpkg.CallNarrow(-3))
	r1, r2 := asmpkg.CallTwo(5, 6)
	fmt.Println("two", r1, r2)
	m1, m2 := asmpkg.CallMix(1, 2, "abc", 4.0)
	fmt.Println("mix", m1, m2)
	fmt.Println("firstbyte", asmpkg.CallFirstByte([]byte("hello")))
	fmt.Println("tail", asmpkg.CallTail("gopher"))
	fmt.Println("zerosize", asmpkg.CallZeroSize([3]int8{7, 8, 9}, [0]int64{}, [3]int8{1, 2, 3}))

	n := 100
	p, s := asmpkg.CallPtrThrough(&n, "abc")
	fmt.Println("ptrthrough", *p, s)

	// A pointer argument live across the call while the collector runs. The
	// string reaching the wrapper is freshly allocated and nothing else holds
	// it, so a wrapper whose arguments bitmap missed the word would let the
	// collector free it and the answer would change under it. The result is
	// checked rather than printed per iteration, so the comparison is over one
	// line and any wrong byte in any iteration moves it.
	sum := 0
	for i := 0; i < 20000; i++ {
		v := i
		q, out := asmpkg.CallPtrThrough(&v, fmt.Sprint("k", i))
		sum += *q + len(out)
		sum += int(asmpkg.CallFirstByte([]byte(out)))
		sum += len(asmpkg.CallTail(out))
		if i%2000 == 0 {
			runtime.GC()
		}
	}
	fmt.Println("under load", sum)
}
`

// TestGcAndNanogoAgreeOnAnABI0Wrapper is the evidence for stage 3.
//
// nanogo compiles the package that owes the wrappers and gc compiles the
// caller, so the two toolchains have to agree on where the arguments of an
// ABI0 function are. The program's output is compared against an all-gc build
// of the same module, which is the only comparison a wrong placement cannot
// pass: a nanogo-only program is self-consistent whatever the offsets are.
func TestGcAndNanogoAgreeOnAnABI0Wrapper(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":             "module nanogo.example/abi0wrapper\n\ngo 1.27\n",
		"main.go":            abi0Main,
		"asmpkg/asmpkg.go":   abi0Pkg,
		"asmpkg/asm_arm64.s": abi0Asm,
	}, []string{"nanogo.example/abi0wrapper/asmpkg"})

	if out, err := h.build(t, "-o", "abi0wrapper", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/abi0wrapper/asmpkg") {
		t.Fatalf("nanogo delegated the package that owes the wrappers:\n%s", strings.Join(lines, "\n"))
	}
	if compiled(lines, "main") {
		t.Fatalf("nanogo compiled the caller as well, so the comparison is not against gc:\n%s",
			strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "abi0wrapper"))
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
}

// TestABI0WrapperObjectHoldsBothConventions reads what nanogo wrote.
//
// The program above says the answers agree. This says why they can: one name
// is two text symbols, the Go function under ABIInternal and the wrapper under
// ABI0, and go tool nm prints a T for each. A build that resolved the
// assembly's ABI0 reference to the ABIInternal definition would take its
// arguments out of registers nothing wrote.
func TestABI0WrapperObjectHoldsBothConventions(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":             "module nanogo.example/abi0wrapper\n\ngo 1.27\n",
		"main.go":            abi0Main,
		"asmpkg/asmpkg.go":   abi0Pkg,
		"asmpkg/asm_arm64.s": abi0Asm,
	}, []string{"nanogo.example/abi0wrapper/asmpkg"})

	// -work is what keeps the archive on disk, and it is also what makes the
	// build leave a directory behind. The go command puts that directory under
	// GOTMPDIR, so this build is given one the framework owns and removes.
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

	archive := findABI0Archive(t, work)
	nm, err := exec.Command(h.goCmd, "tool", "nm", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm %s: %v\n%s", archive, err, nm)
	}
	syms := string(nm)
	// Two definitions of one name, one per convention: the Go function the
	// ordinary pipeline compiled and the ABI0 wrapper beside it.
	for _, name := range []string{
		"nanogo.example/abi0wrapper/asmpkg.sTail",
		"nanogo.example/abi0wrapper/asmpkg.sPtrThrough",
		"nanogo.example/abi0wrapper/asmpkg.sZeroSize",
	} {
		texts := 0
		for _, line := range strings.Split(syms, "\n") {
			f := strings.Fields(line)
			if len(f) >= 3 && f[len(f)-2] == "T" && f[len(f)-1] == name {
				texts++
			}
		}
		if texts != 2 {
			t.Errorf("the archive holds %d definitions of %s, want 2, one per ABI", texts, name)
		}
	}
	// The assembly's own declarations still owe the stage 2 pair, because the
	// assembly defines them under ABI0 and a Go caller names them under
	// ABIInternal. Stage 3 must not have displaced that.
	for _, want := range []string{
		"nanogo.example/abi0wrapper/asmpkg.CallTail.args_stackmap",
		"nanogo.example/abi0wrapper/asmpkg.CallPtrThrough.args_stackmap",
	} {
		if !strings.Contains(syms, want) {
			t.Errorf("the archive %s holds no %s", archive, want)
		}
	}
}

// findABI0Archive locates the compiled archive of the package under test.
func findABI0Archive(t *testing.T, work string) string {
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
		if strings.Contains(string(b), "asmpkg.sPtrThrough") {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("no archive for asmpkg under %s", work)
	}
	return found
}
