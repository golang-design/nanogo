// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
)

func TestEqualSymbolIsTheTypesOwn(t *testing.T) {
	c := check(t, "package main\n\ntype pair struct {\n\ta int32\n\tb int64\n}\n")
	got, err := EqualSymbol(c.namedType(t, "pair"))
	if err != nil {
		t.Fatal(err)
	}
	// gc names it TypeSymPrefix(".eq", t): the type prefix, the family and the
	// link string. Two packages that generate it generate one symbol.
	if want := "type:.eq.main.pair"; got != want {
		t.Errorf("the symbol is %q, want %q", got, want)
	}
}

// TestEqualFuncRefusesTheUncomparable checks that a type the language forbids
// == on is refused rather than compared.
func TestEqualFuncRefusesTheUncomparable(t *testing.T) {
	c := check(t, "package main\n\ntype holder struct {\n\ts []int\n}\n")
	if _, err := EqualFunc(c.namedType(t, "holder")); err == nil {
		t.Fatal("a struct holding a slice produced an equality function")
	}
}

// TestEqualFuncRefusesALongArray checks that the case that needs a loop says
// so rather than emitting a function of a different shape.
func TestEqualFuncRefusesALongArray(t *testing.T) {
	c := check(t, "package main\n\ntype many [64]string\n")
	_, err := EqualFunc(c.namedType(t, "many"))
	if err == nil {
		t.Fatal("an array of 64 strings was unrolled")
	}
	if !strings.Contains(err.Error(), "loop") {
		t.Errorf("the refusal is %q and does not name the missing loop", err)
	}
}

// TestGeneratedEqualAgreesWithGc links a generated equality function and asks
// it and gc the same question about the same two values.
//
// gc is the oracle and the program compares the two answers, so nothing here
// carries an expected value that can go stale. This is the gate for the whole
// file: the runtime calls this function for every lookup in a map whose key is
// one of these types, and an answer that differs from == is a map that loses
// keys it holds.
func TestGeneratedEqualAgreesWithGc(t *testing.T) {
	hostRunsNanogoOutput(t)
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)

	tests := []struct {
		name string
		decl string
		// x and y are the two values, as Go expressions of the declared type.
		x, y string
		// equal is what gc answers, and what the generated function must
		// answer as well. The program checks both, so this only records what
		// the case is for.
		equal bool
	}{
		// Padding between the fields. The four bytes after a hold whatever
		// the last write left there, so a comparison of the bytes can say
		// unequal where == says equal.
		{"padding, equal", "struct {\n\ta int32\n\tb int64\n}", "pair{a: 1, b: 2}", "pair{a: 1, b: 2}", true},
		{"padding, unequal", "struct {\n\ta int32\n\tb int64\n}", "pair{a: 1, b: 2}", "pair{a: 1, b: 3}", false},
		// A blank field is not compared, so two values that differ in it are
		// equal. A comparison of the bytes says otherwise.
		{"blank field", "struct {\n\ta int32\n\t_ int32\n\tb int32\n}", "pair{a: 1, b: 2}", "pair{a: 1, b: 2}", true},
		// A string compares by its contents and not by its header, so two
		// equal strings with different data pointers are equal.
		{"string, equal", "struct {\n\ts string\n\tn int\n}",
			"pair{s: string([]byte{104, 105}), n: 4}", `pair{s: "hi", n: 4}`, true},
		{"string, unequal", "struct {\n\ts string\n\tn int\n}", `pair{s: "hi", n: 4}`, `pair{s: "ho", n: 4}`, false},
		// A float compares by the floating-point rule. Negative zero and
		// positive zero are equal and their bytes are not.
		{"negative zero", "struct {\n\tf float64\n\tn int32\n}", "pair{f: 0.0, n: 1}", "pair{f: negZero, n: 1}", true},
		// And a NaN is equal to nothing, its own bytes included.
		{"not a number", "struct {\n\tf float64\n\tn int32\n}", "pair{f: nan, n: 1}", "pair{f: nan, n: 1}", false},
		// A nested struct is walked rather than compared whole, so the
		// padding inside it is skipped as well.
		{"nested", "struct {\n\tin inner\n\tn int32\n}", "pair{in: inner{a: 1, b: 2}, n: 3}", "pair{in: inner{a: 1, b: 2}, n: 3}", true},
		// An array of strings: every element compares by its contents.
		{"array of strings", "[2]string", `pair{"a", "b"}`, `pair{"a", string([]byte{98})}`, true},
		{"array of strings, unequal", "[2]string", `pair{"a", "b"}`, `pair{"a", "c"}`, false},
	}
	for _, tc2 := range tests {
		t.Run(tc2.name, func(t *testing.T) {
			src := "package main\n\ntype inner struct {\n\ta int32\n\tb int64\n}\n\ntype pair " + tc2.decl + "\n"
			c := check(t, src)
			fn, err := EqualFunc(c.namedType(t, "pair"))
			if err != nil {
				t.Fatalf("EqualFunc: %v", err)
			}
			// The generated symbol carries a colon, which no Go identifier
			// can spell, so the caller gc compiles names it "main.eq". The
			// name is not what this proves; the body is.
			fn.Sym = "main.eq"
			p := newMainPackage()
			addFull(t, emitFunc(t, c.build(t, fn), p), p)

			// The two floating-point constants are built rather than
			// imported, because go tool compile runs here without an
			// -importcfg and cannot find package math.
			defs := src[len("package main\n\n"):] +
				"\nvar zero float64\nvar nan = zero / zero\nvar negZero = zero * -1\n" +
				"\nfunc b2i(b bool) int {\n\tif b {\n\t\treturn 1\n\t}\n\treturn 0\n}\n" +
				"\nvar x = " + tc2.x + "\nvar y = " + tc2.y + "\n"
			caller := exitWrapper(t, goCmd, "main.eq", "b2i(eq(&x, &y))*10+b2i(x == y)",
				defs, "func eq(p, q *pair) bool")
			got := strings.TrimSpace(runLinked(t, goCmd, tc, cfg, p, caller))
			want := "0"
			if tc2.equal {
				want = "11"
			}
			if got != want {
				t.Fatalf("the program printed %q, want %q: the two digits are the generated function's answer and gc's", got, want)
			}
			t.Logf("the generated equality function and gc agree that the two values are equal=%v", tc2.equal)
		})
	}
}

// TestGeneratedEqualIsOneSymbolPerType checks that the function is emitted
// under a duplicate-tolerant name, so that two packages that need it do not
// define one symbol twice.
func TestGeneratedEqualIsOneSymbolPerType(t *testing.T) {
	c := check(t, "package main\n\ntype pair struct {\n\ta int32\n\tb int64\n}\n")
	fn, err := EqualFunc(c.namedType(t, "pair"))
	if err != nil {
		t.Fatal(err)
	}
	if !fn.Wrapper {
		t.Error("the function is not marked a wrapper, so a panic raised inside it counts its frame")
	}
	p := newMainPackage()
	r := emitFunc(t, c.build(t, fn), p)
	if r.Text.Name != "type:.eq.main.pair" {
		t.Errorf("the symbol is %q, want type:.eq.main.pair", r.Text.Name)
	}
	if r.Text.ABI != obj.ABIInternal {
		t.Errorf("the function is ABI %d, want ABIInternal", r.Text.ABI)
	}
	if len(r.Text.Data) == 0 {
		t.Error("the function has no instructions")
	}
}
