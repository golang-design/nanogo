// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// The two runtime caches of specs/032-type-descriptors-and-itabs.md, run as a
// program.
//
// A unit test reads the call an assertion to an interface lowers to and cannot
// say whether the symbol it hands the runtime is one the runtime can write
// into. That is the whole question here. An *abi.TypeAssert and an
// *abi.InterfaceSwitch are not read-only descriptors: runtime.typeAssert and
// runtime.interfaceSwitch build a table of the answers they computed and
// install it with a compare-and-swap on the symbol's first word. Three facts
// have to hold for that to work and none of them is visible from a tree:
//
//   - The symbol is in a writable section. A read-only page faults on the
//     store.
//   - The symbol carries the linker name of its own Go type. cmd/link builds
//     the pointer map of the data section out of it, and without one the link
//     stops with "missing Go type information for global symbol".
//   - The collector therefore keeps the table alive. Without the scan the
//     table is freed and the next install reads a pointer to memory the heap
//     has given away.
//
// The program below is the only shape that tests all three. nanogo compiles
// the library that asserts and switches, gc compiles the importer, so the
// runtime does the search through symbols this compiler wrote. The importer
// reads the cache word out of one of them by linkname, before and after, and
// fails when it did not move: the runtime installs a table about one time in a
// thousand, so a single assertion proves the call works and proves nothing
// about the symbol. Between the passes it collects and allocates, so a table
// the collector could not see would have been handed away by the time the next
// pass reads it.
const cacheLibrary = `package lib

type I interface{ M() int }

type J interface {
	M() int
	N() int
}

type A int

func (a A) M() int { return int(a) }

type B int

func (b B) M() int { return int(b) }
func (b B) N() int { return int(b) * 10 }

type C int

func (c C) M() int { return int(c) }

type K interface {
	M() int
	N() int
	O() int
}

type E int

func (e E) M() int { return int(e) }
func (e E) N() int { return int(e) * 10 }
func (e E) O() int { return int(e) * 100 }

// Assert is the one-value form. A nil operand panics before the lookup, which
// is the message the specification asks for, so nothing calls it with one.
//
//go:noinline
func Assert(v any) I { return v.(I) }

// Try is the two-value form, which asks the runtime to answer nothing rather
// than panic.
//
//go:noinline
func Try(v any) (I, bool) { i, ok := v.(I); return i, ok }

// Narrow is the conversion between two interfaces that are not the same type.
// It is the comma-ok assertion with the answer dropped.
//
//go:noinline
func Narrow(j J) I { return j }

// Class writes an interface case before a concrete case that satisfies it, so
// a B reaches case J and not case B. It is the whole of the ordering rule.
//
//go:noinline
func Class(v any) int {
	switch t := v.(type) {
	case nil:
		return -1
	case J:
		return 1000 + t.N()
	case B:
		return 2000 + t.M()
	case I:
		return 3000 + t.M()
	case int:
		return 4000 + t
	default:
		return 0
	}
}

// Ordered is the other order: a concrete case before an interface case the
// same type satisfies.
//
//go:noinline
func Ordered(v any) int {
	switch t := v.(type) {
	case C:
		return 100 + t.M()
	case I:
		return 200 + t.M()
	}
	return 0
}

// Anyone puts the empty interface after an interface with methods. Every
// non-nil dynamic type is a value of it, and a nil one is not.
//
//go:noinline
func Anyone(v any) int {
	switch t := v.(type) {
	case J:
		return 1
	case any:
		if t == nil {
			return -2
		}
		return 2
	}
	return 0
}

// Mixed switches on a guard that has methods, so the operand leads with an
// itab and not with a descriptor. The two runs read that one word differently:
// the concrete case compares it against the itab of its own pair, and the
// interface case hands the runtime the descriptor stored inside it. A mix-up
// between the two is a clause that runs for the wrong value and says nothing.
//
//go:noinline
func Mixed(j J) int {
	switch t := j.(type) {
	case B:
		return 10 + t.M()
	case K:
		return 20 + t.O()
	}
	return 0
}

//go:noinline
func BoxA(n int) any { return A(n) }

//go:noinline
func BoxB(n int) any { return B(n) }

//go:noinline
func BoxC(n int) any { return C(n) }

//go:noinline
func BoxInt(n int) any { return n }

//go:noinline
func BoxJ(n int) J { return B(n) }

//go:noinline
func BoxJE(n int) J { return E(n) }
`

// cacheImporter is compiled by gc, so runtime.GC and the linkname are the
// installed compiler's and only the library under test is nanogo's.
const cacheImporter = `package main

import (
	"os"
	"runtime"
	_ "unsafe"

	"nanogo.example/cache/lib"
)

func want(what string, got, exp int) {
	if got != exp {
		println("FAIL", what, "got", got, "want", exp)
		os.Exit(1)
	}
}

// The two symbols nanogo defined, read as the structures internal/abi
// declares. Only the words this program reads are named: a shorter struct
// still places the fields it declares where the compiler wrote them.

//go:linkname assertCache nanogo.example/cache/lib.Assert..typeAssert.0
var assertCache struct {
	Cache   uintptr
	Inter   uintptr
	CanFail bool
}

// Class writes case J, then case B, then case I. J and I are not consecutive,
// so they are two runs and two symbols, which is the ordering rule made
// visible: one symbol holding both would let the runtime answer I for a value
// the source wrote case B for first.

//go:linkname switchCache nanogo.example/cache/lib.Class..interfaceSwitch.0
var switchCache struct {
	Cache  uintptr
	NCases int
}

//go:linkname switchCache1 nanogo.example/cache/lib.Class..interfaceSwitch.1
var switchCache1 struct {
	Cache  uintptr
	NCases int
}

var sink []*[512]byte

// churn allocates and drops, so that a table the collector cannot see is
// handed out again before the next pass reads it.
func churn() {
	for i := 0; i < 8192; i++ {
		sink = append(sink, new([512]byte))
	}
	sink = nil
}

func main() {
	assertStart, switchStart := assertCache.Cache, switchCache.Cache
	if assertCache.Cache == 0 {
		println("FAIL the assertion cache starts at nil, which the runtime dereferences")
		os.Exit(1)
	}
	if assertCache.CanFail {
		println("FAIL the one-value assertion asks the runtime to answer nothing")
		os.Exit(1)
	}
	if switchCache.NCases != 1 || switchCache1.NCases != 1 {
		println("FAIL the two runs list", switchCache.NCases, "and", switchCache1.NCases, "cases, want one each")
		os.Exit(1)
	}
	if switchCache.Cache == 0 || switchCache1.Cache == 0 {
		println("FAIL a switch cache starts at nil, which the runtime dereferences")
		os.Exit(1)
	}
	a, b, c, i := lib.BoxA(7), lib.BoxB(3), lib.BoxC(5), lib.BoxInt(9)
	for n := 0; n < 200000; n++ {
		want("assert A", lib.Assert(a).M(), 7)
		want("assert B", lib.Assert(b).M(), 3)
		if v, ok := lib.Try(c); !ok || v.M() != 5 {
			println("FAIL try C")
			os.Exit(1)
		}
		if _, ok := lib.Try(i); ok {
			println("FAIL an int has no M and the assertion said it did")
			os.Exit(1)
		}
		if _, ok := lib.Try(nil); ok {
			println("FAIL nil has no dynamic type and the assertion said it did")
			os.Exit(1)
		}
		want("narrow", lib.Narrow(lib.BoxJ(4)).M(), 4)
		want("class nil", lib.Class(nil), -1)
		want("class J before B", lib.Class(b), 1030)
		want("class I", lib.Class(a), 3007)
		want("class I again", lib.Class(c), 3005)
		want("class int", lib.Class(i), 4009)
		want("class default", lib.Class("s"), 0)
		want("ordered concrete first", lib.Ordered(c), 105)
		want("ordered interface", lib.Ordered(a), 207)
		want("any after J", lib.Anyone(b), 1)
		want("any", lib.Anyone(a), 2)
		want("any not nil", lib.Anyone(nil), 0)
		want("mixed concrete", lib.Mixed(lib.BoxJ(3)), 13)
		want("mixed interface", lib.Mixed(lib.BoxJE(4)), 420)
		// The same calls with nothing rooting the boxed value outside nanogo's
		// frame. runtime.typeAssert allocates when it builds a table, so a
		// collection can run inside the call, and the operand's data word is
		// then live only in the frame the stack map describes.
		want("assert unrooted", lib.Assert(lib.BoxA(11)).M(), 11)
		want("class unrooted", lib.Class(lib.BoxB(6)), 1060)
		if n%40000 == 0 {
			runtime.GC()
			churn()
			runtime.GC()
		}
	}
	if assertCache.Cache == assertStart {
		println("FAIL the runtime never installed a type assertion cache")
		os.Exit(1)
	}
	if switchCache.Cache == switchStart {
		println("FAIL the runtime never installed an interface switch cache")
		os.Exit(1)
	}
}
`

// TestToolexecCachesWhatTheRuntimeWrites is the evidence that the two symbols
// this compiler defines are ones the runtime can install a table into.
//
// The library is on the allowlist and the importer is not, so nanogo compiles
// only the code that asserts and switches. gc compiles the program that drives
// it and reads the cache words back.
func TestToolexecCachesWhatTheRuntimeWrites(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":     "module nanogo.example/cache\n\ngo 1.27\n",
		"lib/lib.go": cacheLibrary,
		"main.go":    cacheImporter,
	}, []string{"# nanogo owns the library that asserts and switches", "nanogo.example/cache/lib"})

	if out, err := h.build(t, "-o", "cache", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	lines := h.decisions(t)
	if !compiled(lines, "nanogo.example/cache/lib") {
		t.Fatalf("nanogo delegated the library, so nothing it wrote reached the runtime:\n%s",
			strings.Join(lines, "\n"))
	}
	if compiled(lines, "main") {
		t.Fatalf("nanogo compiled the importer, and the driver must be gc's:\n%s", strings.Join(lines, "\n"))
	}
	runProgram(t, filepath.Join(h.mod, "cache"))
}
