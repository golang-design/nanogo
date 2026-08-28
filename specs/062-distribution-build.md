---
title: "G3: compiling the Go distribution"
status: draft
layer: gate
gate: G3
depends_on:
  - 044-plan9-assembler.md
  - 043-amd64-backend.md
  - 034-write-barriers.md
  - 035-goroutines-and-stack-growth.md
  - 033-closures-defer-panic.md
  - 023-escape-analysis.md
---

# Compiling the distribution

The G3 gate of [001](001-bootstrap-gates.md): nanogo compiles a Go source
checkout, the `src` tree of a Go distribution, in the `CGO_ENABLED=0`
configuration, and the result passes the distribution's own tests. The checkout
is named on the command line; no path to one is written into this repository.

**No part of this gate is built.** The requirements in the table below belong to specs
that are unbuilt in turn: [044](044-plan9-assembler.md),
[034](034-write-barriers.md), [023](023-escape-analysis.md), and the capture
half of [033](033-closures-defer-panic.md). The `linux/amd64` half of the scope
needs [043](043-amd64-backend.md), which is unbuilt too: `driver.TargetArch` is
`arm64`, and a build for `amd64` is refused by name.

What nanogo does compile of the distribution today is five packages, all of
them small `internal` leaves ([060](060-selfhost.md) has the census). It
compiles no package that holds assembly, the runtime among them, and every
`cmd` package tried so far is refused for a type descriptor
([032](032-type-descriptors-and-itabs.md)). No part of the distribution's own
test suite has ever run against nanogo-compiled code.

`dist/` is not this gate. It builds and audits a *nanogo* distribution, the
`bin/`, `src/`, `pkg/` and `VERSION` tree that `nanogo build` takes its standard
library from ([054](054-distribution.md) owns it, and
[051](051-build-integration.md) owns the command that reads it). Whole-world
mode, where nanogo compiles every package including the runtime, is a third mode
that [051](051-build-integration.md) records as unbuilt. G3 is nanogo compiling
Go's distribution, and that is untouched.

The distribution is nevertheless the corpus the front end is already measured
against, so the distance to this gate is measured rather than guessed. The IR
builder walks 536 packages of 663, producing 41,354 functions and 4,245,532
nodes, with 126 packages skipped (31 for `cgo`, 94 with no Go files, 1 with a
type error) and 72 built partially. [004](004-conformance.md) has the rest of
the corpus counts. What none of that says is that a package compiles: three
functions in five get past SSA construction with [020](020-ir.md)'s lowering
pass run first, [003](003-sequencing.md) counts them, and
[060](060-selfhost.md) has what still refuses the rest.

## The scope, bounded honestly

| In | Out |
| --- | --- |
| Every pure-Go package in `src/` | Anything requiring `cgo` ([000](000-decisions.md) decision 8) |
| The runtime, all 112,977 lines | `net` and `os/user` in their cgo configurations |
| Hand-written assembly for the target ([044](044-plan9-assembler.md)) | Targets other than `darwin/arm64` and `linux/amd64` |
| `test/`'s 356 files, driven today by `internal/gotest` ([004](004-conformance.md)) | The distribution's own compiler, which G3 requires nanogo to compile but not to make bit-identical |

The last row deserves care. At G3, nanogo compiling `cmd/compile` has to
produce a working `gc`. It will not produce the *same* `gc` binary that `gc`
produces, and it is not required to. Byte identity is required only of nanogo
compiling nanogo ([060](060-selfhost.md)).

The corpus row is a statement about today and not about the gate.
`internal/gotest` already drives all 356 files with `gc` as the oracle, and
`internal/gotest/testdata/ratchet.txt` records what each one proved. Only its
`matched` class proves code generation, which is the class the file is read
for; a `rejected` file exercises the type checker and a `compiled` one was
never run. [003](003-sequencing.md)'s M5 is finished when the refusals in that
file are gone, and M5 is not started. G3 needs M5 and the runtime's rules and
the assembler besides, so the corpus measures the language and not this gate.

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

## What was wrong

| Claimed | What the code does |
| --- | --- |
| "No package of the distribution is compiled by nanogo" | Five are, all `internal` leaves. The gate is unreached; the floor is not zero. |
| The gate compiles "the Go source tree at `~/dev/go.dev/go`" | One machine's path. The checkout is an argument, and nothing in this repository names one. |
| `linux/amd64` in scope, with no requirement naming the amd64 back end | [043](043-amd64-backend.md) is unbuilt and a build for `amd64` is refused by name, so half the scope had no requirement row. |
| `dist/`'s tree is what [051](051-build-integration.md) "records under whole-world mode" | [051](051-build-integration.md) records `nanogo build` as its own path and whole-world mode as a third mode that is unbuilt. |
| "`test/`'s 356 files, already used at M5" | The corpus runs today under `internal/gotest`. M5 is not started. |
