// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package rules holds the lowering rules of each target.
//
// specs/025-lowering-and-rules.md puts the rule file per target and the rule
// engine in one place. This is the file side of that split: the engine is
// ssa.Lower and it knows nothing about arm64, and this package knows nothing
// about how a rule is applied.
//
// A rule is a Go function, one per operation, that matches on the shape of the
// arguments. There is no DSL, which specs/025 asks for and gives the reason
// for. The cost is that a rule is five lines here where the reference
// implementation writes one line of a pattern language, and the benefit is
// that there is no generator to debug and no build step.
package rules

import (
	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj/arm64"
	"golang.design/x/nanogo/rtsym"
	"golang.design/x/nanogo/ssa"
)

// The arm64 rule set, groups 1 to 5 of specs/042-arm64-backend.md.
//
// What each group buys, in the order specs/042 says they are worth writing:
//
//  1. Arithmetic, comparison and the conditional forms. Every immediate form
//     saves the instruction that would have materialised the constant, and
//     every range comes from the encoder rather than from a number written
//     here, which is what specs/041 requires.
//  2. Loads and stores of every width, signed and unsigned. The signedness is
//     in the load, so a sign extension after a narrow load is never emitted.
//  3. Address computation. This is where the difference between naive and
//     reasonable code generation lives: a constant offset folded into the load
//     removes an ADD, and a scaled index folded into it removes an ADD and a
//     shift, in one rule each rather than a special case in a selector.
//  4. Branches and the condition-code forms. A comparison that only feeds a
//     branch becomes a compare and a conditional branch with no register in
//     between, and a comparison against zero becomes a compare-and-branch with
//     no compare at all.
//  5. Calls. The four shapes differ only in how the destination is computed,
//     so they differ by one rule each.
//
// Floating point, atomics and the inline memmove forms are groups 6 to 8 and
// are not here. Deferred lists what that leaves unlowered and why.

// ARM64 is the arm64 rule set.
var ARM64 = newARM64()

// Types the rules need for values that no Go type describes.
//
// A mask computed to make a shift count safe is a machine word and nothing
// else. Naming the types here keeps ir.Layout out of the rules.
var (
	typeI64  = &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int64"}
	typeU64  = &ir.Type{Kind: ir.Uint64, Size: 8, Align: 8, Name: "uint64"}
	typeFunc = &ir.Type{Kind: ir.FuncKind, Size: 8, Align: 8, Name: "func()"}
)

func newARM64() *ssa.RuleSet {
	v := make([]ssa.ValueRule, ssa.ARM64NumOps())

	// Group 1: arithmetic, comparison and the conditional forms.
	v[ssa.OpConstInt] = lowerConst
	v[ssa.OpConstBool] = lowerConst
	v[ssa.OpConstNil] = lowerConst
	v[ssa.OpAdd] = lowerAdd
	v[ssa.OpSub] = lowerSub
	v[ssa.OpMul] = lowerMul
	v[ssa.OpDiv] = lowerDiv
	v[ssa.OpMod] = lowerMod
	v[ssa.OpAnd] = lowerLogical
	v[ssa.OpOr] = lowerLogical
	v[ssa.OpXor] = lowerLogical
	v[ssa.OpAndNot] = lowerLogical
	v[ssa.OpNeg] = lowerNeg
	v[ssa.OpCom] = lowerCom
	v[ssa.OpNot] = lowerNot
	v[ssa.OpShl] = lowerShift
	v[ssa.OpShr] = lowerShift
	v[ssa.OpEq] = lowerCompare
	v[ssa.OpNeq] = lowerCompare
	v[ssa.OpLess] = lowerCompare
	v[ssa.OpLeq] = lowerCompare
	v[ssa.OpSignExt] = lowerExt
	v[ssa.OpZeroExt] = lowerExt
	v[ssa.OpTrunc] = lowerExt
	v[ssa.OpBitcast] = lowerBitcast

	// Group 2: loads and stores.
	v[ssa.OpLoad] = lowerLoad
	v[ssa.OpStore] = lowerStore
	v[ssa.OpMove] = lowerMove
	v[ssa.OpZero] = lowerZero

	// Group 3: address computation, and the folds that make it worth having.
	v[ssa.OpAddr] = lowerAddr
	v[ssa.OpLocalAddr] = lowerLocalAddr
	v[ssa.OpOffPtr] = lowerOffPtr
	v[ssa.OpPtrIndex] = lowerPtrIndex
	v[ssa.OpARM64ADDconst] = foldAddconst
	for _, op := range loadStoreOps {
		v[op] = foldAddress
	}

	// Group 5: calls, and the checks that become calls.
	v[ssa.OpStaticCall] = lowerStaticCall
	v[ssa.OpClosureCall] = lowerClosureCall
	v[ssa.OpInterCall] = lowerInterCall
	v[ssa.OpMakeResult] = lowerMakeResult
	v[ssa.OpNilCheck] = lowerNilCheck
	v[ssa.OpBoundsCheck] = lowerBoundsCheck
	v[ssa.OpSliceBoundsCheck] = lowerBoundsCheck

	return &ssa.RuleSet{
		Name:      "arm64",
		Value:     v,
		Block:     lowerBlock,
		Machine:   ssa.IsARM64Op,
		Essential: arm64Essential,
	}
}

// loadStoreOps are the operations the address folds apply to.
var loadStoreOps = []ssa.Op{
	ssa.OpARM64MOVDload, ssa.OpARM64MOVWload, ssa.OpARM64MOVWUload,
	ssa.OpARM64MOVHload, ssa.OpARM64MOVHUload, ssa.OpARM64MOVBload,
	ssa.OpARM64MOVBUload,
	ssa.OpARM64MOVDstore, ssa.OpARM64MOVWstore, ssa.OpARM64MOVHstore,
	ssa.OpARM64MOVBstore,
}

// arm64Essential reports the operations that must survive with no user.
//
// The nil check is the only one. It exists to fault, its result is the pointer
// it checked, and lowering redirects that result to the pointer itself, so the
// check has no user at all by the time dead values are removed.
func arm64Essential(op ssa.Op) bool { return op == ssa.OpARM64LoweredNilCheck }

// Deferred names a target-neutral operation that has no arm64 rule, with the
// reason. The coverage report of specs/025 prints it, and an operation that is
// in neither the rule table nor this list fails the test.
//
// The two reasons are different in kind and the report keeps them apart.
// Floating point is deferred by specs/042 itself, which puts it in group 6 and
// marks the F registers deferred because groups 1 to 5 need none of them.
// obj/arm64 draws the same line and has no floating-point encoder.
//
// The multi-word operations are not deferred by any spec. They need a value
// that does not fit a register to be split into the registers that hold it,
// and ssa/build.go's classify comment assigns that work to
// specs/025-lowering-and-rules.md, which never mentions it. It cannot be
// worked around here: a 16-byte Store cannot become a memmove, because memmove
// needs a source address and Store has a source value.
var Deferred = []struct {
	Op     ssa.Op
	Reason string
}{
	{ssa.OpConstFloat, "floating point is specs/042 group 6, and obj/arm64 has no encoder for it"},
	{ssa.OpCvtIntToFloat, "floating point is specs/042 group 6"},
	{ssa.OpCvtFloatToInt, "floating point is specs/042 group 6"},
	{ssa.OpCvtFloatToFloat, "floating point is specs/042 group 6"},
	{ssa.OpConstString, "a string constant is two words and needs value decomposition, which no spec owns"},
}

// ---------------------------------------------------------------------------
// Shape helpers
//
// Every one of them asks the encoder rather than restating a range.

// width returns the operand width a value is computed in.
func width(v *ssa.Value) arm64.Size { return ssa.ARM64Size(v.Type) }

// argConst returns the constant argument i holds.
//
// It accepts both spellings, because the engine walks a block forwards and a
// constant is usually already a machine operation by the time its user is
// rewritten, but a constant defined in another block may not be.
func argConst(v *ssa.Value, i int) (int64, bool) {
	if i >= len(v.Args) {
		return 0, false
	}
	return valConst(v.Args[i])
}

// valConst returns the constant a holds.
func valConst(a *ssa.Value) (int64, bool) {
	switch a.Op {
	case ssa.OpConstInt, ssa.OpConstBool, ssa.OpARM64MOVDconst:
		return a.AuxInt, true
	case ssa.OpConstNil:
		return 0, true
	}
	return 0, false
}

// isFloat reports whether the rules must refuse this value.
//
// A rule that lowered a floating-point compare to an integer one would produce
// code that runs and computes the wrong answer, which is the class of bug
// specs/025 says a silent fallback creates. Refusing leaves the operation for
// the engine to crash on, with the operation named.
func isFloat(t *ir.Type) bool { return t != nil && (t.Kind.IsFloat() || t.Kind.IsComplex()) }

func signed(t *ir.Type) bool { return t != nil && t.Kind.IsSigned() }

// constInto materialises a constant in a register.
//
// Every 64-bit value is reachable: obj/arm64's MovConst writes at most four
// instructions and MovLogicalImm reaches the repeating patterns in one. arm64
// therefore needs no constant pool for an integer, which is the target choice
// specs/025's constants section leaves open.
func constInto(v *ssa.Value, e *ssa.Edit, val int64, t *ir.Type) *ssa.Value {
	c := e.Insert(v.Pos, ssa.OpARM64MOVDconst, t)
	c.AuxInt = val
	return c
}

// ---------------------------------------------------------------------------
// Group 1: integer arithmetic, comparison and the conditional forms

func lowerConst(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	c := v.AuxInt
	e.Set(v, ssa.OpARM64MOVDconst)
	v.AuxInt = c
	return true
}

// lowerAdd covers ADD, ADDconst and SUBconst.
//
// The subtract form is not a micro-optimisation. The add immediate field is
// unsigned, so x + (-8) has no add form at all and would otherwise materialise
// a constant for every negative offset a program computes.
func lowerAdd(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	sz := width(v)
	for i := 0; i < 2; i++ {
		c, ok := argConst(v, i)
		if !ok {
			continue
		}
		x := v.Args[1-i]
		switch {
		case ssa.ARM64ImmFits(ssa.OpARM64ADDconst, sz, c):
			e.Set(v, ssa.OpARM64ADDconst, x)
			v.AuxInt = c
			return true
		case ssa.ARM64ImmFits(ssa.OpARM64SUBconst, sz, -c):
			e.Set(v, ssa.OpARM64SUBconst, x)
			v.AuxInt = -c
			return true
		}
	}
	e.Set(v, ssa.OpARM64ADD, v.Args[0], v.Args[1])
	return true
}

func lowerSub(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	sz := width(v)
	if c, ok := argConst(v, 1); ok {
		switch {
		case ssa.ARM64ImmFits(ssa.OpARM64SUBconst, sz, c):
			x := v.Args[0]
			e.Set(v, ssa.OpARM64SUBconst, x)
			v.AuxInt = c
			return true
		case ssa.ARM64ImmFits(ssa.OpARM64ADDconst, sz, -c):
			x := v.Args[0]
			e.Set(v, ssa.OpARM64ADDconst, x)
			v.AuxInt = -c
			return true
		}
	}
	if c, ok := argConst(v, 0); ok && c == 0 {
		e.Set(v, ssa.OpARM64NEG, v.Args[1])
		return true
	}
	e.Set(v, ssa.OpARM64SUB, v.Args[0], v.Args[1])
	return true
}

// lowerMul turns a multiply by a power of two into a shift.
//
// The multiplier is three cycles and the shift is one, and a multiply by a
// constant is what an index into an array of a power-of-two element size
// becomes before the address rules see it.
func lowerMul(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	sz := width(v)
	for i := 0; i < 2; i++ {
		c, ok := argConst(v, i)
		if !ok || c <= 0 || c&(c-1) != 0 {
			continue
		}
		sh := log2(c)
		if !ssa.ARM64ImmFits(ssa.OpARM64LSLconst, sz, sh) {
			continue
		}
		x := v.Args[1-i]
		e.Set(v, ssa.OpARM64LSLconst, x)
		v.AuxInt = sh
		return true
	}
	e.Set(v, ssa.OpARM64MUL, v.Args[0], v.Args[1])
	return true
}

// lowerDiv emits the divide and the check in front of it.
//
// arm64 division by zero produces zero rather than a trap, which obj/arm64's
// SdivRegReg says in as many words, so the check is the rule's work. A divisor
// that is a non-zero constant needs none.
func lowerDiv(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	guardDivide(v, e)
	op := ssa.OpARM64UDIV
	if signed(v.Type) {
		op = ssa.OpARM64SDIV
	}
	e.Set(v, op, v.Args[0], v.Args[1])
	return true
}

// lowerMod computes the remainder as x - (x/y)*y, which is one multiply and
// subtract instruction after the divide. arm64 has no remainder instruction.
func lowerMod(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	guardDivide(v, e)
	op := ssa.OpARM64UDIV
	if signed(v.Type) {
		op = ssa.OpARM64SDIV
	}
	x, y := v.Args[0], v.Args[1]
	q := e.InsertBefore(v, op, v.Type, x, y)
	e.Set(v, ssa.OpARM64MSUB, q, y, x)
	return true
}

func divisorIsSafe(v *ssa.Value) bool {
	c, ok := argConst(v, 1)
	return ok && c != 0
}

// guardDivide branches to a block that calls runtime.panicdivide.
//
// The divide moves into the continuation block, so the caller lowers it there
// and every value it needs goes with it. A divisor that is a non-zero constant
// needs no guard at all.
func guardDivide(v *ssa.Value, e *ssa.Edit) {
	if divisorIsSafe(v) {
		return
	}
	mem := e.Mem()
	if mem == nil {
		// Nothing in the block has named memory yet, so there is no memory
		// value for the call to take. Leave the guard out rather than invent
		// one: a call with the wrong memory argument breaks the chain that
		// orders every side effect in the function.
		return
	}
	y := v.Args[1]
	cmp := e.Insert(v.Pos, cmpConstOp(y), ssa.FlagsType, y)
	cmp.AuxInt = 0
	br := e.Insert(v.Pos, ssa.OpARM64BRcond, ssa.FlagsType, cmp)
	br.Aux = arm64.NE
	fail, _ := e.Guard(v, br)
	panicCall(fail, v, mem, "runtime.panicdivide")
}

func cmpConstOp(x *ssa.Value) ssa.Op {
	if width(x) == arm64.Size64 {
		return ssa.OpARM64CMPconst
	}
	return ssa.OpARM64CMPWconst
}

func lowerLogical(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	var reg, imm ssa.Op
	switch v.Op {
	case ssa.OpAnd:
		reg, imm = ssa.OpARM64AND, ssa.OpARM64ANDconst
	case ssa.OpOr:
		reg, imm = ssa.OpARM64ORR, ssa.OpARM64ORRconst
	case ssa.OpXor:
		reg, imm = ssa.OpARM64EOR, ssa.OpARM64EORconst
	default:
		reg, imm = ssa.OpARM64BIC, ssa.OpARM64BICconst
	}
	sz := width(v)
	// AndNot is not commutative: only its second argument may become the
	// immediate, and the immediate form is AND of the complement.
	first := 1
	if v.Op != ssa.OpAndNot {
		first = 0
	}
	for i := first; i < 2; i++ {
		if c, ok := argConst(v, i); ok && ssa.ARM64ImmFits(imm, sz, c) {
			x := v.Args[1-i]
			e.Set(v, imm, x)
			v.AuxInt = c
			return true
		}
	}
	e.Set(v, reg, v.Args[0], v.Args[1])
	return true
}

func lowerNeg(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	e.Set(v, ssa.OpARM64NEG, v.Args[0])
	return true
}

func lowerCom(v *ssa.Value, e *ssa.Edit) bool {
	e.Set(v, ssa.OpARM64MVN, v.Args[0])
	return true
}

// lowerNot is the boolean negation, which is an exclusive or with one. A
// boolean holds 0 or 1 and nothing else, so no comparison is needed.
func lowerNot(v *ssa.Value, e *ssa.Edit) bool {
	x := v.Args[0]
	e.Set(v, ssa.OpARM64EORconst, x)
	v.AuxInt = 1
	return true
}

// lowerShift is where arm64 and Go disagree and the rule has to pay for it.
//
// Go defines a shift by a count at least the operand width as zero, and as the
// sign for a signed right shift. The architecture takes the count modulo the
// width, so LSLV alone computes x<<1 for a count of 65. The conditional select
// that would fix it in one instruction has no encoder in obj/arm64, so the
// mask is computed arithmetically:
//
//	u = s >> log2(width)     zero exactly when the count is in range
//	m = -u >> 63             all ones when the count is out of range
//
// which is correct for every unsigned count, including one above 2**63 where a
// signed comparison against the width is not. The left and unsigned right
// shifts then clear the result with BIC, and the arithmetic right shift ors
// the mask into the count instead, because -1 modulo the width is the largest
// in-range count and that is the answer Go wants.
func lowerShift(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	x, s := v.Args[0], v.Args[1]
	sz := width(v)
	bits := int64(32)
	if sz == arm64.Size64 {
		bits = 64
	}
	arith := v.Op == ssa.OpShr && signed(v.Type)

	if c, ok := argConst(v, 1); ok {
		switch {
		case c < 0:
			// A negative count is a run-time panic in Go, inserted before this
			// pass. Nothing here can encode one, so it is left unlowered.
			return false
		case c < bits:
			e.Set(v, constShiftOp(v.Op, arith), x)
			v.AuxInt = c
			return true
		case arith:
			e.Set(v, ssa.OpARM64ASRconst, x)
			v.AuxInt = bits - 1
			return true
		default:
			e.Set(v, ssa.OpARM64MOVDconst)
			v.AuxInt = 0
			return true
		}
	}

	u := e.Insert(v.Pos, ssa.OpARM64LSRconst, typeU64, s)
	u.AuxInt = log2(bits)
	n := e.Insert(v.Pos, ssa.OpARM64NEG, typeI64, u)
	m := e.Insert(v.Pos, ssa.OpARM64ASRconst, typeI64, n)
	m.AuxInt = 63

	if arith {
		count := e.Insert(v.Pos, ssa.OpARM64ORR, typeI64, s, m)
		e.Set(v, ssa.OpARM64ASR, x, count)
		return true
	}
	op := ssa.OpARM64LSL
	if v.Op == ssa.OpShr {
		op = ssa.OpARM64LSR
	}
	sh := e.Insert(v.Pos, op, v.Type, x, s)
	e.Set(v, ssa.OpARM64BIC, sh, m)
	return true
}

func constShiftOp(op ssa.Op, arith bool) ssa.Op {
	if op == ssa.OpShl {
		return ssa.OpARM64LSLconst
	}
	if arith {
		return ssa.OpARM64ASRconst
	}
	return ssa.OpARM64LSRconst
}

func log2(n int64) int64 {
	k := int64(0)
	for n > 1 {
		n >>= 1
		k++
	}
	return k
}

// lowerCompare produces a compare and a conditional set.
//
// The comparison width and the signedness come from the arguments and not from
// the result, which is always a bool. Getting that backwards produces a
// comparison of the wrong width that is right for half of every value range.
func lowerCompare(v *ssa.Value, e *ssa.Edit) bool {
	x, y := v.Args[0], v.Args[1]
	if isFloat(x.Type) {
		return false
	}
	cond := condOf(v.Op, signed(x.Type))
	flags, ok := compareFlags(v, e, x, y, ssa.ARM64Size(x.Type))
	if !ok {
		return false
	}
	e.Set(v, ssa.OpARM64CSET, flags)
	v.Aux = cond
	return true
}

// compareFlags emits the compare at the width sz and returns the flags value.
//
// The width is the caller's because the two callers know different things. A
// comparison takes it from the arguments, which share a type. A bounds check
// cannot: specs/021 builds the length as an int and the index as whatever the
// Go expression's type is, so the two need not be the same width, and the
// check is made at 64 bits. That is safe in both directions, because a narrow
// index reaches the register either sign-extended or with a zeroed upper half,
// and both spellings of a negative index are far above any length as an
// unsigned number.
func compareFlags(v *ssa.Value, e *ssa.Edit, x, y *ssa.Value, sz arm64.Size) (*ssa.Value, bool) {
	is64 := sz == arm64.Size64
	if c, ok := valConst(y); ok {
		cmp, cmn := ssa.OpARM64CMPWconst, ssa.OpARM64CMNWconst
		if is64 {
			cmp, cmn = ssa.OpARM64CMPconst, ssa.OpARM64CMNconst
		}
		switch {
		case ssa.ARM64ImmFits(cmp, sz, c):
			f := e.Insert(v.Pos, cmp, ssa.FlagsType, x)
			f.AuxInt = c
			return f, true
		case ssa.ARM64ImmFits(cmn, sz, -c):
			f := e.Insert(v.Pos, cmn, ssa.FlagsType, x)
			f.AuxInt = -c
			return f, true
		}
	}
	op := ssa.OpARM64CMPW
	if is64 {
		op = ssa.OpARM64CMP
	}
	return e.Insert(v.Pos, op, ssa.FlagsType, x, y), true
}

// condOf is the canonicalisation of specs/021: there is no Greater and no
// GreaterEqual, so four operations and a signedness cover the six spellings.
func condOf(op ssa.Op, sign bool) arm64.Cond {
	switch op {
	case ssa.OpEq:
		return arm64.EQ
	case ssa.OpNeq:
		return arm64.NE
	case ssa.OpLess:
		if sign {
			return arm64.LT
		}
		return arm64.LO
	default:
		if sign {
			return arm64.LE
		}
		return arm64.LS
	}
}

// lowerExt changes the width of a value.
//
// The destination decides, not the source: a truncation to a narrow type must
// leave the register holding exactly the value that type can hold, because the
// next comparison reads the whole register.
func lowerExt(v *ssa.Value, e *ssa.Edit) bool {
	from, to := v.Args[0].Type, v.Type
	if isFloat(from) || isFloat(to) {
		return false
	}
	if to == nil || from == nil {
		return false
	}
	sign := signed(to)
	if v.Op == ssa.OpSignExt {
		sign = true
	} else if v.Op == ssa.OpZeroExt {
		sign = false
	}
	narrow := to.Size
	if from.Size < narrow {
		narrow = from.Size
	}
	var op ssa.Op
	switch {
	case narrow == 1 && sign:
		op = ssa.OpARM64SXTB
	case narrow == 1:
		op = ssa.OpARM64UXTB
	case narrow == 2 && sign:
		op = ssa.OpARM64SXTH
	case narrow == 2:
		op = ssa.OpARM64UXTH
	case narrow == 4 && sign && to.Size == 8:
		op = ssa.OpARM64SXTW
	case narrow == 4:
		op = ssa.OpARM64MOVWUreg
	case narrow == 8:
		op = ssa.OpCopy
	default:
		return false
	}
	e.Set(v, op, v.Args[0])
	return true
}

// lowerBitcast is a change of type and not of bits, so it is a copy and
// register allocation removes it.
func lowerBitcast(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) || isFloat(v.Args[0].Type) {
		return false
	}
	e.Set(v, ssa.OpCopy, v.Args[0])
	return true
}

// ---------------------------------------------------------------------------
// Group 2: loads and stores

func lowerLoad(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) {
		return false
	}
	op, ok := ssa.ARM64LoadOp(v.Type)
	if !ok {
		return false
	}
	ptr, mem := v.Args[0], v.Args[1]
	e.Set(v, op, ptr, mem)
	return true
}

func lowerStore(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Args[1].Type) {
		return false
	}
	op, ok := ssa.ARM64StoreOp(v.AuxInt)
	if !ok {
		return false
	}
	e.Set(v, op, v.Args[0], v.Args[1], v.Args[2])
	return true
}

// lowerMove and lowerZero are the calls of specs/031-runtime-lowering.md.
//
// specs/042 group 8 has the inline forms for a small constant size and they
// are not written yet, so every copy and every clear is a call. That is where
// lowering introduces a call into a block that had none, which is why liveness
// runs after this pass.
func lowerMove(v *ssa.Value, e *ssa.Edit) bool {
	n := constInto(v, e, v.AuxInt, typeU64)
	dst, src, mem := v.Args[0], v.Args[1], v.Args[2]
	e.Set(v, ssa.OpARM64CALLstatic, dst, src, n, mem)
	v.Aux = rtObj("runtime.memmove")
	return true
}

// lowerZero picks the clear by whether the region can hold a pointer.
// specs/031 says which: choosing the wrong one leaves stale pointers visible
// to the collector.
func lowerZero(v *ssa.Value, e *ssa.Edit) bool {
	n := constInto(v, e, v.AuxInt, typeU64)
	dst, mem := v.Args[0], v.Args[1]
	sym := "runtime.memclrNoHeapPointers"
	if hasPointers(dst.Type) {
		sym = "runtime.memclrHasPointers"
	}
	e.Set(v, ssa.OpARM64CALLstatic, dst, n, mem)
	v.Aux = rtObj(sym)
	return true
}

// hasPointers reports whether the region a pointer addresses can hold a
// pointer. The answer is the type's PtrBits, which ir.Layout computed once and
// which the collector reads, so nothing recomputes it here.
func hasPointers(ptr *ir.Type) bool {
	if ptr == nil || ptr.Elem == nil {
		// An address of unknown shape. Assume pointers: the clear that scans
		// is slower and the clear that does not is corruption.
		return true
	}
	for _, b := range ptr.Elem.PtrBits {
		if b != 0 {
			return true
		}
	}
	return len(ptr.Elem.PtrBits) == 0 && ptr.Elem.Size >= ir.PtrSize
}

// ---------------------------------------------------------------------------
// Group 3: address computation and the folds

func lowerAddr(v *ssa.Value, e *ssa.Edit) bool {
	obj := v.Aux
	e.Set(v, ssa.OpARM64MOVDaddr, e.SB())
	v.Aux = obj
	return true
}

// lowerLocalAddr drops the memory argument.
//
// OpLocalAddr takes memory so that an address computed before a store cannot
// float past it. The machine form does not need that: the address of a frame
// slot is the stack pointer plus a constant the ABI assigns, it reads nothing,
// and the store it was ordered against is still ordered by its own memory
// argument.
func lowerLocalAddr(v *ssa.Value, e *ssa.Edit) bool {
	obj := v.Aux
	e.Set(v, ssa.OpARM64ADDframe, e.SP())
	v.Aux = obj
	return true
}

func lowerOffPtr(v *ssa.Value, e *ssa.Edit) bool {
	off, ptr := v.AuxInt, v.Args[0]
	if ssa.ARM64ImmFits(ssa.OpARM64ADDconst, arm64.Size64, off) {
		e.Set(v, ssa.OpARM64ADDconst, ptr)
		v.AuxInt = off
		return true
	}
	c := constInto(v, e, off, typeI64)
	e.Set(v, ssa.OpARM64ADD, ptr, c)
	return true
}

// lowerPtrIndex scales the index and adds it.
//
// The shifted-register add is the whole point of the group: an element size
// that is a power of two costs one instruction, and the load that reads the
// result then folds even that away.
func lowerPtrIndex(v *ssa.Value, e *ssa.Edit) bool {
	esize, ptr, idx := v.AuxInt, v.Args[0], v.Args[1]
	switch {
	case esize == 1:
		e.Set(v, ssa.OpARM64ADD, ptr, idx)
	case esize > 0 && esize&(esize-1) == 0 &&
		ssa.ARM64ImmFits(ssa.OpARM64ADDshiftLL, arm64.Size64, log2(esize)):
		sh := log2(esize)
		e.Set(v, ssa.OpARM64ADDshiftLL, ptr, idx)
		v.AuxInt = sh
	default:
		c := constInto(v, e, esize, typeI64)
		e.Set(v, ssa.OpARM64MADD, idx, c, ptr)
	}
	return true
}

// foldAddconst collapses a chain of constant offsets.
//
// One rule, and it is the reason a field of a field of a struct is one add
// rather than three.
func foldAddconst(v *ssa.Value, e *ssa.Edit) bool {
	p := v.Args[0]
	if p.Op != ssa.OpARM64ADDconst {
		return false
	}
	sum := v.AuxInt + p.AuxInt
	if !ssa.ARM64ImmFits(ssa.OpARM64ADDconst, arm64.Size64, sum) {
		return false
	}
	v.AuxInt = sum
	e.SetArg(v, 0, p.Args[0])
	return true
}

// foldAddress is the address-mode fold of specs/025.
//
// Three shapes, in the order they are worth trying:
//
//	(MOVDload [off1] (ADDconst [off2] ptr) mem) && fits(off1+off2)
//	    => (MOVDload [off1+off2] ptr mem)
//	(MOVDload [0] (ADDshiftLL [3] ptr idx) mem) => (MOVDloadidx8 ptr idx mem)
//	(MOVDload [0] (ADD ptr idx) mem)            => (MOVDloadidx ptr idx mem)
//
// The register-offset forms have no offset field, so they apply only when the
// offset is already zero. The scaled form applies only when the shift is the
// access width, because the S bit is a scale by that width and not a general
// shift amount.
func foldAddress(v *ssa.Value, e *ssa.Edit) bool {
	p := v.Args[0]
	if p.Op == ssa.OpARM64ADDconst {
		sum := v.AuxInt + p.AuxInt
		if ssa.ARM64MemFits(v.Op, sum) {
			v.AuxInt = sum
			e.SetArg(v, 0, p.Args[0])
			return true
		}
		return false
	}
	if v.AuxInt != 0 {
		return false
	}
	scaled := false
	switch p.Op {
	case ssa.OpARM64ADD:
	case ssa.OpARM64ADDshiftLL:
		m, ok := ssa.ARM64MemOp(v.Op)
		if !ok || p.AuxInt != log2(m.Scale()) || m.Scale() == 1 {
			return false
		}
		scaled = true
	default:
		return false
	}
	idxOp, ok := ssa.ARM64IndexOp(v.Op, scaled)
	if !ok {
		return false
	}
	base, idx := p.Args[0], p.Args[1]
	if len(v.Args) == 2 {
		// A load is (base, mem) and its indexed form is (base, index, mem).
		e.Set(v, idxOp, base, idx, v.Args[1])
	} else {
		// A store is (base, value, mem) and its indexed form inserts the
		// index after the base.
		e.Set(v, idxOp, base, idx, v.Args[1], v.Args[2])
	}
	return true
}

// ---------------------------------------------------------------------------
// Group 4: branches and the condition-code forms

// lowerBlock lowers the control transfer of an If block.
//
// Two shapes. A comparison that only exists to be branched on becomes a
// conditional branch that reads the flags the compare already set, with no
// register in between. Anything else is a boolean in a register, and the
// compare-and-branch instruction tests it without a compare at all.
//
// The fold is guarded on the flags being produced in this block. A flags value
// is dead at the end of a block because every instruction in between may write
// the condition codes, and the graph does not say so: nothing stops a
// conditional set in a dominating block from being the control here.
func lowerBlock(b *ssa.Block, e *ssa.Edit) bool {
	c := b.Control
	if c == nil {
		return false
	}
	switch c.Op {
	case ssa.OpARM64BRcond, ssa.OpARM64CBZ, ssa.OpARM64CBNZ:
		return false
	}
	if c.Op == ssa.OpARM64CSET && c.Block == b && c.Args[0].Block == b {
		br := e.Insert(c.Pos, ssa.OpARM64BRcond, ssa.FlagsType, c.Args[0])
		br.Aux = c.Aux
		b.Control = br
		return true
	}
	br := e.Insert(c.Pos, ssa.OpARM64CBNZ, ssa.FlagsType, c)
	b.Control = br
	return true
}

// ---------------------------------------------------------------------------
// Group 5: calls, in all four shapes

// lowerStaticCall keeps the argument list and changes the operation.
//
// The deferred shape is a static call to runtime.deferproc and nothing else,
// because the target-neutral operation set has no defer operation. That is a
// gap in specs/021's set rather than a shape this rule invents: it is matched
// on the callee symbol, which is the only evidence there is.
func lowerStaticCall(v *ssa.Value, e *ssa.Edit) bool {
	op := ssa.OpARM64CALLstatic
	if isDeferSym(v.Aux) {
		op = ssa.OpARM64CALLdefer
	}
	aux := v.Aux
	e.Set(v, op, v.Args...)
	v.Aux = aux
	return true
}

func isDeferSym(aux any) bool {
	o, ok := aux.(*ir.Object)
	if !ok || o == nil {
		return false
	}
	return o.Name == "runtime.deferproc" || o.Name == "deferproc" ||
		o.Name == "runtime.deferprocStack" || o.Name == "deferprocStack"
}

// lowerClosureCall loads the entry point out of the closure.
//
// A closure is a code pointer followed by the captured variables, so the entry
// is the first word. The closure itself stays an argument, because
// specs/030-abi.md passes it in the closure register and the call reads the
// captures through it.
func lowerClosureCall(v *ssa.Value, e *ssa.Edit) bool {
	clo := v.Args[0]
	mem := v.Args[len(v.Args)-1]
	entry := e.Insert(v.Pos, ssa.OpARM64MOVDload, e.PtrType(), clo, mem)
	args := make([]*ssa.Value, 0, len(v.Args)+1)
	args = append(args, entry)
	args = append(args, v.Args...)
	e.Set(v, ssa.OpARM64CALLclosure, args...)
	return true
}

// lowerInterCall loads the method out of the itab.
//
// AuxInt is the byte offset of the method in the interface table, which
// specs/032-type-descriptors-and-itabs.md assigns. The rest is the closure
// shape: an indirect call through a register.
func lowerInterCall(v *ssa.Value, e *ssa.Edit) bool {
	itab := v.Args[0]
	mem := v.Args[len(v.Args)-1]
	entry := e.Insert(v.Pos, ssa.OpARM64MOVDload, e.PtrType(), itab, mem)
	entry.AuxInt = v.AuxInt
	args := make([]*ssa.Value, 0, len(v.Args)+1)
	args = append(args, entry)
	args = append(args, v.Args...)
	e.Set(v, ssa.OpARM64CALLinter, args...)
	return true
}

// lowerMakeResult is the return instruction.
//
// It keeps the result values as arguments so that register allocation can
// place them in their ABI registers, and it keeps memory last so that nothing
// moves across it.
func lowerMakeResult(v *ssa.Value, e *ssa.Edit) bool {
	e.Set(v, ssa.OpARM64RET, v.Args...)
	return true
}

// lowerNilCheck is a load that faults, not a branch.
//
// The check costs one instruction and no block. Its result is the pointer it
// checked, so every use of the check is redirected to the pointer and the
// check itself keeps its place in the memory chain.
func lowerNilCheck(v *ssa.Value, e *ssa.Edit) bool {
	ptr, mem := v.Args[0], v.Args[1]
	e.Replace(v, ptr)
	e.Set(v, ssa.OpARM64LoweredNilCheck, ptr, mem)
	return true
}

// lowerBoundsCheck expands a check into a branch to a block that calls the
// runtime, which is the expansion specs/021 says is not done at construction
// so that the graph stays readable.
//
// The comparison is unsigned, which is the whole trick: a negative index is a
// very large unsigned number, so one comparison rejects both ends of the
// range. The panic symbols never return, so the failure block is an Exit.
func lowerBoundsCheck(v *ssa.Value, e *ssa.Edit) bool {
	mem := v.Args[2]
	idx, limit := v.Args[0], v.Args[1]
	flags, _ := compareFlags(v, e, idx, limit, arm64.Size64)
	br := e.Insert(v.Pos, ssa.OpARM64BRcond, ssa.FlagsType, flags)
	sym := "runtime.goPanicIndex"
	if v.Op == ssa.OpSliceBoundsCheck {
		// A slice bound may equal the capacity, so the valid condition is
		// lower or same rather than lower.
		br.Aux = arm64.LS
		sym = "runtime.goPanicSliceAlen"
	} else {
		br.Aux = arm64.LO
	}
	fail, _ := e.Check(v, br)
	panicCall(fail, v, mem, sym, idx, limit)
	return true
}

// panicCall fills a failure block with a call that never returns.
func panicCall(fail *ssa.Block, v *ssa.Value, mem *ssa.Value, sym string, args ...*ssa.Value) {
	all := make([]*ssa.Value, 0, len(args)+1)
	all = append(all, args...)
	all = append(all, mem)
	c := fail.NewValue(v.Pos, ssa.OpARM64CALLstatic, ssa.MemType, all...)
	c.Aux = rtObj(sym)
	fail.Kind = ssa.BlockExit
	fail.Control = c
}

// rtObjs names each runtime symbol once, so that two calls to one symbol are
// two relocations against one name.
//
// It is built from rtsym, which specs/031 requires to be checked against the
// runtime's source rather than typed in. It is a lookup table and is never
// ranged over.
var rtObjs = func() map[string]*ir.Object {
	all := rtsym.All()
	m := make(map[string]*ir.Object, len(all))
	for _, s := range all {
		m[s.Name] = &ir.Object{Name: s.Name, Type: typeFunc, Class: ir.ClassFunc}
	}
	return m
}()

func rtObj(name string) *ir.Object {
	if o := rtObjs[name]; o != nil {
		return o
	}
	panic("rules: " + name + " is not in rtsym")
}
