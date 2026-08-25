---
title: "Lowering: from target-neutral SSA to machine operations"
status: complete
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

This pass is built, for `arm64`, and gated. `ssa` and `ssa/rules` are both above
the 90% coverage gate, and `TestARM64RuleCoverage` fails if a target-neutral
operation has neither a rule nor a stated reason to be deferred; one operation
is deferred and none is missing.

What lowering sees is narrower than the language, and the narrowing happens
above it. SSA construction accepts 17,905 of the 39,947 functions the IR
builder produces for the Go distribution, and [021](021-ssa-construction.md)
counts the refusals. **17,809 of the 17,905 lower completely.** Read the two
numbers together: this pass finishes all but 96 of what reaches it, which is
two fifths of the distribution. Most of the 96 hold an array or a struct that
decomposition leaves whole.

## Rewrite rules

The machine selection here is expressed as rewrite rules: a pattern over the
value graph, a condition, and a replacement. The target-neutral simplifications
of [022](022-optimization-passes.md) pass 3 are meant to use the same engine,
and that pass is not written, so the engine has one client today.

```
(Add64 (Const64 [c]) (Const64 [d]))  =>  (Const64 [c+d])
(Load <t> ptr mem) && is64BitInt(t)  =>  (ARM64MOVDload ptr mem)
(Less32U x y)                        =>  (ARM64LessThanU (CMPW x y))
```

Rules are matched forward over a block's values, from the first, and the block
is walked again until no rule applies.

Termination is asserted, not reviewed, and the assertion is what the engine
rests on. A selection rule leaves machine and pseudo-operations only, so the
number of target-neutral values strictly decreases at every application;
`applyRule` checks exactly that after every rule fires and crashes naming the
operation if a rule left one behind. A folding rule may only replace an argument
by one of that argument's own arguments, or merge two values into one, which
strictly reduces the sum over values of the height of their arguments in an
acyclic use-def graph. The pass cap of 100 walks per block is therefore not the
termination argument. It is the assertion that the argument holds: a bad rule
makes the engine crash rather than hang.

An earlier version of this paragraph said "bottom-up" and put the whole
termination argument in review. `ssa/lower.go`'s `lowerer.block` walks forward
from index zero, and `checkRule` runs the check on every application.

### Written by hand first, generated later

The reference implementation compiles its rules from a DSL into Go, and the
generated matchers are the bulk of its 150,654-line SSA package. nanogo writes
the rules as Go functions, one per operation, matching on argument shape.

This is more verbose per rule and needs no generator, no DSL, and no build step.
When the rule count makes that intolerable, a generator is added and the rules
are ported, but not before, because a generator written for twenty rules is a
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
version of this spec did not say so anywhere. That omission was not academic.
When it was written, splitting was the largest single reason a function failed
to lower, at 3,164 of 3,483 refusals over the distribution corpus.

That is discharged. `ssa/decompose.go` exists, and the corpus of
`ssa/decompose_test.go` now reports 17,905 functions reaching SSA and 17,809
lowering completely, against 4,755 of 8,238 before the pass. What is left is
residue rather than the old refusal: after decomposition and the ABI
assignment, 87 functions still hold a `SelectN` of multi-word type, 12 hold a
value of array type, and 93 hold a value of struct type. All but 96 of those
functions lower anyway.

Anything in the deck that still names undecomposed wide values as the largest
cause of unlowered functions is out of date by one pass. The cause of a function
not reaching machine code is now construction refusing it, not lowering failing
on it.

It cannot be pushed into a rule or worked around downstream. A 16-byte `Store`
cannot become a call to `runtime.memmove`, because `memmove` takes a source
**address** and a `Store` has a source **value**; there is nothing to take the
address of. The decomposition has to happen while the value is still a value.

So decomposition runs before selection: a value of a multi-word type is replaced
by one value per machine word, and every operation over it is replaced by the
corresponding per-word operations. Selection then sees only single-register
values, which is the property every rule below assumes.

It is a pass of its own, `ssa.Decompose`, and not a step inside `ssa.Lower`, and
the ABI assignment runs between the two. `driver/compile.go` is where that order
is written: `Build`, `Decompose`, `AssignABI`, `Lower`. The assignment sits
there because it finishes the split at the call boundary, which the next
paragraph is about.

The step has a bound on how many parts it will produce, because decomposition
trades one memory object for that many simultaneously live values and stops
paying when the number approaches the register file. **That bound is not a
bound at a call boundary**, and an earlier version of this spec justified it by
claiming an aggregate that large "is passed through memory by every calling
convention". That is false for Go's internal ABI, which passes a five-field
struct in five registers. The ABI pass of [030](030-abi.md) therefore finishes
the split for whatever the convention says travels in registers, and a test
pins that its walk and this one produce identical offsets and widths.

### A call result is named by the word it starts at

`SelectN` has an index and the index means two different things on the two
sides of this pass. Before it, the index is the **result**: `SelectN [1]` of a
call that returns `(string, int)` is the integer. After it, the index is the
**machine word of the result area**, because the values that reach the code
generator are one word each and [030](030-abi.md) places them by walking that
list of words.

So the pass renumbers. The word result $i$ starts at is

$$
w_i = \sum_{k<i} n_k
$$

where $n_k$ is the number of values the pass leaves in place of result $k$:
one per part for a result it splits, one for a result it leaves whole, and
none for a result of no width. Part $j$ of result $i$ is then word $w_i + j$.

The pass numbered part $j$ of result $i$ as word $i+j$ until August 2026, which
is $w_i$ only while every result before $i$ is one word wide. For
`(string, int)` it made the integer word 1, which holds the length of the
string, so the caller read the length in place of the integer. Nothing caught
it, because construction refused the form that produces it rather than emit two
reads of one word, and the refusal was 2,191 functions of the distribution
corpus. Both are gone: the sum above is what the pass computes, and
`CheckDecomposed` reports a call whose results do not name the words of its
result area from zero, once each.

Two properties this rests on, and neither is local to this pass.
[021](021-ssa-construction.md) reads every result of a call, including one
assigned to the blank identifier, so the widths of the earlier results are
there to be summed; a call whose result list is incomplete is left alone rather
than renumbered on a guess. And the caller's list of words and the callee's are
the same walk over the same types, which is why `ABIResults` places them in the
same registers on both sides.

### A result nothing reads is still a word, and the dead-value sweep forgets it

One shape of the form this unblocked does not reach machine code yet, and the
reason is in this pass rather than in the numbering.

`_, n := f()` where the first result is wider than a register builds, splits
and numbers correctly: the parts of the discarded result are words 0 and 1 and
`n` is word 2. Nothing reads the parts, so `deadValues` removes them, because
`removable` excludes `Arg` from the sweep and not `SelectN`. The code generator
then places the results of the call by index, finds no value on word 0, and
refuses the function with `result 0 of the call is never named` and
`a call result that follows no call`.

Both operations are pseudo-operations that name an ABI location whether or not
the body reads them, which is the reason `Arg` is excluded already. This is
recorded rather than fixed here because it was found from outside this pass,
with `go build -toolexec=nanogo` over the program above. The failure is loud
and it is not a miscompile.

## Operations that lower to calls

Some target-neutral operations have no machine instruction. They lower to calls,
which means lowering can introduce a call into a block that had none.
[027](027-liveness-and-stackmaps.md) must therefore run after lowering, because
a call is a safepoint and safepoints determine stack maps.

On `arm64` the rules that do this are a short list, and it is worth naming
because each one is a contract with the runtime rather than an instruction
choice: a wide move becomes `runtime.memmove`, a zeroing becomes
`runtime.memclrNoHeapPointers` or `runtime.memclrHasPointers` by pointer map, a
division guards against zero and branches to `runtime.panicdivide`, and a failed
bounds check calls the `runtime.goPanic*` family.

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
  target-neutral operation remains". Built: `lowerer.check` runs over every
  value after the pass, and `driver/compile.go` runs `ssa.Verify` after it.
- Per-rule tests: a minimal function whose lowered form is asserted exactly.
  Built, in `ssa/rules/arm64_test.go` and `ssa/rules/float_test.go`.
- A rule-coverage report: which target-neutral operations have no rule for a
  target. Built, as `TestARM64RuleCoverage`. It fails on an operation with
  neither a rule nor an entry in `Deferred`, and it fails on an operation that
  has both, because two answers to one question is how such a list rots.
- Differential execution ([004](004-conformance.md) L3), which is what actually
  finds a wrong rule, since a wrong rule usually produces code that runs. This
  spec cites that mechanism and does not build it:
  [004](004-conformance.md) owns it, and it needs a whole program to compile.
  What proves the pass produces runnable code today is `ssagen`'s 18 cases,
  which take source text to a linked, running process.

## Deviations

The spec said splitting wide values is the largest single reason a function
fails to lower, at 3,164 of 3,483. The pass was then written and the number
went to zero. It is 96 of 17,905 today, because SSA construction learned the
assignment statement and the shapes that arrived with it include an array and a
struct that decomposition leaves whole. The note is kept rather than deleted
because the claim it makes
about *where* the split belongs, above selection and while the value is still a
value, is what the pass then proved.

The spec said rules are matched bottom-up. `ssa/lower.go`'s `lowerer.block`
walks a block forward from index zero and repeats. This was found by reading the
engine while checking the termination argument, which turned out to be checked
in code rather than in review.

The spec said lowering runs a decomposition step. Decomposition is its own
exported pass, with the ABI assignment between it and selection. The order is in
`driver/compile.go`.

The spec cited [022](022-optimization-passes.md) pass 3 as the engine's other
client. That pass does not exist.

Accurate and left alone: the pseudo-operation table, which matches
`ssa.isPseudo` operation for operation including the exclusion of `MakeResult`;
the shift-mask derivation, which matches `lowerShift` down to the reason the
one-instruction conditional select is not used, that `obj/arm64` has no encoder
for it; and the requirement that constant and address forms be
rematerialisable.
