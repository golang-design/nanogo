# Contributing

Thanks for looking. nanogo compiles Go source to a `goobj` file that links and
runs. A leaf package compiles under `go build -toolexec=nanogo`, links against
`gc`-compiled code and the real Go runtime, and runs.

The [spec deck](specs/) is the design record, and three [spikes](spikes/) hold
the measurements two decisions rest on. Work arrives in the order
[`specs/003-sequencing.md`](specs/003-sequencing.md) sets, and the deck is
corrected when the code disproves it.

Both an argument against a decision and a patch are welcome, and the argument
is not the cheaper contribution.

## Getting started

```sh
git clone https://github.com/golang-design/nanogo
cd nanogo
go build ./...
go test ./...
```

Requires Go 1.27 or later. `go test ./...` runs in a few minutes and is the
gate that must pass before you send anything.

The spikes are separate modules, so no build at the repository root reaches
them. Run one from its own directory:

```sh
cd spikes/stackmap && go run . multi
```

Two of them are `darwin/arm64` assembly and build nowhere else, which is
[decision 9](specs/000-decisions.md): the host is the first target.

## The gates

Eight jobs run in CI, and each one is runnable locally.

| Job | Command | What it protects |
| --- | --- | --- |
| `test` | `go test ./...` and `go test -race ./...`, on linux and macos | correctness |
| `go test corpus` | `go test ./internal/gotest/` on an arm64 runner | that nanogo still compiles what it compiled, against Go's own test corpus |
| `cross-compile` | `GOOS=... GOARCH=... go build ./... && go vet ./...` | that the compiler builds for every host, whatever it emits |
| `gofmt` | `gofmt -l .`, spikes included | style |
| `coverage` | see below | that no package is carried by another |
| `determinism` | `go test ./...` twice, byte for byte | [`specs/053`](specs/053-determinism.md), because G1 is a byte-identical fixed point |
| `spikes` | each spike from its own directory | that the evidence a decision rests on still runs |
| `vanity import path` | the module path in `go.mod` | that `golang.design/x/nanogo` still resolves |

The numbers in the documentation are gated inside `test`, by
`go test ./internal/hygiene/`.

### Coverage is gated per package at 90%

Never as a repository average. An average lets a well-tested package carry an
untested one and reports a number nobody can act on: "the repository is at 91%"
does not name the package to test next.

```sh
go test -coverprofile=cover.out -coverpkg=./... ./...
go run ./internal/covercheck -profile=cover.out
```

A package can be excluded in
[`internal/covercheck/exclusions.txt`](internal/covercheck/exclusions.txt), and
an entry needs a written reason on the same line. The tool rejects one without.
An exclusion is a debt with a name on it, and the package's real number is
still printed on every run, so the debt is visible rather than forgotten.

Four packages are excluded today: `cmd/nanogo`, whose single statement is a
process boundary no coverage tool can see, and `types2`, `types2/errors` and
`types2/gen`, which are the forked checker and its generator. Each entry names
the gate that replaces coverage. Adding a fifth is a review conversation.

### The numbers in the documentation are gated too

`README.md` and several specs quote counts the tests produce: how many files
agree with `go/scanner`, how many functions get past SSA construction, what
each package's coverage is. Those numbers were all true when they were written
and several had stopped being true before anybody noticed.

[`internal/hygiene/`](internal/hygiene/) holds the gate. It reads the numbers
out of the prose and compares them with
[`internal/hygiene/testdata/facts.json`](internal/hygiene/testdata/facts.json),
a checked-in record of what the tests measured. That comparison is fast and
runs on every `go test`.

If you reword a sentence the gate reads, the gate fails saying so rather than
switching itself off. That is deliberate: a check a prose edit can silently
disable protects nothing. Do not delete a claim to quiet the gate. If a number
no longer belongs, remove it from the gate's list in the same change.

### The capability claims are not gated, so they are probed

The gate above reads numbers. It cannot see a sentence that says nanogo
refuses `defer`, and in August 2026 that sentence was in `README.md`, `doc.go`
and `nanogo help` for weeks after `defer` started working. The same audit
found `println`, an empty function body, strings, package-level variables,
package initialization and export data all documented as impossible while all
six worked.

[`internal/audit/testdata/probes`](internal/audit/testdata/probes) is the
answer. One directory per Go construct, one `main` package each, and `gc` as
the oracle: the same program is compiled by both and the two runs are
compared, so no probe carries an expected value that can itself go stale.

```sh
NG=$(pwd)/nanogo sh internal/audit/testdata/probes/run.sh
```

Each line reads `REFUSED` with the message, `OK` when nanogo agreed with gc,
or `WRONG` when it did not. `WRONG` is the row that matters: a program nanogo
compiled into something that behaves differently is worse than one it refused.

When you lift a refusal, or find a construct nobody has probed, add the probe
in the same change and correct the sentence that described the old behaviour.
Three documents state capabilities and all three must agree with the corpus:
`README.md`, `doc.go`, and `driver/help.go`, whose text `driver/help_test.go`
pins phrase by phrase.

No gate enforces this. A gate over prose was tried and removed, because it
failed contributors on wording rather than on truth. The corpus is evidence a
reviewer can run in seconds, which is the part that was missing.

## Environment variables the tests read

Every corpus test skips when its corpus is not on the machine, so a plain
`go test ./...` is green on a laptop with no Go source tree beside it. A test
that silently compares nothing is worse than one that is absent, so CI sets
these to turn each skip into a failure.

| Variable | Effect |
| --- | --- |
| `NANOGO_REQUIRE_CORPUS=1` | A corpus test must find its corpus. A missing `GOROOT` source tree, or a missing `go tool asm`, becomes a failure instead of a skip. Also re-derives the facts file and fails when it has drifted. |
| `NANOGO_REQUIRE_LINK=1` | A test that links nanogo's output and runs it must run. nanogo emits arm64, so CI sets this on the arm64 runner only, and elsewhere those tests skip. |
| `NANOGO_REFRESH_FACTS=1` | Writes `facts.json` from the measurements instead of comparing against it. |

Refresh the facts after a change that moves a number:

```sh
NANOGO_REFRESH_FACTS=1 go test -timeout 60m -run TestTheFactsAreCurrent ./internal/hygiene/
```

Then fix whatever document quoted the old number. The corpus takes minutes, so
run it when you have moved one:

```sh
NANOGO_REQUIRE_CORPUS=1 go test -run Corpus -v ./ssa/ ./ir/ ./loader/ ./syntax/
```

## What is most useful right now

**A row of the lowering table.**
[`specs/020`](specs/020-ir.md) lists every Go construct with what it becomes and
whether it is built. About half the table is built, and each unbuilt row blocks
a measured number of standard library functions. The count per cause, largest
first, is what this prints:

```sh
NANOGO_REQUIRE_CORPUS=1 go test -run TestLowerCorpus -v ./ir/
```

Work the list from the top. [`specs/021`](specs/021-ssa-construction.md) owns
the guard that refuses whatever the table has not lowered.

**Tell us where the design is wrong.** Especially if you have written a
compiler, or a linker, or have fought Go's object format.
[`specs/000-decisions.md`](specs/000-decisions.md) is the shortest path to
disagreeing with something concrete. Every spec ends with what it leaves open.

**Break a spike.** The spikes under [`spikes/`](spikes/) are the evidence for
[decision 3](specs/000-decisions.md) and
[decision 11](specs/000-decisions.md). If one of them measures the wrong thing,
a decision built on it is wrong too.

**Prior art.** If another compiler solved one of the open questions well,
saying so saves us finding out slowly.

**Code.** The shape of the compiler is still moving, so for anything larger
than a fix, open an issue with what you have in mind first. The answer tells
you whether a spec is about to move the ground under it.

## Ground rules

### The specs are normative, and decision 000 wins

[`specs/000-decisions.md`](specs/000-decisions.md) is the top of the tree. A
spec that contradicts it is wrong, and the fix is to change the spec or to
change decision 000 with the reasoning written down. Every other spec inherits
from it.

Code that contradicts a spec is the same problem one level down. If you change
behaviour a spec describes, change the spec in the same commit. A spec that no
longer matches the code is worse than no spec, because a reader trusts it.

### A bug fix arrives with the program that found it

The program that reproduced the bug becomes a test. It fails before your fix
and passes after it, and you say so in the commit message.

This is not a formality for a compiler. A miscompile is found by one input out
of a very large set, and that input is the only cheap evidence that the bug is
gone. A fix with no test is a claim.

Find the root cause. A compiler bug that is patched where it was noticed moves
to the next pass, and the second sighting looks like a new bug.

### Widening what the compiler accepts means widening what the tests run

A compiler's end-to-end tests are written in the language the compiler accepts,
so a construct it refuses is a construct its gates cannot exercise. That is not
a theory. Construction dropped every `for` statement's post statement for
months, and no test caught it, because construction also refused the assignment
statement a counted loop needs, so no program in the repository had one.

When you lift a refusal, add a program to [`internal/e2e`](internal/e2e/) that
uses it. That package installs the binary the way a user installs it, runs a
real `go build -toolexec=nanogo`, and runs the program that comes out.

### Costs go in the docs

Every design choice states what it gives up, not only what it buys. This is
already the house style of the deck:
[decision 10](specs/000-decisions.md) records what the compiler measures against
its own line budget, with the rows where the estimate was wrong, and
[decision 11](specs/000-decisions.md) lists what object compatibility with `gc`
costs before it lists what it buys.

Reviewers will ask for the cost sentence. A spec that reads as pure gain is a
spec whose author has not found the cost yet.

### Determinism: no map is ranged over on a path that produces output

[`specs/053-determinism.md`](specs/053-determinism.md) is the rule and
[decision 7](specs/000-decisions.md) is why. Go randomises map iteration order,
so any output derived from ranging over a map differs between runs. Collect the
keys, sort them, and range the sorted slice. Or use a structure that keeps
insertion order.

The rule is strict because G1 is defined as a byte-identical fixed point.
Non-determinism does not weaken that gate, it removes it, and it removes it in
the most expensive way: the fixed point fails, and the first suspicion falls on
code generation. The same rule bans ordering by pointer value, merging
concurrent results in completion order, and letting the working directory, the
time, or the environment reach the output.

### Style

- `gofmt`, and CI checks it, including the spikes.
- Comments explain **why**, not what. The what is in the code under the
  comment. The why is the part a reader cannot reconstruct.
- No em dashes in prose. Commas, colons, and parentheses do the work.
- Short sentences, active voice, exact technical terms.
- Prefer a table to a paragraph when the content is a table.
- Commit messages carry the reasoning, not only the content.

## How the repository is organised

| Path | What it is | Spec |
| --- | --- | --- |
| `syntax/` | Positions, scanner, parser, syntax tree | 010, 011 |
| `types2/` | The forked type checker, and the generator that ports it | 012, 013 |
| `loader/` | Build constraints, the import graph, `go list` | 014 |
| `export/`, `export/pkgbits/` | The reader for `gc`'s export data | 015 |
| `ir/` | The typed tree, its type layout, and the lowering pass | 020 |
| `ssa/` | SSA construction, lowering, decomposition, ABI, registers, liveness | 021, 025, 026, 027, 030 |
| `ssa/rules/` | The lowering rules of each target | 025, 042 |
| `rtsym/` | The runtime symbols the compiler calls | 031 |
| `rtype/` | The type descriptor encoder | 032 |
| `obj/`, `obj/arm64/` | The `goobj` writer and the instruction encoder | 040, 041 |
| `ssagen/` | SSA to machine code, prologues, relocations, stack maps | 027, 035, 041 |
| `driver/`, `cmd/nanogo/` | `gc`-compatible flags, `-toolexec` dispatch, the pass list | 050, 051 |
| `specs/` | The design deck, and the normative decisions | read 000 first |
| `spikes/` | Small programs that answer a question a spec depends on | evidence for 000 |
| `dist/`, `cmd/nanogo-dist/` | The distribution tree and its tarball | 054, 062 |
| `internal/e2e/` | Real `go build -toolexec=nanogo` builds, run end to end | 051 |
| `internal/gotest/` | Go's own test corpus, swept against nanogo with gc as the oracle | 004 |
| `internal/audit/` | Probe programs that say what the compiler accepts | |
| `internal/covercheck/` | The per-package coverage gate | |
| `internal/hygiene/` | Checks over the repository's own source and documentation | |
| `internal/release/` | The release build and its checks | 054 |
| `.github/workflows/` | The gates, each with the reason it exists | |

Each package names the spec it implements and the bootstrap gate it serves in
its doc comment.

## Reporting a problem

For a design objection, quote the claim you disagree with and say what you
would do instead.

For a bug, send the smallest program that shows it and what `gc` does with the
same input. That last part is the whole story more often than not: nanogo is
object-compatible with `gc` by [decision 11](specs/000-decisions.md), so a
difference from `gc` is a nanogo bug until proven otherwise. The target is
`darwin/arm64`; nanogo emits arm64 machine code and refuses a build for any
other `GOARCH` by name.

A program that nanogo compiles and that then behaves differently from `gc` is
the most valuable report there is, and it belongs in
[`internal/audit/testdata/probes`](internal/audit/testdata/probes) as well as
in the issue.

## License

Contributions are under [BSD-3-Clause](LICENSE), the same as the project.
