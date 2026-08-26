---
title: "Escape analysis"
status: draft
layer: middle end
gate: "optional at G1, required at G3"
depends_on:
  - 020-ir.md
  - 022-optimization-passes.md
---

# Escape analysis

**Nothing in this spec is built.** There is no pass, no graph, no summary and no
`-m` output. `ir/` holds a builder, a type layout, a node set, a converter, the
lowering pass of [020](020-ir.md) and the descriptor naming of
[032](032-type-descriptors-and-itabs.md), and no analysis over any of them. What
exists is one reserved field, `ir.Object.Escapes`, which is assigned nowhere.

The safe answer is being taken by default, everywhere, which is what makes the
absence tolerable: the compiler is correct and allocates more than it needs to.
Four sites act on its absence, and they do not all do the same thing:

| Site | What it does without this pass |
| --- | --- |
| `ir/lower.go`, allocation | Every allocation goes to the heap. The heap is correct and slower; the frame is sometimes correct and otherwise corrupts memory |
| `ir/lower.go`, composite literals | Decides frame or heap by shape, not by this pass: a struct or array in an expression position is copied out by its reader and is built in the frame, and anything handing out a pointer to its elements goes to the heap |
| `ssa/build.go` | Cannot put a string concatenation buffer in the frame, because it does not know which concatenation escapes, so the buffer is nil and the runtime allocates |
| `ir/build.go` | Keeps a composite literal as one node, because a literal already scattered into element assignments is a decision this pass can no longer make |

The second row is the shape of answer this pass replaces: a rule sound for every
program, taken where a per-allocation judgement is not available. Two rows of
[020](020-ir.md)'s lowering table, the closure and the composite literal, name
this spec as the thing that decides them.

The design below stands. It was reviewed and not disproved, only deferred.

Escape analysis decides, for each allocation, whether it can live in the
caller's stack frame or must go on the heap. It runs on the IR of
[020](020-ir.md), before SSA, because it needs to see assignment to fields and
passing to calls as Go operations.

The safe answer is always "heap". [022](022-optimization-passes.md) explains why
that stops being acceptable at G3.

## The question

An allocation may be stack-allocated when no reference to it outlives the frame
that created it. References outlive the frame by being: returned, assigned to a
global, assigned into a heap object, sent on a channel, captured by an escaping
closure, or passed to a function that does one of those things.

## The model

A directed graph. Vertices are *locations*: named variables, allocation sites,
function parameters, results, and one distinguished vertex `heap`.

An edge $u \xrightarrow{k} v$ means "the value in $v$ flows to $u$ through $k$
dereferences", with $k$ counted as:

| Assignment | Edge |
| --- | --- |
| `u = v` | $u \xrightarrow{0} v$ |
| `u = &v` | $u \xrightarrow{-1} v$ |
| `u = *v` | $u \xrightarrow{1} v$ |
| `u = v.f` | $u \xrightarrow{0} v$, fields are not distinguished |
| `u[i] = v` | $u \xrightarrow{0} v$ |

Field insensitivity is a deliberate loss of precision that removes a whole
dimension from the analysis.

An allocation escapes when there is a path from `heap` to it whose edge weights
sum to $-1$ or less:

$$
\mathrm{escapes}(a) \iff \exists\ \text{path } heap \rightsquigarrow a
\ \text{ with } \sum_i k_i \le -1
$$

The sum being negative means the path passes an address-of that is never undone
by a dereference, so the heap holds the address of $a$.

The computation is a shortest-path traversal from `heap` over the graph, taking
the minimum weight sum to each vertex. Edge weights are bounded, so it
terminates.

## Across function boundaries

The whole-program version of this is not affordable. The standard answer, and
nanogo's, is a **summary per function**: for each parameter, how far it leaks,
to the heap, to a specific result, or not at all.

Summaries are computed bottom-up over the call graph's strongly connected
components. Within a component, iterate to a fixed point. Across packages, the
summary travels in export data ([015](015-export-data.md)).

A call to a function with no summary, indirect or through an interface nanogo
did not devirtualise, leaks every pointer argument to the heap. This is the
conservative case and it is common.

## The directives

Three of [016](016-directives-and-pragmas.md)'s pragmas exist only for this pass
and all three are used by the runtime and the standard library:

| Directive | Effect |
| --- | --- |
| `//go:noescape` | On a bodyless declaration, asserts no parameter leaks. Believed without proof. |
| `//go:uintptrescapes` | A `uintptr` parameter is treated as a pointer that leaks, keeping the referent alive. |
| `//go:uintptrkeepalive` | The referent is kept alive across the call without being forced to the heap. |

`//go:noescape` is an assertion the compiler cannot check and a lie in it is
memory corruption. It is honoured because `syscall` and `runtime` require it.

`driver/pragma.go` parses all three today and records them on the function.
Nothing reads them, because the pass that would is this one.

## What escaping decides

| Decision | Consumer |
| --- | --- |
| Heap or stack for an allocation | [031](031-runtime-lowering.md): `runtime.newobject` against a frame slot |
| Heap or stack for a closure | [033](033-closures-defer-panic.md) |
| Whether a variable is a stack object needing a GC description | [027](027-liveness-and-stackmaps.md) |
| Whether `make` with a constant small size can be a frame array | [031](031-runtime-lowering.md) |

## Precision, and where it is given up

| Given up | Effect |
| --- | --- |
| Field sensitivity | One escaping field escapes the whole object |
| Flow sensitivity | A variable that escapes on one path escapes on all |
| Indexing sensitivity | One escaping element escapes the whole slice or array |
| Indirect and interface calls without devirtualisation | All pointer arguments escape |

`gc` gives up the first three too. The fourth is why
[024](024-inlining-and-devirtualization.md) matters more to this pass than to any
other.

## Testing

None of this is written. Recorded because the oracle is the reason this pass is
cheap to get right, and it should be set up before the first line of the pass:

- `gc` has `-m` output stating each decision, and Go's `test/escape*.go` corpus
  annotates the expected decisions. nanogo is to emit the same shape of output
  and run that corpus. This is a rare case of a compiler-internal decision
  having a ready-made external oracle, and it should be used. nanogo parses `-m`
  today and prints nothing ([050](050-driver.md)).
- Differential execution with the pass off, per
  [022](022-optimization-passes.md): identical output required.
- A corpus for the three directives, checked in generated code.

## What was wrong

The spec said nothing false. It said nothing about being unbuilt either, and a
reader of the deck had no way to tell this apart from a pass that ships. The
state was found by listing `ir/` and grepping for the one field the design
names: `ir.Object.Escapes` has four mentions in the repository, its declaration,
its doc comment, one assertion in `ir/node_test.go` that a fresh object has it
clear, and `ir/lower.go`'s allocation section explaining why every allocation
goes to the heap while the field is unset. None of the four is an assignment.

The spec said two consumers are waiting on this pass. Four sites act on its
absence, and the table above says what each does instead. One of them, the
composite literal lowering, does not wait: it takes a decision by shape that is
sound without an analysis, and says so.

The spec listed `ir/` as holding a builder, a type layout, a node set and a
converter. It also holds the lowering pass of [020](020-ir.md) and the
descriptor naming of [032](032-type-descriptors-and-itabs.md).
