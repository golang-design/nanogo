// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"strings"
	"testing"
)

// rangeFuncPrelude is a package with iterators of each arity in it.
//
// The iterators have bodies, because a range over a function is a call and a
// call needs something to call. What the bodies do is not read here: this file
// is about the shape of the tree, and internal/e2e/rangefunc_test.go is about
// what the program computes.
const rangeFuncPrelude = `package p

func sink(v ...any) {}

func seq0(yield func() bool)            {}
func seq1(yield func(int) bool)         {}
func seq2(yield func(int, string) bool) {}

// Seq is a defined type whose underlying type is an iterator's, which is what
// iter.Seq and iter.Seq2 are and what every range over one reads through.
type Seq func(func(int) bool)

var seqValue Seq
`

// rangeFuncBuild builds a snippet and returns the package, or the error the
// builder reported.
func rangeFuncBuild(t *testing.T, body string) (*Package, error) {
	t.Helper()
	pkg, files, info := buildTypecheck(t, rangeFuncPrelude+"\n"+body)
	return Build(pkg, files, info)
}

// rangeFuncPackage builds a snippet and fails when the builder refused it.
func rangeFuncPackage(t *testing.T, body string) *Package {
	t.Helper()
	p, err := rangeFuncBuild(t, body)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return p
}

// TestRangeFuncBecomesACall is the row: the loop is gone and a call taking the
// body as a closure is in its place.
func TestRangeFuncBecomesACall(t *testing.T) {
	p := rangeFuncPackage(t, `func f() { for v := range seq1 { sink(v) } }`)
	fn := buildFuncOf(t, p, "f")
	if got := buildFind(fn, ORange); len(got) != 0 {
		t.Fatalf("the range survived:\n%s", buildDump(fn))
	}
	call := buildFirst(t, fn, OCall)
	if call.X == nil || call.X.Obj == nil || call.X.Obj.Name != "p.seq1" {
		t.Fatalf("the call is not to the iterator:\n%s", buildDump(fn))
	}
	if len(call.Args) != 1 || call.Args[0].Op != OClosure {
		t.Fatalf("the iterator is not called with a closure:\n%s", buildDump(fn))
	}

	// The body is a function of the package, with the yield function's
	// signature and not the clause's.
	lit := buildFuncOf(t, p, "f.func1")
	if len(lit.Params) != 1 || lit.Params[0].Class != ClassParam {
		t.Fatalf("the yield function takes %d parameters:\n%s", len(lit.Params), buildDump(lit))
	}
	if len(lit.Results) != 1 || lit.Results[0].Type.Kind != Bool {
		t.Fatalf("the yield function does not return one bool:\n%s", buildDump(lit))
	}
	if !endsInReturn(lit, true) {
		t.Errorf("the body does not end in return true:\n%s", buildDump(lit))
	}
}

// TestRangeFuncParametersComeFromTheYieldFunction is the case where the clause
// names fewer variables than the iterator hands out.
//
// The parameters are the yield function's, so a loop with no variables over a
// two-value iterator still takes two. A literal built with the clause's count
// would be a function value of a type the iterator cannot call, and nothing
// between here and the call would say so.
func TestRangeFuncParametersComeFromTheYieldFunction(t *testing.T) {
	for _, tc := range []struct {
		what  string
		body  string
		nparm int
	}{
		{"no variables over a two-value iterator", `func f() { for range seq2 { sink(1) } }`, 2},
		{"one variable over a two-value iterator", `func f() { for k := range seq2 { sink(k) } }`, 2},
		{"no variables over a no-value iterator", `func f() { for range seq0 { sink(1) } }`, 0},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := rangeFuncPackage(t, tc.body)
			lit := buildFuncOf(t, p, "f.func1")
			if len(lit.Params) != tc.nparm {
				t.Fatalf("the yield function takes %d parameters, want %d:\n%s", len(lit.Params), tc.nparm, buildDump(lit))
			}
		})
	}
}

// TestRangeFuncBreakAndContinueAreTheResult is the control flow the row
// carries without any variable: break stops the iteration and continue asks
// for the next one.
func TestRangeFuncBreakAndContinueAreTheResult(t *testing.T) {
	p := rangeFuncPackage(t, `func f() { for v := range seq1 { if v == 1 { break }; if v == 2 { continue }; sink(v) } }`)
	lit := buildFuncOf(t, p, "f.func1")
	rets := buildFind(lit, OReturn)
	// Three: the break, the continue and the end of the body.
	if len(rets) != 3 {
		t.Fatalf("the body has %d returns, want 3:\n%s", len(rets), buildDump(lit))
	}
	want := []string{"false", "true", "true"}
	for i, r := range rets {
		if len(r.Args) != 1 || r.Args[0].Val == nil || r.Args[0].Val.String() != want[i] {
			t.Fatalf("return %d is not %s:\n%s", i, want[i], buildDump(lit))
		}
	}
	if len(buildFind(lit, OBreak))+len(buildFind(lit, OContinue)) != 0 {
		t.Errorf("a break or a continue survived:\n%s", buildDump(lit))
	}
	// Nothing was added to the frame that holds the loop: a body with no
	// return needs no variable to report one through.
	fn := buildFuncOf(t, p, "f")
	if o := findLocal(fn, ".rangefunc_next"); o != nil {
		t.Errorf("a body with no return was given a .rangefunc_next:\n%s", buildDump(fn))
	}
}

// TestRangeFuncKeepsABreakThatBelongsElsewhere is the other half of the same
// rule.
//
// A break the source wrote inside a switch, a select, a type switch or an
// ordinary loop belongs to that statement. Rewriting it into "return false"
// would leave the statement it belongs to and stop the iteration as well,
// which is a wrong answer that compiles.
func TestRangeFuncKeepsABreakThatBelongsElsewhere(t *testing.T) {
	for _, tc := range []struct{ what, body string }{
		{"a switch", `func f() { for v := range seq1 { switch v { case 1: break } } }`},
		{"a type switch", `func f(a any) { for range seq1 { switch a.(type) { case int: break } } }`},
		{"a select", `func f(c chan int) { for range seq1 { select { case <-c: break } } }`},
		{"an ordinary loop", `func f() { for range seq1 { for i := 0; i < 2; i++ { break } } }`},
		{"a loop's continue", `func f() { for range seq1 { for i := 0; i < 2; i++ { continue } } }`},
		{"a nested range", `func f(s []int) { for range seq1 { for range s { break } } }`},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := rangeFuncPackage(t, tc.body)
			lit := buildFuncOf(t, p, "f.func1")
			if len(buildFind(lit, OBreak))+len(buildFind(lit, OContinue)) != 1 {
				t.Fatalf("the branch statement did not stay where the source wrote it:\n%s", buildDump(lit))
			}
			// The only return is the one at the end of the body.
			if got := buildFind(lit, OReturn); len(got) != 1 {
				t.Fatalf("the body has %d returns, want the one at its end:\n%s", len(got), buildDump(lit))
			}
		})
	}
}

// TestRangeFuncReturnCrossesTwoFrames is the shape the row exists to get
// right.
//
// A return inside the body writes the enclosing function's results, sets the
// variable that says a return happened, and stops the iteration; the frame
// that called the iterator tests the variable and returns. A return built as
// the yield function's own return would leave the loop and go on with the next
// statement, which is a wrong answer with no diagnostic.
func TestRangeFuncReturnCrossesTwoFrames(t *testing.T) {
	p := rangeFuncPackage(t, `func f() (int, string) { for v := range seq1 { return v, "x" }; return 0, "" }`)
	fn := buildFuncOf(t, p, "f")
	lit := buildFuncOf(t, p, "f.func1")

	next := findLocal(fn, ".rangefunc_next")
	if next == nil {
		t.Fatalf("the frame that holds the loop has no .rangefunc_next:\n%s", buildDump(fn))
	}
	// The body captures the variable and both results, so that what it
	// writes is what the frame around the loop reads.
	if !captures(lit, next) {
		t.Errorf("the body does not capture .rangefunc_next:\n%s", buildDump(lit))
	}
	for _, r := range fn.Results {
		if !captures(lit, r) {
			t.Errorf("the body does not capture the result %q:\n%s", r.Name, buildDump(lit))
		}
	}
	// The body writes both results, sets the variable and returns false.
	body := lit.Body
	last := body[len(body)-2]
	if last.Op != OReturn || len(last.Args) != 1 || last.Args[0].Val.String() != "false" {
		t.Fatalf("the return does not stop the iteration:\n%s", buildDump(lit))
	}
	if n := countAssignsTo(lit, next); n != 1 {
		t.Errorf("the body writes .rangefunc_next %d times, want once:\n%s", n, buildDump(lit))
	}
	for _, r := range fn.Results {
		if n := countAssignsTo(lit, r); n != 1 {
			t.Errorf("the body writes the result %q %d times, want once:\n%s", r.Name, n, buildDump(lit))
		}
	}

	// The frame around the loop zeroes the variable before the call and
	// returns after it. Zeroed before the call and not at the declaration,
	// because the loop may be inside another loop and reach the statement
	// again.
	if n := countAssignsTo(fn, next); n != 1 {
		t.Fatalf("the frame writes .rangefunc_next %d times, want the zero before the call:\n%s", n, buildDump(fn))
	}
	tests := buildFind(fn, OIf)
	if len(tests) != 1 || len(tests[0].Body) != 1 || tests[0].Body[0].Op != OReturn || len(tests[0].Body[0].Args) != 0 {
		t.Fatalf("the frame does not return on the test after the call:\n%s", buildDump(fn))
	}
}

// TestRangeFuncOverANamedType is the operand every standard-library iterator
// has: a defined type whose underlying type is the function.
//
// iter.Seq and iter.Seq2 are defined types, and `types2`'s own `typeset`
// returns one. A row that read the operand's type rather than its core type
// would refuse every one of them.
func TestRangeFuncOverANamedType(t *testing.T) {
	p := rangeFuncPackage(t, `func f() { for v := range seqValue { sink(v) } }`)
	fn := buildFuncOf(t, p, "f")
	if got := buildFind(fn, ORange); len(got) != 0 {
		t.Fatalf("the range over a defined iterator type survived:\n%s", buildDump(fn))
	}
	lit := buildFuncOf(t, p, "f.func1")
	if len(lit.Params) != 1 || lit.Params[0].Type.Kind != Int64 {
		t.Fatalf("the yield function's parameter is not the element type:\n%s", buildDump(lit))
	}
}

// TestRangeFuncReturnFromAFunctionWithNoResults is the return that has nothing
// to carry.
//
// The frame that holds the loop returns with no result storage to copy out, so
// the report through .rangefunc_next is the whole of what crosses the two
// frames. It is the one path where the body writes the variable and nothing
// else.
func TestRangeFuncReturnFromAFunctionWithNoResults(t *testing.T) {
	p := rangeFuncPackage(t, `func f() { for v := range seq1 { sink(v); if v == 1 { return } } }`)
	fn := buildFuncOf(t, p, "f")
	lit := buildFuncOf(t, p, "f.func1")
	if len(fn.Results) != 0 {
		t.Fatalf("f has %d results", len(fn.Results))
	}
	next := findLocal(fn, ".rangefunc_next")
	if next == nil {
		t.Fatalf("the frame that holds the loop has no .rangefunc_next:\n%s", buildDump(fn))
	}
	if n := countAssignsTo(lit, next); n != 1 {
		t.Errorf("the body writes .rangefunc_next %d times, want once:\n%s", n, buildDump(lit))
	}
	tests := buildFind(fn, OIf)
	if len(tests) != 1 || len(tests[0].Body) != 1 ||
		tests[0].Body[0].Op != OReturn || len(tests[0].Body[0].Args) != 0 {
		t.Fatalf("the frame does not return on the test after the call:\n%s", buildDump(fn))
	}
}

// TestRangeFuncBareReturn is a return with no values, which leaves the named
// results as the body last wrote them.
func TestRangeFuncBareReturn(t *testing.T) {
	p := rangeFuncPackage(t, `func f() (n int) { for v := range seq1 { n = v; return }; return }`)
	fn := buildFuncOf(t, p, "f")
	lit := buildFuncOf(t, p, "f.func1")
	next := findLocal(fn, ".rangefunc_next")
	if next == nil {
		t.Fatalf("the frame that holds the loop has no .rangefunc_next:\n%s", buildDump(fn))
	}
	// The result is written by the body's own assignment and never by the
	// return, which has no values to write.
	if n := countAssignsTo(lit, fn.Results[0]); n != 1 {
		t.Errorf("the body writes the result %d times, want the one the source wrote:\n%s", n, buildDump(lit))
	}
	if n := countAssignsTo(lit, next); n != 1 {
		t.Errorf("the body writes .rangefunc_next %d times, want once:\n%s", n, buildDump(lit))
	}
}

// TestRangeFuncReturnOfACall is "return g()", where one call produces every
// result the enclosing function returns.
func TestRangeFuncReturnOfCall(t *testing.T) {
	p := rangeFuncPackage(t, `
func two() (int, string) { return 1, "x" }

func f() (int, string) { for range seq1 { return two() }; return 0, "" }`)
	fn := buildFuncOf(t, p, "f")
	lit := buildFuncOf(t, p, "f.func1")
	// One assignment with both results as its destinations, because the call
	// produces both before either is stored.
	found := false
	for _, s := range lit.Body {
		if s.Op == OAssign && len(s.Args) == 2 && s.Y != nil && s.Y.Op == OCall {
			found = true
		}
	}
	if !found {
		t.Fatalf("the return of a call does not write both results at once:\n%s", buildDump(lit))
	}
	for _, r := range fn.Results {
		if !captures(lit, r) {
			t.Errorf("the body does not capture the result %q:\n%s", r.Name, buildDump(lit))
		}
	}
}

// TestRangeFuncNestedReturnPropagates is the nesting rule.
//
// A return inside the body of an inner loop has to stop both iterations. The
// inner body reports it through the inner loop's own variable, and the test
// after the inner call is built inside the outer body, where "return" is
// already the rewrite: it reports through the outer variable and stops the
// outer iteration.
func TestRangeFuncNestedReturnPropagates(t *testing.T) {
	p := rangeFuncPackage(t, `func f() int { for a := range seq1 { for b := range seq1 { return a + b } }; return 0 }`)
	fn := buildFuncOf(t, p, "f")
	outer := buildFuncOf(t, p, "f.func1")
	inner := buildFuncOf(t, p, "f.func1.1")

	fnNext := findLocal(fn, ".rangefunc_next")
	outerNext := findLocal(outer, ".rangefunc_next")
	if fnNext == nil || outerNext == nil {
		t.Fatalf("a level has no .rangefunc_next:\n%s\n%s", buildDump(fn), buildDump(outer))
	}
	if fnNext == outerNext {
		t.Fatal("the two loops share one .rangefunc_next; each loop declares its own")
	}
	// The inner body reports through the outer loop's variable, which it
	// owns, and returns from the enclosing function's results.
	if !captures(inner, outerNext) {
		t.Errorf("the inner body does not capture the outer loop's .rangefunc_next:\n%s", buildDump(inner))
	}
	if !captures(inner, fn.Results[0]) {
		t.Errorf("the inner body does not capture the result:\n%s", buildDump(inner))
	}
	// The outer body tests the inner variable after the inner call, and what
	// it does with the answer is its own return: report and stop.
	tests := buildFind(outer, OIf)
	if len(tests) != 1 {
		t.Fatalf("the outer body has %d tests after the inner call, want one:\n%s", len(tests), buildDump(outer))
	}
	prop := tests[0].Body
	if len(prop) != 2 || prop[0].Op != OAssign || prop[1].Op != OReturn ||
		len(prop[1].Args) != 1 || prop[1].Args[0].Val.String() != "false" {
		t.Fatalf("the outer body does not report and stop:\n%s", buildDump(outer))
	}
	if prop[0].X == nil || prop[0].X.Obj != fnNext {
		t.Errorf("the outer body reports through the wrong variable:\n%s", buildDump(outer))
	}
}

// TestRangeFuncLiteralInBodyKeepsItsOwnReturn is the reason the frame is put
// away when a function literal is built.
//
// A return the source wrote inside a literal returns from that literal, even
// where the literal is written inside a range-over-func body. Rewritten as the
// loop's return it would return from two functions out and stop an iteration
// the literal is not part of.
func TestRangeFuncLiteralInBodyKeepsItsOwnReturn(t *testing.T) {
	p := rangeFuncPackage(t, `func f() int { for v := range seq1 { g := func() int { return v }; sink(g) }; return 0 }`)
	fn := buildFuncOf(t, p, "f")
	lit := buildFuncOf(t, p, "f.func1.1")
	rets := buildFind(lit, OReturn)
	if len(rets) != 1 || len(rets[0].Args) != 1 || rets[0].Args[0].Op != OLocal {
		t.Fatalf("the literal's return is not its own:\n%s", buildDump(lit))
	}
	// The loop needed no variable: the only return in the body belongs to the
	// literal, which the scan does not descend into.
	if o := findLocal(fn, ".rangefunc_next"); o != nil {
		t.Errorf("a return inside a literal was counted as the body's:\n%s", buildDump(fn))
	}
}

// TestRangeFuncAssignsToTheVariableAroundTheLoop is the "= range" form, whose
// destination belongs to the frame that holds the loop.
func TestRangeFuncAssignsToTheVariableAroundTheLoop(t *testing.T) {
	p := rangeFuncPackage(t, `func f() int { v := 0; for v = range seq1 { sink(v) }; return v }`)
	fn := buildFuncOf(t, p, "f")
	lit := buildFuncOf(t, p, "f.func1")
	v := findLocal(fn, "v")
	if v == nil {
		t.Fatalf("f has no v:\n%s", buildDump(fn))
	}
	if !captures(lit, v) {
		t.Fatalf("the body does not capture the variable it assigns:\n%s", buildDump(lit))
	}
	first := lit.Body[0]
	if first.Op != OAssign || first.Op1 == defineOp || first.X == nil || first.X.Obj != v {
		t.Fatalf("the body does not assign the variable at its head:\n%s", buildDump(lit))
	}
}

// TestRangeFuncBlankVariableIsNotWritten is the blank identifier, which names
// no storage.
func TestRangeFuncBlankVariableIsNotWritten(t *testing.T) {
	p := rangeFuncPackage(t, `func f() { for _, v := range seq2 { sink(v) } }`)
	lit := buildFuncOf(t, p, "f.func1")
	if len(lit.Params) != 2 {
		t.Fatalf("the yield function takes %d parameters, want 2:\n%s", len(lit.Params), buildDump(lit))
	}
	first := lit.Body[0]
	if first.Op != OAssign || first.Y == nil || first.Y.Obj != lit.Params[1] {
		t.Fatalf("the body's first statement is not the copy of the second parameter:\n%s", buildDump(lit))
	}
}

// TestRangeFuncRefusals is the other half of the row: the shapes it does not
// carry, refused by name.
//
// Each of these leaves the body for somewhere the yield function cannot name,
// and each would be a silent wrong answer if it were approximated: a labelled
// break that stopped only the innermost loop, a goto that jumped inside the
// generated function, a defer that ran at the end of the iteration rather than
// at the end of the function the source wrote it in.
func TestRangeFuncRefusals(t *testing.T) {
	for _, tc := range []struct{ what, body, want string }{
		{"a label", `func f() { for range seq1 { L: sink(1); goto L } }`, "a label"},
		{"a goto out", `func f() { for range seq1 { goto L }; L: sink(1) }`, "a goto"},
		{"a labelled break", `func f() { L: for range seq1 { break L } }`, "a labelled break"},
		{"a labelled continue", `func f() { L: for range seq1 { continue L } }`, "a labelled continue"},
		{"a defer", `func f() { for range seq1 { defer sink(1) } }`, "a defer"},
		{"a defer in a nested loop", `func f(s []int) { for range seq1 { for range s { defer sink(1) } } }`, "a defer"},
		{"a defer in a nested range over a function", `func f() { for range seq1 { for range seq1 { defer sink(1) } } }`, "a defer"},
		{"an index destination", `func f(a []int) { for a[0] = range seq1 { sink(1) } }`, "assigning to"},
		{"a pointer destination", `func f(p *int) { for *p = range seq1 { sink(1) } }`, "assigning to "},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p, err := rangeFuncBuild(t, tc.body)
			if err == nil {
				t.Fatalf("the shape was built")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %q, want it to name %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "specs/020-ir.md") {
				t.Errorf("the refusal %q names no spec", err)
			}
			// The statement is still in the tree. A refusal that dropped
			// the loop would leave a function that reads as a function
			// with nothing wrong with it, and ir/lower.go would have
			// nothing left to refuse a second time.
			fn := buildFuncOf(t, p, "f")
			if got := buildFind(fn, ORange); len(got) == 0 {
				t.Fatalf("the refused statement is gone from the tree:\n%s", buildDump(fn))
			}
			if lerr := Lower(fn); lerr == nil || !strings.Contains(lerr.Error(), "a range over func") {
				t.Errorf("lowering the refused tree said %v, want it to refuse the range as well", lerr)
			}
		})
	}
}

// TestRangeFuncLabelOnTheLoopIsNotRefused is the label that is not in the
// body.
//
// A label on the range statement itself is refused only where something inside
// the body names it, which the row refuses on its own terms. The statement
// becomes several, so what this pins is that the label still covers all of
// them: a goto to it has to reach the closure construction and the call, and
// one that landed after them would call the iterator with an object nothing
// built.
func TestRangeFuncLabelOnTheLoopIsNotRefused(t *testing.T) {
	p := rangeFuncPackage(t, `
func f() int {
	n := 0
L:
	for v := range seq1 {
		n = n + v
	}
	if n == 0 {
		goto L
	}
	return n
}`)
	fn := buildFuncOf(t, p, "f")
	labels := buildFind(fn, OLabel)
	if len(labels) != 1 {
		t.Fatalf("f has %d labels, want one:\n%s", len(labels), buildDump(fn))
	}
	var call, closure bool
	for _, s := range labels[0].Body {
		Walk(s, func(n *Node) bool {
			switch n.Op {
			case OCall:
				call = true
			case OClosure:
				closure = true
			}
			return true
		})
	}
	if !call || !closure {
		t.Fatalf("the label does not cover the call and the closure:\n%s", buildDump(fn))
	}
}

// TestRangeFuncEvaluatesTheIteratorOnce is what the specification requires of
// every range expression.
func TestRangeFuncEvaluatesTheIteratorOnce(t *testing.T) {
	p := rangeFuncPackage(t, `func f(g func() func(func(int) bool)) { for v := range g() { sink(v) } }`)
	fn := buildFuncOf(t, p, "f")
	calls := buildFind(fn, OCall)
	// Two: the call to g and the call to what it returned.
	if len(calls) != 2 {
		t.Fatalf("f makes %d calls, want the one to g and the one to its result:\n%s", len(calls), buildDump(fn))
	}
	if calls[0].X == nil || calls[0].X.Op != OCall {
		t.Fatalf("the iterator is not the value g returned:\n%s", buildDump(fn))
	}
}

// TestRangeFuncFallthroughIsNotRefused is the branch statement that is not a
// way out.
//
// A fallthrough goes to the next clause of the switch it is in, which is
// inside the body, so it is left alone. A scan that refused every branch
// statement carrying a target would refuse it too.
func TestRangeFuncFallthroughIsNotRefused(t *testing.T) {
	p := rangeFuncPackage(t, `func f() { for v := range seq1 { switch v { case 1: fallthrough; case 2: sink(v) } } }`)
	lit := buildFuncOf(t, p, "f.func1")
	if len(buildFind(lit, OGoto)) != 1 {
		t.Fatalf("the fallthrough did not survive:\n%s", buildDump(lit))
	}
}

// TestRangeFuncYieldReadsTheOperand is the test that decides which row a range
// statement takes.
//
// A function whose parameter count is not one, a function whose one parameter
// is not a function, and a function whose yield does not return exactly one
// value are none of them range-over-func operands. The checker rejects every
// one of those before the builder sees it, so what is asserted here is that
// this classifier says so: an operand that is nearly an iterator has to take
// the ORange path and be refused by name rather than the rewrite path, where a
// literal would be built with a signature its caller cannot call.
func TestRangeFuncYieldReadsTheOperand(t *testing.T) {
	pkg, files, info := buildTypecheck(t, rangeFuncPrelude+`
var notAFunction []int
var noYield func(int)
var twoParams func(func(int) bool, int)
var noResult func(func(int))
var twoResults func(func(int) (bool, bool))
var iterReturns func(func(int) bool) int

func f(a []int, m map[int]int, c chan int, s string, n int) {
	for range a { }
	for range m { }
	for range c { }
	for range s { }
	for range n { }
}`)
	p, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The other five rows are still an ORange, so the sixth left them alone.
	fn := buildFuncOf(t, p, "f")
	if got := buildFind(fn, ORange); len(got) != 5 {
		t.Fatalf("%d of the five other range rows survived as an ORange:\n%s", len(got), buildDump(fn))
	}
	if rangeFuncYield(nil) != nil {
		t.Error("a nil operand is a range over a function")
	}
	for _, name := range []string{"notAFunction", "noYield", "twoParams", "noResult", "twoResults", "iterReturns"} {
		o := pkg.Scope().Lookup(name)
		if o == nil {
			t.Fatalf("the package has no %s", name)
		}
		if got := rangeFuncYield(o.Type()); got != nil {
			t.Errorf("%s is read as an iterator whose yield function is %s", name, got)
		}
	}
	// And the shapes that are iterators, so that a classifier answering nil
	// to everything would fail here rather than pass everywhere.
	for _, name := range []string{"seq0", "seq1", "seq2"} {
		o := pkg.Scope().Lookup(name)
		if o == nil {
			t.Fatalf("the package has no %s", name)
		}
		if rangeFuncYield(o.Type()) == nil {
			t.Errorf("%s is not read as an iterator", name)
		}
	}
}

// endsInReturn reports whether the last statement of a function is a return
// of one boolean constant with the wanted value.
func endsInReturn(fn *Func, want bool) bool {
	if len(fn.Body) == 0 {
		return false
	}
	last := fn.Body[len(fn.Body)-1]
	if last.Op != OReturn || len(last.Args) != 1 || last.Args[0].Val == nil {
		return false
	}
	return last.Args[0].Val.String() == "true" == want
}

// findLocal returns the local of a function with a name, or nil.
func findLocal(fn *Func, name string) *Object {
	for _, o := range fn.Locals {
		if o.Name == name {
			return o
		}
	}
	return nil
}

// captures reports whether a function captures an object.
func captures(fn *Func, o *Object) bool {
	for _, c := range fn.Captures {
		if c == o {
			return true
		}
	}
	return false
}

// countAssignsTo counts the assignments in a function whose destination is an
// object.
func countAssignsTo(fn *Func, o *Object) int {
	n := 0
	for _, s := range fn.Body {
		Walk(s, func(m *Node) bool {
			if m.Op != OAssign {
				return true
			}
			if m.X != nil && m.X.Obj == o {
				n++
			}
			for _, a := range m.Args {
				if a != nil && a.Obj == o {
					n++
				}
			}
			return true
		})
	}
	return n
}
