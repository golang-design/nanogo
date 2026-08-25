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
| The reference from generated code | `ir/lower.go` | built for `new`, `&T{...}`, a slice literal and `make([]T, n)` |
| The descriptor as a data symbol in the object | nowhere | **not built**, see the seam below |
| Itabs | nowhere | **not built**, and blocked on the IR rather than on this spec |

The `gclocals·` and `go:string.` rows of the namespace table are produced as
before, in `ssagen/stackmap.go` and `ssa/decompose.go`.

### The two spellings, which the first draft of this spec did not separate

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

### The seam that is left

Nothing writes a descriptor into an object file, and nothing runs the lowering
pass in a real build either. Three changes close the seam and all three are
wiring:

1. **`driver/compile.go` does not call `ir.Lower`.** Its pass list starts at
   `ssa.Build`, and there is no caller of `ir.Lower` outside a test anywhere in
   the tree. So the whole of [020](020-ir.md)'s lowering table, this spec's
   rows included, is measured by `ir/lower_corpus_test.go` and reaches no
   compiled program. This is not a gap in this spec and it is the first thing
   a reader of the corpus number has to know: the number says what the pass
   would buy, and today it buys it only in the test.
2. `emitPackage` collects `ir.LowerAndCollect`'s per function lists, calls
   `rtype.Descriptor` on the union, and adds each symbol with
   `Package.AddHashedDef`, resolving each `rtype.Reloc` target by name.
   `rtype`'s own test does that wiring against a real `obj.Package` and writes
   the object, so the shape is checked; what is missing is the call site.
3. `ssagen/reloc.go`'s `symbolName` prefixes `pkg.Path + "."` onto any global
   whose name holds no dot. `type:p.T` survives that and `type:int`,
   `type:[]int` and `type:interface {}` do not. It must leave a `type:` name
   alone.

Until all three land, a program that reaches one of these rows is refused at
compile time exactly as before, because the pass that would lower it does not
run. Once 1 lands without 2, such a program compiles and reports the descriptor
as undefined at link time, which is a loud failure and not a silent one. That
ordering is why the lowering rows are built ahead of the writer: a row blocked
on a writer in another package should not be counted as a row blocked on the
lowering table.

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
- **`Hash`** must match **`gc`'s**, not the runtime's. This spec said "what the
  runtime computes"; the runtime computes no type hash. `gc` computes it at
  compile time and the runtime only compares it, so the requirement is that two
  compilers agree. `gc` hashes the *link string* with `cmd/internal/hash.Sum32`,
  which is sha256 with the first byte inverted, and takes the first four bytes
  little-endian. Reproducing it is also a check on the link string: a hash that
  matches proves the two compilers spell the type the same way.
- **`Equal`** is a generated function for a struct or an array whose parts do
  **not** compare as one region of memory. This spec said it is never a runtime
  helper, and that is too strong. Every other comparable type reaches a
  function the runtime already has, and a region of memory whose size is not
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
`ir.LowerAndCollect`. This spec said SSA construction, and construction is the
wrong pass: every reference is introduced by lowering, and construction refuses
every node that would introduce one. Deduplication is the linker's, so a
descriptor emitted by two packages is not an error.

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
