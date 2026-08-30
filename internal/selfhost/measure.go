// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package selfhost measures how much of a package list nanogo compiles.
//
// It answers the one question the G1 gate of specs/060-selfhost.md is built
// on: given a set of packages, which of them does nanogo compile and which
// does it refuse, and with what message. Until this package existed the
// answer was produced by hand and copied into prose, and it was wrong three
// times in a row for three different reasons, each recorded below.
//
// # Why a package cannot simply be built
//
// A build with -toolexec runs nanogo in place of each toolchain invocation,
// and nanogo hands gc every package that is not on its allowlist or that it
// cannot compile (specs/051-build-integration.md). So the build succeeds
// either way. Exit status carries no information at all, and the first
// measurement taken this way reported twelve of twelve because of it.
//
// The only evidence is the line nanogo writes to NANOGO_LOG: "compiled",
// "delegated" or "failed", and the package. [Measure] discards the exit
// status on purpose and reads that line.
//
// # The three traps
//
// Each of these produced a confident wrong number before this package
// existed, and each is defended against below rather than described in a
// comment somewhere else.
//
//   - An unset or empty NANOGO_ALLOWLIST is not "compile everything". It is
//     "compile nothing": every package goes to gc and the build still
//     succeeds. [Measure] writes one allowlist per package and fails if the
//     file it wrote is empty.
//
//   - A package built earlier by gc as somebody's dependency has the same
//     action ID when it becomes the allowlisted target later, so the go
//     command reuses the cached archive and -toolexec never runs. The log is
//     then empty for the package under test and the run proved nothing.
//     [Measure] gives each package its own GOCACHE so the compile action
//     cannot be reused, and reports a package with no log line as
//     [NotReached] rather than counting it either way.
//
//   - A whole-tree build stops at the first failure and says nothing about
//     the rest, so it reports the deepest package's blocker as though it were
//     the only one. Each package here is measured on its own, against
//     gc-built dependencies.
//
// # What a compile does and does not prove
//
// It proves nanogo produced an archive the go command accepted. It does not
// prove the archive is correct, and nothing is linked or run. That is the
// division specs/060-selfhost.md draws: compiling every package is a
// precondition of G1 and is not G1, which asks for a compiler that builds
// itself and then builds itself again to the same bytes.
package selfhost

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A Decision is what nanogo did with one package.
type Decision string

const (
	// Compiled: nanogo compiled the package and the go command accepted the
	// archive.
	Compiled Decision = "compiled"

	// Delegated: nanogo handed the package to gc. Under [Measure] this means
	// the allowlist did not select it, which is a fault in the measurement
	// and not a fact about the compiler.
	Delegated Decision = "delegated"

	// Failed: nanogo refused the package or could not compile it. [Result.Reason]
	// holds what it said.
	Failed Decision = "failed"

	// NotReached: nanogo was never asked about the package, so the run proved
	// nothing about it. A cached compile action is the usual cause.
	NotReached Decision = "not-reached"
)

// A Result is one package's measurement.
type Result struct {
	Path     string
	Decision Decision

	// Reason is what nanogo said, for [Failed]. It is the whole message and
	// not a summary: the message names the construct, and a summary of it is
	// the thing that has to be re-measured to be acted on.
	Reason string
}

// A Package is one thing to measure.
//
// Two names are needed and they are not always the same. Target is what the go
// command is asked to build, which is an import path. Name is what nanogo is
// told the package is called, which is the -p value the go command passes, and
// that is what the allowlist has to hold and what the log line carries.
//
// They differ for exactly one kind of package. The go command passes -p main
// for a main package, whatever its import path, so a measurement that used the
// import path for both would put the wrong name on the allowlist, get the
// package delegated to gc, and find no log line for it. That is not a
// hypothetical: the first run of the closure measurement reported the program
// as never reached for this reason, and a build with the import path on the
// allowlist compiles nothing and still succeeds.
type Package struct {
	Target string
	Name   string
}

// Paths turns import paths into packages for the common case, where the two
// names are the same. It is wrong for a main package: use [MainPackage].
func Paths(paths ...string) []Package {
	out := make([]Package, 0, len(paths))
	for _, p := range paths {
		out = append(out, Package{Target: p, Name: p})
	}
	return out
}

// MainPackage is a main package, which the go command names main whatever its
// import path is.
func MainPackage(target string) Package { return Package{Target: target, Name: "main"} }

// Options are the inputs to [Measure].
type Options struct {
	// Compiler is the nanogo binary to run under -toolexec.
	Compiler string

	// Packages are the packages to measure, one build each.
	Packages []Package

	// Dir is the directory the go command runs in. It decides which module
	// the import paths resolve against.
	Dir string

	// Work is a writable directory for the allowlists, the logs, the caches
	// and the output archives. Each package gets its own subtree.
	Work string

	// Env is added to the environment of every build, as "K=V". It is for a
	// caller that has to pin GOFLAGS or CGO_ENABLED, and never for
	// NANOGO_ALLOWLIST, NANOGO_LOG or GOCACHE, which [Measure] owns.
	Env []string

	// Parallel is how many builds run at once, bounded by the number of
	// packages. Zero means [DefaultParallel].
	Parallel int
}

// DefaultParallel is how many builds [Measure] runs at once when the caller
// does not say.
//
// It is not the CPU count. Each build has its own GOCACHE, so each one
// rebuilds the whole standard library, and the measurement is bound by disk
// rather than by the compiler: one per CPU would run more of them at once and
// finish no sooner, on a cache directory per build. Nineteen packages measured
// one at a time took about six minutes on the machine this was written for,
// which is the number to beat.
const DefaultParallel = 4

// ownedEnv are the variables Measure sets per package. A caller that set one
// through [Options.Env] would silently unmeasure a package, so passing one is
// an error rather than an override.
var ownedEnv = []string{"NANOGO_ALLOWLIST", "NANOGO_LOG", "GOCACHE"}

// Measure compiles each package on its own and reports what nanogo did with
// it.
//
// The results are in the order the packages were given, so a caller's list
// decides the report's order and two runs of the same list print the same
// table (specs/053-determinism.md).
func Measure(opts Options) ([]Result, error) {
	if opts.Compiler == "" {
		return nil, fmt.Errorf("selfhost: no compiler to run under -toolexec")
	}
	if len(opts.Packages) == 0 {
		return nil, fmt.Errorf("selfhost: no packages, so the measurement would report nothing and pass")
	}
	if opts.Work == "" {
		return nil, fmt.Errorf("selfhost: no work directory")
	}
	for _, kv := range opts.Env {
		k, _, _ := strings.Cut(kv, "=")
		for _, owned := range ownedEnv {
			if k == owned {
				return nil, fmt.Errorf("selfhost: Env sets %s, which Measure sets per package; setting it here unmeasures every package", owned)
			}
		}
	}

	n := opts.Parallel
	if n <= 0 {
		n = DefaultParallel
	}
	if n > len(opts.Packages) {
		n = len(opts.Packages)
	}

	out := make([]Result, len(opts.Packages))
	errs := make([]error, len(opts.Packages))
	var wg sync.WaitGroup
	sem := make(chan struct{}, n)
	for i, pkg := range opts.Packages {
		wg.Add(1)
		go func(i int, pkg Package) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := measureOne(opts, pkg)
			out[i], errs[i] = r, err
		}(i, pkg)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// measureOne runs one build with an allowlist naming pkg alone.
func measureOne(opts Options, pkg Package) (Result, error) {
	dir := filepath.Join(opts.Work, slug(pkg.Target))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	list := filepath.Join(dir, "allowlist.txt")
	if err := os.WriteFile(list, []byte(pkg.Name+"\n"), 0o644); err != nil {
		return Result{}, err
	}
	// An empty allowlist compiles nothing and the build still succeeds, so
	// the file is read back rather than trusted. This is cheap and it is the
	// trap that produced "twelve of twelve".
	if b, err := os.ReadFile(list); err != nil || strings.TrimSpace(string(b)) != pkg.Name {
		return Result{}, fmt.Errorf("selfhost: the allowlist for %s is not the one that was written: %q, %v", pkg.Target, b, err)
	}

	log := filepath.Join(dir, "decisions.log")
	cmd := exec.Command("go", "build", "-toolexec="+opts.Compiler, "-o", filepath.Join(dir, "out.a"), pkg.Target)
	cmd.Dir = opts.Dir
	cmd.Env = append(os.Environ(),
		"NANOGO_ALLOWLIST="+list,
		"NANOGO_LOG="+log,
		// Its own cache, so the compile action for pkg cannot be answered
		// from an archive gc built for an earlier package in this run.
		"GOCACHE="+filepath.Join(dir, "cache"),
	)
	cmd.Env = append(cmd.Env, opts.Env...)
	// The exit status is read and discarded. nanogo returns 1 when it refuses
	// a package it was told to compile, and 0 when it delegates one it cannot,
	// so a non-zero status and a zero status are both compatible with every
	// decision below.
	_ = cmd.Run()

	r, err := readDecision(log, pkg.Name)
	// The report is keyed by the import path, which is what a reader of
	// specs/060-selfhost.md is looking for. "main" names one package per
	// build and would collide across a list holding two of them.
	r.Path = pkg.Target
	return r, err
}

// readDecision finds the line the log holds for pkg.
//
// The log holds a line for every package the build compiled, including the
// dependencies gc built, so the line is matched on the package path exactly.
// A prefix match would read internal/runtime/gc/scan as internal/runtime/gc,
// which is a real pair in the bootstrap closure.
func readDecision(path, pkg string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// nanogo was never run, so nothing was decided. Reported rather
			// than guessed: a cached compile action looks exactly like this.
			return Result{Path: pkg, Decision: NotReached}, nil
		}
		return Result{}, err
	}
	defer f.Close()

	res := Result{Path: pkg, Decision: NotReached}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		what, rest, ok := strings.Cut(strings.TrimSpace(s.Text()), " ")
		if !ok {
			continue
		}
		named, reason, _ := strings.Cut(rest, " ")
		if named != pkg {
			continue
		}
		switch Decision(what) {
		case Compiled, Delegated, Failed:
			res.Decision, res.Reason = Decision(what), strings.TrimSpace(reason)
		default:
			continue
		}
		// The first decision for the package is the one that counts. nanogo
		// returns on its first error, so a second line for the same package
		// would be a different invocation of the same compile.
		return res, s.Err()
	}
	return res, s.Err()
}

// slug turns an import path into one path element.
func slug(pkg string) string {
	r := strings.NewReplacer("/", "_", ".", "_", string(filepath.Separator), "_")
	s := r.Replace(pkg)
	if s == "" {
		return "_"
	}
	return s
}

// WithDecision is the sorted set of package paths that got one decision.
func WithDecision(rs []Result, d Decision) []string {
	var out []string
	for _, r := range rs {
		if r.Decision == d {
			out = append(out, r.Path)
		}
	}
	sort.Strings(out)
	return out
}

// Count is how many results carry a decision.
func Count(rs []Result, d Decision) int { return len(WithDecision(rs, d)) }

// Table is the report, one line per package, in the order measured.
//
// A failure prints its whole message. A count of failures says that something
// is wrong and not what, and what is the only part that can be acted on.
func Table(rs []Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d packages compiled by nanogo\n", Count(rs, Compiled), len(rs))
	for _, r := range rs {
		fmt.Fprintf(&b, "\t%-10s %s", r.Decision, r.Path)
		if r.Reason != "" {
			fmt.Fprintf(&b, "\n\t\t%s", r.Reason)
		}
		b.WriteString("\n")
	}
	return b.String()
}
