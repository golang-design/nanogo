// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"encoding/binary"
	"testing"

	"golang.design/x/nanogo/export"
	"golang.design/x/nanogo/obj"
)

// TestInitTaskLayoutIsTheRuntimeStruct states the bytes runtime.initTask
// reads, field by field.
//
// runtime/proc.go declares state and nfns as two uint32 words followed by
// nfns pointers, and doInit1 reads them at those offsets: it writes state, it
// takes the first function pointer at offset 8, and it steps by the pointer
// size. A record that disagrees with any of that does not fail to link. It
// runs the wrong function, or none.
func TestInitTaskLayoutIsTheRuntimeStruct(t *testing.T) {
	fn := obj.SymRef{PkgIdx: obj.PkgIdxSelf, SymIdx: 3}
	task, deps := initTaskFor(false, "example.com/a", []export.Import{{Path: "errors"}}, []obj.SymRef{fn})
	if task == nil {
		t.Fatal("no record was built for a package with an init function")
	}
	if task.Name != "example.com/a..inittask" {
		t.Errorf("the record is named %q, and cmd/link looks it up by name", task.Name)
	}
	if task.Type != obj.SNOPTRDATA {
		t.Errorf("the record is %v, want SNOPTRDATA: doInit1 assigns state, so it must be writable", task.Type)
	}
	if task.Size != 16 || len(task.Data) != 16 {
		t.Fatalf("the record is %d bytes with %d of data, want 16: eight of header and one pointer", task.Size, len(task.Data))
	}
	if state := binary.LittleEndian.Uint32(task.Data[0:]); state != 0 {
		t.Errorf("state is %d, want 0: anything else tells the runtime the package is already initialised", state)
	}
	if nfns := binary.LittleEndian.Uint32(task.Data[4:]); nfns != 1 {
		t.Errorf("nfns is %d, want 1", nfns)
	}
	if task.Align != 8 {
		t.Errorf("the record is aligned to %d, want 8", task.Align)
	}
	if len(task.Relocs) != 1 {
		t.Fatalf("the record has %d relocations before its edges, want the one function pointer", len(task.Relocs))
	}
	if r := task.Relocs[0]; r.Off != 8 || r.Size != 8 || r.Type != obj.R_ADDR || r.Sym != fn {
		t.Errorf("the function pointer is %+v; want offset 8, width 8, R_ADDR, to %v", r, fn)
	}
	if len(deps) != 1 || deps[0] != "errors" {
		t.Errorf("the record is ordered after %v, want [errors]", deps)
	}
}

// TestInitTaskWithNoFunctionsIsEightBytes is the size cmd/link prunes on.
//
// inittaskSym leaves a record of eight bytes or fewer out of the schedule and
// keeps it only to order the rest. A record padded past eight bytes with no
// functions in it is scheduled, and doInit1 answers a zero nfns with
// throw("inittask with no functions"), which is a crash inside the runtime
// with no nanogo frame in it.
func TestInitTaskWithNoFunctionsIsEightBytes(t *testing.T) {
	task, _ := initTaskFor(true, "main", []export.Import{{Path: "os"}}, nil)
	if task == nil {
		t.Fatal("a main package got no record; cmd/link starts its walk at main..inittask")
	}
	if task.Size != 8 || len(task.Data) != 8 {
		t.Fatalf("a record with no functions is %d bytes, want 8", task.Size)
	}
	if nfns := binary.LittleEndian.Uint32(task.Data[4:]); nfns != 0 {
		t.Errorf("nfns is %d, want 0", nfns)
	}
	if len(task.Relocs) != 0 {
		t.Errorf("the record points at %d functions and runs none", len(task.Relocs))
	}
}

// TestInitTaskOrdersEveryImport is the reachability requirement.
//
// cmd/link reaches a package's record only by following an edge from a record
// it already has. main..inittask is the one root, so an import with no edge
// from it takes its whole subtree out of the schedule: every init beneath it
// is skipped and every package-level variable it sets keeps its zero value.
// The program starts and dies later, somewhere else.
func TestInitTaskOrdersEveryImport(t *testing.T) {
	imports := []export.Import{{Path: "os"}, {Path: "unsafe"}, {Path: "errors"}}
	_, deps := initTaskFor(true, "main", imports, nil)
	// unsafe is synthesised by the compiler and no archive holds it, so
	// there is nothing for an edge to name. Source order for the rest, which
	// is the order the checker asked for them.
	want := []string{"os", "errors"}
	if len(deps) != len(want) {
		t.Fatalf("the record is ordered after %v, want %v", deps, want)
	}
	for i, w := range want {
		if deps[i] != w {
			t.Errorf("edge %d is to %q, want %q", i, deps[i], w)
		}
	}
}

// TestNoInitTaskWithNothingToOrder keeps the object free of a record that
// says nothing.
//
// gc's rule, in pkginit.MakeTask: a package that imports nothing and
// initialises nothing needs no record. A main package always gets one anyway,
// because cmd/link starts its walk at main..inittask and a program whose root
// is absent runs no init at all.
func TestNoInitTaskWithNothingToOrder(t *testing.T) {
	if task, _ := initTaskFor(false, "example.com/a", nil, nil); task != nil {
		t.Error("a package with nothing to order got a record")
	}
	if task, _ := initTaskFor(false, "example.com/b", []export.Import{{Path: "unsafe"}}, nil); task != nil {
		t.Error("importing unsafe alone produced a record")
	}
	if task, _ := initTaskFor(true, "main", nil, nil); task == nil {
		t.Error("a main package that imports nothing got no record")
	}
}

// TestAddInitTaskReferencesEveryEdgeByName installs the record and checks
// what a reader of the object sees.
//
// A relocation names its target through the non-package reference space, and
// go tool nm prints a name only for a reference the RefName block covers. gc's
// object shows "U os..inittask"; an object without the entry shows an index
// pair, which no reader can act on.
func TestAddInitTaskReferencesEveryEdgeByName(t *testing.T) {
	p := obj.NewPackage("main")
	p.Main = true
	if !addInitTask(p, []export.Import{{Path: "os"}, {Path: "errors"}}, nil) {
		t.Fatal("a main package got no record")
	}
	b, err := p.Bytes()
	if err != nil {
		t.Fatalf("the object with a record in it does not write: %v", err)
	}
	for _, want := range []string{"main..inittask", "os..inittask", "errors..inittask"} {
		if !containsString(b, want) {
			t.Errorf("the object's strings do not carry %q", want)
		}
	}
}

func containsString(b []byte, s string) bool {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}
