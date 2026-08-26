---
title: "Liveness, stack maps, and the contract with the collector"
status: in progress
layer: middle end
gate: G1
depends_on:
  - 026-register-allocation.md
  - 034-write-barriers.md
---

# Liveness and stack maps

The garbage collector must be able to find every pointer in every frame of every
goroutine, at any point where it can stop. The compiler is what tells it where
they are. This is the most consequential metadata nanogo emits and the least
visible: a program with wrong stack maps produces correct output for as long as
it takes for a collection to happen at the wrong moment.

## What is built

The liveness analysis, the frame layout, the two bitmaps and the two pc-value
streams are built and are written into object files. `ssa/liveness.go`,
`ssa/frame.go` and `ssa/stackmap.go` hold them, and `ssagen/stackmap.go`
attaches them to a function.

**The stack objects table is computed and not written.**
`ssa.StackMaps.ObjectsSym` builds one, and 162 functions of the distribution
corpus have a stack object, but `ssagen` does not call it. A record names the
`runtime.gcbits` symbol of its type's descriptor, and what is missing is the
lookup from an `ir.Type` to that symbol's `goobj.SymRef`.
[032](032-type-descriptors-and-itabs.md) writes the descriptor and its pointer
mask, so the gap is the lookup and not the writer. The table is precision rather
than coverage, so its absence is safe: an address-taken local is in the locals
bitmap for the whole of its marked lifetime, which is the conservative
answer.

## The contract

At a **safepoint**, for the frame of a function $f$, the compiler provides:

- an **arguments** bitmap: which words of the incoming argument area hold
  pointers;
- a **locals** bitmap: which words of the local frame area hold pointers;
- a **stack objects** table: the address-taken locals whose lifetime the
  collector must respect. This one is computed and not written, as above.

The collector reads them through `FUNCDATA` symbol references and selects the
bitmap for the current program counter through `PCDATA` index changes.

Because Go's ABI has no callee-saved registers ([026](026-register-allocation.md)),
no register map is needed. Everything live across a call is in the frame.

## Evidence that the mechanism is understood

[`spikes/stackmap`](../spikes/stackmap) is a working demonstration, kept in the
repository, of the exact encoding this spec requires:

- a `stackmap` data symbol holding `n int32`, `nbit int32`, and $n$ bitmaps of
  $\lceil nbit/8 \rceil$ bytes each;
- referenced by `FUNCDATA $FUNCDATA_LocalsPointerMaps`;
- selected per call site by `PCDATA $PCDATA_StackMapIndex, $i`.

The spike's `multi` case declares the same stack slot live at index 0 and dead at
index 1, and the collector honours both: the object survives one collection and is
freed at the next.

The spike was written in assembly text, which [000](000-decisions.md) decision 3
rejects as a build path for unrelated reasons. The encoding it proves is the
encoding nanogo writes into object files.

## Liveness

A backward dataflow over the CFG. For a block $b$ with instructions in order,

$$
\mathrm{live_{out}}(b) = \bigcup_{s \in \mathrm{succ}(b)} \mathrm{live_{in}}(s)
\qquad
\mathrm{live_{in}}(b) = \mathrm{gen}(b) \cup (\mathrm{live_{out}}(b) \setminus \mathrm{kill}(b))
$$

iterated to a fixed point over blocks in reverse postorder. The lattice is finite
and the transfer functions are monotone, so it converges.

The set is over *pointer-typed stack slots*, not over all values. A non-pointer
slot cannot hold a reference and its liveness does not concern the collector.

**Liveness must be a may-analysis.** A slot that is live on any path is live. The
error in the safe direction keeps a dead object alive, which wastes memory. The
error in the other direction frees a live object, which corrupts it.

## Safepoints

Every call is a safepoint, because the callee can allocate and trigger a
collection. So is every instruction where the goroutine can be preempted
asynchronously.

Asynchronous preemption is the complication. The runtime can stop a goroutine at
almost any instruction by delivering a signal, and the frame at that instant may
be mid-way through building a value. The runtime handles this by scanning such
frames **conservatively**, and the compiler's obligation is only to mark the
regions where that is not safe:

`PCDATA $PCDATA_UnsafePoint` marks instruction ranges where the frame is not in a
consistent state: the prologue before the frame is established, the epilogue
after it is torn down, and any sequence where a pointer exists only in a
half-written form. nanogo marks these ranges rather than trying to produce a
precise map for every instruction.

## Stack objects

A local whose address is taken **and whose type holds pointers** lives in the
frame as a stack object. The collector needs to know the object's extent and its
type, not just that a word is a pointer.

The pointer condition is not a refinement of the record's size. Without it a
zero-sized address-taken local gets offset 0 from `Varp`, the runtime reads a
non-negative offset as one into the **incoming argument area**, and the record
points at an argument. An address-taken local whose type holds no pointers is
covered by the locals bitmap anyway, which is where it belongs.

`FUNCDATA_StackObjects` carries a table of offset, size, and type descriptor for
each. `VarDef` and `VarKill` from [025](025-lowering-and-rules.md) mark the
lifetime bounds, so that a slot reused by two objects with disjoint lifetimes is
described correctly at each point.

**No pass emits a marker yet.** `ssa/liveness.go` reads them and hand-built
functions carry them, and neither construction nor lowering produces one:
`ssa/build.go` says so at `assignStmt` and gives the reason, that the decision
belongs to the pass that owns the markers. The analysis is seeded for exactly
this, so every stack object is live from the entry, which is the conservative
answer and costs precision rather than correctness.

This is also the constraint that limits spill slot reuse in
[026](026-register-allocation.md).

## Frame layout

The layout is fixed here, after allocation, because spill slots are part of it.

The diagram below is the shape and not any target's frame: it saves the return
address above the locals, and neither target of this deck does that. The
layout pass takes the placement of the saved words as configuration,
`ssa.FrameConfig`, rather than assuming either one.

On `arm64` the link register is saved at the **bottom** of the frame, inside the
outgoing argument area. The word at the **top** holds the *caller's* saved frame
pointer, and it must be reserved. The runtime's traceback computes

$$
\mathit{varp} = \mathit{fp} - \mathit{PtrSize}, \qquad
\mathit{argp} = \mathit{fp} + \mathit{MinFrameSize}
$$

for a non-empty frame, so `Varp` sits one word below the top of the frame.

Both words have to be reserved, and getting that wrong is the most dangerous
mistake in this spec's area. A frame that reserves neither gives
`Varp == Size`, which puts a local on top of the caller's saved frame pointer
and places `Varp` one word above where the runtime looks. **The stack map is
then well formed, the collector reads it, and it scans the wrong slots.**
Nothing reports it, and no unit test of the map's contents can see it.

```
higher addresses
  +-------------------------+
  | caller's frame          |
  +-------------------------+
  | return address          |   <- saved by the call
  +-------------------------+
  | saved frame pointer     |
  +-------------------------+
  | local variables         |   <- described by the locals bitmap
  | spill slots             |
  | stack objects           |
  +-------------------------+
  | outgoing arguments      |   <- described by the callee's arguments bitmap
  +-------------------------+   <- SP
lower addresses
```

Pointer-containing slots are grouped and placed together, **directly below
`Varp`**. The runtime scans exactly the region

$$
[\, \mathit{Varp} - \mathit{nbit} \times \mathit{PtrSize},\ \mathit{Varp} \,)
$$

so a pointer slot outside that run is a pointer the collector never sees. The
grouping is therefore a correctness constraint on the frame layout and not a
size optimisation, and it places an obligation on the code emitter: `Varp` must
end up where the layout pass put it.

The bitmap being short is the benefit that follows, not the reason.

## Two analyses, not one

Spill slots and frame objects need different analyses, and running one over both
is unsound.

A **spill slot** is described by the backward may-analysis above. Its uses are
all visible: the value is defined into the slot and read from it, and nothing
else can reach it.

A **frame object** cannot be. Its address escaped, which is why it is in the
frame, so the compiler cannot enumerate its uses. It is described instead by a
**forward** may-analysis over the `VarDef` and `VarKill` markers, seeded at
entry with every object that has no `VarDef` anywhere. Running the backward
analysis over an object's uses would report it dead while a pointer to it is
live in another function.

## What is exact and what is approximate

The two are easy to run together and must not be.

**Pointerness is exact.** Whether a word holds a pointer comes from
[020](020-ir.md)'s `PtrBits` and never from liveness. A bit that `PtrBits` does
not justify is a word [035](035-goroutines-and-stack-growth.md) will rewrite
during stack copying.

**Liveness is a may-analysis.** Whether a slot is live at a safepoint errs
toward live, which wastes memory and is safe.

Setting a bit because a slot is live, without checking that the slot's type has
a pointer there, is the mistake this section exists to prevent. It produces a
map that is conservative in the wrong dimension.

## The failure mode, and testing

A wrong stack map does not fail a unit test. It fails when a collection happens
while the wrong bit is set, which depends on allocation timing.

Testing is therefore adversarial rather than exhaustive:

- `GODEBUG=gccheckmark=1`, which makes the collector verify its own marking
  against a second, stop-the-world mark. This turns "a pointer was missed" into
  an immediate crash at the point of the mistake.
- `GOGC=1` allocation stress, so that a collection happens at as many points as
  possible.
- `GODEBUG=clobberfree=1`, which fills freed objects with a recognisable pattern,
  so use-after-free from a missed pointer is loud.
- A corpus in the shape of [`spikes/stackmap`](../spikes/stackmap): a pointer
  live across a call, only reachable from the frame, with a finaliser asserting
  it survived.
- Deep recursion with pointers live across the growth, which exercises the maps
  during stack copying, a second consumer of the same data, and one that
  rewrites pointers rather than only reading them.

### What those tests are today

The first four are one test in `ssagen/gc_test.go`. It runs under
`GODEBUG=gccheckmark=1,clobberfree=1` and `GOGC=1`, and it is the spike's shape:
one function compiled twice, once with the slot live at the collection and once
with an `OpVarKill` before it, so the difference between "survives" and "is
freed" is the bitmap and nothing else. The pointer is in a frame object rather
than in an argument, because the arguments bitmap would keep the object alive
whatever the locals bitmap said.

The fifth is `ssagen`'s stack-growth test, and it is weaker than this spec asks
for. It recurses 200,000 frames under `gccheckmark`, so it does exercise the
copier, but the value it carries through the growth is an integer. The
assertion is that the integer arrives unchanged, which catches a bit set on a
word that holds no pointer. A pointer carried through the growth is not tested
yet.

### The corpus

`ssa/stackmap_test.go` runs the whole pipeline over 536 packages of the
distribution. **17,905 functions reach SSA construction, 17,809 lower
completely, 17,758 reach a stack map, 10,727 carry a pointer bit, 162 carry a
stack object, and there are 120,493 safepoints.**

Two of those numbers need reading with care.

The 51 functions that are built and lowered and never mapped are not stack map
failures. The register allocator refuses them, and the map is built from the
allocation, so the pipeline stops one pass earlier.
[026](026-register-allocation.md) owns those refusals.

The 162 stack objects are objects the analysis found, not records in an object
file. Nothing writes `FUNCDATA_StackObjects` yet, for the reason at the top of
this spec.

## What was wrong

Three claims of this spec were wrong, and each was found by running something
rather than by reading it.

**The spec said an address-taken local is a stack object.** The condition is
narrower: the type must hold pointers as well. The corpus is what showed it. A
zero-sized address-taken local gets offset 0 from `Varp` and the runtime reads
a non-negative offset as one into the incoming argument area, so the record it
produced described an argument.

**The spec said an `arm64` frame reserves neither word.** It reserves the word
at the top of the frame for the caller's saved frame pointer, and the link
register goes at the bottom, inside the outgoing argument area. This was the
most dangerous error the deck has carried, because the map it produces is well
formed and describes the wrong slots. It was found by running a collector
against a compiled function, which is the only thing that could have found it.

**The spec called the grouping of the pointer slots below `Varp` a size
optimisation.** It is a correctness constraint: the runtime scans that run and
nothing else, so a pointer slot outside it is a pointer the collector never
sees.
