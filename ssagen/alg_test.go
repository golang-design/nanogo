// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssagen

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
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

// TestEqualFuncLoopsOverALongArray checks the shape past the unroll bound.
//
// The two shapes answer the same question and the corpus test below is what
// proves they answer it the same way. This one is that the long shape is a
// loop at all: an unrolling of sixty-four string comparisons would pass every
// behavioural test and be the code path this generator refused to write.
func TestEqualFuncLoopsOverALongArray(t *testing.T) {
	c := check(t, "package main\n\ntype many [64]string\n")
	fn, err := EqualFunc(c.namedType(t, "many"))
	if err != nil {
		t.Fatalf("EqualFunc: %v", err)
	}
	if len(fn.Locals) != 1 {
		t.Fatalf("the function declares %d locals, want the one loop index", len(fn.Locals))
	}
	var fors, compares int
	var walk func(ss []ir.Stmt)
	walk = func(ss []ir.Stmt) {
		for _, s := range ss {
			switch s.Op {
			case ir.OFor:
				fors++
				walk(s.Body)
			case ir.OIf:
				compares++
			}
		}
	}
	walk(fn.Body)
	if fors != 1 {
		t.Errorf("the body holds %d loops, want one", fors)
	}
	if compares != 1 {
		t.Errorf("the loop body holds %d comparisons, want the one element", compares)
	}
}

// TestEqualFuncUnrollsAShortArray is the other shape, so that a change that
// made everything a loop is caught here rather than in a benchmark nobody
// runs.
func TestEqualFuncUnrollsAShortArray(t *testing.T) {
	c := check(t, "package main\n\ntype few [4]string\n")
	fn, err := EqualFunc(c.namedType(t, "few"))
	if err != nil {
		t.Fatalf("EqualFunc: %v", err)
	}
	if len(fn.Locals) != 0 {
		t.Errorf("the function declares %d locals, and an unrolled walk needs none", len(fn.Locals))
	}
	for _, s := range fn.Body {
		if s.Op == ir.OFor {
			t.Fatalf("an array of four was compared with a loop:\n%v", fn.Body)
		}
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
		// Past the unroll bound the walk is a loop, which is the second shape
		// this generator writes and the one nothing above the corpus reaches.
		// The unequal element is the last, so a loop that ran one iteration
		// short would answer equal.
		{"long array of strings", "[64]string", "pair(long(1))", "pair(long(1))", true},
		{"long array of strings, unequal", "[64]string", "pair(long(1))", "pair(long(2))", false},
		// The bound is on elements and not on bytes, so an array of a
		// composite past it walks a nested type once per iteration.
		{"long array of structs", "[20]inner", "pair(longInner(1))", "pair(longInner(1))", true},
		{"long array of structs, unequal", "[20]inner", "pair(longInner(1))", "pair(longInner(2))", false},
		// A short array of an array is two unrolled walks, and a long one of a
		// long one is a loop inside a loop, which is what gives each its own
		// index.
		{"array of long arrays", "[2][20]string", "pair(nested(1))", "pair(nested(1))", true},
		{"array of long arrays, unequal", "[2][20]string", "pair(nested(1))", "pair(nested(2))", false},
		// An interface compares its descriptor and then its value through the
		// runtime. A comparison of the two words would answer on the data
		// pointer, which is a boxed value with an address of its own.
		{"empty interface", "struct {\n\tv any\n\tn int32\n}", "pair{v: 7, n: 1}", "pair{v: 7, n: 1}", true},
		{"empty interface, unequal", "struct {\n\tv any\n\tn int32\n}", "pair{v: 7, n: 1}", "pair{v: 8, n: 1}", false},
		// A complex field is two floats and reaches the default branch, so
		// negative zero in the imaginary part is the same case as above.
		{"complex", "struct {\n\tc complex128\n\tn int32\n}", "pair{c: complex(1, 0), n: 1}", "pair{c: complex(1, negZero), n: 1}", true},
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
				// The last element is the one that differs, so a loop that
				// stopped one short of the length would answer equal.
				"\nfunc long(k int) (a [64]string) {\n\tfor i := range a {\n\t\ta[i] = \"x\"\n\t}\n\ta[63] = string([]byte{byte(96 + k)})\n\treturn\n}\n" +
				"\nfunc longInner(k int) (a [20]inner) {\n\tfor i := range a {\n\t\ta[i] = inner{a: 1, b: 2}\n\t}\n\ta[19].b = int64(k)\n\treturn\n}\n" +
				"\nfunc nested(k int) (a [2][20]string) {\n\tfor i := range a {\n\t\tfor j := range a[i] {\n\t\t\ta[i][j] = \"y\"\n\t\t}\n\t}\n\ta[1][19] = string([]byte{byte(96 + k)})\n\treturn\n}\n" +
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

// TestGeneratedHashAgreesWithEquality checks the one invariant a map depends
// on: two values that compare equal hash alike.
//
// A map that broke it would lose keys it holds, and it would lose them only
// for the values whose bytes differ, which is the failure that does not show
// up in a small test. The cases are exactly the ones where == and the bytes
// disagree: a string with its own data pointer, a negative zero, a blank
// field, and the padding between two fields.
func TestGeneratedHashAgreesWithEquality(t *testing.T) {
	hostRunsNanogoOutput(t)
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)

	tests := []struct {
		name string
		decl string
		x, y string
	}{
		{"padding", "struct {\n\ta int32\n\tb int64\n}", "pair{a: 1, b: 2}", "pair{a: 1, b: 2}"},
		{"blank field", "struct {\n\ta int32\n\t_ int32\n\tb int32\n}", "pair{a: 1, b: 2}", "pair{a: 1, b: 2}"},
		{"string contents", "struct {\n\ts string\n\tn int\n}",
			"pair{s: string([]byte{104, 105}), n: 4}", `pair{s: "hi", n: 4}`},
		{"negative zero", "struct {\n\tf float64\n\tn int32\n}", "pair{f: 0.0, n: 1}", "pair{f: negZero, n: 1}"},
		{"nested", "struct {\n\tin inner\n\tn int32\n}", "pair{in: inner{a: 1, b: 2}, n: 3}", "pair{in: inner{a: 1, b: 2}, n: 3}"},
		{"array of strings", "[2]string", `pair{"a", "b"}`, `pair{"a", string([]byte{98})}`},
		// Past the unroll bound the fold is a loop, which is the other shape
		// this generator writes. Two values that are equal and hold no
		// identical bytes are what says the loop folded every element and not
		// the header.
		{"long array of strings", "[64]string", "pair(long(0))", "pair(longCopy(0))"},
		{"empty interface", "struct {\n\tv any\n\tn int32\n}", "pair{v: 7, n: 1}", "pair{v: 7, n: 1}"},
		{"complex", "struct {\n\tc complex128\n\tn int32\n}", "pair{c: complex(1, 0), n: 1}", "pair{c: complex(1, negZero), n: 1}"},
	}
	for _, tc2 := range tests {
		t.Run(tc2.name, func(t *testing.T) {
			src := "package main\n\ntype inner struct {\n\ta int32\n\tb int64\n}\n\ntype pair " + tc2.decl + "\n"
			c := check(t, src)
			fn, err := HashFunc(c.namedType(t, "pair"))
			if err != nil {
				t.Fatalf("HashFunc: %v", err)
			}
			fn.Sym = "main.hash"
			p := newMainPackage()
			addFull(t, emitFunc(t, c.build(t, fn), p), p)

			defs := src[len("package main\n\n"):] +
				"\nvar zero float64\nvar negZero = zero * -1\n" +
				// Two arrays of equal strings whose bytes are separately
				// built, so no element shares a data pointer with its
				// counterpart.
				"\nfunc long(k int) (a [64]string) {\n\tfor i := range a {\n\t\ta[i] = \"x\"\n\t}\n\ta[63] = string([]byte{byte(96 + k)})\n\treturn\n}\n" +
				"\nfunc longCopy(k int) (a [64]string) {\n\tfor i := range a {\n\t\ta[i] = string([]byte{120})\n\t}\n\ta[63] = string([]byte{byte(96 + k)})\n\treturn\n}\n" +
				"\nfunc b2i(b bool) int {\n\tif b {\n\t\treturn 1\n\t}\n\treturn 0\n}\n" +
				"\nvar x = " + tc2.x + "\nvar y = " + tc2.y + "\n"
			// The seed is the same for both, so a difference is the value and
			// not the seed. The second digit is gc's answer to ==, so the
			// program states the invariant rather than a number.
			caller := exitWrapper(t, goCmd, "main.hash",
				"b2i(hash(&x, 0) == hash(&y, 0))*10+b2i(x == y)",
				defs, "func hash(p *pair, h uintptr) uintptr")
			got := strings.TrimSpace(runLinked(t, goCmd, tc, cfg, p, caller))
			if got != "11" {
				t.Fatalf("the program printed %q, want \"11\": the two digits are whether the hashes agree and whether the values are equal", got)
			}
			t.Logf("two equal values hash alike through the generated function")
		})
	}
}

// TestGeneratedHashIsSeeded checks that the seed reaches the answer.
//
// A generated function that dropped h would return the same value for every
// map, which makes a map that holds these keys degrade to a list.
func TestGeneratedHashIsSeeded(t *testing.T) {
	hostRunsNanogoOutput(t)
	goCmd := goTool(t)
	tc := hostToolchain(t)
	cfg := linkConfig(t)

	src := "package main\n\ntype pair struct {\n\ta int32\n\tb int64\n}\n"
	c := check(t, src)
	fn, err := HashFunc(c.namedType(t, "pair"))
	if err != nil {
		t.Fatalf("HashFunc: %v", err)
	}
	fn.Sym = "main.hash"
	p := newMainPackage()
	addFull(t, emitFunc(t, c.build(t, fn), p), p)

	defs := src[len("package main\n\n"):] +
		"\nfunc b2i(b bool) int {\n\tif b {\n\t\treturn 1\n\t}\n\treturn 0\n}\n" +
		"\nvar x = pair{a: 1, b: 2}\n"
	caller := exitWrapper(t, goCmd, "main.hash", "b2i(hash(&x, 1) != hash(&x, 2))",
		defs, "func hash(p *pair, h uintptr) uintptr")
	got := strings.TrimSpace(runLinked(t, goCmd, tc, cfg, p, caller))
	if got != "1" {
		t.Fatalf("the program printed %q, want \"1\": two seeds gave one hash, so the seed is dropped", got)
	}
}
