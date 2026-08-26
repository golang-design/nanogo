package main

import "os"

type five struct{ a, b, c, d, e int }

func make5() five { return five{a: 3, b: 4} }

func main() {
	f := make5()
	os.Exit(f.a + f.b)
}
