// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package types2

import "golang.design/x/nanogo/syntax"

// This file is the whole cost of nanogo's compact position model inside the
// checker.
//
// Upstream's syntax.Pos carries a *PosBase, so a position knows its own file
// and prints itself. nanogo's Pos is a bare uint32 and is resolved through a
// FileSet, because a compiler holds one position per IR node and per SSA value
// and 4 bytes against 16 is worth having. See specs/010-scanner-and-positions.md.
//
// The checker therefore needs the FileSet at every point that prints or
// resolves a position. It arrives on Config.Fset and is reached through these
// helpers. specs/012-type-checking.md measured 34 such points before the port
// started, which is why the model difference was judged affordable.
//
// Every helper tolerates a nil checker and a nil FileSet, and returns an
// unknown position in that case. The checker is nil on the formatting paths
// that upstream also allows to run without one, and the FileSet is nil in unit
// tests that never look at a position.

// fset returns the FileSet positions resolve through, or nil.
func (check *Checker) fset() *syntax.FileSet {
	if check == nil || check.conf == nil {
		return nil
	}
	return check.conf.Fset
}

// position resolves p for printing, with any //line directive applied.
func (check *Checker) position(p syntax.Pos) syntax.Position {
	return positionOf(check.fset(), p)
}

// rawPosition resolves p ignoring every //line directive, so it names the
// file on disk and the line in it.
//
// Upstream reaches the same thing through Pos.FileBase and Pos.Line, which
// skip the directive chain. Use this where upstream uses those, and use
// position where upstream uses RelFilename or prints the position.
func (check *Checker) rawPosition(p syntax.Pos) syntax.Position {
	fset := check.fset()
	if fset == nil {
		return syntax.Position{}
	}
	return fset.RawPosition(p)
}

// srcFile returns the file p belongs to, or nil.
//
// It is the key of the per-file version map, which upstream keys by
// *syntax.PosBase.
func (check *Checker) srcFile(p syntax.Pos) *syntax.SrcFile {
	fset := check.fset()
	if fset == nil {
		return nil
	}
	return fset.SrcFile(p)
}

// positionOf resolves p through fset, which may be nil.
func positionOf(fset *syntax.FileSet, p syntax.Pos) syntax.Position {
	if fset == nil {
		return syntax.Position{}
	}
	return fset.Position(p)
}

// posString is the printed form of p. An unresolvable position prints as
// "<unknown position>", which is what upstream prints for a position with no
// base.
func posString(fset *syntax.FileSet, p syntax.Pos) string {
	return positionOf(fset, p).String()
}
