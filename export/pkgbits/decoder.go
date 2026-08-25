// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkgbits

import (
	"encoding/binary"
	"errors"
	"fmt"
	"go/constant"
	"go/token"
	"io"
	"math/big"
	"strings"
)

// A PkgDecoder provides methods for decoding a package's Unified IR
// export data.
type PkgDecoder struct {
	// version is the file format version.
	version Version

	// sync indicates whether the file uses sync markers.
	sync bool

	// pkgPath is the package path for the package to be decoded.
	//
	// TODO(mdempsky): Remove; unneeded since CL 391014.
	pkgPath string

	// elemData is the full data payload of the encoded package.
	// Elements are densely and contiguously packed together.
	//
	// The last 8 bytes of elemData are the package fingerprint.
	elemData string

	// elemEnds stores the byte-offset end positions of element
	// bitstreams within elemData.
	//
	// For example, element I's bitstream data starts at elemEnds[I-1]
	// (or 0, if I==0) and ends at elemEnds[I].
	//
	// Note: elemEnds is indexed by absolute indices, not
	// section-relative indices.
	elemEnds []uint32

	// elemEndsEnds stores the index-offset end positions of relocation
	// sections within elemEnds.
	//
	// For example, section K's end positions start at elemEndsEnds[K-1]
	// (or 0, if K==0) and end at elemEndsEnds[K].
	elemEndsEnds [numRelocs]uint32

	scratchRelocEnt []RefTableEntry
}

// PkgPath returns the package path for the package
//
// TODO(mdempsky): Remove; unneeded since CL 391014.
func (pr *PkgDecoder) PkgPath() string { return pr.pkgPath }

// NewPkgDecoder returns a PkgDecoder initialized to read the Unified
// IR export data from input. pkgPath is the package path for the
// compilation unit that produced the export data.
func NewPkgDecoder(pkgPath, input string) PkgDecoder {
	pr := PkgDecoder{
		pkgPath: pkgPath,
	}

	// TODO(mdempsky): Implement direct indexing of input string to
	// avoid copying the position information.

	r := strings.NewReader(input)

	var ver uint32
	mustRead(pkgPath, binary.Read(r, binary.LittleEndian, &ver))
	pr.version = Version(ver)

	if pr.version >= numVersions {
		panic(fmt.Errorf("cannot decode %q, export data version %d is greater than maximum supported version %d", pkgPath, pr.version, numVersions-1))
	}

	if pr.version.Has(Flags) {
		var flags uint32
		mustRead(pkgPath, binary.Read(r, binary.LittleEndian, &flags))
		pr.sync = flags&flagSyncMarkers != 0
	}

	mustRead(pkgPath, binary.Read(r, binary.LittleEndian, pr.elemEndsEnds[:]))

	pr.elemEnds = make([]uint32, pr.elemEndsEnds[len(pr.elemEndsEnds)-1])
	mustRead(pkgPath, binary.Read(r, binary.LittleEndian, pr.elemEnds[:]))

	pos, err := r.Seek(0, io.SeekCurrent)
	mustRead(pkgPath, err)

	pr.elemData = input[pos:]

	// The last 8 bytes are the fingerprint, so the elements must end exactly
	// 8 bytes before the input does. A shorter payload is a truncated file,
	// which every read below would otherwise report as an index out of range.
	const fingerprintSize = 8
	if len(pr.elemData)-fingerprintSize != int(pr.elemEnds[len(pr.elemEnds)-1]) {
		panicf("pkgbits: %q: export data is truncated: the element table ends at %d and the payload holds %d bytes",
			pkgPath, pr.elemEnds[len(pr.elemEnds)-1], len(pr.elemData)-fingerprintSize)
	}

	return pr
}

// Fingerprint returns the package fingerprint.
//
// It is the last 8 bytes of the payload. An object that imports the package
// records it in its Autolib entry, and the linker refuses a build whose two
// copies disagree, so a compiler that reads export data must be able to
// report it.
func (pr *PkgDecoder) Fingerprint() [8]byte {
	var fp [8]byte
	copy(fp[:], pr.elemData[len(pr.elemData)-8:])
	return fp
}

// NumElems returns the number of elements in section k.
func (pr *PkgDecoder) NumElems(k SectionKind) int {
	count := int(pr.elemEndsEnds[k])
	if k > 0 {
		count -= int(pr.elemEndsEnds[k-1])
	}
	return count
}

// AbsIdx returns the absolute index for the given (section, index)
// pair.
func (pr *PkgDecoder) AbsIdx(k SectionKind, idx RelElemIdx) int {
	absIdx := int(idx)
	if k > 0 {
		absIdx += int(pr.elemEndsEnds[k-1])
	}
	if absIdx >= int(pr.elemEndsEnds[k]) {
		panicf("%v:%v is out of bounds; %v", k, idx, pr.elemEndsEnds)
	}
	return absIdx
}

// DataIdx returns the raw element bitstream for the given (section,
// index) pair.
func (pr *PkgDecoder) DataIdx(k SectionKind, idx RelElemIdx) string {
	absIdx := pr.AbsIdx(k, idx)

	var start uint32
	if absIdx > 0 {
		start = pr.elemEnds[absIdx-1]
	}
	end := pr.elemEnds[absIdx]

	return pr.elemData[start:end]
}

// StringIdx returns the string value for the given string index.
func (pr *PkgDecoder) StringIdx(idx RelElemIdx) string {
	return pr.DataIdx(SectionString, idx)
}

// NewDecoder returns a Decoder for the given (section, index) pair,
// and decodes the given SyncMarker from the element bitstream.
func (pr *PkgDecoder) NewDecoder(k SectionKind, idx RelElemIdx, marker SyncMarker) Decoder {
	r := pr.NewDecoderRaw(k, idx)
	r.Sync(marker)
	return r
}

// TempDecoder returns a Decoder for the given (section, index) pair,
// and decodes the given SyncMarker from the element bitstream.
// If possible the Decoder should be RetireDecoder'd when it is no longer
// needed, this will avoid heap allocations.
func (pr *PkgDecoder) TempDecoder(k SectionKind, idx RelElemIdx, marker SyncMarker) Decoder {
	r := pr.TempDecoderRaw(k, idx)
	r.Sync(marker)
	return r
}

func (pr *PkgDecoder) RetireDecoder(d *Decoder) {
	pr.scratchRelocEnt = d.Relocs
	d.Relocs = nil
}

// NewDecoderRaw returns a Decoder for the given (section, index) pair.
//
// Most callers should use NewDecoder instead.
func (pr *PkgDecoder) NewDecoderRaw(k SectionKind, idx RelElemIdx) Decoder {
	r := Decoder{
		common: pr,
		k:      k,
		Idx:    idx,
	}

	r.Data.Reset(pr.DataIdx(k, idx))
	r.Sync(SyncRelocs)
	r.Relocs = make([]RefTableEntry, r.Len())
	for i := range r.Relocs {
		r.Sync(SyncReloc)
		r.Relocs[i] = RefTableEntry{SectionKind(r.Len()), RelElemIdx(r.Len())}
	}

	return r
}

func (pr *PkgDecoder) TempDecoderRaw(k SectionKind, idx RelElemIdx) Decoder {
	r := Decoder{
		common: pr,
		k:      k,
		Idx:    idx,
	}

	r.Data.Reset(pr.DataIdx(k, idx))
	r.Sync(SyncRelocs)
	l := r.Len()
	if cap(pr.scratchRelocEnt) >= l {
		r.Relocs = pr.scratchRelocEnt[:l]
		pr.scratchRelocEnt = nil
	} else {
		r.Relocs = make([]RefTableEntry, l)
	}
	for i := range r.Relocs {
		r.Sync(SyncReloc)
		r.Relocs[i] = RefTableEntry{SectionKind(r.Len()), RelElemIdx(r.Len())}
	}

	return r
}

// A Decoder provides methods for decoding an individual element's
// bitstream data.
type Decoder struct {
	common *PkgDecoder

	Relocs []RefTableEntry
	Data   strings.Reader

	k   SectionKind
	Idx RelElemIdx
}

func (r *Decoder) checkErr(err error) {
	if err != nil {
		// nanogo diverges from upstream by naming the package and the
		// element. Upstream reports the error alone, because gc prints it and
		// ends the process next to the file it was reading. nanogo's driver
		// holds only the package it was asked to compile, so an error that
		// named neither would leave a build with no way to tell which archive
		// is the bad one.
		panicf("pkgbits: %q: section %v, index %v: unexpected decoding error: %w",
			r.common.pkgPath, r.k, r.Idx, err)
	}
}

func (r *Decoder) rawUvarint() uint64 {
	x, err := readUvarint(&r.Data)
	r.checkErr(err)
	return x
}

// readUvarint is a type-specialized copy of encoding/binary.ReadUvarint.
// This avoids the interface conversion and thus has better escape properties,
// which flows up the stack.
func readUvarint(r *strings.Reader) (uint64, error) {
	var x uint64
	var s uint
	for i := 0; i < binary.MaxVarintLen64; i++ {
		b, err := r.ReadByte()
		if err != nil {
			if i > 0 && err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return x, err
		}
		if b < 0x80 {
			if i == binary.MaxVarintLen64-1 && b > 1 {
				return x, overflow
			}
			return x | uint64(b)<<s, nil
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return x, overflow
}

var overflow = errors.New("pkgbits: readUvarint overflows a 64-bit integer")

func (r *Decoder) rawVarint() int64 {
	ux := r.rawUvarint()

	// Zig-zag decode.
	x := int64(ux >> 1)
	if ux&1 != 0 {
		x = ^x
	}
	return x
}

func (r *Decoder) rawReloc(k SectionKind, idx int) RelElemIdx {
	e := r.Relocs[idx]
	assert(e.Kind == k)
	return e.Idx
}

// Sync decodes a sync marker from the element bitstream and asserts
// that it matches the expected marker.
//
// If EnableSync is false, then Sync is a no-op.
func (r *Decoder) Sync(mWant SyncMarker) {
	if !r.common.sync {
		return
	}

	pos, _ := r.Data.Seek(0, io.SeekCurrent)
	mHave := SyncMarker(r.rawUvarint())
	writerPCs := make([]int, r.rawUvarint())
	for i := range writerPCs {
		writerPCs[i] = int(r.rawUvarint())
	}

	if mHave == mWant {
		return
	}

	// nanogo diverges from upstream here. Upstream prints both stacks and
	// calls os.Exit, which a compiler that is also a library cannot do: the
	// driver has to report the failure against the package being compiled.
	// The writer's stack is still printed, because it is the only evidence
	// of where the two sides disagree, and the reader's stack comes with the
	// panic this raises.
	var where strings.Builder
	for _, pc := range writerPCs {
		where.WriteString("\n\t")
		where.WriteString(r.common.StringIdx(r.rawReloc(SectionString, pc)))
	}
	panicf("pkgbits: %q: export data desync in section %v, index %v, offset %v: found %v, expected %v, written at:%s",
		r.common.pkgPath, r.k, r.Idx, pos, mHave, mWant, where.String())
}

// Bool decodes and returns a bool value from the element bitstream.
func (r *Decoder) Bool() bool {
	r.Sync(SyncBool)
	x, err := r.Data.ReadByte()
	r.checkErr(err)
	assert(x < 2)
	return x != 0
}

// Int64 decodes and returns an int64 value from the element bitstream.
func (r *Decoder) Int64() int64 {
	r.Sync(SyncInt64)
	return r.rawVarint()
}

// Uint64 decodes and returns a uint64 value from the element bitstream.
func (r *Decoder) Uint64() uint64 {
	r.Sync(SyncUint64)
	return r.rawUvarint()
}

// Len decodes and returns a non-negative int value from the element bitstream.
func (r *Decoder) Len() int { x := r.Uint64(); v := int(x); assert(uint64(v) == x); return v }

// Uint decodes and returns a uint value from the element bitstream.
func (r *Decoder) Uint() uint { x := r.Uint64(); v := uint(x); assert(uint64(v) == x); return v }

// Code decodes a Code value from the element bitstream and returns
// its ordinal value. It's the caller's responsibility to convert the
// result to an appropriate Code type.
//
// TODO(mdempsky): Ideally this method would have signature "Code[T
// Code] T" instead, but we don't allow generic methods and the
// compiler can't depend on generics yet anyway.
func (r *Decoder) Code(mark SyncMarker) int {
	r.Sync(mark)
	return r.Len()
}

// Reloc decodes a relocation of expected section k from the element
// bitstream and returns an index to the referenced element.
func (r *Decoder) Reloc(k SectionKind) RelElemIdx {
	r.Sync(SyncUseReloc)
	return r.rawReloc(k, r.Len())
}

// String decodes and returns a string value from the element
// bitstream.
func (r *Decoder) String() string {
	r.Sync(SyncString)
	return r.common.StringIdx(r.Reloc(SectionString))
}

// Value decodes and returns a constant.Value from the element
// bitstream.
func (r *Decoder) Value() constant.Value {
	r.Sync(SyncValue)
	isComplex := r.Bool()
	val := r.scalar()
	if isComplex {
		val = constant.BinaryOp(val, token.ADD, constant.MakeImag(r.scalar()))
	}
	return val
}

func (r *Decoder) scalar() constant.Value {
	switch tag := CodeVal(r.Code(SyncVal)); tag {
	default:
		panic(fmt.Errorf("unexpected scalar tag: %v", tag))

	case ValBool:
		return constant.MakeBool(r.Bool())
	case ValString:
		return constant.MakeString(r.String())
	case ValInt64:
		return constant.MakeInt64(r.Int64())
	case ValBigInt:
		return constant.Make(r.bigInt())
	case ValBigRat:
		num := r.bigInt()
		denom := r.bigInt()
		return constant.Make(new(big.Rat).SetFrac(num, denom))
	case ValBigFloat:
		return constant.Make(r.bigFloat())
	}
}

func (r *Decoder) bigInt() *big.Int {
	v := new(big.Int).SetBytes([]byte(r.String()))
	if r.Bool() {
		v.Neg(v)
	}
	return v
}

func (r *Decoder) bigFloat() *big.Float {
	v := new(big.Float).SetPrec(512)
	assert(v.UnmarshalText([]byte(r.String())) == nil)
	return v
}

// PeekPkgPath returns the package path for the specified package index.
//
// nanogo restored this and [PkgDecoder.PeekObj] with the writer half: both
// answer what an element is without decoding it, which is what a writer that
// copies or checks elements needs.
func (pr *PkgDecoder) PeekPkgPath(idx RelElemIdx) string {
	var path string
	{
		r := pr.TempDecoder(SectionPkg, idx, SyncPkgDef)
		path = r.String()
		pr.RetireDecoder(&r)
	}
	if path == "" {
		path = pr.pkgPath
	}
	return path
}

// PeekObj returns the package path, object name and CodeObj of the object at
// the given index, without decoding the object itself.
func (pr *PkgDecoder) PeekObj(idx RelElemIdx) (string, string, CodeObj) {
	var ridx RelElemIdx
	var name string
	var rcode int
	{
		r := pr.TempDecoder(SectionName, idx, SyncObject1)
		r.Sync(SyncSym)
		r.Sync(SyncPkg)
		ridx = r.Reloc(SectionPkg)
		name = r.String()
		rcode = r.Code(SyncCodeObj)
		pr.RetireDecoder(&r)
	}

	path := pr.PeekPkgPath(ridx)
	assert(name != "")

	return path, name, CodeObj(rcode)
}

// Version reports the version of the bitstream.
func (w *Decoder) Version() Version { return w.common.version }

// mustRead turns a short read of the header into a diagnostic that names the
// package, because the reader's caller holds the file name and nothing else.
func mustRead(pkgPath string, err error) {
	if err != nil {
		panicf("pkgbits: %q: export data is truncated: %v", pkgPath, err)
	}
}
