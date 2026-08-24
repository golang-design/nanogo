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

The list below is **measured, not transcribed**.
[`spikes/toolexec`](../spikes/toolexec) logs what the `go` command actually sends
during a real build, which is a different set from what `go tool compile -h`
lists — the help text says what the compiler accepts, not what the build sends.

### Sent on every `compile` invocation

Ignoring any of these produces a wrong build, silently.

| Flag | Meaning | Owner |
| --- | --- | --- |
| `-o file` | output file | [040](040-object-format.md) |
| `-p path` | the package's import path, and its symbol prefix | [032](032-type-descriptors-and-itabs.md) |
| `-importcfg file` | map from import path to export data file | [015](015-export-data.md) |
| `-lang version` | the language version the source expects | [012](012-type-checking.md) |
| `-buildid id` | recorded in the output | [053](053-determinism.md) |
| `-goversion string` | required runtime version; a mismatch is an error | [000](000-decisions.md) decision 11 |
| `-trimpath old=>new` | rewrite file paths in the output | [053](053-determinism.md) |
| `-c n` | concurrency | [053](053-determinism.md) |
| `-pack` | write an archive, not a bare object | [040](040-object-format.md) |
| `-nolocalimports` | relative import paths are an error | [014](014-package-loader.md) |
| `-shared` | generate position-independent code | below |

### Sent conditionally

| Flag | When | Owner |
| --- | --- | --- |
| `-complete` | the package has no assembly and no C, so a bodyless declaration is an error | [016](016-directives-and-pragmas.md) |
| `-symabis file` | the package has assembly | [030](030-abi.md) |
| `-asmhdr file` | the package has assembly | [030](030-abi.md) |
| `-std` | the package is in the standard library | |
| `-embedcfg file` | the package uses `go:embed` | below |
| `-+` | compiling the runtime | [034](034-write-barriers.md), [035](035-goroutines-and-stack-growth.md) |

### Rejected

| Flag | |
| --- | --- |
| `-dynlink` | dynamic linking is out of scope ([045](045-linker.md)) |
| `-race`, `-msan`, `-asan` | no instrumentation |

**`-shared` is not in the rejected list, and an earlier draft of this spec put it
there.** The spike shows it is sent on *every* invocation on `darwin/arm64`,
because the platform requires position-independent code. A compiler that rejects
it rejects every build on the first target. This is the clearest example of why
the table is measured.

`-symabis` and `-asmhdr` exist because a package with assembly is compiled in two
passes: the assembler runs first to discover which symbols it defines and with
which ABI, and the compiler is told, so it knows a bodyless declaration is
satisfied. nanogo must participate in that protocol from the first package that
has assembly.

`-+` turns on the runtime's extra checks: the write barrier restrictions of
[034](034-write-barriers.md) and the nosplit accounting of
[035](035-goroutines-and-stack-growth.md). It is the flag that makes G3 different
from G1.

## `-V=full`

Before compiling anything, the `go` command runs `<toolexec> compile -V=full` and
parses the output into the compiler's build ID, which becomes part of every cache
key. `cmd/go/internal/work/buildid.go` requires three or more fields, field 0
equal to the *tool's* name — literally `compile`, not `nanogo` — and field 1
equal to `version`. Malformed output is a fatal error with no fallback.

nanogo prints the pinned Go version and appends its own identity:

```
compile version go1.27.0 X:nanogo-<hash>
```

The appended part is what makes the `go` command's cache notice that the compiler
changed. Without it, a nanogo change would reuse objects built by the previous
nanogo, and [051](051-build-integration.md)'s claim that the cache is correct in
hosted mode would be false.

Two rules that the first draft of this spec left unstated, and that a build gets
wrong silently if they are not followed:

1. **Only `compile` is answered.** For `asm`, `link` and every other tool,
   nanogo execs the real tool and lets it answer for itself. Answering on their
   behalf would pin their cache keys to nanogo's `PinnedGoVersion`, so a host
   toolchain moving from go1.27.0 to go1.27.1 would silently reuse stale
   assembly objects. That is the failure this whole section exists to prevent,
   reintroduced one tool over.
2. **The identity must be real.** It is nanogo's own VCS revision, plus a
   dirty marker when the tree is modified. A build with no version stamp, from
   `-buildvcs=false` or from outside a repository, has no identity to report and
   nanogo **must refuse to compile** rather than report a constant. A constant
   identity is a cache that never invalidates, which is worse than no cache.
   Passing through to `gc` in that state is the correct behaviour.

## Flags nanogo defines

| Flag | Meaning |
| --- | --- |
| `-N` | disable optimizations, per [022](022-optimization-passes.md)'s governing rule |
| `-l` | disable inlining |
| `-S` | print an assembly listing; the textual form of [041](041-instruction-encoding.md)'s output, not a build path |
| `-m` | print optimization decisions, in `gc`'s format so that Go's own corpus can check them ([023](023-escape-analysis.md), [024](024-inlining-and-devirtualization.md)) |
| `-d list` | debugging settings, including per-pass disable for [022](022-optimization-passes.md) |
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

- Every flag in the tables above exercised through a real `go build -toolexec`
  invocation, not through direct calls. The spike's passthrough is the harness.
- A drift test: run the spike's passthrough over the distribution and assert the
  observed flag set is a subset of the tables here. A new flag in a new Go
  release then fails a test rather than a build.
- `-V=full` output parsed by the `go` command itself, which is the only check
  that matters for that protocol.
- Rejection tests for every unsupported flag.
- `-N` and `-l` builds of the whole corpus, per
  [022](022-optimization-passes.md).
