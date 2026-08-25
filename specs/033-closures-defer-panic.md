---
title: "Closures, defer, panic, and recover"
status: in progress
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

## What is built

The captureless half of each feature is built, and every capture is refused.
`ir/lower.go` performs the rows and `internal/e2e` runs each one as a program.

| Feature | State |
| --- | --- |
| a function literal that captures nothing | built; a one-word `funcval` holding the code pointer |
| a method value, or a literal with a capture list | **refused**; a capture is read through the context register and no SSA operation reads one |
| `defer f()` with no arguments and no captures | built; `runtime.deferproc`, plus the single exit below |
| `defer` with arguments | **refused**; an argument becomes a capture |
| `go f()` on the same terms | built; `runtime.newproc` takes the same one word |
| `panic` | built; `runtime.gopanic`, and a deferred call runs off the chain while panicking |
| `recover()` whose value nobody reads | built; `runtime.gorecover`, which is the shape of the idiom |
| `recover()` whose value is read | **refused**; the result is an interface and nothing below the IR decomposes one |
| open-coded `defer` and `FUNCDATA_OpenCodedDeferInfo` | **not built**; nanogo writes three funcdata indices and that is not one of them |
| a heap `_defer` record built by the compiler | **not built**; `runtime.deferproc` builds it |

**One thing the whole feature turns on does not exist: the context register.**
A closure reads a capture through it, and no SSA operation reads it, so every
form that needs a capture is refused rather than miscompiled. That single gap
is what the refusals above have in common, and closing it is the work that
unblocks the rest of this spec.

### The single exit, which the linker requires

A function that defers leaves through one epilogue, `.deferexit`, and that
epilogue holds the only call to `runtime.deferreturn`. `cmd/link` records the
offset of one such call per function in `pclntab`, so a function with two of
them resumes at the wrong one after a panic. Every `return` in a function that
defers becomes a `goto` to that label.

### Capture is by reference, and this spec did not decide it

The section "Capture by value or by reference" below hands the choice to
[023](023-escape-analysis.md). There is no escape analysis, and the IR builder
does not wait for one: it makes **every** capture a capture by reference. One
`ir.Object` is shared by the enclosing function and the literal, and the
builder sets `Addrtaken` on it unconditionally. That is correct and it is slow,
and it is what a closure of a variable nobody assigns costs today.

Everything past the sections above is a design and not a description.

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

At a call, the closure object's address is in the context register, R26 on
`arm64` ([030](030-abi.md)), and the function body loads captures relative to
it.

### Capture by value or by reference

A variable is captured **by value** when the closure does not assign to it and
neither does anything else after the closure is created. Otherwise it is captured
**by reference**, which means the variable is moved out of the frame into a heap
cell and both the enclosing function and the closure access it through a pointer.

The decision belongs with [023](023-escape-analysis.md), since a by-reference
capture is an escape. It is not taken there today. The IR builder captures
everything by reference, as the note at the top of this spec records.

### Where the closure object lives

On the stack when the closure does not outlive the frame, on the heap when it
does.

**Nothing puts one in the frame today**, and the reason is two unbuilt things
rather than an oversight. Deciding that a closure does not outlive its frame is
[023](023-escape-analysis.md)'s judgement, and there is no escape analysis. And
a captureless `funcval` is one word of read-only data, which wants a data
symbol, and the only channel to a data symbol runs through
[032](032-type-descriptors-and-itabs.md)'s descriptors. So every func value
`ir/lower.go` builds is a heap allocation and a store, where `gc` has neither.
That is the cost, it is correct, and the fix is a channel for data symbols
rather than anything in this spec.

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

`runtime.deferproc`, allocating. This spec presented it as the case for a
`defer` in a loop, where the number of records is not known. That reads as a
restriction and it is not one: `deferproc` is correct for every `defer`, and
the other two implementations are optimizations of it. nanogo emits it for
every case, which is why `defer` works before either optimization exists.

### `deferreturn`

A function containing any non-open-coded `defer` ends with a call to
`runtime.deferreturn`, which runs the chain. Open-coded defers do not need it on
the normal return path, only on the panic path.

## Panic and recover

`panic` is `runtime.gopanic`. It walks the goroutine's defer chain, running each
deferred call in a frame it constructs, and either finds a `recover` or reaches
the bottom and terminates the program.

`recover` is `runtime.gorecover`, and it is **only effective when called
directly from a deferred function**.

This spec said the compiler's obligation is to pass the caller's frame pointer
so the runtime can check that relationship. It is not. `runtime.gorecover`
takes no argument. It walks the stack itself and counts the frames between its
own caller and `runtime.gopanic`, recovering only when there is exactly one.
The compiler's obligation is the opposite of passing something: it must not put
anything between the deferred function and the call, because one extra frame
turns a `recover` into a no-op. That is also why
[024](024-inlining-and-devirtualization.md) refuses to inline a function
containing `recover`: inlining changes which frame it is in, and would change
its meaning from "recovers" to "does not".

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
