// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// The prologue is compared against go tool asm, instruction for instruction,
// at every frame size class. specs/042-arm64-backend.md gives one listing and
// there are four: the check has three forms and the frame push has two, and
// the listing is the smallest of each. A form that assembles to something else
// is a function that corrupts its caller's frame, which no unit test of this
// package would notice.

package ssagen

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/obj/arm64"
	"golang.design/x/nanogo/ssa"
)

// synth emits the prologue, the epilogue and the growth tail of a function
// with the given frame, with no body between them.
func synth(t *testing.T, size int64, leaf bool, in []place) []uint32 {
	t.Helper()
	return synthEmitter(t, size, leaf, in).text
}

// synthNoSplit is synth for a function that carries //go:nosplit.
func synthNoSplit(t *testing.T, size int64, leaf bool, in []place) []uint32 {
	t.Helper()
	return synthOpt(t, size, leaf, in, true).text
}

// synthEmitter is synth with the emitter kept, for the tables it filled.
func synthEmitter(t *testing.T, size int64, leaf bool, in []place) *emitter {
	t.Helper()
	return synthOpt(t, size, leaf, in, false)
}

// synthOpt is synthEmitter with the //go:nosplit answer given rather than
// derived, which is the distinction Options.NoSplit records.
func synthOpt(t *testing.T, size int64, leaf bool, in []place, nosplit bool) *emitter {
	t.Helper()
	e := &emitter{opt: Options{Sym: "test.f", NoSplit: nosplit}, syms: newSymbols(obj.NewPackage("test"))}
	e.frame = frame{size: size, leaf: leaf, in: in}
	// The rule is read from the emitter rather than restated, so a test that
	// asks for //go:nosplit measures what layout would decide.
	e.frame.nosplit = e.frame.skipsCheck(nosplit)
	e.markSP() // the row run() writes before the prologue
	e.prologue()
	e.epilogue()
	e.growstack()
	e.patch()
	if err := e.err(); err != nil {
		t.Fatalf("frame %d: %v", size, err)
	}
	return e
}

// asmText assembles a listing and returns the words of the text symbol.
//
// The TEXT flags are NOSPLIT|NOFRAME, spelled 516 so that the file needs no
// include. Any other flags give the function a prologue of the assembler's
// own, and the comparison would then be against that prologue rather than
// against the instruction under test.
func asmText(t *testing.T, lines []string) []uint32 {
	t.Helper()
	goCmd := goTool(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "t.s")
	body := "TEXT ·f(SB), 516, $0-0\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "t.o")
	cmd := exec.Command(goCmd, "tool", "asm", "-p", "test", "-S", "-o", out, src)
	// The assembler reads GOARCH from the environment and assembles for the
	// host by default, so on any other architecture it rejects an arm64
	// listing and the comparison never happens. nanogo emits arm64 whatever
	// the host is, so the oracle has to be told the same thing.
	//
	// Pinning it rather than skipping is the better answer here: the encoding
	// this test checks does not depend on the host, so there is no reason for
	// the check to run on one machine and not another.
	cmd.Env = append(os.Environ(), "GOARCH=arm64", "GOOS=darwin")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool asm rejected the listing: %v\n%s\n%s", err, body, b)
	}
	return parseAsmText(t, string(b))
}

var (
	asmHeadRe = regexp.MustCompile(`^test\.f STEXT`)
	// A byte dump line ends with two spaces and the printable form of the
	// bytes. Without that anchor an instruction line, which also starts with
	// an offset in hexadecimal, parses as one byte of data.
	asmDataRe = regexp.MustCompile(`^\t0x([0-9a-f]+)((?: [0-9a-f]{2}){1,16})  `)
)

// parseAsmText reads the byte dump out of a go tool asm -S listing.
func parseAsmText(t *testing.T, out string) []uint32 {
	t.Helper()
	var raw []byte
	seen := false
	for _, line := range strings.Split(out, "\n") {
		if asmHeadRe.MatchString(line) {
			seen = true
			continue
		}
		if !seen {
			continue
		}
		m := asmDataRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		off, err := strconv.ParseInt(m[1], 16, 64)
		if err != nil || off != int64(len(raw)) {
			continue
		}
		for _, f := range strings.Fields(m[2]) {
			v, err := strconv.ParseUint(f, 16, 8)
			if err != nil {
				t.Fatalf("byte %q in the listing", f)
			}
			raw = append(raw, byte(v))
		}
	}
	if len(raw) == 0 {
		t.Fatalf("the listing held no text:\n%s", out)
	}
	words := make([]uint32, 0, len(raw)/4)
	for i := 0; i+4 <= len(raw); i += 4 {
		words = append(words, binary.LittleEndian.Uint32(raw[i:]))
	}
	return words
}

// prologueListing writes the assembly that a frame of the given size needs.
//
// It is specs/042-arm64-backend.md's listing where that listing covers the
// case, and gc's where it does not: the comparison against the guard has three
// forms and the frame push has two, and specs/042 states the smallest of each.
func prologueListing(size int64, leaf bool, spill, reload []string) []string {
	var l []string
	nosplit := size == 0 || (leaf && size < stackSmall)
	if !nosplit {
		l = append(l, "\tMOVD\t16(g), R16")
		switch {
		case size <= stackSmall:
			l = append(l, "\tCMP\tR16, RSP")
		case size <= stackBig:
			l = append(l,
				fmt.Sprintf("\tSUB\t$%d, RSP, R17", size-stackSmall),
				"\tCMP\tR16, R17")
		default:
			l = append(l,
				fmt.Sprintf("\tSUBS\t$%d, RSP, R17", size-stackSmall),
				"\tBLO\tgrow",
				"\tCMP\tR16, R17")
		}
		l = append(l, "\tBLS\tgrow")
	}
	if size != 0 {
		if size <= maxPreIndex {
			l = append(l,
				fmt.Sprintf("\tMOVD.W\tR30, %d(RSP)", -size),
				"\tMOVD\tR29, -8(RSP)")
		} else {
			l = append(l,
				fmt.Sprintf("\tSUB\t$%d, RSP, R20", size),
				"\tMOVD\tR29, -8(R20)",
				"\tMOVD\tR30, (R20)",
				"\tMOVD\tR20, RSP")
		}
		l = append(l, "\tSUB\t$8, RSP, R29")
	}
	// The epilogue.
	if size != 0 {
		l = append(l, "\tMOVD\t-8(RSP), R29")
		if size <= maxPreIndex {
			l = append(l, fmt.Sprintf("\tMOVD.P\t%d(RSP), R30", size))
		} else {
			l = append(l,
				"\tMOVD\t(RSP), R30",
				fmt.Sprintf("\tADD\t$%d, RSP, RSP", size))
		}
	}
	l = append(l, "\tRET\t(R30)")
	if nosplit {
		return l
	}
	l = append(l, "grow:")
	l = append(l, spill...)
	l = append(l,
		"\tMOVD\tR30, R3",
		"\tCALL\truntime·morestack_noctxt(SB)")
	// The reload of the saved arguments. The jump back to the first
	// instruction is not in the listing, because a label there is not
	// something the assembler accepts.
	return append(l, reload...)
}

// TestPrologueMatchesTheAssembler is the differential test of
// specs/042-arm64-backend.md's listing.
//
// The sizes cover every form: no frame at all, a frame the pre-indexed store
// reaches, one past it, a frame past the guaranteed region below the stack
// guard, and one past the bound where the subtraction can underflow.
func TestPrologueMatchesTheAssembler(t *testing.T) {
	sizes := []struct {
		size int64
		leaf bool
		why  string
	}{
		{0, true, "a leaf with no frame keeps the caller's stack pointer"},
		{16, true, "a leaf below the guaranteed region skips the check"},
		{16, false, "the smallest frame with a check"},
		{128, false, "the largest frame the stack pointer alone tests"},
		{144, false, "past the guaranteed region, so the check subtracts first"},
		{240, false, "the largest frame one pre-indexed store pushes"},
		{256, false, "past it, so the stack pointer is computed in a register"},
		{4096, false, "the largest frame that cannot underflow"},
		{4112, false, "past it, so the subtraction sets the flags"},
	}
	n := 0
	for _, s := range sizes {
		name := fmt.Sprintf("size%d", s.size)
		if s.leaf {
			name += "leaf"
		}
		t.Run(name, func(t *testing.T) {
			got := synth(t, s.size, s.leaf, nil)
			want := asmText(t, prologueListing(s.size, s.leaf, nil, nil))
			// The listing stops before the jump back to the first
			// instruction, because a label at the top of a TEXT is not
			// something the assembler accepts. It is the last instruction and
			// it is checked here.
			if len(got) == 0 {
				t.Fatal("nothing was emitted")
			}
			if !(s.size == 0 || (s.leaf && s.size < stackSmall)) {
				last := got[len(got)-1]
				if want2, ok := arm64.B(-int64(len(got)-1) * 4); !ok || last != want2 {
					t.Errorf("the last instruction is %#08x, and the jump back to the entry is %#08x", last, want2)
				}
				got = got[:len(got)-1]
			}
			// The assembler pads the text of a function with zero words, which
			// are not instructions and are not compared.
			if len(want) < len(got) {
				t.Fatalf("%s: %d instructions and the assembler produced %d\n%v\n%v", s.why, len(got), len(want), got, want)
			}
			for _, w := range want[len(got):] {
				if w != 0 {
					t.Fatalf("%s: the assembler produced %d instructions and this package %d\n%v\n%v", s.why, len(want), len(got), got, want)
				}
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("%s: instruction %d is %#08x and the assembler produced %#08x", s.why, i, got[i], want[i])
				}
				n++
			}
		})
	}
	if n == 0 {
		t.Fatal("no instruction was compared")
	}
	comparisons += n
	t.Logf("%d prologue instructions compared against go tool asm", n)
}

// TestGrowstackSpillsTheArgumentRegisters checks the tail against the
// assembler, with arguments to save.
//
// The order is the property under test. R3 is the fourth integer argument
// register and it is also where runtime.morestack reads the caller's return
// address, so the arguments are saved before the link register is moved into
// it. Neither specs/042 nor specs/035 says so, and the other order loses an
// argument.
func TestGrowstackSpillsTheArgumentRegisters(t *testing.T) {
	word := &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}
	var in []place
	var spill, reload []string
	for i := 0; i < 5; i++ {
		in = append(in, place{reg: intArgRegs[i], inReg: true, off: int64(i) * 8, typ: word})
		spill = append(spill, fmt.Sprintf("\tMOVD\tR%d, %d(RSP)", i, 8+i*8))
		reload = append(reload, fmt.Sprintf("\tMOVD\t%d(RSP), R%d", 8+i*8, i))
	}
	got := synth(t, 32, false, in)
	want := asmText(t, prologueListing(32, false, spill, reload))
	got = got[:len(got)-1] // the jump back to the entry, which has no label here
	if len(want) < len(got) {
		t.Fatalf("%d instructions and the assembler produced %d\n%v\n%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("instruction %d is %#08x and the assembler produced %#08x", i, got[i], want[i])
		}
	}
	comparisons += len(got)

	// The link register is moved after every argument is saved.
	movlr := arm64.MovRegReg(arm64.Size64, arm64.R3, arm64.RegLink)
	at := -1
	for i, w := range got {
		if w == movlr {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("the tail does not put the link register in R3, and runtime.morestack reads it there")
	}
	spilled := 0
	for i := 0; i < at; i++ {
		if got[i]>>24 == 0xf9 { // an unsigned-offset 64-bit store or load
			spilled++
		}
	}
	if spilled < len(in) {
		t.Errorf("%d of %d arguments were saved before the link register overwrote R3", spilled, len(in))
	}
}

// TestLeafWithASmallFrameSkipsTheCheck is specs/035-goroutines-and-stack-growth.md's
// rule: a leaf whose frame fits in the region the runtime guarantees below the
// stack guard cannot overflow it, so it needs no check and no tail.
func TestLeafWithASmallFrameSkipsTheCheck(t *testing.T) {
	small := synth(t, 32, true, nil)
	large := synth(t, 32, false, nil)
	if len(small) >= len(large) {
		t.Fatalf("the leaf is %d instructions and the same frame with a check is %d", len(small), len(large))
	}
	guard, _ := arm64.MemUnsignedOffset(arm64.LoadX, arm64.RegTrampLo, arm64.RegG, 16)
	for _, w := range small {
		if w == guard {
			t.Error("the leaf loads the stack guard")
		}
	}
	found := false
	for _, w := range large {
		if w == guard {
			found = true
		}
	}
	if !found {
		t.Error("the function with a check does not load the stack guard")
	}
}

// TestStackConstantsMatchTheRuntime reads the thresholds out of the runtime's
// own source.
//
// specs/035-goroutines-and-stack-growth.md says the reserved region is the
// runtime's number and must be read from it rather than hard-coded. This is
// that reading, in the shape rtsym uses for the same reason.
func TestStackConstantsMatchTheRuntime(t *testing.T) {
	path := filepath.Join(runtimeGOROOT(t), "src", "internal", "abi", "stack.go")
	b, err := os.ReadFile(path)
	if err != nil {
		if requireCorpus() {
			t.Fatalf("NANOGO_REQUIRE_CORPUS is set and the runtime source is not there: %v", err)
		}
		t.Skipf("no runtime source: %v", err)
	}
	want := map[string]int64{"StackSmall": stackSmall, "StackBig": stackBig}
	re := regexp.MustCompile(`(?m)^\s*(StackSmall|StackBig)\s*=\s*(\d+)`)
	n := 0
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		v, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if v != want[m[1]] {
			t.Errorf("%s is %d in the runtime and %d here", m[1], v, want[m[1]])
		}
		n++
	}
	if n != len(want) {
		t.Fatalf("found %d of %d constants in %s", n, len(want), path)
	}
	t.Logf("%d stack constants compared against the runtime", n)
	comparisons += n
}

func runtimeGOROOT(t *testing.T) string {
	t.Helper()
	out, err := exec.Command(goTool(t), "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// intArgRegs is the integer argument sequence, spelled in obj/arm64's
// registers, so that a test can name the register an argument arrives in
// without going through the allocator's numbering.
var intArgRegs = [16]arm64.Reg{
	arm64.R0, arm64.R1, arm64.R2, arm64.R3, arm64.R4, arm64.R5, arm64.R6, arm64.R7,
	arm64.R8, arm64.R9, arm64.R10, arm64.R11, arm64.R12, arm64.R13, arm64.R14, arm64.R15,
}

// TestAssignFollowsTheConvention checks specs/030-abi.md's walk.
//
// The area advances for every argument, including one that travels in a
// register, because the stack-growth tail spills the registers into it.
func TestAssignFollowsTheConvention(t *testing.T) {
	word := &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}
	byteT := &ir.Type{Kind: ir.Uint8, Size: 1, Align: 1, Name: "byte"}
	word32 := &ir.Type{Kind: ir.Int32, Size: 4, Align: 4, Name: "int32"}

	tests := []struct {
		name  string
		types []*ir.Type
		regs  []arm64.Reg
		offs  []int64
		size  int64
	}{
		{"none", nil, nil, nil, 0},
		{"one", []*ir.Type{word}, []arm64.Reg{arm64.R0}, []int64{0}, 8},
		{"three", []*ir.Type{word, word, word}, []arm64.Reg{arm64.R0, arm64.R1, arm64.R2}, []int64{0, 8, 16}, 24},
		{"packed", []*ir.Type{byteT, word32}, []arm64.Reg{arm64.R0, arm64.R1}, []int64{0, 4}, 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, size, err := assignArgs(tc.types)
			if err != nil {
				t.Fatal(err)
			}
			if size != tc.size {
				t.Errorf("area %d, want %d", size, tc.size)
			}
			for i := range tc.regs {
				if !got[i].inReg || got[i].reg != tc.regs[i] {
					t.Errorf("argument %d is in %v, want %v", i, got[i].reg, tc.regs[i])
				}
				if got[i].off != tc.offs[i] {
					t.Errorf("argument %d is at %d, want %d", i, got[i].off, tc.offs[i])
				}
			}
		})
	}

	// Past the sixteenth integer register an argument travels on the stack.
	many := make([]*ir.Type, 18)
	for i := range many {
		many[i] = word
	}
	got, size, err := assignArgs(many)
	if err != nil {
		t.Fatal(err)
	}
	if size != 18*8 {
		t.Errorf("area %d, want %d", size, 18*8)
	}
	for i, p := range got {
		if want := i < 16; p.inReg != want {
			t.Errorf("argument %d is in a register (%v), want %v", i, p.inReg, want)
		}
	}

	// A float travels in the floating-point file and takes none of the
	// integer registers, so the integer after it is still in R0.
	f64 := &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}
	got, _, err = assignArgs([]*ir.Type{f64, word})
	if err != nil {
		t.Fatalf("a floating-point argument was refused: %v", err)
	}
	if !got[0].inReg || got[0].reg != arm64.F0 {
		t.Errorf("the float is in %v (in a register: %v), want F0", got[0].reg, got[0].inReg)
	}
	if !got[1].inReg || got[1].reg != arm64.R0 {
		t.Errorf("the integer after the float is in %v, want R0", got[1].reg)
	}

	// A value the register set cannot hold is refused rather than written into
	// the area by a form this package does not have.
	big := &ir.Type{Kind: ir.Struct, Size: 40, Align: 8, Name: "big"}
	for i := 0; i < 5; i++ {
		big.Fields = append(big.Fields, ir.Field{Name: "f", Type: word, Offset: int64(i) * 8})
	}
	if _, _, err := assignArgs([]*ir.Type{big}); err == nil {
		t.Error("a five-word value was placed as one, and no register holds it")
	}
}

// assignArgs is specs/030-abi.md's walk as this package consumes it: one place
// per value passed.
func assignArgs(types []*ir.Type) ([]place, int64, error) {
	vals, size, err := ssa.ABIArgs(ssa.NewArm64Target(), types)
	if err != nil {
		return nil, 0, err
	}
	pl, err := valuePlaces(vals)
	if err != nil {
		return nil, 0, err
	}
	return pl, size, nil
}

// TestFrameArithmetic checks the sizes against the numbers gc prints for the
// same shape, which are the ones the runtime reads.
//
// The rule is stated here and applied by ssa.LayoutFrame, which pads the
// locals area until the frame is a multiple of the alignment layout gives it.
// What this checks is the rule and the number FuncInfo carries, which is the
// frame without the link register word.
func TestFrameArithmetic(t *testing.T) {
	tests := []struct {
		body    int64 // the outgoing arguments and the slots
		size    int64
		locals  int64
		comment string
	}{
		{0, 0, 0, "a leaf with nothing in its frame has none"},
		{8, 32, 24, "eight bytes of body, eight for the link register, sixteen for the caller's frame pointer"},
		{16, 32, 24, "the frame pointer reservation absorbs the difference"},
		{24, 48, 40, "past sixteen, so the next multiple"},
	}
	for _, tc := range tests {
		size := tc.body + 8
		if size%16 == 8 {
			size += 8
		} else {
			size += 16
		}
		if tc.body == 0 {
			size = 0
		}
		if size != tc.size {
			t.Errorf("body %d gives frame %d, want %d (%s)", tc.body, size, tc.size, tc.comment)
		}
		f := frame{size: size}
		if f.locals() != tc.locals {
			t.Errorf("body %d gives locals %d, want %d", tc.body, f.locals(), tc.locals)
		}
		if size%16 != 0 {
			t.Errorf("frame %d is not 16-byte aligned", size)
		}
	}
}

// TestStackDeltaTakesEffectAfterTheAdjustment checks where the pc-value table
// says the frame appears.
//
// A row of a pc-value table is in effect from its own program counter, so the
// row that announces the frame belongs to the instruction after the one that
// moved the stack pointer, not to the instruction after the whole prologue.
// Between the two the runtime would look for the caller's frame at the wrong
// address, and the stack copier writes through what it finds there.
func TestStackDeltaTakesEffectAfterTheAdjustment(t *testing.T) {
	tests := []struct {
		size   int64
		leaf   bool
		adjust func(size int64) uint32
	}{
		{32, true, func(size int64) uint32 {
			w, _ := arm64.MemPreIndex(arm64.StoreX, arm64.RegLink, arm64.RSP, -size)
			return w
		}},
		{144, false, func(size int64) uint32 {
			w, _ := arm64.MemPreIndex(arm64.StoreX, arm64.RegLink, arm64.RSP, -size)
			return w
		}},
		{256, false, func(int64) uint32 {
			return arm64.MovSP(arm64.Size64, arm64.RSP, frameScratch)
		}},
	}
	for _, tc := range tests {
		e := synthEmitter(t, tc.size, tc.leaf, nil)
		at := -1
		for i, w := range e.text {
			if w == tc.adjust(tc.size) {
				at = i
				break
			}
		}
		if at < 0 {
			t.Fatalf("frame %d: the instruction that moves the stack pointer is not there", tc.size)
		}
		want := int64(at+1) * 4
		found := false
		for _, row := range e.pcsp {
			if row.Value != int32(tc.size) {
				continue
			}
			found = true
			if row.PC != want {
				t.Errorf("frame %d: the table says the frame appears at %d and the stack pointer moves at %d",
					tc.size, row.PC, want-4)
			}
			break
		}
		if !found {
			t.Errorf("frame %d: the table never says the frame is there", tc.size)
		}
		// It starts at nothing and it ends at nothing.
		if len(e.pcsp) == 0 || e.pcsp[0].PC != 0 || e.pcsp[0].Value != 0 {
			t.Errorf("frame %d: the table starts with %+v", tc.size, e.pcsp)
		}
		if last := e.pcsp[len(e.pcsp)-1]; last.Value != 0 {
			t.Errorf("frame %d: the table ends with %+v, and the growth tail runs with no frame", tc.size, last)
		}
	}
}

// The clearing of the frame objects, compared against go tool asm.
//
// zeroFrameObjects is what makes the locals bitmap true about the frame rather
// than only self-consistent, so a wrong encoding here is a wrong pointer the
// collector follows and not a wrong answer a program prints. The two forms are
// separate encodings and both are checked: a span of at most zeroUnroll words
// is one store per word, and a longer one is the post-indexed loop.
func TestZeroRangeMatchesTheAssembler(t *testing.T) {
	cases := []struct {
		off, size int64
		want      []string
		why       string
	}{
		{40, 24, []string{
			"\tMOVD\tZR, 40(RSP)",
			"\tMOVD\tZR, 48(RSP)",
			"\tMOVD\tZR, 56(RSP)",
		}, "three words, one store each"},
		{0, 8, []string{"\tMOVD\tZR, (RSP)"}, "one word at the bottom of the frame"},
		{40, zeroUnroll * 8, nil, "the largest span the straight sequence covers"},
		{696, 304, []string{
			"\tADD\t$696, RSP, R16",
			"\tADD\t$1000, RSP, R17",
			"loop:",
			"\tMOVD.P\tZR, 8(R16)",
			"\tCMP\tR17, R16",
			"\tBNE\tloop",
		}, "past it, so the loop"},
	}
	n := 0
	for _, tc := range cases {
		t.Run(fmt.Sprintf("off%dsize%d", tc.off, tc.size), func(t *testing.T) {
			want := tc.want
			if want == nil {
				for w := int64(0); w < tc.size/8; w++ {
					want = append(want, fmt.Sprintf("\tMOVD\tZR, %d(RSP)", tc.off+w*8))
				}
			}
			e := &emitter{opt: Options{Sym: "test.f"}, syms: newSymbols(obj.NewPackage("test"))}
			e.zeroRange(tc.off, tc.size)
			e.patch()
			if err := e.err(); err != nil {
				t.Fatalf("%s: %v", tc.why, err)
			}
			got := e.text
			asm := asmText(t, want)
			if len(got) == 0 {
				t.Fatalf("%s: nothing was emitted", tc.why)
			}
			if len(asm) < len(got) {
				t.Fatalf("%s: %d instructions and the assembler produced %d", tc.why, len(got), len(asm))
			}
			for _, w := range asm[len(got):] {
				if w != 0 {
					t.Fatalf("%s: the assembler produced %d instructions and this package %d", tc.why, len(asm), len(got))
				}
			}
			for i := range got {
				if got[i] != asm[i] {
					t.Errorf("%s: instruction %d is %#08x and the assembler produced %#08x", tc.why, i, got[i], asm[i])
				}
				n++
			}
		})
	}
	if n == 0 {
		t.Fatal("no instruction was compared")
	}
	comparisons += n
	t.Logf("%d clearing instructions compared against go tool asm", n)
}

// A frame object whose pointer words are outside the area the collector scans
// is refused rather than cleared.
//
// The store would land on the caller's saved frame pointer or on the return
// address, and the bitmap that named the word could not describe it either.
// checkFrame makes the same claim about the frame the prologue builds, and
// this is that claim for the words zeroFrameObjects writes.
func TestZeroFrameObjectsRefusesAnObjectOutsideTheScannedArea(t *testing.T) {
	ptr := &ir.Type{Kind: ir.Ptr, Size: 8, Align: 8, PtrBits: []byte{1}, Name: "*int"}
	item := func(off int64) []ssa.FrameItem {
		return []ssa.FrameItem{{
			Kind: ssa.ItemObject, Name: "obj", Type: ptr, Size: 8, Align: 8, Off: off,
		}}
	}
	newEmitter := func(off int64) *emitter {
		e := &emitter{opt: Options{Sym: "test.f"}, syms: newSymbols(obj.NewPackage("test"))}
		e.frame = frame{size: 64}
		e.fr = &ssa.Frame{PtrBase: 16, Varp: 56}
		e.items = item(off)
		return e
	}
	for _, tc := range []struct {
		off int64
		why string
	}{
		{8, "below the pointer group"},
		{56, "past the top of the locals"},
	} {
		e := newEmitter(tc.off)
		e.zeroFrameObjects()
		if e.err() == nil {
			t.Errorf("an object %s was cleared rather than refused", tc.why)
		}
		if len(e.text) != 0 {
			t.Errorf("an object %s was refused and %d instructions were emitted anyway", tc.why, len(e.text))
		}
	}
	// The same object inside the area is cleared, so the refusals above are
	// the bound and not a check that never passes.
	e := newEmitter(48)
	e.zeroFrameObjects()
	if err := e.err(); err != nil {
		t.Fatalf("an object inside the area was refused: %v", err)
	}
	if len(e.text) != 1 {
		t.Fatalf("an object inside the area emitted %d instructions, want one store", len(e.text))
	}
}
