---
title: "Goroutines, stack growth, and preemption"
status: in progress
layer: runtime interface
gate: G1
depends_on:
  - 030-abi.md
  - 027-liveness-and-stackmaps.md
  - 033-closures-defer-panic.md
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
`runtime.newproc` on a one-word func value, which is the word
[033](033-closures-defer-panic.md)'s `defer` passes to `runtime.deferproc`.
`internal/e2e` starts a goroutine whose body divides by zero and reads the
runtime's panic as the evidence that it ran, because nanogo compiles no `panic`
statement of its own: the operand's conversion to an interface is
[032](032-type-descriptors-and-itabs.md)'s gap.

`go f(a, b)` is refused, and so is `defer f(a)`, with the same message: an
argument becomes a capture, and a capture is read through the context register,
which no SSA operation reads ([033](033-closures-defer-panic.md)). `go t.m()`
is refused by the same rule, because the receiver is an argument.

The `//go:nosplit` half is not built either, and it is the part of this spec
most likely to be misread. See "The nosplit budget" below.

## The prologue

Every function that is not `//go:nosplit` and has a frame begins with a check:

```
MOVD  16(g), R16       // g.stackguard0
CMP   R16, RSP
BLS   morestack
```

`g` is R28 on `arm64` ([030](030-abi.md)), and the guard is at a fixed offset
in it, which `ssagen/prologue.go` spells as `2*8`. If the stack pointer is
below the guard, the function jumps to a tail that calls
`runtime.morestack_noctxt`, which grows the stack and re-executes the function
from the top.

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
underflowed comparison succeeds where it must fail.

Three requirements follow:

1. **The check must precede any use of the frame.** Nothing may be written below
   SP before the guard is verified.
2. **The morestack tail must save and restore the argument registers.** The
   function will be re-executed, so its arguments must be intact.
   [030](030-abi.md)'s argument spill space exists for this.
   It must also **pass the caller's return address in R3** on `arm64`, which
   `runtime.morestack` reads and `runtime/asm_arm64.s` documents at its entry
   point. The order matters: R3 is the fourth argument register, so the
   arguments are saved before it is overwritten.
3. **The growth tail is an unsafe point. The prologue is built not to be
   one.** `PCDATA $PCDATA_UnsafePoint` marks the tail, per
   [027](027-liveness-and-stackmaps.md). The prologue is not marked, and does
   not need to be: the small form pushes the link register and moves the stack
   pointer in one pre-indexed store, and the large form writes both saved words
   through a scratch register before the stack pointer moves, so no instruction
   boundary inside it shows a half-written frame.

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

where `StackNosplit` is the runtime's reserved region,
`internal/abi.StackNosplitBase` times `internal/runtime/sys.StackGuardMultiplier`.
Both are read from the runtime rather than hard-coded, and the multiplier is
not one on every configuration, so the budget is not a single number the
compiler may carry. Neither constant is in the source today, so neither has the
test `StackSmall` and `StackBig` have.

The compiler computes this over the call graph and **rejects** an overflow at
compile time. The runtime failure mode is a stack overflow inside code that
cannot grow the stack, which manifests as memory corruption in the scheduler.

This is a G3 requirement, like the write barrier checks of
[034](034-write-barriers.md), and for the same reason: only the runtime uses the
directive.

### What `nosplit` means in the code today

Read the paragraphs above as a plan. Three things are true of the code.

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
  preempt where the frame is inconsistent. This is built. Three kinds of range
  are marked and the stream is written into the object file: the growth tail,
  each frame teardown, and each write of a pointer-holding location that takes
  more than one store, which `ssa.HalfWrittenPointer` names. The third is a
  string, a slice or an interface, which holds one word of the new value and
  the rest of the old one between the stores. The prologue is not among them,
  for the reason in requirement 3.
- Ensure that a loop without a call contains a preemptible point. A loop with no
  calls and no preemptible instruction is a goroutine that cannot be stopped;
  asynchronous preemption is what removed the need for the compiler to insert
  explicit checks, and the remaining obligation is only not to mark such a loop
  unsafe throughout. Nothing checks this. It holds because no range nanogo
  marks spans a loop: the teardown and the growth tail are outside any loop
  body, and a half-written pointer range is the two or three stores of one
  assignment, so a loop that contains one still has preemptible instructions
  between them.

Frames stopped by asynchronous preemption are scanned **conservatively** by the
collector, since there is no precise map for an arbitrary instruction. That is
the runtime's mechanism, not the compiler's, and it is the reason unsafe-point
marking is a small obligation rather than a per-instruction map.

## `go` statements

`go f(a, b)` becomes `runtime.newproc` with a closure holding the arguments. The
arguments are evaluated in the current goroutine, at the `go` statement, and
copied into the new goroutine's initial frame. Getting the evaluation point wrong
is a visible semantic bug, not a performance one.

The argument half is not built, for the reason at the top of this spec, so the
evaluation point above is not yet a decision the code has taken: with no
arguments there is nothing to evaluate.

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
Of the other three, one is blocked and two are only unwritten:

- The stack objects test is blocked, though not where this spec said. The
  table is computed and never written: `ssa.StackMaps.ObjectsSym` builds one
  and `ssagen` does not call it, because the lookup from an `ir.Type` to its
  descriptor's `runtime.gcbits` symbol reference is missing.
  [027](027-liveness-and-stackmaps.md) owns that gap and records how many
  functions of the corpus have an object to write.
- The preemption test is not blocked. `go f()` runs, and a goroutine spinning
  in a loop with no call in it is preempted under `GOMAXPROCS=1`: the program
  finishes, and the same program hangs under `GODEBUG=asyncpreemptoff=1`. That
  contrast is the assertion the test needs, and the code it needs is built.
- The nosplit test waits on the budget, which nothing computes.

Two more tests hold the prologue in place. `ssagen` compares 101 emitted
prologue and tail instructions against `go tool asm` on the same function, so a
wrong encoding is a diff and not a crash. And it reads `StackSmall` and
`StackBig` out of `internal/abi/stack.go` and fails when they move, which is
what "read from the runtime rather than hard-coded" has to mean for a constant
that is written into the instruction stream.

## What was wrong

- This spec listed the marked unsafe ranges as the prologue, the growth tail
  and each teardown. The prologue is not marked, and a write of a
  pointer-holding location that takes more than one store is. That last kind is
  the only one that falls inside a loop body, which is what the claim about
  loops turned on.
- It said there was no goroutine to preempt. There is one, so the preemption
  test is unwritten rather than blocked.
- It said [032](032-type-descriptors-and-itabs.md) blocked the stack objects
  table. 032 writes the descriptor now; what is missing is the symbol
  reference lookup in `ssagen`.
- The three prologue forms and the move of the return address into R3 were not
  in this spec. Both came out of comparing the emitted instructions against
  `go tool asm` on functions with large frames, and
  [042](042-arm64-backend.md) records the move from the back end's side. The
  save order is stated above because the other order loses an argument.

## Two symptoms that are open, and are not flakes

Both appeared on 27 August 2026, both only under load, and neither reproduces
on demand. They are written down because "it passed on retry" is not an
explanation, and because a collector or a stack bug that appears once in
twenty runs is exactly the shape that a green suite hides.

**A closure's captures rejected by the collector.**
`internal/e2e`'s `TestToolexecKeepsCapturesThroughACollection` runs a program
whose only reference to a heap object is a capture, under
`GODEBUG=gccheckmark=1,clobberfree=1` with `GOGC=1`. It failed twice: once
locally with a checkmark error, and once on CI's `macos-latest` runner with
`the collector or the program rejected what the closure held: exit status 2`.
It has since passed fifteen consecutive runs here, including under six
artificial busy loops, and under `GOGC=1` and the same `GODEBUG` pair.

A checkmark error is the collector saying it found a pointer the map did not
describe, so the two candidates are a pointer map that is wrong on a path the
test reaches rarely, and a pointer map that is right against a stack that
moved under it.

**A stack span far larger than the limit.**
A corpus run reported `MISCOMPILATION in peano.go`, a stack overflow, printing
`stack=[0x6ac20b60000, 0x6ac40b60000]`. That span is 8.6 GB under a 1 GB
limit, so `g.stack` held something that is not a stack rather than the
recursion running deep. The lane that saw it disassembled all nine of the
file's functions at two commits and found them byte-identical, and the program
runs clean standalone eight times out of eight.

The two share a shape: a `g` whose stack bounds or whose frame contents are
not what the compiler described, seen only when the machine is busy. Whether
they are one defect is unknown. The next person to see either should capture
the failing binary and its `GODEBUG` output before retrying, because a retry
is what has destroyed the evidence both times.
