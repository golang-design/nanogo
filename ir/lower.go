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
// specs/020-ir.md gives them four rows. Three are built here: the length or
// capacity word read out of a slice or string header, and a constant for an
// array or a pointer to one. The fourth, a map and a channel, is refused, and
// the two are refused for different reasons. rtsym holds runtime.chanlen and
// runtime.chancap, so a channel is waiting on the row that calls them and on
// nothing else. A map's length is a field of the runtime's maps.Map, which gc
// reads inline and rtsym holds no symbol for, so a map is waiting on a runtime
// layout this pass may not assume.
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
		l.refuse(n, "a map literal needs runtime.makemap, and the row that calls it is not built")
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
// specs/020-ir.md gives it three rows and one is built. A map needs
// runtime.makemap and the descriptor of a map type, whose tail names the
// runtime's own group type; a channel needs runtime.makechan and the
// descriptor of a channel type, whose direction specs/020's type boundary does
// not carry. Both are refused with the field that is missing.
func (l *lowerer) makeExpr(n Expr) Expr {
	t, pos := n.Type, n.Pos
	if t == nil {
		l.refuse(n, "a make with no type")
		return n
	}
	if t.Kind != Slice {
		l.refuse(n, "a make of "+t.Kind.String()+" needs a descriptor of that kind, which specs/032 does not build")
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
		bail("a range over a map needs runtime.mapIterStart and runtime.mapIterNext, and the row that calls them is not built")
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
		l.refuse(n, "a clear of "+x.Type.Kind.String()+" needs runtime.mapclear, and the row that calls it is not built")
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
