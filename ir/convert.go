// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"sort"

	"golang.design/x/nanogo/types2"
)

// Converter turns a type checker type into the IR's type.
//
// This is the type boundary of specs/002-architecture.md in one direction.
// Everything the compiler needs below the IR is a kind, a size, an alignment
// and a pointer map, and this is where the type checker's answer is reduced to
// those four things. Nothing below the IR sees a types2.Type.
//
// A Converter memoises. The type checker's graph is shared and it is cyclic
// through pointers, so a converter that recursed without a cache would not
// terminate on
//
//	type T struct{ next *T }
//
// The cache entry is installed before the recursion into the type's parts, not
// after, which is what breaks the cycle: the second visit to T finds the entry
// that the first visit installed and stops.
type Converter struct {
	// cache maps one type checker type to one IR type. It is never ranged
	// over. specs/053-determinism.md forbids a map on a path that produces
	// output, and the order of this one is the order of a type graph walk.
	cache map[types2.Type]*Type

	// tuples is the cache for result tuples, which are not types of values and
	// so are kept apart from cache.
	tuples map[*types2.Tuple]*Type
}

// NewConverter returns a Converter with an empty cache.
//
// One converter per package build. Sharing one across packages is safe and
// wastes nothing, because a type's layout does not depend on who asks.
func NewConverter() *Converter {
	return &Converter{
		cache:  make(map[types2.Type]*Type),
		tuples: make(map[*types2.Tuple]*Type),
	}
}

// Convert returns the IR type of t, with Size, Align, PtrBits and every field
// offset already computed.
//
// It reports an error for a type that has no run-time representation: a type
// parameter, a constraint union, and the tuple of a multi-value expression.
// A tuple has Tuple for that reason.
func (c *Converter) Convert(t types2.Type) (*Type, error) {
	out, err := c.convert(t)
	if err != nil {
		return nil, err
	}
	if err := Layout(out); err != nil {
		return nil, err
	}
	return out, nil
}

// Tuple returns the type of a multi-value expression.
//
// The result has kind Tuple, with one field per component, named r0, r1 and so
// on. Read that kind's documentation before using the offsets: its layout is a
// struct's and is deliberately not the result layout of specs/030-abi.md,
// because results are assigned to registers and the stack by the calling
// convention rather than by struct packing.
//
// It is a method of its own rather than a case of Convert so that a tuple
// cannot be reached by accident: nothing that asks for the type of a value
// should get one.
func (c *Converter) Tuple(t *types2.Tuple) (*Type, error) {
	// A nil tuple is the empty result list, which types2 spells as a nil
	// *Tuple, and it converts to a zero-size struct.
	if have, ok := c.tuples[t]; ok {
		return have, nil
	}
	out := &Type{Kind: Tuple}
	c.tuples[t] = out
	for i := 0; i < t.Len(); i++ {
		ft, err := c.convert(t.At(i).Type())
		if err != nil {
			delete(c.tuples, t)
			return nil, err
		}
		out.Fields = append(out.Fields, Field{Name: fmt.Sprintf("r%d", i), Type: ft})
	}
	if err := Layout(out); err != nil {
		delete(c.tuples, t)
		return nil, err
	}
	return out, nil
}

// basicKinds maps the type checker's basic kinds to the IR's.
//
// int, uint and uintptr are 64 bit because both targets of
// specs/000-decisions.md decision 9 are, which is the same assumption PtrSize
// records. A 32-bit target makes this table a property of the target.
var basicKinds = map[types2.BasicKind]Kind{
	types2.Bool:           Bool,
	types2.Int:            Int64,
	types2.Int8:           Int8,
	types2.Int16:          Int16,
	types2.Int32:          Int32,
	types2.Int64:          Int64,
	types2.Uint:           Uint64,
	types2.Uint8:          Uint8,
	types2.Uint16:         Uint16,
	types2.Uint32:         Uint32,
	types2.Uint64:         Uint64,
	types2.Uintptr:        Uintptr,
	types2.Float32:        Float32,
	types2.Float64:        Float64,
	types2.Complex64:      Complex64,
	types2.Complex128:     Complex128,
	types2.String:         String,
	types2.UnsafePointer:  UnsafePtr,
	types2.UntypedBool:    Bool,
	types2.UntypedInt:     Int64,
	types2.UntypedRune:    Int32,
	types2.UntypedFloat:   Float64,
	types2.UntypedComplex: Complex128,
	types2.UntypedString:  String,
}

func (c *Converter) convert(t types2.Type) (*Type, error) {
	if t == nil {
		return nil, fmt.Errorf("ir: convert of a nil type")
	}
	// An alias is a second name for one type, so it converts to the type it
	// names. Unalias first, and cache on the result, so that T and its alias
	// share one IR type rather than two equal ones.
	t = types2.Unalias(t)
	if have, ok := c.cache[t]; ok {
		return have, nil
	}

	out := new(Type)
	// Installed before the recursion below. A type graph is cyclic through
	// pointers and this is what stops the walk from following the cycle.
	c.cache[t] = out
	if err := c.fill(out, t); err != nil {
		delete(c.cache, t)
		return nil, err
	}
	return out, nil
}

// fill sets out from t. out is already in the cache, so a recursive reference
// back to t finds it.
func (c *Converter) fill(out *Type, t types2.Type) error {
	switch t := t.(type) {
	case *types2.Basic:
		k, ok := basicKinds[t.Kind()]
		if !ok {
			return fmt.Errorf("ir: no IR kind for basic type %s", t)
		}
		out.Kind = k
		// unsafe.Pointer is UnsafePtr and not Ptr. The collector treats the two
		// differently: an unsafe.Pointer may point into the middle of an
		// object, so it is not a valid base for an interior pointer check.
		spelling := t.Name()
		if t.Kind() == types2.UnsafePointer {
			// The checker calls it Pointer, because that is its name in its
			// package. A diagnostic wants the name a reader wrote.
			spelling = "unsafe.Pointer"
		}
		// Basic is set on the way through, so that a defined type whose
		// underlying type is basic carries the predeclared spelling as well as
		// its own name. type.go says why: int and int64 are one IR kind and two
		// internal/abi kinds.
		out.Basic = spelling
		if out.Name == "" {
			// A defined type keeps the name it was given. This case is also
			// reached through Named, whose name is the one worth printing.
			out.Name = spelling
		}
		return nil

	case *types2.Named:
		// The name, the package and the method set are the descriptor fields
		// of type.go's second rule: they cross the boundary because nothing
		// below it can write a type descriptor without them, and nothing that
		// generates code may branch on them. The shape still comes from the
		// underlying type.
		out.Name = namedString(t)
		if obj := t.Obj(); obj != nil && obj.Pkg() != nil {
			out.PkgPath = obj.Pkg().Path()
		}
		// The name above is the generic type's, without the arguments, so an
		// instantiation has to be marked as one. See the field's comment.
		out.Instantiated = t.TypeArgs().Len() > 0
		ms, err := c.methodSet(t)
		if err != nil {
			return err
		}
		out.Methods = ms
		return c.fill(out, t.Underlying())

	case *types2.Pointer:
		out.Kind = Ptr
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Elem = elem
		return nil

	case *types2.Slice:
		out.Kind = Slice
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Elem = elem
		return nil

	case *types2.Array:
		out.Kind = Array
		out.Len = t.Len()
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Elem = elem
		return nil

	case *types2.Map:
		out.Kind = Map
		key, err := c.convert(t.Key())
		if err != nil {
			return err
		}
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Key, out.Elem = key, elem
		return nil

	case *types2.Chan:
		out.Kind = Chan
		elem, err := c.convert(t.Elem())
		if err != nil {
			return err
		}
		out.Elem = elem
		return nil

	case *types2.Struct:
		out.Kind = Struct
		// In declaration order, which specs/030-abi.md requires: no field
		// reordering, because every use of unsafe.Offsetof and every assembly
		// file assumes the written order.
		for i := 0; i < t.NumFields(); i++ {
			f := t.Field(i)
			ft, err := c.convert(f.Type())
			if err != nil {
				return err
			}
			fl := Field{Name: f.Name(), Type: ft, Tag: t.Tag(i), Embedded: f.Embedded()}
			// The path qualifies an unexported name only. An exported field is
			// reachable from anywhere and its descriptor carries no path,
			// which is also what makes two structurally equal struct types
			// with exported fields one symbol rather than two.
			if !isExportedName(f.Name()) && f.Pkg() != nil {
				fl.Pkg = f.Pkg().Path()
			}
			out.Fields = append(out.Fields, fl)
		}
		return nil

	case *types2.Signature:
		// FuncKind, not Func. A function value is one word, a pointer to a
		// closure object; the signature is not part of the machine type.
		//
		// The signature crosses the boundary anyway, as a descriptor field by
		// type.go's second rule. A FuncType descriptor's tail is the type of
		// every parameter and every result, and a method descriptor's Mtyp is
		// a TypeOff to a type of exactly this kind, so nothing below the
		// boundary can write either without them.
		out.Kind = FuncKind
		return c.signature(out, t)

	case *types2.Interface:
		out.Kind = Interface
		// Which of the two interface layouts this is. An empty interface holds
		// a *_type in its first word and a non-empty one holds an *itab, so
		// equality calls a different runtime function for each and calling the
		// wrong one reads a function pointer at the wrong offset. See the field
		// comment in type.go.
		out.EmptyIface = t.NumMethods() == 0
		return nil

	case *types2.TypeParam:
		// specs/013-generics.md instantiates before the IR is built, so a type
		// parameter here means an uninstantiated body reached the builder.
		return fmt.Errorf("ir: type parameter %s has no run-time representation", t)

	case *types2.Tuple:
		return fmt.Errorf("ir: a tuple is not the type of a value; use Converter.Tuple")

	case *types2.Union:
		return fmt.Errorf("ir: a constraint union has no run-time representation")
	}
	return fmt.Errorf("ir: no IR type for %T", t)
}

// namedString names a defined type for a diagnostic.
//
// The import path is included because two packages may define one name, and a
// message that says "T" when there are two Ts costs the reader the time this
// name is here to save.
func namedString(t *types2.Named) string {
	obj := t.Obj()
	if obj == nil {
		return ""
	}
	if pkg := obj.Pkg(); pkg != nil {
		return pkg.Path() + "." + obj.Name()
	}
	return obj.Name()
}

// isExportedName reports whether an identifier is exported.
//
// The unqualified name, not the qualified one: a method's and a field's name
// hold no package.
func isExportedName(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}

// signature fills out's parameter and result types from t.
//
// The receiver is not among them. types2 keeps it in Recv() and out of
// Params(), which is exactly the shape a method descriptor's Mtyp needs: the
// method's type with the receiver removed.
//
// Both slices are empty rather than nil when the list is empty, for the reason
// Methods is: a nil slice on a converted type would be indistinguishable from
// one nobody filled in, and the descriptor writer refuses the second.
func (c *Converter) signature(out *Type, t *types2.Signature) error {
	params := t.Params()
	out.Params = make([]*Type, 0, params.Len())
	for i := 0; i < params.Len(); i++ {
		pt, err := c.convert(params.At(i).Type())
		if err != nil {
			return err
		}
		out.Params = append(out.Params, pt)
	}
	results := t.Results()
	out.Results = make([]*Type, 0, results.Len())
	for i := 0; i < results.Len(); i++ {
		rt, err := c.convert(results.At(i).Type())
		if err != nil {
			return err
		}
		out.Results = append(out.Results, rt)
	}
	// The last parameter's own type is already the slice, so this bit is not
	// recoverable from the list: func(...int) and func([]int) have the same
	// parameters and are different types.
	out.Variadic = t.Variadic()
	return nil
}

// methodSig converts a method's type.
//
// The object's type is a *types2.Signature whose Recv is the receiver and
// whose Params are not, so converting it gives the type with the receiver
// removed and nothing is stripped here. A method object with any other type is
// a malformed checker result rather than a program this compiler can refuse
// usefully, so it is reported by name.
func (c *Converter) methodSig(fn *types2.Func) (*Type, error) {
	sig, ok := fn.Type().(*types2.Signature)
	if !ok {
		return nil, fmt.Errorf("ir: the method %s has type %T and not a signature", fn.Name(), fn.Type())
	}
	return c.convert(sig)
}

// methodSet returns the method set of a pointer to t, sorted by name.
//
// The pointer's set is the larger of the two and Method.PtrOnly separates them,
// which is what type.go's Methods field documents.
//
// # Why the checker is asked one name at a time
//
// A method set is not the list of methods declared on the type. Embedding
// promotes a method, a field of the same name shadows one, two embedded types
// at the same depth cancel each other out, and embedding a pointer changes
// which receiver forms are promoted. Reimplementing those rules here would be a
// second answer to a question the checker already answers, and the two would
// differ on exactly the programs that are hard to write. So this walks the type
// to collect candidate *names*, which needs no rules at all, and then asks
// types2.LookupFieldOrMethod what each name resolves to. A name that resolves
// to a field, or to nothing because it is ambiguous, is not in the set.
//
// The addressable argument is what selects the two sets: with it the checker
// answers for a variable whose address can be taken, which is the method set of
// the pointer.
func (c *Converter) methodSet(t *types2.Named) ([]Method, error) {
	var cands []*types2.Func
	seen := make(map[types2.Type]bool)
	collectMethodNames(t, seen, &cands, 0)

	// Empty and not nil. type.go's Methods field says why: a nil set on a
	// defined type would be indistinguishable from a set nobody computed, and
	// the whole point of carrying it is that an empty method set is knowable.
	out := []Method{}
	// added is a lookup table and is never ranged over
	// (specs/053-determinism.md). The output order is the sort below.
	added := make(map[[2]string]bool)
	for _, f := range cands {
		name := f.Name()
		pkg := (*types2.Package)(nil)
		path := ""
		if !isExportedName(name) {
			pkg = f.Pkg()
			if pkg != nil {
				path = pkg.Path()
			}
		}
		key := [2]string{name, path}
		if added[key] {
			continue
		}
		added[key] = true
		ptrObj, _, _ := types2.LookupFieldOrMethod(t, true, pkg, name)
		if _, ok := ptrObj.(*types2.Func); !ok {
			// The name is a field, or it is ambiguous. Either way it is not a
			// method of this type.
			continue
		}
		valObj, _, _ := types2.LookupFieldOrMethod(t, false, pkg, name)
		_, inValue := valObj.(*types2.Func)
		// The signature comes from the object the lookup resolved to and not
		// from the candidate, because a promoted method's declaration is on
		// the embedded type and the lookup is what says which declaration this
		// name reaches.
		sig, err := c.methodSig(ptrObj.(*types2.Func))
		if err != nil {
			return nil, err
		}
		out = append(out, Method{Name: name, Pkg: path, Sig: sig, PtrOnly: !inValue})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Pkg < out[j].Pkg
	})
	return out, nil
}

// maxEmbedDepth bounds the walk that collects candidate method names.
//
// Embedding cannot nest for ever in a well-formed program, because a type
// cannot embed itself by value, but the walk follows pointers as well and a
// malformed graph is what a diagnostic is describing. The seen set already
// stops a cycle; this stops a graph that is merely enormous.
const maxEmbedDepth = 64

// collectMethodNames appends every method a name could be promoted from.
//
// It over-collects on purpose. A name it appends that is shadowed, ambiguous or
// not promoted is dropped by the lookup in methodSet, and a name it misses is a
// method missing from a descriptor.
func collectMethodNames(t types2.Type, seen map[types2.Type]bool, out *[]*types2.Func, depth int) {
	if t == nil || depth > maxEmbedDepth {
		return
	}
	t = types2.Unalias(t)
	if seen[t] {
		return
	}
	seen[t] = true
	switch t := t.(type) {
	case *types2.Named:
		for i := 0; i < t.NumMethods(); i++ {
			*out = append(*out, t.Method(i))
		}
		collectMethodNames(t.Underlying(), seen, out, depth+1)
	case *types2.Pointer:
		// An embedded pointer promotes the pointee's methods. A pointer to a
		// pointer embeds nothing, and the checker rejects it, so following one
		// level costs nothing and stopping here would miss the common case.
		collectMethodNames(t.Elem(), seen, out, depth+1)
	case *types2.Interface:
		for i := 0; i < t.NumMethods(); i++ {
			*out = append(*out, t.Method(i))
		}
	case *types2.Struct:
		for i := 0; i < t.NumFields(); i++ {
			if f := t.Field(i); f.Embedded() {
				collectMethodNames(f.Type(), seen, out, depth+1)
			}
		}
	}
}
