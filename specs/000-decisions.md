---
title: "Decisions: what nanogo is, and what it is not allowed to become"
status: complete
layer: foundation
depends_on: []
---

# Decisions

This file is **normative**. A spec that contradicts it is wrong, and the fix is
either to change the spec or to change this file with the reasoning written
down. Every other spec in this directory inherits from here.

nanogo is a compiler for the Go programming language as defined by the
[Go language specification](https://go.dev/ref/spec). The measure of the project
is not a feature list. It is a fixed point: **nanogo compiles its own source,
and the compiler it produces is byte-identical to itself.**

## The correction this deck is built on

The roadmap was first framed as two options that were thought to be sequential:

- **A**: write the whole front end from scratch: scanner, parser, and type
  checker.
- **B**: use `go/parser` and `go/types` from the standard library.

with A named as the goal because bootstrapping was thought to need it.

**Bootstrapping does not need it.** The reasoning is one step long. `go/types`
is ordinary Go source. If nanogo imports it, then "nanogo compiles nanogo" means
nanogo must compile `go/types`, which nanogo must be able to compile anyway to
compile the distribution. A dependency written in Go is not an obstacle to
self-hosting. It is one more package on the compile list.

The stronger form of the argument is in the reference implementation.
`cmd/compile/internal/types2` is 23,222 lines and is a maintained fork of
`go/types`, which is 24,953 lines. **The Go compiler does not have a
from-scratch type checker either.** It has a fork it owns.

So A and B are not stages. The parts of A that pay and the parts that do not are
different parts:

| Front-end part | Reference size | Decision | Why |
| --- | --- | --- | --- |
| Scanner + parser | 7,522 lines (`cmd/compile/internal/syntax`) | **Write it** | `go/ast` is a lossy tooling AST. `syntax` exists because a compiler needs a different tree. The cost is small and the payoff is direct. |
| Type checker | 23,222 lines (`types2`) | **Fork it** | Rewriting type inference for generics is the largest correctness risk in the project and buys nothing that bootstrapping needs. |

This is decision 1 below. It is stated first because it is the decision that the
rest of the deck is shaped around.

## The decisions

### 1. The front end is owned by forking, not by rewriting

nanogo has its own scanner and parser, producing its own syntax tree
([011](011-parser-and-ast.md)). nanogo's type checker is a fork of `go/types`,
vendored into the repository as `types2` and owned from that point
([012](012-type-checking.md)).

"Owned" is the operative word. The fork is not a dependency to be tracked
upstream. It is nanogo source that nanogo maintains, extends with the positions
and pragmas a compiler needs, and compiles with itself.

The parser is written rather than forked because the requirement differs. A
compiler front end wants a tree with resolved positions, no comment attachment,
no source fidelity guarantees, and a shape that lowers cleanly. `go/ast` is
built for `gofmt` and for tooling that rewrites source. Those are opposed goals.

### 2. Bootstrapping is three gates, not one milestone

The word "bootstrap" collapses three independent properties. They are separated
in [001](001-bootstrap-gates.md) and named **G1**, **G2**, and **G3**
throughout. Every spec states which gate it serves.

Briefly: G1 is self-compilation to a fixed point. G2 is toolchain independence,
which is the retirement of every external binary. G3 is compiling the Go
distribution. G1 does not imply G2, and G3 forces work that G1 does not.

### 3. The compiler emits object files, not assembly text

nanogo writes Go object files in the `goobj` format directly
([040](040-object-format.md)). It does not emit Plan 9 assembly text and shell
out to `go tool asm`.

This decision was taken against evidence, not by preference. Two spikes were
run, and both are kept in [`../spikes`](../spikes).

The first spike asked whether text assembly can express per-PC garbage
collection stack maps, which is the hardest metadata a compiler emits. It can.
A hand-written `FUNCDATA` symbol holding two bitmaps, selected per call site by
`PCDATA $PCDATA_StackMapIndex`, produced the two different collection outcomes
it declared: the object survived at index 0 and was collected at index 1. Text
assembly is not disqualified by GC metadata.

The second spike asked whether text assembly can define the symbols a compiler
must define. It cannot. The assembler rejects any name that contains a colon:

```
./s2_arm64.s:2: expect two operands for DATA      // DATA type:main.Obj(SB)/8, $7
```

The offending file is kept in the spike as `s2_arm64.s.txt`, because a file that
must not assemble cannot sit in a package that is built. CI copies it back to a
`.s` name and fails if the assembler ever accepts it.

The compiler and the linker share a namespace built on that character:
`type:*` for type descriptors, `go:itab.*` for itabs, `go:string.*` for string
data. The linker scans those prefixes to build `runtime.typelinks` and
`runtime.itablinks`. A compiler that cannot write those names cannot register
its itabs, so a dynamic interface conversion would construct a second itab for a
pair that already has one, and itab pointer equality would break. That is a
correctness failure, not a limitation.

The ceiling is therefore fatal and the seam is object files. Textual assembly is
kept as a `-S` debugging output ([052](052-diagnostics.md)) and is not a build
path.

The cost of this decision is that nanogo needs its own instruction encoder from
the first milestone ([041](041-instruction-encoding.md)). The cost is bounded:
`cmd/internal/obj/arm64` is 36,224 lines, tests included, because it encodes
the whole instruction set and most of it is generated tables. nanogo encodes the
instructions its own code generator emits, and that is 2,183 non-test lines.

### 4. External dependencies are explicit, and each has a retirement gate

At G1, nanogo runs alongside the Go toolchain and uses parts of it. That is
allowed. What is not allowed is for the dependency to be implicit. Every one is
listed here with the gate that removes it.

| Dependency | Used for | Retired at | By |
| --- | --- | --- | --- |
| `go tool link` | producing an executable | G2 | [045](045-linker.md) |
| `go list` / `golang.org/x/tools/go/packages` | resolving the import graph | G2 | [014](014-package-loader.md) |
| `go tool asm` | assembling hand-written `.s` files; in hosted mode `gc` and the `go` command do this | G3 | [044](044-plan9-assembler.md) |
| `gc` | building stage 0 of the bootstrap | never | see [001](001-bootstrap-gates.md); a bootstrap needs a seed |
| Go runtime source | the runtime itself | never | it is Go source; nanogo compiles it |

The runtime line is the one most often misread. nanogo does not write a runtime.
The Go runtime is 112,977 lines of Go, with hand-written assembly beside that,
and it is *input* to nanogo, not a component of it. Compiling it is G3.

### 5. One typed IR, one SSA form, targets are parameters

The pipeline has exactly two intermediate representations
([002](002-architecture.md)): a typed tree IR that is close to the source and
carries Go semantics, and an SSA control-flow graph that is progressively
lowered until it is machine-specific.

A target does not get its own path through the compiler. It supplies a lowering
rule set, a register set, an ABI, and an encoder. Adding `amd64`
([043](043-amd64-backend.md)) must not require an edit to the middle end. If it
does, the middle end is wrong.

**A counterexample is in the tree, and it is recorded rather than argued away.**
`ssagen` sits between the last SSA pass and the object writer. It encodes
instructions, builds the prologue and the stack growth tail, emits relocations
and attaches stack maps, and it is 3,241 non-test lines of which the prologue is
arm64 code. It is above [025](025-lowering-and-rules.md)'s target boundary and it
names a target. Adding `amd64` will therefore require an edit there, which is
what this decision says must not happen.

Three separate audits of the deck found this independently, each from a
different direction: no spec owns `ssagen`, [002](002-architecture.md)'s package
layout did not list it, and no reading of the tree reached it.
[002](002-architecture.md) now lists the package and records the gap, and the
stage still has no spec of its own. That is the signature of a stage nobody
specified. The decision is not weakened by the
counterexample. The repair is to give the stage a spec and a target interface
of its own, and [043](043-amd64-backend.md) is the milestone that will force it,
because that is the first time a second target reads the code.

This is also why the GPU work fits without distorting anything
([070](070-gpu-target.md)). A GPU backend is another consumer of the same SSA.
It is a consumer and never a driver: **no decision in this deck may be taken to
suit the GPU target.** The requirement it does impose is the one already stated,
that the middle end stays target-neutral.

### 6. Correctness is differential, and the corpus already exists

nanogo does not get its own conformance suite written from nothing. Go has one,
and it is the same tree nanogo must eventually compile
([004](004-conformance.md)).

Three sources, in increasing strength:

1. `test/` in the Go distribution, 356 files including `errorcheck` cases that
   assert the exact position and text of a rejection.
2. The standard library's own tests, compiled by nanogo and run.
3. Differential execution: build a program with `gc` and with nanogo, run both,
   compare output. A disagreement is a nanogo bug until proven otherwise.

The fixed point of G1 is a gate, not a test. It proves the compiler is
self-consistent. It does not prove it is correct, because a compiler can be
wrong in a way that reproduces exactly.

### 7. Output is deterministic

The same inputs produce byte-identical output. No map iteration order reaches
the output, no timestamps, no absolute paths that are not requested, no
address-dependent ordering ([053](053-determinism.md)).

This is not hygiene. G1 is defined as a byte-identical fixed point, so
non-determinism does not degrade the gate. It removes it.

### 8. `cgo` is out of scope for v1

nanogo does not implement `cgo`. Packages that need it do not compile. The
`CGO_ENABLED=0` subset of the standard library is the target
([062](062-distribution-build.md)).

This bounds G3 honestly: "compiles the distribution" means the pure-Go
distribution, and the specs say so rather than discovering it late.

### 9. `darwin/arm64` first, `linux/amd64` second

The host is the first target ([042](042-arm64-backend.md)). A compiler that
cannot run its own output on the machine that built it has a slow feedback loop,
and the feedback loop is the project's main resource.

`linux/amd64` is second and is what proves decision 5. A third target is not
planned in this deck.

### 10. nanogo is object-compatible with `gc`, and has two modes

nanogo's object files, export data, ABI, symbol names, and runtime data
structures are `gc`'s. A package compiled by nanogo links against packages
compiled by `gc`, and the reverse.

This is what makes bring-up incremental. `go build -toolexec=nanogo` hands every
compile invocation to nanogo, and nanogo compiles the packages it can and execs
`gc` for the rest ([051](051-build-integration.md)). A milestone is then not "the
compiler works" but "these 30 packages compile with nanogo and the binary still
passes their tests", and a failure names one package.

The mechanism is spiked, like decision 3's.
[`spikes/toolexec`](../spikes/toolexec) substitutes a logging passthrough for the
whole toolchain on a real build: 110 invocations, 59 of them `compile`, and a
working binary at the end. It also measured the flag set the `go` command
actually sends and the `-V=full` build-ID protocol, both of which
[050](050-driver.md) had drafted wrongly from the help text.

The harder half of the claim, that nanogo's objects and export data
interoperate with `gc`'s, could not be spiked, because it is not testable
before there is a compiler. M3 in [003](003-sequencing.md) tested it and it
holds in both directions that are built. `internal/e2e` compiles a leaf package
with nanogo under a real `go build -toolexec=nanogo`, links it with the real
linker against `gc`-compiled packages and the real runtime, and runs it; and
`export/` reads `gc`'s export data and writes it, so a nanogo-compiled package
can import a `gc`-compiled one and a `gc`-compiled package can import a
nanogo-compiled one. `gc` reads back what nanogo wrote for 275 of the 375
standard library packages, and refuses none of the 275
([015](015-export-data.md)). What is not built is a generic declaration, which
the writer refuses by name ([013](013-generics.md)) and which accounts for all
100 packages it will not write, and function bodies, which
[024](024-inlining-and-devirtualization.md) needs. The fallback in this
decision is not taken.

Without this, the first milestone that runs real code requires the runtime, the
whole standard library, and the linker at once, and a crash has five hundred
suspects.

nanogo therefore has two modes, and they use the same formats:

| Mode | Who compiles what | Serves |
| --- | --- | --- |
| **hosted** | nanogo compiles a subset, `gc` compiles the rest, `go build` drives | M3 through M6, and every regression hunt afterwards |
| **whole-world** | nanogo compiles every package including the runtime | G2 and G3 |

The costs, accepted:

- nanogo is pinned to one Go release at a time. The object format, the export
  data format, and the runtime's internal structures all change between
  releases. The pin is asserted at startup and a mismatch is an error.
- Several data layouts must match exactly, and a mistake in one is memory
  corruption rather than a failed test. This is the risk that hosted mode's
  per-package blame is there to contain.

If compatibility proves unmaintainable, whole-world mode is a complete fallback:
nanogo only has to agree with itself. The bring-up strategy is what would be
lost, not the project.

## What is deliberately not decided here

- The optimization set. [022](022-optimization-passes.md) owns it. The only
  normative constraint is that no optimization may be required for correctness.
- The register allocator algorithm. [026](026-register-allocation.md) owns it.
- Whether the linker is written before or after `amd64`. [003](003-sequencing.md)
  owns the order of work, and it will change.

## The v1 milestone

v1 is **G1 plus `darwin/arm64`**: nanogo compiles nanogo, the result is a fixed
point, and the binaries run. The Go distribution does not compile at v1 and the
linker is still `go tool link`.

Everything past that is real and specified and not v1.

## What was wrong

**Decision 5 gave `ssagen` a size that the package had outgrown.** It read 2,664
lines. The package is 3,241 non-test lines, which is what
[002](002-architecture.md) and [042](042-arm64-backend.md) already say. The
counterexample the number supports is unchanged and is still open: the stage
names a target, and [043](043-amd64-backend.md) is the milestone that will force
a spec and a target interface for it.

**Two of decision 5's three pieces of evidence had gone out of date.**
[002](002-architecture.md)'s package layout lists `ssagen` now and records the
gap, so "never listed it" was false; and the third clause pointed at an
accounting of the tree that no longer exists. The clause that still holds, that
no spec owns the stage, is stated as present fact and the other two as history.

**Decision 3 compared two line counts measured different ways.** 36,224 for
`cmd/internal/obj/arm64` counts the package's tests and 2,183 for nanogo's
encoder does not, so the ratio read larger than it is. Both numbers stand and
each now says which it is.

**The runtime's 112,977 lines were described as "Go and assembly".** The count
is the package's non-test Go files. The hand-written assembly is beside it and
is not in the figure. [001](001-bootstrap-gates.md) carried the same wording and
is corrected with it.
