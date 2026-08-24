// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package obj writes Go object files in the goobj format.
//
// The format is the one cmd/compile and cmd/asm produce and cmd/link
// consumes. specs/040-object-format.md is the design and
// specs/000-decisions.md decisions 3 and 11 are the reasons: nanogo emits
// objects rather than assembly text, and the objects are byte-compatible
// with gc so a nanogo package and a gc package link together.
//
// # Model
//
// A [Package] holds five symbol lists, one per index space the format
// defines. A caller adds a symbol to a list and gets a [SymRef] back. All
// later references, from relocations and from auxiliary entries, use that
// SymRef. The writer resolves nothing by name, so it needs no symbol map
// and no lookup on the write path. This is what makes the output
// deterministic: every block is written from a slice, in insertion order.
// See specs/053-determinism.md.
//
// # What this package does not do
//
// It writes. It does not read. The linker reads objects and
// specs/045-linker.md owns that.
//
// It does not build FuncInfo. A function symbol carries its FuncInfo as an
// ordinary symbol referenced from an [Aux] entry, so the container is here
// and the contents come with specs/027-liveness-and-stackmaps.md.
//
// The writer preserves the order of [Symbol].Aux. Byte-identity with gc
// therefore needs the caller to use gc's order: Gotype, FuncInfo, Funcdata,
// DWARF, Pcsp, Pcfile, Pcline, Pcinline, Pcdata.
package obj

// Magic is the goobj format version tag. It is the first eight bytes of
// every object file. The version is pinned, not negotiated: specs/040 says
// a mismatch is an error naming both versions, and [VerifyToolchain] is
// where that check happens.
const Magic = "\x00go120ld"

// Block indices. The header stores one uint32 offset per block, in this
// order, and the reader computes a block's length from the next offset.
// BlkEnd is the end of the last block, so the array has NBlk entries and
// the last one is the file size.
const (
	BlkAutolib = iota
	BlkPkgIdx
	BlkFile
	BlkSymdef
	BlkHashed64def
	BlkHasheddef
	BlkNonpkgdef
	BlkNonpkgref
	BlkRefFlags
	BlkHash64
	BlkHash
	BlkRelocIdx
	BlkAuxIdx
	BlkDataIdx
	BlkReloc
	BlkAux
	BlkData
	BlkRefName
	BlkEnd
	NBlk
)

// Package indices. A symbol reference is a package index and a symbol
// index. These package indices are not packages: they select which
// definition array the symbol index points into. Index 0 is invalid, and
// {0, 0} is the nil symbol, which relocations use as a marker.
const (
	PkgIdxNone     = (1<<31 - 1) - iota // NonPkgDefs, overflowing into NonPkgRefs
	PkgIdxHashed64                      // Hashed64Defs
	PkgIdxHashed                        // HashedDefs
	PkgIdxBuiltin                       // predeclared runtime symbols
	PkgIdxSelf                          // SymbolDefs, this package
	PkgIdxSpecial  = PkgIdxSelf         // indices above this have special meaning
	PkgIdxInvalid  = 0
	// Referenced packages are numbered from 1.
)

// Object flags, stored in the header. The linker reads them before it
// reads any symbol.
const (
	ObjFlagShared       = 1 << iota // built with -shared
	_                               // was ObjFlagNeedNameExpansion
	ObjFlagFromAssembly             // produced from assembly source, not Go
	ObjFlagUnlinkable               // compiled without a package path: the linker rejects it
	ObjFlagStd                      // a standard library package
)

// A SymKind is the kind of memory a symbol occupies. The linker maps these
// onto output sections, so the kind decides whether a symbol is code, is
// read only, is zero filled, and whether the garbage collector scans it.
//
// The values are the objabi.SymKind values and the order is load bearing:
// the linker indexes a table with them.
type SymKind uint8

const (
	Sxxx  SymKind = iota // invalid
	STEXT                // executable instructions
	STEXTFIPS
	SRODATA // read only static data
	SRODATAFIPS
	SNOPTRDATA // static data with no pointers in it
	SNOPTRDATAFIPS
	SDATA // static data
	SDATAFIPS
	SBSS      // static data that starts as zero
	SNOPTRBSS // static zero data with no pointers in it
	STLSBSS   // thread local data that starts as zero
	SDWARFCUINFO
	SDWARFCONST
	SDWARFFCN
	SDWARFABSFCN
	SDWARFTYPE
	SDWARFVAR
	SDWARFRANGE
	SDWARFLOC
	SDWARFLINES
	SDWARFADDR
	SLIBFUZZER_8BIT_COUNTER
	SCOVERAGE_COUNTER
	SCOVERAGE_AUXVAR
	SSEHUNWINDINFO
)

// IsText reports whether k is one of the code kinds.
func (k SymKind) IsText() bool { return k == STEXT || k == STEXTFIPS }

// ABI values. The ABI is part of a symbol's identity: the same name can
// exist once per ABI, and the linker keeps them apart so a call through an
// ABI0 wrapper reaches the wrapper and not the ABIInternal body.
// See specs/030-abi.md.
const (
	ABI0        = 0
	ABIInternal = 1

	// ABIStatic marks a file static symbol. It is not an ABI. The linker
	// uses it to keep two static symbols with the same name in different
	// files apart.
	ABIStatic = ^uint16(0)
)

// Symbol flags, the first byte.
const (
	SymFlagDupok = 1 << iota // duplicate definitions are allowed and merged
	SymFlagLocal
	SymFlagTypelink
	SymFlagLeaf
	SymFlagNoSplit
	SymFlagReflectMethod
	SymFlagGoType
)

// Symbol flags, the second byte.
const (
	SymFlagUsedInIface = 1 << iota
	SymFlagItab
	SymFlagDict
	SymFlagPkgInit
	SymFlagLinkname
	SymFlagLinknameStd
	SymFlagABIWrapper
	SymFlagWasmExport
)

// A RelocType names the arithmetic the linker applies when it resolves a
// relocation. The values are objabi.RelocType values. Only the generic
// entries and the arm64 entries are declared, which is what
// specs/042-arm64-backend.md needs. The order is exact, because the
// linker switches on the number.
type RelocType int16

const (
	R_ADDR RelocType = 1 + iota
	R_ADDRPOWER
	R_ADDRARM64 // an adrp and add pair that computes an address
	R_ADDRMIPS
	R_ADDROFF // 32 bit offset from the start of the section being relocated
	R_SIZE
	R_CALL
	R_CALLARM
	R_CALLARM64 // the 26 bit immediate of an arm64 BL
	R_CALLIND
	R_CALLPOWER
	R_CALLMIPS
	R_CONST
	R_PCREL
	R_TLS_LE
	R_TLS_IE
	R_GOTOFF
	R_PLT0
	R_PLT1
	R_PLT2
	R_USEFIELD
	R_USETYPE
	R_USEIFACE // no bytes change: it only keeps a symbol alive
	R_USEIFACEMETHOD
	R_USENAMEDMETHOD
	R_METHODOFF
	R_KEEP
	R_POWER_TOC
	R_GOTPCREL
	R_JMPMIPS
	R_DWARFSECREF
	R_ARM64_TLS_LE
	R_ARM64_TLS_IE
	R_ARM64_GOTPCREL
	R_ARM64_GOT
	R_ARM64_PCREL
	R_ARM64_PCREL_LDST8
	R_ARM64_PCREL_LDST16
	R_ARM64_PCREL_LDST32
	R_ARM64_PCREL_LDST64
	R_ARM64_LDST8
	R_ARM64_LDST16
	R_ARM64_LDST32
	R_ARM64_LDST64
	R_ARM64_LDST128
)

// R_WEAK marks a relocation as weak: the linker resolves it if the target
// is present and leaves it zero if it is not.
const (
	R_WEAK        RelocType = -1 << 15
	R_WEAKADDR              = R_WEAK | R_ADDR
	R_WEAKADDROFF           = R_WEAK | R_ADDROFF
)

// An AuxType names what an auxiliary symbol is to the symbol that carries
// it. specs/040 lists the four the compiler needs first.
type AuxType uint8

const (
	AuxGotype   AuxType = iota // the symbol's type descriptor
	AuxFuncInfo                // frame size, argument size, file table, pcdata offsets
	AuxFuncdata                // one per FUNCDATA index: stack maps, stack objects, defer info
	AuxDwarfInfo
	AuxDwarfLoc
	AuxDwarfRanges
	AuxDwarfLines
	AuxPcsp   // the stack pointer delta table
	AuxPcfile // the file table
	AuxPcline // the line table
	AuxPcinline
	AuxPcdata // one per PCDATA index
	AuxWasmImport
	AuxWasmType
	AuxSehUnwindInfo
)

// A SymRef points at a symbol. PkgIdx selects the array and SymIdx is the
// position in it. The zero value is the nil symbol.
type SymRef struct {
	PkgIdx uint32
	SymIdx uint32
}

// IsZero reports whether r is the nil symbol reference.
func (r SymRef) IsZero() bool { return r == SymRef{} }

// A Reloc is one edit the linker makes to a symbol's data.
type Reloc struct {
	Off  int32     // byte offset in the symbol's data
	Size uint8     // width of the edit in bytes
	Type RelocType // the arithmetic to apply
	Add  int64     // constant added to the target address
	Sym  SymRef    // the target
}

// An Aux attaches one metadata symbol to the symbol that carries it.
type Aux struct {
	Type AuxType
	Sym  SymRef
}

// A Symbol is one definition or one reference.
//
// Size and Data are separate because they disagree for the zero filled
// kinds: an SBSS symbol has a size and no data, and the linker allocates
// the space. For every other kind Size is len(Data), and [Package.Write]
// checks that.
type Symbol struct {
	Name   string
	ABI    uint16
	Type   SymKind
	Flag   uint8 // caller set bits, see SymFlagDupok and the rest
	Flag2  uint8 // caller set bits, see SymFlagUsedInIface and the rest
	Size   uint32
	Align  uint32
	Data   []byte
	Relocs []Reloc

	// Aux is written in the order given. A text symbol needs an
	// AuxFuncInfo entry: without one it belongs to no compilation unit,
	// and cmd/link's DWARF pass sorts an empty list and panics with no
	// diagnostic. Link with -w until nanogo writes FuncInfo.
	Aux []Aux

	// Pcdata marks a pc-value table. It is not a flag in the file. It
	// selects the section class the content hash covers, which keeps a
	// pc-value table from merging with read only data that happens to hold
	// the same bytes.
	Pcdata bool
}

// An ImportedPkg is one Autolib entry: a package this one imports, with
// the fingerprint of the export data it was compiled against. The linker
// compares the fingerprint with the one in that package's own object and
// refuses a mismatched build. specs/040 calls this the mechanism that
// makes hosted mode safe.
type ImportedPkg struct {
	Path        string
	Fingerprint [8]byte
}

// A RefFlag carries flags for a symbol defined in another package. Only
// SymFlagUsedInIface travels this way, because the linker needs it before
// it has read the defining object.
type RefFlag struct {
	Sym   SymRef
	Flag  uint8
	Flag2 uint8
}

// A RefName gives the name of a symbol defined in another package. No
// linker step reads this block. It exists so go tool nm and go tool
// objdump can print a name instead of an index pair.
type RefName struct {
	Sym  SymRef
	Name string
}
