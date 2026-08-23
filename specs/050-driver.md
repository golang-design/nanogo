---
title: "The driver: a gc-compatible command line"
status: draft
layer: driver
gate: G1
depends_on:
  - 000-decisions.md
  - 014-package-loader.md
---

# Driver

`cmd/nanogo` is the executable. Its command line is `go tool compile`'s, because
[051](051-build-integration.md) substitutes it for `go tool compile` through
`-toolexec` and the `go` command constructs the arguments.

This is not a nicety. If the flags differ, the substitution does not work, and
the bring-up strategy of [000](000-decisions.md) decision 11 is gone.

## Flags that must be honoured

These are constructed by the `go` command on every build. Ignoring one produces a
wrong build, silently.

| Flag | Meaning | Owner |
| --- | --- | --- |
| `-o file` | output object file | [040](040-object-format.md) |
| `-p path` | the package's import path, and its symbol prefix | [032](032-type-descriptors-and-itabs.md) |
| `-importcfg file` | map from import path to export data file | [015](015-export-data.md) |
| `-lang version` | the language version the source expects | [012](012-type-checking.md) |
| `-complete` | the package has no assembly or C, so bodyless declarations are an error | [016](016-directives-and-pragmas.md) |
| `-symabis file` | ABIs of symbols defined in assembly | [030](030-abi.md) |
| `-asmhdr file` | write struct offsets for assembly to include | [030](030-abi.md) |
| `-buildid id` | recorded in the object | [053](053-determinism.md) |
| `-goversion string` | required runtime version; mismatch is an error | [000](000-decisions.md) decision 11 |
| `-trimpath prefix` | rewrite file paths in the output | [053](053-determinism.md) |
| `-embedcfg file` | `go:embed` file mapping | below |
| `-shared`, `-dynlink` | not supported; reject with a clear message | [045](045-linker.md) |
| `-race`, `-msan`, `-asan` | not supported; reject | |
| `-+` | compiling the runtime | [034](034-write-barriers.md), [035](035-goroutines-and-stack-growth.md) |

`-symabis` and `-asmhdr` exist because a package with assembly is compiled in two
passes: the assembler is run first to discover which symbols it defines and with
which ABI, and the compiler is told, so it knows a bodyless declaration is
satisfied. nanogo must participate in that protocol from the first package that
has assembly.

`-+` turns on the runtime's extra checks: the write barrier restrictions of
[034](034-write-barriers.md) and the nosplit accounting of
[035](035-goroutines-and-stack-growth.md). It is the flag that makes G3 different
from G1.

## Flags nanogo defines

| Flag | Meaning |
| --- | --- |
| `-N` | disable optimizations, per [022](022-optimization-passes.md)'s governing rule |
| `-l` | disable inlining |
| `-S` | print an assembly listing; the textual form of [041](041-instruction-encoding.md)'s output, not a build path |
| `-m` | print optimization decisions, in `gc`'s format so that Go's own corpus can check them ([023](023-escape-analysis.md), [024](024-inlining-and-devirtualization.md)) |
| `-d list` | debugging settings, including per-pass disable for [022](022-optimization-passes.md) |
| `-c n` | concurrency, per [053](053-determinism.md) |
| `-fallback` | exec `gc` instead of compiling; the mechanism of [051](051-build-integration.md) |

## Rejecting rather than ignoring

An unrecognised flag, or a recognised flag whose feature nanogo does not
implement, is an **error**. It is never ignored.

The reason is specific to the substitution model. `go build -toolexec=nanogo`
passes whatever the `go` command decided, and a flag nanogo ignores produces a
binary that is subtly different from the one the build asked for — built without
race instrumentation that was requested, or without a trimmed path. A hard error
sends the package down the fallback path instead, where `gc` handles it
correctly.

## `go:embed`

`-embedcfg` maps embed patterns to files. The compiler reads the files and emits
their contents as data symbols bound to the package's variables.

This is a front-end feature with a back-end tail and it has no spec of its own
because it is small: parse the config, resolve patterns to the listed files, emit
a string or byte slice or `embed.FS` structure. It is listed here so it is not
forgotten, since a standard library package uses it and G3 needs it.

## Exit status and output

Errors to standard error, in the format of [052](052-diagnostics.md). Non-zero
exit on any error. Nothing on standard output unless a flag asked for it, since
the `go` command reads it.

## Testing

- Every flag in the first table exercised through a real `go build -toolexec`
  invocation, not through direct calls.
- Rejection tests for every unsupported flag.
- `-N` and `-l` builds of the whole corpus, per
  [022](022-optimization-passes.md).
