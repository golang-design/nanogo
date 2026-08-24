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
//  6. Floating point. It reuses the rules of groups 1 to 4 rather than adding
//     a parallel set: the arithmetic, the comparison and the loads and stores
//     branch on the type of the value, because that is the only thing that
//     differs. What is genuinely new is the constant, which has an immediate
//     that reaches 256 values and an integer register for everything else,
//     and the condition codes, which are not the integer ones.
//
// Atomics and the inline memmove forms are groups 7 and 8 and are not here.
// Deferred lists what that leaves unlowered and why.

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

	// Group 6: floating point. The arithmetic, the comparison and the loads
	// and stores reach the rules above, which branch on the type; only the
	// three conversions are operations of their own.
	v[ssa.OpConstFloat] = lowerConst
	v[ssa.OpCvtIntToFloat] = lowerCvtIntToFloat
	v[ssa.OpCvtFloatToInt] = lowerCvtFloatToInt
	v[ssa.OpCvtFloatToFloat] = lowerCvtFloatToFloat

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
	ssa.OpARM64FMOVDload, ssa.OpARM64FMOVSload,
	ssa.OpARM64FMOVDstore, ssa.OpARM64FMOVSstore,
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
// One entry is left. It is not deferred by any spec: a string constant is two
// words, and a value that does not fit a register has to be split into the
// registers that hold it, which specs/025-lowering-and-rules.md's multi-word
// section owns and the decomposition pass performs. It cannot be worked around
// here, because a 16-byte Store cannot become a memmove: memmove needs a
// source address and Store has a source value.
//
// The four floating-point entries this list used to hold are gone. Group 6 is
// written and obj/arm64 encodes it.
var Deferred = []struct {
	Op     ssa.Op
	Reason string
}{
	{ssa.OpConstString, "a string constant is two words, and specs/025's decomposition splits it into its symbol and its length before selection sees it"},
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

// isScalarFloat reports whether t is float32 or float64, the two types that
// live in one floating-point register. A complex is two of them and is not
// one, which is why isFloat above is the wider predicate and this is the one
// the floating-point rules turn on.
func isScalarFloat(t *ir.Type) bool { return t != nil && t.Kind.IsFloat() }

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
	if isScalarFloat(v.Type) {
		return lowerFloatConst(v, e)
	}
	if isFloat(v.Type) {
		// A complex constant is two floats and does not fit one register.
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
	if isScalarFloat(v.Type) {
		// There is no floating-point add with an immediate, so the constant
		// folds of the integer path below have nothing to offer here.
		e.Set(v, ssa.OpARM64FADD, v.Args[0], v.Args[1])
		return true
	}
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
	if isScalarFloat(v.Type) {
		// Never rewritten to a negation of the second operand, which the
		// integer path below does for a zero first operand. 0.0 - (-0.0) is
		// +0.0 and -(-0.0) is +0.0, but 0.0 - (+0.0) is +0.0 where -(+0.0) is
		// -0.0, so the two disagree on a value Go can observe.
		e.Set(v, ssa.OpARM64FSUB, v.Args[0], v.Args[1])
		return true
	}
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
	if isScalarFloat(v.Type) {
		// A multiply by a power of two is not a shift here, and it is not
		// even always exact: the exponent can overflow or go subnormal.
		e.Set(v, ssa.OpARM64FMUL, v.Args[0], v.Args[1])
		return true
	}
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
	if isScalarFloat(v.Type) {
		// No guard. A floating-point divide by zero is defined by IEEE 754 as
		// an infinity, or as a NaN when the numerator is zero too, and Go
		// says the same. It is the integer divide that panics.
		e.Set(v, ssa.OpARM64FDIV, v.Args[0], v.Args[1])
		return true
	}
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
	if isScalarFloat(v.Type) {
		// FNEG and not a subtraction from zero. FNEG inverts the sign bit, so
		// -(+0.0) is -0.0 and the sign of a NaN flips, which is what Go
		// defines; 0.0 - x gets both of those wrong.
		e.Set(v, ssa.OpARM64FNEG, v.Args[0])
		return true
	}
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
	if isScalarFloat(x.Type) {
		return lowerFloatCompare(v, e, x, y)
	}
	if isFloat(x.Type) {
		// A complex comparison is two comparisons and an AND, which nothing
		// above this pass has decomposed.
		return false
	}
	// An operand that does not fit one register has no compare instruction,
	// and refusing it is not optional. ARM64Size answers Size64 for anything
	// wider than four bytes, so without this a 16-byte string comparison
	// became a 64-bit compare of its first word: code that runs, computes the
	// wrong answer, and reports nothing.
	//
	// String and interface equality are not per-part comparisons either, so
	// the decomposition pass leaves them whole on purpose. They become runtime
	// calls, per specs/020-ir.md's lowering table.
	if _, ok := ssa.ClassOfType(x.Type); !ok {
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

// lowerBitcast is a change of type and not of bits.
//
// Within one register file it is a copy and register allocation removes it.
// Across the two files it is an instruction, because the bits have to move
// from an integer register to a floating-point one or back, which is what
// math.Float64bits and math.Float64frombits are.
func lowerBitcast(v *ssa.Value, e *ssa.Edit) bool {
	to, from := v.Type, v.Args[0].Type
	if to == nil || from == nil || to.Size != from.Size {
		return false
	}
	toF, fromF := isScalarFloat(to), isScalarFloat(from)
	switch {
	case isFloat(to) != toF || isFloat(from) != fromF:
		// One side is a complex, which is two registers and not one move.
		return false
	case toF && !fromF:
		e.Set(v, ssa.OpARM64FMOVgpfp, v.Args[0])
	case fromF && !toF:
		e.Set(v, ssa.OpARM64FMOVfpgp, v.Args[0])
	default:
		e.Set(v, ssa.OpCopy, v.Args[0])
	}
	return true
}

// ---------------------------------------------------------------------------
// Group 2: loads and stores

func lowerLoad(v *ssa.Value, e *ssa.Edit) bool {
	if isFloat(v.Type) && !isScalarFloat(v.Type) {
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
	t := v.Args[1].Type
	if isFloat(t) && !isScalarFloat(t) {
		return false
	}
	op, ok := ssa.ARM64StoreOp(v.AuxInt)
	if isScalarFloat(t) {
		// The type and not the size. A four-byte store from an integer
		// register and one from a floating-point register are different
		// instructions, and AuxInt cannot tell them apart.
		op, ok = ssa.ARM64StoreOpForType(t)
		if ok && t.Size != v.AuxInt {
			// The store's width and the value's disagree. A floating-point
			// store transfers the whole register, so there is no form of it
			// that writes some other number of bytes.
			return false
		}
	}
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

// ---------------------------------------------------------------------------
// Group 6: floating point
//
// What is here is only what differs from the integer rules. The arithmetic,
// the loads and the stores branch inside the rules above, because the shape of
// the rewrite is the same and only the operation changes. Three things are
// genuinely different and are here: the constant, the comparison, and the
// conversions.

// floatConstOf returns the value a floating-point constant holds.
//
// It accepts both spellings for the reason argConst does: the engine walks a
// block forwards, so a constant defined earlier is usually already a machine
// operation while one defined in another block may not be. The machine form
// carries the bit pattern, which is why the width is read back from the type.
//
// It is deliberately separate from valConst. Feeding a floating-point constant
// to valConst would offer it to the integer folds, and a rule that turned a
// float into an integer NEG or an ADD immediate would produce code that runs
// and computes the wrong answer.
func floatConstOf(a *ssa.Value) (float64, bool) {
	switch a.Op {
	case ssa.OpConstFloat:
		if a.Aux == nil {
			// The zero value of the type, which build leaves with no Aux.
			return 0, true
		}
		f, ok := a.Aux.(float64)
		return f, ok
	case ssa.OpARM64FMOVconst:
		return arm64.FloatFromBits(width(a), uint64(a.AuxInt)), true
	case ssa.OpARM64FMOVgpfp:
		// The other machine spelling of a constant: a bit pattern built in an
		// integer register and moved across. It is the spelling zero takes,
		// which is the one the compare against zero has to recognise.
		if c, ok := valConst(a.Args[0]); ok {
			return arm64.FloatFromBits(width(a), uint64(c)), true
		}
	}
	return 0, false
}

// lowerFloatConst materialises a floating-point constant.
//
// Two forms. FMOV's immediate names 256 values, a sign and four fraction bits
// against an exponent window of eight around 1, so 1.5 and -0.125 are one
// instruction. Everything else, zero and the infinities included, has the bit
// pattern built in an integer register and moved across with FMOV. arm64
// therefore needs no constant pool for a float, which is the target choice
// specs/025's constants section leaves open.
//
// The pattern is materialised with an integer type on purpose. The register
// allocator takes a value's class from its own type, so a constant that
// carried the float type would be given a floating-point register and the FMOV
// that reads it as a general register would read the wrong file.
func lowerFloatConst(v *ssa.Value, e *ssa.Edit) bool {
	val, ok := floatConstOf(v)
	if !ok {
		// A constant the front end could not parse back out of its text. It
		// is left unlowered rather than guessed at.
		return false
	}
	sz := width(v)
	bits := int64(arm64.FloatBits(sz, val))
	if _, ok := arm64.FloatImm8(val); ok {
		e.Set(v, ssa.OpARM64FMOVconst)
		v.AuxInt = bits
		return true
	}
	c := constInto(v, e, bits, typeU64)
	e.Set(v, ssa.OpARM64FMOVgpfp, c)
	return true
}

// condOfFloat is the condition code a Go comparison of two floats becomes.
//
// It is not condOf with the same four answers, and the difference is IEEE 754.
// FCMP has four outcomes and sets V only in the unordered one, where either
// operand is a NaN. Go requires every ordered comparison to be false there and
// != to be true, so:
//
//	==  EQ   Z set, which is the equal row alone
//	!=  NE   Z clear, which is less, greater and unordered
//	<   MI   N set, which is the less row alone
//	<=  LS   C clear or Z set, which is less or equal and neither of the rest
//
// LT and LE, which the integer rules use, are both true in the unordered row
// because N != V holds there. A float compare lowered with them would report
// NaN < NaN as true.
//
// There is no Greater and no GreaterEqual to answer for: specs/021 rewrites
// x > y as y < x, and the swap keeps the semantics, because the unordered row
// is symmetric and stays false either way.
func condOfFloat(op ssa.Op) arm64.Cond {
	switch op {
	case ssa.OpEq:
		return arm64.EQ
	case ssa.OpNeq:
		return arm64.NE
	case ssa.OpLess:
		return arm64.MI
	default:
		return arm64.LS
	}
}

// lowerFloatCompare produces an FCMP and a conditional set.
func lowerFloatCompare(v *ssa.Value, e *ssa.Edit, x, y *ssa.Value) bool {
	if !isScalarFloat(y.Type) || x.Type.Size != y.Type.Size {
		return false
	}
	// The condition is read before the operation is replaced, because e.Set
	// overwrites v.Op and the condition is derived from it.
	cond := condOfFloat(v.Op)
	flags, cond := compareFloatFlags(v, e, x, y, cond)
	e.Set(v, ssa.OpARM64CSET, flags)
	v.Aux = cond
	return true
}

// compareFloatFlags emits the compare and returns the flags value, with the
// condition the caller must use.
//
// The compare against zero is the only immediate form the class has, and it
// earns its place because zero is exactly the constant FMOV's immediate cannot
// reach: without the fold, x == 0 would cost a move-wide, an FMOV across the
// files, and then the compare.
//
// The fold applies to a zero on either side, and that is not symmetry for its
// own sake. specs/021 rewrites x > 0 as Less(0, x), so the constant of the most
// common float test in Go lands in the first operand. Folding it means
// comparing the other operand against zero and reading the opposite condition,
// which is where GT and GE come from: they are unordered-false, as MI and LS
// are, so an ordered comparison stays false for a NaN.
//
// A constant of negative zero folds too. IEEE 754 makes +0.0 and -0.0 compare
// equal, so the sign cannot change which of the four rows the compare lands
// in, and every condition here reads only those rows.
func compareFloatFlags(v *ssa.Value, e *ssa.Edit, x, y *ssa.Value, cond arm64.Cond) (*ssa.Value, arm64.Cond) {
	op := ssa.OpARM64FCMPS
	zero := ssa.OpARM64FCMPS0
	if width(x) == arm64.Size64 {
		op, zero = ssa.OpARM64FCMPD, ssa.OpARM64FCMPD0
	}
	if c, ok := floatConstOf(y); ok && c == 0 {
		return e.Insert(v.Pos, zero, ssa.FlagsType, x), cond
	}
	if c, ok := floatConstOf(x); ok && c == 0 {
		return e.Insert(v.Pos, zero, ssa.FlagsType, y), swapFloatCond(cond)
	}
	return e.Insert(v.Pos, op, ssa.FlagsType, x, y), cond
}

// swapFloatCond returns the condition that reads the compare with its operands
// exchanged.
//
// Equality is symmetric and keeps its condition. The two orderings become the
// other pair, which is the pair the integer rules never reach because specs/021
// canonicalises Greater away: GT is C set and Z clear, GE is N equal to V, and
// both are false in the unordered row, so the NaN behaviour is the one Go
// defines.
func swapFloatCond(c arm64.Cond) arm64.Cond {
	switch c {
	case arm64.MI:
		return arm64.GT
	case arm64.LS:
		return arm64.GE
	}
	return c
}

// lowerCvtIntToFloat is SCVTF or UCVTF, chosen by the source's signedness.
//
// Both widths come from the two types, so one operation covers the four
// instructions the architecture has. A source narrower than a word is already
// extended in its register, because lowerExt above put it there, and the
// conversion reads the whole W register.
func lowerCvtIntToFloat(v *ssa.Value, e *ssa.Edit) bool {
	x := v.Args[0]
	if !isScalarFloat(v.Type) || !isIntClass(x.Type) {
		return false
	}
	op := ssa.OpARM64UCVTF
	if signed(x.Type) {
		op = ssa.OpARM64SCVTF
	}
	e.Set(v, op, x)
	return true
}

// lowerCvtFloatToInt is FCVTZS or FCVTZU, chosen by the destination's
// signedness. Both round toward zero, which is the truncation Go defines.
//
// No range check is emitted and none is required. Go leaves the result
// unspecified when the value is not representable in the destination type, and
// the instruction saturates there: a value above the largest integer produces
// that integer and a NaN produces zero. What the rule relies on is only the
// in-range behaviour, which is the truncation Go specifies.
func lowerCvtFloatToInt(v *ssa.Value, e *ssa.Edit) bool {
	x := v.Args[0]
	if !isScalarFloat(x.Type) || !isIntClass(v.Type) {
		return false
	}
	op := ssa.OpARM64FCVTZU
	if signed(v.Type) {
		op = ssa.OpARM64FCVTZS
	}
	e.Set(v, op, x)
	return true
}

// lowerCvtFloatToFloat is FCVT, which has no encoding from a width to itself.
// A conversion between two types of the same width is a change of type and
// not of bits, so it is a copy.
func lowerCvtFloatToFloat(v *ssa.Value, e *ssa.Edit) bool {
	x := v.Args[0]
	if !isScalarFloat(v.Type) || !isScalarFloat(x.Type) {
		return false
	}
	if width(v) == width(x) {
		e.Set(v, ssa.OpCopy, x)
		return true
	}
	e.Set(v, ssa.OpARM64FCVT, x)
	return true
}

// isIntClass reports whether a value of type t lives in one integer register.
// It is ssa.ClassOfType, asked rather than restated, so that the rules and the
// allocator cannot disagree about where a value goes.
func isIntClass(t *ir.Type) bool {
	c, ok := ssa.ClassOfType(t)
	return ok && c == ssa.ClassInt
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
