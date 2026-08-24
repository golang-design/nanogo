// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import (
	"go/token"
	"strings"
	"testing"
)

// addLinesFor fills a file's line table the way the scanner would.
func addLinesFor(f *SrcFile, src string) {
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			f.AddLine(i + 1)
		}
	}
}

func TestPosNoPosIsUnknown(t *testing.T) {
	if NoPos.IsKnown() {
		t.Fatal("NoPos reports as known")
	}
	if !Pos(1).IsKnown() {
		t.Fatal("Pos(1) reports as unknown")
	}
	var p Position
	if p.IsKnown() {
		t.Fatal("the zero Position reports as known")
	}
	if got := p.String(); got != "<unknown position>" {
		t.Fatalf("zero Position prints %q", got)
	}
}

func TestPositionString(t *testing.T) {
	for _, tc := range []struct {
		in   Position
		want string
	}{
		{Position{"a.go", 3, 7}, "a.go:3:7"},
		{Position{"a.go", 3, 0}, "a.go:3"},
		{Position{"", 3, 7}, "3:7"},
		{Position{"a.go", 0, 0}, "a.go"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Position%v.String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPosAgreesWithGoToken is the property that matters: nanogo's line and
// column arithmetic must produce exactly what go/token produces, because the
// errorcheck corpus of specs/004-conformance.md is annotated against it.
func TestPosAgreesWithGoToken(t *testing.T) {
	const src = "package p\n\nfunc f() {\n\tx := 1\n\t_ = x\n}\n\n// trailing\n"

	fset := NewFileSet()
	f := fset.AddFile("a.go", len(src))
	addLinesFor(f, src)

	gofset := token.NewFileSet()
	gof := gofset.AddFile("a.go", -1, len(src))
	gof.SetLinesForContent([]byte(src))

	for off := 0; off <= len(src); off++ {
		got := fset.RawPosition(f.Pos(off))
		want := gofset.Position(gof.Pos(off))
		if got.Line != uint(want.Line) || got.Col != uint(want.Column) {
			t.Fatalf("offset %d: nanogo %d:%d, go/token %d:%d",
				off, got.Line, got.Col, want.Line, want.Column)
		}
	}
}

// TestPosColumnsCountBytes pins the decision in specs/010: columns are byte
// counts, not rune counts. A rune count would be correct in a different way and
// would disagree with every errorcheck annotation in the corpus.
func TestPosColumnsCountBytes(t *testing.T) {
	const src = "package p // äöü x\n"
	fset := NewFileSet()
	f := fset.AddFile("a.go", len(src))
	addLinesFor(f, src)

	off := strings.IndexByte(src, 'x')
	got := fset.RawPosition(f.Pos(off))
	if got.Col != uint(off)+1 {
		t.Fatalf("column %d, want %d: columns must count bytes", got.Col, off+1)
	}
}

func TestSrcFileBounds(t *testing.T) {
	fset := NewFileSet()
	f := fset.AddFile("a.go", 10)

	if got := f.Pos(-5); got != f.Base() {
		t.Errorf("Pos(-5) = %d, want the base %d: an offset before the file clamps", got, f.Base())
	}
	if got := f.Pos(99); got != f.Base()+10 {
		t.Errorf("Pos(99) = %d, want %d: an offset past the end clamps to the end", got, f.Base()+10)
	}
	if got := f.Offset(f.Base() - 1); got != 0 {
		t.Errorf("Offset before the base = %d, want 0", got)
	}
	if got := f.Offset(f.Base() + 99); got != 10 {
		t.Errorf("Offset past the end = %d, want 10", got)
	}
	if f.Name() != "a.go" || f.Size() != 10 {
		t.Errorf("Name/Size = %q/%d", f.Name(), f.Size())
	}
	if fset.AddFile("b.go", -1).Size() != 0 {
		t.Error("a negative size was not clamped to zero")
	}
}

// TestSrcFileAddLineIgnoresGoingBackwards guards the table against a double
// call, which would otherwise corrupt the binary search.
func TestSrcFileAddLineIgnoresGoingBackwards(t *testing.T) {
	fset := NewFileSet()
	f := fset.AddFile("a.go", 100)
	f.AddLine(10)
	f.AddLine(10) // repeated
	f.AddLine(5)  // backwards
	f.AddLine(20)
	f.AddLine(1000) // past the end
	if got, want := len(f.lines), 3; got != want {
		t.Fatalf("line table has %d entries, want %d: %v", got, want, f.lines)
	}
}

func TestFileSetSeparatesFiles(t *testing.T) {
	fset := NewFileSet()
	a := fset.AddFile("a.go", 5)
	b := fset.AddFile("b.go", 5)

	if fset.SrcFile(a.Pos(0)) != a {
		t.Error("a position in a resolved to another file")
	}
	// A file of size n owns n+1 positions, so one past the last byte still
	// belongs to it and not to the next file.
	if fset.SrcFile(a.Pos(5)) != a {
		t.Error("the position one past the end of a did not belong to a")
	}
	if fset.SrcFile(b.Pos(0)) != b {
		t.Error("a position in b resolved to another file")
	}
	if fset.SrcFile(NoPos) != nil {
		t.Error("NoPos resolved to a file")
	}
	if fset.SrcFile(Pos(1<<20)) != nil {
		t.Error("a position past every file resolved to one")
	}
	if (&FileSet{}).SrcFile(Pos(1)) != nil {
		t.Error("an empty set resolved a position")
	}
	if got := fset.Position(Pos(1 << 20)); got.IsKnown() {
		t.Error("Position of an unowned Pos reported as known")
	}
	if got := fset.RawPosition(Pos(1 << 20)); got.IsKnown() {
		t.Error("RawPosition of an unowned Pos reported as known")
	}
}

// TestLineDirectiveRewritesReportedNotRaw is the distinction specs/010 draws
// and the one most easily lost: comparison uses raw, printing uses reported.
func TestLineDirectiveRewritesReportedNotRaw(t *testing.T) {
	// Line 1 is the package clause, line 2 holds the directive, and line 3 is
	// the first line the directive governs.
	const src = "package p\n//line gen.go:100\nvar x = 1\nvar y = 2\n"

	fset := NewFileSet()
	f := fset.AddFile("a.go", len(src))
	addLinesFor(f, src)
	f.AddLineDirective(strings.Index(src, "var x"), "gen.go", 100, 0)

	xoff := strings.Index(src, "var x")
	yoff := strings.Index(src, "var y")

	if got := f.RawPosition(f.Pos(xoff)); got.Filename != "a.go" || got.Line != 3 {
		t.Errorf("raw position of x is %v, want a.go:3", got)
	}
	if got := f.Position(f.Pos(xoff)); got.Filename != "gen.go" || got.Line != 100 {
		t.Errorf("reported position of x is %v, want gen.go:100", got)
	}
	if got := f.Position(f.Pos(yoff)); got.Filename != "gen.go" || got.Line != 101 {
		t.Errorf("reported position of y is %v, want gen.go:101", got)
	}
	// A position before the directive is untouched.
	if got := f.Position(f.Pos(0)); got.Filename != "a.go" || got.Line != 1 {
		t.Errorf("reported position before the directive is %v, want a.go:1", got)
	}
}

func TestLineDirectiveColumnAppliesToTheFirstLineOnly(t *testing.T) {
	const src = "package p\n//line gen.go:100:5\nvar x = 1\nvar y = 2\n"
	fset := NewFileSet()
	f := fset.AddFile("a.go", len(src))
	addLinesFor(f, src)
	xoff := strings.Index(src, "var x")
	f.AddLineDirective(xoff, "gen.go", 100, 5)

	if got := f.Position(f.Pos(xoff)); got.Col != 5 {
		t.Errorf("column on the governed line is %d, want 5", got.Col)
	}
	yoff := strings.Index(src, "var y")
	if got := f.Position(f.Pos(yoff)); got.Col != 1 {
		t.Errorf("column on the next line is %d, want 1: the column applies to the first line only", got.Col)
	}
}

// TestLineDirectiveWithoutNameReportsNoName pins a rule that was got wrong by
// reasoning and right by asking both oracles.
//
// `//line :200` does not mean "keep the filename and change the line". It
// means the filename is empty. For a file that already carries
// `//line gen.go:100`, `go tool compile` reports `:200` and go/scanner reports
// a position with no filename. Neither reports `gen.go:200`.
func TestLineDirectiveWithoutNameReportsNoName(t *testing.T) {
	const src = "package p\n//line gen.go:100\nvar x = 1\n//line :200\nvar y = 2\n"
	fset := NewFileSet()
	f := fset.AddFile("a.go", len(src))
	addLinesFor(f, src)
	f.AddLineDirective(strings.Index(src, "var x"), "gen.go", 100, 0)
	f.AddLineDirective(strings.Index(src, "var y"), "", 200, 0)

	if got := f.Position(f.Pos(strings.Index(src, "var y"))); got.Filename != "" || got.Line != 200 {
		t.Errorf("reported position is %#v, want an empty filename at line 200", got)
	}
}

func TestLineDirectiveWithoutNameOrPredecessorReportsNoName(t *testing.T) {
	const src = "package p\n//line :50\nvar x = 1\n"
	fset := NewFileSet()
	f := fset.AddFile("a.go", len(src))
	addLinesFor(f, src)
	f.AddLineDirective(strings.Index(src, "var x"), "", 50, 0)
	if got := f.Position(f.Pos(strings.Index(src, "var x"))); got.Filename != "" || got.Line != 50 {
		t.Errorf("reported position is %#v, want an empty filename at line 50", got)
	}
}

// TestLineDirectiveZeroLineIsIgnored guards the rest of the file. The
// specification requires a positive line number, and accepting zero would make
// every position after the directive unknown.
func TestLineDirectiveZeroLineIsIgnored(t *testing.T) {
	fset := NewFileSet()
	f := fset.AddFile("a.go", 100)
	f.AddLineDirective(10, "gen.go", 0, 0)
	if len(f.directives) != 0 {
		t.Fatal("a directive with line 0 was recorded")
	}
}

func TestFileWithNoLineTableStillResolves(t *testing.T) {
	// A file whose scanner never ran has no line table beyond the implicit
	// first line. Resolving must not divide by zero or search an empty slice.
	f := &SrcFile{name: "a.go", base: 1, size: 10}
	if got := f.RawPosition(f.Pos(4)); got.Line != 1 || got.Col != 5 {
		t.Fatalf("got %v, want a.go:1:5", got)
	}
}

// TestMidLineDirectiveColumn is the case that a //line comment cannot produce
// and a /*line*/ comment can: the directive ends in the middle of a line, so
// the column it asserts is measured from its own offset and not from the start
// of the line.
//
// Three files in the Go distribution's test corpus depend on this, and an
// earlier version of Position got it wrong in a way the scanner could not
// compensate for: the compensation underflows.
func TestMidLineDirectiveColumn(t *testing.T) {
	const src = "package p\nvar x = /*line gen.go:7:3*/ 1 + 2\nvar y = 3\n"
	fset := NewFileSet()
	f := fset.AddFile("a.go", len(src))
	addLinesFor(f, src)

	// The directive governs the byte just after its closing delimiter.
	governed := strings.Index(src, "*/") + len("*/")
	f.AddLineDirective(governed, "gen.go", 7, 3)

	// The governed offset itself is exactly the asserted position.
	if got := f.Position(f.Pos(governed)); got.Filename != "gen.go" || got.Line != 7 || got.Col != 3 {
		t.Errorf("the governed offset reports %v, want gen.go:7:3", got)
	}

	// A position further along the same line is that many bytes further right.
	one := strings.Index(src, "1 + 2")
	wantCol := uint(3 + (one - governed))
	if got := f.Position(f.Pos(one)); got.Line != 7 || got.Col != wantCol {
		t.Errorf("the literal reports %v, want gen.go:7:%d", got, wantCol)
	}

	// The next line restarts at column 1, because the column applies to the
	// governed line only.
	next := strings.Index(src, "var y")
	if got := f.Position(f.Pos(next)); got.Line != 8 || got.Col != 1 {
		t.Errorf("the next line reports %v, want gen.go:8:1", got)
	}
}

// TestMidLineDirectiveMatchesGoToken checks the rule against the standard
// library rather than against my own arithmetic, since the arithmetic is what
// was wrong before.
func TestMidLineDirectiveMatchesGoToken(t *testing.T) {
	const src = "package p\nvar x = /*line gen.go:7:3*/ 1 + 2\n"

	fset := NewFileSet()
	f := fset.AddFile("a.go", len(src))
	addLinesFor(f, src)
	governed := strings.Index(src, "*/") + len("*/")
	f.AddLineDirective(governed, "gen.go", 7, 3)

	gofset := token.NewFileSet()
	gof := gofset.AddFile("a.go", -1, len(src))
	gof.SetLinesForContent([]byte(src))
	gof.AddLineColumnInfo(governed, "gen.go", 7, 3)

	// Compare every offset on the governed line.
	lineEnd := strings.IndexByte(src[governed:], '\n') + governed
	for off := governed; off < lineEnd; off++ {
		got := f.Position(f.Pos(off))
		want := gofset.Position(gof.Pos(off))
		if got.Filename != want.Filename || got.Line != uint(want.Line) || got.Col != uint(want.Column) {
			t.Fatalf("offset %d: nanogo %v, go/token %s", off, got, want)
		}
	}
}

// TestDirectiveColumnRulesMatchTheReferenceCompiler pins the three rules
// against what `go tool compile` actually prints, because they were got wrong
// twice by reasoning and right once by measuring.
//
//	//line gen.go:100      -> gen.go:100      then gen.go:101
//	//line gen.go:100:5    -> gen.go:100:17   then gen.go:101:13
func TestDirectiveColumnRulesMatchTheReferenceCompiler(t *testing.T) {
	build := func(directive string) (*SrcFile, string) {
		src := "package p\n\n" + directive + "\nvar y int = 1\nvar z int = \"nope\"\n"
		fset := NewFileSet()
		f := fset.AddFile("bad.go", len(src))
		addLinesFor(f, src)
		return f, src
	}

	// Without a column, no column is reported on any governed line.
	f, src := build("//line gen.go:100")
	f.AddLineDirective(strings.Index(src, "var y"), "gen.go", 100, 0)
	if got := f.Position(f.Pos(strings.Index(src, `"nope"`))); got.String() != "gen.go:101" {
		t.Errorf("no-column directive reports %q, want %q", got.String(), "gen.go:101")
	}

	// With a column, the governed line measures from the directive and later
	// lines use their ordinary column.
	f, src = build("//line gen.go:100:5")
	governed := strings.Index(src, "var y")
	f.AddLineDirective(governed, "gen.go", 100, 5)

	// "var y int = 1": the literal is at raw column 13, so 5 + 12 = 17.
	one := strings.Index(src, "= 1") + 2
	if got := f.Position(f.Pos(one)); got.String() != "gen.go:100:17" {
		t.Errorf("governed line reports %q, want %q", got.String(), "gen.go:100:17")
	}
	if got := f.Position(f.Pos(strings.Index(src, `"nope"`))); got.String() != "gen.go:101:13" {
		t.Errorf("later line reports %q, want %q", got.String(), "gen.go:101:13")
	}
}
