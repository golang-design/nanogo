---
title: "Directives and pragmas: the comments that change code generation"
status: in progress
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
| The driver's handler records fourteen verbs and their positions | built: `driver/pragma.go`'s `pragmaVerb` |
| A misplaced directive is an error | built: `newPragmaHandler` and `checkDirectives` |
| Any pass reads a directive | **not built** |

The fourteen are `//go:build`, `noescape`, `norace`, `nosplit`, `noinline`,
`nocheckptr`, `systemstack`, `nowritebarrier`, `nowritebarrierrec`,
`yeswritebarrierrec`, `cgo_unsafe_args`, `uintptrkeepalive`, `uintptrescapes`
and `registerparams`. Three verbs the tables below name are absent:
**`//go:linkname`**, `//go:notinheap` and `//go:noabiwrap`. A verb the driver
does not recognise maps to no flag, so it is neither recorded nor reportable as
misplaced. For the last two that costs nothing, because honouring them costs
nothing. For `//go:linkname` it is the gap this spec's opening paragraph
describes: a `//go:linkname` in a package nanogo compiles is a comment, and the
symbol it was meant to bind keeps its declared name.

`ir.Func.Pragma` carries a `*driver.pragma` for a function that was marked, and
nothing below reads it. The chain is complete except at its far end, and
`driver.TestNosplitIsStillDropped` gates that end: two packages that differ
only by a `//go:nosplit` must produce the same object until somebody makes one
of them not.

## Attachment

[010](010-scanner-and-positions.md) routes the comment to the parser with its
position. The parser attaches it to the declaration that follows.

The directive then travels on the syntax declaration into `ir.Func.Pragma`.
The checker never sees one: `types2` holds a single occurrence of the word
`Pragma` and it is upstream's own code. The IR is the right place for the same
reason [012](012-type-checking.md) gives for layout, that the consumers of a
directive are all below the checker.

Two rules:

1. A directive must be on its own line, immediately before the declaration, with
   no blank line between. A directive not immediately before a declaration is
   **an error**, not a comment. Silently ignoring a misplaced `//go:nosplit` is
   the failure mode this rule exists to prevent.
2. An unrecognised `//go:` directive is an error in nanogo's own source and a
   warning elsewhere, because new directives appear in new Go releases and
   [000](000-decisions.md) decision 10 pins nanogo to one.

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

The corpus carries rule 1 in `test/directive.go` and `test/directive2.go`,
which annotate 23 misplaced directives between them and which
[004](004-conformance.md)'s harness now classes as `rejected` rather than
`missed`. What `rejected` proves is narrower than the file looks: the harness
compares the first error's line only, because `gc` collapses several errors on
one line and nanogo stops after ten, so comparing the whole set would compare
two reporting policies. The positions themselves are pinned one at a time by
`driver.TestMisplacedDirectiveIsRejected`, whose expected lines are `gc`'s.

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
| `//go:linkname a b` | Binds local symbol `a` to external symbol `b`, in either direction. Not recognised by `pragmaVerb`, so not recorded either. | [032](032-type-descriptors-and-itabs.md) |
| `//go:uintptrescapes` | A `uintptr` argument keeps the referent alive across the call. **Refused.** | [023](023-escape-analysis.md) |
| `//go:uintptrkeepalive` | The same, without forcing escape. **Refused.** | [023](023-escape-analysis.md) |
| `//go:cgo_unsafe_args` | Argument area may be addressed as one block. | out of scope; [000](000-decisions.md) decision 8 |

The two `uintptr` directives are refused rather than recorded and dropped, and
they are the only ones in this table that are. The rest of the group is written
by the runtime, which nanogo does not compile, so nothing reachable is
miscompiled by dropping them. These two are written by ordinary code that hands
a pointer to a system as an integer, and dropping them collects the object while
the callee is reading it.
[`uintptrescapes3.go`](../internal/gotest/testdata/go/test/uintptrescapes3.go)
is the corpus file that showed it: it printed four failures rather than nothing.
`driver.LifetimeDirective` is the refusal and
[023](023-escape-analysis.md) owns the pass that would lift it.

**The refusal reads the declaration, so it covers the callee's own package and
nothing else.** A package nanogo compiles that *calls* a `//go:uintptrescapes`
function it imported, `syscall.Syscall` for one, is not refused, and the
obligation is the caller's. Closing that needs the directive to travel in the
export data ([015](015-export-data.md)) or the check to read the callee's
declaration through the type checker, and neither is built.

`//go:nosplit` deserves its own note. The budget is a fixed number of bytes of
stack available below the guard, shared by the whole nosplit call chain. The
compiler must compute the chain's depth and reject an overflow, because the
failure mode at run time is a stack overflow in code that cannot grow the stack,
inside the scheduler. [035](035-goroutines-and-stack-growth.md) owns the
computation.

`ssagen/prologue.go` decides `nosplit` without reading the directive:
`f.nosplit = f.size == 0 || (f.leaf && f.size < stackSmall)`. Omitting the
check for a function that cannot overflow is sound on its own terms, and the
consequence is that a `//go:nosplit` on any other function is dropped. It is
not dropped in silence when it stands in the wrong place, because rule 1
reports that, but a directive in the right place reaches no consumer. The
chain-depth computation does not exist at all.

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
| `//go:embed` | Binds a package-level `string`, `[]byte` or `embed.FS` to the files a pattern matches. It is not a pragma: `gc` reads it out of the comment and pairs it with the `-embedcfg` file the `go` command writes, so `pragmaVerb` does not recognise it and nothing here records it. [050](050-driver.md) owns the gap. Both build paths refuse a package that carries the directive, and neither reads it out of a comment: `nanogo build` keys off `go list`'s `EmbedPatterns` and the `-toolexec` path keys off the `-embedcfg` flag the `go` command sends. |
| `//line` | Position rewriting, handled in [010](010-scanner-and-positions.md). |

### Optimisation hints, safe to ignore

nanogo may ignore every directive in this group and still be correct. It must
not error on one, which it satisfies in two ways: a recognised verb is recorded
and no pass reads it, and an unrecognised verb stays a comment.

| Directive | Recognised | Effect if honoured |
| --- | --- | --- |
| `//go:noinline` | yes | Suppress inlining. Not honoured: inlining is not built ([024](024-inlining-and-devirtualization.md)), so there is nothing to suppress. |
| `//go:norace`, `//go:nocheckptr` | yes | Suppress instrumentation nanogo does not implement. Recorded, no effect. |
| `//go:registerparams` | yes | An ABI-transition directive. Recorded, no effect. |
| `//go:notinheap` | no | Type is not heap-allocated. Would affect write barrier elision only as an optimisation. |
| `//go:noabiwrap` | no | Historical. |

A verb that is not recognised cannot be reported as misplaced either. In this
group that costs nothing, which is why the two `no` rows are not the same
omission as `//go:linkname`.

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

No compiler source file in the repository imports `unsafe`. The only importers
are three test files, `ir/type_test.go`, `ir/convert_test.go` and
`rtype/rtype_test.go`, which construct the layouts they assert against.

The G1 requirement rests on the distribution and not on nanogo's own source.
nanogo compiles the standard library and the runtime, which use `unsafe`
throughout, and five intrinsics are already IR nodes: `ir/build.go` builds
`unsafe.Add`, `Slice`, `SliceData`, `String` and `StringData`. `Sizeof`,
`Alignof` and `Offsetof` never reach the builder at all, because the checker
folds them to constants.

## Testing

What is built is the placement rule and the proof that nothing is honoured:

- `driver.TestMisplacedDirectiveIsRejected` pins each misplaced case at the
  line `gc` reports it on, and `driver.TestPragmaVerbMapsTheTable` pins the
  recorded flag set, including the four verbs that imply another: `nosplit` and
  `cgo_unsafe_args` imply `nocheckptr`, `nowritebarrierrec` implies
  `nowritebarrier`, and `uintptrescapes` implies `uintptrkeepalive`.
- `test/directive.go` and `test/directive2.go` are `rejected` in
  [004](004-conformance.md)'s corpus, on the first error's line.
- `driver.TestNosplitIsStillDropped` asserts that two packages differing only
  by a `//go:nosplit` produce the same object, so the day a pass reads one this
  test fails and has to be rewritten.

What is not built, and waits on a consumer:

- A corpus asserting that each correctness-required directive changes generated
  code in the specified way, checked by inspecting the emitted object rather
  than by running.
- Rejection tests: an unknown directive in nanogo's own source, a
  `//go:nosplit` chain that exceeds the budget, and a `//go:nowritebarrier`
  function that needs one.
- `//go:linkname` in both directions, across packages, linked and run. It needs
  the verb recognised first.
- The runtime is the real test and it arrives at M9.

## What was wrong

**The table called itself complete and had no row for `//go:embed`.** It is the
one directive outside this table that changes what a package's data section
holds, and leaving it out let [050](050-driver.md) describe `-embedcfg` as
refused without anyone noticing that the refusal reads a compile command line
and `nanogo build` writes its own. The row above names it and points at the
spec that owns it. `//go:generate` and `//go:fix` are absent for a different
reason and stay absent: `gc` lists both in `noder`'s `allowedStdPragmas`, the
set it accepts without giving them a `syntax.Pragma` value, so neither reaches
code generation. nanogo reaches the same outcome by the rule above, that an
unrecognised verb stays a comment.

**The spec had [012](012-type-checking.md) copy the directive onto the object
when the object is created.** The directive never reaches the checker. It
travels on the syntax declaration into `ir.Func.Pragma`, which is the right
place, because every consumer of a directive is below the checker.

**The spec claimed both files of the directive corpus report the same message
at the same twenty positions as `go tool compile`.** The two files annotate 23
positions, and [004](004-conformance.md)'s harness compares one of them: the
first error's line. The per-position claim belongs to
`driver.TestMisplacedDirectiveIsRejected` and not to the corpus.

**The `//go:nosplit` row read as a plan the code lagged.** It is the reverse.
`ssagen/prologue.go` sets the flag from the frame size and the leaf property
and never from a directive, so a `//go:nosplit` in the right place on a
function that needs one is dropped. The chain-depth computation
[035](035-goroutines-and-stack-growth.md) owns does not exist.

**The spec said nanogo's own object writer uses `unsafe`, which is what made
`unsafe` a G1 requirement.** It does not, and no compiler source file in the
repository does. The G1 requirement holds anyway, because nanogo compiles the
standard library and the runtime, which use `unsafe` throughout.

**The optimisation-hint table said every directive in it is parsed.**
`//go:notinheap` and `//go:noabiwrap` are not in `pragmaVerb` and are not
recognised at all. Neither is `//go:linkname`, which is in the
correctness-required table and is the one where not recognising it costs
something.

**`status` was `draft`.** Six rows of the table above are built and three tests
gate them, which is `in progress` by
[the deck's definition](README.md#reading-the-deck-honestly).
