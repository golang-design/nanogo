// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtype"
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
			// refuses, and append is lowered now. min over floats is the row
			// that stays: the language propagates a NaN operand to the result
			// and a compare and a select do not, so specs/020-ir.md refuses
			// the float form rather than building a wrong one.
			//
			// The stage in the message is ir.Lower and not ssa.Lower, and the
			// two are different decks. A refusal that named the wrong one
			// would send a reader to the wrong spec.
			name: "a construct lowering refuses names the function and the pass",
			src:  "package main\n\nfunc f(a, b float64) float64 {\n\treturn min(a, b)\n}\n",
			want: []string{"function f", "a.go:3:6", "ir.Lower", "min"},
		},
		{
			// By name and by position. A package whose variables have no
			// data symbol is a package whose initialisation record would
			// list work that cannot be done, and a record that is short of
			// what the source asked for produces a program that runs and is
			// wrong.
			//
			// The collector scans the variable and reads its pointer map
			// through a type descriptor, so a variable whose type has none is
			// refused. The type was a map until the slot group gained a
			// spelling, then an array of two hundred pointers until the
			// on-demand mask landed. It is an interface with an unexported
			// method now, whose name needs the package path the name encoder
			// does not write. A variable of a type with no pointers in it, and
			// one of a type whose descriptor rtype can build, both get their
			// symbol now.
			name: "a package-level variable is refused by name and position",
			src:  "package main\n\nvar m interface{ m1() }\n\nfunc f() int { return 1 }\n",
			want: []string{"package-level variable main.m", "a.go:3:5", "type descriptor"},
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
		if err := addDescriptors(&Config{Package: "p"}, p, types, nil, nil, nil, nil, nil); err != nil {
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

// TestAddDescriptorsWritesAnItabAsANamedDefinition is the index-space half of
// specs/032's itab.
//
// cmd/link collects a go:itab. symbol into runtime.itablinks by its name, and
// it reads no name for a symbol in the hashed index space. So an itab written
// as a hashed definition is a table the runtime never registers, and a program
// that asserts on the pair misses every time. The dupok flag is the other half:
// every package that converts the pair writes the itab, and cmd/link merges
// them by name only when they say they tolerate a duplicate.
func TestAddDescriptorsWritesAnItabAsANamedDefinition(t *testing.T) {
	seven, coder := itabPair(t)
	p := obj.NewPackage("p")
	itabs := []ir.Itab{{Type: seven, Iface: coder}, {Type: seven, Iface: coder}}
	if err := addDescriptors(&Config{Package: "p"}, p, []*ir.Type{coder, seven}, itabs, nil, nil, nil, nil); err != nil {
		t.Fatalf("addDescriptors: %v", err)
	}
	name, err := ir.ItabSymbol(seven, coder)
	if err != nil {
		t.Fatal(err)
	}
	sym := findNonPkgDef(p, name)
	if sym == nil {
		t.Fatalf("the object holds no named definition of %s; it defines %v", name, defNames(p))
	}
	if sym.Flag&obj.SymFlagDupok == 0 {
		t.Error("the itab is not dupok, so two packages that convert the pair are two definitions of one name")
	}
	// Once, however many conversions named it. A second definition would move
	// every later symbol index.
	n := 0
	for _, d := range defNames(p) {
		if d == name {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d definitions of %s, want one", n, name)
	}
	if _, err := p.Bytes(); err != nil {
		t.Fatalf("the object does not write: %v", err)
	}
}

// TestAddDescriptorsResolvesAnItabAgainstItsOwnDescriptors is why the itabs go
// through the same two passes as the descriptors.
//
// An itab points at the interface's descriptor and the concrete type's, and
// this object defines both. A pass with tables of its own would emit a
// non-package reference for a name the object already defines, which is a
// second symbol where the linker needs one.
func TestAddDescriptorsResolvesAnItabAgainstItsOwnDescriptors(t *testing.T) {
	seven, coder := itabPair(t)
	p := obj.NewPackage("p")
	itabs := []ir.Itab{{Type: seven, Iface: coder}}
	if err := addDescriptors(&Config{Package: "p"}, p, []*ir.Type{coder, seven}, itabs, nil, nil, nil, nil); err != nil {
		t.Fatalf("addDescriptors: %v", err)
	}
	name, err := ir.ItabSymbol(seven, coder)
	if err != nil {
		t.Fatal(err)
	}
	sym := findNonPkgDef(p, name)
	if sym == nil {
		t.Fatalf("the object holds no definition of %s", name)
	}
	for _, want := range []string{"type:main.coder", "type:main.seven"} {
		found := false
		for _, r := range sym.Relocs {
			if d := p.Def(r.Sym); d != nil && d.Name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the itab does not point at this object's own definition of %s", want)
		}
	}
}

// itabPair builds a concrete type, an interface it implements, and nothing
// else. The two are written by hand rather than compiled, so that the check
// above is about the object writer and not about the front end.
func itabPair(t *testing.T) (concrete, iface *ir.Type) {
	t.Helper()
	i64 := &ir.Type{Kind: ir.Int64, Name: "int"}
	if err := ir.Layout(i64); err != nil {
		t.Fatal(err)
	}
	sig := &ir.Type{Kind: ir.FuncKind, Params: []*ir.Type{}, Results: []*ir.Type{i64}}
	if err := ir.Layout(sig); err != nil {
		t.Fatal(err)
	}
	iface = &ir.Type{Kind: ir.Interface, Name: "main.coder", PkgPath: "main",
		Methods: []ir.Method{{Name: "code", Sig: sig}}}
	concrete = &ir.Type{Kind: ir.Int64, Name: "main.seven", PkgPath: "main", Basic: "int",
		Methods: []ir.Method{{Name: "code", Sig: sig}}}
	for _, ty := range []*ir.Type{iface, concrete} {
		if err := ir.Layout(ty); err != nil {
			t.Fatal(err)
		}
	}
	return concrete, iface
}

// TestCompileEmitsAnItab is the same claim made through a compile.
//
// The conversion names the itab and nothing else does, so the object owes the
// itab, both descriptors, and the wrapper the itab's one Fun entry names.
// main.seven is not stored directly in an interface, so that entry is the
// pointer-receiver form, which no declaration defines and ssagen generates.
// Without the generated definition the object writes and the link reports the
// wrapper as undefined.
func TestCompileEmitsAnItab(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	src := "package main\n\ntype coder interface{ code() int }\n\n" +
		"type seven int\n\nfunc (s seven) code() int { return int(s) }\n\n" +
		"var sink coder\n\nfunc main() { sink = seven(7) }\n"
	out, err := compileSource(t, src, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"go:itab.main.seven,main.coder", // the itab, under ir.ItabSymbol's name
		"type:main.coder",               // the interface's descriptor, which the itab points at
		"type:main.seven",               // the concrete type's
		"main.(*seven).code",            // the Fun entry, which is a generated wrapper
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the object does not carry %q", want)
		}
	}
}

// TestAddDescriptorsMarksATypeUsedInIface checks that rtype's request reaches
// the object.
//
// cmd/link collects a type's Method array only when the symbol carries the
// flag, so a descriptor written without it has every method pruned and the
// runtime installs runtime.unreachableMethod in each one's place.
func TestAddDescriptorsMarksATypeUsedInIface(t *testing.T) {
	elem := &ir.Type{Kind: ir.Int64, Name: "int"}
	if err := ir.Layout(elem); err != nil {
		t.Fatal(err)
	}
	p := obj.NewPackage("p")
	if err := addDescriptors(&Config{Package: "p"}, p, []*ir.Type{elem}, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("addDescriptors: %v", err)
	}
	sym := findNonPkgDef(p, "type:int")
	if sym == nil {
		t.Fatal("the object holds no definition of type:int")
	}
	if sym.Flag2&obj.SymFlagUsedInIface == 0 {
		t.Error("the descriptor is not marked used in an interface, so cmd/link prunes every method it holds")
	}
	// The data the descriptor points at is not a type, and cmd/link panics on
	// a marker aimed at a symbol that is not one.
	for _, name := range defNames(p) {
		if name == "type:int" {
			continue
		}
		if d := findDef(p, name); d != nil && d.Flag2&obj.SymFlagUsedInIface != 0 {
			t.Errorf("%s is marked used in an interface and it is not a type descriptor", name)
		}
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
	err := addDescriptors(&Config{Package: "p"}, obj.NewPackage("p"), []*ir.Type{defined}, nil, nil, nil, nil, nil)
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
// symbol referenced under ABIInternal names a symbol nothing defines. Most of
// what a descriptor points at is data.
func TestTargetABIFollowsTheSymbol(t *testing.T) {
	for _, tt := range []struct {
		name   string
		goFunc bool
		want   uint16
	}{
		{name: "runtime.gcbits.0100000000000000", want: obj.ABI0},
		{name: "type:.namedata.int-", want: obj.ABI0},
		{name: "type:int", want: obj.ABI0},
		{name: "runtime.memequal64", want: obj.ABIInternal},
		// A symbol that exists only in assembly is ABI0, for the reason
		// ssagen's morestackCallee records.
		{name: "runtime.morestack_noctxt", want: obj.ABI0},
		// A Method's Ifn or Tfn. The name says nothing: a method of a type
		// this package declares is compiled by the front end and is in no
		// generated set, so the encoder is what says it is a function.
		{name: "p.T.String", goFunc: true, want: obj.ABIInternal},
		{name: "p.(*T).String", goFunc: true, want: obj.ABIInternal},
	} {
		r := rtype.Reloc{Target: tt.name, GoFunc: tt.goFunc}
		if got := targetABI(r, nil); got != tt.want {
			t.Errorf("targetABI(%q, GoFunc=%v) = %d, want %d", tt.name, tt.goFunc, got, tt.want)
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

// TestCompileReportsAPassFailure covers what a refusal from any pass says.
//
// The message names the pass, the function and the position. The pass tells a
// reader which spec deck owns the gap, and the function and the position are
// needed because the allowlist entry that let the package through names only
// the package.
//
// This drove a construct the last stage could not emit, and the construct
// rotted twice: first a string constant, which needed a data symbol nothing
// wrote, then a floating-point constant, which specs/042 group 6 could not
// encode. Both gaps are closed, and no construct reaches an ssagen.Emit
// refusal today.
//
// The rot is over because the wrapping is now unsupportedFunc, a function of
// the pass name and nothing else, and compileFunc's loop hands it the name of
// whichever pass failed. The shape of any pass's message is therefore asserted
// once, on the pass whose refusal is hardest to reach, with no construct in
// the way. The second half is the loop and the position, which need a real
// compilation, and it takes whatever construct is refused today.
func TestCompileReportsAPassFailure(t *testing.T) {
	// The shape, on the pass name a construct can no longer reach. The error
	// is the emitter's own kind and what is asserted is what the driver wraps
	// it in. This half needs neither the host nor the go command.
	t.Run("the shape", func(t *testing.T) {
		fset := syntax.NewFileSet()
		src := "package main\n\nfunc f() int { return 0 }\n"
		file := fset.AddFile("a.go", len(src))
		off := 0
		for i, c := range src {
			if c == '\n' {
				file.AddLine(i + 1)
			}
			if strings.HasPrefix(src[i:], "func f") {
				off = i + len("func ")
			}
		}
		fn := &ir.Func{Name: "f", Sym: "main.f", Pos: file.Pos(off)}
		err := unsupportedFunc(&Config{Package: "main"}, fset, fn, "ssagen.Emit",
			errors.New("v3: MOVD does not encode"))
		for _, want := range []string{"function f", "a.go:3:6", "ssagen.Emit", "MOVD does not encode"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message does not carry %q:\n%v", want, err)
			}
		}
	})

	// The same shape out of Compile, which is what proves the loop calls the
	// wrapper and that the position is the function's own and not the file's.
	// A conversion of a slice to a pointer to an array is what ssa.Build
	// refuses today, and when that closes this subtest takes the next
	// construct and the one above does not move. It used to be a conversion of
	// a concrete value to an interface, which now compiles.
	t.Run("out of Compile", func(t *testing.T) {
		arm64Only(t)
		needGoCommand(t)
		_, err := compileSource(t, "package main\n\nfunc f(b []byte) *[9]byte { return (*[9]byte)(b) }\n", nil)
		if err == nil {
			t.Fatal("Compile accepted a conversion from a slice to a pointer to an array")
		}
		for _, want := range []string{"function f", "a.go:3:6", "ssa.Build"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message does not carry %q:\n%v", want, err)
			}
		}
	})
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

// TestCompileStencilsAGenericInstantiation is what this test used to refuse.
//
// The declaration is skipped, because a body with type parameters in it has no
// run-time representation in the package that declares it and gc emits none
// either. The instantiation is compiled, as one monomorphic function per
// distinct type argument list (specs/013-generics.md). So a package that
// declares a generic and calls it at two types compiles, and the two bodies
// are in the archive under the names specs/013 chose.
//
// The message this used to pin was "type parameter has no run-time
// representation", which came from the IR type boundary and said nothing about
// generics. That boundary still holds and it is still the honest failure for a
// type parameter that reaches it. It stopped firing here because the bodies
// are instantiated, which is what specs/013 asked for.
func TestCompileStencilsAGenericInstantiation(t *testing.T) {
	arm64Only(t)
	if _, err := compileSource(t, "package main\n\n"+
		"func f[T any](x T) T { return x }\n\n"+
		"func main() { println(f(1)); println(f(\"s\")) }\n", nil); err != nil {
		t.Fatalf("a generic instantiation was refused: %v", err)
	}
}

// TestCompileSkipsAGenericDeclaration is the other half: a package whose
// generic declaration nothing instantiates compiles, and produces no body for
// it.
func TestCompileSkipsAGenericDeclaration(t *testing.T) {
	arm64Only(t)
	if _, err := compileSource(t, "package main\n\nfunc f[T any](x T) T { return x }\n\nfunc main() {}\n", nil); err != nil {
		t.Fatalf("a generic declaration nothing instantiates was refused: %v", err)
	}
}

// TestCompileRefusesWhatTheStencilerDoesNotBuild names the line
// specs/013-generics.md stops at, from the driver's side, so that a diagnostic
// naming the construct cannot silently become an undefined symbol with no
// source position on it.
//
// The two lines that need an importcfg are pinned in ir where the importer is:
// a generic function another package declared
// (TestStencilRefusesAGenericOfAnotherPackage) and a method of an
// instantiation of another package's generic type
// (TestStencilRefusesAMethodOfAnotherPackagesGenericType).
func TestCompileRefusesWhatTheStencilerDoesNotBuild(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  string
		want string
	}{
		{
			"a method with type parameters of its own",
			"package main\n\ntype H struct{}\n\n" +
				"func (H) Do[T any](x T) T { return x }\n\n" +
				"func main() { println(H{}.Do(1)) }\n",
			"an instantiation of one is not built",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSource(t, tt.src, nil)
			if err == nil {
				t.Fatal("the package was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the message does not carry %q:\n%v", tt.want, err)
			}
		})
	}
}

// TestCompileRefusesADeclarationTheExportWriterCannotEncode checks the channel
// an export refusal arrives on.
//
// export.UnsupportedError names the declaration the writer has no encoding
// for, and the package is on the allowlist, so its export data is owed and the
// build has failed for a construct nanogo cannot compile. Reported as anything
// else it reads as a compiler bug: internal/gotest classifies a message that
// does not say "nanogo cannot compile" as nanogo rejecting legal Go.
func TestCompileRefusesADeclarationTheExportWriterCannotEncode(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	_, err := compileSource(t, "package main\n\ntype Box[T any] struct{ V T }\n\n"+
		"func (b Box[T]) Get() T { return b.V }\n\nfunc main() {}\n", nil)
	if err == nil {
		t.Fatal("the package was accepted")
	}
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("the failure is not an UnsupportedError: %v", err)
	}
	if !strings.Contains(err.Error(), "nanogo cannot compile ") {
		t.Errorf("the message does not name a construct nanogo cannot compile:\n%v", err)
	}
	if !strings.Contains(err.Error(), "main.Get") {
		t.Errorf("the message does not name the declaration:\n%v", err)
	}
}

// TestCompileRefusesALifetimeDirective covers the two directives whose whole
// meaning is object lifetime.
//
// //go:uintptrescapes and //go:uintptrkeepalive say that a uintptr argument
// keeps its referent alive across the call, which is an obligation on the
// caller, and no pass in nanogo discharges it. A function compiled without it
// runs until a collection happens while the callee is reading the object,
// which is the failure specs/016-directives-and-pragmas.md rule 1 exists to
// prevent: a directive in the correctness group that is recorded and dropped.
//
// internal/gotest/testdata/go/test/uintptrescapes3.go is the corpus file that
// showed it, and the refusal is what turns a wrong answer into a diagnostic.
func TestCompileRefusesALifetimeDirective(t *testing.T) {
	arm64Only(t)
	for _, tt := range []struct {
		name string
		src  string
	}{
		{"uintptrescapes", "package main\n\n//go:uintptrescapes\nfunc f(p uintptr) {}\n\nfunc main() { f(0) }\n"},
		{"uintptrkeepalive", "package main\n\n//go:uintptrkeepalive\nfunc f(p uintptr) {}\n\nfunc main() { f(0) }\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSource(t, tt.src, nil)
			if err == nil {
				t.Fatal("Compile accepted a directive it records and does not honour")
			}
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("the failure is not an UnsupportedError: %v", err)
			}
			for _, want := range []string{"//go:" + tt.name, "f", "alive"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the message does not carry %q:\n%v", want, err)
				}
			}
		})
	}
	// A function with no such directive is not refused by it.
	if _, err := compileSource(t, "package main\n\n//go:noinline\nfunc f(p uintptr) {}\n\nfunc main() { f(0) }\n", nil); err != nil {
		t.Errorf("a function carrying an ordinary directive was refused: %v", err)
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
		obj.NewPackage("strconv"), types2.NewPackage("strconv", "strconv"), false, nil)
	if err == nil || !strings.Contains(err.Error(), "strconv") || !strings.Contains(err.Error(), "no go command") {
		t.Errorf("writeOutput = %v, want the probe failure with the package named", err)
	}

	// A header the object writer refuses reaches the same call, after the file
	// exists. The file has to be closed even so, or a build that compiles many
	// packages runs out of descriptors before it runs out of packages.
	verifyToolchain = func() (*obj.Toolchain, error) { return &obj.Toolchain{Header: "not a header\n"}, nil }
	out := filepath.Join(t.TempDir(), "o.a")
	err = writeOutput(&Config{Package: "strconv", Output: out, Pack: true},
		obj.NewPackage("strconv"), types2.NewPackage("strconv", "strconv"), false, nil)
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
// because it runs where the call that check makes is not allowed. The parser
// now records the directive and it reaches ir.Func.Pragma, but no pass reads
// it, so a marked function still compiles with the check in it.
//
// Nothing reachable is miscompiled, because the runtime is the only consumer
// of that group and nanogo does not compile the runtime. The gate is on the
// consumer rather than on the handler: two packages that differ only by the
// directive must produce the same object today, and the day one of them stops
// doing so is the day somebody has to look at this.
func TestNosplitIsStillDropped(t *testing.T) {
	arm64Only(t)
	needGoCommand(t)
	// The two sources differ in one comment and in nothing else, not even in
	// line count, and they are compiled from the same path. Anything else
	// would move the positions the object records and the objects would
	// differ for a reason that is not the directive.
	const body = "package main\n\n%s\nfunc f(x int) int { return x*3 + 1 }\n\nfunc main() { f(7) }\n"
	dir := t.TempDir()
	src := filepath.Join(dir, "a.go")
	compile := func(comment, out string) []byte {
		t.Helper()
		if err := os.WriteFile(src, []byte(fmt.Sprintf(body, comment)), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{
			Package:   "main",
			Output:    filepath.Join(dir, out),
			Lang:      "go1.27",
			GoVersion: PinnedGoVersion,
			Files:     []string{src},
		}
		if err := Compile(cfg); err != nil {
			t.Fatalf("Compile %s: %v", comment, err)
		}
		b, err := os.ReadFile(cfg.Output)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(compile("// nosplit.", "plain.a"), compile("//go:nosplit", "marked.a")) {
		t.Errorf("//go:nosplit now changes the object nanogo writes.\n" +
			"Remove this test, and make specs/035-goroutines-and-stack-growth.md's " +
			"chain-depth computation the thing that decides: " +
			"specs/016-directives-and-pragmas.md says which directives are " +
			"required for correctness rather than for speed.")
	}
}

// TestCompileEmitsTheDescriptorsSsaBuildNames pins that the package owes both
// descriptor sets and not only the lowering table's.
//
// A conversion to an interface names a type word, and no row of ir.Lower's
// table reaches one, so ssa.Build reports that set separately. A package that
// emitted only the first set compiled without complaint and failed in the
// linker with "relocation target type:main.local not defined", which is the
// worst place for it: the compiler had every fact it needed and said nothing.
//
// Two things make this test easy to write so that it passes either way, and
// the first version of it did both.
//
// The type has to be one nothing else in the package owes. int and string are
// defined by the runtime whatever the compiled package does. A composite or
// anything reached through new, make or a literal is owed by ir.Lower, because
// allocating one names its descriptor and descriptorClosure then pulls in the
// element. driver/types.go owes nothing at all here, because it returns early
// for a main package. A type declared inside a function, converted and never
// allocated, is owed by none of them.
//
// And the archive has to be read for a definition rather than for the name.
// Every reference carries its target's name so that nm and objdump can print
// it, so searching the bytes for the symbol finds the reference the conversion
// creates and answers yes in exactly the case that is broken.
func TestCompileEmitsTheDescriptorsSsaBuildNames(t *testing.T) {
	arm64Only(t)
	const src = `package main

func box() any {
	type local int
	var v local = 7
	return v
}

func main() { _ = box() }
`
	out, err := compileSource(t, src, nil)
	if err != nil {
		t.Fatalf("the package did not compile: %v", err)
	}
	cmd := exec.Command("go", "tool", "nm", out)
	nm, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm: %v\n%s", err, nm)
	}
	// local is declared inside a function, so its symbol carries the
	// scope-disambiguation number specs/032 sets out: box holds the package's
	// first such declaration, and gc spells it type:main.local·1 too.
	const sym = " type:main.local·1"
	var defined, referenced bool
	for _, line := range strings.Split(string(nm), "\n") {
		if !strings.HasSuffix(line, sym) {
			continue
		}
		referenced = true
		// nm prints U for a symbol the object refers to and does not
		// define. Any other class is a definition.
		if f := strings.Fields(line); len(f) >= 2 && f[len(f)-2] != "U" {
			defined = true
		}
	}
	if !referenced {
		t.Fatalf("the archive does not name%s at all, so this test is no longer measuring what it says", sym)
	}
	if !defined {
		t.Errorf("the archive refers to%s and does not define it, so a program that converts one to an interface cannot link", sym)
	}
}

// findPkgDef returns a definition of the object's own package index space.
func findPkgDef(p *obj.Package, name string) *obj.Symbol {
	for i := uint32(0); ; i++ {
		s := p.Def(obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: i})
		if s == nil {
			return nil
		}
		if s.Name == name {
			return s
		}
	}
}

// TestAddDescriptorsWritesACacheAsAPackageDefinition is specs/032's rule for
// the two runtime caches, which is the opposite of every other symbol here.
//
// A descriptor and an itab are canonical and duplicate tolerant, so cmd/link
// merges two copies by name in the non-package index space. A cache must not
// be merged: the runtime installs a table into it with a compare-and-swap, so
// one symbol shared by two sites would be two questions writing one table. It
// is therefore a package definition, in a writable section, aligned to a
// pointer, and it carries the linker name of its own Go type, without which
// cmd/link stops with "missing Go type information for global symbol".
func TestAddDescriptorsWritesACacheAsAPackageDefinition(t *testing.T) {
	_, coder := itabPair(t)
	p := obj.NewPackage("main")
	asserts := []ir.TypeAssert{{Sym: "main.f..typeAssert.0", Iface: coder, CanFail: true}}
	switches := []ir.InterfaceSwitch{{Sym: "main.g..interfaceSwitch.0", Cases: []*ir.Type{coder}}}
	if err := addDescriptors(&Config{Package: "main"}, p, []*ir.Type{coder}, nil, nil, asserts, switches, nil); err != nil {
		t.Fatalf("addDescriptors: %v", err)
	}
	for _, tc := range []struct{ name, gotype string }{
		{"main.f..typeAssert.0", "type:internal/abi.TypeAssert"},
		{"main.g..interfaceSwitch.0", "type:internal/abi.InterfaceSwitch"},
	} {
		sym := findPkgDef(p, tc.name)
		if sym == nil {
			t.Errorf("the object holds no package definition of %s; it defines %v", tc.name, defNames(p))
			continue
		}
		if findNonPkgDef(p, tc.name) != nil {
			t.Errorf("%s is also a non-package definition, where cmd/link merges by name", tc.name)
		}
		if sym.Type != obj.SDATA {
			t.Errorf("%s is %v, and the runtime stores into it", tc.name, sym.Type)
		}
		if sym.Align != ir.PtrSize {
			t.Errorf("%s is aligned to %d, and the install is an atomic compare-and-swap on the first word", tc.name, sym.Align)
		}
		if sym.Flag&obj.SymFlagDupok != 0 {
			t.Errorf("%s tolerates a duplicate, and two caches are two tables", tc.name)
		}
		if sym.Flag&obj.SymFlagLocal == 0 {
			t.Errorf("%s is not local, and it belongs to one function of one package", tc.name)
		}
		if len(sym.Aux) != 1 || sym.Aux[0].Type != obj.AuxGotype {
			t.Fatalf("%s carries %v, want one AuxGotype entry", tc.name, sym.Aux)
		}
		// internal/abi defines the descriptor, so the entry is a reference and
		// not one of this object's definitions.
		if sym.Aux[0].Sym.PkgIdx == obj.PkgIdxSelf {
			t.Errorf("the Go type of %s is a definition of this package, and internal/abi owns it", tc.name)
		}
	}
	b, err := p.Bytes()
	if err != nil {
		t.Fatalf("the object does not write: %v", err)
	}
	// The names reach the object's string table through the RefName block,
	// which is what makes cmd/link resolve them by name.
	for _, tc := range []string{"type:internal/abi.TypeAssert", "type:internal/abi.InterfaceSwitch"} {
		if !bytes.Contains(b, []byte(tc)) {
			t.Errorf("the object never names %s, so cmd/link has no Go type to read the pointer map out of", tc)
		}
	}
}

// TestAddDescriptorsRefusesTwoCachesOfOneName is the identity rule stated as a
// check rather than assumed.
//
// Two definitions of one name would move every later symbol index, and the
// runtime would write one table for two questions.
func TestAddDescriptorsRefusesTwoCachesOfOneName(t *testing.T) {
	_, coder := itabPair(t)
	p := obj.NewPackage("main")
	same := ir.TypeAssert{Sym: "main.f..typeAssert.0", Iface: coder}
	err := addDescriptors(&Config{Package: "main"}, p, []*ir.Type{coder}, nil, nil, []ir.TypeAssert{same, same}, nil, nil)
	if err == nil {
		t.Fatal("two caches of one name were written")
	}
	if !strings.Contains(err.Error(), "two sites name one cache") {
		t.Errorf("the refusal is %q", err)
	}
}

// TestCheckFilesRecordsTheLanguageVersion is the Go 1.22 loop variable rule,
// which is decided per file and read by the body builder.
//
// The checker fills Info.FileVersions only when the map is there to fill, and
// a nil map answers "no version" for every file. The builder reads that as the
// current release, so a package compiled with -lang=go1.21 would export a loop
// that declares its variables once for the whole loop as one that declares
// them anew on every iteration. gc reads the flag and rewrites the loop, so
// the difference is a program that captures a different variable.
func TestCheckFilesRecordsTheLanguageVersion(t *testing.T) {
	const src = `package p

func F() []func() {
	var out []func()
	for i := 0; i < 3; i++ {
		out = append(out, func() { _ = i })
	}
	return out
}
`
	for _, tc := range []struct {
		lang     string
		distinct bool
	}{
		{"go1.21", false},
		{"go1.27", true},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.go")
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &Config{Package: "p", Output: filepath.Join(dir, "_pkg_.a"), Lang: tc.lang, Files: []string{path}}
			files, fset, err := parseFiles(cfg)
			if err != nil {
				t.Fatal(err)
			}
			pkg, info, err := checkFiles(cfg, newImporter(cfg), files, fset)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.FileVersions) == 0 {
				t.Fatal("the checker recorded no file version, so the loop variable rule has nothing to read")
			}

			fn := files[0].DeclList[0].(*syntax.FuncDecl)
			obj, _ := info.Defs[fn.Name].(*types2.Func)
			if obj == nil {
				t.Fatal("the checker recorded no object for F")
			}
			body, err := export.NewBodySource(pkg, info, fset).BuildBody("p.F", obj.Signature(), fn.Body)
			if err != nil {
				t.Fatal(err)
			}
			loop := body.Stmts[1].(*export.ForStmt)
			if loop.DistinctVars != tc.distinct {
				t.Errorf("-lang=%s built the loop with DistinctVars %v, want %v", tc.lang, loop.DistinctVars, tc.distinct)
			}
		})
	}
}

// TestCompileWritesInlinableBodies is the driver's half of the body wiring:
// the archive nanogo writes carries the bodies an importer can inline.
//
// internal/e2e has the check that matters, where gc reads one and inlines it.
// This one is the unit form and names what the driver decides: which
// declarations are offered at all.
func TestCompileWritesInlinableBodies(t *testing.T) {
	needGoCommand(t)
	arm64Only(t)
	const src = `package p

func Add(a, b int) int { return a + b }

//go:noinline
func Slow(a, b int) int { return a - b }

func Later(f func()) { defer f() }
`
	out, err := compileSource(t, src, func(c *Config) {
		c.Package = "xtest/p"
		c.Pack = true
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	names := archiveBodies(t, "xtest/p", out)
	if len(names) != 1 || names[0] != "Add" {
		t.Fatalf("the archive carries bodies for %v, want only Add", names)
	}
}

// TestCompileSkipsABodyADirectiveForbids pins the two skips separately, so a
// change that widened either one is reported as itself.
func TestCompileSkipsABodyADirectiveForbids(t *testing.T) {
	needGoCommand(t)
	arm64Only(t)
	for _, tc := range []struct{ name, decl string }{
		{"noinline", "//go:noinline\nfunc F(a int) int { return a }\n"},
		{"nosplit", "//go:nosplit\nfunc F(a int) int { return a }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := compileSource(t, "package p\n\n"+tc.decl, func(c *Config) {
				c.Package = "xtest/" + tc.name
				c.Pack = true
			})
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if names := archiveBodies(t, "xtest/"+tc.name, out); len(names) != 0 {
				t.Fatalf("the archive carries bodies for %v, want none", names)
			}
		})
	}
}

// archiveBodies returns the declarations whose bodies an archive carries.
func archiveBodies(t *testing.T, path, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := export.Payload(data)
	if err != nil {
		t.Fatal(err)
	}
	dec := pkgbits.NewPkgDecoder(path, string(payload))
	_, bodies, err := export.ReadBodies(types2.NewContext(), map[string]*types2.Package{}, dec)
	if err != nil {
		t.Fatalf("reading the bodies back: %v", err)
	}
	names := make([]string, len(bodies))
	for i, b := range bodies {
		names[i] = b.Name
	}
	return names
}

// TestRtypeSizeHonoursTheZeroFilledKind covers the one symbol whose size and
// data disagree.
//
// obj.Symbol separates the two because a BSS symbol has a size and no data and
// the linker allocates the space. Only one symbol rtype returns is of that
// kind, the pointer mask the runtime fills in on demand, and writing len(Data)
// for it defines a symbol of no space that the runtime then writes a word
// into. The e2e programs reach it and this pins the rule where it is written.
func TestRtypeSizeHonoursTheZeroFilledKind(t *testing.T) {
	for _, tc := range []struct {
		what string
		sym  rtype.Symbol
		want uint32
	}{
		{"a symbol carrying its own bytes", rtype.Symbol{Data: make([]byte, 24)}, 24},
		{"a symbol carrying none", rtype.Symbol{}, 0},
		{"the zero-filled kind", rtype.Symbol{Kind: obj.SNOPTRBSS, Size: 8}, 8},
	} {
		if got := rtypeSize(tc.sym); got != tc.want {
			t.Errorf("%s is %d bytes, want %d", tc.what, got, tc.want)
		}
	}
}
