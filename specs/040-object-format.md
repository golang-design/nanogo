---
title: "Object files: writing goobj"
status: draft
layer: back end
gate: G1
depends_on:
  - 000-decisions.md
  - 032-type-descriptors-and-itabs.md
---

# Object files

nanogo's output is a Go object file in the `goobj` format, the same one
`cmd/compile` and `cmd/asm` produce and `cmd/link` consumes. This is
[000](000-decisions.md) decision 3 and decision 11.

## The decision, restated with its evidence

Two seams were possible for getting machine code out of nanogo.

**Text assembly**, emitting Plan 9 assembly and calling `go tool asm`. This would
have removed the need for an instruction encoder entirely, which is the single
largest saving available at M3.

**Object files**, emitting `goobj` directly, which needs an encoder
([041](041-instruction-encoding.md)) but controls everything.

Text assembly was tested rather than assumed, and both spikes are kept in
[`../spikes`](../spikes).

It passed the hard test. [`spikes/stackmap`](../spikes/stackmap) shows that
per-PC garbage collection stack maps are expressible in assembly text: a
hand-written `FUNCDATA` symbol with two bitmaps, selected by
`PCDATA $PCDATA_StackMapIndex`, produced exactly the two collection outcomes it
declared.

It failed the easy one. [`spikes/symbolnames`](../spikes/symbolnames) shows the
assembler rejects any symbol name containing a colon, and the entire compiler
namespace — `type:`, `go:itab.`, `go:string.` — uses one.
[032](032-type-descriptors-and-itabs.md) explains why renaming them breaks itab
pointer identity, which is a correctness failure and not a limitation.

So: object files. The encoder is the price and it is a bounded one.

## The format

```
Header      Magic "\x00go120ld", Fingerprint, Flags, block offsets
Strings     the string table
Autolib     imported packages with their fingerprints
PkgIndex    referenced packages, by index
Files       source file names
SymbolDefs  name, ABI, type, flags, size, alignment
Hashed64Defs, HashedDefs   content-addressable definitions
NonPkgDefs, NonPkgRefs     definitions and references outside the package
RefFlags    flags on referenced symbols
Hash64, Hash               content hashes
RelocIndex, AuxIndex, DataIndex   indices into the blocks below
Relocs      offset, size, type, addend, target
Aux         auxiliary symbol references, by type
Data        symbol contents
RefNames    referenced symbol names, for tools only
```

A string is a length and an offset into `Strings`. A symbol reference is a
package index and a symbol index. Slices carry a `uint32` length prefix.

The whole reader and writer in the Go tree is 1,559 lines. That is the scale of
what nanogo writes.

### Content-addressable symbols

`HashedDefs` is how the format deduplicates. A symbol whose identity is its
content — a stack map, a string literal, a type descriptor — is written with its
content hash, and the linker keeps one copy across all object files.

This is the mechanism [032](032-type-descriptors-and-itabs.md)'s canonical
naming feeds. Names make two symbols *nominally* the same; hashing makes them
*actually* the same. Both are needed: the hash merges identical content, and the
name is what a relocation refers to before the merge happens.

### Auxiliary symbols

The `Aux` block attaches metadata symbols to a function symbol by type:

| Aux type | Attached |
| --- | --- |
| `AuxGotype` | the function's type descriptor |
| `AuxFuncInfo` | frame size, arguments size, file table, pcdata offsets |
| `AuxFuncdata` | one per `FUNCDATA` index: stack maps, stack objects, open-coded defer info |
| `AuxDwarfInfo`, `AuxDwarfLoc`, `AuxDwarfRanges`, `AuxDwarfLines` | [046](046-debug-info.md) |

`AuxFuncInfo` is where the pc-value tables live, and those tables are the encoded
form of every `PCDATA` change [027](027-liveness-and-stackmaps.md) produces.

## PC-value tables

A `PCDATA` stream is a mapping from program counter to a small integer, encoded
as a delta-compressed sequence: for each change, a varint value delta and a
varint pc delta. The runtime decodes it linearly.

nanogo produces one per index it uses:

| Index | Contents |
| --- | --- |
| `PCDATA_UnsafePoint` | where the frame is inconsistent |
| `PCDATA_StackMapIndex` | which stack map bitmap is current |
| `PCDATA_InlTreeIndex` | which inlined function the pc belongs to |
| `PCDATA_ArgLiveIndex` | which argument liveness map is current |

Plus the line and file tables, which are the same encoding over source positions
from [010](010-scanner-and-positions.md).

## The fingerprint

Each object records a fingerprint of the package's export data, and each import
records the fingerprint it was compiled against. The linker checks them and
refuses a mismatched build.

This is the mechanism that makes hosted mode ([000](000-decisions.md)
decision 11) safe: a nanogo-compiled package and a `gc`-compiled importer either
agree or fail loudly.

## Version pinning

The magic string carries a version. nanogo asserts it at startup against the
pinned Go release. There is no compatibility range and no negotiation: a
mismatch is an error naming both versions.

## Writing, not reading

nanogo writes objects. It reads export data ([015](015-export-data.md)), which is
a different format carried in a different place. Reading objects is needed only
by the linker, at G2, and [045](045-linker.md) owns it.

## Testing

- `go tool nm` and `go tool objdump` on nanogo's output, compared against `gc`'s
  for the same source. Structural differences are expected; missing symbols and
  wrong types are not.
- The linker is the real test, and it is available from M3 in hosted mode.
- Determinism ([053](053-determinism.md)): the same source produces the same
  object bytes. This is the earliest place the G1 fixed point can be broken and
  the cheapest place to check it.
