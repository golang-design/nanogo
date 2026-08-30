// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/ssa"
)

// abiWrapperSource declares the signatures specs/047-abi-wrappers.md measured
// against the real toolchain, each one bodyless.
//
// The declarations have no body because that is the shape the wrapper is owed
// for: an assembly file defines the symbol under ABI0 and a Go call names the
// ABIInternal symbol of the same name, which is the wrapper.
const abiWrapperSource = `package main

func getisar0() uint64

func g1(a int8) (r int8)

func g2(a int8, b int8) (r1 int8, r2 int32)

func sysctlEnabled(name []byte) bool

func g4(a ...int) int

func gof(a int8, b int64, c string, d float64) (r1 int32, r2 [3]int64)

func main() {}
`

// TestABIWrapperOutgoingAreaIsTheABI0Area is the measurement the whole stage
// turns on.
//
// The size of the outgoing area a wrapper's inner call needs is the ABI0 area
// of the callee, which is specs/047-abi-wrappers.md's recurrence and its six
// measured rows. Under ABIInternal the same signatures need almost no area at
// all, because the registers hold the values, so the two answers are far
// apart and a wrapper that took the wrong one writes its arguments where the
// assembly reads nothing.
//
// The rows were read out of gc: each is the args= field of the TEXT line of
// the ABI0 definition, cross-checked against the offsets gc's own wrapper
// loads and stores.
func TestABIWrapperOutgoingAreaIsTheABI0Area(t *testing.T) {
	c := check(t, abiWrapperSource)
	want := map[string]int64{
		"main.getisar0":      8,
		"main.g1":            16,
		"main.g2":            16,
		"main.sysctlEnabled": 32,
		"main.g4":            32,
		"main.gof":           72,
	}
	seen := 0
	for _, decl := range c.ir.Funcs {
		size, ok := want[decl.Sym]
		if !ok {
			continue
		}
		seen++
		fn, err := ABIWrapper(decl, decl.Sym)
		if err != nil {
			t.Fatalf("ABIWrapper(%s): %v", decl.Sym, err)
		}
		got := c.build(t, fn)
		call := abiWrapperCall(t, got.f)
		rec := got.f.ABI.CallAt(call.ID)
		if rec == nil {
			t.Fatalf("%s: the call has no recorded placement", decl.Sym)
		}
		if rec.Size != size {
			t.Errorf("%s: the wrapper's outgoing area is %d bytes, want the ABI0 area of %d",
				decl.Sym, rec.Size, size)
		}
		// Nothing travels in a register, which is what ABI0 is.
		for i := range rec.Vals {
			if rec.Vals[i].InReg {
				t.Errorf("%s: value %d of the call is in a register, and ABI0 has none", decl.Sym, i)
			}
		}
	}
	if seen != len(want) {
		t.Errorf("%d of the %d declarations were found in the package", seen, len(want))
	}
}

// TestABIWrapperCallsTheAssemblyUnderABI0 checks the other half of the
// callee's identity.
//
// The wrapper and the assembly share a name and differ only by convention, so
// the relocation the wrapper emits has to name ABI0. Under ABIInternal it
// names the wrapper itself, and the wrapper calls itself until the stack runs
// out.
func TestABIWrapperCallsTheAssemblyUnderABI0(t *testing.T) {
	c := check(t, abiWrapperSource)
	decl := abiWrapperDecl(t, c, "main.sysctlEnabled")
	fn, err := ABIWrapper(decl, decl.Sym)
	if err != nil {
		t.Fatalf("ABIWrapper: %v", err)
	}
	got := c.build(t, fn)
	call := abiWrapperCall(t, got.f)
	target, err := callTarget(call.Aux)
	if err != nil {
		t.Fatalf("callTarget: %v", err)
	}
	if target.name != "main.sysctlEnabled" {
		t.Errorf("the wrapper calls %s, want main.sysctlEnabled", target.name)
	}
	if target.abi != obj.ABI0 {
		t.Errorf("the wrapper calls %s at ABI %d, want ABI0", target.name, target.abi)
	}
	// The wrapper's own text symbol is ABIInternal, which is what a Go call
	// names, so the two definitions of one name are kept apart by the
	// convention alone.
	p := obj.NewPackage("main")
	r := emitFunc(t, got, p)
	if r.Text.ABI != obj.ABIInternal {
		t.Errorf("the wrapper is defined at ABI %d, want ABIInternal", r.Text.ABI)
	}
	if r.Text.Name != "main.sysctlEnabled" {
		t.Errorf("the wrapper is named %s, want main.sysctlEnabled", r.Text.Name)
	}
}

// TestABIWrapperRefusesWhatItDoesNotBuild covers the two shapes that would be
// a wrong answer rather than a missing one.
func TestABIWrapperRefusesWhatItDoesNotBuild(t *testing.T) {
	sig := &ir.Type{Kind: ir.FuncKind, Name: "func()"}
	recv := &ir.Object{Name: "r", Class: ir.ClassParam, Type: sig}
	for _, tc := range []struct {
		name string
		decl *ir.Func
		want string
	}{
		{"no declaration", nil, "no declaration"},
		{"no signature", &ir.Func{Name: "f", Bodyless: true}, "the signature"},
		{"a method", &ir.Func{Name: "M", Type: sig, Recv: recv, Bodyless: true}, "for a method"},
		{"a Go body", &ir.Func{Name: "f", Type: sig}, "a Go body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ABIWrapper(tc.decl, "main.f")
			if err == nil {
				t.Fatal("ABIWrapper built it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the message does not carry %q: %v", tc.want, err)
			}
		})
	}
}

// abiWrapperCall returns the one static call of a compiled wrapper.
func abiWrapperCall(t *testing.T, f *ssa.Func) *ssa.Value {
	t.Helper()
	var out *ssa.Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !v.Op.IsCall() {
				continue
			}
			if o, _ := v.Aux.(*ir.Object); o == nil || !o.Assembly {
				continue
			}
			if out != nil {
				t.Fatalf("%s holds more than one call to the assembly", f.Sym)
			}
			out = v
		}
	}
	if out == nil {
		t.Fatalf("%s holds no call to the assembly", f.Sym)
	}
	return out
}

// abiWrapperDecl finds a bodyless declaration by its symbol.
func abiWrapperDecl(t *testing.T, c *checked, sym string) *ir.Func {
	t.Helper()
	for _, fn := range c.ir.Funcs {
		if fn.Sym == sym {
			return fn
		}
	}
	t.Fatalf("%s is not declared in the package", sym)
	return nil
}
