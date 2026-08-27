// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The program that says a type has one descriptor in the linked binary.
//
// A type descriptor is an identity, not only a set of bytes. The runtime
// compares two of them by address: runtime.SetFinalizer accepts func(*int32)
// for a *int32 because the *int32 in the signature's parameter array is the
// same symbol as the *int32 in the interface word, and it throws when the two
// addresses differ however equal the bytes are.
//
// Both descriptors here are written by nanogo and both name the type the
// runtime already defines, so the program holds three claims at once: the
// descriptor nanogo emits merges with the runtime's, the two references from
// one object reach one symbol, and the signature's parameter array points at
// the descriptor rather than at a copy of it.
//
// The exit status is the assertion. SetFinalizer throws on a mismatch, and the
// unsafe read below is the same question asked without the runtime, so a
// failure names which of the two broke.
const descriptorIdentityProgram = `package main

import (
	"runtime"
	"unsafe"
)

type eface struct{ typ, data unsafe.Pointer }

// inSlice returns the first parameter type of a func descriptor. The array of
// parameter and result descriptors follows the FuncType header, which is
// internal/abi.Type plus two counts rounded up to a pointer.
func inSlice(f unsafe.Pointer) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(uintptr(f) + 56))
}

func main() {
	x := new(int32)
	var obj any = x
	var fn any = func(p *int32) {}
	if inSlice((*eface)(unsafe.Pointer(&fn)).typ) != (*eface)(unsafe.Pointer(&obj)).typ {
		panic("the *int32 in func(*int32) is a second descriptor")
	}
	runtime.SetFinalizer(x, func(p *int32) {})
}
`

// TestToolexecTypeDescriptorIsOneSymbol is the identity half of
// specs/032-type-descriptors-and-itabs.md.
//
// It is a build-and-run test rather than a unit test because the claim is
// about the linked binary. A descriptor is deduplicated by cmd/link, by name,
// and only in the non-package index space: a dupok descriptor written as a
// package definition survives beside the runtime's copy, and no test that
// stops at the object file can see that.
func TestToolexecTypeDescriptorIsOneSymbol(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/descriptor\n\ngo 1.27\n",
		"main.go": descriptorIdentityProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "descriptor", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	if b, err := exec.Command(filepath.Join(h.mod, "descriptor")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled did not run: %v\n%s", err, b)
	}
}
