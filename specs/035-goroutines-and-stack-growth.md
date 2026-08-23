---
title: "Goroutines, stack growth, and preemption"
status: draft
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

Three requirements follow:

1. **The check must precede any use of the frame.** Nothing may be written below
   SP before the guard is verified.
2. **The morestack tail must save and restore the argument registers.** The
   function will be re-executed, so its arguments must be intact.
   [030](030-abi.md)'s argument spill space exists for this.
3. **The prologue and the tail are unsafe points.** The frame is not established.
   `PCDATA $PCDATA_UnsafePoint` marks them, per
   [027](027-liveness-and-stackmaps.md).

A leaf function with a small frame skips the check entirely, since it cannot
overflow the guard region. The threshold is the same one `gc` uses, and it is
part of the nosplit budget below.

## The nosplit budget

`//go:nosplit` means no stack-growth check. The function must fit in the space
reserved below the guard, and so must everything it calls, transitively, until a
splittable function is reached.

For a chain of nosplit functions $f_1 \to f_2 \to \dots \to f_n$ with frame sizes
$s_i$, the requirement is

$$
\sum_{i=1}^{n} (s_i + \mathit{callOverhead}) \;\le\; \mathit{StackNosplit}
$$

where `StackNosplit` is the runtime's reserved region — 800 bytes on 64-bit
platforms in recent releases, and read from the runtime rather than hard-coded.

The compiler computes this over the call graph and **rejects** an overflow at
compile time. The runtime failure mode is a stack overflow inside code that
cannot grow the stack, which manifests as memory corruption in the scheduler.

This is a G3 requirement, like the write barrier checks of
[034](034-write-barriers.md), and for the same reason: only the runtime uses the
directive.

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
  preempt where the frame is inconsistent.
- Ensure that a loop without a call contains a preemptible point. A loop with no
  calls and no preemptible instruction is a goroutine that cannot be stopped;
  asynchronous preemption is what removed the need for the compiler to insert
  explicit checks, and the remaining obligation is only not to mark such a loop
  unsafe throughout.

Frames stopped by asynchronous preemption are scanned **conservatively** by the
collector, since there is no precise map for an arbitrary instruction. That is
the runtime's mechanism, not the compiler's, and it is the reason unsafe-point
marking is a small obligation rather than a per-instruction map.

## `go` statements

`go f(a, b)` becomes `runtime.newproc` with a closure holding the arguments. The
arguments are evaluated in the current goroutine, at the `go` statement, and
copied into the new goroutine's initial frame. Getting the evaluation point wrong
is a visible semantic bug, not a performance one.

## Testing

- Deep recursion with large frames and pointers live across the growth, forcing
  many `morestack` calls at every frame size class. This is the primary test for
  [027](027-liveness-and-stackmaps.md) as well.
- A recursion whose frames contain address-taken locals, which exercises the
  stack objects table under copying.
- `GODEBUG=asyncpreemptoff=0` with tight loops, asserting they are preempted.
- The nosplit budget: a chain that fits, a chain that does not, and a check that
  the second is rejected with a message naming the chain.
