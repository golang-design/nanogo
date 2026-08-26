// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

// The assignment walk of specs/030-abi.md.
//
// # What the walk is
//
// Arguments are placed by walking the parameter list and, within each
// parameter, walking its type recursively. A scalar takes the next register of
// its class. A struct or an array is decomposed into its fields and each field
// is assigned on its own. If any part does not fit, the whole top-level value
// goes to the argument area: the rule is all-or-nothing per argument, and it
// is the rule a caller and a callee compiled by different toolchains have to
// agree on. Results are placed by the same walk with the register counters
// restarted.
//
// Every value gets an offset in the argument area whether or not it travels in
// a register. That is the spill space the spec reserves, and it exists because
// runtime.morestack re-executes the function from its first instruction: the
// stack-growth tail saves the argument registers into the area and reloads
// them, so the arguments must have somewhere to be. It is also why the
// arguments bitmap of specs/027-liveness-and-stackmaps.md describes the area
// for a function whose arguments all arrived in registers.
//
// # Why the pass rewrites the function and does not only measure it
//
// specs/025-lowering-and-rules.md decomposes a value wider than a register
// into one value per part and stops at MaxDecomposeParts, because above that
// bound a value is cheaper as a memory object. That bound is a code-quality
// choice inside a function. It is not a choice at a call boundary: gc passes a
// five-field struct in five registers, and a compiler that keeps it in memory
// instead does not produce slower code, it produces a call that reads its
// arguments from the wrong place.
//
// So this pass splits what the convention says travels in registers, however
// many parts that is. It runs after decomposition, so it only ever has to
// finish work that stopped at the bound.
//
// # Where the address of a large value comes from
//
// A value the register set cannot hold travels in the argument area, and the
// callee needs an address to load it from. There is no address to invent: the
// value has one already. specs/021-ssa-construction.md keeps every aggregate
// parameter in the frame and reaches it through OpLocalAddr, and the entry
// block stores the incoming value into that frame slot. This pass deletes that
// store and makes the parameter's home the argument area itself, so the
// OpLocalAddr that is already in the graph names the incoming words. The
// callee then loads from where the caller wrote, with no copy at all, which is
// also what gc does with such a parameter.
//
// The consequence is that the object leaves Func.Frame. It is no longer a
// local: it is described by the arguments bitmap rather than by the locals
// bitmap, and the code generator takes its offset from the assignment rather
// than from the frame layout.
//
// A call argument and a result have no such name, so the pass makes an
// *ir.Object for the slot and copies into it. Such an object is not in
// Func.Frame: the frame layout would give it a second slot in the locals and
// the locals bitmap would describe it twice, and it is not a local. The words
// belong to a callee's incoming argument area, which the callee's own bitmap
// describes.
//
// # The bound this pass does not close
//
// Decomposition runs before this pass and erases which operands of a call
// belonged to one parameter. A call's operands are therefore placed one value
// at a time, which agrees with the per-parameter walk except when the register
// set runs out in the middle of one parameter's parts: gc sends that whole
// parameter to the stack and this leaves its first parts in the registers that
// were left. Reaching it needs a call with fifteen or more scalar words of
// arguments followed by an aggregate. The fix is a call operation that carries
// its callee's signature, which specs/021-ssa-construction.md does not build.

import (
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
)

// ABIPart is one piece of an argument or a result that fits in one register.
//
// The type is the type of the word, not of the value it came out of, because
// ir.Type.PtrBits on each part is what tells the collector which incoming
// words hold pointers.
type ABIPart struct {
	// Off is the byte offset of the part within its top-level value.
	Off int64

	// Type is what the part holds.
	Type *ir.Type

	// Reg is the register the part travels in, or NoReg when the whole value
	// travels in the argument area.
	Reg Reg
}

// ABIValue is one top-level argument or result and everything the convention
// decided about it.
type ABIValue struct {
	// Obj is the parameter this names. It is nil for a result and at a call
	// site, where the values passed are the only description of the signature.
	Obj *ir.Object

	// Type is the declared type.
	Type *ir.Type

	// Off is the byte offset of the value in the argument area. Every value
	// has one, including one that travels in registers, because the
	// stack-growth tail spills it there.
	Off int64

	// Parts holds one entry per word the value occupies, in increasing offset
	// order. A zero-size value has none.
	Parts []ABIPart

	// InReg reports that every part travels in a register.
	InReg bool

	// Home reports that Obj's storage is this slot of the incoming argument
	// area rather than a slot of the locals area. It is set for a parameter
	// the register set could not hold, which this pass leaves where the caller
	// wrote it.
	Home bool

	// Copied reports that the value is in the argument area already, written
	// there by a block move ahead of the call, and is no longer one of the
	// call's operands. The placement still describes it, because the words it
	// occupies are what put every value after it where it is.
	Copied bool
}

// ABIHome names a slot of an argument area, so that OpLocalAddr can address it.
//
// A value the register set cannot hold travels in memory and the code that
// reads or writes it needs an address. A parameter has one already, because
// specs/021-ssa-construction.md gave it a frame slot and this pass moves that
// slot into the incoming argument area. Nothing names a slot of a call's
// outgoing area or of the result area, so this pass makes an object for it and
// the code generator gives that object an offset.
//
// The object is never in Func.Frame. The frame layout would give it a second
// slot and the locals bitmap would describe it twice, and it is not a local:
// the words belong to the callee's incoming argument area, which the callee's
// own bitmap describes.
type ABIHome struct {
	Obj *ir.Object

	// Off is the byte offset in the area.
	Off int64

	// Incoming says the area is this function's own, which is where a
	// parameter and a result live. Otherwise it is the outgoing area of a
	// call this function makes.
	Incoming bool
}

// ABICall is the placement of one call's operands.
//
// It is recorded rather than recomputed, because the operand list stops being
// a faithful description of what the call passes. A value copied into the area
// leaves the list altogether, and a value split into one operand per register
// has a spill slot the split parts do not add up to. A walk of what is left
// would place every value after it at the wrong offset and would size the area
// too small.
type ABICall struct {
	// Vals has one entry per operand the call passes, in operand order, plus
	// one for each value that was copied into the area and left the list.
	Vals []ABIValue

	// Size is the area the call needs.
	Size int64
}

// Operand returns the placement of the operand at index k of the argument
// list, skipping the values that are in the area already.
func (c *ABICall) Operand(k int) (*ABIValue, bool) {
	if c == nil || k < 0 {
		return nil, false
	}
	n := 0
	for i := range c.Vals {
		if c.Vals[i].Copied {
			continue
		}
		if n == k {
			return &c.Vals[i], true
		}
		n++
	}
	return nil, false
}

// NumOperands returns how many of the values are still operands.
func (c *ABICall) NumOperands() int {
	n := 0
	for i := range c.Vals {
		if !c.Vals[i].Copied {
			n++
		}
	}
	return n
}

// ABI is where specs/030-abi.md puts one function's parameters and results.
type ABI struct {
	// In and Out are the parameters and the results, in declaration order.
	In  []ABIValue
	Out []ABIValue

	// ArgsSize is the size of the incoming argument area: the values the
	// registers could not hold, arguments then results, and then one spill
	// slot per argument that travelled in a register.
	ArgsSize int64

	// Homes names the argument-area slots this function addresses.
	Homes []ABIHome

	// Calls holds the placement of each call's operands, indexed by the
	// call's value identifier, and nil where the operand list still describes
	// what the call passes. Selection rewrites a call in place and keeps its
	// identifier, and a call selection creates is not in the table and is
	// walked instead.
	Calls []*ABICall
}

// CallAt returns the recorded placement of a call, if there is one.
func (a *ABI) CallAt(id ID) *ABICall {
	if a == nil || int(id) < 0 || int(id) >= len(a.Calls) {
		return nil
	}
	return a.Calls[id]
}

// abiMaxParts bounds the recursive walk of one value's type.
//
// A value cannot occupy more registers than the two argument sets hold
// together, so a walk that reaches this many parts has already established
// that the value does not fit and can stop. Without a bound, an array of a
// million elements would cost a million entries to learn the same thing.
const abiMaxParts = 33

// ArgOf returns the placement of the parameter named by o.
func (a *ABI) ArgOf(o *ir.Object) (*ABIValue, bool) {
	if a == nil || o == nil {
		return nil, false
	}
	for i := range a.In {
		if a.In[i].Obj == o {
			return &a.In[i], true
		}
	}
	return nil, false
}

// ArgReg returns the register an OpArg value arrives in.
//
// The value names its parameter in Aux and its byte offset within that
// parameter in AuxInt, which is the pair specs/025-lowering-and-rules.md
// leaves on every part of a decomposed argument. A parameter that was never
// decomposed has offset zero and matches its own first part.
func (a *ABI) ArgReg(v *Value) (Reg, bool) {
	if a == nil || v == nil || v.Op != OpArg || v.Type == nil {
		return NoReg, false
	}
	o, _ := v.Aux.(*ir.Object)
	av, ok := a.ArgOf(o)
	if !ok || !av.InReg {
		return NoReg, false
	}
	for _, p := range av.Parts {
		if p.Off == v.AuxInt && p.Type != nil && p.Type.Size == v.Type.Size {
			return p.Reg, p.Reg != NoReg
		}
	}
	return NoReg, false
}

// ArgHome returns the offset in the incoming argument area of a parameter
// whose storage is that area.
func (a *ABI) ArgHome(o *ir.Object) (int64, bool) {
	av, ok := a.ArgOf(o)
	if !ok || !av.Home {
		return 0, false
	}
	return av.Off, true
}

// ---------------------------------------------------------------------------
// The walk over a type

// ABILeaves returns the parts of t, in increasing offset order.
//
// It is the same recursion specs/025-lowering-and-rules.md decomposes a value
// by, so the parts here and the values that pass produces are the same words
// in the same order. complete is false when the walk stopped at the bound,
// which is by itself enough to know the value does not fit in registers.
func ABILeaves(t *ir.Type) (parts []ABIPart, complete bool) {
	parts = abiFlatten(nil, t, 0)
	return parts, len(parts) < abiMaxParts
}

// abiFlatten appends the parts of t at offset off.
//
// The multi-word built-in types are spelled out rather than reached through
// their fields, because they have none: a string is a byte pointer and a
// length, and both words of an interface are pointers whatever it holds.
func abiFlatten(out []ABIPart, t *ir.Type, off int64) []ABIPart {
	if t == nil || len(out) >= abiMaxParts {
		return out
	}
	switch t.Kind {
	case ir.String:
		return append(out,
			ABIPart{Off: off, Type: abiBytePtr, Reg: NoReg},
			ABIPart{Off: off + ir.PtrSize, Type: abiInt, Reg: NoReg})

	case ir.Slice:
		return append(out,
			ABIPart{Off: off, Type: abiPtrTo(t.Elem), Reg: NoReg},
			ABIPart{Off: off + ir.PtrSize, Type: abiInt, Reg: NoReg},
			ABIPart{Off: off + 2*ir.PtrSize, Type: abiInt, Reg: NoReg})

	case ir.Interface:
		return append(out,
			ABIPart{Off: off, Type: abiUnsafePtr, Reg: NoReg},
			ABIPart{Off: off + ir.PtrSize, Type: abiUnsafePtr, Reg: NoReg})

	case ir.Complex64:
		return append(out,
			ABIPart{Off: off, Type: abiFloat32, Reg: NoReg},
			ABIPart{Off: off + 4, Type: abiFloat32, Reg: NoReg})

	case ir.Complex128:
		return append(out,
			ABIPart{Off: off, Type: abiFloat64, Reg: NoReg},
			ABIPart{Off: off + 8, Type: abiFloat64, Reg: NoReg})

	case ir.Struct, ir.Tuple:
		for i := range t.Fields {
			if len(out) >= abiMaxParts {
				return out
			}
			f := &t.Fields[i]
			out = abiFlatten(out, f.Type, off+f.Offset)
		}
		return out

	case ir.Array:
		if t.Elem == nil {
			return out
		}
		for i := int64(0); i < t.Len; i++ {
			if len(out) >= abiMaxParts {
				return out
			}
			out = abiFlatten(out, t.Elem, off+i*t.Elem.Size)
		}
		return out
	}
	// A type that occupies no storage contributes no part. A struct of only
	// zero-size fields is the case, and it must contribute none rather than
	// one of size zero, because no register holds a value of that width.
	if t.Size == 0 {
		return out
	}
	return append(out, ABIPart{Off: off, Type: t, Reg: NoReg})
}

// The types the built-in multi-word shapes decompose into.
//
// They are made once rather than per walk, because a spill slot may hold two
// values only when their types have one size, one alignment and one pointer
// map (specs/026-register-allocation.md), and one shared type makes that
// question cheap and the dumps readable.
var (
	abiInt       = &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}
	abiByte      = &ir.Type{Kind: ir.Uint8, Size: 1, Align: 1, Name: "uint8"}
	abiFloat32   = &ir.Type{Kind: ir.Float32, Size: 4, Align: 4, Name: "float32"}
	abiFloat64   = &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}
	abiUnsafePtr = &ir.Type{Kind: ir.UnsafePtr, Size: ir.PtrSize, Align: ir.PtrSize,
		PtrBits: []byte{1}, Name: "unsafe.Pointer"}
	abiBytePtr = abiPtrTo(abiByte)
)

// abiPtrTo returns the type of a pointer to elem.
func abiPtrTo(elem *ir.Type) *ir.Type {
	if elem == nil {
		elem = abiByte
	}
	return &ir.Type{Kind: ir.Ptr, Size: ir.PtrSize, Align: ir.PtrSize,
		PtrBits: []byte{1}, Elem: elem}
}

// ---------------------------------------------------------------------------
// The assignment

// abiAssigner walks one list of values against one set of register sequences.
//
// # The argument area has two parts and they are not interleaved
//
// gc lays the incoming argument area out as
//
//	[0, stack)              the values the registers could not hold, in order
//	[spillBase, args)       one slot per value that travelled in a register
//
// with spillBase the stack part rounded up to a word. The two run on separate
// counters: a value that takes registers advances the spill counter and not
// the stack one, and the other way round.
//
// specs/030-abi.md says only that "the stack portion is laid out in the
// caller's outgoing argument area at increasing offsets" and, separately, that
// the frame reserves space for the register arguments. Read as one sequence,
// which is how this compiler read it until now, the two agree with gc when
// every value takes a register and when none does, and disagree by the size of
// the stack part whenever a call mixes the two. That is a call whose stack
// arguments are read from the wrong words, and it is silent.
type abiAssigner struct {
	t    *Target
	regs *[NumRegClass][]Reg
	next [NumRegClass]int

	// stack is the offset of the next value the registers cannot hold, and
	// spill is the offset of the next slot in the spill area, measured from
	// the start of that area.
	stack int64
	spill int64

	// isReturn suppresses the spill slot. A result is not saved for
	// runtime.morestack, because the function is re-executed from its first
	// instruction and has not produced one yet.
	isReturn bool
}

// place assigns one top-level value.
//
// The register counters advance only when the whole value fits, which is the
// all-or-nothing rule: a value with one part too many takes no register at
// all, and the registers it would have taken stay available for the value
// after it.
func (as *abiAssigner) place(typ *ir.Type, obj *ir.Object) (ABIValue, bool, error) {
	if typ == nil {
		return ABIValue{}, false, fmt.Errorf("a value with no type")
	}
	parts, complete := ABILeaves(typ)
	av := ABIValue{Obj: obj, Type: typ, Parts: parts, Off: -1}

	// A trial set of counters, copied back only when every part found a
	// register.
	try := as.next
	fits := complete
	for i := range parts {
		if !fits {
			break
		}
		c, ok := as.t.ClassOf(parts[i].Type)
		if !ok || try[c] >= len(as.regs[c]) {
			fits = false
			break
		}
		parts[i].Reg = as.regs[c][try[c]]
		try[c]++
	}
	if !fits {
		for i := range parts {
			parts[i].Reg = NoReg
		}
		as.stack = abiAlign(as.stack, typ)
		av.Off = as.stack
		as.stack += typ.Size
		return av, false, nil
	}
	as.next = try
	av.InReg = true
	if as.isReturn {
		// No slot at all, which is what gc records as offset -1.
		return av, false, nil
	}
	as.spill = abiAlign(as.spill, typ)
	av.Off = as.spill
	as.spill += typ.Size
	return av, true, nil
}

func abiAlign(off int64, t *ir.Type) int64 {
	if t.Align > 0 {
		return abiRoundUp(off, t.Align)
	}
	return off
}

func abiRoundUp(n, align int64) int64 {
	if align <= 1 {
		return n
	}
	return (n + align - 1) / align * align
}

// ABIArgs places a list of argument types and returns the size of the argument
// area they need.
//
// It is the placement of one call's operands: the stack part first, then the
// spill slots the callee writes when it grows the stack.
func ABIArgs(t *Target, types []*ir.Type) ([]ABIValue, int64, error) {
	if t == nil || t.ClassOf == nil {
		return nil, 0, fmt.Errorf("ssa: abi: no target")
	}
	as := &abiAssigner{t: t, regs: &t.ArgRegs}
	in, spilled, err := as.placeAll(types, nil)
	if err != nil {
		return nil, 0, err
	}
	return in, as.finish(in, spilled), nil
}

// ABIResults places a list of result types. The register counters restart,
// which is what specs/030-abi.md means by walking the result list again, and
// nothing is spilled.
func ABIResults(t *Target, types []*ir.Type) ([]ABIValue, int64, error) {
	if t == nil || t.ClassOf == nil {
		return nil, 0, fmt.Errorf("ssa: abi: no target")
	}
	as := &abiAssigner{t: t, regs: &t.ResultRegs, isReturn: true}
	out, _, err := as.placeAll(types, nil)
	if err != nil {
		return nil, 0, err
	}
	return out, abiRoundUp(as.stack, ir.PtrSize), nil
}

// placeAll walks a list of types and reports which of the results took a spill
// slot, by index.
func (as *abiAssigner) placeAll(types []*ir.Type, objs []*ir.Object) ([]ABIValue, []bool, error) {
	out := make([]ABIValue, 0, len(types))
	spilled := make([]bool, 0, len(types))
	for i, typ := range types {
		var obj *ir.Object
		if i < len(objs) {
			obj = objs[i]
		}
		av, sp, err := as.place(typ, obj)
		if err != nil {
			return nil, nil, fmt.Errorf("ssa: abi: value %d: %w", i, err)
		}
		out = append(out, av)
		spilled = append(spilled, sp)
	}
	return out, spilled, nil
}

// finish moves the spill slots above the stack part and returns the size of
// the whole area.
//
// The base is not known until every value is placed, because it is where the
// stack part ended, so the slots are assigned relative offsets first and
// rebased here.
func (as *abiAssigner) finish(vals []ABIValue, spilled []bool) int64 {
	base := abiRoundUp(as.stack, ir.PtrSize)
	for i := range vals {
		if i < len(spilled) && spilled[i] {
			vals[i].Off += base
		}
	}
	return abiRoundUp(base+as.spill, ir.PtrSize)
}

// ---------------------------------------------------------------------------
// The pass

// AssignABI places f's parameters and results and rewrites f so that the
// placement is realisable.
//
// It runs after decomposition and before selection. After, because it finishes
// work decomposition stopped at MaxDecomposeParts and has to see what was
// already done. Before, because the rewrites it makes are arguments, loads,
// stores and address arithmetic, which selection still has rules for.
func AssignABI(f *Func, t *Target) error {
	if f == nil {
		return fmt.Errorf("ssa: abi: nil function")
	}
	if t == nil || t.ClassOf == nil {
		return fmt.Errorf("ssa: abi: %s: no target", f.Name)
	}
	a := &abiPass{f: f, t: t}
	return a.run()
}

type abiPass struct {
	f *Func
	t *Target

	// objs holds the parameters in declaration order and args holds the OpArg
	// values of each, which is one value per part after decomposition.
	objs []*ir.Object
	args [][]*Value

	// users lists the values that read each value, indexed by identifier.
	users [][]*Value

	// before holds the values to insert ahead of an anchor, keyed by the
	// anchor's identifier, and dead marks the values that leave. touched
	// names the blocks that have to be rebuilt.
	before  map[ID][]*Value
	dead    []bool
	touched []bool
}

func (a *abiPass) run() error {
	a.index()
	a.collectArgs()

	types := make([]*ir.Type, 0, len(a.objs))
	for _, o := range a.objs {
		types = append(types, o.Type)
	}
	// The parameters and the results share the stack part of the argument
	// area, in that order, and the spill slots sit above both. One assigner
	// therefore walks both lists, with the register counters restarted between
	// them: that restart is the only difference the spec names, and the
	// shared stack counter is the one it does not.
	as := &abiAssigner{t: a.t, regs: &a.t.ArgRegs}
	in, spilled, err := as.placeAll(types, a.objs)
	if err != nil {
		return fmt.Errorf("ssa: abi: %s: %w", a.f.Name, err)
	}
	as.next = [NumRegClass]int{}
	as.regs = &a.t.ResultRegs
	as.isReturn = true
	out, _, err := as.placeAll(a.resultTypes(), nil)
	if err != nil {
		return fmt.Errorf("ssa: abi: %s: %w", a.f.Name, err)
	}
	argsSize := as.finish(in, spilled)

	a.f.ABI = &ABI{In: in, Out: out, ArgsSize: argsSize, Calls: make([]*ABICall, a.f.NumValues())}
	a.rewriteArgs()
	a.rewriteResults()
	a.rewriteBoundaries()
	if err := a.rewriteCallResults(); err != nil {
		return err
	}
	a.compact()
	return nil
}

// home makes an object that names one slot of an argument area.
//
// The name reaches no object file. It is the key the code generator looks the
// offset up by and the string a dump prints, and it says which area and which
// value so that a dump of a function with several of them reads.
func (a *abiPass) home(what string, n int, typ *ir.Type, off int64, incoming bool) *ir.Object {
	o := &ir.Object{
		Name:  fmt.Sprintf("%s%d", what, n),
		Type:  typ,
		Class: ir.ClassLocal,
	}
	a.f.ABI.Homes = append(a.f.ABI.Homes, ABIHome{Obj: o, Off: off, Incoming: incoming})
	return o
}

// index builds the reader lists. Value.uses is construction bookkeeping and is
// documented as stale, so the graph is walked here instead.
func (a *abiPass) index() {
	n := a.f.NumValues()
	a.users = make([][]*Value, n)
	a.dead = make([]bool, n)
	a.touched = make([]bool, a.f.NumBlocks())
	a.before = make(map[ID][]*Value)
	for _, b := range a.f.Blocks {
		for _, v := range b.Values {
			for _, arg := range v.Args {
				if arg != nil && int(arg.ID) < n {
					a.users[arg.ID] = append(a.users[arg.ID], v)
				}
			}
		}
		if b.Control != nil && int(b.Control.ID) < n {
			// A block reads its control value. Recording it keeps a value
			// with a reader outside the value lists from looking unread.
			a.users[b.Control.ID] = append(a.users[b.Control.ID], b.Control)
		}
	}
}

// collectArgs groups the entry block's OpArg values by the parameter they
// name, in the order the parameters appear.
//
// The order is the entry block's, which specs/021-ssa-construction.md fills in
// parameter order and decomposition rewrites in place. It is a walk of a slice
// and never of a map, which specs/053-determinism.md requires of anything that
// reaches the output.
func (a *abiPass) collectArgs() {
	index := make(map[*ir.Object]int)
	for _, v := range a.f.Entry.Values {
		if v.Op != OpArg {
			continue
		}
		o, _ := v.Aux.(*ir.Object)
		if o == nil || o.Type == nil {
			// An argument that names no parameter is its own parameter. A
			// function specs/021-ssa-construction.md built always names one;
			// one built by hand, which the tests of the passes below do, need
			// not, and giving it a name here is what lets every consumer ask
			// the same question of every argument.
			o = &ir.Object{
				Name:  fmt.Sprintf("arg%d", len(a.objs)),
				Type:  v.Type,
				Class: ir.ClassLocal,
			}
			v.Aux = o
		}
		i, ok := index[o]
		if !ok {
			i = len(a.objs)
			index[o] = i
			a.objs = append(a.objs, o)
			a.args = append(a.args, nil)
		}
		a.args[i] = append(a.args[i], v)
	}
}

// resultTypes returns the types the function returns, from the first block
// that returns.
//
// ir.Type carries no result list for a function kind, so the values a return
// passes are the only description of the signature that reaches here. Every
// return of one function passes the same types, so the first one answers for
// all of them.
func (a *abiPass) resultTypes() []*ir.Type {
	for _, b := range a.f.Blocks {
		if b.Kind != BlockRet || b.Control == nil || b.Control.Op != OpMakeResult {
			continue
		}
		return abiOperandTypes(b.Control, 0)
	}
	return nil
}

// abiOperandTypes returns the types of v's operands from lo, without the
// memory argument.
func abiOperandTypes(v *Value, lo int) []*ir.Type {
	args := v.Args
	if len(args) > 0 && IsMemory(args[len(args)-1]) {
		args = args[:len(args)-1]
	}
	if lo > len(args) {
		lo = len(args)
	}
	args = args[lo:]
	out := make([]*ir.Type, 0, len(args))
	for _, arg := range args {
		out = append(out, arg.Type)
	}
	return out
}

// ---------------------------------------------------------------------------
// The rewrites

// rewriteArgs makes each parameter's placement realisable.
//
// The shape is a parameter that decomposition left whole because it has more
// parts than MaxDecomposeParts. The entry block holds
//
//	v1 = Arg <T> {p}
//	v2 = LocalAddr <*T> {p} mem
//	v3 = Store <mem> [size] v2 v1 mem
//
// which is the frame home specs/021-ssa-construction.md gives every aggregate
// parameter, filled from the incoming value.
//
// When the convention puts the parameter in registers, the store becomes one
// store per register, which is the work decomposition stopped short of.
//
// When the convention puts it in the argument area, the store copies the value
// onto itself, because the destination becomes the source once the object's
// home is that area. Both the store and the argument go, and the object leaves
// the locals.
func (a *abiPass) rewriteArgs() {
	for i, o := range a.objs {
		av := &a.f.ABI.In[i]
		if len(a.args[i]) != 1 {
			continue // decomposition already split it
		}
		v := a.args[i][0]
		if v.Type != o.Type || v.AuxInt != 0 || !Multiword(v.Type) {
			continue
		}
		st := a.selfStore(v, o)
		if st == nil {
			continue
		}
		if av.InReg {
			a.splitArg(v, st, av)
			continue
		}
		a.homeInArgs(v, st, av)
	}
}

// selfStore returns the store that writes an incoming parameter into its frame
// home, or nil when the argument is read any other way.
//
// The match is exact on purpose. A looser one would delete a store that writes
// something else, or one whose destination is not the parameter's own slot.
func (a *abiPass) selfStore(arg *Value, o *ir.Object) *Value {
	if int(arg.ID) >= len(a.users) || len(a.users[arg.ID]) != 1 {
		return nil
	}
	st := a.users[arg.ID][0]
	if st.Op != OpStore || len(st.Args) != 3 || st.Args[1] != arg {
		return nil
	}
	if st.AuxInt != o.Type.Size || st.Block != arg.Block {
		return nil
	}
	dst := st.Args[0]
	if dst.Op != OpLocalAddr {
		return nil
	}
	if d, ok := dst.Aux.(*ir.Object); !ok || d != o {
		return nil
	}
	return st
}

// splitArg replaces one whole argument by one argument per register.
//
// The original store stays and becomes the store of the last part, so every
// value that read the memory it produced still reads the same value and no
// memory chain has to be repaired.
func (a *abiPass) splitArg(arg, st *Value, av *ABIValue) {
	b := arg.Block
	dst, mem := st.Args[0], st.Args[2]
	if len(av.Parts) == 0 {
		// A zero-size parameter occupies no register and no word, so the
		// store writes nothing and goes with the argument.
		a.replaceValue(st, mem)
		a.kill(st)
		a.kill(arg)
		return
	}
	m := mem
	for i := range av.Parts {
		p := &av.Parts[i]
		part := a.mk(st, b, arg.Pos, OpArg, p.Type)
		part.Aux = arg.Aux
		part.AuxInt = p.Off
		addr := a.partAddr(st, b, arg.Pos, dst, p)
		if i == len(av.Parts)-1 {
			// The last store is the original one, so its identifier and its
			// readers are untouched.
			setArgs(st, addr, part, m)
			st.AuxInt = p.Type.Size
			break
		}
		s := a.mk(st, b, arg.Pos, OpStore, MemType, addr, part, m)
		s.AuxInt = p.Type.Size
		m = s
	}
	a.kill(arg)
}

// homeInArgs makes the incoming argument area the parameter's storage.
//
// The copy the entry block made writes the value over itself once the
// destination is the source, so it goes. The object leaves Func.Frame: the
// frame layout must not give it a second slot, and the collector reads it
// through the arguments bitmap instead of the locals bitmap.
func (a *abiPass) homeInArgs(arg, st *Value, av *ABIValue) {
	av.Home = true
	a.replaceValue(st, st.Args[2])
	a.kill(st)
	a.kill(arg)
	for i, o := range a.f.Frame {
		if o == av.Obj {
			a.f.Frame = append(a.f.Frame[:i:i], a.f.Frame[i+1:]...)
			break
		}
	}
}

// rewriteResults writes a result the register set cannot hold into the result
// area of the caller's frame.
//
// Go's convention puts such a result in the incoming argument area, after the
// arguments the registers could not hold. The callee writes it there, so it
// returns nothing in a register and the value stops being an operand of the
// return.
//
// The copy is placed directly ahead of the return, and that placement is what
// makes the arguments bitmap of specs/027-liveness-and-stackmaps.md right. The
// bitmap describes the result area as holding no pointer, because at every
// safepoint of this function the result has not been written yet. A safepoint
// after the copy would see live pointers in words the map calls free. gc's
// bitmap for such a function is the same all-zero map over the same width.
func (a *abiPass) rewriteResults() {
	abi := a.f.ABI
	n := 0
	for _, b := range a.f.Blocks {
		if b.Kind != BlockRet || b.Control == nil || b.Control.Op != OpMakeResult {
			continue
		}
		mr := b.Control
		args := mr.Args
		if len(args) > 0 && IsMemory(args[len(args)-1]) {
			args = args[:len(args)-1]
		}
		if len(args) != len(abi.Out) {
			continue
		}
		mem := a.memoryOf(mr)
		if mem == nil {
			continue
		}
		keep := make([]*Value, 0, len(mr.Args))
		for i := range abi.Out {
			av := &abi.Out[i]
			var src *Value
			if !av.InReg && av.Type.Size > 0 {
				src = a.addressOf(mr, args[i])
			}
			if src == nil {
				// Either the result travels in registers, or it is not a
				// value this pass can take the address of. The second keeps
				// the operand, and the code generator then refuses the
				// function, which is a refusal with a place to look rather
				// than a copy of the wrong bytes.
				keep = append(keep, args[i])
				continue
			}
			o := a.home("~r", n, av.Type, av.Off, true)
			n++
			mem = a.copyInto(mr, b, o, av.Type, src, mem)
			a.kill(args[i])
		}
		if len(keep) == len(args) {
			continue
		}
		setArgs(mr, append(keep, mem)...)
	}
}

// addressOf returns the address the value at a boundary can be copied from,
// or nil when there is none.
//
// A value wider than a register reaches a boundary as a load out of memory,
// which specs/021-ssa-construction.md is the only producer of: an aggregate
// never lives in a value. The load must have this boundary as its only reader,
// or the value is wanted elsewhere too and moving the read would be a second
// copy rather than a placement.
func (a *abiPass) addressOf(user, v *Value) *Value {
	if v == nil || v.Op != OpLoad || len(v.Args) != 2 || v.Block != user.Block {
		return nil
	}
	if int(v.ID) >= len(a.users) || len(a.users[v.ID]) != 1 {
		return nil
	}
	return v.Args[0]
}

// memoryOf returns the memory a value reads.
func (a *abiPass) memoryOf(v *Value) *Value {
	if len(v.Args) == 0 {
		return nil
	}
	m := v.Args[len(v.Args)-1]
	if !IsMemory(m) {
		return nil
	}
	return m
}

// copyInto inserts a block move of a value into an argument-area slot ahead of
// anchor, and returns the memory the move produced.
//
// The move is a call to runtime.memmove once selection has run
// (specs/031-runtime-lowering.md), and that call is a safepoint at which the
// argument area is described by nobody. It is safe for the reason the runtime
// never stops there: memmove is NOSPLIT and carries no stack map, so it is
// neither an asynchronous safepoint nor a place the stack can grow. gc reaches
// the same place by a different road, an inline copy with no call at all,
// which is what specs/042's group 8 leaves open.
func (a *abiPass) copyInto(anchor *Value, b *Block, o *ir.Object, typ *ir.Type, src, mem *Value) *Value {
	dst := a.mk(anchor, b, anchor.Pos, OpLocalAddr, abiPtrTo(typ), mem)
	dst.Aux = o
	mv := a.mk(anchor, b, anchor.Pos, OpMove, MemType, dst, src, mem)
	mv.AuxInt = typ.Size
	return mv
}

// rewriteBoundaries places every value that crosses a call boundary whole.
//
// A call that passes an aggregate, and a return that returns one, hold it as
// one operand when decomposition stopped at its bound. The convention gives it
// several registers, and one value cannot be in several registers, so the
// operand becomes one load per register. When the registers cannot hold it at
// all, it is copied into the outgoing area instead and stops being an operand.
func (a *abiPass) rewriteBoundaries() {
	for _, b := range a.f.Blocks {
		for _, v := range b.Values {
			if a.isDead(v) {
				continue
			}
			if !v.Op.IsCall() && v.Op != OpMakeResult {
				continue
			}
			a.splitOperands(v)
		}
	}
}

// splitOperands places the operands of one call or return.
//
// It records the placement whenever it changes the operand list, because the
// list then stops describing what the call passes: a copied value is not in it
// at all, and a value split into one operand per register has a spill slot the
// parts do not add up to, since the value's own alignment padding is in it and
// theirs is not. Every reader of the placement, the register allocator and the
// code generator both, takes the record rather than walking the list again.
func (a *abiPass) splitOperands(v *Value) {
	lo := abiCallPrefix(v)
	types := abiOperandTypes(v, lo)
	var place []ABIValue
	var size int64
	var err error
	if v.Op == OpMakeResult {
		place, size, err = ABIResults(a.t, types)
	} else {
		place, size, err = ABIArgs(a.t, types)
	}
	if err != nil || len(place) != len(v.Args)-lo-a.memArgs(v) {
		return
	}
	mem := a.memoryOf(v)

	rec := &ABICall{Size: size, Vals: make([]ABIValue, 0, len(place))}
	args := make([]*Value, 0, len(v.Args)+len(place))
	args = append(args, v.Args[:lo]...)
	changed := false
	copies := 0
	for i := range place {
		av := &place[i]
		arg := v.Args[lo+i]
		switch {
		case av.InReg && len(av.Parts) >= 2 && Multiword(arg.Type):
			parts := a.loadParts(v, arg, av)
			if parts == nil {
				args = append(args, arg)
				rec.Vals = append(rec.Vals, *av)
				continue
			}
			args = append(args, parts...)
			// One entry per operand, each one word wide, at the offset the
			// whole value's slot puts it. That is where the callee spills the
			// register the word arrived in.
			for j := range av.Parts {
				p := &av.Parts[j]
				rec.Vals = append(rec.Vals, ABIValue{
					Type:  p.Type,
					Off:   av.Off + p.Off,
					Parts: []ABIPart{{Off: 0, Type: p.Type, Reg: p.Reg}},
					InReg: true,
				})
			}
			changed = true

		case !av.InReg && av.Type.Size > 0 && mem != nil && v.Op != OpMakeResult:
			src := a.addressOf(v, arg)
			if src == nil {
				args = append(args, arg)
				rec.Vals = append(rec.Vals, *av)
				continue
			}
			o := a.home("~a", copies, av.Type, av.Off, false)
			copies++
			mem = a.copyInto(v, v.Block, o, av.Type, src, mem)
			a.kill(arg)
			c := *av
			c.Copied = true
			rec.Vals = append(rec.Vals, c)
			changed = true

		default:
			args = append(args, arg)
			rec.Vals = append(rec.Vals, *av)
		}
	}
	if !changed {
		return
	}
	args = append(args, v.Args[lo+len(place):]...)
	if mem != nil {
		// The copies wrote into the area, so the call reads the memory they
		// produced and not the memory they read.
		args[len(args)-1] = mem
	}
	setArgs(v, args...)
	if int(v.ID) < len(a.f.ABI.Calls) {
		a.f.ABI.Calls[v.ID] = rec
	}
}

// memArgs counts the memory operand a value carries, which is one or none.
func (a *abiPass) memArgs(v *Value) int {
	if a.memoryOf(v) != nil {
		return 1
	}
	return 0
}

// loadParts returns one load per register of a whole aggregate operand, or nil
// when the operand cannot be split.
func (a *abiPass) loadParts(user, arg *Value, av *ABIValue) []*Value {
	base := a.addressOf(user, arg)
	if base == nil {
		return nil
	}
	mem := arg.Args[1]
	out := make([]*Value, 0, len(av.Parts))
	for i := range av.Parts {
		p := &av.Parts[i]
		addr := a.partAddr(user, user.Block, arg.Pos, base, p)
		out = append(out, a.mk(user, user.Block, arg.Pos, OpLoad, p.Type, addr, mem))
	}
	a.kill(arg)
	return out
}

// rewriteCallResults makes a call's results readable at the call site.
//
// It is the mirror of rewriteArgs. Decomposition splits a call result into one
// value per machine word up to MaxDecomposeParts and leaves a wider one whole,
// so the call site holds
//
//	v1 = SelectN <T> [0] call
//	v2 = LocalAddr <*T> {x} mem
//	v3 = Store <mem> [size] v2 v1 mem
//
// with T wider than any store the machine has. The convention gives the result
// one register per word, so the read becomes one SelectN per register and the
// store becomes one store per word, which is the work decomposition stopped
// short of. The callee's half is already done: splitOperands turns the same
// value into one operand of the return per word.
//
// The index of a SelectN is the machine word of the result area it reads, so
// splitting one result renumbers every result after it. The walk therefore
// takes a call's whole result list or none of it, and the list has to be
// complete for the reason decomposition needs it complete: the word a result
// starts at is the sum of the widths of the results before it.
func (a *abiPass) rewriteCallResults() error {
	for _, b := range a.f.Blocks {
		for _, v := range b.Values {
			if a.isDead(v) || !v.Op.IsCall() {
				continue
			}
			if err := a.callResults(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// callResults splits and renumbers the results of one call.
//
// It reports an error for a result the call site cannot read, rather than
// leaving a store no rule lowers. Lowering panics on such a store by the
// design of specs/025-lowering-and-rules.md, so the choice here is between a
// refusal that names the function and a crash that names nothing.
func (a *abiPass) callResults(call *Value) error {
	sel, ok := a.resultReads(call)
	if !ok {
		return a.unreadableResult(call, "the call site does not read one value per result")
	}
	types := make([]*ir.Type, 0, len(sel))
	for _, v := range sel {
		types = append(types, v.Type)
	}
	place, _, err := ABIResults(a.t, types)
	if err != nil || len(place) != len(sel) {
		return a.unreadableResult(call, "the convention does not place the call's results")
	}

	// The survey runs first, because a result the walk below cannot place has
	// to stop the function rather than renumber half of a list.
	split := false
	for i := range place {
		av := &place[i]
		if !Multiword(sel[i].Type) {
			continue
		}
		switch {
		case len(av.Parts) < 2:
			// One word or none, which every store the machine has can write.
		case !av.InReg:
			return a.resultRefusal(sel[i], "the result registers cannot hold it, and reading it back from the frame is not built")
		case a.resultStore(sel[i]) == nil:
			return a.resultRefusal(sel[i], "the call site does not write it to one place, so its words have nowhere to go")
		default:
			split = true
		}
	}
	if !split {
		// Decomposition placed every result already, and renumbering would
		// move each index onto itself.
		return nil
	}

	word := int64(0)
	for i := range place {
		av := &place[i]
		v := sel[i]
		if Multiword(v.Type) && av.InReg && len(av.Parts) >= 2 {
			a.splitResult(v, a.resultStore(v), av, word)
			word += int64(len(av.Parts))
			continue
		}
		// A result this pass leaves whole is one value and one word, which is
		// the width decomposition counted for it.
		v.AuxInt = word
		word++
	}
	return nil
}

// resultReads returns a call's results in index order, or false when the list
// is not one live SelectN per result in the call's own block.
//
// The block matters: the code generator names a call's results by walking the
// call's block, so a result read anywhere else is a result it cannot place.
func (a *abiPass) resultReads(call *Value) ([]*Value, bool) {
	if int(call.ID) >= len(a.users) {
		return nil, false
	}
	var sel []*Value
	for _, u := range a.users[call.ID] {
		if a.isDead(u) || u.Op != OpSelectN || len(u.Args) == 0 || u.Args[0] != call {
			continue
		}
		if u.Block != call.Block {
			return nil, false
		}
		sel = append(sel, u)
	}
	if len(sel) == 0 {
		return nil, false
	}
	out := make([]*Value, len(sel))
	for _, v := range sel {
		i := v.AuxInt
		if i < 0 || int(i) >= len(out) || out[i] != nil {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// resultStore returns the store that writes a whole call result into one
// place, or nil when the result is read any other way.
//
// The match is the one selfStore makes for an incoming parameter: the result
// has exactly one reader, that reader is a store of the whole value, and the
// two are in one block so that the words are written where the result is read.
func (a *abiPass) resultStore(sel *Value) *Value {
	if int(sel.ID) >= len(a.users) {
		return nil
	}
	var st *Value
	for _, u := range a.users[sel.ID] {
		if a.isDead(u) {
			continue
		}
		if st != nil {
			return nil
		}
		st = u
	}
	if st == nil || st.Op != OpStore || len(st.Args) != 3 || st.Args[1] != sel {
		return nil
	}
	if sel.Type == nil || st.AuxInt != sel.Type.Size || st.Block != sel.Block {
		return nil
	}
	return st
}

// splitResult replaces one whole call result by one result per register.
//
// The original store stays and becomes the store of the last word, so every
// value that read the memory it produced still reads the same value and no
// memory chain has to be repaired. base is the word of the result area the
// result starts at, which is what the index of a SelectN means from here on.
//
// Every read is made before any store, and both halves of that matter. A
// SelectN reads the memory the call produced, so one placed after a store of
// this chain would read a memory that store has already superseded, which is
// the one-memory-per-point invariant the verifier checks. The reads take the
// place of the read they replace, and the stores take the place of the store.
func (a *abiPass) splitResult(sel, st *Value, av *ABIValue, base int64) {
	call := sel.Args[0]
	b := sel.Block
	dst, mem := st.Args[0], st.Args[2]
	parts := make([]*Value, 0, len(av.Parts))
	for i := range av.Parts {
		p := &av.Parts[i]
		part := a.mk(sel, b, sel.Pos, OpSelectN, p.Type, call)
		part.AuxInt = base + int64(i)
		parts = append(parts, part)
	}
	m := mem
	for i := range av.Parts {
		p := &av.Parts[i]
		addr := a.partAddr(st, b, sel.Pos, dst, p)
		if i == len(av.Parts)-1 {
			// The last store is the original one, so its identifier and its
			// readers are untouched.
			setArgs(st, addr, parts[i], m)
			st.AuxInt = p.Type.Size
			break
		}
		s := a.mk(st, b, sel.Pos, OpStore, MemType, addr, parts[i], m)
		s.AuxInt = p.Type.Size
		m = s
	}
	a.kill(sel)
}

// unreadableResult turns a call whose result list this pass cannot measure
// into an error, but only when the list holds a result that would reach
// lowering as a store no rule has.
//
// A call whose results are each one word is untouched by this pass, so a list
// it cannot measure is then not a fault. It becomes one as soon as one result
// is wider than a register, because the word a later result reads then depends
// on a width nothing here can establish.
func (a *abiPass) unreadableResult(call *Value, why string) error {
	if int(call.ID) >= len(a.users) {
		return nil
	}
	for _, u := range a.users[call.ID] {
		if a.isDead(u) || u.Op != OpSelectN || len(u.Args) == 0 || u.Args[0] != call {
			continue
		}
		if !Multiword(u.Type) {
			continue
		}
		if parts, complete := ABILeaves(u.Type); complete && len(parts) < 2 {
			continue
		}
		return a.resultRefusal(u, why)
	}
	return nil
}

// resultRefusal names the function, the value and the type of a result the
// call site cannot read.
func (a *abiPass) resultRefusal(sel *Value, why string) error {
	return fmt.Errorf("ssa: abi: %s: v%d: a call returns %s and %s",
		a.f.Name, sel.ID, abiTypeName(sel.Type), why)
}

// abiTypeName names a type in a refusal, in the user's terms where the type
// has a name.
func abiTypeName(t *ir.Type) string {
	if t == nil {
		return "no type"
	}
	if t.Name != "" {
		return t.Name
	}
	return fmt.Sprintf("a %d-byte value", t.Size)
}

// abiCallPrefix returns the number of operands in front of a call's arguments.
//
// Selection puts the entry point of an indirect call there, and the word after
// it is the closure, which travels in the closure register rather than in the
// argument sequence, or the interface table, which the entry point was loaded
// out of and which is not passed at all.
func abiCallPrefix(v *Value) int {
	switch v.Op {
	case OpARM64CALLclosure, OpARM64CALLinter:
		return 2
	case OpClosureCall, OpInterCall:
		return 1
	}
	return 0
}

// partAddr returns the address of one part, reusing the base at offset zero.
func (a *abiPass) partAddr(anchor *Value, b *Block, pos syntax.Pos, base *Value, p *ABIPart) *Value {
	if p.Off == 0 {
		return base
	}
	v := a.mk(anchor, b, pos, OpOffPtr, abiPtrTo(p.Type), base)
	v.AuxInt = p.Off
	return v
}

// replaceValue points every reader of old at repl.
//
// Removing a value that produces memory breaks the chain that orders the side
// effects, and nothing below repairs it, so it is repaired here.
func (a *abiPass) replaceValue(old, repl *Value) {
	if int(old.ID) >= len(a.users) {
		return
	}
	for _, u := range a.users[old.ID] {
		for i, arg := range u.Args {
			if arg == old {
				u.SetArg(i, repl)
			}
		}
	}
	for _, b := range a.f.Blocks {
		if b.Control == old {
			b.Control = repl
		}
	}
}

// mk makes a value and schedules it directly ahead of anchor.
func (a *abiPass) mk(anchor *Value, b *Block, pos syntax.Pos, op Op, t *ir.Type, args ...*Value) *Value {
	v := a.f.newValue(op, t, pos)
	v.Block = b
	for _, arg := range args {
		v.AddArg(arg)
	}
	a.before[anchor.ID] = append(a.before[anchor.ID], v)
	a.touched[b.ID] = true
	return v
}

func (a *abiPass) kill(v *Value) {
	for int(v.ID) >= len(a.dead) {
		a.dead = append(a.dead, false)
	}
	a.dead[v.ID] = true
	a.touched[v.Block.ID] = true
}

func (a *abiPass) isDead(v *Value) bool {
	return int(v.ID) < len(a.dead) && a.dead[v.ID]
}

// compact rebuilds the blocks this pass changed.
//
// The values it made are placed directly ahead of the value they were made
// for, which is where they have to be: each is an argument, an address or a
// store that the anchor reads. The walk is over the block's own value list, so
// the order does not come from a map.
func (a *abiPass) compact() {
	for _, b := range a.f.Blocks {
		if int(b.ID) >= len(a.touched) || !a.touched[b.ID] {
			continue
		}
		out := make([]*Value, 0, len(b.Values))
		for _, v := range b.Values {
			if add := a.before[v.ID]; len(add) > 0 {
				out = append(out, add...)
			}
			if a.isDead(v) {
				continue
			}
			out = append(out, v)
		}
		b.Values = out
	}
}

// ---------------------------------------------------------------------------
// Pre-colouring

// ABIDefReg returns the register the convention fixes for a value's
// definition.
//
// Two operations have one. An incoming argument arrives where the caller put
// it, and a call result comes back where the callee left it. Everything else
// is free, and the allocator places it around these.
func ABIDefReg(t *Target, v *Value) (Reg, bool) {
	if t == nil || v == nil || v.Block == nil || v.Block.Func == nil {
		return NoReg, false
	}
	switch v.Op {
	case OpArg:
		return v.Block.Func.ABI.ArgReg(v)
	case OpSelectN:
		// A call result is not pre-coloured, and the reason is the
		// allocator rather than the convention.
		//
		// specs/026-register-allocation.md spills a value that is live across
		// a call, because no register survives one, and the scan does that
		// before it looks at the register the convention fixed. The check
		// that runs ahead of the scan does not: it refuses two pre-coloured
		// values whose live ranges meet, even when one of them is going to be
		// spilled and will never hold the register at the point they meet.
		// The shape is ordinary, an argument in R0 and a call result in R0
		// with the argument read after the call, and it refuses 216 functions
		// of the distribution corpus.
		//
		// The code generator moves each result out of its result register at
		// the call, which is what it did before any of this existed, so
		// nothing is wrong; the register merely has to be moved out of rather
		// than kept. ABIResults is still the placement both sides read.
		// Recovering the case needs the pre-scan check to skip a value the
		// scan is going to spill, which is a change to a pass this one does
		// not own.
		return NoReg, false
	}
	return NoReg, false
}

// ABIUseReg returns the register the convention fixes for argument i of a
// value.
//
// A call's operands and a return's values are the cases: the callee reads them
// where the convention says, not where the allocator happened to put them.
// Naming the register here is what keeps a call with more spilled operands
// than there are scratch registers from failing to allocate, and it is what
// lets the code generator move each operand straight into its home.
func ABIUseReg(t *Target, v *Value, i int) (Reg, bool) {
	if t == nil || v == nil || i < 0 || i >= len(v.Args) {
		return NoReg, false
	}
	var out []ABIValue
	var lo int
	var err error
	switch {
	case v.Op.IsCall():
		out, lo, _, err = ABICallArgs(t, v)
	case v.Op == OpMakeResult || v.Op == OpARM64RET:
		out, _, err = ABIResults(t, abiOperandTypes(v, 0))
	default:
		return NoReg, false
	}
	if err != nil {
		return NoReg, false
	}
	av, ok := abiOperandAt(out, i-lo)
	if !ok || !av.InReg || len(av.Parts) != 1 {
		return NoReg, false
	}
	return av.Parts[0].Reg, true
}

// abiOperandAt returns the placement of operand k, skipping the values that
// are in the argument area already and are no longer operands.
func abiOperandAt(vals []ABIValue, k int) (*ABIValue, bool) {
	if k < 0 {
		return nil, false
	}
	n := 0
	for i := range vals {
		if vals[i].Copied {
			continue
		}
		if n == k {
			return &vals[i], true
		}
		n++
	}
	return nil, false
}

// ABICallArgs places the operands of a call and returns the index of the first
// of them in the operand list and the size of the outgoing argument area.
//
// The code generator and the allocator both read this, so a call has one
// placement rather than two that have to agree.
func ABICallArgs(t *Target, v *Value) (out []ABIValue, lo int, size int64, err error) {
	lo = abiCallPrefix(v)
	if v.Block != nil && v.Block.Func != nil {
		if c := v.Block.Func.ABI.CallAt(v.ID); c != nil {
			return c.Vals, lo, c.Size, nil
		}
	}
	// No record, so the operand list still describes what the call passes.
	// Selection creates calls of its own, runtime.memmove among them, and
	// they are placed by the walk like any other.
	out, size, err = ABIArgs(t, abiOperandTypes(v, lo))
	return out, lo, size, err
}
