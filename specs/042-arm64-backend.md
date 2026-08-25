---
title: "The arm64 backend"
status: in progress
layer: back end
gate: G1
depends_on:
  - 025-lowering-and-rules.md
  - 041-instruction-encoding.md
---

# arm64 backend

The first target, by [000](000-decisions.md) decision 9: the host is the target,
so the feedback loop is one command.

The backend is a lowering rule set, a register description, and a set of
encoders. Everything general is above it.

## Where the backend is, which is one place more than this spec said

The spec said the backend is three things and nothing else. It is five files,
and the fifth is the one this spec spends the most words on. The prologue, the
epilogue, the frame layout and the stack-growth tail below are emitted by
`ssagen/prologue.go`, which is neither a rule nor an encoder. This was found by
looking for the listing below in `ssa/` and finding it under `ssagen/`.

| Part | File |
| --- | --- |
| Lowering rules | `ssa/rules/arm64.go` |
| Machine operation set | `ssa/macharm64.go` |
| Register description | `ssa/target.go`, over `obj/arm64/arm64.go`'s tables |
| Encoders | `obj/arm64/encode.go`, `obj/arm64/float.go` |
| Prologue, frame, growstack tail | `ssagen/prologue.go` |

The distinction matters for [000](000-decisions.md) decision 5. A rule set and
an encoder are what a second target is supposed to supply. `ssagen` sits above
the target boundary and holds target-specific code anyway, so
[043](043-amd64-backend.md) has to edit it, and decision 5 does not predict
that. The line audit found the same gap from the other side:
`ssagen` is 2,634 lines and decision 10's accounting has no estimate to weigh
them against, so they are spent and unbudgeted.

## Register description

From [030](030-abi.md), with the allocation view:

| Registers | Allocatable | Note |
| --- | --- | --- |
| R0 – R15 | yes | also the integer argument and result registers |
| R19 – R25 | yes | scratch, no ABI meaning |
| F0 – F15 | yes | also the floating-point argument and result registers |
| F16 – F29 | yes | scratch, no ABI meaning |
| F30, F31 | **no** | held back for materialisation, as R16 and R17 are |
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

The floating-point registers arrived with group 6. An earlier version of this
table marked them deferred, because rule groups 1 to 5 need none of them.

F30 and F31 are held back for the same reason R16 and R17 are, and it is not
the linker's: [026](026-register-allocation.md) reserves a pair per class for
materialisation, so that a spilled value can always be read back without
spilling something else first. The pair is taken from the top of the file
because [030](030-abi.md) gives no role to any register above F15.

A note on how the reserved set is checked, because the obvious property is the
wrong one. The test asserts that **no allocatable register encodes as 18**, not
that no encoder can emit 18. The second would be false: the prologue above
legitimately names R16 and `g`, which are also not allocatable.

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
    MOVD  16(g), R16            // stackguard, unless nosplit or a small leaf
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
    MOVD  R30, R3               // morestack reads the caller's return address here
    CALL  runtime.morestack_noctxt(SB)
    ... restore ...
    JMP   0                     // re-execute the function
```

The `MOVD R30, R3` is a correction. `runtime.morestack` reads the caller's
return address from R3, which `runtime/asm_arm64.s` states in a comment on the
entry point, and an earlier version of this listing omitted it. The order is
load-bearing too: R3 is also the fourth argument register, so the arguments are
saved before it is overwritten.

The callee is not spelled in the emitter. `runtime.morestack_noctxt` comes from
`rtsym`, which [031](031-runtime-lowering.md) owns and which is checked against
the runtime's own source rather than typed in: 45 symbols against 2,435 runtime
functions, with `morestack_noctxt` the one entry that has no Go declaration and
is checked against `runtime/asm_arm64.s` instead. `ssagen`'s
`TestMorestackComesFromRtsym` gates it.

### The prologue has four forms, not one

The listing above is the form for a mid-sized frame. The guard comparison has
three forms and the frame push has two, because both depend on how large the
frame is against an immediate range:

| Frame size | Guard | Push |
| --- | --- | --- |
| within 128 bytes | `CMP` against the guard | `MOVD.W` |
| up to 4096 bytes | `SUB` then `CMP` | `MOVD.W` up to 240 bytes |
| beyond 4096 bytes | `SUBS`, `BLO`, then `CMP` | `SUB` then `MOVD` |

The epilogue mirrors the push and is not a fourth axis: a post-indexed load up
to 240 bytes, and a load followed by an add above it.

A test that checks only one frame size checks one of them.
`ssagen`'s `TestPrologueMatchesTheAssembler` compares all four against
`go tool asm` at nine frame sizes, which is 101 instructions, and a frame whose
size and whose size minus 128 both fall outside the 12-bit immediate forms uses
the R27 expansion. The two thresholds, 128 and 4096, are `internal/abi`'s
`StackSmall` and `StackBig`, and `TestStackConstantsMatchTheRuntime` reads them
out of the runtime rather than trusting the copies here.

### The reserved words at the top of a frame

The 8 or 16 bytes at the top of a frame hold the **caller's** saved frame
pointer, not this function's. Without them a call overwrites it. So the locals
area is the frame size less that reservation, and the incoming argument area
starts at SP+8 rather than at SP. An earlier version of this spec described the
saved frame pointer in a way that read as though the reservation were this
frame's own.

Four details are load-bearing and are stated because they are easy to lose.
They were found by writing the encoder, and the first two mean an earlier
version of this listing did not assemble. The count was wrong too: the text
said three and the list below has four.

- **The goroutine register is spelled `g`, not `R28`.** Plan 9 arm64 syntax
  names it that way, and `MOVD 16(R28), R16` is not accepted. This matters again
  in [044](044-plan9-assembler.md), which has to parse the spelling.
- **`CMP R16, RSP` needs the add-and-subtract extended-register class.** In the
  shifted-register class, register 31 reads as the zero register, so there is no
  encoding of that instruction there. The extended-register class is the one
  where 31 means the stack pointer. Neither this spec nor
  [041](041-instruction-encoding.md) mentioned the class existed, and the
  prologue cannot be encoded without it. `obj/arm64`'s
  `TestPrologueIsEncodable` walks every line of this listing and asserts each
  one encodes.
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
5. Calls. The spec said four shapes, static, closure, interface and deferred,
   and there are three. `ssa/op.go` has `OpStaticCall`, `OpClosureCall` and
   `OpInterCall` and no deferred call: a `defer` does not become a call
   operation, it becomes the runtime calls of
   [033](033-closures-defer-panic.md), which is unbuilt. This was found by
   reading the call operations against the rule table.
6. Floating point. Written. The two things in it that are not a transcription
   of the integer rules are the constant, whose immediate reaches 256 values
   and nothing else, and the condition codes, which are not the integer ones:
   `FCMP` has four outcomes and an IEEE 754 comparison has to be false in the
   unordered one, so `<` is `MI` and `<=` is `LS` where the integer rules use
   `LT` and `LE`. Group 6 adds four rules of its own, the constant and three
   conversions, and reuses the rules of groups 1 to 4 for everything else,
   because arithmetic, comparison and the loads and stores differ only in the
   type of the value.
7. Atomics, which the runtime and `sync/atomic` need. **Not written.**
8. `memmove` and `memclr` inline forms for small constant sizes. **Not
   written.**

Groups 1 to 6 are written and groups 7 and 8 are not. `ssa/rules/arm64.go`,
`ssa/macharm64.go` and `obj/arm64` all stop at the same place, which is what
keeps an unencodable operation from existing.

Group 6 is written and does not reach a call boundary, and the spec said only
"Written", which reads as more than is true. `ssagen/prologue.go`'s
`valuePlaces` refuses a value the ABI would place in a floating-point register,
so a float parameter or a float result is a compile error. This was found by
reading the emitter for the call path after the rules said group 6 was done.

The gating stops at the same place. The rules, the machine operations and the
encoders are covered: `ssa/macharm64_test.go`'s `TestARM64Encoders` puts every
operation through `ARM64Encode`, and `obj/arm64/float_test.go` compares 99,368
floating-point encodings against `go tool asm`. What no test does is emit a
floating-point instruction through `ssagen` into an object. The two tests that
name a float there assert the refusal at the boundary, not the emission.

One target-neutral operation has no rule at all. `ssa/rules/arm64.go`'s
`Deferred` list names `OpConstString`, because a string constant is two words
and the decomposition pass of [025](025-lowering-and-rules.md) must split it
before selection sees it. An operation in neither the rule table nor that list
fails a test, so the list is the complete statement of what is unlowered.

Group 7 is worth its own note when it is written. `arm64` has both the
load-exclusive/store-exclusive loop and, on later revisions,
single-instruction atomics. The exclusive-loop form is the one to emit
unconditionally, because selecting the newer form needs a runtime feature check
and the difference is performance only.

## Operating system specifics

`darwin/arm64` is the first configuration. What is specific to it:

- R18 is reserved by the platform, as above.
- The object file is Mach-O, which matters to [045](045-linker.md) and not here.
- The system call convention is the platform's and reaches nanogo only through
  the runtime's assembly, which nanogo compiles at G3.

`linux/arm64` differs in the reserved register and in the linker's output format.
It is not in this deck's scope but nothing here excludes it.

## Testing

What is gated today:

- **Differential disassembly** per [041](041-instruction-encoding.md): 981,124
  encodings agree with `go tool asm`, summed over 43 tests.
- **Source text to a running process.** `ssagen`'s `TestLinkAndRun` compiles a
  function with nanogo, links it against the real Go runtime with
  `go tool link`, and runs it. 18 cases.
- **The prologue against the assembler.** `TestPrologueMatchesTheAssembler`,
  above.
- **Stack growth.** `TestStackGrowthCopiesNanogoFrames` recurses 200,000 frames
  under `GODEBUG=gccheckmark=1`, so a frame the copier read with the wrong map
  is a crash rather than a wrong answer elsewhere.

What this section asked for and does not have:

- **Differential execution over the whole corpus** ([004](004-conformance.md)
  L3). 18 hand-written cases is not a corpus. The blocker is above this spec:
  SSA construction accepts two functions in five of the distribution, so there
  is no body of programs to run yet.
- **A debug build asserting RSP is 16-byte aligned at every call.** No such
  build exists. What exists is static: `ssagen`'s `checkFrame` refuses a frame
  size that is not a multiple of 16, so the alignment is a property of the
  layout rather than a runtime assertion. That is weaker in one way, it cannot
  catch an instruction that moves the stack pointer outside the prologue, and
  stronger in another, it fails at compile time on every function rather than
  on the paths a test runs.
