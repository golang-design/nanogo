# spikes

Small experiments that answer a question a spec depends on. Each one is kept
because a normative decision cites it. They are separate modules, are not part
of `golang.design/x/nanogo`, and are not built by CI.

Run a spike from its own directory with the `go` tool of the day.

| Spike | Question | Answer |
| --- | --- | --- |
| [`stackmap`](stackmap) | Can text Plan 9 assembly express per-PC GC stack maps? | Yes. See [`specs/040-object-format.md`](../specs/040-object-format.md). |
| [`symbolnames`](symbolnames) | Can text Plan 9 assembly define the symbol names the compiler and linker use? | No. Names that contain `:` are rejected. |
| [`toolexec`](toolexec) | Can a foreign compiler be substituted per package by `go build -toolexec`, and with which flags? | Yes. See [`specs/051-build-integration.md`](../specs/051-build-integration.md); the flag set is larger than `-h` suggests. |

## stackmap

Three assembly functions share one frame shape and differ only in the stack map
they declare.

```
cd stackmap
go run . live    # local slot declared as a pointer:  0 finalizers, object survives
go run . dead    # NO_LOCAL_POINTERS:                 1 finalizer,  object collected
go run . multi   # two bitmaps chosen by PCDATA $1:   live at index 0, dead at index 1
```

`multi` is the decisive one. One function, one frame, two stack map bitmaps in a
`FUNCDATA` symbol written by hand, selected per call site by
`PCDATA $PCDATA_StackMapIndex`. The object survives the collection at index 0
and is collected at index 1, so the runtime reads the map that the text
assembly declared.

## symbolnames

`s_arm64.s` assembles and links. It shows that a hand-written `DATA` word can
hold a relocation to another symbol, which is what a type descriptor or an itab
needs.

`s2_arm64.s` does not assemble:

```
./s2_arm64.s:2: expect two operands for DATA
./s2_arm64.s:3: expect two or three operands for GLOBL
```

The name is `type:main.Obj`. The assembler lexer stops at the colon. The
compiler and the linker both use that namespace: `type:*`, `go:itab.*`,
`go:string.*`. A text-assembly emitter cannot write those names, so it cannot
produce type descriptors, itabs, or string data that the linker will place in
`runtime.typelinks` and `runtime.itablinks`.

## toolexec

A passthrough that logs `argv` and execs the real tool, so the build must still
succeed. It does, and the log is the answer.

```
cd toolexec
go build -o /tmp/passthrough .
cd <any module>
PT_LOG=/tmp/log.txt go build -a -toolexec=/tmp/passthrough .
```

The spike's own README has the measured flag set and the `-V=full` build-ID
protocol, which is the part that has no documentation outside
`cmd/go/internal/work/buildid.go`.
