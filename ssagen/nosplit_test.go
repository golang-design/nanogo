// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// //go:nosplit is honoured, and the three things it changes are checked
// separately because each has its own failure. A check that is still emitted
// is a call to runtime.morestack from a function that runs where the call is
// not allowed. A symbol without SymFlagNoSplit is a chain cmd/link never adds
// up. A body that is not one unsafe point is a frame the runtime may stop in
// asynchronously, which gc's liveness.IsUnsafe forbids for the same functions.
//
// See specs/035-goroutines-and-stack-growth.md.

package ssagen

import (
	"fmt"
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
)

// nosplitSource is a function whose frame is too large to skip the check on
// its own, so the only thing that can remove the check is the directive.
//
// The array is written and read through an index the compiler cannot fold, so
// nothing removes the frame, and the call to g makes the function non-leaf,
// which is the other half of the rule the emitter applies.
const nosplitSource = `package main

func g(p *[64]int) int

func f(n int) int {
	var a [64]int
	a[n&63] = n
	return a[(n+1)&63] + g(&a)
}
`

// TestNoSplitRemovesTheCheckAndTheTail is the first of the three.
//
// Without the directive the function loads g.stackguard0, compares, branches
// to a tail and calls runtime.morestack_noctxt. With it, none of that is in
// the symbol. The disassembly is read rather than the instruction count,
// because the relocation is what names the callee.
func TestNoSplitRemovesTheCheckAndTheTail(t *testing.T) {
	for _, nosplit := range []bool{false, true} {
		c := compile(t, nosplitSource, "f")
		p := obj.NewPackage("main")
		r, err := Emit(c.f, c.a, p, Options{
			Sym: c.f.Sym, File: c.file, Line: 1, Fset: c.fset, NoSplit: nosplit,
		})
		if err != nil {
			t.Fatalf("NoSplit=%v: Emit: %v", nosplit, err)
		}
		if r.Frame < stackSmall {
			t.Fatalf("the frame is %d bytes, which is below StackSmall, so this test measures nothing", r.Frame)
		}
		text := disassemble(t, r, p)
		has := strings.Contains(text, "R_CALLARM64:runtime.morestack_noctxt")
		if nosplit && has {
			t.Errorf("//go:nosplit and the symbol still calls runtime.morestack_noctxt:\n%s", text)
		}
		if !nosplit && !has {
			t.Fatalf("without the directive the symbol does not call runtime.morestack_noctxt, so the comparison is empty:\n%s", text)
		}
	}
	comparisons++
}

// TestNoSplitClaimsTheSymbolFlag checks the object file flag.
//
// SymFlagNoSplit is what enrols the symbol in the budget cmd/link computes
// over the call graph, and its bit is goobj.SymFlagNoSplit. A function that
// emits no check and does not carry it is a chain that overflows with nothing
// to report it.
//
// The second half is the distinction the flag records. A leaf with no frame
// emits no check either and must not carry the flag: gc marks such a function
// LEAF|NOFRAME and not NOSPLIT, which `go tool compile -S` prints.
func TestNoSplitClaimsTheSymbolFlag(t *testing.T) {
	for _, nosplit := range []bool{false, true} {
		c := compile(t, nosplitSource, "f")
		r, err := Emit(c.f, c.a, obj.NewPackage("main"), Options{
			Sym: c.f.Sym, File: c.file, Line: 1, Fset: c.fset, NoSplit: nosplit,
		})
		if err != nil {
			t.Fatalf("NoSplit=%v: Emit: %v", nosplit, err)
		}
		if got := r.Text.Flag&obj.SymFlagNoSplit != 0; got != nosplit {
			t.Errorf("NoSplit=%v: SymFlagNoSplit=%v", nosplit, got)
		}
	}
	// A leaf with no frame, which the emitter already skips the check for.
	c := compile(t, "package main\n\nfunc f() int { return 7 }\n", "f")
	r, err := Emit(c.f, c.a, obj.NewPackage("main"), Options{
		Sym: c.f.Sym, File: c.file, Line: 1, Fset: c.fset,
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if r.Frame != 0 {
		t.Fatalf("the leaf has a frame of %d bytes, so it is not the shape under test", r.Frame)
	}
	if r.Text.Flag&obj.SymFlagNoSplit != 0 {
		t.Error("a leaf with no frame carries SymFlagNoSplit, which claims a property its author did not ask for")
	}
	comparisons++
}

// TestNoSplitMakesTheWholeBodyAnUnsafePoint checks the third change.
//
// gc's liveness.IsUnsafe is CompilingRuntime || f.NoSplit and it sets
// allUnsafe, so every value and every block of a //go:nosplit function is an
// unsafe point. `go tool compile -S` shows it: the PCDATA $0 stream of a
// NOSPLIT function goes to -2 at the first instruction of the body and never
// returns to -1, while the same function without the directive returns to -1
// after its frame is pushed.
//
// The expected stream is built with the same encoder the emitter uses, so
// what is compared is the entry list and not the encoding.
func TestNoSplitMakesTheWholeBodyAnUnsafePoint(t *testing.T) {
	c := compile(t, nosplitSource, "f")
	r, err := Emit(c.f, c.a, obj.NewPackage("main"), Options{
		Sym: c.f.Sym, File: c.file, Line: 1, Fset: c.fset, NoSplit: true,
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := r.Pcdata[ssa.PCDATA_UnsafePoint]
	if got == nil {
		t.Fatal("the function has no PCDATA_UnsafePoint stream")
	}
	want, err := obj.EncodePCData(
		[]obj.PCEntry{{PC: 0, Value: ssa.UnsafePointUnsafe}}, int64(r.Text.Size), minLC)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != string(want) {
		t.Errorf("the unsafe-point stream is %x and one unsafe range over the whole symbol is %x", got.Data, want)
	}
	// And the collector's maps are untouched by it. gc's
	// liveness.hasStackMap does not read allUnsafe, so a call still carries a
	// stack map, and a function whose calls lost theirs is a collector fault
	// rather than a missed preemption.
	if sm := r.Pcdata[ssa.PCDATA_StackMapIndex]; sm == nil || len(sm.Data) == 0 {
		t.Error("the function has no stack maps, so the directive removed the collector's tables and not only the safe points")
	}
	comparisons++
}

// TestNoSplitPrologueMatchesTheAssembler is the differential test of
// TestPrologueMatchesTheAssembler for a function that carries the directive.
//
// The frame push is the same at every size and the check is gone, which is
// the pair of claims the emitter makes and which no assertion on the count of
// instructions checks. The sizes are the ones that pick a different form of
// the check, because those are the forms that must not appear.
func TestNoSplitPrologueMatchesTheAssembler(t *testing.T) {
	sizes := []struct {
		size int64
		why  string
	}{
		{16, "the smallest frame that would carry a check"},
		{144, "past the guaranteed region, where the check would subtract first"},
		{256, "past the pre-indexed store, so the stack pointer is computed in a register"},
		{4112, "past the underflow bound, where the check would set the flags"},
	}
	n := 0
	for _, s := range sizes {
		t.Run(fmt.Sprintf("size%d", s.size), func(t *testing.T) {
			got := synthNoSplit(t, s.size, false, nil)
			want := asmText(t, noSplitListing(s.size))
			if len(got) == 0 {
				t.Fatal("nothing was emitted")
			}
			if len(want) < len(got) {
				t.Fatalf("%s: %d instructions and the assembler produced %d\n%v\n%v", s.why, len(got), len(want), got, want)
			}
			for _, w := range want[len(got):] {
				if w != 0 {
					t.Fatalf("%s: the assembler produced %d instructions and this package %d\n%v\n%v", s.why, len(want), len(got), got, want)
				}
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("%s: instruction %d is %#08x and the assembler produced %#08x", s.why, i, got[i], want[i])
				}
				n++
			}
		})
	}
	if n == 0 {
		t.Fatal("no instruction was compared")
	}
	comparisons += n
	t.Logf("%d //go:nosplit prologue instructions compared against go tool asm", n)
}

// noSplitListing is prologueListing with no check and no tail, which is what
// //go:nosplit asks for at every frame size.
func noSplitListing(size int64) []string {
	var l []string
	if size <= maxPreIndex {
		l = append(l,
			fmt.Sprintf("\tMOVD.W\tR30, %d(RSP)", -size),
			"\tMOVD\tR29, -8(RSP)")
	} else {
		l = append(l,
			fmt.Sprintf("\tSUB\t$%d, RSP, R20", size),
			"\tMOVD\tR29, -8(R20)",
			"\tMOVD\tR30, (R20)",
			"\tMOVD\tR20, RSP")
	}
	l = append(l, "\tSUB\t$8, RSP, R29")
	l = append(l, "\tMOVD\t-8(RSP), R29")
	if size <= maxPreIndex {
		l = append(l, fmt.Sprintf("\tMOVD.P\t%d(RSP), R30", size))
	} else {
		l = append(l,
			"\tMOVD\t(RSP), R30",
			fmt.Sprintf("\tADD\t$%d, RSP, RSP", size))
	}
	return append(l, "\tRET\t(R30)")
}
