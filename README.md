<h1 align="center">nanogo</h1>

<p align="center">
  <strong>A small Go compiler that compiles Go programs to native arm64 code.</strong>
  <br>
  Small enough to read end to end. Early enough that much of Go is still refused.
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

nanogo is early. It accepts a part of Go, not all of Go. It compiles the
package you name and takes the standard library from the Go toolchain you
already have. The target is one: `darwin/arm64`.

## Install

```sh
go install golang.design/x/nanogo/cmd/nanogo@latest
```

That gives you the command and nothing beside it, so it takes the standard
library from the Go toolchain on your machine.

A release tarball carries its own. Unpack it and run the binary inside it:

```console
$ tar xzf nanogo<version>.darwin-arm64.tar.gz
$ ./nanogo/bin/nanogo build .
nanogo: 1 of 28 packages compiled by nanogo; 27 by gc go1.27.0 (everything not named on the command line)
nanogo: the standard library and the runtime come from /path/to/nanogo (the tree the nanogo binary is installed in)
nanogo: the executable was written by go tool link; nanogo has no linker (specs/045-linker.md)
```

Every archive that build read came out of `nanogo/pkg/darwin_arm64`, named
from that tree's `MANIFEST` and checked against the SHA-256 in it. The tree
holds the 27 packages the smallest Go program needs, so a program that imports
anything outside that set is refused by name rather than served a copy from your
toolchain. `nanogo-dist tally` lists what the tree holds.

The `go` command must still be installed, and the build says why on every run:
`go list` resolves the packages you name, and `go tool link` writes the
executable. Its release has to be the one the tarball was built with, because
nanogo copies the object header from it.

## Compile a program

The examples below import `os`, which is outside the 27 packages a release
tarball carries, so they are the `go install` path: nanogo takes the standard
library from the toolchain on your machine. A tarball build is limited to what
its tree holds until nanogo compiles more of the standard library, and it says
which packages are missing when it is not.

```sh
mkdir hello && cd hello && go mod init hello
```

```go
// main.go
package main

import "os"

func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

func main() {
	os.Exit(fib(10))
}
```

```console
$ nanogo build .
nanogo: 1 of 51 packages compiled by nanogo; 50 by go1.27.0, which nanogo cannot compile yet
$ ./hello; echo $?
55
```

That number is real. nanogo compiled `fib` and `main` into arm64 instructions,
`go tool link` linked them against the Go runtime, and the process returned 55.

The line nanogo prints is the honest part. nanogo compiled 1 package. The Go
toolchain compiled the other 50, because a Go program needs a scheduler, an
allocator and a garbage collector before `main` runs, and nanogo compiles none
of those yet.

### Print something

`fmt.Println` does not work yet. The reason is below. To see output today, put
the printing in a package nanogo does not compile:

```sh
mkdir count && cd count && go mod init count
```

```go
// say/say.go
package say

import "syscall"

// Number writes n and a newline to standard output.
// The Go toolchain compiles this package, so it may use anything Go has.
func Number(n int) {
	syscall.Write(1, []byte{byte('0' + n/10), byte('0' + n%10), '\n'})
}
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
nanogo: 1 of 38 packages compiled by nanogo; 37 by go1.27.0, which nanogo cannot compile yet
$ ./count
10
```

nanogo compiled the variadic function, the range loop and the call. The
toolchain compiled `say` and the standard library under it.

## What nanogo compiles today

Integer arithmetic, comparisons, conversions between numeric types, and
indexing. Function calls, including recursive calls and variadic calls.

A call into a package the Go toolchain compiled also works, as `os.Exit` and
`say.Number` above show. A read of a package variable does not. The compiler
refuses it, and the variable would be nil anyway, because no package
initialization runs. So `os.Exit` works and `os.Stdout` does not.

Statements: `return`, assignment, `:=`, multi-value assignment such as
`a, b = b, a`, `if`, `for`, `switch` including the expressionless form,
`fallthrough`, `break`, `continue`, labels and `goto`.

Values: slice literals, `make([]int, n)`, `new(int)`, `len`, index and slice
expressions, `range` over a slice or an integer, and a struct type declared in
the package being compiled.

`nanogo help` prints the current list.

## What nanogo refuses

nanogo names the function, the position and the construct. It does not emit
code it cannot emit correctly:

```console
$ nanogo build .
nanogo: main: nanogo cannot compile function main at /tmp/hello/main.go:5:6: ir.Lower: ir: lowering main: append: no row of the lowering table is built for it yet
```

Refused today: `append`, `defer`, `panic`, a type assertion, a type switch,
`print` and `println`, every map operation, every channel operation, a
conversion to an interface, and a closure that captures a variable. A closure
that captures nothing and is called where it is declared compiles.

The interface conversion is why `fmt.Println` does not work. Every argument to
`fmt` converts to `any`:

```console
$ nanogo build .
nanogo: main: nanogo cannot compile function main at /tmp/hello/main.go:12:6: ssa.Build: ssa: main: convert: a conversion from int to interface is not built yet
```

A type descriptor is refused for a type that may carry methods, so
`make([]byte, n)` and `make([]point, n)` are refused while `make([]int, n)`
compiles.

One construct does not refuse cleanly. Floating-point arithmetic reaches the
arm64 encoder and panics there:

```console
$ nanogo build .
panic: arm64: ZR used where the encoding means a floating-point register
```

Use integers until that is finished.

## What nanogo does not do

**It does not compile the standard library.** Every package your program
imports was compiled by `gc`, out of a release tarball's `pkg` tree or out of
the Go toolchain on your machine. nanogo reads the export data `gc` wrote. The
count nanogo prints on every build is how much of the program that is.

**It does not link.** `go tool link` writes the executable. nanogo has no
linker yet. See [`specs/045-linker.md`](specs/045-linker.md).

**It does not run package initialization.** A program nanogo compiles starts
without initializing the packages it imports. So a package variable such as
`os.Stdout` is nil. Give the `say` package above a `fmt.Println` instead of a
`syscall.Write`, and the program crashes at run time with nothing from nanogo
in the traceback:

```console
$ ./count
panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x2 addr=0x0 pc=0x1008b3974]

goroutine 1 [running]:
os.(*File).write(...)
	.../os/file_posix.go:47
```

This is the one failure nanogo does not announce. Do not put a nanogo-compiled
package into a program you care about.

**It compiles one package at a time in a build.** The archive nanogo writes
does carry export data, and `gc` reads back all 275 of the 375 standard library
packages whose surface round-trips through it
([`specs/015-export-data.md`](specs/015-export-data.md)), so a package nanogo
compiled can be imported. What `nanogo build` does not do yet is order two of
its own targets, so it refuses a build in which one package you named imports
another. Name one, or build the other with the `go` command.

**It has one target.** nanogo emits arm64 machine code and refuses to run on a
host that is not arm64. It does not yet refuse a `GOARCH` that is not arm64. It
emits arm64 code and `go tool link` reports an unknown relocation.

## How it works

```
source -> scanner -> parser -> type checker -> typed IR -> SSA -> machine ops -> goobj
```

Two intermediate representations and no more. A typed tree that still speaks
Go, and an SSA graph that starts target-neutral and ends target-specific.
[`specs/002-architecture.md`](specs/002-architecture.md) has the pipeline and
the package layout.

Four decisions shape the rest:

- **The parser is written and the type checker is forked.** Rewriting Go's type
  checker is the largest correctness risk in the project, and it buys nothing
  that bootstrapping needs. The reference Go compiler carries a forked
  `go/types` of its own for the same reason.
  [`specs/012`](specs/012-type-checking.md)
- **The compiler emits object files, not assembly text.** Two experiments
  decided this, and both are in [`spikes/`](spikes/).
  [`specs/040`](specs/040-object-format.md)
- **The objects are compatible with `gc`.** That is why one package can be
  compiled by nanogo inside a build the Go toolchain otherwise owns, and why a
  failure has one suspect. [`specs/051`](specs/051-build-integration.md)
- **The size is a budget, not an adjective.** The compiler is meant to be read
  end to end, and [`specs/000`](specs/000-decisions.md) states the line budget,
  the accounting, and the two places to recover it if it runs out.

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
| [`rtsym`](rtsym/) | 100% | 70 runtime signatures checked against the runtime's own source |
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

What stops the rest is one list: a capturing closure, `defer`, `panic`,
`append`, a type assertion, a type switch, a conversion to an interface, and
every map and channel operation. Each is a row of that lowering table that no
pass performs, or the type descriptor those rows need.

So the back half of the compiler is real and the front of the middle end is not
finished. What stands between here and
[`specs/060`](specs/060-selfhost.md)'s fixed point is the language itself.

## Specs

[`specs/`](specs/) is the design deck. Start with
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
true on the day it was written and several had stopped being true.

## Spikes

[`spikes/`](spikes/) holds the experiments that settled the backend seam. Each
answers one question a spec depends on, and each still runs.

## License

BSD-3-Clause &copy; 2026 The [golang.design](https://golang.design) Initiative Authors
