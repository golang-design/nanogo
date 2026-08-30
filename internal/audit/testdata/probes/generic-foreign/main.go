package main

import (
	"os"
	"sync/atomic"

	"nanogo.probe/genericforeign/lib"
)

func main() {
	var b lib.Box[int]
	b.Set(lib.Max(3, 7))

	// A generic type the standard library declares, which is the shape the
	// help text names, and the one a package of nanogo's own tripped on.
	var p atomic.Int64
	p.Store(int64(b.Get()))

	if lib.Max("a", "b") != "b" {
		os.Exit(1)
	}
	os.Exit(int(p.Load()))
}
