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
// drawn: below the IR, a type that generates code is a size, an alignment and a
// pointer map. Not a name, not a method set, not an underlying type.
//
// The boundary has a second half, and the rule above used to be stated without
// it. A type descriptor is data the runtime and reflect read, and it carries
// facts no instruction depends on: the type's name, its package, its method
// set, a struct field's tag. Those facts have to cross the boundary or nobody
// below it can write the descriptor, which is what
// specs/032-type-descriptors-and-itabs.md found when every defined type in the
// standard library was refused. So Type carries them in one group, marked as
// such, and the rule is now two rules:
//
//   - Nothing that generates code may branch on a descriptor field. Two types
//     with the same kind, size and pointer map compile to the same
//     instructions however their names or method sets differ.
//   - Nothing that writes a descriptor may guess a descriptor field. A field
//     the checker did not supply is refused by name, never filled in.
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

// ChanDir is the direction of a channel type.
//
// The values are internal/abi.ChanDir's, and they are its numbers rather than
// numbers of nanogo's own because a descriptor carries the value directly and
// reflect compares it against the same constants. A direction is two bits, one
// per operation the channel permits, so a bidirectional channel is the two
// together.
type ChanDir uint8

const (
	// InvalidDir is the zero value and is not a direction. A channel type that
	// carries it was built below the type boundary and never converted, and
	// the name and the descriptor both refuse it rather than guessing.
	InvalidDir ChanDir = 0
	// RecvOnly is <-chan T.
	RecvOnly ChanDir = 1
	// SendOnly is chan<- T.
	SendOnly ChanDir = 2
	// SendRecv is chan T, which permits both operations.
	SendRecv ChanDir = RecvOnly | SendOnly
)

var chanDirNames = [...]string{
	InvalidDir: "chandir(invalid)",
	RecvOnly:   "<-chan",
	SendOnly:   "chan<-",
	SendRecv:   "chan",
}

// String returns the spelling that precedes the element type, so that
// ChanDir.String()+" "+elem is the type's own spelling.
func (d ChanDir) String() string {
	if int(d) < len(chanDirNames) && chanDirNames[d] != "" {
		return chanDirNames[d]
	}
	return "chandir(?)"
}

// Field is one field of a struct type.
type Field struct {
	Name   string
	Type   *Type
	Offset int64 // byte offset from the start of the struct, set by Layout

	// The descriptor fields, by the rule at the top of this file.

	// Tag is the field's struct tag, without the back quotes, and is empty
	// when the field has none. Two struct types that differ only in a tag are
	// different types, so a descriptor that dropped it would name two types
	// alike.
	Tag string

	// Embedded reports whether the field was declared without a name. The
	// descriptor carries the bit because reflect reports it and because a
	// struct type's spelling is the field's type alone when it is set.
	Embedded bool

	// Pkg is the import path that qualifies Name when Name is unexported, and
	// is empty for an exported name. A struct descriptor points at the path of
	// its first unexported field, which is how reflect reaches a field name
	// from outside the package that declared it.
	Pkg string
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

	// The descriptor fields. Everything from here down is read by the type
	// descriptor writer of specs/032-type-descriptors-and-itabs.md and by
	// diagnostics, and by nothing that generates an instruction. The rule at
	// the top of this file states both halves of that.

	// Name is the type's name, qualified by its import path for a defined
	// type: "internal/goarch.ArchFamilyType", "int", "unsafe.Pointer". It is
	// empty for a type literal.
	Name string

	// Gen is the scope-disambiguation number of a type declared inside a
	// function, and is zero for every type declared at package scope.
	//
	// Two functions of one package may each declare a type called T, and the
	// two are different types. Name holds "main.T" for both, because that is
	// what reflect.Type.String reports for either, so the name alone does not
	// identify a type and the linker deduplicates by name. gc gives each
	// function-scoped declaration a number, counted over the package in
	// declaration order, and writes the symbol as type:main.T·1. This is that
	// number, and TypeLinkString is where it is spelled.
	//
	// The number does not reach a descriptor's own name field, and reflect
	// never prints it: gc prints main.T for both types, which is a program
	// answering a question about two types with one string, and a compiler
	// naming two types with one symbol is a program reading one type's layout
	// out of the other's values.
	Gen int

	// PkgPath is the import path of the package that declared the type, and is
	// empty for a predeclared type and for a type literal.
	//
	// It is carried rather than cut out of Name, because a descriptor points
	// at the path on its own (an UncommonType's PkgPath, a struct's PkgPath)
	// and because a generic instantiation's name holds the dots of its type
	// arguments, so the last dot of a name is not reliably the one that
	// separates the path from the identifier.
	PkgPath string

	// Basic is the predeclared type that is this type's underlying type, when
	// it has one: "int" for both int and type ArchFamilyType int, "" for a
	// struct.
	//
	// It exists because internal/abi.Kind distinguishes int from int64 and
	// uint from uint64, and Kind above does not: both targets are 64 bit, so
	// the two are one machine type and the IR deliberately gives them one
	// kind. The descriptor's Kind_ byte still has to say which, and a guess
	// there makes reflect report Int64 for an int.
	Basic string

	// Params and Results are a function type's parameter and result types, in
	// declaration order, for Kind == FuncKind.
	//
	// They are descriptor fields by the rule above and nothing that generates
	// code reads them: a function value is one pointer-sized word whatever it
	// is a function of, and specs/030-abi.md's calling convention is applied
	// to a call's own operands rather than to a type. What needs them is a
	// FuncType descriptor, whose tail is an array of *Type with one entry per
	// parameter and one per result, and a method descriptor's Mtyp, which is a
	// TypeOff to exactly this type.
	//
	// A nil slice is not "no parameters" in general. It is the empty list only
	// when the type came from Converter, which sets both on every function
	// type. A type built below the boundary by hand carries neither, and the
	// descriptor writer refuses it rather than writing a FuncType claiming
	// func().
	Params  []*Type
	Results []*Type

	// Variadic reports whether the last parameter is a ... parameter.
	//
	// The parameter's own type is already the slice type, so this is not
	// recoverable from Params: func(...int) and func([]int) have the same
	// Params and are different types. internal/abi.FuncType puts the bit in
	// the top bit of OutCount, and reflect reads it.
	Variadic bool

	// ChanDir is a channel's direction, for Kind == Chan.
	//
	// A descriptor field and not a machine one: chan int, chan<- int and
	// <-chan int are one word each, hold the same hchan, and send and receive
	// compile to the same calls. What the direction decides is the type's
	// identity. chan int and chan<- int are different types with different
	// descriptors, and a name computed without the direction is one symbol for
	// two types, which is the deduplication failure specs/032 records.
	//
	// The zero value is InvalidDir and not chan T, by type.go's second rule: a
	// field the checker did not supply is refused by name and never filled in.
	// Defaulting to bidirectional would name <-chan int as chan int, and the
	// linker deduplicates by name, so the program would read one channel
	// type's descriptor for the other's values. Converter sets the field on
	// every channel it converts.
	ChanDir ChanDir

	// Instantiated reports whether the type is an instantiation of a generic
	// type.
	//
	// Name drops the type arguments: atomic.Pointer[int] and
	// atomic.Pointer[string] both come out as sync/atomic.Pointer, and nothing
	// else in an ir.Type tells the two apart. gc spells the arguments, so a
	// descriptor written under the shortened name would be one symbol for two
	// different types, and the linker would merge them.
	// specs/032-type-descriptors-and-itabs.md records the case as the one that
	// neither the name nor the encoder refused. The naming function refuses it
	// now, and this is the field it reads.
	Instantiated bool

	// MapGroup is the map whose slot group this struct is, for a group and for
	// nothing else.
	//
	// The group is a struct the compiler synthesises, one per map type. The
	// runtime's map is a swiss table of groups and the collector scans one, so
	// a type has to exist to carry its pointer map, and no Go source declares
	// it. rtype.GroupOf builds it and this field is what it was built from.
	//
	// It is the map and not the slot because gc's spelling reads the *map's*
	// key and element: map[[200]byte]int is named map.group[[200]uint8]int
	// even though the slot holds a pointer to the key, because a key past
	// internal/abi.MapMaxKeyBytes is stored indirectly. A name built from the
	// slot would be a second symbol for a type gc has already named.
	MapGroup *Type

	// NoAlg reports whether the type is one the compiler synthesised and gave
	// no hash and no equality function.
	//
	// gc puts such a type's descriptor under a symbol prefixed "noalg." so
	// that it cannot merge with the descriptor of a type of the same shape
	// that a program declares and compares. The prefix is on the *symbol* and
	// not on the link string: the type hash is computed over the link string,
	// and a hash that carried the prefix would not be the hash gc computed for
	// the same type, so a type switch would miss.
	//
	// The mark reaches a type built out of a marked one, which is what
	// TypeSymbol reproduces. It is set here rather than derived, because
	// "synthesised by the compiler" is not a property of a type's shape: the
	// slot group of map[string]int and struct{ key string; elem int } have the
	// same fields and gc names them apart.
	NoAlg bool

	// Methods is the method set of the *pointer* to this type, in MethodOrder.
	//
	// MethodOrder is gc's types.CompareSyms and not byte order by name: every
	// exported method comes first. A descriptor's UncommonType carries the
	// length of that exported prefix and reflect reads the method set out of
	// it, so the order is part of what the field means rather than a detail of
	// how it was built.
	//
	// The pointer's set, because it is the larger of the two: a method with a
	// value receiver is in both sets and one with a pointer receiver is in
	// this one only. Method.PtrOnly says which, so the value type's set is
	// this one with the PtrOnly entries dropped.
	//
	// A nil slice on a type that could have methods is not "no methods". It is
	// only meaningful on a defined type, which is the only kind of type the
	// language lets a method be declared on, and Converter sets it for every
	// one of those, empty set included. That is what makes an empty method set
	// knowable, which is what a descriptor with no UncommonType methods
	// claims.
	//
	// An interface is the one type with no declared methods that carries a set
	// anyway, and it goes somewhere else in the descriptor. An interface's
	// methods are its own rather than declared on it, and they are written as
	// the InterfaceType header's Imethod array rather than as an
	// UncommonType's, so a descriptor writer reads this field from an
	// interface for that section and not for the tail. Converter sets it for a
	// defined interface and for a literal one alike, and PtrOnly is false on
	// every entry, because an interface has no receiver form to promote.
	Methods []Method
}

// Method is one method of a defined type's method set.
type Method struct {
	// Name is the method's name, unqualified.
	Name string

	// Sig is the method's type with the receiver removed, as a Type of kind
	// FuncKind.
	//
	// With the receiver removed, because that is the type a descriptor's Mtyp
	// points at and the type reflect.Method.Type reports for a method reached
	// through reflect.Type.Method. types2 keeps the receiver apart from the
	// parameters, so this is the signature's own parameter list and no
	// stripping happens here.
	//
	// A nil Sig is a gap and is refused. Zero is a legal Mtyp that means "this
	// method is unexported and reflect may not call it", so a zero written for
	// a missing signature would be read as a fact rather than as an absence.
	Sig *Type

	// Pkg is the import path that qualifies Name when Name is unexported, and
	// is empty for an exported name. Two packages may declare an unexported
	// method of the same name and they are different methods.
	Pkg string

	// PtrOnly reports whether the method is in the method set of the pointer
	// and not in the method set of the type itself.
	PtrOnly bool
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

	// A function's parameters and results, and a method's signature, are types
	// reachable from t and contained in nothing. A func value is one word
	// whatever it is a function of, and a method is not part of its type's
	// layout at all, so neither can take part in the containment recursion and
	// both are queued for the reason Layout gives. The descriptor writer still
	// needs their size and pointer map: a FuncType's tail names each one and a
	// method's Mtyp names its signature.
	for i := range t.Methods {
		*pending = append(*pending, t.Methods[i].Sig)
	}
	*pending = append(*pending, t.Params...)
	*pending = append(*pending, t.Results...)

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
