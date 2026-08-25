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
> **nanogo compiles some Go programs, and most of Go is not among them.** The
> whole pipeline is built, from source text to a `goobj` file that `go tool
> link` links against the real Go runtime into a binary that runs. What it
> accepts is far narrower than Go. SSA construction refuses a composite
> literal, a `range`, a closure, `defer`, `panic` and most builtins, so about
> two functions in five of the standard library get past it. Do not depend on
> this.

nanogo is a compiler for the Go language as defined by the
[Go language specification](https://go.dev/ref/spec). It is small on purpose:
the goal is a compiler that can be read end to end, under a stated budget of
40,000 lines, and the measure of the project is that it compiles its own source
to a fixed point.

## Use it

```sh
go install golang.design/x/nanogo/cmd/nanogo@latest
```

nanogo runs as the go command's compiler, for the packages you name and no
others:

```sh
cat > allowlist <<'EOF'
# the package nanogo owns in this build. A main package is spelled "main",
# because that is the name the go command passes in -p.
main
EOF

NANOGO_ALLOWLIST=./allowlist go build -toolexec=nanogo ./...
```

Everything not on the list is handed to `gc`, so a build is part nanogo and
part the real toolchain. Set `NANOGO_LOG=./log` and nanogo records what it
compiled, what it delegated, and what it refused:

```
compiled main /var/folders/.../b001/_pkg_.a
delegated internal/cpu not on the allowlist
```

A refusal names the function and the construct rather than emitting something
wrong:

```
nanogo: main: cannot compile function main at ./main.go:5:6:
        ssa.Build: ssa: main: compositelit reached SSA construction
```

That is the honest state of the compiler: this program compiles and runs,

```go
package main

func compute(a, b int) int { return a*b + 1 }

func main() { compute(20, 3) }
```

and so does the same program with `x := a * b` in it, which it did not until
SSA construction learned the assignment statement. Adding `p := point{1, 2}`
still does not. `nanogo help` lists the subset. One target, `darwin/arm64`.

## The three gates

"Bootstrap" names three separate properties, and nanogo keeps them separate.

| Gate | Means | Reached |
| --- | --- | --- |
| **G1** self-compiling | nanogo compiles nanogo, and the result is byte-identical to itself | no |
| **G2** toolchain-independent | it builds with no `go` binary on the machine: its own linker, its own package loader | no |
| **G3** distribution-compiling | it compiles the pure-Go Go distribution, runtime included | no |

G2 and G3 are siblings. Neither needs the other.
[`specs/001-bootstrap-gates.md`](specs/001-bootstrap-gates.md) has the
definitions and the fixed-point protocol.

## Shape of the compiler

```
source -> scanner -> parser -> type checker -> typed IR -> SSA -> machine ops -> goobj
```

Two intermediate representations and no more: a typed tree that still speaks Go,
and an SSA graph that starts target-neutral and ends target-specific.
[`specs/002-architecture.md`](specs/002-architecture.md) has the pipeline and the
package layout.

Some decisions worth knowing before reading further:

- **The parser is written; the type checker is forked.** Rewriting Go's type
  checker is the largest correctness risk in the project and buys nothing that
  bootstrapping needs. The reference Go compiler has a forked `go/types` of its
  own. [`specs/012`](specs/012-type-checking.md)
- **The compiler emits object files, not assembly text.** Two spikes decided
  this, and both are in [`spikes/`](spikes/). [`specs/040`](specs/040-object-format.md)
- **It is meant to fit 40,000 lines for v1.** The compiler is 36,237 lines
  today, and escape analysis, inlining and generics instantiation are not
  written, nor is the export data writer. [`specs/000`](specs/000-decisions.md) carries the accounting
  and the two places to recover the budget if it runs out.
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

Coverage is stated rounded down, and the gate is 90% per package.

| Package | Coverage | What proves it |
| --- | --- | --- |
| [`syntax`](syntax/) | 99% | 19,674 files agree with `go/scanner` on tokens and positions; 16,293 agree with `go/parser` on accept, reject and first error |
| [`types2`](types2/) | see below | a fork of the Go type checker, re-pointed at nanogo's tree: 613 subtests, a 375-entry errorcheck corpus, and it type-checks nanogo's own source |
| [`loader`](loader/) | 98% | 6,821 files on two platforms agree with `go/build`; 524 packages agree with `go list` |
| [`obj`](obj/) | 98% | **`go tool link` links a nanogo object against the real Go runtime into a binary that runs** |
| [`obj/arm64`](obj/arm64/) | 99% | 981,124 encodings agree with `go tool asm`, with none disagreeing |
| [`ir`](ir/) | 94% | type layout agrees with `reflect`; the builder produces a typed tree for 536 packages of the Go distribution, 39,947 functions and 4,188,075 nodes |
| [`ssa`](ssa/) | 96% | construction, lowering, decomposition, ABI assignment, register allocation, liveness and stack maps, each with a verifier that has a negative test per invariant |
| [`ssa/rules`](ssa/rules/) | 97% | the arm64 rule set, checked by lowering the corpus and by a verifier after every rule |
| [`export`](export/) | 96% | reads gc's export data for all 375 packages of the standard library, 13,518 declarations, and for a fixture carrying every encoding the format has, checked declaration by declaration |
| [`export/pkgbits`](export/pkgbits/) | 92% | the container, ported from `internal/pkgbits` and exercised by every archive the reader above reads |
| [`ssagen`](ssagen/) | 92% | emits machine code that **links and runs**, and stack maps a real collector honours |
| [`rtsym`](rtsym/) | 100% | 59 runtime signatures checked against the runtime's own source |
| [`rtype`](rtype/) | 96% | type descriptors whose every field agrees, byte for byte, with the descriptor `gc` emitted for the same type |
| [`driver`](driver/) | 97% | a real `go build -toolexec` completes |

`types2` is excluded from the coverage gate, with the reason recorded in
[`internal/covercheck/exclusions.txt`](internal/covercheck/exclusions.txt): it
is a fork, and the gate that replaces coverage is upstream's own test suite,
ported with the sources.

## How far it reaches

**nanogo compiles Go source to a running program.** `ssagen`'s `TestLinkAndRun`
is the proof: 18 programs go from source text through the whole pipeline to a
process that returns the right answer, and several of them call into, or are
called from, `gc`-compiled code, so the calling convention is checked across the
toolchain boundary. Its stack maps are proved by a collector: an object
reachable only from a nanogo frame slot survives a collection, the same object
with the slot killed is freed, and 200,000 frames are grown, copied and unwound.

The honest measure of reach is the corpus, and it is one number with two halves.

**17,905 of those functions reach SSA construction**, and 17,809 of them lower
completely to arm64 machine operations. 17,758 of the 17,905 carry a stack map.
What is left undecomposed is 87 functions holding a wide `SelectN`, 12 holding
an array and 93 holding a struct.

The remaining 22,042 functions never reach SSA at all. Construction refuses
them by name, and the counts say what is missing:

| Functions refused | Because construction does not build |
| --- | --- |
| 4,841 | a composite literal |
| 2,800 | `len` |
| 2,253 | a conversion to an interface, which needs a type descriptor |
| 1,605 | `range` |
| 1,371 | a method selected out of an interface |
| 1,132 | a closure |
| 1,052 | the address of a composite literal |
| 934 | `panic` |
| 903 | a slice expression |
| 843 | `make` |
| 672 | `defer` |
| 529 | `append` |
| 450 | a multi-value assignment from a type assertion |
| 346 | an index of a map |
| 334 | `new` |
| the rest | the address of a value, a two-value map read, type switches, type assertions, `print`, `recover`, `copy`, `min` and `max`, `select`, `send`, `recv`, `cap`, `clear`, and the `unsafe` intrinsics |

Every row of that table is a row of [`specs/020`](specs/020-ir.md)'s lowering
table that no pass performs, or the type descriptor those rows need. A row that
was not, a multi-value assignment whose results are wider than a register at
2,191 functions, is gone: it was a limit in the pass below construction, where
`SelectN` names a result before decomposition and a machine word of the result
area after it, and the renumbering between the two was only correct for the
first result.

So the back half of the compiler is real and the front of the middle end is not
finished. What stands between here and
[`specs/060`](specs/060-selfhost.md)'s fixed point is the language itself.
[`specs/003`](specs/003-sequencing.md) says which milestone owns each piece.

## Specs

[`specs/`](specs/) is the design deck: 41 specs. Start with
[`specs/000-decisions.md`](specs/000-decisions.md), which is normative, then
[`specs/003-sequencing.md`](specs/003-sequencing.md) for the order of work.

The specs are corrected by the code rather than defended against it. Each spec
carries a status: `draft` means nothing is built, `in progress` means part of it
is, `complete` means its scope is built and gated. Where the code disproved a
spec, the spec says what was wrong and how it was found, because that record is
worth more than the claim it replaced.

The numbers in this file and in
[`specs/003-sequencing.md`](specs/003-sequencing.md) are gated. A test in
[`internal/hygiene/`](internal/hygiene/) reads them out of the prose and fails
when they disagree with what the tests measure, because every one of them was
true on the day it was written and several had stopped being true.

## Spikes

[`spikes/`](spikes/) holds the experiments that settled the backend seam. Each
answers one question a spec depends on, and each still runs.

## License

BSD-3-Clause &copy; 2026 The [golang.design](https://golang.design) Initiative Authors
