<h1 align="center">nanogo</h1>

<p align="center">
  <strong>A small Go compiler that compiles Go programs to native arm64 code.</strong>
  <br>
  Small enough to read end to end. Early enough that most of Go is still refused.
</p>

<p align="center">
  <a href="https://pkg.go.dev/golang.design/x/nanogo"><img src="https://pkg.go.dev/badge/golang.design/x/nanogo.svg" alt="Go Reference"></a>
  <a href="https://github.com/golang-design/nanogo/actions/workflows/ci.yml"><img src="https://github.com/golang-design/nanogo/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-BSD--3--Clause-blue.svg" alt="License: BSD-3-Clause"></a>
  <img src="https://img.shields.io/badge/go-1.27+-00ADD8.svg" alt="Go 1.27+">
  <img src="https://img.shields.io/badge/status-early-orange.svg" alt="Status: early">
</p>

---

nanogo compiles Go source to native arm64 machine code. It writes the same
object files the Go toolchain writes, so `go tool link` links its output
against the real Go runtime into a program that runs.

nanogo is a compiler under construction. It compiles a small part of Go, so
for most programs the answer today is that it cannot compile them. It says so
by name, at compile time, rather than emitting code it cannot emit correctly.
There is no release. Build it from source and try it.

## Install

```sh
go install golang.design/x/nanogo/cmd/nanogo@latest
```

Or from a clone:

```sh
git clone https://github.com/golang-design/nanogo && cd nanogo
go build -o nanogo ./cmd/nanogo
```

There is no tagged release, so `@latest` installs the current commit.

The `go` command must be on `PATH`, and every build says why: `go list`
resolves the packages you name, and `go tool link` writes the executable.
nanogo has no linker.

## Compile a program

```sh
mkdir hello && cd hello && go mod init hello
```

```go
// main.go
package main

func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

func main() {
	println("fib(10) =", fib(10))
}
```

```console
$ nanogo build .
nanogo: 1 of 28 packages compiled by nanogo; 27 by go1.27.0 (everything not named on the command line)
nanogo: the standard library and the runtime come from /usr/local/go (the installed Go toolchain)
nanogo: the executable was written by go tool link; nanogo has no linker (specs/045-linker.md)
$ ./hello
fib(10) = 55
```

That number is real. nanogo compiled `fib` and `main` into arm64 instructions,
`go tool link` linked them against the Go runtime, and the process printed 55.

The three lines nanogo prints are the honest part, and it prints them on every
build. nanogo compiled 1 package. The Go toolchain compiled the other 27,
because a Go program needs a scheduler, an allocator and a garbage collector
before `main` runs, and nanogo compiles none of those.

### Calling code the Go toolchain compiled

`fmt.Println` is refused, for the reason below. Put the printing in a package
nanogo does not compile, and it works:

```go
// say/say.go
package say

import "fmt"

// Number writes n and a newline to standard output.
// The Go toolchain compiles this package, so it may use anything Go has.
func Number(n int) { fmt.Println(n) }
```

```go
// main.go
package main

import "count/say"

func sum(xs ...int) int {
	total := 0
	for _, x := range xs {
		total = total + x
	}
	return total
}

func main() {
	say.Number(sum(1, 2, 3, 4))
}
```

```console
$ nanogo build .
nanogo: 1 of 58 packages compiled by nanogo; 57 by go1.27.0 (everything not named on the command line)
$ ./count
10
```

nanogo compiled the variadic function, the range loop and the call. The
toolchain compiled `say` and the standard library under it. `fmt` works there
because package initialization runs: `os.Stdout` is a real file by the time
`main` starts.

## Can nanogo compile your program?

Probably not yet. This is the list, and every entry on it is a program in
[`internal/audit/testdata/probes`](internal/audit/testdata/probes) that was
compiled and run against `gc` as the oracle. `nanogo help` prints the same
list, and `sh run.sh` in that directory reproduces it.

### What compiles

- Integer arithmetic, comparisons, conversions between numeric types,
  indexing, and constants.
- `return`, assignment, `:=`, multi-value assignment such as `a, b = b, a`,
  `if`, `for`, `switch` including the expressionless form, `fallthrough`,
  `break`, `continue`, labels and `goto`.
- Calls: recursive, variadic, methods on a value and on a pointer receiver,
  and a call into a package the Go toolchain compiled, as `os.Exit` and
  `say.Number` above show. A function with an empty body compiles.
- Slice literals, `make([]int, n)`, `len`, index and slice expressions, and
  `range` over a slice or over an integer.
- Strings: a literal, `len`, concatenation, indexing, comparison, and a string
  as a parameter or a result.
- A struct type declared in the package being compiled: a composite literal,
  reading and writing a field, and passing one by value. A struct up to four
  machine words is returned by value.
- `new`, and reading and writing through the pointer.
- Package-level variables, including one whose initializer is an expression, a
  string, or a slice literal.
- Package initialization. `init` runs, in the package nanogo compiled and in
  every package it imports, so a package variable such as `os.Stdout` is not
  nil.
- `defer` and `go` of a declared function that takes no arguments. Deferred
  calls run in reverse order, including calls deferred in a loop.
- A closure that captures nothing.
- `print` and `println` of integers, strings and booleans.

### What is refused

nanogo names the function, the position and the construct:

```console
$ nanogo build .
nanogo: main: nanogo cannot compile function main at /tmp/app/main.go:3:6: ir.Lower: ir: lowering main: append: no row of the lowering table is built for it yet
```

- A closure that captures a variable, and `defer` or `go` whose call has an
  argument, because the argument becomes a capture. A method's receiver is an
  argument, so `defer f.end()` is refused with it.
- `defer println(x)`. A builtin is not a function value, so there is nothing
  to hand the runtime.
- A conversion to an interface, a type assertion, and a type switch. The first
  is why `fmt.Println` is refused, because every argument to `fmt` converts to
  `any`, and why `panic("boom")` is refused while `panic(err)` is not:

  ```console
  $ nanogo build .
  nanogo: main: nanogo cannot compile function main at /tmp/greet/main.go:5:6: ssa.Build: ssa: main: convert: a conversion from string to interface is not built yet
  ```
- `recover`, when its result is read.
- `append`.
- Every map operation and every channel operation, including a package-level
  variable whose type is a channel.
- `range` over a string, and a conversion between `string` and `[]byte`.
- Floating point: arithmetic, a parameter, a result, and `println` of a float.
- A local array.
- `make` of a slice whose element type has methods, or is an interface.
  `make([]byte, n)` and `make([]T, n)` for a method-free struct `T` compile;
  `make([]C, n)` where `C` has one method is refused, because the descriptor
  for it needs the method's signature.
- Generics.
- Taking the address of a variable the compiler keeps in a register.
- A package with assembly in it, and a package that imports `"C"`.

One refusal reads as a crash. A function that returns a value wider than four
machine registers stops the build with `panic: ssa: lower: Store: no arm64
rule lowered this operation` and a Go stack trace, naming no position in your
source. No program comes out, so it is a refusal, but it is a poor one.

### Two failures nanogo does not announce

These cost more than any refusal, because nothing is said at compile time and
a program comes out that behaves differently from the one `gc` builds. They
are what the probe corpus found, and a corpus is a sample, so there may be a
third.

| What you write | What happens |
| --- | --- |
| `//go:embed` | it compiles, and the variable is empty at run time |
| `panic(err)` where the operand is already an interface | it compiles, and the runtime dies with `runtime: name offset out of range` instead of printing the panic |

Both are bugs with no fix yet. Until there is one, do not put a
nanogo-compiled package into a program you care about.

## What nanogo does not do

**It does not compile the standard library.** Every package your program
imports was compiled by `gc`, out of the Go toolchain on your machine. nanogo
reads the export data `gc` wrote. The count nanogo prints on every build is
how much of the program that is.

**It does not link.** `go tool link` writes the executable. nanogo has no
linker yet. See [`specs/045-linker.md`](specs/045-linker.md). `nanogo build`
does write the `modinfo` line the linker turns into `runtime.modinfo`, so
`runtime/debug.ReadBuildInfo` and `go version -m` report the package path,
the main module and every dependency module with its checksum. The build
settings recorded are `-buildmode`, `-compiler`, `CGO_ENABLED`, `GOARCH` and
`GOOS`. `DefaultGODEBUG`, the `GOARCH` feature level such as `GOARM64`,
`GOEXPERIMENT` and the `vcs` settings are absent: nanogo passes none of them
to anything, so recording one would describe a program it did not build.

**It has one architecture.** nanogo emits arm64 machine code, and a build for
another `GOARCH` is refused before anything is compiled:

```console
$ GOARCH=amd64 nanogo build .
nanogo: main: nanogo cannot compile for this target: nanogo emits arm64 machine code and the build is for amd64 (specs/043-amd64-backend.md is unbuilt)
```

`darwin/arm64` is the target the tests run on and the one to report a bug
against.

**It compiles one package at a time in a build.** The archive nanogo writes
does carry export data, and `gc` reads back all 275 of the 375 standard library
packages whose surface round-trips through it
([`specs/015-export-data.md`](specs/015-export-data.md)), so a package nanogo
compiled can be imported: `gc` compiles a package that imports one, and the
program runs. What `nanogo build` does not do yet is order two of its own
targets, so it refuses a build in which one package you named imports another.
Name one, or build the other with the `go` command.

**It reports no position inside an imported package.** `gc`'s second line,
`other declaration of New` naming a file under `GOROOT`, is missing from
nanogo's diagnostic, because an imported declaration has no position in the
file set of the package being compiled.

## How it works

```
source -> scanner -> parser -> type checker -> typed IR -> SSA -> machine ops -> goobj
```

Two intermediate representations and no more. A typed tree that still speaks
Go, and an SSA graph that starts target-neutral and ends target-specific.
[`specs/002-architecture.md`](specs/002-architecture.md) has the pipeline and
the package layout, and [`specs/`](specs/) has the reasoning behind every
decision below.

- **The parser is written and the type checker is forked.** Rewriting Go's type
  checker is the largest correctness risk in the project, and it buys nothing
  that bootstrapping needs. [`specs/012`](specs/012-type-checking.md)
- **The compiler emits object files, not assembly text.** Two experiments
  decided this, and both are in [`spikes/`](spikes/).
  [`specs/040`](specs/040-object-format.md)
- **The objects are compatible with `gc`.** That is why one package can be
  compiled by nanogo inside a build the Go toolchain otherwise owns, and why a
  failure has one suspect. [`specs/051`](specs/051-build-integration.md)
- **It is meant to be read end to end.** Every stage is one package with one
  job, and every spec states what its design gives up as well as what it buys.
  [`specs/002`](specs/002-architecture.md)

Generics will be fully stenciled rather than passed a dictionary. The language
guarantees that terminates. [`specs/013`](specs/013-generics.md)

## The measure of the project

The goal is a fixed point. nanogo compiles its own source, and the compiler
that results is byte-identical to itself. That has not happened yet.
"Bootstrap" names three separate properties, and nanogo keeps them apart.

| Gate | Means | Reached |
| --- | --- | --- |
| **G1** self-compiling | nanogo compiles nanogo, and the result is byte-identical to itself | no |
| **G2** toolchain-independent | it builds with no `go` binary on the machine: its own linker, its own package loader | no |
| **G3** distribution-compiling | it compiles the pure-Go Go distribution, runtime included | no |

G2 and G3 are siblings. Neither needs the other.
[`specs/001-bootstrap-gates.md`](specs/001-bootstrap-gates.md) has the
definitions and the fixed-point protocol.
[`specs/003-sequencing.md`](specs/003-sequencing.md) says which milestone owns
each missing piece.

## Working on nanogo

`nanogo build` is the front door. The seam below it is `-toolexec`, and that is
how nanogo is tested against real packages: the go command runs nanogo in place
of each toolchain invocation, nanogo compiles the packages on an allowlist, and
the real tool gets everything else.

```sh
cat > allowlist <<'EOF'
# The import paths nanogo owns in this build. A main package is spelled
# "main", because that is the name the go command passes in -p.
main
EOF

NANOGO_ALLOWLIST=./allowlist NANOGO_LOG=./log go build -toolexec=nanogo .
```

`NANOGO_LOG` records one line per invocation, so a build reports what the
allowlist selected. This is the `count` module above:

```
delegated count/say not on the allowlist
compiled main /var/folders/.../b001/_pkg_.a
```

An allowlist that names nothing delegates everything, and the build then
succeeds without nanogo compiling a line. That is why the log exists.

`nanogo version` prints the release the objects are compatible with.
[`CONTRIBUTING.md`](CONTRIBUTING.md) has the rest.

## What is built

Every package is gated against an external oracle rather than against itself.
That is the point: a compiler's bugs are invisible in its own output.

Coverage is stated rounded down, and the gate is 90% per package.

| Package | Coverage | What proves it |
| --- | --- | --- |
| [`syntax`](syntax/) | 99% | 19,674 files agree with `go/scanner` on tokens and positions; 16,293 agree with `go/parser` on accept, reject and first error |
| [`types2`](types2/) | see below | a fork of the Go type checker, re-pointed at nanogo's tree: 613 subtests, a 375-entry errorcheck corpus, and it type-checks nanogo's own source |
| [`loader`](loader/) | 98% | 6,821 files on two platforms agree with `go/build`; 536 packages agree with `go list` |
| [`obj`](obj/) | 98% | **`go tool link` links a nanogo object against the real Go runtime into a binary that runs** |
| [`obj/arm64`](obj/arm64/) | 99% | 981,124 encodings agree with `go tool asm`, with none disagreeing |
| [`ir`](ir/) | 94% | type layout agrees with `reflect`; the builder produces a typed tree for 536 packages of the Go distribution, 39,947 functions and 4,188,075 nodes |
| [`ssa`](ssa/) | 96% | construction, lowering, decomposition, ABI assignment, register allocation, liveness and stack maps, each with a verifier that has a negative test per invariant |
| [`ssa/rules`](ssa/rules/) | 97% | the arm64 rule set, checked by lowering the corpus and by a verifier after every rule |
| [`export`](export/) | 96% | reads gc's export data for all 375 packages of the standard library, 13,518 declarations, and for a fixture carrying every encoding the format has, checked declaration by declaration |
| [`export/pkgbits`](export/pkgbits/) | 93% | the container, ported from `internal/pkgbits` and exercised by every archive the reader above reads |
| [`ssagen`](ssagen/) | 92% | emits machine code that **links and runs**, and stack maps a real collector honours |
| [`rtsym`](rtsym/) | 100% | 106 runtime signatures checked against the runtime's own source |
| [`rtype`](rtype/) | 96% | type descriptors whose every field agrees, byte for byte, with the descriptor `gc` emitted for the same type |
| [`driver`](driver/) | 96% | a real `go build -toolexec` completes |

`types2` is excluded from the coverage gate, with the reason recorded in
[`internal/covercheck/exclusions.txt`](internal/covercheck/exclusions.txt): it
is a fork, and the gate that replaces coverage is upstream's own test suite,
ported with the sources.

The back end is proved by running programs. `ssagen`'s `TestLinkAndRun` is the
proof: 18 programs go from source text through the whole pipeline to a process
that returns the right answer, and several of them call into, or are called
from, code the Go toolchain compiled, so the calling convention is checked
across the toolchain boundary. The stack maps are proved by a collector: an object
reachable only from a nanogo frame slot survives a collection, the same object
with the slot killed is freed, and 200,000 frames are grown, copied and
unwound.

## How far it reaches

The Go distribution is the measure of reach. The `ir` row above builds a typed
tree from all of it, 39,947 functions. How much of that tree the middle end
accepts is the number below.

**17,905 of those functions reach SSA construction** from a tree the lowering
pass has not touched, and 17,809 of them lower completely to arm64 machine
operations. 17,758 of the 17,905 carry a stack map.

The lowering pass builds part of [`specs/020`](specs/020-ir.md)'s table, and
the driver runs it before construction, so a real compile reaches further:
**24,508 of them get past construction once the lowering pass has run**. A
composite literal, `len`, a slice expression, `new`, and `make` of a slice are
lowered and no longer refused.

Reaching construction is not the same as compiling. It counts functions the
middle end accepts, not programs that run. The list under "What is refused"
above is what stops the rest, and each entry is a row of that lowering table
that no pass performs, or the type descriptor those rows need.

So the back half of the compiler is real and the front of the middle end is not
finished. What stands between here and
[`specs/060`](specs/060-selfhost.md)'s fixed point is the language itself.

## Specs

[`specs/`](specs/) is the design deck, and it is written for somebody changing
the compiler rather than using it. Start with
[`specs/000-decisions.md`](specs/000-decisions.md), which is normative, then
[`specs/003-sequencing.md`](specs/003-sequencing.md) for the order of work.

The specs are corrected by the code rather than defended against it. Each spec
carries a status: `draft` means nothing is built, `in progress` means part of
it is, `complete` means its scope is built and gated. Where the code disproved
a spec, the spec says what was wrong and how it was found, because that record
is worth more than the claim it replaced.

The numbers in this file are gated. A test in
[`internal/hygiene/`](internal/hygiene/) reads them out of the prose and fails
when they disagree with what the tests measure, because every one of them was
true on the day it was written and several had stopped being true. The gate
reads numbers and cannot see a false capability claim, which is what
[`internal/audit/testdata/probes`](internal/audit/testdata/probes) is for.

## Spikes

[`spikes/`](spikes/) holds the experiments that settled the backend seam. Each
answers one question a spec depends on, and each still runs.

## License

BSD-3-Clause &copy; 2026 The [golang.design](https://golang.design) Initiative Authors
