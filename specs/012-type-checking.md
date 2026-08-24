---
title: "Type checking: forking types2 and owning it"
status: draft
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

The Go team already solved the exact problem nanogo has — take this checker and
run it on a different syntax tree — and solved it with a source rewriter rather
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

A generator in nanogo's repository, in the shape of `go/types/generate_test.go`:
it reads the vendored upstream sources and writes nanogo's `types2` package,
applying a table of rewrites. It runs as a test, so CI fails when the vendored
source and the generated output drift.

Three rewrite classes cover almost all of it:

1. **Import path.** `cmd/compile/internal/syntax` becomes
   `golang.design/x/nanogo/syntax`.
2. **Node renames**, where [011](011-parser-and-ast.md) chose a different name.
   The table is the spec of the divergence; a rename that is not in the table is
   a bug in one of the two specs.
3. **Deletions**, for the parts of upstream nanogo does not carry.

What the generator cannot do is ported by hand and marked, exactly as upstream
marks its own unportable files.

### What gets carried and what does not

Carried whole: constants and their arithmetic, conversions, assignability,
method sets, embedding and promotion, interface satisfaction, type inference,
instantiation, initialisation order, the `unsafe` package, and the error set.

Not carried:

| Dropped | Because |
| --- | --- |
| The `Info` maps nanogo does not read | The IR builder ([020](020-ir.md)) names what it needs; the rest is recorded work with no consumer. |
| `gccgo` size rules | One target family. [030](030-abi.md) owns sizes. |
| Deprecated API surface | Nothing outside nanogo calls this package. |

Everything dropped is dropped in the rewrite table, so a later change upstream
that touches it is visible as a conflict rather than silently absent.

## What nanogo adds

The fork is extended, and these are the extensions that make it nanogo's rather
than a copy:

1. **Pragma attachment.** `//go:` directives from
   [016](016-directives-and-pragmas.md) are attached to the objects they modify
   during declaration checking, because that is where the object is created and
   nowhere later has the association.
2. **Layout facts.** Sizes, alignments, and field offsets are computed here, by
   [030](030-abi.md)'s rules, and travel on the type. Recomputing them in the
   backend would give two answers to one question.
3. **Position fidelity for the backend.** Upstream records what diagnostics need.
   nanogo also needs the position of every expression that will become an
   instruction, for [046](046-debug-info.md)'s line table.

## The escape hatch, and its price

If the port resists — if the rewrite table grows without converging — the
fallback is to keep `go/types` unmodified over `go/ast`, parse a second time with
`go/parser`, and translate the checked result into nanogo's IR.

The price is stated so the fallback is never taken quietly: two parses of every
file, positions that must be mapped between two schemes, `go/ast`'s tree in the
dependency set forever, and the loss of every extension listed above, each of
which would then need a side table keyed by node identity.

The decision point is M2 in [003](003-sequencing.md). The trigger is the rewrite
table failing to converge, not the port being tedious.

## Errors

nanogo's messages are its own ([052](052-diagnostics.md)); its judgements are
upstream's. [004](004-conformance.md) L1 compares judgements and positions, and
`errorcheck` in L2 compares against patterns that most nanogo messages will
match anyway, since the upstream error *codes* are carried.

## Testing

- Type-check every package in the distribution and agree with `go/types` on
  accept and reject.
- The upstream `types2` test suite, ported with the sources. It comes with the
  fork and it is the reason the fork is safe.
- `errorcheck` from Go's `test/` corpus, position-exact.
- The generator's own drift test: regenerate and compare.
