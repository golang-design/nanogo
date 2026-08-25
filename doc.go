// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package nanogo is a small compiler for the Go programming language.
//
// nanogo compiles Go source to native arm64 machine code and writes the object
// files the Go toolchain writes, so go tool link links its output against the
// real Go runtime into a program that runs.
//
// # Use
//
//	go install golang.design/x/nanogo/cmd/nanogo@latest
//	nanogo build .
//
// nanogo compiles the packages named on the command line and nothing else. The
// standard library and the runtime come from the installed Go toolchain, and
// go tool link writes the executable. A build reports the split, because a
// build in which nanogo compiled one package of fifty must not read as though
// nanogo built the program.
//
// Run "nanogo help" for the accepted subset and the flags. The build
// subcommand is not in a tagged release yet, so build cmd/nanogo from a clone
// of the repository until it is.
//
// # Scope
//
// nanogo accepts a part of Go. Integer arithmetic, comparisons, numeric
// conversions, calls including variadic and recursive calls, the control
// statements, slices, range, and a struct type declared in the package being
// compiled all compile. A capturing closure, defer, panic, append, a type
// assertion, a type switch, a conversion to an interface, and every map and
// channel operation are refused, each with a message that names the function,
// the position and the construct.
//
// Four limits have no such diagnostic and matter most. A program nanogo
// compiles does not initialize the packages it imports, so a package variable
// such as os.Stdout is nil at run time. The archive nanogo writes holds no
// export data, so no package may import a package nanogo compiled. Floating
// point reaches the arm64 encoder and panics there. The target is darwin/arm64
// and no other.
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
