---
title: "G1: self-hosting"
status: in progress
layer: gate
gate: G1
depends_on:
  - 001-bootstrap-gates.md
  - 023-escape-analysis.md
  - 053-determinism.md
---

# Self-hosting

The G1 gate of [001](001-bootstrap-gates.md), as a procedure that can be run.

## Where this procedure stands

**Stage 1 passes.** nanogo compiles all 20 of its own packages and `main`, the
objects link, and $N_2$ runs and reports its version. $N_2$ is a working
compiler on ordinary input: it builds a Go program that prints 42, and the
program runs.

**Stage 2 passes, and $N_2$ equals $N_3$ byte for byte.** $N_2$ compiles the
same 17 packages the go command asks $N_1$ for, the objects link, and the two
executables compare equal. That is the G1 fixed point of
[001](001-bootstrap-gates.md).

It is a procedure and not yet a job. `internal/selfhost` gates the per-package
measurement under the fixed point and nothing runs the three stages in CI, so
"The build" below is what a reader runs to check the paragraph above. Each
stage takes minutes, which is the reason it is not a job and is the decision to
make rather than assume.

### The fault that stopped stage 2, and why every collector probe was negative

One defect held the whole of stage 2, and it is a frame layout fault and not a
collector fault. It presented as three unrelated failures, all of them in code
$N_1$ compiled: a bad pointer in a heap object while marking, an export data
reader that read a desync out of a file `gc` wrote, and a slice header with a
length no allocation could produce.

**The outgoing argument area belongs to the callee for the whole of a call.**
Go's register ABI gives every register-assigned parameter a home in the
**caller's** argument area, at `abi.ABIParamResultInfo.SpillAreaOffset`, and
the callee's register allocator spills the parameter there whenever it is live
across a call the callee makes. `cmd/compile/internal/abi/abiutils.go` lays the
area out as the stack arguments, then the stack results, then that spill area.

`runtime.typedmemmove` is such a callee. Its three parameters are
register-assigned, and when `runtime.writeBarrier.enabled` is set it spills all
three into the caller's area at offsets 0, 8 and 16 before calling
`runtime.bulkBarrierPreWrite`:

```
MOVBU  writeBarrier.enabled, R4
TBZ    $0, R4, skip
MOVD   8(R0), R4            // typ.PtrBytes
CBZ    R4, skip
MOVD   R0, 56(RSP)          // = caller's SP + 8
MOVD   R1, 64(RSP)
MOVD   R2, 72(RSP)
CALL   runtime.bulkBarrierPreWrite
```

A result the registers cannot hold sits in that same area. `ssa/abi.go`'s
`readFromArea` turned the call site's store of such a result into a block move
whose **source was the area**, and `ssa/rules` lowers a move into the heap to
`runtime.typedmemmove`. So the barrier call replaced the first three words of
its own source with `{type, dst, src}` and then copied them to the
destination. In `export.(*Reader).Read`:

```
CALL  pkgbits.NewPkgDecoder      // 128-byte result at 8(RSP)
ADRP/ADD -> R0 = type:pkgbits.PkgDecoder
MOVD  248(RSP), R1               // dst = the heap PkgDecoder
ADD   $8, RSP, R2                // src = the result area
CALL  runtime.typedmemmove
```

The dump of the destination is the three arguments, word for word, with the
rest of the value intact:

```
*(pr+0)  = 0x1052bd978           // type:pkgbits.PkgDecoder
*(pr+8)  = 0x763130a15200        // dst
*(pr+16) = 0x76313081c468        // src
*(pr+24) = 0x76313096868c        // elemData, correct
*(pr+32) = 0x40ef
```

`version` and `sync` share word 0, so the decoder then believed the file
carried sync markers, and the first `Decoder.Sync` on data `gc` wrote without
them reported a desync. The same three words landed in a scanned heap object
elsewhere and the collector threw on them.

**Why the collector probes all read negative.** Every row of the table below
was a true measurement of a fault that was not there. The write barrier is the
only thing that decides whether `runtime.typedmemmove` takes the spilling path,
so every switch that turns the barrier off cleared the fault and every switch
that left it on did not.

| Probe | Reading | What it was really measuring |
| --- | --- | --- |
| `gccheckmark=1` | silent over 10 of 10 failing runs | nothing was lost, so there was nothing to report |
| a barrier forced on every store | 10 of 10 | no store was missing one |
| bulk barriers | 1994 moves, none wrong | the barrier was correct, its argument area was not |
| the 11,200 emitted barrier sites | all correct | same |
| `GOGC=off` | 0 of 10 | the barrier is never enabled, so `typedmemmove` never spills |
| `gcstoptheworld=1` | 0 of 10 | user goroutines are descheduled for the whole mark, so no `typedmemmove` runs while the barrier is on |
| `asyncpreemptoff=1` | 10 of 10 | preemption is not involved |
| `gcshrinkstackoff=1` | 10 of 10 | stack copying is not involved |
| `GOMAXPROCS=1` | 10 of 10, and 7 of 10 over a second $N_2$ | it is not a race |

The two that read 0 of 10 are one switch spelled two ways: each takes the write
barrier out of the mutator's path, so `runtime.typedmemmove` never reaches the
branch that spills. Every other row leaves it in, so every other row read the
fault whatever it was aimed at. That is what a probe over a symptom costs when
the mechanism is one layer below it.

**The fix is `stageArea` in `ssa/abi.go`, and it is gc's.** A move that
`ssa/bulkbarrier.go` will give a barrier may not read the outgoing argument
area. The result is copied into a frame slot first, and the barrier move reads
that slot. The staging copy may read the area, because its destination is a
frame slot: it gets no barrier and lowers to `runtime.memmove`, which is
`NOSPLIT|NOFRAME` hand-written assembly and has no spill home to write. gc
solves the same problem the same way, in `ssa/writebarrier.go`, where
`isVolatile` names an address in the argument area and the pass copies it to a
local before it can be an operand of `runtime.wbMove`.

`ssa/abi_test.go` holds the layout as an assertion, over the pass and with no
collector in it, because the miscompile is in every build and only its trigger
is timing dependent. `internal/e2e/argarea_test.go` holds it as a program: a
call whose result the registers cannot hold, stored through a pointer, with
`debug.SetGCPercent(1)` keeping a collection in flight. Without the fix the
program reports 888 corrupted words of 64,000 and with it none.

**The loop that found it.** `go build -work -x` on stage 2 keeps the work tree
and prints the failing `compile` invocation. Re-running that one argv
reproduces in about a second, `GOGC=1` makes it deterministic, and
`GOEXPERIMENT=nogreenteagc` on the stage 1 build is what makes the collector
name the object it threw on: the classic `scanobject` passes `refBase` and
`refOff` to `findObject`, so `badPointer` prints the holder and its contents,
and Green Tea's `scanObjectsSmall` passes zeros and prints neither.

**Use a fresh `GOCACHE` for every $N_2$ build.** nanogo's `-V=full` string
changes only with the commit, not with its source, so the go command's tool ID
is stale and the cache silently mixes objects from different nanogo binaries.

### What stage 2 could show that nothing else could

Every fault stage 2 found is a miscompilation in code $N_2$ contains, and none
was reachable by any test in this repository at the time: every one of them
runs a compiler that `gc` built, and these need a compiler that nanogo built,
over input large enough to reach the fault. Each is a test now.

### The other fault stage 2 found

**A frame object the collector read before anything wrote it.** $N_2$ died with

```
runtime: bad pointer in frame golang.design/x/nanogo/rtype.fieldName at 0x...: 0x97
fatal error: invalid pointer found on stack
```

and it took `rtsym` with it. `fieldName` returns a struct too large for the
argument registers, so the result is a frame object, its only write is the copy
after the last call, and the locals bitmap claimed its pointer words at the two
safepoints before that write. The stack copier read one of the claimed words and
found what the previous frame had left there.
[027](027-liveness-and-stackmaps.md) owns the rule and `ssagen`'s
`zeroFrameObjects` is the fix, which is `gc`'s `Needzero` for the same class of
variable. `internal/e2e/framezero_test.go` is the shape as a program: the
construct is ordinary and it took a compiler nanogo built to reach it.

### What used to stop stage 1 from starting

The language, and then three classes of relocation. All 20 of
nanogo's own library packages compile, each measured on its own, and the
section below records that and the harness that keeps it true.

The row after that was that nanogo had never read export data nanogo wrote.
Stage 2 closes it: $N_2$ compiles `driver` against the `export` archive $N_2$
itself produced, so the second half of the round trip runs on every stage 2
build. [005](005-remaining-work.md) carries that row and the graph it sits in.

### What used to stop it

Four refusals did, and all four are closed. They are kept because each one
moved the measurement in a way the refusal message did not predict.

| Refused | Why | What closed it |
| --- | --- | --- |
| a package with assembly | an assembly definition is ABI0 and needs a wrapper | still open, and it blocks no package of nanogo's own: [047](047-abi-wrappers.md) owns it and it is a G3 row |
| a declared type an importer would need a descriptor for | `rtype` could not fill the bytes in | [032](032-type-descriptors-and-itabs.md) |
| a package-level variable whose type holds a pointer and whose descriptor `rtype` cannot build | the collector reads a data symbol's pointer map through its type descriptor | [032](032-type-descriptors-and-itabs.md) |
| a function whose register allocation asks `ssagen` for a move or a scratch register it does not have | no case for an edge move between two spill slots, and two integer scratch registers where an indexed store wants three | `ssagen`, which [000](000-decisions.md) decision 5 records has no spec of its own |

The second row is the one nanogo's own packages hit first, and it is the reason
this document stopped reasoning about the language subset. `driver/types.go`
walks a package's scope in sorted order and refuses on the first declared type
whose descriptor `rtype` cannot write, so the message named one type per package
and said nothing about how many were behind it: `syntax` named `ArrayType`, `ir`
named `Class`, `obj` named `Aux`, `rtsym` named `Group`, and `driver` named
`Allowlist`. The count of nanogo packages that nanogo compiles is the measure,
and no arithmetic over the language subset stands in for it.

Measured over the 28 compile invocations a `func main() {}` needs, nanogo
compiles **24** and refuses 4. `internal/selfhost` takes the reading:

```
NANOGO_MEASURE_CLOSURE=1 go test ./internal/selfhost/
```

| Packages | Refused for | Owner |
| --- | --- | --- |
| 2 | a call to a symbol `//go:linkname` renames, which needs the reference half of the name model | [047](047-abi-wrappers.md) |
| 1 | a method of a generic type whose dictionary is short by the slots its body names | [013](013-generics.md) |
| 1 | `runtime` itself | [023](023-escape-analysis.md), [034](034-write-barriers.md), [035](035-goroutines-and-stack-growth.md), [047](047-abi-wrappers.md) |

The two held by `//go:linkname` are `internal/bytealg` and
`internal/runtime/maps`. The one held by [013](013-generics.md) is
`internal/runtime/atomic`. `runtime` needs three unbuilt specs at once and is
the only package here that does.

`//go:nosplit` holds nothing in this closure any more and neither does the ABI0
wrapper. Those two reasons held six packages at the previous reading and hold
none at this one, which is what took the count from 20 to 24. The previous
reading is below.

### What this reading looked like at 20

The eight refusals split four ways: four on `//go:nosplit` in a runtime
package, two on an assembly call to a Go function needing the ABI0 wrapper, one
on a `//go:linkname` renaming a definition nanogo emits, and `runtime`. Before
that, all eight gave one message, "a package with assembly in it", which was
one refusal standing in front of four separate pieces of missing work. Stages 0
and 1 of [047](047-abi-wrappers.md) split that message into four true ones
without moving the count.

Nothing in the closure is now refused for a reason in the language. The last
two that were, `internal/runtime/gc/scan` and `internal/stringslite`, wanted
`unsafe.Slice` and `unsafe.String`, which are rows of [020](020-ir.md)'s
lowering table and are built.

### What this table looked like before it was re-measured

Every other row is gone, and the way they went is the reason this document now
says how to take the reading rather than only what it said.

| Was refused for | What happened |
| --- | --- |
| 7, a declared type's descriptor | [032](032-type-descriptors-and-itabs.md) closed it. The row was one count and four reasons, so it did not shorten by one per fix |
| 2, the register allocator's output | closed; the two packages moved on to other reasons and then compiled |
| 1, a package-level variable of type `error` in `math/bits` | closed |
| 1, `append` in `internal/byteorder` | closed, a row of [020](020-ir.md)'s lowering table |

The count went from 9 to 18 and only one of the original five reasons is left.
A table of reasons written once and re-read later describes a compiler that no
longer exists, which is why the recipe is in code now.

None of these counts is in `internal/hygiene/testdata/facts.json` and none is
ratcheted, on purpose. The closure is the installed Go distribution's, so it
moves with every Go release: a package appears, a package grows an assembly
file, a generic body changes shape. A gate on it would fail the build on a
toolchain upgrade, which is not a regression in nanogo. This is a reading of
`go1.27.0` on `darwin/arm64`, which is `driver.PinnedGoVersion` and
`driver.TargetArch`, the only pair nanogo emits code for.

### What this count cannot see, and the edge it hides

The measurement builds each package on its own against **`gc`-built**
dependencies, for the reason `internal/selfhost/measure.go` gives: a whole-tree
build stops at the first failure and reports the deepest package's blocker as
though it were the only one. That isolation costs one thing. No `gc` compile in
this harness ever reads an archive nanogo wrote, so nothing this count reports
can depend on what nanogo puts in one.

The mixed build of [051](051-build-integration.md) does read them, and it stops
on something this count is structurally unable to show. `gc` compiles the 24
packages of `cmd/internal/objabi`'s `runtimePkgs` under a rule that forbids a
heap escape, and **17 of the 23 standard library packages nanogo compiles here
are on that list.** `export/writer.go` writes an empty escape analysis note for
every receiver and parameter, `gc` reads an empty note as "leaks to the heap at
zero dereferences", and a runtime package that hands the address of a local to
a nanogo-compiled function is then refused:

```
runtime_fast32.go:479:57: key escapes to heap, not allowed in runtime
```

The note nanogo writes is correct. It is the most pessimistic answer in `gc`'s
encoding and the only sound one available without
[023](023-escape-analysis.md), which is unbuilt. So **the mixed build now
depends on [023](023-escape-analysis.md)**, and no amount of work on the four
refusals above will show it.

The dependency does not go away when nanogo owns `runtime` as well, and it is
not `gc`'s rule that keeps it. `cmd/internal/objabi.PkgSpecial` says what the
list is for, and the first line of it is "implicit allocation is disallowed".
A compiler that puts every allocation on the heap ([023](023-escape-analysis.md)
records that nanogo does) cannot compile a package under that constraint,
whichever compiler is enforcing it. That is the fourth spec on `runtime`'s row
in the table above, and it was not there before this was measured.

`cmd/nanogo/escapenote_test.go` holds the mechanism as a test, and
[023](023-escape-analysis.md) holds the encoding, the two sound notes that need
no analysis, and why writing any other one is a miscompile rather than a
shortcut.

One export-data limit is in the way of this gate specifically.
`export/writer.go` refuses a generic declaration on purpose, and nanogo's own
source has them. They are few and concentrated in the `types2` fork,
`types2/subst.go` and `types2/trie.go` among them, and the instantiations of
`slices` and `cmp` are many and spread across the tree. Declaration and
instantiation are both [013](013-generics.md)'s, so stage 1 needs that spec as
well as the language rows below.

The second-order measure is the language, and it says the same thing more
slowly. The IR builder produces a typed tree for 536 packages of the
distribution. SSA construction takes 20,871 of those functions on its own, and
40,385 once the lowering pass has run first, which is what the driver does. Of
the 20,871, all but 59 lower to arm64 machine operations and the same number
carry a stack map, so the back half of the pipeline refuses almost nothing the
front half hands it. Every one of those numbers is in
`internal/hygiene/testdata/facts.json`, gated, and written out in full in the
README and in [032](032-type-descriptors-and-itabs.md); this section names them
rather than restating them, so that there is one place to correct.
[020](020-ir.md)'s **State** column names the rows this gate still waits on and
its corpus counts them.

The end-to-end gate grows only as the language does, because its programs are
written in the language the compiler accepts. `internal/e2e`'s first program is
a counted loop.

The sections below are the procedure for the day the language is wide enough.

## The build

```
stage0:  gc     compiles nanogo's source  ->  N1
stage1:  N1     compiles nanogo's source  ->  N2
stage2:  N2     compiles nanogo's source  ->  N3
gate:    N2 == N3, byte for byte
```

Stage 0 runs under `go build`. Stages 1 and 2 run under
`go build -toolexec=<compiler>` in hosted mode
([000](000-decisions.md) decision 10), with the allowlist of
[051](051-build-integration.md) covering every package nanogo's source needs.

At G2 the same three stages run in a container with no `go` command
([061](061-toolchain-independence.md)), so they run under `nanogo build`, which
takes no allowlist and refuses a package it cannot compile rather than
delegating it to `gc`.

The gate compares the linked executables. `go tool link` produces both, from the
same objects in the same order, so any difference is in the objects.

### Running it

The allowlist is nanogo's own packages and `main`. Each stage gets a build
cache of its own, for the reason above: nanogo's `-V=full` string moves with
the commit and not with the source, so one cache shared between two stages
replays objects the other stage's compiler wrote.

```sh
D=$(mktemp -d)
L=$D/allowlist
go list ./... | grep -vE '/(cmd|internal)/' > $L && echo main >> $L

go build -o $D/n1 ./cmd/nanogo                                    # stage 0

NANOGO_ALLOWLIST=$L GOCACHE=$D/c1 \
  go build -toolexec=$D/n1 -o $D/n2 ./cmd/nanogo                  # stage 1

NANOGO_ALLOWLIST=$L GOCACHE=$D/c2 \
  go build -toolexec=$D/n2 -o $D/n3 ./cmd/nanogo                  # stage 2

cmp $D/n2 $D/n3                                                   # the gate
```

Add `-work -x` to a stage that fails. The go command then keeps the work tree
and prints the `compile` invocation that failed, and re-running that one argv
is a one-second loop rather than a ten-minute one.

## What nanogo's own source requires

The subset of Go that must work is decided by what the compiler is written in,
and it is worth listing because it sizes M5 in [003](003-sequencing.md):

| Used heavily | Used | Not used |
| --- | --- | --- |
| structs, methods, interfaces | goroutines and channels, in the concurrent compile | reflection beyond `unsafe` |
| slices, maps, strings | `defer`, `panic`, `recover` | `select`, except incidentally |
| closures | `unsafe`, in the object writer | complex numbers |
| generics ([013](013-generics.md)), mostly by instantiation | `sort`, `strconv`, `fmt`, `os`, `io` from the standard library | `cgo` |
| type switches and assertions | `//go:` directives, only as input | assembly of its own |

Read the first column against what the compiler accepts today and the distance
is the gate. Every entry in it compiles now: structs and methods with either
receiver, interfaces with their calls, conversions, assertions and type
switches, slices with `append`, strings with `range`, maps, channels with
`select`, closures with captures, `defer`, `go`, `panic` and `recover`, and a
generic function the compiling package declares, stencilled per list of type
arguments, and a value of any concrete type goes into an interface, whatever
its shape. What the first column still waits on is narrower than the column
itself, and the table of blockers below names it: an instantiation of a generic
another package declares, a generated hash function, and a closure over a named
result in a function that defers.

The standard library entries are the decisive ones. nanogo's dependency set
reaches a large part of `fmt`, `go/types`' fork, and the `internal` packages
underneath them, so "the subset nanogo uses" is not small: it is most of the
language exercised by ordinary application code, and none of the language
exercised only by the runtime.

That is precisely the difference between G1 and G3.

## Diagnosing a failure

$N_2 \ne N_3$ has three causes ([001](001-bootstrap-gates.md)). Take them in
order, because the cheap check eliminates the expensive investigation:

1. **Non-determinism.** Compile the source twice with $N_1$ and compare. If those
   differ, [053](053-determinism.md) is the whole problem and nothing else is
   wrong. This is the common case and it must be ruled out first.
2. **A miscompile in $N_1$'s output.** Bisect by object: compile the packages
   with $N_1$ and with $N_2$ and compare the objects one at a time. The first
   differing object names the package, and its differing symbol names the
   function.
3. **An environmental dependency.** Compile in two different directories with
   different environments. [053](053-determinism.md)'s two-process check is this,
   run deliberately.

The bisection in step 2 is the reason [040](040-object-format.md) requires
deterministic object bytes rather than only deterministic executables. Comparing
executables tells you that something is wrong; comparing objects tells you what.

### Bisecting by allowlist does not work, and cannot

A mixed build, where nanogo owns some packages and `gc` owns the rest, carries
faults of its own that mask whatever is being looked for. That is not bad luck
and it is not a bug to be fixed on the way past. It has one cause: nanogo writes
a type descriptor and an itab into the named non-package definition space, which
the linker dedupes by name, and `gc` writes them into its content-addressable
hashed space. The two never merge, so a type that both compilers reach exists
twice.

It fires two ways, with different triggers.

- **A duplicate itab**, when a body is exported. `gc` inlines a nanogo function
  that builds an interface, emits its own itab in the hashed space with weak
  `Fun` relocations, and the nanogo definition survives only in the named space
  where nothing reaches it. `cmd/link`'s `data.go` redirects a weak unreachable
  address to `runtime.unreachableMethod`, so the call goes to an unslid address
  and faults rather than throwing.
- **A duplicate descriptor**, from a plain import. A `gc`-compiled package that
  type-switches on a type nanogo declared emits its own descriptor for it, and
  the switch misses nanogo's.

The second needs only an import, so any mixed build can hit it. The measured
symptoms are `unknown syntax.Decl node *syntax.ImportDecl` and
`the type checker panicked: unreachable`, and they are the harness talking about
itself rather than about the program.

So step 2's bisection is by object and by package, never by allowlist. This
stays true until descriptors and itabs move into the content-addressable space
[032](032-type-descriptors-and-itabs.md) owns.

## What the gate does not prove

Stated in [001](001-bootstrap-gates.md) and repeated because it is the most
likely thing to be forgotten in a celebration: the fixed point proves
self-consistency. A compiler that miscompiles a construct it does not use, or
that miscompiles one reproducibly, passes.

[004](004-conformance.md)'s L1 through L3 are what make the gate mean something,
and they run before it, not after.

## After the gate

nanogo builds nanogo from then on, and stage 0 becomes a periodic check rather
than the build. The `gc`-built binary is still produced in CI, because a
divergence between $N_1$ and $N_2$ that appears later is a miscompile that the
fixed point alone cannot see.

## What was wrong

**Three package-level refusals this spec listed are gone.** `export/` reads
`gc`'s export data and writes it, so a package that imports is no longer
refused. `driver/` writes the initialisation record, so a package with an `init`
is no longer refused. And `ssagen/data.go` writes the data symbol of a
package-level variable, so a package that declares one is no longer refused for
that reason alone.

**The package census was stale in two directions at once and the totals hid
it.** It read 5 of 28 compiling and 23 refused, split 10 for a descriptor, 8 for
assembly, 1 for data and 4 in a function body. Two re-measurements later it is 9
and 19, and no re-measurement moved the total in one direction only: the first
took the descriptor row from 10 to 11 while the function-body row fell from 4 to
2, and the second, after `rtype` learned the `UncommonType` tail and the struct
field array, took the descriptor row from 11 to 7. A sum alone would have read
the first of those as one package's worth of progress. State every row, not the
total.

**The refusal taxonomy had four categories and needs five.** Two of the
refusals are not a language gap. `internal/stringslite` is refused by `ssagen`
for an edge move between two spill slots, and `internal/runtime/gc` for an
indexed store wanting three integer scratch registers where the target reserves
two. Both are the register allocator's output and not a construct in the source,
so they share a row of their own in both tables above.

**The descriptor row's reason was `ir.Type` carrying no method set.** It
carries one now ([032](032-type-descriptors-and-itabs.md)), and the row survived
because the count did. Three of the seven packages it still holds want the
generated equality function that spec owes, and only two want anything to do
with a method. A reason stated once and never re-read outlives the fact it was
taken from.

## nanogo's own packages, measured

This is the narrower and more useful measure: nanogo's own library packages,
each compiled by nanogo on its own.

**20 of 20 compile**, and the list is derived rather than written down. It is
every package in the module that is not a `main` package and is not under
`internal/`, which is the compiler and not the harnesses that run it. A package
added to the compiler joins the measurement without anybody remembering to add
it, because a written list is how a package gets left out while the total goes
up anyway.

**Compiling every package is not G1.** Each is compiled on its own against
dependencies `gc` built, and a compile means nanogo produced an archive the go
command accepted. Nothing is linked and nothing is run. What G1 asks for is a
compiler that builds itself and then builds itself again to the same bytes, and
the export data reader below is what stands between the two.

### The measurement, and the three traps in it

`internal/selfhost` runs it and `internal/selfhost/testdata/ratchet.txt`
records it. Refresh with

```
NANOGO_REQUIRE_CORPUS=1 NANOGO_REFRESH_RATCHET=1 go test ./internal/selfhost/
```

The number was measured by hand before that package existed and it was wrong
three times, for three different reasons. Each is now defended against in code
rather than described somewhere and forgotten.

- **Exit status carries no information.** A `-toolexec` build hands `gc` every
  package nanogo cannot compile, so the build succeeds either way. The only
  evidence is the line nanogo writes to `NANOGO_LOG`. The measurement reads
  that line and discards the status.
- **An empty `NANOGO_ALLOWLIST` is not "everything".** It is nothing: every
  package goes to `gc` and the build still succeeds. The first attempt at this
  measurement reported twelve of twelve for exactly that reason. The allowlist
  is written per package and read back before the build.
- **A cached compile action means nanogo never runs.** A package `gc` built as
  a dependency earlier has the same action ID when it becomes the allowlisted
  target later, so `-toolexec` is skipped and the log holds no line for it.
  Each package gets its own `GOCACHE`, and a package with no line is reported
  as not reached rather than counted as either answer.

A fourth trap is the reason each package is measured alone. A whole-tree build
stops at the first failure and says nothing about the rest, so it reports the
deepest package's blocker as though it were the only one.

### What the ratchet is for

Nothing else in this repository can see a package leaving this set. The corpus
of [004](004-conformance.md) watches Go's own test files and says nothing about
nanogo's source, and each package's unit tests test the compiler rather than
what the compiler does to itself. A refusal added anywhere in the pipeline can
take a package out with every other gate green.

That is not hypothetical. The wrapper generator refused a variadic method, for
a reason that turned out to be a misreading of where a call's arguments are
packed, and it took `syntax` out of this set at
`syntax.(*parser).errorfAt`. Nothing failed. It was found days later by running
the measurement by hand, which is the same way the number had been wrong three
times before.

The ratchet records the compiled set and the package count and nothing else. A
refusal is never recorded: recording one freezes a gap in place and calls it
progress. Growth does not fail the build, and a run that compiles more says so
and prints the refresh command.

### How each blocker closed

Every blocker this measurement named is closed, and each time the package moved
to the next reason rather than to the compiling column. That is the shape of
the record and the reason it is kept: a work list that shortens by one item per
fix is not what this measurement produced. `rtsym` wanted a
frame object past the twelve-bit `ADD` immediate, which is the R27 expansion
[042](042-arm64-backend.md) already had for `subSP`. `obj` wanted a result that
arrives in the frame to be read there, which [030](030-abi.md) now does at the
call site as well as at the return, and then a data word that is the address of
a copy, which [032](032-type-descriptors-and-itabs.md) now writes; `obj`
compiles. `dist` and `export/pkgbits` wanted the same data word and are now on
their own next reason. `dist` then wanted the body of the hash its
`map[Producer]int` names, which [032](032-type-descriptors-and-itabs.md) now
generates, and moved to the generic instantiation `syntax` waits on: two of
the three rows are one row. `export/pkgbits` wanted a named result its `DumpTo`
captures, which [033](033-closures-defer-panic.md) now builds, and it compiles.
`syntax` was last and cleared four: the foreign walk now carries the statement
forms its instantiations reach, a range over a function is built, a method
value whose receiver is an interface is built, and the wrapper generator
forwards a variadic method's slice instead of refusing it.

Each package's stack of reasons was deeper than its refusal said, because a
refusal names what stopped first and not what the package needs. An earlier
version of this document read one message as a work list and reported the wrong
owner for three packages: the refusal named `sync/atomic.Load`, which reads
like a local declaration, but `objName` joins the package path to the object's
name, so it was always a foreign object.
