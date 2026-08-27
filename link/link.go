// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package link reads Go object files and links them.
//
// specs/045-linker.md is the design. The package is the reading side of
// the format the obj package writes: obj emits goobj objects and cmd/link
// consumes them today, and this package is what replaces cmd/link.
//
// # What is built
//
// The object reader and the reachability pass. Address assignment,
// pclntab, moduledata and the output writers are not built.
//
// # Complete accounting
//
// An object this package cannot account for completely is refused. A
// partial parse is the failure that costs the most later, because the
// stage that needed the dropped field is written against a picture that is
// missing it and nothing says so. Every block is checked against the
// lengths the index blocks state, every string reference must lie inside
// the string region, every symbol reference must resolve in its index
// space, and an auxiliary entry of an unknown type is a refusal.
//
// # The package path comes from the caller
//
// An object records the paths of the packages it references and not its
// own. cmd/link takes the path from the import configuration, and so does
// this package: [ReadObject] and [ReadArchive] both take it as an
// argument.
package link

import "fmt"

// A FormatError is a refusal to read an object.
//
// It names the file and the byte offset, because a caller reading an
// archive of thirteen members needs to know which member and where.
type FormatError struct {
	File string // the file or archive member the refusal is about
	Off  int64  // the byte offset in that file, or -1 when none applies
	Msg  string
}

func (e *FormatError) Error() string {
	if e.Off < 0 {
		return fmt.Sprintf("link: %s: %s", e.File, e.Msg)
	}
	return fmt.Sprintf("link: %s: at byte %d: %s", e.File, e.Off, e.Msg)
}

// errorf returns a refusal at an offset.
func errorf(file string, off int64, format string, args ...any) error {
	return &FormatError{File: file, Off: off, Msg: fmt.Sprintf(format, args...)}
}

// errAt returns a refusal with no meaningful offset.
func errAt(file string, format string, args ...any) error {
	return &FormatError{File: file, Off: -1, Msg: fmt.Sprintf(format, args...)}
}
