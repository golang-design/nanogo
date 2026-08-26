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
| Itabs | nowhere | **not built**; the concrete side's method set is in the IR now and the interface side's is not |

The `gclocals·` and `go:string.` rows of the namespace table are produced
elsewhere. `ssagen/stackmap.go` builds the stack maps, `ssa/decompose.go` names
a string constant the way `gc` names it, and `ssagen/reloc.go` defines its
bytes in the object ([040](040-object-format.md)).

A type descriptor is not the only data symbol nanogo writes.
`ssagen/data.go` writes one per package-level variable, and it reads this
spec's writer for the pointer map: a variable whose type holds a pointer
carries its descriptor through an `AuxGotype` entry, so the writer gap below
refuses such a variable by name and position.

What reaches a running program today: a variadic call, a slice literal, `make`
of a slice, and `new` of a type whose descriptor can be filled in. Those
compile, link against the real runtime and run, and `internal/e2e` runs a
collection over what one of them allocated. A defined type is refused, and the
refusal arrives from `rtype` after the function it came from has compiled,
because lowering can name `main.point` without trouble and only the encoder
decides whether it can fill the `UncommonType` tail in. That is the same gap
that stops itabs, met from the driver.

Four wiring changes join the lowering pass to the object file. **What was
wrong** at the end of this file sets out all four, because two of them are not
what a reader following this design would predict.

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

The name is a function of the `ir.Type`, and four Go distinctions decide
whether it can be built. Three of them do not survive [020](020-ir.md)'s type
boundary and the fourth crosses it and is not read:

| Distinction | Two types that would share one name | Where it stands |
| --- | --- | --- |
| a channel's direction | `chan int` and `chan<- int` | `ir.Type` has no direction field |
| a function's signature | `func(int)` and `func(string)` | `ir.Type` has no parameter or result list |
| a literal interface's method set | `interface{ M() }` and `interface{ N() }` | `ir.Type.Methods` is a defined type's set only, so a literal interface has none |
| a struct field's tag and embedding | `struct{ A int }` and `struct{ A int "x" }` | `ir.Field` carries `Tag` and `Embedded`, and `ir/rtype.go`'s namer does not read them |

Each is refused today. The first three refusals name a field the boundary does
not carry. The fourth names one it does: `ir/rtype.go` still returns "a
struct's field tags are not in the IR type", and the tag is in the IR type, so
what is left is a namer that spells a literal struct out of the fields it
already has. A **defined** type is exempt from all four, because its name is
its identity: `type S func(int)` is `type:p.S` and no signature is needed to
say so.

One case is refused by neither and is wrong: a **generic instantiation**.
`ir/convert.go` names `atomic.Pointer[os.dirInfo]` as `sync/atomic.Pointer`,
dropping the type arguments, so two instantiations of one generic type share
one name. Nothing in an `ir.Type` can detect it. The fix is in `convert.go`'s
`namedString`, not here.

### What `rtype` can fill in

Only a type with no `UncommonType` tail. A descriptor carries that tail
whenever the type has methods, and `rtype` writes no tail, so it refuses every
type that could need one: a defined type and a pointer to one. Emitting one
anyway is the failure the refusal exists to stop: the descriptor would claim
the type has no methods, `reflect` would report an empty method set, and an
itab built against it would find no functions. `rtype` fills in a predeclared
basic type and a slice, an array, a map, a channel, a function, a literal
struct and a literal interface, because the language gives none of those a
method.

**The reason for that refusal is no longer the type boundary.**
`ir.Type.Methods` carries a defined type's method set, sorted by name, set for
every defined type with the empty set included, which is exactly what makes an
empty set knowable; `ir.Type` also carries `PkgPath` and `Basic`, and
`ir.Field` carries `Tag`, `Embedded` and `Pkg`. `rtype/rtype.go` does not read
`Methods` and still refuses with a message saying the set is absent. What is
left is an `UncommonType` writer and a `Method` encoder, and both are this
spec's work rather than [020](020-ir.md)'s.

**Itabs are half unblocked.** The concrete side of an itab is the method set
`ir.Type.Methods` now carries. The interface side is still missing: an
interface's `ir.Type` carries only `EmptyIface`, so there is no method list to
build a `Fun` array from and no way to name a non-empty interface. `ir.Method`
also carries no signature, and a descriptor's `Method` needs an `Mtyp` offset
to the method's type with the receiver removed, which needs the function
signature the boundary does not carry. Those two are what is left for
[020](020-ir.md); the writer is what is left for this spec.

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

`PtrToThis` is left zero, which the field permits. The cost is that
`reflect.PointerTo` builds a descriptor at run time instead of finding the
linked one. Emitting it is open work rather than a blocked one: `obj` declares
`R_WEAKADDROFF`, the weak offset relocation `gc` uses for this field, so what
is missing is a writer that names the descriptor of `*T` and a decision about
which package owes it.

**Where `GCData` diverges from `gc`.** `ir.scalarPtrBits` marks both words
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

### A declared type needs one whether the declaring package uses it or not

The rule above is what the *compiling* package needs, and it is not the whole
rule. gc emits a descriptor for **every type a package declares**, used or not:
`gc/main.go`'s `ir.OTYPE` case calls `reflectdata.NeedRuntimeType` for each
package-scope type declaration. The reason is that the descriptor is owed to an
importer, which refers to the type twice over: directly, where its code needs
the descriptor at run time, and through DWARF, where a variable of that type
names `go:info.<path>.<Type>`. `cmd/link` resolves neither by itself. Its
`ld/dwarf.go` builds the DWARF entry out of the descriptor, looking up
`"type:"+name`, so a missing descriptor surfaces as

    sym 5: relocation target go:info.<path>.<Type> not defined

which names neither the package that owes the symbol nor the fix. This is not a
[046](046-debug-info.md) gap: the entry is generated by the linker and the
compiler owes only the descriptor.

nanogo cannot write one. `rtype` refuses every defined type, because a
descriptor carries an `UncommonType` tail whenever the type has methods and
`rtype` has no writer for that tail. That is this spec's own gap, stated from
the other side, and it is the same one that stops itabs. Until it closes,
`driver/types.go` refuses a package that declares a type, naming the type and
the symbol. Nothing about the declaring package tells the two cases apart: the
same library links against one importer and not against another, depending on
whether the importer puts the type on a local variable
([015](015-export-data.md) has the measurement).

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

### The method set gap was the IR's and is now the writer's

Every refusal of a defined type in this spec was explained by
[020](020-ir.md)'s type boundary dropping the method set. The boundary carries
it: `ir.Type` gained `Methods`, `PkgPath` and `Basic`, and `ir.Field` gained
`Tag`, `Embedded` and `Pkg`, and `ir/convert.go` fills all of them. Nothing
below reads them yet, so `ir/rtype.go` and `rtype/rtype.go` refuse exactly what
they refused before, with messages that name a gap the IR closed. The refusals
in this spec are real and their stated cause was not; the sections above carry
the corrected one. The half of the gap that is still the boundary's is a
literal interface's method set, a channel's direction and a function's
signature.

### `PtrToThis` was blamed on a relocation that exists

This spec said the field is left zero because "the alternative is a weak offset
relocation, and `obj` declares no weak type". `obj/obj.go` declares `R_WEAK`,
`R_WEAKADDR` and `R_WEAKADDROFF`. Zero is still what the field carries and the
cost is unchanged, but the reason above is a writer that does not exist, not a
relocation type that does not exist. `rtype/rtype.go`'s comment on the same
line repeats the old reason and has not been corrected.

### One spelling was two

The first draft of this spec did not separate the link string from the name
string. `gc` writes a type twice and the two differ, and a descriptor built
from one where the other belongs makes `reflect` report a name one character
short. The two spellings are set out above.
