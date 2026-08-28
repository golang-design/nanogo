// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// The oracle for pclntab, and the comparisons that use it.
//
// specs/045-linker.md says why this file exists and why it is not a test
// that runs a program: pclntab is the one stage whose failure is quiet. A
// wrong reachability set fails to link and a wrong layout faults at the
// first call, but a wrong pclntab produces a program that runs correctly
// until something asks where it is, and then lies. So the comparison is
// against cmd/link's own table for the same objects, table by table and
// byte for byte, and a running program proves nothing here.

package link

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"runtime"
	"strconv"
	"testing"
)

// A pclnImage is the pclntab cmd/link wrote, read back out of the
// executable.
//
// Every table but the header is a sub-symbol the linker hides from the
// symbol table, so the bounds come from the header rather than from a
// symbol. That is the same path the runtime takes, which is what makes
// the header the first thing to be right.
type pclnImage struct {
	magic          uint32
	minLC, ptrSize uint8
	nfunc, nfiles  uint64

	funcnametab []byte
	cutab       []byte
	filetab     []byte
	pctab       []byte
	functab     []byte // the func table and everything after it
}

// pclntabMagic is abi.Go120PCLnTabMagic, the version the toolchain
// emits. A table of another version is a table this comparison cannot
// read, and reading it anyway would compare the wrong bytes.
const pclntabMagic = 0xfffffff1

// readPclntab reads the pclntab of an executable cmd/link wrote.
func readPclntab(t *testing.T, exe string) *pclnImage {
	t.Helper()
	f, err := macho.Open(exe)
	if err != nil {
		t.Fatalf("reading the executable: %v", err)
	}
	defer f.Close()
	sect := f.Section("__gopclntab")
	if sect == nil {
		t.Fatal("the executable has no __gopclntab section")
	}
	data, err := sect.Data()
	if err != nil {
		t.Fatalf("reading __gopclntab: %v", err)
	}
	const headerSize = 8 + 8*8
	if len(data) < headerSize {
		t.Fatalf("__gopclntab is %d bytes, which is shorter than a pcHeader", len(data))
	}
	img := &pclnImage{
		magic:   binary.LittleEndian.Uint32(data),
		minLC:   data[6],
		ptrSize: data[7],
		nfunc:   binary.LittleEndian.Uint64(data[8:]),
		nfiles:  binary.LittleEndian.Uint64(data[16:]),
	}
	if img.magic != pclntabMagic {
		t.Fatalf("the table starts with magic %#x and this comparison reads %#x", img.magic, uint32(pclntabMagic))
	}
	// The five offsets are from the start of the header, they are in the
	// order the tables are written, and each table ends where the next
	// one starts. A pair out of order is a header this cannot read.
	off := make([]uint64, 5)
	for i := range off {
		off[i] = binary.LittleEndian.Uint64(data[32+8*i:])
	}
	for i, o := range off {
		if o > uint64(len(data)) {
			t.Fatalf("the header puts table %d at %#x and the section is %#x bytes", i, o, len(data))
		}
		if i > 0 && o < off[i-1] {
			t.Fatalf("the header puts table %d at %#x, in front of table %d at %#x", i, o, i-1, off[i-1])
		}
	}
	if off[0] < headerSize {
		t.Fatalf("the header puts the first table at %#x, inside the header", off[0])
	}
	img.funcnametab = data[off[0]:off[1]]
	img.cutab = data[off[1]:off[2]]
	img.filetab = data[off[2]:off[3]]
	img.pctab = data[off[3]:off[4]]
	img.functab = data[off[4]:]
	return img
}

// TestPclntabOracleReadsTheLinkersTable checks that the oracle every
// pclntab comparison rests on reads a table it understands.
//
// It gates the reader and not the builder. A comparison against a table
// this could not parse would pass by comparing nothing, so the shape of
// what was read is checked before anything is compared against it: the
// version, the two width fields, the counts, and that each table is
// where the header says and holds what its own form requires.
func TestPclntabOracleReadsTheLinkersTable(t *testing.T) {
	target := TargetFor(runtime.GOOS, runtime.GOARCH)
	if target == nil || runtime.GOOS != "darwin" {
		t.Skipf("the section this reads is Mach-O's, and this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	for _, b := range []*build{&hostBuild, &reflectBuild} {
		t.Run(b.pkg, func(t *testing.T) {
			b := b.get(t)
			img := readPclntab(t, linkExe(t, b))

			if int64(img.minLC) != target.MinLC {
				t.Errorf("the table says the shortest instruction is %d bytes and this target's is %d", img.minLC, target.MinLC)
			}
			if int64(img.ptrSize) != target.PtrSize {
				t.Errorf("the table says a pointer is %d bytes and this target's is %d", img.ptrSize, target.PtrSize)
			}
			if img.nfunc < 1000 {
				t.Fatalf("the table describes %d functions, so a comparison against it proves little", img.nfunc)
			}
			// A name table is a run of null terminated strings, so it
			// ends with a null and holds one name per null.
			if n := len(img.funcnametab); n == 0 || img.funcnametab[n-1] != 0 {
				t.Errorf("the function name table is %d bytes and does not end with a null", n)
			}
			if got := uint64(bytes.Count(img.funcnametab, []byte{0})); got < img.nfunc {
				t.Errorf("the function name table holds %d names and the table describes %d functions", got, img.nfunc)
			}
			// The file table is the same shape and the header says how
			// many names are in it. Every table the linker generates is
			// rounded up to a pointer boundary, so the padding at the end
			// is up to one pointer of nulls and no name.
			if got := uint64(bytes.Count(img.filetab, []byte{0})); got < img.nfiles || got-img.nfiles >= uint64(img.ptrSize) {
				t.Errorf("the file table holds %d nulls, the header says %d names and the padding is under %d bytes",
					got, img.nfiles, img.ptrSize)
			}
			// The compilation unit table is one uint32 per entry, and
			// every entry is an offset into the file table or the
			// invalid offset the linker writes for a file the dead code
			// pass dropped.
			if len(img.cutab)%4 != 0 {
				t.Errorf("the compilation unit table is %d bytes, which is not a whole number of entries", len(img.cutab))
			}
			for i := 0; i+4 <= len(img.cutab); i += 4 {
				o := binary.LittleEndian.Uint32(img.cutab[i:])
				if o != ^uint32(0) && int(o) >= len(img.filetab) {
					t.Fatalf("compilation unit entry %d points at %#x and the file table is %#x bytes", i/4, o, len(img.filetab))
					break
				}
			}
			// A pc-value table offset of zero means no table, so the
			// linker pads a byte at the front to keep every real offset
			// non-zero.
			if len(img.pctab) == 0 || img.pctab[0] != 0 {
				t.Errorf("the pc-value table does not start with the byte that keeps a real offset non-zero")
			}
			t.Logf("%d functions, %d files, funcnametab %#x, cutab %#x, filetab %#x, pctab %#x, functab %#x bytes",
				img.nfunc, img.nfiles, len(img.funcnametab), len(img.cutab),
				len(img.filetab), len(img.pctab), len(img.functab))
		})
	}
}

// buildPcln lays a program out and builds its pclntab.
func buildPcln(t *testing.T, b *build, target *Target) (*Loader, *Layout, *Pcln) {
	t.Helper()
	l := loadProgram(t, b)
	setStringVars(t, l, b)
	l.InitTasks()
	r := l.Deadcode(runtime.GOOS, runtime.GOARCH)
	a := l.Layout(r, target)
	return l, a, a.Pclntab()
}

// TestFuncNameTabAgreesWithTheLinker compares the function name table
// with cmd/link's, byte for byte.
//
// A name table that holds the same names in another order is a table
// every _func record indexes wrongly, and every offset in it would then
// name the wrong function in a traceback. So the comparison is over the
// bytes and not over the set of names, and the first byte that differs
// names the function the two orders part company at.
func TestFuncNameTabAgreesWithTheLinker(t *testing.T) {
	target := TargetFor(runtime.GOOS, runtime.GOARCH)
	if target == nil || runtime.GOOS != "darwin" {
		t.Skipf("the section the oracle reads is Mach-O's, and this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	for _, b := range []*build{&hostBuild, &reflectBuild} {
		t.Run(b.pkg, func(t *testing.T) {
			b := b.get(t)
			img := readPclntab(t, linkExe(t, b))
			_, _, p := buildPcln(t, b, target)

			if got, want := uint64(len(p.Funcs)), img.nfunc; got != want {
				t.Errorf("the table describes %d functions and cmd/link's describes %d", got, want)
			}
			if bytes.Equal(p.FuncNameTab, img.funcnametab) {
				t.Logf("%d names in %#x bytes agree with cmd/link", len(p.funcName), len(p.FuncNameTab))
				return
			}
			t.Errorf("the function name table is %#x bytes and cmd/link's is %#x: %s",
				len(p.FuncNameTab), len(img.funcnametab), firstNameDiff(p.FuncNameTab, img.funcnametab))
		})
	}
}

// firstNameDiff describes where two null terminated name tables part
// company, in the names they hold rather than in the bytes.
func firstNameDiff(mine, theirs []byte) string {
	off := 0
	for off < len(mine) && off < len(theirs) && mine[off] == theirs[off] {
		off++
	}
	start := bytes.LastIndexByte(mine[:off], 0) + 1
	name := func(b []byte) string {
		if start >= len(b) {
			return "(past the end)"
		}
		b = b[start:]
		if i := bytes.IndexByte(b, 0); i >= 0 {
			b = b[:i]
		}
		return string(b)
	}
	return "at 0x" + strconv.FormatInt(int64(start), 16) + " nanogo has " +
		strconv.Quote(name(mine)) + " and cmd/link has " + strconv.Quote(name(theirs))
}

// TestFileTabsAgreeWithTheLinker compares the file name table and the
// compilation unit table with cmd/link's, byte for byte.
//
// The two are compared together because neither means anything without
// the other: an entry of the unit table is an offset into the file
// table, so a file table whose names moved is a unit table that points
// at the middle of a name. A function reaches a file name through both.
func TestFileTabsAgreeWithTheLinker(t *testing.T) {
	target := TargetFor(runtime.GOOS, runtime.GOARCH)
	if target == nil || runtime.GOOS != "darwin" {
		t.Skipf("the section the oracle reads is Mach-O's, and this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	for _, b := range []*build{&hostBuild, &reflectBuild} {
		t.Run(b.pkg, func(t *testing.T) {
			b := b.get(t)
			img := readPclntab(t, linkExe(t, b))
			_, _, p := buildPcln(t, b, target)

			if got, want := uint64(p.nfiles), img.nfiles; got != want {
				t.Errorf("the table names %d files and cmd/link's names %d", got, want)
			}
			if !bytes.Equal(p.FileTab, img.filetab) {
				t.Errorf("the file name table is %#x bytes and cmd/link's is %#x: %s",
					len(p.FileTab), len(img.filetab), firstNameDiff(p.FileTab, img.filetab))
			}
			if !bytes.Equal(p.CuTab, img.cutab) {
				t.Errorf("the compilation unit table is %#x bytes and cmd/link's is %#x: %s",
					len(p.CuTab), len(img.cutab), firstEntryDiff(p.CuTab, img.cutab))
			}
			t.Logf("%d files in %#x bytes, %d compilation unit entries", p.nfiles, len(p.FileTab), len(p.CuTab)/4)
		})
	}
}

// firstEntryDiff describes where two tables of uint32 part company.
func firstEntryDiff(mine, theirs []byte) string {
	for i := 0; i+4 <= len(mine) && i+4 <= len(theirs); i += 4 {
		a := binary.LittleEndian.Uint32(mine[i:])
		b := binary.LittleEndian.Uint32(theirs[i:])
		if a != b {
			return "entry " + strconv.Itoa(i/4) + " is 0x" + strconv.FormatUint(uint64(a), 16) +
				" for nanogo and 0x" + strconv.FormatUint(uint64(b), 16) + " for cmd/link"
		}
	}
	return "the shorter table is a prefix of the longer one"
}
