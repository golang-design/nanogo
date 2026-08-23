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

## Attachment

[010](010-scanner-and-positions.md) routes the comment to the parser with its
position. The parser attaches it to the declaration that follows, and
[012](012-type-checking.md) copies it onto the object when the object is created.

Two rules:

1. A directive must be on its own line, immediately before the declaration, with
   no blank line between. A directive not immediately before a declaration is
   **an error**, not a comment. Silently ignoring a misplaced `//go:nosplit` is
   the failure mode this rule exists to prevent.
2. An unrecognised `//go:` directive is an error in nanogo's own source and a
   warning elsewhere, because new directives appear in new Go releases and
   [000](000-decisions.md) decision 11 pins nanogo to one.

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
| `//go:noinline` | Suppress inlining. Honoured, because it is trivial and tests depend on it. |
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

nanogo's own object writer uses `unsafe`, so this is a G1 requirement and not a
G3 one.

## Testing

- A corpus asserting that each correctness-required directive changes generated
  code in the specified way, checked by inspecting the emitted object rather
  than by running.
- Rejection tests: a misplaced directive, an unknown directive in nanogo's own
  source, a `//go:nosplit` chain that exceeds the budget, and a
  `//go:nowritebarrier` function that needs one.
- `//go:linkname` in both directions, across packages, linked and run.
- The runtime is the real test and it arrives at M9.
