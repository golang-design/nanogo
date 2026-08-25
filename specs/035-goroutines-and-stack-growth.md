---
title: "Goroutines, stack growth, and preemption"
status: in progress
layer: runtime interface
gate: G1
depends_on:
  - 030-abi.md
  - 027-liveness-and-stackmaps.md
---

# Goroutines, stack growth, preemption

Go's stacks are small and grow. Growing one moves it, which moves every frame on
it, which means every pointer into it must be found and rewritten. The compiler
provides the prologue that triggers the growth and the metadata that makes the
move safe.

## What is built

The stack-growth half is built and runs. `ssagen/prologue.go` emits the guard
check, the frame push, the teardown and the growth tail, and the tail is
checked instruction by instruction against `go tool asm`. A test recurses
200,000 frames under `GODEBUG=gccheckmark=1` and reads the accumulator back, so
the frames really are copied and the maps of
[027](027-liveness-and-stackmaps.md) really are read by the copier.

The goroutine half is half built. `go f()` with no arguments lowers to
`runtime.newproc` on a one-word func value, and `internal/e2e` runs a goroutine
that reaches its own panic. `go f(a, b)` is refused: an argument becomes a
capture, and a capture is read through the context register, which no SSA
operation reads ([033](033-closures-defer-panic.md)).

The `//go:nosplit` half is not built either, and it is the part of this spec
most likely to be misread. See "The nosplit budget" below.

## The prologue

Every function that is not `//go:nosplit` and has a frame begins with a check:

```
MOVD  16(g), R16       // g.stackguard0
CMP   R16, RSP
BLS   morestack
```

R28 holds the current goroutine on `arm64` ([030](030-abi.md)), and the guard is
at a fixed offset in it. If the stack pointer is below the guard, the function
jumps to a tail that calls `runtime.morestack_noctxt`, which grows the stack and
re-executes the function from the top.

### The listing is one of three forms

The listing above is right for a small frame and wrong for a large one, and the
code emits three sequences chosen by the frame size against two constants of
`internal/abi/stack.go`, `StackSmall` (128) and `StackBig` (4096). Both are
read from the runtime by a test, which is what this spec asks for.

| Frame size | The comparison |
| --- | --- |
| up to `StackSmall` | the stack pointer against the guard, as listed |
| up to `StackBig` | the stack pointer the frame *will leave*, `SP - (size - StackSmall)`, against the guard |
| above `StackBig` | the same subtraction, with the flags set, and a borrow branches straight to the tail |

The reason for the second row is that the stack pointer alone under-tests the
guard once the frame is larger than the region the runtime guarantees below it.
The reason for the third is that `SP - framesize` can underflow, and an
underflowed comparison succeeds where it must fail. Neither was in this spec.
Both were found by comparing the emitted instructions against `go tool asm` on
functions with large frames.

Three requirements follow:

1. **The check must precede any use of the frame.** Nothing may be written below
   SP before the guard is verified.
2. **The morestack tail must save and restore the argument registers.** The
   function will be re-executed, so its arguments must be intact.
   [030](030-abi.md)'s argument spill space exists for this.
   It must also **pass the caller's return address in R3** on `arm64`, which
   `runtime.morestack` reads and `runtime/asm_arm64.s` documents at its entry
   point. The order matters: R3 is the fourth argument register, so the
   arguments are saved before it is overwritten. An earlier version of this
   spec and of [042](042-arm64-backend.md) omitted the move entirely.
3. **The prologue and the tail are unsafe points.** The frame is not established.
   `PCDATA $PCDATA_UnsafePoint` marks them, per
   [027](027-liveness-and-stackmaps.md).

A leaf function with a small frame skips the check entirely, since it cannot
overflow the guard region. The threshold is the same one `gc` uses: a frame of
zero bytes, or a leaf frame under `StackSmall`.

## The nosplit budget

`//go:nosplit` means no stack-growth check. The function must fit in the space
reserved below the guard, and so must everything it calls, transitively, until a
splittable function is reached.

For a chain of nosplit functions $f_1 \to f_2 \to \dots \to f_n$ with frame sizes
$s_i$, the requirement is

$$
\sum_{i=1}^{n} (s_i + \mathit{callOverhead}) \;\le\; \mathit{StackNosplit}
$$

where `StackNosplit` is the runtime's reserved region, 800 bytes on 64-bit
platforms in recent releases, and read from the runtime rather than hard-coded.

The compiler computes this over the call graph and **rejects** an overflow at
compile time. The runtime failure mode is a stack overflow inside code that
cannot grow the stack, which manifests as memory corruption in the scheduler.

This is a G3 requirement, like the write barrier checks of
[034](034-write-barriers.md), and for the same reason: only the runtime uses the
directive.

### What "nosplit" means in the code today, which is not this

Read the paragraphs above as a plan. Three things are true of the code and none
of them is what a reader would assume.

**The `//go:nosplit` directive is never read.** No pass looks at it. The
emitter has a field called `nosplit`, and it is computed from the shape of the
function: `size == 0 || (leaf && size < StackSmall)`. It means "this function
provably cannot overflow the guard region", which is the leaf rule of the
section above, and it does not mean "the author asked for no check".

**`SymFlagNoSplit` is deliberately never set**, even on a function that emits
no check. That flag is what makes `cmd/link` compute the budget over the call
graph, and nanogo does not compute the budget. Claiming the property without
checking it is exactly what this spec says must be rejected, so the emitter
declines to claim it. This is the one place where an unbuilt check is handled
correctly by refusing to assert anything.

**Nothing computes the sum.** There is no call graph traversal, no
`StackNosplit` constant anywhere in the source, and no rejection.

The gap was found by looking for the directive and finding a structural
predicate wearing its name.

## Stack copying

When a stack grows, the runtime allocates a larger one, copies the frames, and
**adjusts every pointer that points into the old stack**. To do that it walks the
frames using exactly the metadata of [027](027-liveness-and-stackmaps.md):

- the locals bitmap, to find pointer slots to adjust;
- the arguments bitmap, for the outgoing argument area;
- the stack objects table, for address-taken locals.

So the stack maps have a second consumer, and this one *writes*. A map that is
too small in the collector's use leaks memory; the same map in the copier's use
leaves a stale pointer to a freed stack. The second failure is immediate and
severe, which makes deep recursion a better test for stack maps than allocation
stress.

## Asynchronous preemption

The runtime preempts a goroutine that has run too long by delivering a signal and
redirecting it to `runtime.asyncPreempt`, which saves all registers and yields.
This can happen at almost any instruction.

The compiler's obligation is small but not zero:

- Mark unsafe ranges with `PCDATA $PCDATA_UnsafePoint`, so the runtime does not
  preempt where the frame is inconsistent. This is built. The ranges marked are
  the prologue, the growth tail, and each frame teardown, and the stream is
  written into the object file.
- Ensure that a loop without a call contains a preemptible point. A loop with no
  calls and no preemptible instruction is a goroutine that cannot be stopped;
  asynchronous preemption is what removed the need for the compiler to insert
  explicit checks, and the remaining obligation is only not to mark such a loop
  unsafe throughout. Nothing checks this. It holds by accident: the only unsafe
  ranges nanogo marks are the three above, and none of them is a loop body.

Frames stopped by asynchronous preemption are scanned **conservatively** by the
collector, since there is no precise map for an arbitrary instruction. That is
the runtime's mechanism, not the compiler's, and it is the reason unsafe-point
marking is a small obligation rather than a per-instruction map.

## `go` statements

`go f(a, b)` becomes `runtime.newproc` with a closure holding the arguments. The
arguments are evaluated in the current goroutine, at the `go` statement, and
copied into the new goroutine's initial frame. Getting the evaluation point wrong
is a visible semantic bug, not a performance one.

The argument half is not built. `go f()` with an empty argument list lowers
to `runtime.newproc`, which is the same one-word func value `defer` passes to
`runtime.deferproc`. An argument list is refused, because the arguments have to
be captured and the captured form is read through the context register that
[033](033-closures-defer-panic.md) owns. So the evaluation point above is not
yet a decision the code has taken: with no arguments there is nothing to
evaluate.

## Testing

- Deep recursion with large frames and pointers live across the growth, forcing
  many `morestack` calls at every frame size class. This is the primary test for
  [027](027-liveness-and-stackmaps.md) as well.
- A recursion whose frames contain address-taken locals, which exercises the
  stack objects table under copying.
- `GODEBUG=asyncpreemptoff=0` with tight loops, asserting they are preempted.
- The nosplit budget: a chain that fits, a chain that does not, and a check that
  the second is rejected with a message naming the chain.

Of those four, one exists. `ssagen` recurses 200,000 frames under
`gccheckmark`, at one frame size, carrying an integer rather than a pointer.
The other three wait on the code they would test: there is no stack objects
table ([032](032-type-descriptors-and-itabs.md) blocks it), no goroutine to
preempt, and no budget to reject a chain.

Two tests this spec did not name are the ones that hold the prologue in place.
`ssagen` compares 101 emitted prologue and tail instructions against
`go tool asm` on the same function, so a wrong encoding is a diff and not a
crash. And it reads `StackSmall` and `StackBig` out of `internal/abi/stack.go`
and fails when they move, which is what "read from the runtime rather than
hard-coded" has to mean for a constant that is written into the instruction
stream.
