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

	// Sections holds the output sections this stage lays out, in the
	// order the linker writes them.
	Sections []*Section

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
	for g := Global(1); g < Global(l.NSym()); g++ {
		if s := l.Def(g); s != nil && r.Reachable(g) && !s.Type.IsText() {
			a.kind[g] = a.group(g, s)
		}
	}
	a.assignData(r)
	return a
}

// Section returns the output section of a name, or nil.
func (a *Layout) Section(name string) *Section {
	for _, s := range a.Sections {
		if s.Name == name {
			return s
		}
	}
	return nil
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

// A Section is one output section: the symbols in it, in order, and the
// space they take.
type Section struct {
	Name   string
	Align  int64
	Length uint64
	Syms   []Global
}

// The names the linker groups read-only symbols by. A symbol whose name
// carries one of these prefixes is laid out with its group and not with
// the read-only data it was compiled as, so that the runtime can find
// every member of the group in one address range.
const (
	prefixGoString = "go:string."
	prefixGCBits   = "runtime.gcbits."
	prefixType     = "type:"
	prefixGCMask   = "type:.gcmask."
	suffixFuncDesc = "·f"
)

// group is the kind the output lays a symbol out as.
//
// The object records read-only data for five different things, and the
// linker separates them by name: the string constants, the garbage
// collection bit masks, the function descriptors, the type descriptors
// and the itabs. cmd/link's symtab does this before dodata, and dodata
// then walks the kinds in order, so the name is what decides which
// section a symbol lands in.
func (a *Layout) group(g Global, s *Sym) Kind {
	k := ObjKind(s.Type)
	if !topLevel(s.Name, k) {
		// A symbol with no name is a payload another symbol carries, and
		// the carrier is what the section holds. Only the debugging
		// kinds and the function descriptors have anonymous symbols the
		// linker lays out on their own.
		return Kxxx
	}
	if s.Name == symModuleData {
		// The module descriptor is the structure the runtime walks at
		// startup to find everything else, and the linker gives it a
		// section of its own rather than leaving it in the zero filled
		// data the compiler put it in.
		return KMODULEDATA
	}
	if k == KNOPTRBSS && hasPrefix(s.Name, prefixGCMask) {
		return KGCMASK
	}
	if k != KRODATA {
		return k
	}
	switch {
	case hasPrefix(s.Name, prefixGoString):
		return KGOSTRING
	case hasPrefix(s.Name, prefixGCBits):
		return KGCBITS
	case hasSuffix(s.Name, suffixFuncDesc):
		return KGOFUNC
	case hasPrefix(s.Name, prefixType), s.Itab():
		return KTYPE
	}
	return k
}

func hasSuffix(s, p string) bool { return len(s) >= len(p) && s[len(s)-len(p):] == p }

// symModuleData is the module descriptor of the program.
const symModuleData = "runtime.firstmoduledata"

// topLevel reports whether a symbol is laid out on its own.
//
// A symbol with no name is a part of another symbol, and the carrier is
// what takes the space, so counting both would count its bytes twice.
// The exceptions are the debugging kinds and the function descriptors,
// where the anonymous symbol is what the section holds.
func topLevel(name string, k Kind) bool {
	if name != "" {
		return true
	}
	switch k {
	case KDWARFFCN, KDWARFABSFCN, KDWARFTYPE, KDWARFCONST, KDWARFCUINFO,
		KDWARFRANGE, KDWARFLOC, KDWARFLINES, KGOFUNC:
		return true
	}
	return false
}

// dataAlign is the alignment a data symbol is laid out at.
//
// An object that asked for one gets it, subject to the target's minimum.
// One that asked for none is aligned by its size, up to the target's
// maximum, which is cmd/link's symalign: a symbol large enough for the
// maximum gets the maximum, and a smaller one gets the largest power of
// two that is not larger than it.
func (a *Layout) dataAlign(g Global) int64 {
	t := a.target
	if align := int64(a.symAlign(g)); align >= t.Minalign {
		return align
	} else if align != 0 {
		return t.Minalign
	}
	align := t.Maxalign
	size := int64(a.symSize(g))
	for align > size && align > t.Minalign {
		align >>= 1
	}
	return align
}

// The sections this stage lays out, and the ones it does not.
//
// A section is laid out here when every symbol in it comes out of an
// object, because then the two linkers have the same members and the
// size is a number they must agree on. Five are in that position, and
// what the linker adds to each of them has size zero:
//
//	.go.module   the module descriptor, in a section of its own
//	.noptrbss    the zero filled data that holds no pointer
//	.go.type     the type descriptors and the itabs
//	.go.func     the function descriptors
//
// The rest are not, and the reason is never that the arithmetic is
// unknown. .rodata holds the garbage collection data for the globals and
// the typelink and itablink tables, .gopclntab holds a structure of its
// own, and .noptrdata and .data hold the FIPS brackets, none of which is
// built. .bss is the one that is blocked on an input rather than on a
// stage: the linker fills in three string variables that the compiler
// left zero filled, which moves them from .bss to .data, and one of the
// three is named by the import configuration rather than by the objects.
// cmd/link also breaks a tie between two symbols of one size by its own
// symbol numbering, which counts the symbols the unbuilt stages create,
// so two symbols of one size can be laid out in an order this package
// cannot predict. Within a size class that changes no total, because the
// sizes are equal, but it does change an address.
// specs/045-linker.md records the boundary.
func (a *Layout) assignData(r *Reachability) {
	buckets := make([][]Global, numKind)
	for g := Global(1); g < Global(len(a.kind)); g++ {
		k := a.kind[g]
		if k == Kxxx || k.IsText() || !r.Reachable(g) {
			continue
		}
		buckets[k] = append(buckets[k], g)
	}
	for k := range buckets {
		a.sortData(Kind(k), buckets[k])
	}

	// The module descriptor is one symbol in a section of its own, which
	// is why it is not part of the zero filled data it was compiled as.
	for _, g := range buckets[KMODULEDATA] {
		a.addSection(&Section{
			Name:   ".go.module",
			Align:  a.dataAlign(g),
			Length: uint64(a.symSize(g)),
			Syms:   []Global{g},
		})
	}
	a.addSection(a.dataSection(".noptrbss", 0, buckets, KNOPTRBSS, KGCMASK, KCOVERAGE_COUNTER))
	// The type descriptors start one pointer into their section, so that
	// no type reference has offset zero.
	a.addSection(a.dataSection(".go.type", a.target.PtrSize, buckets, KTYPE))
	a.addSection(a.dataSection(".go.func", 0, buckets, KGOFUNC))
}

func (a *Layout) addSection(s *Section) {
	if s != nil {
		a.Sections = append(a.Sections, s)
	}
}

// dataSection lays out one section.
//
// The section starts at a multiple of the alignment its first kind asks
// for, so the offsets it computes are the addresses modulo that multiple
// and its length does not depend on what the segment holds in front of
// it. skip is the space the linker leaves before the first symbol.
func (a *Layout) dataSection(name string, skip int64, buckets [][]Global, kinds ...Kind) *Section {
	sect := &Section{Name: name, Align: a.target.Minalign}
	for _, g := range buckets[kinds[0]] {
		if al := a.dataAlign(g); al > sect.Align {
			sect.Align = al
		}
	}
	size := skip
	for _, k := range kinds {
		for _, g := range buckets[k] {
			size = rndInt(size, a.dataAlign(g))
			a.addr[g] = uint64(size)
			size += int64(a.symSize(g))
			sect.Syms = append(sect.Syms, g)
		}
	}
	sect.Length = uint64(size)
	return sect
}

// sortData puts one kind of data symbol in the order the linker lays it
// out.
//
// The order is by size, so that the symbols an alignment forces a gap in
// front of are gathered rather than spread through the section, and ties
// are broken by the symbol's own number. That number is the linker's and
// not the object's, so two symbols of one size may be laid out in an
// order this package cannot predict, which changes an address and not a
// total.
//
// runtime.zerobase is placed after the last symbol of size zero, so that
// every zero sized symbol has its address. The type descriptors have an
// order of their own, in [Layout.sortTypes].
func (a *Layout) sortData(k Kind, syms []Global) {
	if k == KTYPE {
		a.sortTypes(syms)
		return
	}
	zerobase := a.l.Lookup("runtime.zerobase", VerABI0)
	sort.SliceStable(syms, func(i, j int) bool {
		si, sj := syms[i], syms[j]
		isz, jsz := a.symSize(si), a.symSize(sj)
		switch {
		case si == zerobase:
			return jsz != 0
		case sj == zerobase:
			return isz == 0
		case isz != jsz:
			return isz < jsz
		}
		return si < sj
	})
}

// sortTypes puts the type descriptors in the order the linker writes
// them.
//
// The descriptors a typelink names come first and in order of the string
// the type calls itself, which is what lets the reflect package rely on
// one descriptor per type string. The rest of the descriptors follow by
// size, and the itabs come last, so that the module descriptor can name
// the three ranges by their bounds alone.
func (a *Layout) sortTypes(syms []Global) {
	l := a.l
	type key struct {
		itab     bool
		typelink bool
		str      string
		size     uint32
	}
	keys := make(map[Global]key, len(syms))
	for _, g := range syms {
		st, s := l.def(g)
		k := key{itab: s.Itab(), size: s.Size}
		if !k.itab {
			k.typelink = s.Typelink()
			if k.typelink {
				k.str = l.typeStr(st, s, int(a.target.PtrSize))
			}
		}
		keys[g] = k
	}
	sort.SliceStable(syms, func(i, j int) bool {
		ki, kj := keys[syms[i]], keys[syms[j]]
		if ki.itab != kj.itab {
			return kj.itab
		}
		if !ki.itab {
			if ki.typelink != kj.typelink {
				return ki.typelink
			}
			if ki.typelink {
				return ki.str < kj.str
			}
		}
		if ki.size != kj.size {
			return ki.size < kj.size
		}
		return syms[i] < syms[j]
	})
}

// rndInt rounds a size up to a multiple of align.
func rndInt(v, align int64) int64 {
	if align <= 1 {
		return v
	}
	return (v + align - 1) &^ (align - 1)
}
