// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import "golang.design/x/nanogo/obj"

// A stringVar is the value the linker gives a string variable the
// compiler left zero filled.
type stringVar struct {
	name  string
	value string
}

// SetStringVar records the value the linker writes into a string
// variable, and returns whether the name is new.
//
// Three of these exist in every program and the compiler writes none of
// them. `runtime.buildVersion` and `runtime.defaultGOROOT` are the
// toolchain's own description of itself, and `runtime.modinfo` is the
// module graph the go command computed, which reaches cmd/link through
// the `modinfo` line of the import configuration and reaches no object at
// all. The -X flag adds more of them.
//
// The order is the order of the calls, because two calls for one name are
// the second value and one entry.
func (l *Loader) SetStringVar(name, value string) {
	for i := range l.strvars {
		if l.strvars[i].name == name {
			l.strvars[i].value = value
			return
		}
	}
	l.strvars = append(l.strvars, stringVar{name, value})
}

// StringVars returns the names of the string variables the linker fills
// in, in the order they were set.
func (l *Loader) StringVars() []string {
	out := make([]string, len(l.strvars))
	for i, v := range l.strvars {
		out[i] = v.name
	}
	return out
}

// suffixStringData is the name of the read-only symbol that holds the
// bytes of a string variable the linker filled in.
const suffixStringData = ".str"

// applyStringVars gives the string variables the linker fills in the
// kinds they end up with.
//
// A string variable the compiler left zero filled is in the zero filled
// data, and a variable the linker gives a value to is not: it holds a
// pointer to the bytes and a length, so it moves to the initialised data
// and the bytes become a read-only symbol of their own. The variable's
// size does not change, because the header is two words either way, and
// cmd/link's addstrdata says so in as many words.
//
// This is why .bss depends on an input rather than on a stage. The
// section's size is the sum of the variables that stay in it, and one of
// the three that leave is named by the import configuration and by no
// object.
func (a *Layout) applyStringVars(r *Reachability) {
	l := a.l
	for _, v := range l.strvars {
		g := l.Lookup(v.name, VerABI0)
		if g == 0 || !a.reachable(r, g) {
			// cmd/link sets no value on a variable the program does not
			// reach, because the variable is not in the binary.
			continue
		}
		if !l.isStringVar(g) {
			// cmd/link refuses a -X for a variable that is not a string,
			// because the two words it writes would be some other type's
			// bytes. A name nothing defines is skipped rather than
			// refused, which is how a -X for a package the link dropped
			// behaves.
			continue
		}
		if a.kind[g] == KBSS {
			a.kind[g] = KDATA
		}
		// The bytes become a read-only symbol of their own, with the
		// trailing NUL cmd/link's Addstring writes.
		a.define(v.name+suffixStringData, KRODATA, uint32(len(v.value))+1)
	}
}

// isStringVar reports whether a symbol is a variable of type string.
//
// The type comes from the symbol's own AuxGotype entry, which is the
// only place the object records it. A variable with no recorded type is
// not one the linker may write a string header into.
func (l *Loader) isStringVar(g Global) bool {
	st, s := l.def(g)
	if s == nil {
		return false
	}
	for _, a := range s.Aux {
		if a.Type == obj.AuxGotype {
			return l.Name(l.resolve(st, a.Sym)) == "type:string"
		}
	}
	return false
}
