// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"go/constant"

	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// Range over a function, which is the sixth range row of
// specs/020-ir.md.
//
//	for k, v := range f { body }
//
// where f is func(yield func(K, V) bool) becomes a call to f with the
// body as a closure:
//
//	f(func(#p0 K, #p1 V) bool {
//		k, v := #p0, #p1
//		body
//		return true
//	})
//
// The row is built here and not in ir/lower.go, and the reason is the
// closure. The body has to become a function of the package, with the
// variables it reads as captures, and the builder is the pass that turns
// a body into a function: it owns literalNames, the free set and
// closureNode. A lowering that had to invent a *Func would be inventing
// the builder a second time. gc places the rewrite for the same reason
// one stage earlier still, in cmd/compile/internal/rangefunc, so that the
// generated body is an ordinary function by the time the back end sees
// it.
//
// # The control flow out of the body is the whole of the difficulty
//
// The body is no longer in the frame that wrote it, so every way out of
// it has to be carried across the call.
//
//   - continue is the body running off its end, which is "return true".
//   - break stops the iteration, which is "return false". Nothing else
//     is needed: the statement after the loop is the statement after
//     the call.
//   - a return from the enclosing function has to stop the iteration and
//     then return, and those are two frames. So the body writes the
//     enclosing function's results, sets a variable the enclosing frame
//     reads, and returns false; the enclosing frame tests the variable
//     after the call and returns.
//
// The variable is gc's #next and is spelled .rangefunc_next here. gc
// gives one function one #next and encodes in it which level of a nested
// loop a labelled break or continue means. This row refuses a labelled
// break and a labelled continue, so the variable is only ever 0 or -1 and
// each loop gets one of its own, declared immediately before its own
// call, and given its value there rather than at the head of the
// function. With two values the variable cannot go stale on its own: the
// frame returns as soon as it holds -1, so a loop inside another loop
// never reaches its own statement again with -1 in it. What the placement
// buys is that this stays true of the variable and not of the reasoning
// around it, which is the property gc's encoding does not have: gc's
// #next carries a code per loop level and is read after a call that did
// not set it.
//
// Nesting needs nothing further. The body of an outer loop is itself a
// function being built, so an inner loop's test after its own call is
// built inside the outer body, where "return" is already the rewrite
// above: it writes the outer loop's own variable and returns false, which
// stops the outer iteration, and the outer test then returns. The
// propagation is therefore one rule applied at each level rather than a
// depth counted at the innermost one.
//
// The results the body writes are the enclosing function's own result
// objects, which the closure captures like any other variable. That is
// the whole of the mechanism: specs/033-closures-defer-panic.md moves a
// captured variable into a heap cell, ir/lower.go's singleExit copies the
// cell into the result storage at the single exit, and a bare return
// after the call reads what the body left there.
//
// # What is refused
//
// A label inside the body, a goto, and a labelled break or continue. Each
// leaves the body for a place the yield function cannot name: gc assigns
// a distinct code to each such target, tests for it after every call, and
// unwinds one loop per test. That machinery is what this row does not
// build, and half of it silently misplaces control flow, so the shapes
// that need it are refused by name rather than approximated.
//
// A defer inside the body. A defer written in the loop body belongs to
// the frame that wrote the loop, not to the yield function, and gc keeps
// it there with runtime.deferrangefunc and a defer chain the two frames
// share. Built as an ordinary defer of the yield function it would run at
// the end of the iteration rather than at the end of the enclosing
// function, which is a wrong answer with no diagnostic.
//
// A destination that is not a name in the "for k, v = range f" form. The
// operands of an index expression or a pointer indirection on the left
// are evaluated in the enclosing function, and carrying them into the
// yield function would evaluate them once per iteration in a frame that
// is not the one the source wrote them in.
//
// # What is not checked
//
// The language makes it a run-time error for an iterator to call yield
// again after yield returned false, and gc detects it: each loop carries
// a state variable, the body and the code after the call test it, and
// runtime.panicrangestate raises the error. That check is not built here,
// so an iterator that misbehaves runs the loop body again instead of
// panicking. It is a conformance gap and it is stated in specs/020-ir.md's
// row: no correct program can see it, because a correct iterator does not
// call yield after it returned false.

// yieldFrame is the range-over-func body being built.
//
// It is what a return inside the body needs: where to write the values,
// which variable says a return happened, and the two types the rewrite
// builds constants of.
type yieldFrame struct {
	// next is the .rangefunc_next of this loop, nil when the body holds
	// no return and needs none.
	next *Object

	// results are the result objects of the function the source's return
	// returns from, which is the innermost enclosing function that is not
	// a yield function.
	results []*Object

	boolType *Type
	intType  *Type
}

// rangeFuncYield returns the signature of the yield function a range
// expression takes, or nil when the operand is not a range over a function.
//
// Nil is the answer for the other five range rows and for a function that is
// not an iterator, and the caller does the same thing with all of them: it
// builds an ORange, which ir/lower.go carries for the rows it owns and refuses
// by name for anything else. So an operand the checker would have rejected
// cannot reach the rewrite below by being nearly an iterator.
func rangeFuncYield(t types2.Type) *types2.Signature {
	sig, _ := coreType(t).(*types2.Signature)
	if sig == nil || sig.Params().Len() != 1 || sig.Results().Len() != 0 {
		// The iterator returns nothing, which is what lets the call below
		// be a statement of void type. A signature with a result would
		// make that node say the call produces nothing while the callee
		// writes result registers.
		return nil
	}
	y, _ := coreType(sig.Params().At(0).Type()).(*types2.Signature)
	if y == nil || y.Results().Len() != 1 {
		return nil
	}
	return y
}

// scanRangeFunc reports what in a range statement over a function this row
// does not carry, and whether the body holds a return.
//

// It is separate from the rewrite below, and is called before it, because a
// refusal has to leave the statement in the tree. The caller builds an ORange
// for a shape this reports on, ir/lower.go refuses that a second time, and a
// tree this pass reported an error about still holds the statement the error
// is about. Deciding while rewriting would instead drop the loop, and a
// function that silently lost a statement reads as a function with nothing
// wrong with it.
func scanRangeFunc(s *syntax.ForStmt, rc *syntax.RangeClause) (why string, returns bool) {
	var body []syntax.Stmt
	if s.Body != nil {
		body = s.Body.List
	}
	if why, returns = scanRangeFuncBody(body); why != "" {
		return "whose body holds " + why, returns
	}
	if !rc.Def {
		for _, d := range syntax.UnpackListExpr(rc.Lhs) {
			if name, _ := syntax.Unparen(d).(*syntax.Name); name == nil {
				return fmt.Sprintf("assigning to %T", syntax.Unparen(d)), returns
			}
		}
	}
	return "", returns
}

// rangeFunc builds a range over a function.
//
// returns is what scanRangeFunc reported, and the caller has already refused
// every shape this row does not carry.
func (b *builder) rangeFunc(s *syntax.ForStmt, rc *syntax.RangeClause, yield *types2.Signature, returns bool) {
	pos := s.Pos()
	if b.fn == nil {
		b.errorf("ir: a range over a function outside a function")
		return
	}
	dsts := syntax.UnpackListExpr(rc.Lhs)
	if len(dsts) > yield.Params().Len() {
		b.errorf("ir: a range over a function with %d variables and a yield function taking %d", len(dsts), yield.Params().Len())
		return
	}

	boolType := b.irType(types2.Typ[types2.Bool])
	intType := b.irType(types2.Typ[types2.Int])

	// The iterator is evaluated once, before the call, which is what the
	// specification requires of every range expression.
	callee := b.expr(rc.X)

	// The variable the body reports a return through. It is declared in
	// the function that holds the loop, so a body that returns captures
	// it and the test after the call reads what the body wrote.
	var next *Object
	if returns {
		next = &Object{Name: ".rangefunc_next", Type: intType, Pos: pos, Class: ClassLocal}
		b.fn.Locals = append(b.fn.Locals, next)
		b.owner[next] = b.fn
	}

	// The results a return inside the body writes are the enclosing
	// function's, and an enclosing yield function is not one: its results
	// are the yield function's single bool.
	results := b.fn.Results
	if b.yield != nil {
		results = b.yield.results
	}

	lit := b.newLiteral(pos)
	lit.Type = b.irType(yield)
	saveFn, saveSinks, saveFree, saveYield := b.fn, b.sinks, b.free, b.yield
	b.fn, b.sinks, b.free = lit, nil, make(map[*Object]bool)
	b.yield = &yieldFrame{next: next, results: results, boolType: boolType, intType: intType}

	// The parameters are the yield function's and never the clause's. A
	// "for range f" over a two-value iterator still takes two parameters,
	// and a literal with the clause's count would be a function value of
	// a type the iterator cannot call.
	for i := 0; i < yield.Params().Len(); i++ {
		p := &Object{
			Name:  fmt.Sprintf(".p%d", i),
			Type:  b.irType(yield.Params().At(i).Type()),
			Pos:   pos,
			Class: ClassParam,
		}
		lit.Params = append(lit.Params, p)
		b.owner[p] = lit
	}
	res := &Object{Name: ".r0", Type: boolType, Pos: pos, Class: ClassResult}
	lit.Results = []*Object{res}
	b.owner[res] = lit

	b.push()
	// The iteration variables are written at the head of the body, out of
	// the parameters. The Go 1.22 rule that a := in a range clause
	// declares a fresh variable each iteration needs nothing here and
	// perIteration is not called: each iteration is a call, so each
	// iteration already has its own frame and its own variables.
	for i, d := range dsts {
		name, _ := syntax.Unparen(d).(*syntax.Name)
		if name != nil && name.Value == "_" {
			continue
		}
		src := ref(lit.Params[i], pos)
		if rc.Def {
			if name == nil {
				b.errorf("ir: a range declaration is not a name")
				continue
			}
			b.emit(define(pos, b.name(name), src))
			continue
		}
		b.emit(Assign(pos, b.expr(d), src))
	}
	if s.Body != nil {
		for _, st := range s.Body.List {
			b.stmt(st)
		}
	}
	out := b.pop()
	r := yieldRewriter{boolType: boolType}
	out = r.stmts(out, 0, 0)
	// The body running off its end is the next iteration, which is what
	// yield returning true says.
	out = append(out, r.ret(pos, true))
	lit.Body = out

	free := b.free
	b.fn, b.sinks, b.free, b.yield = saveFn, saveSinks, saveFree, saveYield
	closure := b.closureNode(lit, pos, sortedCaptures(free))

	if next != nil {
		// The declaration is here, immediately before the call, so that
		// the variable starts at zero on every execution of this
		// statement rather than once per frame. Nothing today can leave
		// -1 behind, because the frame returns as soon as it holds -1,
		// and this is what keeps the test below reading only what this
		// call wrote.
		b.emit(define(pos, ref(next, pos), &Node{
			Op: OConst, Pos: pos, Type: intType, Val: Const{Val: constant.MakeInt64(0)},
		}))
	}
	b.emit(&Node{Op: OCall, Pos: pos, Type: voidType, X: callee, Args: []Expr{closure}})
	if next == nil {
		return
	}
	b.emit(&Node{
		Op: OIf, Pos: pos, Type: voidType,
		X: &Node{
			Op: OCompare, Op1: syntax.Neq, Pos: pos, Type: boolType,
			X: ref(next, pos),
			Y: &Node{Op: OConst, Pos: pos, Type: intType, Val: Const{Val: constant.MakeInt64(0)}},
		},
		Body: b.yieldPropagate(pos),
	})
}

// yieldPropagate is what the enclosing frame does when the body reported
// a return.
//
// In the function the source wrote it is a bare return: the body has
// already written the results, and specs/033-closures-defer-panic.md's
// single exit copies the cell of each captured result into the result
// storage. Inside another yield function it is that function's own
// return, so the same rewrite applies one level out and the iteration
// this frame is running stops too.
func (b *builder) yieldPropagate(pos syntax.Pos) []Stmt {
	if b.yield == nil {
		return []Stmt{&Node{Op: OReturn, Pos: pos, Type: voidType}}
	}
	f := b.yield
	b.noteUse(f.next)
	return []Stmt{
		Assign(pos, ref(f.next, pos), &Node{
			Op: OConst, Pos: pos, Type: f.intType, Val: Const{Val: constant.MakeInt64(-1)},
		}),
		&Node{Op: OReturn, Pos: pos, Type: voidType, Args: []Expr{&Node{
			Op: OConst, Pos: pos, Type: f.boolType, Val: Const{Val: constant.MakeBool(false)},
		}}},
	}
}

// yieldReturn builds a return the source wrote inside the body of a range
// over a function.
//
// The values are the enclosing function's results and are written to its
// result objects, which the yield function captures. Every value is
// evaluated before any result is written, for the reason ir/lower.go's
// exitReturn gives: an operand may read a result object, so "return y, x"
// of named results is a swap and a store per operand in order would make
// it a copy.
func (b *builder) yieldReturn(s *syntax.ReturnStmt) {
	pos := s.Pos()
	f := b.yield
	results := syntax.UnpackListExpr(s.Results)
	nres := 0
	if b.sig != nil {
		nres = b.sig.Results().Len()
	}
	switch {
	case len(results) == 0:
		// A bare return, which leaves the named results as they are.

	case len(results) == 1 && nres > 1:
		// return g(), where g returns everything the enclosing function
		// returns. The values are not separable here, so the call
		// produces all of them and the destinations are the results.
		dsts := make([]Expr, 0, len(f.results))
		for _, r := range f.results {
			b.noteUse(r)
			dsts = append(dsts, ref(r, pos))
		}
		b.emit(&Node{Op: OAssign, Pos: pos, Type: voidType, Args: dsts, Y: b.expr(results[0])})

	default:
		srcs := make([]Expr, 0, len(results))
		for i, e := range results {
			var want types2.Type
			if b.sig != nil && i < nres {
				want = b.sig.Results().At(i).Type()
			}
			v := b.assignConv(b.expr(e), b.typeOf(e), want)
			if len(results) > 1 {
				v = b.snapshot(v)
			}
			srcs = append(srcs, v)
		}
		for i, v := range srcs {
			if i >= len(f.results) {
				b.errorf("ir: a return with %d values inside a range over a function whose enclosing function has %d results", len(results), len(f.results))
				break
			}
			b.noteUse(f.results[i])
			b.emit(Assign(pos, ref(f.results[i], pos), v))
		}
	}

	if f.next == nil {
		// scanRangeFuncBody found no return in this body, so the loop was
		// built without the variable this one needs.
		b.errorf("ir: a return inside a range over a function whose body was built without one")
		return
	}
	b.noteUse(f.next)
	b.emit(Assign(pos, ref(f.next, pos), &Node{
		Op: OConst, Pos: pos, Type: f.intType, Val: Const{Val: constant.MakeInt64(-1)},
	}))
	b.emit(&Node{Op: OReturn, Pos: pos, Type: voidType, Args: []Expr{&Node{
		Op: OConst, Pos: pos, Type: f.boolType, Val: Const{Val: constant.MakeBool(false)},
	}}})
}

// yieldRewriter points the break and continue statements that leave the
// loop at the yield function's own result.
//
// The counts are what says which statement a break and a continue belong
// to. A break the source wrote inside a switch, a select or a nested loop
// belongs to that statement and is left alone; one written at the top of
// the body is the range statement's own and stops the iteration. The two
// counts are separate because a switch and a select take a break and
// leave a continue to the loop around them.
//
// A closure written inside the body is not reached, and needs no check to
// keep it out: the builder has already made it a function of its own, and
// its body is that function's Body rather than a statement list here.
type yieldRewriter struct{ boolType *Type }

func (r yieldRewriter) stmts(list []Stmt, breaks, conts int) []Stmt {
	for i, s := range list {
		if s == nil {
			continue
		}
		switch s.Op {
		case OBreak:
			if breaks == 0 && s.Label == "" {
				list[i] = r.ret(s.Pos, false)
				continue
			}
		case OContinue:
			if conts == 0 && s.Label == "" {
				list[i] = r.ret(s.Pos, true)
				continue
			}
		}
		nb, nc := breaks, conts
		switch s.Op {
		case OFor, ORange:
			nb, nc = breaks+1, conts+1
		case OSwitch, OTypeSwitch, OSelect:
			// A break belongs to a switch and a select; a continue passes
			// through them to the loop around them.
			nb = breaks + 1
		}
		s.Init = r.stmts(s.Init, nb, nc)
		s.Body = r.stmts(s.Body, nb, nc)
		s.Else = r.stmts(s.Else, nb, nc)
		s.Post = r.stmts(s.Post, nb, nc)
	}
	return list
}

// ret is the yield function's answer: true to go on, false to stop.
func (r yieldRewriter) ret(pos syntax.Pos, v bool) Stmt {
	return &Node{Op: OReturn, Pos: pos, Type: voidType, Args: []Expr{&Node{
		Op: OConst, Pos: pos, Type: r.boolType, Val: Const{Val: constant.MakeBool(v)},
	}}}
}

// scanRangeFuncBody reports what in a range-over-func body this row does
// not carry, and whether the body holds a return.
//
// The scan is on the syntax tree rather than on the built statements,
// because the decision has to be made before a function is generated for
// the body, and because a label and the statement it names are plain here
// and resolved away later.
//
// A function literal inside the body is not descended into. A return, a
// defer and a label written in one belong to that literal and say nothing
// about this loop. A nested range over a function is descended into: a
// return there leaves through this body too, so it is this loop that
// needs the variable to report it.
func scanRangeFuncBody(body []syntax.Stmt) (why string, returns bool) {
	for _, st := range body {
		syntax.Inspect(st, func(n syntax.Node) bool {
			if why != "" {
				return false
			}
			switch n := n.(type) {
			case *syntax.FuncLit:
				return false
			case *syntax.LabeledStmt:
				why = "a label"
			case *syntax.ReturnStmt:
				returns = true
			case *syntax.BranchStmt:
				switch {
				case n.Tok == syntax.Goto:
					why = "a goto"
				case n.Label != nil:
					why = "a labelled " + n.Tok.String()
				}
			case *syntax.CallStmt:
				if n.Tok == syntax.Defer {
					why = "a defer"
				}
			}
			return why == ""
		})
		if why != "" {
			return why, returns
		}
	}
	return "", returns
}
