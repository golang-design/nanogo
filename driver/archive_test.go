// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
)

const testHeader = "go object darwin arm64 go1.27.0 X:regabiwrappers\n"

// emptyPackage is an object with one data symbol, which is enough to write.
func emptyPackage(t *testing.T) *obj.Package {
	t.Helper()
	p := obj.NewPackage("p")
	p.AddDef(&obj.Symbol{Name: "p.x", Type: obj.SNOPTRDATA, Size: 1, Data: []byte{7}, Align: 1})
	return p
}

// TestArchiveHeaderIsTheFormat checks the field widths, which every reader
// parses by fixed offset. A header that is one byte narrow puts the next
// reader inside the member's data.
func TestArchiveHeaderIsTheFormat(t *testing.T) {
	var b strings.Builder
	if err := writeArchiveHeader(&b, objectMember, 12345); err != nil {
		t.Fatal(err)
	}
	h := b.String()
	if len(h) != archiveHeader {
		t.Fatalf("the header is %d bytes, want %d", len(h), archiveHeader)
	}
	if h[:16] != "_go_.o          " {
		t.Errorf("the name field is %q", h[:16])
	}
	size, err := strconv.Atoi(strings.TrimSpace(h[48:58]))
	if err != nil || size != 12345 {
		t.Errorf("the size field is %q", h[48:58])
	}
	if h[58:] != "`\n" {
		t.Errorf("the terminator is %q", h[58:])
	}
	// The modification time, the owner and the group are zero, so two runs
	// over the same input produce the same bytes (specs/053-determinism.md).
	for _, field := range []string{h[16:28], h[28:34], h[34:40]} {
		if strings.TrimSpace(field) != "0" {
			t.Errorf("field %q is not zero, so the archive is not reproducible", field)
		}
	}
}

func TestArchiveRefusesALongMemberName(t *testing.T) {
	err := writeArchiveHeader(new(strings.Builder), "a-name-that-is-far-too-long.o", 1)
	if err == nil {
		t.Fatal("a name longer than the field was accepted")
	}
}

// TestArchivePadsToAnEvenLength checks the padding byte, which no header
// counts and every reader steps over.
func TestArchivePadsToAnEvenLength(t *testing.T) {
	p := emptyPackage(t)
	var bare failWriter
	if err := writeTo(&bare, p, testHeader, false); err != nil {
		t.Fatal(err)
	}
	var packed failWriter
	if err := writeTo(&packed, p, testHeader, true); err != nil {
		t.Fatal(err)
	}
	body := len(bare.b)
	want := len(archiveMagic) + archiveHeader + body + body%2
	if len(packed.b) != want {
		t.Errorf("the archive is %d bytes, want %d for a %d byte member", len(packed.b), want, body)
	}
	size, err := strconv.Atoi(strings.TrimSpace(string(packed.b[len(archiveMagic)+48 : len(archiveMagic)+58])))
	if err != nil || size != body {
		t.Errorf("the header says %v bytes, and the member is %d", size, body)
	}
}

// failWriter records what it was given and fails after a set number of writes.
type failWriter struct {
	b     []byte
	after int // 0 never fails
	n     int
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.n++
	if w.after > 0 && w.n >= w.after {
		return 0, errors.New("no space")
	}
	w.b = append(w.b, p...)
	return len(p), nil
}

// TestArchiveReportsAWriteFailure covers each place the writer can stop. A
// truncated archive that reported success would reach the linker.
func TestArchiveReportsAWriteFailure(t *testing.T) {
	p := emptyPackage(t)
	for after := 1; after <= 3; after++ {
		w := &failWriter{after: after}
		if err := writeTo(w, p, testHeader, true); err == nil {
			t.Errorf("writeTo succeeded although write %d failed", after)
		}
	}
	if err := writeTo(&failWriter{}, p, "not a header\n", true); err == nil {
		t.Error("writeTo accepted an object header the linker would refuse")
	}
}

// TestViolationsAreJoined covers the report the verifier's findings become.
// The pipeline verifies twice and a finding stops the function, so the text is
// what the author of the failing pass reads.
func TestViolationsAreJoined(t *testing.T) {
	err := violations([]ssa.Violation{
		{Block: 1, Value: -1, Detail: "no terminator"},
		{Block: 2, Value: 7, Detail: "operand is not defined"},
	})
	if err == nil {
		t.Fatal("violations returned no error")
	}
	for _, want := range []string{"b1", "no terminator", "b2 v7", "operand is not defined"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report does not carry %q:\n%v", want, err)
		}
	}
}

// TestArchivePadsAnOddMember covers the padding byte itself. An object whose
// length happens to be odd is what the padding exists for, and a reader that
// trusted the header alone would land inside the next member's name.
func TestArchivePadsAnOddMember(t *testing.T) {
	for size := 1; size <= 32; size++ {
		p := obj.NewPackage("p")
		p.AddDef(&obj.Symbol{
			Name: "p.x", Type: obj.SNOPTRDATA,
			Size: uint32(size), Data: make([]byte, size), Align: 1,
		})
		var bare failWriter
		if err := writeTo(&bare, p, testHeader, false); err != nil {
			t.Fatal(err)
		}
		if len(bare.b)%2 == 0 {
			continue
		}
		var packed failWriter
		if err := writeTo(&packed, p, testHeader, true); err != nil {
			t.Fatal(err)
		}
		if got, want := len(packed.b), len(archiveMagic)+archiveHeader+len(bare.b)+1; got != want {
			t.Errorf("an odd member produced %d bytes, want %d", got, want)
		}
		// The padding byte fails the write, which is the branch a member of
		// even length never reaches.
		if err := writeTo(&failWriter{after: 4}, p, testHeader, true); err == nil {
			t.Error("writeTo succeeded although the padding byte could not be written")
		}
		return
	}
	t.Skip("no symbol size in the range produced an object of odd length")
}
