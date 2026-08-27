// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package rtype_test

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/rtype"
)

// The refusals, each naming the field of specs/020-ir.md's type boundary that
// is missing.
//
// A refusal here is not the same thing as a refusal of the name. A name is a
// function of the type and is wrong when two Go types share it; a descriptor
// needs the type's contents as well, and the contents are missing for more
// types than the name is. So a type may be nameable, and therefore referable
// from generated code, and still have no writer for its bytes. The two sets
// are kept apart on purpose, and this test is the second of them.

func lay(t *testing.T, ty *ir.Type) *ir.Type {
	t.Helper()
	if err := ir.Layout(ty); err != nil {
		t.Fatalf("layout: %v", err)
	}
	return ty
}

func intType(t *testing.T) *ir.Type {
	return lay(t, &ir.Type{Kind: ir.Int64, Name: "int"})
}

func TestDescriptorRefusals(t *testing.T) {
	for _, tc := range []struct {
		what string
		typ  func(*testing.T) *ir.Type
		want string
	}{
		{
			// ir.Converter sets Methods on every defined type, empty set
			// included, so a defined type whose Methods is nil was built below
			// the type boundary by hand. A descriptor written from it would
			// claim a method set nobody established.
			"a defined type whose method set nobody computed",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Struct, Name: "bytes.Buffer", PkgPath: "bytes"})
			},
			"method set of bytes.Buffer is not in the IR type",
		},
		{
			// A method with a pointer receiver belongs to the pointer's method
			// set, so a pointer to a defined type carries one too.
			"a pointer to a defined type",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Ptr, Elem: &ir.Type{Kind: ir.Struct, Name: "bytes.Buffer", PkgPath: "bytes"}})
			},
			"method set of bytes.Buffer is not in the IR type",
		},
		{
			// A literal struct's spelling holds each unexported field's
			// package and distinguishes an embedded field renamed through an
			// alias, and ir.Converter unaliases before it names.
			"a literal struct",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Struct, Fields: []ir.Field{{Name: "A", Type: intType(t)}}})
			},
			"embedded field renamed through an alias",
		},
		{
			"a channel",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Chan, Elem: intType(t)})
			},
			"direction",
		},
		{
			"a function",
			func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.FuncKind}) },
			"signature",
		},
		{
			// The header's bytes are all computable and the slot group is
			// named as gc names it. What has no descriptor is the group, whose
			// slots are a literal struct, so a map is refused with the group's
			// own reason.
			"a map",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Map, Key: intType(t), Elem: intType(t)})
			},
			"group type",
		},
		{
			"an interface with methods",
			func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Interface}) },
			"type of each method",
		},
		{
			// int and int64 are one ir.Kind and two abi.Kinds, so a word with
			// neither predeclared name is refused. The refusal comes from the
			// name rather than from the kind, because a type with no name has
			// no canonical spelling either, and that check runs first.
			"a word with no predeclared name",
			func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Int64}) },
			"no canonical name",
		},
		{
			"an unsigned word with no predeclared name",
			func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Uint64}) },
			"no canonical name",
		},
		{
			// Beyond internal/abi.MaxPtrmaskBytes gc stops emitting a bitmask
			// and writes a symbol the runtime fills in on demand, which needs
			// a BSS symbol and the runtime's cooperation.
			"a type past the ptrmask bound",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Array, Len: 200, Elem: &ir.Type{Kind: ir.Ptr, Elem: intType(t)}})
			},
			"on-demand mask",
		},
		{
			"a void",
			func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Void}) },
			"no canonical name",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			_, err := rtype.Descriptor(tc.typ(t))
			if err == nil {
				t.Fatal("a descriptor was emitted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the reason is %q, want it to name %q", err, tc.want)
			}
		})
	}
	if _, err := rtype.Descriptor(nil); err == nil {
		t.Error("a descriptor for a nil type was emitted")
	}
	if _, err := rtype.Hash(nil); err == nil {
		t.Error("a hash for a nil type was computed")
	}
}

// TestDescriptorOfAnArrayOfDefinedElements checks that a refusal of the
// element does not become a descriptor with a dangling reference.
//
// An array's descriptor names the element's and the slice of the element's, so
// a name the element cannot supply has to stop the array as well.
func TestDescriptorOfAnArrayOfDefinedElements(t *testing.T) {
	// The element is nameable and its own descriptor is not emittable, which
	// is the case the reference is for: the defining package emits it.
	el := lay(t, &ir.Type{Kind: ir.Ptr, Elem: &ir.Type{Kind: ir.Struct, Name: "bytes.Buffer"}})
	arr := lay(t, &ir.Type{Kind: ir.Array, Len: 2, Elem: el})
	syms, err := rtype.Descriptor(arr)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	var targets []string
	for _, r := range syms[0].Relocs {
		targets = append(targets, r.Target)
	}
	for _, want := range []string{"type:*bytes.Buffer", "type:[]*bytes.Buffer"} {
		found := false
		for _, got := range targets {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not referenced; the references are %v", want, targets)
		}
	}

	// An element with no name stops the array.
	bad := lay(t, &ir.Type{Kind: ir.Array, Len: 2, Elem: lay(t, &ir.Type{Kind: ir.FuncKind})})
	if _, err := rtype.Descriptor(bad); err == nil {
		t.Error("an array of functions was emitted")
	}
}

// TestNameDataSymbols checks the name data symbol against the names gc gives
// the same symbols.
//
// The spellings were read out of a gc-compiled object with go tool nm. The
// separator and the flag bit both say whether the name is exported, so that
// two names differing only in that do not share a symbol, and reflect reads
// the bit.
//
// unsafe.Pointer is in the table because it was wrong. gc treats it as a
// defined type whose symbol is Pointer in package unsafe, so it is exported;
// an earlier draft here treated it as a predeclared type and wrote a trailing
// dash, which is a second symbol for a string that already has one.
func TestNameDataSymbols(t *testing.T) {
	for _, tc := range []struct {
		what     string
		typ      func(*testing.T) *ir.Type
		sym      string
		exported bool
	}{
		{
			"int",
			func(t *testing.T) *ir.Type { return intType(t) },
			"type:.namedata.*int-", false,
		},
		{
			"unsafe.Pointer",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.UnsafePtr, Name: "unsafe.Pointer"})
			},
			"type:.namedata.*unsafe.Pointer.", true,
		},
		{
			"a slice",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Slice, Elem: intType(t)})
			},
			"type:.namedata.*[]int-", false,
		},
		{
			// The name string of a pointer already starts with a star, so no
			// star is added and TFlagExtraStar is not set. The exported bit is
			// then the element's, because the string the two share is named
			// after the element, and *bytes.Buffer has no name of its own.
			//
			// This is not the exported-defined-type case. That one is
			// type:*bytes.Buffer, whose descriptor carries the method set of
			// bytes.Buffer and is refused; unsafe.Pointer above is the only
			// exported name this package can reach today.
			"a pointer to a pointer to a defined type",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Ptr, Elem: &ir.Type{Kind: ir.Ptr,
					Elem: &ir.Type{Kind: ir.Struct, Name: "bytes.Buffer"}}})
			},
			"type:.namedata.**bytes.Buffer-", false,
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			syms, err := rtype.Descriptor(tc.typ(t))
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			r, ok := reloc(syms[0], 40)
			if !ok {
				t.Fatal("no relocation for Str")
			}
			if r.Target != tc.sym {
				t.Errorf("the name data is %s, want %s", r.Target, tc.sym)
			}
			nd, ok := find(syms, r.Target)
			if !ok {
				t.Fatalf("%s is not emitted; the symbols are %v", r.Target, names(syms))
			}
			if got := nd.Data[0]&1 != 0; got != tc.exported {
				t.Errorf("the exported bit is %v, want %v", got, tc.exported)
			}
		})
	}
}

// TestExtraStarFollowsTheNameString checks the flag that says the name data
// holds one character more than the type's name.
//
// A pointer type's name already begins with a star, so gc reuses the string
// whole and leaves the flag clear. Every other type has the star prepended and
// the flag set, so that the descriptors of T and *T share one string.
func TestExtraStarFollowsTheNameString(t *testing.T) {
	const extraStar = 1 << 1
	slice := lay(t, &ir.Type{Kind: ir.Slice, Elem: intType(t)})
	ptr := lay(t, &ir.Type{Kind: ir.Ptr, Elem: slice})
	for _, tc := range []struct {
		what string
		typ  *ir.Type
		want bool
	}{
		{"a slice", slice, true},
		{"a pointer", ptr, false},
	} {
		syms, err := rtype.Descriptor(tc.typ)
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		if got := syms[0].Data[20]&extraStar != 0; got != tc.want {
			t.Errorf("%s: TFlagExtraStar is %v, want %v", tc.what, got, tc.want)
		}
	}
}

// TestGCBitsIsNamedByItsContent checks the bitmask symbol's name and padding.
//
// The name is the content in hexadecimal, which is gc's, so a mask nanogo
// emits and one gc emits for the same shape are one symbol in the linked
// binary. The length is a multiple of the pointer size because the runtime
// reads a ptrmask as a sequence of words.
func TestGCBitsIsNamedByItsContent(t *testing.T) {
	for _, tc := range []struct {
		what string
		typ  func(*testing.T) *ir.Type
		sym  string
	}{
		{"a word with no pointers", func(t *testing.T) *ir.Type { return intType(t) }, "runtime.gcbits."},
		{"a pointer", func(t *testing.T) *ir.Type {
			return lay(t, &ir.Type{Kind: ir.Ptr, Elem: intType(t)})
		}, "runtime.gcbits.0100000000000000"},
		{"a slice", func(t *testing.T) *ir.Type {
			return lay(t, &ir.Type{Kind: ir.Slice, Elem: intType(t)})
		}, "runtime.gcbits.0100000000000000"},
		{"an array of two pointers", func(t *testing.T) *ir.Type {
			return lay(t, &ir.Type{Kind: ir.Array, Len: 2, Elem: &ir.Type{Kind: ir.Ptr, Elem: intType(t)}})
		}, "runtime.gcbits.0300000000000000"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			syms, err := rtype.Descriptor(tc.typ(t))
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			r, ok := reloc(syms[0], 32)
			if !ok {
				t.Fatal("no relocation for GCData")
			}
			if r.Target != tc.sym {
				t.Errorf("the bitmask is %s, want %s", r.Target, tc.sym)
			}
		})
	}
}

// TestEqualityFunctions checks the closure each comparable shape reaches.
//
// specs/032 says Equal is "a function the compiler must emit, not a runtime
// helper". That is true for a struct or an array whose parts do not compare as
// one region of memory, and it is not true for anything else: every shape
// below reaches a function the runtime already has, and emitting one per type
// would be work the runtime does.
func TestEqualityFunctions(t *testing.T) {
	str := &ir.Type{Kind: ir.String, Name: "string"}
	for _, tc := range []struct {
		what string
		typ  func(*testing.T) *ir.Type
		want string
	}{
		{"a word", func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Slice, Elem: intType(t)}) }, ""},
		{"a byte", func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Uint8, Name: "uint8"}) }, "runtime.memequal8·f"},
		{"two bytes", func(t *testing.T) *ir.Type {
			return lay(t, &ir.Type{Kind: ir.Array, Len: 2, Elem: &ir.Type{Kind: ir.Uint8, Name: "uint8"}})
		}, "runtime.memequal16·f"},
		{"four bytes", func(t *testing.T) *ir.Type {
			return lay(t, &ir.Type{Kind: ir.Array, Len: 4, Elem: &ir.Type{Kind: ir.Uint8, Name: "uint8"}})
		}, "runtime.memequal32·f"},
		{"sixteen bytes", func(t *testing.T) *ir.Type {
			return lay(t, &ir.Type{Kind: ir.Array, Len: 16, Elem: &ir.Type{Kind: ir.Uint8, Name: "uint8"}})
		}, "runtime.memequal128·f"},
		{"an empty array", func(t *testing.T) *ir.Type {
			return lay(t, &ir.Type{Kind: ir.Array, Len: 0, Elem: str})
		}, "runtime.memequal0·f"},
		{"a one-element array of strings", func(t *testing.T) *ir.Type {
			return lay(t, &ir.Type{Kind: ir.Array, Len: 1, Elem: str})
		}, "runtime.strequal·f"},
		{"a float", func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Float32, Name: "float32"}) }, "runtime.f32equal·f"},
		{"a double", func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Float64, Name: "float64"}) }, "runtime.f64equal·f"},
		{"a complex", func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Complex64, Name: "complex64"}) }, "runtime.c64equal·f"},
		{"a double complex", func(t *testing.T) *ir.Type { return lay(t, &ir.Type{Kind: ir.Complex128, Name: "complex128"}) }, "runtime.c128equal·f"},
		{
			// A region of a size the fixed-width forms do not cover reaches
			// memequal_varlen, which reads the size out of its closure. The
			// closure is data, so no function is generated for it.
			"a three-byte region",
			func(t *testing.T) *ir.Type {
				return lay(t, &ir.Type{Kind: ir.Array, Len: 3, Elem: &ir.Type{Kind: ir.Uint8, Name: "uint8"}})
			},
			"type:.eqfunc.M3",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			syms, err := rtype.Descriptor(tc.typ(t))
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			r, ok := reloc(syms[0], 24)
			if tc.want == "" {
				if ok {
					t.Errorf("Equal points at %s and the type is not comparable", r.Target)
				}
				return
			}
			if !ok {
				t.Fatal("Equal is nil")
			}
			if r.Target != tc.want {
				t.Errorf("Equal points at %s, want %s", r.Target, tc.want)
			}
			closure, ok := find(syms, r.Target)
			if !ok {
				t.Fatalf("the closure %s is not emitted", r.Target)
			}
			if strings.HasPrefix(r.Target, "type:.eqfunc.") {
				// The varlen closure carries the size in its second word.
				if len(closure.Data) != 16 {
					t.Errorf("the closure is %d bytes, want 16", len(closure.Data))
				}
			} else if len(closure.Data) != 8 {
				t.Errorf("the closure is %d bytes, want 8", len(closure.Data))
			}
		})
	}
}

func names(syms []rtype.Symbol) []string {
	var out []string
	for _, s := range syms {
		out = append(out, s.Name)
	}
	return out
}

// TestDescriptorOfAnAlias checks the two predeclared aliases.
//
// types2 declares byte and rune as basic types of their own carrying the alias
// spelling, so a type reaches this package named "byte" and not "uint8". A
// table of predeclared names without them answers that byte is a defined type,
// which is a type that might have methods, and the descriptor of []byte is
// refused because its element is.
func TestDescriptorOfAnAlias(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind ir.Kind
		want string
	}{
		{"byte", ir.Uint8, "type:uint8"},
		{"rune", ir.Int32, "type:int32"},
	} {
		alias := lay(t, &ir.Type{Kind: tc.kind, Name: tc.name})
		syms, err := rtype.Descriptor(alias)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if syms[0].Name != tc.want {
			t.Errorf("%s is described as %s, want %s", tc.name, syms[0].Name, tc.want)
		}
		if rtype.RuntimeOwned(alias) != true {
			t.Errorf("%s is not recognised as a type the runtime owns", tc.name)
		}
		// The slice is what the corpus reaches: a descriptor for []byte names
		// its element, so an element that cannot be described stops it.
		slice := lay(t, &ir.Type{Kind: ir.Slice, Elem: alias})
		if _, err := rtype.Descriptor(slice); err != nil {
			t.Errorf("[]%s: %v", tc.name, err)
		}
	}
}
