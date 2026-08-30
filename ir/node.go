// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import "golang.design/x/nanogo/syntax"

// The typed tree.
//
// specs/020-ir.md gives the node set and, more importantly, the lowering table:
// the exhaustive list of Go constructs and what each becomes. The Go-specific
// nodes here are that table's left column, and the rule the table exists to
// enforce is that none of them survives into SSA.
//
// Three differences from the syntax tree of specs/011-parser-and-ast.md decide
// the shape of everything here:
//
//  1. Implicit operations are explicit. Every implicit conversion the
//     specification permits is a node.
//  2. Names are resolved to objects. An identifier is a pointer to its
//     declaration, not a string to be looked up. Shadowing is gone.
//  3. The tree is mutated. Inlining rewrites it in place, so it is owned rather
//     than borrowed.

// Op identifies a node's operation.
type Op uint8

const (
	OpInvalid Op = iota

	// Values.
	OConst  // a constant, in Val
	OLocal  // a local variable or parameter, in Obj
	OGlobal // a package-level variable or function, in Obj
	OField  // X.Field, the field index in Index
	OIndex  // X[Index]
	ODeref  // *X
	OAddr   // &X

	// Operations.
	OUnary   // Op1 X
	OBinary  // X Op1 Y
	OCompare // X Op1 Y, always yielding bool
	OConvert // conversion of X to Type
	OCall    // Fun(Args...)

	// Control.
	OIf
	OFor
	OSwitch
	OSelect
	OBlock
	OGoto
	OLabel
	OReturn
	OBreak
	OContinue

	// Go-specific. Every one of these is a row of specs/020-ir.md's lowering
	// table, and none of them may reach specs/021-ssa-construction.md.
	ORange
	OTypeAssert
	OTypeSwitch
	OClosure
	ODefer
	OGo
	OSend
	ORecv
	OMake
	OAppend
	OCopy
	ODelete
	OPanic
	ORecover
	OLen
	OCap
	ONew
	OComplex // complex(re, im)
	OReal
	OImag
	OClear
	OMin
	OMax

	// Added after the first consumers were written. specs/020-ir.md's node set
	// omitted them, and both the SSA builder and the IR builder had to invent a
	// local convention to stand in. A convention in two packages is two
	// conventions, so they are ops.
	OAssign // Lhs = Rhs, or Lhs := Rhs when Op1 is syntax.Def
	OCase   // one clause of an OSwitch or OSelect; Args are the case
	//	expressions and no Args means default

	// Also added after the fact, and for a sharper reason: without them the
	// builder encoded a composite literal as ONew, a slice expression as
	// OIndex with extra Args, and close/print/println as calls to function
	// objects it invented. Each of those is a workaround, and a workaround in
	// the IR is a lie that every later pass has to be told about.
	OCompositeLit // T{...}; Args are the elements, Type is T
	OSlice        // X[lo:hi] and X[lo:hi:max]; Args are the three bounds, any nil
	OClose        // close(X)
	OPrint        // print(Args...)
	OPrintln      // println(Args...)

	// The unsafe intrinsics. They are operations, not calls: each lowers to
	// pointer arithmetic or to building a slice or string header, and none of
	// them reaches the runtime.
	//
	// The alternative, which the builder used until these existed, is a call
	// to a global named "unsafe.Add". That makes every later pass recognise an
	// intrinsic by matching a string, and a pass that forgets to match it
	// emits a call to a function that does not exist.
	OUnsafeAdd        // unsafe.Add(X, Y)
	OUnsafeSlice      // unsafe.Slice(X, Y)
	OUnsafeSliceData  // unsafe.SliceData(X)
	OUnsafeString     // unsafe.String(X, Y)
	OUnsafeStringData // unsafe.StringData(X)

	// ODeclare is "var x T" with no initialiser: X is the variable it
	// declares.
	//
	// It is a statement and not the absence of one, because a declaration
	// happens where it is written. The specification makes each execution of a
	// declaration a fresh variable, so a var in a loop body is zero on every
	// iteration, and a frame zeroed once at the entry gives it the previous
	// iteration's value instead. It was the absence of one until a program
	// measured it.
	//
	// It is not Go-specific. Construction writes the zero, because the choice
	// between a zero value and a clear through memory is the choice between a
	// variable that lives in a value and one that lives in the frame, and
	// specs/021-ssa-construction.md is where that is decided. Lowering may
	// still rewrite X: a captured variable lives in a heap cell, and the cell
	// is allocated where the declaration stands and comes back zeroed, so the
	// declaration then writes nothing.
	//
	// A declaration with an initialiser is an OAssign with defineOp, which
	// writes the value and needs no zero.
	ODeclare

	// ONilCheck is "X must hold something where this statement stands". It
	// panics with the fault an ordinary dereference of nil makes, and it is
	// gc's OCHECKNIL.
	//
	// The operand is an interface value and the method table is the word
	// tested, because the one program that needs the node is the method value
	// of an interface: the language evaluates the receiver where the value is
	// written, so a nil receiver has to panic there and not inside the call
	// made later (specs/033-closures-defer-panic.md). gc writes the same test
	// as OCHECKNIL(OITAB(i)), in two nodes, because its IR has a node for the
	// table. This one has none, and ssa.Build's OpITab is the operation that
	// reads one, so the two are one node here.
	//
	// It is not Go-specific. It survives to specs/021-ssa-construction.md,
	// which turns it into the nil check every dereference already gets.
	ONilCheck

	opCount
)

var opNames = [...]string{
	OpInvalid:         "invalid",
	OConst:            "const",
	OLocal:            "local",
	OGlobal:           "global",
	OField:            "field",
	OIndex:            "index",
	ODeref:            "deref",
	OAddr:             "addr",
	OUnary:            "unary",
	OBinary:           "binary",
	OCompare:          "compare",
	OConvert:          "convert",
	OCall:             "call",
	OIf:               "if",
	OFor:              "for",
	OSwitch:           "switch",
	OSelect:           "select",
	OBlock:            "block",
	OGoto:             "goto",
	OLabel:            "label",
	OReturn:           "return",
	OBreak:            "break",
	OContinue:         "continue",
	ORange:            "range",
	OTypeAssert:       "typeassert",
	OTypeSwitch:       "typeswitch",
	OClosure:          "closure",
	ODefer:            "defer",
	OGo:               "go",
	OSend:             "send",
	ORecv:             "recv",
	OMake:             "make",
	OAppend:           "append",
	OCopy:             "copy",
	ODelete:           "delete",
	OPanic:            "panic",
	ORecover:          "recover",
	OLen:              "len",
	OCap:              "cap",
	ONew:              "new",
	OComplex:          "complex",
	OReal:             "real",
	OImag:             "imag",
	OClear:            "clear",
	OMin:              "min",
	OMax:              "max",
	OAssign:           "assign",
	OCase:             "case",
	OCompositeLit:     "compositelit",
	OSlice:            "slice",
	OClose:            "close",
	OPrint:            "print",
	OPrintln:          "println",
	OUnsafeAdd:        "unsafe.Add",
	OUnsafeSlice:      "unsafe.Slice",
	OUnsafeSliceData:  "unsafe.SliceData",
	OUnsafeString:     "unsafe.String",
	OUnsafeStringData: "unsafe.StringData",
	ODeclare:          "declare",
	ONilCheck:         "nilcheck",
}

func (o Op) String() string {
	if int(o) < len(opNames) && opNames[o] != "" {
		return opNames[o]
	}
	return "op(?)"
}

// goSpecificOps is the set that must be empty by the end of SSA construction.
//
// It is a set rather than a range check so that adding an Op in the middle of
// the block cannot silently change what is enforced.
var goSpecificOps = map[Op]bool{
	ORange: true, OTypeAssert: true, OTypeSwitch: true, OClosure: true,
	ODefer: true, OGo: true, OSend: true, ORecv: true, OMake: true,
	OAppend: true, OCopy: true, ODelete: true, OPanic: true, ORecover: true,
	OLen: true, OCap: true, ONew: true, OComplex: true, OReal: true,
	OImag: true, OClear: true, OMin: true, OMax: true, OSelect: true,
	OCompositeLit: true, OSlice: true,
	OClose: true, OPrint: true, OPrintln: true,
	OUnsafeAdd: true, OUnsafeSlice: true, OUnsafeSliceData: true,
	OUnsafeString: true, OUnsafeStringData: true,
}

// IsGoSpecific reports whether o is a construct that must be lowered away.
//
// specs/002-architecture.md claims no Go construct survives into SSA, and that
// claim is only meaningful if it is checked. The SSA builder asserts this over
// every node it consumes, and the assertion is on in test builds always.
func (o Op) IsGoSpecific() bool { return goSpecificOps[o] }

// Object is a resolved declaration.
//
// The IR refers to a declaration by pointer. Two uses of one variable hold one
// Object, which is what makes shadowing a front-end concern rather than a
// pervasive one.
type Object struct {
	Name  string
	Type  *Type
	Pos   syntax.Pos
	Class Class

	// Addrtaken records that the address of this object is taken somewhere.
	//
	// An address-taken local cannot live in an SSA value, because two names
	// would refer to one location. It is allocated in the frame instead, and it
	// becomes a stack object that specs/027-liveness-and-stackmaps.md must
	// describe to the collector. Being conservative here is safe and expensive;
	// being wrong is memory corruption.
	Addrtaken bool

	// Escapes records that this variable's storage may outlive the frame that
	// declares it, so it lives in a heap cell and both halves reach it through
	// a pointer.
	//
	// specs/023-escape-analysis.md is what will decide it. Until that pass
	// exists the sound rule it states is taken instead: the *source* takes the
	// variable's address, or a literal captures it. ir.Build sets it and
	// nothing after ir.Build does, which is what keeps the lowering pass from
	// reading a mark it made itself: ir/lower.go takes addresses of its own
	// temporaries, and a temporary whose address the compiler took does not
	// outlive anything.
	Escapes bool
}

// Class is what kind of declaration an Object is.
type Class uint8

const (
	ClassInvalid Class = iota
	ClassParam
	ClassResult
	ClassLocal
	ClassGlobal
	ClassFunc
	ClassConst
	ClassType
)

var classNames = [...]string{
	ClassInvalid: "invalid",
	ClassParam:   "param",
	ClassResult:  "result",
	ClassLocal:   "local",
	ClassGlobal:  "global",
	ClassFunc:    "func",
	ClassConst:   "const",
	ClassType:    "type",
}

func (c Class) String() string {
	if int(c) < len(classNames) && classNames[c] != "" {
		return classNames[c]
	}
	return "class(?)"
}

// Node is one node of the typed tree.
//
// One struct rather than an interface hierarchy. The tree is walked and
// rewritten far more than it is type-switched over, and a single struct makes
// a rewrite a field assignment rather than a reallocation. The reference
// implementation reached the same conclusion for the same reason.
type Node struct {
	Op   Op
	Pos  syntax.Pos
	Type *Type

	// Operands. Which fields are meaningful depends on Op, and the table in
	// specs/020-ir.md is the specification of that.
	X, Y Expr
	Args []Expr

	// Obj is set for OLocal, OGlobal and the declaration forms.
	Obj *Object

	// Op1 is the operator for OUnary, OBinary and OCompare.
	Op1 syntax.Operator

	// Index is the field index for OField and the case index elsewhere.
	Index int

	// Val is the value of an OConst.
	Val Value

	// Body, Else, Init and Post carry statements for the control forms.
	//
	// Post is the post statement of an OFor. It was added after the fact: the
	// node set had no place for it, and the first consumer put it in Else,
	// which is the field an OIf uses for something else entirely.
	Init, Body, Else, Post []Stmt

	// Label is the target name for OGoto, OLabel, OBreak and OContinue.
	Label string
}

// Expr and Stmt are both *Node. They are distinct names rather than distinct
// types because Go's grammar makes an expression a statement in several
// places, and a conversion between two types at each of those points would be
// noise with no checking value.
type (
	Expr = *Node
	Stmt = *Node
)

// Value is the value of a constant.
//
// Constant arithmetic is arbitrary precision and is the type checker's, so a
// Value here is already reduced to its type. The interface is deliberately
// narrow: nothing in the IR computes with constants, it only carries them.
type Value interface {
	// String returns the value in Go syntax.
	String() string
}

// ConstValue is a Value a consumer can read numerically.
//
// String alone is not enough, and this was found rather than foreseen: the SSA
// builder had to parse the printed Go syntax back into an integer, and the
// constant folding of specs/022-optimization-passes.md cannot fold what it
// cannot read. Printing and reparsing a float is also lossy, which would make
// a folded constant differ from an unfolded one.
//
// It is a second interface rather than a wider Value so that carrying a
// constant stays cheap for anything that only prints it. A consumer that needs
// a number type-asserts, and the assertion failing is a real answer: the
// constant is not one this consumer can use.
type ConstValue interface {
	Value

	// Int64 returns the value as an int64 and whether it fits exactly.
	Int64() (int64, bool)

	// Uint64 returns the value as a uint64 and whether it fits exactly.
	Uint64() (uint64, bool)

	// Float64 returns the value as a float64 and whether it is exact. A
	// value that is not exact is still the nearest float64, which is what a
	// consumer that has to put the constant in a register wants: 1.0/3.0 is
	// exact in no binary float and is still one number and not another.
	Float64() (float64, bool)

	// StringVal returns the value as a string and whether it is one. It is
	// here for the reason the three above are: go/constant's String quotes
	// the value and truncates one longer than 72 runes, so a consumer that
	// read a string constant through it would compile a different string.
	StringVal() (string, bool)

	// IsZero reports whether the value is the zero of its type. This is the
	// question asked most often and the one most easily got wrong through a
	// conversion, so it is answered directly.
	IsZero() bool
}

// Func is one function to be compiled.
type Func struct {
	Name    string
	Sym     string // the linker symbol name
	Type    *Type
	Pos     syntax.Pos
	Recv    *Object
	Params  []*Object
	Results []*Object
	Locals  []*Object
	Body    []Stmt

	// Pragma carries the //go: directives of
	// specs/016-directives-and-pragmas.md. Several of them are correctness
	// requirements rather than hints, so this field is read by the backend and
	// not only by diagnostics.
	Pragma syntax.Pragma

	// Inlinable is set when specs/024-inlining-and-devirtualization.md admits
	// the body. The decision is made when the function is compiled, not when it
	// is imported, so the exporter decides and the importer obeys.
	Inlinable bool

	// Closure is the context parameter of a function literal that captures.
	//
	// specs/033-closures-defer-panic.md gives a closure one hidden parameter:
	// the address of the closure object, which arrives in the context
	// register (R26 on arm64, specs/030-abi.md) rather than in an argument
	// register. The object is a code pointer followed by one word per
	// capture, so a capture is a field read off this object.
	//
	// It is nil for every function that captures nothing, and a function with
	// no capture must keep it nil: the register is read at the entry only
	// where something reads it, and the stack-growth tail of
	// specs/035-goroutines-and-stack-growth.md chooses its runtime symbol by
	// the same field.
	Closure *Object

	// Captures lists the objects of the enclosing function that this function
	// reads through Closure, in the order of the fields after the code
	// pointer. It is the same list, in the same order, as the Args of the
	// OClosure node that names this function.
	Captures []*Object

	// Wrapper records that this function is compiler-generated code the
	// runtime must not count as a frame of the program.
	//
	// runtime.gorecover decides whether recover was called directly from a
	// deferred function by counting the frames between itself and
	// runtime.gopanic, and it skips every frame whose funcID is
	// FuncIDWrapper. The literal ir.Build wraps the operands of a defer in is
	// exactly such a frame: without the mark, "defer f(x)" where f recovers
	// stops recovering, and nothing says so at compile time.
	Wrapper bool

	// Bodyless records that the declaration had no body block at all.
	//
	// An empty body and an absent body both leave Body empty, and the two mean
	// opposite things. "func f() {}" is a complete Go function that does
	// nothing and must be compiled. "func f()" with no block is satisfied
	// elsewhere, by assembly or by a //go:linkname, and there is nothing to
	// compile. A consumer that read only len(Body) refused the first for the
	// reason that belongs to the second, which is a legal program rejected.
	//
	// The sense is negative so that a Func built by hand, which always has a
	// body, needs no field set.
	Bodyless bool

	// ReflectMethod records that the body asks reflect for a method by a name
	// no relocation carries.
	//
	// A method reached that way is reached by no call, so cmd/link cannot
	// decide it is live: it resolves the entry to the sentinel -1 and the
	// runtime installs runtime.unreachableMethod in its place, which is a
	// program that links and then dies. obj.SymFlagReflectMethod on this
	// function is what makes the linker stop deciding and keep every method
	// of every type used in an interface.
	//
	// The question is asked here, over the selection, and not further down
	// over the symbols a function references. gc asks it in walk.usemethod
	// for the same reason: reflect.Type.Method is an interface method and an
	// interface call names no symbol at all, so a rule that reads the
	// reference list cannot see it. specs/032-type-descriptors-and-itabs.md
	// records the one case gc has that this does not.
	ReflectMethod bool

	// Dupok records that another object may define this symbol too, so the
	// linker merges the definitions instead of reporting a duplicate.
	//
	// It is set on an instantiation and on every function literal inside one.
	// An instantiation belongs to no package: List[int] is compiled by every
	// package that names it, because the package that declares List has no
	// way to know which arguments some importer will pick. gc sets the same
	// bit from the same rule, in noder/reader.go, where a declaration with
	// type parameters gets SetDupok(true) and a literal inherits it from the
	// function around it.
	//
	// Without it two packages that name one instantiation give cmd/link two
	// definitions of one symbol, and the build stops on a duplicate that
	// neither source file contains.
	Dupok bool
}

// Package is one compiled package.
type Package struct {
	Path    string
	Name    string
	Funcs   []*Func
	Globals []*Object
	Inits   []*Func

	// ForeignInsts holds every instantiation of a generic type another
	// package declares that this compilation met, with what became of each
	// method of it.
	//
	// The descriptor of an instantiation names its whole method set, and the
	// package that emits the descriptor owes every method in it
	// (specs/017-export-data-reading.md). Whether the descriptor is emitted
	// is decided after this pass, by the lowering and the closure the driver
	// takes over it, so this list is the evidence that decision is made
	// against rather than a refusal made here.
	ForeignInsts []ForeignInstantiation
}

// A ForeignInstantiation is one instantiation of a generic type another
// package declares.
type ForeignInstantiation struct {
	// Type is the instantiation itself, which is the type a descriptor of it
	// describes. Origin is the import path of the package that declares the
	// generic, and Decl is how that package spells its name.
	Type   *Type
	Origin string
	Decl   string

	// Methods is the whole method set of the instantiation, in the order the
	// checker holds it, and not the part of it this package calls.
	Methods []ForeignMethod
}

// A ForeignMethod is one method of a [ForeignInstantiation].
type ForeignMethod struct {
	// Name is the method's name and Sym is the linker symbol of the function
	// the front end compiles for it, which is the receiver form the method is
	// declared with. The other form is a wrapper, and whoever writes the
	// descriptor generates it (ssagen.MethodWrappers).
	Name string
	Sym  string

	// PtrOnly reports whether the method is in the method set of the pointer
	// and not in the method set of the type itself, exactly as [Method]
	// records it. The descriptor of T names the methods it is false for and
	// the descriptor of *T names all of them.
	PtrOnly bool

	// Reason is empty when the body was built, and otherwise says why it was
	// not. A method with a reason is a symbol nothing defines, so a
	// descriptor that names it cannot be emitted.
	Reason string
}

// Walk calls f for n and every node below it, in source order.
//
// f returns false to skip a node's children. A nil node is not visited, which
// is what lets the caller omit a nil check at every use.
func Walk(n *Node, f func(*Node) bool) {
	if n == nil || !f(n) {
		return
	}
	Walk(n.X, f)
	Walk(n.Y, f)
	for _, a := range n.Args {
		Walk(a, f)
	}
	for _, list := range [][]Stmt{n.Init, n.Body, n.Post, n.Else} {
		for _, s := range list {
			Walk(s, f)
		}
	}
}

// HasGoSpecific reports whether any node below n is a construct that must be
// lowered away, and returns the first one found.
//
// This is the invariant check specs/020-ir.md requires. It runs after SSA
// construction, where the answer must be no.
func HasGoSpecific(n *Node) (Op, bool) {
	found := OpInvalid
	Walk(n, func(m *Node) bool {
		if found != OpInvalid {
			return false
		}
		if m.Op.IsGoSpecific() {
			found = m.Op
			return false
		}
		return true
	})
	return found, found != OpInvalid
}
