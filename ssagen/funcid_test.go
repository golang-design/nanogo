// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"encoding/binary"
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
)

// TestFuncIDWrapperMatchesTheRuntime reads the runtime's own list.
//
// A funcID is a position in a const block and nothing in the object says which
// name it stood for, so a value that drifts produces a program that runs and
// recovers in the wrong places. runtime.gorecover counts the frames between
// itself and runtime.gopanic and skips every frame whose funcID is
// FuncIDWrapper, so a wrong value here turns "defer f(x)" where f recovers
// into a program that does not recover, with nothing said at compile time.
//
// rtsym checks its symbols against the runtime's source for the same reason.
func TestFuncIDWrapperMatchesTheRuntime(t *testing.T) {
	path := filepath.Join(build.Default.GOROOT, "src", "internal", "abi", "symtab.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no runtime source to check against: %v", err)
	}
	// The block starts at FuncIDNormal, which is the iota, and each later
	// name is the next value.
	want := -1
	n := -1
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "FuncIDNormal"):
			n = 0
		case n < 0 || line == "" || strings.HasPrefix(line, "//"):
			continue
		case strings.HasPrefix(line, "FuncID"):
			n++
		case line == ")":
			n = -1
			continue
		default:
			continue
		}
		if strings.HasPrefix(line, "FuncIDWrapper") {
			want = n
		}
	}
	if want < 0 {
		t.Fatalf("%s does not name FuncIDWrapper", path)
	}
	if FuncIDWrapper != want {
		t.Errorf("FuncIDWrapper is %d and the runtime puts it at %d", FuncIDWrapper, want)
	}
}

// TestFuncInfoCarriesTheWrapperID checks the byte reaches the object.
//
// The mark travels from ir.Func through ssa.Func to the FuncInfo auxiliary
// symbol, and the middle of that path is where it would be dropped without
// anything failing.
func TestFuncInfoCarriesTheWrapperID(t *testing.T) {
	const src = "package main\n\nfunc g(a int) int\n\nfunc f(a int) int { return g(a) }\n"
	for _, wrapper := range []bool{false, true} {
		c := compile(t, src, "f")
		c.f.Wrapper = wrapper
		r := emit(t, c, obj.NewPackage("main"))
		if r.FuncInfo == nil || len(r.FuncInfo.Data) < 12 {
			t.Fatal("the function has no FuncInfo")
		}
		want := byte(0)
		if wrapper {
			want = FuncIDWrapper
		}
		if got := r.FuncInfo.Data[8]; got != want {
			t.Errorf("Wrapper=%v gave funcID %d, want %d", wrapper, got, want)
		}
		// The two words in front of the identifier are the sizes, and a byte
		// written into the wrong one would be read as a frame size.
		if got := binary.LittleEndian.Uint32(r.FuncInfo.Data[4:]); got != uint32(r.Locals) {
			t.Errorf("the FuncInfo says %d locals and the frame has %d", got, r.Locals)
		}
	}
}
