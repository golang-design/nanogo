// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkgbits

// A CodeVal distinguishes among go/constant.Value encodings.
type CodeVal int

// Note: These values are public and cannot be changed without
// updating the go/types importers.

const (
	ValBool CodeVal = iota
	ValString
	ValInt64
	ValBigInt
	ValBigRat
	ValBigFloat
)

// A CodeType distinguishes among go/types.Type encodings.
type CodeType int

// Note: These values are public and cannot be changed without
// updating the go/types importers.

const (
	TypeBasic CodeType = iota
	TypeNamed
	TypePointer
	TypeSlice
	TypeArray
	TypeChan
	TypeMap
	TypeSignature
	TypeStruct
	TypeInterface
	TypeUnion
	TypeTypeParam
)

// A CodeObj distinguishes among go/types.Object encodings.
type CodeObj int

// Note: These values are public and cannot be changed without
// updating the go/types importers.

const (
	ObjAlias CodeObj = iota
	ObjConst
	ObjType
	ObjFunc
	ObjVar
	ObjStub
)

// A Code is a value written with its own sync marker, so that a reader that
// is checking markers can tell which enumeration it is decoding.
//
// nanogo's reader dropped this interface, because only the encoder calls it.
// The writer half brings it back.
type Code interface {
	// Marker returns the SyncMarker for the Code's dynamic type.
	Marker() SyncMarker
	// Value returns the Code's ordinal value.
	Value() int
}

func (c CodeVal) Marker() SyncMarker  { return SyncVal }
func (c CodeType) Marker() SyncMarker { return SyncType }
func (c CodeObj) Marker() SyncMarker  { return SyncCodeObj }

func (c CodeVal) Value() int  { return int(c) }
func (c CodeType) Value() int { return int(c) }
func (c CodeObj) Value() int  { return int(c) }
