package main

import "os"

type five struct{ a, b, c, d, e int }

func total(f five) int { return f.a + f.b }

func main() {
	os.Exit(total(five{a: 3, b: 4}))
}
