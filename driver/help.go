// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import "runtime"

// Help is what "nanogo help" prints.
//
// It states the limits before it states the features. nanogo compiles a small
// part of Go, and a user who finds that out by hitting an error has already
// spent the afternoon. Every line here is measured against what the compiler
// does today, and the tests in internal/e2e keep it that way.
const Help = `nanogo is a Go compiler for one target: darwin/arm64.

usage:
	nanogo build [-o output] [-v] [packages]   compile and link a program
	go build -toolexec=nanogo ./...           substitute nanogo in a go build
	nanogo <tool> [arguments]                  one toolchain invocation
	nanogo version                             the pinned release and this build
	nanogo help                                this message

nanogo build:

	nanogo build .             compile the package in this directory and link it
	nanogo build ./hello.go    compile the named files and link them
	nanogo build -o hello .    write the executable to hello
	nanogo build -v .          report each package as it is compiled
	nanogo build -work .       keep the scratch directory and print it

	nanogo compiles the packages named on the command line and nothing
	else. Say plainly what that leaves:

	The standard library and the runtime come from the installed Go
	toolchain. nanogo compiles neither. A Go program cannot start without
	a scheduler, an allocator and a collector, so even an empty main
	function needs about thirty packages, and gc builds all of them.
	nanogo asks the go command for those archives and reads them.

	The executable is written by go tool link. nanogo has no linker:
	specs/045-linker.md is G2 work and unbuilt.

	So every build prints how many packages nanogo compiled and how many
	the toolchain did. A build in which nanogo compiled one package of
	twenty-eight must not read as though nanogo built the program.

	A package named on the command line is never handed to gc. When nanogo
	cannot compile it the build stops and names the function, the position
	and the construct.

	One target, darwin/arm64. One package nanogo compiles may not import
	another package the same command compiles, because nanogo writes no
	export data.

What nanogo compiles:

	One target. nanogo emits arm64 machine code. It refuses to compile on
	any other host.
	A package that imports. nanogo reads gc's export data, so an import
	resolves to what the archive names for it declares, and a call reaches
	the symbol that package defines.
	Functions whose bodies are return statements, assignments and short
	variable declarations, if, for, range over a slice, switch with
	fallthrough, labels, goto, break, continue, calls, methods, variadic
	calls, arithmetic, comparisons, conversions, indexing, slice
	expressions, len, make of a slice, and slice and struct composite
	literals.

What nanogo refuses, by name, with the reason:

	A closure, defer, panic, append, a string literal, a conversion to
	an interface, and a map composite literal. Nothing performs the
	lowering specs/020-ir.md describes for them, so each one reaches SSA
	construction intact and is refused there.
	A function whose body is empty. nanogo cannot tell an empty body from
	a declaration that has none, so it reports one as a missing body.
	A multi-value assignment whose call returns a value wider than a
	machine register.
	A package with assembly in it. An assembly definition uses ABI0 and a
	Go call uses ABIInternal, and nanogo generates no wrapper between them.
	A package with package-level variables or an init function.
	go:embed, cgo, the runtime, and instrumentation such as -race.

What a nanogo-compiled package cannot do:

	Be imported. The archive nanogo writes holds the object and no
	export data, so gc cannot compile a package that imports it. nanogo
	reads export data and does not write it, so it takes packages from
	the top of the import graph downwards.

	Report a position inside an imported package. An imported
	declaration has no position in the file set of the package being
	compiled, so a diagnostic about one says the position is unknown.

	Carry build information. nanogo writes no modinfo line, so
	runtime/debug.ReadBuildInfo finds nothing in the executable.

Environment:

	NANOGOROOT        the nanogo distribution to take the standard library
	                  from. Unset means the tree the nanogo binary is
	                  installed in, and then the installed Go toolchain. No
	                  distribution is built today, so every build reaches the
	                  third of those and uses gc-built archives.
	NANOGO_ALLOWLIST  a file that lists the import paths nanogo compiles,
	                  one per line, # starts a comment. It selects packages
	                  in a go build -toolexec=nanogo run and has no effect on
	                  nanogo build. Unset means nanogo compiles nothing and
	                  every package reaches gc.
	NANOGO_LOG        a file that receives one line per compile invocation:
	                  compiled, delegated or failed, and the package. A
	                  -toolexec build looks the same either way, so this is
	                  how such a run reports what the allowlist selected.

Flags of the tool form:

	-fallback         hand every package to gc, whatever the allowlist says

In the tool form the go command runs nanogo in place of each toolchain
invocation. nanogo compiles the packages on its allowlist and runs the real
tool for everything else, so such a build is part nanogo and part gc. nanogo
accepts the flags the go command sends to gc; a flag it does not implement
sends the package to gc rather than producing an object that silently differs
from the one the build asked for.

Documentation: specs/050-driver.md and specs/051-build-integration.md.
`

// HumanVersion is the line "nanogo version" prints.
//
// It is not the -V=full line. That one answers a protocol: the go command
// parses it into the compiler's build ID and a fourth field would change every
// cache key. This one is for a person, so it names the host as well, which is
// what decides whether nanogo can compile anything at all.
func HumanVersion() string {
	host := runtime.GOOS + "/" + runtime.GOARCH
	// The object header nanogo writes is the host toolchain's, so the
	// operating system it targets is the host's. The architecture is not:
	// nanogo emits arm64 wherever it runs, which is why a host that is not
	// arm64 cannot compile anything.
	return "nanogo " + BuildIdentity() +
		" (objects for " + runtime.GOOS + "/" + TargetArch +
		", compatible with " + PinnedGoVersion +
		", host " + host + ")"
}
