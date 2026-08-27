// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"encoding/binary"
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtsym"
)

// The two structures the runtime reads to answer a question about an
// interface it cannot answer at compile time, per
// specs/032-type-descriptors-and-itabs.md.
//
//	type TypeAssert struct {
//	    Cache   *TypeAssertCache
//	    Inter   *InterfaceType
//	    CanFail bool
//	}
//
//	type InterfaceSwitch struct {
//	    Cache  *InterfaceSwitchCache
//	    NCases int
//	    Cases  [1]*InterfaceType   // variable sized, NCases entries
//	}
//
// # Why these are not descriptors
//
// Everything else this package writes is read-only. These two are caches the
// runtime mutates: runtime.typeAssert and runtime.interfaceSwitch build a
// table of the answers they computed and install it with a compare-and-swap on
// the Cache word. Three consequences follow, and none of them is a choice.
//
//   - The symbol lives in a writable section. obj.SDATA, not obj.SRODATA. A
//     read-only page faults on the store.
//   - The symbol is aligned to a pointer. The install is an atomic
//     compare-and-swap on the first word, and an unaligned one does not
//     execute on arm64.
//   - The symbol carries the linker name of its own Go type. The Cache word
//     points at a heap allocation, so the collector has to scan it, and
//     cmd/link builds the data section's pointer map out of each symbol's Go
//     type. Without one the link stops with "missing Go type information for
//     global symbol".
//
// # Why the symbol is a package definition
//
// A descriptor and an itab are canonical, duplicate-tolerant and merged by
// cmd/link. A cache must not be any of those. Two packages sharing one cache
// would be two modules writing one word, and the name is a function of the
// site rather than of the type, so there is nothing to merge. ir names it
// after the function that reads it.
const (
	typeAssertOffCache   = 0
	typeAssertOffInter   = 8
	typeAssertOffCanFail = 16
	typeAssertSize       = 24

	ifaceSwitchOffCache  = 0
	ifaceSwitchOffNCases = 8
	ifaceSwitchOffCases  = 16
)

// The Go types of the two symbols, which cmd/link reads the pointer map out
// of.
//
// internal/abi declares both, and gc points its own at exactly these names
// through reflectdata.TypeLinksym. The descriptors are in the object of
// internal/abi, which every program that has a runtime links, so the reference
// resolves without this package writing the bytes.
//
// The Go type of an InterfaceSwitch covers Cache, NCases and one case, and the
// symbol is one word longer per case after the first. That is deliberate and
// it is gc's comment as well: only the first pointer needs to be scanned,
// because every later one is the address of a descriptor in static data and
// the collector never has to trace it.
const (
	typeAssertGotype  = ir.TypeSymbolPrefix + "internal/abi.TypeAssert"
	ifaceSwitchGotype = ir.TypeSymbolPrefix + "internal/abi.InterfaceSwitch"
)

// TypeAssert returns the symbol that describes one assertion to an interface.
//
// The Cache word points at runtime.emptyTypeAssertCache, which is a cache with
// a zero Mask and one entry whose Typ is nil, so the first lookup misses and
// falls into the runtime. A nil there would be a nil dereference instead:
// runtime.typeAssert reads oldC.Mask before it reads anything else.
//
// CanFail is the comma-ok form and is a byte in the structure rather than a
// second entry point. The runtime returns a nil itab where it would otherwise
// panic, so a descriptor written with the wrong byte turns a failed assertion
// into a panic or a panic into a silent nil.
func TypeAssert(a ir.TypeAssert) (Symbol, error) {
	if a.Sym == "" {
		return Symbol{}, fmt.Errorf("rtype: a type assertion cache has no symbol")
	}
	if a.Iface == nil || a.Iface.Kind != ir.Interface {
		return Symbol{}, fmt.Errorf("rtype: %s targets %s, and runtime.typeAssert answers which itab implements an interface", a.Sym, a.Iface)
	}
	if a.Iface.EmptyIface {
		return Symbol{}, fmt.Errorf("rtype: %s targets the empty interface, which every type implements and no call decides", a.Sym)
	}
	inter, err := ir.TypeSymbol(a.Iface)
	if err != nil {
		return Symbol{}, err
	}
	data := make([]byte, typeAssertSize)
	if a.CanFail {
		data[typeAssertOffCanFail] = 1
	}
	return Symbol{
		Name:   a.Sym,
		Kind:   obj.SDATA,
		Align:  ir.PtrSize,
		Local:  true,
		Gotype: typeAssertGotype,
		Data:   data,
		Relocs: []Reloc{
			{Off: typeAssertOffCache, Size: 8, Type: obj.R_ADDR, Target: emptyTypeAssertCache},
			{Off: typeAssertOffInter, Size: 8, Type: obj.R_ADDR, Target: inter},
		},
	}, nil
}

// InterfaceSwitch returns the symbol that describes one type switch over
// interface cases.
//
// The cases are in source order and the order is the answer.
// runtime.interfaceSwitch walks them in order and returns the index of the
// first the dynamic type implements, so a type that implements two of them
// reaches the clause the source wrote first. A reordering here is a program
// that runs a different clause and reports nothing.
//
// The Cases array is written to its true length and the symbol is that much
// longer than the structure the Go type describes. gc writes the same bytes
// the same way.
func InterfaceSwitch(s ir.InterfaceSwitch) (Symbol, error) {
	if s.Sym == "" {
		return Symbol{}, fmt.Errorf("rtype: an interface switch cache has no symbol")
	}
	if len(s.Cases) == 0 {
		return Symbol{}, fmt.Errorf("rtype: %s lists no case, and a switch with nothing to search is not a call", s.Sym)
	}
	relocs := []Reloc{{Off: ifaceSwitchOffCache, Size: 8, Type: obj.R_ADDR, Target: emptyInterfaceSwitchCache}}
	for i, c := range s.Cases {
		if c == nil || c.Kind != ir.Interface {
			return Symbol{}, fmt.Errorf("rtype: %s case %d is %s, and every case of an interface switch is an interface", s.Sym, i, c)
		}
		if c.EmptyIface {
			return Symbol{}, fmt.Errorf("rtype: %s case %d is the empty interface, which every non-nil dynamic type implements and no call decides", s.Sym, i)
		}
		name, err := ir.TypeSymbol(c)
		if err != nil {
			return Symbol{}, err
		}
		relocs = append(relocs, Reloc{
			Off: int32(ifaceSwitchOffCases + i*ir.PtrSize), Size: 8, Type: obj.R_ADDR, Target: name,
		})
	}
	data := make([]byte, ifaceSwitchOffCases+len(s.Cases)*ir.PtrSize)
	binary.LittleEndian.PutUint64(data[ifaceSwitchOffNCases:], uint64(len(s.Cases)))
	return Symbol{
		Name:   s.Sym,
		Kind:   obj.SDATA,
		Align:  ir.PtrSize,
		Local:  true,
		Gotype: ifaceSwitchGotype,
		Data:   data,
		Relocs: relocs,
	}, nil
}

// TypeAssertReferenced returns the types whose descriptors a type assertion
// cache points at.
//
// One: the interface it targets. The object that defines the cache owes it,
// because cmd/link resolves the relocation by name and a name nothing defines
// is a link failure that names the descriptor and not the cache.
func TypeAssertReferenced(a ir.TypeAssert) []*ir.Type {
	if a.Iface == nil {
		return nil
	}
	return []*ir.Type{a.Iface}
}

// InterfaceSwitchReferenced returns the types whose descriptors an interface
// switch cache points at, in the order it points at them.
//
// Every case, for the reason TypeAssertReferenced gives.
func InterfaceSwitchReferenced(s ir.InterfaceSwitch) []*ir.Type {
	out := make([]*ir.Type, 0, len(s.Cases))
	for _, c := range s.Cases {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

// The two runtime variables the Cache words point at.
//
// The names come from rtsym rather than from this file, because
// specs/031-runtime-lowering.md makes rtsym the one place a runtime symbol is
// spelled and its table is checked against the runtime's own source. A
// misspelling here would be a relocation the linker cannot resolve, and the
// message names neither this file nor the field.
var (
	emptyTypeAssertCache      = runtimeVar("runtime.emptyTypeAssertCache")
	emptyInterfaceSwitchCache = runtimeVar("runtime.emptyInterfaceSwitchCache")
)

// runtimeVar returns the linker name of a runtime variable rtsym holds.
func runtimeVar(name string) string {
	v := rtsym.LookupVar(name)
	if v == nil {
		panic("rtype: rtsym does not name " + name)
	}
	return v.Name
}
