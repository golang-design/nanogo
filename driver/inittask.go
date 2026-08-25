// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"encoding/binary"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/obj"
)

// The initialisation record of a package.
//
// runtime.main runs no init function it was not handed a record for. It walks
// go:main.inittasks, a list cmd/link builds in ld/inittask.go by starting at
// main..inittask and following the ordering edges to the records of every
// package reachable from it. A package whose record is missing from that walk
// is a package whose init never runs and whose package-level variables keep
// their zero values. Nothing reports it: the program starts, and dies later
// where it reads one of them.
//
// The record's layout is runtime.initTask in runtime/proc.go:
//
//	type initTask struct {
//		state uint32 // 0 = uninitialized, 1 = in progress, 2 = done
//		nfns  uint32
//		// followed by nfns pcs, uintptr sized, one per init function to run
//	}
//
// and doInit1 in the same file is what reads it: it writes state, refuses a
// record whose nfns is zero with throw("inittask with no functions"), takes
// the first function pointer at offset 8, and calls each of the nfns pointers
// in turn. cmd/compile/internal/pkginit's MakeTask writes the same bytes.
//
// Three properties of that follow, and each one is a way to produce a program
// that starts and is wrong:
//
//   - The record is writable. state is assigned twice per record, so a record
//     in read-only memory faults inside the runtime, before any Go frame.
//     SNOPTRDATA, which is what gc's objw.Global with NOPTR produces.
//   - A record with no functions is exactly eight bytes. cmd/link leaves a
//     record of eight bytes or fewer out of the schedule and keeps it only to
//     order the rest, so padding one up to nine bytes is what makes the
//     runtime reach doInit1's throw.
//   - The function pointers are text symbols and so ABIInternal. A reference
//     under ABI0 names a symbol nothing defines.
const (
	// initTaskSuffix is the name gc gives the record, and the name cmd/link
	// looks the root up under: "main..inittask" exactly.
	initTaskSuffix = "..inittask"

	// initTaskHeader is the size of the two words before the function
	// pointers, and so the size of a record that has none.
	initTaskHeader = 8

	// initTaskPtrSize is the width of one function pointer. nanogo emits
	// arm64 only (TargetArch), where a uintptr is eight bytes.
	initTaskPtrSize = 8
)

// initTaskName is the record's symbol for a package path.
func initTaskName(path string) string { return path + initTaskSuffix }

// addInitTask writes the package's initialisation record, and returns whether
// it wrote one.
//
// fns are the functions the record runs, in the order they must run. imports
// are the packages this one imports directly; every one of them gets an
// ordering edge, which is what makes its record, and everything beneath it,
// reachable from this one.
//
// The edges are emitted without asking which imports have a record of their
// own. gc asks, because its importer reads the answer out of the export data,
// and nanogo's reader does not read that section. Asking is not needed: an
// ordering edge is a zero-width relocation, cmd/link's relocsym returns on a
// zero width before it looks at the target, and its inittasks() finds a
// symbol nothing defines to be of size zero and leaves it out of the
// schedule. An edge to a package with no record therefore resolves to nothing
// and orders nothing, which is the correct answer for a package with nothing
// to run.
//
// What that costs is a record nanogo writes where gc would write none: gc
// omits the record of a package that has no init work and no import with
// init work, and nanogo cannot tell the second half apart. The extra record
// holds no functions, so cmd/link keeps it for ordering and the runtime never
// sees it. The alternative is to guess which imports have one, and a guess
// that is wrong in the other direction drops an edge and runs an init late.
func addInitTask(out *obj.Package, imports []export.Import, fns []obj.SymRef) bool {
	task, deps := initTaskFor(out.Main, out.Path, imports, fns)
	if task == nil {
		return false
	}
	for _, dep := range deps {
		name := initTaskName(dep)
		// A data symbol is ABI0. cmd/link resolves a reference by name and
		// ABI together, so a record referenced under ABIInternal names a
		// symbol nothing defines.
		ref := out.AddNonPkgRef(&obj.Symbol{Name: name, ABI: obj.ABI0})
		// go tool nm prints a name only for a symbol the RefName block
		// covers. gc's object shows "U os..inittask" and so does this one.
		out.AddRefName(ref, name)
		task.Relocs = append(task.Relocs, obj.Reloc{Type: obj.R_INITORDER, Sym: ref})
	}
	out.AddDef(task)
	return true
}

// initTaskFor builds the record and names the packages it must be ordered
// after. It returns a nil record when the package needs none.
//
// The record and the edges are built apart because the edges need a symbol
// reference per package and the record's own bytes do not. Everything that
// decides what the record says is here, where a test reads it without an
// object around it.
func initTaskFor(main bool, path string, imports []export.Import, fns []obj.SymRef) (*obj.Symbol, []string) {
	deps := initTaskDeps(imports)
	// gc's rule, in pkginit.MakeTask: a package with nothing to run and
	// nothing to order needs no record. A main package always has one,
	// because cmd/link starts its walk at main..inittask and finds nothing
	// when it is absent.
	if !main && len(deps) == 0 && len(fns) == 0 {
		return nil, nil
	}

	size := initTaskHeader + initTaskPtrSize*len(fns)
	data := make([]byte, size)
	// state stays zero: not initialized yet. The runtime writes it.
	binary.LittleEndian.PutUint32(data[4:], uint32(len(fns)))

	relocs := make([]obj.Reloc, 0, len(fns)+len(deps))
	for i, fn := range fns {
		relocs = append(relocs, obj.Reloc{
			Off:  int32(initTaskHeader + initTaskPtrSize*i),
			Size: initTaskPtrSize,
			Type: obj.R_ADDR,
			Sym:  fn,
		})
	}
	return &obj.Symbol{
		Name:   initTaskName(path),
		Type:   obj.SNOPTRDATA,
		Size:   uint32(size),
		Align:  initTaskPtrSize,
		Data:   data,
		Relocs: relocs,
	}, deps
}

// initTaskDeps is the packages an ordering edge is written for, in the order
// the type checker asked for them, which is source order.
//
// unsafe is dropped. It is synthesised by the compiler, no archive holds it,
// and it has no record for an edge to name.
func initTaskDeps(imports []export.Import) []string {
	deps := make([]string, 0, len(imports))
	for _, im := range imports {
		if im.Path == unsafePkg {
			continue
		}
		deps = append(deps, im.Path)
	}
	return deps
}
