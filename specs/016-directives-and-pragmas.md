---
title: "Directives and pragmas: the comments that change code generation"
status: draft
layer: front end
gate: "G1 for the subset nanogo uses, G3 for all of it"
depends_on:
  - 010-scanner-and-positions.md
  - 012-type-checking.md
---

# Directives and pragmas

A `//go:` comment is not a comment. It is a compiler instruction with no syntax
of its own, and the runtime depends on several of them for correctness rather
than for speed. Ignoring one does not produce slower code; it produces a runtime
that crashes.

This spec is the complete table, with what nanogo must do and when.

## What is built

**No directive changes generated code today.** The table below is a plan.

What is built is the plumbing and the placement rule, everything except a
consumer:

| Stage | State |
| --- | --- |
| The scanner routes a `//go:` or `// +build` comment to a handler, with its position | built ([010](010-scanner-and-positions.md)) |
| The parser accumulates directives and binds them to the declaration that follows | built |
| The parser hands back a directive that no declaration claimed, so the handler can reject it | built |
| `ir.Func` carries the directives of the declaration it was built from | built |
| The driver's handler records the verb and its position | built: `driver/pragma.go` |
| A misplaced directive is an error | built: `newPragmaHandler` and `checkDirectives` |
| Any pass reads a directive | **not built** |

`ir.Func.Pragma` now carries a `*driver.pragma` for a function that was marked,
and nothing below reads it. The chain is complete except at its far end, and
`driver.TestNosplitIsStillDropped` gates that end: two packages that differ
only by a `//go:nosplit` must produce the same object until somebody makes one
of them not.

## Attachment

[010](010-scanner-and-positions.md) routes the comment to the parser with its
position. The parser attaches it to the declaration that follows.

The spec then had [012](012-type-checking.md) copy the directive onto the object
when the object is created. That is not what the code does. The directive
travels on the syntax declaration into `ir.Func.Pragma`, and the checker never
sees one. This was found by grepping `types2` for `Pragma` during this audit,
which returns one hit, and that hit is upstream's own code. The IR is the better
place for the same reason [012](012-type-checking.md) gives for layout: the
consumers of a directive are all below the checker.

Two rules:

1. A directive must be on its own line, immediately before the declaration, with
   no blank line between. A directive not immediately before a declaration is
   **an error**, not a comment. Silently ignoring a misplaced `//go:nosplit` is
   the failure mode this rule exists to prevent.
2. An unrecognised `//go:` directive is an error in nanogo's own source and a
   warning elsewhere, because new directives appear in new Go releases and
   [000](000-decisions.md) decision 11 pins nanogo to one.

Rule 1 is enforced, in `driver`, not in `syntax`. The parser decides nothing:
it attaches a pending directive to whatever declaration follows and calls the
handler a second time with the ones no declaration claimed, which is
`clearPragma`. The driver decides in two places, because a directive goes wrong
in two ways:

- **Unclaimed.** `newPragmaHandler` gets the empty text from `clearPragma` and
  reports every recorded directive. This is the `//go:noinline` before a
  statement, before a declaration group, or at the end of a file.
- **Claimed by a declaration with no use for it.** `checkDirectives` walks the
  file after it parses and compares what each declaration collected against
  what its kind accepts: `//go:build` on the file, `funcPragmas` on a function
  declaration, nothing anywhere else. This is the `//go:noinline` before a
  `var`, which the parser does attach, to a declaration no pass will read it
  from.

A directive that shares its line with code is a third case and is decided in
the handler at once, from the scanner's `blank` flag. Nothing in the text of
`//go:noinline` says whether it stood alone.

Rule 2 is not enforced. An unrecognised verb maps to no flag, so it is neither
recorded nor reported, which makes it a comment wherever it stands on a line of
its own. That is deliberate for the release-skew reason above, and the missing
half is the error in nanogo's own source and the warning elsewhere.

The corpus proves both halves of rule 1: `test/directive.go` and
`test/directive2.go` were accepted in full before this, which
[004](004-conformance.md)'s harness classes as `missed`, and they now report
the same message at the same twenty positions as `go tool compile`.

## The table

### Required for correctness

Getting one of these wrong produces a program that is wrong, not slow.

| Directive | Meaning | Consumer |
| --- | --- | --- |
| `//go:nosplit` | No stack-growth check in the prologue. The function must fit the nosplit budget. | [035](035-goroutines-and-stack-growth.md) |
| `//go:nowritebarrier` | Compile error if the body needs a write barrier. | [034](034-write-barriers.md) |
| `//go:nowritebarrierrec` | The same, transitively through calls. | [034](034-write-barriers.md) |
| `//go:yeswritebarrierrec` | Stops the recursive check above. | [034](034-write-barriers.md) |
| `//go:systemstack` | Must run on the system stack; calling it from a user stack is an error the compiler inserts a check for. | [035](035-goroutines-and-stack-growth.md) |
| `//go:linkname a b` | Binds local symbol `a` to external symbol `b`, in either direction. | [032](032-type-descriptors-and-itabs.md) |
| `//go:uintptrescapes` | A `uintptr` argument keeps the referent alive across the call. | [023](023-escape-analysis.md) |
| `//go:uintptrkeepalive` | The same, without forcing escape. | [023](023-escape-analysis.md) |
| `//go:cgo_unsafe_args` | Argument area may be addressed as one block. | out of scope; [000](000-decisions.md) decision 8 |

`//go:nosplit` deserves its own note. The budget is a fixed number of bytes of
stack available below the guard, shared by the whole nosplit call chain. The
compiler must compute the chain's depth and reject an overflow, because the
failure mode at run time is a stack overflow in code that cannot grow the stack,
inside the scheduler. [035](035-goroutines-and-stack-growth.md) owns the
computation.

It is also the one row where the code contradicts this spec rather than lagging
it. `ssagen/prologue.go` sets a function's `nosplit` flag from the frame size
and from whether the function is a leaf, and never from a directive. Omitting
the check for a function that cannot overflow is sound on its own terms. The
consequence is that a `//go:nosplit` on any other function is dropped. It is no
longer dropped in silence at the point it is written, because rule 1 now
reports a directive that stands in the wrong place, but a directive in the
right place still reaches no consumer. The chain-depth computation does not
exist at all. This was found by reading `prologue.go` for its use of the word
during this audit.

`//go:linkname` deserves another. It breaks the package boundary and the
standard library uses it heavily, in both directions: pulling a runtime symbol
into a package, and pushing a package symbol out for the runtime to call. It is
resolved at symbol-naming time, not at type-check time, so a `linkname` target
need not exist in the type checker's world at all.

### Required for the language definition

| Directive | Meaning |
| --- | --- |
| `//go:noescape` | On a bodyless declaration: no argument escapes. The compiler must believe it. |
| `//go:wasmimport`, `//go:wasmexport` | Out of scope; no wasm target in this deck. |
| `//go:build` | Build constraint, handled in [014](014-package-loader.md). |
| `//line` | Position rewriting, handled in [010](010-scanner-and-positions.md). |

### Optimisation hints, safe to ignore

nanogo may ignore every directive in this group and still be correct. It must
still *parse* them and must not error on them.

| Directive | Effect if honoured |
| --- | --- |
| `//go:noinline` | Suppress inlining. Not honoured: inlining is not built ([024](024-inlining-and-devirtualization.md)), so there is nothing to suppress. |
| `//go:norace`, `//go:nocheckptr` | Suppress instrumentation nanogo does not implement. Parsed, no effect. |
| `//go:notinheap` | Type is not heap-allocated. Parsed; affects write barrier elision only as an optimisation. |
| `//go:registerparams`, `//go:noabiwrap` | Historical or ABI-transition directives. Parsed, no effect. |

## `unsafe`

`unsafe` is not a directive but belongs here, because it is the other place the
front end must permit what the type system otherwise forbids.

`unsafe.Pointer` conversions, `unsafe.Sizeof`, `Alignof`, `Offsetof`, `Add`,
`Slice`, `SliceData`, `String`, `StringData` all arrive with the fork in
[012](012-type-checking.md). What does not arrive is the backend's obligation:
a value of type `unsafe.Pointer` is a pointer for the garbage collector, and a
`uintptr` is not. Confusing them in a stack map or a type descriptor's pointer
map is a collector bug that appears as corruption under load.
[027](027-liveness-and-stackmaps.md) and
[032](032-type-descriptors-and-itabs.md) carry that obligation.

The spec said nanogo's own object writer uses `unsafe`, which is what made this
a G1 requirement. It does not. No package in the repository imports `unsafe`,
found by grepping for the import during this audit.

The G1 requirement holds for a stronger reason. nanogo compiles the standard
library and the runtime, which use `unsafe` throughout, and five intrinsics are
already IR nodes: `ir/build.go` builds `unsafe.Add`, `Slice`, `SliceData`,
`String` and `StringData`. `Sizeof`, `Alignof` and `Offsetof` never reach the
builder at all, because the checker folds them to constants.

## Testing

None of this is built, because no directive is honoured yet.

- A corpus asserting that each correctness-required directive changes generated
  code in the specified way, checked by inspecting the emitted object rather
  than by running.
- Rejection tests: a misplaced directive, an unknown directive in nanogo's own
  source, a `//go:nosplit` chain that exceeds the budget, and a
  `//go:nowritebarrier` function that needs one.
- `//go:linkname` in both directions, across packages, linked and run.
- The runtime is the real test and it arrives at M9.
