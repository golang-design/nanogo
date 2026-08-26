---
title: "The package loader: from a directory to a build list"
status: in progress
layer: front end
gate: "G1 partially, G2 fully"
depends_on:
  - 002-architecture.md
---

# Package loader

The loader answers one question: given an import path and a build configuration,
which files are in that package, and what does it import? Everything the
compiler does downstream depends on that answer being the same one the `go`
command would give.

The spec is split because the gates split it ([001](001-bootstrap-gates.md)).
G1 is allowed to ask the `go` command. G2 is not.

## Where this stands

The G1 half is built and gated. `GoList` implements the interface below,
`loader` is above the 90% coverage gate, and the constraint evaluator agrees
with `go/build` over the corpora at the end of this file.

The G2 half is not built. There is no `go.mod` reader, no minimal version
selection, and no import resolution against the module cache or `vendor/`. The
`Loader` interface exists so that the G2 work is a second implementation rather
than a refactor, and that is still the plan.

The constraint evaluator is the exception inside the G2 half. It is written and
tested, in `loader/constraint.go`, because it needs no module graph and because
it is the piece a differential corpus can exercise with no compiler present. It
was built in M0 for that reason, ahead of the milestone
[003](003-sequencing.md) placed it in.

**The G1 half has two consumers, and neither of them is the `-toolexec` path.**
`driver/build.go` calls `loader.GoList` to resolve the graph `nanogo build` was
given a pattern for, and `internal/gotest` takes its build constraints from
`loader.Context`. Nothing else imports the package. Under `-toolexec` the `go`
command has already resolved the graph and hands the driver one package's file
list on the command line, so the loader is not in that path at all. Whole-world
mode is what puts it there, and that is G2 work.

## G1: ask the toolchain

At G1 nanogo runs `go list -e -json -deps -export`, which returns the resolved
package graph with file lists, import maps, build-tag decisions, and the path to
each dependency's compiled archive. The `go` command has already applied module
resolution, vendoring, build constraints, and file suffix rules.

`-e` is not optional. Without it, one unresolvable import makes the `go`
command exit before printing anything, so the requirement below that a
per-package error must not fail the whole load is unreachable. With it, the
error arrives attached to the package it belongs to, which is where a compiler
can report it.

Asking the toolchain is the correct dependency at G1, and it lets M1 through M6
in [003](003-sequencing.md) proceed without a module system.

**`golang.org/x/tools/go/packages` wraps the same call and is not used.**
`go.mod` declares no requirements at all. A front end that depends on a tooling
module has one more module to compile before it can compile itself, which is a
cost G1 pays for convenience G1 does not need. `GoList` runs the `go` command
and decodes the JSON stream itself, in 242 lines.

The seam is one interface, and it is the whole reason this is a spec:

```go
type Loader interface {
    Load(patterns ...string) ([]*Package, error)
}
```

Nothing above the loader knows which implementation answered. The G2 work is a
second implementation, not a refactor.

## G2: resolve it directly

**None of this is built.** The scope below is the plan, and the exclusions are
the part of it that is normative.

At G2 there is no `go` binary. nanogo resolves the graph itself. The scope is
deliberately narrower than the `go` command's, and the exclusions are the spec:

| In scope | Out of scope |
| --- | --- |
| Reading `go.mod`: module path, `go` directive, `require`, `replace`, `exclude` | Downloading anything. No network, ever. |
| Minimal version selection over the requirement graph | `go get`, version upgrades, `go.sum` rewriting |
| Resolving imports against the module cache, `vendor/`, and `GOROOT/src` | Proxy protocol, checksum database |
| Build constraint evaluation | Build cache management ([053](053-determinism.md) owns caching) |

nanogo consumes a module graph that is already materialised. A missing module is
an error that names the module, not a fetch.

### Minimal version selection

For a build list over modules $M$, where $R(m)$ is the requirement set declared
by module $m$, the selected version of a module $n$ is

$$
V(n) = \max \{\, v : (n, v) \in R^{*}(m_0) \,\}
$$

where $m_0$ is the main module and $R^{*}$ is the transitive closure of $R$.
`replace` directives in the main module rewrite edges before the closure is
taken; `replace` in any other module is ignored, matching the `go` command.

This is the whole algorithm. It is deterministic, it needs no solver, and that
is why it can be a section rather than a spec.

### Build constraints

This subsection is built, and it is the one part of the G2 half that is.

Two mechanisms, both required, because the distribution uses both.

**File name suffixes.** A file named `x_GOOS.go`, `x_GOARCH.go`, or
`x_GOOS_GOARCH.go` is constrained by its name. The suffix is only a constraint
when the component is a known `GOOS` or `GOARCH` value, so `x_test.go` and
`vector_amd64.go` are treated differently only because `amd64` is in the table.
A leading `_` or `.` excludes the file entirely.

**`//go:build` expressions.** A boolean expression over build tags with `&&`,
`||`, `!`, and parentheses, appearing before the package clause. The legacy
`// +build` form is parsed and, when both are present, the `//go:build` line
wins. The distribution still contains files with only the legacy form, so it
cannot be dropped.

The tag set is `GOOS`, `GOARCH`, the `go1.N` release tags for every $N$ up to the
targeted release, `unix` where it applies, `cgo` when enabled, `gc` as the
compiler tag, and `-tags` from the command line.

That list is incomplete and the omission matters. `go/build` also consults
`goexperiment.*` tags and target feature tags such as `arm64.v8.0`, and files
in the distribution are gated on both. A loader that does not know them
disagrees with the `go` command about which files are in the runtime, which is
the one package where being wrong is hardest to notice.

nanogo takes them from the caller rather than computing them, because computing
the experiment defaults is module resolution work and belongs to G2. At G1 the
`go` command has already decided and the answer is read from it.

The known `GOOS` and `GOARCH` tables are copied into nanogo and pinned to the
release in `go.mod`. There is no exported `go/build` data for them:
`internal/syslist` is internal to the standard library. A copy pinned with a
comment is the honest form of that dependency.

**`gc` is the compiler tag nanogo sets**, not `nanogo`. The distribution
branches on `gc` against `gccgo` in several places, and a third value would
select neither branch. nanogo claims to be `gc`-compatible everywhere else
([000](000-decisions.md) decision 11); here too.

### Two questions, two methods

"Do this file's build constraints match" and "is this file in the package" are
different questions, and the loader answers each with its own method.

| Method | Question | Oracle |
| --- | --- | --- |
| `MatchFile` | do the constraints match | `go/build.MatchFile` |
| `IncludeFile` | is the file in the package | `go/build.ImportDir`'s partition |

The difference is one rule that no build constraint expresses: **a file that
imports `"C"` is a cgo file, and with cgo disabled the `go` command reports it
as ignored rather than as part of the package.** Nothing in the file says so.

The loader uses `IncludeFile`. `MatchFile` is kept separate because it is the
differential oracle, and folding the cgo rule into it made 45 files in the
distribution disagree with `go/build`, all of them cgo files that `go/build`
matches and does not include.

Detecting the import is done by parsing, not by searching the text. `"C"` can
appear in a grouped import, behind a comment, inside another string, or with a
blank or dot name, and a text search gets at least one of those wrong. The parse
is not wasted in the compiler proper, since a file that reaches this point is
one nanogo is about to parse anyway.

**That disagreement is invisible on darwin.** The file that exposes it is
`crypto/internal/sysrand/internal/seccomp/seccomp_linux.go`, whose `_linux`
suffix excludes it on darwin before its imports are ever read, so only a linux
run sees it. It is the argument for the two-platform test matrix, written
down.

### Import resolution order

1. `unsafe`, which has no files.
2. Standard library, if the path has no dot in its first element: resolve under
   `GOROOT/src`.
3. `vendor/` directories, walking up from the importing package.
4. The module cache, using the build list.

A cycle is an error naming the whole cycle, detected during the topological walk
and not by a depth counter.

## Package identity

A package is identified by its import path, which is also its symbol prefix.
Test variants, a package compiled with its own `_test.go` files, are a distinct
package with a distinct path suffix, because two versions of the same path exist
in one binary and their symbols must not collide.

## Testing

Two corpora, and they are not interchangeable, because **`go list` is an oracle
only for its own `GOROOT`.** Pointed at a newer checkout of the Go repository
than the installed toolchain, it answers about the toolchain instead, and the
comparison is meaningless. One differential against `go list` over the whole
distribution is therefore not achievable, and asking for one is asking for a
comparison with no meaning.

| Corpus | Oracle | What it proves |
| --- | --- | --- |
| `GOROOT/src` and nanogo's own module | `go list` | file lists, import maps, and the partition of a directory's `.go` files |
| Any Go source tree, including a newer checkout | `go/build.MatchFile` | constraint evaluation, with no `go` command involved |

The second needs no toolchain agreement, so it is the one that runs against a
tree the installed `go` does not know.

**The corpus must not be optional.** `GOROOT/src` ships with every Go
installation, so it is present on a CI runner, and the tests read it from there
rather than from a developer's checkout. When `NANOGO_REQUIRE_CORPUS=1` is set,
which CI does, a missing or empty corpus fails rather than skips. A skipped test
is indistinguishable from a passing one in `go test ./...` output, and a gate
that silently compares zero files is not a gate.

Also:

- Constraint evaluation against Go's own `go/build` constraint tests.
- Minimal version selection against hand-built module graphs with the answer
  computed by hand. **Not built**, with the algorithm it tests.
- Determinism: load the same patterns twice and compare the output ordering.

The measured results, with `NANOGO_REQUIRE_CORPUS=1` set:

| Corpus | Result |
| --- | --- |
| `GOROOT` `.go` files walked | 8,078 |
| Per-file constraints against `go/build.MatchFile` | 6,821 files per platform, 0 mismatches, on `linux/amd64` and on `darwin/arm64` |
| `crypto` under `-tags purego` | 599 files, 0 mismatches |
| Package-level partition | 765 directories and 6,560 files on `linux/amd64`, 762 and 6,564 on `darwin/arm64` |
| `go list` agreement | 536 packages, 7,128 files, 0 mismatches: 379 standard library packages and 157 of nanogo's own. This is the count `internal/hygiene/testdata/facts.json` gates as `loader.golist.packages` |

### One deliberate difference from `go/build`

`go/build.MatchFile` parses the file and returns an error for source it cannot
parse. nanogo's evaluator reads header comments only and reports no such error.

This is intentional. A syntax error belongs to the parser
([011](011-parser-and-ast.md)), which has positions and can say where. A loader
that reported it would report it worse, and would have to parse every file twice.
The consequence is that a corpus walk skips `testdata` directories, where
deliberately broken files live.

## What was wrong

**The spec said no package outside `loader` imports it.** That was true when
the loader had no caller above it and stopped being true when `nanogo build`
landed: `driver/build.go` resolves its package graph through `loader.GoList`,
and `internal/gotest` reads its build constraints from `loader.Context`. The
claim the sentence was making, that the package is proved against `go list` and
`go/build` rather than against a compiler that depends on it, still holds for
the compile path, which the loader is not in.

**The `go list` agreement was recorded at 524 packages and 6,966 files.** It is
536 and 7,128, and the nanogo half moved from 145 packages to 157 as the
repository grew. The package count is gated, so the prose is the half that
drifted.

**`-e` was omitted from the `go list` invocation.** Without it one unresolvable
import makes the `go` command exit before printing anything, which makes the
per-package error requirement unreachable.

**The spec offered `golang.org/x/tools/go/packages` as a wrapper worth taking
where its convenience paid.** It is not taken and should not be: `go.mod`
declares no requirements, and a front end that depends on a tooling module has
one more module to compile before it can compile itself.

**The spec asked for one differential against `go list` over the whole
distribution.** `go list` answers only about its own `GOROOT`, so pointed at a
newer checkout it compares the toolchain with itself. The corpus was split in
two, and only the `go/build.MatchFile` half runs against a tree the installed
`go` does not know.

**`MatchFile` and `IncludeFile` were one method.** Folding the cgo rule into
the constraint evaluator was the shortcut, and the two methods above are what
replaced it. CI on linux found it; a darwin-only run could not have.
