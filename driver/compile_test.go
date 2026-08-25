// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// arm64Only guards the tests that reach the backend.
//
// Everything above code generation is architecture independent and runs
// everywhere, which is why only these tests carry the guard. CI sets
// NANOGO_REQUIRE_LINK on the arm64 runner, so a skip there is a failure.
func arm64Only(t *testing.T) {
	t.Helper()
	if runtime.GOARCH == "arm64" {
		return
	}
	if os.Getenv("NANOGO_REQUIRE_LINK") == "1" {
		t.Fatalf("NANOGO_REQUIRE_LINK is set and GOARCH is %s; nanogo emits arm64", runtime.GOARCH)
	}
	t.Skipf("nanogo emits arm64 machine code and GOARCH is %s", runtime.GOARCH)
}

// needGoCommand skips a test that cannot run without the go command. The
// object header nanogo writes is the installed toolchain's, and obj probes for
// it rather than declaring it.
func needGoCommand(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go command: %v", err)
	}
}

// compileSource writes src to a temporary file and compiles it with cfg
// filled in. It returns the output path and the error Compile reported.
func compileSource(t *testing.T, src string, edit func(*Config)) (string, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Package:   "main",
		Output:    filepath.Join(dir, "_pkg_.a"),
		Lang:      "go1.27",
		GoVersion: PinnedGoVersion,
		Files:     []string{path},
	}
	if edit != nil {
		edit(cfg)
	}
	return cfg.Output, Compile(cfg)
}

// TestCompileWritesAnArchive is the unit form of the end to end test: the
// pipeline runs and the bytes that come out are the archive -pack asks for.
func TestCompileWritesAnArchive(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	out, err := compileSource(t, "package main\n\nfunc f(a, b int) int { return a*b + 1 }\n\nfunc main() { f(20, 3) }\n",
		func(c *Config) { c.Pack = true })
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), archiveMagic) {
		t.Fatalf("the output does not start with the archive magic: %q", b[:min(16, len(b))])
	}
	// __.PKGDEF first, because internal/exportdata.FindPackageDefinition
	// reads member zero and does not search for it.
	header := string(b[len(archiveMagic) : len(archiveMagic)+archiveHeader])
	if !strings.HasPrefix(header, definitionMember) {
		t.Errorf("the first member is %q, want %q", header[:16], definitionMember)
	}
	if !strings.HasSuffix(header, "`\n") {
		t.Errorf("the member header does not end with the archive terminator: %q", header)
	}
	if !strings.Contains(string(b), "\n$$B\nu") {
		t.Error("the archive carries no unified export data section")
	}
	if !strings.Contains(string(b), objectMember) {
		t.Errorf("the archive holds no %s member", objectMember)
	}
	// The linker refuses an object whose header line is not its own, so the
	// member has to carry the installed toolchain's.
	if !strings.Contains(string(b), "go object ") {
		t.Error("the member is not a Go object")
	}
	if !strings.Contains(string(b), "\nmain\n") {
		t.Error("the object does not carry the main mark, and cmd/link needs it")
	}
}

// TestCompileWritesABareObject checks the other half of -pack: without it the
// output is the object and nothing around it.
func TestCompileWritesABareObject(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	out, err := compileSource(t, "package p\n\nfunc f(a int) int { return a + 1 }\n",
		func(c *Config) { c.Package = "p" })
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "go object ") {
		t.Errorf("the output is not a bare object: %q", b[:min(16, len(b))])
	}
	if strings.Contains(string(b), "\nmain\n") {
		t.Error("a package that is not main carries the main mark")
	}
}

// TestCompileIsDeterministic is specs/053-determinism.md at the driver's
// level: the same source compiled twice is the same bytes.
func TestCompileIsDeterministic(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	// One of the functions makes a variadic call, so the object carries type
	// descriptors as well as text symbols. The union of the per function lists
	// and the tables addDescriptors resolves relocations through are exactly
	// the shapes specs/053-determinism.md is about, and a run over the same
	// source that ordered them differently would produce different bytes here.
	body := "package p\n\n" +
		"func a(x int) int { return x * 2 }\n\n" +
		"func b(x int) int { return x + 3 }\n\n" +
		"func c(xs ...int) int { return len(xs) }\n\n" +
		"func d(x int) int { return c(x, x) + c(x) }\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The same source file, because the file name reaches the pc-file table
	// and two paths are two different objects for a good reason.
	compile := func(out string) []byte {
		t.Helper()
		if err := Compile(&Config{Package: "p", Output: out, Lang: "go1.27", Files: []string{src}}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if string(compile(filepath.Join(dir, "1.o"))) != string(compile(filepath.Join(dir, "2.o"))) {
		t.Error("two compilations of the same source produced different objects")
	}
}

// TestCompileRefusals covers every input nanogo declines, and checks that the
// message names what stopped it.
//
// The message is the whole point. An allowlist entry is a claim that nanogo
// owns the package, so a refusal has to say which construct to remove or which
// component to build, per specs/051-build-integration.md.
func TestCompileRefusals(t *testing.T) {
	// Compile refuses a host it cannot emit for before it looks at the source,
	// so on any other architecture every one of these cases reports that
	// instead of the refusal it is written to check. The guard is not about
	// code generation here; it is that the message under test is unreachable.
	arm64Only(t)

	tests := []struct {
		name string
		src  string
		edit func(*Config)
		want []string // every substring the message must carry
	}{
		{
			name: "a type error names the position",
			src:  "package main\n\nfunc f() int { return \"x\" }\n",
			want: []string{"a.go:3:23", "cannot use"},
		},
		{
			name: "a syntax error names the position",
			src:  "package main\n\nfunc f() int { return }}\n",
			want: []string{"a.go:3:24"},
		},
		{
			name: "an import with no configuration entry says so",
			src:  "package main\n\nimport \"errors\"\n\nvar _ = errors.New\n",
			want: []string{"errors", "no entry"},
		},
		{
			name: "an import whose archive is not there names the file",
			src:  "package main\n\nimport \"errors\"\n\nvar _ = errors.New\n",
			edit: func(c *Config) {
				c.ImportCfg = mustImportCfg(t, "packagefile errors=/pkg/errors.a\n")
			},
			want: []string{"errors", "/pkg/errors.a", "no such file"},
		},
		{
			name: "an importmap is applied before the lookup",
			src:  "package main\n\nimport \"errors\"\n\nvar _ = errors.New\n",
			edit: func(c *Config) {
				c.ImportCfg = mustImportCfg(t, "importmap errors=vendor/errors\npackagefile vendor/errors=/pkg/v.a\n")
			},
			want: []string{"/pkg/v.a"},
		},
		{
			// The construct this case names has to be one the pipeline still
			// refuses, and the struct literal it used to name is lowered now
			// that ir.Lower runs. A map is the largest row of
			// specs/020-ir.md's lowering table that specs/032 still blocks:
			// a map descriptor's tail names the runtime's own group type.
			//
			// The stage in the message is ir.Lower and not ssa.Lower, and the
			// two are different decks. A refusal that named the wrong one
			// would send a reader to the wrong spec.
			name: "a construct lowering refuses names the function and the pass",
			src:  "package main\n\nfunc f() int {\n\tm := make(map[int]int)\n\treturn len(m)\n}\n",
			want: []string{"function f", "a.go:3:6", "ir.Lower", "make", "specs/032"},
		},
		{
			// The second refusal site specs/032 opens. Lowering refuses a type
			// it cannot name; rtype refuses one whose bytes it cannot fill in,
			// and a defined type is named and not fillable. The message names
			// the type rather than the function, because the gap is the type
			// boundary of specs/020-ir.md and not this function.
			name: "a type with no writable descriptor is refused by name",
			src:  "package main\n\ntype point struct{ x, y int }\n\nfunc f() int {\n\tp := &point{1, 2}\n\treturn p.x\n}\n",
			want: []string{"a type its code needs a descriptor for", "main.point", "method set"},
		},
		{
			// By name and by position. A package whose variables have no
			// data symbol is a package whose initialisation record would
			// list work that cannot be done, and a record that is short of
			// what the source asked for produces a program that runs and is
			// wrong.
			name: "a package-level variable is refused by name and position",
			src:  "package main\n\nvar n = 1\n\nfunc f() int { return n }\n",
			want: []string{"package-level variable main.n", "a.go:3:5", "data symbol"},
		},
		{
			name: "-complete makes a bodyless declaration an error",
			src:  "package main\n\nfunc f(a int) int\n\nfunc g() int { return 1 }\n",
			edit: func(c *Config) { c.Complete = true },
			want: []string{"a.go:3:6", "missing function body"},
		},
		{
			name: "the runtime is refused",
			src:  "package main\n\nfunc f() int { return 1 }\n",
			edit: func(c *Config) { c.CompilingRuntime = true },
			want: []string{"the runtime", "specs/034"},
		},
		{
			name: "go:embed is refused",
			src:  "package main\n\nfunc f() int { return 1 }\n",
			edit: func(c *Config) { c.EmbedCfgFile = "/tmp/embedcfg" },
			want: []string{"go:embed", "-embedcfg"},
		},
		{
			name: "no output file",
			src:  "package main\n\nfunc f() int { return 1 }\n",
			edit: func(c *Config) { c.Output = "" },
			want: []string{"no -o output file"},
		},
		{
			name: "no source files",
			src:  "package main\n\nfunc f() int { return 1 }\n",
			edit: func(c *Config) { c.Files = nil },
			want: []string{"no source files"},
		},
		{
			name: "a source file that is not there",
			src:  "package main\n\nfunc f() int { return 1 }\n",
			edit: func(c *Config) { c.Files = []string{"nosuch.go"} },
			want: []string{"nosuch.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSource(t, tt.src, tt.edit)
			if err == nil {
				t.Fatal("Compile succeeded, want a refusal")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the message does not carry %q:\n%v", want, err)
				}
			}
		})
	}
}

// TestCompileLowersWhatConstructionRefuses is the first change of specs/032's
// seam, stated as a program rather than as a pass list.
//
// ssa.Build refuses every node Op.IsGoSpecific reports, so until ir.Lower
// joined the list a composite literal was refused by the compiler a user runs
// even though the pass that lowers it was built and measured. This is the
// exact source TestCompileRefusals used to check the refusal with.
func TestCompileLowersWhatConstructionRefuses(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	src := "package main\n\ntype p struct{ x int }\n\nfunc f(a int) int {\n\treturn p{a}.x\n}\n\nfunc main() { f(1) }\n"
	if _, err := compileSource(t, src, nil); err != nil {
		t.Fatalf("a struct literal is still refused: %v", err)
	}
}

// TestCompileEmitsTypeDescriptors is the second change, and it is the one that
// keeps the first from being a link-time failure.
//
// A variadic call is the largest row that needs a descriptor: the builder packs
// the arguments into a slice literal, the literal allocates, and
// runtime.newarray takes the element type. So the object has to carry type:int,
// the pointer bitmask it points at, and the func value of the equality routine.
// Without the descriptors the object still writes and the link reports type:int
// as undefined, which is why the names are read out of the object here.
func TestCompileEmitsTypeDescriptors(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	src := "package main\n\nfunc count(xs ...int) int { return len(xs) }\n\nfunc main() { count(1, 2, 3) }\n"
	out, err := compileSource(t, src, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// The names live in the object's string table, so a search over the bytes
	// says the symbol reached the writer under the name specs/032 requires.
	for _, want := range []string{
		"type:int",                  // the descriptor, under its canonical name
		"runtime.gcbits.",           // the bitmask GCData points at
		"runtime.memequal64\u00b7f", // the func value Equal holds
		"runtime.newarray",          // the call that reads the descriptor
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the object does not carry %q", want)
		}
	}
}

// TestAddDescriptorsEmitsEachSymbolOnce checks the union, without a compile.
//
// A descriptor is named once however many functions reach it, and two
// descriptors share one pointer bitmask whenever their pointer maps agree. The
// check is that a list with repetitions writes the same object as the list
// without them: a second definition of one name would move every later symbol
// index and change the bytes.
func TestAddDescriptorsEmitsEachSymbolOnce(t *testing.T) {
	elem := &ir.Type{Kind: ir.Int64, Name: "int"}
	if err := ir.Layout(elem); err != nil {
		t.Fatal(err)
	}
	slice := &ir.Type{Kind: ir.Slice, Elem: elem}
	if err := ir.Layout(slice); err != nil {
		t.Fatal(err)
	}
	write := func(types []*ir.Type) []byte {
		t.Helper()
		p := obj.NewPackage("p")
		if err := addDescriptors(&Config{Package: "p"}, p, types); err != nil {
			t.Fatalf("addDescriptors: %v", err)
		}
		b, err := p.Bytes()
		if err != nil {
			t.Fatalf("the object does not write: %v", err)
		}
		return b
	}
	once := write([]*ir.Type{elem, slice})
	twice := write([]*ir.Type{elem, slice, elem, slice})
	if string(once) != string(twice) {
		t.Error("naming a type twice emitted its descriptor twice")
	}
	if !strings.Contains(string(once), "type:[]int") {
		t.Error("the object does not carry type:[]int")
	}
}

// TestAddDescriptorsRefusesATypeWithNoMethodSet covers the refusal that arrives
// after the function it came from compiled.
//
// Naming and filling in are two questions and specs/032 keeps them apart. The
// lowering pass names a defined type without trouble, so the refusal can only
// come from here, and it has to say which type stopped the compile.
func TestAddDescriptorsRefusesATypeWithNoMethodSet(t *testing.T) {
	defined := &ir.Type{Kind: ir.Struct, Name: "p.T", Fields: []ir.Field{{Name: "x", Type: &ir.Type{Kind: ir.Int64, Name: "int"}}}}
	if err := ir.Layout(defined); err != nil {
		t.Fatal(err)
	}
	err := addDescriptors(&Config{Package: "p"}, obj.NewPackage("p"), []*ir.Type{defined})
	if err == nil {
		t.Fatal("addDescriptors accepted a type whose method set is unknown")
	}
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("the failure is not an UnsupportedError: %v", err)
	}
	for _, want := range []string{"p.T", "method set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not carry %q:\n%v", want, err)
		}
	}
}

// TestTargetABIFollowsTheSymbol checks the half of a symbol's identity that is
// not its name.
//
// cmd/link resolves a by-name reference by name and ABI together, so a data
// symbol referenced under ABIInternal names a symbol nothing defines. Every
// symbol a descriptor points at is data except the equality routine.
func TestTargetABIFollowsTheSymbol(t *testing.T) {
	for _, tt := range []struct {
		name string
		want uint16
	}{
		{"runtime.gcbits.0100000000000000", obj.ABI0},
		{"type:.namedata.int-", obj.ABI0},
		{"type:int", obj.ABI0},
		{"runtime.memequal64", obj.ABIInternal},
		// A symbol that exists only in assembly is ABI0, for the reason
		// ssagen's morestackCallee records.
		{"runtime.morestack_noctxt", obj.ABI0},
	} {
		if got := targetABI(tt.name); got != tt.want {
			t.Errorf("targetABI(%q) = %d, want %d", tt.name, got, tt.want)
		}
	}
}

// TestCompileRefusesAssembly checks both flags the go command sends for a
// package that has .s files in it.
func TestCompileRefusesAssembly(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*Config)
	}{
		{"symabis", func(c *Config) { c.SymABIs = "/work/b001/symabis" }},
		{"asmhdr", func(c *Config) { c.AsmHdr = "/work/b001/go_asm.h" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSource(t, "package main\n\nfunc f(a int) int\n", tt.edit)
			if err == nil {
				t.Fatal("Compile accepted a package with assembly")
			}
			for _, want := range []string{"assembly", "ABI0", "ABIInternal", "specs/030"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the message does not carry %q:\n%v", want, err)
				}
			}
		})
	}
}

// TestCompileRefusesAnUnwritableOutput covers the error the writer returns
// rather than the ones the front end does.
func TestCompileRefusesAnUnwritableOutput(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	_, err := compileSource(t, "package main\n\nfunc f() int { return 1 }\n",
		func(c *Config) { c.Output = filepath.Join(t.TempDir(), "nosuchdir", "o.a") })
	if err == nil {
		t.Fatal("Compile wrote to a directory that does not exist")
	}
}

func TestCompileRejectsNilConfig(t *testing.T) {
	if err := Compile(nil); err == nil {
		t.Fatal("Compile(nil) = nil")
	}
}

func mustImportCfg(t *testing.T, body string) *ImportCfg {
	t.Helper()
	cfg, err := ParseImportCfg("importcfg", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestUnsupportedErrorMessage(t *testing.T) {
	e := &UnsupportedError{Package: "strconv", What: "a thing"}
	if got, want := e.Error(), "strconv: nanogo cannot compile a thing"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	e.Detail = "why"
	if !strings.HasSuffix(e.Error(), ": why") {
		t.Errorf("Error() = %q, want the detail at the end", e)
	}
}

func TestTrimPath(t *testing.T) {
	sep := string(filepath.Separator)
	rewrites := []TrimRewrite{
		{Old: "/src/mod", New: "example.com/m"},
		{Old: "/work/b001", New: ""},
	}
	tests := []struct{ in, want string }{
		{filepath.Join("/src/mod", "a.go"), "example.com/m" + sep + "a.go"},
		{filepath.Join("/work/b001", "importcfg"), "importcfg"},
		{"/src/mod", "example.com/m"},
		{filepath.Join("/elsewhere", "a.go"), filepath.Join("/elsewhere", "a.go")},
		// A prefix that is not a path element must not match, or a rewrite of
		// /src/mod would also rewrite /src/module.
		{filepath.Join("/src/modular", "a.go"), filepath.Join("/src/modular", "a.go")},
	}
	for _, tt := range tests {
		if got := TrimPath(rewrites, tt.in); got != tt.want {
			t.Errorf("TrimPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := TrimPath(nil, "/a/b.go"); got != "/a/b.go" {
		t.Errorf("TrimPath with no rewrites changed %q", got)
	}
}

// TestCompileAppliesTrimPath checks that the rewrite reaches the object,
// because the file name in the pc-file table is what a traceback prints.
func TestCompileAppliesTrimPath(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package p\n\nfunc f(a int) int { return a }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "p.o")
	cfg := &Config{Package: "p", Output: out, Lang: "go1.27", Files: []string{src}}
	if err := setTrimPath(cfg, dir+"=>trimmed"); err != nil {
		t.Fatal(err)
	}
	if err := Compile(cfg); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), dir) {
		t.Error("the object carries the untrimmed source directory")
	}
	if !strings.Contains(string(b), "trimmed/a.go") {
		t.Error("the object does not carry the trimmed file name")
	}
}

func TestPositionFallsBackToTheName(t *testing.T) {
	if got := position(nil, 0, "f"); got != "f" {
		t.Errorf("position with no file set = %q, want %q", got, "f")
	}
	if _, line := fileAndLine(&Config{}, nil, 0); line != 1 {
		t.Errorf("fileAndLine with no file set gave line %d, want 1", line)
	}
}

func TestPragmaHandlerRecordsNothing(t *testing.T) {
	if p := pragmaHandler(0, false, "go:noinline", nil); p != nil {
		t.Errorf("pragmaHandler = %v, want nil", p)
	}
}

// TestCompileEmitsAnInitTaskForADeclaredInit reads the archive with the tool
// that reads objects.
//
// ir.Build puts a declared init in p.Funcs under its own symbol and
// synthesises one function in p.Inits that calls it. Both are compiled, and
// the record lists the synthesised one alone: it is the entry point for
// everything this package initialises, in the order the source fixed.
func TestCompileEmitsAnInitTaskForADeclaredInit(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	out, err := compileSource(t, "package main\n\nfunc init() {}\n\nfunc f() int { return 1 }\n",
		func(c *Config) { c.Pack = true })
	if err != nil {
		t.Fatalf("Compile refused a package with an init function: %v", err)
	}
	syms := symbolsOf(t, out)
	for _, want := range []string{"main..inittask", "main.init", "main.init.0"} {
		if !strings.Contains(syms, want) {
			t.Errorf("the object holds no %s:\n%s", want, syms)
		}
	}
}

// symbolsOf is go tool nm over an archive nanogo wrote. The reader is the
// toolchain's, so a symbol it does not report is a symbol cmd/link would not
// see either.
func symbolsOf(t *testing.T, archive string) string {
	t.Helper()
	out, err := exec.Command("go", "tool", "nm", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm %s: %v\n%s", archive, err, out)
	}
	return string(out)
}

// TestCompileReportsAnEmitFailure covers the last stage of the pipeline.
//
// A string constant needs a data symbol, and specs/032 has no writer, so the
// emitter has no address to relocate against. The message has to name the
// function, because the allowlist entry that produced it names only a package.
func TestCompileReportsAnEmitFailure(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	_, err := compileSource(t, "package main\n\nfunc f() string { return \"x\" }\n", nil)
	if err == nil {
		t.Fatal("Compile emitted a function that returns a string constant")
	}
	for _, want := range []string{"function f", "a.go:3:6", "ssagen.Emit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not carry %q:\n%v", want, err)
		}
	}
}

// TestCompileCapsTheErrorReport checks the bound on how many errors one
// package reports. The checker keeps going after a soft error, and a hundred
// follow-on messages hide the first one, which is the one to act on.
func TestCompileCapsTheErrorReport(t *testing.T) {
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < maxReportedErrors*3; i++ {
		b.WriteString("func f() int { return }}\n")
	}
	_, err := compileSource(t, b.String(), nil)
	if err == nil {
		t.Fatal("Compile accepted a file of syntax errors")
	}
	if n := strings.Count(err.Error(), "a.go:"); n > maxReportedErrors {
		t.Errorf("the report carries %d positions, and the cap is %d", n, maxReportedErrors)
	}
}

// TestPositionOfAnUnknownOffset covers the fallback for a position the file
// set cannot resolve. A node with no position must still produce a message
// that names something.
func TestPositionOfAnUnknownOffset(t *testing.T) {
	fset := syntax.NewFileSet()
	fset.AddFile("a.go", 4)
	if got := position(fset, syntax.Pos(1<<30), "f"); got != "f" {
		t.Errorf("position of an offset outside the set = %q, want %q", got, "f")
	}
}

// TestFileAndLineOfAnUnknownOffset covers the line the object records when
// the position cannot be resolved. Zero is not a line number, and the runtime
// reads this table to print a traceback.
func TestFileAndLineOfAnUnknownOffset(t *testing.T) {
	fset := syntax.NewFileSet()
	fset.AddFile("a.go", 4)
	if _, line := fileAndLine(&Config{}, fset, syntax.Pos(1<<30)); line != 1 {
		t.Errorf("line = %d, want 1", line)
	}
}

// TestCompileRefusesAGenericFunction covers the report ir.Build makes.
//
// specs/013-generics.md stencils, and stenciling needs the body of the
// instantiated function, which is the same export data specs/015 has no reader
// for. The builder says so and the driver passes it on with the package named.
func TestCompileRefusesAGenericFunction(t *testing.T) {
	_, err := compileSource(t, "package main\n\nfunc f[T any](x T) T { return x }\n\nfunc g() int { return f(1) }\n", nil)
	if err == nil {
		t.Fatal("Compile accepted a generic function")
	}
	for _, want := range []string{"main", "generic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not carry %q:\n%v", want, err)
		}
	}
}

// TestTheTargetDecidesTheArchCheck covers the refusal a build for an
// architecture nanogo has no backend for gets.
//
// The check is a function of the architecture rather than of runtime, so the
// arm64 runner checks the message the amd64 one sees.
//
// The architecture it is a function of is the **target's**, and that is the
// regression. The check used to read runtime.GOARCH, which is the
// architecture of the nanogo binary. On an arm64 host, GOARCH=amd64 therefore
// passed it, nanogo emitted arm64 machine code into an object declared to be
// amd64, and the first thing to notice was go tool link:
//
//	unknown reloc to os.Exit: 9 (R_CALLARM64)
//
// That message names neither the cause nor the fix. The old test could not see
// this, because it called the check directly and so never asked where the
// architecture came from.
func TestTheTargetDecidesTheArchCheck(t *testing.T) {
	if err := checkTarget("strconv", TargetArch); err != nil {
		t.Errorf("the target nanogo emits for was refused: %v", err)
	}
	err := checkTarget("strconv", "amd64")
	if err == nil {
		t.Fatal("a build for amd64 was accepted; nanogo has no amd64 backend")
	}
	for _, want := range []string{"strconv", "amd64", TargetArch, "specs/043"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not carry %q:\n%v", want, err)
		}
	}

	// Where the architecture comes from. An unset GOARCH is the host, which is
	// what a build that names no target means. A Config that names one answers
	// to it instead, whatever the host is.
	if got := (&Config{}).TargetArch(); got != runtime.GOARCH {
		t.Errorf("an unset GOARCH gave %q, want the host %q", got, runtime.GOARCH)
	}
	if got := (&Config{GOARCH: "amd64"}).TargetArch(); got != "amd64" {
		t.Errorf("TargetArch gave %q, want the target the build named", got)
	}
}

// TestWriteOutputReportsAToolchainFailure covers the two failures the writer
// can report after the whole pipeline has succeeded.
//
// nanogo writes the object header the installed toolchain writes, because
// cmd/link compares it and refuses an object whose line differs by one
// character. So a toolchain nanogo cannot probe stops the compile, and it stops
// it with the package named.
func TestWriteOutputReportsAToolchainFailure(t *testing.T) {
	saved := verifyToolchain
	defer func() { verifyToolchain = saved }()

	verifyToolchain = func() (*obj.Toolchain, error) { return nil, errors.New("no go command") }
	err := writeOutput(&Config{Package: "strconv", Output: filepath.Join(t.TempDir(), "o.a")},
		obj.NewPackage("strconv"), types2.NewPackage("strconv", "strconv"))
	if err == nil || !strings.Contains(err.Error(), "strconv") || !strings.Contains(err.Error(), "no go command") {
		t.Errorf("writeOutput = %v, want the probe failure with the package named", err)
	}

	// A header the object writer refuses reaches the same call, after the file
	// exists. The file has to be closed even so, or a build that compiles many
	// packages runs out of descriptors before it runs out of packages.
	verifyToolchain = func() (*obj.Toolchain, error) { return &obj.Toolchain{Header: "not a header\n"}, nil }
	out := filepath.Join(t.TempDir(), "o.a")
	err = writeOutput(&Config{Package: "strconv", Output: out, Pack: true},
		obj.NewPackage("strconv"), types2.NewPackage("strconv", "strconv"))
	if err == nil || !strings.Contains(err.Error(), "strconv") {
		t.Errorf("writeOutput = %v, want the writer's refusal with the package named", err)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Errorf("the output file was not created: %v", statErr)
	}
}

// TestNosplitIsStillDropped records a known correctness gap so that closing it
// is deliberate rather than accidental.
//
// specs/016-directives-and-pragmas.md classes //go:nosplit as required for
// correctness: a function marked with it must get no stack-growth check,
// because it runs where the call that check makes is not allowed. nanogo drops
// every //go: directive at the parser, so a marked function today compiles
// with the check in it.
//
// Nothing reachable is miscompiled, because the runtime is the only consumer
// of that group and nanogo does not compile the runtime. This test fails the
// moment the handler starts recording, which is the point: it turns closing
// the gap into a change somebody has to look at.
func TestNosplitIsStillDropped(t *testing.T) {
	for _, directive := range []string{
		"go:nosplit",
		"go:nowritebarrier",
		"go:systemstack",
		"go:noescape",
	} {
		if p := pragmaHandler(0, false, directive, nil); p != nil {
			t.Errorf("%s now reaches the compiler.\n"+
				"Remove this test, and make ssa and ssagen honour it: "+
				"specs/016-directives-and-pragmas.md says which of them are "+
				"required for correctness rather than for speed.", directive)
		}
	}
}
