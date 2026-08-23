---
title: "The arm64 backend"
status: draft
layer: back end
gate: G1
depends_on:
  - 025-lowering-and-rules.md
  - 041-instruction-encoding.md
---

# arm64 backend

The first target, by [000](000-decisions.md) decision 9: the host is the target,
so the feedback loop is one command.

The backend is three things and nothing else — a lowering rule set, a register
description, and a set of encoders. Everything general is above it.

## Register description

From [030](030-abi.md), with the allocation view:

| Registers | Allocatable | Note |
| --- | --- | --- |
| R0 – R15 | yes | also the integer argument and result registers |
| R19 – R25 | yes | scratch, no ABI meaning |
| F0 – F15 | yes | floating-point arguments and results |
| F16 – F31 | yes | scratch |
| R16, R17 | **no** | linker trampolines |
| R18 | **no** | reserved by Darwin |
| R26 | **no** | closure context at a call |
| R27 | **no** | assembler scratch for expanded instructions |
| R28 | **no** | current goroutine |
| R29 | **no** | frame pointer |
| R30 | **no** | link register |
| RSP | **no** | stack pointer |
| ZR | **no** | reads as zero, writes discarded |

The zero register is not allocatable but is useful: storing zero, comparing
against zero, and materialising zero all use it, and the rules should.

## What makes arm64 the easy target

| Property | Consequence |
| --- | --- |
| Fixed 4-byte instructions | Instruction layout needs no iteration ([041](041-instruction-encoding.md)) |
| Three-operand arithmetic | No two-address fix-up pass ([026](026-register-allocation.md)) |
| 31 general registers | Spilling is rare |
| Uniform condition codes | One comparison feeds any conditional branch or conditional set |
| Shifted and extended operands | Address modes fold cleanly into rules |

`amd64` has none of these, which is why [043](043-amd64-backend.md) is the target
that tests whether the middle end is genuinely target-neutral.

## The prologue and epilogue

```
    MOVD  16(R28), R16          // stackguard, unless nosplit or a small leaf
    CMP   R16, RSP
    BLS   growstack
    MOVD.W R30, -framesize(RSP) // push link register, adjust SP
    MOVD  R29, -8(RSP)          // save frame pointer
    SUB   $8, RSP, R29
    ... body ...
    MOVD  -8(RSP), R29
    MOVD.P framesize(RSP), R30  // pop link register
    RET   (R30)
growstack:
    ... spill argument registers ...
    CALL  runtime.morestack_noctxt(SB)
    ... restore ...
    JMP   0                     // re-execute the function
```

Two details are load-bearing and are stated because they are easy to lose:

- The frame pointer is saved **below** the new stack pointer, at `-8(RSP)`. It is
  in the reserved region below SP, not in the frame. This is Go's convention on
  `arm64` and the frame size does not include it.
- The stack pointer is always 16-byte aligned, which the architecture requires
  and the operating system enforces.

The unsafe-point ranges of [035](035-goroutines-and-stack-growth.md) cover the
prologue up to the frame being established, the epilogue after it is torn down,
and the whole growstack tail.

## Lowering rules

In `ssa/rules/arm64.go`, per [025](025-lowering-and-rules.md). The groups, in the
order they are worth writing:

1. Integer arithmetic, comparison, and the conditional forms.
2. Loads and stores of every width, signed and unsigned.
3. Address computation, folding constant offsets and scaled indices.
4. Branches and the condition-code forms.
5. Calls, in all four shapes: static, closure, interface, and deferred.
6. Floating point.
7. Atomics, which the runtime and `sync/atomic` need.
8. `memmove` and `memclr` inline forms for small constant sizes.

Group 7 is worth its own note. `arm64` has both the load-exclusive/store-exclusive
loop and, on later revisions, single-instruction atomics. nanogo emits the
exclusive-loop form unconditionally, because selecting the newer form needs a
runtime feature check and the difference is performance only.

## Operating system specifics

`darwin/arm64` is the first configuration. What is specific to it:

- R18 is reserved by the platform, as above.
- The object file is Mach-O, which matters to [045](045-linker.md) and not here.
- The system call convention is the platform's and reaches nanogo only through
  the runtime's assembly, which nanogo compiles at G3.

`linux/arm64` differs in the reserved register and in the linker's output format.
It is not in this deck's scope but nothing here excludes it.

## Testing

- Differential disassembly per [041](041-instruction-encoding.md).
- Differential execution ([004](004-conformance.md) L3) over the whole corpus.
- The prologue specifically: deep recursion forcing `morestack` at every frame
  size class, per [035](035-goroutines-and-stack-growth.md).
- Alignment: an assertion that RSP is 16-byte aligned at every call, checked by
  a debug build that verifies it in the prologue.
