---
title: "Debug information: tracebacks first, DWARF second"
status: in progress
layer: back end
gate: "traceback metadata at G1, DWARF at G3"
depends_on:
  - 040-object-format.md
  - 045-linker.md
---

# Debug information

**One half of this spec is built and the other half is not.** Traceback
metadata is emitted and gated. DWARF is not written at all: there is no `dwarf`
package, no DIE is constructed, and no `.debug_*` section content exists. The
one DWARF symbol nanogo emits is empty, and the section below explains why an
empty one is mandatory.

What exists, by name:

| Item | Where | Gated by |
| --- | --- | --- |
| `AuxFuncInfo`: frame size, argument size, file table | `ssagen/ssagen.go` | `TestAddWritesTheAuxiliarySymbols` |
| `Pcsp`, `Pcfile`, `Pcline` | `ssagen/ssagen.go` | presence by `TestEveryFunctionCarriesWhatTheLinkerReads`; `Pcsp` semantics by `TestStackDeltaTakesEffectAfterTheAdjustment` and, at runtime, by `TestStackGrowthCopiesNanogoFrames` |
| `PCDATA_UnsafePoint`, `PCDATA_StackMapIndex` | `ssagen/stackmap.go` | `ssagen/stackmap_test.go`, and the corpus of [027](027-liveness-and-stackmaps.md) |
| An empty `go:info.<sym>` subprogram symbol | `ssagen/ssagen.go` | `TestLinksUnderTheDwarf5Experiment` |

Two separate things are called debug information in a Go binary, and confusing
them causes the wrong one to be prioritised.

| | Traceback metadata | DWARF |
| --- | --- | --- |
| Consumer | the Go runtime | `delve`, `lldb`, `gdb` |
| Contains | function bounds, line tables, inline trees, pc-value tables | types, variable locations, scopes, line tables |
| Where | `pclntab`, from `AuxFuncInfo` | DWARF sections |
| If missing | panics have no stack, `runtime.Caller` fails, profiles are blank | no source-level debugger |
| Priority | **required at G1** | useful at G3 |

Traceback metadata is not optional and is not a debugging convenience. It is how
the runtime works. A panic in a binary without it prints nothing usable, and
debugging the compiler itself becomes guesswork at exactly the milestone where
that matters most.

## Traceback metadata

Produced by the compiler into `AuxFuncInfo` ([040](040-object-format.md)) and
assembled by the linker into `pclntab` ([045](045-linker.md)). The compiler's
contribution:

| Item | Source | Emitted |
| --- | --- | --- |
| Function name, start, size | the symbol | yes |
| File and line pc-value table | positions from [010](010-scanner-and-positions.md) | yes |
| Inline tree and `PCDATA_InlTreeIndex` | [024](024-inlining-and-devirtualization.md) | no |
| Frame size, argument size | [030](030-abi.md) | yes |
| `PCDATA_UnsafePoint`, `PCDATA_StackMapIndex` | [027](027-liveness-and-stackmaps.md) | yes |

The emitted column is a correction. The spec listed five items as the
compiler's contribution and nanogo writes four. There is no
`PCDATA_InlTreeIndex` stream and no inline tree, because
[024](024-inlining-and-devirtualization.md) is unbuilt and there is nothing to
index. This was found by reading the pc-value index constants in
`ssa/stackmap.go`, which declares two, against this table.
[040](040-object-format.md) carries the same correction on its own table.

The file table has a narrower shape than the words above imply. `ssagen` writes
one file entry per function rather than a stream, so a function whose
instructions come from more than one file is attributed to the first. That is
correct for every function nanogo compiles today, because only inlining and
`//line` produce the other case and neither reaches the emitter.

The inline tree is the part most easily skipped and least easily lived without. A
traceback through inlined frames that names only the outermost function makes
every runtime bug harder to read, and [003](003-sequencing.md)'s M4 and M9 are
spent reading runtime bugs.

## Line attribution

Every instruction gets the position of the IR node it came from. Two rules keep
the result useful rather than merely present:

1. **Never attribute to position zero.** A synthesised node takes the position of
   whatever it was synthesised for. [010](010-scanner-and-positions.md) states
   this and here is where the consequence appears: a zero line makes a debugger
   step into nothing and a profile attribute time to nowhere.

   `ssagen`'s `markLine` holds the rule by a different mechanism than the one
   this spec described. It does not require every node to carry a position. It
   falls back to the function's own line when a position is unknown, so a
   synthesised instruction is attributed to the function rather than to line
   zero. The outcome is the one the rule wants and the guarantee is weaker: a
   whole prologue attributed to the function's line is right, and a body
   instruction that lost its position is wrong in a way nothing reports.
2. **Attribute a statement's first instruction to the statement.** The `is_stmt`
   marking in the line table is what lets a debugger stop at a line once rather
   than several times. **Not implemented.** `markLine` writes a line number and
   no statement flag. This costs nothing today because no debugger reads a
   nanogo binary, and it is the first thing the DWARF work below needs.

## The empty subprogram symbol, which is not optional

Before any of the above, one DWARF symbol is mandatory at G1, and the reason is
the linker rather than a debugger.

`cmd/link/internal/ld.writedebugaddr` walks every text symbol of a compilation
unit and reads the relocations of its `AuxDwarfInfo` symbol **without checking
that there is one**; the two lines beside it check the range and the location
symbol and this one does not. A text symbol with no such entry resolves to
symbol 0, and the linker panics with `trying to get oreader for invalid sym 0`.

The pass runs only under the `dwarf5` experiment, and `internal/buildcfg` puts
that in the baseline for every target **except darwin, ios and aix**. So an
object without the entry links on darwin and links nowhere else. A compilation
unit is per package, and a package half compiled by `gc` is one unit, so the
mixture that reaches the pass is the ordinary one.

nanogo therefore emits an `SDWARFFCN` symbol named `go:info.<sym>` for every
function, holding no bytes. A unit's function DIEs are the contents of these
symbols concatenated, so an empty one contributes no DIE: the unit describes
the functions `gc` compiled and not nanogo's. That is the honest state until
this spec's own milestone, and a DIE invented earlier would be a wrong one
rather than a missing one.

`gc` omits the entry when its symbol is empty, so `gc` never produces this
shape and the gap upstream was never reached.

## DWARF

Version 4, which is what the Go tools emit and what `delve` reads.

| Emitted | Contents |
| --- | --- |
| `.debug_info` | one compilation unit per package; a DIE per function, parameter, local, and type |
| `.debug_line` | the line table, derived from the same positions as `pclntab`'s |
| `.debug_frame` | unwinding rules, from the frame layout of [027](027-liveness-and-stackmaps.md) |
| `.debug_loc` | variable locations, which change as the register allocator moves values |

`.debug_loc` is the expensive one and the one that degrades gracefully. A
variable with no location entry shows as optimised out; a variable with a *wrong*
one shows the wrong value, which is worse than nothing. When the allocator's
information is not confidently available, nanogo emits no entry.

## Ordering

DWARF is scheduled at G3 rather than G1, and traceback metadata at G1, because
of the table at the top: one is required for the runtime to function and the
other is required for a debugger to be pleasant.

## Testing

Written and gated:

- The link under the `dwarf5` experiment, forced on whatever the host is. Every
  platform-divergent failure in this compiler so far has cost a continuous
  integration round trip to find, and a linker pass one host skips is invisible
  on it. `ssagen`'s `TestLinksUnderTheDwarf5Experiment` builds a standard
  library with the experiment and links against it.
- The structural check over a text symbol's auxiliary list: every entry
  `cmd/link` reads without checking is written down with the failure its
  omission causes, and asserted on each shape of function.
  `TestEveryFunctionCarriesWhatTheLinkerReads` is that check. It catches a
  missing entry; only the link above catches a pass nobody has enumerated yet.
- The pc-value encoding against `gc`'s own bytes. `obj`'s
  `TestPCDataMatchesCompiler` takes the entries
  `go tool compile -d=pctab=pctospadj` prints, re-encodes them and compares.
  That gates the delta scheme, not nanogo's line and file streams: nanogo and
  `gc` do not lay out the same instructions, so there is nothing to compare
  those against. nanogo's own streams are checked by shape rather than by
  oracle: `TestTablesAreIndexedByPosition` asserts that each pc-value symbol is
  anonymous and marked, and `TestEveryFunctionCarriesWhatTheLinkerReads`
  asserts that no function reaches an object without all three.

  `Pcsp` is the exception and has a real proof.
  `TestStackDeltaTakesEffectAfterTheAdjustment` checks that a row lands after
  the instruction that moved the stack pointer, and
  `TestStackGrowthCopiesNanogoFrames` grows and copies 200,000 nanogo frames,
  which the runtime cannot do if the stream is wrong.

Not written, and each waits on something this spec does not own:

- Panic in a deeply nested, partly inlined call chain, compared against the same
  program built by `gc`. Waits on [024](024-inlining-and-devirtualization.md),
  because there are no inlined frames to print.
- `runtime.Callers` and `runtime.CallersFrames` over the same chain.
- A CPU profile of a nanogo-built binary, checked for attribution to real
  functions and lines.
- `delve` stepping through a nanogo-built binary: breakpoint on a line, inspect a
  parameter, step over a call. This is the DWARF gate, and it is a manual test
  that is worth writing down as a checklist rather than automating badly. It
  waits on DWARF itself, which is unwritten.
