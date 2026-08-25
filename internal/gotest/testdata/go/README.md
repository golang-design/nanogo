# Go's own test corpus, vendored

**The files under `test/` are not nanogo's work.** They are the Go Authors',
copied verbatim from a Go release, and they are redistributed here under Go's
BSD-3-Clause licence. `LICENSE` and `PATENTS` beside this file are Go's, also
verbatim, and they govern everything in this directory.

Every file under `test/` keeps its original header:

```go
// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
```

nanogo's own header (`// Copyright 2026 The golang.design Initiative Authors.`)
appears on no file in this directory. A nanogo header on a file the Go Authors
wrote would be a false claim of authorship.

## What is here

| Path | What it is | Whose |
| --- | --- | --- |
| `LICENSE` | Go's licence | The Go Authors |
| `PATENTS` | Go's patent grant | The Go Authors |
| `test/*.go` | 356 files from `$GOROOT/test` | The Go Authors |
| `README.md` | this file | golang.design |

The subdirectories of `$GOROOT/test` (`fixedbugs/`, `typeparam/`, `chan/`,
`syntax/`, the `.dir` bundles, and the rest) are **not** copied. The harness in
`internal/gotest` runs the single-file kinds only, and a directory nothing runs
would be redistributed source with no purpose.

## Provenance

| | |
| --- | --- |
| Go release | `go1.27.0` |
| `$GOROOT/VERSION` | `go1.27.0` / `time 2026-08-18T21:24:23Z` |
| Copied on | 2026-08-25 |
| Files | 356 |

## Why vendored rather than read from `$GOROOT/test`

A gate whose inputs come from whichever Go happens to be installed is not a
gate. It changes when the toolchain changes, it cannot be reproduced from a
checkout, and a release cannot honestly claim to have passed it. These bytes are
in the repository, so the corpus a tag was cut against is the corpus in that
tag.

## Do not edit these files

A vendored file that has been changed still carries the Go Authors' header and
therefore misrepresents what they wrote. Anything a file needs in order to run
under nanogo's harness belongs in the harness or in a table beside it, never in
the file. If a file genuinely has to be altered, copy it into `internal/gotest`
under a different name, with nanogo's own header and a comment saying what it
was derived from and why.

## Refreshing

From a checkout of this repository, with the target Go release installed and
first on `PATH`:

```sh
G=$(go env GOROOT)
rm -rf internal/gotest/testdata/go/test
mkdir -p internal/gotest/testdata/go/test
cp "$G/LICENSE" "$G/PATENTS" internal/gotest/testdata/go/
cp "$G"/test/*.go internal/gotest/testdata/go/test/
```

Then update the provenance table above, and run

```sh
NANOGO_REQUIRE_CORPUS=1 NANOGO_REQUIRE_LINK=1 go test ./internal/gotest/
```

which fails until the ratchet in `internal/gotest/testdata/ratchet.txt` is
refreshed for the new corpus. Refresh it with `NANOGO_REFRESH_RATCHET=1` and
read the diff before committing: a file that stopped passing is a regression,
not a refresh.
