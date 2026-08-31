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

**No analysis in this spec is built.** There is no pass, no graph, no summary
and no `-m` output. `ir/` holds a builder, a type layout, a node set, a
converter, the lowering pass of [020](020-ir.md) and the descriptor naming of
[032](032-type-descriptors-and-itabs.md), and no analysis over any of them.
What exists is `ir.Object.Escapes`, which now carries the interim answer rather
than the analysis's: `ir.Build` sets it where the **source** takes a variable's
address and where a literal captures one, and `ir/lower.go` moves every
variable it names into a heap cell.

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

## What the missing pass blocks in a mixed build

`export/writer.go` writes one escape analysis note per receiver and parameter,
and every one of them is the empty string. **That is not a missing note and
`gc` does not reject it.** It is the short encoding of the most pessimistic
answer there is, and reading it as anything else has sent one investigation
down the wrong path already.

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
What nanogo writes today is therefore already the only sound note available
without this pass, and the two `esc:` forms below are the only others.

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
local, fails. `cmd/nanogo/escapenote_test.go` is that case in eight lines of
Go, with `//go:noinline` as the only directive, and it asserts all four halves:
`gc`'s archive carries an `esc:` tag, nanogo's carries none, `gc` compiles the
consumer against `gc`'s archive under `-+`, and it refuses the same consumer
against nanogo's.

### The two sound notes available without the pass

Both are `gc`'s own, taken from `paramTag`. Neither is written today, because
neither moves the blocker and an unexercised note-writing path is where the
next unsound note gets introduced.

| Case | Sound note | Why it is sound |
| --- | --- | --- |
| A parameter that is unnamed or `_` | `esc:`, the encoding of no flow to anywhere | The body cannot name the parameter, so nothing can flow out of it |
| A bodyless declaration carrying `//go:noescape` | mutator and callee at 0, no heap flow | The pragma is an assertion by the author, and `gc` honours it without proof for the same reason |

Every other note needs the graph below. **A note saying a parameter does not
escape, written without proving it, is not a pessimisation that costs an
allocation. It tells `gc` it may leave a caller's local in the frame while
nanogo's callee retains a pointer to it, and the collector then holds a
pointer into a frame that is gone.** Writing `esc:` for every parameter was
tried in `cmd/nanogo/escapenote_test.go`'s fixture and it makes the refusal go
away. That is the reason it must not be done: the refusal leaves the build log
and the miscompile does not.

The design below stands. It was reviewed and not disproved, only deferred.

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

None of this is written. Recorded because the oracle is the reason this pass is
cheap to get right, and it should be set up before the first line of the pass:

- `gc` has `-m` output stating each decision, and Go's `test/escape*.go` corpus
  annotates the expected decisions. nanogo is to emit the same shape of output
  and run that corpus. This is a rare case of a compiler-internal decision
  having a ready-made external oracle, and it should be used. nanogo parses `-m`
  today and prints nothing ([050](050-driver.md)).
- Differential execution with the pass off, per
  [022](022-optimization-passes.md): identical output required.
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
