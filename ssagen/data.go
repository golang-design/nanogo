// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"encoding/binary"
	"fmt"
	"go/constant"
	"math"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/ssa"
)

// Package-level variables, per specs/020-ir.md's object model.
//
// A package-level variable is one symbol in the program. Its size is its
// type's, its contents are its initialiser where the initialiser is a
// constant, and its kind decides three things the linker and the collector
// read: which section it lands in, whether the linker allocates the space or
// copies bytes, and whether the collector scans it.
//
// # The pointer map is the type descriptor
//
// cmd/link builds the pointer bitmap of the data and bss sections from the
// type descriptor each symbol names through an AuxGotype entry, in
// GCProg.AddSym. A symbol in a scanned section with no descriptor is
// "missing Go type information for global symbol", and one in an unscanned
// section needs none. So a variable whose type holds a pointer carries its
// descriptor and lands in SDATA or SBSS, and one whose type holds none lands
// in SNOPTRDATA or SNOPTRBSS and carries nothing.
//
// gc attaches a descriptor to every global, including the ones with no
// pointers in them. nanogo attaches one only where it is read, because
// specs/032-type-descriptors-and-itabs.md's writer refuses a defined type, and
// a descriptor demanded where nothing reads it would refuse a variable this
// compiler can otherwise emit. The cost is that a debugger sees no type for
// such a variable: cmd/link's dwarfGenerateDebugSyms skips a global with no
// descriptor rather than failing.
//
// # What is written and what is assigned
//
// A constant initialiser is written into the symbol's bytes. The assignment
// that ir.Build put in the initialisation function is left in place, so the
// same value is written twice: once by the linker and once at run time. That
// is deliberate. Removing it means rewriting the initialisation function,
// whose ordering is what specs/012-type-checking.md computed, and a static
// value that disagreed with the assignment would be overwritten by the
// assignment anyway.
//
// An initialiser that is not a constant leaves the symbol zero, and the
// assignment in the initialisation function is what gives it its value. That
// function is compiled with every other, so an initialiser this compiler
// cannot generate code for is refused there, by position, rather than
// producing a variable that is silently left zero.

// A GlobalError names the package-level variable nanogo cannot write a data
// symbol for.
//
// The object is carried rather than formatted in, because the caller reports a
// refusal with the source position of the declaration and only it can resolve
// a position to a file and a line (specs/010-scanner-and-positions.md).
type GlobalError struct {
	Obj    *ir.Object
	Reason string
}

func (e *GlobalError) Error() string {
	if e.Obj == nil {
		return e.Reason
	}
	return e.Obj.Name + ": " + e.Reason
}

// AddGlobals defines the data symbol of every package-level variable of p, and
// returns the types whose descriptors those symbols name.
//
// The caller adds the returned types to the set it emits descriptors for. They
// are not emitted here because a package emits one descriptor per type however
// many symbols name it, and the caller is what holds that set.
//
// The order is p.Globals, which ir.Build fills in declaration order. The
// object's symbol table is written from a slice in insertion order, so two
// runs over one input must add the symbols in one order
// (specs/053-determinism.md). Nothing here ranges over a map.
func AddGlobals(out *obj.Package, p *ir.Package) ([]*ir.Type, error) {
	if out == nil || p == nil {
		return nil, fmt.Errorf("ssagen: nil package")
	}
	static := staticInits(p)
	syms := newSymbols(out)
	var types []*ir.Type
	for _, g := range p.Globals {
		t, err := addGlobal(out, syms, g, static[g])
		if err != nil {
			return nil, err
		}
		if t != nil {
			types = append(types, t)
		}
	}
	return types, nil
}

// CheckGlobals reports the first package-level variable of p that nanogo
// cannot write a data symbol for.
//
// It is [AddGlobals] with the object thrown away, so there is one
// implementation of what can be written. The caller runs it before it compiles
// any function, because a refusal that names a variable is worth more than the
// undefined symbol the linker would report much later.
func CheckGlobals(p *ir.Package) error {
	if p == nil {
		return nil
	}
	_, err := AddGlobals(obj.NewPackage(p.Path), p)
	return err
}

// addGlobal defines one variable and returns the type its descriptor is of, or
// nil when it needs none.
func addGlobal(out *obj.Package, syms *symbols, g *ir.Object, init ir.Value) (*ir.Type, error) {
	if g == nil || g.Name == "" {
		return nil, &GlobalError{Obj: g, Reason: "a package-level variable with no symbol"}
	}
	t := g.Type
	if t == nil || t.Kind == ir.Invalid {
		return nil, &GlobalError{Obj: g, Reason: "a package-level variable with no type"}
	}
	if t.Size < 0 {
		return nil, &GlobalError{Obj: g, Reason: "a type of no size, so the linker cannot allocate it"}
	}
	data, relocs, err := staticValue(t, init)
	if err != nil {
		return nil, &GlobalError{Obj: g, Reason: err.Error()}
	}
	sym := &obj.Symbol{
		Name:  g.Name,
		Size:  uint32(t.Size),
		Align: uint32(t.Align),
	}
	if sym.Align == 0 {
		sym.Align = 1
	}
	sym.Type = globalKind(t.HasPointers(), len(relocs) > 0 || anyNonZero(data))
	if !isBSS(sym.Type) {
		// The linker allocates the space of a zero-filled symbol and copies
		// the bytes of every other one, so the two disagree about Data and
		// obj checks that.
		sym.Data = data
	}
	var descriptor *ir.Type
	if t.HasPointers() {
		// The collector reads the pointer map of this symbol through the type
		// descriptor, so the descriptor is mandatory and its absence is a
		// refusal rather than a symbol the collector misreads.
		if _, err := rtype.Descriptor(t); err != nil {
			return nil, &GlobalError{
				Obj:    g,
				Reason: "a variable whose type holds a pointer needs its type descriptor, which the collector reads to find the pointer: " + err.Error(),
			}
		}
		name, err := ir.TypeSymbol(t)
		if err != nil {
			return nil, &GlobalError{Obj: g, Reason: err.Error()}
		}
		// A data symbol is defined with no ABI, and cmd/link resolves a
		// by-name reference by name and ABI together (specs/030-abi.md).
		sym.Aux = []obj.Aux{{Type: obj.AuxGotype, Sym: syms.ref(callee{name: name, abi: obj.ABI0})}}
		descriptor = t
	}
	if err := resolveDataRelocs(syms, sym, relocs); err != nil {
		return nil, &GlobalError{Obj: g, Reason: err.Error()}
	}
	out.AddDef(sym)
	return descriptor, nil
}

// resolveDataRelocs turns the symbolic targets of a static value into the
// index pairs the object carries.
func resolveDataRelocs(syms *symbols, sym *obj.Symbol, relocs []dataReloc) error {
	for _, r := range relocs {
		ref, err := syms.stringData(r.text)
		if err != nil {
			return err
		}
		sym.Relocs = append(sym.Relocs, obj.Reloc{
			Off: r.off, Size: r.size, Type: r.typ, Add: r.add, Sym: ref,
		})
	}
	return nil
}

// globalKind is the section a variable lands in.
//
// Four kinds and not two. Whether the collector scans the symbol is the
// pointer question, and whether the linker allocates the space or copies bytes
// is the contents question, and the two are independent.
func globalKind(pointers, contents bool) obj.SymKind {
	switch {
	case pointers && contents:
		return obj.SDATA
	case pointers:
		return obj.SBSS
	case contents:
		return obj.SNOPTRDATA
	}
	return obj.SNOPTRBSS
}

func isBSS(k obj.SymKind) bool { return k == obj.SBSS || k == obj.SNOPTRBSS }

func anyNonZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return true
		}
	}
	return false
}

// A dataReloc is one edit the linker makes to a variable's bytes, with the
// string constant it points at rather than an index pair.
type dataReloc struct {
	off  int32
	size uint8
	typ  obj.RelocType
	add  int64
	text *ssa.StringSym
}

// staticInits returns the constant initialiser of each package-level variable,
// read out of the function ir.Build synthesised to run the initialisation.
//
// The initialiser is not on the object. ir.Package carries the variables and,
// separately, one function whose body assigns them in the order
// specs/012-type-checking.md computed. A constant assignment in that body is
// an initialiser the linker can write; anything else is code, and it stays
// code.
//
// The result is a lookup table and is never ranged over: the caller walks
// p.Globals, which is in declaration order (specs/053-determinism.md).
func staticInits(p *ir.Package) map[*ir.Object]ir.Value {
	out := map[*ir.Object]ir.Value{}
	for _, fn := range p.Inits {
		for _, s := range fn.Body {
			if s == nil || s.Op != ir.OAssign {
				continue
			}
			// A multiple assignment has its destinations in Args and one
			// value, which is a call and never a constant.
			if len(s.Args) != 0 || s.X == nil || s.Y == nil {
				continue
			}
			if s.X.Op != ir.OGlobal || s.X.Obj == nil || s.Y.Op != ir.OConst {
				continue
			}
			out[s.X.Obj] = s.Y.Val
		}
	}
	return out
}

// staticValue lays a constant out as the bytes of a symbol of type t.
//
// A nil value, or one this function has no layout for, gives the zero of the
// type: the variable is assigned at run time by the initialisation function
// instead. That is not a guess. A wrong guess would be writing bytes that
// disagree with what the source said, and the only values written here are the
// ones the type checker already reduced to a constant.
func staticValue(t *ir.Type, v ir.Value) ([]byte, []dataReloc, error) {
	data := make([]byte, t.Size)
	c, ok := v.(ir.Const)
	if !ok || c.Val == nil {
		return data, nil, nil
	}
	switch {
	case t.Kind == ir.Bool:
		if constant.BoolVal(c.Val) {
			data[0] = 1
		}
	case t.Kind.IsInteger():
		n, ok := c.Uint64()
		if !ok {
			i, ok := c.Int64()
			if !ok {
				return nil, nil, fmt.Errorf("the constant %s does not fit %v", c, t)
			}
			n = uint64(i)
		}
		putUint(data, n)
	case t.Kind == ir.Float32:
		f, _ := c.Float64()
		binary.LittleEndian.PutUint32(data, math.Float32bits(float32(f)))
	case t.Kind == ir.Float64:
		f, _ := c.Float64()
		binary.LittleEndian.PutUint64(data, math.Float64bits(f))
	case t.Kind == ir.String:
		return stringHeader(t, c)
	}
	// Every other kind whose constant is the predeclared nil is already the
	// zero bytes above, and every other kind has no constant at all.
	return data, nil, nil
}

// stringHeader lays out the two words of a string variable.
//
// The first is the address of the constant's bytes and the second is the
// length. The address is a relocation, because the bytes are a symbol of their
// own and the linker places it. The empty string points at nothing, as it does
// everywhere else in this compiler: a symbol of no bytes would be a definition
// the linker has to place for a pointer nothing dereferences.
func stringHeader(t *ir.Type, c ir.Const) ([]byte, []dataReloc, error) {
	if t.Size != 2*ir.PtrSize {
		return nil, nil, fmt.Errorf("a string of %d bytes, want %d", t.Size, 2*ir.PtrSize)
	}
	text := constant.StringVal(c.Val)
	data := make([]byte, t.Size)
	binary.LittleEndian.PutUint64(data[ir.PtrSize:], uint64(len(text)))
	if text == "" {
		return data, nil, nil
	}
	return data, []dataReloc{{
		off:  0,
		size: ir.PtrSize,
		typ:  obj.R_ADDR,
		text: ssa.NewStringSym(text),
	}}, nil
}

// putUint writes n into the width of the destination.
func putUint(b []byte, n uint64) {
	for i := range b {
		if i >= 8 {
			return
		}
		b[i] = byte(n >> (8 * i))
	}
}
