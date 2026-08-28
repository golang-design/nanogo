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

The backend is a lowering rule set, a machine operation set, a register
description, a set of encoders, and one emitter. Everything general is above
it.

## Where the backend is

Five parts, in the files below. The part that is neither a rule nor an encoder
is `ssagen/prologue.go`, which emits the prologue, the epilogue, the frame
layout and the stack-growth tail.

| Part | File |
| --- | --- |
| Lowering rules | `ssa/rules/arm64.go` |
| Machine operation set | `ssa/macharm64.go` |
| Register description | `ssa/target.go`, over `obj/arm64/arm64.go`'s tables |
| Encoders | `obj/arm64/encode.go`, `obj/arm64/float.go`, `obj/arm64/condsel.go` |
| Prologue, frame, growstack tail | `ssagen/prologue.go` |

The distinction matters for [000](000-decisions.md) decision 5. A rule set and
an encoder are what a second target is supposed to supply. `ssagen` sits above
the target boundary and holds target-specific code anyway, so
[043](043-amd64-backend.md) has to edit it, and decision 5 does not predict
that. [002](002-architecture.md) records the same gap from the other side: `ssagen`
is 3,241 non-test lines and was in no package layout and no spec.

## Register description

From [030](030-abi.md), with the allocation view:

| Registers | Allocatable | Note |
| --- | --- | --- |
| R0 – R15 | yes | also the integer argument and result registers |
| R19 – R24 | yes | scratch, no ABI meaning |
| F0 – F15 | yes | also the floating-point argument and result registers |
| F16 – F29 | yes | scratch, no ABI meaning |
| F30, F31 | **no** | held back for materialisation, as R16, R17 and R25 are |
| R16, R17 | **no** | linker trampolines, and two of the three integer materialisation registers |
| R25 | **no** | the third integer materialisation register |
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

The floating-point registers belong to group 6. Rule groups 1 to 5 need none of
them.

F30 and F31 are held back for the same reason R16, R17 and R25 are, and it is
not the linker's: [026](026-register-allocation.md) reserves per class as many
registers as the widest instruction reads operands, so that a spilled value can
always be read back without spilling something else first. The floating-point
pair is taken from the top of the file because [030](030-abi.md) gives no role
to any register above F15.

The integer file needs three and the floating-point file needs two, and the
difference is what the widest instruction of each file reads. `MOVDstoreidx8`
reads a base, an index and the value to store, and `MADD` and `MSUB` read
three. The indexed floating-point stores read three registers as well, and two
of them are the address, which is an integer, so no operation reads three
registers of the floating-point file. `TestArm64ScratchCoversTheOperationTable`
walks the operation table and computes both numbers, so an operation added with
a wider operand list fails a test rather than a program.

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

The `MOVD R30, R3` is required. `runtime.morestack` reads the caller's return
address from R3, which `runtime/asm_arm64.s` states in a comment on the entry
point. The order is significant too: R3 is also the fourth argument register, so
the arguments are saved before it is overwritten.

The callee is not spelled in the emitter. `runtime.morestack_noctxt` comes from
`rtsym`, which [031](031-runtime-lowering.md) owns and which is checked against
the runtime's own source rather than typed in: 70 symbols against 2,435 runtime
functions, with `morestack_noctxt` the one entry that has no Go declaration and
is checked against `runtime/asm_arm64.s` instead. `ssagen`'s
`TestMorestackComesFromRtsym` gates it.

### The prologue has four forms

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

### The frame address has the same two forms

`ADDframe` is the address of a frame object, and its offset is the frame
layout's: no pass above the code generator knows it, so no rule can choose a
form that fits. The offset is therefore checked where the instruction is built,
against the same 12-bit immediate with an optional 12-bit shift the prologue
checks against. A frame object further away than that reaches uses the R27
expansion, which is what `go tool asm` produces for

```
	ADD	$4136, RSP, R0    ->    MOVD $4136, R27
	                              ADD  R27, RSP, R0
```

Both the definition of the address and its rematerialisation
([026](026-register-allocation.md) recomputes one at each use) go through
`addSP`, so the two spell the same address. The rematerialisation is then one
or two instructions rather than one, which nothing downstream counts: a
constant already took up to four.

`rtsym.init` is the frame that found this. It builds every runtime symbol in
one function, and its locals reach past 4096 bytes.

### The reserved words at the top of a frame

The 8 or 16 bytes at the top of a frame hold the **caller's** saved frame
pointer, not this function's. Without them a call overwrites it. So the locals
area is the frame size less that reservation, and the incoming argument area
starts at SP+8 rather than at SP.

Four details are required and are easy to lose. The first two are what make the
listing assemble at all, and both were found by writing the encoder.

- **The goroutine register is spelled `g`, not `R28`.** Plan 9 arm64 syntax
  names it that way, and `MOVD 16(R28), R16` is not accepted. This matters again
  in [044](044-plan9-assembler.md), which has to parse the spelling.
- **`CMP R16, RSP` needs the add-and-subtract extended-register class.** In the
  shifted-register class, register 31 reads as the zero register, so there is no
  encoding of that instruction there. The extended-register class is the one
  where 31 means the stack pointer, and the prologue cannot be encoded without
  it. `obj/arm64`'s `TestPrologueIsEncodable` walks every line of this listing
  and asserts each one encodes.
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
5. Calls, in three shapes. `ssa/op.go` has `OpStaticCall`, `OpClosureCall` and
   `OpInterCall` and no deferred call, and that is correct: a `defer` never
   becomes a call operation. It becomes a static call to `runtime.deferproc`
   and one to `runtime.deferreturn`, which
   [033](033-closures-defer-panic.md) owns.
6. Floating point. The two things in it that are not a transcription of the
   integer rules are the constant, whose immediate reaches 256 values
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

**Group 6 reaches an object.** `ssagen` refused a floating-point value at three
doors, `incoming` and `reg` in `ssagen/ssagen.go` and `valuePlaces` in
`ssagen/prologue.go`, and none of the three remains. What each needed is the
class of a register or of a type where the code generator had assumed the
integer file:

| Was refused | Now |
| --- | --- |
| a parameter the ABI places in a floating-point register | `memOpFor` takes the type rather than the width, so the stack-growth tail saves F0 with `STR (D)` |
| a call-site operand and a result | `valuePlaces` places a floating-point word like any other; only a value of more than one word is still refused |
| any value the allocator puts in a floating-point register | `reg` converts both files, `copyReg` copies with `FMOV`, and `spill` and `load` use the store and the load of the type |

Three places in the code generator carry a register file rather than a width.
`copyReg` picks `FMOV` or `MOV` from the register and refuses a copy across the
files, because such a copy is a class the allocation and the convention
disagree about and `FMOVgpfp` here would compute an integer's bits as a float.
`permute` splits a parallel move into one per file and breaks each cycle with
that file's own scratch register, F31 for `ClassFloat` as R17 is for
`ClassInt`, since an integer register cannot hold a float. `memOpFor` asks
`ssa.ARM64StoreOpForType` rather than `ssa.ARM64StoreOp`, which is the single
answer for a spill, a reload, an outgoing argument and the stack-growth tail.
`mem` also checks that the transferred register is in the file the operation
transfers, because obj/arm64 panics on the mismatch rather than reporting it.

### The move table

`move` covers every pair of locations. A register to a register is `copyReg`, a
slot to a register is the load, a register to a slot is the store, and nothing
to a register is the recomputation [026](026-register-allocation.md) allows
instead of a home. Two of the pairs have no instruction on this machine and are
each a staged pair:

| From | To | What is emitted |
| --- | --- | --- |
| nothing | a slot | the recomputation into `scratchFor(type)`, then the store |
| a slot | a slot | the load into `scratchFor(type)`, then the store |

`scratchFor` reads the type and not the width, so a `float64` stages through
F31 and stores with `STR (D)`, and a `uint8` moves eight bits and not
sixty-four. The register is the second of the file's reserved pair, because
[026](026-register-allocation.md)'s phi cycle breaking holds a value in the
first across the copies a broken cycle became.

The destination with no kind is what the table's last arm now catches. It is a
pass that lost the destination and not one that named an unusual pair.

The gating follows. `ssa/macharm64_test.go`'s `TestARM64Encoders` puts every
operation through `ARM64Encode` and `obj/arm64/float_test.go` compares 99,368
floating-point encodings against `go tool asm`, as before.
`TestFloatRegistersReachTheEncoder` and `TestGrowstackSpillsAFloatParameter`
now assert the emission where they asserted the refusal, and seven of
`TestLinkAndRun`'s cases compute in floating point, link against the real
linker and run: arithmetic with no float in the signature, a constant `FMOV`'s
immediate cannot name, a `float64` and a `float32` parameter and result, floats
between integers, a float live across a call, and a float through a call gc
compiled.

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

- **Differential disassembly** per [041](041-instruction-encoding.md): 998,947
  encodings agree with `go tool asm`. The figure is the encoder package's own
  count, printed by its `TestMain`, and never a sum of the per-test log lines.
- **Source text to a running process.** `ssagen`'s `TestLinkAndRun` compiles a
  function with nanogo, links it against the real Go runtime with
  `go tool link`, and runs it. 25 cases, seven of them in floating point.
- **The prologue against the assembler.** `TestPrologueMatchesTheAssembler`,
  above.
- **Stack growth.** `TestStackGrowthCopiesNanogoFrames` recurses 200,000 frames
  under `GODEBUG=gccheckmark=1`, so a frame the copier read with the wrong map
  is a crash rather than a wrong answer elsewhere.

What this section asked for and does not have:

- **Differential execution over the whole corpus** ([004](004-conformance.md)
  L3). 18 hand-written cases is not a corpus. The harness is no longer the
  blocker: `internal/gotest` sweeps Go's own 356 vendored files, carries out the
  single-file recipes with `gc` as the oracle, and files every other file under
  a named class. 10 of the 356 are **matched**, which is the only class that
  proves code generation: nanogo compiled the program, it ran, and its output
  and exit status are `gc`'s. 3 more are compiled without being run, and 74 are
  rejected, which exercises the checker and not the back end. The blocker is
  above this spec: the compiler refuses the rest, and 40,385 of the
  distribution's 41,354 functions get past SSA construction once the lowering
  pass has run, which is what the driver does. The 10/3/74 split lives in
  `internal/gotest/testdata/ratchet.txt`, not in
  `internal/hygiene/testdata/facts.json`, so it is ratcheted and not gated on a
  number. Their sum is the 87 [004](004-conformance.md) states, and only the sum
  is checked against the ratchet.
- **A debug build asserting RSP is 16-byte aligned at every call.** No such
  build exists. What exists is static: `ssagen`'s `checkFrame` refuses a frame
  size that is not a multiple of 16, so the alignment is a property of the
  layout rather than a runtime assertion. That is weaker in one way, it cannot
  catch an instruction that moves the stack pointer outside the prologue, and
  stronger in another, it fails at compile time on every function rather than
  on the paths a test runs.

## What was wrong

The spec said the backend is a rule set, a register description and a set of
encoders, and nothing else. `ssagen/prologue.go` is a fifth part that names the
target, which is the counterexample [000](000-decisions.md) decision 5 records.
This was found by looking for the prologue listing in `ssa/` and finding it
under `ssagen/`.

The register table marked the floating-point registers deferred, which was true
only while rule groups 1 to 5 were the whole rule set.

The prologue listing omitted `MOVD R30, R3`, so `runtime.morestack` would have
read the caller's return address from a register nothing wrote. The listing also
did not assemble: it spelled the goroutine register `R28` where Plan 9 arm64
syntax needs `g`, and it used `CMP R16, RSP` without the add-and-subtract
extended-register class, which neither this spec nor
[041](041-instruction-encoding.md) said existed. The list of details under that
listing was introduced as three items and holds four.

The spec described the reserved words at the top of a frame as though the saved
frame pointer were this function's. It is the caller's.

The spec gave calls four shapes, static, closure, interface and deferred. There
are three, because a `defer` becomes a static call to `runtime.deferproc` and
not a call operation. This was found by reading `ssa/op.go`'s call operations
against the rule table.

**The spec said group 6 refuses a float at the call boundary, and the refusal is
total.** It named `valuePlaces` alone, so a float local read as though it
compiled. `reg` refuses every value the allocator puts in a floating-point
register, which is what makes a float local, and a package-level float
variable, a compile error too.

**The spec said there is no body of programs to run.** `internal/gotest` sweeps
Go's own corpus now. The 10 matched files are programs nanogo compiled and ran
against `gc`'s output, which is [004](004-conformance.md) L3 on the files the
compiler accepts.

The `rtsym` count was 45 and the table holds 70
([031](031-runtime-lowering.md)).

The encoding total was described as a sum over 43 tests. It is the encoder
package's own count and a sum of the log lines is a different, smaller number,
which [041](041-instruction-encoding.md) records.
