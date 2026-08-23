<h1 align="center">nanogo</h1>

<p align="center">
  <strong>A small Go compiler that aims to compile itself.</strong>
</p>

<p align="center">
  <a href="https://pkg.go.dev/golang.design/x/nanogo"><img src="https://pkg.go.dev/badge/golang.design/x/nanogo.svg" alt="Go Reference"></a>
  <a href="https://github.com/golang-design/nanogo/actions/workflows/ci.yml"><img src="https://github.com/golang-design/nanogo/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-BSD--3--Clause-blue.svg" alt="License: BSD-3-Clause"></a>
  <img src="https://img.shields.io/badge/go-1.27+-00ADD8.svg" alt="Go 1.27+">
  <img src="https://img.shields.io/badge/status-specs%20only-orange.svg" alt="Status: specs only">
</p>

---

> [!IMPORTANT]
> **No compiler exists yet.** This repository holds a design spec deck and two
> spikes. Nothing compiles anything. Do not depend on this.

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
- **It is object-compatible with `gc`**, so `go build -toolexec=nanogo` can
  compile one package with nanogo while `gc` compiles the rest. That is how the
  compiler is brought up and how a failure gets one suspect.
  [`specs/051`](specs/051-build-integration.md)
- **Generics are fully stenciled.** The language guarantees this terminates, and
  the guarantee arrives with the forked checker.
  [`specs/013`](specs/013-generics.md)

## Specs

[`specs/`](specs/) is the design deck: 33 specs, all `draft`, nothing built.
Start with [`specs/000-decisions.md`](specs/000-decisions.md), which is normative,
then [`specs/003-sequencing.md`](specs/003-sequencing.md) for the order of work.

## Spikes

[`spikes/`](spikes/) holds the experiments that settled the backend seam. Each
answers one question a spec depends on, and each still runs.

## License

BSD-3-Clause &copy; 2026 The [golang.design](https://golang.design) Initiative Authors
