// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// A call's stack result read out of the outgoing argument area by another
// call.
//
// The outgoing argument area is the callee's for the whole of a call and not
// only while the arguments are being written. Go's register ABI gives every
// register-assigned parameter a home in the CALLER's area, at
// abi.ABIParamResultInfo.SpillAreaOffset, and the callee's register allocator
// spills the parameter there whenever it is live across a call the callee
// makes. runtime.typedmemmove is such a callee: when
// runtime.writeBarrier.enabled is set it spills its three parameters into the
// caller's area at offsets 0, 8 and 16 before calling
// runtime.bulkBarrierPreWrite.
//
// A result the registers cannot hold sits in that same area. Handing its
// address to runtime.typedmemmove as the source therefore replaces the first
// three words of the value with {type, dst, src} and copies them to the
// destination. ssa/abi.go's stageArea is the fix, and it is gc's: its
// ssa/writebarrier.go calls the same address volatile and copies it to a local
// before it can be an operand of runtime.wbMove.
//
// The failure is silent. It corrupts a heap object rather than throwing, and
// it only appears while the write barrier is on, which is why
// specs/060-selfhost.md read it as a collector fault for as long as it did.

// argareaProgram calls a function whose result the registers cannot hold and
// stores the result through a pointer, with the collector running.
//
// Each part earns its place. wide holds twelve pointers and four integers, so
// it is over the sixteen result registers and travels in the argument area,
// and its pointer map is what makes the move a barrier move rather than a
// plain memmove. store writes it through a parameter, which is an address the
// compiler cannot prove is a frame slot, so the move needs the barrier. The
// loop allocates, and SetGCPercent(1) keeps a collection in flight, which is
// what keeps runtime.writeBarrier.enabled set: with the barrier off the
// spilling path in runtime.typedmemmove is not taken and the fault does not
// appear at all.
const argareaProgram = `package main

import "runtime/debug"

type wide struct {
	p [12]*int
	n [4]int
}

//go:noinline
func mk(x *int) wide {
	var w wide
	for i := 0; i < 12; i++ {
		w.p[i] = x
	}
	for i := 0; i < 4; i++ {
		w.n[i] = 7
	}
	return w
}

//go:noinline
func store(dst *wide, x *int) {
	*dst = mk(x)
}

var keep []byte

func main() {
	debug.SetGCPercent(1)
	x := 1
	bad := 0
	for i := 0; i < 4000; i++ {
		d := new(wide)
		store(d, &x)
		for j := 0; j < 12; j++ {
			if d.p[j] != &x {
				bad++
			}
		}
		for j := 0; j < 4; j++ {
			if d.n[j] != 7 {
				bad++
			}
		}
		keep = make([]byte, 512)
	}
	println(bad, x, len(keep))
}
`

// TestToolexecDoesNotReadAStackResultOutOfTheAreaTheBarrierCallOverwrites
// compiles, links and runs the program, and compares what it printed with gc.
func TestToolexecDoesNotReadAStackResultOutOfTheAreaTheBarrierCallOverwrites(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/argarea\n\ngo 1.27\n",
		"main.go": argareaProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "argarea", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	got := runProgram(t, filepath.Join(h.mod, "argarea"))
	if want := "0 1 512\n"; string(got) != want {
		t.Errorf("the program printed %q, want %q", got, want)
	}
	if want := gcOutput(t, h); string(got) != string(want) {
		t.Errorf("nanogo's program printed %q and gc's printed %q", got, want)
	}
}
