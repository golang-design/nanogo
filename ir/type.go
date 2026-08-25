// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package ir holds the typed tree intermediate representation.
//
// It is the first of the two representations of specs/002-architecture.md: the
// type-checked program with Go semantics still visible, where a range statement
// is a range statement and a map index is a map index. The passes that reason
// about the program as Go rather than as a machine run here.
//
// This file holds the type boundary. specs/002-architecture.md forbids anything
// below the IR from importing the type checker, and this is where that line is
// drawn: below the IR, a type is a size, an alignment, and a pointer map. Not a
// name, not a method set, not an underlying type.
//
// See specs/020-ir.md and specs/030-abi.md.
package ir

import (
	"fmt"
	"strings"
)

// Kind is the machine-relevant classification of a type.
//
// It is coarser than the language's set of types on purpose. Two types with the
// same kind, size and pointer map are the same type to everything below the IR.
type Kind uint8

const (
	Invalid Kind = iota

	Bool
	Int8
	Int16
	Int32
	Int64
	Uint8
	Uint16
	Uint32
	Uint64
	Uintptr
	Float32
	Float64
	Complex64
	Complex128

	Ptr
	UnsafePtr
	String
	Slice
	Array
	Struct
	Map
	Chan
	// FuncKind rather than Func, because Func is the function being compiled.
	// The collision is resolved here rather than there: a kind is named a
	// handful of times and a function is named everywhere.
	FuncKind
	Interface

	// Void is the type of an expression with no value, and of a function with
	// no results. It has size zero and no pointers.
	Void

	// Tuple is the type of a multi-value expression: a call with several
	// results, or a two-value map read, type assertion or channel receive.
	//
	// It is not a Go type and no variable has one. It exists because such an
	// expression still needs a type at this boundary, and the alternative is a
	// nil type that every consumer must special-case.
	//
	// Its layout is a struct's and is deliberately NOT the result layout of
	// specs/030-abi.md. Results are assigned to registers and the stack by the
	// calling convention, not by struct packing, and reading a tuple's field
	// offsets as an ABI answer would be wrong. Fields carry the components.
	Tuple
)

var kindNames = [...]string{
	Invalid:    "invalid",
	Bool:       "bool",
	Int8:       "int8",
	Int16:      "int16",
	Int32:      "int32",
	Int64:      "int64",
	Uint8:      "uint8",
	Uint16:     "uint16",
	Uint32:     "uint32",
	Uint64:     "uint64",
	Uintptr:    "uintptr",
	Float32:    "float32",
	Float64:    "float64",
	Complex64:  "complex64",
	Complex128: "complex128",
	Ptr:        "ptr",
	UnsafePtr:  "unsafe.Pointer",
	String:     "string",
	Slice:      "slice",
	Array:      "array",
	Struct:     "struct",
	Map:        "map",
	Chan:       "chan",
	FuncKind:   "func",
	Interface:  "interface",
	Void:       "void",
	Tuple:      "tuple",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return "kind(?)"
}

// IsInteger reports whether k is an integer kind.
func (k Kind) IsInteger() bool { return k >= Int8 && k <= Uintptr }

// IsFloat reports whether k is a floating-point kind.
func (k Kind) IsFloat() bool { return k == Float32 || k == Float64 }

// IsComplex reports whether k is a complex kind.
func (k Kind) IsComplex() bool { return k == Complex64 || k == Complex128 }

// IsSigned reports whether k is a signed integer kind.
func (k Kind) IsSigned() bool { return k >= Int8 && k <= Int64 }

// Field is one field of a struct type.
type Field struct {
	Name   string
	Type   *Type
	Offset int64 // byte offset from the start of the struct, set by Layout
}

// Type is a type as everything below the IR sees it.
//
// Size, Align and PtrBits are computed once, by Layout, and are not recomputed
// anywhere else. Recomputing them would give two answers to one question, and
// the answer the garbage collector reads has to be the same one the code
// generator used.
type Type struct {
	Kind Kind

	Size  int64 // bytes
	Align int64 // bytes

	// PtrBits holds one bit per pointer-sized word of the type, set when the
	// word may hold a pointer. Bit i covers the word at offset i*PtrSize.
	//
	// This is the field the backend actually needs and the one most easily got
	// wrong. It is also the source of a type descriptor's GCData, which is
	// derived from it rather than computed again.
	PtrBits []byte

	Elem   *Type   // pointer, slice, array, chan, map value
	Key    *Type   // map key
	Fields []Field // struct
	Len    int64   // array length

	// EmptyIface distinguishes an interface with no methods from one with
	// methods, for Kind == Interface only.
	//
	// This looks like a method set fact and is not one, which is why it is
	// here rather than excluded by the rule above. Both kinds of interface are
	// two pointer words, but the *meaning of the first word differs*: an empty
	// interface holds a *_type, and a non-empty one holds an *itab. Equality
	// therefore calls runtime.efaceeq for one and runtime.ifaceeq for the
	// other, and calling the wrong one makes the runtime read a function
	// pointer at the wrong offset and jump through it.
	//
	// So this is which of two layouts the first word has. That is as
	// machine-relevant as the pointer map, and a backend that cannot see it
	// cannot generate a correct comparison. It was added when the SSA builder
	// reached for the distinction, found nothing, and stopped rather than
	// guessing.
	EmptyIface bool

	// Name is carried for diagnostics only. Nothing below the IR may branch on
	// it, which is the rule this comment exists to state.
	Name string
}

// PtrSize is the size of a pointer on the targets in this deck.
//
// Both targets of specs/000-decisions.md decision 9 are 64 bit. A 32-bit target
// would make this a property of the target rather than a constant, and every
// use of it here would have to be revisited, which is why it is named rather
// than written as 8.
const PtrSize = 8

// scalarLayout gives the size and alignment of every kind with a fixed one.
//
// The table is specs/030-abi.md's, and it is a table rather than a switch so
// that a missing entry is visible as a zero rather than as a fallthrough.
var scalarLayout = map[Kind]struct{ size, align int64 }{
	Bool:       {1, 1},
	Int8:       {1, 1},
	Uint8:      {1, 1},
	Int16:      {2, 2},
	Uint16:     {2, 2},
	Int32:      {4, 4},
	Uint32:     {4, 4},
	Float32:    {4, 4},
	Int64:      {8, 8},
	Uint64:     {8, 8},
	Uintptr:    {8, 8},
	Float64:    {8, 8},
	Complex64:  {8, 4},
	Complex128: {16, 8},
	Ptr:        {PtrSize, PtrSize},
	UnsafePtr:  {PtrSize, PtrSize},
	Map:        {PtrSize, PtrSize},
	Chan:       {PtrSize, PtrSize},
	FuncKind:   {PtrSize, PtrSize},
	String:     {2 * PtrSize, PtrSize},
	Slice:      {3 * PtrSize, PtrSize},
	Interface:  {2 * PtrSize, PtrSize},
	Void:       {0, 1},
}

// Layout computes Size, Align, PtrBits, and every struct field's Offset, for t
// and for every type reachable from it.
//
// It is idempotent. A type whose layout is already computed is returned
// unchanged, which is what makes it safe to call on a shared type graph.
//
// # Two walks, because a pointer is not containment
//
// A type contained in another by value has to be laid out before the one that
// contains it, and cannot contain that one in turn: the language forbids it
// and layout's cycle check reports it. A type behind a pointer is the
// opposite. It is not contained, it does not affect the containing type's size
// or alignment, and it may well be the containing type: type T struct{ next
// *T } is an ordinary declaration. So the pointee cannot take part in the
// containment recursion and is laid out afterwards, from a queue.
//
// This is the invariant the two walks establish, and every reader of a
// composite type depends on it:
//
//	if t.Align != 0 then every type reachable from t has Align != 0
//
// It did not hold before. Layout of a slice, a pointer, a map or a channel
// came out of the scalar table and stopped there, so the element type of
// []*int had size zero unless something else happened to convert *int on its
// own. Three consumers read that zero: this package's lowering multiplies an
// index by the element size to make a slice expression's data pointer,
// specs/031-runtime-lowering.md chooses memclrNoHeapPointers or
// memclrHasPointers by whether the element holds a pointer, and
// specs/021-ssa-construction.md's indexAddr puts the element size in the
// stride of every OpPtrIndex. A zero there is an index that does not move, a
// clear that leaves pointers where the collector reads them, and neither is
// visible to the verifier. The doc comment above has always said "and for
// every type reachable from it"; the code did not.
func Layout(t *Type) error {
	var pending []*Type
	if err := layout(t, make(map[*Type]bool), &pending); err != nil {
		return err
	}
	// The queue is appended to while it is drained, because laying out a
	// pointee finds the next pointer. A type already laid out is skipped
	// rather than revisited, which is what the invariant above buys: its own
	// pointees were queued when it was laid out.
	for i := 0; i < len(pending); i++ {
		cur := pending[i]
		if cur == nil || cur.Align != 0 {
			continue
		}
		if err := layout(cur, make(map[*Type]bool), &pending); err != nil {
			return err
		}
	}
	return nil
}

func layout(t *Type, inProgress map[*Type]bool, pending *[]*Type) error {
	if t == nil {
		return fmt.Errorf("ir: layout of a nil type")
	}
	if t.Align != 0 {
		// Already laid out. Align is the marker rather than Size, because a
		// struct with no fields has size zero and alignment one.
		return nil
	}
	if inProgress[t] {
		// A type cannot contain itself by value. The language forbids it and
		// the type checker rejects it, so reaching here means a malformed
		// graph rather than a malformed program.
		return fmt.Errorf("ir: type %s contains itself", t)
	}
	inProgress[t] = true
	defer delete(inProgress, t)

	if s, ok := scalarLayout[t.Kind]; ok {
		t.Size, t.Align = s.size, s.align
		t.PtrBits = scalarPtrBits(t.Kind)
		// A pointer, a slice, a map and a channel are one word here and name
		// a type that is not one. That type is queued rather than recursed
		// into, for the reason Layout gives.
		*pending = append(*pending, t.Elem, t.Key)
		return nil
	}

	switch t.Kind {
	case Array:
		if t.Elem == nil {
			return fmt.Errorf("ir: array with no element type")
		}
		if t.Len < 0 {
			return fmt.Errorf("ir: array with length %d", t.Len)
		}
		if err := layout(t.Elem, inProgress, pending); err != nil {
			return err
		}
		t.Align = t.Elem.Align
		t.Size = t.Elem.Size * t.Len
		t.PtrBits = arrayPtrBits(t.Elem, t.Len)
		return nil

	case Struct, Tuple:
		return layoutStruct(t, inProgress, pending)
	}

	return fmt.Errorf("ir: cannot lay out kind %s", t.Kind)
}

func layoutStruct(t *Type, inProgress map[*Type]bool, pending *[]*Type) error {
	var off, align int64 = 0, 1
	for i := range t.Fields {
		f := &t.Fields[i]
		if f.Type == nil {
			return fmt.Errorf("ir: struct field %s has no type", f.Name)
		}
		if err := layout(f.Type, inProgress, pending); err != nil {
			return err
		}
		off = roundUp(off, f.Type.Align)
		f.Offset = off
		off += f.Type.Size
		if f.Type.Align > align {
			align = f.Type.Align
		}
	}

	// A struct that ends in a zero-size field gets a byte of padding, so that
	// taking the address of that field cannot produce a pointer one past the
	// end of the allocation. specs/030-abi.md states the rule; the runtime
	// depends on it, because such a pointer could otherwise be attributed to
	// the next object in the heap.
	//
	// The padding applies only when something precedes the field. A struct
	// whose fields are all zero-size is itself zero-size, checked directly:
	//
	//	struct{A struct{}}           0
	//	struct{A, B struct{}}        0
	//	struct{A int8; B struct{}}   2
	//	struct{A int64; B struct{}}  16
	//	struct{A struct{}; B int64}  8
	//
	// An earlier version padded whenever the last field was zero-size, which
	// gave the first two a size of 1.
	if n := len(t.Fields); n > 0 && off > 0 && t.Fields[n-1].Type.Size == 0 {
		off++
	}

	t.Align = align
	t.Size = roundUp(off, align)
	t.PtrBits = structPtrBits(t)
	return nil
}

func roundUp(n, align int64) int64 {
	if align <= 1 {
		return n
	}
	return (n + align - 1) / align * align
}

// scalarPtrBits returns the pointer map of a scalar or built-in kind.
func scalarPtrBits(k Kind) []byte {
	switch k {
	case Ptr, UnsafePtr, Map, Chan, FuncKind:
		return setBits(nil, 0)
	case String:
		// The data pointer, then the length.
		return setBits(nil, 0)
	case Slice:
		// The data pointer, then the length and the capacity.
		return setBits(nil, 0)
	case Interface:
		// The type or itab word, then the data word. Both are pointers.
		return setBits(setBits(nil, 0), 1)
	}
	return nil
}

func arrayPtrBits(elem *Type, n int64) []byte {
	if len(elem.PtrBits) == 0 || n == 0 {
		return nil
	}
	var bits []byte
	stride := elem.Size / PtrSize
	for i := int64(0); i < n; i++ {
		for w := int64(0); w < stride; w++ {
			if bitSet(elem.PtrBits, w) {
				bits = setBits(bits, i*stride+w)
			}
		}
	}
	return bits
}

func structPtrBits(t *Type) []byte {
	var bits []byte
	for _, f := range t.Fields {
		if len(f.Type.PtrBits) == 0 {
			continue
		}
		base := f.Offset / PtrSize
		for w := int64(0); w*PtrSize < f.Type.Size; w++ {
			if bitSet(f.Type.PtrBits, w) {
				bits = setBits(bits, base+w)
			}
		}
	}
	return bits
}

func setBits(b []byte, i int64) []byte {
	for int64(len(b))*8 <= i {
		b = append(b, 0)
	}
	b[i/8] |= 1 << uint(i%8)
	return b
}

func bitSet(b []byte, i int64) bool {
	if i < 0 || i/8 >= int64(len(b)) {
		return false
	}
	return b[i/8]&(1<<uint(i%8)) != 0
}

// HasPointers reports whether any word of t may hold a pointer.
func (t *Type) HasPointers() bool {
	for _, x := range t.PtrBits {
		if x != 0 {
			return true
		}
	}
	return false
}

// PtrBytes is the prefix length in bytes that can contain pointers.
//
// This is a prefix length, not a size, and a type descriptor's PtrBytes field
// takes its value from here. Too small and the collector misses pointers; too
// large and it reads other bytes as pointers. See
// specs/032-type-descriptors-and-itabs.md.
func (t *Type) PtrBytes() int64 {
	last := int64(-1)
	for i := int64(0); i < int64(len(t.PtrBits))*8; i++ {
		if bitSet(t.PtrBits, i) {
			last = i
		}
	}
	if last < 0 {
		return 0
	}
	return (last + 1) * PtrSize
}

func (t *Type) String() string { return t.string(0) }

// maxTypeDepth bounds the printer.
//
// A well-formed type graph is acyclic through value fields, but a malformed one
// is exactly what an error message is trying to describe, and a printer that
// recurses forever turns a diagnosable bug into a stack overflow. The limit is
// generous enough that no real type reaches it.
const maxTypeDepth = 64

func (t *Type) string(depth int) string {
	if t == nil {
		return "<nil>"
	}
	if depth > maxTypeDepth {
		return "..."
	}
	if t.Name != "" {
		return t.Name
	}
	switch t.Kind {
	case Ptr:
		return "*" + t.Elem.string(depth+1)
	case Slice:
		return "[]" + t.Elem.string(depth+1)
	case Array:
		return fmt.Sprintf("[%d]%s", t.Len, t.Elem.string(depth+1))
	case Map:
		return fmt.Sprintf("map[%s]%s", t.Key.string(depth+1), t.Elem.string(depth+1))
	case Chan:
		return "chan " + t.Elem.string(depth+1)
	case Tuple:
		var b strings.Builder
		b.WriteString("(")
		for i, f := range t.Fields {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(f.Type.string(depth + 1))
		}
		b.WriteString(")")
		return b.String()
	case Struct:
		var b strings.Builder
		b.WriteString("struct{")
		for i, f := range t.Fields {
			if i > 0 {
				b.WriteString("; ")
			}
			if f.Name != "" {
				b.WriteString(f.Name)
				b.WriteString(" ")
			}
			b.WriteString(f.Type.string(depth + 1))
		}
		b.WriteString("}")
		return b.String()
	}
	return t.Kind.String()
}
