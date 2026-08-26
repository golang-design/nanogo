// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype

import (
	"encoding/binary"
	"fmt"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
)

// The layout of internal/abi.FuncType after the common Type, on a 64-bit
// target, and of the array that follows it.
//
//	type FuncType struct {
//	    Type
//	    InCount  uint16
//	    OutCount uint16 // top bit is set if the last input is ...
//	}
//
// The two counts are four bytes and the header is eight. The four bytes of
// padding are not slack: what follows is an array of *Type, and the array is
// addressed from the end of the header, so a seven-byte header would put every
// pointer in it at an odd offset. gc rounds the same way, because its cursor
// is over a Go struct and the compiler pads it.
//
// The array holds the receiver, then the parameters, then the results, in that
// order. There is no receiver here: a method's type is written with the
// receiver removed, which is what ir.Method.Sig carries.
const (
	funcOffInCount  = 0
	funcOffOutCount = 2
	funcTailSize    = 8

	// funcParamSize is one entry of the array that follows the header.
	funcParamSize = 8

	// funcVariadic is the top bit of OutCount. internal/abi puts it there
	// rather than in TFlag because the flag belongs to the function type and
	// TFlag is a byte with no spare bits.
	funcVariadic = 1 << 15

	// funcMaxCount is what a count field holds. OutCount gives up its top bit
	// to the variadic flag, so the two limits differ by one bit.
	funcMaxInCount  = 1<<16 - 1
	funcMaxOutCount = 1<<15 - 1
)

// funcTail returns the FuncType header that follows internal/abi.Type.
func funcTail(t *ir.Type) ([]byte, []Reloc, []Symbol, error) {
	if err := funcEmittable(t); err != nil {
		return nil, nil, nil, err
	}
	data := make([]byte, funcTailSize)
	binary.LittleEndian.PutUint16(data[funcOffInCount:], uint16(len(t.Params)))
	out := uint16(len(t.Results))
	if t.Variadic {
		out |= funcVariadic
	}
	binary.LittleEndian.PutUint16(data[funcOffOutCount:], out)
	return data, nil, nil, nil
}

// funcParams returns the array of parameter and result descriptors that
// follows the header, which starts at base within the descriptor.
//
// It is one array and not two, and the split is the counts above. reflect
// reads In(i) from the first InCount entries and Out(i) from the rest, so an
// array in the wrong order reports a function's results as its parameters.
func funcParams(t *ir.Type, base int) ([]byte, []Reloc, []Symbol, error) {
	if err := funcEmittable(t); err != nil {
		return nil, nil, nil, err
	}
	all := make([]*ir.Type, 0, len(t.Params)+len(t.Results))
	all = append(all, t.Params...)
	all = append(all, t.Results...)

	data := make([]byte, len(all)*funcParamSize)
	relocs := make([]Reloc, 0, len(all))
	for i, p := range all {
		name, err := ir.TypeSymbol(p)
		if err != nil {
			return nil, nil, nil, err
		}
		relocs = append(relocs, Reloc{
			Off: int32(base + i*funcParamSize), Size: 8, Type: obj.R_ADDR, Target: name,
		})
	}
	return data, relocs, nil, nil
}

// funcEmittable reports the reason a function's descriptor cannot be filled
// in.
//
// A nil parameter or result list is the gap. ir.Converter sets both on every
// function type, the empty list included, so a nil one means the type was
// built below the type boundary and never converted. A descriptor written from
// it would claim func(), and reflect would report a function of no arguments
// for one that takes three.
func funcEmittable(t *ir.Type) error {
	if t.Params == nil || t.Results == nil {
		return fmt.Errorf("rtype: the signature of %s is not in the IR type", t)
	}
	if len(t.Params) > funcMaxInCount {
		return fmt.Errorf("rtype: %s takes %d parameters and InCount holds %d", t, len(t.Params), funcMaxInCount)
	}
	if len(t.Results) > funcMaxOutCount {
		return fmt.Errorf("rtype: %s returns %d results and OutCount holds %d", t, len(t.Results), funcMaxOutCount)
	}
	if t.Variadic && len(t.Params) == 0 {
		// The variadic bit says the last parameter is a ... parameter, and
		// there is no last parameter. reflect would read In(-1).
		return fmt.Errorf("rtype: %s is variadic and takes no parameters", t)
	}
	return nil
}
