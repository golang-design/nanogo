// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rules

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj/arm64"
	"golang.design/x/nanogo/ssa"
)

// The group 6 rules of specs/042-arm64-backend.md, per rule, in the form the
// rest of this package's tests use.

var tF32 = &ir.Type{Kind: ir.Float32, Size: 4, Align: 4, Name: "float32"}

// fkonst builds a floating-point constant in the shape specs/021 builds one:
// the value in Aux and nothing in AuxInt.
func (p *builder) fkonst(t *ir.Type, val float64) *ssa.Value {
	v := p.b.NewValue(0, ssa.OpConstFloat, t)
	v.Aux = val
	return v
}

func TestARM64FloatArithmetic(t *testing.T) {
	runRules(t, []ruleTest{
		{"a double add is FADD", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAdd, tF64, p.arg(tF64), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Arg <float64>
  t3 = ARM64FADD <float64> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a single add is the same operation at another width", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAdd, tF32, p.arg(tF32), p.arg(tF32)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float32>
  t2 = Arg <float32>
  t3 = ARM64FADD <float32> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a subtract from zero stays a subtract", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpSub, tF64, p.fkonst(tF64, 0), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = ARM64MOVDconst <uint64> [0]
  t2 = ARM64FMOVgpfp <float64> t1
  t3 = Arg <float64>
  t4 = ARM64FSUB <float64> t2 t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"a multiply by a power of two stays a multiply", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpMul, tF64, p.arg(tF64), p.fkonst(tF64, 8)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FMOVconst <float64> [4620693217682128896]
  t3 = ARM64FMUL <float64> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a divide needs no guard", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpDiv, tF64, p.arg(tF64), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Arg <float64>
  t3 = ARM64FDIV <float64> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a negation is FNEG and not a subtract", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpNeg, tF64, p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FNEG <float64> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
	})
}

// TestARM64FloatConstant covers the two forms a floating-point constant takes.
//
// The immediate reaches 256 values and nothing else, so the second form is not
// an optimisation that was skipped: it is the only way zero, an infinity or
// 0.1 arrives in a register.
func TestARM64FloatConstant(t *testing.T) {
	runRules(t, []ruleTest{
		{"a value the immediate reaches is one instruction", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.fkonst(tF64, 1.5))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = ARM64FMOVconst <float64> [4609434218613702656]
  t2 = ARM64RET <mem> t1 t0
  Ret t2
`},
		{"a value it does not goes through an integer register", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.fkonst(tF64, 0.1))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = ARM64MOVDconst <uint64> [4591870180066957722]
  t2 = ARM64FMOVgpfp <float64> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"zero is not an immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.fkonst(tF64, 0))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = ARM64MOVDconst <uint64> [0]
  t2 = ARM64FMOVgpfp <float64> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a single-precision constant carries the 32-bit pattern", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.fkonst(tF32, 1.5))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = ARM64FMOVconst <float32> [1069547520]
  t2 = ARM64RET <mem> t1 t0
  Ret t2
`},
		{"an infinity goes through an integer register", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.fkonst(tF64, math.Inf(1)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = ARM64MOVDconst <uint64> [9218868437227405312]
  t2 = ARM64FMOVgpfp <float64> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
	})
}

// TestARM64FloatConstantPatternIsAnInteger asserts the type of the value the
// pattern is built in.
//
// The register allocator takes a value's class from its own type. A constant
// that carried the float type would be given a floating-point register, and
// the FMOV that reads it as a general register would read the other file. The
// dump above says uint, and this says why that matters.
func TestARM64FloatConstantPatternIsAnInteger(t *testing.T) {
	p := newBuilder()
	f := lower(t, p.ret(p.fkonst(tF64, 0.1)))
	var found bool
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != ssa.OpARM64FMOVgpfp {
				continue
			}
			found = true
			c, ok := ssa.ClassOfType(v.Args[0].Type)
			if !ok || c != ssa.ClassInt {
				t.Errorf("the pattern of %s has class %v, want the integer class",
					v.LongString(), c)
			}
			if c, ok := ssa.ClassOfType(v.Type); !ok || c != ssa.ClassFloat {
				t.Errorf("the result of %s has class %v, want the float class",
					v.LongString(), c)
			}
		}
	}
	if !found {
		t.Fatal("the constant did not go through an integer register")
	}
}

// TestARM64FloatCompare covers the condition codes and the compare against
// zero.
func TestARM64FloatCompare(t *testing.T) {
	runRules(t, []ruleTest{
		{"equality is EQ", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpEq, tBool, p.arg(tF64), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Arg <float64>
  t3 = ARM64FCMPD <flags> t1 t2
  t4 = ARM64CSET <bool> {EQ} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"inequality is NE, which is the one condition true of a NaN", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpNeq, tBool, p.arg(tF64), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Arg <float64>
  t3 = ARM64FCMPD <flags> t1 t2
  t4 = ARM64CSET <bool> {NE} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"less than is MI and not LT", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tF64), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Arg <float64>
  t3 = ARM64FCMPD <flags> t1 t2
  t4 = ARM64CSET <bool> {MI} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"less or equal is LS and not LE", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLeq, tBool, p.arg(tF64), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Arg <float64>
  t3 = ARM64FCMPD <flags> t1 t2
  t4 = ARM64CSET <bool> {LS} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"a single-precision compare is the other width", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tF32), p.arg(tF32)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float32>
  t2 = Arg <float32>
  t3 = ARM64FCMPS <flags> t1 t2
  t4 = ARM64CSET <bool> {MI} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"a compare against zero needs no register", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpEq, tBool, p.arg(tF64), p.fkonst(tF64, 0)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCMPD0 <flags> t1
  t3 = ARM64CSET <bool> {EQ} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a compare against negative zero folds the same way", func() *ssa.Func {
			p := newBuilder()
			z := p.fkonst(tF64, math.Copysign(0, -1))
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tF64), z))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCMPD0 <flags> t1
  t3 = ARM64CSET <bool> {MI} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a zero in the first operand folds and swaps the condition", func() *ssa.Func {
			// The shape specs/021 gives x > 0.0, which it rewrites as
			// Less(0.0, x). It is the most common float test in Go, so the
			// fold has to reach the constant on either side.
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.fkonst(tF64, 0), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCMPD0 <flags> t1
  t3 = ARM64CSET <bool> {GT} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"and the same for x >= 0.0", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLeq, tBool, p.fkonst(tF64, 0), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCMPD0 <flags> t1
  t3 = ARM64CSET <bool> {GE} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"equality against a leading zero keeps its condition", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpEq, tBool, p.fkonst(tF64, 0), p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCMPD0 <flags> t1
  t3 = ARM64CSET <bool> {EQ} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a compare against a value that is not zero keeps the register form", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tF64), p.fkonst(tF64, 1.5)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FMOVconst <float64> [4609434218613702656]
  t3 = ARM64FCMPD <flags> t1 t2
  t4 = ARM64CSET <bool> {MI} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"a comparison of a value with itself is the IsNaN idiom", func() *ssa.Func {
			p := newBuilder()
			x := p.arg(tF64)
			return p.ret(p.val(ssa.OpNeq, tBool, x, x))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCMPD <flags> t1 t1
  t3 = ARM64CSET <bool> {NE} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
	})
}

// TestARM64FloatBranch is group 4 applied to a floating-point compare.
//
// A comparison that only feeds a branch never reaches a register: the fold
// turns it into a conditional branch that reads the flags FCMP already set.
// The condition is group 6's and not the integer one, so this is where the two
// groups meet.
func TestARM64FloatBranch(t *testing.T) {
	runRules(t, []ruleTest{
		{"a float compare that only feeds a branch needs no register", func() *ssa.Func {
			p := newBuilder()
			x, y := p.arg(tF64), p.arg(tF64)
			c := p.val(ssa.OpLess, tBool, x, y)
			b0 := p.b
			b0.Kind = ssa.BlockIf
			b0.Control = c
			then := p.block(ssa.BlockRet)
			p.ret(x)
			els := p.block(ssa.BlockRet)
			p.ret(x)
			b0.AddEdgeTo(then)
			b0.AddEdgeTo(els)
			return p.f
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Arg <float64>
  t3 = ARM64FCMPD <flags> t1 t2
  t4 = ARM64BRcond <flags> {MI} t3
  If t4 -> b1 b2
b1: <- b0
  t5 = ARM64RET <mem> t1 t0
  Ret t5
b2: <- b0
  t6 = ARM64RET <mem> t1 t0
  Ret t6
`},
		{"and a compare against zero branches on the immediate form", func() *ssa.Func {
			p := newBuilder()
			x := p.arg(tF64)
			c := p.val(ssa.OpLess, tBool, p.fkonst(tF64, 0), x)
			b0 := p.b
			b0.Kind = ssa.BlockIf
			b0.Control = c
			then := p.block(ssa.BlockRet)
			p.ret(x)
			els := p.block(ssa.BlockRet)
			p.ret(x)
			b0.AddEdgeTo(then)
			b0.AddEdgeTo(els)
			return p.f
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCMPD0 <flags> t1
  t3 = ARM64BRcond <flags> {GT} t2
  If t3 -> b1 b2
b1: <- b0
  t4 = ARM64RET <mem> t1 t0
  Ret t4
b2: <- b0
  t5 = ARM64RET <mem> t1 t0
  Ret t5
`},
	})
}

// TestARM64FloatConversions covers group 6's conversions.
func TestARM64FloatConversions(t *testing.T) {
	runRules(t, []ruleTest{
		{"a signed integer widens with SCVTF", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtIntToFloat, tF64, p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64SCVTF <float64> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"an unsigned integer widens with UCVTF", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtIntToFloat, tF64, p.arg(tU32)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <uint32>
  t2 = ARM64UCVTF <float64> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a float narrows to a signed integer with FCVTZS", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtFloatToInt, tI64, p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCVTZS <int> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"and to an unsigned one with FCVTZU", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtFloatToInt, tU64, p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCVTZU <uint> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a single widens to a double with FCVT", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtFloatToFloat, tF64, p.arg(tF32)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float32>
  t2 = ARM64FCVT <float64> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a double narrows to a single with the same operation", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtFloatToFloat, tF32, p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FCVT <float32> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a conversion between two types of one width is a copy", func() *ssa.Func {
			p := newBuilder()
			other := &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "myfloat"}
			return p.ret(p.val(ssa.OpCvtFloatToFloat, other, p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Copy <myfloat> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a bit conversion to a float is FMOV across the files", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpBitcast, tF64, p.arg(tU64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <uint>
  t2 = ARM64FMOVgpfp <float64> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"and from a float it is the other direction", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpBitcast, tU64, p.arg(tF64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = ARM64FMOVfpgp <uint> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
	})
}

// TestARM64FloatMemory covers the loads and stores and the address folds they
// inherit from group 3.
func TestARM64FloatMemory(t *testing.T) {
	tPtrF := &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, Elem: tF64, Name: "*float64"}
	runRules(t, []ruleTest{
		{"a double load", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLoad, tF64, p.arg(tPtrF), p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*float64>
  t2 = ARM64FMOVDload <float64> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a single load", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLoad, tF32, p.arg(tPtrF), p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*float64>
  t2 = ARM64FMOVSload <float32> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a constant offset folds into the load", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpOffPtr, tPtrF, p.arg(tPtrF)), 16, nil)
			return p.ret(p.val(ssa.OpLoad, tF64, a, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*float64>
  t2 = ARM64FMOVDload <float64> [16] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a scaled index folds into the load", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpPtrIndex, tPtrF, p.arg(tPtrF), p.arg(tI64)), 8, nil)
			return p.ret(p.val(ssa.OpLoad, tF64, a, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*float64>
  t2 = Arg <int>
  t3 = ARM64FMOVDloadidx8 <float64> t1 t2 t0
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a double store", func() *ssa.Func {
			p := newBuilder()
			s := aux(p.val(ssa.OpStore, ssa.MemType, p.arg(tPtrF), p.arg(tF64), p.mem), 8, nil)
			p.setMem(s)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*float64>
  t2 = Arg <float64>
  t3 = ARM64FMOVDstore <mem> [0] t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"a single store", func() *ssa.Func {
			p := newBuilder()
			s := aux(p.val(ssa.OpStore, ssa.MemType, p.arg(tPtrF), p.arg(tF32), p.mem), 4, nil)
			p.setMem(s)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*float64>
  t2 = Arg <float32>
  t3 = ARM64FMOVSstore <mem> [0] t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"a scaled index folds into the store", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpPtrIndex, tPtrF, p.arg(tPtrF), p.arg(tI64)), 8, nil)
			s := aux(p.val(ssa.OpStore, ssa.MemType, a, p.arg(tF64), p.mem), 8, nil)
			p.setMem(s)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*float64>
  t2 = Arg <int>
  t3 = Arg <float64>
  t4 = ARM64FMOVDstoreidx8 <mem> t1 t2 t3 t0
  t5 = ARM64RET <mem> t4
  Ret t5
`},
	})
}

// ---------------------------------------------------------------------------
// IEEE 754 semantics
//
// The condition code a float comparison lowers to is the one place where an
// encoder that is byte-for-byte right still produces a wrong program, so it is
// checked twice: once against the ARM flag table, evaluated here, and once by
// running the instructions on the host.

// nzcv is the four condition flags an FCMP sets.
type nzcv struct{ n, z, c, v bool }

// fcmpFlags is the ARM manual's table for FCMP, written as the definition and
// not as a lowering of it. Four outcomes, and V is set in exactly one.
func fcmpFlags(x, y float64) nzcv {
	switch {
	case math.IsNaN(x) || math.IsNaN(y):
		return nzcv{false, false, true, true} // unordered
	case x < y:
		return nzcv{true, false, false, false}
	case x == y:
		return nzcv{false, true, true, false}
	default:
		return nzcv{false, false, true, false}
	}
}

// condHolds evaluates a condition code against the flags, from the manual's
// ConditionHolds.
func condHolds(c arm64.Cond, f nzcv) bool {
	switch c {
	case arm64.EQ:
		return f.z
	case arm64.NE:
		return !f.z
	case arm64.HS:
		return f.c
	case arm64.LO:
		return !f.c
	case arm64.MI:
		return f.n
	case arm64.PL:
		return !f.n
	case arm64.VS:
		return f.v
	case arm64.VC:
		return !f.v
	case arm64.HI:
		return f.c && !f.z
	case arm64.LS:
		return !f.c || f.z
	case arm64.GE:
		return f.n == f.v
	case arm64.LT:
		return f.n != f.v
	case arm64.GT:
		return !f.z && f.n == f.v
	case arm64.LE:
		return f.z || f.n != f.v
	}
	return true
}

// floatCases are the values a comparison has to be right about. NaN is the
// reason the table exists, and the two zeros and the two infinities are the
// values next most likely to be got wrong.
func floatCases() []float64 {
	return []float64{
		math.NaN(), 0, math.Copysign(0, -1),
		math.Inf(1), math.Inf(-1),
		-1.5, 1.5, 3, -3,
		math.MaxFloat64, -math.MaxFloat64,
		math.SmallestNonzeroFloat64,
	}
}

// goCompare is what Go defines each operator to mean, written with Go's own
// operators so that the expectation is the language and not a restatement of
// the rule.
func goCompare(op ssa.Op, x, y float64) bool {
	switch op {
	case ssa.OpEq:
		return x == y
	case ssa.OpNeq:
		return x != y
	case ssa.OpLess:
		return x < y
	default:
		return x <= y
	}
}

// TestFloatConditionCodes asserts that the condition condOfFloat picks
// computes what Go defines, over every pair of the interesting values.
//
// This is the check that catches the mistake the integer conditions invite:
// LT and LE are true in the unordered row, so a float comparison lowered with
// them reports NaN < NaN as true.
func TestFloatConditionCodes(t *testing.T) {
	ops := []ssa.Op{ssa.OpEq, ssa.OpNeq, ssa.OpLess, ssa.OpLeq}
	vals := floatCases()
	n := 0
	for _, op := range ops {
		c := condOfFloat(op)
		for _, x := range vals {
			for _, y := range vals {
				got := condHolds(c, fcmpFlags(x, y))
				want := goCompare(op, x, y)
				if got != want {
					t.Errorf("%v(%v, %v) lowered to %v gives %v, Go says %v",
						op, x, y, c, got, want)
				}
				n++
			}
		}
	}
	// The swapped forms. specs/021 rewrites x > y as y < x, so the same two
	// conditions have to answer for the other two operators.
	for _, x := range vals {
		for _, y := range vals {
			if got := condHolds(condOfFloat(ssa.OpLess), fcmpFlags(y, x)); got != (x > y) {
				t.Errorf("%v > %v lowered as the swapped less-than gives %v", x, y, got)
			}
			if got := condHolds(condOfFloat(ssa.OpLeq), fcmpFlags(y, x)); got != (x >= y) {
				t.Errorf("%v >= %v lowered as the swapped less-or-equal gives %v", x, y, got)
			}
			n += 2
		}
	}
	// The zero fold, which compares the other operand against zero and reads
	// the swapped condition. GT and GE appear only here: the integer rules
	// never reach them, because specs/021 canonicalises Greater away.
	for _, x := range vals {
		if got := condHolds(swapFloatCond(condOfFloat(ssa.OpLess)), fcmpFlags(x, 0)); got != (0 < x) {
			t.Errorf("0 < %v folded to a compare against zero gives %v", x, got)
		}
		if got := condHolds(swapFloatCond(condOfFloat(ssa.OpLeq)), fcmpFlags(x, 0)); got != (0 <= x) {
			t.Errorf("0 <= %v folded to a compare against zero gives %v", x, got)
		}
		if got := condHolds(swapFloatCond(condOfFloat(ssa.OpEq)), fcmpFlags(x, 0)); got != (0 == x) {
			t.Errorf("0 == %v folded to a compare against zero gives %v", x, got)
		}
		if got := condHolds(swapFloatCond(condOfFloat(ssa.OpNeq)), fcmpFlags(x, 0)); got != (0 != x) {
			t.Errorf("0 != %v folded to a compare against zero gives %v", x, got)
		}
		n += 4
	}
	t.Logf("%d comparisons evaluated against the ARM flag table", n)
	// The conditions the integer rules use must fail here. Without this the
	// test above would pass for a lowering that never distinguished them.
	nan := fcmpFlags(math.NaN(), math.NaN())
	if !condHolds(arm64.LT, nan) || !condHolds(arm64.LE, nan) {
		t.Error("LT and LE are not true in the unordered row, so the flag table is wrong")
	}
	if condHolds(arm64.MI, nan) || condHolds(arm64.LS, nan) {
		t.Error("MI or LS is true in the unordered row")
	}
}

// ---------------------------------------------------------------------------
// The NaN semantics, executed

// probeOps are the four operators, in the order the generated probe reports
// them.
var probeOps = []struct {
	name string
	op   ssa.Op
}{
	{"==", ssa.OpEq},
	{"!=", ssa.OpNeq},
	{"<", ssa.OpLess},
	{"<=", ssa.OpLeq},
}

// TestFloatNaNOnHardware runs the lowered comparison on the machine.
//
// The table test above evaluates the ARM manual's flag rules in Go, so it is
// only as right as the transcription. This one assembles an FCMP and the
// conditional set with the condition condOfFloat chose, runs it over NaN, the
// two zeros, the two infinities and ordinary values, and compares against
// Go's own operator on the same pair.
//
// It cannot go through ssagen, which is the stronger form specs/004 asks for:
// that package's argument assignment refuses a floating-point argument or
// result, because the ABI pass that would place one is not written. So the
// probe is a Plan 9 assembly stub built by the host tool chain, and what it
// proves is the one thing left that a byte-exact encoder cannot: that the
// condition code selected for each Go operator is the right one.
func TestFloatNaNOnHardware(t *testing.T) {
	// The gate is NANOGO_REQUIRE_LINK, not NANOGO_REQUIRE_CORPUS. The two mean
	// different things and conflating them made this test fail on the amd64
	// runner: a corpus is a directory that either exists or does not, while
	// running arm64 instructions needs an arm64 host. CI sets the link
	// variable on the arm64 runner alone, which is exactly where this probe
	// can run.
	if runtime.GOARCH != "arm64" {
		if os.Getenv("NANOGO_REQUIRE_LINK") == "1" {
			t.Fatalf("NANOGO_REQUIRE_LINK=1 on %s and the probe executes arm64 instructions", runtime.GOARCH)
		}
		t.Skipf("the probe runs the instructions, so it needs an arm64 host, not %s", runtime.GOARCH)
	}

	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("go.mod", "module nanogofloatprobe\n\ngo 1.24\n")
	write("probe_arm64.s", probeAsm())
	write("main.go", probeMain())

	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if os.Getenv("NANOGO_REQUIRE_LINK") == "1" {
			t.Fatalf("go run: %v\n%s", err, out)
		}
		t.Skipf("the probe did not build or run: %v\n%s", err, out)
	}
	text := strings.TrimSpace(string(out))
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "checked ") {
			t.Log(line)
			continue
		}
		if line != "" {
			t.Errorf("%s", line)
		}
	}
}

// probeAsm builds the stub. Each function is one FCMP and one conditional set,
// with the condition taken from condOfFloat, so the assembly under test is the
// rule's own answer and not a copy of it.
func probeAsm() string {
	var b strings.Builder
	b.WriteString("// Generated by ssa/rules/float_test.go.\n\n")
	b.WriteString("#include \"textflag.h\"\n")
	for i, o := range probeOps {
		b.WriteString(fmt.Sprintf(`
TEXT ·cmpD%d(SB), NOSPLIT, $0-24
	FMOVD	x+0(FP), F0
	FMOVD	y+8(FP), F1
	FCMPD	F1, F0
	CSET	%v, R0
	MOVD	R0, ret+16(FP)
	RET

TEXT ·cmpS%d(SB), NOSPLIT, $0-16
	FMOVS	x+0(FP), F0
	FMOVS	y+4(FP), F1
	FCMPS	F1, F0
	CSET	%v, R0
	MOVD	R0, ret+8(FP)
	RET
`, i, condOfFloat(o.op), i, condOfFloat(o.op)))
	}
	// The swapped conditions, which the zero fold reads. Each stub compares
	// its one operand against the immediate zero, which is the instruction
	// compareFloatFlags emits for a constant in the first operand.
	for i, o := range probeOps {
		b.WriteString(fmt.Sprintf(`
TEXT ·zeroD%d(SB), NOSPLIT, $0-16
	FMOVD	x+0(FP), F0
	FCMPD	$(0.0), F0
	CSET	%v, R0
	MOVD	R0, ret+8(FP)
	RET
`, i, swapFloatCond(condOfFloat(o.op))))
	}
	// The two conditions the integer rules use. They are in the probe so that
	// a run which reports nothing proves the probe can tell the two apart:
	// both of these are true in the unordered row and must disagree with Go.
	for i, c := range []arm64.Cond{arm64.LT, arm64.LE} {
		b.WriteString(fmt.Sprintf(`
TEXT ·wrongD%d(SB), NOSPLIT, $0-24
	FMOVD	x+0(FP), F0
	FMOVD	y+8(FP), F1
	FCMPD	F1, F0
	CSET	%v, R0
	MOVD	R0, ret+16(FP)
	RET
`, i, c))
	}
	return b.String()
}

// probeMain builds the driver. It compares each stub against Go's own operator
// on the same pair, so the expectation is the language and not a restatement
// of the lowering.
func probeMain() string {
	var b strings.Builder
	b.WriteString(`// Generated by ssa/rules/float_test.go.

package main

import (
	"fmt"
	"math"
)

`)
	for i := range probeOps {
		fmt.Fprintf(&b, "func cmpD%d(x, y float64) uint64\n", i)
		fmt.Fprintf(&b, "func cmpS%d(x, y float32) uint64\n", i)
		fmt.Fprintf(&b, "func zeroD%d(x float64) uint64\n", i)
	}
	b.WriteString("func wrongD0(x, y float64) uint64\nfunc wrongD1(x, y float64) uint64\n")
	b.WriteString(`
func vals() []float64 {
	return []float64{
		math.NaN(), 0, math.Copysign(0, -1),
		math.Inf(1), math.Inf(-1),
		-1.5, 1.5, 3, -3, math.MaxFloat64, -math.MaxFloat64,
	}
}

func main() {
	n := 0
	for _, x := range vals() {
		for _, y := range vals() {
			f := float32(x)
			g := float32(y)
`)
	for i, o := range probeOps {
		fmt.Fprintf(&b, "\t\t\tcheck(%q, x, y, cmpD%d(x, y) != 0, x %s y, &n)\n", o.name, i, o.name)
		fmt.Fprintf(&b, "\t\t\tcheck(%q, float64(f), float64(g), cmpS%d(f, g) != 0, f %s g, &n)\n", o.name+" (single)", i, o.name)
		fmt.Fprintf(&b, "\t\t\tcheck(%q, 0, y, zeroD%d(y) != 0, 0 %s y, &n)\n", o.name+" (zero folded)", i, o.name)
	}
	b.WriteString(`		}
	}
	// The two conditions the integer rules use have to disagree with Go on a
	// NaN. If they did not, the checks above would pass for a lowering that
	// never told the two apart.
	nan := math.NaN()
	if wrongD0(nan, nan) == 0 || wrongD1(nan, nan) == 0 {
		fmt.Println("LT or LE is false for an unordered compare, so the probe proves nothing")
	}
	fmt.Printf("checked %d comparisons on the machine\n", n)
}

func check(what string, x, y float64, got, want bool, n *int) {
	*n++
	if got != want {
		fmt.Printf("%v %s %v gave %v, Go says %v\n", x, what, y, got, want)
	}
}
`)
	return b.String()
}

// TestARM64FloatRefusals walks the shapes group 6 must refuse.
//
// A refusal is a crash naming the operation, which specs/025 requires of a
// missing rule: a rule that guessed would produce a function that computes
// something plausible and reports nothing. A complex is the main case, because
// it is two floating-point registers and not one.
func TestARM64FloatRefusals(t *testing.T) {
	tC128 := &ir.Type{Kind: ir.Complex128, Size: 16, Align: 8, Name: "complex128"}
	tC64 := &ir.Type{Kind: ir.Complex64, Size: 8, Align: 4, Name: "complex64"}
	cases := []struct {
		name string
		fn   func() *ssa.Func
		op   string
	}{
		{"a complex add", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAdd, tC128, p.arg(tC128), p.arg(tC128)))
		}, "Add"},
		{"a complex subtract", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpSub, tC128, p.arg(tC128), p.arg(tC128)))
		}, "Sub"},
		{"a complex multiply", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpMul, tC128, p.arg(tC128), p.arg(tC128)))
		}, "Mul"},
		{"a complex divide", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpDiv, tC128, p.arg(tC128), p.arg(tC128)))
		}, "Div"},
		{"a complex negation", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpNeg, tC128, p.arg(tC128)))
		}, "Neg"},
		{"a complex constant", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.fkonst(tC128, 1))
		}, "ConstFloat"},
		{"a bit conversion from a complex", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpBitcast, tU64, p.arg(tC64)))
		}, "Bitcast"},
		{"a bit conversion of two different widths", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpBitcast, tU32, p.arg(tF64)))
		}, "Bitcast"},
		{"a float store whose width is not the value's", func() *ssa.Func {
			p := newBuilder()
			s := aux(p.val(ssa.OpStore, ssa.MemType, p.arg(tPtr), p.arg(tF64), p.mem), 4, nil)
			p.setMem(s)
			return p.ret()
		}, "Store"},
		{"a constant the front end could not parse", func() *ssa.Func {
			p := newBuilder()
			c := p.b.NewValue(0, ssa.OpConstFloat, tF64)
			c.Aux = "1e999999"
			return p.ret(c)
		}, "ConstFloat"},
		{"an integer conversion to a complex", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtIntToFloat, tC128, p.arg(tI64)))
		}, "CvtIntToFloat"},
		{"a conversion from a float to a float, spelled as an integer one", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtIntToFloat, tF64, p.arg(tF64)))
		}, "CvtIntToFloat"},
		{"a conversion from an integer, spelled as a float one", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtFloatToInt, tI64, p.arg(tI64)))
		}, "CvtFloatToInt"},
		{"a conversion to a complex", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtFloatToInt, tC128, p.arg(tF64)))
		}, "CvtFloatToInt"},
		{"a comparison of two floats of different widths", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tF64), p.arg(tF32)))
		}, "Less"},
		{"a comparison of a float against an integer", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tF64), p.arg(tI64)))
		}, "Less"},
		{"a precision change that is not between two floats", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCvtFloatToFloat, tF64, p.arg(tI64)))
		}, "CvtFloatToFloat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := lowerPanic(t, c.fn())
			if msg == "" {
				t.Fatalf("lowering %s did not fail, so the rule guessed at it", c.name)
			}
			if !strings.Contains(msg, c.op) {
				t.Errorf("the refusal does not name %s: %s", c.op, msg)
			}
		})
	}
}

// TestARM64ComplexReachesTheFloatRules records what the refusals above do not
// cover, and why.
//
// A complex load, store and comparison never reach group 6 whole. The
// decomposition pass of specs/025 splits each of them into its real and
// imaginary parts first, and the parts are float64 values that the rules
// lower one at a time. So the refusals above are for the operations
// decomposition leaves alone, which is the arithmetic.
func TestARM64ComplexReachesTheFloatRules(t *testing.T) {
	tC128 := &ir.Type{Kind: ir.Complex128, Size: 16, Align: 8, Name: "complex128"}
	runRules(t, []ruleTest{
		{"a complex load becomes two double loads", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLoad, tC128, p.arg(tPtr), p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64FMOVDload <float64> [0] t1 t0
  t3 = ARM64FMOVDload <float64> [8] t1 t0
  t4 = ARM64RET <mem> t2 t3 t0
  Ret t4
`},
		{"a complex comparison becomes two and an AND", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpEq, tBool, p.arg(tC128), p.arg(tC128)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <float64>
  t2 = Arg <float64>
  t3 = Arg <float64>
  t4 = Arg <float64>
  t5 = ARM64FCMPD <flags> t1 t3
  t6 = ARM64CSET <bool> {EQ} t5
  t7 = ARM64FCMPD <flags> t2 t4
  t8 = ARM64CSET <bool> {EQ} t7
  t9 = ARM64AND <bool> t6 t8
  t10 = ARM64RET <mem> t9 t0
  Ret t10
`},
	})
}

// TestARM64FloatMemOpTables covers the two type-to-operation lookups, at the
// widths that have an instruction and at one that does not.
func TestARM64FloatMemOpTables(t *testing.T) {
	tF16 := &ir.Type{Kind: ir.Float32, Size: 2, Align: 2, Name: "half"}
	for _, c := range []struct {
		t          *ir.Type
		load, stor ssa.Op
		ok         bool
	}{
		{tF32, ssa.OpARM64FMOVSload, ssa.OpARM64FMOVSstore, true},
		{tF64, ssa.OpARM64FMOVDload, ssa.OpARM64FMOVDstore, true},
		{tI64, ssa.OpARM64MOVDload, ssa.OpARM64MOVDstore, true},
		{tF16, ssa.OpInvalid, ssa.OpInvalid, false},
		{nil, ssa.OpInvalid, ssa.OpInvalid, false},
	} {
		name := "nil"
		if c.t != nil {
			name = c.t.Name
		}
		got, ok := ssa.ARM64LoadOp(c.t)
		if ok != c.ok || (c.ok && got != c.load) {
			t.Errorf("ARM64LoadOp(%s) = %v, %v", name, got, ok)
		}
		got, ok = ssa.ARM64StoreOpForType(c.t)
		if ok != c.ok || (c.ok && got != c.stor) {
			t.Errorf("ARM64StoreOpForType(%s) = %v, %v", name, got, ok)
		}
	}
	// Every floating-point load and store has to answer for its own address
	// mode, or a fold would silently produce an operation with no encoder.
	for _, op := range []ssa.Op{ssa.OpARM64FMOVDload, ssa.OpARM64FMOVSload,
		ssa.OpARM64FMOVDstore, ssa.OpARM64FMOVSstore} {
		m, ok := ssa.ARM64MemOp(op)
		if !ok || !m.IsFloat() {
			t.Errorf("%v is not a floating-point transfer", op)
		}
		if !ssa.ARM64MemFits(op, 0) {
			t.Errorf("%v cannot reach a zero offset", op)
		}
		for _, scaled := range []bool{false, true} {
			if _, ok := ssa.ARM64IndexOp(op, scaled); !ok {
				t.Errorf("%v has no register-offset form, scaled=%v", op, scaled)
			}
		}
	}
}
