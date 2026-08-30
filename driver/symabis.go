// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.design/x/nanogo/obj"
)

// The symabis file, per specs/047-abi-wrappers.md.
//
// The go command compiles a package that has assembly in two passes. It
// writes an empty go_asm.h, runs cmd/asm with -gensymabis to produce a
// symabis file, runs the compiler with -symabis and -asmhdr, then runs cmd/asm
// again for real against the header the compiler wrote. The compile line for
// internal/cpu, logged from a real build, is
//
//	asm -p internal/cpu -I $WORK/b006/ -I $GOROOT/pkg/include \
//	    -D GOOS_darwin -D GOARCH_arm64 -shared -std -gensymabis \
//	    -o $WORK/b006/symabis ./cpu.s ./cpu_arm64.s
//	compile ... -symabis $WORK/b006/symabis ... -asmhdr $WORK/b006/go_asm.h <go files>
//
// The file is what the assembler learned about the ABI of every text symbol
// its input names. It is the compiler's only way to know that a bodyless Go
// declaration is satisfied by an ABI0 definition, and therefore its only way
// to know that a Go call to it needs a wrapper.

// SymABI is the calling convention a symabis line names.
//
// It is obj.ABI0 or obj.ABIInternal and never anything else. cmd/asm writes
// what obj.ParseABI accepts, and specs/047-abi-wrappers.md refuses a third
// spelling by name rather than guessing at it: a value nanogo mapped to the
// wrong convention places every argument of that symbol at the wrong offset,
// and nothing reports it.
type SymABI = uint16

// SymABIRecord is one def or ref line, in the order the file gave it.
type SymABIRecord struct {
	Def  bool   // "def" rather than "ref"
	Sym  string // the linker symbol name
	ABI  SymABI // obj.ABI0 or obj.ABIInternal
	Line int    // the 1-based line the record was read from
}

// SymABIs is a decoded symabis file.
//
// Both the ordered record list and the two lookup tables are kept. The tables
// answer "what is this symbol's definition ABI", which is what the wrapper
// decision asks. The list is what a diagnostic walks, because a message that
// named symbols in map order would differ between two runs over one input
// (specs/053-determinism.md).
type SymABIs struct {
	// Records is every line, in file order.
	Records []SymABIRecord

	// defs maps a symbol to the ABI its assembly definition uses. A symbol
	// defined twice keeps the last, which is what gc's map assignment does.
	defs map[string]SymABI

	// refs maps a symbol to the set of ABIs some assembly file names it
	// under, as a bit per ABI. gc stores the same thing as an obj.ABISet.
	refs map[string]abiSet
}

// abiSet is a set of ABIs, one bit per obj.ABI value. It is gc's obj.ABISet
// with the same bit positions, because the wrapper decision is a set
// difference over it and a different encoding would be a second statement of
// the same rule.
type abiSet uint8

// abiSetOf is the singleton set holding one ABI.
func abiSetOf(a SymABI) abiSet { return 1 << a }

// Has reports whether s holds a.
func (s abiSet) Has(a SymABI) bool { return s&abiSetOf(a) != 0 }

// parseABI is obj.ParseABI: the two spellings cmd/asm writes and nothing
// else.
//
// A spelling outside the two is refused by name. specs/047-abi-wrappers.md
// makes that a rule rather than a fallback: obj.ParseABI is the only accepted
// spelling and a fourth value would have to be guessed at.
func parseABI(name string) (SymABI, bool) {
	switch name {
	case "ABI0":
		return obj.ABI0, true
	case "ABIInternal":
		return obj.ABIInternal, true
	}
	return 0, false
}

// abiName is the spelling of an ABI, for a diagnostic.
func abiName(a SymABI) string {
	switch a {
	case obj.ABI0:
		return "ABI0"
	case obj.ABIInternal:
		return "ABIInternal"
	}
	return fmt.Sprintf("ABI(%d)", a)
}

// ReadSymABIs reads the file -symabis names.
func ReadSymABIs(file string) (*SymABIs, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("-symabis: %v", err)
	}
	return ParseSymABIs(file, data)
}

// ParseSymABIs decodes a symabis file.
//
// The format is ssagen.SymABIs.ReadSymABIs's, and the real files confirm it:
// whitespace-separated fields, one record per line, blank lines and lines
// starting with # skipped. The first field is "def" or "ref", the second is
// the linker symbol name, the third is the ABI name.
//
// Every malformed line is an error and never a skip. gc calls log.Fatalf on
// each of the three, and the reason is the same one specs/047-abi-wrappers.md
// gives for refusing an unknown ABI: a reader that dropped the lines it did
// not understand would decide that a symbol needs no wrapper because it never
// read the line that says it does.
func ParseSymABIs(file string, data []byte) (*SymABIs, error) {
	s := &SymABIs{
		defs: make(map[string]SymABI),
		refs: make(map[string]abiSet),
	}
	for i, line := range strings.Split(string(data), "\n") {
		num := i + 1 // 1-based, as gc reports it
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		switch parts[0] {
		case "def", "ref":
		default:
			return nil, fmt.Errorf(`%s:%d: invalid symabi type %q`, file, num, parts[0])
		}
		if len(parts) != 3 {
			return nil, fmt.Errorf(`%s:%d: invalid symabi: syntax is "%s sym abi"`, file, num, parts[0])
		}
		abi, ok := parseABI(parts[2])
		if !ok {
			return nil, fmt.Errorf(`%s:%d: invalid symabi: unknown abi %q`, file, num, parts[2])
		}
		rec := SymABIRecord{Def: parts[0] == "def", Sym: parts[1], ABI: abi, Line: num}
		s.Records = append(s.Records, rec)
		if rec.Def {
			s.defs[rec.Sym] = rec.ABI
		} else {
			s.refs[rec.Sym] |= abiSetOf(rec.ABI)
		}
	}
	return s, nil
}

// Def returns the ABI an assembly file defines sym with.
func (s *SymABIs) Def(sym string) (SymABI, bool) {
	if s == nil {
		return 0, false
	}
	a, ok := s.defs[sym]
	return a, ok
}

// Refs returns the set of ABIs some assembly file names sym under.
func (s *SymABIs) Refs(sym string) abiSet {
	if s == nil {
		return 0
	}
	return s.refs[sym]
}

// Defs returns every defined symbol, sorted.
//
// Sorted rather than in file order, because the one caller compares sets and
// a listing built from a map would differ between two runs over one input
// (specs/053-determinism.md).
func (s *SymABIs) Defs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.defs))
	for sym := range s.defs {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}
