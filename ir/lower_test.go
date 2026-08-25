// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"strings"
	"testing"

	"golang.design/x/nanogo/syntax"
)

// specs/020-ir.md's lowering table as a checklist, one case per row.
//
// A case is Go source, built and lowered, and the assertion is on the tree the
// pass produced. The tree is the contract: specs/021-ssa-construction.md reads
// it and has no other statement of what a lowered range or a lowered len looks
// like.

// lowerPrelude is the package every case is built into. It is separate from
// buildPrelude because that one declares a variadic function, and a variadic
// call packs its arguments into a slice literal, which is a row this pass
// refuses. A case must fail for its own row and no other.
const lowerPrelude = `package p

import "unsafe"

var _ unsafe.Pointer

type T struct {
	A, B int
}

type P struct {
	A int
	S []int
}

type Arr struct {
	A [4]int
}

var g int
var gp *int
var gs []int

func arr2() [2][4]int { return [2][4]int{} }

func mkslice() []int { return gs }

func use(int)      {}
func useAny(any)   {}
func two() (int, int) { return 1, 2 }
`

// lowerFunc builds one function from source and lowers it.
//
// The returned error is the pass's, so a case that asserts a refusal reads the
// same helper as one that asserts a tree.
func lowerFunc(t *testing.T, body, name string) (*Func, error) {
	t.Helper()
	pkg, files, info := buildTypecheck(t, lowerPrelude+"\n"+body)
	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	fn := buildFuncOf(t, out, name)
	return fn, Lower(fn)
}

// lowerOK lowers and fails when the pass refused, or when a Go-specific node
// survived. The second half is the invariant this pass exists to satisfy.
func lowerOK(t *testing.T, body string) *Func {
	t.Helper()
	fn, err := lowerFunc(t, body, "f")
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	for _, s := range fn.Body {
		if op, ok := HasGoSpecific(s); ok {
			t.Fatalf("%s survived lowering:\n%s", op, buildDump(fn))
		}
	}
	return fn
}

// lowerCalls returns the names of the runtime functions a lowered body calls.
func lowerCalls(fn *Func) []string {
	var out []string
	for _, s := range fn.Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OCall && n.X != nil && n.X.Op == OGlobal && n.X.Obj != nil &&
				strings.HasPrefix(n.X.Obj.Name, "runtime.") {
				out = append(out, n.X.Obj.Name)
			}
			return true
		})
	}
	return out
}

func lowerCalled(fn *Func, name string) bool {
	for _, c := range lowerCalls(fn) {
		if c == name {
			return true
		}
	}
	return false
}

// headerRead reports whether n reads field index of a header, which is the
// shape every slice and string row produces: the address of the value, read as
// a pointer to a struct with the header's fields.
func headerRead(n *Node, index int, fields int) bool {
	if n == nil || n.Op != OField || n.Index != index || n.X == nil {
		return false
	}
	p := n.X
	if p.Op != OConvert || p.Type == nil || p.Type.Kind != Ptr || p.Type.Elem == nil {
		return false
	}
	h := p.Type.Elem
	return h.Kind == Struct && len(h.Fields) == fields && p.X != nil && p.X.Op == OAddr
}

func TestLowerHeaderRows(t *testing.T) {
	for _, tc := range []struct {
		row    string
		body   string
		index  int
		fields int
	}{
		{"len of a slice", `func f(s []int) int { return len(s) }`, 1, 3},
		{"cap of a slice", `func f(s []int) int { return cap(s) }`, 2, 3},
		{"len of a string", `func f(s string) int { return len(s) }`, 1, 2},
		{"unsafe.SliceData", `func f(s []int) *int { return unsafe.SliceData(s) }`, 0, 3},
		{"unsafe.StringData", `func f(s string) *byte { return unsafe.StringData(s) }`, 0, 2},
	} {
		t.Run(tc.row, func(t *testing.T) {
			fn := lowerOK(t, tc.body)
			ret := fn.Body[len(fn.Body)-1]
			if ret.Op != OReturn || len(ret.Args) != 1 {
				t.Fatalf("the body does not end in a return of one value:\n%s", buildDump(fn))
			}
			if !headerRead(ret.Args[0], tc.index, tc.fields) {
				t.Errorf("the result is not field %d of a %d-field header:\n%s",
					tc.index, tc.fields, buildDump(fn))
			}
		})
	}
}

// TestLowerHeaderLayout checks the header against the type it describes.
//
// A header that disagrees with the layout reads the wrong word, and the
// disagreement is silent: every field is a machine word and every offset is a
// legal one.
func TestLowerHeaderLayout(t *testing.T) {
	l := &lowerer{fn: &Func{Name: "f"}, ptrs: make(map[*Type]*Type), hdrs: make(map[*Type]*Type)}
	slice := &Type{Kind: Slice, Elem: lowerInt}
	str := &Type{Kind: String}
	for _, tc := range []struct {
		t      *Type
		fields []string
	}{
		{slice, []string{"ptr", "len", "cap"}},
		{str, []string{"ptr", "len"}},
	} {
		if err := Layout(tc.t); err != nil {
			t.Fatal(err)
		}
		h := l.header(tc.t)
		if h == nil {
			t.Fatalf("no header for %s", tc.t.Kind)
		}
		if h.Size != tc.t.Size || h.Align != tc.t.Align {
			t.Errorf("the %s header is %d/%d and the type is %d/%d",
				tc.t.Kind, h.Size, h.Align, tc.t.Size, tc.t.Align)
		}
		if len(h.Fields) != len(tc.fields) {
			t.Fatalf("the %s header has %d fields, want %d", tc.t.Kind, len(h.Fields), len(tc.fields))
		}
		for i, name := range tc.fields {
			if h.Fields[i].Name != name {
				t.Errorf("field %d is %s, want %s", i, h.Fields[i].Name, name)
			}
			if h.Fields[i].Offset != int64(i)*PtrSize {
				t.Errorf("field %s is at %d, want %d", name, h.Fields[i].Offset, int64(i)*PtrSize)
			}
		}
		// The cache answers with one type, so two reads of one header are one
		// type and compare by pointer.
		if l.header(tc.t) != h {
			t.Errorf("the %s header is not cached", tc.t.Kind)
		}
	}
	if l.header(lowerInt) != nil {
		t.Error("an int has a header")
	}
}

// TestLowerConstantLengthRows is the row for an array and a pointer to one:
// a constant, with the operand still evaluated for its effects.
func TestLowerConstantLengthRows(t *testing.T) {
	for _, tc := range []struct {
		row  string
		body string
		want int64
	}{
		{"len of an array", `func f() int { var a [4]int; return len(a) }`, 4},
		{"cap of an array", `func f() int { var a [7]int; return cap(a) }`, 7},
		{"len of a pointer to an array", `func f(p *[9]int) int { return len(p) }`, 9},
		{"len of an array reached through a field", `func f(a Arr) int { return len(a.A) }`, 4},
		// The operand names no storage of its own, so it is spilled rather
		// than dropped: the row says it is still evaluated for its effects,
		// and the checker leaves it unfolded exactly because it calls.
		{"len of an array a call produced", `func f() int { return len(arr2()[0]) }`, 4},
		{"cap of a pointer to an array", `func f(p *[3]int) int { return cap(p) }`, 3},
	} {
		t.Run(tc.row, func(t *testing.T) {
			fn := lowerOK(t, tc.body)
			ret := fn.Body[len(fn.Body)-1]
			got := ret.Args[0]
			if got.Op != OConst {
				t.Fatalf("the result is a %s, want a constant:\n%s", got.Op, buildDump(fn))
			}
			v, ok := got.Val.(ConstValue)
			if !ok {
				t.Fatal("the constant has no value")
			}
			if n, exact := v.Int64(); !exact || n != tc.want {
				t.Errorf("the length is %v, want %d", got.Val, tc.want)
			}
		})
	}
}

// TestLowerKeepsTheOperandOfAConstantLength is the second half of that row.
//
// The checker folds len of an array wherever the language says it is constant,
// so an operand reaching the pass is one the language says is not, which is
// exactly where it may call a function.
func TestLowerKeepsTheOperandOfAConstantLength(t *testing.T) {
	fn := lowerOK(t, `func arr() [4]int { return [4]int{} }
func f() int { return len(arr()) }`)
	found := false
	for _, s := range fn.Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OCall {
				found = true
			}
			return true
		})
	}
	if !found {
		t.Errorf("the call was dropped:\n%s", buildDump(fn))
	}
}

// TestLowerCompositeLiteralRows is the frame form of specs/020's composite
// literal row: an allocation plus element stores.
func TestLowerCompositeLiteralRows(t *testing.T) {
	t.Run("struct", func(t *testing.T) {
		fn := lowerOK(t, `func f() int { t := T{1, 2}; return t.A + t.B }`)
		var stores []*Node
		for _, s := range fn.Body {
			if s.Op == OAssign && s.X != nil && s.X.Op == OField {
				stores = append(stores, s)
			}
		}
		if len(stores) != 2 {
			t.Fatalf("%d field stores, want 2:\n%s", len(stores), buildDump(fn))
		}
		for i, s := range stores {
			if s.X.Index != i {
				t.Errorf("store %d writes field %d", i, s.X.Index)
			}
		}
		if lowerCalled(fn, "runtime.memclrNoHeapPointers") {
			t.Error("a literal that writes every field also cleared the storage")
		}
	})

	t.Run("array, every element written", func(t *testing.T) {
		fn := lowerOK(t, `func f() int { a := [3]int{4, 5, 6}; return a[0] }`)
		var stores []*Node
		for _, s := range fn.Body {
			if s.Op == OAssign && s.X != nil && s.X.Op == OIndex {
				stores = append(stores, s)
			}
		}
		if len(stores) != 3 {
			t.Fatalf("%d element stores, want 3:\n%s", len(stores), buildDump(fn))
		}
		if lowerCalled(fn, "runtime.memclrNoHeapPointers") {
			t.Error("a literal that writes every element also cleared the storage")
		}
	})

	t.Run("array with a keyed element", func(t *testing.T) {
		// The language gives an element with no key the index after the
		// previous one, so 2 and 3 are written and the rest are zero.
		fn := lowerOK(t, `func f() int { a := [8]int{2: 7, 8}; return a[3] }`)
		at := map[int64]bool{}
		for _, s := range fn.Body {
			if s.Op == OAssign && s.X != nil && s.X.Op == OIndex && s.X.Y != nil {
				if v, ok := s.X.Y.Val.(ConstValue); ok {
					if n, exact := v.Int64(); exact {
						at[n] = true
					}
				}
			}
		}
		for i := int64(0); i < 8; i++ {
			if !at[i] {
				t.Errorf("element %d is never written:\n%s", i, buildDump(fn))
			}
		}
	})

	t.Run("array too large to fill with stores", func(t *testing.T) {
		fn := lowerOK(t, `func f() int { a := [64]int{2: 7}; return a[3] }`)
		if !lowerCalled(fn, "runtime.memclrNoHeapPointers") {
			t.Errorf("the array was not cleared:\n%s", buildDump(fn))
		}
	})

	t.Run("an array of pointers is cleared with the barrier form", func(t *testing.T) {
		fn := lowerOK(t, `func f() *int { a := [64]*int{2: gp}; return a[3] }`)
		if !lowerCalled(fn, "runtime.memclrHasPointers") {
			t.Errorf("the array was cleared without the barrier form:\n%s", buildDump(fn))
		}
	})

	t.Run("a struct field the literal left out", func(t *testing.T) {
		// The builder writes an omitted field out as its zero value, and the
		// zero of a struct is a literal with no elements.
		fn := lowerOK(t, `type Q struct{ A int; B T }
func f() int { q := Q{A: 1}; return q.A }`)
		if !lowerCalled(fn, "runtime.memclrNoHeapPointers") {
			t.Errorf("the omitted field was not cleared:\n%s", buildDump(fn))
		}
	})
}

// lowerDescriptors returns the type descriptor symbols a lowered body names.
func lowerDescriptors(fn *Func) []string {
	var out []string
	for _, s := range fn.Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OGlobal && n.Obj != nil && strings.HasPrefix(n.Obj.Name, TypeSymbolPrefix) {
				out = append(out, n.Obj.Name)
			}
			return true
		})
	}
	return out
}

func lowerNames(fn *Func, name string) bool {
	for _, d := range lowerDescriptors(fn) {
		if d == name {
			return true
		}
	}
	return false
}

// TestLowerAllocationRows is specs/020's heap rows, which
// specs/032-type-descriptors-and-itabs.md unblocked.
//
// Each row is checked twice: the runtime symbol it calls, and the descriptor
// symbol it passes. The second is the half that specs/032 makes load bearing,
// because the linker deduplicates a descriptor by name and a name that differs
// from gc's is a second descriptor for a type that already has one.
func TestLowerAllocationRows(t *testing.T) {
	for _, tc := range []struct {
		row  string
		body string
		call string
		desc string
	}{
		{"new", `func f() *int { return new(int) }`, "runtime.newobject", "type:int"},
		{"new of a defined type", `func f() *T { return new(T) }`, "runtime.newobject", "type:p.T"},
		{"the address of a literal", `func f() *T { return &T{1, 2} }`, "runtime.newobject", "type:p.T"},
		{"the address of an array literal", `func f() *[2]int { return &[2]int{1, 2} }`, "runtime.newobject", "type:[2]int"},
		{"a slice literal", `func f() []int { return []int{1, 2} }`, "runtime.newarray", "type:int"},
		{"a slice literal of pointers", `func f() []*T { return []*T{nil} }`, "runtime.newarray", "type:*p.T"},
		{"an empty slice literal", `func f() []int { return []int{} }`, "runtime.newarray", "type:int"},
		{"a keyed slice literal", `func f() []int { return []int{5: 1} }`, "runtime.newarray", "type:int"},
		{"make", `func f() []int { return make([]int, 4) }`, "runtime.makeslice", "type:int"},
		{"make with a capacity", `func f(n int) []int { return make([]int, n, 2*n) }`, "runtime.makeslice", "type:int"},
		{"make of a slice of empty interfaces", `func f(n int) []any { return make([]any, n) }`, "runtime.makeslice", "type:interface {}"},
	} {
		t.Run(tc.row, func(t *testing.T) {
			fn := lowerOK(t, tc.body)
			if !lowerCalled(fn, tc.call) {
				t.Errorf("%s was not called; the calls are %v:\n%s",
					tc.call, lowerCalls(fn), buildDump(fn))
			}
			if !lowerNames(fn, tc.desc) {
				t.Errorf("%s was not named; the descriptors are %v:\n%s",
					tc.desc, lowerDescriptors(fn), buildDump(fn))
			}
		})
	}
}

// TestLowerSliceLiteralLength checks the length a keyed slice literal produces.
//
// It is one past the largest index written and not the number of elements:
// []int{5: 1} holds one element and has length six. An earlier draft used the
// element count, which produced a slice whose only element was out of bounds.
func TestLowerSliceLiteralLength(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int64
	}{
		{`func f() []int { return []int{} }`, 0},
		{`func f() []int { return []int{1, 2, 3} }`, 3},
		{`func f() []int { return []int{5: 1} }`, 6},
		{`func f() []int { return []int{2: 1, 9} }`, 4},
	} {
		fn := lowerOK(t, tc.body)
		got := int64(-1)
		for _, s := range fn.Body {
			Walk(s, func(n *Node) bool {
				if n.Op != OCall || n.X == nil || n.X.Obj == nil ||
					n.X.Obj.Name != "runtime.newarray" || len(n.Args) != 2 {
					return true
				}
				if c, ok := n.Args[1].Val.(ConstValue); ok {
					got, _ = c.Int64()
				}
				return true
			})
		}
		if got != tc.want {
			t.Errorf("%s: newarray of %d, want %d", tc.body, got, tc.want)
		}
	}
}

// TestLowerSliceLiteralStoresThroughTheAllocation checks that the elements are
// written into the heap and not into the frame.
//
// The frame form is the corruption specs/023-escape-analysis.md would have to
// rule out and cannot, because it is not built: a header returned from this
// function would point at a slot the caller has already reused.
func TestLowerSliceLiteralStoresThroughTheAllocation(t *testing.T) {
	fn := lowerOK(t, `func f() []int { return []int{1, 2} }`)
	// The allocation is assigned to a temporary of pointer-to-array type, and
	// every element store indexes off that same temporary.
	var alloc *Object
	for _, s := range fn.Body {
		if s.Op == OAssign && s.X != nil && s.X.Op == OLocal && s.Y != nil &&
			s.X.Type != nil && s.X.Type.Kind == Ptr && s.X.Type.Elem != nil &&
			s.X.Type.Elem.Kind == Array {
			alloc = s.X.Obj
		}
	}
	if alloc == nil {
		t.Fatalf("no pointer to the allocated array:\n%s", buildDump(fn))
	}
	stores := 0
	for _, s := range fn.Body {
		if s.Op != OAssign || s.X == nil || s.X.Op != OIndex || s.X.X == nil {
			continue
		}
		if s.X.X.Op == OLocal && s.X.X.Obj == alloc {
			stores++
		}
	}
	if stores != 2 {
		t.Errorf("%d elements stored through the allocation, want 2:\n%s", stores, buildDump(fn))
	}
}

// TestLowerEmptySliceLiteralIsNotNil is the semantics of []T{}.
//
// The language distinguishes []int{} from a nil slice: the first is non-nil
// and the second is not. runtime.newarray(t, 0) reaches mallocgc with size
// zero, which returns the address of runtime.zerobase rather than nil, so the
// call is what gives the empty literal a non-nil pointer. Building the header
// with a nil pointer instead would be one comparison different from Go and
// nothing downstream would report it.
func TestLowerEmptySliceLiteralIsNotNil(t *testing.T) {
	fn := lowerOK(t, `func f() []int { return []int{} }`)
	if !lowerCalled(fn, "runtime.newarray") {
		t.Errorf("the empty literal did not allocate; the calls are %v:\n%s",
			lowerCalls(fn), buildDump(fn))
	}
}

// TestLowerNamesNoDescriptorOfItsOwnTypes checks that the types this pass
// invents stay out of the collected set.
//
// The pass builds types the checker never produced: the slice and string
// headers, the pointer to a descriptor, the descriptor itself. None of them is
// a Go type and none has a descriptor gc would emit, so a name for one would
// be a symbol the linker never resolves.
func TestLowerNamesNoDescriptorOfItsOwnTypes(t *testing.T) {
	_, types, err := lowerCollect(t, `func f(s []int) []int { p := new(int); use(*p); return []int{s[0]} }`)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	for _, ty := range types {
		name, err := TypeSymbol(ty)
		if err != nil {
			t.Errorf("the collected type %s has no name: %v", ty, err)
			continue
		}
		for _, bad := range []string{"type:runtime._type", "type:slice", "type:string."} {
			if name == bad {
				t.Errorf("the pass asked for %s, which gc never emits", name)
			}
		}
	}
}

// TestLowerCollectsTheDescriptors checks the list a caller emits from.
//
// Nothing between this pass and the object writer carries a list of data
// symbols, so LowerAndCollect returns one. Its order is the order the names
// were first met, which specs/053-determinism.md requires of anything that
// reaches output, and it holds one entry per name rather than one per use.
func TestLowerCollectsTheDescriptors(t *testing.T) {
	body := `func f() []*T { p := new(int); q := new(int); use(*p + *q); return []*T{nil} }`
	_, types, err := lowerCollect(t, body)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var got []string
	for _, ty := range types {
		name, err := TypeSymbol(ty)
		if err != nil {
			t.Fatalf("the collected type %s has no name: %v", ty, err)
		}
		got = append(got, name)
	}
	// int is named twice by the two calls to new and appears once, and the
	// order is the order of first use.
	want := []string{"type:int", "type:*p.T"}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("collected %v, want %v", got, want)
			break
		}
	}

	// Two runs over the same input collect the same list.
	_, again, err := lowerCollect(t, body)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(again) != len(types) {
		t.Errorf("a second run collected %d types and the first collected %d", len(again), len(types))
	}
}

// TestLowerCollectsOnARefusal checks that the list survives a refusal.
//
// A function that is refused is still lowered everywhere else, so a descriptor
// its lowered part names still has to be emitted. Returning an empty list on a
// refusal would drop it.
func TestLowerCollectsOnARefusal(t *testing.T) {
	_, types, err := lowerCollect(t, `func f(s []int) []int { p := new(int); use(*p); return append(s, 1) }`)
	if err == nil {
		t.Fatal("append was lowered")
	}
	if len(types) != 1 {
		t.Fatalf("collected %d types, want the one new named", len(types))
	}
}

// lowerCollect lowers and returns the collected descriptor types.
func lowerCollect(t *testing.T, body string) (*Func, []*Type, error) {
	t.Helper()
	pkg, files, info := buildTypecheck(t, lowerPrelude+"\n"+body)
	out, err := Build(pkg, files, info)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	fn := buildFuncOf(t, out, "f")
	types, lerr := LowerAndCollect(fn)
	return fn, types, lerr
}

// TestLowerRangeRows is specs/020's range rows: an index loop with the bound
// hoisted, and a counted loop for an integer.
func TestLowerRangeRows(t *testing.T) {
	for _, tc := range []struct {
		row      string
		body     string
		constant bool // the bound is a constant rather than a hoisted read
	}{
		{"range over a slice", `func f(s []int) { for i, v := range s { use(i + v) } }`, false},
		{"range over an array", `func f() { var a [4]int; for i, v := range a { use(i + v) } }`, true},
		{"range over a pointer to an array", `func f(p *[4]int) { for i, v := range p { use(i + v) } }`, true},
		{"range over an integer", `func f(n int) { for i := range n { use(i) } }`, false},
	} {
		t.Run(tc.row, func(t *testing.T) {
			fn := lowerOK(t, tc.body)
			var loop *Node
			for _, s := range fn.Body {
				if s.Op == OFor {
					loop = s
				}
			}
			if loop == nil {
				t.Fatalf("the range did not become a loop:\n%s", buildDump(fn))
			}
			// The setup is in the loop's own init list, and the index is
			// declared last, after everything the bound needs.
			if len(loop.Init) == 0 {
				t.Fatalf("the loop has no init list:\n%s", buildDump(fn))
			}
			last := loop.Init[len(loop.Init)-1]
			if last.Op != OAssign || last.Op1 != syntax.Def {
				t.Errorf("the loop does not open by declaring its index:\n%s", buildDump(fn))
			}
			// Nothing of the range is emitted in front of the loop, because a
			// label names the statement after it and the loop needs the name.
			for _, st := range fn.Body {
				if st.Op == OAssign && st.X != nil && st.X.Obj != nil &&
					strings.HasPrefix(st.X.Obj.Name, ".lowertmp_") {
					t.Errorf("the range setup was emitted in front of the loop:\n%s", buildDump(fn))
				}
			}
			if loop.X == nil || loop.X.Op != OCompare || loop.X.Op1 != syntax.Lss {
				t.Errorf("the condition is not an ordered comparison:\n%s", buildDump(fn))
			}
			if tc.constant != (loop.X.Y != nil && loop.X.Y.Op == OConst) {
				t.Errorf("the bound is %v, want constant=%v:\n%s", loop.X.Y.Op, tc.constant, buildDump(fn))
			}
			// The increment is in the post list and not at the end of the
			// body, because continue reaches the post list and does not reach
			// the end of the body.
			if len(loop.Post) != 1 || loop.Post[0].Op != OAssign ||
				loop.Post[0].Y == nil || loop.Post[0].Y.Op != OBinary ||
				loop.Post[0].Y.Op1 != syntax.Add {
				t.Errorf("the index is not incremented in the post list:\n%s", buildDump(fn))
			}
			if len(loop.Body) == 0 || loop.Body[0].Op != OAssign {
				t.Errorf("the body does not open by writing the iteration variables:\n%s", buildDump(fn))
			}
		})
	}
}

// TestLowerRangeReadsTheElementOnlyWhenItIsAsked is the part of the row that
// says what the loop body opens with.
func TestLowerRangeReadsTheElementOnlyWhenItIsAsked(t *testing.T) {
	one := lowerOK(t, `func f(s []int) { for i := range s { use(i) } }`)
	two := lowerOK(t, `func f(s []int) { for i, v := range s { use(i + v) } }`)
	count := func(fn *Func) int {
		n := 0
		for _, s := range fn.Body {
			if s.Op != OFor {
				continue
			}
			for _, b := range s.Body {
				if b.Op == OAssign && b.Y != nil && b.Y.Op == OIndex {
					n++
				}
			}
		}
		return n
	}
	if got := count(one); got != 0 {
		t.Errorf("a range with one variable reads the element %d times", got)
	}
	if got := count(two); got != 1 {
		t.Errorf("a range with two variables reads the element %d times, want 1", got)
	}
}

// TestLowerRangeKeepsThePerIterationVariable is the Go 1.22 rule.
//
// The builder owns it: perIteration points the clause's destinations at a
// carrier and opens the body with the declaration that copies the carrier into
// the per-iteration variable. What the pass owes it is to write the
// destinations before that declaration and to leave it where it is. Prepending
// after it would give every iteration the previous iteration's value.
func TestLowerRangeKeepsThePerIterationVariable(t *testing.T) {
	fn := lowerOK(t, `func f(s []int) { for i := range s { gp = &i } }`)
	var loop *Node
	for _, s := range fn.Body {
		if s.Op == OFor {
			loop = s
		}
	}
	if loop == nil {
		t.Fatalf("no loop:\n%s", buildDump(fn))
	}
	if len(loop.Body) < 2 {
		t.Fatalf("the body has %d statements:\n%s", len(loop.Body), buildDump(fn))
	}
	carrier := loop.Body[0]
	if carrier.Op != OAssign || carrier.X == nil || carrier.X.Obj == nil ||
		!strings.HasPrefix(carrier.X.Obj.Name, ".loopvar_") {
		t.Errorf("the body does not open by writing the carrier:\n%s", buildDump(fn))
	}
	decl := loop.Body[1]
	if decl.Op != OAssign || decl.Op1 != syntax.Def || decl.Y == nil ||
		decl.Y.Obj == nil || !strings.HasPrefix(decl.Y.Obj.Name, ".loopvar_") {
		t.Errorf("the per-iteration declaration is not the next statement:\n%s", buildDump(fn))
	}
	if decl.X == nil || decl.X.Obj == nil || decl.X.Obj.Name != "i" {
		t.Errorf("the declaration does not name the loop variable:\n%s", buildDump(fn))
	}
}

// TestLowerRangeEvaluatesTheOperandOnce is what the row means by hoisted: the
// specification evaluates the range expression exactly once, so an assignment
// in the body cannot change what is iterated.
func TestLowerRangeEvaluatesTheOperandOnce(t *testing.T) {
	fn := lowerOK(t, `func f(s []int) { for i := range s { s = nil; use(i) } }`)
	for _, st := range fn.Body {
		if st.Op != OFor {
			continue
		}
		if st.X == nil || st.X.Y == nil || st.X.Y.Op != OLocal || st.X.Y.Obj == nil ||
			!strings.HasPrefix(st.X.Y.Obj.Name, ".lowertmp_") {
			t.Errorf("the bound is not a hoisted temporary:\n%s", buildDump(fn))
		}
	}
}

// TestLowerSliceExpressionRows is specs/020's row: bounds checks plus pointer
// arithmetic.
func TestLowerSliceExpressionRows(t *testing.T) {
	for _, tc := range []struct {
		row    string
		body   string
		guards int
		fields int
	}{
		{"the whole slice", `func f(s []int) []int { return s[:] }`, 0, 3},
		{"a low bound", `func f(s []int, a int) []int { return s[a:] }`, 2, 3},
		{"both bounds", `func f(s []int, a, b int) []int { return s[a:b] }`, 3, 3},
		{"three bounds", `func f(s []int, a, b, c int) []int { return s[a:b:c] }`, 4, 3},
		{"a constant low bound", `func f(s []int) []int { return s[2:] }`, 1, 3},
		{"a string", `func f(s string, a int) string { return s[a:] }`, 2, 2},
		{"a pointer to an array", `func f(p *[8]int) []int { return p[2:4] }`, 2, 3},
	} {
		t.Run(tc.row, func(t *testing.T) {
			fn := lowerOK(t, tc.body)
			guards, stores := 0, 0
			for _, s := range fn.Body {
				if s.Op == OIf {
					guards++
				}
				if s.Op == OAssign && s.X != nil && headerRead(s.X, s.X.Index, tc.fields) {
					stores++
				}
			}
			if guards != tc.guards {
				t.Errorf("%d bounds checks, want %d:\n%s", guards, tc.guards, buildDump(fn))
			}
			if stores != tc.fields {
				t.Errorf("%d header stores, want %d:\n%s", stores, tc.fields, buildDump(fn))
			}
			if !lowerCalled(fn, "runtime.goPanicSliceB") && tc.guards > 0 &&
				!lowerCalled(fn, "runtime.goPanicSliceAlen") {
				t.Errorf("no bounds check calls a panic:\n%s", buildDump(fn))
			}
		})
	}
}

// TestLowerSliceExpressionMasksTheOffset is the part of the row that is not an
// optimisation: an empty result must not point one past the end of the object.
func TestLowerSliceExpressionMasksTheOffset(t *testing.T) {
	fn := lowerOK(t, `func f(s []int, a int) []int { return s[a:] }`)
	masked := false
	for _, st := range fn.Body {
		Walk(st, func(n *Node) bool {
			if n.Op == OBinary && n.Op1 == syntax.And && n.Y != nil &&
				n.Y.Op == OUnary && n.Y.Op1 == syntax.Sub {
				masked = true
			}
			return true
		})
	}
	if !masked {
		t.Errorf("the offset is not masked by the result capacity:\n%s", buildDump(fn))
	}
}

// TestLowerRuntimeRows is the rows that become one call.
func TestLowerRuntimeRows(t *testing.T) {
	for _, tc := range []struct {
		row  string
		body string
		want string
	}{
		{"close", `func f(c chan int) { close(c) }`, "runtime.closechan"},
		{"panic", `func f(v any) { panic(v) }`, "runtime.gopanic"},
		{"copy", `func f(d, s []int) int { return copy(d, s) }`, "runtime.memmove"},
		{"copy from a string", `func f(d []byte, s string) { copy(d, s) }`, "runtime.memmove"},
		{"clear of a slice", `func f(s []int) { clear(s) }`, "runtime.memclrNoHeapPointers"},
		{"clear of a slice of pointers", `func f(s []*int) { clear(s) }`, "runtime.memclrHasPointers"},
	} {
		t.Run(tc.row, func(t *testing.T) {
			fn := lowerOK(t, tc.body)
			if !lowerCalled(fn, tc.want) {
				t.Errorf("%s was not called; the calls are %v:\n%s",
					tc.want, lowerCalls(fn), buildDump(fn))
			}
		})
	}
}

// TestLowerMinMaxRow is specs/020's row: a compare and a select per operand,
// left to right.
func TestLowerMinMaxRow(t *testing.T) {
	for _, tc := range []struct {
		row  string
		body string
		op   syntax.Operator
		n    int
	}{
		{"min of two", `func f(a, b int) int { return min(a, b) }`, syntax.Lss, 1},
		{"min of three", `func f(a, b, c int) int { return min(a, b, c) }`, syntax.Lss, 2},
		{"max of two", `func f(a, b int) int { return max(a, b) }`, syntax.Gtr, 1},
		{"min of strings", `func f(a, b string) string { return min(a, b) }`, syntax.Lss, 1},
	} {
		t.Run(tc.row, func(t *testing.T) {
			fn := lowerOK(t, tc.body)
			n := 0
			for _, s := range fn.Body {
				if s.Op == OIf && s.X != nil && s.X.Op == OCompare && s.X.Op1 == tc.op {
					n++
				}
			}
			if n != tc.n {
				t.Errorf("%d comparisons, want %d:\n%s", n, tc.n, buildDump(fn))
			}
		})
	}
}

// TestLowerUnsafeAddRow is specs/020's row: pointer arithmetic, not a call.
func TestLowerUnsafeAddRow(t *testing.T) {
	fn := lowerOK(t, `func f(p unsafe.Pointer, n int32) unsafe.Pointer { return unsafe.Add(p, n) }`)
	got := fn.Body[len(fn.Body)-1].Args[0]
	if got.Op != OBinary || got.Op1 != syntax.Add {
		t.Fatalf("the result is a %s:\n%s", got.Op, buildDump(fn))
	}
	// The offset was written with an integer type of its own, which the
	// specification leaves free, so it is widened rather than assumed to be a
	// machine word.
	if got.Y == nil || got.Y.Op != OConvert || got.Y.Type.Size != PtrSize {
		t.Errorf("the offset is not widened to a word:\n%s", buildDump(fn))
	}
	if len(lowerCalls(fn)) != 0 {
		t.Errorf("unsafe.Add reached the runtime: %v", lowerCalls(fn))
	}
}

// TestLowerRefusals is the other half of the table: the rows that are not
// built, each refused for its own reason and named in the report.
//
// A row is refused rather than approximated. The heap rows are the ones this
// matters most for: every allocation symbol rtsym holds takes a *_type, which
// specs/032-type-descriptors-and-itabs.md does not produce, and a frame slot
// whose address outlives the frame is memory corruption.
func TestLowerRefusals(t *testing.T) {
	for _, tc := range []struct {
		row  string
		body string
		op   Op
		want string
	}{
		{"a map literal", `func f() map[int]int { return map[int]int{1: 2} }`, OCompositeLit, "makemap"},
		{"append", `func f(s []int) []int { return append(s, 1) }`, OAppend, "no row"},
		// The allocation rows are built, and each still refuses a type whose
		// descriptor cannot be named. The reason names the field
		// specs/020-ir.md's type boundary drops, so that a count by cause says
		// which field is holding the row back rather than only that a name was
		// wanted.
		{"make of a map", `func f() map[int]int { return make(map[int]int) }`, OMake, "descriptor"},
		{"make of a channel", `func f() chan int { return make(chan int) }`, OMake, "descriptor"},
		{"new of a literal struct", `func f() *struct{ A int } { return new(struct{ A int }) }`, ONew, "field tags"},
		{"new of a function type", `func f() *func() { return new(func()) }`, ONew, "signature"},
		{"a slice literal of channels", `func f() []chan int { return []chan int{nil} }`, OCompositeLit, "direction"},
		{"len of a map", `func f(m map[int]int) int { return len(m) }`, OLen, "the length of map"},
		{"len of a channel", `func f(c chan int) int { return len(c) }`, OLen, "the length of chan"},
		{"range over a map", `func f(m map[int]int) { for k := range m { use(k) } }`, ORange, "mapiterinit"},
		{"range over a string", `func f(s string) { for i := range s { use(i) } }`, ORange, "UTF-8"},
		{"range over a channel", `func f(c chan int) { for v := range c { use(v) } }`, ORange, "channel"},
		{"a closure", `func f() func() { return func() {} }`, OClosure, "no row"},
		{"defer", `func f() { defer use(1) }`, ODefer, "no row"},
		{"a type assertion", `func f(v any) int { return v.(int) }`, OTypeAssert, "no row"},
		{"min of floats", `func f(a, b float64) float64 { return min(a, b) }`, OMin, "NaN"},
		{"clear of a map", `func f(m map[int]int) { clear(m) }`, OClear, "mapclear"},
		{"range over a function", `func f(it func(func(int) bool)) { for v := range it { use(v) } }`, ORange, "range over func"},
	} {
		t.Run(tc.row, func(t *testing.T) {
			fn, err := lowerFunc(t, tc.body, "f")
			if err == nil {
				t.Fatalf("the row was lowered:\n%s", buildDump(fn))
			}
			le, ok := err.(*LowerError)
			if !ok {
				t.Fatalf("the error is a %T: %v", err, err)
			}
			if le.Op != tc.op {
				t.Errorf("the refusal names %s, want %s", le.Op, tc.op)
			}
			if !strings.Contains(le.What, tc.want) {
				t.Errorf("the reason is %q, want it to name %q", le.What, tc.want)
			}
			// A refused row leaves its node in place, so that construction
			// reports it too rather than building a tree with a hole in it.
			found := false
			for _, s := range fn.Body {
				if op, ok := HasGoSpecific(s); ok && op == tc.op {
					found = true
				}
			}
			if !found {
				t.Errorf("the refused %s was removed anyway:\n%s", tc.op, buildDump(fn))
			}
		})
	}
}

// TestLowerReportsTheFirstRefusal checks that a function with two refusals is
// counted once, under the first one.
func TestLowerReportsTheFirstRefusal(t *testing.T) {
	// Two refusals, in source order. append is the earlier one, so it is the
	// one the count is grouped under; the map literal after it is not counted
	// again. Both rows are chosen because neither is built, and the test moves
	// to another pair when one of them is.
	_, err := lowerFunc(t, `func f(s []int) int { c := append(s, 1); m := map[int]int{1: 2}; return c[0] + m[1] }`, "f")
	le, ok := err.(*LowerError)
	if !ok {
		t.Fatalf("the error is a %T: %v", err, err)
	}
	if le.Op != OAppend {
		t.Errorf("the refusal names %s, want %s", le.Op, OAppend)
	}
	if !strings.Contains(le.Error(), "lowering f") {
		t.Errorf("the message does not name the function: %s", le.Error())
	}
	if le.Cause() != "append: "+le.What {
		t.Errorf("the cause is %q", le.Cause())
	}
}

// TestLowerContexts checks the places a temporary may not be hoisted to.
//
// The right operand of && and a loop condition are evaluated somewhere other
// than where the statement holding them is, so anything they need goes into
// their own Init and not into the enclosing list.
func TestLowerContexts(t *testing.T) {
	t.Run("the right operand of &&", func(t *testing.T) {
		fn := lowerOK(t, `func f(a bool, s []int) bool { return a && len(s) > 0 }`)
		ret := fn.Body[len(fn.Body)-1]
		and := ret.Args[0]
		if and.Op != OBinary || and.Op1 != syntax.AndAnd {
			t.Fatalf("the result is a %s:\n%s", and.Op, buildDump(fn))
		}
		// Nothing the right operand needs may be in the enclosing list.
		for _, s := range fn.Body {
			if s.Op == OAssign && s.X != nil && s.X.Obj != nil &&
				strings.HasPrefix(s.X.Obj.Name, ".lowertmp_") {
				t.Errorf("a temporary of the right operand was hoisted:\n%s", buildDump(fn))
			}
		}
	})

	t.Run("a loop condition", func(t *testing.T) {
		fn := lowerOK(t, `func f(s []int) { for len(s) > 0 { s = nil } }`)
		var loop *Node
		for _, s := range fn.Body {
			if s.Op == OFor {
				loop = s
			}
		}
		if loop == nil || loop.X == nil {
			t.Fatalf("no loop with a condition:\n%s", buildDump(fn))
		}
		// The condition reads the header on every iteration, so the read is
		// inside the condition rather than before the loop.
		if _, ok := HasGoSpecific(loop.X); ok {
			t.Errorf("the condition was not lowered:\n%s", buildDump(fn))
		}
		read := false
		Walk(loop.X, func(n *Node) bool {
			if headerRead(n, 1, 3) {
				read = true
			}
			return true
		})
		if !read {
			t.Errorf("the condition does not read the length:\n%s", buildDump(fn))
		}
	})

	t.Run("a case expression", func(t *testing.T) {
		fn := lowerOK(t, `func f(a int, s []int) int {
	switch a {
	case len(s):
		return 1
	}
	return 0
}`)
		var sw *Node
		for _, s := range fn.Body {
			if s.Op == OSwitch {
				sw = s
			}
		}
		if sw == nil {
			t.Fatalf("no switch:\n%s", buildDump(fn))
		}
		if _, ok := HasGoSpecific(sw); ok {
			t.Errorf("the switch was not lowered:\n%s", buildDump(fn))
		}
	})
}

// TestLowerNestedStatements checks that the pass reaches every statement list.
func TestLowerNestedStatements(t *testing.T) {
	fn := lowerOK(t, `func f(s []int, c bool) int {
	n := 0
	{
		if c {
			n += len(s)
		} else {
			n -= cap(s)
		}
	}
L:
	for i := 0; i < len(s); i++ {
		switch {
		case c:
			break L
		default:
			n += len(s[i:])
		}
	}
	return n
}`)
	if len(fn.Body) == 0 {
		t.Fatal("the body is empty")
	}
}

// TestLowerAddressable is the set of forms ssa.Build can take the address of.
//
// It is written down rather than assumed: a form this reports as addressable
// and that pass does not becomes "an address is not built yet" at construction,
// and one it reports as not addressable costs a spill that nothing needs.
func TestLowerAddressable(t *testing.T) {
	slice := &Type{Kind: Slice, Elem: lowerInt}
	arr := &Type{Kind: Array, Elem: lowerInt, Len: 4}
	st := &Type{Kind: Struct, Fields: []Field{{Name: "a", Type: lowerInt}}}
	for _, tp := range []*Type{slice, arr, st} {
		if err := Layout(tp); err != nil {
			t.Fatal(err)
		}
	}
	l := &lowerer{fn: &Func{Name: "f"}, ptrs: make(map[*Type]*Type), hdrs: make(map[*Type]*Type)}
	ptrSlice := l.ptrTo(slice)
	local := &Node{Op: OLocal, Type: slice, Obj: &Object{Name: "s", Type: slice, Class: ClassLocal}}
	blank := &Node{Op: OLocal, Type: slice, Obj: &Object{Name: "_", Type: slice, Class: ClassLocal}}
	global := &Node{Op: OGlobal, Type: slice, Obj: &Object{Name: "g", Type: slice, Class: ClassGlobal}}
	fnObj := &Node{Op: OGlobal, Type: funcType, Obj: &Object{Name: "h", Type: funcType, Class: ClassFunc}}
	deref := &Node{Op: ODeref, Type: slice, X: &Node{Op: OLocal, Type: ptrSlice}}
	arrLocal := &Node{Op: OLocal, Type: arr, Obj: &Object{Name: "a", Type: arr, Class: ClassLocal}}
	stLocal := &Node{Op: OLocal, Type: st, Obj: &Object{Name: "t", Type: st, Class: ClassLocal}}

	for _, tc := range []struct {
		what string
		n    Expr
		want bool
	}{
		{"a local", local, true},
		{"the blank identifier", blank, false},
		{"a global", global, true},
		{"a function", fnObj, false},
		{"a dereference", deref, true},
		{"a field of a value", &Node{Op: OField, Type: lowerInt, X: stLocal}, true},
		{"a field of a pointer", &Node{Op: OField, Type: lowerInt,
			X: &Node{Op: OLocal, Type: l.ptrTo(st)}}, true},
		{"an element of a slice", &Node{Op: OIndex, Type: lowerInt, X: local}, true},
		{"an element of an array", &Node{Op: OIndex, Type: lowerInt, X: arrLocal}, true},
		{"an element of a pointer to an array", &Node{Op: OIndex, Type: lowerInt,
			X: &Node{Op: OLocal, Type: l.ptrTo(arr)}}, true},
		{"an element of a string", &Node{Op: OIndex, Type: lowerByte,
			X: &Node{Op: OLocal, Type: mustLayoutNamed(String, "string")}}, false},
		{"a constant", &Node{Op: OConst, Type: lowerInt}, false},
		{"nothing", nil, false},
		{"a node with no type", &Node{Op: OLocal}, false},
		{"an index of nothing", &Node{Op: OIndex, Type: lowerInt}, false},
	} {
		if got := addressable(tc.n); got != tc.want {
			t.Errorf("%s is addressable=%v, want %v", tc.what, got, tc.want)
		}
	}
}

// TestLowerSpillsAnOperandThatNamesNoStorage checks the other half of that:
// an operand read more than once is evaluated once.
func TestLowerSpillsAnOperandThatNamesNoStorage(t *testing.T) {
	fn := lowerOK(t, `func slice() []int { return gs }
func f() int { return len(slice()) }`)
	calls := 0
	for _, s := range fn.Body {
		Walk(s, func(n *Node) bool {
			if n.Op == OCall {
				calls++
			}
			return true
		})
	}
	if calls != 1 {
		t.Errorf("the operand is evaluated %d times, want 1:\n%s", calls, buildDump(fn))
	}
}

// TestLowerConstIndex reads the key of an element of an array literal.
func TestLowerConstIndex(t *testing.T) {
	for _, tc := range []struct {
		what string
		n    Expr
		want int64
		ok   bool
	}{
		{"a constant", intConst(0, lowerInt, 7), 7, true},
		{"a converted constant", &Node{Op: OConvert, Type: lowerInt,
			X: intConst(0, lowerInt, 3)}, 3, true},
		{"a local", &Node{Op: OLocal, Type: lowerInt}, 0, false},
		{"nothing", nil, 0, false},
		{"a constant with no value", &Node{Op: OConst, Type: lowerInt}, 0, false},
	} {
		got, ok := constIndex(tc.n)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s reads %d,%v, want %d,%v", tc.what, got, ok, tc.want, tc.ok)
		}
	}
	for _, tc := range []struct {
		what string
		n    Expr
		want bool
	}{
		{"a non-negative constant", intConst(0, lowerInt, 1), true},
		{"zero", intConst(0, lowerInt, 0), true},
		{"a negative constant", intConst(0, lowerInt, -1), false},
		{"a local", &Node{Op: OLocal, Type: lowerInt}, false},
		{"nothing", nil, false},
		{"a constant with no value", &Node{Op: OConst, Type: lowerInt}, false},
	} {
		if got := nonNegative(tc.n); got != tc.want {
			t.Errorf("%s is non-negative=%v, want %v", tc.what, got, tc.want)
		}
	}
}

// TestLowerNeedsAFunction is the one input that is not a program.
func TestLowerNeedsAFunction(t *testing.T) {
	if err := Lower(nil); err == nil {
		t.Error("Lower accepted no function")
	}
}

// TestLowerRuntimeSymbolsAreChecked is specs/031's rule: a symbol the compiler
// generates a call to is one rtsym holds, and rtsym is checked against the
// runtime's own source. A name that is not there is a call to a symbol that
// does not exist, which links against nothing.
func TestLowerRuntimeSymbolsAreChecked(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a name outside rtsym was accepted")
		}
	}()
	runtimeFunc("runtime.thereIsNoSuchSymbol")
}

// TestLowerIsIdempotent checks that lowering a lowered tree changes nothing and
// reports nothing. The pass rewrites in place, so a caller that ran it twice
// would otherwise double every temporary.
func TestLowerIsIdempotent(t *testing.T) {
	fn := lowerOK(t, `func f(s []int) int { n := 0; for i, v := range s { n += i + v }; return n }`)
	before := buildDump(fn)
	locals := len(fn.Locals)
	if err := Lower(fn); err != nil {
		t.Fatalf("the second run refused: %v", err)
	}
	if got := buildDump(fn); got != before {
		t.Errorf("the second run changed the tree:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if len(fn.Locals) != locals {
		t.Errorf("the second run added %d locals", len(fn.Locals)-locals)
	}
}

// TestLowerRefusesAMalformedTree covers the reports a well-typed program
// cannot produce.
//
// Every one of them is a tree the checker would have rejected, so the cases are
// built by hand. They are here because the alternative to reporting them is a
// nil dereference in a compiler, and a compiler that crashes has replaced a
// diagnostic with a stack trace.
func TestLowerRefusesAMalformedTree(t *testing.T) {
	l := &lowerer{fn: &Func{Name: "f"}, ptrs: make(map[*Type]*Type), hdrs: make(map[*Type]*Type)}
	str := mustLayoutNamed(String, "string")
	chn := mustLayoutNamed(Chan, "chan")
	st := &Type{Kind: Struct, Fields: []Field{{Name: "a", Type: lowerInt}, {Name: "b", Type: lowerInt}}}
	arr := &Type{Kind: Array, Elem: lowerInt, Len: 2}
	slice := &Type{Kind: Slice, Elem: lowerInt}
	empty := &Type{Kind: Struct}
	for _, tp := range []*Type{st, arr, slice, empty} {
		if err := Layout(tp); err != nil {
			t.Fatal(err)
		}
	}
	local := func(tp *Type, name string) Expr {
		return &Node{Op: OLocal, Type: tp, Obj: &Object{Name: name, Type: tp, Class: ClassLocal}}
	}

	for _, tc := range []struct {
		what string
		n    Expr
		want string
	}{
		{"len with no operand", &Node{Op: OLen, Type: lowerInt}, "an operand with no type"},
		{"cap of a string", &Node{Op: OCap, Type: lowerInt, X: local(str, "s")},
			"the capacity of a string"},
		{"len of a pointer to something else", &Node{Op: OLen, Type: lowerInt,
			X: local(l.ptrTo(lowerInt), "p")}, "a pointer that is not to an array"},
		{"len of a struct", &Node{Op: OLen, Type: lowerInt, X: local(st, "t")},
			"the length of struct"},
		{"a literal with no type", &Node{Op: OCompositeLit}, "a literal with no type"},
		{"a literal of a channel", &Node{Op: OCompositeLit, Type: chn}, "a literal of chan"},
		{"a struct literal with too few elements", &Node{Op: OCompositeLit, Type: st,
			Args: []Expr{intConst(0, lowerInt, 1)}}, "1 elements for 2 fields"},
		{"an element index that is not a constant", &Node{Op: OCompositeLit, Type: arr,
			Args: []Expr{{Op: OAssign, Type: voidType, X: local(lowerInt, "i"),
				Y: intConst(0, lowerInt, 1)}}}, "not a constant"},
		{"an element index outside the array", &Node{Op: OCompositeLit, Type: arr,
			Args: []Expr{{Op: OAssign, Type: voidType, X: intConst(0, lowerInt, 5),
				Y: intConst(0, lowerInt, 1)}}}, "outside the array"},
		{"a slice expression with no bounds", &Node{Op: OSlice, Type: slice,
			X: local(slice, "s")}, "without its three bounds"},
		{"a three-index slice of a string", &Node{Op: OSlice, Type: str, X: local(str, "s"),
			Args: []Expr{nil, nil, intConst(0, lowerInt, 1)}}, "three-index slice of a string"},
		{"a slice expression over a struct", &Node{Op: OSlice, Type: slice, X: local(st, "t"),
			Args: []Expr{nil, nil, nil}}, "a slice expression over struct"},
		{"a copy with no operand", &Node{Op: OCopy, Type: lowerInt}, "a copy with no operand"},
		{"a copy into a string", &Node{Op: OCopy, Type: lowerInt, X: local(str, "d"),
			Y: local(slice, "s")}, "a copy into string"},
		{"a copy from an integer", &Node{Op: OCopy, Type: lowerInt, X: local(slice, "d"),
			Y: local(lowerInt, "n")}, "a copy from int"},
		{"a clear with no operand", &Node{Op: OClear, Type: voidType}, "a clear with no operand"},
		{"min with no operands", &Node{Op: OMin, Type: lowerInt}, "no operands"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			fn := &Func{Name: "f", Type: funcType}
			fn.Body = []Stmt{{Op: OReturn, Type: voidType, Args: []Expr{tc.n}}}
			err := Lower(fn)
			if err == nil {
				t.Fatalf("the tree was lowered:\n%s", buildDump(fn))
			}
			le, ok := err.(*LowerError)
			if !ok {
				t.Fatalf("the error is a %T: %v", err, err)
			}
			if !strings.Contains(le.What, tc.want) {
				t.Errorf("the reason is %q, want it to name %q", le.What, tc.want)
			}
		})
	}

	// A range whose operand has no type, and one over a kind with no row.
	for _, tc := range []struct {
		what string
		n    Stmt
		want string
	}{
		{"a range over nothing", &Node{Op: ORange, Type: voidType}, "an operand with no type"},
		{"a range over a function", &Node{Op: ORange, Type: voidType, X: local(funcType, "it")},
			"a range over func"},
		{"a range over an integer with two variables", &Node{Op: ORange, Type: voidType,
			X:    local(lowerInt, "n"),
			Args: []Expr{local(lowerInt, "i"), local(lowerInt, "v")}}, "two variables"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			fn := &Func{Name: "f", Type: funcType}
			fn.Body = []Stmt{tc.n}
			err := Lower(fn)
			if err == nil {
				t.Fatalf("the tree was lowered:\n%s", buildDump(fn))
			}
			if !strings.Contains(err.(*LowerError).What, tc.want) {
				t.Errorf("the reason is %q, want it to name %q", err.(*LowerError).What, tc.want)
			}
		})
	}

	// A literal of a type with no storage needs no clear, and a header is only
	// defined for the two types that have one.
	fn := &Func{Name: "f", Type: funcType}
	fn.Body = []Stmt{{Op: OReturn, Type: voidType,
		Args: []Expr{{Op: OCompositeLit, Type: empty}}}}
	if err := Lower(fn); err != nil {
		t.Errorf("a literal of a zero-size struct: %v", err)
	}
	if l.ptrTo(nil).Elem.Kind != UnsafePtr {
		t.Error("a pointer to no type is not a pointer to unsafe.Pointer")
	}
	bad := l.headerField(local(lowerInt, "n"), 0, lowerInt)
	if bad.Op == OField {
		t.Error("a header field of an integer was built")
	}
}

// TestLowerZeroValues covers the constants an array literal fills its gaps
// with, and the clear it falls back to for a type that has no constant zero.
func TestLowerZeroValues(t *testing.T) {
	for _, tc := range []struct {
		what  string
		body  string
		clear bool
	}{
		{"integers", `func f() int { a := [8]int{2: 1}; return a[0] }`, false},
		{"booleans", `func f() bool { a := [8]bool{2: true}; return a[0] }`, false},
		{"strings", `func f() string { a := [8]string{2: "x"}; return a[0] }`, false},
		{"floats", `func f() float64 { a := [8]float64{2: 1}; return a[0] }`, false},
		{"pointers", `func f() *int { a := [8]*int{2: gp}; return a[0] }`, false},
		{"structs", `func f() int { a := [8]T{2: T{1, 2}}; return a[0].A }`, true},
		{"arrays", `func f() int { a := [8][2]int{2: [2]int{1, 2}}; return a[0][0] }`, true},
		// An array of a type with no storage has nothing to clear.
		{"empty structs", `func f() { a := [8]struct{}{2: {}}; _ = a }`, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			fn := lowerOK(t, tc.body)
			cleared := lowerCalled(fn, "runtime.memclrNoHeapPointers") ||
				lowerCalled(fn, "runtime.memclrHasPointers")
			if cleared != tc.clear {
				t.Errorf("cleared=%v, want %v:\n%s", cleared, tc.clear, buildDump(fn))
			}
		})
	}
}

// TestLowerLabelledRange checks that the loop is the statement the label names.
//
// specs/021-ssa-construction.md gives a pending label to the first statement
// after it, so the range setup goes in the loop's init list. A spill emitted
// between "L:" and the loop would take the name, and construction would refuse
// the continue for having no enclosing loop.
func TestLowerLabelledRange(t *testing.T) {
	// The second case ranges over a call, so the builder has already put a
	// temporary in the range statement's Init. That list moves into the loop
	// too, and not in front of it.
	for _, body := range []string{
		`func f(s []int) {
L:
	for i := range s {
		if i == 0 {
			continue L
		}
		use(i)
	}
}`,
		`func f() {
L:
	for i := range mkslice() {
		if i == 0 {
			continue L
		}
		use(i)
	}
}`,
	} {
		fn := lowerOK(t, body)
		if len(fn.Body) != 1 || fn.Body[0].Op != OLabel {
			t.Fatalf("the body is not one labelled statement:\n%s", buildDump(fn))
		}
		list := fn.Body[0].Body
		if len(list) != 1 || list[0].Op != OFor {
			t.Fatalf("the label names %d statements, want the loop alone:\n%s",
				len(list), buildDump(fn))
		}
	}
}
