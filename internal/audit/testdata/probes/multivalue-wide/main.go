package main

import "os"

type pair struct {
	a int
	b int
	c int
}

func two() (pair, int) { return pair{a: 3, b: 4}, 7 }

func main() {
	p, n := two()
	os.Exit(p.a + p.b + n - 7)
}
