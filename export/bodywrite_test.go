// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/export/pkgbits"
	"golang.design/x/nanogo/types2"
)

// The oracle the body encoder is measured by.
//
// It is stronger than "gc accepts what nanogo wrote": every body element of
// every standard library package is decoded, encoded again through
// [elemRefs], and compared with gc's own bytes and gc's own reference table.
// A field written at the wrong width, in the wrong order, or left out moves
// every byte after it, and a reference made in the wrong order moves the
// table entry it produced, so an element that matches both agrees with gc's
// encoder field for field.
//
// What it does not prove is which element a reference names. [elemRefs]
// answers with the index the archive already gave it, so the round trip
// proves the layout and says nothing about a tree built from syntax, which
// has no such index. See bodywrite.go.

// writeCensus counts what one run of the round trip compared.
type writeCensus struct {
	packages int

	// elements is the number of body elements encoded and compared, and
	// claimed is the number the reader said it decoded. A function literal's
	// body is an element of its own that hangs off the tree rather than off
	// the returned list, so the two disagree wherever the walk misses one.
	elements int
	claimed  int

	failures []string
}

func (c *writeCensus) fail(format string, args ...any) {
	c.failures = append(c.failures, fmt.Sprintf(format, args...))
}

// encodeOne writes one body element and returns it with the function literals
// it named.
func encodeOne(pe *pkgbits.PkgEncoder, refs bodyRefs, path, name string, b *Body) (enc *pkgbits.Encoder, nested []*FuncLitExpr, err error) {
	defer func() {
		if v := recover(); v != nil {
			if e, ok := v.(*BodyError); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", v)
		}
	}()
	w := &bodyWriter{
		Encoder: pe.NewEncoder(pkgbits.SectionBody, pkgbits.SyncFuncBody),
		refs:    refs,
		path:    path,
		name:    name,
	}
	w.encodeBody(b)
	return w.Encoder, w.nested, nil
}

// compareElement checks an encoded element against the one it came from.
//
// The payload and the reference table are compared separately, because they
// fail for different reasons: a payload difference is a field at the wrong
// width or in the wrong order, and a table difference is a reference made in
// the wrong order or not made at all.
func compareElement(pr *pkgReader, idx pkgbits.Index, enc *pkgbits.Encoder) error {
	d := pr.NewDecoderRaw(pkgbits.SectionBody, idx)
	want, err := io.ReadAll(&d.Data)
	if err != nil {
		return err
	}

	if len(enc.Relocs) != len(d.Relocs) {
		return fmt.Errorf("the reference table holds %d entries and gc's holds %d", len(enc.Relocs), len(d.Relocs))
	}
	for i := range d.Relocs {
		if enc.Relocs[i] != d.Relocs[i] {
			return fmt.Errorf("reference %d of the table is %v %d and gc's is %v %d",
				i, enc.Relocs[i].Kind, enc.Relocs[i].Idx, d.Relocs[i].Kind, d.Relocs[i].Idx)
		}
	}

	got := enc.Data.Bytes()
	if bytes.Equal(got, want) {
		return nil
	}
	n := min(len(got), len(want))
	for i := range n {
		if got[i] != want[i] {
			return fmt.Errorf("byte %d of %d is %#x and gc's is %#x", i, len(want), got[i], want[i])
		}
	}
	return fmt.Errorf("the element is %d bytes and gc's is %d", len(got), len(want))
}

// checkBodies encodes every body of one package and compares each with gc's.
func (c *writeCensus) checkBodies(pr *pkgReader, bodies []*FuncBody) {
	refs, err := newElemRefs(pr)
	if err != nil {
		c.fail("%v", err)
		return
	}
	// The version the archive was written at, so that every version-gated
	// field the encoder writes is the one the decoder read.
	root := pr.NewDecoderRaw(pkgbits.SectionMeta, pkgbits.PrivateRootIdx)
	version := root.Version()
	pe := pkgbits.NewPkgEncoder(version)

	type pending struct {
		idx  pkgbits.Index
		body *Body
		name string
	}
	var queue []pending
	for _, b := range bodies {
		c.claimed += 1 + b.Nested
		queue = append(queue, pending{b.Idx, b.Body, b.Path + "." + b.Name})
	}

	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		c.elements++

		enc, nested, err := encodeOne(&pe, refs, pr.PkgPath(), p.name, p.body)
		if err != nil {
			c.fail("%s: %s: %v", pr.PkgPath(), p.name, err)
			continue
		}
		if err := compareElement(pr, p.idx, enc); err != nil {
			c.fail("%s: %s: body element %d does not match gc's: %v", pr.PkgPath(), p.name, p.idx, err)
			continue
		}
		for _, f := range nested {
			queue = append(queue, pending{f.Body, f.Decoded, p.name + ", in a function literal"})
		}
	}
	c.packages++
}

// TestWriteBodies is the oracle the body encoder is measured by.
//
// Under NANOGO_REQUIRE_CORPUS it runs over the whole standard library.
// Without it, it runs over [bodyPackages], which is one package per shape the
// encoding has.
func TestWriteBodies(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/std\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := bodyPackages
	if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
		want = []string{"std"}
	}

	c := &writeCensus{}
	for _, a := range archives(t, dir, want...) {
		path, file := a[0], a[1]
		pr, bodies, err := readArchiveBodies(path, file)
		if err != nil {
			// A package the reader refuses is the reader's census and not
			// this one's, and TestReadBodies is where it is reported.
			t.Errorf("%s: %v", path, err)
			continue
		}
		c.checkBodies(pr, bodies)
	}

	t.Logf("encoded %d body elements of %d packages and compared each with gc's own bytes", c.elements, c.packages)
	for _, f := range c.failures {
		t.Errorf("%s", f)
	}
	if c.elements != c.claimed {
		t.Errorf("encoded %d body elements and the reader decoded %d, so the walk misses a function literal's body", c.elements, c.claimed)
	}
	if c.elements == 0 {
		t.Error("no body element was encoded, so the encoder proved nothing")
	}
}

// readArchiveBodies reads one archive's bodies with a package table of its
// own.
//
// A fresh table per archive, because the reverse maps [newElemRefs] builds are
// of this archive's sections and a package another archive materialised has no
// element here.
func readArchiveBodies(path, file string) (*pkgReader, []*FuncBody, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	def, err := packageDefinition(data)
	if err != nil {
		return nil, nil, err
	}
	payload, err := unified(def)
	if err != nil {
		return nil, nil, err
	}
	dec := pkgbits.NewPkgDecoder(path, string(payload))
	_, pr, bodies, err := readBodies(types2.NewContext(), make(map[string]*types2.Package), dec)
	if err != nil {
		return nil, nil, err
	}
	return pr, bodies, nil
}

// @@@ Refusals

// stubRefs resolves every reference to element zero.
//
// The refusals below are decided before any reference is written, or on a
// field whose index is not what is wrong with it, so what the resolver answers
// does not enter the test.
type stubRefs struct{}

func (stubRefs) strIdx(string) pkgbits.Index          { return 0 }
func (stubRefs) pkgIdx(*types2.Package) pkgbits.Index { return 0 }
func (stubRefs) typIdx(TypeUse) pkgbits.Index         { return 0 }
func (stubRefs) objIdx(ObjUse) pkgbits.Index          { return 0 }
func (stubRefs) posBaseIdx(Pos) pkgbits.Index         { return 0 }
func (stubRefs) bodyIdx(*FuncLitExpr) pkgbits.Index   { return 0 }

// unknownStmt is a statement no code of the format names.
type unknownStmt struct{}

func (*unknownStmt) stmtKind() StmtKind { return StmtEnd }

// typedLocal is a use of a local carrying the type the stream gave it.
func typedLocal(t types2.Type) *LocalExpr {
	return &LocalExpr{exprType: exprType{typ: t}}
}

// TestWriteBodyRefusals checks that a tree the format has no shape for is
// refused by name rather than encoded with every byte after the field moved.
//
// The round trip cannot reach these: it encodes trees the reader built, and
// the reader builds each of these fields by the same test the encoder makes on
// it. They are what a builder from syntax would get wrong.
func TestWriteBodyRefusals(t *testing.T) {
	mapType := types2.NewMap(types2.Typ[types2.Int], types2.Typ[types2.Int])
	intType := types2.Typ[types2.Int]
	appendCall := func(rt *RType) *CallExpr {
		return &CallExpr{
			Fun:   &GlobalExpr{Obj: ObjUse{Name: "append"}},
			Args:  MultiExpr{},
			RType: rt,
		}
	}

	tests := []struct {
		name string
		expr Expr
		stmt Stmt
		want string
	}{{
		name: "an operator no body carries",
		expr: &UnaryExpr{Op: Op(3), X: typedLocal(intType)},
		want: "gc's ir.Op 3",
	}, {
		name: "a statement the format has no code for",
		stmt: &unknownStmt{},
		want: "which the format has no encoding for",
	}, {
		name: "an index of a map with no descriptor",
		expr: &IndexExpr{X: typedLocal(mapType), Index: typedLocal(intType)},
		want: "the indexed operand is a map and carries no descriptor",
	}, {
		name: "an index of what is not a map with a descriptor",
		expr: &IndexExpr{X: typedLocal(intType), Index: typedLocal(intType), MapRType: &RType{}},
		want: "the indexed operand is not a map and carries a descriptor",
	}, {
		name: "an index whose operand carries no type",
		expr: &IndexExpr{X: typedLocal(nil), Index: typedLocal(intType)},
		want: "the indexed operand carries no type",
	}, {
		name: "a call of append with no runtime type",
		expr: appendCall(nil),
		want: "needs a runtime type and carries none",
	}, {
		name: "a call carrying a runtime type its callee needs none for",
		expr: &CallExpr{Fun: typedLocal(intType), Args: MultiExpr{}, RType: &RType{}},
		want: "carries a runtime type and its callee needs none",
	}, {
		name: "a composite literal with no type",
		expr: &CompLitExpr{},
		want: "the composite literal carries no type",
	}, {
		name: "Offsetof naming no field",
		expr: &OffsetofExpr{},
		want: "Offsetof names no field",
	}, {
		name: "a type written where an expression is expected with neither shape",
		expr: &MakeExpr{},
		want: "carries neither a descriptor nor an itab",
	}, {
		name: "an expression the format requires and the tree does not hold",
		stmt: &ExprStmt{},
		want: "an expression the format requires is absent",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := tt.stmt
			if stmt == nil {
				stmt = &ExprStmt{X: tt.expr}
			}
			body := &Body{HasBlock: true, Stmts: []Stmt{stmt}}
			pe := pkgbits.NewPkgEncoder(Version)
			_, _, err := encodeOne(&pe, stubRefs{}, "xtest", "xtest.F", body)
			if err == nil {
				t.Fatalf("the tree encoded and the format has no shape for it")
			}
			e, ok := err.(*BodyError)
			if !ok {
				t.Fatalf("the refusal is a %T and not a *BodyError: %v", err, err)
			}
			if e.Name != "xtest.F" {
				t.Errorf("the refusal names %q and not the declaration", e.Name)
			}
			if !strings.Contains(e.Reason, tt.want) {
				t.Errorf("the refusal says %q and not %q", e.Reason, tt.want)
			}
		})
	}
}
