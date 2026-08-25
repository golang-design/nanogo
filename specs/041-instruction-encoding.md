---
title: "Instruction encoding"
status: complete
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

`obj/arm64` draws its line where the rules draw theirs. It encodes groups 1 to
6 of [042](042-arm64-backend.md) and no atomic instruction and no inline
`memmove` form, which are groups 7 and 8. `ssa/macharm64.go` and
`ssa/rules/arm64.go` stop at the same place, so no operation exists that no
encoder can take.

## Structure

One package per target, with one function per instruction form:

```go
func AddRegReg(size Size, dst, a, b Reg) uint32
func AddRegImm(size Size, dst, a Reg, imm int64) (uint32, bool)  // false if imm does not fit
```

The leading `size` is a correction. An earlier version of this spec omitted it,
and a code generator needs both the 32-bit and 64-bit form of every arithmetic
and logical instruction. Carrying the width as a parameter rather than doubling
the function count is the cheaper of the two ways to fix it.

Returning "does not fit" rather than panicking is deliberate.
[025](025-lowering-and-rules.md)'s rules are responsible for choosing a form that
fits, and the encoder is the check that they did. A rule that emits an
out-of-range immediate is a compiler bug caught at the encoder, with the
instruction and the value named.

## Immediates and their ranges

Immediate ranges are the most common source of encoder bugs because they differ
per instruction in ways that are easy to assume away.

On `arm64`: arithmetic immediates are 12 bits with an optional 12-bit left
shift; load and store offsets are 12 bits scaled by the access size, or 9 bits
unscaled and signed; branch offsets are 26 bits for `B`, 19 bits for conditional
branches and `CBZ`, and **14 bits for `TBZ` and `TBNZ`**.

The 14-bit range is the tightest branch on the target and is the one a lowering
rule is most likely to violate. An earlier version of this spec did not name it.

**Logical immediates** deserve more than "only certain patterns". The encoding
is a replicated run of ones described by `N`, `immr` and `imms`, and three
consequences bind the rules that use it:

- **Zero and all-ones are not representable.** The run of ones can be neither
  empty nor fill the element.
- **The 32-bit forms require `N = 0`**, which removes the 64-bit-only patterns.
- **`BIC` and `TST` with an immediate do not exist.** `BIC` immediate is `AND`
  of the complement, so a rule must range-check the **complement's**
  representability, not the value's. `TST` is `ANDS` into the zero register.

Each range is a constant in the encoder and a condition in the corresponding
rule. They are written once and referenced, never repeated.

## Branch ranges and relocation

A branch whose target is too far to encode needs a trampoline. The linker
inserts them, using the scratch registers [030](030-abi.md) reserves for it,
R16 and R17 on `arm64`, which is one of the reasons those registers are never
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
  produces for the equivalent instruction. This is an exact oracle and it is
  exhaustive over the operand ranges, not sampled. The measured total is
  **981,124 comparisons with zero disagreements**.

  The package counts them itself. `comparisons` is an atomic that every
  comparison increments, and `TestMain` prints the total when the package
  finishes. A reader who runs one test sees a much smaller number, because no
  single test compares more than a fraction of it.

  **This number was corrected twice, and the second correction was wrong.** An
  audit summed the per-test log lines, got 913,069, and changed this spec to
  say so. The sum is smaller than the truth because not every comparison
  happens inside a test that logs a count, and the audit had also double
  counted `TestMain`'s own line in one variant of the same sum. 981,124 is the
  figure the package reports and it is the one to quote.

  The lesson is worth more than the number. A total that is reconstructed by
  adding up log lines is a second implementation of the count, and the two
  implementations disagreed. `internal/hygiene` now reads `TestMain`'s line,
  which is the count itself rather than a reconstruction of it.

  Two traps in reading `go tool asm -S`, both of which produce a comparison that
  passes while testing nothing:

  1. **The listing prints one line per source instruction, before expansion.**
     `ADD $4097, R2, R3` is one line and eight bytes. Instruction size must be
     computed as the distance to the next instruction's offset, and each
     comparison must assert the source line produced exactly four bytes.
     Counting listing lines compares against the first quarter of an expansion.
  2. **The `TEXT` flags matter.** `NOSPLIT|NOFRAME` is needed, or a body
     containing a call grows a prologue in front of the instruction under test.

  Where a range cannot be reached through the assembler, and `B`'s 26-bit range
  cannot, the encoding is checked by recovering the offset from the encoded word
  and sign-extending it back, rather than by restating the encoder's own
  expression.

  There is a third case of that, and it is an assembler defect rather than a
  syntax gap. `FMOV` with a floating-point immediate encodes an 8-bit field,
  and `cmd/internal/obj/arm64/obj7.go` chooses the immediate form with
  `chipfloat7(f64) > 0` where the field's zero is a legal encoding. So the one
  value whose field is zero, `+2.0`, assembles as a PC-relative load from a
  constant symbol, while its negative and the other 255 values take the
  immediate form. That value is checked by recovering the field and expanding
  it through the manual's `VFPExpandImm`.
- Range rejection: every immediate form tested one past its limit, asserting the
  encoder reports the overflow rather than truncating.
- `go tool objdump` on nanogo's objects, checked against the intended
  instruction sequence.
- Rule coverage from [025](025-lowering-and-rules.md): every operation a rule can
  emit has an encoder.
