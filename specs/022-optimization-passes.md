---
title: "Optimization passes: the minimum set, and their order"
status: draft
layer: middle end
gate: G1
depends_on:
  - 021-ssa-construction.md
---

# Optimization passes

The governing rule comes from [000](000-decisions.md) decision 10 and is stated
first because it decides every argument in this spec:

**No optimization may be required for correctness.** Deleting the whole pass list
must produce a compiler that is slower and still right. A pass that cannot be
deleted is not an optimization and belongs in [021](021-ssa-construction.md) or
[025](025-lowering-and-rules.md).

There is one honest exception and it arrives late. See *the escape analysis
exception* below.

## The list

Order is significant. A pass may be run twice where the second run cleans up
after a later one.

| # | Pass | Does | Why here |
| --- | --- | --- | --- |
| 1 | deadcode | Remove unreachable blocks and unused values | Every later pass is cheaper on a smaller graph |
| 2 | phielim, copyelim | Remove phis with one distinct argument, and copies | Construction produces these by the thousand |
| 3 | opt | Constant folding and algebraic identities, by rewrite rules | Exposes constants to everything below |
| 4 | zcse | Common subexpression elimination for constants | Cheap, and constants dominate the value count |
| 5 | cse | Common subexpression elimination for pure values | The main redundancy pass |
| 6 | nilcheckelim | Remove nil checks dominated by a proven-safe access | Removes a branch per dereference |
| 7 | prove | Derive value ranges; remove bounds checks it can discharge | The pass that pays for itself on array code |
| 8 | deadstore | Remove stores never read | Needs memory ordering, which [021](021-ssa-construction.md) provides |
| 9 | tighten | Move values to the block where they are used | Shortens live ranges before allocation |
| 10 | lower | Machine operation selection, [025](025-lowering-and-rules.md) | The target boundary |
| 11 | opt, cse, deadcode again | Clean up what lowering exposed | Lowering creates new redundancy |
| 12 | schedule | Order values within a block | Register allocation needs a linear order |
| 13 | regalloc | [026](026-register-allocation.md) | |
| 14 | stackframe, liveness | [027](027-liveness-and-stackmaps.md) | Must be after allocation, since spills are locations |

Passes 1 to 9 are target-neutral. Everything from 10 down is not. That line is
where [070](070-gpu-target.md) attaches, per [002](002-architecture.md).

## The passes that need explaining

### prove

The only pass with real analysis in it. It walks the dominator tree carrying a
set of facts of the form "value $v$ is in range $[lo, hi]$" and "value $v$
relates to value $w$ by $\le$, $<$, or $=$". A branch adds its condition as a
fact on the taken edge.

A bounds check `0 <= i < len(s)` is discharged when the facts imply it. For the
common loop

```go
for i := 0; i < len(s); i++ { s[i] = 0 }
```

the induction variable's lower bound comes from the initialiser and its upper
bound from the loop condition, and the check is removed.

nanogo implements the dominator-tree fact propagation and no more. Loop
induction analysis beyond the direct comparison, and the full integer relation
lattice, are explicitly not implemented. The budget in
[000](000-decisions.md) decision 10 buys the simple version of this pass and not
the complete one.

### deadstore

Requires knowing that a store's location is never loaded before being
overwritten. This is decidable in SSA only because memory is a value
([021](021-ssa-construction.md)) and stores to distinct stack slots are known to
be distinct. Stores through arbitrary pointers are never removed.

### tighten and schedule

These two are about live ranges rather than about work. A value computed in an
early block and used in a late one is live across everything between, and
[026](026-register-allocation.md) then either keeps a register busy or spills.
Moving the computation down is usually free.

`schedule` fixes the order of values within a block, subject to data and memory
dependence. It is the last pass that may reorder anything.

## The escape analysis exception

[023](023-escape-analysis.md) is listed as an optimization and is, until G3.

At G3 nanogo compiles the runtime, and the runtime contains functions marked
`//go:nowritebarrier` and `//go:nosplit` that assume their locals are on the
stack. A compiler that heap-allocates everything makes those functions allocate,
which makes them need write barriers, which makes them fail to compile — and the
ones that do compile call the allocator from contexts where the allocator cannot
be called.

So escape analysis is optional for G1 and required for G3, and
[003](003-sequencing.md) places it accordingly. This is stated rather than hidden
because "no optimization is required for correctness" is a rule that a reader
will otherwise find a counterexample to, in the hardest possible package.

## What is deliberately absent

| Not implemented | Cost |
| --- | --- |
| Loop-invariant code motion | Redundant work inside loops |
| Loop unrolling | Branch overhead in short loops |
| Instruction scheduling for the pipeline | The scheduler orders for register pressure, not latency |
| Profile-guided anything | No profile input |
| Interprocedural analysis beyond inlining | Conservative escape results at call boundaries |

Each is a real cost in generated code speed. [000](000-decisions.md) decision 10
accepts all of them.

## Testing

- The verifier of [021](021-ssa-construction.md) after every pass.
- Each pass individually disableable by flag, with a test asserting the program
  still produces the same output with the pass off. This is the mechanical form
  of the governing rule, and it is what keeps the rule true.
- Differential execution ([004](004-conformance.md) L3) with all passes on and
  all passes off, over the whole corpus. A disagreement isolates to a pass by
  bisection over the flag set.
