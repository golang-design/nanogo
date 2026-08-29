---
title: "Write barriers"
status: draft
layer: runtime interface
gate: G1
depends_on:
  - 031-runtime-lowering.md
  - 016-directives-and-pragmas.md
---

# Write barriers

Go's collector runs concurrently with the program. A pointer store that the
collector does not observe can hide a reachable object from a marking phase that
has already passed the place the pointer came from. The compiler's job is to make
those stores observable.

A missing write barrier does not fail a test. It frees a live object under
concurrent marking, some of the time.

## What is built

`ssa/writebarrier.go` is the pass. It runs between instruction selection and
register allocation (`driver/compile.go`), walks every block, and replaces each
pointer store into memory the collector may own with the diamond below.

The position in the pipeline is the whole of the design and both sides of it
are forced. It runs **after** selection because the values it inserts are
machine operations, and a target-neutral barrier would need a rule to select
with nothing to choose between. It runs **before** allocation so that the call
it makes is a safepoint like any other: the allocator spills what is live
across it and [027](027-liveness-and-stackmaps.md) describes the frame at it.
Critical-edge splitting runs after the pass and repairs the edges the diamond
creates.

The three directives still reach no check. `driver/pragma.go` recognises
`//go:nowritebarrier`, `//go:nowritebarrierrec` and `//go:yeswritebarrierrec`
and stores a bit for each. Nothing reads those bits, so a function that
forbids a barrier is not told when it gets one. That check is the remaining
work in this spec, and it matters only for runtime code, which nanogo does not
compile.

The elision rules at the end have one analysis behind them and not two. A
destination that is provably `ADDframe` is a frame slot and gets no barrier.
The "freshly allocated and not yet published" row needs
[023](023-escape-analysis.md), which is `draft`, so that row is unavailable and
the barrier is emitted there.

### How it was measured

Go's own `test/gcgort.go` is the program that samples the gap, under `GOGC=1`
with `GODEBUG=gccheckmark=1`:

| | without the barrier | with it |
| --- | --- | --- |
| `gcgort.go`, 20 runs | 14 pass | 20 pass |
| `gcgort.go`, 20 runs, no checkmark | 18 pass | 20 pass |

`test/chanlinear.go` samples the same gap and samples it by machine load: it
deadlocked in about one run of two on a busy machine and passed 20 of 20 on a
quiet one, with and without the barrier. A file that only fails under load is
evidence when it fails and is not evidence when it passes, which is why the
table above is `gcgort.go`.

### The defect this pass introduced, and what now catches it

The first version of the pass made its own `OpSB` rather than reusing the one
lowering had already put in the entry block, and typed it with the pointer
type an interface's data word carries. The two spellings of a machine word
differ in their pointer map and in nothing else, so the frame then had a spill
slot that the locals bitmap called a pointer and that held the static base.
`runtime.adjustpointers` reads exactly those words when a stack grows under
the frame, and Go's own `test/linkmain_run.go` stopped with `bad pointer in
frame main.main` on every run.

Two things close it. `ssa/decompose.go` now writes both machine-word pointer
types in one place, `unsafePtrType` and `machinePtrType`, with the reason they
differ next to them. And `ssa/verify.go` gains `InvOneBase`: a function has at
most one `OpSP` and one `OpSB`, and neither may carry a type that holds a
pointer. Any future pass that makes a second base fails verification in the
compiler rather than in the collector.

## When a barrier is required

A store of a **pointer** to a location that may be in the **heap**.

Both conditions are needed and each removes work:

| Store | Barrier |
| --- | --- |
| pointer into a heap object | required |
| pointer into a local that is provably in the frame | not required |
| pointer into a global | required; globals are scanned but the barrier is still needed for deletion tracking |
| non-pointer of any kind | not required |
| pointer into freshly allocated memory not yet published | not required |

The third row is the one that surprises. Go's barrier is a **hybrid** barrier: it
records both the old value being overwritten and the new value being written, so
that both deletion and insertion are tracked. That is what allows the collector
to avoid rescanning stacks, and it means the barrier is needed even where a
purely insertion-based barrier would not need one.

## The generated code

A barrier records a pair and does not perform the store. The store is
unconditional and happens on both paths:

```
if runtime.writeBarrier.enabled {
    buf := runtime.gcWriteBarrier2()   // two slots, returned in R25
    buf[0] = val                       // the value being written
    buf[1] = *dst                      // the value being overwritten
}
*dst = val
```

This is `gc`'s **buffered** barrier and the shape is taken from what `gc`
emits, not from a description of it. For `func set(p *node, v *node) { p.next
= v }`, with `p` in R0 and `v` in R1:

```
MOVB  (R0), R27                  // the nil check of p
ADRP  <runtime.writeBarrier>, R27
MOVWU 2096(R27), R2              // enabled, read as a 32-bit word
CBZW  R2, +4(PC)                 // not marking: straight to the store
MOVD  (R0), R2                   // R2 = the old value
CALL  runtime.gcWriteBarrier2(SB)
STP   (R1, R2), (R25)            // buf[0] = new, buf[1] = old
MOVD  R1, (R0)                   // the store, on both paths
```

An earlier draft of this section had the older form, a call that performs the
store and an else that performs it instead. Building that would store twice on
one path or not at all on the other. The flag is a byte at offset zero of the
`runtime.writeBarrier` variable and the compiler reads it as a 32-bit word,
which is what the three bytes of padding beside it are declared for.

### The operation

`runtime.gcWriteBarrier2` does not follow the Go ABI. It takes nothing, returns
the buffer pointer in R25, and clobbers R27 and the link register and no other
general register.

`ssa.OpARM64LoweredWB` is the call and the read of R25 as **one** operation.
They cannot be two, because R25 is also the third reload register
[026](026-register-allocation.md) reserves, and a spill placed between a call
and a read of R25 would destroy the pointer the barrier just returned. The
operation is a call by the allocator's reckoning, so everything live across it
is spilled. That is more than `gcWriteBarrier2` clobbers, and the difference is
instructions rather than correctness.

The eight entry points are declared `<ABIInternal>` in the runtime's assembly
and `rtsym.Internal` records it. An assembly symbol has no Go declaration, so
nothing else says which ABI it is: the two morestack entry points are ABI0 and
these eight are not, and a caller that inferred ABI0 from "written in assembly"
would name a symbol nothing defines.

Two consequences for the earlier passes:

1. **A pointer store is a branch.** [021](021-ssa-construction.md) sees a store
   and [025](025-lowering-and-rules.md) turns it into a diamond, which means the
   block structure changes late. The lowering must therefore run before
   [027](027-liveness-and-stackmaps.md), which it does.
2. **The barrier call is a safepoint.** Everything live across it is in the frame.

### Batching

Several consecutive pointer stores to the same object share one flag test and one
call with several pairs. This matters for struct assignment and for `copy` of a
slice of pointers, where the alternative is a test per word.

## The directives

Three of [016](016-directives-and-pragmas.md)'s pragmas exist for this pass, and
the runtime uses all three:

| Directive | Meaning | Compiler action |
| --- | --- | --- |
| `//go:nowritebarrier` | This function must not need a barrier | Reject at compile time if one is generated |
| `//go:nowritebarrierrec` | Neither may anything it calls | Reject, checked transitively over the call graph |
| `//go:yeswritebarrierrec` | Stop the transitive check here | Prune the traversal |

The recursive check is a real graph traversal over the package's call graph, not
a local check. It exists because the runtime has functions that run in contexts
where the barrier's own machinery is not available, inside the collector or on
the system stack with no goroutine, and a barrier there deadlocks or corrupts.

This check is a **G3 requirement**. Nothing outside the runtime uses these
directives, and the runtime is compiled at M9 in [003](003-sequencing.md). It is
specified now because the check has to be designed into the call graph
construction rather than added to it.

## Elision, and its limits

The compiler may omit a barrier only when it can prove the destination is not in
the heap. The two provable cases:

1. The destination is a frame slot of a variable [023](023-escape-analysis.md)
   determined does not escape.
2. The destination is memory returned by an allocation in the same function that
   has not yet been made reachable from anywhere else.

Everything else gets a barrier. Being wrong in the conservative direction costs a
branch. Being wrong in the other direction is the failure at the top of this
spec.

## Testing

- `GODEBUG=gccheckmark=1` under concurrent mutation of a large pointer graph.
  This is the test that finds a missing barrier, and it finds it as a crash.
- A corpus of every store shape from the table above, with the generated code
  inspected for the presence or absence of the barrier. Inspecting generated code
  is unusual in this deck and is justified here because the runtime behaviour is
  probabilistic.
- The directive checks, positive and negative, including a transitive case where
  the barrier appears three calls deep.
- The runtime itself at M9, which is the only complete test.

## What was wrong

- This spec said the missing barrier was safe, because "the assignment
  statement is refused by SSA construction, and so is every form that
  allocates", which left the stack as the only destination of any store. All
  three claims were false. Assignment to a local, to a global, to a struct
  field, to a slice element and through a pointer all compile; `make` lowers to
  `runtime.makeslice` and `new` to `runtime.newobject`; and a store into a
  package-level pointer targets the symbol's address. The section above names
  the reachable rows instead.
- It said the word did not appear in the compiler outside this spec. The pragma
  parser names all three directives. What is true is narrower: nothing reads
  the bits the parser stores.
