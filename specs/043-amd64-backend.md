---
title: "The amd64 backend"
status: draft
layer: back end
gate: G1 on a second target
depends_on:
  - 042-arm64-backend.md
---

# amd64 backend

**This spec describes unbuilt work.** There is no `obj/amd64` package, no amd64
rule set, and no amd64 machine operation set. `darwin/arm64` is the only target
that exists: `driver.TargetArch` is `"arm64"`, and `driver/compile.go`'s
`checkTarget` refuses every other `GOARCH` with `nanogo emits arm64 machine
code and the build is for amd64 (specs/043-amd64-backend.md is unbuilt)`. The
refusal names this file, so a reader who hits it arrives here.

The middle end's amd64 seams do exist, and a contributor needs them before
reading further. `ssa/target.go` carries a `TwoAddress` predicate for the
operand form that belongs to amd64 alone, `ssa/regalloc.go` emits the `Fixup`
copy it needs, and `ssa/frame.go` carries the case where the call instruction
pushes the return address above the frame. So three of the hooks
[000](000-decisions.md) decision 5 predicts were built in advance of the target
that needs them. Whether they are the right hooks is untested, because only one
target has ever used them.

## Purpose

The second target. Its purpose is partly to run on `linux/amd64` and mostly to
test [000](000-decisions.md) decision 5: **if adding this target requires an edit
above [025](025-lowering-and-rules.md), the middle end is wrong.**

That is the acceptance criterion, and it is checked by review of the diff, not by
a test. It is already known to fail in one place, and
[000](000-decisions.md) decision 5 records the failure rather than arguing it
away: `ssagen` sits above [025](025-lowering-and-rules.md)'s target boundary,
names a target, and holds an arm64 prologue, so this target is an edit there.
This spec is the milestone that forces `ssagen` to get a spec and a target
interface of its own.

## What amd64 breaks that arm64 did not

| Property | Consequence |
| --- | --- |
| Variable-length instructions, 1 to 15 bytes | Instruction layout iterates to a fixed point; a branch that grows can push another branch out of range |
| Two-address arithmetic | The destination is also a source; [026](026-register-allocation.md)'s fix-up pass exists for this |
| 16 general registers, some with fixed roles | More spilling; `RSP` and `RBP` are not allocatable and `RCX` is fixed for variable shifts |
| Complex addressing modes | More rule opportunity, and more rules |
| Condition codes set as a side effect | Arithmetic sets flags, so a comparison can sometimes be removed, and a rule that reorders across a flag-setting instruction is wrong |
| Byte and word register aliasing | A write to a 32-bit register zeroes the upper half; a write to an 8-bit one does not |

The last row is the classic source of miscompiles on this target. A 32-bit
operation whose result is used as 64 bits is correct because of the zeroing; the
same assumption for an 8-bit or 16-bit operation is not.

## Register description

| Registers | Allocatable | Note |
| --- | --- | --- |
| RAX, RBX, RCX, RDI, RSI, R8 – R11 | yes | RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11 are the argument and result registers |
| X0 – X14 | yes | floating point |
| RDX | yes, constrained | fixed for division's high half, and the closure context pointer at a call |
| RCX | yes, constrained | fixed as the variable shift count |
| RSP | no | stack pointer |
| RBP | no | frame pointer |
| R12, R13 | yes | permanent scratch in Go's amd64 convention; `gc` allocates both, and a reservation here would be a choice with a spilling cost |
| R14 | no | current goroutine |
| R15 | no | GOT reference temporary in a dynamically linked binary; [045](045-linker.md) excludes those, so this reservation is a choice and not a requirement |
| X15 | no | holds zero |

RDX carries two fixed roles and is in neither convention's argument sequence.

The constrained registers are what a pre-coloured operand is for, and the
mechanism is already in the allocator. `ssa/target.go`'s `UseReg` fixes the
register an operand must occupy and `ssa/regalloc.go` honours it. What is
missing is a second caller: [030](030-abi.md)'s assignment walk is the only
thing that fixes an operand today, for a call's operands and a return's values,
and no machine instruction constrains one. Division and the variable shift are
therefore the first users of a hook nothing has exercised. That is a change to
what feeds [026](026-register-allocation.md) and not to its abstraction, which
is why it does not violate the acceptance criterion.

## Instruction layout iteration

Branch displacements come in 1-byte and 4-byte forms. Choosing the short form
shrinks the function, which can bring another branch into short range, which
shrinks it further.

Layout therefore iterates: assume all branches are long, then repeatedly shorten
any whose distance permits, until nothing changes. This converges because
shortening never increases a distance.

The alternative, assuming all branches are short and lengthening, does not
converge as cleanly and is not used.

## Frame layout

```
    // no stack check for small leaf functions
    CMPQ  SP, 16(R14)
    JLS   growstack
    PUSHQ BP
    MOVQ  SP, BP
    SUBQ  $framesize, SP
    ... body ...
    ADDQ  $framesize, SP
    POPQ  BP
    RET
```

The call instruction pushes the return address, so the frame includes it and the
offsets differ from `arm64` throughout. This is contained in
[030](030-abi.md)'s layout rules and does not reach the middle end.

## Testing

Everything in [042](042-arm64-backend.md), plus:

- **Cross-target rule coverage**: every target-neutral operation with an `arm64`
  rule must have an `amd64` rule, checked mechanically.
- **Sub-register width tests**: a corpus of 8-, 16-, and 32-bit operations whose
  results are used at wider widths, which is the aliasing hazard above.
- **Layout convergence**: a generated function with many branches at the short
  or long boundary, asserting the iteration terminates and the result is
  correct.
- The G1 fixed point on `linux/amd64`, which is the milestone's real gate.

## What was wrong

- The spec said the allocator has no pre-coloured operand and that amd64's
  constrained registers need the notion added to
  [026](026-register-allocation.md). `ssa/target.go`'s `UseReg` is that hook and
  `ssa/regalloc.go` honours it at two sites. Only the caller is missing. Found
  by reading `Target.UseReg`'s declaration against the allocator's uses of it.
- The register description gave R15 the closure context. On amd64 the closure
  context pointer is RDX: `cmd/internal/obj/x86` defines `REGCTXT` as it, and
  the ABIInternal register table lists it. R15 is the GOT reference temporary in
  a dynamically linked binary and holds nothing else.
- The same description called R12 the assembler's scratch, which is `arm64`'s
  R27 role moved to the wrong machine. R12 and R13 are permanent scratch
  registers on amd64, which means caller-clobberable and not unallocatable:
  `gc` allocates both. R13 was absent from the table, so the table described 15
  of the 16 general registers it counted.
- The absence of `obj/amd64` was found by listing the tree. `obj/` holds
  `arm64` and nothing else.
