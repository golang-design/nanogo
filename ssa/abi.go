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

	// Results has one entry per value the call site reads back, in the order
	// the SelectN indices name them, and is nil for a boundary that reads
	// none.
	//
	// It is a second list because a call's results are placed over the
	// callee's declared result list while its operands are placed over the
	// operand list, and a result the registers cannot hold is read out of the
	// area rather than out of a register.
	Results []ABIValue

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
// in the same order. complete is false when the walk stopped at the bound or
// when the convention refuses the type registers at all, and either is enough
// to know the value does not fit in registers.
//
// The parts are returned in both cases. A value in the argument area still
// needs its words described, because the arguments bitmap of
// specs/027-liveness-and-stackmaps.md is built from them.
func ABILeaves(t *ir.Type) (parts []ABIPart, complete bool) {
	parts = abiFlatten(nil, t, 0)
	return parts, len(parts) < abiMaxParts && abiRegisterizable(t)
}

// abiRegisterizable reports whether the convention lets a value of type t
// travel in registers at all.
//
// Go's internal ABI passes an array in registers only when it is "trivial",
// which means a length of zero or one. gc states the rule in
// types.CalcArraySize:
//
//	// ABIInternal only allows "trivial" arrays (i.e., length 0 or 1)
//	// to be passed by register.
//	switch n {
//	case 0:  t.intRegs, t.floatRegs = 0, 0
//	case 1:  t.intRegs, t.floatRegs = elem.intRegs, elem.floatRegs
//	default: t.intRegs, t.floatRegs = math.MaxUint8, math.MaxUint8
//	}
//
// and MaxUint8 is above any register count, so such an array never fits. The
// refusal reaches whatever contains it, because types.CalcStructSize sums its
// fields' counts and caps the sum at MaxUint8 as well. `go tool compile -S`
// agrees on all four edges: a [1]int parameter takes a register, a [0]int
// takes none and shifts nothing, a [2]byte is read from FP, and a struct
// holding a [2]byte is read from FP whole.
//
// Without this rule nanogo gave a [16]byte result sixteen registers and put
// the error that followed it in the frame, which is the opposite of both
// placements gc makes. Inside a nanogo-only program the wrong rule is
// self-consistent, so nothing but a comparison against gc finds it.
func abiRegisterizable(t *ir.Type) bool {
	if t == nil {
		return true
	}
	switch t.Kind {
	case ir.Array:
		if t.Len > 1 {
			return false
		}
		return abiRegisterizable(t.Elem)
	case ir.Struct, ir.Tuple:
		for i := range t.Fields {
			if !abiRegisterizable(t.Fields[i].Type) {
				return false
			}
		}
		return true
	}
	// Every other shape is a scalar, a pointer, or one of the built-in
	// multi-word types abiFlatten spells out, and each of those has a fixed
	// register count.
	return true
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

	if typ.Size == 0 {
		// A zero-size value takes a stack slot and no register. It is step 2
		// of gc's assignment algorithm and it comes before every register
		// rule, so it is written here rather than left to the loop below.
		//
		// The loop would otherwise place it in registers by accident. A
		// zero-size type has no parts, so nothing fails to fit, and the value
		// would be marked InReg having consumed no register at all.
		//
		// It takes no space. What it takes is alignment. abi-internal.md
		// states the reason: the rule exists to keep the internal ABI
		// equivalent to ABI0, where a zero-size value still pads the stack to
		// its own alignment, and without it an architecture with no argument
		// registers would lay the same signature out two ways.
		//
		// This was a miscompile and not a missing feature. gc puts c at FP+8
		// in f(a [3]int8, b [0]int64, c [3]int8), because b aligns the area to
		// 8 before c is placed. nanogo put c at FP+3, so a program whose
		// caller nanogo compiled and whose callee gc compiled read the wrong
		// argument and returned a wrong answer with no diagnostic.
		as.stack = abiAlign(as.stack, typ)
		av.Off = as.stack
		return av, false, nil
	}

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

// ABI0Target returns t with both argument register sets empty.
//
// ABI0 is not a second convention and this is not a second walk.
// cmd/compile/abi-internal.md states the relationship outright: "The ABI
// assignment algorithm above is equivalent to Go's stack-based ABI0 calling
// convention if there are zero architecture registers." So the walk is
// [ABIWalk] with nothing for it to assign, and every clause that makes the two
// agree is already in it. The pointer-alignment field between the arguments
// and the results is the abiRoundUp between the two placeAll calls, the
// trailing one is in finish, and the zero-size rule is in place. The spill
// part disappears on its own, because it holds one slot per value that
// travelled in a register.
//
// A second walk that laid a signature out from the type list directly is ten
// lines and is the wrong ten lines. It would be a second statement of one
// rule, checked against the first by nothing, and specs/030-abi.md records
// what the last divergence of that kind cost.
//
// The copy is shallow and the register sets are the only fields it clears.
// Everything else the walk reads, ClassOf above all, has to be the target's
// own: a class function that disagreed would place a float where the callee
// reads an integer.
func (t *Target) ABI0Target() *Target {
	if t == nil {
		return nil
	}
	abi0 := *t
	abi0.Name = t.Name + "/ABI0"
	abi0.ArgRegs = [NumRegClass][]Reg{}
	abi0.ResultRegs = [NumRegClass][]Reg{}
	return &abi0
}

// CalleeABI0 reports that v calls a symbol defined in assembly under ABI0.
//
// The callee's identity is the *ir.Object in a static call's Aux, which is
// where specs/021-ssa-construction.md puts it and what the lowering rules
// preserve, so the ABI travels with the name rather than in a field of its
// own. An indirect call has no object and is never ABI0: a func value holds
// the address of an ABIInternal entry point.
func CalleeABI0(v *Value) bool {
	if v == nil || !v.Op.IsCall() {
		return false
	}
	o, _ := v.Aux.(*ir.Object)
	return o != nil && o.Assembly
}

// ABITargetOf returns the register sets one call boundary is placed with.
//
// It is the whole of the callee's half of specs/047-abi-wrappers.md. Without
// it the ABIInternal wrapper lays its outgoing area out by the ABIInternal
// walk while the assembly reads ABI0 offsets, which is a caller writing
// arguments where the callee reads none and registers holding the values
// instead. The program links and prints a plausible answer.
func ABITargetOf(t *Target, v *Value) *Target {
	if !CalleeABI0(v) {
		return t
	}
	return t.ABI0Target()
}

// ABIWalk places one function's arguments and results with one assigner, and
// returns the size of the argument area the two need together.
//
// The two lists share the stack part of the area and are separated only by the
// register counters, which restart. gc's abi.ABIAnalyzeFuncType is the same
// walk: it rounds one stack counter to a word between the two lists and takes
// the spill base from where the results left it, not from where the arguments
// did.
//
//	info.inparams  = assignParams(ft.RecvParams(), false)
//	s.stackOffset  = types.RoundUp(s.stackOffset, int64(types.RegSize))
//	s.rUsed        = RegAmounts{}
//	info.outparams = assignParams(ft.Results(), true)
//	info.offsetToSpillArea = alignTo(s.stackOffset, types.RegSize)
//
// The spill base is why the two cannot be walked apart. A result in the stack
// part moves every spill slot above it, so an argument placement made without
// the results puts the slots over words the callee writes its results into.
func ABIWalk(t *Target, argTypes, resTypes []*ir.Type, objs []*ir.Object) (in, out []ABIValue, size int64, err error) {
	if t == nil || t.ClassOf == nil {
		return nil, nil, 0, fmt.Errorf("ssa: abi: no target")
	}
	as := &abiAssigner{t: t, regs: &t.ArgRegs}
	in, spilled, err := as.placeAll(argTypes, objs)
	if err != nil {
		return nil, nil, 0, err
	}
	as.stack = abiRoundUp(as.stack, ir.PtrSize)
	as.next = [NumRegClass]int{}
	as.regs = &t.ResultRegs
	as.isReturn = true
	out, _, err = as.placeAll(resTypes, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	return in, out, as.finish(in, spilled), nil
}

// ABIArgs places a list of argument types and returns the size of the argument
// area they need.
//
// It is the placement of one call's operands: the stack part first, then the
// spill slots the callee writes when it grows the stack. A caller that also
// has results to place must use ABIWalk, because the results sit between the
// two parts.
func ABIArgs(t *Target, types []*ir.Type) ([]ABIValue, int64, error) {
	in, _, size, err := ABIWalk(t, types, nil, nil)
	return in, size, err
}

// ABIResults places a list of result types. The register counters restart,
// which is what specs/030-abi.md means by walking the result list again, and
// nothing is spilled.
//
// The stack part starts at base, which is where the arguments of the same
// boundary left it. It is zero only for a boundary with no argument in the
// stack part.
func ABIResults(t *Target, base int64, types []*ir.Type) ([]ABIValue, int64, error) {
	if t == nil || t.ClassOf == nil {
		return nil, 0, fmt.Errorf("ssa: abi: no target")
	}
	as := &abiAssigner{t: t, regs: &t.ResultRegs, isReturn: true,
		stack: abiRoundUp(base, ir.PtrSize)}
	out, _, err := as.placeAll(types, nil)
	if err != nil {
		return nil, 0, err
	}
	return out, abiRoundUp(as.stack, ir.PtrSize), nil
}

// ABIStackEnd returns where a placement left the stack part of its area.
//
// Only a value the registers could not hold occupies that part, so the walk
// skips the rest: a value in registers carries a spill offset, which is above
// the part and is not what a result continues from.
func ABIStackEnd(vals []ABIValue) int64 {
	end := int64(0)
	for i := range vals {
		av := &vals[i]
		if av.InReg || av.Type == nil {
			continue
		}
		if e := av.Off + av.Type.Size; e > end {
			end = e
		}
	}
	return end
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

	// spills counts the frame slots spillResults has made, so that each one
	// has a name of its own in a dump.
	spills int
}

func (a *abiPass) run() error {
	a.index()
	if a.spillResults() {
		// The spill rewrote operand lists and added values, so the block
		// lists and the reader lists are rebuilt before the walks below read
		// them. Rebuilding is cheaper to be sure of than maintaining, and the
		// spill is the only rewrite this pass makes before the placement.
		a.compact()
		a.index()
	}
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
	//
	// The target is a.own() and not a.t, and the two differ for the ABI0
	// wrapper of specs/047-abi-wrappers.md alone. Every other use of a.t in
	// this pass is a call boundary, where the convention is the callee's and
	// ABITargetOf reads it off the callee. Passing the ABI0 target in as a.t
	// instead would place the wrapper's own boundary correctly and lay the
	// outgoing area of its ABIInternal inner call out with no registers,
	// which is a caller writing its arguments into memory the callee never
	// reads.
	in, out, argsSize, err := ABIWalk(a.own(), types, a.declaredResults(), a.objs)
	if err != nil {
		return fmt.Errorf("ssa: abi: %s: %w", a.f.Name, err)
	}

	a.f.ABI = &ABI{In: in, Out: out, ArgsSize: argsSize, Calls: make([]*ABICall, a.f.NumValues())}
	a.rewriteArgs()
	a.rewriteResults()
	if err := a.rewriteBoundaries(); err != nil {
		return err
	}
	if err := a.rewriteCallResults(); err != nil {
		return err
	}
	a.compact()
	return nil
}

// own returns the register sets this function's own boundary is placed with.
//
// It is [ABITargetOf] for a definition rather than for a call: the same
// question, asked about the symbol being defined instead of about the symbol
// being called. [Func.ABI0] is the answer and the ABI0 wrapper of
// specs/047-abi-wrappers.md is the only function that answers yes.
func (a *abiPass) own() *Target {
	if !a.f.ABI0 {
		return a.t
	}
	return a.t.ABI0Target()
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
	// The declaration first, so that the walk sees every parameter the
	// convention places and not only the ones an instruction still reads.
	//
	// Decomposition deletes the OpArg of a zero-size parameter, because there
	// is no word to decompose it into, and a scan of the entry block therefore
	// cannot see it. It still takes a stack slot at its own alignment
	// (specs/030-abi.md rule 2), and the slot is what moves every parameter
	// and every result after it. Dropping it put c at 3 in
	// func(a [3]int8, b [0]int64, c [3]int8) int8 where gc puts it at 8, and
	// the result at 8 where gc puts it at 16, in both conventions. The caller
	// placed the same signature correctly, so a nanogo callee disagreed with a
	// nanogo caller as well as with gc, and nothing said so.
	for _, o := range a.f.Params {
		if o == nil || o.Type == nil {
			// An incomplete declaration is no declaration. Fall back to the
			// values, which is what declaredResults does with an incomplete
			// signature, rather than place a parameter with no type.
			a.objs, a.args, index = nil, nil, make(map[*ir.Object]int)
			break
		}
		index[o] = len(a.objs)
		a.objs = append(a.objs, o)
		a.args = append(a.args, nil)
	}
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

// declaredResults returns the list the convention places this function's
// results over, which is the declared result list and not the values a return
// passes.
//
// The two are not the same list. Decomposition has run, so a return passes one
// value per machine word of a result it split, and specs/030-abi.md assigns
// all-or-nothing per entry of the list it walks. A list of words and a list of
// declared results therefore give the same registers in the same order only
// while the register set holds them both. Above that they part: gc gives
// (Collected, error) fifteen registers for the struct and puts the whole error
// in the frame, and a walk of the words puts the itab in the sixteenth
// register and only the data word in the frame. That is a callee writing one
// half of its error where its caller reads neither.
//
// A function assembled value by value carries no signature, which the tests of
// the passes below do, and the values a return passes are then the only
// description there is.
func (a *abiPass) declaredResults() []*ir.Type {
	t := a.f.Type
	if t == nil || t.Kind != ir.FuncKind {
		return a.resultTypes()
	}
	out := make([]*ir.Type, 0, len(t.Results))
	for _, r := range t.Results {
		if r == nil {
			return a.resultTypes()
		}
		out = append(out, r)
	}
	return out
}

// resultTypes returns the types the values of the first return have.
func (a *abiPass) resultTypes() []*ir.Type {
	for _, b := range a.f.Blocks {
		if b.Kind != BlockRet || b.Control == nil || b.Control.Op != OpMakeResult {
			continue
		}
		return abiOperandTypes(b.Control, 0)
	}
	return nil
}

// abiOperands returns v's operands from lo, without the memory argument.
func abiOperands(v *Value, lo int) []*Value {
	args := v.Args
	if len(args) > 0 && IsMemory(args[len(args)-1]) {
		args = args[:len(args)-1]
	}
	if lo > len(args) {
		lo = len(args)
	}
	return args[lo:]
}

// abiOperandTypes returns the types of v's operands from lo, without the
// memory argument.
func abiOperandTypes(v *Value, lo int) []*ir.Type {
	args := abiOperands(v, lo)
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
			if !av.InReg {
				// Decomposition split the parameter and the convention put
				// it in the area, so the copy is one store per word and the
				// rewrite below is homeInArgs read over that list.
				a.homeSplitInArgs(o, a.args[i], av)
			}
			continue
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
	a.leaveFrame(av.Obj)
}

// homeSplitInArgs is homeInArgs for a parameter decomposition already split.
//
// The two are one rule of specs/030-abi.md read over two shapes of the entry
// block. A parameter the registers cannot hold keeps the storage the caller
// wrote it into, so the copy specs/021-ssa-construction.md made into a frame
// slot writes the value over itself and goes. Where decomposition left the
// parameter whole that copy is one store, which homeInArgs deletes. Where
// decomposition split it the copy is one store per word, and this deletes the
// whole chain of them.
//
// Without it the parameter kept a second slot in the locals and the entry
// block kept a load per word to fill it. Those loads are the first thing the
// function does, so the allocator gives them the registers it believes are
// free, and the registers the caller left the *following* arguments in are
// exactly the ones nothing in the graph says are taken yet: an OpArg is live
// from the caller's store and the allocator's range for it begins at its own
// definition. A method with a wide value receiver is the smallest case, and
// Go's own test/method5.go is where it prints the receiver's words in place of
// its arguments.
func (a *abiPass) homeSplitInArgs(o *ir.Object, args []*Value, av *ABIValue) {
	sts := a.selfStoreChain(o, args, av)
	if sts == nil {
		return
	}
	av.Home = true
	// The stores are a chain, so the memory the first one read is the memory
	// every reader of the last one reads once they have all gone.
	a.replaceValue(sts[len(sts)-1], sts[0].Args[2])
	for _, st := range sts {
		a.kill(st)
	}
	for _, arg := range args {
		a.kill(arg)
	}
	a.leaveFrame(av.Obj)
}

// selfStoreChain returns the stores that write the words of one decomposed
// parameter into its frame home, in word order, or nil when the words are read
// any other way.
//
// It is selfStore over a list, and it is exact for the same reason: a looser
// match would delete a store that writes something else, or one whose
// destination is not this parameter's own slot. The word order is the
// placement's, matched on the byte offset each OpArg carries, so a list in any
// other order is not this shape. Every store but the last has to be read by
// the next one alone, because only the last one's readers are moved.
func (a *abiPass) selfStoreChain(o *ir.Object, args []*Value, av *ABIValue) []*Value {
	if o == nil || len(args) == 0 || len(args) != len(av.Parts) {
		return nil
	}
	out := make([]*Value, 0, len(args))
	for k := range av.Parts {
		p := &av.Parts[k]
		arg := args[k]
		if p.Type == nil || arg.Type == nil || arg.AuxInt != p.Off || arg.Type.Size != p.Type.Size {
			return nil
		}
		if int(arg.ID) >= len(a.users) || len(a.users[arg.ID]) != 1 {
			return nil
		}
		st := a.users[arg.ID][0]
		if st.Op != OpStore || len(st.Args) != 3 || st.Args[1] != arg {
			return nil
		}
		if st.AuxInt != p.Type.Size || st.Block != arg.Block {
			return nil
		}
		if !abiAddressesPart(st.Args[0], o, p.Off) {
			return nil
		}
		if k > 0 {
			prev := out[k-1]
			if st.Args[2] != prev {
				return nil
			}
			if int(prev.ID) >= len(a.users) || len(a.users[prev.ID]) != 1 {
				return nil
			}
		}
		out = append(out, st)
	}
	return out
}

// abiAddressesPart reports whether dst is the address of the word at off of
// o's own slot.
//
// specs/021-ssa-construction.md names the slot with OpLocalAddr and every word
// above the first is reached by an OpOffPtr on it, which is the address
// arithmetic partAddr makes and decomposition leaves behind.
func abiAddressesPart(dst *Value, o *ir.Object, off int64) bool {
	if dst == nil {
		return false
	}
	if dst.Op == OpOffPtr && len(dst.Args) == 1 {
		if dst.AuxInt != off {
			return false
		}
		dst = dst.Args[0]
		off = 0
	}
	if off != 0 || dst == nil || dst.Op != OpLocalAddr {
		return false
	}
	d, ok := dst.Aux.(*ir.Object)
	return ok && d == o
}

// leaveFrame drops a parameter whose storage is the incoming argument area
// from the locals.
//
// The frame layout must not give it a second slot, and the collector reads it
// through the arguments bitmap instead of the locals bitmap.
func (a *abiPass) leaveFrame(o *ir.Object) {
	for i, have := range a.f.Frame {
		if have == o {
			a.f.Frame = append(a.f.Frame[:i:i], a.f.Frame[i+1:]...)
			return
		}
	}
}

// abiResultSpans says how many of the values a return passes, or a call site
// reads, each entry of a placement occupies.
//
// The placement is over the declared results and the values are the machine
// words decomposition left in their place, so the two lists have different
// lengths and one has to be mapped onto the other. Decomposition states the
// rule this reads back (ssa/decompose.go, number): a result it split is one
// value per part, and a result it left whole is one value whatever its type
// is. The two are told apart by the value's own type, because every part of a
// split value is a scalar or a pointer and only a whole aggregate is
// multiword.
//
// The walk has to consume the value list exactly. A mapping that ended early
// or ran past the end would put a later result over words that hold an earlier
// one, so the answer is refused instead and the caller falls back to the list
// it can measure.
func abiResultSpans(place []ABIValue, vals []*Value) ([]int, bool) {
	spans := make([]int, len(place))
	j := 0
	for i := range place {
		t := place[i].Type
		if t == nil {
			return nil, false
		}
		switch {
		case j < len(vals) && vals[j] != nil && vals[j].Type != nil &&
			Multiword(vals[j].Type) && vals[j].Type.Size == t.Size:
			// One value of the result's own type, which decomposition either
			// left whole or never split because it has no parts.
			spans[i] = 1
		case !Multiword(t):
			spans[i] = 1
		default:
			spans[i] = len(place[i].Parts)
		}
		j += spans[i]
		if j > len(vals) {
			return nil, false
		}
	}
	if j != len(vals) {
		return nil, false
	}
	return spans, true
}

// abiWordValue is the placement of one word of a value the convention placed
// as a whole.
//
// A result decomposition already split arrives at the boundary as one value
// per word, and each of those needs a placement of its own: the register its
// part travels in, or the word of the area the part sits at. The offset is the
// value's own offset plus the part's, because the parts of one value are
// consecutive words of the value and the value is one run of the area.
func abiWordValue(av *ABIValue, k int) ABIValue {
	p := &av.Parts[k]
	return ABIValue{
		Type:  p.Type,
		Off:   av.Off + p.Off,
		Parts: []ABIPart{{Off: 0, Type: p.Type, Reg: p.Reg}},
		InReg: av.InReg,
	}
}

// rewriteResults places the values one return passes and records the
// placement.
//
// It is splitOperands for a return, and it is a function of its own because a
// return is placed over the declared result list while a call's operands are
// placed over the operand list. The two lists part above sixteen result words,
// which is what declaredResults says.
//
// Three shapes come out of the walk, one per row of specs/030-abi.md's table
// of where the address of a value in memory comes from:
//
//   - A result in registers that reaches here whole takes one load per
//     register, which is the work decomposition stopped short of.
//   - A result the registers cannot hold that reaches here whole is copied
//     into the result area by a block move ahead of the return.
//   - A result the registers cannot hold that decomposition already split
//     stays as its words, each placed on the word of the area it belongs to.
//     The code generator stores those words like any other operand the
//     convention put in the area.
//
// The copies and the loads are threaded on one memory, so a load made after a
// copy reads the memory that copy produced. Both are scheduled at the return
// and the older memory is no longer live there, which is the InvMemChain of
// ssa/verify.go and, below the verifier, a load the scheduler may move across
// a store.
//
// The copy and the stores are the last thing before the return, and that is
// what makes the arguments bitmap of specs/027-liveness-and-stackmaps.md
// right. The bitmap describes the result area as holding no pointer, because
// at every safepoint of this function the result has not been written yet. A
// safepoint after the copy would see live pointers in words the map calls
// free. gc's bitmap for such a function is the same all-zero map over the same
// width.
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
		mem := a.memoryOf(mr)
		if mem == nil {
			continue
		}
		spans, ok := abiResultSpans(abi.Out, args)
		if !ok {
			continue
		}

		// The copies run first, all of them, so that every load the split
		// below makes reads the memory they left. Both are scheduled at the
		// return, and a load placed there reading the older memory is two
		// memory values live at one point, which is the InvMemChain of
		// ssa/verify.go and, below the verifier, a load the scheduler may
		// move across a store.
		copied := make([]bool, len(abi.Out))
		lo := 0
		for i := range abi.Out {
			av := &abi.Out[i]
			at := lo
			lo += spans[i]
			if spans[i] != 1 || av.InReg || av.Type.Size == 0 || len(av.Parts) == 0 {
				continue
			}
			if !Multiword(args[at].Type) {
				continue
			}
			src := a.addressOf(mr, args[at])
			if src == nil {
				// A value this pass cannot take the address of. The operand
				// stays and the code generator refuses the function, which is
				// a refusal with a place to look rather than a copy of the
				// wrong bytes.
				continue
			}
			o := a.home("~r", n, av.Type, av.Off, true)
			n++
			mem = a.copyInto(mr, b, o, av.Type, src, mem)
			a.kill(args[at])
			copied[i] = true
		}

		rec := &ABICall{Size: abi.ArgsSize, Vals: make([]ABIValue, 0, len(args))}
		out := make([]*Value, 0, len(args)+1)
		changed := false
		lo = 0
		for i := range abi.Out {
			av := &abi.Out[i]
			hi := lo + spans[i]
			whole := hi-lo == 1 && args[lo] != nil && Multiword(args[lo].Type)
			switch {
			case copied[i]:
				// In the result area already, and no longer an operand.
				c := *av
				c.Copied = true
				rec.Vals = append(rec.Vals, c)
				changed = true

			case av.Type.Size == 0 || len(av.Parts) == 0:
				// No width, so no register and no word. The operands, if
				// there are any, are placed where they are.
				for k := lo; k < hi; k++ {
					out = append(out, args[k])
					rec.Vals = append(rec.Vals, *av)
				}

			case av.InReg && whole && len(av.Parts) >= 2:
				parts := a.loadParts(mr, args[lo], av, mem)
				if parts == nil {
					out = append(out, args[lo])
					rec.Vals = append(rec.Vals, *av)
					break
				}
				out = append(out, parts...)
				for k := range av.Parts {
					rec.Vals = append(rec.Vals, abiWordValue(av, k))
				}
				changed = true

			case hi-lo == len(av.Parts):
				// One operand per word already. Each takes the register its
				// part travels in, or the word of the area the part sits at.
				for k := lo; k < hi; k++ {
					out = append(out, args[k])
					rec.Vals = append(rec.Vals, abiWordValue(av, k-lo))
				}

			default:
				for k := lo; k < hi; k++ {
					out = append(out, args[k])
					rec.Vals = append(rec.Vals, *av)
				}
			}
			lo = hi
		}
		// The record is kept whether or not the operand list changed, because
		// the placement is over the declared results either way and a walk of
		// the operand list is not it.
		if int(mr.ID) < len(abi.Calls) {
			abi.Calls[mr.ID] = rec
		}
		if !changed {
			continue
		}
		setArgs(mr, append(out, mem)...)
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

// rewriteBoundaries places every value a call passes whole.
//
// A call that passes an aggregate holds it as one operand when decomposition
// stopped at its bound. The convention gives it several registers, and one
// value cannot be in several registers, so the operand becomes one load per
// register. When the registers cannot hold it at all, it is copied into the
// outgoing area instead and stops being an operand.
//
// A return is rewriteResults', because a return is placed over the declared
// result list and a call's operands are placed over the operand list.
func (a *abiPass) rewriteBoundaries() error {
	for _, b := range a.f.Blocks {
		for _, v := range b.Values {
			if a.isDead(v) {
				continue
			}
			if !v.Op.IsCall() {
				continue
			}
			if err := a.splitOperands(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitOperands places the operands of one call.
//
// It is rewriteResults for a call, and it walks the same two lists: the
// callee's declared arguments, which is what the convention is assigned over,
// and the operands, which are the machine words decomposition left in their
// place. abiCallArgs maps one onto the other.
//
// It records the placement whether or not it changes the operand list, because
// the operand list is not the list the placement is over. A copied value is
// not in it at all, a value split into one operand per register has a spill
// slot the parts do not add up to, and a value the registers cannot hold that
// decomposition split is one operand per word over a placement made for the
// whole. Every reader of the placement, the register allocator and the code
// generator both, takes the record rather than walking the list again.
func (a *abiPass) splitOperands(v *Value) error {
	lo := abiCallPrefix(v)
	ops := abiOperands(v, lo)
	types, spans := abiCallArgs(v, ops)
	if types == nil {
		return nil
	}
	// The call's results are walked with its arguments, because the spill
	// slots the size covers sit above both.
	place, _, size, err := ABIWalk(ABITargetOf(a.t, v), types, abiCallResultTypes(v), nil)
	if err != nil || len(place) != len(types) {
		return nil
	}
	mem := a.memoryOf(v)

	rec := &ABICall{Size: size, Vals: make([]ABIValue, 0, len(ops)+len(place))}
	args := make([]*Value, 0, len(v.Args)+len(place))
	args = append(args, v.Args[:lo]...)
	changed := false
	copies := 0
	at := 0
	for i := range place {
		av := &place[i]
		from, to := at, at+spans[i]
		at = to
		// A value decomposition left whole, which is the one shape the copy
		// and the register split below have an address to work from.
		whole := to-from == 1 && ops[from] != nil && Multiword(ops[from].Type)
		switch {
		case av.Type.Size == 0 || len(av.Parts) == 0:
			// No width, so no register and no word. The operands, if there
			// are any, are placed where they are.
			for k := from; k < to; k++ {
				args = append(args, ops[k])
				rec.Vals = append(rec.Vals, *av)
			}

		case av.InReg && whole && len(av.Parts) >= 2:
			parts := a.loadParts(v, ops[from], av, mem)
			if parts == nil {
				args = append(args, ops[from])
				rec.Vals = append(rec.Vals, *av)
				break
			}
			args = append(args, parts...)
			// One entry per operand, each one word wide, at the offset the
			// whole value's slot puts it. That is where the callee spills the
			// register the word arrived in.
			for k := range av.Parts {
				rec.Vals = append(rec.Vals, abiWordValue(av, k))
			}
			changed = true

		case !av.InReg && whole && Multiword(av.Type) && mem != nil:
			src := a.addressOf(v, ops[from])
			if src == nil {
				// One value the machine has no move for, left as an operand
				// for the code generator, which has no move for it either.
				// Reporting it here names the function and the type; leaving
				// it reached "no arm64 rule lowered this operation", which is
				// a panic and names neither.
				return fmt.Errorf("ssa: abi: %s: v%d: an argument of %s does not fit the registers, and the value it is read from is not one this pass can copy out of",
					a.f.Name, ops[from].ID, abiTypeName(av.Type))
			}
			o := a.home("~a", copies, av.Type, av.Off, false)
			copies++
			mem = a.copyInto(v, v.Block, o, av.Type, src, mem)
			a.kill(ops[from])
			c := *av
			c.Copied = true
			rec.Vals = append(rec.Vals, c)
			changed = true

		case to-from == len(av.Parts):
			// One operand per word already. Each takes the register its part
			// travels in, or the word of the area the part sits at. A scalar
			// the registers could not hold is this case with one part, and
			// the code generator stores it into the area.
			for k := from; k < to; k++ {
				args = append(args, ops[k])
				rec.Vals = append(rec.Vals, abiWordValue(av, k-from))
			}

		default:
			for k := from; k < to; k++ {
				args = append(args, ops[k])
				rec.Vals = append(rec.Vals, *av)
			}
		}
	}
	if changed {
		args = append(args, v.Args[lo+len(ops):]...)
		if mem != nil {
			// The copies wrote into the area, so the call reads the memory
			// they produced and not the memory they read.
			args[len(args)-1] = mem
		}
		setArgs(v, args...)
	}
	// The record is kept whether or not the operand list changed, for the
	// reason rewriteResults keeps its own: the placement is over the declared
	// arguments either way and a walk of the operand list is not it.
	if int(v.ID) < len(a.f.ABI.Calls) {
		a.f.ABI.Calls[v.ID] = rec
	}
	return nil
}

// abiCallArgs returns the list the convention places a call's arguments over
// and how many operands each entry of it occupies.
//
// It is the argument half of the rule declaredResults states for the results.
// Assignment is all-or-nothing per entry of the list it walks, so a list of
// machine words and a list of declared arguments give the same registers in
// the same order only while the register set holds them both. Above that they
// part: gc gives a call of fifteen scalar words followed by a two-word struct
// the sixteenth register to nothing and puts the whole struct in the area,
// where a walk of the words puts the struct's first word in that register and
// only its second in the area. That is a caller writing one half of an
// argument where the callee reads neither, and only a comparison against gc
// reports it.
//
// The fallback is the operand list, one entry each, which is the list that was
// walked before the declared one was read and which agrees with the declared
// walk wherever no register file runs out inside one argument. It is taken for
// a call with no signature, which is a call built by hand or created by a pass
// below ssa.Build, and for one whose operands abiCallArgTypes cannot map onto
// the signature.
func abiCallArgs(v *Value, ops []*Value) ([]*ir.Type, []int) {
	if types, spans, ok := abiCallArgTypes(v, ops); ok {
		return types, spans
	}
	types := make([]*ir.Type, 0, len(ops))
	spans := make([]int, 0, len(ops))
	for _, op := range ops {
		if op == nil || op.Type == nil {
			return nil, nil
		}
		types = append(types, op.Type)
		spans = append(spans, 1)
	}
	return types, spans
}

// abiCallArgTypes reads the declared argument list off the callee's signature
// and maps the call's operands onto it.
//
// The signature's parameter list is not the whole of that list. ir.Converter
// keeps a method's receiver out of Params, the way types2 keeps it out of
// Params and in Recv, so the operands of a call to a method of a concrete type
// are the receiver and then the words of the parameters, and the operands of a
// call to a method of an interface are the receiver's data word and then the
// same. The receiver is therefore recovered from the operand list rather than
// from the signature: the declared list is either the parameters alone or one
// leading operand and then the parameters, and the span walk of
// abiResultSpans says which of the two consumes the operands exactly.
//
// Both consuming them exactly is an ambiguity, and an ambiguity is not an
// answer: placing the operands over the wrong one of the two would put an
// argument in a register the callee does not read. The walk of the operand
// list is taken instead, which is what this compiler did before the declared
// list was read at all.
//
// A receiver decomposition split is the shape neither candidate matches, since
// its declared type is then in no operand and in no list. The walk of the
// operand list is taken there too, and specs/030-abi.md names it as the bound
// that is left.
func abiCallArgTypes(v *Value, ops []*Value) ([]*ir.Type, []int, bool) {
	sig := v.Sig
	if sig == nil || sig.Kind != ir.FuncKind || sig.Params == nil {
		return nil, nil, false
	}
	params := make([]*ir.Type, 0, len(sig.Params)+1)
	for _, p := range sig.Params {
		if p == nil {
			return nil, nil, false
		}
		params = append(params, p)
	}
	var types []*ir.Type
	var spans []int
	found := 0
	for pre := 0; pre <= 1; pre++ {
		if pre > len(ops) {
			break
		}
		try := params
		if pre == 1 {
			if ops[0] == nil || ops[0].Type == nil {
				continue
			}
			try = append([]*ir.Type{ops[0].Type}, params...)
		}
		place := make([]ABIValue, len(try))
		for i, t := range try {
			if t == nil {
				return nil, nil, false
			}
			parts, _ := ABILeaves(t)
			place[i] = ABIValue{Type: t, Parts: parts}
		}
		sp, ok := abiResultSpans(place, ops)
		if !ok {
			continue
		}
		found++
		types, spans = try, sp
	}
	if found != 1 {
		return nil, nil, false
	}
	return types, spans, true
}

// loadParts returns one load per register of a whole aggregate operand, or nil
// when the operand cannot be split.
//
// The loads are scheduled at the boundary and read the memory that is live
// there, which is mem, and not the memory the load they replace read. The two
// differ as soon as this pass has already put a block move ahead of the same
// boundary: rewriteResults copies a result the registers cannot hold into the
// result area ahead of the return, and splitOperands copies an argument into
// the outgoing area ahead of the call, and either leaves a newer memory live
// where these loads go. A load placed there reading the older memory is two
// memory values live at one point, which is the InvMemChain of ssa/verify.go,
// and below the verifier it is a load the scheduler may move across a store.
//
// Reading the newer memory moves no read across a store the program wrote. The
// only memory this pass inserts at a boundary writes an argument-area slot,
// which is a slot the convention owns and no value of the function names.
//
// mem is nil for a boundary that carries no memory operand, which a return
// built by hand can be. There is then no chain to hold and the load's own
// memory is the only answer.
func (a *abiPass) loadParts(user, arg *Value, av *ABIValue, mem *Value) []*Value {
	base := a.addressOf(user, arg)
	if base == nil {
		return nil
	}
	if mem == nil {
		mem = arg.Args[1]
	}
	out := make([]*Value, 0, len(av.Parts))
	for i := range av.Parts {
		p := &av.Parts[i]
		addr := a.partAddr(user, user.Block, arg.Pos, base, p)
		out = append(out, a.mk(user, user.Block, arg.Pos, OpLoad, p.Type, addr, mem))
	}
	a.kill(arg)
	return out
}

// spillResults gives every call result that is wider than a register and that
// the call site writes nowhere a place in memory.
//
// It closes the third bound of specs/030-abi.md: the forwarding return
// "return g()" and the forwarding call "h(g())". The words come back in
// registers, or in the call's result area when the registers could not hold
// them, and the value they make up is handed straight to another boundary
// without ever being written down.
//
// Every other rewrite in this pass reaches a value at a boundary through the
// address of the place it lives in: rewriteResults and splitOperands copy out
// of that place, rewriteCallResults writes each word into it. A forwarded
// result lives in no place, which is why the pass had nothing to write into
// and nothing to read out of. This makes the place. The store into it is the
// destination rewriteCallResults needs, and the load out of it is the address
// the boundary below needs, so both halves become the rewrite that exists.
//
// The slot is an ordinary local and goes into Func.Frame. It holds a live
// value across a boundary, so the collector has to find it, and it finds a
// local through the locals bitmap of specs/027-liveness-and-stackmaps.md. That
// is what separates it from the argument-area slots ABIHome names, which hold
// a value only for the instant of a call and which the callee describes.
//
// It runs ahead of every other rewrite, so that the placement and the walks
// below see a graph in which no boundary reads a call result directly.
func (a *abiPass) spillResults() bool {
	changed := false
	for _, b := range a.f.Blocks {
		for _, v := range b.Values {
			if !v.Op.IsCall() {
				continue
			}
			if a.spillCallResults(v) {
				changed = true
			}
		}
	}
	return changed
}

// spillCallResults spills the results of one call.
//
// The placement is the walk callResults makes, over the callee's declared
// result list, because the two have to agree on which results are wide: one
// that gives a result a slot the other then leaves whole would write a value
// nobody reads.
//
// Only a result that reaches the call site whole needs a place. One
// decomposition already split is one value per machine word, and each of those
// is a value the machine can move or store: a word in a register is read out
// of it and a word of the result area is loaded out of it, and neither is a
// value this pass has to write down first.
func (a *abiPass) spillCallResults(call *Value) bool {
	sel, ok := a.resultReads(call)
	if !ok {
		return false
	}
	args, _, _, err := ABICallArgs(a.t, call)
	if err != nil {
		return false
	}
	place, _, err := ABIResults(ABITargetOf(a.t, call), ABIStackEnd(args), abiCallResultTypes(call))
	if err != nil {
		return false
	}
	spans, ok := abiResultSpans(place, sel)
	if !ok {
		return false
	}

	// mem is the memory the call left, threaded through the spills so that a
	// reader that takes two wide results of one call gets one chain.
	mem := call
	changed := false
	at := 0
	for i := range place {
		lo := at
		at += spans[i]
		if spans[i] != 1 {
			continue
		}
		v := sel[lo]
		if !a.wideResult(v, &place[i]) || a.resultStore(v) != nil {
			continue
		}
		reader := a.soleReader(v)
		if reader == nil || reader.Block != v.Block || a.memoryOf(reader) != mem {
			// Nothing reads the result, or it is read where this pass cannot
			// put the store: in another block, or behind a memory this call
			// did not leave.
			//
			// The memory is the condition that matters and it is not a
			// convenience. A result the registers could not hold sits in the
			// call's outgoing argument area, and the next call this function
			// makes writes over that area, so a copy out of it that is not
			// ahead of every other call reads what the later call left.
			// Requiring the reader to read the memory the call produced puts
			// the store where the call ended.
			continue
		}
		mem = a.spillResult(v, reader, mem)
		changed = true
	}
	return changed
}

// wideResult reports whether a call result occupies more than one machine word
// and therefore needs a place in memory at the call site.
//
// A result of one word never needs one. It travels in one register, or it is
// one word of the area the code generator reads directly, and a store of it is
// a store the machine has.
func (a *abiPass) wideResult(sel *Value, av *ABIValue) bool {
	if !Multiword(sel.Type) {
		return false
	}
	if !av.InReg {
		return av.Type != nil && av.Type.Size > 0
	}
	return len(av.Parts) >= 2
}

// spillResult writes one call result into a frame slot and points its reader
// at a load out of that slot. It returns the memory the store produced.
func (a *abiPass) spillResult(sel, reader, mem *Value) *Value {
	o := &ir.Object{
		Name:  fmt.Sprintf("~f%d", a.spills),
		Type:  sel.Type,
		Class: ir.ClassLocal,
	}
	a.spills++
	a.f.Frame = append(a.f.Frame, o)

	b := reader.Block
	dst := a.mk(reader, b, sel.Pos, OpLocalAddr, abiPtrTo(sel.Type), mem)
	dst.Aux = o
	st := a.mk(reader, b, sel.Pos, OpStore, MemType, dst, sel, mem)
	st.AuxInt = sel.Type.Size
	src := a.mk(reader, b, sel.Pos, OpLocalAddr, abiPtrTo(sel.Type), st)
	src.Aux = o
	ld := a.mk(reader, b, sel.Pos, OpLoad, sel.Type, src, st)

	for i, arg := range reader.Args {
		if arg == sel {
			reader.SetArg(i, ld)
		}
	}
	reader.SetArg(len(reader.Args)-1, st)
	return st
}

// unread reports that nothing live reads v.
//
// A call result nobody reads is the discarded result of "_, err := g()". The
// words still come back where the convention says, so it still occupies them,
// but it needs no place to be written to.
func (a *abiPass) unread(v *Value) bool {
	if int(v.ID) >= len(a.users) {
		return false
	}
	for _, u := range a.users[v.ID] {
		if !a.isDead(u) {
			return false
		}
	}
	return true
}

// soleReader returns the one live value that reads v, or nil when v has none
// or has more than one.
func (a *abiPass) soleReader(v *Value) *Value {
	if int(v.ID) >= len(a.users) {
		return nil
	}
	var out *Value
	for _, u := range a.users[v.ID] {
		if a.isDead(u) {
			continue
		}
		if out != nil {
			return nil
		}
		out = u
	}
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

// callResults splits and renumbers the results of one call, and records where
// each value the call site reads comes back from.
//
// The placement is over the callee's declared result list, which
// abiCallResultTypes reads off the signature the call carries, and not over
// the values the call site reads. The two part above sixteen result words for
// the reason declaredResults gives, and the callee places over the declared
// list, so a call site that placed over the words would read half of a result
// out of a register the callee never wrote.
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
	args, _, size, err := ABICallArgs(a.t, call)
	if err != nil {
		return a.unreadableResult(call, "the convention does not place the call's arguments")
	}
	place, _, err := ABIResults(ABITargetOf(a.t, call), ABIStackEnd(args), abiCallResultTypes(call))
	if err != nil {
		return a.unreadableResult(call, "the convention does not place the call's results")
	}
	spans, ok := abiResultSpans(place, sel)
	if !ok {
		return a.unreadableResult(call, "the values the call site reads are not the words of the results the callee returns")
	}

	// The survey runs first, because a result the walk below cannot place has
	// to stop the function rather than renumber half of a list.
	//
	// Only a result that reaches the call site whole can fail it. One
	// decomposition already split is one value per machine word, and every
	// word is a value the machine moves or loads.
	at := 0
	for i := range place {
		av := &place[i]
		lo := at
		at += spans[i]
		if av.Type == nil || av.Type.Size == 0 || len(av.Parts) == 0 {
			continue
		}
		if spans[i] != 1 || !Multiword(sel[lo].Type) {
			continue
		}
		v := sel[lo]
		if a.unread(v) {
			// Nothing reads it, so it needs no place at all: the words come
			// back where the convention says and are dropped.
			continue
		}
		switch {
		case !av.InReg:
			// The registers could not hold it, so the callee wrote it into
			// the area and the call site reads it back from there.
			if a.resultStore(v) == nil {
				return a.resultRefusal(v, "the call site does not read it into one place, so the words it arrived in have nowhere to go")
			}
		case len(av.Parts) >= 2:
			if a.resultStore(v) == nil {
				return a.resultRefusal(v, "the call site does not write it to one place, so its words have nowhere to go")
			}
		}
	}

	mem := a.memoryOf(call)
	res := make([]ABIValue, 0, len(sel))
	word := int64(0)
	homes := 0
	at = 0
	for i := range place {
		av := &place[i]
		n := spans[i]
		vs := sel[at : at+n]
		at += n
		whole := n == 1 && Multiword(vs[0].Type)

		switch {
		case av.Type == nil || av.Type.Size == 0 || len(av.Parts) == 0:
			for _, v := range vs {
				v.AuxInt = word
				word++
				res = append(res, *av)
			}

		case !av.InReg && whole:
			// A result the registers could not hold and that the call site
			// reads as one value. The store it already makes becomes a block
			// move out of the area; one that nothing reads is left where it
			// is and occupies its word of the list.
			if st := a.resultStore(vs[0]); st != nil {
				a.readFromArea(call, vs[0], st, av, mem, homes)
				homes++
			}
			vs[0].AuxInt = word
			word++
			res = append(res, *av)

		case !av.InReg:
			// The callee wrote the result into the area and decomposition
			// already split the read of it, so each word is loaded out of the
			// word of the area that holds it.
			a.readWordsFromArea(call, vs, av, homes)
			homes++
			for k := range av.Parts {
				vs[k].AuxInt = word
				word++
				res = append(res, abiWordValue(av, k))
			}

		case whole && len(av.Parts) >= 2:
			if st := a.resultStore(vs[0]); st != nil {
				a.splitResult(vs[0], st, av, word)
			} else {
				a.dropResult(vs[0], av, word)
			}
			for k := range av.Parts {
				word++
				res = append(res, abiWordValue(av, k))
			}

		case n == len(av.Parts):
			for k := range av.Parts {
				vs[k].AuxInt = word
				word++
				res = append(res, abiWordValue(av, k))
			}

		default:
			for _, v := range vs {
				v.AuxInt = word
				word++
				res = append(res, *av)
			}
		}
	}
	if int(call.ID) < len(a.f.ABI.Calls) {
		rec := a.f.ABI.Calls[call.ID]
		if rec == nil {
			rec = &ABICall{Vals: args, Size: size}
			a.f.ABI.Calls[call.ID] = rec
		}
		rec.Results = res
	}
	return nil
}

// readWordsFromArea replaces the words of a result the registers could not
// hold by loads out of the outgoing argument area.
//
// It is readFromArea for a result decomposition already split. There is no
// store to turn into a block move, because there is no whole value at the call
// site: each word is a value of its own and each is read from the word of the
// area the callee wrote it to.
//
// The loads sit directly behind the call, for the reason spillResults puts its
// store there: the area they read is the call's outgoing argument area and the
// next call this function makes writes over it.
//
// The SelectN values stay, with no reader. Each names one place of the call's
// result list and the code generator walks that list by index, so one removed
// here is a place the generator cannot count past. They are given no home,
// because nothing reads them, and the generator then emits nothing for them.
func (a *abiPass) readWordsFromArea(call *Value, sel []*Value, av *ABIValue, n int) {
	if len(sel) != len(av.Parts) {
		return
	}
	anchor := sel[0]
	b := anchor.Block
	o := a.home("~R", n, av.Type, av.Off, false)
	base := a.mk(anchor, b, anchor.Pos, OpLocalAddr, abiPtrTo(av.Type), call)
	base.Aux = o
	for k := range av.Parts {
		p := &av.Parts[k]
		addr := a.partAddr(anchor, b, anchor.Pos, base, p)
		ld := a.mk(anchor, b, anchor.Pos, OpLoad, p.Type, addr, call)
		a.replaceValue(sel[k], ld)
	}
}

// dropResult replaces a call result nothing reads by one result per register.
//
// The words arrive whether or not the call site wants them, so the split is
// the same one splitResult makes and only the stores are absent. It is not an
// optimisation to leave the value whole instead: ssagen names a call's results
// by walking the SelectN values and indexing them by the word each reads, so a
// list with a fifteen-word hole in it is a list it refuses.
func (a *abiPass) dropResult(sel *Value, av *ABIValue, base int64) {
	call := sel.Args[0]
	for i := range av.Parts {
		p := &av.Parts[i]
		part := a.mk(sel, sel.Block, sel.Pos, OpSelectN, p.Type, call)
		part.AuxInt = base + int64(i)
	}
	a.kill(sel)
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

// readFromArea turns the call site's read of a result that arrived in the
// outgoing argument area into a block move out of that area.
//
// It is the mirror of rewriteResults, which is the callee's half: the callee
// copies the value into the area ahead of its return, and the caller copies it
// out of the area after the call. Go's convention puts such a result after the
// arguments the registers could not hold, and av.Off is that offset, because
// ABIWalk placed the two lists with one stack counter.
//
// The store the call site already makes becomes the move, rather than being
// removed and replaced. Its identifier and its position in the memory chain
// stay, so every value that read the memory it produced still reads the same
// value and no chain has to be repaired.
//
// The SelectN stays as well, with no reader. It names one place of the call's
// result list, and the code generator walks that list by index: a result
// removed here is a result the generator cannot count past. It is given no
// home, because nothing reads it, and the generator then emits nothing for it.
func (a *abiPass) readFromArea(call, sel, st *Value, av *ABIValue, mem *Value, n int) {
	if st == nil || mem == nil {
		return
	}
	o := a.home("~R", n, av.Type, av.Off, false)
	src := a.mk(st, st.Block, st.Pos, OpLocalAddr, abiPtrTo(av.Type), st.Args[2])
	src.Aux = o
	dst := st.Args[0]
	prev := st.Args[2]
	st.Op = OpMove
	st.Type = MemType
	st.AuxInt = av.Type.Size
	setArgs(st, dst, src, prev)
	a.touched[st.Block.ID] = true
}

// resultStore returns the store that writes a whole call result into one
// place, or nil when the result is read any other way.
//
// The match is the one selfStore makes for an incoming parameter: the result
// has exactly one reader, that reader is a store of the whole value, and the
// two are in one block so that the words are written where the result is read.
func (a *abiPass) resultStore(sel *Value) *Value {
	st := a.soleReader(sel)
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

// ABIPlacesOperands reports whether the convention, and not the instruction,
// decides where a value finds each of its operands.
//
// A call and a return are the cases, and they are the whole of ABIUseReg's
// domain: the predicate and the placement below cannot drift, because the
// placement asks the predicate first.
//
// The register allocator reads it for a second reason. An operand the
// convention puts in a register is named by ABIUseReg. An operand it puts in
// the outgoing argument area is written there by a store of its own, which
// materialises it into one register and holds it no longer than that store. So
// a call and a return read no operand out of a scratch register, and the
// number of operands they have, which no machine bounds, never becomes a
// scratch demand.
func ABIPlacesOperands(v *Value) bool {
	if v == nil {
		return false
	}
	return v.Op.IsCall() || v.Op == OpMakeResult || v.Op == OpARM64RET
}

// ABIUseReg returns the register the convention fixes for argument i of a
// value.
//
// A call's operands and a return's values are the cases: the callee reads them
// where the convention says, not where the allocator happened to put them.
// Naming the register here is what keeps a call with spilled operands from
// failing to allocate, and it is what lets the code generator move each
// operand straight into its home.
//
// False has two meanings and the allocator has to tell them apart, which is
// what ABIPlacesOperands is for: an operand of an ordinary instruction, which
// the allocator places itself, and an operand of a call or a return that the
// convention put in the argument area rather than a register.
func ABIUseReg(t *Target, v *Value, i int) (Reg, bool) {
	if t == nil || v == nil || i < 0 || i >= len(v.Args) {
		return NoReg, false
	}
	if !ABIPlacesOperands(v) {
		return NoReg, false
	}
	if r, ok := abiIndirectEntryReg(t, v, i); ok {
		return r, ok
	}
	var out []ABIValue
	var lo int
	var err error
	switch {
	case v.Op.IsCall():
		out, lo, _, err = ABICallArgs(t, v)
	default:
		out, err = ABIReturn(t, v)
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

// abiIndirectEntryReg returns the register the entry point of an indirect call
// is read from.
//
// The entry point is not an argument and the convention places it nowhere, so
// before this it kept whatever home the allocator gave it. That home may be an
// argument register of the same call, and the arguments are loaded before the
// branch, so the branch would jump to an argument. The code generator saw the
// collision and refused, which turned a program the allocator could have
// placed into a compiler error.
//
// The first scratch register of the class is the answer, and it is not a
// choice of convenience. Every register the convention uses for an argument or
// a result is excluded by construction, a scratch register is never any
// value's home, and the parallel move at the call site writes it in the same
// instant as the argument registers, so no source is destroyed. It is also
// what a scratch register is already for: an operand read into a register at
// the instruction that uses it.
func abiIndirectEntryReg(t *Target, v *Value, i int) (Reg, bool) {
	if i != 0 || (v.Op != OpARM64CALLclosure && v.Op != OpARM64CALLinter) {
		return NoReg, false
	}
	if len(t.Scratch[ClassInt]) == 0 {
		return NoReg, false
	}
	return t.Scratch[ClassInt][0], true
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
	// they are placed by the walk like any other. The results are walked with
	// them so that the size covers the whole area.
	out, _, size, err = ABIWalk(ABITargetOf(t, v), abiOperandTypes(v, lo), abiCallResultTypes(v), nil)
	return out, lo, size, err
}

// ABIReturn places the values one return passes, one entry per operand.
//
// It is the record rewriteResults left, for the reason ABICallArgs takes the
// record for a call: the placement is over the declared result list and the
// operand list is the machine words decomposition left in its place, so a walk
// of the operands is a different walk. The register allocator and the code
// generator both read this, so a return has one placement rather than two that
// have to agree.
//
// The walk is the fallback, for a return in a function assembled by hand and
// for one selection created after the assignment pass ran. Neither has a
// record and in both the operand list is the only list there is.
func ABIReturn(t *Target, v *Value) ([]ABIValue, error) {
	if v != nil && v.Block != nil && v.Block.Func != nil {
		if c := v.Block.Func.ABI.CallAt(v.ID); c != nil {
			return c.Vals, nil
		}
	}
	out, _, err := ABIResults(t, abiFuncArgStackEnd(v), abiOperandTypes(v, 0))
	return out, err
}

// abiCallResultTypes returns the types the call's results occupy the outgoing
// area as, in the order the area holds them, or nil when neither source has
// them.
//
// The callee's signature answers this and the call site does not. A result the
// registers cannot hold is written into the area by the callee whether or not
// the call site reads it, so a discarded result takes the same space as one
// that is assigned, and `f()` as a statement needs the same area as `x := f()`.
// nanogo sized the area from the reads, so a statement call to a function
// returning [3000]byte was given no area at all and the callee wrote three
// thousand bytes over its caller's frame. Go's own test/stack.go is where that
// surfaced, and the frame gc gives such a caller is 3024 bytes where nanogo
// gave it 16.
//
// The reads are the fallback, for a call built by hand rather than by
// ssa.Build: the passes below are tested on functions assembled value by
// value, and those carry no signature. The walk is over the call's own block,
// which is where the code generator looks for a result: one read anywhere else
// is one it cannot place.
func abiCallResultTypes(call *Value) []*ir.Type {
	if call == nil || call.Block == nil {
		return nil
	}
	if sig := call.Sig; sig != nil && sig.Kind == ir.FuncKind && sig.Results != nil {
		out := make([]*ir.Type, 0, len(sig.Results))
		for _, t := range sig.Results {
			if t == nil {
				return nil
			}
			out = append(out, t)
		}
		return out
	}
	var out []*ir.Type
	for _, v := range call.Block.Values {
		if v.Op != OpSelectN || len(v.Args) == 0 || v.Args[0] != call {
			continue
		}
		i := int(v.AuxInt)
		if i < 0 || i >= abiMaxParts {
			return nil
		}
		for len(out) <= i {
			out = append(out, nil)
		}
		if out[i] != nil {
			return nil
		}
		out[i] = v.Type
	}
	for _, t := range out {
		if t == nil {
			return nil
		}
	}
	return out
}

// abiFuncArgStackEnd returns where the stack part of a function's own incoming
// argument area ended, which is where its results continue from.
func abiFuncArgStackEnd(v *Value) int64 {
	if v == nil || v.Block == nil || v.Block.Func == nil || v.Block.Func.ABI == nil {
		return 0
	}
	return ABIStackEnd(v.Block.Func.ABI.In)
}
