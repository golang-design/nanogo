// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package rtsym names the runtime functions the compiler generates calls to.
//
// specs/031-runtime-lowering.md requires this table to be checked against the
// runtime rather than typed in and trusted: "A signature that drifts from the
// runtime is a crash with no diagnostic, and a generated table turns that into
// a build failure." The check is in rtsym_test.go and it reads the runtime's
// own source.
//
// The symbols are unexported in the runtime, so none of them appears in export
// data and none can be found through go/importer. The runtime's source is the
// only oracle there is.
package rtsym

import "sort"

// Sym is one runtime function the compiler may call.
type Sym struct {
	// Name is the linker symbol, always with the "runtime." prefix.
	Name string

	// Sig is the signature as written in the runtime's source, with the
	// parameter names removed and the types left exactly as spelled. It is
	// compared textually against the runtime, so a change of spelling is a
	// failure even when the type is the same. That is the intent: a spelling
	// change is a signal to look.
	//
	// The spelling is the runtime's own vocabulary, so the types here are
	// names like tmpBuf and *itab that mean nothing outside that package. That
	// is deliberate. Writing what the type resolves to, *[32]byte for tmpBuf,
	// would compare equal today and stop comparing the day the runtime changes
	// the buffer's size. The first draft of this table did exactly that and
	// this test caught thirteen of its thirty-nine entries.
	Sig string

	// NoReturn marks a function that never returns.
	//
	// This is not a hint. specs/031 explains the consequence: a block after a
	// call to one is unreachable, and liveness that thinks otherwise keeps
	// values alive across every bounds check in the program.
	NoReturn bool

	// Assembly marks a symbol with no Go declaration.
	//
	// The prologue calls runtime.morestack_noctxt, which exists only in
	// runtime/asm_<arch>.s. Its signature cannot be read from Go source, so
	// the table records what the compiler calls it with and a separate test
	// checks the symbol is defined in the assembly rather than checking a
	// signature that no source states.
	Assembly bool

	// Group is what the symbol is for, so that a reader can find the family
	// rather than the name.
	Group Group
}

// Group classifies a runtime symbol.
type Group uint8

const (
	GroupInvalid Group = iota
	GroupAlloc
	GroupMap
	GroupChan
	GroupInterface
	GroupString
	GroupMemory
	GroupPanic
	GroupBarrier
	GroupGoroutine
	GroupDefer
)

var groupNames = [...]string{
	GroupInvalid:   "invalid",
	GroupAlloc:     "allocation",
	GroupMap:       "map",
	GroupChan:      "channel",
	GroupInterface: "interface",
	GroupString:    "string",
	GroupMemory:    "memory",
	GroupPanic:     "panic",
	GroupBarrier:   "write barrier",
	GroupGoroutine: "goroutine",
	GroupDefer:     "defer",
}

func (g Group) String() string {
	if int(g) < len(groupNames) && groupNames[g] != "" {
		return groupNames[g]
	}
	return "group(?)"
}

// syms is the table, in one place so the test can walk all of it.
//
// It is a slice and not a map, because specs/053-determinism.md forbids ranging
// over a map on a path that produces output and this table is walked to emit
// relocations.
var syms = []Sym{
	// Allocation.
	{Name: "runtime.newobject", Sig: "func(*_type) unsafe.Pointer", Group: GroupAlloc},
	{Name: "runtime.newarray", Sig: "func(*_type, int) unsafe.Pointer", Group: GroupAlloc},
	{Name: "runtime.makeslice", Sig: "func(*_type, int, int) unsafe.Pointer", Group: GroupAlloc},
	{Name: "runtime.makeslice64", Sig: "func(*_type, int64, int64) unsafe.Pointer", Group: GroupAlloc},
	{Name: "runtime.growslice", Sig: "func(unsafe.Pointer, int, int, int, *_type) slice", Group: GroupAlloc},
	{Name: "runtime.makechan", Sig: "func(*chantype, int) *hchan", Group: GroupAlloc},
	{Name: "runtime.makechan64", Sig: "func(*chantype, int64) *hchan", Group: GroupAlloc},

	// Channels.
	{Name: "runtime.chansend1", Sig: "func(*hchan, unsafe.Pointer)", Group: GroupChan},
	{Name: "runtime.chanrecv1", Sig: "func(*hchan, unsafe.Pointer)", Group: GroupChan},
	{Name: "runtime.chanrecv2", Sig: "func(*hchan, unsafe.Pointer) bool", Group: GroupChan},
	{Name: "runtime.closechan", Sig: "func(*hchan)", Group: GroupChan},
	{Name: "runtime.selectnbsend", Sig: "func(*hchan, unsafe.Pointer) bool", Group: GroupChan},
	{Name: "runtime.selectnbrecv", Sig: "func(unsafe.Pointer, *hchan) (bool, bool)", Group: GroupChan},
	{Name: "runtime.selectgo", Sig: "func(*scase, *uint16, *uintptr, int, int, bool) (int, bool)", Group: GroupChan},

	// Strings.
	{Name: "runtime.concatstring2", Sig: "func(*tmpBuf, string, string) string", Group: GroupString},
	{Name: "runtime.concatstring3", Sig: "func(*tmpBuf, string, string, string) string", Group: GroupString},
	{Name: "runtime.concatstring4", Sig: "func(*tmpBuf, string, string, string, string) string", Group: GroupString},
	{Name: "runtime.concatstring5", Sig: "func(*tmpBuf, string, string, string, string, string) string", Group: GroupString},
	{Name: "runtime.slicebytetostring", Sig: "func(*tmpBuf, *byte, int) string", Group: GroupString},
	{Name: "runtime.stringtoslicebyte", Sig: "func(*tmpBuf, string) []byte", Group: GroupString},
	{Name: "runtime.stringtoslicerune", Sig: "func(*[tmpStringBufSize]rune, string) []rune", Group: GroupString},
	{Name: "runtime.slicerunetostring", Sig: "func(*tmpBuf, []rune) string", Group: GroupString},
	{Name: "runtime.intstring", Sig: "func(*[4]byte, int64) string", Group: GroupString},

	// Memory.
	{Name: "runtime.memmove", Sig: "func(unsafe.Pointer, unsafe.Pointer, uintptr)", Group: GroupMemory},
	{Name: "runtime.memclrNoHeapPointers", Sig: "func(unsafe.Pointer, uintptr)", Group: GroupMemory},
	{Name: "runtime.memclrHasPointers", Sig: "func(unsafe.Pointer, uintptr)", Group: GroupMemory},
	{Name: "runtime.memequal", Sig: "func(unsafe.Pointer, unsafe.Pointer, uintptr) bool", Group: GroupMemory},

	// Panics. Every one of these never returns.
	{Name: "runtime.gopanic", Sig: "func(any)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.gorecover", Sig: "func() any", Group: GroupPanic},
	{Name: "runtime.goPanicIndex", Sig: "func(int, int)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.goPanicSliceAlen", Sig: "func(int, int)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.goPanicSliceB", Sig: "func(int, int)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.panicdivide", Sig: "func()", NoReturn: true, Group: GroupPanic},

	// Goroutines and defer.
	{Name: "runtime.newproc", Sig: "func(*funcval)", Group: GroupGoroutine},
	// morestack_noctxt has no Go declaration: it is written in assembly, so
	// the table records the signature it is called with rather than one the
	// checker can read. TestAssemblySymbolsExist is what keeps it honest.
	{Name: "runtime.morestack_noctxt", Sig: "func()", Assembly: true, Group: GroupGoroutine},
	{Name: "runtime.deferproc", Sig: "func(func())", Group: GroupDefer},
	{Name: "runtime.deferreturn", Sig: "func()", Group: GroupDefer},

	// Interfaces.
	{Name: "runtime.convT64", Sig: "func(uint64) unsafe.Pointer", Group: GroupInterface},
	{Name: "runtime.convTstring", Sig: "func(string) unsafe.Pointer", Group: GroupInterface},
	{Name: "runtime.convTslice", Sig: "func([]byte) unsafe.Pointer", Group: GroupInterface},
	{Name: "runtime.typeAssert", Sig: "func(*abi.TypeAssert, *_type) *itab", Group: GroupInterface},
	{Name: "runtime.interfaceSwitch", Sig: "func(*abi.InterfaceSwitch, *_type) (int, *itab)", Group: GroupInterface},
}

// index is built once from syms, for lookup only. It never reaches an output
// path, so specs/053's rule against ranging over a map does not apply to it.
var index = func() map[string]*Sym {
	m := make(map[string]*Sym, len(syms))
	for i := range syms {
		m[syms[i].Name] = &syms[i]
	}
	return m
}()

// Lookup returns the symbol with the given linker name, or nil.
func Lookup(name string) *Sym { return index[name] }

// All returns every symbol, sorted by name.
//
// Sorted rather than in table order, so that a caller emitting relocations
// produces the same bytes whatever the table's order happens to be.
func All() []Sym {
	out := make([]Sym, len(syms))
	copy(out, syms)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Base returns the name without the "runtime." prefix.
func (s Sym) Base() string { return s.Name[len("runtime."):] }
