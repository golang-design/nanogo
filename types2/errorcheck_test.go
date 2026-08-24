// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package types2_test

import (
	"flag"
	"fmt"
	"go/build"
	"go/build/constraint"
	"go/scanner"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/syntax"
	. "golang.design/x/nanogo/types2"
)

// This file replaces the upstream check_test.go harness.
//
// The harness type-checks a package and compares the errors the checker
// reports against /* ERROR "pattern" */ annotations in the source. That corpus
// is the reason the fork is safe (specs/012-type-checking.md), so it comes
// across with the sources; the corpus itself is vendored verbatim under
// types2/upstream/testdata.
//
// The one piece that cannot come across is how the annotations are found.
// Upstream reads them with syntax.CommentMap, which re-scans the source with
// the syntax scanner and keeps the comments. nanogo's tree carries no comments
// and its scanner does not surface them, so the annotations are scanned here
// with go/scanner instead. The rule is upstream's: an annotation belongs to the
// position of the token in front of it.

// corpus is the vendored upstream test corpus. Every file in it carries a
// .txt suffix on top of its .go name, for the same reason the vendored sources
// do: the corpus contains Go that is deliberately malformed, and CI runs
// gofmt -l over the whole tree. See types2/upstream/README.md.
const corpus = "upstream/testdata"

// corpusSuffix is stripped from a corpus file name before the file is named to
// the checker, so that a reported position reads as an ordinary Go position.
const corpusSuffix = ".txt"

// sourceName is the name a corpus file is known by inside the checker.
func sourceName(path string) string { return strings.TrimSuffix(path, corpusSuffix) }

// knownGaps names the corpus files that do not pass yet, and the gap each one
// stands on. Nothing here is a disagreement with upstream about what is legal
// Go: every one of these files is a message or a position that differs, and
// each gap is in the syntax package rather than in the checker.
//
// 5 files of 374. The two gaps are:
//
//  1. Per-file language version. nanogo's syntax.File has no GoVersion field,
//     so a //go:build line that names a language version does not reach the
//     checker and every file takes Config.GoVersion.
//  2. Annotation scanning over invalid characters. The harness finds the ERROR
//     annotations with go/scanner, which splits an invalid rune and the name
//     after it into two tokens where the syntax scanner reports one. The
//     annotation then lands on the wrong column.
//
// A file leaves this table when the gap it names is closed, and testPkg fails
// the run if it does not: an entry that no longer reproduces is reported as
// stale rather than left to skip a test that would pass. See the report in
// specs/012-type-checking.md.
var knownGaps = map[string]string{
	"check/go1_19_20.go":      gapFileVersion,
	"check/go1_21_22.go":      gapFileVersion,
	"check/go1_22_21.go":      gapFileVersion,
	"fixedbugs/issue66064.go": gapFileVersion,
	"local/issue68183.go":     gapAnnotationScan,
}

const (
	gapFileVersion    = "syntax.File has no GoVersion field, so a //go:build language version does not reach the checker"
	gapAnnotationScan = "the ERROR annotations are scanned with go/scanner, which tokenizes an invalid rune differently from the syntax scanner"
)

var (
	haltOnError  = flag.Bool("halt", false, "halt on error")
	verifyErrors = flag.Bool("verify", false, "verify errors (rather than list them) in TestManual")
)

func TestCheck(t *testing.T) {
	DefPredeclaredTestFuncs()
	testDirFiles(t, filepath.Join(corpus, "check"), 50, false) // TODO(gri) narrow column tolerance
}

func TestSpec(t *testing.T) {
	testDirFiles(t, filepath.Join(corpus, "spec"), 20, false) // TODO(gri) narrow column tolerance
}

func TestExamples(t *testing.T) {
	testDirFiles(t, filepath.Join(corpus, "examples"), 125, false)
}

func TestFixedbugs(t *testing.T) {
	testDirFiles(t, filepath.Join(corpus, "fixedbugs"), 100, false)
}

func TestLocal(t *testing.T) {
	testDirFiles(t, filepath.Join(corpus, "local"), 0, false)
}

func testDirFiles(t *testing.T, dir string, colDelta uint, manual bool) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range ents {
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			testDir(t, path, colDelta, manual)
		} else {
			t.Run(filepath.Base(sourceName(path)), func(t *testing.T) {
				testPkg(t, path, []string{path}, colDelta, manual)
			})
		}
	}
}

func testDir(t *testing.T, dir string, colDelta uint, manual bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Error(err)
		return
	}
	var filenames []string
	for _, e := range ents {
		filenames = append(filenames, filepath.Join(dir, e.Name()))
	}
	t.Run(filepath.Base(dir), func(t *testing.T) {
		testPkg(t, dir, filenames, colDelta, manual)
	})
}

// testPkg checks one corpus package. key is the file or directory the package
// is named by, which is how knownGaps refers to it.
func testPkg(t *testing.T, key string, filenames []string, colDelta uint, manual bool) {
	fs := filenames[:0]
	srcs := make([][]byte, 0, len(filenames))
	for _, filename := range filenames {
		src, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("could not read %s: %v", filename, err)
		}
		if !shouldTest(src) {
			continue
		}
		fs = append(fs, filename)
		srcs = append(srcs, src)
	}
	if len(fs) == 0 {
		t.Skip("all files skipped by build tags")
	}

	problems, skip := checkFiles(t, fs, srcs, colDelta, manual)
	if skip != "" {
		t.Skip(skip)
	}

	rel := gapKey(key)
	why, isGap := knownGaps[rel]
	switch {
	case isGap && len(problems) == 0:
		// A gap that has been closed must leave the table. A stale entry
		// silently skips a test that would pass, which is how a list of known
		// gaps turns into a place to hide a bug.
		t.Errorf("known gap %s no longer reproduces; remove it from knownGaps (it named: %s)", rel, why)
	case isGap:
		t.Skip("known gap: " + why)
	default:
		for _, p := range problems {
			t.Error(p)
		}
	}
}

// gapKey is the name knownGaps refers to a corpus package by: the path
// relative to the corpus root, with the vendoring suffix removed.
func gapKey(path string) string {
	rel, err := filepath.Rel(corpus, sourceName(path))
	if err != nil {
		return filepath.ToSlash(sourceName(path))
	}
	return filepath.ToSlash(rel)
}

// TestKnownGapsNameRealFiles reports an entry in knownGaps that names nothing
// in the corpus.
//
// It reports and does not fail. An upstream refresh may drop a file, and the
// entry that named it is then noise rather than a fault; the run that removes
// the file is not the run to break. An entry whose file is present but no
// longer fails is a different matter, and testPkg fails on that.
func TestKnownGapsNameRealFiles(t *testing.T) {
	absent := 0
	for _, rel := range slices.Sorted(maps.Keys(knownGaps)) {
		if _, err := os.Stat(filepath.Join(corpus, rel+corpusSuffix)); err == nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(corpus, rel)); err == nil {
			continue // a package directory
		}
		t.Logf("known gap %s names nothing in the corpus", rel)
		absent++
	}
	if absent > 0 {
		t.Logf("%d of %d known gaps name nothing in the corpus", absent, len(knownGaps))
	}
}

// shouldTest checks build tags in src and returns whether the file
// should be tested according to the tags.
func shouldTest(src []byte) bool {
	match := func(tag string) bool {
		// We only care GOOS, GOARCH, and go version tags.
		if slices.Contains(build.Default.ReleaseTags, tag) {
			return true
		}
		return tag == runtime.GOOS || tag == runtime.GOARCH
	}
	for line := range strings.SplitSeq(string(src), "\n") {
		if strings.HasPrefix(line, "package ") {
			break
		}
		if expr, err := constraint.Parse(line); err == nil {
			return expr.Eval(match)
		}
	}
	return true
}

// parseFlags parses flags from the first line of the given source if the line
// starts with "//" (line comment) followed by "-" (possibly with spaces
// between). Otherwise the line is ignored.
func parseFlags(src []byte, flags *flag.FlagSet) error {
	const prefix = "//"
	if !strings.HasPrefix(string(src), prefix) {
		return nil // first line is not a line comment
	}
	s := string(src)[len(prefix):]
	if i := strings.Index(s, "-"); i < 0 || len(strings.TrimSpace(s[:i])) != 0 {
		return nil // comment doesn't start with a "-"
	}
	end := strings.Index(s, "\n")
	const maxLen = 256
	if end < 0 || end > maxLen {
		return errTooLong
	}
	return flags.Parse(strings.Fields(s[:end]))
}

var errTooLong = errorString("flags comment line too long")

type errorString string

func (e errorString) Error() string { return string(e) }

// testFiles type-checks the package consisting of the given files, compares
// the resulting errors with the ERROR annotations in the source, and reports
// every mismatch.
//
// If provided, opts may be used to mutate the Config before type-checking.
func testFiles(t *testing.T, filenames []string, srcs [][]byte, colDelta uint, manual bool, opts ...func(*Config)) {
	t.Helper()
	problems, skip := checkFiles(t, filenames, srcs, colDelta, manual, opts...)
	if skip != "" {
		t.Skip(skip)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// checkFiles is testFiles without the reporting: it returns the mismatches
// instead of failing the test, and returns a non-empty skip reason instead of
// skipping.
//
// The corpus driver needs the mismatches in hand rather than reported, because
// a file listed in knownGaps must be told apart from one that has started
// passing. A gap that no longer reproduces is a fault, and it can only be seen
// by running the file and finding nothing wrong with it.
//
// Infrastructure failures, such as a file that cannot be read, stay fatal.
func checkFiles(t *testing.T, filenames []string, srcs [][]byte, colDelta uint, manual bool, opts ...func(*Config)) (problems []string, skip string) {
	t.Helper()
	if len(filenames) == 0 {
		t.Fatal("no source files")
	}

	// parse files
	fset := syntax.NewFileSet()
	var files []*syntax.File
	var errlist []error
	errh := func(err syntax.Error) { errlist = append(errlist, err) }
	for i, filename := range filenames {
		f := fset.AddFile(sourceName(filename), len(srcs[i]))
		file, _ := syntax.Parse(f, srcs[i], errh, nil, 0)
		if file == nil {
			t.Fatalf("%s: could not parse", filename)
		}
		files = append(files, file)
	}

	pkgName := "<no package>"
	if len(files) > 0 {
		pkgName = files[0].PkgName.Value
	}
	listErrors := manual && !*verifyErrors
	if listErrors && len(errlist) > 0 {
		t.Errorf("--- %s:", pkgName)
		for _, err := range errlist {
			t.Error(err)
		}
	}

	// set up typechecker
	var conf Config
	conf.Fset = fset
	conf.Trace = manual && testing.Verbose()
	conf.Importer = newSrcImporter(fset)
	conf.Error = func(err error) {
		if *haltOnError {
			defer panic(err)
		}
		if listErrors {
			t.Error(err)
			return
		}
		errlist = append(errlist, err)
	}

	// apply custom configuration
	for _, opt := range opts {
		opt(&conf)
	}

	// apply flag setting (overrides custom configuration)
	var goexperiment string
	flags := flag.NewFlagSet("", flag.PanicOnError)
	flags.StringVar(&conf.GoVersion, "lang", "", "")
	flags.StringVar(&goexperiment, "goexperiment", "", "")
	flags.BoolVar(&conf.FakeImportC, "fakeImportC", false, "")
	if err := parseFlags(srcs[0], flags); err != nil {
		t.Fatal(err)
	}
	if goexperiment != "" {
		// nanogo has no GOEXPERIMENT machinery (specs/051). A test that needs
		// one is not run rather than run under the wrong language rules.
		return nil, fmt.Sprintf("test needs GOEXPERIMENT=%s, which nanogo does not have", goexperiment)
	}

	// Provide Config.Info with all maps so that info recording is tested.
	info := Info{
		Types:        make(map[syntax.Expr]TypeAndValue),
		Instances:    make(map[*syntax.Name]Instance),
		Defs:         make(map[*syntax.Name]Object),
		Uses:         make(map[*syntax.Name]Object),
		Implicits:    make(map[syntax.Node]Object),
		Selections:   make(map[*syntax.SelectorExpr]*Selection),
		Scopes:       make(map[syntax.Node]*Scope),
		FileVersions: make(map[*syntax.SrcFile]string),
	}

	conf.Check(pkgName, files, &info)
	if listErrors {
		return nil, ""
	}

	// collect expected errors
	errmap := make(map[string]map[int][]expectedError)
	for i, filename := range filenames {
		if m := errorAnnotations(sourceName(filename), srcs[i]); len(m) > 0 {
			errmap[sourceName(filename)] = m
		}
	}

	// match against found errors
	var indices []int // list indices of matching errors, reused for each error
	for _, err := range errlist {
		gotPos, gotMsg := unpackError(fset, err)

		filemap := errmap[gotPos.Filename]
		var errList []expectedError
		if filemap != nil {
			errList = filemap[int(gotPos.Line)]
		}

		// At least one of the errors in errList should match the current error.
		indices = indices[:0]
		for i, want := range errList {
			if want.regexp {
				rx, err := regexp.Compile(want.pattern)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s:%d:%d: %v", gotPos.Filename, want.line, want.col, err))
					continue
				}
				if !rx.MatchString(gotMsg) {
					continue
				}
			} else if !strings.Contains(gotMsg, want.pattern) {
				continue
			}
			indices = append(indices, i)
		}
		if len(indices) == 0 {
			problems = append(problems, fmt.Sprintf("%s: no error expected: %q", gotPos, gotMsg))
			continue
		}

		// If there are multiple matching errors, select the one with the
		// closest column position.
		index := -1
		var delta uint
		for _, i := range indices {
			if d := absDiff(uint(gotPos.Col), uint(errList[i].col)); index < 0 || d < delta {
				index, delta = i, d
			}
		}

		if delta > colDelta {
			problems = append(problems, fmt.Sprintf("%s: got col = %d; want %d", gotPos, gotPos.Col, errList[index].col))
		}

		// eliminate from errList
		line := int(gotPos.Line)
		if n := len(errList) - 1; n > 0 {
			copy(errList[index:], errList[index+1:])
			filemap[line] = errList[:n]
		} else {
			delete(filemap, line)
		}
		if len(filemap) == 0 {
			delete(errmap, gotPos.Filename)
		}
	}

	// there should be no expected errors left
	if len(errmap) > 0 {
		problems = append(problems, fmt.Sprintf("--- %s: unreported errors:", pkgName))
		for filename, filemap := range errmap {
			for line, errList := range filemap {
				for _, err := range errList {
					problems = append(problems, fmt.Sprintf("%s:%d:%d: %s", filename, line, err.col, err.pattern))
				}
			}
		}
	}

	return problems, ""
}

// absDiff returns the absolute difference between x and y.
func absDiff(x, y uint) uint {
	if x < y {
		return y - x
	}
	return x - y
}

// unpackError returns the position and message of err.
func unpackError(fset *syntax.FileSet, err error) (syntax.Position, string) {
	switch err := err.(type) {
	case syntax.Error:
		return fset.Position(err.Pos), err.Msg
	case Error:
		return err.Position, err.Msg
	default:
		return syntax.Position{}, err.Error()
	}
}

// expectedError is one ERROR or ERRORx annotation.
type expectedError struct {
	line, col int    // position of the token the annotation belongs to
	pattern   string // the unquoted pattern
	regexp    bool   // the annotation was ERRORx, so pattern is a regular expression
}

var errorRx = regexp.MustCompile(`^ ERROR(x?) `)

// errorAnnotations scans the ERROR and ERRORx comments out of src.
//
// The annotation belongs to the token in front of the comment, which is the
// rule upstream's syntax.CommentMap applies. Automatically inserted semicolons
// are not tokens for this purpose, so a comment at the end of a line belongs to
// the last token written on it.
//
// go/scanner does the tokenizing because nanogo's scanner does not surface
// comments and its tree does not carry them. This is test-only source, so
// depending on the standard library here costs nothing.
func errorAnnotations(filename string, src []byte) map[int][]expectedError {
	fset := token.NewFileSet()
	file := fset.AddFile(filename, -1, len(src))

	var s scanner.Scanner
	s.Init(file, src, nil, scanner.ScanComments)

	res := make(map[int][]expectedError)
	prev := token.Position{Line: 1, Column: 1}
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT {
			text := lit
			if strings.HasPrefix(text, "/*") {
				text = text[2 : len(text)-2]
			} else {
				text = text[2:]
			}
			m := errorRx.FindStringSubmatch(text)
			if m == nil {
				continue
			}
			pattern, err := strconv.Unquote(strings.TrimSpace(text[len(m[0]):]))
			if err != nil {
				continue // an unquotable pattern is reported as an unmatched expectation
			}
			res[prev.Line] = append(res[prev.Line], expectedError{
				line:    prev.Line,
				col:     prev.Column,
				pattern: pattern,
				regexp:  m[1] == "x",
			})
			continue
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue // automatically inserted, not a token in the source
		}
		prev = fset.Position(pos)
	}
	if len(res) == 0 {
		return nil
	}
	return res
}
