---
title: "G3: compiling the Go distribution"
status: draft
layer: gate
gate: G3
depends_on:
  - 044-plan9-assembler.md
  - 034-write-barriers.md
  - 035-goroutines-and-stack-growth.md
---

# Compiling the distribution

The G3 gate of [001](001-bootstrap-gates.md): nanogo compiles the Go source tree
at `~/dev/go.dev/go`, in the `CGO_ENABLED=0` configuration, and the result passes
the distribution's own tests.

## The scope, bounded honestly

| In | Out |
| --- | --- |
| Every pure-Go package in `src/` | Anything requiring `cgo` ([000](000-decisions.md) decision 8) |
| The runtime, all 112,977 lines | `net` and `os/user` in their cgo configurations |
| Hand-written assembly for the target ([044](044-plan9-assembler.md)) | Targets other than `darwin/arm64` and `linux/amd64` |
| `test/`'s 356 files, already used at M5 | The distribution's own compiler, which nanogo compiles but does not have to make bit-identical |

The last row deserves care. nanogo compiling `cmd/compile` produces a working
`gc`. It does not produce the *same* `gc` binary that `gc` produces, and it is not
required to. Byte identity is required only of nanogo compiling nanogo
([060](060-selfhost.md)).

## What the runtime demands that nothing else does

The runtime is not a large package. It is a package with different rules, and
this is the list of them:

| Requirement | Spec |
| --- | --- |
| `//go:nosplit` enforced, with the budget computed over the call chain | [035](035-goroutines-and-stack-growth.md) |
| `//go:nowritebarrier` and its recursive form enforced over the call graph | [034](034-write-barriers.md) |
| `//go:systemstack` checked | [035](035-goroutines-and-stack-growth.md) |
| `//go:linkname` resolved in both directions across packages | [016](016-directives-and-pragmas.md) |
| Escape analysis good enough that runtime functions do not allocate | [023](023-escape-analysis.md) |
| Assembly assembled and its ABIs reconciled with Go declarations | [044](044-plan9-assembler.md), [050](050-driver.md)'s `-symabis` |
| The `-+` flag's extra checks | [050](050-driver.md) |

The escape analysis row is the one that turns an optimization into a
requirement, and [022](022-optimization-passes.md) documents that inversion
rather than hiding it.

## Order of work

The allowlist of [051](051-build-integration.md) is the plan. It grows by
dependency depth, and the runtime is last because everything depends on it and
because a runtime miscompile has no diagnostic.

A useful intermediate: compile the runtime with nanogo while everything else is
`gc`-compiled. If that binary runs, the hardest package is done and the rest is
volume.

## The test

`go tool dist test`, or its equivalent driven by nanogo, on a distribution built
with nanogo. That runs the standard library's tests, the `test/` corpus, the
runtime's own tests, and the toolchain's tests.

Failures are expected to be concentrated rather than spread: a wrong stack map
shows up as garbage collector crashes across many packages, and a wrong bounds
check shows up in a few. The shape of the failure list is diagnostic in itself.

## What G3 does not include

- Performance parity. nanogo's output is slower and there is no benchmark gate.
- Every `GOOS`/`GOARCH` combination. Two configurations.
- `cgo`, plugins, shared libraries, and race detection, per
  [045](045-linker.md)'s exclusions.

A distribution built by nanogo is a usable Go toolchain for pure-Go programs on
two platforms. That is the claim, and it is the whole claim.
