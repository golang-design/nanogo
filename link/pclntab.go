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

	// funcName is the offset of each name in FuncNameTab. A _func record
	// names its function by this offset.
	funcName map[Global]uint32
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
	p := &Pcln{Funcs: a.Textp, funcName: map[Global]uint32{}}
	a.buildFuncNameTab(p)
	return p
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
		for _, node := range a.l.inlTree(g) {
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
		fi := l.Def(l.resolve(st, au.Sym))
		if fi == nil || len(fi.Data) < funcInfoFixed {
			return funcInfo{}, false
		}
		return decodeFuncInfo(l, st, fi.Data)
	}
	return funcInfo{}, false
}

// inlTree returns the calls the compiler inlined into a function.
func (l *Loader) inlTree(g Global) []inlNode {
	fi, ok := l.funcInfo(g)
	if !ok {
		return nil
	}
	return fi.inlTree
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
