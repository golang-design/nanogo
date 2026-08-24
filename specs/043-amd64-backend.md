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
that exists, and `driver/compile.go` refuses any other `GOARCH` with an error
that names this spec as the reason. This was found by listing the tree: `obj/`
holds `arm64` and nothing else.

The middle end's amd64 seams do exist, and that is the part worth reporting
rather than the absence. `ssa/target.go` carries a `TwoAddress` predicate
for the operand form that belongs to amd64 alone, `ssa/regalloc.go` emits the
`Fixup` copy it needs, and `ssa/frame.go` carries the case where the call
instruction pushes the return address above the frame. So three of the hooks
[000](000-decisions.md) decision 5 predicts were built in advance of the target
that needs them. Whether they are the right hooks is untested, because only one
target has ever used them, and one thing this section asks for is not there at
all: the allocator has no pre-coloured operand, only the pre-coloured argument
and result that [030](030-abi.md) fixes.

Everything below is the design, unchanged, and none of it is code.

## Purpose

The second target. Its purpose is partly to run on `linux/amd64` and mostly to
test [000](000-decisions.md) decision 5: **if adding this target requires an edit
above [025](025-lowering-and-rules.md), the middle end is wrong.**

That is the acceptance criterion, and it is checked by review of the diff, not by
a test.

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
| RDX | yes, constrained | fixed for division's high half |
| RCX | yes, constrained | fixed as the variable shift count |
| RSP | no | stack pointer |
| RBP | no | frame pointer |
| R12 | no | scratch, used by the assembler |
| R14 | no | current goroutine |
| R15 | no | closure context at a call, and GOT base in dynamic builds |
| X15 | no | holds zero |

The constrained registers are the reason the allocator needs a notion of a
pre-coloured operand rather than only pre-coloured arguments. That is a change to
[026](026-register-allocation.md), and it is a change *inside* the allocator's
existing abstraction, which is why it does not violate the acceptance criterion.

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
