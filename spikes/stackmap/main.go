package main

import (
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
)

type Obj struct{ a, b, c, d int }

var fin int32
var seen int

//go:noinline
func mk() *Obj {
	o := new(Obj)
	o.a = 0x5eed
	runtime.SetFinalizer(o, func(*Obj) { atomic.AddInt32(&fin, 1) })
	return o
}

//go:noinline
func gcNow() { runtime.GC(); runtime.GC(); runtime.GC() }

//go:noinline
func check(p *Obj) { seen = p.a }

var p1, p2 int32

//go:noinline
func phase1() { p1 = atomic.LoadInt32(&fin) }

//go:noinline
func phase2() { p2 = atomic.LoadInt32(&fin) }

func spikeLive() int
func spikeDead() int
func spikeBogus() int
func spikeMulti() int

func main() {
	switch os.Args[1] {
	case "live":
		atomic.StoreInt32(&fin, 0)
		spikeLive()
		n := atomic.LoadInt32(&fin)
		fmt.Printf("live: finalizers-during-call=%d seen=%#x\n", n, seen)
	case "dead":
		atomic.StoreInt32(&fin, 0)
		spikeDead()
		n := atomic.LoadInt32(&fin)
		fmt.Printf("dead: finalizers-during-call=%d\n", n)
	case "multi":
		atomic.StoreInt32(&fin, 0)
		spikeMulti()
		fmt.Printf("multi: after-index0-gc=%d (want 0)  after-index1-gc=%d (want 1)\n", p1, p2)
		if p1 == 0 && p2 == 1 {
			fmt.Println("RESULT: per-PC stack maps in text asm ARE honoured")
		} else {
			fmt.Println("RESULT: per-PC stack maps NOT honoured")
		}
	case "bogus":
		spikeBogus()
		fmt.Println("bogus: survived (runtime did not read our map)")
	}
}
