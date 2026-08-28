// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestTextLayoutAgreesWithTheLinker is specs/045-linker.md's oracle for
// address assignment, over the text segment.
//
// The comparison is per symbol and not over the section size. Both
// linkers lay the same symbols out, so a size that agrees can still hold
// two functions in the wrong order, and an offset that disagrees names
// the first function the two orders part company at.
//
// The offsets are measured from runtime.text rather than compared as
// addresses, because the segment's own base is the output writer's
// business and this stage does not have one yet.
func TestTextLayoutAgreesWithTheLinker(t *testing.T) {
	target := TargetFor(runtime.GOOS, runtime.GOARCH)
	if target == nil {
		t.Skipf("no layout arithmetic for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	for _, b := range []*build{&hostBuild, &reflectBuild} {
		t.Run(b.pkg, func(t *testing.T) {
			b := b.get(t)
			want := nmText(t, linkExe(t, b))
			base, ok := want[symTextStart]
			if !ok {
				t.Fatalf("the linker's symbol table has no %s", symTextStart)
			}

			l := loadProgram(t, b)
			l.InitTasks()
			r := l.Deadcode(runtime.GOOS, runtime.GOARCH)
			a := l.Layout(r, target)

			mine := map[string]uint64{}
			for _, g := range a.Textp {
				if name := l.symtabName(g); name != "" {
					mine[name] = a.addr[g] - a.TextStart
				}
			}

			var wrong []string
			compared := 0
			for name, addr := range want {
				got, ok := nmMatch(mine, name)
				if !ok {
					continue
				}
				compared++
				if got != addr-base {
					wrong = append(wrong, name+": cmd/link puts it at +0x"+
						strconv.FormatUint(addr-base, 16)+" and nanogo at +0x"+
						strconv.FormatUint(got, 16))
				}
			}
			sort.Strings(wrong)
			t.Logf("%d text symbols laid out, %d of them named in the linker's symbol table",
				len(a.Textp), compared)
			if compared < 1000 {
				t.Fatalf("only %d symbols could be compared, so this proves little", compared)
			}
			if len(wrong) > 0 {
				t.Errorf("%d text symbols are at a different offset, the first are %v",
					len(wrong), first(wrong, 10))
			}

			// The end of the section is the second half of the check. An
			// error inside a symbol the linker hides would have to be
			// cancelled exactly by another one for both the shared
			// symbols and the end to agree.
			if end, ok := want[symTextEnd]; ok {
				if got := a.TextEnd - a.TextStart; got != end-base {
					t.Errorf("%s is at +0x%x for cmd/link and +0x%x for nanogo",
						symTextEnd, end-base, got)
				}
			} else {
				t.Errorf("the linker's symbol table has no %s", symTextEnd)
			}
		})
	}
}

// TestDataSectionSizesAgreeWithTheLinker is specs/045-linker.md's oracle
// for address assignment, over the data sections a linker at this stage
// can lay out completely.
//
// The two linkers have the same members in these five, so the size is a
// number they must agree on. Four hold only symbols the objects define.
// .bss holds those less the string variables the linker fills in, which
// [setStringVars] gives it from the same places cmd/link takes them
// from. The sections this stage does not build hold pclntab, the garbage
// collection data for the globals and the FIPS brackets.
func TestDataSectionSizesAgreeWithTheLinker(t *testing.T) {
	target := TargetFor(runtime.GOOS, runtime.GOARCH)
	if target == nil || runtime.GOOS != "darwin" {
		t.Skipf("the section names this reads are Mach-O's, and this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	// The Mach-O name of a section is the linker's own with the dots
	// replaced and the leading one doubled.
	machoName := map[string]string{
		".go.module": "__go_module",
		".noptrdata": "__noptrdata",
		".data":      "__data",
		".bss":       "__bss",
		".noptrbss":  "__noptrbss",
		".go.type":   "__go_type",
		".go.func":   "__go_func",
	}
	for _, b := range []*build{&hostBuild, &reflectBuild} {
		t.Run(b.pkg, func(t *testing.T) {
			b := b.get(t)
			want := machoSections(t, linkExe(t, b))

			l := loadProgram(t, b)
			setStringVars(t, l, b)
			l.InitTasks()
			r := l.Deadcode(runtime.GOOS, runtime.GOARCH)
			a := l.Layout(r, target)

			if len(a.Sections) != len(machoName) {
				t.Fatalf("the layout built %d sections and this compares %d", len(a.Sections), len(machoName))
			}
			for _, sect := range a.Sections {
				name, ok := machoName[sect.Name]
				if !ok {
					t.Errorf("the layout built a section %s this test does not know", sect.Name)
					continue
				}
				size, ok := want[name]
				if !ok {
					t.Errorf("the executable has no %s section", name)
					continue
				}
				t.Logf("%-11s %5d symbols, 0x%x bytes", sect.Name, len(sect.Syms), sect.Length)
				if sect.Length != size {
					t.Errorf("%s is 0x%x bytes for cmd/link and 0x%x for nanogo", sect.Name, size, sect.Length)
				}
			}
		})
	}
}

// TestRuntimePackagesMatchTheToolchain compares the pinned list of
// packages the runtime depends on with the installed toolchain's.
//
// The linker lays their text out first, so a list that is one package
// short moves every function of every package after it, and the binary
// still links and still runs. The list is pinned for the same reason the
// predeclared symbol list is.
func TestRuntimePackagesMatchTheToolchain(t *testing.T) {
	goCmd := goTool(t)
	root := strings.TrimSpace(string(out(t, goCmd, "env", "GOROOT")))
	path := filepath.Join(root, "src", "cmd", "internal", "objabi", "pkgspecial.go")
	src, err := os.ReadFile(path)
	if err != nil {
		if requireCorpus() {
			t.Fatalf("NANOGO_REQUIRE_CORPUS is set and the toolchain's package list is not readable: %v", err)
		}
		t.Skipf("no toolchain source to compare against: %v", err)
	}
	block := regexp.MustCompile(`(?s)var runtimePkgs = \[\]string\{(.*?)\n\}`).FindSubmatch(src)
	if block == nil {
		t.Fatalf("%s holds no runtimePkgs list this test recognises", path)
	}
	var want []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(string(block[1]), -1) {
		want = append(want, m[1])
	}
	sort.Strings(want)
	got := RuntimePackages()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the toolchain names %d runtime packages and this package pins %d\ntoolchain: %v\nnanogo:    %v",
			len(want), len(got), want, got)
	}
	t.Logf("%d runtime packages agree with the installed toolchain", len(got))
}

// setStringVars gives the loader the string variables the linker fills
// in, from the places cmd/link takes them from.
//
// None of the three is in any object. The module graph reaches cmd/link
// through the modinfo line of the import configuration, and the two
// others are the toolchain's description of itself, which it prints for
// -V. Reading them from the executable would prove nothing, because the
// executable is the thing under comparison.
func setStringVars(t *testing.T, l *Loader, b *build) {
	t.Helper()
	goCmd := goTool(t)
	if root := strings.TrimSpace(string(out(t, goCmd, "env", "GOROOT"))); root != "" {
		// cmd/go clears GOROOT for -trimpath, and cmd/link then leaves
		// the variable alone rather than writing an empty string.
		l.SetStringVar("runtime.defaultGOROOT", root)
	}
	l.SetStringVar("runtime.buildVersion", linkerBuildVersion(t, goCmd))
	data, err := os.ReadFile(b.cfg)
	if err != nil {
		t.Fatalf("reading the import configuration: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		arg, ok := strings.CutPrefix(line, "modinfo ")
		if !ok {
			continue
		}
		info, err := strconv.Unquote(strings.TrimSpace(arg))
		if err != nil {
			t.Fatalf("the import configuration has a modinfo line this cannot read: %v", err)
		}
		l.SetStringVar("runtime.modinfo", info)
	}
}

// linkerBuildVersion is the value cmd/link writes into
// runtime.buildVersion.
//
// It is the toolchain version, and the experiments that differ from the
// baseline appended. The linker prints the same pair for -V, with a
// space where the variable takes a dash for a version that holds none.
func linkerBuildVersion(t *testing.T, goCmd string) string {
	t.Helper()
	line := strings.TrimSpace(string(out(t, goCmd, "tool", "link", "-V")))
	version, ok := strings.CutPrefix(line, "link version ")
	if !ok {
		t.Fatalf("go tool link -V printed %q, which names no version", line)
	}
	if base, exp, ok := strings.Cut(version, " X:"); ok {
		sep := " "
		if !strings.Contains(base, "-") {
			sep = "-"
		}
		return base + sep + "X:" + exp
	}
	return version
}
