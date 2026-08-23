# toolexec

**Question.** Can a foreign compiler be substituted for `cmd/compile` on a
per-package basis by `go build -toolexec`, and what does the `go` command
actually pass to it?

**Answer.** Yes, and the flag set is larger than `go tool compile -h` suggests.

## Running it

```
go build -o /tmp/passthrough .
cd <any module>
PT_LOG=/tmp/log.txt go build -a -toolexec=/tmp/passthrough .
```

The passthrough logs `argv` and execs the real tool, so the build must still
succeed. It does.

## What was measured

A two-package module, `go build -a` on `darwin/arm64`, Go 1.27.0:

| Tool | Invocations |
| --- | --- |
| `compile` | 59 |
| `asm` | 49 |
| `link` | 2 |

Substitution is per invocation, so a compiler can take some `compile` calls and
pass the rest through. That is the mechanism
[`specs/051-build-integration.md`](../../specs/051-build-integration.md)
depends on.

## The flags the go command actually sends

On **every** `compile` invocation:

```
-o -trimpath -p -lang -buildid -goversion -c -shared
-nolocalimports -importcfg -pack
```

Conditionally:

| Flag | When |
| --- | --- |
| `-complete` | the package has no assembly and no C (40 of 58 here) |
| `-symabis`, `-asmhdr` | the package has assembly (14 of 58 here) |
| `-std` | the package is in the standard library (56 of 58 here) |

Three of these are not in `go tool compile -h`'s obvious set and each would have
been missed:

- **`-shared` is sent on every invocation** on this platform. A compiler that
  rejects it, as an early draft of the driver spec proposed, rejects every build.
- **`-pack`** means the output is an archive, not a bare object file.
- **`-nolocalimports`** forbids relative import paths.

## The `-V=full` protocol

Before any compilation, the `go` command runs

```
<toolexec> <tool> -V=full
```

and parses the output to derive the tool's build ID, which becomes part of every
cache key. `cmd/go/internal/work/buildid.go` requires:

- at least three whitespace-separated fields;
- field 0 equal to the tool name — literally `compile`, not the substitute's own
  name;
- field 1 equal to `version`.

Malformed output is a fatal error, not a fallback. The real tool prints:

```
compile version go1.27.0
```

For a release version the whole line becomes the ID, so a substitute compiler
must append its own identity — `compile version go1.27.0 X:nanogo-<hash>` — or
the build cache will not notice when the compiler changes.
