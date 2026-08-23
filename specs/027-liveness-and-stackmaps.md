---
title: "Liveness, stack maps, and the contract with the collector"
status: draft
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

## The contract

At a **safepoint**, for the frame of a function $f$, the compiler provides:

- an **arguments** bitmap: which words of the incoming argument area hold
  pointers;
- a **locals** bitmap: which words of the local frame area hold pointers;
- a **stack objects** table: the address-taken locals whose lifetime the
  collector must respect.

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
consistent state — the prologue before the frame is established, the epilogue
after it is torn down, and any sequence where a pointer exists only in a
half-written form. nanogo marks these ranges rather than trying to produce a
precise map for every instruction.

## Stack objects

A local whose address is taken lives in the frame, and a pointer to it can be
held anywhere. The collector needs to know the object's extent and its type, not
just that a word is a pointer.

`FUNCDATA_StackObjects` carries a table of offset, size, and type descriptor for
each. `VarDef` and `VarKill` from [025](025-lowering-and-rules.md) mark the
lifetime bounds, so that a slot reused by two objects with disjoint lifetimes is
described correctly at each point.

This is also the constraint that limits spill slot reuse in
[026](026-register-allocation.md).

## Frame layout

The layout is fixed here, after allocation, because spill slots are part of it:

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

Pointer-containing slots are grouped and placed together, so the bitmap is dense
and short. This is a size optimisation with a correctness benefit: a shorter
bitmap is one that can be checked by reading it.

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
  during stack copying — a second consumer of the same data, and one that
  rewrites pointers rather than only reading them.
