---
title: "Register allocation"
status: complete
layer: middle end
gate: G1
depends_on:
  - 025-lowering-and-rules.md
  - 030-abi.md
---

# Register allocation

Assign each SSA value a machine register, or a stack slot when no register is
available. Runs after lowering ([025](025-lowering-and-rules.md)), on the order
of values each block already holds. [022](022-optimization-passes.md)'s
`schedule` pass is what would choose that order for register pressure, and it
is not built, so the order is the one construction and lowering left.

## The simplification Go's ABI provides

Go's internal ABI has **no callee-saved registers**. Every register is clobbered
by every call. The ABI document states the reason directly: it "significantly
simplifies the garbage collector and the compiler's register allocator, but at
some performance cost."

Two consequences, and both are large for nanogo:

1. **A value live across a call must be in a stack slot.** There is no register
   that survives the call, so the allocator does not choose: it spills. The
   allocation problem is therefore bounded by call sites rather than by the whole
   function.
2. **The garbage collector never needs a register map.** Every live pointer at a
   safepoint is in memory, described by the stack map of
   [027](027-liveness-and-stackmaps.md). This is why [027](027-liveness-and-stackmaps.md)
   is a spec about frames and not about registers.

A compiler that fought this would produce faster code and would need both a
register map format and the collector support to read it. nanogo takes the
simplification.

## Algorithm

**Linear scan over a reverse postorder linearisation of the blocks.**

Live intervals are computed by a liveness analysis of this pass, over SSA
values. It is not [027](027-liveness-and-stackmaps.md)'s analysis, and **What
was wrong** below says why the two cannot be one.

For each value in order:

1. Expire intervals that ended.
2. If a register of the right class is free, take it.
3. Otherwise spill the interval with the furthest next use, take its register,
   and give the spilled value a stack slot.

Graph colouring would produce better allocations and needs an interference graph,
a coalescing phase, and a spill-cost model. Linear scan is the version of this
pass a reader can hold in their head, which is what
[002](002-architecture.md) arranges the pipeline for.

### Rematerialisation

A value that is cheap to recompute, a constant, a frame address or a static
symbol address, is never spilled. It is recomputed at each use. This removes
most spill traffic in practice at the cost of one predicate over operations.

That predicate reads the op table, which means **a target's lowering rules have
to mark their constants as constants** or rematerialisation quietly stops
applying to that target. Nothing fails when they do not: the code merely gets
worse, which is the kind of regression nobody notices. The target description's
tests assert that at least one machine constant and one address form are
rematerialisable, so the omission is caught rather than measured later.

### Register classes

Integer and floating point are separate classes with separate free lists. A value
belongs to a class by its type. There is no case in Go where a value can go in
either.

## Phi resolution

A phi is not an instruction. It means "the value arriving on this edge". After
allocation, each phi becomes copies placed at the end of each predecessor block.

Two classical problems must be handled and both appear in ordinary Go code.

**The swap problem.** Two phis in one block exchange registers:

```
b3: v1 = Phi(a from b1, b from b2)
    v2 = Phi(b from b1, a from b2)
```

Emitting the copies in sequence overwrites one source with the other. The copies
on an edge form a permutation and must be executed as a **parallel copy**:
decompose into cycles, and break each cycle with one temporary register or with
a three-instruction exchange.

A note on when this fires, because it is easy to conclude the code is dead. Under
the hole-free range model of the spill-slot section below, a phi's range covers
every predecessor's block end and every phi argument is live at that same
point, so a phi's home is never an argument's home on that edge: sources and
destinations are disjoint and no cycle forms. The parallel copy is implemented
and tested directly all the same, because it becomes reachable the moment
coalescing or hole-aware ranges land, and because a permutation bug found then
would be found in the wrong place.

**The lost copy problem.** A critical edge, one from a block with several
successors to a block with several predecessors, has no block to put the copies
in. `ssa.SplitCriticalEdges` removes every one by inserting an empty block, and
the driver runs it immediately before allocation. `Allocate` does not repair an
edge it is handed: `checkEdges` refuses a function whose control-flow graph
still holds one, so the precondition is checked and not assumed.

### Reserved registers, the control value, and a missing move

**A spilled value needs a register to be reloaded into**, and it cannot be one
the allocator handed out. The target reserves, per class, **as many scratch
registers as the widest instruction of that class reads operands**, plus one on
a two-address machine for a spilled result. On `arm64` that is three integer
registers and two floating-point ones.

The count is a property of the machine and nothing else, which it is only
because two kinds of value draw no scratch register at all. A phi has one
operand per predecessor and is not an instruction: it becomes a move on each
edge and the code generator emits nothing for it. A call has one operand per
argument and a return one per result, and [030](030-abi.md) places every one of
them: an operand in a register is named by the convention, and an operand in
the argument area is written there by a store of its own. Drawing a scratch
register for either would make the demand grow with the program, and then no
fixed reservation could serve it.

R16 and R17 are free. They were never allocatable, because [030](030-abi.md)
reserves them for linker trampolines. R25 is not free: the third integer
register comes out of the allocatable set, which drops from 23 registers to 22.
The floating-point pair is not free either. There is no reserved float
register, so the target takes F30 and F31 from the top of the file and the
allocator loses two.

**A block's control value is a use.** A pass that leaves it out of the use set
collapses a branch condition's live range to nothing and lets two live
conditions share one register.

**A copy from a slot to a slot is a load and a store.** `resolvePhis` emits one
whenever a phi and one of its operands both live in the frame, which is two
values live across a call in a loop. `arm64` has no memory-to-memory move, so
[042](042-arm64-backend.md)'s move table stages it through `Scratch[class][1]`,
the same register the rematerialising arm uses.

The index is the whole of the correctness argument. `resolvePhis` breaks a
cycle of edge copies with `Scratch[class][0]`, so a cycle of two slots becomes

```text
R16 <- s1
s1  <- s0
s0  <- R16
```

and R16 holds a value of the function across the slot-to-slot copy in the
middle. Staging that copy through R16 as well would write one slot into both,
which is a wrong answer with the right instruction count and no failure. The
two registers are therefore taken from opposite ends of the reservation, and
`TestSlotToSlotMoveDoesNotDestroyThePhiCycleTemporary` asserts the pair rather
than describing it.

Until the arm existed the copy failed the function with `no move from sN to
sM`, which capped a loop at about one value live across a call and refused
`test/divmod.go` and `test/stackobj2.go` of Go's own corpus.

## Constraints from the ABI

[030](030-abi.md) fixes the locations of arguments and results, so some values
are pre-coloured:

- On `arm64`, integer arguments and results are in R0–R15 and floating-point in
  F0–F15. R28 holds the current goroutine, R26 the closure context, R27 and R16
  and R17 are scratch used by the assembler and linker, R25 is the allocator's
  third materialisation register, R18 is reserved by the operating system on
  Darwin and must never be touched.
- Every register is clobbered at a call, as above.

The reserved set is not advice. R18 on Darwin is used by the platform, and a
compiler that allocates it produces a binary that fails in ways that have nothing
to do with the program.

## Two-address forms

`amd64` instructions overwrite one source operand. `arm64` does not. The
allocator copies the first source into the destination when the two differ, and
records each such copy in `Alloc.Fixups`.

The fix-up is built and it produces nothing today. It is driven by the target's
`TwoAddress` predicate, which the `arm64` description answers false for every
value, so the list is always empty and no emitter reads it. This is one of the
two places where a target property reaches above
[025](025-lowering-and-rules.md); the other is register class, which is
universal. [043](043-amd64-backend.md) is the target that turns the predicate
on and it is unbuilt, so what covers the fix-up is the allocator's own tests: a
target description with the predicate set, one instruction whose destination
differs from its first source, and the reloaded operand the copy must not
destroy.

## Spill slot reuse

Two values may share a slot only when sharing cannot be observed. The rule is
**disjoint live ranges, identical size, identical alignment, and an identical
pointer map**.

Disjointness alone is not sufficient. The reason is not the collector; it is
stack copying.
[035](035-goroutines-and-stack-growth.md) grows a stack by copying it and
**rewriting every word the frame's pointer map calls a pointer**. The map is
per frame, not per instant, and [027](027-liveness-and-stackmaps.md)'s liveness
is a may-analysis: a slot is described as holding a pointer if it holds one on
any path. So a slot shared by a pointer and an integer is a slot the copier will
adjust while it holds the integer, and the integer changes value for reasons the
program cannot see.

This is why the rule is about the slot's description rather than about which
value is live. Disjointness answers "can both be read"; the pointer map answers
"will something else write here".

One clause of the rule rejects nothing today. Neither value may be a pointer
live at a safepoint in the other's interval, and live ranges here are hole-free
intervals from definition to last use, so disjointness already implies it. It
is implemented anyway, computed independently from the safepoint live sets,
because it stops being redundant the moment ranges gain holes, and a rule that
is required later should not be absent now.

## Testing

- The verifier extended with allocation invariants: no value in a clobbered
  register across a call, no two live values in one register, every phi resolved.
- A stress corpus of functions with more live values than registers, forcing
  spills, checked by that verifier. The corpus is built by hand out of SSA and
  is not a Go program, so it cannot be run against `gc`: the verifier is the
  oracle here, not differential execution ([004](004-conformance.md) L3).
- Parallel copy tests over hand-built permutations including cycles of length 2
  and 3.
- Spill-slot reuse checked for correctness rather than for size. The rule is in
  the section above.
- The distribution corpus, which is [027](027-liveness-and-stackmaps.md)'s and
  runs this pass on the way through. Of the 17,809 functions that reach SSA
  construction and lower completely, the allocator places all but 51. Those
  refusals are why 17,758 functions carry a stack map and not 17,809.

## What was wrong

Four claims of this spec were wrong and the code is what showed each one.

**The spec said one liveness analysis serves both this pass and
[027](027-liveness-and-stackmaps.md), run twice. The code runs two different
analyses over two different domains.** This pass computes liveness over SSA
values, before it knows what has a slot. [027](027-liveness-and-stackmaps.md)
computes liveness over frame slots and frame objects, from this pass's result,
so it cannot run first. The two also disagree about what they track: this pass
drops a rematerialised value, and a frame address is rematerialisable. It was
found when the stack map pass needed a safepoint set and the allocator's result
carried none.

**The spec said one scratch register per class. The number is the widest
instruction's operand count, which is three on `arm64`.** One instruction can
read two operands that are both in slots, so one scratch register lets the
second reload destroy the first. It was found by an instruction with two
spilled operands, and then again by `var a [4]int; a[2] = 7`, whose indexed
store reads three.

**Raising the number was the wrong fix twice before it was the right one.** The
demand was computed over every operand of every value, and a phi and a call
have as many operands as the program wrote arms and arguments. A merge of three
arms and a call of twenty integers each asked for more than the machine
reserves, and reserving one more would have moved the refusal to four arms and
twenty-one arguments. Both were fixed by saying that no register is read for
those operands, which is what the code generator already did. Only then does a
fixed count bound anything, and
`TestArm64ScratchCoversTheOperationTable` holds it against the operation
table.

**The spec's list of what constitutes a use left out a block's control
value.** Two live branch conditions were then given one register.
`TestABlockControlValueIsAUse` pins the answer.

**The spec's spill-slot rule was disjoint live ranges and the pointer clause,
and nothing about the shape of the slot.** Size, alignment and pointer map have
to match as well, for the stack-copying reason in the section above, and the
pointer clause the spec did state rejects nothing that disjointness has not
already rejected. It was found by implementing the rule as written.
