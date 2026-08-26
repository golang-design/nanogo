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

// The layout of internal/abi.ChanType after the common Type, on a 64-bit
// target.
//
//	type ChanType struct {
//	    Type
//	    Elem *Type
//	    Dir  ChanDir
//	}
//
// ChanDir is an int, so the direction occupies a whole word rather than a
// byte. gc writes it with WriteInt for the same reason.
const (
	chanOffElem  = 0
	chanOffDir   = 8
	chanTailSize = 16
)

// chanTail returns the ChanType header that follows internal/abi.Type.
//
// The direction is refused rather than defaulted when the IR does not carry
// one. chan int and chan<- int are different types with different descriptors,
// and the linker deduplicates by name, so a descriptor written under the wrong
// direction is one symbol standing for two types.
func chanTail(t *ir.Type) ([]byte, []Reloc, []Symbol, error) {
	if err := chanEmittable(t); err != nil {
		return nil, nil, nil, err
	}
	elem, err := ir.TypeSymbol(t.Elem)
	if err != nil {
		return nil, nil, nil, err
	}
	data := make([]byte, chanTailSize)
	binary.LittleEndian.PutUint64(data[chanOffDir:], uint64(t.ChanDir))
	return data, []Reloc{{
		Off: int32(TypeSize + chanOffElem), Size: 8, Type: obj.R_ADDR, Target: elem,
	}}, nil, nil
}

// chanEmittable reports the reason a channel's descriptor cannot be filled in.
func chanEmittable(t *ir.Type) error {
	switch t.ChanDir {
	case ir.SendRecv, ir.SendOnly, ir.RecvOnly:
	default:
		return fmt.Errorf("rtype: %s carries no channel direction, so its descriptor would name a different type", t)
	}
	if t.Elem == nil {
		return fmt.Errorf("rtype: %s has no element type", t)
	}
	return nil
}
