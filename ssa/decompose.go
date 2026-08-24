// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"crypto/sha256"
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
)

// The value decomposition pass of specs/025-lowering-and-rules.md.
//
// A value wider than a machine register cannot be one machine instruction. A
// string is a pointer and a length, a slice adds a capacity, an interface is
// two words, and a struct, an array or a tuple is whatever its parts are.
// Selection has one rule per operation and every rule assumes a value that
// fits one register, so a value that does not fit has to stop existing before
// selection runs. That is what this pass does, and it is why it runs first.
//
// # Why it cannot be a rule and cannot be pushed downstream
//
// A 16-byte Store cannot become a call to runtime.memmove. memmove takes a
// source address and a Store has a source value; there is nothing to take the
// address of. The split has to happen while the value is still a value, which
// means before selection and not inside it.
//
// # The parts carry the right types
//
// ir.Type.PtrBits on each part is what tells specs/027-liveness-and-stackmaps.md
// which words of a frame hold pointers. A string split into two int64s would
// make its data pointer invisible to the collector, and nothing would fail
// until a collection ran at the wrong moment. Every part therefore keeps the
// type of the word it holds, and the union of the parts' pointer maps, at
// their offsets, is the pointer map of the type they came from. TestPointerMap
// asserts exactly that.
//
// # Where decomposition stops
//
// Decomposition trades one memory object for several values that are live at
// once. It pays while that number is small against the register file, and it
// stops paying when the allocator has to spill the parts back into the frame
// that they came out of. Above the bound a value is a memory object: a copy of
// one is a block move, which is one instruction sequence rather than a load
// and a store per word.
//
// An object whose address is taken is not affected. It never becomes a value
// at all, because specs/021-ssa-construction.md keeps it in the frame and
// reaches it through memory, so there is nothing here to split.

// MaxDecomposeParts is the largest number of parts a value is split into.
//
// Four covers every multi-word type the language builds in: a string is two, a
// slice is three, an interface is two, a complex is two, and a struct or an
// array of up to four scalars is four. Beyond it a value is treated as a
// memory object, which is what the language already does with it: an aggregate
// that large is passed and returned through memory by every calling convention
// on both targets of specs/000-decisions.md.
//
// A tuple is exempt. It is not a memory object and has no address, so there is
// no fallback to be had; see leavesOf.
const MaxDecomposeParts = 4

// StringSym is the static data a string constant points at.
//
// It carries the bytes because *ir.Object cannot: an object names a
// declaration, and a string constant has no declaration. The value that points
// at the bytes is an OpAddr with this as its Aux, so the set of string symbols
// a function needs is found by walking the function rather than by being
// handed a map, which is what specs/053-determinism.md prefers.
//
// The name is content addressed, so two equal constants in one build name one
// symbol and two builds of one input name the same symbols.
type StringSym struct {
	Obj  *ir.Object
	Text string
}

// String returns the symbol name, which is what a dump prints. Without it a
// dump would print an address and two dumps of one build would differ.
func (s *StringSym) String() string {
	if s == nil || s.Obj == nil {
		return "<nil>"
	}
	return s.Obj.Name
}

// Decompose replaces every value wider than a machine register by one value
// per part, and rewrites every operation over such a value into the
// corresponding operations over its parts.
//
// It is idempotent: a second call finds nothing left to split.
func Decompose(f *Func) {
	d := &decomposer{
		f:         f,
		leafCache: make(map[*ir.Type][]partLeaf),
		ptrCache:  make(map[*ir.Type]*ir.Type),
	}
	d.run()
}

// partLeaf is one part of a composite type: where it sits and what it is.
type partLeaf struct {
	off int64
	typ *ir.Type
}

type decomposer struct {
	f *Func

	// leafCache and ptrCache are looked up by key and never ranged over.
	leafCache map[*ir.Type][]partLeaf
	ptrCache  map[*ir.Type]*ir.Type

	tInt   *ir.Type
	tByte  *ir.Type
	tUnsaf *ir.Type

	// users lists the values that read each value, indexed by identifier.
	// Value.uses is construction bookkeeping and is documented as stale, so
	// the graph is walked here instead.
	users [][]*Value

	// control marks a value that a block branches on or returns with. Such a
	// value is never composite, and the mark is what makes that a checked
	// property rather than an assumption.
	control []bool

	// split marks a value this pass replaces by its parts, and parts holds the
	// replacements. Both are indexed by the identifiers that existed when the
	// plan was made; a value created by this pass is never composite.
	split []bool
	parts [][]*Value

	// remove marks a value that leaves the function: a split original, or a
	// load whose only reader became a block move.
	remove []bool

	// moves counts the load and store pairs that became one block move. It is
	// reported separately, because it is a different answer to the same
	// question and the corpus number should say how much came from each.
	moves int
}

func (d *decomposer) run() {
	d.index()
	d.aggregateCopies()
	d.index()
	d.plan()
	d.create()
	d.link()
	d.rewrite()
}

// index rebuilds the reader lists and the control marks.
func (d *decomposer) index() {
	n := d.f.NumValues()
	d.users = make([][]*Value, n)
	d.control = make([]bool, n)
	for _, b := range d.f.Blocks {
		if b.Control != nil && int(b.Control.ID) < n {
			d.control[b.Control.ID] = true
		}
		for _, v := range b.Values {
			for _, a := range v.Args {
				if a != nil && int(a.ID) < n {
					d.users[a.ID] = append(d.users[a.ID], v)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Types

// Multiword reports whether a value of type t needs more than one machine
// value to hold it.
//
// It is a question about the shape of the type and not about its size. A
// complex64 is eight bytes and still two values, because the two halves are
// two floating-point registers and no instruction adds them as one.
func Multiword(t *ir.Type) bool {
	if t == nil || t == MemType || t == FlagsType {
		return false
	}
	switch t.Kind {
	case ir.String, ir.Slice, ir.Interface, ir.Complex64, ir.Complex128,
		ir.Struct, ir.Array, ir.Tuple:
		return true
	}
	return false
}

// splittable reports whether a value of type t is replaced by its parts.
//
// A tuple always is. It is a multi-value expression rather than an object: it
// has no address, no layout the ABI respects, and no memory form to fall back
// to, so a bound on it would leave a value that nothing can lower.
func (d *decomposer) splittable(t *ir.Type) bool {
	if !Multiword(t) {
		return false
	}
	if t.Kind == ir.Tuple {
		return true
	}
	return len(d.leaves(t)) <= MaxDecomposeParts
}

// leaves returns the parts of t, in increasing offset order.
//
// The walk stops at the bound, so an array of a million elements costs the
// bound rather than a million entries.
func (d *decomposer) leaves(t *ir.Type) []partLeaf {
	if have, ok := d.leafCache[t]; ok {
		return have
	}
	out := d.flatten(nil, t, 0, MaxDecomposeParts+1)
	d.leafCache[t] = out
	return out
}

// leavesOf returns the parts of t with no bound, for a type already known to
// be splittable.
func (d *decomposer) leavesOf(t *ir.Type) []partLeaf {
	if t.Kind != ir.Tuple {
		return d.leaves(t)
	}
	// A tuple has no bound, so the cached bounded walk may be short.
	return d.flatten(nil, t, 0, 1<<30)
}

// flatten appends the parts of t at offset off, stopping once limit parts are
// collected. Recursion ends at a type that fits one machine value.
func (d *decomposer) flatten(out []partLeaf, t *ir.Type, off int64, limit int) []partLeaf {
	if t == nil || len(out) >= limit {
		return out
	}
	switch t.Kind {
	case ir.String:
		out = append(out, partLeaf{off, d.ptrTo(d.byteType())})
		out = append(out, partLeaf{off + ir.PtrSize, d.intType()})
		return out

	case ir.Slice:
		out = append(out, partLeaf{off, d.ptrTo(t.Elem)})
		out = append(out, partLeaf{off + ir.PtrSize, d.intType()})
		out = append(out, partLeaf{off + 2*ir.PtrSize, d.intType()})
		return out

	case ir.Interface:
		// Both words are pointers, which is what scalarPtrBits records for an
		// interface. A part typed as an integer here would hide one of them
		// from the collector.
		p := d.unsafeType()
		out = append(out, partLeaf{off, p})
		out = append(out, partLeaf{off + ir.PtrSize, p})
		return out

	case ir.Complex64:
		e := d.scalarType(ir.Float32)
		out = append(out, partLeaf{off, e}, partLeaf{off + 4, e})
		return out

	case ir.Complex128:
		e := d.scalarType(ir.Float64)
		out = append(out, partLeaf{off, e}, partLeaf{off + 8, e})
		return out

	case ir.Struct, ir.Tuple:
		for i := range t.Fields {
			if len(out) >= limit {
				return out
			}
			f := &t.Fields[i]
			out = d.flatten(out, f.Type, off+f.Offset, limit)
		}
		return out

	case ir.Array:
		if t.Elem == nil {
			return out
		}
		for i := int64(0); i < t.Len; i++ {
			if len(out) >= limit {
				return out
			}
			out = d.flatten(out, t.Elem, off+i*t.Elem.Size, limit)
		}
		return out
	}

	// A type that occupies no storage contributes no part. A struct of only
	// zero-size fields is the case, and it must contribute none rather than
	// one of size zero, because no load or store has that width.
	if t.Size == 0 {
		return out
	}
	return append(out, partLeaf{off, t})
}

// intType returns the type of a length or a capacity.
func (d *decomposer) intType() *ir.Type {
	if d.tInt == nil {
		d.tInt = &ir.Type{Kind: ir.Int64, Size: 8, Align: 8, Name: "int"}
	}
	return d.tInt
}

func (d *decomposer) byteType() *ir.Type {
	if d.tByte == nil {
		d.tByte = &ir.Type{Kind: ir.Uint8, Size: 1, Align: 1, Name: "uint8"}
	}
	return d.tByte
}

// unsafeType returns the type of a word that holds a pointer to something the
// type system no longer describes: an itab, or the data word of an interface.
func (d *decomposer) unsafeType() *ir.Type {
	if d.tUnsaf == nil {
		d.tUnsaf = &ir.Type{
			Kind: ir.UnsafePtr, Size: ir.PtrSize, Align: ir.PtrSize,
			PtrBits: []byte{1}, Name: "unsafe.Pointer",
		}
	}
	return d.tUnsaf
}

func (d *decomposer) scalarType(k ir.Kind) *ir.Type {
	t := &ir.Type{Kind: k, Name: k.String()}
	// Layout fills Size, Align and PtrBits. Writing them here instead would be
	// a second answer to a question ir.Layout already answers, and the pointer
	// map is the answer that must not be got twice.
	if err := ir.Layout(t); err != nil {
		panic("ssa: decompose: " + err.Error())
	}
	return t
}

// ptrTo returns the pointer type to t, with the pointer map a pointer has.
//
// PtrBits is set explicitly rather than left to ir.Layout, because a type with
// a non-zero Align is already laid out as far as ir.Layout is concerned and it
// would return without filling the map. A pointer part with an empty map is
// the exact failure this pass exists to avoid.
func (d *decomposer) ptrTo(elem *ir.Type) *ir.Type {
	if elem == nil {
		elem = d.byteType()
	}
	if p, ok := d.ptrCache[elem]; ok {
		return p
	}
	p := &ir.Type{
		Kind: ir.Ptr, Size: ir.PtrSize, Align: ir.PtrSize,
		PtrBits: []byte{1}, Elem: elem,
	}
	d.ptrCache[elem] = p
	return p
}

// ---------------------------------------------------------------------------
// Aggregate copies

// aggregateCopies rewrites the copy of a value that stays in memory.
//
// A store of a load, of a type too wide to split, is a copy from one address
// to another. Both addresses are already in the graph, so the copy has a block
// move and needs no value that does not fit a register. This is the memory
// half of the answer that MaxDecomposeParts divides: above the bound a value
// is a memory object, and this is what an assignment of one becomes.
//
// The conditions are strict on purpose. The load must be the store's only
// reader, or the value is needed elsewhere and the load cannot go. The load
// must read the memory the store writes, or something wrote in between and
// moving the read past it would change what is copied.
func (d *decomposer) aggregateCopies() {
	for _, b := range d.f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpStore || len(v.Args) != 3 {
				continue
			}
			ld := v.Args[1]
			if ld.Op != OpLoad || ld.Block != b || len(ld.Args) != 2 {
				continue
			}
			t := ld.Type
			if !Multiword(t) || d.splittable(t) || t.Size == 0 || t.Size != v.AuxInt {
				continue
			}
			if len(d.users[ld.ID]) != 1 || d.control[ld.ID] {
				continue
			}
			if ld.Args[1] != v.Args[2] {
				continue
			}
			size := t.Size
			setArgs(v, v.Args[0], ld.Args[0], v.Args[2])
			v.Op = OpMove
			v.AuxInt = size
			d.dropLater(ld)
			d.moves++
		}
	}
}

func (d *decomposer) dropLater(v *Value) {
	for len(d.remove) <= int(v.ID) {
		d.remove = append(d.remove, false)
	}
	d.remove[v.ID] = true
}

func (d *decomposer) dropped(v *Value) bool {
	return int(v.ID) < len(d.remove) && d.remove[v.ID]
}

// ---------------------------------------------------------------------------
// The plan

// plan decides which values are replaced by their parts.
//
// A value is a candidate when its type is splittable and its operation has a
// per-part form. It stops being one when a reader has no per-part form, or
// when a value it is built from is not split either. The second is why this is
// a fixed point rather than one walk: a phi over a value that stays whole must
// stay whole, and clearing it can clear a phi that feeds it.
func (d *decomposer) plan() {
	f := d.f
	n := f.NumValues()
	d.split = make([]bool, n)
	d.parts = make([][]*Value, n)

	for _, b := range f.Blocks {
		for _, v := range b.Values {
			d.split[v.ID] = d.candidate(v)
		}
	}
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if !d.split[v.ID] {
					continue
				}
				if !d.readersOK(v) || !d.sourcesOK(v) {
					d.split[v.ID] = false
					changed = true
				}
			}
		}
	}
}

// candidate reports whether v produces a splittable value in a form this pass
// knows how to build one part at a time.
func (d *decomposer) candidate(v *Value) bool {
	if d.dropped(v) || d.control[v.ID] || !d.splittable(v.Type) {
		return false
	}
	switch v.Op {
	case OpLoad, OpPhi, OpCopy, OpArg, OpConstNil:
		return true
	case OpSelectN:
		// The index counts the machine words of the result area after this
		// pass, so a part of result i is word i+j. That renumbering is only
		// correct for the first result, and specs/021 builds no other: a call
		// with several results is one SelectN of a tuple.
		return v.AuxInt == 0
	case OpConstString:
		_, ok := v.Aux.(string)
		return ok || v.Aux == nil
	}
	return false
}

// readersOK reports whether every reader of v has a per-part form.
func (d *decomposer) readersOK(v *Value) bool {
	if d.control[v.ID] {
		return false
	}
	for _, u := range d.users[v.ID] {
		if d.dropped(u) {
			continue
		}
		switch u.Op {
		case OpStore:
			// A composite value is never an address, so it can only be the
			// value the store writes.
			if len(u.Args) != 3 || u.Args[1] != v {
				return false
			}
		case OpPhi, OpCopy:
			if !d.isSplit(u) {
				return false
			}
		case OpStaticCall, OpClosureCall, OpInterCall, OpMakeResult:
			// The parts take the place of the whole in the argument list.
			// specs/030-abi.md assigns them locations.
		case OpEq, OpNeq:
			if !d.equalityOK(u) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// sourcesOK reports whether the values v is built from are split too.
func (d *decomposer) sourcesOK(v *Value) bool {
	if v.Op != OpPhi && v.Op != OpCopy {
		return true
	}
	want := len(d.leavesOf(v.Type))
	for _, a := range v.Args {
		if a == nil || !d.isSplit(a) || len(d.leavesOf(a.Type)) != want {
			return false
		}
	}
	return len(v.Args) > 0
}

// equalityOK reports whether == and != over this type are the comparison of
// its parts.
//
// For most types they are, and per part is more correct than a comparison of
// the bytes: Go does not define the padding between two fields, and a part
// comparison never reads it.
//
// For a string or an interface they are not. String equality compares the
// bytes and not the pointer, so two equal strings at two addresses would
// compare unequal. General interface equality calls the type's equality
// function. Both need a call and a branch, which this pass does not build, so
// it leaves them alone and lowering refuses them by name.
//
// An interface against the literal nil is the exception, and it is the common
// one. The zero interface is two zero words and nothing else is, so comparing
// both words against zero is the whole answer.
func (d *decomposer) equalityOK(u *Value) bool {
	if len(u.Args) != 2 {
		return false
	}
	x, y := u.Args[0], u.Args[1]
	if x.Type == nil || y.Type == nil {
		return false
	}
	if !d.isSplit(x) || !d.isSplit(y) {
		return false
	}
	xs, ys := d.leavesOf(x.Type), d.leavesOf(y.Type)
	if len(xs) != len(ys) {
		return false
	}
	for i := range xs {
		if xs[i].typ.Size != ys[i].typ.Size || xs[i].off != ys[i].off {
			return false
		}
	}
	if comparableByParts(x.Type) && comparableByParts(y.Type) {
		return true
	}
	return x.Type.Kind == ir.Interface && y.Type.Kind == ir.Interface &&
		(x.Op == OpConstNil || y.Op == OpConstNil)
}

// comparableByParts reports whether == over t is == over each of its parts.
func comparableByParts(t *ir.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case ir.String, ir.Interface, ir.Slice, ir.Map, ir.FuncKind:
		// The first two need a call. The last three are not comparable in Go
		// at all, except against nil, and a part comparison against nil is
		// not what the language means by it.
		return false
	case ir.Struct, ir.Tuple:
		for i := range t.Fields {
			if !comparableByParts(t.Fields[i].Type) {
				return false
			}
		}
		return true
	case ir.Array:
		return comparableByParts(t.Elem)
	}
	return true
}

func (d *decomposer) isSplit(v *Value) bool {
	return v != nil && int(v.ID) < len(d.split) && d.split[v.ID]
}

// ---------------------------------------------------------------------------
// Building the parts

// create makes the parts of every value the plan splits.
//
// The parts go immediately before the value they replace, which keeps a phi's
// parts inside the phi prefix the verifier requires and keeps every other
// part after the values it reads. A phi's and a copy's arguments are filled
// afterwards, by link, because a loop phi reads a value that is defined later.
func (d *decomposer) create() {
	for _, b := range d.f.Blocks {
		out := make([]*Value, 0, len(b.Values))
		for _, v := range b.Values {
			if d.isSplit(v) {
				before, ps := d.makeParts(b, v)
				out = append(out, before...)
				out = append(out, ps...)
				d.parts[v.ID] = ps
			}
			out = append(out, v)
		}
		b.Values = out
	}
}

// makeParts returns the values to insert ahead of v and the parts themselves.
func (d *decomposer) makeParts(b *Block, v *Value) (before, parts []*Value) {
	ls := d.leavesOf(v.Type)
	parts = make([]*Value, 0, len(ls))

	switch v.Op {
	case OpLoad:
		ptr, mem := v.Args[0], v.Args[1]
		for _, lf := range ls {
			a := ptr
			if lf.off != 0 {
				a = d.mk(b, v.Pos, OpOffPtr, d.ptrTo(lf.typ), ptr)
				a.AuxInt = lf.off
				before = append(before, a)
			}
			parts = append(parts, d.mk(b, v.Pos, OpLoad, lf.typ, a, mem))
		}

	case OpPhi:
		for _, lf := range ls {
			parts = append(parts, d.mk(b, v.Pos, OpPhi, lf.typ))
		}

	case OpCopy:
		for _, lf := range ls {
			parts = append(parts, d.mk(b, v.Pos, OpCopy, lf.typ))
		}

	case OpArg:
		// Aux still names the parameter and AuxInt is the byte offset of the
		// part within it. specs/030-abi.md needs both to place the part, and
		// the pair says which word of which parameter this is.
		for _, lf := range ls {
			p := d.mk(b, v.Pos, OpArg, lf.typ)
			p.Aux = v.Aux
			p.AuxInt = lf.off
			parts = append(parts, p)
		}

	case OpSelectN:
		call := v.Args[0]
		for i, lf := range ls {
			p := d.mk(b, v.Pos, OpSelectN, lf.typ, call)
			p.AuxInt = v.AuxInt + int64(i)
			parts = append(parts, p)
		}

	case OpConstString:
		s, _ := v.Aux.(string)
		data := d.mk(b, v.Pos, OpConstNil, ls[0].typ)
		if s != "" {
			data = d.mk(b, v.Pos, OpAddr, ls[0].typ)
			data.Aux = newStringSym(s)
		}
		n := d.mk(b, v.Pos, OpConstInt, ls[1].typ)
		n.AuxInt = int64(len(s))
		parts = append(parts, data, n)

	case OpConstNil:
		for _, lf := range ls {
			parts = append(parts, d.zeroPart(b, v.Pos, lf.typ))
		}
	}
	return before, parts
}

// zeroPart returns the zero value of one part.
func (d *decomposer) zeroPart(b *Block, pos syntax.Pos, t *ir.Type) *Value {
	switch {
	case t.Kind == ir.Bool:
		return d.mk(b, pos, OpConstBool, t)
	case t.Kind.IsInteger():
		return d.mk(b, pos, OpConstInt, t)
	case t.Kind.IsFloat():
		v := d.mk(b, pos, OpConstFloat, t)
		v.Aux = float64(0)
		return v
	}
	return d.mk(b, pos, OpConstNil, t)
}

// newStringSym returns the symbol that holds the bytes of s.
//
// The name is the hash of the content, so it does not depend on the order the
// constants were met in, which specs/053-determinism.md rules out, and two
// equal constants name one symbol.
func newStringSym(s string) *StringSym {
	sum := sha256.Sum256([]byte(s))
	t := &ir.Type{Kind: ir.Array, Elem: &ir.Type{Kind: ir.Uint8}, Len: int64(len(s))}
	if err := ir.Layout(t); err != nil {
		panic("ssa: decompose: " + err.Error())
	}
	return &StringSym{
		Obj: &ir.Object{
			Name:  fmt.Sprintf("go:string.%x", sum[:8]),
			Type:  t,
			Class: ir.ClassGlobal,
		},
		Text: s,
	}
}

// link fills the arguments of the phi and copy parts.
//
// It runs after every part exists, because a loop phi reads a value defined in
// the block that branches back to it.
func (d *decomposer) link() {
	for _, b := range d.f.Blocks {
		for _, v := range b.Values {
			if !d.isSplit(v) || (v.Op != OpPhi && v.Op != OpCopy) {
				continue
			}
			ps := d.parts[v.ID]
			for i, p := range ps {
				for _, a := range v.Args {
					p.AddArg(d.parts[a.ID][i])
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Rewriting the readers

// rewrite replaces every operation over a whole value by the operations over
// its parts, and drops the values that are gone.
func (d *decomposer) rewrite() {
	for _, b := range d.f.Blocks {
		out := make([]*Value, 0, len(b.Values))
		for _, v := range b.Values {
			if d.isSplit(v) || d.dropped(v) {
				v.dead = true
				continue
			}
			switch v.Op {
			case OpStore:
				if len(v.Args) == 3 && d.isSplit(v.Args[1]) {
					if !d.expandStore(b, v, &out) {
						v.dead = true
						continue
					}
				}
			case OpEq, OpNeq:
				if len(v.Args) == 2 && d.isSplit(v.Args[0]) {
					d.expandEqual(b, v, &out)
				}
			case OpStaticCall, OpClosureCall, OpInterCall, OpMakeResult:
				d.spliceArgs(v)
			}
			out = append(out, v)
		}
		b.Values = out
	}
}

// expandStore turns one store of a whole value into one store per part.
//
// v itself becomes the last store of the chain, so the memory it produced is
// still the memory it produces and no reader in this or any later block has to
// be redirected. It reports whether v survives: a value with no parts, such as
// an empty struct, writes nothing, and then the store is removed and its
// readers take the memory it took.
func (d *decomposer) expandStore(b *Block, v *Value, out *[]*Value) bool {
	val := v.Args[1]
	ps := d.parts[val.ID]
	ls := d.leavesOf(val.Type)
	ptr, mem := v.Args[0], v.Args[2]

	if len(ps) == 0 {
		// Not a Copy of the memory: Copy does not produce memory, and the
		// chain the verifier walks would stop advancing at it.
		d.replaceAll(v, mem)
		return false
	}

	m := mem
	for i := 0; i < len(ps)-1; i++ {
		a := d.partAddr(b, v.Pos, ptr, ls[i], out)
		s := d.mk(b, v.Pos, OpStore, MemType, a, ps[i], m)
		s.AuxInt = ls[i].typ.Size
		*out = append(*out, s)
		m = s
	}
	last := len(ps) - 1
	a := d.partAddr(b, v.Pos, ptr, ls[last], out)
	setArgs(v, a, ps[last], m)
	v.AuxInt = ls[last].typ.Size
	return true
}

// partAddr returns the address of one part, reusing the base at offset zero.
func (d *decomposer) partAddr(b *Block, pos syntax.Pos, ptr *Value, lf partLeaf, out *[]*Value) *Value {
	if lf.off == 0 {
		return ptr
	}
	a := d.mk(b, pos, OpOffPtr, d.ptrTo(lf.typ), ptr)
	a.AuxInt = lf.off
	*out = append(*out, a)
	return a
}

// expandEqual turns == and != over a whole value into the comparison of every
// part, joined by and, or by or for !=.
//
// v becomes the join, so nothing that reads the result is redirected. A value
// with no parts is equal to every other value of its type, which is what an
// empty struct is.
func (d *decomposer) expandEqual(b *Block, v *Value, out *[]*Value) {
	xs, ys := d.parts[v.Args[0].ID], d.parts[v.Args[1].ID]
	cmp := v.Op
	join := OpAnd
	if cmp == OpNeq {
		join = OpOr
	}

	if len(xs) == 0 {
		v.Op = OpConstBool
		setArgs(v)
		v.AuxInt = 0
		if cmp == OpEq {
			v.AuxInt = 1
		}
		return
	}
	if len(xs) == 1 {
		setArgs(v, xs[0], ys[0])
		return
	}

	acc := d.mk(b, v.Pos, cmp, v.Type, xs[0], ys[0])
	*out = append(*out, acc)
	for i := 1; i < len(xs)-1; i++ {
		c := d.mk(b, v.Pos, cmp, v.Type, xs[i], ys[i])
		*out = append(*out, c)
		j := d.mk(b, v.Pos, join, v.Type, acc, c)
		*out = append(*out, j)
		acc = j
	}
	last := len(xs) - 1
	c := d.mk(b, v.Pos, cmp, v.Type, xs[last], ys[last])
	*out = append(*out, c)
	v.Op = join
	setArgs(v, acc, c)
}

// spliceArgs replaces every whole-value argument of a call or a return by its
// parts, in place, so the argument order is still the source order.
func (d *decomposer) spliceArgs(v *Value) {
	need := false
	for _, a := range v.Args {
		if d.isSplit(a) {
			need = true
			break
		}
	}
	if !need {
		return
	}
	args := make([]*Value, 0, len(v.Args))
	for _, a := range v.Args {
		if d.isSplit(a) {
			args = append(args, d.parts[a.ID]...)
			continue
		}
		args = append(args, a)
	}
	setArgs(v, args...)
}

// replaceAll redirects every reader of old, in the whole function, to fresh.
//
// Function wide, because a memory value that disappears has readers in later
// blocks and Value.uses is stale by the time this pass runs.
func (d *decomposer) replaceAll(old, fresh *Value) {
	if old == fresh {
		return
	}
	for _, b := range d.f.Blocks {
		for _, v := range b.Values {
			for i, a := range v.Args {
				if a == old {
					v.SetArg(i, fresh)
				}
			}
		}
		if b.Control == old {
			b.Control = fresh
		}
	}
}

// mk creates a value in b without adding it to the block's list. The caller
// decides where it goes, which is what keeps the phi prefix intact.
func (d *decomposer) mk(b *Block, pos syntax.Pos, op Op, t *ir.Type, args ...*Value) *Value {
	v := d.f.newValue(op, t, pos)
	v.Block = b
	for _, a := range args {
		v.AddArg(a)
	}
	return v
}

// setArgs replaces the argument list of v.
func setArgs(v *Value, args ...*Value) {
	v.Args = v.Args[:0]
	for _, a := range args {
		v.AddArg(a)
	}
}

// ---------------------------------------------------------------------------
// The checks

// CheckDecomposed reports every value that is still wider than one machine
// register.
//
// It is the assertion this pass owes, and it is separate from CheckLowered for
// a reason that would otherwise hide a failure: OpArg and OpSelectN are
// pseudo-operations, so a composite argument or call result that this pass did
// not split survives lowering without a complaint and would be counted as a
// function that lowered completely.
func CheckDecomposed(f *Func) []Violation {
	var out []Violation
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !Multiword(v.Type) {
				continue
			}
			out = append(out, Violation{
				Invariant: InvOpForm,
				Block:     b.ID,
				Value:     v.ID,
				Detail:    fmt.Sprintf("%v has type %v, which is wider than a register", v.Op, v.Type),
			})
		}
	}
	return out
}

// PartsOfType returns the offset and the type of each part a value of type t
// is split into, and reports whether t is split at all.
//
// It is the interface the tests and specs/030-abi.md need: the ABI assigns a
// location per part, and it must agree with this pass about what the parts
// are.
func PartsOfType(t *ir.Type) ([]int64, []*ir.Type, bool) {
	d := &decomposer{
		leafCache: make(map[*ir.Type][]partLeaf),
		ptrCache:  make(map[*ir.Type]*ir.Type),
	}
	if !d.splittable(t) {
		return nil, nil, false
	}
	ls := d.leavesOf(t)
	offs := make([]int64, len(ls))
	types := make([]*ir.Type, len(ls))
	for i, lf := range ls {
		offs[i], types[i] = lf.off, lf.typ
	}
	return offs, types, true
}
