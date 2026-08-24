<!--
Copyright 2026 The golang.design Initiative Authors.
All rights reserved. Use of this source code is governed by
a BSD-style license that can be found in the LICENSE file.
-->

# Vendored upstream sources

This directory holds the unmodified sources that nanogo's type checker is
generated from. See [specs/012-type-checking.md](../../specs/012-type-checking.md)
for why the checker is forked and not written.

## What is here

| Directory | Upstream path in the Go repository |
| --- | --- |
| `types2/` | `src/cmd/compile/internal/types2/` |
| `errors/` | `src/internal/types/errors/` |
| `testdata/` | `src/internal/types/testdata/` and `src/cmd/compile/internal/types2/testdata/` |

`errors` comes across because `types2` dot-imports it for the error codes.
[specs/012](../../specs/012-type-checking.md) requires nanogo to carry the
upstream error codes, so the package is vendored rather than rewritten.

`testdata` is the checker's error corpus: 374 Go files annotated with the errors
the checker must report at the positions it must report them. It is the reason
the fork is safe, so it comes across with the sources. `types2/errorcheck_test.go`
runs it.

## Upstream revision

| Field | Value |
| --- | --- |
| Repository | https://go.googlesource.com/go |
| Revision | `c97cfcb37fced87a43a3dbab8983d6f76b8b84d1` |
| Date | Sat Aug 22 19:48:18 2026 -0700 |

To refresh, copy the upstream files again, update the table above, then run the
generator (see below). The generator fails when a rewrite no longer applies, so
an upstream change that touches a ported line is reported and not lost.

## Why the files end in `.txt`

Every file keeps its upstream name with `.txt` appended. `check.go` is stored as
`check.go.txt`.

The suffix keeps the directory out of the build. A directory of `.go` files that
declares `package types2` and imports `cmd/compile/internal/syntax` does not
compile in this module, so `go build ./...` and `go vet ./...` would fail on it.
The alternatives were rejected:

- A `//go:build ignore` line changes the file, and this directory must stay
  byte-identical to upstream.
- A directory name that Go tools skip, such as `_upstream`, hides the sources
  from a plain `ls` and reads as private, which the sources are not.

The suffix keeps the upstream name visible, so a diff against the Go tree is a
straight file-to-file comparison.

The corpus under `testdata/` carries the same suffix, for a second reason as
well: much of it is deliberately malformed Go, and CI runs `gofmt -l` over the
whole tree.

## Regenerating

```
go test ./types2/gen/ -run TestGenerate -write   # write the generated files
go test ./types2/gen/                            # fail if the tree has drifted
```

## Rules

1. **Nothing here may be edited by hand.** Every file is a verbatim upstream
   copy, including the BSD copyright headers.
2. Every change nanogo needs goes in the rewrite table in
   [`../gen/gen.go`](../gen/gen.go), or the file becomes a hand-ported file in
   `types2/`. There is no third option.
3. The generated output in `types2/` is not edited by hand either. `TestGenerate`
   fails when it drifts.
