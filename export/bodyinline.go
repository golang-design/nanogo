// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import "fmt"

// Which bodies nanogo offers gc for inlining, and at what price.
//
// gc runs no check of its own on an imported body. cmd/compile's CanInline
// decides inlinability for a function it compiles and writes the answer into
// the export data, and its reader takes the answer as given. So every rule
// CanInline enforces is the exporting compiler's rule to enforce, and a body
// nanogo offers that gc's own inliner would have refused is a program gc
// builds wrong rather than a build that fails.
//
// Two of those rules produce a wrong program rather than a slow one, which is
// why they are the ones this file names:
//
//   - recover. A call to recover has to stand in the function the deferred
//     call belongs to. Inlined into another function it recovers nothing, and
//     nothing reports it. cmd/compile refuses ir.ORECOVER for this reason.
//   - GetCallerPC and GetCallerSP. Both answer about the physical caller, and
//     a caller that was inlined away is not the caller the source meant.
//
// A third produces a broken binary rather than a wrong program, and it was
// found by building one:
//
//   - An instantiation of a generic. gc inlines the offered body into its
//     caller and then inlines the instantiation into that, and the inline tree
//     of the caller then names a symbol nothing defined: cmd/link stops with
//     "inlined function slices.Contains[go.shape.[]int,go.shape.int] missing
//     func info". A body gc wrote reaches its own inliner having already been
//     through gc's own instantiation pass, and one nanogo wrote has not, so
//     what the two hand the inliner is not the same thing.
//     specs/024-inlining-and-devirtualization.md owns closing the gap; until
//     it is closed the body is offered without it.
//
// The rest of the set is narrowness rather than necessity. This is the first
// shape a body reaches gc in, so it holds no statement and no expression whose
// handling in gc's inliner nanogo has not seen work: no go or defer statement,
// which gc refuses outright, no function literal, whose body is a second
// element and a set of captured variables gc rebuilds, and no runtime helper,
// which only the closure gc builds for a loop over a function names.
// specs/024-inlining-and-devirtualization.md owns widening it.

// MaxInlineCost is the price nanogo offers an exported body at.
//
// It is cmd/compile/internal/inline.inlineMaxBudget, which is the cost of the
// largest body gc's default budget accepts. gc reads the field as the budget
// the body spends and uses it twice: to decide whether to inline a call to
// this function, and to charge a function that calls this one when deciding
// whether that one can be inlined in turn.
//
// nanogo runs no inliner and measures no hairiness
// (specs/024-inlining-and-devirtualization.md), so the number is a policy and
// not a measurement, and the policy is the conservative end of both uses. A
// caller is charged the most gc's budget allows, so no caller is made
// inlinable by an understated price. And a big caller, whose budget is 20,
// inlines nothing nanogo exported, which is where gc is most careful with its
// own bodies too.
const MaxInlineCost = 80

// an inlineCheck collects the reason a body is not offered for inlining.
//
// It is filled while the body is encoded, so the walk it needs is the
// encoder's own and no second walk of the tree exists to drift from it. A nil
// check records nothing, which is the ordinary write path.
type inlineCheck struct {
	reason string
}

// refuse records the first reason, which is the one a report names.
func (c *inlineCheck) refuse(format string, args ...any) {
	if c == nil || c.reason != "" {
		return
	}
	c.reason = fmt.Sprintf(format, args...)
}

// stmt notes one statement encoding.
func (c *inlineCheck) stmt(k StmtKind) {
	if k == StmtCall {
		c.refuse("the body starts a goroutine or defers a call, which gc's inliner refuses")
	}
}

// expr notes one expression encoding.
func (c *inlineCheck) expr(k ExprKind) {
	switch k {
	case ExprFuncLit:
		c.refuse("the body holds a function literal, whose own body is a second element")
	case ExprRuntimeBuiltin:
		c.refuse("the body calls a runtime helper")
	}
}

// obj notes one use of a package-scope declaration.
func (c *inlineCheck) obj(o ObjUse) {
	if len(o.Targs) != 0 {
		c.refuse("the body names the instantiation %s, and gc's inliner leaves the inline tree of the caller naming a symbol nothing defines", o.Name)
	}
	if o.Pkg == nil && o.Name == "recover" {
		c.refuse("the body calls recover, which recovers nothing once it is inlined into another function")
	}
	if o.Pkg != nil && o.Pkg.Path() == "internal/runtime/sys" &&
		(o.Name == "GetCallerPC" || o.Name == "GetCallerSP") {
		c.refuse("the body calls %s, which answers about a caller inlining removes", o.Name)
	}
}
