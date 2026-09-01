// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package escape

import (
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The note table.
//
// Every case states the note nanogo writes and the note gc writes for the same
// source. The second column is what makes the table a soundness check rather
// than a record of the current behaviour: a note nanogo writes must be gc's
// own note or the empty one, because the empty note is the top of the lattice
// and everything else is a claim gc's caller acts on.
//
// gc's notes were read out of gc's own archives with the fixtures of
// TestEscapeNoteMatchesGC in cmd/nanogo, which compiles each case with the
// toolchain and reads the tag bytes back.

// notePrelude is the package every case is built into.
const notePrelude = `package p

import "unsafe"

var _ unsafe.Pointer

type T struct {
	A int
	P *int
}

var gp *int
var gs []byte
var gi any
var gu unsafe.Pointer

func use(*int) {}

func two(p *int) (int, *int) { return 1, p }
`

// noFlow is the note for a parameter that flows nowhere: "esc:" and no bytes
// after it, because leaks.Encode trims the trailing zeros of an empty set.
const noFlow = "esc:"

// result0 is the note for a parameter that flows to the first result at zero
// dereferences and nowhere else, which is the one destination besides "nowhere"
// that specs/023-escape-analysis.md's stage 3 can name.
const result0 = "esc:\x00\x00\x00\x01"

// noescapeNote is the note gc writes for a parameter of a bodyless
// //go:noescape declaration: a mutator flow and a callee flow, both at zero
// dereferences.
const noescapeNote = "esc:\x00\x01\x01"

func TestParamNotes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		fn   string
		want map[string]string // parameter name to note
	}{{
		name: "a parameter only read through is proved",
		src:  `func Load(b []byte) uint64 { return uint64(b[0]) | uint64(b[1])<<8 }`,
		fn:   "Load",
		want: map[string]string{"b": noFlow},
	}, {
		name: "a parameter that is never named is proved",
		src:  `func Ignore(p *int) int { return 1 }`,
		fn:   "Ignore",
		want: map[string]string{"p": noFlow},
	}, {
		name: "a blank parameter is proved without an analysis",
		src:  `func Blank(_ *int) {}`,
		fn:   "Blank",
		want: map[string]string{"_": noFlow},
	}, {
		name: "a scalar parameter is not tagged",
		src:  `func Scalar(n int) int { return n }`,
		fn:   "Scalar",
		want: map[string]string{"n": heapNote},
	}, {
		name: "a parameter assigned to a global leaks",
		src:  `func Keep(p *int) { gp = p }`,
		fn:   "Keep",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a parameter that reaches a global through a local leaks",
		src:  `func Keep2(p *int) { q := p; gp = q }`,
		fn:   "Keep2",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a returned parameter is described as a flow to that result",
		src:  `func Ret(p *int) *int { return p }`,
		fn:   "Ret",
		want: map[string]string{"p": result0},
	}, {
		name: "a parameter passed to a call leaks",
		src:  `func Pass(p *int) { use(p) }`,
		fn:   "Pass",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a slice of a parameter that is returned keeps the parameter's own depth",
		src:  `func Tail(b []byte) []byte { return b[1:] }`,
		fn:   "Tail",
		want: map[string]string{"b": result0},
	}, {
		name: "a slice of a parameter that is only measured is proved",
		src:  `func TailLen(b []byte) int { return len(b[1:]) }`,
		fn:   "TailLen",
		want: map[string]string{"b": noFlow},
	}, {
		name: "a write through a parameter leaks, because it is gc's mutator flow",
		src:  `func Mut(b []byte) { b[0] = 1 }`,
		fn:   "Mut",
		want: map[string]string{"b": heapNote},
	}, {
		name: "a write through a pointer parameter leaks",
		src:  `func MutT(t *T) { t.A = 1 }`,
		fn:   "MutT",
		want: map[string]string{"t": heapNote},
	}, {
		name: "a read through a pointer parameter is proved",
		src:  `func ReadT(t *T) int { return t.A }`,
		fn:   "ReadT",
		want: map[string]string{"t": noFlow},
	}, {
		name: "a parameter whose address the source takes leaks, because nanogo puts it in a cell",
		src:  `func Addr(p *int) int { q := &p; return **q }`,
		fn:   "Addr",
		want: map[string]string{"p": heapNote},
	}, {
		name: "the address of an element of a parameter leaks when it is stored",
		src:  `func Elem(b []byte) { gp = (*int)(unsafe.Pointer(&b[0])) }`,
		fn:   "Elem",
		want: map[string]string{"b": heapNote},
	}, {
		name: "a parameter laundered into a uintptr flows nowhere the collector follows",
		src:  `func Word(p *int) uintptr { return uintptr(unsafe.Pointer(p)) }`,
		fn:   "Word",
		want: map[string]string{"p": noFlow},
	}, {
		name: "a parameter boxed into an interface leaks, because nanogo boxes on the heap",
		src:  `func Box(p *int) { var i any = p; _ = i }`,
		fn:   "Box",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a parameter copied into a frame variable is proved",
		src:  `func Frame(p *int) int { var s T; s.P = p; return *s.P }`,
		fn:   "Frame",
		want: map[string]string{"p": noFlow},
	}, {
		name: "a parameter read in a loop is proved",
		src: `func Sum(b []byte) int {
	n := 0
	for i := 0; i < len(b); i++ {
		n += int(b[i])
	}
	return n
}`,
		fn:   "Sum",
		want: map[string]string{"b": noFlow},
	}, {
		name: "a parameter that leaks on one path of a loop leaks on all",
		src: `func SumLeak(b []byte) int {
	n := 0
	for i := 0; i < len(b); i++ {
		if i == 3 {
			gs = b
		}
		n += int(b[i])
	}
	return n
}`,
		fn:   "SumLeak",
		want: map[string]string{"b": heapNote},
	}, {
		name: "a parameter compared in a switch is proved",
		src: `func Sw(p *int) int {
	switch {
	case p == nil:
		return 0
	}
	return 1
}`,
		fn:   "Sw",
		want: map[string]string{"p": noFlow},
	}, {
		name: "a parameter captured by a literal leaks",
		src:  `func Cap(p *int) func() int { return func() int { return *p } }`,
		fn:   "Cap",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a parameter under an operation the analysis does not name leaks",
		src:  `func Send(ch chan []byte, b []byte) { ch <- b }`,
		fn:   "Send",
		want: map[string]string{"ch": heapNote, "b": heapNote},
	}, {
		name: "a parameter beside an operation the analysis does not name is still proved",
		src: `func Beside(b []byte, ch chan int) int {
	ch <- 1
	return len(b)
}`,
		fn:   "Beside",
		want: map[string]string{"b": noFlow, "ch": heapNote},
	}, {
		name: "a parameter assigned to a named result is described by the taint set",
		src:  `func Named(p *int) (r *int) { r = p; return }`,
		fn:   "Named",
		want: map[string]string{"p": result0},
	}, {
		name: "a parameter deferred leaks",
		src:  `func Def(p *int) { defer use(p) }`,
		fn:   "Def",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a parameter started on a goroutine leaks",
		src:  `func Go(p *int) { go use(p) }`,
		fn:   "Go",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a parameter appended to a global leaks",
		src:  `func App(b []byte) { gs = append(gs, b...) }`,
		fn:   "App",
		want: map[string]string{"b": heapNote},
	}, {
		name: "a called function parameter leaks, because it is gc's callee flow",
		src:  `func Callee(f func()) { f() }`,
		fn:   "Callee",
		want: map[string]string{"f": heapNote},
	}, {
		name: "a variadic parameter nothing names is proved",
		src:  `func Var(ps ...*int) int { return 1 }`,
		fn:   "Var",
		want: map[string]string{"ps": noFlow},
	}, {
		name: "a variadic parameter that is returned flows to that result",
		src:  `func VarRet(ps ...*int) []*int { return ps }`,
		fn:   "VarRet",
		want: map[string]string{"ps": result0},
	}, {
		name: "a parameter used as a map key in a read is proved",
		src:  `func Key(m map[string]int, k string) int { return m[k] }`,
		fn:   "Key",
		want: map[string]string{"m": noFlow, "k": noFlow},
	}, {
		name: "a parameter used as a map key in a write leaks",
		src:  `func KeyPut(m map[string]int, k string) { m[k] = 1 }`,
		fn:   "KeyPut",
		want: map[string]string{"m": heapNote, "k": heapNote},
	}, {
		name: "a receiver read through is proved and a stored parameter is not",
		src:  `func (t *T) M(p *int) int { t.P = p; return t.A }`,
		fn:   "(*T).M",
		want: map[string]string{"t": heapNote, "p": heapNote},
	}, {
		name: "a receiver and a parameter both only read are proved",
		src:  `func (t *T) N(p *int) int { return t.A + *p }`,
		fn:   "(*T).N",
		want: map[string]string{"t": noFlow, "p": noFlow},
	},

		// Stage 3: the destinations a note can name besides "nowhere".
		{
			name: "a flow to a result past the first names its own position",
			src:  `func Second(p *int) (int, *int) { return 1, p }`,
			fn:   "Second",
			want: map[string]string{"p": "esc:\x00\x00\x00\x00\x01"},
		}, {
			name: "a parameter returned twice names both results",
			src:  `func Both(p *int) (*int, *int) { return p, p }`,
			fn:   "Both",
			want: map[string]string{"p": "esc:\x00\x00\x00\x01\x01"},
		}, {
			name: "the last result a note can name is the fifth",
			src:  `func Five(p *int) (a, b, c, d, e *int) { e = p; return }`,
			fn:   "Five",
			want: map[string]string{"p": "esc:\x00\x00\x00\x00\x00\x00\x00\x01"},
		}, {
			name: "a result past the note leaks, because no byte holds it",
			src:  `func Six(p *int) (a, b, c, d, e, f *int) { f = p; return }`,
			fn:   "Six",
			want: map[string]string{"p": heapNote},
		}, {
			name: "a parameter that reaches a result and the heap leaks",
			src:  `func RetKeep(p *int) *int { gp = p; return p }`,
			fn:   "RetKeep",
			want: map[string]string{"p": heapNote},
		}, {
			name: "a parameter that reaches a result and an interface leaks",
			src:  `func RetBox(p *int) *int { gi = p; return p }`,
			fn:   "RetBox",
			want: map[string]string{"p": heapNote},
		}, {
			name: "a parameter that reaches a result and a call leaks",
			src:  `func RetPass(p *int) *int { use(p); return p }`,
			fn:   "RetPass",
			want: map[string]string{"p": heapNote},
		}, {
			name: "a result this pass cannot separate from a call's tuple leaks",
			src:  `func RetTuple(p *int) (int, *int) { return two(p) }`,
			fn:   "RetTuple",
			want: map[string]string{"p": heapNote},
		}, {
			name: "the address of an element of a parameter is that parameter's own depth",
			src:  `func AddrElem(b []byte) *byte { return &b[0] }`,
			fn:   "AddrElem",
			want: map[string]string{"b": result0},
		}, {
			name: "a field of a parameter passed by value is at the parameter's own depth",
			src:  `func FieldVal(t T) *int { return t.P }`,
			fn:   "FieldVal",
			want: map[string]string{"t": result0},
		}, {
			name: "a field read through a parameter is one dereference down, so it is refused",
			src:  `func FieldPtr(t *T) *int { return t.P }`,
			fn:   "FieldPtr",
			want: map[string]string{"t": heapNote},
		}, {
			name: "an element of an array parameter is at the parameter's own depth",
			src:  `func ArrElem(a [2]*int) *int { return a[0] }`,
			fn:   "ArrElem",
			want: map[string]string{"a": result0},
		}, {
			name: "an element of a slice parameter is one dereference down, so it is refused",
			src:  `func SliceElem(s []*int) *int { return s[0] }`,
			fn:   "SliceElem",
			want: map[string]string{"s": heapNote},
		}, {
			name: "a value read out of a map parameter is refused",
			src:  `func MapElem(m map[int]*int) *int { return m[0] }`,
			fn:   "MapElem",
			want: map[string]string{"m": heapNote},
		}, {
			name: "a dereference of a parameter is refused, because its depth is not zero",
			src:  `func Deep(q **int) *int { return *q }`,
			fn:   "Deep",
			want: map[string]string{"q": heapNote},
		}, {
			name: "a slice of a string parameter shares the string's own bytes",
			src:  `func StrTail(s string) string { return s[1:] }`,
			fn:   "StrTail",
			want: map[string]string{"s": result0},
		}, {
			name: "a converted parameter is at the parameter's own depth",
			src:  `func Conv(p *int) unsafe.Pointer { return unsafe.Pointer(p) }`,
			fn:   "Conv",
			want: map[string]string{"p": result0},
		}, {
			name: "a string converted to bytes copies, so nothing of it reaches the result",
			src:  `func Bytes(s string) []byte { return []byte(s) }`,
			fn:   "Bytes",
			want: map[string]string{"s": noFlow},
		}, {
			name: "a byte slice converted to a string copies as well",
			src:  `func Str(b []byte) string { return string(b) }`,
			fn:   "Str",
			want: map[string]string{"b": noFlow},
		}, {
			name: "a slice converted to an array reads its elements out, so it is one down",
			src:  `func Arr(s []*int) [2]*int { return [2]*int(s) }`,
			fn:   "Arr",
			want: map[string]string{"s": heapNote},
		}, {
			name: "a slice converted to an array pointer keeps the pointer it held",
			src:  `func ArrPtr(s []*int) *[2]*int { return (*[2]*int)(s) }`,
			fn:   "ArrPtr",
			want: map[string]string{"s": result0},
		},

		// Stage 2: the operations the walk gained.
		{
			name: "a range that only reads its operand is proved",
			src: `func RangeRead(s []*int) int {
	n := 0
	for _, v := range s {
		if v != nil {
			n++
		}
	}
	return n
}`,
			fn:   "RangeRead",
			want: map[string]string{"s": noFlow},
		}, {
			name: "a range whose element reaches a global leaks",
			src: `func RangeKeep(s []*int) {
	for _, v := range s {
		gp = v
	}
}`,
			fn:   "RangeKeep",
			want: map[string]string{"s": heapNote},
		}, {
			name: "a range element is one dereference down, so returning it is refused",
			src: `func RangeRet(s []*int) *int {
	for _, v := range s {
		return v
	}
	return nil
}`,
			fn:   "RangeRet",
			want: map[string]string{"s": heapNote},
		}, {
			name: "a range over a map is proved",
			src: `func RangeMap(m map[string]int) int {
	n := 0
	for range m {
		n++
	}
	return n
}`,
			fn:   "RangeMap",
			want: map[string]string{"m": noFlow},
		}, {
			name: "a range over a string is proved",
			src: `func RangeStr(s string) int {
	n := 0
	for _, c := range s {
		n += int(c)
	}
	return n
}`,
			fn:   "RangeStr",
			want: map[string]string{"s": noFlow},
		}, {
			name: "a range beside a parameter the walk refuses still proves the other",
			src: `func RangeBeside(s []*int, ch chan int) int {
	n := 0
	for range s {
		n++
	}
	ch <- n
	return n
}`,
			fn:   "RangeBeside",
			want: map[string]string{"s": noFlow, "ch": heapNote},
		}, {
			name: "a type switch that only reads is proved",
			src: `func TypeSw(i any) int {
	switch v := i.(type) {
	case *T:
		return v.A
	}
	return 0
}`,
			fn:   "TypeSw",
			want: map[string]string{"i": noFlow},
		}, {
			name: "a type switch variable returned is at the interface's own depth",
			src: `func TypeSwRet(i any) *int {
	switch v := i.(type) {
	case *int:
		return v
	}
	return nil
}`,
			fn:   "TypeSwRet",
			want: map[string]string{"i": result0},
		}, {
			name: "a type switch variable stored in a global leaks",
			src: `func TypeSwKeep(i any) {
	switch v := i.(type) {
	case *int:
		gp = v
	}
}`,
			fn:   "TypeSwKeep",
			want: map[string]string{"i": heapNote},
		}, {
			name: "a type assertion that only reads is proved",
			src:  `func Assert(i any) int { return i.(*T).A }`,
			fn:   "Assert",
			want: map[string]string{"i": noFlow},
		}, {
			name: "a type assertion returned is at the interface's own depth",
			src:  `func AssertRet(i any) *int { return i.(*int) }`,
			fn:   "AssertRet",
			want: map[string]string{"i": result0},
		}, {
			name: "a type assertion to a value type is one dereference down, so it is refused",
			src:  `func AssertVal(i any) T { return i.(T) }`,
			fn:   "AssertVal",
			want: map[string]string{"i": heapNote},
		}, {
			name: "a struct literal built in the frame is proved",
			src: `func Lit(p *int) int {
	t := T{A: 1, P: p}
	return *t.P
}`,
			fn:   "Lit",
			want: map[string]string{"p": noFlow},
		}, {
			name: "a struct literal returned carries the parameter to that result",
			src:  `func LitRet(p *int) T { return T{A: 1, P: p} }`,
			fn:   "LitRet",
			want: map[string]string{"p": result0},
		}, {
			name: "an array literal built in the frame is proved",
			src: `func ArrLit(p *int) int {
	a := [2]*int{p, nil}
	return *a[0]
}`,
			fn:   "ArrLit",
			want: map[string]string{"p": noFlow},
		}, {
			name: "a keyed array literal is proved",
			src: `func KeyLit(p *int) int {
	a := [4]*int{2: p}
	return *a[2]
}`,
			fn:   "KeyLit",
			want: map[string]string{"p": noFlow},
		}, {
			name: "a slice literal leaks, because ir/lower.go allocates it",
			src: `func SliceLit(p *int) int {
	s := []*int{p}
	return *s[0]
}`,
			fn:   "SliceLit",
			want: map[string]string{"p": heapNote},
		}, {
			name: "the address of a struct literal leaks, because ir/lower.go allocates it",
			src: `func AddrLit(p *int) int {
	t := &T{A: 1, P: p}
	return *t.P
}`,
			fn:   "AddrLit",
			want: map[string]string{"p": heapNote},
		},

		// Stage 2: the round trip through a word the collector does not trace.
		{
			name: "a uintptr held in a variable ends the flow, which is unsafe's own rule",
			src: `func NoEscape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}`,
			fn:   "NoEscape",
			want: map[string]string{"p": noFlow},
		}, {
			name: "a round trip inside one expression keeps the flow",
			src:  `func NoEscapeInline(p unsafe.Pointer) unsafe.Pointer { return unsafe.Pointer(uintptr(p) ^ 0) }`,
			fn:   "NoEscapeInline",
			want: map[string]string{"p": result0},
		}, {
			name: "a round trip inside one expression into a global leaks",
			src:  `func RoundKeep(p *int) { gu = unsafe.Pointer(uintptr(unsafe.Pointer(p))) }`,
			fn:   "RoundKeep",
			want: map[string]string{"p": heapNote},
		}, {
			name: "a round trip through a variable into a global is the shape unsafe calls invalid",
			src: `func RoundVarKeep(p *int) {
	u := uintptr(unsafe.Pointer(p))
	gu = unsafe.Pointer(u)
}`,
			fn:   "RoundVarKeep",
			want: map[string]string{"p": noFlow},
		}, {
			name: "an offset inside one expression keeps the flow",
			src:  `func Offset(p *int) unsafe.Pointer { return unsafe.Pointer(uintptr(unsafe.Pointer(p)) + 8) }`,
			fn:   "Offset",
			want: map[string]string{"p": result0},
		},

		// The fixed point. A flow that runs backwards round a loop is only
		// seen on a later round, so a note written after the first one would
		// describe a shallower flow than the body has.
		{
			name: "a result flow found on a later round is still described",
			src: `func Round(p *int) *int {
	var a, b *int
	for i := 0; i < 2; i++ {
		a = b
		b = p
	}
	return a
}`,
			fn:   "Round",
			want: map[string]string{"p": result0},
		}, {
			name: "a leak found on a later round refuses the parameter",
			src: `func RoundLeak(p *int) {
	var a, b *int
	for i := 0; i < 2; i++ {
		gp = a
		a = b
		b = p
	}
}`,
			fn:   "RoundLeak",
			want: map[string]string{"p": heapNote},
		}, {
			name: "a deeper flow found before a shallower one takes the shallower depth",
			src: `func Shallow(q **int) *int {
	var a *int
	for i := 0; i < 2; i++ {
		a = *q
	}
	_ = a
	return nil
}`,
			fn:   "Shallow",
			want: map[string]string{"q": noFlow},
		}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn := buildFunc(t, c.src, c.fn)
			got := byName(t, fn, Params(fn, Directives{}))
			for name, want := range c.want {
				if got[name] != want {
					t.Errorf("the note for %s is %q, want %q", name, got[name], want)
				}
			}
			if len(got) != len(c.want) {
				t.Errorf("the function has %d notes and the case states %d", len(got), len(c.want))
			}
		})
	}
}

// TestBodylessNotes covers the branch that needs no analysis, because the
// declaration has nothing to analyse.
func TestBodylessNotes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		fn   string
		d    Directives
		want map[string]string
	}{{
		name: "a //go:noescape declaration asserts what gc cannot check",
		src:  "func Asm(p *int)",
		fn:   "Asm",
		d:    Directives{Noescape: true},
		want: map[string]string{"p": noescapeNote},
	}, {
		name: "a declaration without the directive is assumed to leak",
		src:  "func Asm2(p *int)",
		fn:   "Asm2",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a uintptr parameter is left for the caller to hold live",
		src:  "func AsmU(p uintptr)",
		fn:   "AsmU",
		d:    Directives{Noescape: true},
		want: map[string]string{"p": heapNote},
	}, {
		name: "a scalar parameter is not tagged",
		src:  "func AsmS(n int)",
		fn:   "AsmS",
		d:    Directives{Noescape: true},
		want: map[string]string{"n": heapNote},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn := buildFunc(t, c.src, c.fn)
			if !fn.Bodyless {
				t.Fatalf("%s has a body, so it does not exercise this branch", c.fn)
			}
			got := byName(t, fn, Params(fn, c.d))
			for name, want := range c.want {
				if got[name] != want {
					t.Errorf("the note for %s is %q, want %q", name, got[name], want)
				}
			}
		})
	}
}

// TestUintptrEscapesTakesTheEmptyNote records that a directive nanogo refuses
// to compile also gets no note.
//
// driver.LifetimeDirective refuses the function before it reaches the back
// end, so this branch writes for a build that does not finish. It stands
// because a later change that stops refusing must not make the notes optimistic
// by omission.
func TestUintptrEscapesTakesTheEmptyNote(t *testing.T) {
	fn := buildFunc(t, `func Keep(p *int) int { return *p }`, "Keep")
	got := byName(t, fn, Params(fn, Directives{UintptrEscapes: true}))
	if got["p"] != heapNote {
		t.Errorf("the note for p is %q, want the conservative one", got["p"])
	}
}

// TestNotesAreInReceiverFirstOrder is the alignment gc reads them in.
func TestNotesAreInReceiverFirstOrder(t *testing.T) {
	fn := buildFunc(t, `func (t *T) M(a *int, b []byte) int { gp = a; return t.A + len(b) }`, "(*T).M")
	got := Params(fn, Directives{})
	want := []string{noFlow, heapNote, noFlow}
	if len(got) != len(want) {
		t.Fatalf("got %d notes, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("note %d is %q, want %q (the receiver comes first)", i, got[i], want[i])
		}
	}
}

// TestEncodeMatchesGC checks the two encodings the writer emits against the
// bytes read out of gc's own archives.
func TestEncodeMatchesGC(t *testing.T) {
	var empty leaks
	if got := empty.Encode(); got != "esc:" {
		t.Errorf("the note for a value that flows nowhere is %q, want %q", got, "esc:")
	}
	var heap leaks
	heap.AddHeap(0)
	if got := heap.Encode(); got != "" {
		t.Errorf("the note for a value that leaks to the heap is %q, want the empty string", got)
	}
	var noesc leaks
	noesc.AddMutator(0)
	noesc.AddCallee(0)
	if got := noesc.Encode(); got != noescapeNote {
		t.Errorf("the //go:noescape note is %q, want %q", got, noescapeNote)
	}
	// gc's own Optimize: a destination reached no sooner than the heap adds
	// nothing, so it is dropped before the bytes are written.
	var both leaks
	both.AddHeap(1)
	both.AddResult(0, 1)
	both.Optimize()
	if got := both.Result(0); got != -1 {
		t.Errorf("a result flow at the heap's own depth survived Optimize: %d", got)
	}
}

// byName pairs each note with the parameter it belongs to.
func byName(t *testing.T, fn *ir.Func, notes []string) map[string]string {
	t.Helper()
	names := make([]string, 0, len(fn.Params)+1)
	if fn.Recv != nil {
		names = append(names, fn.Recv.Name)
	}
	for _, p := range fn.Params {
		names = append(names, p.Name)
	}
	if len(names) != len(notes) {
		t.Fatalf("%d parameters and %d notes", len(names), len(notes))
	}
	out := make(map[string]string, len(names))
	for i, n := range names {
		out[n] = notes[i]
	}
	return out
}

// buildFunc builds one declaration of the prelude plus src and returns it.
func buildFunc(t *testing.T, src, sym string) *ir.Func {
	t.Helper()
	pkg, files, info := typecheck(t, notePrelude+"\n"+src+"\n")
	p, err := ir.Build(pkg, files, info)
	if err != nil {
		t.Fatalf("ir.Build: %v", err)
	}
	want := "p." + sym
	for _, fn := range p.Funcs {
		if fn.Sym == want {
			return fn
		}
	}
	t.Fatalf("the package has no function %s", want)
	return nil
}

func typecheck(t *testing.T, src string) (*types2.Package, []*syntax.File, *types2.Info) {
	t.Helper()
	fset := syntax.NewFileSet()
	file, err := syntax.Parse(fset.AddFile("x.go", len(src)), []byte(src), nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	files := []*syntax.File{file}
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{
		Fset:     fset,
		Importer: unsafeImporter{},
		Sizes:    types2.SizesFor("gc", "arm64"),
	}
	pkg, err := conf.Check("p", files, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	return pkg, files, info
}

// unsafeImporter resolves package unsafe and nothing else, so that a unit test
// here does not depend on a GOROOT.
type unsafeImporter struct{}

func (unsafeImporter) Import(path string) (*types2.Package, error) {
	if path == "unsafe" {
		return types2.Unsafe, nil
	}
	return nil, errNoImporter{path}
}

type errNoImporter struct{ path string }

func (e errNoImporter) Error() string { return "no importer for " + e.path }
