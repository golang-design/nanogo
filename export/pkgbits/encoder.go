// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkgbits

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"go/constant"
	"io"
	"math/big"
	"strings"
)

// A PkgEncoder builds a package's Unified IR export data.
//
// It is the mirror of [PkgDecoder]: the same sections, the same element
// indices, the same reference tables. See doc.go for the container's shape.
type PkgEncoder struct {
	// version is the file format version the elements are written at.
	version Version

	// elems holds the bitstream of every element written so far, by
	// section and then by index within the section.
	elems [numRelocs][]string

	// stringsIdx maps an already written string to its index in
	// SectionString, so that a string is stored once.
	//
	// It is read by key and never ranged over. SectionString's order is
	// the order strings were first written, which is fixed by the write
	// path and not by this map (specs/053-determinism.md).
	stringsIdx map[string]RelElemIdx
}

// NewPkgEncoder returns a PkgEncoder that writes at version v.
//
// nanogo diverges from upstream by taking no frame count. Upstream can write
// a sync marker before every field, which is a debugging aid for the writer
// and the reader together. nanogo writes none, for the reason
// [PkgDecoder.Sync] already records from the other side: the ported reader
// desyncs on marked data at the first object that stands in for another
// package's declaration, so data nanogo marked would be data nanogo cannot
// read.
func NewPkgEncoder(v Version) PkgEncoder {
	return PkgEncoder{
		version:    v,
		stringsIdx: make(map[string]RelElemIdx),
	}
}

// SyncMarkers reports whether the encoder writes sync markers. It never does.
func (pw *PkgEncoder) SyncMarkers() bool { return false }

// Version reports the version the elements are written at.
func (pw *PkgEncoder) Version() Version { return pw.version }

// NumElems returns the number of elements written to section k.
func (pw *PkgEncoder) NumElems(k SectionKind) int { return len(pw.elems[k]) }

// DumpTo writes the encoded package to out and returns its fingerprint.
//
// The fingerprint is the first eight bytes of the SHA-256 of everything
// before it, and it is also the last eight bytes of the payload. An importing
// object records it in its Autolib entry and the linker refuses a build whose
// two copies disagree, so the caller has to carry it into the object it
// writes.
//
// nanogo diverges from upstream by returning the write error rather than
// asserting it away. The caller is writing a file the build asked for.
func (pw *PkgEncoder) DumpTo(out0 io.Writer) (fingerprint [8]byte, err error) {
	h := sha256.New()
	out := io.MultiWriter(out0, h)

	writeUint32 := func(x uint32) {
		if err == nil {
			err = binary.Write(out, binary.LittleEndian, x)
		}
	}

	writeUint32(uint32(pw.version))

	if pw.version.Has(Flags) {
		// No flagSyncMarkers: this encoder writes no markers.
		writeUint32(0)
	}

	// The end index of each section within the element table.
	var sum uint32
	for _, elems := range &pw.elems {
		sum += uint32(len(elems))
		writeUint32(sum)
	}

	// The end offset of each element within the payload.
	sum = 0
	for _, elems := range &pw.elems {
		for _, elem := range elems {
			sum += uint32(len(elem))
			writeUint32(sum)
		}
	}

	if err != nil {
		return fingerprint, err
	}

	for _, elems := range &pw.elems {
		for _, elem := range elems {
			if _, err = io.WriteString(out, elem); err != nil {
				return fingerprint, err
			}
		}
	}

	copy(fingerprint[:], h.Sum(nil))
	if _, err = out0.Write(fingerprint[:]); err != nil {
		return fingerprint, err
	}
	return fingerprint, nil
}

// StringIdx adds s to the strings section if it is not already there, and
// returns its index.
func (pw *PkgEncoder) StringIdx(s string) RelElemIdx {
	if idx, ok := pw.stringsIdx[s]; ok {
		assert(pw.elems[SectionString][idx] == s)
		return idx
	}

	idx := RelElemIdx(len(pw.elems[SectionString]))
	pw.elems[SectionString] = append(pw.elems[SectionString], s)
	pw.stringsIdx[s] = idx
	return idx
}

// NewEncoder reserves a new element in section k and writes marker as the
// start of its bitstream.
func (pw *PkgEncoder) NewEncoder(k SectionKind, marker SyncMarker) *Encoder {
	e := pw.NewEncoderRaw(k)
	e.Sync(marker)
	return e
}

// NewEncoderRaw reserves a new element in section k.
//
// Most callers want [PkgEncoder.NewEncoder]. The index is assigned now and
// the bitstream is stored by [Encoder.Flush], so an element may reference an
// element that is not written yet, which is how a cyclic type graph is
// encoded.
func (pw *PkgEncoder) NewEncoderRaw(k SectionKind) *Encoder {
	idx := RelElemIdx(len(pw.elems[k]))
	pw.elems[k] = append(pw.elems[k], "") // placeholder

	return &Encoder{
		p:   pw,
		k:   k,
		Idx: idx,
	}
}

// An Encoder writes one element's bitstream.
type Encoder struct {
	p *PkgEncoder

	// Relocs is the element's reference table, in the order the
	// references were first made. RelocMap finds an existing entry and is
	// never ranged over.
	Relocs   []RefTableEntry
	RelocMap map[RefTableEntry]uint32
	Data     bytes.Buffer

	k   SectionKind
	Idx RelElemIdx
}

// Flush finalises the element and returns its index.
//
// The reference table is written in front of the data, because a reader
// resolves a reference by table index and must be able to read the table
// without reading the element.
func (w *Encoder) Flush() RelElemIdx {
	var sb strings.Builder

	// Hold the data aside so the reference table goes in front of it.
	var tmp bytes.Buffer
	if _, err := io.Copy(&tmp, &w.Data); err != nil {
		w.checkErr(err)
	}

	w.Len(len(w.Relocs))
	for _, rEnt := range w.Relocs {
		w.Len(int(rEnt.Kind))
		w.Len(int(rEnt.Idx))
	}

	if _, err := io.Copy(&sb, &w.Data); err != nil {
		w.checkErr(err)
	}
	if _, err := io.Copy(&sb, &tmp); err != nil {
		w.checkErr(err)
	}
	w.p.elems[w.k][w.Idx] = sb.String()

	return w.Idx
}

func (w *Encoder) checkErr(err error) {
	if err != nil {
		panicf("pkgbits: unexpected encoding error: %v", err)
	}
}

func (w *Encoder) rawUvarint(x uint64) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], x)
	_, err := w.Data.Write(buf[:n])
	w.checkErr(err)
}

func (w *Encoder) rawVarint(x int64) {
	// Zig-zag encode.
	ux := uint64(x) << 1
	if x < 0 {
		ux = ^ux
	}
	w.rawUvarint(ux)
}

func (w *Encoder) rawReloc(k SectionKind, idx RelElemIdx) int {
	e := RefTableEntry{k, idx}
	if w.RelocMap != nil {
		if i, ok := w.RelocMap[e]; ok {
			return int(i)
		}
	} else {
		w.RelocMap = make(map[RefTableEntry]uint32)
	}

	i := len(w.Relocs)
	w.RelocMap[e] = uint32(i)
	w.Relocs = append(w.Relocs, e)
	return i
}

// Sync writes a sync marker, which this encoder never does.
//
// The calls are kept so that the writer reads as the mirror of the reader
// and a later port can turn markers back on in one place.
func (w *Encoder) Sync(m SyncMarker) {}

// Bool writes b and returns it, so that a caller can branch on the value it
// just wrote.
func (w *Encoder) Bool(b bool) bool {
	w.Sync(SyncBool)
	var x byte
	if b {
		x = 1
	}
	w.checkErr(w.Data.WriteByte(x))
	return b
}

// Int64 writes an int64.
func (w *Encoder) Int64(x int64) {
	w.Sync(SyncInt64)
	w.rawVarint(x)
}

// Uint64 writes a uint64.
func (w *Encoder) Uint64(x uint64) {
	w.Sync(SyncUint64)
	w.rawUvarint(x)
}

// Len writes a non-negative int.
func (w *Encoder) Len(x int) { assert(x >= 0); w.Uint64(uint64(x)) }

// Int writes an int.
func (w *Encoder) Int(x int) { w.Int64(int64(x)) }

// Uint writes a uint.
func (w *Encoder) Uint(x uint) { w.Uint64(uint64(x)) }

// Reloc writes a reference to the element at (k, idx).
//
// Only the table index reaches the bitstream, so a reader knows the section
// from context and not from the stream.
func (w *Encoder) Reloc(k SectionKind, idx RelElemIdx) {
	w.Sync(SyncUseReloc)
	w.Len(w.rawReloc(k, idx))
}

// Code writes a Code value.
func (w *Encoder) Code(c Code) {
	w.Sync(c.Marker())
	w.Len(c.Value())
}

// String writes a string, by adding it to the strings section and writing a
// reference to it.
func (w *Encoder) String(s string) {
	w.StringRef(w.p.StringIdx(s))
}

// StringRef writes a reference to an already added string.
func (w *Encoder) StringRef(idx RelElemIdx) {
	w.Sync(SyncString)
	w.Reloc(SectionString, idx)
}

// Strings writes a length-prefixed list of strings.
func (w *Encoder) Strings(ss []string) {
	w.Len(len(ss))
	for _, s := range ss {
		w.String(s)
	}
}

// Value writes a constant.
func (w *Encoder) Value(val constant.Value) {
	w.Sync(SyncValue)
	if w.Bool(val.Kind() == constant.Complex) {
		w.scalar(constant.Real(val))
		w.scalar(constant.Imag(val))
	} else {
		w.scalar(val)
	}
}

func (w *Encoder) scalar(val constant.Value) {
	switch v := constant.Val(val).(type) {
	default:
		panicf("pkgbits: unhandled constant %v (%v)", val, val.Kind())
	case bool:
		w.Code(ValBool)
		w.Bool(v)
	case string:
		w.Code(ValString)
		w.String(v)
	case int64:
		w.Code(ValInt64)
		w.Int64(v)
	case *big.Int:
		w.Code(ValBigInt)
		w.bigInt(v)
	case *big.Rat:
		w.Code(ValBigRat)
		w.bigInt(v.Num())
		w.bigInt(v.Denom())
	case *big.Float:
		w.Code(ValBigFloat)
		w.bigFloat(v)
	}
}

func (w *Encoder) bigInt(v *big.Int) {
	b := v.Bytes()
	w.String(string(b))
	w.Bool(v.Sign() < 0)
}

func (w *Encoder) bigFloat(v *big.Float) {
	b := v.Append(nil, 'p', -1)
	w.String(string(b))
}

// Version reports the version of the bitstream being written.
func (w *Encoder) Version() Version { return w.p.version }
