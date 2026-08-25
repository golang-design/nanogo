// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package pkgbits

import (
	"bytes"
	"go/constant"
	"math/big"
	"testing"
)

// TestEncoderRoundTripsEveryPrimitive writes one element carrying every
// primitive the container has and reads it back with the decoder.
//
// The decoder is the ported one, so agreement here says the two halves of the
// container agree. It says nothing about gc, which is what the cross-read
// test in export/ is for.
func TestEncoderRoundTripsEveryPrimitive(t *testing.T) {
	pw := NewPkgEncoder(V4)

	// A second element, so that a reference has somewhere to point.
	other := pw.NewEncoder(SectionMeta, SyncPrivate)
	other.String("other")
	other.Flush()

	w := pw.NewEncoder(SectionMeta, SyncPublic)
	if w.Idx != 1 {
		t.Fatalf("second element got index %v, want 1", w.Idx)
	}
	w.Bool(true)
	w.Bool(false)
	w.Int64(-1 << 40)
	w.Uint64(1 << 41)
	w.Len(7)
	w.Int(-9)
	w.Uint(11)
	w.String("hello")
	w.String("hello") // deduplicated; the string table holds nine distinct strings
	w.Strings([]string{"a", "b"})
	w.Code(ObjFunc)
	w.Reloc(SectionMeta, 0)
	w.Value(constant.MakeBool(true))
	w.Value(constant.MakeString("s"))
	w.Value(constant.MakeInt64(-5))
	w.Value(constant.Make(new(big.Int).Lsh(big.NewInt(1), 100)))
	w.Value(constant.Make(new(big.Rat).SetFrac64(3, 2)))
	w.Value(constant.Make(big.NewFloat(1e-300)))
	w.Value(constant.BinaryOp(constant.MakeInt64(3), 12 /* token.ADD */, constant.MakeImag(constant.MakeInt64(4))))
	w.Flush()

	var buf bytes.Buffer
	fp, err := pw.DumpTo(&buf)
	if err != nil {
		t.Fatalf("DumpTo: %v", err)
	}

	pr := NewPkgDecoder("p", buf.String())
	if pr.Fingerprint() != fp {
		t.Errorf("Fingerprint() = %x, DumpTo returned %x", pr.Fingerprint(), fp)
	}
	if got, want := pr.NumElems(SectionMeta), 2; got != want {
		t.Fatalf("NumElems(SectionMeta) = %d, want %d", got, want)
	}
	if got, want := pr.NumElems(SectionString), 9; got != want {
		t.Errorf("NumElems(SectionString) = %d, want %d; a repeated string was stored twice", got, want)
	}

	r := pr.NewDecoder(SectionMeta, 1, SyncPublic)
	if !r.Bool() || r.Bool() {
		t.Errorf("the two bools did not come back as true, false")
	}
	if got := r.Int64(); got != -1<<40 {
		t.Errorf("Int64 = %d", got)
	}
	if got := r.Uint64(); got != 1<<41 {
		t.Errorf("Uint64 = %d", got)
	}
	if got := r.Len(); got != 7 {
		t.Errorf("Len = %d", got)
	}
	if got := r.Int64(); got != -9 {
		t.Errorf("Int = %d", got)
	}
	if got := r.Uint(); got != 11 {
		t.Errorf("Uint = %d", got)
	}
	if got := r.String(); got != "hello" {
		t.Errorf("String = %q", got)
	}
	if got := r.String(); got != "hello" {
		t.Errorf("repeated String = %q", got)
	}
	if got := r.Len(); got != 2 {
		t.Fatalf("Strings length = %d", got)
	}
	for _, want := range []string{"a", "b"} {
		if got := r.String(); got != want {
			t.Errorf("Strings element = %q, want %q", got, want)
		}
	}
	if got := CodeObj(r.Code(SyncCodeObj)); got != ObjFunc {
		t.Errorf("Code = %v, want %v", got, ObjFunc)
	}
	if got := r.Reloc(SectionMeta); got != 0 {
		t.Errorf("Reloc = %v, want 0", got)
	}
	for _, want := range []constant.Value{
		constant.MakeBool(true),
		constant.MakeString("s"),
		constant.MakeInt64(-5),
		constant.Make(new(big.Int).Lsh(big.NewInt(1), 100)),
		constant.Make(new(big.Rat).SetFrac64(3, 2)),
		constant.Make(big.NewFloat(1e-300)),
		constant.BinaryOp(constant.MakeInt64(3), 12, constant.MakeImag(constant.MakeInt64(4))),
	} {
		got := r.Value()
		if constant.Compare(got, 39 /* token.EQL */, want) != true {
			t.Errorf("Value = %v (%v), want %v (%v)", got, got.Kind(), want, want.Kind())
		}
	}
}

// TestEncoderIsDeterministic writes the same element twice and compares the
// bytes, because the export data is part of a compiled package
// (specs/053-determinism.md).
func TestEncoderIsDeterministic(t *testing.T) {
	build := func() []byte {
		pw := NewPkgEncoder(V4)
		for _, s := range []string{"beta", "alpha", "beta", "gamma"} {
			w := pw.NewEncoder(SectionPkg, SyncPkgDef)
			w.String(s)
			w.Reloc(SectionString, pw.StringIdx("shared"))
			w.Flush()
		}
		var buf bytes.Buffer
		if _, err := pw.DumpTo(&buf); err != nil {
			t.Fatalf("DumpTo: %v", err)
		}
		return buf.Bytes()
	}
	if a, b := build(), build(); !bytes.Equal(a, b) {
		t.Errorf("two encodings of the same input differ: %d and %d bytes", len(a), len(b))
	}
}

// TestEncoderWritesNoSyncMarkers pins the divergence README.md records: data
// nanogo marked would be data nanogo cannot read.
func TestEncoderWritesNoSyncMarkers(t *testing.T) {
	pw := NewPkgEncoder(V4)
	if pw.SyncMarkers() {
		t.Fatal("SyncMarkers() is true")
	}
	w := pw.NewEncoder(SectionMeta, SyncPublic)
	w.Bool(true)
	w.Flush()

	var buf bytes.Buffer
	if _, err := pw.DumpTo(&buf); err != nil {
		t.Fatalf("DumpTo: %v", err)
	}
	pr := NewPkgDecoder("p", buf.String())
	if pr.sync {
		t.Error("the decoder found the sync marker flag set")
	}
	// One element: the reference table length and the bool, and nothing
	// before either of them.
	if got, want := pr.DataIdx(SectionMeta, 0), "\x00\x01"; got != want {
		t.Errorf("element bitstream = %q, want %q", got, want)
	}
}
