---
title: "Instruction encoding"
status: draft
layer: back end
gate: G1
depends_on:
  - 040-object-format.md
---

# Instruction encoding

Turning a machine operation and its operands into bytes. This is the work
[000](000-decisions.md) decision 3 accepted in exchange for controlling the
symbol namespace.

## Scope

The reference implementation's `arm64` encoder is 36,224 lines. That number
describes a different job: encoding every instruction in the architecture,
accepting every operand form the assembler's syntax allows, and expanding pseudo
instructions. Most of it is generated tables.

nanogo encodes the instructions its own code generator emits. That set is fixed
by [025](025-lowering-and-rules.md)'s rules and is on the order of a hundred
instructions per target, each with a small number of operand forms.

The set grows only when a rule is added, so the two files change together and a
rule with no encoder is a build failure rather than a crash.

## Structure

One package per target, with one function per instruction form:

```go
func addRegReg(dst, a, b Reg) uint32
func addRegImm(dst, a Reg, imm int64) (uint32, bool)   // false if imm does not fit
```

Returning "does not fit" rather than panicking is deliberate.
[025](025-lowering-and-rules.md)'s rules are responsible for choosing a form that
fits, and the encoder is the check that they did. A rule that emits an
out-of-range immediate is a compiler bug caught at the encoder, with the
instruction and the value named.

## Immediates and their ranges

Immediate ranges are the most common source of encoder bugs because they differ
per instruction in ways that are easy to assume away.

On `arm64`, for example: arithmetic immediates are 12 bits with an optional
12-bit left shift; logical immediates use a bitmask encoding that can represent
only certain patterns; load and store offsets are 12 bits scaled by the access
size, or 9 bits unscaled and signed; branch offsets are 26 bits for `B` and 19
bits for conditional branches.

Each range is a constant in the encoder and a condition in the corresponding
rule. They are written once and referenced, never repeated.

## Branch ranges and relocation

A branch whose target is too far to encode needs a trampoline. The linker
inserts them, using the scratch registers [030](030-abi.md) reserves for it —
R16 and R17 on `arm64` — which is one of the reasons those registers are never
allocated.

The compiler's part is to emit a relocation of the right type and let the linker
decide. It does not measure distances to symbols in other packages, because it
cannot know them.

## Relocations

Every reference to a symbol becomes a relocation: an offset in the symbol's data,
a size, a type, an addend, and a target. The types are the linker's
(`R_CALLARM64`, `R_ADDRARM64`, `R_PCREL`, and the rest), and they are used as
`gc` uses them, by [000](000-decisions.md) decision 11.

Relocation types encode how the linker patches the instruction, so a wrong type
produces a binary that jumps somewhere plausible. This is a class of bug that
`go tool objdump` finds immediately and that running the program finds slowly, so
disassembly is part of the test.

## Assembling a function

1. Lay out values in scheduled order, computing each instruction's size.
2. Resolve local branch targets, now that offsets are known.
3. Emit bytes, collecting relocations.
4. Emit the pc-value tables of [040](040-object-format.md) alongside, since they
   are indexed by the offsets step 3 produced.

Steps 1 and 2 iterate when an instruction's size depends on a distance that
depends on sizes. On `arm64` all instructions are 4 bytes and the iteration is
not needed. On `amd64` it is, and [043](043-amd64-backend.md) owns it.

## Testing

- **Differential disassembly.** For every encoder function, over a generated
  sweep of operand values, compare nanogo's bytes against what `go tool asm`
  produces for the equivalent instruction. This is an exact oracle and it should
  be exhaustive over the operand ranges, not sampled.
- Range rejection: every immediate form tested one past its limit, asserting the
  encoder reports the overflow rather than truncating.
- `go tool objdump` on nanogo's objects, checked against the intended
  instruction sequence.
- Rule coverage from [025](025-lowering-and-rules.md): every operation a rule can
  emit has an encoder.
