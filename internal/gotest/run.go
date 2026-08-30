// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package gotest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.design/x/nanogo/loader"
)

// Options configure a sweep of the corpus.
type Options struct {
	// Corpus is the directory holding the vendored files.
	Corpus string

	// Work is a scratch directory. Each file gets a subdirectory of its own
	// so that both compilers see the source at the same path: a corpus
	// program can print its own position through runtime.Caller, and two
	// build directories would produce a difference that is not a
	// miscompilation.
	Work string

	// Nanogo and Go are the two compilers. Go is the oracle.
	Nanogo string
	Go     string

	// Cache is one GOCACHE for the whole sweep.
	//
	// This is the opposite of internal/e2e, which gives every test a cache
	// of its own so that the go command cannot replay an object a previous
	// run left behind. Here the risk does not exist: every corpus file is
	// different source, so nothing nanogo compiles is ever a cache hit, and
	// only the runtime and standard library are replayed. The measured
	// difference is 6.9 seconds per file cold against 0.5 warm, which is
	// the difference between a four minute sweep and a seventeen minute
	// one.
	Cache string

	// Timeout bounds one compile and one program run. Zero means one
	// minute.
	Timeout time.Duration

	// Only restricts the sweep to these base names. Nil sweeps everything.
	Only map[string]bool

	// Parallel is how many files are worked on at once. Zero means
	// GOMAXPROCS.
	Parallel int

	// Context is the build context the platform exclusion is decided
	// against. Nil means the host.
	Context *loader.Context
}

// A Verdict is what happened to one corpus file.
type Verdict struct {
	File   string // base name
	Kind   string // the recipe kind
	Class  Class
	Reason string // the grouping key: the refusal, or why the file was skipped
	Detail string // everything a reader needs in order to act
}

// A Report is the outcome of a sweep.
type Report struct {
	Files    int // how many files the sweep read
	Verdicts []Verdict
}

// implemented is the set of recipe kinds this harness carries out.
//
// Every other kind is counted under [ClassKindNotImplemented] with its name,
// so the report says what was not done rather than leaving it out of the
// totals. The directory kinds are absent because their inputs are the
// subdirectories of $GOROOT/test, which are not vendored: a harness that ran
// them would need source this repository deliberately does not redistribute.
var implemented = map[string]bool{
	"run":        true,
	"compile":    true,
	"errorcheck": true,
}

// Sweep reads the corpus and carries out each file's recipe.
//
// It returns a verdict for every file it read, with no exceptions: the number
// of verdicts equals the number of files. A file the harness cannot handle
// gets a verdict saying so.
func Sweep(opts Options) (*Report, error) {
	files, err := ReadCorpus(opts.Corpus)
	if err != nil {
		return nil, err
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Minute
	}
	if opts.Parallel <= 0 {
		opts.Parallel = runtime.GOMAXPROCS(0)
	}
	if opts.Context == nil {
		opts.Context = loader.DefaultContext()
	}

	var selected []File
	for _, f := range files {
		if opts.Only == nil || opts.Only[f.Name] {
			selected = append(selected, f)
		}
	}

	verdicts := make([]Verdict, len(selected))
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.Parallel)
	for i, f := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			verdicts[i] = judge(opts, f, files)
		}()
	}
	wg.Wait()

	// Sorted by name, so two runs print the same report
	// (specs/053-determinism.md).
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].File < verdicts[j].File })
	return &Report{Files: len(selected), Verdicts: verdicts}, nil
}

// judge produces the one verdict for one file.
func judge(opts Options, f File, all []File) Verdict {
	v := Verdict{File: f.Name, Kind: f.Header.Kind}
	if f.Err != nil {
		v.Class, v.Reason, v.Detail = ClassNoRecipe, f.Err.Error(), f.Err.Error()
		return v
	}
	if ok, err := opts.Context.ShouldBuild(f.Src); err == nil && !ok {
		v.Class = ClassPlatformExcluded
		v.Reason = "the build constraints exclude " + opts.Context.GOOS + "/" + opts.Context.GOARCH
		return v
	}
	if f.Header.Kind == "skip" {
		v.Class, v.Reason = ClassRecipeSaysSkip, "the corpus itself says not to run this file"
		return v
	}
	if !implemented[f.Header.Kind] {
		v.Class, v.Reason = ClassKindNotImplemented, f.Header.Kind
		return v
	}
	if why := unhonourable(f.Header); why != "" {
		v.Class, v.Reason = ClassRecipeNotImplemented, why
		return v
	}

	srcs, argv, err := inputs(opts.Corpus, f, all)
	if err != nil {
		v.Class, v.Reason, v.Detail = ClassRecipeNotImplemented, "a companion source file is not vendored", err.Error()
		return v
	}
	// The stub is for the two kinds that ask for a compile and not a run.
	// A "run" recipe always has a main function, and a program given one it
	// did not write would be a program nobody wrote.
	stub := f.Header.Kind != "run" && needsMainStub(f.Src)
	dir, names, err := stage(opts.Work, f.Name, srcs, stub)
	if err != nil {
		v.Class, v.Reason, v.Detail = ClassOracleFailed, "the file could not be staged", err.Error()
		return v
	}

	switch f.Header.Kind {
	case "run":
		return judgeRun(opts, v, dir, names, argv)
	case "compile":
		return judgeCompile(opts, v, dir, names)
	default: // errorcheck; implemented gates the rest
		return judgeErrorcheck(opts, v, dir, names, f.Src)
	}
}

// unhonourable reports why a recipe cannot be carried out faithfully, or "".
//
// nanogo has no -gcflags, no GOEXPERIMENT and no module version of its own, so
// a recipe asking for one asks for a test nanogo cannot be given. Running it
// anyway would report a verdict about a different test than the one written.
func unhonourable(h Header) string {
	if len(h.Flags) > 0 {
		return "the recipe asks for compiler flags nanogo has no equivalent of: " + strings.Join(h.Flags, " ")
	}
	if len(h.Env) > 0 {
		return "the recipe asks for a build environment nanogo does not set: " + strings.Join(h.Env, " ")
	}
	if h.ModVersion != "" {
		return "the recipe asks for a module version: " + h.ModVersion
	}
	return ""
}

// inputs splits a recipe's arguments into further source files and the
// program's argv.
//
// An argument naming a .go file in the corpus is a second file of the same
// package; anything else is an argument to the program. cmplxdivide.go is the
// only file in the corpus that takes a companion, and args.go is the only one
// that takes arguments.
func inputs(corpus string, f File, all []File) (srcs, argv []string, err error) {
	srcs = []string{filepath.Join(corpus, f.Name)}
	for _, a := range f.Header.Args {
		if !strings.HasSuffix(a, ".go") {
			argv = append(argv, a)
			continue
		}
		found := false
		for _, o := range all {
			if o.Name == a {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, errors.New(f.Name + " names " + a + ", which is not in the vendored corpus")
		}
		srcs = append(srcs, filepath.Join(corpus, a))
	}
	return srcs, argv, nil
}

// stage copies the sources into a directory of their own and writes the go.mod
// both compilers need.
//
// The vendored files are copied byte for byte and never edited. What stage may
// add is a file of nanogo's own; see [mainStub].
func stage(work, name string, srcs []string, stub bool) (string, []string, error) {
	dir := filepath.Join(work, strings.TrimSuffix(name, ".go"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	mod := "module corpus.test/" + strings.TrimSuffix(name, ".go") + "\n\ngo 1.27\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o600); err != nil {
		return "", nil, err
	}
	var names []string
	for _, src := range srcs {
		b, err := os.ReadFile(src)
		if err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), b, 0o600); err != nil {
			return "", nil, err
		}
		names = append(names, filepath.Base(src))
	}
	if stub {
		if err := os.WriteFile(filepath.Join(dir, mainStubName), []byte(mainStub), 0o600); err != nil {
			return "", nil, err
		}
		names = append(names, mainStubName)
	}
	return dir, names, nil
}

// mainStub is nanogo's own file, added beside a corpus file that declares
// package main and no main function.
//
// A "compile" or "errorcheck" recipe asks for the compiler, not the linker.
// The Go distribution's own driver runs `go tool compile` and stops there.
// This harness drives `nanogo build` and `go build`, because those are the
// commands a user runs and the ones nanogo's driver owns, and both of them
// link a main package. Eleven corpus files are a main package with no main
// function, so both compilers compile them and both linkers then report
// "function main is undeclared in the main package" -- a verdict about the
// linker, not about either compiler.
//
// Adding an empty main is the smallest change that lets the recipe be carried
// out with the commands nanogo has. The vendored file is untouched: this is a
// second file, it carries nanogo's own header, and it is added only when the
// corpus file declares package main and no main function of its own.
const (
	mainStubName = "zz_nanogo_main.go"
	mainStub     = `// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Added by internal/gotest beside a corpus file that is a main package with no
// main function, so that the link both build commands perform has an entry
// point to find. See mainStub in internal/gotest/run.go.

package main

func main() {}
`
)

// needsMainStub reports whether a source is a main package that declares no
// main function.
func needsMainStub(src []byte) bool {
	return packageClause.Match(src) && !mainFunc.Match(src)
}

var (
	packageClause = regexp.MustCompile(`(?m)^package main\b`)
	mainFunc      = regexp.MustCompile(`(?m)^func main\s*\(`)
)

// A result is one process run.
type result struct {
	out      string
	code     int
	timedOut bool
}

// run executes one command in dir, under the sweep's cache and timeout.
func run(opts Options, dir string, timeout time.Duration, argv ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = sweepEnv(opts.Cache, opts.Work)
	b, err := cmd.CombinedOutput()
	r := result{out: string(b), timedOut: ctx.Err() != nil}
	if err != nil {
		r.code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.code = ee.ExitCode()
		}
	}
	return r
}

// dropEnv removes every entry for one variable.
//
// The replacement is appended after this rather than beside the old value,
// because a variable set twice is read by the last entry on some systems and
// the first on others, and a rule that depends on which is not a rule.
func dropEnv(env []string, name string) []string {
	out := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// sweepEnv keeps the go command off the network and away from the developer's
// own nanogo settings, which would change what the sweep measures, and points
// the child's temporary directory at the sweep's own.
//
// nanogo build opens a scratch directory with os.MkdirTemp, which reads
// TMPDIR, and removes it with a defer. A defer does not run when the process
// is killed, and this sweep kills a build that passes its deadline. The
// directory then outlives the run and nothing removes it: the child is gone
// and the parent never knew the name.
//
// So the parent chooses the name instead. work is the sweep's own scratch
// directory and the sweep removes it, so anything the child leaves lands
// inside something that is already going away. This is containment rather
// than a fix for the kill, because there is no fix for the kill: SIGKILL runs
// no code in the process it ends.
//
// It reproduces only under load. Each of the four packages that drive nanogo
// is clean when it runs alone, and one directory survives when the whole
// suite runs, because go test runs packages in parallel and a build that
// clears its deadline on an idle machine does not clear it on a busy one.
func sweepEnv(cache, work string) []string {
	out := append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=", "GO111MODULE=on", "GOPROXY=off", "GOCACHE="+cache)
	if work != "" {
		out = append(dropEnv(out, "TMPDIR"), "TMPDIR="+work)
	}
	for i, kv := range out {
		if strings.HasPrefix(kv, "NANOGO_ALLOWLIST=") || strings.HasPrefix(kv, "NANOGO_LOG=") {
			out[i] = strings.SplitN(kv, "=", 2)[0] + "="
		}
	}
	return out
}

// judgeBuildFailure classifies a nanogo build that did not succeed.
//
// The three outcomes mean different things and must never be merged: a refusal
// is expected, a diagnostic in a program gc accepts is a front-end bug, and a
// crash is a bug of a third kind.
func judgeBuildFailure(v Verdict, r result) Verdict {
	switch {
	case r.timedOut:
		v.Class, v.Reason, v.Detail = ClassTimedOut, "nanogo did not finish compiling", r.out
	case IsCrash(r.out):
		v.Class, v.Reason, v.Detail = ClassCrashed, firstLine(r.out), r.out
	case IsRefusal(r.out):
		v.Class, v.Reason, v.Detail = ClassRefused, Reason(r.out), r.out
	default:
		v.Class, v.Reason, v.Detail = ClassFalseError, firstLine(r.out), r.out
	}
	return v
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// judgeRun is the differential of specs/000-decisions.md decision 6: build the
// same source twice, run both, compare.
func judgeRun(opts Options, v Verdict, dir string, names, argv []string) Verdict {
	gcBin := filepath.Join(dir, "gc.bin")
	if r := run(opts, dir, opts.Timeout, append([]string{opts.Go, "build", "-o", gcBin}, names...)...); r.code != 0 {
		v.Class, v.Reason, v.Detail = ClassOracleFailed, "gc did not build the file", r.out
		return v
	}
	gcRun := run(opts, dir, opts.Timeout, append([]string{gcBin}, argv...)...)
	if gcRun.timedOut {
		v.Class, v.Reason, v.Detail = ClassOracleFailed, "gc's build of the program did not finish", gcRun.out
		return v
	}

	nanogoBin := filepath.Join(dir, "nanogo.bin")
	if r := run(opts, dir, opts.Timeout, append([]string{opts.Nanogo, "build", "-o", nanogoBin}, names...)...); r.code != 0 {
		return judgeBuildFailure(v, r)
	}
	ngRun := run(opts, dir, opts.Timeout, append([]string{nanogoBin}, argv...)...)
	if ngRun.timedOut {
		v.Class, v.Reason, v.Detail = ClassTimedOut, "the program nanogo built did not finish", ngRun.out
		return v
	}

	if d := diff(gcRun, ngRun); d != "" {
		v.Class, v.Reason, v.Detail = ClassMismatched, "the two builds behave differently", d
		return v
	}
	v.Class = ClassMatched
	return v
}

// diff compares two runs and returns "" when they agree.
//
// Output and standard error arrive interleaved on one stream, so they are
// compared together after the addresses a traceback carries are removed.
func diff(want, got result) string {
	if want.code != got.code {
		return "gc exited " + itoa(want.code) + " and nanogo's build exited " + itoa(got.code) +
			"\ngc said:\n" + indent(want.out) + "nanogo's build said:\n" + indent(got.out)
	}
	w, g := NormalizeStderr(want.out), NormalizeStderr(got.out)
	if w != g {
		return "both exited " + itoa(want.code) + " and the output differs" +
			"\ngc said:\n" + indent(want.out) + "nanogo's build said:\n" + indent(got.out)
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func indent(s string) string {
	if s == "" {
		return "\t(nothing)\n"
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("\t" + line + "\n")
	}
	return b.String()
}

// bodylessReason names the one shape of "compile" recipe this harness cannot
// express.
//
// A function declared with no body is an assembly stub. Both build commands
// take a list of .go files and neither accepts a .s file in that list, so
// there is no way to tell either compiler that the body arrives from
// elsewhere. The Go distribution's driver runs `go tool compile` directly and
// does not have this problem. Naming the four files is worth more than a vague
// oracle failure that reads as gc's fault.
const bodylessReason = "the file declares a function with no body, which needs an assembly file a .go file list cannot carry"

// judgeCompile carries out a "compile" recipe: the file must build, and is not
// run.
func judgeCompile(opts Options, v Verdict, dir string, names []string) Verdict {
	if r := run(opts, dir, opts.Timeout, append([]string{opts.Go, "build"}, names...)...); r.code != 0 {
		if strings.Contains(r.out, "missing function body") {
			v.Class, v.Reason, v.Detail = ClassRecipeNotImplemented, bodylessReason, r.out
			return v
		}
		v.Class, v.Reason, v.Detail = ClassOracleFailed, "gc did not build the file", r.out
		return v
	}
	r := run(opts, dir, opts.Timeout, append([]string{opts.Nanogo, "build"}, names...)...)
	if r.code != 0 {
		return judgeBuildFailure(v, r)
	}
	v.Class = ClassCompiled
	return v
}

// judgeErrorcheck carries out an "errorcheck" recipe: the file must be
// rejected, at the annotated position.
//
// The comparison is on the first error's line, which is what
// specs/004-conformance.md asks for and what syntax/parser_test.go already
// does over the distribution. gc collapses several errors on one line into
// one, and nanogo stops after ten, so comparing the whole set would compare
// two reporting policies rather than two judgements.
func judgeErrorcheck(opts Options, v Verdict, dir string, names []string, src []byte) Verdict {
	want := ErrorAnnotations(src)
	if len(want) == 0 {
		v.Class, v.Reason = ClassRecipeNotImplemented, "an errorcheck file with no ERROR annotation"
		return v
	}
	if r := run(opts, dir, opts.Timeout, append([]string{opts.Go, "build"}, names...)...); r.code == 0 {
		v.Class, v.Reason, v.Detail = ClassOracleFailed, "gc accepted a file the recipe says it must reject", r.out
		return v
	}

	r := run(opts, dir, opts.Timeout, append([]string{opts.Nanogo, "build"}, names...)...)
	switch {
	case r.timedOut:
		v.Class, v.Reason, v.Detail = ClassTimedOut, "nanogo did not finish compiling", r.out
	case r.code == 0:
		v.Class, v.Reason = ClassMissed, "nanogo accepted a file that must be rejected"
	case IsCrash(r.out):
		v.Class, v.Reason, v.Detail = ClassCrashed, firstLine(r.out), r.out
	case IsRefusal(r.out):
		v.Class, v.Reason, v.Detail = ClassRefused, Reason(r.out), r.out
	default:
		line, ok := FirstErrorLine(r.out)
		if !ok {
			v.Class, v.Reason, v.Detail = ClassCrashed, "nanogo failed with no diagnostic", r.out
			return v
		}
		if line != want[0] {
			v.Class = ClassWrongPosition
			v.Reason = "the first error is not on the annotated line"
			v.Detail = "annotated line " + itoa(want[0]) + ", nanogo reported line " + itoa(line) + "\n" + indent(r.out)
			return v
		}
		v.Class = ClassRejected
	}
	return v
}
