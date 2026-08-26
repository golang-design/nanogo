package main

import "os"

type wide struct{ a, b, c, d, e int }

func make5() wide { return wide{a: 3, b: 4} }

func main() {
	w := make5()
	os.Exit(w.a + w.b)
}
