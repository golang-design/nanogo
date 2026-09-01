---
title: "Escape analysis"
status: draft
layer: middle end
gate: "required at G1 for a package on objabi.runtimePkgs, required everywhere at G3"
depends_on:
  - 020-ir.md
  - 022-optimization-passes.md
---

# Escape analysis

**Stages 1 to 3 are built and they decide one thing, what the export data
tells `gc`.** `escape/` answers two questions per parameter: does the value the
caller passed flow anywhere at all, and does it flow to one of the function's
own results without a dereference on the way. `export/writer.go` writes the
answer as the note gc reads. Nothing else in this spec is built: there is no
flow graph, no summary across a call, no `-m` output, and **no consumer inside
nanogo**. Every allocation still goes to the heap and every address-taken
variable still goes to a cell, so what is built changes what `gc` may do with
nanogo's archives and changes nothing about the code nanogo emits.

`ir/` holds a builder, a type layout, a node set, a converter, the lowering
pass of [020](020-ir.md) and the descriptor naming of
[032](032-type-descriptors-and-itabs.md), and no analysis over any of them.
What decides nanogo's own lowering is still `ir.Object.Escapes`, which carries
the interim answer rather than the analysis's: `ir.Build` sets it where the
**source** takes a variable's address and where a literal captures one, and
`ir/lower.go` moves every variable it names into a heap cell.

The safe answer is taken by default at every site that allocates, and that is
what makes the absence tolerable: the compiler allocates more than it needs to.
Five sites act on the absence, and they do not all do the same thing:

| Site | What it does without this pass |
| --- | --- |
| `ir/lower.go`, allocation | Every allocation goes to the heap. The heap is correct and slower; the frame is sometimes correct and otherwise corrupts memory |
| `ir/lower.go`, composite literals | Decides frame or heap by shape, not by this pass: a struct or array in an expression position is copied out by its reader and is built in the frame, and anything handing out a pointer to its elements goes to the heap |
| `ssa/build.go` | Cannot put a string concatenation buffer in the frame, because it does not know which concatenation escapes, so the buffer is nil and the runtime allocates |
| `ir/build.go` | Keeps a composite literal as one node, because a literal already scattered into element assignments is a decision this pass can no longer make |
| `ir/closure.go`, a variable | Every variable whose address the source takes, and every variable a literal captures, goes to a heap cell. `gc` moves only the ones whose address reaches a result, a global or a call; every other cell here is an allocation `gc` does not make |

The second row is the shape of answer this pass replaces: a rule sound for every
program, taken where a per-allocation judgement is not available. Two rows of
[020](020-ir.md)'s lowering table, the closure and the composite literal, name
this spec as the thing that decides them.

**A local whose address outlives its frame used to be the site with no safe
default, and it was a miscompile.** `func f() *int { n := 1; return &n }`
compiled, `n` stayed in `f`'s frame, and the returned pointer was into a frame
that is gone. Nothing said so at compile time or at run time. It read correctly
for as long as nothing overwrote that memory, which is what made it worse than
a crash: a short program printed `1` and agreed with `gc`. A `runtime.GC()`
between the call and the read printed a word of whatever the collector left
there, against `12345` from the same source under `gc`.

The last row of the table is that site, and it now takes the sound rule this
spec already stated: **a variable whose address is taken goes to the heap.**
It is blunter than the rule above, which asked whether the address reaches a
result, a global or a call. Answering that question is the analysis, so the
question is not asked: the address alone decides. That is more heap than `gc`
uses and it is correct, which is the trade every other row in this table
already makes.

**Which mark the rule reads, and why it is not `Addrtaken`.** `ir/lower.go`
takes the address of its own temporaries, and a temporary whose address the
compiler took lives exactly as long as the frame. `Addrtaken` holds both kinds
of mark and cannot tell them apart, so reading it would put those temporaries
in the heap and would make a second run of the lowering pass find work the
first run created. `Object.Escapes` is set by `ir.Build` and by nothing after
it, so it names the addresses the program wrote.

**One cell per execution of the declaration.** The cell is allocated where the
variable is declared and not at the function's entry, which is the Go 1.22 loop
rule: a variable declared in a loop body is a fresh variable on every
iteration, so `for i := range s { gp = &i }` hands out a different pointer each
time round. `ir/closure.go`'s `placeCells` is where that is done, and a
parameter's and a named result's cell is at the entry because the signature is
where those are declared.

## What the note decides in a mixed build

`export/writer.go` writes one escape analysis note per receiver and parameter,
and until stage 1 every one of them was the empty string. **That was not a
missing note and `gc` does not reject it.** It is the short encoding of the
most pessimistic answer there is, and reading it as anything else has sent one
investigation down the wrong path already.

`cmd/compile/internal/escape`'s `leaks` is eight bytes, one per destination,
each holding a minimum dereference count offset by one. `Encode` has exactly
one special case:

```go
func (l leaks) Encode() string {
	if l.Heap() == 0 {
		// Space optimization: empty string encodes more
		// efficiently in export data.
		return ""
	}
	...
```

`parseLeaks` is the other half and agrees:

```go
func parseLeaks(s string) leaks {
	var l leaks
	if !strings.HasPrefix(s, "esc:") {
		l.AddHeap(0)
		return l
	}
	copy(l[:], s[4:])
	return l
}
```

The empty note round-trips as "leaks to the heap at zero dereferences", which
is the top of the lattice. **`gc` has no encoding that means "I do not know",
and it needs none: not knowing and leaking to the heap are the same claim
about a caller's obligations, so the common one got the shortest encoding.**
The empty note was therefore the only sound note available without an
analysis, and stage 1 is what replaces it where the value flows nowhere.

### The cost is a refusal and not a slower program

For a package outside the runtime the note costs allocation. For a package
inside it the note is fatal, because `gc` compiles those under a rule that
forbids a heap escape outright:

```go
if loc.hasAttr(attrEscapes) {
	if n.Op() == ir.ONAME {
		if base.Flag.CompilingRuntime {
			base.ErrorfAt(n.Pos(), 0, "%v escapes to heap, not allowed in runtime", n)
		}
```

`base.Flag.CompilingRuntime` is set by `-+`, which `cmd/compile`'s flag parser
also derives from `-std` and `objabi.LookupPkgSpecial(Ctxt.Pkgpath).Runtime`.
`cmd/internal/objabi`'s `runtimePkgs` is that list, it holds 24 packages, and
**17 of the 23 standard library packages nanogo compiles in
[060](060-selfhost.md)'s bootstrap closure are on it.** So the configuration
[051](051-build-integration.md) exists for, nanogo owning the closure and `gc`
owning `runtime`, stops the first time a package on that list hands the address
of a local to a function nanogo compiled.

Measured on `go1.27.0` `darwin/arm64` with nanogo owning all 24 compile
invocations it can, the first failure is `internal/runtime/maps`, at three
sites of the shape `m.Delete(typ, abi.NoEscape(unsafe.Pointer(&key)))`:

```
runtime_fast32.go:479:57: key escapes to heap, not allowed in runtime
runtime_fast64.go:544:57: key escapes to heap, not allowed in runtime
runtime_faststr.go:409:58: key escapes to heap, not allowed in runtime
```

### What `abi.NoEscape` actually is, and what stage said so

The first reading of these three sites was wrong about the note, and the error
was in the source rather than in the measurement. This spec said
`abi.NoEscape` is `func(p unsafe.Pointer) unsafe.Pointer { return p }` and that
its note is a flow to result 0, `esc:\x00\x00\x00\x01`. That is not the body in
`go1.27.0`:

```go
//go:nosplit
//go:nocheckptr
func NoEscape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}
```

**`gc`'s note for it is `esc:`, the value flows nowhere.** That was read out of
`gc`'s own archive, and a pair of fixtures isolates the mechanism to one
boundary:

| Body | `gc`'s note |
| --- | --- |
| `x := uintptr(p); return unsafe.Pointer(x ^ 0)` | `esc:` |
| `return unsafe.Pointer(uintptr(p) ^ 0)` | `esc:\x00\x00\x00\x01` |
| `u := uintptr(unsafe.Pointer(p)); g = unsafe.Pointer(u)` | `esc:` |
| `g = unsafe.Pointer(uintptr(unsafe.Pointer(p)))` | empty |

The two halves of each pair differ only in whether the `uintptr` passes through
a variable. `cmd/compile/internal/escape`'s `unsafeValue` is the reason: it
follows a `uintptr` back through conversions and arithmetic and stops at
anything else, and a variable is anything else.

So **stage 3 never could have freed those three sites.** Stage 3 adds flows;
`esc:` has none. What frees them is the rule in the section below, which is
stage 2's, and the correction is recorded here rather than quietly fixed
because the wrong body sent the staging table's "what it unblocks" column down
the wrong row.

### Two independent gaps stack at `abi.NoEscape`, and only one is this spec's

A note is read only for a call that survives as a call. `tagHole` in
`cmd/compile/internal/escape/call.go` is the only reader of `param.Note`, and
it is reached from the walk over a call's arguments, so an inlined body is
analysed on its own and its callee's notes are never consulted. `gc` compiles
those three sites against its own archives without a word and it inlines
`abi.NoEscape` at all three.

That the inlining is what makes the difference was checked on a pair rather
than inferred from the pair of counts. Two copies of `NoEscape`'s body were put
in one package, one carrying `//go:nosplit` and `//go:nocheckptr` and one
carrying nothing, and a consumer compiled with `-+` called both. nanogo offered
the body of the second, `gc` inlined it, and that call did not fail. nanogo
offered no body for the first, the call survived, and it failed.

nanogo offers no body for `abi.NoEscape`. `driver/export.go`'s
`exportableDecl` withholds the body of any declaration carrying a directive
the driver records, because the writer carries no directive at all
([016](016-directives-and-pragmas.md)), and `NoEscape` carries `//go:nosplit`
and `//go:nocheckptr`. That refusal is right on its own terms: a body offered
without its pragma is a body `gc` would inline where the source forbade it.

So those three sites are held by two gaps and closing either one frees them.
**The blocker this spec owns is the general one and survives either fix.** Any
nanogo-compiled function `gc` does not inline, with a pointer-bearing
parameter, called by a package on the runtime list with the address of a
local, fails when nanogo proved nothing about that parameter.

## Stages 1 to 3: the note, and only the note

`escape/` is the pass and `escape.Params` is its entry point. It answers two
questions per receiver and parameter, **does the value the caller passed flow
anywhere at all**, and **does it flow to one of this function's own results
without a dereference on the way**. `esc:` says it flows nowhere. `esc:` with a
result byte says it flows to that result and to nothing else. Everything else
takes the empty note, which `gc` reads as a flow to the heap at zero
dereferences and which is what nanogo wrote for every parameter before.

The branch order is `cmd/compile/internal/escape.(*batch).paramTag`'s, so the
two can be read side by side, and the encoding in `escape/leaks.go` is a
transcription of `cmd/compile/internal/escape/leaks.go` rather than an
equivalent. The bytes were checked against `gc`'s own archives and not against
`gc`'s source: `cmd/nanogo/escapenote_test.go`'s `TestEscapeNoteMatchesGC`
compiles each case with the toolchain, decodes the export data's string
section, and asserts the note `gc` wrote.

### The five notes the pass writes

| Case | Note | Why it is sound |
| --- | --- | --- |
| A parameter whose type has no pointer word | empty | `gc` writes the same, and no scalar can carry a reference for a caller to worry about |
| A parameter that is unnamed or `_` | `esc:` | The body cannot name it, so nothing can flow out of it. `gc`'s own branch |
| A bodyless declaration carrying `//go:noescape` | `esc:\x00\x01\x01`, a mutator flow and a callee flow at 0 | The pragma is an assertion by the author, and `gc` honours it without proof for the same reason. The bytes are `gc`'s own and are **not** `esc:`, which claims more than the pragma does |
| A parameter the walk proved flows nowhere | `esc:` | The section below |
| A parameter the walk measured into result *i* at zero dereferences, and nowhere else | `esc:` with byte $3+i$ set to 1 | The section below, and the depth rule under it |

### Why the dereference count is only ever zero

`gc`'s note holds a minimum dereference count per destination, so it can say
"leaks to result 0 at one dereference", and `tagHole` reads that count back as
`ks[i].shift(x)`. A larger `x` describes a **weaker** flow, so a count above
the truth is the unsound direction and a count below it is the safe one.

The walk counts dereferences and it only counts them upwards. Every operation
adds one wherever it may read through a pointer, and an operation whose depth
is not obvious adds one rather than none, so the count it holds is never below
the true one. That leaves it unusable for writing a deep flow, because a count
above `gc`'s own would be a claim `gc` did not make, and the note ratchet is
that nanogo's note is `gc`'s or the empty one. **So a flow the walk measured at
any depth but zero is refused, and stage 4 is what makes the deeper counts
exact enough to write.**

The depths the walk assigns, and the reading of `gc` that each was checked
against:

| Shape | Depth | `gc`'s note for it returned |
| --- | --- | --- |
| `p` | 0 | `esc:\x00\x00\x00\x01` |
| `t.P`, `t` a struct by value | 0 | `esc:\x00\x00\x00\x01` |
| `a[i]`, `a` an array | 0 | `esc:\x00\x00\x00\x01` |
| `b[1:]`, `b` a slice or a string | 0 | `esc:\x00\x00\x00\x01` |
| `&b[i]`, `&q.f` | 0 | `esc:\x00\x00\x00\x01` |
| `T{P: p}` for a struct or an array | 0 | `esc:\x00\x00\x00\x01` |
| `i.(*T)`, and a type switch clause variable of pointer shape | 0 | `esc:\x00\x00\x00\x01` |
| `*q`, `t.P` through a pointer, `s[i]` on a slice | 1 and refused | `esc:\x00\x00\x00\x02` |
| a range element, `i.(T)` for a value type | 1 and refused | `esc:\x00\x00\x00\x02` |

Everything else takes the empty note, including every parameter of a function
carrying `//go:uintptrescapes` or `//go:uintptrkeepalive`. `gc` keeps analysing
the parameters of such a function that are not `uintptr`; nanogo refuses to
compile the function at all (`driver.LifetimeDirective`), so the answer that
costs nothing is the one that cannot be wrong.

### What the walk proves, and the shape that makes it safe

The walk is a taint set over one function's body for one parameter. The set
starts as the parameter itself and grows by whole variables: an assignment
whose source can carry a reference derived from the set taints the variable
its destination names. It iterates to a fixed point, so a flow that runs
backwards round a loop is seen.

**Every switch in the walk is an allowlist and the default case is a leak.**
An operation the pass does not name may store, mutate through, or call
anything it is given, so a tainted variable anywhere below one refuses the
parameter. Adding an operation is what makes the analysis see more; forgetting
one costs an allocation. The inverse shape, a list of sinks with everything
else assumed harmless, makes a forgotten operation a wrong answer, and a wrong
answer here lands in a caller `gc` compiled.

A leak is recorded at five kinds of position, and each is a claim in `gc`'s
own lattice that the notes here deny:

| Position | `gc`'s flow |
| --- | --- |
| A value derived from the parameter is assigned to a global, or through a pointer, a slice element or a map | heap |
| The destination of an assignment is reached through a value derived from the parameter | mutator |
| It is an argument of a call, or the value being called | heap, callee |
| It is converted to an interface | heap |
| It reaches a result at a depth other than zero, or a result past the fifth | result, at a count the pass cannot write |

A conversion to an interface is refused **because of nanogo and not because of
the source**: nanogo boxes on the heap, since the pass that would decide
otherwise is this one, so the value is in the heap whether or not the interface
goes anywhere. A slice literal and a map literal are refused for the same
reason and it is the same sentence: `ir/lower.go` allocates both, so an element
of one is in the heap whatever this pass decides. A struct literal and an array
literal are built in the frame, so they are not refused.

Reading is not a leak, and it is what makes the pass find anything at all.
`b[0]`, `*p`, `t.f`, `len(b)` and `b[1:]` are reads, and a read of a value
whose type has no pointer word cannot carry a reference onwards, which is what
stops `uint64(b[0])` from tainting the integer it produces. Ranging is a read,
and so is a type switch and a type assertion: each hands the value out and
retains nothing of the operand.

### The word the collector does not trace, and why its default is inverted

A conversion of a pointer to `uintptr` **ends the flow**, and a conversion back
picks it up again only inside the same expression. That is the one switch in
the package whose default ends a flow where every other default refuses the
parameter, and the inversion is the language's rule rather than this pass's
judgement. `unsafe`'s rule (3) allows a pointer to travel through a `uintptr`
only when both conversions and the arithmetic between them stand in one
expression, and it names the other form invalid in its own words:

```go
// INVALID: uintptr cannot be stored in variable
//  before conversion back to Pointer.
u := uintptr(p)
p = unsafe.Pointer(u + offset)
```

So a flow the walk stops following here is a flow no valid program has, and an
operation the walk does not name costs precision rather than correctness.
`cmd/compile/internal/escape.(*escape).unsafeValue` is the same walk over the
same shapes, which is what keeps nanogo's note equal to `gc`'s wherever the
pattern appears. The table in the `abi.NoEscape` section above is the reading
that fixed the rule's shape: the flow survives the arithmetic and dies at the
variable, in `gc` and here alike.

Stage 1 refused this conversion instead, on the ground that the reference
survives in a word the collector does not trace. The ground was right and the
conclusion did not follow: a word the collector does not trace is a word no
valid program recovers a pointer from, so there is nothing left to describe.

### The one rule that is about nanogo and not about Go

**A parameter with `ir.Object.Escapes` set takes the empty note, whatever the
body does.** `ir.Build` sets the mark where the source takes a variable's
address and where a literal captures one, and `ir/lower.go` moves every such
variable into a heap cell. So nanogo's own callee puts that parameter's value
in the heap before the body runs, and `esc:` would be false about nanogo's code
generation rather than about the source. `gc` reaches a different answer for
some of those cases because `gc` asks whether the address escapes; nanogo does
not ask, so nanogo cannot claim the parameter stayed out of the heap.

### What stops a proved note landing on the wrong parameter

The notes are positional on both sides. `export/writer.go` writes
`sig.Params().Len()` of them plus one for a receiver, and
`cmd/compile/internal/noder`'s `funcExt` reads them over
`types.Type.RecvParams`. Every other failure in this path produces the empty
note; this one produces a proved note on a parameter nothing was proved about.

Two things guard it. `driver.escapeNotes` keys the map by `ir.Func.Sym` and
**drops a key two functions share** rather than letting the second overwrite
the first. `export/writer.go`'s `paramNotes` rebuilds the same key from what
the checker recorded and **drops a list whose length is not the one it is about
to write**, in full, because a list one short does not lose its last entry: it
moves every entry after the gap onto another parameter.

### What stages 1 to 3 moved, and what they did not

`cmd/nanogo/escapenote_test.go` holds the two marks.
`TestProvedNoteUnblocksTheRuntimeRule` is stage 1's: `lib.Load(b []byte)` reads
`b` and keeps nothing, nanogo proves it, and `gc` compiles a consumer of it
under `-+` that hands it a slice of an array in its own frame. The same file's
`lib.Keep`, which stores its parameter in a global, still stops that build with
`escapes to heap, not allowed in runtime`, which is the edge that must not move
with the first half.

`TestTheNoEscapeShapeBuildsAndRuns` is stage 2's, and it reads the program
rather than the archive. `lib.NoEscape` is `internal/abi.NoEscape`'s body
character for character, `user` is compiled with `-+` and hands the address of
its own local through it to a callee `gc` may not inline, and `main` runs the
result twice with a collection between the runs. Against the compiler before
this change the build stops with `key escapes to heap, not allowed in runtime`;
against the compiler after it, the binary prints what an all-`gc` build of the
same source prints.

**The closure reading did not move: 24 of 28, the same four refusals**
(`internal/bytealg`, `internal/runtime/atomic`, `internal/runtime/maps` and
`runtime`, none of them for a reason this spec owns). That measurement compiles
each package alone against dependencies `gc` built, so no nanogo-written note
is ever read in it, and no stage of this spec can move it.

**The whole-closure build moved, and the three sites this spec named are
gone.** With nanogo owning all 24 compile invocations it can,
`internal/runtime/maps` now compiles under `-+`, and the same command against
the compiler before this change still fails at all three:

```
runtime_fast32.go:479:57: key escapes to heap, not allowed in runtime
runtime_fast64.go:544:57: key escapes to heap, not allowed in runtime
runtime_faststr.go:409:58: key escapes to heap, not allowed in runtime
```

**The build now stops at `runtime` instead, and the shape of the new blocker is
the next stage's rather than this one's.** Two kinds of site:

```
print.go:129:6:    buf escapes to heap, not allowed in runtime
debuglog.go:507:6: b escapes to heap, not allowed in runtime
```

The first is `strconv.AppendFloat(buf[:0], ...)` into `internal/strconv`, and
taking that package off nanogo's share makes those sites go away, which is how
they were attributed. The second is `byteorder.LEPutUint64(b[:], x)` and its
neighbours, which write through the slice they are given. **A parameter written
through is `gc`'s mutator flow, `esc:\x00\x01`, and no stage here has an
encoding for it**: the notes above say "no flow at all" or "a flow to a result
and nothing else", and both deny the write. Describing it needs a note that
carries a mutator flow beside the rest, which is stage 4's, along with the
callee summaries the remaining sites want.

No note this change writes can make a new site fail. The empty note is the top
of `gc`'s lattice, so every note here is the one that was written before or one
strictly below it, and a note below the top can only make `gc` assume less
escape. The `runtime` sites were behind `internal/runtime/maps` all along.

## The staging

Every stage keeps the notes sound. A stage that writes a note it cannot prove
is worse than no stage, because the wrong answer lands in a caller `gc`
compiled and not in anything nanogo compiled.

| Stage | What it adds | What it unblocks | State |
| --- | --- | --- | --- |
| 1 | "Flows nowhere" per parameter, from a body walk with an allowlist of operations | A runtime-rule package that hands a local to a nanogo function that keeps nothing | built |
| 2 | More operations in the allowlist: `range`, the type switch, the type assertion, the composite literal that stays in the frame, the string and slice conversion, and the round trip through a `uintptr` | The same, for bodies stage 1 refuses on sight. The `uintptr` row is the one that reaches `abi.NoEscape` | built |
| 3 | Result flows at zero dereferences, so a parameter that leaves through a result is described rather than refused | Every identity-shaped function. **Not** `abi.NoEscape`, whose note has no flow at all: see the correction above | built |
| 4 | Dereference counts deep enough to write, the flow graph below, and summaries across a call | A parameter that reaches a callee that keeps nothing, and every result flow the walk now measures at one or more | not built |
| 5 | A consumer inside nanogo: the allocation decision the rest of this spec is about | The five sites in the opening table, and [022](022-optimization-passes.md)'s G3 gate | not built |

Stages 1 to 4 decide only what nanogo tells `gc`. Stage 5 is the first that
changes the code nanogo emits, and it is the one the opening table waits for.

**A note saying a parameter does not escape, written without proving it, is not
a pessimisation that costs an allocation. It tells `gc` it may leave a caller's
local in the frame while nanogo's callee retains a pointer to it, and the
collector then holds a pointer into a frame that is gone.** Writing `esc:` for
every parameter was tried in `cmd/nanogo/escapenote_test.go`'s fixture and it
makes the refusal go away. That is the reason it must not be done: the refusal
leaves the build log and the miscompile does not.

## The full design, which stages 4 and 5 build

The design below stands and the built stages do not replace it. What is built
is a walk over one body with a taint set and a depth per name. This is the
graph the later stages need, and it was reviewed and not disproved, only
deferred.

Escape analysis decides, for each allocation, whether it can live in the
caller's stack frame or must go on the heap. It runs on the IR of
[020](020-ir.md), before SSA, because it needs to see assignment to fields and
passing to calls as Go operations.

The safe answer is always "heap". [022](022-optimization-passes.md) explains why
that stops being acceptable at G3.

## The question

An allocation may be stack-allocated when no reference to it outlives the frame
that created it. References outlive the frame by being: returned, assigned to a
global, assigned into a heap object, sent on a channel, captured by an escaping
closure, or passed to a function that does one of those things.

## The model

A directed graph. Vertices are *locations*: named variables, allocation sites,
function parameters, results, and one distinguished vertex `heap`.

An edge $u \xrightarrow{k} v$ means "the value in $v$ flows to $u$ through $k$
dereferences", with $k$ counted as:

| Assignment | Edge |
| --- | --- |
| `u = v` | $u \xrightarrow{0} v$ |
| `u = &v` | $u \xrightarrow{-1} v$ |
| `u = *v` | $u \xrightarrow{1} v$ |
| `u = v.f` | $u \xrightarrow{0} v$, fields are not distinguished |
| `u[i] = v` | $u \xrightarrow{0} v$ |

Field insensitivity is a deliberate loss of precision that removes a whole
dimension from the analysis.

An allocation escapes when there is a path from `heap` to it whose edge weights
sum to $-1$ or less:

$$
\mathrm{escapes}(a) \iff \exists\ \text{path } heap \rightsquigarrow a
\ \text{ with } \sum_i k_i \le -1
$$

The sum being negative means the path passes an address-of that is never undone
by a dereference, so the heap holds the address of $a$.

The computation is a shortest-path traversal from `heap` over the graph, taking
the minimum weight sum to each vertex. Edge weights are bounded, so it
terminates.

## Across function boundaries

The whole-program version of this is not affordable. The standard answer, and
nanogo's, is a **summary per function**: for each parameter, how far it leaks,
to the heap, to a specific result, or not at all.

Summaries are computed bottom-up over the call graph's strongly connected
components. Within a component, iterate to a fixed point. Across packages, the
summary travels in export data ([015](015-export-data.md)).

A call to a function with no summary, indirect or through an interface nanogo
did not devirtualise, leaks every pointer argument to the heap. This is the
conservative case and it is common.

## The directives

Three of [016](016-directives-and-pragmas.md)'s pragmas exist only for this pass
and all three are used by the runtime and the standard library:

| Directive | Effect |
| --- | --- |
| `//go:noescape` | On a bodyless declaration, asserts no parameter leaks. Believed without proof. |
| `//go:uintptrescapes` | A `uintptr` parameter is treated as a pointer that leaks, keeping the referent alive. |
| `//go:uintptrkeepalive` | The referent is kept alive across the call without being forced to the heap. |

`//go:noescape` is an assertion the compiler cannot check and a lie in it is
memory corruption. It is honoured because `syscall` and `runtime` require it.

`driver/pragma.go` parses all three today and records them on the function.
Nothing reads them, because the pass that would is this one.

## What escaping decides

| Decision | Consumer |
| --- | --- |
| Heap or stack for an allocation | [031](031-runtime-lowering.md): `runtime.newobject` against a frame slot |
| Heap or stack for a closure | [033](033-closures-defer-panic.md) |
| Whether a variable is a stack object needing a GC description | [027](027-liveness-and-stackmaps.md) |
| Whether `make` with a constant small size can be a frame array | [031](031-runtime-lowering.md) |

## Precision, and where it is given up

| Given up | Effect |
| --- | --- |
| Field sensitivity | One escaping field escapes the whole object |
| Flow sensitivity | A variable that escapes on one path escapes on all |
| Indexing sensitivity | One escaping element escapes the whole slice or array |
| Indirect and interface calls without devirtualisation | All pointer arguments escape |

`gc` gives up the first three too. The fourth is why
[024](024-inlining-and-devirtualization.md) matters more to this pass than to any
other.

## Testing

What stages 1 to 3 have:

- `escape/escape_test.go` is the note table, one case per shape the walk
  proves and one per shape it refuses, built through `ir.Build` from source.
  Every proved case names the reason, and every refused case names the flow.
- `cmd/nanogo/escapenote_test.go`'s `TestEscapeNoteMatchesGC` is the oracle.
  Each case is a package with one pointer-bearing parameter, compiled twice,
  and it asserts three things: the note `gc` wrote, the note nanogo wrote, and
  that nanogo's note is `gc`'s own note or the empty one. The third is the
  ratchet, and it holds for every case added later. **Every depth in the table
  above was read out of `gc`'s archive here first**, which is what the table is
  for: a depth guessed from `gc`'s source rather than measured is how a note
  `gc` never wrote gets written.
- One tagged declaration per package is what makes that a per-parameter
  reading, and "one" counts every declaration the archive carries a note for.
  A package that imports one whose functions carry notes re-exports those
  notes into its own stream, and the string section is deduplicated, so a
  callee whose note happens to equal the subject's leaves one entry that reads
  back as the subject's. A case needing a callee cannot be written there at
  all, in that package or in another, and the first attempt at one was found by
  this and not by review.
- `TestProvedNoteUnblocksTheRuntimeRule` is stage 1's mark, both halves: a
  proved note lets `gc` compile a consumer under `-+`, and an unproved one
  still stops it with the runtime rule's own words.
- `TestTheNoEscapeShapeBuildsAndRuns` is stage 2's, and it runs the program.
  `internal/abi.NoEscape`'s body, a consumer under `-+` handing it the address
  of a local, a callee `gc` may not inline, and two runs with a collection
  between them, checked against an all-`gc` build of the same source.
- `export/writer_test.go` covers the positional guard, and
  `driver/escapenotes_test.go` covers the shared-symbol drop.

What the later stages still need:

- `gc` has `-m` output stating each decision, and Go's `test/escape*.go` corpus
  annotates the expected decisions. nanogo is to emit the same shape of output
  and run that corpus. This is a rare case of a compiler-internal decision
  having a ready-made external oracle, and it should be used. nanogo parses `-m`
  today and prints nothing ([050](050-driver.md)).
- Differential execution with the pass off, per
  [022](022-optimization-passes.md): identical output required. This begins at
  stage 5, which is the first stage that changes what nanogo emits.
- A corpus for the three directives, checked in generated code.

## What was wrong

**The spec said the compiler is correct without this pass.** The sentence was
"the safe answer is being taken by default, everywhere ... the compiler is
correct and allocates more than it needs to", and it was written from the four
sites in the table, all of which do take the safe answer. It missed the site
that takes no answer at all, because it is not an allocation: an address-taken
local stays in the frame and a pointer to it can leave the function. The
opening now says where the default holds and where it does not, and the section
above states what the sound rule would be in the meantime.

The spec said nothing false. It said nothing about being unbuilt either, and a
reader of the deck had no way to tell this apart from a pass that ships. The
state was found by listing `ir/` and grepping for the one field the design
names: `ir.Object.Escapes` has four mentions in the repository, its declaration,
its doc comment, one assertion in `ir/node_test.go` that a fresh object has it
clear, and `ir/lower.go`'s allocation section explaining why every allocation
goes to the heap while the field is unset. None of the four is an assignment.

The spec said two consumers are waiting on this pass. Four sites act on its
absence, and the table above says what each does instead. One of them, the
composite literal lowering, does not wait: it takes a decision by shape that is
sound without an analysis, and says so.

The spec listed `ir/` as holding a builder, a type layout, a node set and a
converter. It also holds the lowering pass of [020](020-ir.md) and the
descriptor naming of [032](032-type-descriptors-and-itabs.md).

**The spec quoted a body `abi.NoEscape` does not have, and routed a whole stage
off it.** It said `abi.NoEscape` is
`func(p unsafe.Pointer) unsafe.Pointer { return p }` and that its note is
`esc:\x00\x00\x00\x01`, and the staging table then gave stage 3 the three
`internal/runtime/maps` sites as the thing it unblocks. The body in `go1.27.0`
launders through a `uintptr` variable and `gc`'s note for it is `esc:`, a note
stage 3 cannot produce because stage 3 only adds flows. What frees those sites
is stage 2's `uintptr` row. The section above holds the reading, and the four
paired fixtures that put the boundary at the variable rather than anywhere else.

The lesson is narrower than "check the source". The note was stated from the
body and the body was stated from memory, and both halves are things a
`go build` answers in a second. Every note in this spec is now a number read
out of an archive, and `TestEscapeNoteMatchesGC` re-reads all of them on every
run so that a toolchain upgrade moves the table rather than silently
invalidating it.

**The spec said stage 1 refuses a conversion to a word with no pointer in it
because the collector does not trace such a word.** The ground was right and
the conclusion did not follow. A word the collector does not trace is a word no
valid program recovers a pointer from, because `unsafe`'s rule (3) says so, so
there is no flow left to describe rather than a flow too hard to follow.
