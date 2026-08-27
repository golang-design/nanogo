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
	GroupPrint
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
	GroupPrint:     "print",
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
	// The three map constructors. makemap takes a third parameter that the
	// older signature did not: a stack buffer the compiler supplies, which the
	// runtime uses instead of allocating when the map does not escape. A call
	// built for the two-parameter form passes the hint where the buffer
	// belongs.
	{Name: "runtime.makemap", Sig: "func(*abi.MapType, int, *maps.Map) *maps.Map", Group: GroupAlloc},
	{Name: "runtime.makemap64", Sig: "func(*abi.MapType, int64, *maps.Map) *maps.Map", Group: GroupAlloc},
	{Name: "runtime.makemap_small", Sig: "func() *maps.Map", Group: GroupAlloc},

	// Maps.
	//
	// Every one of these takes an *abi.MapType and a *maps.Map. The map is a
	// swiss table, not the pre-Go-1.24 bucket map, and the two are different
	// structures with different field offsets.
	//
	// Iteration is mapIterStart and mapIterNext, and this is the row that must
	// not be spelled from memory. runtime.mapiterinit and runtime.mapiternext
	// still exist, in runtime/linkname_shim.go, but they take a
	// *runtime.linknameIter rather than a *maps.Iter and the two layouts
	// differ. They are compatibility shims for packages that reach in through
	// //go:linkname, not the compiler's entry points, and calling one with a
	// maps.Iter frame slot writes past the end of it.
	// cmd/compile/internal/walk/range.go names the two below.
	{Name: "runtime.mapaccess1", Sig: "func(*abi.MapType, *maps.Map, unsafe.Pointer) unsafe.Pointer", Group: GroupMap},
	{Name: "runtime.mapaccess2", Sig: "func(*abi.MapType, *maps.Map, unsafe.Pointer) (unsafe.Pointer, bool)", Group: GroupMap},
	{Name: "runtime.mapassign", Sig: "func(*abi.MapType, *maps.Map, unsafe.Pointer) unsafe.Pointer", Group: GroupMap},
	{Name: "runtime.mapdelete", Sig: "func(*abi.MapType, *maps.Map, unsafe.Pointer)", Group: GroupMap},
	{Name: "runtime.mapclear", Sig: "func(*abi.MapType, *maps.Map)", Group: GroupMap},
	{Name: "runtime.mapIterStart", Sig: "func(*abi.MapType, *maps.Map, *maps.Iter)", Group: GroupMap},
	{Name: "runtime.mapIterNext", Sig: "func(*maps.Iter)", Group: GroupMap},

	// Channels.
	{Name: "runtime.chansend1", Sig: "func(*hchan, unsafe.Pointer)", Group: GroupChan},
	{Name: "runtime.chanrecv1", Sig: "func(*hchan, unsafe.Pointer)", Group: GroupChan},
	{Name: "runtime.chanrecv2", Sig: "func(*hchan, unsafe.Pointer) bool", Group: GroupChan},
	{Name: "runtime.closechan", Sig: "func(*hchan)", Group: GroupChan},
	{Name: "runtime.selectnbsend", Sig: "func(*hchan, unsafe.Pointer) bool", Group: GroupChan},
	{Name: "runtime.selectnbrecv", Sig: "func(unsafe.Pointer, *hchan) (bool, bool)", Group: GroupChan},
	{Name: "runtime.selectgo", Sig: "func(*scase, *uint16, *uintptr, int, int, bool) (int, bool)", Group: GroupChan},
	// len and cap of a channel read a field of the hchan, and the read is not
	// a plain load: a nil channel has length and capacity zero and no hchan to
	// read them from, so the runtime does the nil check.
	{Name: "runtime.chanlen", Sig: "func(*hchan) int", Group: GroupChan},
	{Name: "runtime.chancap", Sig: "func(*hchan) int", Group: GroupChan},

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
	// decoderune takes and returns a uint and not an int, and the range row
	// converts on both sides because of it. cmd/compile/internal/walk's
	// range.go says the same thing where it builds the call: "decoderune
	// expects a uint, but hv1 is an int. This is safe because hv1 is always
	// >= 0." A call built for an int result reads eight bytes of a register
	// the callee wrote four of.
	{Name: "runtime.decoderune", Sig: "func(string, uint) (rune, uint)", Group: GroupString},

	// Memory.
	{Name: "runtime.memmove", Sig: "func(unsafe.Pointer, unsafe.Pointer, uintptr)", Group: GroupMemory},
	{Name: "runtime.memclrNoHeapPointers", Sig: "func(unsafe.Pointer, uintptr)", Group: GroupMemory},
	{Name: "runtime.memclrHasPointers", Sig: "func(unsafe.Pointer, uintptr)", Group: GroupMemory},
	{Name: "runtime.memequal", Sig: "func(unsafe.Pointer, unsafe.Pointer, uintptr) bool", Group: GroupMemory},

	// The equality algorithms a type descriptor's Equal field points at.
	//
	// specs/032-type-descriptors-and-itabs.md calls Equal "a function the
	// compiler must emit". That is true only for a struct or an array whose
	// parts do not compare as one region of memory. Every other comparable
	// type reaches one of these, and a descriptor that named a generated
	// function where one of these belongs would emit a function per type for
	// work the runtime already does.
	//
	// The descriptor's field is a func value, not a code address, so what it
	// points at is a one-word closure symbol holding the address of the
	// function named here. rtype writes that symbol; the name checked against
	// the runtime is the function's.
	{Name: "runtime.memequal0", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.memequal8", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.memequal16", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.memequal32", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.memequal64", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.memequal128", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	// memequal_varlen takes the length out of the closure it is called
	// through, which is why a region of a size the fixed-width forms do not
	// cover still needs no generated function.
	{Name: "runtime.memequal_varlen", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.strequal", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.f32equal", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.f64equal", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.c64equal", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	{Name: "runtime.c128equal", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupMemory},
	// interequal reads an *itab out of the first word and nilinterequal reads
	// a *_type, which is specs/031's rule about ifaceeq and efaceeq applied to
	// the descriptor's Equal field: the choice follows the interface's layout
	// and is never a guess.
	{Name: "runtime.interequal", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupInterface},
	{Name: "runtime.nilinterequal", Sig: "func(unsafe.Pointer, unsafe.Pointer) bool", Group: GroupInterface},

	// The hash algorithms a map descriptor's Hasher field points at.
	//
	// One per equality algorithm and chosen by the same rule, which is not a
	// coincidence: gc derives both from one AlgKind, because two values that
	// compare equal must hash alike or a map loses keys it holds. So a type
	// whose Equal is runtime.strequal has runtime.strhash for its Hasher, and
	// a mismatch between the two tables is a map that cannot find its own
	// entries.
	//
	// The field is a func value like Equal, so what it points at is a one-word
	// closure symbol holding the address of the function named here, and
	// memhash_varlen takes the size out of that closure the way
	// memequal_varlen does.
	{Name: "runtime.memhash0", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.memhash8", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.memhash16", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.memhash32", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.memhash64", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.memhash128", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.memhash_varlen", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.strhash", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.f32hash", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.f64hash", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.c64hash", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.c128hash", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupMemory},
	{Name: "runtime.interhash", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupInterface},
	{Name: "runtime.nilinterhash", Sig: "func(unsafe.Pointer, uintptr) uintptr", Group: GroupInterface},

	// Panics. Every one of these never returns.
	{Name: "runtime.gopanic", Sig: "func(any)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.gorecover", Sig: "func() any", Group: GroupPanic},
	{Name: "runtime.goPanicIndex", Sig: "func(int, int)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.goPanicSliceAlen", Sig: "func(int, int)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.goPanicSliceB", Sig: "func(int, int)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.panicdivide", Sig: "func()", NoReturn: true, Group: GroupPanic},
	// The three panics a failed type assertion raises. Which one is called is
	// decided by what the value holds, not by convenience: panicdottypeI takes
	// the itab the value carried and panicdottypeE takes the *_type, so
	// calling the wrong one reads the wrong word as a descriptor. The third is
	// for a nil interface, which carries neither.
	{Name: "runtime.panicdottypeE", Sig: "func(*_type, *_type, *_type)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.panicdottypeI", Sig: "func(*itab, *_type, *_type)", NoReturn: true, Group: GroupPanic},
	{Name: "runtime.panicnildottype", Sig: "func(*_type)", NoReturn: true, Group: GroupPanic},

	// Goroutines and defer.
	{Name: "runtime.newproc", Sig: "func(*funcval)", Group: GroupGoroutine},
	// morestack_noctxt has no Go declaration: it is written in assembly, so
	// the table records the signature it is called with rather than one the
	// checker can read. TestAssemblySymbolsExist is what keeps it honest.
	{Name: "runtime.morestack_noctxt", Sig: "func()", Assembly: true, Group: GroupGoroutine},
	// runtime.morestack is the same tail for a function that reads the
	// context register. The two differ in one instruction and the difference
	// is a correctness one: morestack_noctxt writes zero into the register
	// before it grows the stack, and morestack saves the register into
	// g.sched.ctxt so that the resumed function still finds its closure. A
	// function with no closure must call the first, because the register then
	// holds whatever the caller left and g.sched.ctxt is scanned by the
	// collector.
	{Name: "runtime.morestack", Sig: "func()", Assembly: true, Group: GroupGoroutine},
	{Name: "runtime.deferproc", Sig: "func(func())", Group: GroupDefer},
	{Name: "runtime.deferreturn", Sig: "func()", Group: GroupDefer},

	// print and println. specs/020-ir.md's rows for the two builtins are a
	// bracketed sequence: the lock, one call per operand, the newline that
	// only println writes, and the unlock. The lock is not decoration. The
	// runtime prints to file descriptor 2 without buffering, so two goroutines
	// printing at once interleave inside a line, and printlock is what the
	// language's one guarantee about print rests on.
	{Name: "runtime.printlock", Sig: "func()", Group: GroupPrint},
	{Name: "runtime.printunlock", Sig: "func()", Group: GroupPrint},
	// printsp and printnl are the separator and the newline. gc writes them as
	// the string constants " " and "\n" and then recognises those two
	// constants again, which is a detour this pass does not take: the two
	// symbols are what the detour arrives at.
	{Name: "runtime.printsp", Sig: "func()", Group: GroupPrint},
	{Name: "runtime.printnl", Sig: "func()", Group: GroupPrint},
	{Name: "runtime.printbool", Sig: "func(bool)", Group: GroupPrint},
	// One symbol per width class and not per type. Every signed kind widens to
	// int64 and every unsigned kind to uint64, which is what gc does, so a
	// program that prints an int8 and one that prints an int64 call one
	// function.
	{Name: "runtime.printint", Sig: "func(int64)", Group: GroupPrint},
	{Name: "runtime.printuint", Sig: "func(uint64)", Group: GroupPrint},
	{Name: "runtime.printfloat32", Sig: "func(float32)", Group: GroupPrint},
	{Name: "runtime.printfloat64", Sig: "func(float64)", Group: GroupPrint},
	{Name: "runtime.printstring", Sig: "func(string)", Group: GroupPrint},
	// A pointer, a map, a channel and a func value are all one word, and the
	// runtime prints the word. A uintptr is not one of them: it is a number,
	// and gc prints it with printuint, so nothing here hands the collector a
	// number as if it were a pointer.
	{Name: "runtime.printpointer", Sig: "func(unsafe.Pointer)", Group: GroupPrint},

	// Interface and string comparison. specs/031's table names these as the
	// calls an equality or an ordering lowers to.
	{Name: "runtime.ifaceeq", Sig: "func(*itab, unsafe.Pointer, unsafe.Pointer) bool", Group: GroupInterface},
	{Name: "runtime.efaceeq", Sig: "func(*_type, unsafe.Pointer, unsafe.Pointer) bool", Group: GroupInterface},
	// cmpstring has no declaration in package runtime. It is written in
	// internal/bytealg and reaches the runtime's symbol namespace through a
	// //go:linkname, so the checker resolves it there rather than asserting a
	// signature nothing states.
	{Name: "runtime.cmpstring", Sig: "func(string, string) int", Group: GroupString},

	// Interfaces.
	{Name: "runtime.convT64", Sig: "func(uint64) unsafe.Pointer", Group: GroupInterface},
	{Name: "runtime.convTstring", Sig: "func(string) unsafe.Pointer", Group: GroupInterface},
	{Name: "runtime.convTslice", Sig: "func([]byte) unsafe.Pointer", Group: GroupInterface},
	{Name: "runtime.typeAssert", Sig: "func(*abi.TypeAssert, *_type) *itab", Group: GroupInterface},
	{Name: "runtime.interfaceSwitch", Sig: "func(*abi.InterfaceSwitch, *_type) (int, *itab)", Group: GroupInterface},
	// convT16 and convT32 return a pointer into the runtime's table of small
	// integers rather than an allocation, which is why they are separate
	// symbols and not calls to convT with a size.
	{Name: "runtime.convT16", Sig: "func(uint16) unsafe.Pointer", Group: GroupInterface},
	{Name: "runtime.convT32", Sig: "func(uint32) unsafe.Pointer", Group: GroupInterface},
	// convT and convTnoptr have one signature and two bodies. The first
	// allocates in a span the collector scans and the second in one it does
	// not, so a type holding a pointer converted through convTnoptr has its
	// pointee freed underneath it. The choice follows the type's pointer map
	// and is never a guess.
	{Name: "runtime.convT", Sig: "func(*_type, unsafe.Pointer) unsafe.Pointer", Group: GroupInterface},
	{Name: "runtime.convTnoptr", Sig: "func(*_type, unsafe.Pointer) unsafe.Pointer", Group: GroupInterface},
	// The itab lookups. getitab is the general one and takes a bool saying
	// whether a missing method is a panic or a nil result; assertE2I and
	// assertE2I2 are the one- and two-value forms of an assertion from an
	// empty interface to one with methods.
	//
	// assertI2I and assertI2I2 are not here, and their absence is a fact about
	// go1.27 rather than an omission: the runtime no longer declares them.
	// runtime.typeAssert, already above, is what the compiler emits for an
	// assertion between two interfaces with methods.
	{Name: "runtime.getitab", Sig: "func(*interfacetype, *_type, bool) *itab", Group: GroupInterface},
	{Name: "runtime.assertE2I", Sig: "func(*interfacetype, *_type) *itab", Group: GroupInterface},
	{Name: "runtime.assertE2I2", Sig: "func(*interfacetype, *_type) *itab", Group: GroupInterface},

	// Write barriers.
	//
	// A pointer write into the heap while the collector is marking has to be
	// recorded, and the eight numbered entry points are how: each one reserves
	// that many slots in the P's write barrier buffer and returns a pointer to
	// them. They are written in assembly, do not follow the Go ABI, and have
	// no Go declaration to read a signature from.
	//
	// runtime.writeBarrier is the flag that guards them and is a variable, not
	// a function. It is in vars below.
	{Name: "runtime.gcWriteBarrier1", Sig: "func() unsafe.Pointer", Assembly: true, Group: GroupBarrier},
	{Name: "runtime.gcWriteBarrier2", Sig: "func() unsafe.Pointer", Assembly: true, Group: GroupBarrier},
	{Name: "runtime.gcWriteBarrier3", Sig: "func() unsafe.Pointer", Assembly: true, Group: GroupBarrier},
	{Name: "runtime.gcWriteBarrier4", Sig: "func() unsafe.Pointer", Assembly: true, Group: GroupBarrier},
	{Name: "runtime.gcWriteBarrier5", Sig: "func() unsafe.Pointer", Assembly: true, Group: GroupBarrier},
	{Name: "runtime.gcWriteBarrier6", Sig: "func() unsafe.Pointer", Assembly: true, Group: GroupBarrier},
	{Name: "runtime.gcWriteBarrier7", Sig: "func() unsafe.Pointer", Assembly: true, Group: GroupBarrier},
	{Name: "runtime.gcWriteBarrier8", Sig: "func() unsafe.Pointer", Assembly: true, Group: GroupBarrier},
	// The copies that carry a barrier of their own. A copy of a value holding
	// a pointer cannot be a memmove while the collector is marking, so the
	// runtime does the copy and the barrier together. wbZero and wbMove are
	// the halves the compiler calls when it has already emitted the copy.
	{Name: "runtime.typedmemmove", Sig: "func(*abi.Type, unsafe.Pointer, unsafe.Pointer)", Group: GroupBarrier},
	{Name: "runtime.typedmemclr", Sig: "func(*_type, unsafe.Pointer)", Group: GroupBarrier},
	{Name: "runtime.typedslicecopy", Sig: "func(*_type, unsafe.Pointer, int, unsafe.Pointer, int) int", Group: GroupBarrier},
	{Name: "runtime.wbZero", Sig: "func(*_type, unsafe.Pointer)", Group: GroupBarrier},
	{Name: "runtime.wbMove", Sig: "func(*_type, unsafe.Pointer, unsafe.Pointer)", Group: GroupBarrier},
}

// A Var is one runtime variable the compiler reads.
//
// It is a table of its own and not a Sym with a flag, because Sym.Sig is a
// function signature and a variable has a type. One field meaning two things
// is what makes a table like this stop being checkable.
type Var struct {
	// Name is the linker symbol, always with the "runtime." prefix.
	Name string

	// Type is the variable's type as written in the runtime's source, with the
	// field comments removed and the whitespace collapsed. It is compared
	// textually against the runtime for the reason Sym.Sig is.
	Type string

	// Group is what the variable is for.
	Group Group
}

// vars is the variable table. It is separate from syms for the reason Var
// documents.
var vars = []Var{
	// The flag that guards every write barrier. The compiler loads the first
	// four bytes and branches on them, which is why the runtime's comment
	// forbids changing them and why the padding is part of the type rather
	// than an accident: enabled is one byte and the load is 32 bit.
	{
		Name:  "runtime.writeBarrier",
		Type:  "struct { enabled bool; pad [3]byte; alignme uint64 }",
		Group: GroupBarrier,
	},
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

// varIndex is built once from vars, for lookup only.
var varIndex = func() map[string]*Var {
	m := make(map[string]*Var, len(vars))
	for i := range vars {
		m[vars[i].Name] = &vars[i]
	}
	return m
}()

// LookupVar returns the variable with the given linker name, or nil.
func LookupVar(name string) *Var { return varIndex[name] }

// AllVars returns every variable, sorted by name, for the reason All is
// sorted.
func AllVars() []Var {
	out := make([]Var, len(vars))
	copy(out, vars)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Base returns the name without the "runtime." prefix.
func (v Var) Base() string { return v.Name[len("runtime."):] }
