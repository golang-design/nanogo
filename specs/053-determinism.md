---
title: "Determinism"
status: in progress
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

## What is built

There is no determinism package and there does not need to be one. The rules
below are constraints on other code, and they are carried where the code is: a
slice instead of a map in `driver`'s flag table and its `-importcfg` entries, an
insertion-ordered symbol list in `obj`, package lists sorted by import path in
`loader`. Each of those carries a comment naming this spec, which is what keeps
the rule from being reverted by someone who reads only the function.

Most checks are per component, because a component is what a fast test can run
twice. Two are not: a whole compile, and a whole distribution tree.

| Check | Where |
| --- | --- |
| The object writer, twice in one process | `obj/obj_test.go` |
| The object writer, in two processes with different environments and working directories | `obj/write_test.go` |
| One function's machine code, 8 times, 32 bytes identical | `ssagen/ssagen_test.go` |
| SSA construction, decomposition, liveness, allocation, frame layout and stack maps, each twice | `ssa/*_test.go` |
| `go list` results, sorted by import path | `loader/golist_test.go` |
| A whole package compile, twice | `driver/compile_test.go` |
| The distribution tarball, from two build directories | `dist/`, per [054](054-distribution.md) |

The two-process check is the one this spec singles out below, and it is built:
the children run in different working directories with different `HOME`, `LANG`,
`TZ` and `GOMAXPROCS`, and the bytes match.

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

Nothing is concurrent yet. The non-test source starts no goroutine, and
`driver.emitPackage` walks the function list in declaration order and adds each
symbol as it goes, so the rule holds by construction rather than by design. It
is written here as a rule and not as a description because the day
[002](002-architecture.md)'s concurrent compile arrives is the day the order
stops being free.

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

| Check | When | State |
| --- | --- | --- |
| Compile every package twice in one process; compare object bytes | every CI run | built one level down, on the object writer and on each SSA pass, not on a package compile |
| Compile in two processes with different environments and working directories; compare | every CI run | built, `obj/write_test.go` |
| Compile with `-c 1` and with `-c 8`; compare | every CI run | vacuous today: `-c` is parsed into `Config.Concurrency`, nothing reads it, and the compile is sequential |
| The G1 fixed point, $N_2 = N_3$ | from M6 | not reachable, per [060](060-selfhost.md) |
| Run with `GOFLAGS=-gcflags=all=-d=maprandomize` equivalents where available | periodically | not run |

The two-process check with different working directories is the one that finds
path leaks, and it is the one most often omitted.

CI has a job named `determinism` and it says in its own comment that it is a
placeholder. It builds every binary the module produces, twice, from two
absolute paths, and compares the bytes. That gates the host toolchain's
determinism and the build's freedom from path leaks, not nanogo's. It is kept
because it is the one check on the list that needs no compiler, and because it
starts comparing objects without an edit on the day `cmd/nanogo` writes them in
a build rather than in a test.

## When the fixed point fails

[001](001-bootstrap-gates.md) gives the diagnostic order and it starts here: check
determinism first, by compiling the same source twice with $N_1$. Only if that
passes is the failure a miscompile.

## Corrections

**The checking table read as though it ran, and three of its five rows do not.**
The rows are unchanged, because they are what the compiler owes, and each now
carries the state it is in. Found by this audit, reading each row against the
tests and against `.github/workflows/ci.yml`.

**The concurrency section described the compiler as concurrent.** It is not, in
any package: a search of the non-test source finds no goroutine. The rule is
kept and the description is now marked as the future it is.
