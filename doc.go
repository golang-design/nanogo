// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package nanogo is a small compiler for the Go programming language.
//
// nanogo compiles Go source to arm64 machine code and writes the object files
// the Go toolchain writes, so go tool link links its output against the real
// Go runtime into a program that runs.
//
// nanogo is under construction. It compiles a small part of Go, so for most
// programs the answer today is that it cannot compile them, and it says so by
// name rather than emitting code it cannot emit correctly.
//
// # Use
//
//	go install golang.design/x/nanogo/cmd/nanogo@latest
//	nanogo build .
//
// There is no tagged release. The command above installs the current commit.
// go list and go tool link must be on PATH: the go command resolves the
// packages, and go tool link writes the executable.
//
// nanogo compiles the packages named on the command line and nothing else.
// The standard library and the runtime come from the installed Go toolchain,
// and go tool link writes the executable. Every build reports that split,
// because a build in which nanogo compiled one package of twenty-eight must
// not read as though nanogo built the program.
//
// # Scope
//
// Run "nanogo help" for the list this paragraph summarises. Integer and
// floating-point arithmetic, comparisons, numeric conversions, calls
// including variadic, recursive and method calls, the control statements,
// slices with append, strings with their conversions, maps, channels with
// select, interfaces with method calls, conversions between them, assertions
// and type switches naming either a concrete type or another interface,
// range over any of them, a struct type declared in the package being
// compiled, package-level variables, init functions, defer and go with their
// arguments, print and println, a closure with or without captures, and a
// declared function used as a value all compile.
//
// A generic function the compiling package declares is stencilled fully: one
// compiled body per distinct list of type arguments, named pkg.F[int], with no
// dictionary and no run-time indirection.
//
// Defer of a builtin, a method of a generic type, a method with type
// parameters of its own, an instantiation of a generic another package
// declared, a package with assembly in it, and a package with a go:embed
// directive in it are refused, each with a message that names the function,
// the position and the construct.
//
// No program the probe corpus reaches behaves differently from the one gc
// builds. That is a measurement over 96 programs compiled twice and run
// twice, not a proof. Two costs it cannot sample for remain: a pointer to a
// local that escapes its frame outlives that frame, and a pointer store emits
// no write barrier. "nanogo help" describes both, and
// internal/audit/testdata/probes is the corpus.
//
// nanogo emits arm64 machine code, and a build for another GOARCH is refused
// before anything is compiled. darwin/arm64 is the target the tests run on.
//
// Nothing here is stable. Do not put a nanogo-compiled package into a program
// you care about.
//
// # Reading the compiler
//
// The pipeline is scanner, parser, type checker, typed IR, SSA, machine
// operations, object file. The packages follow it. syntax holds the front end,
// types2 the forked type checker, ir the typed tree, ssa the graph and its
// passes, ssagen the machine code, and obj the object writer. loader resolves
// packages and driver is the command line.
//
// The measure of the project is a fixed point that nanogo has not reached:
// nanogo compiles its own source, and the compiler that results is
// byte-identical to itself. See specs/001-bootstrap-gates.md for what that
// does and does not prove.
package nanogo
