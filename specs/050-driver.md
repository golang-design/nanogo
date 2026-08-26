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
through `-toolexec` and the `go` command constructs the arguments. Every flag
has to match: if one differs, the substitution does not work, and the bring-up
strategy of [000](000-decisions.md) decision 10 is gone.

The other is for a person.

## The two command lines

`run` tells them apart by the first argument. `-toolexec` splits its value into
words and appends an absolute tool path, so a real substitution's first
argument is a path and no tool is named `build`, `help` or `version`. A word
that is one of those three is a person typing, and cannot be the `go` command.

| Command line | Who sends it | What it does |
| --- | --- | --- |
| `nanogo <tool> [args]` | the `go` command, through `-toolexec` | compiles one package, or execs the real tool. The rest of this spec. |
| `nanogo build [-o out] [-v] [-work] [packages]` | a person | resolves the package graph itself, compiles every package named with the same `Compile` or fails naming it, takes the rest of the graph from the toolchain, and links. [051](051-build-integration.md) owns it as the user's path. |
| `nanogo help` | a person | what nanogo compiles today and what it refuses |
| `nanogo version` | a person | the pinned release and this build's identity |

`nanogo help` states the limits before the features, because nanogo compiles a
small part of Go and a user who finds that out by hitting an error has already
spent the afternoon. Every construct it names is one that was compiled and run,
and each is backed by a probe program that re-runs it. Prose does not re-run, so
a claim in that text with no probe behind it is a claim that will be wrong
within a month.

`-fallback` is the only flag nanogo adds to a `compile` command line, and it is
accepted before the tool path and inside the compile flag set, because the `go`
command puts it in both places. It governs the `-toolexec` path only. So does
`NANOGO_ALLOWLIST`: `nanogo build` reaches `RunBuild` before either is read, and
a package it names and cannot compile is an error rather than a package handed
to `gc`.

## What is built

The command line is built. `driver` parses every flag in the tables below,
answers `-V=full`, reads `-importcfg`, and dispatches per invocation: a tool
other than `compile`, a package off the allowlist, or a command line it cannot
parse goes to `gc` ([051](051-build-integration.md)). `cmd/nanogo` is 45 lines
over `driver.Run`, 13 of them code, so every claim in this spec is testable in
process.

`driver.Compile` runs the pipeline, in the order below, and refuses by name
everything the compiler cannot do yet.

| Stage | Owner |
| --- | --- |
| parse, type check, build the typed tree | [011](011-parser-and-ast.md), [012](012-type-checking.md), [020](020-ir.md) |
| `ir.Lower`, which also collects the descriptors the tree names | [020](020-ir.md), [032](032-type-descriptors-and-itabs.md) |
| `ssa.Build`, then decomposition | [021](021-ssa-construction.md) |
| the ABI assignment, then lowering to arm64 operations, with the verifier run after each | [030](030-abi.md), [025](025-lowering-and-rules.md), [021](021-ssa-construction.md) |
| register allocation | [026](026-register-allocation.md) |
| `ssagen`, then the text symbols and the descriptors into the object | [041](041-instruction-encoding.md), [040](040-object-format.md) |

`ir.Lower` runs first because `ssa.Build` refuses every Go-specific node.

| Refused | Because |
| --- | --- |
| a build whose target `GOARCH` is not `arm64` | there is one backend ([043](043-amd64-backend.md) is unbuilt). `checkTarget` reads the target and not the host, so `GOARCH=amd64` on an arm64 host is refused by name rather than mis-compiled |
| `-+` | the runtime rules of [034](034-write-barriers.md) and [035](035-goroutines-and-stack-growth.md) are unbuilt |
| `-embedcfg` | see [`go:embed`](#goembed) below |
| a package with an assembly definition in `-symabis` | the ABI wrapper of [030](030-abi.md) is unbuilt |
| a package-level variable whose type holds a pointer and whose descriptor `rtype` cannot build | the collector reads the pointer map of a data symbol through its type descriptor ([032](032-type-descriptors-and-itabs.md)) |
| a type its code needs a descriptor for and `rtype` cannot fill in | one of [032](032-type-descriptors-and-itabs.md)'s four stops, which the refusal names |
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
| `-goversion string` | required runtime version; a mismatch is an error | [000](000-decisions.md) decision 10 |
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

**`-shared` is accepted and not rejected.** The spike shows it is sent on
*every* invocation on `darwin/arm64`, because the platform requires
position-independent code. A compiler that rejects it rejects every build on the
first target.

**`-trimpath` takes a list and not one rewrite.** The `go` command joins several
rewrites with `;`, and the last one has an empty new side because it erases the
build's temporary directory (`cmd/go/internal/work.(*Action).trimpath`). A
parser that requires a non-empty new side rejects every real build.
`driver/flags.go` and `driver.TrimPath` carry the shape.

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

Two rules, each of which a build gets wrong silently when it is not followed:

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
   across nanogo changes for ever. The rule stands; the gap is in the code, and
   closing it is a check `run` has to grow.

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

**Six flags break the rule today, and the rule is the side that is right.**
`-N`, `-l`, `-S`, `-m`, `-d` and `-c` are recognised and parsed into
`driver.Config`, and no code reads those six fields. `-N`, `-l`, `-d` and `-c`
are harmless for now: there is no optimizer, no inliner and no concurrency, so
ignoring them is not distinguishable from honouring them. `-S` and `-m` promise
output, so a build that asks for a listing gets silence and a zero exit status.
Until [052](052-diagnostics.md) produces that output, they are the two flags
this section forbids.

## `go:embed`

`-embedcfg` maps embed patterns to files. The compiler reads the files and emits
their contents as data symbols bound to the package's variables.

This is a front-end feature with a back-end tail and it has no spec of its own
because it is small: parse the config, resolve patterns to the listed files, emit
a string or byte slice or `embed.FS` structure. It is listed here so it is not
forgotten, since a standard library package uses it and G3 needs it.

None of it is written. `checkSupported` refuses `-embedcfg`, which is the state
to keep until the front end exists. The data symbol an embedded file needs is
not the missing piece: `ssagen.AddGlobals` binds a data symbol to a
package-level variable and `ssagen`'s string constants write read-only bytes.
What is missing is the front end, which reads the config, resolves the patterns
and builds the `embed.FS` structure. `checkSupported`'s message still says the
data emitter is what is missing, and that message is the part to correct next.

**The refusal covers one of the two paths, and the other is a miscompile.**
`checkSupported` reads a compile command line, so it fires when the `go`
command sends `-embedcfg` under `-toolexec` ([051](051-build-integration.md)):

```
nanogo: main: nanogo cannot compile a package that uses go:embed:
-embedcfg needs the data emitter of specs/050-driver.md, which is unbuilt
```

`nanogo build` builds its own compile command line and never puts `-embedcfg`
on it, so there is nothing for `checkSupported` to see. The package compiles,
links and runs, and every embedded variable is its zero value: a program that
prints `len(data)` for a 14-byte file prints 14 under `gc` and 0 here. That is
the worse failure of the two and it is the one a user meets first, because
`nanogo build` is the user's command. The fix is not a second refusal beside
`checkSupported`. It is in `driver/build.go`, which has to notice a
`//go:embed` directive on the package it is about to compile, and the directive
is one [016](016-directives-and-pragmas.md) does not record.

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
- `internal/e2e` installs the binary the way a user installs it and drives real
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

## What was wrong

**`nanogo help` named `defer`, `println` and an empty function body as
refused**, for weeks after each of them started working. That is why the help
text is backed by probe programs rather than by this spec.

**`-shared` was in the rejected table.** The spike measured it on every
invocation on `darwin/arm64`, so rejecting it would have rejected every build on
the first target. It is the clearest reason the flag tables are measured against
a logged command line rather than transcribed from `go tool compile -h`.

**`-trimpath` was written as one `old=>new` rewrite.** The parser was then
written against a logged command line, which carries several rewrites joined by
`;` with an empty new side on the last.

**The pass list started at `ssa.Build`.** `ir.Lower` never ran in a real
compile; [032](032-type-descriptors-and-itabs.md) records what that cost.

**The architecture check read `runtime.GOARCH`.** That is the host's
architecture, so `GOARCH=amd64` on an arm64 host passed the check and nanogo
emitted arm64 code for an amd64 build. `go tool link` then reported an unknown
relocation, which names neither the cause nor the fix. `checkTarget` now reads
the target.

**The `-V=full` section stated neither rule above.** Answering for `asm` and
`link` as well as `compile`, and reporting a constant identity, are both silent
cache faults, and neither was written down until a reader asked what the section
required.

**The flag table and the rejection rule were audited against each other**, by
reading each `Config` field against its uses. Six fields have none, which is how
the `-S` and `-m` violation above was found.

**The `-V=full` identity gap was found the same way**, by reading
`driver/version.go` and `driver.run` against rule 2: nothing between them tests
the identity.

**The pass table listed lowering before decomposition and omitted the ABI
assignment and the two verifier runs.** `driver.compileFunc` holds the real
list, and it names each pass in the error it produces, so a spec that misorders
it misreads every refusal message.
