// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
	"golang.design/x/nanogo/ssagen"
)

// argMapCorpus is the package the argument maps are compared over.
//
// Every declaration is bodyless and every one is defined as ABI0 by the
// symabis file beside it, which is the only shape gc writes an args_stackmap
// for. The signatures are specs/047-abi-wrappers.md's six measured rows plus
// the cases the map itself turns on: a pointer in the arguments, a pointer
// only in the results, none at all, and the zero-size parameter that moves
// every value after it.
const argMapCorpus = `package p

import "unsafe"

func g0() uint64

func g1(a int8) (r int8)

func g2(a int8, b int8) (r1 int8, r2 int32)

func g3(name []byte) bool

func g4(a ...int) int

func g5(a int8, b int64, c string, d float64) (r1 int32, r2 [3]int64)

func g6(p *int, q string) *byte

func g7(a int) *int

func g8(a [3]int8, b [0]int64, c [3]int8) int8

func g9(a struct {
	P *int
	N int
}, b [2]*byte) (r string, s *int32)

func g10(a complex128, b float32) bool

func g11(a, b, c, d, e, f, g, h, i, j, k, l int) int

func g12(a map[int]int, b chan int, c func()) unsafe.Pointer
`

// argMapSymABIs defines every declaration of [argMapCorpus] as ABI0.
func argMapSymABIs() string {
	var b strings.Builder
	for i := 0; i <= 12; i++ {
		fmt.Fprintf(&b, "def p.g%d ABI0\n", i)
	}
	return b.String()
}

// TestArgMapsMatchGc compares <sym>.args_stackmap and <sym>.arginfo0 against
// the bytes gc writes for the same declarations.
//
// This is the whole gate on the argument map, and a byte comparison is the
// only test worth having. The map is what the garbage collector reads when it
// scans an assembly function's frame: a bit set over a word that is not a
// pointer makes it follow whatever is there, and a bit clear over a live
// pointer makes it free an object something still holds. Neither shows up
// anywhere near the function that caused it, so a test that checked the map
// parses would pass over both.
//
// The oracle is gc's own -S output, which prints a read-only symbol's bytes in
// hex. It is the same mechanism TestAsmHdrMatchesGc uses and it needs the
// assembler's first pass for the same reason: without -symabis gc has no def
// line to set fn.ABI from and writes no map at all.
func TestArgMapsMatchGc(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "p.go")
	if err := os.WriteFile(src, []byte(argMapCorpus), 0o600); err != nil {
		t.Fatal(err)
	}
	abis := filepath.Join(dir, "symabis")
	if err := os.WriteFile(abis, []byte(argMapSymABIs()), 0o600); err != nil {
		t.Fatal(err)
	}
	want := gcRodata(t, dir, "p", src, abis)
	got := nanogoArgMaps(t, "p", src)
	if len(got) == 0 {
		t.Fatal("nanogo wrote no argument map at all")
	}
	for _, name := range sortedKeys(got) {
		w, ok := want[name]
		if !ok {
			t.Errorf("gc wrote no %s", name)
			continue
		}
		if !equalBytes(w, got[name]) {
			t.Errorf("%s\n gc:     % x\n nanogo: % x", name, w, got[name])
		}
	}
	// The other direction: a symbol gc wrote and nanogo did not is a
	// declaration that owes a map and got none, which is an undefined
	// reference at link time.
	for _, name := range sortedKeys(want) {
		if _, ok := got[name]; !ok {
			t.Errorf("nanogo wrote no %s, which gc wrote as % x", name, want[name])
		}
	}
}

// nanogoArgMaps runs nanogo's front end over the corpus and returns the bytes
// [ssagen.ArgMaps] writes for every bodyless declaration.
//
// It goes through the front end rather than through [Compile] for the reason
// nanogoAsmHdr does: what the writer needs is the declarations, and reading
// them out of the object would test the object writer as well.
func nanogoArgMaps(t *testing.T, path, src string) map[string][]byte {
	t.Helper()
	cfg := &Config{Package: path, Lang: "go1.27", Files: []string{src}, GOARCH: TargetArch}
	files, fset, _, err := parseFiles(cfg)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	pkg, info, err := checkFiles(cfg, newImporter(cfg), files, fset)
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	p, err := ir.Build(pkg, files, info)
	if err != nil {
		t.Fatalf("ir.Build: %v", err)
	}
	out := make(map[string][]byte)
	for _, fn := range p.Funcs {
		if !fn.Bodyless {
			continue
		}
		m, a, err := ssagen.ArgMaps(fn, fn.Sym, ssa.NewArm64Target())
		if err != nil {
			t.Fatalf("ssagen.ArgMaps(%s): %v", fn.Sym, err)
		}
		out[m.Name] = m.Data
		out[a.Name] = a.Data
	}
	return out
}

// gcRodata is the oracle: every .args_stackmap and .arginfo0 symbol in gc's
// own -S output, decoded from the hex it prints.
func gcRodata(t *testing.T, dir, path, src, symabis string) map[string][]byte {
	t.Helper()
	cmd := exec.Command("go", "tool", "compile",
		"-o", filepath.Join(dir, "p.o"),
		"-p", path,
		"-lang=go1.27",
		"-symabis", symabis,
		"-S", src)
	cmd.Env = append(os.Environ(),
		"GOOS=darwin", "GOARCH="+TargetArch, "CGO_ENABLED=0",
		"GOTMPDIR="+t.TempDir(), "TMPDIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool compile -S: %v\n%s", err, out)
	}
	return parseRodata(t, string(out))
}

// parseRodata decodes the read-only symbols of a -S listing.
//
// A symbol is a header line naming it and its size, then one line per sixteen
// bytes: a hex offset, the bytes, and an ASCII column. The size is read off
// the header and exactly that many bytes are taken, because the ASCII column
// holds text that a field walk would read as another byte.
func parseRodata(t *testing.T, listing string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	var cur string
	var want int
	for _, line := range strings.Split(listing, "\n") {
		if !strings.HasPrefix(line, "\t0x") {
			cur, want = "", 0
			f := strings.Fields(line)
			if len(f) < 3 || f[1] != "SRODATA" {
				continue
			}
			if !strings.HasSuffix(f[0], ".args_stackmap") && !strings.HasSuffix(f[0], ".arginfo0") {
				continue
			}
			for _, g := range f[2:] {
				if n, ok := strings.CutPrefix(g, "size="); ok {
					v, err := strconv.Atoi(n)
					if err != nil {
						t.Fatalf("unreadable size in %q", line)
					}
					cur, want = f[0], v
				}
			}
			continue
		}
		if cur == "" {
			continue
		}
		f := strings.Fields(line)
		// At most sixteen bytes to a line, and the ASCII column after them
		// holds text a field walk would read as another byte.
		if len(f) > 17 {
			f = f[:17]
		}
		for _, g := range f[1:] {
			if len(out[cur]) >= want {
				break
			}
			v, err := strconv.ParseUint(g, 16, 8)
			if err != nil {
				t.Fatalf("unreadable byte %q in %q", g, line)
			}
			out[cur] = append(out[cur], byte(v))
		}
	}
	return out
}

// sortedKeys is the walk order of every comparison here. A map walk would
// name a different symbol between two runs over one input
// (specs/053-determinism.md).
func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func equalBytes(a, b []byte) bool { return bytes.Equal(a, b) }

// TestArgMapsMatchGcForTheStandardLibrary is the same comparison over the two
// packages the bootstrap closure actually holds.
//
// internal/cpu and internal/runtime/sys are the two packages whose assembly
// defines a symbol under ABI0 that a Go declaration matches, so they are the
// two whose maps a real build reads. The synthetic corpus above covers the
// shapes; this covers the inputs.
func TestArgMapsMatchGcForTheStandardLibrary(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	goroot := stdSourceRoot(t)
	work := t.TempDir()
	for _, path := range []string{"internal/cpu", "internal/runtime/sys"} {
		t.Run(path, func(t *testing.T) {
			pkg := loadStdPackage(t, goroot, work, path)
			if pkg.SymABIs == "" {
				t.Fatalf("%s has no assembly", path)
			}
			want := gcStdRodata(t, work, pkg)
			got, owed := nanogoStdArgMaps(t, pkg)
			if len(owed) == 0 {
				t.Fatalf("%s has no ABI0 definition, so this test proves nothing", path)
			}
			for _, name := range sortedKeys(got) {
				if !equalBytes(want[name], got[name]) {
					t.Errorf("%s\n gc:     % x\n nanogo: % x", name, want[name], got[name])
				}
			}
			// The other direction, over the declarations this stage owns. gc
			// also writes an arginfo for the ABI0 wrapper of a Go-bodied
			// function, which is stage 3's and is not compared here.
			for _, sym := range owed {
				for _, suffix := range []string{".args_stackmap", ".arginfo0"} {
					if _, ok := got[sym+suffix]; !ok {
						t.Errorf("nanogo wrote no %s%s", sym, suffix)
					}
				}
			}
		})
	}
}

// gcStdRodata compiles a standard library package with gc and returns its
// argument maps, on the command line the go command actually sends.
func gcStdRodata(t *testing.T, work string, p stdPackage) map[string][]byte {
	t.Helper()
	name := strings.ReplaceAll(p.Path, "/", "_")
	args := []string{"tool", "compile",
		"-o", filepath.Join(work, name+".maps.o"),
		"-p", p.Path,
		"-lang=go1.27",
		"-std", "-shared", "-nolocalimports",
		"-importcfg", p.ImportCfg,
		"-symabis", p.SymABIs,
		"-S",
	}
	cmd := exec.Command("go", append(args, p.Files...)...)
	cmd.Env = append(os.Environ(),
		"GOOS=darwin", "GOARCH="+TargetArch, "CGO_ENABLED=0",
		"GOTMPDIR="+t.TempDir(), "TMPDIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool compile -S %s: %v\n%s", p.Path, err, out)
	}
	return parseRodata(t, string(out))
}

// nanogoStdArgMaps runs nanogo's front end over a standard library package
// and returns the maps its ABI0 declarations owe.
func nanogoStdArgMaps(t *testing.T, p stdPackage) (map[string][]byte, []string) {
	t.Helper()
	cfg := &Config{Package: p.Path, Lang: "go1.27", Std: true, Files: p.Files, GOARCH: TargetArch}
	var err error
	if cfg.ImportCfg, err = ReadImportCfg(p.ImportCfg); err != nil {
		t.Fatal(err)
	}
	abis, err := ReadSymABIs(p.SymABIs)
	if err != nil {
		t.Fatal(err)
	}
	files, fset, seen, err := parseFiles(cfg)
	if err != nil {
		t.Fatalf("parsing %s: %v", p.Path, err)
	}
	pkg, info, err := checkFiles(cfg, newImporter(cfg), files, fset)
	if err != nil {
		t.Fatalf("checking %s: %v", p.Path, err)
	}
	prog, err := ir.Build(pkg, files, info)
	if err != nil {
		t.Fatalf("ir.Build %s: %v", p.Path, err)
	}
	links, err := ParseLinknames(cfg.Package, seen)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte)
	var owed []string
	for _, fn := range prog.Funcs {
		if !fn.Bodyless {
			continue
		}
		sym := links.SymOf(fn)
		if abi, ok := abis.Def(sym); !ok || abi != 0 {
			continue
		}
		m, a, err := ssagen.ArgMaps(fn, sym, ssa.NewArm64Target())
		if err != nil {
			t.Fatalf("ssagen.ArgMaps(%s): %v", sym, err)
		}
		out[m.Name] = m.Data
		out[a.Name] = a.Data
		owed = append(owed, sym)
	}
	return out, owed
}

// TestArgMapMarksBothWordsOfAnInterface records a divergence from gc that
// stage 2 found and did not create.
//
// gc's typebits.Set clears the first word of an interface with the comment
// "The first word of an interface is a pointer, but we don't treat it as
// such": it is an itab in persistentalloc space, or a _type in the read-only
// section, or a reflect-allocated _type that reflect itself keeps alive.
// nanogo's ir.Type.PtrBits marks both words, and it marks them everywhere:
// the locals and arguments bitmaps of
// specs/027-liveness-and-stackmaps.md read the same field, and so does the
// GCData of a type descriptor. The divergence is one bit per interface and it
// predates this spec.
//
// It is conservative and not wrong. A bit set over a word that holds a
// pointer to memory outside the heap is ignored, because runtime.findObject
// returns nothing for such an address, and a bit set over a reflect-allocated
// _type keeps alive an object reflect is already holding. The failure a
// stack map can produce is the other direction: a bit clear over a live
// pointer.
//
// The map is derived from ir.Type.PtrBits and not from a second walk beside
// it, which is what keeps nanogo's one statement of the rule one statement.
// Closing the divergence means changing that field, which changes every
// descriptor and every stack map nanogo writes, and belongs with
// specs/027-liveness-and-stackmaps.md rather than here.
func TestArgMapMarksBothWordsOfAnInterface(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "q.go")
	const corpus = "package q\n\nfunc h(a complex128, b interface{}) bool\n"
	if err := os.WriteFile(src, []byte(corpus), 0o600); err != nil {
		t.Fatal(err)
	}
	abis := filepath.Join(dir, "symabis")
	if err := os.WriteFile(abis, []byte("def q.h ABI0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := gcRodata(t, dir, "q", src, abis)["q.h.args_stackmap"]
	got := nanogoArgMaps(t, "q", src)["q.h.args_stackmap"]
	// complex128 at 0 and 8, the interface at 16 and 24, the result at 32:
	// five words. gc marks word 3, nanogo marks words 2 and 3.
	if len(want) != 10 || len(got) != 10 {
		t.Fatalf("the maps are %d and %d bytes, want 10 each:\n gc:     % x\n nanogo: % x", len(want), len(got), want, got)
	}
	if !equalBytes(want[:8], got[:8]) {
		t.Errorf("the headers differ:\n gc:     % x\n nanogo: % x", want[:8], got[:8])
	}
	for i := 8; i < 10; i++ {
		if want[i]&^got[i] != 0 {
			t.Errorf("byte %d: gc marks a word nanogo does not: gc %02x, nanogo %02x", i, want[i], got[i])
		}
		if got[i]&^want[i] != 1<<2 {
			t.Errorf("byte %d: nanogo marks %02x and gc marks %02x, and the only difference must be the interface's first word", i, got[i], want[i])
		}
	}
}

// TestABIWrapperCarriesTheFlagsGcSets reads the wrapper's own symbol.
//
// gc sets four things on an ABI wrapper: DUPOK, ABIWRAPPER, NOSPLIT, and the
// linkname attribute of the declaration it wraps. Three of the four are here.
// NOSPLIT is deliberately not, because
// specs/035-goroutines-and-stack-growth.md forbids claiming a property nanogo
// does not compute.
//
// The linkname attribute is the one that is not cosmetic. cmd/link's loader
// checks a reference to another package's assembly symbol by looking the other
// ABI's symbol up by name, which is this wrapper, and reading the attribute
// off it: "For an assembly symbol, check if there is a linkname applied to its
// ABI wrapper." A wrapper that lost it would turn a legitimate pull into a
// link error naming neither the directive nor the function. gc prints
// LINKNAMESTD alone on internal/runtime/atomic.Xadd's wrapper and LINKNAME
// alone on internal/cpu.sysctlEnabled's, so the two bits are exclusive.
func TestABIWrapperCarriesTheFlagsGcSets(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	for _, tt := range []struct {
		name  string
		src   string
		flag2 uint8
	}{
		{
			name:  "no directive",
			src:   "package main\n\nfunc f(a int) int\n\nfunc g() int { return f(1) }\n",
			flag2: obj.SymFlagABIWrapper,
		},
		{
			// internal/cpu's shape.
			name:  "//go:linkname",
			src:   "package main\n\n//go:linkname f\nfunc f(a int) int\n\nfunc g() int { return f(1) }\n",
			flag2: obj.SymFlagABIWrapper | obj.SymFlagLinkname,
		},
		{
			// internal/runtime/atomic's shape, all forty-nine of them.
			name:  "//go:linknamestd",
			src:   "package main\n\n//go:linknamestd f\nfunc f(a int) int\n\nfunc g() int { return f(1) }\n",
			flag2: obj.SymFlagABIWrapper | obj.SymFlagLinknameStd,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sym := abiWrapperSymbol(t, tt.src, "def main.f ABI0\n", "main.f")
			if sym.ABI != obj.ABIInternal {
				t.Errorf("the wrapper is defined at ABI %d, want ABIInternal", sym.ABI)
			}
			if sym.Flag&obj.SymFlagDupok == 0 {
				t.Errorf("the wrapper is not DUPOK, and gc marks it so")
			}
			if sym.Flag&obj.SymFlagNoSplit != 0 {
				t.Errorf("the wrapper claims NOSPLIT, which specs/035-goroutines-and-stack-growth.md forbids")
			}
			if sym.Flag2 != tt.flag2 {
				t.Errorf("the wrapper's second flag byte is %#02x, want %#02x", sym.Flag2, tt.flag2)
			}
		})
	}
}

// abiWrapperSymbol runs the decision and the wrapper emitter over one source
// and returns the text symbol the wrapper defined.
func abiWrapperSymbol(t *testing.T, src, symabis, name string) *obj.Symbol {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	abis := filepath.Join(dir, "symabis")
	if err := os.WriteFile(abis, []byte(symabis), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Package: "main", Lang: "go1.27", GOARCH: TargetArch,
		Files: []string{path}, SymABIs: abis, AsmHdr: filepath.Join(dir, "go_asm.h"),
	}
	files, fset, seen, err := parseFiles(cfg)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	pkg, info, err := checkFiles(cfg, newImporter(cfg), files, fset)
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	prog, err := ir.Build(pkg, files, info)
	if err != nil {
		t.Fatalf("ir.Build: %v", err)
	}
	s, err := ReadSymABIs(abis)
	if err != nil {
		t.Fatal(err)
	}
	set, err := checkABIWrappers(cfg, s, seen, prog, fset)
	if err != nil {
		t.Fatalf("checkABIWrappers: %v", err)
	}
	if len(set.Wrappers) != 1 {
		t.Fatalf("the decision owes %d wrappers, want 1", len(set.Wrappers))
	}
	out := obj.NewPackage("main")
	if err := addABIWrappers(cfg, out, ssa.NewArm64Target(), fset, set); err != nil {
		t.Fatalf("addABIWrappers: %v", err)
	}
	// Both definition spaces. A duplicate-tolerant text symbol is a
	// non-package definition, which is gc's rule in obj.isNonPkgSym, so the
	// wrapper is not where a walk of this package's own definitions would
	// find it.
	for _, space := range []uint32{obj.PkgIdxSelf, obj.PkgIdxNone} {
		for i := uint32(0); ; i++ {
			d := out.Def(obj.SymRef{PkgIdx: space, SymIdx: i})
			if d == nil {
				break
			}
			if d.Name == name && d.Type == obj.STEXT {
				return d
			}
		}
	}
	t.Fatalf("the object holds no text symbol named %s", name)
	return nil
}
