---
title: "The parser and the syntax tree"
status: draft
layer: front end
gate: G1
depends_on:
  - 010-scanner-and-positions.md
---

# Parser and syntax tree

Implements [000](000-decisions.md) decision 1: the parser is written, not
forked. This spec says what the tree looks like, how the grammar's two genuine
ambiguities are resolved, and how errors are recovered from.

## Why not `go/ast`

`go/ast` is built for tooling that rewrites source. It keeps comments attached
to nodes, models parentheses as nodes so that `gofmt` can preserve them, and
guarantees enough fidelity to print the file back. Every one of those is a cost
the compiler pays and no benefit it takes.

A compiler wants the opposite tree:

| `go/ast` | nanogo's tree |
| --- | --- |
| comments attached to nodes | comments dropped, except the three kinds [010](010-scanner-and-positions.md) routes |
| `Object` resolution, partially done and deprecated | no resolution in the parser at all; [012](012-type-checking.md) owns it |
| source fidelity guaranteed | not guaranteed; the tree is not printed back |
| node set sized for tooling | node set sized for a compiler |

`cmd/compile/internal/syntax` exists in the reference implementation for exactly
these reasons and is 7,522 lines. That is the size to expect.

### The tree is shaped like the reference `syntax` package on purpose

This is a constraint, not a coincidence. [012](012-type-checking.md) ports the
type checker from `cmd/compile/internal/types2`, and 38 of that package's 69
files name the syntax tree directly. The port is mechanical only while the node
set it names has the same shape.

So nanogo's node set follows `cmd/compile/internal/syntax`: the same
discriminations, the same names where a name is not actively misleading. Where
nanogo wants a different shape, the cost is paid in [012](012-type-checking.md)
and the spec must say so. Nothing in this spec may diverge from that node set
casually.

`ParenExpr` is the example. A tree built only for lowering does not need it, and
the first draft of this spec dropped it. It is kept, because the checker uses it
in a dozen places, because the "no composite literal in a control clause header"
rule needs to know a parenthesis was there, and because dropping one node to pay
for a dozen edits in a ported 23,000-line checker is a bad trade.

## The tree

Nodes are structs, each embedding a `node` that carries a `Pos`. Interfaces
`Expr`, `Stmt`, and `Decl` are marker interfaces with a private method, so the
set is closed and a type switch over it is exhaustive by construction.

The node set follows the specification's production names. Deviations from
`go/ast` worth stating:

- **One `Operation` node** for unary and binary operators, discriminated by
  whether the second operand is nil. `gc` does this and it removes a duplicated
  half of the expression walker.
- **`CallExpr` carries the instantiation.** `f[int](x)` is a call whose function
  is an `IndexExpr`; the type checker rewrites it. The parser does not decide.
- **Composite literals keep their key form.** Whether a key is a field name, a
  constant index, or a map key is a typing question and is not answered here.

## The two ambiguities

Go's grammar is close to LL(1). Two places are not, and both arrived with
generics. Every other production is resolved by one token of lookahead.

### 1. Array type against type parameter list

In a type declaration, after the name, a `[` can begin either an array or slice
type, or a type parameter list:

```go
type A [N]int      // array of N ints
type B [N any] struct{}   // generic, type parameter N constrained by any
```

The prefix `[N` is common. Deciding needs lookahead past `N`, and `N` can be an
arbitrary expression in the array case.

The resolution is the one the reference implementation uses, and it is a
heuristic with a proof obligation rather than a grammar rule. After `[`, scan
ahead:

- `]` immediately: slice type.
- The token after the first expression is `]`: array type, unless the expression
  was a single identifier and the token after `]` starts a type — which is the
  `type B [N] int` case that the specification resolves as an array.
- A `,` at depth zero before `]`: type parameter list.
- An identifier followed by something that starts a type: type parameter list.

The parser records which branch it took, and [012](012-type-checking.md) is
allowed to reject the result. The obligation is L1 agreement in
[004](004-conformance.md): if nanogo and `go/parser` disagree on any file in the
distribution, the heuristic is wrong and the disagreement is the bug report.

### 2. Index against instantiation

`f[x]` is an index if `f` is a value and an instantiation if `f` is generic.
`f[x, y]` is only an instantiation, because indexing takes one operand.

The parser does not resolve this. It produces `IndexExpr` with a list of
operands, and the type checker rewrites the node once it knows what `f` is. This
is the same choice `go/parser` makes and it is the reason the tree has an
`IndexExpr` with a slice rather than a single index.

### The composite literal case that is not an ambiguity

```go
if x == T{}.f { }
```

is rejected because a composite literal may not appear as the operand of an `if`,
`for`, or `switch` header without parentheses. This is a context flag threaded
through expression parsing, not a lookahead problem. The flag is set when parsing
those headers and cleared inside any bracket or parenthesis.

## Error recovery

The parser reports an error and continues, because [004](004-conformance.md)'s
`errorcheck` corpus annotates several errors per file and a parser that stops at
the first cannot be run against it.

Recovery is by synchronisation on statement and declaration boundaries. On an
error inside a statement, discard tokens until a semicolon, `}`, or a token that
begins a declaration. On an error inside a declaration, discard until a token
that begins a declaration.

Two rules keep this from producing error cascades:

1. **At most one error per position.** A second error at a position already
   reported is dropped.
2. **A node is never nil.** Every production returns a `BadExpr`, `BadStmt`, or
   `BadDecl` on failure, carrying the position range it consumed. The type
   checker skips bad nodes silently, so one syntax error produces one message
   rather than one message plus the type errors that follow from a hole.

## Structure

Recursive descent, one method per production, named for the production.
Expression parsing is precedence climbing over the specification's five binary
precedence levels rather than one method per level, which removes four layers of
call for every expression parsed.

The parser holds one token of lookahead. The array-against-type-parameter case
above is the only place that needs more, and it uses a saved scanner state
restored on the other branch, not a token buffer.

## Testing

- L1 agreement over the distribution, per [004](004-conformance.md): accept the
  same files, reject the same files, at the same first position.
- A round-trip test: parse, print positions of every node, and check that node
  positions are non-decreasing in source order and inside their parent's range.
  This catches the position bugs that no output comparison sees.
- The `errorcheck` files of Go's `test/` corpus that are syntax errors, checked
  for position and count.
- Fuzzing against `go/parser` on mutated distribution files, comparing accept
  and reject only. A crash in either direction is a nanogo bug.
