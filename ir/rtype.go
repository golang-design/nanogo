// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ir

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
// An ir.Type carries what type.go's two rules let it carry, and a spelling
// needs more. A channel's direction and a function's signature were both on
// this list. Both are in type.go now, so chan T and chan<- T are two ir.Types
// and two spellings, and so are func(int) and func(string). What is refused for
// each is the zero value of the field it reads: a channel whose ChanDir is
// InvalidDir, and a function whose Params or Results is nil, are types built
// below the type boundary and never converted.
//
// A literal struct was on this list and is not any more, and the reason it came
// off is the whole of why the list is short. gc spells an embedded field that
// was renamed through a type alias as "struct{ Int = int }", and ir.Converter
// unaliases, so the alias itself is gone before the name is asked for. But gc
// does not read the alias either: types.fldconv writes the field's own name
// unless it is the name of the embedded type, and both of those are in the IR
// type. The alias is what makes the two differ, and the difference is what is
// spelled. type.go carries the field's tag, its package and whether it is
// embedded, so nothing else in a literal struct's spelling is missing.
//
// For each of the two that are left, a name computed from the ir.Type would be
// the same name for two different types. That is the deduplication failure
// specs/032 names, and it is silent: the linker merges the two and the program
// reads one type's descriptor for the other's values. So they are refused, and
// the refusal names the field the IR does not carry. A defined type is exempt
// from both, because its name is its identity: type S func(int) is named main.S
// and no signature is needed to say so.
//
// A generic instantiation is the exception to that exemption, and it is the
// third refusal. Its name is not its identity: Converter's name drops the type
// arguments, so atomic.Pointer[int] and atomic.Pointer[string] both come out as
// sync/atomic.Pointer. specs/032 records it as the case that neither the naming
// function nor the encoder refused, which was true only because every defined
// type was refused for a different reason first.

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
// and what cmd/link collects into runtime.typelinks. A type the compiler
// synthesised and gave no algorithms takes a "noalg." prefix between the two,
// which is gc's types.TypeSymName.
//
// The prefix is here and not in the link string, because the type hash is
// computed over the link string. gc hashes map.group[string]int and names the
// symbol type:noalg.map.group[string]int, so a link string that carried the
// prefix would produce a hash gc never computed.
func TypeSymbol(t *Type) (string, error) {
	s, err := TypeLinkString(t)
	if err != nil {
		return "", err
	}
	if noAlg(t, 0) {
		s = noAlgPrefix + s
	}
	return TypeSymbolPrefix + s, nil
}

// noAlgPrefix separates the descriptor of a synthesised type from the
// descriptor of a declared type of the same shape.
//
// The two would otherwise be one symbol, and the linker would merge them. The
// synthesised one has no hash and no equality function, so the merged symbol
// would leave a declared comparable type with a nil Equal, and the runtime
// panics on a map whose key is that type.
const noAlgPrefix = "noalg."

// NoAlgType reports whether t was synthesised with no algorithms, so that it
// has neither an equality function nor a hash.
//
// It is gc's types.TypeHasNoAlg, and gc gives the mark the highest priority of
// any algorithm: ANOALG implies ANOEQ, so a type the compiler synthesised is
// not comparable at all and its descriptor's Equal is nil. The map slot group
// is the type this exists for. A group holding a string would otherwise be
// given the generated field-wise comparison, and gc emits none, so the two
// compilers would disagree about a symbol the linker merges.
//
// It is exported because the naming and the encoding are in two packages and
// the mark decides both: the symbol takes a "noalg." prefix here and the Equal
// field is nil in rtype. One predicate, because a type that is synthesised to
// one and declared to the other gets a descriptor under the wrong symbol.
func NoAlgType(t *Type) bool { return noAlg(t, 0) }

// noAlg reports whether t or a part of it was synthesised with no algorithms.
//
// It reproduces gc's types.TypeHasNoAlg. The mark is set on the synthesised
// type, and gc's size computation raises a struct and an array to it from
// their contents, so the slot group and the array of slots both carry it. A
// pointer copies the *mark* off its element rather than the element's computed
// answer, which is why the pointer case does not recurse. A slice is on
// neither list, because gc gives a slice no equality algorithm to begin with
// and so never raises it.
func noAlg(t *Type, depth int) bool {
	if t == nil || depth > maxNameDepth {
		return false
	}
	if t.NoAlg {
		return true
	}
	switch t.Kind {
	case Ptr:
		return t.Elem != nil && t.Elem.NoAlg
	case Array:
		return noAlg(t.Elem, depth+1)
	case Struct:
		for _, f := range t.Fields {
			if noAlg(f.Type, depth+1) {
				return true
			}
		}
	}
	return false
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
	path = pathBase(path)
	if path == "" {
		return base
	}
	return path + "." + base
}

// pathBase turns an import path into the package name a name string uses.
//
// It is shortenPath's rule applied to a bare path rather than to a qualified
// name, which is what an interface method carries. The same approximation
// holds and the same thing is unaffected by it: the link string, the symbol
// and the hash all use the path.
func pathBase(path string) string {
	if slash := strings.LastIndex(path, "/"); slash >= 0 {
		return path[slash+1:]
	}
	return path
}

// ExportedName reports whether an unqualified identifier is exported.
//
// The language's rule is that the first character is an upper-case letter, and
// "letter" is Unicode's rather than ASCII's. The ASCII fast path is the shape
// gc's types.IsExported has, and it avoids decoding a rune from a one-byte
// string.
//
// It is here, next to the naming, because the answer is part of a type's
// identity in two places and the two must agree. An interface's spelling
// qualifies an unexported method name with its package and leaves an exported
// one bare, and MethodOrder puts every exported method ahead of every
// unexported one. A rule that answered differently in one of them would give
// one interface two link strings.
func ExportedName(name string) bool {
	if name == "" {
		return false
	}
	if c := name[0]; c < utf8.RuneSelf {
		return c >= 'A' && c <= 'Z'
	}
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

// ItabSymbolPrefix is the linker prefix of an itab.
//
// cmd/link collects a symbol with this prefix into runtime.itablinks, which is
// how the runtime registers a compile-time itab, and the runtime's own itab
// table is what makes a second itab for one pair unreachable rather than
// merely wasteful. The colon is why specs/032 records that the text-assembly
// seam died: the Plan 9 assembler rejects a symbol name that contains one.
const ItabSymbolPrefix = "go:itab."

// ItabSymbol returns the linker symbol of the itab that pairs the concrete
// type t with the interface iface.
//
// It is the prefix, t's link string, a comma and iface's link string, which is
// gc's itabLsym. The link string and not the name string, for the reason
// TypeSymbol gives: identity of link strings is identity of types.
//
// The name is the whole of itab identity. An itab exists once per (interface,
// concrete type) pair, the runtime compares two interface values by comparing
// their first words, and cmd/link merges two duplicate-tolerant symbols of one
// name. So two spellings of one pair are two itabs, and every comparison
// between a value that holds one and a value that holds the other is false.
//
// An empty interface has no itab. Its first word is a *_type, so the pair has
// nothing to name, and a symbol emitted for it would be a table the runtime
// never reads.
func ItabSymbol(t, iface *Type) (string, error) {
	if iface == nil || iface.Kind != Interface {
		return "", fmt.Errorf("ir: an itab needs an interface and %s is not one", iface)
	}
	if iface.EmptyIface {
		return "", fmt.Errorf("ir: %s is the empty interface, whose values lead with a *_type and not with an itab", iface)
	}
	if t == nil {
		return "", fmt.Errorf("ir: an itab needs a concrete type")
	}
	if t.Kind == Interface {
		return "", fmt.Errorf("ir: the itab of %s pairs a concrete type with an interface, and %s is an interface", iface, t)
	}
	ts, err := TypeLinkString(t)
	if err != nil {
		return "", err
	}
	is, err := TypeLinkString(iface)
	if err != nil {
		return "", err
	}
	return ItabSymbolPrefix + ts + "," + is, nil
}

// MethodSymbol returns the linker symbol of a method of t.
//
// ptrRecv chooses between the two spellings: pkg.T.M for a value receiver and
// pkg.(*T).M for a pointer one. The spelling is Build's, because the method
// this names is the one Build compiled, and a wrapper generated under a second
// spelling would be a symbol a descriptor names and nothing defines.
//
// The rule is gc's ir.MethodSym with one clause left out. gc qualifies an
// unexported method by its own package when that package is not the receiver
// type's, which distinguishes two unexported methods of one name declared in
// two packages. nanogo's front end does not spell that clause either (funcSym),
// so adding it here would produce a name for the wrapper that the method it
// calls does not have.
//
// It is here with the descriptor names rather than with the wrapper generator
// because two packages read it. ssagen generates the wrapper and rtype writes
// the descriptor row that names it, and rtype cannot ask ssagen: ssagen already
// imports rtype for the descriptor a stack map and an algorithm need. Two
// spellings of one wrapper would be a descriptor pointing at a function nothing
// defines.
func MethodSymbol(t *Type, m Method, ptrRecv bool) (string, error) {
	name, err := receiverName(t)
	if err != nil {
		return "", err
	}
	if m.Name == "" {
		return "", fmt.Errorf("ir: a method of %s has no name", t)
	}
	if ptrRecv {
		return t.PkgPath + ".(*" + name + ")." + m.Name, nil
	}
	return t.PkgPath + "." + name + "." + m.Name, nil
}

// receiverName returns the identifier a method symbol spells the receiver
// with: the defined type's name with its import path taken off.
//
// Type.Name is qualified by the import path and Type.PkgPath holds that path,
// so the identifier is what is left. The last dot is not the separator (an
// instantiation's name holds the dots of its type arguments), which is why the
// prefix is taken off rather than searched for.
func receiverName(t *Type) (string, error) {
	if t == nil || t.Name == "" {
		return "", fmt.Errorf("ir: a method symbol needs a defined receiver type")
	}
	name := t.Name
	if t.PkgPath != "" {
		if !strings.HasPrefix(name, t.PkgPath+".") {
			return "", fmt.Errorf("ir: the name of %s is not qualified by its package %q", t, t.PkgPath)
		}
		name = name[len(t.PkgPath)+1:]
	}
	if i := strings.IndexByte(name, '['); i >= 0 {
		// A generic instantiation. funcSym spells the method of every
		// instantiation with the origin's name, so the wrapper is spelled the
		// same way and reaches the same symbol.
		name = name[:i]
	}
	return name, nil
}

// MethodOrder returns ms in the order gc writes a method array, which is a
// copy and leaves ms alone.
//
// The order is gc's types.CompareSyms: every exported name ahead of every
// unexported one, then by name, then by the package path that qualifies an
// unexported name. gc sorts both of its method arrays that way, types.CalcMethods
// for a defined type and the interface completer for an interface, so one
// function serves both here.
//
// Byte order by name alone agrees with it for every ASCII identifier, because a
// capital letter sorts below every lower-case one, and agreeing is not being the
// same rule. A name beginning with a capital outside ASCII is exported and sorts
// above no lower-case letter at all, so byte order would put Ärger after the
// unexported names and gc puts it before them.
//
// Three readers depend on this order and they have to agree. The spelling of a
// literal interface and the Imethod array of its descriptor are the same list
// read twice. An UncommonType's Xcount is the length of the exported prefix of
// its Method array, which reflect finds by counting from the front, so an array
// in any other order reports a method set nobody declared.
func MethodOrder(ms []Method) []Method {
	out := make([]Method, len(ms))
	copy(out, ms)
	sortMethods(out)
	return out
}

// sortMethods puts ms in MethodOrder's order, in place.
func sortMethods(ms []Method) {
	sort.SliceStable(ms, func(i, j int) bool {
		ei, ej := ExportedName(ms[i].Name), ExportedName(ms[j].Name)
		if ei != ej {
			return ei
		}
		if ms[i].Name != ms[j].Name {
			return ms[i].Name < ms[j].Name
		}
		return ms[i].Pkg < ms[j].Pkg
	})
}

// fieldName returns the name a struct field's spelling carries, and the
// separator between that name and the field's type.
//
// It is gc's types.fldconv with verb 'L', which is the verb a struct field
// takes, and the two spellings differ here more than anywhere else.
//
//   - A name the language does not export is qualified by the package that
//     declared it in the link string and left bare in the name string. Two
//     packages may declare a field of one name and they are different fields,
//     so the qualifier is part of the type. A field the compiler synthesised
//     carries no package at all, which is what the map slot group's key and
//     elem fields are, and then there is nothing to qualify with.
//   - An embedded field is written as its type alone, and gc writes the field's
//     own name in front of it with " = " when the two differ. That happens when
//     the field was embedded through a type alias: type Int = int embedded is
//     "struct { Int = int }", which is a different type from "struct { int }"
//     and from "struct { Int int }". The name string drops an embedded name
//     whatever it is, which is why one name symbol serves every alias of one
//     type.
//
// An empty name means the field is spelled by its type alone.
func fieldName(f Field, link bool) (name, sep string) {
	if f.Embedded {
		if !link || embeddedNamesItsType(f) {
			return "", ""
		}
		return qualifiedFieldName(f, link), " = "
	}
	return qualifiedFieldName(f, link), " "
}

// qualifiedFieldName returns a field's name with the qualifier the link string
// puts in front of an unexported one.
func qualifiedFieldName(f Field, link bool) string {
	if link && f.Pkg != "" && !ExportedName(f.Name) {
		return f.Pkg + "." + f.Name
	}
	return f.Name
}

// embeddedNamesItsType reports whether an embedded field's name is the name the
// language would have given it, so that gc leaves the name out.
//
// The name the language gives is the embedded type's own, without its package
// and without a pointer: struct{ *T } declares a field named T. So a field
// whose name is anything else was embedded through an alias, and the two are
// different types.
//
// The second half is gc's, and it is not the first half written twice. A field
// declared in one package and a type declared in another have different
// packages and the same name, and embedding an exported type across a package
// boundary is the ordinary case: struct{ io.Reader } declares a field named
// Reader. An unexported name cannot cross that boundary, so for one the
// packages must agree.
func embeddedNamesItsType(f Field) bool {
	t := f.Type
	if t != nil && t.Kind == Ptr {
		// gc asserts that an embedded pointer type has no name of its own, so
		// the name to compare against is the element's.
		t = t.Elem
	}
	if t == nil || t.Name == "" {
		return false
	}
	base := t.Name[strings.LastIndex(t.Name, ".")+1:]
	if f.Name != base {
		return false
	}
	return f.Pkg == t.PkgPath || ExportedName(base)
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
		if t.Instantiated {
			// Two instantiations of one generic type would share this name,
			// and the linker deduplicates by name.
			return fmt.Errorf("ir: %s is a generic instantiation and its type arguments are not in the IR type", name)
		}
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
		if t.Methods == nil {
			if t.EmptyIface {
				// Built below the type boundary with no method list. The
				// layout field says the set is empty, and an empty set is the
				// whole spelling, so nothing is guessed by writing it.
				b.WriteString("interface {}")
				return nil
			}
			// What goes between the braces is each method's name and
			// signature, so an interface with no method list would be spelled
			// as the empty interface and every interface would share one name.
			return fmt.Errorf("ir: a literal interface's spelling holds the name and the type of each method, and its method set is not in the IR type")
		}
		if len(t.Methods) == 0 {
			b.WriteString("interface {}")
			return nil
		}
		b.WriteString("interface {")
		for i, m := range MethodOrder(t.Methods) {
			if i > 0 {
				b.WriteString(";")
			}
			b.WriteString(" ")
			if !ExportedName(m.Name) {
				// Two packages may declare an unexported method of one name
				// and they are different methods, so the qualifier is part of
				// the type. It is qualified the way a defined type's name is:
				// by import path in the link string, by package name in the
				// name string.
				if m.Pkg == "" {
					return fmt.Errorf("ir: the interface method %s is unexported and the package that declares it is not in the IR type", m.Name)
				}
				q := m.Pkg
				if !link {
					q = pathBase(q)
				}
				b.WriteString(q)
				b.WriteString(".")
			}
			b.WriteString(m.Name)
			if m.Sig == nil {
				return fmt.Errorf("ir: the interface method %s has no signature, which its spelling holds", m.Name)
			}
			if err := signatureName(b, m.Sig, link, depth+1); err != nil {
				return err
			}
		}
		b.WriteString(" }")
		return nil

	case Struct:
		if t.MapGroup != nil {
			// The slot group of a map, which gc spells from the map's key and
			// element rather than from the slot's. The slot substitutes a
			// pointer for a key or an element past the size threshold and the
			// name does not, so map[[200]byte]int is map.group[[200]uint8]int.
			m := t.MapGroup
			if m.Kind != Map {
				return fmt.Errorf("ir: a slot group names the map it belongs to and carries a %s", m.Kind)
			}
			b.WriteString("map.group[")
			if err := typeName(b, m.Key, link, depth+1); err != nil {
				return err
			}
			b.WriteString("]")
			return typeName(b, m.Elem, link, depth+1)
		}
		// A defined struct was answered above. What is left is a literal
		// struct type, whose spelling holds the name, the type and the tag of
		// every field. The separators are gc's types.tconv2: a semicolon
		// between two fields, a space in front of each, and a space before the
		// closing brace when there is at least one field, so an empty struct
		// is "struct {}" and a one-field struct is "struct { A int }".
		if len(t.Fields) == 0 {
			b.WriteString("struct {}")
			return nil
		}
		b.WriteString("struct {")
		for i, f := range t.Fields {
			if i > 0 {
				b.WriteString(";")
			}
			b.WriteString(" ")
			name, sep := fieldName(f, link)
			if name != "" {
				b.WriteString(name)
				b.WriteString(sep)
			}
			if err := typeName(b, f.Type, link, depth+1); err != nil {
				return err
			}
			if f.Tag != "" {
				// Two struct types that differ only in a tag are different
				// types, and gc quotes the tag the way Go source does.
				b.WriteString(" ")
				b.WriteString(strconv.Quote(f.Tag))
			}
		}
		b.WriteString(" }")
		return nil

	case Chan:
		switch t.ChanDir {
		case RecvOnly, SendOnly, SendRecv:
		default:
			return fmt.Errorf("ir: a channel's direction is not in the IR type")
		}
		// ChanDir.String is the whole prefix, so the three directions differ
		// here by the field and by nothing this function decides.
		b.WriteString(t.ChanDir.String())
		b.WriteString(" ")
		if t.ChanDir == SendRecv && t.Elem != nil && t.Elem.Name == "" &&
			t.Elem.Kind == Chan && t.Elem.ChanDir == RecvOnly {
			// gc parenthesises the element of chan (<-chan T), and so does the
			// language. Without the parentheses the spelling reads back as
			// chan<- chan T, which is a different type, so one symbol would
			// stand for two.
			b.WriteString("(")
			if err := typeName(b, t.Elem, link, depth+1); err != nil {
				return err
			}
			b.WriteString(")")
			return nil
		}
		return typeName(b, t.Elem, link, depth+1)

	case FuncKind:
		b.WriteString("func")
		return signatureName(b, t, link, depth)

	case UnsafePtr:
		// Reached only when Converter did not set the name, which happens for
		// a type built below the IR rather than converted from the checker.
		b.WriteString("unsafe.Pointer")
		return nil
	}
	return fmt.Errorf("ir: no canonical name for kind %s", t.Kind)
}

// signatureName writes a function type's parameter and result lists into b,
// without the leading "func".
//
// The word is left to the caller because gc writes it for a function type and
// leaves it out for an interface method: func(int) string is the type and
// F(int) string is the method, and the part after the name is this.
//
// Parameter names are not in it. Two functions that differ only in the name of
// a parameter are one type, so a name in the spelling would be two symbols for
// one type, which is the mirror of the failure this file exists to prevent.
func signatureName(b *strings.Builder, t *Type, link bool, depth int) error {
	if t == nil {
		return fmt.Errorf("ir: the name of a nil signature")
	}
	if t.Kind != FuncKind {
		return fmt.Errorf("ir: %s is not a function type and has no signature to spell", t.Kind)
	}
	if depth > maxNameDepth {
		return fmt.Errorf("ir: the name of %s nests deeper than %d", t.Kind, maxNameDepth)
	}
	if t.Params == nil || t.Results == nil {
		// Converter sets both on every function type, the empty list included,
		// so a nil one means the type was built below the type boundary. Read
		// as the empty list it would spell func(int) as func(), and the linker
		// deduplicates by name.
		return fmt.Errorf("ir: a function's parameter and result lists are not in the IR type, so its signature has no spelling")
	}
	if t.Variadic && len(t.Params) == 0 {
		// The bit says the last parameter is a ... parameter and there is no
		// last parameter, so the spelling would drop the bit and name a
		// variadic function as a non-variadic one.
		return fmt.Errorf("ir: a function's signature is variadic and its parameter list is empty")
	}
	b.WriteString("(")
	for i, p := range t.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		if t.Variadic && i == len(t.Params)-1 {
			// The parameter's own type is the slice, and gc spells the
			// element: func(...int) and func([]int) have the same Params and
			// are different types.
			if p == nil || p.Kind != Slice {
				return fmt.Errorf("ir: the last parameter of a variadic function is %s rather than a slice, so ...T has no element to name", p)
			}
			b.WriteString("...")
			if err := typeName(b, p.Elem, link, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := typeName(b, p, link, depth+1); err != nil {
			return err
		}
	}
	b.WriteString(")")

	// One result is written bare and several are parenthesised, which is the
	// language's own spelling and gc's.
	switch len(t.Results) {
	case 0:
		return nil
	case 1:
		b.WriteString(" ")
		return typeName(b, t.Results[0], link, depth+1)
	}
	b.WriteString(" (")
	for i, r := range t.Results {
		if i > 0 {
			b.WriteString(", ")
		}
		if err := typeName(b, r, link, depth+1); err != nil {
			return err
		}
	}
	b.WriteString(")")
	return nil
}
