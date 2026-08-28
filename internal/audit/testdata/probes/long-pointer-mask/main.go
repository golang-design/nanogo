// A type with more pointer words than a bitmask describes.
//
// Past internal/abi.MaxPtrmaskBytes*8 words, which is 128, the descriptor
// points at a word the runtime fills the mask into rather than at the mask,
// with TFlagGCMaskOnDemand set to say so. Every pointer here is reached only
// through such a value, in the heap, in a global and in a frame, so a mask
// that describes nothing is a value the collector frees while it is in use.
package main

import (
	"os"
	"runtime"
)

type cell struct{ n int }

type wide [200]*cell

var root wide

//go:noinline
func hold(w *wide) int {
	for i := 0; i < 2000; i++ {
		_ = make([]byte, 128)
	}
	runtime.GC()
	runtime.GC()
	s := 0
	for _, c := range w {
		s = s + c.n
	}
	return s
}

//go:noinline
func onStack(n int) int {
	var w wide
	for i := range w {
		w[i] = &cell{n + i}
	}
	return hold(&w)
}

func main() {
	for i := range root {
		root[i] = &cell{i}
	}
	heap := new(wide)
	for i := range heap {
		heap[i] = &cell{i * 2}
	}
	frame := onStack(1)

	rs := hold(&root)
	hs := hold(heap)
	// 199*200/2, twice that, and 200*1 + 199*200/2.
	if rs != 19900 || hs != 39800 || frame != 20100 {
		os.Exit(1)
	}
	os.Exit(0)
}
