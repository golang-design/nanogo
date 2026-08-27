// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import "sort"

// runtimePkgs is the set of packages the runtime itself depends on.
//
// The linker lays their text out before everyone else's, so the list
// decides where every function in the binary lands. It is
// objabi.runtimePkgs and it is pinned the way the predeclared symbol list
// is: [TestRuntimePackagesMatchTheToolchain] compares it with the
// installed toolchain's, because one package missing from it moves the
// text of every package after it.
var runtimePkgs = map[string]bool{
	"runtime": true,

	"internal/runtime/atomic":          true,
	"internal/runtime/cgroup":          true,
	"internal/runtime/exithook":        true,
	"internal/runtime/gc":              true,
	"internal/runtime/gc/scan":         true,
	"internal/runtime/maps":            true,
	"internal/runtime/math":            true,
	"internal/runtime/sys":             true,
	"internal/runtime/syscall/linux":   true,
	"internal/runtime/syscall/windows": true,

	"internal/abi":            true,
	"internal/bytealg":        true,
	"internal/byteorder":      true,
	"internal/chacha8rand":    true,
	"internal/coverage/rtcov": true,
	"internal/cpu":            true,
	"internal/goarch":         true,
	"internal/godebugs":       true,
	"internal/goexperiment":   true,
	"internal/goos":           true,
	"internal/profilerecord":  true,
	"internal/strconv":        true,
	"internal/stringslite":    true,
}

// RuntimePackages returns the pinned list of packages the runtime depends
// on, sorted. It exists for the comparison with the toolchain.
func RuntimePackages() []string {
	out := make([]string, 0, len(runtimePkgs))
	for p := range runtimePkgs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// A Target is the layout arithmetic of one architecture.
//
// Every field is a number cmd/link reads from its own architecture table,
// and each one moves addresses on its own: an alignment that is one power
// of two out moves every symbol after the first that needed the padding.
type Target struct {
	GOOS, GOARCH string

	// Funcalign is the alignment every text symbol gets at least.
	Funcalign int64
	// MinFunc is the space a function is given even when its body is
	// shorter, because the runtime's pc to function table needs every
	// function to have an address of its own.
	MinFunc int64
	// MinLC is the length of the shortest instruction. The text section
	// is this much longer than its last symbol, so that the end marker
	// does not share an address with the next section.
	MinLC int64
	// Minalign and Maxalign bound the alignment a data symbol that
	// declared none is given.
	Minalign, Maxalign int64
	// TextAddr is the address the text section starts at.
	TextAddr uint64
	// PtrSize is the width of a pointer.
	PtrSize int64
}

// TargetFor returns the layout arithmetic of a target, or nil for one
// nanogo does not describe.
func TargetFor(goos, goarch string) *Target {
	switch goarch {
	case "arm64":
		return &Target{
			GOOS: goos, GOARCH: goarch,
			Funcalign: 16, MinFunc: 16, MinLC: 4,
			Minalign: 1, Maxalign: 32,
			TextAddr: textAddr(goos, goarch),
			PtrSize:  8,
		}
	}
	return nil
}

// textAddr is the default -T of a target.
func textAddr(goos, goarch string) uint64 {
	if goos == "darwin" {
		return 1<<32 + 0x1000
	}
	return 0x11000
}

// The names of the symbols the linker makes for the text segment. The two
// FIPS brackets are made whether or not any FIPS text is linked, so they
// take space in every binary and the end of the text section depends on
// them.
const (
	textFIPSStart = "go:textfipsstart"
	textFIPSEnd   = "go:textfipsend"
	symTextStart  = "runtime.text"
	symTextEnd    = "runtime.etext"
)

// A library is one package as the layout sees it: the objects of one
// archive, and the packages its objects name in their Autolib lists.
type library struct {
	pkg     string
	imports []int // indexes into Layout.libs, in the order the objects name them
	textp   []Global
	dupText []Global
}

// A Layout is the address every symbol was given.
//
// specs/045-linker.md's oracle for this stage is cmd/link's own layout of
// the same objects, compared symbol by symbol rather than in aggregate,
// because a size that agrees can still hold two symbols in the wrong
// order.
type Layout struct {
	l      *Loader
	target *Target

	// Textp is the text symbols in the order they are laid out.
	Textp []Global
	// TextStart and TextEnd are the addresses of runtime.text and
	// runtime.etext, and TextLength is the length of the section, which
	// is MinLC longer than the last symbol ends.
	TextStart, TextEnd uint64
	TextLength         uint64

	addr []uint64
	kind []Kind
	libs []*library
}

// Addr returns the address a symbol was given, and whether it has one.
func (a *Layout) Addr(g Global) (uint64, bool) {
	if g <= 0 || int(g) >= len(a.addr) {
		return 0, false
	}
	if a.kind[g] == Kxxx {
		return 0, false
	}
	return a.addr[g], true
}

// Kind returns the kind a symbol was laid out as, which is the linker's
// and not the object's for a symbol the linker regrouped.
func (a *Layout) Kind(g Global) Kind {
	if g <= 0 || int(g) >= len(a.kind) {
		return Kxxx
	}
	return a.kind[g]
}

// Layout assigns addresses to everything the program reaches.
//
// It takes the reachability the previous stage computed, because an
// unreachable symbol is not in the binary and takes no space. The order
// within the text segment is the whole of the work: cmd/link walks the
// packages in postorder of the import graph, the packages the runtime
// depends on first, and the addresses follow from the order and the
// alignment.
func (l *Loader) Layout(r *Reachability, t *Target) *Layout {
	a := &Layout{
		l:      l,
		target: t,
		addr:   make([]uint64, l.NSym()),
		kind:   make([]Kind, l.NSym()),
	}
	a.buildLibraries()
	a.assignText(r)
	return a
}

// buildLibraries groups the objects into packages and records the import
// edges between them.
//
// One package is one archive and holds every object of it, so the
// package's text is contiguous. The edges are each object's Autolib list,
// in order and with the repeats kept, because the order decides the
// postorder walk and a repeat does not.
func (a *Layout) buildLibraries() {
	byPkg := map[string]int{}
	for _, st := range a.l.objs {
		if _, ok := byPkg[st.obj.Pkg]; !ok {
			byPkg[st.obj.Pkg] = len(a.libs)
			a.libs = append(a.libs, &library{pkg: st.obj.Pkg})
		}
	}
	for _, st := range a.l.objs {
		lib := a.libs[byPkg[st.obj.Pkg]]
		for _, im := range st.obj.Imports {
			if i, ok := byPkg[im.Path]; ok {
				lib.imports = append(lib.imports, i)
			}
		}
	}
}

// postorder returns the libraries in the order the text is laid out in:
// depth first over the import edges, a package after everything it
// imports, starting each walk from the load order.
func (a *Layout) postorder() []int {
	const (
		unvisited = iota
		visiting
		visited
	)
	mark := make([]int, len(a.libs))
	order := make([]int, 0, len(a.libs))
	var dfs func(i int)
	dfs = func(i int) {
		if mark[i] != unvisited {
			// A cycle cannot happen in a Go import graph and a second
			// visit of a finished library is the common case, so both
			// stop here.
			return
		}
		mark[i] = visiting
		for _, j := range a.libs[i].imports {
			dfs(j)
		}
		mark[i] = visited
		order = append(order, i)
	}
	for i := range a.libs {
		dfs(i)
	}
	return order
}

// assignText orders the text symbols and gives each one an address.
func (a *Layout) assignText(r *Reachability) {
	l := a.l
	byPkg := map[string]*library{}
	for _, lib := range a.libs {
		byPkg[lib.pkg] = lib
	}

	// Split each package's text into the symbols it defines and the ones
	// that are laid out after them. The second list holds three cases: a
	// duplicate-tolerant definition, a copy of a symbol another package
	// owns, and a symbol the linker rewrote a relocation of, which
	// cmd/link copies out of its object and then no longer recognises as
	// the object's own. The first two keep the package order intact,
	// which the trampoline pass depends on.
	for _, st := range l.objs {
		lib := byPkg[st.obj.Pkg]
		for li, s := range allDefs(st.obj) {
			g := st.syms[li]
			if !r.Reachable(g) || !s.Type.IsText() {
				continue
			}
			a.kind[g] = ObjKind(s.Type)
			owner := l.objSyms[g]
			if owner.obj != st.idx || int(owner.li) != li || s.Dupok() || r.Rewritten(g) {
				lib.dupText = append(lib.dupText, g)
				continue
			}
			lib.textp = append(lib.textp, g)
		}
	}

	// The two FIPS brackets are the linker's own text symbols. They are
	// laid out before any package's, and the sort by kind below is what
	// moves them to the end, where the FIPS text of a build that has any
	// falls between them.
	var textp []Global
	for _, br := range []struct {
		name string
		kind Kind
	}{{textFIPSStart, KTEXTFIPSSTART}, {textFIPSEnd, KTEXTFIPSEND}} {
		g := l.addSynthetic(br.name, VerABI0, br.kind, 1, nil)
		for int(g) >= len(a.addr) {
			a.addr = append(a.addr, 0)
			a.kind = append(a.kind, Kxxx)
		}
		a.kind[g] = br.kind
		textp = append(textp, g)
	}

	seen := make([]bool, l.NSym())
	for _, internal := range []bool{true, false} {
		for _, i := range a.postorder() {
			lib := a.libs[i]
			if runtimePkgs[lib.pkg] != internal {
				continue
			}
			for _, list := range [][]Global{lib.textp, lib.dupText} {
				for _, g := range list {
					if !seen[g] {
						seen[g] = true
						textp = append(textp, g)
					}
				}
			}
		}
	}

	// The sort is by kind and it is stable, so it gathers the FIPS text
	// without disturbing the package order inside each kind.
	sort.SliceStable(textp, func(i, j int) bool {
		return a.kind[textp[i]] < a.kind[textp[j]]
	})
	a.Textp = textp

	t := a.target
	va := rnd(a.target.TextAddr, t.Funcalign)
	a.TextStart = va
	for _, g := range textp {
		align := int64(a.symAlign(g))
		if align < t.Funcalign {
			align = t.Funcalign
		}
		va = rnd(va, align)
		a.addr[g] = va
		size := int64(a.symSize(g))
		if size < t.MinFunc {
			size = t.MinFunc
		}
		va += uint64(size)
	}
	a.TextEnd = va
	a.TextLength = va - a.TextStart + uint64(t.MinLC)
}

// symSize is the size a symbol occupies, for a symbol an object defines
// and for one the linker made.
func (a *Layout) symSize(g Global) uint32 {
	if s := a.l.Def(g); s != nil {
		return s.Size
	}
	if syn := a.l.synthetic(g); syn != nil {
		return syn.size
	}
	return 0
}

// symAlign is the alignment an object asked for, or 0 for none.
func (a *Layout) symAlign(g Global) uint32 {
	if s := a.l.Def(g); s != nil {
		return s.Align
	}
	return 0
}

// rnd rounds up to a multiple of align, which must be a power of two.
func rnd(v uint64, align int64) uint64 {
	if align <= 1 {
		return v
	}
	m := uint64(align)
	return (v + m - 1) &^ (m - 1)
}
