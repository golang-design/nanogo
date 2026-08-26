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
[000](000-decisions.md) decision 10.

This spec also owns the symbol namespace, because the linker's handling of these
symbols is keyed on their names, and because that keying is what killed the
text-assembly seam in [000](000-decisions.md) decision 3.

## What is built

**Descriptors are named, encoded and referenced. Itabs are not.**

| Part | Where | State |
| --- | --- | --- |
| The canonical name, both spellings | `ir/rtype.go` | built; the expected spellings were read out of a `gc` object with `go tool nm`, and the hash below re-checks the link string against a running `gc` binary |
| The descriptor bytes | `rtype/` | built for the types below, including a defined type's `UncommonType` tail and a struct's field array, checked field by field against `reflect` |
| The reference from generated code | `ir/lower.go` | built for `new`, `&T{...}`, a slice literal and `make([]T, n)`, and the pass runs in a real compile |
| The descriptor as a data symbol in the object | `driver/compile.go` | built; a named `dupok` definition, and the data it points at is hashed |
| Itabs | nowhere | **not built**; the concrete side's method set is in the IR and the interface side's is not |

The `gclocals·` and `go:string.` rows of the namespace table are produced
elsewhere. `ssagen/stackmap.go` builds the stack maps, `ssa/decompose.go` names
a string constant the way `gc` names it, and `ssagen/reloc.go` defines its
bytes in the object ([040](040-object-format.md)).

A type descriptor is not the only data symbol nanogo writes.
`ssagen/data.go` writes one per package-level variable, and it reads this
spec's writer for the pointer map: a variable whose type holds a pointer
carries its descriptor through an `AuxGotype` entry, so the writer gap below
refuses such a variable by name and position.

What reaches a running program: a variadic call, a slice literal, `make` of a
slice, `new` of a defined struct type, and a method call on a value or a
pointer receiver. Those compile, link against the real runtime and run, and
`internal/e2e` runs a collection over what one of them allocated.

A refusal from `rtype` arrives after the function it came from has compiled,
because lowering can name `main.point` without trouble and only the encoder
knows whether it can fill the bytes in. The same refusals reach the driver from
the other side, through the rule below that a package owes a descriptor for
every type it declares, and `driver/types.go` reports them per package.

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

The name is a function of the `ir.Type`, and four Go distinctions do not
survive [020](020-ir.md)'s type boundary. `ir/rtype.go` refuses each one by
name:

| Distinction | Two types that would share one name |
| --- | --- |
| a channel's direction | `chan int` and `chan<- int` |
| a function's signature | `func(int)` and `func(string)` |
| a literal interface's method signatures | `interface{ M() }` and `interface{ M(int) }` |
| an embedded field renamed through a type alias | `struct{ int }` and `struct{ Int = int }` |

The last two reduce to the second: a literal interface's spelling holds each
method's type and a literal struct's spelling holds each field's type, so both
need the signature the boundary drops. The alias case is separate and is not a
boundary gap in the same sense: `ir.Field` carries `Tag`, `Embedded` and `Pkg`,
and what `Converter` loses is the alias itself, because it unaliases before the
name is asked for. A **defined** type is exempt from all four, because its name
is its identity: `type S func(int)` is `type:p.S` and no signature is needed to
say so.

One case is refused by neither and is wrong: a **generic instantiation**.
`ir/convert.go` names `atomic.Pointer[os.dirInfo]` as `sync/atomic.Pointer`,
dropping the type arguments, so two instantiations of one generic type share
one name. Nothing in an `ir.Type` can detect it. The fix is in `convert.go`'s
`namedString`, not here.

### What `rtype` can fill in

A predeclared basic type, a slice, an array, a pointer, a struct, a **channel**,
a **function** and a defined type over any of those, with the `UncommonType`
tail and the `StructType`, `ChanType` or `FuncType` header and array that each
needs.
`ir.Type.Methods` is what makes the tail writable: a defined type's method set,
set for every defined type with the empty set included, so an empty set is a
fact rather than an absence. `TFlagUncommon` and the tail are one decision, because `gc` gives a
tail to every type that has a name and a flag without a tail makes the runtime
read past the end of the descriptor.

Four things stop a descriptor, and each names itself in the refusal:

| Stop | What is missing |
| --- | --- |
| a method | the two ABI wrappers `Ifn` and `Tfn` that `gc` generates beside every method |
| a struct or an array whose parts do not compare as one region of memory | the generated equality function this spec owes |
| a map, or a literal interface with methods | the group type and the type of each method, which [020](020-ir.md)'s type boundary drops |
| a type holding more pointer words than the inline mask spells | the on-demand mask `gc` writes past `maxPtrmaskBytes`, which this spec does not write |

Writing a tail that claims a type has no methods is the failure the first row
exists to stop: `reflect` would report an empty method set, and an itab built
against it would find no functions.

**The first row lost half its reason.** It read "the method's signature, for
the `Mtyp` offset, and the two ABI wrappers". `ir.Method.Sig` carries the
signature now, so `Mtyp` is writable and only the wrappers are left. They are
not a boundary gap and no field closes them: `Ifn` and `Tfn` are `TextOff`s to
generated *code*, `Tfn` being the method itself for a value receiver and `Ifn`
a wrapper taking a one-word receiver, and `rtype` returns data symbols. The
refusal names the wrappers now, and names the signature only for a method built
below the boundary that genuinely has none, because a message that states a gap
that is already closed sends the next reader to the wrong file.

A struct with an unexported field is **not** a stop, and it is worth saying
because the shape looks like one. `gc` puts the declaring package's path in the
struct descriptor's own `PkgPath`, once for the whole struct rather than once
per field, and `ir.Converter` sets `Field.Pkg` on every unexported field, so
`rtype/name.go` has what it needs. The two refusals that file carries, a field
with no package and fields from two packages, are unreachable from the checker,
because the fields of one struct literal are declared in one file. They guard
an IR built by hand.

One stop is above `rtype` rather than in it. A type parameter has no run-time
representation, so `ir/convert.go` refuses the conversion before a descriptor
is asked for, and the refusal names `ir` and not `rtype`. It closes with
[013](013-generics.md)'s stenciler, which replaces the parameter with the
argument, and not with anything here.

**An itab needs both sides and has one and a half.** The concrete side is
`ir.Type.Methods`, and it carries each method's signature now, so a
descriptor's `Method` has its `Mtyp`. What an itab still lacks is the `Fun`
array, which holds the same `Ifn` wrappers the method rows above are refused
for, and the interface side for a **literal** interface, which carries no
method names because it is not a `*types2.Named`.

What is above them is not `ssa.Build`. `OTypeAssert` and `OTypeSwitch` are in
`ir.goSpecificOps` and have no row in the lowering table, so `ir/lower.go`
refuses each with "no row of the lowering table is built for it yet" and the
node never reaches SSA construction. No program reaches an itab today whatever
this spec builds. The distinction matters to anyone measuring what a change
buys: a row refused in the lowering pass is not evidence about SSA
construction, and this spec named the wrong pass.

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

`driver/types.go` walks the same closure and refuses the package when any type
in it hits one of the four stops above, naming the type, the position and the
stop. It walks the closure and not the declaration list because `cmd/link`'s
`defgotype` follows a struct descriptor into the type of every field, so a
package owes a descriptor for every type its own descriptors reach. Nothing
about the declaring package tells the two cases apart: the same library links
against one importer and not against another, depending on whether the importer
puts the type on a local variable ([015](015-export-data.md) has the
measurement).

## Testing

The first and third bullets are built. The second and fourth need itabs and
generated equality functions, neither of which exists.

- Layout: emit a descriptor with nanogo, read it back with `reflect` in a
  `gc`-compiled program running in the same binary. Hosted mode
  ([000](000-decisions.md) decision 10) makes this direct. `rtype`'s test does
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

### Every defined type was refused, and the reason was the type boundary

This spec explained every refusal of a defined type by [020](020-ir.md)'s type
boundary dropping the method set, and said that extending the boundary was the
work that unblocked both descriptors and itabs. The boundary was extended:
`ir.Type` carries `Methods`, `PkgPath` and `Basic`, and `ir.Field` carries
`Tag`, `Embedded` and `Pkg`. A defined type's descriptor followed, with its
`UncommonType` tail and a struct's field array, and `new(T)` and a method call
on either receiver now run.

What is left is not one gap but four, and the sections above list them. The
prediction that stood is that itabs and a defined type's descriptor were
blocked on the same thing; the prediction that did not is that closing it would
finish both. A descriptor's `Method` still needs a signature, and an itab still
needs an interface's method list, so the boundary owes the same field twice
over.

**One of the four was already closed when it was written down.** The stop list
carried "a struct with an unexported field", on the reasoning that the IR type
does not say which package declared each field. The same boundary work that
added `Methods` added `Field.Pkg`, so the converter sets it on every unexported
field and `rtype/name.go` reads it. A package declaring an exported struct with
an unexported field compiles. The stop list names the pointer-mask limit in its
place, and the paragraph below the table says what the unexported field is
not, because the shape reads like a stop and a reader who skips the paragraph
will assume it is one.

### A channel's direction crossed the boundary

The table above listed a channel beside a map and a function, on the grounds
that [020](020-ir.md)'s type boundary drops the direction. `ir.Type` carries
`ChanDir` now, populated by `Converter` from the checker, and `rtype/chan.go`
writes the `ChanType` header: an element pointer and the direction as a full
word, which is what `internal/abi` spells it as.

The zero value is `InvalidDir` and **not** `chan T`. A channel type built below
the boundary by hand carries no direction, and the encoder refuses it by name
rather than defaulting to bidirectional, because a default is the field being
supplied by the encoder instead of by the checker and that is what the second
rule at the top of `ir/type.go` forbids. What it would cost is the failure this
spec already names: `<-chan int` written under `chan int`'s name, one symbol
for two types, merged by the linker without a diagnostic.

**A channel *literal* is still refused, and the refusal moved.** It is now
`ir/rtype.go`'s and not `rtype`'s. `typeName` spells no direction, so
`chan int` and `chan<- int` would still share one name, and `Descriptor` asks
for the name before it asks whether the bytes can be filled in. What the naming
function needs is three lines: the direction's spelling before the element's,
which `ir.ChanDir.String` already returns in the form that precedes an element
(`chan`, `chan<-`, `<-chan`). A **defined** channel type is exempt, as every
defined type is, because its name is its identity, so `type Signal chan int`
compiles to a descriptor today.

### A function's signature crossed the boundary

`ir.Type` carries `Params`, `Results` and `Variadic`, and `rtype/func.go`
writes the `FuncType` header and the array that follows it. A defined function
type gets a descriptor.

The array is **one** array for both lists, split by the two counts, and the
receiver would come first if there were one. `reflect` reads `In(i)` from the
first `InCount` entries and `Out(i)` from the rest, so an array written in the
wrong order reports a function's results as its parameters and nothing in the
bytes says so. The variadic flag is the top bit of `OutCount`, which is why
`OutCount` holds one bit fewer than `InCount`.

The header is eight bytes for four bytes of content. The padding is not slack:
what follows is an array of `*Type` addressed from the end of the header, so a
four-byte header would put every pointer in it at an offset that is not
pointer-aligned.

A function *literal* is still refused, by `ir/rtype.go` and not here, for the
reason a channel literal is: the naming function spells no signature, so
`func(int)` and `func(string)` would share one symbol. What that file needs is
the spelling `func(` plus each parameter, `, ...` for a variadic last one, and
the results, which is a walk over the two lists it can now see.

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
