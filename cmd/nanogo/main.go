// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Command nanogo is the nanogo compiler driver.
//
// The go command runs it in place of each toolchain invocation:
//
//	go build -toolexec=nanogo ./...
//
// nanogo compiles the packages named by NANOGO_ALLOWLIST and runs the real
// tool for everything else, so a build is part nanogo and part gc. Run
// "nanogo help" for what it compiles and what it refuses, and "nanogo version"
// for the release it is object compatible with.
//
// nanogo has one target, darwin/arm64. See specs/050-driver.md and
// specs/051-build-integration.md. All the logic is in
// golang.design/x/nanogo/driver, so that it is testable without a process.
package main

import (
	"os"

	"golang.design/x/nanogo/driver"
)

func main() {
	os.Exit(driver.Run(driver.Env{
		Args:   os.Args[1:],
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
	}))
}
