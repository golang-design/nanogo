---
title: "Sequencing: the order of work, what done means, and where the risk is"
status: draft
layer: foundation
depends_on:
  - 000-decisions.md
  - 001-bootstrap-gates.md
  - 002-architecture.md
---

# Sequencing

The order of work. This is the file to read before picking anything up, and it
is not the order the specs are numbered in. It will change as milestones land;
the deviations are recorded at the bottom rather than edited away.

## Milestones

```mermaid
flowchart LR
  M0["M0<br/>skeleton"] --> M1["M1<br/>parser"] --> M2["M2<br/>types"] --> M3["M3<br/>first binary"]
  M3 --> M4["M4<br/>runtime interface"] --> M5["M5<br/>full language"] --> M6["M6<br/>G1 fixed point"]
  M6 --> M7["M7<br/>amd64"]
  M6 --> M8["M8<br/>G2 linker"]
  M7 --> M9["M9<br/>G3 distribution"]
  M8 --> M9
  M9 --> M10["M10<br/>GPU target"]
```

### M0 — skeleton

Driver, flag parsing, package layout, CI. The spikes that
[000](000-decisions.md) decision 3 cites are run and kept.

**Done when** `nanogo -V` prints a version, the package tree of
[002](002-architecture.md) exists with doc comments, and both spikes run from a
clean checkout.

### M1 — parser

[010](010-scanner-and-positions.md), [011](011-parser-and-ast.md). The whole Go
grammar including generics. No type checking.

**Done when** nanogo parses every `.go` file in `~/dev/go.dev/go/src` that
`go/parser` parses, and rejects every file `go/parser` rejects. Disagreements are
enumerated, not tolerated.

This gate is worth its cost. It is a corpus of roughly 14,000 files that exercise
every corner of the grammar, and it is free.

### M2 — types

[012](012-type-checking.md), [013](013-generics.md), [015](015-export-data.md),
and the parts of [014](014-package-loader.md) that G1 needs. `types2` is
vendored and re-pointed at nanogo's syntax tree.

**Done when** nanogo type-checks every package in the distribution that
`go/types` checks, produces the same errors for `test/`'s `errorcheck` files,
and round-trips export data for every standard library package.

### M3 — first binary

[020](020-ir.md), [021](021-ssa-construction.md),
[025](025-lowering-and-rules.md), [026](026-register-allocation.md),
[040](040-object-format.md), [041](041-instruction-encoding.md),
[042](042-arm64-backend.md). Enough of each to compile a program that uses
integers, control flow, and function calls.

**Done when** a program whose only output is `println` compiles, links with
`go tool link`, runs, and prints the right thing.

This is the milestone where the object format decision is proved or disproved in
practice, so it is deliberately early and deliberately narrow.

### M4 — runtime interface

[027](027-liveness-and-stackmaps.md), [030](030-abi.md),
[031](031-runtime-lowering.md), [032](032-type-descriptors-and-itabs.md),
[033](033-closures-defer-panic.md), [034](034-write-barriers.md),
[035](035-goroutines-and-stack-growth.md).

This is the largest milestone and the one that decides whether the project is
real. Everything in it is a contract with code nanogo did not write and cannot
change.

**Done when** `fmt.Println` works, a program that allocates under a
`GOGC=1` stress loop survives, `defer`/`panic`/`recover` behave, goroutines run
and the stack grows, and the GC scans nanogo-generated frames precisely.

### M5 — full language

Generics instantiation, `select`, complex numbers, the remaining conversions,
`//go:` pragmas, and everything else the specification requires but nanogo's own
source does not use.

**Done when** the `test/` corpus passes: `run`, `runoutput`, `errorcheck`,
`compile`, and `build` directives all honoured.

### M6 — G1

Self-host. [060](060-selfhost.md).

**Done when** $N_2 = N_3$ byte for byte, per [001](001-bootstrap-gates.md).

### M7 — amd64

[043](043-amd64-backend.md). The test of [000](000-decisions.md) decision 5.

**Done when** the same middle end serves both targets with no target name
appearing above [025](025-lowering-and-rules.md), and G1 holds on
`linux/amd64`.

### M8 — G2

[045](045-linker.md), the rest of [014](014-package-loader.md).

**Done when** nanogo builds nanogo in a container with no `go` binary present.

### M9 — G3

[044](044-plan9-assembler.md), [046](046-debug-info.md),
[062](062-distribution-build.md).

**Done when** nanogo compiles the `CGO_ENABLED=0` distribution and the
distribution's tests pass on the result.

### M10 — GPU target

[070](070-gpu-target.md). Post-v1 and unscheduled.

## Where the risk is

Risks are listed with the milestone that retires them, not with a probability.

| Risk | Retired at | How it is retired |
| --- | --- | --- |
| The object format seam does not work | M3 | The first binary either runs or it does not. This is why M3 is narrow and early. |
| GC metadata is wrong in a way that only shows under load | M4 | Allocation stress with `GOGC=1` and `GODEBUG=gccheckmark=1`, not a unit test. |
| The forked type checker cannot be re-pointed at nanogo's syntax tree cheaply | M2 | [012](012-type-checking.md) names the interface it is re-pointed through. If the fork resists, the fallback is to keep `go/ast` under the checker and translate, at a cost stated in that spec. |
| Generics instantiation is a rewrite rather than a port | M5 | Deferred deliberately: nanogo's own source uses generics, so M6 cannot be reached without it, and M5 is where it is confronted with the corpus available. |
| The 40,000-line budget is spent by M4 | M4 | Counted at every milestone, not at the end. |
| `//go:linkname` and `//go:nosplit` semantics are subtler than documented | M9 | The runtime is the only consumer that cares, and M9 is where it is compiled. |

## Deviations

None yet. Each entry here records a departure from the plan above with the
reason, so that the plan stays honest instead of staying accurate.
