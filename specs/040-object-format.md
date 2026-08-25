---
title: "Object files: writing goobj"
status: in progress
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
namespace (`type:`, `go:itab.`, `go:string.`) uses one.
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

The blank line is part of the shape and this spec's listing did not have one.
`obj.WriteObject` writes the header, then `main` and an empty line for a main
package, then `!`. A package that is not main gets the header and `!` with
nothing between them, because the linker reads one thing out of the region and
stops at the first empty line. This was found by reading the writer against the
listing.

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
content (a stack map, a string literal, a type descriptor) is written with its
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
name matters: the content-hash class is derived from the name, and only
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

## The initialisation record

Every package nanogo compiles gets a symbol named `<path>..inittask`. It is
the only thing that makes the package's initialisation run, and its absence is
silent: the program starts, runs, and dies where it reads something an init
should have set.

The layout is `runtime.initTask` in `runtime/proc.go`, and `doInit1` beside it
is what reads the bytes:

| Offset | Field | Value |
| --- | --- | --- |
| 0 | `state` | 0, meaning not initialised. The runtime writes 1 and then 2 |
| 4 | `nfns` | how many functions follow |
| 8 + 8*i | function pointer | `R_ADDR`, 8 bytes, to an `ABIInternal` text symbol |

Three properties of that decide whether a program starts and is right:

1. The record is **writable**, so `SNOPTRDATA` and not `SRODATA`. `state` is
   assigned twice per record. A read-only record faults inside the runtime,
   before any Go frame.
2. A record with no functions is **exactly eight bytes**. `cmd/link` leaves a
   record of eight bytes or fewer out of the schedule and keeps it only to
   order the rest, so padding one up is what makes the runtime reach
   `doInit1`'s `throw("inittask with no functions")`.
3. The functions are text symbols and so **`ABIInternal`**. A reference under
   `ABI0` names a symbol nothing defines.

nanogo lists one function: the one `ir.Build` synthesises, which assigns the
package's variables in the order [012](012-type-checking.md) computed and then
calls the declared `init` functions in source order.

### Ordering, and who writes the list

A record carries one `R_INITORDER` relocation per direct import, pointing at
that package's record. The relocation changes no bytes: offset zero, width
zero, addend zero. `cmd/link` reads the edges in `ld/inittask.go` to schedule
the records, and reads nothing else from them.

The compiler does **not** write `go:main.inittasks`, which is the list
`runtime.main` walks. `cmd/link` builds it: it starts at `main..inittask`,
follows the edges, and emits the packages in an order that respects them,
breaking ties lexicographically. A compiler that wrote that symbol would
collide with the linker's own builder.

Two consequences follow from that, and a program depends on both:

- A main package always gets a record, even an empty one, because it is the
  only root the walk starts from. Without it the walk starts nowhere and no
  package in the program is initialised.
- An import with no edge takes its whole subtree out of the schedule. The
  program still links.

nanogo writes an edge for **every** direct import, without asking which
imports have a record. `gc` asks, because its importer reads the answer out of
the export data. Asking is not needed: `relocsym` returns on a zero-width
relocation before it looks at the target, so an edge to a package that has no
record resolves to nothing, and `inittasks()` finds a symbol nothing defines
to be of size zero and leaves it out of the schedule. The cost is a record
nanogo writes where `gc` writes none, for a package that has no init work and
no import with any; it holds no functions, so the linker keeps it for ordering
and the runtime never sees it. Guessing the other way would drop an edge and
run an init late, which no test can see.

Whether a record was written travels into the export data, so that an importer
orders its own record after this one ([015](015-export-data.md)).

`R_INITORDER` is relocation 102. The number is stated rather than counted:
more than fifty architecture relocations nanogo never emits sit between it and
the last entry `obj` declares, and `cmd/internal/objabi`'s generated stringer
asserts the value with `_ = x[R_INITORDER-102]`.

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

nanogo produces two, and the count is a correction. This spec listed four
indices and the writer emits two, which `ssa/stackmap.go` declares and
`ssagen/stackmap.go` fills:

| Index | Contents |
| --- | --- |
| `PCDATA_UnsafePoint` | where the frame is inconsistent |
| `PCDATA_StackMapIndex` | which stack map bitmap is current |

Plus the line and file tables, which are the same encoding over source positions
from [010](010-scanner-and-positions.md).

The two that are gone were not dropped, they were never reachable. The spec
said nanogo produces a `PCDATA_InlTreeIndex` and a `PCDATA_ArgLiveIndex`
stream; the code declares neither constant. This was found by reading the index
constants in `ssa/stackmap.go` against the table. The inline tree waits on
[024](024-inlining-and-devirtualization.md), which is unbuilt, so there is no
tree to index. Argument liveness is an optimization of the collector's scan and
nanogo does not compute one.

The `AuxFuncdata` count has the same shape. The format allows one entry per
`FUNCDATA` index and nanogo writes exactly two, `FUNCDATA_ArgsPointerMaps` and
`FUNCDATA_LocalsPointerMaps`. `FUNCDATA_StackObjects` is computed and not written.
`ssa.StackMaps.ObjectsSym` builds the table, and 162 functions of the
distribution corpus have one, but `ssagen` passes it no way to name a local's
type descriptor, so no object carries the symbol. That seam is narrower than it
was: [032](032-type-descriptors-and-itabs.md) writes a descriptor now, and what
is missing is the lookup from an `ir.Type` to the `goobj.SymRef` of the
descriptor's pointer mask. The omission is conservative rather than wrong. Such
a local stays in the locals bitmap for the whole of its marked lifetime, so the
collector scans it and never misses it.

## The fingerprint, which is plumbing with no value in it

Each object records a fingerprint of the package's export data, and each import
records the fingerprint it was compiled against. The linker checks them and
refuses a mismatched build.

That is the format. It is not what nanogo does, and this is the one part of
this spec's scope that is written down and not built. `obj.Package` has a
`Fingerprint` field and `obj.AddImport` takes one, and nothing outside `obj`
ever sets either. `driver/compile.go` records no import at all, so the Autolib
block is empty and the fingerprint is eight zero bytes. This was found by
grepping for every writer of the field and finding only the writer that copies
it into the object.

The spec claimed the opposite. It said the fingerprint is "the mechanism that
makes hosted mode ([000](000-decisions.md) decision 11) safe: a nanogo-compiled
package and a `gc`-compiled importer either agree or fail loudly." They do not
fail loudly. Zero equals zero, so the linker's check passes on any pair, and a
nanogo object compiled against stale export data links silently. The safety net
decision 11 leans on is declared and absent.

It was absent for a reason above this spec: the fingerprint is a hash of the
export data a package *writes*, and [015](015-export-data.md)'s writer was
unbuilt, so there was nothing to hash. The writer exists now, so the input is
there and the field is the work that is left.

**Nothing else may be added to the archive to carry a fact about it.**
[054](054-distribution.md) tried exactly that, appending a member naming which
compiler produced an archive. The importer and the linker both accepted it, and
`go tool nm` and `go tool objdump` both stopped working on every archive,
because `cmd/internal/objfile` treats a member it does not know as a hard
error. The producer record now sits beside the archives, in
`pkg/GOOS_GOARCH/MANIFEST`, with a SHA-256 per archive. A field about an
archive belongs next to it, not inside it, unless the format already has a
field for it. The fingerprint above is the field the format already has.

## Version pinning

The magic string carries a version. `obj.Magic` is nanogo's, and
`obj.VerifyToolchain` compares it against what the installed toolchain writes.
There is no compatibility range and no negotiation: a mismatch is an error
naming both, and it also names the header line so that a reader can see which
release produced it.

Two details of when, because "asserts it at startup" was what this spec said
and is not what happens. The check runs when the driver is about to write an
object (`driver/compile.go`, `writeOutput`), not at process start, and it runs
once per process behind a `sync.Once` because it costs an `asm` invocation. A
compile that fails before it reaches the writer therefore never checks the
version. That is the right order: a version mismatch is only a problem for an
object that is written, and running the probe on every invocation would put a
subprocess in the path of `nanogo -V=full`.

## What the writer proved

The object writer is the first place [000](000-decisions.md) decision 3 is
tested against reality rather than argued, and it passed:

| Check | Result |
| --- | --- |
| `go tool nm -size` on a nanogo object | agrees on names, kinds and sizes |
| `go tool objdump` | names the TEXT symbol and decodes its instructions |
| **`go tool link`** | **links a nanogo object against the real Go runtime into a binary that runs** |
| Content hash against `gc` | byte-identical for a real `go:string.*` symbol |
| pc-value encoder against `-d=pctab` | byte-identical |
| Determinism | identical bytes across processes, working directories and environments |

The pc-value row is narrower than it looks and is worth reading exactly. The
test takes the entries `go tool compile -d=pctab=pctospadj` prints, re-encodes
them with `obj.EncodePCData`, and compares the bytes. It proves the encoding of
[040](040-object-format.md)'s delta scheme against `gc`'s, which is the part
that is easy to get wrong. It does not compare nanogo's own line or file stream
against `gc`'s for the same function, because nanogo and `gc` do not lay out
the same instructions.

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

Every check below is written and gated, in `obj/write_test.go`:

- `TestNMReadsWhatWasWritten` and `TestObjdumpReadsText` run `go tool nm` and
  `go tool objdump` on nanogo's output. `TestSameSymbolsAsAssembler` compares
  the symbol set against `go tool asm`'s for the same input. Structural
  differences are expected there; missing symbols and wrong types are not.
- `TestLinkerReadsObject` is the real test, and it is the one that closed
  [000](000-decisions.md) decision 3's open question.
- `TestPCDataMatchesCompiler` gates the pc-value encoder against `gc`'s own
  bytes.
- `driver/inittask_test.go` states the record's bytes field by field, and
  `internal/e2e/inittask_test.go` runs programs whose exit status depends on an
  init having run: `os.Stdout` reporting file descriptor 1, a package's own
  init, two packages whose inits must run in import order and not in the
  lexicographic order `cmd/link` falls back to, and a blank import.
- `TestDeterminismAcrossProcesses` is determinism
  ([053](053-determinism.md)): the same source produces the same object bytes,
  from two processes with different environments and working directories. This
  is the earliest place the G1 fixed point can be broken and the cheapest place
  to check it.
