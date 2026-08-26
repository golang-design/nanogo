// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"strings"
	"testing"
)

// TestClosureCtxTypeLayout pins the shape of the closure object.
//
// Two facts and both are correctness. The code pointer is the first word,
// because the closure call of specs/042-arm64-backend.md loads the entry point
// from offset zero. The code pointer is not traced and every capture word is,
// because the first holds a text address the collector must not follow and the
// rest hold heap cells it must.
func TestClosureCtxTypeLayout(t *testing.T) {
	for n := 0; n < 4; n++ {
		ct := closureCtxType(n)
		if want := int64(n+1) * PtrSize; ct.Size != want {
			t.Errorf("a closure object with %d captures is %d bytes, want %d", n, ct.Size, want)
		}
		if ct.Fields[closureCodeField].Offset != 0 {
			t.Errorf("the code pointer of a closure object with %d captures is at offset %d",
				n, ct.Fields[closureCodeField].Offset)
		}
		if bitSet(ct.PtrBits, 0) {
			t.Errorf("the code pointer of a closure object with %d captures is traced, and it holds a text address", n)
		}
		for i := 0; i < n; i++ {
			if !bitSet(ct.PtrBits, int64(closureFirstCapture+i)) {
				t.Errorf("capture %d of a closure object with %d captures is not traced", i, n)
			}
		}
		// The method set is stated and empty. rtype refuses a defined type
		// whose Methods is nil, so an unset field here refuses every closure
		// that captures.
		if ct.Methods == nil {
			t.Errorf("the closure object with %d captures has no method set, and rtype refuses that", n)
		}
		if _, err := TypeSymbol(ct); err != nil {
			t.Errorf("the closure object with %d captures has no descriptor symbol: %v", n, err)
		}
	}
	// Two arities are two types. One name for two layouts would make the
	// linker merge descriptors that describe different objects.
	if closureCtxName(1) == closureCtxName(2) {
		t.Error("two arities share one type name")
	}
}

// TestLowerClosureCapture is the caller half: the closure object is built
// where the literal is written.
func TestLowerClosureCapture(t *testing.T) {
	fn := lowerOK(t, `func f(a int) func() int { return func() int { return a } }`)
	// Two allocations: the cell of a, and the closure object.
	calls := lowerCalls(fn)
	n := 0
	for _, c := range calls {
		if c == "runtime.newobject" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("a literal with one capture made %d allocations, want the cell and the object: %v\n%s",
			n, calls, buildDump(fn))
	}
	if !strings.Contains(buildDump(fn), "*"+closureCtxName(1)) {
		t.Errorf("the closure object is not of the one-capture type:\n%s", buildDump(fn))
	}
	// The parameter is copied into its cell, and every later reference reads
	// the cell. A reference to the parameter that is not the copy would be a
	// read of the frame slot the literal cannot see.
	if got := mentionsParam(fn, "a"); got != 1 {
		t.Errorf("the parameter is named %d times after the rewrite, want once for the copy into the cell:\n%s",
			got, buildDump(fn))
	}
}

// TestLowerCaptureReadsTheContext is the callee half: the literal reads its
// capture off the closure object it was called through.
func TestLowerCaptureReadsTheContext(t *testing.T) {
	pkg, files, info := buildTypecheck(t, lowerPrelude+"\n"+
		`func f(a int) func() int { return func() int { return a } }`)
	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lit := buildFuncOf(t, out, "f.func0")
	if len(lit.Captures) != 1 {
		t.Fatalf("the literal captures %d objects, want one", len(lit.Captures))
	}
	if lit.Closure == nil {
		t.Fatal("the literal has no context parameter")
	}
	if lit.Closure.Type == nil || lit.Closure.Type.Kind != Ptr ||
		lit.Closure.Type.Elem.Name != closureCtxName(1) {
		t.Fatalf("the context parameter is %v, want a pointer to %s", lit.Closure.Type, closureCtxName(1))
	}
	if err := Lower(lit); err != nil {
		t.Fatalf("Lower: %v", err)
	}
	// The capture is read as a field of the object, at the word after the
	// code pointer, and then dereferenced: the word holds the cell.
	found := false
	for _, s := range lit.Body {
		Walk(s, func(n *Node) bool {
			if n.Op != OField || n.X == nil || n.X.Obj != lit.Closure {
				return true
			}
			if n.Index != closureFirstCapture {
				t.Errorf("the one capture is read from field %d, want %d", n.Index, closureFirstCapture)
			}
			found = true
			return true
		})
	}
	if !found {
		t.Errorf("the literal does not read its capture off the context:\n%s", buildDump(lit))
	}
	if mentionsParam(lit, "a") != 0 {
		t.Errorf("the literal still names the captured variable directly:\n%s", buildDump(lit))
	}
}

// TestLowerCaptureCellPerIteration is the Go 1.22 loop variable rule, which
// the cell must not undo.
//
// A variable declared in the body of a loop is a fresh variable on every
// iteration, so its cell is allocated in the loop body. One cell allocated at
// the entry would be one variable for every iteration, and every literal made
// in the loop would read the last one's value.
func TestLowerCaptureCellPerIteration(t *testing.T) {
	fn := lowerOK(t, `func f() { for i := 0; i < 3; i++ { x := i; sink(func() int { return x }) } }`)
	loops := buildFind(fn, OFor)
	if len(loops) != 1 {
		t.Fatalf("%d loops, want one", len(loops))
	}
	inLoop := 0
	for _, s := range loops[0].Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OCall && n.X != nil && n.X.Obj != nil && n.X.Obj.Name == "runtime.newobject" {
				inLoop++
			}
			return true
		})
	}
	// The cell and the closure object, both per iteration.
	if inLoop != 2 {
		t.Errorf("%d allocations in the loop body, want the cell and the closure object:\n%s",
			inLoop, buildDump(fn))
	}
}

// TestLowerNestedCapture is the rule that makes two levels of literal work: a
// function reaches the cell of a variable it captured through its own context,
// and passes that pointer on unchanged.
func TestLowerNestedCapture(t *testing.T) {
	pkg, files, info := buildTypecheck(t, lowerPrelude+"\n"+
		`func f(a int) func() func() int { return func() func() int { return func() int { return a } } }`)
	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mid := buildFuncOf(t, out, "f.func0")
	if len(mid.Captures) != 1 {
		t.Fatalf("the outer literal captures %d objects, want one", len(mid.Captures))
	}
	if err := Lower(mid); err != nil {
		t.Fatalf("Lower: %v", err)
	}
	// The middle literal allocates the inner closure object and no cell: the
	// cell belongs to f, and the middle literal only passes the pointer on.
	n := 0
	for _, c := range lowerCalls(mid) {
		if c == "runtime.newobject" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the middle literal made %d allocations, want only the inner closure object:\n%s",
			n, buildDump(mid))
	}
	// The capture word it writes is the word it read out of its own context,
	// with no load in between: two levels of literal read one variable.
	if strings.Count(buildDump(mid), "deref") != 0 {
		t.Errorf("the middle literal loads the captured variable it only passes on:\n%s", buildDump(mid))
	}
}

// TestLowerRefusesACapturedResult names the one capture that is not built.
//
// The epilogue of a function that defers reads the named results out of the
// frame, and deferExit builds that epilogue after the capture rewrite has run.
// A result moved to a cell would leave the epilogue reading the slot the cell
// replaced.
func TestLowerRefusesACapturedResult(t *testing.T) {
	_, err := lowerFunc(t, `func f() (n int) { defer func() { n = 7 }(); return 1 }`, "f")
	if err == nil {
		t.Fatal("a captured named result was lowered")
	}
	if !strings.Contains(err.Error(), "captures the result") {
		t.Errorf("the refusal is %q, and it does not name the result", err)
	}
}

// mentionsParam counts the references to a parameter or local by name.
func mentionsParam(fn *Func, name string) int {
	count := 0
	for _, s := range fn.Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OLocal && n.Obj != nil && n.Obj.Name == name {
				count++
			}
			return true
		})
	}
	return count
}

// TestLowerCaptureCellOutsideALoopItsConditionNames is the other half of the
// per-iteration rule.
//
// A variable the loop's condition names is one variable for the whole loop,
// not one per iteration, so its cell is allocated in front of the loop. The
// rule is the innermost list that holds *every* mention: the condition is a
// mention and it is not in the body.
func TestLowerCaptureCellOutsideALoopItsConditionNames(t *testing.T) {
	fn := lowerOK(t, `func f() { i := 0; for i < 3 { i = i + 1; sink(func() int { return i }) } }`)
	loops := buildFind(fn, OFor)
	if len(loops) != 1 {
		t.Fatalf("%d loops, want one", len(loops))
	}
	inLoop := 0
	for _, s := range loops[0].Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OCall && n.X != nil && n.X.Obj != nil && n.X.Obj.Name == "runtime.newobject" {
				inLoop++
			}
			return true
		})
	}
	// Only the closure object, which is per iteration. The cell is not.
	if inLoop != 1 {
		t.Errorf("%d allocations in the loop body, want only the closure object:\n%s",
			inLoop, buildDump(fn))
	}
}

// TestLowerCaptureCellForSplitMentions checks where the cell of a variable
// whose mentions are in two branches of one statement is allocated.
//
// Neither branch holds every mention, so neither is where the cell belongs:
// one path would allocate and the other would read a nil pointer. The rule is
// the innermost statement list that holds *every* mention, which here is the
// list the branch statement itself is in.
func TestLowerCaptureCellForSplitMentions(t *testing.T) {
	fn := lowerOK(t, `func f(c bool) {
		for i := 0; i < 3; i++ {
			x := 0
			if c {
				x = 1
			} else {
				sink(func() int { return x })
			}
			use(x)
		}
	}`)
	loops := buildFind(fn, OFor)
	if len(loops) != 1 {
		t.Fatalf("%d loops, want one", len(loops))
	}
	// The cell is allocated in the loop body, which is where the variable is
	// declared, and not inside either branch.
	top := 0
	for _, s := range loops[0].Body {
		if s.Op == OAssign && s.X != nil && s.X.Op == OLocal &&
			s.X.Obj != nil && strings.HasPrefix(s.X.Obj.Name, ".cell_") {
			top++
		}
	}
	if top != 1 {
		t.Errorf("%d cell allocations at the top of the loop body, want one:\n%s", top, buildDump(fn))
	}
	ifs := buildFind(fn, OIf)
	if len(ifs) == 0 {
		t.Fatal("the if statement is gone")
	}
	for _, list := range [][]Stmt{ifs[0].Body, ifs[0].Else} {
		for _, s := range list {
			if s.Op == OAssign && s.X != nil && s.X.Op == OLocal &&
				s.X.Obj != nil && strings.HasPrefix(s.X.Obj.Name, ".cell_") {
				t.Errorf("a cell is allocated inside a branch:\n%s", buildDump(fn))
			}
		}
	}
}
