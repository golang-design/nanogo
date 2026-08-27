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
//   - Allocation goes through a type descriptor. Every symbol rtsym holds for
//     the heap takes a *_type, so specs/032-type-descriptors-and-itabs.md
//     decides which of these rows can be built at all: a construct whose type
//     has no canonical name is still refused, and the refusal names the field
//     specs/020-ir.md's type boundary drops rather than saying only that a
//     name was wanted. A frame slot is never the answer for a value that may
//     outlive the frame, because a frame slot whose address outlives the frame
//     is memory corruption, and it is the one failure mode a later pass cannot
//     detect.
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
	_, err := LowerAndCollect(fn)
	return err
}

// LowerAndCollect is Lower, and it also returns the types whose descriptors
// the lowered tree names.
//
// Every allocation the pass introduces passes a *_type to the runtime, and
// specs/032-type-descriptors-and-itabs.md makes the set of descriptors a
// package must emit exactly the set its code names. Nothing between this pass
// and the object writer carries a list of data symbols, so the list is
// returned rather than stored: a caller that gains a writer for them unions
// the per-function lists and emits each one once.
//
// The order is the order the names were first met, not a map's, which
// specs/053-determinism.md requires of anything that reaches output.
func LowerAndCollect(fn *Func) ([]*Type, error) {
	if fn == nil {
		return nil, fmt.Errorf("ir: Lower needs a function")
	}
	l := &lowerer{
		fn:    fn,
		ptrs:  make(map[*Type]*Type),
		hdrs:  make(map[*Type]*Type),
		descs: make(map[string]*Object),
	}
	l.openCaptures()
	fn.Body = l.stmts(fn.Body)
	if l.ndefer > 0 {
		l.deferExit()
	}
	if len(l.errs) > 0 {
		return l.needed, l.errs[0]
	}
	// The invariant this pass exists to satisfy, asserted rather than assumed.
	// specs/020-ir.md names HasGoSpecific as the check and records that nothing
	// called it; a pass that reports no refusal and leaves a Go-specific node
	// behind is the bug this catches, and the alternative is finding it in the
	// code generator.
	for _, s := range fn.Body {
		if op, ok := HasGoSpecific(s); ok {
			return l.needed, &LowerError{Func: fn.Name, Op: op, What: "survived a lowering that reported no refusal"}
		}
	}
	return l.needed, nil
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

	// descs is one object per type descriptor symbol this function names, so
	// that two references to one descriptor become one relocation target. It
	// is a lookup table and is never ranged over; needed is the ordered record
	// of the same set, in the order the names were first met, which is what a
	// caller emitting the descriptors reads.
	descs  map[string]*Object
	needed []*Type

	// capIndex names the field of the closure object each of this function's
	// own captures is read from, and cells names the heap cell of each
	// variable this function declares and a literal in it captures. Both are
	// lookup tables and neither is ranged over; capturedHere carries the
	// order (specs/053-determinism.md).
	capIndex map[*Object]int
	cells    map[*Object]*Object

	// uncelled holds the captures this function refused a cell for. The
	// literal that names one is left in place, so that construction reports
	// it too rather than building a closure object with a hole in it.
	uncelled map[*Object]bool

	// ndefer counts the defer statements this function lowered. One is enough
	// to owe the function the single exit deferExit builds, so the count is
	// read as a flag; it is a count so that a report can say how many.
	ndefer int
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
	lowerInt64     = mustLayoutNamed(Int64, "int64")
	lowerUint16    = mustLayoutNamed(Uint16, "uint16")
	lowerUint64    = mustLayoutNamed(Uint64, "uint64")
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

func boolConst(pos syntax.Pos, v bool) Expr {
	return &Node{Op: OConst, Pos: pos, Type: lowerBool, Val: Const{Val: constant.MakeBool(v)}}
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

// Allocation.
//
// specs/032-type-descriptors-and-itabs.md is what unblocks this section. Every
// symbol rtsym holds for the heap takes a *_type, so until a descriptor could
// be named, a construct that needs the heap had to be refused: a frame slot
// whose address outlives the frame is memory corruption, and it is the one
// failure mode a later pass cannot detect.
//
// # Why every allocation here goes to the heap
//
// specs/023-escape-analysis.md is not built and Object.Escapes is set by
// nothing, so this pass cannot tell an allocation that outlives the frame from
// one that does not. The heap is the answer that is always correct and
// sometimes slower; the frame is the answer that is sometimes correct and
// otherwise corrupts memory. specs/023 is the pass that turns the slow half
// back into the fast one, and it is an optimisation over a correct program
// rather than a repair of an incorrect one.
//
// # What a caller must still do
//
// The reference this section builds names a symbol, and nothing in this deck
// writes that symbol into an object. Nothing calls this pass in a real build
// either: driver/compile.go's pass list starts at ssa.Build, so a program that
// reaches one of these rows is refused at compile time exactly as it was
// before, and the corpus test is the only caller. specs/032 lists the three
// wiring changes that close the seam and the order they have to land in. The
// one that matters here is that calling this pass without a writer for the
// descriptors turns a compile-time refusal into an undefined symbol at link
// time, which is loud rather than silent.
//
// The rows are built ahead of the writer for that reason: the count of causes
// is what says which row to build next, and a row blocked on a writer in
// another package should not read as a row blocked on this pass.

// rtypeType is the type of a type descriptor, as this pass sees it: six words,
// which is the size of internal/abi.Type on a 64-bit target.
//
// The contents are rtype's business. What is needed here is a type of the
// right size and alignment to take the address of, because below the IR a type
// is a size, an alignment and a pointer map and nothing else.
//
// It carries no name on purpose. A name would make TypeSymbol produce
// type:runtime._type, which is a descriptor gc never emits and the linker
// would never resolve, and a collector walking the types this pass reports
// would ask for one.
var rtypeType = func() *Type {
	t := &Type{Kind: Array, Elem: lowerUintptr, Len: 6}
	if err := Layout(t); err != nil {
		panic("ir: runtime._type does not lay out: " + err.Error())
	}
	return t
}()

// descriptor returns the address of the type descriptor of t.
//
// The name comes from TypeSymbol and is never built here.
// specs/032-type-descriptors-and-itabs.md requires one naming function used by
// everything, because the linker deduplicates these symbols by name and two
// spellings of one type are two descriptors for a type that must have one.
//
// The second result is the reason there is no name, for a caller to report.
func (l *lowerer) descriptor(t *Type, pos syntax.Pos) (Expr, string) {
	name, err := TypeSymbol(t)
	if err != nil {
		return nil, err.Error()
	}
	o, ok := l.descs[name]
	if !ok {
		o = &Object{Name: name, Type: rtypeType, Class: ClassGlobal}
		l.descs[name] = o
		l.needed = append(l.needed, t)
	}
	return &Node{
		Op: OAddr, Pos: pos, Type: l.ptrTo(rtypeType),
		X: &Node{Op: OGlobal, Pos: pos, Type: rtypeType, Obj: o},
	}, ""
}

// allocate returns a pointer to a fresh zeroed value of t.
//
// runtime.newobject zeroes what it returns, which is what lets a literal that
// names only some of its parts skip the clear that the frame form needs.
func (l *lowerer) allocate(n Expr, t *Type, pos syntax.Pos) Expr {
	desc, why := l.descriptor(t, pos)
	if desc == nil {
		l.refuse(n, why)
		return nil
	}
	call := &Node{
		Op: OCall, Pos: pos, Type: lowerUnsafePtr,
		X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.newobject")},
		Args: []Expr{desc},
	}
	return &Node{Op: OConvert, Pos: pos, Type: l.ptrTo(t), X: call}
}

// allocateArray returns a pointer to a fresh zeroed array of n elements.
//
// runtime.newarray takes the *element* type and the count, so one descriptor
// serves every length. The alternative, newobject over the array type, would
// need a descriptor per length, and a variadic call site produces a new length
// for every arity in the program.
func (l *lowerer) allocateArray(n Expr, elem *Type, count int64, pos syntax.Pos) Expr {
	desc, why := l.descriptor(elem, pos)
	if desc == nil {
		l.refuse(n, why)
		return nil
	}
	// The pointer's type is a pointer to an array rather than to the element,
	// so an element store is an index off it, which is a form ssa.Build takes
	// the address of. A pointer to the element would need pointer arithmetic
	// that this pass has no node for.
	arr := &Type{Kind: Array, Elem: elem, Len: count}
	if err := Layout(arr); err != nil {
		l.refuse(n, "an array of "+fmt.Sprint(count)+" does not lay out")
		return nil
	}
	call := &Node{
		Op: OCall, Pos: pos, Type: lowerUnsafePtr,
		X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.newarray")},
		Args: []Expr{desc, intConst(pos, lowerInt, count)},
	}
	return &Node{Op: OConvert, Pos: pos, Type: l.ptrTo(arr), X: call}
}

// sliceHeader writes a slice header into a fresh temporary and returns it.
//
// ptr is the data pointer, already of the element's pointer type.
func (l *lowerer) sliceHeader(t *Type, ptr, length, capacity Expr, pos syntax.Pos) Expr {
	o := l.tempObj(t, pos)
	l.emit(Assign(pos, l.headerField(ref(o, pos), hdrPtr, l.ptrTo(t.Elem)), ptr))
	l.emit(Assign(pos, l.headerField(ref(o, pos), hdrLen, lowerInt), length))
	l.emit(Assign(pos, l.headerField(ref(o, pos), hdrCap, lowerInt), capacity))
	return ref(o, pos)
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
		if len(s.Args) == 2 && s.Y != nil && s.Y.Op == ORecv {
			// v, ok = <-c. The value pair has no node of its own, so the row
			// is built here rather than in expr: expr returns one expression
			// and this statement wants two.
			l.recvAssign(s)
			return
		}
		if len(s.Args) == 2 && isMapIndex(s.Y) {
			// v, ok = m[k], for the same reason.
			l.mapAccess2(s)
			return
		}
		if len(s.Args) == 0 && isMapIndex(s.X) {
			// m[k] = v. The destination is a call and not a location, so the
			// statement is built rather than rewritten in place.
			if !l.mapAssign(s.Pos, s.X, s.Y) {
				l.emit(s)
			}
			return
		}
		for _, dst := range s.Args {
			if isMapIndex(dst) {
				// a, m[k] = f(). The values arrive together and the IR has no
				// node for one of them, so the insert cannot be ordered after
				// the call without a temporary per destination, which is a row
				// of its own rather than a case of this one.
				l.refuse(dst, "a map index as one destination of a multi-value assignment")
			}
		}
		s.X = l.expr(s.X)
		for i := range s.Args {
			s.Args[i] = l.expr(s.Args[i])
		}
		s.Y = l.expr(s.Y)
		l.emit(s)

	case OGoto, OBreak, OContinue:
		l.flush(s)
		l.emit(s)

	case ORecover:
		// recover() whose value nobody reads, which is the shape of the
		// idiom: defer func() { recover() }(). It is handled here and not in
		// expr because the position is what makes it buildable. The result is
		// an interface, and a statement discards it, so nothing below the IR
		// has to decompose one. A recover whose value is read reaches expr and
		// is refused there.
		//
		// The call has to be in the deferred function itself.
		// runtime.gorecover counts the frames between itself and
		// runtime.gopanic and recovers only when there is exactly one, so a
		// pass that wrapped this call in anything would turn a recover into a
		// no-op.
		l.flush(s)
		l.emit(runtimeCall(s.Pos, "runtime.gorecover"))

	case OSelect:
		l.selectStmt(s)

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
			return l.addrLit(n, n.X)
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

	case ODefer:
		// Before the descent below, and that is the whole reason these two
		// are here. The descent lowers the deferred call, and lowering a
		// builtin emits its runtime calls into the current list: "defer
		// println(x)" would print where the statement is and defer only
		// runtime.printunlock. What a defer holds is read, not rewritten.
		return l.deferStmt(n, "runtime.deferproc")
	case OGo:
		return l.deferStmt(n, "runtime.newproc")

	case OIndex:
		// A map index in a value position, which is a read. Every destination
		// position goes through store instead, and that is what keeps this row
		// a read. It is before the descent, with the two above, because the
		// row evaluates its own operands: the specification evaluates the map
		// and then the key, and the descent would put the key's statements
		// wherever it reached them.
		if isMapIndex(n) {
			return l.mapRead(n)
		}
	case ODelete:
		return l.mapDelete(n)
	}

	if n.Op == OCall && isFuncSymbol(n.X) {
		// The callee of a direct call names a symbol and not a value, so it
		// is left alone. Every other position a function symbol appears in is
		// a value, and the case below turns it into one.
		l.flush(n.X)
	} else {
		n.X = l.expr(n.X)
	}
	n.Y = l.expr(n.Y)
	for i := range n.Args {
		n.Args[i] = l.expr(n.Args[i])
	}

	if isFuncSymbol(n) {
		// A declared function used as a value. The value of "inc" is a
		// funcval and not inc's entry address, and the two are one word
		// apiece, so nothing below the IR could tell them apart: an indirect
		// call would load a code pointer out of inc's first instruction and
		// jump into the instruction stream read as data. That is the failure
		// specs/033-closures-defer-panic.md records, and this row is what
		// stops it.
		return l.funcValue(n, n.Obj, n.Type)
	}

	switch n.Op {
	case OLen, OCap:
		return l.lenCap(n)
	case OSlice:
		return l.sliceExpr(n)
	case OSend:
		return l.chanSend(n)
	case ORecv:
		return l.chanRecv(n)
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
	case ONew:
		return l.newExpr(n)
	case OMake:
		return l.makeExpr(n)
	case OPrint, OPrintln:
		return l.printExpr(n)
	case OClosure:
		return l.closureExpr(n)
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
// specs/020-ir.md gives them four rows. Three of them read storage: the length
// or capacity word out of a slice or string header, and a constant for an
// array or a pointer to one. The fourth is a channel, and it is a call.
//
// A call and not a load, and that is a correctness rule rather than a choice
// of shape. A nil channel has length and capacity zero and no hchan to read
// them from, so the nil case is the runtime's answer.
// cmd/compile/internal/ssagen/ssa.go's referenceTypeBuiltin stops the compiler
// with "cannot inline len(chan)" for the same reason.
//
// A map is refused. Its length is the first word of the runtime's maps.Map,
// which gc reads inline, and rtsym holds no symbol for it.
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

	case Map:
		if n.Op == OCap {
			// cap of a map is not a program the checker accepts.
			l.refuse(n, "the capacity of a map")
			return n
		}
		return l.mapLen(n)

	case Chan:
		name := "runtime.chanlen"
		if n.Op == OCap {
			name = "runtime.chancap"
		}
		return &Node{
			Op: OCall, Pos: n.Pos, Type: lowerInt,
			X:    &Node{Op: OGlobal, Pos: n.Pos, Type: funcType, Obj: runtimeFunc(name)},
			Args: []Expr{l.chanArg(x)},
		}
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
// specs/020-ir.md: a frame or heap allocation plus element stores. Which one is
// decided by whether the value can outlive the frame, and not by
// specs/023-escape-analysis.md, which is not built:
//
//   - A struct or an array in an expression position is copied out of the
//     frame by its reader, so the frame form is correct and is what is built.
//   - A slice literal keeps a pointer to its elements, and the address form of
//     any literal hands out a pointer, so both go to the heap.
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
		return l.sliceLit(n)
	case Map:
		return l.mapLit(n)
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

	at, written, ok := l.litIndices(n, t.Len)
	if !ok {
		return n
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

// litIndices returns the index each element of an array or slice literal is
// written to, in the order the elements appear.
//
// bound is the length the indices must fall inside, or a negative number when
// the literal decides the length, which is the slice case.
func (l *lowerer) litIndices(n Expr, bound int64) (at []int64, written map[int64]bool, ok bool) {
	at = make([]int64, len(n.Args))
	written = make(map[int64]bool, len(n.Args))
	next := int64(0)
	for i, e := range n.Args {
		if e != nil && e.Op == OAssign {
			key, ok := constIndex(e.X)
			if !ok {
				l.refuse(n, "an element index that is not a constant")
				return nil, nil, false
			}
			next = key
		}
		if next < 0 || (bound >= 0 && next >= bound) {
			l.refuse(n, "an element index outside the array")
			return nil, nil, false
		}
		at[i] = next
		written[next] = true
		next++
	}
	return at, written, true
}

// litValue returns the value an element of an array or slice literal writes.
//
// An element with an index carries the index in X and the value in Y, which is
// how the builder spells a keyed element.
func litValue(e Expr) Expr {
	if e != nil && e.Op == OAssign {
		return e.Y
	}
	return e
}

// sliceLit builds a slice literal.
//
// specs/020-ir.md's row is an allocation plus element stores plus a header.
// The elements go in the heap and not in the frame, because the header outlives
// the literal wherever the slice is returned or stored, and a header pointing
// into a dead frame is the corruption the allocation section describes.
//
// This is also every variadic call. The builder packs a variadic call's
// arguments into a slice literal, so a row that refuses one refuses the other,
// and this is the largest single refusal the pass records.
//
// The array runtime.newarray returns is zeroed, so an element the literal
// leaves out needs no store and no clear. That is the difference from
// arrayLit, whose temporary is reused on the next iteration of whatever loop
// holds it.
func (l *lowerer) sliceLit(n Expr) Expr {
	t, pos := n.Type, n.Pos
	if t.Elem == nil {
		l.refuse(n, "a slice literal with no element type")
		return n
	}
	at, _, ok := l.litIndices(n, -1)
	if !ok {
		return n
	}
	// The length is one past the largest index written, which is not the
	// number of elements: []int{5: 1} has one element and length six.
	length := int64(0)
	for _, i := range at {
		if i+1 > length {
			length = i + 1
		}
	}

	base := l.allocateArray(n, t.Elem, length, pos)
	if base == nil {
		return n
	}
	// Spilled because the pointer is read once per element and once for the
	// header, and a node is a tree that later passes rewrite in place.
	p := l.spill(base)
	for i, e := range n.Args {
		dst := &Node{Op: OIndex, Pos: pos, Type: t.Elem, X: ref(p, pos), Y: intConst(pos, lowerInt, at[i])}
		l.emit(Assign(pos, dst, l.expr(litValue(e))))
	}

	data := &Node{Op: OConvert, Pos: pos, Type: l.ptrTo(t.Elem), X: ref(p, pos)}
	return l.sliceHeader(t, data,
		intConst(pos, lowerInt, length), intConst(pos, lowerInt, length), pos)
}

// addrLit builds the address of a composite literal.
//
// The value is built in the frame and copied into the allocation, rather than
// stored into the allocation field by field. The copy costs a frame slot the
// size of the literal and it keeps one lowering of each literal form: a second
// path that wrote through a pointer would be a second set of rules for
// structs, arrays and slices, and the two would drift.
func (l *lowerer) addrLit(n, lit Expr) Expr {
	t, pos := lit.Type, lit.Pos
	if t == nil {
		l.refuse(lit, "a literal with no type")
		return n
	}
	base := l.allocate(lit, t, pos)
	if base == nil {
		return n
	}
	p := l.spill(base)
	val := l.expr(lit)
	if val == nil || val.Op == OCompositeLit {
		// The literal itself was refused, and its cause is already recorded.
		return n
	}
	l.emit(Assign(pos, &Node{Op: ODeref, Pos: pos, Type: t, X: ref(p, pos)}, val))
	return ref(p, pos)
}

// makeExpr builds the make builtin.
//
// specs/020-ir.md gives it three rows and two are built. A map is refused: its
// descriptor's tail points at the runtime's own slot group, whose fields are a
// literal struct, and specs/032-type-descriptors-and-itabs.md has no spelling
// for one. The refusal is on the descriptor and not on the call, because
// rtsym holds runtime.makemap and this pass can name the map type already.
func (l *lowerer) makeExpr(n Expr) Expr {
	t, pos := n.Type, n.Pos
	if t == nil {
		l.refuse(n, "a make with no type")
		return n
	}
	switch t.Kind {
	case Slice:
	case Chan:
		return l.makeChan(n)
	case Map:
		return l.makeMap(n)
	default:
		l.refuse(n, "a make of "+t.Kind.String())
		return n
	}
	if t.Elem == nil || len(n.Args) < 1 || len(n.Args) > 2 {
		l.refuse(n, fmt.Sprintf("a make of a slice with %d bounds", len(n.Args)))
		return n
	}
	desc, why := l.descriptor(t.Elem, pos)
	if desc == nil {
		l.refuse(n, why)
		return n
	}
	// The bounds are read twice, once by the call and once by the header, so
	// each is evaluated into storage first. The order is the source's: length
	// then capacity.
	length := l.hold(l.widen(n.Args[0]))
	capacity := length
	if len(n.Args) == 2 {
		capacity = l.hold(l.widen(n.Args[1]))
	}
	// runtime.makeslice performs the checks the specification requires, so no
	// guard is emitted here. A negative length, a negative capacity and a
	// length above the capacity each panic inside the call.
	call := &Node{
		Op: OCall, Pos: pos, Type: lowerUnsafePtr,
		X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.makeslice")},
		Args: []Expr{desc, length(), capacity()},
	}
	data := &Node{Op: OConvert, Pos: pos, Type: l.ptrTo(t.Elem), X: call}
	return l.sliceHeader(t, data, length(), capacity(), pos)
}

// Channels.
//
// specs/031-runtime-lowering.md's channel group. Every operation on a channel
// is a runtime call, and none of them reads the hchan: the runtime owns that
// layout and a nil channel has no hchan at all, so the nil case is the
// runtime's answer and never a load this pass emits.
//
// The element crosses the call as a pointer in both directions.
// runtime.chansend1 copies from the address it is given and runtime.chanrecv1
// writes through it, so a send needs storage holding the value and a receive
// needs storage to land in. Both are frame slots here, and both are safe as
// frame slots: the call reads or writes the storage and keeps no pointer to
// it.

// chanArg returns a channel value as the *hchan every channel symbol takes.
//
// A channel is one word and the runtime's parameter is a pointer, so the
// conversion is a relabelling. It is written down rather than left out because
// the word must reach the collector as a pointer: the argument bitmap of the
// call is read off the argument types, and a channel described as a number
// would let the hchan be freed while the call is blocked in it.
func (l *lowerer) chanArg(c Expr) Expr {
	return &Node{Op: OConvert, Pos: c.Pos, Type: lowerUnsafePtr, X: c}
}

// valueAddr copies a value into a fresh frame slot and returns the slot's
// address, as the unsafe.Pointer the runtime takes.
//
// The copy is not avoidable by taking the address of a variable that already
// names storage. runtime.chansend1 may block before it copies, so the address
// it is given must name storage no other goroutine writes; the specification
// evaluates the sent value before the communication begins, and a variable the
// program keeps assigning is not that value. The copy is also what keeps the
// program's own variables out of the frame: taking their address would make
// every variable ever sent a frame slot.
//
// A map key crosses every map symbol the same way and for the second reason.
// The runtime reads the key through the pointer and, for an insert, copies it
// into the table, so a frame slot is enough and the program's own variable
// stays out of the frame.
func (l *lowerer) valueAddr(v Expr) Expr {
	return &Node{Op: OConvert, Pos: v.Pos, Type: lowerUnsafePtr,
		X: l.addrOf(ref(l.spill(v), v.Pos))}
}

// elemSlot returns a fresh frame slot for the element a receive lands in, and
// its address.
//
// The slot is not cleared first. Every receive symbol writes the slot on every
// path: a receive from a closed channel writes the zero value rather than
// leaving the slot as it was.
func (l *lowerer) elemSlot(t *Type, pos syntax.Pos) (*Object, Expr) {
	o := l.tempObj(t, pos)
	addr := l.addrOf(ref(o, pos))
	return o, &Node{Op: OConvert, Pos: pos, Type: lowerUnsafePtr, X: addr}
}

// chanElem returns the element type of a channel operand, or the reason it is
// not one.
func chanElem(c Expr) (*Type, string) {
	if c == nil || c.Type == nil {
		return nil, "a channel operand with no type"
	}
	if c.Type.Kind != Chan {
		return nil, "a channel operation on " + c.Type.Kind.String()
	}
	if c.Type.Elem == nil {
		return nil, "a channel with no element type"
	}
	return c.Type.Elem, ""
}

// chanSend builds a send statement.
func (l *lowerer) chanSend(n Expr) Expr {
	elem, why := chanElem(n.X)
	if elem == nil {
		l.refuse(n, why)
		return n
	}
	if n.Y == nil || n.Y.Type == nil {
		l.refuse(n, "a send of an operand with no type")
		return n
	}
	// The channel is read into the call's operand before the value is spilled,
	// which is the source's order: the specification evaluates the channel
	// operand first.
	ch := l.chanArg(n.X)
	return runtimeCall(n.Pos, "runtime.chansend1", ch, l.valueAddr(n.Y))
}

// chanRecv builds the one-value receive.
//
// The value comes back through the frame slot and not in a register, because
// runtime.chanrecv1 returns nothing at all. The two-value form is recvAssign,
// which calls runtime.chanrecv2 for the same reason: its one result is whether
// a value arrived, and the value itself still comes back through the slot.
func (l *lowerer) chanRecv(n Expr) Expr {
	elem, why := chanElem(n.X)
	if elem == nil {
		l.refuse(n, why)
		return n
	}
	pos := n.Pos
	ch := l.chanArg(n.X)
	o, addr := l.elemSlot(elem, pos)
	l.emit(runtimeCall(pos, "runtime.chanrecv1", ch, addr))
	return ref(o, pos)
}

// recvAssign builds the two-value receive: v, ok = <-c.
//
// It is a statement row rather than an expression one because the IR has no
// node for a value pair, and it does not need one: runtime.chanrecv2 returns
// the one bool and writes the element through the pointer, so the call has a
// single result and the destinations are two ordinary assignments after it.
//
// Both destinations are written after the call and neither before it, which is
// the specification's order for a multi-value assignment. A destination that
// names no storage, which is the blank identifier, is not written at all: the
// receive has already happened and the store would be for nobody.
func (l *lowerer) recvAssign(s Stmt) {
	rv := s.Y
	ch := l.expr(rv.X)
	elem, why := chanElem(ch)
	if elem == nil {
		l.refuse(rv, why)
		l.emit(s)
		return
	}
	pos := s.Pos
	arg := l.chanArg(ch)
	o, addr := l.elemSlot(elem, pos)
	got := l.tempObj(lowerBool, pos)
	l.emit(define(pos, ref(got, pos), &Node{
		Op: OCall, Pos: pos, Type: lowerBool,
		X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.chanrecv2")},
		Args: []Expr{arg, addr},
	}))
	l.assignTo(s, 0, ref(o, pos))
	l.assignTo(s, 1, ref(got, pos))
}

// store emits an assignment of val into one destination.
//
// Every destination this pass writes goes through here, and that is the whole
// reason it exists rather than each row calling expr on its own destination. A
// map index is a different runtime call in a destination position from the one
// it is in a value position, and lowering it as a value produces a program that
// links, runs and drops the write. One router, so that a row added later cannot
// forget.
//
// The destination is lowered here and not by the caller, because a map index
// destination is not lowered at all: its map and its key are, and the index
// itself becomes a call.
func (l *lowerer) store(pos syntax.Pos, op1 syntax.Operator, dst, val Expr) {
	if isMapIndex(dst) {
		l.mapStore(pos, dst, val)
		return
	}
	out := Assign(pos, l.expr(dst), val)
	out.Op1 = op1
	l.emit(out)
}

// assignTo writes one value into destination i of a multi-value assignment,
// keeping the statement's own sense of whether it declares its destinations.
//
// The caller reaches here only for a statement with two destinations, which is
// what the interception in stmt matches on, so the index is in range.
func (l *lowerer) assignTo(s Stmt, i int, val Expr) {
	dst := s.Args[i]
	if dst == nil || namesNoStorage(dst) {
		return
	}
	l.store(s.Pos, s.Op1, dst, val)
}

// namesNoStorage reports whether a destination is the blank identifier.
//
// ir.Build gives it the void type, so a store into it would be a store of a
// value into a location of no size.
func namesNoStorage(dst Expr) bool {
	return dst.Op == OLocal && dst.Obj != nil && dst.Obj.Name == "_"
}

// makeChan builds make of a channel.
//
// The buffer size arrives as an int, because ir.Build converts every operand
// of make to one. specs/030-abi.md makes int 64 bits, so runtime.makechan64 is
// unreachable on this target: gc reaches it only where int is narrower than
// the operand's own type, and it cannot be here. A size above the largest int,
// written as a uint64, converts to a negative int and runtime.makechan panics
// on it, which is the check the specification requires and the same place gc
// leaves it.
func (l *lowerer) makeChan(n Expr) Expr {
	t, pos := n.Type, n.Pos
	if t.Elem == nil {
		l.refuse(n, "a make of a channel with no element type")
		return n
	}
	if len(n.Args) > 1 {
		l.refuse(n, fmt.Sprintf("a make of a channel with %d bounds", len(n.Args)))
		return n
	}
	desc, why := l.descriptor(t, pos)
	if desc == nil {
		l.refuse(n, why)
		return n
	}
	size := intConst(pos, lowerInt, 0)
	if len(n.Args) == 1 {
		size = l.widen(n.Args[0])
	}
	call := &Node{
		Op: OCall, Pos: pos, Type: lowerUnsafePtr,
		X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.makechan")},
		Args: []Expr{desc, size},
	}
	// The value is the *hchan the call returns, and a channel is one word
	// (specs/030-abi.md), so the conversion is a relabelling and not a copy.
	return &Node{Op: OConvert, Pos: pos, Type: t, X: call}
}

// newExpr builds the new builtin.
//
// The node's type is the result type, which is the pointer, so the type to
// allocate is its element.
func (l *lowerer) newExpr(n Expr) Expr {
	if n.Type == nil || n.Type.Kind != Ptr || n.Type.Elem == nil {
		l.refuse(n, "a new whose result is not a pointer")
		return n
	}
	out := l.allocate(n, n.Type.Elem, n.Pos)
	if out == nil {
		return n
	}
	return out
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

// Maps.
//
// specs/031-runtime-lowering.md's map group. Go's map is a swiss table from
// 1.24 on, and every operation on one is a runtime call taking the map's
// *abi.MapType descriptor, the *maps.Map itself and, where there is a key, the
// address of the key.
//
// Nothing here reads the maps.Map, with one exception this file argues where it
// is written: len. The runtime owns that layout, a nil map has no maps.Map at
// all, and every symbol below already answers for the nil case. mapaccess1 and
// mapaccess2 return the zero value, mapassign panics, mapdelete and mapclear do
// nothing, and mapIterStart starts an iteration that is already finished. A nil
// check emitted here would be a second answer to a question the runtime has
// already answered.
//
// The element does not cross the call. mapaccess1, mapaccess2 and mapassign
// each return a *pointer* into the table, and the value is read or written
// through it, which is what lets one symbol serve every element type. The key
// crosses as the address of a frame slot, for the reason valueAddr gives.
//
// A pointer into a table is an interior pointer to a heap object, which is what
// gc holds too: the collector finds the object from the span, so the whole
// group stays alive while the pointer does.

// mapArg returns a map value as the *maps.Map every map symbol takes.
//
// A map is one word and the runtime's parameter is a pointer, so the conversion
// is a relabelling. It is written down rather than left out because the word
// must reach the collector as a pointer: the argument bitmap of the call is
// read off the argument types, and a map described as a number would let the
// table be freed while the call is walking it.
func (l *lowerer) mapArg(m Expr) Expr {
	return &Node{Op: OConvert, Pos: m.Pos, Type: lowerUnsafePtr, X: m}
}

// mapKeyElem returns the key and element types of a map operand, or the reason
// it is not one.
func mapKeyElem(m Expr) (key, elem *Type, why string) {
	if m == nil || m.Type == nil {
		return nil, nil, "a map operand with no type"
	}
	if m.Type.Kind != Map {
		return nil, nil, "a map operation on " + m.Type.Kind.String()
	}
	if m.Type.Key == nil || m.Type.Elem == nil {
		return nil, nil, "a map with no key type or no element type"
	}
	return m.Type.Key, m.Type.Elem, ""
}

// isMapIndex reports whether n indexes a map.
//
// It is the test every destination is put through, because a map index in a
// destination position is a different runtime call from the same expression in
// a value position. specs/021-ssa-construction.md's indexAddr refuses a map, so
// an index that reached it would be reported rather than miscompiled, but the
// failure this guards is not that one: lowering a destination through expr
// produces a perfectly ordinary mapaccess1 and a store into the zero value the
// runtime hands back for a key that is not there. That program links, runs and
// drops every write.
func isMapIndex(n Expr) bool {
	return n != nil && n.Op == OIndex && n.X != nil && n.X.Type != nil && n.X.Type.Kind == Map
}

// mapOperands lowers the map and the key of a map index, in that order, and
// returns the call operands they become.
//
// The order is the specification's: a map index evaluates its operand and then
// its index. The map is put in storage first, so that a map expression with an
// effect happens before the key's rather than after it; a map operand that
// already names storage is left where it is, because a load has no effect to
// order against.
func (l *lowerer) mapOperands(idx Expr, pos syntax.Pos) (desc, m, key Expr, why string) {
	mv := l.stable(l.expr(idx.X))
	_, _, why = mapKeyElem(mv)
	if why != "" {
		return nil, nil, nil, why
	}
	desc, why = l.descriptor(mv.Type, pos)
	if desc == nil {
		return nil, nil, nil, why
	}
	return desc, l.mapArg(mv), l.valueAddr(l.expr(idx.Y)), ""
}

// mapPtrCall builds a map symbol whose result is a pointer to the element.
func (l *lowerer) mapPtrCall(name string, pos syntax.Pos, args ...Expr) Expr {
	return &Node{
		Op: OCall, Pos: pos, Type: lowerUnsafePtr,
		X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc(name)},
		Args: args,
	}
}

// mapElem reads the element a map symbol returned a pointer to.
func (l *lowerer) mapElem(p Expr, elem *Type, pos syntax.Pos) Expr {
	return &Node{Op: ODeref, Pos: pos, Type: elem,
		X: &Node{Op: OConvert, Pos: pos, Type: l.ptrTo(elem), X: p}}
}

// mapRead builds m[k] in the one-value form.
//
// runtime.mapaccess1 never returns nil. A key that is not in the map gets a
// pointer to the runtime's zero value for the element type, which is what makes
// the one-value form a read with no branch in it.
func (l *lowerer) mapRead(n Expr) Expr {
	pos := n.Pos
	desc, m, key, why := l.mapOperands(n, pos)
	if why != "" {
		l.refuse(n, why)
		return n
	}
	return l.mapElem(l.mapPtrCall("runtime.mapaccess1", pos, desc, m, key), n.X.Type.Elem, pos)
}

// mapAccess2 builds v, ok = m[k].
//
// It is a statement row rather than an expression one, for the reason
// recvAssign is: the IR has no node for a value pair. runtime.mapaccess2
// returns two values, so the call is built by hand the way selectgo's is, with
// a tuple result and one destination per component.
//
// Both destinations are written after the call and neither before it, which is
// the specification's order for a multi-value assignment.
func (l *lowerer) mapAccess2(s Stmt) {
	idx, pos := s.Y, s.Pos
	desc, m, key, why := l.mapOperands(idx, pos)
	if why != "" {
		l.refuse(idx, why)
		l.emit(s)
		return
	}
	p := l.tempObj(lowerUnsafePtr, pos)
	got := l.tempObj(lowerBool, pos)
	l.emit(&Node{
		Op: OAssign, Pos: pos, Type: voidType,
		Args: []Expr{ref(p, pos), ref(got, pos)},
		Y: &Node{
			Op: OCall, Pos: pos, Type: mapAccessResult,
			X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.mapaccess2")},
			Args: []Expr{desc, m, key},
		},
	})
	l.assignTo(s, 0, l.mapElem(ref(p, pos), idx.X.Type.Elem, pos))
	l.assignTo(s, 1, ref(got, pos))
}

// mapAccessResult is what runtime.mapaccess2 returns: a pointer to the element,
// and whether the key was there.
//
// The pointer component is unsafe.Pointer and not a number, because the value
// is live in a register across the store that follows and the collector reads
// the register map off this type.
var mapAccessResult = func() *Type {
	t := &Type{Kind: Tuple, Fields: []Field{
		{Name: "r0", Type: lowerUnsafePtr},
		{Name: "r1", Type: lowerBool},
	}}
	if err := Layout(t); err != nil {
		panic("ir: the result of runtime.mapaccess2 does not lay out: " + err.Error())
	}
	return t
}()

// mapStore inserts a value the caller has already built.
//
// It is what the store router reaches for a map index destination, and the
// value is a value: a range clause's key, a chosen receive's element. The
// destination is not lowered, because a map index destination is a call and not
// a location.
func (l *lowerer) mapStore(pos syntax.Pos, dst, val Expr) {
	desc, m, key, why := l.mapOperands(dst, pos)
	if why != "" {
		l.refuse(dst, why)
		return
	}
	l.mapInsert(pos, desc, m, key, dst.X.Type.Elem, val)
}

// mapAssign builds m[k] = v from the two expressions the source wrote.
//
// The order is the specification's and it is the whole of what this adds over
// mapStore: the map, then the key, then the value. A value evaluated after the
// call would be a value whose panic leaves a key inserted that the source never
// stored anything under.
func (l *lowerer) mapAssign(pos syntax.Pos, dst, val Expr) (ok bool) {
	desc, m, key, why := l.mapOperands(dst, pos)
	if why != "" {
		l.refuse(dst, why)
		return false
	}
	l.mapInsert(pos, desc, m, key, dst.X.Type.Elem, l.expr(val))
	return true
}

// mapInsert emits the call and the store through the pointer it returns.
//
// runtime.mapassign inserts the key when it is not there and returns a pointer
// to the element. The pointer goes into a frame slot rather than staying in the
// tree, so that it is evaluated where it is written: an assignment whose
// destination held the call would leave the order of the call and the value to
// specs/021-ssa-construction.md, which is not this pass's to decide.
func (l *lowerer) mapInsert(pos syntax.Pos, desc, m, key Expr, elem *Type, val Expr) {
	held := l.hold(val)()
	p := l.spill(l.mapPtrCall("runtime.mapassign", pos, desc, m, key))
	l.emit(Assign(pos, l.mapElem(ref(p, pos), elem, pos), held))
}

// mapDelete builds delete(m, k).
func (l *lowerer) mapDelete(n Expr) Expr {
	pos := n.Pos
	if n.X == nil || n.Y == nil {
		l.refuse(n, "a delete with no map or no key")
		return n
	}
	idx := &Node{Op: OIndex, Pos: pos, Type: voidType, X: n.X, Y: n.Y}
	desc, m, key, why := l.mapOperands(idx, pos)
	if why != "" {
		l.refuse(n, why)
		return n
	}
	return runtimeCall(pos, "runtime.mapdelete", desc, m, key)
}

// makeMap builds make of a map.
//
// runtime.makemap takes three parameters and the third is last: the descriptor,
// the hint, and a *maps.Map the runtime uses instead of allocating. gc passes
// the address of a frame buffer for a map that does not escape and this passes
// nil, for the reason the allocation section gives: specs/023-escape-analysis.md
// is not built, so the heap is the answer that is always correct.
//
// runtime.makemap64 is unreachable on this target. gc reaches it only where int
// is narrower than the hint's own type, and specs/030-abi.md makes int 64 bits.
// A hint above the largest int, written as a uint64, converts to a negative int
// and makemap clamps it to zero, which is what gc does with it too.
//
// runtime.makemap_small is not built either. It is a size optimisation for a
// map with no hint, and it returns a map with no descriptor recorded, so the
// first mapassign would still need the descriptor this row already names.
func (l *lowerer) makeMap(n Expr) Expr {
	t, pos := n.Type, n.Pos
	if t.Key == nil || t.Elem == nil {
		l.refuse(n, "a make of a map with no key type or no element type")
		return n
	}
	if len(n.Args) > 1 {
		l.refuse(n, fmt.Sprintf("a make of a map with %d bounds", len(n.Args)))
		return n
	}
	hint := intConst(pos, lowerInt, 0)
	if len(n.Args) == 1 {
		hint = l.widen(n.Args[0])
	}
	return l.newMap(n, t, hint, pos)
}

// newMap emits the makemap call for a map type and a hint.
func (l *lowerer) newMap(n Expr, t *Type, hint Expr, pos syntax.Pos) Expr {
	desc, why := l.descriptor(t, pos)
	if desc == nil {
		l.refuse(n, why)
		return nil
	}
	call := &Node{
		Op: OCall, Pos: pos, Type: lowerUnsafePtr,
		X: &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.makemap")},
		// The third parameter is the buffer, and nil asks the runtime to
		// allocate. It is spelled as a pointer rather than as a number so that
		// the argument bitmap of the call describes it as one.
		Args: []Expr{desc, hint, l.zeroOf(lowerUnsafePtr, pos)},
	}
	// The value is the *maps.Map the call returns, and a map is one word
	// (specs/030-abi.md), so the conversion is a relabelling and not a copy.
	return &Node{Op: OConvert, Pos: pos, Type: t, X: call}
}

// mapLit builds a map literal.
//
// The literal is a fresh map and one insert per element, in the order the
// source wrote them, which is what the specification requires of a literal
// whose keys are not constant: the keys and the values are evaluated in
// order, and a duplicate non-constant key leaves the last one.
//
// The hint is the number of elements, so the table is sized once rather than
// grown per insert. gc builds a static array of keys and values for a large
// literal and loops over it, which is a code-size choice over the same
// semantics and is not built here.
func (l *lowerer) mapLit(n Expr) Expr {
	t, pos := n.Type, n.Pos
	if t.Key == nil || t.Elem == nil {
		l.refuse(n, "a map literal with no key type or no element type")
		return n
	}
	made := l.newMap(n, t, intConst(pos, lowerInt, int64(len(n.Args))), pos)
	if made == nil {
		return n
	}
	m := l.spill(made)
	for _, e := range n.Args {
		if e == nil || e.Op != OAssign || e.X == nil || e.Y == nil {
			l.refuse(n, "an element of a map literal is not a key and a value")
			return n
		}
		if !l.mapAssign(e.Pos, &Node{Op: OIndex, Pos: e.Pos, Type: t.Elem, X: ref(m, pos), Y: e.X}, e.Y) {
			return n
		}
	}
	return ref(m, pos)
}

// mapLen builds len(m).
//
// It is the one place in this file that reads the runtime's maps.Map, and the
// dependency is taken rather than avoided because there is nothing to avoid it
// with: no runtime symbol returns a map's length, gc reads the word inline
// (cmd/compile/internal/ssagen.referenceTypeBuiltin), and the runtime writes
// "Must be first (known by the compiler, for len() builtin)" above the field.
// A layout the runtime documents as the compiler's is a layout the compiler is
// meant to read.
//
// The nil check is not optional. len of a nil map is zero and a nil map has no
// maps.Map to read the word out of, so the load has to be behind the check. gc
// emits the same branch and marks it unlikely.
//
//	n := 0
//	if m != nil {
//		n = int(*(*uint64)(m))
//	}
//
// The field is a uint64 and len is an int. Both are 64 bits on this target
// (specs/030-abi.md), so the conversion is a relabelling, and it is written
// down because a target where they differ would need the truncation gc emits.
func (l *lowerer) mapLen(n Expr) Expr {
	pos := n.Pos
	m := l.stable(n.X)
	out := l.tempObj(n.Type, pos)
	l.emit(define(pos, ref(out, pos), intConst(pos, n.Type, 0)))
	word := &Node{Op: ODeref, Pos: pos, Type: lowerUint64,
		X: &Node{Op: OConvert, Pos: pos, Type: l.ptrTo(lowerUint64), X: l.mapArg(m)}}
	l.emit(&Node{
		Op: OIf, Pos: pos, Type: voidType,
		X: &Node{Op: OCompare, Op1: syntax.Neq, Pos: pos, Type: lowerBool,
			X: l.mapArg(cloneExpr(m)), Y: l.zeroOf(lowerUnsafePtr, pos)},
		Body: []Stmt{Assign(pos, ref(out, pos),
			&Node{Op: OConvert, Pos: pos, Type: n.Type, X: word})},
	})
	return ref(out, pos)
}

// mapClear builds clear(m).
func (l *lowerer) mapClear(n Expr) Expr {
	pos := n.Pos
	m := l.stable(n.X)
	desc, why := l.descriptor(m.Type, pos)
	if desc == nil {
		l.refuse(n, why)
		return n
	}
	return runtimeCall(pos, "runtime.mapclear", desc, l.mapArg(m))
}

// mapIterType is internal/runtime/maps.Iter, which the compiler builds because
// the collector needs a pointer map for it and only a type carries one.
//
// It must stay in step with internal/runtime/maps/table.go, and
// cmd/compile/internal/reflectdata.MapIterType is gc's copy of the same
// declaration. Six of the twelve words hold pointers, and a frame slot
// described with the wrong six lets the collector free a table an iteration is
// walking.
//
//	type Iter struct {
//		key   unsafe.Pointer // must be first
//		elem  unsafe.Pointer // must be second
//		typ   unsafe.Pointer
//		m     *Map
//		groupSlotOffset uint64
//		dirOffset       uint64
//		clearSeq        uint64
//		globalDepth     uint8
//		dirIdx          int
//		tab             *table
//		group           unsafe.Pointer
//		entryIdx        uint64
//	}
//
// The two named pointers are named because walk/range.go reads them: key is the
// loop's condition and both are the iteration variables. The rest are the
// runtime's and are here only so that the frame slot is the right size, the
// right alignment and the right pointer map.
//
// It carries no name, for the reason rtypeType and scaseType carry none: a name
// would make TypeSymbol produce a descriptor gc never emits.
var mapIterType = func() *Type {
	t := &Type{Kind: Struct, Fields: []Field{
		{Name: "key", Type: lowerUnsafePtr},
		{Name: "elem", Type: lowerUnsafePtr},
		{Name: "typ", Type: lowerUnsafePtr},
		{Name: "m", Type: lowerUnsafePtr},
		{Name: "groupSlotOffset", Type: lowerUint64},
		{Name: "dirOffset", Type: lowerUint64},
		{Name: "clearSeq", Type: lowerUint64},
		{Name: "globalDepth", Type: lowerByte},
		{Name: "dirIdx", Type: lowerInt},
		{Name: "tab", Type: lowerUnsafePtr},
		{Name: "group", Type: lowerUnsafePtr},
		{Name: "entryIdx", Type: lowerUint64},
	}}
	if err := Layout(t); err != nil {
		panic("ir: maps.Iter does not lay out: " + err.Error())
	}
	if t.Size != 96 {
		panic(fmt.Sprintf("ir: maps.Iter is %d bytes and internal/runtime/maps declares 96", t.Size))
	}
	return t
}()

// The fields of a maps.Iter that this file reads.
const (
	iterKey  = 0
	iterElem = 1
)

// rangeMap builds a range over a map.
//
// specs/020-ir.md's row and specs/031's shape:
//
//	var it maps.Iter
//	runtime.mapIterStart(desc, m, &it)
//	for it.key != nil {
//		k := *(*K)(it.key)
//		v := *(*V)(it.elem)
//		body
//		runtime.mapIterNext(&it)
//	}
//
// mapIterStart and not mapiterinit. runtime.mapiterinit still exists, as a
// //go:linkname shim in runtime/linkname_shim.go, and it takes a
// *runtime.linknameIter rather than a *maps.Iter. The two are different structs
// with different layouts, so a call built for that name would write through the
// wrong offsets into this frame.
//
// The iterator is cleared before the call. maps.Iter.Init returns early for a
// nil or empty map without touching key, so a slot holding a key from a
// previous execution of the same statement would make the loop run again over a
// map that has nothing in it. gc clears it too, in order.go's newTemp.
//
// The loop is not an index loop: a map has no length to stop at and no element
// to index. It ends when the iteration sets key to nil, which is the same fact
// maps.Iter.Key reports.
func (l *lowerer) rangeMap(n *Node, x Expr) {
	pos := n.Pos
	key, elem, why := mapKeyElem(x)
	if why != "" {
		l.refuse(n, why)
		l.emit(l.pop()...)
		l.emit(n)
		return
	}
	desc, why := l.descriptor(x.Type, pos)
	if desc == nil {
		l.refuse(n, why)
		l.emit(l.pop()...)
		l.emit(n)
		return
	}
	m := l.stable(x)
	it := l.tempObj(mapIterType, pos)
	l.zero(it, pos)
	l.emit(runtimeCall(pos, "runtime.mapIterStart", desc, l.mapArg(m),
		&Node{Op: OConvert, Pos: pos, Type: lowerUnsafePtr, X: l.addrOf(ref(it, pos))}))
	// The setup goes in the loop's own init list and not in front of the loop,
	// for the reason rangeStmt gives: a statement between a label and the loop
	// takes the name the loop needs.
	init := l.pop()

	iterField := func(i int) Expr {
		return &Node{Op: OField, Pos: pos, Type: lowerUnsafePtr, X: ref(it, pos), Index: i}
	}

	l.push()
	// The destinations are written before the body, so that the per-iteration
	// declaration ir.Build put at the head of the body copies a value that is
	// already there.
	if len(n.Args) > 0 && n.Args[0] != nil {
		l.store(pos, 0, n.Args[0], l.mapElem(iterField(iterKey), key, pos))
	}
	if len(n.Args) > 1 && n.Args[1] != nil {
		l.store(pos, 0, n.Args[1], l.mapElem(iterField(iterElem), elem, pos))
	}
	for _, st := range n.Body {
		l.stmt(st)
	}
	body := l.pop()

	l.emit(&Node{
		Op: OFor, Pos: pos, Type: voidType, Init: init,
		X: &Node{Op: OCompare, Op1: syntax.Neq, Pos: pos, Type: lowerBool,
			X: iterField(iterKey), Y: l.zeroOf(lowerUnsafePtr, pos)},
		Body: body,
		Post: []Stmt{runtimeCall(pos, "runtime.mapIterNext",
			&Node{Op: OConvert, Pos: pos, Type: lowerUnsafePtr, X: l.addrOf(ref(it, pos))})},
	})
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
		// The map loop has no index and no bound, so it is built whole rather
		// than through the index loop below. The statements the operand needed
		// are on the sink and rangeMap takes them.
		l.rangeMap(n, x)
		return
	case x.Type.Kind == Chan:
		// The channel loop has no index and no bound, so it is built whole
		// rather than through the index loop below. The statements the
		// operand needed are on the sink and rangeChan takes them.
		l.rangeChan(n, x)
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
		l.store(n.Pos, 0, n.Args[0], ref(idx, n.Pos))
	}
	if len(n.Args) > 1 && n.Args[1] != nil {
		if elem == nil {
			l.refuse(n, "a range over an integer with two variables")
			l.pop()
			l.emit(loop.Init...)
			l.emit(n)
			return
		}
		l.store(n.Pos, 0, n.Args[1], elem(ref(idx, n.Pos)))
	}
	for _, s := range n.Body {
		l.stmt(s)
	}
	loop.Body = l.pop()
	l.emit(loop)
}

// rangeChan builds a range over a channel.
//
// specs/020-ir.md's row, and it is not an index loop: a channel has no length
// to stop at and no element to index. The loop runs until the receive says the
// channel is closed and drained, which is the same fact runtime.chanrecv2
// returns to the two-value receive.
//
//	ha := c
//	for {
//		hv, hb := <-ha
//		if !hb {
//			break
//		}
//		v := hv
//		body
//	}
//
// The channel is evaluated once, before the loop, which is what the
// specification requires of every range expression: an assignment inside the
// body cannot change what is iterated.
//
// The receive is emitted whether or not the clause asked for the value.
// "for range c" drains the channel, so a loop that skipped the call would
// either spin or never end.
func (l *lowerer) rangeChan(n *Node, x Expr) {
	pos := n.Pos
	elem, why := chanElem(x)
	if elem == nil {
		l.refuse(n, why)
		l.emit(l.pop()...)
		l.emit(n)
		return
	}
	c := l.spill(x)
	// The operand's temporaries go in the loop's own init list and not in
	// front of the loop, for the reason rangeStmt gives: a statement between a
	// label and the loop takes the name the loop needs.
	init := l.pop()

	l.push()
	arg := l.chanArg(ref(c, pos))
	o, addr := l.elemSlot(elem, pos)
	got := l.tempObj(lowerBool, pos)
	l.emit(define(pos, ref(got, pos), &Node{
		Op: OCall, Pos: pos, Type: lowerBool,
		X:    &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.chanrecv2")},
		Args: []Expr{arg, addr},
	}))
	l.emit(&Node{
		Op: OIf, Pos: pos, Type: voidType,
		X: &Node{Op: OUnary, Op1: syntax.Not, Pos: pos, Type: lowerBool, X: ref(got, pos)},
		// An unlabelled break, which ssa.Build binds to the innermost loop,
		// and this loop is it. A break the source wrote in the body binds to
		// the same one, which is what the language says it does.
		Body: []Stmt{{Op: OBreak, Pos: pos, Type: voidType}},
	})
	// The destination is written before the body, so that the per-iteration
	// declaration ir.Build put at the head of the body copies a value that is
	// already there.
	if len(n.Args) > 0 && n.Args[0] != nil {
		l.store(pos, 0, n.Args[0], ref(o, pos))
	}
	if len(n.Args) > 1 && n.Args[1] != nil {
		l.refuse(n, "a range over a channel with two variables")
	}
	for _, s := range n.Body {
		l.stmt(s)
	}
	body := l.pop()

	// No condition. The loop leaves through the break above and through
	// nothing else, which is what makes the receive the only exit.
	l.emit(&Node{Op: OFor, Pos: pos, Type: voidType, Init: init, Body: body})
}

// select.
//
// specs/020-ir.md's row and specs/031-runtime-lowering.md's shape:
// runtime.selectgo over an array of cases, and a jump table on the index it
// returns. Everything hard about the row is in the two frame arrays.
//
// # The arrays
//
// selectgo takes a [ncases]scase and a [2*ncases]uint16, and it requires both
// to be on the goroutine's stack. scase is two words, the channel and the
// address of the element, and both of them are pointers. That is the part
// specs/031 calls easy to get wrong: an scase described as holding no pointer
// lets the collector free a channel a goroutine is parked in, or free the
// object the element points at, and neither failure is at the mistake.
//
// The arrays are written whole on every execution rather than cleared first. A
// select inside a loop reaches the same frame slots on the next iteration, and
// a field left over from the iteration before would name a channel that
// iteration chose.
//
// # The order of the cases
//
// selectgo needs the sends first and the receives after, and it is told how
// many of each. Within each group the order is free: selectgo shuffles the
// cases into a poll order of its own, which is what makes a select with two
// ready cases pick between them at random. So a send takes the next index from
// the front of the array and a receive the next after the sends, both in
// source order, and the index each clause was given is the case value of the
// switch below.
//
// # What is evaluated when
//
// The specification evaluates the channel operand of every clause, and the
// value of every send, exactly once and in source order on entry. The
// destinations of a receive are not in that list: they are written only if
// that communication happens. So the element of a receive lands in a frame
// slot of the row's own, and the assignment to what the source wrote is in the
// chosen arm of the switch. gc takes the address of the destination instead
// and hands it to selectgo, which evaluates the destination on entry.

// scaseType is runtime.scase: the channel and the address of the element.
//
// It carries no name, for the reason rtypeType carries none: a name would make
// TypeSymbol produce a descriptor gc never emits and the linker would never
// resolve. Nothing asks this type for one today, and a stack object table
// would.
var scaseType = func() *Type {
	t := &Type{Kind: Struct, Fields: []Field{
		{Name: "c", Type: lowerUnsafePtr},
		{Name: "elem", Type: lowerUnsafePtr},
	}}
	if err := Layout(t); err != nil {
		panic("ir: runtime.scase does not lay out: " + err.Error())
	}
	return t
}()

// The fields of an scase, in the order runtime/select.go declares them.
const (
	scaseChan = 0
	scaseElem = 1
)

// selectResult is what runtime.selectgo returns: the index of the chosen case,
// and whether a receive received a value rather than finding the channel
// closed.
var selectResult = func() *Type {
	t := &Type{Kind: Tuple, Fields: []Field{
		{Name: "r0", Type: lowerInt},
		{Name: "r1", Type: lowerBool},
	}}
	if err := Layout(t); err != nil {
		panic("ir: the result of runtime.selectgo does not lay out: " + err.Error())
	}
	return t
}()

// selectCase is one communication clause, taken apart.
type selectCase struct {
	clause *Node  // the OCase the source wrote
	pre    []Stmt // the operand temporaries ir.Build hoisted out of it
	post   []Stmt // the destination copies it emitted after the communication
	send   bool
	ch     Expr
	val    Expr   // the sent value, for a send
	dsts   []Expr // the destinations, for a receive
	op1    syntax.Operator
	index  int     // the position this case takes in the scase array
	slot   *Object // the frame slot a receive lands in
}

// selectStmt builds a select.
func (l *lowerer) selectStmt(n *Node) {
	pos := n.Pos
	l.flush(n)
	bail := func(what string) {
		l.refuse(n, what)
		l.emit(n)
	}
	if len(n.Body) == 0 {
		// select {} parks the goroutine for good, which is runtime.block.
		bail("a select with no clauses blocks for good, which is runtime.block, and rtsym does not carry it")
		return
	}

	var cases []*selectCase
	var deflt *Node
	for _, c := range n.Body {
		if c == nil || c.Op != OCase {
			bail("a select whose clause is not a case")
			return
		}
		// ir.Build puts the communication statement in the clause's Init,
		// with the operand temporaries it needed in front of it and, where a
		// destination's type differs from the value's, the copies that convert
		// after it. So a clause with no Init is the default, and the rest is
		// three parts.
		if len(c.Init) == 0 {
			if deflt != nil {
				bail("a select with two default clauses")
				return
			}
			deflt = c
			continue
		}
		at := commIndex(c.Init)
		if at < 0 {
			bail("a select clause whose communication is a " + c.Init[len(c.Init)-1].Op.String())
			return
		}
		comm := c.Init[at]
		// What comes before the communication is evaluated on entry, which is
		// where the specification evaluates a channel operand and a sent
		// value. What comes after it copies the received value into a
		// destination whose type is not the element's, and that runs only if
		// this communication is the one that happened.
		k := &selectCase{clause: c, pre: c.Init[:at], post: c.Init[at+1:]}
		switch {
		case comm.Op == OSend:
			k.send, k.ch, k.val = true, comm.X, comm.Y
		case comm.Op == ORecv:
			// case <-c: with no destination at all.
			k.ch = comm.X
		case comm.Op == OAssign && comm.Y != nil && comm.Y.Op == ORecv:
			k.ch, k.op1 = comm.Y.X, comm.Op1
			if comm.X != nil {
				k.dsts = []Expr{comm.X}
			} else {
				k.dsts = comm.Args
			}
		}
		cases = append(cases, k)
	}

	if len(cases) == 0 {
		// Only a default clause, which always runs. It is still wrapped in a
		// switch, because a break the clause writes leaves the select and
		// would otherwise leave whatever encloses it.
		l.emit(&Node{Op: OSwitch, Pos: pos, Type: voidType,
			Body: []Stmt{{Op: OCase, Pos: deflt.Pos, Type: voidType, Body: l.stmts(deflt.Body)}}})
		return
	}

	nsends := 0
	for _, k := range cases {
		if k.send {
			nsends++
		}
	}
	ncas := len(cases)

	// The setup goes in the switch's own init list rather than in front of it,
	// for the reason rangeChan gives: a statement between a label and the
	// statement it names takes the label, and "break L" would then find no
	// select.
	l.push()
	selv := l.tempObj(l.arrayOf(scaseType, int64(ncas)), pos)
	order := l.tempObj(l.arrayOf(lowerUint16, int64(2*ncas)), pos)

	send, recv := 0, nsends
	for _, k := range cases {
		for _, s := range k.pre {
			l.stmt(s)
		}
		if k.send {
			k.index, send = send, send+1
		} else {
			k.index, recv = recv, recv+1
		}
		ch := l.expr(k.ch)
		elem, why := chanElem(ch)
		if elem == nil {
			l.refuse(n, why)
			l.emit(l.pop()...)
			l.emit(n)
			return
		}
		l.emit(Assign(pos, l.scaseField(selv, k.index, scaseChan, pos), l.chanArg(ch)))
		var addr Expr
		if k.send {
			if k.val == nil || k.val.Type == nil {
				l.refuse(n, "a select clause sends an operand with no type")
				l.emit(l.pop()...)
				l.emit(n)
				return
			}
			addr = l.valueAddr(l.expr(k.val))
		} else {
			k.slot, addr = l.elemSlot(elem, pos)
		}
		l.emit(Assign(pos, l.scaseField(selv, k.index, scaseElem, pos), addr))
	}

	chosen := l.tempObj(lowerInt, pos)
	received := l.tempObj(lowerBool, pos)
	l.emit(&Node{
		Op: OAssign, Pos: pos, Type: voidType,
		Args: []Expr{ref(chosen, pos), ref(received, pos)},
		Y: &Node{
			Op: OCall, Pos: pos, Type: selectResult,
			X: &Node{Op: OGlobal, Pos: pos, Type: funcType, Obj: runtimeFunc("runtime.selectgo")},
			Args: []Expr{
				l.arrayBase(selv, pos),
				l.arrayBase(order, pos),
				// The third array is the caller's program counters, which the
				// race detector reads and nothing else writes. gc passes nil
				// unless the build is instrumented, and this compiler has no
				// instrumented build.
				l.zeroOf(l.ptrTo(lowerUintptr), pos),
				intConst(pos, lowerInt, int64(nsends)),
				intConst(pos, lowerInt, int64(ncas-nsends)),
				// A select with a default clause does not block, and selectgo
				// reports the default by returning an index below zero.
				boolConst(pos, deflt == nil),
			},
		},
	})
	init := l.pop()

	// The jump table. The switch's default arm is the select's default clause:
	// the index is one of the cases or it is below zero, and below zero is
	// what selectgo returns when nothing was ready.
	clauses := make([]Stmt, 0, len(cases)+1)
	for _, k := range cases {
		l.push()
		if len(k.dsts) > 0 {
			l.selectAssign(k, 0, ref(k.slot, pos), pos)
		}
		if len(k.dsts) > 1 {
			l.selectAssign(k, 1, ref(received, pos), pos)
		}
		for _, s := range k.post {
			l.stmt(s)
		}
		for _, s := range k.clause.Body {
			l.stmt(s)
		}
		clauses = append(clauses, &Node{Op: OCase, Pos: k.clause.Pos, Type: voidType,
			Args: []Expr{intConst(pos, lowerInt, int64(k.index))}, Body: l.pop()})
	}
	if deflt != nil {
		clauses = append(clauses, &Node{Op: OCase, Pos: deflt.Pos, Type: voidType,
			Body: l.stmts(deflt.Body)})
	}
	l.emit(&Node{Op: OSwitch, Pos: pos, Type: voidType,
		Init: init, X: ref(chosen, pos), Body: clauses})
}

// commIndex returns the position of the communication statement in a select
// clause's init list, or a negative number when there is none.
//
// The search runs from the end and not from the front, and the direction is
// the whole of what makes it right. A statement in front of the communication
// may be a receive of its own: ir.Build hoists the operand of "case c <- <-d:"
// into a temporary, and that temporary is assigned from a receive. A statement
// after it never is: what follows is a copy out of a temporary the
// communication wrote.
func commIndex(list []Stmt) int {
	for i := len(list) - 1; i >= 0; i-- {
		s := list[i]
		if s == nil {
			continue
		}
		if s.Op == OSend || s.Op == ORecv ||
			(s.Op == OAssign && s.Y != nil && s.Y.Op == ORecv) {
			return i
		}
	}
	return -1
}

// selectAssign writes one destination of a chosen receive clause.
//
// A destination that names no storage is not written, which is the rule the
// two-value receive follows: the communication has already happened and the
// store would be into a location of no size.
func (l *lowerer) selectAssign(k *selectCase, i int, val Expr, pos syntax.Pos) {
	dst := k.dsts[i]
	if dst == nil || namesNoStorage(dst) {
		return
	}
	l.store(pos, k.op1, dst, val)
}

// arrayOf returns the array type of n elements of t.
func (l *lowerer) arrayOf(t *Type, n int64) *Type {
	a := &Type{Kind: Array, Elem: t, Len: n}
	if err := Layout(a); err != nil {
		panic("ir: an array of " + fmt.Sprint(n) + " does not lay out: " + err.Error())
	}
	return a
}

// arrayBase returns the address of the first element of a frame array, as the
// pointer the runtime takes.
func (l *lowerer) arrayBase(o *Object, pos syntax.Pos) Expr {
	elem := &Node{Op: OIndex, Pos: pos, Type: o.Type.Elem,
		X: ref(o, pos), Y: intConst(pos, lowerInt, 0)}
	return &Node{Op: OConvert, Pos: pos, Type: lowerUnsafePtr, X: l.addrOf(elem)}
}

// scaseField returns one field of one element of the case array.
func (l *lowerer) scaseField(o *Object, i, field int, pos syntax.Pos) Expr {
	elem := &Node{Op: OIndex, Pos: pos, Type: scaseType,
		X: ref(o, pos), Y: intConst(pos, lowerInt, int64(i))}
	return &Node{Op: OField, Pos: pos, Type: lowerUnsafePtr, X: elem, Index: field}
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
// specs/020-ir.md: bounds checks plus pointer arithmetic. No allocation: the
// result points into storage the operand already named, which is why this row
// was built before a descriptor could be named and the composite literal rows
// had to wait for one.
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

// clearExpr builds the clear builtin.
//
// specs/020-ir.md gives clear two rows and both are here. A slice is a memory
// clear over its own elements, and a map is runtime.mapclear: the runtime owns
// the table's layout, so there is no region for this pass to clear.
func (l *lowerer) clearExpr(n Expr) Expr {
	x := n.X
	if x == nil || x.Type == nil {
		l.refuse(n, "a clear with no operand")
		return n
	}
	if x.Type.Kind == Map {
		return l.mapClear(n)
	}
	if x.Type.Kind != Slice {
		l.refuse(n, "a clear of "+x.Type.Kind.String())
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

// Closures, defer and go.
//
// specs/033-closures-defer-panic.md is the design. Three of its rows are built
// here and the rest are refused, and the line between them is one machine
// fact: a closure that captures reads its captured variables through the
// context register, R26 on arm64 (specs/030-abi.md), and no operation of
// specs/021-ssa-construction.md's set reads that register in the callee.
// ssagen writes it at every indirect call site, so the caller half is there
// and the callee half is not.
//
// What that leaves reachable is exact rather than approximate:
//
//   - A function literal that captures nothing. Its body never reads the
//     context register, so the value is a funcval and nothing else.
//   - defer and go of a call with no arguments, through a function symbol or
//     through a value of function type. Neither needs a capture, and the value
//     may be a closure gc compiled, because the runtime calls it and the
//     runtime sets the register.
//
// Everything else is refused with the count of what would have to be captured,
// so the corpus report says how much of the row the register is holding back
// rather than only that the row is unbuilt.
//
// # Why the funcval is on the heap
//
// gc emits a read-only one-word symbol for a literal that captures nothing, so
// the value is a link-time constant. This pass allocates one per evaluation
// instead, because LowerAndCollect's only channel to the object writer carries
// type descriptors and there is no channel for a one-word data symbol. That is
// the cost: a heap allocation and a store where gc has neither. It is correct,
// and the fix is a channel for data symbols rather than anything here.

// deferExitLabel is the label of the epilogue a function with a defer leaves
// through.
//
// It is not a Go identifier, so no source label collides with it.
const deferExitLabel = ".deferexit"

// closureExpr lowers a function literal.
func (l *lowerer) closureExpr(n Expr) Expr {
	if n.Index != closureLiteral {
		// A method value binds its receiver by value and not through a cell,
		// so the receiver is a copy the closure object holds rather than a
		// variable two functions share. closure.go builds the cell form and
		// this is not it.
		l.refuse(n, "a method value captures its receiver by value, and only a capture through a heap cell is built")
		return n
	}
	if n.Obj == nil || n.Obj.Class != ClassFunc {
		l.refuse(n, "a function literal with no symbol")
		return n
	}
	if len(n.Args) == 0 {
		return l.funcValue(n, n.Obj, n.Type)
	}
	if l.hasUncelledCapture(n.Args) {
		// The refusal was reported where the cell was refused. The node stays
		// where it is, so that construction reports it too rather than
		// building a closure object with a hole in it.
		return n
	}
	return l.closureValue(n, n.Obj, n.Args, n.Type)
}

// funcValue builds the func value of a function that captures nothing.
//
// A funcval is a code pointer followed by the captured variables, so with no
// captures it is one word. The word is uintptr and not a pointer: it holds a
// text address, which the collector must not trace and must not be asked to,
// and uintptr is the only spelling that says so to the descriptor
// (specs/032-type-descriptors-and-itabs.md derives GCData from the type).
func (l *lowerer) funcValue(n Expr, o *Object, t *Type) Expr {
	pos := n.Pos
	cell := l.allocate(n, lowerUintptr, pos)
	if cell == nil {
		return n
	}
	p := l.spill(cell)
	entry := &Node{
		Op: OAddr, Pos: pos, Type: l.ptrTo(o.Type),
		X: &Node{Op: OGlobal, Pos: pos, Type: o.Type, Obj: o},
	}
	l.emit(Assign(pos,
		&Node{Op: ODeref, Pos: pos, Type: lowerUintptr, X: ref(p, pos)},
		&Node{Op: OConvert, Pos: pos, Type: lowerUintptr, X: entry}))
	return &Node{Op: OConvert, Pos: pos, Type: t, X: ref(p, pos)}
}

// isFuncSymbol reports whether n names a function rather than a value of
// function type.
//
// It is the one shape ssa.Build turns into a direct call, and it is also the
// shape that must not survive anywhere else: a function symbol in a value
// position is a funcval.
func isFuncSymbol(n Expr) bool {
	return n != nil && n.Op == OGlobal && n.Obj != nil && n.Obj.Class == ClassFunc
}

// deferStmt lowers defer and go into the runtime call that takes a funcval.
//
// runtime.deferproc and runtime.newproc take the same one word, which is why
// the two statements share this function: deferproc's parameter is a func()
// and newproc's is a *funcval, and both are the address of the code pointer.
// The difference is what the function owes afterwards, and only defer owes the
// exit deferExit builds.
func (l *lowerer) deferStmt(n Expr, name string) Expr {
	fv := l.deferredValue(n)
	if fv == nil {
		return n
	}
	if name == "runtime.deferproc" {
		l.ndefer++
	}
	return runtimeCall(n.Pos, name, fv)
}

// deferredValue returns the func value a defer or a go statement calls.
//
// The builder has already evaluated the callee and the arguments into
// temporaries, because the specification evaluates both when the statement
// runs and not when the call runs. What is left here is turning the callee
// into one word.
func (l *lowerer) deferredValue(n Expr) Expr {
	call := n.X
	if call == nil {
		l.refuse(n, "the statement holds nothing to call")
		return nil
	}
	// The call's own scaffolding runs at the statement, which is where the
	// specification evaluates the operands.
	l.flush(call)
	if call.Op != OCall {
		// A builtin. "defer close(c)" is an OClose and not a call to a
		// function value, so there is no funcval to hand the runtime, and gc
		// wraps it in a literal that captures the operand.
		l.refuse(n, "the statement holds a "+call.Op.String()+" and not a call, so there is no function value to give the runtime")
		return nil
	}
	if len(call.Args) > 0 {
		// ir.Build puts the operands of a defer or a go inside a literal, so
		// a call reaching here with operands was built by hand.
		l.refuse(n, fmt.Sprintf("an argument list of %d, which ir.Build wraps in a literal", len(call.Args)))
		return nil
	}
	fun := call.X
	if fun == nil || fun.Type == nil {
		l.refuse(n, "a call with no function")
		return nil
	}
	if fun.Op == OGlobal && fun.Obj != nil && fun.Obj.Class == ClassFunc {
		return l.funcValue(n, fun.Obj, fun.Type)
	}
	if fun.Type.Kind != FuncKind {
		l.refuse(n, "a call through "+fun.Type.Kind.String())
		return nil
	}
	switch fun.Op {
	case OClosure:
		// The literal ir.Build wrapped the statement's operands in, or one
		// the program wrote. Either way it is a func value, and the runtime
		// calls it with nothing.
		return l.expr(fun)
	case OLocal:
		// A temporary the builder wrote at the statement, which is the value
		// the specification says the call uses.
		return fun
	case OGlobal:
		// A package-level variable of function type. The builder does not
		// snapshot it, and the value it holds when the call runs is not the
		// value the statement saw, so it is copied here.
		return ref(l.spill(fun), fun.Pos)
	case OField:
		if fun.X != nil && fun.X.Type != nil && fun.X.Type.Kind == Interface {
			// A method of an interface keeps its receiver inside the
			// selection, so the value the runtime would be given is a method
			// value: the receiver bound to the function the itab names.
			// closureExpr refuses a method value for the same reason.
			l.refuse(n, "a method of an interface is a method value, which binds its receiver by value, and only a capture through a heap cell is built")
			return nil
		}
		l.refuse(n, "a call through a field of a "+fun.X.Type.Kind.String()+", which the builder does not snapshot")
		return nil
	}
	l.refuse(n, "a call through a "+fun.Op.String())
	return nil
}

// deferExit gives a function that defers one exit, and puts the call to
// runtime.deferreturn in it.
//
// One exit and not one call before each return, which is not a tidiness
// choice. cmd/link records the offset of a call to runtime.deferreturn in the
// function's funcInfo, and it records the **first** one it finds
// (cmd/link/internal/ld/pcln.go, computeDeferReturn). runtime.recovery jumps
// to that offset when a deferred function recovers, so a function with two
// deferreturn call sites resumes at the wrong one: it would run the epilogue
// of a return the program did not take. That failure appears only on the
// panic path, which is the one ordinary tests do not take.
//
// So every return writes the result objects and jumps to the epilogue, and the
// epilogue calls deferreturn once and returns what the result objects hold.
// The bare return is what ssa.Build already builds for a named result.
//
// # Why the results move to the frame
//
// runtime.recovery restores the stack pointer and jumps to that offset. It
// restores no register, because it has none to restore: the registers of the
// frame it resumes were the panicking call's and are gone. A result that lived
// in a register at the epilogue would therefore be whatever the runtime left
// behind. Marking the results address-taken puts them in the frame, which
// specs/021-ssa-construction.md's classify reads, so the epilogue loads them
// from storage that survives the unwind. It is also what makes a deferred
// function able to assign a named result, which is the other half of the rule
// gc applies here.
func (l *lowerer) deferExit() {
	pos := l.fn.Pos
	for _, r := range l.fn.Results {
		r.Addrtaken = true
	}
	l.fn.Body = l.exitReturns(l.fn.Body)
	l.fn.Body = append(l.fn.Body,
		&Node{Op: OLabel, Pos: pos, Type: voidType, Label: deferExitLabel},
		runtimeCall(pos, "runtime.deferreturn"),
		&Node{Op: OReturn, Pos: pos, Type: voidType})
}

// exitReturns rewrites every return of a statement list into a jump to the
// epilogue, recursing into the lists a statement holds.
//
// A return is only ever in one of those lists: the lowering pass has already
// flushed the scaffolding out of every Init that could hold a statement, and
// the language puts no return in a for statement's post list.
func (l *lowerer) exitReturns(list []Stmt) []Stmt {
	if len(list) == 0 {
		return list
	}
	out := make([]Stmt, 0, len(list))
	for _, s := range list {
		if s == nil {
			continue
		}
		if s.Op == OReturn {
			out = append(out, l.exitReturn(s)...)
			continue
		}
		s.Init = l.exitReturns(s.Init)
		s.Body = l.exitReturns(s.Body)
		s.Else = l.exitReturns(s.Else)
		s.Post = l.exitReturns(s.Post)
		out = append(out, s)
	}
	return out
}

// exitReturn is one return, rewritten.
func (l *lowerer) exitReturn(s Stmt) []Stmt {
	pos := s.Pos
	res := l.fn.Results
	var out []Stmt
	switch {
	case len(s.Args) == 0:
		// A bare return, which leaves the result objects as they are.

	case len(s.Args) == len(res):
		if len(res) == 1 {
			out = append(out, Assign(pos, ref(res[0], pos), s.Args[0]))
			break
		}
		// Every operand is evaluated before any result is written, because an
		// operand may read a result object: "return y, x" of named results is
		// a swap, and a store per operand in order would make it a copy.
		tmps := make([]*Object, 0, len(res))
		for _, a := range s.Args {
			o := l.tempObj(a.Type, pos)
			out = append(out, define(pos, ref(o, pos), a))
			tmps = append(tmps, o)
		}
		for i, o := range tmps {
			out = append(out, Assign(pos, ref(res[i], pos), ref(o, pos)))
		}

	default:
		// return f(), where f returns everything this function returns. It is
		// the only shape left: ir.Build gives a return one operand per result
		// or the one call that produces all of them. The call produces every
		// result before any of them is stored, so the destinations are the
		// result objects themselves.
		dst := make([]Expr, 0, len(res))
		for _, r := range res {
			dst = append(dst, ref(r, pos))
		}
		out = append(out, &Node{Op: OAssign, Pos: pos, Type: voidType, Args: dst, Y: s.Args[0]})
	}
	return append(out, &Node{Op: OGoto, Pos: pos, Type: voidType, Label: deferExitLabel})
}

// print and println.
//
// specs/020-ir.md gives the two builtins one row each, and both are the same
// bracketed sequence: runtime.printlock, one call per operand, the newline
// that only println writes, and runtime.printunlock.
//
// The lock is not decoration. The runtime writes to file descriptor 2 with no
// buffering, so two goroutines printing at once interleave inside a line, and
// the bracket is what the language's one guarantee about print rests on.
//
// # What this row cannot print yet
//
// A string operand reaches runtime.printstring and stops below this pass: a
// string constant is decomposed into the address of a data symbol, and nothing
// writes that symbol into the object. So println of a number prints and
// println of a literal does not, and the missing piece is a channel from the
// compiler to the object writer for data symbols, which is the same thing the
// funcval of a closure that captures nothing wants.
//
// An interface, a slice and a complex number are refused. Each needs a symbol
// chosen by the operand's own type, which gc instantiates per type, and this
// pass has no instantiation.

// printSym returns the runtime symbol that prints a value of t, and the type
// the operand is converted to first.
//
// One symbol per width class rather than one per type, which is what gc does:
// every signed kind widens to int64 and every unsigned kind to uint64, so a
// program that prints an int8 and one that prints an int64 call one function.
func printSym(t *Type) (string, *Type) {
	switch t.Kind {
	case Bool:
		return "runtime.printbool", t
	case Int8, Int16, Int32, Int64:
		return "runtime.printint", lowerInt64
	case Uint8, Uint16, Uint32, Uint64, Uintptr:
		// A uintptr goes to printuint and not to a symbol of its own, because
		// it is a number. gc prints it the same way.
		return "runtime.printuint", lowerUint64
	case Float32:
		return "runtime.printfloat32", t
	case Float64:
		return "runtime.printfloat64", t
	case String:
		return "runtime.printstring", t
	case Ptr, UnsafePtr, Map, Chan, FuncKind:
		// All one word, and the runtime prints the word.
		return "runtime.printpointer", lowerUnsafePtr
	}
	return "", nil
}

func (l *lowerer) printExpr(n Expr) Expr {
	pos := n.Pos
	// Every operand is evaluated before the lock is taken. An operand that
	// calls a function which prints would otherwise deadlock on printlock,
	// and gc hoists them for the same reason.
	args := make([]Expr, 0, len(n.Args))
	syms := make([]string, 0, len(n.Args))
	for _, a := range n.Args {
		if a == nil || a.Type == nil {
			l.refuse(n, "an operand with no type")
			return n
		}
		name, want := printSym(a.Type)
		if name == "" {
			l.refuse(n, "an operand of "+a.Type.Kind.String()+" needs a symbol chosen by its own type, which gc instantiates per type")
			return n
		}
		switch a.Op {
		case OConst, OLocal, OGlobal:
		default:
			a = ref(l.spill(a), a.Pos)
		}
		if want != a.Type {
			a = &Node{Op: OConvert, Pos: a.Pos, Type: want, X: a}
		}
		args = append(args, a)
		syms = append(syms, name)
	}
	l.emit(runtimeCall(pos, "runtime.printlock"))
	for i, a := range args {
		if n.Op == OPrintln && i > 0 {
			// The separator. gc writes the string constant " " here and then
			// recognises it again to reach this symbol; the symbol is what the
			// detour arrives at.
			l.emit(runtimeCall(pos, "runtime.printsp"))
		}
		l.emit(runtimeCall(pos, syms[i], a))
	}
	if n.Op == OPrintln {
		l.emit(runtimeCall(pos, "runtime.printnl"))
	}
	return runtimeCall(pos, "runtime.printunlock")
}
