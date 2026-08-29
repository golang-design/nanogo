// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtsym"
	"golang.design/x/nanogo/ssa"
)

// TestMorestackComesFromRtsym checks that the stack-growth tail names the
// symbol rtsym carries rather than a literal of its own.
//
// specs/031-runtime-lowering.md requires every runtime symbol the compiler
// generates a call to be checked against the runtime's source, and rtsym is
// where that check lives. The symbol has no Go declaration, so rtsym marks it
// Assembly and checks that the runtime's assembly defines it; this test checks
// that the emitter reads that record, because a literal here would not move
// when the runtime does.
func TestMorestackComesFromRtsym(t *testing.T) {
	s := rtsym.Lookup(morestackName(false))
	if s == nil {
		t.Fatalf("rtsym does not carry %s", morestackName(false))
	}
	if !s.Assembly {
		t.Errorf("%s is not marked Assembly, and only that justifies calling it at ABI0", s.Name)
	}
	c, ok := morestackCallee(false)
	if !ok {
		t.Fatal("the emitter found no callee for the stack-growth tail")
	}
	if c.name != s.Name {
		t.Errorf("the tail calls %q and rtsym names %q", c.name, s.Name)
	}
	// The runtime defines the symbol in assembly, which cmd/internal/obj
	// looks up in the ABI0 table.
	if c.abi != obj.ABI0 {
		t.Errorf("the tail calls %s at ABI %d, want ABI0", c.name, c.abi)
	}
	comparisons++
}

// TestMorestackKeepsTheContextRegister checks the stack-growth symbol a
// function that reads the context register calls.
//
// The two runtime symbols differ in one instruction and the difference is a
// correctness one. runtime.morestack_noctxt writes zero into the register
// before the growth, so a closure that grew its stack resumes with a nil
// closure object and faults on its first capture. runtime.morestack saves the
// register into g.sched.ctxt and the growth is invisible to the function. The
// reverse is wrong for the same field: g.sched.ctxt is scanned by the
// collector, so a function with no closure must call the form that clears it.
func TestMorestackKeepsTheContextRegister(t *testing.T) {
	const src = "package main\n\nfunc g(a int) int\n\nfunc f(a int) int { return g(a) }\n"
	for _, tc := range []struct {
		ctxt bool
		want string
	}{
		{false, "runtime.morestack_noctxt"},
		{true, "runtime.morestack"},
	} {
		c := compile(t, src, "f")
		c.f.NeedCtxt = tc.ctxt
		p := obj.NewPackage("main")
		r := emit(t, c, p)
		text := disassemble(t, r, p)
		want := "R_CALLARM64:" + tc.want
		if !strings.Contains(text, want) {
			t.Errorf("NeedCtxt=%v: the disassembly does not hold %s:\n%s", tc.ctxt, want, text)
		}
		if tc.ctxt && strings.Contains(text, "R_CALLARM64:runtime.morestack_noctxt") {
			t.Errorf("a function that reads the context register calls the form that clears it:\n%s", text)
		}
	}
	comparisons++
}

// TestRuntimeCalleeComesFromRtsym checks that a call to a runtime function
// that lowering emits is looked up rather than spelled.
func TestRuntimeCalleeComesFromRtsym(t *testing.T) {
	c, ok := runtimeCallee("runtime.newobject")
	if !ok {
		t.Fatal("rtsym does not know runtime.newobject")
	}
	if c.name != "runtime.newobject" || c.abi != obj.ABIInternal {
		t.Errorf("callee %+v, want runtime.newobject at ABIInternal", c)
	}
	if _, ok := runtimeCallee("runtime.notAFunction"); ok {
		t.Error("rtsym claims to know a symbol that is not in it")
	}
	// A call to a runtime symbol reaches the same record whether the rule
	// spelled the prefix or not, because callTarget asks rtsym first.
	got, err := callTarget(&ir.Object{Name: "runtime.newobject"})
	if err != nil || got != c {
		t.Errorf("callTarget gave %+v, %v", got, err)
	}
	if _, err := callTarget(nil); err == nil {
		t.Error("a call with no symbol was accepted")
	}
}

// TestSymbolReferencesAreInterned checks that one name produces one reference.
//
// A relocation names a symbol by index, so two calls to one function must
// resolve to one index. A second reference would be a second entry in the
// object and a different byte sequence for the same program, which
// specs/053-determinism.md forbids.
func TestSymbolReferencesAreInterned(t *testing.T) {
	p := obj.NewPackage("main")
	s := newSymbols(p)
	a := s.ref(callee{name: "main.g", abi: obj.ABIInternal})
	b := s.ref(callee{name: "main.g", abi: obj.ABIInternal})
	if a != b {
		t.Errorf("one name gave two references, %v and %v", a, b)
	}
	// The ABI is part of a symbol's identity, so the same name under two of
	// them is two symbols.
	c := s.ref(callee{name: "main.g", abi: obj.ABI0})
	if c == a {
		t.Error("the same name under two ABIs gave one reference")
	}
}

// TestCallRelocations checks the offsets and the types against the
// disassembler.
//
// A call is a BL with a zero displacement and a relocation that names the
// callee, so the check is that the relocation sits on the BL and that objdump
// resolves it to the name. A relocation one instruction out produces a program
// that jumps into the middle of the prologue.
func TestCallRelocations(t *testing.T) {
	c := compile(t, "package main\n\nfunc g(a int) int\n\nfunc f(a int) int { return g(a) }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)

	if len(r.Text.Relocs) != 2 {
		t.Fatalf("%d relocations, want two: the call and the growth tail", len(r.Text.Relocs))
	}
	for _, rel := range r.Text.Relocs {
		if rel.Type != obj.R_CALLARM64 {
			t.Errorf("relocation at %d has type %v, want R_CALLARM64", rel.Off, rel.Type)
		}
		if rel.Size != 4 {
			t.Errorf("relocation at %d is %d bytes, and a call immediate is four", rel.Off, rel.Size)
		}
		if rel.Off%4 != 0 {
			t.Errorf("relocation at %d is not on an instruction boundary", rel.Off)
		}
		// The instruction the relocation covers must be the branch with the
		// link, with no displacement of its own.
		if w := words(r.Text)[rel.Off/4]; w != 0x94000000 {
			t.Errorf("the instruction at %d is %#08x, and a call with no displacement is 0x94000000", rel.Off, w)
		}
	}
	// go tool objdump prints the relocation and the name it resolves to, so
	// the type and the target are checked by the disassembler rather than by
	// this package's own record of them.
	text := disassemble(t, r, p)
	for _, want := range []string{"R_CALLARM64:main.g", "R_CALLARM64:" + morestackName(false)} {
		if !strings.Contains(text, want) {
			t.Errorf("the disassembly does not hold %s:\n%s", want, text)
		}
	}
}

// TestAddressRelocations checks the address of a global.
//
// An address is an ADRP and an ADD, and the linker splits one value across
// them, so it is one relocation of eight bytes and not two of four. Two
// relocations would let the linker patch one instruction and not the other,
// and the address would be the page of the symbol with the offset of whatever
// the ADD held.
func TestAddressRelocations(t *testing.T) {
	c := compile(t, "package main\n\nvar G int\n\nfunc f() int { return G }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	n := 0
	for _, rel := range r.Text.Relocs {
		if rel.Type != obj.R_ADDRARM64 {
			continue
		}
		if rel.Size != 8 {
			t.Errorf("the address relocation is %d bytes, and it covers an ADRP and an ADD", rel.Size)
		}
		w := words(r.Text)
		if got := w[rel.Off/4] & 0x9f000000; got != 0x90000000 {
			t.Errorf("the instruction at %d is %#08x, which is not an ADRP", rel.Off, w[rel.Off/4])
		}
		n++
	}
	if n != 1 {
		t.Fatalf("%d address relocations, want 1", n)
	}
	text := disassemble(t, r, p)
	if !strings.Contains(text, "R_ADDRARM64:main.G") {
		t.Errorf("the disassembly does not name the global:\n%s", text)
	}
}

// TestTypeDescriptorNamesSurviveThePrefix is the third change of specs/032's
// seam.
//
// symbolName puts this package's path back onto a name with no dot in it,
// because ir.Object holds a package-level variable's name as written. A type
// descriptor's name is not a Go identifier and is already a linker symbol, so
// the rule does not apply to it: type:p.T survived by accident because it
// holds a dot, and type:int, type:[]int and type:interface {} became
// main.type:int and friends, symbols nothing defines and cmd/link never
// collects into runtime.typelinks.
func TestTypeDescriptorNamesSurviveThePrefix(t *testing.T) {
	// The name and the ABI are decided by the package path alone, so the
	// emitter needs nothing else.
	e := &emitter{pkg: obj.NewPackage("test")}
	for _, name := range []string{"type:int", "type:[]int", "type:interface {}", "type:*int", "type:p.T"} {
		if got := e.symbolName(&ir.Object{Name: name, Class: ir.ClassGlobal}); got != name {
			t.Errorf("the descriptor %q became %q", name, got)
		}
	}
	// The rule the exception is carved out of still holds.
	if got := e.symbolName(&ir.Object{Name: "G", Class: ir.ClassGlobal}); got != "test.G" {
		t.Errorf("a package-level name became %q, want test.G", got)
	}
}

// TestGlobalReferencesAreABI0 checks the half of a symbol's identity that is
// not its name.
//
// cmd/link resolves a by-name reference by name and ABI together, so a data
// symbol referenced under ABIInternal names a symbol nothing defines and the
// link reports the descriptor as undefined. gc gives a data symbol no ABI,
// which is ABI0.
func TestGlobalReferencesAreABI0(t *testing.T) {
	// The name and the ABI are decided by the package path alone, so the
	// emitter needs nothing else.
	e := &emitter{pkg: obj.NewPackage("test")}
	if c := e.globalCallee(&ir.Object{Name: "type:int", Class: ir.ClassGlobal}); c.abi != obj.ABI0 {
		t.Errorf("a type descriptor is referenced at ABI %d, want ABI0", c.abi)
	}
	if c := e.globalCallee(&ir.Object{Name: "G", Class: ir.ClassGlobal}); c.name != "test.G" || c.abi != obj.ABI0 {
		t.Errorf("a package-level variable is referenced as %+v, want test.G at ABI0", c)
	}
	// A text symbol keeps ABIInternal, which is what specs/030-abi.md gives
	// every function nanogo compiles.
	if c := e.globalCallee(&ir.Object{Name: "main.f", Class: ir.ClassFunc}); c.abi != obj.ABIInternal {
		t.Errorf("a function is referenced at ABI %d, want ABIInternal", c.abi)
	}
}

// TestStringConstantDefinesItsBytes checks that the address of a string
// constant reaches a definition in this object.
//
// A string constant has no declaration, so there is no *ir.Object for it and
// no symbol anywhere else to resolve the name against. The bytes are defined
// here, content-addressably, and the relocation names that definition. Before
// this the emitter refused the address with "an address of no symbol", which
// is the whole reason a program that prints a literal did not compile.
func TestStringConstantDefinesItsBytes(t *testing.T) {
	c := compile(t, "package main\n\nfunc f() string { return \"hi\" }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)

	var data *obj.Symbol
	for _, rel := range r.Text.Relocs {
		if rel.Type != obj.R_ADDRARM64 {
			continue
		}
		if rel.Sym.PkgIdx != obj.PkgIdxHashed {
			t.Errorf("the address of a string constant names index space %d, want the content-addressable one", rel.Sym.PkgIdx)
			continue
		}
		data = p.Def(rel.Sym)
	}
	if data == nil {
		t.Fatalf("no address relocation reached a definition; the relocations are %+v", r.Text.Relocs)
	}
	// The name is gc's, because the linker merges two definitions of one
	// constant by name and by content hash together.
	if got, want := data.Name, `go:string."hi"`; got != want {
		t.Errorf("the symbol is named %q, want %q", got, want)
	}
	if got, want := string(data.Data), "hi"; got != want {
		t.Errorf("the symbol holds %q, want %q", got, want)
	}
	if data.Size != 2 {
		t.Errorf("the symbol has size %d, want 2", data.Size)
	}
	// Read-only, because a string is immutable and every holder of the
	// constant shares these bytes.
	if data.Type != obj.SRODATA {
		t.Errorf("the symbol is of kind %d, want SRODATA", data.Type)
	}
	// gc marks the symbol DUPOK and LOCAL. obj refuses a content-addressable
	// symbol with no alignment, because the linker places it itself.
	if data.Flag&obj.SymFlagDupok == 0 || data.Flag&obj.SymFlagLocal == 0 {
		t.Errorf("the symbol carries flags %#x, want dupok and local", data.Flag)
	}
	if data.Align != 1 {
		t.Errorf("the symbol is aligned to %d, want 1", data.Align)
	}
}

// TestStringDataIsDefinedOnce checks that one constant named twice in one
// function is one definition.
//
// The symbol is content-addressable, so a second definition would still link.
// It would also make the object hold the bytes twice and make two compiles of
// one input differ in size for no reason.
func TestStringDataIsDefinedOnce(t *testing.T) {
	p := obj.NewPackage("main")
	s := newSymbols(p)
	sym := &ssa.StringSym{Obj: &ir.Object{Name: `go:string."hi"`}, Text: "hi"}
	a, err := s.stringData(sym)
	if err != nil {
		t.Fatalf("stringData: %v", err)
	}
	b, err := s.stringData(&ssa.StringSym{Obj: &ir.Object{Name: `go:string."hi"`}, Text: "hi"})
	if err != nil {
		t.Fatalf("stringData: %v", err)
	}
	if a != b {
		t.Errorf("one constant gave two definitions, %v and %v", a, b)
	}
	// A symbol with no name is a compiler bug, and an unnamed definition in
	// the object would be a payload the linker places itself.
	if _, err := s.stringData(nil); err == nil {
		t.Error("a nil string symbol was accepted")
	}
	if _, err := s.stringData(&ssa.StringSym{}); err == nil {
		t.Error("a string symbol with no object was accepted")
	}
}

// TestAddressOfNoSymbolIsRefused checks that an address value carrying
// something the emitter cannot resolve fails the emit.
//
// It is a compiler bug and not a program error. Encoding it against symbol
// zero would produce an instruction the linker resolves to nothing, and the
// program would compute an address in whatever the linker left there.
func TestAddressOfNoSymbolIsRefused(t *testing.T) {
	for _, aux := range []any{nil, (*ir.Object)(nil), "not a symbol", 42} {
		p := obj.NewPackage("main")
		e := &emitter{opt: Options{Sym: "main.f"}, pkg: p, syms: newSymbols(p)}
		v := &ssa.Value{ID: 7, Op: ssa.OpARM64MOVDaddr, Aux: aux}
		if _, ok := e.addrTarget(v); ok {
			t.Errorf("an address with Aux %T was accepted", aux)
		}
		if err := e.err(); err == nil || !strings.Contains(err.Error(), "v7") {
			t.Errorf("the failure of Aux %T does not name the value: %v", aux, err)
		}
	}
	// A string symbol with no object is the same class of bug, and it must not
	// become an unnamed definition the linker places itself.
	p := obj.NewPackage("main")
	e := &emitter{opt: Options{Sym: "main.f"}, pkg: p, syms: newSymbols(p)}
	v := &ssa.Value{ID: 9, Op: ssa.OpARM64MOVDaddr, Aux: &ssa.StringSym{Text: "hi"}}
	if _, ok := e.addrTarget(v); ok {
		t.Error("a string symbol with no object was accepted")
	}
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "v9") {
		t.Errorf("the failure does not name the value: %v", err)
	}
}

// TestReflectMethodMarksTheCallingFunction checks the flag that stops cmd/link
// from pruning a method only reflect can find.
//
// Without it the linker resolves the method's Ifn and Tfn to the sentinel -1,
// runtime.getitab installs runtime.unreachableMethod, and the program dies
// with "unreachable method called. linker bug?" the first time reflect calls
// the method. Go's own test/reflectmethod4.go is exactly that program.
//
// The decision is ir.Func.ReflectMethod's, because the question is about the
// selection and an interface call names no symbol this far down. What is
// checked here is that the answer reaches the text symbol.
func TestReflectMethodMarksTheCallingFunction(t *testing.T) {
	c := check(t, wrapperSource)
	for _, tc := range []struct {
		what string
		want bool
	}{
		{"a function that asks reflect for a method", true},
		{"a function that does not", false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			sig := &ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{}}
			if err := ir.Layout(sig); err != nil {
				t.Fatal(err)
			}
			target := &ir.Object{Name: "main.somewhere", Type: sig, Class: ir.ClassFunc}
			fn := &ir.Func{
				Name:          "caller",
				Sym:           "main.caller",
				ReflectMethod: tc.want,
				Body: []ir.Stmt{
					&ir.Node{Op: ir.OCall, Type: voidType(), X: &ir.Node{Op: ir.OGlobal, Type: sig, Obj: target}},
					&ir.Node{Op: ir.OReturn, Type: voidType()},
				},
			}
			r := emitFunc(t, c.build(t, fn), newMainPackage())
			got := r.Text.Flag&obj.SymFlagReflectMethod != 0
			if got != tc.want {
				t.Errorf("the text symbol is marked %v, want %v", got, tc.want)
			}
		})
	}
}
