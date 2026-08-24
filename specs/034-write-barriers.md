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

## Nothing here is built

**nanogo emits no write barrier.** Not one, anywhere, under any condition. The
word does not appear in the compiler outside this spec: no pass reads
`runtime.writeBarrier`, no lowering rule builds the diamond, and
`runtime.gcWriteBarrier` is not in `rtsym`, which is the table
[031](031-runtime-lowering.md) requires every generated call to come from.
`rtsym` has a write barrier group and the group has no members.

The three directives are not read either. `//go:nowritebarrier`,
`//go:nowritebarrierrec` and `//go:yeswritebarrierrec` reach no check, which is
consistent: a check that rejects a barrier has nothing to reject.

This is safe today, and only because of what nanogo compiles. The barrier is a
rule about a pointer store to a heap location, and nanogo emits no such store.
The assignment statement is refused by SSA construction, and so is every form
that allocates, so the only stores that reach the code generator are the ones
the compiler introduces itself: a spill to a frame slot, a copy into an
argument area, a block move between them. Every destination is the stack.

The condition to watch is therefore narrow and it is not "the runtime
compiles". The first pointer store to a heap location that nanogo emits is the
point at which this spec stops being a plan and starts being a defect.

The rest of this spec is the design, unchanged.

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

A barrier is a conditional call, not an unconditional one:

```
if runtime.writeBarrier.enabled {
    runtime.gcWriteBarrier(dst, val)
} else {
    *dst = val
}
```

The flag is a byte in a runtime global, read on every pointer store. The branch is
predictable and cheap; the call happens only during a collection's mark phase.

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
