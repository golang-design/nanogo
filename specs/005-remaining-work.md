---
title: "The remaining work: a dependency graph from here to a compiler that compiles Go"
status: in progress
layer: foundation
depends_on:
  - 000-decisions.md
  - 001-bootstrap-gates.md
  - 003-sequencing.md
  - 020-ir.md
---

# The remaining work

[003](003-sequencing.md) gives the order of work as milestones M0 to M10. This
spec is narrower and more mechanical: it is the dependency graph of everything
that is still refused, so that work can run in parallel where the graph permits
it and in sequence where the graph does not.

It exists because the question "what is left" has an answer that a milestone
list does not give. A milestone says which stage the project is in. This graph
says which two pieces of work unblock the most, which pieces can start today,
and which file every one of them has to edit. That last part is the constraint
that decides how much can actually run at once.

## What done means

Four counters already in the tree, so that no new measure is invented for this
spec to grade itself against.

| Gate | Counter | Today | Done |
| --- | --- | --- | --- |
| G-A | `internal/audit` probe classes | 95 ok, 3 refused, 0 wrong, of 98 | all ok |
| G-B | `internal/gotest` corpus passes | 209 of 356, 0 miscompilations | all passing |
| G-C | [020](020-ir.md) Go-specific rows | see the correction below | all built |
| G-D | bootstrap standard library closure | 18 of 28 compile invocations | 28 |

G-A and G-B are decided by `internal/audit/testdata/ratchet.txt` and
`internal/gotest/testdata/ratchet.txt` rather than by this prose, and a
regression in either fails the build.

G-D is a reading and not a gate. The closure is the installed Go
distribution's, so it moves with every Go release, and a ratchet on it would
fail the build on a toolchain upgrade. `internal/selfhost` takes the reading in
one command and [060](060-selfhost.md) states what it found. Its own 19
packages are ratcheted, because those do not move under anybody else's
release.

Then the three gates of [001](001-bootstrap-gates.md): G1 self-host, G2
toolchain independence, G3 the distribution.

G-A to G-D measure the language. They do not measure the executable. Today
nanogo writes object files and `go tool link` writes the program. Making nanogo
write the program is [045](045-linker.md) plus [014](014-package-loader.md)'s G2
half, and it is the largest single remaining item by volume. A reader who asks
"can nanogo compile Go programs into executables" is asking two questions, and
they have different answers and very different sizes.

## The graph

Every construct the first version of this graph ordered is built. The graph
below is what measurement left, and it is a different shape: what remains
between here and the gates is not the language.

```mermaid
graph TD
  classDef done fill:#e8f5e9,stroke:#2e7d32
  classDef key fill:#fff3e0,stroke:#e65100,stroke-width:2px
  classDef gate fill:#e3f2fd,stroke:#1565c0

  LANG["the language subset<br/>all 19 of nanogo's packages compile"]:::done
  READ["export data reading<br/>stage 1 runs against nanogo archives"]:::done

  MSET["FOREIGN INSTANTIATION METHOD SET<br/>the descriptor names four, the object defines two"]:::key
  DESC["two descriptor rows<br/>an array's element, a promoted value receiver"]
  DET["determinism<br/>N2 equals N3 byte for byte"]

  ASM["ABI0 WRAPPERS<br/>an assembly definition, a Go call"]:::key
  HDR["-asmhdr and symabis<br/>driver"]
  ASSEM["nanogo's own assembler"]
  LOWER["two lowering rows<br/>unsafe.Slice, unsafe.String"]

  LINK["nanogo's own linker"]
  LOAD["package loader without go list"]

  G1["G1 self-host<br/>N2 equals N3, exact bytes"]:::gate
  G2["G2 toolchain independence<br/>no go command"]:::gate
  G3["G3 compile the distribution"]:::gate

  LANG --> G1
  READ --> G1
  MSET --> G1
  DESC --> G1
  DET --> G1

  G1 --> G2
  LINK --> G2
  LOAD --> G2

  G1 --> G3
  ASM --> HDR
  HDR --> G3
  ASSEM --> G3
  LOWER --> G3
```

Two rows carry the weight now and neither is a construct.

**A foreign generic instantiation's method set is what G1 waits on.**
[017](017-export-data-reading.md) measured stage 1 and it runs: all 19 packages
compile against dependencies nanogo built, and the reader reads every archive
nanogo writes. The build then stops at the linker, with seven relocations in
three classes. The one that owns the gate is this: `ir/stencil.go` builds a
method of a foreign instantiation only where that method is called, so the
descriptor of `sync/atomic.Pointer[[]*syntax.SrcFile]` promises four methods
and `syntax`'s object defines the two it calls.

The gap is not new at G1 and it is not caused by G1. It is **revealed** there.
`gc` emits a whole instantiation `dupok` in every package that reaches it, and
in today's build six `gc`-compiled importers of `syntax` each supply all four
definitions. Put every importer on the allowlist and the six go with them.
That is worth stating on its own: a build in which `gc` compiles anything can
hide a symbol nanogo never emits, so a gap of this kind cannot be found by any
measurement short of stage 1.

**ABI0 wrappers are what the distribution waits on.** 8 of the 27 standard
library archives the smallest Go program needs are refused for one reason, and
`runtime` is one of the eight. An assembly definition uses ABI0 and a Go call
uses ABIInternal, and nanogo generates no wrapper between the two, no `-asmhdr`
header for the assembly sources to include, and no reader for the `symabis`
file the go command produces. [047](047-abi-wrappers.md) is the design and it
stages the work. This is a G3 row and not a G1 one: nanogo's own source has no
assembly in it, and at G1 `gc` still builds the standard library
([001](001-bootstrap-gates.md)'s hosted-mode allowance).

### What this graph replaced, and why the shape changed

The first version ordered thirteen constructs behind two keystones and put G1
behind seven of them: interface conversion, `defer` and `go` with arguments,
maps, `append` and the string conversions, generics stenciling, the register
allocator's scratch registers, and the wide struct return. All of them are
built, and the section below keeps the two keystones because the record of what
unblocked what is worth more than the claim it replaced.

The shape changed because the graph ordered the wrong thing. It ordered the
language, and the language was never what G1 measures.

It was then wrong a second time, in this document, for a day. The row above
read "export data reading" because this file said the reading half had not
started, and that sentence had gone stale the way every other number here went
stale. [017](017-export-data-reading.md) ran stage 1 and the reader works. The
correction is recorded rather than quietly applied, because the sentence that
was wrong is the same kind of sentence as the ones still here: a claim about
what is unbuilt, taken on trust, that nobody had re-measured.

## The two keystones that were

Both are built. Nothing else in the first graph unblocked as much, and this is
the record of what each one carried.

**Type descriptors with method sets, and itabs.** Thirteen refused probes trace
here: every interface conversion, every type assertion, the type switch, method
values, `stdlib-fmt`, a `panic` whose operand is a concrete value, and a read of
the value `recover` returns. `ir.Type` does not carry a method set, so the
descriptor writer cannot emit an `UncommonType` tail for a type it is asked
about through the IR, and an itab cannot be built at all. [032](032-type-descriptors-and-itabs.md)
owns the design.

**The context register.** Eight refused probes trace here. The caller half is
built: `ssagen` writes the closure word into R26 before an indirect call. The
callee half does not exist. No SSA operation reads R26, `ir.Func` has no field
for a closure context, and the entry block materializes an argument only for a
declared parameter. [033](033-closures-defer-panic.md) owns the design.

The two are independent and run in parallel, with one caveat recorded in
[033](033-closures-defer-panic.md): a closure object's natural type is an
anonymous struct, which `ir/rtype.go` refuses, so the closure work synthesizes a
named type rather than waiting for the descriptor work.

## What the graph does not say, and the file that decides it

The graph orders the work by semantics. It does not order it by the file each
item edits, and that is the constraint that decides how much can run at once.

Almost every Tier 2 row is lowered in `ir/lower.go`. Two builders editing that
file at the same time conflict whatever the graph permits. So the practical rule
is to batch by file and not by feature: one builder takes `ir/lower.go` and
lands a related group of rows, while work in `obj/`, `rtype/`, `rtsym/`,
`export/` and `driver/` proceeds beside it.

## Corrections this graph already made

Six claims in circulation were wrong. They are recorded here so they are not
repeated, and because three of them mean [020](020-ir.md)'s own table oversells
what is left to do.

- **Three rows [020](020-ir.md) marks `not built` are built.** Multi-value
  assignment is built at `ir/build.go:1468` and `ssa/build.go:1039`, and the
  probe passes. Package initialisation is built at `ir/build.go:891`, and two
  probes pass. A method expression is built at `ir/build.go:1809` and has no
  refusal at all. What remains of the multi-value row is only a non-call source,
  which is the map, assertion and receive work under its own rows.
- **Struct and array comparison is built and bounded, not unbuilt.**
  `ssa/decompose.go:1114` rewrites an aggregate comparison into a chain over its
  parts. The limit is `MaxDecomposeParts = 4`. Above four leaves the value is
  never split, an `OpEq` on a struct type survives into `ssa.Lower`, and the
  compiler panics. There is no refusal string for it.
- **A missing lowering rule panics, it does not refuse.** `ssa/lower.go:206`
  panics with a `*LowerError`, and [025](025-lowering-and-rules.md) requires the
  crash. So complex arithmetic and a wide aggregate comparison are compiler
  crashes rather than honest refusals, which is the outcome
  `CONTRIBUTING.md` ranks worst after a wrong answer.

- The capture gap was said to account for nine probes. It accounts for eight.
  `go-stmt-closure` is refused first by `make(chan int)`, because
  `ir.LowerAndCollect` returns the first refusal in statement order, so the probe
  belongs to the channel row and not the closure row.
- Escape analysis was said to block the closure work. It does not.
  `ir/lower.go` already sends every allocation to the heap through
  `runtime.newobject`, which is correct and only slower than `gc`.
  [023](023-escape-analysis.md)'s analysis remains unbuilt, and the miscompile
  it owned, a pointer to an address-taken local outliving its frame, is closed:
  the interim rule that spec already stated is built, and a variable whose
  address the source takes lives in a heap cell like a captured one. What is
  still missing is the judgement that keeps such a variable in the frame.
- **`ir.Type` was said not to carry method sets. It does**, at
  `ir/type.go:258`, for every defined type. What is missing is the signature on
  `ir.Method`, and the two ABI wrappers `rtype/uncommon.go:170` names.
- **A type assertion and a type switch were said to refuse at `ssa.Build`.**
  Both refuse earlier, at `ir/lower.go:838`, because `ir.Lower` is the driver's
  first pass. [032](032-type-descriptors-and-itabs.md) carries the stale claim
  and so does a comment at `ssa/build.go:1045`.

## Two root causes the probe names hide

A probe is named for the Go construct it writes, and two of them are refused for
a reason that has nothing to do with that construct. Both deserve their own row
in any plan, because fixing the construct would not fix the probe.

- `array-local` declares a local array and is refused by the register
  allocator: `ssa/regalloc.go:166`, "needs %d scratch registers and the target
  reserves %d". [060](060-selfhost.md) states the shape, which is that an
  indexed store wants three integer scratch registers and the target reserves
  two. This belongs to [026](026-register-allocation.md), not to arrays.
- `go-stmt-closure` is refused first by `make(chan int)`, not by its closure,
  because `ir.LowerAndCollect` returns the first refusal in statement order.

## What the first build-out settled, and what it cost

Floating point, wide struct returns, closures with captures, `defer` and `go`
with arguments, interface construction and conversion, channels with `select`,
generated equality and hash functions, method wrappers and the stack objects
table are built. The probe corpus went from 45 passing with 3 wrong answers to
70 passing with none, and Go's own corpus from 87 files to 119.

Three defects came out of that work that neither corpus had reached before, and
they are the reason the two keystones were worth doing in the order they were
done. Each was found by a test that ran a program rather than by reading code.

- `runtime.gcbits.*` was written as `SNOPTRDATA` where `gc` writes `SRODATA`.
  The stack object record holds an offset the runtime resolves against
  `moduledata.rodata`, so the collector was scanning stack objects through
  whatever bits happened to lie at that address.
- The arguments bitmap covered the reserved words of register-passed
  parameters, which nothing on the ordinary path writes. It claimed a pointer
  in a word the function never wrote, and a caller's previous call had left a
  heap pointer exactly there.
- A type descriptor was written with `AddDef`. `cmd/link` deduplicates a
  duplicate-tolerant symbol only in the non-package space, so `type:*int32`
  survived beside the runtime's own copy and `runtime.SetFinalizer`, which
  compares descriptors by address, refused a finalizer whose type printed
  identically to the one it was given.

One file of Go's corpus moved backward on purpose. `uintptrescapes3.go` passed
only because the over-conservative arguments bitmap implemented
`//go:uintptrescapes` by accident. Narrowing the bitmap broke it, so the
directive is refused by name now. The hole that remains is recorded in
[016](016-directives-and-pragmas.md): a nanogo-compiled package calling an
imported `//go:uintptrescapes` function is still miscompiled, and the refusal
reads the declaration, so it cannot see that call.

## Write barriers are built

[034](034-write-barriers.md) records two corrections, and both were found by
running programs rather than by reading the spec.

The first: the spec claimed the gap was unreachable because every assignment
form that needs a barrier was refused. All of those forms compile. Assignment
to a local, to a global, to a struct field, to a slice element and through a
pointer each lower, and for as long as none emitted a barrier that was a
collector correctness bug no counter in the table above reported, because every
probe it would break was refused for some other reason first.

The second: the spec described `gc`'s *older* barrier, the one that performs
the store itself. Implementing that shape would have stored twice on one path
or not at all on the other. `gc`'s barrier is buffered, the store is
unconditional and stands on both paths, and the spec now takes the shape from
`gc`'s own disassembly.

`ssa/writebarrier.go` is the pass. Go's `test/gcgort.go` goes from 14 runs in
20 to 20 in 20 under `GOGC=1` with `GODEBUG=gccheckmark=1`. What remains is the
directive check: `//go:nowritebarrier` and its two relatives are parsed and
nothing reads them.

## The critical path to G1, which is longer than the graph suggests

The graph above orders the language work. G1 is not gated by the language as a
whole, only by what nanogo's own source uses, and that subset has one item in it
far larger than the others.

| Blocker | Owner | Size |
| --- | --- | --- |
| Descriptor encoder: a method's signature, a generated equality function, a function's signature | [032](032-type-descriptors-and-itabs.md) | in progress |
| An SSA operation that reads the context register | [033](033-closures-defer-panic.md) | in progress |
| **Export data body reader, and a generic stenciler** | [015](015-export-data.md), [013](013-generics.md) | **not started, two subsystems** |
| ABI0 wrappers for a package with assembly | [030](030-abi.md) | not started |
| The register allocator's scratch row | [026](026-register-allocation.md) | not started |

The generics row is the one to plan around. `export/writer.go` refuses a generic
declaration on purpose, and the reason it gives is the whole problem: stenciling
an instantiation in an importing package needs the function bodies that
[015](015-export-data.md)'s body reader would carry, and nanogo writes
declarations only. So G1 needs a body reader in export data before a stenciler
can exist, and nanogo's own `types2` fork is full of generic declarations.

### What actually blocks G1, measured

Each of nanogo's own 19 library packages is compiled on its own, with an
allowlist holding that package alone, so that a failure upstream cannot hide
one downstream. A whole-tree build stops at the first failure and reports
nothing about the rest, which is why the numbers below were wrong before they
were measured this way. The two commands are not in the count: they are `main`
packages, which the go command builds into executables rather than archives an
importer reads.

`internal/selfhost` runs this measurement and `internal/selfhost/testdata/ratchet.txt`
records it, so a package that leaves the set now fails the build.
[060](060-selfhost.md) states the three traps the harness defends against, each
of which produced a confident wrong number while this was done by hand.

**All 19 compile**: the root package, `dist`, `driver`, `export`,
`export/pkgbits`, `ir`, `link`, `loader`, `obj`, `obj/arm64`, `rtsym`, `rtype`,
`ssa`, `ssa/rules`, `ssagen`, `syntax`, `types2`, `types2/errors` and
`types2/gen`. No package fails.

`types2` was the last, and it cleared three blockers to get there. The foreign
walk carries the statement forms its instantiations reach, and a range over a
function is built. The third and last was a method value whose receiver is an
interface, at `types2/sizes.go`'s `alignof`: it has no symbol to name, because
the function is read out of the itab at run time. It is built now, as a literal
holding the receiver whose body makes the interface call, and it took Go's own
`recover.go` with it ([033](033-closures-defer-panic.md)).

**Compiling every package is not G1.** Each package is compiled on its own,
against `gc`-built dependencies, and what G1 asks for is a compiler that builds
itself and then builds itself again to the same bytes. The critical path above
is what stands between the two, and the export data reader is the largest row
in it.

An earlier version of this table said a *method of a generic type* owned three
of these. That was wrong, and the way it was wrong is worth keeping. The
refusal named `sync/atomic.Load`, which reads like a local declaration, but
`objName` is the package path joined to the object's name, so it was always a
foreign object. The local half is built now, and closing it moved nothing:
`export`, `ir` and `ssagen` moved from one message to another and `syntax` did
not move at all. A refusal names what stopped first, not what a package needs,
and a message that names a foreign object does not say so.

G1 does not close until the reader exists. The writing half is done: a generic
type this package declares is written with its methods numbered against one
dictionary in gc's own order, and gc reads it back, instantiates it and runs it
(`export`'s `TestGcInstantiatesAGenericTypeNanogoWrote`). The reading half has
not started, and a build of nanogo by nanogo is what needs it: a package
compiled here reads its dependencies' export data, and today that data is
gc's.

A closed blocker is not a compiled package, and the table reshapes every time
one closes. Closing the method-value receiver moved `link` and `loader` to
compiling and moved `ir`, `driver` and `ssagen` onto other rows. Closing the
operand spill moved `rtype` and `ssa` onto the first row rather than to
compiling. Every row here is what fails **first** and not the whole of what a
package needs.

One caveat [001](001-bootstrap-gates.md) states about its own gate, repeated
here because it decides how much G1 is worth: $N_2 = N_3$ is necessary and not
sufficient. A compiler that miscompiles a construct it does not itself use
reaches the fixed point while being wrong, and so does one whose miscompile
reproduces stably. The three `wrong` probes are exactly that failure mode.

## What is not decided

`asm-package` is deferred to G3 by [044](044-plan9-assembler.md). **cgo has no
spec and no decision.** Until one is taken, "all Go programs" has no definition,
because a program that imports `"C"` is a Go program by the specification and
nanogo has never had a plan for it. This spec assumes cgo is out of scope and
records the assumption rather than hiding it.
