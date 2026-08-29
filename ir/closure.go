// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"strconv"

	"golang.design/x/nanogo/syntax"
)

// Closure objects, which is the capture half of
// specs/033-closures-defer-panic.md, and the heap cell, which is also the
// interim rule of specs/023-escape-analysis.md.
//
// A function literal that reads a variable of the function around it becomes
// two things: a **closure object** on the heap, built where the literal is
// written, and a body that reads the object through the context register. The
// object is a code pointer followed by one word per capture, and the address
// of the object is what a func value holds.
//
//	+------------------+
//	| code pointer     |  F, a uintptr, which the collector must not trace
//	+------------------+
//	| capture 0        |  C0, the address of a heap cell
//	| capture 1        |  C1
//	+------------------+
//
// # Every capture is by reference, and so is every address the source takes
//
// A heap cell is what both cost.
//
// The language shares one variable between the function that declares it and
// every literal that reads it: an assignment in either is seen by the other.
// ir.Build already states that by giving both functions one Object. What is
// left is where the variable lives, and it cannot be the frame: a literal that
// outlives the frame would read a frame slot that no longer exists, which is
// memory corruption and not a wrong value. So a captured variable moves into a
// heap cell of its own and both functions reach it through a pointer.
//
// specs/023-escape-analysis.md is what would decide that a cell is not needed,
// by proving that the literal does not outlive the frame or that nothing
// assigns the variable after the literal is made. It does not exist, so every
// captured variable gets a cell. That is correct and it is slow, and it is the
// same answer this compiler already gives everywhere else it needs a lifetime
// it cannot prove.
//
// A variable whose address the source takes gets one for the same reason and
// through the same code. "func f() *int { n := 1; return &n }" hands out a
// pointer, and nothing here can prove where that pointer goes, so n cannot
// stay in the frame: the pointer would name a frame that is gone. That was a
// miscompile until this rule was taken, and it was the quiet kind, because the
// memory reads correctly until something else writes it.
//
// # Why the object's type is per arity and not per closure
//
// The object is allocated through runtime.newobject, which takes a *_type, and
// specs/032-type-descriptors-and-itabs.md refuses a name for a literal struct.
// A struct synthesised per closure would therefore need a synthesised name per
// closure, and it would inherit every refusal the capture types carry: a
// capture of func type or of a literal struct type would refuse the whole
// closure for a reason that has nothing to do with the closure.
//
// One type per arity avoids both. Each capture word is an unsafe.Pointer,
// which is a name specs/032 already answers, so the descriptor set is a small
// fixed one and no capture's own type is asked for. The collector needs one
// fact about the object, which word holds a pointer, and unsafe.Pointer says
// it: the code pointer stays a uintptr and every capture word is traced.
//
// # A named result is a variable like any other
//
// A literal may read and assign a named result, and the language shares it the
// way it shares every other variable, so it gets a cell too. What is different
// is that the result object is also the storage the ABI returns, so the two
// have to be joined: every return writes the cell, and the single exit of
// ir/lower.go copies the cell into the result object after the deferred
// functions have run. A function that captures a result therefore gets that
// exit whether or not it defers.
//
// # A by-value capture is a cell nobody else can reach
//
// A method value binds its receiver where it is written and the call made
// later uses that copy. ir.Build gives it the same cell as everything else, by
// saving the receiver in a temporary and capturing the temporary: the
// temporary is written once and no other expression names it, so a capture by
// reference of it and a capture by value of the receiver hold the same value
// for as long as the closure exists. That is the argument wrapCallStmt already
// makes for the operands of a defer, and it is why there is one capture shape
// here and not two.
//
// # What is refused
//
// A method value of an interface, whose function is read out of the itab and
// named by no symbol. It is refused where the closure object would be built.

// closureCodeField is the index of the code pointer in a closure object, and
// closureFirstCapture is the index of the first capture after it.
//
// ssa/rules/arm64.go's closure call loads the entry point from offset zero, so
// the code pointer is the first field and no other layout is possible.
const (
	closureCodeField    = 0
	closureFirstCapture = 1
)

// closureCtxName is the name of the closure object type with n captures.
//
// It starts with a dot, which no Go import path element and no Go identifier
// does, so the linker symbol type:.closure2 cannot collide with a type a
// program declares.
func closureCtxName(n int) string { return ".closure" + strconv.Itoa(n) }

// closureCtxType returns the type of a closure object with n captures.
//
// The field names are exported spellings so that the descriptor carries no
// package path: rtype refuses a struct with an unexported field whose package
// the IR type does not name, and this type is declared in no package.
func closureCtxType(n int) *Type {
	fields := make([]Field, 0, n+1)
	fields = append(fields, Field{Name: "F", Type: lowerUintptr})
	for i := 0; i < n; i++ {
		fields = append(fields, Field{Name: "C" + strconv.Itoa(i), Type: lowerUnsafePtr})
	}
	// The method set is empty and it is said rather than left unset. rtype
	// refuses a defined type whose Methods is nil, because nil there means
	// the type was built below the type boundary and a descriptor written
	// from it would claim a method set nobody established. This type is built
	// below that boundary and its method set is empty by construction: the
	// language declares no method on it because no program can name it.
	t := &Type{Kind: Struct, Name: closureCtxName(n), Fields: fields, Methods: []Method{}}
	if err := Layout(t); err != nil {
		panic("ir: " + closureCtxName(n) + " does not lay out: " + err.Error())
	}
	return t
}

// closureContext returns the context parameter of a literal with the given
// captures, or nil when the literal captures nothing.
//
// ir.Build calls it, because the capture list is what it computes. The object
// is a parameter that arrives in no argument register: ssa.Build reads it out
// of the context register instead.
func closureContext(caps []*Object, pos syntax.Pos) *Object {
	if len(caps) == 0 {
		return nil
	}
	t := &Type{Kind: Ptr, Elem: closureCtxType(len(caps))}
	if err := Layout(t); err != nil {
		panic("ir: a closure context does not lay out: " + err.Error())
	}
	return &Object{Name: ".closurectx", Type: t, Class: ClassParam, Pos: pos}
}

// openCaptures prepares the function for the two halves of a capture and
// rewrites the body for both.
//
// It runs before the lowering walk rather than inside it, because a capture is
// a property of the whole function: the cell of a variable is allocated where
// the variable is declared and read wherever it is named, and neither is where
// the closure is written.
func (l *lowerer) openCaptures() {
	l.readContext()
	l.moveCapturedToHeap()
}

// readContext records how this function reaches its own captures.
//
// A function with captures reads them off the closure object the caller left
// in the context register. Nothing is emitted here: cellOf turns each capture
// into a field of that object as the rewrite meets it.
func (l *lowerer) readContext() {
	fn := l.fn
	if len(fn.Captures) == 0 {
		return
	}
	if fn.Closure == nil {
		fn.Closure = closureContext(fn.Captures, fn.Pos)
	}
	l.capIndex = make(map[*Object]int, len(fn.Captures))
	for i, o := range fn.Captures {
		if o == nil {
			continue
		}
		l.capIndex[o] = i
	}
}

// moveCapturedToHeap gives every variable this function owns and a literal in
// it captures a heap cell, and points every reference at the cell.
func (l *lowerer) moveCapturedToHeap() {
	owned, at := l.capturedHere()
	if len(owned) == 0 && len(l.capIndex) == 0 {
		return
	}
	l.cells = make(map[*Object]*Object, len(owned))
	l.uncelled = make(map[*Object]bool)
	for i, o := range owned {
		if _, err := TypeSymbol(o.Type); err != nil {
			// The cell is allocated through runtime.newobject, which takes a
			// *_type, so a capture whose type specs/032 cannot name refuses
			// the closure. The refusal names the capture and the field the
			// type boundary drops, rather than reporting an allocation of a
			// type nothing in the source wrote.
			l.refuse(at[i], "the closure captures "+o.Name+", whose cell needs a type descriptor: "+err.Error())
			l.uncelled[o] = true
			continue
		}
		cell := &Object{
			Name:  ".cell_" + o.Name,
			Type:  l.ptrTo(o.Type),
			Pos:   o.Pos,
			Class: ClassLocal,
		}
		l.fn.Locals = append(l.fn.Locals, cell)
		l.cells[o] = cell
		// The variable's own address is not taken any more. The cell holds
		// it, and a frame slot for a variable nothing reads out of the frame
		// is a slot the stack map has to describe for nothing.
		o.Addrtaken = false
	}
	for _, s := range l.fn.Body {
		l.rewriteCaptured(s)
	}
	l.fn.Body = l.placeCells(l.fn.Body, owned)
}

// capturedHere returns the objects this function owns that need a heap cell,
// in the order the tree names them, and a node that names each one first.
//
// Two sets, and they are one set: a variable a literal captures, and a
// variable whose address the source takes. Both are variables whose storage
// may outlive the frame, and neither can be proved not to without
// specs/023-escape-analysis.md.
//
// The order is the tree's and never a map's, which specs/053-determinism.md
// requires of anything that reaches output: the cells become locals and the
// locals become frame slots. The node is carried so that a refusal names
// something the tree still holds, which is what lets construction report it
// too.
func (l *lowerer) capturedHere() (objs []*Object, at []*Node) {
	seen := make(map[*Object]bool)
	take := func(o *Object, n *Node) {
		if o == nil || seen[o] {
			return
		}
		if _, ok := l.capIndex[o]; ok {
			// A variable this function captures in turn. Its cell belongs to
			// the function that declared it, and this one reaches the cell
			// through its own context.
			return
		}
		seen[o] = true
		objs = append(objs, o)
		at = append(at, n)
	}
	for _, s := range l.fn.Body {
		Walk(s, func(n *Node) bool {
			if n.Op != OClosure || n.Index != closureLiteral {
				return true
			}
			for _, a := range n.Args {
				if a != nil {
					take(a.Obj, n)
				}
			}
			return true
		})
	}
	for _, o := range l.addressed() {
		take(o, &Node{Op: OAddr, Pos: o.Pos, Type: l.ptrTo(o.Type)})
	}
	return objs, at
}

// addressed returns the variables of this function whose address the source
// takes, in declaration order.
//
// # Why the address alone is enough
//
// A local whose address outlives its frame is the one site
// specs/023-escape-analysis.md names as having no safe default, and until that
// pass exists the sound rule it states is this one: a variable whose address
// is taken goes to the heap. "func f() *int { n := 1; return &n }" is the
// shape, and what it produced was a pointer into a frame that is gone. It read
// correctly until something overwrote that memory, which is worse than a
// crash: a short program agreed with gc and a collection between the call and
// the read did not.
//
// The rule is blunter than the one this pass will make. gc moves a variable to
// the heap only where its address reaches a result, a global or a call, and
// keeps the rest in the frame. Every variable here is an allocation gc does
// not make. That is the same trade every other row of specs/023 already takes:
// the heap is correct and slower, and the frame is sometimes correct and
// otherwise corrupts memory.
//
// # Which mark this reads
//
// Object.Escapes, which ir.Build sets from the source and nothing after
// ir.Build sets. Addrtaken is the wrong field to read: ir/lower.go marks its
// own temporaries with it, and a temporary whose address the compiler took
// lives as long as the frame does. Reading it would put those in the heap and
// would make a second run of this pass find work the first run created.
func (l *lowerer) addressed() []*Object {
	fn := l.fn
	var out []*Object
	add := func(o *Object) {
		if o != nil && o.Escapes {
			out = append(out, o)
		}
	}
	add(fn.Recv)
	for _, o := range fn.Params {
		add(o)
	}
	for _, o := range fn.Results {
		add(o)
	}
	for _, o := range fn.Locals {
		add(o)
	}
	return out
}

// cellOf returns the expression that yields the address of the cell holding o,
// or nil when o has none.
//
// Two cases and one rule. A variable this function declared has a cell of its
// own, which is a local holding a pointer. A variable this function captures
// is reached through the closure object, whose capture word holds the same
// pointer. Every read of o is a load through what this returns and every
// capture of o by a further literal passes it on unchanged, which is what
// makes a capture through two levels of literal work.
func (l *lowerer) cellOf(o *Object, pos syntax.Pos) Expr {
	if o == nil {
		return nil
	}
	if cell, ok := l.cells[o]; ok {
		return ref(cell, pos)
	}
	i, ok := l.capIndex[o]
	if !ok {
		return nil
	}
	word := &Node{
		Op: OField, Pos: pos, Type: lowerUnsafePtr,
		X:     ref(l.fn.Closure, pos),
		Index: closureFirstCapture + i,
	}
	return &Node{Op: OConvert, Pos: pos, Type: l.ptrTo(o.Type), X: word}
}

// rewriteCaptured points every reference to a captured variable at its cell.
//
// A reference becomes a load through the cell pointer, and the capture list of
// a nested literal takes the cell pointer itself: the literal captures the
// same variable, which is the same cell.
func (l *lowerer) rewriteCaptured(n *Node) {
	if n == nil {
		return
	}
	if n.Op == OClosure && n.Index == closureLiteral {
		for i, a := range n.Args {
			if a == nil || a.Obj == nil {
				continue
			}
			if cell := l.cellOf(a.Obj, a.Pos); cell != nil {
				n.Args[i] = cell
			}
		}
		for _, s := range n.Init {
			l.rewriteCaptured(s)
		}
		return
	}
	rewrite := func(e Expr) Expr {
		if e == nil || e.Op != OLocal || e.Obj == nil {
			return e
		}
		cell := l.cellOf(e.Obj, e.Pos)
		if cell == nil {
			return e
		}
		return &Node{Op: ODeref, Pos: e.Pos, Type: e.Obj.Type, X: cell}
	}
	n.X, n.Y = rewrite(n.X), rewrite(n.Y)
	for i := range n.Args {
		n.Args[i] = rewrite(n.Args[i])
	}
	if n.Op == OAssign && n.X != nil && n.X.Op == ODeref {
		// The assignment no longer declares a name: it writes through a
		// pointer. A consumer that read the declaring form would skip work
		// that a store through a pointer needs.
		n.Op1 = 0
	}
	l.rewriteCaptured(n.X)
	l.rewriteCaptured(n.Y)
	for _, a := range n.Args {
		l.rewriteCaptured(a)
	}
	for _, list := range [][]Stmt{n.Init, n.Body, n.Post, n.Else} {
		for _, s := range list {
			l.rewriteCaptured(s)
		}
	}
}

// placeCells inserts the allocation of each cell into the body.
//
// A parameter's cell is allocated at the entry and the incoming value is
// copied into it, because a parameter is declared by the signature and not by
// a statement. A named result's cell is allocated at the entry for the same
// reason, and with no copy: the allocation is already zeroed and a named
// result starts at the zero value. A local's cell is allocated in the
// innermost statement list that holds every mention of it, immediately before
// the first of them.
//
// The innermost list is not a tidiness choice. A variable declared in the body
// of a loop is a fresh variable on every iteration, which is the Go 1.22 rule
// ir.Build already performs by putting the declaration inside the loop. A cell
// allocated at the entry would be one cell for every iteration, and every
// literal made in the loop would read the last iteration's value.
func (l *lowerer) placeCells(body []Stmt, owned []*Object) []Stmt {
	var entry []Stmt
	for _, o := range owned {
		cell, ok := l.cells[o]
		if !ok {
			continue
		}
		if o.Class == ClassParam || o == l.fn.Recv {
			entry = append(entry, l.newCell(cell, o.Pos))
			// The reference to the parameter is built after the rewrite has
			// run, so it names the parameter and not the cell.
			entry = append(entry, Assign(o.Pos,
				&Node{Op: ODeref, Pos: o.Pos, Type: o.Type, X: ref(cell, o.Pos)},
				ref(o, o.Pos)))
			continue
		}
		if o.Class == ClassResult {
			// A named result is declared by the signature too, and it starts
			// at the zero value. runtime.newobject returns zeroed memory, so
			// the allocation is the whole of it and no copy is needed: the
			// result object is written only by the exit, out of this cell.
			entry = append(entry, l.newCell(cell, o.Pos))
			continue
		}
		want := 0
		for _, s := range body {
			want += mentions(s, cell)
		}
		if !insertCell(&body, cell, l.newCell(cell, o.Pos), want) {
			entry = append(entry, l.newCell(cell, o.Pos))
		}
	}
	if len(entry) == 0 {
		return body
	}
	return append(entry, body...)
}

// newCell is the statement that allocates one cell.
func (l *lowerer) newCell(cell *Object, pos syntax.Pos) Stmt {
	return define(pos, ref(cell, pos), &Node{Op: ONew, Pos: pos, Type: cell.Type})
}

// insertCell puts alloc in the innermost statement list that holds all want
// mentions of cell, before the first statement that mentions it, and reports
// whether it found one.
//
// want is the count over the whole body and is carried down rather than
// recomputed, which is the difference between the innermost list that holds
// every mention and the innermost list that holds some. A loop whose condition
// names the variable and whose body names it too holds all of them in the
// loop statement and not in the loop's body, so the allocation goes in front
// of the loop and the cell is one cell.
func insertCell(list *[]Stmt, cell *Object, alloc Stmt, want int) bool {
	if want == 0 {
		return false
	}
	total := 0
	for _, s := range *list {
		total += mentions(s, cell)
		if s != nil && s.Op == OCase {
			// The clauses of a switch are the statement list, and a statement
			// between two of them is not a statement of the switch. The
			// allocation belongs in front of the switch or inside one clause,
			// and the recursion below reaches the clause.
			return false
		}
	}
	if total != want {
		return false
	}
	// A statement that holds every mention below it owns a list of its own,
	// and the allocation belongs in that list rather than in this one.
	for _, s := range *list {
		if s == nil || mentions(s, cell) != want {
			continue
		}
		for _, inner := range []*[]Stmt{&s.Init, &s.Body, &s.Post, &s.Else} {
			if insertCell(inner, cell, alloc, want) {
				return true
			}
		}
		break
	}
	for i, s := range *list {
		if mentions(s, cell) == 0 {
			continue
		}
		out := make([]Stmt, 0, len(*list)+1)
		out = append(out, (*list)[:i]...)
		out = append(out, alloc)
		out = append(out, (*list)[i:]...)
		*list = out
		return true
	}
	return false
}

// mentions counts the references to an object below n.
func mentions(n *Node, o *Object) int {
	count := 0
	Walk(n, func(m *Node) bool {
		if m.Obj == o && (m.Op == OLocal || m.Op == OGlobal) {
			count++
		}
		return true
	})
	return count
}

// closureValue builds the closure object of a function literal that captures.
//
// The object is a code pointer and one word per capture, and the value is its
// address. A capture word holds the address of the variable's cell, so the
// literal and the function that made it read one variable, which is what the
// language requires of a capture.
func (l *lowerer) closureValue(n Expr, o *Object, caps []Expr, t *Type) Expr {
	pos := n.Pos
	ct := closureCtxType(len(caps))
	obj := l.allocate(n, ct, pos)
	if obj == nil {
		return n
	}
	p := l.spill(obj)
	entry := &Node{
		Op: OAddr, Pos: pos, Type: l.ptrTo(o.Type),
		X: &Node{Op: OGlobal, Pos: pos, Type: o.Type, Obj: o},
	}
	l.emit(Assign(pos,
		l.ctxField(p, closureCodeField, lowerUintptr, pos),
		&Node{Op: OConvert, Pos: pos, Type: lowerUintptr, X: entry}))
	for i, c := range caps {
		if c == nil {
			l.refuse(n, fmt.Sprintf("capture %d of the literal names no storage", i))
			return n
		}
		l.emit(Assign(pos,
			l.ctxField(p, closureFirstCapture+i, lowerUnsafePtr, pos),
			&Node{Op: OConvert, Pos: c.Pos, Type: lowerUnsafePtr, X: c}))
	}
	return &Node{Op: OConvert, Pos: pos, Type: t, X: ref(p, pos)}
}

// hasUncelledCapture reports whether a literal names a capture this function
// refused a cell for.
func (l *lowerer) hasUncelledCapture(caps []Expr) bool {
	for _, c := range caps {
		if c != nil && c.Op == OLocal && l.uncelled[c.Obj] {
			return true
		}
	}
	return false
}

// ctxField returns one field of a closure object reached through a pointer.
func (l *lowerer) ctxField(p *Object, index int, t *Type, pos syntax.Pos) Expr {
	return &Node{Op: OField, Pos: pos, Type: t, X: ref(p, pos), Index: index}
}
