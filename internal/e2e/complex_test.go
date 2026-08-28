// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// The complex rows of specs/020-ir.md's lowering table, measured against gc.
//
// A complex number is the one arithmetic type whose value is two machine
// values, so every row that touches it is a rewrite and not a machine
// operation. The rewrites round differently from the obvious ones: a
// multiplication is computed in float64 whatever the width of the operands,
// and a division is a call to runtime.complex128div rather than a formula.
// Neither difference is visible except in the last bits of the answer, which
// is why gc is the oracle and the assertion is the printed digits.

// complexProgram exercises every complex row in one program.
//
// The constants are chosen so that the two roundings differ. 1e20 squared
// overflows float32 and not float64, so a complex64 multiplication computed in
// float32 gives an infinity where gc gives a finite number. 1e30/1e-30 is the
// division the naive formula overflows on and runtime.complex128div does not.
const complexProgram = `package main

var globalC128 complex128 = 3 + 4i
var globalC64 complex64 = 1.5 - 2.5i
var globalZero complex128

func add(a, b complex128) complex128 { return a + b }
func mul(a, b complex64) complex64   { return a * b }
func div(a, b complex128) complex128 { return a / b }

func main() {
	// The three builtins.
	c := complex(float64(3), float64(4))
	println(real(c), imag(c))
	println(c)

	// Arithmetic, through a call so that the operands are not constants the
	// type checker folds before this compiler sees them.
	d := complex(float64(1), float64(2))
	println(add(c, d))
	println(c - d)
	println(c * d)
	println(div(c, d))
	println(-c)
	println(+c)

	// The rounding a complex64 multiplication has. Computed in float32 the
	// products overflow and the answer is an infinity.
	small := complex(float32(1e20), float32(1e20))
	println(mul(small, small))

	// The division the naive formula overflows on.
	println(div(complex(1e30, 1e30), complex(1e-30, 1e-30)))
	println(div(complex(1.0, 1.0), complex(0.0, 0.0)))

	// Conversions between the two widths, and back.
	e := complex64(c)
	println(e, real(e), imag(e))
	println(complex128(e))

	// Comparison, which ssa/decompose.go builds out of the parts.
	println(c == d, c == c, c != d, c == complex(3.0, 4.0))

	// Constants, at both widths and at package scope.
	var z complex128 = 2 + 3i
	var w complex64 = 1.5 - 2.5i
	println(z, w)
	println(globalC128, globalC64, globalZero)

	// A zero value declared and never assigned.
	var zero128 complex128
	var zero64 complex64
	println(zero128, zero64)

	// Storage: an array, a slice and a struct field are all two floats side
	// by side, so a compiler that got the layout wrong reads the wrong half.
	arr := [3]complex128{1 + 1i, 2 + 2i, 3 + 3i}
	sl := []complex64{4 + 4i, 5 + 5i}
	println(arr[0], arr[1], arr[2])
	println(sl[0], sl[1])
	type pair struct {
		A complex128
		B complex64
	}
	p := pair{A: c, B: e}
	println(p.A, p.B)
	q := p
	q.A = q.A * 2
	println(p.A, q.A)

	// Through a pointer, which is where an assignment writes both halves.
	pp := &p
	pp.A = pp.A + 1i
	println(p.A)

	// A map key. -0 and +0 are the same key and a NaN is no key at all,
	// which is the float rule applied to each half.
	m := map[complex128]string{}
	m[1+2i] = "a"
	m[3+4i] = "b"
	println(len(m), m[1+2i], m[3+4i], m[0])
	m[complex(0, 0)] = "zero"
	println(len(m), m[complex(negZero(), 0)])
	m64 := map[complex64]string{}
	m64[1+2i] = "narrow"
	println(len(m64), m64[complex(float32(1), float32(2))])

	// An interface. A complex is neither an integer nor a float, so it is
	// copied into the heap by address rather than by one of the by-value
	// helpers, and a classification that took it for one eight-byte scalar
	// would box the real half alone.
	var i128 any = c
	var i64 any = e
	println(i128.(complex128), i64.(complex64))
	println(i128.(complex128) == c, i64.(complex64) == e)

	// A channel, which copies through memory the same way.
	ch := make(chan complex128, 1)
	ch <- c * d
	println(<-ch)

	// A closure capture, which is a heap cell holding both halves.
	acc := complex128(0)
	bump := func(v complex128) { acc = acc + v }
	bump(1 + 1i)
	bump(2 + 2i)
	println(acc)

	// A variadic argument, which packs into a slice of complex.
	println(sum(1+1i, 2+2i, 3+3i))
}

func sum(vs ...complex128) complex128 {
	total := complex128(0)
	for _, v := range vs {
		total += v
	}
	return total
}

func negZero() float64 { return -0.0 }
`

// TestComplexMatchesGc builds the program with nanogo and with the installed
// compiler and compares the digits both print.
//
// No normalisation. Every line is a number the two compilers must agree on to
// the last bit, and a rounding this pass got wrong shows as a differing digit
// rather than as a crash.
func TestComplexMatchesGc(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/complex\n\ngo 1.27\n",
		"main.go": complexProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "cx", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", err, out)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}

	got := string(runProgram(t, filepath.Join(h.mod, "cx")))
	want := string(gcOutput(t, h))
	if got != want {
		t.Errorf("nanogo's program printed\n%s\nand gc's printed\n%s", got, want)
	}
	// The parts of the answer that are the same in every implementation, so
	// that two compilers agreeing on the wrong answer is still a failure.
	// (3+4i)*(1+2i) is -5+10i, and (3+4i)/(1+2i) is 2.2-0.4i.
	//
	// (0+Infi) is the complex64 multiplication and it is the discriminator
	// for where the products are computed. 1e20*1e20 is 1e40, which is finite
	// in float64 and an infinity in float32, so the real half is 1e40-1e40.
	// Computed in float64 that is zero and computed in float32 it is Inf-Inf,
	// which is a NaN. The imaginary half overflows on the way back to float32
	// either way.
	//
	// (1e+60+0i) is the division the naive formula cannot do: the square of
	// the modulus of 1e-30+1e-30i underflows to zero and every half of the
	// answer would be a NaN.
	for _, want := range []string{"(3+4i)", "(-5+10i)", "(2.2-0.4i)", "(0+Infi)", "(1e+60+0i)", "(+Inf+Infi)"} {
		if !strings.Contains(got, want) {
			t.Errorf("the output has no %q:\n%s", want, got)
		}
	}
}
