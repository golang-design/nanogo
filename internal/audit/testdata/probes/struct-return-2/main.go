package main

import "os"

type pair struct{ a, b int }

func make2() pair { return pair{a: 3, b: 4} }

func main() {
	p := make2()
	os.Exit(p.a + p.b)
}
