---
title: "Sequencing: the order of work, what done means, and where the risk is"
status: in progress
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

### M0: skeleton

Driver, flag parsing, package layout, CI. The spikes that
[000](000-decisions.md) decision 3 cites are run and kept.

**Done when** `nanogo -V=full` answers the `go` command's build-ID protocol
([050](050-driver.md)), a passthrough build with `-toolexec=nanogo` completes by
execing `gc` for everything, the package tree of [002](002-architecture.md)
exists with doc comments, and all three spikes run from a clean checkout.

The passthrough build is the first real gate. It proves the driver, the flag
parsing, and the `-V=full` protocol before any compilation exists, and
[`spikes/toolexec`](../spikes/toolexec) is the reference implementation of it.

### M1: parser

[010](010-scanner-and-positions.md), [011](011-parser-and-ast.md). The whole Go
grammar including generics. No type checking.

**Done when** nanogo parses every `.go` file in `~/dev/go.dev/go/src` that
`go/parser` parses, and rejects every file `go/parser` rejects. Disagreements are
enumerated, not tolerated.

This gate is worth its cost. It is a corpus of roughly 14,000 files that exercise
every corner of the grammar, and it is free.

### M2: types

[012](012-type-checking.md), [013](013-generics.md), [015](015-export-data.md),
and the parts of [014](014-package-loader.md) that G1 needs. `types2` is
vendored and re-pointed at nanogo's syntax tree.

**Done when** nanogo type-checks every package in the distribution that
`go/types` checks, produces the same errors for `test/`'s `errorcheck` files,
and round-trips export data for every standard library package.

### M3: first binary

[020](020-ir.md), [021](021-ssa-construction.md),
[025](025-lowering-and-rules.md), [026](026-register-allocation.md),
[040](040-object-format.md), [041](041-instruction-encoding.md),
[042](042-arm64-backend.md). Enough of each to compile a program that uses
integers, control flow, and function calls.

**Done when** a single leaf package, compiled by nanogo under
`go build -toolexec=nanogo` while `gc` compiles everything else, links and runs
with its tests passing.

Hosted mode ([000](000-decisions.md) decision 11) is what makes this gate
narrow. The runtime, the standard library, and the linker are all `gc`'s. The
only new thing in the binary is one package's worth of nanogo code generation,
so a crash has one suspect.

This is the milestone where the object format decision is proved or disproved in
practice, so it is deliberately early.

### M4: runtime interface

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

The measure of progress inside M4 is the number of standard library packages
that compile with nanogo in hosted mode and still pass their own tests. It
starts at one and is tracked.

### M5: full language

Generics instantiation, `select`, complex numbers, the remaining conversions,
`//go:` pragmas, and everything else the specification requires but nanogo's own
source does not use.

**Done when** the `test/` corpus passes: `run`, `runoutput`, `errorcheck`,
`compile`, and `build` directives all honoured.

### M6: G1

Self-host. [060](060-selfhost.md).

**Done when** $N_2 = N_3$ byte for byte, per [001](001-bootstrap-gates.md).

### M7: amd64

[043](043-amd64-backend.md). The test of [000](000-decisions.md) decision 5.

**Done when** the same middle end serves both targets with no target name
appearing above [025](025-lowering-and-rules.md), and G1 holds on
`linux/amd64`.

### M8: G2

[045](045-linker.md), the rest of [014](014-package-loader.md).

**Done when** nanogo builds nanogo in a container with no `go` binary present.

### M9: G3

[044](044-plan9-assembler.md), [046](046-debug-info.md),
[062](062-distribution-build.md).

**Done when** nanogo compiles the `CGO_ENABLED=0` distribution and the
distribution's tests pass on the result.

### M10: GPU target

[070](070-gpu-target.md). Post-v1 and unscheduled.

## Where the risk is

Risks are listed with the milestone that retires them, not with a probability.

| Risk | Retired at | How it is retired |
| --- | --- | --- |
| The object format seam does not work | M3 | The first binary either runs or it does not. This is why M3 is narrow and early. |
| `gc` object compatibility is unmaintainable across a release | M3 | Detected the first time the pin is bumped. The fallback is whole-world mode, stated in [000](000-decisions.md) decision 11. |
| GC metadata is wrong in a way that only shows under load | M4 | Allocation stress with `GOGC=1` and `GODEBUG=gccheckmark=1`, not a unit test. |
| The forked type checker cannot be re-pointed at nanogo's syntax tree cheaply | M2 | [012](012-type-checking.md) names the interface it is re-pointed through. If the fork resists, the fallback is to keep `go/ast` under the checker and translate, at a cost stated in that spec. |
| Generics instantiation is a rewrite rather than a port | M5 | Deferred deliberately: nanogo's own source uses generics, so M6 cannot be reached without it, and M5 is where it is confronted with the corpus available. |
| The v1 line budget is exceeded | M4 | The tree measures 36,237 lines against a budget of 40,000, with the export data writer, escape analysis, inlining and generics instantiation still unwritten and estimated at 14,700. [000](000-decisions.md) decision 10 carries the accounting and the two recovery points, [022](022-optimization-passes.md) and [015](015-export-data.md). `internal/hygiene` gates the figure. |
| `//go:linkname` and `//go:nosplit` semantics are subtler than documented | M9 | The runtime is the only consumer that cares, and M9 is where it is compiled. |

## Where the work stands

**M0 and M1 are complete. M2 is complete except for the export data writer. M3
and M4 are partly built and neither gate is met.**

| Milestone | State | Gate |
| --- | --- | --- |
| M0 skeleton | done | a `go build -a -toolexec=nanogo` completes by delegating |
| M1 parser | done | 19,674 files agree with `go/scanner`, 16,293 with `go/parser` |
| M2 types | done, less the export data writer | 613 subtests, a 375-entry errorcheck corpus, checks nanogo's own source, reads gc's export data |
| M3 first binary | **in progress** | the pipeline is built end to end and a compiled function links against the real runtime and runs; a leaf package under `-toolexec` does not compile |
| M4 runtime interface | **in progress** | liveness, stack maps, the ABI and the stack growth check are built; type descriptors, itabs, closures, `defer`, `panic`, write barriers and goroutines are not |
| M5 to M10 | not started | |

### What M3 reached, and what it did not

The pipeline exists from source text to a `goobj` file. `ssagen`'s
`TestLinkAndRun` takes 18 programs through all of it to a process that returns
the right answer, and several of them call into or are called from
`gc`-compiled code. That retired the milestone's stated risk: **`go tool link`
links a nanogo-written object against the real Go runtime into a binary that
runs.** [040](040-object-format.md) records the result, and the object format
decision of [000](000-decisions.md) decision 3 is no longer a judgement call.

M3's own gate is a leaf package compiled under `-toolexec`, and that is not
met, because the language nanogo accepts is narrower than any real package.

The corpus says how much narrower, and it is one measurement with two halves.
The IR builder produces a typed tree for 536 packages of the Go distribution,
39,947 functions and 4,188,075 nodes.
**17,367 of those functions reach SSA construction**, and 17,285 of them
lower completely to arm64 machine operations. 17,239 of the 17,367 carry a
stack map, over 117,228 safepoints, and 10,242 of them have a pointer bit set.

The remaining 22,580 functions never reach SSA. Construction refuses them by
name. A composite literal accounts for 4,458 of them, then `len` for 2,680, a
multi-value assignment with a result wider than a register for 2,191, a
conversion to an interface for 2,009 and `range` for 1,527. The README carries
the full table. [021](021-ssa-construction.md) owns the pass and the gap.

### Why part of M4 was built before M3 was finished

M4 was reached out of order, the same way M3 was. [027](027-liveness-and-stackmaps.md),
[030](030-abi.md), part of [031](031-runtime-lowering.md) and the prologue half
of [035](035-goroutines-and-stack-growth.md) were built because M3's binary
needed them to run at all: a frame that the collector cannot scan and a call
that disagrees about where its arguments live do not survive `TestLinkAndRun`.
The rest of M4, [032](032-type-descriptors-and-itabs.md),
[033](033-closures-defer-panic.md), [034](034-write-barriers.md) and the
`newproc` half of [035](035-goroutines-and-stack-growth.md), is untouched.

M4's own gate is unchanged and unmet. `fmt.Println` does not compile, because
`fmt` is a package and no package compiles.

Measured coverage. The figures are rounded down, and the gate is 90% per
package:

| Package | Coverage | |
| --- | --- | --- |
| `syntax` | 99% | [010](010-scanner-and-positions.md), [011](011-parser-and-ast.md) |
| `types2` | excluded | [012](012-type-checking.md); gated by upstream's suite |
| `loader` | 98% | [014](014-package-loader.md), G1 half |
| `ir` | 95% | [020](020-ir.md); builds 536 packages of the distribution |
| `ssa` | 96% | [021](021-ssa-construction.md), with the verifier of that spec |
| `ssa/rules` | 97% | [025](025-lowering-and-rules.md), [042](042-arm64-backend.md) |
| `ssagen` | 92% | [027](027-liveness-and-stackmaps.md), [041](041-instruction-encoding.md) |
| `obj` | 98% | [040](040-object-format.md) |
| `obj/arm64` | 99% | [041](041-instruction-encoding.md), [042](042-arm64-backend.md) |
| `rtsym` | 100% | [031](031-runtime-lowering.md), [032](032-type-descriptors-and-itabs.md) |
| `export` | 96% | [015](015-export-data.md) |
| `export/pkgbits` | 92% | [015](015-export-data.md) |
| `driver` | 97% | [050](050-driver.md), [051](051-build-integration.md) |
| `internal/covercheck` | 97% | the gate itself |
| `cmd/nanogo` | excluded | one statement; the reason is in the exclusions file |

## Deviations

Each entry records a departure from the plan above with the reason, so that the
plan stays honest instead of staying accurate.

**M2 was recorded as done and one third of its gate was never built. Half of
that third is built now.** M2's scope names [015](015-export-data.md) and its
gate says "round-trips export data for every standard library package". The
reader half is built: `export/` reads gc's export data, and a package that
imports compiles and runs under `-toolexec`. The writer is not, so nothing
round-trips and the gate is still unmet. The two other parts of the gate, type
checking the distribution and matching `errorcheck`, are met and gated.

The consequence has moved rather than gone. nanogo can compile a package that
imports, so M3's gate is no longer blocked by this spec; what it is blocked by
is the language nanogo accepts. What the missing writer blocks is the other
direction: a package nanogo compiled cannot be imported, so the allowlist can
only hold packages nothing else in the build imports.

**M3 was recorded as done and its gate was never met.** An earlier version of
this section marked M3 done in the table and said in the same paragraph that
its gate was not met. Both sentences were written honestly and they contradict
each other, which is how a status table rots: the table records enthusiasm and
the prose records the truth. M3 is `in progress` until a leaf package compiles
under `-toolexec`. The two numbers below say what changed.

**The lowering measurement was stale by a factor of two, in the good
direction.** This section said 8,238 functions reached SSA and 4,755 lowered
completely. Decomposition was finished after that was written, and the corpus
reported 8,238 of 8,238. The claim that followed it, that the largest cause of
the remainder is values wider than a machine register, went with it.

**Then construction learned the assignment statement and both numbers moved
again.** `ssa/build.go` had no case for `ir.OAssign`, none for `ir.OCase`, and
read a `for` statement's post list out of `Else` rather than `Post`. The three
were 25,036 refusals and one miscompile. The corpus now reports 17,367 reaching
SSA and 17,285 lowering, so the identity that held while the accepted set was
small is gone: what is left undecomposed is 70 functions holding a wide
`SelectN`, 9 holding an array and 77 holding a struct, and 82 of them do not
lower.

**The end-to-end test could not have caught the miscompile, and the reason
generalises.** `ssagen`'s `TestLinkAndRun` says in its own comment that its
programs hold no assignment statement, because construction refused one. A
program with no assignment has no counted loop, so nothing in the repository
ever ran one, and the dropped post list was invisible. A compiler's end-to-end
tests are written in the language the compiler accepts, so a construct it
refuses is a construct its gates cannot exercise. Widening what is accepted has
to be followed by widening what is run.

**The reach of the compiler was reported without its denominator.** "Every
function of the distribution's buildable packages compiles" was true of the
functions SSA construction accepts and read as a claim about the distribution.
It was one function in five and is two in five. The denominator was found
during the August 2026 documentation audit by counting `ssa.Build`'s refusals
across the same corpus, which nothing had measured because the corpus tests
skip a function they cannot build rather than counting it. The counts are in
the README and the largest is now a composite literal, at 5,379 functions.

The lesson is about the measurement and not about the compiler. A corpus test
that reports what worked, and drops what did not, produces a number that only
ever goes up. `ssa/build_test.go` now holds a corpus test for construction that
counts the refusals by cause, and `ssa/decompose_test.go` and
`ssa/stackmap_test.go` count them too rather than returning silently.

**The numbers in this file are now gated.** `internal/hygiene` reads them out
of the prose and compares them against a checked-in record of what the tests
measured. Every stale number above was correct when it was written, which is
the whole difficulty: nothing about a stale number looks wrong.

**M0's gate was wrong and was replaced.** It said "`nanogo -V` prints a
version". The real protocol is `-V=full`, it is parsed by
`cmd/go/internal/work/buildid.go` with requirements the spec did not state, and
printing a version is not the thing that matters. The gate is now that a
passthrough `-toolexec` build of a real module completes. That gate exercises
the flag parser, the tool dispatch and the build-ID protocol before any compiler
exists, which is what M0 is for.

**M0 absorbed the quality gates.** CI, the per-package coverage tool and
`CONTRIBUTING.md` were not in any spec's scope and were built in M0 anyway. A
gate added after the code it measures is a gate calibrated to the code.

**The loader was started in M0, not M2.** [014](014-package-loader.md)'s G1 half
and its constraint evaluator are independent of the front end, and building them
early cost nothing and removed a dependency from M2.

**M3 was started from the back.** [040](040-object-format.md) and
[041](041-instruction-encoding.md) need nothing above them, so they were built
before the middle end rather than after it. That was worth doing for one reason:
the object format seam is this deck's largest untested assumption, and building
the writer first turned it into a linked binary that runs. A middle end built
first would have reached the same question three milestones later.

**Twelve IR nodes and one field were added by implementation, not by design.**
The count in this paragraph said eleven and was itself wrong, because the `for`
post statement is a field on the `for` node and not a node. The audit of
[020](020-ir.md) against `ir/node.go` found the real figure. The node set of
[020](020-ir.md) had no assignment, no case, no composite literal, no slice
expression, no `close`, `print` or `println`, and no `unsafe` intrinsics. Each gap was found by a consumer reaching for a workaround:
an assignment encoded as a binary operation with no operator, a literal as
`new`, an intrinsic as a call to an invented symbol. The nodes exist now, and
[020](020-ir.md) records that its own claim, that a construct absent from its
table does not exist, was false when written.

The lesson is not that the spec was incomplete, which every spec is. It is that
a workaround inside an IR is invisible: the tree still type-checks, the tests
still pass, and the cost arrives later as a pass that must recognise an
intrinsic by matching a string.

**Two platform-divergent cgo bugs, and one data race, were found by CI and
could not be found locally.** They are recorded because they say something about
the test strategy rather than about the bugs: cgo file selection differs by
`GOOS`, so any code that decides which files are in a package is untested until
it runs on both platforms; and a concurrency contract stated only in a comment
is one the race detector finds violated. Both are now gated.
[014](014-package-loader.md) carries the first; [010](010-scanner-and-positions.md)
carries the second.
