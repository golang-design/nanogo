// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"fmt"
	"strings"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtsym"
	"golang.design/x/nanogo/ssa"
)

// Relocations, per specs/041-instruction-encoding.md.
//
// Every reference to a symbol is a relocation: the compiler encodes a zero
// displacement and names the target, and the linker patches the instruction.
// The type says which bits of which instruction the linker edits, so a wrong
// type produces a binary that jumps somewhere plausible. That is why the tests
// disassemble the object rather than only comparing bytes.
//
// Two types are enough for the arm64 rule groups 1 to 5:
//
//   - R_CALLARM64 patches the 26-bit immediate of a BL. The linker also uses
//     it to decide where a trampoline is needed, which is why a call that is
//     too far is the linker's problem and not the compiler's.
//   - R_ADDRARM64 patches an ADRP and ADD pair, which is why its size is
//     eight and not four. The pair computes an address in two instructions and
//     the linker splits the value between them; two separate relocations would
//     let the linker patch one and not the other.

// callee names the symbol a call goes to and the ABI it is defined with.
//
// The ABI is part of a symbol's identity (specs/030-abi.md): the same name
// exists once per ABI and the linker keeps them apart. A reference with the
// wrong ABI resolves to nothing, or to a wrapper, so it is carried here rather
// than defaulted.
type callee struct {
	name string
	abi  uint16
}

// morestackName is the runtime function the stack-growth tail calls.
//
// Neither symbol has a Go declaration: both exist only in
// runtime/asm_<arch>.s. rtsym carries them with Assembly set, so the names are
// checked against the runtime's own source there rather than typed in here,
// which is what specs/031-runtime-lowering.md requires of every runtime symbol
// the compiler generates a call to.
//
// The choice between the two is a correctness one and not a name.
// runtime.morestack_noctxt writes zero into the context register before it
// grows the stack, so a function that reads a capture out of that register
// resumes with a nil closure. runtime.morestack saves the register into
// g.sched.ctxt and the resumed function finds its closure again. The reverse
// is wrong for the same reason: g.sched.ctxt is scanned by the collector, and
// a function with no closure leaves whatever its caller left in the register,
// so it must call the form that clears it.
func morestackName(needCtxt bool) string {
	if needCtxt {
		return "runtime.morestack"
	}
	return "runtime.morestack_noctxt"
}

// morestackCallee returns the callee record for the stack-growth call, and
// reports whether rtsym has the symbol.
//
// The ABI is not rtsym's to state and it is not a default either. The symbol
// is defined in assembly, which cmd/internal/obj looks up in the ABI0 table,
// so a reference under ABIInternal names a symbol that does not exist and the
// call reaches nothing. Assembly is what justifies the ABI, so a table entry
// without it is refused rather than called.
func morestackCallee(needCtxt bool) (callee, bool) {
	s := rtsym.Lookup(morestackName(needCtxt))
	if s == nil || !s.Assembly {
		return callee{}, false
	}
	return callee{name: s.Name, abi: obj.ABI0}, true
}

// runtimeCallee returns the callee record for a runtime symbol, and reports
// whether rtsym knows the name.
//
// Every runtime call the rules emit goes through rtsym, so a name that drifts
// from the runtime is a test failure there rather than a crash at run time.
func runtimeCallee(name string) (callee, bool) {
	if s := rtsym.Lookup(name); s != nil {
		return callee{name: s.Name, abi: obj.ABIInternal}, true
	}
	return callee{}, false
}

// symbols resolves a symbol name to the reference a relocation carries.
//
// The reference is an index pair, not a name, so the same name must always
// return the same index. A name defined in this package resolves to its
// definition; anything else becomes a non-package reference, which the linker
// resolves by name. Package-indexed references need the symbol index the
// export data of the other package assigns, and specs/015-export-data.md is
// what will supply it; until then the by-name form is the one that is always
// correct.
type symbols struct {
	pkg *obj.Package

	// index and data are lookup tables and are never ranged over, so they
	// produce no output order (specs/053-determinism.md).
	index map[callee]obj.SymRef
	data  map[string]obj.SymRef
}

func newSymbols(p *obj.Package) *symbols {
	return &symbols{
		pkg:   p,
		index: make(map[callee]obj.SymRef),
		data:  make(map[string]obj.SymRef),
	}
}

// ref returns the reference of a symbol, adding a reference to the object the
// first time the name is seen.
func (s *symbols) ref(c callee) obj.SymRef {
	if r, ok := s.index[c]; ok {
		return r
	}
	r := s.pkg.AddNonPkgRef(&obj.Symbol{Name: c.name, ABI: c.abi})
	// go tool nm and go tool objdump print a name only for a symbol the
	// RefName block covers, and the disassembly test reads that name.
	s.pkg.AddRefName(r, c.name)
	s.index[c] = r
	return r
}

// stringData returns the reference to the bytes of a string constant, defining
// the symbol in this object the first time the constant is seen.
//
// It is a definition and not a reference: the bytes exist nowhere else, so
// every object that names a constant carries a copy of it. It is
// content-addressable, which is what makes the copies one symbol in the linked
// binary. The linker merges them on the sixteen-byte content hash, and the
// name is what a symbol table shows and what gc writes for the same constant
// (specs/040-object-format.md).
//
// gc marks the symbol DUPOK, RODATA and LOCAL, and this does the same. RODATA
// is not a preference: a string is immutable, so its bytes belong in the
// read-only section, where a write to them faults instead of corrupting every
// other holder of the same constant.
func (s *symbols) stringData(sym *ssa.StringSym) (obj.SymRef, error) {
	if sym == nil || sym.Obj == nil || sym.Obj.Name == "" {
		return obj.SymRef{}, fmt.Errorf("a string constant with no symbol")
	}
	name := sym.Obj.Name
	if r, ok := s.data[name]; ok {
		return r, nil
	}
	r := s.pkg.AddHashedDef(&obj.Symbol{
		Name: name,
		Type: obj.SRODATA,
		Flag: obj.SymFlagDupok | obj.SymFlagLocal,
		Size: uint32(len(sym.Text)),
		// The bytes of a string are read one at a time and need no
		// alignment. obj refuses a content-addressable symbol with none,
		// because the linker places such a symbol itself.
		Align: 1,
		Data:  []byte(sym.Text),
	})
	s.data[name] = r
	return r, nil
}

// callTarget returns the callee a static call value names.
//
// The symbol is in Aux as an *ir.Object, which is what specs/021's call
// operations carry and what the lowering rules preserve. A call with no
// symbol is a compiler bug: an indirect call has a different operation.
func callTarget(aux any) (callee, error) {
	o, ok := aux.(*ir.Object)
	if !ok || o == nil || o.Name == "" {
		return callee{}, fmt.Errorf("static call has no callee symbol")
	}
	if c, ok := runtimeCallee(o.Name); ok {
		return c, nil
	}
	if o.Assembly {
		// The callee is defined in assembly under ABI0, and the ABI is half
		// of a symbol's identity: the ABIInternal symbol of the same name is
		// the wrapper this call sits inside (specs/047-abi-wrappers.md). A
		// reference under the wrong one resolves to the wrapper and the
		// wrapper calls itself.
		return callee{name: o.Name, abi: obj.ABI0}, nil
	}
	return callee{name: o.Name, abi: obj.ABIInternal}, nil
}

// symbolName returns the linker name of an object a value addresses.
//
// A function object carries its linker symbol, because ir.Build derives one
// for a declaration. A package-level variable does not: ir.Object holds the
// name as written, so the package it belongs to has to be put back. This
// package's own path is the only one available, which is right for a variable
// of the package being compiled and produces an undefined symbol at link time
// for one that is imported. That is a gap in specs/020-ir.md's object model
// rather than a decision here: a global needs a linker symbol exactly as a
// function does.
//
// A type descriptor is the exception, and it is not a special case of the
// rule: its name is not a Go identifier at all. specs/032 requires the name to
// be a function of the type alone, so that two packages that name one type
// produce one symbol the linker merges. type:p.T survives the rule by
// accident, because it holds a dot, and type:int, type:[]int and
// type:interface {} do not: each would become main.type:int, a symbol nothing
// defines and cmd/link never collects into runtime.typelinks.
func (e *emitter) symbolName(o *ir.Object) string {
	if strings.HasPrefix(o.Name, ir.TypeSymbolPrefix) {
		return o.Name
	}
	if strings.Contains(o.Name, ".") {
		return o.Name
	}
	return e.pkg.Path + "." + o.Name
}

// globalCallee returns the reference an address of an object names.
//
// The ABI is half of a symbol's identity and cmd/link resolves a by-name
// reference by name and ABI together: its abiToVer maps ABIInternal to one
// version and everything a data symbol carries to another. gc gives a data
// symbol no ABI, so a package-level variable and a type descriptor are both
// ABI0, and a reference to one under ABIInternal names a symbol nothing
// defines. Only a text symbol is ABIInternal.
func (e *emitter) globalCallee(o *ir.Object) callee {
	if o.Class == ir.ClassGlobal || o.Assembly {
		return callee{name: e.symbolName(o), abi: obj.ABI0}
	}
	return callee{name: e.symbolName(o), abi: obj.ABIInternal}
}

// call records the relocation of a BL at off.
func (e *emitter) call(off int32, c callee) {
	e.relocs = append(e.relocs, obj.Reloc{
		Off:  off,
		Size: 4,
		Type: obj.R_CALLARM64,
		Sym:  e.syms.ref(c),
	})
}

// addr records the relocation of an ADRP and ADD pair at off.
//
// The size is eight because the pair is one edit: the linker computes the
// page of the target for the ADRP and the offset inside the page for the ADD,
// and it needs both instructions to do it.
func (e *emitter) addr(off int32, ref obj.SymRef, add int64) {
	e.relocs = append(e.relocs, obj.Reloc{
		Off:  off,
		Size: 8,
		Type: obj.R_ADDRARM64,
		Add:  add,
		Sym:  ref,
	})
}

// addrTarget returns the reference an address value names.
//
// Two kinds of symbol reach it. An *ir.Object is a declaration: a
// package-level variable, a function or a type descriptor, all of which are
// resolved by name because the definition is somewhere the linker will find.
// An *ssa.StringSym is a constant with no declaration, so the bytes are
// defined here and the reference is to that definition.
//
// A value that carries neither is a compiler bug and not a program error, so
// it fails the emit rather than being encoded against symbol zero, which the
// linker would resolve to nothing and the program would jump into.
func (e *emitter) addrTarget(v *ssa.Value) (obj.SymRef, bool) {
	switch aux := v.Aux.(type) {
	case *ir.Object:
		if aux == nil {
			break
		}
		return e.syms.ref(e.globalCallee(aux)), true
	case *ssa.StringSym:
		ref, err := e.syms.stringData(aux)
		if err != nil {
			e.fail("v%d: %v", v.ID, err)
			return obj.SymRef{}, false
		}
		return ref, true
	}
	e.fail("v%d: an address of no symbol", v.ID)
	return obj.SymRef{}, false
}
