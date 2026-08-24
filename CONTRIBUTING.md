# Contributing

Thanks for looking. nanogo compiles Go source to a `goobj` file that links and
runs, and it accepts about two functions in five of the standard library. The
[spec deck](specs/) is the design record and three [spikes](spikes/) hold the
measurements that two decisions rest on. The compiler is built against the deck
in the order [`specs/003-sequencing.md`](specs/003-sequencing.md) sets, and the
deck is corrected when the code disproves it.

Both an argument against a decision and a patch are welcome, and the argument is
not the cheaper contribution.

## What is most useful right now

**The forms SSA construction refuses.** The README lists them with the number
of standard library functions each one blocks. A composite literal blocks 5,379
of them, `len` blocks 2,680 and `range` blocks 1,527, and every one of those is
a row of [`specs/020`](specs/020-ir.md)'s lowering table that no pass performs.
[`specs/021`](specs/021-ssa-construction.md) owns the pass and
[`specs/020`](specs/020-ir.md) owns the table.

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

README.md and [`specs/003-sequencing.md`](specs/003-sequencing.md) quote counts
the tests produce: how many files agree with `go/scanner`, how many functions
reach SSA, what each package's coverage is. Those numbers were all true when
they were written and several had stopped being true before anybody noticed.

[`internal/hygiene/`](internal/hygiene/) holds the gate. It reads the numbers
out of the prose and compares them with
[`internal/hygiene/testdata/facts.json`](internal/hygiene/testdata/facts.json),
which is a checked-in record of what the tests measured. That comparison is
fast and runs on every `go test`.

The facts file itself is re-derived where the corpus already runs, under
`NANOGO_REQUIRE_CORPUS=1`, so a stale facts file fails CI. Regenerate it after
a change that moves a number:

```sh
NANOGO_REFRESH_FACTS=1 go test -timeout 60m -run TestTheFactsAreCurrent ./internal/hygiene/
```

Then fix whatever document quoted the old number. If you reword a sentence the
gate reads, the gate fails saying so rather than switching itself off, which is
deliberate: a check that a prose edit can silently disable protects nothing.

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

| Path | What it is | Spec |
| --- | --- | --- |
| `syntax/` | Positions, scanner, parser, syntax tree | 010, 011 |
| `types2/` | The forked type checker, and the generator that ports it | 012, 013 |
| `loader/` | Build constraints, the import graph, `go list` | 014 |
| `ir/` | The typed tree, and the type layout the backend reads | 020 |
| `ssa/` | SSA construction, lowering, decomposition, ABI, registers, liveness | 021, 025, 026, 027, 030 |
| `ssa/rules/` | The lowering rules of each target | 025, 042 |
| `rtsym/` | The runtime symbols the compiler calls | 031, 032 |
| `obj/`, `obj/arm64/` | The `goobj` writer and the instruction encoder | 040, 041 |
| `ssagen/` | SSA to machine code, prologues, relocations, stack maps | 027, 035, 041 |
| `driver/`, `cmd/nanogo/` | `gc`-compatible flags, `-toolexec` dispatch | 050, 051 |
| `specs/` | The design deck, and the normative decisions | read 000 first |
| `spikes/` | Small programs that answer a question a spec depends on | evidence for 000 |
| `internal/covercheck/` | The per-package coverage gate | |
| `internal/hygiene/` | Checks over the repository's own source and documentation | |
| `.github/workflows/` | The gates, each with the reason it exists | |

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
