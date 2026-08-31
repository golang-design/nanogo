// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package escape

// The encoding of one parameter's note.
//
// This is a transcription of cmd/compile/internal/escape/leaks.go, and it is a
// transcription rather than an equivalent because the bytes are read by gc and
// by nothing here. specs/023-escape-analysis.md records the two halves that
// make the encoding what it is: [leaks.Encode] returns the empty string
// exactly when the value leaks to the heap at zero dereferences, and gc's
// parseLeaks reads any string without the "esc:" prefix back as that same
// answer. So the empty note is the top of the lattice and not a missing note,
// and it is the answer this package falls back to everywhere.

// A leaks is the set of assignment flows from one parameter to the heap, to
// the mutator, to the callee, or to one of the first [numResults] results of
// the function that declares it.
//
// Each byte holds the minimum dereference count of any flow to that
// destination, offset by one, so that zero means "no flow".
type leaks [8]uint8

const (
	leakHeap = iota
	leakMutator
	leakCallee
	leakResult0
)

// numResults is how many results a note can name. A result past it takes the
// heap flow instead, which is what gc does.
const numResults = len(leaks{}) - leakResult0

// Heap returns the minimum dereference count of any flow to the heap, or -1
// when there is none.
func (l leaks) Heap() int { return l.get(leakHeap) }

// Mutator returns the minimum dereference count of any flow to the pointer
// operand of an indirect assignment, or -1 when there is none.
func (l leaks) Mutator() int { return l.get(leakMutator) }

// Callee returns the minimum dereference count of any flow to the callee
// operand of a call, or -1 when there is none.
func (l leaks) Callee() int { return l.get(leakCallee) }

// Result returns the minimum dereference count of any flow to the i'th result,
// or -1 when there is none.
func (l leaks) Result(i int) int { return l.get(leakResult0 + i) }

// AddHeap records a flow to the heap at derefs dereferences.
func (l *leaks) AddHeap(derefs int) { l.add(leakHeap, derefs) }

// AddMutator records a flow to the mutator at derefs dereferences.
func (l *leaks) AddMutator(derefs int) { l.add(leakMutator, derefs) }

// AddCallee records a flow to the callee at derefs dereferences.
func (l *leaks) AddCallee(derefs int) { l.add(leakCallee, derefs) }

// AddResult records a flow to the i'th result at derefs dereferences.
func (l *leaks) AddResult(i, derefs int) { l.add(leakResult0+i, derefs) }

func (l leaks) get(i int) int { return int(l[i]) - 1 }

func (l *leaks) add(i, derefs int) {
	if old := l.get(i); old < 0 || derefs < old {
		l.set(i, derefs)
	}
}

func (l *leaks) set(i, derefs int) {
	v := derefs + 1
	if v < 0 {
		// A negative dereference count is a defect in the caller, and the
		// answer that cannot be wrong is a flow to the heap at zero.
		l[leakHeap] = 1
		return
	}
	if v > 0xff {
		v = 0xff
	}
	l[i] = uint8(v)
}

// Optimize drops the flows that are no shorter than the shortest heap flow.
//
// A destination reached at the same count as the heap adds nothing: the caller
// already has to assume the heap. gc does this before encoding, so a note
// written without it can differ byte for byte from gc's for the same answer.
func (l *leaks) Optimize() {
	if x := l.Heap(); x >= 0 {
		for i := 1; i < len(*l); i++ {
			if l.get(i) >= x {
				l.set(i, -1)
			}
		}
	}
}

// Encode returns the note gc's export data carries.
//
// The empty string is the heap flow at zero dereferences and not the absence
// of an answer, which is why every refusal in this package returns it.
func (l leaks) Encode() string {
	if l.Heap() == 0 {
		// gc's space optimisation, and the reason nanogo's note said
		// "leaks to the heap" for as long as it wrote nothing.
		return ""
	}
	n := len(l)
	for n > 0 && l[n-1] == 0 {
		n--
	}
	return "esc:" + string(l[:n])
}

// heapNote is the note for a parameter nothing was proved about.
//
// It is the empty string, which gc reads back as a flow to the heap at zero
// dereferences. Every refusal below returns it, so a case this package has not
// thought about costs the caller an allocation and never a wrong answer.
const heapNote = ""
