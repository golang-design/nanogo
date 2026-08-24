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

### The file wrapper comes first

An earlier version of this spec started the layout at the magic. That is wrong,
and it is the kind of wrong that produces a linker error with no relation to the
cause. A real object file begins with a text header:

```
go object <goos> <arch> <version> [GOARM64=...] X:<experiments>
main
!
```

The `main` line is present only for package main, and the blank line and `!`
close the header. **The linker compares the header line, not the magic**, and it
refuses the first object it reads with "not package main" when the `main` line
is absent.

A consequence for [000](000-decisions.md) decision 11's version pin: the header
carries the enabled `GOEXPERIMENT` list, and `go env GOEXPERIMENT` does not
report it. The pin therefore cannot be reconstructed. nanogo probes instead: it
assembles an empty file with the installed toolchain and reads the header line
and the magic out of the result. [050](050-driver.md)'s driver calls that probe.

Then the blocks:

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

Three details, each of which produces silent corruption if guessed:

**`Hash64` is not a hash.** It is the symbol's content, padded to eight bytes.
That is sound only because such a symbol must be at most eight bytes, must carry
no relocations, and must be in the default section. An implementation that reads
the name and truncates a real hash instead would merge two symbols that share a
prefix. nanogo rejects all three violations rather than trusting the caller.

**Relocations are hashed before they are sorted.** The reference writer emits
the hash blocks and only then sorts a symbol's relocations by offset. Hashing
sorted relocations diverges from `gc` for any symbol whose relocations are not
already ordered, and a divergent hash for an itab is exactly the pointer
identity failure [032](032-type-descriptors-and-itabs.md) describes. The order
is the emission order, and a test locks it.

**A TEXT symbol needs four auxiliary symbols, not one.** `AuxFuncInfo`,
`AuxPcsp`, `AuxPcfile` and `AuxPcline` are all mandatory. Without `AuxFuncInfo`
the symbol belongs to no compilation unit and `cmd/link` panics in its DWARF
pass with no diagnostic; without the other three, `generatePctab` calls
`SymSize` on an unchecked index and faults on symbol 0. An earlier version of
this spec named only the first.

**A pc-value table must be unnamed; a FUNCDATA bitmap must be named.** The rule
is not uniform and an earlier version of this spec stated only half of it.

A stack map bitmap is named `gclocals·<base64>`, which is `gc`'s scheme, and the
name is load-bearing: the content-hash class is derived from the name, and only
that prefix selects the class that keeps a bitmap from merging with unrelated
read-only data. A `{n=1, nbit=0}` bitmap is eight mostly-zero bytes, so a merge
is not hypothetical. When it happens, the linker's pclntab pass sets the
symbol's value to an offset in another section and marks it special, `dodata`
skips it, and the other reference resolves to a bogus address.

Naming a FUNCDATA symbol is safe for the reason the next paragraph makes
dangerous for a pc-value table: funcdata never enters the data layout, because
the linker claims it before that pass runs.

**A pc-value table must be unnamed.** `cmd/link`'s loader decides whether a
symbol takes part in the data layout by asking whether it has a name, so a
*named* pc-value table is placed into the read-only section, its offset in
`runtime.pctab` is overwritten by that placement, and the linker faults while
writing the table. The writer expresses this with an explicit flag rather than
by inferring intent from an empty name, because forgetting to name a real
symbol and deliberately not naming an auxiliary one produce the same empty
string and only one of them is a bug.

**`AuxFuncInfo` must be a plain definition in this package's index space**, not
a content-addressable one. `cmd/link` reads it at the symbol index the
auxiliary entry names without resolving which index space that is, so a hashed
definition returns another symbol's bytes.

This is the mechanism [032](032-type-descriptors-and-itabs.md)'s canonical
naming feeds. Names make two symbols *nominally* the same; hashing makes them
*actually* the same. Both are needed: the hash merges identical content, and the
name is what a relocation refers to before the merge happens.

### Auxiliary symbols

The `Aux` block attaches metadata symbols to a function symbol by type:

| Aux type | Attached |
| --- | --- |
| `AuxGotype` | the function's type descriptor |
| `AuxFuncInfo` | frame size, arguments size, file table |
| `AuxFuncdata` | one per `FUNCDATA` index: stack maps, stack objects, open-coded defer info |
| `AuxPcsp`, `AuxPcfile`, `AuxPcline`, `AuxPcinline`, `AuxPcdata` | the pc-value tables |
| `AuxDwarfInfo`, `AuxDwarfLoc`, `AuxDwarfRanges`, `AuxDwarfLines` | [046](046-debug-info.md) |

The pc-value rows are a correction. An earlier version of this spec said
`AuxFuncInfo` holds the pc-value tables. It does not: each table is its own aux
symbol, and a writer that packs them into `AuxFuncInfo` produces an object the
linker reads as having none.

## PC-value tables

A `PCDATA` stream is a mapping from program counter to a small integer, encoded
as a delta-compressed sequence: for each change, a value delta and a pc delta.
The runtime decodes it linearly.

Four properties of the encoding that a writer must have exactly right, none of
which an earlier version of this spec stated:

1. The **value delta is zigzag-signed**. Values go down as well as up.
2. The **pc delta is unsigned and scaled by `MinLC`**, which is 4 on `arm64`.
   Writing byte offsets produces a table that decodes to the wrong program
   counters, four times too far apart.
3. The **initial value is -1**, so the first entry carries a value delta from
   -1 and no pc delta.
4. The stream **ends with a final pc delta and then a zero byte**. A table
   without the terminator runs into whatever follows it.

One limit of the encoder, found by its first real consumer and recorded rather
than fixed: **it cannot express a leading region whose value is the initial
value.** An entry equal to the initial value at offset 0 is skipped, so a table
that starts at $-1$ and changes later encodes as though it held the later value
from the start. The reference implementation emits a zero value delta as its
first pair instead, which the runtime accepts only in first position.

This is benign for the two streams nanogo emits today: the runtime substitutes
zero for $-1$ in the stack map index, and a leading unsafe-point region only
costs preemption opportunities. It is a real divergence from `gc`'s bytes, and
it would corrupt any table that genuinely needs a $-1$ prefix.

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

## What the writer proved

The object writer is the first place [000](000-decisions.md) decision 3 is
tested against reality rather than argued, and it passed:

| Check | Result |
| --- | --- |
| `go tool nm -size` on a nanogo object | agrees on names, kinds and sizes |
| `go tool objdump` | names the TEXT symbol and decodes its instructions |
| **`go tool link`** | **links a nanogo object against the real Go runtime into a binary that runs** |
| Content hash against `gc` | byte-identical for a real `go:string.*` symbol |
| pc-value tables against `-d=pctab` | byte-identical |
| Determinism | identical bytes across processes, working directories and environments |

The link result is the one that matters. It is the earliest empirical answer to
whether nanogo's objects are objects the Go toolchain accepts, and it arrived
before any code generator existed.

The comparison against `go tool asm` is a subset check and not byte equality,
which is worth stating rather than glossing: the assembler fabricates
`.arginfo0`, `.args_stackmap` and a `gofile..` symbol that a compiler does not,
and its string and file tables differ. Byte equality is asserted between nanogo
and nanogo, which is what [053](053-determinism.md) needs.

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
