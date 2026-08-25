// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package export

import (
	"fmt"

	"golang.design/x/nanogo/export/pkgbits"
)

// assert and panicf replace cmd/compile/internal/base, which reports a
// compiler bug by ending the process. nanogo's reader is called from the
// driver with a package and a file name in hand, so a broken stream has to
// come back as an error that names them. [Read] turns the panic into that
// error.
func assert(p bool) {
	if !p {
		panic(fmt.Errorf("export: assertion failed"))
	}
}

func assertf(p bool, format string, args ...any) {
	if !p {
		panicf(format, args...)
	}
}

func panicf(format string, args ...any) {
	panic(fmt.Errorf(format, args...))
}

// See cmd/compile/internal/noder.derivedInfo.
type derivedInfo struct {
	idx    pkgbits.Index
	needed bool
}

// See cmd/compile/internal/noder.typeInfo.
type typeInfo struct {
	idx     pkgbits.Index
	derived bool
}
