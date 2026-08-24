// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

// The target-neutral operation set.
//
// This is the whole vocabulary of specs/021-ssa-construction.md. It is smaller
// than Go's expression grammar in two directions and larger in one.
//
// Smaller, because every Go construct of specs/020-ir.md's lowering table is
// gone by the time an operation from this set exists, and because a comparison
// is canonicalised: there is no Greater and no GreaterEqual, only Less and
// LessEqual with the arguments swapped. One rewrite rule then covers both
// spellings, which is the reason the reference implementation does the same.
//
// Larger, because memory is explicit. Loads, stores, calls and the checks all
// name the memory they act on, and the ones that write name the memory they
// produce.
//
// specs/025-lowering-and-rules.md replaces these with machine operations. No
// operation here mentions a register or an addressing mode.

// Op is a target-neutral operation.
type Op uint8

const (
	OpInvalid Op = iota

	// Inputs and constants.
	OpArg         // an incoming parameter. Aux is the *ir.Object
	OpConstBool   // AuxInt is 0 or 1
	OpConstInt    // AuxInt is the value
	OpConstFloat  // Aux is a float64
	OpConstString // Aux is a string
	OpConstNil    // a typed nil pointer, map, channel, or function

	// The phi of specs/021-ssa-construction.md. One argument per predecessor
	// slot, in predecessor order.
	OpPhi

	// OpCopy is the identity. Construction does not create one; a pass that
	// rewrites a value in place does, and copyelim removes them.
	OpCopy

	// Arithmetic. The width and the signedness come from the value's type, not
	// from the operation, which is what keeps this set small.
	OpAdd
	OpSub
	OpMul
	OpDiv
	OpMod
	OpAnd
	OpOr
	OpXor
	OpAndNot
	OpShl
	OpShr
	OpNeg
	OpCom // ^x
	OpNot // !x, boolean

	// Comparison. Always yields a bool.
	OpEq
	OpNeq
	OpLess
	OpLeq

	// Conversions between machine representations.
	OpSignExt
	OpZeroExt
	OpTrunc
	OpCvtIntToFloat
	OpCvtFloatToInt
	OpCvtFloatToFloat
	OpBitcast // a reinterpretation of the same bits, such as pointer to uintptr

	// Memory.
	OpInitMem // the memory a function starts with. Entry block only
	OpLoad    // Load(addr, mem)
	OpStore   // Store(addr, val, mem) -> mem. AuxInt is the size in bytes
	OpMove    // Move(dst, src, mem) -> mem. AuxInt is the size in bytes
	OpZero    // Zero(dst, mem) -> mem. AuxInt is the size in bytes

	// Addressing.
	OpAddr      // the address of a global. Aux is the *ir.Object
	OpLocalAddr // the address of a frame slot. Aux is the *ir.Object. Takes
	// memory, because a frame slot is written through memory and
	// an address computed before a store must not float past it
	OpOffPtr   // OffPtr(ptr) with AuxInt the byte offset
	OpPtrIndex // PtrIndex(ptr, idx) with AuxInt the element size

	// Calls. Every call takes memory and produces memory, which is what stops
	// a load from moving across one. Results are read with OpSelectN.
	OpStaticCall  // Aux is the *ir.Object of the callee
	OpClosureCall // ClosureCall(closure, args..., mem)
	OpInterCall   // InterCall(itab, args..., mem)
	OpSelectN     // SelectN(call) with AuxInt the result index

	// Checks. specs/021-ssa-construction.md inserts one at every index, slice
	// and dereference whose safety is not already established, and
	// specs/022-optimization-passes.md removes the ones it can prove
	// unnecessary. The asymmetry is deliberate: inserting all of them and
	// removing some is safe, inserting some is not.
	//
	// A check takes memory because it can panic, which is an observable
	// effect. specs/025-lowering-and-rules.md expands each into a branch to a
	// block that calls the runtime; that expansion is not done here, so the
	// graph stays small enough to read.
	OpNilCheck         // NilCheck(ptr, mem) -> ptr
	OpBoundsCheck      // BoundsCheck(idx, len, mem) -> mem
	OpSliceBoundsCheck // SliceBoundsCheck(idx, cap, mem) -> mem

	// OpMakeResult gathers the results of a return and the memory the function
	// leaves with. It is the control value of a BlockRet.
	OpMakeResult // MakeResult(results..., mem) -> mem

	opCount
)

// opInfo describes one operation.
//
// A table rather than a set of methods with switches, so that a new operation
// with a missing row is visible as a zero name rather than as a fallthrough to
// whatever the last case was. TestOpTable asserts every row is filled.
type opInfo struct {
	name string

	// argLen is the number of arguments, or -1 when the operation is variadic.
	argLen int8

	// takesMem reports that the last argument is a memory value.
	takesMem bool

	// makesMem reports that the value is a memory value.
	makesMem bool

	// commutative reports that the arguments may be exchanged. cse and the
	// rewrite rules of specs/022-optimization-passes.md use it.
	commutative bool

	// constant reports that the value depends on nothing.
	constant bool

	// call reports that the operation transfers control to another function.
	// specs/027-liveness-and-stackmaps.md needs a stack map at each one.
	call bool

	// hasAuxInt reports that AuxInt is meaningful, which is what a dump prints.
	hasAuxInt bool
}

var opInfos = [opCount]opInfo{
	OpInvalid: {name: "Invalid", argLen: 0},

	OpArg:         {name: "Arg", argLen: 0},
	OpConstBool:   {name: "ConstBool", argLen: 0, constant: true, hasAuxInt: true},
	OpConstInt:    {name: "ConstInt", argLen: 0, constant: true, hasAuxInt: true},
	OpConstFloat:  {name: "ConstFloat", argLen: 0, constant: true},
	OpConstString: {name: "ConstString", argLen: 0, constant: true},
	OpConstNil:    {name: "ConstNil", argLen: 0, constant: true},

	OpPhi:  {name: "Phi", argLen: -1},
	OpCopy: {name: "Copy", argLen: 1},

	OpAdd:    {name: "Add", argLen: 2, commutative: true},
	OpSub:    {name: "Sub", argLen: 2},
	OpMul:    {name: "Mul", argLen: 2, commutative: true},
	OpDiv:    {name: "Div", argLen: 2},
	OpMod:    {name: "Mod", argLen: 2},
	OpAnd:    {name: "And", argLen: 2, commutative: true},
	OpOr:     {name: "Or", argLen: 2, commutative: true},
	OpXor:    {name: "Xor", argLen: 2, commutative: true},
	OpAndNot: {name: "AndNot", argLen: 2},
	OpShl:    {name: "Shl", argLen: 2},
	OpShr:    {name: "Shr", argLen: 2},
	OpNeg:    {name: "Neg", argLen: 1},
	OpCom:    {name: "Com", argLen: 1},
	OpNot:    {name: "Not", argLen: 1},

	OpEq:   {name: "Eq", argLen: 2, commutative: true},
	OpNeq:  {name: "Neq", argLen: 2, commutative: true},
	OpLess: {name: "Less", argLen: 2},
	OpLeq:  {name: "Leq", argLen: 2},

	OpSignExt:         {name: "SignExt", argLen: 1},
	OpZeroExt:         {name: "ZeroExt", argLen: 1},
	OpTrunc:           {name: "Trunc", argLen: 1},
	OpCvtIntToFloat:   {name: "CvtIntToFloat", argLen: 1},
	OpCvtFloatToInt:   {name: "CvtFloatToInt", argLen: 1},
	OpCvtFloatToFloat: {name: "CvtFloatToFloat", argLen: 1},
	OpBitcast:         {name: "Bitcast", argLen: 1},

	OpInitMem: {name: "InitMem", argLen: 0, makesMem: true},
	OpLoad:    {name: "Load", argLen: 2, takesMem: true},
	OpStore:   {name: "Store", argLen: 3, takesMem: true, makesMem: true, hasAuxInt: true},
	OpMove:    {name: "Move", argLen: 3, takesMem: true, makesMem: true, hasAuxInt: true},
	OpZero:    {name: "Zero", argLen: 2, takesMem: true, makesMem: true, hasAuxInt: true},

	OpAddr:      {name: "Addr", argLen: 0},
	OpLocalAddr: {name: "LocalAddr", argLen: 1, takesMem: true},
	OpOffPtr:    {name: "OffPtr", argLen: 1, hasAuxInt: true},
	OpPtrIndex:  {name: "PtrIndex", argLen: 2, hasAuxInt: true},

	OpStaticCall:  {name: "StaticCall", argLen: -1, takesMem: true, makesMem: true, call: true},
	OpClosureCall: {name: "ClosureCall", argLen: -1, takesMem: true, makesMem: true, call: true},
	OpInterCall:   {name: "InterCall", argLen: -1, takesMem: true, makesMem: true, call: true},
	OpSelectN:     {name: "SelectN", argLen: 1, takesMem: true, hasAuxInt: true},

	OpNilCheck:         {name: "NilCheck", argLen: 2, takesMem: true},
	OpBoundsCheck:      {name: "BoundsCheck", argLen: 3, takesMem: true, makesMem: true},
	OpSliceBoundsCheck: {name: "SliceBoundsCheck", argLen: 3, takesMem: true, makesMem: true},

	OpMakeResult: {name: "MakeResult", argLen: -1, takesMem: true, makesMem: true},
}

// extraOpInfos holds the rows of the operations above the target-neutral set:
// the pseudo-operations lowering introduces and one target's machine
// operations. specs/025-lowering-and-rules.md puts both below this file, so
// the rows are registered from there rather than written here. It is indexed
// by Op - opCount.
var extraOpInfos []opInfo

// registerOpInfos records the rows of a run of operations that starts at base.
//
// base is an explicit constant rather than the current length, so two files
// that both register cannot depend on which of them ran first
// (specs/053-determinism.md).
func registerOpInfos(base Op, rows []opInfo) {
	i := int(base) - int(opCount)
	if i < 0 {
		panic("ssa: registerOpInfos below the target-neutral set")
	}
	for len(extraOpInfos) < i+len(rows) {
		extraOpInfos = append(extraOpInfos, opInfo{})
	}
	copy(extraOpInfos[i:], rows)
}

func (o Op) String() string {
	if info := infoOf(o); info.name != "" {
		return info.name
	}
	return "op(?)"
}

// infoOf returns the table row of o, or an empty row when o is not in the
// table. An operation outside the table is a bug in a pass, and the verifier
// must be able to report it rather than fault on it.
func infoOf(o Op) opInfo {
	if int(o) < len(opInfos) {
		return opInfos[o]
	}
	if i := int(o) - int(opCount); i < len(extraOpInfos) {
		return extraOpInfos[i]
	}
	return opInfo{}
}

// TakesMemory reports whether the last argument of o is a memory value.
func (o Op) TakesMemory() bool { return infoOf(o).takesMem }

// MakesMemory reports whether a value with this operation is a memory value.
func (o Op) MakesMemory() bool { return infoOf(o).makesMem }

// IsCommutative reports whether the two arguments of o may be exchanged.
func (o Op) IsCommutative() bool { return infoOf(o).commutative }

// IsConstant reports whether a value with this operation depends on nothing.
func (o Op) IsConstant() bool { return infoOf(o).constant }

// IsCall reports whether o transfers control to another function.
func (o Op) IsCall() bool { return infoOf(o).call }

// ArgLen returns the number of arguments o takes, or -1 when it is variadic.
func (o Op) ArgLen() int { return int(infoOf(o).argLen) }
