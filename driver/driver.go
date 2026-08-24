// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package driver implements nanogo's command line.
//
// The command line is [cmd/compile]'s, because the go command substitutes
// nanogo for each toolchain invocation through -toolexec and constructs the
// arguments itself. See specs/050-driver.md for the flag set and
// specs/051-build-integration.md for the substitution model.
//
// The package holds all the logic so that it is testable without a process.
// cmd/nanogo is a shell over [Run].
//
// The order of decisions in [Run] follows the flowchart in
// specs/051-build-integration.md:
//
//  1. answer -V=full, because the go command asks for the build ID before it
//     compiles anything;
//  2. exec the real tool when the tool is not compile;
//  3. exec the real tool when the package is not on the allowlist;
//  4. exec the real tool when the flags are not all supported;
//  5. otherwise compile with nanogo.
package driver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExecFunc runs the real tool at path with args. It reports the exit status
// nanogo must exit with. On unix the production implementation replaces the
// nanogo process image, so it returns only when the tool cannot start.
type ExecFunc func(path string, args []string) (int, error)

// Env is everything [Run] touches outside itself. The fields exist so that a
// test drives the driver in process.
type Env struct {
	// Args is the command line without the program name. It is the tool path
	// and the tool's arguments, optionally preceded by nanogo's own flags.
	Args []string

	// Stdout receives the -V=full line and nothing else. The go command parses
	// standard output, so any extra line shifts the fields it reads.
	Stdout io.Writer

	// Stderr receives diagnostics.
	Stderr io.Writer

	// Getenv reads the environment. It defaults to os.Getenv.
	Getenv func(string) string

	// Exec runs the real tool. It defaults to the platform passthrough.
	Exec ExecFunc
}

// Run executes one nanogo invocation and reports the process exit status.
func Run(env Env) int {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Exec == nil {
		env.Exec = execTool
	}

	code, err := run(&env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nanogo: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// usage is the message for an empty or malformed command line.
const usage = "usage: nanogo [-fallback] <tool> [arguments]"

func run(env *Env) (int, error) {
	args := env.Args
	fallback := false

	// nanogo's own flags come before the tool path, because -toolexec splits
	// its value into words and the go command appends the tool path after
	// them. -fallback is also accepted inside the compile flag set, where
	// -gcflags puts it.
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-fallback", "--fallback":
			fallback = true
		default:
			return 1, fmt.Errorf("unknown driver flag %q\n%s", args[0], usage)
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return 1, errors.New(usage)
	}

	toolPath, toolArgs := args[0], args[1:]
	tool := ToolName(toolPath)

	// The assembler, the linker, cgo and pack are always the real tool, and
	// that includes their build ID. nanogo changes nothing about their output,
	// so their identity must stay the real tool's: a nanogo answer would pin
	// their cache keys to nanogo's constant pinned release and hide a change
	// of toolchain from the go command.
	if tool != "compile" {
		return env.Exec(toolPath, toolArgs)
	}

	// The go command runs <toolexec> compile -V=full before every build and
	// turns the answer into part of every cache key. This must come before
	// flag parsing, because -V=full is not a compile flag.
	if isVersionQuery(toolArgs) {
		fmt.Fprintln(env.Stdout, VersionLine(tool))
		return 0, nil
	}

	// Package selection reads -p without validating the rest of the command
	// line. A package nanogo does not own must reach gc even when a flag
	// nanogo has never seen appears later on the line.
	pkg, flagFallback := scanCompile(toolArgs)
	if fallback || flagFallback {
		return env.Exec(toolPath, toolArgs)
	}
	allow, err := AllowlistFromEnv(env.Getenv)
	if err != nil {
		return 1, err
	}
	if !allow.Has(pkg) {
		return env.Exec(toolPath, toolArgs)
	}

	// The package is nanogo's. A flag nanogo does not implement now sends the
	// package to gc rather than producing a subtly different object, per
	// specs/050-driver.md and the flowchart in specs/051-build-integration.md.
	cfg, err := ParseCompile(toolArgs)
	if err != nil {
		return env.Exec(toolPath, toolArgs)
	}
	if cfg.ImportCfgFile != "" {
		cfg.ImportCfg, err = ReadImportCfg(cfg.ImportCfgFile)
		if err != nil {
			return 1, err
		}
	}
	if err := Compile(cfg); err != nil {
		return 1, err
	}
	return 0, nil
}

// ToolName is the tool's name as the go command's build ID parser expects it.
// The go command passes an absolute path and compares the first field of
// -V=full output against the base name.
func ToolName(path string) string {
	name := filepath.Base(path)
	return strings.TrimSuffix(name, ".exe")
}

// isVersionQuery reports whether the arguments ask for the build ID line. The
// go command sends exactly -V=full, but the real tools also accept -V and
// -V=goexperiment.
func isVersionQuery(args []string) bool {
	return len(args) > 0 && (args[0] == "-V" || strings.HasPrefix(args[0], "-V="))
}

// scanCompile finds the two things package selection needs: the import path
// from -p, and -fallback. It does not validate anything else, and it must not,
// because selection happens before nanogo claims to understand the line.
func scanCompile(args []string) (pkg string, fallback bool) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-p" || a == "--p":
			if i+1 < len(args) {
				pkg = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-p="):
			pkg = strings.TrimPrefix(a, "-p=")
		case strings.HasPrefix(a, "--p="):
			pkg = strings.TrimPrefix(a, "--p=")
		case a == "-fallback", a == "--fallback",
			a == "-fallback=true", a == "--fallback=true":
			fallback = true
		}
	}
	return pkg, fallback
}

// NotImplementedError reports that nanogo owns a package but has no compiler
// for it yet.
type NotImplementedError struct {
	Package string
}

func (e *NotImplementedError) Error() string {
	return e.Package + ": not implemented: nanogo has no compiler yet"
}

// Compile is the single point where nanogo's own compiler runs. Swapping in a
// real compiler is a change to this function and to nothing else.
//
// M0 has no compiler, so an allowlisted package fails here. The failure names
// the package, because the allowlist is the project's progress metric and a
// failure must say which entry produced it.
func Compile(cfg *Config) error {
	// specs/000-decisions.md decision 11 pins nanogo to one Go release. The
	// go command states the release it expects with -goversion.
	if cfg.GoVersion != "" && cfg.GoVersion != PinnedGoVersion {
		return fmt.Errorf("%s: -goversion %s does not match the pinned %s",
			cfg.Package, cfg.GoVersion, PinnedGoVersion)
	}
	return &NotImplementedError{Package: cfg.Package}
}
