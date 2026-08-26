---
title: "The parser and the syntax tree"
status: complete
layer: front end
gate: G1
depends_on:
  - 000-decisions.md
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
| `Object` resolution, partially done and deprecated | no name resolution; [012](012-type-checking.md) owns it |
| source fidelity guaranteed | not guaranteed; the tree is not printed back |
| node set sized for tooling | node set sized for a compiler |

`cmd/compile/internal/syntax` exists in the reference implementation for exactly
these reasons and is 7,522 lines. That is the size to expect.

The row says *name* resolution, and the word is exact. One resolution does
happen in the parser: under `CheckBranches` mode, `checkBranches` resolves the
target of every `break`, `continue`, `goto` and `fallthrough` and reports one
that has none. It is here and not in [012](012-type-checking.md) because it
needs only the tree, and because a branch with no target makes every later
statement about control flow wrong. Upstream's `syntax` package draws the line
in the same place.

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

### Directives are attached, not parsed

A `//go:` comment never reaches the tree as a comment. The scanner hands each
one to the parser with its position ([010](010-scanner-and-positions.md)), the
parser accumulates them in `p.pragma`, and `takePragma` attaches the set to the
declaration that follows. Every declaration node carries a `Pragma`, and so
does `File`, for a directive that stands before the package clause. A form that
accepts no directive calls `clearPragma`, which hands the pending set back to
the handler unclaimed rather than dropping it.

The parser decides neither which verbs exist nor where a directive is legal.
Both belong to the handler, and [016](016-directives-and-pragmas.md) gives them
to the driver, which is why `Pragma` is an empty interface here.

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
heuristic with a proof obligation rather than a grammar rule.

After `[`, parse one expression, then re-analyse the tree that came out:

- `]` immediately: slice type.
- A single name before `]`: **always an array length**, whatever follows the
  `]`. `type B [N] int` is an array, and so is `type B [N]int`.
- A `,` at depth zero before `]`: type parameter list.
- A name followed by something that can **only** be a type: type parameter
  list. Only a constraint that cannot be an expression tilts the decision.
  `type B [P *E] struct{}` is an **array**, because `*E` reads equally well as
  a value expression; anything ambiguous needs the comma.

**There is no backtracking.** The expression is parsed once and the tree it
produced is re-analysed, in `extractName`. nanogo's `Scanner` offers no way to
save its state and no way to restore one, and the reference implementation does
not backtrack either.

The branch taken is recorded by `TypeDecl.TParamList` being non-nil, and
[012](012-type-checking.md) is allowed to reject the result. The obligation is
L1 agreement in [004](004-conformance.md), and it is met: 16,293 files, zero
undocumented disagreements.

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
2. **A node is never nil.** Every production returns a `BadExpr` or a `BadStmt`
   on failure, carrying the position range it consumed. The type checker skips
   bad nodes silently, so one syntax error produces one message rather than one
   message plus the type errors that follow from a hole.

   There is no `BadDecl` and none is needed. A declaration that fails is still
   a `TypeDecl`, `VarDecl` or `FuncDecl`, with a `BadExpr` in the part that
   could not be read, which is what `typeDecl` builds when it cannot read a
   type.

"One mistake, one message" is the goal and it is not always reachable. Three
inputs produce two messages in nanogo and in the reference parser alike:
`switch { case: }`, `var x map[int = 0`, and `var x, = 1`. Recovery skips the
token it could not read, and that token was the one the next production needed.

What is guaranteed, and what the tests assert, is the weaker pair: no two
messages ever share a position, and the message count matches the reference
parser's on the same input.

The parser reports in the order it meets a mistake, which is source order for
everything but `checkBranches`, and `checkBranches` runs when a function body
closes. Putting the whole list back into source order is the driver's job, not
the parser's: `driver.diagnostics` sorts by raw position before the error limit
is applied ([052](052-diagnostics.md)).

## Structure

Recursive descent, one method per production, named for the production.
Expression parsing is precedence climbing over the specification's five binary
precedence levels rather than one method per level, which removes four layers of
call for every expression parsed.

The parser holds one token of lookahead and no production needs more. The
array-against-type-parameter case above is decided by re-analysing an expression
that was already parsed, in `extractName`, not by looking further ahead.

## Testing

The first three are built and gated. `syntax` is above the 90% coverage gate
and CI sets
`NANOGO_REQUIRE_CORPUS=1`, so a missing corpus fails instead of skipping.

- L1 agreement over the distribution, per [004](004-conformance.md): accept the
  same files, reject the same files, at the same first position. 16,293 files
  compared against `go/parser`, with 14 documented exceptions. Each exception
  carries its reason, and an exception that stops firing is a failure, so the
  list cannot become a place to hide a bug.
- Position invariants over the same corpus: every node's position lies inside
  its parent's range, and positions do not decrease in source order. This
  catches the position bugs that no accept-or-reject comparison sees.
- The `errorcheck` files of Go's `test/` corpus that are syntax errors, checked
  for position and count.
- Fuzzing against `go/parser` on mutated distribution files is **not built**.
  `syntax` has no `Fuzz` function. The corpus comparison already covers every
  file in the distribution, so what fuzzing adds is inputs that no Go program
  contains, and that has not yet been worth the run time.

## What was wrong

**The `go/ast` table said the parser does no resolution at all.** It does one:
`checkBranches` resolves branch targets under `CheckBranches` mode. The row now
says "no name resolution", which is the rule that holds, and the section above
states the exception. This was found by reading `funcBody`.

**An earlier version of the array-against-type-parameter rule stated it wrongly
in three ways, and the implementation found all three.** It made a single name
before `]` conditional on what came after the `]`, which contradicts itself,
because `type B [N] int` and `type B [N]int` are both arrays. It said a name
followed by "something that starts a type" is a type parameter list, which is
too strong: `type B [P *E] struct{}` is an array, because `*E` reads equally
well as a value expression. And it described saving and restoring scanner
state, which neither the reference implementation nor nanogo does.

**The backtracking claim survived a second time, in the Structure section.**
The correction was written into the ambiguity section and not into the section
that repeated it, so the spec contradicted itself for as long as both were
read separately. Found by reading the two against `typeDecl` in one pass.

**The spec named a `BadDecl` node.** There is none, and none is needed. Found
by searching the node set. `syntax/parser.go`'s package comment named it too,
and now does not.

**The spec claimed "one mistake, one message".** No Go parser achieves it.
Three inputs produce two messages in nanogo and in the reference parser alike,
and the guarantee the tests assert is the weaker pair above.

**The spec opened with "Implements [000](000-decisions.md) decision 1" and did
not list `000-decisions.md` in `depends_on`.** [012](012-type-checking.md)
opens the same way and does list it. The front matter now matches the prose.
