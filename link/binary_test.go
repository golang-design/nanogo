// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// The helpers in this file read the executable the real linker wrote
// from the same archives the loader reads. It is a second opinion the
// dependency dump cannot give: the dump is a list of edges, so a symbol
// cmd/link keeps without an edge is absent from it, and the symbol table
// of the binary has no such gap.

package link

import (
	"debug/macho"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// linkExe runs the real linker on the same archives the loader reads and
// returns the executable it wrote.
//
// The build the corpus keeps is not used, because the go command passes
// -buildid and the linker then puts a go:buildid symbol at the front of
// the text segment, which moves every function after it. The linker run
// here is the one the reachability oracle uses.
func linkExe(t *testing.T, b *build) string {
	t.Helper()
	goCmd := goTool(t)
	main := ""
	for _, pf := range b.packagefiles(t) {
		if pf[0] == b.pkg {
			main = pf[1]
		}
	}
	if main == "" {
		t.Fatal("the import configuration does not name the main package")
	}
	exe := filepath.Join(t.TempDir(), "a.out")
	cmd := exec.Command(goCmd, "tool", "link", "-importcfg", b.cfg, "-o", exe, main)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go tool link: %v\n%s", err, head(out))
	}
	return exe
}

// nmText returns the address of every text symbol in an executable, by
// the name the linker put in the symbol table.
//
// The names are the symbol table's own. Mach-O prefixes an underscore to
// a name that holds no dot, and [nmMatch] is where that is undone, so
// that one entry never occupies two keys here.
func nmText(t *testing.T, exe string) map[string]uint64 {
	t.Helper()
	goCmd := goTool(t)
	addrs := map[string]uint64{}
	dup := map[string]bool{}
	for _, line := range strings.Split(string(out(t, goCmd, "tool", "nm", "-sort", "address", exe)), "\n") {
		addr, code, name, ok := parseNMLine(line)
		if !ok || name == "" || (code != 'T' && code != 't') {
			continue
		}
		if _, seen := addrs[name]; seen {
			dup[name] = true
		}
		addrs[name] = addr
	}
	// A name two symbols share says nothing about either one, so it is
	// dropped rather than compared against an arbitrary half of the pair.
	for n := range dup {
		delete(addrs, n)
	}
	return addrs
}

// nmMatch finds a symbol table entry among names the linker's loader
// spells without Mach-O's underscore.
func nmMatch[T any](mine map[string]T, name string) (T, bool) {
	if v, ok := mine[name]; ok {
		return v, true
	}
	if trimmed := strings.TrimPrefix(name, "_"); trimmed != name {
		if v, ok := mine[trimmed]; ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// symtabName is the name the linker writes in the symbol table for a text
// symbol.
//
// An ABI0 function whose ABIInternal form is also in the binary is
// renamed, because two entries of one name are an error for an external
// linker. cmd/link's mangleABIName is the rule and without it the ABI0
// entry point of a function would be compared against the address of its
// body.
func (l *Loader) symtabName(g Global) string {
	name := l.Name(g)
	if name == "" {
		return ""
	}
	s := l.Def(g)
	if s == nil || !s.Type.IsText() {
		return name
	}
	if ver := l.version(l.objs[l.objSyms[g].obj], s.ABI); ver != VerABIInternal && ver < verStatic {
		if s2 := l.Lookup(name, VerABIInternal); s2 != 0 {
			if d := l.Def(s2); d != nil && d.Type.IsText() {
				return name + ".abi" + itoa(ver)
			}
		}
	}
	return name
}

// parseNMLine splits one line of go tool nm output for an executable.
//
// The address column is as wide as the addresses need, which is nine
// digits for a Mach-O executable and eight for an archive member, so the
// fields are found rather than taken by position. A name holds spaces of
// its own, so only the two fields in front of it are split off.
func parseNMLine(line string) (addr uint64, code byte, name string, ok bool) {
	if i := strings.LastIndexByte(line, '\t'); i >= 0 {
		line = line[i+1:]
	}
	line = strings.TrimLeft(line, " ")
	i := strings.IndexByte(line, ' ')
	if i <= 0 {
		return 0, 0, "", false
	}
	addr, err := strconv.ParseUint(line[:i], 16, 64)
	if err != nil {
		return 0, 0, "", false
	}
	rest := line[i+1:]
	if len(rest) < 3 || rest[1] != ' ' {
		return 0, 0, "", false
	}
	return addr, rest[0], rest[2:], true
}

// machoSections returns the size of every section of a Mach-O
// executable, by the name the linker gave it.
//
// The names are Mach-O's, which are the linker's own with the dots
// replaced and cut to sixteen characters, so .go.type is __go_type. A
// name that two segments both use is recorded under the segment and the
// section both.
func machoSections(t *testing.T, exe string) map[string]uint64 {
	t.Helper()
	f, err := macho.Open(exe)
	if err != nil {
		t.Fatalf("reading the executable: %v", err)
	}
	defer f.Close()
	out := map[string]uint64{}
	for _, s := range f.Sections {
		out[s.Seg+","+s.Name] = s.Size
		if _, seen := out[s.Name]; !seen {
			out[s.Name] = s.Size
		}
	}
	if len(out) == 0 {
		t.Fatal("the executable has no section")
	}
	return out
}
