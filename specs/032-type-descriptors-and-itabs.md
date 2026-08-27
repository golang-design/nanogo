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
| The interface value itself | `ssa/build.go` | built for a conversion to an **empty** interface, and for a conversion of an interface with methods to one |
| The reference from an interface conversion | `ssa/build.go` | named, in `ssa.Func.Descriptors`. **`driver/compile.go` does not read that field yet**, so a conversion of a type the runtime does not already carry a descriptor for links against nothing |
| The descriptor as a data symbol in the object | `driver/compile.go` | built; a named `dupok` definition, and the data it points at is hashed |
| Itabs | nowhere | **not built**; the `Fun` array needs the same ABI wrappers a method's descriptor row is refused for |

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

Interface construction joins that list. `any` holding an `int` or a `string`,
`panic` of either, `panic` of an `error`, and `fmt.Println` all compile, link
and print what `gc` prints. `panic` of an interface with methods is the one
that proves the guarded load: the descriptor comes out of the itab, and the
value that used to reach the runtime made it die with "name offset out of
range" instead of printing.

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

The name is a function of the `ir.Type`, and **one** Go distinction does not
survive [020](020-ir.md)'s type boundary. `ir/rtype.go` refuses it by name:

| Distinction | Two types that would share one name |
| --- | --- |
| an embedded field renamed through a type alias | `struct{ int }` and `struct{ Int = int }` |

Three rows left this table. A channel's direction, a function's signature and a
literal interface's method list are all in `ir.Type` now, so each has a
spelling, and what each refuses is the zero value of the field it reads: a
channel whose `ChanDir` is `InvalidDir`, a function whose `Params` or `Results`
is nil, an interface with no method list. Those are types built below the type
boundary and never converted, and a zero read as a fact is what
[020](020-ir.md)'s second rule forbids.

The row that is left is not a boundary gap in the same sense: `ir.Field` carries
`Tag`, `Embedded` and `Pkg`, and what `Converter` loses is the alias itself,
because it unaliases before the name is asked for. A **defined** type is exempt,
because its name is its identity: `type S func(int)` is `type:p.S` and no
signature is needed to say so.

One case is refused by neither and is wrong: a **generic instantiation**.
`ir/convert.go` names `atomic.Pointer[os.dirInfo]` as `sync/atomic.Pointer`,
dropping the type arguments, so two instantiations of one generic type share
one name. Nothing in an `ir.Type` can detect it. The fix is in `convert.go`'s
`namedString`, not here.

### What `rtype` can fill in

A predeclared basic type, a slice, an array, a pointer, a struct, a **channel**,
a **function**, an **interface** and a defined type over any of those, with the
`UncommonType` tail and the `StructType`, `ChanType`, `FuncType` or
`InterfaceType` header and array that each needs.
`ir.Type.Methods` is what makes the tail writable: a defined type's method set,
set for every defined type with the empty set included, so an empty set is a
fact rather than an absence. `TFlagUncommon` and the tail are one decision, because `gc` gives a
tail to every type that has a name and a flag without a tail makes the runtime
read past the end of the descriptor.

Four things stop a descriptor, and each names itself in the refusal:

| Stop | What is missing |
| --- | --- |
| a method | the `Method` array, whose `Ifn` and `Tfn` are `TextOff`s to code |
| a struct or an array whose parts do not compare as one region of memory | the `Equal` closure, which points at code |
| a map | a *descriptor* for the slot group, whose slots are a literal struct |
| a type holding more pointer words than the inline mask spells | the on-demand mask `gc` writes past `maxPtrmaskBytes`, which this spec does not write |

Writing a tail that claims a type has no methods is the failure the first row
exists to stop: `reflect` would report an empty method set, and an itab built
against it would find no functions.

**The first two rows are no longer about missing code.** They read "the two ABI
wrappers `gc` generates" and "the generated equality function this spec owes",
and both functions exist now: `ssagen` builds them and the driver compiles them
into the object beside the package's own functions. See
[The generated functions](#the-generated-functions) below. What is left in both
rows is naming: `rtype` returns data symbols, and it has to spell the symbol
each field points at before the field can be written.

`ir.Method.Sig` carries the signature, so `Mtyp` is writable. The refusal names
the wrappers, and names the signature only for a method built below the
boundary that genuinely has none, because a message that states a gap that is
already closed sends the next reader to the wrong file.

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

A type assertion and a type switch still stop above SSA. `OTypeAssert` and
`OTypeSwitch` are in `ir.goSpecificOps` and have no row in the lowering table,
so `ir/lower.go` refuses each with "no row of the lowering table is built for
it yet" and the node never reaches SSA construction. The distinction matters to
anyone measuring what a change buys: a row refused in the lowering pass is not
evidence about SSA construction.

**One program does reach an itab, and it is a conversion and not an
assertion.** `var c coder = seven{}` converts a concrete type to an interface
with methods, and the type word of that value is the itab of the pair.
`ssa/build.go` refuses it and the refusal names both walls, because naming one
sends the next reader to a wall they cannot see: this spec writes no itab, and
`rtype` refuses `main.seven`'s own descriptor until its one method has the two
wrappers.

## The interface value in SSA

An interface is two words and no arm64 instruction holds one, so it is built as
a value and specs/025's decomposition takes it apart before selection. Three
operations of `ssa/op.go`, and none of them survives that pass:

| Operation | Meaning |
| --- | --- |
| `IMake(word, data)` | the interface value the two words are |
| `ITab(iface)` | the first word |
| `IData(iface)` | the second word |

Which word the first one is does not appear in the operations. It is an `*itab`
for an interface with methods and a `*_type` for one without, and the fact is
the value's type, `ir.Type.EmptyIface`. An operation that named one of them
would leave the other with no operation to be built from.

`CheckDecomposed` names a survivor. The three have no machine form, so one that
reaches lowering is a bug in the pass that owed its removal, and lowering finds
it only by panicking.

### The two words of a concrete conversion

The shape is `cmd/compile`'s `walkConvInterface`, minus the cases that only
avoid an allocation.

The type word is the concrete type's descriptor, named by `ir.TypeSymbol`. The
data word is `dataWord`'s answer:

| The value | The data word |
| --- | --- |
| one machine word wide, and that word holds a pointer | the value itself |
| a string | `runtime.convTstring` |
| a slice | `runtime.convTslice` |
| a scalar 8, 4 or 2 bytes wide, aligned to its width | `runtime.convT64`, `convT32`, `convT16` |
| anything else | **refused**, by name |

The first row is `types.IsDirectIface`, which is width and pointerness and not
a kind test. A `uintptr` is one word and holds no pointer, so it is boxed. A
one-field struct holding a pointer is not pointer shaped and is not boxed.

A float is reinterpreted before the call. `convT64` declares `uint64`, and
[030](030-abi.md) places an argument by the type of the value, so a float left
as a float is written to a floating-point register and read out of an integer
one.

Two shapes have no answer here and each names itself. A one-byte type is
`runtime.staticuint64s` indexed by the value, and anything wider than the
by-value helpers is `runtime.convT` or `convTnoptr` with the address of a copy
in the frame, which construction has no slot to make: it decided which objects
live in the frame before it built any expression.

### An interface with methods becomes an empty one by a load

```
typeWord := unsafe.Pointer(itab)
if typeWord != nil {
    typeWord = itab.Type
}
e = iface{typeWord, data}
```

The data word is carried across unchanged. The type word is not: the source
leads with an `*itab` and the destination leads with a `*_type`, and the
descriptor is the itab's **second** word, `internal/abi.ITab.Type`. A test
reads that offset out of the installed runtime's `internal/abi/iface.go`
rather than trusting the constant, because a wrong offset hands the runtime the
`*InterfaceType` where it reads the `*Type` and every field it reads after that
is another field.

The guard is not optional. A nil interface has a nil first word and reading a
field through it faults. The join is an ordinary phi over an ordinary variable,
which is what `&&` and `||` already build in `ssa/build.go`.

The other direction is refused. An `*itab` is the method table of one
(interface, concrete type) pair, so nothing in an interface value holds one
that was not put there and the conversion is a runtime lookup rather than a
load. `cmd/compile` gives it to `runtime.typeAssert`.

### Identity between two interfaces is the link string

Every interface is two words of one width and reports one kind, so "same kind,
same size" is true of every pair and says nothing. Two facts separate a pair
and neither is visible below the IR: which word the value leads with, and which
interface an `*itab` was built for. An itab lists the concrete type's methods
in the order its own interface declares them, so an itab built for
`io.ReadWriter` has two entries where `io.Reader` reads one.

`ir.TypeLinkString` is therefore the test, because two types have one link
string exactly when they are one type. `walkConvInterface` answers the same
question the same way: it reaches its I2I path for every pair that is not
identical, whatever the method sets are.

### The set a package owes is two sets, and only one is collected

`ir.LowerAndCollect` reports the descriptors the lowering table names. A
conversion to an interface reaches no row of that table, so the descriptors
construction names are a second set, carried on `ssa.Func.Descriptors` in
first-use order.

`driver/compile.go` does not union that field into `needed` yet. Until it does,
a conversion of a type the runtime already carries a descriptor for links, and
a conversion of any other type reaches the linker as

    main.main: relocation target type:main.myInt not defined

The fix is one line where `compileFunc` returns: the types the built function
named join the types the lowered tree named.

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
- **`Equal`** is the generated function of
  [The generated functions](#the-generated-functions) for a struct or an array
  whose parts do **not** compare as one region of memory. Every other comparable type reaches
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

## The generated functions

Three of a descriptor's fields point at code that no declaration defines. The
code is generated, it is `ssagen`'s, and the driver compiles it through the
same passes a declared function goes through: a generated function that took a
shorter path would be a second code generator, and the first bug it hit would
be one the pipeline already handles.

| Field | Symbol | Shape |
| --- | --- | --- |
| `Method.Tfn`, `Method.Ifn` | `p.(*T).M` | `func (p *T) M(a ...) (r ...) { return T.M(*p, a...) }` |
| `Type.Equal` | `type:.eq.<link string>` | `func(p, q *T) bool` |
| `MapType.Hasher` | `type:.hash.<link string>` | `func(p *T, h uintptr) uintptr` |

Each name is a function of the type alone, so two packages that need one
produce one symbol and the linker keeps one copy. Each text symbol is
duplicate-tolerant for the same reason. Each function carries `ir.Func.Wrapper`,
so its `funcID` is `abi.FuncIDWrapper` and `runtime.gorecover` does not count
its frame: a `recover` below a wrapper must recover, and a panic raised inside
a generated comparison belongs to the map operation that reached it.

### The method wrapper

A method of `T` with a value receiver produces exactly one generated function,
`(*T).M`, which loads the receiver through the pointer and calls the method.
Four fields can name a function and each names either the method or that
wrapper:

| descriptor | receiver | `Tfn` | `Ifn` |
| --- | --- | --- | --- |
| `T` | value | `T.M` | `T.M` if `T` is one pointer word, else `(*T).M` |
| `*T` | value | `(*T).M` | `(*T).M` |
| `*T` | pointer | `(*T).M` | `(*T).M` |

A pointer receiver method is already spelled `(*T).M` by the front end, so the
bottom row needs nothing generated. `gc` emits an `OTAILCALL` and leaves no
frame; nanogo has no tail call, so the frame exists and the `Wrapper` mark is
what keeps `recover` working across it.

`gc` emits a call to `runtime.panicwrap` for a nil receiver so that the message
names the method. [031](031-runtime-lowering.md) is the only place a runtime
symbol may be spelled and `panicwrap` is not in it, so the deref of a nil
pointer faults into the ordinary nil-pointer panic instead. The behaviour is
the same and the message is shorter.

### The equality and hash functions

They are one decision made twice. Two values that compare equal must hash alike
or a map loses keys it holds, so the set of types that needs a generated
equality function is the set that needs a generated hash function, and both are
`rtype`'s `algSpecial`: a struct with padding, a struct with a blank field, or
a struct or array with a part that is a string, a float or an interface.

The equality function is a chain of comparisons with an early return, which is
`gc`'s shape, and the short circuit is the point of it: a comparison that can
panic, which an interface field can, must not run after a comparison that
already answered false. A field is compared by its own kind and a nested struct
or array is walked rather than compared whole, so padding and blank fields are
skipped by construction rather than by arithmetic.

The hash function is `runtime.typehash`'s own walk: one call per leaf, in field
order, with the function chosen by the leaf's kind. Every leaf is one scalar,
so its width is one the runtime declares and `memequal_varlen` never applies.

`gc` collapses a run of adjacent memory-comparable fields into one `memequal`
or `memhash` call above a cost threshold and walks field by field below it
(`cmd/compile/internal/compare`, `EqStruct` and `Memrun`). nanogo always walks.
That changes how many instructions the function takes and not what it answers.

**What `rtype` has to do to use them.** Two hooks, and neither needs an IR
field:

1. `equalClosure` and `hashClosure`, for a type whose algorithm is
   `algSpecial`, name a closure `type:.eqfunc.<link string>` or
   `type:.hashfunc.<link string>` holding one word that relocates to
   `type:.eq.<link string>` or `type:.hash.<link string>`. The runtime-owned
   path stays as it is; `algClosure` refuses a name `rtsym` does not hold, and
   a generated name is not one.
2. `uncommonTail` writes the `Method` array rather than refusing it: `Mcount`,
   `Xcount`, `Moff`, and one sixteen-byte entry per method holding `Name` as a
   `NameOff`, `Mtyp` as a `TypeOff` to `ir.Method.Sig`, and `Ifn` and `Tfn` as
   `R_METHODOFF` relocations against the names in the table above. `Referenced`
   grows each method's `Sig`, so the closure the driver emits covers them.

   **The array has to be re-sorted, and `methodSet`'s doc does not say so.**
   `gc` orders methods **exported first**, then by name in byte order, then by
   package path for the unexported ones (`types.CompareSyms`), and
   `reflectdata` computes `Xcount` as a *binary search* for the first
   unexported name (`sort.Search` in `dcommontype`). `ir.Converter.methodSet`
   sorts by name and then by package path with no exported-first clause, so an
   array written in that order gives `Xcount` an answer from a binary search
   over an unsorted predicate, and `reflect` reports a method set that is not
   the type's. This is the same rule the descriptor lane found for an
   interface's `Imethod` array, and it has to be applied again here.

The driver decides the wrapper set from the closed descriptor list, which is
where `gc` decides it too, and it will find the equality and hash functions the
same way: a descriptor names the symbol and whoever writes the descriptor
defines it.

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
   `rtype.Reloc` target by name. **The descriptor itself is `AddNonPkgDef`, not
   `AddHashedDef`, and this spec was wrong to say otherwise.** `cmd/link` reads
   no name for a symbol in the hashed index space, so a hashed descriptor is
   nameless to the linker: no reference from another object resolves to it and
   nothing collects it into `runtime.typelinks`. `gc` agrees. It sets
   `AttrContentAddressable` on `type:.importpath.`, `type:.namedata.`,
   `runtime.gcbits.` and itabs, and never on the descriptor. So the descriptor
   is a named `dupok` definition and the data it points at is hashed, which is
   what `rtype` documents when it returns the descriptor first.

   **It is a non-package definition, and `AddDef` was the second mistake here.**
   A descriptor is `dupok` because every package that names a type writes one,
   and `cmd/link` deduplicates a `dupok` symbol by name in the non-package index
   space only. `loader.addSym` takes a package definition as unique by
   construction, overwrites the name table entry and keeps both copies:

   ```
   case pkgDef:
       // Defined package symbols cannot be dup to each other.
       l.symsByName[ver][name] = i
       addToGlobal()
       return i
   ```

   `gc` states the same rule from the writer's side, in `obj.isNonPkgSym`: a
   `dupok` symbol is a non-package symbol, so that it is deduplicated by name.
   A descriptor written as a package definition is therefore a second
   `type:*int32` in a binary that already holds the runtime's. Both copies are
   byte-identical and both are reachable, one by name and one by index from the
   parameter array of `type:func(*int32)`, and `runtime.SetFinalizer` compares
   two descriptors by address:

   ```
   fatal error: runtime.SetFinalizer: cannot pass *int32 to finalizer func(*int32)
   ```

   The two types in that message read the same because they are the same type.
   `internal/gotest/testdata/go/test/tinyfin.go` is the corpus file that found
   it and `internal/e2e/rtype_test.go` is the claim stated on its own.
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

**A channel literal is named now.** `ir.ChanDir.String` returns the whole
prefix (`chan`, `chan<-`, `<-chan`), so `typeName` writes it before the
element's and the three directions are three symbols. One case needs a
parenthesis: `chan (<-chan T)`, because `chan <-chan T` reads back as
`chan<- chan T`, which is a different type. A slice literal of channels is
lowered as a consequence, and `InvalidDir` is still refused.

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

**A function literal is named now.** `ir/rtype.go`'s `signatureName` writes the
parameter list and the result list, with one result bare and several
parenthesised and a variadic last parameter written from the slice's *element*,
because `func(...int)` and `func([]int)` have the same `Params`. No parameter
name is in it: two functions differing only in a parameter's name are one type.
The leading `func` is the caller's, because `gc` writes it for a function type
and leaves it out for an interface method, and the part after the name is the
same string. `new` of a function type and a closure capturing one are lowered as
a consequence.

### An interface's method list crossed the boundary, and the block moved

`ir.Type.Methods` is set on a **literal** interface now as well as on a defined
one, and each entry carries its signature, so `rtype/iface.go` writes the
`InterfaceType` header and the `Imethod` array: a package path, a method slice
pointing inside the descriptor's own symbol, and one eight-byte entry per
method holding a `NameOff` and a `TypeOff`.

The array's order is `gc`'s and not the IR's. `gc` sorts with
`types.CompareSyms`, which puts every exported name ahead of every unexported
one and then orders by name and package path; the IR sorts by name alone. The
runtime and `reflect` binary-search the array, so an array in a different order
is one a lookup misses. The two rules agree for every ASCII identifier and are
not the same rule, so the encoder sorts.

**A bug this replaced.** Every interface got thirty-two zero bytes, on the
reasoning that an empty interface has a nil package path and an empty method
slice. That is true of `any` and of `error`, whose descriptors the runtime
owns, and false of a **named** empty interface: `gc` writes the declaring
package's path and `reflect.Type.PkgPath` reads it. `type E interface{}`
reported no package for a type that has one.

**The block is gone.** An `Imethod`'s `Typ` is an offset to the descriptor of
the method's signature, that signature is a function literal, and the naming
function spells one now. An interface with methods reaches a descriptor,
defined or literal, and `internal/e2e` links and runs a gc-compiled importer
against a nanogo-compiled library declaring one.

A literal interface's own spelling is `interface {` and one `Name(sig)` per
method. An unexported method name is qualified by the package that declares it,
by import path in the link string and by package name in the name string,
because two packages may declare an unexported method of one name and they are
different methods.

**The order is one definition, not two.** The spelling and the `Imethod` array
are the same list read twice, so `ir.InterfaceMethodOrder` holds `gc`'s order
and `rtype/iface.go` calls it. `ir.ExportedName` holds the one exportedness
rule, which decides the qualifier, the sort and the flag byte of an
`internal/abi.Name`. A method named `Ärger` is exported and its first byte is
`0xC3`, so plain byte order puts it after every unexported name and `gc` puts it
before them. `gc`'s own spelling of that interface,
`interface { Read() int; Ärger() int; example.com/outer.flush() int }`, was read
out of a compiled object rather than reasoned about.

### The map's group type is synthesised, and only its name is missing

This spec said a map was refused because its descriptor "names the runtime's
group type, which specs/032 does not carry". Two things in that sentence were
wrong. The group type is not the runtime's: `gc` **synthesises** it, because
the collector needs a pointer map for a group and only a type carries one, and
it is written nowhere in the runtime's source. And nothing stopped this spec
from carrying it, because everything it is built from is already at the
boundary.

`rtype/map.go` builds it now. Go's map is a swiss table from 1.24 on, so the
group is

```go
type group struct {
    ctrl  uint64
    slots [abi.MapGroupSlots]struct {
        key  K
        elem E
    }
}
```

with `K` replaced by `*K` when the key is larger than `abi.MapMaxKeyBytes`, and
the same for the element. The substitution is in the *type* and not only in the
`MapIndirectKey` flag, because the group is what the collector scans: a group
built from a 200-byte array with the flag set has the collector read that array
as a pointer.

Every computed field of the `MapType` header follows from that one layout, and
they are computed together for a reason. The runtime finds a key with
`KeysOff + i*KeyStride` and an element with `ElemsOff + i*ElemStride`, so an
offset derived apart from its stride is a read at the wrong address rather than
a wrong answer anything reports. `GroupSize`, `KeysOff`, `KeyStride`,
`ElemsOff`, `ElemStride`, `ElemOff` and `Flags` are checked against the
descriptor `gc` emitted for the same map type, for eight map types covering an
indirect key, an indirect element, a zero-size element, an interface key and a
struct key.

`Hasher` is a func value like `Equal` and is chosen by the same algorithm, for
the reason [031](031-runtime-lowering.md) gives: two values that compare equal
must hash alike or a map loses keys it holds. A nil `Hasher` is **not** the
option a nil `Equal` is. The runtime calls it on every operation, so a key with
no hash is a map this compiler cannot describe rather than one that fails when
used.

**The group is named now, and the two spellings are not the two this spec
said.** `gc`'s link string is `map.group[K]V` and the *symbol* is
`type:noalg.map.group[K]V`, because `types.TypeSymName` is `LinkString` plus the
prefix and `typehash` is over `LinkString` alone. A link string carrying the
prefix would produce a hash `gc` never computed, and a type switch compares
hashes before it compares types.

Two facts on `ir.Type` carry it. `MapGroup` is the map the group belongs to, and
the spelling reads the *map's* key and element rather than the slot's: a key
past `abi.MapMaxKeyBytes` is a pointer in the slot and is itself in the name, so
`map[[200]byte]int` is `map.group[[200]uint8]int`. `NoAlg` marks a type the
compiler synthesised and gave no algorithms, and `TypeSymbol` adds the prefix
for it. The mark reaches a type built out of a marked one the way `gc`'s does,
through a struct's fields, an array's element and a pointer's element, and not
through a slice, which `gc` gives no equality algorithm to begin with. The
prefix is not decoration: without it the group's descriptor would merge with the
descriptor of a struct a program declared with the same two fields, and that
struct would be left with a nil `Equal`.

**What is left is one spelling below the group.** The group's slots are a
literal struct, which is the row still in the naming table above, so
`mapEmittable` asks the group for its descriptor and reports the group's own
reason. The map moves when the literal struct is spelled and needs nothing else.

### The naming function was the last block, and it is not one now

Three of the four spellings this spec asked for are written, and each was
checked against `gc` rather than against reasoning. The oracle is the running
binary: `rtype`'s corpus hashes the link string with `gc`'s own algorithm and
compares the result with the hash `gc` put in the descriptor `reflect` is
reading in the same process, so a spelling that differs from `gc`'s by one
character fails as a hash mismatch. Every channel, signature, literal interface
and slot group added here is a corpus row.

The fourth spelling, a literal struct, is the whole of what is left. It is the
one row of the naming table and it is the one thing a map is waiting on.

**Two refusals moved out of the tables and became linked programs.** An
interface with methods is compiled, linked and run against a gc-compiled
importer. A channel literal, `new` of a function type and a closure capturing
one are lowered.

**Four refusals in `ir/lower.go` named symbols that exist.** `rtsym` grew from
70 rows to 120 and the messages were not read again. `runtime.chanlen`,
`runtime.chancap`, `runtime.makemap` and `runtime.mapclear` are all in `rtsym`,
so what is missing for each is the lowering row that calls it. One of the four
named the wrong symbol: a range over a map said `runtime.mapiterinit`, which a
swiss map does not use. `mapiterinit` survives only as a `//go:linkname` shim
taking a `*runtime.linknameIter` rather than a `*maps.Iter`, and the two layouts
differ, so a row built for that name would write past the end of the frame slot.
[031](031-runtime-lowering.md) already recorded the correction and the refusal
contradicted it. [020](020-ir.md)'s row table still names `mapiterinit` and
`mapiternext` and is not corrected here.

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
