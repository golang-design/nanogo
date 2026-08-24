// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"os"
)

// LogEnv names a file that records what nanogo did with each package.
//
// The record exists because the substitution is invisible from outside. A
// build with -toolexec produces the same binary whether nanogo compiled a
// package or handed it to gc, so "the build passed" is not evidence that
// nanogo compiled anything. specs/051-build-integration.md makes the allowlist
// the project's progress metric, and this is how a run reports what the
// allowlist actually selected.
//
// It is off unless the variable is set, because the go command shows a
// compiler's standard error to the user and a line per package would be noise
// in every build.
const LogEnv = "NANOGO_LOG"

// Decision is what nanogo did with one compile invocation.
type Decision string

const (
	// DecisionCompiled means nanogo compiled the package itself.
	DecisionCompiled Decision = "compiled"

	// DecisionDelegated means the real gc compiled it.
	DecisionDelegated Decision = "delegated"

	// DecisionFailed means nanogo owned the package and could not compile it.
	DecisionFailed Decision = "failed"
)

// logDecision appends one line to the file named by LogEnv.
//
// The file is opened in append mode for each line, because the go command runs
// many compile processes at once and they share the file. One short write per
// process is what keeps the lines whole; holding the file open would not.
//
// A logging failure is dropped. The log is a report about the build and never
// a part of it, so a build that cannot write it must still succeed.
func logDecision(getenv func(string) string, d Decision, pkg, detail string) {
	name := getenv(LogEnv)
	if name == "" {
		return
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if pkg == "" {
		pkg = "?"
	}
	line := fmt.Sprintf("%s %s", d, pkg)
	if detail != "" {
		line += " " + detail
	}
	fmt.Fprintln(f, line)
}
