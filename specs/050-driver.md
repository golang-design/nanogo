---
title: "The driver: a gc-compatible command line"
status: in progress
layer: driver
gate: G1
depends_on:
  - 000-decisions.md
  - 014-package-loader.md
---

# Driver

`cmd/nanogo` is the executable, and it has two command lines.

The one this spec is mostly about is `go tool compile`'s, because
[051](051-build-integration.md) substitutes nanogo for `go tool compile`
through `-toolexec` and the `go` command constructs the arguments. This is not
a nicety. If the flags differ, the substitution does not work, and the bring-up
strategy of [000](000-decisions.md) decision 11 is gone.

The other is for a person.

## The two command lines

`run` tells them apart by the first argument. `-toolexec` splits its value into
words and appends an absolute tool path, so a real substitution's first
argument is a path and no tool is named `build`, `help` or `version`. A word
that is one of those three is a person typing, and cannot be the `go` command.

| Command line | Who sends it | What it does |
| --- | --- | --- |
| `nanogo <tool> [args]` | the `go` command, through `-toolexec` | compiles one package, or execs the real tool. The rest of this spec. |
| `nanogo build [-o out] [-v] [-work] [packages]` | a person | resolves the package graph itself, compiles what it can with the same `Compile`, and links. [051](051-build-integration.md) owns it under whole-world mode. |
| `nanogo help` | a person | what nanogo compiles today and what it refuses |
| `nanogo version` | a person | the pinned release and this build's identity |

`nanogo help` is not decoration. A driver whose limits are discoverable only by
hitting them wastes a reader's afternoon, and nanogo's limits are the subject:
the help text names the constructs it compiles and the constructs it refuses.

`-fallback` is nanogo's only driver flag, and it is accepted before the tool
path and inside the compile flag set, because the `go` command puts it in both
places.

## What is built

The command line is built. `driver` parses every flag in the tables below,
answers `-V=full`, reads `-importcfg`, and dispatches per invocation: a tool
other than `compile`, a package off the allowlist, or a command line it cannot
parse goes to `gc` ([051](051-build-integration.md)). `cmd/nanogo` is 34 lines
over `driver.Run`.

`driver.Compile` runs the pipeline, in the order below, and refuses by name
everything the compiler cannot do yet.

| Stage | Owner |
| --- | --- |
| parse, type check, build the typed tree | [011](011-parser-and-ast.md), [012](012-type-checking.md), [020](020-ir.md) |
| `ir.Lower`, which also collects the descriptors the tree names | [020](020-ir.md), [032](032-type-descriptors-and-itabs.md) |
| `ssa.Build`, lowering, decomposition, register allocation | [021](021-ssa-construction.md), [025](025-lowering-and-rules.md), [026](026-register-allocation.md) |
| `ssagen`, then the text symbols and the descriptors into the object | [041](041-instruction-encoding.md), [040](040-object-format.md) |

`ir.Lower` runs first because `ssa.Build` refuses every Go-specific node. An
earlier pass list started at `ssa.Build`, so the lowering pass never ran in a
real compile, and [032](032-type-descriptors-and-itabs.md) records what that
cost.

| Refused | Because |
| --- | --- |
| a host that is not `arm64` | there is one backend ([043](043-amd64-backend.md) is unbuilt) |
| `-+` | the runtime rules of [034](034-write-barriers.md) and [035](035-goroutines-and-stack-growth.md) are unbuilt |
| `-embedcfg` | see [`go:embed`](#goembed) below |
| a package with an assembly definition in `-symabis` | the ABI wrapper of [030](030-abi.md) is unbuilt |
| a package with a package-level variable, or an `init` | neither has a data symbol or an init task yet |
| a package with no function bodies | nanogo writes text symbols and the descriptors its code names, and nothing else |
| a type its code needs a descriptor for and `rtype` cannot fill in | [032](032-type-descriptors-and-itabs.md)'s method set gap |
| a function no pass in the list accepts | the pass names itself and the error names the function and its position |

Each refusal names the spec that owns the gap, because the allowlist is the
progress metric and a failure has to say which entry produced it.

**A package that imports another package is no longer refused.** `export/`
reads `gc`'s export data and writes it ([015](015-export-data.md)), so the
importer resolves a `gc`-compiled dependency and the archive nanogo produces
carries a `__.PKGDEF` that a `gc`-compiled importer can read.
`internal/e2e` compiles a package that imports one of its own and a package
that imports `math/bits` and `strconv`. What is still refused is a generic
declaration, which the writer names rather than encodes.

## Flags that must be honoured

The list below is **measured, not transcribed**.
[`spikes/toolexec`](../spikes/toolexec) logs what the `go` command actually sends
during a real build, which is a different set from what `go tool compile -h`
lists, because the help text says what the compiler accepts, not what the build
sends.

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
| `-trimpath list` | rewrite file paths in the output; see below | [053](053-determinism.md) |
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

**`-trimpath` is a list, and this spec wrote it as one rewrite.** The row said
`old=>new`. The `go` command joins several rewrites with `;`, and the last one
has an empty new side because it erases the build's temporary directory
(`cmd/go/internal/work.(*Action).trimpath`). A parser that requires a non-empty
new side rejects every real build. This was found by writing the parser against
a logged command line, and `driver/flags.go` and `driver.TrimPath` carry the
shape.

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
equal to the *tool's* name, literally `compile` and not `nanogo`, and field 1
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

   **This rule is stated and not enforced.** `driver.BuildIdentity` returns the
   string `unknown` when the build carries no VCS stamp, `VersionLine` reports
   `X:nanogo-unknown`, and `driver.Run` compiles anyway. That is exactly the
   constant identity the rule forbids, so an unstamped build reuses objects
   across nanogo changes for ever. This was found by reading `driver/version.go`
   and `driver.run` against the rule: nothing between them tests the identity.
   The rule stands; the gap is in the code and is the check `run` has to grow.

## Flags nanogo defines

| Flag | Meaning | State |
| --- | --- | --- |
| `-N` | disable optimizations, per [022](022-optimization-passes.md)'s governing rule | parsed, and there is no optimization to disable |
| `-l` | disable inlining | parsed, and there is no inliner |
| `-S` | print an assembly listing; the textual form of [041](041-instruction-encoding.md)'s output, not a build path | **parsed and ignored**; nothing is printed |
| `-m` | print optimization decisions, in `gc`'s format so that Go's own corpus can check them ([023](023-escape-analysis.md), [024](024-inlining-and-devirtualization.md)) | **parsed and ignored**; nothing is printed |
| `-d list` | debugging settings, including per-pass disable for [022](022-optimization-passes.md) | parsed, and no setting is read |
| `-fallback` | exec `gc` instead of compiling; the mechanism of [051](051-build-integration.md) | built, before the tool path and inside the compile flag set |

`gc` counts repetitions of `-N`, `-l`, `-S` and `-m`, so `driver.Config` counts
them too: `-l -l` is not `-l`. A greedy boolean flag would also eat the source
file after it, which is why the parser gives these their own kind.

`-fallback` is accepted in two places because the `go` command puts it in two
places. `-toolexec` splits its value into words and appends the tool path, so
the flag arrives before the tool; `-gcflags` puts it in the compile flag set,
after it.

## Rejecting rather than ignoring

An unrecognised flag, or a recognised flag whose feature nanogo does not
implement, is an **error**. It is never ignored.

The reason is specific to the substitution model. `go build -toolexec=nanogo`
passes whatever the `go` command decided, and a flag nanogo ignores produces a
binary that is subtly different from the one the build asked for, built without
race instrumentation that was requested, or without a trimmed path. A hard error
sends the package down the fallback path instead, where `gc` handles it
correctly.

That is what an unrecognised flag does: `ParseCompile` returns a `FlagError`,
`run` execs `gc`, and the package is built correctly by the other compiler.
`-dynlink`, `-race`, `-msan` and `-asan` are rejected by name so that the
message says why rather than "not recognised".

**The rule and the flag table above contradict each other, and the table is the
side that is wrong today.** `-S` and `-m` are recognised, counted into
`driver.Config`, and read by nothing, so a build that asked for a listing gets
silence and a zero exit status. `-N`, `-l`, `-d` and `-c` are in the same state,
but they differ in consequence: there is no optimizer, no inliner and no
concurrency, so ignoring them is not yet distinguishable from honouring them.
`-S` and `-m` promise output. Until [052](052-diagnostics.md) produces it, they
are the two flags this section forbids. This was found by auditing the flag
table against the uses of each `Config` field, which for these six is none.

## `go:embed`

`-embedcfg` maps embed patterns to files. The compiler reads the files and emits
their contents as data symbols bound to the package's variables.

This is a front-end feature with a back-end tail and it has no spec of its own
because it is small: parse the config, resolve patterns to the listed files, emit
a string or byte slice or `embed.FS` structure. It is listed here so it is not
forgotten, since a standard library package uses it and G3 needs it.

None of it is written. `checkSupported` refuses `-embedcfg` outright, which is
the honest state. nanogo emits one kind of data symbol, the type descriptors of
[032](032-type-descriptors-and-itabs.md), and nothing binds a data symbol to a
package-level variable, which is what an embedded file needs.

## Exit status and output

Errors to standard error, in the format of [052](052-diagnostics.md). Non-zero
exit on any error. Nothing on standard output unless a flag asked for it, since
the `go` command reads it.

## Testing

What runs today:

- `driver/flags_test.go` covers every flag in the tables, in both the `-f v` and
  the `-f=v` form, plus the rejections and the count semantics.
- `cmd/nanogo/main_test.go` drives a real `go build -toolexec=nanogo` over a
  two-package module and runs the program it produces. That is the `-V=full`
  protocol checked by the only reader that matters, because the `go` command
  parses the line itself and a malformed one is fatal to the build.
- The same file drives a build with a package on the allowlist and asserts the
  failure names the package.
- `internal/e2e` installs the binary the way a user installs it and drives six
  builds through it, from the command line to a process that returns the answer
  it computed. Nothing in it calls into nanogo's packages, because the claim is
  about the command line and a test that reached inside would not be testing
  that claim.
- `spikes/toolexec` runs in CI on every commit, which is what keeps the flag
  table measured rather than transcribed.

What is still owed:

- A drift test: run the spike's passthrough over the distribution and assert the
  observed flag set is a subset of the tables here. A new flag in a new Go
  release then fails a test rather than a build.
- `-N` and `-l` builds of the whole corpus, per
  [022](022-optimization-passes.md), which needs passes to disable first.
