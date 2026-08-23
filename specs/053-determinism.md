---
title: "Determinism"
status: draft
layer: driver
gate: G1
depends_on:
  - 000-decisions.md
  - 001-bootstrap-gates.md
---

# Determinism

Implements [000](000-decisions.md) decision 7. The same inputs produce
byte-identical outputs.

This is not hygiene and it is not reproducible-build advocacy. G1's gate is
$N_2 = N_3$ ([001](001-bootstrap-gates.md)), an exact comparison of bytes.
Non-determinism does not weaken that gate. It removes it, and it removes it in a
way that looks like a miscompile: the fixed point fails, and the first suspicion
falls on code generation.

## The sources, and what is done about each

### Map iteration

Go randomises map iteration order deliberately. Any output derived from ranging
over a map differs between runs.

**Rule: no map is ranged over on any path that produces output.** Where a map is
the right data structure, its keys are collected and sorted before use, or an
insertion-ordered structure is used instead.

The dangerous instances, all of them already identified in other specs:

| Map | Spec |
| --- | --- |
| The type checker's object and type maps, on the export path | [015](015-export-data.md) |
| The set of type descriptors to emit | [032](032-type-descriptors-and-itabs.md) |
| The instantiation set | [013](013-generics.md) |
| The symbol table on the object writing path | [040](040-object-format.md) |

[015](015-export-data.md) names this as the most likely single place for the
fixed point to break, and that judgement stands.

### Concurrency

Functions are compiled concurrently ([002](002-architecture.md)). Results merged
in completion order are ordered by scheduling.

**Rule: results are merged in declaration order.** Each function's output is
written to a slot indexed by its position in the package, and the slots are
concatenated after the wait. Never a channel drained into a list.

### Pointer values

Sorting by pointer, hashing a pointer, or using a pointer as a map key that
affects output all vary with allocation addresses.

**Rule: no ordering derives from an address.** Objects that need an order carry
an explicit sequence number assigned at creation.

### The environment

Absolute paths, the working directory, the hostname, the time, the user, and
environment variables all leak into output if allowed.

**Rule: the only paths in output are those `-trimpath` produced or the command
line supplied.** No timestamps. `-buildid` is the one input that is intentionally
build-specific and it is supplied by the caller, not generated.

### Floating point

Constant folding of floating-point expressions must produce the same bits on
every host. Go's untyped constants are arbitrary precision and are evaluated with
`math/big`, which is exact and host-independent. Folding of *typed* floating
point in [022](022-optimization-passes.md) uses the target's semantics, computed
in software rather than on the host's floating-point unit, because the host may
have different rounding or an extended intermediate precision.

## Checking it

Determinism fails silently and rarely, so it is checked mechanically and
constantly.

| Check | When |
| --- | --- |
| Compile every package twice in one process; compare object bytes | every CI run |
| Compile in two processes with different environments and working directories; compare | every CI run |
| Compile with `-c 1` and with `-c 8`; compare | every CI run |
| The G1 fixed point, $N_2 = N_3$ | from M6 |
| Run with `GOFLAGS=-gcflags=all=-d=maprandomize` equivalents where available | periodically |

The two-process check with different working directories is the one that finds
path leaks, and it is the one most often omitted.

## When the fixed point fails

[001](001-bootstrap-gates.md) gives the diagnostic order and it starts here: check
determinism first, by compiling the same source twice with $N_1$. Only if that
passes is the failure a miscompile.
