---
title: "The package loader: from a directory to a build list"
status: draft
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

## G1: ask the toolchain

At G1 nanogo runs `go list -json -deps -export`, which returns the resolved
package graph with file lists, import maps, build-tag decisions, and the path to
each dependency's compiled archive. The `go` command has already applied module
resolution, vendoring, build constraints, and file suffix rules.

This is not a placeholder to be embarrassed about. It is the correct dependency
at G1 and it lets M1 through M6 in [003](003-sequencing.md) proceed without a
module system. `golang.org/x/tools/go/packages` is a wrapper over the same call
and is used where its convenience is worth the dependency.

The seam is one interface, and it is the whole reason this is a spec:

```go
type Loader interface {
    Load(patterns ...string) ([]*Package, error)
}
```

Nothing above the loader knows which implementation answered. The G2 work is a
second implementation, not a refactor.

## G2: resolve it directly

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

**`gc` is the compiler tag nanogo sets**, not `nanogo`. The distribution
branches on `gc` against `gccgo` in several places, and a third value would
select neither branch. nanogo claims to be `gc`-compatible everywhere else
([000](000-decisions.md) decision 11); here too.

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
Test variants — a package compiled with its own `_test.go` files — are a distinct
package with a distinct path suffix, because two versions of the same path exist
in one binary and their symbols must not collide.

## Testing

- Differential against `go list` over the whole distribution and over nanogo's
  own module: same file lists, same import maps, same build-tag decisions. This
  is available from M1 because the G1 implementation is the oracle for the G2
  one.
- Constraint evaluation against Go's own `go/build` constraint tests.
- Minimal version selection against hand-built module graphs with the answer
  computed by hand.
