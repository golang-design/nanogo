---
title: "G2: toolchain independence"
status: draft
layer: gate
gate: G2
depends_on:
  - 060-selfhost.md
  - 045-linker.md
  - 014-package-loader.md
---

# Toolchain independence

The G2 gate of [001](001-bootstrap-gates.md): nanogo builds nanogo on a machine
with no `go` binary.

**Nothing here is built, and G2 is not the next thing to build.** It follows
G1, and [060](060-selfhost.md) records that G1's stage 1 does not start.

The table below has five rows. One is retired: `go build`'s orchestration is
gone from a `nanogo build`, which resolves the package graph and drives one
compile per package ([051](051-build-integration.md)), and that is the
whole-world driver this gate needs. Three are not retired.
[045](045-linker.md) is unbuilt and `nanogo build` still calls `go tool link`.
[014](014-package-loader.md) has its G1 half and still shells out to `go list`.
And the standard library a build reads still comes from a `gc` toolchain
([054](054-distribution.md)). The fifth row, `go tool asm`, is not retired at
this gate on purpose, and the section after the table says why.

nanogo's own source contains no assembly, which this gate and
[060](060-selfhost.md)'s refusal table both rest on. Nothing checks it
mechanically. `internal/hygiene/hygiene_test.go`'s
`TestNoAccidentalPlatformConstraint` walks the tree for a *Go* file name that
carries an accidental platform constraint, which is the adjacent property: an
`.s` file is invisible to that walk.

## The test

A container with:

- nanogo's source;
- one previously built nanogo binary;
- the Go standard library **source**, since nanogo compiles what it needs;

and no `go`, no `gofmt`, no `GOROOT` toolchain binaries.

The build runs in whole-world mode ([051](051-build-integration.md)), which is
how [060](060-selfhost.md)'s three stages run where there is no `go` command,
and produces an executable that passes that spec's fixed point inside the
container.

## What has to be gone

| Dependency | Replaced by |
| --- | --- |
| `go tool link` | [045](045-linker.md) |
| `go list`, `go/packages` | [014](014-package-loader.md)'s direct resolver |
| `go build`'s orchestration | nanogo's own topological driver, [051](051-build-integration.md); this row is retired |
| `go env GOROOT GOVERSION` | [054](054-distribution.md)'s tree, which names its own root and release in `VERSION` |
| `go tool asm` | **not** replaced; see below |

`nanogo build` needs three of these today: `go env` for the root and the
release, `go list` for resolution, and `go tool link` for the executable. It
refuses rather than degrading, and the refusal names the last two:

```
nanogo: nanogo build needs the go command to resolve the packages you name and to link them: exec: "go": executable file not found in $PATH
```

The message is the same from a repository build and from an unpacked
distribution's own `bin/nanogo`, so a distribution on a machine with no Go
installed builds nothing. What does work there is `nanogo version`, and
`nanogo-dist verify`, which reads a tree against its own `MANIFEST`. A
distribution is therefore self-describing and self-auditing today, and not
self-building. Making it self-building is this gate.

## The assembly question

G2 does not require [044](044-plan9-assembler.md). This looks wrong and is not.

nanogo's own source contains no assembly. The standard library packages nanogo
depends on do, `internal/bytealg`, `math`, `sync/atomic` and the runtime, so a
G2 build must handle them somehow.

The size of that set has a floor that is measurable now. Of the 27 packages a
`func main() {}` needs, nanogo refuses 8 for assembly: `internal/abi`,
`internal/cpu`, `internal/bytealg`, `internal/chacha8rand`,
`internal/runtime/atomic`, `internal/runtime/sys`, `internal/runtime/maps` and
`runtime`. nanogo's own closure is larger and holds `math` and `sync/atomic` on
top of those, so 8 is the floor and not the count.

There are two honest answers:

1. **Build the assembly-containing packages ahead of time**, outside the
   container, and carry the objects in. The container then links them. This makes
   G2 a statement about the compiler, the loader, and the linker, which is what
   it is meant to be.
2. **Implement [044](044-plan9-assembler.md) first**, making G2 imply G3's
   assembler.

nanogo takes the first. G2 and G3 are siblings in
[001](001-bootstrap-gates.md) precisely so that neither waits for the other, and
folding the assembler into G2 would merge them.

The carried objects are recorded, and the count is the honest measure of how
independent the build actually is. It should shrink to zero at G3.

## What G2 is actually testing

Not "nanogo is a real toolchain" in a marketing sense. Three concrete properties
that nothing before it tests:

1. **The linker produces a working binary from nanogo's own objects.** Until G2,
   `go tool link` has been quietly correcting or tolerating whatever nanogo
   emitted. A relocation type that was wrong but tolerated, a missing auxiliary
   symbol, an inconsistent ABI record: all of these can survive to G1.
2. **The package loader agrees with the `go` command.** Until G2 it has been
   reading `go list`'s answer. Now it computes one, and a disagreement is a
   different program being built.
3. **The dependency list is complete.** Every implicit use of the toolchain
   surfaces as a failure in the container, which is the only reliable way to find
   them.

## Testing

- The container build, in CI, as a gate.
- The linker compared against `go tool link` on identical objects, outside the
  container, so that a difference is attributable.
- The loader compared against `go list` over the distribution, per
  [014](014-package-loader.md). This runs from M1 and is not new at G2; what is
  new is that its answer is the one used.

## What was wrong

**This spec said no dependency had moved, one paragraph before describing the
one that had.** It called all four rows of the table un-retired and then
recorded that `nanogo build` already orchestrates a whole-world build.
`go build`'s orchestration is retired; the table above now says which row that
is and which three are left.

**The no-assembly claim named a check that does not check it.** It credited
`internal/hygiene` with proving that nanogo's source has no assembly.
`TestNoAccidentalPlatformConstraint` reads Go file names and would not see an
`.s` file at all. The claim is true and nothing in CI would catch it changing.
