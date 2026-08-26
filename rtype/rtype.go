// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package rtype encodes type descriptors.
//
// specs/032-type-descriptors-and-itabs.md owns the layout, and the layout is
// internal/abi's exactly, by specs/000-decisions.md decision 11. The runtime
// this compiler links against is gc's own, so a descriptor that differs from
// gc's by one byte is read by gc's code with gc's field offsets.
//
// # Why this is a package of its own
//
// The canonical name of a type is in package ir, because the lowering pass
// needs it and ir may not import obj. The bytes are here, because they are an
// output format and package ir is a representation. The two halves call one
// naming function between them, which is what specs/032 means by "one naming
// function, in one package, used by everything". The encoders never build a
// name.
//
// # What is emitted and what is refused
//
// The set this package can encode is decided by one question: is the type's
// method set knowably empty? A descriptor carries an UncommonType tail
// whenever the type has methods, and an ir.Type carries no method set
// (type.go's rule: below the IR a type is a size, an alignment and a pointer
// map). So a descriptor for a type that might have methods would claim it has
// none, reflect would report an empty method set, and an itab built against it
// would find no functions.
//
// A method set is knowably empty for a predeclared basic type, because the
// language gives those none, and for a slice, an array, a map, a channel, a
// function, a literal struct and a literal interface, because the language
// gives none of those a method either. It is not knowable for a defined type
// or for a pointer to one, and those are refused. That is the same gap that
// stops itabs, stated from the other side, and it is a gap in the IR type
// boundary rather than in this package.
//
// Four other refusals are recorded where they arise: a struct's field tags, a
// channel's direction, a function's signature and an interface's method set
// are all absent from an ir.Type, so a descriptor for one of those composite
// forms cannot be filled in either.
//
// # One known divergence from gc, in the pointer bitmask of an interface
//
// ir.scalarPtrBits marks both words of an interface as pointers.
// cmd/compile/internal/typebits marks only the second: an itab lives in
// persistentalloc space and a compile-time _type lives in the read-only
// section, so the first word keeps nothing alive and gc leaves its bit clear.
//
// specs/032 requires GCData to come from ir.Type.PtrBits and not to be
// recomputed, so this package emits the wider mask rather than correcting it
// here. Correcting it here would be the second computation the requirement
// exists to forbid, and the wider mask is safe: the extra word points outside
// the heap, so the collector traces nothing through it. The cost is that a
// descriptor nanogo emits for a type holding an interface names a different
// runtime.gcbits symbol from the one gc emits for the same type, so the two
// do not merge. The fix belongs in ir.scalarPtrBits and it moves the stack
// maps as well, which is why it is recorded rather than made here.
package rtype

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/rtsym"
)

// A Reloc is one edit the linker makes to a descriptor's bytes.
//
// The target is a name rather than an index pair, because a package index is
// assigned by the object writer and this package does not own an object. A
// caller resolves the name when it adds the symbol.
type Reloc struct {
	Off    int32
	Size   uint8
	Type   obj.RelocType
	Add    int64
	Target string
}

// A Symbol is one data symbol a descriptor needs.
//
// Dupok is set on every one of them. specs/032 makes deduplication the
// linker's: a descriptor emitted by two packages is not an error, and the
// canonical name is what lets the linker merge the two.
type Symbol struct {
	Name   string
	Kind   obj.SymKind
	Align  uint32
	Dupok  bool
	Data   []byte
	Relocs []Reloc
}

// The offsets of internal/abi.Type on a 64-bit target.
//
// Written as constants rather than computed from a mirrored struct, because a
// mirrored struct would be laid out by the compiler building nanogo and this
// has to be the layout of the runtime nanogo links against. A test compares
// every one of them against what reflect reports for the same Go type, which
// is the only check that reads the target's own view.
const (
	offSize       = 0
	offPtrBytes   = 8
	offHash       = 16
	offTFlag      = 20
	offAlign      = 21
	offFieldAlign = 22
	offKind       = 23
	offEqual      = 24
	offGCData     = 32
	offStr        = 40
	offPtrToThis  = 44

	// TypeSize is the size of internal/abi.Type itself. A kind-specific tail
	// starts here.
	TypeSize = 48
)

// The TFlag bits, from internal/abi.
const (
	tflagUncommon      = 1 << 0
	tflagExtraStar     = 1 << 1
	tflagNamed         = 1 << 2
	tflagRegularMemory = 1 << 3
	tflagDirectIface   = 1 << 5
)

// maxPtrmaskBytes is internal/abi.MaxPtrmaskBytes.
//
// Beyond it gc stops emitting a bitmask and writes a symbol the runtime fills
// in on demand, which needs a BSS symbol and the runtime's cooperation. This
// package refuses such a type rather than emitting a mask the runtime would
// read with the wrong rule.
const maxPtrmaskBytes = 16

// Descriptor returns the symbols that define the type descriptor of t.
//
// The first symbol is the descriptor. The rest are the data it points at: the
// pointer bitmask, the name data, and the closure that holds the equality
// function. Descriptors of other types are referenced by name and are not
// returned, because specs/032 makes each one the responsibility of whichever
// package needs it and the linker merges the copies.
func Descriptor(t *ir.Type) ([]Symbol, error) {
	name, err := ir.TypeSymbol(t)
	if err != nil {
		return nil, err
	}
	if err := emittable(t); err != nil {
		return nil, err
	}

	// The five section markers of cmd/compile's writeType, whose comment draws
	// the same picture:
	//
	//	0 .. A  internal/abi.Type
	//	A .. B  the kind-specific header
	//	B .. C  internal/abi.UncommonType, when the type has a name or a method
	//	C .. D  the variable-length data the header points at
	//	D .. E  the method array
	//
	// The order is not free. UncommonType has to start pointer-aligned and
	// Moff is measured from B, so a header that grew after the UncommonType
	// was written would move the methods without moving the offset that finds
	// them.
	a := TypeSize
	b := a + kindTailSize(t)
	c := b
	if hasUncommon(t) {
		c = b + uncommonSize
	}
	body, bodyRelocs, bodySyms, err := kindData(t, c)
	if err != nil {
		return nil, err
	}
	d := c + len(body)

	tail, tailRelocs, tailSyms, err := kindTail(t, name, c)
	if err != nil {
		return nil, err
	}
	if len(tail) != b-a {
		return nil, fmt.Errorf("rtype: %s has a %d byte header and kindTailSize says %d", t, len(tail), b-a)
	}
	data := make([]byte, d)
	copy(data[a:], tail)
	copy(data[c:], body)

	h, err := Hash(t)
	if err != nil {
		return nil, err
	}
	kind, err := abiKind(t)
	if err != nil {
		return nil, err
	}
	flag, err := tflag(t)
	if err != nil {
		return nil, err
	}

	binary.LittleEndian.PutUint64(data[offSize:], uint64(t.Size))
	binary.LittleEndian.PutUint64(data[offPtrBytes:], uint64(t.PtrBytes()))
	binary.LittleEndian.PutUint32(data[offHash:], h)
	data[offTFlag] = flag
	// The runtime and common sense expect an alignment to be a power of two,
	// and a zero alignment would make the allocator round nothing.
	align := t.Align
	if align == 0 {
		align = 1
	}
	data[offAlign] = uint8(align)
	data[offFieldAlign] = uint8(align)
	data[offKind] = kind

	out := []Symbol{{}}
	relocs := tailRelocs
	relocs = append(relocs, bodyRelocs...)
	out = append(out, tailSyms...)
	out = append(out, bodySyms...)

	if hasUncommon(t) {
		un, unRelocs, unSyms, err := uncommonTail(t, d-c)
		if err != nil {
			return nil, err
		}
		copy(data[b:], un)
		for _, r := range unRelocs {
			r.Off += int32(b)
			relocs = append(relocs, r)
		}
		out = append(out, unSyms...)
	}

	// The pointer bitmask, from ir.Type.PtrBits and not recomputed.
	// specs/032 states the reason: two computations of one fact give two
	// answers, and the answer the collector reads has to be the one the code
	// generator used.
	bits, err := gcbits(t)
	if err != nil {
		return nil, err
	}
	out = append(out, bits)
	relocs = append(relocs, Reloc{Off: offGCData, Size: 8, Type: obj.R_ADDR, Target: bits.Name})

	// The equality function. It is a func value, so the descriptor points at a
	// one-word closure and the closure points at the code.
	eq, extra, err := equalClosure(t)
	if err != nil {
		return nil, err
	}
	if eq != "" {
		out = append(out, extra...)
		relocs = append(relocs, Reloc{Off: offEqual, Size: 8, Type: obj.R_ADDR, Target: eq})
	}

	// Str is an offset from the start of the module's type data, not a
	// pointer. specs/032 says what a pointer here costs: a binary that fails
	// at load. R_ADDROFF is the four-byte offset form the linker resolves.
	nd, err := nameData(t)
	if err != nil {
		return nil, err
	}
	out = append(out, nd)
	relocs = append(relocs, Reloc{Off: offStr, Size: 4, Type: obj.R_ADDROFF, Target: nd.Name})

	// PtrToThis is left zero. The field is documented as optional, and the
	// alternative is a weak offset relocation, which obj does not declare
	// (specs/041 stops at the arm64 group). The cost is that reflect.PointerTo
	// builds a descriptor at run time rather than finding the linked one.
	out[0] = Symbol{
		Name:   name,
		Kind:   obj.SRODATA,
		Align:  ir.PtrSize,
		Dupok:  true,
		Data:   data,
		Relocs: relocs,
	}
	return out, nil
}

// Referenced returns the types whose descriptors t's descriptor points at.
//
// A descriptor is not a leaf. A slice names its element, an array names its
// element and the slice type of that element, and a struct names the type of
// every field. cmd/link resolves each of those by name, and its DWARF pass
// walks the same edges to build a debug entry, so a package that emits a
// descriptor owes every descriptor that one reaches. gc closes the set the
// same way, in writeType, by calling itself on each edge before it writes the
// bytes that name them.
//
// The order is the order the fields are written in, so a caller that walks
// this set produces the same object on every run (specs/053-determinism.md).
func Referenced(t *ir.Type) ([]*ir.Type, error) {
	if t == nil {
		return nil, fmt.Errorf("rtype: the references of a nil type")
	}
	switch t.Kind {
	case ir.Ptr, ir.Slice:
		return []*ir.Type{t.Elem}, nil
	case ir.Array:
		st, err := SliceOf(t.Elem)
		if err != nil {
			return nil, err
		}
		return []*ir.Type{t.Elem, st}, nil
	case ir.Struct:
		out := make([]*ir.Type, 0, len(t.Fields))
		for _, f := range t.Fields {
			out = append(out, f.Type)
		}
		return out, nil
	}
	return nil, nil
}

// RuntimeOwned reports whether the runtime already defines t's descriptor.
//
// gc emits the descriptor of a predeclared type, of unsafe.Pointer, of any, of
// error and of a slice of one of those only when it is compiling package
// runtime, and every other package refers to the runtime's copy
// (cmd/compile/internal/reflectdata.writtenByWriteBasicTypes). The runtime is
// in every link, so the reference always resolves.
//
// Reproducing the rule is not an optimisation. A descriptor nanogo emits for
// one of these is a second definition of a symbol that already exists, and
// while dupok makes that legal it makes the two copies' bytes a thing that can
// differ without anyone noticing.
func RuntimeOwned(t *ir.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind == ir.Slice && t.Name == "" {
		return RuntimeOwned(t.Elem)
	}
	if t.Kind == ir.Interface && t.Name == "" && t.EmptyIface {
		// any, spelled as a type literal.
		return true
	}
	if t.Name == "" || t.PkgPath != "" {
		return false
	}
	// A name with no import path is the universe's: a predeclared type, or
	// error, whose descriptor the runtime owns as well.
	return !defined(t) || t.Name == "error"
}

// Hash returns the type hash gc computes for t.
//
// specs/032 says the hash "must match what the runtime computes". The runtime
// computes no type hash: gc computes it and the runtime compares it, so the
// requirement is that this agrees with gc. gc hashes the link string with
// cmd/internal/hash.Sum32, which is sha256 with the first byte inverted, and
// takes the first four bytes little-endian. That is reproduced here rather
// than approximated, because a type switch compares hashes before it compares
// types and two descriptors of one type with different hashes make a switch
// miss.
func Hash(t *ir.Type) (uint32, error) {
	s, err := ir.TypeLinkString(t)
	if err != nil {
		return 0, err
	}
	// cmd/internal/hash.Sum32 is sha256 with the first byte inverted, which is
	// gc's way of making this hash different from a plain sha256 of the same
	// bytes. The inversion is reproduced rather than skipped, because the
	// value ends up in the descriptor and a type switch compares it.
	sum := sha256.Sum256([]byte(s))
	sum[0] ^= 0xff
	return binary.LittleEndian.Uint32(sum[:4]), nil
}

// emittable reports the reason t's descriptor cannot be filled in.
func emittable(t *ir.Type) error {
	if t == nil {
		return fmt.Errorf("rtype: a descriptor for a nil type")
	}
	ms, err := methodSet(t)
	if err != nil {
		return err
	}
	if len(ms) > 0 {
		return methodRefusal(t, ms)
	}
	switch t.Kind {
	case ir.Chan:
		return fmt.Errorf("rtype: a channel's direction is not in the IR type")
	case ir.FuncKind:
		return fmt.Errorf("rtype: a function's signature is not in the IR type")
	case ir.Map:
		return fmt.Errorf("rtype: a map descriptor names the runtime's group type, which specs/032 does not carry")
	case ir.Interface:
		if !t.EmptyIface {
			// The method set is in the IR type now. What is not is the type of
			// each method: an InterfaceType's Imethod carries a TypeOff to the
			// descriptor of the method's signature, and a function's signature
			// does not cross the type boundary.
			return fmt.Errorf("rtype: an interface descriptor names the type of each method and a function's signature is not in the IR type")
		}
	}
	if words := t.PtrBytes() / ir.PtrSize; words > maxPtrmaskBytes*8 {
		return fmt.Errorf("rtype: %d pointer words needs the on-demand mask, which is not built", words)
	}
	return nil
}

// predeclaredKinds is the name and IR kind of every predeclared type that has
// a run-time representation.
//
// A table and not a "has no dot in the name" test. Both would answer the same
// for the standard library, and only the table answers correctly for a type
// declared at function scope, whose name a checker may leave unqualified. The
// kind is checked as well as the name, so that a defined type that shadows a
// predeclared name in some future spelling cannot pass for one.
//
// byte and rune are absent because they are aliases: ir.Converter unaliases
// before it names, so the name here is always uint8 or int32.
var predeclaredKinds = map[string]ir.Kind{
	"bool":           ir.Bool,
	"int":            ir.Int64,
	"int8":           ir.Int8,
	"int16":          ir.Int16,
	"int32":          ir.Int32,
	"int64":          ir.Int64,
	"uint":           ir.Uint64,
	"uint8":          ir.Uint8,
	"uint16":         ir.Uint16,
	"uint32":         ir.Uint32,
	"uint64":         ir.Uint64,
	"uintptr":        ir.Uintptr,
	"float32":        ir.Float32,
	"float64":        ir.Float64,
	"complex64":      ir.Complex64,
	"complex128":     ir.Complex128,
	"string":         ir.String,
	"unsafe.Pointer": ir.UnsafePtr,
}

// defined reports whether t is a type declared with a name, as opposed to a
// predeclared basic type or a type literal.
//
// The distinction decides two things: whether a method set might exist, and
// whether the name data is written as exported.
func defined(t *ir.Type) bool {
	if t == nil || t.Name == "" {
		return false
	}
	k, ok := predeclaredKinds[t.Name]
	return !ok || k != t.Kind
}

// definedIn returns the import path a defined type's descriptor is owed by.
//
// It is empty for a type this package cannot attribute, which is a type built
// below the type boundary by hand.
func definedIn(t *ir.Type) string {
	if !defined(t) {
		return ""
	}
	return t.PkgPath
}

// abiKinds maps an IR kind to internal/abi.Kind, for the kinds where one IR
// kind is one abi kind.
//
// A table and not a switch, so that a kind with no entry is a zero rather than
// a fallthrough to something plausible.
var abiKinds = map[ir.Kind]uint8{
	ir.Bool:       1,
	ir.Int8:       3,
	ir.Int16:      4,
	ir.Int32:      5,
	ir.Uint8:      8,
	ir.Uint16:     9,
	ir.Uint32:     10,
	ir.Uintptr:    12,
	ir.Float32:    13,
	ir.Float64:    14,
	ir.Complex64:  15,
	ir.Complex128: 16,
	ir.Array:      17,
	ir.Chan:       18,
	ir.FuncKind:   19,
	ir.Interface:  20,
	ir.Map:        21,
	ir.Ptr:        22,
	ir.Slice:      23,
	ir.String:     24,
	ir.Struct:     25,
	ir.UnsafePtr:  26,
}

// The two abi kinds that share one IR kind each.
const (
	abiInt    = 2
	abiInt64  = 6
	abiUint   = 7
	abiUint64 = 11
)

// abiKind returns the descriptor's Kind_ byte.
//
// int and int64 are one ir.Kind and two abi.Kinds, and so are uint and uint64,
// so the name settles which. A guess would make reflect report Int64 for an
// int. The error is unreachable through Descriptor, because a word with
// neither predeclared name has no canonical name either and is refused before
// it gets here, and it is written anyway: this is a field where a plausible
// wrong answer is silent.
func abiKind(t *ir.Type) (uint8, error) {
	// ir.Type.Basic and not ir.Type.Name. A defined type has a name of its own
	// and the underlying spelling is what decides the kind, so type
	// ArchFamilyType int is Int and not Int64. Name is the fallback for a type
	// built below the type boundary rather than converted from the checker,
	// where the predeclared spelling is the name.
	basic := t.Basic
	if basic == "" {
		basic = t.Name
	}
	switch t.Kind {
	case ir.Int64:
		switch basic {
		case "int":
			return abiInt, nil
		case "int64":
			return abiInt64, nil
		}
		return 0, fmt.Errorf("rtype: %s is int or int64 and the IR type does not say which", t)
	case ir.Uint64:
		switch basic {
		case "uint":
			return abiUint, nil
		case "uint64":
			return abiUint64, nil
		}
		return 0, fmt.Errorf("rtype: %s is uint or uint64 and the IR type does not say which", t)
	}
	if k, ok := abiKinds[t.Kind]; ok {
		return k, nil
	}
	return 0, fmt.Errorf("rtype: no abi kind for %s", t.Kind)
}

// tflag returns the descriptor's TFlag byte.
func tflag(t *ir.Type) (uint8, error) {
	var f uint8
	if t.Name != "" {
		f |= tflagNamed
	}
	// The flag and the tail are one decision. Set with no tail, the runtime
	// reads sixteen bytes past the end of the symbol; clear with a tail,
	// reflect reports no package path for a type that has one. hasUncommon is
	// what Descriptor places the section by, so it is what sets the bit.
	if hasUncommon(t) {
		f |= tflagUncommon
	}
	name, err := ir.TypeNameString(t)
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(name, "*") {
		// The name data holds "*T" and the descriptor of T points into it one
		// byte along, so that T and *T share one string.
		f |= tflagExtraStar
	}
	// Regular memory means the equality and hash functions may treat the value
	// as one region of t.Size bytes. A string, a float and an interface each
	// have a function of their own and are not regular memory, even though
	// each is comparable.
	if alg(t) == algMem {
		f |= tflagRegularMemory
	}
	// A cached computation, not a decision: the runtime stores a value of this
	// type directly in an interface's data word exactly when the value is one
	// pointer-sized word that is entirely pointer.
	if t.Size == ir.PtrSize && t.PtrBytes() == ir.PtrSize {
		f |= tflagDirectIface
	}
	return f, nil
}

// kindTailSize is the size of the kind-specific header that follows
// internal/abi.Type.
//
// It is a function of the kind alone and is answered before the header is
// built, because the UncommonType that follows the header has to be placed
// before the header can point past it. Descriptor checks the two against each
// other, so a kind added to one and not the other is a build failure rather
// than a descriptor whose sections overlap.
func kindTailSize(t *ir.Type) int {
	switch t.Kind {
	case ir.Ptr, ir.Slice:
		return 8
	case ir.Array:
		return 24
	case ir.Interface:
		return 32
	case ir.Struct:
		return structTailSize
	}
	return 0
}

// kindTail returns the kind-specific header that follows internal/abi.Type.
//
// self is the descriptor's own symbol and dataOff is the start of the
// variable-length section, for a header whose fields point into it.
func kindTail(t *ir.Type, self string, dataOff int) ([]byte, []Reloc, []Symbol, error) {
	elemRef := func(off int32, e *ir.Type) (Reloc, error) {
		name, err := ir.TypeSymbol(e)
		if err != nil {
			return Reloc{}, err
		}
		return Reloc{Off: off, Size: 8, Type: obj.R_ADDR, Target: name}, nil
	}
	switch t.Kind {
	case ir.Ptr, ir.Slice:
		r, err := elemRef(TypeSize, t.Elem)
		if err != nil {
			return nil, nil, nil, err
		}
		return make([]byte, 8), []Reloc{r}, nil, nil

	case ir.Array:
		// Elem, then the slice type of the same element, then the length.
		// reflect reads Slice for reflect.Value.Slice on an array.
		e, err := elemRef(TypeSize, t.Elem)
		if err != nil {
			return nil, nil, nil, err
		}
		st, err := SliceOf(t.Elem)
		if err != nil {
			return nil, nil, nil, err
		}
		sl, err := elemRef(TypeSize+8, st)
		if err != nil {
			return nil, nil, nil, err
		}
		tail := make([]byte, 24)
		binary.LittleEndian.PutUint64(tail[16:], uint64(t.Len))
		return tail, []Reloc{e, sl}, nil, nil

	case ir.Interface:
		// An empty interface: a nil package path and an empty method slice.
		// The three words of the slice are its pointer, length and capacity,
		// and all three are zero.
		return make([]byte, 32), nil, nil, nil

	case ir.Struct:
		return structTail(t, self, dataOff)
	}
	if noTail[t.Kind] {
		return nil, nil, nil, nil
	}
	// A map, a channel, a function and an interface with methods all have a
	// header this package does not write. Answering "no header" for one of
	// them would produce a descriptor the runtime reads past the end of.
	return nil, nil, nil, fmt.Errorf("rtype: no descriptor header for %s", t.Kind)
}

// kindData returns the variable-length section the kind-specific header points
// at, which starts at base within the descriptor.
//
// Only a struct has one today. A function's parameter array and an interface's
// method array live here too, and both are refused before they get this far.
func kindData(t *ir.Type, base int) ([]byte, []Reloc, []Symbol, error) {
	if t.Kind == ir.Struct {
		return structFields(t, base)
	}
	return nil, nil, nil, nil
}

// SliceOf returns the IR type of a slice whose element is t.
//
// An array's descriptor names the slice type of the same element, so a package
// that emits one owes the other. The type is built here rather than by the
// caller so that the two agree on the layout, which ir.Layout computes.
func SliceOf(elem *ir.Type) (*ir.Type, error) {
	st := &ir.Type{Kind: ir.Slice, Elem: elem}
	if err := ir.Layout(st); err != nil {
		return nil, err
	}
	return st, nil
}

// noTail is the kinds whose descriptor is internal/abi.Type and nothing more.
//
// Written out rather than derived from abiKinds, because a kind having an abi
// number says nothing about whether it has a tail: a struct has both.
var noTail = map[ir.Kind]bool{
	ir.Bool: true, ir.Int8: true, ir.Int16: true, ir.Int32: true, ir.Int64: true,
	ir.Uint8: true, ir.Uint16: true, ir.Uint32: true, ir.Uint64: true,
	ir.Uintptr: true, ir.Float32: true, ir.Float64: true,
	ir.Complex64: true, ir.Complex128: true,
	ir.String: true, ir.UnsafePtr: true,
}

// gcbits returns the symbol holding t's pointer bitmask.
//
// The bytes come from ir.Type.PtrBits, which ir.Layout computed once. The name
// is the content in hexadecimal, which is gc's, so a mask nanogo emits and one
// gc emits for the same shape are one symbol in the linked binary.
func gcbits(t *ir.Type) (Symbol, error) {
	words := t.PtrBytes() / ir.PtrSize
	n := (words + 7) / 8
	// The runtime reads a ptrmask as a sequence of words, so its length is
	// rounded up to a multiple of the pointer size.
	n = (n + ir.PtrSize - 1) &^ (ir.PtrSize - 1)
	mask := make([]byte, n)
	for i := int64(0); i < words; i++ {
		if bitSet(t.PtrBits, i) {
			mask[i/8] |= 1 << uint(i%8)
		}
	}
	return Symbol{
		Name:  fmt.Sprintf("runtime.gcbits.%x", mask),
		Kind:  obj.SNOPTRDATA,
		Align: ir.PtrSize,
		Dupok: true,
		Data:  mask,
	}, nil
}

// bitSet reports whether bit i of a pointer map is set.
//
// ir keeps the same predicate unexported, and duplicating three lines is
// better than exporting a bit accessor from the type boundary for one caller.
func bitSet(b []byte, i int64) bool {
	if i < 0 || i/8 >= int64(len(b)) {
		return false
	}
	return b[i/8]&(1<<uint(i%8)) != 0
}

// nameData returns the symbol holding t's name string.
//
// The content is internal/abi's Name encoding: a flag byte, the length as a
// uvarint, and the bytes. The name written is "*T" rather than "T", with
// TFlagExtraStar set, so that the descriptors of T and *T share one string,
// which is what gc does and what makes the two symbols mergeable.
func nameData(t *ir.Type) (Symbol, error) {
	name, err := ir.TypeNameString(t)
	if err != nil {
		return Symbol{}, err
	}
	exported := false
	if !strings.HasPrefix(name, "*") {
		name = "*" + name
		exported = isExported(t)
	} else {
		exported = isExported(t.Elem)
	}

	var bits byte
	if exported {
		bits |= 1 << 0
	}
	data := []byte{bits}
	data = binary.AppendUvarint(data, uint64(len(name)))
	data = append(data, name...)

	// gc's separator says whether the name is exported, so that two names
	// differing only in that do not share a symbol.
	sep := "-"
	if exported {
		sep = "."
	}
	return Symbol{
		Name:  "type:.namedata." + name + sep,
		Kind:  obj.SRODATA,
		Align: 1,
		Dupok: true,
		Data:  data,
	}, nil
}

// isExported reports whether t has a name of its own that is exported.
//
// gc asks the question of the type's symbol, which every named type has and no
// type literal does, so the test here is the same one applied to the name: the
// part after the last dot, capitalised. A type literal has no name and is
// never exported.
//
// unsafe.Pointer is the case that makes this a name test rather than a test of
// whether the type is user-defined. gc treats it as a defined type whose
// symbol is Pointer in package unsafe, so its name data is written as exported
// and its symbol is type:.namedata.*unsafe.Pointer. with a dot. Treating it as
// a predeclared type gave the symbol a trailing dash instead, which is a
// second symbol for a string that already has one.
func isExported(t *ir.Type) bool {
	if t == nil || t.Name == "" {
		return false
	}
	base := t.Name[strings.LastIndex(t.Name, ".")+1:]
	if base == "" {
		return false
	}
	c := base[0]
	return c >= 'A' && c <= 'Z'
}

// equalClosure returns the symbol the descriptor's Equal field points at, and
// the extra symbols that definition needs.
//
// An empty name means the type is not comparable, which is what a nil Equal
// says.
//
// The field holds a func value rather than a code address, so what it points
// at is always a closure symbol: one word holding the function's address, and
// for the variable-length memory comparison a second word holding the size.
func equalClosure(t *ir.Type) (string, []Symbol, error) {
	fn, varlen, err := equalFunc(t)
	if err != nil || fn == "" {
		return "", nil, err
	}
	if rtsym.Lookup(fn) == nil {
		// The name reaches the linker, so it is checked against the runtime's
		// own source rather than spelled here. specs/031 states the rule and
		// rtsym is where it is enforced.
		return "", nil, fmt.Errorf("rtype: %s is not in rtsym", fn)
	}
	if varlen {
		name := fmt.Sprintf("type:.eqfunc.M%d", t.Size)
		data := make([]byte, 2*ir.PtrSize)
		binary.LittleEndian.PutUint64(data[ir.PtrSize:], uint64(t.Size))
		return name, []Symbol{{
			Name:   name,
			Kind:   obj.SRODATA,
			Align:  ir.PtrSize,
			Dupok:  true,
			Data:   data,
			Relocs: []Reloc{{Off: 0, Size: 8, Type: obj.R_ADDR, Target: fn}},
		}}, nil
	}
	// The middle dot is gc's suffix for the func value of a function, and the
	// linker merges the copies every package emits.
	name := fn + "·f"
	return name, []Symbol{{
		Name:   name,
		Kind:   obj.SRODATA,
		Align:  ir.PtrSize,
		Dupok:  true,
		Data:   make([]byte, ir.PtrSize),
		Relocs: []Reloc{{Off: 0, Size: 8, Type: obj.R_ADDR, Target: fn}},
	}}, nil
}
