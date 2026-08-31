// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
)

// The ABI0 wrapper of specs/047-abi-wrappers.md stage 3.
//
// [ABIWrapper] takes its arguments in registers and writes them where an
// assembly definition reads them. This is the other direction: the wrapper's
// own convention is ABI0, so it reads its arguments out of the incoming area
// and calls the Go function with them in registers.
//
// The two things that are silent if they are wrong are the placement and the
// arguments bitmap, and both are checked here against numbers read out of gc.

// abi0Wrapper compiles the ABI0 wrapper of the named declaration.
func abi0Wrapper(t *testing.T, src, name, sym string) *Result {
	t.Helper()
	c := compile(t, src, name)
	w, err := ABI0Wrapper(c.fn, sym)
	if err != nil {
		t.Fatalf("ABI0Wrapper(%s): %v", name, err)
	}
	f, err := ssa.Build(w)
	if err != nil {
		t.Fatalf("ssa.Build of the wrapper: %v", err)
	}
	wc := pipeline(t, w, f, c.fset, c.file)
	r, err := Emit(wc.f, wc.a, obj.NewPackage("main"), Options{Sym: sym, ABI0: true, File: c.file, Line: 1, Fset: c.fset})
	if err != nil {
		t.Fatalf("Emit of the wrapper: %v", err)
	}
	return r
}

// TestABI0WrapperIsDefinedUnderABI0 is the half of a symbol's identity that
// decides which definition a reference reaches.
//
// The wrapper and the function it calls carry one name. A wrapper emitted
// under ABIInternal would be a second definition of the symbol the ordinary
// pipeline already writes, and the assembly's ABI0 reference would find
// nothing.
func TestABI0WrapperIsDefinedUnderABI0(t *testing.T) {
	r := abi0Wrapper(t, "package main\n\nfunc f(a int) int { return a }\n", "f", "main.f")
	if r.Text.ABI != obj.ABI0 {
		t.Errorf("the wrapper is defined under ABI %d, want ABI0 (%d)", r.Text.ABI, obj.ABI0)
	}
	// Every auxiliary symbol the emitter names carries the convention, because
	// the ABIInternal definition of the same name owns the unsuffixed one.
	if n := r.DwarfInfo.Name; !strings.HasSuffix(n, ".abi0") {
		t.Errorf("the wrapper's DWARF symbol is %q, and the ABIInternal definition of main.f owns that name", n)
	}
}

// TestABI0WrapperPlacesItsOwnBoundaryOnTheStack is the placement.
//
// The numbers are gc's, read out of the ABI0 wrappers it writes: the TEXT line
// carries the area as the argsize half of $frame-args, and the body loads each
// argument from its offset.
func TestABI0WrapperPlacesItsOwnBoundaryOnTheStack(t *testing.T) {
	for _, tt := range []struct {
		name string
		decl string
		// area is gc's argsize and offs are the offsets of the parameters.
		area int64
		offs []int64
	}{
		// TEXT p/p.g1(SB), ..., $32-16, and it loads a from 0 and stores r at
		// 8. The result is at 8 and not at 1: the pointer-alignment field
		// between the arguments and the results is a whole word.
		{"a narrow argument and a narrow result", "func f(a int8) (r int8) { return a }", 16, []int64{0}},
		// TEXT internal/cpu.sysctlEnabled(SB), ..., $48-32.
		{"a slice", "func f(name []byte) bool { return name != nil }", 32, []int64{0}},
		// TEXT runtime.cmpstring(SB), ..., $48-40, loading a from 0 and 8 and
		// b from 16 and 24.
		{"two strings", "func f(a, b string) int { return 0 }", 40, []int64{0, 16}},
		// TEXT p/p.gof(SB), ..., $112-72, loading a from 0, b from 8, the
		// string from 16 and 24 and d from 32, and storing r1 at 40 and r2 at
		// 48. Every clause of the recurrence is in this row.
		{"the six argument row",
			"func f(a int8, b int64, c string, d float64) (r1 int32, r2 [3]int64) { return }",
			72, []int64{0, 8, 16, 32}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := "package main\n\n" + tt.decl + "\n"
			c := compile(t, src, "f")
			w, err := ABI0Wrapper(c.fn, "main.f")
			if err != nil {
				t.Fatalf("ABI0Wrapper: %v", err)
			}
			f, err := ssa.Build(w)
			if err != nil {
				t.Fatalf("ssa.Build: %v", err)
			}
			wc := pipeline(t, w, f, c.fset, c.file)
			if got := wc.f.ABI.ArgsSize; got != tt.area {
				t.Errorf("the ABI0 area is %d bytes, want gc's %d", got, tt.area)
			}
			for i, want := range tt.offs {
				got := wc.f.ABI.In[i].Off
				if got != want {
					t.Errorf("parameter %d is at %d, want gc's %d", i, got, want)
				}
				if wc.f.ABI.In[i].InReg {
					t.Errorf("parameter %d travels in a register, and ABI0 has none", i)
				}
			}
		})
	}
}

// TestABI0WrapperArgumentsBitmapMatchesGc is the map the collector reads.
//
// A missing bit frees a live pointer and a spare bit makes the collector
// follow a word that is not one, and neither shows up where it was caused. So
// the map is compared against gc's own bytes rather than against a rule
// restated here.
//
// gc names a gclocals symbol by a hash of its content, so the bytes below were
// read out of the symbol each ABI0 wrapper's FUNCDATA $0 names in
// go tool compile -S output. The encoding is the number of bitmaps, then the
// number of words each covers, then one bitmap per stack map index.
//
// What is compared is which words are marked, because that is what the
// collector follows. gc's rule has a second half that is easy to get wrong and
// that nanogo also obeys: a result that sits in the area is not marked. It is
// written after the last safepoint, so at every safepoint those words still
// hold whatever the previous frame left, and marking them would make the
// collector follow it. gc's map for func(p *int, s string) *int leaves word 3
// clear, and word 3 is the pointer result.
//
// The declared width is not compared, and the divergence is recorded rather
// than hidden. gc sizes the map to the last word that can hold a pointer,
// counting a pointer result. nanogo sizes it in ssa.LayoutFrame, which reaches
// the end of the last value in the area whether or not that value holds a
// pointer and which describes a result by an opaque type of the same width.
// The two therefore disagree by a word in both directions: 1 against gc's 0
// for func(int8) int8, and 3 against gc's 4 for func(*int, string) *int.
// Neither changes what is scanned, because every word the two widths differ
// over is unmarked in both. A width that was too small would not be silent
// either: ssa.BuildStackMaps refuses a marked word outside the map with an
// error naming the argument, and the last check below is the standing evidence
// that no marked word ever falls outside.
func TestABI0WrapperArgumentsBitmapMatchesGc(t *testing.T) {
	for _, tt := range []struct {
		name string
		decl string
		// words are the area words gc marks, out of the gclocals symbol.
		words []int32
	}{
		// gclocals of 01 00 00 00 00 00 00 00: one bitmap, no word, no bit.
		{"no pointer at all", "func f(a int8) (r int8) { return a }", nil},
		// 02 00 00 00 01 00 00 00 01 00, which internal/cpu.sysctlEnabled's
		// wrapper names. The slice base is word 0 and the bool result holds
		// no pointer.
		{"a slice and a scalar result", "func f(name []byte) bool { return name != nil }", []int32{0}},
		// 02 00 00 00 03 00 00 00 05 00, which runtime.cmpstring's wrapper
		// names. Words 0 and 2 are the two string bases, and the two lengths
		// between them are not marked.
		{"two strings", "func f(a, b string) int { return 0 }", []int32{0, 2}},
		// 02 00 00 00 04 00 00 00 03 00, which p/p.ptrarg's wrapper names.
		// Words 0 and 1 are the pointer and the string base. Word 3 is the
		// pointer result and it is not marked.
		{"a pointer result", "func f(p *int, s string) *int { return p }", []int32{0, 1}},
		// 02 00 00 00 03 00 00 00 04 00, which p/p.gof's wrapper names. Only
		// the string base at word 2 is a pointer, and the [3]int64 result
		// holds none.
		{"the six argument row",
			"func f(a int8, b int64, c string, d float64) (r1 int32, r2 [3]int64) { return }",
			[]int32{2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := abi0Wrapper(t, "package main\n\n"+tt.decl+"\n", "f", "main.f")
			if r.maps == nil {
				t.Fatal("the wrapper carries no stack maps")
			}
			if len(r.maps.Args.Maps) == 0 {
				t.Fatal("the wrapper carries no arguments bitmap at all")
			}
			// Every stack map index, the stack-growth tail included. Under
			// ABI0 nothing travels in a register, so the tail spills nothing
			// and its map is the body's.
			for i, m := range r.maps.Args.Maps {
				got := markedWords(m)
				if !sameWords(got, tt.words) {
					t.Errorf("arguments bitmap %d marks words %v, want gc's %v", i, got, tt.words)
				}
				for _, w := range got {
					if w >= r.maps.Args.NBit {
						t.Errorf("arguments bitmap %d marks word %d, which is outside the %d word map",
							i, w, r.maps.Args.NBit)
					}
				}
			}
		})
	}
}

// markedWords lists the word indices a bitmap sets, low bit first.
func markedWords(b []byte) []int32 {
	var out []int32
	for i, x := range b {
		for k := 0; k < 8; k++ {
			if x&(1<<uint(k)) != 0 {
				out = append(out, int32(i*8+k))
			}
		}
	}
	return out
}

func sameWords(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestABI0WrapperRefusesAMethod keeps the receiver out of this path.
//
// gc refuses the same shape with "makeABIWrapper support for wrapping methods
// not implemented". The receiver takes offset 0 of the ABI0 area, ahead of the
// arguments, and specs/030-abi.md already names nanogo's recovery of a
// receiver at a call site as a heuristic.
func TestABI0WrapperRefusesAMethod(t *testing.T) {
	c := compile(t, "package main\n\ntype T int\n\nfunc (t T) M(a int) int { return int(t) + a }\n", "M")
	if _, err := ABI0Wrapper(c.fn, "main.T.M"); err == nil {
		t.Fatal("ABI0Wrapper built a wrapper for a method")
	} else if !strings.Contains(err.Error(), "method") {
		t.Errorf("the message does not name the shape: %v", err)
	}
}
