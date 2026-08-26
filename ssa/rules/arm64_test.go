// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rules

import (
	"fmt"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj/arm64"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The types the tests build values with. Laid out by hand rather than by
// ir.Layout, so that a test names the size it means.
var (
	tI8   = &ir.Type{Kind: ir.Int8, Size: 1, Align: 1, Name: "int8"}
	tU8   = &ir.Type{Kind: ir.Uint8, Size: 1, Align: 1, Name: "uint8"}
	tI16  = &ir.Type{Kind: ir.Int16, Size: 2, Align: 2, Name: "int16"}
	tU16  = &ir.Type{Kind: ir.Uint16, Size: 2, Align: 2, Name: "uint16"}
	tI32  = &ir.Type{Kind: ir.Int32, Size: 4, Align: 4, Name: "int32"}
	tU32  = &ir.Type{Kind: ir.Uint32, Size: 4, Align: 4, Name: "uint32"}
	tI64  = &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int", PtrBits: []byte{0}}
	tU64  = &ir.Type{Kind: ir.Uint64, Size: 8, Align: 8, Name: "uint"}
	tBool = &ir.Type{Kind: ir.Bool, Size: 1, Align: 1, Name: "bool"}
	tF64  = &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}
	tPtr  = &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, Elem: tI64, Name: "*int"}
	tPtrB = &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, Elem: tU8, Name: "*byte"}
	tPtr3 = &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8,
		Elem: &ir.Type{Kind: ir.Struct, Size: 3, Align: 1, PtrBits: []byte{0}}, Name: "*s3"}
	tFn = &ir.Type{Kind: ir.FuncKind, Size: 8, Align: 8, Name: "func()"}
	// A pointer to a word that holds a pointer, and a pointer to a type whose
	// map was never computed. The clear picks a different runtime symbol for
	// each, and specs/031 says why: the wrong one leaves stale pointers
	// visible to the collector.
	tPtrP = &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, Name: "**int",
		Elem: &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, Elem: tI64, PtrBits: []byte{1}}}
	tPtrX = &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, Name: "*unknown",
		Elem: &ir.Type{Kind: ir.Struct, Size: 16, Align: 8}}
)

// builder assembles a small SSA function by hand.
//
// The IR builder is not the instrument here. A per-rule test names the
// operation and the argument shape the rule matches on, and building the graph
// directly is the shortest path from the rule to the assertion.
type builder struct {
	f   *ssa.Func
	b   *ssa.Block
	mem *ssa.Value
}

func newBuilder() *builder {
	f := ssa.NewFunc("f")
	b := f.Entry
	b.Kind = ssa.BlockRet
	p := &builder{f: f, b: b}
	p.mem = b.NewValue(0, ssa.OpInitMem, ssa.MemType)
	return p
}

func (p *builder) arg(t *ir.Type) *ssa.Value {
	return p.b.NewValue(0, ssa.OpArg, t)
}

func (p *builder) konst(t *ir.Type, c int64) *ssa.Value {
	v := p.b.NewValue(0, ssa.OpConstInt, t)
	v.AuxInt = c
	return v
}

func (p *builder) val(op ssa.Op, t *ir.Type, args ...*ssa.Value) *ssa.Value {
	return p.b.NewValue(0, op, t, args...)
}

// aux sets the auxiliary fields and returns the value, so that a test builds a
// value in one expression.
func aux(v *ssa.Value, auxInt int64, a any) *ssa.Value {
	v.AuxInt = auxInt
	v.Aux = a
	return v
}

// setMem records a value that produced memory, so the next one reads it.
func (p *builder) setMem(v *ssa.Value) *ssa.Value {
	p.mem = v
	return v
}

// ret closes the function with a return of vals.
func (p *builder) ret(vals ...*ssa.Value) *ssa.Func {
	args := append(append([]*ssa.Value{}, vals...), p.mem)
	p.b.Control = p.b.NewValue(0, ssa.OpMakeResult, ssa.MemType, args...)
	return p.f
}

var valueName = regexp.MustCompile(`\bv[0-9]+\b`)

// form returns the function in a shape a test can assert.
//
// Value identifiers are renumbered in printed order, so a rule that inserts a
// value does not renumber every assertion in the file. Everything else is the
// package's own dump, so a form in a test is read the same way as a dump in a
// diagnostic.
func form(f *ssa.Func) string {
	names := make(map[string]string)
	n := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			names[v.String()] = fmt.Sprintf("t%d", n)
			n++
		}
	}
	rename := func(s string) string {
		return valueName.ReplaceAllStringFunc(s, func(m string) string {
			if r, ok := names[m]; ok {
				return r
			}
			return m
		})
	}
	var out []string
	for _, b := range f.Blocks {
		head := b.String() + ":"
		if len(b.Preds) > 0 {
			var ps []string
			for _, p := range b.Preds {
				ps = append(ps, p.String())
			}
			head += " <- " + strings.Join(ps, " ")
		}
		out = append(out, head)
		for _, v := range b.Values {
			out = append(out, "  "+rename(v.LongString()))
		}
		tail := "  " + b.Kind.String()
		if b.Control != nil {
			tail += " " + rename(b.Control.String())
		}
		for i, s := range b.Succs {
			if i == 0 {
				tail += " ->"
			}
			tail += " " + s.String()
		}
		out = append(out, tail)
	}
	return strings.Join(out, "\n")
}

// lower runs the pass and every check that must hold after it.
//
// Verify is the checker of specs/021's invariants and still applies.
// CheckLowered is the one specs/025 adds. The encoder pass is the one
// specs/041 asks for: a rule that produced an immediate out of range is caught
// here rather than at the end of the compiler.
func lower(t *testing.T, f *ssa.Func) *ssa.Func {
	t.Helper()
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify before lowering: %v\n%s", vs, f)
	}
	ssa.Lower(f, ARM64)
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify after lowering: %v\n%s", vs, form(f))
	}
	if vs := ssa.CheckLowered(f, ARM64); len(vs) != 0 {
		t.Fatalf("target-neutral operations survived: %v\n%s", vs, form(f))
	}
	encodeAll(t, f)
	return f
}

// encodeAll asserts that every machine operation the pass produced reaches an
// encoder that accepts its operands.
func encodeAll(t *testing.T, f *ssa.Func) {
	t.Helper()
	out := make([]uint32, 8)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !ssa.IsARM64Op(v.Op) || ssa.ARM64MissingEncoder(v.Op) {
				continue
			}
			dst, regs := encodeRegs(v)
			if n, ok := ssa.ARM64Encode(v, dst, regs, out); !ok || n == 0 {
				t.Errorf("%s: the encoder rejected %s", f.Name, v.LongString())
			}
		}
	}
}

// encodeRegs picks a destination and argument registers of the classes a
// value's types call for.
//
// The class is not decoration here. An encoder is given a register from one
// file or the other and refuses the wrong one, so handing every operation an
// integer register would make every floating-point row fail for a reason that
// has nothing to do with the rule that produced it.
func encodeRegs(v *ssa.Value) (arm64.Reg, []arm64.Reg) {
	pick := func(t *ir.Type, n int) arm64.Reg {
		if c, ok := ssa.ClassOfType(t); ok && c == ssa.ClassFloat {
			return arm64.F0 + arm64.Reg(n)
		}
		return arm64.R0 + arm64.Reg(n)
	}
	args := make([]arm64.Reg, len(v.Args))
	for i, a := range v.Args {
		args[i] = pick(a.Type, i+1)
	}
	return pick(v.Type, 0), args
}

// runRule builds, lowers and compares against the expected form.
type ruleTest struct {
	name string
	fn   func() *ssa.Func
	want string
}

func runRules(t *testing.T, tests []ruleTest) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := lower(t, tc.fn())
			got := form(f)
			if got != strings.TrimSpace(tc.want) {
				t.Errorf("lowered form\ngot:\n%s\nwant:\n%s", got, strings.TrimSpace(tc.want))
			}
		})
	}
}

// TestARM64Arithmetic is group 1: the integer forms and the immediates.
func TestARM64Arithmetic(t *testing.T) {
	runRules(t, []ruleTest{
		{"add immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAdd, tI64, p.arg(tI64), p.konst(tI64, 7)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64ADDconst <int> [7] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"add immediate on the left", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAdd, tI64, p.konst(tI64, 7), p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64ADDconst <int> [7] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"add a negative immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAdd, tI64, p.arg(tI64), p.konst(tI64, -8)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64SUBconst <int> [8] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"add an immediate out of range", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAdd, tI64, p.arg(tI64), p.konst(tI64, 1<<20+1)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVDconst <int> [1048577]
  t3 = ARM64ADD <int> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"add a shifted immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAdd, tI64, p.arg(tI64), p.konst(tI64, 4096)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64ADDconst <int> [4096] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"subtract an immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpSub, tI64, p.arg(tI64), p.konst(tI64, 3)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64SUBconst <int> [3] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"subtract from zero", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpSub, tI64, p.konst(tI64, 0), p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64NEG <int> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"multiply by a power of two", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpMul, tI64, p.arg(tI64), p.konst(tI64, 8)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64LSLconst <int> [3] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"multiply by three", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpMul, tI64, p.arg(tI64), p.konst(tI64, 3)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVDconst <int> [3]
  t3 = ARM64MUL <int> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"and with a logical immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAnd, tI64, p.arg(tI64), p.konst(tI64, 0xff)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64ANDconst <int> [255] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"and with a value the bitmask cannot hold", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAnd, tI64, p.arg(tI64), p.konst(tI64, 5)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVDconst <int> [5]
  t3 = ARM64AND <int> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"and with zero", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAnd, tI64, p.arg(tI64), p.konst(tI64, 0)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVDconst <int> [0]
  t3 = ARM64AND <int> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"or with a logical immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpOr, tI64, p.arg(tI64), p.konst(tI64, 3)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64ORRconst <int> [3] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"exclusive or", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpXor, tI64, p.arg(tI64), p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = Arg <int>
  t3 = ARM64EOR <int> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"and not with an immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAndNot, tI64, p.arg(tI64), p.konst(tI64, 0xff)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64BICconst <int> [255] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"and not by an immediate on the left stays a register", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpAndNot, tI64, p.konst(tI64, 0xff), p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = ARM64MOVDconst <int> [255]
  t2 = Arg <int>
  t3 = ARM64BIC <int> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"negate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpNeg, tI64, p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64NEG <int> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"complement", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpCom, tI64, p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MVN <int> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"boolean not", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpNot, tBool, p.arg(tBool)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <bool>
  t2 = ARM64EORconst <bool> [1] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"divide by a constant needs no check", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpDiv, tI64, p.arg(tI64), p.konst(tI64, 3)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVDconst <int> [3]
  t3 = ARM64SDIV <int> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"unsigned divide", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpDiv, tU64, p.arg(tU64), p.konst(tU64, 3)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <uint>
  t2 = ARM64MOVDconst <uint> [3]
  t3 = ARM64UDIV <uint> t1 t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"remainder", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpMod, tI64, p.arg(tI64), p.konst(tI64, 3)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVDconst <int> [3]
  t3 = ARM64SDIV <int> t1 t2
  t4 = ARM64MSUB <int> t3 t2 t1
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"divide by a value is guarded", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpDiv, tI64, p.arg(tI64), p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = Arg <int>
  t3 = ARM64CMPconst <flags> [0] t2
  t4 = ARM64BRcond <flags> {NE} t3
  If t4 -> b1 b2
b1: <- b0
  t5 = ARM64SDIV <int> t1 t2
  t6 = ARM64RET <mem> t5 t0
  Ret t6
b2: <- b0
  t7 = ARM64CALLstatic <mem> {runtime.panicdivide} t0
  Exit t7
`},
	})
}

// TestARM64Shifts covers the disagreement between Go's shift and the
// architecture's.
func TestARM64Shifts(t *testing.T) {
	runRules(t, []ruleTest{
		{"shift left by a constant", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpShl, tI64, p.arg(tI64), p.konst(tU64, 3)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64LSLconst <int> [3] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"shift left by the width", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpShl, tI64, p.arg(tI64), p.konst(tU64, 64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVDconst <int> [0]
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"unsigned shift right by a constant", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpShr, tU64, p.arg(tU64), p.konst(tU64, 3)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <uint>
  t2 = ARM64LSRconst <uint> [3] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"signed shift right past the width", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpShr, tI64, p.arg(tI64), p.konst(tU64, 100)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64ASRconst <int> [63] t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"shift left by a value", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpShl, tI64, p.arg(tI64), p.arg(tU64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = Arg <uint>
  t3 = ARM64LSRconst <uint64> [6] t2
  t4 = ARM64NEG <int64> t3
  t5 = ARM64ASRconst <int64> [63] t4
  t6 = ARM64LSL <int> t1 t2
  t7 = ARM64BIC <int> t6 t5
  t8 = ARM64RET <mem> t7 t0
  Ret t8
`},
		{"signed shift right by a value", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpShr, tI64, p.arg(tI64), p.arg(tU64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = Arg <uint>
  t3 = ARM64LSRconst <uint64> [6] t2
  t4 = ARM64NEG <int64> t3
  t5 = ARM64ASRconst <int64> [63] t4
  t6 = ARM64ORR <int64> t2 t5
  t7 = ARM64ASR <int> t1 t6
  t8 = ARM64RET <mem> t7 t0
  Ret t8
`},
		{"32-bit shift left by a value", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpShl, tI32, p.arg(tI32), p.arg(tU64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int32>
  t2 = Arg <uint>
  t3 = ARM64LSRconst <uint64> [5] t2
  t4 = ARM64NEG <int64> t3
  t5 = ARM64ASRconst <int64> [63] t4
  t6 = ARM64LSL <int32> t1 t2
  t7 = ARM64BIC <int32> t6 t5
  t8 = ARM64RET <mem> t7 t0
  Ret t8
`},
	})
}

// TestARM64Compare covers the comparisons and the conditional set.
func TestARM64Compare(t *testing.T) {
	runRules(t, []ruleTest{
		{"signed less than", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tI64), p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = Arg <int>
  t3 = ARM64CMP <flags> t1 t2
  t4 = ARM64CSET <bool> {LT} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"unsigned less than", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tU64), p.arg(tU64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <uint>
  t2 = Arg <uint>
  t3 = ARM64CMP <flags> t1 t2
  t4 = ARM64CSET <bool> {LO} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"signed less or equal against an immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLeq, tBool, p.arg(tI64), p.konst(tI64, 10)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64CMPconst <flags> [10] t1
  t3 = ARM64CSET <bool> {LE} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"compare against a negative immediate", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLess, tBool, p.arg(tI64), p.konst(tI64, -5)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64CMNconst <flags> [5] t1
  t3 = ARM64CSET <bool> {LT} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"32-bit equality", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpEq, tBool, p.arg(tI32), p.arg(tI32)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int32>
  t2 = Arg <int32>
  t3 = ARM64CMPW <flags> t1 t2
  t4 = ARM64CSET <bool> {EQ} t3
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"not equal to zero", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpNeq, tBool, p.arg(tI64), p.konst(tI64, 0)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64CMPconst <flags> [0] t1
  t3 = ARM64CSET <bool> {NE} t2
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
	})
}

// TestARM64Conversions covers the width changes.
func TestARM64Conversions(t *testing.T) {
	runRules(t, []ruleTest{
		{"sign extend a byte", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpSignExt, tI64, p.arg(tI8)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int8>
  t2 = ARM64SXTB <int> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"zero extend a byte", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpZeroExt, tU64, p.arg(tU8)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <uint8>
  t2 = ARM64UXTB <uint> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"sign extend a halfword", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpSignExt, tI64, p.arg(tI16)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int16>
  t2 = ARM64SXTH <int> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"zero extend a halfword", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpZeroExt, tU64, p.arg(tU16)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <uint16>
  t2 = ARM64UXTH <uint> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"sign extend a word", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpSignExt, tI64, p.arg(tI32)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int32>
  t2 = ARM64SXTW <int> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"zero extend a word", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpZeroExt, tU64, p.arg(tU32)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <uint32>
  t2 = ARM64MOVWUreg <uint> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"truncate to a byte", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpTrunc, tU8, p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64UXTB <uint8> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"truncate to a word", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpTrunc, tI32, p.arg(tI64)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVWUreg <int32> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"reinterpret the bits", func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpBitcast, tU64, p.arg(tPtr)))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Copy <uint> t1
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
	})
}

// block adds a block and makes it the one values are added to.
func (p *builder) block(kind ssa.BlockKind) *ssa.Block {
	b := p.f.NewBlock(kind)
	p.b = b
	return b
}

// TestARM64Memory is group 2: a load and a store of every width.
func TestARM64Memory(t *testing.T) {
	load := func(t *ir.Type) func() *ssa.Func {
		return func() *ssa.Func {
			p := newBuilder()
			return p.ret(p.val(ssa.OpLoad, t, p.arg(tPtr), p.mem))
		}
	}
	store := func(t *ir.Type, size int64) func() *ssa.Func {
		return func() *ssa.Func {
			p := newBuilder()
			s := aux(p.val(ssa.OpStore, ssa.MemType, p.arg(tPtr), p.arg(t), p.mem), size, nil)
			p.setMem(s)
			return p.ret()
		}
	}
	runRules(t, []ruleTest{
		{"load a signed byte", load(tI8), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVBload <int8> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"load an unsigned byte", load(tU8), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVBUload <uint8> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"load a signed halfword", load(tI16), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVHload <int16> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"load an unsigned halfword", load(tU16), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVHUload <uint16> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"load a signed word", load(tI32), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVWload <int32> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"load an unsigned word", load(tU32), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVWUload <uint32> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"load a doubleword", load(tI64), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVDload <int> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"load a pointer", load(tPtr), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVDload <*int> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"load a bool", load(tBool), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVBUload <bool> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"store a byte", store(tI8, 1), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int8>
  t3 = ARM64MOVBstore <mem> [0] t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"store a halfword", store(tI16, 2), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int16>
  t3 = ARM64MOVHstore <mem> [0] t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"store a word", store(tI32, 4), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int32>
  t3 = ARM64MOVWstore <mem> [0] t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"store a doubleword", store(tI64, 8), `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int>
  t3 = ARM64MOVDstore <mem> [0] t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
	})
}

// TestARM64AddressModes is group 3, and it is the group that pays for the
// engine. Every case here is one instruction that a selector without rules
// would have emitted two or three for.
func TestARM64AddressModes(t *testing.T) {
	runRules(t, []ruleTest{
		{"a constant offset folds into the load", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpOffPtr, tPtr, p.arg(tPtr)), 16, nil)
			return p.ret(p.val(ssa.OpLoad, tI64, a, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVDload <int> [16] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a chain of offsets folds into one load", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpOffPtr, tPtr, p.arg(tPtr)), 16, nil)
			b := aux(p.val(ssa.OpOffPtr, tPtr, a), 8, nil)
			return p.ret(p.val(ssa.OpLoad, tI64, b, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVDload <int> [24] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a constant offset folds into the store", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpOffPtr, tPtr, p.arg(tPtr)), 24, nil)
			p.setMem(aux(p.val(ssa.OpStore, ssa.MemType, a, p.arg(tI64), p.mem), 8, nil))
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int>
  t3 = ARM64MOVDstore <mem> [24] t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"an offset the load cannot reach stays an add", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpOffPtr, tPtr, p.arg(tPtr)), 40960, nil)
			return p.ret(p.val(ssa.OpLoad, tI64, a, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64ADDconst <*int> [40960] t1
  t3 = ARM64MOVDload <int> [0] t2 t0
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"an offset no add immediate can reach materialises", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpOffPtr, tPtr, p.arg(tPtr)), 100000, nil)
			return p.ret(p.val(ssa.OpLoad, tI64, a, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVDconst <int64> [100000]
  t3 = ARM64MOVDloadidx <int> t1 t2 t0
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a scaled index folds into the load", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpPtrIndex, tPtr, p.arg(tPtr), p.arg(tI64)), 8, nil)
			return p.ret(p.val(ssa.OpLoad, tI64, a, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int>
  t3 = ARM64MOVDloadidx8 <int> t1 t2 t0
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a byte index folds into the load unscaled", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpPtrIndex, tPtrB, p.arg(tPtrB), p.arg(tI64)), 1, nil)
			return p.ret(p.val(ssa.OpLoad, tU8, a, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*byte>
  t2 = Arg <int>
  t3 = ARM64MOVBUloadidx <uint8> t1 t2 t0
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a scaled index folds into the store", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpPtrIndex, tPtr, p.arg(tPtr), p.arg(tI64)), 8, nil)
			p.setMem(aux(p.val(ssa.OpStore, ssa.MemType, a, p.arg(tI64), p.mem), 8, nil))
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int>
  t3 = Arg <int>
  t4 = ARM64MOVDstoreidx8 <mem> t1 t2 t3 t0
  t5 = ARM64RET <mem> t4
  Ret t5
`},
		{"an element size that is not a power of two multiplies", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpPtrIndex, tPtr3, p.arg(tPtr3), p.arg(tI64)), 3, nil)
			return p.ret(p.val(ssa.OpLoad, tU8, a, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*s3>
  t2 = Arg <int>
  t3 = ARM64MOVDconst <int64> [3]
  t4 = ARM64MADD <*s3> t2 t3 t1
  t5 = ARM64MOVBUload <uint8> [0] t4 t0
  t6 = ARM64RET <mem> t5 t0
  Ret t6
`},
		{"an index with an offset keeps the add", func() *ssa.Func {
			p := newBuilder()
			a := aux(p.val(ssa.OpPtrIndex, tPtr, p.arg(tPtr), p.arg(tI64)), 8, nil)
			b := aux(p.val(ssa.OpOffPtr, tPtr, a), 8, nil)
			return p.ret(p.val(ssa.OpLoad, tI64, b, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int>
  t3 = ARM64ADDshiftLL <*int> [3] t1 t2
  t4 = ARM64MOVDload <int> [8] t3 t0
  t5 = ARM64RET <mem> t4 t0
  Ret t5
`},
		{"the address of a global", func() *ssa.Func {
			p := newBuilder()
			g := &ir.Object{Name: "pkg.G", Type: tI64, Class: ir.ClassGlobal}
			return p.ret(aux(p.val(ssa.OpAddr, tPtr), 0, g))
		}, `
b0:
  t0 = SB <unsafe.Pointer>
  t1 = InitMem <mem>
  t2 = ARM64MOVDaddr <*int> [0] {pkg.G} t0
  t3 = ARM64RET <mem> t2 t1
  Ret t3
`},
		{"the address of a frame slot", func() *ssa.Func {
			p := newBuilder()
			o := &ir.Object{Name: "x", Type: tI64, Class: ir.ClassLocal}
			return p.ret(aux(p.val(ssa.OpLocalAddr, tPtr, p.mem), 0, o))
		}, `
b0:
  t0 = SP <unsafe.Pointer>
  t1 = InitMem <mem>
  t2 = ARM64ADDframe <*int> [0] {x} t0
  t3 = ARM64RET <mem> t2 t1
  Ret t3
`},
		{"a load from a global", func() *ssa.Func {
			p := newBuilder()
			g := &ir.Object{Name: "pkg.G", Type: tI64, Class: ir.ClassGlobal}
			return p.ret(p.val(ssa.OpLoad, tI64, aux(p.val(ssa.OpAddr, tPtr), 0, g), p.mem))
		}, `
b0:
  t0 = SB <unsafe.Pointer>
  t1 = InitMem <mem>
  t2 = ARM64MOVDaddr <*int> [0] {pkg.G} t0
  t3 = ARM64MOVDload <int> [0] t2 t1
  t4 = ARM64RET <mem> t3 t1
  Ret t4
`},
	})
}

// TestARM64Branches is group 4.
func TestARM64Branches(t *testing.T) {
	runRules(t, []ruleTest{
		{"a comparison that only feeds a branch needs no register", func() *ssa.Func {
			p := newBuilder()
			x := p.arg(tI64)
			c := p.val(ssa.OpLess, tBool, x, p.konst(tI64, 3))
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
  t1 = Arg <int>
  t2 = ARM64CMPconst <flags> [3] t1
  t3 = ARM64BRcond <flags> {LT} t2
  If t3 -> b1 b2
b1: <- b0
  t4 = ARM64RET <mem> t1 t0
  Ret t4
b2: <- b0
  t5 = ARM64RET <mem> t1 t0
  Ret t5
`},
		{"a boolean in a register branches with no compare", func() *ssa.Func {
			p := newBuilder()
			c := p.arg(tBool)
			b0 := p.b
			b0.Kind = ssa.BlockIf
			b0.Control = c
			then := p.block(ssa.BlockRet)
			p.ret(c)
			els := p.block(ssa.BlockRet)
			p.ret(c)
			b0.AddEdgeTo(then)
			b0.AddEdgeTo(els)
			return p.f
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <bool>
  t2 = ARM64CBNZ <flags> t1
  If t2 -> b1 b2
b1: <- b0
  t3 = ARM64RET <mem> t1 t0
  Ret t3
b2: <- b0
  t4 = ARM64RET <mem> t1 t0
  Ret t4
`},
		{"flags do not cross a block boundary", func() *ssa.Func {
			p := newBuilder()
			x := p.arg(tI64)
			c := p.val(ssa.OpLess, tBool, x, p.konst(tI64, 3))
			b0 := p.b
			b0.Kind = ssa.BlockPlain
			mid := p.block(ssa.BlockIf)
			mid.Control = c
			then := p.block(ssa.BlockRet)
			p.ret(x)
			els := p.block(ssa.BlockRet)
			p.ret(x)
			b0.AddEdgeTo(mid)
			mid.AddEdgeTo(then)
			mid.AddEdgeTo(els)
			return p.f
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64CMPconst <flags> [3] t1
  t3 = ARM64CSET <bool> {LT} t2
  Plain -> b1
b1: <- b0
  t4 = ARM64CBNZ <flags> t3
  If t4 -> b2 b3
b2: <- b1
  t5 = ARM64RET <mem> t1 t0
  Ret t5
b3: <- b1
  t6 = ARM64RET <mem> t1 t0
  Ret t6
`},
		{"a phi survives", func() *ssa.Func {
			p := newBuilder()
			x := p.arg(tI64)
			b0 := p.b
			b0.Kind = ssa.BlockIf
			b0.Control = p.val(ssa.OpLess, tBool, x, p.konst(tI64, 3))
			left := p.block(ssa.BlockPlain)
			l := p.konst(tI64, 1)
			right := p.block(ssa.BlockPlain)
			r := p.konst(tI64, 2)
			join := p.block(ssa.BlockRet)
			phi := p.val(ssa.OpPhi, tI64, l, r)
			p.ret(p.val(ssa.OpAdd, tI64, phi, x))
			b0.AddEdgeTo(left)
			b0.AddEdgeTo(right)
			left.AddEdgeTo(join)
			right.AddEdgeTo(join)
			return p.f
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64CMPconst <flags> [3] t1
  t3 = ARM64BRcond <flags> {LT} t2
  If t3 -> b1 b2
b1: <- b0
  t4 = ARM64MOVDconst <int> [1]
  Plain -> b3
b2: <- b0
  t5 = ARM64MOVDconst <int> [2]
  Plain -> b3
b3: <- b1 b2
  t6 = Phi <int> t4 t5
  t7 = ARM64ADD <int> t6 t1
  t8 = ARM64RET <mem> t7 t0
  Ret t8
`},
	})
}

// TestARM64Calls is group 5, in all four shapes.
func TestARM64Calls(t *testing.T) {
	callee := &ir.Object{Name: "pkg.f", Type: tFn, Class: ir.ClassFunc}
	runRules(t, []ruleTest{
		{"a static call", func() *ssa.Func {
			p := newBuilder()
			c := aux(p.val(ssa.OpStaticCall, ssa.MemType, p.arg(tI64), p.mem), 0, callee)
			p.setMem(c)
			return p.ret(aux(p.val(ssa.OpSelectN, tI64, c), 0, nil))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64CALLstatic <mem> {pkg.f} t1 t0
  t3 = SelectN <int> [0] t2
  t4 = ARM64RET <mem> t3 t2
  Ret t4
`},
		{"a closure call", func() *ssa.Func {
			p := newBuilder()
			c := p.val(ssa.OpClosureCall, ssa.MemType, p.arg(tFn), p.arg(tI64), p.mem)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <func()>
  t2 = Arg <int>
  t3 = ARM64MOVDload <unsafe.Pointer> [0] t1 t0
  t4 = ARM64CALLclosure <mem> t3 t1 t2 t0
  t5 = ARM64RET <mem> t4
  Ret t5
`},
		{"an interface call", func() *ssa.Func {
			p := newBuilder()
			c := aux(p.val(ssa.OpInterCall, ssa.MemType, p.arg(tPtr), p.arg(tI64), p.mem), 24, nil)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <int>
  t3 = ARM64MOVDload <unsafe.Pointer> [24] t1 t0
  t4 = ARM64CALLinter <mem> t3 t1 t2 t0
  t5 = ARM64RET <mem> t4
  Ret t5
`},
		{"the closure register at the entry", func() *ssa.Func {
			p := newBuilder()
			ctx := p.val(ssa.OpGetClosurePtr, tPtr)
			return p.ret(p.val(ssa.OpLoad, tI64, ctx, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = ARM64LoweredGetClosurePtr <*int>
  t2 = ARM64MOVDload <int> [0] t1 t0
  t3 = ARM64RET <mem> t2 t0
  Ret t3
`},
		{"a deferred call", func() *ssa.Func {
			p := newBuilder()
			d := &ir.Object{Name: "runtime.deferproc", Type: tFn, Class: ir.ClassFunc}
			c := aux(p.val(ssa.OpStaticCall, ssa.MemType, p.arg(tFn), p.mem), 0, d)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <func()>
  t2 = ARM64CALLdefer <mem> {runtime.deferproc} t1 t0
  t3 = ARM64RET <mem> t2
  Ret t3
`},
	})
}

// TestARM64Checks covers the operations that become a branch and a call,
// which is where lowering puts a call into a block that had none.
func TestARM64Checks(t *testing.T) {
	runRules(t, []ruleTest{
		{"a nil check is a load that faults", func() *ssa.Func {
			p := newBuilder()
			ptr := p.val(ssa.OpNilCheck, tPtr, p.arg(tPtr), p.mem)
			return p.ret(p.val(ssa.OpLoad, tI64, ptr, p.mem))
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64LoweredNilCheck <*int> t1 t0
  t3 = ARM64MOVDload <int> [0] t1 t0
  t4 = ARM64RET <mem> t3 t0
  Ret t4
`},
		{"a bounds check branches to the runtime", func() *ssa.Func {
			p := newBuilder()
			c := p.val(ssa.OpBoundsCheck, ssa.MemType, p.arg(tI64), p.konst(tI64, 4), p.mem)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = ARM64MOVDconst <int> [4]
  t3 = ARM64CMPconst <flags> [4] t1
  t4 = ARM64BRcond <flags> {LO} t3
  If t4 -> b1 b2
b1: <- b0
  t5 = ARM64RET <mem> t0
  Ret t5
b2: <- b0
  t6 = ARM64CALLstatic <mem> {runtime.goPanicIndex} t1 t2 t0
  Exit t6
`},
		{"a narrow index is checked at the width of the length", func() *ssa.Func {
			p := newBuilder()
			c := p.val(ssa.OpBoundsCheck, ssa.MemType, p.arg(tI8), p.arg(tI64), p.mem)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int8>
  t2 = Arg <int>
  t3 = ARM64CMP <flags> t1 t2
  t4 = ARM64BRcond <flags> {LO} t3
  If t4 -> b1 b2
b1: <- b0
  t5 = ARM64RET <mem> t0
  Ret t5
b2: <- b0
  t6 = ARM64CALLstatic <mem> {runtime.goPanicIndex} t1 t2 t0
  Exit t6
`},
		{"a slice bounds check allows the length", func() *ssa.Func {
			p := newBuilder()
			c := p.val(ssa.OpSliceBoundsCheck, ssa.MemType, p.arg(tI64), p.arg(tI64), p.mem)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <int>
  t2 = Arg <int>
  t3 = ARM64CMP <flags> t1 t2
  t4 = ARM64BRcond <flags> {LS} t3
  If t4 -> b1 b2
b1: <- b0
  t5 = ARM64RET <mem> t0
  Ret t5
b2: <- b0
  t6 = ARM64CALLstatic <mem> {runtime.goPanicSliceAlen} t1 t2 t0
  Exit t6
`},
		{"a copy is a call to memmove", func() *ssa.Func {
			p := newBuilder()
			c := aux(p.val(ssa.OpMove, ssa.MemType, p.arg(tPtr), p.arg(tPtr), p.mem), 48, nil)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = Arg <*int>
  t3 = ARM64MOVDconst <uint64> [48]
  t4 = ARM64CALLstatic <mem> {runtime.memmove} t1 t2 t3 t0
  t5 = ARM64RET <mem> t4
  Ret t5
`},
		{"a clear of pointer-free memory calls memclrNoHeapPointers", func() *ssa.Func {
			p := newBuilder()
			c := aux(p.val(ssa.OpZero, ssa.MemType, p.arg(tPtr3), p.mem), 3, nil)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*s3>
  t2 = ARM64MOVDconst <uint64> [3]
  t3 = ARM64CALLstatic <mem> {runtime.memclrNoHeapPointers} t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"a clear of a word with no pointer calls memclrNoHeapPointers", func() *ssa.Func {
			p := newBuilder()
			c := aux(p.val(ssa.OpZero, ssa.MemType, p.arg(tPtr), p.mem), 8, nil)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*int>
  t2 = ARM64MOVDconst <uint64> [8]
  t3 = ARM64CALLstatic <mem> {runtime.memclrNoHeapPointers} t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"a clear of memory that holds pointers calls memclrHasPointers", func() *ssa.Func {
			p := newBuilder()
			c := aux(p.val(ssa.OpZero, ssa.MemType, p.arg(tPtrP), p.mem), 8, nil)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <**int>
  t2 = ARM64MOVDconst <uint64> [8]
  t3 = ARM64CALLstatic <mem> {runtime.memclrHasPointers} t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
		{"a clear of memory with no pointer map is conservative", func() *ssa.Func {
			p := newBuilder()
			c := aux(p.val(ssa.OpZero, ssa.MemType, p.arg(tPtrX), p.mem), 16, nil)
			p.setMem(c)
			return p.ret()
		}, `
b0:
  t0 = InitMem <mem>
  t1 = Arg <*unknown>
  t2 = ARM64MOVDconst <uint64> [16]
  t3 = ARM64CALLstatic <mem> {runtime.memclrHasPointers} t1 t2 t0
  t4 = ARM64RET <mem> t3
  Ret t4
`},
	})
}

// TestARM64RuleCoverage is the rule-coverage report of specs/025.
//
// It prints which target-neutral operations have no arm64 rule, and it fails
// when one has neither a rule nor an entry in Deferred. The report is the
// check that catches a missing rule before a program does.
func TestARM64RuleCoverage(t *testing.T) {
	var covered, pseudo int
	var missing, deferred []string
	for op := ssa.Op(1); int(op) < ssa.NumNeutralOps; op++ {
		switch {
		case ssa.IsPseudoOp(op):
			pseudo++
		case ARM64.Rule(op) != nil:
			covered++
		case deferredReason(op) != "":
			deferred = append(deferred, op.String()+": "+deferredReason(op))
		default:
			missing = append(missing, op.String())
		}
	}
	t.Logf("arm64 rule coverage: %d operations with a rule, %d pseudo-operations that need none, %d deferred, %d missing",
		covered, pseudo, len(deferred), len(missing))
	for _, d := range deferred {
		t.Logf("deferred %s", d)
	}
	if len(missing) > 0 {
		t.Errorf("target-neutral operations with no arm64 rule and no reason: %s", strings.Join(missing, ", "))
	}
	// A deferred operation must not also have a rule. Two answers to one
	// question is how a list like this rots.
	for _, d := range Deferred {
		if ARM64.Rule(d.Op) != nil {
			t.Errorf("%v is deferred and has a rule", d.Op)
		}
	}
}

func deferredReason(op ssa.Op) string {
	for _, d := range Deferred {
		if d.Op == op {
			return d.Reason
		}
	}
	return ""
}

// TestARM64Determinism asserts that two lowerings of one function are the same
// bytes, which specs/053-determinism.md requires of every pass.
func TestARM64Determinism(t *testing.T) {
	mk := func() *ssa.Func {
		p := newBuilder()
		a := aux(p.val(ssa.OpPtrIndex, tPtr, p.arg(tPtr), p.arg(tI64)), 8, nil)
		l := p.val(ssa.OpLoad, tI64, a, p.mem)
		c := p.val(ssa.OpLess, tBool, l, p.konst(tI64, 3))
		return p.ret(l, c)
	}
	first := form(lower(t, mk()))
	for i := 0; i < 4; i++ {
		if got := form(lower(t, mk())); got != first {
			t.Fatalf("lowering %d differs\ngot:\n%s\nwant:\n%s", i, got, first)
		}
	}
}

// ---------------------------------------------------------------------------
// End to end from Go source
//
// The pipeline is the real one: the parser of specs/010, the checker of
// specs/012, the IR of specs/020, the construction of specs/021, and this
// pass. The bar is deliberately low and wide, as it is for the corpus tests of
// the passes below this one: a table test says the rules are right about what
// they were asked, and this says nothing falls over on Go that nobody wrote
// for a compiler under construction.

type corpusPkg struct {
	pkg   *types2.Package
	files []*syntax.File
	info  *types2.Info
	err   error
}

type corpusImporter struct {
	fset *syntax.FileSet
	done map[string]*corpusPkg
}

func newCorpusImporter() *corpusImporter {
	return &corpusImporter{fset: syntax.NewFileSet(), done: make(map[string]*corpusPkg)}
}

func (imp *corpusImporter) Import(path string) (*types2.Package, error) {
	r := imp.check(path)
	return r.pkg, r.err
}

func (imp *corpusImporter) check(path string) *corpusPkg {
	if have, ok := imp.done[path]; ok {
		if have == nil {
			return &corpusPkg{err: fmt.Errorf("import cycle at %s", path)}
		}
		return have
	}
	if path == "unsafe" {
		r := &corpusPkg{pkg: types2.Unsafe}
		imp.done[path] = r
		return r
	}
	imp.done[path] = nil
	r := &corpusPkg{}
	imp.done[path] = r

	bp, err := build.Import(path, "", 0)
	if err != nil {
		for _, prefix := range []string{"vendor/", "cmd/vendor/"} {
			if bp2, err2 := build.Import(prefix+path, "", 0); err2 == nil {
				bp, err = bp2, nil
				break
			}
		}
	}
	if err != nil {
		r.err = err
		return r
	}
	if len(bp.CgoFiles) > 0 || len(bp.GoFiles) == 0 {
		r.err = fmt.Errorf("%s has no plain Go files", path)
		return r
	}
	for _, name := range bp.GoFiles {
		f, err := syntax.ParseFile(imp.fset, filepath.Join(bp.Dir, name), nil, nil, 0)
		if err != nil || f == nil {
			r.err = fmt.Errorf("parse %s: %v", name, err)
			return r
		}
		r.files = append(r.files, f)
	}
	r.info = &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{Fset: imp.fset, Importer: imp, Sizes: types2.SizesFor("gc", "arm64")}
	r.pkg, r.err = conf.Check(path, r.files, r.info)
	return r
}

// corpusCounts is what one run of the corpus produced.
type corpusCounts struct {
	pkgs     int
	funcs    int // functions that reached ssa.Build
	built    int // functions ssa.Build produced
	lowered  int // functions that lowered completely
	refused  map[string]int
	verifyNG int
}

// TestARM64Corpus lowers every function of the standard library that reaches
// SSA, and asserts that nothing panics outside the rules' own refusals and
// that everything that lowered still verifies.
func TestARM64Corpus(t *testing.T) {
	required := os.Getenv("NANOGO_REQUIRE_CORPUS") == "1"
	src := filepath.Join(runtime.GOROOT(), "src")
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		if required {
			t.Fatalf("NANOGO_REQUIRE_CORPUS=1 and %s is not there", src)
		}
		t.Skipf("no corpus at %s", src)
	}

	var paths []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != src && (name == "testdata" || name == "vendor" ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(paths)
	if !required {
		// The unattended run takes a sample and leaves the tool chain out, as
		// the corpus test of specs/020 does. CI sets NANOGO_REQUIRE_CORPUS and
		// gets all of it.
		var library []string
		for _, p := range paths {
			if !strings.HasPrefix(p, "cmd/") && p != "cmd" && p != "unsafe" {
				library = append(library, p)
			}
		}
		const sample = 40
		if len(library) > sample {
			step := len(library) / sample
			var taken []string
			for i := 0; i < len(library); i += step {
				taken = append(taken, library[i])
			}
			library = taken
		}
		paths = library
	}

	imp := newCorpusImporter()
	c := &corpusCounts{refused: make(map[string]int)}
	for _, path := range paths {
		if path == "unsafe" {
			continue
		}
		r := imp.check(path)
		if r.err != nil || r.pkg == nil {
			continue
		}
		pkg, _ := ir.Build(r.pkg, r.files, r.info)
		if pkg == nil {
			continue
		}
		c.pkgs++
		fns := append(append([]*ir.Func{}, pkg.Funcs...), pkg.Inits...)
		for _, fn := range fns {
			lowerOne(t, path, fn, c)
		}
	}

	t.Logf("arm64 corpus: %d packages, %d functions reached SSA, %d lowered completely",
		c.pkgs, c.built, c.lowered)
	// The refusals are the report specs/025 asks for, with counts rather than
	// names only, so that the size of what is missing is visible.
	var ops []string
	for op := range c.refused {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		t.Logf("unlowered %s in %d functions", op, c.refused[op])
	}
	if c.verifyNG > 0 {
		t.Errorf("%d functions did not verify after lowering", c.verifyNG)
	}
	if c.built == 0 {
		t.Fatal("the corpus produced no function")
	}
	if c.lowered*4 < c.built {
		t.Errorf("only %d of %d functions lowered; the rules collapsed", c.lowered, c.built)
	}
	if required && c.built < 1000 {
		t.Fatalf("only %d functions reached SSA; the corpus collapsed", c.built)
	}
}

// lowerOne builds and lowers one function, recovering the crash a missing rule
// is. The operation the crash names is counted, because that count is the
// honest measure of what the rule set does not cover yet.
func lowerOne(t *testing.T, path string, fn *ir.Func, c *corpusCounts) {
	t.Helper()
	c.funcs++
	f, err := ssa.Build(fn)
	if err != nil || f == nil {
		return
	}
	c.built++
	if vs := ssa.Verify(f); len(vs) != 0 {
		// Construction is not this pass's problem, and specs/021's own corpus
		// test owns it. Skipping keeps this test about lowering.
		return
	}
	ok := func() (ok bool) {
		defer func() {
			e := recover()
			if e == nil {
				return
			}
			le, isLower := e.(*ssa.LowerError)
			if !isLower {
				t.Fatalf("%s: %s: lowering panicked: %v\n%s", path, fn.Name, e, debug.Stack())
			}
			c.refused[le.Op.String()]++
			ok = false
		}()
		ssa.Lower(f, ARM64)
		return true
	}()
	if !ok {
		return
	}
	if vs := ssa.Verify(f); len(vs) != 0 {
		c.verifyNG++
		if c.verifyNG < 4 {
			t.Errorf("%s: %s did not verify after lowering: %v", path, fn.Name, vs)
		}
		return
	}
	if vs := ssa.CheckLowered(f, ARM64); len(vs) != 0 {
		t.Errorf("%s: %s kept target-neutral operations: %v", path, fn.Name, vs)
		return
	}
	encodeAll(t, f)
	c.lowered++
}

// TestARM64ImmediateSweep is the range check specs/041 asks for, from the
// rules' side.
//
// A rule that produces an immediate the encoder cannot hold is a compiler bug,
// and the encoder is where it would otherwise be found: at the end of the
// compiler, with no way back to the rule that made it. This sweeps the values
// that sit on every boundary the arm64 immediate forms have and asserts that
// the form each rule chose encodes.
func TestARM64ImmediateSweep(t *testing.T) {
	consts := []int64{
		0, 1, 2, 3, 5, 7, 8, 255, 256, 4094, 4095, 4096, 4097, 8191, 8192,
		1<<12 - 1, 1 << 12, 1<<12 + 1, 1 << 20, 1<<24 - 1, 1 << 24,
		32760, 32768, 40960, 100000, 1 << 40,
		-1, -2, -3, -8, -255, -4095, -4096, -4097, -32768, -100000,
		0x5555555555555555, -0x5555555555555556,
		0x00ff00ff00ff00ff, 0x7fffffffffffffff, -0x8000000000000000,
	}
	binary := []ssa.Op{ssa.OpAdd, ssa.OpSub, ssa.OpMul, ssa.OpAnd, ssa.OpOr,
		ssa.OpXor, ssa.OpAndNot, ssa.OpLess, ssa.OpLeq, ssa.OpEq, ssa.OpNeq}
	for _, c := range consts {
		for _, op := range binary {
			for _, ty := range []*ir.Type{tI64, tU64, tI32, tU32} {
				p := newBuilder()
				res := ty
				if op == ssa.OpLess || op == ssa.OpLeq || op == ssa.OpEq || op == ssa.OpNeq {
					res = tBool
				}
				f := p.ret(p.val(op, res, p.arg(ty), p.konst(ty, c)))
				name := fmt.Sprintf("%v/%v/%d", op, ty.Name, c)
				t.Run(name, func(t *testing.T) { lower(t, f) })
			}
		}
		// The load and store offsets, which have their own two ranges.
		for _, ty := range []*ir.Type{tI8, tU16, tI32, tI64} {
			p := newBuilder()
			a := aux(p.val(ssa.OpOffPtr, tPtr, p.arg(tPtr)), c, nil)
			f := p.ret(p.val(ssa.OpLoad, ty, a, p.mem))
			t.Run(fmt.Sprintf("load/%v/%d", ty.Name, c), func(t *testing.T) { lower(t, f) })

			q := newBuilder()
			b := aux(q.val(ssa.OpOffPtr, tPtr, q.arg(tPtr)), c, nil)
			q.setMem(aux(q.val(ssa.OpStore, ssa.MemType, b, q.arg(ty), q.mem), ty.Size, nil))
			g := q.ret()
			t.Run(fmt.Sprintf("store/%v/%d", ty.Name, c), func(t *testing.T) { lower(t, g) })
		}
		// The shift amounts, which are rejected at the operand width rather
		// than truncated.
		for _, op := range []ssa.Op{ssa.OpShl, ssa.OpShr} {
			for _, ty := range []*ir.Type{tI64, tU64, tI32, tU32} {
				if c < 0 {
					// A negative shift count is a run-time panic in Go and no
					// rule may encode one.
					continue
				}
				p := newBuilder()
				f := p.ret(p.val(op, ty, p.arg(ty), p.konst(tU64, c)))
				t.Run(fmt.Sprintf("%v/%v/%d", op, ty.Name, c), func(t *testing.T) { lower(t, f) })
			}
		}
	}
}

// TestCompareRefusesWideOperands is a regression test for a silent miscompile.
//
// ARM64Size answers Size64 for any type wider than four bytes, so a comparison
// rule that only refused floats emitted a 64-bit CMP for a 16-byte string. The
// result runs, compares the two string headers' first words, and reports
// nothing. Two strings with the same data pointer and different lengths would
// have compared equal.
//
// String and interface equality are not per-part comparisons: a string
// compares its bytes and an interface calls the dynamic type's equality
// function. specs/020-ir.md's lowering table makes both runtime calls, and
// decomposition builds both out of the parts it has, so neither reaches this
// rule whole any more. What still reaches it is every aggregate above the
// decomposition bound, and the rule must refuse rather than guess: a 40-byte
// struct compared with one 64-bit CMP is code that runs, answers on its first
// word, and reports nothing.
func TestCompareRefusesWideOperands(t *testing.T) {
	big := &ir.Type{Kind: ir.Struct, Name: "big", Fields: []ir.Field{
		{Name: "A", Type: tI64}, {Name: "B", Type: tI64},
		{Name: "C", Type: tI64}, {Name: "D", Type: tI64},
		{Name: "E", Type: tI64},
	}}
	if err := ir.Layout(big); err != nil {
		t.Fatal(err)
	}
	for _, ty := range []*ir.Type{big} {
		p := newBuilder()
		cmp := p.val(ssa.OpEq, tBool, p.arg(ty), p.arg(ty))
		f := p.ret(cmp)

		// The refusal is a crash naming the operation, which is what
		// specs/025-lowering-and-rules.md requires of a missing rule: a silent
		// fallback produces a function that is missing an operation, and that
		// is the hardest bug in the compiler to find. Asserting the crash is
		// asserting the contract.
		msg := lowerPanic(t, f)
		if msg == "" {
			t.Errorf("%s: lowering a wide comparison did not fail; it was guessed at, and %v is what it became",
				ty.Name, cmp.Op)
			continue
		}
		if !strings.Contains(msg, "Eq") {
			t.Errorf("%s: the refusal does not name the operation: %s", ty.Name, msg)
		}
	}
}

// lowerPanic runs the pass and returns the panic message, or "" if it did not
// panic.
func lowerPanic(t *testing.T, f *ssa.Func) (msg string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	ssa.Lower(f, ARM64)
	return ""
}

// TestCompareAcceptsRegisterWidthOperands is the other half. A guard that is
// too broad refuses every comparison in the program, which no corpus test
// would notice because such a function simply fails to lower.
func TestCompareAcceptsRegisterWidthOperands(t *testing.T) {
	for _, ty := range []*ir.Type{tI64, tI32} {
		p := newBuilder()
		cmp := p.val(ssa.OpEq, tBool, p.arg(ty), p.arg(ty))
		f := lower(t, p.ret(cmp))
		_ = f
		if cmp.Op == ssa.OpEq {
			t.Errorf("%s: a comparison that fits one register was refused", ty)
		}
	}
}
