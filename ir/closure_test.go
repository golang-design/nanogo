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
	lit := buildFuncOf(t, out, "f.func1")
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
	mid := buildFuncOf(t, out, "f.func1")
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

// exitTail returns the statements of the single exit, which are the label and
// everything after it.
func exitTail(t *testing.T, fn *Func) []Stmt {
	t.Helper()
	for i, s := range fn.Body {
		if s != nil && s.Op == OLabel {
			return fn.Body[i:]
		}
	}
	t.Fatalf("%s has no exit label:\n%s", fn.Name, buildDump(fn))
	return nil
}

// cellOfResult returns the cell of a named result, or nil.
func cellOfResult(fn *Func, name string) *Object {
	for _, o := range fn.Locals {
		if o.Name == ".cell_"+name {
			return o
		}
	}
	return nil
}

// isCellStore reports whether s writes through the cell of a named result
// rather than into the result object.
func isCellStore(s Stmt, cell *Object) bool {
	return s != nil && s.Op == OAssign && s.X != nil && s.X.Op == ODeref &&
		s.X.X != nil && s.X.X.Op == OLocal && s.X.X.Obj == cell
}

// TestLowerCapturesANamedResultThroughACell is the capture the single exit
// used to refuse.
//
// A named result is a variable the language shares with a literal, so it gets
// a heap cell like any other capture. What is different is that the result
// object is also the storage the ABI returns, so the return writes the cell
// and the exit copies the cell into the result object. The copy is after the
// call to runtime.deferreturn, because a deferred function may assign the
// result and what it assigns is the cell.
func TestLowerCapturesANamedResultThroughACell(t *testing.T) {
	fn := lowerOK(t, `func f() (n int) { defer func() { n = 7 }(); return 1 }`)
	cell := cellOfResult(fn, "n")
	if cell == nil {
		t.Fatalf("the captured result has no cell; the locals are %v", localNames(fn))
	}
	// The return writes the cell. A write to the result object instead would
	// be a value the deferred function cannot see.
	stores := 0
	for _, s := range fn.Body {
		if isCellStore(s, cell) {
			stores++
		}
	}
	if stores != 1 {
		t.Errorf("the return wrote the cell %d times, want once:\n%s", stores, buildDump(fn))
	}
	tail := exitTail(t, fn)
	if len(tail) != 4 {
		t.Fatalf("the exit is %d statements, want the label, deferreturn, the copy and the return:\n%s", len(tail), buildDump(fn))
	}
	if tail[1].Op != OCall || tail[1].X == nil || tail[1].X.Obj == nil || tail[1].X.Obj.Name != "runtime.deferreturn" {
		t.Fatalf("the statement after the label is not the deferreturn call:\n%s", buildDump(fn))
	}
	copyStmt := tail[2]
	if copyStmt.Op != OAssign || copyStmt.X == nil || copyStmt.X.Op != OLocal ||
		copyStmt.X.Obj == nil || copyStmt.X.Obj.Class != ClassResult {
		t.Fatalf("the exit does not copy into the result object:\n%s", buildDump(fn))
	}
	if copyStmt.Y == nil || copyStmt.Y.Op != ODeref || copyStmt.Y.X == nil || copyStmt.Y.X.Obj != cell {
		t.Fatalf("the exit does not read the result out of the cell:\n%s", buildDump(fn))
	}
	if tail[3].Op != OReturn {
		t.Fatalf("the exit does not end in a return:\n%s", buildDump(fn))
	}
}

// TestLowerCapturedResultsStayInTheFrame keeps the rule the epilogue depends
// on when a recover resumes it.
//
// A result is assigned at every return, so it is a phi at the epilogue, and
// the copies a phi resolves into sit at the end of the predecessor blocks.
// runtime.recovery jumps straight to the deferreturn call and skips them, so
// the result has to be read from storage instead.
func TestLowerCapturedResultsStayInTheFrame(t *testing.T) {
	fn := lowerOK(t, `func f() (n int) { defer func() { n = 7 }(); return 1 }`)
	for _, r := range fn.Results {
		if !r.Addrtaken {
			t.Errorf("the result %s is not address-taken, so the epilogue may read a phi whose edge copies a recover skipped", r.Name)
		}
	}
}

// TestLowerGivesACapturedResultAnExitWithoutADefer is the same join in a
// function that defers nothing.
//
// "return x" assigns the named result, and a literal that outlives the frame
// reads the cell afterwards, so the return has to write the cell there too and
// the ABI result has to be read back out of it. The exit is the same one, with
// no call to runtime.deferreturn in it.
func TestLowerGivesACapturedResultAnExitWithoutADefer(t *testing.T) {
	fn := lowerOK(t, `func f() (n int) { sink(func() int { n = 7; return n }); return 1 }`)
	cell := cellOfResult(fn, "n")
	if cell == nil {
		t.Fatalf("the captured result has no cell; the locals are %v", localNames(fn))
	}
	tail := exitTail(t, fn)
	if len(tail) != 3 {
		t.Fatalf("the exit is %d statements, want the label, the copy and the return:\n%s", len(tail), buildDump(fn))
	}
	for _, s := range fn.Body {
		if s != nil && s.Op == OCall && s.X != nil && s.X.Obj != nil && s.X.Obj.Name == "runtime.deferreturn" {
			t.Fatalf("a function that defers nothing calls deferreturn:\n%s", buildDump(fn))
		}
	}
	if tail[1].Op != OAssign || tail[1].X == nil || tail[1].X.Op != OLocal ||
		tail[1].X.Obj == nil || tail[1].X.Obj.Class != ClassResult {
		t.Fatalf("the exit does not copy into the result object:\n%s", buildDump(fn))
	}
}

// TestLowerCapturedResultCellIsAllocatedOnce checks the cell of a named result
// is allocated at the entry and nowhere else.
//
// A result is declared by the signature, as a parameter is, so there is no
// statement to put the allocation in front of. A second allocation would be a
// second variable, and the literal and the function would stop sharing one.
func TestLowerCapturedResultCellIsAllocatedOnce(t *testing.T) {
	fn := lowerOK(t, `func f() (n int) { for i := 0; i < 3; i = i + 1 { sink(func() int { n = n + i; return n }) }; return n }`)
	cell := cellOfResult(fn, "n")
	if cell == nil {
		t.Fatalf("the captured result has no cell; the locals are %v", localNames(fn))
	}
	allocs := 0
	for _, s := range fn.Body {
		Walk(s, func(m *Node) bool {
			if m.Op == OAssign && m.X != nil && m.X.Op == OLocal && m.X.Obj == cell {
				allocs++
			}
			return true
		})
	}
	if allocs != 1 {
		t.Errorf("the cell of the result is assigned %d times, want the one allocation at the entry:\n%s", allocs, buildDump(fn))
	}
	if fn.Body[0] == nil || fn.Body[0].X == nil || fn.Body[0].X.Obj != cell {
		t.Errorf("the cell of the result is not allocated by the first statement:\n%s", buildDump(fn))
	}
}

// localNames lists a function's locals, for a failure that has to say what is
// there instead.
func localNames(fn *Func) []string {
	out := make([]string, 0, len(fn.Locals))
	for _, o := range fn.Locals {
		out = append(out, o.Name)
	}
	return out
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

// TestLowerMovesAnAddressTakenLocalToTheHeap is the interim rule
// specs/023-escape-analysis.md states for the one site with no safe default.
//
// "func f() *int { n := 1; return &n }" used to return a pointer into a frame
// that is gone. It read correctly until something overwrote that memory, which
// is worse than a crash: a short program agreed with gc and a collection
// between the call and the read did not. Without the pass that decides which
// variable stays, the sound answer is the heap.
func TestLowerMovesAnAddressTakenLocalToTheHeap(t *testing.T) {
	fn := lowerOK(t, `func f(x int) *int { n := x; return &n }`)
	cell := cellOfResult(fn, "n")
	if cell == nil {
		t.Fatalf("the address-taken local has no cell; the locals are %v", localNames(fn))
	}
	for _, o := range fn.Locals {
		if o.Name == "n" && o.Addrtaken {
			t.Error("the variable is still address-taken, so it also has a frame slot the cell replaced")
		}
	}
	allocs := 0
	for _, s := range fn.Body {
		Walk(s, func(m *Node) bool {
			if m.Op == OCall && m.X != nil && m.X.Obj != nil && m.X.Obj.Name == "runtime.newobject" {
				allocs++
			}
			return true
		})
	}
	if allocs != 1 {
		t.Errorf("the function makes %d allocations, want the one cell:\n%s", allocs, buildDump(fn))
	}
}

// TestLowerLeavesALocalTheCompilerTookTheAddressOf checks which mark the rule
// above reads.
//
// ir/lower.go takes the address of its own temporaries, and a temporary whose
// address the compiler took lives exactly as long as the frame. Reading
// Addrtaken would move those to the heap as well, and a second run of the pass
// would find work the first run created. Object.Escapes is set by ir.Build and
// by nothing after it, which is why the pass is idempotent.
func TestLowerLeavesALocalTheCompilerTookTheAddressOf(t *testing.T) {
	fn := lowerOK(t, `func f(s []int) int { n := 0; for _, v := range s { n = n + v }; return n }`)
	for _, o := range fn.Locals {
		if o.Escapes {
			t.Errorf("the lowering pass marked %s as escaping; only ir.Build may", o.Name)
		}
		if strings.HasPrefix(o.Name, ".cell_") {
			t.Errorf("a temporary of the pass was moved to the heap:\n%s", buildDump(fn))
		}
	}
}
