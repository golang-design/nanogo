---
title: "G1: self-hosting"
status: draft
layer: gate
gate: G1
depends_on:
  - 001-bootstrap-gates.md
  - 053-determinism.md
---

# Self-hosting

The G1 gate of [001](001-bootstrap-gates.md), as a procedure that can be run.

## This procedure cannot be run yet

Stage 1 does not start. The blocker is not a percentage of the language, it is
three categorical refusals in `driver.Compile`, and nanogo's own packages trip
all three:

| Refused | Why | Owner |
| --- | --- | --- |
| a package with a package-level variable | a global needs a data symbol | [020](020-ir.md), [040](040-object-format.md) |
| a package with an `init` function | an init needs a package init task | [040](040-object-format.md) |
| a package with assembly | an assembly definition is ABI0 and needs a wrapper | [030](030-abi.md) |

A fourth refusal is gone. `export/` reads `gc`'s export data and writes it, so
a package that imports is no longer refused and a package nanogo compiled can
be imported. That removes the one blocker every nanogo package tripped, and it
removes no other: every nanogo package also has a package-level variable or an
`init`. The count of nanogo packages that nanogo compiles today is still zero,
and no arithmetic over the language subset changes that.

One export-data limit is still in the way of this gate specifically. The writer
refuses a generic declaration, and nanogo's own source uses generics, so stage
1 needs [013](013-generics.md) as well as the language rows below.

The second-order measure is the language, and it says the same thing more
slowly. The IR builder produces 39,947 functions for 536 packages of the
distribution. SSA construction accepts 17,905 of them, two in five. 17,809 of
those lower completely to arm64 machine operations and 17,758 carry a stack
map, so the back half of the pipeline is in better shape than the front of it.
The largest single refusal is the composite literal, at 4,841 functions, and
every large refusal after it is a row of [020](020-ir.md)'s lowering table.
About half those rows are performed now, and with the lowering pass run first,
which is what the driver does, three functions in five get past construction.
[020](020-ir.md)'s **State** column names the rows this gate still waits on and
its corpus counts them.

The end-to-end gate grows only as the language does, because its programs are
written in the language the compiler accepts. `internal/e2e`'s first program is
a counted loop, which was impossible while construction refused an assignment
statement.

The sections below are the procedure for the day the language is wide enough,
and they are unchanged because the procedure is not what was wrong.

## The build

```
stage0:  gc     compiles nanogo's source  ->  N1
stage1:  N1     compiles nanogo's source  ->  N2
stage2:  N2     compiles nanogo's source  ->  N3
gate:    N2 == N3, byte for byte
```

Stage 0 runs under `go build`. Stages 1 and 2 run under
`go build -toolexec=<compiler>` in hosted mode
([000](000-decisions.md) decision 11), with the allowlist of
[051](051-build-integration.md) covering every package nanogo's source needs.

The gate compares the linked executables. `go tool link` produces both, from the
same objects in the same order, so any difference is in the objects.

## What nanogo's own source requires

The subset of Go that must work is decided by what the compiler is written in,
and it is worth listing because it sizes M5 in [003](003-sequencing.md):

| Used heavily | Used | Not used |
| --- | --- | --- |
| structs, methods, interfaces | goroutines and channels, in the concurrent compile | reflection beyond `unsafe` |
| slices, maps, strings | `defer`, `panic`, `recover` | `select`, except incidentally |
| closures | `unsafe`, in the object writer | complex numbers |
| generics ([013](013-generics.md)) | `sort`, `strconv`, `fmt`, `os`, `io` from the standard library | `cgo` |
| type switches and assertions | `//go:` directives, only as input | assembly of its own |

The standard library entries are the decisive ones. nanogo's dependency set
reaches a large part of `fmt`, `go/types`' fork, and the `internal` packages
underneath them, so "the subset nanogo uses" is not small: it is most of the
language exercised by ordinary application code, and none of the language
exercised only by the runtime.

That is precisely the difference between G1 and G3.

## Diagnosing a failure

$N_2 \ne N_3$ has three causes ([001](001-bootstrap-gates.md)). Take them in
order, because the cheap check eliminates the expensive investigation:

1. **Non-determinism.** Compile the source twice with $N_1$ and compare. If those
   differ, [053](053-determinism.md) is the whole problem and nothing else is
   wrong. This is the common case and it must be ruled out first.
2. **A miscompile in $N_1$'s output.** Bisect by object: compile the packages
   with $N_1$ and with $N_2$ and compare the objects one at a time. The first
   differing object names the package, and its differing symbol names the
   function.
3. **An environmental dependency.** Compile in two different directories with
   different environments. [053](053-determinism.md)'s two-process check is this,
   run deliberately.

The bisection in step 2 is the reason [040](040-object-format.md) requires
deterministic object bytes rather than only deterministic executables. Comparing
executables tells you that something is wrong; comparing objects tells you what.

## What the gate does not prove

Stated in [001](001-bootstrap-gates.md) and repeated because it is the most
likely thing to be forgotten in a celebration: the fixed point proves
self-consistency. A compiler that miscompiles a construct it does not use, or
that miscompiles one reproducibly, passes.

[004](004-conformance.md)'s L1 through L3 are what make the gate mean something,
and they run before it, not after.

## After the gate

nanogo builds nanogo from then on, and stage 0 becomes a periodic check rather
than the build. The `gc`-built binary is still produced in CI, because a
divergence between $N_1$ and $N_2$ that appears later is a miscompile that the
fixed point alone cannot see.
