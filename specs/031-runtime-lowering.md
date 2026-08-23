---
title: "Runtime lowering: the calls the compiler generates"
status: draft
layer: runtime interface
gate: G1
depends_on:
  - 020-ir.md
  - 030-abi.md
---

# Runtime lowering

[020](020-ir.md)'s table says which Go constructs become runtime calls. This spec
is the other half: the symbols, their signatures, and the rules for calling them.

The runtime is code nanogo did not write and cannot change. Every entry here is a
fixed contract.

## The symbol table

Symbol names and signatures are held in one package, `rtsym`, generated from the
runtime source of the pinned Go release rather than typed in. A signature that
drifts from the runtime is a crash with no diagnostic, and a generated table
turns that into a build failure.

### Allocation

| Symbol | Called for |
| --- | --- |
| `runtime.newobject` | `new`, escaping composite literals |
| `runtime.newarray` | escaping arrays |
| `runtime.makeslice`, `makeslice64` | `make([]T, n)` |
| `runtime.makemap`, `makemap64`, `makemap_small` | `make(map[K]V)` |
| `runtime.makechan`, `makechan64` | `make(chan T)` |
| `runtime.growslice` | `append` beyond capacity |

### Maps

| Symbol | Called for |
| --- | --- |
| `runtime.mapaccess1`, `mapaccess2` | `m[k]` in one- and two-value form |
| `runtime.mapassign` | `m[k] = v` |
| `runtime.mapdelete` | `delete(m, k)` |
| `runtime.mapiterinit`, `mapiternext` | `range` over a map |
| type-specialised variants (`mapaccess1_fast64`, …) | fast paths for common key types |

The specialised variants are an optimisation and are skipped until the general
ones work. Skipping them is a performance decision; using the wrong one is
corruption.

### Channels and select

`runtime.chansend1`, `chanrecv1`, `chanrecv2`, `closechan`, `selectgo`,
`selectnbsend`, `selectnbrecv`.

`selectgo` takes arrays of cases and returns the chosen index, which the compiler
turns into a jump table. Building those arrays in the frame, with correct GC
description, is the part that is easy to get wrong.

### Interfaces and types

`runtime.convT16`, `convT32`, `convT64`, `convTstring`, `convTslice`, `convT`,
`assertE2I`, `assertI2I`, `interfaceSwitch`, `typeAssert`.

Covered by [032](032-type-descriptors-and-itabs.md).

### Strings

`runtime.concatstring2` through `concatstring5`, `slicebytetostring`,
`stringtoslicebyte`, `stringtoslicerune`, `slicerunetostring`,
`intstring`, `decoderune`.

### Memory

`runtime.memmove`, `memclrNoHeapPointers`, `memclrHasPointers`, `memequal`,
`memequal{8,16,32,64,128}`.

`memclr` is split by whether the region contains pointers, and choosing the wrong
one leaves stale pointers visible to the collector.

### Panics

`runtime.panicIndex`, `panicSlice`, `panicdivide`, `panicnildereference`,
`goPanicIndex` and the rest of the bounds-check family.

These are called from the failure edge of a check and never return. Marking them
`no-return` matters: a block after a call to one is unreachable, and liveness
that thinks otherwise keeps values alive across every bounds check in the
program.

### Write barriers

`runtime.gcWriteBarrier` and its register-argument variants. Owned by
[034](034-write-barriers.md).

## Calling rules

1. **The ABI is ABIInternal**, per [030](030-abi.md). Runtime functions are
   ordinary Go functions.
2. **A runtime call is a safepoint.** [027](027-liveness-and-stackmaps.md)
   applies.
3. **Arguments that are pointers must be visible to the collector at the call.**
   A pointer computed into a register and passed directly is fine because it is
   also in the argument area's spill space; a pointer stored only in a
   non-pointer-typed slot is not.
4. **`no-return` symbols end the block.**
5. **Some runtime calls may not appear in some functions.** A function marked
   `//go:nowritebarrier` may not contain a write barrier, and a
   `//go:nosplit` function's callees are constrained by the nosplit budget
   ([035](035-goroutines-and-stack-growth.md)). Both are checked here, where the
   call is introduced.

## Inlined fast paths

Three constructs have a fast path that the compiler emits inline and a slow path
that calls the runtime. They are worth the complexity because they are the hot
paths of ordinary Go code:

| Construct | Inline | Call |
| --- | --- | --- |
| `append` | capacity check, store, length update | `growslice` |
| small `copy`, small `memclr` | a sequence of stores for constant sizes | `memmove`, `memclr*` |
| `len`, `cap` | a field load | never |

Everything else calls.

## Testing

- The generated symbol table checked against the runtime's actual signatures at
  build time.
- One program per row of the table, run under differential execution
  ([004](004-conformance.md) L3).
- Hosted mode ([000](000-decisions.md) decision 11) means these calls reach the
  real runtime from M3 onward, so the tests are end-to-end from the start.
