---
title: "Inlining and devirtualization"
status: draft
layer: middle end
gate: G1
depends_on:
  - 020-ir.md
  - 015-export-data.md
---

# Inlining and devirtualization

**Neither is built.** There is no cost model, no substitution, no inline tree,
and no devirtualization. `ir.Func.Inlinable` is a reserved field assigned
nowhere. `driver/flags.go` parses `gc`'s `-l` into `Config.NoInline`, and
nothing reads it, so the flag that disables inlining and the inlining it
disables are both absent.

Only half of inlining can be built where it stands. Inlining rewrites the IR
tree in place, and [020](020-ir.md)'s tree exists. What is missing under it is a
body in export data, the only way a body crosses a package boundary:
[015](015-export-data.md) is written and read, and it carries declarations only.
`export/writer.go` sets the inlinable-body flag false for every function, and
the reader is the types-only one, so cross-package inlining has nothing to
inline.
Intra-package inlining is buildable now and is worth having for that reason: it
is the half that needs nothing new.

The design below stands. It was reviewed and not disproved, only deferred.

Both passes run on the IR of [020](020-ir.md), both are optional under
[022](022-optimization-passes.md)'s governing rule, and both matter more for what
they enable than for what they save directly.

Inlining's real payoff is that it turns interprocedural questions into
intraprocedural ones. [023](023-escape-analysis.md) is imprecise across calls and
precise within a function; inlining a call converts one into the other.
Devirtualization's payoff is that it creates calls that can be inlined at all.

## Inlining

### The cost model

A function is inlinable when its body's cost is below a budget and it contains
nothing on the forbidden list.

Cost is a weighted node count over the IR, walked once per function and cached.
Ordinary nodes cost 1. A call costs 57 in the reference implementation against a
budget of 80, both in `cmd/compile/internal/inline/inl.go`, which is the
mechanism that stops a function containing two calls from being inlined. nanogo
takes the same shape of number and tunes it against the corpus rather than
inventing one.

The walk stops as soon as the budget is exceeded, so the common case of a large
function is cheap to reject.

### The forbidden list

These make a function non-inlinable regardless of cost:

| Construct | Why |
| --- | --- |
| `recover` | Its meaning depends on the frame it is in |
| `select` | Complexity, no payoff |
| Labelled `break` or `continue` crossing the inline boundary | Control flow would have to be rewritten |
| `go` and `defer` of a closure over the frame | Frame identity matters |
| Direct recursion | Would not terminate |
| `//go:noinline`, `//go:norace`, `//go:yeswritebarrierrec` | Directives, [016](016-directives-and-pragmas.md) |

`defer` is allowed only in the shape [033](033-closures-defer-panic.md)
specifies for open coding, because inlining a `defer` into a caller changes
which frame it belongs to. That shape is a constraint on this pass and not a
capability to lean on: [033](033-closures-defer-panic.md) does not open-code any
`defer` today, and it writes no `FUNCDATA_OpenCodedDeferInfo`.

### Mechanism

Substitute the callee's IR into the caller, with:

- Parameters bound to temporaries assigned in argument order, preserving
  evaluation order per [020](020-ir.md).
- Locals renamed to fresh objects.
- `return` rewritten to an assignment to result temporaries and a `goto` to a
  label at the end of the inlined body.
- Positions rewritten to record the inline tree, so that
  [046](046-debug-info.md) and a panic traceback name the original function.

The inline tree is not optional. A traceback through inlined frames that names
the wrong function makes every runtime bug harder, and the runtime is what nanogo
compiles at G3.

### Across packages

A body can only be inlined if the importer has it, so inlinable bodies travel in
export data ([015](015-export-data.md)). The decision of whether a function is
inlinable is made when it is compiled, not when it is imported, so the exporter
decides and the importer obeys.

Inlining is applied transitively, bottom-up over the call graph, with a depth
limit. A callee that was itself produced by inlining is inlined at its
post-inlining cost, which is why the order is bottom-up.

## Devirtualization

An interface call whose dynamic type is known at compile time becomes a direct
call.

### Where the type comes from

| Source | Example |
| --- | --- |
| A concrete value assigned to an interface variable in the same function | `var w io.Writer = &buf; w.Write(p)` |
| A type assertion or type switch that dominates the call | `if f, ok := w.(*os.File); ok { f.Write(p) }` |
| An interface with exactly one implementing type in the program | whole-program only; excluded by design, see below |

The first two are the ones nanogo is to implement. Both are local reasoning over
the IR: a use of an interface value whose definition in the same function is a
conversion from a known concrete type.

The third requires the whole program and a closed world, which conflicts with
separate compilation, and it is excluded by design rather than deferred.

### Why it is worth having

A devirtualized call is a direct call, so it can be inlined, so
[023](023-escape-analysis.md) gets a summary instead of the conservative
"everything escapes" it applies to indirect calls. The chain matters more than
the removed indirect branch.

The interface method call in

```go
var b bytes.Buffer
var w io.Writer = &b
w.Write(p)
```

becomes `(*bytes.Buffer).Write(&b, p)`, becomes inlined, and `b` then has a
chance of staying on the stack. Without devirtualization it never does.

## Interaction, and the order

Devirtualization runs before inlining and again after it, because inlining
exposes assignments that were in another function. Two rounds, not a fixed point:
the third round's yield does not pay for its cost.

## Testing

None of this is written, because neither pass is. The oracles are listed because
they are external and free, and they should be wired up before the first
decision the compiler makes is one nobody can check:

- `gc`'s `-m` output annotates inlining decisions, and Go's `test/inline*.go`
  corpus asserts them. Same oracle as [023](023-escape-analysis.md).
- Differential execution with both passes off.
- Traceback tests: a panic inside an inlined function must name the inlined
  function and its caller, at the right lines.
- Devirtualization corpus: each of the two sources above, asserted in generated
  code, plus a negative case where the type is genuinely unknown.

## What was wrong

The spec said which of the three devirtualization sources nanogo implements and
which it does not. It implements none, so the table's rows say which are
designed for and which is excluded, and none of them is a description of code.
The state was found by grepping the whole repository for `Inlinable` and for
`NoInline`: the first has three mentions, a doc comment, a declaration, and one
assertion in `ir/node_test.go`, and the second is parsed by the flag table and
read by nobody.

The spec said export data is not written. It is written and read, by
`export/writer.go` and `export/read.go`. What it does not carry is a function
body, which is the thing cross-package inlining needs, and
[015](015-export-data.md) states that gap in its own terms.

The cost model's numbers, 57 for a call against a budget of 80, are the
reference implementation's and are recorded as a starting point to tune from.
Nothing has been tuned, because nothing has been measured.
