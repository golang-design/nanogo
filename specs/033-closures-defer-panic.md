---
title: "Closures, defer, panic, and recover"
status: in progress
layer: runtime interface
gate: G1
depends_on:
  - 023-escape-analysis.md
  - 031-runtime-lowering.md
---

# Closures, defer, panic, recover

Four features that share one property: each makes the frame itself a data
structure that the runtime inspects. They are specified together because their
interactions are where the bugs are.

## What is built

Closures are built, captures and all, and so are `defer` and `go` with
arguments: an argument is a capture and the capture mechanism is the same one.
That mechanism, the heap cell, is what [023](023-escape-analysis.md) also takes
for a variable whose address the source writes. This spec owns the capture and
that one owns the address; `ir/closure.go` holds one cell for both.
`panic` and `recover` are built and are gated by a different thing: an
interface. `ir/lower.go` performs the rows and `internal/e2e` runs each one as
a program.

| Feature | State |
| --- | --- |
| a function literal that captures nothing | built; a static one-word `funcval` holding the code pointer |
| a **declared** function used as a func value | built; the same static one-word `funcval` |
| a literal with a capture list | built; a heap closure object holding the code pointer and one heap cell per capture |
| a method value of a concrete type | built; the receiver is saved in a temporary where the value is written and a literal captures the temporary, marked `FuncIDWrapper` |
| a method value of an interface | **refused**; the function is read out of the itab and a closure object holds a symbol |
| a named result a literal captures | built; the result gets a cell like any other capture, and the single exit copies the cell into the result object |
| `defer f()` with no arguments and no captures | built; `runtime.deferproc`, plus the single exit below |
| `defer f(x)`, with arguments | built; `ir.Build` puts the call in a literal that captures the operands, marked `FuncIDWrapper` |
| `defer x.M()`, a method of a concrete type | built; the receiver is the call's first operand and travels the same way |
| `defer i.M()`, a method of an interface | **refused**; the value the runtime is given would be a method value of an interface, which is the row above |
| `defer println(x)`, `defer close(c)`, a builtin | built; a builtin is an operation and not a func value, so it travels inside the same literal, whether or not it has operands |
| `defer recover()` | built, and it does not recover; the literal is marked, so `runtime.gorecover` counts no non-wrapper frame, which is what the language asks of a `recover` no deferred function called directly |
| `go f()` and `go f(x)` | built; `runtime.newproc` takes the same one word, on the same terms |
| `panic` of a value that is already an interface | reaches `runtime.gopanic` and runs the deferred chain; the runtime then prints the value, and for a non-nil operand that print is a fatal error, so this row is a miscompile and not a feature |
| `panic` of anything else | **refused**; the operand converts to an interface and `ssa.Build` builds no such conversion |
| `recover()` whose value nobody reads | built; `runtime.gorecover`, which is the shape of the idiom |
| `recover()` whose value is read | **refused**; the result is an interface and nothing below the IR decomposes one |
| open-coded `defer` and `FUNCDATA_OpenCodedDeferInfo` | **not built**; nanogo writes two funcdata indices and that is not one of them |
| a heap `_defer` record built by the compiler | **not built**; `runtime.deferproc` builds it |

**The context register is read now, and the refusals above are what is left
to build on top of it.** `ssa.OpGetClosurePtr` is the callee half: `ir.Func`
carries a `Closure` object, `ssa.Build` materialises the operation in the entry
block for a function that has one, and the arm64 rules lower it to
`OpARM64LoweredGetClosurePtr`, one register move out of R26.

The operation is not pre-coloured to R26 and it must not be. `ssa/regalloc.go`
refuses a `Target.DefReg` answer that names a register outside the allocatable
set, and R26 is outside it deliberately: `ssagen` writes the closure word into
R26 at every indirect call site, outside the allocator's model, so a value the
allocator parked there would be destroyed at that call. The operation is
therefore an ordinary value with an ordinary home, and the move out of R26 is
the instruction that fills it. It is defined in the entry block, which is what
makes the move correct: the register holds the closure until the function's
first call and no longer.

**The stack-growth tail is part of the same answer.** A function that reads the
register calls `runtime.morestack` and every other function calls
`runtime.morestack_noctxt`. The two differ in one instruction:
`morestack_noctxt` writes zero into R26 before it grows the stack and
`morestack` saves R26 into `g.sched.ctxt`, which `gogo` restores. Calling the
first from a closure resumes it with a nil closure object, and calling the
second from a function with no closure puts whatever the caller left in R26
into a field the collector scans. `ssa.Func.NeedCtxt` carries the answer from
`ir.Func.Closure` to the code generator.
`internal/e2e`'s `TestToolexecGrowsAClosureStack` runs the tail: a recursive
literal twenty thousand frames deep that reads a capture in every frame. Built
with the other symbol it dies with a nil dereference.

**The `panic` and `recover` refusals are not that gap.** Both are the interface
one. `panic("boom")` and `panic(1)` are refused at `ssa.Build`, on the
conversion of the operand, and so is every `recover` whose result is read. A
conversion to an interface in `ssa.Build`, and the descriptor and itab writers
[032](032-type-descriptors-and-itabs.md) owes it, are what unblock that half.

**What `panic` of an interface value does is worse than a refusal, and this is
the one row in the table that names a miscompile.** `panic(v)` compiles when
`v` is already an interface value, which in practice means a parameter of
interface type or `nil`. `runtime.gopanic` is reached and the deferred chain
runs, both observable. `panic(nil)` then ends as `gc`'s does, with `panic:
runtime error: panic called with nil argument` and exit 2, because the runtime
special-cases a nil operand before it reads the operand's type. A non-nil
operand is read, and the read fails:

```
panic: runtime: nameOff 0x4310388 out of range 0x104304b00 - 0x104318a80
fatal error: runtime: name offset out of range
```

`runtime.printpanicval` resolves the value's type name through the eface's type
word, so the eface nanogo built carries a type word the runtime cannot follow.
Nothing is said at compile time, which makes this the worse failure of the two:
a refusal names a gap and this prints a runtime crash naming none. It belongs
to the same interface gap as the refusals and it is a separate piece of work,
because this operand needed no conversion: what fails is the eface nanogo
already builds, not one it declines to build.

The two funcdata indices nanogo writes are `FUNCDATA_ArgsPointerMaps` and
`FUNCDATA_LocalsPointerMaps`, indices 0 and 1, both built by
`ssagen/stackmap.go`, which is the count [040](040-object-format.md) records
for `AuxFuncdata`. `internal/abi` puts `FUNCDATA_OpenCodedDeferInfo` at index
4, and an entry's position in the object is its index, so writing it means
writing indices 2 and 3 first: the stack-object table
[027](027-liveness-and-stackmaps.md) owns, and the inline tree
[024](024-inlining-and-devirtualization.md) owns. Neither is written.

### An operand of `defer` or `go` is a capture, and the literal is a wrapper

`runtime.deferproc` and `runtime.newproc` take one word and call it with
nothing, so an operand has to travel inside that word. `ir.Build` puts the call
in a function literal and the operands become its captures:

```
defer f(x)   becomes   t := x
                       defer func() { f(t) }()
```

The operands are already in temporaries, because the specification evaluates
them when the statement runs and not when the call runs. Each temporary is
written once, so capturing it by reference and capturing it by value are the
same value, and one capture mechanism covers the row: `defer end(n)` followed
by an assignment to `n` still calls `end` with the value `n` held at the
statement. The callee travels the same way unless it is a declared function,
which nothing can reassign.

**The literal is marked `FuncIDWrapper`, and the mark is a correctness
requirement.** `runtime.gorecover` recovers only when exactly one non-wrapper
frame stands between it and `runtime.gopanic`, and it decides which frames are
wrappers by the funcID in each function's `FuncInfo`. Without the mark, `defer
f(x)` where `f` recovers stops recovering, and nothing says so at compile time.
`ssagen` writes the funcID and `internal/e2e`'s
`TestToolexecRecoversFromAWrappedDefer` is the program that proves it.

A call of a func value with no operands is not wrapped. Wrapping it would add
the very frame `recover` counts, for nothing.

**A builtin is wrapped whether or not it has operands, and the reason is a
different one.** `defer println(x)` and `go println(x)` hold an operation and
not a call, so there is no word to hand `runtime.deferproc` until a literal
holds one. `ir.Build` therefore wraps every builtin a statement can name:
`close`, `copy`, `delete`, `clear`, `panic`, `print`, `println` and `recover`,
which is the set the language allows as a statement. gc wraps the same set, in
`typecheck`'s `normalizeGoDeferCall`.

`panic` is in that set, so `defer panic(v)` and `go panic(v)` now reach the two
`panic` rows above rather than a refusal of their own: the operand of an
interface type reaches the miscompile and every other operand is still refused
at `ssa.Build`, on the conversion. No file of Go's own corpus writes either
statement.

Wrapping is also what puts the builtin's own lowering inside the literal.
Lowering a `println` emits its runtime calls into the list being lowered, so a
builtin left where the statement is would print at the statement and defer
only `runtime.printunlock`: a wrong program rather than a refused one.

**`defer recover()` is built and it does not recover, which is what the
language requires.** `recover` returns nil unless a deferred function called it
directly, and here the deferred function is `recover` itself.
`runtime.gorecover` decides this by counting the non-wrapper frames between
itself and `runtime.gopanic` and recovering only when there is exactly one; the
literal is marked, so there are none. `runtime.gorecover`'s own comment names
this case and its own answer for it. The mark is therefore what makes the
statement correct rather than merely compilable: without it the wrapper is one
non-wrapper frame, the panic is caught, and the program that should have died
exits zero. `internal/e2e`'s `TestToolexecDoesNotRecoverFromADeferredRecover`
is the program that says so, and Go's own `test/recover1.go` runs the
interaction with an enclosing panic.

**An operand that reads nothing gets no temporary.** A temporary here is a
capture, a capture is a heap cell, and a cell needs a type descriptor, so
saving an operand whose value cannot change refuses programs for nothing:
`defer println((func())(nil))` is Go's own `test/deferprint.go` and a literal
func type has no descriptor to allocate a cell with. A constant is such an
operand, so is a conversion of one, because a conversion is a function of its
operand and reads nothing else, and so is a declared function, which nothing
can reassign. gc leaves the same operands where they are, and for the same
reason its `visit` returns for a literal, for nil and for a function name.

**A method of an interface is the row that is left.** Its receiver stays inside
the selection, so the value the runtime would be given is a method value whose
function is read out of the itab. `closureExpr` refuses one for the same
reason, and both wait on the same work.

**A capture whose type has no canonical name refuses the closure.** The cell is
allocated through `runtime.newobject`, which takes a `*_type`, so a capture of
a literal func type or a literal struct type is refused and the refusal names
the capture. `defer h(x)` through a parameter `h func(int)` is the shape that
meets it, and a defined func type does not.

### The single exit, which the linker requires

A function that defers leaves through one epilogue, `.exit`, and that epilogue
holds the only call to `runtime.deferreturn`. `cmd/link` records the offset of
one such call per function in `pclntab`, so a function with two of them
resumes at the wrong one after a panic. Every `return` in a function that
defers becomes a `goto` to that label.

### A method value binds its receiver, and the binding is a capture

The language evaluates the receiver where the method value is written and the
call made later uses that saved copy. That is a capture by value, and this
spec builds one capture shape, so the two are joined the way the operands of a
`defer` above are joined:

```
x.M   becomes   t := x
                func(a ...) (r ...) { return T.M(t, a...) }
```

`t` is written once and no other expression names it, so a capture of `t` by
reference and a capture of `x` by value hold the same value for as long as the
closure exists. **A by-value capture is a by-reference capture of a variable
nobody else can reach**, and no second capture shape is built for it.

What is saved is the receiver the method's signature wants and not the operand
the source wrote. `ir/build.go`'s `recvArg` has already taken the address for a
pointer receiver on an addressable value, which is what the language saves
there, so `f := v.M` through a pointer receiver sees a later assignment to `v`
and one through a value receiver does not.

**The literal is marked `FuncIDWrapper`, for the reason the `defer` literal
is.** `f := x.M` followed by `defer f()` puts this frame between
`runtime.gopanic` and the method, and `runtime.gorecover` counts the frames it
is not told to skip. Without the mark a method that recovers stops recovering
and nothing says so at compile time.

`gc` reaches the same behaviour with one function per method rather than one
per site. `walkMethodValue` builds `&struct{F uintptr; R T}{T.M-fm·f, x}` and
`methodValueWrapper` generates `T.M-fm`, a duplicate-tolerant function whose
receiver is a hidden closure variable held **inline** in that struct, and whose
body is a tail call to the method. Holding the receiver inline needs a closure
object type per receiver type, and the section above gives one reason there is
one type per arity here; a wrapper shared between packages also needs a
duplicate-tolerant symbol, which only the generated functions of
[032](032-type-descriptors-and-itabs.md) have. The literal costs one function
symbol per site and one cell, and needs neither.

`gc` also emits a nil check of the itab at a method value whose receiver is an
interface, so that the panic happens where the value is formed rather than
inside the wrapper. That row is refused here for the reason above, so the
question does not arise yet, and it is part of the same work.

### A captured named result

A named result is a variable the language shares with a literal, so it gets a
heap cell like every other capture. What makes it different is that the result
object is also the storage the ABI returns, so the two have to be joined:

- every `return` writes the **cell**, which is what makes `return x` visible to
  a deferred function and to a literal that outlives the frame;
- the exit copies the cell into the result object, **after** the call to
  `runtime.deferreturn`, because a deferred function may assign the result and
  what it assigns is the cell.

A function that captures a result and defers nothing gets the same exit without
the `deferreturn` call. `return x` assigns the named result whether or not the
function defers, and a literal that outlives the frame reads the cell
afterwards, so the write and the read back have to happen there too.

**The results are address-taken and the cell is not.** A result is assigned at
every return, so it is a phi at the exit, and the copies a phi resolves into
sit at the end of the predecessor blocks. `runtime.recovery` restores the stack
pointer, restores no register and jumps straight to the `deferreturn` call, so
it skips those copies; the result has to come out of the frame instead. The
cell is assigned once at the entry, which dominates the exit, so it is one
value with one home, and `ssa/regalloc.go`'s `AllocInvCall` already puts a
value live across a call in the frame. Marking it as well would pin a frame
slot for a reason that does not hold.

### Capture is by reference, and this spec does not decide it

The section "Capture by value or by reference" below hands the choice to
[023](023-escape-analysis.md). There is no escape analysis, and nothing waits
for one: **every** capture is a capture by reference. One `ir.Object` is shared
by the enclosing function and the literal, and lowering moves that object into
a heap cell of its own, so both functions reach one variable through one
pointer.

The cell is not an optimisation this spec skipped. A literal that outlives the
frame that made it would read a frame slot that no longer exists, which is
memory corruption and not a wrong value, and deciding that a literal does not
outlive its frame is [023](023-escape-analysis.md)'s judgement. So the cell is
what a capture costs until that pass exists.

**The cell is allocated where the variable is declared and not at the entry.**
A variable declared in the body of a loop is a fresh variable on every
iteration, which is the Go 1.22 rule the IR builder already performs by putting
the declaration inside the loop. One cell allocated at the entry would be one
variable for every iteration, and every literal made in the loop would read the
last one's value. Lowering puts the allocation in the innermost statement list
that holds every mention of the variable: the loop body when the variable is
declared there, and in front of the loop when the loop's own condition names
it.

**The closure object's type is one type per arity and not one per closure.**
The object is allocated through `runtime.newobject`, which takes a `*_type`,
and [032](032-type-descriptors-and-itabs.md) refuses a name for a literal
struct. A struct synthesised per closure would need a synthesised name per
closure and would inherit every refusal its capture types carry: a capture of
func type would refuse the whole closure for a reason that has nothing to do
with the closure. So the object is `.closureN`, a code pointer followed by N
`unsafe.Pointer` words, and no capture's own type is asked for. The collector
needs one fact about the object and `unsafe.Pointer` says it: the code pointer
stays a `uintptr` and every capture word is traced.

`internal/e2e`'s `TestToolexecKeepsCapturesThroughACollection` is the evidence.
It runs a program whose only reference to a heap object is the capture, under
`GODEBUG=gccheckmark=1,clobberfree=1` and `GOGC=1`, so a capture word the map
missed is a crash where the mistake is, and a code pointer the map claimed
would send the collector after a text address.

Everything past the sections above is a design and not a description.

## Closures

A function literal that references variables from an enclosing function becomes a
**closure object**: a pointer to the code, followed by the captured variables.

```
+------------------+
| code pointer     |
+------------------+
| captured var 1   |
| captured var 2   |
| ...              |
+------------------+
```

At a call, the closure object's address is in the context register, R26 on
`arm64` ([030](030-abi.md)), and the function body loads captures relative to
it.

### Capture by value or by reference

A variable is captured **by value** when the closure does not assign to it and
neither does anything else after the closure is created. Otherwise it is captured
**by reference**, which means the variable is moved out of the frame into a heap
cell and both the enclosing function and the closure access it through a pointer.

The decision belongs with [023](023-escape-analysis.md), since a by-reference
capture is an escape. It is not taken there today. The IR builder captures
everything by reference, as the note at the top of this spec records.

### Where the closure object lives

On the stack when the closure does not outlive the frame, on the heap when it
does.

**Nothing puts one in the frame today**, and the reason is unbuilt work rather
than an oversight. Deciding that a closure does not outlive its frame is
[023](023-escape-analysis.md)'s judgement, and there is no escape analysis. So
every func value `ir/lower.go` builds is a heap allocation and a store, where
`gc` has neither. That is the cost and it is correct.

**A declared function used as a func value is the case this spec never
separated from a literal, and it did not behave like one.** `apply(inc)` and
`return inc` compiled, linked, and faulted with `unexpected fault address
0xd65f03c091000400`. The address is the evidence: `0x91000400` is
`ADD X0, X0, #1` and `0xd65f03c0` is `RET`, the two instructions of `inc`. The
value handed across the call boundary was `inc`'s entry address where a
`funcval` was expected, and the indirect call loaded a code pointer from it and
branched into the instruction stream read as data. `f := inc` then `f(1)` was
refused instead, with `the entry point is in R0, which is also an argument
register`, which was a second failure with a second cause.

Both are fixed and the causes were separate.

A function symbol in a value position is now a `funcval`, built by the same
lowering row as a captureless literal. `ir/lower.go` builds it wherever a
function symbol is not the callee of a direct call, which is one rule and not a
list of positions: an argument, a result, an assignment, an element of a
composite literal, and whatever else the grammar allows all reach it.

The register collision was the ABI's. specs/030-abi.md now fixes where an
indirect call reads its entry point: the first scratch register of the class,
which no argument and no result uses and which is never a value's home. The
entry point moves there with the arguments, in the one parallel move the call
site already makes, so no source is destroyed and no allocation can put the
branch target in a register the arguments have overwritten. `ssagen` had a
guard for the collision and it is not needed any more.

**The read-only `funcval` symbol is built and the row above is no longer an
allocation.** gc emits one word of read-only data per function used as a value,
`dupok`, named `<fn>·f`, so the value is a link-time constant; `walkClosure`
says it in one line, "If no closure variables, don't allocate a closure object;
use a static funcval". `ir.LowerAndCollect` reports the functions whose value
the tree names, beside the type descriptors and itabs it already reported, and
`driver` defines each symbol once per package.

It is a non-package definition and not a package one. `obj` refuses a
duplicate-tolerant symbol in the package index space, because `cmd/link`
deduplicates by name in the non-package space only, and two packages do emit
this symbol when both take the value of a function one of them declares.

The allocation this replaced was correct and slow, and it was also a difference
a program can see. Go's own `test/closure.go` reads `runtime.MemStats` around
two calls to a function returning a literal with no captures and fails when
either allocated, which is what it did.

The same channel is what a string constant reaching `runtime.printstring`
wants, and that one is still missing.

## Defer

Three implementations, chosen per call site. The choice is a real difference in
cost, not a tuning knob.

### Open-coded

When a function's deferred calls are all unconditional or conditional in a way
the compiler can track, and there are at most eight of them, and none is in a
loop:

- the deferred function and its arguments are evaluated into frame slots at the
  `defer` statement;
- a bitmask in the frame records which have been armed;
- the deferred calls are emitted inline before each `return`;
- `FUNCDATA_OpenCodedDeferInfo` describes the slots and the bitmask so that
  `panic` can run them from the runtime.

This is the fast path and it costs about as much as writing the call out by hand.
It is also the one that requires the runtime to understand the frame, through
that `FUNCDATA`, which is why it cannot be approximated.

### Stack-allocated `_defer`

When open-coding does not apply but the `defer` is not in a loop: a `_defer`
record in the frame, linked onto the goroutine's defer chain by
`runtime.deferprocStack`.

### Heap-allocated `_defer`

`runtime.deferproc`, allocating. It is correct for every `defer`, and the other
two implementations are optimizations of it rather than cases it excludes. A
`defer` in a loop is the case where it is the only choice, because the number
of records is not known. nanogo emits it for every case, which is why `defer`
works before either optimization exists.

### `deferreturn`

A function containing any non-open-coded `defer` ends with a call to
`runtime.deferreturn`, which runs the chain. Open-coded defers do not need it on
the normal return path, only on the panic path.

## Panic and recover

`panic` is `runtime.gopanic`. It walks the goroutine's defer chain, running each
deferred call in a frame it constructs, and either finds a `recover` or reaches
the bottom and terminates the program.

`recover` is `runtime.gorecover`, and it is **only effective when called
directly from a deferred function**. It takes no argument. It walks the stack
itself and counts the frames between its own caller and `runtime.gopanic`,
recovering only when there is exactly one.

The compiler's obligation is therefore not to pass anything. It is to put
nothing between the deferred function and the call, because one extra frame
turns a `recover` into a no-op. That is also why
[024](024-inlining-and-devirtualization.md) refuses to inline a function
containing `recover`: inlining changes which frame it is in, and would change
its meaning from "recovers" to "does not".

### The interaction that must be right

A panic unwinds through frames whose deferred calls are open-coded. The runtime
has no defer records for those; it has the `FUNCDATA` table and the frame. So the
panic path reads compiler-generated metadata to reconstruct calls that were going
to be emitted inline.

If the table is wrong, a panic in a function with open-coded defers runs the
wrong function, or none, and the failure appears only on the panic path, which
ordinary tests do not take.

## Testing

- Go's `test/` corpus holds `defer*.go`, `recover*.go` and `closure*.go`, and
  they cover the interactions above better than a new suite would.
- A corpus that panics through every defer implementation, including a mix in one
  call chain: open-coded frame calls stack-allocated calls heap-allocated.
- `recover` in a directly deferred function, in a nested one, and in a function
  called by a deferred one. Only the first recovers, and asserting the other two
  do not is the test that catches a frame-pointer mistake.
- Closure capture by value and by reference, with the loop-variable semantics of
  Go 1.22 and later, where each iteration has a fresh variable.

None of the four bullets is reached in full. Of the corpus files the first
bullet names, four run and match `gc`'s own build: `deferprint.go`,
`print.go`, `goprint.go` and `recover1.go`, each of which the deferred-builtin
row unblocked. `recover.go` is still refused, on `defer i.M()`, which is the
interface method value row above. `recover5.go` reaches
`internal/gotest/testdata/ratchet.txt` as `rejected`, which proves the type
checker and not the back end. No `closure*.go` file runs.

What else runs is `internal/e2e`'s programs: a closure, a `defer`, a `go`, a
`defer` that runs while panicking, a `recover`, a `print`, a deferred builtin
and a `defer recover()`. [004](004-conformance.md) counts them under L3, and
its own caution about the link-and-run cases applies to them as well: the
oracle is expected output written in the test, except for the deferred builtin,
whose oracle is the same program built by `gc`.

## What was wrong

### The escape decision was handed to a spec with no code

This spec left "capture by value or by reference" to
[023](023-escape-analysis.md) and did not say what happens until that pass
exists. It exists nowhere, and the IR builder does not wait: every capture is
by reference and every func value is a heap allocation. The two sections above
carry the state; neither was a decision this spec took.

### The heap `_defer` was presented as a restriction

`runtime.deferproc` was described as the case for a `defer` in a loop, where
the number of records is not known. That reads as a restriction and it is not
one. `deferproc` is correct for every `defer`, and it is what nanogo emits for
all of them.

### `recover` was said to take the caller's frame pointer

This spec said the compiler's obligation is to pass the caller's frame pointer
so the runtime can check that `recover` was called directly from a deferred
function. `runtime.gorecover` takes no argument and walks the stack itself. The
obligation is the opposite one, and the section above states it.

### The funcdata count was three

`ssagen/stackmap.go` allocates two entries and fills
`FUNCDATA_ArgsPointerMaps` and `FUNCDATA_LocalsPointerMaps`. The count of
three also made an open-coded defer table look like one more entry, and
`internal/abi` numbers it 4, with two unwritten tables below it.

### A declared function was assumed to reach a `funcval` like a literal

The table had one row for a function literal that captures nothing and no row
for `inc` itself, so a reader would take `apply(inc)` to be the same case. It
is not, and the difference is not a refusal: passing or returning a declared
function compiles and faults. The row and the table above separate the three
outcomes, because a spec that lists only the working one is what let this reach
a binary.

### `panic` was called built without saying what it is built for

The table said `panic` was built, which is true of the lowering row and false
of nearly every `panic` a Go program writes. A `panic` whose operand is not
already an interface is refused at `ssa.Build`. The row is now two rows.

**The row that was left said the surviving case runs, and it does not.** The
spec read "the deferred chain runs, the runtime prints the trace, the process
exits 2", which was measured on `panic(nil)` and generalised. A non-nil
interface operand dies in `runtime.printpanicval` with `name offset out of
range` and prints no panic value. The section above states both cases
separately, because the difference between them is a special case in the
runtime and not anything this spec's lowering does.

### The wrapping rule was written for a call and read as one for a statement

This spec said the operands of a `defer` or a `go` travel inside a literal and
that a call with no operands is not wrapped, and both sentences are about a
call of a func value. `defer println(x)` has operands and was refused anyway,
because a builtin is not a call, and `defer recover()` has none and needs the
literal for a reason the no-operand sentence does not cover: there is no func
value to give the runtime, with or without operands. The sections above
separate the two rules, and `defer recover()` gets a row of its own, because a
reader who took the no-operand sentence at its word would conclude that
wrapping it is wrong when it is the only thing that makes it right.

### The operands were all said to be temporaries

"The operands are already in temporaries" was true of the builder and it was
too much. A temporary is a capture and a capture is a heap cell with a type
descriptor, so `defer println((func())(nil))` was refused for a capture of a
literal func type, where the operand cannot change and needs no cell at all.
The section above states which operands need one.
