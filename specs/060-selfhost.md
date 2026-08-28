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
what `driver.Compile` refuses, and nanogo's own packages trip it:

| Refused | Why | Owner |
| --- | --- | --- |
| a package with assembly | an assembly definition is ABI0 and needs a wrapper | [030](030-abi.md) |
| a declared type an importer would need a descriptor for | `rtype` cannot fill the bytes in: a method's signature and the two ABI wrappers beside it, a generated equality function, or a distinction [020](020-ir.md)'s type boundary drops | [032](032-type-descriptors-and-itabs.md) |
| a package-level variable whose type holds a pointer and whose descriptor `rtype` cannot build | the collector reads a data symbol's pointer map through its type descriptor | [032](032-type-descriptors-and-itabs.md) |
| a function the register allocator's output asks `ssagen` for a move or a scratch register it does not have | there is no case for an edge move between two spill slots, and the target reserves two integer scratch registers where an indexed store wants three | `ssagen`, which [000](000-decisions.md) decision 5 records has no spec of its own |

The second row is the one nanogo's own packages hit first. `driver/types.go`
walks a package's scope in sorted order and refuses on the first declared type
whose descriptor `rtype` cannot write, so the message names one type per package
and says nothing about how many are behind it: `syntax` names `ArrayType`, `ir`
names `Class`, `obj` names `Aux`, `rtsym` names `Group`, and `driver` names
`Allowlist`. The count of nanogo packages that nanogo compiles is the measure,
and no arithmetic over the language subset stands in for it. It is two of
nineteen, and the section that measures it names the five packages that block
the rest.

What is left is [032](032-type-descriptors-and-itabs.md)'s encoder gap, which
rows two and three above both name, [030](030-abi.md)'s wrapper, and the fourth
row, which belongs to no spec. Measured over the 28 packages a `func main() {}`
needs, nanogo compiles 9 and refuses 19:

| Packages | Refused for |
| --- | --- |
| 8 | assembly |
| 7 | a declared type's descriptor |
| 2 | the register allocator's output, in `internal/stringslite` and `internal/runtime/gc` |
| 1 | a package-level variable of type `error`, `math/bits` |
| 1 | a row of [020](020-ir.md)'s lowering table, `append` in `internal/byteorder` |

The 9 that compile are the `main` package itself, `internal/goarch`,
`internal/goexperiment`, `internal/goos`, `internal/profilerecord`,
`internal/asan`, `internal/msan`, `internal/race` and `internal/runtime/math`.
The seven descriptor refusals split four ways, which is why the row is one
count and not one reason: three want the generated equality function
`internal/coverage/rtcov`, `internal/godebugs` and
`internal/runtime/pprof/label` each need, two want a method's signature
(`internal/strconv`, `internal/trace/tracev2`), one a function's signature
(`internal/runtime/exithook`) and one a type parameter's run-time
representation (`internal/runtime/gc/scan`).
None of these counts is in `internal/hygiene/testdata/facts.json`, so nothing
gates them and each Go release moves them. They are a reading of one toolchain,
`go1.27.0` on `darwin/arm64`, which is `driver.PinnedGoVersion` and
`driver.TargetArch`, the only pair nanogo emits code for.

One export-data limit is in the way of this gate specifically.
`export/writer.go` refuses a generic declaration on purpose, and nanogo's own
source has them. They are few and concentrated in the `types2` fork,
`types2/subst.go` and `types2/trie.go` among them, and the instantiations of
`slices` and `cmp` are many and spread across the tree. Declaration and
instantiation are both [013](013-generics.md)'s, so stage 1 needs that spec as
well as the language rows below.

The second-order measure is the language, and it says the same thing more
slowly. The IR builder produces a typed tree for 536 packages of the
distribution. SSA construction takes 20,850 of those functions on its own, and
39,450 once the lowering pass has run first, which is what the driver does. Of
the 20,850, all but 57 lower to arm64 machine operations and the same number
carry a stack map, so the back half of the pipeline refuses almost nothing the
front half hands it. Every one of those numbers is in
`internal/hygiene/testdata/facts.json`, gated, and written out in full in the
README and in [032](032-type-descriptors-and-itabs.md); this section names them
rather than restating them, so that there is one place to correct.
[020](020-ir.md)'s **State** column names the rows this gate still waits on and
its corpus counts them.

The end-to-end gate grows only as the language does, because its programs are
written in the language the compiler accepts. `internal/e2e`'s first program is
a counted loop.

The sections below are the procedure for the day the language is wide enough.

## The build

```
stage0:  gc     compiles nanogo's source  ->  N1
stage1:  N1     compiles nanogo's source  ->  N2
stage2:  N2     compiles nanogo's source  ->  N3
gate:    N2 == N3, byte for byte
```

Stage 0 runs under `go build`. Stages 1 and 2 run under
`go build -toolexec=<compiler>` in hosted mode
([000](000-decisions.md) decision 10), with the allowlist of
[051](051-build-integration.md) covering every package nanogo's source needs.

At G2 the same three stages run in a container with no `go` command
([061](061-toolchain-independence.md)), so they run under `nanogo build`, which
takes no allowlist and refuses a package it cannot compile rather than
delegating it to `gc`.

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
| generics ([013](013-generics.md)), mostly by instantiation | `sort`, `strconv`, `fmt`, `os`, `io` from the standard library | `cgo` |
| type switches and assertions | `//go:` directives, only as input | assembly of its own |

Read the first column against what the compiler accepts today and the distance
is the gate. Every entry in it compiles now: structs and methods with either
receiver, interfaces with their calls, conversions, assertions and type
switches, slices with `append`, strings with `range`, maps, channels with
`select`, closures with captures, `defer`, `go`, `panic` and `recover`, and a
generic function the compiling package declares, stencilled per list of type
arguments. What the first column still waits on is narrower than the column
itself, and the table of blockers below names it: the data word of a conversion
to an interface when the value has to be copied into the frame first, an
instantiation of a generic another package declares, and a descriptor whose
pointer map is longer than one word.

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

## What was wrong

**Three package-level refusals this spec listed are gone.** `export/` reads
`gc`'s export data and writes it, so a package that imports is no longer
refused. `driver/` writes the initialisation record, so a package with an `init`
is no longer refused. And `ssagen/data.go` writes the data symbol of a
package-level variable, so a package that declares one is no longer refused for
that reason alone.

**The package census was stale in two directions at once and the totals hid
it.** It read 5 of 28 compiling and 23 refused, split 10 for a descriptor, 8 for
assembly, 1 for data and 4 in a function body. Two re-measurements later it is 9
and 19, and no re-measurement moved the total in one direction only: the first
took the descriptor row from 10 to 11 while the function-body row fell from 4 to
2, and the second, after `rtype` learned the `UncommonType` tail and the struct
field array, took the descriptor row from 11 to 7. A sum alone would have read
the first of those as one package's worth of progress. State every row, not the
total.

**The refusal taxonomy had four categories and needs five.** Two of the
refusals are not a language gap. `internal/stringslite` is refused by `ssagen`
for an edge move between two spill slots, and `internal/runtime/gc` for an
indexed store wanting three integer scratch registers where the target reserves
two. Both are the register allocator's output and not a construct in the source,
so they share a row of their own in both tables above.

**The descriptor row's reason was `ir.Type` carrying no method set.** It
carries one now ([032](032-type-descriptors-and-itabs.md)), and the row survived
because the count did. Three of the seven packages it still holds want the
generated equality function that spec owes, and only two want anything to do
with a method. A reason stated once and never re-read outlives the fact it was
taken from.

## The closure measured against nanogo's own packages

The count above is over the packages a `func main() {}` needs. This is the
narrower and more useful measure: nanogo's own nineteen, compiled by nanogo under
an allowlist that names all of them.

**Two of nineteen compile, and five leaf packages block the rest.** The other
twelve fail on an import rather than on themselves, so the work list is the
five below and not nineteen. `rtsym` and `types2/errors` compile.

| Package | What stops it |
| --- | --- |
| `obj`, `dist`, `export/pkgbits` | `a conversion to an interface, whose data word needs runtime.convT with the address of a copy in the frame is not built yet`. One gap, three packages, and it is the largest refusal class of Go's own corpus as well |
| `syntax` | `ir: sync/atomic.Pointer is a generic instantiation and its type arguments are not in the IR type`, reached through `FileSet`. [032](032-type-descriptors-and-itabs.md) owns the descriptor and [013](013-generics.md) the instantiation |
| `obj/arm64` | `rtype: 129 pointer words needs the on-demand mask, which is not built`, for the package-level `regNames`. [032](032-type-descriptors-and-itabs.md) owns it |

`loader` and `types2` wait on `syntax`. `export` waits on `pkgbits`. `link`,
`rtype`, `ir`, `ssa`, `ssagen` and `driver` wait on what is above them.

Two of the three blockers this table used to name are closed. `rtsym` wanted a
frame object past the twelve-bit `ADD` immediate, which is the R27 expansion
[042](042-arm64-backend.md) already had for `subSP`. `obj` wanted a result that
arrives in the frame to be read there, which [030](030-abi.md) now does at the
call site as well as at the return. `obj` did not start compiling: it moved to
the `convT` row, which is what a work list does when the item above it closes.

The measurement is worth repeating rather than quoting, and it has one trap in
it. `go build -toolexec` with no `NANOGO_ALLOWLIST` compiles **nothing** with
nanogo: an unset variable is an empty list and every package goes to `gc`, so
the build succeeds and proves only that `gc` works. The allowlist must name the
packages, and the first attempt at this measurement reported twelve of twelve
for exactly that reason.
