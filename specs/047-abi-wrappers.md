---
title: "ABI wrappers: ABI0, symabis, the assembly header, and the argument map"
status: draft
layer: back end
gate: G3
depends_on:
  - 000-decisions.md
  - 030-abi.md
  - 027-liveness-and-stackmaps.md
  - 050-driver.md
---

# ABI wrappers

nanogo already emits ABI0 **references**. `ssagen.morestackCallee` names
`runtime.morestack_noctxt` under ABI0, and `globalCallee` names every data
symbol under ABI0. What nanogo has never emitted is an ABI0 **definition**.
That one sentence is the whole gap this spec closes.

Eight of the twenty-seven standard library archives a minimal `func main() {}`
needs are refused by `driver.checkAssembly`, and all eight give one message.
They are `internal/abi`, `internal/bytealg`, `internal/chacha8rand`,
`internal/cpu`, `internal/runtime/atomic`, `internal/runtime/maps`,
`internal/runtime/sys` and `runtime`. That is the largest single group of
refusals in the closure, and it is one refusal covering four separate pieces of
missing work.

Everything below was measured against the installed toolchain, `go1.27.0` on
`darwin/arm64`, with `go tool compile -S`, `go tool nm`, `cmd/asm -gensymabis`,
and the sources in `GOROOT`. Nothing here is recalled. [030](030-abi.md)
records what a remembered ABI rule cost the project once already.

## What ABI0 is

ABI0 is the stack-only convention. Every receiver, argument and result is in
memory, at an offset in the callee's incoming argument area, and no register
carries a value across the boundary. `cmd/compile/abi-internal.md` states the
relationship directly:

> The ABI assignment algorithm above is equivalent to Go's stack-based ABI0
> calling convention if there are zero architecture registers.

That sentence is the design of this spec's first half. ABI0 is not a second
convention. It is [030](030-abi.md)'s convention with both register sets empty.

### The layout recurrence

Read `abi-internal.md`'s assignment algorithm with `NI = NFP = 0`. Every
register assignment fails, so step 4 fires for every value, and what is left is
this walk over the declared lists:

```
off ← 0
for each of the receiver, then each argument, in declaration order:
    off ← roundUp(off, align(T))
    place the value at off
    off ← off + size(T)

off ← roundUp(off, 8)               // the pointer-alignment field

for each result, in declaration order:
    off ← roundUp(off, align(T))
    place the value at off
    off ← off + size(T)

argsize = roundUp(off, 8)           // the trailing pointer-alignment field
```

There is no spill part. The spill part of [030](030-abi.md)'s argument area
holds one slot per value that travelled in a register, and in ABI0 no value
does.

The pointer-alignment field between the arguments and the results is not
cosmetic and it is not derivable from the types. `func g1(a int8) (r int8)`
places `a` at 0 and `r` at **8**, not at 1, and its area is 16 bytes and not 2.
A walk that ran the results straight on from the arguments would put every
result of every ABI0 function at the wrong offset whenever the arguments do not
end on a word.

### The measured cases

Each row is the ABI0 area of a real function, read out of `go tool compile -S`
by the offsets a `gc`-generated wrapper loads and stores, and cross-checked
against the `args=` field of the wrapper's `TEXT` line.

| Signature | Placement | `argsize` |
| --- | --- | --- |
| `getisar0() uint64` | `ret` 0 | 8 |
| `g1(a int8) (r int8)` | `a` 0, `r` 8 | 16 |
| `g2(a int8, b int8) (r1 int8, r2 int32)` | `a` 0, `b` 1, `r1` 8, `r2` 12 | 16 |
| `sysctlEnabled(name []byte) bool` | `name` 0, 8 and 16, `~r0` 24 | 32 |
| `g4(a ...int) int` | `a` 0, 8 and 16, `~r0` 24 | 32 |
| `f(a int8, b int64, c string, d float64) (r1 int32, r2 [3]int64)` | `a` 0, `b` 8, `c` 16 and 24, `d` 32, `r1` 40, `r2` 48 | 72 |

The last row is worth following through the recurrence, because it exercises
every clause. `a` at 0 leaves `off` at 1. `b` needs 8-byte alignment, so it
goes to 8 and leaves `off` at 16. The string is two words at 16 and 24. `d` is
at 32 and leaves `off` at 40, which is already a multiple of 8, so the
pointer-alignment field adds nothing. `r1` is at 40 and leaves `off` at 44.
`r2` needs 8-byte alignment, so it goes to 48 and leaves `off` at 72. The area
is 72 bytes, which is `0x48`, and that is what `gc` prints:

```
p.gof STEXT dupok nosplit size=96 align=0x0 args=0x48 locals=0x68 funcid=0x17
	TEXT	p.gof(SB), DUPOK|NOSPLIT|ABIWRAPPER|LINKNAME, $112-72
	MOVB	p.a(FP), R0
	MOVD	p.b+8(FP), R1
	MOVD	p.c+16(FP), R2
	MOVD	p.c+24(FP), R3
	FMOVD	p.d+32(FP), F0
	CALL	p.gof(SB)
	...
	MOVD	R4, p.r2+64(FP)
	MOVW	R0, p.r1+40(FP)
```

**Variadic is not a special case.** `g4(a ...int) int` places one `[]int` at 0,
8 and 16 and the result at 24. The convention sees a slice parameter and
nothing else. `makeABIWrapper` sets `call.IsDDD` from
`fn.Type().IsVariadic()`, so the wrapper passes its own slice through rather
than building a second one.

### The zero-size rule, and where nanogo's walk already parts from `gc`

`abi-internal.md`'s assignment step 2 is explicit and is the one clause a
reader skips:

> If T has zero size, add T to the stack sequence S and return.

with the reason stated in the same document: this is what makes the algorithm
ABI0-equivalent, and such a value "does result in alignment padding on the
stack in ABI0."

`ssa.abiAssigner.place` does not do this. `ABILeaves` of a zero-size type
returns no parts and reports the flatten complete, so the `fits` loop never
runs, `fits` stays true, and the value is recorded as register-assigned with a
zero-width spill slot. It never reaches `as.stack`, so it contributes no
alignment.

The divergence is measurable, and it is measurable **today, in ABIInternal**,
before any of this spec is built:

```go
func f1(a [3]int8, b [0]int64, c [3]int8) int8   // gc: a at FP+0, c at FP+8
func f2(a [3]int8,               c [3]int8) int8 // gc: a at FP+0, c at FP+3
```

`gc` compiles the first to `MOVB z.c+8(FP), R2` and the second to
`MOVB z.c+3(FP), R2`. The zero-size `[0]int64` moves `c` by five bytes because
it forces the running offset up to the next multiple of 8. nanogo's walk skips
it and would place `c` at 3 in both. In ABI0 the same signature gives an area
of 24 with the zero-size parameter and 16 without, and every result moves with
it.

The rule the walk needs is one clause and not a rewrite: a value whose type has
zero size takes a stack slot at `roundUp(off, align(T))` of width zero, in both
ABIs, and takes no register and no spill slot. Adding it is a prerequisite of
this spec and a fix to [030](030-abi.md)'s existing pass.

### The arm64 frame the offsets are relative to

`abi-internal.md`'s arm64 section gives the frame, and the ABI0 area sits
inside the caller's:

```
   higher addresses
        ...
   +----------------------------+
   | caller's locals            |
   +----------------------------+
   | outgoing argument area     |  ← the callee's ABI0 offset 0 is here,
   |   [RSP+8, RSP+8+argsize)   |    at RSP+8 of the caller
   +----------------------------+
   | caller's return PC         |  ← RSP after the caller's prologue
   +----------------------------+
   | caller's frame pointer     |  ← RSP-8
   +----------------------------+
        ...
   lower addresses
```

The arm64 `CALL` does not move `RSP`. It puts the return address in R30 and
the callee's prologue subtracts its own frame size. So the callee's pseudo-`FP`
resolves to `RSP_callee_after_prologue + framesize + 8`, which is the caller's
`RSP + 8`, which is where the caller wrote the arguments. The identity
`FP == caller RSP + 8` is what makes the wrapper's `MOVB R0, 8(RSP)` and the
assembly's `MOVD R0, ret+0(FP)` name the same word, and it was checked against
`internal/cpu.getisar0` and against the six-argument case above.

`abi-internal.md` states that this stack layout "is used by both register-based
(ABIInternal) and stack-based (ABI0) calling conventions", so the frame is not
a second layout either.

## Which direction each wrapper goes

A wrapper exists because a symbol's name is claimed under two ABIs. The ABI is
half of a symbol's identity in `goobj` and in `cmd/link`, so `p.f` under ABI0
and `p.f` under ABIInternal are two symbols, and a reference under the wrong
one resolves to nothing.

```mermaid
flowchart TD
  asm["assembly TEXT for p.f"] --> gen["cmd/asm -gensymabis"]
  gen --> file["symabis file<br/>def p.f ABI0"]
  file --> read["ssagen.SymABIs.ReadSymABIs"]
  read --> apply["GenABIWrappers<br/>fn.ABI = defABI<br/>fn.ABIRefs |= refs<br/>fn.ABIRefs.Set(ABIInternal)"]
  link["//go:linkname on p.f"] --> callable["fn.ABIRefs |= ABISetCallable"]
  callable --> apply
  apply --> need["need = fn.ABIRefs &^ ABISetOf(fn.ABI)"]
  need --> zero{"need == 0 ?"}
  zero -->|yes| none["no wrapper"]
  zero -->|no| make["makeABIWrapper, one per ABI in need"]
```

`ssagen.GenABIWrappers` in `cmd/compile/internal/ssagen/abi.go` is the whole
decision. Three lines of it carry the rule:

- `fn.ABI = defABI` where the symabis file has a `def` for the symbol, and the
  declaration has an empty body. A `def` for a declaration that also has a Go
  body is the error `%v defined in both Go and assembly`.
- `fn.ABIRefs.Set(obj.ABIInternal, true)` unconditionally, with the comment
  "Assume all functions are referenced at least as ABIInternal, since they may
  be referenced from other packages."
- `fn.ABIRefs |= obj.ABISetCallable` when `sym.Linkname != ""` and the symbol
  is defined in this package and is not cgo-exported.

`forEachWrapperABI` then computes `need := fn.ABIRefs &^ obj.ABISetOf(fn.ABI)`
and calls `makeABIWrapper` once per ABI still in `need`. Two shapes come out.

### A Go caller calling an assembly function

The declaration is bodyless, the symabis file says `def p.f ABI0`, so
`fn.ABI` is ABI0 and the unconditional ABIInternal reference is what `need`
holds. The wrapper's **own** ABI is ABIInternal. It takes its arguments in
registers, stores them into the outgoing area at ABI0 offsets, calls the
assembly, and reads the results back out of the area into registers.

```
internal/cpu.getisar0 STEXT dupok nosplit size=32 args=0x0 locals=0x18 funcid=0x17
	TEXT	internal/cpu.getisar0(SB), DUPOK|NOSPLIT|ABIWRAPPER|ABIInternal, $32-0
	MOVD.W	R30, -32(RSP)
	MOVD	R29, -8(RSP)
	SUB	$8, RSP, R29
	FUNCDATA	$0, gclocals·g5+hNtRBP6YXNjfog7aZjQ==(SB)
	FUNCDATA	$1, gclocals·g5+hNtRBP6YXNjfog7aZjQ==(SB)
	PCDATA	$0, $-2
	PCDATA	$1, $0
	CALL	internal/cpu.getisar0(SB)
	MOVD	8(RSP), R0
	MOVD	-8(RSP), R29
	MOVD.P	32(RSP), R30
	RET	(R30)
	rel 12+4 t=R_CALLARM64 internal/cpu.getisar0+0
```

The `CALL` prints the same name as the `TEXT`. The relocation resolves to the
ABI0 symbol because the reference carries the ABI, not because the name
differs.

### An assembly caller calling a Go function

The Go function has a body, so `fn.ABI` is ABIInternal, and the ABI0 entry in
`need` comes from one of two places: a `ref p.f ABI0` line the assembler
recorded because an assembly file names the symbol, or `ABISetCallable` from a
`//go:linkname`. The wrapper's **own** ABI is ABI0. It loads its arguments out
of its own ABI0 area, calls the Go function under ABIInternal, and writes the
results back into the ABI0 area.

That is the `p.gof` listing quoted above. All twenty-one ABI0-own wrappers in
the seven packages other than `runtime` carry `LINKNAME`, so `//go:linkname` is
what triggers this direction in practice. `runtime`'s 328 were not inspected,
and its symabis file has 261 `ref ... ABI0` lines, so a large part of them
probably come from the `ref` path instead.

### The wrapper is a Go function, and `gc` compiles it as one

`makeABIWrapper` builds an `ir.Func` whose body is one call and one return,
runs it through `typecheck`, and lets the ordinary pipeline compile it. It sets
`SetABIWrapper(true)`, `SetDupok(true)` and `fn.Pragma |= ir.Nosplit`, and it
emits a tail call only when the signature has no receiver, no parameters and no
results.

The consequence is visible in `internal/runtime/atomic`. All forty-nine of its
wrappers reach `-S` with **no** `CALL` in them, because
`internal/runtime/atomic.Xadd` and its neighbours are `gc` intrinsics and the
call inside the wrapper was intrinsified away. The wrapper is an ordinary
function that the ordinary optimiser rewrote.

`ir.Func.Wrapper` is already set by every function `ssagen/wrapper.go` builds,
so the `funcID` is `abi.FuncIDWrapper` and `runtime.gorecover` skips the frame.
An ABI wrapper needs the same mark for the same reason [030](030-abi.md) gives.

## What `cmd/asm` actually refuses

[030](030-abi.md) says "`cmd/asm` refuses the `<ABIInternal>` marker outside
the runtime, so every assembly definition in an ordinary package is ABI0."
**That is false**, and the measurement below depends on the correct rule.

`asm/internal/asm.Parser.symRefAttrs` accepts `<ABI0>`, `<ABIInternal>` and
`<>` and rejects a selector only when `p.allowABI` is false. `allowABI` is
`objabi.LookupPkgSpecial(ctxt.Pkgpath).AllowAsmABI`, and the list in
`cmd/internal/objabi/pkgspecial.go` is:

```
runtime, reflect, syscall,
internal/bytealg, internal/chacha8rand,
internal/runtime/syscall/linux, internal/runtime/syscall/windows,
internal/runtime/startlinetest, internal/runtime/maps
```

Assembly in any of those packages may define a symbol directly as ABIInternal,
and then no wrapper is needed for the Go-calls-assembly direction at all. There
is one further rule: `cmd/asm` errors with `TEXT %q: ABIInternal requires
NOSPLIT` for an ABIInternal definition without the flag, because `obj` needs a
fixed frame size for such a symbol.

This is why `internal/chacha8rand` needs zero wrappers and `internal/bytealg`
and `internal/runtime/maps` need only the ABI0 direction.

## `symabis`

The `go` command compiles a package with assembly in two passes. It writes an
empty `go_asm.h`, runs the assembler with `-gensymabis` to produce a symabis
file, runs the compiler with `-symabis` and `-asmhdr`, then runs the assembler
again for real against the header the compiler wrote. The logged command line
for `internal/cpu` is:

```
echo -n > $WORK/b001/go_asm.h
asm -p internal/cpu -I $WORK/b001/ -I $GOROOT/pkg/include \
    -D GOOS_darwin -D GOARCH_arm64 -shared -std -gensymabis \
    -o $WORK/b001/symabis ./cpu.s ./cpu_arm64.s
compile ... -symabis $WORK/b001/symabis ... -asmhdr $WORK/b001/go_asm.h <go files>
asm ... -o $WORK/b001/cpu.o ./cpu.s
asm ... -o $WORK/b001/cpu_arm64.o ./cpu_arm64.s
go tool pack r $WORK/b001/_pkg_.a $WORK/b001/cpu.o $WORK/b001/cpu_arm64.o
```

### The format

`ReadSymABIs` documents it and the file confirms it: whitespace-separated
fields, one record per line, blank lines and lines starting with `#` skipped.
The first field is `def` or `ref`, the second is the linker symbol name, the
third is the ABI name that `obj.ParseABI` accepts, which is `ABI0` or
`ABIInternal`. Any other verb is a fatal error, and so is a line whose field
count is not three.

Two real files, generated by the commands above:

```
def internal/cpu.getisar0 ABI0
def internal/cpu.getisar1 ABI0
def internal/cpu.getpfr0 ABI0
def internal/cpu.getMIDR ABI0
```

```
def internal/runtime/maps.memHash32AES ABIInternal
ref internal/runtime/maps.aeskeysched ABI0
def internal/runtime/maps.memHash64AES ABIInternal
ref internal/runtime/maps.aeskeysched ABI0
def internal/runtime/maps.memHashAES ABIInternal
ref internal/runtime/maps.aeskeysched ABI0
```

A `ref` may name a data symbol, as `aeskeysched` does. Such a line reaches
`s.refs` and is then never matched against a function, because
`GenABIWrappers` walks `typecheck.Target.Funcs` and looks each function's own
name up. A `ref` for a name that is not a function in this package has no
effect.

### What the compiler must do with it

Three things, and only the first is about wrappers.

1. Set the ABI of each declaration the file `def`s, and take the reference set
   from the `ref` lines, so that `need` can be computed.
2. Treat a `def` for a name that has a Go body as an error. It is the source
   of `%v defined in both Go and assembly`.
3. Treat a `def` for a name with no Go declaration at all as nothing. It
   happens: `internal/abi`'s `abi_test.s` defines
   `internal/abi.FuncPCTestFn`, whose only Go declaration is in
   `export_test.go`, which is not in the ordinary build. The compiler writes no
   wrapper and no argument map for it, and `go tool nm` over the built archive
   shows `internal/abi.FuncPCTestFn.args_stackmap` as **U**, undefined. It
   never resolves in an ordinary link because the symbol is unreachable.

## `-asmhdr`

The header exists because an assembly file cannot compute a Go struct's field
offsets. `runtime/asm_arm64.s` writes `MOVD (g_stack+stack_lo)(g), R2`, and
`g_stack` and `stack_lo` are `#define`s the compiler produced from the Go
declaration of `type g struct`.

`gc.dumpasmhdr` in `cmd/compile/internal/gc/export.go` is short, and the rule
is exactly what it does. It walks `typecheck.Target.AsmHdrDecls`, which
`noder` fills with **every** package-scope name whose `Op` is `OLITERAL` or
`OTYPE`, collected only when `-asmhdr` is set. For each:

- a blank name is skipped
- a constant of float or complex kind is skipped, and every other constant
  becomes `#define const_<Name> <ExactString of the value>`
- a type that is not a struct, or is a map's internal struct, or is a
  function's argument struct, is skipped. Every other struct becomes
  `#define <Name>__size <size>` followed by
  `#define <Name>_<Field> <offset>` per non-blank field

Nothing is filtered by whether the assembly refers to it. Real output, from
`go tool compile -asmhdr` on `internal/cpu`:

```
// generated by compile -asmhdr from package cpu

#define CacheLinePad__size 128
#define option__size 32
#define option_Name 0
#define option_Feature 16
#define option_Specified 24
#define option_Enable 25
#define const_CacheLinePadSize 128
```

and the first lines of `internal/abi`'s, which is 251 lines:

```
// generated by compile -asmhdr from package abi

#define RegArgs__size 392
#define RegArgs_Ints 0
#define RegArgs_Floats 128
#define RegArgs_Ptrs 256
#define RegArgs_ReturnIsPtr 384
#define const_IntArgRegs 16
#define const_FloatArgRegs 16
```

Sizes for the eight packages are 251, 12, 15, 9, 30, 57, 19 and 2622 lines, in
the order they are listed at the top of this spec.

Two details a writer gets wrong. The header names the package by its **name**
and not its path, so `internal/cpu` prints `package cpu`. And a string constant
is written with `constant.Value.ExactString`, which is a Go-quoted string, so
`internal/runtime/sys` emits `#define const_ntz8tab "\b\x00\x01\x00..."` for a
256-byte table. A writer that spells a constant differently from `gc` produces
an assembly file that assembles to different code, and nothing reports it.

The header must be written even for a package that owes no wrapper. It is the
input to the second assembler run, and the assembler fails to find the
`#define` otherwise.

## `args_stackmap` and `arginfo`, the fourth piece

[030](030-abi.md) names three missing pieces. There are four.

`cmd/internal/obj/plist.go` appends, to **every** ABI0 text symbol the
assembler produces under the package's own prefix that does not already have
one, a `FUNCDATA $FUNCDATA_ArgsPointerMaps` reference to
`<symbol>.args_stackmap`. The assembly object therefore holds an undefined
reference to a symbol only the compiler can define. `runtime.addmoduledata` is
the single exception, and a symbol whose ABI is not ABI0 is skipped with the
comment "better to have no stackmap than an incorrect/lying stackmap".

`gc/compile.go` defines it. For a bodyless declaration whose `fn.ABI` is ABI0,
it calls `liveness.WriteFuncMap` and `ssagen.EmitArgInfo`. `WriteFuncMap`
writes, into `<symbol>.args_stackmap`, marked `RODATA|LOCAL` and linkname-
visible so that assembly may name it:

```
uint32  nbitmap        // 2 when the function has results, else 1
uint32  bv.N           // bits, which is ArgWidth / PtrSize
bitvec  args           // pointer words of the ABI0 argument part
bitvec  args + results // only when nbitmap is 2, and it is cumulative
```

The second map is cumulative because `WriteFuncMap` keeps writing into the same
`bitvec`. `internal/cpu.getisar0.args_stackmap` is 10 bytes, which is
`4 + 4 + 1 + 1` for a function with one word of area and no pointer in it.

**Every package with an ABI0 `def` needs this**, whether or not it needs a
wrapper. By the measurement below that is `internal/cpu`,
`internal/runtime/atomic`, `internal/runtime/sys`, `internal/abi` and
`runtime`. Without it the link fails on an undefined symbol, which at least is
loud. With a wrong map the collector follows a word that is not a pointer,
which is not.

## What each of the eight packages actually needs

Measured by compiling each package with the real `gc` under the exact command
line the `go` command sends, and counting `ABIWRAPPER` symbols in `-S` output.
A wrapper whose `TEXT` flags include `ABIInternal` has ABIInternal as its own
ABI and therefore calls ABI0 assembly. One whose flags do not is an ABI0
definition and is called from assembly.

| Package | `def` ABI0 | `def` ABIInternal | wrappers, Go→asm | wrappers, asm→Go |
| --- | --- | --- | --- | --- |
| `internal/abi` | 1 | 0 | 0 | 0 |
| `internal/chacha8rand` | 0 | 1 | 0 | 0 |
| `internal/cpu` | 4 | 0 | 4 | 1 |
| `internal/runtime/sys` | 3 | 0 | 3 | 0 |
| `internal/runtime/atomic` | 49 | 0 | 49 | 0 |
| `internal/bytealg` | 0 | 10 | 0 | 3 |
| `internal/runtime/maps` | 0 | 3 | 0 | 17 |
| `runtime` | 112 | 17 | 144 | 328 |

`internal/abi`'s single ABI0 `def` is the `export_test.go` case above, so it
produces nothing. `internal/runtime/atomic`'s forty-nine wrappers match its
forty-nine ABI0 defs one for one, with no unmatched name in either direction.

## The design for nanogo

### ABI0 is `ABIWalk` with empty register sets, and not a second walk

`ssa.ABIWalk` already takes the register sets off the `Target`
(`t.ArgRegs`, `t.ResultRegs`). Give the walk an ABI parameter that selects
between the target's register sets and empty ones, and ABI0 falls out. The
three conditions that make this exact rather than approximate:

- The pointer-alignment field between the arguments and the results is already
  there. `ABIWalk` does `as.stack = abiRoundUp(as.stack, ir.PtrSize)` between
  the two `placeAll` calls, and that is the field measured by `g1`.
- The trailing pointer-alignment field is already there. `finish` returns
  `abiRoundUp(base+as.spill, ir.PtrSize)`, and with nothing spilled that is
  `roundUp(stack, 8)`.
- The zero-size rule is **not** there, and the section above says what to add.

This keeps [000](000-decisions.md) decision 5 intact. The ABI stays a target
parameter and no second placement walk exists to drift from the first.

The alternative considered and rejected is a separate `ABI0Walk` that lays out
a signature directly from the type list. It is ten lines and it is wrong for
one reason: it would be a second statement of the same rule, and the two would
be checked against each other by nothing. [030](030-abi.md)'s record of what
happened the last time one boundary had two placements is the argument.

### Who owns what

| Piece | Owner | State |
| --- | --- | --- |
| the ABI0 register sets and the zero-size rule | `ssa/abi.go` | not built; one parameter and one clause |
| a function's own ABI | `ir.Func` | not built; a new field |
| a call's callee ABI | `ssa.Value` beside `Sig` | not built; see below |
| building the wrapper as IR | `ssagen/wrapper.go` | the machinery is built; `finishWrapper` is the shape |
| reading the symabis file | `driver` | not built |
| writing the assembly header | `driver` | not built |
| `<sym>.args_stackmap` and `<sym>.arginfo0` | `ssagen` beside `stackmap.go` | not built |
| the ABI on the emitted text symbol | `ssagen.Options.ABI` | **declared and ignored** |
| ABI0 references to runtime symbols | `rtsym`, `ssagen/reloc.go` | built |

`ssagen.Options.ABI` is documented as "the calling convention the symbol is
defined with" and `driver.compileFunc` passes `obj.ABIInternal` into it, but
`emitter.result` hardcodes `ABI: obj.ABIInternal` and never reads the field. It
is a declared-and-unread field of the kind [050](050-driver.md) records for six
driver flags. It must be read before any of this works.

### The one new SSA need

A wrapper's own ABI decides how `AssignABI` places its parameters and results.
That is one field on `ir.Func`, threaded to the pass.

The **callee's** ABI decides how the wrapper's outgoing area is laid out, and
nothing in the graph carries it. `ssa.Value.Sig` carries the callee's signature
already, which is what [030](030-abi.md) added to size the outgoing area from
the callee rather than from the call site's reads. It needs one more piece of
the callee's identity beside it. Without it the ABIInternal wrapper sizes and
lays out its outgoing area by the ABIInternal walk while the assembly reads
ABI0 offsets, which is a caller writing arguments where the callee reads none.
That failure is silent inside a nanogo-only program for the same reason every
entry in [030](030-abi.md)'s "What was wrong" list is silent.

### The wrapper is built as IR and compiled by the ordinary pipeline

`ssagen/wrapper.go` builds method wrappers as `ir.Func` with a body of one call
and one return, and `driver.addGenerated` compiles them through `compileFunc`
with every other function. An ABI wrapper is the same shape with a different
ABI on the symbol, so `finishWrapper` is close to the whole body already.

This is also the answer to a correctness question that is easy to miss. A
pointer argument of an ABIInternal wrapper is live across the inner call, so
the collector must find it. `gc` spills it into the wrapper's own ABIInternal
spill slot and the arguments bitmap covers it:

```
	MOVD	R2, p.c+40(FP)      // the string's pointer, into its spill slot
	...
	CALL	p.asmf(SB)
```

nanogo's register allocator would spill the same value into a frame slot and
[027](027-liveness-and-stackmaps.md)'s locals bitmap would cover it. Both are
correct, and neither needs a special case, because the wrapper went through the
ordinary liveness pass. A wrapper emitted by a shortcut path would need the
whole question answered again.

The mirror question is the ABI0 wrapper's own arguments bitmap. Every argument
of an ABI0 function is in memory at a known offset for the whole call, so the
arguments bitmap has to describe the ABI0 area with every pointer word marked.
[027](027-liveness-and-stackmaps.md) builds that map from the ABI placement, so
it follows from the placement being right.

### `NOSPLIT`, resolved rather than noted

`makeABIWrapper` sets `ir.Nosplit` on every wrapper, with a comment saying the
author could not make `all.bash` pass without it. [035](035-goroutines-and-stack-growth.md)
forbids nanogo from claiming the property, and `ssagen.result` deliberately
does not set `SymFlagNoSplit`, because nanogo does not compute the nosplit
budget.

The conflict resolves in favour of setting the flag. `cmd/link`'s
`doStackCheck` in `ld/stackcheck.go` runs unconditionally over `ctxt.Textp`,
reads `ldr.IsNoSplit(sym)` off the symbol, walks the call graph, and compares
each height against `objabi.StackNosplit(race) - callSize - 8` on arm64. It
does not care which compiler produced the object. Setting the flag therefore
**delegates** the check to the linker rather than skipping it, and an overflow
is a link error naming the chain. [035](035-goroutines-and-stack-growth.md)'s
objection is to an unchecked claim, and this claim is checked.

The reverse choice is also not silent. A wrapper without `NOSPLIT` emits the
stack-growth prologue, and reaching it from a context where the stack may not
grow throws `morestack on g0` at run time. Loud, but at run time and far from
the cause. The flag is the better answer and the linker is why.

### What nanogo refuses by name in the first version

Each of these is a shape `gc` handles and nanogo will not, and each is refused
with a message naming this spec. A silent wrong answer is worse than a
refusal.

| Refused | Why |
| --- | --- |
| a wrapper for a method | `makeABIWrapper` errors with `makeABIWrapper support for wrapping methods not implemented`, so refusing costs no compatibility |
| `//go:cgo_export_static` and `//go:cgo_export_dynamic` | the cgo branch of `GenABIWrappers` rewrites the pragma and suppresses the wrapper; cgo is out of scope by [000](000-decisions.md) decision 8 |
| `//go:cgo_unsafe_args` | it pins the function to ABI0 and propagates the linkname attribute to the ABI0 symbol, and it exists because the callee walks arguments by offset |
| a `def` line whose ABI is neither `ABI0` nor `ABIInternal` | `obj.ParseABI` is the only accepted spelling and a fourth value would be guessed |
| a constant whose kind the header writer cannot spell exactly as `gc` does | an approximation reaches the assembler as a different number |
| `runtime` itself | it needs the write barriers of [034](034-write-barriers.md), the nosplit budget of [035](035-goroutines-and-stack-growth.md), and 472 wrappers |

### The runtime detection gate, which is not optional

`driver.checkSupported` refuses "the runtime" when the `-+` flag is present.
**The `go` command never sends `-+`.** There is no occurrence of the flag in
`cmd/go`, and `gc` derives the property instead:

```go
// cmd/compile/internal/base/flag.go
if Flag.Std && objabi.LookupPkgSpecial(Ctxt.Pkgpath).Runtime {
    Flag.CompilingRuntime = true
}
```

All eight packages in this spec are in `objabi.runtimePkgs`, so `gc` compiles
all eight with the runtime rules on, and nanogo's refusal fires for none of
them. Today the assembly refusal hides that. The hole is already open for a
runtime package with no assembly in it, and `internal/byteorder`,
`internal/goarch` and `internal/runtime/math` are three that nanogo reaches
without meeting either refusal.

A blanket refusal of `objabi.runtimePkgs` is the wrong repair, for two reasons.
It would refuse packages nanogo compiles today, and it is far coarser than the
property that matters. What `Flag.CompilingRuntime` changes in `gc` is mostly
optimization and instrumentation policy at `base/flag.go:366`. The two clauses
that decide a program's meaning are `noder.noder.pragma`, which permits
`//go:systemstack`, `//go:nowritebarrier`, `//go:nowritebarrierrec` and
`//go:yeswritebarrierrec` only in a runtime package, and
`liveness.plive.go:504`, which treats every function in a runtime package as
though it were nosplit.

So the gate is per directive and per function, which is where
[016](016-directives-and-pragmas.md) and
[035](035-goroutines-and-stack-growth.md) already put it. Counted over the
files each package actually builds on `darwin/arm64`:

| Package | `//go:nosplit` | write-barrier, `systemstack`, `uintptr*` |
| --- | --- | --- |
| `internal/abi` | 2 | 0 |
| `internal/bytealg` | 0 | 0 |
| `internal/chacha8rand` | 2 | 0 |
| `internal/cpu` | 0 | 0 |
| `internal/runtime/atomic` | 50 | 0 |
| `internal/runtime/maps` | 1 | 0 |
| `internal/runtime/sys` | 3 | 0 |
| `runtime` | 418 | 204 |

The cut is clean. `runtime` is the only package that carries a write barrier or
`systemstack` directive, and it is out of scope for every stage of this spec
anyway. None of the other seven carries one, so none of them needs
[034](034-write-barriers.md) built before it compiles correctly.

`//go:nosplit` is a different matter and it is not this spec's to close.
[035](035-goroutines-and-stack-growth.md) records that nanogo emits a
stack-growth check in a function marked `//go:nosplit`, and
`TestDirectivesAreRecordedButNotHonoured` gates the gap rather than a refusal.
Five of the seven carry the directive, so lifting the assembly refusal makes
that gap reachable for the first time. It must become a refusal by name before
those five compile, and the refusal belongs to
[035](035-goroutines-and-stack-growth.md) and reads the function's own pragma.
`internal/bytealg` and `internal/cpu` carry none and are unaffected.

## The staged plan

Each stage is independently testable and independently valuable, and each names
the packages whose assembly refusal it lifts. Lifting that refusal is not the
same as the package compiling: [035](035-goroutines-and-stack-growth.md)'s
`//go:nosplit` gate stands behind it for five of the seven, and each stage says
so. `runtime` is out of scope for all of them.

### Stage 0: the runtime gate

Derive "compiling a runtime package" from `-std` and the package path rather
than from the `-+` flag the `go` command never sends. Transcribe
`objabi.runtimePkgs` and gate the copy against `GOROOT` the way `rtsym` gates
its symbol table. Then refuse two things by name and nothing else: `runtime`
itself, and a function carrying a directive nanogo cannot honour. The second
refusal is [035](035-goroutines-and-stack-growth.md)'s and covers
`//go:nosplit`, and [034](034-write-barriers.md)'s and covers the write barrier
and `systemstack` directives, which only `runtime` uses.

**Test.** A compile of `runtime` is refused naming the runtime rules, on the
command line the `go` command actually sends, with no `-+` on it. A compile of
a package that is in `runtimePkgs` and carries no such directive is **not**
refused by this gate, which is what keeps `internal/byteorder` and
`internal/goarch` compiling. A drift test reads `runtimePkgs` out of `GOROOT`
and fails when the transcribed list disagrees.

**Lifts the assembly refusal for** nothing. It repairs a refusal that fires for
no package today, and it decides per package which of the eight are gated by
something other than assembly. Five of the seven are gated by `//go:nosplit`,
and closing that is [035](035-goroutines-and-stack-growth.md)'s work rather
than this spec's.

### Stage 1: symabis, the header, and the argument map

Read `-symabis` into a def and ref table. Write `-asmhdr` from the package's
constants and struct types. Emit `<sym>.args_stackmap` and `<sym>.arginfo0` for
every bodyless declaration whose recorded ABI is ABI0. Lift
`driver.checkAssembly` for a package that needs no wrapper, and keep refusing
one that does.

**Test.** Byte-compare nanogo's `go_asm.h` against `gc`'s for each of the eight
packages, which is a fixed set of eight files and catches the whole class.
Compare the `args_stackmap` bytes against `gc`'s for a signature with a pointer
in the arguments, one with a pointer only in the results, and one with none.
Build `internal/abi` and `internal/chacha8rand` under a real
`go build -toolexec=nanogo` and run a program that uses them.

**Lifts the assembly refusal for** `internal/abi` and `internal/chacha8rand`,
which need no wrapper. Both carry `//go:nosplit`, so both still wait on
[035](035-goroutines-and-stack-growth.md).

### Stage 2: the ABIInternal wrapper, Go calling assembly

Add the ABI0 register sets and the zero-size rule to `ssa.ABIWalk`. Add the ABI
to `ir.Func` and to the call site. Read `ssagen.Options.ABI`. Generate one
ABIInternal wrapper per bodyless declaration the symabis file `def`s as ABI0.

**Test.** The differential test [030](030-abi.md) already uses: compile the
middle of three packages with nanogo and the other two with `gc`, where the
middle package declares a function defined in a hand-written ABI0 `.s` file and
calls it. Compare every line of output against an all-`gc` build. Cover the six
measured signatures of the table above, plus the zero-size case, because each
of them is silent inside a nanogo-only program.

**Lifts the assembly refusal for** `internal/runtime/atomic` (49 wrappers),
`internal/runtime/sys` (3), and four of `internal/cpu`'s five. nanogo has no
intrinsics, so its `atomic` wrappers are real calls into the ABI0 assembly
where `gc`'s are intrinsified. Slower, and the same answer. `atomic` and `sys`
still wait on [035](035-goroutines-and-stack-growth.md).

### Stage 3: the ABI0 wrapper, assembly calling Go

Emit a text symbol whose own ABI is ABI0. Compute `ABIRefs` from the symabis
`ref` lines and from `//go:linkname`, and generate one ABI0 wrapper per
function still in `need`.

**Test.** The same three-package differential build, with a hand-written `.s`
file that calls a Go function under ABI0 and prints what it got. A separate
check that the object holds two definitions of one name under two ABIs and that
`go tool nm` prints both.

**Lifts the assembly refusal for** `internal/bytealg` (3),
`internal/runtime/maps` (17), and the `sysctlEnabled` wrapper that completes
`internal/cpu`.

Seven of the eight after stage 3, and two of those seven compile from that
point. `internal/bytealg` and `internal/cpu` carry no `//go:nosplit` and are
held by nothing else this spec knows of. The other five are held by
[035](035-goroutines-and-stack-growth.md) alone, which is a smaller gate than
the one they are behind today and is one gate rather than five.

### Stage 4: not this spec

`runtime` needs 472 wrappers, the write barriers of
[034](034-write-barriers.md), the nosplit budget of
[035](035-goroutines-and-stack-growth.md), and the 2622-line header. It is the
only one of the eight that carries a write barrier or `systemstack` directive,
204 of them. It is named here so that the seven are not described as eight.

## The risks that produce a silently wrong program

Every entry here compiles, links, and prints a plausible answer.

**Zero-size arguments.** Measured above. A `[0]int64` between two parameters
moves every parameter after it by up to seven bytes, in both ABIs, and nanogo's
current walk skips it. The failure is a callee reading its second argument from
the wrong word. Nothing reports it, and a nanogo-only program is
self-consistent.

**The callee's ABI at a call site.** If the outgoing area of the ABIInternal
wrapper is laid out by the ABIInternal walk, the assembly reads offsets nothing
wrote. Registers hold the values instead and the assembly never looks there.

**Wide arguments and results.** [030](030-abi.md)'s own hard cases do not go
away in ABI0, they change shape. In ABI0 every value is in the area, so there
is no register file to run out of and no all-or-nothing decision. What is left
is the alignment recurrence, and one missed `roundUp` moves everything after
it. This is the reason the six measured signatures are in this spec rather than
a description of them.

**The receiver.** `abi-internal.md` assigns the receiver before the arguments,
so an ABI0 method places the receiver at offset 0. nanogo's
`abiCallArgTypes` recovers a receiver by trying two operand lists and taking
whichever the span walk consumes exactly, which is a heuristic
[030](030-abi.md) already names as a bound. Refusing a method wrapper by name
keeps the heuristic out of this path entirely.

**`NOSPLIT` and the frame size.** An ABI0 wrapper always allocates a frame,
because it copies register values into a stack area. `gc` marks it `NOSPLIT`
anyway and lets the linker check the budget. A wrapper marked `NOSPLIT` whose
frame nanogo computed differently from `gc`'s would push a nosplit chain over
the limit, and that is a link error rather than a silent fault. A wrapper *not*
marked `NOSPLIT` that is reached from a nosplit context throws at run time.
Neither is silent, which is why this is a risk and not a blocker.

**Stack maps across an ABI0 frame.** Two maps have to be right at once. The
wrapper's own maps, which [027](027-liveness-and-stackmaps.md) builds from the
placement, and the callee's `args_stackmap`, which the compiler writes from the
Go signature and the assembly references without checking. A wrong bit in the
second makes the collector follow a non-pointer word or miss a live pointer,
and the symptom is a crash in a later collection with no connection to the
function that caused it. The mitigation is a byte comparison against `gc`'s
symbol, which is cheap and exhaustive over a fixed corpus.

**The linkname attribute on the wrapper.** `makeABIWrapper` copies
`IsLinkname` and `IsLinknameStd` from the wrapped symbol onto the wrapper's
symbol. `cmd/link`'s loader checks linkname pushes and pulls against the
*other* ABI's symbol when it cannot find one on the ABI it was given. A wrapper
that loses the attribute turns a working linkname into a link error, which is
loud, and one that gains it wrongly permits a pull that should be refused,
which is not.

**Two symbols with one name.** The wrapper and the wrapped function share a
name and differ only by ABI. `ssagen.symbols.index` is keyed by the
`callee` pair of name and ABI, so non-package references are safe.
`obj.Package.AddDef` appends and does not deduplicate by name, so two
definitions coexist. What is **not** safe is `driver.declaredSyms` and the
`generated` set that `driver.targetABI` reads, both of which are
`map[string]bool`. A wrapper entered into either would make a descriptor
reference to the wrapped function resolve under the wrong ABI. Those two maps
have to become ABI-aware before an ABI0 definition exists.

## What is not settled

**Whether `EmitArgInfo`'s encoding must match `gc` byte for byte.** The symbol
is read by `runtime.printArgs` when a traceback prints argument values. A wrong
encoding produces a wrong traceback and not a wrong program. It is listed as
part of stage 1 because the assembler references it, but its exact bytes were
not compared. Reading `ssagen.EmitArgInfo` against a dump of a real
`.arginfo0` symbol settles it.

**Whether a `def` line may name a symbol that is not in the compiled package's
own prefix.** `internal/bytealg`'s file has `def runtime.cmpstring ABIInternal`
and `def runtime.memequal ABIInternal`, which are not `internal/bytealg`
symbols. `GenABIWrappers` looks each function up by
`sym.Linkname` first and `sym.Pkg.Prefix + "." + sym.Name` second, so the
match happens through the linkname. Whether nanogo's symbol naming reaches the
same answer for those two was not checked. Compiling `internal/bytealg` and
comparing the defined symbol set against `gc`'s with `go tool nm` settles it.

**Whether `runtime`'s 129 assembly definitions include shapes the ABI0 walk
cannot place.** Only the seven non-runtime packages were surveyed in detail.
The count of 472 wrappers is measured, but the signatures behind them were not
read. It does not block stages 0 to 3, and stage 4 must start by reading them.

## What this spec corrects

**[030](030-abi.md) states the `cmd/asm` rule wrongly.** It says the
`<ABIInternal>` marker is refused outside the runtime. The rule is
`objabi.LookupPkgSpecial(pkgpath).AllowAsmABI` over a nine-package list that
includes `reflect`, `syscall`, `internal/bytealg`, `internal/chacha8rand` and
`internal/runtime/maps`. Three of the eight packages this spec is about rely on
it, and one of them needs no wrapper at all because of it.

**[030](030-abi.md) names three missing pieces and there are four.** The
wrapper, the symabis reader and the assembly header are named. The
`args_stackmap` and `arginfo0` symbols are not, and the assembler emits an
undefined reference to the first of them for every ABI0 text symbol it
produces.

**The `-+` refusal is dead.** [050](050-driver.md) lists `-+` under "sent
conditionally, when compiling the runtime". `cmd/go` contains no occurrence of
the flag. `gc` derives the property from `-std` and the package path, so
nanogo's refusal fires for none of the eight packages, and the assembly
refusal is the only thing standing between nanogo and compiling `runtime`
without the rules of [034](034-write-barriers.md) and
[035](035-goroutines-and-stack-growth.md).

**[045](045-linker.md) shows "generate ABI wrappers" as a linker stage.** It is
not one on this target. `buildcfg` sets `regabiAlwaysOn` for `arm64`, so
`RegabiWrappers` cannot be turned off, `abiInternalVer` stays 1, and
`cmd/link` keeps the two ABIs apart rather than collapsing them.
[030](030-abi.md)'s own record already corrected the same claim in its own
text.
