// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// A Class is what happened to one corpus file.
//
// The set is exhaustive and every file lands in exactly one member, because
// the totals must add up to the corpus size. A corpus test that returns
// silently on the file it cannot handle produces a number that can only rise,
// and this repository has been caught by that before.
type Class string

const (
	// ClassMatched: nanogo compiled the program, it ran, and its exit
	// status and output are gc's. This is the only outcome that proves
	// anything about code generation.
	ClassMatched Class = "matched"

	// ClassMismatched: nanogo compiled the program and it behaved
	// differently from gc's build of the same source. A miscompilation, and
	// the most valuable thing this corpus can find.
	ClassMismatched Class = "mismatched"

	// ClassCompiled: a "compile" recipe that nanogo built. It was not run,
	// because the recipe does not ask for it to be.
	ClassCompiled Class = "compiled"

	// ClassRejected: an "errorcheck" recipe that nanogo rejected at the
	// annotated position.
	ClassRejected Class = "rejected"

	// ClassWrongPosition: an "errorcheck" recipe nanogo rejected somewhere
	// other than the annotated position. A front-end bug, not a gap.
	ClassWrongPosition Class = "wrong-position"

	// ClassMissed: an "errorcheck" recipe nanogo accepted. A gap in the
	// checker, and the checker's own progress metric.
	ClassMissed Class = "missed"

	// ClassRefused: nanogo declined by name, saying which construct it
	// cannot compile. Not a failure. Most of the corpus lands here today
	// and the ranked breakdown of the reasons is what says what to build
	// next.
	ClassRefused Class = "refused"

	// ClassFalseError: nanogo reported an ordinary compile error in a
	// program gc accepts. A front-end bug: nanogo rejected legal Go.
	ClassFalseError Class = "false-error"

	// ClassCrashed: nanogo failed in a way that is not a refusal and not a
	// diagnostic, such as a panic. A bug, reported with its message.
	ClassCrashed Class = "crashed"

	// ClassTimedOut: nanogo built the program and the program did not
	// finish. Either an infinite loop nanogo generated, or a corpus program
	// slower than the budget.
	ClassTimedOut Class = "timed-out"

	// ClassOracleFailed: gc could not build or run the file, so there is no
	// expectation to compare against. It says nothing about nanogo.
	ClassOracleFailed Class = "oracle-failed"

	// ClassPlatformExcluded: the file's build constraints exclude this
	// host, so neither compiler is asked about it.
	ClassPlatformExcluded Class = "platform-excluded"

	// ClassNoRecipe: the file's first comment states nothing this package
	// can read. One file in the corpus is like this and it must still be
	// counted.
	ClassNoRecipe Class = "no-recipe"

	// ClassKindNotImplemented: a recipe kind this harness does not carry
	// out, such as the directory kinds. Counted and named, never silent.
	ClassKindNotImplemented Class = "kind-not-implemented"

	// ClassRecipeSaysSkip: the corpus itself says not to run the file. Its
	// recipe word is "skip", and the Go distribution's own driver does not
	// run it either. It is not a gap in nanogo and must not be counted as
	// one.
	ClassRecipeSaysSkip Class = "recipe-says-skip"

	// ClassRecipeNotImplemented: a kind this harness does carry out, with a
	// flag it cannot honour. nanogo has no -gcflags, so a recipe that asks
	// for one asks for a test nanogo cannot be given.
	ClassRecipeNotImplemented Class = "recipe-not-implemented"
)

// Classes is every class, in report order. The order is fixed so that two runs
// print the same table (specs/053-determinism.md).
var Classes = []Class{
	ClassMatched, ClassCompiled, ClassRejected,
	ClassMismatched, ClassWrongPosition, ClassMissed,
	ClassFalseError, ClassCrashed, ClassTimedOut,
	ClassRefused,
	ClassOracleFailed, ClassPlatformExcluded,
	ClassNoRecipe, ClassKindNotImplemented, ClassRecipeNotImplemented,
	ClassRecipeSaysSkip,
}

// IsPass reports whether a class is one the ratchet records.
//
// A pass is an outcome that proves nanogo did the thing the recipe asked for.
// A refusal is not one: it is honest, it is expected, and recording it would
// freeze a gap in place.
func (c Class) IsPass() bool {
	return c == ClassMatched || c == ClassCompiled || c == ClassRejected
}

// IsFailure reports whether a class fails the build on its own, with no
// ratchet involved.
//
// Only a mismatch does. Everything else is either a pass, an honest refusal,
// or a gap that the ratchet catches when it moves backwards.
func (c Class) IsFailure() bool { return c == ClassMismatched }

// Refusal detection.
//
// nanogo's driver has three ways to fail and they mean different things:
//
//   - "nanogo cannot compile X: Y" is [driver.UnsupportedError]. A construct
//     nanogo has no answer for. Expected, and the reason is the deliverable.
//   - "nanogo: file:line:col: message" is a diagnostic. In a program gc
//     accepts, that is nanogo rejecting legal Go, which is a bug.
//   - anything else, a panic above all, is a crash.
var (
	unsupportedMark = "nanogo cannot compile "

	// A diagnostic, with or without nanogo's own prefix. nanogo resolves
	// the package graph by asking the go command, so a file the go command
	// itself cannot parse comes back as go list's diagnostic, unprefixed
	// and on its own line. The position is nanogo's judgement either way.
	diagnosticRe = regexp.MustCompile(`(?m)^(?:nanogo: )?(?:\./)?([^\s:]+\.go):(\d+):(\d+): `)

	crashMarks = []string{"panic: ", "fatal error: ", "goroutine 1 [running]:"}
)

// IsCrash reports whether nanogo's output is a crash rather than a refusal or
// a diagnostic.
func IsCrash(out string) bool {
	for _, m := range crashMarks {
		if strings.Contains(out, m) {
			return true
		}
	}
	return false
}

// IsRefusal reports whether nanogo's output names a construct it cannot
// compile.
func IsRefusal(out string) bool { return strings.Contains(out, unsupportedMark) }

// FirstErrorLine returns the line number of the first diagnostic in nanogo's
// output, and whether there was one.
func FirstErrorLine(out string) (int, bool) {
	m := diagnosticRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, false
	}
	return n, true
}

// Reason turns nanogo's refusal into a key that groups equal refusals
// together.
//
// The stage chain is kept, because "which stage refuses" is half of what a
// contributor needs. What is removed is everything that varies between two
// files refused for the same reason: the position, the name of the function or
// variable the refusal is about, the SSA value number, and absolute paths.
//
// Removing the name matters more than it looks. nanogo's refusal repeats the
// function's name inside the stage chain, so without this every file would be
// its own bucket and the ranked list this package exists to produce would be a
// list of ones.
func Reason(out string) string {
	i := strings.Index(out, unsupportedMark)
	if i < 0 {
		return ""
	}
	s := out[i+len(unsupportedMark):]
	if j := strings.IndexByte(s, '\n'); j >= 0 {
		s = s[:j]
	}
	if m := constructRe.FindStringSubmatch(s); m != nil {
		// The construct's own name, wherever the message repeats it.
		s = wordRe(m[2]).ReplaceAllString(s, "NAME")
	}
	return normalizeReason(s)
}

// constructRe matches the "what" of a refusal: the kind of construct nanogo
// declined, and its name.
var constructRe = regexp.MustCompile(`^(package-level variable|function|method|variable) ([^\s:]+)`)

// wordRe matches one identifier, whole, so that replacing a function named "f"
// does not rewrite every f inside another word.
func wordRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
}

// The variable parts of a refusal, and what each is replaced by. Every
// substitution is here rather than spread through the code, so that a reader
// can see exactly how much two refusals in one bucket are allowed to differ.
var reasonSubs = []struct {
	re   *regexp.Regexp
	with string
}{
	// " at /some/where/file.go:12:3" -- the position of the construct.
	{regexp.MustCompile(` at \S+\.go:\d+:\d+`), ""},
	// A defined type in the program under test. Two files refused because
	// their own type may have methods are refused for one reason.
	{regexp.MustCompile(`\b(main|p|a|b)\.[A-Za-z_]\w*\b`), "PKG.TYPE"},
	// SSA value numbers and value indices.
	{regexp.MustCompile(`\bv\d+\b`), "vN"},
	{regexp.MustCompile(`\bvalue \d+\b`), "value N"},
	// Any absolute path left over.
	{regexp.MustCompile(`/\S+/([\w.-]+\.go)`), "$1"},
	// The name substitution leaves "NAME: NAME:" where the stage chain
	// repeated it.
	{regexp.MustCompile(`(NAME: )+NAME:`), "NAME:"},
	{regexp.MustCompile(`\s+`), " "},
}

func normalizeReason(s string) string {
	for _, sub := range reasonSubs {
		s = sub.re.ReplaceAllString(s, sub.with)
	}
	return strings.TrimSpace(s)
}

// Output comparison.
//
// A program's behaviour is its exit status, its standard output and its
// standard error. The first two are compared byte for byte. The third is
// compared after the parts that differ between two runs of one binary are
// removed: a panic traceback carries pointer values and goroutine numbers, and
// the log package prefixes every line with the wall clock. None of them is
// behaviour, and the two builds are run one after the other, so a run that
// crosses a second boundary would otherwise read as a miscompilation.
var stderrSubs = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`0x[0-9a-fA-F]+`), "0xADDR"},
	// The log package's default prefix, which is the date and the time.
	// test/linkobj.go and test/linkmain_run.go both reach log.Fatal.
	{regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `), "TIME "},
	// A temporary directory the program made for itself. os.MkdirTemp picks a
	// random name, so it differs between two runs of one binary.
	// test/linkmain_run.go prints the command line it runs, and the command
	// line names the directory. The root is os.TempDir() rather than a guess
	// about what a path looks like, and everything under it is kept.
	{tempDirRe, "TMPDIR"},
	{regexp.MustCompile(`goroutine \d+`), "goroutine N"},
	{regexp.MustCompile(`(?m)^\t.*:\d+ \+0xADDR$`), "\tFRAME"},
	{regexp.MustCompile(`\bpc=0xADDR\b`), "pc=0xADDR"},
	{regexp.MustCompile(`(?m)^exit status \d+$`), ""},
}

// tempDirRe matches one directory made directly under the system's temporary
// directory, which is where os.MkdirTemp puts one.
var tempDirRe = regexp.MustCompile(
	regexp.QuoteMeta(strings.TrimRight(os.TempDir(), string(os.PathSeparator))) + `[/\\][^\s/\\]+`)

// NormalizeStderr removes the parts of a program's standard error that differ
// between two runs of the same program.
func NormalizeStderr(s string) string {
	for _, sub := range stderrSubs {
		s = sub.re.ReplaceAllString(s, sub.with)
	}
	return strings.TrimRight(s, "\n")
}

// ErrorAnnotations returns the line numbers an errorcheck file annotates, in
// order.
//
// The corpus marks an expected error with "// ERROR" at the end of the line it
// is expected on. GC_ERROR is the same thing where gc and gccgo disagree;
// GCCGO_ERROR and ERRORAUTO are not nanogo's business and are ignored.
func ErrorAnnotations(src []byte) []int {
	var lines []int
	for i, line := range strings.Split(string(src), "\n") {
		if annotationRe.MatchString(line) {
			lines = append(lines, i+1)
		}
	}
	return lines
}

var annotationRe = regexp.MustCompile(`//\s*(GC_)?ERROR[\s"]`)
