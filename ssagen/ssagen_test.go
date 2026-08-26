// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// The tests in this file use the installed toolchain as the oracle. A code
// generator that is only compared against itself proves that it is consistent
// and nothing more: the question is whether the bytes are the instructions
// they are meant to be, and go tool objdump answers it. See
// specs/041-instruction-encoding.md, "Testing".

package ssagen

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/obj/arm64"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/ssa/rules"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// comparisons counts every instruction compared against the toolchain, so a
// run that silently compared nothing is visible.
var comparisons int

func TestMain(m *testing.M) {
	code := m.Run()
	fmt.Fprintf(os.Stderr, "ssagen: %d instructions compared against the toolchain\n", comparisons)
	// A run that compared nothing is a run where the oracle was absent and
	// the tests passed on their own word. CI sets NANOGO_REQUIRE_CORPUS and
	// that is not allowed to be a green build.
	if code == 0 && comparisons == 0 && requireCorpus() {
		fmt.Fprintln(os.Stderr, "ssagen: NANOGO_REQUIRE_CORPUS is set and nothing was compared against the toolchain")
		code = 1
	}
	os.Exit(code)
}

// requireCorpus reports whether a missing toolchain is a failure rather than a
// reason to skip. CI sets NANOGO_REQUIRE_CORPUS, so a gate that stopped
// running would turn red instead of green.
func requireCorpus() bool { return os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" }

// requireHostCanRunOutput reports whether a host that cannot run nanogo's
// output is a failure rather than a reason to skip.
//
// CI sets NANOGO_REQUIRE_LINK on the arm64 runner only, so the tests that link
// and run are required there and skipped elsewhere. Without the variable a
// green run would be indistinguishable from a run that linked nothing.
func requireHostCanRunOutput() bool { return os.Getenv("NANOGO_REQUIRE_LINK") == "1" }

// hostRunsNanogoOutput guards every test that links nanogo's output and runs it.
//
// nanogo emits arm64 machine code and has no second backend yet
// (specs/000-decisions.md decision 9 makes darwin/arm64 first and
// linux/amd64 second, and specs/043-amd64-backend.md is unbuilt). On any other
// host the linker is asked to build a binary for that host out of arm64
// instructions, and the result dies with "invalid runtime symbol table" as soon
// as the runtime walks its pc tables. That is not a bug in the object; it is a
// test asking the machine to run code for a different architecture.
//
// Cross-linking would not help either: go tool link builds for the host unless
// the whole toolchain is cross-configured, and the runtime it links against is
// the host's.
func hostRunsNanogoOutput(t *testing.T) {
	t.Helper()
	if runtime.GOARCH == "arm64" {
		return
	}
	if requireHostCanRunOutput() {
		t.Fatalf("NANOGO_REQUIRE_LINK is set and GOARCH is %s; nanogo emits arm64 and cannot be run here", runtime.GOARCH)
	}
	t.Skipf("nanogo emits arm64 machine code and GOARCH is %s, so the linked program cannot run here", runtime.GOARCH)
}

func goTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		if requireCorpus() {
			t.Fatalf("NANOGO_REQUIRE_CORPUS is set and there is no go command: %v", err)
		}
		t.Skipf("no go command: %v", err)
	}
	return p
}

// compiled is one function taken through the whole pipeline.
type compiled struct {
	fn   *ir.Func
	f    *ssa.Func
	a    *ssa.Alloc
	fset *syntax.FileSet
	file string
}

// compile runs parse, check, ir.Build, ssa.Build, lower and Allocate over one
// source file and returns the named function.
//
// It is the pipeline of specs/002-architecture.md with nothing left out, so a
// test here is a test of what the compiler will actually hand this package.
func compile(t *testing.T, src, name string) *compiled {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := syntax.NewFileSet()
	file, err := syntax.ParseFile(fset, path, nil, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Implicits:  make(map[syntax.Node]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	conf := types2.Config{Fset: fset, Sizes: types2.SizesFor("gc", "arm64")}
	pkg, err := conf.Check("main", []*syntax.File{file}, info)
	if err != nil {
		t.Fatalf("type check: %v", err)
	}
	p, err := ir.Build(pkg, []*syntax.File{file}, info)
	if err != nil {
		t.Fatalf("ir.Build: %v", err)
	}
	var fn *ir.Func
	for _, c := range p.Funcs {
		if c.Name == name {
			fn = c
		}
	}
	if fn == nil {
		t.Fatalf("%s is not in the package", name)
	}
	f, err := ssa.Build(fn)
	if err != nil {
		t.Fatalf("ssa.Build: %v", err)
	}
	target := ssa.NewArm64Target()
	// The pipeline of specs/002-architecture.md: decomposition, then
	// specs/030-abi.md's assignment, then selection. The assignment runs
	// between them because it finishes work decomposition stopped at its
	// bound and because the rewrites it makes still need lowering rules.
	ssa.Decompose(f)
	if err := ssa.AssignABI(f, target); err != nil {
		t.Fatalf("ssa.AssignABI: %v", err)
	}
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify after the ABI pass: %v", vs)
	}
	ssa.Lower(f, rules.ARM64)
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("the function did not verify after lowering: %v", vs)
	}
	ssa.SplitCriticalEdges(f)
	a, err := ssa.Allocate(f, target)
	if err != nil {
		t.Fatalf("ssa.Allocate: %v", err)
	}
	return &compiled{fn: fn, f: f, a: a, fset: fset, file: path}
}

// emit compiles one function into a symbol.
func emit(t *testing.T, c *compiled, p *obj.Package) *Result {
	t.Helper()
	r, err := Emit(c.f, c.a, p, Options{
		Sym:  c.f.Sym,
		File: c.file,
		Line: 1,
		Fset: c.fset,
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return r
}

// words returns the instruction words of a symbol.
func words(s *obj.Symbol) []uint32 {
	out := make([]uint32, 0, len(s.Data)/4)
	for i := 0; i+4 <= len(s.Data); i += 4 {
		out = append(out, binary.LittleEndian.Uint32(s.Data[i:]))
	}
	return out
}

func TestEmitReturnsAConstant(t *testing.T) {
	c := compile(t, "package main\n\nfunc f() int { return 7 }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	if r.Frame != 0 {
		t.Errorf("frame size %d, want 0: a leaf that returns a constant needs no frame", r.Frame)
	}
	if r.Args != 0 {
		t.Errorf("argument area %d, want 0", r.Args)
	}
	if r.Text.Flag&obj.SymFlagLeaf == 0 {
		t.Error("the symbol is not marked a leaf")
	}
	got := words(r.Text)
	if len(got) != 2 {
		t.Fatalf("the function is %d instructions, want 2:\n%v", len(got), got)
	}
	// The constant into R0 and the return. The result register is the ABI's
	// and not the allocator's, which is the placement this package does
	// because no pass above it does.
	if want := uint32(0xd65f03c0); got[1] != want {
		t.Errorf("second instruction %#08x, want %#08x (RET (R30))", got[1], want)
	}
	text := disassemble(t, r, p)
	if !strings.Contains(text, "R0") || !strings.Contains(text, "$7") {
		t.Errorf("the disassembly does not put 7 in R0:\n%s", text)
	}
}

func TestEmitAddsItsArguments(t *testing.T) {
	c := compile(t, "package main\n\nfunc f(a, b int) int { return a + b }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	if r.Args != 16 {
		t.Errorf("argument area %d, want 16 for two words", r.Args)
	}
	if r.Frame != 0 {
		t.Errorf("frame size %d, want 0", r.Frame)
	}
	text := disassemble(t, r, p)
	if !strings.Contains(text, "ADD") {
		t.Errorf("no addition in the disassembly:\n%s", text)
	}
	if !strings.Contains(text, "RET") {
		t.Errorf("no return in the disassembly:\n%s", text)
	}
}

// TestEmitIsDeterministic is specs/053-determinism.md's rule at this stage:
// the same function produces the same bytes, whatever a map iteration happens
// to do.
func TestEmitIsDeterministic(t *testing.T) {
	const src = "package main\n\nfunc f(a, b int) int {\n\tif a > b {\n\t\treturn a - b\n\t}\n\treturn b - a\n}\n"
	var first []byte
	for i := 0; i < 8; i++ {
		c := compile(t, src, "f")
		p := obj.NewPackage("main")
		r := emit(t, c, p)
		if i == 0 {
			first = r.Text.Data
			continue
		}
		if !bytes.Equal(first, r.Text.Data) {
			t.Fatalf("run %d produced different bytes:\n% x\n% x", i, first, r.Text.Data)
		}
	}
	if len(first) == 0 {
		t.Fatal("the function is empty")
	}
	t.Logf("%d bytes, identical across 8 runs", len(first))
}

// ---------------------------------------------------------------------------
// The toolchain as the oracle

// hostToolchain returns what the installed toolchain writes into an object.
func hostToolchain(t *testing.T) *obj.Toolchain {
	t.Helper()
	goTool(t)
	tc, err := obj.VerifyToolchain()
	if err != nil {
		t.Fatalf("the installed toolchain does not write the format nanogo writes: %v", err)
	}
	return tc
}

// writeObject writes an object and returns its path.
//
// The auxiliary symbols carry no name, which obj.Symbol.Anonymous states and
// the writer enforces. An earlier version of this file patched the written
// bytes to clear those names, because the writer rejected every empty name and
// the format's requirement was unexpressible.
func writeObject(t *testing.T, p *obj.Package, tc *obj.Toolchain) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nanogo.o")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.WriteObject(f, tc.Header); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// addFull puts a result and every symbol it carries into an object.
//
// It is Result.Add and nothing else. A test that built the auxiliary list of
// its own would be a second answer to the question of which tables a text
// symbol needs, and the linker reads the one this package writes.
func addFull(t *testing.T, r *Result, p *obj.Package) {
	t.Helper()
	if _, err := r.Add(p); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

var textRe = regexp.MustCompile(`^\s+[^\s]+:\d+\s+0x[0-9a-f]+\s+[0-9a-f]+\s+(.*)$`)

// disassemble runs go tool objdump over an object holding one function and
// returns the instructions, one per line.
//
// This is the differential test specs/041 asks for. A relocation of the wrong
// type produces a call that goes somewhere plausible, and the disassembly is
// where that shows up immediately rather than at run time.
func disassemble(t *testing.T, r *Result, p *obj.Package) string {
	t.Helper()
	// The object carries the host toolchain's header, because
	// obj.VerifyToolchain probes the installed go command and nanogo has no
	// way to write a header for a target it is not running on. On an amd64
	// host that means an object labelled amd64 holding arm64 instructions, and
	// objdump believes the header rather than the environment.
	//
	// That is a real limit of the compiler and not of this test: nanogo cannot
	// yet produce an object for a host it is not running on. It is recorded in
	// specs/040-object-format.md. Until it can, reading its own output back is
	// an arm64 host's job.
	hostRunsNanogoOutput(t)
	tc := hostToolchain(t)
	addFull(t, r, p)
	path := writeObject(t, p, tc)
	cmd := exec.Command(goTool(t), "tool", "objdump", path)
	// objdump decodes for the ambient GOARCH, as the assembler does.
	// nanogo emits arm64 whatever the host is, so an unpinned run
	// decodes arm64 bytes as some other instruction set and the
	// comparison is meaningless rather than merely failing.
	cmd.Env = append(os.Environ(), "GOARCH=arm64", "GOOS=darwin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool objdump rejected the object: %v\n%s", err, out)
	}
	var b strings.Builder
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		m := textRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		b.WriteString(strings.TrimSpace(m[1]))
		b.WriteString("\n")
		n++
	}
	if n == 0 {
		t.Fatalf("go tool objdump decoded nothing:\n%s", out)
	}
	comparisons += n
	return b.String()
}

// ---------------------------------------------------------------------------
// The milestone: link a compiled function and run it

// linkCfg is one import configuration and the environment that produced it.
//
// There is one per environment rather than one in all, because a build
// configuration is not portable between them. The object header carries the
// experiment set and the linker refuses an object whose header is not its own,
// so a test that forces an experiment needs a standard library built with it.
type linkCfg struct {
	once    sync.Once
	env     []string
	link    string // importcfg.link, for go tool link
	compile string // the main package's importcfg, for go tool compile
	err     error
}

var (
	// hostCfg is the toolchain as installed.
	hostCfg = &linkCfg{}

	// dwarf5Cfg is the toolchain with the dwarf5 experiment forced on.
	//
	// internal/buildcfg makes dwarf5 part of the baseline everywhere except
	// darwin, ios and aix, so on those three the linker skips a DWARF pass
	// that every other target runs. That pass is where an object nanogo
	// writes has failed before, and a machine that never runs it is a machine
	// where the failure is invisible. See
	// TestLinksUnderTheDwarf5Experiment.
	dwarf5Cfg = &linkCfg{env: []string{"GOEXPERIMENT=dwarf5"}}
)

// build compiles a stub once with c's environment and records where the go
// command left the two import configurations it wrote.
//
// The stub imports what a caller compiled by these tests imports, because the
// compile step needs an import configuration of its own and the go command
// writes one per package it builds. A stub that imported nothing would produce
// a configuration that names nothing.
func (c *linkCfg) build(t *testing.T) {
	t.Helper()
	goCmd := goTool(t)
	c.once.Do(func() {
		dir, err := os.MkdirTemp("", "nanogo-ssagen-link")
		if err != nil {
			c.err = err
			return
		}
		files := map[string]string{
			"go.mod": "module nanogo.example/link\n\ngo 1.27\n",
			// The imports of this probe decide the importcfg every caller in
			// this package is compiled against, so a caller may import only
			// what appears here. time is present because the finaliser tests
			// have to wait for another goroutine to run, and runtime.Gosched
			// only yields: it returns as soon as the caller is runnable again,
			// which on a loaded machine is often before the finaliser has run.
			"main.go": "package main\n\nimport (\n\t\"runtime\"\n\t\"sync/atomic\"\n\t\"time\"\n)\n\n" +
				"var n int32\n\nfunc main() {\n\truntime.GC()\n\tatomic.AddInt32(&n, 1)\n\ttime.Sleep(0)\n}\n",
		}
		for name, body := range files {
			if c.err = os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); c.err != nil {
				return
			}
		}
		cmd := exec.Command(goCmd, "build", "-work", "-o", filepath.Join(dir, "prog"), ".")
		cmd.Dir = dir
		cmd.Env = append(append(os.Environ(), "GOPROXY=off"), c.env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			c.err = fmt.Errorf("go build %v: %v\n%s", c.env, err, out)
			return
		}
		work := ""
		for _, line := range strings.Split(string(out), "\n") {
			if w, ok := strings.CutPrefix(line, "WORK="); ok {
				work = strings.TrimSpace(w)
			}
		}
		if work == "" {
			c.err = fmt.Errorf("go build did not report its work directory:\n%s", out)
			return
		}
		c.err = filepath.WalkDir(work, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			switch d.Name() {
			case "importcfg.link":
				c.link = path
			case "importcfg":
				// The main package's, which is the one that names every
				// package the stub imports.
				b, err := os.ReadFile(path)
				if err == nil && strings.Contains(string(b), "packagefile sync/atomic=") {
					c.compile = path
				}
			}
			return nil
		})
	})
	if c.err != nil {
		t.Fatalf("cannot build an import configuration to link against: %v", c.err)
	}
	if c.link == "" || c.compile == "" {
		t.Fatalf("the go command produced no importcfg (link %q, compile %q)", c.link, c.compile)
	}
}

// linkConfig returns an importcfg that maps every package the runtime needs to
// a compiled archive, which is what the linker wants and what nanogo cannot
// produce for itself yet. obj/write_test.go builds it the same way.
func linkConfig(t *testing.T) string {
	t.Helper()
	hostCfg.build(t)
	return hostCfg.link
}

// compileConfig returns the import configuration a caller compiled by these
// tests needs. It is built by the same run as linkConfig.
func compileConfig(t *testing.T) string {
	t.Helper()
	hostCfg.build(t)
	return hostCfg.compile
}

// bigType is a struct with more parts than specs/025-lowering-and-rules.md
// decomposes and few enough for the convention to put in registers. It is the
// shape specs/030-abi.md's assignment pass exists for.
const bigType = "type Big struct{ A, B, C, D, E int }\n\n"

// big20Type has more parts than the sixteen integer argument registers, so the
// convention sends it to the argument area whole.
const big20Type = "type Big20 struct{ A, B, C, D, E, F, G, H, I, J, K, L, M, N, O, P, Q, R, S, T int }\n\n"

// TestLinkAndRun is the milestone of this package.
//
// A Go function goes through parse, check, ir.Build, ssa.Build, lower,
// Allocate and this package, into an object, through the real linker, and
// runs. Every stage below the front end is exercised and the result is a
// process whose exit status is what the compiled code computed.
func TestLinkAndRun(t *testing.T) {
	hostRunsNanogoOutput(t)
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)

	// The sources have no assignment statement in them, because ssa.Build
	// refuses one: "assign: statement is not built yet". That is the widest
	// program this pipeline compiles today and it is a limit above this
	// package, not in it.
	tests := []struct {
		name string
		src  string
		call string
		defs string
		// decl is the declaration the gc-compiled caller sees. It defaults to
		// the two-integer signature the older cases use.
		decl string
		want int
	}{
		{"empty", "func run(a, b int) int { return 0 }", "run(1, 2)", "", "", 0},
		{"constant", "func run(a, b int) int { return 7 }", "run(1, 2)", "", "", 7},
		{"argument", "func run(a, b int) int { return b }", "run(3, 9)", "", "", 9},
		{"arithmetic", "func run(a, b int) int { return a*b + 1 }", "run(20, 3)", "", "", 61},
		{"branch", "func run(a, b int) int {\n\tif a > b {\n\t\treturn a - b\n\t}\n\treturn b - a\n}", "run(4, 30)", "", "", 26},
		{"nested", "func run(a, b int) int {\n\tif a > b {\n\t\tif a > 100 {\n\t\t\treturn 1\n\t\t}\n\t\treturn 2\n\t}\n\treturn 3\n}", "run(200, 1)", "", "", 1},
		// A call reaches a function gc compiled, so the argument and the
		// result cross the toolchain boundary through specs/030-abi.md and
		// nothing else. It is also the first function here with a frame, a
		// stack-growth check and a growth tail.
		{"call", "func twice(x int) int\n\nfunc run(a, b int) int { return twice(a) }", "run(21, 0)",
			"func twice(x int) int { return x * 2 }", "", 42},
		// A division carries a check that branches to a block that calls the
		// runtime and does not return, so the object holds a relocation
		// against a symbol rtsym names.
		{"divide", "func run(a, b int) int { return a / b }", "run(41, 6)", "", "", 6},

		// specs/030-abi.md across the toolchain boundary. gc compiles the
		// caller, nanogo compiles run, and the two agree or the program reads
		// a word the other one never wrote. Nothing below the assignment can
		// catch a disagreement, so these are the gate.
		//
		// A five-field struct is the case the assignment pass exists for: gc
		// passes it in five registers, and the bound of
		// specs/025-lowering-and-rules.md stops decomposition at four.
		{"struct in registers",
			bigType + "func run(s Big) int { return s.A*10000 + s.B*1000 + s.C*100 + s.D*10 + s.E }",
			"run(Big{1, 2, 3, 4, 5})", bigType, "func run(s Big) int", 12345},
		// A struct with more parts than the register set holds travels in the
		// argument area whole, and the callee reads it where the caller wrote
		// it.
		{"struct in the argument area",
			big20Type + "func run(s Big20) int { return s.A*100 + s.J*10 + s.T }",
			"run(Big20{A: 7, J: 8, T: 9})", big20Type, "func run(s Big20) int", 789},
		// A mixture: the scalars around the struct take the registers the
		// struct did not, which is where an off-by-one in the walk shows up.
		{"struct between scalars",
			bigType + "func run(x int, s Big, y int) int { return x*100000 + s.A*10000 + s.E*10 + y }",
			"run(6, Big{1, 2, 3, 4, 5}, 9)", bigType, "func run(x int, s Big, y int) int", 610059},
		// Eighteen integers exhaust the sixteen argument registers, so the
		// last two travel in the area. Pre-colouring is what lets the
		// allocator hold sixteen live arguments at once.
		{"registers exhausted",
			"func run(a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q, r int) int { return a + r*1000 }",
			"run(1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5)", "",
			"func run(a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q, r int) int", 5001},
		// nanogo calls gc with a struct that travels in registers, and gc
		// returns it into nanogo's own return. Both directions of the
		// convention are in one program.
		{"struct through a call",
			bigType + "func sum(s Big) int\n\nfunc run(s Big) int { return sum(s) }",
			"run(Big{10, 20, 30, 40, 50})",
			bigType + "func sum(s Big) int { return s.A + s.B + s.C + s.D + s.E }",
			"func run(s Big) int", 150},
		// Eighteen operands at a call, more than the two scratch registers
		// the target reserves. Without ssa.Target.UseReg naming the register
		// each operand is read from, the allocator asks for a scratch
		// register per spilled operand and refuses the function. The last two
		// operands travel in the argument area, so this is also a stack
		// argument written by nanogo and read by gc.
		{"a call with more operands than scratch registers",
			"func g(a, b, c, d, e, f2, h, i, j, k, l, m, n, o, p, q, r, s int) int\n\n" +
				"func run(a, b int) int { return g(a, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, b) }",
			"run(100, 200)",
			"func g(a, b, c, d, e, f2, h, i, j, k, l, m, n, o, p, q, r, s int) int { return a + s }",
			"", 300},
		// A value the register set cannot hold travels in the argument area
		// whole, and the caller writes it there. This is nanogo writing and
		// gc reading, so a disagreement about where the area starts or how
		// wide it is shows up as the wrong number.
		{"a large struct through a call",
			big20Type + "func g(s Big20) int\n\nfunc run(s Big20) int { return g(s) }",
			"run(Big20{A: 3, T: 4})",
			big20Type + "func g(s Big20) int { return s.A*100 + s.T }",
			"func run(s Big20) int", 304},
		// The same value in the other direction: nanogo writes the result
		// into the area the caller reserved for it and returns nothing in a
		// register, and gc reads it out.
		{"a large struct returned",
			big20Type + "func run(s Big20) Big20 { return s }",
			"run(Big20{A: 5, T: 6}).T",
			big20Type, "func run(s Big20) Big20", 6},
		// The scalar after a large argument is still in the first argument
		// register, because a value the registers could not hold takes none
		// of them. Its spill slot is above the whole stack part, which is
		// where an area laid out as one sequence would put it wrong.
		{"a scalar after a large struct",
			big20Type + "func g(s Big20, n int) int\n\nfunc run(s Big20, n int) int { return g(s, n) }",
			"run(Big20{A: 7}, 9)",
			big20Type + "func g(s Big20, n int) int { return s.A*100 + n }",
			"func run(s Big20, n int) int", 709},
		// A five-field struct coming back from a call, which is the caller's
		// half of the same bound: gc returns it in five result registers and
		// decomposition stopped at four, so the assignment pass reads five
		// results at the call site. gc writes the registers and nanogo reads
		// them, so a disagreement is the wrong number rather than a crash.
		{"struct returned by a call",
			bigType + "func mk() Big\n\nfunc run(a, b int) int { s := mk(); return s.A*10000 + s.B*1000 + s.C*100 + s.D*10 + s.E }",
			"run(0, 0)",
			bigType + "func mk() Big { return Big{1, 2, 3, 4, 5} }", "", 12345},
		// A wide result followed by a narrow one. The struct occupies result
		// words 0 to 4, so the integer is word 5 and comes back in the sixth
		// result register. Decomposition numbered the struct as one word, and
		// a call site that kept that numbering would read the integer out of
		// the struct's second field.
		{"a result after a struct returned by a call",
			bigType + "func mk() (Big, int)\n\nfunc run(a, b int) int { s, n := mk(); return s.A*1000 + s.E*100 + n }",
			"run(0, 0)",
			bigType + "func mk() (Big, int) { return Big{1, 2, 3, 4, 5}, 6 }", "", 1506},
		// A string is two words, so the integer after it is in the fourth
		// argument register and not in the third. A walk that counted the
		// string as one word would read it from the wrong place, and the
		// pointer in front of it is a word the arguments bitmap has to mark.
		{"a string between a pointer and an integer",
			"func run(p *int, s string, n int) int { return n * 3 }",
			`run(new(int), "abcd", 7)`, "", "func run(p *int, s string, n int) int", 21},
	}
	for _, tc2 := range tests {
		t.Run(tc2.name, func(t *testing.T) {
			c := compile(t, "package main\n\n"+tc2.src+"\n", "run")
			p := newMainPackage()
			r := emit(t, c, p)
			addFull(t, r, p)

			// main.main calls the compiled function and exits with what it
			// returned, so the process's exit status is the value the
			// generated code computed. The caller is assembly rather than Go
			// because nanogo compiles one function here and the driver that
			// compiles a package is specs/050-driver.md.
			caller := exitWrapper(t, goCmd, c.f.Sym, tc2.call, tc2.defs, tc2.decl)
			got := strings.TrimSpace(runLinked(t, goCmd, tc, cfg, p, caller))
			if want := strconv.Itoa(tc2.want); got != want {
				t.Fatalf("the program printed %q, and the function returns %s", got, want)
			}
			t.Logf("linked and ran a nanogo-compiled function, which returned %s", got)
		})
	}
}

// newMainPackage returns an object for package main.
func newMainPackage() *obj.Package {
	p := obj.NewPackage("main")
	p.Main = true
	return p
}

// runLinked writes the object, links it with the caller and runs the program.
//
// go tool link takes one object, so the two halves of the main package travel
// in an archive: the function nanogo compiled and the caller gc compiled.
func runLinked(t *testing.T, goCmd string, tc *obj.Toolchain, cfg string, p *obj.Package, caller string) string {
	t.Helper()
	objPath := writeObject(t, p, tc)
	dir := t.TempDir()
	archive := filepath.Join(dir, "main.a")
	if b, err := exec.Command(goCmd, "tool", "pack", "c", archive, caller, objPath).CombinedOutput(); err != nil {
		t.Fatalf("go tool pack: %v\n%s", err, b)
	}
	out := filepath.Join(dir, "a.out")
	if b, err := exec.Command(goCmd, "tool", "link", "-importcfg", cfg, "-o", out, archive).CombinedOutput(); err != nil {
		t.Fatalf("the linker rejected the object: %v\n%s", err, b)
	}
	b, err := exec.Command(out).CombinedOutput()
	if err != nil {
		t.Fatalf("the linked program failed: %v\n%s", err, b)
	}
	return string(b)
}

// runLinkedEnvCfg links and runs with an environment on every step.
//
// The environment reaches the linker as well as the program, which the other
// runners do not need: an experiment that changes what the linker does has to
// be set when the linker runs and not only when the binary does.
func runLinkedEnvCfg(t *testing.T, goCmd string, tc *obj.Toolchain, cfg string, p *obj.Package, caller string, env []string) (string, error) {
	t.Helper()
	objPath := writeObject(t, p, tc)
	dir := t.TempDir()
	archive := filepath.Join(dir, "main.a")
	pack := exec.Command(goCmd, "tool", "pack", "c", archive, caller, objPath)
	pack.Env = append(os.Environ(), env...)
	if b, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("go tool pack: %v\n%s", err, b)
	}
	out := filepath.Join(dir, "a.out")
	link := exec.Command(goCmd, "tool", "link", "-importcfg", cfg, "-o", out, archive)
	link.Env = append(os.Environ(), env...)
	if b, err := link.CombinedOutput(); err != nil {
		return string(b), fmt.Errorf("the linker rejected the object: %w", err)
	}
	run := exec.Command(out)
	run.Env = append(os.Environ(), env...)
	b, err := run.CombinedOutput()
	return string(b), err
}

// runLinkedRaw is runLinked for a program that is expected to fail.
func runLinkedRaw(t *testing.T, goCmd string, tc *obj.Toolchain, cfg string, p *obj.Package, caller string) (string, error) {
	t.Helper()
	objPath := writeObject(t, p, tc)
	dir := t.TempDir()
	archive := filepath.Join(dir, "main.a")
	if b, err := exec.Command(goCmd, "tool", "pack", "c", archive, caller, objPath).CombinedOutput(); err != nil {
		t.Fatalf("go tool pack: %v\n%s", err, b)
	}
	out := filepath.Join(dir, "a.out")
	if b, err := exec.Command(goCmd, "tool", "link", "-importcfg", cfg, "-o", out, archive).CombinedOutput(); err != nil {
		t.Fatalf("the linker rejected the object: %v\n%s", err, b)
	}
	b, err := exec.Command(out).CombinedOutput()
	return string(b), err
}

// TestStackGrowthCopiesNanogoFrames drives the stack-growth path of
// specs/035-goroutines-and-stack-growth.md with a recursion deep enough to
// exhaust the goroutine's first stack several times over.
//
// Four things happen and the result asserts all of them, because each is a
// different piece of this package:
//
//   - the prologue's comparison against the stack guard fires, so
//     runtime.morestack runs, which means the tail spilled the arguments and
//     put the link register in R3;
//   - the runtime's stack copier walks the nanogo frames, which it can only do
//     with the pc-value table this package wrote;
//   - it reads a stack map for each of them, which is what this package now
//     writes: without one the runtime throws "missing stackmap" rather than
//     copying;
//   - it rewrites every word the locals bitmap marks, so a bit set on a word
//     that holds an integer turns that integer into a wrong one. The recursion
//     carries its accumulator through the growth, so the printed value is the
//     evidence that nothing was rewritten that should not have been.
//
// This test used to assert the throw. The stack maps are what turned it into
// an assertion about the value.
func TestStackGrowthCopiesNanogoFrames(t *testing.T) {
	hostRunsNanogoOutput(t)
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)
	const src = "func run(a, b int) int {\n\tif a > 0 {\n\t\treturn run(a-1, b+1)\n\t}\n\treturn b\n}"
	c := compile(t, "package main\n\n"+src+"\n", "run")
	p := newMainPackage()
	r := emit(t, c, p)
	addFull(t, r, p)
	caller := exitWrapper(t, goCmd, c.f.Sym, "run(200000, 0)", "", "")
	// gccheckmark marks a second time with the world stopped and compares, so
	// a frame the copier read with the wrong map is a crash here rather than a
	// wrong answer somewhere else.
	out, err := runLinkedEnv(t, goCmd, tc, cfg, p, caller, []string{"GODEBUG=gccheckmark=1"})
	if err != nil {
		t.Fatalf("the program failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(out); got != "200000" {
		t.Fatalf("the recursion printed %q, and it counts 200000 frames", got)
	}
	t.Logf("200000 frames were grown, copied and unwound")
}

// exitWrapper compiles the main package that calls the function under test and
// exits with its result. It is compiled by gc, so the test is a real
// cross-toolchain call: gc's caller reaches nanogo's callee through
// specs/030-abi.md and nothing else.
func exitWrapper(t *testing.T, goCmd, sym, call, defs, decl string) string {
	t.Helper()
	return exitWrapperEnv(t, goCmd, sym, call, defs, decl, nil)
}

// exitWrapperEnv is exitWrapper with an environment for the compile step, so
// that a test can build the caller with the experiment set it means to link
// under. The object header carries that set and the linker refuses a mismatch.
func exitWrapperEnv(t *testing.T, goCmd, sym, call, defs, decl string, env []string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if decl == "" {
		decl = "func run(a, b int) int"
	}
	body := "package main\n\n" + defs + "\n\n" + decl + "\n\nfunc main() { println(" + call + ") }\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The caller declares the function without a body, which gc reads as a
	// symbol defined elsewhere. The symbol ABI file is how the toolchain is
	// told which convention that definition uses; without it gc assumes ABI0
	// and the call reaches nothing.
	abis := filepath.Join(dir, "symabis")
	if err := os.WriteFile(abis, []byte("def "+sym+" ABIInternal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "main.o")
	cmd := exec.Command(goCmd, "tool", "compile", "-p", "main", "-symabis", abis, "-o", out, src)
	cmd.Env = append(os.Environ(), env...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compiling the caller failed: %v\n%s", err, b)
	}
	return out
}

// TestObjdumpNamesTheCall checks that a call carries the relocation the linker
// needs and that the disassembler resolves it to the callee.
//
// A wrong relocation type is a call that goes somewhere plausible, so the
// check is the name objdump prints and not the bytes.
func TestObjdumpNamesTheCall(t *testing.T) {
	c := compile(t, "package main\n\nfunc g(a int) int\n\nfunc f(a int) int { return g(a) }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	if len(r.Text.Relocs) == 0 {
		t.Fatal("the call produced no relocation")
	}
	for _, rel := range r.Text.Relocs {
		if rel.Type != obj.R_CALLARM64 {
			t.Errorf("relocation type %v, want R_CALLARM64", rel.Type)
		}
		if rel.Size != 4 {
			t.Errorf("relocation size %d, want 4", rel.Size)
		}
	}
	text := disassemble(t, r, p)
	if !strings.Contains(text, "main.g") {
		t.Errorf("the disassembly does not name the callee:\n%s", text)
	}
}

// TestFrameGrowsWithLocals checks the frame arithmetic against the toolchain's
// own answer for the same source.
//
// gc prints the argument area and the frame size of every function it
// compiles. The argument area is the calling convention and nothing else, so
// nanogo has to agree with it exactly; the frame size depends on how many
// values were spilled and is compared only for the shape it must have.
func TestFrameGrowsWithLocals(t *testing.T) {
	goCmd := goTool(t)
	tests := []struct {
		name string
		src  string
		args int64
	}{
		{"none", "package main\n\nfunc f() int { return 1 }\n", 0},
		{"one", "package main\n\nfunc f(a int) int { return a }\n", 8},
		{"three", "package main\n\nfunc f(a, b, c int) int { return a + b + c }\n", 24},
		{"narrow", "package main\n\nfunc f(a int8, b int32) int32 { return b }\n", 8},
		// specs/030-abi.md's argument area has two parts: the values the
		// registers could not hold, then one spill slot per value that took
		// registers. gc's number is the only check that the two are laid out
		// in that order and not interleaved, because the total is the same
		// either way and only the offsets differ.
		{"a struct in registers", "package main\n\n" + bigType + "func f(s Big) int { return s.A }\n", 40},
		{"a struct in the area", "package main\n\n" + big20Type + "func f(s Big20) int { return s.A }\n", 160},
		{"a struct between scalars", "package main\n\n" + bigType +
			"func f(x int, s Big, y int) int { return x + s.A + y }\n", 56},
		{"registers exhausted", "package main\n\n" +
			"func f(a, b, c, d, e, g, h, i, j, k, l, m, n, o, p, q, r, s int) int { return a + s }\n", 144},
		{"a struct past the registers", "package main\n\n" + bigType +
			"func f(a, b, c, d, e, g, h, i, j, k, l, m, n, o, p, q int, s Big) int { return a + s.A }\n", 168},
		// A result the registers cannot hold is in the same area, after the
		// arguments that could not fit and under the spill slots.
		{"a large result", "package main\n\n" + big20Type +
			"func f(s Big20) Big20 { return s }\n", 320},
		{"a large result after a register argument", "package main\n\n" + big20Type +
			"func f(a int, s Big20) Big20 { return s }\n", 328},
	}
	n := 0
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := compile(t, tc.src, "f")
			p := obj.NewPackage("main")
			r := emit(t, c, p)
			if r.Args != tc.args {
				t.Errorf("argument area %d, want %d", r.Args, tc.args)
			}
			if want := gcArgs(t, goCmd, tc.src, "f"); want >= 0 && r.Args != want {
				t.Errorf("argument area %d, and gc says %d", r.Args, want)
			}
			if r.Frame%16 != 0 {
				t.Errorf("frame size %d is not 16-byte aligned", r.Frame)
			}
			n++
		})
	}
	if n == 0 {
		t.Fatal("no frame was compared")
	}
	comparisons += n
}

var argsRe = regexp.MustCompile(`^main\.(\w+) STEXT.* args=0x([0-9a-f]+) locals=0x([0-9a-f]+)`)

// gcArgs returns the argument area gc computes for a function, or -1.
func gcArgs(t *testing.T, goCmd, src, name string) int64 {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(goCmd, "tool", "compile", "-p", "main", "-S", "-o", filepath.Join(dir, "a.o"), path).CombinedOutput()
	if err != nil {
		t.Logf("go tool compile: %v\n%s", err, out)
		return -1
	}
	for _, line := range strings.Split(string(out), "\n") {
		m := argsRe.FindStringSubmatch(line)
		if m == nil || m[1] != name {
			continue
		}
		v, err := strconv.ParseInt(m[2], 16, 64)
		if err != nil {
			return -1
		}
		return v
	}
	return -1
}

// ---------------------------------------------------------------------------
// Shapes the front end cannot build yet

// hand builds an SSA function directly, lowers it and allocates it.
//
// ssa.Build refuses an assignment statement, so a phi and a spill cannot be
// reached from Go source through this pipeline. They are the two shapes the
// allocation produces that the emitter has to realise, so they are built here
// in the form the lowering pass expects, exactly as the rule tests of
// specs/025-lowering-and-rules.md build theirs.
func hand(t *testing.T, name string, build func(f *ssa.Func)) *compiled {
	t.Helper()
	f := ssa.NewFunc(name)
	f.Sym = "main." + name
	build(f)
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("the hand-built function did not verify: %v\n%s", vs, f)
	}
	target := ssa.NewArm64Target()
	if err := ssa.AssignABI(f, target); err != nil {
		t.Fatalf("ssa.AssignABI: %v", err)
	}
	ssa.Lower(f, rules.ARM64)
	if vs := ssa.Verify(f); len(vs) != 0 {
		t.Fatalf("the hand-built function did not verify after lowering: %v", vs)
	}
	ssa.SplitCriticalEdges(f)
	a, err := ssa.Allocate(f, target)
	if err != nil {
		t.Fatalf("ssa.Allocate: %v", err)
	}
	return &compiled{f: f, a: a}
}

var (
	typeInt  = &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}
	typeBool = &ir.Type{Kind: ir.Bool, Size: 1, Align: 1, Name: "bool"}
)

// TestEmitAPhi checks the copies an edge carries.
//
// A phi is not an instruction. specs/026-register-allocation.md resolves it
// into moves on the edges that reach the block, and this package emits them
// where the allocation says: at the end of a predecessor with one successor,
// or at the start of a successor with one predecessor.
func TestEmitAPhi(t *testing.T) {
	c := hand(t, "phi", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockIf
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		a := e.NewValue(0, ssa.OpArg, typeInt)
		b := e.NewValue(0, ssa.OpArg, typeInt)
		e.Control = e.NewValue(0, ssa.OpLess, typeBool, a, b)

		yes := f.NewBlock(ssa.BlockPlain)
		no := f.NewBlock(ssa.BlockPlain)
		join := f.NewBlock(ssa.BlockRet)
		e.AddEdgeTo(yes)
		e.AddEdgeTo(no)
		yes.AddEdgeTo(join)
		no.AddEdgeTo(join)
		// Each arm computes a different value, so the join needs a phi and
		// the phi needs a move on each edge.
		x := yes.NewValue(0, ssa.OpAdd, typeInt, a, b)
		y := no.NewValue(0, ssa.OpSub, typeInt, a, b)
		phi := join.NewValue(0, ssa.OpPhi, typeInt, x, y)
		join.Control = join.NewValue(0, ssa.OpMakeResult, ssa.MemType, phi, mem)
	})
	if len(c.a.Edges) == 0 {
		t.Fatal("the allocation carries no edge copies, so this test proves nothing")
	}
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	text := disassemble(t, r, p)
	if n := strings.Count(text, "MOVD"); n == 0 {
		t.Errorf("the phi produced no move:\n%s", text)
	}
	if !strings.Contains(text, "ADD") || !strings.Contains(text, "SUB") {
		t.Errorf("an arm of the branch is missing:\n%s", text)
	}
	t.Logf("%d edge copy sequences realised\n%s", len(c.a.Edges), text)
}

// TestEmitSpills drives the frame: more live values than there are registers,
// so the allocator spills, and the emitter has to write each one to its slot
// and read it back.
func TestEmitSpills(t *testing.T) {
	const n = 30
	c := hand(t, "spill", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		args := make([]*ssa.Value, n)
		for i := range args {
			args[i] = e.NewValue(0, ssa.OpArg, typeInt)
		}
		// Every argument is read after every other one is defined, so they are
		// all live at once and there are more of them than there are
		// registers.
		sum := args[0]
		for i := 1; i < n; i++ {
			sum = e.NewValue(0, ssa.OpAdd, typeInt, sum, args[i])
		}
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, sum, mem)
	})
	if len(c.a.Slots) == 0 {
		t.Fatal("nothing was spilled, so this test proves nothing")
	}
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	if r.Frame == 0 {
		t.Fatal("values were spilled and the frame is empty")
	}
	if r.Args != n*8 {
		t.Errorf("argument area %d, want %d", r.Args, n*8)
	}
	text := disassemble(t, r, p)
	if !strings.Contains(text, "(RSP)") {
		t.Errorf("no slot was written or read:\n%s", text)
	}
	t.Logf("%d slots in a frame of %d bytes", len(c.a.Slots), r.Frame)
}

// TestEmitFrameObject checks the address of a local that lives in the frame.
//
// specs/027-liveness-and-stackmaps.md owns the layout and does not exist yet,
// so this package assigns the offsets. The address is an ADD from the stack
// pointer, and the offset has to be inside the frame and clear of the outgoing
// argument area.
func TestEmitFrameObject(t *testing.T) {
	local := &ir.Object{Name: "x", Type: typeInt, Class: ir.ClassLocal}
	c := hand(t, "frameobj", func(f *ssa.Func) {
		f.Frame = []*ir.Object{local}
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		a := e.NewValue(0, ssa.OpArg, typeInt)
		addr := e.NewValue(0, ssa.OpLocalAddr, typeInt, mem)
		addr.Aux = local
		st := e.NewValue(0, ssa.OpStore, ssa.MemType, addr, a, mem)
		st.AuxInt = 8
		ld := e.NewValue(0, ssa.OpLoad, typeInt, addr, st)
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, ld, st)
	})
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	if r.Frame == 0 {
		t.Fatal("the frame holds an object and its size is zero")
	}
	text := disassemble(t, r, p)
	if !strings.Contains(text, "RSP") {
		t.Errorf("the frame object is not addressed from the stack pointer:\n%s", text)
	}
	t.Logf("frame %d bytes\n%s", r.Frame, text)
}

// linkReadsAux lists the auxiliary entries cmd/link reads for a text symbol
// without first checking that they are there, with what each omission does.
//
// This is the list the object has to satisfy, and it is written down rather
// than inferred, because every entry on it was found by a linker crash and not
// by a diagnostic. An entry added to it is a one-line change here and a test
// that fails until the emitter writes it.
var linkReadsAux = []struct {
	typ  obj.AuxType
	name string
	why  string
}{
	{obj.AuxFuncInfo, "AuxFuncInfo",
		"the symbol belongs to no compilation unit and the DWARF pass sorts an empty list"},
	{obj.AuxDwarfInfo, "AuxDwarfInfo",
		"cmd/link/internal/ld.writedebugaddr reads the relocations of symbol 0 " +
			"and panics with \"trying to get oreader for invalid sym 0\""},
	{obj.AuxPcsp, "AuxPcsp", "the pclntab pass reads the table through an index it does not check"},
	{obj.AuxPcfile, "AuxPcfile", "the pclntab pass reads the table through an index it does not check"},
	{obj.AuxPcline, "AuxPcline", "the pclntab pass reads the table through an index it does not check"},
}

// checkTextAux asserts that a text symbol carries what the linker reads.
//
// It is a structural check on the object and it runs on any host, which is the
// point: the DWARF entry above is read by a pass that darwin does not run, so
// the failure it prevents was invisible on the machine this compiler is
// written on for as long as it took a continuous integration run to say so.
//
// What it cannot do is find the next such entry. Only running the linker does
// that, which is TestLinksUnderTheDwarf5Experiment.
func checkTextAux(t *testing.T, text *obj.Symbol) {
	t.Helper()
	for _, want := range linkReadsAux {
		found := false
		for _, a := range text.Aux {
			if a.Type != want.typ {
				continue
			}
			found = true
			if a.Sym.IsZero() {
				t.Errorf("%s: the %s entry names no symbol, and %s", text.Name, want.name, want.why)
			}
		}
		if !found {
			t.Errorf("%s: no %s entry, and %s", text.Name, want.name, want.why)
		}
	}
	// Every entry has to name a symbol, whether or not the linker reads it
	// unconditionally: a zero reference is index 0 of the definition array.
	for i, a := range text.Aux {
		if a.Sym.IsZero() {
			t.Errorf("%s: auxiliary entry %d (%v) names no symbol", text.Name, i, a.Type)
		}
	}
}

// TestEveryFunctionCarriesWhatTheLinkerReads runs the structural check over
// the shapes whose auxiliary lists differ: a leaf with no frame, a function
// that makes a call and therefore has a growth tail, and one with a frame
// object.
func TestEveryFunctionCarriesWhatTheLinkerReads(t *testing.T) {
	srcs := []struct{ name, src string }{
		{"leaf", "package main\n\nfunc f(a int) int { return a }\n"},
		{"call", "package main\n\nfunc g(a int) int\n\nfunc f(a int) int { return g(a) }\n"},
		{"frame", "package main\n\n" + bigType + "func f(s Big) int { return s.A + s.E }\n"},
		{"area", "package main\n\n" + big20Type + "func f(s Big20) int { return s.A }\n"},
	}
	for _, tc := range srcs {
		t.Run(tc.name, func(t *testing.T) {
			c := compile(t, tc.src, "f")
			p := obj.NewPackage("main")
			r := emit(t, c, p)
			if _, err := r.Add(p); err != nil {
				t.Fatal(err)
			}
			checkTextAux(t, r.Text)
		})
	}
	// A result with no DWARF symbol is refused rather than written, so the
	// object cannot reach the linker without one.
	c := compile(t, "package main\n\nfunc f(a int) int { return a }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	r.DwarfInfo = nil
	if _, err := r.Add(p); err == nil || !strings.Contains(err.Error(), "DWARF subprogram") {
		t.Errorf("a result with no DWARF subprogram symbol gave %v", err)
	}
}

// TestLinksUnderTheDwarf5Experiment links and runs the same program the
// milestone links, with the DWARF 5 experiment forced on.
//
// internal/buildcfg puts dwarf5 in the baseline for every target except
// darwin, ios and aix. The pass it enables, cmd/link/internal/ld.writedebugaddr,
// walks every text symbol of a compilation unit and reads the relocations of
// its DWARF subprogram symbol without checking that there is one, so an object
// with a text symbol that has no such entry links on darwin and panics the
// linker everywhere else.
//
// This test is what makes that reachable here. It costs a standard library
// built with the experiment, which the build cache keeps, and it buys the
// whole class: any other linker pass that darwin skips runs here too.
func TestLinksUnderTheDwarf5Experiment(t *testing.T) {
	hostRunsNanogoOutput(t)
	goCmd := goTool(t)
	dwarf5Cfg.build(t)
	env := []string{"GOEXPERIMENT=dwarf5"}
	// The header line carries the experiment set and the linker refuses an
	// object whose header is not its own, so the object nanogo writes here
	// has to claim the same set as the caller and the standard library.
	tc := toolchainEnv(t, env)

	// A nanogo function called by gc-compiled code, which is the shape that
	// puts both compilers' text symbols in one compilation unit. A unit whose
	// functions all lack DWARF is textless and the pass skips it, so the
	// mixture is the case that has to be linked.
	c := compile(t, "package main\n\nfunc run(a, b int) int { return a * b }\n", "run")
	p := newMainPackage()
	r := emit(t, c, p)
	addFull(t, r, p)
	caller := exitWrapperEnv(t, goCmd, c.f.Sym, "run(6, 7)", "", "", env)
	out, err := runLinkedEnvCfg(t, goCmd, tc, dwarf5Cfg.link, p, caller, env)
	if err != nil {
		t.Fatalf("the linker or the program failed under dwarf5: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(out); got != "42" {
		t.Fatalf("the program printed %q, and the function returns 42", got)
	}
	t.Logf("linked and ran under GOEXPERIMENT=dwarf5, which returned %s", strings.TrimSpace(out))
}

// toolchainEnv reports what the installed toolchain writes under an
// environment, which obj.VerifyToolchain answers only for the one this process
// runs in. The probe is that function's: assemble an empty file and read the
// header line out of the result, because no go env variable reports the
// experiment set.
func toolchainEnv(t *testing.T, env []string) *obj.Toolchain {
	t.Helper()
	host := hostToolchain(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "probe.s")
	out := filepath.Join(dir, "probe.o")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goTool(t), "tool", "asm", "-p", "nanogo/probe", "-o", out, src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cannot probe the object format under %v: %v\n%s", env, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 || !bytes.HasPrefix(b, []byte("go object ")) {
		t.Fatalf("the probe under %v produced no object header", env)
	}
	return &obj.Toolchain{Header: string(b[:nl+1]), Magic: host.Magic}
}

// TestAddWritesTheAuxiliarySymbols checks that a result reaches an object with
// every symbol the format wants, in gc's order.
func TestAddWritesTheAuxiliarySymbols(t *testing.T) {
	tc := hostToolchain(t)
	c := compile(t, "package main\n\nfunc f(a int) int { return a }\n", "f")
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	if _, err := r.Add(p); err != nil {
		t.Fatal(err)
	}
	// gc's order, and the two FUNCDATA and two PCDATA entries are in the
	// index order the runtime defines, because the position of an entry is
	// its index.
	want := []obj.AuxType{
		obj.AuxFuncInfo,
		obj.AuxFuncdata, obj.AuxFuncdata,
		obj.AuxDwarfInfo,
		obj.AuxPcsp, obj.AuxPcfile, obj.AuxPcline,
		obj.AuxPcdata, obj.AuxPcdata,
	}
	if len(r.Text.Aux) != len(want) {
		t.Fatalf("%d auxiliary entries, want %d", len(r.Text.Aux), len(want))
	}
	for i, w := range want {
		if r.Text.Aux[i].Type != w {
			t.Errorf("auxiliary entry %d is %v, want %v", i, r.Text.Aux[i].Type, w)
		}
		if r.Text.Aux[i].Sym.IsZero() {
			t.Errorf("auxiliary entry %d names no symbol", i)
		}
	}
	// The object still has to be one the tools read.
	path := writeObject(t, p, tc)
	out, err := exec.Command(goTool(t), "tool", "nm", "-size", path).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm rejected the object: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "main.f") {
		t.Errorf("go tool nm does not report the function:\n%s", out)
	}
	if _, err := (&Result{}).Add(p); err == nil {
		t.Error("an empty result was added")
	}

	// The FuncInfo of a function with one word of arguments and no frame.
	info := r.FuncInfo.Data
	if len(info) < 24 {
		t.Fatalf("the FuncInfo is %d bytes", len(info))
	}
	if got := binary.LittleEndian.Uint32(info); got != uint32(r.Args) {
		t.Errorf("the FuncInfo says the arguments are %d bytes and the frame says %d", got, r.Args)
	}
	if got := binary.LittleEndian.Uint32(info[4:]); got != uint32(r.Locals) {
		t.Errorf("the FuncInfo says the locals are %d bytes and the frame says %d", got, r.Locals)
	}
	if got := binary.LittleEndian.Uint32(info[16:]); got != 1 {
		t.Errorf("the FuncInfo names %d files, want 1", got)
	}
}

// TestEmitRejectsWhatItCannotEncode checks the failures are named rather than
// producing an instruction that is not the one meant.
func TestEmitRejectsWhatItCannotEncode(t *testing.T) {
	// This test used to assert that a conditional set had no encoder, which
	// was true until obj/arm64 gained the conditional select class. Every
	// operation a rule can produce is now encodable, so there is no operation
	// left to reject and that half of the test is gone rather than weakened.
	//
	// ssagen.ARM64MissingEncoder and the guard that reads it stay, because
	// they are the counter for the next gap of this kind. The end of a gap is
	// not the end of the mechanism that reports one.
	c := hand(t, "cset", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		a := e.NewValue(0, ssa.OpArg, typeInt)
		b := e.NewValue(0, ssa.OpArg, typeInt)
		less := e.NewValue(0, ssa.OpLess, typeBool, a, b)
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, less, mem)
	})
	p := obj.NewPackage("main")
	if _, err := Emit(c.f, c.a, p, Options{Sym: "main.cset"}); err != nil {
		t.Fatalf("a comparison whose result is a value did not emit: %v", err)
	}

	// The arguments of Emit are checked, because a mismatched allocation
	// would index the wrong tables.
	if _, err := Emit(nil, c.a, p, Options{}); err == nil {
		t.Error("a nil function was emitted")
	}
	if _, err := Emit(c.f, nil, p, Options{}); err == nil {
		t.Error("a nil allocation was emitted")
	}
	if _, err := Emit(c.f, c.a, nil, Options{}); err == nil {
		t.Error("a nil package was emitted")
	}
	other := hand(t, "other", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, mem)
	})
	if _, err := Emit(c.f, other.a, p, Options{}); err == nil {
		t.Error("an allocation of another function was accepted")
	}
}

// TestEmitHarderShapes takes shapes the front end can build through the whole
// pass and asserts the disassembly is the instruction sequence they mean.
//
// None of them reaches the linker test, because the caller's signature there
// is fixed. What they exercise here is what the earlier tests do not: an
// argument that decomposes into several words, a check that branches to a
// block which calls the runtime and does not return, and a load through a
// pointer.
func TestEmitHarderShapes(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"deref", "func f(a *int) int { return *a }", []string{"MOVD (R0), R0"}},
		{"index", "func f(a []int, i int) int { return a[i] }",
			[]string{"R_CALLARM64:runtime.goPanicIndex", "MOVD (R"}},
		{"array", "func f(a [4]int, i int) int { return a[i] }",
			[]string{"R_CALLARM64:runtime.goPanicIndex"}},
		{"divide", "func f(a, b int) int { return a / b }",
			[]string{"DIV", "R_CALLARM64:runtime.panicdivide"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := compile(t, "package main\n\n"+tc.src+"\n", "f")
			p := obj.NewPackage("main")
			r := emit(t, c, p)
			text := disassemble(t, r, p)
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("the disassembly does not hold %q:\n%s", w, text)
				}
			}
			if r.Frame%16 != 0 {
				t.Errorf("frame %d is not 16-byte aligned", r.Frame)
			}
		})
	}
}

// TestPermuteBreaksACycle checks the parallel move.
//
// A set of moves that happen at one instant can be a cycle, and no order of
// two-operand moves realises one: the swap of two registers needs a third.
// specs/026-register-allocation.md reserves the scratch registers that make
// this possible and this is where one of them is used.
func TestPermuteBreaksACycle(t *testing.T) {
	tests := []struct {
		name string
		dst  []arm64.Reg
		src  []arm64.Reg
		want int // the number of instructions
	}{
		{"nothing", []arm64.Reg{arm64.R0}, []arm64.Reg{arm64.R0}, 0},
		{"chain", []arm64.Reg{arm64.R1, arm64.R2}, []arm64.Reg{arm64.R0, arm64.R1}, 2},
		{"swap", []arm64.Reg{arm64.R0, arm64.R1}, []arm64.Reg{arm64.R1, arm64.R0}, 3},
		{"three", []arm64.Reg{arm64.R0, arm64.R1, arm64.R2}, []arm64.Reg{arm64.R2, arm64.R0, arm64.R1}, 4},
		{"fanout", []arm64.Reg{arm64.R1, arm64.R2}, []arm64.Reg{arm64.R0, arm64.R0}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &emitter{opt: Options{Sym: "test.f"}}
			e.permute(append([]arm64.Reg{}, tc.dst...), append([]arm64.Reg{}, tc.src...))
			if err := e.err(); err != nil {
				t.Fatal(err)
			}
			if len(e.text) != tc.want {
				t.Fatalf("%d instructions, want %d", len(e.text), tc.want)
			}
			// Replay the moves over a model of the register file and check
			// that every destination holds the value its source had.
			var file [64]int
			for i := range file {
				file[i] = i
			}
			for _, w := range e.text {
				rd := arm64.Reg(w & 31)
				rm := arm64.Reg((w >> 16) & 31)
				file[rd] = file[rm]
			}
			for i := range tc.dst {
				if got, want := file[tc.dst[i]], int(tc.src[i]); got != want {
					t.Errorf("%v holds the value of register %d, want the value of %v", tc.dst[i], got, tc.src[i])
				}
			}
		})
	}
}

// TestCallWithStackArguments drives the outgoing argument area.
//
// Past the sixteenth integer register an argument travels in the caller's
// frame, at the bottom of it, because the callee reads it at its own stack
// pointer plus eight. The frame has to reserve that area and the emitter has
// to write into it.
func TestCallWithStackArguments(t *testing.T) {
	const n = 20
	callee := &ir.Object{Name: "main.g", Type: &ir.Type{Kind: ir.FuncKind, Size: 8, Align: 8}, Class: ir.ClassFunc}
	c := hand(t, "manyargs", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		args := make([]*ssa.Value, 0, n+1)
		for i := 0; i < n; i++ {
			args = append(args, e.NewValue(0, ssa.OpArg, typeInt))
		}
		args = append(args, mem)
		call := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, args...)
		call.Aux = callee
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, call)
	})
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	// The outgoing area holds the four arguments that did not fit in
	// registers, and the frame holds the area, the link register slot and the
	// caller's frame pointer reservation.
	if r.Frame < 8+n*8 {
		t.Errorf("frame %d, and %d bytes of arguments have to fit under it", r.Frame, n*8)
	}
	text := disassemble(t, r, p)
	if !strings.Contains(text, "R_CALLARM64:main.g") {
		t.Errorf("the call is not there:\n%s", text)
	}
	t.Logf("frame %d for %d arguments\n%s", r.Frame, n, text)
}

// TestIndirectCall checks the shape a closure call takes: the entry point is
// loaded into a register and the call is a branch through it, with no
// relocation because there is no symbol to name.
func TestIndirectCall(t *testing.T) {
	c := hand(t, "closure", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		clo := e.NewValue(0, ssa.OpArg, &ir.Type{Kind: ir.FuncKind, Size: 8, Align: 8, Name: "func()"})
		a := e.NewValue(0, ssa.OpArg, typeInt)
		call := e.NewValue(0, ssa.OpClosureCall, ssa.MemType, clo, a, mem)
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, call)
	})
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	text := disassemble(t, r, p)
	if !strings.Contains(text, "CALL (R") {
		t.Errorf("the call does not go through a register:\n%s", text)
	}
	// The closure travels in the closure register and not in the argument
	// sequence. specs/030-abi.md reserves R26 for it and the callee reads its
	// captured variables through it.
	if !strings.Contains(text, "R26") {
		t.Errorf("the closure does not reach the closure register:\n%s", text)
	}
	t.Logf("\n%s", text)
}

// TestEmitterRejectsBadInput covers the checks that turn a broken input into a
// named error rather than into an instruction that is not the one meant.
func TestEmitterRejectsBadInput(t *testing.T) {
	// A register the target does not describe is refused rather than
	// converted into whatever number it happens to be.
	e := &emitter{opt: Options{Sym: "test.f"}}
	if got, ok := e.reg(ssa.Reg(ssa.MaxRegs - 1)); ok || got != arm64.ZR {
		t.Errorf("a register outside the target converted to %v, ok=%v", got, ok)
	}
	if e.err() == nil {
		t.Error("a register outside the target was accepted")
	}

	e = &emitter{opt: Options{Sym: "test.f"}}
	e.slotOffset(3)
	if e.err() == nil {
		t.Error("a slot outside the frame was accepted")
	}

	e = &emitter{opt: Options{Sym: "test.f"}, frames: map[*ir.Object]int64{}}
	v := &ssa.Value{Op: ssa.OpARM64ADDframe}
	if _, ok := e.frameOff(v); ok {
		t.Error("a frame address with no object was accepted")
	}
	v.Aux = &ir.Object{Name: "x"}
	if _, ok := e.frameOff(v); ok {
		t.Error("a frame address of an object with no slot was accepted")
	}

	e = &emitter{opt: Options{Sym: "test.f"}}
	e.wordIf(0, false, "an instruction that does not fit")
	if e.err() == nil {
		t.Error("an instruction the encoder rejected was emitted")
	}
	if len(e.text) != 0 {
		t.Error("the rejected instruction reached the text")
	}

	// A load or a store of a type no width covers is named rather than
	// truncated.
	if _, ok := memOpFor(nil, true); ok {
		t.Error("a store of no type was accepted")
	}
	if _, ok := memOpFor(&ir.Type{Kind: ir.Int64, Size: 3, Align: 1}, true); ok {
		t.Error("a store of three bytes was accepted")
	}
	if _, ok := memOpFor(&ir.Type{Kind: ir.Int64, Size: 3, Align: 1}, false); ok {
		t.Error("a load of three bytes was accepted")
	}
}

// TestBranchOutOfRangeIsReported checks the failure specs/041 leaves to the
// linker for a call and to nobody for a branch inside a function.
func TestBranchOutOfRangeIsReported(t *testing.T) {
	e := &emitter{opt: Options{Sym: "test.f"}}
	e.blockPC = []int32{0, 1 << 22} // past the 19-bit conditional range
	e.branchToBlock(fixup{kind: fixCond, cond: arm64.EQ, block: 1})
	e.patch()
	if e.err() == nil {
		t.Fatal("a conditional branch out of range was encoded")
	}
	if !strings.Contains(e.err().Error(), "does not encode") {
		t.Errorf("the error is %q", e.err())
	}

	e = &emitter{opt: Options{Sym: "test.f"}}
	e.blockPC = []int32{0}
	e.branchToBlock(fixup{kind: fixB, block: 7})
	e.patch()
	if e.err() == nil {
		t.Fatal("a branch to a block that was not laid out was encoded")
	}
}

// TestEmitterNamesEveryFailure drives the paths that report a broken input.
//
// Every one of them is a compiler bug rather than a program that cannot be
// compiled, and specs/041-instruction-encoding.md asks for the operation and
// the value to be named rather than for a wrong instruction to be emitted. The
// test is white box because there is no input to this package that produces
// these shapes: they are what a pass above it would produce if it were wrong.
func TestEmitterNamesEveryFailure(t *testing.T) {
	newEmitter := func(f *ssa.Func, a *ssa.Alloc) *emitter {
		e := &emitter{
			f: f, a: a,
			opt:    Options{Sym: "test.f", File: "t.go"},
			pkg:    obj.NewPackage("test"),
			frames: map[*ir.Object]int64{},
			done:   map[ssa.ID]bool{},
		}
		e.syms = newSymbols(e.pkg)
		return e
	}
	// An allocation with room for a handful of values, none of which has a
	// home. Every table the emitter reads is indexed by value identifier.
	alloc := func(n int) *ssa.Alloc {
		a := &ssa.Alloc{
			Home:    make([]ssa.Loc, n),
			Fixed:   make([]ssa.Reg, n),
			Result:  make([]ssa.Reg, n),
			Args:    make([][]ssa.Reg, n),
			Remat:   make([]bool, n),
			Spilled: make([]bool, n),
		}
		for i := range a.Fixed {
			a.Fixed[i] = ssa.NoReg
			a.Result[i] = ssa.NoReg
		}
		return a
	}

	t.Run("value", func(t *testing.T) {
		f := ssa.NewFunc("f")
		e := newEmitter(f, alloc(8))
		b := f.Entry
		// A target-neutral operation that lowering should have replaced.
		e.value(b.NewValue(0, ssa.OpAdd, typeInt))
		// A call result that no call produced.
		e.value(b.NewValue(0, ssa.OpSelectN, typeInt))
		// An address of nothing.
		e.value(b.NewValue(0, ssa.OpARM64MOVDaddr, typeInt))
		// A frame address of nothing.
		e.value(b.NewValue(0, ssa.OpARM64ADDframe, typeInt))
		// A value with a width that the allocation gave nowhere to go. The
		// conditional set used to be listed here as the operation obj/arm64
		// could not encode; it encodes now, so it reports the same missing
		// register as any other operation and adds nothing this line does not
		// already cover.
		e.value(b.NewValue(0, ssa.OpARM64ADD, typeInt))
		err := e.err()
		if err == nil {
			t.Fatal("nothing was reported")
		}
		for _, want := range []string{"not a machine operation", "follows no call", "address of no symbol",
			"frame address of no object", "gives it no register"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the errors do not name %q:\n%v", want, err)
			}
		}
	})

	t.Run("copy", func(t *testing.T) {
		f := ssa.NewFunc("f")
		e := newEmitter(f, alloc(4))
		e.byID = make([]*ssa.Value, 4)
		e.copies([]ssa.Copy{{Value: 3}})
		if err := e.err(); err == nil || !strings.Contains(err.Error(), "not in the function") {
			t.Errorf("a copy of a value that is not there gave %v", e.err())
		}
		// A move between two slots has no instruction: the emitter reads a
		// slot into a register and writes a register to a slot, and the pair
		// is what a pass above it must not ask for.
		e = newEmitter(f, alloc(4))
		v := f.Entry.NewValue(0, ssa.OpARM64ADD, typeInt)
		e.move(ssa.SlotLoc(0), ssa.SlotLoc(1), v)
		if err := e.err(); err == nil || !strings.Contains(err.Error(), "no move") {
			t.Errorf("a move between two slots gave %v", e.err())
		}
	})

	t.Run("terminator", func(t *testing.T) {
		f := ssa.NewFunc("f")
		e := newEmitter(f, alloc(8))
		e.blockPC = make([]int32, 32)

		plain := f.NewBlock(ssa.BlockPlain)
		e.terminator(plain, nil) // no successor

		iff := f.NewBlock(ssa.BlockIf)
		e.terminator(iff, nil) // no successors and no control

		iff2 := f.NewBlock(ssa.BlockIf)
		iff2.AddEdgeTo(plain)
		iff2.AddEdgeTo(plain)
		iff2.Control = iff2.NewValue(0, ssa.OpARM64ADD, typeInt)
		e.terminator(iff2, nil) // a control that is not a branch

		iff3 := f.NewBlock(ssa.BlockIf)
		iff3.AddEdgeTo(plain)
		iff3.AddEdgeTo(plain)
		iff3.Control = iff3.NewValue(0, ssa.OpARM64BRcond, ssa.FlagsType)
		e.terminator(iff3, nil) // a conditional branch with no condition

		ret := f.NewBlock(ssa.BlockRet)
		e.terminator(ret, nil) // no return value

		bad := f.NewBlock(ssa.BlockInvalid)
		e.terminator(bad, nil)

		err := e.err()
		if err == nil {
			t.Fatal("nothing was reported")
		}
		for _, want := range []string{"is plain and has 0 successors", "is a branch with 0 successors",
			"does not end a block", "no condition", "returns and its control is", "has kind"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the errors do not name %q:\n%v", want, err)
			}
		}
	})

	t.Run("frame", func(t *testing.T) {
		f := ssa.NewFunc("f")
		e := newEmitter(f, alloc(4))
		v := f.Entry.NewValue(0, ssa.OpARM64ADD, &ir.Type{Kind: ir.Int64, Size: 3, Align: 1})
		e.load(arm64.R0, 0, v.Type)
		e.store(arm64.R0, 0, v.Type)
		if err := e.err(); err == nil || !strings.Contains(err.Error(), "no load") || !strings.Contains(err.Error(), "no store") {
			t.Errorf("a slot of a width that has no access gave %v", e.err())
		}

		// A frame object with no type is a layout that cannot be computed.
		e = newEmitter(f, alloc(4))
		e.f.Frame = []*ir.Object{{Name: "x"}}
		if err := e.layout(); err == nil {
			t.Error("a frame object with no type was laid out")
		}
	})

	t.Run("symbolName", func(t *testing.T) {
		e := newEmitter(ssa.NewFunc("f"), alloc(1))
		if got := e.symbolName(&ir.Object{Name: "main.G"}); got != "main.G" {
			t.Errorf("a name that is already a linker symbol became %q", got)
		}
		if got := e.symbolName(&ir.Object{Name: "G"}); got != "test.G" {
			t.Errorf("a package-level name became %q, want test.G", got)
		}
	})
}

// TestMaterialisationIntoTheOutgoingArea covers an outgoing argument the
// allocator decided to recompute rather than hold.
//
// The two constants at the end of the list are past the sixteenth register, so
// they are written into the outgoing area, and they have no home to be read
// from: each is recomputed into the scratch register and stored. There are two
// of them and not more because the allocator reserves two scratch registers
// and reports a call that needs a third.
func TestMaterialisationIntoTheOutgoingArea(t *testing.T) {
	const n = 18
	callee := &ir.Object{Name: "main.g", Type: &ir.Type{Kind: ir.FuncKind, Size: 8, Align: 8}, Class: ir.ClassFunc}
	c := hand(t, "outgoing", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		args := make([]*ssa.Value, 0, n+1)
		for i := 0; i < n-2; i++ {
			args = append(args, e.NewValue(0, ssa.OpArg, typeInt))
		}
		// A constant is rematerialised rather than held, so the store into
		// the outgoing area has to recompute it first.
		for i := 0; i < 2; i++ {
			k := e.NewValue(0, ssa.OpConstInt, typeInt)
			k.AuxInt = int64(i) + 1
			args = append(args, k)
		}
		args = append(args, mem)
		call := e.NewValue(0, ssa.OpStaticCall, ssa.MemType, args...)
		call.Aux = callee
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, call)
	})
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	text := disassemble(t, r, p)
	if !strings.Contains(text, "R_CALLARM64:main.g") {
		t.Errorf("the call is not there:\n%s", text)
	}
	t.Logf("frame %d\n%s", r.Frame, text)
}

// TestReturnValuesTravelInRegisters checks the placement of a return, and the
// report when a result does not fit the convention's registers.
func TestReturnValuesTravelInRegisters(t *testing.T) {
	build := func(n int) func(f *ssa.Func) {
		return func(f *ssa.Func) {
			e := f.Entry
			e.Kind = ssa.BlockRet
			mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
			args := make([]*ssa.Value, 0, n+1)
			for i := 0; i < n; i++ {
				args = append(args, e.NewValue(0, ssa.OpArg, typeInt))
			}
			args = append(args, mem)
			e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, args...)
		}
	}
	c := hand(t, "results", build(4))
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	text := disassemble(t, r, p)
	if !strings.Contains(text, "RET") {
		t.Errorf("no return:\n%s", text)
	}

	// Past the sixteenth register a result travels in the argument area. The
	// assignment writes an aggregate there with a block move, which needs an
	// address to copy from; a scalar has none, so the seventeenth integer
	// result stays an operand of the return and this package refuses it
	// rather than writing it into a register that is not its own.
	c = hand(t, "manyresults", build(20))
	_, err := Emit(c.f, c.a, obj.NewPackage("main"), Options{Sym: "main.manyresults"})
	if err == nil || !strings.Contains(err.Error(), "travels in the frame") {
		t.Errorf("a scalar result past the registers gave %v", err)
	}
	if n := len(c.f.ABI.Out); n != 20 {
		t.Errorf("%d results are placed, want 20", n)
	}
	for i := 16; i < 20; i++ {
		if c.f.ABI.Out[i].InReg {
			t.Errorf("result %d took a register, and there are sixteen", i)
		}
	}
}

// TestALargeParameterIsDescribedByTheArgumentsBitmap is the collector's half
// of homing a parameter in the incoming argument area.
//
// The parameter is no longer a local, so the locals bitmap does not describe
// it. The arguments bitmap has to, and it has to describe it by the
// parameter's own type: the pointer word is at offset 0 of a twenty-word
// struct that lives where the caller wrote it, and a collector that missed it
// would free an object the function still reads.
func TestALargeParameterIsDescribedByTheArgumentsBitmap(t *testing.T) {
	// The pointer is the last field, so the bitmap has to be twenty bits wide
	// and the bit that is set is the last one. A map sized from the pointer
	// alone would be one bit long and would describe the wrong word.
	const src = "package main\n\n" +
		"type BigP struct {\n\tA, B, C, D, E, F, G, H, I, J, K, L, M, N, O, Q, R, S, T int\n\tP *int\n}\n\n" +
		"func g(x int) int\n\n" +
		"func run(s BigP) int { return g(s.T) }\n"
	c := compile(t, src, "run")
	if len(c.f.Frame) != 0 {
		t.Errorf("the parameter is still a local, and its storage is the argument area")
	}
	if _, ok := c.f.ABI.ArgHome(c.f.ABI.In[0].Obj); !ok {
		t.Fatal("the parameter is not homed in the argument area")
	}
	p := obj.NewPackage("main")
	r := emit(t, c, p)
	if r.Args != 160 {
		t.Errorf("the argument area is %d bytes and the parameter is 160", r.Args)
	}
	// The bitmap symbol holds n, nbit and one bitmap per stack map index. The
	// arguments bitmap is the same at every safepoint, so there is one.
	data := r.Funcdata[ssa.FUNCDATA_ArgsPointerMaps].Data
	if len(data) < 9 {
		t.Fatalf("the arguments bitmap is %d bytes", len(data))
	}
	if nbit := int32(data[4]); nbit != 20 {
		t.Fatalf("the arguments bitmap has %d bits, and the parameter is twenty words", nbit)
	}
	// Twenty bits are three bytes, and the pointer is bit nineteen.
	if got := []byte{data[8], data[9], data[10]}; got[0] != 0 || got[1] != 0 || got[2] != 1<<3 {
		t.Errorf("the arguments bitmap is %#x, and the pointer is the twentieth word", got)
	}
	t.Logf("argument area %d bytes, %d bitmap bits", r.Args, data[4])
}

// TestTheAssignmentHasToBeThere covers the joins between this package and
// specs/030-abi.md's assignment pass.
//
// The placement is not computed here any more, so every way the assignment can
// fail to describe what the function holds is a named error and not a wrong
// offset.
func TestTheAssignmentHasToBeThere(t *testing.T) {
	c := hand(t, "noabi", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		a := e.NewValue(0, ssa.OpArg, typeInt)
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, a, mem)
	})
	abi := c.f.ABI
	c.f.ABI = nil
	if _, err := Emit(c.f, c.a, obj.NewPackage("main"), Options{Sym: "main.noabi"}); err == nil ||
		!strings.Contains(err.Error(), "no ABI assignment") {
		t.Errorf("a function with no assignment gave %v", err)
	}
	c.f.ABI = abi

	// An argument at an offset the assignment does not describe. Placing it
	// anyway would read one of the caller's words at random.
	for _, v := range c.f.Entry.Values {
		if v.Op == ssa.OpArg {
			v.AuxInt = 4096
		}
	}
	if _, err := Emit(c.f, c.a, obj.NewPackage("main"), Options{Sym: "main.noabi"}); err == nil ||
		!strings.Contains(err.Error(), "not a word of any parameter") {
		t.Errorf("an argument outside its parameter gave %v", err)
	}

	// A result a floating-point register would carry is refused, because
	// obj/arm64's encoder for it is specs/042 group 6 and the rules that need
	// it are not written.
	e := &emitter{opt: Options{Sym: "test.f"}, a: &ssa.Alloc{Target: ssa.NewArm64Target()}}
	f64 := &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}
	if _, err := e.resultPlaces([]*ir.Type{f64}); err == nil {
		t.Error("a floating-point result was placed, and no instruction writes it")
	}
	// A result of no width takes no register and is not an error.
	empty := &ir.Type{Kind: ir.Struct, Size: 0, Align: 1, Name: "struct{}"}
	if pl, err := e.resultPlaces([]*ir.Type{empty}); err != nil || len(pl) != 1 || pl[0].inReg {
		t.Errorf("a result of no width gave %v and %v", pl, err)
	}
	// A result the register set cannot hold has no form here.
	big := &ir.Type{Kind: ir.Struct, Size: 40, Align: 8, Name: "big"}
	for i := 0; i < 5; i++ {
		big.Fields = append(big.Fields, ir.Field{Name: "f", Type: typeInt, Offset: int64(i) * 8})
	}
	if _, err := e.resultPlaces([]*ir.Type{big}); err == nil {
		t.Error("a five-word result was placed as one register")
	}
}

// TestSmallHelpers covers the readers that a broken table would send off the
// end of a slice.
func TestSmallHelpers(t *testing.T) {
	e := &emitter{opt: Options{Sym: "test.f"}, a: &ssa.Alloc{Home: make([]ssa.Loc, 2)}}
	if got := e.home(nil); got.Kind != ssa.LocNone {
		t.Errorf("the home of no value is %v", got)
	}
	if got := e.home(&ssa.Value{ID: 9}); got.Kind != ssa.LocNone {
		t.Errorf("the home of a value outside the table is %v", got)
	}
	if got := e.valueOf(-1); got != nil {
		t.Errorf("a negative identifier gave %v", got)
	}
	if got := e.valueOf(3); got != nil {
		t.Errorf("an identifier outside the table gave %v", got)
	}

	// A constant that is a logical immediate is one instruction, and one that
	// is not is a move-wide sequence. Both reach the frame arithmetic.
	for _, v := range []int64{0xff0, 0x1010, 1 << 40} {
		e := &emitter{opt: Options{Sym: "test.f"}}
		e.constInto(arm64.RegAsmScratch, v)
		if err := e.err(); err != nil {
			t.Errorf("%#x: %v", v, err)
		}
		if len(e.text) == 0 {
			t.Errorf("%#x produced no instruction", v)
		}
	}

	// An argument of a width that has no store cannot be saved for the
	// stack-growth tail.
	e = &emitter{opt: Options{Sym: "test.f"}}
	e.argSpill(place{reg: arm64.R0, inReg: true, typ: &ir.Type{Kind: ir.Int64, Size: 3, Align: 1}}, true)
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "no load or store") {
		t.Errorf("an argument of three bytes gave %v", e.err())
	}
}

// TestCallFailuresAreNamed covers the checks around a call.
//
// A call is where the convention, the frame and the relocations meet, so it is
// where a pass above this one going wrong has the most ways to show up.
func TestCallFailuresAreNamed(t *testing.T) {
	newEmitter := func(f *ssa.Func, n int) *emitter {
		a := &ssa.Alloc{
			Target: ssa.NewArm64Target(),
			Home:   make([]ssa.Loc, n),
			Fixed:  make([]ssa.Reg, n),
			Result: make([]ssa.Reg, n),
			Args:   make([][]ssa.Reg, n),
			Remat:  make([]bool, n),
		}
		for i := range a.Fixed {
			a.Fixed[i] = ssa.NoReg
			a.Result[i] = ssa.NoReg
		}
		e := &emitter{f: f, a: a, opt: Options{Sym: "test.f", File: "t.go"},
			pkg: obj.NewPackage("test"), frames: map[*ir.Object]int64{}, done: map[ssa.ID]bool{}}
		e.syms = newSymbols(e.pkg)
		e.byID = make([]*ssa.Value, n)
		return e
	}

	f := ssa.NewFunc("f")
	b := f.Entry
	mem := b.NewValue(0, ssa.OpInitMem, ssa.MemType)

	// A static call whose callee is not a symbol.
	e := newEmitter(f, 32)
	e.value(b.NewValue(0, ssa.OpARM64CALLstatic, ssa.MemType, mem))
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "no callee symbol") {
		t.Errorf("a call to nothing gave %v", e.err())
	}

	// An indirect call with no entry point and no closure.
	e = newEmitter(f, 32)
	e.value(b.NewValue(0, ssa.OpARM64CALLclosure, ssa.MemType, mem))
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "no entry point") {
		t.Errorf("an indirect call with nothing to call gave %v", e.err())
	}

	// An indirect call the allocation named no entry register for. The
	// convention fixes that register (specs/030-abi.md), so an allocation
	// without it is an allocation this call cannot be emitted from.
	e = newEmitter(f, 32)
	entry := b.NewValue(0, ssa.OpARM64MOVDload, typeInt, mem)
	clo := b.NewValue(0, ssa.OpArg, typeInt)
	e.value(b.NewValue(0, ssa.OpARM64CALLinter, ssa.MemType, entry, clo, mem))
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "no register") {
		t.Errorf("an indirect call through nothing gave %v", e.err())
	}

	// An indirect call whose entry point lives in an argument register of the
	// same call. The arguments overwrite it before the branch, so the branch
	// must not read it there: the entry moves into the fixed register with
	// them, in the same instant, and the branch reads that register.
	e = newEmitter(f, 32)
	call := b.NewValue(0, ssa.OpARM64CALLinter, ssa.MemType, entry, clo, clo, mem)
	e.a.Home[entry.ID] = ssa.RegLoc(ssa.Reg(arm64.R0))
	e.a.Home[clo.ID] = ssa.RegLoc(ssa.Reg(arm64.R1))
	e.a.Args[call.ID] = []ssa.Reg{ssa.Reg(arm64.RegTrampLo), ssa.NoReg, ssa.NoReg, ssa.NoReg}
	e.value(call)
	if err := e.err(); err != nil {
		t.Errorf("an entry point in an argument register gave %v", err)
	}
	if got := e.text[len(e.text)-1]; got != arm64.Blr(arm64.RegTrampLo) {
		t.Errorf("the branch is %#08x, want BLR %v", got, arm64.RegTrampLo)
	}

	// A call whose results are not all named leaves a register unaccounted
	// for, and the placement of the rest would be wrong.
	e = newEmitter(f, 32)
	c2 := b.NewValue(0, ssa.OpARM64CALLstatic, ssa.MemType, mem)
	c2.Aux = &ir.Object{Name: "main.g"}
	sel := b.NewValue(0, ssa.OpSelectN, typeInt, c2)
	sel.AuxInt = 1
	e.a.Home[sel.ID] = ssa.RegLoc(ssa.Reg(arm64.R1))
	e.value(c2)
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "never named") {
		t.Errorf("a call with a gap in its results gave %v", e.err())
	}

	// An argument read where the function has none.
	e = newEmitter(f, 32)
	e.loadArg(clo)
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "outside the entry block") {
		t.Errorf("an argument that is not one gave %v", e.err())
	}

	// A value that is spilled to a slot whose width has no store.
	e = newEmitter(f, 32)
	wide := b.NewValue(0, ssa.OpARM64ADD, &ir.Type{Kind: ir.Int64, Size: 3, Align: 1})
	e.a.Home[wide.ID] = ssa.SlotLoc(0)
	e.slotOff = []int64{8}
	e.spill(wide)
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "no store") {
		t.Errorf("a spill of three bytes gave %v", e.err())
	}

	// A rematerialisation of a symbol address with no symbol.
	e = newEmitter(f, 32)
	e.remat(arm64.R0, b.NewValue(0, ssa.OpARM64MOVDaddr, typeInt))
	if err := e.err(); err == nil || !strings.Contains(err.Error(), "address of no symbol") {
		t.Errorf("an address of nothing gave %v", e.err())
	}
}

// TestStackArgumentsAreReadWhereTheyAreDefined is the property pre-colouring
// left behind.
//
// The arguments past the sixteenth register arrive in the caller's frame. The
// allocator may give one of them the register an earlier argument vacated,
// because their live ranges do not meet, so reading it at the entry would
// destroy that earlier argument. It is read at its own definition instead, and
// this checks that every one of them is read at all and from its own offset.
func TestStackArgumentsAreReadWhereTheyAreDefined(t *testing.T) {
	const n = 18
	c := hand(t, "shared", func(f *ssa.Func) {
		e := f.Entry
		e.Kind = ssa.BlockRet
		mem := e.NewValue(0, ssa.OpInitMem, ssa.MemType)
		sum := e.NewValue(0, ssa.OpArg, typeInt)
		for i := 1; i < n; i++ {
			a := e.NewValue(0, ssa.OpArg, typeInt)
			sum = e.NewValue(0, ssa.OpAdd, typeInt, sum, a)
		}
		e.Control = e.NewValue(0, ssa.OpMakeResult, ssa.MemType, sum, mem)
	})
	// The premise: the first sixteen arguments are fixed to the argument
	// registers and the last two are not.
	fixed := 0
	for _, v := range c.f.Entry.Values {
		if v.Op == ssa.OpArg && c.a.Fixed[v.ID] != ssa.NoReg {
			fixed++
		}
	}
	if fixed != 16 {
		t.Fatalf("%d arguments are pre-coloured, and the convention has sixteen integer argument registers", fixed)
	}

	p := obj.NewPackage("main")
	r := emit(t, c, p)
	text := disassemble(t, r, p)
	// The stack part of the argument area comes first and the spill slots of
	// the register arguments follow it, so the seventeenth argument is at
	// offset zero of the area and not at offset 128.
	for i := 16; i < n; i++ {
		off := r.Frame + 8 + int64(i-16)*8
		if !strings.Contains(text, fmt.Sprintf("MOVD %d(RSP), R", off)) {
			t.Errorf("argument %d is never read from %d(RSP):\n%s", i, off, text)
		}
	}
	t.Logf("frame %d, %d arguments in registers and %d in the area", r.Frame, fixed, n-fixed)
}

// TestRegRefusesAFloatRegister guards a check that was dead.
//
// reg() promises to refuse anything that is not an integer register of this
// target. It used to test `arm64.Reg(r) != x` with x already assigned
// arm64.Reg(r), which is always false, so a float register reached an integer
// encoder as its raw number and encoded as some unrelated integer register.
func TestRegRefusesAFloatRegister(t *testing.T) {
	var e emitter
	e.reg(ssa.Reg(arm64.F0))
	if e.err() == nil {
		t.Error("a float register passed the integer register check")
	}

	// An integer register still passes, or the guard is too broad and every
	// function stops compiling.
	var ok emitter
	if got, good := ok.reg(ssa.Reg(arm64.R5)); !good || got != arm64.R5 {
		t.Errorf("reg(R5) = %v, %v, want R5, true", got, good)
	}
	if err := ok.err(); err != nil {
		t.Errorf("an integer register was refused: %v", err)
	}
}

// TestRematerialisationIntoASlotStoresIt covers the move the allocator asks
// for when a rematerialisable value is live across a call.
//
// specs/026-register-allocation.md keeps a constant, a frame address and a
// symbol address out of the frame by recomputing them at each use. That is
// where the value comes from, not where it lives: a call clobbers every
// register under specs/030-abi.md, so a value live across one is given a slot
// as well. The emitter then has to write a computed value to memory, and no
// instruction does that, so it is the recomputation followed by a store.
//
// The case reached the emitter with no arm of the move table to take, and
// reported "no move from - to s1". It was found by compiling
//
//	for _, x := range xs { n = add(n, x) }
//
// where the slice's base address is rematerialisable and the call in the loop
// body is what makes it live across one. Before the fix nanogo refused that
// loop; after it, the program compiles and runs.
func TestRematerialisationIntoASlotStoresIt(t *testing.T) {
	f := ssa.NewFunc("f")
	a := &ssa.Alloc{
		Target:  ssa.NewArm64Target(),
		Home:    make([]ssa.Loc, 4),
		Fixed:   make([]ssa.Reg, 4),
		Result:  make([]ssa.Reg, 4),
		Args:    make([][]ssa.Reg, 4),
		Remat:   make([]bool, 4),
		Spilled: make([]bool, 4),
	}
	for i := range a.Fixed {
		a.Fixed[i] = ssa.NoReg
		a.Result[i] = ssa.NoReg
	}
	e := &emitter{
		f: f, a: a,
		opt:    Options{Sym: "test.f", File: "t.go"},
		pkg:    obj.NewPackage("test"),
		frames: map[*ir.Object]int64{},
		done:   map[ssa.ID]bool{},
	}
	e.syms = newSymbols(e.pkg)
	e.slotOff = []int64{8}

	c := f.Entry.NewValue(0, ssa.OpARM64MOVDconst, typeInt)
	c.AuxInt = 42

	// The source has no kind, which is how a rematerialised value is spelled.
	e.move(ssa.SlotLoc(0), ssa.Loc{}, c)
	if err := e.err(); err != nil {
		t.Fatalf("a rematerialisation into a slot gave %v", err)
	}
	if len(e.text) < 2 {
		t.Fatalf("%d instructions, want the recomputation and the store", len(e.text))
	}
	// The store is last, and it writes the reserved register rather than one
	// the allocator handed out, because no value of the function is in it.
	if got := e.text[len(e.text)-1]; got == 0 {
		t.Errorf("the store is not encoded")
	}
}

// TestFloatRegisterRefusalDoesNotReachTheEncoder covers the three paths that
// carried a refused register into obj/arm64 and crashed there.
//
// reg() refused a floating-point register, recorded the refusal and then
// returned arm64.ZR. ZR is a real register, so every caller went on with it
// and reached an encoder that wanted the floating-point file. regF panics on
// an integer register by design, so the compiler died with
//
//	panic: arm64: ZR used where the encoding means a floating-point register
//
// instead of reporting. Go's own test corpus crashed nanogo on align.go,
// float_lit.go, literal.go and rune.go this way, one panic per path: the
// rematerialisation of a floating-point constant, the destination of a
// floating-point load, and the operand of a floating-point compare.
//
// A compiler may decline to compile a program. It may not crash on one.
func TestFloatRegisterRefusalDoesNotReachTheEncoder(t *testing.T) {
	f64 := &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}

	newEmitter := func(f *ssa.Func, n int) *emitter {
		a := &ssa.Alloc{
			Target:  ssa.NewArm64Target(),
			Home:    make([]ssa.Loc, n),
			Fixed:   make([]ssa.Reg, n),
			Result:  make([]ssa.Reg, n),
			Args:    make([][]ssa.Reg, n),
			Remat:   make([]bool, n),
			Spilled: make([]bool, n),
		}
		for i := range a.Fixed {
			a.Fixed[i] = ssa.NoReg
			a.Result[i] = ssa.NoReg
		}
		e := &emitter{
			f: f, a: a,
			opt:    Options{Sym: "test.f", File: "t.go"},
			pkg:    obj.NewPackage("test"),
			frames: map[*ir.Object]int64{},
			done:   map[ssa.ID]bool{},
		}
		e.syms = newSymbols(e.pkg)
		e.slotOff = []int64{8}
		return e
	}

	// float_lit.go: a floating-point constant is rematerialised into the
	// register the allocation named, and the constant's encoder is FMOV with
	// an immediate.
	t.Run("remat", func(t *testing.T) {
		f := ssa.NewFunc("f")
		e := newEmitter(f, 8)
		c := f.Entry.NewValue(0, ssa.OpARM64FMOVconst, f64)
		c.AuxInt = int64(math.Float64bits(1.5))
		e.move(ssa.RegLoc(ssa.Reg(arm64.F1)), ssa.Loc{}, c)
		mustRefuseFloat(t, e)
	})

	// align.go: the destination of a floating-point load.
	t.Run("result", func(t *testing.T) {
		f := ssa.NewFunc("f")
		e := newEmitter(f, 8)
		ptr := f.Entry.NewValue(0, ssa.OpARM64MOVDconst, typeInt)
		mem := f.Entry.NewValue(0, ssa.OpInitMem, typeInt)
		ld := f.Entry.NewValue(0, ssa.OpARM64FMOVDload, f64, ptr, mem)
		e.a.Result[ptr.ID] = ssa.Reg(arm64.R0)
		e.a.Home[ptr.ID] = ssa.RegLoc(ssa.Reg(arm64.R0))
		e.a.Args[ld.ID] = []ssa.Reg{ssa.Reg(arm64.R0), ssa.NoReg}
		e.a.Result[ld.ID] = ssa.Reg(arm64.F0)
		e.value(ld)
		mustRefuseFloat(t, e)
	})

	// literal.go: an operand of a floating-point compare.
	t.Run("operand", func(t *testing.T) {
		f := ssa.NewFunc("f")
		e := newEmitter(f, 8)
		x := f.Entry.NewValue(0, ssa.OpArg, f64)
		y := f.Entry.NewValue(0, ssa.OpArg, f64)
		cmp := f.Entry.NewValue(0, ssa.OpARM64FCMPD, typeInt, x, y)
		e.a.Args[cmp.ID] = []ssa.Reg{ssa.Reg(arm64.F1), ssa.Reg(arm64.F2)}
		if _, ok := e.operands(cmp); ok {
			t.Error("operands accepted a floating-point register")
		}
		mustRefuseFloat(t, e)
	})
}

// mustRefuseFloat asserts that the emitter reported a floating-point register
// it cannot generate code for, and emitted nothing.
func mustRefuseFloat(t *testing.T, e *emitter) {
	t.Helper()
	err := e.err()
	if err == nil {
		t.Fatal("a floating-point register was accepted")
	}
	if !strings.Contains(err.Error(), "floating-point") {
		t.Errorf("the refusal does not name the construct: %v", err)
	}
	if len(e.text) != 0 {
		t.Errorf("%d instructions reached the text after a refusal", len(e.text))
	}
}

// TestIncomingRefusesAFloatParameter covers the third door into the
// floating-point gap.
//
// valuePlaces refuses a floating-point value at a call site and at a return,
// and reg refuses one the allocation put in a register. Neither covers a
// parameter: the stack-growth tail spills every incoming register to the
// argument area, it reads the placement rather than a value of the function,
// and argSpill has no failure path. So a floating-point parameter reached
// obj/arm64's integer store and it panicked:
//
//	panic: arm64: F0 used where the encoding means an integer register
//
// Go's own test corpus reaches it on test/torture.go, whose determinant takes
// a [4][4]float64 whose first word arrives in F0.
func TestIncomingRefusesAFloatParameter(t *testing.T) {
	f64 := &ir.Type{Kind: ir.Float64, Size: 8, Align: 8, Name: "float64"}
	obj := &ir.Object{Name: "x"}

	f := ssa.NewFunc("f")
	f.Entry.Kind = ssa.BlockRet
	f.ABI = &ssa.ABI{
		In: []ssa.ABIValue{{
			Obj: obj, Type: f64, InReg: true,
			Parts: []ssa.ABIPart{{Type: f64, Reg: ssa.Reg(arm64.F0)}},
		}},
	}
	e := &emitter{f: f, opt: Options{Sym: "test.f"}}
	err := e.incoming()
	if err == nil {
		t.Fatal("a floating-point parameter was accepted")
	}
	if !strings.Contains(err.Error(), "floating-point") || !strings.Contains(err.Error(), "x") {
		t.Errorf("the refusal names neither the construct nor the parameter: %v", err)
	}

	// An integer parameter still passes, or no function compiles.
	f.ABI.In[0].Type = typeInt
	f.ABI.In[0].Parts[0] = ssa.ABIPart{Type: typeInt, Reg: ssa.Reg(arm64.R0)}
	if err := e.incoming(); err != nil {
		t.Errorf("an integer parameter was refused: %v", err)
	}
}
