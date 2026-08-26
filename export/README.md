<!--
Copyright 2026 The golang.design Initiative Authors.
All rights reserved. Use of this source code is governed by
a BSD-style license that can be found in the LICENSE file.
-->

# The export data reader and writer

This package reads the export data `gc` writes and writes export data `gc`
reads, so that nanogo can compile a package that imports one `gc` compiled and
a package `gc` compiles can import one nanogo compiled. See
[specs/015-export-data.md](../specs/015-export-data.md) for why the format is
`gc`'s and not nanogo's own.

It is a port, not a rewrite. The format is undocumented outside its
implementation, so a second implementation written from a description would be
a second guess. This file is the record of what was copied and of every place
the copy differs, so that a re-port against a later release is a file-to-file
diff.

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
| `writer.go` | `src/cmd/compile/internal/noder/writer.go` and `linker.go`; see below |

## Which upstream reader, and why

`gc`'s export data has two types-only readers in the Go tree, and they are the
same reader twice: `go/internal/gcimporter` produces `go/types` packages and
`cmd/compile/internal/importer` produces `cmd/compile/internal/types2`
packages. nanogo's checker is a fork of `types2`, so the second one is a port
of import paths and positions rather than a translation between two type APIs.
[specs/015](../specs/015-export-data.md) sizes the port by the same row: 772
non-test lines for the `types2` reader against 925 for its `go/types` twin.
File for file, `reader.go` here is 645 lines against `ureader.go`'s 642.

The third reader, `cmd/compile/internal/noder`, is the one `gc` itself uses and
is the one that carries function bodies. It is not ported. What that defers is
in [specs/015](../specs/015-export-data.md): inlining across packages
([024](../specs/024-inlining-and-devirtualization.md)) and instantiating a
generic declared in another package ([013](../specs/013-generics.md)) both need
bodies, and this reader has none.

`gc` reads what nanogo writes with two readers, not one. `noder.readPackage`
walks the object list and `importer.ReadPackage`, the reader ported here,
builds the types. Both run over nanogo's bytes in
`crossread_test.go`.

## Divergences

Every entry is a place the copy differs from upstream and the reason. A line
that is not here is upstream's.

### The container, `pkgbits/`

| Change | Why |
| --- | --- |
| `encoder.go`: `NewPkgEncoder` takes no frame count and `Encoder.Sync` is a no-op | Upstream can write a sync marker before every field. nanogo writes none, for the reason the decoder's `Sync` records from the other side: the ported reader desyncs on marked data at the first object that stands in for another package's declaration, so data nanogo marked is data nanogo cannot read. The calls stay, so the writer reads as the mirror of the reader. |
| `encoder.go`: `DumpTo` returns its error | Upstream asserts it away. The caller is writing a file the build asked for. |
| `encoder.go`: `fmtFrames` is not ported | It formats the writer's stack for a sync marker, and there are none. |
| `sync.go`: `fmtFrames` and `walkFrames` are dropped | They format the reader's own backtrace for a desync report, and the panic below carries that backtrace already. |
| `decoder.go`: `SyncMarkers`, `TotalElems`, `Int` and `Strings` are dropped | The linker calls them; neither half here does. |
| `codes.go`: the `Code` interface and the `Marker`/`Value` methods are back | Only the encoder calls them, and there is an encoder now. |
| `decoder.go`: `PeekPkgPath` and `PeekObj` are back | They answer what an element is without decoding it, which is what a writer checking its own output needs. |
| `decoder.go`: `NewPkgDecoder`'s header reads report truncation by name | Upstream asserts. A file the build handed nanogo has to be reported as a file, and "assertion failed" names nothing. |
| `decoder.go`: `Decoder.checkErr` names the package and the element | Upstream reports the error alone, because `gc` prints it next to the file it was reading. nanogo's driver holds only the package it was asked to compile. |
| `decoder.go`: `Decoder.Sync` panics instead of calling `os.Exit(1)` | `gc` treats a desync as a compiler bug and ends the process. nanogo is a library the driver calls, so the same event has to come back as an error about the package being compiled. |

### The reader, `reader.go`

| Change | Why |
| --- | --- |
| Import paths point at nanogo's `types2`, `syntax` and `export/pkgbits` | What the port is for: nanogo's checker is a fork of `types2`. |
| `pos` consumes the position and returns `syntax.NoPos`; `posBases`, `posBase` and `posBaseIdx` are deleted | nanogo's `syntax.Pos` is an offset into the `FileSet` the compiled files were parsed with ([specs/010](../specs/010-scanner-and-positions.md)), and a file in another package is not in it. The fields are still read: they are inline in the element, and skipping them desyncs everything after. The position base is a reference to another element, so that element is never visited. The writer has the same gap from the other side: it writes every position as absent, so `SectionPosBase` is empty and a `gc` diagnostic about a declaration nanogo compiled says the position is unknown. |
| `base.FatalfAt`, `base.Fatalf` and `base.Assertf` become `panicf` and `assertf` | Same reason as `Decoder.Sync`. |
| `enableAlias` and its branch are removed | It selects between the alias representations of two `go/types` releases. nanogo is pinned to one. |
| `readerTypeBound` is removed | Unused upstream as well. |
| `ObjFunc` decodes a promoted generic method instead of asserting it cannot appear | See below. |

### The writer, `writer.go`

The writer is not a port of one file. `noder/writer.go` encodes a package from
the type checker's output and produces the **stub** form, in which a
declaration of another package is a name with no definition. `noder/linker.go`
turns that into the **linked** form by copying each stub's definition out of
the archive it came from, and the linked form is the only one that ever
reaches a file. `writer.go` produces the linked form directly, so its shape
comes from `writer.go` for the public part of a declaration and from
`linker.go` for the extension data.

| Change | Why |
| --- | --- |
| No stub for a declaration of another package | A file's export data has no stub left except the universe's and unsafe's, and every reader asserts it. nanogo has no linker pass, so a foreign declaration the exported surface reaches is written out in full at the point it is reached. |
| The public root lists every object in the file | This is `linker.go`'s list and not `writer.go`'s. `gc` builds its stub resolution table from it, so a root naming only nanogo's own declarations leaves `gc` unable to resolve, say, `io.Reader` in a `bufio` signature. |
| `pos` writes an absent position | The mirror of the reader's gap. See below. |
| No function body, and the private root lists none | The body writer is [specs/015](../specs/015-export-data.md)'s other half. |
| A generic declaration is refused by name | See below. |
| `funcExt` writes no `//go:` directive | The driver records the fourteen verbs its handler recognises, and their positions (`driver/pragma.go`, [specs/016](../specs/016-directives-and-pragmas.md)), and nothing carries them this far: the flag bits there are nanogo's own numbering and this field is read with `gc`'s. |
| `funcExt` writes no linkname | `//go:linkname` is not one of those fourteen verbs, so the driver records nothing for the writer to drop ([016](../specs/016-directives-and-pragmas.md)). |
| `funcExt` writes `ABIInternal` and an empty escape note per receiver and parameter | Every function nanogo compiles is ABIInternal ([specs/030](../specs/030-abi.md)), and an empty note parses as "leaks to the heap", which is what a caller must assume when no escape analysis has run ([specs/023](../specs/023-escape-analysis.md) is unbuilt). |
| `typeExt` writes -1 for both type descriptor symbol indices | The importer finds them by name. It is what `gc` writes before it has assigned indices of its own. |
| The private root carries the initialisation flag and no function body | `driver/inittask.go` decides the flag: an importer orders its own record after this package's only when it is set. The body list is empty because there is no body writer. |
| A local alias is stripped to its right-hand side | Upstream does the same, to keep two local aliases from colliding on one symbol. |
| An empty interface is written as a reference to `any` | Both spellings are one type. The reader has already lost the difference: `types2.NewInterfaceType` returns the one canonical empty interface for `interface{}` and for `any`, so the writer cannot tell them apart. Only the printed form differs. |

### What the writer made required elsewhere

A package that can be imported owes an importer more than its export data.
`gc` refers to an imported type twice, directly for the runtime type descriptor and
through DWARF for `go:info.<path>.<Type>`, and `cmd/link` builds the second out
of the first, so both come back to `type:<path>.<Type>`. nanogo writes no
descriptor for a declared type, and `driver/types.go` refuses such a package by
name rather than letting the build fail at link time. The gap is
[specs/032](../specs/032-type-descriptors-and-itabs.md) and not this package's;
[specs/015](../specs/015-export-data.md) records the measurement.

### Why a generic declaration is refused

A generic declaration cannot be written without a function body, and the
format says so rather than the implementation guessing it. `linker.go` writes
the relocated extension data for a function as `Bool(true)` followed by the
ABI, the escape notes and the inlining cost; but it takes that branch only
when the object has a definition, and a generic function never does. For a
generic it copies the stub extension data verbatim, which is `Bool(false)`
followed by a reference to a `SectionBody` element. There is no third shape.

`gc`'s reader agrees from the other side: for an instantiation it sets
`name.Defn` and then asserts `name.Defn == nil` inside the `Bool(true)`
branch, so a generic written that way fails on the first importer that
instantiates it, with a message that names neither the generic nor the
package.

So the writer refuses, and the message names the declaration. 100 of the 375
standard library packages are refused for it, and 4 of the 27 packages with
export data in the closure of an empty `main`: `internal/abi`,
`internal/bytealg`, `internal/runtime/atomic` and `runtime`. All four are
refused by the driver for other reasons as well.

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
`writer.go`'s `Definition` builds the same member and `driver/archive.go`
writes it first, because that assumption *is* in `internal/exportdata`'s
reader and nanogo has to satisfy it.

## The promoted generic method

A method with type parameters of its own is new in Go 1.27
(`types2/resolver.go` gates it on the `go1.27` language version). `gc` promotes
one to a package-scope object under a name no source can spell, such as
`(*List).Zip` or `Point.Map`, so it appears in the export data whether an
importer wants it or not. The two upstream readers do two different things with
it, and this reader does a third.

`go/internal/gcimporter` drops it. Its `objIdx` returns early on any object
name that holds a `.`, so the object is never decoded and never reaches a
scope, and importing a package that declares one succeeds with the method
present on its defining type and absent from the package scope.

`cmd/compile/internal/importer`, the reader ported here, has no such early
return. It inserts the object lazily under the unspellable name and asserts the
standalone bool is false when something decodes it. A reader that looks up only
names a source can spell never decodes it, so the assertion does not fire
there.

nanogo cannot leave it lazy. `writer.go` looks every name in the scope up
before it asks whether the object is exported, so every name the export data
declares is decoded on every package nanogo writes, and an assertion would
refuse the package. `ObjFunc` therefore decodes the promoted object, and the
scope holds it: on the fixture in `export_test.go` this reader's scope holds 18
names against `go/internal/gcimporter`'s 16, the two extra being `(*List).Zip`
and `Point.Map`. The type that declares the method decodes it a second time
through `ObjType`, and that copy is the one in the method set.

## What sync-marked export data costs both readers

`gc` built with `-d=syncframes` writes a sync marker before every field.
Reading such an archive works until the first object that stands in for a
declaration of another package, where the marker stream desyncs.
`go/internal/gcimporter` fails on the same archive at the same offset, so this
is upstream's and not the port's. `export_test.go`'s marker fixture has no
import for that reason, and ordinary export data has no markers at all.
