// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj/arm64"
)

// machValue builds a value of a machine operation with plausible operands, so
// that every row of the table can be handed to the encoder.
// The floating-point types the group 6 rows are built with. A conversion
// stands between two widths and reads both from its types, so the table walk
// below needs a float32 as well as a float64 to reach every instruction.
var (
	lowF64 = &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}
	lowF32 = &ir.Type{Kind: ir.Float32, Size: 4, Align: 4, Name: "float32"}
)

// machTypes returns the result type and the argument types a row of the table
// is exercised with.
//
// The types are not decoration. ARM64Encode reads the operand width from them
// and the register class follows from them too, so a floating-point operation
// built with integer types would be handed an integer register and the
// encoder would refuse it, correctly and for the wrong reason.
func machTypes(op Op, info opInfo, n int) (res *ir.Type, args []*ir.Type) {
	res = lowI64
	if info.makesMem {
		res = MemType
	}
	switch op {
	case OpARM64CMP, OpARM64CMPW, OpARM64CMPconst, OpARM64CMPWconst,
		OpARM64CMN, OpARM64CMNW, OpARM64CMNconst, OpARM64CMNWconst,
		OpARM64TST, OpARM64TSTW, OpARM64TSTconst, OpARM64TSTWconst,
		OpARM64BRcond, OpARM64CBZ, OpARM64CBNZ,
		OpARM64FCMPD, OpARM64FCMPS, OpARM64FCMPD0, OpARM64FCMPS0:
		res = FlagsType
	}
	args = make([]*ir.Type, n)
	for i := range args {
		args[i] = lowI64
	}
	if info.takesMem && n > 0 {
		args[n-1] = MemType
	}

	// The floating-point rows, by which operand lives in which file.
	float := func(t *ir.Type, which ...int) {
		for _, i := range which {
			if i < len(args) {
				args[i] = t
			}
		}
	}
	switch op {
	case OpARM64FADD, OpARM64FSUB, OpARM64FMUL, OpARM64FDIV:
		res = lowF64
		float(lowF64, 0, 1)
	case OpARM64FNEG, OpARM64FABS, OpARM64FSQRT:
		res = lowF64
		float(lowF64, 0)
	case OpARM64FCMPD, OpARM64FCMPD0:
		float(lowF64, 0, 1)
	case OpARM64FCMPS, OpARM64FCMPS0:
		float(lowF32, 0, 1)
	case OpARM64FMOVconst:
		res = lowF64
	case OpARM64FMOVgpfp:
		res = lowF64
	case OpARM64FMOVfpgp:
		float(lowF64, 0)
	case OpARM64SCVTF, OpARM64UCVTF:
		res = lowF64
	case OpARM64FCVTZS, OpARM64FCVTZU:
		float(lowF64, 0)
	case OpARM64FCVT:
		// The two widths must differ: the class has no conversion from a
		// width to itself.
		res = lowF64
		float(lowF32, 0)
	case OpARM64FMOVDload:
		res = lowF64
	case OpARM64FMOVSload:
		res = lowF32
	case OpARM64FMOVDstore:
		float(lowF64, 1)
	case OpARM64FMOVSstore:
		float(lowF32, 1)
	case OpARM64FMOVDloadidx, OpARM64FMOVDloadidx8:
		res = lowF64
	case OpARM64FMOVSloadidx, OpARM64FMOVSloadidx4:
		res = lowF32
	case OpARM64FMOVDstoreidx, OpARM64FMOVDstoreidx8:
		float(lowF64, 2)
	case OpARM64FMOVSstoreidx, OpARM64FMOVSstoreidx4:
		float(lowF32, 2)
	}
	return res, args
}

// machRegs returns a destination and argument registers of the classes a
// value's types call for, so that no encoder is handed a register from the
// wrong file.
func machRegs(v *Value) (arm64.Reg, []arm64.Reg) {
	pick := func(t *ir.Type, n int) arm64.Reg {
		if c, ok := ClassOfType(t); ok && c == ClassFloat {
			return arm64.F0 + arm64.Reg(n)
		}
		return arm64.R0 + arm64.Reg(n)
	}
	dst := pick(v.Type, 0)
	args := make([]arm64.Reg, len(v.Args))
	for i, a := range v.Args {
		args[i] = pick(a.Type, i+1)
	}
	return dst, args
}

func machValue(f *Func, op Op) *Value {
	info := infoOf(op)
	n := int(info.argLen)
	if n < 0 {
		n = 2
	}
	res, argTypes := machTypes(op, info, n)
	v := f.Entry.NewValue(0, op, res)
	for i := 0; i < n; i++ {
		v.AddArg(f.Entry.NewValue(0, OpArg, argTypes[i]))
	}
	switch op {
	case OpARM64BRcond, OpARM64CSET:
		// The condition is in Aux for both, and it is the same value for the
		// same comparison: one branches on the flags and the other reads them
		// into a register.
		v.Aux = arm64.LT
	case OpARM64TSTconst, OpARM64TSTWconst, OpARM64ANDconst, OpARM64ORRconst,
		OpARM64EORconst, OpARM64BICconst:
		// Zero is not a logical immediate, so the table walk uses a value the
		// bitmask scheme can hold.
		v.AuxInt = 0xff
	case OpARM64FMOVconst:
		// Zero is not one of the 256 values FMOV's immediate reaches, so the
		// table walk uses one that is.
		v.AuxInt = int64(arm64.FloatBits(arm64.Size64, 1.5))
	}
	return v
}

// TestARM64OpTable asserts every machine operation has a filled row.
//
// A missing row prints as an empty name and reads as an unknown operation in
// the verifier, which is a long way from the file that forgot it.
func TestARM64OpTable(t *testing.T) {
	seen := make(map[string]Op)
	for op := OpARM64ADD; op < opARM64End; op++ {
		row := arm64Row(op)
		if row.info.name == "" {
			t.Errorf("operation %d has no row", op-OpARM64ADD)
			continue
		}
		if !strings.HasPrefix(row.info.name, "ARM64") {
			t.Errorf("%v is not named for its target", op)
		}
		if have, ok := seen[row.info.name]; ok {
			t.Errorf("%v and %v have the same name", have, op)
		}
		seen[row.info.name] = op
		if infoOf(op).name != row.info.name {
			t.Errorf("%v is registered as %q", op, infoOf(op).name)
		}
		if row.info.argLen < -1 || row.info.argLen > 4 {
			t.Errorf("%v takes %d arguments", op, row.info.argLen)
		}
		if row.info.makesMem && !row.info.takesMem {
			t.Errorf("%v produces memory and does not take any", op)
		}
		// A constant depends on nothing, and specs/026 rematerialises one
		// rather than spilling it. An operation that is marked constant and
		// reads a register would be recomputed from a register that is no
		// longer live.
		if row.info.constant && row.info.argLen != 0 {
			t.Errorf("%v is constant and takes %d arguments", op, row.info.argLen)
		}
		if row.info.call && !(row.info.takesMem && row.info.makesMem) {
			t.Errorf("%v is a call and does not thread memory", op)
		}
		if row.info.commutative && row.info.argLen != 2 {
			t.Errorf("%v is commutative with %d arguments", op, row.info.argLen)
		}
		if !IsARM64Op(op) {
			t.Errorf("%v is not an arm64 operation", op)
		}
	}
	if !OpARM64MOVDconst.IsConstant() {
		t.Error("the machine constant is not marked constant, so specs/026 will spill it rather than rematerialise it")
	}
	if IsARM64Op(OpAdd) || IsARM64Op(OpSP) || IsARM64Op(opARM64End) {
		t.Error("IsARM64Op accepted an operation outside the set")
	}
	if arm64Row(OpAdd).info.name != "" {
		t.Error("arm64Row answered for a target-neutral operation")
	}
	// Op is a uint8. The set grows only when a rule is added, and this is the
	// assertion that says when the type has to grow with it.
	if int(opARM64End) > 255 {
		t.Fatalf("the operation set reached %d and Op is a uint8", opARM64End)
	}
	t.Logf("arm64 machine operations: %d, last operation number %d of 255",
		opARM64End-OpARM64ADD, opARM64End-1)
}

// TestARM64Encoders is the encoder agreement of specs/041: every operation a
// rule can emit reaches an encoder that accepts its operands.
func TestARM64Encoders(t *testing.T) {
	f := NewFunc("f")
	out := make([]uint32, 8)
	var gaps []string
	for op := OpARM64ADD; op < opARM64End; op++ {
		v := machValue(f, op)
		dst, regs := machRegs(v)
		n, ok := ARM64Encode(v, dst, regs, out)
		if ARM64MissingEncoder(op) {
			gaps = append(gaps, op.String())
			if ok {
				t.Errorf("%v is listed as having no encoder and encoded", op)
			}
			continue
		}
		if !ok || n == 0 {
			t.Errorf("%v did not encode", op)
		}
	}
	// The list is expected to be empty. It held ARM64CSET, the conditional
	// set of specs/042 group 1, until obj/arm64 got the conditional select
	// class; with that in, every operation the rules emit reaches an encoder,
	// which is the agreement specs/041 describes.
	//
	// The assertion is on the whole list and not on the absence of one name,
	// so a rule group that lands ahead of its encodings fails here and is not
	// discovered by a program that will not compile.
	var want []string
	if strings.Join(gaps, ",") != strings.Join(want, ",") {
		t.Errorf("operations with no encoder: %v, want %v", gaps, want)
	}
}

// TestARM64EncodeAgreesWithEncoder checks a sample against the encoder called
// directly, so that the switch cannot drift into calling the wrong function.
func TestARM64EncodeAgreesWithEncoder(t *testing.T) {
	f := NewFunc("f")
	out := make([]uint32, 8)
	regs := []arm64.Reg{arm64.R1, arm64.R2, arm64.R3}
	enc := func(op Op, auxInt int64, aux any, t2 *ir.Type) uint32 {
		v := machValue(f, op)
		v.AuxInt = auxInt
		if aux != nil {
			v.Aux = aux
		}
		if t2 != nil {
			v.Type = t2
		}
		n, ok := ARM64Encode(v, arm64.R0, regs, out)
		if !ok || n == 0 {
			t.Fatalf("%v did not encode", op)
		}
		return out[0]
	}
	sub, _ := arm64.SubRegImm(arm64.Size64, arm64.R0, arm64.R1, 24)
	lsl, _ := arm64.LslRegImm(arm64.Size64, arm64.R0, arm64.R1, 3)
	ldr, _ := arm64.MemUnsignedOffset(arm64.LoadX, arm64.R0, arm64.R1, 16)
	str, _ := arm64.MemUnsignedOffset(arm64.StoreX, arm64.R2, arm64.R1, 8)
	idx, _ := arm64.MemRegOffset(arm64.LoadX, arm64.R0, arm64.R1, arm64.R2, arm64.LSLX, true)
	bcc, _ := arm64.BCond(arm64.LT, 0)
	tests := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"ADD", enc(OpARM64ADD, 0, nil, nil), arm64.AddRegReg(arm64.Size64, arm64.R0, arm64.R1, arm64.R2)},
		{"ADD 32-bit", enc(OpARM64ADD, 0, nil, lowI32), arm64.AddRegReg(arm64.Size32, arm64.R0, arm64.R1, arm64.R2)},
		{"SUBconst", enc(OpARM64SUBconst, 24, nil, nil), sub},
		{"MUL", enc(OpARM64MUL, 0, nil, nil), arm64.MulRegReg(arm64.Size64, arm64.R0, arm64.R1, arm64.R2)},
		{"MSUB", enc(OpARM64MSUB, 0, nil, nil), arm64.Msub(arm64.Size64, arm64.R0, arm64.R1, arm64.R2, arm64.R3)},
		{"NEG", enc(OpARM64NEG, 0, nil, nil), arm64.NegReg(arm64.Size64, arm64.R0, arm64.R1)},
		{"MVN", enc(OpARM64MVN, 0, nil, nil), arm64.MvnReg(arm64.Size64, arm64.R0, arm64.R1)},
		{"LSLconst", enc(OpARM64LSLconst, 3, nil, nil), lsl},
		{"SXTB", enc(OpARM64SXTB, 0, nil, nil), arm64.SxtbReg(arm64.Size64, arm64.R0, arm64.R1)},
		{"SXTW", enc(OpARM64SXTW, 0, nil, nil), arm64.SxtwReg(arm64.R0, arm64.R1)},
		{"MOVWUreg", enc(OpARM64MOVWUreg, 0, nil, nil), arm64.MovRegReg(arm64.Size32, arm64.R0, arm64.R1)},
		{"CMP", enc(OpARM64CMP, 0, nil, nil), arm64.CmpRegReg(arm64.Size64, arm64.R1, arm64.R2)},
		{"CMPW", enc(OpARM64CMPW, 0, nil, nil), arm64.CmpRegReg(arm64.Size32, arm64.R1, arm64.R2)},
		{"TST", enc(OpARM64TST, 0, nil, nil), arm64.TstRegReg(arm64.Size64, arm64.R1, arm64.R2)},
		{"MOVDload", enc(OpARM64MOVDload, 16, nil, nil), ldr},
		{"MOVDstore", enc(OpARM64MOVDstore, 8, nil, nil), str},
		{"MOVDloadidx8", enc(OpARM64MOVDloadidx8, 0, nil, nil), idx},
		{"BRcond", enc(OpARM64BRcond, 0, arm64.LT, nil), bcc},
		// The conditional set carries the same condition as the branch above
		// it and reads the same flags, so the pair is here together. The width
		// comes from the result type, as it does for every other value: a bool
		// is narrower than a word and computes in the 32-bit form.
		{"CSET", enc(OpARM64CSET, 0, arm64.LT, nil),
			mustEnc(arm64.Cset(arm64.Size64, arm64.R0, arm64.LT))},
		{"CSET 32-bit", enc(OpARM64CSET, 0, arm64.LO, lowI32),
			mustEnc(arm64.Cset(arm64.Size32, arm64.R0, arm64.LO))},
		{"CBNZ", enc(OpARM64CBNZ, 0, nil, nil), mustEnc(arm64.Cbnz(arm64.Size64, arm64.R1, 0))},
		{"CALLclosure", enc(OpARM64CALLclosure, 0, nil, nil), arm64.Blr(arm64.R1)},
		{"RET", enc(OpARM64RET, 0, nil, nil), arm64.Ret(arm64.RegLink)},
		// The read of the context register names the register, and it is the
		// one row of the table where the source is not an operand. A move out
		// of any other register would compile and hand the body the wrong
		// closure.
		{"LoweredGetClosurePtr", enc(OpARM64LoweredGetClosurePtr, 0, nil, nil),
			arm64.MovRegReg(arm64.Size64, arm64.R0, arm64.RegClosure)},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s encoded as %#08x, the encoder says %#08x", tc.name, tc.got, tc.want)
		}
	}
}

// TestARM64ConditionNeedsAux checks that the two operations which carry a
// condition are refused when nothing put one there.
//
// Aux is an any, so a rule that forgets the condition would otherwise encode
// as the zero Cond, which is EQ. That is an instruction that assembles, runs,
// and tests the wrong flag, and no later stage could tell it from the one the
// rule meant.
func TestARM64ConditionNeedsAux(t *testing.T) {
	f := NewFunc("f")
	out := make([]uint32, 8)
	for _, op := range []Op{OpARM64CSET, OpARM64BRcond} {
		v := machValue(f, op)
		v.Aux = nil
		if _, ok := ARM64Encode(v, arm64.R0, []arm64.Reg{arm64.R1}, out); ok {
			t.Errorf("%v encoded with no condition in Aux", op)
		}
	}
}

var lowI32 = &ir.Type{Kind: ir.Int32, Size: 4, Align: 4, Name: "int32"}

func mustEnc(w uint32, ok bool) uint32 {
	if !ok {
		panic("ssa: the encoder rejected a test operand")
	}
	return w
}

// TestARM64ImmFitsTraps covers the two immediate traps specs/041 names.
//
// Both come out right only because the encoder is asked rather than a range
// restated here, which is the property the test exists to hold.
func TestARM64ImmFitsTraps(t *testing.T) {
	// BIC has no immediate form. It is AND of the complement, so the range
	// that matters is the complement's. 0xff is not a bitmask immediate for
	// AND is false here: 0xff is one, and its complement is one too. The
	// value that separates the two is one whose complement is not encodable.
	if !ARM64ImmFits(OpARM64BICconst, arm64.Size64, 0xff) {
		t.Error("BIC of 0xff does not fit, and the complement is a valid bitmask")
	}
	if ARM64ImmFits(OpARM64BICconst, arm64.Size64, 5) {
		t.Error("BIC of 5 fits, and neither 5 nor its complement is a bitmask")
	}
	// Zero and all-ones are not representable, whatever the width, because the
	// encoded run of ones can be neither empty nor fill the element.
	for _, sz := range []arm64.Size{arm64.Size32, arm64.Size64} {
		for _, op := range []Op{OpARM64ANDconst, OpARM64ORRconst, OpARM64EORconst, OpARM64TSTconst} {
			if ARM64ImmFits(op, sz, 0) {
				t.Errorf("%v accepted zero at %v", op, sz)
			}
			if ARM64ImmFits(op, sz, -1) {
				t.Errorf("%v accepted all ones at %v", op, sz)
			}
		}
	}
	// BIC of zero is AND of all ones, which is not representable either, and
	// BIC of all ones is AND of zero, which is not representable either.
	if ARM64ImmFits(OpARM64BICconst, arm64.Size64, 0) || ARM64ImmFits(OpARM64BICconst, arm64.Size64, -1) {
		t.Error("BIC accepted a value whose complement is not a bitmask")
	}
}

// TestARM64ImmFitsRanges walks the boundary of every immediate form.
func TestARM64ImmFitsRanges(t *testing.T) {
	tests := []struct {
		op   Op
		imm  int64
		want bool
	}{
		{OpARM64ADDconst, 0, true},
		{OpARM64ADDconst, arm64.MaxAddSubImm, true},
		// One past the unshifted field still fits when it is a multiple of
		// 4096, because bit 22 shifts the field by twelve. The first value
		// that does not fit is the one that is neither.
		{OpARM64ADDconst, arm64.MaxAddSubImm + 1, true},
		{OpARM64ADDconst, arm64.MaxAddSubImm + 2, false},
		{OpARM64ADDconst, 1 << 12, true},
		{OpARM64ADDconst, arm64.MaxAddSubImmShifted, true},
		{OpARM64ADDconst, arm64.MaxAddSubImmShifted + 1, false},
		{OpARM64ADDconst, -1, false},
		{OpARM64SUBconst, arm64.MaxAddSubImm, true},
		{OpARM64SUBconst, arm64.MaxAddSubImm + 2, false},
		{OpARM64CMPconst, arm64.MaxAddSubImm, true},
		{OpARM64CMPconst, arm64.MaxAddSubImm + 2, false},
		{OpARM64CMNconst, arm64.MaxAddSubImm, true},
		{OpARM64LSLconst, 0, true},
		{OpARM64LSLconst, 63, true},
		{OpARM64LSLconst, 64, false},
		{OpARM64LSLconst, -1, false},
		{OpARM64LSRconst, 63, true},
		{OpARM64LSRconst, 64, false},
		{OpARM64ASRconst, 63, true},
		{OpARM64ASRconst, 64, false},
		{OpARM64ADDshiftLL, 63, true},
		{OpARM64ADDshiftLL, 64, false},
		{OpARM64ADD, 0, false}, // not an immediate form at all
	}
	for _, tc := range tests {
		if got := ARM64ImmFits(tc.op, arm64.Size64, tc.imm); got != tc.want {
			t.Errorf("ARM64ImmFits(%v, 64, %d) = %v, want %v", tc.op, tc.imm, got, tc.want)
		}
	}
	// The 32-bit forms are narrower: a shift of 32 has no encoding.
	if ARM64ImmFits(OpARM64LSLconst, arm64.Size32, 32) {
		t.Error("a 32-bit shift by 32 fits")
	}
	if !ARM64ImmFits(OpARM64LSLconst, arm64.Size32, 31) {
		t.Error("a 32-bit shift by 31 does not fit")
	}
}

// TestARM64MemFits covers the two load and store offset ranges.
func TestARM64MemFits(t *testing.T) {
	tests := []struct {
		op   Op
		off  int64
		want bool
	}{
		// The unsigned form is scaled by the access size, so a doubleword
		// reaches eight times as far as a byte.
		{OpARM64MOVDload, 0, true},
		{OpARM64MOVDload, 8, true},
		{OpARM64MOVDload, 32760, true},
		{OpARM64MOVDload, 32768, false},
		{OpARM64MOVBUload, 4095, true},
		{OpARM64MOVBUload, 4096, false},
		// An offset that is not a multiple of the access size falls to the
		// unscaled form, which reaches 255 bytes and no further.
		{OpARM64MOVDload, 4, true},
		{OpARM64MOVDload, 260, false},
		// The unscaled form is the only one that reaches backwards.
		{OpARM64MOVDload, -8, true},
		{OpARM64MOVDload, -256, true},
		{OpARM64MOVDload, -257, false},
		{OpARM64MOVDstore, 32760, true},
		{OpARM64MOVDstore, 32768, false},
		{OpARM64ADD, 0, false},
	}
	for _, tc := range tests {
		if got := ARM64MemFits(tc.op, tc.off); got != tc.want {
			t.Errorf("ARM64MemFits(%v, %d) = %v, want %v", tc.op, tc.off, got, tc.want)
		}
	}
}

// TestARM64MemOpSelection covers the width and signedness table. A load that
// picks the wrong signedness is a value that compares equal to the right one
// half the time.
func TestARM64MemOpSelection(t *testing.T) {
	ty := func(k ir.Kind, size int64) *ir.Type {
		return &ir.Type{Kind: k, Size: size, Align: size}
	}
	loads := []struct {
		t    *ir.Type
		want Op
	}{
		{ty(ir.Int8, 1), OpARM64MOVBload},
		{ty(ir.Uint8, 1), OpARM64MOVBUload},
		{ty(ir.Bool, 1), OpARM64MOVBUload},
		{ty(ir.Int16, 2), OpARM64MOVHload},
		{ty(ir.Uint16, 2), OpARM64MOVHUload},
		{ty(ir.Int32, 4), OpARM64MOVWload},
		{ty(ir.Uint32, 4), OpARM64MOVWUload},
		{ty(ir.Int64, 8), OpARM64MOVDload},
		{ty(ir.Ptr, 8), OpARM64MOVDload},
	}
	for _, tc := range loads {
		got, ok := ARM64LoadOp(tc.t)
		if !ok || got != tc.want {
			t.Errorf("ARM64LoadOp(%v) = %v, want %v", tc.t.Kind, got, tc.want)
		}
	}
	if _, ok := ARM64LoadOp(ty(ir.Struct, 16)); ok {
		t.Error("a 16-byte load has a form, and no single instruction moves 16 bytes")
	}
	if _, ok := ARM64LoadOp(nil); ok {
		t.Error("a load with no type has a form")
	}
	for _, tc := range []struct {
		size int64
		want Op
	}{{1, OpARM64MOVBstore}, {2, OpARM64MOVHstore}, {4, OpARM64MOVWstore}, {8, OpARM64MOVDstore}} {
		got, ok := ARM64StoreOp(tc.size)
		if !ok || got != tc.want {
			t.Errorf("ARM64StoreOp(%d) = %v, want %v", tc.size, got, tc.want)
		}
	}
	if _, ok := ARM64StoreOp(3); ok {
		t.Error("a 3-byte store has a form")
	}
}

// TestARM64IndexOp covers the register-offset table, including the byte
// widths, which have no scaled form because a shift of zero is the plain one.
func TestARM64IndexOp(t *testing.T) {
	tests := []struct {
		base   Op
		scaled bool
		want   Op
		ok     bool
	}{
		{OpARM64MOVDload, true, OpARM64MOVDloadidx8, true},
		{OpARM64MOVDload, false, OpARM64MOVDloadidx, true},
		{OpARM64MOVWUload, true, OpARM64MOVWUloadidx4, true},
		{OpARM64MOVHload, true, OpARM64MOVHloadidx2, true},
		{OpARM64MOVBUload, true, OpInvalid, false},
		{OpARM64MOVBUload, false, OpARM64MOVBUloadidx, true},
		{OpARM64MOVDstore, true, OpARM64MOVDstoreidx8, true},
		{OpARM64MOVBstore, true, OpInvalid, false},
		{OpARM64ADD, false, OpInvalid, false},
	}
	for _, tc := range tests {
		got, ok := ARM64IndexOp(tc.base, tc.scaled)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ARM64IndexOp(%v, %v) = %v, %v, want %v, %v", tc.base, tc.scaled, got, ok, tc.want, tc.ok)
		}
	}
	// The scale in the operation must be the scale the encoder uses, or a fold
	// that checks the shift amount folds the wrong shift.
	for _, tc := range []struct {
		op    Op
		scale int64
	}{
		{OpARM64MOVDloadidx8, 8}, {OpARM64MOVWUloadidx4, 4},
		{OpARM64MOVHloadidx2, 2}, {OpARM64MOVBUloadidx, 1},
	} {
		m, ok := ARM64MemOp(tc.op)
		if !ok || m.Scale() != tc.scale {
			t.Errorf("%v scales by %d, want %d", tc.op, m.Scale(), tc.scale)
		}
	}
	if _, ok := ARM64MemOp(OpARM64ADD); ok {
		t.Error("ADD is a memory operation")
	}
}

// TestARM64SizeFromType covers the width rule: anything narrower than a word
// is computed in the 32-bit form, because a W register write zeroes the upper
// half and leaves the value in the shape the next instruction wants.
func TestARM64SizeFromType(t *testing.T) {
	tests := []struct {
		size int64
		want arm64.Size
	}{{0, arm64.Size32}, {1, arm64.Size32}, {2, arm64.Size32}, {4, arm64.Size32}, {8, arm64.Size64}}
	for _, tc := range tests {
		got := ARM64Size(&ir.Type{Kind: ir.Int64, Size: tc.size})
		if got != tc.want {
			t.Errorf("ARM64Size(size %d) = %v, want %v", tc.size, got, tc.want)
		}
	}
	if ARM64Size(nil) != arm64.Size32 {
		t.Error("a value with no type is not 32-bit")
	}
}

// TestARM64FloatMemOpSelection covers the two type-to-operation lookups.
//
// ARM64StoreOp answers from a size and cannot tell a four-byte integer store
// from a four-byte floating-point one, which is why the type-taking form
// exists and why ssagen's spill path still goes through the size-only one.
func TestARM64FloatMemOpSelection(t *testing.T) {
	f32 := &ir.Type{Kind: ir.Float32, Size: 4, Align: 4, Name: "float32"}
	f64 := &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}
	odd := &ir.Type{Kind: ir.Float64, Size: 16, Align: 8, Name: "wide"}
	for _, c := range []struct {
		t          *ir.Type
		load, stor Op
		ok         bool
	}{
		{f32, OpARM64FMOVSload, OpARM64FMOVSstore, true},
		{f64, OpARM64FMOVDload, OpARM64FMOVDstore, true},
		{lowI64, OpARM64MOVDload, OpARM64MOVDstore, true},
		{odd, OpInvalid, OpInvalid, false},
		{nil, OpInvalid, OpInvalid, false},
	} {
		got, ok := ARM64LoadOp(c.t)
		if ok != c.ok || (c.ok && got != c.load) {
			t.Errorf("ARM64LoadOp(%v) = %v, %v", c.t, got, ok)
		}
		got, ok = ARM64StoreOpForType(c.t)
		if ok != c.ok || (c.ok && got != c.stor) {
			t.Errorf("ARM64StoreOpForType(%v) = %v, %v", c.t, got, ok)
		}
	}
	// The size-only form is the integer one and stays that way.
	if got, ok := ARM64StoreOp(4); !ok || got != OpARM64MOVWstore {
		t.Errorf("ARM64StoreOp(4) = %v, %v", got, ok)
	}
	if _, ok := ARM64StoreOp(3); ok {
		t.Error("ARM64StoreOp accepted a width with no instruction")
	}
}

// arm64OperandClasses returns how many register operands of an operation are
// read out of the integer file and how many out of the floating-point file.
//
// The operation table records how many operands an instruction has, not which
// file each one comes from, so the encoder family supplies the missing half.
// A family this switch does not name counts its operands against both files,
// so adding one fails TestArm64ScratchCoversTheOperationTable until it is
// classified here rather than passing on an assumption.
//
// A variadic operation is a call or a return. specs/030-abi.md places every
// one of their operands and ssa/regalloc.go reads none of them out of a
// scratch register, so they have no count.
func arm64OperandClasses(o arm64Op) (ints, floats int, ok bool) {
	n := int(o.info.argLen)
	if n < 0 {
		return 0, 0, false
	}
	if o.info.takesMem {
		n--
	}
	if n <= 0 {
		return 0, 0, true
	}
	switch o.enc {
	case encFArith, encFUnary, encFCmp, encFCmpZero, encFConst, encFCvt:
		return 0, n, true
	case encFMove:
		// A move between the two files. The operand is in whichever file the
		// result is not, so it counts against both.
		return n, n, true
	case encMem, encMemIdx:
		// The base and the index are addresses and are always integers. A
		// store also reads the value it transfers, in the file the memory
		// operation names.
		if o.info.makesMem && o.mem.IsFloat() {
			return n - 1, 1, true
		}
		return n, 0, true
	case encNone, encArith, encArith3, encArithImm, encLogicalImm, encShiftImm,
		encAddShift, encUnary, encExt, encMovConst, encCmp, encCmpImm,
		encLogicalCmpImm, encAddr, encFrame, encGetClosure, encCondSet,
		encBranchCond, encCompareBranch, encCall, encCallInd, encRet:
		return n, 0, true
	}
	return n, n, true
}

// TestArm64ScratchCoversTheOperationTable is the assertion behind the scratch
// count in ssa/target.go.
//
// A value in a slot is read into a scratch register at each use and a
// rematerialised value is recomputed into one, so an instruction whose
// operands are all in slots needs one scratch register per operand at once.
// The count the target reserves is therefore the widest instruction the back
// end can emit, per register file, and this walks the table and computes it.
//
// Adding an operation that reads more registers than the target reserves is
// the change this catches. Without it the failure arrives as a ScratchError on
// whichever program first reaches the new operation, which is how the indexed
// stores and MADD were found.
//
// The reservation is also asserted to be no larger than the bound. Every
// register held back for materialisation is a register no value can live in,
// and R25 is already one the allocator lost.
func TestArm64ScratchCoversTheOperationTable(t *testing.T) {
	tg := NewArm64Target()
	var need [NumRegClass]int
	widest := [NumRegClass]string{}
	for i := range arm64Ops {
		o := arm64Ops[i]
		if o.info.name == "" {
			continue
		}
		ints, floats, ok := arm64OperandClasses(o)
		if !ok {
			if !o.info.call && Op(i)+OpARM64ADD != OpARM64RET {
				t.Errorf("%s has a variable operand list and is neither a call nor a return, "+
					"so specs/030-abi.md does not place its operands", o.info.name)
			}
			continue
		}
		for c, n := range [NumRegClass]int{ClassInt: ints, ClassFloat: floats} {
			if n > need[c] {
				need[c] = n
				widest[c] = o.info.name
			}
		}
	}
	for c := RegClass(0); c < NumRegClass; c++ {
		have := len(tg.Scratch[c])
		if have < need[c] {
			t.Errorf("%v: %s reads %d registers and the target reserves %d scratch registers; "+
				"an instruction with that many operands in slots cannot be read",
				c, widest[c], need[c], have)
		}
		if have > need[c] {
			t.Errorf("%v: the target reserves %d scratch registers and the widest operation, %s, "+
				"reads %d; a register held back for materialisation is one no value can live in",
				c, have, widest[c], need[c])
		}
	}
	// The numbers, so that a change to either side reads as a change and not
	// as an inequality that still holds.
	if need[ClassInt] != 3 || need[ClassFloat] != 2 {
		t.Errorf("the widest operation reads %d integer and %d floating-point registers, "+
			"and ssa/target.go says three and two", need[ClassInt], need[ClassFloat])
	}
}
