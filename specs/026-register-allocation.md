---
title: "Register allocation"
status: draft
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
   that survives the call, so the allocator does not choose — it spills. The
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

Live intervals are computed from the liveness analysis of
[027](027-liveness-and-stackmaps.md), which runs first for this purpose and again
afterwards for stack maps, since spill slots are only known after allocation.

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

A value that is cheap to recompute — a constant, a frame address, a static
symbol address — is never spilled. It is recomputed at each use. This removes
most spill traffic in practice at the cost of one predicate over operations.

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

**The lost copy problem.** A critical edge — one from a block with several
successors to a block with several predecessors — has no block to put the copies
in. Every critical edge is split by inserting an empty block before allocation.
This is done once, as a pass, and it is why the pass list of
[022](022-optimization-passes.md) can treat edges as ordinary.

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

## Testing

- The verifier extended with allocation invariants: no value in a clobbered
  register across a call, no two live values in one register, every phi resolved.
- A stress corpus of functions with more live values than registers, forcing
  spills, checked by differential execution ([004](004-conformance.md) L3).
- Parallel copy tests over hand-built permutations including cycles of length 2
  and 3.
- Spill-slot reuse checked for correctness rather than for size: two values with
  disjoint intervals may share a slot only if neither is a pointer live at a
  safepoint in the other's interval, which is [027](027-liveness-and-stackmaps.md)'s
  constraint.
