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

The standard library entries are the load-bearing ones. nanogo's dependency set
reaches a large part of `fmt`, `go/types`' fork, and the `internal` packages
underneath them, so "the subset nanogo uses" is not small — it is most of the
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
