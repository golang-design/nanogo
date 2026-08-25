// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist_test

import (
	"go/build"
	"strings"
	"testing"
)

// dist imports nothing from the rest of nanogo, and this is where that rule is
// carried.
//
// specs/054-distribution.md: dist produces the tree and driver consumes it. An
// import in either direction would stop driver from calling dist.TallyLine,
// which is the one function a "nanogo version" needs, and the cycle would not
// appear until somebody made that change in a different package. A rule stated
// only in a comment is one the next person reverts by accident.
func TestDistImportsNothingFromNanogo(t *testing.T) {
	const self = "golang.design/x/nanogo/dist"
	pkg, err := build.Import(self, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.GoFiles) == 0 {
		t.Fatal("no files were read, so this test proves nothing")
	}
	for _, imp := range pkg.Imports {
		if strings.HasPrefix(imp, "golang.design/x/nanogo/") {
			t.Errorf("dist imports %s; it must depend on nothing in this module, "+
				"or driver cannot call dist.TallyLine", imp)
		}
	}
	t.Logf("dist reads %d files and imports %d packages, all of them standard", len(pkg.GoFiles), len(pkg.Imports))
}
