---
title: "The Plan 9 assembler"
status: draft
layer: back end
gate: G3
depends_on:
  - 041-instruction-encoding.md
  - 040-object-format.md
---

# Plan 9 assembler

**This spec describes unbuilt work.** There is no `asm` package. Nothing in the
tree parses Plan 9 syntax. The tree only emits it, and always to hand to
`go tool asm` as an oracle: `ssagen/prologue_test.go`,
`obj/arm64/encode_test.go` and `obj/arm64/condsel_test.go` all write assembly
text and read back what the reference assembler made of it. That is the reverse
of what this spec builds.

The dependency this retires is therefore still live.
[000](000-decisions.md) decision 4 lists `go tool asm` as retired at G3 by this
spec, and G3 is not reached.

nanogo needs an assembler for one reason: the Go runtime is not all Go. It has
hand-written `.s` files in Plan 9 syntax, and G3 ([001](001-bootstrap-gates.md))
is compiling the distribution.

Until G3, `gc` and the `go` command assemble those files in hosted mode
([000](000-decisions.md) decision 11).

## What has to be assembled

The scope is the distribution's assembly, not the syntax's full generality.

The nearest measurement of that scope is the bootstrap closure, the 27 archives
a `func main() {}` needs and the archives a nanogo distribution ships
([054](054-distribution.md)). Eight of the 27 are refused today for their
assembly alone: `internal/abi`, `internal/cpu`, `internal/bytealg`,
`internal/chacha8rand`, `internal/runtime/atomic`, `internal/runtime/sys`,
`internal/runtime/maps` and `runtime`. Those eight are the assembler's first
corpus, and the full distribution adds `math`, `crypto` and a handful of others
above them.

That is still a real assembler. The runtime's assembly uses nearly everything the
syntax offers, because it is where the syntax's features exist for.

## The syntax

Plan 9 assembly is not the vendor's syntax. Its distinguishing features, each of
which is work:

| Feature | Meaning |
| --- | --- |
| Operand order is source, destination | `MOVD R1, R2` moves R1 into R2 |
| Pseudo-registers | `SB` static base, `FP` frame pointer, `SP` local frame, `PC` program counter |
| Named offsets | `arg+8(FP)` names an argument; the name is checked against the Go declaration |
| Pseudo-instructions | `TEXT`, `DATA`, `GLOBL`, `FUNCDATA`, `PCDATA`, `RET`, `NOP` |
| Automatic prologue | `TEXT ·f(SB), $32-16` generates the stack check and frame setup |
| Middle dot and division slash | `·` separates package from symbol, `∕` is a path separator |

The automatic prologue is the feature that makes this an assembler rather than an
encoder with a parser. `TEXT` with a frame size generates the stack-growth check,
the frame setup, and the epilogue at every `RET`, using
[035](035-goroutines-and-stack-growth.md)'s rules, the same rules the compiler's
backend uses, which is why they live in one place.

### `SP` is two things

`SP` as a pseudo-register is the top of the local frame, and `RSP` or the
hardware `SP` is the actual stack pointer. `x-8(SP)` is a named local; `8(RSP)`
is an absolute offset. Handwritten runtime assembly uses both, sometimes in one
function.

The [`stackmap` spike](../spikes/stackmap) is written in this syntax and shows
both forms in use, which makes it a small worked example of what the assembler
must accept.

### Argument name checking

`arg+8(FP)` is checked against the Go prototype: the name must match a parameter
and the offset must be that parameter's offset. This catches the most common
assembly bug, which is a frame offset that was right before a signature changed.

nanogo will implement the check. It is cheap, it needs the type information the
front end already has, and skipping it means a class of runtime bug with no
diagnostic.

## Structure

Parser, per-architecture operand parser, and the encoder of
[041](041-instruction-encoding.md), which is shared with the compiler backend.

The sharing is the point. The compiler and the assembler emit the same
instructions into the same object format, so they differ only in what produces
the instruction list.

One half of that sharing is already built and is worth naming, because it
bounds the work this spec still has. `obj` writes the object format and
`obj/arm64` encodes the instructions, both gated
([040](040-object-format.md), [041](041-instruction-encoding.md)). What is
missing is the parser, the per-architecture operand parser, and the automatic
prologue. The prologue is missing in a narrower sense than the rest:
`ssagen/prologue.go` emits [042](042-arm64-backend.md)'s four forms today, and
sharing it means lifting it out of the compiler's emitter rather than writing
it again.

## Directives that carry metadata

`FUNCDATA` and `PCDATA` in assembly source become the same object file structures
[040](040-object-format.md) describes. `DATA` and `GLOBL` define data symbols with
relocations, which [`spikes/symbolnames`](../spikes/symbolnames) demonstrates and
also demonstrates the limit of: a name containing a colon cannot be written in
this syntax at all.

That limit does not matter here, because handwritten assembly never defines such
a symbol. It mattered in [040](040-object-format.md) because the *compiler* must.

## Testing

- Assemble every `.s` file in the distribution for the target and compare the
  resulting object, symbol by symbol, against `go tool asm`'s output. This is an
  exact oracle over the exact corpus that matters.
- The argument name checker against a corpus of deliberately wrong offsets.
- The generated prologue compared against the compiler's for the same frame size,
  which must be identical.

## What was wrong

- The argument name checker was written in the present tense, "nanogo
  implements the check", which reads as a statement about the code. Nothing
  implements it, because nothing parses the syntax it would check. The tense is
  corrected and the decision is not.
- The scope named `sync/atomic` among the packages to assemble first. It is not
  in the bootstrap closure. The eight packages above are, and they are the ones
  a G3 attempt meets first.
- The absence of an `asm` package was found by listing the tree against
  [002](002-architecture.md)'s package layout.
