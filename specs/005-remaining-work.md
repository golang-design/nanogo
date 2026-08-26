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
| G-A | `internal/audit` probe classes | 45 ok, 47 refused, 3 wrong | 95 ok |
| G-B | `internal/gotest` corpus classes | 356 classified, most refused | all passing |
| G-C | [020](020-ir.md) Go-specific rows | see the correction below | all built |
| G-D | bootstrap standard library closure | 8 of 27 packages | 27 |

Then the three gates of [001](001-bootstrap-gates.md): G1 self-host, G2
toolchain independence, G3 the distribution.

G-A to G-D measure the language. They do not measure the executable. Today
nanogo writes object files and `go tool link` writes the program. Making nanogo
write the program is [045](045-linker.md) plus [014](014-package-loader.md)'s G2
half, and it is the largest single remaining item by volume. A reader who asks
"can nanogo compile Go programs into executables" is asking two questions, and
they have different answers and very different sizes.

## The graph

```mermaid
graph TD
  classDef free fill:#e8f5e9,stroke:#2e7d32
  classDef key fill:#fff3e0,stroke:#e65100,stroke-width:2px
  classDef gate fill:#e3f2fd,stroke:#1565c0

  FLOAT["floats<br/>ssagen only"]:::free
  WIDE["wide struct return<br/>decompose + ABI"]:::free
  SCRATCH["regalloc scratch<br/>three where two"]:::free
  DRIVER["modinfo, go:embed<br/>driver only"]:::free
  SEQ["append, string conversions<br/>range over string"]:::free
  SENDRECV["send, recv, select, delete<br/>nothing but the work"]:::free

  DESC["TYPE DESCRIPTORS<br/>method signatures, itabs"]:::key
  CTX["CONTEXT REGISTER<br/>an SSA op that reads R26"]:::key

  IFACE["interface conversion<br/>assertion, switch"]
  MAPS["maps"]
  CHANS["channels, select"]
  CAPT["defer and go with arguments<br/>method values"]
  RANGE["range over map, chan, func"]
  GEN["generics stenciling"]

  DESC --> IFACE
  DESC --> MAPS
  DESC --> CHANS
  CTX --> CAPT
  MAPS --> RANGE
  CHANS --> RANGE
  CTX --> RANGE
  IFACE --> GEN
  DESC --> GEN

  IFACE --> G1
  CAPT --> G1
  MAPS --> G1
  SEQ --> G1
  GEN --> G1
  SCRATCH --> G1
  WIDE --> G1

  G1["G1 self-host<br/>N2 equals N3, exact bytes"]:::gate
  G2["G2 nanogo's own linker<br/>pclntab, moduledata"]:::gate
  G3["G3 compile the distribution"]:::gate
  G1 --> G2 --> G3
```

Tier 0 and the two keystones have no incoming edge. They can all start at once.
Everything in Tier 2 waits on a keystone or on nothing at all, and the column of
Tier 2 items with no incoming edge is the work that is available whenever a
builder is free.

## The two keystones

Nothing else in the graph unblocks as much.

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
  [023](023-escape-analysis.md) remains unbuilt, and the miscompile it owns, a
  pointer to an address-taken local outliving its frame, is a separate defect
  from anything the closure work touches.
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

## Floating point is one file away

Every layer below `ssagen` is already floating-point aware and tested. The
register allocator has a separate class with its own free list and scratch pair
([026](026-register-allocation.md)). The lowering rules select `FADD`, `FMUL`,
`SCVTF` and the rest. The encoder agrees with `go tool asm` on 99,368 floating
point encodings. The ABI assigns arguments and results to F0 to F15.

What is missing is the code generator: `ssagen` copies a register with an
integer `MOV`, spills with an integer store, and breaks a parallel-move cycle
with an integer scratch register. So it refuses at three doors,
`ssagen/ssagen.go:424`, `ssagen/ssagen.go:889` and `ssagen/prologue.go:214`.
[042](042-arm64-backend.md) records this. It is the smallest piece of work in
this spec with a whole class of programs behind it.

## Write barriers are a defect, not a missing feature

[034](034-write-barriers.md) records its own correction: the spec first claimed
the gap was unreachable because the assignment forms that need a barrier were
all refused. All of those forms compile. Assignment to a local, to a global, to
a struct field, to a slice element and through a pointer each lower today, and
none emits a barrier. That is a collector correctness bug that no counter in the
table above reports, because every probe it would break is refused for some
other reason first. It belongs with the miscompiles and not with the unbuilt
rows.

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

Of the 28 packages a `func main() {}` needs, nanogo compiles 9 and refuses 19:
8 for assembly, 7 for a declared type's descriptor, 2 for register allocator
output, 1 for a package-level `error` variable, and 1 for `append`. The count of
nanogo's own packages that nanogo compiles is zero.

One caveat [001](001-bootstrap-gates.md) states about its own gate, repeated
here because it decides how much G1 is worth: $N_2 = N_3$ is necessary and not
sufficient. A compiler that miscompiles a construct it does not itself use
reaches the fixed point while being wrong, and so does one whose miscompile
reproduces stably. The three `wrong` probes are exactly that failure mode, and
so is every missing write barrier.

## What is not decided

`asm-package` is deferred to G3 by [044](044-plan9-assembler.md). **cgo has no
spec and no decision.** Until one is taken, "all Go programs" has no definition,
because a program that imports `"C"` is a Go program by the specification and
nanogo has never had a plan for it. This spec assumes cgo is out of scope and
records the assumption rather than hiding it.
