---
title: "Export data: gc-compatible package summaries"
status: draft
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

The alternative — nanogo defines its own format — is simpler to write and is
wrong, for one reason: it makes the two compilers unable to share a build. That
forfeits the bring-up strategy in which nanogo compiles one package while `gc`
compiles the other five hundred, which is the only way to get an early milestone
that runs real code and localises blame to one package.

The costs are real and are accepted:

1. **The format is not stable across Go releases.** nanogo targets one release at
   a time, pinned in the repository and asserted at startup. A mismatch is a
   clear error, never a misread.
2. **It is undocumented outside its implementation.** The reader and writer are
   ported from `cmd/compile/internal/typecheck` and `go/internal/gcimporter`, in
   the manner of [012](012-type-checking.md).
3. **Reading is easier than writing, and both are required.** A `gc`-compiled
   package importing a nanogo-compiled one needs nanogo's output to be readable
   by `gc`. Bring-up therefore starts with leaf packages, where only the reader
   is exercised, and the writer is proved by moving up the graph.

## Structure

The format is a string table plus an index plus a body, with declarations
referenced by offset so that a reader can decode one declaration without
decoding the package. That laziness is not a nicety: a large package's export
data is read by every importer, and eager decoding is a measurable share of
build time.

Types are interned and referenced by index. A cyclic type graph — a struct with
a pointer to itself — is expressed by the index reference, which is why the
format is a graph encoding and not a tree encoding.

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

- Round trip: write nanogo's export data for every standard library package,
  read it back, and compare the reconstructed package surface against the
  original type checker output.
- Cross-read: `gc` reads nanogo's export data, nanogo reads `gc`'s, for the same
  package, and the two reconstructions agree.
- Byte determinism: compile the same package twice in one process and in two,
  and compare bytes.
- Generic bodies: an importer instantiates a generic declared in a package it
  only has export data for, and the instantiation compiles and runs.
