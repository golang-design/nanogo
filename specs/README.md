# nanogo specs

Design specs, written before implementation. Each is bounded, states its
decisions with the reasoning behind them, and is honest about what it does not
resolve.

These are **internal design documents**: decisions, tradeoffs, and the reasoning
behind them, written for whoever is building or reviewing the compiler.

The decisions everything here is built on live in
[`000-decisions.md`](000-decisions.md). It is normative: a spec that contradicts
it is wrong.

**Nothing is built.** Every spec in this directory is `draft`. The repository
holds the specs, two spikes, and a module identity. A spec being here means its
scope and current decisions are reviewable, not that its code exists.

## Reading order

Start with [000](000-decisions.md). Its first section corrects the framing the
project began with — that a from-scratch type checker was required for
bootstrapping — and every other decision follows from that correction.

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

| | | |
| --- | --- | --- |
| [000](000-decisions.md) | Decisions | normative; read first |
| [001](001-bootstrap-gates.md) | Bootstrap gates | G1, G2, G3, and the fixed point |
| [002](002-architecture.md) | Architecture | pipeline, two IRs, package layout |
| [003](003-sequencing.md) | Sequencing | milestones M0–M10, risks, deviations |
| [004](004-conformance.md) | Conformance | four levels of proof |

### Front end

| | | |
| --- | --- | --- |
| [010](010-scanner-and-positions.md) | Positions and scanner | `Pos`, `//line`, semicolon insertion |
| [011](011-parser-and-ast.md) | Parser and syntax tree | the two grammar ambiguities |
| [012](012-type-checking.md) | Type checking | forking `types2` |
| [013](013-generics.md) | Generics | full stenciling |
| [014](014-package-loader.md) | Package loader | `go list` at G1, direct resolution at G2 |
| [015](015-export-data.md) | Export data | `gc`-compatible |
| [016](016-directives-and-pragmas.md) | Directives and pragmas | the complete `//go:` table |

### Middle end

| | | |
| --- | --- | --- |
| [020](020-ir.md) | Typed IR | the exhaustive lowering table |
| [021](021-ssa-construction.md) | SSA construction | memory as a value, on-the-fly phis |
| [022](022-optimization-passes.md) | Optimization passes | the pass list, and the one exception |
| [023](023-escape-analysis.md) | Escape analysis | |
| [024](024-inlining-and-devirtualization.md) | Inlining and devirtualization | |
| [025](025-lowering-and-rules.md) | Lowering | rewrite rules, the target boundary |
| [026](026-register-allocation.md) | Register allocation | linear scan; no callee-saved registers |
| [027](027-liveness-and-stackmaps.md) | Liveness and stack maps | the contract with the collector |

### Runtime interface

| | | |
| --- | --- | --- |
| [030](030-abi.md) | ABI | layout and calling convention |
| [031](031-runtime-lowering.md) | Runtime lowering | the calls the compiler generates |
| [032](032-type-descriptors-and-itabs.md) | Type descriptors and itabs | and the symbol namespace |
| [033](033-closures-defer-panic.md) | Closures, defer, panic, recover | |
| [034](034-write-barriers.md) | Write barriers | |
| [035](035-goroutines-and-stack-growth.md) | Goroutines and stack growth | prologue, nosplit budget, preemption |

### Back end

| | | |
| --- | --- | --- |
| [040](040-object-format.md) | Object files | `goobj`, and why not assembly text |
| [041](041-instruction-encoding.md) | Instruction encoding | |
| [042](042-arm64-backend.md) | arm64 backend | the first target |
| [043](043-amd64-backend.md) | amd64 backend | the test of target neutrality |
| [044](044-plan9-assembler.md) | Plan 9 assembler | G3 |
| [045](045-linker.md) | Linker | G2; `pclntab` and `moduledata` |
| [046](046-debug-info.md) | Debug information | tracebacks first, DWARF second |

### Driver

| | | |
| --- | --- | --- |
| [050](050-driver.md) | Driver | `gc`-compatible flags |
| [051](051-build-integration.md) | Build integration | `-toolexec`, the allowlist |
| [052](052-diagnostics.md) | Diagnostics | |
| [053](053-determinism.md) | Determinism | what the fixed point depends on |

### Gates

| | | |
| --- | --- | --- |
| [060](060-selfhost.md) | G1 self-hosting | |
| [061](061-toolchain-independence.md) | G2 toolchain independence | |
| [062](062-distribution-build.md) | G3 compiling the distribution | |

### Extension

| | | |
| --- | --- | --- |
| [070](070-gpu-target.md) | GPU targets | post-v1; a consumer, never a driver |

## Where the work stands

| | |
| --- | --- |
| Done | The spec deck, and the two spikes in [`../spikes`](../spikes) that settle the backend seam |
| Next | M0 in [003](003-sequencing.md): skeleton, driver, package layout |
| Blocked on nothing | Every spec is drafted; none waits on another to begin |
| Decided against | Assembly text as a build path, a from-scratch type checker, GC-shape stenciling with dictionaries |

## The two spikes

Both are in [`../spikes`](../spikes) and both are cited by
[000](000-decisions.md) decision 3.

[`stackmap`](../spikes/stackmap) shows that per-PC garbage collection stack maps
are expressible in text assembly: a hand-written `FUNCDATA` symbol with two
bitmaps, selected by `PCDATA`, produced the two collection outcomes it declared.

[`symbolnames`](../spikes/symbolnames) shows that the assembler rejects symbol
names containing a colon, and that the compiler's entire namespace — `type:`,
`go:itab.`, `go:string.` — uses one.

Together they are why nanogo writes object files and not assembly.

## Conventions

Frontmatter is `title`, `status`, `layer`, `gate`, and `depends_on`. `status` is
`draft`, `in progress`, or `complete`. `gate` names which of
[001](001-bootstrap-gates.md)'s gates the spec serves.
