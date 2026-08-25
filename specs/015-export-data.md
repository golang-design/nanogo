---
title: "Export data: gc-compatible package summaries"
status: in progress
layer: front end
gate: G1
depends_on:
  - 012-type-checking.md
  - 013-generics.md
---

# Export data

Compiling a package requires knowing the exported surface of every package it
imports, without re-checking their source. That summary is export data.

nanogo's export data is **`gc`'s format**, read and written. That decision comes
from [000](000-decisions.md) decision 11 and is what makes the incremental
bring-up of [051](051-build-integration.md) possible: a package compiled by
nanogo can be imported by a package compiled by `gc`, and the reverse.

## The reader is built and the writer is not

`export/` reads gc's export data and produces the `*types2.Package` values
nanogo's checker imports. `driver/importer.go` turns an import path into a file
through `-importcfg` and hands it to that reader, so a package that imports
compiles. `export/README.md` records what was ported and every divergence.

There is no writer. The archive nanogo produces carries the object and no
`__.PKGDEF` (`driver/archive.go`), so a package nanogo compiled cannot be
imported. The consequence is the reverse of the one this spec used to record:
nanogo takes packages from the top of the import graph downwards, and the
allowlist grows towards the leaves rather than away from them.

The `types2` fork carries a hand-written `srcimporter_test.go`, which
type-checks an imported package from source. Upstream's `importer_test.go` is
still skipped, because it names upstream's importer and not this one.

**The importcfg section below is also built.** `driver/importcfg.go` parses all
four directives, keeps them in separate tables as specified, and rejects an
unknown directive. It sits in `driver` and not in an export package, because
the driver is what reads the command line and the parser was built with
[050](050-driver.md)'s flag handling.

### What the reader does not carry

Positions and bodies, and each one costs something named elsewhere in the deck.

An imported declaration decodes to `syntax.NoPos`. nanogo's position is an
offset into the `FileSet` the compiled files were parsed with
([010](010-scanner-and-positions.md)), and a file in another package has no
place in it. A diagnostic about an imported declaration therefore says the
position is unknown, which is a gap in [052](052-diagnostics.md). Closing it
needs a `FileSet` that can hold a foreign file's line table, which is a change
to `syntax` and not to the reader.

No function body of any kind is read, because the reader is the types-only one.
That blocks [024](024-inlining-and-devirtualization.md) entirely, and it blocks
the part of [013](013-generics.md) where a package instantiates a generic
another package declares. The row below that calls generic bodies required data
is still true, and it is still unmet.

## What is in it

| Contents | Needed by | Note |
| --- | --- | --- |
| Exported constants, variables, types, functions, methods | every importer | the obvious part |
| Unexported types reachable from exported ones | every importer | a field's type must be describable even when the field is not exported |
| Inlinable function bodies | [024](024-inlining-and-devirtualization.md) | inlining across packages needs the body |
| **Generic function and type bodies** | [013](013-generics.md) | not an optimisation; an importer that instantiates cannot proceed without it |
| Position information | [052](052-diagnostics.md), [046](046-debug-info.md) | errors that point into an imported package |

The generic row is the one that changes the character of the format. Under full
stenciling, a body is not an optional extra that a fast build can skip. It is
required data, and a build that drops it fails rather than deoptimises.

## Why gc's format and not nanogo's own

The alternative, that nanogo defines its own format, is simpler to write and is
wrong, for one reason: it makes the two compilers unable to share a build. That
forfeits the bring-up strategy in which nanogo compiles one package while `gc`
compiles the other five hundred, which is the only way to get an early milestone
that runs real code and localises blame to one package.

The costs are real and are accepted:

1. **The format is not stable across Go releases.** nanogo targets one release at
   a time, pinned in the repository and asserted at startup. A mismatch is a
   clear error, never a misread.
2. **It is undocumented outside its implementation, and it is not small.** The
   modern format is the unified IR carried by `internal/pkgbits`. The sizes:

   | Component | Lines | Role |
   | --- | --- | --- |
   | `internal/pkgbits` | 1,568 | the container: sections, indices, cross-references |
   | `cmd/compile/internal/noder` writer, reader, linker | 8,097 | encoding and decoding declarations and bodies |
   | `go/internal/gcimporter` | 1,259 | the types-only reader, against `go/types` |
   | `cmd/compile/internal/importer` | 772 | **the same reader, against `types2`** |

   The last row is the one this spec had wrong. It named only
   `go/internal/gcimporter` and sized the port by it, which measured the wrong
   thing twice: that reader produces `go/types` packages and would have to be
   translated, and it is 1,259 lines. Its `types2` twin needs no translation,
   because nanogo's checker is a fork of `types2`, and it is 772. The reader
   here measures 1,948 lines including the container's read half and the
   archive, against an estimate of 7,000 for the whole component.

   What is left of the estimate is the writer and the body reader, and both are
   real. nanogo still needs the container's write half, a declaration writer,
   and `noder`'s reader for the bodies below.
3. **Both directions are required, and which one comes first depends on where
   the allowlist starts.** A leaf package has no imports, so compiling it with
   nanogo exercises no reader, but anything that imports it reads what nanogo
   *wrote*. A `main` package is the opposite: it imports and nothing imports it,
   so it exercises the reader and no writer. nanogo started at `main`
   ([051](051-build-integration.md)), so the reader came first, and the writer
   is what the allowlist needs before it can hold a package anything imports.

## Structure

The format is a string table plus an index plus a body, with declarations
referenced by offset so that a reader can decode one declaration without
decoding the package. That laziness is not a nicety: a large package's export
data is read by every importer, and eager decoding is a measurable share of
build time.

Types are interned and referenced by index. A cyclic type graph (a struct with
a pointer to itself) is expressed by the index reference, which is why the
format is a graph encoding and not a tree encoding.

## The importcfg file

The compiler is told where each import's export data lives by `-importcfg`
([050](050-driver.md)). The file is lines of directives, and which directives
belong to which tool is easy to get wrong:

| Directive | Consumer |
| --- | --- |
| `packagefile <path>=<file>` | the compiler |
| `importmap <old>=<new>` | the compiler |
| `packageshlib <path>=<file>` | the **linker** only |
| `modinfo <data>` | the **linker** only |

nanogo parses all four and keeps them in **separate** tables. Folding
`packageshlib` into the `packagefile` table, which is the obvious shortcut since
the compiler never reads it, makes a file carrying both for one path resolve to
whichever came last. The linker keeps them apart and so does nanogo, so that the
day nanogo has a linker ([045](045-linker.md)) the parser does not have to
change underneath it.

An unknown directive is an error. A new one in a new Go release must be a build
failure, not a line nanogo skips.

## Determinism

Export data is part of a compiled package's bytes, so [053](053-determinism.md)
applies without exception. Declarations are written in source order, not in map
order. Types are interned in first-use order. A type's methods are written
sorted by name.

This is the most likely single place for the G1 fixed point to break, because
the type checker's internal maps are the natural thing to range over and the
result changes per run. The rule is that **no map is ranged over on the write
path**, enforced by review and by the drift test below.

## Testing

What the reader has:

- Agreement on a fixture: one package carrying every type tag, every constant
  encoding, a generic type with a method, a method with type parameters of its
  own, an alias with type parameters and a self-referential type, compiled by
  `gc` and compared declaration by declaration (`export/export_test.go`).
- The standard library: every package of it, 375 of them and 13,518
  declarations, read from the archives the installed toolchain wrote, with
  every declaration forced. An unattended run reads 21 of them; the sweep runs
  under `NANOGO_REQUIRE_CORPUS=1`. Nothing fails.
- Sharing: a package reached through two different archives is one package, so
  the checker does not see two `io.Writer` types.
- Refusal: thirteen malformed archives, each producing a message that names
  what is wrong with it, and each naming the package and the file through
  `driver.ImportError`.
- End to end: `go build -toolexec=nanogo` over a module where `gc` compiles the
  library and nanogo compiles the package that imports it, and over a package
  that imports `math/bits` and `strconv`. Both programs run
  (`internal/e2e/import_test.go`).

What the writer will need, unchanged from before:

- Round trip: write nanogo's export data for every standard library package,
  read it back, and compare the reconstructed package surface against the
  original type checker output.
- Cross-read: `gc` reads nanogo's export data, nanogo reads `gc`'s, for the same
  package, and the two reconstructions agree.
- Byte determinism: compile the same package twice in one process and in two,
  and compare bytes.
- Generic bodies: an importer instantiates a generic declared in a package it
  only has export data for, and the instantiation compiles and runs.
