// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// TrimRewrite is one entry of -trimpath. The go command sends a list of
// rewrites joined by ";", each written old=>new, and the last entry has an
// empty new side because it erases the object directory. See
// cmd/go/internal/work.(*Action).trimpath.
type TrimRewrite struct {
	Old string
	New string
}

// Config is a decoded compile command line.
//
// The field set is measured, not transcribed. spikes/toolexec logs what the go
// command sends during a real build, which is a smaller and different set from
// what go tool compile -h lists. See specs/050-driver.md.
type Config struct {
	// Sent on every compile invocation. Ignoring any of these produces a wrong
	// build, silently.

	Output         string        // -o, the output file
	Package        string        // -p, the import path and the symbol prefix
	ImportCfgFile  string        // -importcfg, the file named on the command line
	ImportCfg      *ImportCfg    // the contents of that file, once read
	Lang           string        // -lang, the language version the source expects
	BuildID        string        // -buildid, recorded in the output
	GoVersion      string        // -goversion, the required runtime version
	TrimPath       string        // -trimpath, verbatim
	TrimRewrites   []TrimRewrite // -trimpath, split
	Concurrency    int           // -c
	Pack           bool          // -pack, write an archive and not a bare object
	NoLocalImports bool          // -nolocalimports
	Shared         bool          // -shared, position independent code

	// Sent conditionally.

	Complete         bool   // -complete, no assembly and no C in the package
	SymABIs          string // -symabis, the package has assembly
	AsmHdr           string // -asmhdr, the package has assembly
	Std              bool   // -std, the package is in the standard library
	EmbedCfgFile     string // -embedcfg, the package uses go:embed
	CompilingRuntime bool   // -+, compiling the runtime

	// Flags nanogo defines. gc counts repetitions of these, so nanogo does
	// too: -l -l is not -l.

	NoOptimize   int      // -N
	NoInline     int      // -l
	AsmListing   int      // -S
	OptDecisions int      // -m
	Debug        []string // -d, one entry per occurrence
	Fallback     bool     // -fallback, hand the package to gc

	// Files are the source files, in the order given.
	Files []string

	// GOARCH is the architecture the build is for, which is not the
	// architecture nanogo is running on. The two differ whenever the go
	// command is cross-compiling, and the code generator has to answer to the
	// target rather than to the host: a compiler that reads runtime.GOARCH
	// emits arm64 machine code for an amd64 build and the failure surfaces in
	// the linker, as an unknown relocation, a long way from its cause.
	//
	// Empty means the host, which is what a build with no GOARCH set means.
	GOARCH string
}

// TargetArch is the architecture the compiled code must run on.
func (c *Config) TargetArch() string {
	if c.GOARCH != "" {
		return c.GOARCH
	}
	return runtime.GOARCH
}

// flagKind is how a flag takes its value.
type flagKind int

const (
	// flagBool is set by -f and by -f=true. It never consumes the next
	// argument. A greedy bool flag eats the source file that follows it.
	flagBool flagKind = iota
	// flagCount increments on -f and is set directly by -f=2. gc calls this a
	// CountFlag.
	flagCount
	// flagString takes -f v or -f=v.
	flagString
	// flagInt takes -f n or -f=n.
	flagInt
)

// flagSpec describes one flag nanogo accepts.
type flagSpec struct {
	name string
	kind flagKind
	set  func(c *Config, v string) error
}

// knownFlags is a slice and not a map, so that every listing built from it is
// deterministic, per specs/053-determinism.md.
var knownFlags = []flagSpec{
	// Sent on every compile invocation.
	{"o", flagString, func(c *Config, v string) error { c.Output = v; return nil }},
	{"p", flagString, func(c *Config, v string) error { c.Package = v; return nil }},
	{"importcfg", flagString, func(c *Config, v string) error { c.ImportCfgFile = v; return nil }},
	{"lang", flagString, func(c *Config, v string) error { c.Lang = v; return nil }},
	{"buildid", flagString, func(c *Config, v string) error { c.BuildID = v; return nil }},
	{"goversion", flagString, func(c *Config, v string) error { c.GoVersion = v; return nil }},
	{"trimpath", flagString, setTrimPath},
	{"c", flagInt, func(c *Config, v string) error { return setInt(&c.Concurrency, v) }},
	{"pack", flagBool, func(c *Config, v string) error { return setBool(&c.Pack, v) }},
	{"nolocalimports", flagBool, func(c *Config, v string) error { return setBool(&c.NoLocalImports, v) }},
	// -shared is not rejected. The spike shows the go command sends it on
	// every invocation on darwin/arm64, because the platform requires
	// position independent code. A compiler that rejects it rejects every
	// build on the first target.
	{"shared", flagBool, func(c *Config, v string) error { return setBool(&c.Shared, v) }},

	// Sent conditionally.
	{"complete", flagBool, func(c *Config, v string) error { return setBool(&c.Complete, v) }},
	{"symabis", flagString, func(c *Config, v string) error { c.SymABIs = v; return nil }},
	{"asmhdr", flagString, func(c *Config, v string) error { c.AsmHdr = v; return nil }},
	{"std", flagBool, func(c *Config, v string) error { return setBool(&c.Std, v) }},
	{"embedcfg", flagString, func(c *Config, v string) error { c.EmbedCfgFile = v; return nil }},
	{"+", flagBool, func(c *Config, v string) error { return setBool(&c.CompilingRuntime, v) }},

	// Flags nanogo defines.
	{"N", flagCount, func(c *Config, v string) error { return setCount(&c.NoOptimize, v) }},
	{"l", flagCount, func(c *Config, v string) error { return setCount(&c.NoInline, v) }},
	{"S", flagCount, func(c *Config, v string) error { return setCount(&c.AsmListing, v) }},
	{"m", flagCount, func(c *Config, v string) error { return setCount(&c.OptDecisions, v) }},
	{"d", flagString, func(c *Config, v string) error { c.Debug = append(c.Debug, v); return nil }},
	{"fallback", flagBool, func(c *Config, v string) error { return setBool(&c.Fallback, v) }},
}

// rejectedFlags are flags nanogo understands and refuses. Each entry carries
// the reason, because the message is read by whoever added the package to the
// allowlist.
var rejectedFlags = []struct {
	name   string
	reason string
}{
	{"dynlink", "dynamic linking is out of scope (specs/045-linker.md)"},
	{"race", "nanogo emits no instrumentation"},
	{"msan", "nanogo emits no instrumentation"},
	{"asan", "nanogo emits no instrumentation"},
}

// FlagError reports a flag nanogo will not act on. specs/050-driver.md makes
// this an error and never a silent omission: a flag nanogo ignored produces a
// binary that differs from the one the build asked for, and the difference is
// invisible.
type FlagError struct {
	Flag   string
	Reason string
}

func (e *FlagError) Error() string {
	return "flag -" + e.Flag + ": " + e.Reason
}

// ParseCompile decodes a compile command line.
//
// It accepts both -flag value and -flag=value, because the go command uses
// both forms: it sends -trimpath and its value as two arguments, and -lang and
// -c with an equals sign inside -gcflags.
func ParseCompile(args []string) (*Config, error) {
	cfg := &Config{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" || a == "-" || !strings.HasPrefix(a, "-") {
			cfg.Files = append(cfg.Files, a)
			continue
		}

		name := strings.TrimPrefix(a, "-")
		// The go command never writes --flag, but a person testing the driver
		// by hand does, and gc accepts it.
		if len(name) > 1 && strings.HasPrefix(name, "-") {
			name = name[1:]
		}
		value, hasValue := "", false
		if j := strings.Index(name, "="); j > 0 {
			value, hasValue = name[j+1:], true
			name = name[:j]
		}

		if r := findRejected(name); r != "" {
			return cfg, &FlagError{Flag: name, Reason: r}
		}
		spec := findFlag(name)
		if spec == nil {
			return cfg, &FlagError{Flag: name, Reason: "not recognised by nanogo"}
		}

		switch spec.kind {
		case flagBool, flagCount:
			// A bool or count flag never consumes the next argument.
		default:
			if !hasValue {
				if i+1 >= len(args) {
					return cfg, &FlagError{Flag: name, Reason: "needs a value"}
				}
				i++
				value = args[i]
			}
		}
		if err := spec.set(cfg, value); err != nil {
			return cfg, &FlagError{Flag: name, Reason: err.Error()}
		}
	}
	return cfg, nil
}

func findFlag(name string) *flagSpec {
	for i := range knownFlags {
		if knownFlags[i].name == name {
			return &knownFlags[i]
		}
	}
	return nil
}

func findRejected(name string) string {
	for _, r := range rejectedFlags {
		if r.name == name {
			return r.reason
		}
	}
	return ""
}

func setBool(dst *bool, v string) error {
	if v == "" {
		*dst = true
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("%q is not a boolean", v)
	}
	*dst = b
	return nil
}

func setCount(dst *int, v string) error {
	if v == "" {
		*dst++
		return nil
	}
	// gc accepts -l=false as well as -l=2.
	if b, err := strconv.ParseBool(v); err == nil {
		if b {
			*dst = 1
		} else {
			*dst = 0
		}
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%q is not a count", v)
	}
	*dst = n
	return nil
}

func setInt(dst *int, v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%q is not an integer", v)
	}
	*dst = n
	return nil
}

// setTrimPath stores -trimpath verbatim and also splits it.
//
// The value is a list of old=>new rewrites joined by ";". The new side is
// empty in the last rewrite the go command builds, so an empty new side is
// valid and a parser that rejects it rejects every real build.
func setTrimPath(c *Config, v string) error {
	c.TrimPath = v
	c.TrimRewrites = nil
	for _, part := range strings.Split(v, ";") {
		if part == "" {
			continue
		}
		oldPath, newPath, found := strings.Cut(part, "=>")
		if !found || oldPath == "" {
			return fmt.Errorf("rewrite %q is not old=>new", part)
		}
		c.TrimRewrites = append(c.TrimRewrites, TrimRewrite{Old: oldPath, New: newPath})
	}
	return nil
}
