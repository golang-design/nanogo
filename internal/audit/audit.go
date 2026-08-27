// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package audit runs the probe corpus and says what nanogo did with each
// probe.
//
// The corpus is testdata/probes, one directory per Go construct, one main
// package each. It exists because the documentation's capability claims are
// prose, and prose does not re-run: in August 2026 README.md, doc.go and
// nanogo help all said nanogo refused defer for weeks after defer started
// working.
//
// Every probe is compiled twice, once by nanogo and once by the Go toolchain,
// and both programs are run and compared. gc is the oracle, so no probe
// carries an expected value that can itself go stale. That is the whole design
// and testdata/probes/run.sh states it too: this package is the same audit
// with a timeout, a ratchet and a place in CI.
//
// The shell script remains the way a person runs the corpus by hand, and
// TestHarnessAgreesWithRunScript keeps the two from drifting apart.
package audit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProbesDir is the corpus, relative to this package's directory.
const ProbesDir = "testdata/probes"

// A Class is what nanogo did with one probe.
//
// The four classes are totally ordered by [Class.Rank], best first, and the
// order is the ratchet's whole rule. CONTRIBUTING.md settles the hard pair:
// "WRONG is the row that matters: a program nanogo compiled into something
// that behaves differently is worse than one it refused."
type Class string

const (
	// ClassOK means nanogo compiled the probe and the program agreed with
	// gc, in exit status and in output.
	ClassOK Class = "ok"

	// ClassRefused means nanogo would not build the probe. The refusal is
	// recorded, not hidden: a refusal that becomes an OK is the moment a
	// documented limitation turns into a stale claim, and the ratchet can
	// only see that moment if it knows the probe used to be refused.
	//
	// A compiler crash lands here too. It is still a build that produced no
	// program, and the detail carries the panic.
	ClassRefused Class = "refused"

	// ClassWrong means nanogo compiled the probe and the program disagreed
	// with gc. This is worse than a refusal, because a user meets it as a
	// wrong answer rather than as a message.
	ClassWrong Class = "wrong"

	// ClassBroken means gc could not build the probe, so the run has no
	// oracle and says nothing about nanogo. The probe is broken, not the
	// compiler. It ranks below every other class so that it can never be
	// recorded as an acceptable resting state.
	ClassBroken Class = "broken"
)

// Rank orders the classes. Higher is better, and a probe whose rank falls is a
// regression whatever the two classes are.
func (c Class) Rank() int {
	switch c {
	case ClassOK:
		return 3
	case ClassRefused:
		return 2
	case ClassWrong:
		return 1
	default:
		return 0
	}
}

// Options configure a sweep of the corpus.
type Options struct {
	// Probes is the directory holding the probe directories.
	Probes string

	// Nanogo and Go are the two compilers. Go is the oracle.
	Nanogo string
	Go     string

	// Cache is one GOCACHE for the whole sweep. Empty inherits the
	// environment's.
	//
	// Sharing it is safe here for the reason internal/gotest gives: every
	// probe is different source, so nothing either compiler produces for a
	// probe is ever a cache hit, and only the standard library is replayed.
	// Replaying it is the difference between a two minute sweep and a
	// twenty minute one.
	Cache string

	// Work is where the built executables go. Empty uses a temporary
	// directory that the sweep removes.
	Work string

	// Timeout bounds one build and one program run. Zero means one minute.
	//
	// The corpus probes channels, goroutines and select. A deadlock in a
	// miscompiled one would otherwise hang CI until the runner's own ceiling
	// killed it, with no line in the log saying which probe did it.
	Timeout time.Duration

	// Only restricts the sweep to these probe names. Nil sweeps everything.
	Only map[string]bool

	// Parallel is how many probes are worked on at once. Zero means
	// GOMAXPROCS.
	Parallel int
}

// A Result is one compile-and-run of one probe by one compiler.
type Result struct {
	Built bool   // the compiler produced an executable
	Build string // what the compiler said when it did not
	Exit  int    // the program's exit status
	Out   string // the program's combined output
}

// Same reports whether two results are the same observation. A build that
// failed is never the same as one that succeeded, whatever the exit codes say.
func (r Result) Same(o Result) bool {
	return r.Built == o.Built && r.Exit == o.Exit && sameOutput(r.Out, o.Out)
}

// sameOutput compares two program outputs.
//
// Byte equality is the rule, and a Go traceback is the one exception. A
// traceback prints two things that describe the compiler rather than the
// program: the offset of the program counter inside its frame, and the
// arguments of a frame the compiler inlined. Two compilers that agree
// perfectly about what a program does still disagree about both.
//
//	nanogo   main.boom(0x0)
//	                 /tmp/probe/main.go:4 +0x38
//	gc       main.boom(...)
//	                 /tmp/probe/main.go:4 +0x2c
//
// Demanding byte equality there makes "agrees with gc" a property only gc can
// have, for every program that panics without recovering. That is not a strict
// test, it is a broken one: it reports a difference in code generation as a
// difference in behaviour, which is the distinction this whole corpus exists to
// draw.
//
// What is normalised is exactly those two things and nothing else. The panic
// message, the goroutine header, the frame order, every function name, and
// every file and line still have to match byte for byte, and so does all
// output that is not a traceback. A wrong panic value, a missing frame, a
// frame in the wrong order and a wrong line number all still fail.
func sameOutput(a, b string) bool {
	if a == b {
		return true
	}
	return normalizeTraceback(a) == normalizeTraceback(b)
}

// pcOffset matches the program counter offset a traceback prints after a
// position, which is where in the frame execution had reached.
var pcOffset = regexp.MustCompile(`( \+0x[0-9a-f]+)$`)

// frameArgs matches the argument list of a traceback's frame line. gc prints
// "(...)" for a frame it inlined, because the arguments are not recoverable,
// and the real values otherwise.
var frameArgs = regexp.MustCompile(`^([\w./\-*()\[\]]+)\(.*\)$`)

func normalizeTraceback(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "\t") {
			lines[i] = pcOffset.ReplaceAllString(line, "")
			continue
		}
		if m := frameArgs.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + "(~)"
		}
	}
	return strings.Join(lines, "\n")
}

// A Verdict is what happened to one probe.
type Verdict struct {
	Probe  string
	Class  Class
	Nanogo Result
	Gc     Result
}

// Detail is the one line a reader needs in order to act on this verdict.
func (v Verdict) Detail() string {
	switch v.Class {
	case ClassOK:
		return "exit=" + strconv.Itoa(v.Nanogo.Exit) + " out=" + oneLine(v.Nanogo.Out)
	case ClassRefused:
		return oneLine(v.Nanogo.Build)
	case ClassBroken:
		return "gc could not build this probe: " + oneLine(v.Gc.Build)
	default:
		return "nanogo=" + describe(v.Nanogo) + " gc=" + describe(v.Gc)
	}
}

func describe(r Result) string {
	if !r.Built {
		return "build-failed(" + oneLine(r.Build) + ")"
	}
	return "exit=" + strconv.Itoa(r.Exit) + " out=" + oneLine(r.Out)
}

// A Report is the outcome of a sweep. It holds one verdict per probe read,
// with no exceptions, so the verdicts are the denominator.
type Report struct {
	Verdicts []Verdict
}

// Probes is how many probes the sweep read.
func (r *Report) Probes() int { return len(r.Verdicts) }

// ByClass counts the verdicts of each class.
func (r *Report) ByClass() map[Class]int {
	out := make(map[Class]int)
	for _, v := range r.Verdicts {
		out[v.Class]++
	}
	return out
}

// String renders the report, one line per probe, in the shape run.sh prints.
func (r *Report) String() string {
	var b strings.Builder
	for _, v := range r.Verdicts {
		b.WriteString(pad(v.Probe, 24))
		b.WriteString(" " + pad(strings.ToUpper(string(v.Class)), 7) + " ")
		b.WriteString(v.Detail())
		b.WriteString("\n")
	}
	for _, c := range []Class{ClassOK, ClassRefused, ClassWrong, ClassBroken} {
		b.WriteString(pad(string(c), 24) + " " + strconv.Itoa(r.ByClass()[c]) + "\n")
	}
	b.WriteString(pad("TOTAL", 24) + " " + strconv.Itoa(r.Probes()) + "\n")
	return b.String()
}

// ReadProbes returns the names of the probe directories, sorted.
//
// A probe is a directory holding a main.go. Anything else in the corpus
// directory, run.sh and go.mod included, is not a probe.
func ReadProbes(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "main.go")); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// Sweep reads the corpus and judges every probe against gc.
func Sweep(opts Options) (*Report, error) {
	names, err := ReadProbes(opts.Probes)
	if err != nil {
		return nil, err
	}
	if opts.Nanogo == "" || opts.Go == "" {
		return nil, errors.New("a sweep needs both compilers: nanogo and go")
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Minute
	}
	if opts.Parallel <= 0 {
		opts.Parallel = runtime.GOMAXPROCS(0)
	}
	if opts.Work == "" {
		work, err := os.MkdirTemp("", "nanogo-probes")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
		opts.Work = work
	}

	var selected []string
	for _, n := range names {
		if opts.Only == nil || opts.Only[n] {
			selected = append(selected, n)
		}
	}

	verdicts := make([]Verdict, len(selected))
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.Parallel)
	for i, name := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			verdicts[i] = judge(opts, name)
		}()
	}
	wg.Wait()

	// Already sorted, because ReadProbes sorts and the slice is filled by
	// index. Stated as a sort anyway so that a future Only ordering cannot
	// make two runs print different reports (specs/053-determinism.md).
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].Probe < verdicts[j].Probe })
	return &Report{Verdicts: verdicts}, nil
}

// judge builds and runs one probe under both compilers and compares them.
//
// nanogo runs first and gc only when nanogo built something. A refusal needs
// no oracle, and not asking for one halves the work on the half of the corpus
// nanogo cannot compile yet.
func judge(opts Options, name string) Verdict {
	v := Verdict{Probe: name}
	v.Nanogo = build(opts, opts.Nanogo, name, "nanogo")
	if !v.Nanogo.Built {
		v.Class = ClassRefused
		return v
	}
	v.Gc = build(opts, opts.Go, name, "gc")
	switch {
	case !v.Gc.Built:
		v.Class = ClassBroken
	case v.Nanogo.Same(v.Gc):
		v.Class = ClassOK
	default:
		v.Class = ClassWrong
	}
	return v
}

// noise is the three lines every nanogo build prints. They report the
// nanogo/gc split and are not part of a refusal, so they are stripped here for
// the same reason run.sh strips them.
var noise = []string{
	"nanogo: ",
}

func build(opts Options, compiler, name, tag string) Result {
	out := filepath.Join(opts.Work, name+"."+tag)
	if err := os.MkdirAll(opts.Work, 0o700); err != nil {
		return Result{Build: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, compiler, "build", "-o", out, "./"+name)
	cmd.Dir = opts.Probes
	cmd.Env = env(opts)
	cmd.WaitDelay = waitDelay
	text, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Build: strip(string(text)) + timedOut(ctx, "the build")}
	}
	return run(opts, out)
}

func run(opts Options, bin string) Result {
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = opts.Probes
	cmd.Env = env(opts)
	cmd.WaitDelay = waitDelay
	text, err := cmd.CombinedOutput()
	r := Result{Built: true, Out: string(text) + timedOut(ctx, "the program")}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.Exit = ee.ExitCode()
	case err != nil:
		r.Exit = -1
		r.Out += err.Error()
	}
	return r
}

// waitDelay bounds how long a killed process may go on holding the output
// pipe.
//
// Killing the process is not enough on its own. A probe that spawned a
// goroutine, or a shell that started a child, leaves a grandchild holding the
// write end, and CombinedOutput then waits for a pipe that never closes. The
// timeout would fire and the sweep would hang anyway, which is the failure the
// timeout exists to prevent.
const waitDelay = 5 * time.Second

// timedOut names the timeout in the text the report prints. Without it a probe
// the harness killed reads as a probe that produced no output, which is a
// different bug entirely.
func timedOut(ctx context.Context, what string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "\n" + what + " did not finish before the audit's timeout"
	}
	return ""
}

func env(opts Options) []string {
	e := os.Environ()
	if opts.Cache != "" {
		e = append(e, "GOCACHE="+opts.Cache)
	}
	return e
}

// strip removes the progress lines a nanogo build prints on its way to a
// refusal, so that the refusal is what the report shows.
func strip(text string) string {
	var keep []string
	for _, line := range strings.Split(text, "\n") {
		if isNoise(line) {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

func isNoise(line string) bool {
	for _, pre := range noise {
		rest, ok := strings.CutPrefix(line, pre)
		if !ok {
			continue
		}
		for _, tail := range []string{"the standard library", "the executable was"} {
			if strings.HasPrefix(rest, tail) {
				return true
			}
		}
		// "nanogo: N of M packages ..."
		if f := strings.Fields(rest); len(f) > 3 && f[1] == "of" && f[3] == "packages" {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", "/"))
	const max = 160
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}
