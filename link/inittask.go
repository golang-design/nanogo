// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import "golang.design/x/nanogo/obj"

// initTaskEntrySize is the size of an initialisation record that has
// nothing to run: a state word and two counts. cmd/link keeps a larger
// record and drops a record of exactly this size from the list, because
// the runtime needs only the records that have functions in them. About
// half of the standard library's packages have none.
const initTaskEntrySize = 8

// InitTasks builds the initialisation task lists.
//
// cmd/link synthesises them between loading and reachability, and they are
// roots: without the list the whole initialisation chain is unreachable
// and the reachable set is short by most of the program.
//
// The list is a topological order of the initialisation records, over the
// R_INITORDER edges an importer records against the record of each package
// it imports. Reachability needs the set and not the order, so what is
// built here is the set the schedule would hold.
func (l *Loader) InitTasks() {
	l.mainInitTasks = l.initTaskSym("main..inittask", "go:main.inittasks")

	// The runtime keeps its own list, in a slice header it declares. The
	// linker rewrites the header to point at the list it built, so the
	// edge exists in the program and in no object.
	if sh := l.Lookup("runtime.runtime_inittasks", VerABI0); sh != 0 {
		if t := l.initTaskSym("runtime..inittask", "go:runtime.inittasks"); t != 0 {
			l.extra[sh] = append(l.extra[sh], t)
		}
	}
}

// initTaskSym builds one list from one root record and returns the
// synthetic symbol that holds it, or 0 when the root is not in the link.
func (l *Loader) initTaskSym(root, name string) Global {
	r := l.Lookup(root, VerABI0)
	if r == 0 {
		return 0
	}
	// The closure over the ordering edges is every record the root's
	// package needs, directly or through an import.
	seen := map[Global]bool{r: true}
	queue := []Global{r}
	var closure []Global
	for len(queue) > 0 {
		g := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		closure = append(closure, g)
		st, s := l.def(g)
		if s == nil {
			continue
		}
		for _, rel := range s.Relocs {
			if rel.Type != obj.R_INITORDER {
				continue
			}
			t := l.resolve(st, rel.Sym)
			if t == 0 || seen[t] {
				continue
			}
			seen[t] = true
			queue = append(queue, t)
		}
	}
	// A record with nothing to run is used to order the others and is not
	// in the list the runtime walks.
	var targets []Global
	for _, g := range closure {
		if s := l.Def(g); s != nil && s.Size > initTaskEntrySize {
			targets = append(targets, g)
		}
	}
	// The list is initialised data and not read-only, which cmd/link's
	// inittaskSym marks with a reference to issue 58857. The size is one
	// pointer per record and it is given at layout time, where the width
	// of a pointer is known.
	return l.addSynthetic(name, VerABI0, KNOPTRDATA, 0, targets)
}

// applyInitTasks gives the initialisation task lists their sizes and
// moves the runtime's slice header to the initialised data.
//
// The lists are the linker's own symbols and hold one pointer per record
// that has a function to run. The header is a variable the runtime
// declares and the compiler left zero filled, and the linker fills it in
// with the address of the list and the two counts, which moves it out of
// .bss the way a string variable moves.
func (a *Layout) applyInitTasks(r *Reachability) {
	l := a.l
	for _, name := range []string{"go:main.inittasks", "go:runtime.inittasks"} {
		g := l.Lookup(name, VerABI0)
		syn := l.synthetic(g)
		if syn == nil || !a.reachable(r, g) {
			continue
		}
		syn.size = uint32(a.target.PtrSize) * uint32(len(syn.targets))
		a.setKind(g, syn.kind)
	}
	if g := l.Lookup("runtime.runtime_inittasks", VerABI0); g != 0 && a.reachable(r, g) {
		if a.kind[g] == KBSS {
			a.kind[g] = KNOPTRDATA
		}
	}
}

// MainInitTasks is the list the program runs at startup, or 0.
func (l *Loader) MainInitTasks() Global { return l.mainInitTasks }
