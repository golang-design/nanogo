// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"encoding/binary"

	"golang.design/x/nanogo/obj"
)

// A Pcln is the pclntab of a program.
//
// It is what every traceback, every panic message, every runtime.Caller
// and every garbage collection stack scan reads, and it is the one stage
// whose failure is quiet: a wrong table is a program that runs correctly
// until something asks where it is, and then answers with another
// function's name or another frame's stack map. specs/045-linker.md
// makes that the reason the oracle is cmd/link's own table for the same
// objects, compared table by table, and not a program that runs.
//
// The tables are laid out in the order the fields are declared, each one
// a symbol of its own that the section carries, and the header holds the
// offset of each from its own start.
type Pcln struct {
	// Funcs is the functions the table describes, in the order the text
	// is laid out. cmd/link calls this Textp filtered by emitPcln, and
	// for an internal link of Go objects the filter keeps everything,
	// the two FIPS brackets included.
	Funcs []Global

	// FuncNameTab is the null terminated name of every function the
	// table describes and of every function inlined into one of them.
	FuncNameTab []byte

	// CuTab is one entry per file index of every compilation unit the
	// table describes, holding the offset of that file's name in
	// FileTab. A _func record names a file by an index into its own
	// unit's run of this table, so the two hops are what let a function
	// name a file with one small number.
	CuTab []byte

	// FileTab is the null terminated name of every file the table names.
	FileTab []byte

	// PcTab is every pc-value table of every function, concatenated and
	// deduplicated.
	PcTab []byte

	// funcName is the offset of each name in FuncNameTab. A _func record
	// names its function by this offset.
	funcName map[Global]uint32
	// cuOffset is where each compilation unit's run of CuTab starts,
	// counted in entries, indexed by the unit's position in the table.
	cuOffset []uint32
	// cuIndex is the position of an object's compilation unit in the
	// table, by the object's own index.
	cuIndex map[int]int
	// nfiles is how many names FileTab holds, which the header states.
	nfiles int

	// info is the AuxFuncInfo record of each function, decoded once
	// because every table below reads it.
	info map[Global]funcInfo
	// pcOffset is where each pc-value table's bytes start in PcTab. A
	// table with no bytes is at zero, which the runtime reads as no
	// table.
	pcOffset map[Global]uint32
}

// generatedAlign is the boundary every table the linker generates is
// rounded up to.
//
// cmd/link rounds in addGeneratedSym, before it records the size, so the
// padding is part of the table rather than a gap in front of the next
// one. A table compared without it is a table compared one alignment
// short at the end.
func (a *Layout) generatedAlign(n int64) int64 { return rndInt(n, a.target.PtrSize) }

// Pclntab builds the pclntab of the program.
func (a *Layout) Pclntab() *Pcln {
	p := &Pcln{
		Funcs:    a.Textp,
		funcName: map[Global]uint32{},
		info:     map[Global]funcInfo{},
		pcOffset: map[Global]uint32{},
	}
	for _, g := range p.Funcs {
		if fi, ok := a.l.funcInfo(g); ok {
			p.info[g] = fi
		}
	}
	a.buildFuncNameTab(p)
	a.buildFileTabs(p)
	a.buildPctab(p)
	return p
}

// buildPctab writes the table of pc-value tables.
//
// Every pc-value table a function carries is an auxiliary symbol of its
// own, and the linker concatenates them into one and gives each function
// the offset of each of its tables. The offsets deduplicate: two
// functions with the same table share one copy, which is most of what
// makes the table as small as it is.
//
// The first byte is padding and belongs to no table, because an offset
// of zero is how the runtime says a function has no table of that kind.
// A table with no bytes gets that offset for the same reason.
func (a *Layout) buildPctab(p *Pcln) {
	l := a.l
	size := int64(1)
	var order []Global
	save := func(g Global) {
		if _, seen := p.pcOffset[g]; seen {
			return
		}
		n := int64(a.symSize(g))
		if n == 0 {
			p.pcOffset[g] = 0
		} else {
			p.pcOffset[g] = uint32(size)
			size += n
		}
		order = append(order, g)
	}
	for _, g := range p.Funcs {
		fi, ok := p.info[g]
		if !ok {
			continue
		}
		save(fi.pcsp)
		save(fi.pcfile)
		save(fi.pcline)
		for _, d := range fi.pcdata {
			save(d)
		}
		if len(fi.inlTree) > 0 {
			save(fi.pcinline)
		}
	}
	tab := make([]byte, a.generatedAlign(size))
	for _, g := range order {
		if s := l.Def(g); s != nil {
			copy(tab[p.pcOffset[g]:], s.Data)
		}
	}
	p.PcTab = tab
}

// buildFileTabs writes the file name table and the compilation unit
// table in front of it.
//
// A function names a file by an index into its own compilation unit's
// file list, which is a small number and is the only thing the pc to
// file table can afford to hold. The linker turns that into one index
// per unit into a table of unique names: CuTab holds a run per unit, and
// entry j of a unit's run is where the name of that unit's file j starts
// in FileTab. So a lookup is the unit's offset, plus the function's file
// index, then the name at the offset that entry holds.
//
// A unit's run covers every index from zero to the largest one any of
// its functions used, and an index in that range that no function used
// gets the invalid offset, because the dead code pass may have dropped
// the only function that named the file.
func (a *Layout) buildFileTabs(p *Pcln) {
	l := a.l
	p.cuIndex = map[int]int{}
	var units []int
	for _, g := range p.Funcs {
		o, ok := l.compilationUnit(g)
		if !ok {
			continue
		}
		if _, seen := p.cuIndex[o]; !seen {
			p.cuIndex[o] = len(units)
			units = append(units, o)
		}
	}

	// Walk the file indexes every function names, in the pc to file
	// table and in the inline tree, and record the name of each one once.
	offsets := map[string]uint32{}
	entries := make([]uint32, len(units))
	var size int64
	visit := func(o int, i uint32) {
		files := l.objs[o].obj.Files
		if int(i) >= len(files) {
			return
		}
		if _, ok := offsets[files[i]]; !ok {
			offsets[files[i]] = uint32(size)
			size += int64(len(a.expandFile(files[i]))) + 1
		}
		if u := p.cuIndex[o]; entries[u] < i+1 {
			entries[u] = i + 1
		}
	}
	for _, g := range p.Funcs {
		fi, ok := p.info[g]
		if !ok {
			continue
		}
		o, ok := l.compilationUnit(g)
		if !ok {
			continue
		}
		for _, f := range fi.files {
			visit(o, f)
		}
		for _, n := range fi.inlTree {
			visit(o, n.file)
		}
	}

	p.cuOffset = make([]uint32, len(units))
	total := uint32(0)
	for u, n := range entries {
		p.cuOffset[u] = total
		total += n
	}
	cutab := make([]byte, a.generatedAlign(int64(total)*4))
	for u, o := range units {
		files := l.objs[o].obj.Files
		for j := uint32(0); j < entries[u]; j++ {
			off := ^uint32(0)
			if int(j) < len(files) {
				if got, ok := offsets[files[j]]; ok {
					off = got
				}
			}
			binary.LittleEndian.PutUint32(cutab[4*(p.cuOffset[u]+j):], off)
		}
	}
	p.CuTab = cutab

	filetab := make([]byte, a.generatedAlign(size))
	for name, off := range offsets {
		copy(filetab[off:], a.expandFile(name))
	}
	p.FileTab = filetab
	p.nfiles = len(offsets)
}

// compilationUnit returns the object a function was compiled in, which
// is the compilation unit its file indexes are counted against, and
// whether it has one. A text symbol the linker made rather than compiled
// has none.
func (l *Loader) compilationUnit(g Global) (int, bool) {
	st, _ := l.def(g)
	if st == nil {
		return 0, false
	}
	return st.idx, true
}

// filePrefix is what the compiler puts in front of a file name in the
// object, left from the days when a file was a symbol of its own.
const filePrefix = "gofile.."

// gorootPlaceholder is what the compiler writes in place of the root the
// toolchain was installed under, so that an object compiled from the
// standard library is the same wherever the toolchain is.
const gorootPlaceholder = "$GOROOT"

// expandFile is the name a file has in the table.
//
// The prefix the object carries comes off, and the placeholder the
// compiler wrote for the toolchain root is replaced by the root this
// link was given. A link with no root leaves the placeholder, which is
// what cmd/link does for a build that asked for no path in the binary.
func (a *Layout) expandFile(name string) string {
	name = trimPrefix(name, filePrefix)
	root := a.l.goroot
	if root == "" || !hasPrefix(name, gorootPlaceholder) {
		return name
	}
	rest := name[len(gorootPlaceholder):]
	if rest == "" || (rest[0] != '/' && rest[0] != '\\') {
		return name
	}
	return root + rest
}

func trimPrefix(s, p string) string {
	if hasPrefix(s, p) {
		return s[len(p):]
	}
	return s
}

// buildFuncNameTab writes the function name table.
//
// The table holds one null terminated name per function the table
// describes, and then the name of every function inlined into one of
// them. An inlined callee has no text of its own, so its name reaches
// the binary through this table alone, and a traceback that reports an
// inlined frame reads it from here.
//
// The walk visits each function and then the callees of its inline tree,
// so the order is the text order with each function's inlined callees
// behind it, and a name is written once however many times it is
// reached.
func (a *Layout) buildFuncNameTab(p *Pcln) {
	var tab []byte
	add := func(g Global) {
		if _, seen := p.funcName[g]; seen {
			return
		}
		p.funcName[g] = uint32(len(tab))
		tab = append(tab, a.l.Name(g)...)
		tab = append(tab, 0)
	}
	for _, g := range p.Funcs {
		add(g)
		for _, node := range p.info[g].inlTree {
			add(node.fn)
		}
	}
	p.FuncNameTab = append(tab, make([]byte, a.generatedAlign(int64(len(tab)))-int64(len(tab)))...)
}

// FuncNameOffset returns where a function's name starts in
// [Pcln.FuncNameTab], and whether the table holds it.
func (p *Pcln) FuncNameOffset(g Global) (uint32, bool) {
	off, ok := p.funcName[g]
	return off, ok
}

// A funcInfo is the AuxFuncInfo record a function symbol carries.
//
// specs/040-object-format.md owns the encoding. The record holds the
// frame and argument sizes, the identity bytes the runtime treats some
// functions by, the line the function starts on, the file table indexes
// its pc-value tables refer to, and the inline tree.
type funcInfo struct {
	args, locals     uint32
	funcID, funcFlag uint8
	startLine        int32
	files            []uint32
	inlTree          []inlNode

	// The pc-value tables are auxiliary symbols of the function and not
	// contents of the record. specs/040-object-format.md owns that
	// correction and this field is what it means here.
	pcsp, pcfile, pcline, pcinline Global
	pcdata                         []Global
}

// An inlNode is one call the compiler inlined into a function.
type inlNode struct {
	parent   int32
	file     uint32
	line     int32
	fn       Global
	parentPC int32
}

// The fixed part of an AuxFuncInfo record, and the width of one inline
// tree node. Both are the writer's, so a reader that guessed either one
// would read the file table out of the middle of a node.
const (
	funcInfoFixed  = 20 // through the file count
	inlTreeNodeLen = 24
)

// funcInfo decodes the record a function symbol carries, or returns
// false when the symbol carries none.
//
// A text symbol with no record belongs to no compilation unit, which is
// the case for a symbol the linker made rather than compiled: the two
// FIPS brackets are in the text and have no function to describe.
func (l *Loader) funcInfo(g Global) (funcInfo, bool) {
	st, s := l.def(g)
	if s == nil {
		return funcInfo{}, false
	}
	for _, au := range s.Aux {
		if au.Type != obj.AuxFuncInfo {
			continue
		}
		rec := l.Def(l.resolve(st, au.Sym))
		if rec == nil || len(rec.Data) < funcInfoFixed {
			return funcInfo{}, false
		}
		fi, ok := decodeFuncInfo(l, st, rec.Data)
		if !ok {
			return funcInfo{}, false
		}
		for _, pc := range s.Aux {
			switch pc.Type {
			case obj.AuxPcsp:
				fi.pcsp = l.resolve(st, pc.Sym)
			case obj.AuxPcfile:
				fi.pcfile = l.resolve(st, pc.Sym)
			case obj.AuxPcline:
				fi.pcline = l.resolve(st, pc.Sym)
			case obj.AuxPcinline:
				fi.pcinline = l.resolve(st, pc.Sym)
			case obj.AuxPcdata:
				fi.pcdata = append(fi.pcdata, l.resolve(st, pc.Sym))
			}
		}
		return fi, true
	}
	return funcInfo{}, false
}

// decodeFuncInfo reads one AuxFuncInfo record.
//
// The record is self describing: a count in front of the file table and
// another in front of the inline tree. A record that ends inside either
// one is a record this cannot read, and a partly decoded record is worse
// than none, so it is refused whole.
func decodeFuncInfo(l *Loader, st *objState, b []byte) (funcInfo, bool) {
	u32 := binary.LittleEndian.Uint32
	fi := funcInfo{
		args:      u32(b),
		locals:    u32(b[4:]),
		funcID:    b[8],
		funcFlag:  b[9],
		startLine: int32(u32(b[12:])),
	}
	nfile := u32(b[16:])
	off := funcInfoFixed + 4*int64(nfile)
	if off+4 > int64(len(b)) {
		return funcInfo{}, false
	}
	fi.files = make([]uint32, nfile)
	for i := range fi.files {
		fi.files[i] = u32(b[funcInfoFixed+4*i:])
	}
	ninl := u32(b[off:])
	off += 4
	if off+inlTreeNodeLen*int64(ninl) > int64(len(b)) {
		return funcInfo{}, false
	}
	fi.inlTree = make([]inlNode, ninl)
	for i := range fi.inlTree {
		n := b[off+inlTreeNodeLen*int64(i):]
		fi.inlTree[i] = inlNode{
			parent:   int32(u32(n)),
			file:     u32(n[4:]),
			line:     int32(u32(n[8:])),
			fn:       l.resolve(st, obj.SymRef{PkgIdx: u32(n[12:]), SymIdx: u32(n[16:])}),
			parentPC: int32(u32(n[20:])),
		}
	}
	return fi, true
}
