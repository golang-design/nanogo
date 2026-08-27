# nanogo specs

Design specs, written before implementation. Each is bounded, states its
decisions with the reasoning behind them, and is honest about what it does not
resolve.

These are **internal design documents**: decisions, tradeoffs, and the reasoning
behind them, written for whoever is building or reviewing the compiler.

The decisions everything here is built on live in
[`000-decisions.md`](000-decisions.md). It is normative: a spec that contradicts
it is wrong.

## Reading the deck honestly

The deck was written before the code, and the code disproved a great deal of it.
Every spec now carries a `status` and the index below repeats it, so that a
reader knows before opening a spec whether it describes code or a plan:

| Status | Means |
| --- | --- |
| `complete` | the spec's scope is built and gated |
| `in progress` | part of it is built, and the spec says which part |
| `draft` | nothing in it is built |

43 specs, and the index below is the only place their statuses are counted, so
that no second copy can drift from it. A third of the deck is still `draft`,
which is to say still a plan, and now says so.

Where the code disproved a spec, the spec keeps the record: what it claimed,
what the code does, and how the difference was found. None of it is deleted
when it is superseded.

The convention is that a spec with several such records gathers them in a
closing section, so that the design reads top to bottom and the corrections
read as one list. A single correction stays where it matters, marked as one.
[003](003-sequencing.md) calls its closing section **Deviations** because it
records departures from a plan rather than from a design; the component specs
call theirs **What was wrong**.

Two audits in August 2026 went through the whole deck against the tree. The
first found stale claims in most of it. The second, after the driver was wired
end to end, found the reverse fault: specs describing as unbuilt work that had
landed. [003](003-sequencing.md)'s Deviations section carries the corrections
that cross spec boundaries.

The numbers in [003](003-sequencing.md) are gated by `internal/hygiene`, which
reads them out of the prose and fails when they disagree with what the tests
measure. A reworded sentence fails the gate rather than switching it off.

## Reading order

Start with [000](000-decisions.md). Its first section corrects the framing the
project began with, that a from-scratch type checker was required for
bootstrapping, and every other decision follows from that correction.

Then [001](001-bootstrap-gates.md), which splits "bootstrap" into three gates
that are reached at different times and need different work. Specs name their
gate in the frontmatter and the word is used precisely throughout.

Then [002](002-architecture.md) for the pipeline, and
[003](003-sequencing.md) for the order of work. [003](003-sequencing.md) is the
file to read before picking anything up, and it is not the order these files are
numbered in.

If you are here to evaluate rather than to build, read
[004](004-conformance.md) fourth. It is the argument for why any of this can be
believed.

## The deck

### Foundation

| | | | |
| --- | --- | --- | --- |
| [000](000-decisions.md) | Decisions | `complete` | normative; read first |
| [001](001-bootstrap-gates.md) | Bootstrap gates | `draft` | G1, G2, G3, and the fixed point |
| [002](002-architecture.md) | Architecture | `in progress` | pipeline, two IRs, package layout |
| [003](003-sequencing.md) | Sequencing | `in progress` | milestones M0 to M10, risks, deviations |
| [004](004-conformance.md) | Conformance | `in progress` | four levels of proof; L1 built for the front end, L2 for the single-file recipes, L3 partial, L4 blocked |
| [005](005-remaining-work.md) | Remaining work | `in progress` | the dependency graph of everything still refused, and the file that constrains it |

### Front end

| | | | |
| --- | --- | --- | --- |
| [010](010-scanner-and-positions.md) | Positions and scanner | `complete` | `Pos`, `//line`, semicolon insertion |
| [011](011-parser-and-ast.md) | Parser and syntax tree | `complete` | the two grammar ambiguities |
| [012](012-type-checking.md) | Type checking | `complete` | forking `types2` |
| [013](013-generics.md) | Generics | `draft` | full stenciling; the stenciler is not written |
| [014](014-package-loader.md) | Package loader | `in progress` | `go list` at G1, direct resolution at G2 |
| [015](015-export-data.md) | Export data | `in progress` | `gc`-compatible; both directions work, a generic declaration is refused |
| [016](016-directives-and-pragmas.md) | Directives and pragmas | `in progress` | the complete `//go:` table; fourteen verbs are recorded and no pass reads one |

### Middle end

| | | | |
| --- | --- | --- | --- |
| [020](020-ir.md) | Typed IR | `in progress` | the node set, and the lowering table with a state per row |
| [021](021-ssa-construction.md) | SSA construction | `in progress` | memory as a value, on-the-fly phis |
| [022](022-optimization-passes.md) | Optimization passes | `draft` | the pass list; no pass is written |
| [023](023-escape-analysis.md) | Escape analysis | `draft` | not written; every allocation goes to the heap, and an address-taken local does not, which is a miscompile |
| [024](024-inlining-and-devirtualization.md) | Inlining and devirtualization | `draft` | not written |
| [025](025-lowering-and-rules.md) | Lowering | `complete` | rewrite rules, the target boundary |
| [026](026-register-allocation.md) | Register allocation | `complete` | linear scan; no callee-saved registers |
| [027](027-liveness-and-stackmaps.md) | Liveness and stack maps | `in progress` | the contract with the collector |

### Runtime interface

| | | | |
| --- | --- | --- | --- |
| [030](030-abi.md) | ABI | `in progress` | layout and calling convention |
| [031](031-runtime-lowering.md) | Runtime lowering | `in progress` | the calls the compiler generates |
| [032](032-type-descriptors-and-itabs.md) | Type descriptors and itabs | `in progress` | and the symbol namespace; a defined type's descriptor reaches the object file with its `UncommonType` tail, itabs are written and called, and the two runtime caches an assertion to an interface and a type switch over interface cases read are written into a writable section with the Go type the collector needs; one spelling and four stops still refuse a descriptor by name |
| [033](033-closures-defer-panic.md) | Closures, defer, panic, recover | `in progress` | `defer` and `go` of a function value run, a captureless closure runs, and a deferred call runs while the goroutine panics; a `panic` whose operand is not already an interface value, a read of `recover`'s value, and every capture are refused; a `panic` of a non-nil interface value and a declared function used as a func value are miscompiles |
| [034](034-write-barriers.md) | Write barriers | `draft` | not written |
| [035](035-goroutines-and-stack-growth.md) | Goroutines and stack growth | `in progress` | stack growth built; `go f()` with no arguments reaches `newproc`, an argument is a capture and is refused |

### Back end

| | | | |
| --- | --- | --- | --- |
| [040](040-object-format.md) | Object files | `in progress` | `goobj`, and why not assembly text |
| [041](041-instruction-encoding.md) | Instruction encoding | `complete` | 998,947 encodings agree with `go tool asm` |
| [042](042-arm64-backend.md) | arm64 backend | `in progress` | the first target |
| [043](043-amd64-backend.md) | amd64 backend | `draft` | the test of target neutrality; not written |
| [044](044-plan9-assembler.md) | Plan 9 assembler | `draft` | G3; not written |
| [045](045-linker.md) | Linker | `draft` | G2; `pclntab` and `moduledata`; not written |
| [046](046-debug-info.md) | Debug information | `in progress` | tracebacks built, DWARF not |

### Driver

| | | | |
| --- | --- | --- | --- |
| [050](050-driver.md) | Driver | `in progress` | `gc`-compatible flags, and the pass list a compile runs |
| [051](051-build-integration.md) | Build integration | `in progress` | `-toolexec`, the allowlist, and the two modes |
| [052](052-diagnostics.md) | Diagnostics | `in progress` |  |
| [053](053-determinism.md) | Determinism | `in progress` | what the fixed point depends on |
| [054](054-distribution.md) | Distribution | `in progress` | the tarball, and what every archive in it records |

### Gates

| | | | |
| --- | --- | --- | --- |
| [060](060-selfhost.md) | G1 self-hosting | `draft` | not reached |
| [061](061-toolchain-independence.md) | G2 toolchain independence | `draft` | not reached |
| [062](062-distribution-build.md) | G3 compiling the distribution | `draft` | not reached |

### Extension

| | | | |
| --- | --- | --- | --- |
| [070](070-gpu-target.md) | GPU targets | `draft` | post-v1; a consumer, never a driver |

## Where the work stands

| | |
| --- | --- |
| Built and gated | The scanner, the parser, the forked type checker, the package loader's G1 half, the export data reader and writer, the typed IR and part of its lowering table, SSA construction and lowering, register allocation, liveness and stack maps, the ABI, the type descriptors an allocation needs and a defined type's, the object writer, the arm64 encoder, the driver, and `nanogo build` |
| Proved | A leaf package compiles under `go build -toolexec=nanogo`, links against `gc`-compiled code and the real runtime, and runs. A nanogo-compiled frame keeps an allocation alive across `runtime.GC()`, so a real collector reads the stack map and the pointer mask nanogo wrote. A deferred call runs while the goroutine is panicking on a runtime-raised panic, in the frame order `gc` produces |
| The largest gap | Three functions in five of the Go distribution get past SSA construction, and the unbuilt rows of [020](020-ir.md)'s lowering table block the other two. No SSA operation reads the context register, so every capture is refused. [020](020-ir.md) owns the table with a state per row, [021](021-ssa-construction.md) owns the pass, and [003](003-sequencing.md) carries the counts |
| Not started | Escape analysis, inlining, generics instantiation, itabs, the conversion to an interface that a `panic` of anything else and a read of `recover`'s value both need, write barriers, the linker, the assembler, DWARF, and the amd64 backend |
| Silently wrong | Three things compile and behave differently from `gc`'s build, and nothing is said at compile time. A `panic` of a non-nil interface value dies in the runtime with `name offset out of range` and prints no panic value ([033](033-closures-defer-panic.md)). A declared function passed or returned as a func value faults on the call ([033](033-closures-defer-panic.md)). A pointer to an address-taken local outlives its frame ([023](023-escape-analysis.md)) |
| Decided against | Assembly text as a build path, a from-scratch type checker, GC-shape stenciling with dictionaries |

## The spikes

Three, in [`../spikes`](../spikes), each cited by a normative decision.

[`stackmap`](../spikes/stackmap) shows that per-PC garbage collection stack maps
are expressible in text assembly: a hand-written `FUNCDATA` symbol with two
bitmaps, selected by `PCDATA`, produced the two collection outcomes it declared.

[`symbolnames`](../spikes/symbolnames) shows that the assembler rejects symbol
names containing a colon, and that the compiler's entire namespace, `type:`,
`go:itab.` and `go:string.`, uses one.

Together those two are why nanogo writes object files and not assembly
([000](000-decisions.md) decision 3).

[`toolexec`](../spikes/toolexec) shows that a foreign compiler can be substituted
per package by `go build -toolexec`, and measured what the `go` command actually
sends: a larger flag set than the help text lists, plus the `-V=full` build-ID
protocol. It is the evidence for [000](000-decisions.md) decision 10's mechanism,
and [050](050-driver.md) records the two flags it corrected.

## Conventions

Frontmatter is `title`, `status`, `layer`, `depends_on`, and, on every spec
outside the foundation group, `gate`. `status` is `draft`, `in progress`, or
`complete`, and `internal/hygiene` rejects a fourth value and a status that
disagrees with the index above. `gate` names which of [001](001-bootstrap-gates.md)'s
gates the spec serves; the foundation specs serve all of them and omit it.
