// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Package ssa holds the control-flow graph in static single assignment form.
//
// It is the second of the two representations of specs/002-architecture.md.
// Above it, a program is a tree with Go semantics still visible. Here a program
// is a set of basic blocks holding values, and no Go construct survives:
// specs/020-ir.md's lowering table is complete by the time construction ends.
//
// Three properties of the form decide everything a pass can do.
//
// Each value is assigned once, so a value is its definition and use-def chains
// cost nothing.
//
// Memory is a value. Every operation that reads memory takes a memory value as
// an argument and every operation that writes memory produces a new one, so the
// ordering of side effects is data dependence and a pass that respects data
// dependence respects memory order without knowing that memory exists. See
// specs/021-ssa-construction.md.
//
// Values and blocks carry dense identifiers allocated from the Func. A pass
// indexes a slice by identifier instead of keying a map by pointer, which is
// what keeps specs/053-determinism.md's rule cheap to obey: no map is ranged
// over on a path that produces output, and no order derives from an address.
package ssa

import (
	"fmt"
	"strings"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
)

// ID is a dense identifier of a value or a block within one Func.
//
// Dense and stable: an identifier is allocated once and is never reused, so a
// pass can size a lookup table with NumValues or NumBlocks and index it
// directly. Deleting a block leaves a hole in the table rather than renumbering
// what is left, because renumbering would make an identifier mean two things at
// two times.
type ID int32

// MemType is the type of a memory value.
//
// Memory is not a Go type, so it has no ir.Kind of its own. It is identified by
// pointer, never by the name, which exists only to make a dump readable. The
// alignment is one rather than zero so that a type laid out by ir.Layout and
// this one cannot be told apart by the marker ir.Layout uses.
var MemType = &ir.Type{Kind: ir.Void, Size: 0, Align: 1, Name: "mem"}

// IsMemory reports whether v is a memory value.
func IsMemory(v *Value) bool { return v != nil && v.Type == MemType }

// Value is one operation and its result.
//
// A value belongs to exactly one block. Its arguments are the values it reads,
// and when the operation touches memory the memory argument is the last one.
// That position is an invariant of the whole package, checked by Verify, and it
// is what lets a pass find the memory argument without a table lookup.
type Value struct {
	ID   ID
	Op   Op
	Type *ir.Type
	Args []*Value

	// AuxInt and Aux carry what is not a value: an integer constant, a byte
	// offset, an element size, a symbol, a string.
	AuxInt int64
	Aux    any

	Block *Block
	Pos   syntax.Pos

	// uses lists the values that hold this one as an argument. It is
	// construction bookkeeping, needed to replace a phi that turned out to be
	// redundant, and it is dropped when construction ends. Entries may be
	// stale, so a reader re-checks the argument before acting on it.
	uses []*Value

	// forward is set when this value was replaced, by the value that replaced
	// it. Braun et al. (2013) removes a redundant phi while other phis still
	// refer to it, and following this chain is how a later read of the variable
	// map reaches the survivor.
	forward *Value

	// dead marks a value removed from its block.
	dead bool
}

func (v *Value) String() string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("v%d", v.ID)
}

// LongString returns the value in the form a dump uses.
func (v *Value) LongString() string {
	var b strings.Builder
	fmt.Fprintf(&b, "v%d = %v", v.ID, v.Op)
	if v.Type != nil {
		fmt.Fprintf(&b, " <%v>", v.Type)
	}
	if infoOf(v.Op).hasAuxInt {
		fmt.Fprintf(&b, " [%d]", v.AuxInt)
	}
	if v.Aux != nil {
		fmt.Fprintf(&b, " {%v}", auxString(v.Aux))
	}
	for _, a := range v.Args {
		fmt.Fprintf(&b, " %v", a)
	}
	return b.String()
}

func auxString(aux any) string {
	switch a := aux.(type) {
	case *ir.Object:
		return a.Name
	case ir.Value:
		return a.String()
	default:
		return fmt.Sprint(aux)
	}
}

// MemArg returns the memory argument of v, or nil if v does not take memory.
func (v *Value) MemArg() *Value {
	if !infoOf(v.Op).takesMem || len(v.Args) == 0 {
		return nil
	}
	return v.Args[len(v.Args)-1]
}

// AddArg appends an argument.
func (v *Value) AddArg(a *Value) {
	v.Args = append(v.Args, a)
	if a != nil {
		a.uses = append(a.uses, v)
	}
}

// SetArg replaces argument i.
func (v *Value) SetArg(i int, a *Value) {
	v.Args[i] = a
	if a != nil {
		a.uses = append(a.uses, v)
	}
}

// BlockKind is what a block does when it ends.
//
// The kind fixes the number of successors and whether a control value is
// required, which is the first invariant of specs/021-ssa-construction.md:
// exactly one control value, or none if the block has one successor.
type BlockKind uint8

const (
	BlockInvalid BlockKind = iota

	// BlockPlain has one successor and no control value.
	BlockPlain
	// BlockIf has two successors and a boolean control value. Successor 0 is
	// taken when the control is true.
	BlockIf
	// BlockRet has no successor and a memory control value, produced by
	// OpMakeResult.
	BlockRet
	// BlockExit has no successor and a memory control value. It ends a path
	// that does not return, such as one that panics.
	BlockExit
)

var blockKindNames = [...]string{
	BlockInvalid: "invalid",
	BlockPlain:   "Plain",
	BlockIf:      "If",
	BlockRet:     "Ret",
	BlockExit:    "Exit",
}

func (k BlockKind) String() string {
	if int(k) < len(blockKindNames) && blockKindNames[k] != "" {
		return blockKindNames[k]
	}
	return "blockkind(?)"
}

// NumSuccs returns how many successors a block of this kind has, and whether
// the number is fixed.
func (k BlockKind) NumSuccs() (int, bool) {
	switch k {
	case BlockPlain:
		return 1, true
	case BlockIf:
		return 2, true
	case BlockRet, BlockExit:
		return 0, true
	}
	return 0, false
}

// Block is one basic block.
type Block struct {
	ID     ID
	Kind   BlockKind
	Values []*Value

	// Control is the value the block branches on, or the memory a return
	// leaves with. A block with one successor has none.
	Control *Value

	// Preds and Succs may contain the same block twice. An if statement with
	// two empty arms produces exactly that, and a phi in the successor then
	// needs one argument per predecessor slot rather than one per distinct
	// predecessor. Slot i of Preds and argument i of every phi are the same
	// edge.
	Preds []*Block
	Succs []*Block

	Func *Func
	Pos  syntax.Pos

	// sealed reports that every predecessor of this block is known. Braun et
	// al. (2013) needs it: a read of a variable in an unsealed block cannot
	// look at the predecessors yet, so it leaves an incomplete phi behind.
	sealed bool

	// incomplete lists the phis waiting for this block to be sealed, in
	// creation order. A slice rather than a map, because the order in which
	// they are filled decides the order in which values are created, and
	// specs/053-determinism.md does not allow that to come from a map.
	incomplete []incompletePhi

	// defs is the current definition of each variable in this block. It is
	// looked up by key and never ranged over.
	defs map[*ir.Object]*Value
}

type incompletePhi struct {
	variable *ir.Object
	phi      *Value
}

func (b *Block) String() string {
	if b == nil {
		return "<nil>"
	}
	return fmt.Sprintf("b%d", b.ID)
}

// NewValue adds a value to the end of the block.
func (b *Block) NewValue(pos syntax.Pos, op Op, t *ir.Type, args ...*Value) *Value {
	v := b.Func.newValue(op, t, pos)
	v.Block = b
	for _, a := range args {
		v.AddArg(a)
	}
	b.Values = append(b.Values, v)
	return v
}

// AddEdgeTo adds an edge from b to c.
//
// The two lists are appended together, so the n-th occurrence of c in b.Succs
// and the n-th occurrence of b in c.Preds are the same edge. predIndex depends
// on that and so does critical edge splitting.
func (b *Block) AddEdgeTo(c *Block) {
	b.Succs = append(b.Succs, c)
	c.Preds = append(c.Preds, b)
}

// removeValue deletes v from the block.
func (b *Block) removeValue(v *Value) {
	for i, w := range b.Values {
		if w == v {
			b.Values = append(b.Values[:i], b.Values[i+1:]...)
			v.dead = true
			return
		}
	}
}

// Func is one function in SSA form.
type Func struct {
	Name string
	Sym  string
	Type *ir.Type
	Pos  syntax.Pos

	// Entry is the block execution starts in. It has no predecessor.
	Entry *Block

	// Blocks holds every live block in layout order. A deleted block is
	// removed from here but its identifier is not reused, so len(Blocks) is
	// not NumBlocks.
	Blocks []*Block

	// Frame lists the objects that do not live in a value, in declaration
	// order: the address-taken ones, and the ones whose type is not held in a
	// single value. specs/027-liveness-and-stackmaps.md describes them to the
	// collector and specs/030-abi.md gives them offsets. Neither is done here.
	Frame []*ir.Object

	// NeedCtxt records that the caller left a closure object in the context
	// register for this function, which is true of a function literal that
	// captures and of nothing else.
	//
	// Two consumers read it and neither can derive it. The code generator
	// picks the stack-growth symbol from it, because
	// runtime.morestack_noctxt clears the register and runtime.morestack
	// saves it across the growth, and reading the graph instead would answer
	// wrongly for a function whose context value became dead.
	NeedCtxt bool

	// Wrapper records that the function is compiler-generated code the
	// runtime must not count as a frame of the program, which the object's
	// FuncInfo carries as a funcID (ir.Func.Wrapper).
	Wrapper bool

	// Descriptors lists the types whose type descriptors this function's code
	// names, in first-use order.
	//
	// A reference is not a definition. The linker resolves type: symbols by
	// name and defines none of them, so the package that names one owes the
	// bytes unless the runtime already carries them. ir.LowerAndCollect
	// reports the set the lowering table names and this is the set
	// construction names, which is a different set: a conversion to an
	// interface reaches specs/032's descriptor and no row of that table.
	//
	// First-use order and not a map, because the object's symbol table is
	// written in the order symbols were added (specs/053-determinism.md).
	Descriptors []*ir.Type

	// Itabs lists the (concrete type, interface) pairs whose itabs this
	// function's code names, in first-use order.
	//
	// It is a second list and not part of Descriptors because an itab is not a
	// descriptor. It is named per pair, it is collected into
	// runtime.itablinks, and rtype writes it from a different encoder. The
	// obligation is the same one: a reference is not a definition, and
	// cmd/link resolves go:itab. symbols by name and defines none of them.
	//
	// First-use order and not a map, for the reason Descriptors gives
	// (specs/053-determinism.md).
	Itabs []ir.Itab

	// ABI is where specs/030-abi.md puts this function's parameters and
	// results. AssignABI fills it and it is nil until then.
	//
	// It is a field of the function rather than a table the assignment pass
	// keeps, because two consumers read it and they must read the same
	// answer: Target.DefReg pre-colours an incoming argument from it, and the
	// code generator places the arguments from it. Two placements computed
	// apart are a call that reads its arguments from the wrong place.
	ABI *ABI

	nextValue ID
	nextBlock ID
}

// NewFunc returns an empty function with an entry block.
func NewFunc(name string) *Func {
	f := &Func{Name: name, Sym: name}
	f.Entry = f.NewBlock(BlockInvalid)
	return f
}

// NewBlock adds a block to the function.
func (f *Func) NewBlock(kind BlockKind) *Block {
	b := &Block{ID: f.nextBlock, Kind: kind, Func: f, defs: make(map[*ir.Object]*Value)}
	f.nextBlock++
	f.Blocks = append(f.Blocks, b)
	return b
}

func (f *Func) newValue(op Op, t *ir.Type, pos syntax.Pos) *Value {
	v := &Value{ID: f.nextValue, Op: op, Type: t, Pos: pos}
	f.nextValue++
	return v
}

// NumBlocks returns one more than the largest block identifier in use.
//
// It is the length a slice indexed by block identifier must have.
func (f *Func) NumBlocks() int { return int(f.nextBlock) }

// NumValues returns one more than the largest value identifier in use.
func (f *Func) NumValues() int { return int(f.nextValue) }

// removeBlock deletes b from the layout.
//
// It does not touch edges. The only caller is construction, which drops a join
// block that turned out to have no predecessor, such as the block after an if
// whose two arms both return.
func (f *Func) removeBlock(b *Block) {
	for i, c := range f.Blocks {
		if c == b {
			f.Blocks = append(f.Blocks[:i], f.Blocks[i+1:]...)
			return
		}
	}
}

// String returns a dump of the function.
//
// The order is the layout order of Blocks and the value order within a block.
// Nothing here derives from a map or an address, so two dumps of two builds of
// one input are the same bytes.
func (f *Func) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "func %s:\n", f.Name)
	for _, blk := range f.Blocks {
		fmt.Fprintf(&b, "%v:", blk)
		if len(blk.Preds) > 0 {
			b.WriteString(" <-")
			for _, p := range blk.Preds {
				fmt.Fprintf(&b, " %v", p)
			}
		}
		b.WriteString("\n")
		for _, v := range blk.Values {
			fmt.Fprintf(&b, "    %s\n", v.LongString())
		}
		fmt.Fprintf(&b, "  %v", blk.Kind)
		if blk.Control != nil {
			fmt.Fprintf(&b, " %v", blk.Control)
		}
		for i, s := range blk.Succs {
			if i == 0 {
				b.WriteString(" ->")
			}
			fmt.Fprintf(&b, " %v", s)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// predIndex returns the slot in b.Succs[i]'s predecessor list that the edge
// b -> b.Succs[i] occupies.
//
// A block can be its own successor twice, so counting is required: the k-th
// occurrence of s in b.Succs is the k-th occurrence of b in s.Preds, because
// AddEdgeTo appends to both lists at once.
func predIndex(b *Block, i int) int {
	s := b.Succs[i]
	k := 0
	for j := 0; j < i; j++ {
		if b.Succs[j] == s {
			k++
		}
	}
	n := 0
	for j, p := range s.Preds {
		if p == b {
			if n == k {
				return j
			}
			n++
		}
	}
	return -1
}

// SplitCriticalEdges splits every critical edge of f.
//
// An edge from a block with several successors to a block with several
// predecessors is critical: there is no block on it that runs exactly when the
// edge is taken. specs/026-register-allocation.md needs somewhere to put the
// move that a phi becomes, and this is that place. Doing it here rather than
// there keeps the CFG surgery next to the CFG.
//
// Phi arguments are not touched. The new block takes the predecessor's slot in
// the successor's predecessor list, so slot i still means the same edge.
func SplitCriticalEdges(f *Func) int {
	n := 0
	// The block list grows as edges are split. Only the blocks present at the
	// start can carry a critical edge, and a new block has one successor.
	blocks := make([]*Block, len(f.Blocks))
	copy(blocks, f.Blocks)
	for _, b := range blocks {
		if len(b.Succs) < 2 {
			continue
		}
		for i := range b.Succs {
			s := b.Succs[i]
			if len(s.Preds) < 2 {
				continue
			}
			j := predIndex(b, i)
			d := f.NewBlock(BlockPlain)
			d.Pos = b.Pos
			d.sealed = true
			d.Preds = []*Block{b}
			d.Succs = []*Block{s}
			b.Succs[i] = d
			s.Preds[j] = d
			n++
		}
	}
	return n
}
