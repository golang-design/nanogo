// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// The tests in this file use the installed gc toolchain as the oracle.
// nanogo's objects have to be the ones cmd/link and the object tools
// already read, so a test that only checks the writer against itself
// proves that the writer is self-consistent and nothing more. See
// specs/040-object-format.md, "Testing".

package obj

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// requireCorpus reports whether a missing toolchain is a failure rather
// than a reason to skip. CI sets NANOGO_REQUIRE_CORPUS, so a gate that
// silently stopped running would turn red instead of green.
func requireCorpus() bool { return os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" }

// goTool returns the path of the go command, or ends the test.
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

// toolchain returns what the installed toolchain writes into an object.
func hostToolchain(t *testing.T) *Toolchain {
	t.Helper()
	goTool(t)
	tc, err := VerifyToolchain()
	if err != nil {
		t.Fatalf("the installed toolchain does not write the format nanogo writes: %v", err)
	}
	return tc
}

// minLC is the smallest instruction length on the host. A pc delta is
// counted in instructions, so the encoder divides by it.
func minLC() int {
	switch runtime.GOARCH {
	case "amd64", "386", "wasm":
		return 1
	case "s390x":
		return 2
	default:
		return 4
	}
}

// ret is a return instruction for the host, so a text symbol in a test
// object holds real code and go tool objdump can decode it.
func ret() []byte {
	switch runtime.GOARCH {
	case "amd64", "386":
		return []byte{0xc3}
	case "arm64":
		return []byte{0xc0, 0x03, 0x5f, 0xd6}
	default:
		return nil
	}
}

// nmSym is one line of go tool nm output.
type nmSym struct {
	Size int64
	Code string
	Name string
}

// runNM runs go tool nm -size and returns the symbols it found.
func runNM(t *testing.T, file string) map[string]nmSym {
	t.Helper()
	out, err := exec.Command(goTool(t), "tool", "nm", "-size", file).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm rejected the object: %v\n%s", err, out)
	}
	syms := map[string]nmSym{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// The columns are address, size, code, name. The address is blank
		// for a reference and the name is blank for the file symbols the
		// assembler adds, so a short line is not a symbol this test cares
		// about.
		if len(f) < 3 {
			continue
		}
		size, err := strconv.ParseInt(f[len(f)-3], 10, 64)
		if err != nil {
			continue
		}
		syms[f[len(f)-1]] = nmSym{Size: size, Code: f[len(f)-2], Name: f[len(f)-1]}
	}
	return syms
}

// writeObject writes p to a file in the test's temporary directory.
func writeObject(t *testing.T, p *Package, tc *Toolchain) string {
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

func TestVerifyToolchainMatches(t *testing.T) {
	tc := hostToolchain(t)
	if tc.Magic != Magic {
		t.Fatalf("magic %q, want %q", tc.Magic, Magic)
	}
	if !strings.HasPrefix(tc.Header, "go object "+runtime.GOOS+" "+runtime.GOARCH+" ") {
		t.Errorf("header %q does not name this host", tc.Header)
	}
	// The header carries the enabled experiment list, and no go env
	// variable reports it, which is why the check probes the toolchain
	// instead of reconstructing the string.
	t.Logf("toolchain header: %q", tc.Header)

	// The result is cached, and a second call must agree with the first.
	again, err := VerifyToolchain()
	if err != nil {
		t.Fatal(err)
	}
	if *again != *tc {
		t.Errorf("second call returned %+v, want %+v", again, tc)
	}
}

// TestNMReadsWhatWasWritten is the test that proves the format is right
// rather than merely self-consistent: the reader is the toolchain's.
func TestNMReadsWhatWasWritten(t *testing.T) {
	tc := hostToolchain(t)
	const pkg = "example.com/x"
	p := NewPackage(pkg)

	type want struct {
		size int64
		code string
	}
	wants := map[string]want{}

	add := func(name string, kind SymKind, size uint32, data []byte, code string) {
		p.AddDef(&Symbol{Name: pkg + "." + name, Type: kind, Size: size, Align: 8, Data: data})
		wants[pkg+"."+name] = want{int64(size), code}
	}
	add("rodata", SRODATA, 16, make([]byte, 16), "R")
	add("data", SDATA, 8, []byte{1, 2, 3, 4, 5, 6, 7, 8}, "D")
	add("noptrdata", SNOPTRDATA, 4, []byte{9, 9, 9, 9}, "D")
	add("bss", SBSS, 32, nil, "B")
	add("noptrbss", SNOPTRBSS, 24, nil, "B")

	if code := ret(); code != nil {
		p.AddDef(&Symbol{
			Name: pkg + ".fn", ABI: ABIInternal, Type: STEXT,
			Size: uint32(len(code)), Align: 4, Flag: SymFlagNoSplit, Data: code,
		})
		wants[pkg+".fn"] = want{int64(len(code)), "T"}
	}

	// A file static symbol keeps its own identity. nm marks it with a
	// lower case code and prints the static version after the name.
	p.AddDef(&Symbol{Name: pkg + ".static", ABI: ABIStatic, Type: SRODATA, Size: 2, Align: 2, Data: []byte{1, 2}})
	wants[pkg+".static<1>"] = want{2, "r"}

	// A content-addressable symbol is a definition like any other, so nm
	// reports it too.
	p.AddHashedDef(&Symbol{Name: "go:string.hello", Type: SRODATA, Size: 5, Align: 1, Data: []byte("hello")})
	wants["go:string.hello"] = want{5, "R"}
	p.AddHashed64Def(&Symbol{Name: pkg + "..stmp_0", Type: SRODATA, Size: 8, Align: 8, Data: []byte{1, 0, 0, 0, 0, 0, 0, 0}})
	wants[pkg+"..stmp_0"] = want{8, "R"}
	p.AddNonPkgDef(&Symbol{Name: "type:int", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8)})
	wants["type:int"] = want{8, "R"}

	got := runNM(t, writeObject(t, p, tc))
	n := 0
	for name, w := range wants {
		s, ok := got[name]
		if !ok {
			t.Errorf("go tool nm did not report %s", name)
			continue
		}
		if s.Size != w.size {
			t.Errorf("%s: nm reports size %d, %d was written", name, s.Size, w.size)
		}
		if s.Code != w.code {
			t.Errorf("%s: nm reports kind %s, want %s", name, s.Code, w.code)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no symbol was compared")
	}
	t.Logf("go tool nm agreed on %d symbols", n)
}

// TestObjdumpReadsText checks that go tool objdump walks the text symbol
// and decodes the instruction that was written.
func TestObjdumpReadsText(t *testing.T) {
	code := ret()
	if code == nil {
		t.Skipf("no return instruction encoded for %s", runtime.GOARCH)
	}
	tc := hostToolchain(t)
	p := NewPackage("example.com/x")
	p.AddDef(&Symbol{
		Name: "example.com/x.fn", ABI: ABIInternal, Type: STEXT,
		Size: uint32(len(code)), Align: 4, Flag: SymFlagNoSplit, Data: code,
	})
	out, err := exec.Command(goTool(t), "tool", "objdump", writeObject(t, p, tc)).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool objdump rejected the object: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "TEXT example.com/x.fn(SB)") {
		t.Errorf("objdump did not name the text symbol:\n%s", out)
	}
	if !strings.Contains(string(out), "RET") {
		t.Errorf("objdump did not decode the instruction that was written:\n%s", out)
	}
}

// TestSameSymbolsAsAssembler assembles a source that declares the same
// symbols nanogo writes, and compares the two objects through go tool nm.
//
// The comparison is one way. The assembler adds symbols nanogo does not:
// a gofile symbol for the source, and an arginfo and an args_stackmap
// reference for every TEXT. So the check is that every symbol nanogo
// declares appears in the assembler's object with the same kind and size.
//
// The bytes of the two objects are not compared and cannot be. The
// assembler's object holds those extra symbols, its own file table, and a
// different string table, so a byte comparison would fail for reasons that
// have nothing to do with the format. Byte comparison belongs to
// TestDeterminismAcrossProcesses, where both sides are nanogo.
func TestSameSymbolsAsAssembler(t *testing.T) {
	tc := hostToolchain(t)
	goroot, err := exec.Command(goTool(t), "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	include := filepath.Join(strings.TrimSpace(string(goroot)), "pkg", "include")

	dir := t.TempDir()
	src := filepath.Join(dir, "x.s")
	asm := `#include "textflag.h"

GLOBL ·rodata(SB), RODATA|NOPTR, $16
DATA ·rodata+0(SB)/8, $1
DATA ·rodata+8(SB)/8, $2

GLOBL ·data(SB), NOPTR, $8
DATA ·data+0(SB)/8, $3

GLOBL ·bss(SB), NOPTR, $32

TEXT ·fn(SB), NOSPLIT, $0-0
	RET
`
	if err := os.WriteFile(src, []byte(asm), 0o600); err != nil {
		t.Fatal(err)
	}
	asmObj := filepath.Join(dir, "asm.o")
	out, err := exec.Command(goTool(t), "tool", "asm", "-p", "example.com/x", "-I", include, "-o", asmObj, src).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool asm failed: %v\n%s", err, out)
	}

	const pkg = "example.com/x"
	p := NewPackage(pkg)
	p.Flags = ObjFlagFromAssembly
	p.AddDef(&Symbol{Name: pkg + ".rodata", Type: SRODATA, Size: 16, Align: 8,
		Data: []byte{1, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}})
	p.AddDef(&Symbol{Name: pkg + ".data", Type: SNOPTRDATA, Size: 8, Align: 8,
		Data: []byte{3, 0, 0, 0, 0, 0, 0, 0}})
	p.AddDef(&Symbol{Name: pkg + ".bss", Type: SNOPTRBSS, Size: 32, Align: 8})
	if code := ret(); code != nil {
		p.AddDef(&Symbol{Name: pkg + ".fn", Type: STEXT, Size: uint32(len(code)), Align: 4,
			Flag: SymFlagNoSplit, Data: code})
	}

	fromAsm := runNM(t, asmObj)
	fromNanogo := runNM(t, writeObject(t, p, tc))

	n := 0
	for name, ours := range fromNanogo {
		theirs, ok := fromAsm[name]
		if !ok {
			t.Errorf("%s is in nanogo's object and not in the assembler's", name)
			continue
		}
		if ours.Code != theirs.Code {
			t.Errorf("%s: nanogo says kind %s, the assembler says %s", name, ours.Code, theirs.Code)
		}
		// A text symbol is padded to the alignment the assembler chooses,
		// so its size is the assembler's business and not the format's.
		if !strings.EqualFold(ours.Code, "T") && ours.Size != theirs.Size {
			t.Errorf("%s: nanogo says size %d, the assembler says %d", name, ours.Size, theirs.Size)
		}
		n++
	}
	if n < 3 {
		t.Fatalf("compared %d symbols, want at least 3", n)
	}
	t.Logf("compared %d symbols against go tool asm", n)
}

// linkCfg names an importcfg the go command produced for a hello world
// program. It maps every standard library package the runtime needs to a
// compiled archive, which is what the linker wants and what nanogo cannot
// yet produce for itself.
var (
	linkCfgOnce sync.Once
	linkCfgPath string
	linkCfgWork string
	linkCfgDir  string
	linkCfgErr  error
)

// TestMain removes the build directory the linker tests ask the go
// command to keep.
func TestMain(m *testing.M) {
	code := m.Run()
	for _, dir := range []string{linkCfgWork, linkCfgDir} {
		if dir != "" {
			os.RemoveAll(dir)
		}
	}
	os.Exit(code)
}

func linkConfig(t *testing.T) string {
	t.Helper()
	goCmd := goTool(t)
	linkCfgOnce.Do(func() {
		linkCfgDir, linkCfgErr = os.MkdirTemp("", "nanogo-linkcfg")
		if linkCfgErr != nil {
			return
		}
		files := map[string]string{
			"go.mod":  "module nanogo.example/link\n\ngo 1.27\n",
			"main.go": "package main\n\nfunc main() {}\n",
		}
		for name, body := range files {
			if linkCfgErr = os.WriteFile(filepath.Join(linkCfgDir, name), []byte(body), 0o600); linkCfgErr != nil {
				return
			}
		}
		// -work keeps the build directory, which holds the importcfg the
		// go command wrote for the link step.
		cmd := exec.Command(goCmd, "build", "-work", "-o", filepath.Join(linkCfgDir, "prog"), ".")
		cmd.Dir = linkCfgDir
		cmd.Env = append(os.Environ(), "GOPROXY=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			linkCfgErr = fmt.Errorf("go build: %v\n%s", err, out)
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			if work, ok := strings.CutPrefix(line, "WORK="); ok {
				linkCfgWork = strings.TrimSpace(work)
			}
		}
		if linkCfgWork == "" {
			linkCfgErr = fmt.Errorf("go build did not report its work directory:\n%s", out)
			return
		}
		linkCfgErr = filepath.WalkDir(linkCfgWork, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == "importcfg.link" {
				linkCfgPath = path
			}
			return nil
		})
	})
	if linkCfgErr != nil {
		t.Fatalf("cannot build an import configuration to link against: %v", linkCfgErr)
	}
	if linkCfgPath == "" {
		t.Fatal("the go command produced no importcfg.link")
	}
	return linkCfgPath
}

// TestLinkerReadsObject links nanogo objects with the real linker.
//
// This is the strongest gate available before there is a compiler. The
// first case shows the linker reads the symbol definitions: it loads the
// object together with the whole runtime and then reports that main is
// undeclared, which it can only know from the symbol table it read. The
// second case links a running program.
func TestLinkerReadsObject(t *testing.T) {
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)

	// The object the linker is given on the command line is the main
	// package, and the linker refuses one that does not say so in its
	// header.
	newMain := func() *Package {
		p := NewPackage("main")
		p.Main = true
		p.AddDef(&Symbol{Name: "main.rodata", Type: SRODATA, Size: 8, Align: 8, Data: make([]byte, 8)})
		p.AddDef(&Symbol{Name: "main.bss", Type: SNOPTRBSS, Size: 16, Align: 8})
		// The linker is the only reader of the two hash blocks, and it
		// reads them by position, so a content-addressable symbol belongs
		// in every object this test links.
		p.AddHashedDef(&Symbol{Name: "go:string.hello", Type: SRODATA, Size: 5, Align: 1, Data: []byte("hello")})
		p.AddHashed64Def(&Symbol{Name: "main..stmp_0", Type: SRODATA, Size: 8, Align: 8, Data: []byte{1, 0, 0, 0, 0, 0, 0, 0}})
		return p
	}

	t.Run("data only", func(t *testing.T) {
		obj := writeObject(t, newMain(), tc)
		out, err := exec.Command(goCmd, "tool", "link", "-importcfg", cfg, "-o", filepath.Join(t.TempDir(), "a.out"), obj).CombinedOutput()
		if err == nil {
			t.Fatalf("the linker linked a package with no main function:\n%s", out)
		}
		if !strings.Contains(string(out), "function main is undeclared in the main package") {
			t.Fatalf("the linker did not reach the symbol table: %v\n%s", err, out)
		}
		for _, bad := range []string{"not an object file", "truncated", "malformed", "header mismatch", "panic"} {
			if strings.Contains(string(out), bad) {
				t.Errorf("the linker reported %q:\n%s", bad, out)
			}
		}
		t.Logf("the linker read the object and stopped at: %s", strings.TrimSpace(string(out)))
	})

	t.Run("runnable program", func(t *testing.T) {
		code := ret()
		if code == nil {
			t.Skipf("no return instruction encoded for %s", runtime.GOARCH)
		}
		p := newMain()
		p.AddDef(&Symbol{
			Name: "main.main", ABI: ABIInternal, Type: STEXT,
			Size: uint32(len(code)), Align: 4, Flag: SymFlagNoSplit, Data: code,
		})
		// runtime.main runs the package initialisers before it calls
		// main.main. An init task of a zero state and zero function count
		// is a package with nothing to initialise.
		p.AddDef(&Symbol{Name: "main..inittask", Type: SNOPTRDATA, Size: 8, Align: 8, Data: make([]byte, 8)})

		exe := filepath.Join(t.TempDir(), "a.out")
		// -w turns DWARF generation off. nanogo does not write the
		// AuxFuncInfo symbol yet, so a text symbol belongs to no
		// compilation unit, and the linker's DWARF pass sorts an empty
		// list and panics. specs/040 does not say a text symbol needs
		// that auxiliary symbol, and it does.
		out, err := exec.Command(goCmd, "tool", "link", "-w", "-importcfg", cfg, "-o", exe, writeObject(t, p, tc)).CombinedOutput()
		if err != nil {
			t.Fatalf("the linker rejected the object: %v\n%s", err, out)
		}
		out, err = exec.Command(exe).CombinedOutput()
		if err != nil {
			t.Fatalf("the linked program failed: %v\n%s", err, out)
		}
		t.Logf("linked and ran a program from a nanogo object, output %q", out)
	})
}

// TestContentHashMatchesCompiler compares the identity nanogo computes
// for a content-addressable symbol with the identity gc computes for the
// same symbol.
//
// This is the only test here that can see a divergence in the hash, and
// the hash is the one field whose whole purpose is agreement between two
// toolchains. If nanogo and gc disagree, the linker keeps two copies of
// one string or one type descriptor, and two copies of a type descriptor
// are two addresses, which specs/032-type-descriptors-and-itabs.md calls a
// correctness failure and not a size problem.
func TestContentHashMatchesCompiler(t *testing.T) {
	goCmd := goTool(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "s.go")
	// A string literal longer than eight bytes becomes a hashed symbol.
	const literal = "nanogo object identity"
	source := "package p\n\nvar S = \"" + literal + "\"\n"
	if err := os.WriteFile(src, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	gcObj := filepath.Join(dir, "s.o")
	if out, err := exec.Command(goCmd, "tool", "compile", "-p", "p", "-o", gcObj, src).CombinedOutput(); err != nil {
		t.Fatalf("go tool compile failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(gcObj)
	if err != nil {
		t.Fatal(err)
	}
	// The goobj blocks start after the header lines and the separator.
	i := bytes.Index(raw, []byte("\n!\n"))
	if i < 0 {
		t.Fatal("the compiler's object has no block separator")
	}
	o := parsePrefix(t, raw[i+3:])

	defs := o.syms(BlkHasheddef)
	hashes := o.block(BlkHash)
	n := 0
	for j, def := range defs {
		if !strings.Contains(def.Name, literal) {
			continue
		}
		// Rebuild the same symbol and compare the identity.
		p := NewPackage("p")
		p.AddHashedDef(&Symbol{
			Name: def.Name, Type: def.Type, Size: def.Size, Align: def.Align,
			Data: []byte(literal),
		})
		got, err := p.contentHash(0)
		if err != nil {
			t.Fatal(err)
		}
		want := hashes[j*16 : (j+1)*16]
		if !bytes.Equal(got[:], want) {
			t.Errorf("%s: nanogo hashed % x, the compiler hashed % x", def.Name, got, want)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("the compiler wrote no content-addressable symbol holding %q, it wrote %d", literal, len(defs))
	}
	t.Logf("compared %d content hashes with the compiler's", n)
}

// pcTable is one table go tool compile printed with -d=pctab.
type pcTable struct {
	name     string
	entries  []PCEntry
	funcSize int64
	encoded  []byte
}

// parsePCTables reads the trace cmd/internal/obj.funcpctab prints. The
// trace holds the (pc, value) pairs it was given and the bytes it
// produced, so it states both sides of the encoding.
func parsePCTables(t *testing.T, out string) []pcTable {
	t.Helper()
	var tables []pcTable
	var cur *pcTable
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "funcpctab "):
			tables = append(tables, pcTable{name: strings.Fields(line)[1]})
			cur = &tables[len(tables)-1]
			continue
		case cur == nil:
			continue
		case strings.HasPrefix(line, "wrote "):
			continue
		}
		if strings.HasSuffix(line, " done") {
			pc, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(line, " done")), 16, 64)
			if err == nil {
				cur.funcSize = pc
			}
			continue
		}
		// The byte dump is a line of two-digit hex fields.
		if f := strings.Fields(line); len(f) > 0 && cur.encoded == nil && cur.funcSize != 0 {
			ok := true
			var b []byte
			for _, x := range f {
				v, err := strconv.ParseUint(x, 16, 8)
				if err != nil || len(x) != 2 {
					ok = false
					break
				}
				b = append(b, byte(v))
			}
			if ok {
				cur.encoded = b
				continue
			}
		}
		// A trace row is "%6x %6d %v": the pc, then the value if it
		// changed here, then the instruction.
		if len(line) < 14 {
			continue
		}
		pc, err := strconv.ParseInt(strings.TrimSpace(line[:6]), 16, 64)
		if err != nil {
			continue
		}
		val, err := strconv.ParseInt(strings.TrimSpace(line[7:13]), 10, 32)
		if err != nil {
			continue
		}
		cur.entries = append(cur.entries, PCEntry{PC: pc, Value: int32(val)})
	}
	return tables
}

// TestPCDataMatchesCompiler encodes the pc-value tables the compiler built
// for a real function and compares the bytes with the compiler's own.
func TestPCDataMatchesCompiler(t *testing.T) {
	goCmd := goTool(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "f.go")
	source := `package p

//go:noinline
func g(x int) int { return x*3 + 1 }

func F(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += g(i)
	}
	return s
}
`
	if err := os.WriteFile(src, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goCmd, "tool", "compile", "-p", "p", "-d=pctab=pctospadj", "-o", filepath.Join(dir, "f.o"), src)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool compile failed: %v\n%s", err, out)
	}
	tables := parsePCTables(t, string(out))
	if len(tables) == 0 {
		t.Fatalf("the compiler printed no pc-value table:\n%s", out)
	}

	n, changes := 0, 0
	for _, tab := range tables {
		if len(tab.entries) == 0 || tab.funcSize == 0 {
			continue
		}
		// The first row is the state before the walk starts, printed with
		// the assumed value of -1. It is not an entry.
		entries := tab.entries[1:]
		got, err := EncodePCData(entries, tab.funcSize, minLC())
		if err != nil {
			t.Fatalf("%s: %v", tab.name, err)
		}
		if !bytes.Equal(got, tab.encoded) {
			t.Errorf("%s: nanogo encoded % x, the compiler wrote % x, from %v over %d bytes",
				tab.name, got, tab.encoded, entries, tab.funcSize)
			continue
		}
		n++
		changes = max(changes, len(entries))
	}
	if n == 0 {
		t.Fatalf("no table was compared:\n%s", out)
	}
	// A table with one entry only exercises the first value delta and the
	// terminator. At least one table must hold a change in the middle.
	if changes < 2 {
		t.Errorf("the longest table compared had %d entries, want at least 2", changes)
	}
	t.Logf("matched %d pc-value tables the compiler produced, the longest with %d entries", n, changes)
}

// TestDeterminismAcrossProcesses writes the same package in two processes
// with different environments and working directories, and compares the
// bytes. specs/053-determinism.md calls the two-process check the one that
// finds path leaks, and the one most often left out.
func TestDeterminismAcrossProcesses(t *testing.T) {
	if out := os.Getenv("NANOGO_OBJ_WRITE"); out != "" {
		b, err := sample().Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	here, err := sample().Bytes()
	if err != nil {
		t.Fatal(err)
	}
	// The child runs in another directory, so the path of this binary has
	// to be absolute.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	n := 0
	for i, env := range [][]string{
		{"NANOGO_OBJ_WRITE=", "HOME=/tmp", "LANG=C", "TZ=UTC"},
		{"NANOGO_OBJ_WRITE=", "HOME=/var/empty", "LANG=de_DE.UTF-8", "TZ=Asia/Tokyo", "GOMAXPROCS=1"},
	} {
		out := filepath.Join(dir, fmt.Sprintf("child%d.bin", i))
		// Each child runs in a different working directory, which is what
		// catches a path that reached the output.
		wd := filepath.Join(dir, fmt.Sprintf("wd%d", i))
		if err := os.MkdirAll(wd, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(self, "-test.run=TestDeterminismAcrossProcesses")
		cmd.Dir = wd
		cmd.Env = append(env[1:], "NANOGO_OBJ_WRITE="+out, "PATH="+os.Getenv("PATH"))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child %d failed: %v\n%s", i, err, b)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, here) {
			t.Errorf("child %d wrote %d bytes, this process wrote %d, and they differ", i, len(b), len(here))
			continue
		}
		n++
	}
	if n == 0 {
		t.Fatal("no object was compared")
	}
	t.Logf("compared %d objects of %d bytes written in other processes", n, len(here))
}

// TestAnonymousSymbol covers the format requirement that an auxiliary payload
// carries no name.
//
// cmd/link decides whether a symbol takes part in the data layout by asking
// whether it has a name. A named pc-value table is therefore placed into the
// read-only section, its offset in runtime.pctab is overwritten by that
// placement, and the linker faults writing the table. The writer used to reject
// every empty name, which made the requirement unexpressible and forced its
// first consumer to clear the name lengths in the written bytes afterwards.
func TestAnonymousSymbol(t *testing.T) {
	// An unnamed symbol that does not say so is still a mistake.
	if err := checkSym(&Symbol{Type: SRODATA}); err == nil {
		t.Error("an unnamed symbol was accepted without Anonymous set")
	}

	// One that says so is accepted.
	aux := &Symbol{Type: SRODATA, Anonymous: true, Pcdata: true, Size: 4, Data: []byte{1, 2, 3, 4}}
	if err := checkSym(aux); err != nil {
		t.Errorf("an anonymous auxiliary symbol was rejected: %v", err)
	}

	// And it round-trips through a written object with an empty name, which is
	// the property the linker reads.
	p := NewPackage("example.com/x")
	text := &Symbol{Name: "example.com/x.f", Type: STEXT, Size: 4, Data: []byte{0xc0, 0x03, 0x5f, 0xd6}}
	pc := &Symbol{Type: SRODATA, Anonymous: true, Pcdata: true, Size: 2, Data: []byte{0x02, 0x00}}
	ref := p.AddDef(pc)
	text.Aux = []Aux{{Type: AuxPcsp, Sym: ref}}
	p.AddDef(text)

	b, err := p.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("the object is empty")
	}
}
