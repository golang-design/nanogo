---
title: "Diagnostics"
status: in progress
layer: driver
gate: G1
depends_on:
  - 010-scanner-and-positions.md
  - 012-type-checking.md
---

# Diagnostics

What the compiler prints when the program is wrong, and what it prints when it is
asked about itself.

## What is built

There is no diagnostics package. What exists is spread across the two places
that produce messages today, and it is about a third of this spec:

| Section | State |
| --- | --- |
| Error format | built; `-trimpath` reaches the object's line table and not the messages |
| Ordering | not built, and not yet reachable: nothing in the compiler is concurrent |
| The ten-error limit | built as `driver.maxReportedErrors`, without the `too many errors` line |
| `-C` and `-e` | **not accepted at all**; see below |
| Cascades | built in the parser, which returns `Bad*` nodes and never nil, and inherited from the fork in the checker |
| Wording and error codes | inherited from the fork |
| `-S` assembly listing | **not built**; the flag is parsed and ignored |
| `-m` optimization decisions | **not built**; there is nothing to report |
| Internal errors | partly built, through [021](021-ssa-construction.md)'s verifier |

## Errors

### Format

```
file.go:12:5: undefined: foo
```

Path, line, column, message. The path is as given on the command line, rewritten
by `-trimpath` when set. Columns are byte counts
([010](010-scanner-and-positions.md)) and are printed unless `-C` suppresses
them.

The format is what the two producers already emit: the checker formats
`filename:line:column: message` itself, and the driver formats its own parse
errors through the file set. The `-trimpath` half is not done. `driver.TrimPath`
rewrites the file names that go into the object's line table and nothing
rewrites the ones in a message, so a diagnostic still carries the absolute path
the build handed to the compiler.

**`-C` and `-e` are not in the flag table nanogo parses**, so
`driver.ParseCompile` returns a `FlagError` for either one and
[051](051-build-integration.md)'s selection sends the package to `gc`. The
consequence is worse than a missing feature: a build that passes `-C` or `-e`
turns nanogo off for every allowlisted package and says nothing, which is the
silent failure [050](050-driver.md)'s rejection rule exists to make loud. Found
by this audit, comparing this section against `driver/flags.go`'s `knownFlags`.
Either the flags are implemented or this section drops them, and implementing
them is cheap.

Positions are **reported** positions, so `//line` directives apply. This is the
distinction [010](010-scanner-and-positions.md) draws between raw and reported,
and diagnostics are the reason it exists.

### Ordering and limits

Errors are printed in source position order, not in discovery order. Discovery
order depends on the order packages and functions were processed, which is
concurrent, so printing in discovery order would make the output
non-deterministic, a violation of [053](053-determinism.md) that a user would
see directly.

At most ten errors are printed, then `too many errors`, unless `-e` is given.
This matches `gc` and it matters for [004](004-conformance.md)'s L1 comparison,
which compares first errors.

The ordering rule is implemented. `driver.diagnostics` collects every message
with the raw position it belongs to and sorts by that position before the limit
is applied. Raw, not reported: `syntax.Pos` states that comparison uses raw and
printing uses reported, and a raw position is a byte offset into the file set,
so one comparison orders by file and then by offset. A message with no position
sorts last, because an error the compiler could not locate must not displace the
one the user can act on.

Discovery order is not source order, and the corpus proved it rather than the
reasoning. types2 reports a duplicate label where it meets it and an unused
label only when it has finished the function body, so `test/label.go` came out
with line 31 first and line 16 sixth. `test/import1.go`, `test/method1.go` and
`test/method2.go` inverted the same way. All four were rejected for the right
reason at the wrong first line until the sort was added. gc sorts in
`base.FlushErrors` for this reason.

The limit is `driver.maxReportedErrors`, which is ten. It is applied after the
sort, so the ten messages kept are the ten the user reads first rather than the
ten the checker found first. It still stops without printing the `too many
errors` line that tells the reader there are more, and that half of the rule is
unbuilt.

### Cascades

One mistake must produce one message. The mechanisms:

- The parser produces `Bad*` nodes rather than nil ([011](011-parser-and-ast.md)),
  and the type checker is silent about them.
- A value whose type could not be determined gets an invalid type that suppresses
  every later message about it.
- At most one error per position.

A compiler that reports twelve errors for a missing brace is a compiler whose
error output is not read.

The first mechanism is built: `syntax/parser.go` states that no production
returns nil and that a failed one returns a `BadExpr`, `BadStmt` or `BadDecl`
covering the range it consumed. The other two come from the fork with the
checker.

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

**Nothing is printed.** `-S` is parsed into `Config.AsmListing` and no code
reads that field, so a build that asked for a listing gets silence and exit
status zero. [050](050-driver.md) records this as a violation of its own
rejection rule.

The disassembly the tests use is not this listing. `ssagen`'s tests read the
encoded bytes back with `go tool objdump`, so the oracle is the toolchain rather
than a nanogo printer. A `-S` implementation would be nanogo's own text and
would need that oracle to check it.

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

Unbuilt on both sides. There is no escape analysis and no inliner, so there is
no decision to print, and [004](004-conformance.md) records that Go's `test/`
corpus is read by nothing.

## Internal errors

A compiler bug, a failed invariant or a missing lowering rule or an unencodable
instruction, prints:

- what invariant failed, in terms a compiler author can act on;
- the function being compiled and the position in the source;
- the pass that was running;
- a request to report it.

It never continues. A compiler that recovers from an internal error produces
output that is wrong in a way nobody will look for.

The verifier of [021](021-ssa-construction.md) is the main producer of these, and
that is its purpose: to convert a silent miscompile into a loud internal error in
the pass that caused it.

That much runs. `driver.compileFunc` verifies after the ABI pass and again after
lowering, and a violation stops the compile with the function, the position, the
pass name and every violation the verifier found. What is missing is the
distinction: a verifier violation is reported as an `UnsupportedError`, in the
same shape as a construct nanogo has not written yet. A gap and a bug read alike
to whoever gets the message, and only the second one is worth reporting.

## Testing

None of the four checks below exists. They are what the sections above need,
and each one waits on the feature it measures rather than on a harness:

- `errorcheck` from Go's `test/` corpus, which pins positions and patterns.
- Cascade tests: a corpus of single mistakes, each asserted to produce exactly
  one message.
- Ordering: compile a package with errors in several files concurrently, and
  assert the output is identical across runs.
- `-m` output against Go's escape and inline corpora.

What covers diagnostics today is upstream's: the fork's 375-entry `errorcheck`
corpus pins the checker's messages and positions against the checker it came
from ([012](012-type-checking.md)). It says nothing about the driver's output.
