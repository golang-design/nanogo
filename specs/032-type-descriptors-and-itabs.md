---
title: "Type descriptors, itabs, and the symbol namespace"
status: in progress
layer: runtime interface
gate: G1
depends_on:
  - 030-abi.md
  - 040-object-format.md
---

# Type descriptors and itabs

Go's runtime and `reflect` need a description of every type that reaches an
interface, a map, a channel, or reflection. The compiler emits those
descriptions as data symbols. Their layout is `internal/abi`'s, exactly, by
[000](000-decisions.md) decision 11.

This spec also owns the symbol namespace, because the linker's handling of these
symbols is keyed on their names, and because that keying is what killed the
text-assembly seam in [000](000-decisions.md) decision 3.

## What is built

**Descriptors are named, encoded and referenced. Itabs are not.**

| Part | Where | State |
| --- | --- | --- |
| The canonical name, both spellings | `ir/rtype.go` | built; the expected spellings were read out of a `gc` object with `go tool nm`, and the hash below re-checks the link string against a running `gc` binary |
| The descriptor bytes | `rtype/` | built for the types below, checked field by field against `reflect` |
| The reference from generated code | `ir/lower.go` | built for `new`, `&T{...}`, a slice literal and `make([]T, n)`, and the pass runs in a real compile |
| The descriptor as a data symbol in the object | `driver/compile.go` | built; a named `dupok` definition, and the data it points at is hashed |
| Itabs | nowhere | **not built**, and blocked on the IR rather than on this spec |

The `gclocals·` and `go:string.` rows of the namespace table are produced
elsewhere, in `ssagen/stackmap.go` and `ssa/decompose.go`.

What reaches a running program today: a variadic call, a slice literal, `make`
of a slice, and `new` of a type whose descriptor can be filled in. Those
compile, link against the real runtime and run, and `internal/e2e` runs a
collection over what one of them allocated. A defined type is refused, and the
refusal arrives from `rtype` after the function it came from has compiled,
because lowering can name `main.point` without trouble and only the encoder
knows that its method set is not in the IR. That is the same gap that stops
itabs, met from the driver.

Four wiring changes closed the seam between the lowering pass and the object
file. Two of them were not what this spec predicted, and **What was wrong** at
the end of this file records all four.

### The two spellings

`gc` writes a type twice and the two strings differ.

- The **link string** qualifies a defined type by its import path. It is the
  symbol name after `type:`, and it is what the hash is computed over.
  `type:sync/atomic.Pointer[os.dirInfo]`.
- The **name string** qualifies by package name. It is the content of the
  descriptor's `Str` name data and it is what `reflect.Type.String` returns.
  `atomic.Pointer[os.dirInfo]`.

`Str` does not point at the name string. It points at `"*"` followed by it,
with `TFlagExtraStar` set, so that the descriptors of `T` and `*T` share one
string. A descriptor that pointed at the bare name would make `reflect` report
a name one character short.

### Naming and encoding are two questions, and the second is harder

A type may have a canonical name and still have no writer for its bytes. That
is not an inconsistency: a descriptor for a type defined in another package is
that package's to emit, and this package only needs to name it. So the
lowering pass refuses a row when the type cannot be *named*, and `rtype`
refuses separately when the contents cannot be *filled in*.

The name is a function of the `ir.Type`, and four Go distinctions do not
survive [020](020-ir.md)'s type boundary:

| Distinction | Two types that would share one name |
| --- | --- |
| a channel's direction | `chan int` and `chan<- int` |
| a function's signature | `func(int)` and `func(string)` |
| an interface's method set | `interface{ M() }` and `interface{ N() }` |
| a struct field's tag and embedding | `struct{ A int }` and `struct{ A int "x" }` |

Each is refused, and the refusal names the missing field. A **defined** type is
exempt from all four, because its name is its identity: `type S func(int)` is
`type:p.S` and no signature is needed to say so.

One case is refused by neither and is wrong: a **generic instantiation**.
`ir/convert.go` names `atomic.Pointer[os.dirInfo]` as `sync/atomic.Pointer`,
dropping the type arguments, so two instantiations of one generic type share
one name. Nothing in an `ir.Type` can detect it. The fix is in `convert.go`'s
`namedString`, not here.

### What `rtype` can fill in

Only a type whose method set is *knowably empty*. A descriptor carries an
`UncommonType` tail whenever the type has methods, and an `ir.Type` carries no
method set, so a descriptor for a type that might have methods would claim it
has none, `reflect` would report an empty method set, and an itab built against
it would find no functions.

A method set is knowably empty for a predeclared basic type and for a slice, an
array, a map, a channel, a function, a literal struct and a literal interface,
because the language gives none of those a method. It is not knowable for a
defined type or for a pointer to one, and both are refused.

**This is the same gap that stops itabs.** An interface's `ir.Type` carries
only `EmptyIface`. There is no method set to build a `Fun` array from and no
method set on the concrete side to fill it with. Itabs are not the harder half
of this spec: they are blocked on a different thing, the type boundary itself,
and so is every defined type's descriptor. Extending [020](020-ir.md) to carry
a method set is the work that unblocks both.

## The type descriptor

```go
type Type struct {
    Size_       uintptr
    PtrBytes    uintptr   // prefix bytes that can contain pointers
    Hash        uint32    // type hash, used by maps and type switches
    TFlag       TFlag
    Align_      uint8
    FieldAlign_ uint8
    Kind_       Kind
    Equal       func(unsafe.Pointer, unsafe.Pointer) bool
    GCData      *byte     // pointer bitmask, or a pointer to one
    Str         NameOff   // string form
    PtrToThis   TypeOff   // descriptor for *T, or zero
}
```

A descriptor for a composite type is this struct followed by kind-specific
fields: element type for slices and pointers, key and element for maps, fields
for structs, methods for interfaces, in and out for functions.

Four fields are worth calling out because each has a way of being subtly wrong:

- **`PtrBytes`** is a prefix length, not a size, as [030](030-abi.md) states. Too
  small and the collector misses pointers. Too large and it reads garbage as
  pointers.
- **`GCData`** is the pointer bitmask. It is the same information as the IR
  type's `PtrBits` from [020](020-ir.md) and must be computed from it, not
  recomputed, so that the two cannot disagree.
- **`Hash`** must match **`gc`'s**, not the runtime's. `gc` computes it at
  compile time and the runtime only compares it, so the requirement is that two
  compilers agree. `gc` hashes the *link string* with `cmd/internal/hash.Sum32`,
  which is sha256 with the first byte inverted, and takes the first four bytes
  little-endian. Reproducing it is also a check on the link string: a hash that
  matches proves the two compilers spell the type the same way.
- **`Equal`** is a generated function for a struct or an array whose parts do
  **not** compare as one region of memory. Every other comparable type reaches
  a function the runtime already has, and a region of memory whose size is not
  one of the fixed widths reaches `runtime.memequal_varlen`, which takes the
  size out of the closure and needs no generated function either. The field
  holds a **func value**, so it points at a one-word closure symbol and never
  at the code: pointing it at the function makes the runtime call whatever the
  first instruction encodes.

`PtrToThis` is left zero, which the field permits. The alternative is a weak
offset relocation, and `obj` declares no weak type. The cost is that
`reflect.PointerTo` builds a descriptor at run time instead of finding the
linked one.

**A divergence found while building this.** `ir.scalarPtrBits` marks both words
of an interface as pointers and `cmd/compile/internal/typebits` marks only the
second: an itab is in `persistentalloc` space and a compile-time `_type` is in
the read-only section, so the first word keeps nothing alive. `rtype` emits the
wider mask, because this spec requires `GCData` to come from `PtrBits` and
correcting it in the encoder would be the second computation that requirement
forbids. The mask is safe and it is not `gc`'s, so a descriptor for a type
holding an interface names a different `runtime.gcbits` symbol from `gc`'s and
the two do not merge. The fix is in `ir.scalarPtrBits`, and it moves the stack
maps as well.

`NameOff` and `TypeOff` are offsets relative to the module's data section, not
pointers. The linker resolves them. A compiler that emits pointers where offsets
belong produces a binary that fails at load.

## Itabs

```go
type ITab struct {
    Inter *InterfaceType
    Type  *Type
    Hash  uint32       // copy of Type.Hash, for type switches
    Fun   [1]uintptr   // variable sized; Fun[0] == 0 means Type does not implement Inter
}
```

An itab exists per (interface, concrete type) pair that the program converts. The
compiler emits it, the linker fills in `Fun` from the concrete type's method set,
and the runtime registers it at startup.

**Itab identity is pointer identity.** Two interface values with the same dynamic
type and interface compare equal because they share one itab. If a program ends
up with two itabs for one pair, interface comparison breaks and type switches
become unreliable. That is why the naming rules below are not cosmetic.

## The symbol namespace

| Prefix | Contents | Linker behaviour |
| --- | --- | --- |
| `type:` | type descriptors | deduplicated by name; collected into `runtime.typelinks` |
| `go:itab.` | itabs | deduplicated; collected into `runtime.itablinks` |
| `go:string.` | string literal data | deduplicated by content |
| `go:func.` | function value symbols | deduplicated |
| `gclocals·` | stack maps | deduplicated by content hash |

Two properties are required of every name in this table:

1. **Canonical.** The name is a function of the type alone, not of which package
   emitted it. Two packages that both convert `*bytes.Buffer` to `io.Writer` must
   produce the same itab name so the linker merges them.
2. **Deterministic.** [053](053-determinism.md), without exception. A name that
   depends on map order breaks the G1 fixed point.

One naming function, in one package, used by everything. The encoders never
build a name.

### Why the text-assembly seam died here

The Plan 9 assembler rejects a symbol name containing a colon:

```
./s2_arm64.s:2: expect two operands for DATA      // DATA type:main.Obj(SB)/8, $7
```

recorded in [`spikes/symbolnames`](../spikes/symbolnames) (the offending file is kept there as
`s2_arm64.s.txt`, because a file that must not assemble cannot sit in a package
that is built). Every prefix in the
table above uses one. A text-assembly emitter would have to rename them, the
linker would stop collecting them, `runtime.itablinks` would be empty, itabs
would not be registered, and a dynamic conversion would build a second itab for a
pair that already had one. That is the pointer-identity failure above.

## What must be emitted, and when

A descriptor is emitted for a type when the program needs it at run time:
conversion to an interface, use as a map key or element, use as a channel
element, reflection, and a type assertion's target.

The set is collected by the lowering pass and returned per function, by
`ir.LowerAndCollect`. Deduplication is the linker's, so a descriptor emitted by
two packages is not an error.

For types defined in another package, the descriptor is emitted by the defining
package and referenced. For composite types built from them, such as
`[]*pkg.T`, the
descriptor is emitted by whichever package needs it, under the canonical name, and
merged.

## Testing

The first and third bullets are built. The second and fourth need itabs and
generated equality functions, neither of which exists.

- Layout: emit a descriptor with nanogo, read it back with `reflect` in a
  `gc`-compiled program running in the same binary. Hosted mode
  ([000](000-decisions.md) decision 11) makes this direct. `rtype`'s test does
  this by reading the `*abi.Type` out of a `reflect.Type`'s interface word,
  because `reflect` exposes no accessor for `Hash`, `TFlag`, `PtrBytes` or the
  bitmask. Every field of every type in its corpus agrees with `gc`'s.
- Itab identity: convert the same concrete type to the same interface in two
  packages, one compiled by nanogo and one by `gc`, and assert the interface
  values compare equal.
- `GCData` against `PtrBits` for every type in a corpus, and against the
  collector's own view under `GODEBUG=gccheckmark=1`. The first half is built
  and the mask is asserted to be exactly what `PtrBits` says, so that a second
  computation cannot creep in. It is also compared with `gc`'s, where a bit
  `gc` sets and nanogo does not is always a failure and the interface type word
  is the one permitted difference.
- Generated equality functions used as map keys, for structs and arrays of every
  comparable field type, including nested and padded ones. Padding bytes must not
  be compared.

## What was wrong

### The seam between lowering and the object file

Nothing wrote a descriptor into an object file and nothing ran the lowering
pass in a real build. This spec named three changes that would close that seam.
Two of them were wrong and a fourth was missing, so the record is kept: a
reader who follows the same reasoning again makes the same two mistakes.

1. **`driver/compile.go` now calls `ir.Lower`.** It is the first stage of
   `compileFunc`'s pass list, ahead of `ssa.Build`, and it is named `ir.Lower`
   rather than "lowering" because `ssa.Lower` is in the same list and the two
   are different decks. The corpus now measures what the pass buys rather than
   what it would buy: 24,508 of 39,947 distribution functions get past
   construction with the pass, and 17,905 without it.
2. `emitPackage` collects `ir.LowerAndCollect`'s per function lists, unions
   them in first-use order, calls `rtype.Descriptor` on each, and resolves each
   `rtype.Reloc` target by name. **The descriptor itself is `AddDef`, not
   `AddHashedDef`, and this spec was wrong to say otherwise.** `cmd/link` reads
   no name for a symbol in the hashed index space, so a hashed descriptor is
   nameless to the linker: no reference from another object resolves to it and
   nothing collects it into `runtime.typelinks`. `gc` agrees. It sets
   `AttrContentAddressable` on `type:.importpath.`, `type:.namedata.`,
   `runtime.gcbits.` and itabs, and never on the descriptor. So the descriptor
   is a named `dupok` definition and the data it points at is hashed, which is
   what `rtype` documents when it returns the descriptor first.
3. `ssagen/reloc.go`'s `symbolName` prefixed `pkg.Path + "."` onto any global
   whose name held no dot. `type:p.T` survived that and `type:int`,
   `type:[]int` and `type:interface {}` did not. It leaves a `type:` name
   alone.
4. **A fourth change this spec did not name.** A symbol's identity is its name
   *and* its ABI, and `cmd/link` resolves a by-name reference with both:
   `abiToVer` maps `ABIInternal` to one version and everything else to
   another, and `regabiwrappers` is always on for arm64, so the two versions
   are not the same. `gc` gives a data symbol no ABI, so a descriptor is ABI0,
   and `ssagen` referenced every global at `ABIInternal`. With changes 1 to 3
   and not this one the link reports

   ```
   main.main: relocation target type:int not defined for ABIInternal (but is defined for ABI0)
   ```

   which was measured, not predicted. The reference now follows the object's
   class: `ClassGlobal` is ABI0 and a text symbol is `ABIInternal`. The
   equality routine a descriptor's `Equal` closure points at is the one target
   that is not data, and `rtsym` is what says so.

The ordering the seam had was still the right one. A program that reached one
of these rows was refused at compile time before change 1, and after change 1
without change 2 it compiles and reports the descriptor as undefined at link
time, which is loud rather than silent. That is why the lowering rows were
built ahead of the writer: a row blocked on a writer in another package should
not be counted as a row blocked on the lowering table.

### Three claims the encoder disproved

**The hash is `gc`'s, not the runtime's.** This spec said `Hash` must match
"what the runtime computes". The runtime computes no type hash. The field above
carries the correction and the method.

**`Equal` is sometimes a runtime helper.** This spec said it is never one. Every
comparable type whose parts compare as one region of memory reaches a function
the runtime already has, and a region whose size is not a fixed width reaches
`runtime.memequal_varlen`.

**The reference set is collected by lowering, not by SSA construction.** This
spec named construction, and construction is the wrong pass: every reference is
introduced by lowering, and construction refuses every node that would
introduce one.

### One spelling was two

The first draft of this spec did not separate the link string from the name
string. `gc` writes a type twice and the two differ, and a descriptor built
from one where the other belongs makes `reflect` report a name one character
short. The two spellings are set out above.
