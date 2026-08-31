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

The `//go:nosplit` half is built. A function that carries the directive gets no
stack-growth check and no growth tail at any frame size, its text symbol carries
`SymFlagNoSplit`, and its whole body is one unsafe point. `cmd/link` computes the
budget over the call graph and fails the link when a chain does not fit. See
"The nosplit budget" below, which this work corrected: the budget was never the
compiler's to compute.

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
`internal/abi.StackNosplitBase` times the guard multiplier. The multiplier is
not one on every configuration, so the budget is not a single number a compiler
may carry, and neither constant is in nanogo's source. That is correct rather
than missing, for the reason below.

**`cmd/link` computes this, not the compiler.** The sum lives in
`cmd/link/internal/ld/stackcheck.go`, which walks the call graph over every
symbol that carries `SymFlagNoSplit` and reports `nosplit stack over N byte
limit` with the chain printed under it. `gc`'s compiler computes nothing of the
kind: `grep -rn "nosplit stack" $GOROOT/src/cmd` finds `stackcheck.go` and its
test and nothing else. The limit is `objabi.StackNosplit(race)`, which is
`abi.StackNosplitBase` times `stackGuardMultiplier(race)`, less the size of a
call, and the linker is where the multiplier is known.

The runtime failure mode of getting it wrong is a stack overflow inside code
that cannot grow the stack, which manifests as memory corruption in the
scheduler.

### What `nosplit` means in the code

Three things change when a declaration carries the directive, and each has its
own failure if it is missed.

**No check and no tail, at any frame size.** `driver.NoSplit` reads the
directive off `ir.Func.Pragma` and `ssagen.Options.NoSplit` carries it to
`frame.skipsCheck`, which is the one place the two reasons for having no check
meet: the emitter's own proof that the frame cannot overflow the guard region,
and the author's requirement that no check be emitted whatever the frame is.
`prologue` then pushes the frame with no guard load and `growstack` emits
nothing. The instructions are compared against `go tool asm` at four frame
sizes, the ones that would each pick a different form of the check.

**`SymFlagNoSplit` on the text symbol.** This is what enrols the function in
the linker's budget, so setting it is not a claim the compiler has to prove: it
is what asks for the proof. Not setting it on a function that emits no check is
the state that hides an overflow. It follows the directive and not
`frame.nosplit`, because a leaf with no frame emits no check either and `gc`
marks such a function `LEAF|NOFRAME` and not `NOSPLIT`.

**The whole body is one unsafe point.** `gc`'s `liveness.IsUnsafe` is
`CompilingRuntime || f.NoSplit`, and it sets `allUnsafe`, so every value and
every block is marked. `go tool compile -S` shows it: a `NOSPLIT` function's
`PCDATA $0` stream goes to `-2` at the first instruction of the body and never
returns to `-1`, while the same function without the directive returns to `-1`
once its frame is pushed. nanogo marks the whole symbol, which is wider than
`gc`'s and is the conservative direction.

This removes the asynchronous half of the safe points and only that half.
`liveness.hasStackMap` does not read `allUnsafe`, so `gc` still writes a stack
map at every call and so does nanogo. A frame stopped by asynchronous
preemption is scanned conservatively, and this range says the runtime may not
stop there at all. A change that removed the collector's maps instead would be
a collector fault rather than a missed preemption, so the two streams are
asserted separately and the second is measured rather than argued:
`e2e.TestNoSplitKeepsTheStackMapAtACall` runs a `//go:nosplit` function that
allocates an object, holds the only reference to it in its own frame across two
collections and then reads it, under `GODEBUG=gccheckmark=1,clobberfree=1` with
`GOGC=1`. With the stack map removed for a nosplit function and nothing else
changed, that program dies on the read.

### It is not the runtime that made this urgent

This spec said the directive was a G3 requirement, like the write barrier
checks of [034](034-write-barriers.md), because only the runtime uses it. That
is wrong about who is exposed. A user's own function carrying `//go:nosplit`
reaches this compiler, and it used to come out with a call to
`runtime.morestack_noctxt` in it. `internal/e2e` builds such a program, links
it with the real linker and reads the linked instructions back. On the tree
before this work the disassembly held the call, and the over-budget program
linked and ran instead of being refused.

Neither `internal/abi` nor `internal/chacha8rand` is unblocked by it. Both are
refused earlier, by `checkAssembly`, and both would still be unusable if that
gate were relaxed. See "What was wrong" below.

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
  preempt where the frame is inconsistent. This is built. Four kinds of range
  are marked and the stream is written into the object file: the growth tail,
  each frame teardown, each write of a pointer-holding location that takes more
  than one store, which `ssa.HalfWrittenPointer` names, and the whole body of a
  `//go:nosplit` function. The third is a string, a slice or an interface, which
  holds one word of the new value and the rest of the old one between the
  stores. The prologue is not among them, for the reason in requirement 3.
- Ensure that a loop without a call contains a preemptible point. A loop with no
  calls and no preemptible instruction is a goroutine that cannot be stopped;
  asynchronous preemption is what removed the need for the compiler to insert
  explicit checks, and the remaining obligation is only not to mark such a loop
  unsafe throughout. Nothing checks this. Of the four kinds above, three cannot
  span a loop: the teardown and the growth tail are outside any loop body, and a
  half-written pointer range is the two or three stores of one assignment, so a
  loop that contains one still has preemptible instructions between them. The
  fourth does span every loop in its function, deliberately, and `gc` does the
  same for the same functions. A `//go:nosplit` function that spins is a
  goroutine the runtime cannot stop, which is why the directive is written on
  short functions and why the linker's budget bounds them.

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

Two of those four exist. `ssagen` recurses 200,000 frames under
`gccheckmark`, at one frame size, carrying an integer rather than a pointer.
The other two are the stack objects table, which is blocked, and preemption,
which is only unwritten. Each of the four:

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
- The nosplit test exists now, in both halves.
  `TestNoSplitLinksAndRunsWithNoGrowthCheck` builds a `//go:nosplit` function
  with nanogo, links it with the real linker, runs it, compares the exit status
  against an all-`gc` build of the same source, and reads the linked
  instructions back with `go tool objdump` to check that no `morestack` call
  and no guard load survived.
  `TestNoSplitOverBudgetIsRefusedByTheLinker` gives the same function a
  1600-byte frame and asserts that the link fails with `nosplit stack over` and
  names the function. Only the second proves the symbol flag was written and
  read, because no compile-only assertion reaches the linker.
  `TestNoSplitKeepsTheStackMapAtACall` is the third, and it is the collector's
  rather than the scheduler's.

Three more tests hold the prologue in place. `ssagen` compares 101 emitted
prologue and tail instructions against `go tool asm` on the same function, so a
wrong encoding is a diff and not a crash, and 32 more for the `//go:nosplit`
forms at the four frame sizes that would each pick a different check. And it
reads `StackSmall` and `StackBig` out of `internal/abi/stack.go` and fails when
they move, which is what "read from the runtime rather than hard-coded" has to
mean for a constant that is written into the instruction stream.

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
- It said "the compiler computes this over the call graph and rejects an
  overflow at compile time". No Go compiler does. `cmd/link` does, in
  `stackcheck.go`, and `gc`'s compiler has no budget code at all.
- It said `SymFlagNoSplit` was correctly withheld because "claiming the
  property without checking it is exactly what this spec says must be
  rejected". The premise was the sentence above. Setting the flag does not
  claim a property nanogo failed to check; it is what asks the linker to check
  it. Withholding it on a function that emits no check is the state that hides
  an overflow, and that is what the code did.
- It said `//go:nosplit` was a G3 requirement because only the runtime uses the
  directive. A user's own function reaches this compiler with it and came out
  with a call to `runtime.morestack_noctxt` in it, which is a crash in the
  scheduler.
- It said no unsafe range nanogo marks spans a loop. One does now, and `gc`
  marks the same one.

### Two packages `//go:nosplit` does not unblock

`internal/abi` and `internal/chacha8rand` hold every `//go:nosplit` function in
the bootstrap closure of [060](060-selfhost.md). There are three:
`internal/abi.IntArgRegBitmap.Get`, `internal/abi.NoEscape` and
`internal/chacha8rand.State.Next`. The other twenty-six packages of the closure
hold none, so honouring the directive changes no object nanogo already writes.
Neither of the two compiles, and the directive is not why. Both are refused by `checkAssembly` before any function is looked
at, because the go command sends `-symabis` and `-asmhdr` for a package with a
`.s` file in it. The closure reading is 20 of 28 before this work and 20 of 28
after it.

Relaxing that gate would not help either, which was measured rather than
reasoned. With the gate bypassed the closure reads 25 of 28, and then the build
fails in `gc`:

```
runtime/rand.go:61:23: chacha8rand.seed escapes to heap, not allowed in runtime
```

The reason is in `export/writer.go`: nanogo writes one empty escape note per
receiver and parameter, and `gc` reads an empty note as "leaks to the heap".
The `runtime` package forbids a heap escape, and `runtime` imports both of
these packages, so an object nanogo produced for either one is refused by the
compiler that builds the runtime against it. The blocker is
[023](023-escape-analysis.md), which is unbuilt, and it stands whatever
`checkAssembly` does.

Two claims in `checkAssembly`'s own message are also wrong for these two
packages on `arm64`, and the message is left alone because it fires on the
presence of the flags rather than on either claim. Neither package needs an ABI
wrapper: `internal/abi`'s assembly defines `FuncPCTestFn` at ABI0 and the Go
declaration for it is in `export_test.go`, which is not in the non-test build,
while `internal/chacha8rand`'s `block` is defined in assembly at
**ABIInternal** already. And neither package's `.s` files include `go_asm.h`, so
neither needs the `-asmhdr` header.

## Three symptoms, one with a cause and two without

The first two appeared on 27 August 2026 and the third on 29 August. All three
appear only under load and none reproduces on demand. They are written down because "it passed on retry" is not an
explanation, and because a collector or a stack bug that appears once in twenty
runs is exactly the shape that a green suite hides.

**A closure's captures rejected by the collector. The cause is identified and
the entry stays open.** `internal/e2e`'s `TestToolexecKeepsCapturesThroughACollection` runs
a program whose only reference to a heap object is a capture, under
`GODEBUG=gccheckmark=1,clobberfree=1` with `GOGC=1`. It failed twice: once
locally with a checkmark error, and once on CI's `macos-latest` runner.

A checkmark error is the collector reporting an object that was reachable and
was not marked, which is what a store the collector did not observe produces
and is the same measurement [034](034-write-barriers.md) records against Go's
own `test/gcgort.go`. The object it named was a capture of a two-capture
closure whose descriptor and pointer mask were both correct, and a mask that is
right while the object is still lost is the store and not the description. The
barrier is built and removes that store, which is a cause and not a proof. This
entry stays open until a run under load confirms it, because counting passes
cannot: the test passed fifteen consecutive runs before the barrier existed and
twelve after it, and neither number distinguishes a fixed defect from a
sleeping one.

**A stack span far larger than the limit. This one is open.**
A corpus run reported `MISCOMPILATION in peano.go`, a stack overflow, printing
`stack=[0x6ac20b60000, 0x6ac40b60000]`. That span is 8.6 GB under a 1 GB
limit, so `g.stack` held something that is not a stack rather than the
recursion running deep. The lane that saw it disassembled all nine of the
file's functions at two commits and found them byte-identical, and the program
runs clean standalone eight times out of eight.

**A free object the collector had marked.** `test/heapsampling.go` ran clean on
one CI run and, on the next, with no compiler change between them, died in
nanogo's build with

	runtime: marked free object in span 0x12fd09228, elemsize=1024 freeindex=3
	(bad use of unsafe.Pointer or having race conditions)
	0x42d866189000 free  marked   zombie

A zombie is an object the sweeper freed and something marked afterwards, and
the runtime's own message names the two things that produce one. It is not the
statistical part of that test: `heapsampling.go` randomises its sampling and
says so, but this is the collector refusing to continue rather than a
measurement outside a threshold.

What rules out the obvious suspect is the pair of runs. `18992b5` and
`fa22198` differ by one number in a JSON file, the corpus passed on the first
and this failed on the second, so no commit introduced it. The write barrier
was in both and has been in every green run since `ab048d6`. Locally the same
binary ran the file 25 times clean, and 10 more under `GOGC=1` with
`gccheckmark=1,clobberfree=1`.

**A mechanism that produces all three, found later.** The write barrier read
the wrong word. For a pointer store past field 0 it shaded word 0 of the object
instead of the word the store overwrites, so the overwritten pointer was never
shaded and a word that need not be a pointer was. The first frees an object the
collector has still to reach through that slot, and the second hands the
collector a word to follow that is not a pointer.

Those are the two symptoms above, in that order: a marked free object, and a
bad pointer in the heap. `heapsampling.go` failed a third time on CI at
`a6647f6` with the second of them, which is what led to the barrier.

This is a mechanism and not a proof for these three runs. The fault needs the
collector to be marking concurrently, so it is rare by construction: the same
binary with the bug still in it ran this file clean three times in a row here,
which is why no local run ever caught it and why the pair of CI runs above
looked like it had no cause. What is established is that the barrier was wrong
in exactly the way that produces this signature, and that it is right now
(`ssa/writebarrier.go`, with the offsets read off two compiled binaries). If a
fourth occurrence appears after that fix, this paragraph is what to disbelieve
first.

That leaves a defect that samples with the machine, which is the shape of the
two symptoms above. Whether it is the same defect is unknown. The evidence to
capture next time is the span address and `elemsize` the message prints,
because those say which size class the zombie was in, and a `GODEBUG` run with
`clobberfree=1` on the failing machine, because a freed object that was
overwritten and then marked says the mark came after the free rather than
before it.

The next person to see any of these should capture the failing binary and its
`GODEBUG` output before retrying, because a retry is what destroyed the
evidence both times.

The write barrier does not explain this one. A missing barrier frees a
reachable object and does not put a value that is not a stack into `g.stack`.
The claim is also measured rather than reasoned: on the tree that emits the
barrier, `peano.go` passed 20 runs in 20 under `GOGC=1` and 20 in 20 again
under `GOGC=1` with `GODEBUG=gccheckmark=1`. That is the same kind of count
this section refuses to read as a fix, so it says only that the barrier did not
make this worse.
