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
	go build -toolexec=nanogo ./...      compile a build, package by package
	nanogo <tool> [arguments]            one toolchain invocation
	nanogo version                       the pinned release and this build
	nanogo help                          this message

The go command runs nanogo in place of each toolchain invocation. nanogo
compiles the packages on its allowlist and runs the real tool for everything
else, so a build is part nanogo and part gc.

What nanogo compiles:

	One target. nanogo emits arm64 machine code. It refuses to compile on
	any other host.
	A package that imports. nanogo reads gc's export data, so an import
	resolves to what the archive -importcfg names for it declares, and a
	call reaches the symbol that package defines.
	Functions whose bodies are return statements, assignments and short
	variable declarations, if, for, switch with fallthrough, labels, goto,
	break, continue, calls, arithmetic, comparisons, conversions and
	indexing.

What nanogo refuses, by name, with the reason:

	A composite literal, a range statement, a closure, defer, panic, a
	slice expression, a conversion to an interface, and most builtins
	including len, make and append. Nothing performs the lowering
	specs/020-ir.md describes for them, so each one reaches SSA
	construction intact and is refused there.
	A multi-value assignment whose call returns a value wider than a
	machine register.
	A package with assembly in it. An assembly definition uses ABI0 and a
	Go call uses ABIInternal, and nanogo generates no wrapper between them.
	A package with package-level variables or an init function.
	go:embed, the runtime, and instrumentation such as -race.

What a nanogo-compiled package cannot do:

	Be imported. The archive nanogo writes holds the object and no
	export data, so gc cannot compile a package that imports it. nanogo
	reads export data and does not write it, so it takes packages from
	the top of the import graph downwards.

	Report a position inside an imported package. An imported
	declaration has no position in the file set of the package being
	compiled, so a diagnostic about one says the position is unknown.

Environment:

	NANOGO_ALLOWLIST  a file that lists the import paths nanogo compiles,
	                  one per line, # starts a comment. Unset means nanogo
	                  compiles nothing and every package reaches gc.
	NANOGO_LOG        a file that receives one line per compile invocation:
	                  compiled, delegated or failed, and the package. A
	                  build looks the same either way, so this is how a run
	                  reports what the allowlist selected.

Flags:

	-fallback         hand every package to gc, whatever the allowlist says

nanogo accepts the flags the go command sends to gc. A flag it does not
implement sends the package to gc rather than producing an object that
silently differs from the one the build asked for.

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
