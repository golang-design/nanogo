// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"testing"

	"golang.design/x/nanogo/ir"
	"golang.design/x/nanogo/syntax"
)

// cfg builds a control-flow graph of n blocks from an edge list.
//
// The kind follows the successor count, so the graph verifies as far as its
// shape goes. These tests are about dominance, not about values.
func cfg(n int, edges ...[2]int) ([]*Block, *Func) {
	f := NewFunc("t")
	blocks := []*Block{f.Entry}
	for i := 1; i < n; i++ {
		blocks = append(blocks, f.NewBlock(BlockInvalid))
	}
	for _, e := range edges {
		blocks[e[0]].AddEdgeTo(blocks[e[1]])
	}
	for _, b := range blocks {
		switch len(b.Succs) {
		case 0:
			b.Kind = BlockExit
		case 1:
			b.Kind = BlockPlain
		default:
			b.Kind = BlockIf
		}
	}
	return blocks, f
}

// bruteDominates is the definition of dominance, computed the slow way: a
// dominates b when every path from the entry to b passes through a.
//
// It is here to check the fast answer. Cooper, Harvey and Kennedy is an
// iterative fixed point, and a fixed point that converges to the wrong answer
// converges silently.
func bruteDominates(f *Func, a, b *Block) bool {
	if a == b {
		return reachableAvoiding(f, nil, b)
	}
	return !reachableAvoiding(f, a, b)
}

// reachableAvoiding reports whether b is reachable from the entry without
// passing through avoid.
func reachableAvoiding(f *Func, avoid, b *Block) bool {
	seen := make([]bool, f.NumBlocks())
	var walk func(*Block) bool
	walk = func(x *Block) bool {
		if x == avoid || seen[x.ID] {
			return false
		}
		seen[x.ID] = true
		if x == b {
			return true
		}
		for _, s := range x.Succs {
			if walk(s) {
				return true
			}
		}
		return false
	}
	if f.Entry == avoid {
		return false
	}
	return walk(f.Entry)
}

func TestDominators(t *testing.T) {
	tests := []struct {
		name  string
		n     int
		edges [][2]int
		// idom[i] is the immediate dominator of block i, or -1 for the entry
		// and -2 for an unreachable block.
		idom []int
	}{
		{
			name:  "straight line",
			n:     3,
			edges: [][2]int{{0, 1}, {1, 2}},
			idom:  []int{-1, 0, 1},
		},
		{
			name:  "diamond",
			n:     4,
			edges: [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}},
			idom:  []int{-1, 0, 0, 0},
		},
		{
			name:  "if with one arm",
			n:     3,
			edges: [][2]int{{0, 1}, {0, 2}, {1, 2}},
			idom:  []int{-1, 0, 0},
		},
		{
			name:  "loop",
			n:     4,
			edges: [][2]int{{0, 1}, {1, 2}, {2, 1}, {1, 3}},
			idom:  []int{-1, 0, 1, 1},
		},
		{
			name: "nested loops",
			n:    6,
			edges: [][2]int{
				{0, 1}, {1, 2}, {2, 3}, {3, 2}, {2, 4}, {4, 1}, {1, 5},
			},
			idom: []int{-1, 0, 1, 2, 2, 1},
		},
		{
			name:  "a self loop",
			n:     3,
			edges: [][2]int{{0, 1}, {1, 1}, {1, 2}},
			idom:  []int{-1, 0, 1},
		},
		{
			name: "irreducible, two entries to one cycle",
			n:    3,
			// 1 and 2 form a cycle that is entered at both of its blocks, so
			// neither dominates the other. Go's goto produces this, and it is
			// the case a dominator algorithm is most often wrong on.
			edges: [][2]int{{0, 1}, {0, 2}, {1, 2}, {2, 1}},
			idom:  []int{-1, 0, 0},
		},
		{
			name: "irreducible, the Cooper Harvey Kennedy example",
			n:    6,
			edges: [][2]int{
				{0, 1}, {0, 2}, {1, 3}, {2, 4}, {2, 5},
				{3, 4}, {4, 3}, {4, 5}, {5, 4},
			},
			idom: []int{-1, 0, 0, 0, 0, 0},
		},
		{
			name:  "an unreachable block",
			n:     4,
			edges: [][2]int{{0, 1}, {2, 3}, {3, 1}},
			idom:  []int{-1, 0, -2, -2},
		},
		{
			name:  "a block that only a back edge reaches",
			n:     4,
			edges: [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 1}},
			idom:  []int{-1, 0, 1, 2},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blocks, f := cfg(tc.n, tc.edges...)
			d := Dominators(f)

			for i, want := range tc.idom {
				b := blocks[i]
				got := d.Idom(b)
				switch want {
				case -1:
					if got != nil {
						t.Errorf("Idom(%v) is %v, want none for the entry", b, got)
					}
					if !d.Reachable(b) {
						t.Errorf("the entry is not reachable")
					}
				case -2:
					if d.Reachable(b) {
						t.Errorf("%v is reachable, want unreachable", b)
					}
					if got != nil {
						t.Errorf("Idom(%v) is %v, want none for an unreachable block", b, got)
					}
				default:
					if got != blocks[want] {
						t.Errorf("Idom(%v) is %v, want %v", b, got, blocks[want])
					}
				}
			}

			// The tree must agree with the definition on every pair.
			for _, a := range blocks {
				for _, b := range blocks {
					want := d.Reachable(a) && d.Reachable(b) && bruteDominates(f, a, b)
					if got := d.Dominates(a, b); got != want {
						t.Errorf("Dominates(%v, %v) is %v, want %v", a, b, got, want)
					}
					if got := d.StrictlyDominates(a, b); got != (want && a != b) {
						t.Errorf("StrictlyDominates(%v, %v) is %v, want %v", a, b, got, want && a != b)
					}
				}
			}

			// Reverse postorder puts a block after its immediate dominator,
			// which is what a forward data flow pass relies on.
			pos := make(map[*Block]int, len(blocks))
			for i, b := range d.ReversePostorder() {
				pos[b] = i
			}
			for _, b := range d.ReversePostorder() {
				if id := d.Idom(b); id != nil && pos[id] >= pos[b] {
					t.Errorf("%v comes before its dominator %v in reverse postorder", b, id)
				}
			}
		})
	}
}

func TestDominatorsOfAnEmptyFunc(t *testing.T) {
	f := &Func{Name: "t"}
	d := Dominators(f)
	if got := f.Postorder(); got != nil {
		t.Errorf("Postorder of a function with no entry is %v, want nil", got)
	}
	if d.Idom(nil) != nil || d.Reachable(nil) || d.Dominates(nil, nil) {
		t.Error("a function with no entry has a dominator relation")
	}
	// An identifier from another function must not index into this tree.
	other := NewFunc("other")
	for i := 0; i < 4; i++ {
		other.NewBlock(BlockPlain)
	}
	if d.Idom(other.Blocks[3]) != nil || d.Reachable(other.Blocks[3]) {
		t.Error("a block of another function is in this tree")
	}
}

func TestDominatorsOfASingleBlock(t *testing.T) {
	f, entry, _ := minimalFunc()
	d := Dominators(f)
	if d.Idom(entry) != nil {
		t.Errorf("Idom(entry) is %v, want none", d.Idom(entry))
	}
	if !d.Dominates(entry, entry) {
		t.Error("a block does not dominate itself")
	}
	if d.StrictlyDominates(entry, entry) {
		t.Error("a block strictly dominates itself")
	}
}

// TestDominatorsAfterConstruction checks the tree against the shape the
// builder produces, which is the only way it is used in practice.
func TestDominatorsAfterConstruction(t *testing.T) {
	x := obj("x", tInt, ir.ClassLocal)
	fn := fun("f", []*ir.Object{x},
		forStmt([]ir.Stmt{asn(local(x), cint("0"))},
			cmp(syntax.Lss, local(x), cint("10")),
			[]ir.Stmt{asn(local(x), bin(syntax.Add, local(x), cint("1")))},
			nil),
		ret(local(x)))
	f := build(t, fn)
	d := Dominators(f)
	for _, b := range f.Blocks {
		for _, a := range f.Blocks {
			want := bruteDominates(f, a, b)
			if got := d.Dominates(a, b); got != want {
				t.Errorf("Dominates(%v, %v) is %v, want %v\n%s", a, b, got, want, f)
			}
		}
	}
}
