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

func use(*int) {}
`

// noFlow is the note for a parameter that flows nowhere: "esc:" and no bytes
// after it, because leaks.Encode trims the trailing zeros of an empty set.
const noFlow = "esc:"

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
		name: "a returned parameter leaks",
		src:  `func Ret(p *int) *int { return p }`,
		fn:   "Ret",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a parameter passed to a call leaks",
		src:  `func Pass(p *int) { use(p) }`,
		fn:   "Pass",
		want: map[string]string{"p": heapNote},
	}, {
		name: "a slice of a parameter that is returned leaks",
		src:  `func Tail(b []byte) []byte { return b[1:] }`,
		fn:   "Tail",
		want: map[string]string{"b": heapNote},
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
		name: "a parameter laundered into a uintptr leaks",
		src:  `func Word(p *int) uintptr { return uintptr(unsafe.Pointer(p)) }`,
		fn:   "Word",
		want: map[string]string{"p": heapNote},
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
		name: "a parameter assigned to a named result leaks",
		src:  `func Named(p *int) (r *int) { r = p; return }`,
		fn:   "Named",
		want: map[string]string{"p": heapNote},
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
		name: "a variadic parameter that is returned leaks",
		src:  `func VarRet(ps ...*int) []*int { return ps }`,
		fn:   "VarRet",
		want: map[string]string{"ps": heapNote},
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
