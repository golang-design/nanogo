// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"go/constant"

	"golang.design/x/nanogo/rtsym"
	"golang.design/x/nanogo/syntax"
)

// The lowering pass: specs/020-ir.md's lowering table, performed.
//
// specs/002-architecture.md states that no Go construct survives into SSA, and
// specs/021-ssa-construction.md enforces it: ssa.Build refuses every node
// Op.IsGoSpecific reports. Until this file existed the claim had no performer,
// so the enforcement was the whole of the implementation and 22,580 functions
// of the distribution were refused for constructs that had nowhere to go.
//
// # Why the pass is here and not in SSA construction
//
// A row of the table could be handled in ssa/build.go, ahead of the guard. It
// would defeat the guard: the invariant is what makes the table's left column
// a finite list rather than a habit, and a case in front of the check removes
// one row from the list without removing it from the language. The pass runs
// over the tree, before construction sees it, and construction keeps refusing
// everything.
//
// # What a lowered tree may contain
//
// Only the operations of specs/020-ir.md's Values, Operations and Control
// groups, and only in the shapes ssa.Build accepts. That bound is narrow and
// it decides most of what follows. Two consequences are worth naming here
// rather than at each use:
//
//   - There is no node that reads the length word out of a slice or a string.
//     The header is reached the way the machine reaches it: the address of the
//     value, reinterpreted as a pointer to a struct with the header's fields,
//     and a field selection off that. header states the layout once.
//   - There is no allocation. Every symbol rtsym holds for the heap
//     (newobject, newarray, makeslice, makechan, growslice) takes a *_type,
//     and specs/032-type-descriptors-and-itabs.md, which produces one, is not
//     built. A construct that needs the heap is therefore refused rather than
//     given a frame slot: a frame slot whose address outlives the frame is
//     memory corruption, and it is the one failure mode that a later pass
//     cannot detect.
//
// # What it refuses
//
// A row that cannot be performed leaves its node in place and is reported.
// Lowering does not stop at the first one, because a function is refused by
// construction either way and the count of causes is what says which row to
// build next.
//
// See specs/031-runtime-lowering.md for the symbols, and lower_test.go for the
// table as a checklist.

// LowerError names a construct this pass does not lower.
//
// Op and What are separate so that a caller counting causes can group by the
// pair rather than by a formatted string with a function name in it.
type LowerError struct {
	Func string
	Op   Op
	What string
}

func (e *LowerError) Error() string {
	return fmt.Sprintf("ir: lowering %s: %s: %s", e.Func, e.Op, e.What)
}

// Cause is the error without the function it happened in, which is what a
// count by cause groups on.
func (e *LowerError) Cause() string { return e.Op.String() + ": " + e.What }

// Lower rewrites the Go-specific nodes of fn into the operations of
// specs/020-ir.md's lowering table.
//
// The tree is mutated in place, which specs/020-ir.md's third difference from
// the syntax tree permits: the IR is owned rather than borrowed.
//
// The returned error is the first construct the pass could not lower. The tree
// is still rewritten everywhere else, so a caller that hands the result to SSA
// construction gets as far as the refused node and no further, which is what
// makes the count of causes a measurement rather than a guess.
func Lower(fn *Func) error {
	if fn == nil {
		return fmt.Errorf("ir: Lower needs a function")
	}
	l := &lowerer{fn: fn, ptrs: make(map[*Type]*Type), hdrs: make(map[*Type]*Type)}
	fn.Body = l.stmts(fn.Body)
	if len(l.errs) > 0 {
		return l.errs[0]
	}
	// The invariant this pass exists to satisfy, asserted rather than assumed.
	// specs/020-ir.md names HasGoSpecific as the check and records that nothing
	// called it; a pass that reports no refusal and leaves a Go-specific node
	// behind is the bug this catches, and the alternative is finding it in the
	// code generator.
	for _, s := range fn.Body {
		if op, ok := HasGoSpecific(s); ok {
			return &LowerError{Func: fn.Name, Op: op, What: "survived a lowering that reported no refusal"}
		}
	}
	return nil
}

// lowerer holds the state of one function's lowering.
type lowerer struct {
	fn *Func

	// sinks is the stack of statement lists being built. An expression that
	// needs a temporary appends to the top of the stack, so the lexical order
	// of the operands becomes the order of the statements, exactly as the
	// builder's sink does.
	sinks [][]Stmt

	ntmp int
	errs []error

	// ptrs and hdrs are lookup tables, never ranged over
	// (specs/053-determinism.md). Sharing one pointer type per element type
	// keeps two addresses of one type comparable by pointer.
	ptrs map[*Type]*Type
	hdrs map[*Type]*Type
}

func (l *lowerer) refuse(n *Node, what string) {
	name := ""
	if l.fn != nil {
		name = l.fn.Name
	}
	l.errs = append(l.errs, &LowerError{Func: name, Op: n.Op, What: what})
}

// The statement sink.

func (l *lowerer) push()          { l.sinks = append(l.sinks, nil) }
func (l *lowerer) emit(s ...Stmt) { l.sinks[len(l.sinks)-1] = append(l.sinks[len(l.sinks)-1], s...) }

func (l *lowerer) pop() []Stmt {
	out := l.sinks[len(l.sinks)-1]
	l.sinks = l.sinks[:len(l.sinks)-1]
	return out
}

// stmts lowers a statement list into a list of its own.
func (l *lowerer) stmts(list []Stmt) []Stmt {
	l.push()
	for _, s := range list {
		l.stmt(s)
	}
	return l.pop()
}

// Types the lowered tree needs and the checker never produced.

func mustLayoutNamed(k Kind, name string) *Type {
	t := &Type{Kind: k, Name: name}
	if err := Layout(t); err != nil {
		panic("ir: " + name + " does not lay out: " + err.Error())
	}
	return t
}

// The lowered tree names these types where the source named none: the index of
// a range loop, the length word of a header, the size of a memory clear. They
// are the machine's types and not the checker's, and a type below the IR is a
// size, an alignment and a pointer map, so one shared value per kind is the
// whole of what is needed.
var (
	lowerInt       = mustLayoutNamed(Int64, "int")
	lowerBool      = mustLayoutNamed(Bool, "bool")
	lowerByte      = mustLayoutNamed(Uint8, "byte")
	lowerUintptr   = mustLayoutNamed(Uintptr, "uintptr")
	lowerUnsafePtr = mustLayoutNamed(UnsafePtr, "unsafe.Pointer")
)

// ptrTo returns the pointer type to t, one per element type.
func (l *lowerer) ptrTo(t *Type) *Type {
	if t == nil {
		t = lowerUnsafePtr
	}
	if p, ok := l.ptrs[t]; ok {
		return p
	}
	p := &Type{Kind: Ptr, Elem: t}
	if err := Layout(p); err != nil {
		panic("ir: a pointer does not lay out: " + err.Error())
	}
	l.ptrs[t] = p
	return p
}

// The header fields, in the order specs/030-abi.md lays them out.
const (
	hdrPtr = 0
	hdrLen = 1
	hdrCap = 2
)

// header returns the struct that describes the layout of a slice or a string.
//
// The IR has no node for "the length word of a slice", and inventing one would
// not help: ssa.Build has no case for it either and specs/021 owns that file.
// What both already have is a field of a struct reached through a pointer, and
// a header is a struct. So the header is written down as one, once, and the
// size is checked against the type it describes: a header that disagrees with
// the layout would read the wrong word and the disagreement would be silent.
func (l *lowerer) header(t *Type) *Type {
	if h, ok := l.hdrs[t]; ok {
		return h
	}
	var h *Type
	switch t.Kind {
	case Slice:
		h = &Type{Kind: Struct, Name: "slice", Fields: []Field{
			{Name: "ptr", Type: l.ptrTo(t.Elem)},
			{Name: "len", Type: lowerInt},
			{Name: "cap", Type: lowerInt},
		}}
	case String:
		h = &Type{Kind: Struct, Name: "string", Fields: []Field{
			{Name: "ptr", Type: l.ptrTo(lowerByte)},
			{Name: "len", Type: lowerInt},
		}}
	default:
		return nil
	}
	if err := Layout(h); err != nil {
		panic("ir: a header does not lay out: " + err.Error())
	}
	if h.Size != t.Size || h.Align != t.Align {
		panic(fmt.Sprintf("ir: the %s header is %d bytes and the type is %d", t.Kind, h.Size, t.Size))
	}
	l.hdrs[t] = h
	return h
}

// headerField returns the field of the header of x, as a value.
//
// x is addressed rather than copied, which costs nothing: a slice and a string
// are wider than a register, so specs/021-ssa-construction.md's ssaAble already
// puts every one of them in the frame.
func (l *lowerer) headerField(x Expr, index int, t *Type) Expr {
	h := l.header(x.Type)
	if h == nil || index >= len(h.Fields) {
		l.refuse(x, "no header field "+fmt.Sprint(index)+" of "+x.Type.Kind.String())
		return x
	}
	p := &Node{Op: OConvert, Pos: x.Pos, Type: l.ptrTo(h), X: l.addrOf(x)}
	return &Node{Op: OField, Pos: x.Pos, Type: t, X: p, Index: index}
}

// Temporaries and addresses.

// tempObj declares a temporary of the function being lowered.
//
// The name is distinct from the builder's .autotmp_ so that a dump of the tree
// says which pass introduced the storage. Two objects with one name would read
// as shadowing, which this is not.
func (l *lowerer) tempObj(t *Type, pos syntax.Pos) *Object {
	o := &Object{Name: fmt.Sprintf(".lowertmp_%d", l.ntmp), Type: t, Pos: pos, Class: ClassLocal}
	l.ntmp++
	l.fn.Locals = append(l.fn.Locals, o)
	return o
}

// ref returns a reference to an object.
func ref(o *Object, pos syntax.Pos) Expr {
	return &Node{Op: OLocal, Pos: pos, Type: o.Type, Obj: o}
}

// spill evaluates n into a temporary and returns it.
func (l *lowerer) spill(n Expr) *Object {
	o := l.tempObj(n.Type, n.Pos)
	l.emit(define(n.Pos, ref(o, n.Pos), n))
	return o
}

// addressable reports whether ssa.Build can take the address of n.
//
// It is the set of forms that pass's addr accepts, and it is written here
// rather than assumed, because a form it does not accept becomes "an address
// is not built yet" at construction rather than a spill here.
func addressable(n Expr) bool {
	if n == nil || n.Type == nil {
		return false
	}
	switch n.Op {
	case OLocal:
		// The blank identifier names no storage at all.
		return n.Obj != nil && n.Obj.Name != "_"
	case OGlobal:
		return n.Obj != nil && n.Obj.Class == ClassGlobal
	case ODeref:
		return true
	case OField:
		return n.X != nil && n.X.Type != nil && (n.X.Type.Kind == Ptr || addressable(n.X))
	case OIndex:
		if n.X == nil || n.X.Type == nil {
			return false
		}
		switch n.X.Type.Kind {
		case Slice:
			return true
		case Ptr:
			return n.X.Type.Elem != nil && n.X.Type.Elem.Kind == Array
		case Array:
			return addressable(n.X)
		}
	}
	return false
}

// addrOf returns the address of n, spilling n into a temporary when it does
// not name storage.
//
// The spill is a copy, so the address is the address of the copy. That is the
// right answer for every caller here: each of them reads a header out of a
// value, and a value that does not name storage cannot be written through.
func (l *lowerer) addrOf(n Expr) Expr {
	if !addressable(n) {
		n = ref(l.spill(n), n.Pos)
	}
	markAddrtaken(n)
	return &Node{Op: OAddr, Pos: n.Pos, Type: l.ptrTo(n.Type), X: n}
}

// stable returns an expression that names storage holding n's value, spilling
// n into a temporary when it does not name storage already.
//
// It is what lets one operand be read more than once. Reading the same storage
// twice is two loads of one location; evaluating the same expression twice is
// two evaluations, and one of them may panic or call.
func (l *lowerer) stable(n Expr) Expr {
	if addressable(n) {
		return n
	}
	return ref(l.spill(n), n.Pos)
}

// hold evaluates n once and returns a factory for fresh references to it.
//
// A node is a tree that later passes rewrite in place, so one node may not
// appear in two places. Every reader of a held value therefore asks for its
// own reference rather than sharing one.
func (l *lowerer) hold(n Expr) func() Expr {
	if n.Op == OConst {
		return func() Expr { return cloneExpr(n) }
	}
	o := l.spill(n)
	pos := n.Pos
	return func() Expr { return ref(o, pos) }
}

// Constants the lowered tree needs.

func intConst(pos syntax.Pos, t *Type, v int64) Expr {
	return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{Val: constant.MakeInt64(v)}}
}

// Runtime calls.

// runtimeFunc returns the object that names a runtime function.
//
// The name must be one rtsym holds, which specs/031-runtime-lowering.md
// requires to be checked against the runtime's own source. A name that is not
// there panics rather than producing a call to a symbol that does not exist,
// which links against nothing and jumps wherever the linker left that address.
func runtimeFunc(name string) *Object {
	if rtsym.Lookup(name) == nil {
		panic("ir: " + name + " is not in rtsym")
	}
	return &Object{Name: name, Type: funcType, Class: ClassFunc}
}

// runtimeCall returns a call to a runtime function.
func runtimeCall(pos syntax.Pos, name string, args ...Expr) Stmt {
	return &Node{
		Op:   OCall,
		Pos:  pos,
		Type: voidType,
		X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc(name)},
		Args: args,
	}
}

// memclr zeroes the storage a pointer names.
//
// The symbol is chosen by whether the region can hold a pointer, which
// specs/031-runtime-lowering.md states as a correctness rule rather than a
// performance one: memclrNoHeapPointers over a region holding pointers leaves
// stale pointers where the collector will read them.
func (l *lowerer) memclr(addr Expr, t *Type, pos syntax.Pos) Stmt {
	name := "runtime.memclrNoHeapPointers"
	if t.HasPointers() {
		name = "runtime.memclrHasPointers"
	}
	p := &Node{Op: OConvert, Pos: pos, Type: lowerUnsafePtr, X: addr}
	return runtimeCall(pos, name, p, intConst(pos, lowerUintptr, t.Size))
}

// Statements.

// stmt lowers one statement into the current list.
//
// Whatever the statement's operands need is emitted into that list before the
// statement, which is what keeps the order of evaluation the builder fixed.
//
// The control forms keep their Init where it is and the others flush theirs
// into the enclosing list, and the difference is not cosmetic. An if and a
// switch evaluate their init statement before their operand, so a temporary
// the operand needs has to go after that list rather than before the whole
// statement: hoisting it out of "if x := f(); x > g()" would call g before x
// was declared. Everywhere else Init is the builder's scaffolding and the
// enclosing list is where it belongs.
func (l *lowerer) stmt(s Stmt) {
	if s == nil {
		return
	}
	switch s.Op {
	case OBlock:
		s.Init = l.stmts(s.Init)
		s.Body = l.stmts(s.Body)
		l.emit(s)

	case OIf:
		init := l.stmts(s.Init)
		l.push()
		s.X = l.expr(s.X)
		s.Init = append(init, l.pop()...)
		body, els := l.stmts(s.Body), l.stmts(s.Else)
		s.Body, s.Else = body, els
		l.emit(s)

	case OFor:
		// The condition and the post statement run again on every iteration,
		// so nothing may be hoisted out of them. The condition keeps its
		// statements in its own Init, which is what an expression's Init
		// means, and the post list is a list already.
		s.Init = l.stmts(s.Init)
		s.X = l.guarded(s.X)
		s.Body = l.stmts(s.Body)
		s.Post = l.stmts(s.Post)
		l.emit(s)

	case OSwitch:
		init := l.stmts(s.Init)
		l.push()
		s.X = l.expr(s.X)
		s.Init = append(init, l.pop()...)
		for _, c := range s.Body {
			if c == nil || c.Op != OCase {
				continue
			}
			// A case expression is evaluated only when the clauses before it
			// did not match, so it keeps its statements in its own Init.
			for i := range c.Args {
				c.Args[i] = l.guarded(c.Args[i])
			}
			c.Body = l.stmts(c.Body)
		}
		l.emit(s)

	case OLabel:
		s.Body = l.stmts(s.Body)
		l.emit(s)

	case OReturn:
		l.flush(s)
		for i := range s.Args {
			s.Args[i] = l.expr(s.Args[i])
		}
		l.emit(s)

	case OAssign:
		l.flush(s)
		s.X = l.expr(s.X)
		for i := range s.Args {
			s.Args[i] = l.expr(s.Args[i])
		}
		s.Y = l.expr(s.Y)
		l.emit(s)

	case OGoto, OBreak, OContinue:
		l.flush(s)
		l.emit(s)

	case ORange:
		// Init is not flushed here. It holds the range expression's
		// temporaries, and rangeStmt puts them in the loop's own init list for
		// the reason it gives: a statement in front of the loop would take the
		// label the loop needs.
		l.rangeStmt(s)

	default:
		l.flush(s)
		// Everything else in a statement position is an expression evaluated
		// for its effect: a call, and the Go-specific statements that become
		// one. What the expression produces is a value nobody reads once the
		// effect is in the sink, and ssa.Build has no statement case for a
		// value, so a lowering that left one behind is dropped here rather
		// than emitted for nothing. A node that is still Go-specific is
		// emitted, so that construction reports it.
		out := l.expr(s)
		if out == nil {
			return
		}
		switch out.Op {
		case OLocal, OGlobal, OConst, OField, OIndex, ODeref, OAddr,
			OBinary, OCompare, OUnary, OConvert:
			return
		}
		l.emit(out)
	}
}

// flush moves a node's Init into the enclosing list.
//
// Every consumer of Init evaluates it immediately before the node, so a
// statement whose Init is the builder's scaffolding reads the same either way,
// and one list is easier to walk than two.
func (l *lowerer) flush(n *Node) {
	if len(n.Init) == 0 {
		return
	}
	init := n.Init
	n.Init = nil
	for _, s := range init {
		l.stmt(s)
	}
}

// Expressions.

// guarded lowers an expression that is not evaluated where it is written: the
// right operand of && and ||, a loop condition, and a case expression.
//
// Its statements go into its own Init rather than into the enclosing list,
// because the enclosing list runs once and unconditionally and the expression
// does not. An expression node's Init means exactly that: evaluate these
// statements immediately before this node.
func (l *lowerer) guarded(n Expr) Expr {
	if n == nil {
		return nil
	}
	l.push()
	out := l.expr(n)
	pre := l.pop()
	if out != nil && len(pre) > 0 {
		out.Init = append(pre, out.Init...)
	}
	return out
}

// expr lowers an expression and returns what replaces it.
func (l *lowerer) expr(n Expr) Expr {
	if n == nil {
		return nil
	}
	// The builder attaches statements to an operand where there was no
	// enclosing list to put them in. They run immediately before the operand,
	// so they are flushed into the current sink before anything lowering adds.
	l.flush(n)
	switch n.Op {
	case OCompositeLit:
		return l.compositeLit(n)

	case OAddr:
		if n.X != nil && n.X.Op == OCompositeLit {
			// &T{...} is an allocation, and there is none to make. Lowering
			// the literal into a frame slot and taking its address would
			// produce a pointer that outlives the frame whenever the value
			// escapes, and nothing here knows whether it does:
			// specs/023-escape-analysis.md is not built and Object.Escapes is
			// set by nothing.
			l.refuse(n.X, "the address of a composite literal needs an allocation")
			return n
		}
		n.X = l.expr(n.X)
		return n

	case OBinary:
		n.X = l.expr(n.X)
		if n.Op1 == syntax.AndAnd || n.Op1 == syntax.OrOr {
			n.Y = l.guarded(n.Y)
		} else {
			n.Y = l.expr(n.Y)
		}
		return n
	}

	n.X = l.expr(n.X)
	n.Y = l.expr(n.Y)
	for i := range n.Args {
		n.Args[i] = l.expr(n.Args[i])
	}

	switch n.Op {
	case OLen, OCap:
		return l.lenCap(n)
	case OSlice:
		return l.sliceExpr(n)
	case OClose:
		return runtimeCall(n.Pos, "runtime.closechan", n.X)
	case OPanic:
		return runtimeCall(n.Pos, "runtime.gopanic", n.X)
	case OUnsafeSliceData, OUnsafeStringData:
		// The data pointer read out of a header, which is the row exactly.
		if n.X == nil || n.X.Type == nil {
			l.refuse(n, "an operand with no type")
			return n
		}
		return l.headerField(l.stable(n.X), hdrPtr, n.Type)
	case OCopy:
		return l.copyExpr(n)
	case OClear:
		return l.clearExpr(n)
	case OMin, OMax:
		return l.minMax(n)
	case OUnsafeAdd:
		// Pointer arithmetic. The offset was written with an integer type of
		// its own, which the specification leaves free, so it is widened to a
		// machine word rather than assumed to be one.
		return &Node{Op: OBinary, Op1: syntax.Add, Pos: n.Pos, Type: n.Type,
			X: n.X, Y: l.widen(n.Y)}
	}
	if n.Op.IsGoSpecific() {
		l.refuse(n, "no row of the lowering table is built for it yet")
	}
	return n
}

// len and cap.
//
// specs/020-ir.md gives them four rows. Three are built here: the length or
// capacity word read out of a slice or string header, and a constant for an
// array or a pointer to one. The fourth, a map and a channel, is refused:
// specs/031's chanlen and chancap are named in its prose and are not in rtsym,
// and a map's count field is a runtime layout this pass may not assume.
func (l *lowerer) lenCap(n Expr) Expr {
	x := n.X
	if x == nil || x.Type == nil {
		l.refuse(n, "an operand with no type")
		return n
	}
	switch x.Type.Kind {
	case Slice:
		index := hdrLen
		if n.Op == OCap {
			index = hdrCap
		}
		return l.headerField(x, index, n.Type)

	case String:
		if n.Op == OCap {
			// cap of a string is not a program the checker accepts.
			l.refuse(n, "the capacity of a string")
			return n
		}
		return l.headerField(x, hdrLen, n.Type)

	case Array:
		return l.constLen(n, x, x.Type.Len)

	case Ptr:
		if x.Type.Elem == nil || x.Type.Elem.Kind != Array {
			l.refuse(n, "a pointer that is not to an array")
			return n
		}
		return l.constLen(n, x, x.Type.Elem.Len)
	}
	l.refuse(n, "the length of "+x.Type.Kind.String())
	return n
}

// constLen returns the constant length of an array, keeping the operand.
//
// specs/020-ir.md's row says the operand is still evaluated for its effects.
// The checker folds len of an array to a constant wherever the language says
// it is one, so an operand reaching here is one the language says is not
// constant, which is exactly the case where it may call a function or receive
// from a channel. Anything that is not a plain reference is therefore spilled
// rather than dropped.
func (l *lowerer) constLen(n, x Expr, length int64) Expr {
	switch x.Op {
	case OConst, OLocal, OGlobal:
	default:
		l.spill(x)
	}
	return intConst(n.Pos, n.Type, length)
}

// Composite literals.
//
// specs/020-ir.md: a frame or heap allocation plus element stores. The heap is
// not available, so what is built is the frame form, and it is built only where
// the value is copied out of the frame rather than pointed at. compositeLit is
// reached from an expression position; the address form is refused in expr.
//
// maxZeroStores bounds the element stores that fill in what a literal left out.
// Beyond it the temporary is cleared with one call instead, which is both
// smaller and the only form that scales: [1 << 20]byte{} is a legal literal.
const maxZeroStores = 8

func (l *lowerer) compositeLit(n Expr) Expr {
	t := n.Type
	if t == nil {
		l.refuse(n, "a literal with no type")
		return n
	}
	switch t.Kind {
	case Struct:
		return l.structLit(n)
	case Array:
		return l.arrayLit(n)
	case Slice:
		// The elements go in an array and the header points at it. In the
		// frame that header outlives nothing it may point at, so the array
		// belongs in the heap and runtime.newarray takes a *_type
		// (specs/032-type-descriptors-and-itabs.md). This is also every
		// variadic call, whose arguments the builder packs into a slice
		// literal.
		l.refuse(n, "a slice literal needs a heap allocation")
	case Map:
		l.refuse(n, "a map literal needs runtime.makemap, which rtsym does not have")
	default:
		l.refuse(n, "a literal of "+t.Kind.String())
	}
	return n
}

// structLit builds a struct literal in the frame.
//
// The builder normalises a struct literal to one element per field, in
// declaration order, with a field the source left out written out as its zero
// value. So a literal with elements writes every field and needs no clear, and
// the only literal without elements is the zero value the builder itself
// produces for an omitted field.
//
// The padding between fields is left as it was. Nothing in the language reads
// it: specs/020-ir.md makes struct comparison field-wise, and padding never
// holds a pointer, so the collector does not read it either.
func (l *lowerer) structLit(n Expr) Expr {
	t := n.Type
	o := l.tempObj(t, n.Pos)
	switch len(n.Args) {
	case len(t.Fields):
		for i := range n.Args {
			dst := &Node{Op: OField, Pos: n.Pos, Type: t.Fields[i].Type, X: ref(o, n.Pos), Index: i}
			l.emit(Assign(n.Pos, dst, l.expr(n.Args[i])))
		}
	case 0:
		l.zero(o, n.Pos)
	default:
		l.refuse(n, fmt.Sprintf("%d elements for %d fields", len(n.Args), len(t.Fields)))
		return n
	}
	return ref(o, n.Pos)
}

// arrayLit builds an array literal in the frame.
//
// An array literal does not have to name every element, and the ones it does
// not name are zero. The builder does not fill them in, as it does for a
// struct, because an element carries its index rather than its position: the
// language gives an element with no key the index after the previous one, so
// [...]int{5: 1, 2} writes 5 and 6.
func (l *lowerer) arrayLit(n Expr) Expr {
	t := n.Type
	o := l.tempObj(t, n.Pos)

	// The indices, in the order the elements are written, so that the stores
	// happen in source order.
	at := make([]int64, len(n.Args))
	next := int64(0)
	written := make(map[int64]bool, len(n.Args))
	for i, e := range n.Args {
		if e != nil && e.Op == OAssign {
			key, ok := constIndex(e.X)
			if !ok {
				l.refuse(n, "an element index that is not a constant")
				return n
			}
			next = key
		}
		if next < 0 || next >= t.Len {
			l.refuse(n, "an element index outside the array")
			return n
		}
		at[i] = next
		written[next] = true
		next++
	}

	// A literal that names every element overwrites the whole array, so
	// nothing has to clear it first. One that does not must clear, because the
	// temporary is reused on the next iteration of whatever loop holds it and
	// the entry block's zero only happens once.
	if missing := t.Len - int64(len(written)); missing > 0 {
		if missing <= maxZeroStores && t.Elem != nil {
			for i := int64(0); i < t.Len; i++ {
				if written[i] {
					continue
				}
				if z := l.zeroOf(t.Elem, n.Pos); z != nil {
					l.emit(Assign(n.Pos, l.elem(o, t, i, n.Pos), z))
				} else {
					l.zero(o, n.Pos)
					break
				}
			}
		} else {
			l.zero(o, n.Pos)
		}
	}

	for i, e := range n.Args {
		val := e
		if e != nil && e.Op == OAssign {
			val = e.Y
		}
		l.emit(Assign(n.Pos, l.elem(o, t, at[i], n.Pos), l.expr(val)))
	}
	return ref(o, n.Pos)
}

// elem returns the element of an array temporary at a constant index.
func (l *lowerer) elem(o *Object, t *Type, i int64, pos syntax.Pos) Expr {
	return &Node{
		Op:   OIndex,
		Pos:  pos,
		Type: t.Elem,
		X:    ref(o, pos),
		Y:    intConst(pos, lowerInt, i),
	}
}

// zero clears a temporary.
func (l *lowerer) zero(o *Object, pos syntax.Pos) {
	if o.Type.Size == 0 {
		return
	}
	l.emit(l.memclr(l.addrOf(ref(o, pos)), o.Type, pos))
}

// zeroOf returns the zero value of a type as a constant, or nil when the type
// has no constant zero and has to be cleared through memory.
func (l *lowerer) zeroOf(t *Type, pos syntax.Pos) Expr {
	switch {
	case t.Kind == Bool:
		return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{Val: constant.MakeBool(false)}}
	case t.Kind == String:
		return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{Val: constant.MakeString("")}}
	case t.Kind.IsInteger():
		return intConst(pos, t, 0)
	case t.Kind.IsFloat():
		return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{Val: constant.MakeFloat64(0)}}
	case t.Kind == Ptr, t.Kind == UnsafePtr, t.Kind == Map, t.Kind == Chan, t.Kind == FuncKind:
		return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{}}
	}
	return nil
}

// constIndex reads the constant index an element of an array literal carries.
func constIndex(n Expr) (int64, bool) {
	// The builder converts an element key to int like an index, so a key
	// written with another integer type arrives wrapped in a conversion. The
	// conversion of a constant is still a constant.
	for n != nil && n.Op == OConvert {
		n = n.X
	}
	if n == nil || n.Op != OConst {
		return 0, false
	}
	c, ok := n.Val.(ConstValue)
	if !ok {
		return 0, false
	}
	return c.Int64()
}

// range.
//
// specs/020-ir.md gives range six rows. Three are built here: slice, array and
// pointer to array as an index loop with the bound hoisted, and integer as a
// counted loop. String, map, channel and function are refused; the report says
// why for each.
//
// The Go 1.22 rule that a := in a range clause declares a fresh variable each
// iteration is the builder's and is not touched here. perIteration has already
// pointed the clause's destinations at a carrier and opened the body with the
// declaration that copies the carrier into the per-iteration variable. So the
// destinations are written before the body, and the body is emitted after them
// with its first statement intact.
func (l *lowerer) rangeStmt(n *Node) {
	// The setup goes into the loop's own init list rather than in front of the
	// loop, and the reason is a label. specs/021-ssa-construction.md gives a
	// pending label to the first statement after it, so a spill emitted
	// between "L:" and the loop would take the name the loop needs, and
	// "continue L" inside it would find no loop.
	l.push()
	bail := func(what string) {
		l.refuse(n, what)
		l.emit(l.pop()...)
		l.emit(n)
	}
	l.flush(n)

	x := l.expr(n.X)
	if x == nil || x.Type == nil {
		bail("a range over an operand with no type")
		return
	}

	// The container the loop reads and the bound it stops at, both evaluated
	// once, before the loop. The specification evaluates the range expression
	// exactly once, so an assignment in the body cannot change what is
	// iterated.
	var container *Object
	var bound Expr
	var elem func(idx Expr) Expr

	switch {
	case x.Type.Kind == Slice:
		container = l.spill(x)
		bound = ref(l.spill(l.headerField(ref(container, n.Pos), hdrLen, lowerInt)), n.Pos)
		et := x.Type.Elem
		elem = func(idx Expr) Expr {
			return &Node{Op: OIndex, Pos: n.Pos, Type: et, X: ref(container, n.Pos), Y: idx}
		}

	case x.Type.Kind == Array:
		container = l.spill(x)
		bound = intConst(n.Pos, lowerInt, x.Type.Len)
		et := x.Type.Elem
		elem = func(idx Expr) Expr {
			return &Node{Op: OIndex, Pos: n.Pos, Type: et, X: ref(container, n.Pos), Y: idx}
		}

	case x.Type.Kind == Ptr && x.Type.Elem != nil && x.Type.Elem.Kind == Array:
		container = l.spill(x)
		bound = intConst(n.Pos, lowerInt, x.Type.Elem.Len)
		et := x.Type.Elem.Elem
		elem = func(idx Expr) Expr {
			return &Node{Op: OIndex, Pos: n.Pos, Type: et, X: ref(container, n.Pos), Y: idx}
		}

	case x.Type.Kind.IsInteger():
		bound = ref(l.spill(x), n.Pos)

	case x.Type.Kind == String:
		bail("a range over a string needs the UTF-8 decode of specs/020's row")
		return
	case x.Type.Kind == Map:
		bail("a range over a map needs runtime.mapiterinit, which rtsym does not have")
		return
	case x.Type.Kind == Chan:
		bail("a range over a channel")
		return
	default:
		bail("a range over " + x.Type.Kind.String())
		return
	}

	// The index has the type of the first iteration variable where there is
	// one, so that the assignment to it is a copy rather than a conversion. A
	// range over an integer takes its type from the operand, which is what the
	// language gives the variable.
	idxType := lowerInt
	if x.Type.Kind.IsInteger() {
		idxType = x.Type
	}
	if len(n.Args) > 0 && n.Args[0] != nil && n.Args[0].Type != nil &&
		n.Args[0].Type.Kind.IsInteger() && n.Args[0].Type.Size == idxType.Size {
		idxType = n.Args[0].Type
	}
	idx := l.tempObj(idxType, n.Pos)

	loop := &Node{
		Op:   OFor,
		Pos:  n.Pos,
		Type: voidType,
		Init: append(l.pop(), define(n.Pos, ref(idx, n.Pos), intConst(n.Pos, idxType, 0))),
		X: &Node{
			Op: OCompare, Op1: syntax.Lss, Pos: n.Pos, Type: lowerBool,
			X: ref(idx, n.Pos), Y: bound,
		},
		Post: []Stmt{Assign(n.Pos, ref(idx, n.Pos), &Node{
			Op: OBinary, Op1: syntax.Add, Pos: n.Pos, Type: idxType,
			X: ref(idx, n.Pos), Y: intConst(n.Pos, idxType, 1),
		})},
	}

	// The destinations are written into the loop body, before everything the
	// source wrote, and the element is read only when the clause asked for it.
	l.push()
	if len(n.Args) > 0 && n.Args[0] != nil {
		l.emit(Assign(n.Pos, l.expr(n.Args[0]), ref(idx, n.Pos)))
	}
	if len(n.Args) > 1 && n.Args[1] != nil {
		if elem == nil {
			l.refuse(n, "a range over an integer with two variables")
			l.pop()
			l.emit(loop.Init...)
			l.emit(n)
			return
		}
		l.emit(Assign(n.Pos, l.expr(n.Args[1]), elem(ref(idx, n.Pos))))
	}
	for _, s := range n.Body {
		l.stmt(s)
	}
	loop.Body = l.pop()
	l.emit(loop)
}

// widen converts an integer to a machine word, so that it can be added to a
// pointer.
func (l *lowerer) widen(n Expr) Expr {
	if n == nil || n.Type == nil || n.Type.Size >= PtrSize {
		return n
	}
	return &Node{Op: OConvert, Pos: n.Pos, Type: lowerInt, X: n}
}

// Slice expressions.
//
// specs/020-ir.md: bounds checks plus pointer arithmetic. No allocation, which
// is why this row is built and the composite literal rows that need one are
// not: the result points into storage the operand already named.
//
// The shape is the reference implementation's, and one part of it is not an
// optimisation:
//
//	rlen  = hi - lo
//	rcap  = max - lo
//	delta = lo * elemsize
//	rptr  = base + delta & mask(rcap)
//
// The mask is what keeps the result pointer inside the object. lo may be the
// capacity, which is a legal empty slice, and base + cap*elemsize is one byte
// past the end of the allocation. The collector attributes such a pointer to
// whatever object comes next in the heap. With the mask an empty result points
// at the base instead, which is inside the object and is still a non-nil slice.
func (l *lowerer) sliceExpr(n Expr) Expr {
	x, t, pos := n.X, n.Type, n.Pos
	if x == nil || x.Type == nil || t == nil || len(n.Args) != 3 {
		l.refuse(n, "a slice expression without its three bounds")
		return n
	}
	haveLo, haveHi, haveMax := n.Args[0] != nil, n.Args[1] != nil, n.Args[2] != nil

	var elem *Type
	var base, blen, bcap func() Expr
	switch {
	case x.Type.Kind == Slice:
		x = l.stable(x)
		elem = x.Type.Elem
		base = l.hold(l.headerField(x, hdrPtr, l.ptrTo(elem)))
		blen = l.hold(l.headerField(x, hdrLen, lowerInt))
		bcap = l.hold(l.headerField(x, hdrCap, lowerInt))

	case x.Type.Kind == String:
		if haveMax {
			l.refuse(n, "a three-index slice of a string")
			return n
		}
		x = l.stable(x)
		elem = lowerByte
		base = l.hold(l.headerField(x, hdrPtr, l.ptrTo(elem)))
		blen = l.hold(l.headerField(x, hdrLen, lowerInt))
		bcap = blen

	case x.Type.Kind == Ptr && x.Type.Elem != nil && x.Type.Elem.Kind == Array:
		arr := x.Type.Elem
		elem = arr.Elem
		// p[lo:hi] is (*p)[lo:hi], so a nil p panics. The address of the
		// dereference is where ssa.Build puts the nil check.
		deref := &Node{Op: ODeref, Pos: pos, Type: arr, X: x}
		p := &Node{Op: OAddr, Pos: pos, Type: l.ptrTo(arr), X: deref}
		base = l.hold(&Node{Op: OConvert, Pos: pos, Type: l.ptrTo(elem), X: p})
		blen = func() Expr { return intConst(pos, lowerInt, arr.Len) }
		bcap = blen

	default:
		// An array that is not addressable has no storage to point into. The
		// builder takes the address of every addressable one, so what reaches
		// here is a value, and slicing a copy of it would give the caller a
		// pointer to a temporary.
		l.refuse(n, "a slice expression over "+x.Type.Kind.String())
		return n
	}

	lo := func() Expr { return intConst(pos, lowerInt, 0) }
	if haveLo {
		lo = l.hold(n.Args[0])
	}
	hi := blen
	if haveHi {
		hi = l.hold(n.Args[1])
	}
	max := bcap
	if haveMax {
		max = l.hold(n.Args[2])
	}

	// The checks the specification requires: 0 <= lo <= hi <= max <= cap. Each
	// one is emitted only where a bound the source wrote could fail it. A
	// bound the source left out is the one the specification supplies, and
	// those are in range by construction.
	if haveMax {
		l.guard(pos, syntax.Gtr, max(), bcap(), "runtime.goPanicSliceAlen", max, bcap)
	}
	if haveHi || haveMax {
		l.guard(pos, syntax.Gtr, hi(), max(), "runtime.goPanicSliceB", hi, max)
	}
	if haveLo || haveHi {
		l.guard(pos, syntax.Gtr, lo(), hi(), "runtime.goPanicSliceB", lo, hi)
	}
	if haveLo && !nonNegative(n.Args[0]) {
		l.guard(pos, syntax.Lss, lo(), intConst(pos, lowerInt, 0), "runtime.goPanicSliceB", lo, hi)
	}

	sub := func(a, b Expr) Expr {
		return &Node{Op: OBinary, Op1: syntax.Sub, Pos: pos, Type: lowerInt, X: a, Y: b}
	}
	rlen := l.hold(sub(hi(), lo()))
	rcap := rlen
	if t.Kind == Slice {
		rcap = l.hold(sub(max(), lo()))
	}

	delta := lo()
	if elem != nil && elem.Size != 1 {
		delta = &Node{Op: OBinary, Op1: syntax.Mul, Pos: pos, Type: lowerInt,
			X: delta, Y: intConst(pos, lowerInt, elem.Size)}
	}
	// mask is 0 when the result holds nothing and all ones otherwise. The
	// conversion of the comparison to a word and the negation are what make it
	// branchless; a branch here would need a join with a phi, and the shape
	// this pass produces is a tree.
	nz := &Node{Op: OCompare, Op1: syntax.Neq, Pos: pos, Type: lowerBool,
		X: rcap(), Y: intConst(pos, lowerInt, 0)}
	mask := &Node{Op: OUnary, Op1: syntax.Sub, Pos: pos, Type: lowerInt,
		X: &Node{Op: OConvert, Pos: pos, Type: lowerInt, X: nz}}
	off := &Node{Op: OBinary, Op1: syntax.And, Pos: pos, Type: lowerInt, X: delta, Y: mask}
	rptr := &Node{Op: OBinary, Op1: syntax.Add, Pos: pos, Type: l.ptrTo(elem), X: base(), Y: off}

	o := l.tempObj(t, pos)
	l.emit(Assign(pos, l.headerField(ref(o, pos), hdrPtr, l.ptrTo(elem)), rptr))
	l.emit(Assign(pos, l.headerField(ref(o, pos), hdrLen, lowerInt), rlen()))
	if t.Kind == Slice {
		l.emit(Assign(pos, l.headerField(ref(o, pos), hdrCap, lowerInt), rcap()))
	}
	return ref(o, pos)
}

// guard emits the failure edge of one bounds check.
//
// The call never returns, so the statement after it is unreachable. Nothing in
// the IR says so: specs/031-runtime-lowering.md's rule 4 is enforced by the
// rule that writes the block, and rtsym.Sym.NoReturn is read by nothing. What
// that costs here is a join block that no path reaches, which is correct and
// larger than it needs to be.
func (l *lowerer) guard(pos syntax.Pos, op syntax.Operator, a, b Expr, sym string, pa, pb func() Expr) {
	l.emit(&Node{
		Op: OIf, Pos: pos, Type: voidType,
		X:    &Node{Op: OCompare, Op1: op, Pos: pos, Type: lowerBool, X: a, Y: b},
		Body: []Stmt{runtimeCall(pos, sym, pa(), pb())},
	})
}

// nonNegative reports whether a bound is a constant that cannot be negative,
// which is what lets the check against zero be left out.
func nonNegative(n Expr) bool {
	if n == nil || n.Op != OConst {
		return false
	}
	c, ok := n.Val.(ConstValue)
	if !ok {
		return false
	}
	v, exact := c.Int64()
	return exact && v >= 0
}

// copy, clear, min and max.

// copyExpr builds the copy builtin.
//
// specs/020-ir.md gives it runtime.memmove, and an inlined move for small
// constant sizes that specs/031 records as not built anywhere. The count is
// the smaller of the two lengths, which is also the value copy returns.
func (l *lowerer) copyExpr(n Expr) Expr {
	dst, src := n.X, n.Y
	if dst == nil || src == nil || dst.Type == nil || src.Type == nil {
		l.refuse(n, "a copy with no operand")
		return n
	}
	if dst.Type.Kind != Slice {
		l.refuse(n, "a copy into "+dst.Type.Kind.String())
		return n
	}
	elem := dst.Type.Elem
	switch src.Type.Kind {
	case Slice, String:
	default:
		l.refuse(n, "a copy from "+src.Type.Kind.String())
		return n
	}
	pos := n.Pos
	dst, src = l.stable(dst), l.stable(src)
	dp := l.hold(l.headerField(dst, hdrPtr, l.ptrTo(elem)))
	dl := l.headerField(dst, hdrLen, lowerInt)
	sp := l.hold(l.headerField(src, hdrPtr, l.ptrTo(lowerByte)))
	sl := l.headerField(src, hdrLen, lowerInt)

	// The count, held in one place so that the call and the result read it
	// once. The smaller of the two lengths, written as a conditional
	// assignment rather than a select, because the IR has no select.
	o := l.tempObj(lowerInt, pos)
	l.emit(define(pos, ref(o, pos), dl))
	shorter := l.hold(sl)
	l.emit(&Node{
		Op: OIf, Pos: pos, Type: voidType,
		X: &Node{Op: OCompare, Op1: syntax.Lss, Pos: pos, Type: lowerBool,
			X: shorter(), Y: ref(o, pos)},
		Body: []Stmt{Assign(pos, ref(o, pos), shorter())},
	})

	size := Expr(ref(o, pos))
	if elem != nil && elem.Size != 1 {
		size = &Node{Op: OBinary, Op1: syntax.Mul, Pos: pos, Type: lowerInt,
			X: size, Y: intConst(pos, lowerInt, elem.Size)}
	}
	l.emit(runtimeCall(pos, "runtime.memmove",
		&Node{Op: OConvert, Pos: pos, Type: lowerUnsafePtr, X: dp()},
		&Node{Op: OConvert, Pos: pos, Type: lowerUnsafePtr, X: sp()},
		&Node{Op: OConvert, Pos: pos, Type: lowerUintptr, X: size}))
	return ref(o, pos)
}

// clearExpr builds the clear builtin for a slice.
//
// specs/020-ir.md gives clear two rows, and the map one needs runtime.mapclear,
// which rtsym does not carry.
func (l *lowerer) clearExpr(n Expr) Expr {
	x := n.X
	if x == nil || x.Type == nil {
		l.refuse(n, "a clear with no operand")
		return n
	}
	if x.Type.Kind != Slice {
		l.refuse(n, "a clear of "+x.Type.Kind.String()+" needs runtime.mapclear, which rtsym does not have")
		return n
	}
	pos := n.Pos
	elem := x.Type.Elem
	x = l.stable(x)
	ptr := l.headerField(x, hdrPtr, l.ptrTo(elem))
	size := Expr(l.headerField(x, hdrLen, lowerInt))
	if elem != nil && elem.Size != 1 {
		size = &Node{Op: OBinary, Op1: syntax.Mul, Pos: pos, Type: lowerInt,
			X: size, Y: intConst(pos, lowerInt, elem.Size)}
	}
	name := "runtime.memclrNoHeapPointers"
	if elem != nil && elem.HasPointers() {
		name = "runtime.memclrHasPointers"
	}
	return runtimeCall(pos, name,
		&Node{Op: OConvert, Pos: pos, Type: lowerUnsafePtr, X: ptr},
		&Node{Op: OConvert, Pos: pos, Type: lowerUintptr, X: size})
}

// minMax builds the min and max builtins.
//
// specs/020-ir.md: a compare and a select per operand, left to right. The
// floating-point kinds are refused rather than built that way, and the reason
// is the row's own silence: the language propagates a NaN operand to the
// result, and a compare and a select do not. Building the integer form and
// refusing the rest is the only answer that is not a wrong one.
func (l *lowerer) minMax(n Expr) Expr {
	t := n.Type
	if t == nil || len(n.Args) == 0 {
		l.refuse(n, "no operands")
		return n
	}
	if !t.Kind.IsInteger() && t.Kind != String {
		l.refuse(n, "the operands are "+t.Kind.String()+", whose NaN the language propagates")
		return n
	}
	pos := n.Pos
	op := syntax.Lss
	if n.Op == OMax {
		op = syntax.Gtr
	}
	o := l.tempObj(t, pos)
	l.emit(define(pos, ref(o, pos), n.Args[0]))
	for _, a := range n.Args[1:] {
		next := l.hold(a)
		l.emit(&Node{
			Op: OIf, Pos: pos, Type: voidType,
			X: &Node{Op: OCompare, Op1: op, Pos: pos, Type: lowerBool,
				X: next(), Y: ref(o, pos)},
			Body: []Stmt{Assign(pos, ref(o, pos), next())},
		})
	}
	return ref(o, pos)
}
