// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import "golang.design/x/nanogo/obj"

// A Kind is the kind of memory a symbol occupies in the output, which is
// not the kind the object file records.
//
// The object records what the compiler knew: text, read-only data,
// pointer-free data, data, and the two zero-filled kinds. The linker
// splits some of those again, brackets each group with a start and an end
// kind, and adds the kinds no object can hold, and it is the linker's
// alphabet that decides layout. Two rules read the numeric order and not
// the name, so the order is the format's and not this file's choice:
//
//   - the text symbols are sorted by kind, which is what gathers the FIPS
//     text between its two bracketing symbols;
//   - dodata's grouping walks the kinds in order, so a kind between two
//     others lands between their bytes.
//
// The list mirrors cmd/link/internal/sym.SymKind and is pinned the way
// obj.Magic is: [TestKindsMatchTheToolchain] compares it with the
// installed toolchain's, because a list one entry out lays every later
// kind out in the wrong place and nothing else would say so.
type Kind uint8

// The kinds, in the toolchain's order.
const (
	Kxxx Kind = iota

	// Text.
	KTEXT
	KTEXTFIPSSTART
	KTEXTFIPS
	KTEXTFIPSEND
	KTEXTEND
	KELFRXSECT
	KMACHOPLT

	// Read only, not executable, not relocated.
	KSTRING
	KGOSTRING
	KGCBITS
	KRODATA
	KRODATAFIPSSTART
	KRODATAFIPS
	KRODATAFIPSEND
	KRODATAEND
	KPCLNTAB
	KELFROSECT

	// Read only, not executable, relocated at load time when the output
	// is position independent.
	KRODATARELRO
	KTYPE
	KGOFUNC
	KELFRELROSECT
	KMACHORELROSECT

	// Writable.
	KFirstWritable
	KBUILDINFO
	KFIPSINFO
	KELFSECT
	KMACHO
	KWINDOWS
	KMODULEDATA
	KELFGOT
	KMACHOGOT
	KNOPTRDATA
	KNOPTRDATAFIPSSTART
	KNOPTRDATAFIPS
	KNOPTRDATAFIPSEND
	KNOPTRDATAEND
	KINITARR
	KDATA
	KDATAFIPSSTART
	KDATAFIPS
	KDATAFIPSEND
	KDATAEND
	KXCOFFTOC

	// Zero filled.
	KBSS
	KNOPTRBSS
	KGCMASK
	KLIBFUZZER_8BIT_COUNTER
	KCOVERAGE_COUNTER
	KCOVERAGE_AUXVAR
	KTLSBSS

	// Not allocated in the image.
	KFirstUnallocated
	KXREF
	KMACHOSYMSTR
	KMACHOSYMTAB
	KMACHOINDIRECTPLT
	KMACHOINDIRECTGOT
	KDYNIMPORT
	KHOSTOBJ
	KUNDEFEXT

	KDWARFSECT
	KDWARFCUINFO
	KDWARFCONST
	KDWARFFCN
	KDWARFABSFCN
	KDWARFTYPE
	KDWARFVAR
	KDWARFRANGE
	KDWARFLOC
	KDWARFLINES
	KDWARFADDR

	KSEHUNWINDINFO
	KSEHSECT

	numKind
)

// kindNames is the toolchain's name for each kind, without the leading
// S. It exists so a diagnostic and the comparison with the toolchain both
// name a kind the way cmd/link does.
var kindNames = [numKind]string{
	Kxxx:                    "xxx",
	KTEXT:                   "TEXT",
	KTEXTFIPSSTART:          "TEXTFIPSSTART",
	KTEXTFIPS:               "TEXTFIPS",
	KTEXTFIPSEND:            "TEXTFIPSEND",
	KTEXTEND:                "TEXTEND",
	KELFRXSECT:              "ELFRXSECT",
	KMACHOPLT:               "MACHOPLT",
	KSTRING:                 "STRING",
	KGOSTRING:               "GOSTRING",
	KGCBITS:                 "GCBITS",
	KRODATA:                 "RODATA",
	KRODATAFIPSSTART:        "RODATAFIPSSTART",
	KRODATAFIPS:             "RODATAFIPS",
	KRODATAFIPSEND:          "RODATAFIPSEND",
	KRODATAEND:              "RODATAEND",
	KPCLNTAB:                "PCLNTAB",
	KELFROSECT:              "ELFROSECT",
	KRODATARELRO:            "RODATARELRO",
	KTYPE:                   "TYPE",
	KGOFUNC:                 "GOFUNC",
	KELFRELROSECT:           "ELFRELROSECT",
	KMACHORELROSECT:         "MACHORELROSECT",
	KFirstWritable:          "FirstWritable",
	KBUILDINFO:              "BUILDINFO",
	KFIPSINFO:               "FIPSINFO",
	KELFSECT:                "ELFSECT",
	KMACHO:                  "MACHO",
	KWINDOWS:                "WINDOWS",
	KMODULEDATA:             "MODULEDATA",
	KELFGOT:                 "ELFGOT",
	KMACHOGOT:               "MACHOGOT",
	KNOPTRDATA:              "NOPTRDATA",
	KNOPTRDATAFIPSSTART:     "NOPTRDATAFIPSSTART",
	KNOPTRDATAFIPS:          "NOPTRDATAFIPS",
	KNOPTRDATAFIPSEND:       "NOPTRDATAFIPSEND",
	KNOPTRDATAEND:           "NOPTRDATAEND",
	KINITARR:                "INITARR",
	KDATA:                   "DATA",
	KDATAFIPSSTART:          "DATAFIPSSTART",
	KDATAFIPS:               "DATAFIPS",
	KDATAFIPSEND:            "DATAFIPSEND",
	KDATAEND:                "DATAEND",
	KXCOFFTOC:               "XCOFFTOC",
	KBSS:                    "BSS",
	KNOPTRBSS:               "NOPTRBSS",
	KGCMASK:                 "GCMASK",
	KLIBFUZZER_8BIT_COUNTER: "LIBFUZZER_8BIT_COUNTER",
	KCOVERAGE_COUNTER:       "COVERAGE_COUNTER",
	KCOVERAGE_AUXVAR:        "COVERAGE_AUXVAR",
	KTLSBSS:                 "TLSBSS",
	KFirstUnallocated:       "FirstUnallocated",
	KXREF:                   "XREF",
	KMACHOSYMSTR:            "MACHOSYMSTR",
	KMACHOSYMTAB:            "MACHOSYMTAB",
	KMACHOINDIRECTPLT:       "MACHOINDIRECTPLT",
	KMACHOINDIRECTGOT:       "MACHOINDIRECTGOT",
	KDYNIMPORT:              "DYNIMPORT",
	KHOSTOBJ:                "HOSTOBJ",
	KUNDEFEXT:               "UNDEFEXT",
	KDWARFSECT:              "DWARFSECT",
	KDWARFCUINFO:            "DWARFCUINFO",
	KDWARFCONST:             "DWARFCONST",
	KDWARFFCN:               "DWARFFCN",
	KDWARFABSFCN:            "DWARFABSFCN",
	KDWARFTYPE:              "DWARFTYPE",
	KDWARFVAR:               "DWARFVAR",
	KDWARFRANGE:             "DWARFRANGE",
	KDWARFLOC:               "DWARFLOC",
	KDWARFLINES:             "DWARFLINES",
	KDWARFADDR:              "DWARFADDR",
	KSEHUNWINDINFO:          "SEHUNWINDINFO",
	KSEHSECT:                "SEHSECT",
}

// String returns the toolchain's name for the kind, with the S the
// toolchain spells it with.
func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return "S" + kindNames[k]
	}
	return "Kind(" + itoa(int(k)) + ")"
}

// NumKind is how many kinds the format defines. It is what the comparison
// with the toolchain counts.
func NumKind() int { return int(numKind) }

// KindName returns the toolchain's name for the kind at index i, without
// the leading S.
func KindName(i int) string {
	if i < 0 || i >= len(kindNames) {
		return ""
	}
	return kindNames[i]
}

// IsText reports whether the kind lives in the text segment.
func (k Kind) IsText() bool { return k >= KTEXT && k <= KTEXTEND }

// objKind maps an object file's kind to the linker's.
//
// It is cmd/link's AbiSymKindToSymKind. The object has no kind for the
// bracketing symbols or for the groups the linker forms by name, so the
// mapping is into a subset and the rest of the alphabet is reached by
// [Loader.group] and by the symbols the linker makes.
var objKind = [...]Kind{
	obj.Sxxx:                    Kxxx,
	obj.STEXT:                   KTEXT,
	obj.STEXTFIPS:               KTEXTFIPS,
	obj.SRODATA:                 KRODATA,
	obj.SRODATAFIPS:             KRODATAFIPS,
	obj.SNOPTRDATA:              KNOPTRDATA,
	obj.SNOPTRDATAFIPS:          KNOPTRDATAFIPS,
	obj.SDATA:                   KDATA,
	obj.SDATAFIPS:               KDATAFIPS,
	obj.SBSS:                    KBSS,
	obj.SNOPTRBSS:               KNOPTRBSS,
	obj.STLSBSS:                 KTLSBSS,
	obj.SDWARFCUINFO:            KDWARFCUINFO,
	obj.SDWARFCONST:             KDWARFCONST,
	obj.SDWARFFCN:               KDWARFFCN,
	obj.SDWARFABSFCN:            KDWARFABSFCN,
	obj.SDWARFTYPE:              KDWARFTYPE,
	obj.SDWARFVAR:               KDWARFVAR,
	obj.SDWARFRANGE:             KDWARFRANGE,
	obj.SDWARFLOC:               KDWARFLOC,
	obj.SDWARFLINES:             KDWARFLINES,
	obj.SDWARFADDR:              KDWARFADDR,
	obj.SLIBFUZZER_8BIT_COUNTER: KLIBFUZZER_8BIT_COUNTER,
	obj.SCOVERAGE_COUNTER:       KCOVERAGE_COUNTER,
	obj.SCOVERAGE_AUXVAR:        KCOVERAGE_AUXVAR,
	obj.SSEHUNWINDINFO:          KSEHUNWINDINFO,
}

// ObjKind returns the linker kind an object file's kind maps to.
func ObjKind(k obj.SymKind) Kind {
	if int(k) < len(objKind) {
		return objKind[k]
	}
	return Kxxx
}
