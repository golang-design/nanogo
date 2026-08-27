// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"sort"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// Carrying function bodies into a file the writer produces.
//
// bodywrite.go encodes one body element and asks a [bodyRefs] which element
// of the package holds each thing the body names. elemRefs answers with the
// index the archive a body was read from already gave it, which proves the
// layout and nothing else. This file is the other resolver: the body was
// built from syntax and carries no index at all, so every reference has to be
// allocated in the package being written.
//
// The path a body takes to gc is the private root's list, which is the
// inlining path of specs/015-export-data.md. gc reads a body from it only
// when the declaration's extension data also says the declaration has an
// inlinable body, and it reads it in order to inline the call. The generic
// path, which is a reference in the extension data itself, stays refused: it
// needs the object dictionary the writer does not fill.

// A Source is what a package's own source adds to what the checker recorded.
//
// It is what the driver assembles: the FileSet that resolves a declaration's
// position, and the bodies [BodySource] built. The writer allocates the
// elements each body names and writes nothing it cannot allocate.
type Source struct {
	// Fset resolves the position of a declaration and of a position a body
	// carries. Without it every position the file holds is absent.
	Fset *syntax.FileSet

	// Funcs is one entry per declaration whose body reaches the file, in
	// the order the caller decided. The order the file holds them in is the
	// order of the elements they were written to, which is gc's order.
	Funcs []InlineFunc

	// File maps a position's file name to the name the export data records
	// for it. It is nil when the names are recorded as they were parsed.
	//
	// The driver owns the answer, because it is the same answer the object's
	// line table already holds: one compiler must not report two names for
	// one file. See [pkgWriter.posBaseIdx].
	File func(string) string
}

// An InlineFunc is one declaration whose body gc can inline.
type InlineFunc struct {
	// Obj is the declaration. It is what pairs the body with the extension
	// data that says the declaration has one.
	Obj *types2.Func

	// Name is gc's linker symbol name for the declaration: "F" for a
	// function, "T.M" or "(*T).M" for a method. gc looks a body up in the
	// private root's list by this name and the package path.
	Name string

	// Cost is the inlining budget the body spends, in gc's units. gc reads
	// it and inlines the call when the cost is inside the budget of the
	// function it would inline into.
	Cost int

	// Body is the tree [BodySource.BuildBody] built.
	Body *Body
}

// bodyEntry is one entry of the private root's body list.
type bodyEntry struct {
	path string
	name string
	idx  pkgbits.Index
}

// @@@ Position bases

// posBaseIdx returns the index of the position base for file, writing its
// element if it is new.
//
// The name is the driver's answer and not this package's. gc writes
// objabi.AbsFile's form, which joins a relative path against the process
// working directory and rewrites a GOROOT prefix to "$GOROOT". Both make the
// bytes depend on where the compiler ran, which specs/053-determinism.md
// forbids, so nanogo records the name its own object's line table records:
// the parsed path with -trimpath applied and nothing else.
//
// Every element is a file base. nanogo resolves a //line directive when it
// resolves the position (specs/010-scanner-and-positions.md), so the name,
// the line and the column a body carries are already the ones the directive
// asks for, and a reader that rebuilt the directive chain from them would
// apply it twice.
func (pw *pkgWriter) posBaseIdx(file string) pkgbits.Index {
	if pw.fileName != nil {
		file = pw.fileName(file)
	}
	if idx, ok := pw.posBasesIdx[file]; ok {
		return idx
	}

	w := pw.newWriter(pkgbits.SectionPosBase, pkgbits.SyncPosBase)
	pw.posBasesIdx[file] = w.Idx
	w.String(file)
	w.Bool(true) // a file base, so no directive position follows
	return w.Flush()
}

// @@@ The resolver for a body that was built

// declRefs answers with the element of the package being written that holds
// each thing a body names, allocating the element when it is new.
//
// It refuses what it cannot allocate. An index it guessed would be a
// declaration gc reads as a different one, which is a wrong answer with gc as
// the reader rather than a refusal.
type declRefs struct {
	pw *pkgWriter
}

func (d *declRefs) strIdx(s string) pkgbits.Index { return d.pw.StringIdx(s) }

func (d *declRefs) pkgIdx(pkg *types2.Package) pkgbits.Index { return d.pw.pkgIdx(pkg) }

func (d *declRefs) typIdx(t TypeUse) pkgbits.Index {
	if t.Type == nil {
		d.pw.refuse("a type the body names", "the checker recorded no type for it")
	}
	return d.pw.typIdx(t.Type)
}

func (d *declRefs) objIdx(o ObjUse) pkgbits.Index {
	if o.Obj == nil {
		d.pw.refuse(o.Name, "the body names a declaration the checker recorded no object for")
	}
	return d.pw.objIdx(o.Obj)
}

func (d *declRefs) posBaseIdx(p Pos) pkgbits.Index {
	if p.File == "" {
		d.pw.refuse("a position the body carries", "the position is known and names no file")
	}
	return d.pw.posBaseIdx(p.File)
}

func (d *declRefs) bodyIdx(e *FuncLitExpr) pkgbits.Index { return d.pw.litBodyIdx(e) }

// dictIdx refuses every slot.
//
// objDict writes four zeros for the runtime lists and no derived type, so the
// dictionary a slot would be read out of is empty. Writing the slot anyway is
// a type gc resolves to whatever the empty dictionary happens to give it,
// which is a wrong answer and not a refusal. specs/015-export-data.md has
// what filling the dictionary needs.
func (d *declRefs) dictIdx(what string, slot int) int {
	d.pw.refuse(fmt.Sprintf("%s, slot %d", what, slot),
		"the body names a slot of an object dictionary the writer does not fill")
	panic("unreachable")
}

var _ bodyRefs = (*declRefs)(nil)

// litBodyIdx claims the element a function literal's body goes in.
//
// The index is claimed while the enclosing body is being encoded, because
// that is where the reference to it is written, and the element itself is
// filled in afterwards by [pkgWriter.encodeBodyInto].
func (pw *pkgWriter) litBodyIdx(e *FuncLitExpr) pkgbits.Index {
	if enc, ok := pw.lits[e]; ok {
		return enc.Idx
	}
	enc := pw.NewEncoder(pkgbits.SectionBody, pkgbits.SyncFuncBody)
	pw.lits[e] = enc
	return enc.Idx
}

// @@@ Writing the elements

// writeBody writes b into a new element and returns its index.
//
// check collects the reasons the body cannot be offered for inlining and is
// nil on the write path, where the decision has already been made.
func (pw *pkgWriter) writeBody(path, name string, b *Body, check *inlineCheck) pkgbits.Index {
	enc := pw.NewEncoder(pkgbits.SectionBody, pkgbits.SyncFuncBody)
	pw.encodeBodyInto(enc, path, name, b, check)
	return enc.Idx
}

// encodeBodyInto fills one claimed element and then the elements of every
// function literal that element named.
//
// A literal's body is an element of its own, so a body is a tree of elements
// and not one element. The walk is depth first in the order the literals were
// named, which is the order gc's own writer produces.
func (pw *pkgWriter) encodeBodyInto(enc *pkgbits.Encoder, path, name string, b *Body, check *inlineCheck) {
	w := &bodyWriter{Encoder: enc, refs: pw.refs, path: path, name: name, check: check}
	w.encodeBody(b)
	enc.Flush()

	for _, lit := range w.nested {
		if lit.Decoded == nil {
			w.refuse("a function literal in the body carries no body of its own")
		}
		pw.encodeBodyInto(pw.lits[lit], path, name+".func", lit.Decoded, check)
	}
}

// writeBodies writes the body of each function and returns the private
// root's list.
//
// The list is sorted by element index, which is gc's order and is fixed by
// the order the bodies were written rather than by the caller's slice.
func (pw *pkgWriter) writeBodies(path string, funcs []InlineFunc) []bodyEntry {
	out := make([]bodyEntry, 0, len(funcs))
	for i := range funcs {
		fn := &funcs[i]
		out = append(out, bodyEntry{path: path, name: fn.Name, idx: pw.writeBody(path, fn.Name, fn.Body, nil)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].idx < out[j].idx })
	return out
}

// @@@ Deciding which bodies can be written

// writableBodies returns the entries of funcs whose bodies the writer can
// allocate every element of and gc's inliner can be offered.
//
// A body is optional data. The declaration it belongs to is written either
// way, so a body that fails either test is left out rather than made a
// refusal of the package. Which declarations reach here at all is the
// caller's decision: the driver refuses one by what the declaration is, such
// as a //go: directive it carries, and this refuses one by what its body
// holds.
//
// The decision is made by writing the body into a package the caller never
// sees. Which elements a body needs is a property of the types and the
// declarations it names and not of the indices they land at, so a body that
// writes into an empty package writes into the real one: allocation is
// memoized, and a memoized entry is one that already succeeded.
//
// The probe is what keeps a refused body from leaving a hole behind. By the
// time the writer refuses, it has claimed element indices it will never fill,
// and a claimed index that holds nothing is exactly the declaration gc reads
// as a different one. Here the holes are in the probe, which is thrown away.
func writableBodies(pkg *types2.Package, fset *syntax.FileSet, file func(string) string, funcs []InlineFunc) []InlineFunc {
	reached := surfaceObjects(pkg, fset, file)
	out := make([]InlineFunc, 0, len(funcs))
	probe := newPkgWriter(pkg, nil, fset, file)
	for i := range funcs {
		fn := funcs[i]
		if !reaches(reached, fn.Obj) {
			continue
		}
		check := &inlineCheck{}
		if !writesInto(probe, pkg.Path(), &fn, check) {
			// The probe now holds elements it claimed and did not fill, so
			// it cannot answer for the next body. Start a fresh one.
			probe = newPkgWriter(pkg, nil, fset, file)
			continue
		}
		if check.reason != "" {
			continue
		}
		out = append(out, fn)
	}
	return out
}

// surfaceObjects returns the declarations the exported surface reaches.
//
// It is what the file holds an object element for. A body is reached through
// the private root's list, and the entry names the declaration rather than
// pointing at it, so an entry naming a declaration the file has no element for
// is an entry no reader can pair with anything. nanogo's own reader refuses
// such a file by name.
//
// The set is measured on a package the caller never sees, for the reason
// [writableBodies] gives: the answer has to be known before the first
// extension data is written, because that is where a declaration says whether
// it has an inlinable body.
func surfaceObjects(pkg *types2.Package, fset *syntax.FileSet, file func(string) string) map[types2.Object]bool {
	pw := newPkgWriter(pkg, nil, fset, file)
	ok := func() (ok bool) {
		defer func() {
			switch v := recover().(type) {
			case nil:
			case *UnsupportedError, *BodyError:
				ok = false
			default:
				panic(v)
			}
		}()
		scope := pkg.Scope()
		for _, name := range scope.Names() {
			if obj := scope.Lookup(name); obj != nil && obj.Exported() {
				pw.objIdx(obj)
			}
		}
		return true
	}()
	if !ok {
		// The surface itself is refused, so Write refuses the package and
		// no body of it reaches a file either way.
		return nil
	}
	out := make(map[types2.Object]bool, len(pw.objsIdx))
	for obj := range pw.objsIdx {
		out[obj] = true
	}
	return out
}

// reaches reports whether the file holds the declaration fn's body would be
// paired with.
//
// A function is its own object element. A method is not: it is written inside
// the element of the type that declares it, and a reader pairs the two by
// position, so what the file has to hold is the receiver's type.
func reaches(objs map[types2.Object]bool, fn *types2.Func) bool {
	recv := fn.Signature().Recv()
	if recv == nil {
		return objs[fn]
	}
	typ := recv.Type()
	if p, isPtr := types2.Unalias(typ).(*types2.Pointer); isPtr {
		typ = p.Elem()
	}
	named, ok := types2.Unalias(typ).(*types2.Named)
	if !ok {
		return false
	}
	return objs[named.Obj()]
}

// SymName returns the name gc's linker gives a function or a method, which is
// the name the private root's body list names a body by.
//
// A method is "T.M" for a value receiver and "(*T).M" for a pointer one. The
// second result is false for a method whose receiver is not a defined type,
// which no declaration has.
func SymName(fn *types2.Func) (string, bool) {
	recv := fn.Signature().Recv()
	if recv == nil {
		return fn.Name(), true
	}
	typ := recv.Type()
	if p, isPtr := types2.Unalias(typ).(*types2.Pointer); isPtr {
		typ = p.Elem()
	}
	named, ok := types2.Unalias(typ).(*types2.Named)
	if !ok {
		return "", false
	}
	return methodSym(named, fn), true
}

// writesInto reports whether probe can write fn's body, and fills check with
// what would keep the body from being offered for inlining.
//
// Only the writer's own two refusals are caught. Anything else is a defect in
// the writer rather than a body it has no shape for, and swallowing it here
// would turn it into a body that silently never reaches a file.
func writesInto(probe *pkgWriter, path string, fn *InlineFunc, check *inlineCheck) (ok bool) {
	defer func() {
		switch v := recover().(type) {
		case nil:
			return
		case *UnsupportedError, *BodyError:
			ok = false
		default:
			panic(v)
		}
	}()
	probe.writeBody(path, fn.Name, fn.Body, check)
	return true
}
