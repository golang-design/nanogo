// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package nanogo is a small compiler for the Go programming language.
//
// nanogo compiles a Go package and the result runs. Point the go command at
// it and name the packages it owns:
//
//	go install golang.design/x/nanogo/cmd/nanogo@latest
//	NANOGO_ALLOWLIST=./allowlist go build -toolexec=nanogo ./...
//
// Everything not on the allowlist is handed to the ordinary Go compiler, so a
// build is part nanogo and part gc.
//
// What nanogo accepts is far narrower than Go, and it refuses the rest with an
// error naming the function and the construct rather than emitting something
// wrong. SSA construction has no assignment statement, which alone stops about
// four functions in five of the standard library. One target, darwin/arm64.
// Run "nanogo help" for the current subset. Nothing here is stable.
//
// The measure of the project is a fixed point. nanogo compiles its own
// source, and the compiler that results is byte-identical to itself. See
// specs/001-bootstrap-gates.md for what that does and does not prove.
package nanogo
