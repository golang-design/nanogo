// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"fmt"

	"golang.design/x/nanogo/obj"
)

// A Global names one symbol of the whole program. It is an index into the
// loader's table and 0 is the nil symbol, which a marker relocation uses
// as its target.
type Global int32

// Symbol versions.
//
// A symbol's identity is its name and its version, and the version is what
// keeps the ABI0 entry point and the ABIInternal body of one function
// apart. specs/030-abi.md owns the distinction and specs/045-linker.md
// says why the linker must keep it: a reference with the wrong one
// resolves to nothing or to a wrapper.
const (
	VerABI0        = 0
	VerABIInternal = 1
	// verStatic is the first version handed to a file-static symbol. Each
	// object gets one of its own, and a static is never entered in the
	// name table, so two objects may define a static of the same name.
	verStatic = 2
)

// objState is the loader's state for one object.
type objState struct {
	obj *Object
	idx int   // position in Loader.objs
	ver int   // the version of this object's static symbols
	pkg []int // one entry per PkgIdx, the object that index space belongs to
	// syms maps a local index to a global one. It covers the whole local
	// index space, definitions and then references.
	syms []Global
}

// objSym is where a global symbol's definition lives.
type objSym struct {
	obj int    // index into Loader.objs
	li  uint32 // local index in that object
}

// A Loader holds the whole program's symbols.
//
// It is cmd/link's loader with the parts a reachability pass needs. The
// rules it keeps are the ones a reader is most likely to flatten: a
// package definition is unique by construction and is never merged by
// name, a content-addressable symbol is merged by its hash and is not in
// the name table at all, and a non-package definition is the only kind
// that deduplicates by name.
type Loader struct {
	objs    []*objState
	objSyms []objSym // index 0 is the nil symbol

	byName  [2]map[string]Global // one per ABI version; statics are absent
	byPkg   map[string]int       // import path to the object that holds its index space
	builtin []Global             // one per entry of builtins

	// hashed64 and hashed map a content hash to the symbol that owns it
	// and the size it had, because two symbols with one hash may differ in
	// size and the larger one wins.
	hashed64 map[[hash64Size]byte]symAndSize
	hashed   map[[HashSize]byte]symAndSize

	local       []bool // the symbol is file local
	usedInIface []bool
	reachable   []bool

	dup       []string
	undefined []undef

	// synth holds the symbols the linker makes that no object carries,
	// and extra holds the references it adds to symbols an object does
	// carry. Both exist because a linker writes into the program as well
	// as reading from it: the initialisation task list is the first case
	// and the runtime's slice header for it is the second.
	synth         []synthetic
	extra         map[Global][]Global
	mainInitTasks Global
}

// A synthetic symbol is one the linker built. It has a name, a version,
// and the symbols it refers to, and no object holds it.
type synthetic struct {
	name    string
	ver     int
	targets []Global
}

type symAndSize struct {
	sym  Global
	size uint32
}

// NewLoader returns an empty loader.
func NewLoader() *Loader {
	l := &Loader{
		objSyms:  make([]objSym, 1), // index 0 is the nil symbol
		byPkg:    map[string]int{},
		builtin:  make([]Global, len(builtins)),
		hashed64: map[[hash64Size]byte]symAndSize{},
		hashed:   map[[HashSize]byte]symAndSize{},
		extra:    map[Global][]Global{},
	}
	for i := range l.byName {
		l.byName[i] = map[string]Global{}
	}
	return l
}

// AddObject records an object. Every object must be added before [Loader.Load]
// runs, because a reference from one object to another resolves by index
// and the index space of the referenced package must already be known.
func (l *Loader) AddObject(o *Object) {
	st := &objState{obj: o, idx: len(l.objs), ver: verStatic + len(l.objs)}
	st.syms = make([]Global, o.NSym())
	l.objs = append(l.objs, st)
	// The first object of a package holds the index space its importers
	// reference. The others are the assembly objects of the same archive,
	// which have index spaces of their own that nobody references by
	// package.
	if _, ok := l.byPkg[o.Pkg]; !ok {
		l.byPkg[o.Pkg] = st.idx
	}
}

// Load builds the global symbol table.
//
// The order is the one cmd/link uses and it is not a detail: every package
// definition of every object is entered first, so a non-package definition
// that follows can see the package definition it would otherwise overwrite.
func (l *Loader) Load() error {
	for _, st := range l.objs {
		l.preload(st, spacePkg)
	}
	for _, st := range l.objs {
		l.preload(st, spaceHashed64)
		l.preload(st, spaceHashed)
		l.preload(st, spaceNonPkg)
	}
	for _, st := range l.objs {
		if err := l.loadRefs(st); err != nil {
			return err
		}
	}
	l.local = make([]bool, len(l.objSyms))
	l.usedInIface = make([]bool, len(l.objSyms))
	for _, st := range l.objs {
		for i, s := range allDefs(st.obj) {
			g := st.syms[i]
			if s.Local() {
				l.local[g] = true
			}
			if s.UsedInIface() {
				l.usedInIface[g] = true
			}
		}
		// A reference carries the flags of a definition another object
		// holds, because the linker needs them before it has read that
		// object.
		for _, rf := range st.obj.RefFlags {
			if rf.Flag2&obj.SymFlagUsedInIface != 0 {
				if g := l.resolve(st, rf.Sym); g != 0 {
					l.usedInIface[g] = true
				}
			}
		}
	}
	return nil
}

// The four definition spaces, in the order the local index space lists
// them.
const (
	spacePkg = iota
	spaceHashed64
	spaceHashed
	spaceNonPkg
)

// allDefs returns every defined symbol of an object, in local index order.
func allDefs(o *Object) []*Sym {
	out := make([]*Sym, 0, o.NDef())
	out = append(out, o.Defs...)
	out = append(out, o.Hashed64Defs...)
	out = append(out, o.HashedDefs...)
	out = append(out, o.NonPkgDefs...)
	return out
}

// preload enters one definition space of one object.
func (l *Loader) preload(st *objState, space int) {
	o := st.obj
	var list []*Sym
	var start int
	switch space {
	case spacePkg:
		list, start = o.Defs, 0
	case spaceHashed64:
		list, start = o.Hashed64Defs, len(o.Defs)
	case spaceHashed:
		list, start = o.HashedDefs, len(o.Defs)+len(o.Hashed64Defs)
	case spaceNonPkg:
		list, start = o.NonPkgDefs, len(o.Defs)+len(o.Hashed64Defs)+len(o.HashedDefs)
	}
	for i, s := range list {
		li := start + i
		g := l.addSym(st, s, uint32(li), space, i)
		st.syms[li] = g
		if space == spacePkg || space == spaceNonPkg {
			l.registerBuiltin(st, s, g)
		}
	}
}

// registerBuiltin records where a predeclared runtime symbol is defined.
// A reference to one carries an index into the pinned list and no name, so
// without this the reference resolves to nothing.
func (l *Loader) registerBuiltin(st *objState, s *Sym, g Global) {
	if !hasPrefix(s.Name, "runtime.") && !(st.obj.Pkg == "runtime" && hasPrefix(s.Name, "type:")) {
		return
	}
	if i := builtinIndex(s.Name, s.ABI); i >= 0 {
		l.builtin[i] = g
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// addSym enters one definition and returns its global index.
//
// hashIdx is the position in the hash block for a content-addressable
// symbol, which is where its identity is stored.
func (l *Loader) addSym(st *objState, s *Sym, li uint32, space, hashIdx int) Global {
	g := Global(len(l.objSyms))
	add := func() { l.objSyms = append(l.objSyms, objSym{st.idx, li}) }

	switch space {
	case spaceHashed64:
		h := st.obj.Hash64[hashIdx]
		if old, ok := l.hashed64[h]; ok {
			// Two symbols with one hash may differ in size, because a
			// short symbol's identity is its eight bytes and trailing
			// zeros do not change them. The larger one wins, so a
			// reference through the smaller one still reads bytes that
			// are there.
			if s.Size > old.size {
				l.objSyms[old.sym] = objSym{st.idx, li}
				l.hashed64[h] = symAndSize{old.sym, s.Size}
			}
			return old.sym
		}
		l.hashed64[h] = symAndSize{g, s.Size}
		add()
		return g
	case spaceHashed:
		h := st.obj.Hash[hashIdx]
		if old, ok := l.hashed[h]; ok {
			if s.Size > old.size {
				l.objSyms[old.sym] = objSym{st.idx, li}
				l.hashed[h] = symAndSize{old.sym, s.Size}
			}
			return old.sym
		}
		l.hashed[h] = symAndSize{g, s.Size}
		add()
		return g
	}

	// An anonymous payload is a symbol nothing names, so it is in the
	// table by index and not by name.
	if s.Name == "" {
		add()
		return g
	}
	ver := l.version(st, s.ABI)
	if ver >= verStatic {
		// A file-static symbol cannot be referenced by name, so it is
		// never entered in the name table and two files may define one of
		// the same name.
		add()
		return g
	}

	if space == spacePkg {
		// A package definition is unique by construction. cmd/link enters
		// it in the name table, because a linkname directive may still
		// name it, and never merges it with the copy that is there.
		// obj.Package.check guards the write side of the same rule.
		l.byName[ver][s.Name] = g
		add()
		return g
	}

	// A non-package definition is the only kind that deduplicates by name.
	old, existed := l.byName[ver][s.Name]
	if !existed {
		l.byName[ver][s.Name] = g
		add()
		return g
	}
	oldSt, oldSym := l.def(old)
	switch {
	case s.Dupok():
		// Two duplicate-tolerant definitions of one name are one symbol,
		// and the larger one wins: issue 47185 in cmd/link.
		if oldSym.Dupok() && oldSym.Size < s.Size {
			l.objSyms[old] = objSym{st.idx, li}
		}
	case oldSym.Dupok():
		l.objSyms[old] = objSym{st.idx, li}
	default:
		// One of the two has content and the other is zero filled, or one
		// is text. The rules are cmd/link's table in addSym.
		newIsText := s.Type.IsText()
		oldIsBSS := isData(oldSym.Type) && len(oldSym.Data) == 0
		newIsBSS := isData(s.Type) && len(s.Data) == 0
		switch {
		case newIsText && oldIsBSS,
			len(s.Data) != 0 && oldIsBSS,
			newIsBSS && oldIsBSS && s.Size > oldSym.Size:
			l.objSyms[old] = objSym{st.idx, li}
		case newIsBSS:
			// The old definition wins.
		default:
			// Two definitions with content. cmd/link stops here and so
			// does this, because keeping either one is a program whose
			// symbol holds bytes from a package that did not write them.
			l.dup = append(l.dup, fmt.Sprintf("%s: defined by %s and by %s",
				s.Name, oldSt.obj.Name, st.obj.Name))
		}
	}
	return old
}

// isData reports whether a kind occupies data rather than text or debug
// information. It is what separates a zero-filled definition from one with
// content in the duplicate rules.
func isData(k obj.SymKind) bool {
	switch k {
	case obj.SRODATA, obj.SRODATAFIPS, obj.SNOPTRDATA, obj.SNOPTRDATAFIPS,
		obj.SDATA, obj.SDATAFIPS, obj.SBSS, obj.SNOPTRBSS, obj.STLSBSS:
		return true
	}
	return false
}

// loadRefs resolves an object's references: the non-package references by
// name, and the referenced packages by path.
func (l *Loader) loadRefs(st *objState) error {
	o := st.obj
	base := o.NDef()
	for i, s := range o.NonPkgRefs {
		st.syms[base+i] = l.lookupOrCreate(s.Name, l.version(st, s.ABI))
	}
	st.pkg = make([]int, len(o.Pkglist))
	for i := 1; i < len(o.Pkglist); i++ {
		idx, ok := l.byPkg[o.Pkglist[i]]
		if !ok {
			return errAt(o.Name, "references package %q, which is not in the link", o.Pkglist[i])
		}
		st.pkg[i] = idx
	}
	return nil
}

// lookupOrCreate returns the symbol of a name and version, creating an
// entry with no definition when nothing defines it. An undefined symbol is
// how a reference to a package outside the link survives to the stage that
// reports it.
func (l *Loader) lookupOrCreate(name string, ver int) Global {
	if ver < verStatic {
		if g, ok := l.byName[ver][name]; ok {
			return g
		}
	}
	g := Global(len(l.objSyms))
	l.objSyms = append(l.objSyms, objSym{-1, 0})
	if ver < verStatic {
		l.byName[ver][name] = g
	}
	l.undefined = append(l.undefined, undef{g, name, ver})
	return g
}

type undef struct {
	sym  Global
	name string
	ver  int
}

// version is the version a symbol of this ABI has in this object.
func (l *Loader) version(st *objState, abi uint16) int {
	switch abi {
	case obj.ABIStatic:
		return st.ver
	case obj.ABI0:
		return VerABI0
	case obj.ABIInternal:
		return VerABIInternal
	}
	return VerABI0
}

// resolve turns a reference held by an object into a global symbol.
func (l *Loader) resolve(st *objState, r obj.SymRef) Global {
	o := st.obj
	switch r.PkgIdx {
	case obj.PkgIdxInvalid:
		return 0
	case obj.PkgIdxHashed64:
		return st.syms[len(o.Defs)+int(r.SymIdx)]
	case obj.PkgIdxHashed:
		return st.syms[len(o.Defs)+len(o.Hashed64Defs)+int(r.SymIdx)]
	case obj.PkgIdxNone:
		return st.syms[len(o.Defs)+len(o.Hashed64Defs)+len(o.HashedDefs)+int(r.SymIdx)]
	case obj.PkgIdxBuiltin:
		if int(r.SymIdx) < len(l.builtin) {
			return l.builtin[r.SymIdx]
		}
		return 0
	case obj.PkgIdxSelf:
		return st.syms[r.SymIdx]
	}
	other := l.objs[st.pkg[r.PkgIdx]]
	if int(r.SymIdx) >= len(other.obj.Defs) {
		return 0
	}
	return other.syms[r.SymIdx]
}

// Resolve turns a reference held by one object into a global symbol. The
// object must be one the loader holds.
func (l *Loader) Resolve(o *Object, r obj.SymRef) Global {
	for _, st := range l.objs {
		if st.obj == o {
			return l.resolve(st, r)
		}
	}
	return 0
}

// objSynthetic marks an entry of objSyms whose definition is a symbol the
// linker built rather than one an object holds.
const objSynthetic = -2

// addSynthetic records a symbol the linker built and returns it. A name
// that already exists is reused, so a second call replaces the targets
// rather than adding a second symbol of one name.
func (l *Loader) addSynthetic(name string, ver int, targets []Global) Global {
	g := Global(len(l.objSyms))
	l.objSyms = append(l.objSyms, objSym{objSynthetic, uint32(len(l.synth))})
	l.synth = append(l.synth, synthetic{name: name, ver: ver, targets: targets})
	if ver < len(l.byName) {
		l.byName[ver][name] = g
	}
	return g
}

// synthetic returns the linker-built symbol at g, or nil.
func (l *Loader) synthetic(g Global) *synthetic {
	if g <= 0 || int(g) >= len(l.objSyms) {
		return nil
	}
	if os := l.objSyms[g]; os.obj == objSynthetic {
		return &l.synth[os.li]
	}
	return nil
}

// def returns where a global symbol is defined, or nil for one nothing
// defines.
func (l *Loader) def(g Global) (*objState, *Sym) {
	if g <= 0 || int(g) >= len(l.objSyms) {
		return nil, nil
	}
	os := l.objSyms[g]
	if os.obj < 0 {
		return nil, nil // undefined, or a symbol the linker built
	}
	st := l.objs[os.obj]
	return st, st.obj.Local(int(os.li))
}

// NSym is the number of symbols, including the nil symbol at index 0.
func (l *Loader) NSym() int { return len(l.objSyms) }

// Def returns the definition of a symbol, or nil when nothing in the link
// defines it.
func (l *Loader) Def(g Global) *Sym {
	_, s := l.def(g)
	return s
}

// Owner returns the object a symbol is defined in, or nil.
func (l *Loader) Owner(g Global) *Object {
	st, _ := l.def(g)
	if st == nil {
		return nil
	}
	return st.obj
}

// Name returns a symbol's name. An undefined symbol keeps the name the
// reference used, and an anonymous payload has none.
func (l *Loader) Name(g Global) string {
	if s := l.Def(g); s != nil {
		return s.Name
	}
	if syn := l.synthetic(g); syn != nil {
		return syn.name
	}
	for _, u := range l.undefined {
		if u.sym == g {
			return u.name
		}
	}
	return ""
}

// Lookup returns the symbol of a name and version, or 0.
func (l *Loader) Lookup(name string, ver int) Global {
	if ver < 0 || ver >= len(l.byName) {
		return 0
	}
	return l.byName[ver][name]
}

// Duplicates lists the definitions the loader could not merge. Two
// definitions of one name that both have content are a build whose symbol
// would hold bytes from a package that did not write them, and cmd/link
// stops on it.
func (l *Loader) Duplicates() []string { return l.dup }

// Undefined lists the names nothing in the link defines.
func (l *Loader) Undefined() []string {
	var out []string
	for _, u := range l.undefined {
		if l.Def(u.sym) == nil {
			out = append(out, u.name)
		}
	}
	return out
}

// UsedInIface reports whether a type was converted to an interface
// somewhere the program reaches. It is what decides whether the methods of
// the type can be called without a direct call, and the reachability pass
// sets it as it walks.
func (l *Loader) UsedInIface(g Global) bool {
	return g > 0 && int(g) < len(l.usedInIface) && l.usedInIface[g]
}
