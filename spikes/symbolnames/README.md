# symbolnames

**Question.** Can text Plan 9 assembly define the symbol names the compiler and
the linker use?

**Answer.** No. The assembler rejects any name that contains a colon, and the
compiler and linker namespace is built on that character.

## Running it

```
go build -o symbolnames .
./symbolnames        # target=42 ptrdata=0x...
```

`s_arm64.s` assembles and links. It shows what a hand-written `DATA` word can
do: hold a constant, and hold a relocation to another symbol, which is what a
type descriptor or an itab needs.

## The file that must not assemble

`s2_arm64.s.txt` is the counter-example, and it is kept with a `.txt` suffix on
purpose. Its whole value is that it does **not** assemble, so a `.s` suffix
would break `go build ./...` in this directory and the spike could not be run
at all. It is text, not source.

Assemble it by hand to see the failure:

```
tmp=$(mktemp -d)
cp s2_arm64.s.txt "$tmp/s2_arm64.s"
go tool asm -I "$(go env GOROOT)/src/runtime" -o /dev/null "$tmp/s2_arm64.s"
```

```
s2_arm64.s:2: expect two operands for DATA
s2_arm64.s:3: expect two or three operands for GLOBL
```

The name is `type:main.Obj`. The assembler lexer stops at the colon.

## Why it matters

The compiler and the linker share a namespace built on that character: `type:*`
for type descriptors, `go:itab.*` for itabs, `go:string.*` for string data. The
linker scans those prefixes to build `runtime.typelinks` and
`runtime.itablinks`. A text-assembly emitter cannot write those names, so it
cannot register its itabs, and a dynamic interface conversion would build a
second itab for a pair that already has one. Itab pointer equality then breaks,
which is a correctness failure and not a limitation.

This is the evidence for
[`specs/000-decisions.md`](../../specs/000-decisions.md) decision 3: the seam is
object files, not assembly text.
