// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package ssa

// The dominator tree.
//
// specs/021-ssa-construction.md builds phis with the on-the-fly algorithm of
// Braun et al. (2013) precisely so that construction needs no dominator tree.
// The tree is still needed afterwards, by the verifier and by prove, cse and
// nilcheckelim in specs/022-optimization-passes.md, so it is computed once,
// here, after construction.
//
// The algorithm is the iterative one of Cooper, Harvey and Kennedy, "A Simple,
// Fast Dominance Algorithm" (2001). Lengauer-Tarjan has the better asymptotic
// bound, and this one is chosen anyway for two reasons. It is about forty
// lines against about two hundred, in a compiler meant to be read end to end.
// And it is fast in practice on the graphs a compiler sees: the paper measures
// it faster than Lengauer-Tarjan below roughly a thousand blocks, and a Go
// function almost never reaches that. It is also correct on an irreducible
// graph, which Go's goto can produce, with no special case.

// DomTree is the dominator tree of one function.
//
// It is a snapshot. Any pass that changes the control-flow graph invalidates
// it, and the pass recomputes it rather than repairing it.
type DomTree struct {
	// idom holds the immediate dominator of each block, indexed by block
	// identifier. The entry block is its own immediate dominator, and an
	// unreachable block has none.
	idom []*Block

	// po holds the postorder number of each block, indexed by block
	// identifier, or -1 when the block is unreachable. intersect compares
	// these numbers, which is the whole trick of the algorithm: the block
	// with the smaller postorder number is the deeper one.
	po []int32

	// order is the reverse postorder of the reachable blocks.
	order []*Block

	entry *Block
}

// Postorder returns the reachable blocks of f in postorder.
//
// The walk visits successors in slot order, so the result depends only on the
// graph, not on a map or an address.
func (f *Func) Postorder() []*Block {
	if f.Entry == nil {
		return nil
	}
	visited := make([]bool, f.NumBlocks())
	order := make([]*Block, 0, len(f.Blocks))

	// An explicit stack rather than recursion. A generated function can have
	// thousands of blocks in one chain, and the Go stack would grow to match.
	type frame struct {
		b *Block
		i int
	}
	stack := []frame{{f.Entry, 0}}
	visited[f.Entry.ID] = true
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.i < len(top.b.Succs) {
			s := top.b.Succs[top.i]
			top.i++
			if !visited[s.ID] {
				visited[s.ID] = true
				stack = append(stack, frame{s, 0})
			}
			continue
		}
		order = append(order, top.b)
		stack = stack[:len(stack)-1]
	}
	return order
}

// Dominators computes the dominator tree of f.
func Dominators(f *Func) *DomTree {
	d := &DomTree{
		idom:  make([]*Block, f.NumBlocks()),
		po:    make([]int32, f.NumBlocks()),
		entry: f.Entry,
	}
	for i := range d.po {
		d.po[i] = -1
	}
	post := f.Postorder()
	for i, b := range post {
		d.po[b.ID] = int32(i)
	}
	d.order = make([]*Block, len(post))
	for i, b := range post {
		d.order[len(post)-1-i] = b
	}
	if len(post) == 0 {
		return d
	}

	// The entry block dominates itself and nothing else is known yet.
	d.idom[f.Entry.ID] = f.Entry

	for changed := true; changed; {
		changed = false
		for _, b := range d.order {
			if b == f.Entry {
				continue
			}
			var new *Block
			for _, p := range b.Preds {
				if d.idom[p.ID] == nil {
					// Not processed yet, or unreachable. Either way it carries
					// no information on this round.
					continue
				}
				if new == nil {
					new = p
					continue
				}
				new = d.intersect(new, p)
			}
			if new != nil && d.idom[b.ID] != new {
				d.idom[b.ID] = new
				changed = true
			}
		}
	}
	return d
}

// intersect walks the two blocks up the partial tree until they meet.
func (d *DomTree) intersect(b1, b2 *Block) *Block {
	for b1 != b2 {
		for d.po[b1.ID] < d.po[b2.ID] {
			b1 = d.idom[b1.ID]
		}
		for d.po[b2.ID] < d.po[b1.ID] {
			b2 = d.idom[b2.ID]
		}
	}
	return b1
}

// Idom returns the immediate dominator of b, or nil for the entry block and
// for an unreachable block.
func (d *DomTree) Idom(b *Block) *Block {
	if b == nil || int(b.ID) >= len(d.idom) {
		return nil
	}
	if b == d.entry {
		return nil
	}
	return d.idom[b.ID]
}

// Reachable reports whether b is reachable from the entry block.
func (d *DomTree) Reachable(b *Block) bool {
	return b != nil && int(b.ID) < len(d.po) && d.po[b.ID] >= 0
}

// ReversePostorder returns the reachable blocks in reverse postorder.
//
// Every block appears after its dominator, which is the order a forward data
// flow pass wants.
func (d *DomTree) ReversePostorder() []*Block { return d.order }

// Dominates reports whether a dominates b. A block dominates itself.
func (d *DomTree) Dominates(a, b *Block) bool {
	if a == nil || b == nil || !d.Reachable(a) || !d.Reachable(b) {
		return false
	}
	for b != nil {
		if b == a {
			return true
		}
		if b == d.entry {
			return false
		}
		b = d.idom[b.ID]
	}
	return false
}

// StrictlyDominates reports whether a dominates b and is not b.
func (d *DomTree) StrictlyDominates(a, b *Block) bool {
	return a != b && d.Dominates(a, b)
}
