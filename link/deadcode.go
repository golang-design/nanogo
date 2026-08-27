// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"fmt"

	"golang.design/x/nanogo/obj"
)

// EntrySymbol is the name of the entry point of a static executable.
//
// The runtime's assembly defines one per platform and reachability starts
// there rather than at main.main: everything that runs before the first Go
// function is reached through it.
func EntrySymbol(goos, goarch string) string {
	return fmt.Sprintf("_rt0_%s_%s", goarch, goos)
}

// ptrSize is the pointer width of a target. The descriptor decoders need
// it, and nanogo's targets are 64 bit.
func ptrSize(goarch string) int {
	switch goarch {
	case "386", "arm", "mips", "mipsle":
		return 4
	}
	return 8
}

// Reachability is the result of the pass: which symbols the program uses.
type Reachability struct {
	l    *Loader
	mark []bool
	from []Global
	n    int
}

// ReachedBy returns the symbol whose reference first reached g, or 0 for a
// root and for a symbol the program does not use. It is the edge
// cmd/link -dumpdep prints, and it is what turns a disagreement about one
// symbol into a chain that names its cause.
func (r *Reachability) ReachedBy(g Global) Global {
	if g <= 0 || int(g) >= len(r.from) {
		return 0
	}
	return r.from[g]
}

// Reachable reports whether the program uses a symbol.
func (r *Reachability) Reachable(g Global) bool {
	return g > 0 && int(g) < len(r.mark) && r.mark[g]
}

// Count is how many symbols the program uses.
func (r *Reachability) Count() int { return r.n }

// Names returns the name of every reachable symbol.
//
// A symbol with no name is left out. It is one nothing can name, and
// cmd/link -dumpdep prints no line for it either, so the two sets are
// comparable only without them.
func (r *Reachability) Names() map[string]bool {
	out := make(map[string]bool, r.n)
	for g := Global(1); int(g) < len(r.mark); g++ {
		if !r.mark[g] {
			continue
		}
		if name := r.l.Name(g); name != "" {
			out[name] = true
		}
	}
	return out
}

// A deadcodePass is the state of one reachability computation.
type deadcodePass struct {
	l       *Loader
	r       *Reachability
	queue   []Global
	ptrSize int

	// ifaceMethod holds the signatures of methods called through a
	// reachable interface, and genericIfaceMethod the names of the ones a
	// generic call site names. A concrete method that matches either one
	// is kept.
	ifaceMethod        map[methodSig]bool
	genericIfaceMethod map[string]bool

	// markable holds every method of every reachable type, waiting for
	// something to say it can be called.
	markable []methodRef

	// reflectSeen says the program looks a method up by a name the
	// compiler could not determine, so static analysis is given up and
	// every exported method of every reachable type is kept.
	reflectSeen bool
}

// Deadcode marks every symbol the program uses, from the entry point.
//
// The roots are three. The entry symbol is where the process starts.
// runtime.unreachableMethod is a root because the linker redirects a
// method it pruned to it, so it must survive whether or not anything calls
// it. The initialisation task list is a root because the whole init chain
// hangs off it, and [Loader.InitTasks] must have run first.
//
// The flood follows relocations and auxiliary entries. Three things it
// does are not a plain graph walk, and each one exists because a Go
// program can reach a method without naming it:
//
//   - a type converted to an interface is marked as such, and so are its
//     child types, because reflection can obtain an interface of a child
//     from an interface of the parent;
//   - a method of a reachable type is kept when a reachable interface
//     declares a method of the same name and signature;
//   - a function that looks a method up by a name the compiler could not
//     determine gives up the analysis, and every exported method of every
//     reachable type is kept.
//
// specs/045-linker.md's oracle for this pass is cmd/link -dumpdep on the
// same program.
func (l *Loader) Deadcode(goos, goarch string) *Reachability {
	d := &deadcodePass{
		l:                  l,
		r:                  &Reachability{l: l, mark: make([]bool, len(l.objSyms)), from: make([]Global, len(l.objSyms))},
		ptrSize:            ptrSize(goarch),
		ifaceMethod:        map[methodSig]bool{},
		genericIfaceMethod: map[string]bool{},
	}
	l.reachable = d.r.mark

	for _, name := range []string{EntrySymbol(goos, goarch), "runtime.unreachableMethod"} {
		for ver := VerABI0; ver <= VerABIInternal; ver++ {
			d.mark(l.Lookup(name, ver), 0)
		}
	}
	d.mark(l.mainInitTasks, 0)
	d.flood()

	// A method the flood found is kept once something says it can be
	// called, and keeping it can reach a new type with new methods. So the
	// two run alternately until neither finds anything.
	for {
		rem := d.markable[:0]
		for _, m := range d.markable {
			if (d.reflectSeen && m.exported()) || d.ifaceMethod[m.sig] || d.genericIfaceMethod[m.sig.name] {
				d.markMethod(m)
			} else {
				rem = append(rem, m)
			}
		}
		d.markable = rem
		if len(d.queue) == 0 {
			break
		}
		d.flood()
	}
	return d.r
}

func (d *deadcodePass) mark(g, parent Global) {
	if g <= 0 || int(g) >= len(d.r.mark) || d.r.mark[g] {
		return
	}
	d.r.mark[g] = true
	d.r.from[g] = parent
	d.r.n++
	d.queue = append(d.queue, g)
}

// markMethod keeps the three symbols one method of a type is described by.
func (d *deadcodePass) markMethod(m methodRef) {
	st, s := d.l.def(m.src)
	if s == nil {
		return
	}
	for i := 0; i < 3; i++ {
		if m.rel+i < len(s.Relocs) {
			d.mark(d.l.resolve(st, s.Relocs[m.rel+i].Sym), m.src)
		}
	}
}

func (d *deadcodePass) flood() {
	l := d.l
	for len(d.queue) > 0 {
		g := d.queue[len(d.queue)-1]
		d.queue = d.queue[:len(d.queue)-1]

		for _, t := range l.extra[g] {
			d.mark(t, g)
		}
		if syn := l.synthetic(g); syn != nil {
			for _, t := range syn.targets {
				d.mark(t, g)
			}
			continue
		}
		st, s := l.def(g)
		if s == nil {
			continue
		}
		if s.ReflectMethod() {
			d.reflectSeen = true
		}
		isType := s.GoType()
		usedInIface := isType && l.usedInIface[g]

		var methods []methodRef
		for i := 0; i < len(s.Relocs); i++ {
			rel := s.Relocs[i]
			if rel.Type&obj.R_WEAK != 0 {
				// A weak reference keeps nothing alive. The linker
				// resolves it if the target is there and leaves it zero
				// if it is not.
				continue
			}
			switch rel.Type {
			case obj.R_METHODOFF:
				// Three consecutive relocations describe one method. They
				// are kept only when the type reached an interface, and
				// only for the methods something can call.
				if usedInIface {
					methods = append(methods, methodRef{src: g, rel: i})
					// The signature is a type descriptor of its own and
					// reflection can reach further types through it, so
					// it inherits the attribute.
					d.setUsedInIface(l.resolve(st, rel.Sym), g)
				}
				i += 2
				continue
			case obj.R_USETYPE:
				// A marker for DWARF. It keeps no symbol alive.
				continue
			case obj.R_USEIFACE:
				rs := l.resolve(st, rel.Sym)
				if rd := l.Def(rs); rd != nil && rd.Itab() {
					// The marker can name an itab, and then it means the
					// type that itab is for.
					rs = l.itabType(st, rd, d.ptrSize)
				}
				d.setUsedInIface(rs, g)
				continue
			case obj.R_USEIFACEMETHOD:
				// The interface descriptor and the offset of one of its
				// methods. Every concrete method that matches is kept.
				rs := l.resolve(st, rel.Sym)
				rst, rd := l.def(rs)
				if rd != nil {
					if m, ok := l.ifaceMethod(rst, rd, rel.Add); ok {
						d.ifaceMethod[m] = true
					}
				}
				continue
			case obj.R_USENAMEDMETHOD:
				// A method looked up by a constant name. The symbol holds
				// the name and nothing else, and it is not in the binary.
				if nd := l.Def(l.resolve(st, rel.Sym)); nd != nil {
					d.genericIfaceMethod[string(nd.Data)] = true
				}
				continue
			case obj.R_INITORDER:
				// An ordering edge, already used by InitTasks. The live
				// records are the ones the schedule names.
				continue
			}
			rs := l.resolve(st, rel.Sym)
			if isType && usedInIface && rs != 0 && !l.usedInIface[rs] {
				if rd := l.Def(rs); rd != nil && rd.GoType() {
					// An interface of a child type can be obtained from
					// an interface of the parent, so the child inherits
					// the attribute and is walked again with it set.
					l.usedInIface[rs] = true
					d.unmark(rs)
				}
			}
			d.mark(rs, g)
		}

		for _, a := range s.Aux {
			if a.Type == obj.AuxGotype {
				// A symbol being reachable does not make its type
				// descriptor reachable.
				continue
			}
			d.mark(l.resolve(st, a.Sym), g)
		}

		if len(methods) > 0 {
			sigs := l.typeMethods(st, g, s, d.ptrSize)
			if len(sigs) != len(methods) {
				// The descriptor and its relocations disagree, so the
				// pass cannot say which method is which. Keeping all of
				// them is the answer that is never wrong.
				for _, m := range methods {
					d.markMethod(m)
				}
			} else {
				for i := range methods {
					methods[i].sig = sigs[i]
				}
				d.markable = append(d.markable, methods...)
			}
		}
	}
}

// unmark clears a symbol's mark so the flood visits it again. It is what
// lets a type be walked a second time with the interface attribute set,
// and a type is walked at most twice because the attribute only ever goes
// from unset to set.
func (d *deadcodePass) unmark(g Global) {
	if g > 0 && int(g) < len(d.r.mark) && d.r.mark[g] {
		d.r.mark[g] = false
		d.r.n--
	}
}

// setUsedInIface records that a type reached an interface.
//
// A type that was already walked is walked again, because its children
// inherit the attribute and the first walk did not give it to them. A type
// that was not reached is left unreached: the marker says what the type is
// and not that the program uses it, so marking it here would keep every
// type any function converts to an interface whether or not that function
// is in the binary.
func (d *deadcodePass) setUsedInIface(g, parent Global) {
	if g == 0 || d.l.usedInIface[g] {
		return
	}
	d.l.usedInIface[g] = true
	if d.r.mark[g] {
		d.unmark(g)
		d.mark(g, parent)
	}
}
