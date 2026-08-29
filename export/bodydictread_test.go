// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"os"
	"path/filepath"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// readOneArchive reads every body of one package out of the archive gc wrote
// for it.
func readOneArchive(t *testing.T, path string) []*FuncBody {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/dict\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := archives(t, dir, path)
	if len(list) == 0 {
		t.Skipf("no archive for %s", path)
	}
	data, err := os.ReadFile(list[0][1])
	if err != nil {
		t.Fatal(err)
	}
	payload, err := Payload(data)
	if err != nil {
		t.Fatal(err)
	}
	dec := pkgbits.NewPkgDecoder(list[0][0], string(payload))
	_, bodies, err := ReadBodies(types2.NewContext(), map[string]*types2.Package{}, dec)
	if err != nil {
		t.Fatalf("ReadBodies(%s): %v", path, err)
	}
	return bodies
}

// bodyNamed returns the generic body of one declaration.
func bodyNamed(t *testing.T, bodies []*FuncBody, name string) *Body {
	t.Helper()
	for _, b := range bodies {
		if b.Generic && b.Name == name {
			return b.Body
		}
	}
	t.Fatalf("the archive carries no generic body for %s", name)
	return nil
}

// TestReadBodyCarriesTheDictionaryTheArchiveHolds is what makes a decoded body
// usable.
//
// The slots of a dictionary were numbered when the declaring package was
// compiled and the body names them by number, so a body without its dictionary
// is a tree whose references cannot be resolved. Two of the lists are the ones
// the stenciler of specs/013-generics.md cannot do without: the type
// parameters, which are the domain of its substitution and are the objects
// this decode made rather than the ones the checker made, and the
// subdictionaries, which name the callee of a call whose type arguments depend
// on the enclosing declaration's.
//
// slices.Contains is the declaration that shows both. Its whole body is
// "return Index(s, v) >= 0", and the call to Index carries a slot number and
// no reference to Index at all.
func TestReadBodyCarriesTheDictionaryTheArchiveHolds(t *testing.T) {
	bodies := readOneArchive(t, "slices")
	body := bodyNamed(t, bodies, "Contains")

	if body.Dict == nil {
		t.Fatal("the decoded body carries no dictionary, so the slots it names cannot be resolved")
	}
	if body.Dict.Pkg == nil || body.Dict.Pkg.Path() != "slices" {
		t.Errorf("the dictionary belongs to %v, want slices", body.Dict.Pkg)
	}
	// Contains[S ~[]E, E comparable] binds two, on its own signature, so
	// neither the enclosing list nor the receiver's holds anything.
	if got := len(body.Dict.TypeParams); got != 2 {
		t.Errorf("the dictionary holds %d type parameters, want 2", got)
	}
	if got := len(body.Dict.Implicits) + len(body.Dict.Receivers); got != 0 {
		t.Errorf("the dictionary holds %d enclosing or receiver type parameters, want 0", got)
	}
	for i, tp := range body.Dict.TypeParams {
		if tp == nil {
			t.Fatalf("type parameter %d is absent", i)
		}
	}

	if len(body.Dict.Subdicts) == 0 {
		t.Fatal("the dictionary holds no subdictionary, and the call to Index names one")
	}
	found := false
	for _, use := range body.Dict.Subdicts {
		if use.Name != "Index" {
			continue
		}
		found = true
		if use.Obj == nil {
			t.Error("the subdictionary names Index and the decode resolved no object for it")
		}
		if got := len(use.Targs); got != 2 {
			t.Errorf("the subdictionary names Index with %d type arguments, want 2", got)
		}
		for j, a := range use.Targs {
			if a.Type == nil {
				t.Errorf("type argument %d of the subdictionary is absent", j)
			}
		}
	}
	if !found {
		t.Errorf("no subdictionary of Contains names Index")
	}
}

// TestReadBodySharesOneDictionaryAcrossAGenericTypesMethods pins the identity.
//
// gc writes the methods of a generic type inside the type's element and
// numbers their bodies against the type's dictionary, so a method with a
// dictionary of its own would name different types than the ones the archive
// holds. sync/atomic.Pointer is the declaration nanogo meets first.
func TestReadBodySharesOneDictionaryAcrossAGenericTypesMethods(t *testing.T) {
	bodies := readOneArchive(t, "sync/atomic")
	load := bodyNamed(t, bodies, "(*Pointer).Load")
	store := bodyNamed(t, bodies, "(*Pointer).Store")

	if load.Dict == nil || store.Dict == nil {
		t.Fatal("a method of a generic type decoded without its dictionary")
	}
	if load.Dict != store.Dict {
		t.Error("the methods of one generic type decoded against two dictionaries")
	}
	// Pointer[T] binds one, and it is the type's own rather than a receiver
	// list: the dictionary is the type's, which its methods share.
	if got := len(load.Dict.TypeParams); got != 1 {
		t.Errorf("the dictionary holds %d type parameters, want 1", got)
	}
}
