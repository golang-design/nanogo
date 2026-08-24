---
title: "Type checking: forking types2 and owning it"
status: complete
layer: front end
gate: G1
depends_on:
  - 000-decisions.md
  - 011-parser-and-ast.md
---

# Type checking

Implements [000](000-decisions.md) decision 1. nanogo does not write a type
checker. It takes `cmd/compile/internal/types2`, re-points it at nanogo's syntax
tree, and owns the result from that moment.

## Why the fork is safe, and why it is cheap

The safety argument is in [000](000-decisions.md): a dependency written in Go is
not an obstacle to self-hosting, and the reference compiler has a forked checker
of its own rather than a written one.

The cost argument is stronger than it looks, and it rests on a fact in the Go
tree that is worth stating precisely.

**`go/types` is generated from `types2` by a mechanical rewriter.**
`src/go/types/generate_test.go` parses each `types2` source file, applies AST
rewrites that swap one syntax tree for another, and writes the `go/types` file.
The two checkers are 23,222 and 24,953 lines and are kept in sync this way.

The Go team already solved the exact problem nanogo has (take this checker and
run it on a different syntax tree), and solved it with a source rewriter rather
than by hand. nanogo uses the same technique on the same source.

### Fork `types2`, not `go/types`

`types2` is the better base and the choice is not close:

| | `types2` | `go/types` |
| --- | --- | --- |
| Syntax tree | compiler-shaped, which is what [011](011-parser-and-ast.md) builds | `go/ast`, built for tooling |
| API stability burden | none, it is internal | strict backward compatibility, including deprecated surface |
| Compiler-facing hooks | present (`compilersupport.go`, `recording.go`) | absent |
| Position model | compact `Pos`, matching [010](010-scanner-and-positions.md) | `token.Pos` with a `FileSet` |

`types2` is internal to the Go repository, so it is copied rather than imported.
It is BSD-licensed; the copy keeps the copyright header and records its upstream
revision.

## The port

### Scale

38 of the 69 non-test files name the syntax tree. The names they use are few and
concentrated:

| Reference | Uses | Reference | Uses |
| --- | --- | --- | --- |
| `syntax.Expr` | 106 | `syntax.IndexExpr` | 14 |
| `syntax.Pos` | 79 | `syntax.Stmt` | 13 |
| `syntax.Name` | 44 | `syntax.SelectorExpr` | 12 |
| `syntax.CallExpr` | 17 | `syntax.Node` | 10 |
| `syntax.Operation` | 16 | `syntax.Field` | 10 |

The tail is long but shallow. This is a rename-and-adjust job over a bounded
vocabulary, which is why [011](011-parser-and-ast.md) is constrained to keep
that vocabulary intact.

### The gap, measured

Every name `types2` writes as `syntax.X` was extracted and checked against
nanogo's tree. 117 names, of which **92 already exist with the same shape**.
The 25 that do not are the whole of the port's surface, and they are helpers
rather than nodes:

| Missing name | Call sites in types2 | Disposition |
| --- | --- | --- |
| `Unparen` | 14 | provided in `syntax` |
| `UnpackListExpr` | 13 | provided in `syntax` |
| `StartPos` | 13 | provided in `syntax` |
| `EndPos` | 9 | provided in `syntax` |
| `NewName` | 7 | provided in `syntax` |
| `Inspect` | 7 | provided in `syntax` |
| `PosBase`, `NewFileBase` | 8 | **rewrite-table entry**; nanogo's position model is a `FileSet` and a `SrcFile`, per [010](010-scanner-and-positions.md) |
| `Parse`, `ParseFile` | 7 | provided in `syntax`, with nanogo's signature |
| `Fprint`, `String`, `ShortForm` | 3 | a tree printer, for error messages |
| `CommentMap`, `CommentsDo` | 2 | **dropped**; nanogo's tree carries no comments |
| the rest | 0 | mentions in comments, not code |

No node type is missing. That is the result worth stating: the node set of
[011](011-parser-and-ast.md) is complete for the checker, and the port's real
work is a handful of helpers plus one model difference.

Every number in this section was re-measured against the vendored sources after
the port landed and every one still holds: 69 non-test files, 38 of them naming
the syntax tree, 117 distinct `syntax.X` names. The helpers listed as provided
are provided, in `syntax/walk.go` and `syntax/printer.go`.

### The one model difference

`types2` resolves a position through a `*PosBase` that the `Pos` itself points
to. nanogo's `Pos` is a bare `uint32` resolved through a `FileSet`, because
there is a position in every IR node and SSA value and 4 bytes against 16 is
worth having ([010](010-scanner-and-positions.md)).

The cost of that choice inside the port was measured too, and it is small:
across all of `types2`, **34 call sites** call a method on a `Pos`, and they use
four methods, `Col`, `IsKnown`, `Line`, and `RelFilename`. Every one of them is
inside the checker, which already holds a context object, so the `FileSet` is
threaded through that object and the rewrite is mechanical.

Had that number been in the hundreds, the compact `Pos` would have been the
wrong decision and [010](010-scanner-and-positions.md) would need revisiting. It
is 34, so it is not.

### Mechanism

The generator is `types2/gen`. It reads the vendored upstream sources under
`types2/upstream`, which are stored with a `.txt` suffix so that they are not
built, and it writes nanogo's `types2` package. It runs as a test, so CI fails
when the vendored source and the generated output drift, and `-write`
regenerates.

**The mechanism has two rewrite kinds, not the three classes this spec listed.**

1. A **rule** is a literal replacement applied to every generated file. There
   are three and all three are import paths: the syntax tree, the checker
   itself, and the error codes.
2. A **patch** is a literal replacement applied to one named file, with the
   number of matches it must make. A patch that stops matching is an error and
   not a silent no-op, so an upstream change that touches a ported line is
   reported instead of dropped. 24 upstream files need one.

The class this spec called **node renames does not exist, and its absence is the
result worth recording.** No rewrite renames a node, because
[011](011-parser-and-ast.md) held the vocabulary and the node set matched. The
class it called **deletions does not exist either**: `dropped` is an empty map.
Both were found by reading `gen.go` for this audit.

What the generator cannot do is ported by hand and listed in `handPorted`,
exactly as upstream marks its own unportable files. There is one entry,
`compiler_internal.go`, because `RenameResult` writes a type back into the
syntax tree and nanogo's tree has no place to put one.

### What gets carried and what does not

Carried whole: constants and their arithmetic, conversions, assignability,
method sets, embedding and promotion, interface satisfaction, type inference,
instantiation, initialisation order, the `unsafe` package, and the error set.

**Nothing is dropped, and this spec said three things were.** It listed the
`Info` maps nanogo does not read, the `gccgo` size rules, and the deprecated API
surface. All three are carried. `Info` is generated whole and `driver/compile.go`
fills seven of its maps. `gccgosizes.go` is a generated file in `types2`. No
rule and no patch removes deprecated surface. This was found during this audit
by reading `gen.go`'s `dropped` map, which is empty, and by listing `types2`.

Carrying them is the cheaper choice and the spec's reasoning did not survive
contact with the generator. A deletion is not free. It is a patch that has to
keep matching across upstream revisions, and what it buys is lines in a package
that [000](000-decisions.md) decision 10 already excludes from the budget. Dead
code in an excluded package costs less than a patch that must be maintained.

## What nanogo adds

The fork is extended, and these are the extensions that make it nanogo's rather
than a copy:

1. **The `FileSet` plumbing**, in the hand-written `types2/position.go`. It is
   the whole cost of the compact `Pos` inside the checker. `Config.Fset` carries
   the `FileSet`, and every point that prints or resolves a position reaches it
   through a helper that tolerates a nil one, because the formatting paths and
   the position-free unit tests both run without a `FileSet`.
2. **Its own test harnesses**, where upstream's read comments out of the tree.
   `errorcheck_test.go` scans `/* ERROR */` annotations out of the source text,
   because upstream reads them with `syntax.CommentMap` and nanogo's tree
   carries no comments. `srcimporter_test.go` type-checks an imported package
   from source, because [015](015-export-data.md) is not built.
3. **A drift test over the whole port.** Every rewrite must state its reason,
   every upstream test file must be either ported or listed as skipped with a
   reason, and regeneration must be idempotent.

**This spec listed three different extensions and none of them was built.** It
claimed pragma attachment during declaration checking, layout facts computed on
the type, and extra position fidelity for the backend. What exists instead: the
only pragma the checker reads is upstream's own `Nointerface` test in `decl.go`,
and it never fires, because the driver's pragma handler discards every directive
([016](016-directives-and-pragmas.md)); layout is computed below the checker, by
`Layout` in `ir/type.go`, on the side of the boundary
[002](002-architecture.md) draws; and the backend reads the same `Pos` the
checker does, with nothing added. This was
found by grepping `types2` for `Pragma` and for `Sizeof` during this audit.

The correction is worth more than the three items. The spec assumed the checker
was the place to hang compiler-specific facts. The IR boundary turned out to be
the better place, and `ir/type.go` states why: below the IR a type is a size, an
alignment and a pointer map, and nothing below the IR needs to agree about
anything else.

## The escape hatch, and its price

If the port resists, if the rewrite table grows without converging, the
fallback is to keep `go/types` unmodified over `go/ast`, parse a second time with
`go/parser`, and translate the checked result into nanogo's IR.

The price is stated so the fallback is never taken quietly: two parses of every
file, positions that must be mapped between two schemes, `go/ast`'s tree in the
dependency set forever, and the loss of every extension listed above, each of
which would then need a side table keyed by node identity.

The decision point is M2 in [003](003-sequencing.md). The trigger is the rewrite
table failing to converge, not the port being tedious.

**The hatch was not taken and the port converged.** The rewrite table is three
rules plus patches over 24 files, the checker runs on nanogo's tree, and
`go/types` is not in the dependency set: `go.mod` has no requirements at all.
The section is kept because the price it names is what makes the decision
reviewable, not because the option is still open.

## Errors

nanogo's messages are its own ([052](052-diagnostics.md)); its judgements are
upstream's. [004](004-conformance.md) L1 compares judgements and positions, and
`errorcheck` in L2 compares against patterns that most nanogo messages will
match anyway, since the upstream error *codes* are carried.

## Testing

Built, and these numbers are measured. `types2` sits just on the 90% line and is
excluded from the coverage gate, because it is upstream's code under upstream's
tests and a nanogo-shaped threshold would measure the wrong thing.

- 613 subtests in `types2`, which is the ported upstream suite. It comes with
  the fork and it is the reason the fork is safe.
- An `errorcheck` corpus of 375 entries, of which 370 are checked and 5 are
  named as known gaps and skipped.
- Agreement with `go/types` on accept and reject over 14 standard library
  packages, parsed by nanogo's parser and checked by this checker, plus
  nanogo's own `syntax` package.
- The generator's own drift test: regenerate and compare, plus the checks that
  every rewrite states a reason and that every upstream test file is either
  ported or skipped with a reason.

The first bullet used to say "every package in the distribution". 14 named
packages is what runs, and the reduction is deliberate rather than a shortfall:
walking `GOROOT` is [004](004-conformance.md)'s gate and belongs there, not in a
unit test that every commit runs.
