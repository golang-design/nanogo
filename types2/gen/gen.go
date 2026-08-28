// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package gen generates nanogo's type checker from the vendored upstream
// sources in types2/upstream.
//
// The technique is the one the Go team uses to generate go/types from
// cmd/compile/internal/types2: parse nothing, rewrite the source, and let the
// compiler find what the rewrite missed. See go/types/generate_test.go and
// specs/012-type-checking.md.
//
// Two rewrite kinds exist here.
//
// A [rule] is a literal replacement applied to every generated file. Rules
// carry the changes that are uniform across the port, such as the import path
// of the syntax tree.
//
// Patches run before rules, so a patch matches upstream text: it names
// "cmd/compile/internal/syntax", not nanogo's import path.
//
// A [patch] is a literal replacement applied to one named file, and it must
// match the number of times the table says. A patch that stops matching is an
// error, not a silent no-op, so an upstream change that touches a ported line
// is reported instead of being dropped. This is the drift detector for
// everything that is not a rule.
//
// A file that cannot be rewritten this way is listed in [handPorted] and lives
// in types2/ as ordinary source. Upstream marks its own unportable files the
// same way.
package gen

import (
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// upstreamSuffix is appended to every vendored file name. See
// types2/upstream/README.md for why the sources are not stored as .go.
const upstreamSuffix = ".txt"

// rule is a literal replacement applied to every generated file.
type rule struct {
	// why states the reason the rewrite exists. specs/012-type-checking.md
	// calls the table the specification of the divergence, so an entry
	// without a reason is a hole in that specification.
	why string
	old string
	new string
}

// rules are the port-wide replacements.
var rules = []rule{
	{
		why: "the whole point of the port: the checker reads nanogo's syntax tree",
		old: `"cmd/compile/internal/syntax"`,
		new: `"golang.design/x/nanogo/syntax"`,
	},
	{
		why: "the external test package imports the checker under test, and it now lives here",
		old: `"cmd/compile/internal/types2"`,
		new: `"golang.design/x/nanogo/types2"`,
	},
	{
		why: "the error codes are vendored beside the checker; nanogo carries the upstream code set so that errorcheck patterns keep matching",
		old: `"internal/types/errors"`,
		new: `"golang.design/x/nanogo/types2/errors"`,
	},
}

// patch is a literal replacement applied to one file.
type patch struct {
	// why states the reason the rewrite exists.
	why string
	old string
	new string
	// n is the number of matches required. Zero means one.
	n int
}

// patches maps an upstream file name to the replacements it needs.
//
// The entries are grouped by the reason they exist. The groups are:
//
//   - Positions. nanogo's syntax.Pos is a bare uint32 resolved through a
//     FileSet, where upstream's Pos carries a *PosBase. Every site that calls
//     a method on a Pos, or prints one, needs the FileSet.
//   - Type info in the tree. nanogo's Expr carries no type-and-value record,
//     so Config.StoreTypesInSyntax has nothing to store into.
//   - Build environment. nanogo does not have the Go repository's internal
//     packages.
var patches = map[string][]patch{
	"api.go": {
		{
			why: "the checker cannot resolve a position without the FileSet the files were parsed with; there is no back pointer from a bare Pos",
			old: `type Config struct {
	// Context is the context used for resolving global identifiers. If nil, the
	// type checker will initialize this field with a newly created context.
	Context *Context
`,
			new: `type Config struct {
	// Fset resolves positions to files, lines, and columns. It must be the
	// FileSet the files were parsed with.
	//
	// nanogo's syntax.Pos is a bare offset with no back pointer to a file,
	// so the checker cannot print or compare a position without it. A nil
	// Fset is allowed and makes every reported position unknown, which is
	// what the position-free unit tests want.
	Fset *syntax.FileSet

	// Context is the context used for resolving global identifiers. If nil, the
	// type checker will initialize this field with a newly created context.
	Context *Context
`,
		},
		{
			why: "an Error must print its position after the FileSet is out of reach, so it carries the resolved position rather than a set pointer",
			old: `type Error struct {
	Pos  syntax.Pos // error position
	Msg  string     // default error message, user-friendly
	Full string     // full error message, for debugging (may contain internal details)
	Soft bool       // if set, error is "soft"
	Code Code       // error code
}

// Error returns an error string formatted as follows:
// filename:line:column: message
func (err Error) Error() string {
	return fmt.Sprintf("%s: %s", err.Pos, err.Msg)
}`,
			new: `type Error struct {
	Pos  syntax.Pos // error position
	Msg  string     // default error message, user-friendly
	Full string     // full error message, for debugging (may contain internal details)
	Soft bool       // if set, error is "soft"
	Code Code       // error code

	// Position is Pos resolved through Config.Fset, with any //line
	// directive applied. It is resolved when the error is reported, so an
	// Error prints itself with no reference to the FileSet.
	Position syntax.Position
}

// Error returns an error string formatted as follows:
// filename:line:column: message
func (err Error) Error() string {
	return fmt.Sprintf("%s: %s", err.Position, err.Msg)
}`,
		},
		{
			why: "the same resolved position, for FullError, which prints the debugging form of the message",
			old: `	return fmt.Sprintf("%s: %s", err.Pos, err.Full)`,
			new: `	return fmt.Sprintf("%s: %s", err.Position, err.Full)`,
		},
		{
			why: "nanogo's syntax tree has no per-file version field, so a file cannot override Config.GoVersion; the map is keyed by the file it applies to",
			old: `	FileVersions map[*syntax.PosBase]string`,
			new: `	FileVersions map[*syntax.SrcFile]string`,
		},
		{
			why: "nanogo's Expr has nowhere to store a type-and-value record, so the flag that asks for one is removed rather than left as a switch that does nothing",
			old: `	// If StoreTypesInSyntax is set, type information identical to
	// that which would be put in the Types map, will be set in
	// syntax.Expr.TypeAndValue (independently of whether Types
	// is nil or not).
	StoreTypesInSyntax bool

`,
			new: ``,
		},
		{
			why: "Info.recordTypes asked whether either recording channel was open; only Info.Types is left",
			old: `	return info.Types != nil || info.StoreTypesInSyntax`,
			new: `	return info.Types != nil`,
		},
		{
			why: "TypeOf's documented precondition names a field this package no longer has",
			old: `// Precondition 1: the Types map is populated or StoreTypesInSyntax is set.`,
			new: `// Precondition 1: the Types map is populated.`,
		},
		{
			why: "nanogo's Expr carries no type-and-value record; recording into the tree is dropped, so reading it back is too",
			old: `	if info.Types != nil {
		if t, ok := info.Types[e]; ok {
			return t.Type
		}
	} else if info.StoreTypesInSyntax {
		if tv := e.GetTypeInfo(); tv.Type != nil {
			return tv.Type
		}
	}
`,
			new: `	if info.Types != nil {
		if t, ok := info.Types[e]; ok {
			return t.Type
		}
	}
`,
		},
	},

	"api_test.go": {
		{
			why: "the harness in harness_test.go owns these helpers now: nanogo's Parse takes a *SrcFile from a FileSet, and the Config must name that FileSet, which the upstream copies cannot do",
			old: `// nopos indicates an unknown position
var nopos syntax.Pos

func mustParse(src string) *syntax.File {
	f, err := syntax.Parse(syntax.NewFileBase(pkgName(src)), strings.NewReader(src), nil, nil, 0)
	if err != nil {
		panic(err) // so we don't need to pass *testing.T
	}
	return f
}

func typecheck(src string, conf *Config, info *Info) (*Package, error) {
	f := mustParse(src)
	if conf == nil {
		conf = &Config{
			Error:    func(err error) {}, // collect all errors
			Importer: defaultImporter(),
		}
	}
	return conf.Check(f.PkgName.Value, []*syntax.File{f}, info)
}

func mustTypecheck(src string, conf *Config, info *Info) *Package {
	pkg, err := typecheck(src, conf, info)
	if err != nil {
		panic(err) // so we don't need to pass *testing.T
	}
	return pkg
}

// pkgName extracts the package name from src, which must contain a package header.
func pkgName(src string) string {
	const kw = "package "
	if i := strings.Index(src, kw); i >= 0 {
		after := src[i+len(kw):]
		n := len(after)
		if i := strings.IndexAny(after, "\n\t ;/"); i >= 0 {
			n = i
		}
		return after[:n]
	}
	panic("missing package header: " + src)
}

`,
			new: ``,
		},
		{
			why: "the test asserts that a //go:build line too new for this checker is reported per file",
			old: `func TestTooNew(t *testing.T) {
`,
			new: `func TestTooNew(t *testing.T) {
	t.Skip("known gap: " + gapFileVersion)

`,
		},
		{
			why: "the test reads a version error out of a file whose version came from a //go:build line",
			old: `func TestVersionWithoutPos(t *testing.T) {
`,
			new: `func TestVersionWithoutPos(t *testing.T) {
	t.Skip("known gap: " + gapFileVersion)

`,
		},
		{
			why: "the test sets a per-file version with a //go:build line",
			old: `func TestFileVersions(t *testing.T) {
`,
			new: `func TestFileVersions(t *testing.T) {
	t.Skip("known gap: " + gapFileVersion)

`,
		},
		{
			why: "internal/goversion and internal/testenv are not importable outside the Go repository",
			old: `import (
	"cmd/compile/internal/syntax"
	"errors"
	"fmt"
	"internal/goversion"
	"internal/testenv"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	. "cmd/compile/internal/types2"
)`,
			new: `import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"cmd/compile/internal/syntax"
	. "cmd/compile/internal/types2"
)`,
		},
		{
			why: "testenv_test.go has the one check these call",
			old: `	testenv.MustHaveGoBuild(t)`,
			new: `	mustHaveGoBuild(t)`,
			n:   5,
		},
		{
			why: "a bare Pos has no line or column; the harness resolves it",
			old: `	target := int(pos.Line())`,
			new: `	target := int(position(pos).Line)`,
		},
		{
			why: "a bare Pos has no column either; indexFor walks the source to the reported position",
			old: `	return i + int(pos.Col()-1) // columns are 1-based`,
			new: `	return i + int(position(pos).Col-1) // columns are 1-based`,
		},
		{
			why: "internal/goversion is not importable; the harness carries the constant",
			old: `	goversion := fmt.Sprintf("go1.%d", goversion.Version)`,
			new: `	goversion := fmt.Sprintf("go1.%d", goVersionMinor)`,
		},
		{
			why: "the per-file version map is keyed by file, and nanogo names a file *syntax.SrcFile",
			old: `		versions := make(map[*syntax.PosBase]string)`,
			new: `		versions := make(map[*syntax.SrcFile]string)`,
		},
	},

	"check.go": {
		{
			why: "the version map is keyed by file, and nanogo names a file *syntax.SrcFile",
			old: `	versions      map[*syntax.PosBase]string // maps files to version strings (each file has an entry); shared with Info.FileVersions if present; may be unaltered Config.GoVersion`,
			new: `	versions      map[*syntax.SrcFile]string // maps files to version strings (each file has an entry); shared with Info.FileVersions if present; may be unaltered Config.GoVersion`,
		},
		{
			why: "the same key change, where Checker.initFiles allocates the version map",
			old: `		versions = make(map[*syntax.PosBase]string)`,
			new: `		versions = make(map[*syntax.SrcFile]string)`,
		},
		{
			why: "nanogo's File has no GoVersion field, so every file takes Config.GoVersion; the per-file //go:build downgrade is reported as a syntax gap, not worked around",
			old: `		// If the file specifies a version, use max(fileVersion, go1.21).
		if fileVersion := asGoVersion(file.GoVersion); fileVersion.isValid() {
			// Go 1.21 introduced the feature of allowing //go:build lines
			// to sometimes set the Go version in a given file. Versions Go 1.21 and later
			// can be set backwards compatibly as that was the first version
			// files with go1.21 or later build tags could be built with.
			//
			// Set the version to max(fileVersion, go1.21): That will allow a
			// downgrade to a version before go1.22, where the for loop semantics
			// change was made, while being backwards compatible with versions of
			// go before the new //go:build semantics were introduced.
			v = string(versionMax(fileVersion, go1_21))

			// Report a specific error for each tagged file that's too new.
			// (Normally the build system will have filtered files by version,
			// but clients can present arbitrary files to the type checker.)
			if fileVersion.cmp(go_current) > 0 {
				// Use position of 'package [p]' for types/types2 consistency.
				// (Ideally we would use the //build tag itself.)
				check.errorf(file.PkgName, TooNew, "file requires newer Go version %v", fileVersion)
			}
		}
		versions[file.Pos().FileBase()] = v // file.Pos().FileBase() may be nil for tests`,
			new: `		// nanogo's syntax.File has no GoVersion field. A //go:build line that
		// names a language version is not carried into the tree yet, so every
		// file in the package takes Config.GoVersion.
		// The key is nil when the checker has no FileSet, which collapses the
		// map to one entry. Upstream has the same hole for a file parsed with
		// no position base, and it matters only once a file can carry its own
		// version, which nanogo's tree cannot yet.
		versions[check.srcFile(file.Pos())] = v`,
		},
		{
			why: "the tree carries no type-and-value record, so there is nothing to record into it",
			old: `func (check *Checker) recordTypeAndValueInSyntax(x syntax.Expr, mode operandMode, typ Type, val constant.Value) {
	if check.StoreTypesInSyntax {
		tv := TypeAndValue{mode, typ, val}
		stv := syntax.TypeAndValue{Type: typ, Value: val}
		if tv.IsVoid() {
			stv.SetIsVoid()
		}
		if tv.IsType() {
			stv.SetIsType()
		}
		if tv.IsBuiltin() {
			stv.SetIsBuiltin()
		}
		if tv.IsValue() {
			stv.SetIsValue()
		}
		if tv.IsNil() {
			stv.SetIsNil()
		}
		if tv.Addressable() {
			stv.SetAddressable()
		}
		if tv.Assignable() {
			stv.SetAssignable()
		}
		if tv.HasOk() {
			stv.SetHasOk()
		}
		x.SetTypeInfo(stv)
	}
}`,
			new: `func (check *Checker) recordTypeAndValueInSyntax(x syntax.Expr, mode operandMode, typ Type, val constant.Value) {
	// nanogo's syntax.Expr carries no type-and-value record,
	// so there is nowhere to store one. Info.Types is the only channel.
	// See specs/012-type-checking.md.
}`,
		},
		{
			why: "the same, for the comma-ok form, which upstream reads the recorded type back out of the tree to rewrite it",
			old: `func (check *Checker) recordCommaOkTypesInSyntax(x syntax.Expr, t0, t1 Type) {
	if check.StoreTypesInSyntax {
		// Note: this loop is duplicated because the type of tv is different.
		// Above it is types2.TypeAndValue, here it is syntax.TypeAndValue.
		for {
			tv := x.GetTypeInfo()
			assert(tv.Type != nil) // should have been recorded already
			pos := x.Pos()
			tv.Type = NewTuple(
				NewParam(pos, check.pkg, "", t0),
				NewParam(pos, check.pkg, "", t1),
			)
			x.SetTypeInfo(tv)
			p, _ := x.(*syntax.ParenExpr)
			if p == nil {
				break
			}
			x = p.X
		}
	}
}`,
			new: `func (check *Checker) recordCommaOkTypesInSyntax(x syntax.Expr, t0, t1 Type) {
	// See recordTypeAndValueInSyntax.
}`,
		},
		{
			why: "desc.pos is a poser, which is any here, so its position comes from atPos",
			old: `				check.trace(a.desc.pos.Pos(), "-- "+a.desc.format, a.desc.args...)`,
			new: `				check.trace(atPos(a.desc.pos), "-- "+a.desc.format, a.desc.args...)`,
		},
		{
			why: "a panic path has no checker to reach the FileSet through",
			old: `	panic(sprintf(nil, true, "instantiated ident not found; please report: %s", expr))`,
			new: `	panic(sprintf(nil, nil, true, "instantiated ident not found; please report: %s", expr))`,
		},
	},

	"errors.go": {
		{
			why: "nanogo's Pos is a uint32 with no method set, so poser cannot be an interface with a Pos method; atPos does the discrimination instead, at the cost of the compile-time check on what may be passed as a position",
			old: `// The poser interface is used to extract the position of type-checker errors.
type poser interface {
	Pos() syntax.Pos
}`,
			new: `// poser is anything that carries a source position: a syntax node, an
// operand, an object, or a bare syntax.Pos.
//
// Upstream writes this as an interface with a Pos method. nanogo's syntax.Pos
// is a uint32 and has no method set, so a bare position does not satisfy such
// an interface and the type switch in atPos does the work instead.
type poser = any`,
		},
		{
			why: "atPos discriminates on the dynamic type now that poser is any, so a bare Pos is a case rather than the fallthrough call to at.Pos",
			old: `func atPos(at poser) syntax.Pos {
	switch x := at.(type) {
	case *operand:
		if x.expr != nil {
			return syntax.StartPos(x.expr)
		}
	case syntax.Node:
		return syntax.StartPos(x)
	}
	return at.Pos()
}`,
			new: `func atPos(at poser) syntax.Pos {
	switch x := at.(type) {
	case *operand:
		if x.expr != nil {
			return syntax.StartPos(x.expr)
		}
	case syntax.Pos:
		return x
	case syntax.Node:
		return syntax.StartPos(x)
	}
	if p, ok := at.(interface{ Pos() syntax.Pos }); ok {
		return p.Pos()
	}
	return nopos
}`,
		},
		{
			why: "errpos is a poser, which is now any, so its position comes from atPos",
			old: `		if check.errpos.Pos().IsKnown() {`,
			new: `		if atPos(check.errpos).IsKnown() {`,
		},
		{
			why: "a sub-error position is printed, and a bare Pos does not print itself",
			old: `			if p.pos.IsKnown() {
				fmt.Fprintf(&buf, "%s: ", p.pos)
			}`,
			new: `			if p.pos.IsKnown() {
				fmt.Fprintf(&buf, "%s: ", check.position(p.pos))
			}`,
		},
		{
			why: "msg has no receiver name upstream because it needed none; it needs the checker to resolve the sub-error positions above",
			old: `func (err *error_) msg() string {
	if err.empty() {
		return "no error"
	}

	var buf strings.Builder`,
			new: `func (err *error_) msg() string {
	if err.empty() {
		return "no error"
	}

	check := err.check
	var buf strings.Builder`,
		},
		{
			why: "the reported Error carries a resolved position; see the Error patch in api.go",
			old: `	e := Error{
		Pos:  pos,
		Msg:  stripAnnotations(msg),
		Full: msg,
		Soft: soft,
		Code: code,
	}`,
			new: `	e := Error{
		Pos:      pos,
		Msg:      stripAnnotations(msg),
		Full:     msg,
		Soft:     soft,
		Code:     code,
		Position: check.position(pos),
	}`,
		},
	},

	"errorcalls_test.go": {
		{
			why: "a bare Pos does not print itself; the harness resolves it through the FileSet the source was parsed into",
			old: `				t.Errorf("%s: got %d arguments, want at least %d", call.Pos(), n, errorfMinArgCount)`,
			new: `				t.Errorf("%s: got %d arguments, want at least %d", position(call.Pos()), n, errorfMinArgCount)`,
		},
		{
			why: "the same, for the unbalanced-parentheses report on a format string literal",
			old: `							t.Errorf("%s: unbalanced parentheses/brackets", lit.Pos())`,
			new: `							t.Errorf("%s: unbalanced parentheses/brackets", position(lit.Pos()))`,
		},
	},

	"example_test.go": {
		{
			why: "the example builds its own Config, so it must name the FileSet the harness parsed into or every position it prints is unknown",
			old: `	conf := types2.Config{Importer: defaultImporter()}`,
			new: `	conf := types2.Config{Fset: testFset, Importer: defaultImporter()}`,
		},
		{
			why: "a bare Pos has no line or column; the harness resolves it through the FileSet the example parsed into",
			old: `		posn := id.Pos()
		lineCol := fmt.Sprintf("%d:%d", posn.Line(), posn.Col())`,
			new: `		posn := position(id.Pos())
		lineCol := fmt.Sprintf("%d:%d", posn.Line, posn.Col)`,
		},
		{
			why: "the same, for the definition position the example prints beside each object",
			old: `			obj.Pos(),`,
			new: `			position(obj.Pos()),`,
		},
		{
			why: "the same, for the start position of every recorded expression",
			old: `		posn := syntax.StartPos(expr)`,
			new: `		posn := position(syntax.StartPos(expr))`,
		},
		{
			why: "the resolved position carries Line and Col as fields, not as methods",
			old: `			posn.Line(), posn.Col(), types2.ExprString(expr),`,
			new: `			posn.Line, posn.Col, types2.ExprString(expr),`,
		},
	},

	"expr.go": {
		{
			why: "a bare Pos does not print itself; this one goes straight to fmt.Sprintf and would print a raw offset",
			old: `		panic(fmt.Sprintf("%s: unknown expression type %T", atPos(e), e))`,
			new: `		panic(fmt.Sprintf("%s: unknown expression type %T", check.position(atPos(e)), e))`,
		},
	},

	"format.go": {
		{
			why: "sprintf prints positions, and a bare Pos needs the FileSet to resolve; the argument is nil where there is no checker",
			old: `func sprintf(qf Qualifier, tpSubscripts bool, format string, args ...any) string {`,
			new: `func sprintf(fset *syntax.FileSet, qf Qualifier, tpSubscripts bool, format string, args ...any) string {`,
		},
		{
			why: "upstream asks the position to print itself; a bare Pos cannot, so the FileSet resolves it",
			old: `		case syntax.Pos:
			arg = a.String()`,
			new: `		case syntax.Pos:
			arg = posString(fset, a)`,
		},
		{
			why: "the checker's own sprintf passes the FileSet it holds",
			old: `	return sprintf(qf, false, format, args...)`,
			new: `	return sprintf(check.fset(), qf, false, format, args...)`,
		},
		{
			why: "the trace prefix is a resolved position, and the padding counts its digits",
			old: `func (check *Checker) trace(pos syntax.Pos, format string, args ...any) {
	// Use the width of line and pos values to align the ":" by adding padding before it.
	// Cap padding at 5: 3 digits for the line, 2 digits for the column number, which is
	// ok for most cases.
	w := ndigits(pos.Line()) + ndigits(pos.Col())
	pad := "     "[:max(5-w, 0)]
	fmt.Printf("%s%s:  %s%s\n",
		pos,
		pad,
		strings.Repeat(".  ", check.indent),
		sprintf(check.qualifier, true, format, args...),
	)
}`,
			new: `func (check *Checker) trace(pos syntax.Pos, format string, args ...any) {
	// Use the width of line and pos values to align the ":" by adding padding before it.
	// Cap padding at 5: 3 digits for the line, 2 digits for the column number, which is
	// ok for most cases.
	p := check.position(pos)
	w := ndigits(p.Line) + ndigits(p.Col)
	pad := "     "[:max(5-w, 0)]
	fmt.Printf("%s%s:  %s%s\n",
		p,
		pad,
		strings.Repeat(".  ", check.indent),
		sprintf(check.fset(), check.qualifier, true, format, args...),
	)
}`,
		},
		{
			why: "the same FileSet argument, for the debug dump",
			old: `	fmt.Println(sprintf(check.qualifier, true, format, args...))`,
			new: `	fmt.Println(sprintf(check.fset(), check.qualifier, true, format, args...))`,
		},
	},

	"instantiate.go": {
		{
			why: "a bare Pos does not print itself, and this panic goes straight to fmt.Sprintf",
			old: `		panic(fmt.Sprintf("%v: cannot instantiate %v", pos, orig))`,
			new: `		panic(fmt.Sprintf("%v: cannot instantiate %v", check.position(pos), orig))`,
		},
		{
			why: "the same, for validateTArgLen, where check may be nil and the position helper tolerates that",
			old: `	panic(fmt.Sprintf("%v: %s", pos, msg))`,
			new: `	panic(fmt.Sprintf("%v: %s", check.position(pos), msg))`,
		},
		{
			why: "a panic path has no checker to reach the FileSet through",
			old: `		panic(sprintf(nil, false, "cannot instantiate non-generic %s: expected *Named, *Alias, or *Signature", orig))`,
			new: `		panic(sprintf(nil, nil, false, "cannot instantiate non-generic %s: expected *Named, *Alias, or *Signature", orig))`,
		},
		{
			why: "the same, for the empty type argument list panic",
			old: `		panic(sprintf(nil, false, "cannot instantiate %s: empty type argument list", orig))`,
			new: `		panic(sprintf(nil, nil, false, "cannot instantiate %s: empty type argument list", orig))`,
		},
	},

	"type.go": {
		{
			why: "upstream keeps the Type interface in the syntax package so that the compiler can name a type without importing types2; nanogo's syntax tree has no such interface and the checker declares it",
			old: `import "cmd/compile/internal/syntax"

// A Type represents a type of Go.
// All types implement the Type interface.
type Type = syntax.Type`,
			new: `// A Type represents a type of Go.
// All types implement the Type interface.
type Type interface {
	// Underlying returns the underlying type of a type.
	// Underlying types are never Named, TypeParam, or Alias types.
	//
	// See https://go.dev/ref/spec#Underlying_types.
	Underlying() Type

	// String returns a string representation of a type.
	String() string
}`,
		},
	},

	"util.go": {
		{
			why: "nanogo's Pos is an offset into a FileSet, so numeric order is source order inside a file and FileSet order across files; upstream orders across files by file name. The doc comment is rewritten with the function because the old one would be false here",
			old: `// cmpPos compares the positions p and q and returns a result r as follows:
//
// r <  0: p is before q
// r == 0: p and q are the same position (but may not be identical)
// r >  0: p is after q
//
// If p and q are in different files, p is before q if the filename
// of p sorts lexicographically before the filename of q.
func cmpPos(p, q syntax.Pos) int { return p.Cmp(q) }`,
			new: `// cmpPos compares the positions p and q and returns a result r as follows:
//
// r <  0: p is before q
// r == 0: p and q are the same position (but may not be identical)
// r >  0: p is after q
//
// If p and q are in different files, p is before q if p's file was added to
// the FileSet first. Upstream compares file names instead, so a caller that
// wants upstream's error ordering must add a package's files to the FileSet in
// sorted-name order. Nothing here enforces that; it is a contract on whoever
// builds the FileSet (specs/014-package-loader.md).
func cmpPos(p, q syntax.Pos) int { return cmp.Compare(p, q) }`,
		},
		{
			why: "cmp is needed for cmpPos above",
			old: `import (
	"cmd/compile/internal/syntax"
	"go/constant"
	"go/token"
)`,
			new: `import (
	"cmp"
	"go/constant"
	"go/token"

	"cmd/compile/internal/syntax"
)`,
		},
	},

	"unify.go": {
		{
			why: "the unifier's trace path has no FileSet",
			old: `	fmt.Println(strings.Repeat(".  ", u.depth) + sprintf(nil, true, format, args...))`,
			new: `	fmt.Println(strings.Repeat(".  ", u.depth) + sprintf(nil, nil, true, format, args...))`,
		},
		{
			why: "the same, for the unifier's panic path",
			old: `		panic(sprintf(nil, true, "u.nify(%s, %s, %d)", xorig, yorig, mode))`,
			new: `		panic(sprintf(nil, nil, true, "u.nify(%s, %s, %d)", xorig, yorig, mode))`,
		},
	},

	"sizes_test.go": {
		{
			why: "internal/testenv is not importable outside the Go repository",
			old: `import (
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
	"internal/testenv"
	"testing"
)`,
			new: `import (
	"testing"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)`,
		},
		{
			why: "testenv_test.go has the one check this file makes",
			old: `	testenv.MustHaveGoBuild(t) // The Go command is needed for the importer to determine the locations of stdlib .a files.`,
			new: `	mustHaveGoBuild(t) // The Go command is needed for the importer to determine the locations of stdlib .a files.`,
		},
	},

	"sizeof_test.go": {
		{
			why: "every object holds two positions and a Scope holds two more; nanogo's Pos is 4 bytes against upstream's 16 on a 64-bit machine, so each of these is 24 bytes smaller. That saving is the reason specs/010 chose the compact Pos, and the test still guards against accidental growth",
			old: `		{PkgName{}, 56, 96},
		{Const{}, 60, 104},
		{TypeName{}, 52, 88},
		{Var{}, 60, 104},
		{Func{}, 60, 104},
		{Label{}, 56, 96},
		{Builtin{}, 56, 96},
		{Nil{}, 52, 88},

		// Misc
		{Scope{}, 60, 104},`,
			new: `		{PkgName{}, 40, 72},
		{Const{}, 44, 80},
		{TypeName{}, 36, 64},
		{Var{}, 44, 80},
		{Func{}, 44, 80},
		{Label{}, 40, 72},
		{Builtin{}, 40, 72},
		{Nil{}, 36, 64},

		// Misc
		{Scope{}, 44, 80},`,
		},
	},

	"stmt.go": {
		{
			why: "the message names where the first default clause is, and a bare Pos does not print itself; go vet is a gate here and rejects a uint32 under %s",
			old: `check.errorf(c, DuplicateDefault, "multiple defaults (first at %s)", first.Pos())`,
			new: `check.errorf(c, DuplicateDefault, "multiple defaults (first at %s)", check.position(first.Pos()))`,
			n:   2,
		},
	},

	"issues_test.go": {
		{
			why: "internal/testenv is not importable outside the Go repository",
			old: `import (
	"cmd/compile/internal/syntax"
	"fmt"
	"internal/testenv"
	"regexp"
	"slices"
	"strings"
	"testing"

	. "cmd/compile/internal/types2"
)`,
			new: `import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"cmd/compile/internal/syntax"
	. "cmd/compile/internal/types2"
)`,
		},
		{
			why: "testenv_test.go has these checks",
			old: `	testenv.MustHaveGoBuild(t)
`,
			new: `	mustHaveGoBuild(t)
`,
		},
		{
			why: "testenv_test.go carries the cgo check as well; nanogo does not compile cgo input, so the test is skipped rather than run under a guess",
			old: `	testenv.MustHaveCGO(t)`,
			new: `	mustHaveCGO(t)`,
		},
		{
			why: "the same call with the trailing comment upstream put on the line",
			old: `	testenv.MustHaveGoBuild(t) // The go command is needed for the importer to determine the locations of stdlib .a files.`,
			new: `	mustHaveGoBuild(t) // The go command is needed for the importer to determine the locations of stdlib .a files.`,
		},
		{
			why: "a bare Pos does not print itself; go vet is a gate here",
			old: `				t.Errorf("%s: got %s; want %s", x.Pos(), tv.Type, want)`,
			new: `				t.Errorf("%s: got %s; want %s", position(x.Pos()), tv.Type, want)`,
		},
		{
			why: "a bare Pos has no line; the harness resolves it",
			old: `			fact := fmt.Sprintf("L%d defs %s", id.Pos().Line(), obj)`,
			new: `			fact := fmt.Sprintf("L%d defs %s", position(id.Pos()).Line, obj)`,
		},
		{
			why: "the same, for the line the test records beside each use",
			old: `		fact := fmt.Sprintf("L%d uses %s", id.Pos().Line(), obj)`,
			new: `		fact := fmt.Sprintf("L%d uses %s", position(id.Pos()).Line, obj)`,
		},
		{
			why: "the test builds its own Config, so it must name the FileSet the harness parsed into or every position it prints is unknown",
			old: `		conf := Config{Importer: importHelper{pkg: bpkg}}`,
			new: `		conf := Config{Fset: testFset, Importer: importHelper{pkg: bpkg}}`,
		},
		{
			why: "the source importer type-checks an imported package from source, and runtime/cgo cannot be read that way",
			old: `func TestIssue59944(t *testing.T) {
	mustHaveCGO(t)
`,
			new: `func TestIssue59944(t *testing.T) {
	mustHaveCGO(t)
	t.Skip("known gap: the source importer cannot import runtime/cgo, which needs cgo-generated files")
`,
		},
		{
			why: "the test asserts that a //go:build line raises the file's language version",
			old: `func TestIssue64759(t *testing.T) {
`,
			new: `func TestIssue64759(t *testing.T) {
	t.Skip("known gap: " + gapFileVersion)

`,
		},
		{
			why: "nanogo's Parse takes a *SrcFile and a byte slice; the harness owns the FileSet",
			old: `	f, err := syntax.Parse(syntax.NewFileBase(pkgName(src)), strings.NewReader(src), func(error) {}, nil, 0)
	if err == nil {
		t.Fatal("expected syntax error")
	}

	var conf Config
	conf.Check(f.PkgName.Value, []*syntax.File{f}, nil) // must not panic`,
			new: `	f, err := parseSrc(pkgName(src), src)
	if err == nil {
		t.Fatal("expected syntax error")
	}

	conf := Config{Fset: testFset}
	conf.Check(f.PkgName.Value, []*syntax.File{f}, nil) // must not panic`,
		},
	},

	"labels.go": {
		{
			why: "the message names the line a declaration is on, and a bare Pos does not know its line; upstream uses Pos.Line, which ignores //line directives, so this uses the raw line too",
			old: `								varDeclPos.Line(),`,
			new: `								check.rawPosition(varDeclPos).Line,`,
		},
	},

	"object_test.go": {
		{
			why: "internal/testenv is not importable outside the Go repository; the harness in testenv_test.go has the one check this file makes",
			old: `import (
	"fmt"
	"internal/testenv"
	"strings"
	"testing"

	. "cmd/compile/internal/types2"
)`,
			new: `import (
	"fmt"
	"strings"
	"testing"

	. "cmd/compile/internal/types2"
)`,
		},
		{
			why: "the call the dropped import was there for",
			old: `	testenv.MustHaveGoBuild(t)`,
			new: `	mustHaveGoBuild(t)`,
		},
	},

	"typeset_test.go": {
		{
			why: "nanogo's Parse takes a *SrcFile and a byte slice, not a *PosBase and an io.Reader, because a FileSet owns the coordinate space",
			old: `		errh := func(error) {} // dummy error handler so that parsing continues in presence of errors
		src := "package p; type T interface" + body
		file, err := syntax.Parse(nil, strings.NewReader(src), errh, nil, 0)`,
			new: `		errh := func(syntax.Error) {} // dummy error handler so that parsing continues in presence of errors
		src := "package p; type T interface" + body
		fset := syntax.NewFileSet()
		file, err := syntax.Parse(fset.AddFile("p.go", len(src)), []byte(src), errh, nil, 0)`,
		},
		{
			why: "strings was only there for the io.Reader the old Parse wanted",
			old: `import (
	"cmd/compile/internal/syntax"
	"strings"
	"testing"
)`,
			new: `import (
	"testing"

	"cmd/compile/internal/syntax"
)`,
		},
	},

	"typestring_test.go": {
		{
			why: "internal/testenv is not importable outside the Go repository",
			old: `import (
	"internal/testenv"
	"testing"

	. "cmd/compile/internal/types2"
)`,
			new: `import (
	"testing"

	. "cmd/compile/internal/types2"
)`,
		},
		{
			why: "the call the dropped import was there for",
			old: `	testenv.MustHaveGoBuild(t)`,
			new: `	mustHaveGoBuild(t)`,
		},
	},

	"resolver.go": {
		{
			why: "the message names where the extra initialiser is, and a bare Pos does not print itself",
			old: `			check.errorf(pos, code, "extra init expr at %s", n.Pos())`,
			new: `			check.errorf(pos, code, "extra init expr at %s", check.position(n.Pos()))`,
		},
		{
			why: "a debugging file name comes from the resolved position",
			old: `		// return check.fset.File(pos).Name()
		// TODO(gri) do we need the actual file name here?
		return pos.RelFilename()`,
			new: `		return check.position(pos).Filename`,
		},
		{
			why: "the version map is keyed by file; see the check.go patches",
			old: `		check.version = asGoVersion(check.versions[file.Pos().FileBase()])`,
			new: `		check.version = asGoVersion(check.versions[check.srcFile(file.Pos())])`,
		},
		{
			why: "the import directory comes from the resolved file name",
			old: `		fileDir := dir(file.PkgName.Pos().RelFilename()) // TODO(gri) should this be filename?`,
			new: `		fileDir := dir(check.position(file.PkgName.Pos()).Filename)`,
		},
	},

	"resolver_test.go": {
		{
			why: "internal/testenv is not importable outside the Go repository",
			old: `import (
	"cmd/compile/internal/syntax"
	"fmt"
	"internal/testenv"
	"slices"
	"testing"

	. "cmd/compile/internal/types2"
)`,
			new: `import (
	"fmt"
	"slices"
	"testing"

	"cmd/compile/internal/syntax"
	. "cmd/compile/internal/types2"
)`,
		},
		{
			why: "testenv_test.go has the one check this file makes",
			old: `	testenv.MustHaveGoBuild(t)`,
			new: `	mustHaveGoBuild(t)`,
		},
		{
			why: "a bare Pos does not print itself; the harness resolves it, and go vet is a gate here",
			old: `x.Pos(), x.Value)`,
			new: `position(x.Pos()), x.Value)`,
			n:   4,
		},
		{
			why: "the same, for the unresolved selector report",
			old: `s.Sel.Pos(), s.Sel.Value)`,
			new: `position(s.Sel.Pos()), s.Sel.Value)`,
		},
		{
			why: "the same, for the nil-object report over the Uses map",
			old: `id.Pos(), id.Value)`,
			new: `position(id.Pos()), id.Value)`,
		},
	},

	"signature.go": {
		{
			why: "the cgo test reads the file a name was declared in, which needs the FileSet; upstream uses Pos.FileBase, which names the file on disk, so this ignores //line directives too",
			old: `// isCGoTypeObj reports whether the given type name was created by cgo.
func isCGoTypeObj(obj *TypeName) bool {
	return strings.HasPrefix(obj.name, "_Ctype_") ||
		strings.HasPrefix(filepath.Base(obj.pos.FileBase().Filename()), "_cgo_")
}`,
			new: `// isCGoTypeObj reports whether the given type name was created by cgo.
func (check *Checker) isCGoTypeObj(obj *TypeName) bool {
	return strings.HasPrefix(obj.name, "_Ctype_") ||
		strings.HasPrefix(filepath.Base(check.rawPosition(obj.pos).Filename), "_cgo_")
}`,
		},
		{
			why: "isCGoTypeObj is a method now",
			old: `|| isCGoTypeObj(T.obj) {`,
			new: `|| check.isCGoTypeObj(T.obj) {`,
			n:   1,
		},
	},

	"version.go": {
		{
			why: "internal/goversion is not importable outside the Go repository; the current language version is a constant here and the toolchain gate in specs/001 owns keeping it honest",
			old: `import (
	"fmt"
	"go/version"
	"internal/goversion"
)`,
			new: `import (
	"fmt"
	"go/version"
)

// goVersionMinor is the minor version of the Go language this checker
// implements. Upstream reads internal/goversion.Version, which is not
// importable outside the Go repository.
const goVersionMinor = 27`,
		},
		{
			why: "the one use of the import that was dropped above",
			old: `	go_current = asGoVersion(fmt.Sprintf("go1.%d", goversion.Version))`,
			new: `	go_current = asGoVersion(fmt.Sprintf("go1.%d", goVersionMinor))`,
		},
	},
}

// portedTests lists the upstream test files that are generated.
//
// The upstream test suite comes with the fork and is the reason the fork is
// safe (specs/012-type-checking.md), so a test is skipped only when it needs
// infrastructure that nanogo does not have. skippedTests records why for each
// one that stays behind.
var portedTests = []string{
	"alias_test.go",
	"api_test.go",
	"builtins_test.go",
	"context_test.go",
	"errorcalls_test.go",
	"errors_test.go",
	"example_test.go",
	"hilbert_test.go",
	"instantiate_test.go",
	"issues_test.go",
	"lookup_test.go",
	"mono_test.go",
	"named_test.go",
	"resolver_test.go",
	"object_test.go",
	"sizeof_test.go",
	"sizes_test.go",
	"termlist_test.go",
	"trie_test.go",
	"typeset_test.go",
	"typestring_test.go",
	"typeterm_test.go",
	"util_test.go",
}

// skippedTests lists the upstream test files that are not generated, and why.
var skippedTests = map[string]string{
	"check_test.go":    "the error-annotation harness reads /* ERROR */ comments through syntax.CommentsDo, and nanogo's tree carries no comments; replaced by errorcheck_test.go, which scans the annotations out of the source text",
	"importer_test.go": "needs cmd/compile/internal/importer, which reads gc export data; nanogo's export data is specs/015 and is not built yet",
	"main_test.go":     "TestMain only sets up internal/testenv and go/build for the tests that are skipped anyway",
	"self_test.go":     "type-checks cmd/compile/internal/types2 itself out of GOROOT, which needs an importer",
	"stdlib_test.go":   "walks GOROOT and needs an importer; this is the conformance job in specs/004, not a unit test",
}

// handPorted lists upstream files that are not generated. A copy lives in
// types2/ and the generator neither reads nor writes it.
//
// Upstream marks its own unportable files the same way, in
// go/types/generate_test.go.
var handPorted = map[string]string{
	"compiler_internal.go": "RenameResult writes a type back into the syntax tree, and nanogo's tree has no place to put one",
}

// dropped lists upstream files that are not carried at all.
var dropped = map[string]string{}

// handWritten lists the files in types2/ that nanogo wrote and that no
// upstream file corresponds to. The drift test needs the list so that it does
// not report them as stale generated output.
var handWritten = map[string]string{
	"position.go":         "the FileSet plumbing that nanogo's compact Pos needs",
	"stencil.go":          "the exported door on subst.go that specs/013-generics.md's stenciler substitutes through; upstream has none, because upstream has no stenciler",
	"stencil_test.go":     "the tests for stencil.go",
	"testenv_test.go":     "the pieces of internal/testenv the ported tests use",
	"e2e_test.go":         "the end-to-end gate: nanogo's parser, then this checker, judged against go/types on accept and reject",
	"errorcheck_test.go":  "the /* ERROR */ corpus harness; upstream reads the annotations with syntax.CommentMap and nanogo's tree carries no comments",
	"srcimporter_test.go": "an importer that type-checks an imported package from source; nanogo has no export data yet (specs/015)",
	"harness_test.go":     "the parse and type-check helpers the ported tests share; upstream keeps them in api_test.go and check_test.go, which are not ported whole",
}

// skippedErrorTests lists the vendored internal/types/errors test files that
// are not generated, and why.
var skippedErrorTests = map[string]string{
	"codes_test.go": "it walks the checker's source with go/ast and go/types to prove every error code is used and documented; the walk is over upstream's directory layout and is a lint on the Go tree rather than a test of the checker",
}

// errorFiles are the vendored internal/types/errors sources. They need no
// rewriting: nanogo carries the upstream error code set unchanged, so that the
// errorcheck corpus in specs/004-conformance.md keeps matching.
var errorFiles = []string{"codes.go", "code_string.go"}

// File is one generated file.
type File struct {
	// Name is the path relative to the types2 directory.
	Name string
	Data []byte
}

// Generate reads the vendored sources under root/upstream and returns the
// files that belong in root.
func Generate(root string) ([]File, error) {
	var out []File

	names, err := upstreamNames(filepath.Join(root, "upstream", "types2"))
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if _, ok := handPorted[name]; ok {
			continue
		}
		if _, ok := dropped[name]; ok {
			continue
		}
		if strings.HasSuffix(name, "_test.go") && !slices.Contains(portedTests, name) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, "upstream", "types2", name+upstreamSuffix))
		if err != nil {
			return nil, err
		}
		data, err := rewrite(name, src, "../upstream/types2/"+name+upstreamSuffix)
		if err != nil {
			return nil, err
		}
		out = append(out, File{Name: name, Data: data})
	}

	for _, name := range errorFiles {
		src, err := os.ReadFile(filepath.Join(root, "upstream", "errors", name+upstreamSuffix))
		if err != nil {
			return nil, err
		}
		data, err := rewrite(name, src, "../../upstream/errors/"+name+upstreamSuffix)
		if err != nil {
			return nil, err
		}
		out = append(out, File{Name: filepath.Join("errors", name), Data: data})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// upstreamNames returns the upstream file names, with the .txt suffix removed.
func upstreamNames(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go"+upstreamSuffix) {
			continue
		}
		names = append(names, strings.TrimSuffix(n, upstreamSuffix))
	}
	sort.Strings(names)
	return names, nil
}

// rewrite applies the table to one file and formats the result.
func rewrite(name string, src []byte, source string) ([]byte, error) {
	s := string(src)

	for _, p := range patches[name] {
		want := p.n
		if want == 0 {
			want = 1
		}
		if got := strings.Count(s, p.old); got != want {
			return nil, fmt.Errorf("%s: patch matched %d times, want %d\n\twhy: %s\n\tpattern:\n%s", name, got, want, p.why, indent(p.old))
		}
		s = strings.Replace(s, p.old, p.new, want)
	}

	for _, r := range rules {
		s = strings.ReplaceAll(s, r.old, r.new)
	}

	s = header(source) + s

	data, err := format.Source([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	return data, nil
}

func header(source string) string {
	return "// Code generated by types2/gen. DO NOT EDIT.\n" +
		"// Source: " + source + "\n\n"
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "\t\t" + l
	}
	return strings.Join(lines, "\n")
}
