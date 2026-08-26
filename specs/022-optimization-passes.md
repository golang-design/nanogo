---
title: "Optimization passes: the minimum set, and their order"
status: draft
layer: middle end
gate: G1
depends_on:
  - 021-ssa-construction.md
---

# Optimization passes

**No pass in this spec is built as a pass.** Rows 1 to 9, 11 and 12 of the
list below exist nowhere as a pass the driver runs, and no flag turns one off
because there is none to turn off. Rows 10, 13 and 14 exist and are owned by
[025](025-lowering-and-rules.md), [026](026-register-allocation.md) and
[027](027-liveness-and-stackmaps.md), which is where they are gated.

Three narrow pieces of rows 1 and 3 do exist, each inside a pass that needs it
for its own correctness or its own output, and each says so in a comment that
cites this spec:

- `ssa/build.go`'s `removeUnreachable` deletes the blocks the entry cannot
  reach. Construction produces them, `InvReachable` forbids them, and row 1 is
  not allowed to be required for correctness, so the graph has to satisfy the
  invariant before construction hands it over.
- `ssa/lower.go`'s `deadValues` removes the values a rule's fold orphaned. An
  address computation a load absorbed has no user left, and leaving it would
  cost a register and make the lowered form impossible to assert against.
- `ssa/rules/arm64.go` collapses a chain of constant offsets and folds a
  constant operand into an immediate form. That is
  [025](025-lowering-and-rules.md)'s address-mode work, in the target's own
  rule file, and it folds an address rather than an expression.

None of the three is the target-neutral pass its row names, and the boundary
matters: a pass here may be deleted, and each of those three may not.

This is a design that has held up rather than a design that shipped, and it
costs nothing yet: the governing rule below is exactly why. What is missing is
speed in the generated code, not correctness.

The pipeline that runs today is `driver/compile.go`'s per-function pass list.
It is nine stages, and it opens and closes outside this spec: with
[020](020-ir.md)'s `ir.Lower`, which performs the lowering table before
construction can refuse it, and with `ssagen.Emit`. Between them are six `ssa`
calls:

    ssa.Build, ssa.Decompose, ssa.AssignABI, ssa.Lower,
    ssa.SplitCriticalEdges, ssa.Allocate

`ssa.Verify` runs after `AssignABI` and after `Lower`, as its own stage each
time, so a violation names the pass that made it. Three of the six are not in
the ordered list below and have to be: decomposition and the ABI assignment
split values wider than a register, per [025](025-lowering-and-rules.md) and
[030](030-abi.md), and `SplitCriticalEdges` prepares the graph for
[026](026-register-allocation.md)'s phi resolution. The list below is a plan for
the optional work, and it was never the whole pass order.

The governing rule follows from the compiler being meant to be read end to end,
and it is stated first because it decides every argument in this spec:

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

nanogo is to implement the dominator-tree fact propagation and no more. Loop
induction analysis beyond the direct comparison, and the full integer relation
lattice, are excluded by design. A pass a reader can follow in one sitting is
the simple version of this one, not the complete one.

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
which makes them need write barriers, which makes them fail to compile, and the
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

Each is a real cost in generated code speed. This spec accepts all of them, and
the governing rule above is why none of them is a correctness cost.

## Testing

None of this exists, because no pass does. Recorded so that the mechanism is
designed before the first pass is written, not after:

- The verifier of [021](021-ssa-construction.md) after every pass.
- Each pass individually disableable by flag, with a test asserting the program
  still produces the same output with the pass off. This is the mechanical form
  of the governing rule, and it is what keeps the rule true. There is no such
  flag today. `driver/flags.go` parses `-N`, which `gc` uses to disable
  optimization, and nothing reads it.
- Differential execution ([004](004-conformance.md) L3) with all passes on and
  all passes off, over the whole corpus. A disagreement isolates to a pass by
  bisection over the flag set.

## What was wrong

The spec said the list is ordered and complete. It is neither, and both were
found by reading `driver/compile.go`, which is the only place the pass order is
written down in code. It runs three passes this list never named, and none of
the fourteen it did name except lowering, allocation and liveness.

The spec said it is cited by two files that assume its passes exist. It is
cited by five, at eight sites: `ssa/build.go` three times, `ssa/op.go` twice,
and `ssa/dom.go`, `ssa/lower.go` and `ir/node.go` once each. `ssa/build.go`
inserts a bounds check "specs/022 removes the ones prove can discharge",
`ssa/dom.go` computes the dominator tree "by the verifier and by prove, cse and
nilcheckelim in specs/022", `ssa/op.go` marks an operation commutative for "cse
and the rewrite rules", and `ir/node.go` gives a constant a numeric reader
because "the constant folding of specs/022 cannot fold what it cannot read".
The checks are inserted, the tree is computed, the flag is set and the reader
exists. Nothing removes a check, and no pass named in any of those comments
exists.

The opening of this spec said nothing in it is built and that rows 1 to 9, 11
and 12 have no code anywhere in the repository. The second half was too strong.
`ssa/build.go`'s `removeUnreachable`, `ssa/lower.go`'s `deadValues` and
`ssa/rules/arm64.go`'s constant-offset folds each do a narrow part of row 1 or
row 3, inside a pass that cannot delete it. The correction is above rather than
here, because a reader who greps for `deadcode` finds those three and needs the
distinction before the list, not after it.
