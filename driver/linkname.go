// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"strings"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtype"
	"golang.design/x/nanogo/syntax"
)

// The //go:linkname model of specs/047-abi-wrappers.md stage 2.
//
// The directive decides two things about the ABI of a symbol, and
// ssagen.GenABIWrappers reads both:
//
//   - the name the symabis file's def and ref lines are matched against.
//     gc takes sym.Linkname first and sym.Pkg.Prefix + "." + sym.Name second,
//     so internal/bytealg's `//go:linkname abigen_runtime_cmpstring
//     runtime.cmpstring` is what makes the line `def runtime.cmpstring
//     ABIInternal` land on that declaration and on nothing else.
//   - whether the symbol is callable under both ABIs. gc's conjunction is
//     `sym.Linkname != "" && (hasBody || hasDefABI) && len(cgoExport) == 0`,
//     and the first term is true for the one-argument form as well, because
//     noder fills the target in with the default name. So the two questions
//     are separate booleans and never one: "was the directive written" and
//     "does it rename the symbol".
//
// Getting the second wrong is silent and large. internal/runtime/atomic writes
// //go:linknamestd on forty-nine bodyless declarations that assembly defines
// under ABI0. With the callable bit they owe forty-nine ABIInternal wrappers.
// Without it they owe none, and every Go call to atomic.Xadd resolves to a
// symbol nothing defines.

// Linkname is one decoded //go:linkname or //go:linknamestd directive.
type Linkname struct {
	// Local is the name as declared in this package.
	Local string

	// Target is the linker symbol the directive names. For the one-argument
	// form it is the default object symbol name of Local, which is what
	// noder.pragma fills in, so this field is never empty.
	Target string

	// Default is the object symbol name Local would have without any
	// directive, which is objabi.PathToPrefix of the import path and then the
	// name. It is kept beside Target because a declaration is found by one and
	// renamed to the other, and because ir.Build spells a package-level
	// variable's object with the prefix already on it.
	Default string

	// Renames reports that the directive wrote a second argument that is not
	// the default name. It is the property that decides whether the symbol
	// nanogo emits has to be emitted under another name, and it is not the
	// same question as whether the directive was written.
	Renames bool

	// Std records the //go:linknamestd spelling. gc marks the symbol
	// AttrLinknameStd with it, which is how cmd/link tells a pull inside the
	// standard library from one out of it. nanogo records it and the ABI
	// decision does not read it: gc's own conjunction tests sym.Linkname,
	// which both spellings set.
	Std bool

	// Pos is where the directive was written.
	Pos syntax.Pos
}

// ParseLinkname decodes one directive against the package being compiled.
//
// It is noder.pragma's case for the two verbs, with gc's own message. The
// second argument is optional: when it is omitted the target is the default
// object symbol name, and the directive then only marks the symbol as one
// another package may name.
func ParseLinkname(pkgPath string, d LinknameText) (Linkname, error) {
	f := strings.Fields(d.Text)
	if len(f) < 2 || len(f) > 3 {
		return Linkname{}, fmt.Errorf("usage: //%s localname [linkname]", d.Verb)
	}
	if pkgPath == "" {
		return Linkname{}, fmt.Errorf("//%s needs the import path of the package being compiled", d.Verb)
	}
	l := Linkname{
		Local:   f[1],
		Default: rtype.PathToPrefix(pkgPath) + "." + f[1],
		Std:     d.Verb == "go:linknamestd",
		Pos:     d.Pos,
	}
	l.Target = l.Default
	if len(f) == 3 {
		l.Target = f[2]
		l.Renames = f[2] != l.Default
	}
	return l, nil
}

// Linknames indexes a package's decoded directives by the local name.
//
// A local name may carry more than one directive in a legal program, and gc
// applies them in order and keeps the last, so this keeps the last as well.
type Linknames struct {
	byLocal map[string]Linkname
	order   []string
}

// ParseLinknames decodes every directive the package wrote.
//
// The first malformed one is the error, which is the position a reader acts
// on, and it is a hard error for the reason specs/047-abi-wrappers.md gives
// for a malformed symabis line: a directive nanogo dropped would make the ABI
// decision be taken against the wrong symbol name and produce a definition
// under a name nothing calls.
func ParseLinknames(pkgPath string, seen *sourceDirectives) (*Linknames, error) {
	ls := &Linknames{byLocal: make(map[string]Linkname)}
	if seen == nil {
		return ls, nil
	}
	for _, d := range seen.Linknames {
		l, err := ParseLinkname(pkgPath, d)
		if err != nil {
			return nil, err
		}
		if _, dup := ls.byLocal[l.Local]; !dup {
			ls.order = append(ls.order, l.Local)
		}
		ls.byLocal[l.Local] = l
	}
	return ls, nil
}

// Of returns the directive that names local, if the package wrote one.
func (ls *Linknames) Of(local string) (Linkname, bool) {
	if ls == nil {
		return Linkname{}, false
	}
	l, ok := ls.byLocal[local]
	return l, ok
}

// All returns every directive, in the order the package wrote the first
// occurrence of each local name.
//
// Sorted by nothing and ranged over a slice, because a diagnostic built from
// a map walk would name a different directive between two runs over one input
// (specs/053-determinism.md).
func (ls *Linknames) All() []Linkname {
	if ls == nil {
		return nil
	}
	out := make([]Linkname, 0, len(ls.order))
	for _, local := range ls.order {
		out = append(out, ls.byLocal[local])
	}
	return out
}

// SymOf returns the linker symbol name the ABI decision matches fn under.
//
// It is gc's two lines, in gc's order: the linkname if the declaration
// carries one, and the declaration's own symbol otherwise.
//
// A method never carries one. gc binds the directive to a package-scope
// object, and a method's name is not one: matching a method by its bare name
// would give T.M the directive written for a package-scope function M.
func (ls *Linknames) SymOf(fn *ir.Func) string {
	if fn.Recv != nil {
		return fn.Sym
	}
	if l, ok := ls.Of(fn.Name); ok {
		return l.Target
	}
	return fn.Sym
}
