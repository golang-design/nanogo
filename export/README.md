<!--
Copyright 2026 The golang.design Initiative Authors.
All rights reserved. Use of this source code is governed by
a BSD-style license that can be found in the LICENSE file.
-->

# The export data reader

This package reads the export data `gc` writes, so that nanogo can compile a
package that imports one `gc` compiled. See
[specs/015-export-data.md](../specs/015-export-data.md) for why the format is
`gc`'s and not nanogo's own.

It is a port, not a rewrite. The format is undocumented outside its
implementation, so a second implementation written from a description would be
a second guess. This file is the record of what was copied and of every place
the copy differs, so that a re-port against a later release is a file-to-file
diff and not an archaeology exercise.

## Upstream revision

| Field | Value |
| --- | --- |
| Repository | https://go.googlesource.com/go |
| Release | `go1.27.0` |
| Date | Tue Aug 18 21:24:23 2026 +0000 |

The release rather than a commit, because the sources were copied out of the
installed toolchain's `GOROOT`. That is also the toolchain nanogo is pinned to
([`driver.PinnedGoVersion`](../driver/version.go)), so the code that reads the
format and the code that wrote it come from the same tree by construction.

## What came from where

| Here | Upstream |
| --- | --- |
| `pkgbits/` | `src/internal/pkgbits/`, the container |
| `reader.go` | `src/cmd/compile/internal/importer/ureader.go` |
| `support.go` | `src/cmd/compile/internal/importer/support.go` |
| `read.go` | written here; see below |

## Which upstream reader, and why

`gc`'s export data has two types-only readers in the Go tree, and they are the
same reader twice: `go/internal/gcimporter` produces `go/types` packages and
`cmd/compile/internal/importer` produces `cmd/compile/internal/types2`
packages. nanogo's checker is a fork of `types2`, so the second one is a port
of import paths and positions rather than a translation between two type APIs.
[specs/015](../specs/015-export-data.md) names only the first and sizes the
work by it, which overstates it: the ported reader is 637 lines against that
spec's 1,259.

The third reader, `cmd/compile/internal/noder`, is the one `gc` itself uses and
is the one that carries function bodies. It is not ported. What that defers is
in [specs/015](../specs/015-export-data.md): inlining across packages
([024](../specs/024-inlining-and-devirtualization.md)) and instantiating a
generic declared in another package ([013](../specs/013-generics.md)) both need
bodies, and this reader has none.

## Divergences

Every entry is a place the copy differs from upstream and the reason. A line
that is not here is upstream's.

### The container, `pkgbits/`

| Change | Why |
| --- | --- |
| `encoder.go` not ported | nanogo has no writer. [specs/015](../specs/015-export-data.md)'s writer half brings it. |
| `codes.go`: the `Code` interface and the `Marker`/`Value` methods are dropped | Only the encoder calls them. |
| `sync.go`: `fmtFrames` and `walkFrames` are dropped | They format the reader's own backtrace for a desync report, and the panic below carries that backtrace already. |
| `decoder.go`: `SyncMarkers`, `TotalElems`, `Int`, `Strings`, `PeekPkgPath`, `PeekObj` are dropped | The linker and the writer call them; this reader does not. |
| `decoder.go`: `NewPkgDecoder`'s header reads report truncation by name | Upstream asserts. A file the build handed nanogo has to be reported as a file, and "assertion failed" names nothing. |
| `decoder.go`: `Decoder.checkErr` names the package and the element | Upstream reports the error alone, because `gc` prints it next to the file it was reading. nanogo's driver holds only the package it was asked to compile. |
| `decoder.go`: `Decoder.Sync` panics instead of calling `os.Exit(1)` | `gc` treats a desync as a compiler bug and ends the process. nanogo is a library the driver calls, so the same event has to come back as an error about the package being compiled. |

### The reader, `reader.go`

| Change | Why |
| --- | --- |
| Import paths point at nanogo's `types2`, `syntax` and `export/pkgbits` | The whole point of the port. |
| `pos` consumes the position and returns `syntax.NoPos`; `posBases`, `posBase` and `posBaseIdx` are deleted | nanogo's `syntax.Pos` is an offset into the `FileSet` the compiled files were parsed with ([specs/010](../specs/010-scanner-and-positions.md)), and a file in another package is not in it. The fields are still read: they are inline in the element, and skipping them desyncs everything after. The position base is a reference to another element, so that element is never visited. |
| `base.FatalfAt`, `base.Fatalf` and `base.Assertf` become `panicf` and `assertf` | Same reason as `Decoder.Sync`. |
| `enableAlias` and its branch are removed | It selects between the alias representations of two `go/types` releases. nanogo is pinned to one. |
| `readerTypeBound` is removed | Unused upstream as well. |
| `ObjFunc` reads a generic method instead of asserting there is none | See below. |

### The archive, `read.go`

Upstream splits this between `cmd/compile/internal/importer/gcimporter.go` and
`internal/exportdata`. Neither is ported.

`gcimporter.go` finds a package's archive with `go/build`'s search rules, which
is how a tool that was handed an import path and a source directory locates a
build it did not run. nanogo is handed the file: the go command writes
`-importcfg` and [specs/050](../specs/050-driver.md) makes reading it the
driver's job. Porting the search would add a second answer to a question the
build already answered.

`internal/exportdata` reads the archive through a `bufio.Reader` and assumes
`__.PKGDEF` is the first member. `read.go` walks the members instead, because
that assumption is not in the format, and reports each malformed shape by name.

## What upstream cannot read and this can

A method with type parameters of its own is new in Go 1.27. `gc` promotes one
to a package-scope object under a name no source can spell, such as
`(*List).Zip`, so it appears in the export data whether an importer wants it or
not. Both upstream types-only readers assert that it cannot appear, so both
fail on any package that declares one. `reader.go` decodes it.

The failure is reproducible against the Go tree: `go/internal/gcimporter`
panics at `ureader.go`'s `assert(!r.Bool())` on the archive of a package with a
generic method.

## What neither can read

`gc` built with `-d=syncframes` writes a sync marker before every field.
Reading such an archive works until the first object that stands in for a
declaration of another package, where the marker stream desyncs.
`go/internal/gcimporter` fails on the same archive at the same offset, so this
is upstream's and not the port's. `export_test.go`'s marker fixture has no
import for that reason, and ordinary export data has no markers at all.
