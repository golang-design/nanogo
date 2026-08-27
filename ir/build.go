// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"go/constant"
	"sort"

	"golang.design/x/nanogo/syntax"
	"golang.design/x/nanogo/types2"
)

// The builder: the type-checked syntax tree of specs/011-parser-and-ast.md
// becomes the typed tree of specs/020-ir.md.
//
// One walk over the checked tree, consulting the checker's recorded types and
// definitions. specs/020-ir.md names the three differences from the syntax
// tree, and they are what the code below spends its length on:
//
//  1. Implicit operations are explicit. Every implicit conversion the
//     specification permits is an OConvert, and every implicit address is an
//     OAddr. After this pass, the type on a node is the type of the value that
//     node produces, with no assignability rule left to apply.
//  2. Names are resolved to objects. One *Object per declaration, shared by
//     every use. Shadowing is gone, because two shadowing declarations are two
//     objects.
//  3. Order of evaluation is explicit. The specification pins the order of
//     function calls, communication operations and index expressions within a
//     statement, and this is the last pass that can pin it: SSA construction is
//     free to reorder whatever the data dependencies allow. A call in an
//     operand position becomes a temporary assigned in a statement of its own,
//     in lexical order.
//
// # Conventions the node set does not name
//
// specs/020-ir.md's node set was missing several constructs of the language
// when this pass was written: an assignment, a switch clause, a composite
// literal, a slice expression, close, print, println, and the unsafe
// intrinsics. All of them are ops now, and no construct is encoded as
// something it is not. What is left below is how the operands of the ops are
// arranged, in one place so that specs/021-ssa-construction.md reads one
// comment rather than the whole file:
//
//   - An assignment is an OAssign: X is the destination and Y is the source.
//     Op1 is syntax.Def when the assignment declares its destinations, which
//     is x := y and var x = y. Because x += y and x++ are rewritten to
//     x = x + y here, an OBinary is always an operation and never an
//     assignment.
//   - A multi-value assignment sets X to nil and lists its destinations in
//     Args. Y is then a call, a receive, a type assertion or a map index that
//     produces more than one value.
//   - An element of a map literal, and of an array or slice literal written
//     with an index, is a key/value pair carried as an OAssign whose X is the
//     key. A struct literal's Args are positional and complete: a keyed
//     literal is normalised to one Arg per field, and a field the literal left
//     out gets its zero value written out. An OCompositeLit with no Args is
//     the zero value of its type.
//   - An index expression is an OIndex with X and Y.
//   - A switch clause and a select clause are OCase nodes in the switch's
//     Body. A clause carries its case expressions in Args, its statements in
//     Body, and its position in the switch in Index; a clause with no Args is
//     the default. A select clause carries its communication statement in
//     Init, and a type switch clause carries its variable in Obj.
//   - A type operand, in a type switch clause, is an OGlobal whose Obj has
//     class ClassType. A case nil in a type switch is an OConst with a nil
//     value.
//   - An OClosure names its function in Obj and holds its values in Args.
//     Index tells the two forms apart, because they differ in a way
//     specs/023-escape-analysis.md cares about: it is the method index for a
//     method value, whose one Arg is the receiver by value, and -1 for a
//     function literal, whose Args are captured objects shared with the
//     function that made it.
//   - fallthrough is an OGoto labelled "fallthrough". A source label cannot
//     collide with it, because fallthrough is a keyword.
//   - OFor keeps its init statements in Init, its condition in X, its body in
//     Body and its post statement in Post.
//   - A select clause's Init must be evaluated on entry to the select, for
//     every clause and in clause order, before any clause is chosen. That is
//     an obligation on specs/021-ssa-construction.md and not a fact this
//     encoding enforces: the specification evaluates the channel operands and
//     the sent values of every clause exactly once, in source order, and a
//     lowering that evaluated a clause's Init only when the clause is chosen
//     would produce the failure this pass exists to prevent.
//   - An OAssign whose Op1 is syntax.Def, in the body of an OFor or an
//     ORange, declares a new instance of its object every time it executes.
//     That is the whole of the Go 1.22 loop variable rule as this pass states
//     it: the builder puts the declaration inside the loop, and a consumer
//     that gave an address-taken object one allocation per frame rather than
//     one per declaration would put the pre-1.22 semantics back.
//   - An expression node's Init holds statements to evaluate immediately
//     before the node. It is used where there is no enclosing statement list
//     to put them in, which is a loop condition and the right operand of &&
//     and ||: both are evaluated somewhere other than where the statement
//     that holds them is.
//
// # What is not built
//
// A generic function or method is skipped. specs/013-generics.md instantiates
// before this pass runs, and a body with type parameters in it has no
// run-time representation to build. The skip is recorded as an error and the
// rest of the package is built.

// Assign returns the assignment of src to dst.
//
// Op1 is left zero, which is a plain assignment. defineOp marks the form that
// declares its destination.
func Assign(pos syntax.Pos, dst, src Expr) Stmt {
	return &Node{Op: OAssign, Pos: pos, Type: voidType, X: dst, Y: src}
}

// defineOp is the Op1 of an assignment that declares its destinations, which
// is x := y and var x = y. Both give the destination its first value, so a
// consumer that wants to skip the load of the old value can look at one field
// for both.
const defineOp = syntax.Def

// IsAssign reports whether n is an assignment.
func IsAssign(n *Node) bool { return n != nil && n.Op == OAssign }

// define returns an assignment that gives its destination its first value.
func define(pos syntax.Pos, dst, src Expr) Stmt {
	n := Assign(pos, dst, src)
	n.Op1 = defineOp
	return n
}

// spread marks a call whose final argument was written with ... .
const spread = 1

// closureLiteral is the Index of an OClosure that is a function literal. A
// method value carries the method's index there instead.
const closureLiteral = -1

// voidType is the type of a statement and of an expression with no value.
//
// One shared value. It is immutable after the layout below, and a type with no
// size and no pointers is the same type wherever it appears.
var voidType = mustLayoutKind(Void)

// funcType is the type of a function value: one word, holding a pointer to a
// closure object. A signature is not part of a machine type.
var funcType = mustLayoutKind(FuncKind)

func mustLayoutKind(k Kind) *Type {
	t := &Type{Kind: k}
	if err := Layout(t); err != nil {
		panic("ir: " + k.String() + " does not lay out: " + err.Error())
	}
	return t
}

// Const is the value of an OConst node.
//
// specs/020-ir.md keeps Value narrow on purpose: constant arithmetic is
// arbitrary precision and it is the type checker's, so the IR carries a
// constant and never computes with one.
type Const struct {
	// Val is the type checker's constant. A nil Val is the predeclared nil,
	// which is a value with no constant of its own.
	Val constant.Value
}

// String returns the constant in Go syntax.
func (c Const) String() string {
	if c.Val == nil {
		return "nil"
	}
	return c.Val.String()
}

// Int64 returns the value as an int64 and whether it fits exactly.
func (c Const) Int64() (int64, bool) {
	if c.Val == nil {
		return 0, false
	}
	v := constant.ToInt(c.Val)
	if v.Kind() != constant.Int {
		return 0, false
	}
	return constant.Int64Val(v)
}

// Uint64 returns the value as a uint64 and whether it fits exactly.
func (c Const) Uint64() (uint64, bool) {
	if c.Val == nil {
		return 0, false
	}
	v := constant.ToInt(c.Val)
	if v.Kind() != constant.Int || constant.Sign(v) < 0 {
		return 0, false
	}
	return constant.Uint64Val(v)
}

// Float64 returns the value as a float64 and whether it is exact.
//
// The checker's constants are arbitrary precision, so a value that does not
// round-trip through a float64 says so rather than rounding quietly. A folded
// constant that differs from an unfolded one is a miscompile nobody looks for.
func (c Const) Float64() (float64, bool) {
	if c.Val == nil {
		return 0, false
	}
	v := constant.ToFloat(c.Val)
	if v.Kind() != constant.Float && v.Kind() != constant.Int {
		return 0, false
	}
	return constant.Float64Val(v)
}

// StringVal returns the value as a string and whether it is one.
//
// A string constant is read here and never through String. go/constant's
// String is a display form: it quotes the value and truncates one longer than
// 72 runes with an ellipsis, so a consumer that read a string constant through
// it would compile a program holding a different string.
func (c Const) StringVal() (string, bool) {
	if c.Val == nil || c.Val.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(c.Val), true
}

// IsZero reports whether the value is the zero of its type.
//
// nil is the zero value of every type it can be written for, which is why the
// constant with no value answers yes.
func (c Const) IsZero() bool {
	if c.Val == nil {
		return true
	}
	switch c.Val.Kind() {
	case constant.Unknown:
		return false
	case constant.Bool:
		return !constant.BoolVal(c.Val)
	case constant.String:
		return constant.StringVal(c.Val) == ""
	}
	return constant.Sign(c.Val) == 0
}

// Build converts a type-checked package into IR.
//
// files must be the files that were checked, in the order they were given to
// the checker, and info must be the Info those files filled. The order of the
// output follows the order of the input: declarations in file order, and
// nothing is derived from ranging over a map, which specs/053-determinism.md
// requires.
//
// A declaration that cannot be built is skipped and named in the returned
// error. The package is returned either way, because a partial IR is what a
// diagnostic for the skipped declaration is written against.
func Build(pkg *types2.Package, files []*syntax.File, info *types2.Info) (*Package, error) {
	if pkg == nil || info == nil {
		return nil, fmt.Errorf("ir: Build needs a checked package and its Info")
	}
	b := &builder{
		conv:  NewConverter(),
		info:  info,
		tpkg:  pkg,
		objs:  make(map[types2.Object]*Object),
		owner: make(map[*Object]*Func),
		ptrs:  make(map[types2.Type]*Type),
		out: &Package{
			Path: pkg.Path(),
			Name: pkg.Name(),
		},
	}
	b.buildPackage(files)
	if len(b.errs) > 0 {
		return b.out, b.errs[0]
	}
	return b.out, nil
}

// builder holds the state of one package build.
type builder struct {
	conv *Converter
	info *types2.Info
	tpkg *types2.Package
	out  *Package

	// objs maps one checker object to one IR object. It is a lookup table and
	// it is never ranged over: every list this package produces is built in
	// declaration order instead (specs/053-determinism.md).
	objs  map[types2.Object]*Object
	owner map[*Object]*Func
	ptrs  map[types2.Type]*Type

	// fn is the function being built, and sinks is the stack of statement
	// lists it is being built into. An expression that needs a temporary
	// appends to the top of the stack, which is why expression building and
	// statement building are one pass rather than two.
	fn    *Func
	sig   *types2.Signature
	sinks [][]Stmt

	// noHoist counts the enclosing contexts in which a temporary must not be
	// introduced, because the expression is not evaluated exactly once here:
	// the right operand of && and ||, and a loop condition and post statement.
	noHoist int

	// free collects the objects the function being built refers to and does
	// not own. It is the capture set of a closure.
	free map[*Object]bool

	any types2.Type

	ntmp  int
	nfunc int
	errs  []error
}

func (b *builder) errorf(format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf(format, args...))
}

// irType converts a checker type, and never returns nil.
//
// A failure is recorded and the void type is returned, so that one type the
// builder cannot represent does not produce a tree with a hole in it that
// every later pass has to check for.
func (b *builder) irType(t types2.Type) *Type {
	if t == nil {
		return voidType
	}
	if basic, ok := unalias(t).(*types2.Basic); ok && basic.Kind() == types2.UntypedNil {
		// An untyped nil that reaches here has no destination to take its type
		// from. It is one word and it holds no pointer worth scanning.
		return b.irType(types2.Typ[types2.UnsafePointer])
	}
	out, err := b.conv.Convert(t)
	if err != nil {
		b.errs = append(b.errs, err)
		return voidType
	}
	return out
}

// nodeType is the type of the value an expression produces.
//
// The comma-ok forms of a map index, a receive and a type assertion produce
// two values, and the checker records their type as a tuple.
func (b *builder) nodeType(x syntax.Expr) *Type {
	t := b.typeOf(x)
	if tup, ok := t.(*types2.Tuple); ok {
		out, err := b.conv.Tuple(tup)
		if err != nil {
			b.errs = append(b.errs, err)
			return voidType
		}
		return out
	}
	return b.irType(t)
}

// ptrTo returns the IR type of a pointer to t.
func (b *builder) ptrTo(t types2.Type) *Type {
	if t == nil {
		return b.irType(types2.Typ[types2.UnsafePointer])
	}
	if have, ok := b.ptrs[t]; ok {
		return have
	}
	out := b.irType(types2.NewPointer(t))
	b.ptrs[t] = out
	return out
}

// typeOf returns the checker's type for an expression.
func (b *builder) typeOf(x syntax.Expr) types2.Type {
	if x == nil {
		return nil
	}
	if tv, ok := b.info.Types[x]; ok && tv.Type != nil {
		return tv.Type
	}
	if n, ok := syntax.Unparen(x).(*syntax.Name); ok {
		if o := b.info.Uses[n]; o != nil {
			return o.Type()
		}
		if o := b.info.Defs[n]; o != nil {
			return o.Type()
		}
	}
	return nil
}

// obj returns the IR object of a checker object, creating it once.
//
// The one object per declaration is what makes shadowing a front-end concern:
// two declarations of one name are two checker objects and so two IR objects.
func (b *builder) obj(o types2.Object) *Object {
	if o == nil {
		return nil
	}
	if have, ok := b.objs[o]; ok {
		b.noteUse(have)
		return have
	}
	out := &Object{
		Name: o.Name(),
		Pos:  o.Pos(),
		Type: b.irType(o.Type()),
	}
	switch o := o.(type) {
	case *types2.Var:
		switch o.Kind() {
		case types2.PackageVar:
			out.Class = ClassGlobal
			out.Name = varSym(o)
		case types2.ParamVar, types2.RecvVar:
			out.Class = ClassParam
		case types2.ResultVar:
			out.Class = ClassResult
		default:
			out.Class = ClassLocal
		}
	case *types2.Func:
		out.Class = ClassFunc
		out.Name = funcSym(o)
	case *types2.Const:
		out.Class = ClassConst
	case *types2.TypeName:
		out.Class = ClassType
	case *types2.Nil:
		out.Class = ClassConst
	default:
		out.Class = ClassLocal
	}
	b.objs[o] = out
	// A local of the function being built gets a frame slot. The blank
	// identifier does not: it names no storage, and an assignment to it is
	// kept only for the effects of its right-hand side.
	if out.Class == ClassLocal && b.fn != nil && out.Name != "_" {
		b.fn.Locals = append(b.fn.Locals, out)
		b.owner[out] = b.fn
	}
	return out
}

// noteUse records that the function being built refers to an object it may not
// own, which is what makes the object a capture of a closure.
func (b *builder) noteUse(o *Object) {
	if b.free == nil || o == nil || b.fn == nil {
		return
	}
	switch o.Class {
	case ClassLocal, ClassParam, ClassResult:
	default:
		return
	}
	if b.owner[o] == b.fn {
		return
	}
	b.free[o] = true
}

// varSym is the linker symbol of a package-level variable.
//
// The package is part of the symbol and not part of the name the source
// wrote, exactly as it is for a function: a package-level variable is one
// symbol in the program, and every package that reads it names the same one.
// An object that carried the bare name left the package to be put back
// downstream, where the only path available is the package being compiled, so
// a read of os.Stdout emitted a relocation against main.Stdout, which nothing
// defines. The linker reported an undefined symbol with no source position on
// it.
//
// A variable of the package being compiled goes through the same rule and
// comes out with the same symbol gc gives it.
func varSym(v *types2.Var) string {
	name := v.Name()
	if p := v.Pkg(); p != nil && p.Path() != "" {
		return p.Path() + "." + name
	}
	return name
}

// funcSym is the linker symbol of a function or method.
//
// specs/040-object-format.md owns the final spelling. What matters here is
// that it is unique and that it is derived from the declaration rather than
// from a counter, so that two builds of one package agree.
func funcSym(fn *types2.Func) string {
	name := fn.Name()
	pkgPath := ""
	if p := fn.Pkg(); p != nil {
		pkgPath = p.Path()
	}
	sig, _ := fn.Type().(*types2.Signature)
	if sig != nil && sig.Recv() != nil {
		recv := sig.Recv().Type()
		star := ""
		if p, ok := unalias(recv).(*types2.Pointer); ok {
			recv, star = p.Elem(), "*"
		}
		rname := "?"
		if named, ok := unalias(recv).(*types2.Named); ok {
			rname = named.Obj().Name()
			if p := named.Obj().Pkg(); p != nil {
				pkgPath = p.Path()
			}
		}
		if star != "" {
			return pkgPath + ".(*" + rname + ")." + name
		}
		return pkgPath + "." + rname + "." + name
	}
	if pkgPath == "" {
		return name
	}
	return pkgPath + "." + name
}

// The statement sink.
//
// Every expression case may append a statement, for a temporary or for the
// elements of a composite literal, so the sink is on the builder rather than
// threaded through every signature.

func (b *builder) push()          { b.sinks = append(b.sinks, nil) }
func (b *builder) emit(s ...Stmt) { b.sinks[len(b.sinks)-1] = append(b.sinks[len(b.sinks)-1], s...) }

func (b *builder) pop() []Stmt {
	out := b.sinks[len(b.sinks)-1]
	b.sinks = b.sinks[:len(b.sinks)-1]
	return out
}

// block builds a statement list into a list of its own.
func (b *builder) block(list []syntax.Stmt) []Stmt {
	b.push()
	for _, s := range list {
		b.stmt(s)
	}
	return b.pop()
}

// temp introduces a temporary holding n and returns a reference to it.
//
// This is where the order of evaluation stops being implicit. The assignment
// is emitted into the enclosing statement list at the point the operand was
// reached, so the lexical order of the operands becomes the order of the
// statements.
func (b *builder) temp(n Expr) Expr {
	o := &Object{
		Name:  fmt.Sprintf(".autotmp_%d", b.ntmp),
		Type:  n.Type,
		Pos:   n.Pos,
		Class: ClassLocal,
	}
	b.ntmp++
	if b.fn != nil {
		b.fn.Locals = append(b.fn.Locals, o)
		b.owner[o] = b.fn
	}
	ref := &Node{Op: OLocal, Pos: n.Pos, Type: n.Type, Obj: o}
	b.emit(define(n.Pos, ref, n))
	return &Node{Op: OLocal, Pos: n.Pos, Type: n.Type, Obj: o}
}

// ordered returns n, or a temporary holding n when n is an operation whose
// order the specification pins.
//
// The specification orders function calls, method calls and communication
// operations within a statement. A conversion of that rule into temporaries
// has to stop at a context that does not evaluate its operand exactly once, or
// it changes the program: hoisting the call out of a() && b() calls b
// unconditionally. noHoist counts those contexts.
func (b *builder) ordered(n Expr) Expr {
	if n == nil || b.noHoist > 0 {
		return n
	}
	switch n.Op {
	case OCall, ORecv:
	default:
		return n
	}
	if n.Type == nil || n.Type.Kind == Void {
		return n
	}
	return b.temp(n)
}

// operand builds a subexpression, in a position where the order of evaluation
// is observable.
func (b *builder) operand(x syntax.Expr) Expr { return b.ordered(b.expr(x)) }

// guarded builds an expression that is not evaluated unconditionally here, or
// that is evaluated more than once: the right operand of && and ||, and a loop
// condition.
//
// Any statement the expression needs is attached to the node's Init rather
// than emitted into the enclosing list, because the enclosing list runs once
// and unconditionally and the expression does not. An expression node's Init
// means exactly that: evaluate these statements immediately before this node.
func (b *builder) guarded(x syntax.Expr) Expr {
	b.noHoist++
	b.push()
	n := b.expr(x)
	pre := b.pop()
	b.noHoist--
	if n != nil && len(pre) > 0 {
		n.Init = append(pre, n.Init...)
	}
	return n
}

// addrOf returns the address of n and records that the address was taken.
func (b *builder) addrOf(n Expr, elem types2.Type) Expr {
	if n == nil {
		return nil
	}
	markAddrtaken(n)
	return &Node{Op: OAddr, Pos: n.Pos, Type: b.ptrTo(elem), X: n}
}

// markAddrtaken walks from an expression to the object whose storage it names.
//
// One helper rather than a flag set at each site that takes an address. Being
// conservative is safe and expensive and being wrong is memory corruption, so
// the walk stops and marks nothing only where the address provably belongs to
// something else: the element of a slice or a map lives in the heap, not in
// the frame slot of the variable that names it.
func markAddrtaken(n Expr) {
	for n != nil {
		switch n.Op {
		case OLocal, OGlobal:
			if n.Obj != nil {
				n.Obj.Addrtaken = true
			}
			return
		case OField:
			n = n.X
		case OIndex:
			// An array is stored in the object; a slice, a string and a map
			// only hold a pointer to their elements.
			if n.X != nil && n.X.Type != nil && n.X.Type.Kind == Array {
				n = n.X
				continue
			}
			return
		default:
			return
		}
	}
}

// cloneExpr copies the structure of an expression.
//
// The leaves are objects and constants, which are shared on purpose: two
// copies of a reference to one temporary are two reads of one location. It is
// used where one written operand is read twice, as in x += 1.
func cloneExpr(n Expr) Expr {
	if n == nil {
		return nil
	}
	out := *n
	out.X = cloneExpr(n.X)
	out.Y = cloneExpr(n.Y)
	out.Args = nil
	for _, a := range n.Args {
		out.Args = append(out.Args, cloneExpr(a))
	}
	return &out
}

// isUntyped reports whether t is the type of an untyped constant.
func isUntyped(t types2.Type) bool {
	if t == nil {
		return false
	}
	basic, ok := unalias(t).Underlying().(*types2.Basic)
	return ok && basic.Info()&types2.IsUntyped != 0
}

func isUntypedNil(t types2.Type) bool {
	if t == nil {
		return false
	}
	basic, ok := unalias(t).(*types2.Basic)
	return ok && basic.Kind() == types2.UntypedNil
}

// coreType is types2.CoreType with no type, which the checker cannot produce
// and a broken Info can, mapping to no core type rather than to a crash. A
// compiler that crashes on a bad input has replaced a diagnostic with a stack
// trace.
func coreType(t types2.Type) types2.Type {
	if t == nil {
		return nil
	}
	return types2.CoreType(t)
}

// unalias is types2.Unalias with the same guard.
func unalias(t types2.Type) types2.Type {
	if t == nil {
		return nil
	}
	return types2.Unalias(t)
}

// isInterface is types2.IsInterface with the same guard.
func isInterface(t types2.Type) bool {
	return t != nil && types2.IsInterface(t)
}

// assignConv makes an implicit conversion explicit.
//
// specs/020-ir.md's first difference from the syntax tree: an assignment of a
// concrete value to an interface variable is a conversion node, not an
// assignment with unequal types. Every context the specification calls
// assignment goes through here, which is assignment itself, an argument, a
// result, a channel send, a composite literal element and a case of an
// expression switch.
func (b *builder) assignConv(n Expr, from, to types2.Type) Expr {
	if n == nil || to == nil || from == nil {
		return n
	}
	if isUntypedNil(from) {
		// nil is not converted. It is the zero value of the destination, and
		// the destination is what gives it a shape.
		n.Type = b.irType(to)
		return n
	}
	if isUntyped(from) {
		// An untyped constant has no representation until a context gives it
		// one. Assigning it to an interface gives it its default type first,
		// and the conversion to the interface is then a node like any other.
		dst := to
		if isInterface(to) {
			dst = types2.Default(from)
		}
		n.Type = b.irType(dst)
		from = dst
	}
	if types2.Identical(from, to) {
		return n
	}
	return &Node{Op: OConvert, Pos: n.Pos, Type: b.irType(to), X: n}
}

// constNode returns a constant.
func (b *builder) constNode(pos syntax.Pos, v constant.Value, t types2.Type) Expr {
	if t != nil {
		t = types2.Default(t)
	}
	return &Node{Op: OConst, Pos: pos, Type: b.irType(t), Val: Const{Val: v}}
}

func (b *builder) intConst(pos syntax.Pos, v int64) Expr {
	return &Node{
		Op:   OConst,
		Pos:  pos,
		Type: b.irType(types2.Typ[types2.Int]),
		Val:  Const{Val: constant.MakeInt64(v)},
	}
}

// zeroValue returns the zero value of a type.
//
// A struct or an array is an OCompositeLit with no elements, which is the
// literal T{}. Everything else is a constant, and nil is the constant with no
// value.
func (b *builder) zeroValue(pos syntax.Pos, t *Type) Expr {
	switch t.Kind {
	case Struct, Array:
		return &Node{Op: OCompositeLit, Pos: pos, Type: t}
	case Bool:
		return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{Val: constant.MakeBool(false)}}
	case String:
		return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{Val: constant.MakeString("")}}
	}
	if t.Kind.IsInteger() {
		return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{Val: constant.MakeInt64(0)}}
	}
	if t.Kind.IsFloat() || t.Kind.IsComplex() {
		return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{Val: constant.MakeFloat64(0)}}
	}
	return &Node{Op: OConst, Pos: pos, Type: t, Val: Const{}}
}

// buildPackage walks the files in the order they were checked.
//
// Three passes, and the order of each is the order of the source. The globals
// come first so that Package.Globals is in declaration order whatever order
// the function bodies happen to refer to them in.
func (b *builder) buildPackage(files []*syntax.File) {
	for _, f := range files {
		for _, d := range f.DeclList {
			vd, ok := d.(*syntax.VarDecl)
			if !ok {
				continue
			}
			for _, name := range vd.NameList {
				o := b.info.Defs[name]
				if o == nil || name.Value == "_" {
					continue
				}
				b.out.Globals = append(b.out.Globals, b.obj(o))
			}
		}
	}

	var inits []*Func
	for _, f := range files {
		for _, d := range f.DeclList {
			fd, ok := d.(*syntax.FuncDecl)
			if !ok {
				continue
			}
			fn := b.buildFunc(fd, len(inits))
			if fn == nil {
				continue
			}
			b.out.Funcs = append(b.out.Funcs, fn)
			if fd.Recv == nil && fd.Name.Value == "init" {
				inits = append(inits, fn)
			}
		}
	}

	b.buildInit(inits)
}

// buildFunc builds one declared function or method.
//
// nth is the number of init functions already seen, which is what makes the
// symbol of the second init in a package different from the first.
func (b *builder) buildFunc(d *syntax.FuncDecl, nth int) *Func {
	obj, _ := b.info.Defs[d.Name].(*types2.Func)
	if obj == nil {
		b.errorf("ir: %s has no object", d.Name.Value)
		return nil
	}
	sig, _ := obj.Type().(*types2.Signature)
	if sig == nil {
		b.errorf("ir: %s is not a function", d.Name.Value)
		return nil
	}
	if sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0 {
		// specs/013-generics.md instantiates before this pass. A body with
		// type parameters in it has no run-time representation.
		b.errorf("ir: %s is generic and was not built", obj.Name())
		return nil
	}

	fn := &Func{
		Name:   d.Name.Value,
		Sym:    funcSym(obj),
		Type:   b.irType(sig),
		Pos:    d.Pos(),
		Pragma: d.Pragma,
	}
	if d.Recv == nil && d.Name.Value == "init" {
		// Two init functions in one package have one name and need two
		// symbols. The counter is the position in the file order, so it is the
		// same on every build.
		fn.Sym = fmt.Sprintf("%s.init.%d", b.out.Path, nth)
	}

	saveFn, saveSig, saveSinks, saveFree := b.fn, b.sig, b.sinks, b.free
	b.fn, b.sig, b.sinks, b.free = fn, sig, nil, nil

	if recv := sig.Recv(); recv != nil {
		fn.Recv = b.declare(recv, fn)
	}
	for i := 0; i < sig.Params().Len(); i++ {
		fn.Params = append(fn.Params, b.declare(sig.Params().At(i), fn))
	}
	for i := 0; i < sig.Results().Len(); i++ {
		fn.Results = append(fn.Results, b.declare(sig.Results().At(i), fn))
	}

	if d.Body != nil {
		b.free = make(map[*Object]bool)
		fn.Body = b.block(d.Body.List)
	} else {
		fn.Bodyless = true
	}
	b.fn, b.sig, b.sinks, b.free = saveFn, saveSig, saveSinks, saveFree
	return fn
}

// declare creates the object of a parameter or a result and records which
// function owns it, which is what tells a closure that a name is a capture.
func (b *builder) declare(v *types2.Var, fn *Func) *Object {
	o := b.obj(v)
	b.owner[o] = fn
	return o
}

// buildInit builds the package's initialisation function.
//
// specs/020-ir.md's lowering table: one init function, ordered by
// specs/012-type-checking.md. The order is the checker's InitOrder, which is a
// list and not a map, followed by the declared init functions in source order.
func (b *builder) buildInit(inits []*Func) {
	if len(b.info.InitOrder) == 0 && len(inits) == 0 {
		return
	}
	fn := &Func{
		Name: "init",
		Sym:  b.out.Path + ".init",
		Type: funcType,
	}
	saveFn, saveSig, saveSinks, saveFree := b.fn, b.sig, b.sinks, b.free
	b.fn, b.sig, b.sinks, b.free = fn, nil, nil, make(map[*Object]bool)

	b.push()
	for _, init := range b.info.InitOrder {
		b.initializer(init)
	}
	for _, sub := range inits {
		callee := &Node{
			Op:   OGlobal,
			Type: sub.Type,
			Pos:  sub.Pos,
			Obj:  &Object{Name: sub.Sym, Class: ClassFunc, Type: sub.Type, Pos: sub.Pos},
		}
		b.emit(&Node{Op: OCall, Pos: sub.Pos, Type: voidType, X: callee})
	}
	fn.Body = b.pop()

	b.fn, b.sig, b.sinks, b.free = saveFn, saveSig, saveSinks, saveFree
	b.out.Inits = append(b.out.Inits, fn)
}

// initializer builds one package-level variable initialisation.
func (b *builder) initializer(init *types2.Initializer) {
	rhsType := b.typeOf(init.Rhs)
	if len(init.Lhs) == 1 {
		dst := b.varNode(init.Lhs[0], init.Rhs.Pos())
		src := b.assignConv(b.expr(init.Rhs), rhsType, init.Lhs[0].Type())
		b.emit(Assign(init.Rhs.Pos(), dst, src))
		return
	}
	dsts := make([]Expr, 0, len(init.Lhs))
	for _, v := range init.Lhs {
		dsts = append(dsts, b.varNode(v, init.Rhs.Pos()))
	}
	b.emit(&Node{
		Op:   OAssign,
		Pos:  init.Rhs.Pos(),
		Type: voidType,
		Args: dsts,
		Y:    b.expr(init.Rhs),
	})
}

// varNode returns a reference to a declared variable.
func (b *builder) varNode(v *types2.Var, pos syntax.Pos) Expr {
	o := b.obj(v)
	op := OLocal
	if o.Class == ClassGlobal {
		op = OGlobal
	}
	return &Node{Op: op, Pos: pos, Type: o.Type, Obj: o}
}

// stmt builds one statement into the current statement list.
//
// A statement may produce more than one node, because the temporaries that fix
// the order of evaluation are statements of their own and are emitted before
// the statement that needed them.
func (b *builder) stmt(s syntax.Stmt) {
	switch s := s.(type) {
	case nil:
		return
	case *syntax.EmptyStmt:
		return
	case *syntax.BlockStmt:
		b.emit(&Node{Op: OBlock, Pos: s.Pos(), Type: voidType, Body: b.block(s.List)})
	case *syntax.ExprStmt:
		if n := b.expr(s.X); n != nil {
			b.emit(n)
		}
	case *syntax.SendStmt:
		b.sendStmt(s)
	case *syntax.AssignStmt:
		b.assignStmt(s)
	case *syntax.DeclStmt:
		for _, d := range s.DeclList {
			b.localDecl(d)
		}
	case *syntax.ReturnStmt:
		b.returnStmt(s)
	case *syntax.IfStmt:
		b.emit(b.ifStmt(s))
	case *syntax.ForStmt:
		b.forStmt(s)
	case *syntax.SwitchStmt:
		b.switchStmt(s)
	case *syntax.SelectStmt:
		b.selectStmt(s)
	case *syntax.LabeledStmt:
		b.emit(&Node{
			Op:    OLabel,
			Pos:   s.Pos(),
			Type:  voidType,
			Label: s.Label.Value,
			Body:  b.block([]syntax.Stmt{s.Stmt}),
		})
	case *syntax.BranchStmt:
		b.branchStmt(s)
	case *syntax.CallStmt:
		b.callStmt(s)
	default:
		b.errorf("ir: cannot build the statement %T", s)
	}
}

func (b *builder) sendStmt(s *syntax.SendStmt) {
	ch := b.operand(s.Chan)
	var elem types2.Type
	if c, ok := coreType(b.typeOf(s.Chan)).(*types2.Chan); ok {
		elem = c.Elem()
	}
	val := b.assignConv(b.operand(s.Value), b.typeOf(s.Value), elem)
	b.emit(&Node{Op: OSend, Pos: s.Pos(), Type: voidType, X: ch, Y: val})
}

func (b *builder) branchStmt(s *syntax.BranchStmt) {
	label := ""
	if s.Label != nil {
		label = s.Label.Value
	}
	n := &Node{Pos: s.Pos(), Type: voidType, Label: label}
	switch s.Tok {
	case syntax.Break:
		n.Op = OBreak
	case syntax.Continue:
		n.Op = OContinue
	case syntax.Goto:
		n.Op = OGoto
	case syntax.Fallthrough:
		// A source label cannot collide: fallthrough is a keyword.
		n.Op, n.Label = OGoto, "fallthrough"
	default:
		b.errorf("ir: unknown branch %s", s.Tok)
		return
	}
	b.emit(n)
}

// callStmt builds go and defer.
//
// The specification evaluates the function value and the arguments of go and
// defer when the statement runs, not when the call runs, so every operand
// becomes a temporary here. specs/033-closures-defer-panic.md reads them back
// out of the frame.
func (b *builder) callStmt(s *syntax.CallStmt) {
	call := b.expr(s.Call)
	if call == nil {
		return
	}
	// A name is not enough here. The variable a name refers to may be
	// assigned between the statement and the call, and the call has to use
	// the value the statement saw, so everything but a constant and a known
	// function symbol goes into a temporary.
	switch {
	case call.X == nil:
	case isInterfaceMethod(call.X):
		// An interface method call keeps the receiver inside the selection,
		// so the receiver is what is snapshotted.
		call.X.X = b.snapshot(call.X.X)
	case !isFuncSymbol(call.X):
		// Everything else, a field of function type and a package-level
		// variable of function type included. The value the call uses is what
		// the statement reads, so the whole selection is snapshotted:
		// snapshotting only the struct would read the field when the call
		// runs, and leaving a global alone would read the variable then.
		//
		// A declared function is the one case that needs no temporary,
		// because nothing can reassign it.
		call.X = b.snapshot(call.X)
	}
	for i, a := range call.Args {
		call.Args[i] = b.snapshot(a)
	}
	op := ODefer
	if s.Tok == syntax.Go {
		op = OGo
	}
	b.emit(&Node{Op: op, Pos: s.Pos(), Type: voidType, X: b.wrapCallStmt(s.Pos(), call)})
}

// isInterfaceMethod reports whether n is a method selected on an interface
// value, which is the one selection that keeps its receiver inside it.
func isInterfaceMethod(n Expr) bool {
	return n != nil && n.Op == OField && n.X != nil && n.X.Type != nil &&
		n.X.Type.Kind == Interface
}

// wrapCallStmt turns the call of a defer or a go statement into a call that
// takes nothing, by putting it in a function literal.
//
// runtime.deferproc and runtime.newproc take one word, a func value, and call
// it with no arguments. An operand therefore has to travel inside that value,
// which is what a capture is: the literal reads the operand off the closure
// object (specs/033-closures-defer-panic.md).
//
// The operands are already in temporaries, written where the statement is,
// because the specification evaluates them when the statement runs and not
// when the call runs. Each temporary is written once, so capturing it by
// reference and capturing it by value are the same value, and the one capture
// mechanism covers both: "defer end(n)" followed by an assignment to n still
// calls end with the value n held at the statement.
//
// A call with no operands needs no literal and gets none. Wrapping it would
// add a frame between the deferred call and runtime.gopanic, which is the one
// thing recover counts.
func (b *builder) wrapCallStmt(pos syntax.Pos, call Expr) Expr {
	if call == nil || call.Op != OCall || len(call.Args) == 0 {
		return call
	}
	fn := b.newLiteral(pos)
	fn.Body = []Stmt{call}
	// The runtime must not count this frame. runtime.gorecover recovers only
	// when exactly one non-wrapper frame stands between it and
	// runtime.gopanic, so "defer f(x)" where f recovers keeps recovering only
	// because this literal is marked.
	fn.Wrapper = true
	node := b.closureNode(fn, pos, b.freeIn(call))
	return &Node{Op: OCall, Pos: pos, Type: voidType, X: node}
}

// newLiteral returns an empty func() literal of the function being built.
func (b *builder) newLiteral(pos syntax.Pos) *Func {
	outerName, outerSym := "func", b.out.Path+".func"
	if b.fn != nil {
		outerName, outerSym = b.fn.Name, b.fn.Sym
	}
	fn := &Func{
		Name: fmt.Sprintf("%s.func%d", outerName, b.nfunc),
		Sym:  fmt.Sprintf("%s.func%d", outerSym, b.nfunc),
		Type: funcType,
		Pos:  pos,
	}
	b.nfunc++
	return fn
}

// closureNode builds the OClosure of a literal and its capture list, and adds
// the literal to the package.
//
// The node's Args and the function's Captures are one list in one order, which
// is what makes a capture's position in the closure object a fact both halves
// read.
func (b *builder) closureNode(fn *Func, pos syntax.Pos, caps []*Object) Expr {
	node := &Node{
		Op:    OClosure,
		Pos:   pos,
		Index: closureLiteral,
		Type:  fn.Type,
		Obj:   &Object{Name: fn.Sym, Class: ClassFunc, Type: fn.Type, Pos: pos},
	}
	for _, o := range caps {
		o.Addrtaken = true
		// A variable this literal captures and does not own is also captured
		// by the function that holds it, which is what makes a capture
		// through two levels of literal work.
		b.noteUse(o)
		node.Args = append(node.Args, &Node{Op: OLocal, Pos: o.Pos, Type: o.Type, Obj: o})
	}
	fn.Captures = caps
	fn.Closure = closureContext(caps, pos)
	b.out.Funcs = append(b.out.Funcs, fn)
	return node
}

// freeIn returns the local objects a subtree names, in the order the tree
// names them.
//
// The order is the tree's and never a map's (specs/053-determinism.md): it
// becomes the order of the words in a closure object.
func (b *builder) freeIn(n *Node) []*Object {
	var out []*Object
	seen := make(map[*Object]bool)
	Walk(n, func(m *Node) bool {
		if m.Op != OLocal || m.Obj == nil || seen[m.Obj] {
			return true
		}
		switch m.Obj.Class {
		case ClassLocal, ClassParam, ClassResult:
		default:
			return true
		}
		seen[m.Obj] = true
		out = append(out, m.Obj)
		return true
	})
	return out
}

func (b *builder) returnStmt(s *syntax.ReturnStmt) {
	n := &Node{Op: OReturn, Pos: s.Pos(), Type: voidType}
	results := syntax.UnpackListExpr(s.Results)
	if len(results) == 0 {
		b.emit(n)
		return
	}
	nres := 0
	if b.sig != nil {
		nres = b.sig.Results().Len()
	}
	if len(results) == 1 && nres > 1 {
		// return f(), where f returns everything this function returns. The
		// results are not separable here and the call keeps its tuple type.
		n.Args = []Expr{b.expr(results[0])}
		b.emit(n)
		return
	}
	for i, r := range results {
		var want types2.Type
		if b.sig != nil && i < nres {
			want = b.sig.Results().At(i).Type()
		}
		val := b.expr(r)
		if len(results) > 1 {
			val = b.ordered(val)
		}
		n.Args = append(n.Args, b.assignConv(val, b.typeOf(r), want))
	}
	b.emit(n)
}

func (b *builder) ifStmt(s *syntax.IfStmt) Stmt {
	n := &Node{Op: OIf, Pos: s.Pos(), Type: voidType}
	b.push()
	b.stmt(s.Init)
	n.X = b.expr(s.Cond)
	n.Init = b.pop()
	n.Body = b.block(s.Then.List)
	if s.Else != nil {
		n.Else = b.block([]syntax.Stmt{s.Else})
	}
	return n
}

func (b *builder) forStmt(s *syntax.ForStmt) {
	if rc, ok := s.Init.(*syntax.RangeClause); ok {
		b.rangeStmt(s, rc)
		return
	}
	n := &Node{Op: OFor, Pos: s.Pos(), Type: voidType}
	b.push()
	b.stmt(s.Init)
	n.Init = b.pop()
	if s.Cond != nil {
		// The condition is evaluated again on every iteration, so its
		// temporaries belong to it and not to the enclosing list.
		n.X = b.guarded(s.Cond)
	}
	n.Body = b.block(s.Body.List)
	if s.Post != nil {
		b.noHoist++
		n.Post = b.block([]syntax.Stmt{s.Post})
		b.noHoist--
	}
	// After the body, because whether the address of a loop variable is taken
	// is not known until the body that takes it has been built.
	b.perIteration(n, b.initDecls(s.Init))
	b.emit(n)
}

// perIteration gives each iteration of a loop its own copy of the variables
// the loop declares, which is what the language has required since Go 1.22.
//
// The rule is a fact about the declaration: "each iteration has its own
// separate declared variable", so it belongs where declarations become
// objects, and a consumer of the IR cannot invent the second object. The shape
// is the one the reference implementation uses. The loop keeps a carrier that
// the init statement, the condition and the post statement work on, and the
// body opens by declaring the variable again from the carrier:
//
//	for i := 0; i < n; i++ { body }   becomes   for i' := 0; i' < n; i'++ {
//	                                                i := i'
//	                                                body
//	                                                // post list: i' = i
//	                                            }
//
// The copy back is the first statement of the post list rather than the last
// statement of the body, because continue reaches the post list and does not
// reach the end of the body. Without that, an assignment the body made before
// a continue would be lost.
//
// It is done only for a variable whose address is taken, which includes every
// variable a closure captures. With no address, one instance and many are
// indistinguishable, and the copy would be work for a difference nobody can
// observe. This is the reference implementation's condition too.
func (b *builder) perIteration(loop *Node, decls []*Object) {
	for _, o := range decls {
		if o == nil || !o.Addrtaken {
			continue
		}
		// The name says what the object is and where it came from. Two
		// objects with one name would read as shadowing in a dump of the
		// tree, and this is not shadowing.
		carrier := &Object{Name: ".loopvar_" + o.Name, Type: o.Type, Pos: o.Pos, Class: ClassLocal}
		if b.fn != nil {
			b.fn.Locals = append(b.fn.Locals, carrier)
			b.owner[carrier] = b.fn
		}
		replaceObj(loop.X, o, carrier)
		for _, list := range [][]Stmt{loop.Init, loop.Post, loop.Args} {
			for _, n := range list {
				replaceObj(n, o, carrier)
			}
		}
		ref := func(of *Object) Expr {
			return &Node{Op: OLocal, Pos: of.Pos, Type: of.Type, Obj: of}
		}
		loop.Body = append([]Stmt{define(o.Pos, ref(o), ref(carrier))}, loop.Body...)
		if loop.Op == OFor {
			// A range statement needs no copy back: the next iteration takes
			// its value from the range expression and not from the variable.
			loop.Post = append([]Stmt{Assign(o.Pos, ref(carrier), ref(o))}, loop.Post...)
		}
	}
}

// replaceObj points every reference to one object at another.
//
// It does not descend into a closure, whose Args name the objects the closure
// captured while the body was built and which belong to that body.
func replaceObj(n *Node, from, to *Object) {
	Walk(n, func(m *Node) bool {
		if m.Op == OClosure {
			return false
		}
		if m.Obj == from && (m.Op == OLocal || m.Op == OGlobal) {
			m.Obj = to
		}
		return true
	})
}

// initDecls returns the objects a loop's init statement declares.
func (b *builder) initDecls(init syntax.SimpleStmt) []*Object {
	as, ok := init.(*syntax.AssignStmt)
	if !ok || as.Op != syntax.Def {
		return nil
	}
	var out []*Object
	for _, e := range syntax.UnpackListExpr(as.Lhs) {
		name, _ := syntax.Unparen(e).(*syntax.Name)
		if name == nil || name.Value == "_" {
			continue
		}
		if o := b.info.Defs[name]; o != nil {
			out = append(out, b.objs[o])
		}
	}
	return out
}

// rangeStmt builds a range statement.
//
// It stays a range statement. specs/021-ssa-construction.md turns it into the
// loop its row of the lowering table names, and what this pass owes it is the
// operands: the range expression evaluated once, and the destinations of the
// iteration variables. A variable the clause declares gets its own instance
// per iteration, which perIteration explains.
func (b *builder) rangeStmt(s *syntax.ForStmt, rc *syntax.RangeClause) {
	n := &Node{Op: ORange, Pos: s.Pos(), Type: voidType}
	b.push()
	// The checker records the range expression of an array as the constant
	// length where the specification does not evaluate it: at most one
	// iteration variable, and no call or receive in the operand. The operand
	// is then a count and there is nothing to address.
	x := b.expr(rc.X)
	xt := b.typeOf(rc.X)
	// A range over an addressable array runs over the array in place rather
	// than over a copy of it, so the address is taken and the object holding
	// the array cannot live in a register.
	if arr, ok := coreType(xt).(*types2.Array); ok && arr != nil && b.addressable(rc.X) {
		x = b.addrOf(x, xt)
	}
	n.X = x
	n.Init = b.pop()

	dsts := syntax.UnpackListExpr(rc.Lhs)
	var pre []Stmt
	for _, lhs := range dsts {
		if rc.Def {
			name, _ := syntax.Unparen(lhs).(*syntax.Name)
			if name == nil {
				b.errorf("ir: a range declaration is not a name")
				continue
			}
			n.Args = append(n.Args, b.name(name))
			continue
		}
		// The destination's own temporaries stay with the statement rather
		// than going into the list this statement is in. A range clause's
		// destination is written once per iteration, and the specification
		// evaluates an index expression's operands with the assignment that
		// reads it, so a temporary in front of the loop evaluates them once:
		// "for a[f()] = range [2]int{}" calls f twice and hoisting the index
		// calls it once. An expression's Init runs immediately before the
		// expression, which is inside the loop.
		b.push()
		dst := b.expr(lhs)
		if len(dsts) > 1 {
			// A clause with two destinations assigns them at once, which is a
			// parallel assignment: the operands of the index expressions and
			// the pointer indirections on the left are evaluated before the
			// first destination is written.
			dst = b.stabilizeParallel(dst)
		}
		pre = append(pre, b.pop()...)
		n.Args = append(n.Args, dst)
	}
	// Every destination's temporaries go in front of the first destination
	// that is written, and not in front of the destination they belong to.
	// specs/021-ssa-construction.md writes the index and then the element, so
	// a temporary held with the second one would read the first after the loop
	// wrote it, and "for i, x[i] = range y" would write x[0] rather than x[1].
	if len(pre) > 0 {
		for _, dst := range n.Args {
			if dst == nil {
				continue
			}
			dst.Init = append(pre, dst.Init...)
			break
		}
	}
	n.Body = b.block(s.Body.List)
	if rc.Def {
		var decls []*Object
		for _, a := range n.Args {
			if a != nil && a.Op == OLocal {
				decls = append(decls, a.Obj)
			}
		}
		b.perIteration(n, decls)
	}
	b.emit(n)
}

// addressable reports whether the checker recorded an expression as
// addressable, which is what decides whether an implicit address may be taken.
func (b *builder) addressable(x syntax.Expr) bool {
	tv, ok := b.info.Types[syntax.Unparen(x)]
	return ok && tv.Addressable()
}

// switchStmt builds an expression switch or a type switch.
func (b *builder) switchStmt(s *syntax.SwitchStmt) {
	if guard, ok := s.Tag.(*syntax.TypeSwitchGuard); ok {
		b.typeSwitchStmt(s, guard)
		return
	}
	n := &Node{Op: OSwitch, Pos: s.Pos(), Type: voidType}
	b.push()
	b.stmt(s.Init)
	tagType := types2.Type(types2.Typ[types2.Bool])
	if s.Tag != nil {
		n.X = b.operand(s.Tag)
		if t := b.typeOf(s.Tag); t != nil {
			tagType = types2.Default(t)
			n.X = b.assignConv(n.X, b.typeOf(s.Tag), tagType)
		}
	} else {
		// A switch with no tag is a switch on true. The comparison against
		// each case is then the same operation in both forms.
		n.X = &Node{
			Op:   OConst,
			Pos:  s.Pos(),
			Type: b.irType(types2.Typ[types2.Bool]),
			Val:  Const{Val: constant.MakeBool(true)},
		}
	}
	n.Init = b.pop()

	for i, clause := range s.Body {
		c := &Node{Op: OCase, Pos: clause.Pos(), Type: voidType, Index: i}
		for _, e := range syntax.UnpackListExpr(clause.Cases) {
			c.Args = append(c.Args, b.assignConv(b.expr(e), b.typeOf(e), tagType))
		}
		c.Body = b.block(clause.Body)
		n.Body = append(n.Body, c)
	}
	b.emit(n)
}

// typeSwitchStmt builds a type switch.
//
// Each clause carries its own variable, because the specification gives the
// variable of x := y.(type) a different type in each clause. The checker
// records one object per clause in Implicits, and the clause node holds it.
func (b *builder) typeSwitchStmt(s *syntax.SwitchStmt, guard *syntax.TypeSwitchGuard) {
	n := &Node{Op: OTypeSwitch, Pos: s.Pos(), Type: voidType}
	b.push()
	b.stmt(s.Init)
	n.X = b.operand(guard.X)
	n.Init = b.pop()

	for i, clause := range s.Body {
		c := &Node{Op: OCase, Pos: clause.Pos(), Type: voidType, Index: i}
		if o := b.info.Implicits[clause]; o != nil {
			c.Obj = b.obj(o)
		}
		for _, e := range syntax.UnpackListExpr(clause.Cases) {
			t := b.typeOf(e)
			if t == nil || isUntypedNil(t) {
				// case nil, which matches an interface holding nothing.
				c.Args = append(c.Args, &Node{
					Op:   OConst,
					Pos:  e.Pos(),
					Type: b.irType(b.typeOf(guard.X)),
					Val:  Const{},
				})
				continue
			}
			c.Args = append(c.Args, b.typeNode(e.Pos(), t))
		}
		c.Body = b.block(clause.Body)
		n.Body = append(n.Body, c)
	}
	b.emit(n)
}

// selectStmt builds a select.
//
// The clause's communication statement is in the clause's Init, and the
// specification's rule that the channel operands of every clause are evaluated
// once, in source order, on entry to the select is the reason it is a
// statement there rather than an expression somewhere.
func (b *builder) selectStmt(s *syntax.SelectStmt) {
	n := &Node{Op: OSelect, Pos: s.Pos(), Type: voidType}
	for i, clause := range s.Body {
		c := &Node{Op: OCase, Pos: clause.Pos(), Type: voidType, Index: i}
		b.push()
		b.stmt(clause.Comm)
		c.Init = b.pop()
		c.Body = b.block(clause.Body)
		n.Body = append(n.Body, c)
	}
	b.emit(n)
}

// localDecl builds a declaration inside a function body.
//
// A constant and a type declare nothing at run time: a constant use is already
// folded by the checker, and a type is not a value. A variable with no
// initialiser emits no statement either, because a frame is zeroed
// (specs/030-abi.md); what it does need is an entry in the function's locals,
// which the object creation does.
func (b *builder) localDecl(d syntax.Decl) {
	vd, ok := d.(*syntax.VarDecl)
	if !ok {
		return
	}
	if vd.Values == nil {
		for _, name := range vd.NameList {
			if o := b.info.Defs[name]; o != nil {
				b.obj(o)
			}
		}
		return
	}
	lhs := make([]syntax.Expr, len(vd.NameList))
	for i, name := range vd.NameList {
		lhs[i] = name
	}
	// A var declaration with an initialiser declares its destination, like
	// the short form does.
	b.assign(vd.Pos(), lhs, syntax.UnpackListExpr(vd.Values), true)
}

// assignStmt builds =, :=, an operation assignment, and ++ and --.
func (b *builder) assignStmt(s *syntax.AssignStmt) {
	switch {
	case s.Rhs == syntax.ImplicitOne:
		// x++ and x--. syntax.ImplicitOne is one shared node with no entry in
		// the checker's maps, so the constant is built here rather than looked
		// up.
		b.incDec(s)
	case s.Op != 0 && s.Op != syntax.Def:
		b.opAssign(s)
	default:
		b.assign(s.Pos(), syntax.UnpackListExpr(s.Lhs), syntax.UnpackListExpr(s.Rhs), s.Op == syntax.Def)
	}
}

func (b *builder) incDec(s *syntax.AssignStmt) {
	dst := b.stabilize(b.lvalue(s.Lhs))
	if dst == nil {
		return
	}
	one := &Node{Op: OConst, Pos: s.Pos(), Type: dst.Type, Val: Const{Val: constant.MakeInt64(1)}}
	sum := &Node{Op: OBinary, Op1: s.Op, Pos: s.Pos(), Type: dst.Type, X: cloneExpr(dst), Y: one}
	b.emit(Assign(s.Pos(), dst, sum))
}

// opAssign rewrites x op= y to x = x op y.
//
// The rewrite is what keeps an OBinary an operation and never an assignment.
// The left operand is evaluated once, which is why its parts are held in
// temporaries before it is used twice.
func (b *builder) opAssign(s *syntax.AssignStmt) {
	dst := b.stabilize(b.lvalue(s.Lhs))
	if dst == nil {
		return
	}
	lt := b.typeOf(s.Lhs)
	var rhs Expr
	if s.Op == syntax.Shl || s.Op == syntax.Shr {
		// The count of a shift keeps its own type. The specification does not
		// convert it to the type of the shifted operand.
		rhs = b.operand(s.Rhs)
	} else {
		rhs = b.assignConv(b.operand(s.Rhs), b.typeOf(s.Rhs), lt)
	}
	op := &Node{Op: OBinary, Op1: s.Op, Pos: s.Pos(), Type: dst.Type, X: cloneExpr(dst), Y: rhs}
	b.emit(Assign(s.Pos(), dst, op))
}

// assign builds an assignment or a short variable declaration.
//
// The specification's two phases are the shape of this function. First the
// operands of the index expressions and the pointer indirections on the left,
// and the expressions on the right, are evaluated in the usual order. Then the
// assignments are carried out left to right. A parallel assignment therefore
// reads every right-hand side into a temporary before it writes anything,
// which is what makes a, b = b, a a swap.
func (b *builder) assign(pos syntax.Pos, lhs, rhs []syntax.Expr, def bool) {
	assign := Assign
	if def {
		assign = define
	}
	switch {
	case len(lhs) == 1 && len(rhs) == 1:
		dst := b.lvalue(lhs[0])
		src := b.assignConv(b.expr(rhs[0]), b.typeOf(rhs[0]), b.typeOf(lhs[0]))
		b.emit(assign(pos, dst, src))

	case len(lhs) > 1 && len(rhs) == 1:
		b.multiAssign(pos, lhs, rhs[0], def)

	case len(lhs) == len(rhs):
		dsts := make([]Expr, len(lhs))
		for i := range lhs {
			dsts[i] = b.stabilizeParallel(b.lvalue(lhs[i]))
		}
		srcs := make([]Expr, len(rhs))
		for i := range rhs {
			v := b.assignConv(b.expr(rhs[i]), b.typeOf(rhs[i]), b.typeOf(lhs[i]))
			srcs[i] = b.snapshot(v)
		}
		for i := range dsts {
			b.emit(assign(pos, dsts[i], srcs[i]))
		}

	default:
		b.errorf("ir: %d destinations and %d values", len(lhs), len(rhs))
	}
}

// multiAssign builds an assignment whose one right-hand side produces several
// values: a call, and the comma-ok forms of a map index, a receive and a type
// assertion.
func (b *builder) multiAssign(pos syntax.Pos, lhs []syntax.Expr, rhs syntax.Expr, def bool) {
	op1 := syntax.Operator(0)
	if def {
		op1 = defineOp
	}
	dsts := make([]Expr, len(lhs))
	for i := range lhs {
		dsts[i] = b.stabilizeParallel(b.lvalue(lhs[i]))
	}
	src := b.expr(rhs)
	srcTypes := b.valueTypes(rhs, len(lhs))

	// A destination whose type differs from the value's needs a conversion,
	// and one node cannot hold a conversion per destination. The values go
	// into temporaries and the conversions are assignments of their own.
	needConv := false
	for i := range dsts {
		if i < len(srcTypes) && !b.sameType(srcTypes[i], b.typeOf(lhs[i])) {
			needConv = true
		}
	}
	if !needConv {
		b.emit(&Node{Op: OAssign, Op1: op1, Pos: pos, Type: voidType, Args: dsts, Y: src})
		return
	}
	tmps := make([]Expr, len(dsts))
	for i := range dsts {
		var t *Type
		if i < len(srcTypes) {
			t = b.irType(srcTypes[i])
		} else {
			t = dsts[i].Type
		}
		o := &Object{Name: fmt.Sprintf(".autotmp_%d", b.ntmp), Type: t, Pos: pos, Class: ClassLocal}
		b.ntmp++
		if b.fn != nil {
			b.fn.Locals = append(b.fn.Locals, o)
			b.owner[o] = b.fn
		}
		tmps[i] = &Node{Op: OLocal, Pos: pos, Type: t, Obj: o}
	}
	b.emit(&Node{Op: OAssign, Op1: defineOp, Pos: pos, Type: voidType, Args: tmps, Y: src})
	for i := range dsts {
		var from types2.Type
		if i < len(srcTypes) {
			from = srcTypes[i]
		}
		val := &Node{Op: OLocal, Pos: pos, Type: tmps[i].Type, Obj: tmps[i].Obj}
		n := Assign(pos, dsts[i], b.assignConv(val, from, b.typeOf(lhs[i])))
		n.Op1 = op1
		b.emit(n)
	}
}

// valueTypes returns the types of the values one expression produces.
func (b *builder) valueTypes(x syntax.Expr, n int) []types2.Type {
	t := b.typeOf(x)
	if tup, ok := t.(*types2.Tuple); ok {
		out := make([]types2.Type, tup.Len())
		for i := range out {
			out[i] = tup.At(i).Type()
		}
		return out
	}
	if n == 2 && t != nil {
		// The comma-ok forms: the value, and whether it was there.
		return []types2.Type{t, types2.Typ[types2.Bool]}
	}
	return []types2.Type{t}
}

func (b *builder) sameType(a, c types2.Type) bool {
	if a == nil || c == nil {
		return a == c
	}
	return types2.Identical(a, c)
}

// lvalue builds a destination.
func (b *builder) lvalue(x syntax.Expr) Expr {
	x = syntax.Unparen(x)
	if name, ok := x.(*syntax.Name); ok {
		return b.name(name)
	}
	return b.expr(x)
}

// stable returns n, or a temporary holding n, so that the value can be read
// twice without evaluating the expression twice.
func (b *builder) stable(n Expr) Expr {
	if n == nil {
		return nil
	}
	switch n.Op {
	case OConst, OLocal, OGlobal:
		return n
	}
	if n.Type == nil || n.Type.Kind == Void {
		return n
	}
	return b.temp(n)
}

// snapshot forces a temporary for anything that is not a constant.
//
// A parallel assignment needs the value as it was before any destination was
// written, so a name is not stable enough here: the name may be one of the
// destinations.
func (b *builder) snapshot(n Expr) Expr {
	if n == nil || n.Op == OConst {
		return n
	}
	if n.Type == nil || n.Type.Kind == Void {
		return n
	}
	return b.temp(n)
}

// stabilize holds the parts of a destination that are evaluated, so that the
// destination can be written and read in one statement without evaluating
// those parts twice.
//
// A name is held as it is. The statement writes one destination, so nothing
// between the two reads of the name can change it, which is what ++, -- and an
// operation assignment need.
func (b *builder) stabilize(n Expr) Expr { return b.stabilizeWith(n, b.stable) }

// stabilizeParallel is stabilize for a destination of a parallel assignment.
//
// The specification evaluates the operands of every index expression and every
// pointer indirection on the left before it writes any destination, so a name
// among those operands has to be read before the first write rather than at
// the write that reads it. stabilize keeps a name, so
//
//	i, a[i] = 0, 99
//
// wrote a[0] instead of a[1], and
//
//	p, *p = &n, 7
//
// wrote through the new pointer instead of the old one. Each was a silent
// wrong answer: the program ran and computed a value Go does not define.
// test/range.go of Go's own corpus is the file that reports it.
func (b *builder) stabilizeParallel(n Expr) Expr { return b.stabilizeWith(n, b.snapshot) }

// stabilizeWith is stabilize with the treatment of a name as a parameter.
//
// The recursion carries the treatment, because a field of an array of a struct
// is one destination and its innermost index is evaluated under the same rule
// as its outermost one.
func (b *builder) stabilizeWith(n Expr, keep func(Expr) Expr) Expr {
	if n == nil {
		return nil
	}
	switch n.Op {
	case OField:
		n.X = b.stabilizeWith(n.X, keep)
	case ODeref:
		n.X = keep(n.X)
	case OIndex:
		if n.X != nil && n.X.Type != nil && n.X.Type.Kind == Array {
			// An array is the storage the assignment writes and not a value it
			// reads, so the destination goes on naming it and the recursion
			// carries on into whatever addresses it.
			n.X = b.stabilizeWith(n.X, keep)
		} else {
			n.X = keep(n.X)
		}
		n.Y = keep(n.Y)
	}
	return n
}

// expr builds an expression.
//
// A constant is taken from the checker rather than recomputed. Constant
// arithmetic is arbitrary precision and it is the checker's; nothing here
// computes with a constant.
func (b *builder) expr(x syntax.Expr) Expr {
	x = syntax.Unparen(x)
	if x == nil {
		return nil
	}
	if tv, ok := b.info.Types[x]; ok && tv.Value != nil {
		return b.constNode(x.Pos(), tv.Value, tv.Type)
	}
	switch x := x.(type) {
	case *syntax.Name:
		return b.name(x)
	case *syntax.CompositeLit:
		return b.compositeLit(x)
	case *syntax.FuncLit:
		return b.closure(x)
	case *syntax.SelectorExpr:
		return b.selector(x)
	case *syntax.IndexExpr:
		return b.index(x)
	case *syntax.SliceExpr:
		return b.sliceExpr(x)
	case *syntax.AssertExpr:
		return &Node{
			Op:   OTypeAssert,
			Pos:  x.Pos(),
			Type: b.nodeType(x),
			X:    b.operand(x.X),
			Y:    b.typeNode(x.Type.Pos(), b.typeOf(x.Type)),
		}
	case *syntax.Operation:
		return b.operation(x)
	case *syntax.CallExpr:
		return b.call(x)
	case *syntax.KeyValueExpr:
		// Reached only through a composite literal, which unpacks the pair
		// itself. Anywhere else it is a syntax error the checker rejected.
		b.errorf("ir: a key/value pair outside a composite literal")
		return b.badExpr(x.Pos())
	}
	if tv, ok := b.info.Types[x]; ok && tv.IsType() {
		return b.typeNode(x.Pos(), tv.Type)
	}
	b.errorf("ir: cannot build the expression %T", x)
	return b.badExpr(x.Pos())
}

// badExpr stands in for an expression the builder could not make.
//
// It is a node rather than nil, so that one construct the builder does not
// know does not put a hole in the tree that every later pass has to test for.
func (b *builder) badExpr(pos syntax.Pos) Expr {
	return &Node{Op: OConst, Pos: pos, Type: voidType, Val: Const{}}
}

// name resolves an identifier to its object.
//
// specs/020-ir.md's second difference from the syntax tree. The object is the
// declaration, so two shadowing declarations of one name are two objects and
// the shadowing is gone.
func (b *builder) name(x *syntax.Name) Expr {
	if x.Value == "_" {
		// The blank identifier names no storage. It gets an object so that an
		// assignment to it keeps its shape, and the object is in no frame, so
		// nothing allocates a slot for it.
		o := &Object{Name: "_", Class: ClassLocal, Type: b.nodeType(x), Pos: x.Pos()}
		return &Node{Op: OLocal, Pos: x.Pos(), Type: o.Type, Obj: o}
	}
	obj := b.info.Uses[x]
	if obj == nil {
		obj = b.info.Defs[x]
	}
	switch o := obj.(type) {
	case nil:
		b.errorf("ir: %s resolves to nothing", x.Value)
		return b.badExpr(x.Pos())
	case *types2.Nil:
		return &Node{Op: OConst, Pos: x.Pos(), Type: b.nodeType(x), Val: Const{}}
	case *types2.Const:
		// Reached only when the checker recorded no value, which is an
		// invalid constant it already reported.
		return &Node{Op: OConst, Pos: x.Pos(), Type: b.irType(o.Type()), Val: Const{Val: o.Val()}}
	case *types2.TypeName:
		return b.typeNode(x.Pos(), o.Type())
	case *types2.Builtin:
		// A builtin is not a value. It reaches an expression position only as
		// the callee of a call, and b.call handles that before the callee is
		// built as an operand.
		b.errorf("ir: the builtin %s is not a value", o.Name())
		return b.badExpr(x.Pos())
	case *types2.Func:
		io := b.obj(o)
		return &Node{Op: OGlobal, Pos: x.Pos(), Type: io.Type, Obj: io}
	case *types2.Var:
		io := b.obj(o)
		op := OLocal
		if io.Class == ClassGlobal {
			op = OGlobal
		}
		return &Node{Op: op, Pos: x.Pos(), Type: io.Type, Obj: io}
	}
	b.errorf("ir: %s is a %T", x.Value, obj)
	return b.badExpr(x.Pos())
}

// typeNode returns the node that names a type.
//
// A type is not a value, and the only places one appears in the IR are the
// clauses of a type switch and the operand of a type assertion. It is an
// OGlobal whose object has class ClassType, so that the walkers do not need a
// node kind of their own for it.
func (b *builder) typeNode(pos syntax.Pos, t types2.Type) Expr {
	irt := b.irType(t)
	if named, ok := unalias(t).(*types2.Named); ok && named.Obj() != nil {
		o := b.obj(named.Obj())
		return &Node{Op: OGlobal, Pos: pos, Type: irt, Obj: o}
	}
	o := &Object{Name: irt.String(), Class: ClassType, Type: irt, Pos: pos}
	return &Node{Op: OGlobal, Pos: pos, Type: irt, Obj: o}
}

// selector builds x.f: a qualified identifier, a field, a method value or a
// method expression.
func (b *builder) selector(x *syntax.SelectorExpr) Expr {
	if name, ok := syntax.Unparen(x.X).(*syntax.Name); ok {
		if _, isPkg := b.info.Uses[name].(*types2.PkgName); isPkg {
			return b.name(x.Sel)
		}
	}
	sel := b.info.Selections[x]
	if sel == nil {
		b.errorf("ir: no selection for %s", x.Sel.Value)
		return b.badExpr(x.Pos())
	}
	switch sel.Kind() {
	case types2.FieldVal:
		base, _ := b.fieldPath(b.expr(x.X), b.typeOf(x.X), sel.Index())
		return base

	case types2.MethodVal:
		// A method value is a closure holding the receiver, which is the row
		// specs/020-ir.md's lowering table gives it.
		fn, _ := sel.Obj().(*types2.Func)
		if fn == nil {
			b.errorf("ir: the method %s has no object", x.Sel.Value)
			return b.badExpr(x.Pos())
		}
		recv, recvType := b.fieldPath(b.expr(x.X), b.typeOf(x.X), sel.Index()[:len(sel.Index())-1])
		recv = b.recvArg(recv, recvType, fn)
		io := b.obj(fn)
		return &Node{
			Op:    OClosure,
			Pos:   x.Pos(),
			Type:  b.irType(sel.Type()),
			Obj:   io,
			Args:  []Expr{recv},
			Index: b.methodIndex(sel),
		}

	case types2.MethodExpr:
		// A method expression is the method's function symbol. Its first
		// parameter is the receiver.
		fn, _ := sel.Obj().(*types2.Func)
		if fn == nil {
			b.errorf("ir: the method %s has no object", x.Sel.Value)
			return b.badExpr(x.Pos())
		}
		io := b.obj(fn)
		return &Node{Op: OGlobal, Pos: x.Pos(), Type: b.irType(sel.Type()), Obj: io}
	}
	b.errorf("ir: unknown selection kind %d", sel.Kind())
	return b.badExpr(x.Pos())
}

// methodIndex is the position of a method in the method set of the interface
// it is selected from, which is what an itab is indexed by.
func (b *builder) methodIndex(sel *types2.Selection) int {
	idx := sel.Index()
	if len(idx) == 0 {
		return 0
	}
	return idx[len(idx)-1]
}

// fieldPath follows a selection's index path, making the implicit
// dereferences of an embedded pointer field explicit.
func (b *builder) fieldPath(base Expr, t types2.Type, index []int) (Expr, types2.Type) {
	for _, i := range index {
		if p, ok := coreType(t).(*types2.Pointer); ok {
			base = &Node{Op: ODeref, Pos: base.Pos, Type: b.irType(p.Elem()), X: base}
			t = p.Elem()
		}
		st, ok := coreType(t).(*types2.Struct)
		if !ok || i >= st.NumFields() {
			b.errorf("ir: field %d of %s", i, t)
			return base, t
		}
		f := st.Field(i)
		base = &Node{Op: OField, Pos: base.Pos, Type: b.irType(f.Type()), X: base, Index: i}
		t = f.Type()
	}
	return base, t
}

// recvArg adjusts a receiver to what the method's signature wants.
//
// This is the implicit address of specs/020-ir.md's first difference: a method
// call on an addressable value whose method has a pointer receiver takes the
// address of the value, and the object holding it can no longer live in a
// register.
func (b *builder) recvArg(recv Expr, recvType types2.Type, fn *types2.Func) Expr {
	sig, _ := fn.Type().(*types2.Signature)
	if sig == nil || sig.Recv() == nil || recv == nil {
		return recv
	}
	want := sig.Recv().Type()
	if isInterface(want) {
		return recv
	}
	_, wantPtr := unalias(want).(*types2.Pointer)
	have, havePtr := unalias(recvType).(*types2.Pointer)
	switch {
	case wantPtr && !havePtr:
		return b.addrOf(recv, recvType)
	case !wantPtr && havePtr:
		return &Node{Op: ODeref, Pos: recv.Pos, Type: b.irType(have.Elem()), X: recv}
	}
	return recv
}

// index builds an index expression, and the instantiation the parser cannot
// tell from one.
func (b *builder) index(x *syntax.IndexExpr) Expr {
	if tv, ok := b.info.Types[x]; ok && tv.IsType() {
		return b.typeNode(x.Pos(), tv.Type)
	}
	if _, ok := unalias(b.typeOf(x.X)).(*types2.Signature); ok {
		// F[int]: an instantiation, not an index. A function value cannot be
		// indexed, so the type of the operand settles which one it is.
		// specs/013-generics.md owns the instance; the symbol here is the
		// generic function's.
		return b.expr(x.X)
	}
	xt := b.typeOf(x.X)
	base := b.operand(x.X)
	core := coreType(xt)
	if p, ok := core.(*types2.Pointer); ok {
		// Indexing a pointer to an array dereferences it. The specification
		// makes that implicit and this tree does not.
		base = &Node{Op: ODeref, Pos: x.Pos(), Type: b.irType(p.Elem()), X: base}
		xt = p.Elem()
		core = coreType(xt)
	}
	want := types2.Type(types2.Typ[types2.Int])
	if m, ok := core.(*types2.Map); ok {
		want = m.Key()
	}
	idx := b.assignConv(b.operand(x.Index), b.typeOf(x.Index), want)
	return &Node{Op: OIndex, Pos: x.Pos(), Type: b.nodeType(x), X: base, Y: idx}
}

// sliceExpr builds a slice expression.
//
// Args holds the three bounds in source order and a bound the source left out
// is nil, which is what tells specs/021-ssa-construction.md that the default
// applies rather than that a zero was written. A three-index slice is the one
// with a third bound.
func (b *builder) sliceExpr(x *syntax.SliceExpr) Expr {
	xt := b.typeOf(x.X)
	base := b.operand(x.X)
	if _, ok := coreType(xt).(*types2.Array); ok && b.addressable(x.X) {
		// Slicing an array takes its address. The result shares the array's
		// storage, so the array cannot live in a register.
		base = b.addrOf(base, xt)
	}
	n := &Node{Op: OSlice, Pos: x.Pos(), Type: b.nodeType(x), X: base}
	n.Args = make([]Expr, 3)
	for i, e := range x.Index {
		if e == nil {
			continue
		}
		// Every bound is an integer index, and one written with any other
		// integer type is converted like an index is.
		n.Args[i] = b.assignConv(b.operand(e), b.typeOf(e), types2.Typ[types2.Int])
	}
	return n
}

// operation builds a unary or a binary operation.
func (b *builder) operation(x *syntax.Operation) Expr {
	pos := x.Pos()
	t := b.nodeType(x)
	if x.Y == nil {
		switch x.Op {
		case syntax.And:
			return b.addrOf(b.expr(x.X), b.typeOf(x.X))
		case syntax.Mul:
			return &Node{Op: ODeref, Pos: pos, Type: t, X: b.operand(x.X)}
		case syntax.Recv:
			return &Node{Op: ORecv, Pos: pos, Type: t, X: b.operand(x.X)}
		default:
			return &Node{Op: OUnary, Op1: x.Op, Pos: pos, Type: t, X: b.operand(x.X)}
		}
	}

	switch x.Op {
	case syntax.AndAnd, syntax.OrOr:
		// The right operand is evaluated only if the left does not decide the
		// result, so nothing may be hoisted out of it.
		return &Node{Op: OBinary, Op1: x.Op, Pos: pos, Type: t, X: b.operand(x.X), Y: b.guarded(x.Y)}
	case syntax.Shl, syntax.Shr:
		// The count keeps its own type.
		return &Node{Op: OBinary, Op1: x.Op, Pos: pos, Type: t, X: b.operand(x.X), Y: b.operand(x.Y)}
	}

	lt, rt := b.typeOf(x.X), b.typeOf(x.Y)
	lhs, rhs := b.operand(x.X), b.operand(x.Y)
	lhs, rhs = b.balance(lhs, rhs, lt, rt)
	op := OBinary
	switch x.Op {
	case syntax.Eql, syntax.Neq, syntax.Lss, syntax.Leq, syntax.Gtr, syntax.Geq:
		op = OCompare
	}
	return &Node{Op: op, Op1: x.Op, Pos: pos, Type: t, X: lhs, Y: rhs}
}

// balance makes the implicit conversion between the two operands of a binary
// operation explicit.
//
// The specification converts the untyped operand to the type of the other one,
// and a comparison of a concrete value against an interface converts the
// concrete value to the interface type.
func (b *builder) balance(lhs, rhs Expr, lt, rt types2.Type) (Expr, Expr) {
	if lt == nil || rt == nil {
		return lhs, rhs
	}
	switch {
	case isUntyped(lt) && !isUntyped(rt):
		lhs = b.assignConv(lhs, lt, rt)
	case isUntyped(rt) && !isUntyped(lt):
		rhs = b.assignConv(rhs, rt, lt)
	case isInterface(lt) && !isInterface(rt):
		rhs = b.assignConv(rhs, rt, lt)
	case isInterface(rt) && !isInterface(lt):
		lhs = b.assignConv(lhs, lt, rt)
	}
	return lhs, rhs
}

// compositeLit builds a composite literal.
func (b *builder) compositeLit(x *syntax.CompositeLit) Expr {
	t := b.typeOf(x)
	if p, ok := coreType(t).(*types2.Pointer); ok {
		// An element of []*T written as {…} is the address of a T literal.
		return &Node{
			Op:   OAddr,
			Pos:  x.Pos(),
			Type: b.irType(t),
			X:    b.literal(x, p.Elem()),
		}
	}
	return b.literal(x, t)
}

// literal builds the elements of a composite literal of type t.
//
// The literal stays one node. specs/023-escape-analysis.md decides whether it
// is built in the frame or in the heap, and a literal already scattered into
// element assignments is a decision that pass can no longer make.
func (b *builder) literal(x *syntax.CompositeLit, t types2.Type) Expr {
	n := &Node{Op: OCompositeLit, Pos: x.Pos(), Type: b.irType(t)}
	switch ct := coreType(t).(type) {
	case *types2.Struct:
		// Normalised to one element per field, in declaration order, so that
		// a keyed literal and a positional one are one shape.
		n.Args = make([]Expr, ct.NumFields())
		for i, e := range x.ElemList {
			idx, val := i, e
			if kv, ok := e.(*syntax.KeyValueExpr); ok {
				key, _ := syntax.Unparen(kv.Key).(*syntax.Name)
				idx = -1
				for j := 0; j < ct.NumFields(); j++ {
					if key != nil && ct.Field(j).Name() == key.Value {
						idx = j
						break
					}
				}
				val = kv.Value
			}
			if idx < 0 || idx >= len(n.Args) {
				b.errorf("ir: no field for element %d of %s", i, t)
				continue
			}
			n.Args[idx] = b.elem(val, ct.Field(idx).Type())
		}
		for i := range n.Args {
			if n.Args[i] == nil {
				// A field the literal left out is its zero value, written
				// out rather than left implicit.
				n.Args[i] = b.zeroValue(x.Pos(), b.irType(ct.Field(i).Type()))
			}
		}

	case *types2.Array:
		n.Args = b.indexedElems(x, ct.Elem())
	case *types2.Slice:
		n.Args = b.indexedElems(x, ct.Elem())

	case *types2.Map:
		for _, e := range x.ElemList {
			kv, ok := e.(*syntax.KeyValueExpr)
			if !ok {
				b.errorf("ir: an element of a map literal is not a pair")
				continue
			}
			n.Args = append(n.Args, Assign(e.Pos(),
				b.elem(kv.Key, ct.Key()), b.elem(kv.Value, ct.Elem())))
		}

	default:
		b.errorf("ir: a composite literal of %s", t)
	}
	return n
}

// indexedElems builds the elements of an array or slice literal. An element
// with an index is a key/value pair; the others are positional.
func (b *builder) indexedElems(x *syntax.CompositeLit, elem types2.Type) []Expr {
	var out []Expr
	for _, e := range x.ElemList {
		if kv, ok := e.(*syntax.KeyValueExpr); ok {
			key := b.assignConv(b.expr(kv.Key), b.typeOf(kv.Key), types2.Typ[types2.Int])
			out = append(out, Assign(e.Pos(), key, b.elem(kv.Value, elem)))
			continue
		}
		out = append(out, b.elem(e, elem))
	}
	return out
}

// elem builds one element of a composite literal, with the conversion the
// specification calls an assignment.
func (b *builder) elem(e syntax.Expr, want types2.Type) Expr {
	return b.assignConv(b.expr(e), b.typeOf(e), want)
}

// closure builds a function literal.
//
// The literal becomes a function of the package, and the expression becomes an
// OClosure naming it and listing what it captures. A captured variable is one
// object shared by both functions, which is what makes the capture by
// reference the specification requires: an assignment in either function is
// seen by the other. Every captured object has its address taken, because that
// is what a closure holds.
func (b *builder) closure(x *syntax.FuncLit) Expr {
	sig, _ := unalias(b.typeOf(x)).(*types2.Signature)
	outerName, outerSym := "func", b.out.Path+".func"
	if b.fn != nil {
		outerName, outerSym = b.fn.Name, b.fn.Sym
	}
	fn := &Func{
		Name: fmt.Sprintf("%s.func%d", outerName, b.nfunc),
		Sym:  fmt.Sprintf("%s.func%d", outerSym, b.nfunc),
		Type: b.nodeType(x),
		Pos:  x.Pos(),
	}
	b.nfunc++

	saveFn, saveSig, saveSinks, saveFree := b.fn, b.sig, b.sinks, b.free
	b.fn, b.sig, b.sinks, b.free = fn, sig, nil, nil
	if sig != nil {
		for i := 0; i < sig.Params().Len(); i++ {
			fn.Params = append(fn.Params, b.declare(sig.Params().At(i), fn))
		}
		for i := 0; i < sig.Results().Len(); i++ {
			fn.Results = append(fn.Results, b.declare(sig.Results().At(i), fn))
		}
	}
	b.free = make(map[*Object]bool)
	fn.Body = b.block(x.Body.List)
	free := b.free
	b.fn, b.sig, b.sinks, b.free = saveFn, saveSig, saveSinks, saveFree

	// The keys are collected and sorted rather than ranged over into the
	// output, which specs/053-determinism.md requires.
	caps := make([]*Object, 0, len(free))
	for o := range free {
		caps = append(caps, o)
	}
	sort.Slice(caps, func(i, j int) bool {
		if caps[i].Pos != caps[j].Pos {
			return caps[i].Pos < caps[j].Pos
		}
		return caps[i].Name < caps[j].Name
	})

	// Index is -1 rather than a method index. A method value and a literal
	// with one capture are otherwise the same node, and a consumer that could
	// not tell them apart would treat a receiver passed by value as a
	// captured object shared by reference.
	return b.closureNode(fn, x.Pos(), caps)
}

// call builds a call, a conversion and a call to a builtin. The parser cannot
// tell the three apart and the checker can.
func (b *builder) call(x *syntax.CallExpr) Expr {
	fun := syntax.Unparen(x.Fun)
	if tv, ok := b.info.Types[fun]; ok {
		if tv.IsType() {
			if len(x.ArgList) != 1 {
				b.errorf("ir: a conversion with %d operands", len(x.ArgList))
				return b.badExpr(x.Pos())
			}
			return &Node{
				Op:   OConvert,
				Pos:  x.Pos(),
				Type: b.irType(tv.Type),
				X:    b.operand(x.ArgList[0]),
			}
		}
		if tv.IsBuiltin() {
			return b.builtin(x, fun)
		}
	}
	if sel, ok := fun.(*syntax.SelectorExpr); ok {
		if s := b.info.Selections[sel]; s != nil && s.Kind() == types2.MethodVal {
			return b.methodCall(x, sel, s)
		}
	}
	sig, _ := coreType(b.typeOf(fun)).(*types2.Signature)
	n := &Node{Op: OCall, Pos: x.Pos(), Type: b.resultType(sig), X: b.ordered(b.expr(fun))}
	n.Args = b.callArgs(x, sig)
	if x.HasDots {
		n.Index = spread
	}
	return n
}

// methodCall builds a call through a selector.
//
// Two shapes, because the two are lowered differently. A call to a method of a
// concrete type is a call to a known symbol with the receiver as the first
// argument. A call to a method of an interface reads the function out of the
// itab, so the callee is the selection itself and the receiver stays inside
// it.
func (b *builder) methodCall(x *syntax.CallExpr, sel *syntax.SelectorExpr, s *types2.Selection) Expr {
	fn, _ := s.Obj().(*types2.Func)
	if fn == nil {
		b.errorf("ir: the method %s has no object", sel.Sel.Value)
		return b.badExpr(x.Pos())
	}
	fsig, _ := fn.Type().(*types2.Signature)
	recv, recvType := b.fieldPath(b.expr(sel.X), b.typeOf(sel.X), s.Index()[:len(s.Index())-1])
	n := &Node{Op: OCall, Pos: x.Pos(), Type: b.resultType(fsig)}
	if x.HasDots {
		n.Index = spread
	}
	io := b.obj(fn)
	if isInterface(recvType) {
		n.X = &Node{
			Op:    OField,
			Pos:   sel.Pos(),
			Type:  b.irType(s.Type()),
			X:     b.ordered(recv),
			Index: b.methodIndex(s),
			Obj:   io,
		}
		n.Args = b.callArgs(x, fsig)
		return n
	}
	n.X = &Node{Op: OGlobal, Pos: sel.Pos(), Type: io.Type, Obj: io}
	n.Args = append([]Expr{b.recvArg(recv, recvType, fn)}, b.callArgs(x, fsig)...)
	return n
}

// resultType is the type of the value a call produces.
func (b *builder) resultType(sig *types2.Signature) *Type {
	if sig == nil {
		return voidType
	}
	switch sig.Results().Len() {
	case 0:
		return voidType
	case 1:
		return b.irType(sig.Results().At(0).Type())
	}
	t, err := b.conv.Tuple(sig.Results())
	if err != nil {
		b.errs = append(b.errs, err)
		return voidType
	}
	return t
}

// callArgs builds the arguments of a call, with the conversion each parameter
// makes explicit and the variadic parameter packed into its slice.
//
// The packing happens here because the element type is a fact of the
// signature, and the signature is a type checker type that no later pass may
// read (specs/002-architecture.md).
func (b *builder) callArgs(x *syntax.CallExpr, sig *types2.Signature) []Expr {
	if sig == nil {
		out := make([]Expr, 0, len(x.ArgList))
		for _, a := range x.ArgList {
			out = append(out, b.operand(a))
		}
		return out
	}
	np := sig.Params().Len()
	if len(x.ArgList) == 1 && np != 1 {
		// f(g()), where the results of g are the parameters of f. The results
		// are not separable, so the call is one argument.
		if _, ok := b.typeOf(x.ArgList[0]).(*types2.Tuple); ok {
			return []Expr{b.expr(x.ArgList[0])}
		}
	}
	var out, variadic []Expr
	for i, a := range x.ArgList {
		if sig.Variadic() && i >= np-1 {
			last := sig.Params().At(np - 1).Type()
			if x.HasDots {
				out = append(out, b.assignConv(b.operand(a), b.typeOf(a), last))
				continue
			}
			want := last
			if s, ok := coreType(last).(*types2.Slice); ok {
				want = s.Elem()
			}
			variadic = append(variadic, b.assignConv(b.operand(a), b.typeOf(a), want))
			continue
		}
		if i >= np {
			b.errorf("ir: argument %d of a call with %d parameters", i, np)
			continue
		}
		out = append(out, b.assignConv(b.operand(a), b.typeOf(a), sig.Params().At(i).Type()))
	}
	if sig.Variadic() && !x.HasDots {
		last := sig.Params().At(np - 1).Type()
		out = append(out, &Node{
			Op:   OCompositeLit,
			Pos:  x.Pos(),
			Type: b.irType(last),
			Args: variadic,
		})
	}
	return out
}

// builtinName returns the name of the builtin a call names.
func (b *builder) builtinName(fun syntax.Expr) string {
	var bi *types2.Builtin
	switch f := fun.(type) {
	case *syntax.Name:
		bi, _ = b.info.Uses[f].(*types2.Builtin)
	case *syntax.SelectorExpr:
		bi, _ = b.info.Uses[f.Sel].(*types2.Builtin)
	}
	if bi == nil {
		return ""
	}
	if pkg := bi.Pkg(); pkg != nil && pkg.Name() == "unsafe" {
		return "unsafe." + bi.Name()
	}
	return bi.Name()
}

// builtin builds a call to a predeclared function.
//
// Most of them have a node of their own, because they are rows of
// specs/020-ir.md's lowering table and specs/031-runtime-lowering.md needs to
// find them. The ones with no node are a call to a symbol named after the
// builtin.
func (b *builder) builtin(x *syntax.CallExpr, fun syntax.Expr) Expr {
	name := b.builtinName(fun)
	pos := x.Pos()
	t := b.nodeType(x)
	arg := func(i int) Expr {
		if i >= len(x.ArgList) {
			return nil
		}
		return b.operand(x.ArgList[i])
	}
	argTo := func(i int, want types2.Type) Expr {
		if i >= len(x.ArgList) {
			return nil
		}
		return b.assignConv(b.operand(x.ArgList[i]), b.typeOf(x.ArgList[i]), want)
	}
	intType := types2.Type(types2.Typ[types2.Int])

	switch name {
	case "len":
		return &Node{Op: OLen, Pos: pos, Type: t, X: arg(0)}
	case "cap":
		return &Node{Op: OCap, Pos: pos, Type: t, X: arg(0)}
	case "new":
		// The type of the node is the result type, which is the pointer.
		//
		// Go 1.26 accepts new(expr) as well as new(T), and the two are not the
		// same node. new(T) allocates a zero value, and new(expr) allocates
		// and stores the value the expression produced. Dropping the operand
		// would compile the second into the first, which is a pointer to a
		// zero where the program asked for a pointer to 123.
		n := &Node{Op: ONew, Pos: pos, Type: t}
		if len(x.ArgList) == 1 && !b.info.Types[x.ArgList[0]].IsType() {
			var elem types2.Type
			if p, ok := coreType(b.typeOf(x)).(*types2.Pointer); ok {
				elem = p.Elem()
			}
			n.X = b.assignConv(b.operand(x.ArgList[0]), b.typeOf(x.ArgList[0]), elem)
		}
		return n
	case "make":
		n := &Node{Op: OMake, Pos: pos, Type: t}
		for i := 1; i < len(x.ArgList); i++ {
			n.Args = append(n.Args, argTo(i, intType))
		}
		return n
	case "append":
		n := &Node{Op: OAppend, Pos: pos, Type: t}
		if x.HasDots {
			n.Index = spread
		}
		elem := types2.Type(nil)
		if s, ok := coreType(b.typeOf(x)).(*types2.Slice); ok {
			elem = s.Elem()
		}
		for i := range x.ArgList {
			if i == 0 || (x.HasDots && i == len(x.ArgList)-1) {
				n.Args = append(n.Args, arg(i))
				continue
			}
			n.Args = append(n.Args, argTo(i, elem))
		}
		return n
	case "copy":
		return &Node{Op: OCopy, Pos: pos, Type: t, X: arg(0), Y: arg(1)}
	case "delete":
		key := types2.Type(nil)
		if m, ok := coreType(b.typeOf(x.ArgList[0])).(*types2.Map); ok {
			key = m.Key()
		}
		return &Node{Op: ODelete, Pos: pos, Type: voidType, X: arg(0), Y: argTo(1, key)}
	case "panic":
		// The operand of panic is an interface value, so a concrete operand is
		// converted, like any other assignment to an interface.
		return &Node{Op: OPanic, Pos: pos, Type: voidType, X: argTo(0, b.anyType())}
	case "recover":
		return &Node{Op: ORecover, Pos: pos, Type: t}
	case "complex":
		return &Node{Op: OComplex, Pos: pos, Type: t, X: arg(0), Y: arg(1)}
	case "real":
		return &Node{Op: OReal, Pos: pos, Type: t, X: arg(0)}
	case "imag":
		return &Node{Op: OImag, Pos: pos, Type: t, X: arg(0)}
	case "clear":
		return &Node{Op: OClear, Pos: pos, Type: voidType, X: arg(0)}
	case "close":
		return &Node{Op: OClose, Pos: pos, Type: voidType, X: arg(0)}
	case "print", "println":
		op := OPrint
		if name == "println" {
			op = OPrintln
		}
		n := &Node{Op: op, Pos: pos, Type: voidType}
		for i := range x.ArgList {
			n.Args = append(n.Args, arg(i))
		}
		return n
	case "min", "max":
		op := OMin
		if name == "max" {
			op = OMax
		}
		n := &Node{Op: op, Pos: pos, Type: t}
		for i := range x.ArgList {
			n.Args = append(n.Args, argTo(i, b.typeOf(x)))
		}
		return n

	// The unsafe intrinsics. They are operations and not calls: each lowers to
	// pointer arithmetic or to building a slice or string header, and none of
	// them reaches the runtime.
	//
	// The offset of Add and the lengths of Slice and String keep the type they
	// were written with. The specification asks only for an integer type, and
	// converting a uintptr offset to int here would be a claim about its sign
	// that the source did not make.
	case "unsafe.Add":
		return &Node{Op: OUnsafeAdd, Pos: pos, Type: t, X: arg(0), Y: arg(1)}
	case "unsafe.Slice":
		return &Node{Op: OUnsafeSlice, Pos: pos, Type: t, X: arg(0), Y: arg(1)}
	case "unsafe.SliceData":
		return &Node{Op: OUnsafeSliceData, Pos: pos, Type: t, X: arg(0)}
	case "unsafe.String":
		return &Node{Op: OUnsafeString, Pos: pos, Type: t, X: arg(0), Y: arg(1)}
	case "unsafe.StringData":
		return &Node{Op: OUnsafeStringData, Pos: pos, Type: t, X: arg(0)}
	}

	// Every builtin the language has is a case above, and the ones that are
	// always constant were folded by the checker before this pass saw them.
	// Reaching here is a builtin the language grew, and naming it is better
	// than encoding it as something it is not.
	b.errorf("ir: no node for the builtin %s", name)
	return b.badExpr(pos)
}

// anyType is the empty interface, which is the type of the operand of panic
// and of a value assigned to a variable of interface type with no methods.
func (b *builder) anyType() types2.Type {
	if b.any == nil {
		if o := types2.Universe.Lookup("any"); o != nil {
			b.any = o.Type()
		} else {
			b.any = types2.NewInterfaceType(nil, nil)
		}
	}
	return b.any
}
