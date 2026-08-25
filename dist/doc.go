// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package dist builds and audits a nanogo distribution.
//
// A distribution is the tree specs/054-distribution.md describes and
// [golang.design/x/nanogo/driver.FindRoot] resolves:
//
//	nanogo/bin/nanogo
//	nanogo/src/...                 the pinned standard library sources
//	nanogo/pkg/darwin_arm64/...    the archives built from them
//	nanogo/VERSION
//
// The package answers two questions that a tarball cannot be trusted about
// otherwise. Which release the tree is, which VERSION records; and which
// compiler produced each archive in pkg, which every archive records in itself.
//
// The second question is the reason this package exists. nanogo writes gc's
// object header verbatim, because the linker checks that header and a build is
// part nanogo and part gc (specs/051-build-integration.md). So the bytes of an
// archive do not say who wrote them, and a distribution of gc-compiled
// archives under a nanogo name would look exactly like a distribution nanogo
// compiled.
//
// pkg/GOOS_GOARCH/MANIFEST therefore names the producer of every archive
// beside it, with the SHA-256 that binds each record to the bytes it
// describes. An archive with no record fails the whole tally rather than being
// assumed to be gc's, and an archive whose hash has moved fails it too. The
// archives themselves are untouched: an earlier design carried the record
// inside each one and broke go tool nm and go tool objdump, which refuse any
// archive member that is not an object. specs/054-distribution.md has that
// account.
//
// dist imports nothing from driver. driver consumes a tree and dist produces
// one, and an import in either direction would stop the other from calling
// [TallyLine].
package dist
