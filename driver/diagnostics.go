// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"sort"

	"golang.design/x/nanogo/syntax"
)

// diagnostic is one message together with the position it belongs to.
//
// The position is the raw one, not the reported one. syntax.Pos states the
// rule: comparison uses raw and printing uses reported. A raw position is a
// byte offset into the file set, and the set lays its files out in the order
// the command line named them, so ordering by it orders by file and then by
// offset in one comparison. The printed text has already resolved the reported
// position, so a //line directive changes what a message says and never where
// it sits in the list.
type diagnostic struct {
	pos syntax.Pos
	err error
}

// diagnostics collects compiler messages in discovery order and hands them
// back in source position order.
//
// specs/052-diagnostics.md requires source order. Discovery order is the order
// the checker happened to visit declarations in: types2 reports a duplicate
// label where it finds it and an unused label when it finishes the function
// body, so the two come out inverted relative to the source. gc sorts for the
// same reason, in base.FlushErrors, and specs/004-conformance.md compares the
// first error of a rejected file against gc's, so an unsorted list disagrees
// with the reference compiler about which mistake the user made first.
//
// The limit is applied after the sort, not during collection. Truncating first
// would keep the ten errors the checker found first rather than the ten the
// user reads first, which is the same bug one step earlier.
type diagnostics struct {
	list []diagnostic
}

// add records one message at pos. A pos of [syntax.NoPos] is allowed and sorts
// to the end.
func (d *diagnostics) add(pos syntax.Pos, err error) {
	d.list = append(d.list, diagnostic{pos: pos, err: err})
}

// len reports how many messages were collected, before the limit is applied.
func (d *diagnostics) len() int { return len(d.list) }

// err returns the messages in source position order, joined, or nil if there
// are none.
//
// At most [maxReportedErrors] are returned. A message with no position sorts
// last rather than first: an error the compiler could not locate must not
// displace the one the user can act on.
func (d *diagnostics) err() error {
	if len(d.list) == 0 {
		return nil
	}
	sort.SliceStable(d.list, func(i, j int) bool {
		a, b := d.list[i].pos, d.list[j].pos
		if a.IsKnown() != b.IsKnown() {
			return a.IsKnown()
		}
		return a < b
	})
	n := len(d.list)
	if n > maxReportedErrors {
		n = maxReportedErrors
	}
	errs := make([]error, n)
	for i := range errs {
		errs[i] = d.list[i].err
	}
	return errors.Join(errs...)
}
