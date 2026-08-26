package main

import "os"

type quad struct{ a, b, c, d int }

func make4() quad { return quad{a: 3, b: 4} }

func main() {
	q := make4()
	os.Exit(q.a + q.b)
}
