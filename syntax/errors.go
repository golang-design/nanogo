// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import "fmt"

// Error is one syntax error.
type Error struct {
	Pos Pos
	Msg string
}

func (e Error) Error() string { return e.Msg }

// ErrorHandler receives each error as it is found.
//
// The scanner and the parser report and continue. They never stop at the first
// error, because the conformance corpus of specs/004-conformance.md annotates
// several errors in one file and a front end that aborts cannot be run against
// it. Limiting the number printed belongs to the driver, not here.
type ErrorHandler func(err Error)

// PragmaHandler receives a //go: directive comment.
//
// The scanner routes the comment; the parser attaches the result to the next
// declaration. Returning nil means the directive is not one the compiler acts
// on. See specs/016-directives-and-pragmas.md.
type PragmaHandler func(pos Pos, blank bool, text string, current Pragma) Pragma

// Mode controls optional scanner and parser behaviour.
type Mode uint

const (
	// CheckBranches makes the parser resolve branch statement targets and
	// report a branch that has none.
	CheckBranches Mode = 1 << iota
)

// errorf reports a formatted error at p through h.
func errorf(h ErrorHandler, p Pos, format string, args ...any) {
	if h == nil {
		return
	}
	h(Error{Pos: p, Msg: fmt.Sprintf(format, args...)})
}
