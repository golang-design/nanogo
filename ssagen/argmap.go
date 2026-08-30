// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
)

// The argument map and the argument info of specs/047-abi-wrappers.md.
//
// An assembly file defines a text symbol and knows nothing about the Go types
// of the arguments it reads. cmd/internal/obj/plist.go closes that gap from
// the compiler's side: for every ABI0 text symbol the assembler produces under
// the package's own prefix, it appends a FUNCDATA reference to
// <symbol>.args_stackmap unless the file wrote one itself, with
// runtime.addmoduledata the single exception. The assembly object therefore
// holds an undefined reference to a symbol only the compiler can define, and
// gc/compile.go defines it for a bodyless declaration whose ABI is ABI0.
//
// The map is what the garbage collector reads when it scans the frame of the
// assembly function. A bit set over a word that is not a pointer makes the
// collector follow whatever is there. A bit clear over a live pointer makes it
// free an object something still holds. Neither shows up where it was caused,
// so the bytes below are checked against gc's own for a fixed corpus rather
// than argued about.
//
// A symbol whose ABI is not ABI0 is skipped by obj with the comment "better to
// have no stackmap than an incorrect/lying stackmap", and that is the rule
// this file follows as well: it is called for an ABI0 definition and for
// nothing else.

// ArgMaps returns the <sym>.args_stackmap and <sym>.arginfo0 symbols a
// bodyless ABI0 declaration owes.
//
// The placement is ABI0, which is [ssa.ABIWalk] with the target's register
// sets empty, so the offsets the map describes and the offsets the wrapper
// stores its arguments at come from one walk and cannot disagree.
func ArgMaps(decl *ir.Func, sym string, t *ssa.Target) (stackmap, arginfo *obj.Symbol, err error) {
	if decl == nil || decl.Type == nil {
		return nil, nil, fmt.Errorf("ssagen: the argument map of %s needs the signature, which the IR does not carry", sym)
	}
	if t == nil {
		return nil, nil, fmt.Errorf("ssagen: the argument map of %s needs a target", sym)
	}
	in, out, width, err := ssa.ABIWalk(t.ABI0Target(), decl.Type.Params, decl.Type.Results, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("ssagen: the argument map of %s: %w", sym, err)
	}
	m, err := argsStackMap(sym, in, out, width)
	if err != nil {
		return nil, nil, err
	}
	a, err := argInfo(sym, in)
	if err != nil {
		return nil, nil, err
	}
	return m, a, nil
}

// argsStackMap writes liveness.WriteFuncMap's bytes.
//
//	uint32  nbitmap        // 2 when the function has results, else 1
//	uint32  bv.N           // the area in words
//	bitvec  args           // one byte per eight words, low bit first
//	bitvec  args + results // only when nbitmap is 2, and it is cumulative
//
// The second bitmap is cumulative because gc keeps writing into the same
// bitvec: the results are added to the bits the arguments already set rather
// than replacing them. internal/cpu.getisar0 is the smallest case and is
// 02 00 00 00 01 00 00 00 00 00, which is nbitmap 2, one word, and no pointer
// in either map.
func argsStackMap(sym string, in, out []ssa.ABIValue, width int64) (*obj.Symbol, error) {
	if width%ir.PtrSize != 0 {
		return nil, fmt.Errorf("ssagen: the argument map of %s: the ABI0 area is %d bytes, which is not a whole number of words", sym, width)
	}
	words := width / ir.PtrSize
	if words < 0 || words > maxArgMapWords {
		return nil, fmt.Errorf("ssagen: the argument map of %s: the ABI0 area is %d words, and the runtime reads the map with int32 arithmetic", sym, words)
	}
	bv := make([]byte, (words+7)/8)
	for i := range in {
		if err := setArgBits(sym, bv, words, &in[i]); err != nil {
			return nil, err
		}
	}
	data := make([]byte, 0, 8+2*len(bv))
	nbitmap := 1
	if len(out) > 0 {
		nbitmap = 2
	}
	data = appendUint32(data, uint32(nbitmap))
	data = appendUint32(data, uint32(words))
	data = append(data, bv...)
	if nbitmap == 2 {
		for i := range out {
			// gc guards this with len(p.Registers) == 0, which under ABI0 is
			// true of every result. The guard is kept in the comment and not
			// in the code, because a result that took a register would have
			// no offset in the area at all and setArgBits would refuse it.
			if err := setArgBits(sym, bv, words, &out[i]); err != nil {
				return nil, err
			}
		}
		data = append(data, bv...)
	}
	return &obj.Symbol{
		Name: sym + ".args_stackmap",
		Type: obj.SRODATA,
		// Named from assembly, so the linker must keep the name resolvable
		// rather than fold the symbol into the content-addressable space.
		// gc sets AttrLinkname here for exactly that: "allow args_stackmap
		// referenced from assembly".
		Flag2: obj.SymFlagLinkname,
		Size:  uint32(len(data)),
		Data:  data,
	}, nil
}

// maxArgMapWords bounds the area the map describes.
//
// runtime.stackmapdata computes n*((nbit+7)/8) in int32 arithmetic, so a map
// past this is read at a wrapped offset. gc checks the same product in
// liveness.checkStackmapOverflow. An argument area cannot realistically reach
// it, which is why the check is here rather than trusted to be unreachable.
const maxArgMapWords = 1 << 20

// setArgBits sets one bit per pointer word of one value at its offset.
//
// It is typebits.SetNoCheck over ir.Type.PtrBits, which
// specs/027-liveness-and-stackmaps.md already computes for every type. A value
// with no pointer in it contributes nothing, and a zero-size value has no
// words at all.
func setArgBits(sym string, bv []byte, words int64, av *ssa.ABIValue) error {
	if av.Type == nil || len(av.Type.PtrBits) == 0 {
		return nil
	}
	if av.Off < 0 || av.Off%ir.PtrSize != 0 {
		return fmt.Errorf("ssagen: the argument map of %s: a value holding a pointer is at offset %d, which is not a word boundary", sym, av.Off)
	}
	base := av.Off / ir.PtrSize
	for w := int64(0); w*ir.PtrSize < av.Type.Size; w++ {
		if !ptrBitSet(av.Type.PtrBits, w) {
			continue
		}
		if base+w >= words {
			return fmt.Errorf("ssagen: the argument map of %s: a pointer word at %d is outside the %d-word area", sym, base+w, words)
		}
		bv[(base+w)/8] |= 1 << uint((base+w)%8)
	}
	return nil
}

func ptrBitSet(bits []byte, w int64) bool {
	i := w / 8
	return i >= 0 && i < int64(len(bits)) && bits[i]&(1<<uint(w%8)) != 0
}

func appendUint32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// The argument info of ssagen.EmitArgInfo.
//
// It is what runtime.printArgs reads when a traceback prints the arguments of
// a frame. The encoding is a byte stream over the arguments only: an ordinary
// value is its offset and then its size, an aggregate is bracketed, and four
// operator bytes above every ordinary offset carry the rest.
//
// A wrong stream gives a wrong traceback and never a wrong program, which is
// the one thing in this file that is not a correctness requirement. It is
// written anyway because the assembler emits a FUNCDATA reference to it for
// the wrapper and because an approximation would be a second encoding of a
// format gc already fixes.
const (
	traceArgsLimit          = 10 // no more than ten components
	traceArgsMaxDepth       = 5  // no more than five layers of nesting
	traceArgsMaxLen         = (traceArgsMaxDepth*3+2)*traceArgsLimit + 1
	traceArgsEndSeq         = 0xff
	traceArgsStartAgg       = 0xfe
	traceArgsEndAgg         = 0xfd
	traceArgsDotdotdot      = 0xfc
	traceArgsOffsetTooLarge = 0xfb
	traceArgsSpecial        = 0xf0 // above this are operators
)

// argInfo writes EmitArgInfo's bytes for the arguments of an ABI0 signature.
//
// The name carries the ABI, because gc spells it "%s.arginfo%d" with the
// function's own ABI, and this file is called for an ABI0 definition only.
func argInfo(sym string, in []ssa.ABIValue) (*obj.Symbol, error) {
	w := &argInfoWriter{}
	for i := range in {
		if !w.visit(in[i].Off, in[i].Type, 0) {
			break
		}
	}
	w.byte(traceArgsEndSeq)
	if len(w.out) > traceArgsMaxLen {
		return nil, fmt.Errorf("ssagen: the argument info of %s is %d bytes, and the runtime reads at most %d", sym, len(w.out), traceArgsMaxLen)
	}
	return &obj.Symbol{
		Name:  sym + ".arginfo0",
		Type:  obj.SRODATA,
		Align: 1,
		Size:  uint32(len(w.out)),
		Data:  w.out,
	}, nil
}

type argInfoWriter struct {
	out []byte
	n   int // components written, against traceArgsLimit
}

func (w *argInfoWriter) byte(b byte) { w.out = append(w.out, b) }

// one writes an ordinary component: its offset and then its size.
//
// An offset the byte cannot hold is written as the operator alone, with no
// size after it, which is what makes the stream self-describing: the reader
// tells the two apart by the first byte.
func (w *argInfoWriter) one(size, off int64) {
	if off >= traceArgsSpecial {
		w.byte(traceArgsOffsetTooLarge)
	} else {
		w.byte(byte(off))
		w.byte(byte(size))
	}
	w.n++
}

// visit writes t at off and reports whether the stream may continue.
func (w *argInfoWriter) visit(off int64, t *ir.Type, depth int) bool {
	if w.n >= traceArgsLimit {
		w.byte(traceArgsDotdotdot)
		return false
	}
	if t == nil {
		return true
	}
	if !argInfoAggregate(t) {
		w.one(t.Size, off)
		return true
	}
	w.byte(traceArgsStartAgg)
	depth++
	if depth >= traceArgsMaxDepth {
		w.byte(traceArgsDotdotdot)
		w.byte(traceArgsEndAgg)
		w.n++
		return true
	}
	switch {
	case t.Kind == ir.Interface || t.Kind == ir.String:
		_ = w.visit(off, argInfoWord, depth) && w.visit(off+ir.PtrSize, argInfoWord, depth)
	case t.Kind == ir.Slice:
		_ = w.visit(off, argInfoWord, depth) &&
			w.visit(off+ir.PtrSize, argInfoWord, depth) &&
			w.visit(off+2*ir.PtrSize, argInfoWord, depth)
	case t.Kind.IsComplex():
		half := argInfoFloats[t.Kind]
		_ = w.visit(off, half, depth) && w.visit(off+t.Size/2, half, depth)
	case t.Kind == ir.Array:
		if t.Len == 0 {
			w.n++ // an empty aggregate is one component
			break
		}
		for i := int64(0); i < t.Len; i++ {
			if !w.visit(off, t.Elem, depth) {
				break
			}
			off += t.Elem.Size
		}
	case t.Kind == ir.Struct:
		if len(t.Fields) == 0 {
			w.n++
			break
		}
		for _, f := range t.Fields {
			if !w.visit(off+f.Offset, f.Type, depth) {
				break
			}
		}
	}
	w.byte(traceArgsEndAgg)
	return true
}

// argInfoAggregate is gc's isAggregate, which decides which types the stream
// brackets. A map and a channel are one word and are not in it.
func argInfoAggregate(t *ir.Type) bool {
	switch t.Kind {
	case ir.Struct, ir.Array, ir.String, ir.Slice, ir.Interface:
		return true
	}
	return t.Kind.IsComplex()
}

// argInfoWord is what gc substitutes for the words of a string, an interface
// and a slice: the stream prints machine words and not the Go types they came
// out of. [uintptrType] is the same type and is a func, so this names the
// value once rather than calling it at every word.
var argInfoWord = uintptrType()

// argInfoFloats are the halves a complex is printed as. gc writes
// types.FloatForComplex, which is float32 for complex64 and float64 for
// complex128.
var argInfoFloats = map[ir.Kind]*ir.Type{
	ir.Complex64:  mustLayout(&ir.Type{Kind: ir.Float32, Name: "float32", Basic: "float32"})(),
	ir.Complex128: mustLayout(&ir.Type{Kind: ir.Float64, Name: "float64", Basic: "float64"})(),
}
