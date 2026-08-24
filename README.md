<h1 align="center">nanogo</h1>

<p align="center">
  <strong>A small Go compiler that aims to compile itself.</strong>
</p>

<p align="center">
  <a href="https://pkg.go.dev/golang.design/x/nanogo"><img src="https://pkg.go.dev/badge/golang.design/x/nanogo.svg" alt="Go Reference"></a>
  <a href="https://github.com/golang-design/nanogo/actions/workflows/ci.yml"><img src="https://github.com/golang-design/nanogo/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-BSD--3--Clause-blue.svg" alt="License: BSD-3-Clause"></a>
  <img src="https://img.shields.io/badge/go-1.27+-00ADD8.svg" alt="Go 1.27+">
  <img src="https://img.shields.io/badge/status-early-orange.svg" alt="Status: early">
</p>

---

> [!IMPORTANT]
> **nanogo cannot compile a program yet.** The front end, the type checker, the
> object writer and the arm64 encoder are built and gated. The middle of the
> compiler, from the typed tree down through SSA to code generation, is not.
> Do not depend on this.

nanogo is a compiler for the Go language as defined by the
[Go language specification](https://go.dev/ref/spec). It is small on purpose: the
goal is a compiler that can be read end to end, under a stated budget of 40,000
lines, and the measure of the project is that it compiles its own source to a
fixed point.

## The three gates

"Bootstrap" names three separate properties, and nanogo keeps them separate.

| Gate | Means |
| --- | --- |
| **G1** self-compiling | nanogo compiles nanogo, and the result is byte-identical to itself |
| **G2** toolchain-independent | it builds with no `go` binary on the machine: its own linker, its own package loader |
| **G3** distribution-compiling | it compiles the pure-Go Go distribution, runtime included |

G2 and G3 are siblings. Neither needs the other.
[`specs/001-bootstrap-gates.md`](specs/001-bootstrap-gates.md) has the
definitions and the fixed-point protocol.

## Shape of the compiler

```
source → scanner → parser → type checker → typed IR → SSA → machine ops → goobj
```

Two intermediate representations and no more: a typed tree that still speaks Go,
and an SSA graph that starts target-neutral and ends target-specific.
[`specs/002-architecture.md`](specs/002-architecture.md) has the pipeline and the
package layout.

Some decisions worth knowing before reading further:

- **The parser is written; the type checker is forked.** Rewriting Go's type
  checker is the largest correctness risk in the project and buys nothing that
  bootstrapping needs — the reference Go compiler has a forked `go/types` of its
  own. [`specs/012`](specs/012-type-checking.md)
- **The compiler emits object files, not assembly text.** Two spikes decided
  this, and both are in [`spikes/`](spikes). [`specs/040`](specs/040-object-format.md)
- **It is meant to fit 40,000 lines for v1**, and the accounting in
  [`specs/000`](specs/000-decisions.md) says it currently does not, by five per
  cent. That is recorded rather than adjusted.
- **It is object-compatible with `gc`**, so `go build -toolexec=nanogo` can
  compile one package with nanogo while `gc` compiles the rest. That is how the
  compiler is brought up and how a failure gets one suspect.
  [`specs/051`](specs/051-build-integration.md)
- **Generics are fully stenciled.** The language guarantees this terminates, and
  the guarantee arrives with the forked checker.
  [`specs/013`](specs/013-generics.md)

## What is built

Every package is gated against an external oracle rather than against itself.
That is the point: a compiler's bugs are invisible in its own output.

| Package | Coverage | What proves it |
| --- | --- | --- |
| [`syntax`](syntax/) | 99% | 19,674 files agree with `go/scanner` on tokens and positions; 16,293 agree with `go/parser` on accept, reject and first error |
| [`types2`](types2/) | see below | a fork of the Go type checker, re-pointed at nanogo's tree: 613 subtests, a 375-package errorcheck corpus, and it type-checks nanogo's own source |
| [`loader`](loader/) | 99% | 6,821 files on two platforms agree with `go/build`; 467 packages agree with `go list` |
| [`obj`](obj/) | 98% | **`go tool link` links a nanogo object against the real Go runtime into a binary that runs** |
| [`obj/arm64`](obj/arm64/) | 99% | 864,092 encodings agree with `go tool asm`, with none disagreeing |
| [`ir`](ir/) | 94% | type layout agrees with `reflect`; the builder produces a typed tree for 536 packages of the Go distribution, 4.2M nodes |
| [`ssa`](ssa/) | 97% | construction, lowering and register allocation, each with a verifier that has a negative test per invariant; 4,755 of 8,238 distribution functions lower completely |
| [`rtsym`](rtsym/) | 100% | 41 runtime signatures checked against the runtime's own source |
| [`driver`](driver/) | 97% | a real `go build -toolexec` completes |

`types2` is excluded from the coverage gate, with the reason recorded in
[`internal/covercheck/exclusions.txt`](internal/covercheck/exclusions.txt): it
is a fork, and the gate that replaces coverage is upstream's own test suite,
ported with the sources.

**Not built:** liveness and stack maps, code emission, and the decomposition of
values wider than a machine register. That is what stands between here and a
program that runs.

## Specs

[`specs/`](specs/) is the design deck: 41 specs. Start with
[`specs/000-decisions.md`](specs/000-decisions.md), which is normative, then
[`specs/003-sequencing.md`](specs/003-sequencing.md) for the order of work.

The specs are corrected by the code rather than defended against it. Roughly
thirty defects have been found by implementing them, and each correction says
what was wrong and how it was found.

## Spikes

[`spikes/`](spikes/) holds the experiments that settled the backend seam. Each
answers one question a spec depends on, and each still runs.

## License

BSD-3-Clause &copy; 2026 The [golang.design](https://golang.design) Initiative Authors
