---
title: "Closures, defer, panic, and recover"
status: draft
layer: runtime interface
gate: G1
depends_on:
  - 023-escape-analysis.md
  - 031-runtime-lowering.md
---

# Closures, defer, panic, recover

Four features that share one property: each makes the frame itself a data
structure that the runtime inspects. They are specified together because their
interactions are where the bugs are.

## Closures

A function literal that references variables from an enclosing function becomes a
**closure object**: a pointer to the code, followed by the captured variables.

```
+------------------+
| code pointer     |
+------------------+
| captured var 1   |
| captured var 2   |
| ...              |
+------------------+
```

At a call, the closure object's address is in the context register — R26 on
`arm64` ([030](030-abi.md)) — and the function body loads captures relative to
it.

### Capture by value or by reference

A variable is captured **by value** when the closure does not assign to it and
neither does anything else after the closure is created. Otherwise it is captured
**by reference**, which means the variable is moved out of the frame into a heap
cell and both the enclosing function and the closure access it through a pointer.

The decision belongs with [023](023-escape-analysis.md), since a by-reference
capture is an escape.

### Where the closure object lives

On the stack when the closure does not outlive the frame, on the heap when it
does. A closure that is only called immediately, which is the common case in
`defer` and in `range`-over-function, stays in the frame and costs nothing.

## Defer

Three implementations, chosen per call site. The choice is a real difference in
cost, not a tuning knob.

### Open-coded

When a function's deferred calls are all unconditional or conditional in a way
the compiler can track, and there are at most eight of them, and none is in a
loop:

- the deferred function and its arguments are evaluated into frame slots at the
  `defer` statement;
- a bitmask in the frame records which have been armed;
- the deferred calls are emitted inline before each `return`;
- `FUNCDATA_OpenCodedDeferInfo` describes the slots and the bitmask so that
  `panic` can run them from the runtime.

This is the fast path and it costs about as much as writing the call out by hand.
It is also the one that requires the runtime to understand the frame, through
that `FUNCDATA`, which is why it cannot be approximated.

### Stack-allocated `_defer`

When open-coding does not apply but the `defer` is not in a loop: a `_defer`
record in the frame, linked onto the goroutine's defer chain by
`runtime.deferprocStack`.

### Heap-allocated `_defer`

`defer` in a loop, where the number of records is not known: `runtime.deferproc`,
allocating.

### `deferreturn`

A function containing any non-open-coded `defer` ends with a call to
`runtime.deferreturn`, which runs the chain. Open-coded defers do not need it on
the normal return path, only on the panic path.

## Panic and recover

`panic` is `runtime.gopanic`. It walks the goroutine's defer chain, running each
deferred call in a frame it constructs, and either finds a `recover` or reaches
the bottom and terminates the program.

`recover` is `runtime.gorecover`, and it is **only effective when called directly
from a deferred function**. The compiler's obligation is to pass the caller's
frame pointer so the runtime can check that relationship. This is also why
[024](024-inlining-and-devirtualization.md) refuses to inline a function
containing `recover`: inlining changes which frame it is in, and would change its
meaning from "recovers" to "does not".

### The interaction that must be right

A panic unwinds through frames whose deferred calls are open-coded. The runtime
has no defer records for those; it has the `FUNCDATA` table and the frame. So the
panic path reads compiler-generated metadata to reconstruct calls that were going
to be emitted inline.

If the table is wrong, a panic in a function with open-coded defers runs the
wrong function, or none, and the failure appears only on the panic path, which
ordinary tests do not take.

## Testing

- Go's `test/` corpus has directories for `defer`, `recover`, and closures, and
  they cover the interactions above better than a new suite would.
- A corpus that panics through every defer implementation, including a mix in one
  call chain: open-coded frame calls stack-allocated calls heap-allocated.
- `recover` in a directly deferred function, in a nested one, and in a function
  called by a deferred one. Only the first recovers, and asserting the other two
  do not is the test that catches a frame-pointer mistake.
- Closure capture by value and by reference, with the loop-variable semantics of
  Go 1.22 and later, where each iteration has a fresh variable.
