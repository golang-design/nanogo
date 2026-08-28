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

## Nothing here is built, and the absence is a defect

**nanogo emits no write barrier.** Not one, anywhere, under any condition. No
pass reads `runtime.writeBarrier` and no rule builds the diamond.

Two of the pieces exist. `rtsym` carries all eight `runtime.gcWriteBarrier`
entry points and the flag, each checked against the runtime's own source, and
`ssa.OpARM64LoweredWB` is the operation with its encoder. What is missing is
the pass: the walk that decides which stores need a barrier, and the block
surgery that turns one store into the diamond above. `ssa.Edit.Guard` is the
primitive for the cut, and the join needs a memory phi, which is the part no
existing pass in this compiler builds.

Go's own `test/chanlinear.go` samples the gap. Under `GOGC=1` with the
collector concurrent it deadlocks in about one run of two; under
`GODEBUG=gcstoptheworld=2` it never does; and `gccheckmark=1` names the object
it loses, a capture of a two-capture closure whose descriptor and pointer mask
are both correct. The mask being right and the object still being lost is the
whole of this spec in one measurement.

The three directives reach no check. `driver/pragma.go` recognises
`//go:nowritebarrier`, `//go:nowritebarrierrec` and `//go:yeswritebarrierrec`
and stores a bit for each, with `nowritebarrierrec` implying `nowritebarrier`.
Nothing reads those bits, which is consistent: a check that rejects a barrier
has nothing to reject.

The condition this spec sets for itself is the first pointer store to a heap
location that nanogo emits, and **nanogo emits them today**. Two rows of the
table below that the design marks *required* are already reachable:

- **A pointer into a global.** `var sink []int` with `sink = make([]int, 3)`
  compiles, and the header's pointer word reaches `main.sink` as a plain `MOVD`
  into the symbol's address, with no flag test before it.
- **A pointer into a heap object.** `make([]*int, 2)` lowers to
  `runtime.makeslice` (`ir/lower.go`), and `ps[0] = &n` stores the pointer into
  the memory it returned, again unconditionally. `new(T)` and the address of a
  composite literal lower to `runtime.newobject` in the same file, and nothing
  elides either call.

So this spec is a plan for the design and a record of a live defect for the
code. Every program nanogo compiles that allocates and then stores a pointer
runs with the barrier missing. What hides it is the size of the programs nanogo
compiles: the failure needs a collection to run concurrently with such a store,
and a program small enough to compile at all rarely reaches one. The absence is
silent, not harmless.

The elision rules at the end of this spec have no analysis behind them either.
[023](023-escape-analysis.md) is `draft`, so nothing decides whether a variable
escapes, and both provable cases below are unavailable.

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
