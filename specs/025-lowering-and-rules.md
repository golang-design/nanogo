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
rewrites one into the other, and it is the only pass that turns one into the
other. It is not the only place above the encoders that names a target: the
machine operation set is `ssa/macharm64.go`, the register file and the target
description are `ssa/target.go`, and [030](030-abi.md)'s placement and
[027](027-liveness-and-stackmaps.md)'s frame layout name `arm64` where the
convention differs. What is confined to this pass is the choice of which machine
operation a target-neutral one becomes.

This pass is built, for `arm64`, and gated. `ssa` and `ssa/rules` are both above
the 90% coverage gate, and `TestARM64RuleCoverage` fails if a target-neutral
operation has neither a rule nor a stated reason to be deferred; one operation
is deferred and none is missing.

What lowering sees is narrower than the language, and the narrowing happens
above it. SSA construction accepts 17,905 of the 39,947 functions the IR
builder produces for the Go distribution, and [021](021-ssa-construction.md)
counts the refusals. **17,809 of the 17,905 lower completely.** Read the two
numbers together: this pass finishes all but 96 of what reaches it, and what
reaches it is under half of the distribution. The gap between the two numbers
is where a contributor's work is, and it is above this pass rather than in it.
What remains is a `Load` or a `Store` of an array or a struct that
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
`lowerer.checkRule` checks exactly that after every rule fires and crashes
naming the operation if a rule left one behind. A folding rule may only replace
an argument by one of that argument's own arguments, or merge two values into
one, which strictly reduces the sum over values of the height of their
arguments in an acyclic use-def graph. The pass cap of 100 walks per block is
therefore not the termination argument. It is the assertion that the argument
holds: a bad rule makes the engine crash rather than hang.

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

`SelectN` and `InitMem` earn their rows for reasons that are easy to miss.
`SelectN` is the exact mirror of `Arg` on the other side of a call and is
resolved by the same ABI pass. `InitMem` names the start of the memory chain
rather than any instruction, so nothing can lower it. `MakeResult` sits beside
them in the operation set and is *not* here, because it does have a machine form
and becomes the target's return instruction. `ssa.isPseudo` is the list this
table describes, and the two must agree operation for operation.

Anything else remaining is a missing rule and is a compiler crash with the
operation named, never a silent fallback. A silent fallback in this pass produces
a function that is missing an operation, which is the hardest class of bug in the
whole compiler to find.

## Multi-word values

A value wider than a machine register cannot be one machine operation. A string
is a pointer and a length, a slice adds a capacity, an interface is two words,
and a struct or array can be any size. `Load` and `Store` of such a type, and a
string constant, have no single instruction on any target here.

**Splitting them into per-word operations is this pass's work.** It is done, in
`ssa/decompose.go`, and `ssa/decompose_test.go` measures what it leaves.

What it leaves is residue rather than refusal. After decomposition and the ABI
assignment, over the distribution corpus, some functions still hold a value
wider than a register: 87 hold a `SelectN`, 16 hold a `Load`, 3 hold a
`ConstNil`, and by type, 93 hold a struct and 12 hold an array. Most of those
functions lower anyway, because a wide value that no rule has to select is not
a problem for this pass. These five counts are the test's own report and are
not in `internal/hygiene/testdata/facts.json`, so nothing fails when they
drift. Re-run the test rather than trusting them.

The 96 that do not lower are a different tally and the two must not be added
up. What stops them is a `Load` or a `Store` of an array or a struct that
decomposition leaves whole, which is a value a rule would have to select and
cannot.

Splitting cannot be pushed into a rule or worked around downstream. A 16-byte
`Store` cannot become a call to `runtime.memmove`, because `memmove` takes a
source **address** and a `Store` has a source **value**, so there is nothing to
take the address of. The decomposition has to happen while the value is still
a value.

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
bound at a call boundary.** Go's internal ABI passes a five-field struct in five
registers, so an aggregate this pass leaves whole may still travel in pieces.
The ABI pass of [030](030-abi.md) therefore finishes the split for whatever the
convention says travels in registers, and `TestABILeavesMatchesDecomposition`
pins that its walk and this one produce identical offsets and widths for the
types the language builds.

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

`CheckDecomposed` is what holds the invariant: it reports a call whose results
do not name the words of its result area from zero, once each. The invariant is
worth a checker rather than a test, because $w_i = i$ for every result list
whose earlier results are one word wide, which is most of them, and a
numbering that is right on most calls and wrong on the rest reads the wrong
word rather than crashing.

Two properties this rests on, and neither is local to this pass.
[021](021-ssa-construction.md) reads every result of a call, including one
assigned to the blank identifier, so the widths of the earlier results are
there to be summed; a call whose result list is incomplete is left alone rather
than renumbered on a guess. And the caller's list of words and the callee's are
the same walk over the same types, which is why `ABIResults` places them in the
same registers on both sides.

### A result nothing reads is still a word, and the dead-value sweep forgets it

One shape of multi-value assignment does not reach machine code yet, and the
reason is in this pass rather than in the numbering above.

`_, n := f()` where the first result is wider than a register builds, splits
and numbers correctly: the parts of the discarded result are words 0 and 1 and
`n` is word 2. Nothing reads the parts, so `deadValues` removes them, because
`removable` excludes `Arg` from the sweep and not `SelectN`. The code generator
then places the results of the call by index, finds no value on word 0, and
refuses the function with `result 0 of the call is never named` and
`a call result that follows no call`.

Both operations are pseudo-operations that name an ABI location whether or not
the body reads them, which is the reason `Arg` is excluded already, and adding
`SelectN` beside it is the fix. It is stated here and not taken because it was
found from outside this pass, with `go build -toolexec=nanogo` over the program
above, and the failure is a refusal rather than a miscompile.

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
operators are the example that matters.

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
  Built, in `ssa/rules/arm64_test.go` and `ssa/rules/float_test.go`. A rule set
  for the floating-point operations is not a compiler that emits them: a
  function that needs a floating-point register is refused later, by `ssagen`
  and by the allocation, per [042](042-arm64-backend.md).
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

## What was wrong

The spec did not say anywhere that splitting wide values belongs to this pass.
When that was found, splitting was the largest single reason a function failed
to lower, at 3,164 of 3,483 refusals over the distribution corpus. The pass was
then written and that reason went to zero. The 96 that remain today are a
different shape: SSA construction learned the assignment statement, and the
shapes that arrived with it include an array and a struct that decomposition
leaves whole. The claim the correction made, that the split belongs above
selection and while the value is still a value, is what the pass then proved.
The corpus measured 4,755 of 8,238 functions lowering when the pass landed and
17,809 of 17,905 now; the denominator moved because construction learned more
Go, not because this pass did.

The spec's pseudo-operation table omitted `SelectN` and `InitMem`. Both survive
lowering and both are in `ssa.isPseudo`.

The spec justified the bound on decomposition parts by claiming an aggregate
that large "is passed through memory by every calling convention". That is
false for Go's internal ABI, which passes a five-field struct in five
registers. The bound stands; the justification does not, and [030](030-abi.md)
finishes the split instead.

The pass numbered part $j$ of result $i$ as word $i+j$ until August 2026, which
is $w_i$ only while every result before $i$ is one word wide. For
`(string, int)` it made the integer word 1, which holds the length of the
string, so the caller read the length in place of the integer. Nothing caught
it, because construction refused the form that produces it rather than emit two
reads of one word, and the refusal was 2,191 functions of the distribution
corpus. Both are gone: the sum in the design above is what the pass computes,
and `CheckDecomposed` checks it.

The spec said rules are matched bottom-up. `ssa/lower.go`'s `lowerer.block`
walks a block forward from index zero and repeats. This was found by reading the
engine while checking the termination argument, which turned out to be checked
in code rather than in review. The spec named the checker `applyRule`; no such
function exists, and the check is `lowerer.checkRule`.

The spec said this pass is the only place a target name appears above the
encoders. `ssa/macharm64.go` holds the machine operation set and
`ssa/target.go` holds the register file, both above the encoders and both named
for the target. What is confined to this pass is the choice of machine
operation.

The spec said the pass finishes "two fifths of the distribution". 17,809 of
39,947 is closer to a half, and the fraction was doing work the two counts
already do. The counts stay and the fraction is gone.

The spec said lowering runs a decomposition step. Decomposition is its own
exported pass, with the ABI assignment between it and selection. The order is in
`driver/compile.go`.

The spec cited [022](022-optimization-passes.md) pass 3 as the engine's other
client. That pass does not exist.

No spec in this deck stated that a rule owes the language's semantics for every
input until the shift rule was written. The section above is that statement, and
it belongs to every rule and not only to the shifts.

The spec said the one-instruction conditional select is not used because
`obj/arm64` has no encoder for it. `obj/arm64/condsel.go` encodes the whole
class, `Csel` included. The gap moved up a layer: `ssa/macharm64.go` has one
operation from that class, `CSET`, so there is no machine operation for a rule
to select and the arithmetic mask is still what `lowerShift` emits. The
derivation above is unaffected, and `ssa/rules/arm64.go`'s comment on
`lowerShift` still gives the old reason.

Accurate and left alone: the pseudo-operation table, which matches
`ssa.isPseudo` operation for operation including the exclusion of `MakeResult`;
the shift-mask derivation, which matches `lowerShift` arithmetically; and the
requirement that constant and address forms be rematerialisable.
