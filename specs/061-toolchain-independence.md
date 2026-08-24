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
G1, and [060](060-selfhost.md) records that G1's stage 1 does not start. Of the
four dependencies in the table below, none is retired: [045](045-linker.md) is
unbuilt, [014](014-package-loader.md) has its G1 half and shells out to
`go list`, and there is no orchestrator. One claim below is confirmed and worth
keeping: nanogo's own source still contains no assembly, which
`internal/hygiene` checks from the other side by walking the tree for file names
that carry an accidental platform constraint.

## The test

A container with:

- nanogo's source;
- one previously built nanogo binary;
- the Go standard library **source**, since nanogo compiles what it needs;

and no `go`, no `gofmt`, no `GOROOT` toolchain binaries.

The build runs in whole-world mode ([051](051-build-integration.md)) and produces
an executable that passes [060](060-selfhost.md)'s fixed point inside the
container.

## What has to be gone

| Dependency | Replaced by |
| --- | --- |
| `go tool link` | [045](045-linker.md) |
| `go list`, `go/packages` | [014](014-package-loader.md)'s direct resolver |
| `go build`'s orchestration | nanogo's own topological driver, [051](051-build-integration.md) |
| `go tool asm` | **not** replaced; see below |

## The assembly question

G2 does not require [044](044-plan9-assembler.md). This looks wrong and is not.

nanogo's own source contains no assembly. The standard library packages nanogo
depends on do, `internal/bytealg`, `math`, `sync/atomic` and the runtime, so a
G2 build must handle them somehow, and there are two honest answers:

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
