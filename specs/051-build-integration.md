---
title: "Build integration: bringing the compiler up one package at a time"
status: in progress
layer: driver
gate: G1
depends_on:
  - 050-driver.md
  - 000-decisions.md
---

# Build integration

The mechanism that makes [000](000-decisions.md) decision 11 pay: a build in
which nanogo compiles some packages and `gc` compiles the rest, producing one
working binary.

Without it, the first program that runs requires the front end, the middle end,
the backend, the runtime interface, and the object format all to be correct
simultaneously, and a crash has no suspect.

## What is built

Two build paths are built and gated, and they are for different people.

`nanogo build` is the user's, and it needs no `go` command in the loop. It is
the section further down.

The substitution is the compiler developer's. `driver.Run` implements the
flowchart below, `cmd/nanogo/main_test.go` drives a real
`go build -toolexec=nanogo` over a two-package module and runs the result, and
a second test puts a package on the allowlist and asserts the build stops with
that package named.

The payload is no longer empty. `driver.Compile` runs the pipeline of
[002](002-architecture.md) and writes the archive `-pack` asks for, and
`internal/e2e` runs a real `go build -toolexec=nanogo ./...` over a module whose
`main` package is on the allowlist. nanogo compiles it, `gc` compiles the
standard library beneath it, the real linker joins them, and the program runs.
[003](003-sequencing.md) lists the programs that go that route and what each
one proves. One of them divides by zero, and the traceback the runtime prints
names both nanogo-compiled functions with the file and the line each one is on.
Another makes a variadic call under `runtime.GC()`, so the object it allocated
is scanned with the pointer mask nanogo emitted.

What that build compiles is small, and the size is set above the driver.
`driver.Compile` compiles what SSA construction accepts once
[020](020-ir.md)'s lowering pass has run, which is three functions in five of
the Go distribution ([003](003-sequencing.md) counts them), and refuses the
rest by name. A package that imports is no
longer refused: `export/` reads `gc`'s export data, and `internal/e2e` compiles
a package that imports one of its own and a package that imports `math/bits`
and `strconv`. No allowlist file is committed, so a checkout still compiles
nothing until a build names one.

## `-toolexec`

```
go build -toolexec=nanogo ./...
```

The `go` command runs `nanogo <tool> <args...>` in place of each toolchain
invocation. [`spikes/toolexec`](../spikes/toolexec) measures this on a real
build: 110 invocations for a two-package module, 59 of them `compile`, and a
passthrough that logs and execs produces a working binary. Substitution is per
invocation, which is what makes the allowlist below possible.

The spike also produced the flag set [050](050-driver.md) tabulates, and the
`-V=full` build-ID protocol that the caching claim below depends on. nanogo inspects the tool and the arguments and decides:

```mermaid
flowchart TD
  inv["nanogo compile -p pkg ... files"]
  q1{"tool is<br/>compile?"}
  q0{"-V=full?"}
  ver["print the build ID line"]
  q2{"package in<br/>the allowlist?"}
  q3{"flags all<br/>supported?"}
  own["compile with nanogo"]
  fall["exec the real tool"]

  inv --> q1
  q1 -->|no| fall
  q1 -->|yes| q0
  q0 -->|yes| ver
  q0 -->|no| q2
  q2 -->|no| fall
  q2 -->|yes| q3
  q3 -->|no| fall
  q3 -->|yes| own
```

The version query comes before flag parsing because `-V=full` is not a compile
flag, and package selection comes before it because a package nanogo does not
own must reach `gc` even when a flag nanogo has never seen appears later on the
line. `driver.scanCompile` therefore reads `-p` and `-fallback` and validates
nothing else.

Everything that is not a `compile` invocation, the assembler, the linker,
`cgo` and `pack`, is passed through to the real tool.

## The allowlist

A file listing the packages nanogo compiles, one import path per line, `#` for a
comment. It starts with one package and grows.

The file is named by the environment variable `NANOGO_ALLOWLIST` and is not in
the repository. The spec said "a file, in the repository", and the variable is
what the driver reads. The reason is that a build selects its own list: a
regression hunt wants one package, CI wants the whole list, and a bisection
wants a list that is not a commit. Two rules follow from the variable being the
interface, and `driver/allowlist.go` states both:

- An unset variable is an empty list, so nanogo compiles nothing and every
  package reaches `gc`. That is the safe state, and it is the state a checkout
  is in today.
- A variable that names a file nanogo cannot read is an **error**, not an empty
  list. A mistyped path would otherwise turn nanogo off silently and for ever,
  which reads exactly like a passing build.

This is the project's actual progress metric, and it is better than a milestone
list because it is mechanical:

| Measure | Read from |
| --- | --- |
| How much of Go nanogo compiles | the allowlist's length |
| Whether it is correct | the tests of those packages, passing |
| What is next | the smallest package not on the list |

That metric reads zero today. Nothing stops a package being listed, and a listed
package that nanogo cannot compile fails by name, which is the mechanism working
as designed. What is missing is a package worth listing: the refusals in
[050](050-driver.md) catch imports, package-level variables and `init`
functions, and a real leaf package has at least one of the three. Until then the
honest measures are the corpus counts in
[004](004-conformance.md): 536 packages of the distribution reach the IR builder
and 17,905 functions reach SSA construction. Neither is a package compiled end to
end, and the allowlist is what will say when one is.

The list is ordered by dependency depth, so early entries are leaves with no
imports and no assembly. `runtime` is last, and reaching it is G3.

### `-p main` is not a package name

The `go` command sends `-p main` for **every** main package, in every module.
This was found by building with the driver in place, not by reading the flags.

So an allowlist of import paths cannot name one main package without claiming
all of them. The rule is therefore: **`main` is never matched by an allowlist
entry.** A main package is compiled by `gc` until nanogo can compile every main
package, which is a later and separate switch.

The cost is that the program's own top-level package is the last thing nanogo
compiles rather than a convenient early target. The alternative, matching on the
output path or the source directory, makes the allowlist depend on the build's
temporary directory layout, which is not something to build a gate on.

**The rule is not enforced, and enforcing it today would leave nanogo with
nothing to compile.** `Allowlist.Has` matches `main` like any other entry, so a
list with `main` on it claims every main package in every module. That was read
out of `driver/allowlist.go` against the rule above, and the first reading of it
was that the check belongs in `Has`.

Building the compiler said the opposite. When that was written `main` was the
only package a whole `go build` could hand to nanogo, and the reason was
[015](015-export-data.md) rather than this rule. Both directions of that spec
were unbuilt, and a main package is the one package in a build that neither
imports nor is imported, so it was the only one both directions left alone.
Enforcing the rule in `Has` would have turned that into zero compiled packages.

**That reason is gone.** [015](015-export-data.md) reads and writes export
data, so a package with imports compiles and a package nanogo compiled can be
imported. The rule is now what it always should have been: a constraint on the
**repository's** allowlist file, where `main` would claim every main package in
the Go corpus, and not a mechanism in the driver. Putting the check in `Has` is
worth reconsidering, because a non-main package is a usable first entry now.

The driver's protection is separate and already in place: a package it owns and
cannot compile is an error that names the package, never a silent hand back to
`gc`, so a `main` entry that claimed a main package nanogo cannot compile stops
the build loudly.

## Why this is better than a self-contained test suite

A package's own tests are adversarial, maintained by someone else, and cover
constructs a compiler author would not think to test. Compiling `strconv` with
nanogo and running `strconv`'s tests is a stronger statement than any corpus
nanogo could write, and it costs nothing to obtain.

It also localises blame precisely. If the binary works with package $P$ compiled
by nanogo and fails when $Q$ is added, the bug is in what $Q$ uses.

## `nanogo build`, which is the user's path

`-toolexec` is the compiler developer's path. It substitutes nanogo inside
someone else's build, it needs an allowlist, and everything above is about
making that substitution safe.

A user types `nanogo build .`, and there is no allowlist, no environment
variable and no `go` command in the loop. nanogo resolves the package graph
itself ([014](014-package-loader.md)), takes the standard library from the tree
the binary is installed in ([054](054-distribution.md)), compiles what it can,
delegates the rest, and links.

| | `-toolexec` | `nanogo build` |
| --- | --- | --- |
| Who decides which packages nanogo compiles | the allowlist file, per invocation | nanogo, per package, by whether it compiles |
| Who resolves the graph | the `go` command | `driver/build.go` |
| Where the standard library comes from | the ambient toolchain | `NANOGOROOT`, or the tree beside the binary |
| Who computes `-p` | the `go` command | nanogo |
| Cache | the `go` command's | none |

The last two rows change what the `-p main` rule above binds. nanogo computes
the package path itself here, so a main package is not the ambiguous `-p main`
the `go` command sends, and the rule that `main` is never matched by an
allowlist entry has nothing to constrain on this path.

Every build reports how many packages nanogo compiled and how many the
toolchain did. That count is the defence against the failure
[054](054-distribution.md) names: delegation is the fallback, so a build that
succeeds proves nothing on its own about who compiled what.

## Whole-world mode

The third mode, and the one neither of the two above is: nanogo compiles every
package including the runtime, for G2 and G3
([000](000-decisions.md) decision 11).

It uses the same object format, the same export data, and the same ABI. The
difference is only who drives.

**The orchestrator is built and the mode is not.** `nanogo build` is that
orchestrator, and every package it cannot compile it hands to `gc`. Whole-world
mode is the same command with nothing left to hand over, which needs the
runtime's rules ([034](034-write-barriers.md),
[035](035-goroutines-and-stack-growth.md)), the assembler
([044](044-plan9-assembler.md)) and the linker ([045](045-linker.md)). The
counter `nanogo build` prints is the distance: today it reports zero packages
of the bootstrap closure compiled by nanogo, and [054](054-distribution.md)
records the same figure for a released tree.

## Caching

nanogo does not implement a build cache. In hosted mode the `go` command's cache
applies and is correct **provided nanogo answers `-V=full` with its own
identity**, per [050](050-driver.md). The `go` command derives the compiler's
build ID from that one line and mixes it into every cache key, so a nanogo change
invalidates the affected packages and switching a package between `gc` and nanogo
invalidates it too.

An implementation that echoed the real compiler's version string would build once
and then silently reuse stale objects, which is the failure this paragraph exists
to prevent.

`driver.VersionLine` does answer with nanogo's own VCS revision, so the claim
holds for a stamped build. It does not hold for an unstamped one, where the
identity is the constant `unknown`; [050](050-driver.md) records that gap under
`-V=full`.

In whole-world mode, rebuilds are from scratch. [053](053-determinism.md) makes a
cache possible later; nothing depends on it existing.

## Testing

What runs today:

- `internal/e2e` installs the binary and drives real builds through it, each
  one a module nobody wrote for a harness, and runs the program that comes out.
  That is the mechanism and the payload together, which is the pairing the four
  unwired passes of [032](032-type-descriptors-and-itabs.md) got past.
- `cmd/nanogo/main_test.go` builds the real binary and runs
  `go build -toolexec=nanogo ./...` over a module, then runs the program. The
  passthrough path is therefore gated end to end.
- The same file runs a build with `NANOGO_ALLOWLIST` naming a package and
  asserts the build fails with the package named. That is the selection wiring
  proved in the real binary rather than in process.
- `driver/driver_test.go` covers the branches of the flowchart in process, and
  `driver/allowlist_test.go` covers the file format and the unset and unreadable
  cases.
- `spikes/toolexec` runs in CI and asserts the `go` command still sends
  `compile` invocations and still asks for `-V=full`.

What is still owed, and what the allowlist is for:

- The allowlist itself as the test suite. CI compiles every listed package with
  nanogo and runs that package's own tests. There is no such CI job, because
  there is no package to list.
- A regression gate: a package may not be removed from the allowlist to make CI
  pass. Removal requires a recorded reason, in the same manner as
  [003](003-sequencing.md)'s deviations.
- Both modes build nanogo itself, from M6 onward.
