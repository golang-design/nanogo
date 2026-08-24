---
title: "Debug information: tracebacks first, DWARF second"
status: draft
layer: back end
gate: "traceback metadata at G1, DWARF at G3"
depends_on:
  - 040-object-format.md
  - 045-linker.md
---

# Debug information

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

| Item | Source |
| --- | --- |
| Function name, start, size | the symbol |
| File and line pc-value table | positions from [010](010-scanner-and-positions.md) |
| Inline tree and `PCDATA_InlTreeIndex` | [024](024-inlining-and-devirtualization.md) |
| Frame size, argument size | [030](030-abi.md) |
| `PCDATA_UnsafePoint`, `PCDATA_StackMapIndex` | [027](027-liveness-and-stackmaps.md) |

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
2. **Attribute a statement's first instruction to the statement.** The `is_stmt`
   marking in the line table is what lets a debugger stop at a line once rather
   than several times.

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

- The link under the `dwarf5` experiment, forced on whatever the host is. Every
  platform-divergent failure in this compiler so far has cost a continuous
  integration round trip to find, and a linker pass one host skips is invisible
  on it. `ssagen`'s `TestLinksUnderTheDwarf5Experiment` builds a standard
  library with the experiment and links against it.
- The structural check over a text symbol's auxiliary list: every entry
  `cmd/link` reads without checking is written down with the failure its
  omission causes, and asserted on each shape of function. This catches a
  missing entry; only the link above catches a pass nobody has enumerated yet.
- Panic in a deeply nested, partly inlined call chain; compare the printed stack
  against the same program built by `gc`. Function names and line numbers must
  match.
- `runtime.Callers` and `runtime.CallersFrames` over the same chain.
- A CPU profile of a nanogo-built binary, checked for attribution to real
  functions and lines.
- `delve` stepping through a nanogo-built binary: breakpoint on a line, inspect a
  parameter, step over a call. This is the DWARF gate, and it is a manual test
  that is worth writing down as a checklist rather than automating badly.
