---
title: "Diagnostics"
status: draft
layer: driver
gate: G1
depends_on:
  - 010-scanner-and-positions.md
  - 012-type-checking.md
---

# Diagnostics

What the compiler prints when the program is wrong, and what it prints when it is
asked about itself.

## Errors

### Format

```
file.go:12:5: undefined: foo
```

Path, line, column, message. The path is as given on the command line, rewritten
by `-trimpath` when set. Columns are byte counts
([010](010-scanner-and-positions.md)) and are printed unless `-C` suppresses
them.

Positions are **reported** positions, so `//line` directives apply. This is the
distinction [010](010-scanner-and-positions.md) draws between raw and reported,
and diagnostics are the reason it exists.

### Ordering and limits

Errors are printed in source position order, not in discovery order. Discovery
order depends on the order packages and functions were processed, which is
concurrent, so printing in discovery order would make the output
non-deterministic — a violation of [053](053-determinism.md) that a user would
see directly.

At most ten errors are printed, then `too many errors`, unless `-e` is given.
This matches `gc` and it matters for [004](004-conformance.md)'s L1 comparison,
which compares first errors.

### Cascades

One mistake must produce one message. The mechanisms:

- The parser produces `Bad*` nodes rather than nil ([011](011-parser-and-ast.md)),
  and the type checker is silent about them.
- A value whose type could not be determined gets an invalid type that suppresses
  every later message about it.
- At most one error per position.

A compiler that reports twelve errors for a missing brace is a compiler whose
error output is not read.

### Wording

nanogo's messages are its own, but the *judgement* is upstream's
([012](012-type-checking.md)) and the upstream error codes are carried. Where a
message can match `gc`'s without contortion it should, because Go's `errorcheck`
corpus matches against patterns and a matching message is a free test.

## Assembly listings

`-S` prints the generated code in Plan 9 assembly syntax, with positions, symbol
names, and the `PCDATA` and `FUNCDATA` directives interleaved.

This is a **debugging output and not a build path**. [000](000-decisions.md)
decision 3 rejected assembly text as an intermediate for reasons that do not
apply to reading it. The listing's value is that it is directly comparable to
`go tool compile -S` on the same source, which makes a code generation difference
visible in a diff.

## Optimization decisions

`-m` prints inlining and escape decisions in `gc`'s format:

```
./x.go:5:6: can inline f
./x.go:9:10: inlining call to f
./x.go:5:8: leaking param: p
./x.go:12:9: &T{...} escapes to heap
```

The format is copied rather than invented for one reason: Go's `test/escape*.go`
and `test/inline*.go` files annotate the expected output, and matching the format
turns that corpus into a test of nanogo's analyses. This is stated in
[023](023-escape-analysis.md) and [024](024-inlining-and-devirtualization.md) and
it is the reason this section exists.

## Internal errors

A compiler bug — a failed invariant, a missing lowering rule, an unencodable
instruction — prints:

- what invariant failed, in terms a compiler author can act on;
- the function being compiled and the position in the source;
- the pass that was running;
- a request to report it.

It never continues. A compiler that recovers from an internal error produces
output that is wrong in a way nobody will look for.

The verifier of [021](021-ssa-construction.md) is the main producer of these, and
that is its purpose: to convert a silent miscompile into a loud internal error in
the pass that caused it.

## Testing

- `errorcheck` from Go's `test/` corpus, which pins positions and patterns.
- Cascade tests: a corpus of single mistakes, each asserted to produce exactly
  one message.
- Ordering: compile a package with errors in several files concurrently, and
  assert the output is identical across runs.
- `-m` output against Go's escape and inline corpora.
