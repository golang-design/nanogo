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
available. Runs after lowering ([025](025-lowering-and-rules.md)) and after
scheduling has fixed the order of values within each block.

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
values. It is not [027](027-liveness-and-stackmaps.md)'s analysis. See the
report below.

For each value in order:

1. Expire intervals that ended.
2. If a register of the right class is free, take it.
3. Otherwise spill the interval with the furthest next use, take its register,
   and give the spilled value a stack slot.

Graph colouring would produce better allocations and needs an interference graph,
a coalescing phase, and a spill-cost model. Under
[000](000-decisions.md) decision 10, linear scan is the version nanogo can afford
to have and to read.

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
point, so a phi's home is never
an argument's home on that edge: sources and destinations are disjoint and no
cycle forms. The parallel copy is still implemented and tested directly, because
it becomes reachable the moment coalescing or hole-aware ranges land, and
because a permutation bug found then would be found in the wrong place.

**The lost copy problem.** A critical edge, one from a block with several
successors to a block with several predecessors, has no block to put the copies
in. Every critical edge is split by inserting an empty block before allocation.
This is done once, as a pass, and it is why the pass list of
[022](022-optimization-passes.md) can treat edges as ordinary.

### Reload registers, and the control value

Two things this spec did not say, both found by building it.

**A spilled value needs a register to be reloaded into**, and it cannot be one
the allocator handed out. The target reserves **two** scratch registers per
class, because one instruction can read two spilled operands. On `arm64` the
integer pair is free: R16 and R17 were never allocatable, because
[030](030-abi.md) reserves them for linker trampolines. The floating-point pair
is not free. There is no reserved float register, so the target takes F30 and
F31 from the top of the file and the allocator loses two.

**A block's control value is a use.** Omitting it collapses a branch
condition's live range to nothing and lets two live conditions share one
register. The spec's list of what constitutes a use did not mention it.

## Constraints from the ABI

[030](030-abi.md) fixes the locations of arguments and results, so some values
are pre-coloured:

- On `arm64`, integer arguments and results are in R0–R15 and floating-point in
  F0–F15. R28 holds the current goroutine, R26 the closure context, R27 and R16
  and R17 are scratch used by the assembler and linker, R18 is reserved by the
  operating system on Darwin and must never be touched.
- Every register is clobbered at a call, as above.

The reserved set is not advice. R18 on Darwin is used by the platform, and a
compiler that allocates it produces a binary that fails in ways that have nothing
to do with the program.

## Two-address forms

`amd64` instructions overwrite one source operand. `arm64` does not. The
allocator therefore needs, for the second target, a fix-up pass that inserts a
copy when the destination differs from the first source.

This is confined to one pass, driven by a per-operation flag from
[043](043-amd64-backend.md), and it is one of the two places where a target
property reaches above [025](025-lowering-and-rules.md). The other is register
class, which is universal.

## Spill slot reuse

Two values may share a slot only when sharing cannot be observed. The rule this
spec first gave was: disjoint live ranges, and neither value a pointer live at a
safepoint in the other's interval. Implementing it showed that rule is both
incomplete and, as stated, redundant.

**Redundant, under the range model actually used.** Live ranges here are
hole-free intervals from definition to last use. Under that model disjointness
already implies the pointer clause, so the clause never rejects a sharing that
disjointness accepted. It is implemented anyway, computed independently from the
safepoint live sets, because it stops being redundant the moment ranges gain
holes, and a rule that is load-bearing later should not be absent now.

**Incomplete, in a way that matters more.** Disjointness is not sufficient.
Two values may share a slot only when they also have **identical size,
alignment and pointer map**.

The reason is not the collector; it is stack copying.
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

## The report

Two claims of this spec were wrong and the code is what showed it.

**The spec said one liveness analysis serves both this pass and
[027](027-liveness-and-stackmaps.md), run twice. The code runs two different
analyses over two different domains.** This pass computes liveness over SSA
values, before it knows what has a slot. [027](027-liveness-and-stackmaps.md)
computes liveness over frame slots and frame objects, from this pass's result,
so it cannot run first. The two also disagree about what they track: this pass
drops a rematerialised value, and a frame address is rematerialisable. It was
found when the stack map pass needed a safepoint set and the allocator's result
carried none.

**The spec said one scratch register per class. Two are needed.** One
instruction can read two operands that are both in slots, so one scratch
register lets the second reload destroy the first. It was found by an
instruction with two spilled operands.

## Testing

- The verifier extended with allocation invariants: no value in a clobbered
  register across a call, no two live values in one register, every phi resolved.
- A stress corpus of functions with more live values than registers, forcing
  spills, checked by that verifier. Differential execution
  ([004](004-conformance.md) L3) is what this spec first named for it, and the
  verifier is what it has: the pressure corpus is built by hand and is not a Go
  program that can be run against `gc`.
- Parallel copy tests over hand-built permutations including cycles of length 2
  and 3.
- Spill-slot reuse checked for correctness rather than for size. The rule is in
  the section above.
- The distribution corpus, which is [027](027-liveness-and-stackmaps.md)'s and
  runs this pass on the way through. Of the 17,809 functions that reach SSA
  construction and lower completely, the allocator places all but 51. Those
  refusals are why 17,758 functions carry a stack map and not 17,809.
