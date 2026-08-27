// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// The tests in this file read the archives a real build produced. The
// round trip in read_test.go proves the reader agrees with nanogo's
// writer, and nothing more: an object gc wrote holds records nanogo does
// not write yet. See specs/045-linker.md, "Complete accounting".

package link

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"golang.design/x/nanogo/obj"
)

// requireCorpus reports whether a missing toolchain is a failure rather
// than a reason to skip. CI sets NANOGO_REQUIRE_CORPUS, so a gate that
// silently stopped running would turn red instead of green.
func requireCorpus() bool { return os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" }

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

// A build is a program the go command built with its work directory kept,
// so the tests can read the import configuration and the archives it names.
type build struct {
	once sync.Once
	err  error
	dir  string // the module directory
	work string // the go command's work directory
	cfg  string // importcfg.link
	exe  string
}

// hostBuild is the one program every test here reads. It is built once.
var hostBuild build

// token is a string that appears in the built program and nowhere else, so
// a cached replay of an older build is visible rather than silent.
const token = "nanogo-link-corpus-b41d"

func (b *build) get(t *testing.T) *build {
	t.Helper()
	goCmd := goTool(t)
	b.once.Do(func() {
		b.dir, b.err = os.MkdirTemp("", "nanogo-linkcorpus")
		if b.err != nil {
			return
		}
		files := map[string]string{
			"go.mod":  "module nanogo.example/linkcorpus\n\ngo 1.27\n",
			"main.go": "package main\n\nimport \"os\"\n\nfunc main() {\n\tif len(os.Args) > 99 {\n\t\tprintln(\"" + token + "\")\n\t}\n}\n",
		}
		for name, body := range files {
			if b.err = os.WriteFile(filepath.Join(b.dir, name), []byte(body), 0o600); b.err != nil {
				return
			}
		}
		b.exe = filepath.Join(b.dir, "prog")
		cmd := exec.Command(goCmd, "build", "-work", "-o", b.exe, ".")
		cmd.Dir = b.dir
		cmd.Env = append(os.Environ(), "GOPROXY=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			b.err = fmt.Errorf("go build: %v\n%s", err, out)
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			if w, ok := strings.CutPrefix(line, "WORK="); ok {
				b.work = strings.TrimSpace(w)
			}
		}
		if b.work == "" {
			b.err = fmt.Errorf("go build did not report its work directory:\n%s", out)
			return
		}
		b.err = filepath.WalkDir(b.work, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == "importcfg.link" {
				b.cfg = path
			}
			return nil
		})
		if b.err == nil && b.cfg == "" {
			b.err = fmt.Errorf("the go command produced no importcfg.link")
		}
	})
	if b.err != nil {
		t.Fatalf("cannot build a program to read the archives of: %v", b.err)
	}
	return b
}

func TestMain(m *testing.M) {
	code := m.Run()
	for _, dir := range []string{hostBuild.work, hostBuild.dir} {
		if dir != "" {
			os.RemoveAll(dir)
		}
	}
	os.Exit(code)
}

// packagefiles returns the import path and archive path of every package
// the program links, in the order the configuration lists them.
func (b *build) packagefiles(t *testing.T) [][2]string {
	t.Helper()
	data, err := os.ReadFile(b.cfg)
	if err != nil {
		t.Fatalf("reading the import configuration: %v", err)
	}
	var out [][2]string
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "packagefile ")
		if !ok {
			continue
		}
		pkg, file, ok := strings.Cut(rest, "=")
		if !ok {
			t.Fatalf("the import configuration has a packagefile line with no path: %q", line)
		}
		out = append(out, [2]string{pkg, file})
	}
	if len(out) == 0 {
		t.Fatal("the import configuration names no package")
	}
	return out
}

// TestReadsEveryArchiveOfARealBuild is the corpus gate.
//
// Every archive a hello-world links is read, and every object in it must
// account for itself. A refusal here is either a reader that is wrong or a
// record of the format nanogo does not know about, and both are worth
// stopping for.
func TestReadsEveryArchiveOfARealBuild(t *testing.T) {
	b := hostBuild.get(t)
	var objects, defs, refs, relocs, auxes, uncovered int
	var strBytes, strCovered int
	for _, pf := range b.packagefiles(t) {
		pkg, file := pf[0], pf[1]
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading the archive of %s: %v", pkg, err)
		}
		objs, err := ReadFile(data, file, pkg)
		if err != nil {
			t.Fatalf("%s: %v", pkg, err)
		}
		for _, o := range objs {
			objects++
			defs += o.NDef()
			refs += len(o.NonPkgRefs)
			for i := 0; i < o.NDef(); i++ {
				s := o.Local(i)
				relocs += len(s.Relocs)
				auxes += len(s.Aux)
			}
			strBytes += o.StringBytes
			strCovered += o.StringCovered
			if o.StringCovered != o.StringBytes {
				uncovered++
			}
			if o.Unlinkable() {
				t.Errorf("%s is unlinkable, and cmd/link refuses one", o.Name)
			}
		}
	}
	t.Logf("%d objects, %d definitions, %d references, %d relocations, %d auxiliary entries",
		objects, defs, refs, relocs, auxes)
	t.Logf("string region: %d of %d bytes reached by a reference, %d objects with a gap",
		strCovered, strBytes, uncovered)
	if objects < 10 {
		t.Fatalf("only %d objects were read, so this test proves little", objects)
	}
	// The region is not covered completely and the reason is gc's: see
	// Object.checkStringRegion. What the reader requires is that the
	// residue is strings, which it checks on every object above. A large
	// residue would mean a block whose strings the reader stopped
	// resolving, so the fraction is gated as well as reported.
	if got := float64(strCovered) / float64(strBytes); got < 0.99 {
		t.Errorf("a reference reached %.2f%% of the string bytes, and gc's dead names are under one per cent of them",
			100*got)
	}
}

// TestAgreesWithNM compares the symbols the reader finds with the ones
// go tool nm reports for the same archive.
//
// nm reads the object with cmd/internal/goobj, so it is a second opinion
// on the same bytes and not a second opinion on the same reader.
func TestAgreesWithNM(t *testing.T) {
	goCmd := goTool(t)
	b := hostBuild.get(t)

	for _, pf := range b.packagefiles(t) {
		pkg, file := pf[0], pf[1]
		switch pkg {
		case "runtime", "os", "internal/abi", "errors", "sync":
		default:
			continue
		}
		t.Run(pkg, func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("reading the archive: %v", err)
			}
			objs, err := ReadFile(data, file, pkg)
			if err != nil {
				t.Fatal(err)
			}
			// nm reports one entry per definition, named the way the
			// loader names it: the plain name for ABI0 and the name with
			// the version appended for anything else. The four definition
			// spaces are all definitions, and an anonymous payload has no
			// name for either side to print.
			mine := map[string]int{}
			for _, o := range objs {
				for _, list := range [][]*Sym{o.Defs, o.Hashed64Defs, o.HashedDefs, o.NonPkgDefs} {
					for _, s := range list {
						if s.Name != "" {
							mine[nmName(s)]++
						}
					}
				}
			}
			var missing []string
			seen := 0
			for _, line := range strings.Split(string(out(t, goCmd, "tool", "nm", file)), "\n") {
				code, name, ok := parseNM(line)
				if !ok || name == "" {
					continue
				}
				// U is a reference with no definition in this archive and
				// ? is a file name, which is not a symbol.
				if code == 'U' || code == '?' {
					continue
				}
				seen++
				if mine[name] == 0 && len(missing) < 10 {
					missing = append(missing, name)
				}
			}
			if seen == 0 {
				t.Fatal("go tool nm reported no symbol")
			}
			if len(missing) > 0 {
				t.Errorf("go tool nm reports %d symbols and the reader did not find %v", seen, missing)
			}
			t.Logf("%d symbols agree with go tool nm", seen)
		})
	}
}

// nmName is the name go tool nm prints for a definition.
//
// nm versions a file-static symbol and nothing else, so a name it prints
// with "<1>" is a static and a bare name is any ABI. That is not the
// linker's mapping, where the ABI is part of the identity, and the
// difference is why this comparison is over names and not over the pairs.
func nmName(s *Sym) string {
	if s.ABI == obj.ABIStatic {
		return s.Name + "<1>"
	}
	return s.Name
}

// parseNM splits one line of go tool nm output.
//
// The format is eight columns of address, a space, the one character code,
// a space, and the name. A name may hold spaces, so the fields are taken
// by position and not by splitting. An archive of more than one member is
// printed with the member in front of each line, ended by a tab.
func parseNM(line string) (code byte, name string, ok bool) {
	if i := strings.LastIndexByte(line, '\t'); i >= 0 {
		line = line[i+1:]
	}
	if len(line) < 11 || line[8] != ' ' || line[10] != ' ' {
		return 0, "", false
	}
	return line[9], line[11:], true
}

// out runs a command and fails the test on an error.
func out(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	b, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return b
}

// TestBuiltinListMatchesTheToolchain compares the pinned list of
// predeclared runtime symbols with the installed toolchain's.
//
// A reference to one of these carries an index and no name, so a list that
// is one entry out resolves every reference past that entry to the wrong
// symbol, and nothing else in the object would say so. The list is pinned
// the way obj.Magic is: specs/040-object-format.md says a version mismatch
// is an error and not a negotiation, and this is where the mismatch is
// reported.
func TestBuiltinListMatchesTheToolchain(t *testing.T) {
	goCmd := goTool(t)
	root := strings.TrimSpace(string(out(t, goCmd, "env", "GOROOT")))
	path := filepath.Join(root, "src", "cmd", "internal", "goobj", "builtinlist.go")
	src, err := os.ReadFile(path)
	if err != nil {
		if requireCorpus() {
			t.Fatalf("NANOGO_REQUIRE_CORPUS is set and the toolchain's builtin list is not readable: %v", err)
		}
		t.Skipf("no toolchain source to compare against: %v", err)
	}
	entry := regexp.MustCompile(`\{"([^"]+)",\s*(\d+)\},`)
	found := entry.FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatalf("%s holds no entry this test recognises", path)
	}
	if len(found) != NumBuiltin() {
		t.Fatalf("the toolchain declares %d predeclared symbols and this package pins %d", len(found), NumBuiltin())
	}
	for i, m := range found {
		name, abi, _ := BuiltinName(i)
		wantABI := uint16(obj.ABI0)
		if m[2] == "1" {
			wantABI = obj.ABIInternal
		}
		if name != m[1] || abi != wantABI {
			t.Fatalf("builtin %d is %q ABI %s in the toolchain and %q ABI %d here", i, m[1], m[2], name, abi)
		}
	}
	t.Logf("%d predeclared symbols agree with the installed toolchain", NumBuiltin())
}
