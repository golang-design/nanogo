---
title: "Type descriptors, itabs, and the symbol namespace"
status: draft
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
- **`Hash`** must match what the runtime computes for the same type, because
  maps and type switches compare hashes before comparing types.
- **`Equal`** is a generated function for struct and array types whose fields are
  comparable. It is a *function the compiler must emit*, not a runtime helper,
  and a type without one cannot be a map key.

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

recorded in [`spikes/symbolnames`](../spikes/symbolnames). Every prefix in the
table above uses one. A text-assembly emitter would have to rename them, the
linker would stop collecting them, `runtime.itablinks` would be empty, itabs
would not be registered, and a dynamic conversion would build a second itab for a
pair that already had one. That is the pointer-identity failure above.

## What must be emitted, and when

A descriptor is emitted for a type when the program needs it at run time:
conversion to an interface, use as a map key or element, use as a channel
element, reflection, and a type assertion's target.

The set is computed during SSA construction and collected per package.
Deduplication is the linker's, so a descriptor emitted by two packages is not an
error.

For types defined in another package, the descriptor is emitted by the defining
package and referenced. For composite types built from them — `[]*pkg.T` — the
descriptor is emitted by whichever package needs it, under the canonical name, and
merged.

## Testing

- Layout: emit a descriptor with nanogo, read it back with `reflect` in a
  `gc`-compiled program running in the same binary. Hosted mode
  ([000](000-decisions.md) decision 11) makes this direct.
- Itab identity: convert the same concrete type to the same interface in two
  packages, one compiled by nanogo and one by `gc`, and assert the interface
  values compare equal.
- `GCData` against `PtrBits` for every type in a corpus, and against the
  collector's own view under `GODEBUG=gccheckmark=1`.
- Generated equality functions used as map keys, for structs and arrays of every
  comparable field type, including nested and padded ones. Padding bytes must not
  be compared.
