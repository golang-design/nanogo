// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

// Pos is a compact source position.
//
// It is one uint32 because there is a position in almost every syntax node,
// IR node and SSA value, and a compiler holds millions of them. A Pos is a
// FileSet base plus a byte offset, so resolving one to a file is a search over
// bases and resolving it to a line is a search over that file's line starts.
//
// Two different positions are distinguished throughout the compiler and must
// not be confused. The raw position orders tokens and is what the compiler
// compares. The reported position is what a diagnostic or a line table entry
// prints, and it is the raw position after any //line directive in force. The
// rule is that comparison uses raw and printing uses reported, so no method
// here returns both.
//
// See specs/010-scanner-and-positions.md.
type Pos uint32

// NoPos is the unknown position.
//
// A synthesised node takes the position of whatever it was synthesised for and
// never takes NoPos. A zero in a line table produces a debugger that steps into
// nothing and a profile that attributes time to nowhere.
const NoPos Pos = 0

// IsKnown reports whether p is a real position.
func (p Pos) IsKnown() bool { return p != NoPos }

// Position is a resolved source position. Columns count bytes, not runes.
//
// Bytes rather than runes because that is what the reference compiler counts,
// and every errorcheck file in the conformance corpus is annotated against it.
// A rune count would be correct in a different way and would disagree with all
// of them.
type Position struct {
	Filename string
	Line     uint // 1-based; 0 means unknown
	Col      uint // 1-based byte column; 0 means unknown
}

// IsKnown reports whether the position resolved to a real line.
func (p Position) IsKnown() bool { return p.Line > 0 }

func (p Position) String() string {
	if p.Filename == "" && p.Line == 0 {
		return "<unknown position>"
	}
	s := p.Filename
	if p.Line > 0 {
		if s != "" {
			s += ":"
		}
		s += strconv.FormatUint(uint64(p.Line), 10)
		if p.Col > 0 {
			s += ":" + strconv.FormatUint(uint64(p.Col), 10)
		}
	}
	return s
}

// lineDirective records a //line or /*line*/ directive.
//
// The rewrite is stored out of band rather than applied to the Pos, so a
// directive costs nothing at the point of use and the raw ordering of positions
// is never disturbed.
type lineDirective struct {
	offset int    // byte offset in the file where the directive takes effect
	line   uint   // the line number it asserts for that offset
	col    uint   // the column it asserts, or 0 for none
	name   string // the filename it asserts
}

// SrcFile is one source file's coordinate space inside a FileSet.
type SrcFile struct {
	set  *FileSet
	name string
	base Pos
	size int

	lines      []int // byte offset of the start of each line; lines[0] is always 0
	directives []lineDirective
}

// Name returns the file name the file was added under. It ignores //line
// directives, so it names the file on disk.
func (f *SrcFile) Name() string { return f.name }

// Base returns the first Pos belonging to the file.
func (f *SrcFile) Base() Pos { return f.base }

// Size returns the file's length in bytes.
func (f *SrcFile) Size() int { return f.size }

// Pos returns the position for a byte offset in the file.
//
// An offset outside the file is clamped rather than rejected. The scanner
// reports the end of a truncated file at the file's end, and a panic there
// would turn a malformed input into a compiler crash.
func (f *SrcFile) Pos(offset int) Pos {
	if offset < 0 {
		offset = 0
	}
	if offset > f.size {
		offset = f.size
	}
	return f.base + Pos(offset)
}

// Offset returns the byte offset of p in the file.
func (f *SrcFile) Offset(p Pos) int {
	if p < f.base {
		return 0
	}
	off := int(p - f.base)
	if off > f.size {
		return f.size
	}
	return off
}

// AddLine records that a line starts at the given byte offset.
//
// The scanner calls this as it consumes each newline, in increasing order. An
// offset that is not greater than the last one recorded is ignored, so a double
// call cannot corrupt the table.
//
// An offset equal to the file size is ignored too. A file that ends in a
// newline has no line after it, and go/token records none, so recording one
// here would report the end of such a file one line further down than every
// errorcheck annotation in the conformance corpus expects.
func (f *SrcFile) AddLine(offset int) {
	if n := len(f.lines); n == 0 || f.lines[n-1] < offset {
		if offset >= 0 && offset < f.size {
			f.lines = append(f.lines, offset)
		}
	}
}

// AddLineDirective records a //line directive taking effect at offset.
//
// offset is the offset of the first byte the directive governs. For a //line
// comment that is the start of the next line. For a /*line*/ comment, which may
// stand anywhere, it is the byte just after the closing delimiter, which may be
// in the middle of a line.
//
// The directive asserts that this offset is at line and, when col is not zero,
// at that column. A position further along the same line is that many bytes
// further right, which is what makes a mid-line directive resolvable.
//
// An empty name means the reported filename is empty, not inherited. See the
// comment on the append below.
//
// A directive with line 0 is ignored: the specification requires a positive
// line number and accepting one would produce unknown positions for the rest of
// the file.
func (f *SrcFile) AddLineDirective(offset int, name string, line, col uint) {
	if line == 0 {
		return
	}
	// An empty filename stays empty. It does not inherit the name in force and
	// it does not fall back to the file on disk.
	//
	// An earlier version inherited, on the reasoning that `//line :200` only
	// means to change the line. Both oracles disagree: for a file that already
	// carries `//line gen.go:100`, a later `//line :200` makes `go tool
	// compile` report `:200` and go/scanner report a position with no
	// filename, not `gen.go:200`.
	f.directives = append(f.directives, lineDirective{offset: offset, name: name, line: line, col: col})
}

// lastDirectiveAt returns the directive in force at offset, or nil.
func (f *SrcFile) lastDirectiveAt(offset int) *lineDirective {
	if len(f.directives) == 0 {
		return nil
	}
	i := sort.Search(len(f.directives), func(i int) bool {
		return f.directives[i].offset > offset
	})
	if i == 0 {
		return nil
	}
	return &f.directives[i-1]
}

// lineCol resolves a byte offset to a 1-based line and byte column, ignoring
// directives.
func (f *SrcFile) lineCol(offset int) (uint, uint) {
	if len(f.lines) == 0 {
		return 1, uint(offset) + 1
	}
	i := sort.Search(len(f.lines), func(i int) bool { return f.lines[i] > offset })
	// i is the first line start after offset, so line i is the one containing it.
	return uint(i), uint(offset-f.lines[i-1]) + 1
}

// RawPosition resolves p ignoring every //line directive.
//
// This is the position of the byte in the file on disk. Use it to compare
// positions and to find source text. Use Position to print.
func (f *SrcFile) RawPosition(p Pos) Position {
	off := f.Offset(p)
	line, col := f.lineCol(off)
	return Position{Filename: f.name, Line: line, Col: col}
}

// Position resolves p, applying the //line directive in force.
//
// This is what a diagnostic prints and what a line table records.
func (f *SrcFile) Position(p Pos) Position {
	off := f.Offset(p)
	line, col := f.lineCol(off)
	d := f.lastDirectiveAt(off)
	if d == nil {
		return Position{Filename: f.name, Line: line, Col: col}
	}
	dline, _ := f.lineCol(d.offset)
	// The directive asserts that its own offset is at d.line, so a position n
	// lines further down is at d.line + n.
	rline := d.line + (line - dline)

	// Columns under a directive follow three rules, and all three were checked
	// against the reference compiler rather than reasoned about:
	//
	//	//line gen.go:100      -> gen.go:100      and gen.go:101
	//	//line gen.go:100:5    -> gen.go:100:17   and gen.go:101:13
	//
	// So a directive with no column makes the column unknown for every line it
	// governs, and an unknown column is not printed. Reporting the original
	// file's raw column under the generated file's name would be a fabrication,
	// and it would disagree with the errorcheck annotations in the conformance
	// corpus.
	//
	// A directive with a column keeps columns known. On the line the directive
	// governs, the column is measured from the directive's own offset, which is
	// what makes a mid-line /*line f:l:c*/ resolvable. An earlier version
	// measured from the start of the line and reported a column too far right
	// by however much text preceded the directive; three files in the Go
	// distribution's test corpus depend on the correct form, and the scanner
	// cannot compensate for the wrong one because the compensation underflows.
	// On every later line the ordinary raw column applies.
	rcol := uint(0)
	switch {
	case d.col == 0:
		// Unknown, as above.
	case line == dline:
		rcol = d.col + uint(off-d.offset)
	default:
		rcol = col
	}
	return Position{Filename: d.name, Line: rline, Col: rcol}
}

// FileSet assigns each file a disjoint range of the Pos space.
//
// Adding a file and resolving a position are safe to do concurrently. An
// earlier version documented the opposite, that files are all added before any
// concurrent work starts, and that contract was wrong in two ways. It does not
// survive parsing files in parallel, which is what a compiler does. And a
// contract enforced by a comment is one the race detector finds violated in a
// test rather than in review, which is how this was found.
//
// The implementation appends under a lock and reads a snapshot with no lock.
// Entries are only ever appended and never mutated, so a reader that holds an
// older snapshot sees a prefix of the truth, and a Pos it can resolve is in
// that prefix by construction: a Pos exists only because AddFile returned the
// file that owns it, which happened before.
//
// Resolving positions is on the hot path, since every line table entry needs
// one, so the read side must not take a lock. Adding a file happens once per
// file.
type FileSet struct {
	mu    sync.Mutex // held only while appending
	base  Pos
	files atomic.Pointer[[]*SrcFile] // sorted by base; append-only
}

// NewFileSet returns an empty set. The first file starts at Pos 1, so that
// NoPos stays outside every file.
func NewFileSet() *FileSet {
	return &FileSet{base: 1}
}

// AddFile adds a file of the given size and returns its coordinate space.
//
// A file of size n occupies n+1 positions, so that the position one past the
// last byte is representable and belongs to this file rather than the next.
func (s *FileSet) AddFile(name string, size int) *SrcFile {
	if size < 0 {
		size = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.base == 0 {
		// A FileSet used as a zero value rather than through NewFileSet still
		// has to keep NoPos outside every file.
		s.base = 1
	}
	f := &SrcFile{
		set:   s,
		name:  name,
		base:  s.base,
		size:  size,
		lines: []int{0},
	}
	s.base += Pos(size) + 1

	// Copy on append. A reader holding the old slice keeps a valid one, which
	// is what makes the read side lock-free.
	old := s.snapshot()
	next := make([]*SrcFile, len(old), len(old)+1)
	copy(next, old)
	next = append(next, f)
	s.files.Store(&next)
	return f
}

// snapshot returns the current file list, which the caller must not modify.
func (s *FileSet) snapshot() []*SrcFile {
	if p := s.files.Load(); p != nil {
		return *p
	}
	return nil
}

// SrcFile returns the file containing p, or nil.
func (s *FileSet) SrcFile(p Pos) *SrcFile {
	files := s.snapshot()
	if p == NoPos || len(files) == 0 {
		return nil
	}
	i := sort.Search(len(files), func(i int) bool { return files[i].base > p })
	if i == 0 {
		return nil
	}
	f := files[i-1]
	if int(p-f.base) > f.size {
		return nil
	}
	return f
}

// Position resolves p with directives applied, for printing.
func (s *FileSet) Position(p Pos) Position {
	f := s.SrcFile(p)
	if f == nil {
		return Position{}
	}
	return f.Position(p)
}

// RawPosition resolves p ignoring directives, for comparison and for finding
// source text.
func (s *FileSet) RawPosition(p Pos) Position {
	f := s.SrcFile(p)
	if f == nil {
		return Position{}
	}
	return f.RawPosition(p)
}
