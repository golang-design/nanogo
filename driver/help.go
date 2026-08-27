// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import "runtime"

// Help is what "nanogo help" prints.
//
// It states the limits before it states the features. nanogo compiles a small
// part of Go, and a user who finds that out by hitting an error has already
// spent the afternoon.
//
// Every line here corresponds to a program that was compiled and run. The
// corpus is internal/audit/testdata/probes, one directory per construct, and
// run.sh in it reproduces the whole list. Prose does not re-run, so a claim
// with no probe behind it is a claim that will be wrong within a month: this
// text said defer, println and an empty function body were refused for weeks
// after each of them started working.
const Help = `nanogo is a Go compiler for arm64. It is under construction and it
compiles a small part of Go.

usage:
	nanogo build [-o output] [-v] [packages]   compile and link a program
	go build -toolexec=nanogo ./...            substitute nanogo in a build
	nanogo <tool> [arguments]                  one toolchain invocation
	nanogo version                             the pinned release and this build
	nanogo help                                this message

nanogo build:

	nanogo build .             compile the package here and link it
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

	The link configuration carries a modinfo line, so
	runtime/debug.ReadBuildInfo answers: the package path, the main
	module, every dependency module with its version and checksum, and
	the settings nanogo can state as fact, which are -buildmode,
	-compiler, CGO_ENABLED, GOARCH and GOOS. gc records more.
	DefaultGODEBUG, the GOARCH feature level such as GOARM64,
	GOEXPERIMENT and the vcs settings are absent, because nanogo passes
	none of them to anything and a recorded value would describe a
	program it did not build.

	So every build prints how many packages nanogo compiled, how many the
	toolchain did, and which tree the standard library came from. A build
	in which nanogo compiled one package of twenty-eight must not read as
	though nanogo built the program.

	A package named on the command line is never handed to gc. When nanogo
	cannot compile it the build stops and names the function, the position
	and the construct.

	One command may not name two packages where one imports the other.
	nanogo writes the import configuration before it compiles anything, so
	the second target has no packagefile entry for the first. That is an
	ordering nanogo build does not do yet, not a limit of the archive:
	build one of the two with the go command, or name one of them.

Target:

	nanogo emits arm64 machine code. A build for another GOARCH is refused
	before anything is compiled, naming the architecture asked for:

		nanogo cannot compile for this target: nanogo emits arm64
		machine code and the build is for amd64

	darwin/arm64 is the target the tests run on and the one to report a
	bug against.

What nanogo compiles:

	Integer arithmetic, comparisons, conversions between numeric types,
	indexing, and constants.

	Statements: return, assignment, :=, multi-value assignment such as
	a, b = b, a, if, for, switch including the expressionless form,
	fallthrough, break, continue, labels and goto.

	Calls: recursive, variadic, methods on a value and on a pointer
	receiver, a call whose results are several registers wide, and a call
	into a package gc compiled. A function whose body is empty compiles.

	Slices: a slice literal, make of a slice of int, len, index and slice
	expressions, and range over a slice or over an integer.

	Strings: a literal, len, concatenation, indexing, comparison, and a
	string as a parameter or a result.

	Structs declared in the package being compiled: a composite literal,
	reading and writing a field, and passing one by value. A struct up to
	four machine words is returned by value; wider is the first entry
	under "What nanogo does not announce" below.

	new, and reading and writing through the pointer.

	Package-level variables, including one whose initialiser is an
	expression, a string, or a slice literal. Package initialisation runs:
	an init function of the compiled package and of every package it
	imports, so a package variable such as os.Stdout is not nil.

	defer and go, arguments included. The operands are evaluated where
	the statement is written, and deferred calls run in reverse order,
	including calls deferred in a loop.

	A closure, capturing or not. A capture is by reference, so the
	literal and the function around it share one variable, and a literal
	that outlives the frame that made it keeps its captures.

	A declared function used as a value: passed to a function, returned
	from one, or assigned to a variable and called through it.

	Interfaces. A value of a concrete type goes into an interface with
	methods, a call through such an interface reaches the method, and an
	assertion or a type switch on it names a concrete type or another
	interface. A value also converts from one interface to another. The
	empty interface works the same way. What the two forms carry differs:
	an empty interface leads with the dynamic type's descriptor and one
	with methods leads with the itab of that pair (specs/032).

	Which itab implements an interface is not known until the value is,
	so an assertion to one, a type switch case that names one, and a
	conversion between two of them are a call to runtime.typeAssert or
	runtime.interfaceSwitch. Each call reads a cache nanogo writes into
	the object and the runtime fills in as the program runs.

	print and println of integers, strings and booleans, with any number
	of operands.

	The archive nanogo writes carries export data, so a package nanogo
	compiled can be imported: gc compiles a package that imports it and
	the program runs (specs/015-export-data.md).

What nanogo refuses, by name, with the reason:

	defer of print or println. A builtin is not a function value, so there
	is nothing to hand the runtime.

	A generic function.

	Taking the address of a variable the compiler keeps in a register.

	A package with assembly in it. An assembly definition uses ABI0 and a
	Go call uses ABIInternal, and nanogo generates no wrapper between them.

	A package with a go:embed directive in it, naming the patterns the
	directive binds. Reading -embedcfg, resolving the patterns and
	building the embed.FS structure is unbuilt, and a variable nanogo
	compiled would be its zero value at run time.

	A package that imports "C". Decision 8 of specs/000-decisions.md puts
	cgo out of scope. Instrumentation such as -race goes to gc, because
	nanogo does not implement the flags it needs.

	A function whose result the sixteen result registers cannot hold, and
	a wide result the call site does not write to one place, such as
	return g() or h(g()). A result that the registers do hold compiles,
	however many of them it takes: the four-register bound was a bound on
	decomposition and not on the convention (specs/030-abi.md).

What nanogo does not announce:

	Nothing the probe corpus reaches. Every program in it that nanogo
	compiles behaves the way the same program compiled by gc behaves. A
	corpus is a sample, so this is a measurement and not a proof, and the
	sample is 95 programs compiled twice and run twice.

	Two limits it cannot sample for you. A pointer to a local whose
	address escapes its frame outlives that frame, because escape
	analysis is unbuilt (specs/023-escape-analysis.md). A pointer store
	emits no write barrier, so a collection concurrent with a store may
	free memory that is still reachable (specs/034-write-barriers.md).
	Neither shows up as a wrong answer in a short program that does not
	collect, which is why the corpus does not carry them.

What a nanogo-compiled package cannot do:

	Report a position inside an imported package. gc's second line, "other
	declaration of New" naming a file under GOROOT, is missing from
	nanogo's diagnostic, because an imported declaration has no position
	in the file set of the package being compiled.

Environment:

	NANOGOROOT        the nanogo distribution to take the standard library
	                  from. Unset means the tree the nanogo binary is
	                  installed in, and then the installed Go toolchain.
	                  nanogo-dist build writes such a tree. It carries the
	                  packages the smallest Go program needs, every one of
	                  them compiled by gc, and a program that imports one
	                  it does not carry is refused by name with the count.
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
// cache key. This one is for a person, so it names the host as well.
func HumanVersion() string {
	host := runtime.GOOS + "/" + runtime.GOARCH
	// The object header nanogo writes is the host toolchain's, so the
	// operating system it targets is the host's. The architecture is not:
	// nanogo emits arm64 wherever it runs, and a build for any other GOARCH
	// is refused before a function is compiled.
	return "nanogo " + BuildIdentity() +
		" (objects for " + runtime.GOOS + "/" + TargetArch +
		", compatible with " + PinnedGoVersion +
		", host " + host + ")"
}
