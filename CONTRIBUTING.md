# Contributing

Thanks for looking. nanogo is at the design stage. The
[spec deck](specs/) is the artifact, three [spikes](spikes/) hold the
measurements that two decisions rest on, and the compiler is being built
against them in the order [`specs/003-sequencing.md`](specs/003-sequencing.md)
sets.

That means an argument against a decision is worth as much as a patch right
now, and patches are welcome.

## What is most useful right now

**Tell us where the design is wrong.** Especially if you have written a
compiler, or a linker, or have fought Go's object format.
[`specs/000-decisions.md`](specs/000-decisions.md) is the shortest path to
disagreeing with something concrete. Every spec ends with what it leaves open.

**Break a spike.** The spikes under [`spikes/`](spikes/) are the evidence for
[decision 3](specs/000-decisions.md) and
[decision 11](specs/000-decisions.md). If one of them measures the wrong thing,
a decision built on it is wrong too, and that is worth more than a bug report
about code that does not exist yet.

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
still printed on every run, so the debt is visible rather than forgotten. The
file is empty. Adding the first entry is a review conversation.

### Costs go in the docs

Every design choice states what it gives up, not only what it buys. This is
already the house style of the deck:
[decision 10](specs/000-decisions.md) records that v1 misses its own line
budget by five per cent rather than adjusting the budget, and
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
- Commit messages carry the reasoning, not only the content.

## Getting started

```sh
git clone https://github.com/golang-design/nanogo
cd nanogo
go build ./...
go test ./...
go test -race ./...
```

Requires Go 1.27 or later.

The spikes are separate modules, so no build at the repository root reaches
them. Run one from its own directory:

```sh
cd spikes/stackmap && go run . multi
```

Two of them are `darwin/arm64` assembly and do not build anywhere else, which
is [decision 9](specs/000-decisions.md): the host is the first target.

## How the repository is organised

| Path | What it is | Who it is for |
| --- | --- | --- |
| `specs/` | The design deck, and the normative decisions | Anyone building or reviewing nanogo |
| `spikes/` | Small programs that answer a question a spec depends on | Anyone doubting a decision |
| `internal/covercheck/` | The per-package coverage gate | Anyone whose package is below it |
| `.github/workflows/` | The gates, each with the reason it exists | Anyone whose build went red |

The compiler's own packages arrive in the order
[`specs/003-sequencing.md`](specs/003-sequencing.md) sets, and each one names
the spec it implements and the bootstrap gate it serves.

## Reporting a problem

For a design objection, quote the claim you disagree with and say what you
would do instead.

For a bug, send the smallest program that shows it, the target
(`darwin/arm64` or `linux/amd64`), and what `gc` does with the same input. That
last part is the whole story more often than not: nanogo is object-compatible
with `gc` by [decision 11](specs/000-decisions.md), so a difference from `gc`
is a nanogo bug until proven otherwise.

## License

Contributions are under [BSD-3-Clause](LICENSE), the same as the project.
