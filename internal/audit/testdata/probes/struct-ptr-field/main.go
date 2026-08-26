package main

import "os"

type node struct {
	n    int
	next *node
}

func main() {
	b := &node{n: 4}
	a := &node{n: 3, next: b}
	os.Exit(a.n + a.next.n)
}
