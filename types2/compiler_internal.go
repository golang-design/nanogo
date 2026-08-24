// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Hand-ported from cmd/compile/internal/types2/compiler_internal.go.
//
// The upstream file writes a type back into the syntax tree through
// Expr.SetTypeInfo. nanogo's tree carries no type-and-value record
// (specs/012-type-checking.md), so the rewrite is not mechanical and the file
// is listed as hand-ported in types2/gen/gen.go. The type the upstream code
// reads out of the tree is the result variable's own type, so nothing is lost.

package types2

import (
	"fmt"

	"golang.design/x/nanogo/syntax"
)

// RenameResult takes an array of (result) fields and an index, and if the indexed field
// does not have a name and if the result in the signature also does not have a name,
// then the signature and field are renamed to
//
//	fmt.Sprintf("#rv%d", i+1)
//
// the newly named object is inserted into the signature's scope,
// and the object and new field name are returned.
//
// The intended use for RenameResult is to allow rangefunc to assign results within a closure.
// This is a hack, as narrowly targeted as possible to discourage abuse.
func (s *Signature) RenameResult(results []*syntax.Field, i int) (*Var, *syntax.Name) {
	a := results[i]
	obj := s.Results().At(i)

	if !(obj.name == "" || obj.name == "_" && a.Name == nil || a.Name.Value == "_") {
		panic("Cannot change an existing name")
	}

	pos := a.Pos()

	name := fmt.Sprintf("#rv%d", i+1)
	obj.name = name
	s.scope.Insert(obj)
	obj.setScopePos(pos)

	n := syntax.NewName(pos, obj.Name())
	a.Name = n

	return obj, n
}

// Comment returns the scope comment, for debugging purposes.
func (s *Scope) Comment() string { return s.comment }
