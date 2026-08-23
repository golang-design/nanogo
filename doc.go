// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package nanogo is a small compiler for the Go programming language.
//
// The project is at the design stage. No compiler exists yet: this package
// holds the module identity while the specs in the specs directory are
// reviewed. Nothing here is stable.
//
// The measure of the project is a fixed point. nanogo compiles its own
// source, and the compiler that results is byte-identical to itself. See
// specs/001-bootstrap-gates.md for what that does and does not prove.
package nanogo
