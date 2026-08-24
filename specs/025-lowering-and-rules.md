---
title: "Lowering: from target-neutral SSA to machine operations"
status: draft
layer: middle end
gate: G1
depends_on:
  - 021-ssa-construction.md
  - 030-abi.md
---

# Lowering

The boundary of [002](002-architecture.md). Above it, SSA operations are
target-neutral: `Add64`, `Load`, `Less32U`. Below it they are machine
operations: `ARM64ADD`, `ARM64MOVDload`, `ARM64CMP`. Lowering is the pass that
rewrites one into the other, and it is the only place a target name appears above
the encoders.

## Rewrite rules

Both the target-neutral simplifications of [022](022-optimization-passes.md) pass
3 and the machine selection here are expressed as rewrite rules: a pattern over
the value graph, a condition, and a replacement.

```
(Add64 (Const64 [c]) (Const64 [d]))  =>  (Const64 [c+d])
(Load <t> ptr mem) && is64BitInt(t)  =>  (ARM64MOVDload ptr mem)
(Less32U x y)                        =>  (ARM64LessThanU (CMPW x y))
```

Rules are matched bottom-up over a block's values, repeatedly until no rule
applies. Termination is by construction: every rule either reduces a size measure
or moves an operation strictly closer to machine form, and a rule that does
neither is rejected in review.

### Written by hand first, generated later

The reference implementation compiles its rules from a DSL into Go, and the
generated matchers are the bulk of its 150,654-line SSA package. nanogo writes
the rules as Go functions, one per operation, matching on argument shape.

This is more verbose per rule and needs no generator, no DSL, and no build step.
When the rule count makes that intolerable, a generator is added and the rules
are ported — but not before, because a generator written for twenty rules is a
generator with twenty users' worth of bugs and no test corpus.

The rule *file* is per target, in `ssa/rules/`. The rule *engine* is shared.

## What lowering must produce

After lowering, every value in the function is either a machine operation, a phi,
or one of a short list of pseudo-operations that survive to
[026](026-register-allocation.md):

| Pseudo-operation | Meaning | Resolved by |
| --- | --- | --- |
| `Phi` | SSA merge | [026](026-register-allocation.md) |
| `Arg` | An incoming argument in its ABI location | [030](030-abi.md) |
| `SelectN` | A call result in its ABI location | [030](030-abi.md) |
| `InitMem` | The root of the memory chain; not an instruction | never; it has no machine form |
| `Copy` | Value identity, no machine effect | [026](026-register-allocation.md) |
| `SP`, `SB` | Frame and static base pointers | [027](027-liveness-and-stackmaps.md) |
| `VarDef`, `VarKill` | Lifetime markers for stack objects | [027](027-liveness-and-stackmaps.md) |

`SelectN` and `InitMem` are a correction. The first version of this table
omitted both, and both must survive lowering: `SelectN` is the exact mirror of
`Arg` and is resolved by the same ABI pass, and `InitMem` names the start of the
memory chain rather than any instruction. The asymmetry that made them look like
oversights is that `MakeResult`, which sits beside them, *does* have a machine
form and becomes the target's return instruction.

Anything else remaining is a missing rule and is a compiler crash with the
operation named, never a silent fallback. A silent fallback in this pass produces
a function that is missing an operation, which is the hardest class of bug in the
whole compiler to find.

## Multi-word values

A value wider than a machine register cannot be one machine operation. A string
is a pointer and a length, a slice adds a capacity, an interface is two words,
and a struct or array can be any size. `Load` and `Store` of such a type, and a
string constant, have no single instruction on any target here.

**Splitting them into per-word operations is this pass's work**, and an earlier
version of this spec did not say so anywhere. That omission was not academic: it
is the largest single reason a function fails to lower, accounting for 3,164 of
3,483 refusals over the distribution corpus.

It cannot be pushed into a rule or worked around downstream. A 16-byte `Store`
cannot become a call to `runtime.memmove`, because `memmove` takes a source
**address** and a `Store` has a source **value**; there is nothing to take the
address of. The decomposition has to happen while the value is still a value.

So lowering runs a decomposition step before selection: a value of a multi-word
type is replaced by one value per machine word, and every operation over it is
replaced by the corresponding per-word operations. Selection then sees only
single-register values, which is the property every rule below assumes.

## Operations that lower to calls

Some target-neutral operations have no machine instruction. On both targets these
include 64-bit division on 32-bit registers, some conversions, and every
operation in [031](031-runtime-lowering.md)'s table that survived
[020](020-ir.md).

These lower to calls, which means lowering can introduce a call into a block that
had none. [027](027-liveness-and-stackmaps.md) must therefore run after lowering,
because a call is a safepoint and safepoints determine stack maps.

## Address modes

The payoff of rules over a hand-written selector is address modes, where a
multi-value pattern collapses into one instruction:

```
(ARM64MOVDload [off1] (ADDconst [off2] ptr) mem) && fits(off1+off2)
    => (ARM64MOVDload [off1+off2] ptr mem)
```

This is where most of the difference between naive and reasonable code
generation lives, and it costs one rule per mode rather than a special case in a
selector.

## Constants

A constant that fits a target's immediate encoding stays an immediate. One that
does not becomes either a materialising instruction sequence or a load from a
constant pool, per target. The decision is in the rules, and
[041](041-instruction-encoding.md) states each target's immediate forms.

**A target's constant and address forms must be rematerialisable**, or
[026](026-register-allocation.md)'s rematerialisation stops applying to that
target. It is worth stating here rather than only there, because the two halves
are written by different people and the failure is silent: nothing breaks, every
constant is simply spilled and reloaded instead of recomputed. An address form
cannot be marked constant in the op table, since it takes a frame or static base
argument and "constant" means "depends on nothing", so the target's
rematerialisable list is where it goes.

## Where the target's semantics differ from Go's

A rule may not assume the machine does what the language says. The shift
operators are the example that matters, and nothing in this deck said so before
one was written.

Go defines a shift by a count at or above the operand width as producing zero,
and the count is unsigned, so a count of $2^{63}$ is a legal enormous number.
`arm64`'s `LSLV` takes the count **modulo the width**, so it computes $x \ll 1$
for a count of 65. A signed comparison against the width is not a correct guard
either, since a count above $2^{63}$ reads as negative.

The rule therefore computes the mask arithmetically, which is correct for every
unsigned count:

$$
u = s \gg \log_2 w, \qquad m = -u \gg 63
$$

with $m$ then cleared from the result for a left or unsigned right shift, and
merged into the count for an arithmetic right shift, where the defined result is
the sign bit rather than zero.

Any rule that maps a Go operator to a machine instruction owes the same
question: does the instruction agree with the language for every input, or only
for the ones a test happens to try.

## Testing

- The verifier of [021](021-ssa-construction.md), extended with "no
  target-neutral operation remains".
- Per-rule tests: a minimal function whose lowered form is asserted exactly.
  These are the tests that make a rule change reviewable.
- Differential execution ([004](004-conformance.md) L3), which is what actually
  finds a wrong rule, since a wrong rule usually produces code that runs.
- A rule-coverage report: which target-neutral operations have no rule for a
  target. It must be empty before that target is claimed to work.
