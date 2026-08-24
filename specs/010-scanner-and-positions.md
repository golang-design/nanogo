---
title: "Source positions and the scanner"
status: complete
layer: front end
gate: G1
depends_on:
  - 002-architecture.md
---

# Positions and the scanner

Positions are threaded through every stage of [002](002-architecture.md), so
their representation is decided once, here, before anything produces one. The
scanner is specified in the same file because the two are built together and the
scanner is the only producer of a primary position.

## Positions

### The requirement

A position must answer four questions: which file, which line, which column, and
what the `//line` directives in force say the answer should be. It must do so
while being small, because there is one in almost every IR node and SSA value,
and a compiler holds millions.

### Representation

A position is a single `uint32`.

```go
type Pos uint32   // an index into the Fileset's coordinate space; 0 means unknown
```

The `Fileset` assigns each file a half-open range of the `uint32` space, sized to
the file's byte length. A `Pos` is `base + byteOffset`. Resolving a `Pos` to a
file is a binary search over bases; resolving to a line is a binary search over
that file's line-start table, which the scanner fills as it consumes newlines.

For a file with base $b$ and line starts $\ell_0 < \ell_1 < \dots < \ell_{n-1}$,
a position $p$ has

$$
\mathrm{line}(p) = \max\{\, i : \ell_i \le p - b \,\} + 1
\qquad
\mathrm{col}(p) = (p - b) - \ell_{\mathrm{line}(p)-1} + 1
$$

Columns are counted in bytes, not runes, matching `gc`. A rune count would be
correct in a different way and would disagree with every `errorcheck` file in
[004](004-conformance.md)'s L2 corpus.

### `//line` directives

`//line file:line[:col]` and `/*line file:line:col*/` rewrite the position of
everything after them. Generated code depends on this, and the runtime's
generated files use it, so it is not optional.

The rewrite is stored out of band. Each file holds a sorted list of *bases*: a
byte offset where a directive took effect, plus the filename, line, and column it
asserts. Resolution finds the last base at or before the offset and computes the
reported position relative to it. The raw `Pos` is untouched, so a directive
never costs anything at the point of use.

Two positions are therefore distinguished throughout the compiler:

- the **raw** position, which orders tokens and is what the compiler compares;
- the **reported** position, which is what a diagnostic or a DWARF line entry
  prints.

Confusing them is the standard bug here. The rule is that comparison uses raw and
printing uses reported, and no API returns both from one call.

### Unknown positions

`Pos(0)` is the unknown position. Synthesised nodes get the position of whatever
they were synthesised for, never `0`, because a `0` in a line table produces a
debugger that steps into nothing. [046](046-debug-info.md) states the consequence.

## The scanner

A hand-written scanner over a `[]byte` holding the whole file. No `io.Reader`,
no buffering: files are read whole, the corpus is on disk, and the simplification
is worth more than the memory.

### Input encoding

UTF-8. A leading byte order mark is skipped. An invalid UTF-8 sequence, a NUL
byte, and a byte order mark anywhere but the first position are errors, matching
the specification's requirement that source is UTF-8 text.

The ASCII byte is decoded inline and `utf8.DecodeRune` runs only where a
multibyte rune is possible. This is the hottest loop in the front end and the
branch is worth it.

The spec said decoding is by hand rather than through `utf8.DecodeRune`. The
code calls `utf8.DecodeRune` in `skipCh` and in `atIdentChar` and decodes only
the ASCII byte itself. This was found when the scanner was read for this audit.
The sentence was absolute where the code is a split, and the code's own comment
already stated the split correctly.

### Token set

The specification's token set, unchanged: identifiers, keywords, operators and
punctuation, and four literal kinds. A token is a small integer plus, for
literals and identifiers, the text as a `string`.

**The spec claimed two optimisations that the code does not have.** It said the
token text is a subslice of the input with no allocation per token, and that
identifiers are interned into a string table so that later comparison is
pointer comparison. Neither is true. `Scanner.Lit` is a `string`, and both the
producers build it with `string(s.src[s.tokOff:s.off])`, which allocates:
`ident` for a name, `setLit` for every literal. No string table exists anywhere
in `syntax`. This was found by reading `ident` for this audit.
The claims are removed rather than turned into work, because the corpus gate
passes without them and neither has been measured as a cost.

### Semicolon insertion

The specification inserts a semicolon at the end of a line whose final token is
one of: an identifier; an integer, floating-point, imaginary, rune, or string
literal; `break`, `continue`, `fallthrough`, `return`; `++`, `--`, `)`, `]`, `}`.

Two consequences are easy to get wrong and are called out because
[004](004-conformance.md)'s L1 level will find them either way:

1. A general comment `/* ... */` that contains a newline acts as a newline for
   this rule. A general comment without one does not.
2. The rule is applied by the scanner, which therefore must know the previous
   token. The parser never inserts a semicolon.

### Literals

Numeric literals carry the full modern syntax and each part of it is a corner
that the corpus tests:

| Form | Note |
| --- | --- |
| `0b`, `0o`, `0x` prefixes | including `0` followed by digits as octal |
| `_` separators | legal only between digits, and not adjacent to another `_`, not leading, not trailing |
| hexadecimal floats | `0x1.8p3`, exponent mandatory, `p` not `e` |
| imaginary suffix `i` | legal on every numeric form |

Invalid separator placement is a scanner error and not a parser error, so that
its position is the separator.

Rune and string literals: the escape set of the specification, with the checks
that `\u` and `\U` reject surrogate halves and values above `0x10FFFF`, and that
`\x` and octal escapes in a *string* produce bytes while in a *rune* they must
produce a valid code point. Raw strings drop carriage returns.

### Comments the compiler must keep

Most comments are discarded. Three kinds are not, and the scanner routes them
rather than the parser, because two of them change the meaning of positions and
one changes the meaning of the next declaration:

| Comment | Consumer |
| --- | --- |
| `//line` and `/*line*/` | this spec, above |
| `//go:` directives, `//go:build` among them | [016](016-directives-and-pragmas.md) |
| `// +build` | the same handler, by a second prefix |

A `//go:` comment binds to the next declaration and is attached by the parser,
so the scanner's job is only to hand it over with its position rather than drop
it.

**The table used to route `//export`, `// +build` and `//go:build` to
[014](014-package-loader.md).** Two of those three rows were wrong. `lineComment`
has no case for `//export`, and the loader never reads the scanner. It reads a
file's header comments itself, in `parseFileHeader` in `loader/constraint.go`,
because it decides which files are in a package before anything is parsed.
`lineComment` routes exactly two prefixes to the pragma handler, `go:` and
` +build`. This was found by reading `lineComment` against the loader for this
audit.

### Errors

The scanner reports and continues. It never panics and never stops at the first
error, because L1 agreement in [004](004-conformance.md) compares first errors
and a scanner that aborts cannot be compared on a file with two.

The error limit and the reporting interface belong to
[052](052-diagnostics.md).

## Testing

All three are built and gated. `syntax` is above the 90% coverage gate, and CI
sets
`NANOGO_REQUIRE_CORPUS=1`, which makes a missing corpus fail instead of skip.

- Token-stream equality against `go/scanner`. 19,674 of 19,691 files compared,
  17 skipped because `go/scanner` rejects them, 0 failures
  (`syntax/scanner_test.go`). Different messages are allowed; different token
  streams are not. The reverse direction holds over the corpus today as well:
  no file that `go/scanner` rejects is read without an error.
- Position round-trip against `go/token`, including under `//line` and
  `/*line*/`.
- A rejection corpus of invalid literals, invalid UTF-8, and misplaced
  separators, each pinned to an exact position.
