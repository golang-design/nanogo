// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package arm64

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The oracle is go tool asm. It is exact: it is the assembler the reference
// implementation ships, it encodes the same architecture, and it is not
// derived from anything in this package. Every encoder is swept over its
// operand range and compared byte for byte.
//
// The harness depends on three properties.
//
// First, go tool asm does not report an out-of-range immediate. It expands the
// instruction through R27 instead. So every comparison asserts that one source
// line produced exactly one instruction. Without that, a sweep that walks past
// a range boundary compares four bytes against the first quarter of an
// expansion and the difference looks like an encoder bug.
//
// Second, the TEXT flags are NOSPLIT|NOFRAME, spelled 516 so that the file
// needs no include. Any other flags give the function a prologue as soon as it
// contains a call, and the prologue moves every offset.
//
// Third, Plan 9 syntax is source first. ADD R1, R2, R3 means R3 = R2 + R1, so
// the assembly text puts the operands in the reverse of this package's order.

// comparisons counts every encoding compared against go tool asm.
var comparisons atomic.Int64

func TestMain(m *testing.M) {
	code := m.Run()
	fmt.Fprintf(os.Stderr, "arm64: %d encodings compared against go tool asm\n",
		comparisons.Load())
	os.Exit(code)
}

var asmProbe struct {
	once sync.Once
	err  error
}

// needAsm skips, or fails when CI demands the corpus, if go tool asm cannot
// run. A test that silently compares nothing is worse than one that is absent,
// because the coverage number still moves.
func needAsm(t *testing.T) {
	t.Helper()
	asmProbe.once.Do(func() {
		_, asmProbe.err = runAsm([]string{"\tNOOP"})
	})
	if asmProbe.err == nil {
		return
	}
	if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
		t.Fatalf("go tool asm is required but did not run: %v", asmProbe.err)
	}
	t.Skipf("go tool asm did not run: %v", asmProbe.err)
}

var (
	instRe = regexp.MustCompile(`^\t0x([0-9a-f]+) +\d+ \(t\.s:(\d+)\)\s+(\S+)`)
	dataRe = regexp.MustCompile(`^\t0x([0-9a-f]+)((?: [0-9a-f]{2}){1,16})  `)
)

// pseudoOp reports whether an opcode in the -S listing produces no machine
// code. They share an offset with the instruction that follows, so they have
// to be dropped before offsets are attributed.
func pseudoOp(op string) bool {
	switch op {
	case "TEXT", "FUNCDATA", "PCDATA", "GLOBL", "DATA":
		return true
	}
	return false
}

// runAsm assembles one TEXT holding the given lines and returns, per input
// line, the instructions go tool asm produced for it. A line that produces no
// instruction, a label for example, gets an empty slice.
func runAsm(lines []string) ([][]uint32, error) {
	dir, err := os.MkdirTemp("", "nanogo-arm64")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	var b strings.Builder
	b.WriteString("TEXT ·f(SB), 516, $0-0\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("\tRET\n")
	if err := os.WriteFile(filepath.Join(dir, "t.s"), []byte(b.String()), 0o600); err != nil {
		return nil, err
	}

	cmd := exec.Command("go", "tool", "asm", "-o", "t.o", "-S", "t.s")
	cmd.Dir = dir
	// Pin the target. Otherwise this passes on an arm64 host and fails
	// somewhere else for a reason that has nothing to do with the encoder.
	cmd.Env = append(os.Environ(), "GOARCH=arm64", "GOOS=darwin")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go tool asm: %v: %s", err, stderr.String())
	}
	return parseListing(stdout.String(), len(lines))
}

type located struct {
	off  int
	line int
}

func parseListing(out string, nlines int) ([][]uint32, error) {
	var text []byte
	var found []located
	for _, ln := range strings.Split(out, "\n") {
		if m := dataRe.FindStringSubmatch(ln); m != nil {
			off, err := strconv.ParseInt(m[1], 16, 32)
			if err != nil {
				return nil, err
			}
			for i, tok := range strings.Fields(m[2]) {
				v, err := strconv.ParseUint(tok, 16, 8)
				if err != nil {
					return nil, err
				}
				for int(off)+i >= len(text) {
					text = append(text, 0)
				}
				text[int(off)+i] = byte(v)
			}
			continue
		}
		m := instRe.FindStringSubmatch(ln)
		if m == nil || pseudoOp(m[3]) {
			continue
		}
		off, err := strconv.ParseInt(m[1], 16, 32)
		if err != nil {
			return nil, err
		}
		line, err := strconv.Atoi(m[2])
		if err != nil {
			return nil, err
		}
		found = append(found, located{int(off), line})
	}

	// The listing prints one line per source instruction, before expansion.
	// An immediate that does not fit is expanded through R27 later, and the
	// only evidence in the listing is that the next instruction starts eight
	// bytes on. So the size of an instruction is the distance to its
	// successor, and every line under test needs one.
	sort.SliceStable(found, func(i, j int) bool { return found[i].off < found[j].off })
	if len(found) > 0 && found[0].off != 0 {
		return nil, fmt.Errorf("the first instruction is at %#x, so the function got a prologue",
			found[0].off)
	}
	out2 := make([][]uint32, nlines)
	for k, f := range found {
		// File line 1 is the TEXT, so line n holds input line n-2.
		i := f.line - 2
		if i < 0 || i >= nlines {
			continue // the RET this harness appends
		}
		if k+1 >= len(found) {
			return nil, fmt.Errorf("instruction at %#x has no successor to size it against", f.off)
		}
		size := found[k+1].off - f.off
		if size <= 0 || size%4 != 0 {
			return nil, fmt.Errorf("instruction at %#x has size %d", f.off, size)
		}
		if f.off+size > len(text) {
			return nil, fmt.Errorf("instruction at %#x runs past the end of the text", f.off)
		}
		for b := 0; b < size; b += 4 {
			out2[i] = append(out2[i], binary.LittleEndian.Uint32(text[f.off+b:]))
		}
	}
	return out2, nil
}

// asmOne assembles lines and requires that every one of them produced exactly
// one instruction.
func asmOne(t *testing.T, lines []string) []uint32 {
	t.Helper()
	got, err := runAsm(lines)
	if err != nil {
		t.Fatalf("assembling %d lines: %v", len(lines), err)
	}
	out := make([]uint32, len(lines))
	for i, w := range got {
		if len(w) != 1 {
			t.Fatalf("%q assembled to %d instructions, want 1; "+
				"go tool asm expands rather than rejecting, so the comparison would be meaningless",
				strings.TrimSpace(lines[i]), len(w))
		}
		out[i] = w[0]
	}
	return out
}

// compare walks a batch of assembly lines and the encodings this package
// produced for them.
func compare(t *testing.T, lines []string, want []uint32) {
	t.Helper()
	if len(lines) != len(want) {
		t.Fatalf("internal: %d lines and %d encodings", len(lines), len(want))
	}
	if len(lines) == 0 {
		t.Fatal("no comparisons generated")
	}
	got := asmOne(t, lines)
	bad := 0
	for i := range got {
		comparisons.Add(1)
		if got[i] != want[i] {
			bad++
			if bad <= 10 {
				t.Errorf("%s: go tool asm %#08x, encoder %#08x",
					strings.TrimSpace(lines[i]), got[i], want[i])
			}
		}
	}
	if bad > 10 {
		t.Errorf("... and %d more disagreements", bad-10)
	}
	t.Logf("%d encodings compared", len(lines))
}

// ---------------------------------------------------------------------------
// Operand sweeps

// sweepRegs is every integer register the code generator can put in an
// instruction, plus the zero register.
//
// That is the allocatable registers and the scratch registers together, and
// not the allocatable ones alone. A scratch register carries a value the same
// way any other does: ssagen moves through one, spills through one and breaks
// a parallel copy cycle with one, so an encoding that is wrong for R16 is
// wrong in emitted code. Sweeping only what the allocator hands out leaves
// exactly the registers the code generator reaches for itself untested.
//
// This was found when R25 moved from allocatable to scratch and the swept
// count fell by eleven thousand, which said the sweep was following the
// allocator rather than the encoder. R18 is in neither set and stays out.
func sweepRegs() []Reg {
	out := append(AllocatableRegs(), ZR)
	for _, r := range []Reg{RegTrampLo, RegTrampHi, RegScratchThird} {
		out = append(out, r)
	}
	return out
}

// smallRegs is the subset used where the sweep is over triples.
var smallRegs = []Reg{R0, R7, R15, R19, R25, ZR}

// regTriples covers every triple over smallRegs and, over the full register
// set, every pair in each of the three positions. Sweeping all triples over
// the full set would be 13824 per instruction form and would prove nothing the
// pairs do not.
func regTriples() [][3]Reg {
	var out [][3]Reg
	for _, a := range smallRegs {
		for _, b := range smallRegs {
			for _, c := range smallRegs {
				out = append(out, [3]Reg{a, b, c})
			}
		}
	}
	full := sweepRegs()
	for _, a := range full {
		for _, b := range full {
			out = append(out,
				[3]Reg{a, b, R3},
				[3]Reg{R3, a, b},
				[3]Reg{a, R3, b})
		}
	}
	return out
}

func regPairs() [][2]Reg {
	var out [][2]Reg
	full := sweepRegs()
	for _, a := range full {
		for _, b := range full {
			out = append(out, [2]Reg{a, b})
		}
	}
	return out
}

// mnemonic picks the Plan 9 spelling for an operand width. The 32-bit forms
// take a W suffix.
func mnemonic(base string, sz Size) string {
	if sz == Size32 {
		return base + "W"
	}
	return base
}

func bothSizes(f func(sz Size)) {
	f(Size64)
	f(Size32)
}

// ---------------------------------------------------------------------------
// Add, subtract, and the logical shifted-register forms

// regForm is a three-register instruction and its Plan 9 spelling.
type regForm struct {
	name string
	enc  func(sz Size, dst, a, b Reg) uint32
}

var regForms = []regForm{
	{"ADD", AddRegReg},
	{"SUB", SubRegReg},
	{"ADDS", AddsRegReg},
	{"SUBS", SubsRegReg},
	{"AND", AndRegReg},
	{"ORR", OrrRegReg},
	{"EOR", EorRegReg},
	{"BIC", BicRegReg},
	{"ORN", OrnRegReg},
	{"EON", EonRegReg},
	{"ANDS", AndsRegReg},
	{"BICS", BicsRegReg},
	{"MUL", MulRegReg},
	{"MNEG", MnegRegReg},
	{"SDIV", SdivRegReg},
	{"UDIV", UdivRegReg},
	{"LSL", LslRegReg},
	{"LSR", LsrRegReg},
	{"ASR", AsrRegReg},
	{"ROR", RorRegReg},
}

func TestDiffThreeRegisterForms(t *testing.T) {
	needAsm(t)
	triples := regTriples()
	var lines []string
	var want []uint32
	for _, f := range regForms {
		bothSizes(func(sz Size) {
			for _, r := range triples {
				dst, a, b := r[0], r[1], r[2]
				lines = append(lines, fmt.Sprintf("\t%s\t%s, %s, %s",
					mnemonic(f.name, sz), b, a, dst))
				want = append(want, f.enc(sz, dst, a, b))
			}
		})
	}
	compare(t, lines, want)
}

// shiftedForm is a three-register instruction that also accepts a shift on its
// second source.
type shiftedForm struct {
	name  string
	enc   func(sz Size, dst, a, b Reg, sh Shift, amount uint32) (uint32, bool)
	noRor bool // the add and subtract class cannot encode ROR
}

var shiftedForms = []shiftedForm{
	{"ADD", AddRegRegShift, true},
	{"SUB", SubRegRegShift, true},
	{"AND", AndRegRegShift, false},
	{"ORR", OrrRegRegShift, false},
	{"EOR", EorRegRegShift, false},
	{"BIC", BicRegRegShift, false},
}

// p9Shift is the Plan 9 spelling of a shifted source operand.
func p9Shift(b Reg, sh Shift, amount uint32) string {
	switch sh {
	case LSL:
		return fmt.Sprintf("%s<<%d", b, amount)
	case LSR:
		return fmt.Sprintf("%s>>%d", b, amount)
	case ASR:
		return fmt.Sprintf("%s->%d", b, amount)
	default:
		return fmt.Sprintf("%s@>%d", b, amount)
	}
}

func TestDiffShiftedRegisterForms(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, f := range shiftedForms {
		bothSizes(func(sz Size) {
			for sh := LSL; sh < numShift; sh++ {
				if sh == ROR && f.noRor {
					continue
				}
				for amount := uint32(0); amount < sz.bits(); amount++ {
					v, ok := f.enc(sz, R3, R2, R1, sh, amount)
					if !ok {
						t.Fatalf("%s %s #%d rejected", f.name, sh, amount)
					}
					lines = append(lines, fmt.Sprintf("\t%s\t%s, %s, %s",
						mnemonic(f.name, sz), p9Shift(R1, sh, amount), R2, R3))
					want = append(want, v)
				}
			}
		})
	}
	compare(t, lines, want)
}

// ---------------------------------------------------------------------------
// Two-register forms

type twoRegForm struct {
	name string
	enc  func(sz Size, dst, src Reg) uint32
}

var twoRegForms = []twoRegForm{
	{"NEG", NegReg},
	{"MVN", MvnReg},
}

func TestDiffTwoRegisterForms(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, f := range twoRegForms {
		bothSizes(func(sz Size) {
			for _, p := range regPairs() {
				lines = append(lines, fmt.Sprintf("\t%s\t%s, %s",
					mnemonic(f.name, sz), p[1], p[0]))
				want = append(want, f.enc(sz, p[0], p[1]))
			}
		})
	}
	// The comparison forms write their result to the zero register, so Plan 9
	// spells them with two operands and no destination.
	for _, p := range regPairs() {
		bothSizes(func(sz Size) {
			lines = append(lines, fmt.Sprintf("\t%s\t%s, %s", mnemonic("CMP", sz), p[1], p[0]))
			want = append(want, CmpRegReg(sz, p[0], p[1]))
			lines = append(lines, fmt.Sprintf("\t%s\t%s, %s", mnemonic("CMN", sz), p[1], p[0]))
			want = append(want, CmnRegReg(sz, p[0], p[1]))
			lines = append(lines, fmt.Sprintf("\t%s\t%s, %s", mnemonic("TST", sz), p[1], p[0]))
			want = append(want, TstRegReg(sz, p[0], p[1]))
		})
	}
	compare(t, lines, want)
}

func TestDiffMoveAndExtend(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, p := range regPairs() {
		if p[0] == p[1] {
			// The assembler may drop a move with the same source and
			// destination, and a dropped line breaks the one-line one-
			// instruction rule the harness relies on.
			continue
		}
		lines = append(lines, fmt.Sprintf("\tMOVD\t%s, %s", p[1], p[0]))
		want = append(want, MovRegReg(Size64, p[0], p[1]))
		lines = append(lines, fmt.Sprintf("\tMOVWU\t%s, %s", p[1], p[0]))
		want = append(want, MovRegReg(Size32, p[0], p[1]))
	}
	// The extensions are swept over the allocatable registers only. go tool
	// asm rewrites an extension of the zero register into a plain move, which
	// is a different instruction and not a disagreement about encoding.
	alloc := AllocatableRegs()
	for _, dst := range alloc {
		for _, src := range alloc {
			lines = append(lines, fmt.Sprintf("\tSXTB\t%s, %s", src, dst))
			want = append(want, SxtbReg(Size64, dst, src))
			lines = append(lines, fmt.Sprintf("\tSXTH\t%s, %s", src, dst))
			want = append(want, SxthReg(Size64, dst, src))
			lines = append(lines, fmt.Sprintf("\tSXTW\t%s, %s", src, dst))
			want = append(want, SxtwReg(dst, src))
			lines = append(lines, fmt.Sprintf("\tUXTB\t%s, %s", src, dst))
			want = append(want, UxtbReg(Size64, dst, src))
			lines = append(lines, fmt.Sprintf("\tUXTH\t%s, %s", src, dst))
			want = append(want, UxthReg(Size64, dst, src))
		}
	}
	// MOVD between a general register and the stack pointer is a different
	// class: ADD with a zero immediate, because the register move that ORR
	// encodes cannot name the stack pointer.
	for _, r := range AllocatableRegs() {
		lines = append(lines, fmt.Sprintf("\tMOVD\tRSP, %s", r))
		want = append(want, MovSP(Size64, r, RSP))
		lines = append(lines, fmt.Sprintf("\tMOVD\t%s, RSP", r))
		want = append(want, MovSP(Size64, RSP, r))
	}
	compare(t, lines, want)
}

func TestDiffMultiplyAccumulate(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	// Plan 9 order is MADD Rm, Ra, Rn, Rd for Rd = Ra + Rn*Rm.
	for _, r := range regTriples() {
		bothSizes(func(sz Size) {
			dst, a, b := r[0], r[1], r[2]
			lines = append(lines, fmt.Sprintf("\t%s\t%s, %s, %s, %s",
				mnemonic("MADD", sz), b, R4, a, dst))
			want = append(want, Madd(sz, dst, a, b, R4))
			lines = append(lines, fmt.Sprintf("\t%s\t%s, %s, %s, %s",
				mnemonic("MSUB", sz), b, R4, a, dst))
			want = append(want, Msub(sz, dst, a, b, R4))
		})
	}
	compare(t, lines, want)
}

// ---------------------------------------------------------------------------
// Add and subtract immediates: the whole 12-bit range, shifted and not

type immForm struct {
	name string
	enc  func(sz Size, dst, a Reg, imm int64) (uint32, bool)
}

var addSubImmForms = []immForm{
	{"ADD", AddRegImm},
	{"SUB", SubRegImm},
	{"ADDS", AddsRegImm},
	{"SUBS", SubsRegImm},
}

func TestDiffAddSubImmediate(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	add := func(text string, v uint32, ok bool, what string) {
		if !ok {
			t.Fatalf("%s rejected", what)
		}
		lines = append(lines, text)
		want = append(want, v)
	}
	for _, f := range addSubImmForms {
		bothSizes(func(sz Size) {
			for imm := int64(0); imm <= MaxAddSubImm; imm++ {
				v, ok := f.enc(sz, R3, R2, imm)
				add(fmt.Sprintf("\t%s\t$%d, %s, %s", mnemonic(f.name, sz), imm, R2, R3),
					v, ok, fmt.Sprintf("%s $%d", f.name, imm))
				sh := imm << 12
				if sh == 0 {
					continue
				}
				v, ok = f.enc(sz, R3, R2, sh)
				add(fmt.Sprintf("\t%s\t$%d, %s, %s", mnemonic(f.name, sz), sh, R2, R3),
					v, ok, fmt.Sprintf("%s $%d", f.name, sh))
			}
		})
	}
	// CMP and CMN are the same class with the zero register as destination.
	for imm := int64(0); imm <= MaxAddSubImm; imm++ {
		bothSizes(func(sz Size) {
			v, ok := CmpRegImm(sz, R2, imm)
			add(fmt.Sprintf("\t%s\t$%d, %s", mnemonic("CMP", sz), imm, R2), v, ok, "CMP")
			v, ok = CmnRegImm(sz, R2, imm)
			add(fmt.Sprintf("\t%s\t$%d, %s", mnemonic("CMN", sz), imm, R2), v, ok, "CMN")
		})
	}
	// The base register sweep, including the stack pointer, which this class
	// can name where the shifted-register class cannot.
	for _, r := range append(AllocatableRegs(), RSP) {
		v, ok := AddRegImm(Size64, R3, r, 24)
		add(fmt.Sprintf("\tADD\t$24, %s, %s", r, R3), v, ok, "ADD base")
		v, ok = AddRegImm(Size64, r, R2, 24)
		add(fmt.Sprintf("\tADD\t$24, %s, %s", R2, r), v, ok, "ADD dst")
	}
	compare(t, lines, want)
}

// TestAddImmediateAgreesOnWhatFits walks past the range in both directions and
// requires that the encoder accepts a value exactly when go tool asm needs one
// instruction for it. This is the two-way form of the range check: it catches
// an encoder that is too permissive as well as one that is too strict.
func TestAddImmediateAgreesOnWhatFits(t *testing.T) {
	needAsm(t)
	const limit = 1 << 17
	var lines []string
	var mine []bool
	for imm := int64(0); imm < limit; imm++ {
		_, ok := AddRegImm(Size64, R3, R2, imm)
		lines = append(lines, fmt.Sprintf("\tADD\t$%d, R2, R3", imm))
		mine = append(mine, ok)
	}
	got, err := runAsm(lines)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for i, w := range got {
		single := len(w) == 1
		if single != mine[i] {
			t.Fatalf("%s: go tool asm used %d instructions, encoder reports fits=%v",
				strings.TrimSpace(lines[i]), len(w), mine[i])
		}
		n++
	}
	if n == 0 {
		t.Fatal("no values checked")
	}
	t.Logf("%d immediates agreed on what fits", n)
}

// ---------------------------------------------------------------------------
// The logical immediate bitmask encoding
//
// Two independent oracles. The first is closed: every (N, immr, imms) triple
// the architecture defines is decoded with the ARM pseudocode, and the encoder
// has to map the decoded value back to the same triple. That is what catches a
// non-canonical encoder, which decodes correctly and still disagrees with
// every other assembler. The second is go tool asm.

// decodeBitMask is ARM's DecodeBitMasks with immediate=TRUE, transcribed. It
// returns the 64-bit mask and the element size.
func decodeBitMask(n, immr, imms uint32) (val uint64, esize uint32, ok bool) {
	x := n<<6 | (^imms & 0x3f)
	length := 31 - bits.LeadingZeros32(x)
	if length < 1 {
		return 0, 0, false
	}
	esize = uint32(1) << uint(length)
	levels := esize - 1
	s := imms & levels
	r := immr & levels
	if s == levels {
		return 0, 0, false // a run of ones cannot fill the element
	}
	welem := ^uint64(0) >> (63 - s) // s+1 ones at the bottom
	rot := welem>>r | welem<<(esize-r)
	if esize < 64 {
		rot &= uint64(1)<<esize - 1
	}
	for i := uint32(0); i < 64; i += esize {
		val |= rot << i
	}
	return val, esize, true
}

type bitmaskEntry struct {
	val           uint64
	n, immr, imms uint32
	esize         uint32
}

// bitmaskTable returns one entry per representable 64-bit value, holding the
// canonical triple: the one with the smallest element size, and within that
// the smallest immr. Distinct triples can decode to the same value, so
// "decodes correctly" is not "encodes canonically" and the canonical choice is
// the one every assembler makes.
func bitmaskTable() []bitmaskEntry {
	var all []bitmaskEntry
	for n := uint32(0); n <= 1; n++ {
		for immr := uint32(0); immr < 64; immr++ {
			for imms := uint32(0); imms < 64; imms++ {
				val, esize, ok := decodeBitMask(n, immr, imms)
				if !ok {
					continue
				}
				all = append(all, bitmaskEntry{val, n, immr, imms, esize})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.val != b.val {
			return a.val < b.val
		}
		if a.esize != b.esize {
			return a.esize < b.esize
		}
		if a.immr != b.immr {
			return a.immr < b.immr
		}
		return a.imms < b.imms
	})
	var out []bitmaskEntry
	for i, e := range all {
		if i == 0 || all[i-1].val != e.val {
			out = append(out, e)
		}
	}
	return out
}

func TestBitMaskRoundTrip(t *testing.T) {
	table := bitmaskTable()
	if len(table) == 0 {
		t.Fatal("no representable patterns enumerated")
	}
	for _, e := range table {
		n, immr, imms, ok := LogicalImm(Size64, e.val)
		if !ok {
			t.Fatalf("LogicalImm(64, %#016x) rejected a representable value", e.val)
		}
		if n != e.n || immr != e.immr || imms != e.imms {
			t.Fatalf("LogicalImm(64, %#016x) = N=%d immr=%d imms=%d, want N=%d immr=%d imms=%d",
				e.val, n, immr, imms, e.n, e.immr, e.imms)
		}
		if got, _, ok := decodeBitMask(n, immr, imms); !ok || got != e.val {
			t.Fatalf("re-decoding %#016x gave %#016x ok=%v", e.val, got, ok)
		}
	}
	t.Logf("%d distinct 64-bit patterns round-tripped", len(table))

	// The 32-bit forms replicate a pattern of at most 32 bits and must leave N
	// zero, because N is what selects a 64-bit element.
	seen32 := 0
	for _, e := range table {
		if e.esize > 32 {
			continue
		}
		v32 := e.val & 0xffffffff
		n, immr, imms, ok := LogicalImm(Size32, v32)
		if !ok {
			t.Fatalf("LogicalImm(32, %#08x) rejected", v32)
		}
		if n != 0 {
			t.Fatalf("LogicalImm(32, %#08x) set N", v32)
		}
		if n != e.n || immr != e.immr || imms != e.imms {
			t.Fatalf("LogicalImm(32, %#08x) = N=%d immr=%d imms=%d, want N=%d immr=%d imms=%d",
				v32, n, immr, imms, e.n, e.immr, e.imms)
		}
		seen32++
	}
	t.Logf("%d distinct 32-bit patterns round-tripped", seen32)

	// Neither zero nor all ones is representable: the run of ones can be
	// neither empty nor the whole element.
	for _, v := range []uint64{0, ^uint64(0)} {
		if _, _, _, ok := LogicalImm(Size64, v); ok {
			t.Errorf("LogicalImm(64, %#016x) accepted", v)
		}
	}
	for _, v := range []uint64{0, 0xffffffff} {
		if _, _, _, ok := LogicalImm(Size32, v); ok {
			t.Errorf("LogicalImm(32, %#08x) accepted", v)
		}
	}
	// A 32-bit form cannot carry a value with anything in its top half.
	if _, _, _, ok := LogicalImm(Size32, 0x1_0000_00ff); ok {
		t.Error("LogicalImm(32) accepted a value wider than 32 bits")
	}
}

// TestBitMaskRejectsExactly sweeps a contiguous range and requires the encoder
// to accept a value exactly when the enumeration says it is representable.
func TestBitMaskRejectsExactly(t *testing.T) {
	table := bitmaskTable()
	vals := make([]uint64, len(table))
	for i, e := range table {
		vals[i] = e.val
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	member := func(v uint64) bool {
		i := sort.Search(len(vals), func(i int) bool { return vals[i] >= v })
		return i < len(vals) && vals[i] == v
	}
	const limit = 1 << 17
	n := 0
	for v := uint64(0); v < limit; v++ {
		_, _, _, ok := LogicalImm(Size64, v)
		if ok != member(v) {
			t.Fatalf("LogicalImm(64, %#x) ok=%v, enumeration says %v", v, ok, member(v))
		}
		n++
	}
	t.Logf("%d values agreed on representability", n)
}

var logicalImmForms = []struct {
	name string
	enc  func(sz Size, dst, a Reg, val uint64) (uint32, bool)
}{
	{"AND", AndRegImm},
	{"ORR", OrrRegImm},
	{"EOR", EorRegImm},
	{"ANDS", AndsRegImm},
}

func TestDiffLogicalImmediate(t *testing.T) {
	needAsm(t)
	table := bitmaskTable()
	var lines []string
	var want []uint32
	for _, f := range logicalImmForms {
		for _, e := range table {
			v, ok := f.enc(Size64, R3, R2, e.val)
			if !ok {
				t.Fatalf("%s $%#x rejected", f.name, e.val)
			}
			lines = append(lines, fmt.Sprintf("\t%s\t$%d, %s, %s", f.name, int64(e.val), R2, R3))
			want = append(want, v)
			if e.esize > 32 {
				continue
			}
			v32 := e.val & 0xffffffff
			v, ok = f.enc(Size32, R3, R2, v32)
			if !ok {
				t.Fatalf("%sW $%#x rejected", f.name, v32)
			}
			lines = append(lines, fmt.Sprintf("\t%sW\t$%d, %s, %s",
				f.name, int64(int32(uint32(v32))), R2, R3))
			want = append(want, v)
		}
	}
	// TST and BIC are the same class: TST discards the result and BIC inverts
	// the immediate. The architecture has no BIC with an immediate at all.
	for _, e := range table {
		v, ok := TstRegImm(Size64, R2, e.val)
		if !ok {
			t.Fatalf("TST $%#x rejected", e.val)
		}
		lines = append(lines, fmt.Sprintf("\tTST\t$%d, %s", int64(e.val), R2))
		want = append(want, v)

		if v, ok := BicRegImm(Size64, R3, R2, ^e.val); ok {
			lines = append(lines, fmt.Sprintf("\tBIC\t$%d, %s, %s", int64(^e.val), R2, R3))
			want = append(want, v)
		}
		if v, ok := MovLogicalImm(Size64, R3, e.val); ok {
			lines = append(lines, fmt.Sprintf("\tORR\t$%d, ZR, %s", int64(e.val), R3))
			want = append(want, v)
		}
	}
	compare(t, lines, want)
}

// ---------------------------------------------------------------------------
// Move wide immediates and the constant materialiser

func TestDiffMoveWide(t *testing.T) {
	needAsm(t)
	forms := []struct {
		name string
		enc  func(sz Size, dst Reg, imm16 uint16, shift uint32) (uint32, bool)
	}{
		{"MOVZ", Movz},
		{"MOVK", Movk},
		{"MOVN", Movn},
	}
	var lines []string
	var want []uint32
	for _, f := range forms {
		bothSizes(func(sz Size) {
			for shift := uint32(0); shift < sz.bits(); shift += 16 {
				// The whole 16-bit field at the first shift, and a stride at
				// the others. The two fields are independent in the layout, so
				// a full sweep of each is a full sweep of the pair.
				step := 1
				if shift != 0 {
					step = 17
				}
				for imm := 1; imm <= 0xffff; imm += step {
					v, ok := f.enc(sz, R3, uint16(imm), shift)
					if !ok {
						t.Fatalf("%s $%d, LSL %d rejected", f.name, imm, shift)
					}
					lines = append(lines, fmt.Sprintf("\t%s\t$%d, %s",
						mnemonic(f.name, sz), int64(imm)<<shift, R3))
					want = append(want, v)
				}
			}
		})
	}
	// The destination sweep. go tool asm rejects a zero immediate for this
	// class, so the value is fixed and non-zero here.
	for _, r := range sweepRegs() {
		v, _ := Movz(Size64, r, 0x1234, 32)
		lines = append(lines, fmt.Sprintf("\tMOVZ\t$%d, %s", int64(0x1234)<<32, r))
		want = append(want, v)
	}
	compare(t, lines, want)
}

// moveWideValue runs a MOVZ, MOVN and MOVK sequence and returns what it leaves
// in the destination. It rejects anything that is not one of those three, so
// it cannot accidentally approve a sequence the materialiser should not emit.
func moveWideValue(t *testing.T, sz Size, seq []uint32) uint64 {
	t.Helper()
	var val uint64
	for _, in := range seq {
		if (in>>23)&0x3f != 0x25 {
			t.Fatalf("%#08x is not a move wide instruction", in)
		}
		if (in >> 31) != uint32(sz) {
			t.Fatalf("%#08x has the wrong operand width", in)
		}
		shift := ((in >> 21) & 3) * 16
		imm := uint64(uint16(in>>5)) << shift
		switch (in >> 29) & 3 {
		case 0: // MOVN
			val = ^imm
		case 2: // MOVZ
			val = imm
		case 3: // MOVK
			val = val&^(0xffff<<shift) | imm
		default:
			t.Fatalf("%#08x is not MOVN, MOVZ or MOVK", in)
		}
	}
	if sz == Size32 {
		val &= 0xffffffff
	}
	return val
}

// movConstValues is a deterministic sweep: every 16-bit value, every halfword
// in every position, the neighbourhoods of every power of two, and a
// pseudo-random tail from a fixed seed.
func movConstValues() []int64 {
	var out []int64
	for v := int64(0); v <= 0xffff; v++ {
		out = append(out, v)
	}
	for h := int64(1); h <= 0xffff; h += 251 {
		for sh := uint(0); sh < 64; sh += 16 {
			out = append(out, h<<sh, ^(h << sh), h<<sh|0xffff)
		}
	}
	for b := uint(0); b < 64; b++ {
		v := int64(1) << b
		out = append(out, v, v-1, v+1, -v, ^v)
	}
	seed := uint64(0x2545f4914f6cdd1d)
	for i := 0; i < 20000; i++ {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		out = append(out, int64(seed))
	}
	return out
}

func TestMovConst(t *testing.T) {
	var buf [MaxMovConst]uint32
	checked := 0
	for _, v := range movConstValues() {
		bothSizes(func(sz Size) {
			n := MovConst(sz, R3, v, buf[:])
			if n < 1 || n > MaxMovConst {
				t.Fatalf("MovConst(%s, %d) used %d instructions", sz, v, n)
			}
			want := uint64(v)
			if sz == Size32 {
				want &= 0xffffffff
			}
			if got := moveWideValue(t, sz, buf[:n]); got != want {
				t.Fatalf("MovConst(%s, %#x) leaves %#x", sz, want, got)
			}
			if min := minMoveWide(sz, want); n != min {
				t.Fatalf("MovConst(%s, %#x) used %d instructions, %d suffice", sz, want, n, min)
			}
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("no constants checked")
	}
	t.Logf("%d constants materialised and re-evaluated", checked)
}

// minMoveWide is the shortest MOVZ, MOVN and MOVK sequence that can exist: one
// instruction per halfword that differs from the starting point, and never
// fewer than one.
func minMoveWide(sz Size, v uint64) int {
	halves := 4
	if sz == Size32 {
		halves = 2
		v &= 0xffffffff
	}
	zeros, ones := 0, 0
	for i := 0; i < halves; i++ {
		h := uint16(v >> (16 * i))
		if h != 0 {
			zeros++
		}
		if h != 0xffff {
			ones++
		}
	}
	n := zeros
	if ones < n {
		n = ones
	}
	if n == 0 {
		return 1
	}
	return n
}

// TestDiffMovConst compares the materialiser against go tool asm wherever the
// assembler also chose a move wide sequence.
//
// It does not compare instruction counts. The assembler reaches values such as
// 0x0000ffff0000ffff with one ORR of a bitmask immediate, which no MOVZ and
// MOVK sequence can match, and MovConst deliberately does not use that class.
// MovLogicalImm covers it and lowering is expected to try it first.
func TestDiffMovConst(t *testing.T) {
	needAsm(t)
	all := movConstValues()
	// A stride rather than a prefix: the head of the list is the 16-bit sweep,
	// and every one of those is a single MOVZ, which would test nothing about
	// the multi-instruction sequences.
	var vals []int64
	for i := 0; i < len(all); i += 3 {
		vals = append(vals, all[i])
	}
	var lines []string
	for _, v := range vals {
		lines = append(lines, fmt.Sprintf("\tMOVD\t$%d, %s", v, R3))
	}
	got, err := runAsm(lines)
	if err != nil {
		t.Fatal(err)
	}
	var buf [MaxMovConst]uint32
	compared, skipped := 0, 0
	for i, seq := range got {
		allMoveWide := len(seq) > 0
		for _, in := range seq {
			if (in>>23)&0x3f != 0x25 {
				allMoveWide = false
			}
		}
		if !allMoveWide {
			skipped++
			continue
		}
		n := MovConst(Size64, R3, vals[i], buf[:])
		if n != len(seq) {
			t.Fatalf("%s: go tool asm used %d move wide instructions, encoder used %d",
				strings.TrimSpace(lines[i]), len(seq), n)
		}
		for j := range seq {
			comparisons.Add(1)
			if seq[j] != buf[j] {
				t.Fatalf("%s instruction %d: go tool asm %#08x, encoder %#08x",
					strings.TrimSpace(lines[i]), j, seq[j], buf[j])
			}
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("no sequences compared")
	}
	t.Logf("%d sequences matched go tool asm, %d used a class MovConst does not emit",
		compared, skipped)
}

// ---------------------------------------------------------------------------
// Shifts by an immediate

func TestDiffShiftImmediate(t *testing.T) {
	needAsm(t)
	forms := []struct {
		name string
		enc  func(sz Size, dst, a Reg, shift uint32) (uint32, bool)
	}{
		{"LSL", LslRegImm},
		{"LSR", LsrRegImm},
		{"ASR", AsrRegImm},
		{"ROR", RorRegImm},
	}
	var lines []string
	var want []uint32
	for _, f := range forms {
		bothSizes(func(sz Size) {
			for sh := uint32(0); sh < sz.bits(); sh++ {
				v, ok := f.enc(sz, R3, R2, sh)
				if !ok {
					t.Fatalf("%s $%d rejected", f.name, sh)
				}
				lines = append(lines, fmt.Sprintf("\t%s\t$%d, %s, %s",
					mnemonic(f.name, sz), sh, R2, R3))
				want = append(want, v)
			}
			for _, p := range regPairs() {
				v, _ := f.enc(sz, p[0], p[1], 5)
				lines = append(lines, fmt.Sprintf("\t%s\t$5, %s, %s",
					mnemonic(f.name, sz), p[1], p[0]))
				want = append(want, v)
			}
		})
	}
	compare(t, lines, want)
}

// ---------------------------------------------------------------------------
// Loads and stores

// memForm carries the Plan 9 spelling of a load or store width. LoadBS32 and
// LoadHS32 have none: Plan 9 syntax has no W register names, so a byte load
// that sign-extends to 32 bits cannot be written. Those two are covered by
// TestMemOpFieldLayout instead.
type memForm struct {
	op    MemOp
	p9    string
	store bool
}

var memForms = []memForm{
	{StoreB, "MOVB", true},
	{StoreH, "MOVH", true},
	{StoreW, "MOVW", true},
	{StoreX, "MOVD", true},
	{LoadBU, "MOVBU", false},
	{LoadHU, "MOVHU", false},
	{LoadWU, "MOVWU", false},
	{LoadX, "MOVD", false},
	{LoadBS64, "MOVB", false},
	{LoadHS64, "MOVH", false},
	{LoadWS64, "MOVW", false},
}

func (f memForm) line(suffix string, rt Reg, off int64, base Reg) string {
	if f.store {
		return fmt.Sprintf("\t%s%s\t%s, %d(%s)", f.p9, suffix, rt, off, base)
	}
	return fmt.Sprintf("\t%s%s\t%d(%s), %s", f.p9, suffix, off, base, rt)
}

func TestDiffMemUnsignedOffset(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, f := range memForms {
		scale := f.op.Scale()
		for i := int64(0); i <= MaxMemOffsetScaled; i++ {
			off := i * scale
			v, ok := MemUnsignedOffset(f.op, R2, R1, off)
			if !ok {
				t.Fatalf("%v offset %d rejected", f.op, off)
			}
			lines = append(lines, f.line("", R2, off, R1))
			want = append(want, v)
		}
	}
	// The register sweep, with the stack pointer as a base because loading
	// from the frame is the common case.
	bases := append(AllocatableRegs(), RSP)
	for _, f := range memForms {
		for _, base := range bases {
			for _, rt := range sweepRegs() {
				off := 3 * f.op.Scale()
				v, ok := MemUnsignedOffset(f.op, rt, base, off)
				if !ok {
					t.Fatalf("%v rejected", f.op)
				}
				lines = append(lines, f.line("", rt, off, base))
				want = append(want, v)
			}
		}
	}
	compare(t, lines, want)
}

func TestDiffMemUnscaledAndIndexed(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, f := range memForms {
		scale := f.op.Scale()
		for off := int64(MinMemOffsetUnscaled); off <= MaxMemOffsetUnscaled; off++ {
			// go tool asm picks the unscaled form only when the scaled one
			// cannot hold the offset, which is exactly when the offset is
			// negative or not a multiple of the access size.
			if off >= 0 && off%scale == 0 {
				continue
			}
			v, ok := MemUnscaled(f.op, R2, R1, off)
			if !ok {
				t.Fatalf("%v unscaled offset %d rejected", f.op, off)
			}
			lines = append(lines, f.line("", R2, off, R1))
			want = append(want, v)
		}
		for off := int64(MinMemOffsetUnscaled); off <= MaxMemOffsetUnscaled; off++ {
			v, ok := MemPreIndex(f.op, R2, R1, off)
			if !ok {
				t.Fatalf("%v pre-index offset %d rejected", f.op, off)
			}
			lines = append(lines, f.line(".W", R2, off, R1))
			want = append(want, v)

			v, ok = MemPostIndex(f.op, R2, R1, off)
			if !ok {
				t.Fatalf("%v post-index offset %d rejected", f.op, off)
			}
			lines = append(lines, f.line(".P", R2, off, R1))
			want = append(want, v)
		}
	}
	compare(t, lines, want)
}

func extendSuffix(e Extend, shifted bool, scale int64) string {
	s := ""
	switch e {
	case UXTW:
		s = ".UXTW"
	case SXTW:
		s = ".SXTW"
	case SXTX:
		s = ".SXTX"
	}
	if shifted {
		s += fmt.Sprintf("<<%d", bits.TrailingZeros64(uint64(scale)))
	}
	return s
}

func TestDiffMemRegisterOffset(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	exts := []Extend{UXTW, LSLX, SXTW, SXTX}
	bases := append(AllocatableRegs(), RSP)
	for _, f := range memForms {
		scale := f.op.Scale()
		for _, ext := range exts {
			for _, shifted := range []bool{false, true} {
				// Plan 9 writes a scaled index as <<n, and for a byte access
				// n would be zero, which the assembler reads as no scaling at
				// all. The S bit is unreachable there.
				if shifted && scale == 1 {
					continue
				}
				v, ok := MemRegOffset(f.op, R3, R1, R2, ext, shifted)
				if !ok {
					t.Fatalf("%v %v rejected", f.op, ext)
				}
				idx := R2.String() + extendSuffix(ext, shifted, scale)
				if f.store {
					lines = append(lines, fmt.Sprintf("\t%s\t%s, (%s)(%s)", f.p9, R3, R1, idx))
				} else {
					lines = append(lines, fmt.Sprintf("\t%s\t(%s)(%s), %s", f.p9, R1, idx, R3))
				}
				want = append(want, v)
			}
		}
		for _, base := range bases {
			for _, index := range AllocatableRegs() {
				v, ok := MemRegOffset(f.op, R3, base, index, LSLX, false)
				if !ok {
					t.Fatal("rejected")
				}
				if f.store {
					lines = append(lines, fmt.Sprintf("\t%s\t%s, (%s)(%s)", f.p9, R3, base, index))
				} else {
					lines = append(lines, fmt.Sprintf("\t%s\t(%s)(%s), %s", f.p9, base, index, R3))
				}
				want = append(want, v)
			}
		}
	}
	compare(t, lines, want)
}

// TestMemOpFieldLayout covers the two forms Plan 9 syntax cannot spell, by
// checking the size and opc fields the ARM manual gives them.
func TestMemOpFieldLayout(t *testing.T) {
	cases := []struct {
		op        MemOp
		size, opc uint32
	}{
		{LoadBS32, 0, 3},
		{LoadHS32, 1, 3},
		{LoadBS64, 0, 2},
		{LoadHS64, 1, 2},
	}
	for _, c := range cases {
		v, ok := MemUnsignedOffset(c.op, R2, R1, c.op.Scale())
		if !ok {
			t.Fatalf("%v rejected", c.op)
		}
		if got := v >> 30; got != c.size {
			t.Errorf("%v size field %d, want %d", c.op, got, c.size)
		}
		if got := (v >> 22) & 3; got != c.opc {
			t.Errorf("%v opc field %d, want %d", c.op, got, c.opc)
		}
		if got := (v >> 10) & 0xfff; got != 1 {
			t.Errorf("%v imm12 field %d, want 1", c.op, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Branches
//
// N branches that all target one label give N distinct offsets in N
// instructions, so a label at each end of a block sweeps a contiguous range in
// both directions at the cost of one instruction per offset.

// compareAt compares only the lines named by idx, which lets a batch hold
// labels and padding.
func compareAt(t *testing.T, lines []string, idx []int, want []uint32) {
	t.Helper()
	if len(idx) == 0 {
		t.Fatal("no comparisons generated")
	}
	got, err := runAsm(lines)
	if err != nil {
		t.Fatal(err)
	}
	for k, i := range idx {
		if len(got[i]) != 1 {
			t.Fatalf("%q assembled to %d instructions, want 1",
				strings.TrimSpace(lines[i]), len(got[i]))
		}
		comparisons.Add(1)
		if got[i][0] != want[k] {
			t.Fatalf("%s: go tool asm %#08x, encoder %#08x",
				strings.TrimSpace(lines[i]), got[i][0], want[k])
		}
	}
	t.Logf("%d encodings compared", len(idx))
}

// branchSweep builds a block that sweeps back count instructions to a label at
// the top and forward count instructions to a label at the bottom. emit writes
// one assembly line for a branch to the named label and returns the encoding
// this package produces for the resulting byte offset.
func branchSweep(t *testing.T, back, fwd int, emit func(label string, off int64) (string, uint32)) {
	t.Helper()
	var lines []string
	var idx []int
	var want []uint32
	lines = append(lines, "start:")
	for i := 0; i < back; i++ {
		text, v := emit("start", int64(-4*i))
		idx = append(idx, len(lines))
		lines = append(lines, text)
		want = append(want, v)
	}
	for j := 0; j < fwd; j++ {
		text, v := emit("end", int64(4*(fwd-j)))
		idx = append(idx, len(lines))
		lines = append(lines, text)
		want = append(want, v)
	}
	lines = append(lines, "end:")
	compareAt(t, lines, idx, want)
}

func mustEnc(t *testing.T, v uint32, ok bool, what string) uint32 {
	t.Helper()
	if !ok {
		t.Fatalf("%s rejected", what)
	}
	return v
}

func TestDiffBranchUnconditional(t *testing.T) {
	needAsm(t)
	const n = 4096
	t.Run("B", func(t *testing.T) {
		branchSweep(t, n, n, func(label string, off int64) (string, uint32) {
			v, ok := B(off)
			return "\tJMP\t" + label, mustEnc(t, v, ok, fmt.Sprintf("B %d", off))
		})
	})
	t.Run("BL", func(t *testing.T) {
		branchSweep(t, n, n, func(label string, off int64) (string, uint32) {
			v, ok := Bl(off)
			return "\tBL\t" + label, mustEnc(t, v, ok, fmt.Sprintf("BL %d", off))
		})
	})
}

// condNamesP9 is the Plan 9 spelling of each condition that has one. AL and NV
// have no conditional branch mnemonic, so they are covered by
// TestBranchFieldLayout.
var condNamesP9 = map[Cond]string{
	EQ: "BEQ", NE: "BNE", HS: "BHS", LO: "BLO", MI: "BMI", PL: "BPL",
	VS: "BVS", VC: "BVC", HI: "BHI", LS: "BLS", GE: "BGE", LT: "BLT",
	GT: "BGT", LE: "BLE",
}

func TestDiffBranchConditional(t *testing.T) {
	needAsm(t)
	const n = 2048
	t.Run("offset", func(t *testing.T) {
		branchSweep(t, n, n, func(label string, off int64) (string, uint32) {
			v, ok := BCond(EQ, off)
			return "\tBEQ\t" + label, mustEnc(t, v, ok, fmt.Sprintf("BEQ %d", off))
		})
	})
	t.Run("condition", func(t *testing.T) {
		var lines []string
		var idx []int
		var want []uint32
		lines = append(lines, "start:")
		for c := EQ; c < numCond; c++ {
			name, spelled := condNamesP9[c]
			if !spelled {
				continue
			}
			for i := 0; i < 8; i++ {
				off := int64(-4 * len(idx))
				idx = append(idx, len(lines))
				lines = append(lines, "\t"+name+"\tstart")
				v, ok := BCond(c, off)
				want = append(want, mustEnc(t, v, ok, name))
			}
		}
		compareAt(t, lines, idx, want)
	})
}

func TestDiffCompareAndBranch(t *testing.T) {
	needAsm(t)
	const n = 2048
	for _, sz := range []Size{Size64, Size32} {
		for _, neg := range []bool{false, true} {
			name := mnemonic("CBZ", sz)
			enc := Cbz
			if neg {
				name = mnemonic("CBNZ", sz)
				enc = Cbnz
			}
			t.Run(name, func(t *testing.T) {
				branchSweep(t, n, n, func(label string, off int64) (string, uint32) {
					v, ok := enc(sz, R1, off)
					return fmt.Sprintf("\t%s\tR1, %s", name, label),
						mustEnc(t, v, ok, name)
				})
			})
		}
	}
	// The register field.
	var lines []string
	var idx []int
	var want []uint32
	lines = append(lines, "start:")
	for _, r := range sweepRegs() {
		off := int64(-4 * len(idx))
		idx = append(idx, len(lines))
		lines = append(lines, fmt.Sprintf("\tCBZ\t%s, start", r))
		v, ok := Cbz(Size64, r, off)
		want = append(want, mustEnc(t, v, ok, "CBZ"))
	}
	compareAt(t, lines, idx, want)
}

// TestDiffTestAndBranch sweeps the whole 14-bit range. It is the shortest
// branch on the target, so the whole range is 16384 instructions and fits in
// one assembly file.
func TestDiffTestAndBranch(t *testing.T) {
	needAsm(t)
	for _, neg := range []bool{false, true} {
		name, enc := "TBZ", Tbz
		if neg {
			name, enc = "TBNZ", Tbnz
		}
		t.Run(name, func(t *testing.T) {
			// 8188 instructions in each direction sweeps to plus and minus
			// 32752 bytes. go tool asm keeps a small margin below the true
			// 14-bit limit and rewrites a branch past it into a test and a
			// long branch, so the last few offsets of the range are covered by
			// TestBranchFieldLayout instead.
			branchSweep(t, 8188, 8188, func(label string, off int64) (string, uint32) {
				v, ok := enc(R1, 5, off)
				return fmt.Sprintf("\t%s\t$5, R1, %s", name, label),
					mustEnc(t, v, ok, fmt.Sprintf("%s %d", name, off))
			})
		})
	}
	// The bit number field, which is split across bit 31 and bits 23:19.
	var lines []string
	var idx []int
	var want []uint32
	lines = append(lines, "start:")
	for bit := uint32(0); bit < 64; bit++ {
		for _, r := range []Reg{R1, R25, ZR} {
			off := int64(-4 * len(idx))
			idx = append(idx, len(lines))
			lines = append(lines, fmt.Sprintf("\tTBNZ\t$%d, %s, start", bit, r))
			v, ok := Tbnz(r, bit, off)
			want = append(want, mustEnc(t, v, ok, "TBNZ"))
		}
	}
	compareAt(t, lines, idx, want)
}

func TestDiffBranchRegister(t *testing.T) {
	needAsm(t)
	var lines []string
	var want []uint32
	for _, r := range sweepRegs() {
		lines = append(lines, fmt.Sprintf("\tJMP\t(%s)", r))
		want = append(want, Br(r))
		lines = append(lines, fmt.Sprintf("\tCALL\t(%s)", r))
		want = append(want, Blr(r))
		lines = append(lines, fmt.Sprintf("\tRET\t(%s)", r))
		want = append(want, Ret(r))
	}
	compare(t, lines, want)
}

// TestBranchFieldLayout covers the offsets the assembler cannot be asked for:
// the extremes of the 26-bit and 19-bit ranges, which would need an assembly
// file of 33 million instructions to reach through a label, the last few
// offsets of the 14-bit range, which go tool asm expands rather than emits,
// and the two unconditional condition codes.
//
// It recovers the offset from the encoding rather than restating the encoder's
// expression, so a shifted or truncated field fails here.

// signExtend returns the low width bits of v as a signed value.
func signExtend(v uint32, width uint) int64 {
	return int64(int32(v<<(32-width))) >> (32 - width)
}

func TestBranchFieldLayout(t *testing.T) {
	branchOffsets := []int64{MinBranchOffset, MinBranchOffset + 4, -8, -4, 0, 4, 8,
		MaxBranchOffset - 4, MaxBranchOffset}
	for _, off := range branchOffsets {
		for _, c := range []struct {
			name string
			enc  func(int64) (uint32, bool)
			op   uint32
		}{{"B", B, 0x14000000}, {"BL", Bl, 0x94000000}} {
			v, ok := c.enc(off)
			if !ok {
				t.Fatalf("%s %d rejected", c.name, off)
			}
			if v&0xfc000000 != c.op {
				t.Errorf("%s %d = %#08x, wrong instruction class", c.name, off, v)
			}
			if got := signExtend(v&0x03ffffff, 26) * 4; got != off {
				t.Errorf("%s %d encodes offset %d", c.name, off, got)
			}
		}
	}

	condOffsets := []int64{MinCondBranchOffset, MinCondBranchOffset + 4, -4, 0, 4,
		MaxCondBranchOffset - 4, MaxCondBranchOffset}
	for _, off := range condOffsets {
		for c := EQ; c < numCond; c++ {
			v, ok := BCond(c, off)
			if !ok {
				t.Fatalf("B.%v %d rejected", c, off)
			}
			if v&0xff000010 != 0x54000000 {
				t.Errorf("B.%v %d = %#08x, wrong instruction class", c, off, v)
			}
			if got := Cond(v & 0xf); got != c {
				t.Errorf("B.%v %d encodes condition %v", c, off, got)
			}
			if got := signExtend((v>>5)&0x7ffff, 19) * 4; got != off {
				t.Errorf("B.%v %d encodes offset %d", c, off, got)
			}
		}
		for _, c := range []struct {
			name string
			sz   Size
			enc  func(Size, Reg, int64) (uint32, bool)
			op   uint32
		}{
			{"CBZ", Size64, Cbz, 0xb4000000},
			{"CBNZ", Size64, Cbnz, 0xb5000000},
			{"CBZW", Size32, Cbz, 0x34000000},
			{"CBNZW", Size32, Cbnz, 0x35000000},
		} {
			v, ok := c.enc(c.sz, R7, off)
			if !ok {
				t.Fatalf("%s %d rejected", c.name, off)
			}
			if v&0xff00001f != c.op|7 {
				t.Errorf("%s %d = %#08x, wrong class or register", c.name, off, v)
			}
			if got := signExtend((v>>5)&0x7ffff, 19) * 4; got != off {
				t.Errorf("%s %d encodes offset %d", c.name, off, got)
			}
		}
	}

	testOffsets := []int64{MinTestBranchOffset, MinTestBranchOffset + 4, -4, 0, 4,
		MaxTestBranchOffset - 4, MaxTestBranchOffset}
	for _, off := range testOffsets {
		for bit := uint32(0); bit < 64; bit++ {
			for _, c := range []struct {
				name string
				enc  func(Reg, uint32, int64) (uint32, bool)
				op   uint32
			}{{"TBZ", Tbz, 0x36000000}, {"TBNZ", Tbnz, 0x37000000}} {
				v, ok := c.enc(R7, bit, off)
				if !ok {
					t.Fatalf("%s bit %d offset %d rejected", c.name, bit, off)
				}
				if v&0x7f00001f != c.op|7 {
					t.Errorf("%s bit %d offset %d = %#08x, wrong class or register",
						c.name, bit, off, v)
				}
				// The bit number is split: bit 31 carries its top bit and bits
				// 23:19 the rest.
				if got := (v>>31)<<5 | (v>>19)&31; got != bit {
					t.Errorf("%s bit %d offset %d encodes bit %d", c.name, bit, off, got)
				}
				if got := signExtend((v>>5)&0x3fff, 14) * 4; got != off {
					t.Errorf("%s bit %d offset %d encodes offset %d", c.name, bit, off, got)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// PC-relative addresses

func TestDiffAdrpAdd(t *testing.T) {
	needAsm(t)
	var lines []string
	for _, r := range AllocatableRegs() {
		lines = append(lines, fmt.Sprintf("\tMOVD\t$·f(SB), %s", r))
	}
	got, err := runAsm(lines)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for i, r := range AllocatableRegs() {
		if len(got[i]) != 2 {
			t.Fatalf("%s produced %d instructions, want the ADRP and ADD pair",
				strings.TrimSpace(lines[i]), len(got[i]))
		}
		// The assembler emits the pair with both immediates zero and leaves a
		// relocation for the linker, which is what nanogo does too.
		hi, lo, ok := AdrpAdd(r, 0, 0)
		if !ok {
			t.Fatalf("AdrpAdd(%v, 0, 0) rejected", r)
		}
		comparisons.Add(2)
		if got[i][0] != hi || got[i][1] != lo {
			t.Fatalf("%s: go tool asm %#08x %#08x, encoder %#08x %#08x",
				strings.TrimSpace(lines[i]), got[i][0], got[i][1], hi, lo)
		}
		n += 2
	}
	t.Logf("%d encodings compared", n)
}

// TestAdrpImmediateSplit checks the 21-bit page offset, which the encoding
// splits into immlo at bits 30:29 and immhi at bits 23:5. go tool asm always
// emits it as zero and lets the linker fill it in, so the split needs its own
// check.
func TestAdrpImmediateSplit(t *testing.T) {
	for _, pages := range []int64{0, 1, 2, 3, 4, -1, -2, 1 << 19, -(1 << 19),
		1<<20 - 1, -(1 << 20)} {
		delta := pages * 4096
		v, ok := Adrp(R5, delta)
		if !ok {
			t.Fatalf("Adrp %d pages rejected", pages)
		}
		imm := (v>>29)&3 | (v>>5&0x7ffff)<<2
		got := int64(int32(imm<<11) >> 11) // sign-extend the 21-bit field
		if got != pages {
			t.Errorf("Adrp %d pages encoded %d", pages, got)
		}
		if v&0x1f != 5 || v&0x9f000000 != 0x90000000 {
			t.Errorf("Adrp %d pages = %#08x, wrong class or destination", pages, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Range rejection
//
// Every immediate form is tested one past its limit. An encoder that truncates
// instead of reporting produces an instruction that runs and computes the
// wrong answer, which is the failure mode specs/041 says the bool return
// exists to prevent.

func TestRejectOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		fits func() bool
		want bool
	}{
		{"ADD at the limit", func() bool { _, ok := AddRegImm(Size64, R3, R2, MaxAddSubImm); return ok }, true},
		{"ADD one past the unshifted limit, which the LSL 12 form reaches", func() bool {
			_, ok := AddRegImm(Size64, R3, R2, MaxAddSubImm+1)
			return ok
		}, true},
		{"ADD past the unshifted limit and not a page multiple", func() bool {
			_, ok := AddRegImm(Size64, R3, R2, MaxAddSubImm+2)
			return ok
		}, false},
		{"ADD at the shifted limit", func() bool {
			_, ok := AddRegImm(Size64, R3, R2, MaxAddSubImmShifted)
			return ok
		}, true},
		{"ADD one page past the shifted limit", func() bool {
			_, ok := AddRegImm(Size64, R3, R2, MaxAddSubImmShifted+4096)
			return ok
		}, false},
		{"ADD negative", func() bool { _, ok := AddRegImm(Size64, R3, R2, -1); return ok }, false},
		{"SUB one past", func() bool {
			_, ok := SubRegImm(Size64, R3, R2, MaxAddSubImmShifted+4096)
			return ok
		}, false},
		{"ADDS one past", func() bool { _, ok := AddsRegImm(Size64, R3, R2, MaxAddSubImm+2); return ok }, false},
		{"SUBS one past", func() bool { _, ok := SubsRegImm(Size64, R3, R2, MaxAddSubImm+2); return ok }, false},
		{"CMP one past", func() bool { _, ok := CmpRegImm(Size64, R2, MaxAddSubImm+2); return ok }, false},
		{"CMN one past", func() bool { _, ok := CmnRegImm(Size64, R2, MaxAddSubImm+2); return ok }, false},

		{"shifted register at the limit", func() bool {
			_, ok := AddRegRegShift(Size64, R3, R2, R1, LSL, 63)
			return ok
		}, true},
		{"shifted register one past", func() bool {
			_, ok := AddRegRegShift(Size64, R3, R2, R1, LSL, 64)
			return ok
		}, false},
		{"shifted register one past at 32 bits", func() bool {
			_, ok := AddRegRegShift(Size32, R3, R2, R1, LSL, 32)
			return ok
		}, false},
		{"add cannot rotate", func() bool {
			_, ok := AddRegRegShift(Size64, R3, R2, R1, ROR, 1)
			return ok
		}, false},
		{"sub cannot rotate", func() bool {
			_, ok := SubRegRegShift(Size64, R3, R2, R1, ROR, 1)
			return ok
		}, false},
		{"logical can rotate", func() bool {
			_, ok := AndRegRegShift(Size64, R3, R2, R1, ROR, 1)
			return ok
		}, true},
		{"logical shift one past", func() bool {
			_, ok := OrrRegRegShift(Size64, R3, R2, R1, LSR, 64)
			return ok
		}, false},
		{"invalid shift kind", func() bool {
			_, ok := EorRegRegShift(Size64, R3, R2, R1, numShift, 0)
			return ok
		}, false},
		{"bic shift one past", func() bool {
			_, ok := BicRegRegShift(Size64, R3, R2, R1, LSL, 64)
			return ok
		}, false},

		{"logical immediate not representable", func() bool {
			_, ok := AndRegImm(Size64, R3, R2, 0xb)
			return ok
		}, false},
		{"logical immediate representable", func() bool { _, ok := AndRegImm(Size64, R3, R2, 0xf); return ok }, true},
		{"ORR immediate not representable", func() bool { _, ok := OrrRegImm(Size64, R3, R2, 5); return ok }, false},
		{"EOR immediate not representable", func() bool { _, ok := EorRegImm(Size64, R3, R2, 5); return ok }, false},
		{"ANDS immediate not representable", func() bool { _, ok := AndsRegImm(Size64, R3, R2, 5); return ok }, false},
		{"TST immediate not representable", func() bool { _, ok := TstRegImm(Size64, R2, 5); return ok }, false},
		{"MOV logical immediate not representable", func() bool {
			_, ok := MovLogicalImm(Size64, R3, 5)
			return ok
		}, false},
		{"BIC immediate whose complement is not representable", func() bool {
			_, ok := BicRegImm(Size64, R3, R2, ^uint64(5))
			return ok
		}, false},
		{"BIC immediate whose complement is representable", func() bool {
			_, ok := BicRegImm(Size64, R3, R2, ^uint64(0xf))
			return ok
		}, true},
		{"BIC 32-bit complement stays in 32 bits", func() bool {
			_, ok := BicRegImm(Size32, R3, R2, 0xfffffff0)
			return ok
		}, true},

		{"MOVZ at the top halfword", func() bool { _, ok := Movz(Size64, R3, 1, 48); return ok }, true},
		{"MOVZ one halfword past", func() bool { _, ok := Movz(Size64, R3, 1, 64); return ok }, false},
		{"MOVZ 32-bit one halfword past", func() bool { _, ok := Movz(Size32, R3, 1, 32); return ok }, false},
		{"MOVK unaligned shift", func() bool { _, ok := Movk(Size64, R3, 1, 8); return ok }, false},
		{"MOVN unaligned shift", func() bool { _, ok := Movn(Size64, R3, 1, 24); return ok }, false},

		{"LSL at the limit", func() bool { _, ok := LslRegImm(Size64, R3, R2, 63); return ok }, true},
		{"LSL one past", func() bool { _, ok := LslRegImm(Size64, R3, R2, 64); return ok }, false},
		{"LSL 32-bit one past", func() bool { _, ok := LslRegImm(Size32, R3, R2, 32); return ok }, false},
		{"LSR one past", func() bool { _, ok := LsrRegImm(Size64, R3, R2, 64); return ok }, false},
		{"ASR one past", func() bool { _, ok := AsrRegImm(Size64, R3, R2, 64); return ok }, false},
		{"ROR one past", func() bool { _, ok := RorRegImm(Size64, R3, R2, 64); return ok }, false},

		{"load at the scaled limit", func() bool {
			_, ok := MemUnsignedOffset(LoadX, R2, R1, MaxMemOffsetScaled*8)
			return ok
		}, true},
		{"load one unit past the scaled limit", func() bool {
			_, ok := MemUnsignedOffset(LoadX, R2, R1, (MaxMemOffsetScaled+1)*8)
			return ok
		}, false},
		{"load not a multiple of the access size", func() bool {
			_, ok := MemUnsignedOffset(LoadX, R2, R1, 4)
			return ok
		}, false},
		{"load negative", func() bool { _, ok := MemUnsignedOffset(LoadX, R2, R1, -8); return ok }, false},
		{"byte load at the scaled limit", func() bool {
			_, ok := MemUnsignedOffset(LoadBU, R2, R1, MaxMemOffsetScaled)
			return ok
		}, true},
		{"byte load one past", func() bool {
			_, ok := MemUnsignedOffset(LoadBU, R2, R1, MaxMemOffsetScaled+1)
			return ok
		}, false},
		{"unscaled at the low limit", func() bool {
			_, ok := MemUnscaled(LoadX, R2, R1, MinMemOffsetUnscaled)
			return ok
		}, true},
		{"unscaled one below", func() bool {
			_, ok := MemUnscaled(LoadX, R2, R1, MinMemOffsetUnscaled-1)
			return ok
		}, false},
		{"unscaled at the high limit", func() bool {
			_, ok := MemUnscaled(StoreX, R2, R1, MaxMemOffsetUnscaled)
			return ok
		}, true},
		{"unscaled one above", func() bool {
			_, ok := MemUnscaled(StoreX, R2, R1, MaxMemOffsetUnscaled+1)
			return ok
		}, false},
		{"pre-index one below", func() bool {
			_, ok := MemPreIndex(StoreX, R2, R1, MinMemOffsetUnscaled-1)
			return ok
		}, false},
		{"post-index one above", func() bool {
			_, ok := MemPostIndex(LoadX, R2, R1, MaxMemOffsetUnscaled+1)
			return ok
		}, false},
		{"register offset with a byte extend", func() bool {
			_, ok := MemRegOffset(LoadX, R3, R1, R2, Extend(0), false)
			return ok
		}, false},
		{"register offset with a halfword extend", func() bool {
			_, ok := MemRegOffset(LoadX, R3, R1, R2, Extend(5), false)
			return ok
		}, false},

		{"B at the limit", func() bool { _, ok := B(MaxBranchOffset); return ok }, true},
		{"B one instruction past", func() bool { _, ok := B(MaxBranchOffset + 4); return ok }, false},
		{"B at the low limit", func() bool { _, ok := B(MinBranchOffset); return ok }, true},
		{"B one instruction below", func() bool { _, ok := B(MinBranchOffset - 4); return ok }, false},
		{"B unaligned", func() bool { _, ok := B(2); return ok }, false},
		{"BL one instruction past", func() bool { _, ok := Bl(MaxBranchOffset + 4); return ok }, false},
		{"B.cond at the limit", func() bool { _, ok := BCond(EQ, MaxCondBranchOffset); return ok }, true},
		{"B.cond one instruction past", func() bool { _, ok := BCond(EQ, MaxCondBranchOffset+4); return ok }, false},
		{"B.cond one instruction below", func() bool { _, ok := BCond(EQ, MinCondBranchOffset-4); return ok }, false},
		{"B.cond unaligned", func() bool { _, ok := BCond(EQ, 6); return ok }, false},
		{"B.cond invalid condition", func() bool { _, ok := BCond(numCond, 0); return ok }, false},
		{"CBZ one instruction past", func() bool { _, ok := Cbz(Size64, R1, MaxCondBranchOffset+4); return ok }, false},
		{"CBNZ one instruction below", func() bool {
			_, ok := Cbnz(Size32, R1, MinCondBranchOffset-4)
			return ok
		}, false},
		{"TBZ at the limit", func() bool { _, ok := Tbz(R1, 0, MaxTestBranchOffset); return ok }, true},
		{"TBZ one instruction past", func() bool { _, ok := Tbz(R1, 0, MaxTestBranchOffset+4); return ok }, false},
		{"TBNZ one instruction below", func() bool {
			_, ok := Tbnz(R1, 0, MinTestBranchOffset-4)
			return ok
		}, false},
		{"TBZ at the top bit", func() bool { _, ok := Tbz(R1, 63, 0); return ok }, true},
		{"TBZ one bit past", func() bool { _, ok := Tbz(R1, 64, 0); return ok }, false},

		{"ADRP at the limit", func() bool { _, ok := Adrp(R1, MaxAdrpDelta); return ok }, true},
		{"ADRP one page past", func() bool { _, ok := Adrp(R1, MaxAdrpDelta+4096); return ok }, false},
		{"ADRP at the low limit", func() bool { _, ok := Adrp(R1, MinAdrpDelta); return ok }, true},
		{"ADRP one page below", func() bool { _, ok := Adrp(R1, MinAdrpDelta-4096); return ok }, false},
		{"ADRP not a page multiple", func() bool { _, ok := Adrp(R1, 1); return ok }, false},
		{"ADRP and ADD at the page limit", func() bool { _, _, ok := AdrpAdd(R1, 0, 4095); return ok }, true},
		{"ADRP and ADD one past the page", func() bool { _, _, ok := AdrpAdd(R1, 0, 4096); return ok }, false},
		{"ADRP and ADD negative page offset", func() bool { _, _, ok := AdrpAdd(R1, 0, -1); return ok }, false},
		{"ADRP and ADD out of page range", func() bool {
			_, _, ok := AdrpAdd(R1, MaxAdrpDelta+4096, 0)
			return ok
		}, false},
	}
	for _, c := range cases {
		if got := c.fits(); got != c.want {
			t.Errorf("%s: fits=%v, want %v", c.name, got, c.want)
		}
	}
}

// TestRegisterMisuseIsCaught checks the forms that panic. A Reg is an enum, so
// naming the stack pointer where the encoding means the zero register is a bug
// in the caller's code and not a value that came from a Go program.
func TestRegisterMisuseIsCaught(t *testing.T) {
	cases := []struct {
		name string
		call func()
	}{
		{"RSP where the class means ZR", func() { AddRegReg(Size64, R3, R2, RSP) }},
		{"ZR where the class means SP", func() { AddRegImm(Size64, R3, ZR, 1) }},
		{"RSP as a load transfer register", func() { MemUnsignedOffset(LoadX, RSP, R1, 0) }},
		{"ZR as a load base", func() { MemUnsignedOffset(LoadX, R2, ZR, 0) }},
		{"invalid register", func() { MovRegReg(Size64, Reg(200), R1) }},
		{"invalid register as a base", func() { MemUnsignedOffset(LoadX, R2, Reg(200), 0) }},
		{"invalid memory op", func() { MemUnsignedOffset(numMemOp, R2, R1, 0) }},
		{"invalid memory op scale", func() { numMemOp.Scale() }},
		{"MovConst with no room", func() { MovConst(Size64, R3, 1, nil) }},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", c.name)
				}
			}()
			c.call()
		}()
	}
}

// TestDiffAddSubExtendedRegister covers the class that can name the stack
// pointer. specs/042's prologue needs it: CMP R16, RSP has no encoding in the
// shifted-register class, where register 31 reads as the zero register.
func TestDiffAddSubExtendedRegister(t *testing.T) {
	needAsm(t)
	forms := []struct {
		name  string
		enc   func(sz Size, dst, a, b Reg) uint32
		spDst bool
	}{
		{"ADD", AddRegRegSP, true},
		{"SUB", SubRegRegSP, true},
		{"ADDS", AddsRegRegSP, false},
		{"SUBS", SubsRegRegSP, false},
	}
	var lines []string
	var want []uint32
	sources := append(AllocatableRegs(), RSP)
	for _, f := range forms {
		bothSizes(func(sz Size) {
			dsts := AllocatableRegs()
			if f.spDst {
				dsts = append(dsts, RSP)
			}
			for _, dst := range dsts {
				for _, b := range AllocatableRegs() {
					lines = append(lines, fmt.Sprintf("\t%s\t%s, RSP, %s",
						mnemonic(f.name, sz), b, dst))
					want = append(want, f.enc(sz, dst, RSP, b))
				}
			}
			// Only with the stack pointer as the first source. With two
			// general registers the assembler picks the shifted-register
			// class, which is the canonical encoding for that operand pair,
			// so a comparison there would be a disagreement about choice and
			// not about encoding.
			_ = sources
		})
	}
	for _, b := range AllocatableRegs() {
		bothSizes(func(sz Size) {
			lines = append(lines, fmt.Sprintf("\t%s\t%s, RSP", mnemonic("CMP", sz), b))
			want = append(want, CmpRegRegSP(sz, RSP, b))
			lines = append(lines, fmt.Sprintf("\t%s\t%s, RSP", mnemonic("CMN", sz), b))
			want = append(want, CmnRegRegSP(sz, RSP, b))
		})
	}
	compare(t, lines, want)

	// The same class with a general register as the first source is legal and
	// unreachable through the assembler, so its fields are checked directly.
	v := AddRegRegSP(Size64, R3, R2, R1)
	if want := uint32(0x8b200000 | 1<<16 | 3<<13 | 2<<5 | 3); v != want {
		t.Errorf("AddRegRegSP(64, R3, R2, R1) = %#08x, want %#08x", v, want)
	}
	v = AddRegRegSP(Size32, R3, R2, R1)
	if want := uint32(0x0b200000 | 1<<16 | 2<<13 | 2<<5 | 3); v != want {
		t.Errorf("AddRegRegSP(32, R3, R2, R1) = %#08x, want %#08x", v, want)
	}
}

// TestPrologueIsEncodable walks the prologue of specs/042-arm64-backend.md.
// Every line of it has to have an encoder, or the first function nanogo
// compiles cannot be emitted.
func TestPrologueIsEncodable(t *testing.T) {
	const frame = 32
	steps := []struct {
		what string
		fits bool
	}{
		{"MOVD 16(g), R16", func() bool { _, ok := MemUnsignedOffset(LoadX, R16, R28, 16); return ok }()},
		{"CMP R16, RSP", func() bool { CmpRegRegSP(Size64, RSP, R16); return true }()},
		{"BLS growstack", func() bool { _, ok := BCond(LS, 64); return ok }()},
		{"MOVD.W R30, -frame(RSP)", func() bool { _, ok := MemPreIndex(StoreX, R30, RSP, -frame); return ok }()},
		{"MOVD R29, -8(RSP)", func() bool { _, ok := MemUnscaled(StoreX, R29, RSP, -8); return ok }()},
		{"SUB $8, RSP, R29", func() bool { _, ok := SubRegImm(Size64, R29, RSP, 8); return ok }()},
		{"MOVD -8(RSP), R29", func() bool { _, ok := MemUnscaled(LoadX, R29, RSP, -8); return ok }()},
		{"MOVD.P frame(RSP), R30", func() bool { _, ok := MemPostIndex(LoadX, R30, RSP, frame); return ok }()},
		{"RET (R30)", func() bool { Ret(R30); return true }()},
	}
	for _, s := range steps {
		if !s.fits {
			t.Errorf("%s has no encoding", s.what)
		}
	}
}
