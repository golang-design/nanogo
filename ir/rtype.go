// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"strconv"
	"strings"
)

// Canonical names for type descriptors, per
// specs/032-type-descriptors-and-itabs.md.
//
// # Why the naming lives here and the bytes do not
//
// specs/032 requires one naming function, in one package, used by everything,
// and it requires the name to be a function of the type alone. The lowering
// pass needs a name for every *_type it passes to the runtime, so the function
// has to be reachable from package ir. The descriptor's *bytes* need the object
// format, and ir may not import obj without inverting the layering that
// specs/002-architecture.md sets, so the encoder is package rtype and it calls
// back into these functions for the name. Naming and encoding are therefore one
// definition each, in the package that can hold it.
//
// # The two spellings
//
// gc writes a type twice, and the two are not the same string.
//
//   - The *link string* qualifies a defined type by its import path. It is the
//     symbol name after the "type:" prefix, and it is what the type hash is
//     computed over. Identity of link strings is identity of types, which is
//     what makes the linker's deduplication by name correct.
//   - The *name string* qualifies by package name instead. It is the content of
//     the descriptor's Str name data, which is what reflect.Type.String
//     returns.
//
// Both are reproduced here rather than approximated, because a link string that
// differs from gc's by one character produces a second symbol for a type that
// already has one, and specs/032 records what a second itab or a second
// descriptor costs.
//
// # What cannot be named, and why it is refused rather than guessed
//
// An ir.Type is a size, an alignment and a pointer map, by the rule at the top
// of type.go. Four kinds of Go type are distinguishable to the language and not
// to that boundary:
//
//   - a channel's direction, so chan T and chan<- T reduce to one ir.Type;
//   - a function's signature, so func(int) and func(string) reduce to one;
//   - an interface's method set, so interface{ M() } and interface{ N() }
//     reduce to one;
//   - a struct field's tag and whether it is embedded, so struct{ A int } and
//     struct{ A int "x" } reduce to one.
//
// For each of those, a name computed from the ir.Type would be the same name
// for two different types. That is the deduplication failure specs/032 names,
// and it is silent: the linker merges the two and the program reads one type's
// descriptor for the other's values. So they are refused, and the refusal names
// the field the IR does not carry. A defined type is exempt from all four,
// because its name is its identity: type S func(int) is named main.S and no
// signature is needed to say so.

// maxNameDepth bounds the recursion of the two spellings.
//
// A type graph is cyclic only through a defined type or a pointer, and both of
// those stop the walk here, so a well-formed graph cannot reach the limit. A
// malformed one is exactly what a diagnostic is describing, and a printer that
// recurses forever turns a reportable bug into a stack overflow.
const maxNameDepth = 64

// TypeSymbolPrefix is the linker prefix of a type descriptor.
//
// The colon is why specs/032 records that the text-assembly seam died: the
// Plan 9 assembler rejects a symbol name that contains one, and the linker
// collects descriptors by exactly this prefix.
const TypeSymbolPrefix = "type:"

// TypeSymbol returns the linker symbol of the type descriptor of t.
//
// It is TypeSymbolPrefix followed by the link string, which is what gc emits
// and what cmd/link collects into runtime.typelinks.
func TypeSymbol(t *Type) (string, error) {
	s, err := TypeLinkString(t)
	if err != nil {
		return "", err
	}
	return TypeSymbolPrefix + s, nil
}

// TypeLinkString returns the spelling of t that identifies it to the linker.
//
// Two types have the same link string exactly when they are the same type, so
// it is also what the type hash is computed over.
func TypeLinkString(t *Type) (string, error) {
	var b strings.Builder
	if err := typeName(&b, t, true, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// TypeNameString returns the spelling of t that reflect.Type.String returns.
//
// It differs from the link string in one way only: a defined type is qualified
// by its package's name rather than by its import path.
func TypeNameString(t *Type) (string, error) {
	var b strings.Builder
	if err := typeName(&b, t, false, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// definedName returns the name of a defined type, and reports whether t is one.
//
// A basic type is a defined type for this purpose: gc writes int as "int", with
// no package qualifier, which is exactly what Converter puts in Name. So the
// two cases share a return and differ only in whether the name holds a path.
func definedName(t *Type) (string, bool) {
	if t == nil || t.Name == "" {
		return "", false
	}
	// An untyped constant's type reaches here when a constant escapes the
	// checker's defaulting. It has no run-time representation and no
	// descriptor, and its name has a space in it, which would produce a symbol
	// no linker collects.
	if strings.HasPrefix(t.Name, "untyped ") {
		return "", false
	}
	// byte and rune are aliases, and the checker's universe declares each as a
	// basic type carrying the alias spelling rather than the spelling of the
	// type it names. gc writes the type it names, so []byte and []uint8 are one
	// symbol rather than two, and a name of "byte" here would be a second
	// descriptor for a type that already has one.
	if k, ok := aliasKinds[t.Name]; ok && k == t.Kind {
		return aliasNames[t.Name], true
	}
	return t.Name, true
}

// The two predeclared aliases, with the kind each one must have for the
// spelling to be the alias rather than a defined type that shadows it.
var (
	aliasKinds = map[string]Kind{"byte": Uint8, "rune": Int32}
	aliasNames = map[string]string{"byte": "uint8", "rune": "int32"}
)

// shortenPath turns an import path into the package name a name string uses.
//
// The last element of the path is the package name for every package whose
// name matches its directory, which is the convention gofmt-era Go follows and
// the whole standard library obeys. A package whose name differs from its
// directory is named wrongly here, and that is a name string only: the link
// string, the symbol and the hash all use the path and are unaffected. The
// import path is the only thing an ir.Type carries, so the alternative is to
// refuse every defined type, which would refuse every descriptor in the deck.
func shortenPath(name string) string {
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return name
	}
	path, base := name[:dot], name[dot+1:]
	if slash := strings.LastIndex(path, "/"); slash >= 0 {
		path = path[slash+1:]
	}
	if path == "" {
		return base
	}
	return path + "." + base
}

// typeName writes one spelling of t into b. link selects the link string.
func typeName(b *strings.Builder, t *Type, link bool, depth int) error {
	if t == nil {
		return fmt.Errorf("ir: the name of a nil type")
	}
	if depth > maxNameDepth {
		return fmt.Errorf("ir: the name of %s nests deeper than %d", t.Kind, maxNameDepth)
	}
	if name, ok := definedName(t); ok {
		if !link {
			name = shortenPath(name)
		}
		b.WriteString(name)
		return nil
	}

	switch t.Kind {
	case Ptr:
		b.WriteString("*")
		return typeName(b, t.Elem, link, depth+1)

	case Slice:
		b.WriteString("[]")
		return typeName(b, t.Elem, link, depth+1)

	case Array:
		b.WriteString("[")
		b.WriteString(strconv.FormatInt(t.Len, 10))
		b.WriteString("]")
		return typeName(b, t.Elem, link, depth+1)

	case Map:
		b.WriteString("map[")
		if err := typeName(b, t.Key, link, depth+1); err != nil {
			return err
		}
		b.WriteString("]")
		return typeName(b, t.Elem, link, depth+1)

	case Interface:
		if !t.EmptyIface {
			// The method set is what would go between the braces, and
			// specs/020-ir.md's boundary drops it. Two interfaces with
			// different methods would share this name.
			return fmt.Errorf("ir: an interface's method set is not in the IR type")
		}
		b.WriteString("interface {}")
		return nil

	case Struct:
		// A defined struct was answered above. What is left is a literal
		// struct type, whose spelling holds each field's tag, and a tag is not
		// in the IR type.
		return fmt.Errorf("ir: a struct's field tags are not in the IR type")

	case Chan:
		return fmt.Errorf("ir: a channel's direction is not in the IR type")

	case FuncKind:
		return fmt.Errorf("ir: a function's signature is not in the IR type")

	case UnsafePtr:
		// Reached only when Converter did not set the name, which happens for
		// a type built below the IR rather than converted from the checker.
		b.WriteString("unsafe.Pointer")
		return nil
	}
	return fmt.Errorf("ir: no canonical name for kind %s", t.Kind)
}
